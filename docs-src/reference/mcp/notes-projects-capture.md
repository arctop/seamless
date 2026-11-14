---
title: Notes, projects & capture
description: Work artifacts, project scope, SSRF-safe URL capture, and asking for a locked project - the tools around the edges of memory.
generate: mcp-tools
tools:
  - notes_create
  - notes_read
  - notes_update
  - notes_edit
  - notes_append
  - notes_delete
  - project_list
  - project_create
  - capture_url
  - access_request
---

## A note is not a memory

This is the most common confusion in the whole system, so it is worth being blunt
about it.

|  | Memory | Note |
|---|---|---|
| Answers | "What is true about this project?" | "What did we produce?" |
| Length | One idea, one line of `description` | However long the artifact is |
| Lifecycle | Superseded and archived | Written, occasionally updated |
| Reaches a briefing | Yes - this is what agents get injected | No; found via `recall` |
| Good examples | A constraint, a gotcha, a decision | Research findings, a meeting summary, a design record |

The test: **would a future agent need this injected into its context before it
starts working?** If yes, it is a memory, and it needs to fit in a `description`.
If it is something you'd want to *find* and read in full when the topic comes up,
it is a note.

Writing a journal entry into memory is the classic failure: it is too long to
inject, too specific to generalize, and it pushes real constraints out of the
briefing's budget.

Agent-created notes are automatically tagged `created-by:agent`.

## Four ways to change a note

Notes are long, and the transport caps a request body at 1 MB, so resending a
whole note to fix one paragraph is both wasteful and the thing most likely to
fail on the biggest artifacts.

| You want to | Use | What happens |
|---|---|---|
| Fix or restructure part of the body | `notes_edit` | Exact search/replace, all-or-nothing, returns a diff |
| Replace the body, or change title/description/project/tags | `notes_update` | Field-wise patch; omitted fields are untouched |
| Add to the end | `notes_append` | A UTC-timestamped line joins the body |
| Remove an artifact that should not exist | `notes_delete` | Gone, with no pointer left behind |

`notes_edit` takes the note's `id` and a list of `{old_string, new_string}`
edits. Each `old_string` must match the current body exactly and uniquely - or
pass `replace_all` - and if any edit fails to match, **nothing** is written. That
refusal is the feature: a partial apply, or a fuzzy match landing somewhere
plausible, is the silent corruption the exact-match contract exists to prevent.

