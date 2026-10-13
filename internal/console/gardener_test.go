package console

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/gardener"
	"github.com/arctop/seamless/internal/llm"
	"github.com/arctop/seamless/internal/store"
)

// stubChat is a canned llm.Chat: it returns a fixed completion regardless of the
// prompt, so the console can exercise the natural-language request path.
type stubChat struct{ out string }

func (c stubChat) Model() string                                            { return "stub-chat" }
func (c stubChat) Complete(context.Context, string, string) (string, error) { return c.out, nil }

// seqChat is an llm.Chat returning one canned completion per call in order (the
// last one repeats), so a flow chaining two interpretations -- a request that
// routes into a split plan -- can stub both.
type seqChat struct {
	outs []string
	n    int
}

func (c *seqChat) Model() string { return "stub-chat-seq" }
func (c *seqChat) Complete(context.Context, string, string) (string, error) {
	out := c.outs[min(c.n, len(c.outs)-1)]
	c.n++
	return out, nil
}

// newConsoleWithGardener wires a real gardener (no embedder/chat) so apply/dismiss
// round-trip through the proposal store and files.
func newConsoleWithGardener(t *testing.T) (context.Context, *sql.DB, *files.Manager, *http.ServeMux) {
	t.Helper()
	dir := t.TempDir()
	db, err := openFreshDB(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	dataDir := filepath.Join(dir, "data")
	mgr, err := files.NewManager(dataDir, db, slog.Default())
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })
	rec := events.NewRecorder(db)
	garden := gardener.New(db, mgr, nil, nil, rec, gardener.FromConfig(config.Gardener{}), slog.Default())

	svc, err := New(Config{DB: db, Files: mgr, Gardener: garden, Events: rec, DataDir: dataDir, APIKey: testKey, Level: "advanced"})
	require.NoError(t, err)
	mux := http.NewServeMux()
	svc.Register(mux)
	return context.Background(), db, mgr, mux
}

func post(mux *http.ServeMux, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	return do(mux, req)
}

func TestProposalPresentation_CoversCanonicalKinds(t *testing.T) {
	for _, kind := range store.ProposalKinds {
		t.Run(kind, func(t *testing.T) {
			label, eyebrow, iconName, tone := proposalPresentation(kind)
			require.NotEqual(t, "Review a knowledge change", label, "a canonical kind must not fall through to generic review copy")
			require.NotEmpty(t, eyebrow)
			require.NotEmpty(t, iconName)
			require.NotEmpty(t, tone)
		})
	}
}

func TestGardenerPage_PolishContracts(t *testing.T) {
	source, err := templateFS.ReadFile("templates/gardener.html")
	require.NoError(t, err)
	require.Contains(t, string(source), `class="gardener-contract-copy"`,
		"the contract explanation must wrap independently of its badge")

	_, mux := newConsole(t)
	css := getPeek(t, mux, "/console/static/console.css").Body.String()
	require.Contains(t, css, ".lib-page.gardener-page { --lib-accent: var(--ok); }",
		"the Gardener accent selector must outrank the later shared library default")
	require.Contains(t, css, "grid-template-columns: minmax(0, min(100%, var(--content-max)));",
		"the centered content track must shrink inside the usable viewport")
	require.Contains(t, css, ".mv2-sort .seg { max-width: 100%; overflow-x: auto; }",
		"the compact bar's filters must not force the page wider than the viewport")
}

