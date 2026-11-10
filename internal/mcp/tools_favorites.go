package mcp

import (
	"context"
	"errors"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/plans"
	"github.com/arctop/seamless/internal/store"
)

// favoriteKinds lists every entity kind favorite_set accepts. Plans resolve to
// their primary note (a plan is a composition, not a record); the rest map to
// their own store row or file.
var favoriteKinds = []string{"memory", "note", "project", "plan", "task", "session", "trial"}

func favoriteSetTool() mcp.Tool {
	return mcp.NewTool("favorite_set", hintSet(),
		mcp.WithDescription("Star or unstar an item. Favorites sort first in the console, are pinned "+
			"into session briefings (memories), and get a mild recall rank boost. For memories and notes "+
			"the flag is stored in the file's frontmatter; starring never bumps an item's updated time."),
		mcp.WithString("kind", mcp.Required(), enumOf(favoriteKinds),
			mcp.Description("what kind of item to star")),
		mcp.WithString("id", mcp.Required(),
			mcp.Description("the item's identifier: memory name, note id (or slug), project slug, "+
				"plan slug, task id, session id (or name), trial id")),
		mcp.WithBoolean("favorite", mcp.Required(),
			mcp.Description("true to star, false to unstar")),
		mcp.WithString("project", mcp.Description("project scope for memory-name/note-slug resolution; "+
			"defaults to the bound session's project")),
	)
}

func (s *Server) handleFavoriteSet(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// Stars are the owner's curation: a starred memory pins into every
	// session's briefing, and a star sorts any item first on every surface. A
	// restricted account is refused before anything is resolved, so the answer
	// says nothing about the item it named.
	if account := access.AccountOf(ctx); account != "" {
		return errResult("favorite_set", ownerCurationRefusal(account,
			"a star is the owner's curation (a starred memory pins into every session's briefing and boosts in recall)",
			"tell the user which item deserves a star; the owner sets it from the console"))
	}
	kind := argString(req, "kind")
	id := argString(req, "id")
	fav := argBool(req, "favorite", false)
	if id == "" {
		return errResult("favorite_set", errors.New("id is required"))
	}

	var project, itemID string
	var err error
	switch kind {
	case "memory":
		project, itemID, err = s.setMemoryFavorite(ctx, req, id, fav)
	case "note", "plan":
		project, itemID, err = s.setNoteFavorite(ctx, req, kind, id, fav)
	case "project":
		_, found, perr := store.ProjectBySlug(ctx, s.cfg.DB, id)
		if perr == nil && !found {
			perr = fmt.Errorf("no project with slug %q", id)
		}
		if perr == nil {
			perr = s.fenceWrite(ctx, id)
		}
		if perr == nil {
			perr = store.SetProjectFavorite(ctx, s.cfg.DB, id, fav)
		}
		project, itemID, err = id, id, perr
	case "task":
		// No claim-lock check: a star is metadata, not a content mutation, so
		// starring a task another session holds is safe and allowed.
		t, terr := store.TaskByID(ctx, s.cfg.DB, id)
		if terr == nil {
			terr = s.fenceWrite(ctx, t.ProjectSlug)
		}
		if terr == nil {
			terr = store.SetTaskFavorite(ctx, s.cfg.DB, id, fav)
		}
		project, itemID, err = t.ProjectSlug, id, terr
	case "session":
		sess, found, serr := store.SessionByID(ctx, s.cfg.DB, id)
		if serr == nil && !found {
			sess, found, serr = store.SessionByName(ctx, s.cfg.DB, id)
		}
		if serr == nil && !found {
			serr = fmt.Errorf("no session with id or name %q", id)
		}
		if serr == nil {
			serr = s.fenceWrite(ctx, sess.ProjectSlug)
		}
		if serr == nil {
			serr = store.SetSessionFavorite(ctx, s.cfg.DB, sess.ID, fav)
		}
		project, itemID, err = sess.ProjectSlug, sess.ID, serr
	case "trial":
		// favorite_set is the one tool that reaches trial data while staying
		// exposed for its other kinds, so the research gate is in-handler here.
		// The three research tools are gated by the tool filter instead, which is
		// why this is the only such check in the tool surface.
		if !s.researchEnabled(ctx) {
			return errResult("favorite_set", errResearchDisabled)
		}
		tr, found, terr := store.TrialByID(ctx, s.cfg.DB, id)
		if terr == nil && !found {
			terr = fmt.Errorf("no trial with id %q", id)
		}
		if terr == nil {
			terr = s.fenceWrite(ctx, tr.ProjectSlug)
		}
		if terr == nil {
			terr = store.SetTrialFavorite(ctx, s.cfg.DB, id, fav)
		}
		project, itemID, err = tr.ProjectSlug, id, terr
	default:
		err = fmt.Errorf("unknown kind %q", kind)
	}
	if err != nil {
		return errResult("favorite_set", err)
	}

	s.record(ctx, core.EventFavoriteChanged, s.boundSession(ctx), project, itemID,
		map[string]any{"kind": kind, "id": id, "favorite": fav, "by": "agent"})
	return jsonResult(map[string]any{"kind": kind, "id": id, "favorite": fav})
}

