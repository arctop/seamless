package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/arctop/seamless/internal/agentguide"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/lifecycle"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/validate"
)

const maxDescriptionRunes = 150

// memoryWriteContentFields is what memory_write withholds from a caller that may
// write the target project but not read it: the dedup hint, which is a DIFFERENT
// memory's name and description. `updated` deliberately stays -- it reports what
// the caller's own write did (create vs. replace in place), and an agent that
// cannot tell it just overwrote existing knowledge is worse off than the name
// existence it reveals.
var memoryWriteContentFields = []string{"similar"}

func memoryWriteTool() mcp.Tool {
	return mcp.NewTool("memory_write", hintOverwrite(),
		mcp.WithDescription("Create or update a durable memory -- the compact knowledge a future session must not miss (a constraint, gotcha, decision, runbook). Long-form write-ups belong in notes_create; put the one-line lesson here. Writing an existing name updates it in place (its id is stable). On a new name, a semantically similar existing memory is reported as an advisory hint; the write still proceeds. The hint is withheld when the target project's content is fenced from you (see the withheld marker on the response) -- the write lands either way. Pass supersedes to replace a DIFFERENT, now-outdated memory: it is marked invalid and leaves every index (briefing, recall) but stays readable with a pointer here. If superseding fails, the new memory is still written and kept; the error says how to retry."),
		mcp.WithString("name", mcp.Required(), mcp.Description("kebab-case identifier, unique within the project")),
		mcp.WithString("kind", mcp.Required(), enumOf(core.MemoryKinds), mcp.Description("memory kind; "+agentguide.KindDiscriminator+"; "+agentguide.StageContract)),
		mcp.WithString("description", mcp.Required(), mcp.Description("one line, <=150 chars -- the only text shown in indexes")),
		mcp.WithString("body", mcp.Required(), mcp.Description("markdown body (aliases: content, text)")),
		mcp.WithString("project", mcp.Description(writeProjectArgDesc)),
		mcp.WithArray("tags", mcp.WithStringItems(), mcp.Description("tags, replacing all (a comma-separated string is also accepted); omit to leave an existing memory's tags untouched, and note an empty list reads as absent, not as a clear")),
		mcp.WithString("supersedes", mcp.Description("name of an existing memory this one replaces; that memory is marked superseded (invalid) and pointed here")),
	)
}

