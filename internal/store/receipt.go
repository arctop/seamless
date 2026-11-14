package store

// Receipts (isolation.requests, owner decision D5): the one content-free
// write a moved conversation may make outside what its label clears. An
// orchestrator in an open project claims a step, then moves into a
// confidential project for good; after the move the clearance rule refuses
// every write into the home it left (access.WriteCleared), so without a
// receipt the step it claimed would stay in progress for ever. A receipt
// closes it -- status done or dropped -- and carries at most one reference,
// a project slug and an item id, to an item in its home: no text, so nothing
// the conversation read crosses back.
//
// JudgeReceipt is the class: it decides whether a receipt may be written, and
// Receipt.Patch is the only write it allows. ClaimedBeforeMove names the
// class for a surface (a tasks_update closing a task one of the
// conversation's pre-move sessions claimed is a receipt), and SendReceipt
// judges one and makes its write. The tool that sends one and the event that
// records it are the MCP surface's (plan fenced-sessions P8).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
)

// ErrReceiptRefused is a receipt the class does not allow. The error names
// the rule it broke.
var ErrReceiptRefused = errors.New("receipt refused")

// ReceiptRef names one item in the conversation's home: its project slug and
// its id. It is the only text a receipt carries.
type ReceiptRef struct {
	Project, ID string
}

// IsZero reports that r names nothing.
func (r ReceiptRef) IsZero() bool { return r.Project == "" && r.ID == "" }

// String renders the reference as "<project>:<id>".
func (r ReceiptRef) String() string {
	if r.IsZero() {
		return ""
	}
	return r.Project + ":" + r.ID
}

// ParseReceiptRef reads a reference in the one form String renders,
// "<project>:<id>": the project before the first colon, the item id after
// it, which ULIDs carry in either case (it is kept upper case, as Seamless
// mints them). Whether it names an item in the conversation's home is
// JudgeReceipt's to decide; a value of any other shape is refused here and
// never echoed, since it is the text a receipt may not carry.
func ParseReceiptRef(s string) (ReceiptRef, error) {
	project, id, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok || project == "" || id == "" {
		return ReceiptRef{}, fmt.Errorf("store.ParseReceiptRef: %w: a receipt's reference is one item in the "+
			"conversation's home, written <project>:<id> -- the home's slug and the item's id", ErrReceiptRefused)
	}
	return ReceiptRef{Project: project, ID: strings.ToUpper(id)}, nil
}

// Receipt is one receipt: the task it closes, the status it sets (done or
// dropped), and at most one reference to an item in the home.
type Receipt struct {
	TaskID string
	Status core.TaskStatus
	Ref    ReceiptRef
	// Other names every other field the call that sends it set, in its
	// surface's words (title, body, ...). A receipt carries none of them:
	// JudgeReceipt refuses one that names any, rather than drop them.
	Other []string
}

// Patch is the one write a receipt makes: the task's status, and nothing
// else. The reference is the event's to record, never the task's text.
func (r Receipt) Patch() TaskPatch {
	status := r.Status
	return TaskPatch{Status: &status}
}

// JudgeReceipt decides whether the conversation ref names, for the holder ctx
// speaks for (the same holders as ConversationClearance), may write receipt r,
// and returns the task it would close. It allows one exactly when:
//
//   - the conversation has a home, and its home is not sealed: a sealed
//     project's conversation sends nothing out, not even a status;
//   - r sets done or dropped and nothing else (Other is empty), and its
//     reference is none, or the id of an item in the conversation's home: a
//     memory, a note, a task, a trial or a session there. An id that names
//     nothing is refused, so a reference cannot carry text dressed as one;
//   - the task is reachable to the request's View, its project is not sealed
//     (no receipt opens a sealed destination), and its claim (live or lapsed)
//     is held by a session the conversation left by a move -- the pre-move
//     session claimed it, and nothing else has claimed it since.
//
// Every refusal wraps ErrReceiptRefused and names the rule; a task id that
// names nothing is ErrTaskNotFound.
func JudgeReceipt(ctx context.Context, db *sql.DB, ref ConversationRef, r Receipt) (core.Task, error) {
	const op = "store.JudgeReceipt"
	refuse := func(rule string) error { return fmt.Errorf("%s: %w: %s", op, ErrReceiptRefused, rule) }
	c, ok, err := clearanceHoldersOf(ctx, db, ref)
	if err != nil {
		return core.Task{}, fmt.Errorf("%s: %w", op, err)
	}
	if !ok || c.home == "" {
		return core.Task{}, refuse("receipts are sent from a conversation's home, and this request names none")
	}
	home, err := IsolationOf(ctx, db, c.home)
	if err != nil {
		return core.Task{}, fmt.Errorf("%s: %w", op, err)
	}
	if home == core.IsolationSealed {
		return core.Task{}, refuse("a conversation whose home is sealed project " + c.home + " sends no receipts")
	}
	if r.Status != core.TaskDone && r.Status != core.TaskDropped {
		return core.Task{}, refuse(fmt.Sprintf("a receipt sets status done or dropped, not %q", r.Status))
	}
	if len(r.Other) > 0 {
		verb := "is"
		if len(r.Other) > 1 {
			verb = "are"
		}
		return core.Task{}, refuse(fmt.Sprintf("a receipt carries no text: it sets the status alone, with at most one "+
			"reference to an item in the conversation's home, %s, so %s %s refused", c.home, strings.Join(r.Other, ", "), verb))
	}
	if !r.Ref.IsZero() {
		if r.Ref.Project != c.home {
			return core.Task{}, refuse("a receipt's reference names an item in the conversation's home, " + c.home)
		}
		if _, err := ulid.ParseStrict(r.Ref.ID); err != nil {
			return core.Task{}, refuse("a receipt's reference is one item id")
		}
		named, err := itemOfProject(ctx, db, c.home, r.Ref.ID)
		if err != nil {
			return core.Task{}, fmt.Errorf("%s: %w", op, err)
		}
		if !named {
			return core.Task{}, refuse("a receipt's reference names an item in the conversation's home, " + c.home +
				", and " + r.Ref.ID + " names none there")
		}
	}
	task, err := TaskByID(ctx, db, r.TaskID)
	if err != nil {
		return core.Task{}, fmt.Errorf("%s: %w", op, err)
	}
	if !access.Writable(ctx, task.ProjectSlug) {
		return core.Task{}, fmt.Errorf("%s: %w: %w", op, ErrReceiptRefused, access.ErrNotReachable)
	}
	dest, err := IsolationOf(ctx, db, task.ProjectSlug)
	if err != nil {
		return core.Task{}, fmt.Errorf("%s: %w", op, err)
	}
	if dest == core.IsolationSealed && task.ProjectSlug != c.home {
		return core.Task{}, refuse("no receipt is written into sealed project " + task.ProjectSlug)
	}
	left, err := c.leftSessions(ctx, db)
	if err != nil {
		return core.Task{}, fmt.Errorf("%s: %w", op, err)
	}
	if task.ClaimedBy == "" || !slices.Contains(left, task.ClaimedBy) {
		return core.Task{}, refuse("a receipt closes only a task this conversation claimed before it moved")
	}
	return task, nil
}

