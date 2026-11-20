// Package transcripts keeps the transcript copies of flagged sessions.
//
// A session flagged with a verdict (session_update / session_end) has its
// agent client's transcript -- the whole JSONL conversation -- copied into
// <data_dir>/transcripts/<session ULID>.jsonl.gz, so the conversation can be
// re-read after the client prunes its own copy. Only flagged sessions are
// copied, and clearing the flag deletes the copy; the event log keeps the
// history of both.
//
// Who reads the agent's file decides how a copy is made:
//
//   - A daemon running as the agent's own account, on the agent's computer,
//     copies the file itself (Snapshot), in the background, at the session's
//     end signal (Claude Code's SessionEnd, each Codex Stop), or at once for a
//     session flagged after it ended.
//   - Under peer_auth the daemon opens no path an agent names: the agent's own
//     `seam hook` sends the transcript, gzip-compressed, in a request of its
//     own, when the daemon's answer to the end signal asks for it
//     (harvest.TranscriptWantedHeader), and Receive keeps it.
//   - On any other server, for an agent on another computer, no copy is made,
//     and the record says why (core.TranscriptRemote).
//
// A copy is written to a temporary file in the same directory, synced and
// renamed into place, 0600 in a 0700 directory, and recorded on the session's
// row only while the session is still flagged. Copies of one session are
// serialized; a flag cleared while a copy is in flight wins.
//
// The copies hold whole conversations: other projects' content and pasted
// secrets, possibly. Reading one is the owner's alone (the console route and
// the seam CLI through it); this package serves no route itself.
package transcripts

import (
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/harvest"
	"github.com/arctop/seamless/internal/store"
)

// DirName is the copies' directory under the data dir.
const DirName = "transcripts"

// Suffix is every copy's file-name suffix: <session ULID>.jsonl.gz.
const Suffix = ".jsonl.gz"

// tmpPrefix starts every partial copy's name; New sweeps any it finds.
const tmpPrefix = ".tmp-"

// copyTimeout bounds one background copy.
const copyTimeout = 2 * time.Minute

// uploadSlack is how far an upload's compressed body may run past the
// uncompressed cap: deflate's own overhead is far below it, and a body past it
// is padding, never a transcript.
const uploadSlack int64 = 1 << 20

// The vias an event names: who read the agent's file.
const (
	ViaDaemon = "daemon"
	ViaClient = "client"
)

// ErrNoCopy reports that a session has no copy on this disk.
var ErrNoCopy = errors.New("transcripts: no copy kept")

// Config carries the Service's dependencies.
type Config struct {
	// DataDir is the instance root; copies go under DirName in it.
	DataDir string
	// DB is the daemon's main (write) handle.
	DB *sql.DB
	// Events records the flag and copy events; nil records none.
	Events *events.Recorder
	// LocalHost is this daemon's own machine name (config.Hostname): a
	// session recorded on another host is another computer's, whose files the
	// daemon does not read.
	LocalHost string
	// MaxBytes caps a copy, uncompressed; 0 is harvest.MaxTranscriptBytes.
	MaxBytes int64
	Logger   *slog.Logger
}

// Service keeps the copies.
type Service struct {
	dir       string
	db        *sql.DB
	events    *events.Recorder
	localHost string
	max       int64
	logger    *slog.Logger
	locks     keyLocks
	wg        sync.WaitGroup
	now       func() time.Time
}

