---
title: Tasks
description: The six task tools - the dependency-aware ready queue and lease-based claiming that lets parallel agents divide work safely.
generate: mcp-tools
tools:
  - tasks_add
  - tasks_update
  - tasks_ready
  - tasks_list
  - tasks_claim
  - tasks_release
---

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
   clocks](/concepts/tasks-and-plans/).
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
[Feeding a security monitor](/guides/security-monitor/#recording-commits).
