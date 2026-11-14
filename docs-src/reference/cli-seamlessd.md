---
title: seamlessd CLI
description: The daemon and operator CLI - serve, doctor, export, import, backups, restore, setup, install-hooks, client-config, uninstall, update, map-repo, unmap-repo, retire-project, family, devices, relay, console-open, start/stop/restart/status, and version.
---

`seamlessd` is both the server and the operator CLI. `serve` runs the daemon;
every other subcommand is a one-shot that opens the same config and database
directly, without going through a running server. That means most of them work
whether or not the daemon is up - and that `map-repo`, `unmap-repo`,
`retire-project` and `family` write state the running daemon reads. They never
migrate: a database behind this binary's schema is refused (the daemon migrates
it at its next start), and only `map-repo` creates a database when there is
none.

Each subcommand parses its own flags. None of them take positional arguments
except `family`, which takes only positionals, and `restore`, which takes the
backup to restore.

For the keys every command below resolves, see
[Configuration](/reference/configuration/).

## seamlessd serve {#seamlessd_serve}

```bash
seamlessd serve [--addr HOST:PORT]
```

Starts the HTTP server and blocks until SIGINT or SIGTERM, then shuts down
gracefully. `--addr` overrides the configured bind address (default
`127.0.0.1:8081`). The flag is the bind address, so everything downstream that
derives "where do clients reach this daemon" answers from the address actually
listened on - a configured `server_url` still wins over both.

It refuses to start at all under `role: client`, before it opens a file or a
port: a client install has no daemon of its own by definition, and serving one
would give the machine a second, empty corpus its own hooks never write to.

One daemon per data dir: `serve` takes an exclusive lock on
`<data_dir>/seamlessd.lock` before it opens the database, and records its PID
there. A second `serve` on the same data dir waits up to 30s for the holder to
exit, then fails naming the lock and the holder's PID without touching the
database. The operating system drops the lock when the holder exits, however it
exits; never delete the file. On a file system that cannot lock, `serve` warns
and runs unguarded, as earlier releases did.

With `tls.cert_file` and `tls.key_file` both set, the listener is **https**
with a TLS 1.2 floor, and the console session cookie is marked `Secure`. One
without the other is refused when the config loads. Outermost in the handler
chain is the Host-header allowlist: the loopback names, a concrete bind host,
the host of `server_url`, and `allowed_hosts`. Anything else gets `421
Misdirected Request`. A wildcard bind with no host named anywhere is the single
unguarded case - naming one arms the guard there too. Binding beyond loopback
logs a `SECURITY` warning at startup whose text differs with TLS on or off, and
a second warning when the bind is wide while `server_url` still names loopback.
[Share one daemon across your computers](/guides/network-install/) is the setup guide.

Beside a concrete IPv4 bind, `serve` also listens on the IPv6 addresses of that
interface that reach no further: `[::1]` beside `127.0.0.1`, a tailnet's
`fd7a:...` address beside its `100.x` one. If another process holds one of
them, `serve` refuses to start rather than leave it to that process: `[::1]`
always, an address the `server_url` host or (with passkeys on) a passkey
origin's host resolves to, and under peer_auth any of them. The error names the
address, says it is in use by another process and how to find it (`sudo lsof
-nP -iTCP:<port> -sTCP:LISTEN`); the service starts `serve` again, and it runs
once the address is free. An address this computer does not have yet is skipped
with a warning, as is, without peer_auth, one no name points at.

On a true first run - no config file anywhere in the search order and no
`SEAMLESS_MCP_API_KEY` in the environment - it generates the bearer key and
writes it to `~/.config/seamless/seamless.yaml` before starting. An existing
config file is never edited, even when its key is empty.

It wires up:

- `/healthz` - liveness plus a database ping. Reports `degraded` with a 503 when
  the ping fails.
- `/api/mcp` - the MCP tool endpoint, bearer-authenticated.
- `/api/hooks/...` - the session and plan-capture hooks.
- `/console/...` - the observability console. The bare root `/` redirects here.

Startup is deliberately tolerant of a half-configured install, and the log is
where you find out:

- **No embedder** - recall degrades to FTS-only for the life of the process. A
  missing credential logs a warning; a *malformed* setting (a bad `base_url`)
  logs an error, because that one is a typo rather than a choice.
- **No chat client** - gardener digest passes no-op.
- **Empty `mcp.api_key`** (a config file exists but leaves it blank) - logs a
  warning, and every MCP and hook request is then rejected.
- **Gardener disabled** - logged, and no maintenance passes run.

The startup line carries the version, commit, and data directory, which is how
you spot a daemon running older code than your working tree.

## seamlessd doctor {#seamlessd_doctor}

```bash
seamlessd doctor
```

Server-side self-checks. Each line reports `ok`, `info`, `warn`, or `fail`;
**only a `fail` exits non-zero** - warnings and info lines are informational.
Checks stop early if config or the database cannot be loaded at all.

