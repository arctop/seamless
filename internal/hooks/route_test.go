package hooks

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/harvest"
	"github.com/arctop/seamless/internal/plans"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

const (
	rtOps       = "example-ops"     // open: the agent's cwd maps here
	rtDocs      = "example-docs"    // open
	rtFin       = "example-finance" // confidential; its own repository too
	rtLegal     = "example-legal"   // confidential
	rtSealed    = "example-sealed"  // sealed; its own repository too
	rtConv      = "conv-5a1d-0001"  // the conversation's client session id
	rtSecret    = "SECRET-DIGEST"   // text only the isolated project may hold
	rtOpsCWD    = "/work/ops"
	rtFinCWD    = "/work/finance"
	rtSealedCWD = "/work/sealed"
	rtHarvest   = "(auto-harvested) " + rtSecret + " of the review."
)

// routeEnv is a hook server with the files layer and plan capture on, over an
// open project the agent's cwd maps to and isolated projects it can bind
// itself to with session_start, for the record routing (route.go).
type routeEnv struct {
	t        *testing.T
	ts       *httptest.Server
	db       *sql.DB
	mgr      *files.Manager
	rec      *events.Recorder
	plansDir string
	clock    time.Time
}

func newRouteEnv(t *testing.T) *routeEnv {
	t.Helper()
	return newRouteEnvWith(t, nil)
}

// newRouteEnvWith is newRouteEnv with change applied to the handler's config.
func newRouteEnvWith(t *testing.T, change func(*Config)) *routeEnv {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, store.SetSetting(ctx, db, store.SettingRepoProjectMap,
		`{"`+rtOpsCWD+`":"`+rtOps+`","`+rtFinCWD+`":"`+rtFin+`","`+rtSealedCWD+`":"`+rtSealed+`"}`))
	for _, p := range []struct {
		slug  string
		state core.Isolation
	}{
		{rtOps, core.IsolationOpen}, {rtDocs, core.IsolationOpen},
		{rtFin, core.IsolationConfidential}, {rtLegal, core.IsolationConfidential},
		{rtSealed, core.IsolationSealed},
	} {
		_, err := store.EnsureProject(ctx, db, p.slug, p.slug)
		require.NoError(t, err)
		require.NoError(t, store.SetProjectIsolation(ctx, db, p.slug, p.state))
	}
	mgr, err := files.NewManager(filepath.Join(dir, "data"), db, nil)
	require.NoError(t, err)
	plansDir := filepath.Join(dir, "plans")
	require.NoError(t, os.MkdirAll(plansDir, 0o755))
	rec := events.NewRecorder(db)
	cfg := Config{
		DB: db, Retrieve: retrieve.New(db, nil, config.Budgets{MaxBriefingTokens: 1500, RecallBudgetTokens: 1000}, nil),
		Events: rec, Files: mgr, APIKey: testKey,
		PlanCapture: config.PlanCapture{Enabled: true, AutoTask: true}, PlansDir: plansDir,
	}
	if change != nil {
		change(&cfg)
	}
	h := NewHandler(cfg)
	mux := http.NewServeMux()
	h.Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return &routeEnv{t: t, ts: ts, db: db, mgr: mgr, rec: rec, plansDir: plansDir,
		clock: time.Now().UTC().Add(-time.Hour)}
}

// hook posts one hook as client, returning its decoded reply and the refusal
// notice `seam hook` would print.
func (e *routeEnv) hook(client Client, endpoint string, body map[string]any) (map[string]any, string) {
	e.t.Helper()
	u := e.ts.URL + "/api/hooks/" + endpoint
	if client == ClientCodex {
		u += "?client=codex"
	}
	resp, out := post(e.t, u, testKey, body)
	require.Equal(e.t, http.StatusOK, resp.StatusCode)
	return out, resp.Header.Get(harvest.RefusedHeader)
}

// start opens the conversation's ambient row from cwd, as the SessionStart
// hook does.
func (e *routeEnv) start(client Client, cwd string) core.Session {
	e.t.Helper()
	e.hook(client, "session-start", map[string]any{"session_id": rtConv, "cwd": cwd, "source": "startup"})
	return requireAmbientSession(e.t, e.db, client, rtConv)
}

// bind links an explicit session in project to the conversation, as
// `session_start project=<slug>` from the agent's own process does
// (linkedExternalIdentity stamps its external identity); each one is newer
// than the last.
func (e *routeEnv) bind(client Client, project string, status core.SessionStatus, findings string) core.Session {
	e.t.Helper()
	id, err := core.NewID()
	require.NoError(e.t, err)
	e.clock = e.clock.Add(time.Second)
	s := core.Session{
		ID: id, Name: "sess/" + id, ProjectSlug: project, Status: status, Findings: findings,
		CWD: rtOpsCWD, Source: "explicit", ExternalSessionID: rtConv, ExternalClient: client.externalIdentity(),
		CreatedAt: e.clock, UpdatedAt: e.clock,
	}
	require.NoError(e.t, store.CreateSession(context.Background(), e.db, s))
	return s
}

func (e *routeEnv) session(id string) core.Session {
	e.t.Helper()
	s, ok, err := store.SessionByID(context.Background(), e.db, id)
	require.NoError(e.t, err)
	require.True(e.t, ok, "session %s", id)
	return s
}

