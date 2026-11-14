package mcp

// Receipts at the tool surface (isolation.requests, owner decision D5; plan
// fenced-sessions P8). Once the owner moves a conversation into another
// project, the clearance rule refuses every write into the home it left, so a
// step its pre-move session claimed there would stay in progress for good. A
// receipt closes it: tasks_update with status done or dropped on a task whose
// claim (live or lapsed) a session the conversation left by a move still
// holds, carrying nothing else but, optionally, one reference to an item in
// its home (reference=<home>:<item id>).
//
// The class is the store's (store.JudgeReceipt decides, store.SendReceipt
// writes); this file names it for a call (receiptOf) and records what was
// sent. Off (Config.Grants nil) nothing here runs: tasks_update is registered
// without the reference (tasksUpdateToolFor) and every call is judged as it
// always was.

import (
	"context"
	"errors"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/screening"
	"github.com/arctop/seamless/internal/store"
)

// receiptRefParam is tasks_update's receipt reference, and the key its
// answer and its task.transition record carry it under.
const receiptRefParam = "reference"

// receiptKey marks a tasks_update answer, and its task.transition record, as
// a receipt's.
const receiptKey = "receipt"

// receiptRefArgDesc documents the reference. The catalog keeps it, so the
// docs site's tool reference lists it; a server registers it only while
// isolation.requests is on (tasksUpdateToolFor).
const receiptRefArgDesc = "A receipt's one reference, to an item in this conversation's home, written <project>:<id> " +
	"-- the home's slug and the id of a memory, note, task, trial or session there. Once the owner has moved this " +
	"conversation into another project, it closes a task it claimed before the move with a receipt: tasks_update with " +
	"status done or dropped and nothing else, plus, optionally, this reference, which the task's history records. A " +
	"receipt carries no title, body, project, dependencies or commits, and a conversation whose home is sealed sends " +
	"none. Offered only while isolation.requests is on"

// tasksUpdateToolFor is tasks_update as this server registers it: the
// catalog's tool without the commits argument while commit records are off
// (offerCommits), and without the receipt reference while isolation.requests
// is off, so a default server's schema is the one it always had.
func (s *Server) tasksUpdateToolFor() mcp.Tool {
	t := s.offerCommits(tasksUpdateTool())
	if s.cfg.Grants == nil {
		return withoutParam(t, receiptRefParam)
	}
	return t
}

// receiptRefArg reads the reference: the zero ReceiptRef when absent, else
// the parsed one, or the refusal of a value of any other shape. It runs
// before the call changes anything.
func (s *Server) receiptRefArg(req mcp.CallToolRequest) (store.ReceiptRef, error) {
	if !argPresent(req, receiptRefParam) {
		return store.ReceiptRef{}, nil
	}
	if s.cfg.Grants == nil {
		// Unreachable: off, the registered schema has no reference, so the
		// validator refused the call. Kept so no handler ever sends one.
		return store.ReceiptRef{}, errors.New(`unknown parameter "reference": this server keeps no receipts`)
	}
	return store.ParseReceiptRef(argString(req, receiptRefParam))
}

// receiptOf names the receipt class for a tasks_update call on task current:
// with isolation.requests on, a call that names a reference is a receipt, and
// so is one that sets status done or dropped on a task whose claim a session
// the conversation left by a move holds (store.ClaimedBeforeMove). Every
// other call is judged as it always was. c is the conversation the call
// speaks for, as grantsMiddleware judged it.
func (s *Server) receiptOf(ctx context.Context, current core.Task, status *core.TaskStatus, ref store.ReceiptRef) (conversation, bool, error) {
	if s.cfg.Grants == nil {
		return conversation{}, false, nil
	}
	c, ok := conversationOf(ctx)
	if !ok {
		return conversation{}, false, nil
	}
	if !ref.IsZero() {
		return c, true, nil
	}
	if status == nil || (*status != core.TaskDone && *status != core.TaskDropped) {
		return c, false, nil
	}
	claimed, err := store.ClaimedBeforeMove(ctx, s.cfg.DB, c.ref, current)
	if err != nil {
		return c, false, err
	}
	return c, claimed, nil
}

// handleReceipt sends the receipt a tasks_update call is (receiptOf) for the
// conversation c, on task current. The store decides it and writes it as the
// pre-move session that claimed the task (store.SendReceipt), which releases
// the claim; any other field the call set is refused with the rule rather
// than dropped. The task's task.transition record is filed under the task's
// project and that session, as the step's earlier transitions are, and
// carries the receipt and its reference: nothing of the home but the one
// reference crosses into the project the task is in.
func (s *Server) handleReceipt(ctx context.Context, req mcp.CallToolRequest, c conversation, current core.Task,
	status *core.TaskStatus, ref store.ReceiptRef) (*mcp.CallToolResult, error) {
	// A project the request's View closes is refused at the View, in the
	// words, and with the request, any write there gets (the store refuses
	// it too, should the fence ever let it by).
	if !access.Writable(ctx, current.ProjectSlug) {
		if err := s.fenceWrite(ctx, current.ProjectSlug); err != nil {
			return errResult("tasks_update", err)
		}
	}
	// An identity the call names must name a session, as on every update; the
	// receipt itself is written as the session whose claim it closes.
	if _, _, err := s.resolveActor(ctx, req); err != nil && !errors.Is(err, errAmbiguousActor) {
		return errResult("tasks_update", err)
	}
	r := store.Receipt{TaskID: current.ID, Ref: ref, Other: receiptOther(req)}
	if status != nil {
		r.Status = *status
	}
	sent, err := store.SendReceipt(ctx, s.cfg.DB, c.ref, r, time.Now().UTC())
	if err != nil {
		return errResult("tasks_update", s.receiptRefusal(ctx, current.ProjectSlug, err))
	}
	updated := sent.Update.Task
	transition := map[string]any{"to": string(updated.Status), "from": string(sent.Update.From), receiptKey: true}
	if !ref.IsZero() {
		transition[receiptRefParam] = ref.String()
	}
	s.record(ctx, core.EventTaskTransition, sent.Holder, updated.ProjectSlug, updated.ID, transition)
	out := s.withholdTask(ctx, updated.ProjectSlug, updated)
	out[receiptKey] = true
	if !ref.IsZero() {
		out[receiptRefParam] = ref.String()
	}
	return jsonResult(out)
}

// receiptOther names the tasks_update arguments a call set that a receipt
// carries none of, in the order the schema lists them.
func receiptOther(req mcp.CallToolRequest) []string {
	var out []string
	for _, name := range []string{"title", "body", "project", "add_depends_on", removeDependsOnParam, commitsParam} {
		if argPresent(req, name) {
			out = append(out, name)
		}
	}
	return out
}

// receiptRefusal is the refusal of a receipt on a task in project. Where the
// clearance rule refuses project, the receipt was the one way past it, so its
// refusal is the write across a fence it would otherwise have been, and is
// recorded as one (access.refused, with write screening on), as the refusal of
// the ordinary update would have been. Any other error comes back unchanged.
func (s *Server) receiptRefusal(ctx context.Context, project string, err error) error {
	if !errors.Is(err, store.ErrReceiptRefused) || access.WriteCleared(ctx, project) {
		return err
	}
	f := &fenceRefusal{reason: screening.RefusedFenceWrite, err: err}
	s.noteRefusal(ctx, f, project)
	return f
}
