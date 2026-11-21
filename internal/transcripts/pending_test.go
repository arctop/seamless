package transcripts

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// clock is a settable fake time for the asks' waits.
type clock struct{ at time.Time }

func (c *clock) now() time.Time { return c.at }

// sessionAs creates a session row of client and account, with ext as its
// agent session id.
func (f fixture) sessionAs(t *testing.T, id, client, account, ext string, status core.SessionStatus, host, path string) core.Session {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	s := core.Session{ID: id, Name: fmt.Sprintf("x/%s", id), Status: status, Ambient: true, Account: account,
		ExternalClient: client, ExternalSessionID: ext, Host: host, Source: "startup", CreatedAt: now, UpdatedAt: now}
	require.NoError(t, store.CreateSession(ctx, f.db, s))
	require.NoError(t, store.SetSessionTranscriptPath(ctx, f.db, id, path))
	got, ok, err := store.SessionByID(ctx, f.db, id)
	require.NoError(t, err)
	require.True(t, ok)
	return got
}

// A caller's pending transcripts are its own: the same agent client, the same
// account, the same computer, never its current session, and none with a copy
// kept or an upload under way.
func TestPendingFor_TheSameCallerOnly(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, 0)
	path := f.write(t, "p.jsonl", line1)
	mine := f.flag(t, f.sessionAs(t, "01MPENDMINEAAAAAAAAAAAAAAA", "claude-code", "", "cc-mine", core.SessionExpired, testHost, path))
	older := f.flag(t, f.sessionAs(t, "01MPENDOLDHOSTNAMEAAAAAAAA", "claude-code", "", "cc-older", core.SessionCompleted, "", path))
	f.flag(t, f.sessionAs(t, "01MPENDCODEXAAAAAAAAAAAAAA", "codex", "", "cx-other", core.SessionExpired, testHost, path))
	f.flag(t, f.sessionAs(t, "01MPENDACCTAAAAAAAAAAAAAAA", "claude-code", "_example_agent", "_example_agent:cc-acct", core.SessionExpired, testHost, path))
	f.flag(t, f.sessionAs(t, "01MPENDFARAAAAAAAAAAAAAAAA", "claude-code", "", "cc-far", core.SessionExpired, "beta", path))
	current := f.flag(t, f.sessionAs(t, "01MPENDCURRENTAAAAAAAAAAAA", "claude-code", "", "cc-current", core.SessionActive, testHost, path))
	f.sessionAs(t, "01MPENDUNFLAGGEDAAAAAAAAAA", "claude-code", "", "cc-plain", core.SessionExpired, testHost, path)
	kept := f.flag(t, f.sessionAs(t, "01MPENDKEPTAAAAAAAAAAAAAAA", "claude-code", "", "cc-kept", core.SessionExpired, testHost, path))
	f.svc.Snapshot(ctx, kept.ID, path, true)
	f.svc.Wait()
	final := f.flag(t, f.sessionAs(t, "01MPENDFINALAAAAAAAAAAAAAA", "claude-code", "", "cc-final", core.SessionExpired, testHost, path))
	f.svc.NotMade(ctx, final, core.TranscriptRefused, ViaClient)

	ids := func(c Caller) []string {
		t.Helper()
		pending, err := f.svc.PendingFor(ctx, c)
		require.NoError(t, err)
		var out []string
		for _, p := range pending {
			for _, s := range p.Sessions {
				out = append(out, s.ID)
			}
		}
		return out
	}
	owner := Caller{Client: "claude-code", Host: testHost, Current: current.ExternalSessionID}
	require.ElementsMatch(t, []string{mine.ID, older.ID}, ids(owner))
	owner.Current = ""
	require.ElementsMatch(t, []string{mine.ID, older.ID, current.ID}, ids(owner), "another conversation's live session is asked for too")
	require.Equal(t, []string{"01MPENDACCTAAAAAAAAAAAAAAA"}, ids(Caller{Client: "claude-code", Account: "_example_agent", Host: testHost}))
	require.Equal(t, []string{"01MPENDFARAAAAAAAAAAAAAAAA"}, ids(Caller{Client: "claude-code", Host: "beta"}))

	end, ok := f.svc.BeginUpload([]core.Session{mine})
	require.True(t, ok)
	owner.Current = current.ExternalSessionID
	require.Equal(t, []string{older.ID}, ids(owner), "an upload under way is not asked for again")
	_, ok = f.svc.BeginUpload([]core.Session{mine, older})
	require.False(t, ok, "one upload of a session at a time")
	end()
	require.ElementsMatch(t, []string{mine.ID, older.ID}, ids(owner))
}

