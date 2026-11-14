package files_test

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/archive"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// An archive carries the corpus files verbatim, so a memory's provenance
// stamps ride along in its frontmatter, and the restored index mirrors them;
// a task's ride in the database (plan fenced-sessions P6).
func TestStampsArchive_FreshRestoreKeepsTheStamps(t *testing.T) {
	ctx := context.Background()
	src := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(src, archive.DBName))
	require.NoError(t, err)
	mgr, err := files.NewManager(src, db, nil)
	require.NoError(t, err)
	now := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	stamps := []core.Stamp{{Project: "example-legal", Via: core.StampViaGrant, Ref: "01GRANTLEGAL"}}
	mem, err := mgr.WriteMemory(ctx, core.Memory{ID: "01STAMPARCHIVEMEM000000000", Kind: core.KindDecision, Name: "close-rule",
		Description: "d", Project: "example-data", Body: "b\n", Created: now, Updated: now, ValidFrom: now, MayContain: stamps})
	require.NoError(t, err)
	require.NoError(t, store.CreateTask(ctx, db, core.Task{ID: "01STAMPARCHIVETASK00000000", ProjectSlug: "example-data",
		Title: "t", Status: core.TaskOpen, CreatedAt: now, UpdatedAt: now}))
	require.NoError(t, store.StampWorkItem(ctx, db, core.ItemKindTask, "01STAMPARCHIVETASK00000000", "example-data", stamps, now))
	require.NoError(t, mgr.Close())
	require.NoError(t, db.Close())

	var buf bytes.Buffer
	_, err = archive.Export(ctx, archive.ExportOptions{DataDir: src, Out: &buf, Host: "stamphost"})
	require.NoError(t, err)
	dst := filepath.Join(t.TempDir(), "restored")
	_, err = archive.Import(ctx, archive.ImportOptions{DataDir: dst, Src: bytes.NewReader(buf.Bytes())})
	require.NoError(t, err)

	restored, err := files.NewStore(dst).ReadMemory(mem.FilePath)
	require.NoError(t, err)
	require.Equal(t, stamps, restored.MayContain, "the frontmatter key rides along")
	rdb, err := store.OpenExisting(filepath.Join(dst, archive.DBName))
	require.NoError(t, err)
	t.Cleanup(func() { _ = rdb.Close() })
	for kind, id := range map[string]string{core.ItemKindMemory: mem.ID, core.ItemKindTask: "01STAMPARCHIVETASK00000000"} {
		got, err := store.ProvenanceForItems(ctx, rdb, kind, []string{id})
		require.NoError(t, err)
		require.Len(t, got[id], 1, kind)
		require.Equal(t, "example-legal", got[id][0].Project, kind)
	}
}
