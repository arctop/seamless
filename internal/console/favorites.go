package console

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/plans"
	"github.com/arctop/seamless/internal/store"
)

// favoriteListPages maps a favorite kind to its library page, the redirect
// fallback when a toggle form carries no usable next target. Doubling as the
// kind allowlist for the route's {kind} segment.
var favoriteListPages = map[string]string{
	"memory":  "/console/memories",
	"note":    "/console/notes",
	"plan":    "/console/plans",
	"project": "/console/projects",
	"task":    "/console/tasks",
	"session": "/console/sessions",
	"trial":   "/console/trials",
}

// favoriteToggle is the one owner mutation behind every star in the console:
// POST /console/favorites/{kind}/{id} with favorite=1|0 and an optional next
// redirect target. File-backed kinds (memory, note, plan-via-primary-note) are
// rewritten through the files manager so frontmatter stays the source of truth;
// DB kinds are a targeted UPDATE. Neither path bumps the item's updated time.
func (s *Service) favoriteToggle(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	kind := r.PathValue("kind")
	id := r.PathValue("id")
	listPage, knownKind := favoriteListPages[kind]
	if !knownKind {
		s.notFound(w, r, "Unknown favorite kind "+kind+".")
		return
	}
	favorites, present := r.PostForm["favorite"]
	if !present || len(favorites) != 1 || (favorites[0] != "0" && favorites[0] != "1") {
		s.badRequest(w, r, "favorite must be exactly one of 0 or 1")
		return
	}
	fav := favorites[0] == "1"

	var project, itemID string
	var err error
	switch kind {
	case "memory", "note", "plan":
		project, itemID, err = s.setFileFavorite(ctx, kind, id, fav)
	case "project":
		_, found, perr := store.ProjectBySlug(ctx, s.cfg.DB, id)
		if perr == nil && found {
			perr = store.SetProjectFavorite(ctx, s.writeDB(), id, fav)
		} else if perr == nil {
			perr = errFavoriteNotFound
		}
		project, itemID, err = id, id, perr
	case "task":
		t, terr := store.TaskByID(ctx, s.cfg.DB, id)
		if terr == nil {
			terr = store.SetTaskFavorite(ctx, s.writeDB(), id, fav)
		}
		project, itemID, err = t.ProjectSlug, id, terr
	case "session":
		sess, found, serr := store.SessionByID(ctx, s.cfg.DB, id)
		if serr == nil && found {
			serr = store.SetSessionFavorite(ctx, s.writeDB(), id, fav)
		} else if serr == nil {
			serr = errFavoriteNotFound
		}
		project, itemID, err = sess.ProjectSlug, id, serr
	case "trial":
		tr, found, terr := store.TrialByID(ctx, s.cfg.DB, id)
		if terr == nil && found {
			terr = store.SetTrialFavorite(ctx, s.writeDB(), id, fav)
		} else if terr == nil {
			terr = errFavoriteNotFound
		}
		project, itemID, err = tr.ProjectSlug, id, terr
	}
	switch {
	case errIsNotFound(err):
		s.notFound(w, r, "No "+kind+" with id "+id+".")
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}

	if s.cfg.Events != nil {
		if _, err := s.cfg.Events.Record(ctx, core.Event{
			Kind: core.EventFavoriteChanged, ProjectSlug: project, ItemID: itemID,
			Payload: map[string]any{"kind": kind, "id": id, "favorite": fav, "by": "console"},
		}); err != nil {
			s.logger.Warn("console: record favorite event", "error", err)
		}
	}

	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, map[string]any{"kind": kind, "id": id, "favorite": fav})
		return
	}
	next := safeNext(r.PostForm.Get("next"))
	if next == "/console/" {
		next = listPage
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// errFavoriteNotFound marks a favorite target that does not exist, so the
// handler can 404 instead of 500. Task lookups signal the same through
// store.ErrTaskNotFound.
var errFavoriteNotFound = fmt.Errorf("favorite target: %w", store.ErrTaskNotFound)

// errIsNotFound reports whether err is either not-found sentinel.
func errIsNotFound(err error) bool {
	return errors.Is(err, store.ErrTaskNotFound)
}

// setFileFavorite flips the frontmatter flag on a memory, a note, or a plan's
// primary note. The full file is read first (index rows carry no body) and
// Updated is left alone -- a star is not authorship.
//
// Resolution is a DB lookup that only produces the path, so it stays outside the
// file's mutation lock; the read that feeds the write is inside it. The owner
// starring a memory an agent is appending to is the ordinary case here, and
// unserialized the star rendered the whole file from the pre-append body and
// erased the agent's write with no error anywhere.
func (s *Service) setFileFavorite(ctx context.Context, kind, id string, fav bool) (project, itemID string, err error) {
	if s.cfg.Files == nil {
		return "", "", errNoFiles
	}
	var filePath string
	switch kind {
	case "memory":
		idx, ok, merr := store.MemoryByID(ctx, s.cfg.DB, id)
		if merr != nil {
			return "", "", merr
		}
		if !ok {
			return "", "", errFavoriteNotFound
		}
		filePath = idx.FilePath
	case "note":
		idx, ok, nerr := store.NoteByID(ctx, s.cfg.DB, id)
		if nerr != nil {
			return "", "", nerr
		}
		if !ok {
			return "", "", errFavoriteNotFound
		}
		filePath = idx.FilePath
	case "plan":
		primary, _, ok, perr := plans.Composition(ctx, s.cfg.DB, id)
		if perr != nil {
			return "", "", perr
		}
		if !ok {
			return "", "", errFavoriteNotFound
		}
		filePath = primary.FilePath
	}

	// An item already in the requested state reports ErrNoChange, which skips the
	// write: re-rendering an unchanged file would re-index and re-embed it, so a
	// double-click on a star would cost real work for nothing.
	if kind == "memory" {
		mem, merr := s.cfg.Files.MutateMemory(ctx, filePath, func(_ context.Context, mem core.Memory) (core.Memory, error) {
			if mem.Favorite == fav {
				return mem, files.ErrNoChange
			}
			mem.Favorite = fav
			return mem, nil
		})
		if merr != nil {
			return "", "", merr
		}
		return mem.Project, mem.ID, nil
	}
	note, nerr := s.cfg.Files.MutateNote(ctx, filePath, func(_ context.Context, note core.Note) (core.Note, error) {
		if note.Favorite == fav {
			return note, files.ErrNoChange
		}
		note.Favorite = fav
		return note, nil
	})
	if nerr != nil {
		return "", "", nerr
	}
	return note.Project, note.ID, nil
}
