package feed

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// The spool: what the feed keeps while its consumer is down. A folder of its
// own (0700, refused when its group or others can reach it) holding
// state.json and segment files seg-<n>.ndjson, one frame per line, appended to
// and never rewritten. A segment holds at most a batch's worth of frames, so
// it is one batch.
//
//   - Bounded: past maxEvents frames or maxBytes bytes the oldest segment not
//     in flight is dropped whole, and the drop counter grows by its frames. A
//     frame longer than frameBytes is never spooled and counts as dropped.
//   - One batch in flight, oldest first, resent unchanged under its id until
//     a receipt settles or discards it -- across restarts too: the in-flight
//     batch's id and drop count live in state.json.
//   - Ids never repeat: <epoch>.<segment>, a random epoch chosen when the
//     spool is made and segment numbers that only grow.
//   - Crash order reports drops twice rather than hide one: a settled
//     segment's file goes before state.json is written, a dropped or
//     discarded one's after. A line half written when the process died is cut
//     and counted as dropped.
//   - One writer: a lock file holding the holder's pid.
//
// state.json also carries the feed's cursor into the event log (the newest
// event spooled) and whether the feed was active, so a restart resumes where
// it stopped.

// Spool bounds.
const (
	spoolMaxEvents     = 10000
	spoolMaxBytes      = 8 << 20
	spoolHeaderRoom    = 1024
	spoolSegmentFrames = 500
	spoolSegmentBytes  = 1<<20 - spoolHeaderRoom
	spoolFrameBytes    = 16384
	spoolMaxFields     = 32
)

var segmentRe = regexp.MustCompile(`^seg-(\d{12})\.ndjson$`)

func segmentName(n int) string { return fmt.Sprintf("seg-%012d.ndjson", n) }

type spoolInflight struct {
	Segment int    `json:"segment"`
	Batch   string `json:"batch"`
	Dropped int    `json:"dropped"`
}

type spoolState struct {
	V           int            `json:"v"`
	Epoch       string         `json:"epoch"`
	NextSegment int            `json:"next_segment"`
	NextSeq     int64          `json:"next_seq"`
	Dropped     int            `json:"dropped"`
	Inflight    *spoolInflight `json:"inflight"`
	// The feed's own bookkeeping: the event log row last spooled and that
	// event's id (to re-find the row if the log was rewritten), and whether
	// the feed was on.
	CursorRow int64  `json:"cursor_row"`
	CursorID  string `json:"cursor_id"`
	Active    bool   `json:"active"`
}

type spoolSegment struct {
	n      int
	events int
	bytes  int
}

// spool is one open spool. It is not safe for concurrent use; the feed's
// single loop goroutine owns it.
type spool struct {
	dir      string
	lockPath string
	st       spoolState
	segs     []spoolSegment
	events   int
	bytes    int
	closed   bool
	mu       sync.Mutex // guards stats() against the loop
}

// errSpoolHeld: another process holds the spool.
var errSpoolHeld = errors.New("feed: the spool is held by another process")

// openSpool opens (creating when absent) the spool in dir.
func openSpool(dir string) (*spool, error) {
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("feed: spool %q is not an absolute path", dir)
	}
	if err := checkSpoolFolder(dir); err != nil {
		return nil, err
	}
	lockPath, err := lockSpool(dir)
	if err != nil {
		return nil, err
	}
	sp := &spool{dir: dir, lockPath: lockPath}
	if err := sp.load(); err != nil {
		_ = os.Remove(lockPath) //nolint:errcheck // best effort: the open already failed
		return nil, err
	}
	return sp, nil
}

func checkSpoolFolder(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("feed: spool: %w", err)
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("feed: spool: %w", err)
	}
	if !st.IsDir() {
		return fmt.Errorf("feed: spool %s is not a folder (a symbolic link is refused too)", dir)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("feed: spool %s has mode %o; the spool is the daemon's alone (chmod 700)", dir, st.Mode().Perm())
	}
	if !ownedByUs(st) {
		return fmt.Errorf("feed: another user owns the spool %s", dir)
	}
	return nil
}

