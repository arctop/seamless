---
title: Sessions, memory & recall
description: The nine tools an agent uses most - open a session, write, edit and read memory, and search the store.
generate: mcp-tools
tools:
  - session_start
  - session_update
  - session_end
  - memory_write
  - memory_append
  - memory_edit
  - memory_read
  - memory_delete
  - recall
---

These nine tools are the agent loop. In a repo mapped to a project, most of them
need no `project` argument at all: the connection is bound to a session -
automatically for a Claude Code or Codex agent, by `session_start` for any other
client - and everything inherits that session's scope.

## The shape of a session

`session_start` returns the project briefing and binds the session to the
connection. `session_end` persists findings for the next agent's briefing.

A Claude Code or Codex agent does not need `session_start`: its hooks open an
*ambient* session and bind the agent's tool calls to it
([how](/concepts/sessions/#process-binding)). Called from such an agent without
arguments, `session_start` hands back that same session, and its `scope` note
says the call was not needed; with a different `project` it starts a separate
session there. Every other client calls it once per connection, with `cwd` or
`project` - see [when to call it](/concepts/sessions/#when-to-call-session-start).
Whichever way it binds, the briefing it returns is for the project actually
bound.

`session_start` no longer takes `source`. The legacy values (`startup`,
`resume`, `clear`, `compact`, `explicit`) are accepted and ignored, so older
clients keep working; any other value is refused with
`session_start no longer takes source -- omit it`. The server derives what the
briefing needs from what the call did: resuming a named session adds the
resumed-session hint to re-ground with `recall`.

`session=` on `session_update` and `session_end` - and on the
[task tools](/reference/mcp/tasks/) - takes a session name (the `cc/...` or
`cx/...` on the briefing's `Seam session` line, or a `sess/*` name) or a session
ULID; one that matches nothing is an error saying what the argument takes. Both
tools also accept `summary` as an alias of `findings`.

On a server that records commits (`commits.record`, off by default),
`session_end` also takes `commits`: the full ids of the commits the session
made, recorded on it for an integrity monitor. Off, the argument does not
exist. See [Record commits for an integrity monitor](/guides/commit-records/).

If `tasks_update` ever fails claiming a task is held by *your own* session id,
the connection lost an explicit `session_start` binding - those live in daemon
memory, so a restart or reconnect drops them. Re-run `session_start` with the
same name to rebind. A Claude Code or Codex agent's automatic binding is not lost
this way.

## Edit, update, append, supersede, or delete?

Five ways to change memory, and picking the wrong one is how a store rots:

| You want to | Use | What happens |
|---|---|---|
| Fix part of a memory without resending it | `memory_edit` | Exact search/replace on the body, plus `description` and tag add/remove; returns a diff |
| Rewrite what a memory says, whole | `memory_write` with the same `name` | Updated in place; the id is stable |
| Add to the end without rereading it | `memory_append` | Body grows; nothing else changes |
| Replace a **different**, now-outdated memory | `memory_write` with `supersedes` | The old one is marked invalid, leaves every index, and stays readable with a pointer to its replacement |
| Remove something written by mistake | `memory_delete` | Gone |

The distinction that matters is **supersede vs. delete**. Superseding is how the
store stays honest about its own history: the old memory leaves the briefing and
recall, but an agent that follows an old reference still finds it, marked
invalid, pointing at what replaced it. Delete is for mistakes - things that were
never true - not for things that stopped being true.

A `supersedes` that fails is reported rather than swallowed: the new memory is
still written and kept, and the call returns an error naming it. The target is
then still active, so re-run the supersede.

### Edit vs. supersede

`memory_edit` is cheap, which is exactly why its boundary has to be explicit.
Edit is for changes that carry no new claim: a typo, broken formatting, a stale
path or command, a stage's `Status` flip, a description, a tag. If the **meaning**
changes - the conclusion is now different, the advice reversed - that is a new
memory, and it goes through `memory_write` with `supersedes`.

The reason is provenance. A supersession retires the old memory into readable
history; an in-place edit leaves no trace that the store ever believed something
else. Using edit to change what a memory claims silently rewrites the record.

`memory_edit` is also the only way to change a memory's `description` or tags
without rewriting its body, and `tags_remove` is the only way to clear a tag at
all - an empty `tags` array reads as absent everywhere else.

## Concurrency: content_hash and expect_hash

`memory_read` and `notes_read` return a `content_hash` - the SHA-256 of the whole
file. Pass it back as `expect_hash` on `memory_edit`, `notes_edit`, or
`notes_update`, and the write is refused if the stored file has moved on since
you read it. Omit it and the write is unconditional.

It answers a different question from the daemon's own serialization. Every
application write already goes through a per-file lock, so two agents can no
longer interleave a read and a write and lose one of them. What the lock cannot
see is an agent acting on something it read minutes ago, or the owner editing the
markdown in an editor - which is why the precondition is checked against the
**file**, inside the lock, rather than against the index (the watcher re-indexes
on a debounce, so the index would happily confirm the stale hash).

## Scope

`memory_write` **fails closed**: with no session and no explicit `project`, it is
rejected as ambiguous rather than silently landing in the global scope. Pass
`project: global` to write a deliberately cross-project memory.

`memory_edit` targets a memory that already exists, so it resolves like
`memory_append`: the session's scope first, then a global fallback. The write is
judged against the project the memory actually sits in.

## Recall is the only search tool

There is one search entry point. `recall` fuses FTS5 keyword matching and vector
similarity with reciprocal rank fusion, nudges the fused order by favorite and
[utility](/concepts/recall/#the-utility-nudge) (both bounded), scoped to the
current project plus global items, and packs results into a token budget. A call
that finds nothing is recorded as a miss - recurring misses become the
gardener's [memory-wanted proposals](/concepts/gardener/#what-it-looks-for).

The optional `kind` filter restricts hits to memories of one frontmatter kind.
It implies memories-only: combining it with `scope=notes` is rejected as
contradictory rather than returning a misleading empty result, and a
kind-filtered miss still counts as memory-wanted demand.

With `kind` set, `query` becomes optional: a kind alone is the **browse mode**
behind briefing hints like `recall kind=convention` - the scope's active
memories of that kind, listed newest-first under the same limit and token
budget. A browse is a listing, not a search: no fusion, no favorite or utility
boost, its hits record as passive exposure (never query-gated demand), and an
empty browse records no miss - "this project has no conventions yet" is not a
missing memory.

It degrades rather than fails: if the embedding provider is unreachable, recall
falls back to keyword-only results instead of erroring. A local misconfiguration
is surfaced instead of hidden - the two cases are deliberately not treated alike.

## Results and failures

| Call | Success result | Failure that matters |
|---|---|---|
| `session_start` | `session_id`, `name`, resolved `project`, explanatory `scope`, and `briefing`; resumed/adopted sessions also say `resumed: true`, and a `warning` flags a named `project` that was created or a remote `cwd` that could not be placed | Briefing assembly degrades to an empty string and logs; creating or binding the session itself still fails loudly, as does a `project` that contradicts the repository `cwd` is in |
| `memory_write` | Stable `id`, canonical `name`, resolved `project`, `updated`, optional `similar`, and optional `superseded` | An occupied tombstone path is an error; if the new memory lands but supersession fails, the tool errors while naming the kept replacement and the still-active target |
| `memory_edit` | `id`, `name`, `project`, the new `content_hash`, a unified `diff`, and a `stage_hint` when a `kind=stage` body still has no parseable `Status` | An `old_string` that matches zero or several places is an error naming the count, and **nothing** is written - the edits apply all-or-nothing. A stale `expect_hash` is refused rather than overwriting |
| `recall` | `hits`, possibly empty | Remote embedder failures degrade to lexical-only; local request/config construction errors surface |
| `session_end` | Confirmation of the close - `session_id`, `claims_released`, `mishaps_recorded`, and `commits_recorded` when it recorded commits; findings persist for the next briefing | A missing/ambiguous session is an error rather than a fabricated successful close; so is a `commits` entry that is not a full commit id, which leaves the session open |
