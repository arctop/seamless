package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

func TestProjectContentCounts(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 11, 3, 0, 0, 0, time.UTC)
	ts := core.FormatTime(now)

	_, err := EnsureProject(ctx, db, "alpha", "Alpha project")
	require.NoError(t, err)
	_, err = EnsureProject(ctx, db, "beta", "beta")
	require.NoError(t, err)

	insertMemory(t, db, "01MA", "gotcha", "a-live", "", "alpha", "body", ts, "")
	insertMemory(t, db, "01MB", "gotcha", "a-old", "", "alpha", "body", ts, ts)
	insertMemory(t, db, "01MG", "convention", "g-live", "", "", "body", ts, "")
	insertNote(t, db, "01NA", "design", "design", "alpha", ts)
	require.NoError(t, CreateTask(ctx, db, core.Task{ID: "01TA", ProjectSlug: "alpha", Title: "t", Status: core.TaskOpen, CreatedAt: now, UpdatedAt: now}))
	require.NoError(t, CreateSession(ctx, db, core.Session{ID: "01SA", Name: "cc/a", ProjectSlug: "alpha", Status: core.SessionActive, CreatedAt: now, UpdatedAt: now}))
	require.NoError(t, CreateTrial(ctx, db, core.Trial{ID: "01RA", Lab: "lab", ProjectSlug: "alpha", Outcome: core.OutcomePass, CreatedAt: now}))
	// Content under a slug with no projects row still counts.
	insertMemory(t, db, "01MO", "gotcha", "orphan", "", "orphan", "body", ts, "")

	got, err := ProjectContentCounts(ctx, db)
	require.NoError(t, err)
	require.Equal(t, []ProjectContent{
		{Slug: "", Memories: 1},
		{Slug: "alpha", Name: "Alpha project", Memories: 1, Retired: 1, Notes: 1, Tasks: 1, Sessions: 1, Trials: 1},
		{Slug: "beta"},
		{Slug: "orphan", Memories: 1},
	}, got)
	require.Equal(t, 6, got[1].Total())
}

func TestProjectContentCounts_EmptyGlobalIsLeftOut(t *testing.T) {
	db := openTestDB(t)
	got, err := ProjectContentCounts(context.Background(), db)
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestContentFingerprint_MovesOnlyWithWhatABackupCarries(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 11, 3, 0, 0, 0, time.UTC)
	ts := core.FormatTime(now)
	_, err := EnsureProject(ctx, db, "alpha", "alpha")
	require.NoError(t, err)
	insertMemory(t, db, "01MA", "gotcha", "a-live", "", "alpha", "body", ts, "")

	fp := func() string {
		t.Helper()
		got, err := ContentFingerprint(ctx, db)
		require.NoError(t, err)
		require.Len(t, got, 64)
		return got
	}
	base := fp()
	require.Equal(t, base, fp(), "a fingerprint is stable while nothing changes")

	// Telemetry and caches are not what a backup is for.
	_, err = db.ExecContext(ctx, `INSERT INTO events (id, ts, kind, session_id, project_slug, item_id, payload)
		VALUES ('01EV', ?, 'tool.call', '', 'alpha', '', '{}')`, ts)
	require.NoError(t, err)
	require.NoError(t, SetSetting(ctx, db, SettingGamificationRecords, `{"x":1}`))
	require.NoError(t, SetSettingTime(ctx, db, SettingCorpusReviewed, now))
	require.Equal(t, base, fp(), "events and recomputed caches leave it alone")

	// A fence change bumps no updated_at, and must still count.
	require.NoError(t, SetProjectIsolation(ctx, db, "alpha", core.IsolationConfidential))
	fenced := fp()
	require.NotEqual(t, base, fenced, "an isolation change is a change")

	_, err = db.ExecContext(ctx, `UPDATE memories_index SET content_hash = 'h2' WHERE id = '01MA'`)
	require.NoError(t, err)
	edited := fp()
	require.NotEqual(t, fenced, edited, "an edited memory is a change")

	require.NoError(t, SetSetting(ctx, db, SettingProjectFamilies, `{"fam":["alpha"]}`))
	require.NotEqual(t, edited, fp(), "a settings row a restore brings back is a change")
}

