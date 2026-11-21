# Tasks

> The six task tools - the dependency-aware ready queue and lease-based claiming that lets parallel agents divide work safely.

Tasks are how agents hand work to each other. A task is **ready** when it has no
unfinished blocker and no open gate; `tasks_ready` returns exactly those, so an
agent asking "what can I do now?" never has to reason about the dependency graph
itself.

## How claiming works

Claiming is the concurrency control. Two agents that both see the same ready task
will both try to take it, and exactly one wins:

1. `tasks_claim` atomically moves a ready task to `in_progress` and stamps a
   lease: 900 seconds by default, and `lease_seconds` may ask for up to 14400
   (four hours). A longer lease is refused, and the refusal names the ceiling.
   The loser gets an error naming the holder.
2. Re-claiming a task you already hold **refreshes** the lease - that is the
   heartbeat for long work. While its lease is live, a claim also keeps its
   session live: the idle reaper does not end a session that holds one, however
   long its work goes quiet (see [Liveness](https://thereisnospoon.org/docs/concepts/sessions/#liveness)).
3. An **expired** lease is reclaimable *by id* - but the task stays
   `in_progress`, so `tasks_ready` does not show it. Only releasing it (by hand,
   or by the gardener's session reaper expiring the holder's session once none
   of its claims is live) sets the status back to `open` and returns it to the
   queue. See [the two clocks](https://thereisnospoon.org/docs/concepts/tasks-and-plans/).
4. `tasks_release`, closing the task (`tasks_update` to `done`/`dropped`), or
   `session_end` frees the claim.
5. A session that **loses** a claim it held - another session claims the task
   after its lease lapsed, the owner releases it, or the reaper releases it from
   the idle session after the lease lapsed - is told once. Its next tool result
   carries a `Claim lost:` notice as an extra text item after the answer: the
   task, when, the session that took it when this one may see that session
   (`another session` otherwise), and what to do. Through the hooks the same
   notice can lead the next prompt or session-start injection instead;
   whichever comes first carries it. A call from the `seam` CLI, which prints
   only the answer, never carries one.

A lease is not a lock on the files - it is a coordination signal between
cooperating agents. Nothing stops a determined agent from working on a task it
did not claim; the point is that well-behaved agents do not collide.

## Plan steps are not queue items

A task created with `plan=<slug>` is a **step of a plan**, and is excluded from
the default `tasks_ready` and `tasks_list`. That keeps a twelve-step plan from
burying the handful of tasks that are actually loose work. Pass `plan=<slug>` to
either tool to see that plan's steps instead.

`tasks_list` lists; it does not search. A `query` holding exactly one task id
is read as `id=<that id>`: the direct lookup, which ignores project, status and
plan. Any other `query` is refused with a pointer to `recall query=<text>
scope=tasks`.

## Gates

A task that waits on a person, or on an event outside Seamless, carries a gate
([Tasks & plans](https://thereisnospoon.org/docs/concepts/tasks-and-plans/#gates) explains the two forms):

- `tasks_add` and `tasks_update` take `gate_who` and `gate_what` (both, one line
  each; `gate_who` at most 100 characters, the others 500), plus the optional
  `gate_decision` and `gate_ready_when`, and `kind`: `step` (the default) or
  `gate`. A task of kind `gate` must carry a gate. On `tasks_update`, setting any
  `gate_*` field (re)opens the gate as it then reads, keeping the fields the
  call does not name; setting `gate_who` and `gate_what` to empty strings
  removes a step's gate without clearing it.
- `tasks_update gate_clear=true` clears an open gate, with an optional
  `gate_note`. It records when, the acting session (`cleared_by`) and the note.
  A step keeps its status; a task of kind `gate` closes as `done` (a
  `gate_clear` that also asks for another status is refused). Reopening a
  cleared gate task opens its gate again.
- Every task answer carries the gate as `gate` - `who`, `what`, `open`, the
  optional lines, and once cleared `cleared_at`, `cleared_by` and `note` - and
  a gate task says `"kind": "gate"`. A task without a gate carries neither key.
- `tasks_ready` adds `gated` (steps whose own gate is open, or that an open
  gate blocks) and `gates` (gate tasks open or in progress, ordered by who) to
  its answer when either holds anything. Their entries come without `body`, and
  each lists its unfinished blockers; a blocker that is a gate carries its
  `kind` and `gate`. `agent=true` answers `ready` alone with `omitted: {blocked,
  gated, gates}` counts; `who=<who>` answers only the `gates` and `gated` tasks
  waiting on that person (case-insensitive; `reviewer` matches `reviewer:dana`).
  The two filters cannot be combined, and a blank `who` is refused.
- The `task.transition` record of a call that set, cleared or removed a gate
  carries `gate: set|cleared|removed`, and of one that changed the kind,
  `kind`.

A caller outside a confidential project that writes one of its tasks gets no
gate back, like the rest of the task's content, and a `task gated` claim
refusal names no one.

## Changing a task's blockers

`tasks_update` edits an existing task's schedule: `add_depends_on` adds blockers
(each must exist and must not create a cycle) and `remove_depends_on` removes
them, so a blocker that turned out to be the wrong task is dropped without
re-creating the task under a new id - its dependents, and every note that cites
it, keep the id they have. Removing a task's last unfinished blocker makes it
ready. One call may not both add and remove the same id.

The answer lists the ids it removed under `removed_depends_on`. An id that is
not one of the task's blockers - another task, or no task at all - changes
nothing and is listed under `not_depends_on`; a call whose only ask was such
removals writes nothing, not even the task's `updated_at`. A removal is recorded
in the task's history as a `task.transition` event under the acting session,
with `removed_depends_on` beside `to` and `from` - every `tasks_update` event
carries `from`, the status before the call, so an edit reads as an edit rather
than a status move - and it never changes the task's `updated_account`. It is behind the
same claim lock as every other update.

A blocker the caller may not reach - under `peer_auth`, a task in a project the
connection does not reach, the rule `add_depends_on` applies - is not one it can
remove, and is listed under `not_depends_on` exactly as an id that names nothing
would be. A caller that may write the task but not read it (outside a
confidential project) may remove its blockers, but its answer is withheld like
any other, with neither list: which ids were blockers is that project's
dependency graph.

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
| `tasks_add` | A blocker is missing, the new dependency would create a cycle, or the gate breaks the [gate rules](#gates) |
| `tasks_update` | Nothing was supplied to change, a new dependency is invalid, one id is both added and removed, a `commits` entry is not a full commit id, the gate would break the [gate rules](#gates), one call both edits and clears the gate, `gate_clear` finds no open gate, `gate_note` comes without `gate_clear`, or another live session holds the task; with `isolation.requests` on, a [receipt](#receipts) that breaks its rule |
| `tasks_claim` | The task is blocked, gated (a gate task, or a step whose own gate is open: `task gated`, naming who and what), closed, or held by another live claimant; each error names the actual cause. Also `lease_seconds` that is not positive, or above the 14400-second ceiling (the refusal names it) |
| `tasks_release` | The acting session is not the holder; force release exists only on the owner console/CLI surface |

An expired claim is not reported as live. Claiming that task by id succeeds at
the exact lease boundary, even while its status remains `in_progress`.

## Commit records

On a server that records commits (`commits.record`, off by default),
`tasks_update` also takes `commits`: the full ids of the commits that did the
task's work, recorded on the task in the same transaction as the rest of the
call and behind the same claim lock. Off, the argument does not exist. See
[Feeding a security monitor](https://thereisnospoon.org/docs/guides/security-monitor/#recording-commits).

## Receipts

With `isolation.requests` on, a conversation the owner has
[moved](https://thereisnospoon.org/docs/concepts/project-isolation/#moves) into another project can no longer
write where it worked before, so a task its pre-move session claimed there
would stay in progress for good. A **receipt** closes it: `tasks_update` with
`status` `done` or `dropped` on a task whose claim, live or lapsed, that
pre-move session still holds. It may name one `reference`, an item in the new
home written `<project>:<id>` (a memory, note, task, trial or session there),
and nothing else: a title, body, project, dependency, kind, gate or commit is
refused with the rule, and so is a reference that names no item in the home. It sets the
status alone, as that pre-move session, and releases the claim. The task's
`task.transition` record carries `receipt` and `reference` beside `to` and
`from`, and the answer carries `receipt: true`. A conversation whose home is
sealed sends none. Off, the argument does not exist. See
[Project isolation](https://thereisnospoon.org/docs/concepts/project-isolation/#receipts).

## tasks_add {#tasks_add}

Add a task to the dependency-aware ready queue. depends_on lists task ids that must finish first (done or dropped unblocks); each must exist and must not create a cycle. The task is 'ready' once it has no open/in_progress blocker and no open gate. A task that waits on a person or an event outside Seamless carries a gate: gate_who and gate_what on the step itself, or a task of kind gate that the waiting steps depend on, so one gate holds many steps and clearing it frees them all.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `title` | string | **yes** | short task title |
| `body` | string | no | optional details / acceptance criteria (aliases: content, text) |
| `depends_on` | array | no | task ids this task is blocked by (a comma-separated string is also accepted) |
| `gate_decision` | string | no | optional: the open question the person answers, one line |
| `gate_ready_when` | string | no | optional: when the task is ready, one line |
| `gate_what` | string | no | what must happen, one line (with gate_who) |
| `gate_who` | string | no | who must act before this task can go ahead: owner, reviewer:&lt;name&gt;, sitting:&lt;name&gt;, hardware, sealed:&lt;project&gt;, or free text (one line). With gate_what it gives the task a gate: a step whose gate is open is not ready, and tasks_ready lists it under gated |
| `kind` | string | no | step (the default: work an agent claims and finishes) or gate (a person's action, or an event outside Seamless, that the tasks depending on it wait for: never ready and never claimed; gate_who and gate_what describe it, and clearing it closes it and unblocks its dependents). One of: `step`, `gate`. |
| `plan` | string | no | optional plan slug (plan:&lt;slug&gt; convention) that composes this task as a step of a plan. Plan steps are excluded from the default ready-queue and surfaced under the plan filter. |
| `project` | string | no | project slug; defaults to the bound/ambient session's project. An unknown slug CREATES that project -- naming a new one is normal and never an error. Pass project=global ONLY for knowledge that belongs in EVERY project's briefing; it is not a neutral default. With no session and no explicit project the call is rejected as ambiguous. A session bound to a confidential or sealed project can write ONLY into that project. |
| `session` | string | no | the session to act as: the name session_start returned, the cc/&lt;id&gt; or cx/&lt;id&gt; on your briefing's 'Seam session' line, or a session ULID. It scopes and credits this call. Defaults to this connection's session; pass it on every call when session_start reports the connection as stateless |

## tasks_update {#tasks_update}

Update a task: change status (open|in_progress|done|dropped), edit title/body, add or remove dependencies, or set, edit or clear its gate. body replaces the WHOLE body; to add to it -- a finding, a note, progress -- pass body_append instead, which adds a UTC-timestamped line and keeps everything already there. The body a change replaces or appends to is kept, with when and by which session (the newest 20 per task): tasks_list id=&lt;id&gt; body_revisions=true reads them back. Moving to done/dropped closes it and unblocks its dependents; removing its last unfinished blocker makes it ready. Removing an id that is not one of its blockers changes nothing and is named back in not_depends_on; the ids removed are in removed_depends_on. Setting any gate_* field (re)opens the gate as it then reads; gate_clear=true clears an open gate once the person has acted, recording when, by this session, and gate_note: a step keeps its status and becomes ready once nothing else holds it, and a task of kind gate closes as done, which unblocks every task that depends on it. A task another session holds via a live claim is locked to its holder: updating it fails with 'already claimed' until the lease lapses or the holder releases it.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `id` | string | **yes** | task id |
| `add_depends_on` | array | no | task ids to add as blockers (a comma-separated string is also accepted) |
| `body` | string | no | new body, replacing the WHOLE body (aliases: content, text); the body it replaces is kept as a revision. To add to the body instead, use body_append |
| `body_append` | string | no | text to add to the end of the body as a UTC-timestamped line, the line notes_append adds to a note; everything already in the body stays, and the body before it is kept as a revision. Not with body |
| `commits` | array | no | Full ids of the commits that did this task's work, recorded on the task for an integrity monitor that matches a branch's commits to the work that made them: the 40- or 64-character hex id git prints, never an abbreviation (git rev-parse &lt;short id&gt; expands one); a short id here refuses the whole call. Recording them is an update like any other: a task another session holds refuses it. At most 256 per call; repeats are ignored. Offered only while this server records commits (commits.record) |
| `gate_clear` | boolean | no | true: clear the task's open gate now that the person has acted (not with the gate_* edits) |
| `gate_decision` | string | no | optional: the open question the person answers, one line |
| `gate_note` | string | no | optional, with gate_clear: what the person decided or did, kept with the cleared gate |
| `gate_ready_when` | string | no | optional: when the task is ready, one line |
| `gate_what` | string | no | what must happen, one line (with gate_who) |
| `gate_who` | string | no | who must act before this task can go ahead: owner, reviewer:&lt;name&gt;, sitting:&lt;name&gt;, hardware, sealed:&lt;project&gt;, or free text (one line). With gate_what it gives the task a gate: a step whose gate is open is not ready, and tasks_ready lists it under gated |
| `kind` | string | no | step (the default: work an agent claims and finishes) or gate (a person's action, or an event outside Seamless, that the tasks depending on it wait for: never ready and never claimed; gate_who and gate_what describe it, and clearing it closes it and unblocks its dependents). One of: `step`, `gate`. |
| `project` | string | no | reassign the task to another project slug (used when a split moves a project's open work to a child) |
| `reference` | string | no | A receipt's one reference, to an item in this conversation's home, written &lt;project&gt;:&lt;id&gt; -- the home's slug and the id of a memory, note, task, trial or session there. Once the owner has moved this conversation into another project, it closes a task it claimed before the move with a receipt: tasks_update with status done or dropped and nothing else, plus, optionally, this reference, which the task's history records. A receipt carries no title, body, project, dependencies or commits, and a conversation whose home is sealed sends none. Offered only while isolation.requests is on |
| `remove_depends_on` | array | no | task ids to remove as blockers (a comma-separated string is also accepted); an id that is not a blocker of this task is a no-op, named back in not_depends_on |
| `session` | string | no | the acting agent's session (the cc/&lt;id&gt; or cx/&lt;id&gt; on your briefing's 'Seam session' line, or a session name); needed only to mutate a task you hold a live claim on when several agents are active |
| `session_id` | string | no | the acting agent's session ULID; takes precedence over session and the bound session |
| `status` | string | no | new status. One of: `open`, `in_progress`, `done`, `dropped`. |
| `title` | string | no | new title |

## tasks_ready {#tasks_ready}

List the actionable (ready) tasks for a project -- open tasks with no unfinished blocker and no open gate -- oldest first, plus the blocked tasks with their still-open blockers. Work that waits on people is listed apart, without bodies (tasks_list id=&lt;id&gt; loads one), and only when there is some: gated holds the steps whose own gate is open or that an open gate blocks, and gates the tasks of kind gate, ordered by who must act -- the people's queue. agent=true answers ready alone, with the other groups' sizes under omitted; who=&lt;who&gt; answers only the gates and gated tasks waiting on that person. By default plan-step tasks are excluded; pass plan=&lt;slug&gt; to list that plan's steps instead.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `agent` | boolean | no | optional: true answers only the tasks an agent can work now (ready), with the sizes of the groups left out |
| `plan` | string | no | optional plan slug: return that plan's ready/blocked step tasks instead of the default (non-plan) queue |
| `project` | string | no | project slug; defaults to the bound session's project |
| `session` | string | no | the session to act as: the name session_start returned, the cc/&lt;id&gt; or cx/&lt;id&gt; on your briefing's 'Seam session' line, or a session ULID. It scopes and credits this call. Defaults to this connection's session; pass it on every call when session_start reports the connection as stateless |
| `who` | string | no | optional: only the gates and gated tasks waiting on this person, e.g. owner; a class before a colon matches every name in it (reviewer matches reviewer:dana) |

## tasks_list {#tasks_list}

List a project's tasks, optionally filtered by status, newest first. A task that waits on people carries its gate (who, what, and whether it is still open), and a task of kind gate says so. By default plan-step tasks are excluded; pass plan=&lt;slug&gt; to list that plan's steps instead. Pass id=&lt;task id&gt; to load a single task by its globally-unique id (a direct lookup that ignores project/status/plan and needs no session scope). With id, body_revisions=true also answers the task's earlier bodies under body_revisions, newest first: each body a tasks_update replaced or appended to, with when and by which session.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `body_revisions` | boolean | no | optional, with id: true adds body_revisions to the task, its earlier bodies newest first, each with revision, body, change (replace or append), replaced_at and replaced_by (the session's id); a task keeps its newest 20 |
| `id` | string | no | load exactly one task by its globally-unique id; when set, project/status/plan are ignored and the response's tasks array holds just that task |
| `plan` | string | no | optional plan slug: list that plan's step tasks instead of the default (non-plan) tasks |
| `project` | string | no | project slug; defaults to the bound session's project |
| `session` | string | no | the session to act as: the name session_start returned, the cc/&lt;id&gt; or cx/&lt;id&gt; on your briefing's 'Seam session' line, or a session ULID. It scopes and credits this call. Defaults to this connection's session; pass it on every call when session_start reports the connection as stateless |
| `status` | string | no | optional status filter. One of: `open`, `in_progress`, `done`, `dropped`. |

## tasks_claim {#tasks_claim}

Atomically claim a task for the current session, moving it to in_progress with a lease: 900 seconds unless lease_seconds sets another, at most 14400 seconds (4 hours). While its lease is live a claim keeps its session live: the idle reaper does not end a session that holds one, however long its work goes quiet. A refused claim names its cause: held by another live claim ('task already claimed'), waiting on unfinished dependencies ('task blocked', naming the blockers -- finish those first), waiting on people ('task gated': a task of kind gate is never claimed, and a step whose own gate is open waits until it is cleared), or already done/dropped ('task closed'). Re-claiming a task you already hold refreshes (heartbeats) the lease; a task whose lease has expired can be reclaimed, and a session that loses a claim that way, or by the owner's release, is told once, in its next Seamless tool result or prompt. Release it with tasks_release or by closing it (tasks_update done/dropped); session_end releases all of a session's claims.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `id` | string | **yes** | task id to claim |
| `lease_seconds` | number | no | lease duration in seconds before the claim lapses and the task becomes reclaimable (default 900, at most 14400) |
| `session` | string | no | the acting agent's session: the cc/&lt;id&gt; or cx/&lt;id&gt; on your briefing's 'Seam session' line, another session name, or a session ULID. Defaults to this connection's session -- bound automatically for Claude Code and Codex agents -- then a sole active ambient. Pass it when a call reports the agent as ambiguous: the bare call then fails rather than guesses. |
| `session_id` | string | no | the acting agent's session ULID; takes precedence over session and the bound session |

## tasks_release {#tasks_release}

Release a task the current session holds, reopening it (status back to open, claim cleared) so another agent can claim it. Only the current holder may release.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `id` | string | **yes** | task id to release |
| `session` | string | no | the acting agent's session: the cc/&lt;id&gt; or cx/&lt;id&gt; on your briefing's 'Seam session' line, another session name, or a session ULID. Defaults to this connection's session -- bound automatically for Claude Code and Codex agents -- then a sole active ambient. Pass it when a call reports the agent as ambiguous: the bare call then fails rather than guesses. |
| `session_id` | string | no | the acting agent's session ULID; takes precedence over session and the bound session |