| Check | What it reports |
|---|---|
| `binary` | The version that ran. |
| `config` | Which file it loaded, or that it fell back to defaults + env. A `./seamless.yaml` found as the search's last step is an **info** line: `seam` never reads the working directory's file, so it names the `SEAMLESS_CONFIG=` that would make `seam` read the same one. |
| `config permissions` | Who can read and rewrite that file. A file that holds a secret (`mcp.api_key`, `llm.openai.api_key`, `llm.anthropic.api_key`, or a `server_url` or `base_url` with a user or password in it), and any `role: server` file, must be owner-only: a **warn** names a file or directory that grants group or other access, or a symlink, with the `chmod` that fixes it. A `role: client` file holding none of them may instead be a **shared client config**: a regular file owned by root or the [pinned daemon account](/guides/peer-auth/#deploy-it-the-daemon-runs-as-its-own-account), writable by no other account, under directories no other account can write (sticky ones aside). That is an **info** line, readable by every account; one that another account could rewrite is a **warn** naming how. A secret-free client file you own yourself keeps the owner-only rule. A [peer_auth](/guides/peer-auth/) daemon's own config is an **info** line here: the `peer_auth` rows hold it to `serve`'s rule instead (root's or the daemon's account's, writable by no other account, not world-readable), which the guide's `root:_seamless` `0640` layout meets. Windows is an **info** line: its ACLs are not checked. |
| `data_dir` | The resolved data directory. |
| `mcp.api_key` | Set, or a warning that `/api/mcp` will reject everything. |
| `llm` | The provider, or a warning that its credential is missing. |
| `embedder` | Probes the embedder with a real embed call. Unreachable, unconfigured, or provider `anthropic` (no embeddings API) is a warning: recall degrades to FTS. |
| `bind` | Loopback is OK. Non-loopback with TLS is OK with the reminder that the bearer key is still the only authentication; non-loopback *without* TLS is a warning naming what travels in the clear. |
| `bind.addresses` | Dials the bind and each IPv6 address `serve` listens on beside it, and reads the instance id `/healthz` names at each. One daemon at all of them is OK; another process answering beside it is a **fail** (stop it, then restart the daemon), as is one answering there while the daemon is not; nothing answering beside a daemon that answers is a **warn** (it could not listen there when it started, and another process can take it). The `server_url.addresses` and passkey rows read the same dial: an address another process answers at is not held, and on a passkey origin a **fail**. No row for a wildcard bind, a bind with nothing beside it, or while nothing answers. |
| `server_url` | Fetches `/healthz` through the advertised URL. Not answering is **info** (the daemon is often stopped while doctor runs); a `421` is a **fail** naming the Host header it just refused, which means the allowlist and the advertised name disagree. A derived URL says so. A `server_url` that names a user (`https://user:password@host`) is a **warn** instead, and is not fetched: the fetch would send the user and password as Basic auth. Every other row, and serve's log, names the address without its user. |
| `tls` | Off, or the certificate's expiry (a warning from 30 days out, nothing auto-renews) and whether its SANs cover the host of `server_url` - the one that fails at the client's handshake with a message that names no file on the server. |
| `tls.key_file` | Without peer_auth, on a server other computers reach: who can read the TLS key. Your account alone is **info** (the daemon runs as you, so every process of yours can read it and serve the daemon's name on any port; locked projects move it out of their reach); group- or world-readable is a **warn** with the `chmod 600`. Under peer_auth the `peer_auth` rows judge it, as `serve` does. |
| `server_url.addresses` | Every address the configured `server_url` host resolves to, against what the daemon listens on (bind plus `serve`'s paired IPv6, each dialed: `bind.addresses`): all held is OK; one this machine holds and the daemon does not is a **warn** (another local process could answer for the name); one this machine does not hold is a **warn**. No row for a derived, loopback or proxied URL, or a host the passkey rows judge. |
| `tailscale` | **info** when the tailnet issues certificates for the `server_url` host: an account allowed to run `tailscale cert` can serve the name with a valid certificate, so the daemon holding every address of the name is what keeps its pages off the origin. |
| `database` | Path, schema version, and table count, or that none exists yet (a fresh install: the daemon creates it at its first start). Never creates or migrates a database. |
| `schema version` | An **info** line pairing what the database has applied with what this binary compiles - `v25 applied / v25 compiled`. A database *ahead* of the binary warns: it was migrated by a newer `seamlessd` (a downgrade, or an update rollback that could not restore the pre-update database). Run that newer release again (`seamlessd update`), or restore a backup taken before it (`seamlessd restore`, or `seamlessd import` into an empty data directory). `serve` logs the same warning at startup, and an archive from such a database would be refused. A database *behind* the binary warns too: a daemon of this version migrates it at its next start, and doctor skips the checks that need the current schema. |
| `repo map` | Warns when mapped paths belonging to **this** host name directories that no longer exist on disk. A moved repo adopts its project at its next session start; a moved-and-renamed repo needs the printed `map-repo` override, and `unmap-repo --stale` clears the dead entries. Rows belonging to other hosts are counted and reported as not verifiable from here - never stat'd, never treated as missing. |
| `remote sessions` | An **info** line: which other machines used this daemon in the last 24 hours, and how many daemon-side captures were skipped for them. `none in 24h` on a single-machine install. |
| `mcp_tools` | On a server install, fails if the number of registered tools disagrees with the expected count - catches a tool written but never wired in. On a `role: client` install it is the live count instead: `tools/list` against the server, judged against that server's effective feature state. |
| `claude CLI runtime` / `claude app runtime` | Each discoverable Claude Code runtime's self-reported version, separately: the PATH CLI and, on macOS, every runtime the desktop app has retained - they can differ, and collapsing them would hide exactly that skew. No discoverable runtime means no lines. |
| `hooks` | Claude Code definitions compared with today's desired profile, in `~/.claude/settings.json`, `./.claude/settings.json`, and [Claude Code's managed settings](#seamlessd_install_hooks_managed). An event the managed settings carry counts as installed there: a stale definition or a missing `seam` executable in them is a **warn** naming the file, which an administrator must update, and a copy of that event in a user or project file is a **warn** because it runs twice. |
| `claude desktop mcp` | The chat surface's `claude_desktop_config.json` entry compared with the desired stdio bridge. Absent is an **info** line naming the opt-in command, never a nag; an exact entry reports OK while stating that the running app's loaded state is unverifiable (the app reads the config at startup). No lines when neither the app nor a desktop config exists. |
| `codex CLI runtime` / `codex app runtime` | Each discoverable Codex runtime's self-reported version, separately, on the same principle. |
| `codex hooks` | Codex current, stale, and missing definitions, including command targets. |
| `codex hook trust` | Always warns that trust is unverified and directs you to Codex `/hooks`; no private trust state is read. |
| `codex hook activity` | Last observed SessionStart/UserPromptSubmit event, if any; evidence only, not proof of current trust. |
| `codex mcp` | Exact enabled stdio bridge state from `codex mcp get seamless --json`, plus executable/config target existence. |
| `feature skills` | An **info** line when a client's skill home still holds a skill for an [optional feature](/reference/console/#optional-features) you switched off - the daemon never deletes there on a toggle. Re-run `install-hooks` to remove it, or re-enable the feature. |
| `gardener` | The ticker configuration, or a warning that it is disabled. |
| `backups` | [Scheduled backups](/guides/data/#backups-in-the-console) and restores, from the records in `~/.seamless/backups/` and `~/.seamless/restore/`. A restore in flight comes first: one staged (**info**: it applies when Seamless restarts, and expires an hour after it was staged), one stopped part way through its swap or its merge (**warn**: the next start undoes the swap, or runs the merge again), or one applied with its bookkeeping left for the next start (**info**). Then a **warn** for replaced data still waiting to be saved as a backup, a `restore/<id>/previous` folder no record accounts for, or a last restore that did not finish. Otherwise the schedule: **info** when automatic backups are off, a **warn** when the last two or more automatic backups failed (the first failure's time and the last error) or nothing has confirmed the backups current for three intervals, **info** when none has been made yet, and **ok** with when they were last confirmed. Absent under `role: client`. |
| `updates` | The [automatic update check](/updating/#automatic-update-checks) and [automatic updates](/updating/#automatic-updates), read from the daemon's and the updater's own records (`~/.seamless/update/state.json`, `attempt.json`) without asking GitHub or the daemon. One line, the most urgent thing first. An update that could not be rolled back cleanly, and that the daemon has not settled, is a **warn** carrying the updater's recovery steps and its log. An update under way is **info**. Automatic updates that paused themselves are a **warn** naming why, with the way out: resume them in the console under **Settings > Updates**. A daemon that updates itself says, as **info**, what it installs next and when: once its soak (`update.min_age`) is over, then when no agent session is live, or at a lull in requests after its deadline (`update.max_defer`). A newer release that automatic updates skip (blocked after a failed attempt, or published without a signed checksums bundle) is a **warn** with the command to install it by hand; a hold after a deliberate downgrade is **info**. A daemon that only notifies is **ok** when up to date and a **warn** naming the newer release and the command for this install. The last attempt is added while it concerns the running release, with the updater's own error; one that applied with warnings names `seamlessd install-hooks`. One the updater's gates refused is a **warn** ending in the owner's action for that refusal: `seamlessd restart` when the installed seamlessd is not the version the daemon runs (a release installed without a restart) or the install no longer looks installer-made, and `seamlessd update`, run by hand, when the updater could not make sure it runs outside the service. Automatic updates turned off before it began need no action. **info** when checks are off (and why) or have not run yet. A check failing for a week is a **warn** - usually a proxy the service's environment does not know about. |
| `update.release_signers` | Only while the config sets [`update.release_signers`](/updating/#owner-signed-releases): **ok** naming the keys a release must be signed with (principal and SHA256 fingerprint), and the last release the pin refused, from the daemon's record; **fail** when the file cannot be used - missing, unreadable, writable by another account, or naming no key for `seamless-release` - since every update is then refused. Under peer_auth the file is held to serve's rule (root's, through root's directories), so run doctor as the daemon's account to see what serve sees. |
| `hooks' seam` | Only on a client of a [peer_auth](/guides/peer-auth/) socket: who can replace the `seam` the Claude Code hooks run. **info** when it is your own copy (every agent you run could rewrite it) or the daemon's (`/opt/seamless/bin/seam`, which the daemon's account could); **ok** for a copy only root can replace, naming whether it is the daemon's release; **warn** when root's copy was built from another commit than the daemon runs - refresh it ([how](/guides/peer-auth/#client-copy)) - or when another account can replace it. |
| `relay.binary` | Only on a computer with a relay: root's copy of `seamlessd` (`/opt/seamless/bin/seamlessd`, the one the relay's service runs) against the `seamlessd` running doctor, by SHA-256. **info** when they are the same; a **warn** when they differ, since an update replaces your own copy alone (`~/.local/bin`): run `seamlessd setup` again, then block 2 of [the root step](/guides/network-install/#the-relays-root-step) as an administrator, and restart the relay. A **warn** too when root's copy is missing or cannot be read. |
| `update drill` | Only while `~/.seamless/update/test-fail-confirm` exists: a **warn**, because the rollback drill's switch makes every update fail its confirmation and roll back. Delete the file to end the drill. |
| `tls trust` | Whether this machine's own client verifies the certificate the daemon serves for `server_url`. That client is the one its hooks dial with, and the one an update confirms the new daemon through. The answer comes from the config (the system roots plus `tls.ca_file`) without a dial, so it answers while the daemon is down. **ok** when the client verifies it; otherwise a **warn** naming the repair, since every update would fail its confirmation, and so would its rollback's: the attempt ends not rolled back and automatic updates pause. A `tls.ca_file` that cannot be loaded warns on any install. Absent when TLS is off and the client is usable. |

Under `role: client` the report is a deliberately short, different list: a
`role` info line naming the server it dials, a `server_url` reachability probe,
the API key, the live MCP tool count, and the same desired-state hook and MCP
comparisons above. That tool count is the one client check that presents the
bearer key, so a wrong or rotated key surfaces there rather than as a silent
green. Everything else is absent because it describes a machine
that is somewhere else - the database, schema version, repo map, remote
sessions and feature skills all read a local `seam.db` a client does not have
(opening one would *mint* the database whose absence is what `role: client`
means); the gardener runs inside the daemon; and the LLM and embedder belong to
the server that does the embedding. The `server_url` probe is a **fail** rather
than info on a client, because a client has no benign "daemon is stopped"
state: until the server answers there are no briefings, memories, or tools on
this machine.

The definition checks compare current desired state, not mere existence. The
shared classifier recognizes exact current definitions, marked stale entries,
and only unmistakable legacy Seamless shapes; arbitrary foreign hooks survive.
Codex trust is a separate fact because Codex exposes no supported query for the
current trust decision. A recent hook observation cannot make a changed command
healthy.

Reach for it after changing config, after an upgrade, or as the first step when
recall has quietly gone lexical.

## seamlessd export {#seamlessd_export}

```bash
seamlessd export [-o FILE|-] [--no-db]
```

Writes the whole instance to one gzipped tar: the markdown corpus, a consistent
snapshot of `seam.db`, and a `manifest.json` describing what is inside.

**Config and `mcp.api_key` are never in the archive.** A restored instance gets
its own config and a freshly generated key, which is what lets an archive be
copied to another machine, a NAS, or a colleague without carrying this machine's
only credential.

| Flag | Default | Meaning |
|---|---|---|
| `-o` | `seamless-<host>-<UTC timestamp>.tar.gz` in the working directory | Where to write the archive. `-` streams it to stdout and puts the report on stderr. |
| `--no-db` | `false` | Export the markdown trees only. Sessions, tasks, trials, events, and embeddings are then **not** in the archive. |

Layout, in tar order:

```text
manifest.json                       always first
seam.db                             absent with --no-db
memory/{project|_global}/{name}.md
notes/{project|_global}/{slug}.md
```

`manifest.json` first is deliberate: a reader can refuse an archive - wrong
format, or a schema newer than its own binary understands - before extracting a
single byte. Every entry is a regular file with mode `0600` and uid/gid zeroed,
so a restore under another account carries no ownership from the exporting
machine.

**It is safe to run against a live instance.** The snapshot is SQLite's
`VACUUM INTO` on a handle that never migrates, so a newer binary cannot move the
schema under a running older daemon, and a write in flight is simply not in the
snapshot rather than half in it. The database is snapshotted *before* the trees
are walked, so the file set is a superset of what the snapshot's index describes;
the import's reconciliation heals that window.

A named destination is claimed with `O_EXCL` and written through a sibling
`.tmp` that is renamed into place, so an export never overwrites an existing
archive and never leaves a truncated one under a finished-looking name.

## seamlessd import {#seamlessd_import}

```bash
seamlessd import [--from DIR|FILE|-] [--embed=false]
                 [--skip LIST]              # v1 directory only
                 [--dry-run] [--force]      # archive only
                 [--keep-locks|--remove-locks]  # archive only
```

Imports another store into this instance. **What `--from` names on disk decides
which of two unrelated operations runs** - never a flag:

| `--from` is | Source | What happens |
|---|---|---|
| a directory (the default `~/.seam`) | A Seam v1 data directory | Memory and note files are written and indexed here; trials, sessions, and tool-call events are inserted into the database. |
| a regular file, or `-` | A `seamlessd export` archive | A **restore** into an empty data directory, or a **merge** into a populated one. |

| Flag | Default | Applies to | Meaning |
|---|---|---|---|
| `--from` | `~/.seam` | both | Source path. A leading `~` expands; `-` reads an archive from stdin. |
| `--embed` | `true` | both | Embed imported items for cosine search, using the configured provider. |
| `--skip` | `briefings` | v1 directory | Comma-separated storage projects to skip. |
| `--dry-run` | `false` | archive | Report what the import would do and change nothing. |
| `--force` | `false` | archive | Allow a fresh restore, or a merge that brings locks, while a daemon is still answering for this data directory. |
| `--keep-locks` | `false` | archive | Keep the backup's locks without being asked, even with no passkey able to open them yet. |
| `--remove-locks` | `false` | archive | Import the backup's locked projects without their locks. Cannot be combined with `--keep-locks`. |

A flag from the other family is an **error**, not an ignored no-op: `--dry-run`
against a v1 directory would otherwise read as "previewed, nothing happened"
while the import actually ran.

### Importing a v1 directory

**Idempotent by id**, so re-running imports only what is new - which makes a
delta re-import safe after the first pass. It honours SIGINT/SIGTERM, and prints
a report even when the import ends in an error. With `--embed` on and no usable
embedder, it warns and imports without vectors rather than failing.

### Importing an archive

An archive that holds locked projects is checked for a passkey able to open them
before anything is written. See [Locked projects](/guides/data/#locked-projects).

Restore-or-merge is a property of the **destination**, printed in the report and
never overridable. `import` has no `--mode`: the only thing an override could do
is replace a populated instance's database. To replace a populated instance's
data on purpose, with today's saved first, use
[`seamlessd restore`](#seamlessd_restore).

- **Fresh** (restore) when the data directory is absent, or holds neither
  `seam.db` nor `seam.db-wal` and no regular non-dot file in either tree. The
  markdown is restored byte-for-byte and the snapshot is renamed into place last,
  so an interrupted restore leaves files and no database - which the next run
  reads as fresh again and repeats cleanly.
- **Merge** otherwise, first-writer-wins by ULID: an item or row whose id is
  already here is skipped, which makes re-merging the same archive a no-op.

A fresh restore replaces `seam.db` wholesale, so it **refuses while a daemon is
answering** for that data directory and tells you to run `seamlessd stop`;
`--force` overrides. A merge and a `--dry-run` need no such thing - a merge
writes through the same files layer the daemon uses, and a dry run writes
nothing.

What a merge does **not** touch:

| Not merged | Why |
|---|---|
| `settings` | Repo paths, families, briefing overrides, and the embedder switch describe *this* machine. |
| `embeddings` | Vectors belong to whichever model this instance runs; imported items are embedded on write instead. |
| `*_index`, `fts`, `retrieval_stats`, `jobs` | Rebuildable mirrors, refreshed by the import itself. |

Collisions are **reported, never resolved**. A corpus file whose path is already
held by a different item is left unwritten and both ids are named; a
`sessions.name` or `projects.slug` already taken is reported rather than renamed,
because minting a new name would invent an identifier nothing refers to. The run
still exits 0 - the report is the work item.

When the archive's `embedding_models` differ from this instance's embedder, the
report says so and points at the console's re-embed
(Settings → Embeddings). Vectors from two models are not comparable, so that
mismatch does not heal itself.

## seamlessd backups {#seamlessd_backups}

```bash
seamlessd backups [--json]
```

Lists the backups that [`restore`](#seamlessd_restore) and the console's
**Settings > Backups** can put back, newest first: those in the backup folder
and those in `~/.seamless/backups`, where the safety backups taken before a
restore and the updater's pre-update backups always stay. Each line has when the
backup was made, its kind (`automatic`, `made by you`, `before restore`,
`before v0.8.1 update` or `exported`), its size and its file name, and under it
what it holds: memories, notes, projects and sessions, `(memories and notes
only)` for a `--no-db` export, and the machine it came from when another
Seamless made it. An iCloud placeholder or a backup a newer Seamless made says
why it cannot be restored.

The folder is the one the daemon last recorded in
`~/.seamless/backups/.schedule.json`, so a folder chosen in the console counts,
and otherwise the config's (`backup.dir`, or `~/.seamless/backups`). A folder
that cannot be used says why. `--json` prints the same listing as JSON.

It opens no database, so it answers whether or not the daemon runs. Under
`role: client` it refuses: the backups are on the server.

## seamlessd restore {#seamlessd_restore}

```bash
seamlessd restore <file|name> [--merge] [--yes]
```

Puts a backup back. `<name>` is a file name as `seamlessd backups` lists it;
`<file>` is any backup's path, such as an export or a backup copied from
another machine. The command shows where the backup came from and what it
holds, says what will happen, and asks on the terminal. `--yes` skips the
question, and without a terminal it is required.

| Flag | Default | Meaning |
|---|---|---|
| `--merge` | `false` | Only bring back what the backup has and this install lacks, first-writer-wins by ULID, as an [import merge](#importing-an-archive) does. Nothing here changes. Without it, the backup replaces all data, and today's is saved first as a `-before-restore` backup in `~/.seamless/backups`, so restoring that one undoes it. A `--no-db` export can only be merged. |
| `--yes` | `false` | Restore without asking. |

Either way, locks, passkeys, project privacy, repo routes and console settings
stay as they are now. A project locked today stays locked, a lock in the backup
comes back, each project keeps the stricter of its two privacy settings, and
every agent lease, console unlock and read grant in the backup is ended.
[What a restore keeps](/guides/data/#restore-in-the-console) has the list.

How it finishes depends on the daemon:

- **With the daemon running**, the command checks the backup, prepares the
  restored data beside today's (or, for a merge, saves today's data first), and
  records the restore in `~/.seamless/restore/pending.json`. The daemon notices
  within seconds, restarts, and applies it before it opens the database. The
  command waits up to three minutes for the result and prints it. On Windows,
  where the daemon cannot restart itself in place, the command restarts the
  service. A restore that waits more than an hour for its restart is dropped,
  and nothing changes.
- **With no daemon**, the command takes the data directory's lock and applies
  the restore itself, then, for a replace, saves the replaced data as a backup.

A replace swaps the data in with a few renames, recorded before each step, so
a start interrupted part way puts today's data back by itself (see
[A restore did not finish](/guides/troubleshooting/#a-restore-did-not-finish)).
Nothing under `~/.seamless/restore/` is deleted until its data is saved
elsewhere.

It refuses:

- a database older than this binary, like every one-shot command, and a data
  directory with no database: on a new machine, start Seamless once first;
- a backup a newer Seamless made, an iCloud placeholder that is not downloaded,
  and a file that is not a backup;
- a file system that cannot lock the data directory, since no swap is safe
  without the lock;
- while an update is running, or another restore is waiting to finish;
- under `role: client`: restore on the server.

Under [peer_auth](/guides/peer-auth/#restore), run it as the daemon's own
account, never as root. It has no `--config` flag, so name the config through
the environment, as the console's command does:

```bash
sudo -u _seamless env SEAMLESS_CONFIG=/opt/seamless/etc/seamless.yaml /opt/seamless/bin/seamlessd restore <file>
```

The account is the proof the console asks a passkey for.

## seamlessd setup {#seamlessd_setup}

```bash
seamlessd setup [--state FILE [--finish]]
```

How this computer uses Seamless: **Standalone** - everything on this
computer, the default install - or **Networked**: the **Server**, which keeps
the data for the other computers, or a **Client** of one, which keeps none of
its own. Set up the Server first: each Client needs its address and key.
Windows is offered Standalone and Client only: it has no peer_auth and no
relay, so a Windows computer can be a Client but not the Server, and as a
Client it is always a key client, whose agents never open a locked project
([Networked Seamless](/concepts/networked/)).

The installers ask through it. Once they have downloaded and verified a
release, they run its `seamlessd setup` before anything is installed and act
on the answer. It asks only a person at a terminal who set no `SEAMLESS_*`
variable; with one set, or no terminal, the environment is the whole answer,
and a run without the setup variables below installs exactly as it did before
they existed - every run `seamlessd update` makes is one. On a re-run it says
how the computer is set up and offers only the changes it can make: a
Standalone install can become the Server, a computer that runs the daemon
can turn on locked projects, and a Standalone install with locked projects can
[sign in with passkeys and retire the key](#seamlessd_setup_retire). A Client chosen at the terminal is asked for its
Server's address and key, which `seamlessd client-config` prints on the
Server; the key goes straight into `~/.config/seamless/seamless.yaml` (`0600`).

Run by itself, `seamlessd setup` says how this computer is set up and offers
the same changes. On a computer with nothing installed it points at the
installer.

| Variable | Answer |
|---|---|
| `SEAMLESS_SETUP=standalone\|server\|client` | the kind, for a script (`standalone\|client` on Windows). On a re-run, the installed kind keeps it as it is and `server` makes a Standalone install the Server; any other change is refused, naming why |
| `SEAMLESS_SERVER_URL`, `SEAMLESS_MCP_API_KEY` | a Client's Server and its key, as the installers have always read them (a Client of the Server's own key, which pairs nothing); `SEAMLESS_SETUP` may only agree |
| `SEAMLESS_PAIR_CODE` | a Client's pairing code from the Server's Devices page (one computer, 10 minutes): with `SEAMLESS_SERVER_URL`, setup pairs this computer -- a relay on macOS and Linux, whose root step it prints, or a key device of its own |
| `SEAMLESS_SERVER_FINGERPRINT` | the Server certificate's SHA-256 as its setup printed it (the Server's one-liners carry it): compared before the code is sent, and the certificate trusted alone when this computer's roots do not vouch for it |
| `SEAMLESS_PAIR_AS=relay\|key` | what a macOS or Linux Client pairs as; `key` where you have no root (a container, CI). Windows is always `key` |
| `SEAMLESS_IMPORT` | a backup a first Standalone or Server install starts from ([below](#seamlessd_setup_import)) |
| `SEAMLESS_IMPORT_LOCKS=keep\|remove` | what that import does with the backup's locked projects when no passkey can open them, as `--keep-locks` and `--remove-locks` answer [`seamlessd import`](#seamlessd_import) |
| `SEAMLESS_SERVER_NAME` | the name the other computers reach the Server at ([below](#seamlessd_setup_address)); with `SEAMLESS_SETUP=server` only |
| `SEAMLESS_TLS_CERT`, `SEAMLESS_TLS_KEY` | that name's certificate and key, full paths; none for this computer's Tailscale name, whose certificate setup gets from the tailnet |

A refusal stops the install before anything is installed. Locked projects are
not built into setup yet: the install ends by naming the guide that sets them
up by hand. A Server's install ends with what
each Client needs - its address, its certificate's fingerprint, how to get a
pairing code, and the one-liners - as `seamlessd devices code` prints them.

`--state FILE` and `--finish` are the installers' hand-off, and
`seamlessd setup -h` ends with `installer protocol: 1` for them to find: the
first writes the answer to `FILE`, the second finishes setup's part once the
installer's own steps are done.

### The Server's address {#seamlessd_setup_address}

A first Server install decides where the other computers reach it, before
anything is installed, and says each choice in one line:

- **The name.** This computer's Tailscale name (MagicDNS) when Tailscale runs
  here and the tailnet issues certificates for it: setup gets the certificate
  with `tailscale cert`, into `~/.config/seamless/tls`. Otherwise a domain of
  yours with its certificate and key, or a name on your LAN, for which setup
  prints the `mkcert` and `openssl` commands that make one, as
  [the network-install guide](/guides/network-install/#step-2-tls) does. The
  certificate must name the host and be valid, and its key be readable by
  your account alone (`chmod 600`). A script gives `SEAMLESS_SERVER_NAME`, and
  `SEAMLESS_TLS_CERT` and `SEAMLESS_TLS_KEY` unless it is this computer's
  tailnet name; `SEAMLESS_SETUP=server` without a name is refused.
- **The bind.** Setup resolves the name, A and AAAA, and binds the narrowest
  address that holds every one: an IPv4 address of the name with the IPv6
  addresses `serve` listens on beside it (a tailnet name's two), else the
  wildcard. An address of the name the daemon did not hold is one another
  local process could answer for the name from. A name that resolves to
  nothing, to loopback, or to an address this computer does not hold is refused.
- **The passkey origin** is [the one derived from `server_url`](/guides/peer-auth/#the-passkey-origin).
- **The config.** On a first install setup writes `addr`, `server_url`,
  `allowed_hosts` (empty) and `tls` into a new `~/.config/seamless/seamless.yaml`
  with the bearer key, after its other steps before the install, so an import
  that stops leaves no config behind. It never edits a config that exists:
  elsewhere it prints the lines, and the installer's next run restarts the
  daemon and wires the hooks for https.
- **With locked projects on**, the daemon's config and TLS pair are its own
  account's. Setup runs no root step: it prints the commands that put the pair
  in `/opt/seamless/etc/tls` (`root:_seamless`, the key `0640`), the daemon
  config's lines and the restart. Root never opens a file of yours: your shell
  reads the certificate and the key and root only writes them, each into a copy
  that is root's alone until it matches what setup read and checked, and only
  then moves into place. The commands are one AND-list, so the first failure
  stops them, and every tool is named by its full path.

When your account can get a certificate for the name itself (`tailscale cert`),
setup says so: the TLS key is then not what keeps a page of yours off the
console's origin; the bind holding every address of the name is. `seamlessd
doctor` reports the same facts (`tls.key_file`, `server_url.addresses`, `tailscale`).

### Starting from a backup {#seamlessd_setup_import}

A first install of a Standalone computer or of the Server asks whether to
start fresh or from a backup: the `.tar.gz` that `seamlessd export` writes, or
one from Settings > Backups. Setup reads the whole file through the import's
checks first, then restores it into the empty data directory exactly as
`seamlessd import --from <backup>` does - every memory and note byte for byte,
and the database with its sessions, tasks, passkeys and console settings -
before the installer installs anything or starts the daemon. A script names
the backup with `SEAMLESS_IMPORT`; a relative path is the directory the
installer ran in.

Setup imports only where that restore is safe, and otherwise refuses with the
reason before anything is installed: on a computer already set up (use
[`seamlessd restore`](#seamlessd_restore) there), on a Client (its data is its
Server's), into a data directory that holds a database or any memory or note
file, while a daemon answers at the address the new one will use, and with
`SEAMLESS_DATA_DIR` set (the service the installer starts reads its config,
not that variable).

A backup with [locked projects](/guides/data/#locked-projects) goes through
the import's own check. On a first install peer_auth is not on yet, so no
passkey can open them: at a terminal setup asks whether to stop, keep the
locks or remove them, and a script answers with `SEAMLESS_IMPORT_LOCKS=keep`
or `remove`. With neither, the install stops with nothing changed, as
`seamlessd import` does with no terminal. A kept lock is not enforced until
locked projects are on - the step the install names last - so until then
every agent on the computer can read those projects; on Windows, which has no
peer_auth, it never is. To keep them closed from the first moment a daemon
serves them, install with `SEAMLESS_IMPORT_LOCKS=keep` and
`SEAMLESS_NO_SERVICE=1`, then
[move the install to peer_auth](/guides/peer-auth/#move-an-existing-install-over)
before you start the daemon: the move takes the data directory whole, with
the backup's locks, fences and passkeys.

### Retiring the key {#seamlessd_setup_retire}

On a Standalone install with locked projects (peer_auth) your agents use the
socket and hold no key: the key's only use left is the console's config,
`~/.config/seamless/console.yaml`, which every process of yours can read. Run
`seamlessd setup` at a terminal and choose **Sign in with passkeys and retire
the key**. Setup runs no root step: it prints each one, then waits for its
effect, asking the daemon over your socket, since the daemon's config is not
yours to read.

1. **Checks.** Your config names the socket; the daemon's account is pinned
   (else it prints the two commands that pin it); a `localhost` passkey origin
   answers as this daemon on both `127.0.0.1` and `::1`; and the daemon's
   sign-in binds its cookie to the browser's key (a daemon too old for that is
   refused, to be updated first).
2. **Passkey sign-in on.** As an administrator, `sudo -e
   /opt/seamless/etc/seamless.yaml`, add `sign_in: passkey` under `console:`,
   keep `mcp.api_key`, and restart the daemon (the command is printed). With
   no passkey yet, it shows the Add a passkey address and the
   `approvers approve --code` command, and waits for one.
3. **Proof.** Sign in to the console with your passkey. Setup waits until that
   sign-in is recorded; nothing is removed before it.
4. **What still dials TCP with the key.** It looks for the key, and for
   `console.yaml`, in Claude Code's settings and MCP registration, Codex's
   hooks and MCP registration, the Claude app's MCP bridge, and your other
   Seamless configs, names each, and goes on only if you say so (with no
   terminal, it stops). Scripts on other computers and A2A clients are not
   seen: MCP over TCP names the socket to them.
5. **Retire.** As an administrator, delete the `api_key` line under `mcp:` in
   the daemon's config and restart it. Once the daemon serves without a key,
   setup takes the key out of `console.yaml`, keeping every other line, and
   points its `server_url` from `127.0.0.1` to the passkey origin
   (`localhost`), so `console-open` opens the sign-in page where a passkey
   works. TCP then serves the console's sign-in, its pages and `/healthz`; MCP
   over TCP is refused with the socket's address.

A run that stops anywhere - a timeout after 15 minutes, a crash, Ctrl-C -
continues from where things are when you run `seamlessd setup` again, and until
the key is gone each run asks for a fresh passkey sign-in first.

With every passkey lost, the login page's **Show a code** is the way in: run
the command it shows, `console-open --sign-in --code`, as the daemon's account at
a terminal ([below](#seamlessd_console_open)); it shows which browser the code
names and asks before it signs it in. Run setup again on a retired key to bring a
key back: it mints a fresh one (never the old one), writes it into
`console.yaml`, and waits for root to copy it into the daemon's config; it never
prints the key.

## seamlessd install-hooks {#seamlessd_install_hooks}

```bash
seamlessd install-hooks [--client claude|claude-desktop|codex|all|detect] [--settings PATH] [--codex-hooks PATH] [--desktop-config PATH] [--config PATH] [--url BASE] [--server-url URL] [--api-key KEY] [--seam PATH] [--mcp=false] [--skills=false]
```

Wires the selected install target(s) to Seamless: merges the hook entries into
each hook client's file (Claude Code `settings.json`, Codex `hooks.json`),
registers the MCP server (via the client's CLI, or for the Claude app chat
surface by editing `claude_desktop_config.json` directly), and installs the
embedded skills into each hook client's skill home - `seam-onboard` always, and
`seam-research` only while its
[optional feature](/reference/console/#optional-features) is on. A skill whose
feature is off is not installed, and a copy this installer previously delivered
is removed, so an agent never reads about tools the server no longer exposes.
The `claude-desktop` target is the chat surface's MCP bridge only -
it has no hooks and no skills - so selecting only it together with
`--mcp=false` is an error rather than a silent no-op.

| Flag | Default | Meaning |
|---|---|---|
| `--client` | `detect` | Which target(s) to wire: `claude`, `codex`, `claude-desktop`, a comma list of those (`claude,claude-desktop`), `all` (every target this platform can host), or `detect` (the targets present on this machine). With the flag omitted on a terminal, a multi-select menu prompts, defaulting to the detected set; non-interactive runs detect without prompting. With nothing detected, `detect` is an error, never a silent Claude Code default. |
| `--settings` | `~/.claude/settings.json` | Target Claude Code settings file, created if absent. Point it at a project-scoped `.claude/settings.json` to scope the hooks to one repo. |
| `--codex-hooks` | `$CODEX_HOME/hooks.json`, else `~/.codex/hooks.json` | Target Codex hooks file, created if absent. |
| `--desktop-config` | the app's per-OS location | Claude app `claude_desktop_config.json` to register the chat-surface bridge in (macOS `~/Library/Application Support/Claude/`, Windows `%APPDATA%\Claude\`). |
| `--config` | `$SEAMLESS_CONFIG`, then the search path | The config file to load and to wire every hook and MCP registration to, as `--config <absolute path>`. It must exist. Use it for a [shared client config](/guides/peer-auth/#restricted-local-accounts) that several accounts' hooks read. |
| `--url` | derived from the config addr | Base URL of the daemon, for this run only. It does not change the config file. |
| `--server-url` | none | Install as a **client** of the `seamlessd` at this base URL: writes `role: client`, `server_url`, and `mcp.api_key` into `~/.config/seamless/seamless.yaml` on first run, and wires every client against that URL. |
| `--api-key` | `$SEAMLESS_MCP_API_KEY` | That server's bearer key, for `--server-url`. On its own it is an error, not a silent no-op - a server reads its own key from its config file. |
| `--seam` | sibling of this binary, else `seam` on PATH | Path to the `seam` CLI baked into the command hooks. |
| `--mcp` | `true` | Register the MCP server via the client CLI (`claude mcp add-json --scope user` / `codex mcp add`). |
| `--skills` | `true` | Install the embedded skills for each wired client. A failure here degrades to a warning - skills must not cost the daemon bootstrap. |

It generates `mcp.api_key` on a true first run under the same rule as `serve`,
and refuses to run when an existing config leaves the key empty, since the key
is what the hooks authenticate with. The loaded config path (`--config`, else
the search's) is made absolute and passed to command hooks, `seam mcp-headers`
and `seam mcp-proxy` as `--config`, so they read that file from any working
directory - `seam` itself never reads a `./seamless.yaml`. A `--seam` binary
that cannot be found is a printed warning, not an error - the hooks would fail
at fire time, so it says so now.

With `--server-url`, that first-run step writes a *client* config instead -
`role: client`, `server_url`, `mcp.api_key`, and deliberately no `data_dir` -
and the run wires every selected target against the server's URL. The same
bootstrap rule applies: a config file already in the search order is never
edited, and the error names the exact lines to add by hand. The one file that
is not an error is one that already says exactly this, which is what keeps
re-running the installer from failing. No key is generated, because a client's
key belongs to the server it dials: it comes from `--api-key`, else
`$SEAMLESS_MCP_API_KEY`, and neither is an error. A client also has no local
database to read the [optional features](/reference/console/#optional-features)
from, so it asks the server over HTTP and falls back to its file/env config
with a warning. The run closes by naming the server it is now a client of.

Under an **https** base URL the wiring changes shape on purpose: every Claude
Code hook becomes a command hook (including `UserPromptSubmit`, otherwise the
one http hook) and the MCP registration becomes the `seam mcp-proxy` stdio
bridge. Claude Code performs http hooks and http MCP connections with its own
client, which has nowhere to be told about `tls.ca_file`; routing both through
`seam` puts them on the CLI's trust store. Under http the shapes are unchanged.

The hook file is written before MCP registration. Claude Code registration stays
best-effort because its current CLI exposes no machine-readable state: a missing
CLI or failed `mcp add-json` prints the exact command to run yourself. Codex is
stricter. It decodes `codex mcp get seamless --json`, leaves an exact enabled
stdio bridge unchanged, repairs an owned disabled/stale bridge with `mcp add`,
and re-reads it before reporting success. A direct-HTTP or other incompatible
entry under `seamless` is not overwritten; remove it explicitly or rerun with
`--mcp=false` to keep that manual transport.

The `claude-desktop` target has no management CLI at all, so its registration
is a merge-preserving edit of `claude_desktop_config.json`: the reserved
`seamless` entry is set to the `seam mcp-proxy` stdio bridge (absolute paths,
no secret - the bridge reads the bearer key from Seamless's config), every
foreign key round-trips byte-for-byte, the file is backed up once before the
first change, and the write is verified by re-reading it. An incompatible entry
under the reserved name is an error naming the in-app fix (Settings >
Developer > Edit Config), and every change ends with a restart notice - the
app reads the file only at startup. See
[Claude app chat setup](/claude-app/).

The hook merge preserves unknown keys and foreign entries, replaces marked stale
definitions, adopts only recognizable legacy Seamless URLs/commands, deduplicates
owned entries, and backs the file up once before the first change. An arbitrary
executable merely containing `hook <event>` is foreign. An already-current file
is reported as up to date and left untouched. Each event reports `added`,
`updated`, `adopted`, `deduped`, or `unchanged`, or for an event the managed
settings carry, `managed` or `removed`.

### Claude Code's managed settings {#seamlessd_install_hooks_managed}

An administrator or a device-management tool can deliver Claude Code settings
in a system-wide file the user cannot edit: `managed-settings.json`, plus any
`*.json` drop-ins in `managed-settings.d/`, in `/Library/Application
Support/ClaudeCode/` on macOS, `/etc/claude-code/` on Linux and WSL, and
`C:\Program Files\ClaudeCode\` on Windows. Claude Code runs the hooks in those
files *alongside* the ones in `settings.json` rather than instead of them. So a
Seamless hook in both places fires twice, and the briefing is injected twice.

`install-hooks` reads those files first. An event they already carry is left to
them (`managed`), and a copy in the target file from an earlier install is
removed (`removed`); every other event installs as usual. A `--settings` that
names one of those files is the exception: an administrator writing the
managed hooks there is installing them, so that file is not counted as
carrying them, and a second run keeps its hooks. A `managed` row names
the file the events run from. If a managed definition differs from what this
install would write - another `seam` path, an older shape - the row says so
instead of changing it, since the file belongs to whoever deployed it. The
other managed sources (the claude.ai admin console, MDM profiles, the Windows
registry) are not files Seamless can read, so they are not consulted.

For Claude Code, seven events are installed together: `SessionStart`,
`UserPromptSubmit`, `SessionEnd`, `PostToolUse`, `SubagentStart`,
`SubagentStop`, and
`PermissionRequest`. All are command hooks that run `seam hook <event>` (exec
form, no shell) except `UserPromptSubmit`, which is an http hook - Claude Code
will not run an http hook for SessionStart at all, and at SessionEnd a
fire-and-forget request races process teardown, so the findings harvest would
often be lost. The Codex profile is five shell-string command hooks. Both
profiles include safe constraint injection and parent-only lifecycle handling
for subagents; the [hooks reference](/reference/hooks/) has both tables.

## seamlessd client-config {#seamlessd_client_config}

```bash
seamlessd client-config [--redact]
```

Runs on the **server** and prints exactly what you paste on another machine to
make it a client of this daemon. It reads the resolved config and formats it:
it mutates nothing, opens no connection, and contacts no network.

The output is the server URL, the bearer key, the minimum client version, and
three paste-able commands - the macOS/Linux installer one-liner
(`SEAMLESS_SERVER_URL` + `SEAMLESS_MCP_API_KEY`), the PowerShell equivalent,
and the manual `install-hooks --server-url ... --api-key ...` form for a
machine that already has the binaries. `--redact` substitutes a placeholder for
the key so the block is safe to paste into a ticket or a chat; the shape of
every command is unchanged.

Every line it prints carries a URL and a key, so the refusals matter more than
the output. It errors rather than printing a command that looks right and
cannot work:

| State | Why it refuses |
|---|---|
| `role: client` | This install has no clients of its own to pair. The message names the server to run it on instead. |
| `mcp.api_key` empty | A client would have nothing to authenticate with. It names the config file and `openssl rand -hex 32`. |
| `server_url` resolves to loopback | Pasted on a second machine that command dials *that* machine's port 8081 and fails as a connection error from the wrong end of the network. It names the fix (`server_url` plus `addr: 0.0.0.0:8081`, then restart) and points out that a second user **on the same box** needs none of that and can pair against `http://127.0.0.1:8081` directly. |
| `server_url` names a user | Every command would carry it, password and all, to the other machine and its shell history, `--redact` included, and Seamless uses no user. It names the config file: leave `scheme://host:port`, then restart. |

Plain `http://` is a warning, not a refusal - it is a legitimate choice on a
trusted LAN, and the operator already had to widen the bind and name
`server_url` to get here. What it must not be is silent: the key in those
commands, and every memory fetched with it, cross the network in the clear.

See [Share one daemon across your computers](/guides/network-install/) for the whole
procedure.

## seamlessd uninstall {#seamlessd_uninstall}

```bash
seamlessd uninstall [--client claude|claude-desktop|codex|all|detect] [--dry-run] [--yes] [--purge]
```

Reverses a full install on any supported OS: stops and removes the per-user
service and what [the updater](/reference/service/#the-updater) leaves behind
(a Linux update unit still running, the Windows update task), strips the
Seamless hook entries, deregisters the MCP server
(including the chat surface's `claude_desktop_config.json` entry - only the
reserved `seamless` key is removed, everything else stays byte-for-byte),
removes the installed skills, and deletes the binaries. Config and the data dir
(`~/.seamless` - memories and notes are markdown that outlive the program) are
kept unless `--purge` is passed. Every external step is best-effort: an
already-gone file or a missing client CLI is a note, never a failure, so
uninstall is idempotent and safe to re-run.

Hook removal uses the installer's same classifier: current, marked-stale, and
recognizable legacy Seamless definitions are removed; foreign definitions are
preserved. Seamless hooks in [Claude Code's managed settings](#seamlessd_install_hooks_managed)
are not Seamless's to edit, so uninstall leaves them and names them in a
`managed` row: ask whoever deployed that file to remove them. Skill removal is scoped to `seam-onboard`, `seam-research`, and the
one-shot delivery marker in each selected client's skill root.

Under `role: client` two steps narrow rather than run. The service block reports
`not installed` and no teardown happens - a client registered none, and running
the teardown anyway would stop the daemon of a *server* sharing the same box.
And `--purge` deletes only the config directory: the `~/.seamless` a client's
config resolves to is a default it never wrote, and on a box converted from a
server it is the server's corpus.

| Flag | Default | Meaning |
|---|---|---|
| `--client` | `all` | Which target(s) to remove hooks/MCP/skills for: `claude`, `codex`, `claude-desktop`, a comma list, `all`, or `detect`. `claude-desktop` scopes the run to the chat surface's desktop-config entry. |
| `--dry-run` | `false` | Print what would be removed and exit without changing anything. |
| `--yes` | `false` | Skip the confirmation prompt. |
| `--purge` | `false` | Also delete the config dir (`~/.config/seamless`) and data dir (`~/.seamless`). |
| `--settings` | `~/.claude/settings.json` | Claude Code settings file to remove hooks from. |
| `--codex-hooks` | `$CODEX_HOME/hooks.json`, else `~/.codex/hooks.json` | Codex hooks file to remove hooks from. |
| `--desktop-config` | the app's per-OS location | Claude app `claude_desktop_config.json` to remove the chat-surface bridge from. |
| `--url` | derived from the config addr | Base URL the hook entries were installed with. |
| `--install-dir` | `$SEAMLESS_INSTALL_DIR`, else `~/.local/bin` | Directory the binaries were installed to. |
| `--mcp` | `true` | Also deregister the MCP server (`claude`/`codex mcp remove`). |

## seamlessd update {#seamlessd_update}

```bash
seamlessd update [--check] [--dry-run] [--url URL] [--config PATH]
```

Upgrades Seamless in place to the newest published release by running that
release's own installer for this OS - the same script a fresh
`curl ... | sh` / `irm ... | iex` install runs. There is deliberately no second
upgrade implementation: after verification, `update` pipes that script to `sh`
(or `powershell`), so binaries are swapped by rename, the service restarts on
the new build, and hooks are reconciled exactly as [Install & deploy](/install/)
describes for a re-run.

"Newest" is the highest version on GitHub's release list, never GitHub's
"latest" (a backport can make that an older one). Before anything executes,
`update` re-reads the target release, then verifies its installer script and
its `checksums.txt` against the **Sigstore bundles** published alongside them,
in-process, against this repository's release workflow on that release's exact
tag - so an older signed installer or manifest cannot pass as the one asked
for. Verification failure is fatal, with no fallback. Every fetch is
HTTPS-only, including every redirect hop. The installer then runs pinned to the
verified manifest (`SEAMLESS_CHECKSUMS_SHA256`).

On an install the installer made, `update` also backs the instance up first
(`~/.seamless/backups/pre-update-v<old>-<time>.tar.gz`, the newest two kept),
counts the update only once the new release answers `/healthz` as a freshly
started daemon that is still the same one ten seconds later, and otherwise
rolls back to the release you had. Each such run is recorded in
`~/.seamless/update/`. A daemon that [updates itself](/updating/#automatic-updates)
runs this same engine unattended. Already on the newest release, it says so
and changes nothing. [Update & uninstall](/updating/) has the details.

`update` refuses while a [restore](#seamlessd_restore) is waiting to finish
(`~/.seamless/restore/pending.json` exists), since it would back the data up and
replace the binary under it: restart Seamless to finish the restore, or cancel
it in the console's **Settings > Backups**, then update. Automatic updates and
**Update now** wait the same way, and an attempt the daemon had already started
stops with the refusal `restore_pending`.

With a config that turns peer_auth on, `update` runs no installer, which would
set up a per-user service that cannot host it. A system daemon that
[updates itself](/guides/peer-auth/#updates), run as the daemon's account
(`sudo -u _seamless /opt/seamless/bin/seamlessd update --config /opt/seamless/etc/seamless.yaml`),
is updated the way its own updater does it: the archive checked against the
verified `checksums.txt`, `seamlessd` and `seam` swapped in place, the daemon
ended for its service manager to start again, and the same backup,
confirmation and rollback. Any other peer_auth layout is refused.

| Flag | Default | Meaning |
|---|---|---|
| `--check` | `false` | Report installed vs the newest release and exit without changing anything. It reads GitHub's release list - the highest version that is not a draft, a prerelease, or still uploading its assets - and adds what the daemon's [update check](/updating/#automatic-update-checks) and [automatic updates](/updating/#automatic-updates) have recorded: the mode and the last check; for a daemon that updates itself, the release it installs next with its soak (`target`) and the deadline (`pending`); an update under way; the last attempt with the updater's error, log and backup (and a `fix` row: `seamlessd install-hooks` after one that applied with warnings, or the owner's action - `seamlessd restart`, or `seamlessd update` by hand - after one the updater's gates refused while the daemon still runs the release it started from); a hold, a pause, blocked releases, a backoff, and the rollback drill while it is on. These come from the records, so they print even when the release list cannot be read. It asks even when `update.check` is `false`, because running it is you asking. |
| `--dry-run` | `false` | Print the target, the installer's source and signature, the install dir, and the backup and rollback plan, plus the equivalent one-liner, without downloading, installing or recording anything (it reads the release list and the target release). |
| `--url` | the target release's installer | Run the installer at this https URL instead: TLS only, with a printed warning, your environment as is, and no backup, rollback or record. |
| `--config` | `$SEAMLESS_CONFIG`, then the search path | The config the data dir and the daemon's address come from. |

The installer's env knobs pass through, and a knob you set wins:
`SEAMLESS_VERSION=0.3.0 seamlessd update` pins a version (older ones too),
`SEAMLESS_INSTALL_DIR=...` retargets, exactly as the curl installer does.
`--auto` with `--attempt`, `--from`, `--to` and `--why` is the command line a
daemon that updates itself starts its updater with, outside the service
([The updater on your OS](/reference/service/#the-updater)). It is not for
interactive use, and it refuses `--check`, `--dry-run` and `--url`.

## seamlessd map-repo {#seamlessd_map_repo}

```bash
seamlessd map-repo --project SLUG [--path DIR]
```

Adds an entry to the `repo_project_map` setting, so an agent whose working
directory is under that path resolves to that project - in the hooks and in
`session_start`. This is what makes a briefing arrive scoped to the right
project without the agent passing `project` anywhere.

Mostly you will not need it: a git repo maps itself on its first session, taking
the slug from the repo root's directory name. Run `map-repo` to override that
derived slug, or to map a directory that is not a git repo.

`--project` is required. `--path` defaults to the current directory and is made
absolute. The command also ensures the project exists, so mapping a new slug
registers it. Writes straight to the database; no running daemon needed.

## seamlessd unmap-repo {#seamlessd_unmap_repo}

```bash
seamlessd unmap-repo --path DIR [--dry-run]
seamlessd unmap-repo --stale [--dry-run]
seamlessd unmap-repo --host NAME --path DIR [--dry-run]
```

Removes this machine's repo mappings - the inverse of `map-repo`. `--path`
removes the mapping for exactly that directory (made absolute; a directory
nested under a mapped repo is not a match). `--stale` removes every mapping on
this machine whose path no longer exists on disk: the entries `doctor`'s
`repo map` check warns about after a repo is moved, renamed or deleted. Pass
exactly one of the two.

| Flag | Default | Meaning |
|---|---|---|
| `--path` | - | The repo root whose mapping to remove. Unmapped on this machine is an error, and nothing is removed. |
| `--stale` | `false` | Remove every local mapping whose path is gone. Only a clean "does not exist" counts; a path that cannot be stat'd for another reason is kept. |
| `--host` | this machine | With `--path`: remove that host's mapping instead. The path must be absolute on that host, whatever this machine runs: `/home/me/app`, `C:\repos\app` and `\\server\share\app` all work. Not valid with `--stale`. |
| `--dry-run` | `false` | Print what would be removed and change nothing. |

Only the route goes. The project stays, with its memories, notes and tasks,
and a repo still on disk maps itself again on its next session - so to move a
repo to another project, `map-repo` its root there rather than unmapping it.
The table row and the legacy `repo_project_map` entry are removed together, so
the removal survives a daemon restart. Writes straight to the database; no
running daemon needed.

Without `--host`, mappings belonging to other hosts are never touched. On a
[shared daemon](/guides/network-install/) a client machine's mappings live in the
server's database, where that machine's own `unmap-repo` cannot reach them, so
run `--host NAME --path DIR` beside the server's database instead. It names
the mapping exactly and never checks the path: another machine's disk cannot be
read from here, which is also why `--stale` refuses `--host`. `doctor` lists
those rows as not verifiable from this machine. Removing another host's mapping
leaves this machine's mapping for the same path alone.

## seamlessd retire-project {#seamlessd_retire_project}

```bash
seamlessd retire-project --project SLUG [--dry-run]
seamlessd retire-project --project SLUG --undo [--dry-run]
```

Marks a project retired - the cleanup for one minted by mistake, such as a
`<slug>-2` left behind when a repo moved before moved repos were adopted, or
the project a moved-and-renamed repo registered before you pointed it back with
`map-repo`. It sets the same flag a gardener split stamps on its emptied source:
the project row stays, nothing is deleted, and the console lists it as retired.
`--undo` clears the flag.

Retiring moves nothing, so it is refused while the project is still in use.
Every blocker is printed with its remedy, and nothing changes:

- a repo mapping on any host still routes sessions to it - `map-repo` the path
  to the right project, or `unmap-repo` it (`--host NAME` for another
  machine's mapping);
- it holds active memories, notes, or open or in-progress tasks - move them to
  the project they belong in, or archive or close them.

Its sessions, closed tasks and archived memories are history and never block.
Un-retiring has no guard. Both directions are idempotent: a project already in
the requested state is reported and left alone.

| Flag | Default | Meaning |
|---|---|---|
| `--project` | - | The slug to retire. Required; an unknown slug is an error. |
| `--undo` | `false` | Clear the retired flag instead. |
| `--dry-run` | `false` | Print what would happen, including any blockers, and change nothing. |

Writes straight to the database; no running daemon needed.

## seamlessd family {#seamlessd_family}

```bash
seamlessd family list
seamlessd family add <name> <slug> [<slug>...]
seamlessd family remove <name> [<slug>...]
```

Manages the `project_families` setting: named groupings whose members surface
each other's recent findings in briefings. Use it when two projects are really
one body of work and an agent in either should see what happened in the other.

Members are **project slugs, not repo paths** - resolve a repo to its slug with
`map-repo` first. `remove` with no slugs removes the whole family; with slugs it
removes just those members. `rm` is accepted as an alias for `remove`.

Adding a slug that is not yet a registered project prints a warning but
succeeds: the membership starts taking effect once an agent opens that repo and
registers it.

## seamlessd approvers {#seamlessd_approvers}

```bash
seamlessd approvers enroll-code <passkey|touchid> --key <fingerprint>
seamlessd approvers add
seamlessd approvers approve [--code CODE] [--yes]
seamlessd approvers list [--json]
seamlessd approvers remove <id>
seamlessd approvers reset --yes
```

Manages the owner's approver credentials: the passkeys and Touch ID keys that
can approve an agent's access to a locked project (see the
[configuration reference](/reference/configuration/)). Every verb opens the
database directly, so run it as the daemon's own account, for example
`sudo -k -u _seamless env SEAMLESS_CONFIG=/opt/seamless/etc/seamless.yaml
seamlessd approvers list`. Becoming that account is the proof: no route, over
the socket or with the TCP key, can mint a code or remove a credential, except
that a Mac re-enrolling with `seam touchid enroll` retires its own earlier
Touch ID key, which signs for it.

- `enroll-code` mints a one-time enrollment code for one key, named by its
  fingerprint (`--key`). It works once, for 10 minutes, only for that key; only
  its hash, which binds the key, is stored. You rarely type this by hand. For a
  passkey, create it on the console's Approvers page (`/console/approvers`, on
  [the passkey origin](/guides/peer-auth/#the-passkey-origin)), which shows the
  command with the new key's fingerprint and takes the code. For a Touch ID key, the Seamless Approve window does the same. Every
  credential needs a code, however many are enrolled: an enrolled passkey never
  vouches for a new one.
- `add` prints the no-secret Add a passkey address (`/console/passkeys/add`,
  with `console.sign_in: passkey`), and at a terminal goes on to `approve`.
- `approve` shows the waiting passkey's name, fingerprint and relying party,
  asks `[y/N]`, and adds exactly that passkey. `--yes` is for scripts. A code
  works once, for 10 minutes. Run it as the daemon's account under peer_auth,
  else as yourself.
- `list` prints the enrolled credentials: id, kind, name, how each was enrolled
  and when it was last used. `--json` prints them as JSON. It also names waiting
  passkeys (`pending` in JSON, only while one waits).
- `remove` deletes one credential. Leases it already approved are untouched; the
  browsers it had signed in are signed out.
- `reset --yes` deletes every credential and every unused code, revokes every
  live lease, signs out every signed-in browser and drops every waiting passkey,
  in one transaction: for when a credential may be in someone else's hands.

With no credential enrolled, `seamlessd doctor` warns while a project is
locked: only the terminal path can approve then. Both doctor and `list` say how
to enroll one: the console's Approvers page, which shows the command for a
passkey's code, and, on a Mac whose daemon serves the `peer_auth` socket, `seam touchid enroll`
in your own terminal.

## seamlessd approve {#seamlessd_approve}

```bash
seamlessd approve <request> --for <session|1h|4h> --digest <facts digest> [--access <read-only|read-write>]
seamlessd approve <request> --deny
```

The terminal approval path's commit for an access request to a locked
project. You do not normally type it: `seam approve`, in your own terminal,
shows the request's facts, asks for the duration, and runs it as
`sudo -k -u <daemon account> seamlessd approve ...`. The `sudo -k` asks for
your Touch ID or password every time, and becoming the daemon's account is
the proof - this command refuses unless it runs as that account (the data
directory is its own and `0700`, and it is not the owner's account).

`<request>` is the short id an agent's refusal names (`R-7F3K`) or the full
id. `--for` is the lease's length: until the session ends (at most
`peer_auth.max_lease`), one hour or four hours. `--digest` is the digest of
the facts you were shown; when they changed since (another request from the
same agent, a mark on the session), the approval is refused and you look
again. `--deny` closes the request; the project stays locked for that
session. A request naming a shell, a terminal, a multiplexer or a login
process (one an older release filed) is never approved: deny it.

It answers in one line: `Approved R-7F3K: Claude Code 2.1.319 (pid 4242) may
read and write example-finance until 14:53.` (`until the session ends, at
the latest ...` for `--for session`; a read grant names the asking project,
and says the content may be copied into its memories), or `Denied R-7F3K:
example-finance stays locked for that session.` When nothing changed it exits
1 and says why: the facts changed (`the facts of R-7F3K changed since they were
shown: run seam approve again and look at them first`), the request expired
before the decision reached it (`R-7F3K expired at 14:05, before this
decision: nothing changed ...`), or it was decided meanwhile (`R-7F3K was
already approved: nothing changed`).

With read grants on (`isolation.requests`) it decides the two other kinds as
well. `--access read-write` approves a grant between confidential projects
read and write (read-only is the default, and `--access` names only an access
the request offers): `Approved R-7F3K: pid 200, working in example-hr, may
read and write example-payroll until 15:05; content from example-payroll may
be copied into example-hr, and it may change example-payroll's tasks, memories
and notes; content from example-hr may be copied into example-payroll.` A move
request moves the agent into the project for good, as a new session that
continues the one it left: `Approved R-7F3K: Claude Code 2.1.319 (pid 4242)
moved from example-ops into example-hr for good, as session
cc/08eb9006-5c473accb1746e1b@example-hr.`, with `; its lock is open to it
until ...` for a locked project, whose lease the same approval records. A
denied move answers `Denied R-7F3K: the agent stays in example-ops, and
example-hr stays closed to it.`

## seamlessd devices {#seamlessd_devices}

```bash
seamlessd devices code [--json]
seamlessd devices list [--json]
seamlessd devices revoke <id|name>
```

The computers paired with this server: relays (macOS and Linux clients) and key devices (Windows, or a machine where you have no root). Only on a server that serves other computers (TLS, a bind beyond loopback, or a `server_url` naming another host); elsewhere every verb refuses. Each verb opens the database directly: run it on the server, as the daemon's own account under peer_auth.

- `code` mints a one-time pairing code (10 minutes, one computer; only its hash is stored) and prints what the other computer needs: this server's address, its certificate's SHA-256 fingerprint (as `openssl x509 -noout -fingerprint -sha256` prints it), the code and the client one-liners, which carry the fingerprint (`SEAMLESS_SERVER_FINGERPRINT`) so the other computer's setup compares it before it sends the code. The code works once, so the line left in that computer's shell history is dead after use. Where `server_url` is plain http to another host it refuses (`pairing needs TLS here`) and mints nothing. Where `server_url` names a user it refuses too (`server_url names a user: remove it, Seamless uses none`), giving both reasons when both hold, and mints nothing; the address it prints never carries a user. Where the address is this computer's loopback (no `server_url` on a wildcard bind) it mints the code, for a client on this computer, and says another computer cannot use the one-liners and that `server_url` is the fix; `--json` adds `"loopback": true`. Without peer_auth the code is the approval; under it the pairing waits for your passkey on the console's Devices page.
- `list` prints every device: id, name, kind, status, fingerprint or key prefix, last seen.
- `revoke` revokes a paired device (refused from its next request; the daemon ends its running requests within 30 seconds, and a relay's leases, read grants and waiting access requests at once) or denies a pairing that waits, named by its id or by a live device's name. [The Devices page](/guides/network-install/#devices) does the same in the console.

## seamlessd relay {#seamlessd_relay}

```bash
seamlessd relay serve [--data-dir DIR] [--socket PATH] [--socket-mode MODE] [--socket-group GROUP]
seamlessd relay pair --server URL --code CODE [--fingerprint SHA256] [--ca-file FILE] [--name NAME] [--replace] [--json] [--data-dir DIR]
```

The relay serves the socket agents dial beside a peer_auth daemon and forwards
each request to the paired server, so the computer holds no key. macOS and
Linux only. Both verbs run as the relay's own account and refuse root, your
account, and any layout another account could change.

- `serve`: the socket defaults to `/opt/seamless/run/seamlessd.sock`, mode
  0666. It refuses while unpaired, and answers 502 with the reason when the
  server is unreachable or presents another certificate, never from memory.
  Under peer_auth the server judges each uid once you map this relay in
  `peer_auth.relays`, and refuses it until then; without peer_auth every caller
  is you. Restrict the socket: `--socket-mode 0660 --socket-group
  _seamlessowner` (you and the relay's account), as `seamlessd setup` installs
  it.
- `pair`: the code comes from the server's Devices page or `seamlessd devices
  code` (`--code -` reads it from stdin). `--fingerprint` makes sure the code
  goes only to that certificate; `--ca-file` names the root the server's
  certificate chains to, copied into the data dir. Under peer_auth the pairing
  waits for your passkey on the Devices page, which shows the same key
  fingerprint. `--name` is the name the relay speaks for, which the Devices page
  shows and `peer_auth.relays` maps (this computer's host name by default).
  `--replace` pairs again after you revoked the old device;
  `--json` prints the public facts. Run it by its root-owned path,
  `/opt/seamless/bin/seamlessd`, with `--code -`, and paste the code.

Its data dir is `/opt/seamless/relay` (the relay's account, 0700; `--data-dir`
names another): `relay.json` (0600) holds the pairing and the relay's key.
Doctor reports the relay, and warns when root's copy of seamlessd is not the one
you run (`relay.binary`); run it as the relay's account to see the pairing
too. [The relay's root step](/guides/network-install/#the-relays-root-step)
has the account, the layout and every step.

## seamlessd console-open {#seamlessd_console_open}

```bash
seamlessd console-open [--browser APP]
seamlessd console-open --unlock PROJECT [--code CODE] [--for DURATION]
seamlessd console-open --sign-in [--code CODE]
```

Opens the console in a browser, already authenticated. It renders a one-shot
self-submitting login page to a `0600` temp file and opens it; the page POSTs
the static key to the console's login endpoint, which sets the session cookie
and redirects into the console - so you land on an authenticated page without
pasting a key.

`--browser` targets a specific browser application (for example
`"Google Chrome"`, so an agent driving Chrome gets the auth cookie even when
another browser is the default). It is **macOS only** and is rejected with an
error on other platforms rather than silently opening the default browser.

It refuses to run when `mcp.api_key` is empty, or when the server does not
answer `/healthz` within two seconds - the page has nowhere to POST otherwise.

Under passkey sign-in (`console.sign_in: passkey`) it opens the console's
sign-in page and writes nothing: no temp page, no key, no link that signs a
browser in. There is no local shortcut around the passkey, since any such value
is one every process of yours could read first.
Any HTTP response counts as reachable, including a degraded 503.

While `peer_auth` is on, it signs in on
[the passkey origin](/guides/peer-auth/#the-passkey-origin) instead of the server URL: the
console's cookie belongs to one host, and browsers refuse passkeys on an IP
address such as `127.0.0.1`. A client's config carries no `peer_auth` block, so
there it asks the daemon for that origin, and uses `localhost` only when the
daemon is on this machine. A socket `server_url` is refused: a browser cannot
dial one, so the command says where the console is and to sign in with the
daemon's key, or to run it with a config that names the TCP `server_url` and
`mcp.api_key` (set `SEAMLESS_CONFIG`).

`--unlock PROJECT` is the terminal's half of opening a locked project in one
browser's console without a passkey. The browser begins it: open the project's
unlock page (`/console/unlock/PROJECT`) in the browser you sign in to the
console with and choose **Approve from a terminal**. The page shows a code and
the command that approves it, run as the daemon's own account through `sudo -k`
so it asks for your password or Touch ID every time:

```bash
sudo -k -u _seamless seamlessd console-open --unlock example-finance --code 7KQ2-M9XD
```

It refuses unless it runs as the daemon's account (the same check as
`seamlessd approve`) and `PROJECT` is locked, by the config file or from the
console. It finds the unlock of `PROJECT` waiting for that code, makes it live
in the browser that showed the code, and says so (`Unlocked example-finance
for 3 hours, until 17:04, in the browser that showed code 7KQ2-M9XD.`); the
page notices within a few seconds and shows the project unlocked. It prints
nothing secret and launches no browser (`--browser` is refused with
`--unlock`).

`--code` takes the code in any case, with or without the hyphen. A code works
once, within ten minutes of the page showing it, and only for its own project:
a malformed, mistyped, used, expired or other project's code is refused and
changes nothing. Without `--code` the command names the page to open and asks
for the code when standard input is a terminal; anywhere else it refuses,
naming `--code` and the page. `--for` is how long the unlock lasts once
approved: at most, and by default, the console lease of three hours (shorter
when `peer_auth.max_lease` is).

`--sign-in` is the terminal's half of a terminal sign-in, the way into the
console under passkey sign-in with no passkey at all. The browser begins it:
**Show a code** on the console's sign-in page gives that browser a cookie bound
to its key and shows a code with the command that approves it, run as the
daemon's account under peer_auth (as you, without it):

```bash
sudo -k -u _seamless env SEAMLESS_CONFIG=/opt/seamless/etc/seamless.yaml /opt/seamless/bin/seamlessd console-open --sign-in --code 7KQ2-M9XD
```

Before it signs anything in, it shows the browser the code names, as the console
recorded it when that browser chose **Show a code**: when it began, the address
its request came from (another computer, or this computer, which a proxy in
front of the console also is) and its User-Agent, as plain text that cannot act
on your terminal, each run of spaces shown as one, so that a long one cannot wrap
into what looks like another of these lines. Then it asks
`Sign in this browser? [y/N]`. Anyone who reaches
the console can show a code, so say yes only to a code your own browser shows
you now, never to one someone sent you. Yes signs in that browser, for 7 days,
as a passkey sign-in lasts, and says so; the page goes on by itself. No, Enter
or the end of input signs nothing in and leaves the code waiting. The code works
once, within ten minutes, and signs in no other browser: whoever else saw it, or
read the cookie on another port, gets nothing. A malformed, mistyped, used or
expired code is refused before anything is asked, and changes nothing. Because
it asks, it runs only at a terminal (over ssh, `ssh -t`): anywhere else it
refuses, with or without `--code`, and no flag skips the question. Without
`--code` it asks for the code first. It refuses `--browser`, `--unlock` and
`--for`, and a config whose `console.sign_in` is `key`.

An unlock is per browser: the one that showed the code reads the project, and
another browser, the TCP key and every agent stay locked out, whoever else saw
the code. It is a console cookie, never a lease, so it never opens the project
to an agent - the owner's own agents included. The console's access page lists
every live unlock with **Lock again**, and each agent lease with **Revoke**.
The unlock page can also unlock with a passkey, with no terminal at all.

## seamlessd start / stop / restart / status {#seamlessd_service}

```bash
seamlessd start      # or: stop | restart | status
```

Control the installed background service without remembering each platform's
service manager. `start`, `stop`, and `restart` act on the LaunchAgent (macOS),
the systemd `--user` unit (Linux), or the Scheduled Task (Windows); `status`
prints that manager's own state output.

These control an **already-installed** service - they do not create one. If it
was never installed, they exit with a hint to run the installer (or `make
install` from a clone) rather than a cryptic launchctl/systemctl/schtasks error.

`restart` is in-place and fast (on macOS `launchctl kickstart -k`, falling back
to a fresh load if the job was unloaded). `stop` fully stops the service - on
macOS the LaunchAgent has `KeepAlive`, so this unloads it rather than letting it
be resurrected. When the service manager refuses - starting a running service,
stopping a stopped one - the command prints a "could not" row with the
manager's own message and still exits 0.

On Windows, `schtasks /End` returns about a second before Task Scheduler ends
the daemon's process, so `stop` and `restart` then wait up to 15 seconds for it
to let go of the data directory's `seamlessd.lock`. `stop` reports the service
stopped only once that process is gone; `restart` runs `schtasks /Run` either
way, and its new daemon waits for the data directory itself. A process still
holding the lock after 15 seconds is not the task's own - a serve started by
hand, or a daemon on another data directory that this shell's
`SEAMLESS_CONFIG` or `SEAMLESS_DATA_DIR` names - so it is reported by PID and
left running. If the config cannot be loaded, both say so and skip the wait.

From a clone, `make start` / `stop` / `restart` / `status` wrap these exactly.

## seamlessd version {#seamlessd_version}

```bash
seamlessd version
```

Prints the version, commit, build date, and whether this is a `release` build
(made by the release pipeline) or a `source` build (everything else), e.g.
`seamlessd 0.7.2 (commit 1a2b3c4, built 2026-10-09T12:00:00Z, release build)`.
`-v` and `--version` are aliases. Only a release build checks for updates by
default ([automatic update checks](/updating/#automatic-update-checks)).

Version, commit, and build date are link-time metadata stamped by the Makefile
and the release build. A binary built without them reports what the Go
toolchain recorded instead: `go install github.com/arctop/seamless/cmd/...@vX.Y.Z`
reports that version with commit and build date `unknown` (the module proxy
carries neither), and a plain `go build` in a git checkout reports the
toolchain's version for the checkout (a pseudo-version past a tag, ending
`-dirty` with uncommitted changes), the short commit, and, for a clean checkout,
the commit time as the build date. The same version string appears in `/healthz`,
the MCP handshake, and the startup log - compare them when you suspect the
daemon is running older code than what you just built.
