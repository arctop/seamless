# Import, back up & restore

> One archive with seamlessd export, restoring or merging it with seamlessd import, putting ~/.seamless in git, what deleting seam.db actually costs, and moving to a new machine.

Because durable knowledge is markdown files, backup and restore are boring - and
that is the feature. This page is mostly about knowing which half of
`~/.seamless` is precious and which half regenerates.

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
seamlessd stop
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

A backup keeps a locked project locked. `seamlessd export` records every locked
project in the archive, whether the console set the lock or
`peer_auth.force_locked` holds it, and a `--no-db` archive records them too. The
file itself is not encrypted, so a lock does not protect it: keep the archive as
carefully as the most sensitive project in it.

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

# on the new one
make install                        # seeds a config with a NEW generated key
seamlessd stop                      # the installer starts the service
seamlessd import --from seamless.tar.gz
seamlessd doctor && seamlessd start
```

The new machine's data directory is empty, so this is a restore: every memory
and note lands byte-for-byte, and the sessions, tasks, trials, and events come
with it. If you are folding the old machine into an instance that already has
data, the same command is a merge instead - see
[Merging two instances](#merging-two-instances).

The install generates a fresh `mcp.api_key` rather than copying the old config:
the key is the only credential, it is deliberately not in the archive, and a
machine migration is a good moment not to spread it around. See
[Install & deploy](https://thereisnospoon.org/docs/install/).

If all you want is the knowledge, the git recipe moves it just as well
(`git clone <remote> ~/.seamless`), and so does `seamlessd export --no-db`. The
index rebuilds itself on first start either way.