// end posts the conversation's SessionEnd with a transcript whose last
// assistant message is the secret, returning the refusal notice.
func (e *routeEnv) end() string {
	e.t.Helper()
	transcript := writeTranscript(e.t,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"`+rtSecret+` of the review."}]}}`)
	_, refused := e.hook(ClientClaudeCode, "session-end",
		map[string]any{"session_id": rtConv, "transcript_path": transcript, "reason": "logout"})
	return refused
}

// holding counts, per project, the rows a GROUP BY project query matches:
// where a piece of content ended up.
func (e *routeEnv) holding(query string, args ...any) map[string]int {
	e.t.Helper()
	rows, err := e.db.QueryContext(context.Background(), query, args...)
	require.NoError(e.t, err)
	defer func() { _ = rows.Close() }()
	out := map[string]int{}
	for rows.Next() {
		var project string
		var n int
		require.NoError(e.t, rows.Scan(&project, &n))
		out[project] = n
	}
	require.NoError(e.t, rows.Err())
	return out
}

// whereIs reports every project whose events, sessions or search rows hold
// text.
func (e *routeEnv) whereIs(text string) map[string]map[string]int {
	like := "%" + text + "%"
	return map[string]map[string]int{
		"events": e.holding(`SELECT project_slug, count(*) FROM events WHERE payload LIKE ? GROUP BY project_slug`, like),
		"sessions": e.holding(
			`SELECT project_slug, count(*) FROM sessions WHERE findings LIKE ? OR metadata LIKE ? GROUP BY project_slug`, like, like),
		"fts": e.holding(`SELECT project, count(*) FROM fts WHERE body LIKE ? GROUP BY project`, like),
	}
}

// endedEvents are the session.ended events recorded for one session.
func (e *routeEnv) endedEvents(sessionID string) []core.Event {
	e.t.Helper()
	var out []core.Event
	for _, ev := range eventsOfKind(e.t, e.rec, core.EventSessionEnded) {
		if ev.SessionID == sessionID {
			out = append(out, ev)
		}
	}
	return out
}

// An agent in an open project's repository that binds itself to a
// confidential or sealed project with session_start keeps its record there:
// the SessionEnd harvest becomes that session's findings, and the ambient row
// in the open project ends with a pointer, its session.ended event and search
// row holding nothing of the harvest.
func TestFencedRoute_SessionEndHarvestGoesToTheIsolatedSession(t *testing.T) {
	for _, project := range []string{rtFin, rtSealed} {
		t.Run(project, func(t *testing.T) {
			e := newRouteEnv(t)
			amb := e.start(ClientClaudeCode, rtOpsCWD)
			require.Equal(t, rtOps, amb.ProjectSlug)
			link := e.bind(ClientClaudeCode, project, core.SessionActive, "")

			require.Empty(t, e.end())

			got := e.session(link.ID)
			require.Equal(t, core.SessionCompleted, got.Status)
			require.Equal(t, rtHarvest, got.Findings)
			amb = e.session(amb.ID)
			require.Equal(t, core.SessionCompleted, amb.Status)
			require.Equal(t, "Continued in project "+project+"; this conversation's findings go there, not here.", amb.Findings)

			ambEnded := e.endedEvents(amb.ID)
			require.Len(t, ambEnded, 1)
			require.Equal(t, rtOps, ambEnded[0].ProjectSlug)
			require.Equal(t, amb.Findings, ambEnded[0].Payload["findings"])
			require.Equal(t, false, ambEnded[0].Payload["harvested"])
			require.Equal(t, true, ambEnded[0].Payload["ambient"])
			linkEnded := e.endedEvents(link.ID)
			require.Len(t, linkEnded, 1)
			require.Equal(t, project, linkEnded[0].ProjectSlug)
			require.Equal(t, rtHarvest, linkEnded[0].Payload["findings"])
			require.Equal(t, true, linkEnded[0].Payload["harvested"])
			require.Equal(t, true, linkEnded[0].Payload["linked"])

			require.Equal(t, map[string]map[string]int{
				"events": {project: 1}, "sessions": {project: 1}, "fts": {project: 1},
			}, e.whereIs(rtSecret), "the harvest is in the isolated project and nowhere else")
		})
	}
}

// The routing follows the conversation, not the binding: a later rebind to an
// open project, a link the agent ended itself, or one the reaper expired all
// keep the record in the isolated project. Findings the agent wrote itself are
// kept, and only the hooks' own are replaced.
func TestFencedRoute_RebindKeepsTheRouting(t *testing.T) {
	t.Run("rebound to an open project", func(t *testing.T) {
		e := newRouteEnv(t)
		amb := e.start(ClientClaudeCode, rtOpsCWD)
		link := e.bind(ClientClaudeCode, rtFin, core.SessionActive, "")
		docs := e.bind(ClientClaudeCode, rtDocs, core.SessionActive, "")

		require.Empty(t, e.end())
		require.Equal(t, rtHarvest, e.session(link.ID).Findings)
		require.Equal(t, core.SessionCompleted, e.session(docs.ID).Status)
		require.Empty(t, e.session(docs.ID).Findings, "an open link keeps its own findings, as always")
		require.Contains(t, e.session(amb.ID).Findings, "Continued in project "+rtFin)
		require.Equal(t, map[string]int{rtFin: 1}, e.whereIs(rtSecret)["events"])
	})

	t.Run("ended by the agent with its own findings", func(t *testing.T) {
		e := newRouteEnv(t)
		amb := e.start(ClientClaudeCode, rtOpsCWD)
		link := e.bind(ClientClaudeCode, rtFin, core.SessionCompleted, "Reviewed the example ledger.")

		require.Empty(t, e.end())
		require.Equal(t, "Reviewed the example ledger.", e.session(link.ID).Findings, "the agent's own findings are kept")
		require.Empty(t, e.endedEvents(link.ID), "it was not ended again")
		require.Contains(t, e.session(amb.ID).Findings, "Continued in project "+rtFin)
		require.Equal(t, map[string]map[string]int{"events": {}, "sessions": {}, "fts": {}}, e.whereIs(rtSecret),
			"the harvest landed nowhere")
	})

	t.Run("expired by the reaper", func(t *testing.T) {
		e := newRouteEnv(t)
		e.start(ClientClaudeCode, rtOpsCWD)
		link := e.bind(ClientClaudeCode, rtSealed, core.SessionExpired, "")

		require.Empty(t, e.end())
		got := e.session(link.ID)
		require.Equal(t, core.SessionCompleted, got.Status, "the known end completes it")
		require.Equal(t, rtHarvest, got.Findings)
		ended := e.endedEvents(link.ID)
		require.Len(t, ended, 1)
		require.Equal(t, rtSealed, ended[0].ProjectSlug)
		require.Equal(t, map[string]int{rtSealed: 1}, e.whereIs(rtSecret)["events"])
	})

	t.Run("a resumed conversation's next end", func(t *testing.T) {
		e := newRouteEnv(t)
		amb := e.start(ClientClaudeCode, rtOpsCWD)
		link := e.bind(ClientClaudeCode, rtFin, core.SessionActive, "")
		require.Empty(t, e.end())
		e.hook(ClientClaudeCode, "session-start", map[string]any{"session_id": rtConv, "cwd": rtOpsCWD, "source": "resume"})
		require.Equal(t, core.SessionActive, e.session(amb.ID).Status)

		transcript := writeTranscript(t,
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Second pass done."}]}}`)
		e.hook(ClientClaudeCode, "session-end", map[string]any{"session_id": rtConv, "transcript_path": transcript, "reason": "logout"})
		require.Equal(t, "(auto-harvested) Second pass done.", e.session(link.ID).Findings,
			"the earlier harvest is the hooks' own, so the new one replaces it")
		require.Contains(t, e.session(amb.ID).Findings, "Continued in project "+rtFin)
	})

	t.Run("two links into one isolated project: the newest", func(t *testing.T) {
		e := newRouteEnv(t)
		e.start(ClientClaudeCode, rtOpsCWD)
		older := e.bind(ClientClaudeCode, rtFin, core.SessionActive, "")
		newer := e.bind(ClientClaudeCode, rtFin, core.SessionActive, "")

		require.Empty(t, e.end())
		require.Equal(t, rtHarvest, e.session(newer.ID).Findings)
		require.Equal(t, core.SessionCompleted, e.session(older.ID).Status)
		require.Empty(t, e.session(older.ID).Findings)
	})
}

