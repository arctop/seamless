package mcp_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// notes_delete reads the note under its file's lock before it removes the
// file: when the path the index names now holds another note, that note is
// left alone and the call answers as for an unknown id, recording nothing.
func TestNotesDelete_LeavesAnotherNoteAtThePath(t *testing.T) {
	ctx := context.Background()
	url, db, _, dir := newServerFiles(t, nil)
	cli := dialClient(t, ctx, url, testKey)
	callJSON(t, ctx, cli, "session_start", map[string]any{"cwd": "/work/demo", "name": "note-delete-lock"})

	created := callJSON(t, ctx, cli, "notes_create", map[string]any{"title": "Doomed note", "body": "body"})
	id, _ := created["id"].(string)
	idx, ok, err := store.NoteByID(ctx, db, id)
	require.NoError(t, err)
	require.True(t, ok)

	// Another note now sits at the path: the same file, another id.
	path := filepath.Join(dir, idx.FilePath)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	other, err := core.NewID()
	require.NoError(t, err)
	require.Contains(t, string(raw), "id: "+id)
	require.NoError(t, os.WriteFile(path, []byte(strings.Replace(string(raw), "id: "+id, "id: "+other, 1)), 0o600))

	before := len(noteEventRows(t, db, core.EventNoteWritten))
	isErr, text := callErr(t, ctx, cli, "notes_delete", map[string]any{"id": id})
	require.True(t, isErr)
	require.Contains(t, text, "no note with id")
	_, err = os.Stat(path)
	require.NoError(t, err, "the other note's file stays")
	require.Len(t, noteEventRows(t, db, core.EventNoteWritten), before, "nothing was deleted, so nothing is recorded")
}
