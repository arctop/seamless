---
id: 01KXPP6V4MSV7G05RQ4DDZGTTQ
kind: convention
name: seam-vs-seamless-naming-convention
description: "Two naming families: `seam` = the short handle a human types (CLI + skills); `seamless` = the system/namespace (product, daemon, MCP). Keep them…"
project: seamless
created: 2026-07-17T00:04:20Z
invalid_at: null
superseded_by: null
source_session: 01KXPNNW2JBGYW83AR0MNRW7XC
---
The project has two deliberate, load-bearing naming families. They are not
interchangeable, and the split is intentional -- do not "unify" them.

## The rule

- **`seam`** -- the short handle a human *types* at a prompt:
  - the CLI command and its verbs: `seam task`, `seam recall`, `seam doctor`,
    `seam plan`, `seam hook`, `seam capture`, `seam status`
  - the Claude Code skills: `/seam-onboard`, `/seam-research`
- **`seamless`** -- the system and its namespaces (machinery, not typed by hand):
  - product name ("Seamless"), the daemon binary `seamlessd`
  - the MCP server name (`serverName = "Seamless"`, `internal/mcp/server.go`) and
    the tool prefix `mcp__seamless__*`
  - the data dir `~/.seamless`, env prefix `SEAMLESS_*`, config `seamless.yaml`

Mnemonic: **`seam` is what you type; `seamless` is the system underneath.** An
agent legitimately calls `mcp__seamless__recall`, `seam recall`, and
`/seam-research` in the same session -- that is the convention working, not drift.

## Applying it to new names

- A new user-typed CLI verb or Claude Code skill -> `seam` / `seam-<x>`.
- A new daemon-side surface, MCP tool namespace, env var, or on-disk path ->
  `seamless` / `SEAMLESS_*`.

## Why we keep it (decided 2026-07-16)

Considered renaming the skills to `/seamless-*` for symmetry with the
`mcp__seamless__` namespace. Rejected: it would break the parallel with the
`seam` CLI verb (you'd have `seam plan` but `/seamless-onboard`), orphan the
`seam` handle to just the CLI, and force a real migration -- the
`.seam-onboard-delivered` marker + self-delete logic (`docs/install:204-217`),
`scripts/install-skill.sh` / `uninstall-skill.sh`, `.goreleaser.yaml` bundling,
and all docs. The only friction the split causes (a first-timer seeing
`mcp__seamless__*` and guessing `/seamless-onboard`) resolves the moment they
meet the `seam` CLI. See [[installer-bundles-claude-skills]].
