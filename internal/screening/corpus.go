package screening

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/store"
)

// The corpus watch (write screening, part 2): the review that marks the
// existing corpus safe, the drift watch that quarantines a file changed
// outside Seamless, and the lockdown switch. Every part runs only while the
// screening feature is on.
//
// One record backs all three (store.CorpusRecord, migration 029): for each
// memory and note file, the hash Seamless last wrote (drift compares the file
// with it), the review's verdict and the digest it judged (the safe mark holds
// while the item still digests the same), and the drift quarantine.
//
// The watch is ARMED while the feature is on. files.Manager records its writes
// only while it is armed, so the record never claims a write that happened
// while nobody recorded. Arming the first time runs the baseline review, which
// records every file; arming again after the feature was off resyncs the
// hashes (a change made while the watch was off was never watched, so it is
// adopted, not reported) and leaves every trust mark as it was.

// ErrNotDrifted: an accept was asked of a file that is not in drift quarantine.
var ErrNotDrifted = errors.New("the file is not in drift quarantine")

// ErrNotCorpus: the path names no memory or note file Seamless can read.
var ErrNotCorpus = errors.New("not a memory or note file")

// Owner-confirmation sources added by part 2.
const (
	ConfirmedViaReview = "review" // the owner marked a flagged item safe
	ConfirmedViaDrift  = "drift"  // the owner accepted a file changed outside Seamless
)

// On reports whether the screening feature is on for this request.
func (s *Service) On(ctx context.Context) bool {
	return s.cfg.Enabled != nil && s.cfg.Enabled(ctx)
}

// corpusItem is one corpus file as the watch reads it.
type corpusItem struct {
	path     string
	raw      string
	hash     string
	itemKind string
	id       string
	project  string
	text     string // what the screens read (MemoryText / NoteText)
	digest   string // the trust digest: core.MemoryDigest, or a note's file hash
}

// readItem reads and parses one corpus file. ok is false for a file that is
// not a corpus item (gone, unparseable, or without an id): the index skips
// those too, so they reach no agent.
func (s *Service) readItem(path string) (corpusItem, bool, error) {
	raw, err := s.cfg.Files.Store().ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return corpusItem{}, false, nil
	}
	if err != nil {
		if errors.Is(err, files.ErrSymlink) || errors.Is(err, files.ErrNotRegular) {
			return corpusItem{}, false, nil
		}
		return corpusItem{}, false, err
	}
	it := corpusItem{path: path, raw: raw, hash: files.ContentHash(raw)}
	switch {
	case isMemoryPath(path):
		m, perr := files.ParseMemory(raw, path)
		if perr != nil || m.ID == "" {
			return corpusItem{}, false, nil
		}
		it.itemKind, it.id, it.project = core.ItemKindMemory, m.ID, m.Project
		it.text, it.digest = MemoryText(m), core.MemoryDigest(m)
	case isNotePath(path):
		n, perr := files.ParseNote(raw, path)
		if perr != nil || n.ID == "" {
			return corpusItem{}, false, nil
		}
		it.itemKind, it.id, it.project = core.ItemKindNote, n.ID, n.Project
		it.text, it.digest = NoteText(n), it.hash
	default:
		return corpusItem{}, false, nil
	}
	return it, true, nil
}

func isMemoryPath(p string) bool { return len(p) > 7 && p[:7] == "memory/" }
func isNotePath(p string) bool   { return len(p) > 6 && p[:6] == "notes/" }

// scanCodes is every verdict code the screens find in text, holding or not.
func (s *Service) scanCodes(text string) []Code {
	return Scan(text, s.cfg.Rules).Codes()
}

