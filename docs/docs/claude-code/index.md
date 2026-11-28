# Claude Code setup

> Register the MCP endpoint, install the hooks that make sessions ambient, map your repos, and verify each step.

Claude Code is the client Seamless is built around. Wiring it up is two
independent halves - **the MCP endpoint** (what an agent can call) and **the
hooks** (what happens without an agent calling anything) - and you want both.
One command installs both.

## Install the hooks and register MCP

```bash
seamlessd install-hooks --client claude
```

The explicit flag selects only Claude Code. Omit it to use the normal
detection-based installer, which prompts on a terminal and can wire Codex too.

This merges seven hook entries into your Claude Code `settings.json`, preserving
everything already there and backing the file up once before the first change.
It is idempotent: an already-current file is left untouched. If your
organization's [managed settings](https://thereisnospoon.org/docs/reference/cli-seamlessd/#seamlessd_install_hooks_managed)
already carry a Seamless hook, that event is left to them and not written
again, because Claude Code would run both copies.

It then registers the MCP server with `claude mcp add-json --scope user`. For a
daemon reached over plain http, which a default install is, the registration
names the daemon's URL and uses `seam mcp-headers --config <absolute yaml>` as
`headersHelper`, so the bearer key stays out of subprocess argv and
`~/.claude.json`. Over https or a socket it registers the `seam mcp-proxy`
bridge instead ([below](#https-and-socket-servers)). Pass
`--mcp=false` to skip that half; if the CLI is missing or registration fails,
the hooks still land and the command prints the exact invocation to run
yourself.

The hooks are what make sessions **ambient** - an agent gets a briefing at
startup, its prompts get matched against your memories, its Task subagents get
the project's constraints plus spawn-prompt-matched memories at spawn, its
findings get harvested at the end, and
its plan-mode activity gets captured, all without the agent choosing to call
anything. See [Sessions & briefings](https://thereisnospoon.org/docs/concepts/sessions/)
for what that delivers and [the hooks reference](https://thereisnospoon.org/docs/reference/hooks/) for the
exact table.

### Why every hook is a command

Claude Code can run a hook as a command or as an http request, and every
Seamless hook is a command running `seam hook <event>`: SessionStart only fires
as a command hook, SessionEnd as http races Claude Code's teardown, the
plan-capture hooks pre-filter locally so the machine-wide `Write`/`Edit` hot
path never touches the network, and the Claude app may not send an http hook
at all. No bearer key lands in `settings.json`. The full
rationale, hook by hook, is in
[the hooks reference](https://thereisnospoon.org/docs/reference/hooks/#why-every-hook-is-a-command).

### Over https or a socket {#https-and-socket-servers}

The `headersHelper` registration is the shape for a daemon reached over plain
http. When `server_url` is `https://` (a Client reaching its Server over TLS)
or `unix://` (a [relay](https://thereisnospoon.org/docs/concepts/networked/#the-relay), or a
[peer_auth](https://thereisnospoon.org/docs/guides/peer-auth/) daemon's socket), MCP is registered as the
`seam mcp-proxy` stdio bridge instead. Claude Code's own http client cannot be
told about a private CA, and cannot dial a socket at all, so MCP goes through
`seam` there, as the hooks do everywhere
([why](https://thereisnospoon.org/docs/guides/network-install/#why-https-changes-the-claude-code-wiring)).

## Registering the MCP endpoint by hand

For a different MCP client, or when `install-hooks` could not find the
`claude` CLI:

```bash
claude mcp add-json --scope user seamless \
  '{"type":"http","url":"http://127.0.0.1:8081/api/mcp","headersHelper":"/abs/path/seam mcp-headers --config /abs/path/seamless.yaml"}'
```

**`--scope user` is required.** Without it, `claude mcp add` defaults to `local`,
which ties the registration to whichever directory you happened to run it from.
The tools then work in that one directory and silently vanish everywhere else -
which reads exactly like "the MCP server is broken" rather than "the
registration is scoped wrong".

Use the absolute installed `seam` and config paths. The helper reads
`mcp.api_key` at connection time; do not replace it with a literal `--header`,
which exposes the daemon's sole credential in process argv and client config -
and drops the agent-process header the helper also prints, which is what binds
each agent's tool calls to its own session.

With an `https://` or `unix://` `server_url`, register the stdio bridge
instead, as `install-hooks` does:

```bash
claude mcp add-json --scope user seamless \
  '{"type":"stdio","command":"/abs/path/seam","args":["mcp-proxy","--config","/abs/path/seamless.yaml"]}'
```

## Map your repos

Usually you do not have to. The first session inside a git repo maps itself: the
SessionStart hook resolves the agent's cwd to its git root, derives a project
slug from that directory's name, and records the mapping. Agents then inherit
project scope from their cwd - no `project` argument on any call, and no
`session_start`: each agent's tool calls are
[bound to the session its hook opened](https://thereisnospoon.org/docs/concepts/sessions/#process-binding),
even with agents working in several repos at once.

Map by hand to override the derived slug - an `ios` directory that should be the
`example-ios` project:

```bash
seamlessd map-repo --path ~/code/ios --project example-ios
```

See [Projects & scope](https://thereisnospoon.org/docs/concepts/projects/) for the full precedence chain and why
unmapped writes are rejected rather than defaulted to global.

## Tell your agents it exists

Installing the tools does not mean an agent will use them well. Two things help.

First, run `/seam-onboard`. The installer drops the portable Claude Code copy
into `~/.claude/skills/`; from a clone, `make install-onboard-skill`
(re)installs it for the explicit Claude profile:

```bash
make install-onboard-skill CLIENT=claude
```

`/seam-onboard` walks an agent through the setup above, verifies each step, and
shows the marked Seamless-awareness block it can add to global or project
`CLAUDE.md`. It edits only after you choose a scope and approve the change, then
removes its own one-shot skill directory.

The installer drops a second skill alongside it *if research is enabled*:
`/seam-research`, the research-lab workflow for systematic debugging (see
[Run research trials](https://thereisnospoon.org/docs/guides/research-trials/)). Research is an
[optional feature](https://thereisnospoon.org/docs/reference/console/#optional-features) and ships off, so a
fresh install has no research skill until you turn it on and re-run
`seamlessd install-hooks`. Unlike `/seam-onboard` it is a recurring
workflow, not a one-shot - it never self-removes, upgrades refresh it in place,
and Claude can activate it on its own when an investigation calls for
structured trials. From a clone, `make install-research-skill CLIENT=claude`
(re)installs it.

Second - or if you skip the skill - add that block to your `CLAUDE.md` by hand:
describe when to reach for Seamless (memory that should outlive the
conversation, work that crosses agents) and when not to (trivial edits, things
the codebase already records).
The briefing tells an agent *what you know*; your `CLAUDE.md` tells it *when to
write more down*.

## The Claude app's code surface {#the-claude-apps-code-surface}

Code sessions inside the Claude desktop app are real Claude Code, running
against the same `~/.claude` profile as your terminal sessions. The setup on
this page is therefore all the setup they need: the hooks in `settings.json`,
the user-scope MCP registration, and the skills apply to app code sessions
unchanged, with nothing extra to install.

Three app-specific facts are worth knowing, all recorded with build-pinned
evidence in the [Claude app compatibility
matrix](https://thereisnospoon.org/docs/reference/claude-app-compatibility/):

- **The app bundles its own Claude Code runtime**, retained under
  `~/Library/Application Support/Claude/claude-code/<version>/`, and it can lag
  or lead the PATH CLI (observed: 2.1.215 in the app beside a 2.1.216 CLI).
  `seamlessd doctor` reports each discoverable runtime on its own line -
  `claude CLI runtime` and `claude app runtime` - so the skew is visible when
  an app-only failure has you debugging.
- **Every hook is a command hook, the shape the app was seen to run.** On the
  observed build, the SessionStart briefing, PostToolUse events, and the
  SessionEnd findings harvest - all command hooks - worked. UserPromptSubmit,
  which installs against a plain-http daemon used to write as an http hook,
  left no trace, though that capture predates v0.10.6, whose daemon left no
  trace of any Claude Code prompt, so it shows nothing either way. It is a
  command hook now, and per-prompt `<seam-recall>` and the Diary's mid-session
  usage ride it; an install from before that gets the command hook the next
  time `seamlessd install-hooks` runs, which an update does for you.
- **SessionEnd may come late, or never.** The app keeps a session open until
  you archive it or quit, and a session it stops ends with no hook at all.
  Once the agent ends such a session with `session_end`, or the idle reaper
  expires it after `gardener.session_idle_minutes`, its usage is
  [owed](https://thereisnospoon.org/docs/reference/hooks/#owed-usage), and the daemon reads its transcript
  later, so its agent time still reaches the
  [Diary](https://thereisnospoon.org/docs/reference/console/#diary-usage).
- **Managed worktrees keep their project.** The app may run a session in a
  managed worktree (`<repo>/.claude/worktrees/<name>`); Seamless resolves a
  linked worktree through its git common directory to the main checkout, so
  such a session inherits the repo's project instead of registering a
  transient one named after the worktree.

Chat conversations in the same app are a different surface entirely - no
hooks, no `CLAUDE.md`, an explicit opt-in MCP registration of its own. See
[Claude app chat setup](https://thereisnospoon.org/docs/claude-app/).

## Verify

```bash
seamlessd doctor    # config, database, tool count, and the hooks check
```

Then start a Claude Code session in a mapped repo and look for the injected
block:

```text
Injected context myrepo
<seam-briefing>
Seam project: myrepo -- 58 memories (4 constraints), 3 recent findings.
...
</seam-briefing>
The project name is the useful proof: the ambient session resolved to the repository you opened.
```

If it is there, the whole loop is closed: the hook resolved your cwd to a
project, the daemon assembled a briefing within its token budget, and the agent
started already knowing what you know.

If it is **not** there, note that hooks fail open by design - a broken hook never
blocks your agent, which means **silence is the failure mode**. Start with
`seamlessd doctor`; the [Troubleshooting](https://thereisnospoon.org/docs/guides/troubleshooting/) guide is
organized by symptom.
