package console

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/store"
)

// UX 41: Access requests as what waits for the owner and what is open now,
// and a request decided in one look (plan:console-ux-pass, D14 and D15).

// axHTML reads path as the owner's browser on the passkey origin.
func axHTML(t *testing.T, mux http.Handler, path string) string {
	t.Helper()
	req := authedReq(httptest.NewRequest(http.MethodGet, path, nil))
	req.Host = "localhost:8081"
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	return rr.Body.String()
}

// axSection is the markup of the element carrying attr, up to its close.
func axSection(t *testing.T, page, attr, closer string) string {
	t.Helper()
	at := strings.Index(page, attr)
	require.GreaterOrEqual(t, at, 0, "no %s", attr)
	end := strings.Index(page[at:], closer)
	require.GreaterOrEqual(t, end, 0, "no %s after %s", closer, attr)
	return page[at : at+end]
}

// axRequest files a lock request with every fact the page words: the program
// the kernel names, a session, and what the agent reports of itself.
func axRequest(t *testing.T, f *accessFixture) store.AccessApproval {
	t.Helper()
	a, err := store.CreateAccessApproval(context.Background(), f.db, store.AccessRequest{
		Project: "example-finance", Kind: store.AccessKindRequest, Lease: time.Hour, TTL: 15 * time.Minute,
		PeerUID: 501, PeerPID: accAgent.PID, ProcessKey: accAgent.Key(),
		Program: "/Users/owner/.local/share/claude/versions/2.1.319", SessionName: "cc/ledger-close",
		CWD: "/work/elsewhere-checkout", Repo: "elsewhere-repo", Model: "example-model-7", Reason: "reconcile\n  the ledger   notes",
	}, time.Now())
	require.NoError(t, err)
	return a
}

// The request's card splits what Seamless checked -- the program, pid and
// account the kernel names, the kind -- from what the agent reported: its
// session, working directory, repository and model, which never appear among
// the checked facts.
func TestAxRequestSplitsCheckedFromReported(t *testing.T) {
	f := newAccessFixture(t)
	a := axRequest(t, f)
	page := axHTML(t, f.mux, "/console/access/"+a.ID)

	checked := axSection(t, page, `data-facts="checked"`, "</section>")
	for _, want := range []string{"Seamless checked", "<dt>Program</dt>", "Claude Code 2.1.319", "pid 200",
		"/Users/owner/.local/share/claude/versions/2.1.319", "<dt>Account</dt><dd>uid 501</dd>",
		"<dt>Kind</dt><dd>a first request</dd>", "<dt>Asks for</dt><dd>read and write</dd>", "<dt>Lock</dt>"} {
		require.Contains(t, checked, want)
	}
	for _, never := range []string{"/work/elsewhere-checkout", "elsewhere-repo", "example-model-7", "cc/ledger-close",
		"Working in", "Repository", "Model"} {
		require.NotContains(t, checked, never, "the agent's own report is not among what Seamless checked")
	}

	reported := axSection(t, page, `data-facts="reported"`, "</section>")
	for _, want := range []string{"The agent reported", approval.SelfReportedNote,
		`<dt>Session</dt><dd><span class="mono">cc/ledger-close</span></dd>`,
		`<dt>Working in</dt><dd><code class="ax-path">/work/elsewhere-checkout</code></dd>`,
		"<dt>Repository</dt><dd>elsewhere-repo</dd>", "<dt>Model</dt><dd>example-model-7</dd>"} {
		require.Contains(t, reported, want)
	}
	require.NotContains(t, reported, "pid 200")
	require.NotContains(t, reported, "uid 501")

	// The head: the project, locked, its isolation, the time left and the
	// short id to copy; then one sentence; the reason quoted as its words.
	head := axSection(t, page, `<header class="ax-request-head">`, "</header>")
	require.Contains(t, head, `<h1 id="ax-request-title" class="ax-request-title">example-finance</h1>`)
	require.Contains(t, head, `<span class="badge brand">locked</span><span class="badge quiet">confidential</span>`)
	require.Contains(t, head, `data-left`)
	require.Contains(t, head, `data-copy="`+approval.ShortID(a.ID)+`"`)
	require.Contains(t, head, `<span class="ax-prog">Claude Code 2.1.319</span>, pid 200, asks to <strong>read and write</strong> it.`)
	require.Contains(t, page, `The agent says <span class="ax-kicker-note">&middot; its own words, not checked</span>`)
	require.Contains(t, page, "<q>reconcile\n  the ledger   notes</q>", "the page quotes the reason whole")

	// Missing reports read as unknown, a missing reason as none given.
	g := newAccessFixture(t)
	id, _ := g.file(t)
	bare := axSection(t, axHTML(t, g.mux, "/console/access/"+id), `data-facts="reported"`, "</section>")
	require.Contains(t, bare, `<dt>Working in</dt><dd class="is-none">unknown</dd>`)
	require.Contains(t, bare, `<dt>Model</dt><dd class="is-none">unknown</dd>`)
}