// A conversation whose ambient row is in an open project and whose links reach
// two isolated projects keeps its harvest in none of them: every row it ends
// takes the line saying why, the agent's own findings stay, and the hook
// answers with the notice. (A conversation native to an isolated project keeps
// its record at home instead: TestFencedRoute_UnlinkedAndNativeUnchanged.)
func TestFencedRoute_TwoIsolatedBindingsWithholdTheHarvest(t *testing.T) {
	line := "Not recorded: this conversation was bound to more than one isolated project (" +
		rtFin + ", " + rtSealed + "), and its record goes to none of them."
	notice := "Seamless did not record this conversation's findings: this conversation was bound to more than one " +
		"isolated project (" + rtFin + ", " + rtSealed + "), and its record goes to none of them."

	t.Run("from an open project", func(t *testing.T) {
		e := newRouteEnv(t)
		amb := e.start(ClientClaudeCode, rtOpsCWD)
		fin := e.bind(ClientClaudeCode, rtFin, core.SessionActive, "Closed the example ledger.")
		sealed := e.bind(ClientClaudeCode, rtSealed, core.SessionActive, "")

		require.Equal(t, notice, e.end())
		require.Equal(t, line, e.session(amb.ID).Findings)
		require.Equal(t, line, e.session(sealed.ID).Findings)
		require.Equal(t, "Closed the example ledger.", e.session(fin.ID).Findings)
		for _, id := range []string{amb.ID, fin.ID, sealed.ID} {
			require.Equal(t, core.SessionCompleted, e.session(id).Status)
			ended := e.endedEvents(id)
			require.Len(t, ended, 1)
			require.Equal(t, false, ended[0].Payload["harvested"])
		}
		require.Equal(t, map[string]map[string]int{"events": {}, "sessions": {}, "fts": {}}, e.whereIs(rtSecret))
	})
}

// Nothing changes for a conversation whose links reach no isolated project
// (no link, or a link into another open project), nor for one native to a
// confidential or sealed project, whatever it binds itself to: an open
// project, its own home, or one or more other isolated projects. Its home
// keeps its record, with no pointer and no withheld line.
func TestFencedRoute_UnlinkedAndNativeUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name, cwd, home string
		links           []string
	}{
		{name: "no link", cwd: rtOpsCWD, home: rtOps},
		{name: "a link into an open project", cwd: rtOpsCWD, home: rtOps, links: []string{rtDocs}},
		{name: "native confidential", cwd: rtFinCWD, home: rtFin},
		{name: "native confidential bound to an open project", cwd: rtFinCWD, home: rtFin, links: []string{rtOps}},
		{name: "native confidential bound to itself", cwd: rtFinCWD, home: rtFin, links: []string{rtFin}},
		{name: "native confidential bound to another confidential", cwd: rtFinCWD, home: rtFin, links: []string{rtLegal}},
		{name: "native confidential bound to a sealed project", cwd: rtFinCWD, home: rtFin, links: []string{rtSealed}},
		{name: "native confidential bound to two isolated projects", cwd: rtFinCWD, home: rtFin,
			links: []string{rtLegal, rtSealed}},
		{name: "native sealed bound to a confidential project", cwd: rtSealedCWD, home: rtSealed, links: []string{rtFin}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newRouteEnv(t)
			amb := e.start(ClientClaudeCode, tc.cwd)
			require.Equal(t, tc.home, amb.ProjectSlug)
			var links []core.Session
			for _, p := range tc.links {
				links = append(links, e.bind(ClientClaudeCode, p, core.SessionActive, "interim progress"))
			}
			require.Empty(t, e.end())
			require.Equal(t, rtHarvest, e.session(amb.ID).Findings)
			ended := e.endedEvents(amb.ID)
			require.Len(t, ended, 1)
			require.Equal(t, true, ended[0].Payload["harvested"])
			for _, l := range links {
				require.Equal(t, "interim progress", e.session(l.ID).Findings, "an explicit session keeps its own findings")
			}
			require.Equal(t, map[string]int{tc.home: 1}, e.whereIs(rtSecret)["events"])
		})
	}
}