func (s *Server) handleMemoryWrite(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name, err := memoryName(argString(req, "name"))
	if err != nil {
		return errResult("memory_write", err)
	}
	kindStr := argString(req, "kind")
	desc := argString(req, "description")
	body := argRaw(req, "body")

	if name == "" || kindStr == "" || desc == "" || strings.TrimSpace(body) == "" {
		return errResult("memory_write", errors.New("name, kind, description, and body are required"))
	}
	kind := core.MemoryKind(kindStr)
	if !kind.Valid() {
		return errResult("memory_write", fmt.Errorf("invalid kind %q", kindStr))
	}
	project, err := s.resolveWriteScope(ctx, argString(req, "project"))
	if err != nil {
		return errResult("memory_write", err)
	}
	// Cap on a word boundary with a trailing ellipsis so the stored description
	// -- the only text shown in every index and briefing -- never ends mid-word.
	desc = core.TruncateWords(desc, maxDescriptionRunes)

	// The dedup hint stays OUTSIDE the mutation below, in both directions. It
	// costs an embedding round trip to a provider, so running it under the file's
	// lock would park every other writer of this memory behind a network call for
	// an advisory nicety; and it must describe the corpus as it stood BEFORE this
	// write, because files.WriteMemory embeds synchronously -- asked afterwards it
	// would happily offer the memory we just wrote as its own duplicate. That
	// leaves a lock-free probe as the only way to decide whether to spend it. The
	// probe may disagree with what the lock finds a moment later (a concurrent
	// create), which is why the hint is only reported when the authoritative
	// answer from inside the lock agrees the name was new.
	var similar *map[string]any
	if _, probed, perr := s.resolveMemory(ctx, project, name, false); perr != nil {
		return errResult("memory_write", perr)
	} else if !probed {
		if hint, herr := s.cfg.Retrieve.DedupHint(ctx, project, name, desc); herr == nil && hint != nil {
			similar = &map[string]any{"name": hint.Name, "description": hint.Description, "score": hint.Score}
		}
	}

	now := time.Now().UTC()
	var written core.Memory
	var found bool
	// Resolve, read, build and write as one serialized step. Split apart, two
	// concurrent writers of the same name both saw the same starting file and the
	// second rename won: the first write's body, tags and star were gone with no
	// error anywhere, because the index upsert is keyed by id and the file is
	// replaced wholesale rather than merged. Mutate is not reentrant, but
	// WriteMemory takes no lock of its own, so calling it here is the intended
	// shape rather than a deadlock.
	err = s.cfg.Files.Mutate(ctx, files.MemoryRelPath(project, name), func(ctx context.Context) error {
		existing, ok, err := s.resolveMemory(ctx, project, name, false)
		if err != nil {
			return err
		}
		found = ok

		mem := core.Memory{
			Kind: kind, Name: name, Description: desc, Project: project, Body: body,
			Updated: now, ValidFrom: now, SourceSession: s.boundSession(ctx),
			Model: s.boundSessionModel(ctx),
		}
		if found {
			// Update in place: the ULID and creation provenance are identity and
			// must not change just because the content did.
			mem.ID = existing.ID
			mem.Created = existing.Created
			if !existing.ValidFrom.IsZero() {
				mem.ValidFrom = existing.ValidFrom
			}
			if existing.SourceSession != "" {
				mem.SourceSession = existing.SourceSession
			}
			// Model attribution follows the CONTENT, not creation: a rewrite is new
			// knowledge produced by the current model. Only an unknown current model
			// keeps the prior attribution -- never erase a known producer with "".
			if mem.Model == "" {
				mem.Model = existing.Model
			}
			// Curation the caller did not necessarily send. files.WriteMemory renders
			// the struct as-is and never merges with the file on disk, so every field
			// not carried forward here is erased from the frontmatter: a correction to
			// the body would silently untag a memory, unstar it out of its briefing
			// pin, and drop the unknown keys Extra exists to round-trip. Favorite and
			// Extra have no argument at all; tags carried forward here are what an
			// explicit tags argument overrides further down.
			mem.Tags = existing.Tags
			mem.Favorite = existing.Favorite
			// The index row carries tags and favorite but NOT Extra (core.Memory.Extra
			// is deliberately unmirrored), and frontmatter is the authority for stars
			// anyway, so the file is the real source for all three. This read is inside
			// the lock, which is what makes it the content the write is about to
			// replace rather than a snapshot something else has since moved on from.
			if existing.FilePath != "" {
				onDisk, rerr := s.cfg.Files.Store().ReadMemory(existing.FilePath)
				if rerr != nil {
					// A failed re-read REFUSES the write; it used to degrade to the index
					// values and carry on. Extra is the one field with no second copy
					// anywhere, so degrading rendered the file without the owner's unknown
					// frontmatter keys and destroyed them for good -- while reporting
					// success, which is the shape meta-rule 3 forbids. Serialized, this is
					// no longer a lost race but a real filesystem fault (the file moved,
					// vanished, or became unreadable out of band), so retrying once it
					// clears is the honest instruction. The warn is not a duplicate of the
					// returned error: the error reaches only the calling agent, and a
					// corpus file the daemon cannot read is the owner's to see.
					s.logger.Warn("mcp: memory_write frontmatter preservation",
						"name", name, "project", project, "error", rerr)
					return fmt.Errorf(
						"re-reading %s to preserve its frontmatter failed: %w -- refusing the write rather than dropping the unknown frontmatter keys only the file carries; retry once the file is readable",
						existing.FilePath, rerr)
				}
				mem.Tags, mem.Favorite, mem.Extra = onDisk.Tags, onDisk.Favorite, onDisk.Extra
			}
		} else {
			id, err := core.NewID()
			if err != nil {
				return err
			}
			mem.ID = id
			mem.Created = now
		}
		// Deliberate re-tagging, replacing the whole set. The argPresent guard is the
		// entire contract: a bare `mem.Tags = argStrings(...)` would clear the tags of
		// every caller that omits the argument, which is precisely the silent erasure
		// the preservation above exists to prevent. validateMiddleware drops an empty
		// array as "absent", so clearing tags is deliberately not expressible here --
		// same as notes_update, and the parameter description says so.
		if argPresent(req, "tags") {
			mem.Tags = argStrings(req, "tags")
		}

		w, werr := s.cfg.Files.WriteMemory(ctx, mem)
		if werr != nil {
			return werr
		}
		written = w
		return nil
	})
	if err != nil {
		if errors.Is(err, files.ErrPathOccupied) {
			return errResult("memory_write", fmt.Errorf(
				"name %q is still held by a superseded or archived memory (%w) -- free it with memory_delete or pick a different name", name, err))
		}
		return errResult("memory_write", err)
	}
	s.record(ctx, core.EventMemoryWritten, s.boundSession(ctx), project, written.ID,
		map[string]any{"name": name, "kind": kindStr, "updated": found})

	resp := map[string]any{"id": written.ID, "name": name, "project": project, "updated": found}
	// The probe that earned the hint ran before the lock; report it only if the
	// serialized answer still says this was a create, so a write that raced a
	// concurrent create of the same name does not present a stale "similar".
	if similar != nil && !found {
		resp["similar"] = *similar
	}
	if hint := stageHeaderHint(kind, body); hint != "" {
		resp["stage_hint"] = hint
	}
	// Canonicalized like every other name, so `supersedes: "My Old Note"`
	// resolves to the same memory `memory_write name: "my-old-note"` created.
	supersedes, serr := memoryName(argString(req, "supersedes"))
	if serr != nil {
		return errResult("memory_write", serr)
	}
	if supersedes != "" {
		superseded, serr := s.supersedeMemory(ctx, project, supersedes, written, now)
		if serr != nil {
			// Partial failure: the new memory is kept -- its content is valid
			// knowledge, an update-in-place has no previous body to restore, and
			// re-writing the same name is a lossless in-place update, so fixing
			// the target and retrying is safe. But the supersession did NOT
			// happen, so this must be an explicit tool error: an error field
			// embedded in a success payload reads as success, and the agent
			// would leave the old memory live alongside its replacement.
			return errResult("memory_write", fmt.Errorf(
				"memory %q written and kept (id %s), but superseding %q failed: %w -- %q is STILL ACTIVE; fix the supersedes target and retry (re-writing %q updates it in place)",
				name, written.ID, supersedes, serr, supersedes, name))
		}
		if superseded != "" {
			resp["superseded"] = superseded
		}
	}
	// The dedup hint is another project's memory: its name, its description --
	// the one line every index shows -- and how close the two are. A confidential
	// project takes writes from outside, so without this an outsider could write
	// throwaway memories into it and harvest the descriptions of what is already
	// there, one probe per write. The rest of the payload is the caller's own
	// input plus the id it just minted, so only the hint is withheld.
	return jsonResult(s.withholdContent(ctx, project, resp, memoryWriteContentFields))
}

