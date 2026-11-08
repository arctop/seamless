---
title: seam CLI
description: Every seam subcommand - agent loop, memories and notes, tasks, plans, locked-project access requests, observability, seam call, hooks - plus --json output, the flag-order rules and what each one rejects.
---

`seam` is the headless CLI. It does no work itself: it loads the same
configuration the daemon does, then talks to a running `seamlessd` - over MCP at
`/api/mcp` for most commands, and over the console's JSON endpoints for the
owner-only actions that do not exist as MCP tools (force-releasing a task lock,
approving a plan, deciding a locked project's access request). Both paths authenticate with the static bearer key from
[Configuration](/reference/configuration/), so if the key is wrong or the daemon
is down, every subcommand below fails at the connection.

The target address is derived from the configured bind address, with a bind-all
host (`0.0.0.0`, `::`, or empty) mapped to loopback.

Every read command also takes [`--json`](#json-output), and
[`seam call`](#seam_call) reaches any MCP tool by name, so a script or an agent
with a shell can read everything the MCP surface serves and filter it with
`jq` - through the same project fences, never by reading the data directory.

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

`seam plan approve` parses no flags at all. Commands that take only flags and no
positionals (`ready`, `task list`, `task add`, `plan list`, `status`, `usage`,
`doctor`) are unaffected either way.

## JSON output {#json-output}

Every read command takes `--json`: `recall`, `memory read`, `memory list`,
`note read`, `note list`, `ready`, `task list`, `plan list`, `plan show`,
`status`, `sessions`, `project isolation`, `usage`, and `version`. With it, the
command prints the daemon's own answer - the tool's result, or the console
route's JSON - byte for byte, followed by a newline. It is not re-encoded from
anything the CLI decoded, so a field this `seam` does not know about still
reaches `jq`. Without `--json` every command prints exactly the text it always
has.

```bash
seam recall lease grace --limit 50 --json | jq -r '.hits[] | "\(.kind)\t\(.name)"'
seam task list --status open --json | jq '.tasks | length'
seam sessions --status active --json | jq -r '.sessions[].name'
```

`--json` changes the rendering and nothing else: the same tool is called with
the same arguments, the same fences apply, the server's limits (recall's 100-hit
cap and token budget among them) are the MCP surface's, and the exit status is
the command's usual one - a tool error still exits 1 with nothing on stdout. An
answer that is not JSON is refused rather than passed on, so a pipe never
receives something other than `seamlessd`'s answer. What a CLI gains over an MCP
client is only the pipe: an MCP client caps how much of a result its agent sees,
and a shell does not.

Three commands assemble or filter their answer, and say so:

- `seam status --json` prints the two answers it is made of, each as received:
  `{"healthz": <GET /healthz>, "project_list": <the project_list result>}`. A half
  that could not be read is `null`, and the command still exits 1 naming it.
- `seam plan list --project` filters the console's rows on the client, so it
  cannot be combined with `--json` (which prints the console's answer
  untouched). The pair is refused with the equivalent filter:
  `seam plan list --json | jq '.rows[] | select(.project == "SLUG")'`.
- `seam ready --json` prints `tasks_ready`'s answer, which always carries both
  the `ready` and the `blocked` lists; `--blocked` adds nothing to it.

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
seam recall QUERY [--scope SCOPE] [--project P] [--limit N] [--json]
```

Calls `recall`, the single RRF-fused search entry point. `--scope` is `all`,
`memories`, `notes`, `tasks`, `trials`, or `sessions`; it defaults to `all` and
limit to `10`. Query words are joined with spaces, and flags may sit on
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

## Memories and notes

Read verbs for the knowledge the agent loop writes, as thin clients of the same
MCP tools an agent calls: the same project fences, the same limits. Each takes
[`--json`](#json-output). A reference is either a name or slug positional or
`--id`, never both; `--project` scopes a name or slug lookup and is refused
beside `--id`, which reads by ULID from any scope the caller may read. These
refusals happen before connecting and exit 1.

### seam memory read {#seam_memory_read}

```bash
seam memory read [--project P] [--json] <name>
seam memory read --id ID [--json]
```

Calls `memory_read`. A name resolves in the project (`--project`, else the
session's) and falls back to a global memory of the same name. Prints the name,
kind and project, then the id, description, tags, model, a `provenance:
written by account <name>` line for a memory a restricted local account wrote,
and - for a superseded memory, which still reads - a warning naming its
replacement, then the body. Empty fields print no line.

### seam memory list {#seam_memory_list}

```bash
seam memory list --kind KIND [--project P] [--limit N] [--json]
seam memory list [--kind KIND] [--project P] [--limit N] [--json] WORD...
```

Calls `recall` scoped to memories. `--kind` with no words lists that kind newest
first (recall's browse mode); words search memories, within `--kind` when it is
given. One of the two is required - there is no unfiltered listing. The project's
memories and global ones are both included. Each hit prints its name, project
and age, then its description; nothing found prints `(no memories)`. The limits
are recall's: at most 100 hits (`--limit`, default 10) inside the server's
recall token budget.

### seam note read {#seam_note_read}

```bash
seam note read [--project P] [--json] <slug>
seam note read --id ID [--json]
```

Calls `notes_read`. A slug resolves in the project (`--project`, else the
session's) and falls back to a global note of the same slug. Prints the slug and
project, then the title, id, description, tags, source URL and model, then the
body.

### seam note list {#seam_note_list}

```bash
seam note list [--project P] [--limit N] [--json] WORD...
```

Calls `recall` with scope `notes`. No tool lists notes without a query -
recall's browse mode is for memory kinds - so this takes words, as `recall`
does, and returns the best matches first: slug, title, project and age, then the
description. Nothing found prints `(no notes)`. The limits are recall's.

## Tasks

The queue side of the CLI. See [Tasks](/reference/mcp/tasks/) for what ready,
blocked, and claimed mean.

### seam ready {#seam_ready}

```bash
seam ready [--project P] [--blocked] [--plan S] [--json]
```

Calls `tasks_ready` and lists the actionable queue by short id and title.
`--blocked` additionally lists blocked tasks, each followed by its blockers and
their statuses; a blocker in a project this connection may not read prints the
server's one-line notice in its place. `--plan S` shows that plan's step tasks
instead of the default queue - plan steps are excluded from the default view.
Prints
`ready: (nothing actionable)` when the queue is empty.

### seam task list {#seam_task_list}

```bash
seam task list [--id ID] [--project P] [--status S] [--plan S] [--json]
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
seam plan list [--project SLUG] [--window WINDOW] [--json]
```

Lists captured plans with slug, status, title, project, iteration, agent count,
task progress, and age. `--window` is `24h`, `7d`, `30d`, or `all` (default
`all`) and is applied by the server; anything else is a parse error rather than a
silent fall back to the server's `24h` default. `--project` filters the returned
rows client-side, which is why it cannot be combined with `--json`
([see above](#json-output)).
Prints `(no captured plans)` when nothing matches.

`plan list` takes no positional: `seam plan list <slug>` is an error pointing at
`seam plan show`, not a listing of every plan.

### seam plan show {#seam_plan_show}

```bash
seam plan show [--json] <slug>
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

## Locked projects

Under `peer_auth`, a project listed in `peer_auth.locked` (see the
[configuration reference](/reference/configuration/#peer-auth)) is closed to
every owner request - the owner's own agents included - until the owner
approves a lease for the asking agent's process. A refused call files a
pending access request by itself; an agent can also ask explicitly. The owner
decides it in their own terminal with `seam approve`, on the console's passkey
page, or - when the agent's client supports MCP elicitation - in an approval
dialog inside the agent's session, confirmed with Touch ID (`seam touchid
enroll`) or a passkey. These commands talk to the console's `/console/access`
routes, which answer `no project is locked` while the daemon locks nothing.

### seam access request {#seam_access_request}

```bash
seam access request [--json] [--reason TEXT] <project>
```

Files an access request for a locked project, naming the agent that runs it:
run it from the agent's own shell, where `seam` names the agent process that
started it (the same identity every MCP request carries, see
[which session a command acts in](#which-session)). `--reason` is the agent's
own words on why it needs the project, at most 200 characters; the owner sees
it quoted and labeled as the agent's, never as a fact. A longer reason is
refused before anything is sent.

```text
Requested R-7F3K for example-finance (request). The owner approves it with: seam approve R-7F3K
or with a passkey: https://seamless.example.org/console/access/01K...
```

The second line appears only when a passkey can approve the request there:
passkeys are set up and at least one is enrolled. Asking again while a request
is pending answers with that same request; a request from an agent that
already holds a lease is a `refresh`. `--json` prints the console's answer as
received ([JSON output](#json-output)). The console's refusal is printed as it
is: the project is not locked, the connection has no agent process the daemon
could verify, or the process asking is not an agent's (a shell, a terminal, a
multiplexer such as tmux, or a login process), so nothing was filed - run it
from the agent's own shell.

### seam approve {#seam_approve}

```bash
seam approve [--for DURATION] [<request>]
seam approve --passkey [<request>]
seam approve --watch
```

The owner's terminal path. It needs a terminal on both stdin and stdout and
refuses otherwise: an agent cannot use it. `<request>` is the short id
(`R-7F3K`) or the full id; without one, `seam approve` lists what is waiting
(short id, project, session, warning count, and when each stops waiting),
takes the only request, or asks which one by number or short id. With nothing
waiting it prints `Nothing is waiting.`

It then shows the request's facts - the daemon's warnings first, most serious first, each on its
own line behind `!`, then the project with its isolation and access, the
asking program with its pid and uid, the session and model, the working
directory, the agent's reason (quoted, and labeled as the agent's own words),
whether this is a first request or a refresh, and until when it waits. Only
the warnings are colored, and not with `NO_COLOR` set or `TERM=dumb`. Every
value is printed with control characters escaped, so nothing an agent wrote
can repaint the terminal.

Next it asks for the duration - `session` (the default: until the asking
session ends, at most `peer_auth.max_lease`), `1h` or `4h` - unless `--for`
names one; `--for` takes only a duration the daemon offers. Last it asks to
confirm:

```text
Approve R-7F3K (example-finance, read-write, until the session ends, at most 8h)? [y/N/d=deny]
```

`y` approves and `d` denies; any other answer prints `Nothing changed.` and
exits 0. Either decision runs [`seamlessd approve`](/reference/cli-seamlessd/#seamlessd_approve)
as the daemon's account, with the account, executable and config the daemon
names in its answer:

```bash
sudo -k -u _seamless env SEAMLESS_CONFIG=<config> <seamlessd> approve <id> --for <duration> --digest <facts digest>
sudo -k -u _seamless env SEAMLESS_CONFIG=<config> <seamlessd> approve <id> --deny
```

`sudo -k` asks for your Touch ID or password every time and leaves no cached
ticket behind; that authentication is the proof, not `seam approve`. The
command runs on your terminal, so the prompt shows there, and `seamlessd`'s
own answer (`Approved R-7F3K: example-finance, read and write, ...`) is what
you read; when it fails, `seam approve` exits 1. The digest is that of the
facts you were shown: when they changed in the meantime, the approval is
refused and you look again.

Before running it, `seam approve` drops sudo's cached ticket and tries
`sudo -n -u <account> true`, which succeeds only under a sudoers rule that asks
for no authentication. When it does, it warns that the approval proves nothing
while the rule stands - any process running as you could run the same command
unasked - and goes ahead. A rule scoped to `seamlessd` alone is not seen by
that probe.

When the daemon's answer does not say how to reach the terminal path (its
account, executable or config file is unknown), the facts are still shown and
the command points at the request's passkey page instead.

`--passkey` opens the request's passkey page (the `page` the daemon names) in
the default browser - `open` on macOS, `xdg-open` on Linux, `rundll32` on
Windows - or prints the address when no browser could be opened. A daemon
that offers no passkey approval names no page, and the command says so.

`--watch` polls the pending list every 3 seconds, prints each new request once
(with its warnings and the `seam approve` line to decide it), and raises a
notification - `osascript` on macOS, a terminal bell elsewhere - whose text is
the short id and project only, never the agent's reason. Ctrl-C ends it. It
decides nothing; it takes no request, `--for` or `--passkey`.

Exit status: 0 when a request was approved or denied, when nothing changed,
and when nothing is waiting; 1 on any error, including a request that is no
longer pending (and 2, as everywhere, for a command line it cannot parse).

### seam touchid enroll {#seam_touchid_enroll}

```bash
seam touchid enroll [--app PATH] [--name NAME]
```

Installs the Touch ID approver, Seamless Approve, and enrolls its key. macOS
only, in the owner's own terminal, with `server_url: unix:///path` naming the
`peer_auth` socket; nothing installs or launches the app unless you run this.

A release build of `seam` fetches its own version's
`seamless-approve_<version>_darwin.zip` and checks its SHA-256 against the
release's `checksums.txt`, which must carry the release workflow's Sigstore
signature for that version, as an update's does; a source build passes
`--app` with a local `.app` or zip. Either way the app must pass `codesign
--verify --strict --deep`, be named `com.arctop.seamless.approve`, and chain
to Apple for the same Developer ID team as `seam` itself; a release asset
must also pass Gatekeeper. When `seam` itself is unsigned (a `go install`),
a release asset is held to the team that signed it, and a local build's team
is shown for you to confirm. It is installed at
`~/Applications/Seamless Approve.app`, replacing an existing copy only after
a y/N.

It then prints the command that mints a one-time enrollment code as the
daemon's account:

```bash
sudo -k -u _seamless env SEAMLESS_CONFIG=<config> <seamlessd> approvers enroll-code touchid
```

and opens the app, which asks for the code in its own dialog, creates its key
(one touch) and registers it. The key lives in the Secure Enclave, in a
keychain group only the signed app can use, and needs your presence - finger,
Apple Watch or login password - for every approval. `seam touchid enroll`
finishes by confirming that the daemon lists the new key. `--name` labels it
(default `Touch ID on <host>`; 1-64 printable characters, not starting with
`-`).

Once enrolled, accepting an access request's dialog with Touch ID opens the
app for that request: it reads the request's facts from the daemon itself,
shows them in the Touch ID prompt, and signs over the request and the
duration you chose. A Touch ID approval is accepted on the `peer_auth` socket
only.

### seam touchid status {#seam_touchid_status}

```bash
seam touchid status
```

Says whether the app is installed and how it is signed, whether `server_url`
names the socket, and which Touch ID keys the daemon has enrolled.

## Restricted accounts' proposals

A [restricted local account](/guides/peer-auth/#restricted-local-accounts)
(`peer_auth.restricted`) cannot change a constraint, convention, protocol or
stage on its own: each such change waits as a **proposal** on the console's
Held screen, out of every briefing, recall and gardener pass, until you promote
it. These commands are yours, over the console's JSON endpoints with the bearer
key; the account has no console, so it cannot run them. A proposal is named by
its full id or a unique prefix of it.

### seam proposals {#seam_proposals}

```bash
seam proposals [--project P] [--json]
```

Lists the proposals waiting for you, newest first: the id, what it changes,
the project and name, "proposed by account <name>", and its age. `--json`
prints `{"proposals": [...]}`, each row as the console sent it. A daemon with
no restricted accounts (and write screening off) keeps no proposals and says
so.

### seam promote {#seam_promote}

```bash
seam promote <proposal>
```

Shows the proposal and the text as an agent would read it, then asks you to
confirm. Yes lands it exactly as written, still labeled "written by account
<name>", and superseding what it replaces when it was proposed that way. It
refuses a write that write screening held (decide that one on the console) and
a proposal whose target changed since it was filed (discard it; the account
writes again). It needs a terminal: run from an agent, with no TTY, it refuses
before reaching the daemon.

### seam proposals discard {#seam_proposals_discard}

```bash
seam proposals discard <proposal>
```

Confirms, then discards the proposal: nothing lands and its text is erased; the
row stays as the record. The same refusals and the same terminal requirement as
`seam promote`.

## Observability

### seam status {#seam_status}

```bash
seam status [--json]
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
(that is exit 2). `--json` prints `/healthz` and `project_list` as one object
under the same rule ([JSON output](#json-output)).

### seam sessions {#seam_sessions}

```bash
seam sessions [--status STATUS] [--json] [<id>]
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
seam project isolation [--yes] [--json] <slug> [<state>]
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

With `--json` the console's answer is printed in place of the text, and the exit
status is unchanged: an unconfirmed tighten still exits non-zero.

A project with children refuses to tighten until they are re-parented; the
refusal names them. Promise text is echoed from the server rather than
transcribed here, so the console and the CLI cannot drift apart.

### seam usage {#seam_usage}

```bash
seam usage [--json]
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
seam version [--json]
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
running server. `--json` prints that `/healthz` answer itself.

## Any MCP tool

### seam call {#seam_call}

```bash
seam call <tool> [<json> | -]
```

Calls one MCP tool by name and prints its result to stdout exactly as the
daemon returned it - a JSON object, ready for `jq`. It dials the way every other
command does: the bearer key (or the [peer_auth](/guides/peer-auth/) socket when
`server_url` names one), plus the `X-Seamless-Host` and
`X-Seamless-Agent-Process` headers, so a call an agent runs through its shell is
[bound to that agent's session](#which-session) like its MCP calls.

```bash
seam call recall '{"query":"lease grace","scope":"notes","limit":50}' | jq -r '.hits[].name'
echo '{"name":"lease-steal-window"}' | seam call memory_read -
seam call session_end '{"findings":"..."}'
```

The arguments are one JSON object, given inline or as `-` to read it from stdin;
none means `{}`. Anything else - an array, a string, `null`, two objects, text
after the object, an empty stdin - is refused before connecting. Numbers travel
as written. Each tool's arguments are in the [MCP tool
reference](/reference/mcp/), and `seam call --help` lists the tool names.

A tool error exits 1 with the tool's own message on stderr and nothing on
stdout. A name the daemon does not serve is refused by the daemon - `seam` does
not gate names itself, since a newer daemon may serve tools the CLI predates -
and an [optional feature](/reference/console/#optional-features) that is off
serves none of its tools. A missing tool name or a second positional is a usage
error (exit 2): quote the JSON object as one argument.

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
seam hook session-taint
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
- **On the peer_auth socket the hook makes the captures.** With `server_url`
  naming the socket the daemon opens none of the agent's files, so `seam hook`
  reads them and sends what it found beside the payload: the transcript's
  findings, token totals and model, the plan file, a subagent's prompt and
  report (waiting up to 3s for its final message), and the git head. A capture
  the daemon refuses (a locked project the owner has not approved) is named in
  one line on stderr and not retried. On any other `server_url` the payload is
  forwarded exactly as the client wrote it.

Install these with `seamlessd install-hooks`. The one exception is
`session-taint`, which no installer wires: an agent guard's own PostToolUse
hook pipes its stdin to it after a tool that read outside text, and the daemon
marks the session for [write screening](/guides/write-screening/#the-outside-text-mark).
It retries and gives up like the others (5s, 9s).
