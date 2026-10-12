# Architecture

> The package layering, what each package owns, three data-flow traces through the real code, and the things Seamless deliberately does not have.

Seamless ships two Go binaries: the `seamlessd` daemon and the `seam` CLI. The
daemon owns the database and serves MCP, hooks, and the console; the CLI is a
headless client of that daemon. Inside the codebase, dependencies are arranged
so their direction is obvious from the package name. This page is the map: the
layers, who owns what, and how a write and a read actually travel through them.

If you are looking for the conceptual model rather than the code, start at
[How Seamless works](https://thereisnospoon.org/docs/concepts/how-it-works/).

## The layering

```text
Dependency direction
Entry points cmd/seamlessd · cmd/seam · cmd/docsgen Wire dependencies and process boundaries.
API surfaces internal/mcp · hooks · console Translate HTTP, MCP, hooks, and UI requests; own no domain policy.
Domains retrieve · lifecycle · gardener · files · plans · capture · importer · update Business rules and reusable workflows.
Foundations store · events · llm · markdown · core · config · validate Leaves, or nearly so; never reach upward.
Imports move downward. Shared behavior moves into a lower layer rather than sideways through another surface.
```

Two rules make this hold, and both are load-bearing:

- **No package imports `cmd/`.** Wiring is the binary's job. A domain package that
  reached back into `cmd/` could not be tested without booting a daemon.
- **No domain package imports another's HTTP layer.** `mcp`, `hooks`, and
  `console` all sit at the same altitude and never call each other. When two of
  them need the same behavior, the behavior moves down a layer - which is why
  `lifecycle.Supersede` exists as a function rather than as a method on the MCP
  server. All three surfaces call it, so none of them owns it.

The layering is not enforced by a tool. It is enforced by the fact that a
violation is visible in the import block.

## Package responsibilities

| Package | Layer | Owns | Imports (internal) |
|---|---|---|---|
| `core` | foundation | Domain types and enums - `Project`, `Memory`, `Session`, `Task`, `Trial`, `Event`, `NewID()`. Pure data, no I/O. | - |
| `config` | foundation | One YAML file plus `SEAMLESS_*` env overrides; env wins over file, file over defaults. | - |
| `validate` | foundation | `Path`, `Name`, `Title` - the guards that stand between agent text and the filesystem. | - |
| `agentproc` | foundation | Names the agent process (`claude` or `codex`) that launched a `seam` process - its nearest non-shell ancestor, as pid plus start time. The SessionStart hook stamps it on the ambient session and the MCP transports send it, which binds a connection to its agent's session. | - |
| `store` | foundation | SQLite: connection setup, migrations, FTS5, embeddings, and every query. Sessions, tasks (including the dependency-aware ready-queue and lease-based claims), trials, proposals, settings, and the retrieval stats live here. | `core`, `config` |
| `events` | foundation | The append-only event log - the single write path for the record of what happened - plus SSE fan-out to subscribers. | `core` |
| `llm` | foundation | Chat and embeddings across OpenAI (default), Ollama, and Anthropic, with the remote/local error taxonomy. | `config` |
| `markdown` | foundation | Rendering a body to HTML with raw HTML disabled and a bluemonday UGC policy on the output. | `core` |
| `files` | domain | The markdown layer: frontmatter parse/render, atomic writes, the watcher, startup reconciliation, and synchronous indexing of what it wrote. | `core`, `llm`, `store`, `validate` |
| `lifecycle` | domain | Supersession, archival, provenance. The only writer of `invalid_at`/`superseded_by`. | `core`, `files` |
| `retrieve` | domain | The briefing assembler, the prompt-context matcher, and recall/search (FTS5 + cosine, RRF-fused). | `config`, `core`, `llm`, `plans`, `store` |
| `gardener` | domain | The timed passes (dedup, staleness, digest, stale-plan) and request-driven interpretation. Writes proposals; never mutates a memory. | `config`, `core`, `events`, `files`, `lifecycle`, `llm`, `plans`, `store` |
| `plans` | domain | The captured-plan vocabulary: note-slug prefixes, the `plan-status` tag lifecycle, the tracking-task composition. One home so the tag spellings cannot drift. | `core`, `store` |
| `capture` | domain | SSRF-safe URL fetch: private-IP rejection, a pinned dialer, a port allowlist, redirect validation, a size cap. | - |
| `archive` | domain | Instance archives: `VACUUM INTO` snapshot + corpus + manifest out, guarded tar extraction and restore-or-merge back in. Never imports `config` - an archive is described by its manifest and the data dir it is handed, not by whichever process is holding it. | `core`, `files`, `llm`, `store`, `validate` |
| `importer` | domain | One-way migration from the v1 store. Reads v1, writes v2, never modifies v1. | `core`, `files`, `store` |
| `update` | domain | The background update check and automatic updates: the GitHub release list, install-kind detection (`Detect`, a pure function of the probe `cmd/seamlessd` gathers), the policy that picks a release and a moment (`Target`, `Decide`), folding the updater's attempt records into blocks, backoff, holds and pauses, `<data_dir>/update/state.json`, and the `Status` every surface renders. It decides and records; installing is the detached `seamlessd update --auto` that `cmd/seamlessd` starts. | `config`, `core` |
| `mcp` | surface | The tool surface over streamable HTTP, plus session bindings (per connection, and by agent process) and scope resolution. | `agentproc`, `capture`, `core`, `events`, `files`, `gardener`, `lifecycle`, `llm`, `plans`, `retrieve`, `store`, `validate` |
| `hooks` | surface | Shared Claude Code/Codex hook endpoints and adapters, ambient sessions, bounded injection, findings harvest, and Claude-specific plan capture. | `agentproc`, `config`, `core`, `events`, `files`, `plans`, `retrieve`, `store`, `validate` |
| `console` | surface | The server-rendered observability UI and its SSE feed. | `config`, `core`, `events`, `files`, `gardener`, `lifecycle`, `markdown`, `plans`, `retrieve`, `store` |

The import columns are the real ones, and they are the quickest way to check a
change: a foundation package that grows an edge to a domain package has inverted
the layering.

## Trace: `memory_write`, from tool call to file on disk

```text
memory_write trace
MCP boundary · 1–4 Validate and resolve Check content; fail closed on scope; reuse or mint the ULID; request an advisory dedup hint.
Files boundary · 5–6 Protect the path Reject another item's path and suppress the watcher's echo.
Durable + index · 7–9 Atomic file, FTS, vector Render and atomically write Markdown; synchronously index; embed best-effort with hash retry.
Domain outcome · 10–11 Record and supersede Append memory.written ; invoke lifecycle supersession only when requested.
The path crosses the MCP surface once, the files domain once, then returns to record the outcome and optional lifecycle change.
```

Five things in that path are decisions, not mechanics:

**Step 2 fails closed.** `resolveWriteScope` returns `errNoScope` when there is
no explicit project, no bound session, and no ambient session to inherit from.
The alternative - defaulting to global - puts a project's private knowledge in
front of every agent on the machine, silently. See
[Domain invariants](https://thereisnospoon.org/docs/internals/invariants/).

**Step 4 cannot fail the write.** `DedupHint` swallows every embedding error on
purpose. A dedup hint is advisory; a down embedder must not stop an agent from
recording what it learned.

**Step 5 refuses rather than overwrites.** A superseded memory keeps its
tombstone file at `memory/{project}/{name}.md`, so that name stays occupied. A
write that landed on it would destroy readable supersession history.
`memory_delete` is the only way to free the name.

**Step 7 is the whole point.** The file write is the durable act. Everything
after it - the index row, the FTS row, the vector - is rebuildable from disk by
`files.Reconcile` at startup. This is why the ordering is file first, index
second, and why the index is allowed to be best-effort while the file is not.

**Step 11 is a tool error, not a field in a success payload.** If the new memory
is written but superseding the old one fails, the call returns an error naming
the target that is *still active*. An error embedded in a success payload reads
as success to an agent, which would then leave two contradictory memories live.

## Trace: SessionStart, from Claude Code to injected briefing

```text
SessionStart trace
Claude → hook · 1–3 Authenticate, bound, map seam hook session-start forwards stdin; only auth and request shape can return non-2xx; work is capped at two seconds; cwd grows the project map.
Retrieve · 4–8 Resolve effective scope Merge runtime settings, resolve cwd and family scope, load active memories, partition pinned kinds; subagents take constraints plus spawn-prompt-matched RELEVANT lines.
Pack · 9–11 Trim only eligible context Apply recency after partitioning, add findings/tasks/family/plan signals, then pack to budget and hard cap.
Hook response · 12–15 Bind, inject, record Create or resume the ambient session, stamp the agent process on it, append its line, record the exact text sent, and return additionalContext .
The hook fails open after authentication: a retrieval problem may remove context, but it cannot stop the agent's turn.
```

The contract that shapes this path is **fail open**. Every error between steps 3
and 13 is logged and swallowed; the briefing degrades to empty and the agent
proceeds. Only a bad bearer key or a malformed `client` query parameter - both
request defects, not retrieval work - return non-2xx. A memory system that can block
an agent from working is worse than no memory system, so the failure mode is
chosen deliberately: silence, not obstruction. The cost is that hook failure is
invisible, which is why `seamlessd doctor` and `seam doctor` exist - see
[Claude Code hooks](https://thereisnospoon.org/docs/reference/hooks/).

Two orderings inside it are also deliberate. Step 9 runs *after* step 8 so that
`constraint` and `stage` memories are never age-filtered or dropped for budget -
they are partitioned out before the trim ever sees them. And step 14 runs after
step 13 so the recorded event contains exactly the text the agent received,
ambient line included, rather than the briefing as it looked one step earlier.

## Trace: an automatic update, from decision to restarted daemon

```text
Automatic update trace
Checker · every minute Fold, target, decide Settle the updater's last record (re-read every few seconds while an update runs); pick the newest release that has soaked by GitHub's clock, carries signed checksums and is neither blocked nor held; wait for an idle daemon or a lull past max_defer .
Spawn Record, then hand off Re-check a release list older than a minute; save the spawn and record update.started ; the OS spawner starts seamlessd update --auto outside the service.
Updater Verify, back up, install Take update.lock ; re-run the gates; verify the release's installer and checksums against its exact tag; prove the rollback; archive the instance; run the installer, which restarts the service.
Confirm Observe, or roll back A new daemon of the new release, the same one ten seconds later; otherwise put the database back if it was migrated and reinstall the old release. The record goes to attempt.json for the new daemon to fold.
The daemon decides and records; a separate process, outside the service, does the installing.
```

Three things in that path are decisions, not mechanics:

**The decision and the act live in different processes.** The daemon is the
thing being replaced, so it cannot install itself: the installer's service
restart would kill it mid-swap. It starts the updater where that restart cannot
reach (per OS, [The updater on your OS](https://thereisnospoon.org/docs/reference/service/#the-updater)), and
the two talk only through files. The daemon writes `state.json`; the updater,
holding `update.lock`, writes `attempt.json` and `attempts.jsonl`. Every release
reads every other release's records, so their format only ever gains fields
(see [Domain invariants](https://thereisnospoon.org/docs/internals/invariants/#automatic-updates)).

**There is one upgrade implementation.** The updater runs the same installer
script a fresh install runs, after verifying its Sigstore bundle in-process.
It fetches only installers, checksums and their signatures, and swaps no
binary itself. A manual `seamlessd update` runs the same engine, attended.

**Success is observed, not reported.** The installer's exit code decides
nothing. The update counts only once `/healthz` answers from a new process of
the new release, the one the update state names as running, and the same one
ten seconds later. An installer that exits non-zero while the new release
serves is "applied with warnings". A release that never answers is rolled back
and blocked for automatic updates.

## The deliberate no's

Each of these is a thing Seamless could have and does not.

**No CGO.** The database is `modernc.org/sqlite`, a pure-Go SQLite. `go build`
produces a static binary that cross-compiles and needs no toolchain on the target
machine. CGO would buy a faster driver at the price of a build that breaks
differently on every machine - a bad trade for a tool whose main virtue is that
it is one file you run locally.

**No vector database.** Embeddings are little-endian float32 BLOBs in the
`embeddings` table, and similarity is brute-force cosine in Go. There is no ANN
index and no second service. A personal knowledge store is thousands of items,
not billions; an exact scan over that is simple, has no index to rebuild, no
recall/latency tuning curve, and no separate process to keep alive. Adding
ChromaDB would double the number of things that must be running for recall to
work, in exchange for solving a scale problem nobody here has.

**No JWT, no users, no registration.** One static bearer key guards `/api/mcp`,
the hook endpoints, and the console, and the daemon binds `127.0.0.1`. The
security boundary is the loopback interface and the OS user account - the same
boundary that already protects `~/.ssh`. Sessions, refresh, and revocation are
the machinery of multi-user auth over a hostile network. There is no network here
and there is one user, so that machinery would be pure ceremony: more code, more
failure modes, and no threat it actually removes.

**No node, no npm, no React, no build step.** The console is `html/template`,
vanilla JS, and SSE. It exists so that an agent can edit a console page with the
Go toolchain it already has, and so `make build` produces the UI along with the
binary. A frontend build step would mean a second dependency tree, a second
lockfile, and a class of "works in dev, stale in prod" bugs - for a read-mostly
observability surface with no client-side state worth a framework.

**ULIDs, never UUIDs.** Every identity is a ULID via `core.NewID()`. ULIDs sort
lexicographically by creation time, so an id column is a timeline: `ORDER BY id`
is chronological, and a range scan over ids is a range scan over time. UUIDv4
throws that away for randomness this system does not need. (The console displays
the *last* 8 characters of an id, precisely because the leading characters are
the timestamp and recent ids all share them.)

## Where to go next

- [Domain invariants](https://thereisnospoon.org/docs/internals/invariants/) - the rules that plausible-looking
  code breaks.
- [Contributing](https://thereisnospoon.org/docs/internals/contributing/) - the `make check` gate and how to add
  a tool.
- [Storage layout](https://thereisnospoon.org/docs/reference/storage/) - what is on disk and what is in SQLite.