// supersedeMemory marks the memory named target (in project, falling back to
// global) as superseded by replacement. It returns the superseded memory's name
// (project-qualified) on success, "" when the target IS the replacement (an
// in-place update, nothing to supersede), or an error the caller surfaces as a
// tool error (the write itself is kept). A target already superseded by this
// same replacement reports success, so retrying a memory_write whose supersede
// already landed stays idempotent.
func (s *Server) supersedeMemory(ctx context.Context, project, target string, replacement core.Memory, now time.Time) (string, error) {
	old, found, err := s.resolveMemory(ctx, project, target, true)
	if err != nil {
		return "", err
	}
	if !found {
		// The active index missed: either the name is wrong, or the target is
		// already invalid. Already superseded by this exact replacement is the
		// goal state (a retried call), not a failure.
		prev, ok, perr := s.resolveSupersededMemory(ctx, project, target)
		if perr != nil {
			return "", perr
		}
		if ok && prev.SupersededBy == replacement.ID {
			return lifecycle.MemoryRef(prev.Project, prev.Name), nil
		}
		if ok {
			return "", fmt.Errorf("memory %q is already superseded or archived", target)
		}
		return "", fmt.Errorf("no memory named %q to supersede", target)
	}
	if old.ID == replacement.ID {
		return "", nil // same memory: an in-place update, not a supersession
	}
	// resolveMemory falls back to global, so the target can sit outside the
	// caller's project -- and superseding it is a write there: Supersede rewrites
	// the old file with a tombstone naming the replacement's project and memory,
	// which is a fenced project's vocabulary landing in a scope every project
	// reads.
	if err := s.fenceWrite(ctx, old.Project); err != nil {
		return "", err
	}
	// Index rows carry no body; read the file so the tombstone appends to the
	// real content rather than truncating it. Read and rewrite are one serialized
	// step because Supersede renders the WHOLE file from what this read returned:
	// a memory_append landing between the two would be silently undone by the
	// tombstone write, and losing content while marking a memory invalid is
	// exactly the case where the record must stay complete. Mutate is generic
	// rather than MutateMemory because Supersede owns the write itself.
	var updated core.Memory
	if err := s.cfg.Files.Mutate(ctx, old.FilePath, func(ctx context.Context) error {
		full, rerr := s.cfg.Files.Store().ReadMemory(old.FilePath)
		if rerr != nil {
			return rerr
		}
		u, serr := lifecycle.Supersede(ctx, s.cfg.Files, full, replacement, now)
		if serr != nil {
			return serr
		}
		updated = u
		return nil
	}); err != nil {
		return "", err
	}
	s.record(ctx, core.EventMemorySuperseded, s.boundSession(ctx), updated.Project, updated.ID,
		map[string]any{"name": updated.Name, "superseded_by": replacement.ID})
	return lifecycle.MemoryRef(updated.Project, updated.Name), nil
}

