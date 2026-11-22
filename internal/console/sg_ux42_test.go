package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/passkey"
	"github.com/arctop/seamless/internal/passkey/passkeytest"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// UX 42 (plan:console-ux-pass): Approvers as key cards with adding a passkey
// in three steps, and the sign-in as one card.

// sgPage is GET target from h with the bearer key, on host when one is
// given, and its body; the answer must be a 200.
func sgPage(t *testing.T, h http.Handler, host, target string) string {
	t.Helper()
	req := authedReq(httptest.NewRequest(http.MethodGet, target, nil))
	if host != "" {
		req.Host = host
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	return rr.Body.String()
}

// sgHintPop is a key card's hint: its tooltip's id names the key, and its
// content is the words in it.
var sgHintPop = regexp.MustCompile(`(?s)<span class="hint-pop" id="approver-more-([^"]+)" role="tooltip">(.*?)</span>`)

// sgHintIDs is every hint id a page draws, button and tooltip alike.
var sgHintIDs = regexp.MustCompile(`id="(hint-[0-9a-f]{8}(?:-\d+)?)"`)

// sgRequireUniqueHints holds a page to one tooltip per hint id, so a pinned
// hint and a screen reader find the right words.
func sgRequireUniqueHints(t *testing.T, page, what string) {
	t.Helper()
	seen := map[string]bool{}
	for _, m := range sgHintIDs.FindAllStringSubmatch(page, -1) {
		require.False(t, seen[m[1]], "%s: hint id %s is drawn twice", what, m[1])
		seen[m[1]] = true
		require.Contains(t, page, `aria-describedby="`+m[1]+`"`, "%s: the button names its tooltip", what)
	}
}

// sgStepTitles is the titles of a steps list's steps, in order.
func sgStepTitles(t *testing.T, page, listID string) []string {
	t.Helper()
	start := strings.Index(page, `id="`+listID+`"`)
	require.Positive(t, start, "the %s steps", listID)
	end := strings.Index(page[start:], "</ol>")
	require.Positive(t, end)
	var titles []string
	for _, m := range regexp.MustCompile(`<li class="steps-item[^"]*"[^>]*>\s*<span class="steps-mark"[^>]*>.*?</span>\s*<div class="steps-label">\s*<strong>([^<]+)</strong>`).
		FindAllStringSubmatch(page[start:start+end], -1) {
		titles = append(titles, m[1])
	}
	return titles
}

// Each key is one card, and the command that removes it is in that card's
// hint and nowhere else on the page: removal stays in the terminal (Addendum
// 11 of locked-projects-approval-design), as the daemon's account.
func TestSgApprovers_OneCardPerKeyAndItsRemovalOnlyInItsHint(t *testing.T) {
	f := newAccessFixture(t)
	hardware, err := passkeytest.New(true)
	require.NoError(t, err)
	synced, err := passkeytest.New(false)
	require.NoError(t, err)
	security := enrollPasskeyByCode(t, f.db, f.svc, hardware, "security key")
	phone := enrollPasskeyByCode(t, f.db, f.svc, synced, "phone")
	touch := f.enrollTouch(t, newTouchKey(t), "Touch ID on example-mac")

	page := sgPage(t, f.mux, "localhost:8081", "/console/approvers")
	cards := regexp.MustCompile(`<li class="ap-key" data-approver="([^"]+)" data-kind="([^"]+)">`).FindAllStringSubmatch(page, -1)
	require.Len(t, cards, 3, "one card per key")
	kinds := map[string]string{}
	for _, c := range cards {
		kinds[c[1]] = c[2]
	}
	require.Equal(t, map[string]string{security.ID: "passkey", phone.ID: "passkey", touch: "touchid"}, kinds)
	require.Contains(t, page, `aria-label="3 added"><strong>3</strong>added</span>`)

	card := func(id string) string {
		t.Helper()
		at := strings.Index(page, `data-approver="`+id+`"`)
		require.Positive(t, at)
		end := strings.Index(page[at:], "</li>")
		require.Positive(t, end)
		return page[at : at+end]
	}
	require.Contains(t, card(security.ID), "ico-key-round")
	require.Contains(t, card(security.ID), "<span>passkey</span>")
	require.Contains(t, card(phone.ID), "ico-cloud", "a synced passkey wears the cloud")
	require.Contains(t, card(phone.ID), "passkey &middot; synced to your devices")
	require.Contains(t, card(touch), "ico-fingerprint")
	require.Contains(t, card(touch), "Touch ID &middot; in the agent's dialog")
	for _, id := range []string{security.ID, phone.ID, touch} {
		c := card(id)
		require.Contains(t, c, "never used", id)
		require.Contains(t, c, "added "+approverDay(time.Now(), time.Now()), id)
		require.Contains(t, c, `aria-describedby="approver-more-`+id+`"`, "the hint button names its own tooltip")
	}

	pops := sgHintPop.FindAllStringSubmatch(page, -1)
	require.Len(t, pops, 3)
	for _, p := range pops {
		require.Contains(t, p[2], "Its id is <code>"+p[1]+"</code>.")
		require.Contains(t, p[2], "How it was added: an enrollment code.")
		require.Contains(t, p[2], `<code class="ap-remove">/usr/bin/sudo -k -u _seamless /usr/bin/env SEAMLESS_CONFIG=/opt/seamless/etc/seamless.yaml `+
			`/opt/seamless/bin/seamlessd approvers remove `+p[1]+`</code>`)
		require.Contains(t, p[2], "the console's key is no proof of you")
	}
	rest := sgHintPop.ReplaceAllString(page, "")
	require.NotContains(t, rest, "approvers remove", "removal is said only in a key's hint")
	require.NotContains(t, rest, "approvers reset")

	// A key used once says when, in the ok tone.
	require.NoError(t, store.RecordApproverUse(context.Background(), f.db, security.ID, security.SignCount, security.SignCount+1,
		security.BackupState, time.Now().Add(-3*24*time.Hour)))
	used := sgPage(t, f.mux, "localhost:8081", "/console/approvers")
	require.Regexp(t, `<span class="badge ok compact"><time [^>]*>used 3d ago</time></span>`, used)
}

// Adding a passkey is three steps, in order: name and create it, make its
// one-time code in the owner's own terminal (a code that enrolls this passkey
// only: Addendum 10), and enter the code. The code's form stays hidden until
// a passkey exists, and the status line keeps data-live-skip.
func TestSgApprovers_AddingAPasskeyIsThreeStepsInOrder(t *testing.T) {
	_, mux := newApproversConsole(t)
	page := sgPage(t, mux, "localhost:8081", "/console/approvers")
	require.Contains(t, page, "Nothing is enrolled yet")
	require.Equal(t, []string{"Name it, and create it", "Make its one-time code in your own terminal", "Enter the code"},
		sgStepTitles(t, page, "approver-steps"))

	steps := page[strings.Index(page, `id="approver-steps"`):]
	steps = steps[:strings.Index(steps, "</ol>")]
	items := strings.Split(steps, `<li class="steps-item`)[1:]
	require.Len(t, items, 3)
	require.True(t, strings.HasPrefix(items[0], ` is-now" aria-current="step">`), "step 1 is under way")
	require.Contains(t, items[0], `<form id="approver-form" class="approver-form" autocomplete="off">`)
	require.True(t, strings.HasPrefix(items[1], ` is-todo">`))
	require.Contains(t, items[1], `<div class="steps-body" id="approver-mint" hidden>`, "the command waits for the passkey")
	require.Contains(t, items[1], "the code it prints enrolls only the passkey you just created")
	require.Contains(t, items[1], "Every passkey needs a one-time code, even when one is already enrolled")
	require.True(t, strings.HasPrefix(items[2], ` is-todo">`))
	require.Contains(t, items[2], `<form id="approver-confirm" class="approver-form" autocomplete="off" hidden>`)
	require.Contains(t, page, `<p class="ap-status" id="approver-status" data-live-skip role="status" aria-live="polite"></p>`)

	// The two tiles: real radios, the passkey chosen first, and Touch ID's
	// command with its Copy.
	require.Contains(t, page, `<input type="radio" name="ap-kind" value="passkey" checked>`)
	require.Contains(t, page, `<input type="radio" name="ap-kind" value="touchid">`)
	require.Less(t, strings.Index(page, `value="passkey"`), strings.Index(page, `value="touchid"`))
	require.Contains(t, page, `<code>seam touchid enroll</code><button type="button" class="copy-btn" data-copy="seam touchid enroll"`)
	require.NotContains(t, page, "It also signs you in", "an approver passkey signs in only under passkey sign-in")
	sgRequireUniqueHints(t, page, "approvers")

	// The CSS shows the Touch ID panel only while its tile is chosen.
	css := string(consoleCSS)
	require.Contains(t, css, `.ap-panel-touchid { display: none; }`)
	require.Contains(t, css, `.ap-add:has(input[value="touchid"]:checked) .ap-panel-touchid { display: block; }`)
	require.Contains(t, css, `.steps-body[hidden] { display: none; }`, "display beats the hidden attribute otherwise")
}

// A passkey made elsewhere is one notice each: passkeys off, the wrong origin,
// and under passkey sign-in the Add a passkey page on the device that will
// hold it. None of them offers this page's own steps.
func TestSgApprovers_ANoticeEachForTheOtherWays(t *testing.T) {
	notice := func(t *testing.T, page string) string {
		t.Helper()
		require.NotContains(t, page, `id="approver-form"`, "no steps of its own")
		require.Equal(t, 1, strings.Count(page, `class="ap-notice"`), "one notice")
		at := strings.Index(page, `class="ap-notice"`)
		return page[at : at+strings.Index(page[at:], "</div>\n  </div>")+1]
	}

	t.Run("passkeys off", func(t *testing.T) {
		db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = db.Close() })
		svc, err := New(Config{DB: db, Events: events.NewRecorder(db), APIKey: testKey, Level: "advanced",
			Approvals: approval.New(approval.Config{DB: db})})
		require.NoError(t, err)
		mux := http.NewServeMux()
		svc.Register(mux)
		page := sgPage(t, mux, "", "/console/approvers")
		require.Contains(t, notice(t, page), "Passkeys are not configured on this daemon.")
		require.Contains(t, page, `data-copy="seam touchid enroll"`, "Touch ID enrolls without passkeys")
	})
	t.Run("the wrong origin", func(t *testing.T) {
		_, mux := newApproversConsole(t)
		got := notice(t, sgPage(t, mux, "127.0.0.1:8081", "/console/approvers"))
		require.Contains(t, got, "can only be created on")
		require.Contains(t, got, `href="http://localhost:8081/console/approvers"`)
	})
	t.Run("passkey sign-in", func(t *testing.T) {
		db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = db.Close() })
		v, err := passkey.New(passkey.Config{RPID: "localhost", Origins: []string{approverOrigin}})
		require.NoError(t, err)
		in, err := approval.NewSignIn(approval.SignInConfig{DB: db, Passkeys: v, Origins: []string{approverOrigin}})
		require.NoError(t, err)
		svc, err := New(Config{DB: db, Events: events.NewRecorder(db), APIKey: testKey, Level: "advanced", SignIn: in,
			Approvals: approval.New(approval.Config{DB: db, Passkeys: v, Origins: []string{approverOrigin}})})
		require.NoError(t, err)
		mux := http.NewServeMux()
		svc.Register(mux)
		page := sgPage(t, mux, "localhost:8081", "/console/approvers")
		got := notice(t, page)
		require.Contains(t, got, `<a href="/console/passkeys/add">http://localhost:8081/console/passkeys/add</a>`)
		require.Contains(t, got, `<code>seamlessd approvers approve --code XXXX-XXXX</code>`)
		require.Contains(t, page, "It also signs you in.")
		sgRequireUniqueHints(t, page, "approvers under passkey sign-in")
	})
}