// New builds a Service over cfg.DataDir and removes the partial copies a
// daemon that stopped mid-copy left behind (one daemon serves a data dir, so
// none can be in flight yet).
func New(cfg Config) (*Service, error) {
	if cfg.DataDir == "" {
		return nil, errors.New("transcripts.New: empty data dir")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = harvest.MaxTranscriptBytes
	}
	s := &Service{
		dir: filepath.Join(cfg.DataDir, DirName), db: cfg.DB, events: cfg.Events,
		localHost: strings.ToLower(strings.TrimSpace(cfg.LocalHost)), max: cfg.MaxBytes, logger: cfg.Logger,
		locks: keyLocks{m: map[string]*keyLock{}}, now: time.Now,
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("transcripts.New: %w", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tmpPrefix) {
			if err := os.Remove(filepath.Join(s.dir, e.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				s.logger.Warn("transcripts: remove partial copy", "file", e.Name(), "error", err)
			}
		}
	}
	return s, nil
}

// MaxBytes is the largest transcript a copy keeps, uncompressed.
func (s *Service) MaxBytes() int64 { return s.max }

// Wait blocks until every background copy has finished.
func (s *Service) Wait() { s.wg.Wait() }

// file is the copy's path for session id; ok is false for an id that cannot
// name a file (only a session ULID does).
func (s *Service) file(id string) (string, bool) {
	if !store.LooksLikeSessionULID(id) {
		return "", false
	}
	return filepath.Join(s.dir, id+Suffix), true
}

// Kept reports whether session id's copy is on this disk.
func (s *Service) Kept(id string) bool {
	path, ok := s.file(id)
	if !ok {
		return false
	}
	fi, err := os.Lstat(path)
	return err == nil && fi.Mode().IsRegular()
}

// Open opens session id's copy for reading, decompressed: the transcript's
// JSONL as it was copied. ErrNoCopy when none is on this disk.
func (s *Service) Open(id string) (io.ReadCloser, error) {
	path, ok := s.file(id)
	if !ok {
		return nil, ErrNoCopy
	}
	f, err := os.Open(path) //nolint:gosec // the path is this service's own copy, named by a session ULID
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoCopy
	}
	if err != nil {
		return nil, fmt.Errorf("transcripts.Open: %w", err)
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("transcripts.Open: %w", err)
	}
	return readCloser{Reader: gz, close: func() error {
		return errors.Join(gz.Close(), f.Close())
	}}, nil
}

type readCloser struct {
	io.Reader
	close func() error
}

func (r readCloser) Close() error { return r.close() }

// Flag sets sess's flag (verdict and reason) and records session.flagged. by
// names who set it (agent or owner); retroactive says the session had ended.
func (s *Service) Flag(ctx context.Context, sess core.Session, verdict core.SessionVerdict, reason, by string, retroactive bool) (core.Session, error) {
	unlock := s.locks.lock(sess.ID)
	defer unlock()
	flagged, err := store.SetSessionFlag(ctx, s.db, sess.ID, verdict, reason, s.now())
	if err != nil {
		return core.Session{}, fmt.Errorf("transcripts.Flag: %w", err)
	}
	payload := map[string]any{"verdict": string(verdict), "by": by, "retroactive": retroactive}
	if reason != "" {
		payload["reason"] = reason
	}
	s.record(ctx, core.EventSessionFlagged, flagged, payload)
	return flagged, nil
}

// Unflag clears session id's flag, removes its copy and records
// session.unflagged. cleared is false, and nothing is recorded, when the
// session carries no flag; a copy left on disk without one (a restored
// database) is removed all the same.
func (s *Service) Unflag(ctx context.Context, id, by string) (cleared, removed bool, err error) {
	unlock := s.locks.lock(id)
	defer unlock()
	prior, cleared, err := store.ClearSessionFlag(ctx, s.db, id)
	if err != nil {
		return false, false, fmt.Errorf("transcripts.Unflag: %w", err)
	}
	// The database first, then the file: a removal that fails leaves a copy
	// no flag names, which the next unflag of the session removes and the
	// next copy replaces.
	if path, ok := s.file(id); ok {
		switch err := os.Remove(path); {
		case err == nil:
			removed = true
		case !errors.Is(err, os.ErrNotExist):
			return cleared, false, fmt.Errorf("transcripts.Unflag: remove copy: %w", err)
		}
	}
	if cleared {
		s.record(ctx, core.EventSessionUnflagged, prior, map[string]any{
			"verdict": string(prior.Verdict), "by": by, "copy_removed": removed,
		})
	}
	return cleared, removed, nil
}

// Readable reports whether the daemon itself may read the transcript of a
// session on host for the request in ctx, and when not, the word for why: no
// peer_auth View (the daemon runs as the agent's account), and the agent's
// own computer (an empty host is an older client's, on this one).
func (s *Service) Readable(ctx context.Context, host string) (ok bool, why core.TranscriptCopy) {
	if _, peerAuth := access.FromContext(ctx); peerAuth {
		return false, core.TranscriptNotPushed
	}
	host = strings.ToLower(strings.TrimSpace(host))
	if host != "" && host != s.localHost {
		return false, core.TranscriptRemote
	}
	return true, ""
}