// A request page that names no duration opens with the first, until the
// session ends, selected, and its passkey options sign that one: an assertion
// over them approves that duration, and the same assertion sent as another
// approves nothing (Addendum 6). A choice the cap leaves out reads as the
// cap's choice, which grants the same.
func TestAxNoDurationSelectsTheFirst(t *testing.T) {
	f := newAccessFixture(t)
	ctx := context.Background()
	key := f.enrollPasskey(t)
	id, _ := f.file(t)

	page := axHTML(t, f.mux, "/console/access/"+id)
	require.Contains(t, page, `<input type="radio" name="for" value="session" checked><span>Until its session ends</span>`)
	require.Contains(t, page, `<input type="radio" name="for" value="1h"><span>1 hour</span>`)
	require.Contains(t, page, `data-duration="session"`)
	require.Contains(t, page, "Approve until its session ends</button>")
	require.Equal(t, 1, strings.Count(page, ` checked>`), "one duration selected, the one the options sign")

	one, facts := f.page(t, "/console/access/"+id)
	require.JSONEq(t, `"session"`, string(one["for"]))
	chosen, _ := f.page(t, "/console/access/"+id+"?for=session")
	require.JSONEq(t, string(chosen["passkey_options"]), string(one["passkey_options"]), "no ?for= signs what ?for=session signs")
	assertion, err := key.Get(one["passkey_options"], approverOrigin)
	require.NoError(t, err)
	status, out := f.post(t, "/console/access/"+id+"/approve", url.Values{"duration": {approval.For1h},
		"digest": {facts.Digest}, "response": {string(assertion)}}, false)
	require.Equal(t, http.StatusConflict, status, "signed for the session, sent as 1h")
	require.Contains(t, string(out["error"]), "the passkey approved this request for another duration")
	status, out = f.post(t, "/console/access/"+id+"/approve", url.Values{"duration": {approval.ForSession},
		"digest": {facts.Digest}, "response": {string(assertion)}}, false)
	require.Equal(t, http.StatusOK, status, string(out["error"]))
	leases, err := store.ActiveAccessLeasesFor(ctx, f.db, []string{accAgent.Key()}, time.Now())
	require.NoError(t, err)
	require.Len(t, leases, 1)
	require.WithinDuration(t, time.Now().Add(8*time.Hour), leases[0].ExpiresAt, time.Minute)

	// The duration shown selected is always one the segments offer.
	capped := durationChoices("1h0m0s")
	for _, tc := range []struct{ asked, want string }{
		{"", approval.ForSession}, {approval.ForSession, approval.ForSession},
		{approval.For1h, approval.For1h}, {approval.For4h, approval.For1h},
	} {
		require.Equal(t, tc.want, axOfferedDuration(tc.asked, capped).Value, "asked %q", tc.asked)
	}
	require.Equal(t, approval.For4h, axOfferedDuration(approval.For4h, durationChoices("8h0m0s")).Value)
	require.Equal(t, "for 20m", axApproveWords(durationChoices("20m0s")[1]))
	require.Equal(t, "20m", durationChoices("20m0s")[1].Label())
}

