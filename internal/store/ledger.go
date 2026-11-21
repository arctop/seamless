package store

// The usage ledger (migration 053): a session's model token usage by
// quarter-hour (UTC), model and thread. It is the token record itself, kept on
// every install by the harvests that exist: Claude Code's SessionEnd replaces
// a conversation's rows with its transcript's buckets
// (ReplaceConversationUsage), and each Codex Stop credits what the cumulative
// counter grew by to the bucket of the Stop (CreditConversationUsage). While
// the Diary is on, a Claude Code prompt also keeps a live conversation's rows
// current (RefreshConversationUsage, Replace's rule for a read that may land
// after a later one). In the same transaction every session a write touches
// has its token totals (the sessions columns of migration 011) set to the sum
// of its rows, so every surface that reads the totals reads the ledger's sum.
//
// Every writer finds the conversation by its client's external identity: its
// ambient row, whatever its status, and the sessions its moves ended
// (isolation.requests: the parent_session_id chain). A harvest lands on an
// ambient row the agent already completed with session_end, or the idle reaper
// expired, without reviving it and without moving its updated_at: token counts
// are metadata, and the row keeps the time it ended at. Only an active row's
// updated_at moves, as the heartbeat a harvest always was.
//
// A moved conversation is one transcript across its sessions. A bucket that
// ended by the time a move began stays with the session the move ended; every
// later bucket goes to the session the move opened (usageOwner). The
// conversation's usage is counted once, and the project it left shows none of
// what came after.

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
)

// ErrInvalidUsage is a ledger write whose rows do not hold: a bucket that is
// not a quarter-hour's start, an unknown thread, a negative count, a minute
// mask past the bucket's 15 minutes, or two rows for one bucket, model and
// thread.
var ErrInvalidUsage = errors.New("store: invalid usage rows")

// UsageRow is one ledger row: SessionID's usage in one quarter-hour, of one
// model and thread.
type UsageRow struct {
	SessionID string `json:"sessionId"`
	core.UsageBucket
}

// usageCols is every column a ledger read scans (scanUsageRows), in order.
const usageCols = `session_id, bucket, model, thread, input_tokens, cached_input_tokens,
	cache_creation_tokens, output_tokens, messages, minute_mask`

