package screening

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/lifecycle"
	"github.com/arctop/seamless/internal/store"
)

// Domain errors of the held queue.
var (
	// ErrStale: the item a held write lands on changed after it was held (a
	// later write landed, the name was taken, the target was retired). A
	// release would silently undo that change, so it is refused; the owner
	// discards the hold instead and the agent writes again.
	ErrStale = errors.New("the item changed after this write was held")
	// ErrNoApplier: a held gardener apply cannot be released on a service
	// built without the gardener.
	ErrNoApplier = errors.New("no gardener is wired to release a held gardener apply")
)

// Applier carries out a gardener proposal: gardener.Service.Apply.
type Applier interface {
	Apply(ctx context.Context, id string) (map[string]any, error)
}

// Config wires a Service.
type Config struct {
	// DB is the main (writable) handle: holds, confirmations and marks are
	// written here, never through a filtered console view.
	DB       *sql.DB
	Files    *files.Manager
	Events   *events.Recorder // may be nil (no events are recorded)
	Gardener Applier          // may be nil (held gardener applies cannot be released)
	Rules    Rules
	Logger   *slog.Logger
	Now      func() time.Time // nil = time.Now
}

// Service owns the held queue and the session marks. It never decides whether
// the feature is on: the surfaces ask features.Screening first.
type Service struct {
	cfg Config
}

// New builds a Service.
func New(cfg Config) *Service {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Service{cfg: cfg}
}

// Rules returns the screens' tunables.
func (s *Service) Rules() Rules { return s.cfg.Rules }

func (s *Service) now() time.Time { return s.cfg.Now().UTC() }

func (s *Service) record(ctx context.Context, e core.Event) {
	if s.cfg.Events == nil {
		return
	}
	if _, err := s.cfg.Events.Record(ctx, e); err != nil {
		s.cfg.Logger.Warn("screening: record event", "kind", e.Kind, "error", err)
	}
}

// Screened is one write's memory.screened record: codes and sizes only.
type Screened struct {
	ItemKind  string
	ItemID    string
	Project   string
	SessionID string
	Kind      core.MemoryKind // the memory kind ("" for a note)
	Tool      string
	Size      int // bytes of screened text
	Verdicts  []Code
	Held      bool
}

// RecordScreened records memory.screened. Every screened write gets one, the
// clean ones included (verdicts: []): a monitor reading the log can tell a
// clean write from one that was never screened, which is what makes switching
// the feature off visible.
func (s *Service) RecordScreened(ctx context.Context, sc Screened) {
	payload := map[string]any{
		"item_kind": sc.ItemKind, "tool": sc.Tool, "size": sc.Size,
		"verdicts": Strings(sc.Verdicts), "held": sc.Held,
	}
	if sc.Kind != "" {
		payload["kind"] = string(sc.Kind)
	}
	s.record(ctx, core.Event{Kind: core.EventMemoryScreened, SessionID: sc.SessionID,
		ProjectSlug: sc.Project, ItemID: sc.ItemID, Payload: payload})
}

// Hold stores a write in the owner's queue instead of the corpus and records
// memory.held. The caller fills everything but the id and the time.
func (s *Service) Hold(ctx context.Context, h store.HeldWrite) (store.HeldWrite, error) {
	id, err := core.NewID()
	if err != nil {
		return store.HeldWrite{}, fmt.Errorf("screening.Hold: %w", err)
	}
	h.ID, h.CreatedAt, h.Status = id, s.now(), store.HeldStatusHeld
	if err := store.InsertHeldWrite(ctx, s.cfg.DB, h); err != nil {
		return store.HeldWrite{}, fmt.Errorf("screening.Hold: %w", err)
	}
	s.record(ctx, core.Event{Kind: core.EventMemoryHeld, SessionID: h.SessionID, ProjectSlug: h.Project,
		ItemID: h.ItemID, Payload: heldPayload(h, map[string]any{"tool": h.Tool, "reasons": h.Reasons})})
	return h, nil
}

