package hooks

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/plans"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

const testSID = "abcdef12-3456"

// captureEnv is a hook server wired with the files layer and a plans dir, for
// exercising the plan-capture endpoints end to end.
type captureEnv struct {
	ts       *httptest.Server
	db       *sql.DB
	mgr      *files.Manager
	rec      *events.Recorder
	plansDir string
	logs     *logBuffer
}

// logBuffer collects the handler's log output, debug level included. The
// handler writes from its request goroutine, which the race detector cannot
// order against the test reading after the HTTP response, hence the lock.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newCaptureEnv(t *testing.T, pc config.PlanCapture) *captureEnv {
	t.Helper()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, store.SetSetting(context.Background(), db,
		store.SettingRepoProjectMap, `{"/work/demo":"demo"}`))

	mgr, err := files.NewManager(filepath.Join(dir, "data"), db, nil)
	require.NoError(t, err)

	plansDir := filepath.Join(dir, "plans")
	require.NoError(t, os.MkdirAll(plansDir, 0o755))

	rec := events.NewRecorder(db)
	ret := retrieve.New(db, nil, config.Budgets{MaxBriefingTokens: 1500, RecallBudgetTokens: 1000}, nil)
	logs := &logBuffer{}
	h := NewHandler(Config{
		DB: db, Retrieve: ret, Events: rec, Files: mgr,
		APIKey: testKey, PlanCapture: pc, PlansDir: plansDir,
		Logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	mux := http.NewServeMux()
	h.Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return &captureEnv{ts: ts, db: db, mgr: mgr, rec: rec, plansDir: plansDir, logs: logs}
}

// startSession creates the ambient session the capture correlates against.
func (e *captureEnv) startSession(t *testing.T) {
	t.Helper()
	resp, _ := post(t, e.ts.URL+"/api/hooks/session-start", testKey, map[string]any{
		"session_id": testSID, "cwd": "/work/demo", "source": "startup",
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func (e *captureEnv) writePlanFile(t *testing.T, basename, content string) string {
	t.Helper()
	path := filepath.Join(e.plansDir, basename+".md")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

func (e *captureEnv) postHook(t *testing.T, endpoint string, body map[string]any) {
	t.Helper()
	resp, _ := post(t, e.ts.URL+"/api/hooks/"+endpoint, testKey, body)
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func (e *captureEnv) planWrite(t *testing.T, toolName, filePath string) {
	t.Helper()
	e.postHook(t, "post-tool-use", map[string]any{
		"session_id": testSID, "cwd": "/work/demo", "tool_name": toolName,
		"tool_input": map[string]any{"file_path": filePath},
	})
}

// planWriteOut is planWrite returning the decoded hook response, for asserting
// on hookSpecificOutput.
func (e *captureEnv) planWriteOut(t *testing.T, toolName, filePath string) map[string]any {
	t.Helper()
	resp, out := post(t, e.ts.URL+"/api/hooks/post-tool-use", testKey, map[string]any{
		"session_id": testSID, "cwd": "/work/demo", "tool_name": toolName,
		"tool_input": map[string]any{"file_path": filePath},
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	return out
}

func (e *captureEnv) approve(t *testing.T, toolResponse map[string]any) {
	t.Helper()
	e.postHook(t, "post-tool-use", map[string]any{
		"session_id": testSID, "cwd": "/work/demo", "tool_name": "ExitPlanMode",
		"tool_input": map[string]any{}, "tool_response": toolResponse,
	})
}

func (e *captureEnv) loadNote(t *testing.T, slug string) (core.Note, bool) {
	t.Helper()
	idx, ok, err := store.NoteBySlug(context.Background(), e.db, "demo", slug)
	require.NoError(t, err)
	if !ok {
		return core.Note{}, false
	}
	note, err := e.mgr.Store().ReadNote(idx.FilePath)
	require.NoError(t, err)
	return note, true
}

func (e *captureEnv) sessionPlanMeta(t *testing.T) map[string]any {
	t.Helper()
	sess, ok, err := store.AmbientSessionByExternalIdentity(
		context.Background(), e.db, ClientClaudeCode.externalIdentity(), testSID)
	require.NoError(t, err)
	require.True(t, ok)
	m, _ := sess.Metadata["plan_capture"].(map[string]any)
	return m
}

func TestPlanCaptureIterationUpsert(t *testing.T) {
	e := newCaptureEnv(t, config.PlanCapture{Enabled: true, AutoTask: true})
	e.startSession(t)

	path := e.writePlanFile(t, "clever-stallman", "# My Plan\n\nStep one.\n")
	e.planWrite(t, "Write", path)

	note, found := e.loadNote(t, "cc-plan-clever-stallman")
	require.True(t, found)
	require.Equal(t, "My Plan", note.Title)
	require.Equal(t, "demo", note.Project)
	require.Contains(t, note.Tags, "plan:my-plan")
	require.Contains(t, note.Tags, "cc-plan")
	require.Contains(t, note.Tags, "plan-status:draft")
	require.Contains(t, note.Tags, "created-by:agent")
	require.Contains(t, note.Body,
		"> captured from "+ambientName(ClientClaudeCode, testSID)+" | clever-stallman.md | iter 1 | git ")
	require.Contains(t, note.Body, "# My Plan\n\nStep one.")
	require.Equal(t, 1, plans.NoteIteration(note))

	meta := e.sessionPlanMeta(t)
	require.Equal(t, "clever-stallman", meta["basename"])
	require.Equal(t, "my-plan", meta["plan_slug"])
	require.Equal(t, "draft", meta["status"])

	evs := eventsOfKind(t, e.rec, core.EventPlanCaptured)
	require.Len(t, evs, 1)
	require.Equal(t, note.ID, evs[0].ItemID)
	require.Equal(t, "clever-stallman", evs[0].Payload["basename"])
	require.Equal(t, "my-plan", evs[0].Payload["plan_slug"])
	require.Contains(t, payloadString(evs[0].Payload["content"]), "Step one.")

	// Second iteration: body and title follow the file; the note identity and
	// the plan composition slug do not.
	e.writePlanFile(t, "clever-stallman", "# My Better Plan\n\nStep two.\n")
	e.planWrite(t, "Edit", path)

	note2, found := e.loadNote(t, "cc-plan-clever-stallman")
	require.True(t, found)
	require.Equal(t, note.ID, note2.ID)
	require.Equal(t, note.Created.Unix(), note2.Created.Unix())
	require.Equal(t, "My Better Plan", note2.Title)
	require.Contains(t, note2.Tags, "plan:my-plan", "title change must not re-slug the plan")
	require.Equal(t, 2, plans.NoteIteration(note2))
	require.Contains(t, note2.Body, "Step two.")
	require.NotContains(t, note2.Body, "Step one.")
	require.Len(t, eventsOfKind(t, e.rec, core.EventPlanCaptured), 2)
}

// tagCount counts occurrences of v in tags, for duplicate-tag assertions.
func tagCount(tags []string, v string) int {
	n := 0
	for _, t := range tags {
		if t == v {
			n++
		}
	}
	return n
}

// TestPlanCapturePreservesForeignTags pins the upsert tag-merge: a tag the
// owner (or another agent) added to a captured plan note survives the next
// plan-file save and the approval, while the hook-managed tags (plan:<slug>,
// cc-plan, created-by:agent, plan-status:*) stay authoritative and are never
// duplicated.
func TestPlanCapturePreservesForeignTags(t *testing.T) {
	e := newCaptureEnv(t, config.PlanCapture{Enabled: true, AutoTask: false})
	e.startSession(t)

	path := e.writePlanFile(t, "clever-stallman", "# My Plan\n\nStep one.\n")
	e.planWrite(t, "Write", path)

	// The owner tags the captured note; a foreign plan:* tag is added too, to
	// pin that the composition tag remains hook-managed.
	note, found := e.loadNote(t, "cc-plan-clever-stallman")
	require.True(t, found)
	note.Tags = append(note.Tags, "priority:high", "plan:owner-renamed")
	_, err := e.mgr.WriteNote(context.Background(), note)
	require.NoError(t, err)

	// Re-saving the plan file re-upserts the note: the foreign tag survives,
	// the managed tags update without duplicating.
	e.writePlanFile(t, "clever-stallman", "# My Plan\n\nStep two.\n")
	e.planWrite(t, "Edit", path)

	note, found = e.loadNote(t, "cc-plan-clever-stallman")
	require.True(t, found)
	require.Contains(t, note.Tags, "priority:high", "owner-added tag must survive a re-capture")
	require.Contains(t, note.Tags, "plan:my-plan")
	require.NotContains(t, note.Tags, "plan:owner-renamed", "the composition tag is hook-managed")
	require.Contains(t, note.Tags, "plan-status:draft")
	require.Equal(t, 1, tagCount(note.Tags, "plan:my-plan"))
	require.Equal(t, 1, tagCount(note.Tags, "cc-plan"))
	require.Equal(t, 1, tagCount(note.Tags, "created-by:agent"))
	require.Equal(t, 1, tagCount(note.Tags, "priority:high"))

	// Approval updates the managed status tag and still keeps the foreign tag.
	e.approve(t, map[string]any{"filePath": path})
	note, found = e.loadNote(t, "cc-plan-clever-stallman")
	require.True(t, found)
	require.Contains(t, note.Tags, "priority:high", "owner-added tag must survive approval")
	require.Contains(t, note.Tags, "plan-status:approved")
	require.NotContains(t, note.Tags, "plan-status:draft")
	require.Equal(t, 1, tagCount(note.Tags, "plan:my-plan"))
}

func TestPlanCaptureIgnoresNonPlanWrites(t *testing.T) {
	e := newCaptureEnv(t, config.PlanCapture{Enabled: true, AutoTask: true})
	e.startSession(t)

	e.planWrite(t, "Write", "/work/demo/main.go")
	e.planWrite(t, "Write", filepath.Join(e.plansDir, "nested", "deep.md"))
	e.planWrite(t, "Write", filepath.Join(e.plansDir, "notes.txt"))
	e.planWrite(t, "Write", filepath.Join(e.plansDir, "..", "escape.md"))

	require.Empty(t, eventsOfKind(t, e.rec, core.EventPlanCaptured))
}

func TestPlanCaptureDisabled(t *testing.T) {
	e := newCaptureEnv(t, config.PlanCapture{Enabled: false, AutoTask: true})
	e.startSession(t)

	path := e.writePlanFile(t, "clever-stallman", "# My Plan\n\nStep one.\n")
	e.planWrite(t, "Write", path)

	_, found := e.loadNote(t, "cc-plan-clever-stallman")
	require.False(t, found)
	require.Empty(t, eventsOfKind(t, e.rec, core.EventPlanCaptured))
}

func TestPlanApprovalFlipsStatusAndCreatesTask(t *testing.T) {
	e := newCaptureEnv(t, config.PlanCapture{Enabled: true, AutoTask: true})
	e.startSession(t)
	ctx := context.Background()

	path := e.writePlanFile(t, "clever-stallman", "# My Plan\n\nDo it.\n")
	e.planWrite(t, "Write", path)
	e.approve(t, map[string]any{"filePath": path, "plan": nil})

	note, found := e.loadNote(t, "cc-plan-clever-stallman")
	require.True(t, found)
	require.Contains(t, note.Tags, "plan-status:approved")
	require.Equal(t, 1, plans.NoteIteration(note), "approval must not bump the iteration")

	tasks, err := store.ListTasksForPlan(ctx, e.db, "demo", "", "my-plan")
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	require.Equal(t, "Implement plan: My Plan", tasks[0].Title)
	require.Equal(t, core.TaskOpen, tasks[0].Status)
	require.Equal(t, "my-plan", tasks[0].PlanSlug)
	require.Equal(t, ambientName(ClientClaudeCode, testSID), tasks[0].CreatedBy)
	require.Contains(t, tasks[0].Body, "cc-plan-clever-stallman")
	require.Contains(t, tasks[0].Body, note.ID)

	require.Equal(t, "approved", e.sessionPlanMeta(t)["status"])
	require.Len(t, eventsOfKind(t, e.rec, core.EventPlanApproved), 1)

	// Re-approval records another event but never a second task.
	e.approve(t, map[string]any{"filePath": path})
	tasks, err = store.ListTasksForPlan(ctx, e.db, "demo", "", "my-plan")
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	require.Len(t, eventsOfKind(t, e.rec, core.EventPlanApproved), 2)
}

func TestPlanApprovalNoAutoTask(t *testing.T) {
	e := newCaptureEnv(t, config.PlanCapture{Enabled: true, AutoTask: false})
	e.startSession(t)

	path := e.writePlanFile(t, "clever-stallman", "# My Plan\n\nDo it.\n")
	e.planWrite(t, "Write", path)
	e.approve(t, map[string]any{"filePath": path})

	note, found := e.loadNote(t, "cc-plan-clever-stallman")
	require.True(t, found)
	require.Contains(t, note.Tags, "plan-status:approved")
	tasks, err := store.ListTasksForPlan(context.Background(), e.db, "demo", "", "my-plan")
	require.NoError(t, err)
	require.Empty(t, tasks)
}

func TestPlanApprovalPlanTextFallback(t *testing.T) {
	e := newCaptureEnv(t, config.PlanCapture{Enabled: true, AutoTask: true})
	e.startSession(t)

	path := e.writePlanFile(t, "clever-stallman", "# My Plan\n\nV1.\n")
	e.planWrite(t, "Write", path)

	// No usable filePath in the tool_response: the basename comes from the
	// session's plan_capture metadata, the content from tool_response.plan.
	require.NoError(t, os.Remove(path))
	e.approve(t, map[string]any{"filePath": "", "plan": "# My Plan\n\nFinal text.\n"})

	note, found := e.loadNote(t, "cc-plan-clever-stallman")
	require.True(t, found)
	require.Contains(t, note.Tags, "plan-status:approved")
	require.Contains(t, note.Body, "Final text.")
}

func TestPlanApprovalMissingFileFailsOpen(t *testing.T) {
	e := newCaptureEnv(t, config.PlanCapture{Enabled: true, AutoTask: true})
	e.startSession(t)

	e.approve(t, map[string]any{"filePath": filepath.Join(e.plansDir, "ghost.md")})

	_, found := e.loadNote(t, "cc-plan-ghost")
	require.False(t, found)
	require.Empty(t, eventsOfKind(t, e.rec, core.EventPlanApproved))
	tasks, err := store.ListTasksForPlan(context.Background(), e.db, "demo", "", "ghost")
	require.NoError(t, err)
	require.Empty(t, tasks)
}

func TestPermissionRequestMarksPresented(t *testing.T) {
	e := newCaptureEnv(t, config.PlanCapture{Enabled: true, AutoTask: true})
	e.startSession(t)

	path := e.writePlanFile(t, "clever-stallman", "# My Plan\n\nDo it.\n")
	e.planWrite(t, "Write", path)

	present := map[string]any{
		"session_id": testSID, "cwd": "/work/demo", "tool_name": "ExitPlanMode",
	}
	e.postHook(t, "permission-request", present)

	note, found := e.loadNote(t, "cc-plan-clever-stallman")
	require.True(t, found)
	require.Contains(t, note.Tags, "plan-status:presented")
	require.Contains(t, note.Tags, "plan:my-plan")
	require.Equal(t, "presented", e.sessionPlanMeta(t)["status"])
	require.Len(t, eventsOfKind(t, e.rec, core.EventPlanPresented), 1)

	// Only a draft flips; a second delivery is a no-op.
	e.postHook(t, "permission-request", present)
	require.Len(t, eventsOfKind(t, e.rec, core.EventPlanPresented), 1)

	// A presented plan still approves normally afterwards.
	e.approve(t, map[string]any{"filePath": path})
	note, _ = e.loadNote(t, "cc-plan-clever-stallman")
	require.Contains(t, note.Tags, "plan-status:approved")
}

func TestPermissionRequestWithoutDraftIsNoop(t *testing.T) {
	e := newCaptureEnv(t, config.PlanCapture{Enabled: true, AutoTask: true})
	e.startSession(t)

	e.postHook(t, "permission-request", map[string]any{
		"session_id": testSID, "cwd": "/work/demo", "tool_name": "ExitPlanMode",
	})
	require.Empty(t, eventsOfKind(t, e.rec, core.EventPlanPresented))
}

func TestFirstPlanCaptureInjectsRelated(t *testing.T) {
	e := newCaptureEnv(t, config.PlanCapture{Enabled: true, AutoTask: true, InjectRelated: true})
	e.startSession(t)
	insertMemory(t, e.db, "01R", "decision", "gardener-pass-order",
		"gardener passes run dedup then staleness then digest", "demo")

	path := e.writePlanFile(t, "clever-stallman", "# Rework the gardener staleness pass\n\nSteps.\n")
	out := e.planWriteOut(t, "Write", path)
	ac := additionalContext(t, out)
	require.Contains(t, ac, "<seam-plan-context>")
	require.Contains(t, ac, "memory_read name=gardener-pass-order")
	require.NotContains(t, ac, "cc-plan-clever-stallman", "the plan's own note is excluded")

	// The injection feeds the funnel like any other.
	evs := eventsOfKind(t, e.rec, core.EventInjected)
	require.Len(t, evs, 1)
	require.Equal(t, "post-tool-use", evs[0].Payload["hook"])

	// Later iterations never re-inject: only the session's first capture does.
	e.writePlanFile(t, "clever-stallman", "# Rework the gardener staleness pass\n\nMore.\n")
	out = e.planWriteOut(t, "Edit", path)
	_, has := out["hookSpecificOutput"]
	require.False(t, has, "no hookSpecificOutput after the first capture")
	require.Len(t, eventsOfKind(t, e.rec, core.EventInjected), 1)
}

// InjectRelated governs the prior-knowledge lookup, not the composition line:
// the slug is mechanism the agent needs in order to compose steps into this
// plan rather than minting a second one, so it rides on capture being enabled
// at all. With no recall hits attached there is nothing retrieved to attribute,
// so no injection event is recorded either.
func TestFirstPlanCaptureInjectRelatedDisabled(t *testing.T) {
	e := newCaptureEnv(t, config.PlanCapture{Enabled: true, AutoTask: true})
	e.startSession(t)
	insertMemory(t, e.db, "01R", "decision", "gardener-pass-order",
		"gardener passes run dedup then staleness then digest", "demo")

	path := e.writePlanFile(t, "clever-stallman", "# Rework the gardener staleness pass\n\nSteps.\n")
	out := e.planWriteOut(t, "Write", path)
	ac := additionalContext(t, out)
	require.Contains(t, ac, "plan:rework-the-gardener-staleness-pass", "the composition slug is not gated by InjectRelated")
	require.NotContains(t, ac, "memory_read name=gardener-pass-order", "but the related-knowledge lookup is")
	require.Empty(t, eventsOfKind(t, e.rec, core.EventInjected))
}

// The composition slug is what stops an agent minting a second plan for work it
// has just planned: without it the slug first appears in the NEXT session's
// briefing, long after tasks_add has filed the steps somewhere else.
func TestPlanCaptureNamesItsCompositionSlug(t *testing.T) {
	e := newCaptureEnv(t, config.PlanCapture{Enabled: true, AutoTask: true})
	e.startSession(t)

	path := e.writePlanFile(t, "clever-stallman", "# Rework the gardener staleness pass\n\nSteps.\n")
	ac := additionalContext(t, e.planWriteOut(t, "Write", path))
	require.Contains(t, ac, "plan:rework-the-gardener-staleness-pass")
	require.Contains(t, ac, "tasks_add plan=rework-the-gardener-staleness-pass")

	// Approval repeats it: that is the moment the agent stops planning and starts
	// filing steps, and it is reached even when the first capture was missed.
	resp, out := post(t, e.ts.URL+"/api/hooks/post-tool-use", testKey, map[string]any{
		"session_id": testSID, "cwd": "/work/demo", "tool_name": "ExitPlanMode",
		"tool_input": map[string]any{}, "tool_response": map[string]any{"filePath": path},
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, additionalContext(t, out), "plan:rework-the-gardener-staleness-pass")
}

// The direct git-reading tests moved with the reader to internal/gitread; the
// capture tests above still cover the stamp end to end.
