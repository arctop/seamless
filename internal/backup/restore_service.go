package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"time"

	"github.com/arctop/seamless/internal/archive"
	"github.com/arctop/seamless/internal/core"
)

// RestoreStatus is the restore side of the service's status.
type RestoreStatus struct {
	// Staging is a restore being prepared, Archiving the replaced data of the
	// last one being saved as a backup.
	Staging   *Staging `json:"staging,omitempty"`
	Archiving bool     `json:"archiving,omitempty"`
	// Pending is a restore staged and waiting for the start that applies it.
	Pending *Pending `json:"pending,omitempty"`
	// Last is the last restore's result, until the owner dismisses it.
	Last *Result `json:"last,omitempty"`
	// Failed is the last staging that failed in this process (nothing
	// changed), until dismissed or another restore starts.
	Failed *StageFailure `json:"failed,omitempty"`
}

func (r RestoreStatus) clone() RestoreStatus {
	c := r
	if r.Staging != nil {
		v := *r.Staging
		c.Staging = &v
	}
	if r.Pending != nil {
		v := *r.Pending
		v.Entries = append([]SwapEntry(nil), r.Pending.Entries...)
		c.Pending = &v
	}
	if r.Last != nil {
		v := *r.Last
		c.Last = &v
	}
	if r.Failed != nil {
		v := *r.Failed
		c.Failed = &v
	}
	return c
}

// Staging is a restore being prepared.
type Staging struct {
	Mode      string    `json:"mode"`
	Backup    BackupRef `json:"backup"`
	Step      string    `json:"step"`
	StartedAt time.Time `json:"started_at"`
}

// StageFailure is a staging that failed: nothing changed.
type StageFailure struct {
	Mode    string    `json:"mode"`
	Backup  BackupRef `json:"backup"`
	Reason  string    `json:"reason"`
	Message string    `json:"message"`
	At      time.Time `json:"at"`
}

// RestoreRequest is a restore the console asks for.
type RestoreRequest struct {
	// Ref names the backup in the current listing.
	Ref  string
	Mode string
	// ExpectSHA256 is the digest a passkey ceremony approved, "" without one.
	ExpectSHA256 string
	// By names who asked ("console"), Proof how it was approved ("passkey",
	// "owner"), and Credential the passkey that did.
	By         string
	Proof      string
	Credential string
}

// StageRestore starts preparing the restore req names, while the daemon keeps
// serving. It answers at once; the status follows the steps, and a staged
// restore restarts the daemon to apply it (where it can restart). Refusals
// are *Failure values, with ErrBusy while a backup runs.
func (s *Service) StageRestore(ctx context.Context, req RestoreRequest) (Status, error) {
	if req.Mode != ModeReplace && req.Mode != ModeMerge {
		return s.Status(), failf(RestoreReasonInternal, "unknown restore mode %q", req.Mode)
	}
	if _, _, err := parseRef(req.Ref); err != nil {
		return s.Status(), err
	}
	l, err := s.List(ctx)
	if err != nil {
		return s.Status(), err
	}
	e, ok := l.Find(req.Ref)
	if !ok {
		return s.Status(), fmt.Errorf("%w: %q", ErrNoSuchBackup, req.Ref)
	}
	if f := Restorable(e, req.Mode); f != nil {
		return s.Status(), f
	}
	if !s.started.Load() {
		return s.Status(), ErrNotRunning
	}
	reply := make(chan result, 1)
	select {
	case s.cmds <- command{kind: cmdStageRestore, entry: e, restore: req, reply: reply}:
	case <-s.done:
		return s.Status(), ErrNotRunning
	case <-ctx.Done():
		return s.Status(), ctx.Err()
	}
	select {
	case r := <-reply:
		return r.status, r.err
	case <-ctx.Done():
		return s.Status(), ctx.Err()
	}
}

// Restorable says why e cannot be restored in mode, nil when it can.
func Restorable(e Entry, mode string) *Failure {
	switch {
	case e.Dataless:
		return failf(RestoreReasonDataless, "this backup is in iCloud and not on this Mac: download it in Finder first")
	case e.TooNew:
		return failf(RestoreReasonTooNew, "a newer Seamless made this backup: update Seamless to restore it")
	case e.Manifest == nil:
		why := e.Unreadable
		if why == "" {
			why = "its contents could not be read"
		}
		return failf(RestoreReasonNotRestorable, "this backup cannot be restored: %s", why)
	case mode == ModeReplace && !e.Manifest.IncludesDB:
		return failf(RestoreReasonNoDatabase,
			"this backup holds memories and notes only, not the database, so it cannot replace everything: bring back what is missing instead")
	}
	return nil
}