func memoryAppendTool() mcp.Tool {
	return mcp.NewTool("memory_append", hintAdd(),
		mcp.WithDescription("Append markdown to an existing memory's body. The memory keeps its id. To create a new memory, use memory_write."),
		mcp.WithString("name", mcp.Required(), mcp.Description("memory name")),
		mcp.WithString("body", mcp.Required(), mcp.Description("markdown to append (aliases: content, text)")),
		mcp.WithString("project", mcp.Description("project slug; defaults to the bound/ambient session's project, then global. Pass project=global to target a global memory.")),
	)
}

func (s *Server) handleMemoryAppend(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name, err := memoryName(argString(req, "name"))
	if err != nil {
		return errResult("memory_append", err)
	}
	content := argRaw(req, "body")
	if name == "" || strings.TrimSpace(content) == "" {
		return errResult("memory_append", errors.New("name and a non-empty body are required (body aliases: content, text)"))
	}
	project, err := s.resolveReadScope(ctx, argString(req, "project"))
	if err != nil {
		return errResult("memory_append", err)
	}
	// Look up in the project scope, falling back to global (as memory_read does)
	// so an append does not miss a global memory the session is scoped to.
	idx, found, err := s.resolveMemory(ctx, project, name, true)
	if err != nil {
		return errResult("memory_append", err)
	}
	if !found {
		return errResult("memory_append", s.scopedNotFound(ctx, "memory", project, name,
			memoryAppendMissHelp, memoryAppendMissHelpSealed))
	}
	// The lookup falls back to the global scope, so the memory this append is
	// about to rewrite may sit outside the caller's project -- and an append is a
	// durable write, so it is judged as one against the scope it actually landed
	// in, not the scope that was searched.
	if err := s.fenceWrite(ctx, idx.Project); err != nil {
		return errResult("memory_append", err)
	}
	// Read the full memory (index rows have no body) and append, both under the
	// file's lock. An append is the read-modify-write most likely to be raced --
	// two agents adding findings to the same memory is the normal case, not the
	// pathological one -- and unserialized both read the same starting body and
	// the second rename kept only its own addition.
	mem, err := s.cfg.Files.MutateMemory(ctx, idx.FilePath, func(ctx context.Context, cur core.Memory) (core.Memory, error) {
		cur.Body = strings.TrimRight(cur.Body, "\n") + "\n" + content + "\n"
		cur.Updated = time.Now().UTC()
		// Any body change re-stamps the model, the same rule memory_write follows on
		// a rewrite: the appended prose was produced by the model appending it, and
		// leaving the old value credits a model that never wrote those lines. An
		// unknown current model keeps the prior attribution -- never erase a known
		// producer with "". SourceSession is deliberately untouched: it records who
		// created the memory, and a create happens once.
		if model := s.boundSessionModel(ctx); model != "" {
			cur.Model = model
		}
		return cur, nil
	})
	if err != nil {
		return errResult("memory_append", err)
	}
	s.record(ctx, core.EventMemoryWritten, s.boundSession(ctx), mem.Project, mem.ID,
		map[string]any{"name": name, "appended": true})
	resp := map[string]any{"id": mem.ID, "name": name, "status": "appended"}
	// Parsed AFTER the append: if the added lines landed a valid header inside
	// the parse window the stage is fixed and no hint fires; if the stage is
	// still headerless, appending was the wrong tool for a status change.
	if hint := stageHeaderHint(mem.Kind, mem.Body); hint != "" {
		resp["stage_hint"] = hint
	}
	return jsonResult(resp)
}

