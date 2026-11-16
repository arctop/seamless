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
