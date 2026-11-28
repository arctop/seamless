---
title: "Two agents, one plan step, no collision: what a lease actually buys you"
description: Two live Claude Code sessions pick the same task from one shared task queue. One claim wins, one bounces with the holder's name, and the second agent takes the next ready step - no duplicate work, no referee.
scene: coordination
order: 4
---

Run two coding agents against the same backlog and sooner or later they pick
the same task. Both reason correctly - the oldest ready step, the obvious
`TODO`, the relevant constraint - which is exactly why they converge, and you
get the same limiter built twice in two branches, or worse, interleaved into
one. Separate git worktrees do not prevent it: they keep the files apart, and
the two sessions still choose the same work. What prevents it is a shared
task queue the agents claim from, and the fix is an old one from distributed
systems: **claim with a lease**. A claim atomically moves a ready task to
`in_progress` with a named holder and an expiry; a second claim of the same
task does not half-succeed - it bounces, naming the holder, and the loser
picks the next ready step. No coordinator process, no human referee.

That race is exactly what the recorded timeline below shows. Two live Claude
Code agents were given the identical prompt - *pick up the next step of the
plan* - at the same moment, against one seeded [Seamless](/) instance. Both
see two claimable steps; both, independently, choose the rate-limiting step
for the same good reasons. Agent B's `tasks_claim` lands first. Agent A's
identical call comes back `task already claimed: held by "01KXP8FE…"` - and A
pivots to the metrics step without being told. Both steps ship. The
transcripts are unedited, merged onto one clock the way the claims actually
interleaved; the session ids are on the panes.

<!-- transcript -->

## What a lease actually buys you

The collision protection is not a lock, and the difference matters when the
workers are agents that can die mid-task:

- **Claiming is atomic.** `tasks_claim` moves a *ready* task - open, with
  every dependency done - to `in_progress` with a holder and a lease
  (15 minutes by default). There is no window where two callers both think
  they won; the loser's call fails, naming the holder.
- **A dead agent cannot strand work.** A lock needs an unlocker; a lease only
  needs a clock. If the holder crashes, its lease lapses and the step becomes
  claimable again. A live holder re-claims to refresh the lease - a heartbeat.
- **Release is tied to the work, not the process.** Finishing or dropping the
  task releases it, `tasks_release` releases it explicitly, and ending a
  session releases everything that session held.
- **The queue is dependency-aware.** Steps declare what blocks them, so
  "ready" already means "safe to start" - the race is only ever between tasks
  that genuinely could both proceed.

Two honest limits. A lease serializes *intent*, not file edits - both agents
here still edit the same repository, and you can see B reconcile with A's
changes in the fast-forward line; that remains git's job. And a claim is
advisory for humans: the console shows who holds what, but nothing stops you
from editing the code yourself.

The claim also outlives the moment. Claude Code's Agent Teams gives one team
of agents a shared task list with claiming and dependencies - first-party and
good - for the life of that team. The queue here is the layer underneath: the
plan, its steps, and their claims persist across sessions and days, and an
agent in a different client ([Codex CLI](/docs/codex-cli/), or any MCP client)
reads the same queue. [Tasks and plans](/docs/concepts/tasks-and-plans/) has
the full model; [coordinate agents](/docs/guides/coordinate-agents/) is the
working guide.

## How to reproduce this

Seed the fixture with the race flag, which leaves two steps claimable so the
collision can happen:

```sh
git clone https://github.com/arctop/seamless && cd seamless
go run ./cmd/demoseed -scenes -race -data /tmp/seamless-demo -repo /path/to/your/test/repo
SEAMLESS_DATA_DIR=/tmp/seamless-demo go run ./cmd/seamlessd serve
```

Then start two agent sessions in the mapped repo and give both the same
prompt at the same time. The bounce is deterministic - whoever's claim lands
second gets the holder's session id back. For a real setup, start at the
[quickstart](/docs/quickstart/).
