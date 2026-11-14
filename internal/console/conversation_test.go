package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// A conversation is one page (plan fenced-sessions P10): every session of it
// shows its home chain, every project it reached with the approval and who
// decided it, and what it wrote in each, with the stamps.

// convFixture is a console over a fresh database with files and events, and
// the fixture's projects: example-ops open, example-finance and
// example-legal confidential.
type convFixture struct {
	db  *sql.DB
	mgr *files.Manager
	rec *events.Recorder
	mux *http.ServeMux
}

func convConsole(t *testing.T) convFixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	dataDir := filepath.Join(dir, "data")
	mgr, err := files.NewManager(dataDir, db, slog.Default())
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })
	rec := events.NewRecorder(db)
	svc, err := New(Config{DB: db, Files: mgr, Events: rec, DataDir: dataDir, APIKey: testKey, Level: "advanced",
		Features: config.Features{Research: true}, Retrieve: retrieve.New(db, nil, config.Defaults().Budgets, nil)})
	require.NoError(t, err)
	mux := http.NewServeMux()
	svc.Register(mux)
	convProjects(t, db)
	_ = ctx
	return convFixture{db: db, mgr: mgr, rec: rec, mux: mux}
}

// convProjects registers the fixture's projects.
func convProjects(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	for slug, state := range map[string]core.Isolation{"example-ops": core.IsolationOpen,
		"example-finance": core.IsolationConfidential, "example-legal": core.IsolationConfidential} {
		_, err := store.EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
		require.NoError(t, store.SetProjectIsolation(ctx, db, slug, state))
	}
}

// convHome opens the conversation conv-p10's ambient row in project, as the
// SessionStart hook does.
func convHome(t *testing.T, db *sql.DB, project string, at time.Time) core.Session {
	t.Helper()
	s := core.Session{ID: mustID(t), Name: "cc/conv-p10", ProjectSlug: project, Status: core.SessionActive,
		ExternalClient: "claude-code", ExternalSessionID: "conv-p10", Ambient: true, Source: "startup",
		CreatedAt: at, UpdatedAt: at}
	require.NoError(t, store.CreateSession(context.Background(), db, s))
	return s
}

// convMoveInto moves the conversation from its ambient row left into target,
// decided by decidedBy, and returns the move's request and what it recorded.
func convMoveInto(t *testing.T, db *sql.DB, left core.Session, target, decidedBy string, at time.Time) (store.AccessApproval, store.MoveApproved) {
	t.Helper()
	ctx := context.Background()
	carry, err := store.CarryOf(left.ProjectSlug, []string{target}, []string{target})
	require.NoError(t, err)
	a, err := store.MoveRequestCreate(ctx, db, store.MoveRequest{Project: target, From: left.ProjectSlug, Carry: carry,
		Lease: time.Hour, TTL: 10 * time.Minute, ConversationClient: left.ExternalClient, ConversationID: left.ExternalSessionID,
		SessionID: left.ID, SessionName: left.Name}, at)
	require.NoError(t, err)
	done, err := store.MoveApproveWith(ctx, db, store.MoveDecision{ID: a.ID, Challenge: a.Challenge, DecidedBy: decidedBy}, at.Add(time.Minute))
	require.NoError(t, err)
	return a, done
}

// convWrote records that session wrote an item, as the MCP tools record it.
func convWrote(t *testing.T, rec *events.Recorder, kind core.EventKind, session core.Session, project, item string, payload map[string]any, at time.Time) {
	t.Helper()
	_, err := rec.Record(context.Background(), core.Event{Kind: kind, SessionID: session.ID, ProjectSlug: project, ItemID: item,
		Payload: payload, TS: at})
	require.NoError(t, err)
}

// convCard is the Conversation card of a session page, "" when it has none.
func convCard(page string) string {
	at := strings.Index(page, `id="conversation"`)
	if at < 0 {
		return ""
	}
	rest := page[at:]
	if end := strings.Index(rest, "</section>"); end >= 0 {
		rest = rest[:end]
	}
	return rest
}

