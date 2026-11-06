package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/agentproc"
	"github.com/arctop/seamless/internal/config"

	// Test-only, as in plan_test.go: the binary must not import these (they pull
	// SQLite in), but the test binary can, which is what lets the transcribed
	// shapes and the reason limit be pinned against the daemon's own.
	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/console"
	"github.com/arctop/seamless/internal/store"
)

// seenRequest is one request the stand-in console served.
type seenRequest struct {
	method string
	uri    string
	header http.Header
	form   map[string][]string
}

// accessConsole is a stand-in for the console's access routes: h answers, and
// every request is recorded with its form already parsed.
type accessConsole struct {
	srv  *httptest.Server
	mu   sync.Mutex
	seen []seenRequest
}

func newAccessConsole(t *testing.T, h http.HandlerFunc) *accessConsole {
	t.Helper()
	ac := &accessConsole{}
	ac.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		ac.mu.Lock()
		ac.seen = append(ac.seen, seenRequest{method: r.Method, uri: r.URL.RequestURI(), header: r.Header.Clone(), form: r.PostForm})
		ac.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		h(w, r)
	}))
	t.Cleanup(ac.srv.Close)
	return ac
}

func (ac *accessConsole) config() config.Config {
	cfg := config.Defaults()
	cfg.Addr = strings.TrimPrefix(ac.srv.URL, "http://")
	cfg.MCP.APIKey = "fixture-key"
	return cfg
}

func (ac *accessConsole) requests() []seenRequest {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	return slices.Clone(ac.seen)
}

// env is a test env aimed at the stand-in console.
func (ac *accessConsole) env() (*env, *strings.Builder, *strings.Builder) {
	e, _, _ := stubEnv()
	var out, errb strings.Builder
	e.stdout, e.stderr = &out, &errb
	cfg := ac.config()
	e.loadConfig = func() (config.Config, error) { return cfg, nil }
	return e, &out, &errb
}

const filedAnswer = `{"id":"01KACCESSREQUEST0000007F3K","short_id":"R-7F3K","project":"example-finance","kind":"request",` +
	`"page":"https://seamless.example.org/console/access/01KACCESSREQUEST0000007F3K","extra":"kept"}`

func TestAccessRequest_FilesTheRequestAndNamesTheOwnersCommand(t *testing.T) {
	ac := newAccessConsole(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(filedAnswer + "\n"))
	})
	e, out, errb := ac.env()
	code := dispatch(context.Background(), e, []string{"access", "request", "example-finance", "--reason", "  reconcile the Q3 ledger  "})
	require.Equal(t, 0, code, errb.String())
	require.Equal(t,
		"Requested R-7F3K for example-finance (request). The owner approves it with: seam approve R-7F3K\n"+
			"or with a passkey: https://seamless.example.org/console/access/01KACCESSREQUEST0000007F3K\n",
		out.String())

	reqs := ac.requests()
	require.Len(t, reqs, 1)
	got := reqs[0]
	require.Equal(t, http.MethodPost, got.method)
	require.Equal(t, "/console/access/request?format=json", got.uri)
	require.Equal(t, "application/x-www-form-urlencoded", got.header.Get("Content-Type"))
	require.Equal(t, []string{"example-finance"}, got.form["project"])
	require.Equal(t, []string{"reconcile the Q3 ledger"}, got.form["reason"], "the reason is trimmed, as the daemon trims it")
	require.Equal(t, "Bearer fixture-key", got.header.Get("Authorization"))
}

// The request route finds the asking agent's session by the machine and the
// agent process this CLI names; consoleRequest sends neither, so this call must.
func TestAccessRequest_SendsTheAgentIdentityHeaders(t *testing.T) {
	ac := newAccessConsole(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(filedAnswer))
	})
	e, _, errb := ac.env()
	require.Equal(t, 0, dispatch(context.Background(), e, []string{"access", "request", "example-finance"}), errb.String())
	h := ac.requests()[0].header
	if host := config.Hostname(); host != "" {
		require.Equal(t, host, h.Get(hostHeader))
	}
	want, ok := agentproc.Anchor()
	if !ok {
		t.Skip("no agent process resolvable on this platform")
	}
	require.Equal(t, want, h.Get(agentProcessHeader))
	require.Equal(t, "X-Seamless-Agent-Process", agentProcessHeader, "the literal the console's request route reads")
}