// approvers.html runs its ceremony on static/passkey.js (window.SeamPasskey)
// and keeps no copy of its helpers.
func TestSgApprovers_UsesTheSharedPasskeyScript(t *testing.T) {
	raw, err := templateFS.ReadFile("templates/approvers.html")
	require.NoError(t, err)
	src := string(raw)
	for _, copied := range []string{"function b64urlToBuf", "function bufToB64url", "function creationOptions", "function toJSON",
		"function post(", "parseCreationOptionsFromJSON", "navigator.credentials", "atob(", "btoa("} {
		require.NotContains(t, src, copied, "approvers.html copies a passkey helper")
	}
	require.Contains(t, src, `<script src="{{static "passkey.js"}}"></script>`)
	for _, use := range []string{"pk.post(", "pk.create(", "pk.toJSON(", "pk.supported()", "pk.refusal("} {
		require.Contains(t, src, use)
	}
	_, mux := newApproversConsole(t)
	require.Contains(t, sgPage(t, mux, "localhost:8081", "/console/approvers"), `<script src="`+staticLink(t, "passkey.js")+`"></script>`)
}

// Passkey sign-in (D16) is one card: the passkey first, then under "no
// passkey here?" the terminal's code and the Add a passkey page, each once.
// The left "New device" card and the cards of paragraphs are gone.
func TestSgSignIn_PasskeyModeIsOneCard(t *testing.T) {
	sc := signinNewConsole(t, nil)
	login := func() string {
		t.Helper()
		b := signinNewBrowser(t)
		rr := sc.do(nil, b.request(t, http.MethodGet, "/console/login", nil))
		require.Equal(t, http.StatusOK, rr.Code)
		return rr.Body.String()
	}
	for _, enrolled := range []bool{false, true} {
		if enrolled {
			sc.signinEnroll(t, signinAuthenticator(t), "laptop")
		}
		page := login()
		require.Equal(t, 1, strings.Count(page, `class="login-card`), "one card (enrolled %v)", enrolled)
		require.Equal(t, 1, strings.Count(page, `href="/console/passkeys/add"`), "one way to add a passkey (enrolled %v)", enrolled)
		require.Equal(t, 1, strings.Count(page, "Add a passkey"), "no duplicate Add a passkey (enrolled %v)", enrolled)
		require.Equal(t, !enrolled, strings.Contains(page, "No passkey is added yet"), "enrolled %v", enrolled)
		for _, gone := range []string{"login-guide", "login-config", "New device", "One passkey, every device", "Owner access",
			"Sign in from a terminal on the server"} {
			require.NotContains(t, page, gone)
		}
		require.Contains(t, page, `<h2 id="signin-title">Sign in</h2>`)
		require.Contains(t, page, "Lasts 7 days in this browser.")
		require.Contains(t, page, "A passkey belongs to this server&#39;s name, http://localhost:8081: one made for another site")
		require.Contains(t, page, "the seam CLI still uses it")
		sgRequireUniqueHints(t, page, "passkey sign-in")

		// In order: the passkey, the divider, the terminal's code, adding one.
		order := []string{`id="signin-go"`, `<div class="signin-or">no passkey here?</div>`, `id="terminal-go"`,
			"Show a code for the server's terminal", `<div class="signin-code" id="terminal-command" hidden>`, `href="/console/passkeys/add"`}
		last := -1
		for _, s := range order {
			at := strings.Index(page, s)
			require.Greater(t, at, last, "%q comes after what is above it", s)
			last = at
		}
		require.Equal(t, []string{"This browser's code", "Run this in your own terminal on the server", "Say yes if it shows this browser"},
			sgStepTitles(t, page, "terminal-steps"))
		require.Contains(t, page, "It shows this browser as ", "the browser's facts beside the code")
	}
}

