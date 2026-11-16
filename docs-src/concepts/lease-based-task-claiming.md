---
title: Lease-based task claiming
description: The coordination primitive that lets parallel AI agents share one task queue - an atomic claim with an expiry, so two agents never work the same task and a crashed agent never strands one.
---

Lease-based task claiming is the coordination primitive that lets multiple AI
agents work one shared task queue without duplicating work: an agent *claims* a
task atomically, the claim carries a *lease* - an expiry timestamp - and a
task whose lease has lapsed is claimable again. Two agents that reach for the
same task cannot both win: the second claim fails, naming the holder. An agent
that crashes mid-task cannot strand it: when its lease runs out, the task
returns to the pool. No coordinator process, no human referee.

The lease is the part that matters for agents specifically. A plain lock
assumes its holder will live to unlock it; agents get killed, hit context
limits, and lose network. A lock needs an unlocker - a lease only needs a
clock.

## The mechanics in Seamless

- **Ready, then claimed.** Tasks declare dependencies; a task is *ready* when
  every blocker is done. `tasks_claim` atomically moves a ready task to
  `in_progress` with a holder and a lease (900 seconds by default, at most
  14400 - four hours; a longer one is refused), so the race is only ever
  between tasks that genuinely could both proceed.
- **The bounce names the holder.** A refused claim says *why*: held by a live
  claim (and by whom), blocked by unfinished dependencies (and which), or
  already closed. The losing agent picks the next ready step instead.
- **Heartbeat by re-claiming.** A holder re-claims to refresh its lease; a
  claim that stops being refreshed eventually expires, at which point the
  task - and everything that was blocked behind it stays consistent - is
  reclaimable by anyone.
- **A claim keeps its session live.** While its lease is live, a claim keeps
  the holder's [session](/concepts/sessions/#liveness) live: the idle reaper
  does not end a session that holds one, however long its work goes quiet - a
  long test run, a wait on CI. Once the last lease lapses, the next reaper
  pass ends the session as usual, so a crashed holder's session outlives it by
  one lease at most.
- **A holder hears when it loses a claim.** When another session claims the
  task after its lease lapsed, when the owner releases the claim (the console,
  `seam task release --force`), or when the reaper releases a lapsed claim from
  a session gone idle, the session that held it is told once. Its next
  Seamless tool result, prompt injection or session-start briefing carries a
  `Claim lost:` notice: the task, when, and the session that took it when the
  holder may see that session (another session otherwise), with what to do
  next.
- **Release follows the work.** Finishing or dropping the task frees it,
  `tasks_release` frees it explicitly, and ending a session frees every claim
  the session held.

Because the queue lives in [Seamless](/) rather than inside any one agent
runtime, the claims work *across* clients - a Claude Code agent and a
[Codex CLI](/codex-cli/) agent share one queue - and they persist across
sessions and days, unlike coordination state scoped to a single agent team's
lifetime.

## The honest limit

A lease serializes *intent*, not edits. Two agents holding two different tasks
can still touch the same file; that conflict belongs to git, not the queue.
And claims are advisory for humans - the console shows who holds what, but
nothing stops you from editing code yourself.

See it happen: [two agents race for the same plan step](/scenarios/task-collision/),
with the winning claim, the bounce, and the pivot, in two live recorded
sessions. The full model - the ready queue, plans as compositions - is in
[tasks & plans](/concepts/tasks-and-plans/).
