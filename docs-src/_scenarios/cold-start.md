---
title: "Your agent starts every session from zero. Here's what it costs"
description: Two real Claude Code sessions, same repo, same prompt - one continues yesterday's plan from a briefing, one re-derives the work from a TODO and re-ships a known bug.
scene: cold-start
order: 1
---

An AI coding agent starts every session with no memory of the previous one.
Whatever the last session learned - the plan step it finished, the constraint it
discovered, the bug it already shipped and fixed once - is gone when the context
window closes, and the next session starts from the repository alone. The cost
is not just re-reading files: it is re-deciding things that were already
decided, without the evidence that decided them.

This page shows that cost concretely. Two real headless Claude Code sessions
were given the identical prompt - *continue where we left off* - in the
identical repo, against the same model. The only difference is whether Seamless
is installed. Without it, the agent finds a `TODO` comment, infers the task, and
ships an in-process rate limiter - the exact per-instance bug a recorded memory
warns about, because behind a load balancer each instance only counts its own
slice of traffic. With it, the session opens with an injected briefing: the
plan is 4/6 done, one step is claimable, and the `rate-limit-not-in-memory`
gotcha is one `memory_read` away at the moment it matters.

[Seamless](/) is shared memory and a task queue for the coding agents you
run: persistent memory, dependency-aware tasks, and shared plans, stored as
markdown files you own and indexed by one local daemon. The transcripts below are
the recorded sessions, unedited - whole steps are collapsed into fast-forward
markers, but no line is reworded. The session id is on each pane.

<!-- transcript -->

## The memory file that made the difference

The with-side agent did not get lucky. `rate-limit-not-in-memory` was recorded
by an earlier session, and it is a plain markdown file on disk - the SQLite
index could vanish and this would not:

```
---
id: 01JZFJ4S6M3T9BWKS60AQNDAAX
kind: gotcha
name: rate-limit-not-in-memory
description: In-memory rate limit on the refresh endpoint resets per instance; use shared storage.
project: myapp
created: 2026-07-08T14:21:07Z
updated: 2026-07-08T14:21:07Z
valid_from: 2026-07-08T14:21:07Z
invalid_at: null
superseded_by: null
source_session: 01JZFJ3QK7Y2M8D4R6T1VNB5XC
---

A rate limiter kept in a process map only sees one instance's traffic. myapp
runs several instances behind the load balancer, so an attacker's requests fan
out across them and each instance sees a fraction of the limit -- the endpoint
is effectively unthrottled. Keep the counter in shared storage (Redis) keyed by
IP and token family, with a short sliding window.
```

Two things carry the weight here. The `description` is the only line the
session-start briefing shows, so it is written to be recognized in an index;
the body is what `memory_read` returns at the moment of action, and it names
the failure mode, the reason, and the fix. And because the file is the source
of truth - not an opaque database - you can read it, edit it, and `git diff`
it like anything else you own. [How memory works](/docs/concepts/memory/)
covers the lifecycle; [write good memories](/docs/guides/write-good-memories/)
covers the craft.

## How to reproduce this

The fixture is in the repo. Seed a throwaway instance with exactly this state -
the `myapp` project, its nine memories, and the `auth-refresh` plan at 4/6:

```sh
git clone https://github.com/arctop/seamless && cd seamless
go run ./cmd/demoseed -scenes -data /tmp/seamless-demo -repo /path/to/your/test/repo
SEAMLESS_DATA_DIR=/tmp/seamless-demo go run ./cmd/seamlessd serve
```

The `myapp` source itself was a small fictional Go auth service (it is not
committed; any repo with an obvious unfinished thread works). Run your agent in
the mapped repo twice with the same prompt: once vanilla, once with the
[Claude Code hooks](/docs/claude-code/) installed, and diff what comes back.
For a real setup, start at the [quickstart](/docs/quickstart/).