// lockSpool takes the spool's lock file, taking over one left by a process
// that is gone.
func lockSpool(dir string) (string, error) {
	path := filepath.Join(dir, "lock")
	for range 2 {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			_, werr := f.WriteString(strconv.Itoa(os.Getpid()))
			cerr := f.Close()
			if werr != nil || cerr != nil {
				_ = os.Remove(path) //nolint:errcheck // best effort: the lock is unusable either way
				return "", fmt.Errorf("feed: spool lock: %w", errors.Join(werr, cerr))
			}
			return path, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", fmt.Errorf("feed: spool lock: %w", err)
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return "", fmt.Errorf("feed: spool lock: %w", rerr)
		}
		pid, perr := strconv.Atoi(strings.TrimSpace(string(raw)))
		if perr == nil && pid > 0 && processAlive(pid) {
			return "", fmt.Errorf("%w (pid %d)", errSpoolHeld, pid)
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("feed: spool lock: %w", err)
		}
	}
	return "", errors.New("feed: could not take the spool lock")
}

func (sp *spool) statePath() string { return filepath.Join(sp.dir, "state.json") }
func (sp *spool) segPath(n int) string {
	return filepath.Join(sp.dir, segmentName(n))
}

// load scans the segments (cutting a half-written last line) and reads or
// makes state.json.
func (sp *spool) load() error {
	entries, err := os.ReadDir(sp.dir)
	if err != nil {
		return fmt.Errorf("feed: spool: %w", err)
	}
	var nums []int
	for _, e := range entries {
		if m := segmentRe.FindStringSubmatch(e.Name()); m != nil {
			n, _ := strconv.Atoi(m[1]) //nolint:errcheck // twelve digits, matched above
			nums = append(nums, n)
		}
	}
	slices.Sort(nums)
	cut := 0
	var maxSeq int64
	for _, n := range nums {
		seg, c, seq, err := scanSegment(sp.segPath(n))
		if err != nil {
			return err
		}
		cut += c
		if seg.events == 0 {
			if err := os.Remove(sp.segPath(n)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("feed: spool: %w", err)
			}
			continue
		}
		seg.n = n
		sp.segs = append(sp.segs, seg)
		sp.events += seg.events
		sp.bytes += seg.bytes
		maxSeq = max(maxSeq, seq)
	}
	top := 0
	if len(sp.segs) > 0 {
		top = sp.segs[len(sp.segs)-1].n
	}
	st, found, err := readSpoolState(sp.statePath())
	if err != nil {
		return err
	}
	if !found {
		epoch, err := newEpoch()
		if err != nil {
			return err
		}
		st = spoolState{V: 1, Epoch: epoch, NextSegment: top + 1, NextSeq: 1}
	}
	st.NextSegment = max(st.NextSegment, top+1)
	st.NextSeq = max(st.NextSeq, maxSeq+1)
	st.Dropped += cut
	if st.Inflight != nil && !slices.ContainsFunc(sp.segs, func(s spoolSegment) bool { return s.n == st.Inflight.Segment }) {
		st.Inflight = nil
	}
	sp.st = st
	return sp.persist()
}

func newEpoch() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("feed: spool epoch: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

var epochRe = regexp.MustCompile(`^[0-9a-f]{16}$`)

func readSpoolState(path string) (spoolState, bool, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return spoolState{}, false, nil
	}
	if err != nil {
		return spoolState{}, false, fmt.Errorf("feed: spool state: %w", err)
	}
	var st spoolState
	if err := json.Unmarshal(raw, &st); err != nil {
		return spoolState{}, false, fmt.Errorf("feed: %s is not JSON; look at it before the spool is used again: %w", path, err)
	}
	ok := st.V == 1 && epochRe.MatchString(st.Epoch) && st.NextSegment >= 1 && st.NextSeq >= 1 && st.Dropped >= 0 &&
		(st.Inflight == nil || (st.Inflight.Segment >= 1 && batchIDRe.MatchString(st.Inflight.Batch) && st.Inflight.Dropped >= 0))
	if !ok {
		return spoolState{}, false, fmt.Errorf("feed: %s is not a spool's state; look at it before the spool is used again", path)
	}
	return st, true, nil
}

