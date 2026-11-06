package main

// seam task -- add / list / transition / claim / release tasks.

import (
	"context"
	"flag"
	"fmt"
	"strings"

	"github.com/arctop/seamless/internal/core"
)

// --- task list ---

var taskListCmd = spec("task list", groupTasks, "list tasks, newest first",
	// `seam task list <id>` is a natural thing to type, and it used to list EVERY
	// task: --id was registered but the bare positional was never checked, so it
	// was dropped. The hint is per-spec because "unexpected argument" is the right
	// generic frame and no amount of arity arithmetic knows to suggest --id.
	noArgs().withHint("to load one task by id, use --id"),
	bindTaskList, runTaskList)

type taskListOpts struct {
	id      *string
	project *string
	status  *string
	plan    *string
	asJSON  *bool
}

func bindTaskList(fs *flag.FlagSet) *taskListOpts {
	return &taskListOpts{
		id:      fs.String("id", "", "load a single task by its globally-unique `ID` (ignores project/status/plan)"),
		project: fs.String("project", "", "project `SLUG`"),
		// Derived from the canonical set, not transcribed. The server validates the
		// same slice (internal/mcp/tools_tasks.go), so the client now agrees at
		// parse time instead of round-tripping to find out.
		status: enumFlag(fs, "status", "", "filter by `STATUS`", enumOf(core.TaskStatuses)),
		plan:   fs.String("plan", "", "list the step tasks of plan `SLUG` instead of the default (non-plan) tasks"),
		asJSON: jsonFlag(fs, jsonUsage),
	}
}

func runTaskList(ctx context.Context, e *env, o *taskListOpts, _ []string) error {
	cli, _, err := e.dial(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = cli.Close() }()
	args := map[string]any{
		"id": *o.id, "project": *o.project, "status": *o.status, "plan": *o.plan,
	}
	if *o.asJSON {
		return printToolJSON(ctx, e, cli, "tasks_list", args)
	}
	out, err := callTool(ctx, cli, "tasks_list", args)
	if err != nil {
		return err
	}
	tasks, _ := out["tasks"].([]any)
	if len(tasks) == 0 {
		fmt.Fprintln(e.stdout, "(no tasks)")
		return nil
	}
	for _, t := range tasks {
		m, _ := t.(map[string]any)
		fmt.Fprintf(e.stdout, "  %s  [%-11s] %s\n", shortID(str(m["id"])), str(m["status"]), str(m["title"]))
	}
	return nil
}

// --- task add ---

var taskAddCmd = spec("task add", groupTasks, "add a task to the ready queue",
	noArgs().withHint("the title is a flag, not a positional: seam task add --title \"...\""),
	bindTaskAdd, runTaskAdd)

type taskAddOpts struct {
	title   *string
	body    *string
	project *string
	depends *string
	plan    *string
}

func bindTaskAdd(fs *flag.FlagSet) *taskAddOpts {
	return &taskAddOpts{
		title:   fs.String("title", "", "task `TITLE` (required)"),
		body:    fs.String("body", "", "optional details: `TEXT`"),
		project: fs.String("project", "", "project `SLUG`"),
		depends: fs.String("depends", "", "comma-separated blocker task `IDS`"),
		plan:    fs.String("plan", "", "plan `SLUG`: compose this task as a step of a plan (plan:<slug>)"),
	}
}

func runTaskAdd(ctx context.Context, e *env, o *taskAddOpts, _ []string) error {
	if strings.TrimSpace(*o.title) == "" {
		return fmt.Errorf("--title is required")
	}
	cli, _, err := e.dial(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = cli.Close() }()
	out, err := callTool(ctx, cli, "tasks_add", map[string]any{
		"title": *o.title, "body": *o.body, "project": *o.project,
		"depends_on": *o.depends, "plan": *o.plan,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "added task %s %q\n", shortID(str(out["id"])), str(out["title"]))
	return nil
}

// --- task done / start / drop / reopen ---

// actorSessionFlag declares --session on the identity-sensitive task commands.
// Every seam invocation dials a fresh connection, so it never has a bound
// session; with several agents active the server refuses to guess the actor
// (errAmbiguousActor), and this flag is how the caller names itself -- the
// cc/... or cx/... on its briefing's 'Seam session' line, or a sess/* name.
func actorSessionFlag(fs *flag.FlagSet) *string {
	return fs.String("session", "",
		"act as session `NAME` (a cc/..., cx/..., or sess/... name); required when several agents are active, since a fresh CLI connection cannot be attributed automatically")
}

// taskActorOpts carries --session for the commands whose only option it is.
type taskActorOpts struct {
	session *string
}

func bindTaskActor(fs *flag.FlagSet) *taskActorOpts {
	return &taskActorOpts{session: actorSessionFlag(fs)}
}

// taskTransitionSpec builds the four one-word status changes from one declaration.
// They differ only in the status they send, so each stays a real command with its
// own name in the help rather than a hidden alias -- and the status comes from the
// spec, replacing a map lookup that yielded "" for anything unrecognized.
func taskTransitionSpec(sub, status, summary string) cmd {
	return spec("task "+sub, groupTasks, summary, exactly(1, "id"), bindTaskActor,
		func(ctx context.Context, e *env, o *taskActorOpts, pos []string) error {
			return runTaskTransition(ctx, e, o, sub, status, pos[0])
		})
}

var (
	taskDoneCmd   = taskTransitionSpec("done", "done", "mark a task done (unblocks its dependents)")
	taskStartCmd  = taskTransitionSpec("start", "in_progress", "mark a task in_progress")
	taskDropCmd   = taskTransitionSpec("drop", "dropped", "drop a task (unblocks its dependents)")
	taskReopenCmd = taskTransitionSpec("reopen", "open", "reopen a closed task")
)

func runTaskTransition(ctx context.Context, e *env, o *taskActorOpts, sub, status, id string) error {
	cli, _, err := e.dial(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = cli.Close() }()
	out, err := callTool(ctx, cli, "tasks_update", map[string]any{"id": id, "status": status, "session": *o.session})
	if err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "task %s %s -> %s\n", shortID(id), sub, str(out["status"]))
	return nil
}

