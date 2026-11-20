package transcripts

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

const testHost = "alpha"

type fixture struct {
	svc  *Service
	db   *sql.DB
	dir  string
	tdir string // where the agent's transcripts live
}

func newFixture(t *testing.T, maxBytes int64) fixture {
	t.Helper()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	svc, err := New(Config{DataDir: dir, DB: db, Events: events.NewRecorder(db), LocalHost: testHost, MaxBytes: maxBytes})
	require.NoError(t, err)
	return fixture{svc: svc, db: db, dir: dir, tdir: t.TempDir()}
}

// session creates a session row and returns it.
func (f fixture) session(t *testing.T, id string, status core.SessionStatus, host, path string) core.Session {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	s := core.Session{ID: id, Name: "cc/" + strings.ToLower(id), Status: status, Ambient: true,
		ExternalClient: "claude-code", ExternalSessionID: "ext-" + id, Host: host, Source: "startup",
		CreatedAt: now, UpdatedAt: now}
	require.NoError(t, store.CreateSession(ctx, f.db, s))
	require.NoError(t, store.SetSessionTranscriptPath(ctx, f.db, id, path))
	got, ok, err := store.SessionByID(ctx, f.db, id)
	require.NoError(t, err)
	require.True(t, ok)
	return got
}

func (f fixture) flag(t *testing.T, sess core.Session) core.Session {
	t.Helper()
	got, err := f.svc.Flag(context.Background(), sess, core.VerdictGood, "went well", "agent", false)
	require.NoError(t, err)
	return got
}

func (f fixture) reload(t *testing.T, id string) core.Session {
	t.Helper()
	got, ok, err := store.SessionByID(context.Background(), f.db, id)
	require.NoError(t, err)
	require.True(t, ok)
	return got
}

func (f fixture) read(t *testing.T, id string) string {
	t.Helper()
	rc, err := f.svc.Open(id)
	require.NoError(t, err)
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	require.NoError(t, err)
	return string(b)
}

func (f fixture) write(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(f.tdir, name)
	require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
	return p
}

func (f fixture) events(t *testing.T, id string, kind core.EventKind) []core.Event {
	t.Helper()
	evs, err := events.NewRecorder(f.db).BySession(context.Background(), id, 100)
	require.NoError(t, err)
	var out []core.Event
	for _, e := range evs {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func gz(t *testing.T, s string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	_, err := w.Write([]byte(s))
	require.NoError(t, err)
	require.NoError(t, w.Close())
	return b.Bytes()
}

const line1 = `{"type":"user","message":{"content":"hello"}}` + "\n"

// The daemon's own copy: written whole, gzip, 0600 in a 0700 directory,
// recorded with its size, and read back as the JSONL it was.
func TestSnapshot_CopiesTheTranscript(t *testing.T) {
	f := newFixture(t, 0)
	path := f.write(t, "a.jsonl", line1+line1)
	sess := f.flag(t, f.session(t, "01MSNAPAAAAAAAAAAAAAAAAAAA", core.SessionCompleted, testHost, path))

	f.svc.Snapshot(context.Background(), sess.ID, path, true)
	f.svc.Wait()
	require.Equal(t, line1+line1, f.read(t, sess.ID))
	got := f.reload(t, sess.ID)
	require.Equal(t, core.TranscriptCopied, got.Transcript)
	require.Equal(t, int64(2*len(line1)), got.TranscriptBytes)
	require.NotNil(t, got.TranscriptCopiedAt)
	require.True(t, f.svc.Kept(sess.ID))
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(filepath.Join(f.dir, DirName, sess.ID+Suffix))
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
		di, err := os.Stat(filepath.Join(f.dir, DirName))
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o700), di.Mode().Perm())
	}
	evs := f.events(t, sess.ID, core.EventSessionTranscript)
	require.Len(t, evs, 1)
	require.Equal(t, "copied", evs[0].Payload["copy"])
	require.Equal(t, ViaDaemon, evs[0].Payload["via"])
}

