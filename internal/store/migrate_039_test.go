package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// Migration 039 (fenced sessions) only adds: a fresh database has the new
// tables and columns, a database the previous release left at version 38
// upgrades with its rows reading back at the new defaults, and the previous
// release's own statements -- every column list it names -- still run against
// the new schema, in both directions.

// The previous release's statements over the three tables 039 extends,
// verbatim from sessions.go, leases.go and grants.go at schema version 38.
const (
	migration039PrevSessionCols = `id, name, project_slug, status, findings, claude_session_id,
	external_client, cwd, host, source, model, ambient, favorite, metadata, created_at, updated_at,
	input_tokens, cached_input_tokens, cache_creation_tokens, output_tokens, total_tokens, account`
	migration039PrevSessionInsert = `
		INSERT INTO sessions
		    (id, name, project_slug, status, findings, claude_session_id,
		     external_client, cwd, host, source, model, ambient, metadata, created_at, updated_at, account)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	migration039PrevSessionUpdate = `
		UPDATE sessions
		   SET project_slug = ?, status = ?, findings = ?, metadata = ?, updated_at = ?
		 WHERE id = ?`
	migration039PrevApprovalCols = `id, project_slug, kind, challenge, status, requested_at, expires_at, lease_seconds,
	peer_uid, peer_pid, process_key, program, session_id, session_name, cwd, repo, model, reason,
	decided_at, decided_by, lease_id, from_project`
	migration039PrevLockInsert = `INSERT INTO access_approvals (id, project_slug, kind, challenge, status,
		requested_at, expires_at, lease_seconds, peer_uid, peer_pid, process_key, program,
		session_id, session_name, cwd, repo, model, reason)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	migration039PrevGrantRequestInsert = `INSERT INTO access_approvals (id, project_slug, kind, challenge, status,
		requested_at, expires_at, lease_seconds, peer_uid, peer_pid, process_key, program,
		session_id, session_name, cwd, repo, model, reason, from_project)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	migration039PrevGrantCols = `id, project_slug, from_project, process_key, session_id, peer_uid, peer_pid, approval_id,
	decided_by, granted_at, expires_at, revoked_at, revoked_reason`
	migration039PrevGrantInsert = `INSERT INTO access_grants (id, project_slug, from_project, process_key, session_id,
		peer_uid, peer_pid, approval_id, decided_by, granted_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	// migration039PrevSessionHolder is the previous release's session holder
	// clause (holderClause at schema 38).
	migration039PrevSessionHolder = `process_key = '' AND session_id = ?`
)

var migration039Now = time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)

// migration039PrevWrites writes one row of each kind the way the previous
// release does: an ambient session, a lock's request, a grant request and its
// grant. It returns their ids.
func migration039PrevWrites(t *testing.T, db *sql.DB, suffix string) (session, lock, grantReq, grant string) {
	t.Helper()
	ctx := context.Background()
	ts := core.FormatTime(migration039Now)
	until := core.FormatTime(migration039Now.Add(time.Hour))
	session, lock, grantReq, grant = "01SESSPREV"+suffix, "01LOCKPREV"+suffix, "01GREQPREV"+suffix, "01GRANTPREV"+suffix
	_, err := db.ExecContext(ctx, migration039PrevSessionInsert,
		session, "cc/prev-"+suffix, "example-ops", "active", "", "ext-prev-"+suffix,
		"claude-code", "/work/example-ops", "example-host", "startup", "claude-opus-5-5", 1, "{}", ts, ts, "")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, migration039PrevLockInsert,
		lock, "example-finance", AccessKindRequest, make([]byte, AccessChallengeBytes), ApprovalPending, ts, until, 3600,
		501, 4242, "boot-1:4242:7", "/opt/seamless/bin/seam", session, "cc/prev", "/work/example-ops", "example-ops",
		"claude-opus-5-5", "reconcile")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, migration039PrevGrantRequestInsert,
		grantReq, "example-finance", AccessKindGrant, make([]byte, AccessChallengeBytes), ApprovalApproved, ts, until, 3600,
		0, 0, "", "", session, "cc/prev", "/work/example-ops", "example-ops", "claude-opus-5-5", "invoices", "example-ops")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, migration039PrevGrantInsert,
		grant, "example-finance", "example-ops", "", session, 0, 0, grantReq, "bearer-key", ts, until)
	require.NoError(t, err)
	return session, lock, grantReq, grant
}