// stageHeaderHint returns the non-blocking advisory for a stage body whose
// Status header is missing or unrecognized. The write always proceeds -- the
// stage just renders as status unknown and ages out of the briefing instead of
// pinning -- and the hint teaches the header contract while the writing agent
// still has the context to fix the body in-session.
func stageHeaderHint(kind core.MemoryKind, body string) string {
	if kind != core.KindStage {
		return ""
	}
	status, _ := core.ParseStageHeader(body)
	if core.StageStatusLive(status) || status == core.StageStatusDone {
		return ""
	}
	problem := "stage body has no parseable Status header, so the briefing shows it as status unknown and ages it out"
	if status != "" {
		problem = fmt.Sprintf("stage body has unrecognized Status value %q, so the briefing shows it as status unknown and ages it out", status)
	}
	return problem + "; " + agentguide.StageContract
}

func memoryReadTool() mcp.Tool {
	return mcp.NewTool("memory_read", hintRead(),
		mcp.WithDescription("Read a memory by name within the current project (falling back to a global memory of the same name), or directly by id. It reads Seamless memories only -- "+clientMemoryNote+"."),
		mcp.WithString("name", mcp.Description("memory name; pass exactly one of name or id")),
		mcp.WithString("project", mcp.Description("project slug; defaults to the bound session's project")),
		mcp.WithString("id", mcp.Description("memory id (ULID), as carried by events, recall results, and gardener proposals; bypasses name/project resolution")),
	)
}

