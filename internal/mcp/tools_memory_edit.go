package mcp

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/arctop/seamless/internal/core"
)

// editVsSupersede is the lifecycle boundary both edit tools police, in one place
// so the two descriptions cannot drift into different rules.
//
// It matters because a cheap in-place edit is exactly the tool an agent will
// reach for to change what a memory CLAIMS, and that would quietly rewrite the
// corpus's history: a superseded memory leaves the indexes but stays readable
// pointing at its replacement, which is how a later reader learns the thing was
// reconsidered rather than that it was never believed. An edit leaves no such
// trace, so it must stay confined to the changes that carry no new claim.
const editVsSupersede = "Use this for corrections that do not change what the item CLAIMS: typos, broken " +
	"formatting, a stale path or command, a stage's Status flip, metadata. If the MEANING changes -- the " +
	"conclusion is now different, the advice reversed -- that is a new memory: use memory_write with " +
	"supersedes, which retires the old one into readable history instead of erasing what it used to say."

// memoryEditContentFields is what memory_edit withholds from a caller that may
// write the target project but not READ it: the diff, which is the memory's own
// authored text quoted back with its surrounding context lines. Returning it
// would make memory_edit a second memory_read for anyone outside the fence --
// one throwaway edit yields several lines of the body, and repeating it walks
// the whole memory out. id, name, project and content_hash stay: they are the
// caller's own handles on the write it just made, not content.
//
// It is the guard for the SHAPE rather than a live path today: this tool
// addresses by name+project, so resolveReadScope's fence has already refused an
// outside caller before any write happens. That stops holding the moment
// memory_edit grows id addressing the way notes_edit has -- a ULID resolves no
// scope, fenceWrite lets an outside write into a confidential project land on
// purpose, and the diff would then be the read that same fence refuses.
var memoryEditContentFields = []string{"diff"}

func memoryEditTool() mcp.Tool {
	return mcp.NewTool("memory_edit", hintOverwrite(),
		mcp.WithDescription("Edit an existing memory in place with exact search/replace, instead of resending its "+
			"whole body through memory_write. Each edit's old_string must match the current body exactly and "+
			"uniquely (or pass replace_all); all edits apply together or none do, so a failed match changes "+
			"nothing. It is also the only way to change a memory's description or tags on their own: send "+
			"description/tags_add/tags_remove with no edits and the body is left untouched (memory_write requires "+
			"a body, so there was no metadata-only path before this), and tags_remove is the only way to clear a "+
			"tag at all. Returns a unified diff of what landed plus the new content_hash. "+editVsSupersede),
		mcp.WithString("name", mcp.Required(), mcp.Description("memory name (kebab-case, as memory_read takes it)")),
		withEditsParam(false),
		mcp.WithString("project", mcp.Description("project slug; defaults to the bound/ambient session's project, then global. Pass project=global to target a global memory.")),
		withExpectHashParam(),
		mcp.WithString("description", mcp.Description("replace the one-line description (<=150 chars -- the only text shown in indexes); omit to leave it untouched")),
		mcp.WithArray("tags_add", mcp.WithStringItems(), mcp.Description("tags to add, leaving the rest in place (a comma-separated string is also accepted)")),
		mcp.WithArray("tags_remove", mcp.WithStringItems(), mcp.Description("tags to remove, leaving the rest in place; this is how a tag gets cleared (a comma-separated string is also accepted)")),
	)
}

