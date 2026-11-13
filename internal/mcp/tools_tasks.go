package mcp

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// defaultClaimLease is how long a task claim holds before it lapses and the task
// becomes reclaimable, unless the caller overrides it via lease_seconds.
const defaultClaimLease = 15 * time.Minute

// actorSessionArgDesc documents the `session` argument on the identity-sensitive
// task tools (claim/release/update). It carries the one fact an agent stuck behind
// errAmbiguousActor cannot get anywhere else: its own identity is the cc/<id> or
// cx/<id> its briefing prints, and passing it is how a concurrent agent names the claim as its
// own instead of having one guessed for it.
const actorSessionArgDesc = "the acting agent's session: the cc/<id> or cx/<id> on your briefing's 'Seam session' line, " +
	"another session name, or a session ULID. Defaults to this connection's session -- bound automatically for Claude " +
	"Code and Codex agents -- then a sole active ambient. Pass it when a call reports the agent as ambiguous: the bare " +
	"call then fails rather than guesses."

// actorSessionIDArgDesc documents the `session_id` argument -- the ULID power form
// of actorSessionArgDesc, taking precedence over the name and the binding.
const actorSessionIDArgDesc = "the acting agent's session ULID; takes precedence over session and the bound session"

func tasksAddTool() mcp.Tool {
	return mcp.NewTool("tasks_add", hintAdd(),
		mcp.WithDescription("Add a task to the dependency-aware ready queue. depends_on lists task ids that must finish first (done or dropped unblocks); each must exist and must not create a cycle. The task is 'ready' once it has no open/in_progress blocker."),
		mcp.WithString("title", mcp.Required(), mcp.Description("short task title")),
		mcp.WithString("body", mcp.Description("optional details / acceptance criteria (aliases: content, text)")),
		mcp.WithString("project", mcp.Description(writeProjectArgDesc)),
		mcp.WithArray("depends_on", mcp.WithStringItems(), mcp.Description("task ids this task is blocked by (a comma-separated string is also accepted)")),
		mcp.WithString("plan", mcp.Description("optional plan slug (plan:<slug> convention) that composes this task as a step of a plan. Plan steps are excluded from the default ready-queue and surfaced under the plan filter.")),
	)
}