`notes_update` gains the same staleness guard (`expect_hash`) plus `tags_add` and
`tags_remove`, which are race-friendlier than replacing the whole tag list and
are the only way to clear a tag - an empty `tags` array reads as absent. See
[Concurrency](/reference/mcp/sessions-memory-recall/#concurrency-content-hash-and-expect-hash).

Every note mutation now records a `note.written` event, and `notes_read` records
`note.read` - the note-side twin of `memory.read`, which is what lets note demand
count toward [recall's utility nudge](/concepts/recall/#the-utility-nudge).

## Notes are how plans get their narrative

A plan is not a primitive - it is a composition keyed by `plan:<slug>`. Tag a
note `plan:<slug>` and it joins that plan's supporting context, so the next agent
inherits the design and the reasoning behind it, not just the step list. See
[Tasks](/reference/mcp/tasks/) for the step half of the composition.

## Projects and scope

`project_list` and `project_create` manage the scopes everything else inherits.
Most agents never call either: a git repo maps itself on the first session and
resolves its project from the agent's working directory, and the agent's session
carries it - bound automatically under Claude Code or Codex, or by
`session_start`.

The `global` project slug is the deliberate cross-project scope. It is a token
you pass on purpose, never a default you fall into - see the fail-closed rule in
the [MCP API overview](/reference/mcp/).

## access_request asks the owner for a locked project

`access_request` exists only on a daemon whose `peer_auth` locks a project, or
whose access requests (`isolation.requests`) are on without `peer_auth`; every
other install neither lists nor accepts it. It files the same request a call
refused at the lock files by itself, with an optional reason of up to 200
characters that the owner sees quoted and labeled as the agent's own words.

With [access requests](/concepts/project-isolation/#asking-for-access) on, it
also asks for a confidential or sealed project no lock covers, as a refusal at
that project's fence would: what the conversation's home and the project call
for. From a confidential home that is a grant of another confidential project
(kind `grant`, with `from`); from an open home, or for a sealed project from
any home, a move of the conversation into the project for good (kind `move`,
with `from` the home it would leave). `write` asks for read and write: a grant
the owner may still approve read-only, or the move. For a locked project,
`write` files the move or grant whose approval opens its lock too; without it
the ask is for the lock's lease, which opens the project only to a conversation
that works in it. A move still waiting tells the agent, first in `next`, to
record what belongs in its current home, since it can write there no longer
once the move is approved. With access requests off, a project no lock covers
is not locked, and nothing is filed.

The answer says what the call did: `filed` is `true` when it filed a new
request, and `false` when it found one already waiting or the agent already
holds the access; `reason` is `added` when this call's reason was stored on the
request, `kept` when a waiting request already had one, and `none` otherwise.
An agent that already holds the access it asks for is answered
`status: approved` with `open_until`, and nothing is filed, until 30 minutes
before it ends. `access` is what the request asked for; once a grant is
approved, `granted` says what it opened (`read` or `write`). A request for read
and write approved read-only answers with `granted: read`, and its `next` says
the write stays refused rather than to retry.

When the agent's client supports MCP elicitation (Claude Code does, and so do
Codex builds that declare it), the request opens an approval dialog in the
session: the owner sees the daemon's
facts, warnings first, and chooses a duration and Touch ID or a passkey. The
dialog's answer is never the approval - any process of the owner's account
could answer it - the proof is: the Touch ID helper's signature or the
passkey's assertion, checked by the daemon. The call waits up to 110 seconds
for it, then answers with the request id; the owner can still decide later
with `seam approve`, or with a passkey on the request's console page when one
is enrolled. Choosing a passkey in the dialog opens the request's page for the
duration chosen there; the passkey signs it with the facts. Where a passkey is
the only way the dialog can finish, the dialog is that page, which asks the
duration. Cancelling the Touch ID prompt ends the wait at once
(`dialog: touchid_cancelled`; `touchid_closed` when the helper closed without
approving for another reason), the request still pending. A call refused at a
lock opens the same dialog. The dialog works across a daemon restart: the daemon
keeps a connection's declared dialog modes while a lock or read grants are
configured, for clients on the socket, so an agent that was already connected
keeps the dialog. Once approved, retry the call. A call whose asking process is
not an agent's (a tmux pane, a terminal, an ssh login) files nothing and says
why: an approval would cover every program that process started. A move's
dialog never offers Touch ID, which approves no move: it is the request's
passkey page, or none.

On a daemon without `peer_auth` (the advisory tier) the tool is registered in
words of its own: nothing is locked and no proof is asked, so every ask is a
grant or a move. In a client that shows MCP forms, the dialog's **Approve** or
**Deny** decides the request itself, advisory like `seam approve` and the
console's Approve; a grant's form asks for the access and how long, a move's
only for the decision. `dialog` may then also be `changed` (the request changed
while the dialog showed it, so nothing was approved) or `failed` (the decision
did not go through), both leaving the request pending, as declining, cancelling
or 110 seconds without an answer do. The daemon keeps a TCP client's declared
dialog modes in this tier too, so the dialog survives a restart.

## capture_url is SSRF-safe on purpose

`capture_url` fetches a URL and returns its content as markdown. It is the one
tool that makes an outbound request on an agent's behalf, so it is guarded:
destination ports are restricted to `capture.allowed_ports` (80 and 443 by
default, never "any port"), and the fetcher refuses to be talked into reaching
things it should not. See [Configuration](/reference/configuration/).

Scope resolves before the network fetch, so an ambiguous durable destination
fails without making a request. Success returns the note's `id`, `slug`, `title`,
resolved `project`, and `source_url` - the URL as requested; redirects are
followed but do not rewrite it. The fetcher validates the initial URL and every
redirect, rejecting non-HTTP schemes, private/loopback destinations, and
disallowed ports. Size is bounded by truncation rather than rejection: at most
2 MB of the response is read, and the stored readable body is capped again at
50,000 runes with a visible `[content truncated]` marker.
