package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/passkey/passkeytest"
	"github.com/arctop/seamless/internal/store"
)

// The door (plan:console-ux-pass UX 40): a locked project's unlock page and
// the lock page are one card each, with the state, the lock's kind and one
// primary action, drawn from the console's locked-project fixture.

// drDoor is the door card of a rendered unlock or lock page.
func drDoor(t *testing.T, page string) string {
	t.Helper()
	start := strings.Index(page, `<section class="door`)
	require.NotEqual(t, -1, start, "the page draws the door")
	end := strings.Index(page[start:], "</section>")
	require.NotEqual(t, -1, end)
	return page[start : start+end]
}

// drBetween is s from the first open to the first closer after it.
func drBetween(t *testing.T, s, open, closer string) string {
	t.Helper()
	start := strings.Index(s, open)
	require.NotEqual(t, -1, start, "no %q", open)
	end := strings.Index(s[start:], closer)
	require.NotEqual(t, -1, end, "no %q after %q", closer, open)
	return s[start : start+end]
}

// drPrimaryRe finds the element a door marks as its one primary action.
var drPrimaryRe = regexp.MustCompile(`<(a|button)[^>]*\bdata-door-primary\b[^>]*>`)

// drPrimaries is every primary action the door draws, each its opening tag.
func drPrimaries(door string) []string {
	return drPrimaryRe.FindAllString(door, -1)
}

// drEnroll enrolls a passkey, so the passkey path is ready.
func drEnroll(t *testing.T, lc *lockedConsole) {
	t.Helper()
	key, err := passkeytest.New(true)
	require.NoError(t, err)
	enrollPasskeyByCode(t, lc.db, lc.svc, key, "key")
}

