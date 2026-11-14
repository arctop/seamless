# Sessions & briefings

> How an agent gets your knowledge injected before it does anything - ambient sessions, how its tool calls bind to them, the briefing's packing order, and what never gets dropped.

A Seamless **session** is one agent's stretch of work. A **briefing** is what
that agent gets handed at the start of it, before it has called a single tool:
a SessionStart hook - installed for [Claude Code](https://thereisnospoon.org/docs/claude-code/) or
[Codex](https://thereisnospoon.org/docs/codex-cli/) - resolves the working directory to a project, assembles
constraints, plan rollups, and recent findings inside a token budget, and
injects the result into the agent's context. A client without hooks gets a
briefing only by calling `session_start` itself.

This is the mechanism that makes Seamless ambient rather than opt-in. An agent
does not have to remember to ask what it knows; it begins already knowing your
constraints.

## Ambient vs. explicit sessions

| | Ambient | Explicit |
|---|---|---|
| Opened by | The SessionStart hook, automatically | `session_start` |
| Named | `cc/<prefix>-<digest>` (Claude Code) or `cx/<prefix>-<digest>` (Codex) | Whatever you pass, or generated |
| Gets | The briefing, injected at session start | The same briefing, returned by the call |
| Binds tool calls | Automatically, through the [agent process](#process-binding) | On the connection that called it |

The ambient handle keeps the first eight external-ID characters readable and
adds 64 stable SHA-256 bits. Seamless resolves lifecycle activity by the full
external ID plus client, so two UUIDv7 sessions sharing a timestamp prefix never
share scope, findings, or provenance. Pre-upgrade handles keep their old names
when resumed. The handle is a display label, not an API: treat `cc/...` and
`cx/...` as opaque and never parse or construct them.

They are not two competing sessions. Without a `name`, `session_start`
**adopts** an agent's existing session rather than opening a second one - that
adoption rule exists because the alternative was double-counting every agent.
From Claude Code or Codex it adopts only the agent's **own** session, which
stays exact even with several agents in one repo (a different `project` is
[the one exception](#when-to-call-session-start)). A client that names no agent
process adopts the sole active ambient session sharing its working directory. A
caller that names its process but owns no live session is never handed another
agent's.

That distinction also appears in knowledge provenance. A write attributed by an
ambient hook stores the session's `cc/...` or `cx/...` **name** in
`source_session`; a write made through a bound MCP connection stores the
session's ULID. Readers resolve both forms. Do not infer client, scope, or
liveness from the spelling of `source_session`.

## How tool calls find their session {#process-binding}

The SessionStart hook and the agent's MCP connection reach the daemon on two
channels that share nothing on the wire: the hook carries the client's session
id, the connection only an opaque transport id. What they share is the process
that launched both - the `claude` or `codex` process - and that is the join:

1. `seam hook session-start` names the agent process - the nearest ancestor
   that is not a shell, as `<pid>.<start time>` - in its `agent_process` query
   parameter, and the daemon stamps it on the ambient session.
2. The MCP transports the same agent launches - the `seam mcp-proxy` stdio
   bridge, `seam mcp-headers` (Claude Code's `headersHelper`), and the `seam`
   CLI's own dial - send that identity in an `X-Seamless-Agent-Process` header
   on every request.
3. The daemon binds each call to the single active ambient session on the
   caller's machine stamped with it.

So a Claude Code or Codex agent never needs `session_start`. Unscoped `recall`,
`memory_write`, `notes_create`, and `tasks_*` calls resolve to the agent's own
project even while agents are active in several repos, and `session_update`,
`session_end`, and `tasks_claim` without `session=` act on the agent's own
session. The binding lives in the session row and on every request, not in
daemon memory, so it survives a daemon restart and an MCP reconnect - unlike the
per-connection binding `session_start` makes.

Its edges are deliberate:

- **An explicit binding wins.** A `session_start` on the connection takes
  precedence over the process binding, and a call's own `session=` argument
  takes precedence over both, for that call only.
- **One process, one session.** If two live ambient sessions are stamped with
  the same process - a host process running several sessions at once - nothing
  is bound automatically, and resolution falls back to the earlier rules: an
  explicit binding, then the sole-ambient-project inference in
  [Projects & scope](https://thereisnospoon.org/docs/concepts/projects/).
- **Never another agent's session.** A caller that names its process but owns
  no live session - say, after ending its own - is not inferred into anyone
  else's: that inference skips sessions stamped with a different process.
  Unstamped sessions from older clients are still candidates. So a bare call
  from an agent whose session has ended - a read as much as a write - is refused
  as ambiguous scope instead of landing in another agent's project or quietly
  falling back to the global scope, and a bare `session_end` cannot complete
  another agent's session.
- **It needs the current `seam`** on the agent's machine. An older `seam` sends
  no identity, and everything behaves as before.
- **The `seam` CLI is bound by the same rule.** A `seam recall` an agent runs
  through its shell is attributed to that agent's session. `seam` run by hand
  in a terminal is not bound: its nearest non-shell ancestor is the terminal or
  tmux, which no hook names.
- **No hook, nothing to bind to.** A hand-integrated client or the
  [Claude app chat](https://thereisnospoon.org/docs/claude-app/) has no SessionStart hook and so no ambient
  session. It still calls `session_start`, and calls it again after a
  reconnect.

Like the explicit binding, it is self-reported: it tells Seamless which session
a call belongs to - for scope, and for
[project isolation](https://thereisnospoon.org/docs/concepts/project-isolation/#honest-limits) - but it is not
an authentication boundary.

### Home versus binding {#home}

With [access requests](https://thereisnospoon.org/docs/concepts/project-isolation/#asking-for-access)
(`isolation.requests`) on, two things that are usually the same come apart for
a Claude Code or Codex agent:

- **Its home** is the project its conversation's own session is in: the
  repository's project when it starts, and after an approved
  [move](https://thereisnospoon.org/docs/concepts/project-isolation/#moves) the project it moved into. The
  fences judge every call from the home, and the conversation's record goes
  there.
- **Its binding** - the process binding, a `session_start`, or a call's
  `session=` - still chooses which session a call is credited to and where a
  call that names no project lands. It never changes what the fence lets
  through: a binding or a `session=` handle that would have the conversation
  act as a session outside its home is honored only where the home may read
  and write, and refused anywhere else, with the rule and the remedy. So after
  a move, a connection that `session_start` bound to the old home is refused
  until an argument-less `session_start` hands it back to the conversation's
  own session; the process binding follows the move by itself.

A client that sends no agent-process identity has no home: its binding is
judged as it always was.

### When to call `session_start` {#when-to-call-session-start}

- **From a client without hooks**, once per connection, with `cwd` (the
  repository it is in decides the project) or `project` (names it; an unknown
  slug creates the project, and the result's `warning` says so). Given both,
  they must agree: a `cwd` in a repository mapped to a different project is an
  error. With neither, the session is global.
- **To move this connection to another project.** A `project` other than your
  own session's starts a separate session there - linked to yours, so a
  SessionEnd closes both - and binds the connection to it. An argument-less
  `session_start` hands the connection back to your own session. Moving into a
  confidential or sealed project also moves
  [where your conversation's record goes](#moved-record). With
  `isolation.requests` on, a Claude Code or Codex agent's `session_start` moves
  the connection only where its [home](#home) may read and write: into a
  confidential or sealed project it binds nothing and files the request its
  home and that project call for - a move into it for good, or a grant of it -
  and the refusal names it; into an open project or the global scope it is
  refused once the conversation holds a fenced project's content.
- **To resume a named session.** `name` takes a `sess/*` name or the `cc/...` or
  `cx/...` on the briefing's `Seam session` line; a new name starts a separate
  named session.

The result carries the briefing for the project actually bound and a `scope`
note saying how that project was chosen. When the call only adopted your own
session, the note says so: you did not need it.

### Where a moved conversation's record goes {#moved-record}

A Claude Code or Codex agent's hooks keep describing the repository it runs
in, so its ambient session stays in that repository's project when
`session_start` binds its tool calls somewhere else. The conversation's own
record - the SessionEnd harvest (for Codex, each `Stop`'s provisional
findings), plan captures and planning-subagent reports - follows the
[isolated projects](https://thereisnospoon.org/docs/concepts/project-isolation/) the conversation holds,
counting the ambient session's project and every session `session_start`
linked to it, ended or not:

| The conversation | Its record goes to |
|---|---|
| holds no confidential or sealed project | the ambient session, as always |
| started in one (its ambient session is there), whatever it binds itself to | the ambient session, as always |
| started outside them, and bound itself to one with `session_start` | the session `session_start` opened there (the newest, if it opened several) |
| started outside them, and bound itself to two or more | none of them |

- **Moved, the record stays moved.** Once a conversation that started
  outside them binds itself to an isolated project, its harvest and captures
  go there for the rest of the conversation, and so do their events: an
  argument-less `session_start`, a move to an open project, or `session_end`
  on the isolated session does not bring them back. In the harvest's place
  the ambient session's findings become a pointer -
  `Continued in project <slug>; this conversation's findings go there, not here.` -
  and its `session.ended` event carries that line, never the harvest.
- **Your own findings win.** If that session already has findings you wrote
  with `session_update` or `session_end`, the harvest does not replace them;
  findings the hooks wrote there, such as an earlier harvest, it does replace.
  A SessionEnd that gives the harvest to a session the idle reaper had expired
  also completes it.
- **A home keeps its record.** A conversation that started in a confidential
  or sealed project keeps its harvest and captures there, under its ambient
  session, whatever it binds itself to - other isolated projects included -
  with no pointer and no line.
- **Two isolated projects, no record.** A conversation that started outside
  them and bound itself to two or more keeps its harvest and captures in none
  of them. The ambient session, and each active isolated session without
  findings of your own, takes a line naming the projects in the harvest's
  place, and the hook's notice says why.
- **What stays where it was.** Token totals are metadata and stay on the
  ambient session. Briefings and prompt recall still follow the repository
  while `isolation.requests` is off. A session native to an isolated project,
  whatever it binds itself to, a move between open projects, and a client that
  cannot link its session (an older `seam` that names no agent process) record
  exactly as before.

This applies on every install, whether or not
[`isolation.requests`](https://thereisnospoon.org/docs/reference/configuration/#isolation-requests) is on.
With it on, the routed record also meets the
[clearance rule](https://thereisnospoon.org/docs/concepts/project-isolation/#the-one-rule): where what the
conversation holds does not clear the isolated session's project, its harvest
and captures land nowhere, and the notice says so. An identified conversation
cannot bind itself into an isolated project with the switch on, so for one,
the next section is the rule.

### With `isolation.requests` on, the record follows the home {#home-record}

Everything the hooks record for a conversation goes to its [home](#home),
wherever the agent works, once the clearance rule clears it there:

- **Findings.** The SessionEnd harvest (for Codex, each `Stop`'s findings) and
  the findings a remote host pushes land on the conversation's own session in
  its home. After an approved move that is the child session in the project it
  moved into; the session it left keeps its own findings and the move's
  pointer.
- **Captures.** Plan captures and planning-subagent reports land in the home's
  project, with their events.
- **Recall.** Prompt recall and a subagent's briefing read the home, plus the
  projects its live grants open; the constraints are the home's.
- **Resume.** A resumed or compacted conversation stays in its home and is
  briefed from there. It moves to the repository's project only where its home
  may write there - never the child of a move, and never into a sealed project
  from an open one. A home that is locked to the agent, whose lease has ended,
  gets the locked notice and a request instead of a briefing.
- **A home that is not cleared records nothing.** An open home that holds
  fenced content - which only a read grant filed without the conversation's
  identity gives it, or under `peer_auth` what the same agent process held
  before `/clear` - takes no harvest and no capture. Its session's findings
  become one line, and the hook's notice says why:
  `Not recorded: this session holds content from example-finance, and its findings may not be written into project example-ops.`
  The events about its prompts carry none of their text. Briefings skip that
  line, and a move's pointer, as they skip a session with no findings.
- **`/clear`** starts a new conversation without `peer_auth`, with an empty
  label. Under `peer_auth` the label stays with the agent's process tree, so a
  new conversation in the same process still holds what the old one read.
- **Token totals** after a move count only what came after it: the child
  session reports the conversation's totals minus every earlier session's.

With the switch off, every hook is as the previous section describes.

## An actual briefing, annotated

This is a real briefing, in the order the assembler packs it:

```text
Actual packing order budgeted
<seam-briefing>
Seam project: seamless -- 62 memories (24 constraints, 9 conventions, 1 stage), 3 recent findings.
Constraints (binding for every session):
- errcheck-check-blank-two-category-rule: errcheck runs with check-blank ...
- llm-degradation-remote-vs-local: llm errors split remote ...
... 2 more bullets ...
- +20 more, equally binding -- memory_read name=<name> before working near one: fts-or-vs-allterms-presence-probe, console-csrf-origin-check-contract, ...
Stages (world-state gates):
- deep-audit-f15-f18-landed -- status unknown (no Status: header)
(details: memory_read name=<stage>)
Plans:
- marketing -- 2/3 done, 1 claimable, 0 in flight
- seamless-documentation-site -- awaiting approval: Seamless documentation site (presented, 2m)
(steps: tasks_ready plan=<slug>; claim: tasks_claim id=<task id>; attach work via the plan:<slug> tag)
Conventions (project-local choices):
- wordmark-caret-l-spans-three-files: The wordmark markup must stay in sync ...
... 3 more bullets ...
(9 total, 4 shown -- recall kind=convention for the rest)
Recent findings:
- cc/1fa4b02d (1h): Landed the installer check; the release gate now fails on ...
Ready tasks (2):
- Fix tool.call misattribution
- Polish the docs nav
(full queue with ids: tasks_ready; claim: tasks_claim id=<id>)
Memories (seamless):
- gofmt-must-scope-to-tracked-files: gofmt walks the filesystem ...
- shared-worktree-concurrent-agents-verify: Agents share the main worktree ...
- (+34 older -- recall query=<topic>, optionally kind=<kind>)
Recall on demand with recall; read a memory with memory_read.
Seam session: cc/8dd2fd5b-55d96b8d15ff0104 (ambient) -- this agent's Seamless tool calls bind to it automatically; no session_start needed
</seam-briefing>
Situation before library: the pinned head leads (tiered constraints, stages, plan rollups), what just happened follows (pending plans, conventions, recent findings, ready tasks), the memory index packs after it, and retrieval guidance plus session identity close the envelope.
```

Line by line:

- **The header** counts what exists, so an agent knows how much it is *not* being
  shown. Constraints, conventions, and pinned stages are memories too, so they
  are reported as subsets of the total, not second pools.
- **The Constraints section** comes first and is **never dropped for budget**. A
  constraint is a rule the project cannot violate; a briefing that omitted one to
  fit a token budget would be worse than no briefing at all. It is *tiered*:
  the top `briefing.constraint_max_full` (default 4) render as full
  `- name: description` bullets - starred constraints first, then constraints a
  recent mishap referenced (last 30 days, most recent first), then the same
  blended recency+utility order the memory index uses - and the remainder
  collapse into the compact **`+N more, equally binding`** line, which still
  names every one so an agent can `memory_read name=<name>` before working near
  it. Setting the knob to 0 disables tiering and renders every constraint full.
- **The Stages section** is pinned right after constraints, for the same reason:
  a gated stage's status is load-bearing for the whole session. The pin belongs
  to the *gate*: a stage whose body does not open with a live
  `Status: open|in_progress|blocked` header only renders (as
  `status unknown (no Status: header)` - the nudge names the fix) for the
  `briefing.stage_unknown_max_age_days` grace window (default 7) after its last
  update, then leaves the briefing rather than squatting in it forever.
- **The Plans section** follows. The pinned rollup rows (`2/3 done,
  1 claimable`), most recently active plan first, tell the next agent what work
  it can pick up right now, and the closing hint names the premade filters:
  `tasks_ready plan=<slug>` for a plan's steps, `tasks_claim` to take one.
- **`awaiting approval` rows** sit in the same section but open the budgeted
  body: captured but unapproved plans are a hint, not a commitment, so unlike
  the rollups they compete for budget and expire after
  `briefing.pending_plan_max_days`.
- **The Conventions section** follows: project-local choices and layout facts
  (`kind: convention`) - binding, but topically triggered, so unlike
  constraints they compete for budget. The top `briefing.convention_max_full`
  (default 4; 0 renders all) show in full and a count line always closes the
  section, pointing at `recall kind=convention` - a real command: `recall`
  with a kind and no query lists that kind newest-first. A starred convention
  heads the section and renders regardless of the tier cap or the budget.
- **Recent findings** - what previous sessions learned, harvested at their end -
  render right after: they say what just happened here, so they pack (and
  render) before the memory index rather than below it.
- **The Ready tasks section** closes the situation half: the open queue, newest
  first, before the library begins, with the full-queue and claim commands as
  its trailer. The display order is the only thing that changes at the top:
  `tasks_ready` itself still lists oldest-first, so claiming stays FIFO.
- **The Memories index** is `- name: description` bullets only - the description
  is the *only* text an index ever shows, which is why writing a good one
  matters more than writing a good body. Starred memories head the list,
  exempt from every trim and from the budget.
- **`(+34 older -- recall query=<topic>, optionally kind=<kind>)`** is the
  honest tail: the index was trimmed, and the briefing says so instead of
  pretending it is complete.
- **`Seam session:`** closes the envelope with the agent's own session handle,
  and says its tool calls are already bound to it ([how](#process-binding)). A
  hook that could not name the agent process prints
  [the line's other form](https://thereisnospoon.org/docs/reference/hooks/#the-ambient-session-line) instead.

## The budget, and what survives it

Sections are packed against `budgets.max_briefing_tokens` (default 1500), then
the whole thing is hard-capped at `briefing.hard_cap_multiplier` times that
(default 2x).

The **never-drop invariant**: constraints (both the full tier and the compact
`+N more, equally binding` line), pinned stages, and active-plan rollups are
counted first and are exempt from budget dropping - every constraint name
appears in every briefing. Starred memories keep the same exemption inside
their sections: a starred convention or index memory renders no matter how
starved the budget. Everything else packs in render order - pending
plans, conventions, recent findings, ready tasks, the memory index, sibling
findings, sibling memories - so budget priority and render priority agree: the sections
that say what is happening now pack before the memory library, a fat index can
no longer evict the findings that render above it, and the sibling sections
are the first to go when the budget runs out. The header counts only the
findings that actually rendered, and findings or index lines cut by budget
leave an explicit `+N more/older` trailer.

The memory index's own order starts as newest-first - but each memory also
carries a [utility score](https://thereisnospoon.org/docs/concepts/recall/#the-utility-nudge), a time-decayed
record of actual demand (reads, recall hits, prompt matches; being briefed
counts for nothing), and once a project's demand history matures the index is
ranked by the blend `(1-w)·recency + w·utility`, both on a 14-day half-life,
with `briefing.utility_weight` as `w` (default 0.4, 0 restores pure recency).
The same blended key ranks constraints ahead of their tier split, after stars
and recent mishaps have claimed the head of the full tier; while utility is
inactive the constraint order degrades to pure recency.
The switch is deliberate, not silent: in `briefing.utility_mode: auto` the
gardener latches it per project only once the project's first demand is at
least 14 days old and it has shown 20+ demand events and 10+ memories touched
in 30 days - a young project keeps the recency order, because a utility signal
with no history behind it is noise. `on`/`off` force it everywhere, and the
console's Settings → Knowledge engine shows each scope's progress toward the
latch, with a per-scope force.

Every knob is tunable in [Configuration](https://thereisnospoon.org/docs/reference/configuration/), and the
`briefing:` block is also editable live in the console. Those runtime edits are
stored in the database and win over both the file and the environment, applying
from the next session start without a daemon restart. If a briefing setting seems
to be ignored, check the console before you check the YAML.

Codex adds one client-specific safety ceiling after this packing step: every
model-visible Seamless hook context is capped at 2,400 estimated tokens, below
Codex's approximate 2,500-token temporary-file spill threshold. The cap runs
before telemetry is recorded and preserves generated closing tags and the
ambient handle. Claude Code does not use this additional cap.

## Liveness

Sessions heartbeat. A session that ends cleanly cascades immediately; one that is
abandoned is caught by an idle reaper after `gardener.session_idle_minutes` and
marked `expired`. The TTL only applies when there was no end signal at all - a
crashed agent's session does not sit "live" forever, and a slow one is not
reaped out from under itself. An agent that was only idle gets its session
back: the next Seamless tool call from the same live agent process reactivates
the expired ambient session (recorded as a `session.started` event marked
`revived`) and binds to it again. A session ended on purpose - by `session_end`
or a SessionEnd hook - stays ended, and that agent's later calls are unbound.
Two expired sessions stamped with the same process revive neither.
With [peer_auth](https://thereisnospoon.org/docs/guides/peer-auth/) on, a call revives only a session its own
account started. A restricted account's agent gets its session back exactly
as yours does, and no call - yours or an account's - reopens a session another
account started, whatever agent process it names. A restricted account's
session in a project it can no longer reach stays expired.

`session_end` is the explicit findings path. Claude Code's SessionEnd hook calls
the same lifecycle. Codex 0.144.6 has no SessionEnd event, so each `Stop` hook
updates provisional findings from `last_assistant_message` and the reaper later
marks the idle ambient session `expired`; those expired ambient findings still
surface in future briefings. Keep findings tight - briefings show a short preview
- but they are stored in full, not rejected, so a long finding is fine when it
earns it.

The end of a session also harvests **real model token usage** - input, cached,
cache-creation, and output counts read from the client's own transcript
(Claude Code) or rollout file (Codex), not estimated from injected bytes. The
session's console page shows them as its "Model tokens" panel; a session whose
client exposes no transcript simply records none.
