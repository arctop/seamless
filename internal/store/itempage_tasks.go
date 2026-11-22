package store

// The Tasks list's place query (an item's own page, store/itempage.go).

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
)

// TaskList is a Tasks list's state as its place query reads it
// (plan:console-ux-pass UX 30): the filter, the order, the closed tasks it
// holds, and the tasks it counts as blocked although the request's handle
// reads them as ready.
type TaskList struct {
	Filter TaskFilter
	// Newest orders the open groups newest-created first (the gates on
	// others keep theirs: by who must act).
	Newest bool
	// ClosedSince keeps the closed tasks that closed at or after it (a row
	// with no closed_at by its last update): the list's recently closed.
	// Zero keeps every closed task (closed=all, which the list pages
	// through).
	ClosedSince time.Time
	// LockedBlocked is the open tasks an unfinished task in a locked project
	// blocks (LockedBlockedTasks on the main handle): the request's handle
	// hides those blockers, so they read as ready on it, and the list draws
	// them as blocked.
	LockedBlocked []string
}

// The Tasks list's groups, in its order, as TaskPlace ranks them: Needs you
// (a gate on the owner, a lapsed claim, a claimless start), In progress
// under a live claim, Ready, Blocked (a step only the owner's gates hold
// among them), Waiting on others (the gates on anyone else, then the steps
// an open gate holds), and the closed tasks.
const (
	taskRankOwnerGate = iota
	taskRankLapsed
	taskRankClaimless
	taskRankLive
	taskRankReady
	taskRankBlocked
	taskRankGates
	taskRankGated
	taskRankClosed
)

// sqlGateOnOwner holds for a row of alias whose gate names the owner:
// core.GateWhoMatches(gate_who, "owner"), the whole value or its class
// before a colon, either one trimmed, in any case.
func sqlGateOnOwner(alias string) string {
	const ws = "' ' || char(9, 10, 11, 12, 13)"
	who := alias + ".gate_who"
	return "(lower(trim(" + who + ", " + ws + ")) = 'owner' OR (instr(" + who + ", ':') > 0 AND lower(trim(substr(" +
		who + ", 1, instr(" + who + ", ':') - 1), " + ws + ")) = 'owner'))"
}

// sqlOwnerGateOpen holds for a row of alias that waits on the owner as the
// console's needs model counts it (internal/console ownerGateOpen): a gate
// open or in progress, or an open step whose own gate is open, whose gate
// names the owner. A step an owner's gate blocks is counted through that
// gate.
func sqlOwnerGateOpen(alias string) string {
	return "(((" + alias + ".kind = 'gate' AND " + alias + ".status IN ('open','in_progress')) OR (" +
		alias + ".kind <> 'gate' AND " + alias + ".status = 'open' AND " + sqlOwnGateOpen(alias) + ")) AND " +
		sqlGateOnOwner(alias) + ")"
}

// sqlGateBlockerOnOthers holds for a row of alias that an unfinished gate
// waiting on anyone but the owner blocks. A step of the people's queue
// without one, whose own gate is not open, waits only on the owner's gates:
// the list draws it as blocked by them (internal/console heldByOwnersGates).
func sqlGateBlockerOnOthers(alias string) string {
	return `EXISTS (
		      SELECT 1 FROM task_deps d
		      JOIN tasks b ON b.id = d.depends_on
		      WHERE d.task_id = ` + alias + `.id AND b.status IN ('open','in_progress') AND b.kind = 'gate'
		        AND NOT ` + sqlGateOnOwner("b") + `)`
}

// TaskPlace is where task id sits in the Tasks list l: its groups in the
// list's order (the taskRank constants), each open group oldest-created
// first (newest first with Newest), but the gates on others, which go by
// who must act, and the closed tasks, newest-closed first. The tasks that
// wait on the owner are in the list whether or not it keeps the plan steps
// out, as the list draws them. The list is the whole of what its URL names:
// the page draws the first ready tasks and one page of the closed ones, the
// rest a link away, and the place counts them all. now is the instant a
// claim is judged live at. found is false when the list does not hold the
// task.
func TaskPlace(ctx context.Context, db *sql.DB, l TaskList, now time.Time, id string) (ItemPlace, bool, error) {
	started := "t.status = 'in_progress' AND t.kind <> 'gate'"
	lockedReady := "0"
	if len(l.LockedBlocked) > 0 {
		lockedReady = "t.id IN (" + placeholders(len(l.LockedBlocked)) + ")"
	}
	closed := "t.status IN ('done','dropped')"
	if !l.ClosedSince.IsZero() {
		closed += " AND COALESCE(t.closed_at, t.updated_at) >= ?"
	}
	// The binds follow the text: the live claim's instant, the locked
	// blocked ids, the closed window's start, then the filter's.
	rank := fmt.Sprintf(`CASE
		WHEN %s THEN %d
		WHEN %s AND t.claimed_by <> '' AND NOT %s THEN %d
		WHEN %s AND t.claimed_by = '' THEN %d
		WHEN %s THEN %d
		WHEN %s AND NOT %s THEN %d
		WHEN %s OR %s THEN %d
		WHEN %s THEN %d
		WHEN %s AND NOT %s AND NOT %s THEN %d
		WHEN %s THEN %d
		WHEN %s THEN %d
	END`,
		sqlOwnerGateOpen("t"), taskRankOwnerGate,
		started, sqlClaimLive("t"), taskRankLapsed,
		started, taskRankClaimless,
		started, taskRankLive,
		sqlReady("t"), lockedReady, taskRankReady,
		sqlReady("t"), sqlBlocked("t"), taskRankBlocked,
		sqlGates("t"), taskRankGates,
		sqlGated("t"), sqlOwnGateOpen("t"), sqlGateBlockerOnOthers("t"), taskRankBlocked,
		sqlGated("t"), taskRankGated,
		closed, taskRankClosed)
	args := []any{core.FormatTime(now.UTC())}
	for _, blocked := range l.LockedBlocked {
		args = append(args, blocked)
	}
	if !l.ClosedSince.IsZero() {
		args = append(args, core.FormatTime(l.ClosedSince.UTC()))
	}
	where, scope := l.Filter.whereScope("t")
	args = append(args, scope...)
	keep := "rk IS NOT NULL"
	if l.Filter.offPlanOnly() {
		keep += fmt.Sprintf(" AND (plan = '' OR rk <= %d)", taskRankClaimless)
	}
	inner := `SELECT id, label, rk, created_at, closed, who FROM (
		SELECT t.id AS id, t.title AS label, ` + rank + ` AS rk, t.created_at AS created_at,
		       COALESCE(t.closed_at, t.updated_at) AS closed, lower(t.gate_who) AS who, t.plan_slug AS plan
		  FROM tasks t
		 WHERE 1 = 1` + where + `
	) WHERE ` + keep
	dir := "ASC"
	if l.Newest {
		dir = "DESC"
	}
	order := strings.NewReplacer("GATES", fmt.Sprint(taskRankGates), "CLOSED", fmt.Sprint(taskRankClosed)).Replace(`rk,
		CASE WHEN rk = GATES THEN who END,
		CASE WHEN rk = CLOSED THEN closed END DESC,
		CASE WHEN rk = CLOSED THEN id END DESC,
		CASE WHEN rk = GATES THEN created_at END,
		CASE WHEN rk = GATES THEN id END,
		created_at ` + dir + `, id ` + dir)
	place, found, err := itemPlace(ctx, db, placeSQL(inner, order), append(args, id), id)
	if err != nil {
		return ItemPlace{}, false, fmt.Errorf("store.TaskPlace: %w", err)
	}
	return place, found, nil
}
