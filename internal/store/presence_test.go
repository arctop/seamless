package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// prSession creates a session with the given status, last update and metadata,
// and returns its id.
func prSession(t *testing.T, db *sql.DB, name string, status core.SessionStatus, updated time.Time, meta map[string]any) string {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	require.NoError(t, CreateSession(context.Background(), db, core.Session{
		ID: id, Name: name, ProjectSlug: "example-ops", Status: status, Ambient: true,
		Metadata: meta, CreatedAt: updated, UpdatedAt: updated,
	}))
	return id
}

// prRead reads a session back, failing the test when it is gone.
func prRead(t *testing.T, db *sql.DB, id string) core.Session {
	t.Helper()
	sess, ok, err := SessionByID(context.Background(), db, id)
	require.NoError(t, err)
	require.True(t, ok)
	return sess
}

// The record is two metadata keys, written alone: every other key, the
// findings and the session's last update (its liveness) stay as they were.
func TestRecordSessionRepoLocks_WritesOnlyItsKeys(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	id := prSession(t, db, "cc/reader", core.SessionActive, at, map[string]any{
		"cwd": "/work/app", "plan_capture": map[string]any{"basename": "plan.md"},
	})

	aliases := []string{"github.com/example/app", "example-laptop:/work/app"}
	require.NoError(t, RecordSessionRepoLocks(ctx, db, id, aliases, map[string]string{"L1": "", "L2": "S9"}))
	sess := prRead(t, db, id)
	gotAliases, seen, ok := SessionRepoLocks(sess)
	require.True(t, ok)
	require.Equal(t, aliases, gotAliases)
	require.Equal(t, map[string]string{"L1": "", "L2": "S9"}, seen)
	require.Equal(t, "/work/app", sess.Metadata["cwd"], "another key survives")
	require.Equal(t, map[string]any{"basename": "plan.md"}, sess.Metadata["plan_capture"])
	require.Equal(t, at, sess.UpdatedAt, "recording is no activity")

	// The seen holders move alone.
	require.NoError(t, SetSessionRepoLockSeen(ctx, db, id, map[string]string{"L1": "S7"}))
	gotAliases, seen, ok = SessionRepoLocks(prRead(t, db, id))
	require.True(t, ok)
	require.Equal(t, aliases, gotAliases)
	require.Equal(t, map[string]string{"L1": "S7"}, seen)

	// No aliases -- a checkout no lock can name -- removes both keys.
	require.NoError(t, RecordSessionRepoLocks(ctx, db, id, nil, nil))
	sess = prRead(t, db, id)
	_, _, ok = SessionRepoLocks(sess)
	require.False(t, ok)
	require.NotContains(t, sess.Metadata, SessionMetaRepoLockAliases)
	require.NotContains(t, sess.Metadata, SessionMetaRepoLockSeen)
	require.Equal(t, "/work/app", sess.Metadata["cwd"])

	require.ErrorIs(t, RecordSessionRepoLocks(ctx, db, "01NOSUCHSESSION", aliases, nil), ErrSessionNotFound)
	require.ErrorIs(t, SetSessionRepoLockSeen(ctx, db, "01NOSUCHSESSION", nil), ErrSessionNotFound)
}

// A row whose metadata is not JSON (a hand edit) takes the record as if it
// were empty, rather than failing every write after it.
func TestRecordSessionRepoLocks_ToleratesBrokenMetadata(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	id := prSession(t, db, "cc/broken", core.SessionActive, time.Now().UTC(), nil)
	_, err := db.ExecContext(ctx, `UPDATE sessions SET metadata = 'not json' WHERE id = ?`, id)
	require.NoError(t, err)
	require.NoError(t, RecordSessionRepoLocks(ctx, db, id, []string{"h:/work/app"}, nil))
	aliases, seen, ok := SessionRepoLocks(prRead(t, db, id))
	require.True(t, ok)
	require.Equal(t, []string{"h:/work/app"}, aliases)
	require.Empty(t, seen)
}

