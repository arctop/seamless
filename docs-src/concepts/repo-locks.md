---
title: Repo locks
description: A lock on a repository's refs - its main branch, or a tag pattern - that agents on several machines declare once and hold while they land changes, so landings happen one at a time.
---

A repo lock guards a repository's refs - its main branch, or a pattern such
as every release tag - so that agents working on several machines land their
changes one at a time. An agent declares a repository's lock once, takes it
before it lands, and lets it go when the refs have landed. While one agent
holds the lock, another agent's attempt to take it is refused, naming the
holder (when the asker may see that session), since when it has held it and
until when.

Repo locks are an [optional feature](/reference/console/#optional-features),
off by default. Turn them on in the console under **Settings → Features**
(**Repo locks**), or in the configuration file with `features.repo_locks:
true`, or with `SEAMLESS_FEATURES_REPO_LOCKS=true`. While the feature is off,
the four `locks_*` tools are neither listed nor callable, the Locks screen is
gone, and `seam lock` says the feature is off. Turning it off deletes no lock.

## A lock belongs to a repository

A lock is never a project's. Project mappings are path prefixes, not
repositories: one mapping can cover many repositories, a repository can be
carved out of it for another project, and sessions of two projects work in one
repository by design. A lock owned by a project could not be unique, and two
projects could each lock the same repository and split the mutex. So a lock
has no project, and every surface shows it with its repository and its name:
`LOCK seamless main`.

A lock carries no free text either. Its state is the resource (its guarded
refs), the holder (a session), since and until.

## Declared once, keyed by the origin

`seam lock declare` (or the `locks_declare` tool) declares a lock from inside
the repository's checkout. The repository is named by its git identity, which
`seam lock` reads on its own machine:

- **The origin URL is the key.** It is normalized, so the ssh and https forms
  of one repository (`git@host:owner/repo.git`, `https://host/owner/repo`) are
  one lock, from every machine.
- **Host and path are the fallback.** A repository with no origin is locked by
  its machine and its main checkout root. Every checkout seen is recorded the
  same way, so a repository that gains or changes its origin keeps its lock.
- **A side clone is its origin's repository.** A clone whose origin is a local
  path is a checkout of the repository at that path.

A second declaration of the same repository and name, from any project or any
machine, returns the first lock unchanged and only records the new checkout's
identity on it. Two locks on one repository and name cannot exist.

A lock's name is a token (`main` by default, or `release`), and it guards
`refs/heads/<name>` unless the declaration names other refs: full ref names
under `refs/`, or patterns with one `*`, such as `refs/tags/v*`. A declaration
also lists the projects whose restricted accounts may hold the lock (by
default the project the checkout maps to).

A declaration from a [restricted account](/guides/peer-auth/) waits for the
owner: it is stored as requested, nobody can hold it, and the owner approves
or discards it on the console's Locks page. It must list a project the account
may write, or the account could not see its own lock. Everyone else declares
directly.

A declaration writes its refs and the checkout's identity where every caller
reads them, so it is a write into the global scope: a conversation that may not
write there (one bound to a confidential project, or confined by
`isolation.requests`) is refused a declaration, and records no new identity of
a checkout when it acquires a lock. It still lists, acquires, heartbeats and
releases locks.

## Holding a lock

Holding a lock works the way [claiming a task](/concepts/lease-based-task-claiming/)
does, with the same compare-and-set:

- **Acquire** takes a free lock for the current session, with a lease (900
  seconds by default, at most 4 hours).
- **Heartbeat** is acquiring again: the holder's lease is refreshed, and the
  hold keeps the time it began.
- **A lapsed lease is free.** A hold is live only while its lease has not run
  out; at the moment it expires, another session may take the lock, and the
  answer says it took over a lapsed hold.
- **Release** lets the lock go; only the holder may.
- **Force-release** is the owner's override, on the console's Locks page (or
  `seam lock release --force`): it ends any hold, live or lapsed.
- **A hold keeps its session live.** While its lease is live, the idle reaper
  does not end the session holding it, however quiet its landing goes, as with
  a live [task claim](/concepts/sessions/#liveness).
- **A session's holds end with it.** `session_end` and the SessionEnd hook
  free every lock the session holds; the reaper frees the lapsed holds of a
  session it ends.
- **A lost hold is news to its holder.** A holder whose lapsed hold another
  session took, whose hold the owner force-released, or whose lapsed hold the
  reaper freed is told once, as for a [lost claim](/reference/hooks/#claim-notices):
  a `Lock lost: ...` line naming the lock by id and name.

## Waiting for a lock

A busy lock is taken first come, first served. `seam lock acquire --wait` (or
`locks_acquire` with `wait`) joins the lock's queue instead of being refused,
and asks again every few seconds until the lock is the session's:

- **No barging.** While anyone waits, only the head of the queue may take the
  lock, the moment it is free or its hold lapses. Anyone else is refused with
  the next waiter and their own place in the queue (or that they are not in
  it). The holder's own heartbeat is never refused.
- **A place lasts while its waiter keeps asking.** A waiter that has not asked
  again within two minutes loses its place, and so does one whose session ended
  or is no longer live, so a killed or interrupted wait never blocks the
  sessions behind it. A waiter that asks again after losing its place joins at
  the back.
- **The grant is an ordinary hold.** The head takes the lock with its next
  attempt, with a lease of its own, and leaves the queue; every other surface
  sees an ordinary holder.
- **Leaving.** `locks_release` from a session that waits and does not hold the
  lock leaves the queue. `seam lock acquire --wait --timeout 10m` gives up after
  ten minutes: it leaves the queue and exits 1. Interrupting it leaves the queue
  too. A session's places end with the session, as its holds do.

`seam lock acquire --wait` is made for an agent harness's background mode: it
prints its place on stderr when the place changes, and exits 0 once the
session holds the lock, so the agent is re-invoked then. `seam lock list` and
the console's Locks page show each lock's queue: how many sessions wait, and
which is next (named where the reader may see that session).

## What a session is told

With repo locks on, a session that starts in a checkout of a repository with
a declared lock - any checkout or worktree of it, on any machine - is told in
its briefing, before it does anything:

- one `LOCK` line per lock it may see: `free`, or who holds it, for how long
  and until when, with the queue's length while anyone waits;
- `LOCK hooks off` when its checkout runs no lock hook;
- the other live sessions in a checkout of the repository, by name with their
  idle minutes, counting the ones it may not see.

Its prompts then repeat a lock's line once whenever the lock's holder
changes, so a session waiting for a lock learns when it is free. A lock
outside the session's reach, and a declaration still waiting for the owner,
get no line. The lines and their rules are in
[the hooks reference](/reference/hooks/#repo-locks).

## Who may see and hold a lock

A lock has no project, so no project fence judges it:

- **The owner** sees and holds every lock, from any conversation on any host:
  a lock's state carries no content.
- **A [restricted account](/guides/peer-auth/#repo-locks)**, on this machine or
  through a relay, sees and holds (and waits for) a lock only when one of its
  listed projects is a project the account may write, judged on every request.
  The owner alone sets the listed projects. Outside that reach the lock does not
  exist for it: not listed, and refused in a missing lock's words.
- **A holder or a waiter is named** only to a reader who may see that session;
  anyone else reads "another session". A restricted reader sees no repository
  identity at all, and no agent sees the identities of a lock a restricted
  account declared; the console shows them to the owner.

A lock is a coordination signal between cooperating agents: holding one tells
every other agent that the refs it guards are being landed. It does not, by
itself, stop anyone from pushing.
