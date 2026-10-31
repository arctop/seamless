# CLAUDE.md

Quick-reference for AI assistants working in the Seamless repository. For coding
conventions see `AGENTS.md`.

## What is Seamless?

Seamless is a local-first agent memory and coordination substrate. Its clients are
AI agents (Claude Code and friends); the human is an observer/editor. It provides
persistent memory with a lifecycle (supersession, provenance, archival, a
gardener), ambient sessions via Claude Code hooks, a dependency-aware task
ready-queue, research trials, hybrid recall, and a read-mostly observability
console. Go backend, no CGO, single binary + CLI.

It is a ground-up rebuild of Seam v1 (a private predecessor, read-only
reference). As of the P6 cutover (2026-07-10) Seamless is the sole agent-memory system, serving on
port 8081 with data dir `~/.seamless` and env prefix `SEAMLESS_*`. v1 is
decommissioned (launchd services disabled, port 8080 freed) with `~/.seam`
preserved read-only as a fallback archive.

## Project structure

```
cmd/seamlessd/     server daemon: serve, doctor, export, import, install-hooks,
                   map-repo, unmap-repo, retire-project, family, console-open
cmd/seam/          headless CLI (agents + owner observability)  [P2/P5]
cmd/docsgen/       docs site generator: docs-src/ -> docs/docs/ (see SITE.md)
cmd/demoseed/      thin CLI over internal/demokit: the console-fleet seed
                   (branding screenshots) and `-scenes` (the shared fixture)
cmd/seambench/     agent-scenario benchmark: `run` (headless scenario x condition
                   matrix) + `report` (grade, uplift, version deltas). Driven by
                   `make seambench`; see cmd/seambench/README.md
docs-src/          docs site markdown + nav.yaml (source; docs/docs/ is output)
internal/core/     domain types: Project, Memory, Session, Task, Trial, Event
internal/config/   single YAML + env config (static key, budgets, briefing tunables, llm,
                   peer_auth)
internal/access/   peer_auth policy: who a request speaks for (the owner on TCP, a
                   kernel-named uid on the Unix socket) and which projects it may
                   reach (View); Unix-socket peer credentials. Foundation layer
internal/store/    SQLite: schema, FTS5, embeddings (BLOB + cosine), migrations;
                   index_work.go mirrors the DB-native work record (tasks,
                   trials, session findings) into the same fts table the files
                   layer fills with memories and notes
internal/events/   append-only event log; SSE fan-out; retrieval stats
internal/validate/ path/title/name guards
internal/agentproc/ names the agent process (claude/codex) that launched a seam
                   process: the identity the session-start hook stamps and the
                   MCP bridge sends, which binds a connection to its session
internal/files/    markdown layer: frontmatter, atomic writes, watcher     [P1]
internal/markdown/ agent markdown -> sanitized console HTML (goldmark + bluemonday)
internal/llm/      OpenAI (default), Ollama, Anthropic chat + embeddings   [P1]
internal/retrieve/ briefing assembler, prompt-context matcher, recall (RRF),
                   console search                                          [P2/P4]
internal/lifecycle/ supersession, archival, provenance                     [P3]
# (the dependency-aware ready-queue + lease-based claiming live in
#  internal/store/tasks*.go -- there is no internal/tasks package)
internal/gardener/ the propose-only passes (dedup, staleness, stale-stage,
                   dead-weight, digest, stale-plan, memory-wanted -- the
                   canonical list is RunOnce), plus the request- and
                   split-driven proposals                                  [P4]
internal/plans/    captured CC plan vocabulary: tags, statuses, tracking task
internal/mcp/      MCP tools (streamable HTTP, static bearer key or the peer_auth
                   socket); see ToolCount [P2+]
internal/hooks/    session hooks + CC plan-mode capture (PostToolUse etc.) [P2/P3]
internal/console/  server-rendered observability UI (html/template + SSE)  [P5]
internal/capture/  SSRF-safe URL fetch                                     [P4]
internal/update/   update check + automatic updates: GitHub release list,
                   install-kind detection, Target/Decide policy, attempt-record
                   fold, <data_dir>/update/state.json, the Status every surface
                   renders (briefing notice, console, doctor, /healthz). Never
                   installs in-process: the daemon spawns a detached `seamlessd
                   update --auto` (cmd/seamlessd/update_spawn*.go) to run the
                   release's installer. Imports config + core only
internal/archive/  instance archives: VACUUM INTO snapshot + corpus + manifest
                   out; guarded tar extract and restore-or-merge back in.
                   Never imports config
internal/importer/ one-shot import of the v1 (~/.seam) snapshot
internal/demokit/  importable fixture-seeding primitives + the demo spec sets;
                   the shared seam under BOTH branding and the benchmark
internal/bench/    agent-scenario benchmark: scenario table, condition arms,
                   graders (repo assertions + event log + optional LLM judge),
                   results/uplift aggregation, the on-disk artifact contract
scripts/fixture/   the shared throwaway fixture: make-myapp.sh (demo repo) +
                   harness.sh --mode record|bench (branding | benchmark arms)
scripts/branding/  GH-Pages surface: record.sh, distill.py (verbatim-audited
                   scene data), console-shots.js -- see its README.md
```