// Sync arms or disarms the watch to match the feature: the first arming runs
// the baseline review, a later one resyncs. It is safe to call from anywhere,
// any number of times.
func (s *Service) Sync(ctx context.Context) error {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	armed, err := store.CorpusArmed(ctx, s.cfg.DB)
	if err != nil {
		return fmt.Errorf("screening.Sync: %w", err)
	}
	if !s.On(ctx) {
		if armed {
			if err := store.SetCorpusArmed(ctx, s.cfg.DB, false); err != nil {
				return fmt.Errorf("screening.Sync: %w", err)
			}
		}
		return nil
	}
	if armed {
		return nil
	}
	// Arm first: a write landing during the pass below is then recorded by its
	// writer, and the pass takes each file's lock, so the two cannot interleave.
	if err := store.SetCorpusArmed(ctx, s.cfg.DB, true); err != nil {
		return fmt.Errorf("screening.Sync: %w", err)
	}
	_, baselined, err := store.SettingTime(ctx, s.cfg.DB, store.SettingCorpusBaseline)
	if err != nil {
		return fmt.Errorf("screening.Sync: %w", err)
	}
	if !baselined {
		if _, err := s.review(ctx, store.ReviewByReview, true); err != nil {
			return fmt.Errorf("screening.Sync: baseline review: %w", err)
		}
		return nil
	}
	return s.resync(ctx)
}

// resync records every file's current hash, keeping trust marks and drift
// quarantines as they are.
func (s *Service) resync(ctx context.Context) error {
	paths, err := s.cfg.Files.Store().CorpusPaths()
	if err != nil {
		return fmt.Errorf("screening.resync: %w", err)
	}
	for _, p := range paths {
		if err := s.cfg.Files.Mutate(ctx, p, func(ctx context.Context) error {
			it, ok, rerr := s.readItem(p)
			if rerr != nil || !ok {
				return rerr
			}
			return store.SetCorpusHash(ctx, s.cfg.DB, p, it.itemKind, it.id, it.project, it.hash, store.CorpusViaResync, s.now())
		}); err != nil {
			return fmt.Errorf("screening.resync: %s: %w", p, err)
		}
	}
	return s.dropMissing(ctx, paths, false)
}

// ReviewSummary is what one review pass found.
type ReviewSummary struct {
	Items    int
	Safe     int
	Flagged  int
	Baseline bool
}

// Review runs the review over the whole corpus on the owner's request: every
// memory and note is screened again, clean ones are marked safe at their
// current content, and the rest are flagged for the owner. An item the owner
// marked safe keeps the mark while its content is unchanged. It needs the
// feature on (the watch armed): the drift check runs first, so a file changed
// outside Seamless is quarantined rather than adopted by the review.
func (s *Service) Review(ctx context.Context) (ReviewSummary, error) {
	if err := s.Sync(ctx); err != nil {
		return ReviewSummary{}, err
	}
	if !s.On(ctx) {
		return ReviewSummary{}, errors.New("screening.Review: write screening is off")
	}
	if _, err := s.CheckDrift(ctx); err != nil {
		return ReviewSummary{}, err
	}
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	return s.review(ctx, store.ReviewByOwner, false)
}

