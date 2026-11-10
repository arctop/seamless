package main

// Step 10's console rows: the console's browsers and socket callers against
// locked projects. The console is an owner surface, so the
// owner's own browser reads a locked project only once THAT browser is
// unlocked (a console unlock: approval-backed, cookie-scoped), and a socket
// caller reads what its agent tree's lease opens. A restricted local account
// has no console at all.
//
// The fixture (peerauth_console_matrix_helpers_test.go): example-finance (confidential)
// and example-legal (open) are locked; a leased agent planted both; the
// owner's example-open is not locked.

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/mcp"
	"github.com/arctop/seamless/internal/passkey/passkeytest"
	"github.com/arctop/seamless/internal/plans"
	"github.com/arctop/seamless/internal/store"
)

// crawl GETs every target as c, HTML and JSON. Nothing below a 500; no
// locked project's secret or item id unless the project is in open (this
// caller reads it), and then the secrets it saw are reported. sawOpen says
// whether the owner's own content showed somewhere.
func (d *cmDaemon) crawl(t *testing.T, c cmCaller, targets []string, open ...string) (seen map[string]bool, sawOpen bool) {
	t.Helper()
	seen = map[string]bool{}
	for _, target := range targets {
		for _, asJSON := range []bool{false, true} {
			status, body, hdr := c.do(t, http.MethodGet, target, nil, asJSON)
			what := c.name + " GET " + target
			if asJSON {
				what += " (json)"
			}
			// Non-fatal: a failing route is a finding, and the crawl goes on
			// to judge every other one.
			assert.Less(t, status, 500, "%s failed: %s", what, firstN(body, 300))
			d.noLockedContent(t, body, target, what, open...)
			d.noLockedContent(t, hdr.Get("Location"), target, what+" redirect", open...)
			for _, it := range d.secrets() {
				seen[it.project] = seen[it.project] || strings.Contains(body, it.secret)
			}
			sawOpen = sawOpen || strings.Contains(body, cmOpenMarker)
		}
	}
	return seen, sawOpen
}

// postTargets is every console POST route with the form each one reads,
// filled with each locked project's items. The login and logout forms (the
// browser's own credential) and the update controls (no update check runs
// here) are left out.
func (d *cmDaemon) postTargets(t *testing.T, pendingID string) []struct {
	target string
	form   url.Values
} {
	t.Helper()
	_, posts := consoleRoutes(t)
	var out []struct {
		target string
		form   url.Values
	}
	seen := map[string]bool{}
	add := func(target string, form url.Values) {
		if seen[target] {
			return
		}
		seen[target] = true
		out = append(out, struct {
			target string
			form   url.Values
		}{target, form})
	}
	for _, pattern := range posts {
		if pattern == "/console/login" || pattern == "/console/logout" || strings.HasPrefix(pattern, "/console/settings/updates") {
			continue
		}
		for _, it := range d.secrets() {
			note, ok, err := store.NoteByID(context.Background(), d.db, it.noteID)
			require.NoError(t, err)
			require.True(t, ok)
			loosen := "open"
			if it.project == cmLegal {
				loosen = "confidential" // a tighten, confirmed
			}
			p := it.project
			forms := map[string]url.Values{
				"/console/projects/{slug}/isolation":   {"state": {loosen}, "confirm": {"1"}},
				"/console/favorites/{kind}/{id}":       {"favorite": {"1"}},
				"/console/gardener/{id}/retarget":      {"to": {cmOpen}},
				"/console/gardener/request":            {"request": {"tidy " + p}, "project": {p}},
				"/console/gardener/split":              {"source": {p}},
				"/console/settings/families/save":      {"name": {"fam"}, "members": {p + "," + cmOpen}},
				"/console/settings/families/delete":    {"name": {"fam"}},
				"/console/settings/briefing/preview":   {"project": {p}},
				"/console/gardener/group/dismiss":      {"kind": {"archive"}},
				"/console/settings/level":              {"level": {"advanced"}},
				"/console/settings/features":           {"feature_research": {"1"}, "feature_momentum": {"1"}, "feature_gamification": {"1"}, "feature_screening": {"1"}},
				"/console/settings/embeddings/mode":    {"mode": {"auto"}},
				"/console/settings/embeddings/reembed": {},
				"/console/held/lockdown":               {"on": {"0"}},
				"/console/held/corpus/safe":            {"path": {note.FilePath}},
				"/console/held/corpus/accept":          {"path": {note.FilePath}},
				"/console/access/request":              {"project": {p}},
				"/console/access/{id}/approve":         {"duration": {"1h"}, "digest": {"0000"}, "response": {"{}"}},
				"/console/unlock/{project}/finish":     {"id": {cmNoSuch}, "response": {"{}"}},
				"/console/lock/{project}/begin":        {"action": {"lock"}},
				"/console/lock/{project}/finish":       {"id": {cmNoSuch}, "action": {"lock"}, "response": {"{}"}},
				"/console/approvers/passkey/begin":     {"name": {"probe"}, "by_code": {"1"}},
				"/console/approvers/passkey/finish":    {"id": {cmNoSuch}, "response": {"{}"}},
				"/console/approvers/passkey/approve":   {"id": {cmNoSuch}, "response": {"{}"}},
				"/console/approvers/passkey/confirm":   {"id": {cmNoSuch}, "code": {"AAAA-BBBB-CCCC-DDDD"}},
				"/console/approvers/touchid/enroll":    {"name": {"probe"}, "public_key": {"AAAA"}},
			}
			form := forms[pattern]
			if form == nil {
				form = url.Values{}
			}
			add(d.cmFill(pattern, it, pendingID), form)
			if pattern == "/console/favorites/{kind}/{id}" {
				for kind, id := range map[string]string{"note": it.noteID, "task": it.taskID, "session": it.sessID,
					"trial": it.trialID, "project": it.project, "plan": it.plan} {
					add("/console/favorites/"+kind+"/"+id, form)
				}
			}
			if pattern == "/console/memories/{id}/archive" {
				add("/console/memories/"+it.victimMemID+"/archive", form)
			}
			if pattern == "/console/tasks/{id}/release" {
				add("/console/tasks/"+it.victimTaskID+"/release", form)
			}
			if pattern == "/console/held/{id}/release" {
				add("/console/held/"+it.holdID+"/discard", form)
			}
		}
	}
	return out
}