// migration039PrevReads runs the previous release's SELECTs and returns how many
// rows and columns each read: they must run, and name exactly its columns.
func migration039PrevReads(t *testing.T, db *sql.DB) map[string][2]int {
	t.Helper()
	out := map[string][2]int{}
	for name, q := range map[string]string{
		"sessions":         `SELECT ` + migration039PrevSessionCols + ` FROM sessions`,
		"access_approvals": `SELECT ` + migration039PrevApprovalCols + ` FROM access_approvals`,
		"access_grants":    `SELECT ` + migration039PrevGrantCols + ` FROM access_grants`,
	} {
		out[name] = migration039Read(t, db, q)
	}
	return out
}

// migration039Read scans every row of q into its own columns, returning the
// row and column counts.
func migration039Read(t *testing.T, db *sql.DB, q string) [2]int {
	t.Helper()
	rows, err := db.Query(q)
	require.NoError(t, err, q)
	defer func() { require.NoError(t, rows.Close()) }()
	cols, err := rows.Columns()
	require.NoError(t, err)
	n := 0
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		require.NoError(t, rows.Scan(ptrs...), q)
		n++
	}
	require.NoError(t, rows.Err())
	return [2]int{n, len(cols)}
}

func migration039Count(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM `+table).Scan(&n))
	return n
}

func TestMigration039_FreshSchemaDefaultsAndChecks(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	v, err := SchemaVersion(db)
	require.NoError(t, err)
	require.GreaterOrEqual(t, v, 39)
	for _, table := range []string{"access_moves", "access_flows", "item_provenance"} {
		require.Zero(t, migration039Count(t, db, table), "%s starts empty", table)
	}

	// The new columns take their defaults for every row today's paths write.
	lock, err := CreateAccessApproval(ctx, db, leaseReq("example-finance", fenceProc), migration039Now)
	require.NoError(t, err)
	require.Equal(t, GrantAccessRead, lock.RequestedAccess)
	require.True(t, lock.Carry.IsZero())
	require.Empty(t, lock.ConversationClient+lock.ConversationID)
	req, err := CreateGrantRequest(ctx, db, grantReq("S1"), migration039Now)
	require.NoError(t, err)
	require.Equal(t, GrantAccessRead, req.RequestedAccess)
	g, err := ApproveAccessGrant(ctx, db, req.ID, req.Challenge, "bearer-key", 0, migration039Now)
	require.NoError(t, err)
	stored, err := AccessGrantByID(ctx, db, g.ID)
	require.NoError(t, err)
	require.Equal(t, GrantAccessRead, stored.Access)
	require.Empty(t, stored.ConversationClient+stored.ConversationID)

	ins := func(q string, args ...any) error {
		_, err := db.ExecContext(ctx, q, args...)
		return err
	}
	ts := core.FormatTime(migration039Now)
	approval := func(id, extraCols, extraVals string) error {
		return ins(`INSERT INTO access_approvals (id, project_slug, kind, challenge, status, requested_at, expires_at,
			lease_seconds, peer_uid, peer_pid, process_key`+extraCols+`) VALUES (?, 'p', 'grant', ?, 'pending', 'x', 'x', 60, 0, 0, ''`+extraVals+`)`,
			id, make([]byte, AccessChallengeBytes))
	}
	require.Error(t, approval("01BADACCESS", ", requested_access", ", 'admin'"), "the access is read or write")
	require.Error(t, approval("01BADCARRY", ", carry", ", 'not json'"), "a carry is JSON")
	require.Error(t, approval("01HALFCONV1", ", conversation_client", ", 'claude-code'"), "a conversation is both halves")
	require.Error(t, approval("01HALFCONV2", ", conversation_id", ", 'ext-1'"), "a conversation is both halves")
	require.NoError(t, approval("01FULLCONV", ", conversation_client, conversation_id, carry", `, 'claude-code', 'ext-1', '{"home":"a"}'`))

	grant := func(id, extraCols, extraVals string) error {
		return ins(`INSERT INTO access_grants (id, project_slug, from_project, session_id, approval_id, decided_by,
			granted_at, expires_at`+extraCols+`) VALUES (?, 'a', 'b', 'S', ?, 'x', 'x', 'x'`+extraVals+`)`, id, req.ID)
	}
	require.Error(t, grant("01G1", ", access", ", 'admin'"))
	require.Error(t, grant("01G2", ", conversation_client", ", 'claude-code'"))
	require.NoError(t, grant("01G3", ", access, conversation_client, conversation_id", ", 'write', 'claude-code', 'ext-1'"))

	move := func(id, approvalID, holder, fromSess, toSess, from, to string) error {
		return ins(`INSERT INTO access_moves (id, approval_id, holder, from_session_id, to_session_id, from_project,
			to_project, decided_by, moved_at) VALUES (?, ?, ?, ?, ?, ?, ?, 'owner', ?)`, id, approvalID, holder, fromSess, toSess, from, to, ts)
	}
	require.NoError(t, move("01M1", req.ID, "conversation:claude-code/ext-1", "S1", "S2", "example-ops", "example-finance"))
	require.Error(t, move("01M2", req.ID, "conversation:claude-code/ext-1", "S1", "S3", "example-ops", "example-legal"), "one move per approval")
	require.Error(t, move("01M3", lock.ID, "", "S1", "S2", "example-ops", "example-finance"), "a move names its holder")
	require.Error(t, move("01M4", lock.ID, "h", "S1", "S2", "example-ops", ""), "a move is into a project, never the global scope")
	require.Error(t, move("01M5", lock.ID, "h", "S1", "S2", "example-ops", "example-ops"), "a move goes somewhere")
	require.Error(t, move("01M6", lock.ID, "h", "S1", "S1", "", "example-ops"), "the child is another session")
	require.Error(t, move("01M7", "01NOSUCHAPPROVAL", "h", "S1", "S2", "", "example-ops"), "a move names a real approval")

	flow := func(id, holder, from, to string) error {
		return ins(`INSERT INTO access_flows (id, approval_id, holder, from_project, to_project, created_at)
			VALUES (?, ?, ?, ?, ?, ?)`, id, req.ID, holder, from, to, ts)
	}
	require.NoError(t, flow("01F1", "h", "example-finance", "example-ops"))
	require.Error(t, flow("01F2", "h", "example-finance", "example-ops"), "one row per holder and pair")
	require.NoError(t, flow("01F3", "other", "example-finance", "example-ops"), "another holder's flow is its own")
	require.Error(t, flow("01F4", "h", "", "example-ops"), "the global scope is never fenced content")
	require.Error(t, flow("01F5", "h", "example-finance", ""), "the global scope is never cleared")
	require.Error(t, flow("01F6", "h", "example-ops", "example-ops"))
	require.Error(t, flow("01F7", "", "example-finance", "example-ops"))

	stamp := func(kind, id, project, source, approvalID string) error {
		return ins(`INSERT INTO item_provenance (item_kind, item_id, project_slug, source_project, approval_id, created_at)
			VALUES (?, ?, ?, ?, ?, ?)`, kind, id, project, source, approvalID, ts)
	}
	require.NoError(t, stamp("task", "01T", "example-ops", "example-finance", "01A"))
	require.Error(t, stamp("task", "01T", "example-legal", "example-finance", "01B"), "one stamp per item and source")
	require.NoError(t, stamp("task", "01T", "example-ops", "example-legal", "01A"))
	require.Error(t, stamp("task", "01T2", "example-ops", "example-ops", "01A"), "an item never needs a stamp for its own project")
	require.Error(t, stamp("task", "01T3", "example-ops", "example-finance", ""), "a stamp names its approval")
}

func TestMigration039_UpgradesA038Database(t *testing.T) {
	db := openPartialDB(t, 39)
	ctx := context.Background()
	v, err := SchemaVersion(db)
	require.NoError(t, err)
	require.Equal(t, 38, v, "the fixture is the previous release's schema")
	session, lock, grantReqID, grantID := migration039PrevWrites(t, db, "A")

	require.NoError(t, migrate(db, migrationList()))
	v, err = SchemaVersion(db)
	require.NoError(t, err)
	require.Equal(t, LatestSchemaVersion(), v)

	s, found, err := SessionByID(ctx, db, session)
	require.NoError(t, err)
	require.True(t, found)
	require.Empty(t, s.ParentSessionID, "a session from before the column continues none")
	require.Equal(t, "ext-prev-A", s.ExternalSessionID)

	a, err := AccessApprovalByID(ctx, db, lock)
	require.NoError(t, err)
	require.Equal(t, GrantAccessRead, a.RequestedAccess)
	require.True(t, a.Carry.IsZero())
	require.Equal(t, HolderProcess, a.Holder().Kind())
	require.Equal(t, "boot-1:4242:7", a.Holder().ProcessKey())

	r, err := AccessApprovalByID(ctx, db, grantReqID)
	require.NoError(t, err)
	require.Equal(t, "example-ops", r.FromProject)
	require.Equal(t, HolderSession, r.Holder().Kind(), "a request the previous release filed keeps its session holder")

	g, err := AccessGrantByID(ctx, db, grantID)
	require.NoError(t, err)
	require.Equal(t, GrantAccessRead, g.Access, "every grant from before is read-only")
	h, err := HolderOfSession(session)
	require.NoError(t, err)
	require.Equal(t, h, g.Holder())
	byHolder, err := HolderAccessGrants(ctx, db, []Holder{h})
	require.NoError(t, err)
	require.Len(t, byHolder, 1)

	for _, table := range []string{"access_moves", "access_flows", "item_provenance"} {
		require.Zero(t, migration039Count(t, db, table), "%s is empty after the upgrade", table)
	}
}

// The previous release keeps running on a migrated database after a rollback
// whose restore failed, or a downgrade: its writes take the defaults, and its
// reads -- by column name -- see every row, the new release's included.
func TestMigration039_PreviousReleaseStatementsStillRun(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	session, lock, _, grantID := migration039PrevWrites(t, db, "B")
	_, err := db.ExecContext(ctx, migration039PrevSessionUpdate,
		"example-ops", "completed", "findings", "{}", core.FormatTime(migration039Now.Add(time.Minute)), session)
	require.NoError(t, err)

	// What the previous release wrote reads at the defaults.
	s, found, err := SessionByID(ctx, db, session)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, core.SessionCompleted, s.Status)
	require.Empty(t, s.ParentSessionID)
	a, err := AccessApprovalByID(ctx, db, lock)
	require.NoError(t, err)
	require.Equal(t, GrantAccessRead, a.RequestedAccess)
	g, err := AccessGrantByID(ctx, db, grantID)
	require.NoError(t, err)
	require.Equal(t, GrantAccessRead, g.Access)

	// What this release writes, the previous release reads by its own columns:
	// a child session, a conversation's grant request and grant, a move.
	require.NoError(t, CreateSession(ctx, db, core.Session{ID: "01CHILD", Name: "cc/child", ProjectSlug: "example-finance",
		Status: core.SessionActive, ParentSessionID: session, CreatedAt: migration039Now, UpdatedAt: migration039Now}))
	req := grantReq("S9")
	req.ConversationClient, req.ConversationID = "claude-code", "ext-conv"
	cr, err := CreateGrantRequest(ctx, db, req, migration039Now)
	require.NoError(t, err)
	cg, err := ApproveAccessGrant(ctx, db, cr.ID, cr.Challenge, "bearer-key", 0, migration039Now)
	require.NoError(t, err)
	_, mv := moveApproved(t, db, moveReq("example-finance", "example-ops"))

	got := migration039PrevReads(t, db)
	require.Equal(t, [2]int{2, 22}, got["sessions"], "the previous release's session and this release's child")
	require.Equal(t, [2]int{4, 22}, got["access_approvals"])
	require.Equal(t, [2]int{2, 13}, got["access_grants"])

	// The previous release has no conversations: it reads a conversation's
	// grant as the grant of the session that asked, which its row still names.
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM access_grants WHERE `+migration039PrevSessionHolder+` AND id = ?`,
		"S9", cg.ID).Scan(&n))
	require.Equal(t, 1, n)
	// This release keeps the two apart.
	sessHolder, err := HolderOfSession("S9")
	require.NoError(t, err)
	byHolder, err := HolderAccessGrants(ctx, db, []Holder{sessHolder})
	require.NoError(t, err)
	require.Empty(t, byHolder, "a conversation's grant never answers for the session that asked")
	require.Equal(t, HolderConversation, cg.Holder().Kind())

	child, found, err := SessionByID(ctx, db, "01CHILD")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, session, child.ParentSessionID)
	require.Equal(t, migration039Conversation(t), mv.Holder)
}