func (s *Server) handleTasksAdd(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	title := argString(req, "title")
	if title == "" {
		return errResult("tasks_add", errors.New("title is required"))
	}
	project, err := s.resolveWriteScope(ctx, argString(req, "project"))
	if err != nil {
		return errResult("tasks_add", err)
	}
	if err := s.reachableDeps(ctx, argStrings(req, "depends_on")); err != nil {
		return errResult("tasks_add", err)
	}
	id, err := core.NewID()
	if err != nil {
		return errResult("tasks_add", err)
	}
	now := time.Now().UTC()
	task := core.Task{
		ID: id, ProjectSlug: project, Title: title, Body: argRaw(req, "body"),
		Status: core.TaskOpen, CreatedBy: s.boundSessionName(ctx),
		PlanSlug:  argString(req, "plan"),
		DependsOn: argStrings(req, "depends_on"),
		CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateTask(ctx, s.cfg.DB, task); err != nil {
		return errResult("tasks_add", err)
	}
	created, err := store.TaskByID(ctx, s.cfg.DB, id)
	if err != nil {
		return errResult("tasks_add", err)
	}
	s.record(ctx, core.EventTaskTransition, s.boundSession(ctx), project, id,
		map[string]any{"to": string(core.TaskOpen), "created": true})
	out := taskJSON(created)
	s.fenceWrittenTaskDeps(ctx, created, out)
	return jsonResult(out)
}

func tasksUpdateTool() mcp.Tool {
	return mcp.NewTool("tasks_update", hintSet(),
		mcp.WithDescription("Update a task: change status (open|in_progress|done|dropped), edit title/body, or add or remove dependencies. Moving to done/dropped closes it and unblocks its dependents; removing its last unfinished blocker makes it ready. Removing an id that is not one of its blockers changes nothing and is named back in not_depends_on; the ids removed are in removed_depends_on. A task another session holds via a live claim is locked to its holder: updating it fails with 'already claimed' until the lease lapses or the holder releases it."),
		mcp.WithString("id", mcp.Required(), mcp.Description("task id")),
		mcp.WithString("status", enumOf(core.TaskStatuses), mcp.Description("new status")),
		mcp.WithString("title", mcp.Description("new title")),
		mcp.WithString("body", mcp.Description("new body (aliases: content, text)")),
		mcp.WithString("project", mcp.Description("reassign the task to another project slug (used when a split moves a project's open work to a child)")),
		mcp.WithArray("add_depends_on", mcp.WithStringItems(), mcp.Description("task ids to add as blockers (a comma-separated string is also accepted)")),
		mcp.WithArray(removeDependsOnParam, mcp.WithStringItems(), mcp.Description("task ids to remove as blockers (a comma-separated string is also accepted); an id that is not a blocker of this task is a no-op, named back in not_depends_on")),
		mcp.WithString("session", mcp.Description("the acting agent's session (the cc/<id> or cx/<id> on your briefing's 'Seam session' line, or a session name); needed only to mutate a task you hold a live claim on when several agents are active")),
		mcp.WithString("session_id", mcp.Description(actorSessionIDArgDesc)),
		mcp.WithArray(commitsParam, mcp.WithStringItems(), mcp.Description(taskCommitsArgDesc)),
	)
}

func (s *Server) handleTasksUpdate(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id := argString(req, "id")
	if id == "" {
		return errResult("tasks_update", errors.New("id is required"))
	}
	var patch store.TaskPatch
	if st := argString(req, "status"); st != "" {
		status := core.TaskStatus(st)
		if !status.Valid() {
			return errResult("tasks_update", fmt.Errorf("invalid status %q", st))
		}
		patch.Status = &status
	}
	if argPresent(req, "title") {
		title := argString(req, "title")
		if title == "" {
			return errResult("tasks_update", errors.New("title must not be blank; omit it to keep the current title"))
		}
		patch.Title = &title
	}
	if argPresent(req, "body") {
		body := argRaw(req, "body")
		patch.Body = &body
	}
	if argPresent(req, "project") {
		project, perr := validateProjectArg(argString(req, "project"))
		if perr != nil {
			return errResult("tasks_update", perr)
		}
		patch.ProjectSlug = &project
	}
	patch.AddDependsOn = argStrings(req, "add_depends_on")
	if err := s.reachableDeps(ctx, patch.AddDependsOn); err != nil {
		return errResult("tasks_update", err)
	}
	// Removals need no reach check here: the store judges each against the
	// request's View inside the update's transaction, and a blocker the caller
	// may not reach reads exactly like an id that names nothing (removeDeps).
	patch.RemoveDependsOn = argStrings(req, removeDependsOnParam)
	commits, err := s.commitsArg(req)
	if err != nil {
		return errResult("tasks_update", err)
	}
	patch.AddCommits = commits

	if patch.Status == nil && patch.Title == nil && patch.Body == nil && patch.ProjectSlug == nil &&
		len(patch.AddDependsOn) == 0 && len(patch.RemoveDependsOn) == 0 && len(patch.AddCommits) == 0 {
		if s.cfg.RecordCommits {
			return errResult("tasks_update", errors.New("nothing to update: pass status, title, body, project, add_depends_on, remove_depends_on, or commits"))
		}
		return errResult("tasks_update", errors.New("nothing to update: pass status, title, body, project, add_depends_on, or remove_depends_on"))
	}
	// A task id is globally unique, so nothing above resolved a write scope. The
	// row is loaded first purely to learn the project the fence judges against;
	// UpdateTask still owns the update itself.
	current, err := s.taskByIDFor(ctx, id)
	if err != nil {
		return errResult("tasks_update", err)
	}
	if err := s.fenceWrite(ctx, current.ProjectSlug); err != nil {
		return errResult("tasks_update", err)
	}
	if patch.ProjectSlug != nil && *patch.ProjectSlug != current.ProjectSlug {
		// Reassignment is a read of the old project plus a write into the new, so
		// an outside caller may edit a confidential task in place but may not
		// carry it out of the fence (the notes_update move, same reasoning).
		if err := s.fenceReadNoAsk(ctx, current.ProjectSlug); err != nil {
			return errResult("tasks_update", err)
		}
		if err := s.fenceWrite(ctx, *patch.ProjectSlug); err != nil {
			return errResult("tasks_update", err)
		}
	}
	// Resolve the acting session for the holder-lock. An explicit-but-unknown
	// identity (or a store error) fails loudly; an AMBIGUOUS actor does not --
	// actor stays "" (a sentinel that matches no live holder). UpdateTask's
	// holder-lock defers to core.Task.ClaimLive, so "" still freely edits an
	// unclaimed task while a task under a sibling's live claim is refused. The
	// agent names itself (session=) only when it must mutate a task it holds --
	// exactly the case where guessing an identity onto a claim would corrupt it.
	actor, _, err := s.resolveActor(ctx, req)
	if err != nil && !errors.Is(err, errAmbiguousActor) {
		return errResult("tasks_update", err)
	}
	// The commits and the removals ride in the patch: UpdateTask applies them
	// in the same transaction and behind the same holder lock as every other
	// change, so whoever may update the task may record its commits or drop
	// its blockers, and nobody else.
	res, err := store.UpdateTaskDetailed(ctx, s.cfg.DB, id, patch, actor, time.Now().UTC())
	if err != nil {
		return errResult("tasks_update", err)
	}
	updated := res.Task
	// A call whose only ask was removals that named no blocker changed nothing,
	// so the task's history gains nothing either.
	if !res.Unchanged {
		// from is the status before the call, read inside its transaction, so
		// the history tells a status move (from != to) from an update that moved
		// none -- an edit, a new or removed blocker, a commit, a project move --
		// which a bare to would read as a move to the status the task had.
		transition := map[string]any{"to": string(updated.Status), "from": string(res.From)}
		if len(commits) > 0 {
			transition["commits_recorded"] = len(commits)
		}
		if len(res.RemovedDependsOn) > 0 {
			transition[removedDependsOnKey] = res.RemovedDependsOn
		}
		s.record(ctx, core.EventTaskTransition, actor, updated.ProjectSlug, id, transition)
	}
	// The response is judged against the project the task now sits in: a move
	// INTO a fenced project is sanctioned, but the item it lands on is that
	// project's content from the moment it arrives.
	out := s.withholdTask(ctx, updated.ProjectSlug, updated)
	if len(commits) > 0 {
		out["commits_recorded"] = len(commits)
	}
	if _, withheld := out[withheldKey]; !withheld {
		// The removal's report names only ids the caller sent. A caller that may
		// write the task but not read it gets no report: whether an id was one
		// of its blockers is that project's dependency graph (taskContentFields).
		if len(res.RemovedDependsOn) > 0 {
			out[removedDependsOnKey] = res.RemovedDependsOn
		}
		if len(res.AbsentDependsOn) > 0 {
			out[notDependsOnKey] = res.AbsentDependsOn
		}
	}
	return jsonResult(out)
}

// removeDependsOnParam is tasks_update's blocker-removal argument.
const removeDependsOnParam = "remove_depends_on"

// removedDependsOnKey lists, in a tasks_update answer and in its
// task.transition record, the remove_depends_on ids that were blockers and no
// longer are; notDependsOnKey the ones that were not, each a no-op. Both are
// present only when non-empty, so an answer without a removal reads as before.
const (
	removedDependsOnKey = "removed_depends_on"
	notDependsOnKey     = "not_depends_on"
)

func tasksReadyTool() mcp.Tool {
	return mcp.NewTool("tasks_ready", hintRead(),
		mcp.WithDescription("List the actionable (ready) tasks for a project -- open tasks with no unfinished blocker -- oldest first, plus the blocked tasks with their still-open blockers. By default plan-step tasks are excluded; pass plan=<slug> to list that plan's steps instead."),
		mcp.WithString("project", mcp.Description("project slug; defaults to the bound session's project")),
		mcp.WithString("plan", mcp.Description("optional plan slug: return that plan's ready/blocked step tasks instead of the default (non-plan) queue")),
	)
}

func (s *Server) handleTasksReady(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project, err := s.resolveReadScope(ctx, argString(req, "project"))
	if err != nil {
		return errResult("tasks_ready", err)
	}
	plan := argString(req, "plan")
	var ready []core.Task
	var blocked []store.BlockedTask
	if plan != "" {
		ready, err = store.ReadyTasksForPlan(ctx, s.cfg.DB, project, plan)
		if err != nil {
			return errResult("tasks_ready", err)
		}
		blocked, err = store.BlockedTasksForPlan(ctx, s.cfg.DB, project, plan)
	} else {
		ready, err = store.ReadyTasks(ctx, s.cfg.DB, project)
		if err != nil {
			return errResult("tasks_ready", err)
		}
		blocked, err = store.BlockedTasks(ctx, s.cfg.DB, project)
	}
	if err != nil {
		return errResult("tasks_ready", err)
	}
	readable := s.taskReader(ctx)
	readyJSON, err := s.tasksJSON(ctx, readable, ready)
	if err != nil {
		return errResult("tasks_ready", err)
	}
	blockedTasks := make([]core.Task, len(blocked))
	for i, b := range blocked {
		blockedTasks[i] = b.Task
	}
	blockedJSON, err := s.tasksJSON(ctx, readable, blockedTasks)
	if err != nil {
		return errResult("tasks_ready", err)
	}
	for i, b := range blocked {
		blockers := make([]map[string]any, 0, len(b.Blockers))
		for _, bl := range b.Blockers {
			// A blocker from a project the caller may not read is withheld --
			// its title is that project's content, and its id the handle to it
			// -- while the entry itself stays, so the count is true. Only the
			// owner, kept out by a lock alone, is told which locked project.
			ok, err := readable(bl.ProjectSlug)
			if err != nil {
				return errResult("tasks_ready", err)
			}
			if !ok {
				blockers = append(blockers, map[string]any{withheldKey: withheldBlocker(ctx, lockedBlockerProject(ctx, bl.ProjectSlug))})
				continue
			}
			blockers = append(blockers, map[string]any{"id": bl.ID, "title": bl.Title, "status": string(bl.Status)})
		}
		blockedJSON[i]["blockers"] = blockers
	}
	return jsonResult(map[string]any{"project": project, "ready": readyJSON, "blocked": blockedJSON})
}

func tasksListTool() mcp.Tool {
	return mcp.NewTool("tasks_list", hintRead(),
		mcp.WithDescription("List a project's tasks, optionally filtered by status, newest first. By default plan-step tasks are excluded; pass plan=<slug> to list that plan's steps instead. Pass id=<task id> to load a single task by its globally-unique id (a direct lookup that ignores project/status/plan and needs no session scope)."),
		mcp.WithString("id", mcp.Description("load exactly one task by its globally-unique id; when set, project/status/plan are ignored and the response's tasks array holds just that task")),
		mcp.WithString("project", mcp.Description("project slug; defaults to the bound session's project")),
		mcp.WithString("status", enumOf(core.TaskStatuses), mcp.Description("optional status filter")),
		mcp.WithString("plan", mcp.Description("optional plan slug: list that plan's step tasks instead of the default (non-plan) tasks")),
	)
}

func (s *Server) handleTasksList(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// id is a globally-unique ULID, so a by-id load is a direct lookup that needs
	// no project scope -- an agent can load a task it only knows the id of.
	if id := argString(req, "id"); id != "" {
		task, err := s.taskByIDFor(ctx, id)
		if err != nil {
			return errResult("tasks_list", err)
		}
		// Needing no scope is precisely why this path needs the fence: the row
		// itself names the project the isolation check is made against.
		if err := s.fenceRead(ctx, task.ProjectSlug); err != nil {
			return errResult("tasks_list", err)
		}
		out, err := s.tasksJSON(ctx, s.taskReader(ctx), []core.Task{task})
		if err != nil {
			return errResult("tasks_list", err)
		}
		return jsonResult(map[string]any{"project": task.ProjectSlug, "tasks": out})
	}
	project, err := s.resolveReadScope(ctx, argString(req, "project"))
	if err != nil {
		return errResult("tasks_list", err)
	}
	var status core.TaskStatus
	if st := argString(req, "status"); st != "" {
		status = core.TaskStatus(st)
		if !status.Valid() {
			return errResult("tasks_list", fmt.Errorf("invalid status %q", st))
		}
	}
	var tasks []core.Task
	if plan := argString(req, "plan"); plan != "" {
		tasks, err = store.ListTasksForPlan(ctx, s.cfg.DB, project, status, plan)
	} else {
		tasks, err = store.ListTasks(ctx, s.cfg.DB, project, status)
	}
	if err != nil {
		return errResult("tasks_list", err)
	}
	out, err := s.tasksJSON(ctx, s.taskReader(ctx), tasks)
	if err != nil {
		return errResult("tasks_list", err)
	}
	return jsonResult(map[string]any{"project": project, "tasks": out})
}

func tasksClaimTool() mcp.Tool {
	return mcp.NewTool("tasks_claim", hintAdd(),
		mcp.WithDescription("Atomically claim a task for the current session, moving it to in_progress with a lease. A refused claim names its cause: held by another live claim ('task already claimed'), waiting on unfinished dependencies ('task blocked', naming the blockers -- finish those first), or already done/dropped ('task closed'). Re-claiming a task you already hold refreshes (heartbeats) the lease; a task whose lease has expired can be reclaimed. Release it with tasks_release or by closing it (tasks_update done/dropped); session_end releases all of a session's claims."),
		mcp.WithString("id", mcp.Required(), mcp.Description("task id to claim")),
		// No Min/Max: the handler owns this parameter's range, because its upper
		// bound is an int64-overflow hazard in the seconds->Duration conversion
		// rather than a domain limit a schema could state. The validator owns the
		// type; the handler owns range and domain.
		mcp.WithNumber("lease_seconds", mcp.Description("lease duration in seconds before the claim lapses and the task becomes reclaimable (default 900)")),
		mcp.WithString("session", mcp.Description(actorSessionArgDesc)),
		mcp.WithString("session_id", mcp.Description(actorSessionIDArgDesc)),
	)
}

func (s *Server) handleTasksClaim(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id := argString(req, "id")
	if id == "" {
		return errResult("tasks_claim", errors.New("id is required"))
	}
	// The same by-id hole as tasks_update, and a read as well as a write: a claim
	// locks the task to its holder AND returns the full task body, so an unfenced
	// claim hands an outside caller a fenced project's work by ULID alone. The
	// write half is refused here; the read half is trimmed out of the response
	// below, because a confidential project takes the claim but never answers with
	// its content.
	target, err := s.taskByIDFor(ctx, id)
	if err != nil {
		return errResult("tasks_claim", err)
	}
	if err := s.fenceWrite(ctx, target.ProjectSlug); err != nil {
		return errResult("tasks_claim", err)
	}
	sessionID, ok, err := s.resolveActor(ctx, req)
	if err != nil {
		return errResult("tasks_claim", err)
	}
	if !ok {
		return errResult("tasks_claim", errors.New(
			"no active session to claim as: start a session (session_start), or name yourself with session=<cc/... or cx/... or ULID>"))
	}
	lease := defaultClaimLease
	if argPresent(req, "lease_seconds") {
		secs := argInt(req, "lease_seconds", 0)
		// The upper bound guards int64 overflow in the seconds->Duration
		// conversion: an overflowed lease goes negative, so the claim would
		// report success while being instantly expired (silently reclaimable).
		if secs <= 0 || int64(secs) > int64(math.MaxInt64/time.Second) {
			return errResult("tasks_claim", fmt.Errorf("invalid lease_seconds %d", secs))
		}
		lease = time.Duration(secs) * time.Second
	}
	res, err := store.ClaimTask(ctx, s.cfg.DB, id, sessionID, lease, time.Now().UTC())
	if err != nil {
		return errResult("tasks_claim", s.fenceBlockedClaim(ctx, err))
	}
	payload := map[string]any{"to": string(res.Task.Status), "claimed_by": sessionID}
	if res.Reclaimed {
		payload["reclaimed"] = true
		payload["prior_holder"] = res.PriorHolder
	}
	s.record(ctx, core.EventTaskTransition, sessionID, res.Task.ProjectSlug, id, payload)
	return jsonResult(s.withholdTask(ctx, res.Task.ProjectSlug, res.Task))
}

func tasksReleaseTool() mcp.Tool {
	return mcp.NewTool("tasks_release", hintSet(),
		mcp.WithDescription("Release a task the current session holds, reopening it (status back to open, claim cleared) so another agent can claim it. Only the current holder may release."),
		mcp.WithString("id", mcp.Required(), mcp.Description("task id to release")),
		mcp.WithString("session", mcp.Description(actorSessionArgDesc)),
		mcp.WithString("session_id", mcp.Description(actorSessionIDArgDesc)),
	)
}

func (s *Server) handleTasksRelease(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id := argString(req, "id")
	if id == "" {
		return errResult("tasks_release", errors.New("id is required"))
	}
	// Fenced for the same reason as the claim: only the holder may release, but a
	// successful release still returns the task body, so the response is a read
	// across the fence even when the mutation is benign -- hence the withhold on
	// the way out as well as the refusal on the way in.
	target, err := s.taskByIDFor(ctx, id)
	if err != nil {
		return errResult("tasks_release", err)
	}
	if err := s.fenceWrite(ctx, target.ProjectSlug); err != nil {
		return errResult("tasks_release", err)
	}
	sessionID, ok, err := s.resolveActor(ctx, req)
	if err != nil {
		return errResult("tasks_release", err)
	}
	if !ok {
		return errResult("tasks_release", errors.New("no active session; nothing to release"))
	}
	released, err := store.ReleaseTask(ctx, s.cfg.DB, id, sessionID, time.Now().UTC())
	if err != nil {
		return errResult("tasks_release", err)
	}
	s.record(ctx, core.EventTaskTransition, sessionID, released.ProjectSlug, id,
		map[string]any{"to": string(released.Status), "released": true})
	return jsonResult(s.withholdTask(ctx, released.ProjectSlug, released))
}

// taskContentFields are the taskJSON keys a caller that may WRITE a task's
// project but not READ it does not get back (withholdContent). What stays is
// identity (id, project) and the state that call itself produced (status, and
// for a claim the holder and lease) -- the caller needs those to know what its
// own write did and to release what it now holds. Everything else describes the
// fenced project rather than the caller's action: authored text (title, body),
// and the curation and graph around it (plan, depends_on, created_by, favorite).
// A permitted write is not a licence to read any of it.
var taskContentFields = []string{"title", "body", "plan", "depends_on", "created_by", "favorite"}

// taskJSON renders a task for a tool response.
func taskJSON(t core.Task) map[string]any {
	j := map[string]any{
		"id": t.ID, "title": t.Title, "status": string(t.Status),
		"project": t.ProjectSlug, "created_by": t.CreatedBy,
	}
	if t.Body != "" {
		j["body"] = t.Body
	}
	if t.PlanSlug != "" {
		j["plan"] = t.PlanSlug
	}
	if t.ClaimedBy != "" {
		j["claimed_by"] = t.ClaimedBy
	}
	if t.LeaseExpiresAt != nil {
		j["lease_expires_at"] = core.FormatTime(*t.LeaseExpiresAt)
	}
	if len(t.DependsOn) > 0 {
		j["depends_on"] = t.DependsOn
	}
	if t.Favorite {
		j["favorite"] = true
	}
	// Provenance (peer_auth.restricted): the restricted account that created
	// the task, and the one whose rewrite of its title or body the text may
	// still carry (a status change or a claim never sets it). The owner's
	// tasks carry neither key.
	if t.Account != "" {
		j["account"] = t.Account
	}
	if t.UpdatedAccount != "" {
		j["updated_account"] = t.UpdatedAccount
	}
	return j
}

// taskAccountFields are the provenance keys taskJSON adds only when set. A
// trimmed response (withholdContent) drops them like created_by, but they are
// not in taskContentFields: that list is named back to the caller verbatim,
// and the owner's withheld responses must read exactly as they always did.
// Dropping keys that are absent anyway is a no-op, so this says nothing about
// whether a withheld task had them.
var taskAccountFields = []string{"account", "updated_account"}

// withholdTask is withholdContent for a task response. A task the caller may
// read still lists only the dependencies it may read (fenceWrittenTaskDeps).
func (s *Server) withholdTask(ctx context.Context, project string, t core.Task) map[string]any {
	out := s.withholdContent(ctx, project, taskJSON(t), taskContentFields)
	if _, withheld := out[withheldKey]; withheld {
		for _, f := range taskAccountFields {
			delete(out, f)
		}
		return out
	}
	s.fenceWrittenTaskDeps(ctx, t, out)
	return out
}

// unreachableBlocker is what tasks_ready shows in place of a blocker that lies
// in a project the caller may not read. The words are the ones a restricted
// account (peer_auth.restricted) has always been shown, and every caller gets
// them but the owner at a locked project it holds no lease on
// (lockedBlockerProject, withheldBlocker).
const unreachableBlocker = "a task in a project this connection may not reach blocks this one"

// withheldBlockers is tasks_claim's "task blocked" refusal's words for the n
// blockers it may not name: the count, never an id.
func withheldBlockers(n int) string {
	if n == 1 {
		return "a task in a project this connection may not reach"
	}
	return fmt.Sprintf("%d tasks in projects this connection may not reach", n)
}

// withheldBlocker is tasks_ready's words for one blocker the caller may not
// read. locked is the locked project lockedBlockerProject named for it: the
// owner is told which, and how to reach it -- "a task in locked project
// acme-finance blocks this one: bind to acme-finance, or approve a request for
// it". With locked "" it is unreachableBlocker, as before.
func withheldBlocker(ctx context.Context, locked string) string {
	if locked == "" {
		return unreachableBlocker
	}
	return "a task in locked project " + locked + " blocks this one: " + lockedBlockerRemedy(ctx, locked)
}

// lockedBlockerRemedy is how the owner reaches a locked project's blocker
// from this connection. On the peer_auth socket it is the owner's approval of
// a request -- for a session bound there, since a locked project is normally
// confidential and reads only from its own sessions. The TCP connection never
// reaches a locked project (access.View.Refusal says why), so its remedy is
// the socket.
func lockedBlockerRemedy(ctx context.Context, project string) string {
	if !access.PeerAuthenticated(ctx) {
		return "the TCP connection never reaches a locked project, so work in it from an agent session on the " +
			"peer_auth Unix socket"
	}
	return "bind to " + project + ", or approve a request for it"
}

// lockedBlockersWithheld is tasks_claim's "task blocked" refusal's words for
// the blockers it may not name, and their remedy. byLocked counts the ones in
// each locked project the owner is told about (lockedBlockerProject), named
// with their count and never an id; generic counts the rest, in the words
// every caller has always had. The remedy is "" with nothing named.
func lockedBlockersWithheld(ctx context.Context, byLocked map[string]int, generic int) (withheld, remedy string) {
	projects := make([]string, 0, len(byLocked))
	for p := range byLocked {
		projects = append(projects, p)
	}
	slices.Sort(projects)
	parts := make([]string, 0, len(projects)+1)
	remedies := make([]string, 0, len(projects))
	for _, p := range projects {
		if n := byLocked[p]; n == 1 {
			parts = append(parts, "a task in locked project "+p)
		} else {
			parts = append(parts, fmt.Sprintf("%d tasks in locked project %s", n, p))
		}
		remedies = append(remedies, lockedBlockerRemedy(ctx, p))
	}
	if generic > 0 {
		parts = append(parts, withheldBlockers(generic))
	}
	if len(remedies) > 0 {
		remedy = strings.Join(slices.Compact(remedies), "; ") + "."
	}
	return strings.Join(parts, ", "), remedy
}

// lockedBlockerProject is the locked project a blocker
// the caller may not read is named by, or "" for the generic words. It names
// one only when the blocker's project is locked and this connection holds no
// lease on it (access.Locked), and the caller is the owner's own OS account:
// a locked project's name is already listed to the owner -- project_list,
// scope errors and recall name it -- so naming it says nothing new, and its
// remedy is the owner's to give. Everyone else keeps the generic words: a
// restricted account (its refusals never tell a closed project from a missing
// one), and every caller kept out of a blocker by an isolation fence alone --
// a confidential or sealed project that is not locked, or a locked one this
// connection holds a lease on -- since naming that project would leak across
// the fence (project-isolation-fence-contract).
func lockedBlockerProject(ctx context.Context, project string) string {
	if access.AccountOf(ctx) != "" || !access.Locked(ctx, project) {
		return ""
	}
	return project
}

// depsWithheldKey is the count a task answer carries in place of the
// depends_on ids it leaves out. It is present only when one was left out, so
// an answer whose dependencies are all readable reads exactly as before.
const depsWithheldKey = "depends_on_withheld"

// taskReader answers, for one call, whether its caller may read the tasks of
// a project: store.CanRead from callerScope -- the question withholdContent
// asks of a write's answer -- asked once per project. The caller's scope is
// resolved on the first question, so an answer that names no other task costs
// nothing. Under peer_auth CanRead asks the request's View first: a locked
// project without a lease, or (for a restricted account) any project that is
// not open, reads as no.
func (s *Server) taskReader(ctx context.Context) func(project string) (bool, error) {
	var caller string
	scoped := false
	memo := map[string]bool{}
	return func(project string) (bool, error) {
		if ok, seen := memo[project]; seen {
			return ok, nil
		}
		if !scoped {
			caller, scoped = s.callerScope(ctx), true
		}
		ok, err := store.CanRead(ctx, s.cfg.DB, caller, project)
		if err != nil {
			return false, err
		}
		memo[project] = ok
		return ok, nil
	}
}

// tasksJSON renders tasks for a read answer, each listing only the
// dependencies its caller may read (fenceTaskDeps).
func (s *Server) tasksJSON(ctx context.Context, readable func(string) (bool, error), tasks []core.Task) ([]map[string]any, error) {
	out := make([]map[string]any, len(tasks))
	for i, t := range tasks {
		out[i] = taskJSON(t)
	}
	if err := s.fenceTaskDeps(ctx, readable, tasks, out); err != nil {
		return nil, err
	}
	return out, nil
}

// fenceTaskDeps trims each answer's depends_on (out[i] renders tasks[i]) to
// the dependencies whose project the caller may read, and counts the rest
// under depsWithheldKey. A dependency id is the handle to a task in its own
// project -- by id alone an agent asks for it, or hangs work on it -- so a
// project's fence covers the ids of its tasks as much as their titles. An id
// that names no task row anymore has no project to judge and stays.
func (s *Server) fenceTaskDeps(ctx context.Context, readable func(string) (bool, error), tasks []core.Task, out []map[string]any) error {
	var ids []string
	for _, t := range tasks {
		ids = append(ids, t.DependsOn...)
	}
	if len(ids) == 0 {
		return nil
	}
	projects, err := store.TaskProjects(ctx, s.cfg.DB, ids)
	if err != nil {
		return err
	}
	for i, t := range tasks {
		kept := make([]string, 0, len(t.DependsOn))
		for _, dep := range t.DependsOn {
			if project, known := projects[dep]; known {
				ok, err := readable(project)
				if err != nil {
					return err
				}
				if !ok {
					continue
				}
			}
			kept = append(kept, dep)
		}
		n := len(t.DependsOn) - len(kept)
		if n == 0 {
			continue
		}
		if len(kept) == 0 {
			delete(out[i], "depends_on")
		} else {
			out[i]["depends_on"] = kept
		}
		out[i][depsWithheldKey] = n
	}
	return nil
}

// fenceWrittenTaskDeps is fenceTaskDeps for a write's answer, which cannot fail
// the call: the write already landed. A lookup that fails withholds every
// dependency, failing closed as withholdContent does.
func (s *Server) fenceWrittenTaskDeps(ctx context.Context, t core.Task, out map[string]any) {
	if err := s.fenceTaskDeps(ctx, s.taskReader(ctx), []core.Task{t}, []map[string]any{out}); err != nil {
		s.logger.Warn("mcp: response dependency fence", "task", t.ID, "error", err)
		delete(out, "depends_on")
		out[depsWithheldKey] = len(t.DependsOn)
	}
}

// fenceBlockedClaim withholds, from tasks_claim's "task blocked" refusal, the
// blockers its caller may not read: the refusal names the rest as before and
// counts these -- by locked project, with the remedy on a second line, for
// the owner at a locked project it holds no lease on (lockedBlockerProject).
// Any other error, and a refusal whose blockers are all readable, is returned
// unchanged. A lookup that fails withholds every blocker in the generic
// words: the refusal's cause stays true, and nothing it cannot vouch for is
// named.
func (s *Server) fenceBlockedClaim(ctx context.Context, err error) error {
	var blocked *store.TaskBlockedError
	if !errors.As(err, &blocked) {
		return err
	}
	readable := s.taskReader(ctx)
	kept := make([]core.Task, 0, len(blocked.Blockers))
	byLocked := map[string]int{}
	generic := 0
	for _, b := range blocked.Blockers {
		ok, rerr := readable(b.ProjectSlug)
		if rerr != nil {
			s.logger.Warn("mcp: claim refusal fence", "task", blocked.TaskID, "error", rerr)
			kept, byLocked, generic = kept[:0], map[string]int{}, len(blocked.Blockers)
			break
		}
		switch locked := lockedBlockerProject(ctx, b.ProjectSlug); {
		case ok:
			kept = append(kept, b)
		case locked != "":
			byLocked[locked]++
		default:
			generic++
		}
	}
	if len(kept) == len(blocked.Blockers) {
		return err
	}
	withheld, remedy := lockedBlockersWithheld(ctx, byLocked, generic)
	return &store.TaskBlockedError{TaskID: blocked.TaskID, Blockers: kept, Withheld: withheld, Remedy: remedy}
}

// taskByIDFor is store.TaskByID for the caller in ctx. A restricted account
// (peer_auth.restricted) is told that a task in a project it may not reach
// does not exist, in exactly the words a missing id gets: its refusals never
// tell a closed project from a missing one, and the fence refusal that would
// follow the load names the task's project. Everyone else gets TaskByID, and
// the fence after it, as before.
func (s *Server) taskByIDFor(ctx context.Context, id string) (core.Task, error) {
	t, err := store.TaskByID(ctx, s.cfg.DB, id)
	if err == nil && access.AccountOf(ctx) != "" && !access.Visible(ctx, t.ProjectSlug) {
		return core.Task{}, fmt.Errorf("store.TaskByID: %w: %q", store.ErrTaskNotFound, id)
	}
	return t, err
}

// reachableDeps refuses a dependency on a task the caller may not reach under
// peer_auth: a restricted account's closed projects, and a project locked to
// this connection. An edge to such a task would confirm its id exists and let
// the caller watch it close, and for a restricted account would put its title
// in tasks_ready. A missing task gets the same text, so the refusal never
// tells the two apart. With peer_auth off the store alone checks dependencies,
// as before; confidential projects are not judged here.
func (s *Server) reachableDeps(ctx context.Context, deps []string) error {
	if _, on := access.FromContext(ctx); !on {
		return nil
	}
	for _, dep := range deps {
		t, err := store.TaskByID(ctx, s.cfg.DB, dep)
		if err == nil && access.Visible(ctx, t.ProjectSlug) {
			continue
		}
		if err == nil || errors.Is(err, store.ErrTaskNotFound) {
			return fmt.Errorf("depends_on %q: %w", dep, store.ErrTaskNotFound)
		}
		return err
	}
	return nil
}

// boundSessionName resolves the bound (or ambient-fallback) session's name for a
// task's created_by field, or "" when there is no session.
func (s *Server) boundSessionName(ctx context.Context) string {
	id := s.boundSession(ctx)
	if id == "" {
		return ""
	}
	if sess, ok, err := store.SessionByID(ctx, s.cfg.DB, id); err == nil && ok {
		return sess.Name
	}
	return ""
}
