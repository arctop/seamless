# Coordinate multiple agents

> Fan-out over the ready queue, planner/executor splits via plan composition, shared-lab investigation, what happens when a claim holder dies, and the git hooks that check a repo lock.

Coordinating multiple coding agents with Seamless means pointing N workers -
Claude Code, Codex, or any MCP client - at one shared, dependency-aware task
queue with atomic lease-based claiming: every worker runs the same claim loop,
exactly one wins each task, and a dead worker's lease expires instead of
stranding its work. There is no scheduler process and no orchestrator - the
coordination is the workers cooperating over the queue.

One agent with memory is a better agent. Several agents with *shared* memory and
no coordination is a worse outcome than one - they duplicate work, contradict each
other's writes, and discover the same dead end in parallel.

This page is the part of Seamless that exists for the fleet: how N agents divide
work without colliding, and what happens when one of them dies holding something.

## Fan-out over the ready queue

The pattern is a seeder and N identical workers. The seeder decomposes the work
once:

```text
tasks_add title="extract the parser"          → 01K...A
tasks_add title="port callers"  depends_on=A  → 01K...B
tasks_add title="delete the old path" depends_on=B
```

`depends_on` is the whole schedule. A task is **ready** when no blocker is still
`open` or `in_progress`; either `done` or `dropped` unblocks a dependent, because
work abandoned on purpose should not wedge the queue forever. Cycles are rejected
at creation rather than discovered at scheduling time.

A schedule is corrected in place. When a blocker turns out to be the wrong task,
`tasks_update remove_depends_on=<wrong>` drops it and `add_depends_on=<right>`
adds the right one (`seam task unblock` and `seam task block` from a shell), so
the task keeps its id and nothing that cites it goes stale. Removing a task's
last unfinished blocker makes it ready; naming an id that is not a blocker
changes nothing and the answer says so.

Every worker then runs the same loop, and needs no knowledge of the others:

```text
Worker loop
session Establish identity The hook's session, or session_start : every claim names a live session.
tasks_ready Read actionable work Dependencies are already resolved.
tasks_claim Race atomically Win and work, or lose and refresh the ready list.
finish Close and hand off Update the task; session_end releases any claim still held.
Workers coordinate through shared state, so the planner never has to remain alive as a referee.
```

