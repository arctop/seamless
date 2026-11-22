---
title: Lab, gardener & usage
description: Research trials, the gardener (propose-only unless you opt in), the usage summary, and favorites - the nine tools for keeping the store honest.
generate: mcp-tools
tools:
  - lab_open
  - trial_record
  - trial_query
  - gardener_proposals
  - gardener_request
  - gardener_split
  - gardener_apply
  - usage_summary
  - favorite_set
---

## The research lab

A lab is a shared workspace for a systematic investigation: a hypothesis, a
series of trials, and what each one actually did. `lab_open` binds a lab to the
connection, so `trial_record` inherits it the way memory inherits a session's
project. `trial_query` reads the trials back.

The point is **parallel agents collaborating on one investigation**. Several
agents chasing the same bug can open the same lab, record what they tried, and
see each other's dead ends instead of re-walking them. When the investigation
resolves, distill the finding into a memory - the lab is the working record, not
the conclusion.

## The gardener proposes; you decide who acts

The gardener runs periodically over the store looking for drift. Every pass it
makes ends in a **proposal row for you to review** - it never rewrites your
knowledge behind your back. That is the contract, and it is why the tool
surface has both `gardener_proposals` (read them) and `gardener_apply` (act on
one) instead of a single "tidy up" button.

Two switches, both yours, say who may act on a proposal
([The gardener](/concepts/gardener/#propose-only-unless-you-opt-in)).
`gardener.agents_may_apply` is on by default; turned off, `gardener_apply`
refuses an agent's apply, dismiss and hide, and `gardener_proposals` says so.
`gardener.auto_apply` is empty by default; it may name any of `memory_wanted`,
`tool_error`, `abandon_plan` and `ship_plan` - kinds that only open a task or
retag a plan note, each undoable from the console - and a pass then applies a
new proposal of a named kind by itself, recorded as `auto`.

Its passes:

| Pass | What it looks for |
|---|---|
| dedup | Two memories that say the same thing, above `gardener.dedup_threshold` similarity. Proposes a **merge**: one is kept, the other superseded and pointed at it. |
| staleness | Memories untouched for `gardener.staleness_days`. Proposes an **archive**: marked invalid, still readable. |
| stale-stage | A `stage` memory whose gate is done, missing, or unparseable, unchanged for `gardener.stale_stage_days`. Proposes an **archive**. |
| dead-weight | A memory briefings keep injecting with zero recall hits, prompt matches, or reads. Proposes an **archive**. |
| digest | Enough recent activity to be worth a summary over `gardener.digest_days`. Proposes a **digest** note. |
| stale-plan | A captured plan still unapproved after `gardener.stale_plan_days`. Proposes settling it: **ship_plan** when commits show the work landed, **merge_plans** when another composition holds its steps, **abandon_plan** otherwise. |
| stale-copy | A memory with a [provenance stamp](/concepts/project-isolation/#provenance-stamps) whose cited source in another project changed after the memory was written. Proposes an **archive**. |
| memory-wanted | The same recall query missing across sessions. Proposes a **memory_wanted**: applying opens a task to write the knowledge agents keep searching for. |
| tool-error | The same normalized tool or hook error recurring (tool errors across 2+ sessions). Proposes a **tool_error**: applying opens an investigation task with the observed evidence. |
| copy-relocate | A stamped memory whose stamps and source references all name one other project. Proposes a **relocate** into that project. |

Constraints and pinned stages are never age-filtered or staleness-archived. A
constraint does not become less true by sitting still.

`gardener_request` takes a request in natural language ("fold the two console
theme memories together") and turns it into the same reviewable proposals. It can
also **reproject** a memory filed under the wrong project - moving it to a
different project that already exists - and **rekind** a memory filed under the
wrong kind, reclassifying it in place (most often demoting a project-local
constraint to a convention, or the reverse).

Its `project` argument scopes the memories it may reference: a project slug (that
project plus globals), `global` for globals only, or `all` for every project on
the machine. Omit it and the scope resolves the way every other read does - from
the session - rather than quietly widening to everything. A slug that is not a
project is an error, not an empty result.

## Splits are a different thing

Moving a memory into a project that does not exist yet is not a reproject - it is
a split, and `gardener_split` plans it. Splitting one project into new children
creates those projects and a shared parent, so it is planned as a unit rather
than as a pile of individual moves.

## usage_summary

`usage_summary` reports what the store actually contains and what retrieval has
been doing: memory counts, retrieval statistics including the highest-utility
memories by decayed demand, events by kind. It answers "is
this thing working, and on how much?" - the same question the console's Overview
answers, for an agent that cannot open a browser.

## favorite_set

`favorite_set` stars or unstars an item - a memory, note, project, plan, task,
session, or trial. A starred memory keeps a pinned place in its project's
briefings, inside its own section: a starred constraint or convention heads
that section, any other starred memory heads the Memories list, and no trim or
budget drops it. It also gets a mild recall rank boost; in the console, favorites
carry a star, sort first, and can be filtered. For memories and notes the flag
lives in the file's frontmatter (`favorite: true`), so it survives an index
rebuild and can be hand-edited. A plan's favorite lives on its primary note, so
a task-only plan (no note) cannot be starred. Starring never bumps an item's
updated time - it is metadata, not authorship.

## Applying is an explicit boundary

`gardener_apply` accepts `apply`, `dismiss`, or `hide`. Both rejections close
the proposal without changing its target; they differ in how long the answer
holds. `dismiss` settles the evidence in front of you, so a pattern that keeps
recurring is raised again later - reach for it by default. `hide` blocks the
pattern permanently, for a suggestion that is wrong in principle rather than
wrong for now; the owner can lift it from the console's **Hidden forever** list.
Apply performs the proposal's typed action and
returns that real outcome; it never substitutes a plausible dummy when the
target disappeared or a file/store mutation failed. Memory-changing actions
route through the lifecycle/files services, so supersession, atomic Markdown
writes, and occupied-path protections remain the same as direct tools.
