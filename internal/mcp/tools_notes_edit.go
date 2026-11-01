package mcp

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

func notesEditTool() mcp.Tool {
	return mcp.NewTool("notes_edit", hintOverwrite(),
		mcp.WithDescription("Edit an existing note in place with exact search/replace, by id, instead of resending "+
			"its whole body through notes_update. Each edit's old_string must match the current body exactly and "+
			"uniquely (or pass replace_all); all edits apply together or none do, so a failed match changes "+
			"nothing. This is the tool for correcting or restructuring part of a long note -- notes_append only "+
			"ever adds, and notes_update replaces the whole body. Returns a unified diff of what landed plus the "+
			"new content_hash."),
		mcp.WithString("id", mcp.Required(), mcp.Description("note id (ULID), as notes_create returns and briefings and plan compositions carry")),
		withEditsParam(true),
		withExpectHashParam(),
	)
}

// notesEditContentFields is the authored text notes_edit answers with, and
// therefore what withholdContent removes for a caller that may WRITE the note's
// project but not READ it. It is noteContentFields (the title, which the sibling
// note mutations already withhold) plus the diff, and the diff is the reason this
// list is longer than theirs: a diff is the note's own prose quoted back with
// surrounding context lines, so returning it would make notes_edit a second
// notes_read for anyone outside the fence -- one throwaway edit yields several
// lines of the body, and repeating it walks the whole note out.
//
// id and content_hash deliberately stay: they are the caller's own handles on the
// write it just made, not content, and without the hash a permitted writer could
// not state a precondition on its next write.
var notesEditContentFields = append(slices.Clone(noteContentFields), "diff")

// handleNotesEdit applies the edits[] search/replace engine to one note's body
// under the file's mutation lock.
//
// It carries no metadata parameters, unlike memory_edit. That asymmetry is not an
// omission: notes_update already patches a note's title, description, project and
// tags field-wise, so repeating them here would be a second way to say the same
// thing, with two spellings of the tag-composition order to keep in step. A
// memory has no such tool -- memory_write renders the whole struct -- which is why
// memory_edit had to grow description/tags_add/tags_remove and this one must not.
func (s *Server) handleNotesEdit(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id := argString(req, "id")
	if id == "" {
		return errResult("notes_edit", errors.New("id is required"))
	}
	// Parsed before anything is locked: a malformed edits[] needs neither the index
	// nor the file, and a usage error should not make a concurrent writer wait on
	// this call's mistake.
	edits, err := parseEdits(req)
	if err != nil {
		return errResult("notes_edit", err)
	}
	// The INDEX row, for its project and file path only -- the row is a mirror, so
	// nothing decided inside the lock (the fence's answer aside, the precondition
	// and the content) is read from it. The path is needed before the file's lock
	// can be taken, which is why the lookup and the read are two steps here.
	idx, ok, err := store.NoteByID(ctx, s.cfg.DB, id)
	if err != nil {
		return errResult("notes_edit", err)
	}
	if !ok {
		return errResult("notes_edit", fmt.Errorf(
			"no note with id %q; read by slug=<slug>, or use recall to search by text", id))
	}
	// Post-load, on the project the note actually sits in. A note id is globally
	// unique, so nothing up to here resolved a scope: without this fence any caller
	// holding a ULID edits a sealed project's note. It is the same reasoning
	// notes_update carries, and it is judged on the project rather than on any
	// scope the caller named, because the caller named none.
	if err := s.fenceWrite(ctx, idx.Project); err != nil {
		return errResult("notes_edit", err)
	}

	screen := s.screeningOn(ctx)
	var screenSession string
	if screen {
		screenSession = s.boundSession(ctx)
	}
	var jd judged
	var oldBody, newBody string
	// MutateNote hands over the note as the FILE carries it and writes back
	// whatever this returns, so the edit mutates `cur` in place. That is what
	// preserves everything this tool has no argument for -- the favorite star, the
	// tags, source_url, created, title, slug, project, and the unknown frontmatter
	// keys Extra round-trips. Building a fresh core.Note here would erase all of
	// them while reporting success.
	note, err := s.cfg.Files.MutateNote(ctx, idx.FilePath, func(ctx context.Context, cur core.Note) (core.Note, error) {
		// Against the FILE's hash, inside the lock. The watcher re-indexes on a
		// debounce, so the index row happily reports the hash the caller is holding
		// for exactly the window an owner's out-of-band save opened -- the window
		// this precondition exists to catch.
		if err := checkExpectHash(req, "notes_edit", cur.ContentHash); err != nil {
			return core.Note{}, err
		}
		edited, aerr := applyEdits(cur.Body, edits)
		if aerr != nil {
			// Nothing is written: returning the error aborts the mutation before
			// WriteNote, which is what makes a failed match leave the file
			// byte-identical rather than partially applied.
			return core.Note{}, aerr
		}
		oldBody, newBody = cur.Body, edited
		before := cur
		cur.Body = edited
		cur.Updated = time.Now().UTC()
		// The body changed, so the prose is this model's: leaving the old value
		// credits the previous model with words it never wrote. An unknown current
		// model keeps the prior attribution -- never erase a known producer with "".
		// SourceSession has no note-side field at all, so the model is the whole
		// attribution here.
		if model := s.boundSessionModel(ctx); model != "" {
			cur.Model = model
		}
		if screen {
			if herr := s.judgeNoteWrite(ctx, "notes_edit", cur, before, "", screenSession, &jd); herr != nil {
				return core.Note{}, herr
			}
		}
		return cur, nil
	})
	if held, ok := asHeld(err); ok {
		s.recordScreened(ctx, "notes_edit", core.ItemKindNote, held.hold.ItemID, held.hold.Project, screenSession, "", jd)
		return heldResult(held.hold, map[string]any{"id": held.hold.ItemID})
	}
	if err != nil {
		return errResult("notes_edit", err)
	}
	if screen {
		s.recordScreened(ctx, "notes_edit", core.ItemKindNote, note.ID, note.Project, screenSession, "", jd)
	}

	diff := unifiedDiff(oldBody, newBody)
	// The diff rides in the payload so the console can render edit history later
	// from the event log alone, without re-reading a file that has since moved on.
	// Recorded against the project the note sits in, which is the project whose
	// feed and counts just changed.
	s.record(ctx, core.EventNoteWritten, s.boundSession(ctx), note.Project, note.ID,
		map[string]any{"title": note.Title, "edited": true, "diff": diff})

	resp := map[string]any{
		"id": note.ID, "title": note.Title,
		// The NEW hash: the caller's handle for its next expect_hash, and the
		// authority on what landed (the diff is advisory and may be truncated).
		"content_hash": note.ContentHash,
	}
	if diff != "" {
		resp["diff"] = diff
	}
	return jsonResult(s.withholdContent(ctx, note.Project, resp, notesEditContentFields))
}