`tasks_claim` fails with *no active session to claim as* if the connection has no
session. That is not incidental strictness: the claim records **who** holds the
task, and a claim by nobody could never be released, reclaimed, or attributed. A
Claude Code or Codex worker needs no setup for this: its tool calls are
[bound to its own session](https://thereisnospoon.org/docs/concepts/sessions/#process-binding), so its claims
name it even with several workers in one repo.

### What the loser of a race sees

`tasks_ready` orders oldest-created first, ties broken by id. That order is stable
and identical for every worker - which means every worker tries the *same task
first*. Contention on the head of the queue is the designed behavior, not a
thundering-herd bug, because losing is cheap and unambiguous:

```text
task already claimed: task "01K7ABCD..." held by "01K7SESS..."
```

The holder is named as a **session id**, not an agent name. One claim landed; the
loser got a clean error, not a corrupted second claim.

What the loser does next matters more than the error. **Do not retry the same
id** - someone is working it, and a retry loop is just a slower way to do nothing.
**Do not walk to the next id in the list you already have** either: that list was
read before the race and is now stale in exactly the way that caused the
collision. Call `tasks_ready` again and claim from the fresh list. Two workers
that both do this converge on disjoint tasks within one round trip.

Claiming is a coordination signal, not a lock on the files. Nothing physically
stops an agent from editing code for a task it did not claim. The model works
because well-behaved agents do not collide - not because misbehaving ones cannot.

## Planner and executor, split by plan composition

Fan-out works when the work is a flat list. Real work has a design, and a design
that lives only in the planner's context dies when that agent does.

A [plan](https://thereisnospoon.org/docs/concepts/tasks-and-plans/) is not a primitive - it is everything keyed
by `plan:<slug>`. That is what makes the split possible:

| The planner writes | With |
|---|---|
| The narrative - the design and why it looks like that | `notes_create plan=<slug>` |
| Supporting context - the research behind it | More notes, same `plan=<slug>` |
| The steps, with their dependencies | `tasks_add plan=<slug> depends_on=...` |

The executors never need the planner. They read the narrative note, then work the
plan's queue:

```text
tasks_ready plan=<slug>   ─▶ that plan's claimable steps
tasks_claim <id>          ─▶ same race, same rules
```

**Plan steps are excluded from the default queue.** `tasks_ready` and `tasks_list`
skip them unless you pass `plan=`, so a twelve-step plan does not bury the handful
of tasks that are genuinely loose work. This is why a planner can decompose
aggressively without drowning every other agent on the machine.

The briefing surfaces each active plan as one rolled-up row rather than a step
list:

```text
Plans:
- marketing -- 2/3 done, 1 claimable, 0 in flight
(steps: tasks_ready plan=<slug>; claim: tasks_claim id=<task id>; attach work via the plan:<slug> tag)
```

That is enough for an arriving agent to decide whether to pick something up,
without spending briefing budget on steps it will not touch.

Claude Code's plan mode builds this composition automatically - plan-file saves
become notes, approval creates the tracking task. See
[Tasks & plans](https://thereisnospoon.org/docs/concepts/tasks-and-plans/).

## Shared-lab investigation

Some work does not decompose into tasks at all. Chasing an intermittent bug is N
agents trying things, and the expensive failure is two of them trying the same
thing an hour apart.

A lab is the shared record for one line of investigation:

```text
lab_open lab=dfu-timeout        ─▶ binds the lab; returns up to 10 recent trials
trial_record title="..." changes="..." expected="..." actual="..." outcome=fail
              metrics={"hz":497}
trial_query outcome=fail metrics_filter={"hz":497}
```

`lab_open` binds the lab to the connection the way `session_start` binds the
project, so `trial_record` inherits it. The context it returns on open is the
point: an agent joining an investigation already in progress sees what has been
tried before it tries anything.

Record failures, and record them with the `expected` and `actual` fields
populated. A trial that says what you thought would happen and what did is the
only kind another agent can reason from. `metrics` is an exact-match structured
filter, so "every trial where the rate was 497" is a query rather than a reading
exercise.

When the investigation resolves, **distill it into a memory** - a `gotcha` for the
trap, a `refuted` for the theory that looked right and wasn't. The lab is the
working record; it is not the conclusion, and nothing reads it into a briefing.

## Leases, crashes, and reclaiming

A claim stamps a lease - 900 seconds by default, overridable with
`lease_seconds`. Four rules cover the model:

1. **Claiming is atomic.** A compare-and-set: the write lands only if the task is
   claimable. The loser gets an error, never a second claim.
2. **Re-claiming refreshes the lease.** That is the heartbeat. An agent still
   working long work keeps its claim by claiming again - there is no separate
   heartbeat tool, and `seam task heartbeat` is this path under a clearer name.
3. **An expired lease is reclaimable.** Expiry is enforced **lazily, inside the
   claim** - there is no background sweeper watching leases.
4. **Closing frees it.** `tasks_release`, `tasks_update` to `done`/`dropped`, or
   `session_end`, which releases every claim the session still holds.

Rule 3 has a consequence worth stating plainly, because "reclaimable" is easy to
over-read: **an expired lease does not put the task back in `tasks_ready`.** The
task is still `in_progress`, and `tasks_ready` returns only `open` tasks. Nothing
re-queues it, because nothing is scanning for it.

So when an agent dies holding a task, two different clocks are running:

| Clock | Length | What it changes |
|---|---|---|
| **The lease** | `lease_seconds`, default 900s, at most 14400s | The task becomes stealable **by id** - a worker that knows the id can `tasks_claim` it and win. It is still not in the ready queue |
| **The session idle reap** | `gardener.session_idle_minutes` (45) after the last lease lapses, on the gardener's tick | The reaper expires the dead session and **releases its claims** - status back to `open`, and the task is genuinely back in the queue. A live claim keeps its session live, so the reap never comes while a lease runs |

The reaper is the backstop that makes crashes self-healing, and it is the only one
that re-queues. A clean `session_end` does the same thing immediately, which is
the real reason a fleet worker should always end its session rather than just
exiting.

One caveat that turns a self-healing system into a stuck one: **the reaper runs
inside the gardener's pass**. With `gardener.enabled` off, nothing reaps idle
sessions and nothing returns a dead agent's claims. See
[Troubleshooting](https://thereisnospoon.org/docs/guides/troubleshooting/).

Nobody wants to wait 45 minutes for an obvious corpse, so there is an owner
override - the console's **release lock** button, or `seam task release --force
<id>`. It force-releases any holder's claim regardless of the lease, and it is
deliberately **not on the MCP surface**: agents get the cooperative protocol, you
get the override.

## Commit-time lock checks {#commit-time-lock-checks}

A [repo lock](https://thereisnospoon.org/docs/concepts/repo-locks/) tells cooperating agents that a
repository's main branch, or its release tags, are being landed. Two git hooks
make git itself check the lock where a guarded ref moves:

- **reference-transaction** runs for every ref update in the checkout: a
  commit, a cherry-pick, `git merge --ff-only`, a rebase, a reset, a tag, and
  `git update-ref`.
- **pre-push** runs for the refs a push updates on the remote.

Both run `seam lock check`. It reads the updates git hands the hook and refuses
one that moves a ref one of the repository's locks guards, unless the caller
may move it:

| Caller | Lock held by the caller | Held by another session | Free |
|---|---|---|---|
| An agent session | moves the ref | refused | refused: take the lock first |
| Anyone else, such as the owner at a terminal | - | refused | moves the ref |

An agent session is one the daemon can name. `seam` sends the agent process
that ran git, as every `seam` call does ([process
binding](https://thereisnospoon.org/docs/concepts/sessions/#process-binding)), and the daemon answers whether
that process belongs to a live session and whether that session holds the
lock. A caller it binds to no session is treated as anyone else.

The lock is found the way `seam lock` finds it: by the checkout's origin first,
then its host and path. A second clone of the repository, on another machine,
is checked against the same lock. A push is checked against the lock of the
checkout it is pushed from, whatever URL it goes to.

The hooks need git 2.28 or later, `seam` on the PATH git runs them with, and
repo locks turned on in the server.

### Add the hooks to a repository

The reference-transaction hook (the Seamless repository commits this one as
`.githooks/reference-transaction`):

```sh
#!/bin/sh

[ "$1" = prepared ] || exit 0

updates=$(cat)
case $updates in
*" refs/"*) ;;
*) exit 0 ;;
esac

if ! command -v seam >/dev/null 2>&1; then
	echo "reference-transaction: seam is not on PATH, so the repository's locks were not checked" >&2
	exit 0
fi
if ! seam lock check --help >/dev/null 2>&1; then
	echo "reference-transaction: this seam has no lock check (it predates repo locks), so the repository's locks were not checked" >&2
	exit 0
fi

printf '%s\n' "$updates" | seam lock check --hook reference-transaction -- "$@"
status=$?
case $status in
0) exit 0 ;;
77) exit 1 ;;
esac
echo "reference-transaction: seam lock check exited $status, so the repository's locks were not checked" >&2
exit 0
```

The pre-push hook is the same script without the first line (git passes it no
state), with `--hook pre-push` and its own name in the messages. Make both
executable, then either:

- commit them under `.githooks/` and run `git config core.hooksPath .githooks`
  once per clone. git reads a relative path against each worktree's root, so
  every worktree runs its own checkout's copy; or
- copy them into `.git/hooks/`, which every worktree of the repository shares
  and which is never committed.

In the Seamless repository, `make install-git-hooks` sets `core.hooksPath` to
`.githooks`, which also runs the pre-commit check.

The script refuses only when `seam lock check` exits 77. Every other outcome
lets the update through, so a broken check never wedges git:

- with no `seam` on the PATH, or a `seam` that predates `lock check`, the hook
  prints a warning;
- with the daemon down, or silent for 10 seconds, `seam lock check` prints a
  warning and exits 0;
- with repo locks turned off in the server, or no lock declared for the
  repository, it exits 0 and prints nothing.

### What a refusal means

```text
error: refused: this update moves refs/heads/main, which LOCK example-app main guards, and session cc/1a2b3c holds the lock (since 2026-11-15T18:00:00Z, until 2026-11-15T18:15:00Z).
       Wait for its release, or queue for the lock: seam lock acquire --wait
       --no-verify does not skip the reference-transaction hook, which runs this check on every ref update. The owner's override is a force-release: seam lock release --force, or the console's Locks page.