func heldPayload(h store.HeldWrite, extra map[string]any) map[string]any {
	p := map[string]any{"hold": h.ID, "op": h.Op, "item_kind": h.ItemKind}
	if h.MemoryKind != "" {
		p["kind"] = string(h.MemoryKind)
	}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

// ---------------------------------------------------------------------------
// Session marks
// ---------------------------------------------------------------------------

// Tainted reports whether a session carries the outside-text mark. A session
// id of "" (an unbound call) carries none.
func (s *Service) Tainted(ctx context.Context, sessionID string) (bool, error) {
	if sessionID == "" {
		return false, nil
	}
	_, ok, err := store.SessionTaintOf(ctx, s.cfg.DB, sessionID)
	if err != nil {
		return false, fmt.Errorf("screening.Tainted: %w", err)
	}
	return ok, nil
}

// Taint marks a session as having read outside text and records
// session.tainted the first time. It reports whether this call set the mark.
func (s *Service) Taint(ctx context.Context, sess core.Session, reason, via string) (bool, error) {
	first, err := store.SetSessionTaint(ctx, s.cfg.DB, sess.ID, reason, via, s.now())
	if err != nil {
		return false, fmt.Errorf("screening.Taint: %w", err)
	}
	if first {
		s.record(ctx, core.Event{Kind: core.EventSessionTainted, SessionID: sess.ID, ProjectSlug: sess.ProjectSlug,
			ItemID: sess.ID, Payload: map[string]any{"reason": reason, "via": via}})
	}
	return first, nil
}

// ClearTaint removes a session's mark -- the owner's call, from the console,
// and nowhere else -- and records session.untainted.
func (s *Service) ClearTaint(ctx context.Context, sess core.Session) (bool, error) {
	cleared, err := store.ClearSessionTaint(ctx, s.cfg.DB, sess.ID)
	if err != nil {
		return false, fmt.Errorf("screening.ClearTaint: %w", err)
	}
	if cleared {
		s.record(ctx, core.Event{Kind: core.EventSessionCleared, SessionID: sess.ID, ProjectSlug: sess.ProjectSlug,
			ItemID: sess.ID, Payload: map[string]any{"by": "owner"}})
	}
	return cleared, nil
}

// ---------------------------------------------------------------------------
// Release and discard (the owner's)
// ---------------------------------------------------------------------------

// Released is what a release applied.
type Released struct {
	Hold       store.HeldWrite
	ItemID     string
	Superseded string // the memory a held supersede retired ("" for none)
	Result     map[string]any
}

// Release applies a held write through the files layer -- exactly the write
// the agent asked for -- and marks the hold released. A memory it lands is
// stamped owner-confirmed. A write whose target moved on after it was held is
// refused with ErrStale and stays held.
func (s *Service) Release(ctx context.Context, id string) (Released, error) {
	h, ok, err := store.HeldWriteByID(ctx, s.cfg.DB, id)
	if err != nil {
		return Released{}, err
	}
	if !ok || h.Status != store.HeldStatusHeld {
		return Released{}, store.ErrHoldNotPending
	}
	var out Released
	switch {
	case h.Op == store.HeldOpWrite && h.ItemKind == core.ItemKindMemory:
		out, err = s.releaseMemoryWrite(ctx, h)
	case h.Op == store.HeldOpWrite && h.ItemKind == core.ItemKindNote:
		out, err = s.releaseNoteWrite(ctx, h)
	case h.Op == store.HeldOpDelete:
		out, err = s.releaseDelete(ctx, h)
	case h.Op == store.HeldOpGardener:
		out, err = s.releaseGardener(ctx, h)
	default:
		return Released{}, fmt.Errorf("screening.Release: unknown held op %q on %s", h.Op, h.ItemKind)
	}
	if err != nil {
		return Released{}, err
	}
	if err := store.ResolveHeldWrite(ctx, s.cfg.DB, h.ID, store.HeldStatusReleased, s.now()); err != nil {
		// The write already landed; a concurrent resolution of the same row is
		// the only way here, and the corpus -- not the row -- is the record.
		s.cfg.Logger.Warn("screening: release landed but the hold was resolved concurrently", "hold", h.ID, "error", err)
	}
	s.record(ctx, core.Event{Kind: core.EventMemoryReleased, SessionID: h.SessionID, ProjectSlug: h.Project,
		ItemID: h.ItemID, Payload: heldPayload(h, map[string]any{"tool": h.Tool, "by": "owner"})})
	out.Hold = h
	return out, nil
}

// Discard drops a held write: nothing lands, its text is erased, and the row
// stays as the record of what was refused.
func (s *Service) Discard(ctx context.Context, id string) (store.HeldWrite, error) {
	h, ok, err := store.HeldWriteByID(ctx, s.cfg.DB, id)
	if err != nil {
		return store.HeldWrite{}, err
	}
	if !ok || h.Status != store.HeldStatusHeld {
		return store.HeldWrite{}, store.ErrHoldNotPending
	}
	if err := store.ResolveHeldWrite(ctx, s.cfg.DB, h.ID, store.HeldStatusDiscarded, s.now()); err != nil {
		return store.HeldWrite{}, err
	}
	s.record(ctx, core.Event{Kind: core.EventMemoryDiscarded, SessionID: h.SessionID, ProjectSlug: h.Project,
		ItemID: h.ItemID, Payload: heldPayload(h, map[string]any{"tool": h.Tool, "by": "owner"})})
	return h, nil
}

func (s *Service) releaseMemoryWrite(ctx context.Context, h store.HeldWrite) (Released, error) {
	path := h.DetailValue("path")
	mem, err := files.ParseMemory(h.Content, path)
	if err != nil {
		return Released{}, fmt.Errorf("screening.Release: held memory: %w", err)
	}
	mem.FilePath = ""
	// A held supersede retires its target only if that target is still the
	// active memory it was when held; checked before anything is written, so a
	// stale target refuses the whole release rather than half of it.
	var target core.Memory
	if tid := h.DetailValue("supersedes_id"); tid != "" {
		t, found, terr := store.MemoryByID(ctx, s.cfg.DB, tid)
		if terr != nil {
			return Released{}, terr
		}
		if !found || !t.Active() {
			return Released{}, fmt.Errorf("screening.Release: the memory it supersedes is gone or no longer active: %w", ErrStale)
		}
		target = t
	}
	var written core.Memory
	if err := s.cfg.Files.Mutate(ctx, path, func(ctx context.Context) error {
		if err := s.checkBase(path, h.BaseHash, func(p string) (string, error) {
			cur, rerr := s.cfg.Files.Store().ReadMemory(p)
			return cur.ContentHash, rerr
		}); err != nil {
			return err
		}
		w, werr := s.cfg.Files.WriteMemory(ctx, mem)
		if werr != nil {
			return werr
		}
		written = w
		return nil
	}); err != nil {
		return Released{}, fmt.Errorf("screening.Release: %w", err)
	}
	s.record(ctx, core.Event{Kind: core.EventMemoryWritten, SessionID: h.SessionID, ProjectSlug: written.Project,
		ItemID: written.ID, Payload: map[string]any{"name": written.Name, "kind": string(written.Kind),
			"updated": h.BaseHash != "", "released": h.ID}})
	if err := store.UpsertOwnerConfirmation(ctx, s.cfg.DB, written.ID, core.MemoryDigest(written),
		store.ConfirmedViaRelease, s.now()); err != nil {
		// The write landed; a missing mark only understates trust.
		s.cfg.Logger.Warn("screening: owner confirmation", "id", written.ID, "error", err)
	}
	out := Released{ItemID: written.ID}
	if target.ID != "" {
		var updated core.Memory
		if err := s.cfg.Files.Mutate(ctx, target.FilePath, func(ctx context.Context) error {
			full, rerr := s.cfg.Files.Store().ReadMemory(target.FilePath)
			if rerr != nil {
				return rerr
			}
			u, serr := lifecycle.Supersede(ctx, s.cfg.Files, full, written, s.now())
			if serr != nil {
				return serr
			}
			updated = u
			return nil
		}); err != nil {
			return Released{}, fmt.Errorf("screening.Release: %s landed, but superseding %s failed (it is still active): %w",
				lifecycle.MemoryRef(written.Project, written.Name), lifecycle.MemoryRef(target.Project, target.Name), err)
		}
		s.record(ctx, core.Event{Kind: core.EventMemorySuperseded, SessionID: h.SessionID, ProjectSlug: updated.Project,
			ItemID: updated.ID, Payload: map[string]any{"name": updated.Name, "superseded_by": written.ID, "released": h.ID}})
		out.Superseded = lifecycle.MemoryRef(updated.Project, updated.Name)
	}
	return out, nil
}

func (s *Service) releaseNoteWrite(ctx context.Context, h store.HeldWrite) (Released, error) {
	path := h.DetailValue("path")
	note, err := files.ParseNote(h.Content, path)
	if err != nil {
		return Released{}, fmt.Errorf("screening.Release: held note: %w", err)
	}
	note.FilePath = ""
	from := h.DetailValue("from_path") // a notes_update that moved the note
	base := path
	paths := []string{path}
	if from != "" && from != path {
		paths = append(paths, from)
		base = from
	}
	var written core.Note
	if err := s.cfg.Files.MutatePaths(ctx, paths, func(ctx context.Context) error {
		if err := s.checkBase(base, h.BaseHash, func(p string) (string, error) {
			cur, rerr := s.cfg.Files.Store().ReadNote(p)
			return cur.ContentHash, rerr
		}); err != nil {
			return err
		}
		if base != path && s.cfg.Files.Store().Exists(path) {
			return fmt.Errorf("a note already sits at %s: %w", path, ErrStale)
		}
		w, werr := s.cfg.Files.WriteNote(ctx, note)
		if werr != nil {
			return werr
		}
		written = w
		if base != path {
			return s.cfg.Files.Remove(ctx, base)
		}
		return nil
	}); err != nil {
		return Released{}, fmt.Errorf("screening.Release: %w", err)
	}
	s.record(ctx, core.Event{Kind: core.EventNoteWritten, SessionID: h.SessionID, ProjectSlug: written.Project,
		ItemID: written.ID, Payload: map[string]any{"title": written.Title, "updated": h.BaseHash != "", "released": h.ID}})
	return Released{ItemID: written.ID}, nil
}

func (s *Service) releaseDelete(ctx context.Context, h store.HeldWrite) (Released, error) {
	idx, found, err := store.MemoryByID(ctx, s.cfg.DB, h.ItemID)
	if err != nil {
		return Released{}, err
	}
	if !found {
		return Released{}, fmt.Errorf("screening.Release: the memory is already gone: %w", ErrStale)
	}
	if err := s.cfg.Files.Mutate(ctx, idx.FilePath, func(ctx context.Context) error {
		return s.cfg.Files.Remove(ctx, idx.FilePath)
	}); err != nil {
		return Released{}, fmt.Errorf("screening.Release: %w", err)
	}
	s.record(ctx, core.Event{Kind: core.EventMemoryArchived, SessionID: h.SessionID, ProjectSlug: idx.Project,
		ItemID: idx.ID, Payload: map[string]any{"name": idx.Name, "released": h.ID}})
	return Released{ItemID: idx.ID}, nil
}

func (s *Service) releaseGardener(ctx context.Context, h store.HeldWrite) (Released, error) {
	if s.cfg.Gardener == nil {
		return Released{}, ErrNoApplier
	}
	res, err := s.cfg.Gardener.Apply(ctx, h.DetailValue("proposal"))
	if err != nil {
		return Released{}, fmt.Errorf("screening.Release: %w", err)
	}
	return Released{ItemID: h.ItemID, Result: res}, nil
}

// checkBase compares the file at path with the hash the write was held
// against: "" means the write created the file, so it must still not exist.
// Called with the path's lock held.
func (s *Service) checkBase(path, base string, hashOf func(string) (string, error)) error {
	exists := s.cfg.Files.Store().Exists(path)
	if base == "" {
		if exists {
			return fmt.Errorf("%s was written after this was held: %w", path, ErrStale)
		}
		return nil
	}
	if !exists {
		return fmt.Errorf("%s is gone: %w", path, ErrStale)
	}
	hash, err := hashOf(path)
	if err != nil {
		return err
	}
	if hash != base {
		return fmt.Errorf("%s changed after this was held: %w", path, ErrStale)
	}
	return nil
}

// Stale reports whether a pending held write could no longer be released as
// held: its target moved on. The console offers only Discard for those.
func (s *Service) Stale(ctx context.Context, h store.HeldWrite) bool {
	switch h.Op {
	case store.HeldOpWrite:
		path := h.DetailValue("path")
		base := path
		if from := h.DetailValue("from_path"); from != "" {
			base = from
		}
		err := s.checkBase(base, h.BaseHash, func(p string) (string, error) {
			if h.ItemKind == core.ItemKindNote {
				cur, rerr := s.cfg.Files.Store().ReadNote(p)
				return cur.ContentHash, rerr
			}
			cur, rerr := s.cfg.Files.Store().ReadMemory(p)
			return cur.ContentHash, rerr
		})
		if err != nil {
			return true
		}
		if tid := h.DetailValue("supersedes_id"); tid != "" {
			t, found, terr := store.MemoryByID(ctx, s.cfg.DB, tid)
			return terr != nil || !found || !t.Active()
		}
		return false
	case store.HeldOpDelete:
		_, found, err := store.MemoryByID(ctx, s.cfg.DB, h.ItemID)
		return err != nil || !found
	case store.HeldOpGardener:
		p, found, err := store.ProposalByID(ctx, s.cfg.DB, h.DetailValue("proposal"))
		return err != nil || !found || p.Status != store.ProposalPending
	}
	return false
}