func TestGardenerPage_CardsDismissApply(t *testing.T) {
	ctx, db, _, mux := newConsoleWithGardener(t)

	archiveP, err := store.CreateProposal(ctx, db, store.ProposalArchive, map[string]any{
		"key": "archive:MISSING", "id": "MISSINGMEM", "name": "old-note",
		"project": "seamless", "kind": "reference", "description": "went stale", "reason": "no activity in 90d",
	})
	require.NoError(t, err)
	mergeP, err := store.CreateProposal(ctx, db, store.ProposalMerge, map[string]any{
		"key": "merge:a|b", "score": 0.91,
		"keep": map[string]any{"id": "K", "name": "keep-me", "project": "seamless", "kind": "gotcha", "description": "newer"},
		"drop": map[string]any{"id": "D", "name": "drop-me", "project": "seamless", "kind": "gotcha", "description": "older"},
	})
	require.NoError(t, err)
	digestP, err := store.CreateProposal(ctx, db, store.ProposalDigest, map[string]any{
		"key": "digest:seamless:2026-07", "project": "seamless", "month": "2026-07",
		"session_count": 4.0, "title": "seamless digest 2026-07", "body": "## Findings\n- did the thing",
	})
	require.NoError(t, err)
	wantedP, err := store.CreateProposal(ctx, db, store.ProposalMemoryWanted, map[string]any{
		"key": "memory_wanted:seamless:boot-chroma-race", "project": "seamless",
		"signature":  "boot-chroma-race",
		"queries":    []string{"chroma boot race", "race during chroma boot"},
		"miss_count": 3.0, "session_count": 2.0,
		"suggested_title": "chroma boot race",
		"reason":          "knowledge gap: recall found nothing 3x across 2 sessions in 14d",
	})
	require.NoError(t, err)
	toolErrP, err := store.CreateProposal(ctx, db, store.ProposalToolError, map[string]any{
		"key": "tool_error:seamless:abcd1234abcd1234", "project": "seamless",
		"surface": "tool", "name": "tasks_add", "signature": "unknown parameter <v>",
		"examples":    []string{`tasks_add: unknown parameter "titel"`},
		"error_count": 3.0, "session_count": 2.0,
		"suggested_title": "tasks_add: unknown parameter <v>",
		"reason":          "recurring tool error: tasks_add returned this error 3x across 2 sessions in 14d",
	})
	require.NoError(t, err)

	// Cards render, one per kind.
	var data gardenerData
	getJSON(t, mux, "/console/gardener?format=json", &data)
	require.Len(t, data.Cards, 5)
	require.Equal(t, 5, data.PendingCount)
	require.Zero(t, data.RequestedCount)
	require.Equal(t, 5, data.BackgroundCount)
	byKind := map[string]proposalCard{}
	for _, c := range data.Cards {
		byKind[c.Kind] = c
	}
	require.Equal(t, "keep-me", byKind["merge"].Keep.Name)
	require.InDelta(t, 0.91, byKind["merge"].Score, 0.001)
	require.Equal(t, 91, byKind["merge"].ScorePercent)
	require.Equal(t, "Merge duplicate memories", byKind["merge"].Label)
	require.Equal(t, "git-merge", byKind["merge"].Icon)
	require.Equal(t, 4, byKind["digest"].SessionCount)
	require.Equal(t, "old-note", byKind["archive"].Archive.Name)
	require.Equal(t, "Write a missing memory", byKind["memory_wanted"].Label)
	require.Equal(t, "search", byKind["memory_wanted"].Icon)
	require.Equal(t, []string{"chroma boot race", "race during chroma boot"}, byKind["memory_wanted"].MemoryWanted.Queries)
	require.Equal(t, 3, byKind["memory_wanted"].MemoryWanted.MissCount)
	require.Equal(t, 2, byKind["memory_wanted"].MemoryWanted.SessionCount)
	require.Equal(t, "chroma boot race", byKind["memory_wanted"].MemoryWanted.SuggestedTitle)
	require.Equal(t, "Fix a recurring error", byKind["tool_error"].Label)
	require.Equal(t, "triangle-alert", byKind["tool_error"].Icon)
	require.Equal(t, "danger", byKind["tool_error"].Tone)
	require.Equal(t, "tool", byKind["tool_error"].ToolError.Surface)
	require.Equal(t, "tasks_add", byKind["tool_error"].ToolError.Key)
	require.Equal(t, "unknown parameter <v>", byKind["tool_error"].ToolError.Signature)
	require.Equal(t, []string{`tasks_add: unknown parameter "titel"`}, byKind["tool_error"].ToolError.Examples)
	require.Equal(t, 3, byKind["tool_error"].ToolError.ErrorCount)
	require.Equal(t, 2, byKind["tool_error"].ToolError.SessionCount)
	require.Equal(t, "tasks_add: unknown parameter <v>", byKind["tool_error"].ToolError.SuggestedTitle)

	// The HTML is a two-stage trust surface: the propose-only contract is
	// explicit above the composer, and the selected proposal exposes its effects
	// before the Apply gate.
	req := httptest.NewRequest(http.MethodGet, "/console/gardener", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	body := do(mux, req).Body.String()
	require.Contains(t, body, "Propose-only by design.")
	require.Contains(t, body, `id="gardener-ask-title"`)
	require.Contains(t, body, `id="gardener-review-title"`)
	require.Contains(t, body, "Found by a background Gardener pass")
	require.Contains(t, body, "Dismiss suggestion")
	require.Contains(t, body, "Apply change")
	require.Contains(t, body, `data-base="/console/gardener"`, "the queue uses the shared library reader")
	require.Contains(t, body, `id="ri-`+archiveP.ID+`"`, "every pending proposal has a rail row")

	// Dismiss the merge -> gone from pending.
	require.Equal(t, http.StatusSeeOther, post(mux, "/console/gardener/"+mergeP.ID+"/dismiss").Code)
	p, ok, err := store.ProposalByID(ctx, db, mergeP.ID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, store.ProposalDismissed, p.Status)

	// Apply the digest -> a note is written and the proposal is applied.
	require.Equal(t, http.StatusSeeOther, post(mux, "/console/gardener/"+digestP.ID+"/apply").Code)
	p, _, err = store.ProposalByID(ctx, db, digestP.ID)
	require.NoError(t, err)
	require.Equal(t, store.ProposalApplied, p.Status)

	// Apply the memory-wanted -> an open task appears and the proposal is applied.
	require.Equal(t, http.StatusSeeOther, post(mux, "/console/gardener/"+wantedP.ID+"/apply").Code)
	p, _, err = store.ProposalByID(ctx, db, wantedP.ID)
	require.NoError(t, err)
	require.Equal(t, store.ProposalApplied, p.Status)
	tasks, err := store.ListTasks(ctx, db, "seamless", core.TaskOpen)
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	require.Equal(t, "Write a memory: chroma boot race", tasks[0].Title)
	require.Equal(t, "gardener", tasks[0].CreatedBy)

	// Apply the tool-error -> a fix task appears and the proposal is applied.
	require.Equal(t, http.StatusSeeOther, post(mux, "/console/gardener/"+toolErrP.ID+"/apply").Code)
	p, _, err = store.ProposalByID(ctx, db, toolErrP.ID)
	require.NoError(t, err)
	require.Equal(t, store.ProposalApplied, p.Status)
	tasks, err = store.ListTasks(ctx, db, "seamless", core.TaskOpen)
	require.NoError(t, err)
	require.Len(t, tasks, 2)
	titles := []string{tasks[0].Title, tasks[1].Title}
	require.Contains(t, titles, "Fix recurring error: tasks_add: unknown parameter <v>")

	// Apply the archive whose memory is missing -> flash error, proposal stays
	// pending and stays selected (the owner still has to deal with it).
	rr := post(mux, "/console/gardener/"+archiveP.ID+"/apply")
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.True(t, strings.HasPrefix(rr.Header().Get("Location"), "/console/gardener/"+archiveP.ID+"?error="),
		"a failed apply keeps the proposal selected, got %q", rr.Header().Get("Location"))
	p, _, err = store.ProposalByID(ctx, db, archiveP.ID)
	require.NoError(t, err)
	require.Equal(t, store.ProposalPending, p.Status)
}

// TestGardenerPage_RekindCardAndApply round-trips a rekind proposal: the card
// projects the from/to kinds, and applying it reclassifies the memory in place.
func TestGardenerPage_RekindCardAndApply(t *testing.T) {
	ctx, db, mgr, mux := newConsoleWithGardener(t)

	id, err := core.NewID()
	require.NoError(t, err)
	now := time.Now().UTC()
	_, err = mgr.WriteMemory(ctx, core.Memory{
		ID: id, Kind: core.KindConstraint, Name: "wordmark-rule", Description: "branding fact",
		Project: "seamless", Body: "body", Created: now, Updated: now, ValidFrom: now,
	})
	require.NoError(t, err)

	p, err := store.CreateProposal(ctx, db, store.ProposalRekind, map[string]any{
		"key": "rekind:" + id + ":convention", "id": id, "name": "wordmark-rule",
		"project": "seamless", "from": "constraint", "to": "convention",
		"rationale": "project-local branding, not a systemic rule",
	})
	require.NoError(t, err)

	var data gardenerData
	getJSON(t, mux, "/console/gardener?format=json", &data)
	require.Len(t, data.Cards, 1)
	card := data.Cards[0]
	require.Equal(t, "Reclassify a memory", card.Label)
	require.Equal(t, "arrow-up-down", card.Icon)
	require.NotNil(t, card.Rekind)
	require.Equal(t, "wordmark-rule", card.Rekind.Name)
	require.Equal(t, "constraint", card.Rekind.From)
	require.Equal(t, "convention", card.Rekind.To)

	req := httptest.NewRequest(http.MethodGet, "/console/gardener", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	body := do(mux, req).Body.String()
	require.Contains(t, body, "From kind")
	require.Contains(t, body, "To kind")
	require.Contains(t, body, "only its kind classification changes")

	require.Equal(t, http.StatusSeeOther, post(mux, "/console/gardener/"+p.ID+"/apply").Code)
	got, _, err := store.ProposalByID(ctx, db, p.ID)
	require.NoError(t, err)
	require.Equal(t, store.ProposalApplied, got.Status)
	mem, found, err := store.MemoryByName(ctx, db, "seamless", "wordmark-rule")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, core.KindConvention, mem.Kind, "apply reclassified the memory in place")
}

// newConsoleWithChat wires a chat-backed gardener and seeds two active global
// memories in a known newest-first order ([1] keep-me, [2] drop-me), so the
// natural-language request path has candidates to reference.
func newConsoleWithChat(t *testing.T, chatOut string) (context.Context, *sql.DB, *http.ServeMux) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := openFreshDB(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	dataDir := filepath.Join(dir, "data")
	mgr, err := files.NewManager(dataDir, db, slog.Default())
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })

	now := time.Now().UTC()
	writeConsoleMem(t, mgr, "keep-me", now)
	writeConsoleMem(t, mgr, "drop-me", now.Add(-time.Hour))

	rec := events.NewRecorder(db)
	garden := gardener.New(db, mgr, nil, stubChat{out: chatOut}, rec, gardener.FromConfig(config.Gardener{}), slog.Default())
	svc, err := New(Config{DB: db, Files: mgr, Gardener: garden, Events: rec, DataDir: dataDir, APIKey: testKey, Level: "advanced"})
	require.NoError(t, err)
	mux := http.NewServeMux()
	svc.Register(mux)
	return ctx, db, mux
}

