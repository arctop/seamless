package files

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// Provenance stamps in frontmatter (may_contain, plan fenced-sessions P6): a
// key only ever added, every entry round-trips, and the index mirrors the
// file.

func stampSet() []core.Stamp {
	return []core.Stamp{
		{Project: "example-legal", Via: core.StampViaGrant, Ref: "01M7999M7MBPEKDYS4P1XBJC4D"},
		{Project: "example-finance", Via: core.StampViaMove, Ref: "01M79FJBCPRF2WWPHWE592Y3C1"},
		{Project: "example-vault", Via: core.StampViaSession, Ref: "01M7C6HQJW9BGFW332C57F3W2M"},
	}
}

func stampRows(t *testing.T, db *sql.DB, kind, id string) []store.ProvenanceStamp {
	t.Helper()
	got, err := store.ProvenanceForItem(context.Background(), db, kind, id)
	require.NoError(t, err)
	return got
}

func TestStamps_FrontmatterRoundTrip(t *testing.T) {
	m := sampleMemory()
	m.MayContain = stampSet()
	content, err := RenderMemory(m)
	require.NoError(t, err)
	require.Contains(t, content, "may_contain:\n    - project: example-legal\n      grant: 01M7999M7MBPEKDYS4P1XBJC4D\n")
	back, err := ParseMemory(content, MemoryRelPath(m.Project, m.Name))
	require.NoError(t, err)
	require.Equal(t, m.MayContain, back.MayContain)
	require.Empty(t, back.Extra, "a known key, never an unknown one")
	again, err := RenderMemory(back)
	require.NoError(t, err)
	require.Equal(t, content, again, "the rendering is stable")

	n := core.Note{ID: "01NOTE", Title: "t", Slug: "t", Project: "example-data", Body: "b\n",
		Created: m.Created, Updated: m.Updated, MayContain: stampSet()[:1]}
	nc, err := RenderNote(n)
	require.NoError(t, err)
	nb, err := ParseNote(nc, NoteRelPath(n.Project, n.Slug))
	require.NoError(t, err)
	require.Equal(t, n.MayContain, nb.MayContain)

	// No stamp, no key: a file written with isolation.requests off is what it
	// always was.
	plain, err := RenderMemory(sampleMemory())
	require.NoError(t, err)
	require.NotContains(t, plain, "may_contain")
}

// An entry's keys this release does not know (a newer release's), and an
// entry naming no reference it knows, survive a rewrite.
func TestStamps_UnknownEntryKeysRoundTrip(t *testing.T) {
	content := "---\nid: 01MEM\nkind: gotcha\nname: n\ndescription: d\ncreated: 2026-07-10T18:00:00Z\n" +
		"updated: 2026-07-10T18:00:00Z\nvalid_from: 2026-07-10T18:00:00Z\ninvalid_at: null\nsuperseded_by: null\n" +
		"may_contain:\n" +
		"    - project: example-legal\n      grant: 01GRANT\n      at: 2026-11-13T09:00:00Z\n" +
		"    - project: example-desk\n      relay: 01RELAY\n" +
		"    - example-hr\n" +
		"x_plugin: kept\n---\nbody\n"
	m, err := ParseMemory(content, "memory/_global/n.md")
	require.NoError(t, err)
	require.Len(t, m.MayContain, 3)
	require.Equal(t, core.Stamp{Project: "example-legal", Via: core.StampViaGrant, Ref: "01GRANT",
		Extra: map[string]any{"at": time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)}}, m.MayContain[0])
	require.Equal(t, core.Stamp{Project: "example-desk", Extra: map[string]any{"relay": "01RELAY"}}, m.MayContain[1])
	require.Equal(t, core.Stamp{Project: "example-hr"}, m.MayContain[2])
	out, err := RenderMemory(m)
	require.NoError(t, err)
	for _, want := range []string{"grant: 01GRANT", "at: 2026-11-13T09:00:00Z", "relay: 01RELAY", "project: example-hr", "x_plugin: kept"} {
		require.Contains(t, out, want)
	}
	back, err := ParseMemory(out, "memory/_global/n.md")
	require.NoError(t, err)
	require.Equal(t, m.MayContain, back.MayContain)

	_, err = ParseMemory(strings.Replace(content, "    - example-hr\n", "    - [a, b]\n", 1), "memory/_global/n.md")
	require.Error(t, err, "a malformed entry fails like any malformed known key")
}

