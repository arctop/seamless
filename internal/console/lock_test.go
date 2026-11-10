package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/passkey/passkeytest"
	"github.com/arctop/seamless/internal/store"
)

const lockableSlug = "example-conf"

// lockable adds a confidential project nothing locks to a locked console.
func lockable(t *testing.T, lc *lockedConsole) {
	t.Helper()
	ctx := context.Background()
	_, err := store.EnsureProject(ctx, lc.db, lockableSlug, lockableSlug)
	require.NoError(t, err)
	require.NoError(t, store.SetProjectIsolation(ctx, lc.db, lockableSlug, core.IsolationConfidential))
}

func (lc *lockedConsole) lockChange(t *testing.T, key *passkeytest.Authenticator, slug string, action approval.LockAction) *httptest.ResponseRecorder {
	t.Helper()
	path := "/console/lock/" + slug
	rr := lc.do(lc.browser(http.MethodPost, path+"/begin", "", url.Values{"action": {string(action)}}))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var begun approval.Begun
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &begun))
	assertion, err := key.Get(begun.Options, approverOrigin)
	require.NoError(t, err)
	return lc.do(lc.browser(http.MethodPost, path+"/finish", "", url.Values{"id": {begun.ID}, "action": {string(action)}, "response": {string(assertion)}}))
}

// Off, nothing shows and nothing answers.
func TestLock_AbsentWithoutPeerAuth(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	_, err := store.EnsureProject(ctx, db, lockableSlug, lockableSlug)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/console/projects/"+lockableSlug, nil)
	req.AddCookie(consoleCookie())
	rr := do(mux, req)
	require.Equal(t, http.StatusOK, rr.Code)
	require.NotContains(t, rr.Body.String(), `id="lock"`)
	require.NotContains(t, rr.Body.String(), "/console/lock/")

	req = httptest.NewRequest(http.MethodGet, "/console/lock/"+lockableSlug, nil)
	req.AddCookie(consoleCookie())
	require.Equal(t, http.StatusNotFound, do(mux, req).Code)
	for _, suffix := range []string{"/begin", "/finish"} {
		post := authedReq(httptest.NewRequest(http.MethodPost, "/console/lock/"+lockableSlug+suffix, strings.NewReader("")))
		post.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		require.Equal(t, http.StatusNotFound, do(mux, post).Code, suffix)
	}
}

// With no identity set up, the control is a link to the Approvers page and the
// ceremony will not begin.
func TestLock_NeedsAnEnrolledPasskey(t *testing.T) {
	lc := newLockedConsole(t)
	lockable(t, lc)

	for _, target := range []string{"/console/projects/" + lockableSlug, "/console/lock/" + lockableSlug} {
		_, page := lc.get(t, target, "")
		require.Contains(t, page, "data-needs-passkey", target)
		require.Contains(t, page, `href="/console/approvers"`, target)
		require.NotContains(t, page, `id="lock-form"`, target)
	}
	rr := lc.do(lc.browser(http.MethodPost, "/console/lock/"+lockableSlug+"/begin", "", url.Values{"action": {"lock"}}))
	require.Equal(t, http.StatusConflict, rr.Code)
	require.Contains(t, rr.Body.String(), "Approvers page")
}

func TestLock_LockThenRemove(t *testing.T) {
	lc := newLockedConsole(t)
	lockable(t, lc)
	ctx := context.Background()
	key, err := passkeytest.New(true)
	require.NoError(t, err)
	enrollPasskeyByCode(t, lc.db, lc.svc, key, "key")

	_, page := lc.get(t, "/console/projects/"+lockableSlug, "")
	require.Contains(t, page, `href="/console/lock/`+lockableSlug+`"`)
	_, page = lc.get(t, "/console/lock/"+lockableSlug, "")
	require.Contains(t, page, `id="lock-form"`)
	require.Contains(t, page, `data-action="lock"`)

	rr := lc.lockChange(t, key, lockableSlug, approval.LockActionAdd)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	require.Contains(t, rr.Body.String(), `"restarting": true`)
	require.Equal(t, 1, lc.restarts, "the daemon restarts to apply it")
	has, err := store.HasConsoleLock(ctx, lc.db, lockableSlug)
	require.NoError(t, err)
	require.True(t, has)

	_, page = lc.get(t, "/console/lock/"+lockableSlug, "")
	require.Contains(t, page, `data-action="remove"`)
	require.Contains(t, page, "takes effect when Seamless next restarts", "the running daemon has not applied it")

	rr = lc.lockChange(t, key, lockableSlug, approval.LockActionRemove)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	require.Equal(t, 2, lc.restarts)
	has, err = store.HasConsoleLock(ctx, lc.db, lockableSlug)
	require.NoError(t, err)
	require.False(t, has)
}