func writeConsoleMem(t *testing.T, mgr *files.Manager, name string, updated time.Time) {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	_, err = mgr.WriteMemory(context.Background(), core.Memory{
		ID: id, Kind: core.KindGotcha, Name: name, Description: name + " description",
		Body: "body", Created: updated, Updated: updated, ValidFrom: updated,
	})
	require.NoError(t, err)
}

func postForm(mux *http.ServeMux, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return do(mux, req)
}

func TestGardenerRequest_CreatesProposals(t *testing.T) {
	ctx, db, mux := newConsoleWithChat(t, `{"ops":[{"op":"merge","keep":1,"drop":2}]}`)

	rr := postForm(mux, "/console/gardener/request", "request=keep-me+and+drop-me+are+duplicates&project=")
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.True(t, strings.HasPrefix(rr.Header().Get("Location"), "/console/gardener?notice="),
		"a successful request redirects with a positive notice, got %q", rr.Header().Get("Location"))

	merges, err := store.PendingProposals(ctx, db, store.ProposalMerge)
	require.NoError(t, err)
	require.Len(t, merges, 1)
	require.Equal(t, "request", merges[0].Payload["source"], "request-originated proposals are tagged for the UI chip")
}

func TestGardenerRequest_UnavailableWithoutChat(t *testing.T) {
	_, _, _, mux := newConsoleWithGardener(t) // gardener without a chat client
	var data gardenerData
	getJSON(t, mux, "/console/gardener?format=json", &data)
	require.False(t, data.CanRequest, "the request box is gated off when no chat client is configured")
}