// The previous release knows no may_contain: it keeps the key in Extra and
// writes it back re-encoded. This release reads that back as the same stamps.
func TestStamps_PreviousReleaseRewriteReadsBack(t *testing.T) {
	m := sampleMemory()
	m.MayContain = stampSet()
	content, err := RenderMemory(m)
	require.NoError(t, err)
	yamlText, _ := splitDocument(content)
	var raw map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(yamlText), &raw))
	old := sampleMemory()
	old.Extra = map[string]any{"may_contain": raw["may_contain"]} // captureExtra, as the previous release holds it
	rewritten, err := RenderMemory(old)
	require.NoError(t, err)
	back, err := ParseMemory(rewritten, MemoryRelPath(m.Project, m.Name))
	require.NoError(t, err)
	require.Equal(t, m.MayContain, back.MayContain)
}

// The index mirrors the file on every write: an agent's edit keeps the key
// and the rows; the owner's hand edit removing it removes the rows at the
// next reindex; the file leaving the corpus takes them along.
func TestStamps_IndexMirrorsTheFile(t *testing.T) {
	m, db := newManager(t)
	ctx := context.Background()
	mem := sampleMemory()
	mem.MayContain = stampSet()
	written, err := m.WriteMemory(ctx, mem)
	require.NoError(t, err)
	rows := stampRows(t, db, core.ItemKindMemory, mem.ID)
	require.Len(t, rows, 3)
	for _, r := range rows {
		require.Equal(t, mem.Project, r.Project)
	}

	// An edit through the mutation lock edits the item as the file carries it.
	edited, err := m.MutateMemory(ctx, written.FilePath, func(_ context.Context, cur core.Memory) (core.Memory, error) {
		cur.Body += "one more line\n"
		return cur, nil
	})
	require.NoError(t, err)
	require.Equal(t, mem.MayContain, edited.MayContain)
	onDisk, err := m.Store().ReadMemory(written.FilePath)
	require.NoError(t, err)
	require.Equal(t, mem.MayContain, onDisk.MayContain)
	require.Len(t, stampRows(t, db, core.ItemKindMemory, mem.ID), 3)

	// The owner removes one stamp by hand: the file is the source of truth.
	onDisk.MayContain = onDisk.MayContain[1:]
	_, err = m.Store().WriteMemory(onDisk)
	require.NoError(t, err)
	require.NoError(t, m.handleFile(ctx, written.FilePath))
	rows = stampRows(t, db, core.ItemKindMemory, mem.ID)
	require.Len(t, rows, 2)
	require.Equal(t, "example-finance", rows[0].Source)

	// A move into a project the memory was stamped for keeps the file's entry
	// and drops that row; the remaining rows follow the memory's project.
	moved, err := m.MoveMemory(ctx, onDisk, "example-vault")
	require.NoError(t, err)
	require.Equal(t, onDisk.MayContain, moved.MayContain)
	rows = stampRows(t, db, core.ItemKindMemory, mem.ID)
	require.Len(t, rows, 1)
	require.Equal(t, "example-vault", rows[0].Project)
	require.Equal(t, "example-finance", rows[0].Source)

	require.NoError(t, m.Remove(ctx, moved.FilePath))
	require.Empty(t, stampRows(t, db, core.ItemKindMemory, mem.ID), "the item left the corpus")

	// A memory with no stamp touches no row.
	_, err = m.WriteMemory(ctx, sampleMemory())
	require.NoError(t, err)
	require.Zero(t, indexedCount(t, db, "item_provenance"))
}

// The owner's editor save reaches the mirror through the watcher.
func TestStamps_WatcherHandEditUpdatesTheMirror(t *testing.T) {
	m, db := newManager(t)
	m.watcher.debounce = 15 * time.Millisecond
	ctx := t.Context()
	require.NoError(t, m.Start(ctx))
	n := core.Note{ID: "01NOTESTAMP", Title: "t", Slug: "stamped", Project: "example-data", Body: "b\n",
		Created: time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC), Updated: time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC),
		MayContain: stampSet()[:2]}
	content, err := RenderNote(n)
	require.NoError(t, err)
	abs := filepath.Join(m.Store().DataDir(), filepath.FromSlash(NoteRelPath(n.Project, n.Slug)))
	require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o700))
	require.NoError(t, os.WriteFile(abs, []byte(content), 0o600))
	require.Eventually(t, func() bool { return len(stampRows(t, db, core.ItemKindNote, n.ID)) == 2 },
		3*time.Second, 10*time.Millisecond, "the watcher indexes the stamps the file carries")

	n.MayContain = nil
	content, err = RenderNote(n)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(abs, []byte(content), 0o600))
	require.Eventually(t, func() bool { return len(stampRows(t, db, core.ItemKindNote, n.ID)) == 0 },
		3*time.Second, 10*time.Millisecond, "the owner's hand edit that removes the key removes the rows")
}