// Plan and subagent captures follow the binding too, with their events and the
// plan's tracking metadata: the note lands in the isolated project, its events
// are filed under the session there, and the ambient row in the open project
// learns nothing of the plan. They stay there after a rebind to an open
// project.
func TestFencedRoute_CapturesFollowTheBinding(t *testing.T) {
	e := newRouteEnv(t)
	ctx := context.Background()
	amb := e.start(ClientClaudeCode, rtOpsCWD)
	link := e.bind(ClientClaudeCode, rtFin, core.SessionActive, "")

	plan := filepath.Join(e.plansDir, "quiet-ledger.md")
	require.NoError(t, os.WriteFile(plan, []byte("# Example ledger review\n\n1. "+rtSecret+" step one.\n"), 0o644))
	out, refused := e.hook(ClientClaudeCode, "post-tool-use", map[string]any{
		"session_id": rtConv, "cwd": rtOpsCWD, "tool_name": "Write", "tool_input": map[string]any{"file_path": plan},
	})
	require.Empty(t, refused)
	require.Contains(t, additionalContext(t, out), "composition plan:example-ledger-review")

	_, inOps, err := store.NoteBySlug(ctx, e.db, rtOps, plans.NotePrefix+"quiet-ledger")
	require.NoError(t, err)
	require.False(t, inOps, "nothing lands in the cwd's open project")
	idx, ok, err := store.NoteBySlug(ctx, e.db, rtFin, plans.NotePrefix+"quiet-ledger")
	require.NoError(t, err)
	require.True(t, ok, "the plan lands in the project the conversation bound itself to")
	captured := eventsOfKind(t, e.rec, core.EventPlanCaptured)
	require.Len(t, captured, 1)
	require.Equal(t, link.ID, captured[0].SessionID)
	require.Equal(t, rtFin, captured[0].ProjectSlug)
	require.Equal(t, idx.ID, captured[0].ItemID)

	// The plan's tracking metadata is the link's, never the ambient row's.
	_, onAmbient := e.session(amb.ID).Metadata["plan_capture"]
	require.False(t, onAmbient)
	meta, onLink := e.session(link.ID).Metadata["plan_capture"].(map[string]any)
	require.True(t, onLink)
	require.Equal(t, "example-ledger-review", meta["plan_slug"])

	// A planning subagent's report.
	main := placeSubagentTranscript(t, "ag01")
	_, refused = e.hook(ClientClaudeCode, "subagent-stop", map[string]any{
		"session_id": rtConv, "cwd": rtOpsCWD, "permission_mode": "plan",
		"transcript_path": main, "agent_id": "ag01", "agent_type": "Explore",
	})
	require.Empty(t, refused)
	_, ok, err = store.NoteBySlug(ctx, e.db, rtFin, plans.AgentNotePrefix+"ag01")
	require.NoError(t, err)
	require.True(t, ok)
	_, ok, err = store.NoteBySlug(ctx, e.db, rtOps, plans.AgentNotePrefix+"ag01")
	require.NoError(t, err)
	require.False(t, ok)
	subs := eventsOfKind(t, e.rec, core.EventSubagentCaptured)
	require.Len(t, subs, 1)
	require.Equal(t, link.ID, subs[0].SessionID)
	require.Equal(t, rtFin, subs[0].ProjectSlug)

	// A rebind to an open project, then the approval: still the isolated
	// project, with its tracking task.
	e.bind(ClientClaudeCode, rtDocs, core.SessionActive, "")
	_, refused = e.hook(ClientClaudeCode, "post-tool-use", map[string]any{
		"session_id": rtConv, "cwd": rtOpsCWD, "tool_name": "ExitPlanMode",
		"tool_response": map[string]any{"filePath": plan},
	})
	require.Empty(t, refused)
	n, err := e.mgr.Store().ReadNote(idx.FilePath)
	require.NoError(t, err)
	require.Equal(t, plans.StatusApproved, plans.StatusFromTags(n.Tags))
	approved := eventsOfKind(t, e.rec, core.EventPlanApproved)
	require.Len(t, approved, 1)
	require.Equal(t, rtFin, approved[0].ProjectSlug)
	tasks, err := store.ListTasksForPlan(ctx, e.db, rtFin, "", "example-ledger-review")
	require.NoError(t, err)
	require.Len(t, tasks, 1, "the tracking task is the isolated project's")
	ops, err := store.ListTasksForPlan(ctx, e.db, rtOps, "", "example-ledger-review")
	require.NoError(t, err)
	require.Empty(t, ops)
	transition := eventsOfKind(t, e.rec, core.EventTaskTransition)
	require.Len(t, transition, 1)
	require.Equal(t, link.ID, transition[0].SessionID)

	// Nothing of the plan's content reached the open projects.
	where := e.whereIs(rtSecret)
	require.NotContains(t, where["events"], rtOps)
	require.NotContains(t, where["events"], rtDocs)
	require.NotContains(t, where["sessions"], rtOps)
}

