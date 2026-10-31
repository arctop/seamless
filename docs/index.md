# Seamless

> A local-first memory and coordination substrate for the fleet of coding agents you run - markdown files you own, indexed by one local daemon.

## Getting started

- [What is Seamless?](https://thereisnospoon.org/docs/): A local-first memory and coordination substrate for the fleet of coding agents you run - markdown files you own, indexed by one local daemon.
- [Quickstart](https://thereisnospoon.org/docs/quickstart/): Install Seamless with one command, start Claude Code or Codex in a repo, and confirm the first briefing lands.
- [First prompts to try](https://thereisnospoon.org/docs/first-prompts/): Requests to paste into Claude Code, Codex, or a Claude app chat right after installing - rules, notes, plans, imported history, recall, and handoffs.
- [Claude Code setup](https://thereisnospoon.org/docs/claude-code/): Register the MCP endpoint, install the hooks that make sessions ambient, map your repos, and verify each step.
- [Claude app chat setup](https://thereisnospoon.org/docs/claude-app/): Register the seam mcp-proxy bridge in claude_desktop_config.json, restart the app, and run the session loop explicitly - a chat has no hooks and no cwd.
- [Codex local setup (app, CLI, and IDE)](https://thereisnospoon.org/docs/codex-cli/): Wire the shared local Codex host into Seamless - one app/CLI/IDE profile for hooks, the mcp-proxy tool bridge, skills, and reaper-driven session lifecycle.
- [Install & deploy](https://thereisnospoon.org/docs/install/): The one-command install and every other route to the same place - Homebrew, a clone, go install - plus the override knobs and the security posture you are accepting.
- [Update & uninstall](https://thereisnospoon.org/docs/updating/): The lifecycle journeys that are one command on every OS - an installer install updates itself, seamlessd update swaps in the latest release now, seamlessd uninstall reverses the install, and adding a client later is one more install-hooks run.
- [Changelog](https://thereisnospoon.org/docs/changelog/): Every Seamless release, newest first - what shipped and when, regenerated from the git tags at each release.

## Concepts

- [Concepts](https://thereisnospoon.org/docs/concepts/): The mental model behind Seamless - memory with a lifecycle, ambient sessions, one search entry point, a dependency-aware task queue, and a gardener that only proposes.
- [How Seamless works](https://thereisnospoon.org/docs/concepts/how-it-works/): One daemon, three surfaces, and a hard line between the files that are truth and the database that is an index.
- [Memory & notes](https://thereisnospoon.org/docs/concepts/memory/): What a memory is, the nine kinds, how supersession keeps the store honest, and when to write a note instead.
- [Sessions & briefings](https://thereisnospoon.org/docs/concepts/sessions/): How an agent gets your knowledge injected before it does anything - ambient sessions, how its tool calls bind to them, the briefing's packing order, and what never gets dropped.
- [Recall](https://thereisnospoon.org/docs/concepts/recall/): One search entry point fusing keyword and vector search - and the three different ways your knowledge actually reaches an agent.
- [Tasks & plans](https://thereisnospoon.org/docs/concepts/tasks-and-plans/): The dependency-aware ready queue, lease-based claiming that lets parallel agents divide work, and plans as compositions rather than primitives.
- [Projects & scope](https://thereisnospoon.org/docs/concepts/projects/): How Seamless decides which project a call belongs to - the precedence chain, the fail-closed rule, and project families.
- [Project isolation](https://thereisnospoon.org/docs/concepts/project-isolation/): Confidential and sealed projects - what each state promises, how tightening works, and the limits of the fence.
- [The gardener](https://thereisnospoon.org/docs/concepts/gardener/): The background pass that finds duplicates, staleness, and drift - and proposes, because nothing rewrites your knowledge behind your back.
- [Memory supersession](https://thereisnospoon.org/docs/concepts/memory-supersession/): How an agent memory store stays true instead of just growing - new knowledge explicitly replaces old, with provenance, unlike decay scores or append-only logs.
- [Lease-based task claiming](https://thereisnospoon.org/docs/concepts/lease-based-task-claiming/): The coordination primitive that lets parallel AI agents share one task queue - an atomic claim with an expiry, so two agents never work the same task and a crashed agent never strands one.
- [Reciprocal rank fusion for agent recall](https://thereisnospoon.org/docs/concepts/reciprocal-rank-fusion/): How RRF merges keyword and vector search into one ranked list by rank, not score - and how Seamless runs both legs in a single SQLite file with no vector database.
- [What is a coordination substrate?](https://thereisnospoon.org/docs/concepts/coordination-substrate/): A definition - the durable, shared layer underneath a fleet of AI agents where memory, tasks, and plans live, so agents in different processes, sessions, and clients can act like one team.

## Guides

- [Guides](https://thereisnospoon.org/docs/guides/): Task-shaped walkthroughs - wiring an agent into the loop, writing memories that get recalled, coordinating a fleet, capturing plans, running trials, and fixing what broke.
- [Integrate your agent](https://thereisnospoon.org/docs/guides/integrate-your-agent/): Wire another agent into the loop - the MCP handshake, stdio bridge, session binding, scope discipline, and seam CLI fallback.
- [Connect Cursor, Cline, Windsurf & Zed](https://thereisnospoon.org/docs/guides/mcp-clients/): Point any MCP client at Seamless's local streamable-HTTP endpoint with bearer auth - a verified config block per client, and an honest account of what a client without hooks does and does not get.
- [Write memories that get recalled](https://thereisnospoon.org/docs/guides/write-good-memories/): The description is the retrieval surface, not a label - how to write one, how to pick a kind, and the four habits that fill a store with noise.
- [Coordinate multiple agents](https://thereisnospoon.org/docs/guides/coordinate-agents/): Fan-out over the ready queue, planner/executor splits via plan composition, shared-lab investigation, and what happens when a claim holder dies.
- [Capture Claude Code plans](https://thereisnospoon.org/docs/guides/plan-mode/): How plan mode is captured automatically - which hook does what, a plan's life from draft to approved, and the escape hatches.
- [Run research trials](https://thereisnospoon.org/docs/guides/research-trials/): The lab loop for systematic debugging - recording what was tried, letting parallel agents share dead ends, and distilling the result into memory.
- [Import, back up & restore](https://thereisnospoon.org/docs/guides/data/): One archive with seamlessd export, restoring or merging it with seamlessd import, putting ~/.seamless in git, what deleting seam.db actually costs, and moving to a new machine.
- [Share one daemon across a LAN](https://thereisnospoon.org/docs/guides/network-install/): Run one seamlessd for several devices - the server config, TLS with mkcert or openssl, the client-config pairing command, and the captures a remote device honestly does not get.
- [Seal projects to OS accounts](https://thereisnospoon.org/docs/guides/peer-auth/): peer_auth - one daemon, a Unix socket that names each caller by its uid, projects only their own OS account can reach, and the deployment that makes the files hold too.
- [Troubleshooting](https://thereisnospoon.org/docs/guides/troubleshooting/): Symptom-first fixes for a system whose hooks fail open - where silence, not an error, is what a broken install looks like.

## Reference

- [Reference](https://thereisnospoon.org/docs/reference/): The complete Seamless surface - every MCP tool, both CLIs, every configuration key, every hook, the console, and the on-disk file formats.
- [MCP API overview](https://thereisnospoon.org/docs/reference/mcp/): The endpoint, the auth model, the scope rules, and an index of every tool Seamless serves.
- [Sessions, memory & recall](https://thereisnospoon.org/docs/reference/mcp/sessions-memory-recall/): The nine tools an agent uses most - open a session, write, edit and read memory, and search the store.
- [Notes, projects & capture](https://thereisnospoon.org/docs/reference/mcp/notes-projects-capture/): Work artifacts, project scope, and SSRF-safe URL capture - the nine tools around the edges of memory.
- [Tasks](https://thereisnospoon.org/docs/reference/mcp/tasks/): The six task tools - the dependency-aware ready queue and lease-based claiming that lets parallel agents divide work safely.
- [Lab, gardener & usage](https://thereisnospoon.org/docs/reference/mcp/lab-gardener-usage/): Research trials, the propose-only gardener, the usage summary, and favorites - the nine tools for keeping the store honest.
- [seam CLI](https://thereisnospoon.org/docs/reference/cli-seam/): Every seam subcommand - agent loop, tasks, plans, observability, hooks - plus the flag-order rules and what each one rejects.
- [seamlessd CLI](https://thereisnospoon.org/docs/reference/cli-seamlessd/): The daemon and operator CLI - serve, doctor, export, import, install-hooks, client-config, uninstall, update, map-repo, unmap-repo, retire-project, family, console-open, start/stop/restart/status, and version.
- [Configuration](https://thereisnospoon.org/docs/reference/configuration/): Every configuration key, its type and default, plus the annotated example file and the four layers that resolve them.
- [Hooks](https://thereisnospoon.org/docs/reference/hooks/): The hooks Seamless installs per client - seven for Claude Code, five for Codex - their transports and timeouts, the fail-open contract, and what install-hooks writes.
- [The service & where things live](https://thereisnospoon.org/docs/reference/service/): Everything OS-specific in one place - the per-user service (launchd, systemd, or a Scheduled Task), its native controls and logs, the detached updater behind automatic updates, and every path Seamless touches.
- [Claude app compatibility matrix](https://thereisnospoon.org/docs/reference/claude-app-compatibility/): Versioned evidence for the Claude app's two surfaces - embedded code sessions (hooks, MCP, runtime skew) and the chat surface's stdio MCP bridge.
- [Codex compatibility matrix](https://thereisnospoon.org/docs/reference/codex-compatibility/): Versioned, platform-specific evidence for the Codex hooks, MCP transports, trust gate, output limit, and the contract-recapture procedure.
- [Console](https://thereisnospoon.org/docs/reference/console/): The read-mostly observability UI at /console - the complete list of what it can change, how sign-in works, the three experience levels, and what each page shows.
- [Storage and file formats](https://thereisnospoon.org/docs/reference/storage/): The ~/.seamless tree, memory and note frontmatter field by field, what lives only in SQLite, and the rules for hand-editing.
- [Glossary](https://thereisnospoon.org/docs/reference/glossary/): The vocabulary, with the distinctions that actually matter - memory vs note vs finding, briefing vs recall, archive vs supersede vs delete.

## Internals

- [Internals](https://thereisnospoon.org/docs/internals/): For contributors - how the Go packages layer, what the check gate enforces, and the domain invariants that plausible-looking code breaks.
- [Architecture](https://thereisnospoon.org/docs/internals/architecture/): The package layering, what each package owns, three data-flow traces through the real code, and the things Seamless deliberately does not have.
- [Contributing](https://thereisnospoon.org/docs/internals/contributing/): The make targets, the check gate and what CI runs, the conventions that matter, the forbidden APIs, and the three places a new MCP tool must be wired.
- [Domain invariants](https://thereisnospoon.org/docs/internals/invariants/): The rules plausible-looking code breaks - supersession, scope resolution, FTS and LIKE escaping, LLM degradation, automatic updates and release discipline - and why each exists.

## Scenarios

- [Your agent starts every session from zero. Here's what it costs](https://thereisnospoon.org/scenarios/cold-start/): Two real Claude Code sessions, same repo, same prompt - one continues yesterday's plan from a briefing, one re-derives the work from a TODO and re-ships a known bug.
- [“Tighten the auth cookies”: when your agent breaks a rule you already wrote down](https://thereisnospoon.org/scenarios/constraint-violation/): A security scanner demands SameSite=Strict. The team already learned Strict breaks external-link logins. Two real sessions - one ships the regression anyway, one refuses and cites the constraint.
- [“Persist the refresh tokens”: why one agent stored them raw and one hashed them](https://thereisnospoon.org/scenarios/token-safety/): Two real sessions given the same persistence task. One mirrors an in-memory map into a raw-token SQL column; one reads a recorded rule first and stores only SHA-256 hashes.
- [Two agents, one plan step, no collision: what a lease actually buys you](https://thereisnospoon.org/scenarios/task-collision/): Two live Claude Code agents race for the same plan step. One claim wins, one bounces with the holder's name, and the second agent pivots to the next ready step - no duplicate work, no referee.
