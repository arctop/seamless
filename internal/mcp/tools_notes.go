package mcp

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/plans"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/validate"
)

func notesCreateTool() mcp.Tool {
	return mcp.NewTool("notes_create", hintAdd(),
		mcp.WithDescription("Create a work note -- a research finding, decision record, meeting summary, or any artifact long enough to deserve its own file. Auto-tagged created-by:agent. Notes carry the long form; memory_write carries the compact durable knowledge a future session must not miss, so put the write-up here and the one-line lesson there rather than duplicating either. Pass plan=<slug> when the note is a plan's narrative or supporting context, so it joins that plan's composition beside its tasks. Do not use this for what the repo, AGENTS.md/CLAUDE.md, or the current conversation already records, and use notes_append to extend an existing note rather than creating a near-duplicate of it."),
		mcp.WithString("title", mcp.Required(), mcp.Description("note title")),
		mcp.WithString("body", mcp.Required(), mcp.Description("markdown body (aliases: content, text)")),
		mcp.WithString("description", mcp.Description("optional one-line summary")),
		mcp.WithString("project", mcp.Description(writeProjectArgDesc)),
		mcp.WithArray("tags", mcp.WithStringItems(), mcp.Description("tags (a comma-separated string is also accepted)")),
		mcp.WithString("plan", mcp.Description("optional plan slug (plan:<slug> convention): tags this note into that plan's composition so it surfaces on the Plans screen alongside its tasks_add plan=<slug> steps. Use it whenever this note is a plan's narrative or supporting context.")),
		mcp.WithString("source_url", mcp.Description("optional source URL")),
	)
}

func (s *Server) handleNotesCreate(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	title := argString(req, "title")
	body := argRaw(req, "body")
	if title == "" || strings.TrimSpace(body) == "" {
		return errResult("notes_create", errors.New("title and body are required"))
	}
	if err := validate.Title(title); err != nil {
		return errResult("notes_create", err)
	}
	project, err := s.resolveWriteScope(ctx, argString(req, "project"))
	if err != nil {
		return errResult("notes_create", err)
	}
	id, err := core.NewID()
	if err != nil {
		return errResult("notes_create", err)
	}
	now := time.Now().UTC()
	tags := appendUnique(argStrings(req, "tags"), "created-by:agent")
	// A plan slug carries the note into the plan:<slug> composition -- the same
	// key tasks_add plan= writes -- so agents attach a plan's narrative without
	// having to hand-type the tag prefix.
	if plan := argString(req, "plan"); plan != "" {
		tags = appendUnique(tags, plans.SlugTag(plan))
	}
	note := core.Note{
		ID: id, Title: title, Slug: core.Slugify(title), Description: argString(req, "description"),
		Project: project, Body: body, Tags: tags,
		SourceURL: argString(req, "source_url"), Model: s.boundSessionModel(ctx),
		Created: now, Updated: now,
	}
	written, err := s.cfg.Files.WriteNote(ctx, note)
	if err != nil {
		return errResult("notes_create", err)
	}
	s.record(ctx, core.EventNoteWritten, s.boundSession(ctx), project, written.ID, map[string]any{"title": title})
	out := map[string]any{"id": written.ID, "slug": written.Slug, "title": title, "project": project}
	if plan := plans.SlugFromTags(written.Tags); plan != "" {
		out["plan"] = plan
	}
	return jsonResult(out)
}

func notesReadTool() mcp.Tool {
	return mcp.NewTool("notes_read", hintRead(),
		mcp.WithDescription("Read a note by id, or by slug within the current project (falling back to a global note of the same slug). A plan's slug is not a note's: a miss on one names the plan's narrative note."),
		mcp.WithString("id", mcp.Description("note id (ULID); pass exactly one of id or slug")),
		mcp.WithString("slug", mcp.Description("note slug, as briefings, plan compositions, and notes_create responses name notes (alias: name)")),
		mcp.WithString("project", mcp.Description("project slug for the slug lookup; defaults to the bound session's project")),
	)
}

