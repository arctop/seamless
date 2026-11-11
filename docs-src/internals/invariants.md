---
title: Domain invariants
description: The rules plausible-looking code breaks - supersession, scope resolution, FTS and LIKE escaping, LLM degradation, automatic updates and release discipline - and why each exists.
---

These are the rules that reasonable code violates. Every one of them has a
sensible-looking alternative that compiles, passes review, and is wrong; each is
enforced somewhere specific, and the pointer here is where to look, not a
substitute for reading it.

## Supersession

Enforced in `internal/lifecycle`. Concepts: [Memory & notes](/concepts/memory/).

### `invalid_at` is the only authoritative "has left the indexes" predicate

`core.Memory.Active()` is `InvalidAt == nil`; SQL filters `WHERE invalid_at IS
NULL`.

**Never infer live-ness from an empty `superseded_by`.** An archived memory has
`invalid_at` set and `superseded_by` empty - it is equally gone, and it does not
point anywhere because nothing replaced it. Code that reads "no replacement,
therefore still valid" resurrects every archived memory into briefings and recall
at once, which is exactly the class of thing nobody notices until an agent acts on
a retired constraint.

### `lifecycle.Supersede` and `lifecycle.Archive` are the only writers

They are the only non-test writers of `invalid_at` and `superseded_by`. MCP, the
gardener, and the console all route through them. Do not stamp those fields by
hand.

They exist as one path because a supersession is not a field update: it stamps the
old memory, appends a tombstone to its body, rewrites its file, and updates the
index. Every hand-rolled version of that has been a subset of it.

### `valid_from` is never touched by supersession

`[valid_from, invalid_at)` is the memory's bi-temporal record: when it started
being believed, and when it stopped. Moving `valid_from` on supersession would
erase the first half and, with it, the ability to ask what the store believed at
some past moment.

### It is stamped exactly once, guarded on the way in

The guards are:

| Condition | Error |
|---|---|
| `old` is already invalid | `ErrAlreadyInvalid` |
| `old.ID == replacement.ID` | `ErrSelfSupersede` |
| replacement is invalid or empty | `ErrInvalidReplacement` |

**Acyclicity falls out of those guards** rather than out of any traversal. Every
edge points from an invalid memory to an active one, and nothing ever clears
`invalid_at` - so no cycle can form. There is no cycle check to lean on, because
there is nothing for one to find. Weaken a guard and you do not get a slow cycle
check; you get cycles.

### Pass the FULL body

Both functions append a tombstone to `old.Body` and rewrite the whole file.
Handing them an index row - which carries no body - truncates the memory to just
the tombstone. Re-read the file first.

This is the invariant most likely to be broken by code that looks correct: the
index row has every field the signature wants, so it type-checks, writes
successfully, and destroys the memory's content.

### A superseded name stays occupied

Reviving it is `ErrPathOccupied`, never a silent clobber. The superseded memory's
tombstone file still sits at `memory/{project}/{name}.md`, and overwriting it
would destroy the readable history that supersession exists to preserve.
`memory_delete` is the only escape hatch, and it drops that history - which is
why it is the escape hatch and not the default.

### A failed supersede is a tool ERROR, not a payload

When `memory_write` writes the new memory but the supersession fails, the call
returns an error naming the still-active target. It never returns a success
payload with an error nested inside it.

The reason is what an agent does with each. An error field inside a success
payload reads as success: the agent moves on, believing the old memory is retired,
and the store now serves two contradictory answers to the same question. An
explicit tool error makes the agent deal with it. The new memory is still kept -
its content is valid knowledge, and re-writing the same name is a lossless
in-place update, so fixing the target and retrying is safe.

## Mutating a corpus file

Enforced in `internal/files` (`mutate.go`). Concepts:
[Memory & notes](/concepts/memory/).

### Every read-modify-write goes through `Mutate`

A handler that reads an item, edits the struct, and writes the whole file back
is a lost update waiting to happen: two of them racing on one file both read the
same starting content, both render, and the second atomic rename wins. Nothing
errors - the index upsert is by id, so even the `UNIQUE file_path` constraint
stays quiet, and the losing agent is told its write succeeded.

`files.Manager.Mutate(path, fn)` takes a per-path lock for the duration of the
read *and* the write. `seamlessd` is a single daemon, which is what makes an
in-process lock a complete answer rather than a partial one: every application
write to the corpus goes through this one Manager.

The invariant is not "the write is locked" - it is **the read that feeds the
write happens inside the same lock**. Wrapping only the write fixes nothing, and
looks exactly like a fix.

`MutatePaths` locks several files in sorted order for a mutation that spans two
of them (a note moving project writes the new path and removes the old); the
sorted order is what keeps two opposite moves from deadlocking. The lock is not
reentrant, which is why `WriteMemory`/`WriteNote`/`Remove` deliberately take no
lock of their own - calling them from inside `fn` is the point.