// review is the pass itself. baseline records every file's hash as Seamless's
// (the first arming); otherwise hashes are left to the write path and the
// drift watch, and a file with no record yet is left to the drift watch.
func (s *Service) review(ctx context.Context, by string, baseline bool) (ReviewSummary, error) {
	paths, err := s.cfg.Files.Store().CorpusPaths()
	if err != nil {
		return ReviewSummary{}, fmt.Errorf("screening.review: %w", err)
	}
	sum := ReviewSummary{Baseline: baseline}
	for _, p := range paths {
		if err := s.cfg.Files.Mutate(ctx, p, func(ctx context.Context) error {
			it, ok, rerr := s.readItem(p)
			if rerr != nil || !ok {
				return rerr
			}
			rec, found, rerr := store.CorpusRecordByPath(ctx, s.cfg.DB, p)
			if rerr != nil {
				return rerr
			}
			if baseline || !found {
				if !baseline {
					return nil // unrecorded while armed: the drift watch's to judge
				}
				if err := store.SetCorpusHash(ctx, s.cfg.DB, p, it.itemKind, it.id, it.project, it.hash, store.CorpusViaReview, s.now()); err != nil {
					return err
				}
			}
			sum.Items++
			if found && rec.ReviewBy == store.ReviewByOwner && rec.ReviewStatus == store.ReviewSafe && rec.ReviewDigest == it.digest {
				sum.Safe++ // the owner's own mark, on unchanged content, stands
				return nil
			}
			codes := s.scanCodes(it.text)
			status := store.ReviewSafe
			if len(HoldCodes(codes)) > 0 {
				status = store.ReviewFlagged
				sum.Flagged++
			} else {
				sum.Safe++
			}
			return store.SetCorpusReview(ctx, s.cfg.DB, p, status, it.digest, Strings(codes), store.ReviewByReview, s.now())
		}); err != nil {
			return ReviewSummary{}, fmt.Errorf("screening.review: %s: %w", p, err)
		}
	}
	if baseline {
		if err := s.dropMissing(ctx, paths, false); err != nil {
			return ReviewSummary{}, err
		}
		if err := store.SetSettingTime(ctx, s.cfg.DB, store.SettingCorpusBaseline, s.now()); err != nil {
			return ReviewSummary{}, fmt.Errorf("screening.review: %w", err)
		}
	}
	if err := store.SetSettingTime(ctx, s.cfg.DB, store.SettingCorpusReviewed, s.now()); err != nil {
		return ReviewSummary{}, fmt.Errorf("screening.review: %w", err)
	}
	s.record(ctx, core.Event{Kind: core.EventCorpusReviewed, Payload: map[string]any{
		"items": sum.Items, "safe": sum.Safe, "flagged": sum.Flagged, "by": by, "baseline": baseline}})
	return sum, nil
}

// DriftSummary is what one drift check found.
type DriftSummary struct {
	Checked  int
	Changed  int
	Unknown  int
	Missing  int
	Restored int
}

// CheckDrift compares every memory and note file with the hash Seamless last
// recorded for it. A file that differs, or that no Seamless write is on record
// for, is quarantined IN PLACE: it stays on disk untouched, the screens run
// over its text, corpus.drift is recorded, and it is kept out of briefing and
// prompt injection until the owner accepts it. A recorded file that is gone
// is reported and its record dropped; nothing is restored. A quarantined file
// put back to the exact bytes it was recorded at leaves quarantine.
//
// It runs only while the watch is armed and the feature is on.
func (s *Service) CheckDrift(ctx context.Context) (DriftSummary, error) {
	var sum DriftSummary
	if !s.On(ctx) {
		return sum, nil
	}
	armed, err := store.CorpusArmed(ctx, s.cfg.DB)
	if err != nil || !armed {
		return sum, err
	}
	paths, err := s.cfg.Files.Store().CorpusPaths()
	if err != nil {
		return sum, fmt.Errorf("screening.CheckDrift: %w", err)
	}
	for _, p := range paths {
		if err := s.cfg.Files.Mutate(ctx, p, func(ctx context.Context) error {
			return s.checkOne(ctx, p, &sum)
		}); err != nil {
			return sum, fmt.Errorf("screening.CheckDrift: %s: %w", p, err)
		}
	}
	missing, err := s.missing(ctx, paths)
	if err != nil {
		return sum, err
	}
	sum.Missing = len(missing)
	if err := s.dropMissing(ctx, paths, true); err != nil {
		return sum, err
	}
	return sum, nil
}