// newConsoleForSplit wires a chat-backed console and seeds two active memories in
// project "arctop-app" ([1] ios-thing, [2] shared-thing), so the split path has
// something to classify.
func newConsoleForSplit(t *testing.T, chat llm.Chat) (context.Context, *sql.DB, *http.ServeMux) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := openFreshDB(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	dataDir := filepath.Join(dir, "data")
	mgr, err := files.NewManager(dataDir, db, slog.Default())
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })

	now := time.Now().UTC()
	writeProjectMem(t, mgr, "ios-thing", "arctop-app", now)
	writeProjectMem(t, mgr, "shared-thing", "arctop-app", now.Add(-time.Hour))
	_, err = store.EnsureProject(ctx, db, "arctop-app", "Arctop App")
	require.NoError(t, err)

	rec := events.NewRecorder(db)
	garden := gardener.New(db, mgr, nil, chat, rec, gardener.FromConfig(config.Gardener{}), slog.Default())
	svc, err := New(Config{DB: db, Files: mgr, Gardener: garden, Events: rec, DataDir: dataDir, APIKey: testKey, Level: "advanced"})
	require.NoError(t, err)
	mux := http.NewServeMux()
	svc.Register(mux)
	return ctx, db, mux
}

func writeProjectMem(t *testing.T, mgr *files.Manager, name, project string, updated time.Time) {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	_, err = mgr.WriteMemory(context.Background(), core.Memory{
		ID: id, Kind: core.KindGotcha, Name: name, Description: name + " description",
		Project: project, Body: "body", Created: updated, Updated: updated, ValidFrom: updated,
	})
	require.NoError(t, err)
}