// The bearer key and the socket cannot start a change: it needs a browser, for
// the passkey, however the request is signed in.
func TestLock_NeedsABrowser(t *testing.T) {
	lc := newLockedConsole(t)
	lockable(t, lc)
	key, err := passkeytest.New(true)
	require.NoError(t, err)
	enrollPasskeyByCode(t, lc.db, lc.svc, key, "key")

	req := authedReq(httptest.NewRequest(http.MethodPost, "/console/lock/"+lockableSlug+"/begin", strings.NewReader("action=lock")))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	require.Equal(t, http.StatusForbidden, lc.do(req).Code)

	rr := lc.do(lc.browser(http.MethodPost, "/console/lock/"+lockableSlug+"/begin", "", url.Values{"action": {"open"}}))
	require.Equal(t, http.StatusBadRequest, rr.Code, "an unknown action is refused, not defaulted")
}

// A force-locked project reads as such and refuses a change in both directions.
func TestLock_ForceLockedStaysInTheFile(t *testing.T) {
	lc := newLockedConsole(t)
	key, err := passkeytest.New(true)
	require.NoError(t, err)
	enrollPasskeyByCode(t, lc.db, lc.svc, key, "key")

	_, page := lc.get(t, "/console/lock/"+lockedSlug, "")
	require.Contains(t, page, "peer_auth.force_locked")
	require.NotContains(t, page, `id="lock-form"`)
	_, page = lc.get(t, "/console/unlock/"+lockedSlug, "")
	require.Contains(t, page, "Force locked in the daemon")
	require.NotContains(t, page, "Remove the lock")
	require.NotContains(t, page, lockedSecret)

	for _, action := range []string{"lock", "remove"} {
		rr := lc.do(lc.browser(http.MethodPost, "/console/lock/"+lockedSlug+"/begin", "", url.Values{"action": {action}}))
		require.Equal(t, http.StatusConflict, rr.Code, action)
		require.Contains(t, rr.Body.String(), "force locked")
	}
	require.Zero(t, lc.restarts)
}

// A project with no fence offers no lock.
func TestLock_NeedsAFence(t *testing.T) {
	lc := newLockedConsole(t)
	key, err := passkeytest.New(true)
	require.NoError(t, err)
	enrollPasskeyByCode(t, lc.db, lc.svc, key, "key")

	_, page := lc.get(t, "/console/lock/"+openSlug, "")
	require.Contains(t, page, "needs an isolation fence")
	require.NotContains(t, page, `id="lock-form"`)
	rr := lc.do(lc.browser(http.MethodPost, "/console/lock/"+openSlug+"/begin", "", url.Values{"action": {"lock"}}))
	require.Equal(t, http.StatusConflict, rr.Code)
	require.Contains(t, rr.Body.String(), "confidential or sealed")
}

// A locked project keeps its fence: opening it would otherwise leave the daemon
// refusing to start.
func TestLock_FenceStaysUp(t *testing.T) {
	lc := newLockedConsole(t)
	lockable(t, lc)
	ctx := context.Background()
	key, err := passkeytest.New(true)
	require.NoError(t, err)
	enrollPasskeyByCode(t, lc.db, lc.svc, key, "key")
	rr := lc.lockChange(t, key, lockableSlug, approval.LockActionAdd)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	_, page := lc.get(t, "/console/projects/"+lockableSlug, "")
	require.Contains(t, page, "must stay confidential or sealed", "the open option is off")

	open := func(slug, unlock string) *httptest.ResponseRecorder {
		req := lc.browser(http.MethodPost, "/console/projects/"+slug+"/isolation", unlock, url.Values{"state": {"open"}})
		req.Header.Set("Accept", "application/json")
		return lc.do(req)
	}
	rr = open(lockableSlug, "")
	require.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
	require.Contains(t, rr.Body.String(), "remove the lock first")
	state, err := store.IsolationOf(ctx, lc.db, lockableSlug)
	require.NoError(t, err)
	require.Equal(t, core.IsolationConfidential, state)

	rr = open(lockedSlug, lc.unlockByTerminal(t))
	require.Equal(t, http.StatusConflict, rr.Code, "a force-locked project too, from a browser that unlocked it: %s", rr.Body.String())
	state, err = store.IsolationOf(ctx, lc.db, lockedSlug)
	require.NoError(t, err)
	require.Equal(t, core.IsolationConfidential, state)
}
