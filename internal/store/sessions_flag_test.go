package store

// Migration 051 and the session flag's store functions.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// A database from before 051 migrates with every session unflagged and no
// transcript path, the previous release's insert (which names its columns)
// keeps working, and the verdict column takes only a flag.
func TestMigration051_SessionFlags(t *testing.T) {
	ctx := context.Background()
	db := openPartialDB(t, 51)
	v, err := SchemaVersion(db)
	require.NoError(t, err)
	require.Equal(t, 50, v, "the fixture is the previous release's schema")
	insert := func(id, name string) {
		t.Helper()
		now := core.FormatTime(time.Now())
		_, err := db.ExecContext(ctx, `INSERT INTO sessions (id, name, project_slug, status, findings, claude_session_id,
			external_client, cwd, host, source, model, ambient, metadata, created_at, updated_at, account)
			VALUES (?, ?, '', 'completed', 'old findings', 'ext-'||?, 'claude-code', '/w', '', 'startup', '', 1, '{}', ?, ?, '')`,
			id, name, id, now, now)
		require.NoError(t, err)
	}
	insert("01MOLDSESSAAAAAAAAAAAAAAAA", "cc/old")

	require.NoError(t, migrate(db, migrationList()))
	v, err = SchemaVersion(db)
	require.NoError(t, err)
	require.Equal(t, LatestSchemaVersion(), v)
	old, ok, err := SessionByID(ctx, db, "01MOLDSESSAAAAAAAAAAAAAAAA")
	require.NoError(t, err)
	require.True(t, ok)
	require.False(t, old.Flagged())
	require.Empty(t, old.TranscriptPath)
	require.Empty(t, old.Transcript)
	require.Nil(t, old.FlaggedAt)
	require.Equal(t, "old findings", old.Findings)

	// The previous release, rolled back to, still inserts by name.
	insert("01MROLLEDBACKAAAAAAAAAAAAA", "cc/rolled")
	back, ok, err := SessionByID(ctx, db, "01MROLLEDBACKAAAAAAAAAAAAA")
	require.NoError(t, err)
	require.True(t, ok)
	require.False(t, back.Flagged())

	for _, bad := range []string{"none", "great", ""} {
		_, err := db.ExecContext(ctx, `UPDATE sessions SET verdict = ? WHERE id = '01MOLDSESSAAAAAAAAAAAAAAAA'`, bad)
		require.Error(t, err, "verdict %q", bad)
	}
}

// A flag round-trips through every read, never bumps updated_at, replaces an
// earlier flag, and clears whole -- the copy's record with it.
func TestSessionFlag_RoundTrip(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	s := newSession("01MFLAGAAAAAAAAAAAAAAAAAAA", "cc/flag")
	s.Ambient, s.ExternalClient, s.ExternalSessionID = true, "claude-code", "flag-ext"
	s.UpdatedAt = time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	require.NoError(t, CreateSession(ctx, db, s))

	require.NoError(t, SetSessionTranscriptPath(ctx, db, s.ID, "/home/x/.claude/projects/p/flag-ext.jsonl"))
	require.NoError(t, SetSessionTranscriptPath(ctx, db, s.ID, ""), "an empty path keeps the recorded one")
	at := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	got, err := SetSessionFlag(ctx, db, s.ID, core.VerdictGood, "shipped it clean", at)
	require.NoError(t, err)
	require.Equal(t, core.VerdictGood, got.Verdict)
	require.Equal(t, "shipped it clean", got.VerdictReason)
	require.Equal(t, at, *got.FlaggedAt)
	require.Equal(t, "/home/x/.claude/projects/p/flag-ext.jsonl", got.TranscriptPath)
	require.True(t, got.UpdatedAt.Equal(s.UpdatedAt), "a flag never bumps updated_at")

	got, err = SetSessionFlag(ctx, db, s.ID, core.VerdictBad, "", at.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, core.VerdictBad, got.Verdict, "the last flag wins")
	require.Empty(t, got.VerdictReason, "and replaces the reason too")

	recorded, err := SetTranscriptCopy(ctx, db, s.ID, core.TranscriptCopied, 1234, at)
	require.NoError(t, err)
	require.True(t, recorded)
	flagged, err := FlaggedSessionsByExternalIdentity(ctx, db, "claude-code", "flag-ext")
	require.NoError(t, err)
	require.Len(t, flagged, 1)
	require.Equal(t, core.TranscriptCopied, flagged[0].Transcript)
	require.Equal(t, int64(1234), flagged[0].TranscriptBytes)

	// A not-made word leaves the copy's size and time alone.
	_, err = SetTranscriptCopy(ctx, db, s.ID, core.TranscriptTooLarge, 0, at)
	require.NoError(t, err)
	got, _, err = SessionByID(ctx, db, s.ID)
	require.NoError(t, err)
	require.Equal(t, core.TranscriptTooLarge, got.Transcript)
	require.Equal(t, int64(1234), got.TranscriptBytes)

	list, err := ListSessionsWhere(ctx, db, SessionFilter{Verdict: core.VerdictBad})
	require.NoError(t, err)
	require.Len(t, list, 1)
	list, err = ListSessionsWhere(ctx, db, SessionFilter{Verdict: core.VerdictGood})
	require.NoError(t, err)
	require.Empty(t, list)
	n, err := CountFlaggedSessions(ctx, db)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	prior, cleared, err := ClearSessionFlag(ctx, db, s.ID)
	require.NoError(t, err)
	require.True(t, cleared)
	require.Equal(t, core.VerdictBad, prior.Verdict, "prior is the flag cleared")
	got, _, err = SessionByID(ctx, db, s.ID)
	require.NoError(t, err)
	require.False(t, got.Flagged())
	require.Empty(t, got.Transcript)
	require.Zero(t, got.TranscriptBytes)
	require.Nil(t, got.TranscriptCopiedAt)
	require.Equal(t, "/home/x/.claude/projects/p/flag-ext.jsonl", got.TranscriptPath, "the path is the session's, not the flag's")

	_, cleared, err = ClearSessionFlag(ctx, db, s.ID)
	require.NoError(t, err)
	require.False(t, cleared, "nothing to clear")
	recorded, err = SetTranscriptCopy(ctx, db, s.ID, core.TranscriptCopied, 1, at)
	require.NoError(t, err)
	require.False(t, recorded, "an unflagged session records no copy")
}

