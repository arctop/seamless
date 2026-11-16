---
title: Glossary
description: The vocabulary, with the distinctions that actually matter - memory vs note vs finding, briefing vs recall, archive vs supersede vs delete.
---

Terms are listed alphabetically. The ones worth reading even if you think you
know them are the four **disambiguation** entries at the end: each is a pair
people routinely use interchangeably, and each pair means genuinely different
things.

## A–Z

**Agent process** - the `claude` or `codex` process that runs an agent's hooks
and launches its MCP transports. The SessionStart hook stamps its identity on
the ambient session, and the agent's MCP requests name it, which binds the
agent's tool calls to that session with no `session_start`. See [Sessions &
briefings](/concepts/sessions/#process-binding).

**Ambient session** - a Seamless session opened automatically by a Claude Code
or Codex SessionStart hook, without the agent asking, displayed with an opaque
`cc/...` or `cx/...` handle. Lifecycle identity uses the full external session
ID plus client, not the display handle. Contrast *explicit session*.

**Archive** - marking a Seamless memory invalid because it is no longer
relevant: it leaves the indexes and stays readable. Proposed by the gardener's
staleness pass, never done to a `constraint` or a pinned `stage`.

**Binding** - the association between an MCP connection and a Seamless session,
set by `session_start` or, for a Claude Code or Codex agent, automatically
through its *agent process*; an explicit `session_start` wins. Everything on
that connection inherits the bound session's project.

**Briefing** - the `<seam-briefing>` context block Seamless injects into an
agent at session start - constraints, pinned stages, plan rollups, the memory
index, recent findings - assembled inside a token budget. See [Sessions &
briefings](/concepts/sessions/).

**Claim** - an atomic, leased hold on a Seamless task, taken with `tasks_claim`;
exactly one agent can hold a live claim.

**Console** - Seamless's read-mostly web UI at `/console`, an observability
surface for the human owner; agents use MCP.

**Constraint** - the Seamless memory kind for a rule the project cannot
violate; pinned into every briefing, never dropped for budget, never
staleness-archived.

**Description** - the one-line summary in a Seamless memory's frontmatter, the
**only** text shown in any index and therefore the entire retrieval surface.
See [Write memories that get recalled](/guides/write-good-memories/).

**Digest** - a note summarizing a Seamless project's recent activity, proposed
by the gardener.

**Explicit session** - a Seamless session opened by calling `session_start`.
Without a `name`, the call adopts the calling agent's existing ambient session
rather than opening a second one; a Claude Code or Codex agent that names a
different project gets a separate session there instead.

**Family** - a set of Seamless projects related by parent/child, so a child's
briefing can carry the parent's memories and a sibling's recent findings.

**Fail closed** - the Seamless rule that a durable write with no resolvable
scope is rejected rather than defaulted to global. See [Projects &
scope](/concepts/projects/).

**Fail open** - the Seamless rule that a hook never blocks an agent: an
internal error still returns success. The cost is that failure is silent.

**Finding** - what a Seamless session learned, passed to `session_end` and
surfaced in later briefings. Not a memory: a finding is what *happened*, a
memory is what is *true*.

**FTS5** - SQLite's built-in full-text search engine, the keyword half of
Seamless recall.

**Gardener** - the Seamless background pass that finds duplicates, staleness,
and drift and **proposes** fixes; it never acts on its own. See [The
gardener](/concepts/gardener/).

**Gate** - what a Seamless task waits on outside Seamless: who must act and
what must happen. A step whose own gate is open, or that a gate task (`kind:
gate`) blocks, is not ready; a person's action clears it. See [Tasks &
plans](/concepts/tasks-and-plans/#gates).

**Global** - the Seamless scope with no project, visible to every agent in
every repo; reached only by passing `project: global` deliberately.

**Kind** - a Seamless memory's type: `constraint`, `convention`, `runbook`,
`protocol`, `gotcha`, `decision`, `refuted`, `reference`, or `stage`. See
[Memory & notes](/concepts/memory/).

**Lab** - a shared Seamless workspace for a systematic investigation, holding
trials; opened with `lab_open`.

**Lease** - the expiry on a Seamless task claim (default 900 seconds).
Re-claiming refreshes it; an expired lease is reclaimable, so a crashed agent
does not strand a task.

**Memory** - in Seamless, a markdown file with YAML frontmatter holding one
durable piece of knowledge; the unit that reaches briefings.

**Note** - in Seamless, a markdown file holding a work artifact - research
findings, a meeting summary, a design record; found via recall, never injected
into a briefing.

**Plan** - in Seamless, not a primitive but a composition keyed by
`plan:<slug>`: a narrative note, supporting notes, and step tasks. See [Tasks &
plans](/concepts/tasks-and-plans/).

**Project** - the scope a Seamless memory, note, task, or session belongs to;
resolved from an explicit argument, a bound session, or the agent's cwd.

**Proposal** - the Seamless gardener's output: a suggestion for the owner to
review, applied only with `gardener_apply`.

**Provenance** - the Seamless record of where knowledge came from and what
replaced it: `source_session`, `superseded_by`, `invalid_at`.

**Ready** - a Seamless task with no unfinished blocker and no open gate;
`tasks_ready` returns exactly those.

**Recall** - Seamless's single search entry point, fusing FTS5 keyword matching
and vector similarity with RRF. Also, loosely, the `<seam-recall>` block
injected on prompt match - see the disambiguation below.

**Repo lock** - a Seamless lock on a repository's refs (its main branch, or a
tag pattern), declared once per repository and name and held with a lease by
one session while it lands changes. It belongs to the repository, never to a
project, and is always named with it: `LOCK seamless main`. Sessions waiting
for a busy one queue for it and take it first come, first served
(`seam lock acquire --wait`). An optional feature, off by default.