Bracketed tags mark the phase that introduces the package; unbuilt ones do not
exist yet.

## Common commands

```bash
make build      # ./bin/seamlessd + ./bin/seam (touches nothing live)
make install    # build + snapshot binaries/config to ~/.local/bin +
                # ~/.config/seamless, repoint the service and hooks, restart
make test       # unit tests
make test-race  # unit tests with the race detector
make check      # the full gate: build + vet + cross-check (darwin/windows
                # vet) + fmt-check + docs-check + installer-check +
                # site-check + lint + vulncheck + test-race
make check-fast # the pre-commit subset (no build/vulncheck/test-race);
                # .githooks/pre-commit runs it
make lint       # golangci-lint
make vet        # go vet
make fmt        # gofmt tracked files -- NOT `gofmt -w .`, which rewrites other
                # agents' worktrees under dot-dirs that go's ./... skips
make run        # build + serve on 127.0.0.1:8081
make doctor     # build + config/DB self-checks
make console    # open the console in a browser, pre-authenticated
make clean      # remove bin/ and coverage files

make docs       # regenerate the docs site (docs-src/ -> docs/docs/, committed)
make docs-check # fail if the committed docs site is stale (runs inside `check`)
make docs-serve # regenerate + serve the site at 127.0.0.1:8899/docs/
make site-check # fail if the hand-written landing page (docs/index.html) drifts
                # from the installer or the CLI -- docs-check cannot see it
make indexnow   # submit the sitemap's URLs to the IndexNow engines (manual,
                # run after a deploy is live; DRY=1 previews the payload)
make metrics    # GitHub stars + release-asset download counts (needs gh)

# single test
go test ./internal/validate -run TestTitle -v

# Two different things called "benchmark". Do not conflate them:
make bench      # Go hot-path MICRO-benchmarks (ns/op, offline, free)
make seambench  # the AGENT-SCENARIO benchmark: runs a real `claude` over the
                # seeded fixture in each condition arm (vanilla/mechanism/full),
                # grades it, and prints with-vs-without uplift + version deltas.
                # SPENDS REAL API TOKENS; needs a `claude` binary + credentials.
                # MANUAL ONLY -- deliberately not in check, check-fast, the
                # pre-commit hook, CI, the release flow, or any schedule.
                # N=5, BASELINE=<ref>, SCENARIO=, CONDITIONS=, OUT=,
                # RUN_ARGS=, REPORT_ARGS=--no-trials. See cmd/seambench/README.md
```

## Tech stack