// A conversation bound to two isolated projects records no capture in any of
// them: the plan and the subagent report are dropped with the notice.
func TestFencedRoute_CapturesWithheldWithANotice(t *testing.T) {
	e := newRouteEnv(t)
	ctx := context.Background()
	e.start(ClientClaudeCode, rtOpsCWD)
	e.bind(ClientClaudeCode, rtFin, core.SessionActive, "")
	e.bind(ClientClaudeCode, rtLegal, core.SessionCompleted, "Read the example contract.")
	why := "this conversation was bound to more than one isolated project (" + rtFin + ", " + rtLegal +
		"), and its record goes to none of them."

	plan := filepath.Join(e.plansDir, "quiet-ledger.md")
	require.NoError(t, os.WriteFile(plan, []byte("# Example ledger review\n\n"+rtSecret+".\n"), 0o644))
	out, refused := e.hook(ClientClaudeCode, "post-tool-use", map[string]any{
		"session_id": rtConv, "cwd": rtOpsCWD, "tool_name": "Write", "tool_input": map[string]any{"file_path": plan},
	})
	require.Equal(t, "Seamless did not record this plan: "+why, refused)
	require.Equal(t, "<seam-plan-context>\n"+refused+"\n</seam-plan-context>", additionalContext(t, out))

	_, refused = e.hook(ClientClaudeCode, "subagent-stop", map[string]any{
		"session_id": rtConv, "cwd": rtOpsCWD, "permission_mode": "plan",
		"transcript_path": placeSubagentTranscript(t, "ag02"), "agent_id": "ag02", "agent_type": "Explore",
	})
	require.Equal(t, "Seamless did not record this subagent's report: "+why, refused)

	for _, project := range []string{rtOps, rtFin, rtLegal} {
		for _, slug := range []string{plans.NotePrefix + "quiet-ledger", plans.AgentNotePrefix + "ag02"} {
			_, ok, err := store.NoteBySlug(ctx, e.db, project, slug)
			require.NoError(t, err)
			require.False(t, ok, "%s/%s", project, slug)
		}
	}
	require.Empty(t, eventsOfKind(t, e.rec, core.EventPlanCaptured))
	require.Empty(t, eventsOfKind(t, e.rec, core.EventSubagentCaptured))
}

// A conversation native to an isolated project keeps its captures at home,
// whatever other isolated projects it binds itself to: the plan and the
// subagent report land in its own project, filed under its ambient row, with
// no notice.
func TestFencedRoute_NativeHomeKeepsCaptures(t *testing.T) {
	e := newRouteEnv(t)
	ctx := context.Background()
	amb := e.start(ClientClaudeCode, rtFinCWD)
	require.Equal(t, rtFin, amb.ProjectSlug)
	e.bind(ClientClaudeCode, rtLegal, core.SessionActive, "")
	e.bind(ClientClaudeCode, rtSealed, core.SessionCompleted, "")

	plan := filepath.Join(e.plansDir, "quiet-ledger.md")
	require.NoError(t, os.WriteFile(plan, []byte("# Example ledger review\n\n1. "+rtSecret+" step one.\n"), 0o644))
	out, refused := e.hook(ClientClaudeCode, "post-tool-use", map[string]any{
		"session_id": rtConv, "cwd": rtFinCWD, "tool_name": "Write", "tool_input": map[string]any{"file_path": plan},
	})
	require.Empty(t, refused)
	require.Contains(t, additionalContext(t, out), "composition plan:example-ledger-review")
	_, refused = e.hook(ClientClaudeCode, "subagent-stop", map[string]any{
		"session_id": rtConv, "cwd": rtFinCWD, "permission_mode": "plan",
		"transcript_path": placeSubagentTranscript(t, "ag03"), "agent_id": "ag03", "agent_type": "Explore",
	})
	require.Empty(t, refused)

	for _, project := range []string{rtFin, rtLegal, rtSealed, rtOps} {
		for _, slug := range []string{plans.NotePrefix + "quiet-ledger", plans.AgentNotePrefix + "ag03"} {
			_, ok, err := store.NoteBySlug(ctx, e.db, project, slug)
			require.NoError(t, err)
			require.Equal(t, project == rtFin, ok, "%s/%s", project, slug)
		}
	}
	for _, kind := range []core.EventKind{core.EventPlanCaptured, core.EventSubagentCaptured} {
		evs := eventsOfKind(t, e.rec, kind)
		require.Len(t, evs, 1, kind)
		require.Equal(t, amb.ID, evs[0].SessionID, kind)
		require.Equal(t, rtFin, evs[0].ProjectSlug, kind)
	}
	_, onAmbient := e.session(amb.ID).Metadata["plan_capture"]
	require.True(t, onAmbient, "the plan's tracking metadata is the home session's")
}

