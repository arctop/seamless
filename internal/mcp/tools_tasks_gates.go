package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// Task gates on the MCP surface (store/tasks_gates.go holds the rules): the
// kind and gate_* arguments of tasks_add and tasks_update, gate_clear, the
// gated and gates groups of tasks_ready with its who and agent filters, and
// the claim's gated refusal.

// The gate's argument names.
const (
	taskKindParam       = "kind"
	gateWhoParam        = "gate_who"
	gateWhatParam       = "gate_what"
	gateDecisionParam   = "gate_decision"
	gateReadyWhenParam  = "gate_ready_when"
	gateClearParam      = "gate_clear"
	gateNoteParam       = "gate_note"
	readyWhoParam       = "who"
	readyAgentParam     = "agent"
	gatedKey            = "gated"
	gatesKey            = "gates"
	omittedKey          = "omitted"
	gateTransitionKey   = "gate"
	gateTransitionSet   = "set"
	gateTransitionClear = "cleared"
	gateTransitionGone  = "removed"
)

// gateTextParams are the gate's text arguments, in schema order.
var gateTextParams = []string{gateWhoParam, gateWhatParam, gateDecisionParam, gateReadyWhenParam}

// taskGateFields are the gate keys taskJSON adds only when set. A trimmed
// response (withholdContent) drops them, like taskAccountFields and for the
// same reason they are not in taskContentFields: that list is named back to
// the caller verbatim, and withheld answers must read as they always did.
var taskGateFields = []string{"kind", "gate"}

// gateArgOptions are the gate arguments tasks_add and tasks_update share.
func gateArgOptions() []mcp.ToolOption {
	return []mcp.ToolOption{
		mcp.WithString(taskKindParam, enumOf(core.TaskKinds), mcp.Description(
			"step (the default: work an agent claims and finishes) or gate (a person's action, or an event outside "+
				"Seamless, that the tasks depending on it wait for: never ready and never claimed; gate_who and gate_what "+
				"describe it, and clearing it closes it and unblocks its dependents)")),
		mcp.WithString(gateWhoParam, mcp.Description(
			"who must act before this task can go ahead: owner, founder:<name>, sitting:<name>, hardware, "+
				"sealed:<project>, or free text (one line). With gate_what it gives the task a gate: a step whose gate is "+
				"open is not ready, and tasks_ready lists it under gated")),
		mcp.WithString(gateWhatParam, mcp.Description("what must happen, one line (with gate_who)")),
		mcp.WithString(gateDecisionParam, mcp.Description("optional: the open question the person answers, one line")),
		mcp.WithString(gateReadyWhenParam, mcp.Description("optional: when the task is ready, one line")),
	}
}

// tasksAddGateArgs reads tasks_add's kind and gate: the kind ("" for a step)
// and the gate as sent (the store trims and checks it).
func tasksAddGateArgs(req mcp.CallToolRequest) (core.TaskKind, core.TaskGate) {
	return core.TaskKind(argString(req, taskKindParam)), core.TaskGate{
		Who:       argRaw(req, gateWhoParam),
		What:      argRaw(req, gateWhatParam),
		Decision:  argRaw(req, gateDecisionParam),
		ReadyWhen: argRaw(req, gateReadyWhenParam),
	}
}

// tasksUpdateGateArgs reads tasks_update's kind, gate edits and gate clear
// into patch. The clear's By is filled in once the acting session is known
// (clearedBy). gate_clear=false asks for nothing; gate_note without a clear
// is refused, since a note is the record of a clearing.
func tasksUpdateGateArgs(req mcp.CallToolRequest, patch *store.TaskPatch) error {
	if argPresent(req, taskKindParam) {
		kind := core.TaskKind(argString(req, taskKindParam))
		patch.Kind = &kind
	}
	var edit store.TaskGatePatch
	edited := false
	for _, f := range []struct {
		name string
		dst  **string
	}{{gateWhoParam, &edit.Who}, {gateWhatParam, &edit.What}, {gateDecisionParam, &edit.Decision}, {gateReadyWhenParam, &edit.ReadyWhen}} {
		if argPresent(req, f.name) {
			v := argRaw(req, f.name)
			*f.dst, edited = &v, true
		}
	}
	if edited {
		patch.Gate = &edit
	}
	clearing := argBool(req, gateClearParam, false)
	if argPresent(req, gateNoteParam) && !clearing {
		return errors.New("gate_note is the record of a clearing: pass it with gate_clear=true")
	}
	if clearing {
		if edited {
			return errors.New("edit a gate (gate_who, gate_what, gate_decision, gate_ready_when) or clear it (gate_clear), not both in one call")
		}
		patch.ClearGate = &store.TaskGateClear{Note: argRaw(req, gateNoteParam)}
	}
	return nil
}

// clearedBy names who cleared a gate: the acting session's name, else its
// ULID, else "" (a caller with no session).
func (s *Server) clearedBy(ctx context.Context, actor string) string {
	if actor == "" {
		return ""
	}
	if sess, ok, err := store.SessionByID(ctx, s.cfg.DB, actor); err == nil && ok && sess.Name != "" {
		return sess.Name
	}
	return actor
}

