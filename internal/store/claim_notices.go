package store

// Claim notices (migration 046): a session that lost a claim learns it once,
// on its next hook injection or Seamless tool result.
//
// A claim is lost three ways, and each records its notice in the transaction
// that takes the claim away, so a notice says exactly what happened: another
// session claims the task after its lease lapsed (ClaimTask's reclaim), the
// owner releases it (ForceReleaseTask), or the reaper releases it from a
// session that went idle once the lease had lapsed (ReleaseReapedClaims). A
// repo lock's hold is lost the same three ways (AcquireRepoLock's take of a
// lapsed hold, ForceReleaseRepoLock, ReleaseReapedRepoLocks). A holder that
// releases or closes its own task, releases its own lock, or ends its own
// session, loses nothing it does not know about, and is told nothing.
//
// Delivery claims the rows before anything renders (DeliverClaimNotices): the
// row's delivered_at is the claim, as access_moves.briefed_at is for a move's
// briefing (ClaimMoveBriefing), so "once" holds across a daemon restart and two
// concurrent deliveries cannot both carry a notice.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
)

// ClaimResourceTask is the kind of a task claim (tasks_claim): its notice's
// resource id is the task's (ClaimNotice.Kind).
const ClaimResourceTask = "task"

// ClaimResourceLock is the kind of a repo lock's hold (locks_acquire): its
// notice's resource id is the lock's, its label the lock's name, and it has no
// project.
const ClaimResourceLock = "lock"

// How a claim was lost (ClaimNotice.Reason).
const (
	// ClaimLostReclaimed: another session claimed it after its lease lapsed.
	ClaimLostReclaimed = "reclaimed"
	// ClaimLostReleased: the owner released it (the console's release button,
	// `seam task release --force`).
	ClaimLostReleased = "released"
	// ClaimLostReaped: the reaper released it when the session went idle after
	// its lease lapsed.
	ClaimLostReaped = "reaped"
)

// claimResourceKinds and claimLostReasons are the kinds and reasons
// RecordClaimNotice accepts.
var (
	claimResourceKinds = []string{ClaimResourceTask, ClaimResourceLock}
	claimLostReasons   = []string{ClaimLostReclaimed, ClaimLostReleased, ClaimLostReaped}
)

// ErrInvalidClaimNotice is a notice RecordClaimNotice refuses: one nobody could
// read, or one that could not say what happened.
var ErrInvalidClaimNotice = errors.New("store: invalid claim notice")

// ClaimNotice is one lost claim, as recorded and as delivered.
type ClaimNotice struct {
	ID string
	// SessionID is the session that held the claim: the one the notice is for.
	SessionID string
	// Kind and ResourceID name what the claim was on (ClaimResourceTask and the
	// task's id).
	Kind, ResourceID string
	// Label is a short identifier the holder knew the resource by beside its
	// id, "" when the id is its whole name (a task). Never free text: the
	// notice renders it as it is.
	Label string
	// Project is the resource's project, "" the global scope.
	Project string
	// NewHolder is the session that holds the resource now, "" when none does.
	NewHolder string
	// Reason is how the claim was lost (ClaimLost*).
	Reason string
	// LeaseExpiresAt is the lost claim's lease: when it lapsed, or would have.
	// nil when the claim carried none.
	LeaseExpiresAt *time.Time
	// LostAt is when the claim was lost.
	LostAt time.Time
	// DeliveredAt is when a hook injection or tool result carried the notice,
	// nil until one did.
	DeliveredAt *time.Time
}