// Codex has no SessionEnd: each Stop's findings are provisional, and they go
// where the conversation's record goes, every turn, while the ambient row keeps
// the pointer. Findings the agent wrote on the link itself are never replaced.
func TestFencedRoute_CodexStopFollowsTheBinding(t *testing.T) {
	stop := func(e *routeEnv, msg string) string {
		_, refused := e.hook(ClientCodex, "stop", map[string]any{
			"session_id": rtConv, "cwd": rtOpsCWD, "last_assistant_message": msg,
		})
		return refused
	}

	t.Run("routed", func(t *testing.T) {
		e := newRouteEnv(t)
		amb := e.start(ClientCodex, rtOpsCWD)
		require.Empty(t, stop(e, "Opened the repository."))
		require.Equal(t, "(auto-harvested) Opened the repository.", e.session(amb.ID).Findings, "before any binding, as always")

		link := e.bind(ClientCodex, rtFin, core.SessionActive, "")
		require.Empty(t, stop(e, rtSecret+" turn one."))
		require.Equal(t, "(auto-harvested) "+rtSecret+" turn one.", e.session(link.ID).Findings)
		require.Equal(t, "Continued in project "+rtFin+"; this conversation's findings go there, not here.",
			e.session(amb.ID).Findings)
		require.Empty(t, stop(e, rtSecret+" turn two."))
		require.Equal(t, "(auto-harvested) "+rtSecret+" turn two.", e.session(link.ID).Findings)

		got := e.session(link.ID)
		got.Findings = "The agent's own summary."
		require.NoError(t, store.UpdateSession(context.Background(), e.db, got))
		require.Empty(t, stop(e, rtSecret+" turn three."))
		require.Equal(t, "The agent's own summary.", e.session(link.ID).Findings)

		require.Equal(t, map[string]map[string]int{"events": {}, "sessions": {}, "fts": {}}, e.whereIs(rtSecret))
		require.Equal(t, map[string]int{rtOps: 1}, e.whereIs("Continued in project")["fts"])
	})

	t.Run("withheld", func(t *testing.T) {
		e := newRouteEnv(t)
		amb := e.start(ClientCodex, rtOpsCWD)
		fin := e.bind(ClientCodex, rtFin, core.SessionActive, "")
		e.bind(ClientCodex, rtSealed, core.SessionActive, "")
		require.Equal(t, "Seamless did not record this turn's findings: this conversation was bound to more than one "+
			"isolated project ("+rtFin+", "+rtSealed+"), and its record goes to none of them.", stop(e, rtSecret+" turn."))
		line := "Not recorded: this conversation was bound to more than one isolated project (" +
			rtFin + ", " + rtSealed + "), and its record goes to none of them."
		require.Equal(t, line, e.session(amb.ID).Findings)
		require.Equal(t, line, e.session(fin.ID).Findings)
		require.Equal(t, map[string]map[string]int{"events": {}, "sessions": {}, "fts": {}}, e.whereIs(rtSecret))
	})

	t.Run("native home keeps it", func(t *testing.T) {
		e := newRouteEnv(t)
		amb := e.start(ClientCodex, rtFinCWD)
		require.Equal(t, rtFin, amb.ProjectSlug)
		legal := e.bind(ClientCodex, rtLegal, core.SessionActive, "")
		sealed := e.bind(ClientCodex, rtSealed, core.SessionActive, "")
		_, refused := e.hook(ClientCodex, "stop", map[string]any{
			"session_id": rtConv, "cwd": rtFinCWD, "last_assistant_message": rtSecret + " turn.",
		})
		require.Empty(t, refused)
		require.Equal(t, "(auto-harvested) "+rtSecret+" turn.", e.session(amb.ID).Findings)
		require.Empty(t, e.session(legal.ID).Findings)
		require.Empty(t, e.session(sealed.ID).Findings)
		require.Equal(t, map[string]int{rtFin: 1}, e.whereIs(rtSecret)["sessions"])
	})
}

// pushLink links an explicit session in project to the pushed conversation
// ext, as session_start does from the agent's own process.
func pushLink(t *testing.T, e *pushHooks, ext, project string, status core.SessionStatus) core.Session {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	now := e.clock()
	s := core.Session{
		ID: id, Name: "sess/" + id, ProjectSlug: project, Status: status, Source: "explicit",
		ExternalSessionID: ext, ExternalClient: ClientClaudeCode.externalIdentity(), CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, store.CreateSession(context.Background(), e.db, s))
	return s
}

// Under peer_auth the client pushes what it read, and the push lands where the
// conversation's record goes too: the plan, the subagent report and the end's
// findings in the isolated project the conversation bound itself to, the
// pointer on the ambient row, whose token totals stay with it.
func TestFencedRoute_PushedCapturesFollowTheBinding(t *testing.T) {
	e := newPushHooks(t)
	ctx := context.Background()
	_, err := store.EnsureProject(ctx, e.db, rtLegal, rtLegal)
	require.NoError(t, err)
	require.NoError(t, store.SetProjectIsolation(ctx, e.db, rtLegal, core.IsolationConfidential))

	e.send("session-start", "", map[string]any{"session_id": "push-1", "cwd": "/work/ops", "source": "startup"}, nil)
	amb := requireAmbientSession(t, e.db, ClientClaudeCode, "push-1")
	require.Equal(t, lkOps, amb.ProjectSlug)
	link := pushLink(t, e, "push-1", rtLegal, core.SessionActive)

	ctxText, refused := e.send("post-tool-use", "", map[string]any{"session_id": "push-1", "cwd": "/work/ops",
		"tool_name": "Write", "tool_input": map[string]any{"file_path": "/home/owner/.claude/plans/example-plan.md"}},
		planPush("# Legal plan\n\n"+rtSecret+" clause."))
	require.Empty(t, refused)
	require.Contains(t, ctxText, "composition plan:legal-plan")
	n, ok := e.note(rtLegal, plans.NotePrefix+"example-plan")
	require.True(t, ok)
	require.Contains(t, n.Body, rtSecret+" clause.")
	_, ok = e.note(lkOps, plans.NotePrefix+"example-plan")
	require.False(t, ok)

	_, refused = e.send("subagent-stop", "", map[string]any{"session_id": "push-1", "cwd": "/work/ops",
		"agent_id": "a9", "agent_type": "Explore", "permission_mode": "plan"},
		map[string]any{"version": 1, "prompt": "Read the example clause", "report": rtSecret + " report."})
	require.Empty(t, refused)
	_, ok = e.note(rtLegal, plans.AgentNotePrefix+"a9")
	require.True(t, ok)
	_, ok = e.note(lkOps, plans.AgentNotePrefix+"a9")
	require.False(t, ok)

	_, refused = e.send("session-end", "", map[string]any{"session_id": "push-1"},
		map[string]any{"version": 1, "findings": "(auto-harvested) " + rtSecret + " pushed.", "tokens": pushTokens})
	require.Empty(t, refused)
	got, _, err := store.SessionByID(ctx, e.db, link.ID)
	require.NoError(t, err)
	require.Equal(t, core.SessionCompleted, got.Status)
	require.Equal(t, "(auto-harvested) "+rtSecret+" pushed.", got.Findings)
	amb = requireAmbientSession(t, e.db, ClientClaudeCode, "push-1")
	require.Equal(t, "Continued in project "+rtLegal+"; this conversation's findings go there, not here.", amb.Findings)
	require.Equal(t, 130, amb.Tokens.Total, "token totals are metadata: the ambient row keeps them")

	rows, err := e.db.QueryContext(ctx,
		`SELECT DISTINCT project_slug FROM events WHERE payload LIKE ?`, "%"+rtSecret+"%")
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var where []string
	for rows.Next() {
		var p string
		require.NoError(t, rows.Scan(&p))
		where = append(where, p)
	}
	require.NoError(t, rows.Err())
	require.Equal(t, []string{rtLegal}, where, "the pushed content is filed under the isolated project only")
}

