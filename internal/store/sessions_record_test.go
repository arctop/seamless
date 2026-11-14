package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// homeSessionTokens sets a session's token totals directly, as a harvest
// before a move left them.
func homeSessionTokens(t *testing.T, db *sql.DB, id string, u core.TokenUsage) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `UPDATE sessions SET input_tokens = ?, cached_input_tokens = ?,
		cache_creation_tokens = ?, output_tokens = ?, total_tokens = ? WHERE id = ?`,
		u.Input, u.Cached, u.CacheCreation, u.Output, u.Total, id)
	require.NoError(t, err)
}

// A move's child continues a conversation whose earlier sessions already hold
// the totals they harvested, while the transcript's totals are the whole
// conversation's: the child takes what came after -- through every move of
// the chain -- so the conversation's usage is counted once. A harvest that
// adds nothing writes nothing, and a damaged chain that loops still ends.
func TestSetAmbientSessionTokens_AMoveChildKeepsWhatCameAfter(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	now := time.Now().UTC()
	mk := func(id, name, project, parent string, ambient bool, status core.SessionStatus) {
		require.NoError(t, CreateSession(ctx, db, core.Session{
			ID: id, Name: name, ProjectSlug: project, Status: status, Ambient: ambient, ParentSessionID: parent,
			ExternalSessionID: "conv-home-1", ExternalClient: "codex", CreatedAt: now, UpdatedAt: now,
		}))
	}
	// Two moves: example-ops -> example-finance -> example-sealed. The rows
	// the moves ended are no longer ambient and keep what they harvested.
	mk("01HTOKA0000000000000000000", "cx/conv", "example-ops", "", false, core.SessionCompleted)
	mk("01HTOKB0000000000000000000", "cx/conv@example-finance", "example-finance", "01HTOKA0000000000000000000", false, core.SessionCompleted)
	mk("01HTOKC0000000000000000000", "cx/conv@example-sealed", "example-sealed", "01HTOKB0000000000000000000", true, core.SessionActive)
	homeSessionTokens(t, db, "01HTOKA0000000000000000000", core.TokenUsage{Input: 100, Output: 10, Total: 110})
	homeSessionTokens(t, db, "01HTOKB0000000000000000000", core.TokenUsage{Input: 40, Cached: 20, Output: 5, Total: 65})

	tokens := func(id string) core.TokenUsage {
		s, ok, err := SessionByID(ctx, db, id)
		require.NoError(t, err)
		require.True(t, ok)
		return s.Tokens
	}
	ok, err := SetAmbientSessionTokens(ctx, db, "codex", "conv-home-1",
		core.TokenUsage{Input: 300, Cached: 20, Output: 30, Total: 350}, now)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, core.TokenUsage{Input: 160, Output: 15, Total: 175}, tokens("01HTOKC0000000000000000000"))
	require.Equal(t, 110, tokens("01HTOKA0000000000000000000").Total, "the earlier sessions keep theirs")
	require.Equal(t, 65, tokens("01HTOKB0000000000000000000").Total)

	ok, err = SetAmbientSessionTokens(ctx, db, "codex", "conv-home-1",
		core.TokenUsage{Input: 140, Cached: 20, Output: 15, Total: 175}, now)
	require.NoError(t, err)
	require.False(t, ok, "nothing after the moves: nothing written")
	require.Equal(t, 175, tokens("01HTOKC0000000000000000000").Total)

	_, err = db.ExecContext(ctx, `UPDATE sessions SET parent_session_id = ? WHERE id = ?`,
		"01HTOKC0000000000000000000", "01HTOKA0000000000000000000")
	require.NoError(t, err)
	ok, err = SetAmbientSessionTokens(ctx, db, "codex", "conv-home-1",
		core.TokenUsage{Input: 400, Cached: 20, Output: 40, Total: 460}, now)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, core.TokenUsage{Input: 260, Output: 25, Total: 285}, tokens("01HTOKC0000000000000000000"),
		"a loop in the chain counts each earlier session once")
}