func (s *Server) handleNotesRead(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, slug := argString(req, "id"), argString(req, "slug")
	if id != "" && slug != "" {
		return errResult("notes_read", errors.New("pass exactly one of id or slug"))
	}
	var note core.Note
	var err error
	if slug != "" {
		note, err = s.loadNoteBySlug(ctx, req, slug)
	} else {
		if id == "" {
			return errResult("notes_read", errors.New("id or slug is required (notes_read reads one note by ULID or exact slug; to search text use recall)"))
		}
		note, err = s.loadNote(ctx, id)
		if err == nil {
			// The by-id path resolves no scope -- a note id is globally unique --
			// so the fence is applied after the load, on the project the loaded
			// note names. Nothing of it reaches the caller before that answer.
			err = s.fenceRead(ctx, note.Project)
		}
	}
	if err != nil {
		return errResult("notes_read", err)
	}
	// The note-side twin of memory.read, and a demand signal in its own right: a
	// note nobody ever reads back is the note the gardener should be asking about,
	// and only a durable domain event can say so after the session that read it is
	// gone. Recorded after the fence, so a refused read leaves no trace of the
	// project it was refused on.
	s.record(ctx, core.EventNoteRead, s.boundSession(ctx), note.Project, note.ID,
		map[string]any{"slug": note.Slug})
	out := map[string]any{
		"id": note.ID, "title": note.Title, "slug": note.Slug, "description": note.Description,
		"project": note.Project, "body": note.Body, "tags": note.Tags, "source_url": note.SourceURL,
		// The caller's own handle on what it just read, not authored text: it is
		// what notes_update's expect_hash compares against, so withholding it would
		// leave a permitted writer unable to write safely.
		"content_hash": note.ContentHash,
	}
	if note.Model != "" {
		out["model"] = note.Model
	}
	if note.Favorite {
		out["favorite"] = true
	}
	return jsonResult(out)
}

func notesUpdateTool() mcp.Tool {
	return mcp.NewTool("notes_update", hintOverwrite(),
		mcp.WithDescription("Update a note's fields by id (title, description, body, project, tags). Omitted fields are untouched; the slug and id stay stable. body replaces the WHOLE body, so pass expect_hash (notes_read returns it) to have the write refused rather than silently overwriting an edit that landed after you read the note -- and use notes_append when you only mean to add to it. Tags come in three flavors: tags replaces the whole set, while tags_add and tags_remove edit it in place; prefer add/remove, since a replace discards whatever another agent tagged in between and tags_remove is the only way to clear a tag at all."),
		mcp.WithString("id", mcp.Required(), mcp.Description("note id (ULID)")),
		mcp.WithString("title", mcp.Description("new title")),
		mcp.WithString("description", mcp.Description("new description")),
		mcp.WithString("body", mcp.Description("new body, replacing the whole body (aliases: content, text)")),
		mcp.WithString("project", mcp.Description("new project slug (\"\" or \"global\" = global scope)")),
		mcp.WithArray("tags", mcp.WithStringItems(), mcp.Description("tags, replacing all (a comma-separated string is also accepted); an empty list is read as absent and leaves the tags untouched -- to drop a tag use tags_remove")),
		mcp.WithArray("tags_add", mcp.WithStringItems(), mcp.Description("tags to add, leaving the rest in place; a tag already on the note is not duplicated. Applied after tags.")),
		mcp.WithArray("tags_remove", mcp.WithStringItems(), mcp.Description("tags to drop, matched exactly; a tag the note does not carry is ignored. Applied last, so it also removes what tags/tags_add just set.")),
		withExpectHashParam(),
	)
}