// A conversation whose record goes to a project locked to the caller -- it
// bound itself there under a lease that has since ended -- records nothing
// there and nothing in its cwd's project: each push is refused with the lock's
// notice, and the ambient row keeps only the pointer.
func TestFencedRoute_PushedIntoALockedProjectRefused(t *testing.T) {
	e := newPushHooks(t)
	e.send("session-start", "", map[string]any{"session_id": "push-2", "cwd": "/work/ops", "source": "startup"}, nil)
	link := pushLink(t, e, "push-2", lkFin, core.SessionExpired)
	locked := func(what string) string {
		return "Seamless did not record " + what + ": project " + lkFin + " is locked and this session holds no " +
			"approval; the next capture after the owner approves carries the current state " +
			"(to ask: `seam access request " + lkFin + "`)."
	}

	_, refused := e.send("post-tool-use", "", map[string]any{"session_id": "push-2", "cwd": "/work/ops",
		"tool_name": "Write", "tool_input": map[string]any{"file_path": "/home/owner/.claude/plans/example-plan.md"}},
		planPush("# Fin plan\n\n"+rtSecret+"."))
	require.Equal(t, locked("this plan"), refused)
	for _, project := range []string{lkFin, lkOps} {
		_, ok := e.note(project, plans.NotePrefix+"example-plan")
		require.False(t, ok, project)
	}

	_, refused = e.send("session-end", "", map[string]any{"session_id": "push-2"},
		map[string]any{"version": 1, "findings": "(auto-harvested) " + rtSecret + "."})
	require.Equal(t, locked("this conversation's findings"), refused)
	got, _, err := store.SessionByID(context.Background(), e.db, link.ID)
	require.NoError(t, err)
	require.Equal(t, core.SessionExpired, got.Status)
	require.Empty(t, got.Findings)
	require.Equal(t, "Continued in project "+lkFin+"; this conversation's findings go there, not here.",
		requireAmbientSession(t, e.db, ClientClaudeCode, "push-2").Findings)
}

// With isolation.requests on, the record routing obeys the clearance rule as
// a capture's own target does: a conversation in an open project holding a
// read grant of one confidential project that binds itself to another
// records its harvest and captures in neither -- no project is cleared for
// both projects' content, and the open one for neither.
func TestFencedRoute_ReadGrantConfinementHolds(t *testing.T) {
	e := newRouteEnvWith(t, func(c *Config) { c.ReadGrants = true })
	ctx := context.Background()
	amb := e.start(ClientClaudeCode, rtOpsCWD)
	approveGrant(t, &captureEnv{db: e.db}, rtOps, rtLegal, amb.ID, "")
	link := e.bind(ClientClaudeCode, rtFin, core.SessionActive, "")
	confined := func(what string) string {
		return "Seamless did not record " + what + ": this session holds content from " + rtFin + ", " + rtLegal +
			", and no project is cleared for all of it: this would land in project " + rtFin + "."
	}

	plan := filepath.Join(e.plansDir, "quiet-ledger.md")
	require.NoError(t, os.WriteFile(plan, []byte("# Example ledger review\n\n"+rtSecret+".\n"), 0o644))
	_, refused := e.hook(ClientClaudeCode, "post-tool-use", map[string]any{
		"session_id": rtConv, "cwd": rtOpsCWD, "tool_name": "Write", "tool_input": map[string]any{"file_path": plan},
	})
	require.Equal(t, confined("this plan"), refused)
	for _, project := range []string{rtOps, rtFin} {
		_, ok, err := store.NoteBySlug(ctx, e.db, project, plans.NotePrefix+"quiet-ledger")
		require.NoError(t, err)
		require.False(t, ok, project)
	}

	require.Equal(t, confined("this conversation's findings"), e.end())
	require.Empty(t, e.session(link.ID).Findings)
	require.Equal(t, core.SessionCompleted, e.session(link.ID).Status)
	require.Equal(t, "Continued in project "+rtFin+"; this conversation's findings go there, not here.",
		e.session(amb.ID).Findings)
	require.Equal(t, map[string]map[string]int{"events": {}, "sessions": {}, "fts": {}}, e.whereIs(rtSecret))
}

