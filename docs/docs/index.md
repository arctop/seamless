# What is Seamless?

> One shared memory and one task queue for the AI coding agents you run - Claude Code, Codex, any MCP client - with the memory kept as markdown files on your own computer.

Seamless is shared, persistent memory and a task queue for the AI coding agents
you run: Claude Code, Codex, and any other MCP client. What one session learns,
every later session starts with, in any of those clients. Agents working side by
side claim tasks from one queue instead of picking the same one, and plans get
executed together. All of it is stored as markdown files in a directory you
own, indexed by one SQLite database, and served over MCP by the `seamlessd`
daemon on your own computer. The companion `seam` CLI gives headless agents a
direct interface. There is no hosted Seamless service, external vector
database, or account.

The same store also works from chats in the Claude desktop app, for long-running
work that isn't code: [Seamless in the Claude app](https://thereisnospoon.org/claude-app/)
covers that side in plain terms.

## Which agent do you run?

Setup is client-shaped: pick yours and that page walks install, wiring, and
verification end to end. The [Quickstart](https://thereisnospoon.org/docs/quickstart/) is the same path for
everyone - one install command, then your client.

<div class="card-grid router-grid">
  <a class="doc-card router-card" href="claude-code/">
    <span class="card-glyph" aria-hidden="true"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M4 17l6-5-6-5"/><path d="M12 19h8"/></svg></span>
    <h2>Claude Code</h2>
    <p>The terminal CLI and the Claude app's code sessions - one setup covers both.</p>
    <span class="card-cta">Set up</span>
  </a>
  <a class="doc-card router-card" href="claude-app/">
    <span class="card-glyph" aria-hidden="true"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z"/></svg></span>
    <h2>Claude app chat</h2>
    <p>Chat conversations in the desktop app - MCP only, no hooks, wired through the app's own config.</p>
    <span class="card-cta">Set up</span>
  </a>
  <a class="doc-card router-card" href="codex-cli/">
    <span class="card-glyph" aria-hidden="true"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M4 17l6-5-6-5"/><path d="M12 19h8"/></svg></span>
    <h2>Codex</h2>
    <p>CLI, desktop app, and IDE extension - one shared local profile named codex.</p>
    <span class="card-cta">Set up</span>
  </a>
  <a class="doc-card router-card" href="guides/mcp-clients/">
    <span class="card-glyph" aria-hidden="true"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M9 2v6M15 2v6"/><path d="M6 8h12v3a6 6 0 0 1-12 0z"/><path d="M12 17v5"/></svg></span>
    <h2>Any other MCP client</h2>
    <p>Cursor, Cline, Windsurf, Zed, or an agent you wrote - point it at the MCP endpoint.</p>
    <span class="card-cta">Set up</span>
  </a>
</div>

## Or start from the task

| If you want to | Read |
|---|---|
| Install from zero in ten minutes | [Quickstart](https://thereisnospoon.org/docs/quickstart/) |
| Update, uninstall, or add a client later | [Update & uninstall](https://thereisnospoon.org/docs/updating/) |
| Control the service, find logs and paths | [The service & where things live](https://thereisnospoon.org/docs/reference/service/) |
| Understand the model before trusting it | [How Seamless works](https://thereisnospoon.org/docs/concepts/how-it-works/) |
| Know what is protected, and from whom | [Security and privacy](https://thereisnospoon.org/docs/concepts/security/) |
| Keep a client's project away from your other work | [Project isolation](https://thereisnospoon.org/docs/concepts/project-isolation/) |
| Share one memory across your computers | [Networked Seamless](https://thereisnospoon.org/docs/concepts/networked/) |
| Point a hand-rolled agent at it | [Integrate your agent](https://thereisnospoon.org/docs/guides/integrate-your-agent/) |
| Carry what one Claude Code session learned into the next | [Make Claude Code remember between sessions](https://thereisnospoon.org/docs/guides/claude-code-memory-between-sessions/) |
| Give Claude Code and Codex the same memory | [Share memory between Claude Code and Codex](https://thereisnospoon.org/docs/guides/share-memory-claude-code-codex/) |
| Run several agents on one backlog without relaying between them | [Coordinate multiple agents](https://thereisnospoon.org/docs/guides/coordinate-agents/) |
| Look up a tool, key, or command | [Reference](https://thereisnospoon.org/docs/reference/) |
| Fix something that is silently not working | [Troubleshooting](https://thereisnospoon.org/docs/guides/troubleshooting/) |

Its clients are agents. You are the observer and editor - there is a console,
and agents use Seamless without you in the loop until you ask to be: lock a
project and they wait for your approval, turn on write screening and suspicious
writes wait for your review. It is personal, and it is yours to run: one
daemon on your computer, or one Server your other computers pair with. There
is no hosted team knowledge base behind it.

## What it gives a fleet

- **Memory with a lifecycle.** Not an append-only log: memories are superseded
  and archived, so what an agent recalls is what is currently true.
- **Ambient sessions.** Claude Code and Codex hooks open a session per agent,
  inject a budgeted briefing at startup, and harvest findings. No tool calls are
  required from the agent.
- **A ready-queue.** Dependency-aware tasks with atomic lease-based claiming, so
  parallel agents divide work without stepping on each other.
- **Hybrid recall.** One search entry point fusing keyword and vector search.
- **A console you can read.** Every memory, session, task, and retrieval decision
  is inspectable. The files are plain markdown; the store is not a black box.
- **Walls you choose.** Fence a project so its knowledge stays put, hold
  suspicious writes for review, and lock a project so even its own agents wait
  for your Touch ID, passkey, or password ([security and privacy](https://thereisnospoon.org/docs/concepts/security/)).

## Design principles

**Files are the source of truth.** Durable knowledge lives in markdown you can
read, `grep`, edit, and put in git. The database is a rebuildable index over
them, plus the record for high-churn state (sessions, tasks, events).

**Local-first.** One daemon process and one SQLite file, listening on loopback
unless you set up a Server for your other computers. No external database, no
required cloud service, and no product telemetry. On its own it only checks
GitHub for new releases, anonymously, and downloads the updates it installs;
`update.check: false` turns that off. Text goes to a model provider only if you
add one ([what leaves your computer](https://thereisnospoon.org/docs/concepts/security/#outbound)).

**Propose, don't act.** The gardener finds duplicates, staleness, and drift - and
proposes. Nothing rewrites your knowledge behind your back.