// drGetAt is lc.get with the request's host set: a browser that reached the
// console on another address than the passkey's origin.
func drGetAt(t *testing.T, lc *lockedConsole, host, target, unlock string) string {
	t.Helper()
	req := lc.browser(http.MethodGet, target, unlock, nil)
	req.Host = host
	rr := lc.do(req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	return rr.Body.String()
}

// drBearer is a page as the TCP key reads it: not a browser.
func drBearer(t *testing.T, lc *lockedConsole, target string) string {
	t.Helper()
	rr := lc.do(authedReq(httptest.NewRequest(http.MethodGet, target, nil)))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	return rr.Body.String()
}

// drLeased is the unlock page as a seam command in an agent tree that holds a
// lease on the locked project reads it over the socket.
func drLeased(t *testing.T, lc *lockedConsole) string {
	t.Helper()
	ctx := context.Background()
	agent := access.ProcessToken{PID: 200, Version: 2000, Boot: "6c2f1a3e-0b7d-4c55-9a51-3d8f00a1b2c3"}
	a, err := store.CreateAccessApproval(ctx, lc.db, store.AccessRequest{Project: lockedSlug, Kind: store.AccessKindRequest,
		Lease: time.Hour, TTL: time.Minute, PeerUID: 501, PeerPID: agent.PID, ProcessKey: agent.Key()}, time.Now())
	require.NoError(t, err)
	_, err = store.ApproveAccessFor(ctx, lc.db, a.ID, a.Challenge, "daemon-account", time.Hour, time.Now())
	require.NoError(t, err)
	v, ok := lc.policy.PeerChainView(501, []access.ProcessToken{agent})
	require.True(t, ok)
	rows, err := store.ActiveAccessLeasesFor(ctx, lc.db, []string{agent.Key()}, time.Now())
	require.NoError(t, err)
	v = v.WithLeases(time.Now(), store.AccessLeases(rows))
	req := httptest.NewRequest(http.MethodGet, "/console/unlock/"+lockedSlug, nil)
	rr := lc.do(req.WithContext(access.WithView(req.Context(), v)))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	return rr.Body.String()
}

// The unlock page in each state: the medallion, the state chip and the lock's
// kind, the four counts as tiles, and exactly one primary action -- none for
// a caller that cannot hold an unlock.
func TestDrUnlockDoor_States(t *testing.T) {
	planNext := "/console/plans/ledger-plan"
	itemURL := "/console/unlock/" + lockedSlug + "?item=plan&next=" + url.QueryEscape(planNext)
	for _, tc := range []struct {
		name    string
		page    func(t *testing.T, lc *lockedConsole) string
		open    bool     // the medallion is open, the chip says Unlocked here
		primary string   // what the one primary action holds ("" for none)
		has     []string // what the door says
		hasNot  []string
	}{
		{name: "passkey ready", page: func(t *testing.T, lc *lockedConsole) string {
			drEnroll(t, lc)
			_, page := lc.get(t, "/console/unlock/"+lockedSlug, "")
			return page
		}, primary: `id="unlock-go"`, has: []string{"Unlock for 3 hours", "In this browser only. Agents still ask you.",
			`<p class="door-divider">no passkey here?</p>`, `class="btn door-way" id="unlock-terminal-go"`},
			hasNot: []string{"door-notice", "No passkey is added yet."}},
		{name: "no passkey", page: func(t *testing.T, lc *lockedConsole) string {
			_, page := lc.get(t, "/console/unlock/"+lockedSlug, "")
			return page
		}, primary: `id="unlock-terminal-go"`, has: []string{"No passkey is added yet.", `<a href="/console/approvers">Add one on Approvers</a>`},
			hasNot: []string{`id="unlock-form"`, "no passkey here?", "door-notice"}},
		{name: "wrong origin", page: func(t *testing.T, lc *lockedConsole) string {
			drEnroll(t, lc)
			return drGetAt(t, lc, "127.0.0.1:8081", "/console/unlock/"+lockedSlug, "")
		}, primary: `id="unlock-terminal-go"`, has: []string{"door-notice", "A passkey works only on",
			`href="` + approverOrigin + `/console/unlock/` + lockedSlug + `"`},
			hasNot: []string{`id="unlock-form"`, "No passkey is added yet."}},
		{name: "passkeys off", page: func(t *testing.T, _ *lockedConsole) string {
			lc := signinNewLockedConsole(t, func(c *Config) {
				c.Approvals = approval.New(approval.Config{DB: c.WriteDB, ForceLocked: []string{lockedSlug}})
			})
			_, page := lc.get(t, "/console/unlock/"+lockedSlug, "")
			return page
		}, primary: `id="unlock-terminal-go"`, has: []string{"Passkeys are not configured on this daemon"},
			hasNot: []string{`id="unlock-form"`, "No passkey is added yet."}},
		{name: "not a browser", page: func(t *testing.T, lc *lockedConsole) string {
			return drBearer(t, lc, "/console/unlock/"+lockedSlug)
		}, has: []string{"An unlock is per browser"}, hasNot: []string{`id="unlock-terminal-form"`, `id="unlock-form"`, `id="unlock-terminal"`}},
		{name: "leased", page: drLeased, has: []string{"This request reads " + lockedSlug + " through its agent's lease."},
			hasNot: []string{`id="unlock-terminal-form"`, `id="unlock-form"`}},
		{name: "unlocked", page: func(t *testing.T, lc *lockedConsole) string {
			_, page := lc.get(t, "/console/unlock/"+lockedSlug, lc.unlockByTerminal(t))
			return page
		}, open: true, primary: `href="/console/projects/` + lockedSlug + `"`, has: []string{"Open " + lockedSlug + "<",
			"Open in this browser", "time-left", "data-left", `<i style="width:100%">`, "Lock it again now",
			`<input type="hidden" name="next" value="/console/unlock/` + lockedSlug + `">`},
			hasNot: []string{`id="unlock-terminal-form"`, `id="unlock-form"`}},
		{name: "from an item link", page: func(t *testing.T, lc *lockedConsole) string {
			_, page := lc.get(t, itemURL, "")
			return page
		}, primary: `id="unlock-terminal-go"`, has: []string{"No passkey is added yet."}},
		{name: "from an item link, unlocked", page: func(t *testing.T, lc *lockedConsole) string {
			_, page := lc.get(t, itemURL, lc.unlockByTerminal(t))
			return page
		}, open: true, primary: `href="` + planNext + `"`, has: []string{"Open the plan<"}, hasNot: []string{"Open " + lockedSlug + "<"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lc := newLockedConsole(t)
			page := tc.page(t, lc)
			door := drDoor(t, page)
			require.NotContains(t, page, lockedSecret)

			// The medallion, the state chip and the lock's kind.
			if tc.open {
				require.Contains(t, door, `<section class="door is-open"`)
				require.Contains(t, door, "ico-lock-open")
				require.Contains(t, door, `<span class="badge ok" data-door-state="unlocked">Unlocked here</span>`)
			} else {
				require.Contains(t, door, `<section class="door"`)
				require.NotContains(t, door, "ico-lock-open")
				require.Contains(t, door, `<span class="badge brand" data-door-state="locked">Locked</span>`)
			}
			require.Contains(t, door, `<div class="door-medallion" aria-hidden="true"><svg class="ico ico-lock`)
			require.Contains(t, door, `<h1 class="door-title" id="door-title">`+lockedSlug+`</h1>`)
			require.Contains(t, door, `data-lock-kind="file"`, "the fixture's lock is the config file's")
			require.Contains(t, door, "peer_auth.force_locked", "named in the kind's hint")

			// The counts as tiles.
			for _, tile := range []string{"memories", "notes", "sessions", "open tasks"} {
				require.Contains(t, door, "<strong>1</strong><span>"+tile+"</span>", tile)
			}

			primaries := drPrimaries(door)
			if tc.primary == "" {
				require.Empty(t, primaries, "a caller that cannot hold an unlock is offered none")
			} else {
				require.Len(t, primaries, 1, "exactly one primary action: %v", primaries)
				require.Contains(t, primaries[0], tc.primary)
				require.Contains(t, primaries[0], `class="btn primary"`)
			}
			for _, s := range tc.has {
				require.Contains(t, door, s)
			}
			for _, s := range tc.hasNot {
				require.NotContains(t, door, s)
			}
		})
	}
}

// Reached from a locked item's page, a strip above the card names the item,
// with the words the item's page promised and, once unlocked, a way back.
func TestDrUnlockDoor_ItemStrip(t *testing.T) {
	lc := newLockedConsole(t)
	target := "/console/unlock/" + lockedSlug + "?item=plan&next=" + url.QueryEscape("/console/plans/"+lc.plan)
	_, page := lc.get(t, target, "")
	strip := drBetween(t, page, `<p class="door-strip"`, "</p>")
	require.Contains(t, strip, `data-locked-item="plan"`)
	require.Contains(t, strip, "ico-map")
	require.Contains(t, strip, "The plan you opened is in this locked project. Unlock the project in this browser to read it.")
	require.Less(t, strings.Index(page, `<p class="door-strip"`), strings.Index(page, `<section class="door`), "the strip is above the card")

	_, page = lc.get(t, target, lc.unlockByTerminal(t))
	strip = drBetween(t, page, `<p class="door-strip"`, "</p>")
	require.Contains(t, strip, `The plan you opened is in this locked project. <a href="/console/plans/`+lc.plan+`">Open it</a>.`)

	_, page = lc.get(t, "/console/unlock/"+lockedSlug, "")
	require.NotContains(t, page, "door-strip", "no item, no strip")
}

// The lock's kind: the config file's, a console lock with a link to remove
// it, or a removed lock the restart has not applied yet.
func TestDrUnlockDoor_LockKind(t *testing.T) {
	ctx := context.Background()
	kind := func(t *testing.T, lc *lockedConsole) (string, unlockData) {
		t.Helper()
		_, page := lc.get(t, "/console/unlock/"+lockedSlug, "")
		_, js := lc.get(t, "/console/unlock/"+lockedSlug+"?format=json", "")
		var d unlockData
		require.NoError(t, json.Unmarshal([]byte(js), &d))
		return drDoor(t, page), d
	}

	door, d := kind(t, newLockedConsole(t))
	require.Equal(t, lockKindFile, d.LockKind)
	require.Contains(t, door, `data-lock-kind="file"`)
	require.Contains(t, door, "config file</span>")
	require.NotContains(t, door, "Remove the lock")

	// The console's own lock: the file does not list the project here.
	lc := signinNewLockedConsole(t, func(c *Config) { c.ForceLocked = nil })
	require.NoError(t, store.AddConsoleLock(ctx, lc.db, lockedSlug, "key", time.Now()))
	door, d = kind(t, lc)
	require.Equal(t, lockKindConsole, d.LockKind)
	require.True(t, d.ConsoleLocked)
	require.Contains(t, door, `data-lock-kind="console"`)
	require.Contains(t, door, `<a href="/console/lock/`+lockedSlug+`">Remove the lock</a>`)

	// Neither: removed, and the running daemon keeps it locked until it
	// restarts.
	lc = signinNewLockedConsole(t, func(c *Config) { c.ForceLocked = nil })
	door, d = kind(t, lc)
	require.Empty(t, d.LockKind)
	require.Contains(t, door, `data-lock-kind="removed"`)
	require.Contains(t, door, "until then the project stays locked")
}

// Once begun, the terminal way shows the steps: the code, its time left and
// the command with Copy, then the wait, with Cancel. The page draws them
// hidden with nothing in them -- no code and no command before this browser
// begins -- and the hint by the command keeps the rule about whose code to run.
func TestDrUnlockDoor_TerminalSteps(t *testing.T) {
	lc := newLockedConsole(t)
	_, page := lc.get(t, "/console/unlock/"+lockedSlug, "")
	box := drBetween(t, page, `<div class="door-code" id="unlock-terminal" hidden>`, "</ol>")
	require.Contains(t, box, `<code id="unlock-terminal-code"></code>`)
	require.Contains(t, box, `<p class="door-code-left" id="unlock-terminal-left"></p>`)
	require.Contains(t, box, `<code id="unlock-terminal-command"></code>`)
	require.Contains(t, box, `id="unlock-terminal-copy"`)
	require.Contains(t, box, "This browser asked for a code.")
	require.Contains(t, box, "Run this in your own terminal on the server.")
	require.Contains(t, box, "Run it with the code this page shows, never with one you were handed; a code works for 10 minutes.")
	require.Contains(t, box, "This page opens the project once the command approves it.")
	require.NotContains(t, box, "console-open", "no command until this browser has its code")
	require.Contains(t, page, `id="unlock-terminal-cancel"`)

	// The script fills them from the begin's answer and waits in place.
	src, err := templateFS.ReadFile("templates/unlock.html")
	require.NoError(t, err)
	js := string(src)
	for _, s := range []string{"begun.code", "begun.command", "begun.pending_until", "'data-copy', begun.command",
		"window.SeamConsole.load(location.href", "seam:content-updated"} {
		require.Contains(t, js, s)
	}
	require.NotContains(t, js, "location.reload(")

	// What the begin answers is what the script reads.
	begun, _ := lc.beginTerminal(t, lockedSlug, "")
	require.NotEmpty(t, begun.Code)
	require.NotEmpty(t, begun.Command)
	require.NotEmpty(t, begun.PendingUntil)
	require.Equal(t, 10, begun.Minutes)
}

// Both pages run their ceremony through static/passkey.js: unlock.html holds
// no copy of its helpers.
func TestDrDoorPages_UseTheSharedPasskeyScript(t *testing.T) {
	for _, name := range []string{"unlock.html", "lock.html"} {
		src, err := templateFS.ReadFile("templates/" + name)
		require.NoError(t, err)
		s := string(src)
		require.Contains(t, s, `<script src="{{static "passkey.js"}}"></script>`, name)
		for _, copied := range []string{"b64urlToBuf", "bufToB64url", "function requestOptions", "function toJSON",
			"function post(", "parseRequestOptionsFromJSON", "navigator.credentials.get", "atob(", "btoa("} {
			require.NotContains(t, s, copied, "%s copies a passkey helper", name)
		}
	}
	lc := newLockedConsole(t)
	_, page := lc.get(t, "/console/unlock/"+lockedSlug, "")
	require.Contains(t, page, `src="`+staticLink(t, "passkey.js")+`"`)
}

// The lock page in each state: the ladder at the right rung, with the change's
// rung marked, and the right action.
func TestDrLockDoor_States(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		setup    func(t *testing.T, lc *lockedConsole) string // the page
		title    string
		here, to string // the ladder's rungs
		primary  string // what the one primary action holds ("" for none)
		has      []string
		hasNot   []string
	}{
		{name: "needs a fence", setup: func(t *testing.T, lc *lockedConsole) string {
			drEnroll(t, lc)
			_, page := lc.get(t, "/console/lock/"+openSlug, "")
			return page
		}, title: "Lock " + openSlug + "?", here: "open", primary: `href="/console/projects/` + openSlug + `?tab=overview#isolation"`,
			has:    []string{"A lock sits on a fence: make it confidential first.", "needs an isolation fence"},
			hasNot: []string{`id="lock-form"`, "door-what"}},
		{name: "no passkey", setup: func(t *testing.T, lc *lockedConsole) string {
			lockable(t, lc)
			_, page := lc.get(t, "/console/lock/"+lockableSlug, "")
			return page
		}, title: "Lock " + lockableSlug + "?", here: "confidential", to: "locked", primary: `href="/console/approvers"`,
			has:    []string{"data-needs-passkey", "A lock change needs your passkey, and none is added yet.", "Then come back here."},
			hasNot: []string{`id="lock-form"`}},
		{name: "ready", setup: func(t *testing.T, lc *lockedConsole) string {
			lockable(t, lc)
			drEnroll(t, lc)
			_, page := lc.get(t, "/console/lock/"+lockableSlug, "")
			return page
		}, title: "Lock " + lockableSlug + "?", here: "confidential", to: "locked", primary: `id="lock-go"`,
			has: []string{`data-action="lock"`, `class="btn primary" id="lock-go"`, "Lock with a passkey",
				"Its own agents ask you before they read or write it.",
				"This console shows only its name and counts until you unlock it in a browser.",
				"Seamless restarts to apply it", `<div class="door-steps-after" id="lock-steps" hidden>`,
				"Approved with your passkey", "Restarting Seamless", "Locked, and this page refreshes"},
			hasNot: []string{"data-lock-pending"}},
		{name: "sealed, ready", setup: func(t *testing.T, lc *lockedConsole) string {
			lockable(t, lc)
			require.NoError(t, store.SetProjectIsolation(ctx, lc.db, lockableSlug, core.IsolationSealed))
			drEnroll(t, lc)
			_, page := lc.get(t, "/console/lock/"+lockableSlug, "")
			return page
		}, title: "Lock " + lockableSlug + "?", here: "sealed", to: "locked", primary: `id="lock-go"`,
			has: []string{"<strong>Sealed</strong>", "nothing in or out"}, hasNot: []string{"<strong>Confidential</strong>"}},
		{name: "console-locked", setup: func(t *testing.T, _ *lockedConsole) string {
			// The running daemon locks it, and the console's lock holds it.
			lc := signinNewLockedConsole(t, func(c *Config) { c.ForceLocked = nil })
			drEnroll(t, lc)
			require.NoError(t, store.AddConsoleLock(ctx, lc.db, lockedSlug, "key", time.Now()))
			_, page := lc.get(t, "/console/lock/"+lockedSlug, "")
			return page
		}, title: "Remove the lock on " + lockedSlug + "?", here: "locked", to: "confidential", primary: `id="lock-go"`,
			has: []string{`data-action="remove"`, `class="btn danger" id="lock-go"`, "Remove the lock with a passkey",
				"Its own agents reach it again, inside its confidential fence.",
				"This console shows its content again without an unlock.", "Lock removed, and this page refreshes"},
			hasNot: []string{"data-lock-pending"}},
		{name: "force-locked", setup: func(t *testing.T, lc *lockedConsole) string {
			drEnroll(t, lc)
			_, page := lc.get(t, "/console/lock/"+lockedSlug, "")
			return page
		}, title: lockedSlug + " is locked", here: "locked",
			has: []string{"Only the config file can remove this lock.", "<code>peer_auth.force_locked</code>",
				`data-copy="peer_auth.force_locked"`, `<a class="door-link" href="/console/unlock/` + lockedSlug + `">Unlock it in this browser instead`},
			hasNot: []string{`id="lock-form"`, "door-what"}},
		{name: "pending restart", setup: func(t *testing.T, lc *lockedConsole) string {
			lockable(t, lc)
			key, err := passkeytest.New(true)
			require.NoError(t, err)
			enrollPasskeyByCode(t, lc.db, lc.svc, key, "key")
			rr := lc.lockChange(t, key, lockableSlug, approval.LockActionAdd)
			require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
			_, page := lc.get(t, "/console/lock/"+lockableSlug, "")
			return page
		}, title: "Remove the lock on " + lockableSlug + "?", here: "locked", to: "confidential", primary: `id="lock-go"`,
			has: []string{"data-lock-pending", "Saved: takes effect when Seamless next restarts", `data-action="remove"`}},
		{name: "wrong origin", setup: func(t *testing.T, lc *lockedConsole) string {
			lockable(t, lc)
			drEnroll(t, lc)
			return drGetAt(t, lc, "127.0.0.1:8081", "/console/lock/"+lockableSlug, "")
		}, title: "Lock " + lockableSlug + "?", here: "confidential", to: "locked",
			has: []string{"A passkey works only on", `href="` + approverOrigin + `/console/lock/` + lockableSlug + `"`}, hasNot: []string{`id="lock-form"`}},
		{name: "not a browser", setup: func(t *testing.T, lc *lockedConsole) string {
			lockable(t, lc)
			drEnroll(t, lc)
			return drBearer(t, lc, "/console/lock/"+lockableSlug)
		}, title: "Lock " + lockableSlug + "?", here: "confidential", to: "locked",
			has: []string{"A lock change needs a passkey: open this page in the browser"}, hasNot: []string{`id="lock-form"`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lc := newLockedConsole(t)
			page := tc.setup(t, lc)
			door := drDoor(t, page)
			require.NotContains(t, page, lockedSecret)
			require.Contains(t, door, `<h1 class="door-title" id="lock-title">`+tc.title+`</h1>`)

			// The ladder: Open, the fence, Locked, one rung here and at most
			// one where the change goes.
			ladder := drBetween(t, door, `<ol class="door-ladder"`, "</ol>")
			require.Equal(t, 3, strings.Count(ladder, `<li class="door-rung`))
			require.Equal(t, 1, strings.Count(ladder, "is-here"))
			require.Contains(t, ladder, `class="door-rung is-here" data-rung="`+tc.here+`" aria-current="step"`)
			if tc.to == "" {
				require.NotContains(t, ladder, "is-to")
				require.NotContains(t, door, "data-rung-to")
			} else {
				require.Equal(t, 1, strings.Count(ladder, "is-to"))
				require.Contains(t, ladder, `class="door-rung is-to" data-rung="`+tc.to+`"`)
			}

			primaries := drPrimaries(door)
			if tc.primary == "" {
				require.Empty(t, primaries)
			} else {
				require.Len(t, primaries, 1, "exactly one primary action: %v", primaries)
				require.Contains(t, primaries[0], tc.primary)
			}
			for _, s := range tc.has {
				require.Contains(t, door, s)
			}
			for _, s := range tc.hasNot {
				require.NotContains(t, door, s)
			}
		})
	}
}

