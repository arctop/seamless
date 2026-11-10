package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/console/browserproof"
	"github.com/arctop/seamless/internal/store"
)

// A browser sends its cookies to every port of the host that set them, so a
// program running as the owner can open the owner's browser on its own port
// and receive the unlock cookie (task 01M73SVP8HWD6KPF3ZC29RG6CT, confirmed in
// Safari, Chrome and Firefox). Replayed to the console, that cookie must open
// nothing: only a request whose proof the browser's own key signed reads the
// unlocked project.
func TestLockedConsole_CookieWithoutProofOpensNothing(t *testing.T) {
	lc := newLockedConsole(t)
	cookie := lc.unlockByTerminal(t)

	_, body := lc.get(t, "/console/memories/"+lc.memID, cookie)
	require.Contains(t, body, lockedSecret, "the browser that holds the key reads the unlocked project")
	require.Contains(t, body, `src="/console/static/browserkey.js"`, "a locked console's page loads the browser key")
	require.NotContains(t, body, "data-browser-proof", "a proven page is not marked")

	// The same cookie with no proof: everything stays locked.
	lc.unsigned = true
	for _, target := range lc.crawlTargets(t) {
		for _, asJSON := range []bool{false, true} {
			req := lc.browser(http.MethodGet, target, cookie, nil)
			if asJSON {
				req.Header.Set("Accept", "application/json")
			}
			rr := lc.do(req)
			require.Less(t, rr.Code, 500, "GET %s: %s", target, rr.Body.String())
			if !strings.Contains(target, lockedSecret) {
				require.NotContains(t, rr.Body.String(), lockedSecret, "GET %s (json=%v): the cookie alone opened the project", target, asJSON)
			}
		}
	}
	status, page := lc.get(t, "/console/unlock/"+lockedSlug, cookie)
	require.Equal(t, http.StatusOK, status)
	require.NotContains(t, page, "Unlocked in this browser")
	require.Contains(t, page, `data-browser-proof="missing"`, "the page says it was drawn without a proof, so its script asks again")
	lc.unsigned = false

	// A proof made by another key, with the right cookie: still locked.
	other, err := browserproof.NewKey()
	require.NoError(t, err)
	theirs := func(at time.Time) *http.Request {
		req := lc.browser(http.MethodGet, "/console/memories/"+lc.memID, cookie, nil)
		proof, err := browserproof.Sign(other, at)
		require.NoError(t, err)
		req.Header.Set(browserproof.Header, proof)
		return req
	}
	require.NotContains(t, lc.do(theirs(time.Now())).Body.String(), lockedSecret, "another key's proof opens nothing")

	// The right key, out of its time: still locked.
	stale := lc.browser(http.MethodGet, "/console/memories/"+lc.memID, cookie, nil)
	proof, err := browserproof.Sign(lc.key, time.Now().Add(-browserproof.Window-time.Minute))
	require.NoError(t, err)
	stale.Header.Set(browserproof.Header, proof)
	require.NotContains(t, lc.do(stale).Body.String(), lockedSecret, "a proof from outside its window opens nothing")

	// A tampered proof: still locked.
	tampered := lc.browser(http.MethodGet, "/console/memories/"+lc.memID, cookie, nil)
	tampered.Header.Set(browserproof.Header, tampered.Header.Get(browserproof.Header)+"A")
	require.NotContains(t, lc.do(tampered).Body.String(), lockedSecret)

	// The proof without the cookie: a key alone names no unlock either.
	_, body = lc.get(t, "/console/memories/"+lc.memID, "")
	require.NotContains(t, body, lockedSecret)

	// And the browser itself still reads it.
	_, body = lc.get(t, "/console/memories/"+lc.memID, cookie)
	require.Contains(t, body, lockedSecret)
}

// A hover prefetch the service worker did not sign would show the locked page
// when clicked: it is refused instead, and the click goes through the worker.
func TestLockedConsole_UnprovenPrefetchRefused(t *testing.T) {
	lc := newLockedConsole(t)
	cookie := lc.unlockByTerminal(t)

	lc.unsigned = true
	req := lc.browser(http.MethodGet, "/console/memories", cookie, nil)
	req.Header.Set("Sec-Purpose", "prefetch")
	rr := lc.do(req)
	require.Equal(t, http.StatusServiceUnavailable, rr.Code)
	require.Equal(t, "no-store", rr.Header().Get("Cache-Control"))

	// Without an unlock cookie there is nothing to prove: prefetched as before.
	req = lc.browser(http.MethodGet, "/console/memories", "", nil)
	req.Header.Set("Sec-Purpose", "prefetch")
	require.Equal(t, http.StatusOK, lc.do(req).Code)

	// With the proof, prefetched as before.
	lc.unsigned = false
	req = lc.browser(http.MethodGet, "/console/memories", cookie, nil)
	req.Header.Set("Sec-Purpose", "prefetch")
	require.Equal(t, http.StatusOK, lc.do(req).Code)
}

