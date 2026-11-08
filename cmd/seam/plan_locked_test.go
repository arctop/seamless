package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
)

// Under peer_auth.locked the console-backed commands name a locked project
// instead of answering "not found" (single lookups) or filtering in silence
// (lists): owner decision D2. The daemon here is a stand-in that answers the
// way the console does: a lookup of an item in a locked project is 403 with
// the lock refusal and {"locked": true, "project"}, a list carries "locked"
// with the projects it left out.

// lockedRefusalFx is the console's answer for an item in example-finance
// (console.answerLocked, the socket wording of access.View.Refusal).
const lockedRefusalFx = "project example-finance is locked: even the owner's own agents need the owner's approval, " +
	"and this connection holds none.\nRemedy: run `seam access request example-finance` from the agent's shell to " +
	"file an access request, and the owner approves it in their own terminal with `seam approve` and the request's id."

// lockedConsole serves canned console answers by "METHOD /path?query": a
// status and a body. Anything else is a 404, as the console answers a miss.
func lockedConsole(t *testing.T, routes map[string]struct {
	status int
	body   string
}) *env {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route, ok := routes[r.Method+" "+r.URL.RequestURI()]
		if !ok {
			http.Error(w, `{"error":"No such thing."}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(route.status)
		_, _ = io.WriteString(w, route.body)
	}))
	t.Cleanup(srv.Close)
	cfg := config.Defaults()
	cfg.Addr = strings.TrimPrefix(srv.URL, "http://")
	cfg.MCP.APIKey = "fixture-key"
	e, _, _ := stubEnv()
	e.loadConfig = func() (config.Config, error) { return cfg, nil }
	return e
}

func runLocked(t *testing.T, e *env, argv ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errb strings.Builder
	e.stdout, e.stderr = &out, &errb
	code = dispatch(context.Background(), e, argv)
	return out.String(), errb.String(), code
}

func TestLockedLookups_NameTheLock(t *testing.T) {
	locked := mustJSON(t, map[string]any{"error": lockedRefusalFx, "locked": true, "project": "example-finance"})
	type route = struct {
		status int
		body   string
	}
	e := lockedConsole(t, map[string]route{
		"GET /console/plans/ledger-close?format=json":                  {http.StatusForbidden, locked},
		"POST /console/plans/ledger-close/approve?format=json":         {http.StatusForbidden, locked},
		"GET /console/sessions/01KHHHHHHHHHHHHHHHHHHHHHHH?format=json": {http.StatusForbidden, locked},
	})
	// plan check reads the plan before it runs git, so the lock answers first.
	repo := t.TempDir()
	for _, argv := range [][]string{
		{"plan", "show", "ledger-close"},
		{"plan", "show", "--json", "ledger-close"},
		{"plan", "approve", "ledger-close"},
		{"plan", "check", "--cwd", repo, "ledger-close"},
		{"sessions", "01KHHHHHHHHHHHHHHHHHHHHHHH"},
		{"sessions", "--json", "01KHHHHHHHHHHHHHHHHHHHHHHH"},
	} {
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			stdout, stderr, code := runLocked(t, e, argv...)
			require.Equal(t, 1, code, stderr)
			require.Empty(t, stdout, "nothing but the refusal")
			// Two lines: the rule, then the remedy -- the words a tool-backed
			// command prints for the same lock.
			require.Equal(t, "error: "+lockedRefusalFx+"\n", stderr)
			require.NotContains(t, stderr, "not found")
		})
	}

	// A miss that is not a lock still says "not found", as before.
	_, stderr, code := runLocked(t, e, "plan", "show", "no-such-plan")
	require.Equal(t, 1, code)
	require.Equal(t, "error: not found\n", stderr)
}

func TestLockedLists_NameWhatTheyLeftOut(t *testing.T) {
	type route = struct {
		status int
		body   string
	}
	plans := `{"rows":[{"slug":"lease-rework","title":"Rework leases","project":"example-open","status":"approved",` +
		`"updated":"2999-01-01T00:00:00Z"}],"locked":["example-finance","example-legal"]}`
	sessions := `{"total":1,"active":1,"sessions":[{"id":"01KHHHHHHHHHHHHHHHHHHHHHHH","name":"cc/quiet-otter",` +
		`"project":"example-open","status":"active","updated":"2999-01-01T00:00:00Z"}],"locked":["example-finance"]}`
	held := `{"pending":[],"recent":[],"locked":["example-finance","example-legal"]}`
	e := lockedConsole(t, map[string]route{
		"GET /console/plans?format=json&w=all": {http.StatusOK, plans},
		"GET /console/sessions?format=json":    {http.StatusOK, sessions},
		"GET /console/held?format=json":        {http.StatusOK, held},
	})
	for _, tc := range []struct {
		argv     []string
		lastLine string
	}{
		{[]string{"plan", "list"}, "(not shown: locked projects example-finance, example-legal)"},
		// A --project filter names only a locked project it would have shown.
		{[]string{"plan", "list", "--project", "example-legal"}, "(not shown: locked project example-legal)"},
		{[]string{"plan", "list", "--project", "example-open"}, "  lease-rework"},
		{[]string{"sessions"}, "(not shown: locked project example-finance)"},
		{[]string{"proposals"}, "(not shown: locked projects example-finance, example-legal)"},
		{[]string{"proposals", "--project", "example-finance"}, "(not shown: locked project example-finance)"},
	} {
		t.Run(strings.Join(tc.argv, " "), func(t *testing.T) {
			stdout, stderr, code := runLocked(t, e, tc.argv...)
			require.Equal(t, 0, code, stderr)
			lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
			require.True(t, strings.HasPrefix(lines[len(lines)-1], tc.lastLine), "last line %q", lines[len(lines)-1])
			// Names only: no count of what a locked project holds.
			require.NotRegexp(t, `\d+ (plans?|sessions?|proposals?) in`, stdout)
		})
	}

	// The proposals' own JSON carries the names too; the console's JSON is
	// printed as sent, "locked" included.
	stdout, _, code := runLocked(t, e, "proposals", "--json")
	require.Equal(t, 0, code)
	require.JSONEq(t, `{"proposals":[],"locked":["example-finance","example-legal"]}`, stdout)
	stdout, _, code = runLocked(t, e, "proposals", "--json", "--project", "example-open")
	require.Equal(t, 0, code)
	require.JSONEq(t, `{"proposals":[]}`, stdout)
}

func TestLockedSkipped(t *testing.T) {
	for _, tc := range []struct {
		locked []string
		only   string
		want   string
	}{
		{nil, "", ""},
		{nil, "example-open", ""},
		{[]string{"example-finance"}, "", "(not shown: locked project example-finance)"},
		{[]string{"example-finance", "example-legal"}, "", "(not shown: locked projects example-finance, example-legal)"},
		{[]string{"example-finance", "example-legal"}, "example-legal", "(not shown: locked project example-legal)"},
		{[]string{"example-finance"}, "example-open", ""},
		{[]string{"a\x1b[2Jb"}, "", `(not shown: locked project a\x1b[2Jb)`},
	} {
		require.Equal(t, tc.want, lockedSkipped(tc.locked, tc.only), "%v %q", tc.locked, tc.only)
	}
}