// Facts that change after the page loaded are never approved: the answer
// asks the owner to look again, the page then shows the new facts with
// options over them, and a page whose options could not be issued for the
// facts it shows says the same instead of offering Approve.
func TestAxFactsChangedAsksToLookAgain(t *testing.T) {
	f := newAccessFixture(t)
	ctx := context.Background()
	key := f.enrollPasskey(t)
	id, _ := f.file(t)
	shown, facts := f.page(t, "/console/access/"+id)
	assertion, err := key.Get(shown["passkey_options"], approverOrigin)
	require.NoError(t, err)
	_, err = store.EnsureProject(ctx, f.db, "example-legal", "example-legal")
	require.NoError(t, err)
	_, err = store.CreateAccessApproval(ctx, f.db, store.AccessRequest{Project: "example-legal", Kind: store.AccessKindRequest,
		Lease: time.Hour, TTL: 15 * time.Minute, PeerUID: 501, PeerPID: accAgent.PID, ProcessKey: accAgent.Key()}, time.Now())
	require.NoError(t, err)

	status, out := f.post(t, "/console/access/"+id+"/approve", url.Values{"duration": {approval.ForSession},
		"digest": {facts.Digest}, "response": {string(assertion)}}, false)
	require.Equal(t, http.StatusConflict, status)
	var msg string
	require.NoError(t, json.Unmarshal(out["error"], &msg))
	require.Equal(t, "the request's facts changed since this page loaded: reload and look at them again", msg)

	fresh, now := f.page(t, "/console/access/"+id)
	require.NotEqual(t, facts.Digest, now.Digest)
	require.NotEqual(t, string(shown["passkey_options"]), string(fresh["passkey_options"]), "new options, over the new facts")
	page := axHTML(t, f.mux, "/console/access/"+id)
	require.Contains(t, page, `data-digest="`+now.Digest+`"`)
	require.Contains(t, page, "<li>The same agent is also asking for example-legal.</li>")

	// The page's own answer when its options are missing for the facts it
	// shows (issued and refused within one load): look again, no Approve.
	cs, err := New(Config{DB: f.db, Events: events.NewRecorder(f.db), APIKey: testKey, Level: "advanced", Approvals: f.svc})
	require.NoError(t, err)
	d := accessOneData{Request: now, Status: store.ApprovalPending, PasskeyReady: true, OnOrigin: true, For: approval.ForSession,
		Selected: approval.ForSession, ApproveWords: "until its session ends", Choices: durationChoices("8h0m0s")}
	var buf bytes.Buffer
	require.NoError(t, cs.pages["access_one"].ExecuteTemplate(&buf, "content", pageData{Data: d}))
	require.Contains(t, buf.String(), `The request changed while this page loaded. <a href="/console/access/`+id+`?for=session" data-seam-query>Look at it again</a>.`)
	require.NotContains(t, buf.String(), `id="access-approve"`)
	require.Contains(t, buf.String(), `id="access-deny"`, "Deny stays offered")
}