fatal: in 'prepared' phase, update aborted by the reference-transaction hook
```

- **Held by another session.** Wait for its release, or queue behind the
  holder with `seam lock acquire --wait`. The holder is named only where you
  may see that session; otherwise the line says "another session".
- **Free, and this session does not hold it.** An agent session lands only
  while it holds the lock: run `seam lock acquire`, then land.
- **A lapsed hold.** The holder's lease ran out, so the lock is free again:
  acquire it before you land.
- **A refused push** prints the same, then `error: failed to push some refs`.
- **After a refused `git merge --ff-only`**, or the merge step of `git pull`,
  git has already updated the index and the working tree but leaves the branch
  where it was. `git reset --merge` puts the files back.

`--no-verify` does not skip reference-transaction. The owner's override is a
force-release, on the console's Locks page or with `seam lock release --force`.
Like the lock itself, the hooks keep cooperating agents in step; they are not
access control.

### What is never checked

- **A ref outside `refs/`:** HEAD, ORIG_HEAD, AUTO_MERGE, FETCH_HEAD and a
  worktree's own HEAD. A commit on a detached HEAD in your own worktree never
  calls `seam lock check`, let alone the daemon.
- **git's own namespaces:** `refs/remotes/`, `refs/bisect/`, `refs/rewritten/`,
  `refs/worktree/`, `refs/prefetch/` and `refs/stash`. A lock may name a ref
  there, but no hook checks it.
- **An update that leaves a ref where it is.**
- **What a fetch brings:** its remote-tracking refs and its tags, which the
  remote already has. A fetch that moves a local branch with an explicit
  refspec (`git fetch origin main:main`) is checked. `seam lock check` reads
  which git command ran the hook from the process table on macOS and Linux;
  elsewhere, a transaction that updates a remote-tracking ref is taken for a
  fetch.
- **clone, pack-refs, gc and maintenance:** they bring refs here or rewrite how
  refs are stored, without moving one.
- **A declaration still waiting for the owner's approval.**
- **The other states of a transaction.** git runs reference-transaction as a
  transaction is preparing, once it is prepared, and once it is committed or
  aborted. Only the prepared state checks.

### Hooks off

`seam doctor` adds a `repo_lock_hooks` line when repo locks are on and the
working directory's repository declares a lock. The line says `ok` when the
checkout runs the hook, and is a `note` saying **hooks off** when it does not. A
session that starts in that checkout sees the same thing at the head of its
briefing, as a `LOCK hooks off` line (see
[repo locks in the briefing](https://thereisnospoon.org/docs/reference/hooks/#repo-locks)).

It finds the hooks folder as git does. `core.hooksPath` is read from your
global git config, the repository's config and, with `extensions.worktreeConfig`
on, the worktree's `config.worktree`; the last one to set it wins. Otherwise the
folder is the repository's own `.git/hooks`. The `reference-transaction` file
there must be executable, with a line, not a comment, that runs `seam lock
check`.

It does not follow the system git config, `include` and `includeIf`, a `-c`
on one git command, or the config-defined hooks (`hook.<name>.command`) that
newer gits also run. A checkout set up through one of those shows hooks off
even when git runs the check.

## What you see while it runs

The console is read-mostly and live over SSE - it is how you watch a fleet without
being in its loop.

| Screen | What it answers |
|---|---|
| `/console/tasks` | Ready, In progress, Blocked (with each task's blockers), Closed - the queue at a glance |
| A task's detail panel | Who claims it, whether the lease is **live** or **expired**, and the release-lock button |
| `/console/sessions` | Which agents are alive, and each one's claimed tasks with remaining lease |
| A project's detail | The plan timeline - each step's status, holder, and lease |
| `/console/plans` | Captured plans with status, iteration, and task progress |

The same view without a browser:

```bash
seam ready --blocked                 # the queue plus what is blocking what
seam task list --status in_progress  # what is claimed right now
seam sessions --status active        # who is alive
```

A task sitting in **In progress** with an **expired** lease is the signature of a
crashed holder. If the gardener is running, leave it - the reaper will return it.
If you need it now, release the lock.

## One more collision to know about

Coordination has a scope failure mode, not just a queue one. When several agents
are live in **different repos** and one of them writes durable knowledge without a
bound session, Seamless cannot tell which project the write belongs to - and
refuses it as ambiguous rather than inheriting from whichever ambient session was
most recent. That refusal is the feature: the alternative is one agent's memory
silently landing in another agent's project.

Claude Code and Codex agents are bound to their own sessions automatically, so
this bites hookless clients and agents on an older `seam`. Bind the session
([Integrate your agent](https://thereisnospoon.org/docs/guides/integrate-your-agent/)), or pass `project`
explicitly. See [Projects & scope](https://thereisnospoon.org/docs/concepts/projects/).
