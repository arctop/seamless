# Memory & notes

> What a memory is, the nine kinds, how supersession keeps the store honest, and when to write a note instead.

In Seamless, a memory is one markdown file with YAML frontmatter holding one
durable piece of knowledge an agent should not have to rediscover. The files
are the source of truth - the SQLite database is a rebuildable index over them -
and the frontmatter carries a lifecycle: a memory can be superseded or archived,
and an invalid memory leaves every index while staying readable. A memory is for
what is *true*; work artifacts belong in [notes](#memory-or-note) instead.

That the store is plain markdown is not an implementation detail - it is the
design. You can read it, `grep` it, edit it, and put it in git, and no part of
the system needs your permission to be inspected.

```yaml
---
id: 01K...                 # ULID, assigned once, stable forever
kind: gotcha               # what sort of knowledge this is (see below)
name: chroma-boot-race     # kebab-case, unique within the project
description: one line, <=150 chars -- the ONLY text shown in indexes
project: seamless          # empty = global
created: 2026-07-10T18:00:00Z
updated: 2026-07-10T18:00:00Z
valid_from: 2026-07-10T18:00:00Z
invalid_at: null           # set on supersession/archive; invalid memories leave indexes
superseded_by: null        # ULID of the replacement
source_session: 01K...     # ULID of the session that first wrote it
model: claude-fable-5      # the model that produced the content, as the provider names it
favorite: true             # present only while the memory is starred
tags: [x, y]
---
body markdown
```

Two more keys appear only with optional features: `account` names the
[restricted local account](https://thereisnospoon.org/docs/guides/peer-auth/#restricted-local-accounts) that
wrote the content, and `may_contain` lists
[provenance stamps](https://thereisnospoon.org/docs/concepts/project-isolation/#provenance-stamps) naming the
fenced projects whose content the writing conversation held.
[Storage](https://thereisnospoon.org/docs/reference/storage/#memory-frontmatter) has every field.

## The description is the product

Everything above the body is bookkeeping except one field. **The `description` is
the only text an index ever shows** - the briefing, the memory list, the recall
result. An agent decides whether to read the body based on nothing but that line.

A description that says "notes about the console" is invisible: no future agent
can tell whether it matters. One that says "Console: a trailing note inside a
`.card h2` must use `.h2-meta`, never `.count` - header is flex space-between"
does the whole job without the body being opened at all.

Write the description for a future agent deciding whether to read further. That
is its entire purpose.

## The nine kinds

| Kind | What it holds | Never forget |
|---|---|---|
| `constraint` | A rule any agent must follow regardless of task | Pinned into every briefing, never dropped for budget, never staleness-archived; the top `briefing.constraint_max_full` render in full, the rest as names on the compact `+N more, equally binding` line |
| `convention` | A project-local choice or layout fact - naming, branding, where things live and deploy | Binding but topically triggered: its own budget-competing Conventions section (the top `briefing.convention_max_full` in full, the rest behind an always-rendered count line pointing at `recall kind=convention`); filing it as `constraint` crowds the briefing head |
| `runbook` | A procedure that works | The steps, in order, that actually ran |
| `protocol` | An agreed way of doing something | Why the agreement exists |
| `gotcha` | A trap and how to avoid it | The symptom, so it is recognizable next time |
| `decision` | A choice and its reasoning | The alternatives rejected, or it will be relitigated |
| `refuted` | Something believed that turned out false | Keeping this is what stops the fleet re-deriving it |
| `reference` | A pointer to something external | The URL and what is at it |
| `stage` | Where a piece of multi-session work stands | The body must open with `Status: open\|in_progress\|blocked\|done` (plus `Gate: human\|ai`) - the pin belongs to the gate, not the kind |

Choosing the kind is not filing paperwork. `constraint` and `stage` are
**pinned**: they survive budget pressure and are never age-filtered or
staleness-archived. Marking a preference as a constraint crowds out real
constraints; filing a real constraint as a `reference` means agents will violate
it.

A stage's pin is conditional on it actually gating something. `Status: done`
unpins it immediately; a missing or unparseable header renders as
`status unknown (no Status: header)` for a grace window
(`briefing.stage_unknown_max_age_days`, default 7 days since last update) and
then leaves the briefing, and the gardener proposes archiving gateless stages
after `gardener.stale_stage_days` (14). A milestone breadcrumb ("X landed") is
a note or a finding, not a stage - written as a stage without a live gate, it
now expires instead of pinning forever.

Update a stage's status with `memory_edit` (replace the `Status:` line) or by
re-writing it with `memory_write` (same name, in place) - `memory_append` adds
below the existing body and cannot change the header, which is parsed from the
top of the body. And the rule of thumb that
keeps stages out of the task queue: a task is work someone here can claim and
finish; a stage is a state of the world you wait on that every session must
know while it holds.

`refuted` deserves special mention. A store that only records what is true keeps
paying for the same wrong turn: the fleet re-derives the dead end, tries it,
finds it dead, and moves on - every time. Recording the refutation makes that
cost one-time.

## Supersession: how the store stays honest

Memories are not appended forever, and they are not silently overwritten. When
something stops being true, the replacement **supersedes** it:

```text
Explicit supersession
memory_write new-truth supersedes old-truth The replacement is written first.
old-truth Invalid, preserved invalid_at = now
superseded_by = new id
Leaves briefing and recall; remains readable on disk.
new-truth Active and indexed Eligible for recall and the next briefing.
Replacement changes which memory is active without erasing the historical record.
```

The old memory leaves every index but stays readable. An agent following an old
reference lands on it, sees that it is invalid, and finds the pointer to what
replaced it. That is provenance: the store can tell you not just what it believes
but what it used to believe and what changed its mind.

Contrast the two failure modes this avoids. **Append-only** means recall returns
three contradictory answers and the agent picks one. **Destructive overwrite**
means the reasoning is gone and the same argument happens again in six weeks.
[Memory supersession](https://thereisnospoon.org/docs/concepts/memory-supersession/) defines the idea on its
own and contrasts it with decay-style forgetting as well.

## Edit, update, append, supersede, or delete

| You want to | Use |
|---|---|
| Fix part of *this* memory without resending it - a typo, a stale path, a stage's `Status`, its description or tags | `memory_edit` (exact search/replace; returns a diff) |
| Correct or extend what *this* memory says | `memory_write`, same `name` (updated in place, id stable) |
| Add to the end without rereading it | `memory_append` |
| Replace a **different**, now-outdated memory | `memory_write` with `supersedes` |
| Remove something written by mistake | `memory_delete` |

The line that matters: **delete is for things that were never true; supersede is
for things that stopped being true.** Deleting the latter destroys the history
that explains the current state. `memory_edit` has a line of its own: it is
for changes that carry no new claim. When what the memory says changes, write
the new memory with `supersedes`, so the old one stays readable as history.

Never hand-stamp `invalid_at` or `superseded_by` in a file. They are set by the
supersede path, which also updates the indexes; editing them by hand leaves the
database disagreeing with the disk.

## Memory or note?

The single most common confusion:

| | Memory | Note |
|---|---|---|
| Answers | "What is true about this project?" | "What did we produce?" |
| Size | One idea, led by a one-line description | However long the artifact needs |
| Reaches a briefing | Yes | No - found via [recall](https://thereisnospoon.org/docs/concepts/recall/) |
| Examples | A constraint, a gotcha, a decision | Research findings, a meeting summary, a design record |

The test: **would a future agent need this injected before it starts working?**
If yes, it is a memory, and it has to earn its line in the budget. If it is
something you would want to *find* and read in full when the topic comes up, it
is a note.

Journaling into memory is the classic mistake. It is too long to inject, too
specific to generalize, and it pushes real constraints out of the briefing.

## Global vs. project

A memory with an empty `project` is global: every agent in every repo sees it.
That is a strong claim, so writes **fail closed** - with no session and no
explicit project, a write is rejected as ambiguous rather than quietly landing in
the global scope. Pass `project: global` to mean it on purpose. See
[Projects & scope](https://thereisnospoon.org/docs/concepts/projects/).