// A harvest that lands on a session that has ended moves no updated_at, and a
// Codex turn grows a ledger row already there: each still moves the
// fingerprint, since a backup carries the ledger. The same rows again do not.
func TestContentFingerprint_MovesWithTheLedger(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 20, 3, 0, 0, 0, time.UTC)
	require.NoError(t, CreateSession(ctx, db, core.Session{
		ID: "01FPLEDGER0000000000000000", Name: "cx/ended", ProjectSlug: "alpha", Status: core.SessionCompleted, Ambient: true,
		ExternalClient: "codex", ExternalSessionID: "fp-ledger", CreatedAt: at, UpdatedAt: at,
	}))
	fp := func() string {
		t.Helper()
		got, err := ContentFingerprint(ctx, db)
		require.NoError(t, err)
		return got
	}
	credit := func(in int, when time.Time) {
		t.Helper()
		u := core.TokenUsage{Input: in}
		u.Normalize()
		ok, err := CreditConversationUsage(ctx, db, "codex", "fp-ledger", UsageCredit{Cumulative: u, At: when, Turn: true})
		require.NoError(t, err)
		require.True(t, ok)
	}
	base := fp()
	credit(10, at)
	first := fp()
	require.NotEqual(t, base, first, "a harvest on an ended session is a change")
	credit(25, at.Add(time.Minute)) // the same quarter-hour's row grows
	grown := fp()
	require.NotEqual(t, first, grown, "a row that grew is a change")

	rows, err := SessionUsage(ctx, db, "01FPLEDGER0000000000000000")
	require.NoError(t, err)
	_, err = ReplaceConversationUsage(ctx, db, "codex", "fp-ledger", ledgerBuckets(rows), at.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, grown, fp(), "the same rows again are no change")

	var updated string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT updated_at FROM sessions WHERE id = ?`, "01FPLEDGER0000000000000000").Scan(&updated))
	require.Equal(t, core.FormatTime(at), updated, "none of it touched updated_at")
}

func TestContentFingerprint_ToleratesAnOlderSchema(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	_, err := db.ExecContext(ctx, `DROP TABLE console_locks`)
	require.NoError(t, err)
	// A database from before the session flag (migration 051).
	_, err = db.ExecContext(ctx, `DROP INDEX idx_sessions_flagged`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `ALTER TABLE sessions DROP COLUMN verdict`)
	require.NoError(t, err)
	got, err := ContentFingerprint(ctx, db)
	require.NoError(t, err)
	require.Len(t, got, 64)
}

// A flag, a transcript copy made or made again, and a flag cleared each move
// the fingerprint, though none touches updated_at: a backup carries the copies
// the flags keep, so the next scheduled one must not be skipped as unchanged.
func TestContentFingerprint_MovesWithTheSessionFlag(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 20, 3, 0, 0, 0, time.UTC)
	id, err := core.NewID()
	require.NoError(t, err)
	require.NoError(t, CreateSession(ctx, db, core.Session{
		ID: id, Name: "cc/flagged", ProjectSlug: "alpha", Status: core.SessionCompleted, CreatedAt: at, UpdatedAt: at,
	}))
	fp := func() string {
		t.Helper()
		got, err := ContentFingerprint(ctx, db)
		require.NoError(t, err)
		return got
	}
	seen := map[string]string{"unflagged": fp()}
	step := func(name string, fn func()) {
		t.Helper()
		fn()
		got := fp()
		for prev, v := range seen {
			require.NotEqual(t, v, got, "%s leaves the fingerprint as it was at %s", name, prev)
		}
		seen[name] = got
	}
	step("flagged", func() {
		_, err := SetSessionFlag(ctx, db, id, core.VerdictBad, "it went badly", at)
		require.NoError(t, err)
	})
	step("copied", func() {
		_, err := SetTranscriptCopy(ctx, db, id, core.TranscriptCopied, 100, at)
		require.NoError(t, err)
	})
	step("copied again", func() {
		_, err := SetTranscriptCopy(ctx, db, id, core.TranscriptCopied, 200, at.Add(time.Minute))
		require.NoError(t, err)
	})
	_, _, err = ClearSessionFlag(ctx, db, id)
	require.NoError(t, err)
	require.Equal(t, seen["unflagged"], fp(), "a cleared flag is the unflagged session again")

	var updated string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT updated_at FROM sessions WHERE id = ?`, id).Scan(&updated))
	require.Equal(t, core.FormatTime(at), updated, "none of it touched updated_at")
}

func TestInstallID_MintedOnceAndKept(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	first, err := InstallID(ctx, db)
	require.NoError(t, err)
	require.Len(t, first, 26, "a ULID")
	again, err := InstallID(ctx, db)
	require.NoError(t, err)
	require.Equal(t, first, again)

	// A blank row (a hand edit) is replaced rather than handed out.
	require.NoError(t, SetSetting(ctx, db, SettingInstallID, "  "))
	fresh, err := InstallID(ctx, db)
	require.NoError(t, err)
	require.Len(t, fresh, 26)
	require.NotEqual(t, first, fresh)
}