// The key's sign-in is one card too: the key field and Sign in, and "Where is
// my key?" folded under it with what the Credential source card said.
func TestSgSignIn_KeyModeFoldsWhereTheKeyIs(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		want       []string
	}{
		{"a config file", "/home/owner/.config/seamless/seamless.yaml",
			[]string{"/home/owner/.config/seamless/seamless.yaml", "vscode://file/home/owner/.config/seamless/seamless.yaml",
				"in the config file this daemon loaded"}},
		{"no config file", "", []string{"No config file was loaded", "<code>~/.config/seamless/seamless.yaml</code>"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, err := New(Config{APIKey: testKey, ConfigPath: tc.path})
			require.NoError(t, err)
			mux := http.NewServeMux()
			svc.Register(mux)
			rr := do(mux, httptest.NewRequest(http.MethodGet, "/console/login", nil))
			require.Equal(t, http.StatusOK, rr.Code)
			page := rr.Body.String()
			require.Equal(t, 1, strings.Count(page, `class="login-card`), "one card")
			for _, gone := range []string{"Credential source", "login-config", "login-guide", "Fastest way in", "passkey"} {
				require.NotContains(t, page, gone)
			}
			field, fold := strings.Index(page, `<input id="key" name="key" type="password"`), strings.Index(page, `<details class="signin-where">`)
			require.Positive(t, field)
			require.Greater(t, fold, field, "the fold sits under the field")
			end := strings.Index(page[fold:], "</details>")
			require.Positive(t, end)
			where := page[fold : fold+end]
			require.Contains(t, where, "<summary>Where is my key?</summary>")
			for _, s := range append(tc.want, "<code>mcp.api_key</code>", "<code>SEAMLESS_MCP_API_KEY</code>",
				`data-copy="seamlessd console-open"`, "true first run", "never edits an existing config automatically") {
				require.Contains(t, where, s)
			}
			sgRequireUniqueHints(t, page, "key sign-in")
		})
	}
}

