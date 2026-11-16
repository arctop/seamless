# Repo locks

> The four repo lock tools - declare a lock on a repository's refs once, then hold it with a lease while landing changes, so landings happen one at a time.

A repo lock guards a repository's refs, so agents on several machines land
changes one at a time ([how repo locks work](https://thereisnospoon.org/docs/concepts/repo-locks/)). These
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

## locks_list {#locks_list}

List repo locks: each lock's repository and name, the refs it guards, whether it is held, by which session (when you may see that session), since and until when, the repository identities that resolve to it, and the projects whose restricted accounts the declaration lists. Pass a checkout's identity (cwd, or repo_root and repo_origin) to list only that repository's locks. A declaration waiting for the owner's approval is listed with state requested. Read-only.

**Optional** - part of *Repo locks*, hidden when the `repo_locks` feature is disabled: the tool leaves `tools/list` and a call to it is refused as an unknown tool, while its stored data stays exactly where it was. See [Optional features](https://thereisnospoon.org/docs/reference/console/#optional-features).

| Parameter | Type | Required | Description |
|---|---|---|---|
| `cwd` | string | no | Absolute working directory inside the repository's checkout. The daemon reads the checkout's git identity from it only on its own machine with peer_auth off; otherwise pass repo_root (and main_worktree_root, repo_origin) resolved on your machine. `seam lock` sends all of them |
| `host` | string | no | Machine the checkout is on (hostname). Defaults to the X-Seamless-Host header, then to the daemon's own host |
| `main_worktree_root` | string | no | Absolute root of the repository's MAIN checkout when repo_root is a linked worktree; defaults to repo_root |
| `repo_origin` | string | no | The repository's origin remote URL: the key a lock is found by from every machine, so the ssh and https forms of one repository are one lock. A side clone whose origin is a local path is the repository at that path |
| `repo_root` | string | no | Absolute root of the checkout, resolved on its own machine |
| `session` | string | no | the session to act as: the name session_start returned, the cc/&lt;id&gt; or cx/&lt;id&gt; on your briefing's 'Seam session' line, or a session ULID. It scopes and credits this call. Defaults to this connection's session; pass it on every call when session_start reports the connection as stateless |

## locks_declare {#locks_declare}

Declare a lock on a repository's refs, once per repository and name: the lock agents hold while they land changes, so landings happen one at a time across machines. Name the repository by its checkout (cwd, or repo_root, main_worktree_root and repo_origin): its origin URL is the key, so the ssh and https forms of one repository are one lock, and a repository with no origin is locked by host and path. Declaring again, from any project or machine, returns the existing lock unchanged and records this checkout's identity on it, so a repository that gains or changes its origin keeps its lock. A restricted account's declaration waits for the owner's approval on the console. refs defaults to refs/heads/&lt;name&gt;; projects, the projects whose restricted accounts may hold the lock, defaults to the project the checkout maps to.

**Optional** - part of *Repo locks*, hidden when the `repo_locks` feature is disabled: the tool leaves `tools/list` and a call to it is refused as an unknown tool, while its stored data stays exactly where it was. See [Optional features](https://thereisnospoon.org/docs/reference/console/#optional-features).

| Parameter | Type | Required | Description |
|---|---|---|---|
| `cwd` | string | no | Absolute working directory inside the repository's checkout. The daemon reads the checkout's git identity from it only on its own machine with peer_auth off; otherwise pass repo_root (and main_worktree_root, repo_origin) resolved on your machine. `seam lock` sends all of them |
| `host` | string | no | Machine the checkout is on (hostname). Defaults to the X-Seamless-Host header, then to the daemon's own host |
| `main_worktree_root` | string | no | Absolute root of the repository's MAIN checkout when repo_root is a linked worktree; defaults to repo_root |
| `name` | string | no | The lock's name on its repository, a token: main (the default), release. A line naming a lock shows its repository and its name: LOCK seamless main |
| `projects` | array | no | Project slugs whose restricted accounts may hold the lock (a comma-separated string is also accepted). Defaults to the project the checkout maps to |
| `refs` | array | no | The guarded refs: full ref names (refs/heads/main) or patterns with one '*' (refs/tags/v*), at most 16 (a comma-separated string is also accepted). Defaults to refs/heads/&lt;name&gt; |
| `repo_origin` | string | no | The repository's origin remote URL: the key a lock is found by from every machine, so the ssh and https forms of one repository are one lock. A side clone whose origin is a local path is the repository at that path |
| `repo_root` | string | no | Absolute root of the checkout, resolved on its own machine |
| `session` | string | no | the acting agent's session: the cc/&lt;id&gt; or cx/&lt;id&gt; on your briefing's 'Seam session' line, another session name, or a session ULID. Defaults to this connection's session -- bound automatically for Claude Code and Codex agents -- then a sole active ambient. Pass it when a call reports the agent as ambiguous: the bare call then fails rather than guesses. |
| `session_id` | string | no | the acting agent's session ULID; takes precedence over session and the bound session |

## locks_acquire {#locks_acquire}

Take a repo lock for the current session, with a lease, before landing changes on the refs it guards. Re-acquiring a lock you hold refreshes the lease: the heartbeat for a long landing. A lock another session holds under a live lease is refused, naming the holder when you may see that session, since and until; a lapsed lease is taken over. A declaration still waiting for the owner cannot be held. Name the lock by id, or by the checkout and name (default main). Release it with locks_release once the refs have landed; a hold also lapses at the end of its lease.

**Optional** - part of *Repo locks*, hidden when the `repo_locks` feature is disabled: the tool leaves `tools/list` and a call to it is refused as an unknown tool, while its stored data stays exactly where it was. See [Optional features](https://thereisnospoon.org/docs/reference/console/#optional-features).

| Parameter | Type | Required | Description |
|---|---|---|---|
| `cwd` | string | no | Absolute working directory inside the repository's checkout. The daemon reads the checkout's git identity from it only on its own machine with peer_auth off; otherwise pass repo_root (and main_worktree_root, repo_origin) resolved on your machine. `seam lock` sends all of them |
| `host` | string | no | Machine the checkout is on (hostname). Defaults to the X-Seamless-Host header, then to the daemon's own host |
| `id` | string | no | lock id (from locks_list or locks_declare); or name the checkout instead |
| `lease_seconds` | number | no | lease duration in seconds before the hold lapses (default 900, at most 14400) |
| `main_worktree_root` | string | no | Absolute root of the repository's MAIN checkout when repo_root is a linked worktree; defaults to repo_root |
| `name` | string | no | The lock's name on its repository, a token: main (the default), release. A line naming a lock shows its repository and its name: LOCK seamless main |
| `repo_origin` | string | no | The repository's origin remote URL: the key a lock is found by from every machine, so the ssh and https forms of one repository are one lock. A side clone whose origin is a local path is the repository at that path |
| `repo_root` | string | no | Absolute root of the checkout, resolved on its own machine |
| `session` | string | no | the acting agent's session: the cc/&lt;id&gt; or cx/&lt;id&gt; on your briefing's 'Seam session' line, another session name, or a session ULID. Defaults to this connection's session -- bound automatically for Claude Code and Codex agents -- then a sole active ambient. Pass it when a call reports the agent as ambiguous: the bare call then fails rather than guesses. |
| `session_id` | string | no | the acting agent's session ULID; takes precedence over session and the bound session |

## locks_release {#locks_release}

Release a repo lock the current session holds, live or lapsed, so another session can take it. Only the holder may release; the owner can force-release any hold from the console. Name the lock by id, or by the checkout and name (default main).

**Optional** - part of *Repo locks*, hidden when the `repo_locks` feature is disabled: the tool leaves `tools/list` and a call to it is refused as an unknown tool, while its stored data stays exactly where it was. See [Optional features](https://thereisnospoon.org/docs/reference/console/#optional-features).

| Parameter | Type | Required | Description |
|---|---|---|---|
| `cwd` | string | no | Absolute working directory inside the repository's checkout. The daemon reads the checkout's git identity from it only on its own machine with peer_auth off; otherwise pass repo_root (and main_worktree_root, repo_origin) resolved on your machine. `seam lock` sends all of them |
| `host` | string | no | Machine the checkout is on (hostname). Defaults to the X-Seamless-Host header, then to the daemon's own host |
| `id` | string | no | lock id (from locks_list or locks_acquire); or name the checkout instead |
| `main_worktree_root` | string | no | Absolute root of the repository's MAIN checkout when repo_root is a linked worktree; defaults to repo_root |
| `name` | string | no | The lock's name on its repository, a token: main (the default), release. A line naming a lock shows its repository and its name: LOCK seamless main |
| `repo_origin` | string | no | The repository's origin remote URL: the key a lock is found by from every machine, so the ssh and https forms of one repository are one lock. A side clone whose origin is a local path is the repository at that path |
| `repo_root` | string | no | Absolute root of the checkout, resolved on its own machine |
| `session` | string | no | the acting agent's session: the cc/&lt;id&gt; or cx/&lt;id&gt; on your briefing's 'Seam session' line, another session name, or a session ULID. Defaults to this connection's session -- bound automatically for Claude Code and Codex agents -- then a sole active ambient. Pass it when a call reports the agent as ambiguous: the bare call then fails rather than guesses. |
| `session_id` | string | no | the acting agent's session ULID; takes precedence over session and the bound session |
