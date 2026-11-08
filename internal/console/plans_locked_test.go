package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/store"
)

// The console-backed seam commands under peer_auth.locked (owner decision
// D2): a JSON lookup of a plan or a session that lives in a locked project
// the request does not read answers the lock -- 403 with the lock refusal the
// tool-backed commands print, {"locked": true, "project"} -- rather than a
// 404, and the plan and session lists name the locked projects they left
// out. Browser pages, misses and everything with nothing locked are as before.

// lockedAnswer is a decoded lock answer.
type lockedAnswer struct {
	Error   string `json:"error"`
	Locked  bool   `json:"locked"`
	Project string `json:"project"`
	Unlock  string `json:"unlock"`
}

func TestLockedLookups_AnswerTheLock(t *testing.T) {
	lc := newLockedConsole(t)
	ctx := context.Background()
	const boot = "6c2f1a3e-0b7d-4c55-9a51-3d8f00a1b2c3"
	stranger := access.ProcessToken{PID: 400, Version: 4000, Boot: boot}

	tcpRule := "project " + lockedSlug + " is locked: the TCP connection and its bearer key never reach a locked " +
		"project, because a shared key cannot be tied to one process.\nRemedy: "
	socketRule := "project " + lockedSlug + " is locked: even the owner's own agents need the owner's approval, " +
		"and this connection holds none.\nRemedy: run `seam access request " + lockedSlug + "` from the agent's shell"

	bearer := func(method, target string) *http.Request {
		req := httptest.NewRequest(method, target, strings.NewReader(""))
		req.Header.Set("Authorization", "Bearer "+testKey)
		req.Header.Set("Accept", "application/json")
		return req
	}
	socket := func(method, target string) *http.Request {
		v, ok := lc.policy.PeerChainView(501, []access.ProcessToken{stranger})
		require.True(t, ok)
		req := httptest.NewRequest(method, target, strings.NewReader(""))
		req.Header.Set("Accept", "application/json")
		return req.WithContext(access.WithView(req.Context(), v))
	}
	for _, tc := range []struct {
		name   string
		req    func(method, target string) *http.Request
		method string
		target string
		rule   string
	}{
		{"plan show, TCP key", bearer, http.MethodGet, "/console/plans/" + lc.plan + "?format=json", tcpRule},
		{"plan show, socket", socket, http.MethodGet, "/console/plans/" + lc.plan + "?format=json", socketRule},
		{"session, TCP key", bearer, http.MethodGet, "/console/sessions/" + lc.sessID + "?format=json", tcpRule},
		{"session, socket", socket, http.MethodGet, "/console/sessions/" + lc.sessID + "?format=json", socketRule},
		{"plan approve, socket", socket, http.MethodPost, "/console/plans/" + lc.plan + "/approve?format=json", socketRule},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := lc.do(tc.req(tc.method, tc.target))
			require.Equal(t, http.StatusForbidden, rr.Code, rr.Body.String())
			require.NotContains(t, rr.Body.String(), lockedSecret)
			var got lockedAnswer
			require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
			require.True(t, got.Locked)
			require.Equal(t, lockedSlug, got.Project)
			require.True(t, strings.HasPrefix(got.Error, tc.rule), got.Error)
			require.Equal(t, 2, len(strings.Split(got.Error, "\n")), "the rule, then the remedy")
		})
	}

	// Nothing was filed: a console read is not an access request.
	pending, err := store.PendingAccessApprovals(ctx, lc.db, time.Now())
	require.NoError(t, err)
	require.Empty(t, pending)

	// A browser's JSON gets the console unlock, the remedy a browser has; its
	// pages are unchanged.
	req := lc.browser(http.MethodGet, "/console/plans/"+lc.plan+"?format=json", "", nil)
	req.Header.Set("Accept", "application/json")
	rr := lc.do(req)
	require.Equal(t, http.StatusForbidden, rr.Code)
	var got lockedAnswer
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	require.True(t, got.Locked)
	require.Equal(t, "/console/unlock/"+lockedSlug, got.Unlock)
	status, _ := lc.get(t, "/console/plans/"+lc.plan, "")
	require.Equal(t, http.StatusNotFound, status, "a browser page answers as before")
	status, _ = lc.get(t, "/console/sessions/"+lc.sessID, "")
	require.Equal(t, http.StatusNotFound, status)

	// A miss is still a miss, and an open project's item still reads.
	rr = lc.do(bearer(http.MethodGet, "/console/plans/no-such-plan?format=json"))
	require.Equal(t, http.StatusNotFound, rr.Code)
	rr = lc.do(bearer(http.MethodGet, "/console/sessions/01NOSUCHTHING0000000000000?format=json"))
	require.Equal(t, http.StatusNotFound, rr.Code)
	require.NotContains(t, rr.Body.String(), "locked")
}

func TestLockedLists_NameTheLockedProjects(t *testing.T) {
	lc := newLockedConsole(t)
	bearer := func(target string) map[string]any {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.Header.Set("Authorization", "Bearer "+testKey)
		req.Header.Set("Accept", "application/json")
		rr := lc.do(req)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		require.NotContains(t, rr.Body.String(), lockedSecret)
		var out map[string]any
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
		return out
	}
	for _, target := range []string{"/console/plans?format=json&w=all", "/console/sessions?format=json"} {
		require.Equal(t, []any{lockedSlug}, bearer(target)["locked"], target)
	}

	// An unlocked browser reads the project: nothing is left out, and the key
	// is absent.
	cookie := lc.unlockLink(t)
	for _, target := range []string{"/console/plans?format=json&w=all", "/console/sessions?format=json"} {
		req := lc.browser(http.MethodGet, target, cookie, nil)
		req.Header.Set("Accept", "application/json")
		rr := lc.do(req)
		require.Equal(t, http.StatusOK, rr.Code)
		require.NotContains(t, rr.Body.String(), `"locked"`, target)
	}
}

// With nothing locked no answer changes: the lists carry no "locked" key and
// a miss is the 404 it always was.
func TestLockedLookups_NothingLocked(t *testing.T) {
	_, mux := newConsole(t)
	for _, target := range []string{"/console/plans?format=json&w=all", "/console/sessions?format=json"} {
		require.NotContains(t, getPeek(t, mux, target).Body.String(), `"locked"`, target)
	}
	for _, target := range []string{"/console/plans/ledger-plan?format=json", "/console/sessions/01NOSUCHTHING0000000000000?format=json"} {
		rr := getPeek(t, mux, target)
		require.Equal(t, http.StatusNotFound, rr.Code, target)
		require.NotContains(t, rr.Body.String(), "locked", target)
	}
}