// The refusals: a verdict that does not flag, a session that does not exist,
// a copy state outside the set.
func TestSessionFlag_Refusals(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	require.NoError(t, CreateSession(ctx, db, newSession("01MREFUSEAAAAAAAAAAAAAAAAA", "sess/refuse")))
	for _, v := range []core.SessionVerdict{core.VerdictNone, "", "great"} {
		_, err := SetSessionFlag(ctx, db, "01MREFUSEAAAAAAAAAAAAAAAAA", v, "", time.Now())
		require.Error(t, err, "verdict %q", v)
	}
	_, err := SetSessionFlag(ctx, db, "01MNOSUCHAAAAAAAAAAAAAAAAA", core.VerdictGood, "", time.Now())
	require.True(t, errors.Is(err, ErrSessionNotFound), "%v", err)
	_, _, err = ClearSessionFlag(ctx, db, "01MNOSUCHAAAAAAAAAAAAAAAAA")
	require.True(t, errors.Is(err, ErrSessionNotFound), "%v", err)
	_, err = SetTranscriptCopy(ctx, db, "01MREFUSEAAAAAAAAAAAAAAAAA", "lost", 0, time.Now())
	require.Error(t, err)
}

// A flagged explicit session linked to an agent client's session id copies the
// transcript its ambient row recorded; FlaggedSessionsByExternalIdentity finds
// both rows whatever their status, the ambient row first.
func TestSessionFlag_LinkedExplicitSession(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	amb := newSession("01MAMBAAAAAAAAAAAAAAAAAAAA", "cc/amb")
	amb.Ambient, amb.ExternalClient, amb.ExternalSessionID = true, "claude-code", "linked-ext"
	amb.Status = core.SessionCompleted
	require.NoError(t, CreateSession(ctx, db, amb))
	require.NoError(t, SetSessionTranscriptPath(ctx, db, amb.ID, "/t/linked-ext.jsonl"))
	exp := newSession("01MEXPAAAAAAAAAAAAAAAAAAAA", "sess/explicit")
	exp.ExternalClient, exp.ExternalSessionID = "claude-code", "linked-ext"
	require.NoError(t, CreateSession(ctx, db, exp))

	path, err := TranscriptPathOf(ctx, db, exp)
	require.NoError(t, err)
	require.Equal(t, "/t/linked-ext.jsonl", path)
	path, err = TranscriptPathOf(ctx, db, newSession("01MLONEAAAAAAAAAAAAAAAAAAA", "sess/lone"))
	require.NoError(t, err)
	require.Empty(t, path, "a session without hooks has none")

	for _, id := range []string{exp.ID, amb.ID} {
		_, err := SetSessionFlag(ctx, db, id, core.VerdictNotable, "", time.Now())
		require.NoError(t, err)
	}
	flagged, err := FlaggedSessionsByExternalIdentity(ctx, db, "claude-code", "linked-ext")
	require.NoError(t, err)
	require.Len(t, flagged, 2)
	require.Equal(t, amb.ID, flagged[0].ID, "ambient first, though it is completed")
}