// The open list merges leases, read grants and console unlocks into one
// list, what ends first first, in the JSON and on the page; each row ends
// with its own route, confirmed in the page.
func TestAxOpenListMergesInEndOrder(t *testing.T) {
	f := newAccessFixtureWith(t, true)
	ctx := context.Background()
	now := time.Now()
	for _, slug := range []string{"example-hr", "example-ops"} {
		_, err := store.EnsureProject(ctx, f.db, slug, slug)
		require.NoError(t, err)
	}
	lease := func(pid int32, length time.Duration) string {
		t.Helper()
		tok := access.ProcessToken{PID: pid, Version: uint64(pid) * 10, Boot: accessBoot}
		a, err := store.CreateAccessApproval(ctx, f.db, store.AccessRequest{Project: "example-finance", Kind: store.AccessKindRequest,
			Lease: time.Hour, TTL: time.Minute, PeerUID: 501, PeerPID: pid, ProcessKey: tok.Key(), SessionName: fmt.Sprintf("cc/pid-%d", pid)}, now)
		require.NoError(t, err)
		l, err := store.ApproveAccessFor(ctx, f.db, a.ID, a.Challenge, axDecidedByTerminal, length, now)
		require.NoError(t, err)
		return l.ID
	}
	late, soon := lease(410, 3*time.Hour), lease(420, 30*time.Minute)
	gtok := access.ProcessToken{PID: 430, Version: 4300, Boot: accessBoot}
	ga, err := store.CreateGrantRequest(ctx, f.db, store.GrantRequest{Project: "example-hr", From: "example-ops", Lease: time.Hour,
		TTL: time.Minute, PeerUID: 501, PeerPID: 430, ProcessKey: gtok.Key(), SessionName: "cc/grantee"}, now)
	require.NoError(t, err)
	grant, err := store.ApproveAccessGrant(ctx, f.db, ga.ID, ga.Challenge, axDecidedByTerminal, time.Hour, now)
	require.NoError(t, err)
	tokenHash := strings.Repeat("ab", 32)
	_, code, err := store.BeginTerminalUnlock(ctx, f.db, "example-finance", tokenHash, 2*time.Hour, time.Minute, now)
	require.NoError(t, err)
	unlock, err := store.ApproveTerminalUnlock(ctx, f.db, "example-finance", code, 2*time.Hour, now)
	require.NoError(t, err)
	order := []string{soon, grant.ID, unlock.ID, late}

	var list struct {
		Open []struct {
			Kind, ID, Project string
			ExpiresAt         time.Time `json:"expires_at"`
		} `json:"open"`
		Leases  []json.RawMessage `json:"leases"`
		Unlocks []json.RawMessage `json:"unlocks"`
		Grants  []json.RawMessage `json:"grants"`
	}
	rr := do(f.mux, authedReq(httptest.NewRequest(http.MethodGet, "/console/access?format=json", nil)))
	require.Equal(t, http.StatusOK, rr.Code)
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &list))
	require.Len(t, list.Leases, 2, "the rows the JSON always had stay")
	require.Len(t, list.Unlocks, 1)
	require.Len(t, list.Grants, 1)
	var ids, kinds []string
	for _, o := range list.Open {
		ids, kinds = append(ids, o.ID), append(kinds, o.Kind)
	}
	require.Equal(t, order, ids, "what ends first, first")
	require.Equal(t, []string{"lease", "grant", "unlock", "lease"}, kinds)

	page := axHTML(t, f.mux, "/console/access")
	var at []int
	for _, id := range order {
		i := strings.Index(page, `="`+id+`"`)
		require.Positive(t, i, id)
		at = append(at, i)
	}
	require.IsIncreasing(t, at, "the page lists them in the same order")
	row := between(t, page, `data-grant="`+grant.ID+`"`, `<li class="ax-open-row`, "</li>")
	require.Contains(t, row, `<strong>pid 430</strong> <span class="ax-quiet">reads</span> <strong>example-hr</strong> <span class="ax-quiet">for example-ops</span>`)
	require.Contains(t, row, `grant &middot; session <span class="mono">cc/grantee</span> &middot; read-only &middot; approved <time`)
	require.Contains(t, row, "</time> from a terminal &middot; writes only into example-ops from now on")
	require.Contains(t, row, `action="/console/access/grants/`+grant.ID+`/revoke"`)
	require.Contains(t, row, `data-confirm="Revoke this grant? pid 430 can no longer read example-hr`)
	row = between(t, page, `data-unlock-row="`+unlock.ID+`"`, `<li class="ax-open-row`, "</li>")
	require.Contains(t, row, "<strong>Another browser</strong>")
	require.Contains(t, row, "unlock &middot; by the daemon&#39;s account (console-open)")
	require.Contains(t, row, `data-confirm="Lock example-finance again? It closes in that browser until it is unlocked again." data-confirm-label="Lock it">Lock again</button>`)
	row = between(t, page, `data-lease="`+soon+`"`, `<li class="ax-open-row`, "</li>")
	require.Contains(t, row, `data-confirm="Revoke this lease? pid 420 loses example-finance at once, and asks again to get it back." data-confirm-label="Revoke it">Revoke</button>`)
	require.Contains(t, row, `class="badge time-left warn"`, "under 30 minutes left")
	require.NotContains(t, page, `class="ax-kinds"`, "four rows: no filter")
	require.Contains(t, page, "A grant opens a confidential project to one agent", "the hint names every kind this daemon has")
}

// Nothing open is one line; nothing waiting is one line.
func TestAxEmptyStatesAreOneLine(t *testing.T) {
	f := newAccessFixture(t)
	page := axHTML(t, f.mux, "/console/access")
	require.Equal(t, 1, strings.Count(page, `<p class="ax-empty">`+"Nothing is open."))
	require.Contains(t, page, `<p class="ax-empty">Nothing is open. Every locked project is closed to agents and browsers alike.</p>`)
	require.Contains(t, page, `<p class="ax-empty">Nothing is waiting. An agent refused at a locked project asks here.</p>`)
	require.NotContains(t, page, `<ul class="ax-open">`)
	require.NotContains(t, page, "waiting</span>", "no count while nothing waits")
	var list map[string]json.RawMessage
	rr := do(f.mux, authedReq(httptest.NewRequest(http.MethodGet, "/console/access?format=json", nil)))
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &list))
	require.JSONEq(t, `[]`, string(list["open"]))

	g := newGrantConsole(t)
	page = axHTML(t, g.mux, "/console/access")
	require.Contains(t, page, `<p class="ax-empty">Nothing is open. No agent holds a grant of a confidential project.</p>`)
	require.Contains(t, page, `<span class="badge quiet">advisory</span>`, "the advisory tier is a chip")
	require.Contains(t, page, "Without peer_auth these approvals are advisory: any process holding the bearer key can make one.")
}

