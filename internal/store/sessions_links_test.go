package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// linkSession inserts a session for one external identity: the ambient row, or
// an explicit session linked to it the way session_start links one (same
// external id and client, ambient = 0).
func linkSession(t *testing.T, db *sql.DB, id, project string, ambient bool, client, external string, status core.SessionStatus, at time.Time) core.Session {
	t.Helper()
	name := "sess/" + id
	if ambient {
		name = "cc/" + id
	}
	s := core.Session{
		ID: id, Name: name, ProjectSlug: project, Status: status, Ambient: ambient,
		ExternalSessionID: external, ExternalClient: client,
		CreatedAt: at, UpdatedAt: at,
	}
	require.NoError(t, CreateSession(context.Background(), db, s))
	return s
}

// linkFTS reads a session's search mirror row: its project and body.
func linkFTS(t *testing.T, db *sql.DB, id string) (project, body string, found bool) {
	t.Helper()
	err := db.QueryRowContext(context.Background(),
		`SELECT project, body FROM fts WHERE item_id = ?`, id).Scan(&project, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false
	}
	require.NoError(t, err)
	return project, body, true
}

// The links are every explicit session stamped with the conversation's
// identity, in any status -- a link the agent ended, or the reaper expired,
// still says where the conversation went -- newest first, and nothing else:
// not the ambient row, not another client's or another id's session.
func TestFencedLinks_EveryStatusNewestFirst(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	const ext = "conv-0001"

	linkSession(t, db, "01AMB", "example-ops", true, "claude-code", ext, core.SessionActive, base)
	linkSession(t, db, "01L1", "example-finance", false, "claude-code", ext, core.SessionCompleted, base.Add(time.Minute))
	linkSession(t, db, "01L2", "example-ops", false, "claude-code", ext, core.SessionExpired, base.Add(2*time.Minute))
	linkSession(t, db, "01L3", "example-sealed", false, "claude-code", ext, core.SessionActive, base.Add(3*time.Minute))
	// Not this conversation's: another client with the same id, another id,
	// and an explicit session linked to nothing.
	linkSession(t, db, "01CX", "example-finance", false, "codex", ext, core.SessionActive, base.Add(4*time.Minute))
	linkSession(t, db, "01OT", "example-finance", false, "claude-code", "conv-0002", core.SessionActive, base.Add(5*time.Minute))
	linkSession(t, db, "01NO", "example-finance", false, "", "", core.SessionActive, base.Add(6*time.Minute))

	links, err := LinkedSessionsByExternalIdentity(ctx, db, "claude-code", ext)
	require.NoError(t, err)
	ids := make([]string, 0, len(links))
	for _, l := range links {
		ids = append(ids, l.ID)
		require.False(t, l.Ambient)
	}
	require.Equal(t, []string{"01L3", "01L2", "01L1"}, ids)

	for _, missing := range [][2]string{{"", ext}, {"claude-code", ""}, {"", ""}} {
		links, err := LinkedSessionsByExternalIdentity(ctx, db, missing[0], missing[1])
		require.NoError(t, err)
		require.Empty(t, links, "an incomplete identity matches nothing")
	}
}

// LinkUpdateFindings is a compare-and-set on the findings the caller read:
// findings the agent wrote in between are never overwritten. Without complete
// it never changes the status and moves updated_at only on an active session;
// with it, it ends the session the way a graceful SessionEnd does. The search
// mirror follows the write.
func TestFencedLinks_UpdateFindingsIsACompareAndSet(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	later := base.Add(time.Hour)

	active := linkSession(t, db, "01ACT", "example-finance", false, "claude-code", "conv-1", core.SessionActive, base)
	got, ok, err := LinkUpdateFindings(ctx, db, active.ID, "", "(auto-harvested) turn one", false, later)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "(auto-harvested) turn one", got.Findings)
	require.Equal(t, core.SessionActive, got.Status)
	require.Equal(t, later, got.UpdatedAt.UTC(), "an active session's write is a heartbeat")
	project, body, found := linkFTS(t, db, active.ID)
	require.True(t, found)
	require.Equal(t, "example-finance", project)
	require.Equal(t, "(auto-harvested) turn one", body)

	// The agent wrote its own findings after the caller read the row: the
	// stale prior refuses the write.
	got.Findings = "the agent's own words"
	require.NoError(t, UpdateSession(ctx, db, got))
	_, ok, err = LinkUpdateFindings(ctx, db, active.ID, "(auto-harvested) turn one", "(auto-harvested) turn two", false, later)
	require.NoError(t, err)
	require.False(t, ok)
	reread, _, err := SessionByID(ctx, db, active.ID)
	require.NoError(t, err)
	require.Equal(t, "the agent's own words", reread.Findings)

	// An expired session keeps its status and the time it was last alive.
	expired := linkSession(t, db, "01EXP", "example-finance", false, "claude-code", "conv-1", core.SessionExpired, base)
	got, ok, err = LinkUpdateFindings(ctx, db, expired.ID, "", "(auto-harvested) late turn", false, later)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, core.SessionExpired, got.Status)
	require.Equal(t, base, got.UpdatedAt.UTC())

	// complete ends it, at now.
	got, ok, err = LinkUpdateFindings(ctx, db, expired.ID, "(auto-harvested) late turn", "(auto-harvested) the end", true, later)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, core.SessionCompleted, got.Status)
	require.Equal(t, later, got.UpdatedAt.UTC())
	require.Equal(t, "(auto-harvested) the end", got.Findings)

	// Never an ambient row, an unknown id, or an empty write.
	amb := linkSession(t, db, "01AMB", "example-ops", true, "claude-code", "conv-1", core.SessionActive, base)
	for _, tc := range []struct{ id, findings string }{
		{amb.ID, "x"}, {"01NONE", "x"}, {active.ID, ""}, {"", "x"},
	} {
		_, ok, err := LinkUpdateFindings(ctx, db, tc.id, "", tc.findings, false, later)
		require.NoError(t, err)
		require.False(t, ok, "%+v", tc)
	}
	reread, _, err = SessionByID(ctx, db, amb.ID)
	require.NoError(t, err)
	require.Empty(t, reread.Findings)
}
