package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/store"
)

// The console-backed seam commands under a lock (owner decision
// D2): a JSON lookup of a plan or a session that lives in a locked project
// the request does not read answers the lock -- 403 with the lock refusal the
// tool-backed commands print, {"locked": true, "project"} -- rather than a
// 404, and the plan and session lists name the locked projects they left
// out. A browser page names the lock too (owner decision 2026-11-08). Misses
// and everything with nothing locked are as before.

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
	// pages go to the locked page (TestLockedPages_NameTheLock).
	req := lc.browser(http.MethodGet, "/console/plans/"+lc.plan+"?format=json", "", nil)
	req.Header.Set("Accept", "application/json")
	rr := lc.do(req)
	require.Equal(t, http.StatusForbidden, rr.Code)
	var got lockedAnswer
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	require.True(t, got.Locked)
	require.Equal(t, "/console/unlock/"+lockedSlug, got.Unlock)

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
	cookie := lc.unlockByTerminal(t)
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

// A browser page of a plan or a session in a locked project (owner decision
// 2026-11-08): not a 404 that sends the owner looking for something that
// exists, but the project's unlock page, saying the item is in it -- and,
// once this browser unlocked it, linking back. A fragment (peek, reader)
// says so in place. Nothing from inside the project shows.
func TestLockedPages_NameTheLock(t *testing.T) {
	lc := newLockedConsole(t)
	for _, tc := range []struct{ path, item string }{
		{"/console/plans/" + lc.plan, "plan"},
		{"/console/sessions/" + lc.sessID, "session"},
	} {
		rr := lc.do(lc.browser(http.MethodGet, tc.path, "", nil))
		require.Equal(t, http.StatusSeeOther, rr.Code, tc.path)
		loc := rr.Header().Get("Location")
		require.Equal(t, "/console/unlock/"+lockedSlug+"?item="+tc.item+"&next="+url.QueryEscape(tc.path), loc)

		status, page := lc.get(t, loc, "")
		require.Equal(t, http.StatusOK, status)
		require.Contains(t, page, `data-locked-item="`+tc.item+`"`)
		require.Contains(t, page, "The "+tc.item+" you opened is in this locked project. Unlock the project in this browser to read it.")
		require.NotContains(t, page, lockedSecret)

		for _, frag := range []string{"?peek=1", "?reader=1"} {
			rr := lc.do(lc.browser(http.MethodGet, tc.path+frag, "", nil))
			require.Equal(t, http.StatusForbidden, rr.Code, tc.path+frag)
			body := rr.Body.String()
			require.Contains(t, body, "This "+tc.item+" is in locked project "+lockedSlug+".")
			require.Contains(t, body, `<a href="/console/unlock/`+lockedSlug+`">Unlock `+lockedSlug+` in the console</a>`)
			require.NotContains(t, body, "<html", "a fragment, not a page")
			require.NotContains(t, body, lockedSecret)
		}
	}

	// Unlocked, the unlock page links back; the item itself reads.
	cookie := lc.unlockByTerminal(t)
	_, page := lc.get(t, "/console/unlock/"+lockedSlug+"?item=plan&next="+url.QueryEscape("/console/plans/"+lc.plan), cookie)
	require.Contains(t, page, `The plan you opened is in this locked project. <a href="/console/plans/`+lc.plan+`">Open it</a>.`)
	status, _ := lc.get(t, "/console/plans/"+lc.plan, cookie)
	require.Equal(t, http.StatusOK, status)
	// A next outside the console, or an item kind the page does not know,
	// is dropped.
	_, page = lc.get(t, "/console/unlock/"+lockedSlug+"?item=memory&next="+url.QueryEscape("https://example.com/"), cookie)
	require.NotContains(t, page, "data-locked-item")
	_, page = lc.get(t, "/console/unlock/"+lockedSlug+"?item=plan&next="+url.QueryEscape("//example.com/"), cookie)
	require.NotContains(t, page, "example.com")
}