// UsageBetween returns the ledger rows whose quarter-hour starts in [from,
// to), ordered by bucket, then session id, model and thread. A bucket counts
// where it starts: a local day's midnight is a quarter-hour's start in every
// real zone, so a day's bounds select exactly its buckets. Nothing is returned
// unless from is before to.
//
// The ledger's key is (session, bucket, model, thread), so each row is its
// own group of all four; a caller sums the rows over whichever it leaves out.
// On a filtered handle (OpenView) a hidden session's rows are not there, and
// the session a row names is read through the same handle (SessionsByIDs).
func UsageBetween(ctx context.Context, db *sql.DB, from, to time.Time) ([]UsageRow, error) {
	if !from.Before(to) {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT `+usageCols+` FROM session_usage
		WHERE bucket >= ? AND bucket < ?
		ORDER BY bucket, session_id, model, thread`, core.FormatTime(from), core.FormatTime(to))
	if err != nil {
		return nil, fmt.Errorf("store.UsageBetween: %w", err)
	}
	out, err := scanUsageRows(rows)
	if err != nil {
		return nil, fmt.Errorf("store.UsageBetween: %w", err)
	}
	return out, nil
}

// SessionUsage returns one session's ledger rows, ordered by bucket, model and
// thread.
func SessionUsage(ctx context.Context, db *sql.DB, sessionID string) ([]UsageRow, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+usageCols+` FROM session_usage
		WHERE session_id = ? ORDER BY bucket, model, thread`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("store.SessionUsage: %w", err)
	}
	out, err := scanUsageRows(rows)
	if err != nil {
		return nil, fmt.Errorf("store.SessionUsage: %w", err)
	}
	return out, nil
}

// scanUsageRows scans usageCols rows, closing rows.
func scanUsageRows(rows *sql.Rows) ([]UsageRow, error) {
	defer func() { _ = rows.Close() }()
	var out []UsageRow
	for rows.Next() {
		var (
			r              UsageRow
			bucket, thread string
		)
		if err := rows.Scan(&r.SessionID, &bucket, &r.Model, &thread, &r.Input, &r.Cached, &r.CacheCreation,
			&r.Output, &r.Messages, &r.MinuteMask); err != nil {
			return nil, err
		}
		at, err := core.ParseTime(bucket)
		if err != nil {
			return nil, fmt.Errorf("bucket of session %s: %w", r.SessionID, err)
		}
		r.Bucket, r.Thread = at, core.UsageThread(thread)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ReplaceConversationUsage replaces the ledger rows of the conversation the
// external identity names with rows -- a whole transcript's usage, as Claude
// Code's harvest reads it -- and sets each of its sessions' token totals to
// the sum of the rows it now holds, in one transaction. The transcript is the
// whole conversation's, so the rows are split among its sessions where it
// moved (usageOwner). Re-harvesting a transcript that grew writes its new rows
// in place of the old: idempotent, never counted twice.
//
// It reports whether the conversation has an ambient row (whatever its status:
// see the file comment). No rows, or an incomplete identity, writes nothing,
// so a transcript with no usage leaves an earlier harvest intact. Rows that do
// not hold are ErrInvalidUsage. now is the heartbeat an active ambient row
// takes.
func ReplaceConversationUsage(
	ctx context.Context,
	db *sql.DB,
	externalClient, externalSessionID string,
	rows []core.UsageBucket,
	now time.Time,
) (bool, error) {
	const op = "store.ReplaceConversationUsage"
	if externalClient == "" || externalSessionID == "" || len(rows) == 0 {
		return false, nil
	}
	if err := validUsageRows(rows); err != nil {
		return false, fmt.Errorf("%s: %w", op, err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("%s: begin: %w", op, err)
	}
	defer func() { _ = tx.Rollback() }()
	chain, err := conversationSessionsTx(ctx, tx, externalClient, externalSessionID)
	if err != nil {
		return false, fmt.Errorf("%s: %w", op, err)
	}
	if len(chain) == 0 {
		return false, nil
	}
	if err := replaceUsageTx(ctx, tx, chain, splitUsage(chain, rows), now); err != nil {
		return false, fmt.Errorf("%s: %w", op, err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("%s: commit: %w", op, err)
	}
	return true, nil
}

// UsageRefresh is what RefreshConversationUsage did.
type UsageRefresh uint8

// The UsageRefresh outcomes.
const (
	// RefreshNoConversation: no ambient row names the conversation (or no
	// rows were given), and nothing is written.
	RefreshNoConversation UsageRefresh = iota
	// RefreshWritten: the conversation's rows are now the rows given.
	RefreshWritten
	// RefreshUnchanged: the ledger already holds exactly the rows given, each
	// session's totals their sum, so nothing is written.
	RefreshUnchanged
	// RefreshBehind: the rows count fewer messages than the conversation's
	// ledger holds -- a read that a later harvest overtook -- so nothing is
	// written.
	RefreshBehind
)

// RefreshConversationUsage is ReplaceConversationUsage for a harvest made
// while the conversation is still going (the Diary's mid-session harvest,
// internal/hooks usage_live.go), whose read may finish after a later harvest
// has written. It writes rows exactly as Replace does -- the same split among
// the conversation's sessions, the same totals, the same heartbeat of an
// active ambient row -- except when they count fewer messages than the
// ledger holds (RefreshBehind) or the ledger holds them already
// (RefreshUnchanged), when it writes nothing. A transcript only grows, and a
// message counts once in it, so a later read never counts fewer messages
// than an earlier one: whichever of two harvests lands last, the ledger never
// moves back. Rows that do not hold are ErrInvalidUsage.
func RefreshConversationUsage(
	ctx context.Context,
	db *sql.DB,
	externalClient, externalSessionID string,
	rows []core.UsageBucket,
	now time.Time,
) (UsageRefresh, error) {
	const op = "store.RefreshConversationUsage"
	if externalClient == "" || externalSessionID == "" || len(rows) == 0 {
		return RefreshNoConversation, nil
	}
	if err := validUsageRows(rows); err != nil {
		return RefreshNoConversation, fmt.Errorf("%s: %w", op, err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return RefreshNoConversation, fmt.Errorf("%s: begin: %w", op, err)
	}
	defer func() { _ = tx.Rollback() }()
	chain, err := conversationSessionsTx(ctx, tx, externalClient, externalSessionID)
	if err != nil {
		return RefreshNoConversation, fmt.Errorf("%s: %w", op, err)
	}
	if len(chain) == 0 {
		return RefreshNoConversation, nil
	}
	owned := splitUsage(chain, rows)
	holds, held := true, 0
	for i, s := range chain {
		have, err := sessionUsageTx(ctx, tx, s.id)
		if err != nil {
			return RefreshNoConversation, fmt.Errorf("%s: %w", op, err)
		}
		held += usageMessages(have)
		want := slices.Clone(owned[i])
		slices.SortFunc(want, compareUsageRows)
		if !slices.EqualFunc(have, want, sameUsageRow) || s.totals != core.SumUsage(want) {
			holds = false
		}
	}
	switch {
	case holds:
		return RefreshUnchanged, nil
	case usageMessages(rows) < held:
		return RefreshBehind, nil
	}
	if err := replaceUsageTx(ctx, tx, chain, owned, now); err != nil {
		return RefreshNoConversation, fmt.Errorf("%s: %w", op, err)
	}
	if err := tx.Commit(); err != nil {
		return RefreshNoConversation, fmt.Errorf("%s: commit: %w", op, err)
	}
	return RefreshWritten, nil
}

// splitUsage splits a conversation's rows among its sessions (chain, as
// conversationSessionsTx reads it): each row goes to the session that owns its
// bucket (usageOwner).
func splitUsage(chain []usageSession, rows []core.UsageBucket) [][]core.UsageBucket {
	owned := make([][]core.UsageBucket, len(chain))
	for _, r := range rows {
		i := usageOwner(chain, r.Bucket)
		owned[i] = append(owned[i], r)
	}
	return owned
}

// replaceUsageTx gives each session of chain its owned rows (splitUsage) in
// place of the rows it holds and sets its totals to their sum; the ambient
// row, chain's first, takes now as its heartbeat while it is active.
func replaceUsageTx(ctx context.Context, tx *sql.Tx, chain []usageSession, owned [][]core.UsageBucket, now time.Time) error {
	for i, s := range chain {
		if _, err := tx.ExecContext(ctx, `DELETE FROM session_usage WHERE session_id = ?`, s.id); err != nil {
			return fmt.Errorf("clear session %s: %w", s.id, err)
		}
		for _, r := range owned[i] {
			if err := insertUsageTx(ctx, tx, s.id, r); err != nil {
				return err
			}
		}
		if err := setUsageTotalsTx(ctx, tx, s.id, i == 0, now); err != nil {
			return err
		}
	}
	return nil
}

// usageMessages is the number of messages rows count.
func usageMessages(rows []core.UsageBucket) int {
	n := 0
	for _, r := range rows {
		n += r.Messages
	}
	return n
}

// UsageCredit is one harvest that reads a running total instead of buckets:
// Codex's cumulative counter at a Stop, or a client that pushed token totals
// and no buckets (an older `seam`).
type UsageCredit struct {
	// Cumulative is the conversation's whole token usage now, as its client
	// counts it.
	Cumulative core.TokenUsage
	// Model is the model the growth is credited to; "" credits it to the
	// ambient row's own model.
	Model string
	// At is when the growth is credited: the bucket it lands in. The zero time
	// is now.
	At time.Time
	// Turn counts the growth as one model turn that ended at At: the row gains
	// one message and At's minute. A credit that is no turn (a pushed total)
	// adds counts only.
	Turn bool
}

// CreditConversationUsage credits what the conversation's cumulative usage
// grew by since its ledger last counted it -- the cumulative total, less the
// ambient row's ledger sum and the totals the sessions its moves ended keep --
// to the ambient row's bucket of c.At, and sets that row's token totals to the
// sum of its rows, in one transaction. A cumulative total read again writes
// nothing; one that shrank in any count writes nothing either (the counter is
// not the one the ledger has been counting), and the ledger keeps what it has.
//
// It reports whether a credit was written. The ambient row may have ended (see
// the file comment); an incomplete identity, an empty total, or no ambient row
// writes nothing. A negative count is ErrInvalidUsage.
func CreditConversationUsage(ctx context.Context, db *sql.DB, externalClient, externalSessionID string, c UsageCredit) (bool, error) {
	const op = "store.CreditConversationUsage"
	if externalClient == "" || externalSessionID == "" || c.Cumulative.Empty() {
		return false, nil
	}
	if t := c.Cumulative; t.Input < 0 || t.Cached < 0 || t.CacheCreation < 0 || t.Output < 0 {
		return false, fmt.Errorf("%s: %w: a negative count", op, ErrInvalidUsage)
	}
	at := c.At
	if at.IsZero() {
		at = time.Now()
	}
	at = at.UTC()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("%s: begin: %w", op, err)
	}
	defer func() { _ = tx.Rollback() }()
	chain, err := conversationSessionsTx(ctx, tx, externalClient, externalSessionID)
	if err != nil {
		return false, fmt.Errorf("%s: %w", op, err)
	}
	if len(chain) == 0 {
		return false, nil
	}
	amb := chain[0]
	counted, err := ledgerSumTx(ctx, tx, amb.id)
	if err != nil {
		return false, fmt.Errorf("%s: %w", op, err)
	}
	for _, s := range chain[1:] {
		counted = addUsage(counted, s.totals)
	}
	grew, ok := usageGrowth(c.Cumulative, counted)
	if !ok {
		return false, nil
	}
	row := core.UsageBucket{
		Bucket: core.UsageBucketStart(at), Model: cmp.Or(c.Model, amb.model), Thread: core.UsageThreadMain,
		Input: grew.Input, Cached: grew.Cached, CacheCreation: grew.CacheCreation, Output: grew.Output,
	}
	if c.Turn {
		row.Messages, row.MinuteMask = 1, core.UsageMinuteBit(at)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO session_usage (`+usageCols+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (session_id, bucket, model, thread) DO UPDATE SET
		    input_tokens = input_tokens + excluded.input_tokens,
		    cached_input_tokens = cached_input_tokens + excluded.cached_input_tokens,
		    cache_creation_tokens = cache_creation_tokens + excluded.cache_creation_tokens,
		    output_tokens = output_tokens + excluded.output_tokens,
		    messages = messages + excluded.messages,
		    minute_mask = minute_mask | excluded.minute_mask`,
		amb.id, core.FormatTime(row.Bucket), row.Model, string(row.Thread), row.Input, row.Cached,
		row.CacheCreation, row.Output, row.Messages, row.MinuteMask); err != nil {
		return false, fmt.Errorf("%s: credit session %s: %w", op, amb.id, err)
	}
	if err := setUsageTotalsTx(ctx, tx, amb.id, true, at); err != nil {
		return false, fmt.Errorf("%s: %w", op, err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("%s: commit: %w", op, err)
	}
	return true, nil
}

// usageGrowth is cumulative less counted, field by field, its total the sum
// of its parts. ok is false when nothing grew, or when any count shrank.
func usageGrowth(cumulative, counted core.TokenUsage) (core.TokenUsage, bool) {
	g := core.TokenUsage{
		Input: cumulative.Input - counted.Input, Cached: cumulative.Cached - counted.Cached,
		CacheCreation: cumulative.CacheCreation - counted.CacheCreation, Output: cumulative.Output - counted.Output,
	}
	if g.Input < 0 || g.Cached < 0 || g.CacheCreation < 0 || g.Output < 0 {
		return core.TokenUsage{}, false
	}
	g.Normalize()
	return g, g.Total > 0
}

// addUsage is a plus b, field by field, its total the sum of its parts.
func addUsage(a, b core.TokenUsage) core.TokenUsage {
	out := core.TokenUsage{
		Input: a.Input + b.Input, Cached: a.Cached + b.Cached,
		CacheCreation: a.CacheCreation + b.CacheCreation, Output: a.Output + b.Output,
	}
	out.Normalize()
	return out
}

// usageSession is one session of a conversation as the ledger writers read
// it.
type usageSession struct {
	id        string
	parent    string
	createdAt time.Time
	model     string
	totals    core.TokenUsage
}

// conversationSessionsTx returns the conversation's sessions: its ambient row
// first, whatever its status, then each session a move ended, nearest first;
// none when it has no ambient row. A chain a damaged row loops is followed
// until it repeats, and one whose parent is no longer here (a merge skipped
// it) ends there.
func conversationSessionsTx(ctx context.Context, tx *sql.Tx, externalClient, externalSessionID string) ([]usageSession, error) {
	const cols = `id, COALESCE(parent_session_id, ''), created_at, model,
		input_tokens, cached_input_tokens, cache_creation_tokens, output_tokens, total_tokens FROM sessions`
	start, found, err := usageSessionTx(ctx, tx, `SELECT `+cols+`
		WHERE ambient = 1 AND external_client = ? AND claude_session_id = ?`, externalClient, externalSessionID)
	if err != nil || !found {
		return nil, err
	}
	out := []usageSession{start}
	seen := map[string]bool{start.id: true}
	for parent := start.parent; parent != "" && !seen[parent]; {
		s, found, err := usageSessionTx(ctx, tx, `SELECT `+cols+` WHERE id = ?`, parent)
		if err != nil {
			return nil, err
		}
		if !found {
			break
		}
		seen[parent] = true
		out = append(out, s)
		parent = s.parent
	}
	return out, nil
}

// usageSessionTx reads the one usageSession query names.
func usageSessionTx(ctx context.Context, tx *sql.Tx, query string, args ...any) (usageSession, bool, error) {
	var (
		s       usageSession
		created string
	)
	err := tx.QueryRowContext(ctx, query, args...).Scan(&s.id, &s.parent, &created, &s.model,
		&s.totals.Input, &s.totals.Cached, &s.totals.CacheCreation, &s.totals.Output, &s.totals.Total)
	if errors.Is(err, sql.ErrNoRows) {
		return usageSession{}, false, nil
	}
	if err != nil {
		return usageSession{}, false, fmt.Errorf("a session of the conversation: %w", err)
	}
	if s.createdAt, err = core.ParseTime(created); err != nil {
		return usageSession{}, false, fmt.Errorf("session %s: %w", s.id, err)
	}
	return s, true, nil
}

// usageOwner is the index in chain (a conversation's sessions, its ambient row
// first: conversationSessionsTx) of the session the bucket starting at bucket
// belongs to: the latest one that began before the bucket ended. The
// conversation's first session (chain's last) takes every bucket before the
// first move.
func usageOwner(chain []usageSession, bucket time.Time) int {
	end := bucket.Add(core.UsageBucketWidth)
	for i := range len(chain) - 1 {
		if chain[i].createdAt.Before(end) {
			return i
		}
	}
	return len(chain) - 1
}

// ledgerSumTx is a session's ledger rows summed.
func ledgerSumTx(ctx context.Context, tx *sql.Tx, sessionID string) (core.TokenUsage, error) {
	var t core.TokenUsage
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(input_tokens), 0), COALESCE(SUM(cached_input_tokens), 0),
		COALESCE(SUM(cache_creation_tokens), 0), COALESCE(SUM(output_tokens), 0)
		FROM session_usage WHERE session_id = ?`, sessionID).
		Scan(&t.Input, &t.Cached, &t.CacheCreation, &t.Output); err != nil {
		return core.TokenUsage{}, fmt.Errorf("the ledger of session %s: %w", sessionID, err)
	}
	t.Normalize()
	return t, nil
}

