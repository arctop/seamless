package archive

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/store"
)

// LockChoice is what an import does with the locks its archive carries.
type LockChoice int

const (
	// LocksKeep leaves every project the archive held locked locked here. It is
	// the zero value, so a caller that says nothing fails closed.
	LocksKeep LockChoice = iota
	// LocksRemove imports the projects without the archive's locks. A lock this
	// instance holds of its own is not the archive's to remove.
	LocksRemove
)

// lockProofImport is the proof an archive import's lock events name.
const lockProofImport = "import"

// LockOutcome is what became of an archive's locked projects. Each list is
// sorted by slug, because the archive's list is.
type LockOutcome struct {
	// Locked are the projects locked here after the import.
	Locked []string
	// Removed arrived without the archive's lock.
	Removed []string
	// StillLocked asked for removal and stayed locked: this instance's config
	// force locks them, or, on a merge, its console already locked them.
	StillLocked []string
}

// exportLocks builds the manifest's record of the locks, reading dbPath
// read-only. No file there means no console locks and no passkeys; the config's
// force locks are still recorded. A database that exists and cannot be read is
// an error: an archive that silently forgot its locks would restore those
// projects open.
func exportLocks(ctx context.Context, dbPath string, opts ExportOptions) (*Locks, error) {
	var (
		facts store.LockFacts
		db    *sql.DB
	)
	if dbPath != "" {
		_, err := os.Lstat(dbPath)
		switch {
		case err == nil:
			// mode=ro, as in inspectSnapshot: nothing may appear beside a
			// snapshot about to be tarred, and an export never writes.
			db, err = sql.Open("sqlite", "file:"+url.PathEscape(dbPath)+"?mode=ro&_pragma=busy_timeout(5000)")
			if err != nil {
				return nil, fmt.Errorf("archive.Export: read locks: %w", err)
			}
			db.SetMaxOpenConns(1)
			defer func() { _ = db.Close() }()
			if facts, err = store.ReadLockFacts(ctx, db); err != nil {
				return nil, fmt.Errorf("archive.Export: read locks: %w", err)
			}
		case !errors.Is(err, os.ErrNotExist):
			return nil, fmt.Errorf("archive.Export: read locks: %w", err)
		}
	}

	slugs := append(slices.Clone(opts.ForceLocked), facts.ConsoleLocked...)
	slices.Sort(slugs)
	slugs = slices.Compact(slugs)
	if len(slugs) == 0 {
		return nil, nil
	}
	locks := &Locks{PasskeyRPID: opts.PasskeyRPID, Passkeys: facts.Passkeys}
	for _, slug := range slugs {
		p := LockedProject{Slug: slug}
		if db != nil {
			state, err := store.IsolationOf(ctx, db, slug)
			if err != nil {
				return nil, fmt.Errorf("archive.Export: read locks: %w", err)
			}
			p.Isolation = string(state)
		}
		locks.Projects = append(locks.Projects, p)
	}
	return locks, nil
}

// canonicalLocks drops manifest entries that cannot be a project. A slug is
// printed to the owner's terminal and asked about before anything is written,
// and an archive is a file that has been copied around.
func canonicalLocks(m Manifest, rep *ImportReport) {
	if m.Locks == nil {
		return
	}
	m.Locks.Projects = slices.DeleteFunc(m.Locks.Projects, func(p LockedProject) bool {
		if core.Slugify(p.Slug) == p.Slug && p.Slug != "" {
			return false
		}
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("skipped the lock on non-slug project %q", p.Slug))
		return true
	})
}

// manifestLocks returns the projects the manifest records as locked.
func manifestLocks(m Manifest) []LockedProject {
	if m.Locks == nil {
		return nil
	}
	return slices.Clone(m.Locks.Projects)
}

