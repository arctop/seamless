package gardener

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
	"github.com/arctop/seamless/internal/transcripts"
)

// keeperFunc is a TranscriptKeeper that records the cutoffs it was given.
type keeperFunc func(cutoff time.Time)

func (k keeperFunc) KeepEnded(_ context.Context, cutoff time.Time) { k(cutoff) }

// The reaper settles the flagged sessions it expired -- and any other that
// ended without its end signal -- after every pass, reaped or not, with the
// idle threshold as the cutoff, so a completed session's end signal has its
// time to come first.
func TestReapStaleSessions_SettlesTranscriptsEveryPass(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	g := New(db, nil, nil, nil, events.NewRecorder(db), Config{SessionIdle: 30 * time.Minute}, slog.Default())
	g.now = func() time.Time { return now }
	var cutoffs []time.Time
	g.SetTranscripts(keeperFunc(func(cutoff time.Time) { cutoffs = append(cutoffs, cutoff) }))
	g.reapStaleSessions(ctx)
	require.Equal(t, []time.Time{now.Add(-30 * time.Minute)}, cutoffs, "nothing reaped: settled all the same")
}

// End to end with the transcript copies: a flagged session the reaper expires
// is copied where the daemon may read the file (peer_auth off), and recorded
// pending under peer_auth, for the agent's later hooks to send; a live
// flagged session waits for its end signal.
func TestReapStaleSessions_KeepsAReapedFlaggedTranscript(t *testing.T) {
	const host = "alpha"
	line := `{"type":"user","message":{"content":"crashed"}}` + "\n"
	for _, tc := range []struct {
		name     string
		peerAuth bool
		want     core.TranscriptCopy
		kept     bool
	}{
		{"the daemon reads the file", false, core.TranscriptCopied, true},
		{"peer_auth: pending for the agent's hooks", true, core.TranscriptPending, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			rec := events.NewRecorder(db)
			copies, err := transcripts.New(transcripts.Config{DataDir: dir, DB: db, Events: rec, LocalHost: host,
				PeerAuth: tc.peerAuth, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
			require.NoError(t, err)
			path := filepath.Join(t.TempDir(), "crashed.jsonl")
			require.NoError(t, os.WriteFile(path, []byte(line), 0o600))

			now := time.Now().UTC()
			mk := func(name string, updated time.Time) core.Session {
				t.Helper()
				sess := core.Session{ID: mustID(t), Name: name, Status: core.SessionActive, Ambient: true,
					ExternalClient: "claude-code", ExternalSessionID: name, Host: host, Source: "startup",
					CreatedAt: updated, UpdatedAt: updated}
				require.NoError(t, store.CreateSession(ctx, db, sess))
				require.NoError(t, store.SetSessionTranscriptPath(ctx, db, sess.ID, path))
				flagged, err := copies.Flag(ctx, sess, core.VerdictBad, "it crashed", "agent", false)
				require.NoError(t, err)
				return flagged
			}
			crashed := mk("cc/crashed", now.Add(-2*time.Hour))
			live := mk("cc/live", now.Add(-time.Minute))

			g := New(db, nil, nil, nil, rec, Config{SessionIdle: 30 * time.Minute}, slog.Default())
			g.now = func() time.Time { return now }
			g.SetTranscripts(copies)
			g.reapStaleSessions(ctx)
			copies.Wait()

			got, _, err := store.SessionByID(ctx, db, crashed.ID)
			require.NoError(t, err)
			require.Equal(t, core.SessionExpired, got.Status)
			require.Equal(t, tc.want, got.Transcript)
			require.Equal(t, tc.kept, copies.Kept(crashed.ID))
			if tc.kept {
				rc, err := copies.Open(crashed.ID)
				require.NoError(t, err)
				b, err := io.ReadAll(rc)
				require.NoError(t, err)
				require.NoError(t, rc.Close())
				require.Equal(t, line, string(b))
			}
			gotLive, _, err := store.SessionByID(ctx, db, live.ID)
			require.NoError(t, err)
			require.Equal(t, core.SessionActive, gotLive.Status)
			require.Empty(t, gotLive.Transcript, "a live session waits for its end signal")
			require.False(t, copies.Kept(live.ID))
		})
	}
}