// insertUsageTx inserts one ledger row of sessionID.
func insertUsageTx(ctx context.Context, tx *sql.Tx, sessionID string, r core.UsageBucket) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO session_usage (`+usageCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sessionID, core.FormatTime(r.Bucket), r.Model, string(r.Thread), r.Input, r.Cached, r.CacheCreation,
		r.Output, r.Messages, r.MinuteMask); err != nil {
		return fmt.Errorf("a ledger row of session %s: %w", sessionID, err)
	}
	return nil
}

// setUsageTotalsTx sets a session's token totals to the sum of its ledger
// rows. heartbeat moves its updated_at to now as well, only while it is
// active: an ended session keeps the time it ended at.
func setUsageTotalsTx(ctx context.Context, tx *sql.Tx, sessionID string, heartbeat bool, now time.Time) error {
	sum, err := ledgerSumTx(ctx, tx, sessionID)
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE sessions
		   SET input_tokens = ?, cached_input_tokens = ?, cache_creation_tokens = ?, output_tokens = ?, total_tokens = ?,
		       updated_at = CASE WHEN ? = 1 AND status = 'active' THEN ? ELSE updated_at END
		 WHERE id = ?`,
		sum.Input, sum.Cached, sum.CacheCreation, sum.Output, sum.Total, boolToInt(heartbeat), core.FormatTime(now.UTC()), sessionID)
	if err != nil {
		return fmt.Errorf("the totals of session %s: %w", sessionID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("the totals of session %s: rows affected: %w", sessionID, err)
	}
	if n == 0 {
		return fmt.Errorf("the totals of session %s: %w", sessionID, ErrSessionNotFound)
	}
	return nil
}