// The briefing's Recent findings and Sibling findings skip a session whose
// findings are only a metadata-only line of the record routing -- a move's
// pointer, the not-recorded line -- as they skip the no-summary sentinel. An
// agent's own findings with a pointer after them still count, and so does
// P0's pointer, which every install has shown since it shipped.
func TestRecentFindings_SkipsRecordLines(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	now := time.Now().UTC()
	pointer := MovePointer("example-finance", "cc/conv-home@example-finance")
	mk := func(id, name string, ambient bool, findings string, ago time.Duration) {
		require.NoError(t, CreateSession(ctx, db, core.Session{
			ID: id, Name: name, ProjectSlug: "example-ops", Status: core.SessionCompleted, Ambient: ambient,
			Findings: findings, CreatedAt: now.Add(-ago), UpdatedAt: now.Add(-ago),
		}))
	}
	mk("01HRF1", "cc/moved", false, pointer, time.Minute)
	mk("01HRF2", "cc/moved-own", false, "Reviewed the rollout.\n\n"+pointer, 2*time.Minute)
	mk("01HRF3", "cc/unrecorded", true,
		UnrecordedFindings("session", []string{"example-legal", "example-sealed"}, "project example-ops"), 3*time.Minute)
	mk("01HRF4", "cc/p0", true, "Continued in project example-finance; this conversation's findings go there, not here.", 4*time.Minute)
	mk("01HRF5", "cc/plain", true, "Shipped 100% of the cache_ttl change.", 5*time.Minute)

	want := []string{"Reviewed the rollout.\n\n" + pointer,
		"Continued in project example-finance; this conversation's findings go there, not here.",
		"Shipped 100% of the cache_ttl change."}
	findings := func(ss []core.Session) []string {
		out := make([]string, len(ss))
		for i, s := range ss {
			out[i] = s.Findings
		}
		return out
	}
	rf, err := RecentFindings(ctx, db, "example-ops", 10)
	require.NoError(t, err)
	require.Equal(t, want, findings(rf))
	sf, err := SiblingFindings(ctx, db, []string{"example-ops"}, 10)
	require.NoError(t, err)
	require.Equal(t, want, findings(sf))
}

// IsRecordLine knows the metadata-only lines by their builders' shape.
func TestIsRecordLine(t *testing.T) {
	for _, tc := range []struct {
		findings string
		want     bool
	}{
		{MovePointer("example-finance", "cc/conv@example-finance"), true},
		{UnrecordedFindings("agent", []string{"example-legal"}, "the global scope"), true},
		{UnrecordedFindings("session", []string{"example-legal", "example-sealed"}, "project example-ops"), true},
		{"Reviewed the rollout.\n\n" + MovePointer("example-finance", "cc/conv"), false},
		{"Continued in project example-finance; this conversation's findings go there, not here.", false},
		{"Not recorded: nothing to say.", false},
		{"", false},
	} {
		require.Equal(t, tc.want, IsRecordLine(tc.findings), "%q", tc.findings)
	}
}

// RecordDestination.Home is the ambient row's project, and only when the
// record goes there.
func TestRecordDestination_Home(t *testing.T) {
	amb := core.Session{ProjectSlug: "example-finance"}
	for _, tc := range []struct {
		name string
		d    RecordDestination
		home string
		ok   bool
	}{
		{"ambient", RecordDestination{Kind: RecordDestAmbient, Ambient: amb, HasAmbient: true}, "example-finance", true},
		{"global ambient", RecordDestination{Kind: RecordDestAmbient, HasAmbient: true}, "", true},
		{"no ambient row", RecordDestination{Kind: RecordDestAmbient}, "", false},
		{"link", RecordDestination{Kind: RecordDestLink, Ambient: amb, HasAmbient: true}, "", false},
		{"withheld", RecordDestination{Kind: RecordDestWithheld, Ambient: amb, HasAmbient: true}, "", false},
	} {
		home, ok := tc.d.Home()
		require.Equal(t, tc.home, home, tc.name)
		require.Equal(t, tc.ok, ok, tc.name)
	}
}