func TestConversationPage_HomeChainReachAndWrites(t *testing.T) {
	f := convConsole(t)
	ctx := context.Background()
	t0 := time.Now().UTC().Add(-time.Hour)
	home := convHome(t, f.db, "example-ops", t0)
	move, moved := convMoveInto(t, f.db, home, "example-finance", "bearer-key", t0.Add(time.Minute))
	child := moved.Child

	// Before the move the conversation wrote a note in its open home, and
	// claimed a task there (a claim is no write).
	note := writeNote(t, f.mgr, "example-ops", "ops-notes", "ops handoff notes", "what the ops side needs")
	convWrote(t, f.rec, core.EventNoteWritten, home, "example-ops", note.ID, map[string]any{"title": note.Title}, t0.Add(30*time.Second))
	claimed := core.Task{ID: mustID(t), ProjectSlug: "example-ops", Title: "claimed ops chore", Status: core.TaskOpen, CreatedAt: t0, UpdatedAt: t0}
	require.NoError(t, store.CreateTask(ctx, f.db, claimed))
	convWrote(t, f.rec, core.EventTaskTransition, home, "example-ops", claimed.ID,
		map[string]any{"to": "in_progress", "claimed_by": home.ID}, t0.Add(40*time.Second))

	// In example-finance it asks to read and write example-legal: approved
	// in the agent's dialog. It asks again later, and that one waits.
	carry, err := store.CarryOf("example-finance", []string{"example-finance", "example-legal"}, []string{"example-finance", "example-legal"})
	require.NoError(t, err)
	ask, err := store.CreateGrantRequest(ctx, f.db, store.GrantRequest{Project: "example-legal", From: "example-finance",
		Lease: time.Hour, TTL: 10 * time.Minute, SessionID: child.ID, SessionName: child.Name, Access: store.GrantAccessWrite,
		Carry: carry, ConversationClient: "claude-code", ConversationID: "conv-p10"}, t0.Add(3*time.Minute))
	require.NoError(t, err)
	grant, err := store.GrantAccessApprove(ctx, f.db, ask.ID, ask.Challenge, approval.DecidedByDialog, 0, store.GrantAccessWrite, t0.Add(4*time.Minute))
	require.NoError(t, err)
	again, err := store.CreateGrantRequest(ctx, f.db, store.GrantRequest{Project: "example-legal", From: "example-finance",
		Lease: time.Hour, TTL: 10 * time.Hour, SessionID: child.ID, SessionName: child.Name,
		ConversationClient: "claude-code", ConversationID: "conv-p10"}, time.Now().UTC())
	require.NoError(t, err)
	// A lock's request, refused from this console: listed with the rest.
	unlock, err := store.CreateAccessApproval(ctx, f.db, store.AccessRequest{Project: "example-finance", Kind: store.AccessKindRequest,
		Lease: time.Hour, TTL: 10 * time.Minute, PeerUID: 501, PeerPID: 4242, ProcessKey: "boot-1:4242:7",
		SessionID: child.ID, SessionName: child.Name}, t0.Add(2*time.Minute))
	require.NoError(t, err)
	require.NoError(t, store.DenyAccess(ctx, f.db, unlock.ID, "console", t0.Add(150*time.Second)))

	// What it wrote there, stamped as the write paths stamp it.
	legalStamp := []core.Stamp{{Project: "example-legal", Via: core.StampViaGrant, Ref: ask.ID}}
	finStamp := []core.Stamp{{Project: "example-finance", Via: core.StampViaGrant, Ref: ask.ID}}
	mem, err := f.mgr.WriteMemory(ctx, core.Memory{ID: mustID(t), Kind: core.KindDecision, Name: "close-cadence",
		Description: "how the books close", Project: "example-finance", Body: "monthly\n", Created: t0, Updated: t0, ValidFrom: t0,
		MayContain: legalStamp})
	require.NoError(t, err)
	convWrote(t, f.rec, core.EventMemoryWritten, child, "example-finance", mem.ID, map[string]any{"name": mem.Name}, t0.Add(5*time.Minute))
	task := core.Task{ID: mustID(t), ProjectSlug: "example-legal", Title: "review the contract terms", Status: core.TaskOpen,
		CreatedAt: t0, UpdatedAt: t0}
	require.NoError(t, store.CreateTask(ctx, f.db, task))
	require.NoError(t, store.StampWorkItem(ctx, f.db, core.ItemKindTask, task.ID, task.ProjectSlug, finStamp, t0))
	convWrote(t, f.rec, core.EventTaskTransition, child, "example-legal", task.ID, map[string]any{"to": "open", "created": true}, t0.Add(6*time.Minute))
	child.Findings = "reconciled the quarter"
	child.UpdatedAt = t0.Add(7 * time.Minute)
	require.NoError(t, store.UpdateSession(ctx, f.db, child))
	require.NoError(t, store.StampWorkItem(ctx, f.db, core.ItemKindSession, child.ID, child.ProjectSlug, legalStamp, t0))

	// Another conversation, and what it wrote, are not this one's.
	other := core.Session{ID: mustID(t), Name: "cc/other", ProjectSlug: "example-ops", Status: core.SessionActive,
		ExternalClient: "claude-code", ExternalSessionID: "conv-other", Ambient: true, CreatedAt: t0, UpdatedAt: t0}
	require.NoError(t, store.CreateSession(ctx, f.db, other))
	otherNote := writeNote(t, f.mgr, "example-ops", "other-notes", "someone else's notes", "not ours")
	convWrote(t, f.rec, core.EventNoteWritten, other, "example-ops", otherNote.ID, nil, t0.Add(8*time.Minute))

	page := getHTMLBody(t, f.mux, "/console/sessions/"+child.ID)
	card := convCard(page)
	require.NotEmpty(t, card, "a moved conversation has its card")

	// The home chain: example-ops, then the move into example-finance.
	chain := card[strings.Index(card, `class="conv-chain"`):strings.Index(card, "</ol>")]
	require.Less(t, strings.Index(chain, "example-ops"), strings.Index(chain, "example-finance"), "the chain runs oldest first")
	require.Contains(t, chain, `href="/console/sessions/`+home.ID+`"`)
	require.Contains(t, chain, `href="/console/sessions/`+child.ID+`"`)
	require.Contains(t, chain, "this session")
	require.Contains(t, chain, approval.ShortID(move.ID))
	require.Contains(t, chain, "decided by the bearer key: seam approve or the console (advisory)")
	require.NotContains(t, chain, `href="/console/access/`, "no access pages are served here")

	// Every project it reached, with the approval and who decided it.
	ops := between(t, card, `id="conv-reach-example-ops"`, `<div class="conv-reach-row`, "</ul>")
	require.Contains(t, ops, `data-way="home"`)
	fin := between(t, card, `id="conv-reach-example-finance"`, `<div class="conv-reach-row`, "</ul>")
	require.Contains(t, fin, `data-way="moved"`)
	require.Contains(t, fin, approval.ShortID(move.ID))
	require.Contains(t, fin, "asked to unlock")
	require.Contains(t, fin, `<span class="conv-state">denied</span>`)
	require.Contains(t, fin, approval.ShortID(unlock.ID)+"</span> &middot; decided by this console")
	legal := between(t, card, `id="conv-reach-example-legal"`, `<div class="conv-reach-row`, "</ul>")
	require.Contains(t, legal, `data-way="write"`)
	require.Contains(t, legal, "read and write grant")
	require.Contains(t, legal, approval.ShortID(grant.ApprovalID))
	require.Contains(t, legal, "decided by the agent&#39;s approval dialog (advisory)")
	require.Contains(t, legal, `<span class="conv-state">live</span>`)
	require.Contains(t, legal, "asked to read")
	require.Contains(t, legal, approval.ShortID(again.ID))
	require.Contains(t, legal, `<span class="conv-state">pending</span>`)

	// What it wrote, by project, with the stamps.
	finW := between(t, card, `id="conv-w-example-finance"`, `<div class="conv-written`, `</div>`)
	require.Contains(t, finW, "close-cadence")
	require.Contains(t, finW, core.MayContainLine(legalStamp))
	require.Contains(t, finW, "findings of "+child.Name)
	legalW := between(t, card, `id="conv-w-example-legal"`, `<div class="conv-written`, `</div>`)
	require.Contains(t, legalW, "review the contract terms")
	require.Contains(t, legalW, core.MayContainLine(finStamp))
	opsW := between(t, card, `id="conv-w-example-ops"`, `<div class="conv-written`, `</div>`)
	require.Contains(t, opsW, "ops handoff notes")
	require.NotContains(t, opsW, "data-may-contain", "an unstamped item shows no stamp")
	require.NotContains(t, card, "claimed ops chore", "a claim is no write")
	require.NotContains(t, card, "someone else", "another conversation's writes are not this one's")
	require.NotContains(t, card, "cc/other")
	require.Contains(t, page, `data-may-contain`, "the findings card shows the session's own stamp")

	// One page for every session of it: the pre-move session shows the same
	// card, with itself marked.
	before := convCard(getHTMLBody(t, f.mux, "/console/sessions/"+home.ID))
	require.Contains(t, before, "close-cadence")
	require.Contains(t, before, "review the contract terms")
	beforeChain := before[strings.Index(before, `class="conv-chain"`):strings.Index(before, "</ol>")]
	require.Less(t, strings.Index(beforeChain, "this session"), strings.Index(beforeChain, "example-finance"),
		"the pre-move session is the one marked")

	// The JSON carries the same card.
	var d struct {
		Conversation *struct {
			Hops []struct {
				Project   string `json:"project"`
				SessionID string `json:"sessionId"`
				Current   bool   `json:"current"`
				Move      *struct {
					ApprovalID string `json:"approvalId"`
					DecidedBy  string `json:"decidedBy"`
				} `json:"move"`
			} `json:"hops"`
			Reach []struct {
				Project string `json:"project"`
				Ways    []struct {
					Kind string `json:"kind"`
				} `json:"ways"`
			} `json:"reach"`
			Written []struct {
				Project string `json:"project"`
				Items   []struct {
					Kind       string       `json:"kind"`
					ID         string       `json:"id"`
					MayContain []core.Stamp `json:"mayContain"`
				} `json:"items"`
			} `json:"written"`
		} `json:"conversation"`
	}
	getJSON(t, f.mux, "/console/sessions/"+child.ID+"?format=json", &d)
	require.NotNil(t, d.Conversation)
	require.Len(t, d.Conversation.Hops, 2)
	require.Equal(t, "example-ops", d.Conversation.Hops[0].Project)
	require.Nil(t, d.Conversation.Hops[0].Move)
	require.Equal(t, child.ID, d.Conversation.Hops[1].SessionID)
	require.True(t, d.Conversation.Hops[1].Current)
	require.Equal(t, move.ID, d.Conversation.Hops[1].Move.ApprovalID)
	var reached []string
	for _, r := range d.Conversation.Reach {
		reached = append(reached, r.Project)
	}
	require.Equal(t, []string{"example-ops", "example-finance", "example-legal"}, reached)
	require.Equal(t, []string{"example-ops", "example-finance", "example-legal"}, func() []string {
		var out []string
		for _, w := range d.Conversation.Written {
			out = append(out, w.Project)
		}
		return out
	}(), "written in reach's order: the homes first, then the grant's target")
}

