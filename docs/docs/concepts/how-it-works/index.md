# How Seamless works

> One daemon, three surfaces, and a hard line between the files that are truth and the database that is an index.

Seamless is a local-first memory and coordination system for coding agents.
The release ships a daemon (`seamlessd`) and its headless client (`seam`); one
daemon process owns one SQLite database and one directory of markdown files,
and by default it listens on loopback on your machine. Agents reach it over MCP,
hooks make sessions ambient for Claude Code and Codex, and you watch through a
read-mostly console. It is deliberately not a distributed system - one
daemon, one port, one data directory, however many computers use it.
Everything below follows from that.

## One daemon, three surfaces

```text
Local architecture
MCP Agents Tools and session binding
Command → HTTP Hooks Ambient context and harvest
Browser You Read-mostly console
seamlessd one local process · 127.0.0.1:8081
Durable truth memory/*.md
Durable truth notes/*.md
State + indexes seam.db
One daemon, three interfaces. Agents and hooks write through the same process; humans inspect the resulting state in the console.
```

- **Agents** speak MCP at `/api/mcp`. This is the primary interface; the clients
  of Seamless are programs, not people.
- **Hooks** are how sessions become ambient. Claude Code calls them at session
  start, on each prompt, and at session end, so an agent gets briefed and
  harvested without ever choosing to - and its MCP tool calls are
  [bound to the session the hook opened](https://thereisnospoon.org/docs/concepts/sessions/#process-binding),
  with no `session_start`.
- **You** get a read-mostly console. It is an observability surface, not the way
  the system is driven.

One daemon, one port, one data directory. Not a distributed system, by choice.
Several computers can still share one store: in a
[networked setup](https://thereisnospoon.org/docs/concepts/networked/) one computer, the Server, runs the
daemon and keeps the data, and the others are Clients that keep no data of
their own and reach it over the network, normally with TLS.

## Files are truth; the database is an index

This is the load-bearing decision in the whole design.

| | Lives in | Why |
|---|---|---|
| Memories, notes | **Markdown files** | Durable knowledge you own: readable, greppable, editable, git-able |
| Sessions, tasks, trials, events, telemetry | **SQLite** | High-churn state with no reason to be a file |
| FTS index, embeddings | **SQLite** | Derived data, rebuildable from the files |

So: **what would you lose if `seam.db` were deleted?** The sessions, the task
queue, the trials, and the event log - the record of what *happened*. You would
not lose a single memory or note, because those are files, and startup
reconciliation rebuilds their index from the disk.

That asymmetry is the point. The knowledge is yours in the strongest sense: it
survives this program. If Seamless disappeared tomorrow, you would still have a
folder of markdown files that a human - or any other tool - can read.

## The write path

```text
Write path
1 · request memory_write Validate name, content, scope, and fence
2 · durable act Atomic Markdown write Fail closed when scope is ambiguous
3 · rebuildable FTS + embedding Index the exact file content
4 · observable Append event Record what happened
The file is the durable boundary. SQLite mirrors make the knowledge searchable and observable.
```

The file write is the one that matters. Indexing after it can fail and be
rebuilt; the file is already on disk.

A write can also stop before step 2. One that would cross a
[confidential or sealed](https://thereisnospoon.org/docs/concepts/project-isolation/) project's fence, or
reach a project a [peer_auth](https://thereisnospoon.org/docs/guides/peer-auth/) caller may not, is refused.
With [write screening](https://thereisnospoon.org/docs/guides/write-screening/) on, a suspicious one is held:
it waits in a queue in the database, never under `memory/` or `notes/`, until
you release it. [Security and privacy](https://thereisnospoon.org/docs/concepts/security/) covers how these
fit together.

## The read path

```text
Three retrieval paths
SessionStart cwd → project → budgeted briefing → inject Baseline context before the first action
UserPromptSubmit prompt match → <seam-recall> → inject Focused context for the current request
recall FTS5 + cosine → RRF fusion → budget → return Explicit search initiated by the agent
Automatic briefing, prompt-matched injection, and explicit recall share the same knowledge but answer different moments in an agent run.
```

All three are covered in [Recall](https://thereisnospoon.org/docs/concepts/recall/) and
[Sessions & briefings](https://thereisnospoon.org/docs/concepts/sessions/).

## What Seamless is not

- **Not a RAG pipeline over your codebase.** It stores what agents *learned*, not
  what the code says. The code is already in the repo; duplicating it into memory
  is how a store fills with noise.
- **Not a vector database.** Vectors are float32 blobs in SQLite, scanned
  exactly. No ANN index, no separate service.
- **Not a cloud service.** No account, no sync, and no outbound product
  telemetry. The event and retrieval telemetry shown in the console goes to no
  one but you. By default the daemon listens on loopback and
  every caller presents one static key; a Server's paired computers hold
  credentials of their own, a peer_auth daemon names each caller by its uid
  instead of a key, and the console can sign in with a passkey
  ([Security and privacy](https://thereisnospoon.org/docs/concepts/security/)). Apart from the LLM provider
  you configure, the only requests Seamless makes unprompted are the anonymous
  [update check](https://thereisnospoon.org/docs/updating/#automatic-update-checks) against GitHub's release
  list and, on an install the installer made, the downloads that
  [install a newer release by itself](https://thereisnospoon.org/docs/updating/#automatic-updates).
  `update.check: false` turns both off.
- **Not autonomous.** The gardener proposes; you decide. Nothing rewrites your
  knowledge behind your back.
- **Not a chat memory.** It is not trying to remember your conversation. It
  remembers decisions, constraints, and dead ends across many agents and many
  months.