// scanSegment counts a segment's complete lines, cutting a half-written last
// line off the file (cut is 1 then), and reads the last line's source_seq.
func scanSegment(path string) (spoolSegment, int, int64, error) {
	buf, err := os.ReadFile(path)
	if err != nil {
		return spoolSegment{}, 0, 0, fmt.Errorf("feed: spool segment: %w", err)
	}
	end := len(buf)
	cut := 0
	if end > 0 && buf[end-1] != '\n' {
		end = bytes.LastIndexByte(buf, '\n') + 1
		if err := os.Truncate(path, int64(end)); err != nil {
			return spoolSegment{}, 0, 0, fmt.Errorf("feed: spool segment: %w", err)
		}
		cut = 1
	}
	lines := bytes.Split(buf[:end], []byte{'\n'})
	lines = lines[:len(lines)-1] // the empty tail after the last "\n"
	var seq int64
	if len(lines) > 0 {
		var f struct {
			Fields struct {
				SourceSeq int64 `json:"source_seq"`
			} `json:"fields"`
		}
		if json.Unmarshal(lines[len(lines)-1], &f) == nil {
			seq = f.Fields.SourceSeq
		}
	}
	return spoolSegment{events: len(lines), bytes: end}, cut, seq, nil
}

// persist writes state.json atomically.
func (sp *spool) persist() error {
	raw, err := json.Marshal(sp.st)
	if err != nil {
		return fmt.Errorf("feed: spool state: %w", err)
	}
	tmp := sp.statePath() + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("feed: spool state: %w", err)
	}
	if err := os.Rename(tmp, sp.statePath()); err != nil {
		return fmt.Errorf("feed: spool state: %w", err)
	}
	return nil
}

// add spools one frame, stamping fields.source_seq (unless the frame has 32
// fields already). It reports false for a frame longer than frameBytes, which
// is counted as dropped instead.
func (sp *spool) add(f Frame) (bool, error) {
	if sp.closed {
		return false, errors.New("feed: spool closed")
	}
	sp.mu.Lock()
	defer sp.mu.Unlock()
	seq := int64(0)
	if len(f.Fields) < spoolMaxFields {
		seq = sp.st.NextSeq
		if f.Fields == nil {
			f.Fields = map[string]any{}
		}
		f.Fields["source_seq"] = seq
	}
	raw, err := json.Marshal(f)
	if err != nil {
		return false, fmt.Errorf("feed: frame: %w", err)
	}
	if len(raw) > spoolFrameBytes {
		sp.st.Dropped++
		return false, sp.persist()
	}
	if seq != 0 {
		sp.st.NextSeq = seq + 1
	}
	line := append(raw, '\n')
	var cur *spoolSegment
	if n := len(sp.segs); n > 0 {
		cur = &sp.segs[n-1]
	}
	if cur == nil || (sp.st.Inflight != nil && cur.n == sp.st.Inflight.Segment) ||
		cur.events >= spoolSegmentFrames || cur.bytes+len(line) > spoolSegmentBytes {
		sp.segs = append(sp.segs, spoolSegment{n: sp.st.NextSegment})
		sp.st.NextSegment++
		if err := sp.persist(); err != nil {
			return false, err
		}
		cur = &sp.segs[len(sp.segs)-1]
	}
	fh, err := os.OpenFile(sp.segPath(cur.n), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return false, fmt.Errorf("feed: spool segment: %w", err)
	}
	_, werr := fh.Write(line)
	cerr := fh.Close()
	if werr != nil || cerr != nil {
		return false, fmt.Errorf("feed: spool segment: %w", errors.Join(werr, cerr))
	}
	cur.events++
	cur.bytes += len(line)
	sp.events++
	sp.bytes += len(line)
	for sp.events > spoolMaxEvents || sp.bytes > spoolMaxBytes {
		dropped, err := sp.dropOldest()
		if err != nil {
			return true, err
		}
		if !dropped {
			break
		}
	}
	return true, nil
}

// dropOldest drops the oldest segment that is not in flight, counting its
// frames as dropped: the counter is written first, then the file goes.
func (sp *spool) dropOldest() (bool, error) {
	at := slices.IndexFunc(sp.segs, func(s spoolSegment) bool {
		return sp.st.Inflight == nil || s.n != sp.st.Inflight.Segment
	})
	if at < 0 {
		return false, nil
	}
	victim := sp.segs[at]
	sp.st.Dropped += victim.events
	if err := sp.persist(); err != nil {
		return false, err
	}
	if err := os.Remove(sp.segPath(victim.n)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("feed: spool: %w", err)
	}
	sp.forget(victim.n)
	return true, nil
}