func (s *Server) handleMemoryRead(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id := argString(req, "id")
	rawName := argString(req, "name")
	if id != "" && rawName != "" {
		return errResult("memory_read", errors.New("pass exactly one of name or id"))
	}
	var idx core.Memory
	if id != "" {
		m, found, err := store.MemoryByID(ctx, s.cfg.DB, id)
		if err != nil {
			return errResult("memory_read", err)
		}
		if !found {
			return errResult("memory_read", fmt.Errorf("no memory with id %q; use recall to search by text, or memory_read name=<name>", id))
		}
		// A ULID is globally unique, so this lookup resolves no scope and would
		// otherwise hand any caller any project's memory. The check is post-load
		// because the row is what names the project to judge -- nothing of it
		// reaches the caller before the fence answers.
		if err := s.fenceRead(ctx, m.Project); err != nil {
			return errResult("memory_read", err)
		}
		idx = m
	} else {
		name, err := memoryName(rawName)
		if err != nil {
			return errResult("memory_read", err)
		}
		if name == "" {
			return errResult("memory_read", errors.New("name or id is required (memory_read reads one memory by exact name or ULID; to search text use recall)"))
		}
		project, err := s.resolveReadScope(ctx, argString(req, "project"))
		if err != nil {
			return errResult("memory_read", err)
		}
		var found bool
		idx, found, err = s.resolveMemory(ctx, project, name, true)
		if err != nil {
			return errResult("memory_read", err)
		}
		if !found {
			// The active lookup missed; a superseded memory (excluded from the
			// active index) is still readable, returned with a warning pointing at
			// its replacement so the agent reads the current knowledge instead.
			idx, found, err = s.resolveSupersededMemory(ctx, project, name)
			if err != nil {
				return errResult("memory_read", err)
			}
			if !found {
				return errResult("memory_read", s.memoryReadMiss(ctx, project, name))
			}
		}
	}
	mem, err := s.cfg.Files.Store().ReadMemory(idx.FilePath)
	if err != nil {
		return errResult("memory_read", err)
	}
	// Carry index-only lifecycle fields onto the file-parsed memory for the response.
	mem.InvalidAt, mem.SupersededBy = idx.InvalidAt, idx.SupersededBy
	s.record(ctx, core.EventMemoryRead, s.boundSession(ctx), mem.Project, mem.ID, map[string]any{"name": mem.Name})
	s.markFirstReuse(ctx, mem)

	// content_hash is the ETag half of the expect_hash precondition: without it
	// there is no way for an agent to say "write only if nothing moved since I
	// read this", so the precondition is inert. It is the caller's own handle on
	// the item it just read -- a digest of bytes it already holds -- not authored
	// content, so it is never a candidate for withholding.
	out := map[string]any{
		"id": mem.ID, "kind": string(mem.Kind), "name": mem.Name,
		"description": mem.Description, "project": mem.Project, "body": mem.Body,
		"tags": mem.Tags, "source_session": mem.SourceSession,
		"content_hash": mem.ContentHash,
	}
	if mem.Model != "" {
		out["model"] = mem.Model
	}
	if mem.Favorite {
		out["favorite"] = true
	}
	if !mem.Active() {
		out["warning"] = s.supersededWarning(ctx, mem)
	}
	return jsonResult(out)
}

// resolveSupersededMemory finds a superseded (invalid) memory by (project, name),
// falling back to the global scope, for memory_read's warning path. The fallback
// is fenced exactly like the active one: an invalid memory is still the global
// scope's content, so a sealed session must not reach it here either.
func (s *Server) resolveSupersededMemory(ctx context.Context, project, name string) (core.Memory, bool, error) {
	m, ok, err := store.MemoryByNameIncludingInvalid(ctx, s.cfg.DB, project, name)
	if err != nil || ok {
		return m, ok, err
	}
	if project != "" {
		global, gerr := s.canReadGlobal(ctx)
		if gerr != nil {
			return core.Memory{}, false, gerr
		}
		if global {
			return store.MemoryByNameIncludingInvalid(ctx, s.cfg.DB, "", name)
		}
	}
	return core.Memory{}, false, nil
}

// supersededWarning renders the read warning for an invalid memory, naming the
// replacement when superseded_by resolves to a known memory.
func (s *Server) supersededWarning(ctx context.Context, mem core.Memory) string {
	when := ""
	if mem.InvalidAt != nil {
		when = " on " + mem.InvalidAt.Format("2006-01-02")
	}
	if mem.SupersededBy != "" {
		if repl, ok, err := store.MemoryByID(ctx, s.cfg.DB, mem.SupersededBy); err == nil && ok {
			return fmt.Sprintf("superseded by %s%s; read that instead",
				lifecycle.MemoryRef(repl.Project, repl.Name), when)
		}
		return fmt.Sprintf("superseded by %s%s; read that instead", mem.SupersededBy, when)
	}
	return fmt.Sprintf("archived%s; this memory is no longer active", when)
}

func memoryDeleteTool() mcp.Tool {
	return mcp.NewTool("memory_delete", hintOverwrite(),
		mcp.WithDescription("Delete a memory by name: the markdown file leaves the disk and its index row goes with it, with no provenance and no pointer left behind. Prefer nearly anything else. To replace knowledge that turned out to be wrong, use memory_write with supersedes -- the old memory drops out of every index (briefings, recall) but stays readable, pointing at what replaced it, which is how a later reader learns the thing was reconsidered rather than that it was never believed. To retire something merely stale, leave it for the gardener's archive proposal. Reserve deletion for memories written by mistake: a duplicate, a test, a write into the wrong project."),
		mcp.WithString("name", mcp.Required(), mcp.Description("memory name")),
		mcp.WithString("project", mcp.Description("project slug; defaults to the bound session's project")),
	)
}

