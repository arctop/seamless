---
title: Storage and file formats
description: The ~/.seamless tree, memory and note frontmatter field by field, what lives only in SQLite, and the rules for hand-editing.
---

Seamless splits its state in two. Markdown files under the data directory are the
**source of truth** for durable knowledge. SQLite is the **record** for
high-churn state and a **rebuildable index** of the files.

Knowing which half a given piece of state lives in tells you whether you can edit
it by hand, and what happens if you delete it.

## The tree

`data_dir` defaults to `~/.seamless`:

<figure class="doc-figure" aria-labelledby="storage-tree-caption">
  <span class="figure-kicker">On-disk layout</span>
  <div class="doc-tree">
    <div class="tree-root"><code>~/.seamless/</code><span class="tree-note">Owner-only local data directory</span></div>
    <div class="tree-branch"><code>seam.db</code><span class="tree-note">Indexes, sessions, tasks, trials, events, and embeddings</span></div>
    <div class="tree-branch"><code>seamlessd.lock</code><span class="tree-note">serve's one-daemon lock; holds the holder's PID (never delete it)</span></div>
    <div class="tree-branch"><code>seam.db.pre-rollback-{attempt}</code><span class="tree-note">Only after a rollback restored the database: the one the rolled-back release had migrated (with its -wal and -shm), kept until you delete it</span></div>
    <div class="tree-branch"><code>memory/</code><span class="tree-note">Durable memory tree</span></div>
    <div class="tree-leaf"><code>_global/{name}.md</code><span class="tree-note">Machine-wide memories</span></div>
    <div class="tree-leaf"><code>{project}/{name}.md</code><span class="tree-note">One project memory per file</span></div>
    <div class="tree-branch"><code>notes/</code><span class="tree-note">Durable note tree</span></div>
    <div class="tree-leaf"><code>_global/{slug}.md</code><span class="tree-note">Machine-wide notes</span></div>
    <div class="tree-leaf"><code>{project}/{slug}.md</code><span class="tree-note">One project note per file</span></div>
    <div class="tree-branch"><code>backups/</code><span class="tree-note">Backups: the scheduled ones (unless backup.dir names another folder), the safety backups taken before a restore, and the pre-update archives (pre-update-v{old}-{time}.tar.gz), the newest two</span></div>
    <div class="tree-leaf"><code>.schedule.json</code><span class="tree-note">The schedule's record: the last run, the failure streak, the folder in use and this install's id</span></div>
    <div class="tree-branch"><code>restore/</code><span class="tree-note">Only around a restore: what it staged, and its result</span></div>
    <div class="tree-leaf"><code>pending.json</code><span class="tree-note">A restore staged or under way; while it exists, updates wait</span></div>
    <div class="tree-leaf"><code>last.json</code><span class="tree-note">The last restore's result, shown until dismissed</span></div>
    <div class="tree-leaf"><code>{id}/</code><span class="tree-note">archive.tar.gz (the verified copy of the backup), new/ (the restored data, before the swap), previous/ (the data a replace set aside, until it is saved as a backup)</span></div>
    <div class="tree-branch"><code>update/</code><span class="tree-note">The update check's and the updater's record (release builds only)</span></div>
    <div class="tree-leaf"><code>state.json</code><span class="tree-note">Last check, cached release list, running daemon, automatic-update state</span></div>
    <div class="tree-leaf"><code>update.lock</code><span class="tree-note">The updater's lock; never delete it</span></div>
    <div class="tree-leaf"><code>attempt.json</code><span class="tree-note">The newest update attempt, live and finished</span></div>
    <div class="tree-leaf"><code>attempts.jsonl</code><span class="tree-note">Finished attempts, one line each (compacted to the newest 100)</span></div>
    <div class="tree-leaf"><code>logs/{attempt}.log</code><span class="tree-note">Each attempt's full output, the newest 20</span></div>
    <div class="tree-leaf"><code>test-fail-confirm</code><span class="tree-note">Only during a rollback drill: every update rolls back while it exists</span></div>
  </div>
  <figcaption id="storage-tree-caption">Markdown is durable knowledge; the database combines rebuildable indexes with high-churn operational state.</figcaption>
</figure>

A memory's project is its directory. An empty `project` field means global, and
the file lands in `memory/_global/`. Notes work the same way, under
`notes/_global/`. A memory's filename is its `name`; a note's is its `slug`.

## Memory frontmatter

One memory per file, YAML frontmatter plus a markdown body:

```yaml
---
id: 01K...
kind: gotcha
name: chroma-boot-race
description: one line, <=150 chars -- the ONLY text shown in indexes
project: seam
created: 2026-07-10T18:00:00Z
updated: 2026-07-10T18:00:00Z
valid_from: 2026-07-10T18:00:00Z
invalid_at: null
superseded_by: null
source_session: cc/019f7291-7ccbc0d8f16e51a4
model: claude-fable-5
tags: [x, y]
---
body markdown
```

Field by field:

| Field | Set by | Meaning |
|---|---|---|
| `id` | system | ULID. Never a UUID. The identity every other reference points at. |
| `kind` | author | One of the nine kinds below. Kinds are pinned and filtered differently during briefing assembly. |
| `name` | author | The filename stem, and how agents address the memory. Unique per project only among active memories - a superseded memory coexists with a replacement that reuses its name. |
| `description` | author | One line, ≤150 chars. **The only text shown in indexes** - write it for an agent deciding whether to read the body. Longer text is **silently truncated** by `memory_write`, not rejected, so write to the limit deliberately. |
| `project` | author | Project slug. Empty means global, and the file lives under `memory/_global/`. Omitted from the frontmatter when empty. |
| `created` | system | RFC3339. First write. |
| `updated` | system | RFC3339. Last write. |
| `valid_from` | system | RFC3339. Start of the validity window. |
| `invalid_at` | **system only** | RFC3339 or `null`. `null` means active. Set on supersession or archive; a memory with it set leaves every active index. |
| `superseded_by` | **system only** | ULID of the replacement, or `null`. |
| `source_session` | system | Provenance - an ambient session name such as `cc/019f7291-7ccbc0d8f16e51a4`, or the ULID of a bound explicit session. Consumers resolve both; treat names as opaque. |
| `model` | system | The model that produced the content, verbatim as the provider names it (`claude-fable-5`, `gpt-5.5`). Stamped from the writing session; a rewrite by a known model re-attributes, an unknown one preserves the prior value. Omitted when unknown. |
| `favorite` | author | `true` when starred (console, `seam fav`, or `favorite_set`). A starred memory is pinned into every briefing and boosted in recall. Omitted when false; hand-editing it works - the watcher reindexes. Starring never bumps `updated`. |
| `tags` | author | Flow-style list. Omitted when empty. Also the `plan:<slug>` composition key. |

Timestamps are RFC3339 strings on disk. Any key not in that set is preserved
verbatim through a parse/render round-trip (Obsidian plugin fields and the like
survive), but is not mirrored to the index.

### The nine kinds

| Kind | Meaning |
|---|---|
| `constraint` | A hard rule that must hold on any task. |
| `convention` | A project-local choice or layout fact. |
| `runbook` | A procedure to follow. |
| `protocol` | An interaction or coordination contract. |
| `gotcha` | A surprising pitfall. |
| `decision` | A choice and its rationale. |
| `refuted` | A claim investigated and found false. |
| `reference` | A durable pointer or fact. |
| `stage` | A gated stage with status and gate lines. |

### Validity

`invalid_at` is the whole lifecycle in one field. `nil` means active. Anything
else means the memory has left the briefing, prompt, and recall indexes - while
staying on disk and readable, as provenance.

A superseded memory is never deleted. It is stamped `invalid_at` and
`superseded_by`, and keeps a tombstone line in its file body, so the on-disk
truth stays honest about what replaced what. Its file keeps occupying
`memory/{project}/{name}.md`; a new memory cannot silently overwrite it (that
would destroy readable supersession history) and must free the name or pick
another.

## Note frontmatter

A note is a work artifact - research finding, decision record, meeting summary.
Unlike a memory it has **no lifecycle and no validity window**:

```yaml
---
id: 01K...
title: Human-facing title
slug: human-facing-title
description: one line
project: seam
created: 2026-07-10T18:00:00Z
updated: 2026-07-10T18:00:00Z
source_url: https://example.com/page
model: claude-fable-5
tags: [research, plan:my-feature]
---
body markdown
```

`id`, `title`, `created`, and `updated` are always emitted. `slug`,
`description`, `project`, `source_url`, `model`, `favorite`, and `tags` are
omitted when empty (or, for `favorite`, false). `source_url` is set when the
note came from `capture_url`; `model` is the producing model, stamped exactly
as for memories; `favorite` marks a starred note, exactly as for memories.
Empty `project` means `notes/_global/`. Unknown keys round-trip losslessly,
same as memories.

