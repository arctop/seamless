---
title: Repo locks
description: The four repo lock tools - declare a lock on a repository's refs once, then hold it with a lease while landing changes, so landings happen one at a time.
generate: mcp-tools
tools:
  - locks_list
  - locks_declare
  - locks_acquire
  - locks_release
---

A repo lock guards a repository's refs, so agents on several machines land
changes one at a time ([how repo locks work](/concepts/repo-locks/)). These
tools belong to the **Repo locks** optional feature, off by default: while it
is off they are neither listed nor callable.

## Naming the repository

A lock has no project, so these tools take none. They name the repository by
its checkout, with the arguments `session_start` takes: `cwd`, `host`,
`repo_root`, `main_worktree_root` and `repo_origin`. The daemon reads the
checkout's git identity from `cwd` only on its own machine with peer_auth off;
from anywhere else, send `repo_root` (and `main_worktree_root`, `repo_origin`)
resolved on the checkout's own machine. `seam lock` always does.

`locks_acquire` and `locks_release` also take a lock's `id` (from `locks_list`
or `locks_declare`) in place of the checkout and the name.

## What an answer shows

A lock answers with its `label` (the repository and the name), its `refs`, its
`state` (`active`, or `requested` for a declaration waiting for the owner),
whether it is `held`, `since` and `until` for a hold, its `aliases` and its
`projects`. A holder is named (`holder`, `holder_session`) only to a caller who
may read that session; a project is listed only where the caller may list it.

Every lock also answers with:

- `held_by_caller` - true when the lock is held, and by the session this
  connection is bound to: by the agent-process identity its requests carry, or
  by a `session_start` on it. A connection bound to no session reads false,
  whatever session an unbound call would act as.
- `waiting` - how many sessions wait for the lock.
- `next_waiter_session` - the session first in the queue, only where the caller
  may read that session (as for the holder).

`locks_list` adds `caller_bound` beside `locks`: whether this connection is
bound to an active session at all, the same binding `held_by_caller` reads.

A [restricted account](/guides/peer-auth/#repo-locks) sees no `aliases` (the
list is empty), and no caller sees the aliases of a lock a restricted account
declared. A lock a restricted account may not reach is not listed for it, and
by id, by checkout or through `locks_declare` it reads exactly as a lock that
does not exist.

## Waiting for a lock

`locks_acquire` with `wait: true` joins the lock's queue when the lock cannot
be yours now, or keeps your place in it, and answers `acquired: false`,
`queued: true`, your `position` (1 is next) and `place_until`, when your place
lapses unless you ask again. Ask again within two minutes to keep the place;
`seam lock acquire --wait` does so every few seconds. While anyone waits, only
the session first in the queue takes the lock; anyone else's acquire is
refused with the next waiter and their own place. A `locks_release` from a
session that waits and does not hold the lock leaves the queue and answers
`released: false`, `left_queue: true`.

A declaration from a conversation that may not write the global scope (bound
to a confidential project, or confined by `isolation.requests`) is refused in
the fence's own words, and such a conversation's `locks_acquire` records no new
repository identity on the lock.