| Layer | Choice |
|---|---|
| Language | Go 1.26+ (no CGO) |
| Database | SQLite (`modernc.org/sqlite`, WAL, FTS5) |
| Vectors | float32 BLOBs in SQLite + brute-force cosine (no ChromaDB) |
| LLM/embeddings | OpenAI (default, first-class), Ollama, Anthropic |
| IDs | ULID (`oklog/ulid/v2`), never UUID |
| Auth | single static bearer key, localhost bind (no JWT); optional peer_auth Unix socket that names each caller by its kernel-reported uid |
| Console | `html/template` + vanilla JS + SSE (no node/React) |
| Tests | testify/require, table-driven, fresh/in-memory SQLite |

## Configuration

Single YAML file (`$SEAMLESS_CONFIG`, `~/.config/seamless/seamless.yaml`, or
`./seamless.yaml`; see `seamless.yaml.example` for every key) with `SEAMLESS_*`
env overrides -- env wins over file, file over defaults. The `briefing:` block
tunes what the SessionStart `<seam-briefing>` auto-injects (section counts,
recency filters, family cross-over, hard-cap multiplier, and the
utility-vs-recency index order via utility_mode/utility_weight); those knobs are also
editable live in the console (Settings -> Briefing injection), which stores a
runtime override in the DB that wins over file/env until reset and applies from
the next session start without a daemon restart. `gardener.session_idle_minutes`
is the single live/idle threshold shared by the session reaper, the console and
automatic updates' idle gate. The `update:` block (check, check_interval, auto,
max_defer, min_age) drives the update check and automatic updates; its console
override (Settings -> Updates) merges the other way round: `check: false` or
`auto: false` in the file/env is final and locks its toggle (false check means
no update traffic at all). Durations are `config.Duration` ("6h"; a bare
number other than 0 is refused). The `peer_auth:` block (off by default; see
docs-src/guides/peer-auth.md) adds a Unix-socket listener whose callers are
named by the kernel's peer uid and maps OS accounts to the projects they may
reach; a client of it sets `server_url: unix:///path.sock` and needs no key.

## Storage layout

```
~/.seamless/
  seam.db                          SQLite: indexes, sessions, tasks, trials, events, embeddings, ...
  seamlessd.lock                   serve's one-daemon-per-data-dir lock (holder's PID; never delete it)
  memory/{project|_global}/{name}.md   one memory per file (source of truth)
  notes/{project|_global}/{slug}.md    work artifacts (source of truth)
  backups/pre-update-v*.tar.gz     the updater's pre-update archives (newest 2)
  update/state.json                the update check's record (release daemons only)
  update/update.lock               the updater's lock (never delete it)
  update/attempt.json, attempts.jsonl, logs/   the updater's records (it writes, the daemon reads)
```

Files are the source of truth for durable knowledge; the DB is the record for
high-churn state (sessions, tasks, trials, events, telemetry) and a rebuildable
index for the files.

