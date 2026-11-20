---
title: Recall
description: One search entry point fusing keyword and vector search - and the three different ways your knowledge actually reaches an agent.
---

In Seamless, `recall` is the single search entry point an agent calls to
retrieve durable knowledge - memories and notes - from the store. It fuses
SQLite FTS5 keyword matching with float32 cosine vector similarity using
reciprocal rank fusion, all inside one SQLite file with no external vector
database. With no embedding provider configured, recall runs keyword-only
rather than failing; when a configured provider is merely unreachable, it
degrades the same way instead of erroring.

`recall` is also the *only* search tool. There is no separate keyword search, no
separate semantic search, no "advanced" variant. One entry point, because a
surface with three search tools is a surface where agents pick the wrong one.

## What it searches

`recall` spans two halves of the store, and the default scope (`all`) covers
both:

| Half | Kinds | Matched by |
|---|---|---|
| **Durable knowledge** | memories, notes | keyword + vector |
| **The work record** | tasks, trials, session findings | keyword |

The work record is there because knowledge does not only get written as a
memory. A task body carries the acceptance criteria and the record of what was
already tried; a dropped task is often the only account of why something was
*not* done. A trial carries expected-vs-actual. A session's findings are the
handoff its author wrote for whoever came next. All of it used to be reachable
only by an agent already knowing which list tool to call - which is to say, only
by an agent that already suspected the answer existed.

Narrow it with `scope`: `memories`, `notes`, `tasks`, `trials`, `sessions`.

Two things follow from the work record being keyword-only. It carries no
embedding: there is no file behind a task row and so no content-hash reconcile
loop, and vectorizing on write would put a provider round-trip inside every
`tasks_add`. And because fusion rewards an item that *both* retrievers rank, a
work-record hit can never outrank a memory that matched both ways on the same
query - the ordering prefers curated knowledge without needing a rule that says
so.

Work-record hits carry a `status` (a task's status, a trial's outcome), which is
what separates a step you can still claim from a decision you can only learn
from.

## How it works

Two retrievers run over the store and their rankings are fused:

- **FTS5 keyword search.** Exact terms, names, error strings - the things vector
  search is worst at. If you know a memory is called `chroma-boot-race`, this is
  what finds it.
- **Vector similarity.** Embeddings stored as float32 blobs in SQLite, compared
  by brute-force cosine. This finds the memory that is *about* your problem when
  you don't know its name. Knowledge kinds only - see
  [What it searches](#what-it-searches).

Their results are combined with **reciprocal rank fusion**: each retriever's
ranking contributes, and something both rank highly wins. Neither retriever gets
a veto, which matters because each is confidently wrong in a different way.
([RRF for agent recall](/concepts/reciprocal-rank-fusion/) defines the method
and the constant.)

The result set is then packed into `budgets.recall_budget_tokens`. A kind
listing (`kind` with no query) is not a ranking and is not packed: it returns
every match up to its `limit` and counts the rest.

### About that brute-force scan

Cosine similarity is computed by scanning every candidate vector. No ANN index,
no approximate nearest neighbours, no vector database. For a personal knowledge
store this is simply fast enough, and it buys exactness and one fewer moving
part. It is a deliberate trade, not an oversight - and it is the kind of thing
worth saying out loud rather than hiding behind the word "hybrid".

### The utility nudge

Every memory carries a **utility score**: a time-decayed record of query-gated
demand. An explicit read weighs 3.0, a recall hit 1.5, a prompt match 1.0 - and
being pushed into a briefing counts for exactly nothing, so exposure can never
masquerade as usefulness. Scores halve every 14 days.

Recall applies it as a bounded post-fusion multiplier: at most +10%, strictly
below the +15% favorite boost, so an explicit star always outranks an implicit
hot streak - and neither can resurrect something the retrievers did not return.
The ambient `<seam-recall>` injection uses the same score to pick which
qualified matches fill its three slots - after its overlap and score floors,
never instead of them. The briefing blends it with recency once a project's
demand history matures - for the memory index's order, and for ranking
constraints ahead of their full/compact tier split
([Sessions & briefings](/concepts/sessions/#the-budget-and-what-survives-it)).
One signal, three surfaces, all bounded.

## The recall triad

This is the part worth internalizing: **searching is only one of three ways your
knowledge reaches an agent**, and they are easy to confuse because all three end
with the agent knowing something.

| | What fires it | What it delivers | Agent asked? |
|---|---|---|---|
| **The briefing** | SessionStart hook | Tiered constraints, pinned stages, plan rollups, recent findings, the memory index | No |
| **Recall injection** | UserPromptSubmit hook, when a prompt matches stored memories | A `<seam-recall>` block with the matching memories | No |
| **A recall call** | The agent calls `recall` | Ranked, fused, budgeted results | Yes |

The first two are **ambient**: they happen whether or not the agent thinks to
ask. That is the whole point of the hooks. An agent that never calls a Seamless
tool still starts with your constraints and still gets relevant memories surfaced
when its prompt touches them.

The third is what an agent does when it wants something specific. Before you go
looking for something in your own knowledge base by hand, ask an agent to
`recall` it - that is faster and more accurate than making you search.

Read what was already injected before searching again. The briefing is in
context; re-recalling it just spends tokens to learn what the agent was told.

## Degradation is deliberate

Recall keeps a distinction that is easy to get wrong: **a remote failure degrades,
a local misconfiguration surfaces.**

- The embedding provider is unreachable, rate-limited, or rejects the key →
  **degrade**. Recall falls back to keyword-only results. You get worse ranking,
  not an error, because a partial answer beats no answer when the network is
  having a bad day.
- The configuration itself is wrong → **surface it**. This is not a transient
  condition that retrying fixes, and silently degrading forever would hide a
  problem only you can fix.

The failure mode this avoids: a store that has quietly been keyword-only for
three weeks because nobody noticed the embedding key expired.

## Writing memories that get recalled

Recall can only find what was written to be findable. Since the `description` is
the only text an index shows, and it is heavily weighted in matching, it is the
retrieval surface - see [Memory & notes](/concepts/memory/).
