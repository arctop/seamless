---
title: Console
description: The read-mostly observability UI at /console - the complete list of what it can change, how sign-in works, the three experience levels, and what each page shows.
generate: console-levels
---

The console is the owner's window onto a system whose actual clients are agents.
It is `html/template` plus vanilla JS plus SSE, served by `internal/console` from
the same binary as everything else - no node, no npm, no React, no build step. It
is not how Seamless is driven; it is how you watch it.

## What the console can change

The console is **read-mostly**. That is a design claim, so here is the whole list
- every write it is capable of, taken from the `POST` routes in
`internal/console/console.go`. There are no others.

| Action | Route | Effect |
|---|---|---|
| Archive a memory | `POST /console/memories/{id}/archive` | Routes through `lifecycle.Archive`: the memory is stamped invalid and leaves every index, its file stays on disk with a tombstone. |
| Force-release a task claim | `POST /console/tasks/{id}/release` | The owner override: releases a claimed task's lock regardless of who holds it, reopening it for any agent. Not reachable from the agent MCP tools. |
| Approve a captured plan | `POST /console/plans/{slug}/approve` | The escape hatch for a Claude Code approval whose `PostToolUse` never fired: flips the `cc-plan` note to `plan-status:approved` and creates the tracking task, exactly as the hook would have. Only applies to a CC capture. |
| Star / unstar an entity | `POST /console/favorites/{kind}/{id}` | Toggles the same starred flag the `favorite_set` MCP tool sets: a starred memory pins into briefings and gets a post-fusion recall boost. For memories and notes the star lives in frontmatter; it never bumps `updated`. |
| Ask the gardener for proposals | `POST /console/gardener/request` | Interprets a natural-language maintenance request into **pending proposals**. It never mutates a memory. |
| Plan a project split | `POST /console/gardener/split` | Interprets a split request into a plan batch of **pending proposals** (one split setup plus one reproject per memory). Also never mutates a memory. |
| Apply one proposal | `POST /console/gardener/{id}/apply` | Carries out that proposal's effect. |
| Dismiss a proposal | `POST /console/gardener/{id}/dismiss` | Drops it without acting. The pattern is raised again if new evidence for it arrives after the decision. |
| Hide a proposal forever | `POST /console/gardener/{id}/hide` | Drops it without acting and blocks the pattern permanently - no recurrence re-raises it. Listed under **Hidden forever**. |
| Unhide a pattern | `POST /console/gardener/{id}/unhide` | Lifts a forever block. The proposal stays resolved; the gardener may propose the pattern again the next time it recurs. |
| Retarget a reproject proposal | `POST /console/gardener/{id}/retarget` | Rewrites a **pending** reproject's destination project before it is applied. Reproject proposals only. |
| Apply a whole plan batch | `POST /console/gardener/plan/{slug}/apply` | Applies every pending proposal in a plan, split setup first so the child projects exist before the memories move. Best-effort: it applies what it can, reports how many landed, and leaves the rest pending. |
| Dismiss a whole rail group | `POST /console/gardener/group/dismiss` | Dismisses every pending proposal in one rail section or split plan, one at a time, so each keeps its own event and stays individually undoable. Only Dismiss is offered in bulk. |
| Undo a decision | `POST /console/gardener/{id}/undo` | Returns a resolved proposal to the queue from **Recently decided**, inverting whatever its apply did, for the kinds whose apply can be inverted. |
| Set a project's isolation | `POST /console/projects/{slug}/isolation` | Changes how far a project's knowledge travels. A tightening that would cut existing context first renders a confirmation step; nothing is written until it is confirmed. |
| Save briefing settings | `POST /console/settings/briefing` | Writes the briefing knobs as a runtime **override row** in the DB. It never writes the config file. |
| Reset briefing settings | `POST /console/settings/briefing/reset` | Clears the override row, reverting to the file/env configuration. |
| Force utility ranking for a scope | `POST /console/settings/utility` | Sets or clears the owner's per-scope force: `on` and `off` win over the gardener's readiness latch, `auto` defers to it again. |
| Switch embeddings off or back on | `POST /console/settings/embeddings/mode` | Stores or clears the embedder off switch. It is read at serve start, so the change applies from the next restart - the page says so. |
| Re-embed everything | `POST /console/settings/embeddings/reembed` | Starts a background pass that re-embeds every memory and note with the active model. It changes vectors, never content. |
| Choose the console level | `POST /console/settings/level` | Stores the [experience level](#choose-how-much-you-see) as a row that wins over file and env. Presentation only: nothing an agent receives changes. |
| Reset the console level | `POST /console/settings/level/reset` | Clears that row, back to the file/env level. |
| Dismiss the welcome card | `POST /console/settings/level/welcome` | Records that the Home welcome card was seen, without choosing a level. |
| Save optional features | `POST /console/settings/features` | Writes the feature switches as a stored **override row** in the DB. It changes what is exposed - console screens and the matching agent tools - and deletes nothing. |
| Reset optional features | `POST /console/settings/features/reset` | Clears the override row, reverting to the file/env configuration - which, unless you set the keys there, means every optional feature is off again. Still deletes nothing. |
| Turn update checks on or off | `POST /console/settings/updates` | Stores the console's `update.check` override (`check=on` or `off`). It can always turn checks off; it cannot turn them on while the config file or environment says `update.check: false` - that is final, and the form says so. Installs nothing. |
| Reset update checks | `POST /console/settings/updates/reset` | Clears that override - both switches - handing them back to the config file (or, without one, the build's default). |
| Check for updates now | `POST /console/settings/updates/check` | Asks GitHub's release list once, right away - refused while checks are off or within a minute of the last check. Reads; installs nothing. |
| Turn automatic updates on or off | `POST /console/settings/updates/auto` | Stores the console's `update.auto` override (`auto=on` or `off`), keeping its `update.check`. It can always turn automatic updates off; it cannot turn them on while the config file or environment says `update.auto: false`, or locks `update.check: false` (nothing installs without checks). With checks merely off, the choice is kept and applies once they are back on. Only an install the installer made acts on it. |
| Update now | `POST /console/settings/updates/apply` | Installs the newest release at once, after a confirm: it skips the soak, the wait for idle agents, skipped releases and the backoff, and lifts a hold, whether automatic updates are on, off or paused. Refused while checks are off, on an install that does not update itself, while an update is under way, within a minute of a failed release check, when nothing newer is known, or when the release carries no signed checksums bundle. The daemon restarts to finish. |
| Resume automatic updates | `POST /console/settings/updates/resume` | Lifts the pause automatic updates put on themselves after rollbacks, and the hold a deliberate downgrade set. Changes no setting; skipped releases stay skipped, and a running backoff runs out. |
| Save a project family | `POST /console/settings/families/save` | Creates a family or replaces one family's name and member set - the same `project_families` setting `seamlessd family` manages. Members come from a closed picker of registered projects, so a typo cannot create an inert member. |
| Delete a project family | `POST /console/settings/families/delete` | Removes the whole family. Its projects lose the sibling-findings channel; nothing else about them changes. |
| Sign in / sign out | `POST /console/login`, `POST /console/logout` | Sets or clears the console cookie. Touches no data. |

Read the shape of that list. There is no "create memory", no "edit note", no
"delete", no "add task", no "start session". The direct writes to knowledge
state are **archive a memory**, **approve a captured plan**, and the **star**
flag; the rest either manage gardener proposals - which are themselves
proposals, reviewed before they do anything - or free a lock, or set a
configuration knob (briefing overrides, feature switches, project families, the
update switches) that shapes future briefings, or what is exposed, without
touching any memory's content. The console level is presentation state of the same kind: it changes
what this console shows you and nothing else. **Update now** and **Resume
automatic updates** act on the install rather than on knowledge: the first
starts the same backed-up update `seamlessd update` runs, the second lets
automatic updates run again.

This is deliberate, and it is the same principle as
[the gardener's](/concepts/gardener/) propose-only contract. The store is written
by agents doing work, with provenance attached. A console that could quietly edit
a memory would produce knowledge that came from nowhere, attributable to no
session, explaining nothing.

Every write action redirects back with a flash message (`?notice=` for success,
`?error=` for failure) rather than rendering a result page in place, so a reload
never repeats the action. With script, a success notice surfaces as a toast and
both parameters are stripped from the address bar, so a later live refresh never
replays a message about an action that already happened; an error also stays on
the page as a banner.

## Signing in

There is one credential in the whole system: the static bearer key
(`mcp.api_key`). It guards `/api/mcp`, the hook endpoints, and the console alike.

Two ways to present it:

- **A browser** trades the key for a cookie at `/console/login`. The cookie value
  is a SHA-256 hash of the key, not the key - so the raw credential never sits in
  the browser's cookie jar. It is `HttpOnly`, `SameSite=Lax`, and scoped to
  `/console`.
- **The `seam` CLI** sends the key as a bearer token on the `Authorization`
  header, and asks for JSON.

Unauthenticated browsers are redirected to the login page with a `?next=` that is
validated against an open-redirect (an off-site or absolute candidate becomes
`/console/`). Unauthenticated JSON callers get a 401.

Public routes are the login page and the static assets (`console.css`,
`interactions.js`, `search.js`, `favicon.svg`). Everything else requires the key.

### `make console`

```bash
make console          # open in the default browser, already signed in
make console-chrome   # same, but force Google Chrome
```

This builds, then runs `seamlessd console-open`, which renders a one-shot
self-submitting login page to a `0600` temp file and opens it. The page POSTs the
key to `/console/login`, which sets the cookie and 303s into the console - so you
land on an authenticated page with nothing to paste. It refuses to run if
`mcp.api_key` is empty or the server is not answering `/healthz`.

`make console-chrome` exists for agents: they drive Chrome, so this hands the auth
cookie to the browser they can actually see. (`--browser` is macOS-only.)

## Getting around

The sidebar groups the screens by the job they serve rather than by table:

| Group | Screens |
|---|---|
| **Live** | Overview, Now, Interactions |
| **Knowledge** | Memories, Notes, Retrieval, Gardener |
| **Work** | Projects, Plans, Tasks, Sessions |
| **Research** | Labs, Trials (only while that [optional feature](#optional-features) is on) |

That is the full list, at the Advanced [level](#choose-how-much-you-see); a
lower level shows a subset, in the same order and under the same names.
Settings closes the list. Above the groups, a **Search or jump to** field opens
the command palette. The sidebar collapses to an icon rail (the header button, or
`[`) and remembers that per browser; under 720px it becomes a drawer behind the
menu button, with every section's label and count.

Every screen title carries an (i) button that says, in one line, what the screen
is for.

### The command palette

`Cmd/Ctrl+K`, the sidebar's search field, or `/` on a screen with no filter of
its own opens the palette. With nothing typed it offers **Recent** (the entities
you last opened in this browser), **Jump to** (every section the sidebar offers,
with its shortcut, plus Search, Context, and each Settings section the level
shows), and **Actions** (switch theme, collapse the sidebar, the shortcut sheet,
change the experience level). Typing matches sections and actions instantly;
from two characters on it also searches memories, notes, tasks, plans, trials,
projects, and sessions through the same route as [Search](#search), showing the
groups the level offers.

### Keyboard

| Keys | Does |
|---|---|
| `Cmd/Ctrl+K` | Search or jump to a section |
| `/` | Focus this screen's filter; open the palette where there is none |
| `g` then a key | Go to a section: `o` Overview, `n` Now, `i` Interactions, `m` Memories, `e` Notes, `r` Retrieval, `g` Gardener, `w` Projects, `p` Plans, `t` Tasks, `s` Sessions, `l` Labs, `x` Trials, `,` Settings |
| `j` / `k` | Next / previous item in a library rail |
| `[` | Collapse or expand the sidebar |
| `?` | The shortcut sheet |
| `Esc` | Close the palette, a menu, the sheet, or the drawer |

The `g` map is read from the sidebar itself, so a section switched off in
Settings - or one the [level](#choose-how-much-you-see) leaves out of the
sidebar - takes its shortcut with it, and brings it back when it returns.

Moving between screens keeps the sidebar in place and settles the new content
in (cross-document view transitions, where the browser supports them), and
hovering a console link prefetches its page. Neither runs any code of the
destination page early; both degrade to a plain navigation.

### Ambient signals

The console shows activity without asking to be read. The line along the
sidebar's edge - the Seam - carries each event from the live stream as a spark
that travels to the section it belongs to and lights that section's icon:
cyan for context reaching an agent, green for knowledge written, coral for
something wrong, indigo for everything else. The sky behind the pages and the
daemon's dot in the sidebar brighten and breathe faster with the recent event
rate, and settle when the fleet is idle. Headline numbers count up when a page
opens; agents working right now carry a slow orbit of light on the Now screen.

All of it is decoration over data the page already states: nothing here changes
a number, and `prefers-reduced-motion` stills every part of it (the Seam sends no
sparks, numbers render at their value).

## Choose how much you see

The console has three **experience levels**. They change what the console shows
you and nothing else: your agents get the same briefing, the same MCP tools, the
same hooks, the same recall, and the same gardener at every level. The level is
presentation, stored for this installation's owner rather than per browser, so
your phone and your laptop agree.

**Basic** is the essentials, for someone who installed Seamless because they use
Claude Code or Codex and wants to know it works: what the agents remember
(Memories, Notes), what they did (Sessions), cleanup suggestions to accept or
decline (Gardener), and a Home that opens on a health strip - which agent clients
are working here, whether semantic recall is on, when the last briefing went
out, and the version (which says when a newer release is out) - plus the
Updates settings.

**Standard** adds following the work: Now, Projects, Plans, and Tasks (and Labs
and Trials when that feature is on), the Overview's window, vitals, and
workspaces, the Briefing and Workspaces settings, and the finer controls on the
screens Basic already had.

**Advanced** is every screen and every knob: Interactions (the tool-call
transport), Retrieval (the analytics), Context, the Knowledge engine settings,
and raw event payloads.

A fresh installation starts at **Basic**, and Home carries a one-time welcome
card offering all three - pick one, or dismiss it; either way it does not come
back, on any device. The sidebar's account row names the level you are on and
links to **Settings > Experience**, where the three levels sit side by side with
the list of what each one shows. The palette's **Change experience level** action
goes there too.

**Hidden, not locked.** A level removes screens from the sidebar, the palette,
and the shortcut map; it never locks them. A link to a screen above your level -
from these docs, an agent's finding, a briefing, a bookmark - still opens it,
under a note saying it is not in your sidebar, with a button to switch. JSON
callers (`?format=json`, and so the `seam` CLI, `seam doctor` included) get the
same answer at every level.

**Upgrading keeps every screen.** An installation that already recorded sessions
before levels existed is set to Advanced by a one-time upgrade step, so nothing
you were using disappears; Settings > Experience says the level was set by the
upgrade, and the welcome card appears once to announce the choice.

Levels and [optional features](#optional-features) compose: a screen shows when
its feature is on **and** the level includes it. Features change what exists -
for the console and for agents alike; the level only changes how much of it you
see. The level can also come from the config file or environment (`console.level`,
`SEAMLESS_CONSOLE_LEVEL`) - a choice made in the console wins until you reset it
there. See [Configuration](/reference/configuration/).

What each level shows, generated from the console's own registries:

<!-- generate here -->

## Three ways to render a page

Every route answers in the shape the caller asked for:

- **HTML** by default - the full page, layout and all.
- **JSON** when the caller sets `?format=json` or an `Accept` header that wants
  JSON and not HTML. This is how `seam` reads the console's data.
- **An HTML fragment** for entity details when the caller passes `?peek=1` - the
  detail pane loads it without a page navigation - or `?reader=1`, the richer
  reader fragment the library screens (memories, notes, tasks, plans, labs,
  trials) swap in place.

The event page composes its full page from the same `detail-body` block its
peek fragment renders, so the two cannot drift. Session and project
deliberately do not: their fragments are compact summaries of much richer
bespoke pages. Memory, note, task, plan, lab, and trial detail URLs render
their library screen with that entity open in the reader.

Strictly-validated query params (`?sort`, `?scope`, `?tab`, `?w`) return a 400
naming the bad param and listing the valid values, rather than silently falling
back to a default - so an agent driving the console by URL sees the fix.

`GET /console/events` is the SSE stream: every recorded event as one JSON `data:`
frame, with a ping every 25 seconds. A `retrieval.injected` frame also lists the
memories it surfaced as `itemIds` (capped at 48), which is what lets a page point
at exactly what reached an agent. `?feed=interactions` opts into the richer
transport-level rows the Interactions screen consumes.

## Overview

`/console/`

The landing page and the health check. It carries:

- **Counts** - active memories (broken down by kind), notes, sessions, and tasks
  by status.
- **Retrieval health** over a selectable window (`?w=24h|7d|30d|all`): injection
  volume, a trend chart, the **reach rate** (distinct active memories that
  actually surfaced, over all active memories), sessions reached, and the
  most-injected memories.
- **Coverage** - the share of in-window sessions that retained anything, with a
  per-channel breakdown (findings, memories, notes, trials) and a windowed trend.
  The channels overlap, so the shares need not sum to 100%.
- **Projects at a glance** - the top projects by recent activity, drawn from the
  same batched query the Projects board uses, so a row here reconciles with a row
  there exactly.
- **Agent-reported mishaps** - recent incidents agents explicitly supplied to
  `session_end`, attributed through the reporting session's harness and model.
  Warning tones appear only when reports exist; an empty rail is a positive
  "No mishaps reported" state.
- **Knowledge sky** - a star chart of every active memory, with no cap. Each
  project scope (global included) owns a wedge sized by its share of memories,
  largest first. Distance from the centre is when the memory last surfaced to an
  agent: the core holds the last 24 hours, rings mark 7 and 45 days (the same
  fresh and stale lines the memory reader and the "going stale" card use), and
  the rim belt holds memories that have never surfaced. Colour is kind; size is
  demand (the query-gated utility score, so a big star is one agents pull, not
  one the briefing merely shows). Hollow stars have never surfaced, a ring marks
  a starred memory, spikes mark one written today, and today's stars twinkle.
  Beside the chart, a readout states the same picture as text: each ring's
  count, the going-stale count (always equal to the attention card's), and every
  scope with its ring mix as a bar. Point at a star for its name and description
  (the pointer snaps to the nearest star); click it for a card with its facts and
  a link to open it; double-click to open it directly. Type in **Find a memory**
  to light only the matches (name, description, tag, scope, or kind) and list
  them, with the arrow keys and Enter to step through; click a ring, the going-
  stale line, a scope, or a kind chip to filter the same way, and point at one
  to preview it. When an injection or a read lands on the live stream, each
  memory it names sends a beam into the core, and the refresh that follows
  glides the star to its new ring; a newly written memory appears with a flare.
  A star keeps its place while its scope and ring are unchanged, so a refresh
  moves only what changed, and search, filters, and the selection survive it.
  It is HTML only - the JSON answer carries no sky.
- **Recent activity** - the last twelve events, each linking to its detail page.

The four judged vitals at the top (memory reach, knowledge continuity, context
injections, sessions reached) are drill-down links carrying the selected
window: reach, injections, and sessions reached land on the Retrieval screen
whose hero and delivery funnel are the same numbers over the same report, and
continuity lands on the Sessions list filtered to `?retained=no` - the
sessions that kept nothing. An empty-state card stays linked, because the
destination explains why there is nothing.

Live sessions are counted TTL-aware (active *and* heartbeated within the idle
threshold), so the headline matches the Sessions screen rather than the raw
`active` count that an idle session inflates until the reaper runs.

### Since you were last here

When you come back to the Overview after at least five minutes away, a line
above everything else answers the check-in question -- what changed while I was
gone? -- with linked counts: memories written, sessions started, tasks closed,
notes written, gardener proposals raised, and mishaps reported since you last
looked ("Quiet since you were last here 3h ago" when nothing was recorded).

"Last looked" is a per-browser stamp, set whenever a console tab is hidden or
left; the counts come from `GET /console/since?t=<unix ms>`, which answers JSON
only and refuses a missing, malformed, future, or older-than-90-days `t` with a
400 rather than substituting a window.

## Now

`/console/now`

The exploded live view: what every agent is doing right now, across every
project and plan at once. Where the project workspace is task-centric, Now is
agent-centric - the unit is the live session, and everything it holds rides its
card. The sidebar entry's badge is the live agent count, and the page refreshes
on **every** event kind (tool calls included - here they are the signal, not
noise), morphing in place like every other screen.

Top to bottom:

- **The titlebar** - live agent count and the fleet-wide pulse: an events-per-
  five-minutes sparkline over the last hour, all kinds, deliberately unfiltered
  by scope.
- **A scope strip** - one chip per project with a live agent (`?scope=<slug>`;
  the empty project scope filters as `global`). Filtering narrows every zone
  except the pulse.
- **On duty** - one card per live session: harness+model pill, project, last
  heartbeat (cards are toned hot/warm/quiet by heartbeat age), session wall
  clock, cumulative tokens, a star toggle, every claim it holds with a live
  lease countdown, and a short trail of what it just produced. A live agent
  holding nothing says so: "no claim held - roaming".
- **Loose ends** - `in_progress` tasks no live agent is carrying: a lapsed
  lease, a claimless start (`tasks_update status=in_progress` without a
  claim), or a holder that went quiet mid-lease. A lapsed claim offers
  **release claim** with no confirmation - the lease is already dead, so there
  is no live holder to interrupt, only a stale lock to clear.
- **Plans in motion** - a horizontally scrolling rail of every incomplete plan
  across every project, done/in-flight progress bars and ready counts, cards
  dimmed once a plan has rested for 24h. Each links to its project's Plans &
  tasks tab.
- **Up next** - the cross-project ready queue: claimable this instant, plan
  steps included, each naming what closing it would unblock.
- **The wire** - the freshest business events, scope-filtered.

Everything links onward - sessions, tasks, plans, projects, events - and the
lease countdowns tick client-side between refreshes. With the
[gamification](#optional-features) feature on, the page also carries the day
tape, the personal-records rail, the hot-streak pulse, and celebration moments;
off (the default), none of that renders.

## Interactions

`/console/interactions`

The clean live feed of what agents are actually doing: MCP tool calls, hook
injections, recall-miss prompts, session lifecycle, and the plan-mode capture
stream. A fresh page starts empty and listens from that moment forward; merely
visiting never restores old rows.

History is explicit and additive. Choose a recent window and select **Add** when
earlier context is useful; live rows stay in place, and **Load older events**
paginates only inside that chosen window. Filters operate over the rows already
in memory, by event category and session lane. Pausing buffers new arrivals
rather than discarding them.

Each compact row expands just enough to expose its request/result or injected
text. Selecting the inspector keeps that context beside the stream and links to
the full event page. A recall via the MCP tool records both a
`retrieval.injected` and a `tool.call`; the injected twin is dropped here because
the tool call carries the same content plus its arguments. Session lifecycle
twins are kept on purpose, as feed markers.

## Search

`/console/search`

One query across every entity the console can link to. Memories and notes come
through the same fused FTS + semantic retrieval that [recall](/concepts/recall/)
uses, with snippets; tasks, plans, trials, projects, and sessions have no FTS
mirror and match by `LIKE`.

Stable references get their own lookup lane. An exact memory name or note slug
ranks ahead of token-overlap matches and is shown beside the result's display
title. A full memory, note, task, session, or trial ULID works
case-insensitively; an 8-character-or-longer ULID prefix finds every match of
those kinds without
letting a short prefix such as `01` flood the page. Identifier matches keep the
canonical ID-based detail link and are labeled separately from keyword and
semantic matches.

The command palette (⌘K, available on every page) fetches this same route with
`?format=json&fast=1`, which drops the semantic leg - a query per keystroke must
never cost a remote embedding round-trip.

The semantic leg is nearest-neighbor: there is always a "nearest" memory,
however far, so a semantic-only hit must clear `search.semantic_floor` (cosine
similarity, default 0.3) to appear - without the floor any query, including
nonsense, would fill the page to its limit. A hit the keyword leg also matched
is exempt. Every hit the semantic leg found shows its similarity as a
percentage, so you can see where relevance falls off; keyword-only hits show a
highlighted snippet instead. Agent-facing recall applies no floor - an agent
can judge a weak hit for itself.

Coverage is deliberately partial in one place: events are excluded because the
telemetry stream has its own Interactions surface and would flood results.

## Projects

`/console/projects`

The board: one row per project, with live sessions, total sessions, open and
blocked tasks, memory count, inherited memories, reach rate, and last activity.
Grouped by family (`?group=family|flat`) and sortable (`?sort=recent|coverage|name`).
The global (`""`) scope appears as a row but is not a project and has no detail
link.

Selecting a project opens the **project workspace** - a seven-tab page over that
project alone:

| Tab | Shows |
|---|---|
| Overview | The project's metrics, memory kinds, injection trend, recent events. |
| Plans & tasks | Per-plan step timelines with each step's status, claiming session, lease countdown, and blocking dependency; plus the ready queue. |
| Sessions | The project's sessions, with the tasks each currently holds. |
| Memories | The project's memories with a lineage cell - provenance session, or a supersession pointer - plus the memories it *inherits* from a parent that a strict per-slug count excludes. |
| Notes | The project's notes. |
| Interactions | The project-scoped slice of the feed. |
| Context | The effective SessionStart flow into and out of the project: global and parent memory pools, sibling-family channels, and split lineage. |

A retired project still renders, with its banner - kept for provenance. Only an
unknown slug is a 404.

## Sessions

`/console/sessions`, `/console/sessions/{id}`

The list separates **active** (live: active and heartbeated within the idle TTL)
from **idle** (active but gone quiet past it, awaiting the reaper) from
**completed** and **expired**. Filterable by status, searchable, windowed, and
filterable by retention (`?retained=yes|no`): whether the session left a
durable artifact behind - non-empty findings, or a written memory, note, or
recorded trial, the same covered-ness test the coverage numbers apply. The
Overview's continuity vital links straight to `?retained=no`, so its click
answers "which sessions dropped knowledge".

The list defaults to the last 24 hours. When that leaves most sessions out, the
list ends by saying so ("3 of 212 sessions were active in the last 24h") and
offers the wider windows in place, so a quiet day never reads as an empty
system. A row names its host only when the session ran on a different machine
from the console's, and its source only when it was not a normal startup.

A session's page is the workspace: its findings (rendered), its full event
timeline as interaction rows, per-session counts (tool calls, memory reads and
writes, items injected, and read-after-inject), the tasks it currently claims with
their lease countdowns, and the memories it produced.

## Memories

`/console/memories`, `/console/memories/{id}`

A two-pane library: a rail of memories grouped by project (global first, kinds
in canonical order, each dot colored by kind) beside a full-height reader.
Sortable by name, recency, reach, utility, or starred; filterable by a substring
of name, description, kind, or tag. Inactive memories collapse into an
archived-and-superseded group at the rail's end, each carrying its status and,
when superseded, what replaced it.

The reader renders the body uncapped (through the markdown layer, with raw HTML
disabled and a sanitizer on the output), the metadata - kind, project, tags,
timestamps, the session that produced it - its reach counts, its
[utility score](/concepts/recall/#the-utility-nudge) with the per-signal
demand breakdown behind it, the `vscode://` link straight to the file, and its
supersession neighbors in **both** directions: what replaced it, and what it
replaced. The actions here are
**star** - the flag that pins it into briefings and boosts recall - and
**archive**.

Opening `/console/memories` auto-opens the most recently updated match; a
memory's own URL opens the same screen with it selected. Clicking rail items
swaps the reader in place (real URLs, browser Back works), and `j` / `k` step
through the rail.

## Notes

`/console/notes`, `/console/notes/{id}`

The same library shape for notes: a project-grouped rail (global `""` first),
sortable by recency, title, or starred, filterable by title, description, or
tag. The
reader renders the note as a document - uncapped body in a measured reading
column, description, tags, word count, source URL, and the file path with an
editor link.

## Tasks

`/console/tasks`, `/console/tasks/{id}`

The same library shape, with the rail grouped into four buckets: **ready** (no
unfinished blocker), **in progress**, **blocked**, and **closed** (done and
dropped merged, newest first, capped at 25 with a count of the rest, collapsed
by default). The reader carries the task's body, claim and lease state, and
both dependency directions; **force-release** is the action, and it is the
owner override - it takes the lock from whoever holds it.

## Plans

`/console/plans`, `/console/plans/{slug}`

The same library shape, with the rail grouped by phase (**in progress**,
**ready**, **done**) and scoped by the window selector in the rail's tools
(24h by default). Both kinds of plan share the rail:

- **captures** - Claude Code plan-mode captures (`cc-plan` notes), with their
  lifecycle status, iteration count, and cached subagent runs.
- **composed** - plain [plans-as-composition](/concepts/tasks-and-plans/) plans (a
  note tagged `plan:<slug>` plus its tasks), which have none of the capture-only
  fields.

When the window hides plans, the rail ends with how many last moved before it
and the wider windows to switch to.

A capture owns its slug; composed plans fill only the rest. The reader shows
the rendered plan body, the step tasks, and the notes attached to the
composition (supporting notes and agent caches). **Approve** appears here, for
captures only.

Each plan also carries a **model tokens** rollup - the cumulative transcript
tokens of every session attributed to the plan (any session that moved a step,
or captured the plan) - compact in the rail (`~483k tok`) and qualified in the
reader (`~483k model tokens · 3 sessions (1 unreported)`). Attribution is
whole-session on purpose: tokens are only ever known per session, so a
session's full burn counts toward each plan it touched, counted once however
many steps it moved. Claude Code reports tokens at session end, so a live
session stays *unreported* until it finishes; a session that touched more than
one plan is disclosed as *shared* rather than split by guesswork - which is
also why plan totals must never be summed across plans.

## Labs

`/console/labs`, `/console/labs/{name}`

The research-lab surface (the console twin of `lab_open` / `trial_record` /
`trial_query`). Labs and Trials are one [optional feature](#optional-features)
and ship off; while it is off both screens answer with a short "switched off"
page and neither appears in the nav.

A lab is not a stored entity - it is the label its trials carry, a stable name
for one line of investigation - so this screen is an aggregation over the trials
table and there is nothing to write.

The same library shape: a rail of labs, most recently active first, each with
its trial count and pass/fail tallies. The reader shows one lab's whole
identity - outcome tallies (pass, fail, partial, inconclusive, and *other* for
free-form or empty outcomes), the projects and sessions its trials touched,
first and last activity - and its trial history, newest first, each entry
linking into the Trials screen. Long histories cap at 100 with a pointer to the
uncapped, filterable view.

## Trials

`/console/trials`, `/console/trials/{id}`

The flat, filterable view over every recorded trial - the console twin of the
`trial_query` MCP tool, and part of the same
[optional feature](#optional-features) as [Labs](#labs). The rail groups trials
by lab (a group sits where its newest trial does) and filters by `?lab=` and
`?outcome=`. Outcomes are
free-form by design, so `?outcome=` is an exact-match filter rather than a
validated enum; the seg offers the conventional values (`pass`, `fail`,
`partial`, `inconclusive`).

The reader shows one trial's full record: what changed, **expected vs actual**
side by side (the actual pane tinted by outcome), the structured metrics
`trial_record` captured, and its provenance - lab, project, and the recording
session, each linked. Trial hits also surface in [search](#search) and the
command palette, and a session's page lists the trials it recorded.

## Context

`/console/context`

The briefing topology that the plans board does not show: which knowledge pools
are eligible at SessionStart, which configured edges are currently enabled by
the effective briefing settings, and where project splits moved durable memory.
It covers global memory, one-way parent-memory inheritance, bidirectional sibling
families (findings and the opt-in memory channel), unregistered-scope warnings,
and retired-project split lineage reconstructed from the memory-move event log.

`?scope=all` renders every known project scope, with global memory shown as the
shared source pool; `?scope=project&project=<slug>` focuses the same topology on
one project. The legacy `/console/relations` route permanently redirects here
and preserves its query string.

Reachable from the Projects board.

## Retrieval

`/console/retrieval`

The circulation report: is stored knowledge actually reaching agents, and at
what cost? The hero pairs the **reach ring** (distinct active memories that
surfaced, over all active memories) with the window's volume and cost -
injections, sessions reached, and **estimated tokens injected**. Everything
follows the selectable observation window except where a panel says otherwise.

Five zones below it:

1. **Delivery path** - the funnel as a flow: injections → distinct memories →
   sessions reached, ending in the knowledge-base coverage meter (how many
   active memories are still waiting to surface).
2. **Circulation pattern** - the injection trend chart and the traffic-by-kind
   mix.
3. **Scope coverage** - reach per project scope (global first), each row with
   its own reach rate and injection count.
4. **Knowledge pressure** - the most-injected memories against **quiet
   knowledge**: active memories not updated, injected, or read in 90 days,
   mirroring the gardener's default staleness horizon. Unlike everything else
   on the page, the stale list is all-time, not windowed.
5. **Loop health** - push versus pull: is what briefings push also what agents
   pull? **Demand rate** is the share of briefed memories that were also pulled
   by a query; **waste share** is the share of injected tokens spent on memories
   with no query-gated demand, judged against a fixed trailing 30 days whatever
   the window. Two miss stats sit side by side and measure different paths:
   the **recall-miss rate** is ambient - prompts that matched no memory on the
   [`<seam-recall>` path](/concepts/recall/#the-recall-triad) - while **agent
   search misses** are deliberate `recall` calls that found nothing; recurring
   ones feed the gardener's
   [memory-wanted pass](/concepts/gardener/#what-it-looks-for). **Funnel by
   surface** splits the read-after-inject funnel by injection surface -
   session-start briefings versus subagent-start child injections - each with
   its injections, distinct memories, and the share pulled by a query-gated
   read within the following 24 hours. The zone closes
   with the **dead weight** panel: memories briefings kept injecting without a
   single recall hit, prompt match, or read in 30 days (constraints and stages
   exempt as pinned-by-design) - the evidence behind the gardener's dead-weight
   archive proposals.

Reachable from the Overview's retrieval-health card.

## Gardener

`/console/gardener`

The review queue. Each pending proposal renders as a card showing exactly what it
would do - the memory to archive and why (whether staleness, a dead stage, or
dead weight flagged it), the pair to merge with their similarity score, the
digest or consolidated memory with its body rendered, the reproject's source and
destination, the rekind's from and to kinds, the split's children and shared
parent, and the **knowledge gap**
card with the queries agents kept searching for in vain - applying that one
opens a task; nothing is written until someone writes the memory.

Split batches are grouped by plan and reviewed together, setup card first, with an
apply-the-whole-plan action.

The actions are **apply**, **dismiss**, **hide forever**, **retarget**
(reproject cards only), and **apply plan**. Dismissing answers the evidence in
front of you - the pattern comes back if it recurs; hiding answers the pattern
itself. Everything you decide lands in **Recently decided** with an Undo, and a
hide is additionally listed under **Hidden forever**, where **Unhide** lifts the
block without returning the proposal to the queue. Above them sits a single
ask-in-words box, and it only ever
produces more proposals for this same queue. A request recognized as a project
split is planned as a split directly - the plan batch appears below like any
other. When the split's source project cannot be matched, an inline follow-up
asks you to pick the project and plans the split from there; nothing is retyped.

See [The gardener](/concepts/gardener/) for what each proposal type means.

## Settings

`/console/settings?s=<section>`

Settings is one section at a time, each shaped by what you came to do rather
than by subsystem. A sub-nav on the left (a row of chips on a phone) switches
sections in place, without reloading the page. `/console/settings` alone opens
**Experience**; an unknown `?s=` is a 400 that names the valid sections. Which
sections a level offers is in the [matrix above](#choose-how-much-you-see); a
section above your level still opens from a link, under the same note a hidden
screen gets. Old `/console/settings#...` bookmarks land on the matching section.

The editable sections share two habits. A one-line note at the top says where
the values come from - **Following file + env**, or an override in force, with
**Reset to file + env** beside it. And a save bar appears at the bottom once a
form has unsaved changes, with **Save** and **Discard** (which puts the form back
exactly as the page loaded it); without JavaScript the bar is simply always
there. `GET /console/settings?format=json` returns the whole payload whatever
`?s=` says, plus `consoleLevel`, `consoleLevelOverridden`, and
`consoleLevelSource`.

### Experience

`/console/settings?s=experience`

The three [levels](#choose-how-much-you-see) side by side - Basic, Standard,
Advanced - the current one marked, each with a one-line pitch and a list of what
it shows, generated from the same registries the sidebar and the page gates
read, so the list cannot promise something the level does not do. Choosing one
applies at once: the sidebar and the page update in place, and if the new level
hides the screen you were on you land on Home with a note. The note at the top
says whether the level was **chosen in the console** or **set by the upgrade**,
and **Reset to file + env** hands the choice back to `console.level`. The same
section holds the theme and a button for the shortcut sheet. The theme is
**System**, **Light**, or **Dark**, kept per browser. System is what a browser
starts on: it follows your computer's light or dark appearance, and follows it
live when it changes. Light and Dark pin one theme. The sidebar's sun and moon
switch the same setting: under System they pin the theme opposite the one on
screen, and picking System here goes back to following the computer.

### Optional features

`/console/settings?s=features`

Optional features are the parts of Seamless you can switch on and off, and they
ship **off**: a fresh install exposes none of them until you turn one on. The
section renders one card per feature - a toggle, an Enabled/Disabled pill, what the
feature is, a generated line naming exactly what switching it off hides, and a
live count of the data it holds either way ("Data kept: 12 trials across 3
labs").

There are three today:

**Research labs & trials** owns the [Labs](#labs) and [Trials](#trials)
screens, the trials search scope, and the `lab_open`, `trial_record`, and
`trial_query` MCP tools. Screens and tools move together on purpose, so an
agent never sees a tool for a screen you switched off.

**Momentum** is gentle progress cues woven into existing screens -- plan finish
lines, capture streaks, knowledge payoffs, and project growth -- judged from
real activity, never invented. It owns no screens or tools of its own; turning
it on adds seven surfaces where you already look:

- **Plan finish-line cards** on the Overview attention strip: any plan at least
  80% done gets one positive card naming the exact remaining steps ("seambench
  -- one step from shipped"), linking to the plan, with a thin progress bar
  drawn to the plan's exact done/total percent. The agent briefing's plan
  line carries the same emphasis, so agents are nudged to close it too.
- **The capture calendar** on Sessions: a year of daily activity, cell
  intensity from sessions per day, a distinct dot on days that captured
  knowledge (a session left findings, a memory, a note, or a trial), and two
  quiet numbers -- the current capture streak and the longest ever. The streak
  counts covered days, so it rewards capture, not raw usage; a streak of seven
  covered days or more earns a small flame beside the number, and the number
  itself stays verbatim. The grid is an instrument, not wallpaper: hovering a
  cell reads out its day, clicking one focuses the session map on exactly that
  day (a clearable chip names the focus; picking a time window widens back
  out), and the grid is keyboard-walkable -- arrows move a day or a week,
  Enter focuses, all without a page reload.
- **Knowledge payoff moments**: the first time a memory is read by a session
  other than the one that wrote it, the moment lands in the activity ledger
  ("gotcha chroma-boot-race just paid off for the first time") -- once per
  memory, ever -- and the Overview rail gains a **Memory of the month** panel
  naming the last 30 days' top-utility memory with the counts behind the claim.
- **Maturity stages** on the project board and detail header: each project
  earns a latched stage -- seedling, sprouting, established, deep-rooted --
  from real thresholds over age, memories, event volume, and reach. Stages
  never regress, and the pill's tooltip states exactly what the next stage
  asks for.
- **The plan-shipped settle** on Plans: the task transition that closes a
  plan's last step -- wherever it lands, an agent shipping over MCP included --
  mints a once-ever plan.shipped moment. The plan's row settles with a single
  wash when it happens while you are watching, the ledger renders it under its
  own flag icon, and the Plans header counts the local month: "Plans shipped
  this month: N".
- **Milestone moments** in the activity ledger: a short latched set of honest
  firsts and counts -- a project's 100th/500th/1000th memory, its 1000th
  answered recall, its first supersession, its first shipped plan, and its
  birthdays -- each minted once ever and rendered under an award glyph with
  the exact claim ("100 memories written in seamless"). Milestones accumulate
  in the ledger only; there is no trophy screen.
- **Witnessed unlocks** on Settings: the utility-activation table's armed
  note upgrades to the date each stage unlocked, so a latch that used to flip
  silently is witnessed.

Motion keeps one register: every animation plays once -- on a live arrival or
on first render -- and nothing loops except the existing pulse idioms. A moment
already on screen when the page loads renders plain; only a new arrival over
the live feed animates. `prefers-reduced-motion` stills all of it, and the
shipped-plan settle is the ceiling: no confetti and no toasts, which belong to
gamification's arcade on Now.

Momentum keeps the console's judged-numbers ethos: every number is real and
verifiable, empty states say so honestly, and there are no punishment mechanics
-- an inactive day is an empty cell, never a warning, and nothing nags or
expires. Off (the default), none of it renders and none of it is computed: no
latch moves and no moment is minted. Moments minted while it was on stay in
the event history (nothing is ever deleted), though milestone rows leave the
activity feeds until it returns.

**Gamification** is the arcade layer of the [Now](#now) screen. Where momentum
asks "is this knowledge practice building on itself?", gamification plays back
"how hard is the fleet running right now?" - and an owner who enjoys one
framing may find the other noisy, so they toggle separately. It owns no screens
or tools of its own; turning it on adds four surfaces to Now:

- **The day tape**: today's judged output - tasks closed, memories and notes
  written, plans touched, sessions started - each cell against its trailing
  7-day daily average.
- **The personal-records rail**: latched bests (most tasks closed in a day,
  most memories written in a day, most agents live at once). Records only ever
  move forward, like maturity stages; an unset record reads "today could be
  the day".
- **The hot-streak pulse**: the last ten minutes' event count beside the
  titlebar pulse, catching fire past a fixed floor. The count is shown
  verbatim either way, so the claim stays verifiable.
- **Celebration moments**: a record falling or a plan shipping its last step
  today renders a moment chip - and when one lands while you are watching, a
  toast and a brief confetti burst (skipped under reduced motion). A record
  crossing is minted into the event ledger at most once per record per day.

The same guardrails as momentum apply: every number is judged from recorded
activity, never invented, and there are no punishment mechanics - a quiet day
is an empty tape, never a warning. Off (the default), none of it is computed,
no record latch is written, and the Now page carries zero trace of it.

**Nothing is ever deleted.** Switching a feature off gates exposure and nothing
else: the trials stay in the database, its screens answer with a short "switched
off" page that links back here (a JSON caller gets a 403), and switching it back
on restores every surface exactly as it was. That is what the data-kept line on
each card is for - it reports the feature's own rows whether it is on or off.

**When the change lands** has four different answers:

| Surface | When |
|---|---|
| The console | Immediately - nav entries, screens, the search scope, and the overview tiles appear or disappear on the next render. |
| A tool call | Immediately - a disabled feature's tool is refused as an unknown tool, whatever list the caller is holding. |
| The agent briefing | The next session start - momentum's finish-line emphasis reads the same stored override the console gates on. |
| A client's tool list | The next time it lists tools, in practice its next session. Seamless declares `listChanged: false` and sends no tool-list notification, so a connected client keeps the list it already has. |
| The client-side skill | The next `seamlessd install-hooks` run. The daemon does not reach into `~/.claude/skills` on a toggle, so `seamlessd doctor` raises an **info** line while a skill for a disabled feature is still sitting in a client's skill home. |

Saving writes a **stored override** row in the database - the same layer the
briefing form uses. It wins over file and env, never touches your config file,
and holds until **Reset to file + env** clears it; reset means back to the
file/env configuration, which unless you set `features:` there is off.

One override can be in force without you having set it. Upgrading an
installation that already holds trial data seeds the override with research on,
so a feature that now ships off does not disappear from under data you were
already using. That is why the section states that a stored override is in force
rather than crediting you with the choice - and reset clears it like any other.
At Basic the section keeps the cards, what each hides, and the data-kept line,
and leaves out the agent tool names and the precedence line.

### Your setup

`/console/settings?s=setup`

Read-only facts in plain words: the version, the machine the daemon runs on, the
config file (with a link that opens it in your editor, or a note that the daemon
runs on defaults and environment variables), the data folder, the database file
and its size, which agent clients have recorded sessions and when each was last
active, whether semantic recall is on - and when it is off, what to do about it -
whether the gardener is running and how often, and links to these docs. No
budgets or policy numbers: those live in Knowledge engine.

### Updates

`/console/settings?s=updates`

The [automatic update check](/updating/#automatic-update-checks) and
[automatic updates](/updating/#automatic-updates) in plain words: the **Mode** -
whether this install checks for new releases, and whether it installs them by
itself or only tells you, and why (a release build checks by default, a build
from source does not, and only an install the installer made updates itself) -
what a check sends (one anonymous request to `api.github.com` every few hours;
nothing when checks are off), the installed version and kind of build, the
command that updates this install (with a copy button, labeled **To update by
hand** on an install that updates itself), the newest release with its date and
release notes - or "up to date" - and when the daemon last checked and checks
next, including a streak of failed checks and why.

The note at the top says who decided the switches: **Default for this build**,
**Set in the config file or environment**, **Set in this console**, or
**Automatic updates set in this console**, with **Reset to the config file**
beside a console choice. Turning checks off from here always works. Turning them
on does not when the config file or `SEAMLESS_UPDATE_CHECK` says `false`: that
is final, the switch shows locked, and the form refuses - `false` promises no
update traffic at all. **Check now** asks GitHub once, right away (not more than
once a minute).

On an install that updates itself, the section adds **Automatic updates** - on
or off, who decided, and **Turn automatic updates off** / **on**, which
`update.auto: false` in the config file or environment locks off - and, while
they apply, **Next update** (the release it installs next and what it waits
for: the soak, live agent sessions, or a lull after its deadline), **Paused**,
**Held back**, **Skipped releases** and **Next try**. **Updating now** and
**Last attempt** appear whenever there is one; **Last attempt** carries the
outcome, the updater's own error and its log path - the one place in the
console either appears - and, when the updater refused to start, the fix.
**Update to v0.7.3 now** installs the newest release at once after a confirm -
skipping the soak, the idle wait, skipped releases and the backoff - and the
daemon restarts to finish. **Resume automatic updates** appears while they are
paused or holding releases back.

When a newer release is out, the Home health strip's version fact turns into
`v0.7.2 -- v0.7.3 available` and links here - its hover says whether it installs
by itself, and when - and a toast announces it if the console is open when the
daemon finds it. While an update runs, the fact reads `v0.7.2 -- updating to
v0.7.3`, and once v0.7.3 is serving and the updater is only confirming it,
`v0.7.3 -- confirming the update` (**Updating now** says **confirming** too).
The daemon re-reads the updater's record every few seconds while an update
runs, so both clear soon after it is confirmed. For a day after the daemon
starts on a new version, every page carries
a banner - "Seamless updated to v0.7.3 (from v0.7.2)" - with the release notes
and **Got it**, which hides it for that tab. It also says when Codex's hooks
changed with the update (re-approve them in Codex's `/hooks`) or the installer
reported warnings (run `seamlessd install-hooks`). For a week after automatic
updates pause themselves, or after an update the daemon started does not apply,
every page also carries a warning banner pointing here.

### Briefing

`/console/settings?s=briefing`

What every new agent session starts with. Three **presets** lead the section -
**Lean** (a short briefing: the binding rules, the freshest memories, little
else), **Balanced** (exactly the defaults), and **Rich** (more rules in full,
more recent work, and family memories) - each card showing the numbers that set
it apart. Picking one fills the form; nothing is saved until you save. Below
them, **Customize** holds every knob, grouped by what it shapes - the memory
index, recent work, the planning horizon, the project family, utility ranking,
and the safety ceiling - with each knob's "0 means" note under it. Customize is
open by default at Advanced. The section marks the preset your saved values
equal, or **Custom** when they match none, and re-marks it as you edit.

Saving writes a runtime override row in the DB. It layers over the file/env
values and wins until reset, and it applies from the next session start - no
daemon restart. It never touches your config file, so `seamless.yaml` stays the
thing you wrote. The token budget (`budgets.max_briefing_tokens`) is not a
briefing knob and no preset moves it. The form validates: a non-numeric knob or a
value that fails `Briefing.Validate()` comes back as an error flash, not a
silently-dropped save.

The **preview** shows the `<seam-briefing>` a new session in one project would
start with, under the values the form holds right now, saved or not. It sits
between the presets and Customize, or beside the form when the section is wide
enough. Pick a preset or change a knob and it follows within a moment. It
also shows the token estimate against `budgets.max_briefing_tokens` and the hard
cap, and flags a briefing that runs over the budget (only constraints, pinned
stages, and starred rows can put it there) or would be cut at the cap. The project picker
lists the registered projects and opens on the one your agents worked in last.
A preview is a read: it saves nothing, records no event, and no agent receives
it, so it never counts toward retrieval stats or utility. Values the save would
refuse, it refuses with the same message. For scripts,
`GET /console/settings/briefing/preview?project=<slug>&format=json` previews the
saved values. `POST` to the same path, with the form's fields plus `project`,
previews unsaved ones. It is the one POST in the console that writes nothing,
which is why it is not in the table of write routes above.

The **utility ranking** group holds `utility_weight` (utility's share of the
briefing sort key; 0 restores pure recency) and `utility_mode` (`auto` arms each
project as its demand history matures, `on` everywhere now, `off` never). Where
each scope stands against the readiness gates is in Knowledge engine. See
[Sessions & briefings](/concepts/sessions/#the-budget-and-what-survives-it) for
how the blended order behaves once active.

### Workspaces

`/console/settings?s=workspaces`

How repositories resolve to project scopes, and which projects share context.
**Project families** are editable here: create, rename, edit, or delete the named
groupings that [`seamlessd family`](/reference/cli-seamlessd/#seamlessd_family)
manages from the CLI - the same `project_families` setting, so a change on either
surface shows up on the other. Members are chosen from a closed picker of
registered projects; the CLI is the route for pre-registering a slug that does
not exist yet. Below it, one bounded directory lists every scope with its repo
routes and family tags. At Advanced each route also names the machine it lives
on, and repo paths that resolve to no project are listed as unbound.

### Knowledge engine

`/console/settings?s=engine`

The machinery, in three groups.

**Recall** - the embedder card shows the active provider and model, and when
embeddings are off, the exact cause, with distinct copy for the owner off switch,
the no-key lexical fallback, and a config error. The off/auto switch is a
settings row read once at serve start, so the page flags a pending restart
whenever the stored switch disagrees with the running process. Beside it, the
stored-vector counts: totals, the not-yet-embedded backlog, and a per-model
table that badges models the running embedder no longer writes as stale -
**re-embed everything** rewrites the corpus with the active model in the
background.

**Ranking** - the **utility activation by scope** table lists every scope with
each readiness gate against its threshold (demand events, memories touched,
history age - met gates turn green) and, for scopes still building, spells out
exactly what remains before auto arms ("needs 7 more events, 3d more history").
A per-scope **force** overrides the latch in either direction; when the global
mode is `on` or `off`, the table says the per-scope state is dormant until auto
returns.

**Storage & policy** - read-only: the data folder, the context budgets, the
database (path, size on disk including the WAL, schema version), and the
gardener's cadence and policy. Change these through the config file or
environment, then restart.

See [Configuration](/reference/configuration/) for what each knob does, and
[Sessions & briefings](/concepts/sessions/) for what they tune.

## Event detail

`/console/events/{id}`

What a Recent-activity or timeline row links to: a compact event review workspace
with the event's agent/session attribution, verbatim injected or transport
content, surfaced memories resolved to their live index entries (or flagged
missing), remaining payload fields, and raw JSON (the last two at the Advanced
level; `?format=json` always carries them). The Interactions inspector
uses the same content model in a side pane; opening the full page adds context
without changing the underlying event.

## Errors

A bad or stale URL renders a styled, layout-wrapped error page with a way back,
rather than dropping you on a bare `404 page not found` -- including any
`/console/` path no route claims, which gets a search box and links to the
common destinations. A 404 names the missing
entity and a 400 names the bad parameter and its valid values. A 500 stays
generic in the browser - the detail is in the log, not the response. Fragment
fetches (`?peek=1`) get a fragment-shaped error, since a full page injected into
the detail pane would nest the whole console inside itself.
