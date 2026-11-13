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
// Receipt.Patch is the only write it allows. The tool that sends one, the
// event that records it and the claim's release are the MCP surface's (plan
// fenced-sessions P8).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

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

// Receipt is one receipt: the task it closes, the status it sets (done or
// dropped), and at most one reference to an item in the home.
type Receipt struct {
	TaskID string
	Status core.TaskStatus
	Ref    ReceiptRef
}

// Patch is the one write a receipt makes: the task's status, and nothing
// else. The reference is the event's to record (P8), never the task's text.
func (r Receipt) Patch() TaskPatch {
	status := r.Status
	return TaskPatch{Status: &status}
}

// JudgeReceipt decides whether the conversation ref names, for the holder ctx
// speaks for (the same holders as ConversationClearance), may write receipt r,
// and returns the task it would close. It allows one exactly when:
//
//   - r sets done or dropped, and its reference is none, or one item id in
//     the conversation's home;
//   - the conversation has a home, and its home is not sealed: a sealed
//     project's conversation sends nothing out, not even a status;
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
	if r.Status != core.TaskDone && r.Status != core.TaskDropped {
		return core.Task{}, refuse(fmt.Sprintf("a receipt sets status done or dropped, not %q", r.Status))
	}
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
	if !r.Ref.IsZero() {
		if r.Ref.Project != c.home {
			return core.Task{}, refuse("a receipt's reference names an item in the conversation's home, " + c.home)
		}
		if _, err := ulid.ParseStrict(r.Ref.ID); err != nil {
			return core.Task{}, refuse("a receipt's reference is one item id")
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