// checkOne judges one file, with its lock held.
func (s *Service) checkOne(ctx context.Context, p string, sum *DriftSummary) error {
	it, ok, err := s.readItem(p)
	if err != nil || !ok {
		return err
	}
	sum.Checked++
	rec, found, err := store.CorpusRecordByPath(ctx, s.cfg.DB, p)
	if err != nil {
		return err
	}
	switch {
	case found && rec.Drifted():
		if rec.DriftBase != "" && it.hash == rec.DriftBase {
			sum.Restored++
			return store.ClearCorpusDrift(ctx, s.cfg.DB, p, it.hash, store.CorpusViaResync, s.now())
		}
		if it.hash == rec.DriftHash {
			return nil // quarantined already, unchanged since
		}
		return s.quarantine(ctx, it, rec.DriftChange, sum)
	case found && it.hash == rec.Hash:
		return nil
	case found:
		// A writer outside the corpus lock could have written the file a moment
		// before recording it: read the record once more before calling it drift.
		again, ok2, err := store.CorpusRecordByPath(ctx, s.cfg.DB, p)
		if err != nil {
			return err
		}
		if ok2 && again.Hash == it.hash {
			return nil
		}
		return s.quarantine(ctx, it, store.DriftChanged, sum)
	default:
		return s.quarantine(ctx, it, store.DriftUnknown, sum)
	}
}

// quarantine records a drift and its verdict codes, and corpus.drift.
func (s *Service) quarantine(ctx context.Context, it corpusItem, change string, sum *DriftSummary) error {
	codes := s.scanCodes(it.text)
	if err := store.SetCorpusDrift(ctx, s.cfg.DB, it.path, it.itemKind, it.id, it.project, change, it.hash,
		Strings(codes), s.now()); err != nil {
		return err
	}
	if change == store.DriftUnknown {
		sum.Unknown++
	} else {
		sum.Changed++
	}
	s.record(ctx, core.Event{Kind: core.EventCorpusDrift, ProjectSlug: it.project, ItemID: it.id,
		Payload: map[string]any{"change": change, "item_kind": it.itemKind, "verdicts": Strings(codes), "quarantined": true}})
	return nil
}

// missing lists the recorded files that are no longer on disk.
func (s *Service) missing(ctx context.Context, paths []string) ([]store.CorpusRecord, error) {
	present := make(map[string]bool, len(paths))
	for _, p := range paths {
		present[p] = true
	}
	recs, err := store.CorpusRecords(ctx, s.cfg.DB)
	if err != nil {
		return nil, err
	}
	var out []store.CorpusRecord
	for _, r := range recs {
		if !present[r.FilePath] {
			out = append(out, r)
		}
	}
	return out, nil
}

// dropMissing drops the records of files that are gone, each re-checked under
// its lock (a Seamless remove takes the record with it; this is for the rest).
// report records corpus.drift for each: a file gone without a Seamless remove.
func (s *Service) dropMissing(ctx context.Context, paths []string, report bool) error {
	gone, err := s.missing(ctx, paths)
	if err != nil {
		return err
	}
	for _, r := range gone {
		if err := s.cfg.Files.Mutate(ctx, r.FilePath, func(ctx context.Context) error {
			if s.cfg.Files.Store().Exists(r.FilePath) {
				return nil
			}
			if _, still, err := store.CorpusRecordByPath(ctx, s.cfg.DB, r.FilePath); err != nil || !still {
				return err
			}
			if err := store.DeleteCorpusRecord(ctx, s.cfg.DB, r.FilePath); err != nil {
				return err
			}
			if report {
				s.record(ctx, core.Event{Kind: core.EventCorpusDrift, ProjectSlug: r.Project, ItemID: r.ItemID,
					Payload: map[string]any{"change": store.DriftMissing, "item_kind": r.ItemKind, "quarantined": false}})
			}
			return nil
		}); err != nil {
			return fmt.Errorf("screening: drop missing %s: %w", r.FilePath, err)
		}
	}
	return nil
}

// MarkSafe is the owner's mark on a flagged item: safe at its current
// content. A memory is also stamped owner-confirmed (via review), so its
// provenance mark says so and lockdown injects it.
func (s *Service) MarkSafe(ctx context.Context, path string) error {
	return s.confirm(ctx, path, ConfirmedViaReview, false)
}

// AcceptDrift is the owner accepting a file changed outside Seamless as it
// stands: its hash is recorded as Seamless's, the quarantine lifts, and a
// memory is stamped owner-confirmed (via drift).
func (s *Service) AcceptDrift(ctx context.Context, path string) error {
	return s.confirm(ctx, path, ConfirmedViaDrift, true)
}