func (s *Server) handleMemoryDelete(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name, err := memoryName(argString(req, "name"))
	if err != nil {
		return errResult("memory_delete", err)
	}
	if name == "" {
		return errResult("memory_delete", errors.New("name is required"))
	}
	project, err := s.resolveReadScope(ctx, argString(req, "project"))
	if err != nil {
		return errResult("memory_delete", err)
	}
	idx, found, err := s.resolveMemory(ctx, project, name, true)
	if err != nil {
		return errResult("memory_delete", err)
	}
	if !found {
		return errResult("memory_delete", s.scopedNotFound(ctx, "memory", project, name, "", ""))
	}
	// Same reason as memory_append: the global fallback can land this delete on a
	// memory outside the caller's project, and a delete is the least recoverable
	// write there is.
	if err := s.fenceWrite(ctx, idx.Project); err != nil {
		return errResult("memory_delete", err)
	}
	if err := s.cfg.Files.Remove(ctx, idx.FilePath); err != nil {
		return errResult("memory_delete", err)
	}
	s.record(ctx, core.EventMemoryArchived, s.boundSession(ctx), idx.Project, idx.ID, map[string]any{"name": name})
	// project names the scope actually deleted from: the lookup falls back to the
	// global scope, so a delete aimed at a project can land on a global memory --
	// the response must say so rather than implying the project-scoped one.
	return jsonResult(map[string]any{"status": "deleted", "id": idx.ID, "name": name, "project": idx.Project})
}

// clientMemoryNote is the boundary between Seamless memories and the agent
// client's own file-based memory. Claude Code injects its MEMORY.md index of
// local memory files at session start, and agents read a name out of that index
// and asked memory_read for it -- four of the five misses behind a gardener
// recurring-error task (2026-10). The tool description and the miss tail render
// the same string so the two cannot drift.
const clientMemoryNote = "a name listed in your client's own MEMORY.md (Claude Code's file-based auto-memory) is a local file, not a Seamless memory: open it with your file tools"

// The by-name miss tails, paired: the plain form, and the same advice with the
// global scope struck out for a session that cannot reach it.
const (
	memoryReadMissHelp       = "; check the name, pass project=<slug> or project=global, read by id=<ULID>, or use recall to search by text; " + clientMemoryNote
	memoryReadMissHelpSealed = "; check the name, read by id=<ULID>, or use recall to search by text; " + clientMemoryNote

	memoryAppendMissHelp       = "; create it first with memory_write, or pass project=<slug> / project=global"
	memoryAppendMissHelpSealed = "; create it first with memory_write"
)

// scopedNotFound builds a "no such item" error that names the scope actually
// searched and the remedies that exist for THIS caller, so the agent can tell a
// wrong-project mistake from a wrong-name one.
//
// help is the tool's usual tail; sealedHelp is the same advice minus the global
// scope. Both halves are conditional because a sealed session's by-name lookup
// never reaches global and its project=global is refused outright: "(also
// searched global)" would be untrue, and "pass project=global" would name an
// escape hatch the same fence closes. Guidance and behavior land together
// (write-scope-registers-the-project-it-names), here on the read side.
func (s *Server) scopedNotFound(ctx context.Context, kind, project, name, help, sealedHelp string) error {
	if project == "" {
		return fmt.Errorf("no %s named %q in the global scope%s", kind, name, help)
	}
	global, err := s.canReadGlobal(ctx)
	if err != nil {
		// The miss is already decided, and a wrong claim about what was searched
		// would be a fake result, so the message says less rather than something
		// untrue (the ambientFenceErr degradation, applied here).
		s.logger.Warn("mcp: by-name miss fence state", "project", project, "error", err)
		return fmt.Errorf("no %s named %q in project %q%s", kind, name, project, help)
	}
	if !global {
		return fmt.Errorf("no %s named %q in project %q; this session is %s, so the global scope is not searched%s",
			kind, name, project, core.IsolationSealed, sealedHelp)
	}
	return fmt.Errorf("no %s named %q in project %q (also searched global)%s", kind, name, project, help)
}