// Past five rows the open list offers a chip per kind; ?open= keeps that
// kind's rows, and is ignored while the list is short enough to read whole.
func TestAxOpenListKindFilter(t *testing.T) {
	rows := make([]axOpenRow, 0, 7)
	for i, kind := range []string{axOpenLease, axOpenLease, axOpenUnlock, axOpenLease, axOpenUnlock, axOpenLease} {
		rows = append(rows, axOpenRow{Kind: kind, ID: fmt.Sprint(i)})
	}
	shown, kinds, on := axOpenFilter(rows, axOpenUnlock)
	require.Equal(t, axOpenUnlock, on)
	require.Len(t, shown, 2)
	require.Equal(t, []axOpenKind{
		{Label: "All", Href: "/console/access", N: 6},
		{Kind: axOpenLease, Label: "Leases", Href: "/console/access?open=lease", N: 4},
		{Kind: axOpenUnlock, Label: "Unlocks", Href: "/console/access?open=unlock", N: 2, On: true},
	}, kinds, "a kind with no rows has no chip")
	shown, _, on = axOpenFilter(rows, axOpenGrant)
	require.Empty(t, on, "a kind with no rows filters nothing")
	require.Len(t, shown, 6)
	shown, kinds, on = axOpenFilter(rows[:5], axOpenUnlock)
	require.Len(t, shown, 5)
	require.Nil(t, kinds)
	require.Empty(t, on)
}

// A waiting request is a card: who asks for what, the reason on one line in
// the agent's words, its chips and warnings, its time left and Review.
func TestAxWaitingCard(t *testing.T) {
	f := newAccessFixture(t)
	a := axRequest(t, f)
	_, err := store.CreateAccessApproval(context.Background(), f.db, store.AccessRequest{Project: "example-finance", Kind: store.AccessKindRefresh,
		Lease: time.Hour, TTL: 15 * time.Minute, PeerUID: 501, PeerPID: 700, ProcessKey: access.ProcessToken{PID: 700, Version: 7000, Boot: accessBoot}.Key()}, time.Now())
	require.NoError(t, err)
	page := axHTML(t, f.mux, "/console/access")
	require.Contains(t, page, `<span class="badge warn" aria-label="2 waiting">2 waiting</span>`)
	card := between(t, page, `data-request="`+a.ID+`"`, `<li class="ax-card"`, "</li>")
	require.Contains(t, card, `<span class="ax-prog">Claude Code 2.1.319</span> <span class="ax-quiet">pid 200</span> asks to <strong>read and write</strong> <strong>example-finance</strong>`)
	require.Contains(t, card, `<q>reconcile the ledger notes</q>`, "one line, the whitespace folded")
	require.Contains(t, card, `<span class="badge brand">locked</span>`)
	require.Contains(t, card, `data-left`)
	require.Contains(t, card, `<a class="btn small primary" href="/console/access/`+a.ID+`">Review</a>`)
	require.Contains(t, card, approval.ShortID(a.ID))
	require.Regexp(t, `<span class="badge warn">.*</svg>1 warning</span>`, card)
	require.Contains(t, card, "No session named this agent; its cwd and model are unknown.", "the chip's hint")

	refresh := between(t, page, `<span class="badge quiet">refresh</span>`, `<li class="ax-card"`, "</li>")
	require.Contains(t, refresh, `<span class="ax-prog">a process</span> <span class="ax-quiet">pid 700</span>`)
	require.Contains(t, refresh, `<p class="ax-card-reason is-none">no reason given</p>`)
	require.Regexp(t, `<span class="badge warn">.*</svg>2 warnings</span>`, refresh)
	require.Contains(t, refresh, "This session already holds a lease on the project; approving replaces it.",
		"the warnings chip's hint lists them, one sentence each")
}

