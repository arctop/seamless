package mcp

// Provenance stamps at the tool surface (isolation.requests, plan
// fenced-sessions P6; owner decision N2 (a)). Off, ctx carries no judged
// conversation, store.StampsOf answers nil, and nothing here writes or
// answers anything: every file, row and answer is what it was.
//
// On, a write a conversation makes while its label holds a fenced project
// other than the destination records, for each such project, "may contain
// content from <project> (<grant|move> <short id>)" -- from the call's judged
// state (grantsMiddleware), never from an argument:
//
//   - a memory or a note carries it in frontmatter (may_contain), written
//     with the content in the same file write, so a held write holds it too
//     and the files layer mirrors it into item_provenance;
//   - a task and a session's findings carry it in item_provenance only.
//
// What stamps: a write that carries the conversation's text or carries an
// item into a project -- a create, an overwrite, an append, an edit adding
// text (body edits, a description, added tags), a note's or a task's new
// title, body or project, findings. What does not: a status change, a claim
// or a release, a star, a dependency or commit change, a tag removal, a
// delete, and a supersede's tombstone -- none of them writes anything the
// conversation holds. No write removes a stamp: every write path edits the
// item as its file carries it (MutateMemory, MutateNote, the overwrite's
// re-read), and a supersession gives the replacement the union of both
// memories' stamps (lifecycle.UnionStamps).
//
// Reads show them -- memory_read, notes_read, tasks_list and tasks_ready (a
// may_contain field), recall hits (retrieve) -- naming projects and
// approvals, never content, and only the projects the caller's View may list:
// a restricted account is told how many it may not be told about
// (may_contain_withheld), never their names.

import (
	"context"
	"fmt"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// The answer keys a stamped item's read carries; absent for an item with no
// stamp, so every other answer reads as before.
const (
	mayContainKey         = "may_contain"
	mayContainWithheldKey = "may_contain_withheld"
)

// stampMemory adds the call's stamps for mem's project to mem, keeping every
// stamp it already carries.
func stampMemory(ctx context.Context, mem *core.Memory) {
	mem.MayContain, _ = core.MergeStamps(mem.MayContain, store.StampsOf(ctx, mem.Project), mem.Project)
}

// stampNote is stampMemory for a note.
func stampNote(ctx context.Context, note *core.Note) {
	note.MayContain, _ = core.MergeStamps(note.MayContain, store.StampsOf(ctx, note.Project), note.Project)
}

// stampWorkItem records the call's stamps on a task or a session in project
// (item_provenance, their only home). With none it touches nothing.
func (s *Server) stampWorkItem(ctx context.Context, kind, id, project string) error {
	stamps := store.StampsOf(ctx, project)
	if len(stamps) == 0 {
		return nil
	}
	return store.StampWorkItem(ctx, s.cfg.DB, kind, id, project, stamps, s.now())
}

// stampFailed is the tool error for a write that landed whose stamp did not
// (the F9 shape: never a success payload with an error inside): the write is
// named, so the caller neither retries a create nor believes the item carries
// a warning it does not.
func stampFailed(what string, err error) error {
	return fmt.Errorf("%s, but its provenance stamp could not be recorded: %w", what, err)
}

// shownStamps narrows an item's stamps to the ones its answer names: never
// the item's own project, and only projects the caller's View may list -- a
// restricted account reaches open projects alone and is never told another
// project's name. withheld counts the rest.
func shownStamps(ctx context.Context, stamps []core.Stamp, project string) (shown []core.Stamp, withheld int) {
	v, _ := access.FromContext(ctx)
	for _, st := range core.StampsShown(stamps, project) {
		if !v.Listed(st.Project) {
			withheld++
			continue
		}
		shown = append(shown, st)
	}
	return shown, withheld
}

// putStamps adds an item's stamps to its read answer (shownStamps).
func putStamps(ctx context.Context, out map[string]any, stamps []core.Stamp, project string) {
	shown, withheld := shownStamps(ctx, stamps, project)
	if len(shown) > 0 {
		out[mayContainKey] = shown
	}
	if withheld > 0 {
		out[mayContainWithheldKey] = withheld
	}
}
