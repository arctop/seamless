package store

// A conversation as the console's conversation page shows it (plan
// fenced-sessions P10): its sessions, the moves between them, the requests its
// sessions filed and the grants they were approved. The store lists; the
// console words what it shows, and judges what a locked project shows (its
// name, nothing inside), by reading through the request's handle: on a
// filtered handle (store.OpenView) a locked project's sessions and items are
// simply not there. The access tables are no view's (viewExempt): their rows
// are slugs, ids, the deciding proof and times -- the metadata the Access
// requests page shows for a locked project too.
//
// Which rows are a conversation's is read by the sessions that asked, never by
// the holder: under peer_auth a request's holder is the agent's process tree,
// whose key no session records, while every request, grant and move names the
// session it came from (a conversation's rows still name the session that
// asked; a move names the session it left and the child it continues in).

import (
	"context"
	"database/sql"
	"fmt"
	"slices"

	"github.com/arctop/seamless/internal/core"
)

const (
	// conversationMaxSessions bounds the sessions one conversation lookup
	// returns: a conversation is its ambient row, the sessions session_start
	// linked to it, and a move's children.
	conversationMaxSessions = 200
	// conversationMaxRows bounds each list of a conversation's record.
	conversationMaxRows = 500
)

// ConversationSessionsOf returns the sessions of sess's conversation, oldest
// first (created_at, then id): every session sharing its client and external
// session id -- the ambient row, the explicit sessions session_start linked
// to it, a move's pre-move row and its children -- in any status, at most
// conversationMaxSessions of them, sess always among them. A session with no
// complete external identity (a client that sends none) is a conversation of
// its own.
//
// Read through a filtered handle, a session in a project the handle hides is
// not there.
func ConversationSessionsOf(ctx context.Context, db *sql.DB, sess core.Session) ([]core.Session, error) {
	if sess.ExternalClient == "" || sess.ExternalSessionID == "" {
		return []core.Session{sess}, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT `+sessionCols+` FROM sessions
		WHERE external_client = ? AND claude_session_id = ?
		ORDER BY created_at, id LIMIT `+fmt.Sprint(conversationMaxSessions), sess.ExternalClient, sess.ExternalSessionID)
	if err != nil {
		return nil, fmt.Errorf("store.ConversationSessionsOf: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []core.Session
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("store.ConversationSessionsOf: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ConversationSessionsOf: %w", err)
	}
	if !slices.ContainsFunc(out, func(s core.Session) bool { return s.ID == sess.ID }) {
		// Past the bound: the session asked about is still one of them.
		out = append(out, sess)
	}
	return out, nil
}

// ConversationRecord is what a conversation's sessions asked for and were
// given, each list oldest first and at most conversationMaxRows long.
type ConversationRecord struct {
	// Moves are the moves out of or into any of its sessions.
	Moves []Move
	// Approvals are the requests its sessions filed -- every kind, every
	// status -- and the move requests behind Moves.
	Approvals []AccessApproval
	// Grants are the grants its sessions were approved: live, expired and
	// revoked alike.
	Grants []AccessGrant
}

// IsZero reports that the conversation asked for nothing and moved nowhere.
func (r ConversationRecord) IsZero() bool {
	return len(r.Moves) == 0 && len(r.Approvals) == 0 && len(r.Grants) == 0
}

// ConversationRecordOf returns the record of the conversation whose sessions
// are sessionIDs (ConversationSessionsOf): the moves that leave or enter one
// of them, every request one of them filed plus the move requests behind those
// moves, and every grant one of them was approved. A request or grant a
// session not among sessionIDs filed -- one in a project the caller's handle
// hides -- is not listed, unless it is the request behind a listed move. With
// no session it returns the zero record.
func ConversationRecordOf(ctx context.Context, db *sql.DB, sessionIDs []string) (ConversationRecord, error) {
	const op = "store.ConversationRecordOf"
	var ids []any
	for _, id := range slices.Compact(slices.Sorted(slices.Values(sessionIDs))) {
		if id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return ConversationRecord{}, nil
	}
	if len(ids) > conversationMaxSessions+1 {
		return ConversationRecord{}, fmt.Errorf("%s: %w: %d sessions, at most %d", op, ErrInvalidAccessRequest, len(ids), conversationMaxSessions+1)
	}
	in := `(` + placeholders(len(ids)) + `)`
	limit := ` LIMIT ` + fmt.Sprint(conversationMaxRows)

	var (
		rec ConversationRecord
		err error
	)
	if rec.Moves, err = conversationMoves(ctx, db, `SELECT `+moveCols+` FROM access_moves
		WHERE from_session_id IN `+in+` OR to_session_id IN `+in+`
		ORDER BY moved_at, id`+limit, append(slices.Clone(ids), ids...)); err != nil {
		return ConversationRecord{}, fmt.Errorf("%s: moves: %w", op, err)
	}

	where, args := `session_id IN `+in, slices.Clone(ids)
	if len(rec.Moves) > 0 {
		where += ` OR id IN (` + placeholders(len(rec.Moves)) + `)`
		for _, m := range rec.Moves {
			args = append(args, m.ApprovalID)
		}
	}
	err = eachRow(ctx, db, `SELECT `+approvalCols+` FROM access_approvals WHERE `+where+`
		ORDER BY requested_at, id`+limit, args, func(rows *sql.Rows) error {
		a, err := scanApproval(rows)
		if err != nil {
			return err
		}
		rec.Approvals = append(rec.Approvals, a)
		return nil
	})
	if err != nil {
		return ConversationRecord{}, fmt.Errorf("%s: requests: %w", op, err)
	}

	if rec.Grants, err = conversationGrants(ctx, db, `SELECT `+grantCols+` FROM access_grants
		WHERE session_id IN `+in+` ORDER BY granted_at, id`+limit, ids); err != nil {
		return ConversationRecord{}, fmt.Errorf("%s: grants: %w", op, err)
	}
	return rec, nil
}

// conversationMoves runs one query over access_moves.
func conversationMoves(ctx context.Context, db *sql.DB, query string, args []any) ([]Move, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return moveScan(rows)
}

// conversationGrants runs one query over access_grants.
func conversationGrants(ctx context.Context, db *sql.DB, query string, args []any) ([]AccessGrant, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanGrants(rows)
}

// AnyProvenanceStamp reports whether item_provenance holds any stamp at all
// (read through a filtered handle, any the handle does not hide): with none,
// no item anywhere may contain content from another project, and a surface
// that shows stamps has nothing to look up. Every install that never turned
// isolation.requests on answers false.
func AnyProvenanceStamp(ctx context.Context, db *sql.DB) (bool, error) {
	var found bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM item_provenance)`).Scan(&found); err != nil {
		return false, fmt.Errorf("store.AnyProvenanceStamp: %w", err)
	}
	return found, nil
}