// Codex's Stop copies again only once the rollout grew; a forced copy (Claude
// Code's SessionEnd) copies whatever its size.
func TestSnapshot_RecopiesOnlyOnGrowth(t *testing.T) {
	f := newFixture(t, 0)
	path := f.write(t, "rollout.jsonl", line1)
	sess := f.flag(t, f.session(t, "01MGROWAAAAAAAAAAAAAAAAAAA", core.SessionActive, testHost, path))
	ctx := context.Background()
	copies := func() int { return len(f.events(t, sess.ID, core.EventSessionTranscript)) }

	f.svc.Snapshot(ctx, sess.ID, path, false)
	f.svc.Wait()
	require.Equal(t, 1, copies())
	f.svc.Snapshot(ctx, sess.ID, path, false)
	f.svc.Wait()
	require.Equal(t, 1, copies(), "unchanged: not copied again")

	require.NoError(t, os.WriteFile(path, []byte(line1+line1), 0o600))
	f.svc.Snapshot(ctx, sess.ID, path, false)
	f.svc.Wait()
	require.Equal(t, 2, copies(), "grown: copied again")
	require.Equal(t, line1+line1, f.read(t, sess.ID))
	require.Equal(t, int64(2*len(line1)), f.reload(t, sess.ID).TranscriptBytes)

	f.svc.Snapshot(ctx, sess.ID, path, true)
	f.svc.Wait()
	require.Equal(t, 3, copies(), "forced: copied whatever its size")
}

// A transcript over the cap is refused whole and recorded as too large; a
// missing one as unreadable. An earlier copy stays the one kept.
func TestSnapshot_RefusesOverTheCap(t *testing.T) {
	f := newFixture(t, 64)
	big := f.write(t, "big.jsonl", strings.Repeat("x", 65))
	sess := f.flag(t, f.session(t, "01MBIGAAAAAAAAAAAAAAAAAAAA", core.SessionCompleted, testHost, big))
	f.svc.Snapshot(context.Background(), sess.ID, big, true)
	f.svc.Wait()
	require.False(t, f.svc.Kept(sess.ID))
	require.Equal(t, core.TranscriptTooLarge, f.reload(t, sess.ID).Transcript)

	gone := f.flag(t, f.session(t, "01MGONEAAAAAAAAAAAAAAAAAAA", core.SessionCompleted, testHost, "/no/such/file.jsonl"))
	f.svc.Snapshot(context.Background(), gone.ID, "/no/such/file.jsonl", true)
	f.svc.Wait()
	require.Equal(t, core.TranscriptUnreadable, f.reload(t, gone.ID).Transcript)

	// A copy kept, then a re-copy that fails: the copy kept stands.
	small := f.write(t, "small.jsonl", line1)
	kept := f.flag(t, f.session(t, "01MKEPTAAAAAAAAAAAAAAAAAAA", core.SessionActive, testHost, small))
	f.svc.Snapshot(context.Background(), kept.ID, small, true)
	f.svc.Wait()
	require.NoError(t, os.WriteFile(small, []byte(strings.Repeat("y", 65)), 0o600))
	f.svc.Snapshot(context.Background(), kept.ID, small, true)
	f.svc.Wait()
	require.Equal(t, line1, f.read(t, kept.ID))
	require.Equal(t, core.TranscriptCopied, f.reload(t, kept.ID).Transcript)
}

// Clearing the flag removes the copy and its record; a copy that arrives
// after the clear keeps nothing.
func TestUnflag_RemovesTheCopy(t *testing.T) {
	f := newFixture(t, 0)
	ctx := context.Background()
	path := f.write(t, "u.jsonl", line1)
	sess := f.flag(t, f.session(t, "01MUNFLAGAAAAAAAAAAAAAAAAA", core.SessionCompleted, testHost, path))
	f.svc.Snapshot(ctx, sess.ID, path, true)
	f.svc.Wait()
	require.True(t, f.svc.Kept(sess.ID))

	cleared, removed, err := f.svc.Unflag(ctx, sess.ID, "agent")
	require.NoError(t, err)
	require.True(t, cleared)
	require.True(t, removed)
	require.False(t, f.svc.Kept(sess.ID))
	got := f.reload(t, sess.ID)
	require.False(t, got.Flagged())
	require.Empty(t, got.Transcript)
	_, err = f.svc.Open(sess.ID)
	require.ErrorIs(t, err, ErrNoCopy)
	evs := f.events(t, sess.ID, core.EventSessionUnflagged)
	require.Len(t, evs, 1)
	require.Equal(t, "good", evs[0].Payload["verdict"])
	require.Equal(t, true, evs[0].Payload["copy_removed"])

	f.svc.Snapshot(ctx, sess.ID, path, true)
	f.svc.Wait()
	require.False(t, f.svc.Kept(sess.ID), "an unflagged session keeps no copy")
	require.Empty(t, f.svc.Receive(ctx, nil, bytes.NewReader(gz(t, line1)), -1))

	cleared, removed, err = f.svc.Unflag(ctx, sess.ID, "agent")
	require.NoError(t, err)
	require.False(t, cleared)
	require.False(t, removed)
	require.Len(t, f.events(t, sess.ID, core.EventSessionUnflagged), 1, "nothing to clear records nothing")
}