// The pushed captures of a conversation bound to two isolated projects land in
// none of them either, each refused with the notice.
func TestFencedRoute_PushedCapturesWithheld(t *testing.T) {
	e := newPushHooks(t)
	ctx := context.Background()
	_, err := store.EnsureProject(ctx, e.db, rtLegal, rtLegal)
	require.NoError(t, err)
	require.NoError(t, store.SetProjectIsolation(ctx, e.db, rtLegal, core.IsolationConfidential))
	e.send("session-start", "", map[string]any{"session_id": "push-3", "cwd": "/work/ops", "source": "startup"}, nil)
	pushLink(t, e, "push-3", rtLegal, core.SessionCompleted)
	pushLink(t, e, "push-3", lkFin, core.SessionExpired)
	why := "this conversation was bound to more than one isolated project (" + lkFin + ", " + rtLegal +
		"), and its record goes to none of them."

	_, refused := e.send("post-tool-use", "", map[string]any{"session_id": "push-3", "cwd": "/work/ops",
		"tool_name": "Write", "tool_input": map[string]any{"file_path": "/home/owner/.claude/plans/example-plan.md"}},
		planPush("# Legal plan\n\n"+rtSecret+"."))
	require.Equal(t, "Seamless did not record this plan: "+why, refused)
	_, refused = e.send("subagent-stop", "", map[string]any{"session_id": "push-3", "cwd": "/work/ops",
		"agent_id": "a7", "agent_type": "Explore", "permission_mode": "plan"},
		map[string]any{"version": 1, "prompt": "p", "report": rtSecret + "."})
	require.Equal(t, "Seamless did not record this subagent's report: "+why, refused)
	_, refused = e.send("session-end", "", map[string]any{"session_id": "push-3"},
		map[string]any{"version": 1, "findings": "(auto-harvested) " + rtSecret + "."})
	require.Equal(t, "Seamless did not record this conversation's findings: "+why, refused)

	for _, project := range []string{lkOps, lkFin, rtLegal} {
		for _, slug := range []string{plans.NotePrefix + "example-plan", plans.AgentNotePrefix + "a7"} {
			_, ok := e.note(project, slug)
			require.False(t, ok, "%s/%s", project, slug)
		}
	}
	require.Equal(t, "Not recorded: "+why, requireAmbientSession(t, e.db, ClientClaudeCode, "push-3").Findings)
	rows, err := e.db.QueryContext(ctx, `SELECT count(*) FROM events WHERE payload LIKE ?`, "%"+rtSecret+"%")
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	require.True(t, rows.Next())
	var n int
	require.NoError(t, rows.Scan(&n))
	require.NoError(t, rows.Err())
	require.Zero(t, n)
}

// Under peer_auth, a conversation native to a locked confidential project
// that the owner approved keeps its pushed record at home, whatever other
// isolated project it binds itself to: the plan lands in its own project and
// the end's findings on its ambient row, with no notice.
func TestFencedRoute_PushedNativeHomeKeepsItsRecord(t *testing.T) {
	e := newPushHooks(t)
	ctx := context.Background()
	_, err := store.EnsureProject(ctx, e.db, rtLegal, rtLegal)
	require.NoError(t, err)
	require.NoError(t, store.SetProjectIsolation(ctx, e.db, rtLegal, core.IsolationConfidential))
	e.send("session-start", "", map[string]any{"session_id": "push-4", "cwd": "/work/finance", "source": "startup"}, nil)
	amb := requireAmbientSession(t, e.db, ClientClaudeCode, "push-4")
	require.Equal(t, lkFin, amb.ProjectSlug)
	e.grant(amb.ID, lkFin, time.Hour)
	legal := pushLink(t, e, "push-4", rtLegal, core.SessionActive)

	ctxText, refused := e.send("post-tool-use", "", map[string]any{"session_id": "push-4", "cwd": "/work/finance",
		"tool_name": "Write", "tool_input": map[string]any{"file_path": "/home/owner/.claude/plans/fin-plan.md"}},
		map[string]any{"version": 1, "plan": map[string]any{"basename": "fin-plan", "content": "# Fin plan\n\n" + rtSecret + "."}})
	require.Empty(t, refused)
	require.Contains(t, ctxText, "composition plan:fin-plan")
	_, ok := e.note(lkFin, plans.NotePrefix+"fin-plan")
	require.True(t, ok, "the plan lands at home")
	_, ok = e.note(rtLegal, plans.NotePrefix+"fin-plan")
	require.False(t, ok)

	_, refused = e.send("session-end", "", map[string]any{"session_id": "push-4"},
		map[string]any{"version": 1, "findings": "(auto-harvested) " + rtSecret + "."})
	require.Empty(t, refused)
	require.Equal(t, "(auto-harvested) "+rtSecret+".", requireAmbientSession(t, e.db, ClientClaudeCode, "push-4").Findings)
	got, ok, err := store.SessionByID(ctx, e.db, legal.ID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, core.SessionCompleted, got.Status)
	require.Empty(t, got.Findings, "the link keeps its own (none)")
}

// The harvest replaces only findings the hooks wrote: none yet, the no-summary
// sentinel, or an earlier auto-harvest. Anything else is the agent's own, and
// so are the metadata lines, which only ever land where no harvest goes.
func TestFencedRoute_HookFindings(t *testing.T) {
	for _, tc := range []struct {
		findings string
		hooks    bool
	}{
		{"", true},
		{"  \n", true},
		{core.FindingNoSummary, true},
		{harvest.Prefix + "Shipped the ready-queue.", true},
		{"Closed the example ledger.", false},
		{"auto-harvested: not the prefix", false},
		{routedPointer(rtFin), false},
		{withheldLine([]string{rtFin, rtLegal}), false},
	} {
		require.Equal(t, tc.hooks, hookFindings(tc.findings), "%q", tc.findings)
	}
}