// A decided request says what became of it in the card's foot: approved,
// with the way to Access requests; denied, which the agent hears.
func TestAxDecidedRequestFoot(t *testing.T) {
	f := newAccessFixture(t)
	ctx := context.Background()
	id, _ := f.file(t)
	facts, err := f.svc.Facts(ctx, id)
	require.NoError(t, err)
	_, err = f.svc.Approve(ctx, approval.Decision{ID: id, Duration: approval.For1h, Digest: facts.Digest,
		Proof: approval.Proof{Kind: approval.ProofDaemon}})
	require.NoError(t, err)
	page := axHTML(t, f.mux, "/console/access/"+id)
	foot := axSection(t, page, `<footer class="ax-result is-approved" data-result="approved">`, "</footer>")
	require.Contains(t, foot, `<p class="ax-result-title">Approved</p>`)
	require.Contains(t, foot, `<a class="btn" href="/console/access">See it on Access requests</a>`)
	require.Contains(t, page, `<span class="badge ok">approved</span>`)
	require.NotContains(t, page, `id="access-deny"`)
	require.NotContains(t, page, "data-left", "a decided request counts no time down")

	_, err = f.db.Exec(`UPDATE access_leases SET revoked_at = ?, revoked_reason = 'revoked'`, time.Now().UTC().Format(time.RFC3339Nano))
	require.NoError(t, err)
	id2, _ := f.file(t)
	status, out := f.post(t, "/console/access/"+id2+"/deny", url.Values{}, false)
	require.Equal(t, http.StatusOK, status, string(out["error"]))
	page = axHTML(t, f.mux, "/console/access/"+id2)
	foot = axSection(t, page, `data-result="denied"`, "</footer>")
	require.Contains(t, foot, "The agent hears it was denied, and when; it can ask again with a new request.")
	require.NotContains(t, foot, "approval dialog")
}

// Deny keeps UX 16's form and in-place confirm in both tiers: the advisory
// tier's page, which posted a plain form with no confirm, now asks in the
// page as the locked tier's does.
func TestAxDenyConfirmsInBothTiers(t *testing.T) {
	g := newGrantConsole(t)
	rr := g.post(t, "/console/access/request", url.Values{"project": {"example-finance"}}, true)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var filed struct{ ID string }
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &filed))
	page := axHTML(t, g.mux, "/console/access/"+filed.ID)
	require.Contains(t, page, `<button class="btn" id="access-deny" type="submit" form="access-deny-form" data-confirm="Deny this request? example-finance stays closed to that session." data-confirm-label="Deny it">Deny</button>`)
	require.Contains(t, page, `<form id="access-deny-form" method="post" action="/console/access/`+filed.ID+`/deny">`)
	require.Equal(t, 1, strings.Count(page, `/deny"`), "one deny form")
	// The advisory form's segments post with it.
	require.Contains(t, page, `<form id="access-advisory" class="ax-decide-form" method="post" action="/console/access/`+filed.ID+`/approve">`)
	require.Contains(t, page, `<input type="radio" name="duration" value="session" checked><span>Until its session ends</span>`)

	f := newAccessFixture(t)
	id, _ := f.file(t)
	status, _ := f.post(t, "/console/access/"+id+"/deny", url.Values{}, true)
	require.Equal(t, http.StatusForbidden, status, "an agent's process still cannot deny")
}

// The page loads the shared passkey helpers instead of copies of its own.
func TestAxRequestPageUsesThePasskeyScript(t *testing.T) {
	f := newAccessFixture(t)
	f.enrollPasskey(t)
	id, _ := f.file(t)
	page := axHTML(t, f.mux, "/console/access/"+id)
	require.Regexp(t, regexp.MustCompile(`<script src="/console/static/passkey\.js\?v=[0-9a-f]+"></script>`), page)
	for _, copied := range []string{"function b64urlToBuf", "function bufToB64url", "function requestOptions", "parseRequestOptionsFromJSON"} {
		require.NotContains(t, page, copied)
	}
	require.Contains(t, page, "window.SeamPasskey")
}