// Asks are bounded: each waits out a growing wait after the one before, and
// after core.TranscriptAskLimit asks nobody answered, the wait after the last
// one passed, the copy is given up on -- recorded, with the event.
func TestAsk_BoundAndGiveUp(t *testing.T) {
	ctx := context.Background()
	f := newFixtureWith(t, Config{PeerAuth: true})
	c := &clock{at: time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)}
	f.svc.now = c.now
	path := f.write(t, "a.jsonl", line1)
	sess := f.flag(t, f.sessionAs(t, "01MASKAAAAAAAAAAAAAAAAAAAA", "claude-code", "", "cc-ask", core.SessionExpired, testHost, path))
	caller := Caller{Client: "claude-code", Host: testHost}
	ask := func() bool {
		t.Helper()
		pending, err := f.svc.PendingFor(ctx, caller)
		require.NoError(t, err)
		if len(pending) == 0 {
			return false
		}
		got, ok := f.svc.Ask(ctx, pending[0])
		if ok {
			require.Equal(t, path, got)
		}
		return ok
	}

	asksOf := func() (int, *time.Time) {
		t.Helper()
		n, at, err := store.TranscriptAsksOf(ctx, f.db, sess.ID)
		require.NoError(t, err)
		return n, at
	}
	for n := 1; n <= core.TranscriptAskLimit; n++ {
		require.True(t, ask(), "ask %d", n)
		require.Equal(t, core.TranscriptPending, f.reload(t, sess.ID).Transcript)
		asks, at := asksOf()
		require.Equal(t, n, asks)
		require.Equal(t, c.at, *at)
		require.False(t, ask(), "ask %d: not again at once", n)
		c.at = c.at.Add(askWait(n) - time.Second)
		require.False(t, ask(), "ask %d: not before its wait", n)
		c.at = c.at.Add(time.Second)
	}
	require.False(t, ask(), "the limit reached: given up")
	require.Equal(t, core.TranscriptNotSent, f.reload(t, sess.ID).Transcript)
	asks, _ := asksOf()
	require.Equal(t, core.TranscriptAskLimit, asks)
	evs := f.events(t, sess.ID, core.EventSessionTranscript)
	require.Len(t, evs, core.TranscriptAskLimit+1)
	last := evs[len(evs)-1].Payload
	if evs[0].Payload["copy"] == string(core.TranscriptNotSent) {
		last = evs[0].Payload // newest first
	}
	require.Equal(t, string(core.TranscriptNotSent), last["copy"])
	pending, err := f.svc.PendingFor(ctx, caller)
	require.NoError(t, err)
	require.Empty(t, pending, "given up: asked no more")

	// A new flag asks afresh.
	f.flag(t, f.reload(t, sess.ID))
	require.Empty(t, f.reload(t, sess.ID).Transcript)
	asks, at := asksOf()
	require.Zero(t, asks)
	require.Nil(t, at)
	require.True(t, ask())
}

