---
title: seam CLI
description: Every seam subcommand - agent loop, tasks, plans, observability, hooks - plus the flag-order rules and what each one rejects.
---

`seam` is the headless CLI. It does no work itself: it loads the same
configuration the daemon does, then talks to a running `seamlessd` - over MCP at
`/api/mcp` for most commands, and over the console's JSON endpoints for the
owner-only actions that do not exist as MCP tools (force-releasing a task lock,
approving a plan). Both paths authenticate with the static bearer key from
[Configuration](/reference/configuration/), so if the key is wrong or the daemon
is down, every subcommand below fails at the connection.

The target address is derived from the configured bind address, with a bind-all
host (`0.0.0.0`, `::`, or empty) mapped to loopback.

## Flags and positionals

Every `seam` command takes flags on either side of its positionals. These two
lines are the same line:

```bash
seam capture --project myproj https://example.com/page
seam capture https://example.com/page --project myproj
```

```bash
seam task release --force 01K7ABCD    # --force applies
seam task release 01K7ABCD --force    # --force applies here too
```

An unknown flag is rejected rather than absorbed into the positionals, so a typo
is an error instead of a silently different command: `seam recall foo --projct p`
reports `flag provided but not defined: -projct`. A positional that starts with
`-` needs the `--` terminator (`seam recall -- -foo`).

`seam task done|start|drop|reopen` and `seam plan show|approve` parse no flags at
all. Commands that take only flags and no positionals (`ready`, `task list`,
`task add`, `plan list`, `status`, `usage`, `doctor`) are unaffected either way.

## Which session a command acts in {#which-session}