func (s *Service) confirm(ctx context.Context, path, via string, drift bool) error {
	var it corpusItem
	if err := s.cfg.Files.Mutate(ctx, path, func(ctx context.Context) error {
		read, ok, err := s.readItem(path)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%s: %w", path, ErrNotCorpus)
		}
		it = read
		rec, found, err := store.CorpusRecordByPath(ctx, s.cfg.DB, path)
		if err != nil {
			return err
		}
		if drift {
			if !found || !rec.Drifted() {
				return fmt.Errorf("%s: %w", path, ErrNotDrifted)
			}
			if err := store.ClearCorpusDrift(ctx, s.cfg.DB, path, it.hash, store.CorpusViaAccept, s.now()); err != nil {
				return err
			}
		} else if !found {
			if err := store.SetCorpusHash(ctx, s.cfg.DB, path, it.itemKind, it.id, it.project, it.hash, store.CorpusViaReview, s.now()); err != nil {
				return err
			}
		}
		return store.SetCorpusReview(ctx, s.cfg.DB, path, store.ReviewSafe, it.digest, Strings(s.scanCodes(it.text)),
			store.ReviewByOwner, s.now())
	}); err != nil {
		return fmt.Errorf("screening.confirm: %w", err)
	}
	if it.itemKind == core.ItemKindMemory {
		if err := store.UpsertOwnerConfirmation(ctx, s.cfg.DB, it.id, it.digest, via, s.now()); err != nil {
			return fmt.Errorf("screening.confirm: %w", err)
		}
	}
	s.record(ctx, core.Event{Kind: core.EventCorpusConfirmed, ProjectSlug: it.project, ItemID: it.id,
		Payload: map[string]any{"item_kind": it.itemKind, "via": via, "by": "owner"}})
	return nil
}

// ---------------------------------------------------------------------------
// Lockdown
// ---------------------------------------------------------------------------

// Lockdown sources.
const (
	LockdownConsole = "console" // the owner's switch on the Held screen
	LockdownFile    = "file"    // the configured flag file exists
)

// LockdownState is whether lockdown is on, and what holds it.
type LockdownState struct {
	On      bool
	Console bool // the owner's switch is on
	File    bool // the configured flag file exists (or cannot be checked)
	// FilePath is the configured flag file ("" when none is configured).
	FilePath string
}

// Source names what holds the lockdown: console, file, or both.
func (l LockdownState) Source() string {
	switch {
	case l.Console && l.File:
		return LockdownConsole + "+" + LockdownFile
	case l.File:
		return LockdownFile
	case l.Console:
		return LockdownConsole
	}
	return ""
}

// Lockdown reports the lockdown state. It is off whenever the feature is off.
// A flag file that cannot be checked (anything but "it does not exist")
// counts as present: lockdown only ever reduces what agents receive, so the
// doubt goes that way.
func (s *Service) Lockdown(ctx context.Context) LockdownState {
	st := LockdownState{FilePath: s.cfg.LockdownFile}
	if !s.On(ctx) {
		return st
	}
	on, err := store.LockdownOn(ctx, s.cfg.DB)
	if err != nil {
		s.cfg.Logger.Warn("screening: lockdown switch unreadable; treating it as on", "error", err)
		on = true
	}
	st.Console = on
	st.File = s.lockdownFilePresent()
	st.On = st.Console || st.File
	return st
}

// LockdownOn is Lockdown(ctx).On: the form retrieve and the MCP surface take.
func (s *Service) LockdownOn(ctx context.Context) bool { return s.Lockdown(ctx).On }

func (s *Service) lockdownFilePresent() bool {
	if s.cfg.LockdownFile == "" {
		return false
	}
	_, err := os.Lstat(s.cfg.LockdownFile)
	if errors.Is(err, fs.ErrNotExist) {
		return false
	}
	if err != nil {
		s.cfg.Logger.Warn("screening: lockdown file unreadable; treating lockdown as on", "error", err)
	}
	return true
}