// AfterEnd makes the copy of a session flagged after it ended: now, when the
// daemon may read its transcript (Readable), else the record says why not.
// An active session that a hook names (its own path, or an agent client's
// session id it is linked to) waits for its end signal; one no hook names --
// session_start without hooks -- has no transcript, and the record says so.
// An explicit session's transcript is the one its linked agent session names
// (store.TranscriptPathOf). It reports the copy's state as the flag's caller
// should hear it: "" while a copy is being made or waits for the end.
func (s *Service) AfterEnd(ctx context.Context, sess core.Session) core.TranscriptCopy {
	if sess.Status == core.SessionActive && (sess.TranscriptPath != "" || sess.ExternalSessionID != "") {
		return "" // the end signal makes it
	}
	path, err := store.TranscriptPathOf(ctx, s.db, sess)
	if err != nil {
		s.logger.Warn("transcripts: transcript path", "session", sess.ID, "error", err)
		s.NotMade(ctx, sess, core.TranscriptFailed, ViaDaemon)
		return core.TranscriptFailed
	}
	if path == "" {
		s.NotMade(ctx, sess, core.TranscriptNoPath, ViaDaemon)
		return core.TranscriptNoPath
	}
	if ok, why := s.Readable(ctx, sess.Host); !ok {
		s.NotMade(ctx, sess, why, ViaDaemon)
		return why
	}
	s.Snapshot(ctx, sess.ID, path, false)
	return ""
}

// NotMade records why flagged session sess has no copy, unless an earlier copy
// is still kept (that one stands), and the event.
func (s *Service) NotMade(ctx context.Context, sess core.Session, why core.TranscriptCopy, via string) {
	unlock := s.locks.lock(sess.ID)
	defer unlock()
	s.notMade(ctx, sess, why, via)
}

// notMade is NotMade under the session's lock.
func (s *Service) notMade(ctx context.Context, sess core.Session, why core.TranscriptCopy, via string) {
	if !s.Kept(sess.ID) {
		if _, err := store.SetTranscriptCopy(ctx, s.db, sess.ID, why, 0, s.now()); err != nil {
			s.logger.Warn("transcripts: record copy", "session", sess.ID, "error", err)
		}
	}
	s.record(ctx, core.EventSessionTranscript, sess, map[string]any{"copy": string(why), "via": via})
}

// Snapshot copies the transcript at path into session id's copy, in the
// background: the daemon reads the file itself, so the caller has judged that
// it may (Readable). force copies it even when it has not grown past the copy
// already kept; Codex's per-turn Stop passes false, so a rollout is copied
// again only once it grew.
func (s *Service) Snapshot(ctx context.Context, id, path string, force bool) {
	ctx = context.WithoutCancel(ctx)
	s.wg.Go(func() {
		ctx, cancel := context.WithTimeout(ctx, copyTimeout)
		defer cancel()
		s.copyFile(ctx, id, path, force)
	})
}

// copyFile is Snapshot's work, under the session's lock.
func (s *Service) copyFile(ctx context.Context, id, path string, force bool) {
	unlock := s.locks.lock(id)
	defer unlock()
	sess, found, err := store.SessionByID(ctx, s.db, id)
	if err != nil {
		s.logger.Warn("transcripts: load session", "session", id, "error", err)
		return
	}
	if !found || !sess.Flagged() {
		return // the flag was cleared first: nothing to keep
	}
	f, err := os.Open(path) //nolint:gosec // the agent's own transcript, opened only where Readable says the daemon may
	if err != nil {
		s.notMade(ctx, sess, core.TranscriptUnreadable, ViaDaemon)
		return
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		s.notMade(ctx, sess, core.TranscriptUnreadable, ViaDaemon)
		return
	}
	kept := s.Kept(id) && sess.Transcript == core.TranscriptCopied
	if !force && kept && fi.Size() <= sess.TranscriptBytes {
		return // not grown since the copy kept
	}
	if fi.Size() > s.max {
		s.notMade(ctx, sess, core.TranscriptTooLarge, ViaDaemon)
		return
	}
	tmp, raw, state, err := s.writeCompressed(f)
	if err != nil {
		s.logger.Warn("transcripts: copy", "session", id, "error", err)
	}
	if state != core.TranscriptCopied {
		s.notMade(ctx, sess, state, ViaDaemon)
		return
	}
	s.commit(ctx, sess, tmp, raw, ViaDaemon)
}

