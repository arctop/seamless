---
title: Troubleshooting
description: Symptom-first fixes for a system whose hooks fail open - where silence, not an error, is what a broken install looks like.
---

Seamless has one property that makes troubleshooting unlike most software:
**hooks fail open**. A stopped daemon, a wrong key, a `seam` binary that moved -
none of these produce an error you or your agent will see. The handler returns
200 with empty context, `seam hook` reports to stderr and exits 0, and work
proceeds as if Seamless were not installed.

So the failure mode is *silence*. Nothing is broken-looking; you just quietly stop
getting briefings. That is a deliberate trade - a memory system must never block
an agent - but it means you cannot wait for an error. You have to go ask.

## Start here, always

```bash
seamlessd doctor   # server side: config, database, credentials, embedder, hooks
seam doctor        # client side: reachable, key accepted, tool count matches
```

They answer different questions and neither subsumes the other.

| | `seamlessd doctor` | `seam doctor` |
|---|---|---|
| Runs | Against config and the database directly - no daemon needed, except on a `role: client` install, where the tool count dials the server | Against a running daemon over HTTP and MCP |
| Checks | `binary`, `config`, `data_dir`, `mcp.api_key`, `llm`, `embedder`, `database`, `mcp_tools`, **`hooks`**, `gardener` | `server` (`/healthz`), `mcp_tools` (`tools/list`), `projects` |
| Fails on | Only a `fail` - warnings are informational | Any failed check |

**The `hooks` check exists only in `seamlessd doctor`.** If your symptom is "no
briefing", `seam doctor` cannot tell you why - it will happily report a healthy
server that no hook is calling. Run both.

`seamlessd doctor`'s `embedder` check makes a **real embed call**, which is the
one way to learn that recall has quietly gone keyword-only.

---

## No `<seam-briefing>` appears at session start

**What is happening.** Something in the chain - hook installed, `seam` on disk,
daemon up, key valid - is broken, and every link in it fails silently by design.

**Fix.** Walk the chain in order:

1. `seamlessd doctor` → read the client-specific definition line. Claude Code
   checks `~/.claude/settings.json`, `./.claude/settings.json`, and Claude
   Code's [managed settings](/reference/cli-seamlessd/#seamlessd_install_hooks_managed);
   Codex
   names exact `current`, `stale`, and `missing` events in its hooks file. A
   warning does **not** fail the run, so read it rather than trusting the exit
   code. Repair owned drift with `seamlessd install-hooks`.
2. **Codex only:** read `codex hook trust`. `trust unverified` is honest, not a
   failed probe - open `/hooks`, inspect the current commands, and approve them.
   A recent activity timestamp is supporting evidence only; it cannot prove a
   newly changed definition is trusted.
3. `seam doctor` → is the daemon actually up and is the key accepted? An empty
   `mcp.api_key` makes the daemon reject every MCP and hook request while looking
   perfectly healthy on `/healthz`.
4. Did the `seam` binary move? Command hooks bake in an **absolute path**, and
   `make install` points them at `~/.local/bin/seam` - a stable copy, not your
   working tree. If you moved the install prefix or wired the hooks up by hand
   against some other path, re-run `make install`.
5. Check the hook's **type** if you hand-edited settings.json. Claude Code
   silently ignores an `http` hook for `SessionStart` - it must be a `command`
   hook. Codex's Seamless profile uses command handlers for all five events.

Definition classification is stricter than “contains `hook <event>`.” A current
entry must match the desired event, paths, arguments, client discriminator,
timeout, and OS command shape. The installer can adopt a marker-free entry only
when it is unmistakably a documented Seamless URL or `seam`/`seam.exe` command;
foreign entries survive install and uninstall.

## Codex hooks are current, but doctor says `trust unverified`

**What is happening.** Definition validity and trust are different facts. Codex
does not expose a supported command that tells Seamless whether the current hook
hash is trusted, and Seamless deliberately does not parse or pre-seed private
trust state.

**Fix.** Open `/hooks` inside Codex. Review and approve the current Seamless
definitions. This warning remains honest even after approval; use a real session
briefing as the end-to-end check. `codex hook activity` can tell you when
SessionStart/UserPromptSubmit last reached the daemon, but it is not proof for a
definition that may have changed since then.

## Codex MCP is stale or incompatible

**What is happening.** `seamlessd doctor` compares `codex mcp get seamless
--json` with the exact enabled stdio bridge the installer wants, including the
absolute `seam` path, ordered `mcp-proxy --config ...` arguments, and target-file
existence. Mere registration is not enough.

**Fix.** Run `seamlessd install-hooks --client codex`. A disabled or drifted
owned stdio bridge is replaced in place and verified. A direct-HTTP or arbitrary
foreign entry under the reserved `seamless` name is not overwritten: either keep
it and use `--mcp=false` for future hook/skill refreshes, or explicitly run
`codex mcp remove seamless` before reinstalling the maintained proxy.

## The briefing appears twice