// A locked hop (peer_auth): the conversation moved from an open project into
// a locked one. Its page shows the locked project's name, the move's approval
// and who decided it, and the way to unlock it in this browser -- no session,
// no item title, no count and no stamp from inside -- until this browser
// holds a live unlock.
func TestConversationPage_LockedHopShowsItsNameAndNothingInside(t *testing.T) {
	lc := newLockedConsole(t)
	ctx := context.Background()
	t0 := time.Now().UTC().Add(-time.Hour)
	home := convHome(t, lc.db, openSlug, t0)
	move, moved := convMoveInto(t, lc.db, home, lockedSlug, "daemon-account", t0.Add(time.Minute))
	child := moved.Child

	// Before the move, an open note; after it, writes inside the locked
	// project, one of them stamped from a third project.
	note := writeNote(t, lc.mgr, openSlug, "ops-before", "ops note before the move", "the open side")
	convWrote(t, lc.rec, core.EventNoteWritten, home, openSlug, note.ID, nil, t0.Add(30*time.Second))
	inside, err := lc.mgr.WriteMemory(ctx, core.Memory{ID: mustID(t), Kind: core.KindGotcha, Name: "ledger-" + lockedSecret,
		Description: "the close " + lockedSecret, Project: lockedSlug, Body: lockedSecret + "\n", Created: t0, Updated: t0, ValidFrom: t0,
		MayContain: []core.Stamp{{Project: "example-legal", Via: core.StampViaGrant, Ref: "01P10STAMPREF0000000000000"}}})
	require.NoError(t, err)
	convWrote(t, lc.rec, core.EventMemoryWritten, child, lockedSlug, inside.ID, map[string]any{"name": inside.Name}, t0.Add(5*time.Minute))
	child.Findings = "found " + lockedSecret
	require.NoError(t, store.UpdateSession(ctx, lc.db, child))
	require.NoError(t, store.StampWorkItem(ctx, lc.db, core.ItemKindSession, child.ID, child.ProjectSlug,
		[]core.Stamp{{Project: "example-legal", Via: core.StampViaGrant, Ref: "01P10STAMPREF0000000000000"}}, t0))

	read := func(t *testing.T, unlock string, asJSON bool) string {
		t.Helper()
		req := lc.browser(http.MethodGet, "/console/sessions/"+home.ID, unlock, nil)
		if asJSON {
			req.Header.Set("Accept", "application/json")
		}
		rr := lc.do(req)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		return rr.Body.String()
	}
	page := read(t, "", false)
	card := convCard(page)
	require.NotEmpty(t, card)
	for _, inner := range []string{lockedSecret, child.ID, child.Name, inside.ID, "example-legal", "more not shown"} {
		require.NotContains(t, card, inner, "nothing from inside the locked hop: %s", inner)
	}
	require.NotContains(t, page, child.ID, "the child's id is nowhere on the page")
	require.NotContains(t, page, lockedSecret)
	hop := between(t, card, `data-locked-hop="`+lockedSlug+`"`, `<li class="conv-hop`, "</li>")
	require.Contains(t, hop, lockedSlug)
	require.Contains(t, hop, approval.ShortID(move.ID), "the move that reached it is named")
	require.Contains(t, hop, "decided by the terminal (seamlessd approve)")
	require.Contains(t, hop, `href="/console/access/`+move.ID+`"`, "with its request's page, served under peer_auth")
	require.Contains(t, hop, `href="/console/unlock/`+lockedSlug+`?item=session&amp;next=%2Fconsole%2Fsessions%2F`+home.ID+`"`)
	require.NotContains(t, hop, "conv-session", "no session from inside")
	require.Contains(t, card, `data-locked-reach="`+lockedSlug+`"`)
	require.Contains(t, card, `data-locked-out="`+lockedSlug+`"`)
	require.NotContains(t, card, `id="conv-w-`+lockedSlug+`"`, "no group of items from inside")
	require.Contains(t, card, "ops note before the move", "the open side is shown")
	require.Contains(t, card, "findings of "+home.Name, "and the pre-move session's own findings, in the open project")

	// The JSON says the same: the hop names its project and is locked. (The
	// session's own metadata keeps the move's pointer, which names the child
	// by id: that is the record of the move, not the card.)
	conv := func(t *testing.T, body string) string {
		t.Helper()
		var top map[string]json.RawMessage
		require.NoError(t, json.Unmarshal([]byte(body), &top))
		require.Contains(t, top, "conversation")
		return string(top["conversation"])
	}
	js := conv(t, read(t, "", true))
	require.NotContains(t, js, lockedSecret)
	require.NotContains(t, js, child.ID)
	require.NotContains(t, js, inside.ID)
	var d struct {
		Hops []struct {
			Project   string `json:"project"`
			Locked    bool   `json:"locked"`
			SessionID string `json:"sessionId"`
		} `json:"hops"`
	}
	require.NoError(t, json.Unmarshal([]byte(js), &d))
	require.Len(t, d.Hops, 2)
	require.True(t, d.Hops[1].Locked)
	require.Equal(t, lockedSlug, d.Hops[1].Project)
	require.Empty(t, d.Hops[1].SessionID)

	// The bearer key (not a browser) reads the base handle: locked the same.
	req := httptest.NewRequest(http.MethodGet, "/console/sessions/"+home.ID+"?format=json", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	rr := lc.do(req)
	require.Equal(t, http.StatusOK, rr.Code)
	key := conv(t, rr.Body.String())
	require.NotContains(t, key, child.ID)
	require.NotContains(t, key, lockedSecret)
	require.Contains(t, key, `"locked": true`)

	// A browser that unlocked the project reads inside the hop.
	cookie := lc.unlockByTerminal(t)
	open := convCard(read(t, cookie, false))
	require.Contains(t, open, `href="/console/sessions/`+child.ID+`"`)
	require.Contains(t, open, "ledger-"+lockedSecret)
	require.Contains(t, open, `id="conv-w-`+lockedSlug+`"`)
	require.Contains(t, open, "may contain content from example-legal")
	require.NotContains(t, open, `data-locked-hop=`)
	require.NotContains(t, open, `data-locked-out=`)
}

// A conversation that never moved, asked for nothing and wrote nothing
// stamped renders no card: its page is as it was, HTML and JSON -- also on an
// install where other items carry stamps.
func TestConversationPage_NoRecordNoCard(t *testing.T) {
	f := convConsole(t)
	ctx := context.Background()
	t0 := time.Now().UTC().Add(-time.Hour)
	home := convHome(t, f.db, "example-ops", t0)
	link := core.Session{ID: mustID(t), Name: "sess/linked", ProjectSlug: "example-legal", Status: core.SessionActive,
		ExternalClient: "claude-code", ExternalSessionID: "conv-p10", CreatedAt: t0.Add(time.Minute), UpdatedAt: t0.Add(time.Minute)}
	require.NoError(t, store.CreateSession(ctx, f.db, link))
	note := writeNote(t, f.mgr, "example-ops", "plain-notes", "plain notes", "nothing held")
	convWrote(t, f.rec, core.EventNoteWritten, home, "example-ops", note.ID, nil, t0.Add(2*time.Minute))
	bare := core.Session{ID: mustID(t), Name: "sess/bare", ProjectSlug: "example-ops", Status: core.SessionCompleted,
		Findings: "done", CreatedAt: t0, UpdatedAt: t0}
	require.NoError(t, store.CreateSession(ctx, f.db, bare))

	check := func(t *testing.T) {
		t.Helper()
		for _, s := range []core.Session{home, link, bare} {
			page := getHTMLBody(t, f.mux, "/console/sessions/"+s.ID)
			require.NotContains(t, page, `id="conversation"`, s.Name)
			require.NotContains(t, page, `class="conv-`, s.Name)
			rr := getPeek(t, f.mux, "/console/sessions/"+s.ID+"?format=json")
			require.Equal(t, http.StatusOK, rr.Code)
			require.NotContains(t, rr.Body.String(), `"conversation"`, s.Name)
		}
	}
	check(t)

	// A lock's own request (peer_auth, with isolation.requests off) gives it
	// no card.
	_, err := store.CreateAccessApproval(ctx, f.db, store.AccessRequest{Project: "example-legal", Kind: store.AccessKindRequest,
		Lease: time.Hour, TTL: 10 * time.Minute, PeerUID: 501, PeerPID: 4242, ProcessKey: "boot-1:4242:7",
		SessionID: home.ID, SessionName: home.Name}, t0)
	require.NoError(t, err)
	check(t)

	// A stamp elsewhere on the install, on an item this conversation did not
	// write, gives it no card either.
	task := core.Task{ID: mustID(t), ProjectSlug: "example-legal", Title: "theirs", Status: core.TaskOpen, CreatedAt: t0, UpdatedAt: t0}
	require.NoError(t, store.CreateTask(ctx, f.db, task))
	require.NoError(t, store.StampWorkItem(ctx, f.db, core.ItemKindTask, task.ID, task.ProjectSlug,
		[]core.Stamp{{Project: "example-finance", Via: core.StampViaGrant, Ref: "01P10OTHERREF0000000000000"}}, t0))
	check(t)

	// A bare session (a client with no hooks) that holds a legacy read grant
	// is a conversation of its own, with a card.
	ask, err := store.CreateGrantRequest(ctx, f.db, store.GrantRequest{Project: "example-legal", From: "example-ops",
		Lease: time.Hour, TTL: 10 * time.Minute, SessionID: bare.ID, SessionName: bare.Name}, t0)
	require.NoError(t, err)
	_, err = store.ApproveAccessGrant(ctx, f.db, ask.ID, ask.Challenge, "bearer-key", 0, t0.Add(time.Minute))
	require.NoError(t, err)
	card := convCard(getHTMLBody(t, f.mux, "/console/sessions/"+bare.ID))
	require.NotEmpty(t, card)
	require.Contains(t, card, `data-way="home"`)
	require.Contains(t, card, "read-only grant")
	require.NotContains(t, card, "bound with session_start", "the bare session is the conversation's home, not a link")
}
