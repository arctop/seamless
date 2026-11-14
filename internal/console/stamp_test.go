package console

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// Provenance stamps on the console's item pages and search results (plan
// fenced-sessions P6): one line naming the projects and approvals, nothing
// for an unstamped item.

func stampConsole(t *testing.T) (*sql.DB, *files.Manager, *http.ServeMux) {
	t.Helper()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	dataDir := filepath.Join(dir, "data")
	mgr, err := files.NewManager(dataDir, db, slog.Default())
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })
	svc, err := New(Config{DB: db, Files: mgr, Events: events.NewRecorder(db), DataDir: dataDir, APIKey: testKey,
		Level: "advanced", Retrieve: retrieve.New(db, nil, config.Defaults().Budgets, nil)})
	require.NoError(t, err)
	mux := http.NewServeMux()
	svc.Register(mux)
	return db, mgr, mux
}

// stampGrant is a grant of example-legal approved for a conversation homed in
// example-data, as the approve core records one: the approval a stamp names.
func stampGrant(t *testing.T, db *sql.DB) []core.Stamp {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	asker := core.Session{ID: "01STAMPASKER00000000000000", Name: "cc/stamp-asker", ProjectSlug: "example-data",
		Status: core.SessionActive, Ambient: true, ExternalClient: "claude-code", ExternalSessionID: "stamp-asker",
		CreatedAt: now, UpdatedAt: now}
	require.NoError(t, store.CreateSession(ctx, db, asker))
	a, err := store.CreateGrantRequest(ctx, db, store.GrantRequest{Project: "example-legal", From: "example-data",
		Lease: time.Hour, TTL: 10 * time.Minute, SessionID: asker.ID, SessionName: asker.Name,
		ConversationClient: "claude-code", ConversationID: "stamp-asker"}, now)
	require.NoError(t, err)
	_, err = store.GrantAccessApprove(ctx, db, a.ID, a.Challenge, "owner", 0, store.GrantAccessRead, now)
	require.NoError(t, err)
	return []core.Stamp{{Project: "example-legal", Via: core.StampViaGrant, Ref: a.ID}}
}

func TestStampConsole_ItemPagesAndSearchShowTheLine(t *testing.T) {
	ctx := context.Background()
	db, mgr, mux := stampConsole(t)
	now := time.Now().UTC()
	stamps := stampGrant(t, db)
	line := core.MayContainLine(stamps)
	require.Regexp(t, `^may contain content from example-legal \(grant R-[0-9A-Z]{4}\)$`, line)
	mem, err := mgr.WriteMemory(ctx, core.Memory{ID: "01STAMPMEMORY0000000000000", Kind: core.KindDecision, Name: "close-rule",
		Description: "how the quarter closes", Project: "example-data", Body: "reconcile every ledger\n",
		Created: now, Updated: now, ValidFrom: now, MayContain: stamps})
	require.NoError(t, err)
	plain, err := mgr.WriteMemory(ctx, core.Memory{ID: "01STAMPPLAIN00000000000000", Kind: core.KindDecision, Name: "plain-rule",
		Description: "an unstamped rule", Project: "example-data", Body: "nothing held\n",
		Created: now, Updated: now, ValidFrom: now})
	require.NoError(t, err)
	note, err := mgr.WriteNote(ctx, core.Note{ID: "01STAMPNOTE000000000000000", Title: "ledger review", Slug: "ledger-review",
		Project: "example-data", Body: "the review\n", Created: now, Updated: now, MayContain: stamps})
	require.NoError(t, err)
	task := core.Task{ID: "01STAMPTASK000000000000000", ProjectSlug: "example-data", Title: "reconcile the ledger",
		Status: core.TaskOpen, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, store.CreateTask(ctx, db, task))
	require.NoError(t, store.StampWorkItem(ctx, db, core.ItemKindTask, task.ID, task.ProjectSlug, stamps, now))

	for _, path := range []string{
		"/console/memories/" + mem.ID, "/console/memories/" + mem.ID + "?peek=1", "/console/memories/" + mem.ID + "?reader=1",
		"/console/notes/" + note.ID, "/console/notes/" + note.ID + "?peek=1",
		"/console/tasks/" + task.ID, "/console/tasks/" + task.ID + "?peek=1",
		"/console/search?q=ledger",
	} {
		rr := getPeek(t, mux, path)
		require.Equal(t, http.StatusOK, rr.Code, path)
		require.Contains(t, rr.Body.String(), line, path)
		require.Contains(t, rr.Body.String(), "data-may-contain", path)
	}
	for _, path := range []string{"/console/memories/" + plain.ID, "/console/memories/" + plain.ID + "?peek=1"} {
		rr := getPeek(t, mux, path)
		require.Equal(t, http.StatusOK, rr.Code, path)
		require.NotContains(t, rr.Body.String(), "data-may-contain", path)
	}

	var d memoryDetail
	getJSON(t, mux, "/console/memories/"+mem.ID+"?format=json", &d)
	require.Equal(t, stamps, d.MayContain)
}