## What lives only in SQLite

`seam.db` holds two categories of data with very different recovery stories.

**Rebuildable mirrors of the files.** Delete these and they come back:

- `memories_index` - frontmatter mirror, plus `content_hash` and `file_path`.
- `notes_index` - the same for notes.
- `fts` - the FTS5 virtual table over both, indexing title, name, description,
  and body. Self-contained, managed directly by the files layer.
- `embeddings` - one float32 BLOB vector per item (little-endian), with its model
  and dims. Brute-force cosine; there is no vector database.

**DB-of-record state that exists nowhere else.** These have no file behind them,
so losing `seam.db` loses them:

- `sessions` - ambient and explicit sessions, findings, cwd, status, and the
  agent process that owns an ambient session (what binds the agent's tool calls
  to it).
- `tasks` and `task_deps` - the ready-queue, plan slugs, claims, and leases.
- `trials` - research lab records with queryable JSON metrics.
- `events` - the append-only log behind telemetry, the console feed, and
  retrieval stats.
- `retrieval_stats` - inject/read counters plus the per-memory time-decayed
  utility score with its per-signal demand breakdown, rebuilt from events.
- `projects` - slugs, parent topology, retirement.
- `gardener_proposals` - pending proposals, one row per kind-and-key
  (merge, consolidate, archive, digest, reproject, rekind, split, abandon-plan,
  memory-wanted, tool-error).
- `settings` - `repo_project_map`, project families, the runtime briefing
  overrides the console writes, the console's update override (checks and
  automatic updates), the per-scope utility-activation latch, and the embedder
  on/off switch.
- `jobs` - the small queue for embeds and LLM digests.

The split is deliberate: durable knowledge is yours in plain markdown, and
high-churn state that would be miserable as files stays in the database.