### `expect_hash` is checked against the FILE, inside the lock

The mutation lock cannot see an agent acting on something it read minutes ago,
or the owner editing the markdown in an editor. That is what the optional
`expect_hash` precondition catches - and it must be compared against the hash of
the file as just read under the lock, never against the index row.

The watcher re-indexes on a 300 ms debounce. For that window the index still
reports the hash the stale caller is holding, so an index-based check would pass
on precisely the edit it exists to refuse.

## Session binding and scope

Enforced in `internal/mcp`. Concepts: [Projects & scope](/concepts/projects/),
[How tool calls find their session](/concepts/sessions/#process-binding).

### Never read `project` from tool args directly

Route it through `resolveReadScope` (nothing to infer → global) or
`resolveWriteScope` (nothing to infer → `errNoScope`). Both apply
`validateProjectArg`.

`validateProjectArg` is the path-traversal defense, and the reason it cannot be
skipped is specific: a project slug becomes a directory under `memory/` and
`notes/`. The data-dir boundary check alone does not catch `../notes/_global` -
that slug cleans to a path *inside* the data dir, just in the wrong tree, where
one item can clobber another's files.

### A durable create uses `resolveWriteScope`, because it fails closed

Reaching for `resolveReadScope` on a write silently lands the item in the global
scope. Nothing errors, nothing warns; a project's private knowledge is simply now
in front of every agent on the machine.

Failing closed is the whole point: with no bound session, no ambient session, and
no explicit `project`, the write is rejected as ambiguous and the agent must
choose. `project: global` says it on purpose.

### `resolveWriteScope` registers the slug it is given

A durable create into an unregistered slug used to write the file and its index row
while leaving the projects table untouched: the project existed for the write, yet
was absent from `project_list` and the console until some unrelated path (a
`session_start` in a mapped repo, `map-repo`, an import) happened to backfill the
row.

That gap made the fail-closed error above unanswerable in practice. An agent told
to "pass `project=<slug>`" had no cheap way to learn whether an unmapped slug
would be rejected, and an agent that cannot tell a new-project write from a broken
one picks `project=global` - the single scope that reaches every project's
briefing. The guard calls `EnsureProject` on the named slug, which is what lets
the tool descriptions and the scope error *promise* that a new slug creates its
project. The guidance and the behavior are the same fix; either alone is a lie.

A typo'd slug is registered too. That is the same orphan made visible rather than
a new failure: the typo already created a scope on disk, and a row the console
lists is where a wrong one can be noticed and merged.

### Precedence, and no guessing

<figure class="doc-figure" data-tone="warn" aria-labelledby="scope-guard-caption">
  <span class="figure-kicker">Scope resolution</span>
  <div class="doc-flow">
    <div class="flow-node emphasis"><span class="flow-step">1</span><strong>Explicit project</strong></div>
    <div class="flow-node"><span class="flow-step">2</span><strong>Bound session project</strong></div>
    <div class="flow-node"><span class="flow-step">3</span><strong>Sole unambiguous ambient</strong></div>
  </div>
  <figcaption id="scope-guard-caption">Resolution stops at the first available scope; ambiguity is an error, never a fourth fallback.</figcaption>
</figure>

Ambient sessions spanning more than one project are `errAmbiguousScope`, not a
guess. Inheriting the machine-latest ambient in that situation is exactly how a
write bleeds into a concurrent agent's project - and the agent that wrote it will
never see the mistake, because from its side the call succeeded.

### A connection binds two ways, and `getBinding` reads both

An explicit `session_start` binding - held in memory, keyed by the MCP client
session id, evicted on `session_end` and by an opportunistic sweep - wins over
the **process binding**: the single active ambient session stamped with the
agent process the request names in `X-Seamless-Agent-Process`
(`agent_binding.go`). The process binding is identification, not inference. It
lives in the session row and on every request, survives restarts and
reconnects, and counts as a binding for the isolation fence.

### One process, two live sessions: bind neither

Two active sessions stamped with one process are a host running several
sessions at once, so the call stays unbound. Never pick between them by recency
or cwd - that is the cross-agent bleed in a new place.

### An identified caller is never inferred into another agent's session

`store.AmbientScope.Caller` filters every ambient-fallback lookup: a caller that
named its process never resolves into a session stamped with a different one.
Once such a caller's own session has ended, the sole live ambient is by
construction another agent's - inheriting it would bleed a write into that
agent's project, and a bare `session_end` would complete that agent's work.

### Revive what the reaper expired, never what was completed

The process binding reactivates a session the idle reaper **expired**, because
a call from the same live process is proof of life. A session **completed** on
purpose - by `session_end` or the client's SessionEnd - stays ended.

### `session_start` adopts only the caller's own session

When the request names a process. The sole-same-cwd adoption heuristic is for
callers that name none; for one that does, a sole same-cwd ambient that is not
its own belongs to another agent.

### Agent process identities are opaque

Validate with `agentproc.Valid` at every boundary (the header, the hook query)
and compare for equality. Never parse one for its pid or start time.

### A lost binding does NOT error

It degrades to the ambient fallback. Never assume the binding you started with is
still there.

### Stamp provenance with `s.boundSession(ctx)`

Not with the raw binding.

### A new tool must be in `registerTools` AND bump `ToolCount`

Otherwise `doctor` fails its tool-count assertion. See
[Contributing](/internals/contributing/) for the full wiring, which also includes
`Catalog()`.

## FTS5 and LIKE escaping

Enforced in `internal/store`. Concepts: [Recall](/concepts/recall/).

### User text reaching `MATCH` goes through `ftsQuery`

`ftsQuery` (in `fts.go`) splits on non-alphanumeric runes, drops single-rune
tokens, quotes each remaining term, and ORs them. **Never build a MATCH
expression by concatenation.**

The concrete failure is small and total: FTS5 reads `-` as an operator, so a query
for `chroma-boot-race` handed to MATCH raw is parsed as a *subtraction* - and
finds nothing, or the wrong thing, without erroring. Quoting turns it into three
literal terms, which is what the user meant.

### `ftsQuery` returning `""` means "no usable token"

Callers treat that as no results - never as an unfiltered query. The distinction
matters because the empty string is the natural input to "no filter": a caller
that forwards it returns the entire corpus in response to punctuation.

### User text in `LIKE` goes through `escapeLikePrefix`

It escapes `\`, `%`, and `_`, **in that order**, with `ESCAPE '\'`. The order is
not cosmetic - escape the escape character last and you double-escape the escapes
you just added.

**Never LIKE-escape a value compared with `=`.** An equality comparison has no
wildcards, so escaping only corrupts the literal being matched.

## LLM degradation: remote vs local

Enforced in `internal/llm` and `internal/retrieve`.

The single distinction that organizes this whole area: **did a provider answer
badly, or did we fail to ask?**

| Sentinel | Meaning | Response |
|---|---|---|
| `ErrUnavailable` | The provider could not be reached. | **DEGRADE** |
| `ErrAuth` | The provider rejected the key. | **DEGRADE** |
| `ErrRateLimited` | The provider throttled us. | **DEGRADE** |
| `ErrConfig` | The request was never built. | **SURFACE** |

### Remote errors degrade

The provider answered badly or not at all, nothing in this process is wrong, and
the condition may clear on its own. `recall` drops to lexical-only, which is
honest: fewer results, from a leg that still works.

### `ErrConfig` surfaces

No provider was contacted. No retry helps. It will not clear.

Degrading here would trade one loud failure for quietly worse recall **for the
life of the daemon** - with nothing anywhere to tell the owner that semantic
search had stopped. A store that silently searches half as well is worse than one
that says it is broken.

### Classify at the `do` call sites

Use `doErr(op, err)`. Never hand-wrap a request-build failure as
`ErrUnavailable`; that is precisely the reclassification the taxonomy exists to
prevent.

### Why `base_url` is validated at construction

`NewEmbedder` and `NewChatClient` are the single construction points, and both
validate `base_url` there, because `url.Parse` accepts a bare host:
`"api.openai.com/v1"` parses fine, builds a perfectly valid request, and only
fails inside `Do` as an opaque transport error - indistinguishable from an
outage.

So the configuration typo would arrive dressed as `ErrUnavailable`, degrade
silently, and never be found.

That validation is exactly why `ErrConfig` should be **unreachable** at request
time. Which is exactly why it must not hide when it is reached: an unreachable
error that occurs is a defect, and defects should be loud.

### `DedupHint` and `files.embedItem` swallow every embed error on purpose

Leave them. Dedup is advisory and must never block a `memory_write`; indexing is
best-effort with a hash-retry (a failed embed clears the recorded content hash so
the next reconcile re-indexes and tries again). These are not oversights that
survived review - they are the two places where the "no fake results on error"
rule is deliberately traded for "never block the agent".

## Automatic updates

Enforced in `internal/update` (the checker and its policy) and in
`cmd/seamlessd` (`update_spawn*.go`, `update_engine.go`). Concepts:
[Automatic updates](/updating/#automatic-updates),
[The updater on your OS](/reference/service/#the-updater).

### The daemon decides; a detached updater acts

The daemon never installs anything in-process. Its checker picks the release
(`update.Target`) and the moment (`update.Decide`), then hands one attempt to
the `update.Spawner` for its OS. The spawner starts `seamlessd update --auto`
where stopping the service cannot reach it: a `setsid` child on macOS, a
`systemd-run --user` transient unit on Linux, and a detached child on Windows,
with an on-demand Scheduled Task as the fallback.

Never start the updater as a plain child, and never tie it to the daemon's
context. The installer it runs restarts the daemon, so a plain child dies with
the daemon halfway through the install. The daemon never holds `update.lock`
either: it only try-locks the file to see whether an updater is running, and
lets go at once. The updater, for its part, refuses to run inside the service's
own process tree (`outsideServiceCheck`).

### The more restrictive setting wins

The briefing and features overrides work simply: the console's stored row wins.
`update.Effective` works the other way round. An explicit `false` in the file or
environment is final and locks its console toggle. `check: false` means no
update traffic at all and `auto: false` means nothing installs unattended, and
neither may be one click away from untrue. `auto` is a permission, not a
promise: only an install that updates itself acts on it (`Install.NotifyOnly`:
the installer's, or a peer_auth system daemon's), and it is off whenever checks
are.

### Never spawn without the record on disk

`State.Spawn` is saved before `Spawner.Spawn`, and `update.started` is recorded
between the two. A daemon that dies right after spawning finds the spawn in its
state at the next start and settles it from the updater's record. If the state
cannot be saved, nothing is spawned.

### Wall time, and GitHub's clock

The monotonic clock stops while a laptop sleeps, so the checker strips it from
every reading and schedules on wall-clock deadlines. The soak (`min_age`) and
the `max_defer` deadline run on GitHub's clock: the last response's `Date`
header plus the time since. A spawn always rests on a release list fetched
within the last minute. That re-check catches a release pulled in the meantime,
and it measures the soak against a `Date` GitHub has just sent, so a local
clock set days ahead cannot skip the soak.

### Manual attempts are shown, never acted on

Only the attempts a daemon started feed blocks, the backoff and the pause:
automatic ones (`auto`) and the console's Update now (`now`). Only automatic
rollbacks count toward the pause after two releases in a row roll back. A
`seamlessd update` run by hand appears as the last attempt and changes nothing
the daemon decides, since the owner watched it happen.

### The attempt records are a frozen, additive format

`attempt.json` and `attempts.jsonl` are read by every release from v0.7.0 on,
whether older or newer than the one that wrote them. So a field may be added,
but never renamed, retyped or repurposed, and there is no schema number to bump.
Writers are strict (`ErrInvalidAttempt`). Readers ignore fields they do not
know, and skip a history line that does not decode. Only the holder of
`update.lock` writes either file.

A gate refusal is a fixed word in `refusal` (`not_installer`, `stale_binary`,
`self_check`, `auto_off`), written only on a finished attempt that stopped at
`gates` and that a daemon started. A reader keeps a word it does not know and
treats it as a plain failure. Surfaces key the owner's action on that word,
never on the record's `error` text.

## Release discipline

Enforced by review. It binds every release, not one package.

An automatic update rolls back to the release it replaced when the new one does
not come up, and an owner may pin an older release with `SEAMLESS_VERSION`. A
rollback puts the database back only when the new release had migrated it, and
it never touches memories, notes or the config. It also never reduces the set of
locked projects: the backup's database goes in place only if it holds every
console lock the migrated one does, and otherwise the migrated database stays.
So every release must leave an
install that the previous release can still run. These rules keep a rollback
working, and the last one keeps an update from breaking Codex's hooks unnoticed.

### Migrations stay additive

Add tables, columns and indexes. Never drop, rename or retype anything the
previous release reads. A rollback whose database restore fails, and every
deliberate downgrade, leaves the previous release running on the newer schema.
`serve` logs a warning and doctor's `schema version` row warns, and the daemon
still has to work.

### Startup file rewrites stay readable by the previous release

A daemon rewrites some files when it starts, the update state file among them.
The release before it reads whatever the newer one left, so such a file only
gains fields, and readers ignore the fields they do not know.

### New frontmatter fields are optional

The previous release reads the memory and note files a newer one wrote, and
keeps the keys it does not know across a rewrite (`Extra`). It writes new files
without the field, though, so a newer release must treat a missing field as
its zero value.

### Config keys are only ever added

The updater never edits `seamless.yaml`, and the config decoder is strict
(`KnownFields`). If a release renames or removes a key, it refuses a config
the previous release accepted. Its daemon then never comes up, and every update
to it rolls back.

### A release that changes Codex hook definitions says so in its notes

Codex runs a changed hook only after it is re-approved in Codex's `/hooks`. The
updated daemon notices, because it fingerprints `hooks.json` before and after
the update, and says so in the console and the briefing. But it says so only
on that machine, and only afterwards. The release notes are where an owner
learns it before updating.
