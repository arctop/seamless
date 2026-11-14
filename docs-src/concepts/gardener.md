---
title: The gardener
description: The background pass that finds duplicates, staleness, and drift - and proposes, because nothing rewrites your knowledge behind your back.
---

The Seamless gardener is the background pass that keeps an agent-written memory
store from rotting: it detects near-duplicate memories by embedding similarity,
memories gone stale, and captured plans left unapproved (including ones whose
work quietly shipped), and files each finding as a proposal. Nothing is applied
until you approve it with `gardener_apply` - the gardener has no write path of
its own.

Any store that agents write to accumulates cruft: two memories saying the same
thing in different words, a runbook for a system that no longer exists, a plan
abandoned three weeks ago with its steps still open.

The gardener finds those. Then it stops and asks.

## Propose-only is the whole contract

**Every pass the gardener makes ends in a proposal for you to review.** It never
edits, merges, or archives on its own.

This is not timidity. The entire premise of Seamless is that the knowledge is
yours and legible - a store that quietly rewrites itself while you sleep is
exactly the black box the project exists to avoid. If an automated pass can
decide your constraint is stale and archive it, then you do not actually know
what your agents will be told tomorrow.

So the surface is two tools, not one button: `gardener_proposals` to read what it
found, `gardener_apply` to act on one.

## What it looks for