WAL mode means `seam.db` is normally accompanied by `seam.db-wal` and
`seam.db-shm`. They are part of the database; copy all three or none - a `cp` of
just `seam.db` under a live writer is a torn snapshot, not a backup. The way not
to think about any of that is
[`seamlessd export`](/reference/cli-seamlessd/#seamlessd_export), which takes the
snapshot with SQLite's `VACUUM INTO` inside a read transaction and is safe to run
against a running daemon.

### The update records {#the-update-records}

`update/state.json` is the [automatic update check's](/updating/#automatic-update-checks)
memory between restarts: when it last asked GitHub, the cached release list and
its `ETag`, when it asks next, the current error streak, the newest release it
has told you about, the daemon that wrote it (version, install kind, process
and instance id), and the last version change it saw. On an install that
[updates itself](/updating/#automatic-updates) it also keeps what automatic
updates have decided: the pending update and when it began waiting, the update
it started, the last attempt it settled, blocked releases, a hold, a pause, a
backoff, and a fingerprint of Codex's `hooks.json`. Only a daemon from a release
build writes it - a build from source never creates the directory - and
`seamlessd doctor` and `seamlessd update --check` only read it, with
`attempt.json` and `attempts.jsonl` beside it. It is private (mode `0600` in a
`0700` directory) and safe to delete: the next start rebuilds it. That costs
one "version changed" notice it can no longer tell, and everything automatic
updates had decided. A release that rolled back here is then a candidate again,
and a hold or a pause is gone. A file that does not parse is moved aside to
`state.json.corrupt-<time>` and the daemon starts fresh.

The updater, `seamlessd update`, keeps its own records next to it. It writes
them; the daemon only reads them. It holds `update/update.lock` for its whole
run, an OS lock released however the process ends. Never delete that file:
removed while an updater holds it, it lets a second updater lock a new file of
the same name. `attempt.json` is the newest attempt, rewritten every 30
seconds while it runs, and `attempts.jsonl` gets each finished attempt as one
line. A line that does not parse is skipped, never fatal. `logs/<attempt>.log`
holds an attempt's every step and its installer's output, and the newest 20
logs are kept. Each release reads the records of releases older and newer than
itself, so their format only ever gains fields. The updater runs only on an
install the installer made, so a build from source writes none of these.

Before an update on such an install, the updater archives the instance to
`backups/pre-update-v<old>-<time>.tar.gz` with
[`seamlessd export`](/reference/cli-seamlessd/#seamlessd_export)'s machinery,
and keeps the newest two. When an update is rolled back after the new release
had already migrated the database, the backup's database is put back. The
migrated one, with its `-wal` and `-shm`, stays beside it as
`seam.db.pre-rollback-<attempt>` until you delete it. A backup's database that
lacks a console lock the migrated one holds is not put back: the migrated
database stays in place, so a rollback never unlocks a project. Memories and notes are
never rolled back. Creating `update/test-fail-confirm` rehearses a rollback on
a real install: while it exists, every update fails its confirmation on purpose
and rolls back, and `seamlessd doctor` warns. Delete it afterwards.

### The backup records {#the-backup-records}

[Scheduled backups](/guides/data/#backups-in-the-console) go to
`<data_dir>/backups` unless `backup.dir` or the console names another folder.
Their names say what they are: `seamless-<host>-<UTC time>-auto.tar.gz` for an
automatic backup, `seamless-<host>-<UTC time>-before-restore.tar.gz` for the
safety backup a restore takes, which always stays in `<data_dir>/backups`, and
`seamless-<host>-<UTC time>.tar.gz` for one made with **Back up now**, the name
`seamlessd export` gives too. Each one is an export archive whose manifest also
says why it was written, which install wrote it, and what each project held. A
backup is written to a dot-prefixed temporary file in its folder, synced,
renamed into place and read back. Listings skip dot files.

`backups/.schedule.json` is the scheduler's record: when it last ran, confirmed
the backups current and succeeded, the newest backup, the failure streak and its
last reason, the automatic backups it wrote (so it can still prune its own after
a cloud folder turned them into placeholders), the folder in use and this
install's id. It lives beside the backups rather than in the database, so a
restore that replaces the database keeps the schedule's history. `seamlessd
backups` reads its folder from there.

`restore/` exists only around a restore. `pending.json` is the staged restore:
the backup, its SHA-256, the mode, who asked and with what proof, and the stage
it has reached (`staged`, `swapping`, `applied`, or `merging` for a merge). It
is created exclusively, so only one restore waits at a time. `restore/<id>/`
holds the verified copy of the backup and, for a replace, `new/` (the restored
data, prepared while the daemon served) and `previous/` (today's data, once the
swap set it aside, until it is saved as a before-restore backup and read back).
`last.json` is the result the console and `seamlessd restore` report. Never
delete anything under `restore/` by hand while `pending.json` exists: the next
start reads it to finish or undo the swap.

### Reconciliation

At startup the files layer walks both trees and reconciles them against the
index: changed and new files are re-indexed, and index rows whose file has been
deleted are dropped. A watcher then keeps up with out-of-band edits, debounced
(editors emit several writes per save) and with the application's own writes
suppressed so there is no re-index loop.

This is why the index is genuinely rebuildable, and why editing a memory in your
editor works without telling Seamless about it.

## Hand-editing rules

Files are the source of truth, so **hand-editing is allowed and expected**. Open
a memory in your editor, fix the body, save. The watcher picks it up and
re-indexes it. Adding tags, tightening a description, correcting a fact - all
fine.

Two fields are the exception.

**Never hand-stamp `invalid_at` or `superseded_by`.**

These are the lifecycle, and the supersede path enforces invariants a text editor
cannot:

- `invalid_at` is stamped exactly once. Re-stamping an already-invalid memory
  rewrites supersession history and is rejected.
- A `superseded_by` edge must point at an **active** memory. Pointing at an
  inactive one can form a cycle or a dangling chain, and is rejected.
- A memory cannot supersede itself.

Use the supersede path instead - `memory_write` with `supersedes` - which stamps
both fields on the old memory, writes the tombstone line, and points the edge at
the replacement, atomically and with the invariants checked. Archival goes
through the same path.

Hand-stamping these fields does not produce an error. It produces a store that
disagrees with itself: a memory out of the indexes with no valid replacement, or
a supersession chain that loops. Both are quiet, and both are exactly the kind of
thing a future agent will trust anyway.

The rest of the rules are mechanical:

- **`id` is identity.** Changing it makes a new memory and orphans every
  reference to the old one.
- **`name` and `slug` are filenames.** Rename the file and the field together, or
  the watcher will treat it as a delete plus an add.
- **`project` is the directory.** Move the file and change the field together.

## Related

- [Configuration](/reference/configuration/) - `data_dir` and the rest of the key
  set.
- [MCP API overview](/reference/mcp/) - the tools that write these files,
  including `memory_write`'s `supersedes`.
- [MCP: tasks](/reference/mcp/tasks/) - the ready-queue whose state lives only in
  `seam.db`.
