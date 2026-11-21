package store

// The usage backfill's reads (`seam usage backfill`, internal/hooks
// usage_backfill.go): the conversations a principal started, which a backfill
// may fill from the transcripts on their computer, and what a conversation's
// ledger holds now, so a backfill writes only what would change. The write is
// ReplaceConversationUsage's.

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"slices"

	"github.com/arctop/seamless/internal/core"
)

// AmbientSessionsOf returns the ambient sessions account started -- one per
// conversation, "" being the owner's -- oldest first. A session another
// principal started is never among them (access.SessionAccountOK's rule).
func AmbientSessionsOf(ctx context.Context, db *sql.DB, account string) ([]core.Session, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+sessionCols+` FROM sessions
		WHERE ambient = 1 AND account = ?
		ORDER BY created_at, id`, account)
	if err != nil {
		return nil, fmt.Errorf("store.AmbientSessionsOf: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []core.Session
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("store.AmbientSessionsOf: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.AmbientSessionsOf: %w", err)
	}
	return out, nil
}

// ConversationUsage is what a conversation's ledger holds, as a backfill reads
// it before it writes.
type ConversationUsage struct {
	// Totals is the conversation's token totals: its sessions' (its ambient
	// row's, and those its moves ended) summed.
	Totals core.TokenUsage
	// Holds reports whether the ledger already holds exactly the rows asked
	// about, split among the conversation's sessions as
	// ReplaceConversationUsage would split them, each session's totals their
	// sum: a replace with them would change nothing.
	Holds bool
}

// ConversationUsageOf reads the ledger of the conversation the external
// identity names, against rows: found is false when it has no ambient row (an
// incomplete identity has none). Rows that do not hold are ErrInvalidUsage, as
// they are to the writer.
func ConversationUsageOf(ctx context.Context, db *sql.DB, externalClient, externalSessionID string, rows []core.UsageBucket) (ConversationUsage, bool, error) {
	const op = "store.ConversationUsageOf"
	if externalClient == "" || externalSessionID == "" {
		return ConversationUsage{}, false, nil
	}
	if err := validUsageRows(rows); err != nil {
		return ConversationUsage{}, false, fmt.Errorf("%s: %w", op, err)
	}
	// One transaction, so the sessions and their rows are read at one moment;
	// it writes nothing and is rolled back.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return ConversationUsage{}, false, fmt.Errorf("%s: begin: %w", op, err)
	}
	defer func() { _ = tx.Rollback() }()
	chain, err := conversationSessionsTx(ctx, tx, externalClient, externalSessionID)
	if err != nil {
		return ConversationUsage{}, false, fmt.Errorf("%s: %w", op, err)
	}
	if len(chain) == 0 {
		return ConversationUsage{}, false, nil
	}
	owned := make([][]core.UsageBucket, len(chain))
	for _, r := range rows {
		i := usageOwner(chain, r.Bucket)
		owned[i] = append(owned[i], r)
	}
	out := ConversationUsage{Holds: true}
	for i, s := range chain {
		out.Totals = addUsage(out.Totals, s.totals)
		have, err := sessionUsageTx(ctx, tx, s.id)
		if err != nil {
			return ConversationUsage{}, false, fmt.Errorf("%s: %w", op, err)
		}
		want := slices.Clone(owned[i])
		slices.SortFunc(want, compareUsageRows)
		if !slices.EqualFunc(have, want, sameUsageRow) || s.totals != core.SumUsage(want) {
			out.Holds = false
		}
	}
	return out, true, nil
}

// sessionUsageTx is a session's ledger rows, in compareUsageRows' order.
func sessionUsageTx(ctx context.Context, tx *sql.Tx, sessionID string) ([]core.UsageBucket, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+usageCols+` FROM session_usage WHERE session_id = ?`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("the ledger of session %s: %w", sessionID, err)
	}
	read, err := scanUsageRows(rows)
	if err != nil {
		return nil, fmt.Errorf("the ledger of session %s: %w", sessionID, err)
	}
	out := make([]core.UsageBucket, len(read))
	for i, r := range read {
		out[i] = r.UsageBucket
	}
	slices.SortFunc(out, compareUsageRows)
	return out, nil
}

// compareUsageRows orders a session's ledger rows by their key: bucket, model,
// thread.
func compareUsageRows(a, b core.UsageBucket) int {
	return cmp.Or(a.Bucket.Compare(b.Bucket), cmp.Compare(a.Model, b.Model), cmp.Compare(a.Thread, b.Thread))
}

// sameUsageRow reports whether two ledger rows are one: the same key and the
// same counts and minutes.
func sameUsageRow(a, b core.UsageBucket) bool {
	return a.Bucket.Equal(b.Bucket) && a.Model == b.Model && a.Thread == b.Thread && a.Input == b.Input &&
		a.Cached == b.Cached && a.CacheCreation == b.CacheCreation && a.Output == b.Output &&
		a.Messages == b.Messages && a.MinuteMask == b.MinuteMask
}
