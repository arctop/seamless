---
title: Record commits for an integrity monitor
description: Commit records - agents name the commits they made on session_end and tasks_update, and the daemon writes a snapshot of its sessions, tasks and those commits that an integrity monitor matches a branch's commits against.
---

An integrity monitor watching a repository's main branch wants to know which
commits came from tracked work and which came from nowhere: a commit pushed
from outside every agent session and every task is the one worth a look.
Seamless already knows which sessions ran when, and in which repository, and
which tasks exist. **Commit records** let it say so, in two halves:

1. **Agents record their commits.** `session_end` and `tasks_update` take an
   optional `commits` argument: the full ids of the commits a session made or
   a task's work produced, recorded on that session or task.
2. **The daemon writes a snapshot.** One JSON file, replaced whole every few
   minutes, lists the sessions live in a window - name, repository, start and
   end - and the tasks a commit in it may name, each with the commits it
   recorded. The monitor reads that file and matches its own list of the
   branch's commits against it.

Both are off by default, and off is exactly the previous behavior: neither
tool offers the argument (a call that sends one is refused as an unknown
parameter, word for word as before), nothing is recorded, and no file is
written. They switch on separately: a snapshot without recorded commits still
tells a monitor which sessions ran when, and where.

## Turn it on

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
daemon after a change. `snapshot_file` must be absolute (a leading `~`
expands), and its directory must already exist: the daemon creates nothing
there but that file (and, for a moment, a hidden temporary file beside it).

## Recording commits

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

## The snapshot

With `snapshot_file` set, the daemon writes the snapshot as it starts and every
`snapshot_interval` after. Each write builds the whole document, checks it
against the contract below, writes it beside the file and renames it into
place, so a reader never sees half of one. A snapshot that would break the
contract is not written, and the last good file stays. The file's mode is
`0640`: put it in a directory only the daemon's account can write and whose
group is the monitor's. Whoever can write that directory can replace the
snapshot, and with it vouch for any commit.

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
     "ended_at": null}
  ],
  "tasks": [
    {"id": "01ARZ3NDEKTSV4RRFFQ69G5FAV", "repo": "myapp",
     "commits": ["2222222222222222222222222222222222222222222222222222222222222222"]}
  ]
}
```

| Field | What it holds |
|---|---|
| `host` | The daemon's host name, lowercase |
| `taken_at` | When it was built, UTC to the millisecond |
| `since` | The start of what it covers: `snapshot_window` before `taken_at`, or the start of the first session when that is later. A commit seen before it is outside the snapshot |
| `sessions[].id` | The session's name - the `cc/...` or `cx/...` on a briefing's `Seam session` line, or a `sess/*` name - so a commit message that mentions it matches. A name outside the contract's pattern is replaced by the session's ULID |
| `sessions[].repo` | The repository the session's working directory is in, or `null` |
| `sessions[].started_at`, `ended_at` | When it started, and when it ended - `null` while it is active. A session idle longer than `gardener.session_idle_minutes` has ended at its last activity, the end the idle reaper gives it: never at the moment it was reaped, and whether or not the reaper runs |
| `tasks[].id` | A task's ULID, for a commit message that names one |
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

## Which repository a session is in

A session's repository is found from its working directory through the repo
map, exactly as its project is (the longest mapped path the directory lies
under, on the session's own host). The repository's name is the last segment
of its origin remote - `git@host:owner/myapp.git` gives `myapp` - or, with no
origin recorded, the last segment of its path. Configure the monitor with the
same names. A working directory outside every mapped repository gives `null`,
and so does a name outside the contract's pattern.

A confidential or sealed project's repository is never named, and neither is
that of a project sealed to another OS account under
[peer_auth](/guides/peer-auth/): their sessions and tasks are still listed - by
name or id, with their times and recorded commits - with `repo` `null`, the way
the metadata feed sends such a project's slug only as a keyed hash. A monitor
that matches by time cannot match those sessions; a recorded commit, or one
whose message names the session or task, still matches.

## What it does not cover

- **A record is an agent's claim.** Seamless checks that an id is a full commit
  id, never that the commit exists or that this session made it. Any caller
  allowed to end a session or update a task can vouch for a commit on it; the
  per-call bound keeps an agent from vouching for a whole history in one paste.
- **Agents must pass the ids.** Nothing reads git on an agent's behalf. A
  session that ends through the client's own hook (most Claude Code sessions)
  records no commits unless the agent called `session_end` with them; its
  times and repository still reach the snapshot.
- **It is the record's view, not git's.** Commits that never reached a
  session's or task's record are matched, if at all, by the monitor's own
  rules - a message naming a listed session or task, or a commit time inside
  an open session of the same repository.
