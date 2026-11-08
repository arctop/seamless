package files

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// A restricted account's label (core.Memory.Account / core.Note.Account) is
// frontmatter `account`: it round-trips, is mirrored to the index, and is left
// out entirely when empty, so the owner's files are byte-identical to before.
func TestAccountFrontmatterRoundTripsAndIndexes(t *testing.T) {
	m := sampleMemory()
	plain, err := RenderMemory(m)
	require.NoError(t, err)
	require.NotContains(t, plain, "account:", "an owner-written memory carries no account key")

	m.Account = "_example_agent"
	labeled, err := RenderMemory(m)
	require.NoError(t, err)
	require.Contains(t, labeled, "\naccount: _example_agent\n")
	got, err := ParseMemory(labeled, MemoryRelPath(m.Project, m.Name))
	require.NoError(t, err)
	require.Equal(t, "_example_agent", got.Account)
	require.Empty(t, got.Extra, "account is a known key, not an unknown one carried in Extra")

	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	n := core.Note{ID: "01K0NOTE00000000000000000A", Title: "Findings", Slug: "findings", Project: "seam",
		Body: "body\n", Created: now, Updated: now, Account: "_example_agent"}
	noteText, err := RenderNote(n)
	require.NoError(t, err)
	require.Contains(t, noteText, "\naccount: _example_agent\n")
	gotNote, err := ParseNote(noteText, NoteRelPath(n.Project, n.Slug))
	require.NoError(t, err)
	require.Equal(t, "_example_agent", gotNote.Account)
	n.Account = ""
	noteText, err = RenderNote(n)
	require.NoError(t, err)
	require.NotContains(t, noteText, "account:")

	ix, db := newIndexer(t)
	ctx := context.Background()
	got.ContentHash = ContentHash(labeled)
	require.NoError(t, ix.IndexMemory(ctx, got))
	gotNote.ContentHash = "h"
	require.NoError(t, ix.IndexNote(ctx, gotNote))
	mem, found, err := store.MemoryByID(ctx, db, got.ID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "_example_agent", mem.Account)
	note, found, err := store.NoteByID(ctx, db, gotNote.ID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "_example_agent", note.Account)

	// The owner rewrites the content: the label goes with it.
	got.Account = ""
	require.NoError(t, ix.IndexMemory(ctx, got))
	mem, _, err = store.MemoryByID(ctx, db, got.ID)
	require.NoError(t, err)
	require.Empty(t, mem.Account)
}
