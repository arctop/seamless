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