// gateTransition is the task.transition record's word for what an update did
// to the task's gate, or "" when it did nothing to it.
func gateTransition(patch store.TaskPatch, after core.Task) string {
	switch {
	case patch.ClearGate != nil:
		return gateTransitionClear
	case patch.Gate != nil && after.Gate.Set():
		return gateTransitionSet
	case patch.Gate != nil:
		return gateTransitionGone
	}
	return ""
}

// gateJSON renders a task's gate for a tool answer: who and what, the
// optional lines when set, whether it is open, and the clearance once cleared.
func gateJSON(g core.TaskGate) map[string]any {
	j := map[string]any{"who": g.Who, "what": g.What, "open": g.Open()}
	if g.Decision != "" {
		j["decision"] = g.Decision
	}
	if g.ReadyWhen != "" {
		j["ready_when"] = g.ReadyWhen
	}
	if g.ClearedAt != nil {
		j["cleared_at"] = core.FormatTime(*g.ClearedAt)
		if g.ClearedBy != "" {
			j["cleared_by"] = g.ClearedBy
		}
		if g.Note != "" {
			j["note"] = g.Note
		}
	}
	return j
}

// putGate adds the kind and gate keys to a task answer: kind only for a gate
// (a step reads as every task always has), the gate whenever one is set.
func putGate(j map[string]any, t core.Task) {
	if t.IsGate() {
		j["kind"] = string(core.TaskKindGate)
	}
	if t.Gate.Set() {
		j["gate"] = gateJSON(t.Gate)
	}
}

// fenceGatedClaim withholds, from tasks_claim's "task gated" refusal, the
// gate's words when the caller may not read the task's project: who and what
// are the task's content. Any other error comes back unchanged, and a lookup
// that fails withholds them, failing closed as withholdContent does.
func (s *Server) fenceGatedClaim(ctx context.Context, project string, err error) error {
	var gated *store.TaskGatedError
	if !errors.As(err, &gated) {
		return err
	}
	ok, rerr := s.taskReader(ctx)(project)
	if rerr != nil {
		s.logger.Warn("mcp: claim refusal fence", "task", gated.TaskID, "error", rerr)
	}
	if ok && rerr == nil {
		return err
	}
	withheld := *gated
	withheld.Withhold = true
	return &withheld
}

// readyFilter is tasks_ready's choice of queue: everything (the default), the
// steps an agent can work (agent), or the gates and gated steps waiting on one
// person (who).
type readyFilter struct {
	who   string
	agent bool
}

// readyFilterArgs reads tasks_ready's who and agent, refusing a blank who and
// the two together: they ask for different queues.
func readyFilterArgs(req mcp.CallToolRequest) (readyFilter, error) {
	f := readyFilter{who: strings.TrimSpace(argString(req, readyWhoParam)), agent: argBool(req, readyAgentParam, false)}
	if argPresent(req, readyWhoParam) && f.who == "" {
		return readyFilter{}, errors.New("who must not be blank: name the person or class whose queue you want, e.g. owner")
	}
	if f.who != "" && f.agent {
		return readyFilter{}, errors.New("who and agent ask for different queues: who lists the gates and gated tasks " +
			"waiting on that person, agent=true only the tasks an agent can work; pass one")
	}
	return f, nil
}

// readyQueue is what tasks_ready read from the store, before rendering.
type readyQueue struct {
	ready   []core.Task
	blocked []store.BlockedTask
	gated   []store.BlockedTask
	gates   []store.BlockedTask
}

// loadReadyQueue reads a project's default queue, or one plan's steps.
func (s *Server) loadReadyQueue(ctx context.Context, project, plan string) (readyQueue, error) {
	var q readyQueue
	var err error
	if plan != "" {
		if q.ready, err = store.ReadyTasksForPlan(ctx, s.cfg.DB, project, plan); err != nil {
			return q, err
		}
		if q.blocked, err = store.BlockedTasksForPlan(ctx, s.cfg.DB, project, plan); err != nil {
			return q, err
		}
		if q.gated, err = store.GatedTasksForPlan(ctx, s.cfg.DB, project, plan); err != nil {
			return q, err
		}
		q.gates, err = store.GateTasksForPlan(ctx, s.cfg.DB, project, plan)
		return q, err
	}
	if q.ready, err = store.ReadyTasks(ctx, s.cfg.DB, project); err != nil {
		return q, err
	}
	if q.blocked, err = store.BlockedTasks(ctx, s.cfg.DB, project); err != nil {
		return q, err
	}
	if q.gated, err = store.GatedTasks(ctx, s.cfg.DB, project); err != nil {
		return q, err
	}
	q.gates, err = store.GateTasks(ctx, s.cfg.DB, project)
	return q, err
}