// ClaimedBeforeMove reports whether task's claim, live or lapsed, is held by
// a session the conversation ref names left by a move, for the holder ctx
// speaks for (as JudgeReceipt reads them): the tasks a receipt closes. A
// surface asks it to name the class -- a tasks_update closing such a task is
// a receipt -- and JudgeReceipt still decides. A request that names no
// conversation, a conversation that never moved and an unclaimed task hold
// none.
func ClaimedBeforeMove(ctx context.Context, db *sql.DB, ref ConversationRef, task core.Task) (bool, error) {
	const op = "store.ClaimedBeforeMove"
	if task.ClaimedBy == "" {
		return false, nil
	}
	c, ok, err := clearanceHoldersOf(ctx, db, ref)
	if err != nil {
		return false, fmt.Errorf("%s: %w", op, err)
	}
	if !ok {
		return false, nil
	}
	left, err := c.leftSessions(ctx, db)
	if err != nil {
		return false, fmt.Errorf("%s: %w", op, err)
	}
	return slices.Contains(left, task.ClaimedBy), nil
}

// ReceiptSent is what SendReceipt wrote.
type ReceiptSent struct {
	// Update is the write: the task after it, and its status before.
	Update TaskUpdateResult
	// Holder is the pre-move session whose claim the receipt closed. The
	// receipt was written as it, and closing the task released its claim.
	Holder string
}

// SendReceipt judges receipt r for the conversation ref names (JudgeReceipt)
// and, when it is allowed, makes its one write (Receipt.Patch): the status,
// written as the pre-move session that holds the claim -- the holder lock
// UpdateTask applies -- and only while that session still holds it
// (TaskPatch.IfClaimedBy), so a claim taken or released since the judgment
// is not closed. Closing the task releases the claim. The event recording
// the receipt and its reference is the surface's to write.
func SendReceipt(ctx context.Context, db *sql.DB, ref ConversationRef, r Receipt, now time.Time) (ReceiptSent, error) {
	task, err := JudgeReceipt(ctx, db, ref, r)
	if err != nil {
		return ReceiptSent{}, err
	}
	holder := task.ClaimedBy
	patch := r.Patch()
	patch.IfClaimedBy = &holder
	res, err := UpdateTaskDetailed(ctx, db, task.ID, patch, holder, now)
	if err != nil {
		return ReceiptSent{}, err
	}
	return ReceiptSent{Update: res, Holder: holder}, nil
}

// leftSessions returns the sessions c's conversation left by a move: the
// pre-move session of each of its moves.
func (c clearanceHolders) leftSessions(ctx context.Context, db *sql.DB) ([]string, error) {
	var out []string
	for chunk := range slices.Chunk(c.holders, holderMaxLookup) {
		moves, err := MovesForHolder(ctx, db, chunk)
		if err != nil {
			return nil, err
		}
		for _, m := range moves {
			if m.FromSessionID != "" && !slices.Contains(out, m.FromSessionID) {
				out = append(out, m.FromSessionID)
			}
		}
	}
	return out, nil
}

// itemOfProject reports whether id names an item of project: a memory, a
// note, a task, a trial or a session -- the items a conversation's record
// holds.
func itemOfProject(ctx context.Context, db *sql.DB, project, id string) (bool, error) {
	var named bool
	err := db.QueryRowContext(ctx, `SELECT
		   EXISTS (SELECT 1 FROM memories_index WHERE id = ? AND project = ?)
		OR EXISTS (SELECT 1 FROM notes_index WHERE id = ? AND project = ?)
		OR EXISTS (SELECT 1 FROM tasks WHERE id = ? AND project_slug = ?)
		OR EXISTS (SELECT 1 FROM trials WHERE id = ? AND project_slug = ?)
		OR EXISTS (SELECT 1 FROM sessions WHERE id = ? AND project_slug = ?)`,
		id, project, id, project, id, project, id, project, id, project).Scan(&named)
	if err != nil {
		return false, fmt.Errorf("the item a receipt names: %w", err)
	}
	return named, nil
}
