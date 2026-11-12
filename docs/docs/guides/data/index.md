# Import, back up & restore

> Scheduled backups and two-click restores in the console, one archive with seamlessd export, seamlessd restore and import, putting ~/.seamless in git, what deleting seam.db actually costs, and moving to a new machine.

Because durable knowledge is markdown files, backup and restore are boring - and
that is the feature. A release install - the installer's or Homebrew's - backs
itself up every day without being asked, and the console restores any of those
backups in two clicks. The rest of this page is about knowing which half of
`~/.seamless` is precious and which half regenerates.

## Backups in the console {#backups-in-the-console}

**Settings > Backups** in the console backs up the whole instance on a
schedule, lists the backups it finds, and restores one. Each backup is one
archive of the kind [`seamlessd export`](#the-whole-instance-in-one-archive)
writes: every memory and note, a consistent snapshot of the database with your
sessions, tasks, trials and projects, and a list of what each project holds.
Like an export, it holds no config and no key.

### The schedule

A release install (the installer's or Homebrew's) backs up **every day, keeping
the newest 7**, in `~/.seamless/backups`. A Seamless built from source backs up only once you
turn it on, so a throwaway daemon never writes backups. The section's
**Schedule and folder** card sets:

- **Back up automatically**, on or off;
- **How often**: every hour, every 6 hours, every day or every week. The config
  file can set any interval from `1h` to `720h`, and the list then shows it too;
- **Automatic backups to keep**, from 1 to 100. Older automatic backups are
  deleted after the next one is written. Backups you make yourself, the safety
  backups taken before a restore and the updater's pre-update backups are never
  deleted automatically, and neither is another install's backup in a shared
  folder.

The first scheduled backup runs no sooner than ten minutes after Seamless
starts. A run is skipped when nothing has changed since this install's newest
backup in the folder (the card then says **Up to date**), or when the disk is
short of room: a backup needs its own size free on each disk it touches, plus a
reserve of 1 GiB or the backup's size, whichever is larger, so the updater can
always take its own backup. **Back up now** makes one at once, whatever the
schedule says.

What you save there wins over the config file's
[`backup:` block](https://thereisnospoon.org/docs/reference/configuration/#backups) until you press **Reset to
the config file**, and applies from the next backup.

### The folder

Backups go to one folder you choose. Pick a synced folder or an external disk
so a backup outlives the machine: the section offers the places it finds on
this machine and can write to - iCloud Drive and the folders under
`~/Library/CloudStorage` (Dropbox, Google Drive, OneDrive) and every external
disk on macOS, `~/Dropbox` and the disks under `/media/<you>` on Linux,
OneDrive and the other drives on Windows, and Seamless's own data folder
everywhere. A suggestion fills in `<place>/Seamless Backups`.

The folder field checks the path as you type, without writing anything:
**Ready** with the free space, **Will be created**, **Not writable**, that its
disk is not connected, or **Inside Seamless's data folder**, which is refused
(only `~/.seamless/backups` itself is allowed). Saving a new
folder creates it, private to your account, and tests it with a write first.
The backups already in the old folder stay there.

On macOS, a folder that macOS's privacy protection keeps Seamless out of gets
its own answer: allow Seamless in **System Settings > Privacy & Security >
Files and Folders**, or give it **Full Disk Access**. A disk that is not
connected is never written under: the run fails, says so, and the next one
tries again.

### The list

**Available backups** lists, newest first, the backups in the chosen folder and
in `~/.seamless/backups`, where the safety and pre-update backups always stay.
Each one shows when it was made, its size and its kind in words: **Automatic**,
**Made by you**, **Before restore**, **Before updating from v0.8.1**, or
**Exported** for a file `seamlessd export` wrote. A backup another Seamless made
says which machine it came from. **What it holds** opens a table of each
project's memories, notes, tasks and sessions, beside how many more or fewer
memories and notes the project holds now. A locked project shows its name and
what the backup holds of it, never what it holds now.

Two kinds of file are listed but cannot be restored yet: an iCloud placeholder
(download it in Finder first) and a backup a newer Seamless made (update to
restore it). **Delete** removes a backup from its folder, after a confirm.

### Restore, and undo {#restore-in-the-console}

**Restore…** opens a card in the backup's row that compares it with now and
offers two choices:

- **Replace all data with this backup.** Everything goes back to how it was
  when the backup was made. Your current data is saved as a backup first, so
  you can undo it.
- **Only bring back what's missing.** Adds what the backup has and this install
  doesn't, such as deleted memories, notes and sessions. Nothing you have now
  changes. A backup of memories and notes only (`seamlessd export --no-db`)
  offers just this one.

Then the card follows the restore through its steps: **Checking the backup**,
**Preparing the restored data** (replace) or **Saving your current data**
(merge), **Restarting Seamless**, **Done**, and for a replace, **Saving your
previous data as a backup**. The heavy work happens while Seamless still
serves, so a backup that fails its checks changes nothing. Seamless then
restarts to finish, and agents lose it for a few seconds. **Cancel restore** is
there until the restart.

After the restart a result card says what was restored, with **Undo this
restore** - which opens the same card for the safety backup - and
**Dismiss**. On Windows, Seamless cannot restart itself in place: the card asks
you to run `seamlessd restart` within an hour. A restore that waits longer is
dropped, and nothing changes.

**What a restore keeps.** Locks, passkeys, project privacy, repo routes and
console settings stay as they are now:

- a project locked today stays locked, and one locked in the backup comes back
  locked, so a restore never reduces the set of locked projects;
- each project keeps the stricter of its privacy setting now and in the backup;
- the passkeys and enrollment codes stay today's, so a passkey you removed
  after the backup does not come back;
- the repo routes, the briefing, features, level, embeddings, update and backup
  settings stay this machine's;
- every agent lease, console unlock and read grant the backup held is ended,
  so each agent and browser asks again.

Under [peer_auth](https://thereisnospoon.org/docs/guides/peer-auth/#restore) with a project locked, a console
restore needs a passkey, and without one the card shows the terminal command
instead. [`seamlessd restore`](#restore) is the same restore without a browser.

## What is precious, and what is not

```text
Backup priorities
Precious memory/{project|_global}/{name}.md Source of truth for durable memory.
Precious notes/{project|_global}/{slug}.md Source of truth for long-form artifacts.
Mixed seam.db Rebuildable search mirrors plus irreplaceable session, task, trial, and event history.
Back up the whole directory. The file trees are always authoritative; parts of SQLite are authoritative too.
```

`seam.db` holds two different kinds of thing, and conflating them is what makes
people either over-protect it or under-protect it:

| In `seam.db` | If it were lost |
|---|---|
| FTS index, embeddings | **Rebuilt automatically** from the files |
| Sessions, tasks, trials, events, telemetry, briefing overrides | **Gone** - there is no file to rebuild them from |

So: **deleting `seam.db` costs you the record of what happened, not the knowledge
of what is true.** Every memory and note survives, because they are files.

## Put it in git

The strongest backup is the one you already know how to use:

```bash
cd ~/.seamless
git init
printf 'seam.db\nseam.db-wal\nseam.db-shm\n' > .gitignore
git add . && git commit -m "seamless: initial"
```

Ignoring the database is deliberate. It is a binary that changes constantly, it
does not diff usefully, and its contents are either rebuildable or high-churn
state that a nightly commit would capture uselessly. What you want in git is the
knowledge - and that diffs beautifully, because it is markdown.

Commit periodically (a cron job or a `launchd` timer is plenty). The payoff is
that `git log` over your memory directory is a real history of what your agents
learned and when they changed their minds.

Git gives you the knowledge with a history. It deliberately leaves out the other
half - sessions, tasks, trials, events - which is what
[`seamlessd export`](#the-whole-instance-in-one-archive) is for. They compose:
git for the diffable record of what your agents learned, an archive for the whole
instance.

## The whole instance in one archive

```bash
seamlessd export
# wrote /Users/you/seamless-nuc.local-20260923T220501Z.tar.gz (4.1 MiB)
#   host nuc.local, seamlessd 0.9.1, created 2026-09-23T22:05:01Z
#   corpus: 214 memories, 38 notes
#   database: schema v25, 18422 rows across 21 tables
```

One gzipped tar with the markdown corpus, a consistent snapshot of `seam.db`, and
a manifest describing what is inside. Full flags in
[the CLI reference](https://thereisnospoon.org/docs/reference/cli-seamlessd/#seamlessd_export).

Three things worth knowing:

- **Run it while the daemon is up.** The snapshot is SQLite's `VACUUM INTO`,
  taken inside a read transaction, so a write in flight is simply not in it. No
  `cp` of a WAL-mode database, no stopping anything. (A plain `cp` of
  `seam.db` under a live writer can capture a torn state; this cannot.)
- **The key is not in it.** Config and `mcp.api_key` are deliberately excluded,
  so an archive can go to a NAS or another machine without carrying this
  machine's only credential.
- **`-` streams.** `seamlessd export -o - | ssh backup-box 'cat > seam.tgz'`
  writes the archive to stdout and the report to stderr.

`--no-db` gives you a knowledge-only archive: the two markdown trees, nothing
else. It is the tarball equivalent of the git recipe above.

## Restore

```bash
seamlessd backups                                           # what there is to restore
seamlessd restore seamless-nuc.local-20260923T030000Z-auto.tar.gz
seamlessd restore --merge ~/Downloads/seamless.tar.gz       # bring back only what is missing
```

[`seamlessd restore`](https://thereisnospoon.org/docs/reference/cli-seamlessd/#seamlessd_restore) is the
console's [restore](#restore-in-the-console) from a terminal. Name a backup as
`seamlessd backups` lists it, or give any backup file's path. It says what the
backup holds and asks before it changes anything (`--yes` skips the question).
Without `--merge` the backup replaces all data and today's is saved as a
before-restore backup first, so restoring that one undoes it. With `--merge`
only what the backup has and this install lacks is added. Either way it keeps
what [a console restore keeps](#restore-in-the-console): locks, passkeys,
project privacy, repo routes and console settings.

There is nothing to stop first. With the daemon running, the command prepares
the restore, the daemon restarts to finish it, and the command waits for the
result. With no daemon, it applies the restore itself.

`seamlessd restore` needs a database to restore into, so on a machine where
Seamless has never run, start it once first (the installer does). It refuses a
database older than its own binary, like every one-shot command.

### Restoring with `seamlessd import`

`seamlessd import` is the older, lower-level way. It brings the backup's own
passkeys and console settings with it, which `seamlessd restore` never does:

```bash
seamlessd stop
mv ~/.seamless ~/.seamless.old                   # import restores only into an empty data directory
seamlessd import --from seamless-nuc.local-20260923T220501Z.tar.gz
seamlessd doctor && seamlessd start
```

Into an **empty** data directory, that is a restore: every `.md` lands
byte-for-byte and the database snapshot is renamed into place last. Into a
**populated** one it is a merge instead, first-writer-wins by ULID. Which one it
will be is a property of the destination, is printed in the report, and cannot be
overridden - so there is no way to ask for a restore and get a populated instance
wiped. Add `--dry-run` to see the mode and the counts without writing anything.

A restore refuses while a daemon is answering for that data directory (it would
be replacing `seam.db` underneath it) and names `seamlessd stop`; `--force`
overrides. A merge does not need the daemon down.

You can also restore the knowledge alone, from an archive or from git:

```bash
# files back in place
cp -R backup/memory backup/notes ~/.seamless/
make run
```

Startup reconciliation walks the tree, notices which files the index does not
know about (or whose content hash changed), and indexes them. A file watcher
keeps it in sync from then on. You do not run a reindex command, because there
isn't one to forget.

To force a full rebuild, stop the daemon, delete `seam.db`, and start it again.
You lose sessions, tasks, trials, and events; you lose no knowledge.

## Locked projects

A backup keeps a locked project locked. Every backup and `seamlessd export`
records every locked project in the archive, whether the console set the lock or
`peer_auth.force_locked` holds it, and a `--no-db` archive records them too. The
file itself is not encrypted, so a lock does not protect it: keep the archive as
carefully as the most sensitive project in it.

With `seamlessd restore` or the console there is nothing to choose: the restore
keeps today's locks and passkeys and adds the backup's locks. The rest of this
section is about `seamlessd import`.

Before an import writes anything, it looks for a passkey that can open those
projects:

- **Restoring into an empty data directory**, the backup's passkeys come back with
  its database. One counts if it was enrolled for this instance's relying party
  and can reach this machine: a synced passkey, a security key or a phone, or any
  passkey when you restore on the machine that enrolled it.
- **Merging**, this instance's own passkeys count. A merge never brings passkeys.

If it finds one, it asks whether you still have it. If it finds none, or you
don't, you choose:

- **Keep the locks and add a passkey right after.** The import prints the steps.
  The Approvers page takes a new passkey with a one-time code from your terminal,
  so a passkey you no longer have does not lock you out.
- **Add a passkey first**, on a merge. The import stops with nothing changed. A
  restore doesn't offer this, because a daemon started on the empty data directory
  would create a database and turn the restore into a merge.
- **Remove the locks** from these projects. They keep their isolation setting, and
  agents working in them then read them without asking.

With no terminal to ask on, an import that finds no passkey stops with nothing
changed until you pass `--keep-locks` or `--remove-locks`. Add `--dry-run` to see
what it finds.

Once the daemon is running, check the passkey for real by unlocking one of the
projects with it at `/console/unlock/<project>`. Only a browser and the
authenticator can prove you still have it, so the import can't.

A few details:

- A lock held by the old machine's `force_locked` list becomes a console lock
  here, because no archive carries a config. If this machine's config force locks
  the project too, the file keeps holding it.
- With `peer_auth` off, locks do nothing, so the import says so and asks. Turn it
  on before you start the daemon.
- A merge that brings locks needs the daemon stopped, because a daemon reads the
  locked projects when it starts. `--force` overrides that, and you must then
  restart the daemon right away.
- On a merge, a project that is open here and locked in the archive is raised to
  the archive's isolation setting, and leaves any family or parent it had. If it
  has child projects it can't be fenced, so the import stops with nothing changed.
- `--remove-locks` removes the archive's locks only. A force lock in this config,
  or a lock this instance already had, stays.
- A restore puts things back as they stood at the backup. A lock set later isn't
  in it, and a passkey removed later returns: check `seamlessd approvers list`.

## Merging two instances

Importing an archive into an instance that already has data is a **merge**, and
it is idempotent: anything whose ULID is already here is skipped, so running the
same archive twice inserts zero the second time. That makes it the way to fold a
laptop's instance into a desktop's, or several devices' into one.

Two rules keep it honest:

- **Collisions are reported, never resolved.** A memory whose path is already
  held by a different item is left unwritten and both ids are printed; a session
  name or project slug already taken is reported rather than renamed. Deciding
  which one wins is yours.
- **This machine's settings stay this machine's.** Repo mappings, families,
  briefing overrides, and the embedder switch are not merged, and neither are
  embeddings - vectors belong to whichever model you run here, so imported items
  are embedded on write instead.

## Import from Seam v1

```bash
seamlessd import --from ~/.seam
```

`seamlessd import` fronts two operations and picks between them by what
`--from` names on disk: a **directory** is a Seam v1 store, a **file** (or `-`)
is an archive. Point it at a v1 data directory and it brings that store's
memories, sessions, and tool-call events into `seam.db`. It is **idempotent**:
running it twice does not double anything, so a partial import is safe to re-run.

## Hand-editing

Files are the source of truth, so editing them by hand is allowed and expected -
the watcher picks up your change and reindexes it.

Two rules:

1. **Never hand-stamp `invalid_at` or `superseded_by`.** Those are set by the
   supersede path, which also updates the indexes and the pointer between the two
   memories. Writing them by hand produces a file that says one thing and a
   database that believes another, and the lifecycle invariants (stamp once,
   point only at an active memory, never self-supersede) stop being true. Use
   `memory_write` with `supersedes` - see [Memory & notes](https://thereisnospoon.org/docs/concepts/memory/).
2. **Do not hand-edit `id`.** It is a ULID assigned once and referenced by
   `superseded_by` pointers elsewhere.

Everything else - the body, the description, tags, the kind - is yours to edit in
any text editor.

## Moving machines

```bash
# on the old machine
seamlessd export -o seamless.tar.gz

# on the new one, once Seamless is installed and has started
seamlessd restore ~/seamless.tar.gz
```

Every memory and note comes back byte-for-byte, with the sessions, tasks,
trials, and events. Backups in a synced folder work the same way: point the new
machine's backup folder at it, and its backups are in the console's list, marked
as from another Seamless. If you are folding the old machine into an instance
that already has data, add `--merge` - see
[Merging two instances](#merging-two-instances).

The new machine keeps its own settings, as every restore does: its passkeys,
its repo routes (a session started in a repository maps it again by its
directory name, and `seamlessd map-repo` points one at another project) and its
console settings. Locked projects come back locked. To open them,
enroll a passkey here: the Approvers page takes one with a one-time code from
your terminal. To bring the old machine's passkeys and console settings across
too, restore with `seamlessd import` into an empty data directory instead
([Restoring with `seamlessd import`](#restoring-with-seamlessd-import)).

The install generates a fresh `mcp.api_key` rather than copying the old config:
the key is the only credential, it is deliberately not in the archive, and a
machine migration is a good moment not to spread it around. See
[Install & deploy](https://thereisnospoon.org/docs/install/).

If all you want is the knowledge, the git recipe moves it just as well
(`git clone <remote> ~/.seamless`), and so does `seamlessd export --no-db`. The
index rebuilds itself on first start either way.