func (s *Server) handleNotesUpdate(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	idx, err := s.noteIndexByID(ctx, argString(req, "id"))
	if err != nil {
		return errResult("notes_update", err)
	}
	// Parsed before the lock is taken: none of it needs the file, and a usage
	// error (a bad title, an unusable project slug, no field at all) should not
	// make a concurrent writer wait on this call's mistake.
	patch, err := parseNoteUpdate(req)
	if err != nil {
		return errResult("notes_update", err)
	}
	// A project change moves the file, so the destination is locked alongside the
	// source: the move writes one path and removes the other, and both halves must
	// be serialized against anything else touching either. MutatePaths sorts and
	// dedupes, so a "move" naming the note's current project locks one path rather
	// than deadlocking on two spellings of it. The destination is keyed by the
	// index row's slug, which the indexer parsed out of this very file and which
	// notes_update never changes -- the path locked here is the path written below.
	paths := []string{idx.FilePath}
	if patch.project != nil {
		paths = append(paths, files.NoteRelPath(*patch.project, idx.Slug))
	}
	var written core.Note
	err = s.cfg.Files.MutatePaths(ctx, paths, func(ctx context.Context) error {
		// The read is INSIDE the lock. Reading it outside was the lost update: two
		// updaters both loaded the same starting content and the second rename won,
		// erasing the first with no error anywhere.
		note, rerr := s.cfg.Files.Store().ReadNote(idx.FilePath)
		if rerr != nil {
			return rerr
		}
		// A note id is globally unique, so nothing here resolved a write scope: the
		// fence is applied post-load, on the project the loaded note names. Without
		// it any caller holding a ULID edits a sealed project's note.
		if err := s.fenceWrite(ctx, note.Project); err != nil {
			return err
		}
		// Checked against the FILE just read under this lock, never the index row
		// (the watcher's debounce would let a stale hash pass), and enforced whether
		// or not a body was sent. A full-body replace is what the precondition is
		// FOR, but a caller that states one and gets it silently ignored is worse
		// off than one that gets refused: a metadata edit computed from a note
		// somebody has since rewritten is the same stale-context write, and the
		// caller already told us how to detect it.
		if err := checkExpectHash(req, "notes_update", note.ContentHash); err != nil {
			return err
		}
		oldProject := note.Project
		s.applyNoteUpdate(ctx, &note, patch)
		note.Updated = time.Now().UTC()

		// The slug is id-addressed and stays stable; a project change moves the file.
		// Refuse when a different note already owns the target path (the UNIQUE
		// file_path index would reject it after the file was already clobbered).
		if note.Project != oldProject {
			// A move is a read of the old project plus a write into the new one, so it
			// needs both answers rather than the edit fence alone. That is what keeps
			// the inbound asymmetry from becoming an exit: an outside caller may edit a
			// confidential note in place (inbound is deliberately open), but carrying
			// it out of the fence is precisely the outbound leak confidential exists to
			// stop.
			if err := s.fenceRead(ctx, oldProject); err != nil {
				return err
			}
			if err := s.fenceWrite(ctx, note.Project); err != nil {
				return err
			}
			if other, ok, oerr := store.NoteBySlug(ctx, s.cfg.DB, note.Project, note.Slug); oerr != nil {
				return oerr
			} else if ok && other.ID != note.ID {
				return fmt.Errorf("a different note with slug %q already exists in project %q", note.Slug, note.Project)
			}
		}
		// Write the new file BEFORE removing the old one: the index row is keyed by
		// id (the write repoints its file_path), so a failed write leaves the note
		// intact at its old path instead of deleting it outright.
		w, werr := s.cfg.Files.WriteNote(ctx, note)
		if werr != nil {
			return werr
		}
		written = w
		if note.Project != oldProject {
			if err := s.cfg.Files.Remove(ctx, files.NoteRelPath(oldProject, note.Slug)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return errResult("notes_update", err)
	}
	// Recorded against the project the note now sits in: after a move, that is
	// where the change landed, and an event filed under the old project would tell
	// the console the wrong story about both.
	s.record(ctx, core.EventNoteWritten, s.boundSession(ctx), written.Project, written.ID,
		map[string]any{"title": written.Title, "updated": true})
	// A permitted write into a confidential project must not answer with that
	// project's text. The title leaks less than a task body does, but it is the
	// same shape: an outside caller holding a ULID reading authored content
	// through an edit it is entitled to make. Judged against the project the note
	// now sits in, so a move INTO the fence lands under it immediately.
	return jsonResult(s.withholdContent(ctx,
		written.Project, map[string]any{"id": written.ID, "title": written.Title}, noteContentFields))
}

// noteUpdatePatch is a parsed, validated notes_update request: every field the
// caller asked to change, resolved OUTSIDE the mutation lock so the lock covers
// only the read-modify-write. A nil pointer means the parameter was absent,
// which is why these are pointers rather than values -- argPresent is what tells
// "leave this alone" from "set this to empty", and collapsing the two would make
// every omitted field a clear.
type noteUpdatePatch struct {
	title       *string
	description *string
	body        *string
	project     *string
	tags        []string // full replace; meaningful only when tagsSet
	tagsSet     bool
	tagsAdd     []string
	tagsRemove  []string
}

// parseNoteUpdate reads and validates the mutable fields of a notes_update call.
func parseNoteUpdate(req mcp.CallToolRequest) (noteUpdatePatch, error) {
	var p noteUpdatePatch
	if argPresent(req, "title") {
		title := argString(req, "title")
		if err := validate.Title(title); err != nil {
			return noteUpdatePatch{}, err
		}
		p.title = &title
	}
	if argPresent(req, "description") {
		description := argString(req, "description")
		p.description = &description
	}
	if argPresent(req, "body") {
		body := argRaw(req, "body")
		p.body = &body
	}
	if argPresent(req, "project") {
		project, err := validateProjectArg(argString(req, "project"))
		if err != nil {
			return noteUpdatePatch{}, err
		}
		p.project = &project
	}
	if argPresent(req, "tags") {
		p.tags, p.tagsSet = argStrings(req, "tags"), true
	}
	p.tagsAdd, p.tagsRemove = argStrings(req, "tags_add"), argStrings(req, "tags_remove")
	if p.empty() {
		return noteUpdatePatch{}, errors.New("provide at least one field to update")
	}
	return p, nil
}

// empty reports whether the call asked for no change at all. expect_hash alone
// is not a change: it is a precondition ON one, so a call carrying only that has
// still not said what to write.
func (p noteUpdatePatch) empty() bool {
	return p.title == nil && p.description == nil && p.body == nil && p.project == nil &&
		!p.tagsSet && len(p.tagsAdd) == 0 && len(p.tagsRemove) == 0
}

// applyNoteUpdate applies a parsed patch to the note read under the mutation
// lock. It is a method because the body case needs the bound session's model.
func (s *Server) applyNoteUpdate(ctx context.Context, note *core.Note, p noteUpdatePatch) {
	if p.title != nil {
		note.Title = *p.title
	}
	if p.description != nil {
		note.Description = *p.description
	}
	if p.body != nil {
		note.Body = *p.body
		// A replaced body is new content: re-attribute it to the current model
		// when known (an unknown model keeps the prior attribution). Metadata-only
		// updates leave the producer untouched.
		if model := s.boundSessionModel(ctx); model != "" {
			note.Model = model
		}
	}
	if p.project != nil {
		note.Project = *p.project
	}
	// The three tag arguments compose in ONE fixed order -- full replace, then
	// adds, then removes -- so a call carrying more than one has a single meaning
	// instead of one that depends on which the handler happened to read first.
	// Removal comes last because it is the only operation that can express an
	// empty tag set: validateMiddleware drops an empty array as absent, so `tags:
	// []` cannot clear anything and never could.
	if p.tagsSet {
		note.Tags = p.tags
	}
	for _, t := range p.tagsAdd {
		note.Tags = appendUnique(note.Tags, t)
	}
	if len(p.tagsRemove) > 0 {
		note.Tags = slices.DeleteFunc(note.Tags, func(t string) bool {
			return slices.Contains(p.tagsRemove, t)
		})
	}
}

func notesAppendTool() mcp.Tool {
	return mcp.NewTool("notes_append", hintAdd(),
		mcp.WithDescription("Append a UTC-timestamped line to an existing note's body, by id. Use it when a note is already the right home for what you learned -- a running investigation log, a decision record gaining one more data point -- so the note keeps its id, slug, and place in any plan composition instead of fragmenting into near-duplicates. Appending only ever adds: use notes_update to correct or restructure what is already there, and notes_create when the finding deserves an artifact of its own. Needs the note's id (ULID), which notes_create returns and briefings and plan compositions carry; notes_read resolves one from a slug."),
		mcp.WithString("id", mcp.Required(), mcp.Description("note id (ULID)")),
		mcp.WithString("body", mcp.Required(), mcp.Description("text to append (aliases: content, text)")),
	)
}

func (s *Server) handleNotesAppend(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	text := argRaw(req, "body")
	if strings.TrimSpace(text) == "" {
		return errResult("notes_append", errors.New("a non-empty body is required (aliases: content, text)"))
	}
	idx, err := s.noteIndexByID(ctx, argString(req, "id"))
	if err != nil {
		return errResult("notes_append", err)
	}
	// The whole read-modify-write runs under the file's lock. An append is the
	// shape the lost update hurt most: two agents adding a line each both read the
	// same body, and the second write dropped the first line entirely -- silently,
	// since the index upsert is by id and the file_path constraint stayed happy.
	written, err := s.cfg.Files.MutateNote(ctx, idx.FilePath, func(ctx context.Context, note core.Note) (core.Note, error) {
		// The fence is judged on the project the FILE names, and it lives here
		// rather than in the loader: reads and writes cross the fence on different
		// terms, and one check shared with the read path could only encode one of
		// them.
		if err := s.fenceWrite(ctx, note.Project); err != nil {
			return core.Note{}, err
		}
		now := time.Now().UTC()
		note.Body = strings.TrimRight(note.Body, "\n") + "\n\n" + now.Format("2006-01-02 15:04") + " -- " + text + "\n"
		note.Updated = now
		// Appended text is content this model produced, so it re-stamps attribution
		// exactly as a body replace does: leaving the old value credits the previous
		// model with words it never wrote. An unknown current model keeps the prior
		// attribution -- never erase a known producer with "". SourceSession stays
		// create-only: it is provenance of the note's existence, not of its text.
		if model := s.boundSessionModel(ctx); model != "" {
			note.Model = model
		}
		return note, nil
	})
	if err != nil {
		return errResult("notes_append", err)
	}
	s.record(ctx, core.EventNoteWritten, s.boundSession(ctx), written.Project, written.ID,
		map[string]any{"slug": written.Slug, "appended": true})
	// The title is the note's, not the caller's: an append that echoed it back
	// would be notes_read by another name for anyone outside the fence.
	return jsonResult(s.withholdContent(ctx,
		written.Project, map[string]any{"id": written.ID, "title": written.Title}, noteContentFields))
}

func notesDeleteTool() mcp.Tool {
	return mcp.NewTool("notes_delete", hintOverwrite(),
		mcp.WithDescription("Delete a note by id: the markdown file leaves the disk and its index row goes with it. Permanent, and it leaves no pointer behind, so reserve it for notes that should never have existed -- a duplicate, a write into the wrong project, an agent's own scratch. To fix a note's content use notes_update, and to add to it notes_append; neither loses the artifact. A note tagged into a plan (plan:<slug>) is that plan's narrative for whoever inherits it, so read it with notes_read before deciding it is disposable."),
		mcp.WithString("id", mcp.Required(), mcp.Description("note id (ULID)")),
	)
}

func (s *Server) handleNotesDelete(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id := argString(req, "id")
	if id == "" {
		return errResult("notes_delete", errors.New("id is required"))
	}
	idx, ok, err := store.NoteByID(ctx, s.cfg.DB, id)
	if err != nil {
		return errResult("notes_delete", err)
	}
	if !ok {
		return errResult("notes_delete", fmt.Errorf("no note with id %q", id))
	}
	// The most consequential of the three: a delete leaves no pointer behind, so
	// an unfenced by-id path let an outside caller destroy a sealed project's note.
	if err := s.fenceWrite(ctx, idx.Project); err != nil {
		return errResult("notes_delete", err)
	}
	if err := s.cfg.Files.Remove(ctx, idx.FilePath); err != nil {
		return errResult("notes_delete", err)
	}
	// idx.Project, not any scope the caller named: a delete is by globally-unique
	// id, so the only true answer to "where did this land" is the project the note
	// actually sat in, and that is the project whose feed and counts just changed.
	s.record(ctx, core.EventNoteWritten, s.boundSession(ctx), idx.Project, idx.ID,
		map[string]any{"slug": idx.Slug, "deleted": true})
	return jsonResult(map[string]any{"status": "deleted", "id": id})
}

// noteContentFields is the authored text notes_update and notes_append answer
// with, and therefore what withholdContent removes for a caller that may write
// the note's project but not read it. Only the id survives -- it is the caller's
// own handle, and without it the caller could not act on what it just wrote.
var noteContentFields = []string{"title"}

// noteIndexByID resolves a note id to its INDEX row: identity, project, and the
// file path -- no body. The mutating handlers need the path BEFORE they can take
// that file's lock, which is why the lookup and the file read are two steps for
// them; the row is a mirror, so nothing decided inside the lock (the fence, the
// precondition, the content) may be read from it.
func (s *Server) noteIndexByID(ctx context.Context, id string) (core.Note, error) {
	if id == "" {
		return core.Note{}, errors.New("id is required")
	}
	idx, ok, err := store.NoteByID(ctx, s.cfg.DB, id)
	if err != nil {
		return core.Note{}, err
	}
	if !ok {
		return core.Note{}, fmt.Errorf("no note with id %q; read by slug=<slug>, or use recall to search by text", id)
	}
	return idx, nil
}

// loadNote resolves a note id to its full on-disk content, for the READ path.
// A mutation must not use it: the read it performs is outside the file's
// mutation lock, so anything it loaded is already stale by the time a write
// could act on it. Pair noteIndexByID with files.Manager.MutateNote instead.
func (s *Server) loadNote(ctx context.Context, id string) (core.Note, error) {
	idx, err := s.noteIndexByID(ctx, id)
	if err != nil {
		return core.Note{}, err
	}
	return s.cfg.Files.Store().ReadNote(idx.FilePath)
}

// loadNoteBySlug resolves a note slug to its full on-disk content, searching the
// request's read scope first and falling back to the global scope -- the same
// resolution memory_read applies to names, because briefings, plan compositions,
// and notes_create responses all name notes by slug.
func (s *Server) loadNoteBySlug(ctx context.Context, req mcp.CallToolRequest, slug string) (core.Note, error) {
	slug = core.Slugify(slug)
	project, err := s.resolveReadScope(ctx, argString(req, "project"))
	if err != nil {
		return core.Note{}, err
	}
	idx, ok, err := store.NoteBySlug(ctx, s.cfg.DB, project, slug)
	if err != nil {
		return core.Note{}, err
	}
	if !ok && project != "" {
		// The fallback resolves no scope of its own, so a sealed session would
		// otherwise read a global note its fence had removed from its world.
		global, gerr := s.canReadGlobal(ctx)
		if gerr != nil {
			return core.Note{}, gerr
		}
		if global {
			idx, ok, err = store.NoteBySlug(ctx, s.cfg.DB, "", slug)
			if err != nil {
				return core.Note{}, err
			}
		}
	}
	if !ok {
		return core.Note{}, s.noteReadMiss(ctx, project, slug)
	}
	return s.cfg.Files.Store().ReadNote(idx.FilePath)
}

// noteReadMiss explains a by-slug notes_read miss, the twin of memoryReadMiss:
// a slug that names a note in other projects the caller may read gets those
// projects named. Failing that, a PLAN's slug -- which agents hold from
// briefings and tasks, and try as a note slug -- gets the plan's narrative
// note named, when the caller may read its project.
func (s *Server) noteReadMiss(ctx context.Context, project, slug string) error {
	help, err := s.noteReadMissHint(ctx, project, slug)
	if err != nil {
		// The miss is already decided; a failed hint lookup must not turn it into
		// a different error, so the tail falls back to the generic advice.
		s.logger.Warn("mcp: notes_read miss hint", "slug", slug, "error", err)
	}
	if help == "" {
		return s.scopedNotFound(ctx, "note", project, slug, noteReadMissHelp, noteReadMissHelpSealed)
	}
	return s.scopedNotFound(ctx, "note", project, slug, help, help)
}

// noteReadMissHint is noteReadMiss's pointer, or "" when there is none to give.
func (s *Server) noteReadMissHint(ctx context.Context, project, slug string) (string, error) {
	scopes, err := store.ProjectsWithNoteSlug(ctx, s.cfg.DB, slug)
	if err != nil {
		return "", err
	}
	elsewhere, err := s.readableElsewhere(ctx, project, scopes)
	if err != nil {
		return "", err
	}
	if len(elsewhere) > 0 {
		return elsewhereHelp(elsewhere), nil
	}
	primary, _, ok, err := plans.Composition(ctx, s.cfg.DB, slug)
	if err != nil || !ok {
		return "", err
	}
	readable, err := store.CanRead(ctx, s.cfg.DB, s.callerScope(ctx), primary.Project)
	if err != nil || !readable {
		return "", err
	}
	arg := "slug=" + primary.Slug
	if primary.Project != "" && primary.Project != project {
		arg += " project=" + primary.Project
	}
	return fmt.Sprintf("; that is a plan's slug, not a note's: read the plan's narrative with %s, and its steps with tasks_list plan=%s",
		arg, slug), nil
}

// The by-name miss tails for notes_read, paired like the memory ones: the plain
// form, and the same advice minus the global scope a sealed session cannot reach.
const (
	noteReadMissHelp       = "; check the slug, pass project=<slug> or project=global, read by id=<ULID>, or use recall to search by text"
	noteReadMissHelpSealed = "; check the slug, read by id=<ULID>, or use recall to search by text"
)
