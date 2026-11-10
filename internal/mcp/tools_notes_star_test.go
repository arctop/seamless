package mcp_test

// A star is the owner's curation and a restricted account may not set one, so
// a change of a starred note's content drops it: the rule memories already
// follow (starAfter), on the note tools' content writes.

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/store"
)

// A note's content is its body, its title and its description: the text every
// index line shows. Tags and a move leave the star, and so does anything the
// owner does, or anyone with peer_auth off.
func TestRestricted_NoteContentChangeDropsTheStar(t *testing.T) {
	const otherOpen = "ka-open-other"
	edit := func(oldText, newText string) map[string]any {
		return map[string]any{"edits": []any{map[string]any{"old_string": oldText, "new_string": newText}}}
	}
	for _, tc := range []struct {
		name string
		call func(r *kaRig, id string)
		want bool
	}{
		{"body replace", func(r *kaRig, id string) {
			r.call(r.agent, "notes_update", map[string]any{"id": id, "body": "Agent body.\n"})
		}, false},
		{"title change", func(r *kaRig, id string) {
			r.call(r.agent, "notes_update", map[string]any{"id": id, "title": "Agent title"})
		}, false},
		{"description change", func(r *kaRig, id string) {
			r.call(r.agent, "notes_update", map[string]any{"id": id, "description": "the agent's line"})
		}, false},
		{"append", func(r *kaRig, id string) {
			r.call(r.agent, "notes_append", map[string]any{"id": id, "body": "Agent line."})
		}, false},
		{"edit", func(r *kaRig, id string) {
			args := edit("Owner", "Agent")
			args["id"] = id
			r.call(r.agent, "notes_edit", args)
		}, false},
		{"tags only", func(r *kaRig, id string) {
			r.call(r.agent, "notes_update", map[string]any{"id": id, "tags_add": []any{"x"}})
		}, true},
		{"the title and description sent back unchanged", func(r *kaRig, id string) {
			r.call(r.agent, "notes_update", map[string]any{"id": id, "title": "Starred note", "description": "the owner's line"})
		}, true},
		{"a move", func(r *kaRig, id string) {
			_, err := store.EnsureProject(r.ctx, r.db, otherOpen, otherOpen)
			require.NoError(r.t, err)
			r.call(r.agent, "notes_update", map[string]any{"id": id, "project": otherOpen})
		}, true},
		{"the owner's body replace", func(r *kaRig, id string) {
			r.call(r.owner, "notes_update", map[string]any{"id": id, "body": "Owner again.\n"})
		}, true},
		{"the owner's append", func(r *kaRig, id string) {
			r.call(r.owner, "notes_append", map[string]any{"id": id, "body": "Owner line."})
		}, true},
		{"the owner's edit", func(r *kaRig, id string) {
			args := edit("Owner", "The owner")
			args["id"] = id
			r.call(r.owner, "notes_edit", args)
		}, true},
		{"with peer_auth off", func(r *kaRig, id string) {
			r.call(r.plain, "notes_update", map[string]any{"id": id, "body": "No auth.\n"})
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newKARig(t, kaOpts{})
			n := r.call(r.owner, "notes_create", map[string]any{"title": "Starred note", "body": "Owner body.\n",
				"description": "the owner's line", "project": kaOpen})
			id := n["id"].(string)
			r.call(r.owner, "favorite_set", map[string]any{"kind": "note", "id": id, "favorite": true})

			tc.call(r, id)

			idx, ok, err := store.NoteByID(r.ctx, r.db, id)
			require.NoError(t, err)
			require.True(t, ok)
			disk, err := r.mgr.Store().ReadNote(idx.FilePath)
			require.NoError(t, err)
			require.Equal(t, tc.want, idx.Favorite, "index")
			require.Equal(t, tc.want, disk.Favorite, "file")
		})
	}
}

// A star a note never had stays absent, whoever writes.
func TestRestricted_NoteWithoutAStarStaysWithout(t *testing.T) {
	r := newKARig(t, kaOpts{})
	n := r.call(r.owner, "notes_create", map[string]any{"title": "Plain note", "body": "Owner body.\n", "project": kaOpen})
	id := n["id"].(string)
	r.call(r.agent, "notes_append", map[string]any{"id": id, "body": "Agent line."})
	idx, ok, err := store.NoteByID(r.ctx, r.db, id)
	require.NoError(t, err)
	require.True(t, ok)
	require.False(t, idx.Favorite)
}