// The Add a passkey page draws the sign-in's sky and its steps: create it
// here, approve its code on the server, sign in with it.
func TestSgAddPasskey_TheSameSteps(t *testing.T) {
	sc := signinNewConsole(t, nil)
	rr := sc.do(nil, httptest.NewRequest(http.MethodGet, approverOrigin+passkeyAddPath, nil))
	require.Equal(t, http.StatusOK, rr.Code)
	page := rr.Body.String()
	require.Equal(t, []string{"Name it, and create it", "Approve its code on the server", "Sign in with it"},
		sgStepTitles(t, page, "passkey-add-steps"))
	require.Contains(t, page, `<div class="steps-body" id="passkey-add-wait" hidden>`)
	require.Contains(t, page, "It works for 10 minutes.")
	require.Contains(t, page, `<div class="login-sky" aria-hidden="true">`, "the sign-in's sky")
	require.Contains(t, page, "document.querySelectorAll('.hint.is-pinned')", "the standalone hints' script")
	require.Contains(t, page, "every device it syncs to signs in")
	require.Contains(t, page, "This console&#39;s passkeys belong to localhost", "a localhost relying party says so")
	require.Equal(t, 1, strings.Count(page, `class="login-card`))
	for _, gone := range []string{"login-guide", "login-config", "login-kicker", "login-section-label"} {
		require.NotContains(t, page, gone)
	}
	sgRequireUniqueHints(t, page, "Add a passkey")
}

