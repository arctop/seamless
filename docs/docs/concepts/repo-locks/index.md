# Repo locks

> A lock on a repository's refs - its main branch, or a tag pattern - that agents on several machines declare once and hold while they land changes, so landings happen one at a time.

A repo lock guards a repository's refs - its main branch, or a pattern such
as every release tag - so that agents working on several machines land their
changes one at a time. An agent declares a repository's lock once, takes it
before it lands, and lets it go when the refs have landed. While one agent
holds the lock, another agent's attempt to take it is refused, naming the
holder (when the asker may see that session), since when it has held it and
until when.

Repo locks are an [optional feature](https://thereisnospoon.org/docs/reference/console/#optional-features),
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

A declaration from a [restricted account](https://thereisnospoon.org/docs/guides/peer-auth/) waits for the
owner: it is stored as requested, nobody can hold it, and the owner approves
or discards it on the console's Locks page. Everyone else declares directly.

## Holding a lock

Holding a lock works the way [claiming a task](https://thereisnospoon.org/docs/concepts/lease-based-task-claiming/)
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
  a live [task claim](https://thereisnospoon.org/docs/concepts/sessions/#liveness).
- **A session's holds end with it.** `session_end` and the SessionEnd hook
  free every lock the session holds; the reaper frees the lapsed holds of a
  session it ends.
- **A lost hold is news to its holder.** A holder whose lapsed hold another
  session took, whose hold the owner force-released, or whose lapsed hold the
  reaper freed is told once, as for a [lost claim](https://thereisnospoon.org/docs/reference/hooks/#claim-notices):
  a `Lock lost: ...` line naming the lock by id and name.

A lock is a coordination signal between cooperating agents: holding one tells
every other agent that the refs it guards are being landed. It does not, by
itself, stop anyone from pushing.