// The lock page's JSON adds the ladder's rungs and the fence; the unlock
// page's adds the lock's kind. Every earlier field stays.
func TestDrDoorPages_JSON(t *testing.T) {
	lc := newLockedConsole(t)
	lockable(t, lc)
	_, js := lc.get(t, "/console/lock/"+lockableSlug+"?format=json", "")
	var lock map[string]any
	require.NoError(t, json.Unmarshal([]byte(js), &lock))
	for _, k := range []string{"project", "name", "force_locked", "console_locked", "active", "fenced", "passkeys_on", "has_passkey"} {
		require.Contains(t, lock, k, "a field the page had before")
	}
	require.Equal(t, "confidential", lock["isolation"])
	require.Equal(t, "confidential", lock["rung"])
	require.Equal(t, "locked", lock["rung_to"])

	_, js = lc.get(t, "/console/lock/"+lockedSlug+"?format=json", "")
	lock = map[string]any{}
	require.NoError(t, json.Unmarshal([]byte(js), &lock))
	require.Equal(t, "locked", lock["rung"])
	require.NotContains(t, lock, "rung_to", "a force lock offers no change")

	_, js = lc.get(t, "/console/unlock/"+lockedSlug+"?format=json", "")
	var unlock map[string]any
	require.NoError(t, json.Unmarshal([]byte(js), &unlock))
	for _, k := range []string{"project", "unlocked", "leased", "lease", "passkeys_on", "has_passkey", "force_locked", "console_locked"} {
		require.Contains(t, unlock, k, "a field the page had before")
	}
	require.Equal(t, "file", unlock["lock_kind"])
	require.NotContains(t, js, lockedSecret)
}