// An upload is checked as it is written: it must decode whole, to no more
// than the cap, and a claimed size over the cap is refused unread.
func TestReceive(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		body    func(t *testing.T) []byte
		claimed int64
		want    core.TranscriptCopy
	}{
		{"whole", func(t *testing.T) []byte { return gz(t, line1+line1) }, int64(2 * len(line1)), core.TranscriptCopied},
		{"not gzip", func(*testing.T) []byte { return []byte(line1) }, -1, core.TranscriptFailed},
		{"cut off", func(t *testing.T) []byte { b := gz(t, line1+line1); return b[:len(b)-6] }, -1, core.TranscriptFailed},
		{"over the cap", func(t *testing.T) []byte { return gz(t, strings.Repeat("z", 129)) }, 10, core.TranscriptTooLarge},
		{"claimed over the cap", func(t *testing.T) []byte { return gz(t, line1) }, 129, core.TranscriptTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, 128)
			a := f.flag(t, f.session(t, "01MRECVAAAAAAAAAAAAAAAAAAA", core.SessionActive, "beta", "/remote/a.jsonl"))
			b := f.flag(t, f.session(t, "01MRECVBBBBBBBBBBBBBBBBBBB", core.SessionActive, "beta", "/remote/a.jsonl"))
			got := f.svc.Receive(ctx, []core.Session{a, b}, bytes.NewReader(tc.body(t)), tc.claimed)
			require.Equal(t, tc.want, got)
			for _, s := range []core.Session{a, b} {
				require.Equal(t, tc.want == core.TranscriptCopied, f.svc.Kept(s.ID))
				require.Equal(t, tc.want, f.reload(t, s.ID).Transcript)
				if tc.want == core.TranscriptCopied {
					require.Equal(t, line1+line1, f.read(t, s.ID), "each session takes its own copy")
					require.Equal(t, ViaClient, f.events(t, s.ID, core.EventSessionTranscript)[0].Payload["via"])
				}
			}
			entries, err := os.ReadDir(filepath.Join(f.dir, DirName))
			if err == nil {
				for _, e := range entries {
					require.False(t, strings.HasPrefix(e.Name(), tmpPrefix), "no partial copy left behind: %s", e.Name())
				}
			}
		})
	}
}

// A session flagged after it ended is copied at once where the daemon may
// read its transcript; otherwise the record says why not. An active session
// waits for its end signal.
func TestAfterEnd(t *testing.T) {
	ctx := context.Background()
	policy, err := access.New(config.PeerAuth{Owner: "owner"}, func(n string) (access.Account, error) {
		return access.Account{UID: 501, Name: n}, nil
	})
	require.NoError(t, err)
	owner, ok := policy.PeerView(501)
	require.True(t, ok)
	for i, tc := range []struct {
		name   string
		ctx    context.Context
		status core.SessionStatus
		host   string
		path   bool
		want   core.TranscriptCopy
		kept   bool
	}{
		{"ended, readable", ctx, core.SessionCompleted, testHost, true, "", true},
		{"ended, older client names no host", ctx, core.SessionExpired, "", true, "", true},
		{"ended, another computer", ctx, core.SessionCompleted, "beta", true, core.TranscriptRemote, false},
		{"ended, peer_auth", access.WithView(ctx, owner), core.SessionCompleted, testHost, true, core.TranscriptNotPushed, false},
		{"ended, no hook named a transcript", ctx, core.SessionCompleted, testHost, false, core.TranscriptNoPath, false},
		{"active: the end signal copies it", ctx, core.SessionActive, testHost, true, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, 0)
			path := ""
			if tc.path {
				path = f.write(t, "t.jsonl", line1)
			}
			sess := f.flag(t, f.session(t, fmt.Sprintf("01MAFTER%018d", i), tc.status, tc.host, path))
			require.Equal(t, tc.want, f.svc.AfterEnd(tc.ctx, sess))
			f.svc.Wait()
			require.Equal(t, tc.kept, f.svc.Kept(sess.ID))
			if tc.want != "" {
				require.Equal(t, tc.want, f.reload(t, sess.ID).Transcript)
			}
		})
	}
}

// A daemon that stopped mid-copy leaves a partial file; the next start sweeps
// it and leaves the copies.
func TestNew_SweepsPartialCopies(t *testing.T) {
	dir := t.TempDir()
	tdir := filepath.Join(dir, DirName)
	require.NoError(t, os.MkdirAll(tdir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(tdir, tmpPrefix+"123"), []byte("x"), 0o600))
	keep := filepath.Join(tdir, "01MKEEPAAAAAAAAAAAAAAAAAAA"+Suffix)
	require.NoError(t, os.WriteFile(keep, gz(t, line1), 0o600))
	_, err := New(Config{DataDir: dir})
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(tdir, tmpPrefix+"123"))
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(keep)
	require.NoError(t, err)
}
