package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// UX 16 item 8: each lock-only page says what is true of it while peer_auth is
// off, and locking is findable from Privacy and the palette.

// With peer_auth off every lock-only route is a 404, and each page says its
// own sentence, for a browser and for a JSON caller alike.
func TestSwLockPagesSayTheirOwnSentence(t *testing.T) {
	_, mux := newConsole(t)
	for path, want := range map[string]string{
		"/console/approvers":                   approversOffWords,
		"/console/access":                      accessOffWords,
		"/console/access/R-AAAA":               accessOffWords,
		"/console/unlock/example-ops":          unlockOffWords,
		"/console/lock/example-ops":            lockOffWords,
		"/console/unlock/example-ops/terminal": unlockOffWords,
	} {
		rr := getPeek(t, mux, path)
		require.Equal(t, http.StatusNotFound, rr.Code, path)
		require.Contains(t, rr.Body.String(), want, path)

		req := authedReq(httptest.NewRequest(http.MethodGet, path, nil))
		req.Header.Set("Accept", "application/json")
		rr = do(mux, req)
		var out map[string]string
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out), path)
		require.Equal(t, want, out["error"], path)
	}
	for path, want := range map[string]string{
		"/console/lock/example-ops/begin":   lockOffWords,
		"/console/unlock/example-ops/begin": unlockOffWords,
		"/console/access/unlocks/x/end":     unlockOffWords,
		"/console/access/leases/x/revoke":   leaseOffWords,
		"/console/approvers/passkey/begin":  approversOffWords,
		"/console/access/R-AAAA/deny":       accessOffWords,
	} {
		req := authedReq(httptest.NewRequest(http.MethodPost, path, strings.NewReader("")))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rr := do(mux, req)
		require.Equal(t, http.StatusNotFound, rr.Code, path)
		require.Contains(t, rr.Body.String(), want, path)
	}
	// Each its own sentence.
	seen := map[string]bool{}
	for _, w := range []string{approversOffWords, accessOffWords, unlockOffWords, leaseOffWords, lockOffWords} {
		require.False(t, seen[w], w)
		seen[w] = true
	}
}

// Privacy's Locked card links every confidential or sealed project to its lock
// page while the lock pages exist (peer_auth), and the card carries the
// anchor the palette's "Lock a project" opens.
func TestSwPrivacyLinksTheLockPages(t *testing.T) {
	ctx := context.Background()
	db, mux := newApproversConsole(t)
	for slug, state := range map[string]core.Isolation{
		"web-app": core.IsolationOpen, "client-work": core.IsolationConfidential, "clean-room": core.IsolationSealed,
	} {
		_, err := store.EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
		if state != core.IsolationOpen {
			require.NoError(t, store.SetProjectIsolation(ctx, db, slug, state))
		}
	}
	d := privacyJSON(t, mux)
	var lockable []string
	for _, p := range d.Fence(fenceLocked).Lockable {
		lockable = append(lockable, p.Slug)
		require.Equal(t, "/console/lock/"+p.Slug, p.Href)
	}
	require.ElementsMatch(t, []string{"client-work", "clean-room"}, lockable, "fenced and not locked yet; open projects need a fence first")
	for _, p := range d.Fence(fenceConf).Projects {
		require.Equal(t, "/console/projects/"+p.Slug, p.Href, "the fence cards still open the project")
	}

	page := getHTMLBody(t, mux, "/console/privacy")
	require.Contains(t, page, `id="pv-fence-locked"`)
	require.Contains(t, page, `<p class="pv-lbl">Lock one on its page</p>`)
	require.Contains(t, page, `<a class="pv-proj" href="/console/lock/client-work">client-work</a>`)

	// Without peer_auth there is no lock page to link: the card says what
	// locking needs instead, and the anchor stays.
	db2, mux2 := newConsole(t)
	_, err := store.EnsureProject(ctx, db2, "client-work", "client-work")
	require.NoError(t, err)
	require.NoError(t, store.SetProjectIsolation(ctx, db2, "client-work", core.IsolationConfidential))
	require.Empty(t, privacyJSON(t, mux2).Fence(fenceLocked).Lockable)
	off := getHTMLBody(t, mux2, "/console/privacy")
	require.Contains(t, off, `id="pv-fence-locked"`)
	require.NotContains(t, off, "data-pv-lockable")
	require.NotContains(t, off, "/console/lock/")
}

// The palette offers "Lock a project", to Privacy's Locked card, while the
// level shows Privacy.
func TestSwPaletteOffersLockAProject(t *testing.T) {
	js := string(searchJS)
	require.Contains(t, js, `if (document.querySelector('nav.nav a[href="/console/privacy"]')) {`)
	require.Contains(t, js, `out.push({ title: 'Lock a project', href: '/console/privacy#pv-fence-locked' });`)
}