// CancelRestore drops a staged restore that has not begun to apply.
func (s *Service) CancelRestore(ctx context.Context) (Status, error) {
	return s.send(ctx, cmdCancelRestore, Entry{})
}

// DismissResult hides the last restore's result (and a failed staging).
func (s *Service) DismissResult(ctx context.Context) (Status, error) {
	return s.send(ctx, cmdDismissRestore, Entry{})
}

// Digest is the SHA-256 of the backup ref names, as a restore's passkey
// ceremony binds it: computed here, from the file, never taken from a client.
func (s *Service) Digest(ctx context.Context, ref string) (Entry, string, error) {
	if _, _, err := parseRef(ref); err != nil {
		return Entry{}, "", err
	}
	l, err := s.List(ctx)
	if err != nil {
		return Entry{}, "", err
	}
	e, ok := l.Find(ref)
	if !ok {
		return Entry{}, "", fmt.Errorf("%w: %q", ErrNoSuchBackup, ref)
	}
	if e.Dataless {
		return e, "", failf(RestoreReasonDataless, "this backup is in iCloud and not on this Mac: download it in Finder first")
	}
	sum, err := FileSHA256(ctx, e.Path)
	return e, sum, err
}

// FileSHA256 is the lowercase hex SHA-256 of the file at path.
func FileSHA256(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(&ctxWriter{ctx: ctx, w: h}, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// restoreBusy reports a restore job under way.
func (s *Service) restoreBusy() bool { return s.staging != nil || s.archiving }

// restoreJobResult is what a restore job came to.
type restoreJobResult struct {
	archive bool
	pending *Pending
	result  *Result
	err     error
	req     RestoreRequest
	ref     BackupRef
}

// stageRestore starts the staging job, on the loop.
func (s *Service) stageRestore(ctx context.Context, req RestoreRequest, e Entry) error {
	switch {
	case s.running != nil, s.restoreBusy():
		return ErrBusy
	case s.pending != nil, RestorePending(s.d.DataDir):
		return ErrRestorePending
	case s.d.UpdateBusy != nil && s.d.UpdateBusy():
		return failf(RestoreReasonBusy, "Seamless is installing an update: restore once it has finished")
	}
	ref := BackupRef{Name: e.Name, Where: e.Where, Kind: e.Kind}
	now := s.d.Now()
	s.staging = &Staging{Mode: req.Mode, Backup: ref, Step: StepChecking, StartedAt: now}
	step := StepChecking
	s.stageStep.Store(&step)
	s.stageFail = nil
	jctx, cancel := context.WithCancel(ctx)
	s.cancelStage = cancel
	st := Stager{
		DataDir: s.d.DataDir, DB: s.d.DB, ForceLocked: s.d.ForceLocked, PasskeyRPID: s.d.PasskeyRPID,
		Validate: s.d.Validate, FreeBytes: s.d.FreeBytes, Version: s.d.Version, Host: s.d.Host,
		Now: s.d.Now, Log: s.log, export: s.export,
		Step: func(name string) {
			n := name
			s.stageStep.Store(&n)
			s.mu.Lock()
			if s.status.Restore.Staging != nil {
				s.status.Restore.Staging.Step = name
			}
			s.mu.Unlock()
		},
	}
	sreq := StageRequest{Path: e.Path, Backup: ref, Mode: req.Mode, ExpectSHA256: req.ExpectSHA256,
		RequestedBy: req.By, Proof: req.Proof, Credential: req.Credential}
	s.jobs.Add(1)
	go func() {
		defer s.jobs.Done()
		defer cancel()
		res := restoreJobResult{req: req, ref: ref}
		func() {
			defer func() {
				if r := recover(); r != nil {
					s.log.Error("backup: staging a restore panicked", "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
					res.err = failf(RestoreReasonInternal, "the restore stopped on an internal error; nothing changed")
				}
			}()
			res.pending, res.err = st.Stage(jctx, sreq)
		}()
		s.restoreDone <- res
	}()
	return nil
}

// finishRestore folds a restore job's result in, on the loop.
func (s *Service) finishRestore(ctx context.Context, r restoreJobResult) {
	if r.archive {
		s.archiving = false
		if r.result != nil {
			s.last = visible(r.result)
		}
		if r.err == nil && r.result != nil && r.result.Previous != "" {
			s.record(ctx, core.Event{Kind: core.EventBackupCreated, ItemID: r.result.Previous, Payload: map[string]any{
				"kind": archive.KindBeforeRestore, "where": WhereLocal,
			}})
		}
		s.publish(s.settings(ctx))
		return
	}
	s.staging, s.cancelStage = nil, nil
	if r.err != nil {
		var f *Failure
		if !errors.As(r.err, &f) {
			f = &Failure{Reason: RestoreReasonInternal, Message: r.err.Error()}
		}
		if ctx.Err() == nil {
			s.stageFail = &StageFailure{Mode: r.req.Mode, Backup: r.ref, Reason: f.Reason, Message: f.Message, At: s.d.Now()}
			s.log.Warn("backup: a restore could not be prepared; nothing changed", "mode", r.req.Mode, "reason", f.Reason, "err", f.Message)
			s.record(ctx, core.Event{Kind: core.EventBackupRestoreFailed, ItemID: r.ref.Name, Payload: map[string]any{
				"mode": r.req.Mode, "reason": f.Reason, "step": "stage", "by": r.req.By,
			}})
		}
		s.publish(s.settings(ctx))
		return
	}
	s.pending = r.pending
	s.pendingSeen = s.readPending()
	if r.pending != nil && r.pending.Mode == ModeMerge && r.pending.Safety != "" {
		s.record(ctx, core.Event{Kind: core.EventBackupCreated, ItemID: r.pending.Safety, Payload: map[string]any{
			"kind": archive.KindBeforeRestore, "where": WhereLocal,
		}})
	}
	s.publish(s.settings(ctx))
	s.askRestart("backup: a restore is staged; restarting to apply it")
}

// cancelRestore drops a staged restore, on the loop.
func (s *Service) cancelRestore() error {
	p, err := ReadPending(s.d.DataDir)
	if err != nil {
		return fmt.Errorf("backup: the staged restore's record cannot be read: %w", err)
	}
	if p == nil {
		s.pending = nil
		return nil
	}
	if p.Stage != StageStaged {
		return failf(RestoreReasonPending, "the restore is already being applied, so it cannot be cancelled")
	}
	if err := os.Remove(PendingPath(s.d.DataDir)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("backup: cancel the restore: %w", err)
	}
	_ = os.RemoveAll(workDir(s.d.DataDir, p.ID))
	s.pending = nil
	s.pendingSeen = s.readPending()
	s.log.Info("backup: a staged restore was cancelled", "id", p.ID)
	return nil
}

// dismissRestore hides the last result, on the loop.
func (s *Service) dismissRestore() error {
	s.stageFail = nil
	if s.last == nil {
		return nil
	}
	if s.last.NeedsArchive() {
		// The undo is not saved yet: the banner says so until it is.
		return failf(RestoreReasonBusy, "the data the restore replaced is still being saved as a backup")
	}
	res, err := ReadResult(s.d.DataDir)
	if err == nil && res != nil && res.ID == s.last.ID {
		res.Dismissed = true
		if err := writeRecord(LastPath(s.d.DataDir), res); err != nil {
			return fmt.Errorf("backup: dismiss the restore's result: %w", err)
		}
	}
	s.last = nil
	return nil
}

// loadRestore reads the restore records at Start: a staged restore still
// waiting (Windows, or a start that skipped it), the last result -- whose
// event is recorded here, once -- and the replaced data still waiting for its
// backup, which a job saves now.
func (s *Service) loadRestore(ctx context.Context) {
	if p, err := ReadPending(s.d.DataDir); err != nil {
		s.log.Warn("backup: the staged restore's record cannot be read", "err", err)
	} else {
		s.pending = p
	}
	res, err := ReadResult(s.d.DataDir)
	if err != nil {
		s.log.Warn("backup: the last restore's result cannot be read", "err", err)
		return
	}
	if res == nil {
		return
	}
	if !res.Recorded {
		s.recordResult(ctx, res)
		res.Recorded = true
		if err := writeRecord(LastPath(s.d.DataDir), res); err != nil {
			s.log.Warn("backup: could not save the restore's result", "err", err)
		}
	}
	s.last = visible(res)
	if res.NeedsArchive() {
		s.startArchive(ctx)
	}
}

// visible is r unless the owner dismissed it.
func visible(r *Result) *Result {
	if r == nil || r.Dismissed {
		return nil
	}
	return r
}

// recordResult records a restore's event.
func (s *Service) recordResult(ctx context.Context, r *Result) {
	if r.Outcome == RestoreDone {
		payload := map[string]any{
			"mode": r.Mode, "kind": r.Backup.Kind, "memories": r.Memories, "notes": r.Notes,
			"by": r.RequestedBy, "proof": r.Proof,
		}
		if r.Added > 0 {
			payload["added"] = r.Added
		}
		if r.Credential != "" {
			payload["credential"] = r.Credential
		}
		s.record(ctx, core.Event{Kind: core.EventBackupRestored, ItemID: r.Backup.Name, Payload: payload})
		return
	}
	s.record(ctx, core.Event{Kind: core.EventBackupRestoreFailed, ItemID: r.Backup.Name, Payload: map[string]any{
		"mode": r.Mode, "reason": r.Reason, "outcome": r.Outcome, "step": "apply",
	}})
}

// startArchive saves the replaced data as a before-restore backup, as a job.
func (s *Service) startArchive(ctx context.Context) {
	if s.archiving {
		return
	}
	s.archiving = true
	s.jobs.Add(1)
	go func() {
		defer s.jobs.Done()
		res := restoreJobResult{archive: true}
		func() {
			defer func() {
				if r := recover(); r != nil {
					s.log.Error("backup: saving the replaced data panicked", "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
					res.err = errors.New("internal error")
				}
			}()
			id, err := s.installID(ctx)
			if err != nil {
				res.err = err
				return
			}
			ar := Archiver{
				DataDir: s.d.DataDir, Version: s.d.Version, Host: s.d.Host, InstallID: id,
				ForceLocked: s.d.ForceLocked, PasskeyRPID: s.d.PasskeyRPID, Now: s.d.Now, Log: s.log, export: s.export,
			}
			res.result, res.err = ar.ArchivePrevious(ctx)
		}()
		s.restoreDone <- res
	}()
}

// restoreStatus is the restore side of the status, from the loop's state.
func (s *Service) restoreStatus() RestoreStatus {
	r := RestoreStatus{Archiving: s.archiving}
	if s.staging != nil {
		v := *s.staging
		if step := s.stageStep.Load(); step != nil {
			v.Step = *step
		}
		r.Staging = &v
	}
	if s.pending != nil {
		v := *s.pending
		r.Pending = &v
	}
	if s.last != nil {
		v := *s.last
		r.Last = &v
	}
	if s.stageFail != nil {
		v := *s.stageFail
		r.Failed = &v
	}
	return r.clone()
}

// noteFolder records the folder in force, and this install's id, for
// `seamlessd backups`.
func (s *Service) noteFolder(ctx context.Context, set Settings) {
	id := s.state.InstallID
	if id == "" && s.d.DB != nil {
		if got, err := s.installID(ctx); err == nil {
			id = got
		}
	}
	if s.state.Folder == set.Dir && s.state.InstallID == id {
		return
	}
	s.state.Folder, s.state.InstallID = set.Dir, id
	// Saved now only over a record already there: a daemon with backups off
	// writes nothing, and the next save carries it.
	if exists(statePath(s.d.DataDir)) {
		s.persist()
	}
}

// ListBackups lists the backups in dir and in dataDir's LocalDir without the
// service: the CLI's listing, which opens no database. installID tells this
// install's backups apart ("" knows none as ours or another's).
func ListBackups(dataDir, dir, installID string) Listing {
	f := folders{dataDir: dataDir, goos: runtime.GOOS}
	return lister{folders: f, cache: &manifestCache{}}.list(filepath.Clean(dir), installID, nil)
}