// claimNoticeExecutor is the write subset shared by *sql.DB and *sql.Tx, so a
// notice lands inside the transaction that loses the claim.
type claimNoticeExecutor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// RecordClaimNotice records that n.SessionID lost the claim n names, for
// DeliverClaimNotices to carry to it once. Call it inside the transaction that
// takes the claim away (ex is that *sql.Tx), so the notice exists exactly when
// the loss does. It mints the id when n.ID is empty. A notice without a
// session, a known kind, a resource id, a known reason or the time of the loss
// is refused (ErrInvalidClaimNotice).
func RecordClaimNotice(ctx context.Context, ex claimNoticeExecutor, n ClaimNotice) error {
	const op = "store.RecordClaimNotice"
	switch {
	case strings.TrimSpace(n.SessionID) == "":
		return fmt.Errorf("%s: %w: no session", op, ErrInvalidClaimNotice)
	case !slices.Contains(claimResourceKinds, n.Kind):
		return fmt.Errorf("%s: %w: resource kind %q: valid values are %s", op, ErrInvalidClaimNotice,
			n.Kind, strings.Join(claimResourceKinds, ", "))
	case strings.TrimSpace(n.ResourceID) == "":
		return fmt.Errorf("%s: %w: no resource id", op, ErrInvalidClaimNotice)
	case !slices.Contains(claimLostReasons, n.Reason):
		return fmt.Errorf("%s: %w: reason %q: valid values are %s", op, ErrInvalidClaimNotice,
			n.Reason, strings.Join(claimLostReasons, ", "))
	case n.LostAt.IsZero():
		return fmt.Errorf("%s: %w: no time of loss", op, ErrInvalidClaimNotice)
	}
	if n.ID == "" {
		id, err := core.NewID()
		if err != nil {
			return fmt.Errorf("%s: %w", op, err)
		}
		n.ID = id
	}
	var lease any // NULL: the claim carried no lease
	if n.LeaseExpiresAt != nil {
		lease = core.FormatTime(*n.LeaseExpiresAt)
	}
	if _, err := ex.ExecContext(ctx, `INSERT INTO claim_notices
		(id, session_id, resource_kind, resource_id, resource_label, project_slug, new_holder, reason,
		 lease_expires_at, lost_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		n.ID, n.SessionID, n.Kind, n.ResourceID, n.Label, n.Project, n.NewHolder, n.Reason,
		lease, core.FormatTime(n.LostAt)); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	return nil
}

const claimNoticeCols = `id, session_id, resource_kind, resource_id, resource_label, project_slug,
	new_holder, reason, lease_expires_at, lost_at, delivered_at`

// DeliverClaimNotices claims every notice pending for the given sessions and
// returns them, oldest loss first: the caller carries each to its holder now,
// and no later call returns it again. The claim is the row's delivered_at, set
// to now in one statement, so of two concurrent calls one gets each notice,
// and a daemon restart does not deliver one twice. It commits only once every
// claimed row is read: a failure claims nothing. No sessions, nothing claimed.
//
// Every tool call and every prompt asks, and almost every answer is "none", so
// an indexed read (idx_claim_notices_pending) answers that first: only a
// session with a notice waiting takes the write lock.
func DeliverClaimNotices(ctx context.Context, db *sql.DB, sessionIDs []string, now time.Time) ([]ClaimNotice, error) {
	const op = "store.DeliverClaimNotices"
	var ids []any
	for _, id := range sessionIDs {
		if id != "" && !slices.Contains(ids, any(id)) {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	in := `session_id IN (` + placeholders(len(ids)) + `) AND delivered_at IS NULL`
	var waiting int
	err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM claim_notices WHERE `+in+`)`, ids...).Scan(&waiting)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	if waiting == 0 {
		return nil, nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("%s: begin: %w", op, err)
	}
	defer func() { _ = tx.Rollback() }()
	args := append([]any{core.FormatTime(now)}, ids...)
	rows, err := tx.QueryContext(ctx, `UPDATE claim_notices SET delivered_at = ? WHERE `+in+`
		 RETURNING `+claimNoticeCols, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	notices, err := scanClaimNotices(rows)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("%s: commit: %w", op, err)
	}
	slices.SortFunc(notices, func(a, b ClaimNotice) int {
		if c := a.LostAt.Compare(b.LostAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return notices, nil
}

// PendingClaimNotices returns the notices waiting for sessionID, oldest loss
// first, without claiming them.
func PendingClaimNotices(ctx context.Context, db *sql.DB, sessionID string) ([]ClaimNotice, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+claimNoticeCols+` FROM claim_notices
		WHERE session_id = ? AND delivered_at IS NULL ORDER BY lost_at, id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("store.PendingClaimNotices: %w", err)
	}
	notices, err := scanClaimNotices(rows)
	if err != nil {
		return nil, fmt.Errorf("store.PendingClaimNotices: %w", err)
	}
	return notices, nil
}

// scanClaimNotices reads every row a claimNoticeCols query returns, closing
// them.
func scanClaimNotices(rows *sql.Rows) ([]ClaimNotice, error) {
	defer func() { _ = rows.Close() }()
	var out []ClaimNotice
	for rows.Next() {
		var (
			n                ClaimNotice
			lease, delivered sql.NullString
			lost             string
			err              error
		)
		if err := rows.Scan(&n.ID, &n.SessionID, &n.Kind, &n.ResourceID, &n.Label, &n.Project,
			&n.NewHolder, &n.Reason, &lease, &lost, &delivered); err != nil {
			return nil, err
		}
		if n.LostAt, err = core.ParseTime(lost); err != nil {
			return nil, fmt.Errorf("claim notice %s lost_at: %w", n.ID, err)
		}
		for _, f := range []struct {
			raw sql.NullString
			dst **time.Time
		}{{lease, &n.LeaseExpiresAt}, {delivered, &n.DeliveredAt}} {
			if !f.raw.Valid {
				continue
			}
			t, err := core.ParseTime(f.raw.String)
			if err != nil {
				return nil, fmt.Errorf("claim notice %s: %w", n.ID, err)
			}
			*f.dst = &t
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ClaimNoticeTexts renders notices for the request in ctx (ClaimNotice.Text).
// The session that holds a claim now is named only when this request may see
// it: a session its own principal started (access.SessionAccountOK), in a
// project it may see (access.Visible). Any other new holder is "another
// session", as is one this read cannot find: a less specific notice, never a
// wrong one.
func ClaimNoticeTexts(ctx context.Context, db *sql.DB, notices []ClaimNotice) []string {
	out := make([]string, 0, len(notices))
	for _, n := range notices {
		out = append(out, n.Text(claimNoticeHolderName(ctx, db, n.NewHolder)))
	}
	return out
}

// claimNoticeHolderName is the name the request in ctx may know session id by,
// "" when it may not see it or there is none to find.
func claimNoticeHolderName(ctx context.Context, db *sql.DB, id string) string {
	if id == "" {
		return ""
	}
	sess, found, err := SessionByID(ctx, db, id)
	if err != nil || !found {
		return ""
	}
	if !access.SessionAccountOK(ctx, sess.Account) || !access.Visible(ctx, sess.ProjectSlug) {
		return ""
	}
	if sess.Name == "" {
		return sess.ID
	}
	return sess.Name
}

// Text is the notice as its holder reads it: what the claim was on, how and
// when it was lost and to whom, and what to do now. newHolder is the name to
// give the session that holds the resource now; "" says "another session"
// (ClaimNoticeTexts decides which this reader may see). It carries identifiers
// and times only: no text from the resource itself.
func (n ClaimNotice) Text(newHolder string) string {
	w := n.words()
	what := n.Kind + " " + n.ResourceID
	if n.Kind == ClaimResourceLock {
		what = "repo lock " + n.ResourceID
	}
	if n.Label != "" {
		what += " (" + n.Label + ")"
	}
	switch {
	case n.Kind == ClaimResourceLock:
		// A repo lock is no project's: it guards a repository's refs.
	case n.Project != "":
		what += " in project " + n.Project
	default:
		what += " in the global scope"
	}
	lapsed := "had no lease"
	if n.LeaseExpiresAt != nil {
		lapsed = "lapsed at " + access.Clock(*n.LeaseExpiresAt)
	}
	at := access.Clock(n.LostAt)
	switch n.Reason {
	case ClaimLostReclaimed:
		who := "another session"
		if newHolder != "" {
			who = "session " + newHolder
		}
		return w.title + ": your " + w.hold + " on " + what + " " + lapsed + ", and " + who + " " + w.took + " it at " + at +
			". Pick other work, or " + w.take + " it again" + w.how + " once it is free." + w.keep
	case ClaimLostReleased:
		return w.title + ": the owner released your " + w.hold + " on " + what + " at " + at +
			". " + w.Take + " it again" + w.how + " if it is still free, or pick other work."
	case ClaimLostReaped:
		return w.title + ": your " + w.hold + " on " + what + " " + lapsed + ", and it was released at " + at +
			", when this session had gone idle. " + w.Take + " it again" + w.how + " if it is still free, or pick other work." + w.keep
	}
	return w.title + ": your " + w.hold + " on " + what + " ended at " + at + "."
}

// claimNoticeWords are the words a notice uses for its kind of hold.
type claimNoticeWords struct {
	title, hold, took, take, Take, how, keep string
}

// words picks a notice's words: a repo lock is held and acquired, a task
// claimed.
func (n ClaimNotice) words() claimNoticeWords {
	if n.Kind == ClaimResourceLock {
		return claimNoticeWords{title: "Lock lost", hold: "hold", took: "acquired", take: "acquire", Take: "Acquire",
			how: " with locks_acquire", keep: " To keep a lock through a long landing, acquire it again before its lease lapses."}
	}
	w := claimNoticeWords{title: "Claim lost", hold: "claim", took: "claimed", take: "claim", Take: "Claim",
		keep: " To keep a claim through long work, re-claim it before its lease lapses."}
	if n.Kind == ClaimResourceTask {
		w.how = " with tasks_claim"
	}
	return w
}