// writeCompressed gzips r into a new temporary file in the copies' directory,
// synced and closed. It reads at most one byte past the cap, so a file that
// grew past it since its size was read is refused, never cut short.
func (s *Service) writeCompressed(r io.Reader) (tmp string, raw int64, state core.TranscriptCopy, err error) {
	t, err := s.createTemp()
	if err != nil {
		return "", 0, core.TranscriptFailed, err
	}
	gz, err := gzip.NewWriterLevel(t, gzip.BestSpeed)
	if err != nil {
		return "", 0, core.TranscriptFailed, s.discard(t, err)
	}
	raw, err = io.Copy(gz, io.LimitReader(r, s.max+1))
	if err != nil {
		return "", 0, core.TranscriptUnreadable, s.discard(t, err)
	}
	if raw > s.max {
		return "", 0, core.TranscriptTooLarge, s.discard(t, nil)
	}
	if err := gz.Close(); err != nil {
		return "", 0, core.TranscriptFailed, s.discard(t, err)
	}
	if err := finish(t); err != nil {
		return "", 0, core.TranscriptFailed, s.discard(nil, err, t.Name())
	}
	return t.Name(), raw, core.TranscriptCopied, nil
}

// Receive keeps a transcript the agent's own hook sent (peer_auth): body is
// its gzip-compressed JSONL, claimed the size its sender read. The body is
// checked as it is written -- it must decode whole, and to no more than the
// cap -- and the copy then goes to every session in sessions that is still
// flagged. It returns what became of it.
func (s *Service) Receive(ctx context.Context, sessions []core.Session, body io.Reader, claimed int64) core.TranscriptCopy {
	if len(sessions) == 0 {
		return ""
	}
	if claimed > s.max {
		for _, sess := range sessions {
			s.NotMade(ctx, sess, core.TranscriptTooLarge, ViaClient)
		}
		return core.TranscriptTooLarge
	}
	tmp, raw, state, err := s.receiveCompressed(io.LimitReader(body, s.max+uploadSlack))
	if err != nil {
		s.logger.Warn("transcripts: receive", "session", sessions[0].ID, "error", err)
	}
	if state != core.TranscriptCopied {
		for _, sess := range sessions {
			s.NotMade(ctx, sess, state, ViaClient)
		}
		return state
	}
	for i, sess := range sessions {
		src := tmp
		if i < len(sessions)-1 {
			// Every session but the last takes a copy of the received file;
			// the last takes the file itself.
			if src, err = s.duplicate(tmp); err != nil {
				s.logger.Warn("transcripts: duplicate copy", "session", sess.ID, "error", err)
				s.NotMade(ctx, sess, core.TranscriptFailed, ViaClient)
				continue
			}
		}
		func() {
			unlock := s.locks.lock(sess.ID)
			defer unlock()
			cur, found, err := store.SessionByID(ctx, s.db, sess.ID)
			if err != nil || !found || !cur.Flagged() {
				_ = os.Remove(src)
				return
			}
			s.commit(ctx, cur, src, raw, ViaClient)
		}()
	}
	return core.TranscriptCopied
}

// receiveCompressed writes r -- gzip -- to a new temporary file as it arrives,
// decoding it on the way to count what it holds: a body that does not decode
// whole, or that holds more than the cap, is refused.
func (s *Service) receiveCompressed(r io.Reader) (tmp string, raw int64, state core.TranscriptCopy, err error) {
	t, err := s.createTemp()
	if err != nil {
		return "", 0, core.TranscriptFailed, err
	}
	gz, err := gzip.NewReader(io.TeeReader(r, t))
	if err != nil {
		return "", 0, core.TranscriptFailed, s.discard(t, err)
	}
	raw, err = io.Copy(io.Discard, io.LimitReader(gz, s.max+1))
	if err != nil {
		return "", 0, core.TranscriptFailed, s.discard(t, err)
	}
	if raw > s.max {
		return "", 0, core.TranscriptTooLarge, s.discard(t, nil)
	}
	if err := gz.Close(); err != nil {
		return "", 0, core.TranscriptFailed, s.discard(t, err)
	}
	if err := finish(t); err != nil {
		return "", 0, core.TranscriptFailed, s.discard(nil, err, t.Name())
	}
	return t.Name(), raw, core.TranscriptCopied, nil
}

