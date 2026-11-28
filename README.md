# Seamless

[![Go Reference](https://pkg.go.dev/badge/github.com/arctop/seamless.svg)](https://pkg.go.dev/github.com/arctop/seamless)
[![Latest release](https://img.shields.io/github/v/release/arctop/seamless)](https://github.com/arctop/seamless/releases/latest)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![MCP](https://img.shields.io/badge/MCP-compatible-6f42c1)](https://thereisnospoon.org/docs/reference/mcp/)

**Every agent you run should know what the others learned.**

Seamless is shared, persistent memory and a task queue for the AI coding agents
you run: Claude Code, Codex CLI, and any MCP client. Agents save what they
learn as markdown files on your own computer, every new session starts briefed
from them, and agents working side by side claim tasks from one queue.

```bash
curl -fsSL https://thereisnospoon.org/install | sh    # macOS, Linux
```

```powershell
irm https://thereisnospoon.org/install.ps1 | iex      # Windows
```

## Why

You are the only thing your agents have in common. What one of them works out
reaches the next only if you carry it across, and there are three seams where
that happens:

- **Between tools.** Claude Code and Codex each keep their own notes, so what
  one learned the other never sees.
- **Between agents.** Agents working side by side cannot see what the others
  found or which step is taken, so you relay between them, or two of them
  build the same thing.
- **Between sessions.** A new session knows what the last one worked out only
  if someone wrote it down, and that someone is usually you.

Seamless is named for closing them: one store that every client reads and
writes, one queue with atomic, leased claims, and a briefing injected before a
session's first prompt. All of it
stays on your computer, in markdown files you can open, edit and keep in git.

Claude Code's built-in memory is a good start, and it keeps working beside
Seamless. It is one client's notes about one repository on one computer;
[the comparison](https://thereisnospoon.org/compare/#built-in) sets the two
side by side.

It also works from chats in the Claude desktop app, for long-running work that
isn't code: see [Seamless in the Claude app](https://thereisnospoon.org/claude-app/).

## What is in it

A fleet of agents gets a shared, durable memory and a way to divide work
without colliding: memories with a supersession lifecycle, hybrid recall, a
dependency-aware task queue with lease-based claiming, and captured plans,
behind walls you choose. Durable knowledge is stored as markdown files on disk;
the `seamlessd` daemon indexes it, serves it over MCP, and renders a web
console, while the companion `seam` CLI gives headless agents a direct
interface.

**Full documentation: [thereisnospoon.org/docs/](https://thereisnospoon.org/docs/)**
&nbsp;·&nbsp; Website: [thereisnospoon.org](https://thereisnospoon.org) (source in
[`docs/`](docs/))

Developed and published by [Arctop](https://arctop.com). MIT licensed.

## Design principles

- **Built for a fleet, not a lone agent.** Real coordination primitives: a
  dependency-aware ready-queue, atomic lease-based task claiming, and plans
  composed of notes and steps, so agents divide labor instead of colliding.
- **Files are the source of truth.** Every memory and note is a markdown file
  with YAML frontmatter under `~/.seamless` -- git-diffable, greppable,
  hand-editable. SQLite indexes those files and also stores operational state
  such as sessions, tasks, trials, and events; a release install backs the
  whole data directory up every day and keeps the last seven.
- **Built for the day an agent gets tricked.** No account, no telemetry, and
  nothing off your computer can reach it by default. Fence a client's project
  so its knowledge stays put, hold suspicious agent writes for your review, and
  run Seamless in its own OS account so a locked project opens only with your
  Touch ID, a passkey, or your password. The limits are written down too:
  [the security model](https://thereisnospoon.org/security/).
- **Curation proposes, humans dispose.** Every gardener pass -- from
  deduplicating and archiving to flagging dead weight and knowledge gaps --
  only *proposes*; applying is an explicit action. Supersession preserves
  provenance, so nothing is silently rewritten.
- **Small, self-contained runtime.** A static Go daemon and CLI, no CGO,
  pure-Go SQLite, no Node, no separate vector engine, no cloud account.

## How it compares

The agent-memory space splits into a few recognizable categories. By category,
because categories do not go stale:

| | Seamless | Cloud memory APIs | Built-in agent memory | Knowledge-graph servers |
|---|---|---|---|---|
| Storage format | Markdown files on your disk; SQLite indexes them and stores operational state | Their database, reached by API key | Notes one client keeps, per repository or per product | A graph database, often a separate server |
| Runs where | Your computer, or a Server of yours your other computers pair with | Their cloud | Inside one client | Your machine or theirs |
| Account required | No | Yes | The client's | Usually no |
| Multi-agent coordination | Task queue, lease-based claiming, shared plans | None | None -- one client, one store | Shared reads at best |
| Walls between projects | Confidential and sealed projects; locked projects open only with your approval | Workspace permissions | One store per repository or product | Usually none |
| Forgetting policy | Supersession with provenance; a gardener proposes, a human disposes | Automatic summarization you do not control | The model prunes | Manual |
| Runtime | Static Go daemon and CLI | HTTP SDK against their service | None (built in) | Node or Python, plus the database |

For the version with product names and receipts, see
[the full comparison](https://thereisnospoon.org/compare/).

## Security and privacy

Seamless assumes an agent can be tricked: a web page, an issue, or an email can
carry instructions it will follow. Out of the box it runs on your computer,
sends no telemetry, and makes one request of its own, an anonymous check for
new releases. On top of that come five layers, in the order the console's
Privacy screen draws them: project fences, write screening, Seamless in its own
account, locked projects, and restricted agent accounts. Every release is
signed by the release workflow (Sigstore) and by the maintainer's hardware key,
and checked before anything installs.

[The security page](https://thereisnospoon.org/security/) has the whole model,
limits included, and the [docs](https://thereisnospoon.org/docs/concepts/security/)
have every mechanism. To report a vulnerability, use
[private vulnerability reporting](https://github.com/arctop/seamless/security/advisories/new);
see [SECURITY.md](SECURITY.md).

## Real transcripts

Four pairs of real, unedited Claude Code sessions -- identical prompt,
identical repo, with and without Seamless:

- **[Cold start](https://thereisnospoon.org/scenarios/cold-start/)** -- one
  session continues yesterday's plan from an injected briefing; the other
  re-derives the work from a `TODO` and re-ships a bug the project had already
  fixed once.
- **[Constraint violation](https://thereisnospoon.org/scenarios/constraint-violation/)** --
  a security scanner demands `SameSite=Strict`, which the team already learned
  breaks external-link logins. One session ships the regression anyway; one
  refuses and cites the recorded constraint.
- **[Token safety](https://thereisnospoon.org/scenarios/token-safety/)** --
  told to persist refresh tokens, one agent mirrors the in-memory map into a
  raw-token SQL column; the other reads a recorded rule first and stores only
  SHA-256 hashes.
- **[Task collision](https://thereisnospoon.org/scenarios/task-collision/)** --
  two live agents race for the same plan step. One claim wins, the other
  bounces with the holder's name and pivots to the next ready step.

## What Seamless is not

Not a hosted team knowledge base, not a RAG framework, not a benchmark winner:
it is memory and coordination for one owner's fleet of agents, on that owner's
computers.

## Quick start

```bash
curl -fsSL https://thereisnospoon.org/install | sh
```

On Windows, the same install in PowerShell:

```powershell
irm https://thereisnospoon.org/install.ps1 | iex
```

That is the whole install. It needs `curl` and `tar` and nothing else -- no Go,
no CGO toolchain, no database, no Node. It fetches the release archive for your
platform (macOS, Linux, and Windows; amd64 and arm64), checks it against
checksums the maintainer signed with a hardware key, installs
`seamlessd` and `seam` into `~/.local/bin`, generates the bearer key, installs
hooks, MCP, and skills for the detected Claude Code/Codex local hosts, and runs
the daemon as a per-user service -- launchd on macOS, systemd `--user` on Linux, an
at-logon Scheduled Task on Windows. At a terminal it first asks how you will use
Seamless: Standalone, everything on this computer (the default), or Networked,
one computer keeping the data for the others
([Share one daemon across your computers](https://thereisnospoon.org/docs/guides/network-install/)).
It keeps itself up to date: from v0.7.0 it
installs each new release by itself, a day after it is published and at a moment
no agent is using it, or within a few days at the latest -- backed up, and
rolled back if the new release does not come up. It also backs your data up
every day and keeps the last seven. To update right now, run `seamlessd update` (`--check` reports
installed vs latest): your config and `~/.seamless` are preserved.

> **Early days, frequent releases.** Seamless is early in its development
> cycle, and releases with improvements and bug fixes land often. An installer
> install updates itself from v0.7.0 on; on v0.6.0 or older, run
> `seamlessd update` once. See
> [Update & uninstall](https://thereisnospoon.org/docs/updating/).

(Why `seam`? The CLI keeps the short name of Seam v1, the decommissioned
private predecessor Seamless was rebuilt from the ground up to replace.)

Then just start the selected client in a git repo. There is no project to create
and no repo to register: the session-start hook resolves your cwd to its git
root, derives a project from the repo's directory name, and records the mapping
on the spot, so agents inherit project scope without passing it on every call.
Reach for `seamlessd map-repo --path ~/code/myrepo --project myrepo` only to
override the derived slug.

It is [one shell script](docs/install) and piping a stranger's script into a
shell deserves a read first. Prefer the pieces one at a time - Homebrew,
`go install`, prebuilt archives - or want the override knobs? Every route is
on [Install & deploy](https://thereisnospoon.org/docs/install/), and the
[Quickstart](https://thereisnospoon.org/docs/quickstart/) tailors each step to
your OS and client.

Then: [Quickstart](https://thereisnospoon.org/docs/quickstart/) ·
[Claude Code setup](https://thereisnospoon.org/docs/claude-code/) ·
[Claude app chat setup](https://thereisnospoon.org/docs/claude-app/) ·
[Codex local setup](https://thereisnospoon.org/docs/codex-cli/) ·
[Install & deploy](https://thereisnospoon.org/docs/install/)

## Documentation

The full docs are at
**[thereisnospoon.org/docs/](https://thereisnospoon.org/docs/)** (sources in
[`docs-src/`](docs-src/), generated by [`cmd/docsgen`](cmd/docsgen/)).

| | |
|---|---|
| [Concepts](https://thereisnospoon.org/docs/concepts/) | How it works, security and privacy, memory & notes, sessions & briefings, recall, tasks & plans, projects & isolation, networked Seamless, the gardener, repo locks |
| [Guides](https://thereisnospoon.org/docs/guides/) | Integrating an agent, writing memories that get recalled, coordinating a fleet, backups, a networked install, locked projects, write screening, a security monitor, troubleshooting |
| [Reference](https://thereisnospoon.org/docs/reference/) | Every MCP tool, both CLIs, every config key, the hooks, and the file formats |
| [Internals](https://thereisnospoon.org/docs/internals/) | Architecture, contributing, domain invariants |

This README is deliberately short. Anything that can drift from the code -- tool
counts, config keys, CLI flags -- lives in the docs site, where the reference
pages are generated from the code itself and `make check` fails if they go stale.

## Development

```
make build      # ./bin/seamlessd + ./bin/seam
make test       # unit tests
make test-race  # unit tests under the race detector
make bench      # hot-path benchmarks (recall, briefing, matcher, event fan-out)
make lint       # golangci-lint
make check      # the full gate: build + vet + cross-check + fmt-check +
                # docs-check + installer-check + site-check + lint +
                # vulncheck + test-race
make doctor     # config + database self-checks
make run        # serve on 127.0.0.1:8081

make docs       # regenerate the docs site (docs-src/ -> docs/docs/, committed)
make docs-serve # regenerate + serve the site at 127.0.0.1:8899/docs/
```

Tests are table-driven with `testify/require` against fresh or in-memory SQLite.
Use `make fmt` rather than `gofmt -w .`: the Make target scopes formatting to
git-tracked files, while a bare `gofmt` walk also rewrites dot-directories that
Go's `./...` pattern excludes.

The docs site's output under `docs/docs/` is committed, and `make check` runs
`docs-check`, so a change to `docs-src/` -- or to the tool surface or config keys
the reference generates from -- must be followed by `make docs` in the same
change. See [`SITE.md`](SITE.md).

Conventions live in [`AGENTS.md`](AGENTS.md); read it before writing code.