// The chip's day: this year's without the year, another year's with it.
func TestSgApproverDay(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.Local)
	require.Equal(t, "Nov 9", approverDay(time.Date(2026, 11, 9, 8, 0, 0, 0, time.Local), now))
	require.Equal(t, "Dec 30, 2025", approverDay(time.Date(2025, 12, 30, 8, 0, 0, 0, time.Local), now))
	require.Empty(t, approverDay(time.Time{}, now))
}

// The cards' new fields are the page's alone: ?format=json keeps the shape
// `seam touchid enroll` reads, field for field.
func TestSgApprovers_JSONKeepsItsShape(t *testing.T) {
	f := newAccessFixture(t)
	a, err := passkeytest.New(true)
	require.NoError(t, err)
	enrollPasskeyByCode(t, f.db, f.svc, a, "security key")
	status, out := f.jsonDo(t, authedReq(httptest.NewRequest(http.MethodGet, "/console/approvers?format=json", nil)))
	require.Equal(t, http.StatusOK, status)
	var keys []string
	for k := range out {
		keys = append(keys, k)
	}
	require.ElementsMatch(t, []string{"credentials", "passkeys_on", "origin", "on_origin"}, keys)
	var creds []map[string]any
	require.NoError(t, json.Unmarshal(out["credentials"], &creds))
	require.Len(t, creds, 1)
	var fields []string
	for k := range creds[0] {
		fields = append(fields, k)
	}
	require.ElementsMatch(t, []string{"id", "kind", "name", "synced", "added_at", "added_by"}, fields)
}