// postCrawl POSTs every console route as c, HTML and JSON, and checks the
// state after each: no locked item changed, nothing granted. The answer
// carries no locked content either.
func (d *cmDaemon) postCrawl(t *testing.T, c cmCaller, pendingID string) {
	t.Helper()
	before := d.counts(t)
	for _, p := range d.postTargets(t, pendingID) {
		for _, asJSON := range []bool{false, true} {
			what := c.name + " POST " + p.target
			_, body, hdr := c.do(t, http.MethodPost, p.target, p.form, asJSON)
			said := p.target + " " + p.form.Encode() // an echo of the caller's own form discloses nothing
			d.noLockedContent(t, body, said, what)
			d.noLockedContent(t, hdr.Get("Location"), said, what+" redirect")
			d.itemsUnchanged(t, d.fin, true, what)
			d.itemsUnchanged(t, d.legal, true, what)
			require.Equal(t, before, d.counts(t), "%s changed the access tables", what)
		}
	}
	pending, err := store.AccessApprovalByID(context.Background(), d.db, d.pendingID(t))
	require.NoError(t, err)
	require.Equal(t, store.ApprovalPending, pending.Status, "%s decided the pending request", c.name)
}

// pendingID is the stranger tree's pending request.
func (d *cmDaemon) pendingID(t *testing.T) string {
	t.Helper()
	pending, err := d.svc.Pending(context.Background())
	require.NoError(t, err)
	require.Len(t, pending, 1, "one request waits: the unleased tree's")
	return pending[0].ID
}

// cmFeed subscribes c to the console's live feed and returns its frames.
func cmFeed(t *testing.T, c cmCaller) <-chan string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/console/events", nil)
	require.NoError(t, err)
	c.decorate(t, req)
	client := c.client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusOK, resp.StatusCode, c.name+" opens the live feed")
	frames := make(chan string, 1024)
	go func() {
		defer close(frames)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
		for sc.Scan() {
			frames <- sc.Text()
		}
	}()
	require.Eventually(t, func() bool {
		select {
		case f := <-frames:
			return strings.HasPrefix(f, ": connected")
		default:
			return false
		}
	}, 5*time.Second, 10*time.Millisecond, c.name+"'s feed connects")
	return frames
}

// cmUntilMarker reads frames up to the one naming marker, failing on a deadline.
func cmUntilMarker(t *testing.T, frames <-chan string, marker string) []string {
	t.Helper()
	var got []string
	deadline := time.After(10 * time.Second)
	for {
		select {
		case f, open := <-frames:
			require.True(t, open, "the feed closed before the marker")
			got = append(got, f)
			if strings.Contains(f, marker) {
				return got
			}
		case <-deadline:
			t.Fatalf("the owner's marker %s never reached the feed", marker)
		}
	}
}