// A session that recorded nothing, or something of the wrong shape, has no
// lock state.
func TestSessionRepoLocks_AbsentOrMalformedIsNone(t *testing.T) {
	for _, tc := range []struct {
		name string
		meta map[string]any
	}{
		{"nothing recorded", nil},
		{"not a list", map[string]any{SessionMetaRepoLockAliases: "h:/work/app"}},
		{"an empty list", map[string]any{SessionMetaRepoLockAliases: []any{}}},
		{"no strings", map[string]any{SessionMetaRepoLockAliases: []any{1, true}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, ok := SessionRepoLocks(core.Session{Metadata: tc.meta})
			require.False(t, ok)
		})
	}
	aliases, seen, ok := SessionRepoLocks(core.Session{Metadata: map[string]any{
		SessionMetaRepoLockAliases: []any{"a", "a", 3, "b"},
		SessionMetaRepoLockSeen:    map[string]any{"L1": "S1", "L2": 7},
	}})
	require.True(t, ok)
	require.Equal(t, []string{"a", "b"}, aliases)
	require.Equal(t, map[string]string{"L1": "S1"}, seen, "a holder that is not a string reads as unseen")
}

// Presence is the live sessions whose recorded aliases meet the lock's, by
// LiveAsOf's rule: updated within the idle threshold, or holding something
// live. Ended sessions, quiet ones holding nothing, other repositories'
// sessions and the excluded reader are not among them.
func TestLiveRepoPeers(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	const idle = 45 * time.Minute
	in := func(aliases ...string) map[string]any {
		raw := make([]any, len(aliases))
		for i, a := range aliases {
			raw[i] = a
		}
		return map[string]any{SessionMetaRepoLockAliases: raw}
	}
	origin := "github.com/example/app"

	recent := prSession(t, db, "cc/recent", core.SessionActive, now.Add(-2*time.Minute), in(origin, "laptop:/work/app"))
	older := prSession(t, db, "cc/older", core.SessionActive, now.Add(-10*time.Minute), in("desk:/srv/app-side", "desk:/srv/app"))
	reader := prSession(t, db, "cc/reader", core.SessionActive, now.Add(-time.Minute), in(origin))
	prSession(t, db, "cc/elsewhere", core.SessionActive, now, in("github.com/example/other"))
	prSession(t, db, "cc/ended", core.SessionCompleted, now, in(origin))
	prSession(t, db, "cc/quiet", core.SessionActive, now.Add(-2*time.Hour), in(origin))
	prSession(t, db, "cc/unrecorded", core.SessionActive, now, map[string]any{"cwd": "/work/app"})
	holding := prSession(t, db, "cc/holding", core.SessionActive, now.Add(-3*time.Hour), in(origin))
	l := lkTestDeclare(t, db, CWDIdentity{Host: "h", RepoRoot: "/work/other-repo"}, "main").Lock
	_, err := AcquireRepoLock(ctx, db, l.ID, holding, time.Hour, now.Add(-10*time.Minute))
	require.NoError(t, err)

	peers, err := LiveRepoPeers(ctx, db, []string{origin, "desk:/srv/app"}, []string{reader, ""}, now, idle)
	require.NoError(t, err)
	var ids []string
	for _, p := range peers {
		ids = append(ids, p.ID)
		require.True(t, p.LiveAsOf(now, idle), "%s is live by the one rule", p.Name)
	}
	require.Equal(t, []string{recent, older, holding}, ids, "most recently active first; a live hold keeps a quiet session")

	// Without the exclusion the reader is among them.
	peers, err = LiveRepoPeers(ctx, db, []string{origin}, nil, now, idle)
	require.NoError(t, err)
	require.Len(t, peers, 3)

	none, err := LiveRepoPeers(ctx, db, nil, nil, now, idle)
	require.NoError(t, err)
	require.Empty(t, none, "no alias matches nobody")

	// The idle threshold is the configured one: at 5 minutes only the session
	// seen 2 minutes ago and the holder remain.
	peers, err = LiveRepoPeers(ctx, db, []string{origin, "desk:/srv/app"}, []string{reader}, now, 5*time.Minute)
	require.NoError(t, err)
	require.Len(t, peers, 2)
	require.Equal(t, recent, peers[0].ID)
	require.Equal(t, holding, peers[1].ID)
}

func TestRepoLockAliasStringsRoundTrip(t *testing.T) {
	aliases := []core.RepoLockAlias{
		{Kind: core.RepoLockAliasOrigin, Alias: "github.com/example/app"},
		{Kind: core.RepoLockAliasPath, Alias: "h:/work/app"},
		{Kind: core.RepoLockAliasPath, Alias: "h:/work/app"},
		{Kind: core.RepoLockAliasPath},
	}
	strs := RepoLockAliasStrings(aliases)
	require.Equal(t, []string{"github.com/example/app", "h:/work/app"}, strs)
	back := RepoLockAliasesOf(append(strs, ""))
	require.Len(t, back, 2)
	require.Equal(t, "h:/work/app", back[1].Alias)
}
