# Hooks

> The hooks Seamless installs per client - seven for Claude Code, five for Codex - their transports and timeouts, the fail-open contract, and what install-hooks writes.

Seamless installs hooks for two clients: seven for Claude Code (including
plan-mode capture) and five for [Codex](#codex-local-host-five-hooks). The
hooks are what makes sessions ambient - an agent gets a briefing at session
start, its prompts get matched against stored memories, and its findings get
harvested, all without the agent calling a single MCP tool. Every hook fails
open: an internal error still returns success, so a broken daemon never blocks
the agent - see [the fail-open contract](#the-fail-open-contract) for the cost.

`seamlessd install-hooks --client <claude|codex|all|detect>` selects the
profile; run interactively with no
`--client` it prompts for the client(s), and a non-interactive run without the
flag resolves `detect`: the clients present on this machine (the `claude`/`codex`
CLI or a `~/.claude`/`$CODEX_HOME` directory). When neither is found, an
interactive run warns and asks whether to install at all (defaulting to no), and
a non-interactive run errors - nothing is wired without an explicit choice. The
curl installer and `make install` make the same choice.

## The profiles at a glance

The [Claude app chat surface](https://thereisnospoon.org/docs/claude-app/) is in the table because people ask
where its hooks are: it has none - it is an MCP-only install target, not a
hook client.

| | Claude Code | Codex (app, CLI, IDE) | Claude app chat |
|---|---|---|---|
| Hooks installed | seven | five | none - MCP only |
| Command form | exec form (`command` + `args`), identical on every OS | shell string, plus a `command_windows` variant | - |
| Session close | `SessionEnd` harvests and completes | no `SessionEnd` - [the idle reaper expires it](https://thereisnospoon.org/docs/codex-cli/#no-sessionend-the-reaper-closes-sessions) | explicit `session_end`, or it never closes |
| Trust gate | none | [/hooks approval](https://thereisnospoon.org/docs/codex-cli/#trust-the-hooks-once) before any hook runs | - |
| Injection cap | none | 2,400 estimated tokens per response | - |
| Plan capture | `PostToolUse` + `PermissionRequest` | none | - |

## Claude Code: seven hooks

Taken from the `seamlessHooks` definition in `internal/hooks/install.go`, in
install order:

| Event | Matcher | Transport | Timeout | Endpoint | Effect |
|---|---|---|---|---|---|
| `SessionStart` | `startup\|resume\|clear\|compact` | command (`seam hook session-start`) | 10s | `/api/hooks/session-start` | Registers the agent's cwd in the repo→project map, assembles the `<seam-briefing>`, and creates or resumes an opaque `cc/<prefix>-<digest>` ambient handle keyed by the full external ID, stamped with the [agent process](#what-rides-on-the-query-string) that ran the hook. |
| `UserPromptSubmit` | none | http | 5s | `/api/hooks/user-prompt-submit` | Heartbeats the ambient session, matches the prompt against stored memories, and injects a recall block. A miss is logged as a `hook.prompt` event. |
| `SessionEnd` | none | command (`seam hook session-end`) | 10s | `/api/hooks/session-end` | Harvests findings and completes the agent's sessions. Bare ack - Claude Code's schema has no `hookSpecificOutput` for `SessionEnd`. |
| `PostToolUse` | `Write\|Edit\|MultiEdit\|ExitPlanMode` | command (`seam hook post-tool-use`) | 10s | `/api/hooks/post-tool-use` | Heartbeats the ambient session. Captures plan-file iterations (`Write`/`Edit`/`MultiEdit` under the plans dir) and plan approvals (`ExitPlanMode`). |
| `SubagentStart` | none | command (`seam hook subagent-start`) | 10s | `/api/hooks/subagent-start` | Injects the child briefing - pinned constraints, up to three `RELEVANT:` memories matched from the child's spawn prompt, and a recall/memory_read footer - into every Task subagent and heartbeats the parent. It never creates, reactivates, or re-scopes an ambient session. |
| `SubagentStop` | none | command (`seam hook subagent-stop`) | 10s | `/api/hooks/subagent-stop` | Caches a planning subagent's prompt and final report as a `cc-agent-<agent_id>` note in the plan composition. |
| `PermissionRequest` | `ExitPlanMode` | command (`seam hook permission-request`) | 10s | `/api/hooks/permission-request` | Marks the session's draft plan as presented when the user is prompted to review an `ExitPlanMode` call. |

Timeouts are in seconds, Claude Code's unit. Server-side the briefing and recall
paths are additionally bounded at 2s and the capture paths at 8s, so a slow store
cannot spend the whole hook budget.

`SessionStart` is the only hook with a matcher on session sources, and Claude
Code never fires it for Task subagents - its sources are the main-session ones
(`startup`/`resume`/`clear`/`compact`). A child is briefed by `SubagentStart`
instead: the same child briefing the Codex profile injects, keyed off
the parent's `session_id`. Subagents share the parent's session and get no
ambient session of their own.

What a child receives is deliberately narrower than the main briefing - the
parent's prompt carries the task context, so there are no findings, tasks, or
plan sections. It has three parts:

- **Constraints**, pinned and tiered exactly like the main briefing (the top
  `constraint_max_full` in full, the rest named on the compact
  `+N more, equally binding` line).
- **A "Relevant to this task" section** - up to three memories of any kind matched against the
  child's spawn prompt (read best-effort from the child transcript or Codex
  rollout; a prompt that has not been flushed yet just means no section). A
  constraint already visible in either tier is never repeated here, and the
  dedupe never costs the section a genuinely-new match. These are passive
  injects: they carry zero utility weight, unlike a `<seam-recall>` match on
  the agent's own prompt.
- **A closing footer** naming `recall` and `memory_read`, so even a child in a
  project with two constraints knows how to pull more.

```text
<seam-briefing>
Seam project: seamless -- 24 constraints (subagent scope).

Constraints (binding for every session):
- fts-or-vs-allterms-presence-probe: ftsQuery ORs terms ...
- +14 more, equally binding -- memory_read name=<name> before working near one: console-csrf-origin-check-contract, ...

Relevant to this task:
- chroma-boot-race: chroma container health check startup race
Recall on demand with recall; read a memory with memory_read.
</seam-briefing>
```

## Codex local host: five hooks

`seamlessd install-hooks --client codex` installs five hooks into
`~/.codex/hooks.json` (or `$CODEX_HOME/hooks.json`) instead of the seven above.
The desktop app, CLI, and IDE extension share this profile for the same Codex
host. Seamless has no verified Claude-style plan-file/`ExitPlanMode` surface to
capture from Codex, and Codex emits no `SessionEnd` event in 0.144.6. The profile
combines the three parent-session hooks with a deliberately bounded subagent
lifecycle.

| Event | Transport | Endpoint | Effect |
|---|---|---|---|
| `SessionStart` | command (`seam hook session-start --client codex`) | `/api/hooks/session-start` | Registers the cwd's project, assembles the `<seam-briefing>`, and creates or resumes an opaque `cx/<prefix>-<digest>` ambient handle keyed by the full external ID, stamped with the [agent process](#what-rides-on-the-query-string) that ran the hook. |
| `UserPromptSubmit` | command (`seam hook user-prompt-submit --client codex`) | `/api/hooks/user-prompt-submit` | Heartbeats the ambient session, matches the prompt against stored memories, and injects a recall block. |
| `Stop` | command (`seam hook stop --client codex`) | `/api/hooks/stop` | Heartbeats and harvests findings from the turn's final assistant message. No injection - Codex's `Stop` has no `hookSpecificOutput`. Fires at every turn end. |
| `SubagentStart` | command (`seam hook subagent-start --client codex`) | `/api/hooks/subagent-start` | Injects the child briefing (constraints, spawn-prompt-matched `RELEVANT:` memories, recall footer) under the Codex output cap and heartbeats the parent. It never creates, reactivates, or re-scopes an ambient session. |
| `SubagentStop` | command (`seam hook subagent-stop --client codex`) | `/api/hooks/subagent-stop` | Heartbeats the parent only. It does not apply the child's model/final message to parent state or create Claude-style plan notes. |

All five are `command` hooks. Codex's current hook schema executes command
handlers, and every non-managed command hook passes through Codex's **trust
gate**. An untrusted or changed definition is skipped, so a fresh install can
show no briefing until the current commands are reviewed in `/hooks`. The
`--dangerously-bypass-hook-trust` flag is the automation-only alternative.

The `--client codex` discriminator rides as a `?client=codex` query param on the
same `/api/hooks/*` endpoints the daemon already serves; the daemon normalizes
Codex's payload (its `prompt` field, its `Stop` `last_assistant_message`) into the
shared shape. Omitting the flag means Claude Code - the endpoint URLs are
identical. Because Codex never sends a `SessionEnd`, its sessions close through the
idle reaper rather than a clean cascade; [Codex local setup](https://thereisnospoon.org/docs/codex-cli/) covers
that lifecycle and the tool-call approval gate.

Codex limits each model-visible hook-output entry to roughly 2,500 tokens and
spills larger values to a temporary file. Seamless caps every Codex
`additionalContext` response at 2,400 estimated tokens before it records
injection telemetry or serializes the response. That covers SessionStart,
UserPromptSubmit, and SubagentStart, and keeps the emitted bytes equal to the
recorded bytes.

## The fail-open contract

**A hook must never block an agent.** Every handler in `internal/hooks` honors
this: an internal error yields a 200 with empty `additionalContext` rather than a
failure. Only a bad bearer key (401) or an unknown `?client=` discriminator
(400) returns non-2xx - both are install bugs, not runtime conditions.

The same contract runs client-side. `seam hook <event>` reports a failure to
stderr and exits 0 - a server that is down, a config that will not load, an
unreadable stdin, all produce exit 0. Only a misconfigured event name or
`--client` value (an install bug, not a runtime condition) is a hard error.
Each hook also gives up 1s before its client would kill it - at 4s on
`UserPromptSubmit`, 9s on the rest - so a server that never answers, such as
one on a LAN the laptop has left, where a dial hangs rather than being
refused, still ends in a quiet exit 0 rather than a hook-timeout warning.

Plan and subagent capture are best-effort under the same rule: a capture problem
is logged and the hook still acks 200.

The consequence is that **hook failure is silent**. A stopped daemon, a bad key,
a `seam` binary that moved - none of these produce an error an agent or the user
will see. Work simply proceeds without a briefing, and nothing announces it.

So troubleshooting starts with the doctor checks, not with looking for an error:

```bash
seamlessd doctor   # desired definitions, Codex trust/activity, and MCP state
seam doctor        # server reachable, key accepted, tools/list count
```

For Claude Code, `seamlessd doctor` looks in `~/.claude/settings.json` and then
`./.claude/settings.json` and compares installed entries with the definitions the
installer would write now. For Codex it separately reports exact
current/stale/missing definitions, separately discoverable CLI/app runtime
versions, trust (`unverified; inspect /hooks in Codex CLI`; the desktop app does
not expose that command), recent
SessionStart/UserPromptSubmit activity (evidence only), and the machine-readable
MCP state. It also checks the recorded binary and config targets exist. A machine
with no Codex CLI, initialized home, or Seamless Codex configuration is one quiet
`not detected` line, never a failure. `seam doctor` covers the other half: it hits
`/healthz` and calls `tools/list`, which proves the endpoint and key are working.

## What rides on the query string

A hook body is the agent client's schema, not Seamless's, so everything
Seamless needs *about* the call travels beside it as query parameters on the
same `/api/hooks/*` endpoints. `?client=` is one of them; the agent's machine
identity and its agent process are the rest.

| Param | On | Value |
|---|---|---|
| `client` | every hook, when `--client` was passed | `codex`; omitted means Claude Code |
| `host` | every command hook | this machine's hostname, lower-cased |
| `repo_root` | `session-start` only | the enclosing repository root |
| `main_root` | `session-start` only | the main checkout, when `repo_root` is a linked worktree |
| `origin` | `session-start` only | the `origin` remote URL, when there is one |
| `agent_process` | `session-start` only | the agent process that ran the hook - its nearest ancestor that is not a shell, as `<pid>.<start time>` |

`seam hook` resolves the identity locally - it is the only process that can -
and every value is best-effort: an unreadable hostname, an unparseable body, or
a cwd outside any repository simply omits that param. The host goes on every
hook because it is what tells the daemon whether the paths in this payload are
on its own disk; the roots go on session start only, because that is the single
hook that places a working directory in a project.

**An absent `host` means the local machine.** That is what keeps an older
`seam` binary - which only ever talked to a daemon on its own machine - working
byte-for-byte.

**`agent_process` is the one param about the agent, not the machine.** It
names the `claude` or `codex` process, and the daemon stamps it on the ambient
session. The MCP transports that same process launches send it back in an
`X-Seamless-Agent-Process` header, which binds the agent's tool calls to the
session with no `session_start` - see
[How tool calls find their session](https://thereisnospoon.org/docs/concepts/sessions/#process-binding). A
malformed value is dropped; an absent one (an older `seam`) leaves the session
reachable only through the older fallbacks.

Claude Code's `UserPromptSubmit` is the one http hook, so no `seam` process
computes an identity for it; the daemon attributes it through the ambient
session that client already has.

When the host is *not* the daemon's, every daemon-side read of the agent's
filesystem is skipped rather than attempted, and the skip is recorded as a
`hook.error` event with stage `remote-host-skip`. It is a host check and not a
"does the file exist" check on purpose: two devices with the same username and
home layout produce the same transcript and plan-file paths, so a missing-file
test would sometimes find a real file - the wrong one. See
[Share one daemon across a LAN](https://thereisnospoon.org/docs/guides/network-install/#what-a-remote-device-does-not-get)
for the full list of what that covers.

### The ambient session line {#the-ambient-session-line}

Whether the hook named the agent process decides the line SessionStart appends
to the briefing. Named:

```text
Seam session: cc/<id> (ambient) -- this agent's Seamless tool calls bind to it automatically; no session_start needed
```

Not named - an older `seam`, or one that could not identify the agent process -
the agent's connection is not bound to the session, so the line gives the one
call that binds it:

```text
Seam session: cc/<id> (ambient) -- if a tool reports an ambiguous scope, session, or agent, bind once with session_start name=cc/<id>
```

A Codex session's line carries its `cx/...` handle.

## Why Claude Code uses two transports

Six of the Claude Code hooks are `command`, one is `http`. (Codex, above, uses
`command` for all five - its trust gate applies only to command hooks.) The
split is not stylistic:

- **`SessionStart` must be a command hook.** Claude Code only runs
  `command`/`mcp_tool` hooks for `SessionStart`. An `http` hook there is silently
  skipped, and the briefing and ambient session never fire.
- **`SessionEnd` does support `http`, but it races.** At process exit the
  fire-and-forget request races Claude Code's teardown, so the ambient-session
  harvest often never lands and sessions pile up as active. A command hook is
  one Claude Code waits on, which makes the harvest reliable.
- **The plan-capture hooks are commands for cost.** `PostToolUse` fires
  machine-wide on every `Write`/`Edit`. The `seam` CLI pre-filters the payload
  locally and drops non-plan events before any config load or network round-trip,
  so the hot path never touches the network.
- **`SubagentStart` mirrors the Codex profile.** Both clients run the same
  `seam hook subagent-start` command, so the subagent lifecycle stays one
  transport and one code path across profiles.
- **`UserPromptSubmit` stays `http`.** It fires mid-turn, where http is reliable.

Command hooks work by having Claude Code pipe the event JSON to the command's
stdin; `seam hook <arg>` forwards that to the matching endpoint and echoes the
response back on stdout. Same server logic either way - only the transport
differs.

One practical consequence: the bearer key is only written into settings.json for
the `http` hook. Command hooks read it from the config file at hook time.

## What install-hooks writes

```bash
seamlessd install-hooks                        # default: --client detect
seamlessd install-hooks --settings ./.claude/settings.json
seamlessd install-hooks --url http://127.0.0.1:8081
seamlessd install-hooks --seam /path/to/seam
seamlessd install-hooks --client codex         # hooks + codex mcp add + ~/.codex/skills
seamlessd install-hooks --client all           # both clients in one pass
```

The base URL defaults to one derived from the config's bind address (a bind-all
host maps to loopback). `--seam` defaults to the `seam` binary sitting next to
the running `seamlessd`, falling back to a bare `seam` resolved from `PATH` at
hook time. The command fails if `mcp.api_key` is empty. The examples below are the
Claude Code (`settings.json`) shapes; the Codex profile is
[shell strings, not exec form](#the-codex-profile-is-shell-strings).

An http entry looks like this:

```text
{
  "hooks": {
    "UserPromptSubmit": [
      {
        "seamless_managed": true,
        "hooks": [
          {
            "type": "http",
            "url": "http://127.0.0.1:8081/api/hooks/user-prompt-submit",
            "timeout": 5,
            "headers": { "Authorization": "Bearer <mcp.api_key>" }
          }
        ]
      }
    ]
  }
}
```

A command entry carries no key and uses **exec form** - a bare `command` plus an
`args` array, spawned directly with no shell:

```text
{
  "seamless_managed": true,
  "matcher": "startup|resume|clear|compact",
  "hooks": [
    {
      "type": "command",
      "command": "/abs/path/seam",
      "args": ["hook", "session-start", "--config", "/abs/path/seamless.yaml"],
      "timeout": 10
    }
  ]
}
```

Exec form is deliberate: it is the one shape that behaves identically on every
OS. Claude Code runs a shell-form command hook through `sh -c` on Unix but
PowerShell on Windows, where a POSIX string (an env prefix plus single-quoting)
is not valid syntax; exec form passes each argument verbatim with no quoting at
all. The config path is passed as `--config` because the hook fires from any
cwd, where the CLI's cwd-relative search for `seamless.yaml` would miss and leave
it unable to authenticate (exec form carries no environment, so this replaces the
older `SEAMLESS_CONFIG` env prefix). It is omitted when the config came from
defaults and env with no file. The `matcher` key is omitted entirely for hooks
that have none.

### The Codex profile is shell strings

Codex's `hooks.json` schema takes a **shell-string** command, not the exec-form
`command` + `args` array Claude Code uses. So the Codex profile writes a
POSIX-quoted `command` and a double-quoted `command_windows`, both resolving the
same `seam` binary plus `hook <event> --config <yaml> --client codex`:

```text
{
  "hooks": {
    "SessionStart": [
      {
        "seamless_managed": true,
        "hooks": [
          {
            "type": "command",
            "command": "'/abs/path/seam' hook session-start --config '/abs/path/seamless.yaml' --client codex",
            "command_windows": "\"C:\\abs\\path\\seam.exe\" hook session-start --config \"C:\\abs\\path\\seamless.yaml\" --client codex",
            "timeout": 10
          }
        ]
      }
    ]
  }
}
```

Both forms carry the same resolved paths; the installer runs once per OS, so only
the matching one ever fires. Codex's own file struct rejects unknown keys at the
top level (only `description` and `hooks` are allowed there), but its matcher
group and command handler tolerate extra fields, so the `seamless_managed` marker
sits safely on the matcher group exactly as it does for Claude Code.

Install behavior:

- **Unknown keys are preserved.** The file is decoded into a generic map, so
  everything Seamless does not own survives.
- **Backed up once.** The first time Seamless changes a hook file it copies it to
  `<file>.seamless-bak-<timestamp>`. Later installs skip the backup, so the true
  original is never overwritten with a modified copy.
- **Idempotent.** An already-current file is left untouched - no rewrite, no
  backup. Per-hook actions are reported as `added`, `updated`, `adopted`,
  `deduped`, or `unchanged`.
- **Written atomically**, sorted-key indented, preserving the file mode (0600 for
  a file Seamless creates, since it may hold a bearer key).

### Exact definitions, legacy adoption, and foreign hooks

Entries are tagged `seamless_managed: true`, but the marker alone cannot prove a
definition is healthy. It can name an old binary or config, and Claude Code may
strip the unknown marker while preserving a still-valid hook. Install, doctor,
and uninstall therefore share one four-way classifier:

| Class | Meaning | Install | Uninstall |
|---|---|---|---|
| current | Exactly the definition Seamless would write today | Leave byte-for-byte alone | Remove |
| managed-stale | Carries Seamless's marker but differs from desired state | Replace | Remove |
| recognizable legacy | Marker-free, but unmistakably a documented Seamless URL or `seam`/`seam.exe hook <event>` layout | Adopt and replace | Remove |
| foreign | Anything else, even if arbitrary arguments happen to contain `hook <event>` | Preserve | Preserve |

For Codex, “current” includes the event, command handler type, `seam` or
`seam.exe` executable, hook argv, `--client codex`, expected absolute config
path, timeout, and both OS command forms. A missing discriminator or an old
binary is stale, not healthy. Recognizable legacy matching is deliberately
narrow: arbitrary executables, extra shell operators, malformed quoting, and a
v1 `seam_managed` entry at another URL remain foreign.

This classifier is why re-install can repair owned drift without swallowing a
neighboring user's hook, why duplicates collapse to one canonical entry, and why
doctor compares current **desired state** rather than counting anything that
looks vaguely Seamless-shaped.

## Related

- [Configuration](https://thereisnospoon.org/docs/reference/configuration/) - `mcp.api_key`, the bind address
  the hook URLs derive from, and the `briefing:` block that tunes what
  `SessionStart` injects.
- [MCP API overview](https://thereisnospoon.org/docs/reference/mcp/) - the tool surface the same daemon serves.
- [Claude Code setup](https://thereisnospoon.org/docs/claude-code/) and [Codex local setup](https://thereisnospoon.org/docs/codex-cli/) - the
  per-client walkthroughs these hooks come from.
- [Codex compatibility matrix](https://thereisnospoon.org/docs/reference/codex-compatibility/) - versioned
  live/schema evidence and the recapture procedure.
- [Quickstart](https://thereisnospoon.org/docs/quickstart/) - install order for a working setup.