// Every unlock route needs the proof: an unlock's hash covers the key, so a
// begin without one could only make an unlock nothing can ever use, and a
// status or finish without one names nothing.
func TestLockedConsole_UnlockRoutesNeedTheProof(t *testing.T) {
	lc := newLockedConsole(t)
	ctx := context.Background()
	rows := func() int {
		var n int
		require.NoError(t, lc.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM console_unlocks`).Scan(&n))
		return n
	}

	lc.unsigned = true
	for _, path := range []string{"/console/unlock/" + lockedSlug + "/terminal", "/console/unlock/" + lockedSlug + "/begin"} {
		rr := lc.do(lc.browser(http.MethodPost, path, "", url.Values{}))
		require.Equal(t, http.StatusConflict, rr.Code, path)
		require.Contains(t, rr.Body.String(), "no proof of its unlock key", path)
		for _, c := range rr.Result().Cookies() {
			require.NotEqual(t, unlockCookieName, c.Name, "%s: no cookie without a proof", path)
		}
	}
	require.Zero(t, rows(), "nothing was begun")

	// A proof from a browser whose clock is off says so.
	lc.unsigned = false
	req := lc.browser(http.MethodPost, "/console/unlock/"+lockedSlug+"/terminal", "", url.Values{})
	proof, err := browserproof.Sign(lc.key, time.Now().Add(browserproof.Window+time.Minute))
	require.NoError(t, err)
	req.Header.Set(browserproof.Header, proof)
	rr := lc.do(req)
	require.Equal(t, http.StatusConflict, rr.Code)
	require.Contains(t, rr.Body.String(), "clock is more than 5 minutes off")
	require.Zero(t, rows())

	// Begun with the proof, the status endpoint answers only with it.
	begun, cookie := lc.beginTerminal(t, lockedSlug, "")
	lc.unsigned = true
	req = lc.browser(http.MethodGet, "/console/unlock/"+lockedSlug+"/terminal?id="+url.QueryEscape(begun.ID), cookie, nil)
	req.Header.Set("Accept", "application/json")
	rr = lc.do(req)
	require.Equal(t, http.StatusConflict, rr.Code)
	var got map[string]string
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	require.Empty(t, got["state"], "no state without the proof")
	require.NotEmpty(t, got["error"])
	lc.unsigned = false
	status, state := lc.terminalState(t, lockedSlug, begun.ID, cookie)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, terminalPending, state)

	// The row is bound to this browser's cookie and key together.
	u, err := store.ConsoleUnlockFor(ctx, lc.db, begun.ID, lockedSlug, lc.tokenHash(t, cookie))
	require.NoError(t, err)
	require.NotEqual(t, store.HashConsoleToken(cookie), u.TokenHash, "the cookie alone is not the row's hash")
}

// A browser whose key changed (its site data cleared, the cookie left) begins
// a fresh unlock under a fresh cookie: the old cookie's unlocks belong to the
// old key, and a new key never joins them.
func TestLockedConsole_NewKeyGetsAFreshCookie(t *testing.T) {
	lc := newLockedConsole(t)
	cookie := lc.unlockByTerminal(t)
	old := lc.key
	fresh, err := browserproof.NewKey()
	require.NoError(t, err)
	lc.key = fresh

	_, body := lc.get(t, "/console/memories/"+lc.memID, cookie)
	require.NotContains(t, body, lockedSecret, "the new key does not read the old key's unlock")
	_, again := lc.beginTerminal(t, lockedSlug, cookie)
	require.NotEqual(t, cookie, again, "the begin set a fresh cookie rather than adopt the old one")

	lc.key = old
	_, body = lc.get(t, "/console/memories/"+lc.memID, cookie)
	require.Contains(t, body, lockedSecret, "the old key still reads its own unlock")
}

// The worker and the key's script are public, like the console's other
// static files: a browser fetches the worker for updates with whatever
// cookie it has, and a worker answered with a login redirect would never
// update.
func TestConsoleStatic_ServiceWorker(t *testing.T) {
	_, mux := newConsole(t)
	for path, want := range map[string]string{"/console/sw.js": "no-cache", "/console/static/browserkey.js": "public, max-age=300"} {
		rr := do(mux, httptest.NewRequest(http.MethodGet, path, nil))
		require.Equal(t, http.StatusOK, rr.Code, path)
		require.Equal(t, "text/javascript; charset=utf-8", rr.Header().Get("Content-Type"), path)
		require.Equal(t, want, rr.Header().Get("Cache-Control"), path)
		require.Equal(t, "nosniff", rr.Header().Get("X-Content-Type-Options"), path)
	}
	sw, err := os.ReadFile("static/sw.js")
	require.NoError(t, err)
	src := string(sw)
	require.Contains(t, src, "importScripts('/console/static/browserkey.js')")
	// The worker re-sends no navigation but a GET or HEAD: re-sent from the
	// worker, a form posted from another site would pass the origin check as
	// same-origin. This pins the guard in place.
	require.Contains(t, src, "if (navigation && req.method !== 'GET' && req.method !== 'HEAD') return;")
	require.Contains(t, src, "mode: 'same-origin'", "a signed fetch never follows a redirect off this origin")
	key, err := os.ReadFile("static/browserkey.js")
	require.NoError(t, err)
	require.Contains(t, string(key), "'seamless browser proof v1\\n' + ts", "the page signs browserproof.Message")
	require.Contains(t, string(key), "s.add(key, ID)", "a second tab never replaces the first key")
	require.Contains(t, string(key), "generateKey({ name: 'ECDSA', namedCurve: 'P-256' }, false, ['sign'])", "the private key is never extractable")
	require.Equal(t, "seamless browser proof v1\n42", string(browserproof.Message(42)))
	require.Equal(t, "Seamless-Browser-Proof", browserproof.Header)
	require.Contains(t, string(key), "'"+browserproof.Header+"'")
}