| Pass | Trigger | Proposes |
|---|---|---|
| **dedup** | Two memories more similar than `gardener.dedup_threshold` (0.88) | **merge**: one is kept as-is, the other superseded and pointed at it |
| **staleness** | A memory untouched for `gardener.staleness_days` (90) | **archive**: marked invalid, still readable |
| **digest** | Enough recent activity over `gardener.digest_days` (30) | a **digest** note summarizing it |
| **stale-plan** | A captured plan still unapproved after `gardener.stale_plan_days` (14) | settling it deliberately rather than by neglect: **merge_plans** when the capture owns no steps and another composition in the project holds the steps for the same work (fold it in there), **ship_plan** when commits since the capture's git stamp match the plan (the work landed without the approval ceremony), **abandon_plan** otherwise |
| **stale-stage** | A `stage` memory whose `Status:` header is done, missing, or unrecognized, unchanged for `gardener.stale_stage_days` (14) | **archive**: a stage that gates nothing should not hold a permanent briefing pin |
| **dead-weight** | A memory briefings injected 20+ times in 30 days without a single recall hit, prompt match, or read | **archive**: exposure without demand means it costs tokens and steers nothing |
| **stale-copy** | A memory with a [provenance stamp](/concepts/project-isolation/#provenance-stamps) whose body cites another project's memory, note or task (`<project>:<id>`) that changed after the memory was last written | **archive**: the copy may be out of date; the proposal names the changed sources by project and id, never their content |
| **memory-wanted** | The same recall query returned zero hits in 2+ sessions inside 14 days | **memory_wanted**: write the knowledge agents keep searching for; applying opens a task in the queue |
| **tool-error** | The same normalized tool or hook error, 3+ times inside 14 days and still recent; tool errors must span 2+ sessions | **tool_error**: investigate and fix the recurring failure; applying opens a task with the observed error evidence |
| **copy-relocate** | A stamped memory that cites at least one item, whose stamps and [source references](/concepts/project-isolation/#source-references) all name one other project | **relocate** into that project, the one the copy is about |

The memory-wanted pass is the one place the gardener asks *for* knowledge
instead of curating what exists: recurring zero-hit `recall` queries are demand
for a memory nobody wrote, grouped per project so a one-off miss never fires.
Applying the proposal opens a task ("Write a memory: ...") in the ready queue -
the memory itself is only ever written by whoever picks the task up.

The tool-error pass closes the other side of that loop. A single failure may be
a typo; the same normalized error again and again is evidence of a broken
workflow. It also watches hook errors, which are swallowed fail-open and never
reach an agent - recurrence here is the one place they surface. Applying its
proposal opens a repair task rather than pretending the gardener can fix code
or configuration itself.

Four more proposal types come from an action rather than the timer:

- **reproject** - a memory filed under the wrong project, moved to a project that
  **already exists**.
- **rekind** - a memory filed under the wrong kind, reclassified in place (same
  identity, project, and body). The most common correction is constraint vs
  convention: a systemic rule any agent must follow stays a constraint, while a
  project-local choice or layout fact belongs under convention.
- **split** - one project divided into new child projects. This creates projects
  and a shared parent, so it is planned as a unit, not as a pile of moves. That
  is why it is a separate tool (`gardener_split`) and not just a reproject to a
  name that does not exist yet.
- **relocate** - filed when a project is tightened to confidential or sealed
  (see [Project isolation](/concepts/project-isolation/)). Raising a fence does
  nothing about knowledge that already escaped, so the tighten audits the global
  scope for memories whose source session belonged to the project and proposes
  moving each one behind the fence. Dismiss to leave it global. This is the only
  proposal an owner action files rather than a request or the timer; the
  copy-relocate pass above files the same kind on the timer.

## Constraints and stages are exempt from staleness

Age-filtering and staleness-archiving never touch `constraint` or `stage`
memories.

A constraint does not become less true by sitting still. "Never use CGO" is not
stale at 90 days - it is *settled*, and the absence of recent edits is evidence
it is working, not evidence it is rotting. Time-based archival encodes the
opposite assumption, so the pass simply skips the kinds where that assumption is
wrong.

Stages get their own pass instead, because staleness cannot see them at all:
a pinned stage is re-injected into every briefing, so by the activity metric it
never goes quiet. The stale-stage pass keys off the *update* time and the
`Status:` header - a stage still carrying a live gate
(`open`/`in_progress`/`blocked`) is never proposed at any age, while one that is
done, headerless, or unparseable is proposed for archiving once it has sat
unchanged for `gardener.stale_stage_days`. Both passes respect `[[links]]`: a
memory another body points at is not proposed.

## Asking for it in words

```text
gardener_request "fold the two console theme memories together"
```

`gardener_request` takes natural language and turns it into the same reviewable
proposals as the timed passes. It is a way to *aim* the gardener, not a way to
bypass the review step - what comes back is still a proposal.

## Where you review

- **The console** - `/console/gardener` is an inbox: one line per proposal in
  the rail, the full evidence and the decision gate in the reader. A whole
  group can be dismissed at once, and anything you decide lands in **Recently
  decided**, where Undo puts it back in the queue and reverses what applying it
  did. Applying a consolidation or a project split is the exception - both
  create things that immediately accrue content, so they ask for a confirm
  instead and cannot be undone from the console.
- **`gardener_proposals`** - the same, for an agent.

## Saying no, twice

Rejecting has two strengths, because "not this one" and "not ever" are
different answers:

- **Dismiss** settles the proposal and the evidence behind it. It is not a
  verdict on the pattern: if the same thing happens *again* after you decided -
  agents keep hitting that error, keep searching for that missing memory - the
  gardener raises it once more, under the same key, with the new evidence. This
  is the one to reach for by default.
- **Hide forever** blocks the pattern outright. No recurrence gets past it.

For the passes with no recurrence clock (merge, archive, digest, and the
request-driven kinds), a dismissal already lasts until the underlying content
changes - their key encodes what they propose, so a changed world asks a new
question rather than repeating the old one. The distinction bites on the error
and knowledge-gap passes, whose key is the *pattern's* identity and stays put
while evidence piles up under it.

Both are reversible, and the two exits are not the same:

- **Undo**, from Recently decided, puts that proposal back in the queue.
- **Unhide**, from **Hidden forever** in the rail, lifts the block and leaves
  the proposal resolved. Nothing returns on the spot - the next recurrence is
  what brings the pattern back, and if it has stopped recurring, nothing does.

`gardener_apply` carries the same two tiers for agents: `action=dismiss` and
`action=hide`.

The gardener runs every `gardener.interval_minutes` (60) when
`gardener.enabled` is on. Everything in this page is tunable - see
[Configuration](/reference/configuration/).
