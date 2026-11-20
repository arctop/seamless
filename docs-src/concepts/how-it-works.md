---
title: How Seamless works
description: One daemon, three surfaces, and a hard line between the files that are truth and the database that is an index.
---

Seamless is a local-first memory and coordination system for coding agents.
The release ships a daemon (`seamlessd`) and its headless client (`seam`); one
daemon process owns one SQLite database and one directory of markdown files,
and by default it listens on loopback on your machine. Agents reach it over MCP,
hooks make sessions ambient for Claude Code and Codex, and you watch through a
read-mostly console. It is deliberately not a distributed system - one
daemon, one port, one data directory, however many computers use it.
Everything below follows from that.

## One daemon, three surfaces

<figure class="doc-figure" aria-labelledby="daemon-map-caption">
  <span class="figure-kicker">Local architecture</span>
  <div class="system-map">
    <div class="map-column">
      <div class="flow-node"><span class="flow-step">MCP</span><strong>Agents</strong><small>Tools and session binding</small></div>
      <div class="flow-node"><span class="flow-step">Command → HTTP</span><strong>Hooks</strong><small>Ambient context and harvest</small></div>
      <div class="flow-node"><span class="flow-step">Browser</span><strong>You</strong><small>Read-mostly console</small></div>
    </div>
    <div class="system-core">
      <svg class="system-core-glyph" viewBox="0 0 100 100" role="img" aria-label="One central daemon connecting three local interfaces to three stores">
        <circle class="orbit" cx="50" cy="50" r="38"/>
        <path class="orbit" d="M17 31L42 45M17 69L42 55M83 24L58 44M83 50H59M83 76L58 56"/>
        <circle class="hub" cx="50" cy="50" r="13"/>
        <circle class="node" cx="16" cy="30" r="4"/><circle class="node" cx="16" cy="70" r="4"/>
        <circle class="node" cx="84" cy="23" r="4"/><circle class="node" cx="84" cy="50" r="4"/><circle class="node" cx="84" cy="77" r="4"/>
      </svg>
      <strong>seamlessd</strong><small>one local process · 127.0.0.1:8081</small>
    </div>
    <div class="map-column">
      <div class="flow-node"><span class="flow-step">Durable truth</span><strong>memory/*.md</strong></div>
      <div class="flow-node"><span class="flow-step">Durable truth</span><strong>notes/*.md</strong></div>
      <div class="flow-node"><span class="flow-step">State + indexes</span><strong>seam.db</strong></div>
    </div>
  </div>
  <figcaption id="daemon-map-caption"><strong>One daemon, three interfaces.</strong> Agents and hooks write through the same process; humans inspect the resulting state in the console.</figcaption>
</figure>

- **Agents** speak MCP at `/api/mcp`. This is the primary interface; the clients
  of Seamless are programs, not people.
- **Hooks** are how sessions become ambient. Claude Code calls them at session
  start, on each prompt, and at session end, so an agent gets briefed and
  harvested without ever choosing to - and its MCP tool calls are
  [bound to the session the hook opened](/concepts/sessions/#process-binding),
  with no `session_start`.
- **You** get a read-mostly console. It is an observability surface, not the way
  the system is driven.

One daemon, one port, one data directory. Not a distributed system, by choice.
Several computers can still share one store: in a
[networked setup](/concepts/networked/) one computer, the Server, runs the
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

<figure class="doc-figure" data-tone="ok" aria-labelledby="write-path-caption">
  <span class="figure-kicker">Write path</span>
  <div class="doc-flow cols-4">
    <div class="flow-node"><span class="flow-step">1 · request</span><strong>memory_write</strong><small>Validate name, content, scope, and fence</small></div>
    <div class="flow-node emphasis"><span class="flow-step">2 · durable act</span><strong>Atomic Markdown write</strong><small>Fail closed when scope is ambiguous</small></div>
    <div class="flow-node"><span class="flow-step">3 · rebuildable</span><strong>FTS + embedding</strong><small>Index the exact file content</small></div>
    <div class="flow-node success"><span class="flow-step">4 · observable</span><strong>Append event</strong><small>Record what happened</small></div>
  </div>
  <figcaption id="write-path-caption"><strong>The file is the durable boundary.</strong> SQLite mirrors make the knowledge searchable and observable.</figcaption>
</figure>

The file write is the one that matters. Indexing after it can fail and be
rebuilt; the file is already on disk.

A write can also stop before step 2. One that would cross a
[confidential or sealed](/concepts/project-isolation/) project's fence, or
reach a project a [peer_auth](/guides/peer-auth/) caller may not, is refused.
With [write screening](/guides/write-screening/) on, a suspicious one is held:
it waits in a queue in the database, never under `memory/` or `notes/`, until
you release it. [Security and privacy](/concepts/security/) covers how these
fit together.

## The read path

<figure class="doc-figure" data-tone="pop" aria-labelledby="read-path-caption">
  <span class="figure-kicker">Three retrieval paths</span>
  <div class="doc-stack">
    <div class="flow-node"><span class="flow-step">SessionStart</span><strong>cwd → project → budgeted briefing → inject</strong><small>Baseline context before the first action</small></div>
    <div class="flow-node"><span class="flow-step">UserPromptSubmit</span><strong>prompt match → &lt;seam-recall&gt; → inject</strong><small>Focused context for the current request</small></div>
    <div class="flow-node"><span class="flow-step">recall</span><strong>FTS5 + cosine → RRF fusion → budget → return</strong><small>Explicit search initiated by the agent</small></div>
  </div>
  <figcaption id="read-path-caption">Automatic briefing, prompt-matched injection, and explicit recall share the same knowledge but answer different moments in an agent run.</figcaption>
</figure>

All three are covered in [Recall](/concepts/recall/) and
[Sessions & briefings](/concepts/sessions/).

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
  ([Security and privacy](/concepts/security/)). Apart from the LLM provider
  you configure, the only requests Seamless makes unprompted are the anonymous
  [update check](/updating/#automatic-update-checks) against GitHub's release
  list and, on an install the installer made, the downloads that
  [install a newer release by itself](/updating/#automatic-updates).
  `update.check: false` turns both off.
- **Not autonomous.** The gardener proposes; you decide. Nothing rewrites your
  knowledge behind your back.
- **Not a chat memory.** It is not trying to remember your conversation. It
  remembers decisions, constraints, and dead ends across many agents and many
  months.