const splitConsoleJSON = `{"children":[{"slug":"arctop-ios","label":"Arctop iOS"},{"slug":"arctop-android","label":"Arctop Android"}],` +
	`"shared":{"slug":"arctop-mobile-apps","label":"Arctop Mobile"},` +
	`"assignments":[{"memory":1,"to":"arctop-ios"},{"memory":2,"to":"arctop-mobile-apps"}]}`

func TestGardenerSplit_CreatesPlanGroup(t *testing.T) {
	ctx, db, mux := newConsoleForSplit(t, stubChat{out: splitConsoleJSON})

	rr := postForm(mux, "/console/gardener/split", "source=arctop-app&instruction=split+into+ios+and+android")
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.True(t, strings.HasPrefix(rr.Header().Get("Location"), "/console/gardener?notice="),
		"a successful split redirects with a positive notice, got %q", rr.Header().Get("Location"))

	splits, err := store.PendingProposals(ctx, db, store.ProposalSplit)
	require.NoError(t, err)
	require.Len(t, splits, 1)
	reps, err := store.PendingProposals(ctx, db, store.ProposalReproject)
	require.NoError(t, err)
	require.Len(t, reps, 2)

	// The page groups the batch under the plan, setup card first, with retarget targets.
	var data gardenerData
	getJSON(t, mux, "/console/gardener?format=json", &data)
	require.Len(t, data.Groups, 1)
	require.Equal(t, "split-arctop-app", data.Groups[0].Slug)
	require.Len(t, data.Groups[0].Cards, 3)
	require.Equal(t, "split", data.Groups[0].Cards[0].Kind, "the setup card sorts first")
	require.Equal(t, 2, data.Groups[0].MoveCount)
	require.Equal(t, "split into ios and android", data.Groups[0].Request)
	require.NotEmpty(t, data.Groups[0].Targets, "reproject cards have retarget options")
	require.Empty(t, data.Cards, "plan proposals do not pollute the loose grid")

	req := httptest.NewRequest(http.MethodGet, "/console/gardener", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	body := do(mux, req).Body.String()
	require.Contains(t, body, "Split plan:")
	require.Contains(t, body, "Setup is applied first")
	require.Contains(t, body, "Apply whole plan")

	// The retarget correction rides with the reproject in the reader, so open
	// one rather than expecting it on the setup card the queue selects first.
	rep := httptest.NewRequest(http.MethodGet, "/console/gardener/"+reps[0].ID, nil)
	rep.Header.Set("Authorization", "Bearer "+testKey)
	require.Contains(t, do(mux, rep).Body.String(), `class="gardener-retarget"`)
}

// A request recognized as a split of a known project chains straight into split
// planning: one POST, the plan batch lands, no second form.
func TestGardenerRequest_SplitRecognized_PlansInline(t *testing.T) {
	ctx, db, mux := newConsoleForSplit(t, &seqChat{outs: []string{
		`{"split":{"source":"arctop-app","instruction":"split into ios and android"}}`,
		splitConsoleJSON,
	}})

	rr := postForm(mux, "/console/gardener/request", "request=split+arctop-app+into+ios+and+android&project=")
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.True(t, strings.HasPrefix(rr.Header().Get("Location"), "/console/gardener?notice="),
		"a chained split redirects with a positive notice, got %q", rr.Header().Get("Location"))

	splits, err := store.PendingProposals(ctx, db, store.ProposalSplit)
	require.NoError(t, err)
	require.Len(t, splits, 1, "the split setup was planned from the general request")
	reps, err := store.PendingProposals(ctx, db, store.ProposalReproject)
	require.NoError(t, err)
	require.Len(t, reps, 2)
}

// Split intent without a known source bounces the request text back so the page
// renders the inline project picker follow-up.
func TestGardenerRequest_SplitIntentNoSource_ShowsPicker(t *testing.T) {
	_, _, mux := newConsoleForSplit(t, stubChat{out: `{"split":{"source":""}}`})

	rr := postForm(mux, "/console/gardener/request", "request=split+my+app+into+ios+and+android&project=")
	require.Equal(t, http.StatusSeeOther, rr.Code)
	loc := rr.Header().Get("Location")
	require.Equal(t, "/console/gardener?split="+url.QueryEscape("split my app into ios and android"), loc)

	var data gardenerData
	getJSON(t, mux, loc+"&format=json", &data)
	require.Equal(t, "split my app into ios and android", data.SplitReq)

	req := httptest.NewRequest(http.MethodGet, loc, nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	body := do(mux, req).Body.String()
	require.Contains(t, body, `id="split-form"`, "the picker follow-up form renders")
	require.Contains(t, body, "which project should be split")
}

func TestGardenerRetarget_ChangesTarget(t *testing.T) {
	ctx, db, mux := newConsoleForSplit(t, stubChat{out: splitConsoleJSON})
	require.Equal(t, http.StatusSeeOther, postForm(mux, "/console/gardener/split", "source=arctop-app").Code)

	reps, err := store.PendingProposals(ctx, db, store.ProposalReproject)
	require.NoError(t, err)
	require.NotEmpty(t, reps)
	id := reps[0].ID

	rr := postForm(mux, "/console/gardener/"+id+"/retarget", "to=arctop-android")
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.True(t, strings.HasPrefix(rr.Header().Get("Location"), "/console/gardener?notice="))

	p, _, err := store.ProposalByID(ctx, db, id)
	require.NoError(t, err)
	require.Equal(t, "arctop-android", p.Payload["to"], "retarget rewrote the destination project")
}

func TestGardenerApplyPlan_AppliesSetupThenMoves(t *testing.T) {
	ctx, db, mux := newConsoleForSplit(t, stubChat{out: splitConsoleJSON})
	require.Equal(t, http.StatusSeeOther, postForm(mux, "/console/gardener/split", "source=arctop-app").Code)

	rr := postForm(mux, "/console/gardener/plan/split-arctop-app/apply", "")
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.True(t, strings.HasPrefix(rr.Header().Get("Location"), "/console/gardener?notice="),
		"applying the whole plan succeeds, got %q", rr.Header().Get("Location"))

	// The child + shared projects exist and the memories moved out of the source.
	for _, slug := range []string{"arctop-ios", "arctop-mobile-apps"} {
		_, found, err := store.ProjectBySlug(ctx, db, slug)
		require.NoError(t, err)
		require.True(t, found, slug+" created by the setup proposal")
	}
	_, found, err := store.MemoryByName(ctx, db, "arctop-ios", "ios-thing")
	require.NoError(t, err)
	require.True(t, found, "ios-thing moved to arctop-ios")
	_, found, err = store.MemoryByName(ctx, db, "arctop-app", "ios-thing")
	require.NoError(t, err)
	require.False(t, found, "ios-thing left the source project")

	// Everything in the plan is resolved -> the page is tidy.
	pending, err := store.PendingProposals(ctx, db, "")
	require.NoError(t, err)
	require.Empty(t, pending)
}