// validUsageRows checks the rows a write is handed (see ErrInvalidUsage).
func validUsageRows(rows []core.UsageBucket) error {
	type key struct {
		bucket int64
		model  string
		thread core.UsageThread
	}
	seen := make(map[key]bool, len(rows))
	for _, r := range rows {
		switch {
		case !r.Aligned():
			return fmt.Errorf("%w: bucket %s is not a quarter-hour's start", ErrInvalidUsage, core.FormatTime(r.Bucket))
		case !r.Thread.Valid():
			return fmt.Errorf("%w: thread %q: valid values are %s", ErrInvalidUsage, r.Thread, usageThreadList())
		case r.Input < 0 || r.Cached < 0 || r.CacheCreation < 0 || r.Output < 0 || r.Messages < 0:
			return fmt.Errorf("%w: a negative count", ErrInvalidUsage)
		case r.MinuteMask < 0 || r.MinuteMask > core.UsageMinuteMaskAll:
			return fmt.Errorf("%w: minute mask %d is past the bucket's %d minutes", ErrInvalidUsage, r.MinuteMask, core.UsageMinutes)
		}
		k := key{r.Bucket.Unix(), r.Model, r.Thread}
		if seen[k] {
			return fmt.Errorf("%w: two rows for bucket %s, model %q, thread %s", ErrInvalidUsage,
				core.FormatTime(r.Bucket), r.Model, r.Thread)
		}
		seen[k] = true
	}
	return nil
}

// usageThreadList names the valid threads, for an error.
func usageThreadList() string {
	names := make([]string, len(core.UsageThreads))
	for i, t := range core.UsageThreads {
		names[i] = string(t)
	}
	return strings.Join(names, ", ")
}
