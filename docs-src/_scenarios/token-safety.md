---
title: "“Persist the refresh tokens”: why one agent stored them raw and one hashed them"
description: Two real sessions given the same persistence task. One mirrors an in-memory map into a raw-token SQL column; one reads a recorded rule first and stores only SHA-256 hashes.
scene: token-safety
order: 3
---

Some dangers are invisible in the code. An in-memory token store translated 1:1
into SQL still builds, still passes the restart-survival test, still closes the
task - and now every live session's refresh token sits in plaintext in a table
that reaches disk, backups, and read replicas. Refresh tokens are bearer
credentials: a leaked snapshot of that column is account takeover for every
active user. Nothing in the diff looks wrong. The knowledge that makes it
wrong is operational, not syntactic - and an agent reasoning only from the
repository cannot derive it.

The two real Claude Code sessions below were both told: *persist the refresh
tokens to the database so sessions survive a restart*. The without-side does
exactly what was asked, competently - explores the store, ports the maps to
tables "translated 1:1", verifies restart survival, reports done. The
with-side sees one extra thing: the [Seamless](/) briefing lists a recorded
memory called `persist-refresh-tokens`, reads it at the moment of persisting,
and stores only a SHA-256 hash of each token - then adds a test asserting no
raw token is ever written. Same prompt, same repo, same model; the transcripts
are unedited and the session id is on each pane.

<!-- transcript -->

## The memory file that made the difference

```
---
id: 01JZFB93JA738YS86HFKMQTB2R
kind: gotcha
name: persist-refresh-tokens
description: Persist refresh tokens to the database the safe way: one hard rule about what the token column may store.
project: myapp
created: 2026-07-07T11:03:19Z
updated: 2026-07-07T11:03:19Z
valid_from: 2026-07-07T11:03:19Z
invalid_at: null
superseded_by: null
source_session: 01JZFJ3QK7Y2M8D4R6T1VNB5XC
---

When you persist refresh tokens, store only a SHA-256 hash of each token,
never the raw value; on rotate, hash the presented token and look it up by
hash. A database snapshot, backup, or read-replica leak of a raw-token column
is instant account takeover for every live session -- refresh tokens are bearer
credentials. Hashing makes a stolen snapshot useless. The in-memory store keeps
raw tokens today, which is fine for process memory but not for anything that
reaches disk or a backup.
```

The description deserves a second look: it deliberately does *not* contain the
answer. It is written as a pointer - "one hard rule about what the token
column may store" - so the one-line index entry in the briefing invites the
read at the right moment without letting the rule degrade into a
half-remembered summary. The body then states the rule, the reason, and the
rotate-path consequence. That division of labor - description as retrieval
surface, body as payload - is the craft the
[write good memories guide](/docs/guides/write-good-memories/) teaches.

## How to reproduce this

Seed the fixture from the repo - the `myapp` project with this memory among
its nine:

```sh
git clone https://github.com/arctop/seamless && cd seamless
go run ./cmd/demoseed -scenes -data /tmp/seamless-demo -repo /path/to/your/test/repo
SEAMLESS_DATA_DIR=/tmp/seamless-demo go run ./cmd/seamlessd serve
```

Then give your agent a task whose safe version depends on a fact the code
cannot reveal, with and without the [hooks](/docs/claude-code/) installed. The
`myapp` source was a small fictional Go auth service and is not committed. For
a real setup, start at the [quickstart](/docs/quickstart/).