// commit renames tmp into sess's copy and records it, under the session's
// lock; a commit that fails removes tmp.
func (s *Service) commit(ctx context.Context, sess core.Session, tmp string, raw int64, via string) {
	path, ok := s.file(sess.ID)
	if !ok {
		_ = os.Remove(tmp)
		s.notMade(ctx, sess, core.TranscriptFailed, via)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		s.logger.Warn("transcripts: keep copy", "session", sess.ID, "error", err)
		s.notMade(ctx, sess, core.TranscriptFailed, via)
		return
	}
	syncDir(s.dir)
	recorded, err := store.SetTranscriptCopy(ctx, s.db, sess.ID, core.TranscriptCopied, raw, s.now())
	if err != nil {
		s.logger.Warn("transcripts: record copy", "session", sess.ID, "error", err)
	}
	if err == nil && !recorded {
		// The flag went between the check and the record: so does the copy.
		_ = os.Remove(path)
		return
	}
	s.record(ctx, core.EventSessionTranscript, sess, map[string]any{
		"copy": string(core.TranscriptCopied), "bytes": raw, "via": via,
	})
}

// createTemp opens a new temporary file in the copies' directory, creating
// the directory (0700) the first time.
func (s *Service) createTemp() (*os.File, error) {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return nil, fmt.Errorf("transcripts: create %s: %w", s.dir, err)
	}
	f, err := os.CreateTemp(s.dir, tmpPrefix+"*") // 0600
	if err != nil {
		return nil, fmt.Errorf("transcripts: create temp: %w", err)
	}
	return f, nil
}

// duplicate copies the finished file src to a new temporary file.
func (s *Service) duplicate(src string) (string, error) {
	in, err := os.Open(src) //nolint:gosec // this service's own temporary file
	if err != nil {
		return "", err
	}
	defer func() { _ = in.Close() }()
	t, err := s.createTemp()
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(t, in); err != nil {
		return "", s.discard(t, err)
	}
	if err := finish(t); err != nil {
		return "", s.discard(nil, err, t.Name())
	}
	return t.Name(), nil
}

// discard closes and removes a temporary file (or removes the named ones) and
// returns err.
func (s *Service) discard(t *os.File, err error, names ...string) error {
	if t != nil {
		_ = t.Close()
		names = append(names, t.Name())
	}
	for _, n := range names {
		if rerr := os.Remove(n); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			s.logger.Warn("transcripts: remove partial copy", "file", filepath.Base(n), "error", rerr)
		}
	}
	return err
}

// finish syncs and closes a written temporary file.
func finish(t *os.File) error {
	if err := t.Sync(); err != nil {
		_ = t.Close()
		return err
	}
	return t.Close()
}

// syncDir syncs dir so a rename into it survives a crash; best-effort.
func syncDir(dir string) {
	d, err := os.Open(dir) //nolint:gosec // the copies' own directory
	if err != nil {
		return
	}
	_ = d.Sync() //nolint:errcheck // best-effort: some filesystems refuse a directory sync
	_ = d.Close()
}

// record appends one event about sess.
func (s *Service) record(ctx context.Context, kind core.EventKind, sess core.Session, payload map[string]any) {
	if s.events == nil {
		return
	}
	if _, err := s.events.Record(ctx, core.Event{
		Kind: kind, SessionID: sess.ID, ProjectSlug: sess.ProjectSlug, Payload: payload,
	}); err != nil {
		s.logger.Warn("transcripts: record event", "kind", kind, "error", err)
	}
}

// keyLocks serializes the work on one session's copy.
type keyLocks struct {
	mu sync.Mutex
	m  map[string]*keyLock
}

type keyLock struct {
	sync.Mutex
	refs int
}

// lock takes id's lock and returns its release.
func (k *keyLocks) lock(id string) func() {
	k.mu.Lock()
	l := k.m[id]
	if l == nil {
		l = &keyLock{}
		k.m[id] = l
	}
	l.refs++
	k.mu.Unlock()
	l.Lock()
	return func() {
		l.Unlock()
		k.mu.Lock()
		if l.refs--; l.refs == 0 {
			delete(k.m, id)
		}
		k.mu.Unlock()
	}
}

// WaitFor is Wait bounded by d: it reports whether every background copy
// finished in time. A copy still running after d is abandoned to its own
// timeout, and its partial file is swept at the next start.
func (s *Service) WaitFor(d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		s.logger.Warn("transcripts: a copy was still running at shutdown", "waited", d)
		return false
	}
}

// Dir is the copies' directory.
func (s *Service) Dir() string { return s.dir }