// Migration 052: a database from before it migrates with no asks counted,
// the previous release's insert (which names its columns) keeps working, and
// a database whose migration record was cut back below 052 migrates forward
// again.
func TestMigration052_TranscriptAsks(t *testing.T) {
	ctx := context.Background()
	db := openPartialDB(t, 52)
	v, err := SchemaVersion(db)
	require.NoError(t, err)
	require.Equal(t, 51, v, "the fixture is the previous release's schema")
	_, err = db.ExecContext(ctx, `INSERT INTO sessions (id, name, project_slug, status, findings, claude_session_id,
		external_client, cwd, host, source, model, ambient, metadata, created_at, updated_at, account, verdict, transcript_copy)
		VALUES ('01MOLDASKSAAAAAAAAAAAAAAAA', 'cc/old-asks', '', 'completed', '', 'ext-old', 'claude-code', '/w', '', 'startup', '',
		        1, '{}', ?, ?, '', 'good', 'not_pushed')`, core.FormatTime(time.Now()), core.FormatTime(time.Now()))
	require.NoError(t, err)
	require.NoError(t, migrate(db, migrationList()))
	asks, at, err := TranscriptAsksOf(ctx, db, "01MOLDASKSAAAAAAAAAAAAAAAA")
	require.NoError(t, err)
	require.Zero(t, asks)
	require.Nil(t, at)
	pending, err := PendingTranscriptSessions(ctx, db, "claude-code", "")
	require.NoError(t, err)
	require.Len(t, pending, 1, "a not-pushed copy from before is asked for")
	require.Equal(t, core.TranscriptNotPushed, pending[0].Transcript)

	_, err = db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version >= 52`)
	require.NoError(t, err)
	require.NoError(t, migrate(db, migrationList()), "a cut-back record migrates forward again")
}

// The asks of a pending transcript: counted while the copy is still asked
// for, ended by a word that stands, started over by a new flag, cleared with
// the flag; a copy kept is never written over by an ask.
func TestTranscriptAsks_Lifecycle(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	s := newSession("01MASKSAAAAAAAAAAAAAAAAAAA", "cc/asks")
	s.Ambient, s.ExternalClient, s.ExternalSessionID = true, "claude-code", "asks-ext"
	require.NoError(t, CreateSession(ctx, db, s))
	at := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	asksOf := func() (int, *time.Time) {
		t.Helper()
		n, when, err := TranscriptAsksOf(ctx, db, s.ID)
		require.NoError(t, err)
		return n, when
	}

	_, recorded, err := RecordTranscriptAsk(ctx, db, s.ID, at)
	require.NoError(t, err)
	require.False(t, recorded, "an unflagged session is asked nothing")
	_, err = SetSessionFlag(ctx, db, s.ID, core.VerdictNotable, "", at)
	require.NoError(t, err)

	for n := 1; n <= 2; n++ {
		asks, recorded, err := RecordTranscriptAsk(ctx, db, s.ID, at.Add(time.Duration(n)*time.Minute))
		require.NoError(t, err)
		require.True(t, recorded)
		require.Equal(t, n, asks)
	}
	got, _, err := SessionByID(ctx, db, s.ID)
	require.NoError(t, err)
	require.Equal(t, core.TranscriptPending, got.Transcript)
	asks, when := asksOf()
	require.Equal(t, 2, asks)
	require.Equal(t, at.Add(2*time.Minute), *when)
	pending, err := PendingTranscriptSessions(ctx, db, "claude-code", "")
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, 2, pending[0].TranscriptAsks, "the pending read carries the asks")
	require.Equal(t, at.Add(2*time.Minute), *pending[0].TranscriptAskedAt)

	recorded, err = EndTranscriptAsks(ctx, db, s.ID, core.TranscriptNotSent)
	require.NoError(t, err)
	require.True(t, recorded)
	recorded, err = EndTranscriptAsks(ctx, db, s.ID, core.TranscriptRefused)
	require.NoError(t, err)
	require.False(t, recorded, "a word that stands is not written over")
	_, recorded, err = RecordTranscriptAsk(ctx, db, s.ID, at)
	require.NoError(t, err)
	require.False(t, recorded, "given up: asked no more")
	recorded, err = SetTranscriptPending(ctx, db, s.ID)
	require.NoError(t, err)
	require.False(t, recorded)
	_, err = EndTranscriptAsks(ctx, db, s.ID, core.TranscriptCopied)
	require.Error(t, err, "a copy is recorded by SetTranscriptCopy")

	// A new flag asks afresh: the word that stood and the asks start over.
	got, err = SetSessionFlag(ctx, db, s.ID, core.VerdictBad, "", at)
	require.NoError(t, err)
	require.Empty(t, got.Transcript)
	asks, when = asksOf()
	require.Zero(t, asks)
	require.Nil(t, when)
	recorded, err = SetTranscriptPending(ctx, db, s.ID)
	require.NoError(t, err)
	require.True(t, recorded)

	// A copy kept stands through an ask and a new flag.
	_, err = SetTranscriptCopy(ctx, db, s.ID, core.TranscriptCopied, 10, at)
	require.NoError(t, err)
	_, recorded, err = RecordTranscriptAsk(ctx, db, s.ID, at)
	require.NoError(t, err)
	require.False(t, recorded)
	got, err = SetSessionFlag(ctx, db, s.ID, core.VerdictGood, "", at)
	require.NoError(t, err)
	require.Equal(t, core.TranscriptCopied, got.Transcript)

	// A failed upload is asked for again; clearing the flag forgets the asks.
	_, err = SetTranscriptCopy(ctx, db, s.ID, core.TranscriptFailed, 0, at)
	require.NoError(t, err)
	_, recorded, err = RecordTranscriptAsk(ctx, db, s.ID, at)
	require.NoError(t, err)
	require.True(t, recorded)
	_, cleared, err := ClearSessionFlag(ctx, db, s.ID)
	require.NoError(t, err)
	require.True(t, cleared)
	asks, when = asksOf()
	require.Zero(t, asks)
	require.Nil(t, when)
}

// The pending sessions of a caller are its agent client's and account's,
// flagged, still asked for; the ended ones the reaper settles are those last
// updated before the cutoff.
func TestPendingAndEndedFlaggedSessions(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	at := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	mk := func(id, client, account string, status core.SessionStatus, updated time.Time, flag bool, state core.TranscriptCopy) {
		t.Helper()
		s := newSession(id, "x/"+id)
		s.Ambient, s.ExternalClient, s.ExternalSessionID, s.Account, s.Status = true, client, "ext-"+id, account, status
		s.CreatedAt, s.UpdatedAt = updated, updated
		require.NoError(t, CreateSession(ctx, db, s))
		if flag {
			_, err := SetSessionFlag(ctx, db, id, core.VerdictGood, "", at)
			require.NoError(t, err)
		}
		if state != "" {
			_, err := SetTranscriptCopy(ctx, db, id, state, 0, at)
			require.NoError(t, err)
		}
	}
	old := at.Add(-time.Hour)
	mk("01MPAAAAAAAAAAAAAAAAAAAAAA", "claude-code", "", core.SessionExpired, old, true, "")
	mk("01MPBBBBBBBBBBBBBBBBBBBBBB", "claude-code", "", core.SessionActive, at, true, core.TranscriptFailed)
	mk("01MPCCCCCCCCCCCCCCCCCCCCCC", "claude-code", "", core.SessionCompleted, old, true, core.TranscriptCopied)
	mk("01MPDDDDDDDDDDDDDDDDDDDDDD", "claude-code", "", core.SessionCompleted, old, false, "")
	mk("01MPEEEEEEEEEEEEEEEEEEEEEE", "codex", "", core.SessionExpired, old, true, "")
	mk("01MPFFFFFFFFFFFFFFFFFFFFFF", "claude-code", "_example_agent", core.SessionExpired, old, true, core.TranscriptNotPushed)
	mk("01MPGGGGGGGGGGGGGGGGGGGGGG", "claude-code", "", core.SessionCompleted, at, true, core.TranscriptTooLarge)

	ids := func(sessions []core.Session, err error) []string {
		t.Helper()
		require.NoError(t, err)
		var out []string
		for _, s := range sessions {
			out = append(out, s.ID)
		}
		return out
	}
	require.ElementsMatch(t, []string{"01MPAAAAAAAAAAAAAAAAAAAAAA", "01MPBBBBBBBBBBBBBBBBBBBBBB"},
		ids(PendingTranscriptSessions(ctx, db, "claude-code", "")))
	require.Equal(t, []string{"01MPFFFFFFFFFFFFFFFFFFFFFF"}, ids(PendingTranscriptSessions(ctx, db, "claude-code", "_example_agent")))
	require.Equal(t, []string{"01MPEEEEEEEEEEEEEEEEEEEEEE"}, ids(PendingTranscriptSessions(ctx, db, "codex", "")))
	require.Empty(t, ids(PendingTranscriptSessions(ctx, db, "", "")))
	require.ElementsMatch(t, []string{"01MPAAAAAAAAAAAAAAAAAAAAAA", "01MPEEEEEEEEEEEEEEEEEEEEEE", "01MPFFFFFFFFFFFFFFFFFFFFFF"},
		ids(EndedFlaggedSessions(ctx, db, at.Add(-time.Minute))))
}