func (sp *spool) forget(n int) {
	at := slices.IndexFunc(sp.segs, func(s spoolSegment) bool { return s.n == n })
	if at < 0 {
		return
	}
	sp.events -= sp.segs[at].events
	sp.bytes -= sp.segs[at].bytes
	sp.segs = slices.Delete(sp.segs, at, at+1)
}

// pendingBatch is the batch to send.
type pendingBatch struct {
	id      string
	body    []byte
	frames  int
	dropped int
}

// next returns the batch in flight, or puts the oldest segment in flight with
// the drop counter as it stands; nil when there is nothing to send.
func (sp *spool) next() (*pendingBatch, error) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.st.Inflight == nil {
		if len(sp.segs) == 0 {
			return nil, nil
		}
		oldest := sp.segs[0]
		sp.st.Inflight = &spoolInflight{Segment: oldest.n, Batch: sp.st.Epoch + "." + strconv.Itoa(oldest.n), Dropped: sp.st.Dropped}
		if err := sp.persist(); err != nil {
			return nil, err
		}
	}
	raw, err := os.ReadFile(sp.segPath(sp.st.Inflight.Segment))
	if err != nil {
		return nil, fmt.Errorf("feed: spool segment: %w", err)
	}
	text := strings.TrimSuffix(string(raw), "\n")
	var lines []string
	if text != "" {
		lines = strings.Split(text, "\n")
	}
	body, err := encodeBatch(sp.st.Inflight.Batch, sp.st.Inflight.Dropped, lines)
	if err != nil {
		return nil, err
	}
	return &pendingBatch{id: sp.st.Inflight.Batch, body: body, frames: len(lines), dropped: sp.st.Inflight.Dropped}, nil
}

// heartbeatOnly reports whether the spool has nothing to send.
func (sp *spool) empty() bool {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	return len(sp.segs) == 0
}

// settle acts on a receipt for the batch in flight: settle (the segment goes,
// then the counter drops by what the consumer recorded), discard (the counter
// grows by the segment's frames, then the segment goes) or retry (nothing).
func (sp *spool) settle(r Receipt) (string, error) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.st.Inflight == nil {
		return "", errors.New("feed: no batch is in flight")
	}
	n := sp.st.Inflight.Segment
	switch r.Action {
	case ActionSettle:
		if err := os.Remove(sp.segPath(n)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("feed: spool: %w", err)
		}
		sp.forget(n)
		sp.st.Dropped = max(0, sp.st.Dropped-max(0, r.Dropped))
		sp.st.Inflight = nil
		return ActionSettle, sp.persist()
	case ActionDiscard:
		for _, s := range sp.segs {
			if s.n == n {
				sp.st.Dropped += s.events
			}
		}
		sp.st.Inflight = nil
		if err := sp.persist(); err != nil {
			return "", err
		}
		if err := os.Remove(sp.segPath(n)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("feed: spool: %w", err)
		}
		sp.forget(n)
		return ActionDiscard, nil
	}
	return ActionRetry, nil
}

// setCursor records the newest event spooled (and that the feed is active).
func (sp *spool) setCursor(row int64, id string, active bool) error {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	sp.st.CursorRow, sp.st.CursorID, sp.st.Active = row, id, active
	return sp.persist()
}

// SpoolStats is the spool's state, for the heartbeat and the logs.
type SpoolStats struct {
	Events   int
	Bytes    int
	Segments int
	Dropped  int
	Inflight string
}

func (sp *spool) stats() SpoolStats {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	st := SpoolStats{Events: sp.events, Bytes: sp.bytes, Segments: len(sp.segs), Dropped: sp.st.Dropped}
	if sp.st.Inflight != nil {
		st.Inflight = sp.st.Inflight.Batch
	}
	return st
}

// close releases the lock.
func (sp *spool) close() {
	if sp.closed {
		return
	}
	sp.closed = true
	// A lock that cannot be removed is harmless: it holds this process's pid,
	// and a dead pid's lock is taken over at the next open.
	_ = os.Remove(sp.lockPath) //nolint:errcheck // see above
}
