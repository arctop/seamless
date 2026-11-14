# Update & uninstall

> The lifecycle journeys that are one command on every OS - an installer install updates itself, seamlessd update swaps in the latest release now, seamlessd uninstall reverses the install, and adding a client later is one more install-hooks run.

The install is one command, and so is everything after it. `seamlessd`
carries its own lifecycle: update, uninstall, and client wiring are the same
commands on macOS, Linux, and Windows, resolving your platform's service
manager and paths for you. The OS-specific detail all lives on
[The service & where things live](https://thereisnospoon.org/docs/reference/service/).

## Update

Seamless is early in its development cycle: releases with improvements and bug
fixes land often. From v0.7.0, an install made by the curl or PowerShell
installer installs them by itself, once a release has been out for a day and
nothing is using the daemon ([automatic updates](#automatic-updates)). Every
other install checks for them and tells you when one is out
([automatic update checks](#automatic-update-checks)). Either way, updating
right now is one command.

**On v0.6.0 or older?** Those releases have no update check and never update
themselves. Run `seamlessd update` once by hand; from v0.7.0 on, an installer
install keeps itself current. A binary at v0.5.3 or older cannot verify today's
releases, so re-run the installer one-liner instead
([why](https://thereisnospoon.org/docs/guides/troubleshooting/#seamlessd-update-refuses-the-release-with-a-signature-error)).

`seamlessd update` is the one command, on every OS. It upgrades in place to the
newest release by running that release's own installer for you - so there is a
single upgrade path to trust, not a second copy of the download-and-swap logic
that could drift from the installer:

```bash
seamlessd update --check   # report installed vs the newest release, change nothing
seamlessd update --dry-run # print what it would install, from where, and how; change nothing
seamlessd update           # install the newest release, confirm it, roll back if it fails
```

"Newest" is the highest version on GitHub's release list, never GitHub's
"latest" (a backport can make that an older one). It honors the installer's
knobs, and a knob you set wins: `SEAMLESS_VERSION=0.7.1 seamlessd update` pins a
release, older ones too, and every run passes the version it installs to the
installer explicitly.

Under the hood it re-reads the target release from GitHub, downloads that
release's installer script (the PowerShell one on Windows) and `checksums.txt`
with the Sigstore bundles the release workflow signed them with, and verifies
both in-process against the release workflow's identity on that release's exact
tag - so an older signed installer or manifest cannot pass as the one you asked
for. It then runs the installer pinned to the verified `checksums.txt`
(`SEAMLESS_CHECKSUMS_SHA256`). That is the same script as doing it by hand,
minus the signature checks:

```bash
curl -fsSL https://github.com/arctop/seamless/releases/download/v0.7.3/install | SEAMLESS_VERSION=0.7.3 sh
```

On an install the installer made, it also backs the instance up to
`~/.seamless/backups/pre-update-v<old>-<time>.tar.gz` (the newest two kept),
counts the update only once the new release answers `/healthz` as a freshly
started daemon that is still the same one ten seconds later, and otherwise
rolls back: the service stops, the database is put back from the backup if the
new release had already migrated it (the migrated file stays beside it as
`seam.db.pre-rollback-<id>`) and the backup holds every console lock the
migrated one does, and the release you had is reinstalled and confirmed.
Memories and notes are never rolled back. Each run is recorded in
`~/.seamless/update/` (`attempt.json`, `attempts.jsonl`, `logs/`). A run whose
installer failed after the new release came up reports "applied with warnings":
run `seamlessd install-hooks`. Already on the newest release, it says so and
changes nothing; pin the release to reinstall it. A source build
(`make update`), a client machine, or a release binary the service does not run
gets the verified installer without a backup, a rollback or a record, and the
output says why. Homebrew installs are refused with the `brew` command.

Your config and `~/.seamless` are preserved, binaries are swapped by rename
(safe while the daemon holds them open), and the service restarts on the new
build. The selected clients are reconciled to the new stable paths: owned
stale hooks and the Codex stdio registration are repaired, current definitions
are untouched, foreign hooks are preserved, and the recurring skill is
refreshed.

From a clone, `make update` builds first and then runs that same command against
your installed copy (`make update CHECK=1` only reports).

### Automatic update checks {#automatic-update-checks}

A daemon installed from a release checks GitHub for new releases by itself: a
few minutes after it first starts, then about every 6 hours
(`update.check_interval`). A restart keeps the schedule it saved rather than
checking again; **Check now** under **Settings -> Updates** in the console
checks at once. The check itself installs nothing. On an install the installer made, a newer
release then installs by itself ([automatic updates](#automatic-updates)); every
other install is only told. When a newer release is out, the daemon tells you:

- New agent sessions get one line in their briefing, for a week after the
  release first shows up, worded as your action rather than the agent's:
  `Seamless v0.7.3 is available (running v0.7.2). Owner action, not a task for
  this session: seamlessd update`. An install that will install the release by
  itself skips that line. A session on another machine of a
  [shared daemon](https://thereisnospoon.org/docs/guides/network-install/) is told the server can update.
- The console shows it on the Home health strip and in
  **Settings -> Updates**, with the newest release, when the daemon last
  checked, the command for your install, and - on an install that updates
  itself - when it will install it.
- `seamlessd doctor` reports it on its `updates` row, and warns when the
  rollback drill's switch is left on or when this machine cannot verify its own
  TLS daemon (either makes every update fail its confirmation).
  `seamlessd update --check` adds what the daemon's check and automatic updates
  are doing, and `seam version` / `seam status` print an extra line.

For a day after the daemon starts on a new version, new sessions hear
`Seamless updated to v0.7.3 (from v0.7.2)` with a link to the release notes,
and the console shows the same as a banner.

How you update depends on how you installed, and every notice names the right
command:

| Installed with | Updates itself | Update by hand with |
|---|---|---|
| the installer (`curl ... \| sh` or `irm ... \| iex`) | yes, from v0.7.0 | `seamlessd update` |
| Homebrew | no | `brew upgrade --cask arctop/tap/seamless` |
| a clone (`make install`), `go install` or `go build` | no | `git pull && make install` |
| a client of a shared daemon | no | re-run the pairing commands `seamlessd client-config` prints on the server |
| a [peer_auth](https://thereisnospoon.org/docs/guides/peer-auth/) system daemon | yes, in [the guide's layout](https://thereisnospoon.org/docs/guides/peer-auth/#updates) | `sudo -u _seamless /opt/seamless/bin/seamlessd update --config /opt/seamless/etc/seamless.yaml` |

**What is sent.** One anonymous `GET` to
`https://api.github.com/repos/arctop/seamless/releases`, with a fixed
`User-Agent` (`seamlessd-update-check`), no token, no version, and no machine or
install identifier. It honors `HTTPS_PROXY` from the daemon's environment. The
answer is cached with its `ETag` in `~/.seamless/update/state.json`; a failed
check retries after 15 minutes, doubling up to the check interval, and a rate
limit is waited out. Releases still uploading their assets, drafts and
prereleases are ignored, and "newest" means the highest version, not the most
recently published. An automatic update adds the downloads it needs to install
a release ([what happens during an update](#during-an-update)).

**Turning it off.** In `seamless.yaml`:

```yaml
update:
  check: false
```

`false` means no update traffic at all - and so no automatic update either -
and it is final: the console shows its toggle locked. With the key left out,
**Settings -> Updates** turns checks on and off at runtime, and
`SEAMLESS_UPDATE_CHECK=false` does the same from the service's environment.
`seamlessd update --check` and `seamlessd update` work either way - running
them is you asking. The `update:` keys are new in v0.7.0: a seamlessd release
from before them refuses a config that sets one, so setting it ties that file
to v0.7.0 or newer. To keep the check but install by hand, turn
[automatic updates off](#turning-automatic-updates-off) instead.

Builds from source (`make install`, `go install`, `go build`) do not check unless
`update.check: true` is set, and never write the state file - a developer's
daemon, the test fixtures and CI stay off the network.

**Deploying your working tree instead of a release**

Both `seamlessd update` and `make update` install the newest *release*, which
may be older than your clone's HEAD. To deploy the build from your working
tree instead:

```bash
git pull
make check             # everything green before you swap the running daemon
make install           # swap it
make doctor            # confirm config + DB after the swap
```

On a machine the installer set up, `make install` stops before writing
anything, because a source build there ends automatic updates.
`make install FORCE=1` takes the install over; running the installer again
hands it back.


Migrations apply automatically at startup - there is no separate migrate step.
Run `make doctor` afterwards anyway: it is the cheapest way to learn that the new
build disagrees with your config before an agent does.

`/healthz` reports the running build. If a change seems not to have taken effect,
check it before you debug anything else - a stale daemon still serving the old
binary looks exactly like a bug in the new one.

## Automatic updates {#automatic-updates}

From v0.7.0, an install made by the curl or PowerShell installer installs new
releases by itself. It waits until a release has been out for a day, then until
nothing is using the daemon, and then runs the same verified update
`seamlessd update` does - backed up, confirmed, and rolled back if the new
release does not come up. You can watch it, hurry it, or turn it off in
**Settings -> Updates**.

### Who updates itself {#who-updates-itself}

A published release, installed by the installer on macOS, Linux or Windows and
run by the service the installer wrote, with the installer's config file
(`~/.config/seamless/seamless.yaml`). Every other install is only told about new
releases, whatever `update.auto` says:

- Homebrew, which owns its files;
- builds from source: `make install`, `go install`, `go build`;
- a client machine of a [shared daemon](https://thereisnospoon.org/docs/guides/network-install/), which runs no
  daemon of its own;
- a release whose layout the daemon cannot vouch for: started by hand rather
  than by the service, running as root, a service definition the installer did
  not write or that runs another binary, another config file, a binary directory
  it cannot write to, or any `SEAMLESS_*` variable besides `SEAMLESS_CONFIG` in
  the daemon's environment (the installer rewrites the service without it).

One more install updates itself: a [peer_auth](https://thereisnospoon.org/docs/guides/peer-auth/) daemon
running as its own account under a LaunchDaemon or system unit, when that
account holds its binary directory, as in the guide's layout
([how](https://thereisnospoon.org/docs/guides/peer-auth/#updates)). It
installs no installer's service; its updater swaps the binaries in place and
the service manager restarts the daemon. Any other peer_auth layout is only
told, and `seamlessd update` refuses one.

**Settings -> Updates** (its **Mode** row) and `seamlessd update --check` (its
`mode` row) say which kind this install is, and why. That last rule is why the update keys belong in the
config file, not the service's environment: `SEAMLESS_UPDATE_AUTO=true` there
does not turn automatic updates on, it makes the install notify-only. How the
daemon starts the updater on each OS is under
[The updater on your OS](https://thereisnospoon.org/docs/reference/service/#the-updater).

### When it installs {#when-it-installs}

**After the soak.** An automatic update takes only a release published at least
`update.min_age` ago (default `24h`), so a bad release can be pulled before it
reaches you. The age is measured on GitHub's clock - the `Date` of its answers -
not this machine's. The release must also carry its signed checksums bundle; one
without it is announced, never installed by itself. An automatic update never
moves to an older version, and installing by hand ignores the soak.

**At a quiet moment.** The daemon looks once a minute, and the first rule that
holds wins:

1. No live agent session and no request in flight: it installs now. A session is
   live while it is active and has sent a heartbeat within
   `gardener.session_idle_minutes` (45 minutes by default), on any machine that
   shares the daemon.
2. Waiting for `update.max_defer` (default `24h`): it installs at the next 90
   seconds without a request.
3. Waiting for 1.5 times `max_defer`: it installs as soon as no request is in
   flight.
4. Waiting for twice `max_defer`: it installs whatever is in flight. A request
   that never returns (a hung tool call) would otherwise hold every update off;
   the restart ends it, and its client retries.

The wait starts when a soaked release is first ready for the version you run,
so with the defaults a release is installed within about three days of being
published, even on a daemon that is never idle. Not counted as activity:
`/healthz` probes, an open console tab's live feed, an MCP client's standing
notification stream, and requests the Host allowlist refuses. One caveat: an
open console **Interactions** page that lists sessions refreshes every 15
seconds, so rule 2 never fires while it is open; rules 1, 3 and 4 still do.

**With nothing in the way.** No update already running (a `seamlessd update` you
started holds the update lock, and the daemon waits for it), no backoff after a
failed attempt, and no pause or hold (below). Just before it starts, the daemon
re-reads the release list if its copy is more than a minute old, so a release
pulled in the meantime is never installed.

**Settings -> Updates** shows the release it installs next and what it waits
for - `waiting for 2 live agent sessions to go idle, or at a lull in requests in
19h` - and `seamlessd update --check` prints the same on its `target` and
`pending` rows. [An automatic update is waiting](https://thereisnospoon.org/docs/guides/troubleshooting/#an-automatic-update-is-waiting)
lists every reason.

### What happens during an update {#during-an-update}

The daemon never installs anything in-process. It starts `seamlessd update
--auto` outside the service, where the installer's service restart cannot stop
it, and that updater:

1. takes the update lock and records itself in `~/.seamless/update/attempt.json`
   (and, when it is done, `attempts.jsonl`), logging to
   `~/.seamless/update/logs/<attempt>.log`;
2. re-checks that this install may still update itself, that the `seamlessd` on
   disk is the release the daemon runs, that the updater really runs outside the
   service, and - for an automatic update - that you have not turned automatic
   updates off in the meantime. A refusal here changes nothing, and names its
   fix ([the updater refused](https://thereisnospoon.org/docs/guides/troubleshooting/#an-automatic-update-failed));
3. re-reads the release by its tag and downloads its installer and
   `checksums.txt` with their Sigstore bundles from GitHub, then verifies both
   against the release workflow's identity on that exact tag - and, with
   [`update.release_signers`](#owner-signed-releases) set, `checksums.txt`
   against the owner's signature too;
4. proves the way back before changing anything: the release you run now must
   still download and verify the same way, or there is no update;
5. backs the instance up - the database, your memories and your notes - to
   `~/.seamless/backups/pre-update-v<old>-<time>.tar.gz`, keeping the newest two;
6. runs the release's installer, pinned to the verified `checksums.txt`, over
   the install exactly as it is: the same directory, and the same agent clients
   and skills, never adding one you do not have. The installer downloads the
   release archive and restarts the service;
7. confirms the result: the new release has to answer `/healthz` from a freshly
   started process, the one the update state names as running, and still be the
   same process ten seconds later. It has two minutes, or up to five while a new
   daemon is still starting (migrating its database, say). If the installer
   failed after the new release came up, the update counts as "applied with
   warnings": run `seamlessd install-hooks`;
8. otherwise rolls back: it stops the service, puts the database back from the
   backup if the new release had already migrated it (the migrated file stays
   beside it as `seam.db.pre-rollback-<id>`), reinstalls the release you had and
   confirms it the same way. Memories and notes are never rolled back. A
   rollback never unlocks a project: if you locked one from the console while
   the new release ran, the backup's database does not hold that lock, so the
   migrated database stays, the release you had runs on its newer schema, and
   the attempt's record names the project. A lock you removed in that time
   comes back with the backup's database.

A [peer_auth system daemon](https://thereisnospoon.org/docs/guides/peer-auth/#updates) skips the installer:
in step 3 it downloads `checksums.txt`, its bundle and this platform's archive,
and checks the archive against the verified `checksums.txt`; in step 4 it keeps
the `seamlessd` and `seam` it is about to replace beside them; in step 6 it
renames the release's binaries into place and ends the daemon, which its
service manager starts again; and in step 8 it renames the kept binaries back
and ends the daemon the same way, holding the data directory while it puts the
database back.

The restart takes a moment, and agents keep working through it: hooks retry
briefly and then fail open, and the `seam` MCP bridge rides out a restart
([the `seam` CLI](https://thereisnospoon.org/docs/reference/cli-seam/#hooks)). For a day afterwards, new
sessions hear `Seamless updated to v0.7.3 (from v0.7.2)` and every console page
carries the same as a banner. A daemon that has just started on a new version
waits at least half an hour before it asks GitHub anything again.

### When an update fails {#when-an-update-fails}

Every attempt ends in its record, and the daemon reads the outcome from it:

| Outcome | What happened | What automatic updates do next |
|---|---|---|
| applied | The new release is installed and answered as the running daemon. | Nothing more. |
| applied, unconfirmed | The new release runs, but the updater stopped before it confirmed so. | Treat it as applied. |
| rolled back | It failed after installing, and the release you had was restored. | Skip that release. |
| failed | It stopped before anything changed, or the installer failed and left the release you had in place. | Try again after a backoff. A release that fails verification is skipped at once, and one whose installer fails 3 times in a row is skipped too. |
| interrupted | The updater stopped before it finished - killed, crashed, or asleep with the machine - and the release you had still runs. | Try again after a backoff. |
| not rolled back | It failed after installing, and the release you had could not be restored cleanly. | Skip that release and pause. |

The backoff is an hour after the first failure, doubling with each failure in a
row on the same release, up to 24 hours. A skipped release stays skipped; the
next release is taken as usual, and **Update now** or `seamlessd update`
installs a skipped one if you want it.

**Paused.** After automatic updates roll back two different releases in a row,
or after any update the daemon started could not be rolled back cleanly, they
pause themselves: nothing installs by itself until you press **Resume automatic
updates** in **Settings -> Updates**. **Update now** still works while they are
paused. Resume changes no setting, leaves skipped releases skipped, and lets a
running backoff run out.

For a week after automatic updates pause or an update the daemon started does
not apply, every console page carries a banner pointing to
**Settings -> Updates**, where **Last attempt** has the updater's own error and
its log. New sessions' briefings mention a pause, or a release that is now
skipped, as your action, not theirs. `seamlessd doctor` reports the same on its
`updates` row. [An automatic update failed](https://thereisnospoon.org/docs/guides/troubleshooting/#an-automatic-update-failed)
says how to read it and recover.

### Owner-signed releases {#owner-signed-releases}

Every release after v0.8.3 carries a second signature over `checksums.txt`
besides the Sigstore one: `checksums.txt.sshsig`, made by the project's owner
with a hardware security key, in an attended session, after the release
workflow built the release and before it was published. The Sigstore signature
says the release workflow built it; this one says the owner released it.
Whoever holds a token that can push tags and approve the release workflow can
produce the first, but not the second. `checksums.txt` lists every archive and
both installer scripts, so the one signature covers everything an update runs.

The [installers](https://thereisnospoon.org/docs/install/#security-posture) check it on every install of such
a release. The updater checks it only when you pin the owner's keys, by
pointing `update.release_signers` at an OpenSSH allowed_signers file that lists
them - the repository's [`release_signers`](https://github.com/arctop/seamless/blob/main/release_signers),
or your own copy of it:

```yaml
update:
  release_signers: ~/.config/seamless/release_signers
```

With the pin set, an update - automatic, **Update now** or `seamlessd update` -
installs a release only when its `checksums.txt.sshsig` verifies in the
namespace `seamless-release` against a key the file lists, made by a security
key that was touched and unlocked with its PIN. The release it would roll back
to must verify the same way, and the installer it runs must be one
`checksums.txt` lists. Anything else is refused at the verify step, nothing is
installed, and the release is skipped:

- **missing:** the release carries no `checksums.txt.sshsig`;
- **unlisted:** it was signed by a key the file does not list;
- **invalid:** the signature does not verify - another file, another
  namespace, a key that was not unlocked, or not a signature at all.

**Settings -> Updates** names the pin and the last release it refused, the
`update.failed` event carries the word (and the security feed with it), and
`seamlessd doctor` has an `update.release_signers` row. A file that cannot be
used - missing, unreadable, writable by another account, or naming no key for
`seamless-release` - refuses every update until you fix it; doctor says what is
wrong. `seamlessd update --url` is refused while the pin is set, and so is a
release from before signed checksums.

What the pin guards against is a release the owner did not sign, whatever
GitHub's settings allowed. On an install that runs as your own account it does
not guard against that account: anything that can edit your config can remove
the key. Keep the file writable by you (or root) and no one else; under
[peer_auth](https://thereisnospoon.org/docs/guides/peer-auth/#owner-signed-releases) it must be root's.

The key is new after v0.8.3, and releases up to v0.8.3 refuse config keys they
do not know, so set it only once the install runs a newer release. Every
release the pinned updater installs or rolls back to is signed, so each of
them knows the key.

When the owner replaces a key, the repository's `release_signers` adds the new
one a release before releases are signed with it. Copy the file again then: a
pin that lists only the old key refuses the releases the new one signs (and
says **unlisted**).

### Going back to an older release {#going-back}

Installing an older release on purpose - `SEAMLESS_VERSION=0.7.1 seamlessd
update`, or the installer with that knob - sets a hold, so automatic updates do
not undo your choice: they skip every release up to the one you left, or the
newest one the daemon knew of, whichever is higher. A release above that is
taken as usual. **Resume automatic updates** or **Update now** lifts the hold,
and it ends by itself once the install reaches that version again. A rollback
is not a choice, and sets no hold.

### Update now {#update-now}

**Update to v0.7.3 now** in **Settings -> Updates** installs the newest release
at once, after you confirm. It skips the soak, the wait for idle agents, skipped
releases and the backoff, and lifts a hold. It runs whether automatic updates
are on, off or paused - it is you asking - through the same updater, with the
same backup, confirmation and rollback, and the daemon restarts to finish. It is
refused, with the reason, while update checks are off, on an install that does
not update itself (the section shows the command to run instead), while an
update is under way, within a minute of a failed release check, when no newer
release is known, and when the newest release carries no signed checksums
bundle.

### Turning automatic updates off {#turning-automatic-updates-off}

In `seamless.yaml`:

```yaml
update:
  auto: false
```

The daemon still checks and tells you, and installing stays yours:
`seamlessd update`, or **Update now**. Like `check: false`, it is final: the
console shows its switch locked. With the key left out, **Turn automatic updates
off** in **Settings -> Updates** does the same at runtime, and **Reset to the
config file** hands both switches back to the file. `update.check: false` turns
off the check and, with it, every automatic update.

Set these keys in the file, not in the service's environment: any `SEAMLESS_*`
variable besides `SEAMLESS_CONFIG` there makes the install notify-only, so
`SEAMLESS_UPDATE_MAX_DEFER` or `SEAMLESS_UPDATE_MIN_AGE` would switch automatic
updates off rather than tune them. `update.max_defer` and `update.min_age` are
described with [the update block](https://thereisnospoon.org/docs/reference/configuration/#the-update-check).

### Codex hooks after an update {#codex-hooks-after-an-update}

Codex skips a hook whose definition changed until you approve it again. When an
update changes Codex's `hooks.json` (`$CODEX_HOME/hooks.json`, else
`~/.codex/hooks.json`), the day-long "updated" banner and new sessions'
briefings on this machine say so: open `/hooks` in Codex and approve them, as
after the install ([Trust the hooks once](https://thereisnospoon.org/docs/codex-cli/#trust-the-hooks-once)).

### macOS: the background-items notice {#macos-background-items}

From v0.7.0, the macOS release binaries are signed with the project's Developer
ID Application certificate and notarized by Apple, so macOS should keep
treating an updated `seamlessd` as the same background item and show no new
notice. Moving to a signed release from an ad-hoc-signed build - v0.6.0 or
older, or any `make install` build - may still show one "seamlessd can run in
the background" notice, sometimes a while after the update.

## Uninstall {#uninstall}

`seamlessd uninstall` is the one command, on every OS. It reverses the whole
install - stops and removes the per-user service, strips the Claude Code and
Codex hooks, deregisters the MCP server from both client CLIs and from the
Claude app's `claude_desktop_config.json`, removes both hook clients' installed
`seam-onboard` and `seam-research` packages/one-shot markers, and deletes the
binaries - and it is idempotent, so a second run is a clean no-op. Preview it
first with `--dry-run`:

```bash
seamlessd uninstall --dry-run   # print exactly what would be removed
seamlessd uninstall             # do it (asks to confirm on a terminal)
```

From a clone it is `make uninstall`, which builds first and then runs that same
command against your installed copy.

**Your knowledge is kept by default.** `~/.config/seamless` (your bearer key) and
`~/.seamless` (the database, and your memories and notes as markdown) are left in
place - the uninstall of a program should not delete your knowledge. Add
`--purge` (or `make uninstall PURGE=1`) only when you actually mean to delete
them; a guard refuses to purge a path that resolves to your home directory or the
filesystem root. See [Storage](https://thereisnospoon.org/docs/reference/storage/) for what is in there.

The hooks come out of `~/.claude/settings.json` and Codex's `hooks.json` through
the same exact classifier the installer and doctor use. Current, marked-stale,
and unmistakable legacy Seamless entries are removed; foreign entries survive,
even when their arguments happen to contain `hook <event>`. The install's
original backup sits next to each file. Seamless hooks in Claude Code's
[managed settings](https://thereisnospoon.org/docs/reference/cli-seamlessd/#seamlessd_install_hooks_managed)
stay where they are, and uninstall names them: that file belongs to whoever
deployed it.

If you would rather do it by hand - a bare binary you never installed a
service for, say - `claude mcp remove seamless` or `codex mcp remove seamless`
drops that client's MCP registration; the chat surface has no CLI, so delete
the `seamless` entry under `mcpServers` in the Claude app (Settings >
Developer > Edit Config) and restart it. The native service teardown commands
live with the rest of the OS-specific detail:
[Removing the service by hand](https://thereisnospoon.org/docs/reference/service/#removing-the-service-by-hand).

## Add or remove one client

Client wiring is independent per target, and adding one never disturbs
another - Claude's and Codex's skills live in separate homes with independent
delivery markers, and `install-hooks` touches only the target you name:

```bash
seamlessd install-hooks --client codex            # add Codex to an existing install
seamlessd install-hooks --client claude-desktop   # add the Claude app chat surface
seamlessd install-hooks                           # or re-run the interactive multi-select
```

Restart the client you added so it loads hooks, MCP, and skills; Codex
additionally gates new hook definitions behind
[/hooks approval](https://thereisnospoon.org/docs/codex-cli/#trust-the-hooks-once), and the Claude app reads
its config only at startup.

Removing one client's wiring while keeping Seamless installed is the manual
path: `claude mcp remove seamless` or `codex mcp remove seamless` deregisters
MCP, the managed hook entries (tagged `seamless_managed`) come out of
`~/.claude/settings.json` or `${CODEX_HOME:-~/.codex}/hooks.json`, and the
skills are directories you can delete. `seamlessd uninstall --client <name>`
also exists, but it scopes only which clients are un-wired during a **full**
uninstall - the service and binaries come out regardless.