func TestDrRungs(t *testing.T) {
	for _, tc := range []struct {
		force, console bool
		iso            core.Isolation
		here, to       string
	}{
		{false, false, core.IsolationOpen, "open", ""},
		{false, false, core.IsolationConfidential, "confidential", "locked"},
		{false, false, core.IsolationSealed, "sealed", "locked"},
		{false, true, core.IsolationConfidential, "locked", "confidential"},
		{false, true, core.IsolationSealed, "locked", "sealed"},
		{true, false, core.IsolationConfidential, "locked", ""},
		{true, true, core.IsolationConfidential, "locked", ""},
	} {
		here, to := rungs(tc.force, tc.console, tc.iso)
		require.Equal(t, tc.here, here, "%+v", tc)
		require.Equal(t, tc.to, to, "%+v", tc)
	}
	v := &lockView{Name: "example-conf", Console: true, Isolation: core.IsolationSealed, Fenced: true}
	v.Rung, v.RungTo = rungs(v.Force, v.Console, v.Isolation)
	require.Equal(t, "Remove the lock on example-conf?", v.Question())
	require.Equal(t, "Its own agents reach it again, inside its sealed fence.", v.WhatAgents())
	ladder := v.Ladder()
	require.Len(t, ladder, 3)
	require.Equal(t, "Sealed", ladder[1].Label)
	require.True(t, ladder[1].To)
	require.True(t, ladder[2].Here)
}

func TestDrLeftPct(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		left time.Duration
		want int
	}{
		{3 * time.Hour, 100}, {90 * time.Minute, 50}, {time.Second, 1}, {0, 0}, {-time.Minute, 0}, {4 * time.Hour, 100},
	} {
		require.Equal(t, tc.want, leftPct(now.Add(tc.left), 3*time.Hour, now), tc.left)
	}
	require.Zero(t, leftPct(now.Add(time.Hour), 0, now), "no lease, no bar")
}