// memoryReadMiss explains a by-name memory_read miss. When the name is an active
// memory in other projects this caller may read, the agent knew the memory but
// not its scope, so the tail names those projects instead of the generic advice.
func (s *Server) memoryReadMiss(ctx context.Context, project, name string) error {
	scopes, err := store.ProjectsWithMemoryNamed(ctx, s.cfg.DB, name)
	var elsewhere []string
	if err == nil {
		elsewhere, err = s.readableElsewhere(ctx, project, scopes)
	}
	if err != nil {
		// The miss is already decided; a failed hint lookup must not turn it into
		// a different error, so the tail falls back to the generic advice.
		s.logger.Warn("mcp: memory_read miss hint", "name", name, "error", err)
	}
	if len(elsewhere) == 0 {
		return s.scopedNotFound(ctx, "memory", project, name, memoryReadMissHelp, memoryReadMissHelpSealed)
	}
	help := elsewhereHelp(elsewhere)
	return s.scopedNotFound(ctx, "memory", project, name, help, help)
}

// readableElsewhere narrows the scopes a name lives in to the projects other
// than searched that the caller may read: the ones a by-name miss can point
// at. Each is judged by store.CanRead, so a fenced project's names never
// surface; the global scope is never named, because it was either searched
// already or is closed to this caller.
func (s *Server) readableElsewhere(ctx context.Context, searched string, scopes []string) ([]string, error) {
	caller := s.callerScope(ctx)
	var out []string
	for _, p := range scopes {
		if p == "" || p == searched {
			continue
		}
		ok, err := store.CanRead(ctx, s.cfg.DB, caller, p)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, p)
		}
	}
	return out, nil
}

// elsewhereHelp is the miss tail naming the projects that hold the name.
func elsewhereHelp(elsewhere []string) string {
	if len(elsewhere) == 1 {
		return fmt.Sprintf("; it is in project %s: pass project=%s", elsewhere[0], elsewhere[0])
	}
	return fmt.Sprintf("; that name is in projects %s: pass project=<slug> for the one you mean",
		strings.Join(elsewhere, ", "))
}

// memoryName canonicalizes a caller-supplied memory name to the kebab-case form
// the corpus stores, and rejects what cannot be a filename.
//
// Two problems, one place. First, consistency: notes_create runs its title
// through core.Slugify, but memory_write took `name` verbatim -- so an agent
// writing "My Gotcha" got a memory it could never read back with the name it
// had just used, and a corpus that disagreed with itself about capitalization.
// Every memory tool routes through here, so write and lookup canonicalize
// identically and the two forms are the same memory. Second, errors: the
// filesystem layer already refuses unsafe names before anything reaches disk
// (so this is hygiene, not a hole), but it refuses them deep in a write path,
// which surfaces to the agent as a confusing failure rather than "that name is
// not allowed". Validating at the tool boundary says so plainly. (Audit I12.)
//
// Slugify handles most of it; the validate.Name pass catches what slugging
// cannot fix -- notably a Windows reserved device name like "con", which is
// already lowercase and dash-free and so survives slugging untouched.
func memoryName(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", nil
	}
	name := core.Slugify(trimmed)
	if err := validate.Name(name); err != nil {
		return "", fmt.Errorf("invalid memory name %q: %w", raw, err)
	}
	return name, nil
}

// resolveMemory finds an active memory by (project, name); when globalFallback
// is set and none is found in the project scope, it retries the global scope --
// unless the caller's own fence has removed that scope (canReadGlobal).
func (s *Server) resolveMemory(ctx context.Context, project, name string, globalFallback bool) (core.Memory, bool, error) {
	m, ok, err := store.MemoryByName(ctx, s.cfg.DB, project, name)
	if err != nil || ok {
		return m, ok, err
	}
	if globalFallback && project != "" {
		global, gerr := s.canReadGlobal(ctx)
		if gerr != nil {
			return core.Memory{}, false, gerr
		}
		if global {
			return store.MemoryByName(ctx, s.cfg.DB, "", name)
		}
	}
	return core.Memory{}, false, nil
}