**What is happening.** Two copies of the Seamless hooks are installed, and
Claude Code runs both. The usual cause is Claude Code's
[managed settings](/reference/cli-seamlessd/#seamlessd_install_hooks_managed):
a file an administrator or a device-management tool deploys, whose hooks run
alongside the ones in `~/.claude/settings.json`.

**Fix.** `seamlessd doctor`'s `hooks` line names both files: `runs twice:
<events> also in <file>`. Run `seamlessd install-hooks --client claude`, which
leaves those events to the managed settings and removes the copies from your
settings file. If doctor instead calls a managed definition stale or not
runnable, the managed file is the one to fix. It is not yours to edit, so ask
whoever deployed it.

## A briefing appears, but it is nearly empty or names the wrong project

**What is happening.** The hook fired and the daemon answered - this is not a
plumbing problem. The cwd resolved to a project you did not expect, or to none.

**Fix.** The [precedence chain](/concepts/projects/) resolves scope from the
working directory via the repo map. Three outcomes:

- **Wrong project** - the cwd matched a mapping you forgot about. Check
  `/console/projects`, and re-map with `seamlessd map-repo --path <dir> --project
  <slug>`.
- **A project named after your directory** - an unmapped *git* repo
  auto-registers a project named after the repository root. It is real, it is
  just new and therefore empty.
- **A `<slug>-2` project after moving a repo on disk** - the repo's old path
  still owned the slug, so registration minted a fresh project and the
  briefing lost the original's memories. Current daemons adopt the existing
  project automatically when the old path is gone (a `repo.moved` event lands
  in the console feed); if a split already happened, point the path back with
  `seamlessd map-repo --path <new-root> --project <slug>`. `seamlessd doctor`
  lists mapped paths that no longer exist on disk, and
  `seamlessd unmap-repo --stale` removes them. Once the leftover `<slug>-2`
  is empty, `seamlessd retire-project --project <slug>-2` marks it retired.
- **No project at all** - the cwd is not inside a git repo, so nothing resolved
  and the session is global.

An empty briefing for a project that genuinely has no constraints and no memories
is correct behavior, not a fault. The header counts what exists; if it says zero,
the store is telling the truth.

## A call is rejected as ambiguous scope, session, or agent

**What is happening.** This is the [fail-closed rule](/concepts/projects/) doing
its job. A call on a connection bound to no session has to infer its project or
session from the agents that are live, and it refuses rather than guesses when
that inference has no single answer. A durable write with no resolvable scope is
rejected rather than landing in the global scope, because a global memory is seen
by every agent in every repo forever - and that must never be what happens when
the system is *unsure*.

**From Claude Code or Codex, suspect the `seam` binary first.** Their tool calls
are [bound to the agent's own session](/concepts/sessions/#process-binding)
automatically, so they should rarely see these errors. A `seam` older than that
binding sends no agent-process identity, which leaves the connection unbound -
and the briefing's `Seam session` line then tells the agent to
`bind once with session_start name=...` instead of saying no `session_start` is
needed. [Update](/updating/) Seamless on the agent's machine and start a new
session. An agent that has ended its own session is unbound too, by design: its
next bare call - a read as much as a write - is refused rather than borrowing
another agent's project or quietly falling back to the global scope.

**Fix.** The message tells you which case you are in:

| Message says | Cause | Fix |
|---|---|---|
| *no bound or ambient session to infer the project from* | Nothing bound, and no ambient session this caller may inherit | Bind with `session_start` (`cwd`, or `project=<slug>`), or pass `project=<slug>` on the write |
| *the active ones belong to other agents* | This connection names its agent process but owns no live session - an agent that ended its own session, or `seam` run by hand - and every live session is another agent's | Pass `project=<slug>` (`--project` on the CLI); an agent that ended its own session can resume it with `session_start name=` the `cc/...` or `cx/...` on its briefing's `Seam session` line |
| *active ambient sessions span multiple projects* | You are unbound and other agents are live in several repos, so inheriting would bleed your write into someone else's project | Pass `project=<slug>`, or bind once with `session_start` (`name=` the `cc/...` or `cx/...` on your briefing's `Seam session` line, or `project=<slug>`) |
| *ambiguous session* or *ambiguous agent* | `session_update`, `session_end`, or a task claim or release named no session while several agents are live | Pass `session=` with the `cc/...` or `cx/...` on your briefing's `Seam session` line, or bind once with `session_start name=<that name>` |

In the scope cases, `project=<slug>` may name a project that does not exist yet -
the write creates it. Do not answer this error with `project=global` because a new
slug felt riskier: an invented project is cheap and local, while global puts the
item in every project's briefing forever. The error text lists the slugs that do
exist, so you can match one instead of coining a near-duplicate.

If a call that worked all morning starts failing this way on a connection bound
with `session_start`, suspect a **lost binding** - see the daemon-restart section
below. `project: global` is always accepted; it is a token you pass on purpose.

## Recall returns junk, or misses what you just wrote

**What is happening.** Three unrelated causes wear the same symptom.

**Recall has degraded to keyword-only.** If the embedding provider is unreachable,
rate-limited, or rejecting the key, recall **degrades rather than errors** - you
get worse ranking, not a failure, because a partial answer beats no answer during
a network incident. That is correct in the moment and terrible for three weeks.
Run `seamlessd doctor` and read the `embedder` line: it probes with a real call.
Note that provider `anthropic` has no embeddings API at all, so selecting it means
permanently lexical recall.

**The memory is not written to be findable.** The `description` is the retrieval
surface - the only text indexes show, and the *only* text the prompt-injection
matcher scores (name and description; never the body). A description like "notes
about the console" cannot be retrieved by anything. See [Write memories that get
recalled](/guides/write-good-memories/).

**You wrote it seconds ago.** The prompt matcher's corpus is rebuilt on a 30-second
interval, and an expired lookup serves the *stale* corpus while the rebuild runs
behind the hook - so the hook never pays for a cold rebuild, and a brand-new
memory can miss the next prompt's injection by a little more than the interval
suggests. An explicit `recall` call does not use that corpus and sees the write
immediately.

Before searching at all: **read what was already injected.** The briefing is in
context. Re-recalling it spends tokens to learn what the agent was already told.

## A task is stuck `in_progress` and nobody is working it

**What is happening.** Its holder died, and you are between two clocks. An expired
lease makes a task **stealable by id** - but it does *not* re-queue it. The task
is still `in_progress`, and `tasks_ready` returns only `open` tasks, so nothing
surfaces it. Lease expiry is enforced lazily inside `tasks_claim`; there is no
sweeper watching leases.

What *does* re-queue it is the session reaper: it expires sessions idle past
`gardener.session_idle_minutes` (default 45) and releases their claims back to
`open`.

**Fix.**

1. Confirm the diagnosis: `seam task list --status in_progress`, or
   `/console/tasks` - a task's detail panel shows its holder and whether the lease
   is **live** or **expired**.
2. **Check `gardener.enabled`.** The reaper runs inside the gardener's pass. With
   the gardener off, nothing reaps idle sessions and nothing ever returns a dead
   agent's claims - they stay stuck forever. `seamlessd doctor` warns when the
   gardener is disabled.
3. Do not wait if you do not want to: `seam task release --force <id>`, or the
   **release lock** button in the console. Both force-release any holder
   regardless of the lease. Neither is on the MCP surface - agents get the
   cooperative protocol, you get the override.

## `tasks_update` says the task is already claimed - by my own session

**What is happening.** The connection lost its `session_start` binding. The task
is genuinely held by your session id; your *connection* no longer knows that, so
the holder check sees a stranger.

**Fix.** Re-run `session_start` with the **same name** to rebind. It resumes the
session rather than opening a second one.

The binding is keyed by the transport's `Mcp-Session-Id`, held in the daemon's
memory. Any daemon restart drops it - see below. A Claude Code or Codex agent's
automatic binding is not held there - it is matched from the agent process on
every request - so this bites only a connection bound with `session_start`.

## A `seam` flag did nothing

**This no longer happens.** A trailing flag was once dropped in silence - `seam
task release 01K7ABCD --force` took the normal holder-checked path and the
override never happened, with no error and no warning. Both halves of that are
gone:

```bash
seam task release --force 01K7ABCD    # --force applies
seam task release 01K7ABCD --force    # --force applies here too
```

Every `seam` command parses flags and positionals in any order. A typo'd flag
(`--projct`) is an error everywhere rather than being absorbed into the
positionals, and an unrecognized enum value (`--status bogus`) is rejected at
parse time rather than quietly widening the filter.

**If a flag still looks ignored**, it is not a flag-order problem. Check that you
are running the `seam` you think you are (`seam doctor`), and see the [seam CLI
reference](/reference/cli-seam/) for what each command accepts.

## A briefing setting in the YAML is ignored

**What is happening.** The `briefing:` block has a fourth precedence layer above
file and environment: a **runtime override stored in the database**, written by the
console's Settings → Briefing section. It wins over both, applies from the
next session start without a restart, and stays until reset.

**Fix.** Check the console before you check the YAML. This is the one place the
config file is not the last word, and it exists so you can tune what agents get
injected while they are running. See
[Configuration](/reference/configuration/).

## A tool is missing, or a console screen says it is switched off

**What is happening.** It belongs to an
[optional feature](/reference/console/#optional-features), and optional features
ship **off**. Nothing is broken and nothing was deleted - the feature's data is
untouched, and every surface returns the moment it is switched back on. Tools
and whole screens belong to research (the Labs and Trials screens, the trials
search scope, and `lab_open`, `trial_record`, `trial_query`); the in-page
momentum surfaces (finish-line cards, the capture calendar, payoff moments,
maturity stages) belong to momentum; the Now screen's arcade (the day tape,
personal records, the hot-streak pulse, celebration moments) belongs to
gamification - the Now screen itself is core and always on.

**Fix.** Turn the feature on in the console under Settings → Features (or set
its `features:` key in YAML, or `SEAMLESS_FEATURES_RESEARCH=1` /
`SEAMLESS_FEATURES_MOMENTUM=1` / `SEAMLESS_FEATURES_GAMIFICATION=1`). The
console toggle stores an override that beats both the file and the
environment, so check it before you edit YAML.

**If you already turned it on and the agent still cannot call the tool**, you
are looking at the propagation seam, not a failure:

| Surface | When the change lands |
|---|---|
| The console | Immediately. |
| A tool call | Immediately - and while a feature is off its tools are refused as *unknown*, which is what a client holding a stale list should see. |
| A client's tool list | The next time it lists tools, in practice its next session. Seamless declares `listChanged: false` and sends no tool-list notification, so a connected client keeps the list it was given. Start a new session rather than hunting for a refresh button. |
| The `seam-research` skill in a client's skill home | The next `seamlessd install-hooks` run. `seamlessd doctor` raises an **info** line while a skill for a disabled feature is still installed - it never deletes in a client's directory behind your back. |

One thing that is *not* a symptom of this: a gated tool call leaves no row in
the Interactions feed. The filter rejects it before the middleware that records
tool calls, so silence there is expected.

## A console screen is not in the sidebar

**What is happening.** If a link still opens it - with a note that it is not in
your sidebar - it is the console's
[experience level](/reference/console/#choose-how-much-you-see), not a switched-off
feature. A fresh installation starts at Basic, which keeps the sidebar to the
essentials. Levels only change what the console shows: your agents get the same
tools and briefings at every level.

**Fix.** Choose a level in the console under Settings → Experience (the note's
**Switch** button does it in one click), or set `console.level` in YAML /
`SEAMLESS_CONSOLE_LEVEL`. A choice made in the console wins over both until you
reset it there.

## Two daemons, the wrong port, or code changes that never land

**What is happening.** Seamless is **one server instance per machine**: one port
(8081), one data directory (`~/.seamless`). The dev and release install layouts
drive that same instance, so installing one replaces the other. (A `role: client`
install has neither a port nor a data directory, so it cannot collide this way -
see [Share one daemon across your computers](/guides/network-install/).) Most confusion here is
someone accidentally interacting with a second copy, or with the same copy running
older code.

**Fix.**

- **Compare versions.** The same version string appears in `/healthz`, the MCP
  handshake, and the startup log line. `seam status` prints health and version.
  If it disagrees with what you just built, the daemon is running older code.
- **A rebuild does not restart anything.** `make build` and `make check` rewrite
  `bin/seamlessd`, but a running process keeps its in-memory image. The new binary
  takes effect on the next restart - which also means an *accidental* restart
  silently upgrades the running daemon to whatever is in your working tree.
- **Never `pkill -f "seamlessd serve"`.** Running a throwaway daemon on a spare
  port is the right way to test in isolation, but that pattern matches the real
  service's command line too and kills it. Under launchd it comes back in about a
  second with a new pid and no data loss - but **every agent's MCP connection
  drops, and bindings made with `session_start` are lost mid-task**, which
  resurfaces as the ambiguous-scope and self-claimed-task symptoms above (a
  Claude Code or Codex agent's automatic binding survives). Kill the throwaway by
  its own pid, or by its port.
- **Tools missing in some repos?** That is not a daemon problem. `claude mcp add`
  defaults to `local` scope, tying the registration to the directory you ran it
  from. Register with `--scope user`.
- **Codex tools missing?** Run `codex mcp get seamless --json`, then
  `seamlessd doctor`. The maintained registration is an enabled user-level stdio
  bridge; a disabled entry, stale binary/config path, or project override is a
  client-configuration problem even while `/healthz` is green.

## The daemon will not start: `[::1]:8081 is in use by another process` {#paired-address-in-use}

**What is happening.** Beside `127.0.0.1:8081` the daemon listens on
`[::1]:8081`, and on a Server beside its IPv4 address on that interface's IPv6
addresses. Another process holds one of them, and a browser or client that tries
that address first would reach it instead of the daemon, so `serve` refuses to
start (for `[::1]`, for an address the `server_url` or passkey origin's host
resolves to, and under peer_auth for any). The service starts it again every few
seconds and the log repeats the line; `seamlessd doctor`'s `bind.addresses` row
is a **fail** naming the address.

**Fix.** Find the process: `sudo lsof -nP -iTCP:8081 -sTCP:LISTEN` (Windows:
`netstat -ano | findstr :8081`). Stop it, and the daemon starts at the service's
next attempt (on Windows the Scheduled Task retries three times, a minute apart;
after that, `seamlessd start`). If a program of yours needs the port, move
Seamless instead (`addr`, and `server_url`). A process you do not recognize,
under another account, is what this refusal exists for: find out what it is
first.

## An agent on another computer gets no briefing {#an-agent-on-another-computer-gets-no-briefing}

**What is happening.** A [Client](/concepts/networked/) keeps no data and
answers nothing from memory. When the Server, or the relay in front of it, does
not answer, every hook fails open and every tool call fails, so the session
looks like one without Seamless.

**Fix.** On the Client, run `seam doctor`, then `seamlessd doctor`. On a key
client, the `server_url` row is a **fail** until the Server answers. On a
computer with a relay, the rows that start with `relay` name what is wrong; run
doctor as the relay's account to see its pairing too
([how](/guides/network-install/#install-a-relay)). Through a relay, the failing
call or `seam` command carries the reason:

| The answer | What it means | Fix |
|---|---|---|
| `seamless relay: the server at <address> could not be reached (...)` | the relay runs, and the Server does not answer it | start the Server, or mend the route to it: the relay forwards again as soon as it answers |
| `seamless relay: the server at <address> presented a certificate other than the one this relay pinned at pairing` | the Server's certificate changed, most often at a renewal | [pair the relay again](/guides/network-install/#revoke-a-computer): revoke it on the Devices page, get a new code, run `sudo -u _seamless /opt/seamless/bin/seamlessd relay pair --replace --server <address> --fingerprint <the new fingerprint> --code -`, and restart the relay |
| `seamless relay: the server at <address> presented a certificate that does not verify` | the relay does not trust the certificate's issuer | pair again with `--ca-file` naming its root |
| `unauthorized: this relay's pairing was revoked: pair it again` | you revoked it | pair it again, with a new code |
| `forbidden: relay <name> is paired, but peer_auth.relays does not map it` | the Server runs peer_auth and refuses a relay you have not mapped | map it in the daemon's config, by the name the Devices page shows, then restart the daemon ([how](/guides/peer-auth/#agents-on-another-computer-relays)) |
| `forbidden: relay <name> was paired with a one-time code alone, before peer_auth` | the Server maps it in `peer_auth.relays`, and no passkey ever approved it | revoke it, pair it again, and approve it with your passkey |
| `forbidden: uid <n> on relay <name> is neither the owner nor a restricted account peer_auth.relays maps there` | another account on that computer, through a relay the Server maps | run the agent as your own account there, or list that account under the relay's `restricted` |
| `seamless relay: the caller runs as the relay's own account` | an agent runs as `_seamless`, the account that holds the relay's key | run agents as your own account |

With no answer at all, nothing serves the socket: `relay.socket` says the relay
is not running. Start its service.

## Pairing a computer fails

**What is happening.** Setup, or `seamlessd relay pair` in the relay's root
step, stops with the reason, and nothing is saved until the Server has paired
the computer.

**Fix.** Setup's own refusals come first:

| Setup says | Fix |
|---|---|
| `<address> presents <fingerprint>, not the SEAMLESS_SERVER_FINGERPRINT you gave: nothing was sent to it` | the address reaches a computer whose certificate is not the one the Server printed: check the address, and the fingerprint `seamlessd devices code` prints on the Server |
| `this computer does not trust <address>'s certificate (...), and nothing compared it` | add `SEAMLESS_SERVER_FINGERPRINT=<the fingerprint the Server printed>` to the one-liner, or run the installer at a terminal and compare it there |
| `<address> is plain http, and a pairing crosses the network` | give the Server TLS ([setup's address step](/guides/network-install/#set-up-the-server)), then pair with its https address |
| `<config> is here already, and setup never edits a config` | a key device's key goes into a config of its own: move the old one aside, or keep it |
| `SEAMLESS_PAIR_CODE and SEAMLESS_MCP_API_KEY are both set` | a code gives the computer a key of its own: drop the key |

Then the Server's, from either:

| The error | Fix |
|---|---|
| `the server refused the pairing code: it may be mistyped, expired or already used` | get a new code: each works once, for 10 minutes |
| `the server already pairs a device of this name` | revoke the old device of that name on the Devices page, or pair with `--name` |
| `the server's certificate does not have the fingerprint you compared; nothing was sent` | the address reaches a computer whose certificate is not the one the Server shows: check the address, and the fingerprint `seamlessd devices code` prints on the Server |
| `a relay pairs over TLS with its client certificate: the server's address must be https` | a relay needs TLS on the daemon's own listener: give the Server a certificate ([setup's address step](/guides/network-install/#set-up-the-server)) |
| `pairing with a server on another computer needs https: over plain http the code and this computer's key would cross the network in the clear` | the Server has no TLS, and the pairing crossed the network (the Server saw it arrive from another computer, a forwarded port on this one included): give the Server TLS ([setup's address step](/guides/network-install/#set-up-the-server)), then pair with its https address |
| `the owner denied this pairing on the server`, `this pairing was not approved in time: get a new code and pair again` | under peer_auth the pairing waits 10 minutes for your passkey on the Devices page: pair again with a new code, and approve it there |
| `this relay is already paired with <address> as <name> (device <id>)` | revoke that device on the Devices page, then pair with `--replace` |
| `the relay runs as root`, `the relay runs as uid <n>, which is not a system account` | run it as its own account, created and pinned as [Deploy it](/guides/peer-auth/#deploy-it-the-daemon-runs-as-its-own-account) does |

On the Server, `seamlessd devices` refusing with `this install serves no other
computers, so it pairs none` means pairing is off: it is on only on a Server
with TLS, a bind beyond loopback, or a `server_url` naming another host. `devices code` refusing with `pairing needs TLS here` means its address is plain http to another host, which no computer can pair over: give it TLS first. Refusing with `server_url names a user` means `server_url` has `user:password@` in it, which Seamless never uses: leave `scheme://host:port` and restart the daemon.

## An agent on another computer cannot open a locked project

**What is happening.** A locked project opens only to an agent whose process
tree holds a lease, and the Server can name that tree only for its own socket
and for a relay `peer_auth.relays` maps. So it depends on how the agent's
computer reaches the Server:

- **A key device, a client of the Server's own key, or a Windows computer**
  never holds a lease: a key cannot be tied to one process.
- **A relay the Server does not map** is refused outright.
- **A mapped relay of an older release** files nothing: a call refused at the
  lock asks for nothing, and `access_request` or `seam access request` answers
  `no access request is filed for this agent on another computer: its relay
  did not say which program is asking (a relay of wire version 1 never does)`.
- **A process that is not an agent's** - `seam` run by hand in a shell there -
  is refused as it is on the Server ([requests name agents](/guides/peer-auth/#requests)).

**Fix.** Pair the computer as a relay and map it on the Server
([how](/guides/network-install/#locked-projects-and-agents-on-other-computers)),
update Seamless on a relay's computer whose relay is older than the Server's,
and ask from an agent session there. Otherwise run the agent on the Server's
computer, on its socket: a terminal there, or SSH to it.

## `seamlessd update` refuses the release with a signature error

**What is happening.** In v0.6.0 Seamless moved from github.com/0spoon/seamless
to github.com/arctop/seamless, and every release since is signed by the new
repository's release workflow. A binary older than v0.5.4 trusts only the old
repository's signature, so its `seamlessd update` rejects every release after
the move. It stops with `refusing to run ...: sigstore verification: ...`,
and the error names `github.com/arctop/seamless`. Nothing is installed or
changed. v0.5.4 and later accept both signatures, so this happens at most once.

**Fix.** Re-run the installer one-liner once. It installs the latest release
with its own signature check and keeps your config and `~/.seamless`:

```bash
curl -fsSL https://thereisnospoon.org/install | sh    # macOS, Linux
```

```powershell
irm https://thereisnospoon.org/install.ps1 | iex      # Windows
```

After that, `seamlessd update` works again (`seamlessd version` shows what you
have). Installed with Homebrew from the old `0spoon/tap`? That install does not
use `seamlessd update`; switch taps once as
[Install & deploy](/install/#homebrew) describes.

## The update check keeps failing

**What is happening.** The daemon's [automatic update check](/updating/#automatic-update-checks)
could not read GitHub's release list. `seamlessd doctor`'s `updates` row and
**Settings -> Updates** show the error and how long it has lasted; after a week
the row turns into a warning. A failed check retries after 15 minutes, doubling
up to the check interval, and a GitHub rate limit (60 anonymous requests an hour
per address, shared by everything behind your router) is waited out.

**Fix.** Usually the network: the daemon reaches `api.github.com` directly
unless its own environment carries `HTTPS_PROXY`. A proxy exported in your shell
does not reach a daemon the service manager starts; see
[The service & where things live](/reference/service/). If the machine should
not talk to GitHub at all, set `update.check: false` - then nothing is sent and
nothing is reported. `seamlessd update --check` asks directly, so it shows the
same error with its cause.

## The update notice will not go away

**What is happening.** A newer release is out that this install will not install
by itself, and the daemon says so in new sessions' briefings for a week after it
first saw it, in the console, and in `seamlessd doctor`. Either the install is
only ever told - Homebrew, a build from source, a client machine, or a layout the
daemon cannot vouch for ([who updates itself](/updating/#who-updates-itself)) -
or its automatic updates are off, paused, or skip that release.

**Fix.** Update with the command the notice names - it depends on how Seamless
was installed (`seamlessd update`, `brew upgrade --cask arctop/tap/seamless`, or
`git pull && make install`); the table in
[automatic update checks](/updating/#automatic-update-checks) lists them. On an
install that updates itself, **Settings -> Updates** says why it skips the
release, and **Update now** installs it. To stop hearing about updates instead,
turn checks off in **Settings -> Updates** or set `update.check: false`.

## An automatic update is waiting {#an-automatic-update-is-waiting}

**What is happening.** This install updates itself, a newer release is out, and
it has not been installed yet. Usually that is the design working:
[automatic updates](/updating/#when-it-installs) wait out a soak, then wait for a
quiet daemon. **Settings -> Updates** says what this one waits for on its
**Next update** row (and on the Home health strip's hover);
`seamlessd update --check` prints the release and its deadline on its `target`
and `pending` rows.

| Next update says | Why | What to do |
|---|---|---|
| `installs by itself once it is 24h old` | The release is younger than `update.min_age`. | Wait, or use **Update now**. |
| `waiting for 2 live agent sessions to go idle, or at a lull in requests in 19h` | Agents are working. Past `update.max_defer` it takes the next lull instead. | Nothing. |
| `past its 24h deadline; waiting for 90s without requests` | Something keeps calling the daemon. | Nothing: at 1.5 times `max_defer` it installs once no request is in flight. An open **Interactions** page refreshes every 15 seconds, so closing it lets the lull come sooner. |
| `waiting for 1 request in flight to finish` | A request is being served. | Nothing: at twice `max_defer` it installs anyway. |
| `an earlier attempt failed; trying again in 1h` | The backoff after a failed attempt. | Read **Last attempt**; **Update now** does not wait for the backoff. |
| `an updater is already running` | A `seamlessd update` holds the update lock. | Let it finish. |
| `re-checking the release list in 12m before installing v0.7.3`, or `could not be re-checked` | Its copy of the release list is older than a minute, and it re-checks at most every 15 minutes. | Nothing; if the check keeps failing, see [the update check keeps failing](#the-update-check-keeps-failing). |
| `live agent sessions could not be counted` | The database did not answer the count, which never counts as idle. | Run `seamlessd doctor`. |
| `the update state could not be saved` | `~/.seamless/update/` cannot be written - a full disk, or permissions. | Free space or fix the permissions; an update never starts without its record on disk. |
| `v0.7.3 carries no signed checksums bundle` | Automatic updates verify that bundle, and this release has none. | Update by hand with `seamlessd update`. |

No **Next update** row at all means nothing is pending: the release sits under
**Held back** (you went back to an older release) or **Skipped releases** (an
update to it failed), or the **Mode** row says automatic updates are off or
paused. [Automatic updates](/updating/#automatic-updates) covers each.

## An automatic update failed {#an-automatic-update-failed}

**What is happening.** An update the daemon started - by itself, or through
**Update now** - did not apply. For a week every console page says so in a
banner ("The update to v0.7.3 rolled back."), and three places read the same
record:

- **Settings -> Updates -> Last attempt**: the outcome, the updater's own error,
  and its log;
- `seamlessd doctor`, on its `updates` row;
- `seamlessd update --check`, on its `last`, `error`, `log` and `backup` rows.

The log, `~/.seamless/update/logs/<attempt>.log`, has every step and the
installer's full output; the newest 20 logs are kept. The outcome says how bad
it is:

- **rolled back**, **failed** or **interrupted**: Seamless still runs the release
  you had, and nothing is lost. Automatic updates skip a release that rolled
  back or failed verification, and retry the rest after a backoff
  ([when an update fails](/updating/#when-an-update-fails)). There is nothing to
  do unless it keeps happening; the error says why.
- **not rolled back**: the new release did not come up, and the release you had
  could not be confirmed either, so the daemon may be down. Automatic updates
  paused themselves, and the banner is red.

**Fix, when it was not rolled back.** The error spells out the steps. In order:

1. See whether the daemon runs: `seamlessd status`. If `seamlessd doctor` warns
   on `tls trust`, read the TLS cause below first - the release you had is
   probably running, and only its confirmation failed.
2. Reinstall the release you had, pinned to it:

   ```bash
   curl -fsSL https://thereisnospoon.org/install | SEAMLESS_VERSION=0.7.2 sh
   ```

   ```powershell
   $env:SEAMLESS_VERSION='0.7.2'; irm https://thereisnospoon.org/install.ps1 | iex
   ```

3. Only if the data has to go back to how it was before the update, restore the
   backup the error names. `seamlessd import --from <backup>` merges into a data
   dir that already holds data, so a true restore needs an empty one: stop the
   daemon, move the data dir aside, import, start.

   ```bash
   seamlessd stop
   mv ~/.seamless ~/.seamless.broken
   seamlessd import --from ~/.seamless.broken/backups/pre-update-v0.7.2-20261009T141500Z.tar.gz
   seamlessd start
   ```

   ```powershell
   seamlessd stop
   Move-Item $HOME\.seamless $HOME\.seamless.broken
   seamlessd import --from $HOME\.seamless.broken\backups\pre-update-v0.7.2-20261009T141500Z.tar.gz
   seamlessd start
   ```

   The backup lives inside the data dir, so after the move its path starts with
   the new name. Anything written after the backup was taken stays in the moved
   directory.
4. Once the install is healthy, press **Resume automatic updates** in
   **Settings -> Updates**. The release that failed stays skipped.

Causes worth knowing:

- **The updater refused to start it.** The attempt failed while checking it may
  install now (the `gates` stage), before anything changed. The daemon tries
  again after the backoff, and is refused the same way until you act.
  **Last attempt**, the console banner, `seamlessd doctor`'s `updates` row and
  `seamlessd update --check`'s `fix` row name the cause and, while the daemon
  still runs the release the attempt started from, the fix:

  | The cause | The fix |
  |---|---|
  | the installed seamlessd is not the version the daemon runs - a release was installed without restarting the service | `seamlessd restart`, so the daemon runs the release on disk |
  | the install no longer looks like one the installer manages | `seamlessd restart`, so the service re-reads how it was installed |
  | the updater could not make sure it runs outside the service, which the installer restarts | update by hand from a terminal: `seamlessd update` |
  | automatic updates were turned off before it began | nothing: that was your choice |
- **A TLS daemon this machine cannot verify.** The updater confirms an update
  through this machine's own client - the system's roots plus `tls.ca_file`,
  dialing `server_url` - the one its hooks use. If that client cannot verify the
  daemon's certificate, no update can be confirmed, and neither can the rollback
  after it, so every attempt ends "not rolled back" and pauses automatic
  updates. `seamlessd doctor`'s `tls trust` row warns about this and names the
  repair; fix it before you resume.
- **The rollback drill is on.** While `~/.seamless/update/test-fail-confirm`
  exists, every update fails its confirmation on purpose and rolls back, and
  `seamlessd doctor` warns on `update drill`. Delete the file.
- **`not enough free space for the backup`.** The attempt stopped before
  changing anything; the error says how much the backup needs. Free space on
  the data dir's disk; it is retried after the backoff.
- **The updater never started.** Nothing changed, and it is retried after the
  backoff. The daemon's own log says why, on its
  `update: could not start the updater` line;
  [The updater on your OS](/reference/service/#the-updater) has where that log
  is and what the updater needs.
- **applied with warnings.** Not a failure: the new release serves, but the
  installer reported a problem after it came up, usually while wiring the agent
  clients. Run `seamlessd install-hooks`.

## Automatic backups are failing or overdue {#automatic-backups-are-failing}

**What is happening.** Every console page carries one of two alerts, linking to
**Settings > Backups**:

- **Automatic backups are failing.** Two automatic backups in a row did not
  finish. The alert names when the first failed and why.
- **Automatic backups are overdue.** None has finished, or been skipped because
  nothing changed, for three intervals. Usually the daemon was not running, or
  the machine slept through them.

**Got it** hides the alert in that tab until a new episode begins.
`seamlessd doctor`'s `backups` row says the same from a terminal.

**Fix.** **Settings > Backups** shows the last failure's reason under the status
card. Fix what it names, then press **Try again**, which runs the scheduled
backup now:

| The reason says | What to do |
|---|---|
| the backup folder's disk is not connected | Plug the disk in, or choose another folder. Nothing is written under a disk that is not mounted. |
| the backup folder no longer exists | Reconnect its disk, or choose the folder again. |
| the backup folder is not writable | Fix its permissions, or choose another folder. |
| macOS is keeping Seamless out of the backup folder | Allow Seamless in **System Settings > Privacy & Security > Files and Folders**, or give it **Full Disk Access**. A folder in iCloud Drive, Dropbox or on an external disk can need this. |
| the disk is too full | Free space on the folder's disk and on the data folder's: a backup needs its size free on each, plus a reserve of 1 GiB or the backup's size, whichever is larger. Or keep fewer backups. |
| the backup did not read back as written | The disk or the sync service changed the file as it was written. Try again, or choose a local folder. |
| the backup folder is not private to Seamless | Under [peer_auth](/guides/peer-auth/#restore) no other account may be able to change the folder or any folder above it. Choose one that only root and the daemon's account can write to. |
| the disk does not keep backups private to Seamless | Under [peer_auth](/guides/peer-auth/#restore) a backup must stay owned by the daemon's account and private to it, which exFAT disks and many network shares cannot do. Choose a folder on a disk that keeps file owners. |

A backup that **Back up now** makes and that fails shows its reason on the
status card too, but does not count toward the alert.

## A restore did not finish {#a-restore-did-not-finish}

**What is happening.** The result card in **Settings > Backups** (and
`seamlessd restore`, and doctor's `backups` row) says the restore did not
finish, and why. Whatever the reason, today's data is in place:

- **The restore was not applied because Seamless did not restart within an
  hour.** A staged restore expires an hour after it was staged, so an old one
  never lands by surprise. On Windows, the console cannot restart Seamless
  itself: run `seamlessd restart` within the hour, or start the restore again.
- **The backup did not verify**, or **changed after it was approved**. The
  file is damaged, or was replaced between the confirm and the restore. Nothing
  changed. Restore another backup.
- **The swap failed and was rolled back.** A replace swaps the data in with a
  few renames and records each step first. A start stopped part way, by a crash
  or a power cut, puts today's data back by itself, and doctor says so meanwhile.
- **The merge stopped partway.** What the merge added stays, and nothing was
  removed. The next start runs it again, up to three times. Restoring the same
  backup with **Only bring back what's missing** is safe to repeat, since a
  merge skips what is already here.
- **The data folder's file system cannot be locked.** No restore can swap the
  data safely without the data directory's lock, so the restore was dropped.

**When Seamless will not start.** If a swap cannot be rolled back - a file
that cannot be moved back, say - `seamlessd serve` refuses to start rather than
serve half of each. Its error says that today's data is split between
`~/.seamless` and `~/.seamless/restore/<id>/previous`, recorded in
`~/.seamless/restore/pending.json`. Move each entry of `previous/` back into
`~/.seamless` (first moving aside a restored entry of the same name that is in
the way), then remove `pending.json` and start Seamless again. Nothing under
`~/.seamless/restore/` is ever deleted before its data is saved elsewhere.

**The previous data is still in `restore/`.** After a replace, Seamless saves
the data it replaced as a **Before restore** backup and only then removes it.
Until that is done, or when it fails, the result card and doctor name
`~/.seamless/restore/<id>/previous`; a restart tries again. A
`restore/<id>/previous` folder that no record accounts for makes doctor warn:
check that it holds nothing you need, then remove it.

## `seamlessd update` refuses: a restore is waiting to finish

**What is happening.** A restore is staged and has not been applied yet
(`~/.seamless/restore/pending.json` exists). An update backs the data up and
replaces the binary, so it never runs under a restore that has not finished.
Automatic updates wait, **Update now** is refused, and an attempt that was
already starting records the refusal `restore_pending`.

**Fix.** Restart Seamless (`seamlessd restart`) to finish the restore, or cancel
it with **Cancel restore** in **Settings > Backups**. Then update.

## `memory_write` says the name is held by a superseded memory

**What is happening.** A superseded or archived memory leaves every index but
stays on disk as provenance - and it still owns its filename. The name is taken by
something you cannot see in any index.

**Fix.** Pick a different name, or free the old one with `memory_delete`. Prefer a
different name: the old file is the record of what you used to believe, and
deleting it destroys the history that explains the current state. See [Memory &
notes](/concepts/memory/).

## A `supersedes` failed but the memory was written

**What is happening.** This is a deliberate partial failure, reported rather than
swallowed. The new memory's content is valid knowledge, so it is **written and
kept** - but the supersession did not happen, which means **the old memory is
still active**, live in briefings and recall alongside its replacement. That is
exactly the contradictory-store state the lifecycle exists to prevent, so it comes
back as a tool error rather than an error field inside a success payload.

**Fix.** Correct the `supersedes` target and re-run the write. Re-writing the same
name is a lossless in-place update, so retrying is safe.