// withSnapshotLocks adds the console locks of an archive's database to the
// manifest's list. An archive written before the manifest recorded locks has
// them only there, and one whose manifest was edited must not lose them.
func withSnapshotLocks(ctx context.Context, db *sql.DB, locks []LockedProject) ([]LockedProject, error) {
	facts, err := store.ReadLockFacts(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("archive.Import: read the archive's locks: %w", err)
	}
	for _, slug := range facts.ConsoleLocked {
		if slices.ContainsFunc(locks, func(p LockedProject) bool { return p.Slug == slug }) {
			continue
		}
		state, err := store.IsolationOf(ctx, db, slug)
		if err != nil {
			return nil, fmt.Errorf("archive.Import: read the archive's locks: %w", err)
		}
		locks = append(locks, LockedProject{Slug: slug, Isolation: string(state)})
	}
	slices.SortFunc(locks, func(a, b LockedProject) int { return strings.Compare(a.Slug, b.Slug) })
	return locks, nil
}

// warnRestoredApprovers says that a restored database brought its approver
// credentials back as they stood at the backup.
func warnRestoredApprovers(ctx context.Context, db *sql.DB, rep *ImportReport) error {
	n, err := store.CountApprovers(ctx, db)
	if err != nil {
		return fmt.Errorf("archive.Import: %w", err)
	}
	if n > 0 {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf(
			"restored %d approver credential(s) as they stood at this backup, including any removed since; check them with `seamlessd approvers list`", n))
	}
	return nil
}

// fence is the isolation state p needs here: the one it had, or confidential,
// the lowest state that can hold a lock, when the archive cannot say.
func (p LockedProject) fence() core.Isolation {
	if s := core.Isolation(p.Isolation); s.FencesOutbound() {
		return s
	}
	return core.IsolationConfidential
}

// checkLocksFit refuses a merge whose locks cannot go on, before it writes
// anything: a project that is open here must be fenced to hold the lock, and a
// project with children cannot be.
func checkLocksFit(ctx context.Context, db *sql.DB, locks []LockedProject) error {
	for _, p := range locks {
		_, ok, err := store.ProjectBySlug(ctx, db, p.Slug)
		if err != nil {
			return fmt.Errorf("archive.Import: lookup project %q: %w", p.Slug, err)
		}
		if !ok {
			continue
		}
		state, err := store.IsolationOf(ctx, db, p.Slug)
		if err != nil {
			return fmt.Errorf("archive.Import: %w", err)
		}
		if state.FencesOutbound() {
			continue
		}
		eff, err := store.PreviewIsolationTighten(ctx, db, p.Slug, p.fence())
		if err != nil {
			return fmt.Errorf("archive.Import: %w", err)
		}
		if eff.Blocked() {
			return fmt.Errorf("archive.Import: %w: project %q is open here and has child projects (%s), and a locked project cannot. "+
				"Nothing was changed: re-parent them first, or import without the locks",
				ErrLockCannotApply, p.Slug, strings.Join(eff.Children, ", "))
		}
	}
	return nil
}

// applyArchiveLocks makes the destination match choice for the archive's locked
// projects and records the outcome in rep. db is nil only for the dry run of a
// fresh restore, which has no database to read.
func applyArchiveLocks(ctx context.Context, db *sql.DB, opts ImportOptions, mode Mode, locks []LockedProject, choice LockChoice, rep *ImportReport) error {
	if len(locks) == 0 {
		return nil
	}
	pass := lockPass{ctx: ctx, db: db, opts: opts, mode: mode, rep: rep, out: &LockOutcome{}, now: time.Now()}
	if db != nil && !opts.DryRun {
		pass.rec = events.NewRecorder(db)
	}
	rep.Locks = pass.out
	for _, p := range locks {
		var err error
		if choice == LocksRemove {
			err = pass.remove(p)
		} else {
			err = pass.keep(p)
		}
		if err != nil {
			return fmt.Errorf("archive.Import: lock on %q: %w", p.Slug, err)
		}
	}
	return nil
}

