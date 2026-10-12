# Sessions, memory & recall

> The nine tools an agent uses most - open a session, write, edit and read memory, and search the store.

These nine tools are the agent loop. In a repo mapped to a project, most of them
need no `project` argument at all: the connection is bound to a session -
automatically for a Claude Code or Codex agent, by `session_start` for any other
client - and everything inherits that session's scope.

## The shape of a session

`session_start` returns the project briefing and binds the session to the
connection. `session_end` persists findings for the next agent's briefing.

A Claude Code or Codex agent does not need `session_start`: its hooks open an
*ambient* session and bind the agent's tool calls to it
([how](https://thereisnospoon.org/docs/concepts/sessions/#process-binding)). Called from such an agent without
arguments, `session_start` hands back that same session, and its `scope` note
says the call was not needed; with a different `project` it starts a separate
session there. Every other client calls it once per connection, with `cwd` or
`project` - see [when to call it](https://thereisnospoon.org/docs/concepts/sessions/#when-to-call-session-start).
Whichever way it binds, the briefing it returns is for the project actually
bound.

`session_start` no longer takes `source`. The legacy values (`startup`,
`resume`, `clear`, `compact`, `explicit`) are accepted and ignored, so older
clients keep working; any other value is refused with
`session_start no longer takes source -- omit it`. The server derives what the
briefing needs from what the call did: resuming a named session adds the
resumed-session hint to re-ground with `recall`.

`session=` on `session_update` and `session_end` - and on the
[task tools](https://thereisnospoon.org/docs/reference/mcp/tasks/) - takes a session name (the `cc/...` or
`cx/...` on the briefing's `Seam session` line, or a `sess/*` name) or a session
ULID; one that matches nothing is an error saying what the argument takes. Both
tools also accept `summary` as an alias of `findings`.

If `tasks_update` ever fails claiming a task is held by *your own* session id,
the connection lost an explicit `session_start` binding - those live in daemon
memory, so a restart or reconnect drops them. Re-run `session_start` with the
same name to rebind. A Claude Code or Codex agent's automatic binding is not lost
this way.

## Edit, update, append, supersede, or delete?

Five ways to change memory, and picking the wrong one is how a store rots:

| You want to | Use | What happens |
|---|---|---|
| Fix part of a memory without resending it | `memory_edit` | Exact search/replace on the body, plus `description` and tag add/remove; returns a diff |
| Rewrite what a memory says, whole | `memory_write` with the same `name` | Updated in place; the id is stable |
| Add to the end without rereading it | `memory_append` | Body grows; nothing else changes |
| Replace a **different**, now-outdated memory | `memory_write` with `supersedes` | The old one is marked invalid, leaves every index, and stays readable with a pointer to its replacement |
| Remove something written by mistake | `memory_delete` | Gone |

The distinction that matters is **supersede vs. delete**. Superseding is how the
store stays honest about its own history: the old memory leaves the briefing and
recall, but an agent that follows an old reference still finds it, marked
invalid, pointing at what replaced it. Delete is for mistakes - things that were
never true - not for things that stopped being true.

A `supersedes` that fails is reported rather than swallowed: the new memory is
still written and kept, and the call returns an error naming it. The target is
then still active, so re-run the supersede.

### Edit vs. supersede

`memory_edit` is cheap, which is exactly why its boundary has to be explicit.
Edit is for changes that carry no new claim: a typo, broken formatting, a stale
path or command, a stage's `Status` flip, a description, a tag. If the **meaning**
changes - the conclusion is now different, the advice reversed - that is a new
memory, and it goes through `memory_write` with `supersedes`.

The reason is provenance. A supersession retires the old memory into readable
history; an in-place edit leaves no trace that the store ever believed something
else. Using edit to change what a memory claims silently rewrites the record.

`memory_edit` is also the only way to change a memory's `description` or tags
without rewriting its body, and `tags_remove` is the only way to clear a tag at
all - an empty `tags` array reads as absent everywhere else.

## Concurrency: content_hash and expect_hash

`memory_read` and `notes_read` return a `content_hash` - the SHA-256 of the whole
file. Pass it back as `expect_hash` on `memory_edit`, `notes_edit`, or
`notes_update`, and the write is refused if the stored file has moved on since
you read it. Omit it and the write is unconditional.

It answers a different question from the daemon's own serialization. Every
application write already goes through a per-file lock, so two agents can no
longer interleave a read and a write and lose one of them. What the lock cannot
see is an agent acting on something it read minutes ago, or the owner editing the
markdown in an editor - which is why the precondition is checked against the
**file**, inside the lock, rather than against the index (the watcher re-indexes
on a debounce, so the index would happily confirm the stale hash).

## Scope

`memory_write` **fails closed**: with no session and no explicit `project`, it is
rejected as ambiguous rather than silently landing in the global scope. Pass
`project: global` to write a deliberately cross-project memory.

`memory_edit` targets a memory that already exists, so it resolves like
`memory_append`: the session's scope first, then a global fallback. The write is
judged against the project the memory actually sits in.

## Recall is the only search tool

There is one search entry point. `recall` fuses FTS5 keyword matching and vector
similarity with reciprocal rank fusion, nudges the fused order by favorite and
[utility](https://thereisnospoon.org/docs/concepts/recall/#the-utility-nudge) (both bounded), scoped to the
current project plus global items, and packs results into a token budget. A call
that finds nothing is recorded as a miss - recurring misses become the
gardener's [memory-wanted proposals](https://thereisnospoon.org/docs/concepts/gardener/#what-it-looks-for).

The optional `kind` filter restricts hits to memories of one frontmatter kind.
It implies memories-only: combining it with `scope=notes` is rejected as
contradictory rather than returning a misleading empty result, and a
kind-filtered miss still counts as memory-wanted demand.

With `kind` set, `query` becomes optional: a kind alone is the **browse mode**
behind briefing hints like `recall kind=convention` - the scope's active
memories of that kind, listed newest-first under the same limit and token
budget. A browse is a listing, not a search: no fusion, no favorite or utility
boost, its hits record as passive exposure (never query-gated demand), and an
empty browse records no miss - "this project has no conventions yet" is not a
missing memory.

It degrades rather than fails: if the embedding provider is unreachable, recall
falls back to keyword-only results instead of erroring. A local misconfiguration
is surfaced instead of hidden - the two cases are deliberately not treated alike.

## Results and failures

| Call | Success result | Failure that matters |
|---|---|---|
| `session_start` | `session_id`, `name`, resolved `project`, explanatory `scope`, and `briefing`; resumed/adopted sessions also say `resumed: true`, and a `warning` flags a named `project` that was created or a remote `cwd` that could not be placed | Briefing assembly degrades to an empty string and logs; creating or binding the session itself still fails loudly, as does a `project` that contradicts the repository `cwd` is in |
| `memory_write` | Stable `id`, canonical `name`, resolved `project`, `updated`, optional `similar`, and optional `superseded` | An occupied tombstone path is an error; if the new memory lands but supersession fails, the tool errors while naming the kept replacement and the still-active target |
| `memory_edit` | `id`, `name`, `project`, the new `content_hash`, a unified `diff`, and a `stage_hint` when a `kind=stage` body still has no parseable `Status` | An `old_string` that matches zero or several places is an error naming the count, and **nothing** is written - the edits apply all-or-nothing. A stale `expect_hash` is refused rather than overwriting |
| `recall` | `hits`, possibly empty | Remote embedder failures degrade to lexical-only; local request/config construction errors surface |
| `session_end` | Confirmation of the close - `session_id`, `claims_released`, `mishaps_recorded`; findings persist for the next briefing | A missing/ambiguous session is an error rather than a fabricated successful close |

## session_start {#session_start}

Bind this connection to a work session and return its project briefing. Claude Code and Codex agents running the Seamless hooks are bound to their own session automatically (the 'Seam session' line in the briefing) and do not need this. Call it to bind a client without hooks, to move this connection to another project, or to resume a named session. Later calls inherit the session's project, so they need no project argument.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `cwd` | string | no | Absolute working directory. The git repository it is in decides the project, and a repository maps itself to a project on its first session -- no setup step (`seamlessd map-repo` only overrides the derived slug) |
| `host` | string | no | Machine this agent runs on (hostname). Defaults to the X-Seamless-Host header, then to the daemon's own host; pass it only when dialling a daemon on another machine |
| `main_worktree_root` | string | no | Absolute root of the repository's MAIN checkout when repo_root is a linked worktree; defaults to repo_root |
| `model` | string | no | Model id powering this agent, exactly as the provider names it (e.g. claude-fable-5, gpt-5.5). Stamped onto memories/notes this session writes; hooks keep it current for Claude Code/Codex sessions, so pass it mainly from other clients |
| `name` | string | no | Resume the session with this name: a sess/* name from an earlier call, or the cc/... or cx/... on your briefing's 'Seam session' line. A new name starts a separate session under that name; omit it to use your own session, or a fresh one |
| `project` | string | no | Project slug to bind. Defaults to the project of the git repository cwd is in, then to your own session's project. If cwd is in a repository too, the two must agree. project=global binds the global scope; an unknown slug creates that project. |
| `repo_origin` | string | no | The repository's origin remote URL, which is how the same repo checked out on two machines is recognized as one project |
| `repo_root` | string | no | Absolute git repository root enclosing cwd, resolved on YOUR machine. Required when host is not the daemon's: the daemon cannot read your filesystem to find it |

## session_update {#session_update}

Record interim progress on the current session (working findings so far). Uses the bound session unless you pass one.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `findings` | string | **yes** | Working findings / progress note so far |
| `session` | string | no | Session to operate on: a name (the cc/&lt;id&gt; or cx/&lt;id&gt; on your briefing's 'Seam session' line, or a sess/* name) or a session ULID. Defaults to this connection's session -- bound automatically for Claude Code and Codex agents -- so pass it only to act on another session, or when a call reports the session as ambiguous |
| `session_id` | string | no | Session ULID to operate on; takes precedence over session and the bound session |

## session_end {#session_end}

Complete the current session, persisting its findings for future briefings. Uses the bound session unless you pass one.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `findings` | string | **yes** | Final findings: what was learned, decided, or left open. Prefer a tight summary (briefings show a short preview), but long findings are stored in full -- they are not rejected. |
| `mishaps` | array | no | Self-report mishaps this session caused: an action a warning or convention said not to take, live state touched by mistake, a command that hit the wrong target. Pass an array with one short entry per incident; omit when none happened. When a mishap violated a stored memory, name that memory by its exact slug in the entry (e.g. "violated chroma-boot-race by ...") -- the report is then linked to it. Recorded for recurrence review, not blame -- report them even when fully recovered. |
| `session` | string | no | Session to operate on: a name (the cc/&lt;id&gt; or cx/&lt;id&gt; on your briefing's 'Seam session' line, or a sess/* name) or a session ULID. Defaults to this connection's session -- bound automatically for Claude Code and Codex agents -- so pass it only to act on another session, or when a call reports the session as ambiguous |
| `session_id` | string | no | Session ULID to operate on; takes precedence over session and the bound session |

## memory_write {#memory_write}

Create or update a durable memory -- the compact knowledge a future session must not miss (a constraint, gotcha, decision, runbook). Long-form write-ups belong in notes_create; put the one-line lesson here. Writing an existing name updates it in place (its id is stable). On a new name, a semantically similar existing memory is reported as an advisory hint; the write still proceeds. The hint is withheld when the target project's content is fenced from you (see the withheld marker on the response) -- the write lands either way. Pass supersedes to replace a DIFFERENT, now-outdated memory: it is marked invalid and leaves every index (briefing, recall) but stays readable with a pointer here. If superseding fails, the new memory is still written and kept; the error says how to retry.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `name` | string | **yes** | kebab-case identifier, unique within the project |
| `kind` | string | **yes** | memory kind; constraint = what any agent must or must not do regardless of task; convention = a project-local choice or layout fact (naming, branding, where things live or deploy, which files sync together); for kind=stage, open the body with Status: open\|in_progress\|blocked\|done and optionally Gate: human\|ai, and flip the status with memory_edit (or a full memory_write) -- append cannot change the header, which is parsed from the top of the body. One of: `constraint`, `convention`, `runbook`, `protocol`, `gotcha`, `decision`, `refuted`, `reference`, `stage`. |
| `description` | string | **yes** | one line, &lt;=150 chars -- the only text shown in indexes |
| `body` | string | **yes** | markdown body (aliases: content, text) |
| `project` | string | no | project slug; defaults to the bound/ambient session's project. An unknown slug CREATES that project -- naming a new one is normal and never an error. Pass project=global ONLY for knowledge that belongs in EVERY project's briefing; it is not a neutral default. With no session and no explicit project the call is rejected as ambiguous. A session bound to a confidential or sealed project can write ONLY into that project. |
| `session` | string | no | the session to act as: the name session_start returned, the cc/&lt;id&gt; or cx/&lt;id&gt; on your briefing's 'Seam session' line, or a session ULID. It scopes and credits this call. Defaults to this connection's session; pass it on every call when session_start reports the connection as stateless |
| `supersedes` | string | no | name of an existing memory this one replaces; that memory is marked superseded (invalid) and pointed here |
| `tags` | array | no | tags, replacing all (a comma-separated string is also accepted); omit to leave an existing memory's tags untouched, and note an empty list reads as absent, not as a clear |

## memory_append {#memory_append}

Append markdown to an existing memory's body. The memory keeps its id. To create a new memory, use memory_write.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `name` | string | **yes** | memory name |
| `body` | string | **yes** | markdown to append (aliases: content, text) |
| `project` | string | no | project slug; defaults to the bound/ambient session's project, then global. Pass project=global to target a global memory. |
| `session` | string | no | the session to act as: the name session_start returned, the cc/&lt;id&gt; or cx/&lt;id&gt; on your briefing's 'Seam session' line, or a session ULID. It scopes and credits this call. Defaults to this connection's session; pass it on every call when session_start reports the connection as stateless |

## memory_edit {#memory_edit}

Edit an existing memory in place with exact search/replace, instead of resending its whole body through memory_write. Each edit's old_string must match the current body exactly and uniquely (or pass replace_all); all edits apply together or none do, so a failed match changes nothing. It is also the only way to change a memory's description or tags on their own: send description/tags_add/tags_remove with no edits and the body is left untouched (memory_write requires a body, so there was no metadata-only path before this), and tags_remove is the only way to clear a tag at all. Returns a unified diff of what landed plus the new content_hash. Use this for corrections that do not change what the item CLAIMS: typos, broken formatting, a stale path or command, a stage's Status flip, metadata. If the MEANING changes -- the conclusion is now different, the advice reversed -- that is a new memory: use memory_write with supersedes, which retires the old one into readable history instead of erasing what it used to say.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `name` | string | **yes** | memory name (kebab-case, as memory_read takes it) |
| `description` | string | no | replace the one-line description (&lt;=150 chars -- the only text shown in indexes); omit to leave it untouched |
| `edits` | array | no | ordered list of exact search/replace edits, applied in order to the CURRENT body. Each is {old_string, new_string, replace_all?}. old_string must match the body EXACTLY (whitespace and indentation included) and must be unique unless replace_all is true; include surrounding lines to make it unique. All-or-nothing: if any edit fails to match, nothing is written. |
| `expect_hash` | string | no | optional precondition: the content_hash you last read for this item (memory_read/notes_read return it). The write is refused if the stored file has changed since -- another agent or the owner edited it -- so re-read and re-apply your change instead of overwriting theirs. Omit it to write unconditionally. |
| `project` | string | no | project slug; defaults to the bound/ambient session's project, then global. Pass project=global to target a global memory. |
| `session` | string | no | the session to act as: the name session_start returned, the cc/&lt;id&gt; or cx/&lt;id&gt; on your briefing's 'Seam session' line, or a session ULID. It scopes and credits this call. Defaults to this connection's session; pass it on every call when session_start reports the connection as stateless |
| `tags_add` | array | no | tags to add, leaving the rest in place (a comma-separated string is also accepted) |
| `tags_remove` | array | no | tags to remove, leaving the rest in place; this is how a tag gets cleared (a comma-separated string is also accepted) |

## memory_read {#memory_read}

Read a memory by name within the current project (falling back to a global memory of the same name), or directly by id. It reads Seamless memories only -- a name listed in your client's own MEMORY.md (Claude Code's file-based auto-memory) is a local file, not a Seamless memory: open it with your file tools.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `id` | string | no | memory id (ULID), as carried by events, recall results, and gardener proposals; bypasses name/project resolution |
| `name` | string | no | memory name; pass exactly one of name or id |
| `project` | string | no | project slug; defaults to the bound session's project |
| `session` | string | no | the session to act as: the name session_start returned, the cc/&lt;id&gt; or cx/&lt;id&gt; on your briefing's 'Seam session' line, or a session ULID. It scopes and credits this call. Defaults to this connection's session; pass it on every call when session_start reports the connection as stateless |

## memory_delete {#memory_delete}

Delete a memory by name: the markdown file leaves the disk and its index row goes with it, with no provenance and no pointer left behind. Prefer nearly anything else. To replace knowledge that turned out to be wrong, use memory_write with supersedes -- the old memory drops out of every index (briefings, recall) but stays readable, pointing at what replaced it, which is how a later reader learns the thing was reconsidered rather than that it was never believed. To retire something merely stale, leave it for the gardener's archive proposal. Reserve deletion for memories written by mistake: a duplicate, a test, a write into the wrong project.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `name` | string | **yes** | memory name |
| `project` | string | no | project slug; defaults to the bound session's project |
| `session` | string | no | the session to act as: the name session_start returned, the cc/&lt;id&gt; or cx/&lt;id&gt; on your briefing's 'Seam session' line, or a session ULID. It scopes and credits this call. Defaults to this connection's session; pass it on every call when session_start reports the connection as stateless |

## recall {#recall}

Search durable knowledge (memories, notes) and the work record (tasks, trials, session findings) by meaning and keyword (fused), scoped to the current project plus global items. This is the single search entry point. Work-record hits carry a status (a task's status, a trial's outcome) and match on keyword only. With kind set and no query it lists that memory kind newest-first instead (browse).

| Parameter | Type | Required | Description |
|---|---|---|---|
| `kind` | string | no | only memories of this frontmatter kind (e.g. convention); implies memories-only, so any scope that excludes memories is rejected; with no query, lists the kind newest-first. One of: `constraint`, `convention`, `runbook`, `protocol`, `gotcha`, `decision`, `refuted`, `reference`, `stage`. |
| `limit` | integer | no | maximum results (default 10, max 100) |
| `project` | string | no | project slug; defaults to the bound session's project |
| `query` | string | no | what you are looking for; required unless kind is set (kind alone lists that kind newest-first) |
| `scope` | string | no | what to search (default all). One of: `all`, `memories`, `notes`, `tasks`, `trials`, `sessions`. |
| `session` | string | no | the session to act as: the name session_start returned, the cc/&lt;id&gt; or cx/&lt;id&gt; on your briefing's 'Seam session' line, or a session ULID. It scopes and credits this call. Defaults to this connection's session; pass it on every call when session_start reports the connection as stateless |
