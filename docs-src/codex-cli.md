---
title: Codex local setup (app, CLI, and IDE)
description: Wire the shared local Codex host into Seamless - one app/CLI/IDE profile for hooks, the mcp-proxy tool bridge, skills, and reaper-driven session lifecycle.
---

Seamless supports Codex - the CLI, desktop app, and IDE extension - as one
client profile named `codex`, because the three share the same local
configuration layers and MCP setup on a given host. Wiring that host is the
same two independent halves as [Claude Code](/claude-code/) - **the MCP
endpoint** (what an agent can call, installed as the `seam mcp-proxy` stdio
bridge) and **the hooks** (five of them, which brief and harvest an agent
without it calling anything) - and one command installs both when the Codex
management CLI is available.

CLI behavior is supported by the existing compatibility suite. The IDE extension
shares the profile by upstream contract, while desktop app support is currently
a local beta. Each non-CLI mode still needs the live hook-trust and chat evidence
listed in [Compatibility evidence](#compatibility-evidence) before Seamless
claims it as fully verified.

## Install the hooks and register MCP

```bash
seamlessd install-hooks --client codex
```

`--client codex` is the switch, but you rarely need it: without the flag, an
interactive `install-hooks` run prompts for the client(s) to wire (defaulting to
what it found on the machine), and a non-interactive run resolves `--client
detect` - the clients actually present, Codex included. The Codex profile:

1. **Merges five hooks** - SessionStart, UserPromptSubmit, Stop, SubagentStart,
   and SubagentStop - into `~/.codex/hooks.json` (or `$CODEX_HOME/hooks.json`
   when that is set). It replaces exact or stale Seamless definitions, adopts
   only recognizable legacy Seamless commands, preserves foreign hooks, and
   backs the file up once before the first change.
2. **Registers the MCP server** by shelling out to Codex itself:
   `codex mcp add seamless -- <abs seam> mcp-proxy --config <abs seamless.yaml>`.
   Current Codex supports both stdio and Streamable HTTP. Seamless deliberately
   installs the [mcp-proxy bridge](#why-mcp-goes-through-a-bridge) as its default
   secret-handling policy, not because Codex requires stdio. The installer reads
   `codex mcp get seamless --json`, leaves an exact enabled bridge alone, repairs
   a disabled or stale Seamless bridge, and verifies the written state before it
   reports success. A direct-HTTP or otherwise foreign entry under the reserved
   `seamless` name is never overwritten implicitly.
3. **Installs the Codex skills** under
   `${CODEX_HOME:-$HOME/.codex}/skills/`: the one-shot `$seam-onboard` workflow,
   plus the recurring `$seam-research` lab workflow while its
   [optional feature](/reference/console/#optional-features) is on (it ships
   off). Claude's copies remain in
   `~/.claude/skills/`; their one-shot delivery markers are independent.

The `codex` executable is the supported management surface for automated MCP
setup; it is not a different Seamless profile. On an app-only machine where
`codex` is absent from `PATH`, hooks and skills still install into the shared
Codex home, but the MCP row is explicitly marked **incomplete** and prints the
desktop fallback:

1. Open **Settings > MCP servers > Add server** in the Codex desktop app.
2. Choose **STDIO** and name the server `seamless`.
3. Use the printed absolute `seam` path as the command and the printed
   `mcp-proxy --config <absolute seamless.yaml>` values as its arguments.
4. Save, then restart the app.

That path keeps the bearer key out of Codex configuration just like the
automated registration. `doctor` still calls the MCP state unverified without a
management CLI; reading and safely classifying shared TOML is a separate
hardening step.

Passing `--client claude` explicitly is byte-for-byte what it always was, so
nothing about your Claude Code setup changes when you add Codex. Use `--client
all` to install both in one pass, or `--client detect` (the default) to let the
machine decide. `make install` uses the same default, so a Codex-only machine
gets the Codex profile without naming it.

## Trust the hooks once

Hooks installed, one gate remains before anything fires - and it is the
Codex-specific version of "silence is the failure mode". Codex will not run a
non-managed command hook until its **current definition** is trusted. New or
changed definitions are skipped until reviewed, so a fresh install - or a
reinstall that repairs a path or command - can show no briefing until you
approve. Two supported paths:

- **CLI** (the currently verified path): start `codex`, open `/hooks`, inspect the
  current Seamless commands, and approve them. Codex also warns at startup when
  configured hooks need review.
- **Headless automation**: pass `--dangerously-bypass-hook-trust`. As the flag
  name says, it is for automation that already vets its hook sources.

The public hook documentation names `/hooks` in the CLI, and Codex.app
26.715.52143 confirmed the boundary: `/hooks` is not intercepted in a desktop
chat and directs the user back to the CLI. A real repo-local app chat did receive
`<seam-briefing>`, prompt recall, and Stop harvest, so Local app hook execution is
live-verified for that build. Trust state is still not inspectable in the app,
and the presence of `hooks.json` alone remains insufficient evidence.

If a Codex session opens with no briefing, an untrusted hook is the first thing
to check. Seamless does not read or write Codex's private trust hashes, and
`doctor` never claims trust is healthy from a recent observation alone.

## Teach Codex when to use Seamless

Every MCP initialize response carries concise server instructions. The first
512 characters cover the baseline even before onboarding: recall before
guessing, memory versus notes, explicit ambiguous scope, session handoff,
`plan:<slug>` composition, and trusting each tool's `inputSchema` required
fields and enums.

For durable personal or project guidance, invoke `$seam-onboard`. It inspects
the existing instructions, explains the marker-wrapped block it proposes, and
asks whether to add it globally (`${CODEX_HOME:-$HOME/.codex}/AGENTS.md`) or to this
project (`./AGENTS.md`). It never silently edits an instruction file and removes
only its own skill directory after success. Reinstall it from a clone with:

```bash
make install-onboard-skill CLIENT=codex
```

`$seam-research <lab-name> <problem>` opens or resumes the same structured,
multi-agent research-lab workflow as Claude Code. While the research feature is
on it remains installed and is refreshed on upgrades; switch the feature off and
the next install run takes it back out.

## The five hooks, and what they inject

Codex installs five hooks against seven for Claude Code: `SessionStart`
injects the `<seam-briefing>` and creates or resumes the opaque `cx/...`
ambient session, `UserPromptSubmit` injects `<seam-recall>` on a match,
`Stop` heartbeats and harvests findings at every turn end, and
`SubagentStart`/`SubagentStop` brief Task children with constraints and
spawn-prompt-matched memories while only ever heartbeating the parent session.
Seamless has no verified Claude-style plan-file/`ExitPlanMode` surface to
capture from Codex, and Codex 0.144.6 emits **no SessionEnd**, so the parent
lifecycle closes differently (see
[below](#no-sessionend-the-reaper-closes-sessions)). The exact per-event
table - transports, endpoints, effects, and the 2,400-token output cap - is in
[the hooks reference](/reference/hooks/#codex-local-host-five-hooks).

Codex delivers a hook's `additionalContext` to the model on both SessionStart and
UserPromptSubmit, and on SubagentStart, in **both** the interactive TUI and
headless `codex exec` - there is no headless recall-injection gap of the kind
`claude -p` has. The briefing and recall blocks reach the model either way.

All five are **command** hooks - `seam hook <event> --config <yaml> --client
codex` - and every one passes through [the trust gate above](#trust-the-hooks-once).
Codex's hook schema currently executes command handlers; this is independent of
MCP transport, where both stdio and Streamable HTTP are supported.

### The model-visible output ceiling

Codex spills a model-visible hook-output entry above roughly 2,500 estimated
tokens to a temporary file, so Seamless caps every Codex `additionalContext`
response at **2,400 estimated tokens** - briefings, recall, and child
briefings alike - keeping the injected bytes equal to what the model actually
saw. The mechanics live in
[the hooks reference](/reference/hooks/#codex-local-host-five-hooks).

## Why MCP goes through a bridge

Seamless serves MCP over Streamable HTTP, and current Codex can connect to that
URL directly **or** launch a local MCP server over stdio. Seamless chooses the
second form by default: Codex launches `seam mcp-proxy`, which forwards each
JSON-RPC frame to the daemon's `/api/mcp` endpoint with the bearer key from your
config and preserves `Mcp-Session-Id` across calls so session binding keeps
working. The bridge also names the Codex process that launched it on every
request, which binds the agent's tool calls to the `cx/...` session its
SessionStart hook opened - no `session_start` needed
([how](/concepts/sessions/#process-binding)).

The bridge is transport-thin - no tool knowledge, no caching - and it is what the
installer registers. It exists so the key stays in `~/.config/seamless/seamless.yaml`
and never has to be copied into `~/.codex/config.toml` or exported into Codex's
environment. In other words, the proxy is Seamless's maintained default and
secret-handling boundary, not a Codex capability workaround.

One Codex-specific wrinkle for headless use: `codex exec` **cancels every MCP
tool call client-side** unless you also pass `--dangerously-bypass-approvals-and-sandbox`.
This is a separate gate from hook trust - `approval_policy=never` does not lift
it. Interactive `codex` lets you approve tool calls as they happen, which is the
safe path. Fully headless automation of the Seamless loop needs **both**
`--dangerously-bypass-hook-trust` (for the hooks) and
`--dangerously-bypass-approvals-and-sandbox` (for the tool calls).

### Registering MCP by hand

<details>
<summary>Direct Streamable HTTP instead of the bridge</summary>

If you prefer direct Streamable HTTP over the stdio bridge, add it to
`~/.codex/config.toml` yourself:

```toml
[mcp_servers.seamless]
url = "http://127.0.0.1:8081/api/mcp"
http_headers = { Authorization = "Bearer <mcp.api_key>" }
```

The key is `mcp.api_key` from your config (`grep api_key
~/.config/seamless/seamless.yaml`). This is the plain alternative to the proxy,
at the cost of duplicating the key into Codex's TOML - which is exactly what the
bridge exists to avoid. `codex mcp add seamless --url http://127.0.0.1:8081/api/mcp
--bearer-token-env-var SEAMLESS_MCP_API_KEY` is a third route, but it reads the
key from an environment variable that Codex's own process must have set when it
launches - fragile to arrange reliably, which is the other reason the bridge is
the default. A direct entry also sends no agent-process header, so the agent's
tool calls are not bound to its session automatically: with agents active in
several repos, it binds with `session_start name=cx/...` - the handle on its
briefing's `Seam session` line - when a call reports an ambiguous scope,
session, or agent.

</details>

The name `seamless` is the installer's desired-state boundary. If you keep a
manual direct-HTTP entry under that name, run `seamlessd install-hooks --client
codex --mcp=false` for hook and skill updates. With MCP reconciliation enabled,
the installer reports the foreign transport and asks you to remove it explicitly
rather than silently replacing a configuration that may contain credentials.

## Session identity and provenance

A current Codex ambient session has a display handle shaped like
`cx/<first-8>-<16-hex-digest>`. The suffix is 64 stable SHA-256 bits derived from
the **full** external session ID; the readable prefix alone is not unique for
time-ordered UUIDv7 values. Seamless keys lifecycle updates by the full external
ID plus client, not by this label. Treat the handle as opaque: do not parse it or
construct one yourself. Historical `cx/<first-8>` rows keep their names when
they resume. See [Sessions & briefings](/concepts/sessions/) and the
[UUIDv7 layout](https://www.rfc-editor.org/rfc/rfc9562.html#section-5.7).

## No SessionEnd: the reaper closes sessions

Claude Code harvests findings and completes its session on SessionEnd. Codex
0.144.6 emits no such event, so the lifecycle is built around **Stop** and the
**idle reaper** instead:

- **Every turn**, the Stop hook heartbeats the session and re-harvests the
  findings from that turn's final assistant message. The harvest is idempotent -
  an empty turn leaves the prior findings intact - so the findings converge on
  the last substantive turn's summary.
- **The session is never explicitly closed.** It sits active until
  `gardener.session_idle_minutes` of silence, at which point the idle reaper marks
  it `expired`. A Codex session therefore only ever reaches `expired`, never
  `completed`.

Those expired-but-harvested findings still surface in the next agent's briefing:
the briefing assembler includes expired ambient sessions, not just completed
ones, precisely so Codex's harvest is not invisible. The practical consequence is
that a Codex session's findings appear a few minutes after the last turn (once the
reaper runs), rather than the instant the window closes.

With [access requests](/concepts/project-isolation/#asking-for-access) on, each
`Stop`'s findings go to the conversation's home. After an approved move, they are
the child session's, in the project it moved into
([details](/concepts/sessions/#home-record)).

## Verify

```bash
seamlessd doctor    # config, database, tool count, hooks - now Codex-aware
```

`doctor` reports distinct Codex facts instead of rolling them into one
plausible-looking success:

- **runtime versions** - the PATH CLI and, on macOS when it belongs to the same
  default Codex home, the desktop app's retained compatibility runtime are
  reported separately because they can differ;
- **hook definitions** - exact current, stale, and missing events, compared with
  the definitions `install-hooks` would write today, including binary/config
  targets;
- **hook trust** - always `trust unverified` because Codex exposes no supported
  query for the current trust decision; inspect `/hooks`;
- **hook activity** - the last observed SessionStart/UserPromptSubmit event, if
  any, as supporting evidence only, never proof that the current definitions are
  trusted; and
- **MCP** - exact enabled stdio transport, ordered bridge arguments, and existing
  executable/config paths, based on `codex mcp get seamless --json`; without the
  management CLI it reports incomplete/unverified setup and the exact app path.

Run `seamlessd install-hooks --client codex` to repair stale owned hooks and a
disabled or drifted owned bridge. A foreign hook survives, and a foreign MCP
entry requires explicit removal. `doctor` never fails a machine that has no
Codex install: no CLI, home, or Seamless Codex configuration is one quiet
`not detected` line; a detected but incomplete setup is a warning.

Then start a Codex session in a mapped repo and look for the injected
`<seam-briefing>` block, exactly as you would for Claude Code. If it is there, the
loop is closed: the hook resolved your cwd to a project, the daemon assembled a
briefing, and Codex started already knowing what you know. If it is **not** there,
the hooks fail open by design - start with `seamlessd doctor` and the trust gate,
then the [Troubleshooting](/guides/troubleshooting/) guide.

## Compatibility evidence

The maintained [Codex compatibility matrix](/reference/codex-compatibility/)
records the exact frontend, Codex runtime version, and platform used for live
TUI/exec/app hooks, MCP JSON, output-spill, and Windows-command evidence. The
macOS Local app row now covers a real repo chat, MCP read/write/read, and Stop
harvest; it does not silently widen that result to app-only setup, hook trust,
subagents, managed worktrees, uninstall, or Windows. The checked-in fixture
harness documents how to recapture CLI contracts without touching the
operator's `CODEX_HOME`.

Primary upstream contracts: [Codex hooks](https://learn.chatgpt.com/docs/hooks),
[Codex MCP](https://learn.chatgpt.com/docs/extend/mcp), and
[UUIDv7](https://www.rfc-editor.org/rfc/rfc9562.html#section-5.7).