// handleMemoryEdit applies the edits[] search/replace engine to one memory's
// body under the file's mutation lock, together with the metadata this is the
// only tool able to change.
func (s *Server) handleMemoryEdit(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name, err := memoryName(argString(req, "name"))
	if err != nil {
		return errResult("memory_edit", err)
	}
	if name == "" {
		return errResult("memory_edit", errors.New("name is required"))
	}
	// Parsed only when sent, so the guard below stays real rather than
	// decorative. edits is Required in the schema today (validateMiddleware is
	// what enforces that, not mcp.Required), but description/tags_add/tags_remove
	// are a complete change on their own: if edits is ever relaxed to optional, a
	// metadata-only call must keep working and a call carrying nothing at all must
	// still be told so instead of reporting a write it never made.
	var edits []edit
	if argPresent(req, "edits") {
		edits, err = parseEdits(req)
		if err != nil {
			return errResult("memory_edit", err)
		}
	}
	metadata := argPresent(req, "description") || argPresent(req, "tags_add") || argPresent(req, "tags_remove")
	if len(edits) == 0 && !metadata {
		return errResult("memory_edit", errors.New(
			"nothing to change: pass edits, and/or description, tags_add, tags_remove"))
	}
	project, err := s.resolveReadScope(ctx, argString(req, "project"))
	if err != nil {
		return errResult("memory_edit", err)
	}
	// resolveReadScope plus the global fallback, not resolveWriteScope, for the
	// same reason memory_append resolves this way: an edit targets an EXISTING
	// memory, so this is a LOOKUP scope, and a project-bound session must still be
	// able to correct a typo in a global memory it can already read. AGENTS.md's
	// "a durable create uses resolveWriteScope" rule is about a CREATE with
	// nothing to infer silently landing in global -- nothing is created here, and
	// the write is still judged against the scope it lands in, which is what
	// fenceWrite on idx.Project (the project the memory actually sits in, not the
	// one that was searched) does below.
	idx, found, err := s.resolveMemory(ctx, project, name, true)
	if err != nil {
		return errResult("memory_edit", err)
	}
	if !found {
		return errResult("memory_edit", s.scopedNotFound(ctx, "memory", project, name,
			memoryAppendMissHelp, memoryAppendMissHelpSealed))
	}
	if err := s.fenceWrite(ctx, idx.Project); err != nil {
		return errResult("memory_edit", err)
	}

	now := time.Now().UTC()
	screen := s.screeningOn(ctx)
	var screenSession string
	var tainted bool
	if screen {
		screenSession = s.boundSession(ctx)
		tainted = s.sessionTainted(ctx, screenSession)
	}
	var jd judged
	var oldBody, newBody string
	// MutateMemory hands over the memory as the FILE carries it and writes back
	// whatever this returns, so the edit mutates `cur` in place. That is what
	// preserves everything the tool has no argument for -- the favorite star, the
	// unknown frontmatter keys Extra round-trips, created/valid_from, kind, name,
	// project, source_session. Building a fresh core.Memory here would erase all
	// of them while reporting success, which is the failure memory_write's
	// whole-struct render had to be taught out of.
	mem, err := s.cfg.Files.MutateMemory(ctx, idx.FilePath, func(ctx context.Context, cur core.Memory) (core.Memory, error) {
		// Against the FILE's hash, inside the lock. The watcher re-indexes on a
		// debounce, so the index row happily reports the hash the caller is holding
		// for exactly the window an owner's out-of-band save opened -- the window
		// this precondition exists to catch.
		if err := checkExpectHash(req, "memory_edit", cur.ContentHash); err != nil {
			return core.Memory{}, err
		}
		oldBody = cur.Body
		before := cur
		if len(edits) > 0 {
			edited, aerr := applyEdits(cur.Body, edits)
			if aerr != nil {
				// Nothing is written: returning the error aborts the mutation before
				// WriteMemory, which is what makes a failed match leave the file
				// byte-identical rather than partially applied.
				return core.Memory{}, aerr
			}
			cur.Body = edited
		}
		newBody = cur.Body
		if argPresent(req, "description") {
			// Capped on a word boundary exactly as memory_write caps it: the
			// description is the only text every index and briefing shows, so it must
			// never end mid-word, and one truncation rule keeps an edited description
			// identical to a written one.
			cur.Description = core.TruncateWords(argString(req, "description"), maxDescriptionRunes)
		}
		// Add then remove, both against the tags the file carries right now. This
		// pair is what finally makes CLEARING a tag expressible: memory_write's
		// `tags` replaces the whole set, and validateMiddleware drops an empty array
		// as absent, so "no tags" cannot be said through it at all. Remove runs
		// last, so a caller naming the same tag in both lists ends up without it --
		// the only reading of that pair that is not a coin flip.
		if argPresent(req, "tags_add") {
			for _, t := range argStrings(req, "tags_add") {
				cur.Tags = appendUnique(cur.Tags, t)
			}
		}
		if argPresent(req, "tags_remove") {
			drop := argStrings(req, "tags_remove")
			cur.Tags = slices.DeleteFunc(cur.Tags, func(t string) bool { return slices.Contains(drop, t) })
		}
		cur.Updated = now
		// Model attribution follows the CONTENT, the rule memory_write and
		// memory_append both apply: the replacement prose was produced by the model
		// that edited it, and leaving the old value credits a model that never wrote
		// those lines. A metadata-only edit authored no prose, so it leaves the
		// attribution alone; an unknown current model never erases a known producer
		// with "". SourceSession is deliberately untouched -- it records who CREATED
		// the memory, and a create happens once.
		if newBody != oldBody {
			if model := s.boundSessionModel(ctx); model != "" {
				cur.Model = model
			}
		}
		if screen {
			if herr := s.judgeMemoryWrite(ctx, "memory_edit", cur, before, core.Memory{}, screenSession, tainted, &jd); herr != nil {
				return core.Memory{}, herr
			}
		}
		return cur, nil
	})
	if held, ok := asHeld(err); ok {
		s.recordScreened(ctx, "memory_edit", core.ItemKindMemory, held.hold.ItemID, held.hold.Project, screenSession,
			held.hold.MemoryKind, jd)
		return heldResult(held.hold, map[string]any{"id": held.hold.ItemID, "name": name, "project": held.hold.Project})
	}
	if err != nil {
		return errResult("memory_edit", err)
	}
	if screen {
		s.recordScreened(ctx, "memory_edit", core.ItemKindMemory, mem.ID, mem.Project, screenSession, mem.Kind, jd)
	}
	diff := unifiedDiff(oldBody, newBody)
	// The diff rides in the payload so the console can render edit history later
	// from the event log alone, without re-reading a file that has since moved on.
	s.record(ctx, core.EventMemoryWritten, s.boundSession(ctx), mem.Project, mem.ID,
		map[string]any{"name": name, "edited": true, "diff": diff})

	// project names the scope the memory actually lives in: the lookup falls back
	// to global, so an edit aimed at a project can land on a global memory and the
	// response must say which one it touched.
	resp := map[string]any{
		"id": mem.ID, "name": name, "project": mem.Project,
		"content_hash": mem.ContentHash,
	}
	if diff != "" {
		resp["diff"] = diff
	}
	// Recomputed from the POST-edit body: an edit that flips a broken Status
	// header into a valid one is the fix, and must not still be nagged about;
	// one that leaves the stage headerless still renders as status unknown.
	if hint := stageHeaderHint(mem.Kind, newBody); hint != "" {
		resp["stage_hint"] = hint
	}
	return jsonResult(s.withholdContent(ctx, mem.Project, resp, memoryEditContentFields))
}