Under `peer_auth` the data dir belongs to the daemon's own OS account instead
(the guide's layout: `/opt/seamless/data`, 0700), and serve refuses to start
when it, the socket's directory, the config file or the binary could be
rewritten by another account.

## Memory frontmatter

```yaml
---
id: 01K...                 # ULID
kind: gotcha               # constraint|convention|runbook|protocol|gotcha|decision|refuted|reference|stage
name: chroma-boot-race
description: one line, <=150 chars -- the ONLY text shown in indexes
project: seam              # empty = global
created: 2026-07-10T18:00:00Z
updated: 2026-07-10T18:00:00Z
valid_from: 2026-07-10T18:00:00Z
invalid_at: null           # set on supersession/archive; invalid memories leave indexes
superseded_by: null        # ULID of the replacement
source_session: cc/ab12cd34
model: claude-fable-5      # the model that produced the content, verbatim as the provider names it
tags: [x, y]
---
body markdown
```

## MCP surface

The authoritative count is `internal/mcp.ToolCount` (asserted by doctor and by
`catalog_test`); `mcp.Catalog()` is the same list as data, and the docs site
renders its reference from it. Do not transcribe a number here -- three different
stale counts is exactly how this section drifted before.

sessions (`session_start/update/end`), memory (`memory_write/append/read/delete`,
write carries `supersedes`), discovery (`recall` -- the only search tool,
RRF-fused, spanning durable knowledge AND the work record: memories, notes,
tasks, trials, session findings),
notes (`notes_create/read/update/append/delete`), projects (`project_list/create`),
tasks (`tasks_add/update/ready/list` -- `tasks_list id=<id>` loads a single task
by its globally-unique id -- plus `tasks_claim`/`tasks_release` for
atomic lease-based claiming), research lab (`lab_open`, `trial_record`,
`trial_query`), gardener (`gardener_proposals`, `gardener_apply`, plus the
natural-language `gardener_request` and the project-split planner
`gardener_split` -- both LLM-backed, and both only ever propose), utility
(`capture_url`, `usage_summary`, `favorite_set` -- star/unstar any entity;
starred memories pin into briefings and boost recall). Project and session are inherited from the
session binding; agents in mapped repos rarely pass `project` explicitly. A
Claude Code or Codex agent's connection is bound to its own ambient session
automatically (the agent-process identity, `internal/mcp/agent_binding.go`), so
those agents never need `session_start`.

## Plans as composition

A "plan" is not a primitive -- it is a composition keyed by `plan:<slug>`:

- **Narrative + supporting knowledge**: one primary note, plus any further notes
  tagged `plan:<slug>`, so the next agent inherits the design and its supporting
  context. `notes_create` already accepts `tags`.
- **Steps**: tasks created with `plan:<slug>` (`tasks_add plan=...`). Plan-step
  tasks are excluded from the default `tasks_ready`/`tasks_list` (no queue
  pollution) and surfaced with `plan=<slug>`.
- **Claiming**: `tasks_claim` atomically moves a ready task to `in_progress` with
  a lease (default 900s); re-claiming refreshes the lease (heartbeat); an expired
  lease is reclaimable. `tasks_release` (or closing the task, or `session_end`)
  frees it. The briefing surfaces each active plan as a `PLAN: <slug> -- ...` line.

Claude Code plan mode feeds this automatically (`internal/hooks` capture +
`internal/plans` vocabulary): plan-file saves upsert a `cc-plan-<basename>` note
(`plan-status:draft|presented|approved|abandoned|shipped` tag -- the last two
are the gardener's stale-plan settlements: shipped when git evidence shows the
work landed unapproved, abandoned otherwise), planning subagents cache
as `cc-agent-<id>` notes in the composition, approval creates the tracking task,
and the briefing lists unapproved captures as `PLAN (awaiting approval)` lines.
Owner surfaces: `/console/plans` and `seam plan list|show|check|approve` (check =
git-stamp staleness; approve = escape hatch when CC skips the approval hook).

## One fixture, two workflows

`scripts/fixture/harness.sh` stands up the shared throwaway fixture -- the
`myapp` demo repo plus a seeded Seamless instance on a non-live port, never the
live `~/.seamless`/`~/.claude`/`~/.codex`. Above that seam the two halves are
independent and non-interfering:

- **Branding** (`--mode record`): record interactive with/without takes, distill
  them **verbatim** into `docs/static/scenes.js`, publish. Entry point
  `scripts/branding/record.sh`; recipe in `scripts/branding/README.md`.
- **Benchmark** (`--mode bench`): build the `vanilla`/`mechanism`/`full`
  condition arms, run scenarios **headless**, grade, report uplift. Entry point
  `make seambench`; recipe in `cmd/seambench/README.md`.

The demo repo must contain zero Seamless vocabulary (memory
`scene-demo-repo-must-be-seamless-free`), or the without/vanilla side reads its
way out of character. Grading discipline for benchmark code is in `AGENTS.md`.

## Working here

- Conventions live in `AGENTS.md`. Read it before writing code.