func TestPeerAuthMatrix_Console(t *testing.T) {
	ctx := context.Background()
	restricted := access.Account{UID: cmUIDRestricted, Name: cmRestricted, Home: "/Users/" + cmRestricted}
	d := cmNewDaemon(t, access.Account{UID: cmUIDOwner, Name: cmOwnerName, Home: "/Users/" + cmOwnerName}, &restricted)

	agent := access.ProcessToken{PID: 200, Version: 2000, Boot: cmBoot}
	bridge := access.ProcessToken{PID: 300, Version: 3000, Boot: cmBoot}
	cliTok := access.ProcessToken{PID: 320, Version: 3200, Boot: cmBoot}
	stranger := access.ProcessToken{PID: 400, Version: 4000, Boot: cmBoot}

	// The agent's session, stamped with its process by the SessionStart hook,
	// in the owner's open project; its requests are filed at the lock and
	// approved on the terminal path, one lease per locked project.
	sessID, err := core.NewID()
	require.NoError(t, err)
	now := time.Now().UTC()
	require.NoError(t, store.CreateSession(ctx, d.db, core.Session{
		ID: sessID, Name: "cc/cm-agent", ProjectSlug: cmOpen, Status: core.SessionActive, Ambient: true,
		ExternalSessionID: "cm-agent-x", ExternalClient: "claude-code", CWD: "/work/open", CreatedAt: now, UpdatedAt: now,
	}))
	require.NoError(t, store.SetSessionAgentProcess(ctx, d.db, sessID, agent.AgentID()))
	agentMCP := paClientHeaders(t, d.socketAs(t, cmUIDOwner, bridge, agent).URL, "",
		map[string]string{mcp.AgentProcessHeader: agent.AgentID(), mcp.HostHeader: d.host})
	d.leaseFor(t, agentMCP, cmFinance)
	legalLease := d.leaseFor(t, agentMCP, cmLegal)
	d.fin = d.plant(t, agentMCP, cmFinance, cmFinSecret, "fin")
	d.legal = d.plant(t, agentMCP, cmLegal, cmLegalSecret, "legal")
	d.plantOpen(t)

	// Another tree of the owner's asks too: its request waits, pending. Its
	// probe names no item, so the owner's own event log holds no locked
	// identifier the crawl would then find.
	strangerMCP := paClientHeaders(t, d.socketAs(t, cmUIDOwner, stranger).URL, "",
		map[string]string{mcp.AgentProcessHeader: stranger.AgentID(), mcp.HostHeader: d.host})
	text, isErr := paCall(t, strangerMCP, "tasks_list", map[string]any{"project": cmFinance})
	require.True(t, isErr, text)
	require.NotEmpty(t, cmShortRe.FindString(text), "the unleased tree's request is filed: %s", text)
	pendingID := d.pendingID(t)
	targets := d.getTargets(t, pendingID)
	t.Logf("crawl: %d GET targets (each HTML and JSON), %d POST targets", len(targets), len(d.postTargets(t, pendingID)))

	browserA := d.newBrowser(t, "browser A")
	tcpKey := d.bearer()
	unleased := cmSocketCaller("an unleased tree", d.socketAs(t, cmUIDOwner, stranger))
	noChain := cmSocketCaller("a socket connection with no process", d.socketAs(t, cmUIDOwner))
	leased := cmSocketCaller("the leased tree", d.socketAs(t, cmUIDOwner, cliTok, agent))
	restrictedC := cmSocketCaller("the restricted account", d.socketAs(t, cmUIDRestricted))

	t.Run("a locked browser: every GET route", func(t *testing.T) {
		_, sawOpen := d.crawl(t, browserA, targets)
		require.True(t, sawOpen, "the crawl reached the owner's own content: it is not an empty console")

		// Locked rows render as locked: listed by name with their counts,
		// the workspace sends a browser to the unlock page and tells a
		// script it is locked.
		status, body, _ := browserA.do(t, http.MethodGet, "/console/projects?format=json", nil, true)
		require.Equal(t, http.StatusOK, status)
		var board struct {
			Locked []store.ProjectTally `json:"locked"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &board))
		var slugs []string
		for _, p := range board.Locked {
			slugs = append(slugs, p.Slug)
			require.Positive(t, p.Memories, "%s shows its counts", p.Slug)
		}
		require.ElementsMatch(t, []string{cmFinance, cmLegal}, slugs)
		for _, p := range []string{cmFinance, cmLegal} {
			status, _, hdr := browserA.do(t, http.MethodGet, "/console/projects/"+p, nil, false)
			require.Equal(t, http.StatusSeeOther, status)
			require.Equal(t, "/console/unlock/"+p, hdr.Get("Location"))
			status, body, _ = browserA.do(t, http.MethodGet, "/console/projects/"+p, nil, true)
			require.Equal(t, http.StatusForbidden, status)
			require.Contains(t, body, "is locked")
			// The page unlocks in place (console-data-updates-never-reload-document).
			status, body, _ = browserA.do(t, http.MethodGet, "/console/unlock/"+p, nil, false)
			require.Equal(t, http.StatusOK, status)
			require.NotContains(t, body, "location.reload(")
			page := cmUnlockPage(t, browserA, p)
			require.False(t, page.Unlocked)
			require.False(t, page.Leased)
			require.Equal(t, p, page.Project.Slug, "the unlock page names the project")
		}
	})

	t.Run("a locked browser: every POST route", func(t *testing.T) {
		d.postCrawl(t, browserA, pendingID)
		// A cross-origin write is refused before it runs
		// (console-csrf-origin-check-contract), here on an open item.
		req, err := http.NewRequest(http.MethodPost, d.owner.URL+"/console/memories/"+d.openMemID+"/archive", strings.NewReader(""))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", "http://127.0.0.1:1") // another local port: same site, not same origin
		resp, err := browserA.client.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusForbidden, resp.StatusCode)
		mem, ok, err := store.MemoryByID(ctx, d.db, d.openMemID)
		require.NoError(t, err)
		require.True(t, ok)
		require.Nil(t, mem.InvalidAt, "the cross-origin write did nothing")
	})

	t.Run("the TCP key: every GET and POST route", func(t *testing.T) {
		d.crawl(t, tcpKey, targets)
		d.postCrawl(t, tcpKey, pendingID)
	})

	t.Run("an unleased tree on the socket: every GET and POST route", func(t *testing.T) {
		d.crawl(t, unleased, targets)
		d.postCrawl(t, unleased, pendingID)
		d.crawl(t, noChain, targets)
	})

	t.Run("the leased tree on the socket reads the lease-chosen view", func(t *testing.T) {
		seen, _ := d.crawl(t, leased, targets, cmFinance, cmLegal)
		require.True(t, seen[cmFinance] && seen[cmLegal], "the leased tree reads both projects somewhere: %v", seen)
		for _, it := range d.secrets() {
			for _, target := range []string{
				"/console/memories/" + it.memID, "/console/notes/" + it.noteID, "/console/tasks/" + it.taskID,
				"/console/sessions/" + it.sessID, "/console/plans/" + it.capture, "/console/trials/" + it.trialID,
			} {
				status, body, _ := leased.do(t, http.MethodGet, target+"?format=json", nil, true)
				require.Equal(t, http.StatusOK, status, "%s: %s", target, firstN(body, 200))
				require.Contains(t, body, it.secret, "the leased tree's seam reads %s", target)
			}
			page := cmUnlockPage(t, leased, it.project)
			require.True(t, page.Leased, "the unlock page says the tree reads it through its lease")
			require.False(t, page.Unlocked)
		}
		// An agent's tree may not revoke, lock again or deny: the owner's.
		for _, target := range []string{
			"/console/access/leases/" + legalLease.ID + "/revoke", "/console/access/" + pendingID + "/deny",
		} {
			status, _, _ := leased.do(t, http.MethodPost, target, url.Values{}, true)
			require.Equal(t, http.StatusForbidden, status, target)
		}
		live, err := store.LiveAccessLeases(ctx, d.db, time.Now())
		require.NoError(t, err)
		require.Len(t, live, 2)
	})

	t.Run("the restricted account: every console route refused", func(t *testing.T) {
		for _, target := range targets {
			for _, asJSON := range []bool{false, true} {
				status, body, _ := restrictedC.do(t, http.MethodGet, target, nil, asJSON)
				// The login form, the static assets and the service worker
				// are public: served to anyone, holding nothing of anyone's.
				switch {
				case strings.HasPrefix(target, "/console/static/"), target == "/console/sw.js":
					require.Equal(t, http.StatusOK, status, "GET %s", target)
				case target == "/console/login":
					// The login form sends a caller the kernel already
					// authenticated on to the console, which refuses it.
					require.Contains(t, []int{http.StatusOK, http.StatusSeeOther}, status, "GET %s", target)
				default:
					require.Equal(t, http.StatusForbidden, status, "GET %s", target)
				}
				d.noLockedContent(t, body, target, "restricted GET "+target)
			}
		}
		for _, p := range d.postTargets(t, pendingID) {
			status, body, _ := restrictedC.do(t, http.MethodPost, p.target, p.form, true)
			require.Equal(t, http.StatusForbidden, status, "POST %s", p.target)
			d.noLockedContent(t, body, p.target, "restricted POST "+p.target)
		}
		d.itemsUnchanged(t, d.fin, true, "the restricted account's console")
		d.itemsUnchanged(t, d.legal, true, "the restricted account's console")
	})

	t.Run("no console route mints an enrollment code or grants an unlock by itself", func(t *testing.T) {
		c := d.counts(t)
		require.Zero(t, c.enrollCodes, "only seamlessd approvers enroll-code mints one")
		require.Zero(t, c.unlocks, "no unlock granted: no passkey is enrolled and no terminal approved a code")
		require.Equal(t, 2, c.leases, "the agent's two leases and nothing else")
		require.Equal(t, 2, c.approved)
	})

	// The real unlock flow: a passkey enrolled with a code only the daemon's
	// account mints (minted here straight into the store, as `seamlessd
	// approvers enroll-code` does), then the console's passkey ceremony in
	// browser A for example-finance only.
	key, err := passkeytest.New(true)
	require.NoError(t, err)
	var finUnlock string
	t.Run("the same browser, unlocked with a passkey", func(t *testing.T) {
		status, body, _ := browserA.do(t, http.MethodPost, "/console/approvers/passkey/begin", url.Values{"name": {"cm-key"}, "by_code": {"1"}}, true)
		require.Equal(t, http.StatusOK, status, body)
		var begun struct {
			ID      string          `json:"id"`
			Options json.RawMessage `json:"options"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &begun))
		created, err := key.Create(begun.Options, cmOrigin)
		require.NoError(t, err)
		status, body, _ = browserA.do(t, http.MethodPost, "/console/approvers/passkey/finish", url.Values{"id": {begun.ID}, "response": {string(created)}}, true)
		require.Equal(t, http.StatusOK, status, body)
		var fin struct {
			Fingerprint string `json:"fingerprint"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &fin))
		code, _, err := store.MintEnrollCode(ctx, d.db, store.ApproverPasskey, fin.Fingerprint, time.Now())
		require.NoError(t, err)
		status, body, _ = browserA.do(t, http.MethodPost, "/console/approvers/passkey/confirm", url.Values{"id": {begun.ID}, "code": {code}}, true)
		require.Equal(t, http.StatusOK, status, body)
		require.Contains(t, body, `"enrolled"`)

		status, body, _ = browserA.do(t, http.MethodPost, "/console/unlock/"+cmFinance+"/begin", url.Values{}, true)
		require.Equal(t, http.StatusOK, status, body)
		require.NoError(t, json.Unmarshal([]byte(body), &begun))
		cookie := d.unlockCookie(t, browserA)
		require.NotEmpty(t, cookie, "begin issues this browser its unlock cookie")
		assertion, err := key.Get(begun.Options, cmOrigin)
		require.NoError(t, err)
		status, body, _ = browserA.do(t, http.MethodPost, "/console/unlock/"+cmFinance+"/finish", url.Values{"id": {begun.ID}, "response": {string(assertion)}}, true)
		require.Equal(t, http.StatusOK, status, body)
		require.Contains(t, body, `"unlocked": "`+cmFinance+`"`)
		live, err := store.LiveConsoleUnlocks(ctx, d.db, browserA.unlockHash(t, cookie), time.Now())
		require.NoError(t, err)
		require.Len(t, live, 1)
		finUnlock = live[0].ID

		// The secrets render in this browser, the finance project's only.
		seen, _ := d.crawl(t, browserA, targets, cmFinance)
		require.True(t, seen[cmFinance], "the unlocked browser reads example-finance")
		require.False(t, seen[cmLegal], "example-legal stays locked: the unlock is per project")
		for _, target := range []string{
			"/console/memories/" + d.fin.memID, "/console/notes/" + d.fin.noteID, "/console/tasks/" + d.fin.taskID,
			"/console/sessions/" + d.fin.sessID, "/console/plans/" + d.fin.capture, "/console/memories?project=" + cmFinance,
			"/console/projects/" + cmFinance + "?tab=memories",
		} {
			status, body, _ := browserA.do(t, http.MethodGet, target, nil, false)
			require.Equal(t, http.StatusOK, status, target)
			require.True(t, strings.Contains(body, d.fin.secret) || strings.Contains(body, d.fin.memName), "%s renders the unlocked project", target)
		}
		page := cmUnlockPage(t, browserA, cmFinance)
		require.True(t, page.Unlocked)
		require.False(t, page.Leased)
		require.False(t, cmUnlockPage(t, browserA, cmLegal).Unlocked)
		c := d.counts(t)
		require.Equal(t, 2, c.leases, "an unlock is a cookie, never a lease")
	})

	browserB := d.newBrowser(t, "browser B")
	t.Run("another browser while the first is unlocked: still locked everywhere", func(t *testing.T) {
		d.crawl(t, browserB, targets)
		d.postCrawl(t, browserB, pendingID)
		// A forged unlock cookie opens nothing.
		forged, err := store.NewConsoleToken()
		require.NoError(t, err)
		u, err := url.Parse(d.owner.URL + "/console/")
		require.NoError(t, err)
		browserB.client.Jar.SetCookies(u, []*http.Cookie{{Name: "seamless_unlock", Value: forged, Path: "/console"}})
		_, body, _ := browserB.do(t, http.MethodGet, "/console/memories/"+d.fin.memID, nil, false)
		require.NotContains(t, body, d.fin.secret, "a forged cookie opens nothing")
		// The TCP key presenting browser A's unlock cookie: an unlock is a
		// browser's, never the key's.
		withA := tcpKey
		cookie := d.unlockCookie(t, browserA)
		withA.auth = func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+cmKey)
			r.AddCookie(&http.Cookie{Name: "seamless_unlock", Value: cookie})
		}
		withA.name = "the TCP key with browser A's unlock cookie"
		d.crawl(t, withA, targets)
		// Browser A's own cookies, read off another port of this host and
		// replayed without A's key (task 01M73SVP8HWD6KPF3ZC29RG6CT): locked.
		replay := browserA
		replay.key = nil
		replay.name = "browser A's cookies replayed without its key"
		d.crawl(t, replay, targets)
		// Browser B with A's unlock cookie and its own key: locked.
		browserB.client.Jar.SetCookies(u, []*http.Cookie{{Name: "seamless_unlock", Value: cookie, Path: "/console"}})
		_, body, _ = browserB.do(t, http.MethodGet, "/console/memories/"+d.fin.memID, nil, false)
		require.NotContains(t, body, d.fin.secret, "another browser's key does not take up A's cookie")
		_, body, _ = browserA.do(t, http.MethodGet, "/console/memories/"+d.fin.memID, nil, false)
		require.Contains(t, body, d.fin.secret, "browser A, with its key, still reads it")
		// Nor does it reach an agent: the unleased tree's tools stay locked.
		text, isErr := paCall(t, strangerMCP, "tasks_list", map[string]any{"project": cmFinance})
		require.True(t, isErr)
		require.Contains(t, text, "locked")
		require.NotContains(t, text, d.fin.secret)
		d.crawl(t, unleased, targets)
	})

	t.Run("the live feed: the unlocked browser alone sees the unlocked project", func(t *testing.T) {
		feedA, feedB := cmFeed(t, browserA), cmFeed(t, browserB)
		feedKey := cmFeed(t, tcpKey)
		// Each write names its own project's session, as an agent working there
		// is bound: an event tied to a session of a project still locked in
		// this browser is hidden by that session.
		fin := cmCallOK(t, agentMCP, "memory_write", map[string]any{"session": d.fin.sessName, "project": cmFinance, "name": "fin-live", "kind": "gotcha",
			"description": "live " + cmFinSecret, "body": "live " + cmFinSecret})
		cmCallOK(t, agentMCP, "memory_write", map[string]any{"session": d.legal.sessName, "project": cmLegal, "name": "legal-live", "kind": "gotcha",
			"description": "live " + cmLegalSecret, "body": "live " + cmLegalSecret})
		own := paClient(t, d.owner.URL, cmKey)
		paCallOK(t, own, "memory_write", map[string]any{"project": cmOpen, "name": "owner-marker", "kind": "gotcha",
			"description": "owner-marker", "body": "owner-marker"})
		finLive := fin["id"].(string)
		sawFin := false
		framesA := cmUntilMarker(t, feedA, "owner-marker")
		for _, f := range framesA {
			require.NotContains(t, f, cmLegalSecret, "browser A's feed carried example-legal")
			require.NotContains(t, f, "legal-live")
			sawFin = sawFin || strings.Contains(f, finLive)
		}
		require.True(t, sawFin, "browser A's feed carries the project it unlocked: %v", framesA)
		for name, feed := range map[string]<-chan string{"browser B": feedB, "the TCP key": feedKey} {
			for _, f := range cmUntilMarker(t, feed, "owner-marker") {
				require.NotContains(t, f, cmFinSecret, "%s's feed carried example-finance", name)
				require.NotContains(t, f, cmLegalSecret, "%s's feed carried example-legal", name)
				require.NotContains(t, f, finLive, "%s's feed named the locked memory", name)
				d.noLockedContent(t, f, "", name+" feed frame")
			}
		}
	})

	t.Run("writes the unlock allows land; the still-locked project's do not", func(t *testing.T) {
		for _, w := range []struct {
			target string
			form   url.Values
		}{
			{"/console/favorites/memory/" + d.fin.victimMemID, url.Values{"favorite": {"1"}}},
			{"/console/memories/" + d.fin.victimMemID + "/archive", url.Values{}},
			{"/console/tasks/" + d.fin.victimTaskID + "/release", url.Values{}},
			{"/console/plans/" + d.fin.capture + "/approve", url.Values{}},
		} {
			status, body, _ := browserA.do(t, http.MethodPost, w.target, w.form, true)
			require.Less(t, status, 400, "POST %s: %s", w.target, firstN(body, 200))
		}
		mem, ok, err := store.MemoryByID(ctx, d.db, d.fin.victimMemID)
		require.NoError(t, err)
		require.True(t, ok)
		require.True(t, mem.Favorite, "the unlocked browser starred it")
		require.NotNil(t, mem.InvalidAt, "the unlocked browser archived it")
		task, err := store.TaskByID(ctx, d.db, d.fin.victimTaskID)
		require.NoError(t, err)
		require.Equal(t, core.TaskOpen, task.Status, "the unlocked browser force-released it")
		capture, ok, err := store.NoteByID(ctx, d.db, d.fin.captureNoteID)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, plans.StatusApproved, plans.StatusFromTags(capture.Tags), "the unlocked browser approved the plan")
		d.itemsUnchanged(t, d.fin, false, "the unlocked browser's writes")

		for _, target := range []string{
			"/console/favorites/memory/" + d.legal.victimMemID, "/console/memories/" + d.legal.victimMemID + "/archive",
			"/console/tasks/" + d.legal.victimTaskID + "/release", "/console/plans/" + d.legal.capture + "/approve",
		} {
			_, body, _ := browserA.do(t, http.MethodPost, target, url.Values{"favorite": {"1"}}, true)
			d.noLockedContent(t, body, target, "browser A POST "+target, cmFinance)
			d.itemsUnchanged(t, d.legal, true, "browser A POST "+target)
		}
	})

	var legalUnlock string
	t.Run("the terminal path unlocks a second project in the same browser", func(t *testing.T) {
		status, body, _ := browserA.do(t, http.MethodPost, "/console/unlock/"+cmLegal+"/terminal", url.Values{}, true)
		require.Equal(t, http.StatusOK, status, body)
		var begun struct{ ID, Code string }
		require.NoError(t, json.Unmarshal([]byte(body), &begun))
		// The code is read off the terminal by the leased tree too: it opens
		// nothing for it, and only the daemon's account approves it.
		status, _, _ = leased.do(t, http.MethodGet, "/console/unlock/"+cmLegal+"/terminal?id="+url.QueryEscape(begun.ID), nil, true)
		require.Equal(t, http.StatusForbidden, status, "a socket caller reads no browser's unlock")
		require.NoError(t, approveConsoleUnlock(ctx, d.cfg, d.db, d.svc, unlockTerminal{out: io.Discard}, cmLegal, begun.Code, ""))
		status, body, _ = browserA.do(t, http.MethodGet, "/console/unlock/"+cmLegal+"/terminal?id="+url.QueryEscape(begun.ID), nil, true)
		require.Equal(t, http.StatusOK, status, body)
		require.Contains(t, body, `"state": "live"`)
		_, body, _ = browserA.do(t, http.MethodGet, "/console/unlock/"+cmLegal, nil, false)
		require.Contains(t, body, "Unlocked in this browser")
		_, body, _ = browserA.do(t, http.MethodGet, "/console/memories/"+d.legal.memID, nil, false)
		require.Contains(t, body, d.legal.secret)
		live, err := store.LiveConsoleUnlocks(ctx, d.db, browserA.unlockHash(t, d.unlockCookie(t, browserA)), time.Now())
		require.NoError(t, err)
		require.Len(t, live, 2, "one cookie, two unlocks")
		for _, l := range live {
			if l.Project == cmLegal {
				legalUnlock = l.ID
			}
		}
		require.NotEmpty(t, legalUnlock)
		d.crawl(t, browserB, targets)
	})

	t.Run("the unlock's end: locked again", func(t *testing.T) {
		// An agent's tree may not lock it again.
		status, _, _ := leased.do(t, http.MethodPost, "/console/access/unlocks/"+finUnlock+"/end", url.Values{}, true)
		require.Equal(t, http.StatusForbidden, status)
		// Lock again, from the access page.
		status, body, _ := browserA.do(t, http.MethodPost, "/console/access/unlocks/"+finUnlock+"/end", url.Values{}, false)
		require.Equal(t, http.StatusSeeOther, status, body)
		_, body, _ = browserA.do(t, http.MethodGet, "/console/memories/"+d.fin.memID, nil, false)
		require.NotContains(t, body, d.fin.secret, "example-finance is locked again")
		_, body, _ = browserA.do(t, http.MethodGet, "/console/memories/"+d.legal.memID, nil, false)
		require.Contains(t, body, d.legal.secret, "example-legal's unlock is its own")
		// Expiry.
		_, err := d.db.Exec(`UPDATE console_unlocks SET expires_at = ? WHERE id = ?`, core.FormatTime(time.Now().Add(-time.Second)), legalUnlock)
		require.NoError(t, err)
		d.crawl(t, browserA, targets)

		// An agent tree's lease, revoked from the access page: that tree is
		// locked out of it again; its other lease stands.
		status, body, _ = browserA.do(t, http.MethodPost, "/console/access/leases/"+legalLease.ID+"/revoke", url.Values{}, true)
		require.Equal(t, http.StatusOK, status, body)
		_, body, _ = leased.do(t, http.MethodGet, "/console/memories/"+d.legal.memID+"?format=json", nil, true)
		require.NotContains(t, body, d.legal.secret, "the revoked lease opens nothing")
		_, body, _ = leased.do(t, http.MethodGet, "/console/memories/"+d.fin.memID+"?format=json", nil, true)
		require.Contains(t, body, d.fin.secret, "the other lease still opens example-finance")
		text, isErr := paCall(t, agentMCP, "memory_read", map[string]any{"project": cmLegal, "name": d.legal.memName})
		require.True(t, isErr, text)
		require.Contains(t, text, "locked")
		require.NotContains(t, text, d.legal.secret)
	})

	t.Run("after every route: nothing minted, nothing granted by a route alone", func(t *testing.T) {
		c := d.counts(t)
		require.Equal(t, 1, c.enrollCodes, "the one code seamlessd approvers enroll-code would mint")
		require.Equal(t, 2, c.unlocks, "the passkey unlock and the terminal approval: nothing else was granted")
		require.Zero(t, c.liveUnlocks)
		require.Equal(t, 2, c.leases, "the agent's two leases, one revoked")
		require.Equal(t, 2, c.approved)
		pending, err := store.AccessApprovalByID(ctx, d.db, pendingID)
		require.NoError(t, err)
		require.Equal(t, store.ApprovalPending, pending.Status, "no route approved the unleased tree's request")
		rows, err := store.ActiveAccessLeasesFor(ctx, d.db, []string{stranger.Key()}, time.Now())
		require.NoError(t, err)
		require.Empty(t, rows)
	})
}

// cmUnlockData is the unlock page's JSON as this matrix reads it.
type cmUnlockData struct {
	Project  store.ProjectTally `json:"project"`
	Unlocked bool               `json:"unlocked"`
	Leased   bool               `json:"leased"`
}

// cmUnlockPage reads project's unlock page as c sees it.
func cmUnlockPage(t *testing.T, c cmCaller, project string) cmUnlockData {
	t.Helper()
	status, body, _ := c.do(t, http.MethodGet, "/console/unlock/"+project+"?format=json", nil, true)
	require.Equal(t, http.StatusOK, status, body)
	var d cmUnlockData
	require.NoError(t, json.Unmarshal([]byte(body), &d), body)
	return d
}
