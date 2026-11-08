# Tasks

> The six task tools - the dependency-aware ready queue and lease-based claiming that lets parallel agents divide work safely.

Tasks are how agents hand work to each other. A task is **ready** when it has no
unfinished blocker; `tasks_ready` returns exactly those, so an agent asking "what
can I do now?" never has to reason about the dependency graph itself.

## How claiming works

Claiming is the concurrency control. Two agents that both see the same ready task
will both try to take it, and exactly one wins:

1. `tasks_claim` atomically moves a ready task to `in_progress` and stamps a
   lease (default 900 seconds). The loser gets an error naming the holder.
2. Re-claiming a task you already hold **refreshes** the lease - that is the
   heartbeat for long work.
3. An **expired** lease is reclaimable *by id* - but the task stays
   `in_progress`, so `tasks_ready` does not show it. Only releasing it (by hand,
   or by the gardener's session reaper expiring the dead holder's session) sets
   the status back to `open` and returns it to the queue. See [the two
   clocks](https://thereisnospoon.org/docs/concepts/tasks-and-plans/).
4. `tasks_release`, closing the task (`tasks_update` to `done`/`dropped`), or
   `session_end` frees the claim.

A lease is not a lock on the files - it is a coordination signal between
cooperating agents. Nothing stops a determined agent from working on a task it
did not claim; the point is that well-behaved agents do not collide.

## Plan steps are not queue items

A task created with `plan=<slug>` is a **step of a plan**, and is excluded from
the default `tasks_ready` and `tasks_list`. That keeps a twelve-step plan from
burying the handful of tasks that are actually loose work. Pass `plan=<slug>` to
either tool to see that plan's steps instead.

## Results and refusal modes

Every successful task mutation returns the resulting task row, including its id,
status, dependency state, claim holder, and lease fields when present.

No task answer names a task its caller may not read: one in a confidential
project, read from a session bound elsewhere, or in a locked project without a
lease. Where such a task blocks another, `tasks_ready` lists the blocker as
`{"withheld": "..."}` in its place, so the count stays true; `depends_on` lists
only the ids the caller may read, and `depends_on_withheld` counts the rest; and
the claim's "task blocked" refusal names the readable blockers and counts the
others. With nothing isolated or locked, every answer reads as it always has.

The owner's own connection at a locked project it holds no lease on is told
which one: the withheld entry reads `a task in locked project acme-finance
blocks this one: <remedy>`, and the claim refusal names the project, with a
`Remedy:` line. Every other caller, and every blocker an isolation fence alone
withholds, gets the generic words.

| Call | Refuses when |
|---|---|
| `tasks_add` | A blocker is missing or the new dependency would create a cycle |
| `tasks_update` | Nothing was supplied to change, a new dependency is invalid, a `commits` entry is not a full commit id, or another live session holds the task |
| `tasks_claim` | The task is blocked, closed, or held by another live claimant; each error names the actual cause |
| `tasks_release` | The acting session is not the holder; force release exists only on the owner console/CLI surface |

An expired claim is not reported as live. Claiming that task by id succeeds at
the exact lease boundary, even while its status remains `in_progress`.

## Commit records

On a server that records commits (`commits.record`, off by default),
`tasks_update` also takes `commits`: the full ids of the commits that did the
task's work, recorded on the task in the same transaction as the rest of the
call and behind the same claim lock. Off, the argument does not exist. See
[Record commits for an integrity monitor](https://thereisnospoon.org/docs/guides/commit-records/).

## tasks_add {#tasks_add}

Add a task to the dependency-aware ready queue. depends_on lists task ids that must finish first (done or dropped unblocks); each must exist and must not create a cycle. The task is 'ready' once it has no open/in_progress blocker.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `title` | string | **yes** | short task title |
| `body` | string | no | optional details / acceptance criteria (aliases: content, text) |
| `depends_on` | array | no | task ids this task is blocked by (a comma-separated string is also accepted) |
| `plan` | string | no | optional plan slug (plan:&lt;slug&gt; convention) that composes this task as a step of a plan. Plan steps are excluded from the default ready-queue and surfaced under the plan filter. |
| `project` | string | no | project slug; defaults to the bound/ambient session's project. An unknown slug CREATES that project -- naming a new one is normal and never an error. Pass project=global ONLY for knowledge that belongs in EVERY project's briefing; it is not a neutral default. With no session and no explicit project the call is rejected as ambiguous. A session bound to a confidential or sealed project can write ONLY into that project. |
| `session` | string | no | the session to act as: the name session_start returned, the cc/&lt;id&gt; or cx/&lt;id&gt; on your briefing's 'Seam session' line, or a session ULID. It scopes and credits this call. Defaults to this connection's session; pass it on every call when session_start reports the connection as stateless |

## tasks_update {#tasks_update}

Update a task: change status (open|in_progress|done|dropped), edit title/body, or add dependencies. Moving to done/dropped closes it and unblocks its dependents. A task another session holds via a live claim is locked to its holder: updating it fails with 'already claimed' until the lease lapses or the holder releases it.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `id` | string | **yes** | task id |
| `add_depends_on` | array | no | task ids to add as blockers (a comma-separated string is also accepted) |
| `body` | string | no | new body (aliases: content, text) |
| `commits` | array | no | Full ids of the commits that did this task's work, recorded on the task for an integrity monitor that matches a branch's commits to the work that made them: the 40- or 64-character hex id git prints (git rev-parse HEAD), never an abbreviation. Recording them is an update like any other: a task another session holds refuses it. At most 256 per call; repeats are ignored. Offered only while this server records commits (commits.record) |
| `project` | string | no | reassign the task to another project slug (used when a split moves a project's open work to a child) |
| `session` | string | no | the acting agent's session (the cc/&lt;id&gt; or cx/&lt;id&gt; on your briefing's 'Seam session' line, or a session name); needed only to mutate a task you hold a live claim on when several agents are active |
| `session_id` | string | no | the acting agent's session ULID; takes precedence over session and the bound session |
| `status` | string | no | new status. One of: `open`, `in_progress`, `done`, `dropped`. |
| `title` | string | no | new title |

## tasks_ready {#tasks_ready}

List the actionable (ready) tasks for a project -- open tasks with no unfinished blocker -- oldest first, plus the blocked tasks with their still-open blockers. By default plan-step tasks are excluded; pass plan=&lt;slug&gt; to list that plan's steps instead.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `plan` | string | no | optional plan slug: return that plan's ready/blocked step tasks instead of the default (non-plan) queue |
| `project` | string | no | project slug; defaults to the bound session's project |
| `session` | string | no | the session to act as: the name session_start returned, the cc/&lt;id&gt; or cx/&lt;id&gt; on your briefing's 'Seam session' line, or a session ULID. It scopes and credits this call. Defaults to this connection's session; pass it on every call when session_start reports the connection as stateless |

## tasks_list {#tasks_list}

List a project's tasks, optionally filtered by status, newest first. By default plan-step tasks are excluded; pass plan=&lt;slug&gt; to list that plan's steps instead. Pass id=&lt;task id&gt; to load a single task by its globally-unique id (a direct lookup that ignores project/status/plan and needs no session scope).

| Parameter | Type | Required | Description |
|---|---|---|---|
| `id` | string | no | load exactly one task by its globally-unique id; when set, project/status/plan are ignored and the response's tasks array holds just that task |
| `plan` | string | no | optional plan slug: list that plan's step tasks instead of the default (non-plan) tasks |
| `project` | string | no | project slug; defaults to the bound session's project |
| `session` | string | no | the session to act as: the name session_start returned, the cc/&lt;id&gt; or cx/&lt;id&gt; on your briefing's 'Seam session' line, or a session ULID. It scopes and credits this call. Defaults to this connection's session; pass it on every call when session_start reports the connection as stateless |
| `status` | string | no | optional status filter. One of: `open`, `in_progress`, `done`, `dropped`. |

## tasks_claim {#tasks_claim}

Atomically claim a task for the current session, moving it to in_progress with a lease. A refused claim names its cause: held by another live claim ('task already claimed'), waiting on unfinished dependencies ('task blocked', naming the blockers -- finish those first), or already done/dropped ('task closed'). Re-claiming a task you already hold refreshes (heartbeats) the lease; a task whose lease has expired can be reclaimed. Release it with tasks_release or by closing it (tasks_update done/dropped); session_end releases all of a session's claims.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `id` | string | **yes** | task id to claim |
| `lease_seconds` | number | no | lease duration in seconds before the claim lapses and the task becomes reclaimable (default 900) |
| `session` | string | no | the acting agent's session: the cc/&lt;id&gt; or cx/&lt;id&gt; on your briefing's 'Seam session' line, another session name, or a session ULID. Defaults to this connection's session -- bound automatically for Claude Code and Codex agents -- then a sole active ambient. Pass it when a call reports the agent as ambiguous: the bare call then fails rather than guesses. |
| `session_id` | string | no | the acting agent's session ULID; takes precedence over session and the bound session |

## tasks_release {#tasks_release}

Release a task the current session holds, reopening it (status back to open, claim cleared) so another agent can claim it. Only the current holder may release.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `id` | string | **yes** | task id to release |
| `session` | string | no | the acting agent's session: the cc/&lt;id&gt; or cx/&lt;id&gt; on your briefing's 'Seam session' line, another session name, or a session ULID. Defaults to this connection's session -- bound automatically for Claude Code and Codex agents -- then a sole active ambient. Pass it when a call reports the agent as ambiguous: the bare call then fails rather than guesses. |
| `session_id` | string | no | the acting agent's session ULID; takes precedence over session and the bound session |