// setMemoryFavorite resolves a memory by name (project scope + global fallback,
// like memory_read) and rewrites its file with the flag flipped. The full file
// is read first -- index rows carry no body, so writing from one would truncate
// the memory -- and the read is inside the file's mutation lock, because the
// flip renders the WHOLE file from what that read returned: a memory_append
// landing between the two used to disappear under the star's rename with no
// error anywhere. Updated is deliberately not bumped: a star is not authorship.
//
// A star is still a durable write, and one with reach: a starred memory pins
// into its project's briefings and boosts in recall. So the resolved memory's
// own project goes through the write fence, and the global fallback goes through
// the read fence inside resolveMemory. Both fences are resolution, not mutation,
// and stay outside the lock.
func (s *Server) setMemoryFavorite(ctx context.Context, req mcp.CallToolRequest, id string, fav bool) (project, itemID string, err error) {
	name, err := memoryName(id)
	if err != nil {
		return "", "", err
	}
	scope, err := s.resolveReadScope(ctx, argString(req, "project"))
	if err != nil {
		return "", "", err
	}
	idx, found, err := s.resolveMemory(ctx, scope, name, true)
	if err != nil {
		return "", "", err
	}
	if !found {
		return "", "", s.scopedNotFound(ctx, "memory", scope, name, "", "")
	}
	// A star pins a memory into briefings: on a name a restricted account's
	// project memory shares with a global rule, the call must say which one.
	if err := s.refuseShadowedWrite(ctx, idx, argString(req, "project") != ""); err != nil {
		return "", "", err
	}
	if err := s.fenceWrite(ctx, idx.Project); err != nil {
		return "", "", err
	}
	// An already-correct flag reports ErrNoChange rather than writing: re-rendering
	// an unchanged file would re-index and re-embed it, so a repeated star would
	// cost real work and churn the corpus for nothing.
	mem, err := s.cfg.Files.MutateMemory(ctx, idx.FilePath, func(_ context.Context, mem core.Memory) (core.Memory, error) {
		if mem.Favorite == fav {
			return mem, files.ErrNoChange
		}
		mem.Favorite = fav
		return mem, nil
	})
	if err != nil {
		return "", "", err
	}
	return mem.Project, mem.ID, nil
}

// setNoteFavorite flips the flag on a note (by id, falling back to slug in the
// session scope then global) or on a plan's primary note. A task-only plan has
// no note and cannot be starred. Resolution and the fence stay outside the
// file's mutation lock; the read that feeds the write is inside it, for the
// reason setMemoryFavorite spells out.
func (s *Server) setNoteFavorite(ctx context.Context, req mcp.CallToolRequest, kind, id string, fav bool) (project, itemID string, err error) {
	var idx core.Note
	if kind == "plan" {
		primary, _, ok, cerr := plans.Composition(ctx, s.cfg.DB, id)
		if cerr != nil {
			return "", "", cerr
		}
		if !ok {
			return "", "", fmt.Errorf("plan %q has no note to favorite (task-only plans cannot be starred)", id)
		}
		idx = primary
	} else {
		var found bool
		idx, found, err = store.NoteByID(ctx, s.cfg.DB, id)
		if err != nil {
			return "", "", err
		}
		if !found {
			scope, serr := s.resolveReadScope(ctx, argString(req, "project"))
			if serr != nil {
				return "", "", serr
			}
			idx, found, err = store.NoteBySlug(ctx, s.cfg.DB, scope, id)
			if err == nil && !found && scope != "" {
				// Fenced like every other by-name global fallback: a sealed session
				// must not reach a global note it cannot otherwise read.
				var global bool
				if global, err = s.canReadGlobal(ctx); err == nil && global {
					idx, found, err = store.NoteBySlug(ctx, s.cfg.DB, "", id)
				}
			}
			if err != nil {
				return "", "", err
			}
			if !found {
				return "", "", s.scopedNotFound(ctx, "note", scope, id, "", "")
			}
		}
	}
	if err := s.fenceWrite(ctx, idx.Project); err != nil {
		return "", "", err
	}
	note, err := s.cfg.Files.MutateNote(ctx, idx.FilePath, func(_ context.Context, note core.Note) (core.Note, error) {
		if note.Favorite == fav {
			return note, files.ErrNoChange
		}
		note.Favorite = fav
		return note, nil
	})
	if err != nil {
		return "", "", err
	}
	return note.Project, note.ID, nil
}