// lockPass is one run of applyArchiveLocks.
type lockPass struct {
	ctx  context.Context //nolint:containedctx // lives for one applyArchiveLocks call
	db   *sql.DB
	opts ImportOptions
	mode Mode
	rep  *ImportReport
	out  *LockOutcome
	rec  *events.Recorder // nil on a dry run
	now  time.Time
}

func (l lockPass) dry() bool { return l.opts.DryRun }

func (l lockPass) forced(slug string) bool { return slices.Contains(l.opts.ForceLocked, slug) }

// consoleLocked reports whether the destination's console locks slug. Only a
// fresh dry run has no database, and so nothing locked yet.
func (l lockPass) consoleLocked(slug string) (bool, error) {
	if l.db == nil {
		return false, nil
	}
	return store.HasConsoleLock(l.ctx, l.db, slug)
}

// keep leaves p locked here: registered, fenced, and held by a console lock
// unless this instance's config already force locks it.
func (l lockPass) keep(p LockedProject) error {
	l.out.Locked = append(l.out.Locked, p.Slug)
	if l.db == nil {
		return nil
	}
	if err := l.fence(p); err != nil {
		return err
	}
	if l.forced(p.Slug) {
		return nil
	}
	has, err := l.consoleLocked(p.Slug)
	if err != nil || has || l.dry() {
		return err
	}
	if err := store.AddConsoleLock(l.ctx, l.db, p.Slug, store.ImportedLockCredential, l.now); err != nil {
		return err
	}
	return l.record(p.Slug, "lock")
}

// fence gives p the isolation fence a lock needs. On a merge the destination's
// project may be open: raising it changes this instance's project, which is
// said out loud, because the alternative puts locked content in an open one.
func (l lockPass) fence(p LockedProject) error {
	_, existed, err := store.ProjectBySlug(l.ctx, l.db, p.Slug)
	if err != nil {
		return err
	}
	state, err := store.IsolationOf(l.ctx, l.db, p.Slug)
	if err != nil {
		return err
	}
	if state.FencesOutbound() {
		return nil
	}
	if existed && l.mode == ModeMerge {
		l.rep.Warnings = append(l.rep.Warnings, fmt.Sprintf(
			"project %q was open here and becomes %s, because the archive holds it locked; it leaves any family or parent it had", p.Slug, p.fence()))
	}
	if l.dry() {
		return nil
	}
	if _, err := store.EnsureProject(l.ctx, l.db, p.Slug, p.Slug); err != nil {
		return err
	}
	if !existed && l.mode == ModeMerge {
		l.rep.Projects++ // the backfill after the corpus pass finds it registered
	}
	_, err = store.TightenProjectIsolation(l.ctx, l.db, p.Slug, p.fence(), l.now)
	return err
}

// remove brings p in without the archive's lock. A fresh restore's database is
// the archive's, so its lock row goes. A merge never copied one, and a lock this
// instance holds of its own stays.
func (l lockPass) remove(p LockedProject) error {
	if l.forced(p.Slug) {
		l.out.StillLocked = append(l.out.StillLocked, p.Slug)
		return nil
	}
	has, err := l.consoleLocked(p.Slug)
	if err != nil {
		return err
	}
	if has && l.mode == ModeMerge {
		l.out.StillLocked = append(l.out.StillLocked, p.Slug)
		return nil
	}
	l.out.Removed = append(l.out.Removed, p.Slug)
	if l.dry() {
		return nil
	}
	if has {
		if err := store.RemoveConsoleLock(l.ctx, l.db, p.Slug); err != nil {
			return err
		}
	}
	return l.record(p.Slug, "remove")
}

// record writes the audit event for a lock this import changed.
func (l lockPass) record(project, action string) error {
	if l.rec == nil {
		return nil
	}
	_, err := l.rec.Record(l.ctx, core.Event{Kind: core.EventConsoleLockChanged, ProjectSlug: project,
		Payload: map[string]any{"proof": lockProofImport, "action": action}})
	return err
}