**Reproject** - moving a Seamless memory to a different project that **already
exists**; moving it to one that does not is a *split*.

**RRF (reciprocal rank fusion)** - the method Seamless recall uses to combine
the keyword and vector rankings so neither retriever gets a veto.

**Session** - one agent's stretch of work in Seamless. Sessions heartbeat; an
idle one is reaped and marked `expired`.

**Split** - dividing one Seamless project into new child projects, creating
them and a shared parent; planned as a unit by `gardener_split`.

**Stage** - the Seamless memory kind recording where multi-session work stands;
pinned into briefings like a constraint.

**Supersede** - replacing an outdated Seamless memory with a new one: the old
is marked invalid, leaves the indexes, and stays readable pointing at its
replacement.

**Trial** - one attempt recorded in a Seamless lab: what was tried, what was
expected, what happened.

**ULID** - the id format Seamless uses everywhere, sortable by creation time.
Never UUID.

## Four distinctions worth getting right

**Memory vs. note vs. finding.** A *memory* is what is true (injected into
briefings). A *note* is what you produced (found by searching). A *finding* is
what a session learned (surfaced as recent activity). The test for the first
two: would a future agent need this injected before it starts? Then it is a
memory.

**Briefing vs. recall injection vs. recall call.** All three end with the agent
knowing something, which is why they blur. The *briefing* fires at session start
and is unconditional. A *recall injection* fires on prompt match, mid-turn, still
without the agent asking. A *recall call* is the agent choosing to search. The
first two are ambient; only the third is a decision. See
[Recall](/concepts/recall/).

**Ambient vs. explicit session.** *Ambient* is opened by the hook, per agent,
automatically, and the agent's tool calls are bound to it without asking.
*Explicit* is `session_start`, which **adopts** the agent's own ambient session
rather than creating a rival - so a Claude Code or Codex agent needs it only to
move to another project or resume a named session.

**Archive vs. supersede vs. delete.** *Archive*: no longer relevant, marked
invalid, still readable. *Supersede*: replaced by something specific, marked
invalid, still readable, **pointing at its replacement**. *Delete*: gone. The
rule of thumb - delete is for things that were never true; supersede is for
things that stopped being true; archive is for things that stopped mattering.