func TestAccessRequest_NoPageNoPasskeyLine(t *testing.T) {
	ac := newAccessConsole(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"01KX","short_id":"R-0001","project":"example-finance","kind":"refresh","page":""}`))
	})
	e, out, _ := ac.env()
	require.Equal(t, 0, dispatch(context.Background(), e, []string{"access", "request", "example-finance"}))
	require.Equal(t, "Requested R-0001 for example-finance (refresh). The owner approves it with: seam approve R-0001\n", out.String())
	_, sent := ac.requests()[0].form["reason"]
	require.False(t, sent, "an absent reason is not sent as an empty one")
}

func TestAccessRequest_JSONPrintsTheAnswerAsSent(t *testing.T) {
	ac := newAccessConsole(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(filedAnswer + "\n"))
	})
	e, out, _ := ac.env()
	require.Equal(t, 0, dispatch(context.Background(), e, []string{"access", "request", "--json", "example-finance"}))
	require.Equal(t, filedAnswer+"\n", out.String())
	var v map[string]any
	require.NoError(t, json.Unmarshal([]byte(out.String()), &v))
	require.Equal(t, "R-7F3K", v["short_id"])
	require.Equal(t, "kept", v["extra"], "a field this CLI does not know survives --json")
}

func TestAccessRequest_RefusesALongReasonBeforeConnecting(t *testing.T) {
	e, _, errb := stubEnv()
	e.loadConfig = func() (config.Config, error) {
		t.Fatal("a reason over the limit must be refused before any connection")
		return config.Config{}, nil
	}
	code := dispatch(context.Background(), e, []string{"access", "request", "example-finance", "--reason", strings.Repeat("a", 201)})
	require.Equal(t, 1, code)
	require.Contains(t, errb.String(), "the reason is limited to 200 characters (this one has 201)")
}

// The limit counts characters, as the daemon does, not bytes.
func TestAccessRequest_CountsCharactersNotBytes(t *testing.T) {
	ac := newAccessConsole(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(filedAnswer))
	})
	e, _, errb := ac.env()
	reason := strings.Repeat("é", 200)
	require.Equal(t, 0, dispatch(context.Background(), e, []string{"access", "request", "example-finance", "--reason", reason}), errb.String())
	require.Equal(t, []string{reason}, ac.requests()[0].form["reason"])
}

// A 404 is the console saying no project is locked; its message is the answer,
// where consoleRequest would print a bare "not found".
func TestAccessRequest_SurfacesTheConsolesRefusal(t *testing.T) {
	for _, tc := range []struct {
		code int
		body string
		want string
	}{
		{http.StatusNotFound, `{"error":"no project is locked, so there are no access requests"}`, "no project is locked, so there are no access requests"},
		{http.StatusBadRequest, `{"error":"that project is not locked: nothing to request"}`, "that project is not locked: nothing to request"},
	} {
		ac := newAccessConsole(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.code)
			_, _ = w.Write([]byte(tc.body))
		})
		e, out, errb := ac.env()
		require.Equal(t, 1, dispatch(context.Background(), e, []string{"access", "request", "example-finance"}))
		require.Equal(t, "error: "+tc.want+"\n", errb.String())
		require.Empty(t, out.String())
	}
}

func TestAccessRequest_RequiresAProject(t *testing.T) {
	e, _, errb := stubEnv()
	require.Equal(t, 2, dispatch(context.Background(), e, []string{"access", "request"}))
	require.Contains(t, errb.String(), "missing argument: seam access request requires <project>")

	e, _, errb = stubEnv()
	require.Equal(t, 2, dispatch(context.Background(), e, []string{"access"}))
	require.Contains(t, errb.String(), `unknown command "access": valid values are access request`)
}

// The reason limit is the daemon's, transcribed; pinned so the two cannot drift.
func TestMaxReasonRunes_MatchesTheDaemon(t *testing.T) {
	require.Equal(t, store.MaxAccessReasonRunes, maxReasonRunes)
}

// jsonKeys lists a struct type's JSON keys, in declaration order.
func jsonKeys(t reflect.Type) []string {
	var keys []string
	for i := range t.NumField() {
		keys = append(keys, strings.Split(t.Field(i).Tag.Get("json"), ",")[0])
	}
	return keys
}

// The shapes this CLI decodes are transcriptions of the daemon's (it must not
// import them); a renamed or added key on the daemon's side fails here instead
// of rendering as a silently empty fact.
func TestAccessShapes_MatchTheDaemon(t *testing.T) {
	require.Equal(t, jsonKeys(reflect.TypeFor[approval.Facts]()), jsonKeys(reflect.TypeFor[accessFacts]()))
	require.Equal(t, jsonKeys(reflect.TypeFor[approval.Warning]()), jsonKeys(reflect.TypeFor[accessWarning]()))
	require.Equal(t, jsonKeys(reflect.TypeFor[console.TerminalApprover]()), jsonKeys(reflect.TypeFor[terminalApprover]()))
	require.Equal(t, approval.Durations[0], durationSession, "the daemon's default duration is the prompt's")
}
