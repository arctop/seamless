# Reference

> The complete Seamless surface - every MCP tool, both CLIs, every configuration key, every hook, the console, and the on-disk file formats.

This section is the contract: the complete enumeration of every surface
Seamless exposes, with the parts that are generated straight from the code so
they cannot drift from it. The MCP tool reference is rendered from the same
catalog the server registers, and the configuration reference from the same
defaults the daemon loads - adding a tool or a key makes stale docs a build
error, not a doc bug.

Agents mostly care about the [MCP API](https://thereisnospoon.org/docs/reference/mcp/): the endpoint and auth
model, the scope rules, and every tool grouped by area - sessions, memory and
recall, notes and projects, tasks, and the research lab. Humans get the two
CLIs: [seam](https://thereisnospoon.org/docs/reference/cli-seam/), the short handle an agent or owner types,
and [seamlessd](https://thereisnospoon.org/docs/reference/cli-seamlessd/), the daemon and operator commands.

Deployment behavior lives in [Configuration](https://thereisnospoon.org/docs/reference/configuration/) (every
key, type, and default, plus the four layers that resolve them),
[Hooks](https://thereisnospoon.org/docs/reference/hooks/) (what `install-hooks` writes per client, the
transports and timeouts, and the fail-open contract), and
[The service & where things live](https://thereisnospoon.org/docs/reference/service/) - the one page with the
OS-specific service commands, log locations, and every path Seamless touches.
The two compatibility matrices -
[Claude app](https://thereisnospoon.org/docs/reference/claude-app-compatibility/) and
[Codex](https://thereisnospoon.org/docs/reference/codex-compatibility/) - hold versioned, platform-specific
evidence rather than assumptions, and record how to re-verify each claim.

The remaining pages describe what you can see and touch directly:
[Console](https://thereisnospoon.org/docs/reference/console/) documents the observability UI at `/console`
and the complete list of what it can change,
[Storage and file formats](https://thereisnospoon.org/docs/reference/storage/) specifies the `~/.seamless`
tree and every frontmatter field, and the [Glossary](https://thereisnospoon.org/docs/reference/glossary/)
pins the vocabulary - including the distinctions that actually matter, like
memory versus note versus finding, and archive versus supersede versus delete.

- [MCP API overview](https://thereisnospoon.org/docs/reference/mcp/): The endpoint, the auth model, the scope rules, and an index of every tool Seamless serves.
- [Sessions, memory & recall](https://thereisnospoon.org/docs/reference/mcp/sessions-memory-recall/): The nine tools an agent uses most - open a session, write, edit and read memory, and search the store.
- [Notes, projects & capture](https://thereisnospoon.org/docs/reference/mcp/notes-projects-capture/): Work artifacts, project scope, SSRF-safe URL capture, and asking for a locked project - the tools around the edges of memory.
- [Tasks](https://thereisnospoon.org/docs/reference/mcp/tasks/): The six task tools - the dependency-aware ready queue and lease-based claiming that lets parallel agents divide work safely.
- [Lab, gardener & usage](https://thereisnospoon.org/docs/reference/mcp/lab-gardener-usage/): Research trials, the propose-only gardener, the usage summary, and favorites - the nine tools for keeping the store honest.
- [seam CLI](https://thereisnospoon.org/docs/reference/cli-seam/): Every seam subcommand - agent loop, memories and notes, tasks, plans, locked-project access requests, observability, seam call, hooks - plus --json output, the flag-order rules and what each one rejects.
- [seamlessd CLI](https://thereisnospoon.org/docs/reference/cli-seamlessd/): The daemon and operator CLI - serve, doctor, export, import, backups, restore, setup, install-hooks, client-config, uninstall, update, map-repo, unmap-repo, retire-project, family, devices, console-open, start/stop/restart/status, and version.
- [Configuration](https://thereisnospoon.org/docs/reference/configuration/): Every configuration key, its type and default, plus the annotated example file and the four layers that resolve them.
- [Hooks](https://thereisnospoon.org/docs/reference/hooks/): The hooks Seamless installs per client - seven for Claude Code, five for Codex - their transports and timeouts, the fail-open contract, and what install-hooks writes.
- [The service & where things live](https://thereisnospoon.org/docs/reference/service/): Everything OS-specific in one place - the per-user service (launchd, systemd, or a Scheduled Task), its native controls and logs, the detached updater behind automatic updates, and every path Seamless touches.
- [Claude app compatibility matrix](https://thereisnospoon.org/docs/reference/claude-app-compatibility/): Versioned evidence for the Claude app's two surfaces - embedded code sessions (hooks, MCP, runtime skew) and the chat surface's stdio MCP bridge.
- [Codex compatibility matrix](https://thereisnospoon.org/docs/reference/codex-compatibility/): Versioned, platform-specific evidence for the Codex hooks, MCP transports, trust gate, output limit, and the contract-recapture procedure.
- [Console](https://thereisnospoon.org/docs/reference/console/): The read-mostly observability UI at /console - the complete list of what it can change, how sign-in works, the three experience levels, and what each page shows.
- [Storage and file formats](https://thereisnospoon.org/docs/reference/storage/): The ~/.seamless tree, memory and note frontmatter field by field, what lives only in SQLite, and the rules for hand-editing.
- [Glossary](https://thereisnospoon.org/docs/reference/glossary/): The vocabulary, with the distinctions that actually matter - memory vs note vs finding, briefing vs recall, archive vs supersede vs delete.