// --- task claim / heartbeat ---

// taskClaimSpec builds `task claim` and `task heartbeat` from one declaration:
// they are the same tasks_claim call -- it claims a ready task, or refreshes the
// lease when the caller already holds it -- and each gets its own name in the help.
func taskClaimSpec(sub, summary string) cmd {
	return spec("task "+sub, groupTasks, summary, exactly(1, "id"), bindTaskClaim,
		func(ctx context.Context, e *env, o *taskClaimOpts, pos []string) error {
			return runTaskClaim(ctx, e, o, sub, pos[0])
		})
}

var (
	taskClaimCmd     = taskClaimSpec("claim", "claim a ready task, leasing it for this session")
	taskHeartbeatCmd = taskClaimSpec("heartbeat", "refresh the lease on a task you already hold")
)

type taskClaimOpts struct {
	lease   *int
	session *string
}

func bindTaskClaim(fs *flag.FlagSet) *taskClaimOpts {
	// posIntFlag, so --lease 0 and --lease -5 are parse errors rather than a
	// silent fall-through to the server's 900s default. That is what makes the 0
	// default unambiguous below: 0 can now only mean absent.
	return &taskClaimOpts{
		lease:   posIntFlag(fs, "lease", 0, "lease `SECONDS` before the claim lapses (default: the server's 900)"),
		session: actorSessionFlag(fs),
	}
}

func runTaskClaim(ctx context.Context, e *env, o *taskClaimOpts, sub, id string) error {
	cli, _, err := e.dial(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = cli.Close() }()
	call := map[string]any{"id": id, "session": *o.session}
	if *o.lease > 0 {
		call["lease_seconds"] = *o.lease
	}
	out, err := callTool(ctx, cli, "tasks_claim", call)
	if err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "task %s %s -> %s (lease until %s)\n",
		shortID(id), sub, str(out["status"]), str(out["lease_expires_at"]))
	return nil
}

// --- task release ---

var taskReleaseCmd = spec("task release", groupTasks,
	"release a task you hold, reopening it for another agent",
	exactly(1, "id"), bindTaskRelease, runTaskRelease)

type taskReleaseOpts struct {
	force   *bool
	session *string
}

func bindTaskRelease(fs *flag.FlagSet) *taskReleaseOpts {
	return &taskReleaseOpts{
		force: fs.Bool("force", false,
			"owner override: release the lock even if you do not hold it (routes through the console owner surface, not the agent claim path)"),
		session: actorSessionFlag(fs),
	}
}

func runTaskRelease(ctx context.Context, e *env, o *taskReleaseOpts, pos []string) error {
	id := pos[0]

	// --force is the owner override: it force-releases any holder's claim via the
	// console POST route (bearer-authenticated), which agents cannot reach. The
	// plain path is holder-checked: tasks_release only releases a claim you hold.
	if *o.force {
		cfg, err := e.loadConfig()
		if err != nil {
			return err
		}
		var out struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		}
		if err := consolePOST(cfg, "/console/tasks/"+id+"/release?format=json", &out); err != nil {
			return err
		}
		fmt.Fprintf(e.stdout, "task %s force-released -> %s\n", shortID(id), out.Status)
		return nil
	}

	cli, _, err := e.dial(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = cli.Close() }()
	out, err := callTool(ctx, cli, "tasks_release", map[string]any{"id": id, "session": *o.session})
	if err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "task %s released -> %s\n", shortID(id), str(out["status"]))
	return nil
}