// The words the open list and the cards use.
func TestAxWords(t *testing.T) {
	creds := map[string]store.ApproverCredential{
		"01KEY": {ID: "01KEY", Kind: store.ApproverPasskey, Name: "desk key"},
		"01MAC": {ID: "01MAC", Kind: store.ApproverTouchID, Name: "laptop"},
	}
	for by, want := range map[string]string{
		"": "", axDecidedByTerminal: "from a terminal", axDecidedByBearerKey: "with the bearer key",
		approval.DecidedByDialog: "in the agent's approval dialog", "01KEY": "with the passkey desk key",
		"01MAC": "with Touch ID (laptop)", "01GONE": "with an approver since removed",
	} {
		require.Equal(t, want, axHowApproved(by, creds), by)
	}
	require.Equal(t, "No session named this agent; its cwd and model are unknown.",
		axSentence("no session named this agent; its cwd and model are unknown"))
	require.Equal(t, "read", axAsks(approval.Facts{Kind: store.AccessKindGrant, Access: "read-only"}))
	require.Equal(t, "read and write", axAsks(approval.Facts{Kind: store.AccessKindGrant, Access: "read-only", AskedAccess: "read-write"}))
	require.Equal(t, "read and write", axAsks(approval.Facts{Kind: store.AccessKindRequest, Access: "read-write"}))
	require.Equal(t, "move into", axAsks(approval.Facts{Kind: store.AccessKindMove}))
	require.Equal(t, "tmux", axWho(approval.Facts{Program: "/opt/homebrew/bin/tmux"}))
}

// Under peer_auth the list reads through the request's handle, which hides a
// locked project the browser has not unlocked: its leases and unlocks stay
// listed all the same, for the owner to end. They are metadata (slugs, ids,
// times) in tables no handle hides; nothing of the project's work is read.
func TestAxOpenListThroughTheRequestsHandle(t *testing.T) {
	lc := newLockedConsole(t)
	ctx := context.Background()
	cookie := lc.unlockByTerminal(t)
	live, err := store.LiveConsoleUnlocks(ctx, lc.db, lc.tokenHash(t, cookie), time.Now())
	require.NoError(t, err)
	require.Len(t, live, 1)
	agent := access.ProcessToken{PID: 200, Version: 2000, Boot: accessBoot}
	a, err := store.CreateAccessApproval(ctx, lc.db, store.AccessRequest{Project: lockedSlug, Kind: store.AccessKindRequest,
		Lease: time.Hour, TTL: time.Minute, PeerUID: 501, PeerPID: agent.PID, ProcessKey: agent.Key(), SessionName: "cc/ledger"}, time.Now())
	require.NoError(t, err)
	lease, err := store.ApproveAccessFor(ctx, lc.db, a.ID, a.Challenge, axDecidedByTerminal, time.Hour, time.Now())
	require.NoError(t, err)

	status, page := lc.get(t, "/console/access", "")
	require.Equal(t, http.StatusOK, status)
	require.NotContains(t, page, lockedSecret)
	row := between(t, page, `data-lease="`+lease.ID+`"`, `<li class="ax-open-row`, "</li>")
	require.Contains(t, row, `<strong>`+lockedSlug+`</strong>`)
	require.Contains(t, row, `session <span class="mono">cc/ledger</span>`)
	row = between(t, page, `data-unlock-row="`+live[0].ID+`"`, `<li class="ax-open-row`, "</li>")
	require.Contains(t, row, "<strong>Another browser</strong>", "this request carries no unlock cookie")

	_, page = lc.get(t, "/console/access", cookie)
	row = between(t, page, `data-unlock-row="`+live[0].ID+`"`, `<li class="ax-open-row`, "</li>")
	require.Contains(t, row, "<strong>This browser</strong>")
	require.Contains(t, row, "It closes in this browser until it is unlocked again.")
}

// ?format=json keeps every field the pages answered with before UX 41, and
// adds the open list.
func TestAxJSONKeepsItsFields(t *testing.T) {
	f := newAccessFixture(t)
	f.enrollPasskey(t)
	id, _ := f.file(t)
	keys := func(path string) map[string]json.RawMessage {
		t.Helper()
		rr := do(f.mux, authedReq(httptest.NewRequest(http.MethodGet, path, nil)))
		require.Equal(t, http.StatusOK, rr.Code)
		var out map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
		return out
	}
	list := keys("/console/access?format=json")
	for _, k := range []string{"requests", "terminal", "durations", "max_lease", "leases", "unlocks", "origin", "open"} {
		require.Contains(t, list, k)
	}
	one := keys("/console/access/" + id + "?format=json")
	for _, k := range []string{"request", "status", "page", "for", "passkey_options", "terminal", "durations", "max_lease"} {
		require.Contains(t, one, k)
	}
	for _, k := range []string{"challenge", "Who", "Selected", "WarnLines"} {
		require.NotContains(t, one, k, "view fields stay out of the JSON")
	}
}
