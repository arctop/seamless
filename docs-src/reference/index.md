---
title: Reference
description: The complete Seamless surface - every MCP tool, both CLIs, every configuration key, every hook, the console, and the on-disk file formats.
---

This section is the contract: the complete enumeration of every surface
Seamless exposes, with the parts that are generated straight from the code so
they cannot drift from it. The MCP tool reference is rendered from the same
catalog the server registers, and the configuration reference from the same
defaults the daemon loads - adding a tool or a key makes stale docs a build
error, not a doc bug.

Agents mostly care about the [MCP API](/reference/mcp/): the endpoint and auth
model, the scope rules, and every tool grouped by area - sessions, memory and
recall; notes, projects and capture; tasks; the research lab, the gardener and
usage; and repo locks. Humans get the two
CLIs: [seam](/reference/cli-seam/), the short handle an agent or owner types,
and [seamlessd](/reference/cli-seamlessd/), the daemon and operator commands.

Deployment behavior lives in [Configuration](/reference/configuration/) (every
key, type, and default, plus the four layers that resolve them),
[Hooks](/reference/hooks/) (what `install-hooks` writes per client, the
transports and timeouts, and the fail-open contract), and
[The service & where things live](/reference/service/) - the one page with the
OS-specific service commands, log locations, and every path Seamless touches.
The two compatibility matrices -
[Claude app](/reference/claude-app-compatibility/) and
[Codex](/reference/codex-compatibility/) - hold versioned, platform-specific
evidence rather than assumptions, and record how to re-verify each claim.

The remaining pages describe what you can see and touch directly:
[Console](/reference/console/) documents the observability UI at `/console`
and the complete list of what it can change,
[Storage and file formats](/reference/storage/) specifies the `~/.seamless`
tree and every frontmatter field, and the [Glossary](/reference/glossary/)
pins the vocabulary - including the distinctions that actually matter, like
memory versus note versus finding, and archive versus supersede versus delete.