// SetLockdown turns the owner's lockdown switch on or off and records
// lockdown.changed. Turning it off leaves a lockdown the flag file holds in
// place: that one lifts only when the file goes.
func (s *Service) SetLockdown(ctx context.Context, on bool) (LockdownState, error) {
	if err := store.SetLockdown(ctx, s.cfg.DB, on); err != nil {
		return LockdownState{}, fmt.Errorf("screening.SetLockdown: %w", err)
	}
	s.record(ctx, core.Event{Kind: core.EventLockdownChanged, Payload: map[string]any{
		"on": on, "source": LockdownConsole, "by": "owner"}})
	return s.Lockdown(ctx), nil
}

// noteLockdownFile records lockdown.changed when the flag file appears or
// goes. The last state seen is kept in memory: a restart re-reports a file
// that is present, which is the conservative direction.
func (s *Service) noteLockdownFile(ctx context.Context) {
	if s.cfg.LockdownFile == "" || !s.On(ctx) {
		return
	}
	present := s.lockdownFilePresent()
	s.lockMu.Lock()
	changed := !s.fileSeen || present != s.filePresent
	first := !s.fileSeen
	s.fileSeen, s.filePresent = true, present
	s.lockMu.Unlock()
	if !changed || (first && !present) {
		return
	}
	s.record(ctx, core.Event{Kind: core.EventLockdownChanged, Payload: map[string]any{
		"on": present, "source": LockdownFile, "by": "file"}})
}

// ---------------------------------------------------------------------------
// The loop
// ---------------------------------------------------------------------------

// lockdownPoll is how often the loop looks for the flag file appearing or
// going, to record the change. Enforcement does not wait for it: every write
// and every briefing checks the file itself.
const lockdownPoll = 15 * time.Second

// Run keeps the watch in step with the feature until ctx ends: it syncs at
// once and whenever the optional features change, checks drift every
// DriftInterval (0: never), and records the flag file's changes. Call Wait
// after cancelling ctx.
func (s *Service) Run(ctx context.Context) {
	s.runDone = make(chan struct{})
	go func() {
		defer close(s.runDone)
		var sub <-chan core.Event
		if s.cfg.Events != nil {
			ch, unsub := s.cfg.Events.Subscribe()
			defer unsub()
			sub = ch
		}
		s.tick(ctx, true)
		var drift <-chan time.Time
		if s.cfg.DriftInterval > 0 {
			t := time.NewTicker(s.cfg.DriftInterval)
			defer t.Stop()
			drift = t.C
		}
		poll := time.NewTicker(lockdownPoll)
		defer poll.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case e, ok := <-sub:
				if !ok {
					sub = nil
					continue
				}
				if e.Kind == core.EventFeaturesChanged {
					s.tick(ctx, false)
				}
			case <-drift:
				s.tick(ctx, true)
			case <-poll.C:
				s.noteLockdownFile(ctx)
			}
		}
	}()
}

// Wait blocks until Run's loop has returned (immediately when Run never ran).
func (s *Service) Wait() {
	if s.runDone != nil {
		<-s.runDone
	}
}

// tick syncs the watch, checks drift when asked, and notes the flag file.
// Failures are logged: the loop keeps going, and the next tick retries.
func (s *Service) tick(ctx context.Context, drift bool) {
	if err := s.Sync(ctx); err != nil {
		s.cfg.Logger.Warn("screening: corpus watch sync", "error", err)
		return
	}
	if drift && s.cfg.DriftInterval > 0 {
		if sum, err := s.CheckDrift(ctx); err != nil {
			s.cfg.Logger.Warn("screening: drift check", "error", err)
		} else if sum.Changed+sum.Unknown+sum.Missing > 0 {
			s.cfg.Logger.Info("screening: corpus drift", "changed", sum.Changed, "unknown", sum.Unknown, "missing", sum.Missing)
		}
	}
	s.noteLockdownFile(ctx)
}