// An update rollback resets access on the pre-update snapshot without
// migrating it: on a 038 database, which has none of 039's tables or columns,
// it ends the grants as before.
func TestMigration039_ResetOnA038Snapshot(t *testing.T) {
	db := openPartialDB(t, 39)
	_, _, _, grantID := migration039PrevWrites(t, db, "C")
	got, err := ResetRestoredAccess(context.Background(), db, migration039Now.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, RestoredAccess{Grants: 1}, got)
	var reason string
	require.NoError(t, db.QueryRow(`SELECT revoked_reason FROM access_grants WHERE id = ?`, grantID).Scan(&reason))
	require.Equal(t, LeaseRevokedReset, reason)
}

// migration039Conversation is the conversation the fixtures move.
func migration039Conversation(t *testing.T) Holder {
	t.Helper()
	h, err := HolderOfConversation("claude-code", "ext-conv-1")
	require.NoError(t, err)
	return h
}

// sessions.parent_session_id is set once, at creation: every read carries it
// (RETURNING included), UpdateSession never touches it, and a session without
// one says nothing about it in JSON.
func TestMigration039_SessionParentIsSetOnce(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	now := migration039Now
	require.NoError(t, CreateSession(ctx, db, core.Session{ID: "01PARENT", Name: "cc/parent", ProjectSlug: "example-ops",
		Status: core.SessionCompleted, ExternalClient: "claude-code", ExternalSessionID: "ext-1", CreatedAt: now, UpdatedAt: now}))
	require.NoError(t, CreateSession(ctx, db, core.Session{ID: "01CHILD", Name: "cc/child", ProjectSlug: "example-finance",
		Status: core.SessionExpired, ExternalClient: "claude-code", ExternalSessionID: "ext-1", Ambient: true,
		ParentSessionID: "01PARENT", CreatedAt: now, UpdatedAt: now}))

	child, found, err := SessionByID(ctx, db, "01CHILD")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "01PARENT", child.ParentSessionID)
	parent, _, err := SessionByID(ctx, db, "01PARENT")
	require.NoError(t, err)
	require.Empty(t, parent.ParentSessionID)

	child.ParentSessionID, child.Findings = "", "done"
	child.UpdatedAt = now.Add(time.Minute)
	require.NoError(t, UpdateSession(ctx, db, child))
	again, _, err := SessionByID(ctx, db, "01CHILD")
	require.NoError(t, err)
	require.Equal(t, "01PARENT", again.ParentSessionID, "UpdateSession never changes the parent")
	require.Equal(t, "done", again.Findings)

	revived, found, err := ReactivateAmbientSession(ctx, db, "claude-code", "ext-1", "", now.Add(2*time.Minute))
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "01PARENT", revived.ParentSessionID, "a RETURNING read carries it too")

	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE parent_session_id IS NULL`).Scan(&n))
	require.Equal(t, 1, n, `"none" is NULL, as for every row from before the column`)

	plain, err := json.Marshal(parent)
	require.NoError(t, err)
	require.NotContains(t, string(plain), "parentSessionId", "an empty parent is absent, so every JSON surface reads as before")
	withParent, err := json.Marshal(again)
	require.NoError(t, err)
	require.Contains(t, string(withParent), `"parentSessionId":"01PARENT"`)
}

// migration039Plan returns EXPLAIN QUERY PLAN's detail lines for q.
func migration039Plan(t *testing.T, db *sql.DB, q string, args ...any) []string {
	t.Helper()
	rows, err := db.Query(`EXPLAIN QUERY PLAN `+q, args...)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var out []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &notused, &detail))
		out = append(out, detail)
	}
	require.NoError(t, rows.Err())
	return out
}

// The lookups of a conversation's rows search their indexes instead of
// scanning the table: the record routing's read of the explicit sessions a
// conversation linked (verbatim from internal/store/sessions_links.go), the
// label's read of all its sessions, and its grants and requests.
func TestMigration039_ConversationLookupsUseTheirIndexes(t *testing.T) {
	db := leaseDB(t)
	h := migration039Conversation(t)
	grantClause, grantArgs, ok, err := holderAccessClause([]Holder{h})
	require.NoError(t, err)
	require.True(t, ok)
	for _, c := range []struct {
		name, q, index string
		args           []any
	}{
		{"linked sessions", `SELECT ` + sessionCols + ` FROM sessions
			WHERE ambient = 0 AND external_client = ? AND claude_session_id = ?
			ORDER BY created_at DESC, id DESC`, "idx_sessions_conversation", []any{"claude-code", "ext-conv-1"}},
		{"label sessions", `SELECT DISTINCT project_slug FROM sessions WHERE (external_client = ? AND claude_session_id = ?)
			ORDER BY project_slug`, "idx_sessions_conversation", []any{"claude-code", "ext-conv-1"}},
		{"grants", `SELECT ` + grantCols + ` FROM access_grants WHERE ` + grantClause + ` ORDER BY granted_at DESC, id DESC`,
			"idx_access_grants_conversation", grantArgs},
		{"requests", `SELECT ` + approvalCols + ` FROM access_approvals WHERE ` + grantClause + ` ORDER BY requested_at DESC, id DESC`,
			"idx_access_approvals_conversation", grantArgs},
	} {
		plan := migration039Plan(t, db, c.q, c.args...)
		joined := strings.Join(plan, "\n")
		require.Contains(t, joined, "USING INDEX "+c.index, "%s: %s", c.name, joined)
		require.NotContains(t, joined, "SCAN ", "%s: %s", c.name, joined)
	}
	plan := migration039Plan(t, db, `SELECT `+sessionCols+` FROM sessions
		WHERE ambient = 0 AND external_client = ? AND claude_session_id = ?
		ORDER BY created_at DESC, id DESC`, "claude-code", "ext-conv-1")
	require.NotContains(t, strings.Join(plan, "\n"), "TEMP B-TREE", "the newest-first order is the index's own")
}
