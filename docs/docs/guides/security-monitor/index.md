# Feeding a security monitor

> Tell a security monitor what your agents did - a metadata feed of events over a Unix socket and a commit-matching snapshot file, each a small versioned contract, both off by default.

A security monitor can see processes, files and connections. What it cannot see
is what your agents did inside Seamless. Seamless can tell it, in two
independent ways:

1. **A metadata feed of events.** Every event in Seamless's log - a memory
   written, a write held, a read refused, a session started - goes to a Unix
   socket the monitor listens on, as metadata: who, what, where, when and how it
   went, and never the content.
2. **A commit-matching snapshot.** One JSON file, replaced whole every few
   minutes, lists which sessions ran when and in which repository, which tasks a
   commit may name, and the commits agents recorded on each. A monitor watching
   a repository's main branch matches that branch's commits against it: a commit
   pushed from outside every agent session and every task is the one worth a
   look.

Both are optional and off by default, and off is exactly the earlier behavior:
no socket is dialed, no file is written, and no tool changes shape. Each is a
small generic contract, so any program that speaks it can be the monitor. Use
either or both.

| | Metadata feed | Commit-matching snapshot |
|---|---|---|
| Answers | What agents are doing, as it happens | Which commits came from tracked work |
| Delivery | HTTP/1.1 batches pushed to a Unix socket | One JSON file the monitor reads |
| Contract | Wire format `v` 1 | `kind` `seamless_feed`, `version` 1 |
| Carries | Kinds, codes, counts, sizes, flags and ids | Session names, task ids, repository names, times and commit ids |
| Needs | [Write screening](https://thereisnospoon.org/docs/guides/write-screening/) on, and `screening.feed_socket` | `commits.snapshot_file`; `commits.record` adds the commit ids |

## The metadata feed, version 1

With the feed on, the daemon reads its own event log, in the order events
landed, from a cursor it keeps in the spool, so a burst never loses an event.
Each event becomes one **frame**, and frames travel in **batches** to the
**consumer**, the program listening on the socket. The version is the `v` on
every batch header, limits answer and receipt, and it is 1.

Within a version a frame may gain an optional name, one that appears only where
it applies, as `actor.account` did for
[restricted accounts](https://thereisnospoon.org/docs/guides/peer-auth/#restricted-local-accounts). A consumer
ignores a name it does not know. Removing or renaming a name, or changing what
one holds, takes a new version.

### What a frame carries

One JSON object per line on the wire, wrapped here to read:

```json
{"time": "2026-11-02T01:16:54.977Z", "host": "build-host",
 "class": "agent", "activity": "memory_held", "severity": "medium", "outcome": "blocked",
 "actor": {"kind": "agent", "id": "4242.1730000000", "session": "cc/0a1b2c3d-4e5f6a7b"},
 "target": {"system": "seamless", "asset": "myapp", "path_class": "memory"},
 "fields": {"event_id": "01KX0000000000000000000020", "hold_id": "01KX0000000000000000000021",
            "item_kind": "memory", "kind": "constraint", "op": "write",
            "reasons": "instruction.override", "source_seq": 4812, "tool": "memory_write"}}
```

| Field | What it holds |
|---|---|
| `time` | When the event happened, UTC to the millisecond |
| `host` | The daemon's host name, lowercase |
| `class` | `agent`, `auth`, `file`, `config`, `finding` or `health` |
| `activity` | The event's kind as one word: `memory.held` becomes `memory_held` |
| `severity` | `info`, `low`, `medium` or `high` |
| `outcome` | `success`, `failure`, `blocked` or `unknown` |
| `actor` | Who acted: `kind` `agent`, with `session` the session's name and `id` its agent process (pid and start time); `kind` `user` with `id` `owner`; or `kind` `system` with `id` `seamless`. `account` names a restricted account (below) |
| `target.system` | `seamless` |
| `target.asset` | The project: its slug, `global` for the global scope, or a keyed hash (below) |
| `target.path_class` | The item kind, when the event has one |
| `fields` | The event's metadata |

`fields` comes from an allowlist of metadata keys, each with a pattern its value
must fit:

- **Codes and kinds:** `kind`, `item_kind`, `tool`, `op`, `hook`, `via`,
  `reason`, `surface`, `change`, `by`, `action`, `status`, `from`, `to`,
  `where`, `mode`, `step`, `outcome`, `proof`.
- **Numbers:** `size`, `bytes`, `memories`, `notes`, `projects`, `added`,
  `failures`, `duration_ms`, `items`, `safe`, `flagged`, `suppressed`,
  `item_count`, `emitted_tokens`.
- **Flags:** `held`, `quarantined`, `baseline`, `on`, `updated`, `truncated`,
  `is_error`, `ambient`, `favorite`, `matched`.
- **Ids:** `hold_id`, `released_hold_id`, `superseded_by`, `credential_id`, and
  `item_ids` joined with commas.
- **Lists of codes, joined with commas:** `verdicts`, `reasons`.
- **Versions:** `version`, `current_version`.

Beside those, an event's frame has `event_id` and, when the event names an item,
`item_id`. `access.refused` adds `target_project`, and
`settings.features_changed` adds one `feature_<name>` flag per feature.

Nothing else reaches a frame, and a value that does not fit its pattern (free
text where a code belongs) is dropped. So a frame never carries arguments,
results, bodies, prompts, queries, names or titles.

**A restricted account names itself.** Every event recorded for a
[restricted local account](https://thereisnospoon.org/docs/guides/peer-auth/#restricted-local-accounts)'s
request carries `actor.account`, the account's name, whatever the surface (its
agents' tool calls, hooks and A2A recalls) and whatever the event: writes, held
proposals, task changes, session starts and ends, and refused reaches, its
refusal at the console's door included. In one of its sessions `session` is the
session's qualified name (`_example_agent:cc/...`, the name the console and
`seam` show); with no session bound the actor is `kind` `agent` with `account`
alone. An event filed under one of its sessions names the account even when
you caused it, as when you release or discard one of its proposals (those
frames carry `by` `owner` or `released_hold_id` in `fields`):

```json
{"time": "2026-11-12T09:00:00.000Z", "host": "build-host",
 "class": "auth", "activity": "access_refused", "severity": "medium", "outcome": "blocked",
 "actor": {"kind": "agent", "session": "_example_agent:cc/0a1b2c3d-4e5f6a7b", "account": "_example_agent"},
 "target": {"system": "seamless", "asset": "myapp"},
 "fields": {"event_id": "01KX0000000000000000000R01", "reason": "peer.unreachable", "source_seq": 4813,
            "surface": "mcp", "target_project": "h:401a93d78f42331c657f8eea", "tool": "memory_read"}}
```

Your own events never carry `account`, so your frames are exactly as they
were. An account's name is 1 to
64 of `A-Z a-z 0-9 . _ -`, not starting with `.` or `-`. A name outside that
pattern travels as `h:` and the first 24 hex digits of the name's SHA-256, so
the event still names the account. The hash is not keyed, so the monitor can
compute it from its own configuration, and the snapshot below names such an
account the same way. A session's name is 1 to 128 of
`A-Z a-z 0-9 . _ : / -` starting with a letter or digit, or, for a restricted
account's session, `<account>:` followed by such a name, 128 characters in
all. A session name outside both is left out; the frame still names the
account.

**A fenced project travels as a keyed hash.** A confidential or sealed project,
or one locked under [peer_auth](https://thereisnospoon.org/docs/guides/peer-auth/), appears only as `h:` and 24
hex characters, in `target.asset` and in `target_project` alike. The key is
random and lives in the spool folder as `hash.key`, so a project's hash is
stable across restarts and a monitor can follow one project without learning its
name. Remove the key and every project's hash changes.

### Class, severity and outcome

Most events are class `agent`, severity `info`, outcome `success`. These are
not:

| Event | Class | Severity | Outcome |
|---|---|---|---|
| `corpus.drift` | `file` | `high`; `medium` for a missing file | `blocked`; `unknown` for a missing file |
| `lockdown.changed` | `config` | `high` going on, `medium` going off | `success` |
| `access.refused` | `auth` | `medium` | `blocked` |
| `memory.held` | `agent` | `medium` | `blocked` |
| `settings.features_changed`, `project.isolation.changed` | `config` | `medium` | `success` |
| `update.failed` | `config` | `medium` | `failure` |
| `backup.restored` | `file` | `high` | `success` |
| `backup.failed`, `backup.restore_failed` | `file` | `medium` | `failure` |
| `backup.deleted` | `file` | `low` | `success` |
| `backup.created` | `file` | `info` | `success` |
| `memory.screened` | `agent` | `low` with a verdict, else `info` | `blocked` when held, else `success` |
| `memory.released`, `memory.discarded`, `corpus.confirmed`, `session.tainted`, `session.untainted` | `agent` | `low` | `success` |
| `hook.error` | `agent` | `low` | `failure` |
| `update.started`, `update.applied` | `config` | `low` | `success` |
| `update.available` | `config` | `info` | `success` |
| `corpus.reviewed` | `finding` | `info` | `success` |
| `tool.call` | `agent` | `info` | `failure` on a tool error, else `success` |

Write screening's own events, and what each one's payload holds, are in
[its event table](https://thereisnospoon.org/docs/guides/write-screening/#events).

A backup event's item is the backup's file name, which is not an id, so no
frame names the file. What the frames carry:

| Event | Fields |
|---|---|
| `backup.created` | `kind` (`scheduled`, `manual`, `before-restore`), `where` (`folder` or `local`), `bytes`, `memories`, `notes`, `projects` |
| `backup.failed` | `kind`, `where`, `reason` (a fixed word: `no-space`, `export`, `read-back`, `database`, `cancelled`, `unsafe-file`, `internal`, or `folder-` and the folder's state), and `failures`, the automatic streak, for a scheduled backup |
| `backup.deleted` | `kind`, `where`, `by` (`owner`, or `prune` for an old automatic backup) |
| `backup.restored` | `mode` (`replace` or `merge`), `kind`, `memories`, `notes`, `added` for a merge that added something, `by` (`console` or `cli`), `proof` (`owner`, `passkey` or `account`), and `credential_id`, the id of the approver passkey that approved it |
| `backup.restore_failed` | `mode`, `reason` (a fixed word), and `outcome` (`failed`, `expired`, `partial`) with `step` `apply`, or `step` `stage` when the restore could not be prepared |

### The two requests

Both are HTTP/1.1 on the socket, one request to a connection, and the feed waits
up to 30 seconds for an answer.

**`GET /v1/limits`** reads the consumer's limits, when the feed starts and
hourly after:

```json
{"v": 1, "frame_bytes": 16384, "batch_frames": 500, "batch_bytes": 1048576, "heartbeat_s": 300}
```

Those values are also the defaults, used until a consumer says otherwise. A
value that is missing, zero or negative keeps its default, and an answer that is
not status 200 or not version 1 changes nothing. Only `heartbeat_s` changes what
the feed does today. The other three are read but do not narrow what is sent: a
batch never exceeds 500 frames or 1 MiB, and a frame never exceeds 16,384 bytes,
so a consumer must accept at least the defaults.

**`POST /v1/batch`**, with `Content-Type: application/x-ndjson`, sends a header
line and one frame per line, each ending in a newline:

```text
{"v":1,"batch":"<id>","frames":<n>,"dropped":<n>}
{ one frame }
{ one frame }
```

`batch` is 1 to 64 of `A-Z a-z 0-9 . _ : -`, starting with a letter or digit, and
never repeats. `frames` is how many lines follow. `dropped` is how many frames
the feed has lost that the consumer has not yet recorded.

### Receipts

The consumer answers each batch with a JSON receipt, and its `action` decides:

| `action` | HTTP status | What the feed does |
|---|---|---|
| `settle` | 200 | Deletes the batch, and subtracts the receipt's `dropped` from its count of lost frames |
| `retry` | The rule's | Keeps the batch and sends it again after backing off |
| `discard` | The rule's | Deletes the batch and counts its frames as dropped |

A settle carries the `batch` id, a `status` of `stored` or `duplicate`, the whole
numbers `frames`, `accepted`, `refused_count` and `dropped`, and a `refused`
list:

```json
{"v": 1, "action": "settle", "status": "stored", "batch": "<id>",
 "frames": 2, "accepted": 2, "refused_count": 0, "refused": [], "dropped": 0}
```

A retry or a discard names one rule, at that rule's status, and a retry may add
`retry_after_s`:

| `rule` | Status | `action` |
|---|---|---|
| `sender` | 403 | `retry` |
| `path` | 404 | `retry` |
| `method` | 405 | `retry` |
| `type` | 415 | `retry` |
| `version` | 400 | `retry` |
| `store` | 503 | `retry` |
| `size` | 413 | `discard` |
| `newline` | 400 | `discard` |
| `header` | 400 | `discard` |
| `frames` | 400 | `discard` |

Any other reply - no JSON, another version, another batch's id, a number
missing, a status that does not fit the action - counts as a retry: only a
receipt settles or discards a batch. A retry, or a consumer it cannot reach,
backs the feed off from 5 seconds, doubling to 5 minutes; a `retry_after_s` sets
the wait inside those bounds.

### How delivery behaves

- **One batch in flight**, oldest first, resent unchanged, byte for byte, under
  its id until a receipt settles or discards it, across restarts too.
- **A bounded spool.** While the consumer is down the feed spools to
  `feed_spool`: at most 10,000 frames and 8 MiB, the oldest dropped first and
  counted in the next header's `dropped`.
- **Heartbeats.** When nothing else is waiting the feed sends a heartbeat frame
  every third of the consumer's `heartbeat_s`, and never more often than every
  10 seconds: class `health`, activity `heartbeat`, with `interval_s`, `spooled`
  and `dropped_count` in `fields`.
- **A running count.** `fields.source_seq` numbers the frames the spool took, so
  a frame dropped from it shows as a gap.

## The commit-matching snapshot, version 1

One JSON object, whose `kind` and `version` name the contract. (`seamless_feed`
is the contract's name; this file is not the metadata feed.)

```json
{
  "kind": "seamless_feed",
  "version": 1,
  "host": "build-host",
  "taken_at": "2026-11-02T01:16:54.977Z",
  "since": "2026-10-03T01:16:54.977Z",
  "sessions": [
    {"id": "cc/0a1b2c3d-4e5f6a7b", "repo": "myapp", "started_at": "2026-11-01T09:00:00.000Z",
     "ended_at": "2026-11-01T10:30:00.000Z",
     "commits": ["1111111111111111111111111111111111111111"]},
    {"id": "cx/0f0f0f0f-1e1e1e1e", "repo": null, "started_at": "2026-11-02T01:00:00.000Z",
     "ended_at": null},
    {"id": "_example_agent:cc/5a6b7c8d-9e0f1a2b", "account": "_example_agent", "repo": "myapp",
     "started_at": "2026-11-01T11:00:00.000Z", "ended_at": "2026-11-01T11:40:00.000Z",
     "commits": ["3333333333333333333333333333333333333333"]}
  ],
  "tasks": [
    {"id": "01ARZ3NDEKTSV4RRFFQ69G5FAV", "repo": "myapp",
     "commits": ["2222222222222222222222222222222222222222222222222222222222222222"]},
    {"id": "01ARZ3NDEKTSV4RRFFQ69G5FAW", "account": "_example_agent", "repo": "myapp",
     "commits": ["4444444444444444444444444444444444444444"]}
  ]
}
```

| Field | What it holds |
|---|---|
| `host` | The daemon's host name, lowercase |
| `taken_at` | When it was built, UTC to the millisecond |
| `since` | The start of what it covers: `snapshot_window` before `taken_at`, or the start of the first session when that is later. A commit seen before it is outside the snapshot |
| `sessions[].id` | The session's name - the `cc/...` or `cx/...` on a briefing's `Seam session` line, or a `sess/*` name, and for a restricted account's session its qualified name (`_example_agent:cc/...`) - so a commit message that mentions it matches. A name outside the contract's pattern, or one another listed session already took, is replaced by the session's ULID |
| `sessions[].account` | The restricted account that started the session; left out for yours |
| `sessions[].repo` | The repository the session's working directory is in, or `null` |
| `sessions[].started_at`, `ended_at` | When it started, and when it ended - `null` while it is active. A session idle longer than `gardener.session_idle_minutes` has ended at its last activity, the end the idle reaper gives it: never at the moment it was reaped, and whether or not the reaper runs |
| `tasks[].id` | A task's ULID, for a commit message that names one |
| `tasks[].account` | The restricted account behind the task: the one whose session recorded one of its commits (the earliest such record), else the one that created it, else the one whose words its text carries. Left out when none is, as for your own tasks |
| `tasks[].repo` | Its project's repository, when the project maps exactly one; left out otherwise |
| `commits` | The ids recorded on the session or task, oldest first; left out when there are none |

Which entries it lists: every session live at some point since `since`, and
every task that is open or in progress or changed since `since` (closing a
task and recording its commits both change it). Up to 100,000 of each: past
that, the sessions that ended earliest are left out and `since` moves past them,
so the snapshot still covers everything it claims to, and the tasks changed
least recently are left out.

It carries identifiers and times only: never a finding, a title, a body, a
path or a project slug.

**Names and patterns.** A session id is 1 to 128 of `A-Z a-z 0-9 . _ : / -`
starting with a letter or digit, or, for a restricted account's session, an
account name and `:` followed by such a name, 128 characters in all. An
`account` is the name, 1 to 64 of `A-Z a-z 0-9 . _ -` not starting with `.` or
`-`, or, for a name outside that, `h:` and the first 24 hex digits of the name's
SHA-256, as the metadata feed sends it.

**`account` within version 1.** `account`, and session ids in the qualified
form, appear only for sessions and tasks of a restricted account, so the
snapshot of an install that lists none under `peer_auth.restricted` (the
default) is exactly as it was. The check below holds every object to its
names, and a consumer that does the same must accept `account` as optional:
one that predates it refuses a snapshot listing a restricted account's work.

The daemon writes the snapshot as it starts and every `snapshot_interval` after.
Each write builds the whole document, checks it against the contract, writes it
beside the file and renames it into place, so a reader never sees half of one.
The check holds it to exactly the names above (the optional ones - `account`,
`commits` and a task's `repo` - may be absent or null), each value of its type
and on its pattern, no list past its bound, and no session id, task id or
commit in one entry given twice. A snapshot that would break the contract is not written, and
the last good file stays.

### Which repository a session is in

A session's repository is found from its working directory through the repo
map, exactly as its project is (the longest mapped path the directory lies
under, on the session's own host). The repository's name is the last segment
of its origin remote - `git@host:owner/myapp.git` gives `myapp` - or, with no
usable origin, the last segment of its path. Configure the monitor with the
same names. A working directory outside every mapped repository gives `null`,
and so does a name outside the contract's pattern.

A confidential or sealed project's repository is never named, and neither is
that of a project locked under [peer_auth](https://thereisnospoon.org/docs/guides/peer-auth/). Their sessions
and tasks are still listed, with their recorded commits: sessions by name, with
their times and a `repo` of `null`, and tasks by id, with `repo` left out. A
monitor that matches by time cannot match those sessions; a recorded commit, or
one whose message names the session or task, still matches.

## Turn on the metadata feed

The feed is part of [write screening](https://thereisnospoon.org/docs/guides/write-screening/), so it needs
that feature on as well as a socket to deliver to:

```yaml
features:
  screening: true
screening:
  feed_socket: /path/to/consumer/ingest.sock
  feed_spool: /path/to/spool               # default <data_dir>/feed
```

`SEAMLESS_SCREENING_FEED_SOCKET` and `SEAMLESS_SCREENING_FEED_SPOOL` override
the two paths, which expand a leading `~` and must be absolute. They are read at
start, so restart the daemon after a change. The feature itself can also be
[turned on from the console](https://thereisnospoon.org/docs/guides/write-screening/#turn-it-on), and takes
effect without a restart.

With no socket set the feed never starts. With a socket set and the feature off
it sends nothing, and turned on again it starts from the newest event rather
than replaying the gap.

The spool folder is the daemon's alone. It is created `0700`, and the feed
refuses one that is a symbolic link and, on macOS and Linux, one its group or
others can reach or another account owns. A feed that cannot start leaves the
daemon serving without it, and the log says `metadata feed not started`.

## Turn on commit records and the snapshot

```yaml
commits:
  record: true                                               # offer the commits argument
  snapshot_file: /path/to/outbox/snapshot-seamless_feed.json # write the snapshot here
  snapshot_interval: 5m                                      # default 5m, from 30s to 24h
  snapshot_window: 720h                                      # default 30 days, from 1h to a year
```

Each key has an environment override: `SEAMLESS_COMMITS_RECORD`,
`SEAMLESS_COMMITS_SNAPSHOT_FILE`, `SEAMLESS_COMMITS_SNAPSHOT_INTERVAL` and
`SEAMLESS_COMMITS_SNAPSHOT_WINDOW`. They are read at start, so restart the
daemon after a change. Neither half needs write screening.

`record` and `snapshot_file` switch on separately: a snapshot without recorded
commits still tells a monitor which sessions ran when, and where. Off, neither
tool offers the argument (a call that sends one is refused as an unknown
parameter, word for word as before), nothing is recorded, and no file is
written.

`snapshot_file` must be absolute (a leading `~` expands), and its directory must
already exist: the daemon creates nothing there but that file (and, for a
moment, a hidden temporary file beside it). The file's mode is `0640`: put it
in a directory only the daemon's account can write and whose group is the
monitor's. Whoever can write that directory can replace the snapshot, and with
it vouch for any commit.

### Recording commits

With `record` on, both tools list `commits` in their schema:

- `session_end commits=[...]` records the ids on the session being ended - the
  same session the call completes, behind the same project fence.
- `tasks_update id=<task> commits=[...]` records them on the task. Recording is
  an update like any other: it lands in the same transaction as the rest of the
  call, and a task another session holds via a live claim refuses it with
  `already claimed`, exactly as it refuses a retitle.

The argument takes an array, or a comma-separated string. Each entry must be a
full commit id - the 40-character SHA-1 or 64-character SHA-256 name
`git rev-parse HEAD` prints. An abbreviation is refused, never expanded: it can
name a different commit once the repository grows a colliding prefix.
Uppercase is read as lowercase, and a repeat counts once - within the call, and
against what the session or task already holds. One call records at most 256
distinct ids, and one session or task holds at most 10,000. Any bad entry
refuses the whole call: a refused `session_end` leaves the session open, and a
refused `tasks_update` changes nothing, its other fields included.

A successful call reports `commits_recorded`, and its event
(`session.ended`, `task.transition`) carries the same count. The ids
themselves live in the record, never in an event.

## What it does not cover

- **The feed has gaps it can only count.** Frames dropped past the spool's bound
  are counted, not kept. Nothing is spooled while write screening is off. If the
  event log is rewritten (a restore from an archive, say) and the event the
  cursor names is gone, the feed resumes at the newest event and logs a warning.
- **A record is an agent's claim.** Seamless checks that an id is a full commit
  id, never that the commit exists or that this session made it. Any caller
  allowed to end a session or update a task can vouch for a commit on it; the
  per-call bound keeps an agent from vouching for a whole history in one paste.
- **Agents must pass the ids.** Nothing reads git on an agent's behalf. A
  session that ends through the client's own hook (most Claude Code sessions)
  records no commits unless the agent called `session_end` with them; its
  times and repository still reach the snapshot.
- **A task's recorder needs a session.** The snapshot learns who recorded a
  task's commit from the session the recording call ran in. A commit a
  restricted account records on a task with no session bound names no recorder,
  so that task's `account` falls back to whoever created or rewrote it.
- **The snapshot is the record's view, not git's.** Commits that never reached a
  session's or task's record are matched, if at all, by the monitor's own
  rules - a message naming a listed session or task, or a commit time inside
  an open session of the same repository.