// A reported path ends the asks with its word; a copy that arrives ends them
// too, and a session that names no transcript is recorded as having none.
func TestAsk_EndsWithAReportOrACopy(t *testing.T) {
	ctx := context.Background()
	f := newFixtureWith(t, Config{PeerAuth: true})
	path := f.write(t, "r.jsonl", line1)
	caller := Caller{Client: "claude-code", Host: testHost}
	for _, tc := range []struct {
		name  string
		state core.TranscriptCopy
	}{{"refused", core.TranscriptRefused}, {"gone", core.TranscriptUnreadable}} {
		t.Run(tc.name, func(t *testing.T) {
			sess := f.flag(t, f.sessionAs(t, "01MREPORT"+fmt.Sprintf("%017d", len(tc.name)), "claude-code", "", "cc-"+tc.name,
				core.SessionExpired, testHost, path))
			pending, err := f.svc.PendingFor(ctx, caller)
			require.NoError(t, err)
			require.Len(t, pending, 1)
			_, ok := f.svc.Ask(ctx, pending[0])
			require.True(t, ok)
			f.svc.Reported(ctx, pending[0].Sessions, tc.state)
			require.Equal(t, tc.state, f.reload(t, sess.ID).Transcript)
			pending, err = f.svc.PendingFor(ctx, caller)
			require.NoError(t, err)
			require.Empty(t, pending)
		})
	}

	sess := f.flag(t, f.sessionAs(t, "01MREPORTCOPYAAAAAAAAAAAAA", "claude-code", "", "cc-copy", core.SessionExpired, testHost, path))
	pending, err := f.svc.PendingFor(ctx, caller)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	_, ok := f.svc.Ask(ctx, pending[0])
	require.True(t, ok)
	require.Equal(t, core.TranscriptCopied, f.svc.Receive(ctx, pending[0].Sessions, bytes.NewReader(gz(t, line1)), int64(len(line1)), ViaClient))
	f.svc.Reported(ctx, pending[0].Sessions, core.TranscriptRefused)
	require.Equal(t, core.TranscriptCopied, f.reload(t, sess.ID).Transcript, "a report after the copy changes nothing")

	none := f.flag(t, f.sessionAs(t, "01MREPORTNONEAAAAAAAAAAAAA", "claude-code", "", "cc-none", core.SessionExpired, testHost, ""))
	pending, err = f.svc.PendingFor(ctx, caller)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	_, ok = f.svc.Ask(ctx, pending[0])
	require.False(t, ok)
	require.Equal(t, core.TranscriptNoPath, f.reload(t, none.ID).Transcript)
}

// The reaper's pass settles the flagged sessions that ended without a copy:
// copied where the daemon reads the file, pending under peer_auth, the
// computer named where it is another's; a session still active, or ended
// after the cutoff, waits for its end signal.
func TestKeepEnded(t *testing.T) {
	ctx := context.Background()
	cutoff := time.Now().UTC().Add(time.Minute)
	for _, tc := range []struct {
		name    string
		peer    bool
		status  core.SessionStatus
		host    string
		path    bool
		before  core.TranscriptCopy
		want    core.TranscriptCopy
		kept    bool
		atLater bool // updated after the cutoff
	}{
		{"reaped, readable: copied", false, core.SessionExpired, testHost, true, "", core.TranscriptCopied, true, false},
		{"completed without its end signal: copied", false, core.SessionCompleted, "", true, "", core.TranscriptCopied, true, false},
		{"pending from peer_auth days: copied", false, core.SessionExpired, testHost, true, core.TranscriptPending, core.TranscriptCopied, true, false},
		{"a failed daemon copy is not retried", false, core.SessionExpired, testHost, true, core.TranscriptFailed, core.TranscriptFailed, false, false},
		{"another computer", false, core.SessionExpired, "beta", true, "", core.TranscriptRemote, false, false},
		{"no transcript", false, core.SessionExpired, testHost, false, "", core.TranscriptNoPath, false, false},
		{"still active", false, core.SessionActive, testHost, true, "", "", false, false},
		{"ended after the cutoff", false, core.SessionCompleted, testHost, true, "", "", false, true},
		{"peer_auth: pending", true, core.SessionExpired, testHost, true, "", core.TranscriptPending, false, false},
		{"peer_auth: a failed upload stays asked for", true, core.SessionExpired, testHost, true, core.TranscriptFailed, core.TranscriptFailed, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixtureWith(t, Config{PeerAuth: tc.peer})
			path := ""
			if tc.path {
				path = f.write(t, "k.jsonl", line1)
			}
			sess := f.flag(t, f.sessionAs(t, "01MKEEPENDEDAAAAAAAAAAAAAA", "claude-code", "", "cc-k", tc.status, tc.host, path))
			if tc.before != "" {
				_, err := store.SetTranscriptCopy(ctx, f.db, sess.ID, tc.before, 0, time.Now())
				require.NoError(t, err)
			}
			at := cutoff
			if tc.atLater {
				at = cutoff.Add(-2 * time.Minute)
			}
			f.svc.KeepEnded(ctx, at)
			f.svc.Wait()
			require.Equal(t, tc.want, f.reload(t, sess.ID).Transcript)
			require.Equal(t, tc.kept, f.svc.Kept(sess.ID))
		})
	}
}
