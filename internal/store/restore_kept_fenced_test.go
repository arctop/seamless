package store

import (
	"bytes"
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// The access record a restore must keep as it stands today: an approval, its
// grant or lease, a conversation's move and flow, a console unlock.
const (
	keptConvClient = "claude-code"
	keptConvID     = "conv-kept-1"
)

func keptTS(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func insertKeptApproval(t *testing.T, db *sql.DB, id, project, kind string, at time.Time) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO access_approvals (id, project_slug, kind, challenge, status, requested_at, expires_at,
		lease_seconds, peer_uid, peer_pid, process_key, conversation_client, conversation_id, decided_at, decided_by)
		VALUES (?, ?, ?, ?, 'approved', ?, ?, 3600, 0, 0, '', ?, ?, ?, 'bearer-key')`,
		id, project, kind, bytes.Repeat([]byte{7}, 32), keptTS(at), keptTS(at.Add(15*time.Minute)),
		keptConvClient, keptConvID, keptTS(at))
	require.NoError(t, err)
}

func insertKeptGrant(t *testing.T, db *sql.DB, id, approvalID, project, from string, at time.Time) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO access_grants (id, project_slug, from_project, session_id, approval_id, decided_by,
		granted_at, expires_at, conversation_client, conversation_id)
		VALUES (?, ?, ?, 'sess-kept', ?, 'bearer-key', ?, ?, ?, ?)`,
		id, project, from, approvalID, keptTS(at), keptTS(at.Add(8*time.Hour)), keptConvClient, keptConvID)
	require.NoError(t, err)
}

func revokedAt(t *testing.T, db *sql.DB, table, id string) sql.NullString {
	t.Helper()
	var v sql.NullString
	require.NoError(t, db.QueryRow(`SELECT revoked_at FROM "`+table+`" WHERE id = ?`, id).Scan(&v))
	return v
}

func countRows(t *testing.T, db *sql.DB, table, id string) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM "`+table+`" WHERE id = ?`, id).Scan(&n))
	return n
}

func insertKeptMove(t *testing.T, db *sql.DB, id, approvalID string, conv Holder, from, to, child string, at time.Time) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO access_moves (id, approval_id, holder, from_session_id, to_session_id, from_project,
		to_project, decided_by, moved_at) VALUES (?, ?, ?, 'sess-kept', ?, ?, ?, 'bearer-key', ?)`,
		id, approvalID, conv.Key(), child, from, to, keptTS(at))
	require.NoError(t, err)
}

func approvalStatus(t *testing.T, db *sql.DB, id string) string {
	t.Helper()
	var st string
	require.NoError(t, db.QueryRow(`SELECT status FROM access_approvals WHERE id = ?`, id).Scan(&st))
	return st
}

// TestApplyKeptState_KeepsTheConversationsAccessHistory pins the restore's
// half of the clearance rule: what a conversation holds only grows. The
// backup's database is the restored one, so without today's history a
// conversation that moved, was granted or had a flow cleared after the backup
// would lose those label entries, and an open project would be cleared again
// for what it read. Today's history is added (the backup's own rows stay), the
// approvals it names come with it, today's version winning, and every grant
// ends, as the import's ResetRestoredAccess already ended the backup's.
func TestApplyKeptState_KeepsTheConversationsAccessHistory(t *testing.T) {
	ctx := context.Background()
	backupAt := time.Now().Add(-time.Hour).UTC()
	later := backupAt.Add(30 * time.Minute)

	conv, err := HolderOfConversation(keptConvClient, keptConvID)
	require.NoError(t, err)
	fence := func(db *sql.DB) {
		for _, slug := range []string{"example-finance", "example-legal", "example-vault"} {
			ensureFenced(t, db, slug, core.IsolationConfidential)
		}
	}

	// The backup, as the import leaves it: an older move into example-vault,
	// which today's database no longer has (it was itself restored from an
	// older backup since), and a read request for example-finance still
	// waiting; then every live permission ended.
	restored := openTestDB(t)
	fence(restored)
	insertKeptApproval(t, restored, "01APPOLDMOVE", "example-vault", "move", backupAt.Add(-time.Hour))
	insertKeptMove(t, restored, "01MOVEOLD", "01APPOLDMOVE", conv, "example-ops", "example-vault", "sess-kept-old", backupAt.Add(-time.Hour))
	insertKeptApproval(t, restored, "01APPGRANT", "example-finance", "grant", backupAt)
	_, err = restored.Exec(`UPDATE access_approvals SET status = 'pending', decided_at = NULL, decided_by = '' WHERE id = '01APPGRANT'`)
	require.NoError(t, err)
	_, err = ResetRestoredAccess(ctx, restored, time.Now())
	require.NoError(t, err)

	// Today: the read was approved after the backup and its grant is live; and
	// the conversation later moved from example-ops into example-legal, with a
	// flow from example-finance into example-legal.
	today := openTestDB(t)
	fence(today)
	insertKeptApproval(t, today, "01APPGRANT", "example-finance", "grant", backupAt)
	insertKeptGrant(t, today, "01GRANTNEW", "01APPGRANT", "example-finance", "example-ops", later)
	insertKeptApproval(t, today, "01APPMOVE", "example-legal", "move", later)
	insertKeptMove(t, today, "01MOVENEW", "01APPMOVE", conv, "example-ops", "example-legal", "sess-kept-child", later)
	_, err = today.Exec(`INSERT INTO access_flows (id, approval_id, holder, from_project, to_project, created_at)
		VALUES ('01FLOWNEW', '01APPMOVE', ?, 'example-finance', 'example-legal', ?)`, conv.Key(), keptTS(later))
	require.NoError(t, err)

	kept, err := ReadKeptState(ctx, today)
	require.NoError(t, err)
	_, err = ApplyKeptState(ctx, restored, kept, ApplyKeptOptions{Now: time.Now()})
	require.NoError(t, err)

	// Today's history came in, and the backup's own stayed.
	for table, id := range map[string]string{"access_moves": "01MOVENEW", "access_flows": "01FLOWNEW", "access_grants": "01GRANTNEW"} {
		require.Equal(t, 1, countRows(t, restored, table, id), "%s %s", table, id)
	}
	require.Equal(t, 1, countRows(t, restored, "access_moves", "01MOVEOLD"), "a restore never drops history the backup holds")
	require.Equal(t, "approved", approvalStatus(t, restored, "01APPGRANT"), "today's decision of a request wins")

	// The label holds all of it.
	label, err := HolderLabel(ctx, restored, []Holder{conv}, "")
	require.NoError(t, err)
	var projects []string
	for _, e := range label {
		projects = append(projects, e.Project)
	}
	require.Subset(t, projects, []string{"example-legal", "example-finance", "example-vault"})
	flows, err := HolderFlows(ctx, restored, []Holder{conv})
	require.NoError(t, err)
	var pairs []string
	for _, f := range flows {
		pairs = append(pairs, f.From+">"+f.To)
	}
	require.Contains(t, pairs, "example-finance>example-legal")

	// No permission is live: the grant that came in today ended with the
	// restore, and the agents ask again.
	require.True(t, revokedAt(t, restored, "access_grants", "01GRANTNEW").Valid, "a grant carried over ends with the restore")
	var live int
	require.NoError(t, restored.QueryRow(`SELECT count(*) FROM access_grants WHERE revoked_at IS NULL`).Scan(&live))
	require.Zero(t, live)

	// The foreign keys still hold.
	rows, err := restored.Query(`PRAGMA foreign_key_check`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	require.False(t, rows.Next(), "no access row is left pointing at a missing approval")
	require.NoError(t, rows.Err())
}