Every MCP request `seam` makes names the machine it runs on and the agent process
that launched it - its nearest ancestor that is not a shell - the same identity
[the MCP bridge](#mcp-bridge-and-auth-helper) sends for an agent. That decides
where a command without `--project` lands:

- **Run by a Claude Code or Codex agent through its shell**, a command is bound
  to that agent's own session, like the agent's MCP calls: `seam recall` and
  `seam remember` resolve to its project.
- **Run by hand in a terminal**, the nearest non-shell ancestor is the terminal
  or tmux, which no hook names, so the command is unbound - and its fallback
  skips sessions other agents' processes own. Pass `--project` to reach a
  project from a terminal: without it, a read or a write is refused as
  ambiguous while any agent is live, rather than quietly answered from the
  global scope, and only with no agent live does a read see the global scope.

See [How tool calls find their session](/concepts/sessions/#process-binding).

## Agent loop

The four commands an agent - or you, standing in for one - uses to start a
session, write knowledge, and search it back.

### seam prime {#seam_prime}

```bash
seam prime [--cwd DIR] [--name NAME]
```

Calls `session_start` and prints the briefing to stdout; the session id and
resolved project go to stderr, so the briefing pipes cleanly. `--cwd` defaults
to the current directory and is what resolves the project via the repo mapping.
Reusing a `--name` resumes that session rather than opening a new one. When
there is no briefing content yet, it says so on stderr.

This is the explicit form of what the SessionStart hook does automatically. Run
by a Claude Code or Codex agent in its own repository, it adopts that agent's
session rather than opening another.

### seam remember {#seam_remember}

```bash
seam remember --name N --kind K --description D [--body TEXT] [--project P]
```

Calls `memory_write`. `--name`, `--kind`, and `--description` are required.
`--kind` is one of `constraint`, `runbook`, `protocol`, `gotcha`, `decision`,
`refuted`, `reference`, or `stage`. `--description` is the one-line summary
(<=150 chars) that indexes show.

The body comes from `--body`, or from **stdin** when `--body` is omitted - an
empty body after either route is an error, not an empty memory. `--project`
resolves the same way as it does for [capture](#seam_capture): empty inherits the
session's project and is an error when nothing pins it, and `global` is the
explicit escape hatch. Output reports whether the write created or updated the
memory, and names a similar existing memory when the server flags one.

### seam recall {#seam_recall}

```bash
seam recall QUERY [--scope all|memories|notes] [--project P] [--limit N]
```

Calls `recall`, the single RRF-fused search entry point. Scope defaults to `all`
and limit to `10`. Query words are joined with spaces, and flags may sit on
either side of them. A query word that starts with `-` needs the `--` terminator
(`seam recall -- -foo`), which is the only reason to reach for it. Each hit
prints its kind, name, age, source, score, and description; no matches prints
`no results`.

An unknown flag is an error, not a search word: `seam recall foo --projct p`
reports `flag provided but not defined: -projct` rather than quietly searching
for the literal text `foo --projct p`.

### seam capture {#seam_capture}

```bash
seam capture [--project P] URL
```

Calls `capture_url` to fetch a page through the SSRF-safe fetcher and store it
as a note. An empty `--project` does not mean global: the scope resolves to the
session's project - the bound session's, or a single unambiguous ambient one
([which](#which-session)).
The server refuses to guess rather than pick a default, so a capture with
nothing to infer from, or one made while ambient sessions span several projects,
is an error naming the fix. Pass `--project global` to file the note globally
(`notes/_global/`), or `--project <slug>` to name a project outright.

`--project` binds on either side of the URL: `seam capture --project p URL` and
`seam capture URL --project p` are the same line.

## Tasks

The queue side of the CLI. See [Tasks](/reference/mcp/tasks/) for what ready,
blocked, and claimed mean.

### seam ready {#seam_ready}

```bash
seam ready [--project P] [--blocked] [--plan S]
```

Calls `tasks_ready` and lists the actionable queue by short id and title.
`--blocked` additionally lists blocked tasks, each followed by its blockers and
their statuses. `--plan S` shows that plan's step tasks instead of the default
queue - plan steps are excluded from the default view. Prints
`ready: (nothing actionable)` when the queue is empty.

### seam task list {#seam_task_list}

```bash
seam task list [--id ID] [--project P] [--status S] [--plan S]
```

Calls `tasks_list`. `--status` filters to `open`, `in_progress`, `done`, or
`dropped`. `--id` loads a single task by its globally-unique id and ignores the
project, status, and plan filters. `--plan` lists a plan's steps instead of the
default non-plan tasks. Bare `seam task` is not an alias: it names the available
task subcommands and exits 2. Use `seam task list` when listing is what you mean.

### seam task add {#seam_task_add}

```bash
seam task add --title T [--body B] [--project P] [--depends id,id] [--plan S]
```

Calls `tasks_add`. `--title` is required. `--depends` takes a comma-separated
list of blocker task ids. `--plan S` composes the task as a step of the plan
`plan:<slug>`, which keeps it out of the default queue.

### seam task done|start|drop|reopen {#seam_task_transition}

```bash
seam task done <id>
seam task start <id>
seam task drop <id>
seam task reopen <id>
```

Calls `tasks_update` with the mapped status: `done` → `done`, `start` →
`in_progress`, `drop` → `dropped`, `reopen` → `open`. These parse no flags.

### seam task claim {#seam_task_claim}

```bash
seam task claim [--lease SECS] <id>
```

Calls `tasks_claim` to atomically take a ready task, printing the new status and
the lease expiry. `--lease` overrides the server's default lease (900 seconds);
it is only sent when greater than zero.

### seam task heartbeat {#seam_task_heartbeat}

```bash
seam task heartbeat [--lease SECS] <id>
```

The same `tasks_claim` call as `claim`. Re-claiming a task you already hold
refreshes its lease, so `heartbeat` is that path under a name that says what it
is for during long work. There is no separate heartbeat tool.

### seam task release {#seam_task_release}

```bash
seam task release [--force] <id>
```

Without `--force`, calls `tasks_release`, which only releases a claim you hold.

`--force` is the owner override and takes a different route entirely: it POSTs
to the console's release endpoint, which is bearer-authenticated and force-releases
any holder's claim. Agents on the MCP surface cannot reach that path.

## Captured plans

Owner surface over the plans Claude Code plan mode captures. `list`, `show`, and
`approve` are backed by the console's JSON endpoints; `check` also runs `git`
locally. Bare `seam plan` is not a command - it names its subcommands and exits
2, the same as bare `seam task`.

### seam plan list {#seam_plan_list}

```bash
seam plan list [--project SLUG] [--window WINDOW]
```

Lists captured plans with slug, status, title, project, iteration, agent count,
task progress, and age. `--window` is `24h`, `7d`, `30d`, or `all` (default
`all`) and is applied by the server; anything else is a parse error rather than a
silent fall back to the server's `24h` default. `--project` filters the returned
rows client-side.
Prints `(no captured plans)` when nothing matches.

`plan list` takes no positional: `seam plan list <slug>` is an error pointing at
`seam plan show`, not a listing of every plan.

### seam plan show {#seam_plan_show}

```bash
seam plan show <slug>
```

Prints one plan: its status, project, title, source file and iteration, the
notes attached to the composition (each marked `note` or `agent`), its tasks,
and then the plan body.

### seam plan check {#seam_plan_check}

```bash
seam plan check [--cwd DIR] <slug>
```

Staleness check against a repo's git history. `--cwd` defaults to the current
directory and must be a git repo. For the plan body and each attached note, it
reads the capture stamp (the `> captured from ... | git <head> | ...` line) and
compares it to the repo's current HEAD:

- **FRESH** - stamped at the current HEAD, or nothing changed since the stamped
  commit, or files changed but none the note mentions.
- **STALE** - files the note mentions by path changed between the stamped commit
  and HEAD. The changed paths are listed (truncated after five).
- **UNKNOWN** - no git stamp, captured outside a git repo, the stamped commit no
  longer resolves (rebased away), or the note could not be read.

Mentioned paths are extracted from the prose by pattern, and match whether the
note wrote them absolute or repo-relative. **The command exits non-zero when any
note is stale**, so it works as a gate in a script.

### seam plan approve {#seam_plan_approve}

```bash
seam plan approve <slug>
```

Escape hatch for when Claude Code skips the approval hook: flips the plan to
approved and creates its tracking task, reporting the new task or noting that
one already exists.

## Observability

### seam status {#seam_status}

```bash
seam status
```

Server health from the unauthenticated `/healthz` endpoint (status and version),
the configured data directory, then the project count and slugs via
`project_list` - which doubles as proof the static key works. When the daemon's
[update check](/updating/#automatic-update-checks) has seen a newer release, an
`update:` line names it and points at `seamlessd update --check` on the daemon's
machine, which knows how that install updates.

**`seam status` exits non-zero if any check failed**, so it works as a scripted
health gate. A partial answer is still printed - if MCP is unavailable it reports
health and names the failure on the projects line - but the command fails rather
than reporting success. The printed lines are its output; the exit code is its
result. Exit 1 means the server had a problem, not that the command line did
(that is exit 2).

### seam sessions {#seam_sessions}

```bash
seam sessions [--status STATUS] [<id>]
```

With no positional, lists sessions with name (or short id), project, status, age,
and an ambient marker, under a total/active count. With an id, prints that
session's detail: status, project, tool calls, memory writes and reads, the
read-after-inject ratio, and findings when present. Both read the console's JSON
endpoint.

`--status` is `active`, `completed`, or `expired`; anything else is a parse
error. The console rejects an unrecognized `?status=` too, so a direct URL cannot
silently list everything either.

### seam fav {#seam_fav}

```bash
seam fav <kind> <id> [--off] [--project SLUG]
```

Stars (or with `--off`, unstars) an item via `favorite_set`. `kind` is one of
`memory`, `note`, `project`, `plan`, `task`, `session`, or `trial`; `id` is the
kind's natural identifier - a memory name, note id or slug, project slug, plan
slug, task id, session id or name, or trial id. `--project` scopes memory-name
and note-slug resolution when the ambient scope is not the right one.

Favorites sort first in the console, can be filtered there, and starred
memories are pinned into every session briefing and rank-boosted in recall. A
plan's star lives on its primary note, so a task-only plan cannot be starred.

### seam project isolation {#seam_project_isolation}

```bash
seam project isolation [--yes] <slug> [<state>]
```

Shows or sets a project's [isolation](/concepts/project-isolation/) state.
`state` is one of `open`, `confidential`, or `sealed`; omit it to print the
current state and its promise line. Unlike most subcommands this one talks to
the console's JSON endpoint rather than an MCP tool - there is deliberately no
tool to change isolation, because tightening detaches family and parent links
and is an owner decision.

**Tightening requires `--yes`.** Without it, the command prints exactly the
consequences the console's confirm panel shows - the family and parent it would
detach from - applies nothing, and exits non-zero, so a script cannot mistake an
unconfirmed tighten for a fence that went up. **Loosening is immediate** and
ignores `--yes`.

A project with children refuses to tighten until they are re-parented; the
refusal names them. Promise text is echoed from the server rather than
transcribed here, so the console and the CLI cannot drift apart.

### seam usage {#seam_usage}

```bash
seam usage
```

Activity roll-up from `usage_summary`: active memories broken down by kind, note
count, session and task counts by status, retrieval injections and reads with a
read-after-inject percentage, the most-injected items, the highest-utility
memories by decayed demand, and pending gardener proposals.

### seam doctor {#seam_doctor}

```bash
seam doctor
```

Client-side checks, each reported `ok` or `FAIL`, exiting non-zero if any failed:

1. **server** - `/healthz` reachable and reporting `ok`.
2. **mcp_tools** - `tools/list` returns the tool count this CLI was built to
   expect, minus the tools of any [optional
   feature](/reference/console/#optional-features) that is switched off (the
   line reads `31 registered, 28 exposed (research disabled)`). A mismatch that
   feature state does not explain means the running daemon is a different build.
   When the feature state cannot be read - the daemon is older than optional
   features, or its settings endpoint did not answer - the check judges the
   plausible range instead and names the reason rather than failing a healthy
   daemon.
3. **projects** - `project_list` answers.

This is the client-side view. `seamlessd doctor` checks config, database, and
credentials on the server side; they are different commands answering different
questions.

### seam version {#seam_version}

```bash
seam version
seam -v
seam --version
```

Prints the version of the daemon this CLI is configured to talk to, in the same
form as `seamlessd version`:

```
seamlessd 0.3.8 (commit 6d664d2, built 2026-07-18T09:12:04Z, release build)
```

A daemon from before the distribution was reported leaves `, release build`
off. When the daemon's [update check](/updating/#automatic-update-checks) has
seen a newer release, a second line says so:

```
update available: v0.3.9 (seamlessd update --check on the daemon's machine shows how to update)
```

`seam` carries no version of its own. Both binaries ship from one tag and one
commit, so a number stamped into `seam` could only repeat this one or contradict
it - and only the daemon can report what is actually *running*. An installed CLI
sitting next to a daemon nobody restarted would otherwise report the new version
for a process still serving the old one.

The version therefore comes from the daemon's `/healthz`, and an unreachable
daemon fails the command rather than falling back: there is no second source, and
printing the CLI's own build is the confusion this avoids. `seamlessd version`
accepts the same three spellings and answers from the binary directly, without a
running server.

## MCP bridge and auth helper

```bash
seam mcp-proxy [--config PATH]
seam mcp-headers [--config PATH]
```

These are client-launched helpers, not interactive commands.

- `mcp-proxy` speaks MCP over stdio to its parent and forwards frames to
  Seamless's Streamable HTTP `/api/mcp`, preserving `Mcp-Session-Id` and naming
  the process that launched it in an `X-Seamless-Agent-Process` header - which
  binds an agent's calls to the session its hook opened. The Codex
  installer registers it as Seamless's default policy so the bearer key remains
  in the 0600 Seamless config. Current Codex also supports direct Streamable
  HTTP; the bridge is a key-handling choice, not a Codex transport requirement.
  It rides out a daemon restart: while seamlessd cannot be dialed it retries a
  request for up to 30s, then answers it with a JSON-RPC error. A request that
  may already have reached the daemon is never re-sent - it is answered with an
  error - and the bridge keeps serving either way, since a stdio client does not
  restart a server that exits.
- `mcp-headers` prints the request headers as a JSON object for Claude Code's
  `headersHelper`: the Authorization header, plus this machine's hostname and
  the agent process that launched it (`X-Seamless-Host`,
  `X-Seamless-Agent-Process`). That keeps the key out of `claude mcp add` argv
  and stored client configuration, and binds the agent's calls to its session.

`--config` selects the exact `seamless.yaml` both helpers read. Installers bake
an absolute path into the client registration so it works from every repository.

## Hooks

```bash
seam hook session-start|user-prompt-submit|session-end|stop
seam hook subagent-start|subagent-stop|post-tool-use|permission-request
```

Invoked by Claude Code or Codex, not by hand. Each reads the hook payload from stdin,
forwards it to the matching `/api/hooks/...` endpoint with the bearer key, and
copies the JSON response to stdout - so a `command` hook drives the same server
logic an `http` hook would. Claude Code requires a command hook for SessionStart;
Codex's profile uses command hooks throughout and passes `--client codex` so the
daemon selects its payload adapter.

Two behaviours matter if you are debugging a hook:

- **Failures do not block the session.** A missing config, an unreachable
  daemon, or an unreadable stdin is reported on stderr and exits 0. A daemon
  that cannot be dialed - restarting after an update - is retried first: for up
  to 5s, or 1s on `user-prompt-submit`, `stop` and Codex's `subagent-stop`,
  where losing one to a restart costs a single prompt's recall or a heartbeat
  the next turn repeats. A payload that may already have reached it is never
  sent twice. Whatever the network does - a dial that hangs because the
  server's host drops packets, a daemon that accepts the request and never
  answers - the hook gives up 1s inside the timeout its client gives it: at 4s
  on `user-prompt-submit`, which Codex, and Claude Code on an https install,
  kill at 5s, and at 9s on every other event. An unknown event name or
  present-but-invalid `--client` value is an install/configuration bug and
  exits 1; it never silently becomes Claude Code.
- **`post-tool-use` pre-filters locally.** It fires machine-wide on every
  `Write`/`Edit`, so the CLI drops everything that is not an `ExitPlanMode`
  approval or a write to a file directly under `~/.claude/plans` before loading
  config or touching the network. The daemon re-validates the path anyway.

Install these with `seamlessd install-hooks`.