// waitsOn reports whether a gated or gate entry waits on the who filter: its
// own gate names that person, or (for a gated step) a gate blocking it the
// caller may read does.
func waitsOn(readable func(string) (bool, error), b store.BlockedTask, who string) (bool, error) {
	if b.Task.Gate.Open() && core.GateWhoMatches(b.Task.Gate.Who, who) {
		return true, nil
	}
	for _, bl := range b.Blockers {
		if !bl.IsGate() || !core.GateWhoMatches(bl.Gate.Who, who) {
			continue
		}
		ok, err := readable(bl.ProjectSlug)
		if err != nil || ok {
			return ok, err
		}
	}
	return false, nil
}

// filterWho keeps the entries waiting on who.
func filterWho(readable func(string) (bool, error), list []store.BlockedTask, who string) ([]store.BlockedTask, error) {
	out := list[:0:0]
	for _, b := range list {
		ok, err := waitsOn(readable, b, who)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, b)
		}
	}
	return out, nil
}

// heldTasksJSON renders gated or gate entries: compact (no body, which
// tasks_list id=<id> loads), with the gate and the unfinished blockers.
func (s *Server) heldTasksJSON(ctx context.Context, readable func(string) (bool, error), list []store.BlockedTask) ([]map[string]any, error) {
	out, err := s.blockedTasksJSON(ctx, readable, list)
	if err != nil {
		return nil, err
	}
	for i := range out {
		delete(out[i], "body")
		if len(list[i].Blockers) == 0 {
			delete(out[i], "blockers")
		}
	}
	return out, nil
}

// blockedTasksJSON renders blocked-style entries: each task with its
// unfinished blockers, a blocker from a project the caller may not read
// withheld in place (so the count stays true).
func (s *Server) blockedTasksJSON(ctx context.Context, readable func(string) (bool, error), list []store.BlockedTask) ([]map[string]any, error) {
	tasks := make([]core.Task, len(list))
	for i, b := range list {
		tasks[i] = b.Task
	}
	out, err := s.tasksJSON(ctx, readable, tasks)
	if err != nil {
		return nil, err
	}
	for i, b := range list {
		blockers := make([]map[string]any, 0, len(b.Blockers))
		for _, bl := range b.Blockers {
			// A blocker from a project the caller may not read is withheld --
			// its title is that project's content, and its id the handle to it
			// -- while the entry itself stays, so the count is true. Only the
			// owner, kept out by a lock alone, is told which locked project.
			ok, err := readable(bl.ProjectSlug)
			if err != nil {
				return nil, err
			}
			if !ok {
				blockers = append(blockers, map[string]any{withheldKey: withheldBlocker(ctx, lockedBlockerProject(ctx, bl.ProjectSlug))})
				continue
			}
			entry := map[string]any{"id": bl.ID, "title": bl.Title, "status": string(bl.Status)}
			putGate(entry, bl)
			blockers = append(blockers, entry)
		}
		out[i]["blockers"] = blockers
	}
	return out, nil
}

// readyAnswer renders tasks_ready's answer for a filter. The default carries
// ready and blocked as it always has, plus gated and gates when either holds
// anything; agent carries ready alone with the other groups' sizes under
// omitted; who carries the gates and gated tasks waiting on that person.
func (s *Server) readyAnswer(ctx context.Context, project string, q readyQueue, f readyFilter) (map[string]any, error) {
	readable := s.taskReader(ctx)
	out := map[string]any{"project": project}
	if f.who != "" {
		gates, err := filterWho(readable, q.gates, f.who)
		if err != nil {
			return nil, err
		}
		gated, err := filterWho(readable, q.gated, f.who)
		if err != nil {
			return nil, err
		}
		if out[gatesKey], err = s.heldTasksJSON(ctx, readable, gates); err != nil {
			return nil, err
		}
		if out[gatedKey], err = s.heldTasksJSON(ctx, readable, gated); err != nil {
			return nil, err
		}
		out[readyWhoParam] = f.who
		return out, nil
	}
	ready, err := s.tasksJSON(ctx, readable, q.ready)
	if err != nil {
		return nil, err
	}
	out["ready"] = ready
	if f.agent {
		out[omittedKey] = map[string]any{"blocked": len(q.blocked), gatedKey: len(q.gated), gatesKey: len(q.gates)}
		return out, nil
	}
	if out["blocked"], err = s.blockedTasksJSON(ctx, readable, q.blocked); err != nil {
		return nil, err
	}
	if len(q.gated) > 0 {
		if out[gatedKey], err = s.heldTasksJSON(ctx, readable, q.gated); err != nil {
			return nil, err
		}
	}
	if len(q.gates) > 0 {
		if out[gatesKey], err = s.heldTasksJSON(ctx, readable, q.gates); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// nothingToUpdate is tasks_update's refusal of a call that sets nothing,
// naming every argument that would have.
func nothingToUpdate(commits bool) error {
	args := []string{"status", "title", "body", bodyAppendParam, "project", "add_depends_on", removeDependsOnParam, taskKindParam}
	args = append(args, gateTextParams...)
	args = append(args, gateClearParam)
	if commits {
		args = append(args, commitsParam)
	}
	last := len(args) - 1
	return fmt.Errorf("nothing to update: pass %s, or %s", strings.Join(args[:last], ", "), args[last])
}
