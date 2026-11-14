---
title: Project isolation
description: Confidential and sealed projects - what each state promises, how tightening works, and the limits of the fence.
---

By default, projects are cooperative rather than isolated. Any agent can read or
write any project by passing `project`, global memories reach every briefing, and
family surfaces deliberately carry knowledge between related projects. That is
the right default for one person's interlocking work.

It is the wrong default for a client project, private research, or anything you
need to keep out of the agents serving your other work. **Isolation** is the
per-project setting that draws a real boundary.

## The three states

Isolation is **directional**. The two fenced states solve different problems and
should not be conflated.

| State | Nothing leaves | Nothing enters |
|---|---|---|
| `open` (default) | no | no |
| `confidential` | **yes** | no |
| `sealed` | **yes** | **yes** |

**`open`** shares normally. Global knowledge flows in; family surfaces may
include it.

**`confidential` - nothing leaves.** Sessions bound elsewhere can never read this
project: not by explicit `project=`, not by ULID, not through trials, not through
recall. It is excluded from family surfaces and from the gardener's cross-project
passes. Critically, sessions bound *to* it cannot write *outside* it - not to
another slug, and not to `project: global`, because an agent-initiated global
write is a leak too.

Inbound is deliberately unchanged: its agents still receive your global memories
and can still read open projects. This is the common case - sensitive work that
still wants your conventions available inside it.

**`sealed` - nothing in or out.** Confidential plus the inbound fence. Briefing,
recall, and the prompt matcher scope to the project alone; global memories are
dropped, there is no family, and there are no cross-project reads in either
direction. A clean room.

Because a sealed project's world is genuinely smaller, its agents are told so.
The briefing carries a line stating the mode, on both the main and subagent
surfaces, so an agent inside understands why a write outside will be refused
rather than discovering it as an unexplained error.

Isolation is about what flows in and out of a project. Whether even the
project's own agents need a fresh approval from you first is a second setting,
the **lock**, and "locked" never means `sealed`. A sensitive
project is normally `confidential` and locked:
[Lock projects behind your approval](/guides/peer-auth/#two-settings-isolation-and-the-lock)
says when to use which.

## The console is exempt

The threat model is **agent-to-agent leakage, not hiding from you.** Console
search and views still see everything; isolated projects are simply badged with a
lock pill. Nothing about an isolated project's pages turns scary or
half-rendered - the pill and the Overview control are the whole story.

The one exception is a [locked project](/guides/peer-auth/) under `peer_auth`:
there the threat *is* your own account, so the console shows its name and counts,
and nothing inside it until you unlock it in that browser. A session's
[Conversation card](/reference/console/#conversation) follows the same rule: a
locked project in it shows its name and nothing more.

## Isolation requires a standalone project

A project cannot be isolated while it is entangled in a family: no family
membership, no parent, no children.

Tightening therefore **detaches** family membership and the parent link, and the
confirmation step shows you exactly which. A project **with children refuses to
tighten** until they are re-parented - Seamless will not silently orphan a scope
that inherits from it.

This keeps the fence surface small. Rather than threading isolation checks
through every topology path, the rule guarantees there is no topology to thread.
The briefing-side exclusions remain anyway, as defense in depth against stale
data.

## Tightening and loosening

**Tightening is the ceremonious direction**, because it changes what other agents
can see. It is two-step in the console: choosing confidential or sealed returns a
confirmation panel stating exactly what will change - the family and parent it
will detach from, or the children blocking it, plus the promise of the state you
picked - and applies only when you confirm.

**Loosening is immediate.** It is fully reversible and nothing leaked while the
fence was up, so there is nothing to confirm.

## Knowledge that already got out

Raising a fence does nothing about what escaped before it existed. The most
likely leak is **global memories written by this project's own sessions** - they
were the right call at the time and are now injected into every other project's
briefing, forever.

So tightening runs a provenance audit: it traces global memories back through
`source_session` to the sessions that wrote them, and for each one that belonged
to this project it files a **relocate** proposal in the
[Gardener inbox](/concepts/gardener/) offering to move it behind the fence.

Two things this deliberately is not. It does not move anything on its own -
the gardener only ever proposes, and dismissing the proposal is how you say the
memory really is general knowledge. And it does not run on a timer: once the
fence is up, the write funnel refuses this project's sessions any write outside
it, so the leak set is closed at the moment of tightening and never grows.

The confirmation step tells you the count before you commit, and distinguishes a
proven zero from an unanswered question - if the audit could not run, it says
nothing rather than reporting "none".

### In the console

The control lives on the project's **Overview** tab: a three-option segmented
control with each state's promise written under it. The isolation change is
recorded as an event, so it appears in Recent activity like any other change.

### From the CLI

```bash
seam project isolation <slug>                    # print the current state and its promise
seam project isolation <slug> confidential       # print consequences, apply nothing
seam project isolation <slug> confidential --yes # apply
seam project isolation <slug> open               # loosen, immediate
```

Tightening requires `--yes`, which stands in for the console's confirmation step:
without it the command prints the same consequences and exits having changed
nothing.

## What agents see

- **`project_list`** reports each project's isolation state, so an agent can
  discover the boundary rather than guess at it.
- **`project_create`** accepts `isolation`. There is no tool to change it
  afterwards: tightening detaches topology and is an owner decision, not an agent
  one.
- **Refusals state the rule and the remedy** and never echo the content they are
  withholding - an error that quoted the memory it was protecting would leak the
  very thing the fence exists to keep in. They do not hide that the project
  *exists*, because every scope error already lists the machine's projects. An
  outside agent refused at the fence is told to bind to the project with
  `session_start project=<slug>` if its task really is that project, and to
  leave it alone otherwise. With [access requests](#asking-for-access) on, a
  Claude Code or Codex agent is told instead to ask you to move its
  conversation into the project, or for a grant of it, and the refusal names
  the request it filed.
- **A conversation that binds itself in keeps its record inside.** A Claude
  Code or Codex agent working in an open project's repository (or in none)
  that binds itself to a confidential or sealed project with
  `session_start project=<slug>` records its SessionEnd harvest (for Codex,
  each `Stop`'s findings), its plan captures and its planning-subagent reports
  in that project, under the session `session_start` opened there, with their
  events - for the rest of the conversation, wherever it binds afterwards. The
  session it left in the open project ends with a pointer naming the project
  and nothing it holds. One that binds itself to two isolated projects keeps
  that record in none of them. A confidential or sealed project's own agent
  keeps its record at home, whatever it binds itself to. This holds on every
  install, whether or not `isolation.requests` is on
  ([details](/concepts/sessions/#moved-record)). With `isolation.requests` on,
  everything the hooks record follows the conversation's home instead,
  wherever the agent works, and nothing lands where the clearance refuses
  ([details](/concepts/sessions/#home-record)).
- **Task answers withhold what blocks across the fence.** A confidential
  project's task that blocks an outside project's task reaches an outside
  session as a withheld entry in `tasks_ready`, a `depends_on_withheld` count in
  place of its id, and a count in the claim's "task blocked" refusal - never its
  title or its id.

## What a security monitor sees

A monitor you [feed from Seamless](/guides/security-monitor/) follows a fenced
project without learning its name: the metadata feed sends a confidential or
sealed project's slug only as a keyed hash, and the commit-matching snapshot
never names its repository. Both are off by default.

## Asking for access: one home per conversation {#asking-for-access}

A refusal at a confidential or sealed project's fence can become a request you
approve, when the daemon's config turns it on (`isolation.requests: true`; see
[the configuration](/reference/configuration/#isolation-requests)). It is off
by default, and off every refusal reads exactly as above and nothing is
recorded.

Isolation itself is unchanged by it: a confidential project's own sessions
still read open projects and global, and nothing leaves a fenced project
without your approval. What changes is who the fence judges. Without the
switch it judges the session a connection is bound to, which an agent can
change at will (`session_start`, `session=`). With it, the fence judges the
**conversation**, and what a conversation has read only grows.

### The conversation, its home and its label {#home-and-label}

- **The conversation** is one agent context. Without `peer_auth` it is the
  client's own session - the Claude Code or Codex session id that the
  agent-process identity on every call names
  ([how](/concepts/sessions/#process-binding)) - so `session_start`,
  `session=` and a move leave it the same conversation; a resume continues it,
  and `/clear` starts a new one. Under
  [`peer_auth`](/guides/peer-auth/#read-grants-and-the-advisory-tier) it is the
  agent's process tree as the kernel reports it, for the life of the process,
  `/clear` included.
- **Its home** is the project its own session is in: the repository's project
  when the agent starts, and after an approved move the project it moved
  into. Its own record goes there, and calls that name no project work there.
  The home does not limit what the conversation may reach.
- **Its label** is every confidential or sealed project whose content it may
  hold: a fenced home, every project a grant opened to it, every project it
  moved through, and every fenced project one of its own sessions is in. The
  label only grows: a grant's expiry or revocation ends the reads, never what
  was read. A project you loosen to `open` leaves every label.

A Claude Code or Codex agent running the hooks with a current `seam` is
identified this way, from its own session. A client that sends no agent-process
identity is not: it is held by the session it is bound to, and binds exactly as
it did before.

### The one rule {#the-one-rule}

**Every write must be cleared for everything the conversation holds.** A
destination is cleared when every project in the label is that destination or
has a **flow** into it: your approval, recorded with the request that listed
it, for one project's content to be written into another. An open project and
the global scope are cleared only while the label is empty.

So a conversation that holds nothing fenced writes wherever it always could.
One that holds `example-finance`'s content writes only where you let that
content go, and never again into an open project or the global scope - its own
open home included. A confidential or sealed project's own conversation
writes into it as before. The rule lives in the fence itself, so every tool,
every hook and every other agent write meets it, and a refusal names the
projects the conversation holds and where it may still write:

```text
memory_write: this session holds content from example-finance, example-legal, so it writes only into example-legal: a write into project example-hr is refused
```

### What a refused call asks for {#what-a-refusal-asks-for}

The kind of request follows from the conversation's home and the project,
never from the agent's choice:

| The conversation's home | The project it reaches | The request |
|---|---|---|
| open, or the global scope | confidential | a **move** into the project |
| confidential | another confidential project | a **grant** of the project |
| any | sealed | a **move** into the project |
| sealed | confidential | none: a sealed project's conversation reaches no confidential project |

An open project needs no request. A conversation that holds fenced content
never goes back to one, so a write there, or a `session_start` there, is
refused, and nothing is filed.

What files one:

- **A refused read** - naming the project with `project=` (in `recall` or any
  other read), or reading one of its items by ULID. A read that only filters,
  such as `recall` without `project=` or `trial_query`, asks for nothing.
- **A refused write**, which asks for read and write.
- **`session_start project=<slug>`** into a fenced project, which binds
  nothing, asks for read and write, and leaves the home where it is. A
  `session=` handle naming another project's session acts as that session only
  where the home may read and write; anywhere else the call is refused, and
  nothing is filed.
- **The agent's explicit ask**: `access_request` (with `write` to ask for read
  and write) or, from its own shell,
  `seam access request example-finance --reason "..." [--write]`.

The refusal names the request it filed. Here a conversation working in
`example-ops`, an open project, reads one of `example-finance`'s items by id:

```text
notes_read: project example-finance is confidential: only a conversation working in it reads it -- if your task is about example-finance, ask the owner to move this conversation into it (session_start project=example-finance asks); otherwise leave it alone
Remedy: move request R-7F3K is waiting -- the owner approves it in their own terminal with `seam approve R-7F3K`, or with Approve on the console's Access requests page; retry once it is approved. Record what belongs in example-ops now: once the owner approves the move, this conversation works in example-finance for good, and it can no longer write to example-ops or the global scope until the conversation ends. After the move it can still close a task it claimed there, with a receipt: tasks_update with status done or dropped, and no text. Its first result after the approval says so and carries example-finance's briefing.
```

A locked project stays the lock's: a call refused at the lock files the lock's
request, as without the switch. An agent working elsewhere that needs to work
in a locked project asks with `access_request write=true` (or
`seam access request <project> --write`), which files the move or the grant
its home calls for; approving that opens the lock to it too. A lease alone
opens a locked project only to the conversation that works in it
([details](/guides/peer-auth/#read-grants-and-the-advisory-tier)).

**Asking again.** One request waits per conversation and project, whatever its
kind: a repeated refusal names the request already waiting rather than filing
another, and an explicit ask with a reason fills in the reason of a waiting
request that has none (a refusal never has one to give). That reason is one of
the facts you approve, so if you were looking at the request when it arrived,
the approval asks you to look again. A conversation that already holds a live
grant of the project, with at least the access it asks for, is answered with
it, and nothing is filed, until 30 minutes before it ends; from then on asking
again files a new request.

**After you deny one.** The agent's next refused call for that project says you
denied it, and when, and files nothing: a refused call never asks you again
behind your back. If you denied it because it asked for the wrong thing, tell
the agent what to ask for instead; its explicit request always files a new one.

### What you approve {#what-you-approve}

Every request states the whole carry: the conversation's home, every fenced
project it would hold once approved, and every project it could write to then.
Approving clears exactly the flows those facts list. `seam approve`, each
request's page on the console (its passkey page under `peer_auth`) and the
advisory tier's in-session dialog show it, and every surface shows the line
that says what approving lets happen among the warnings:

- **A move**: *This agent has worked in example-ops and read example-legal. If
  you approve, it works in example-finance from now on, and what it saw may be
  written into example-finance. It can no longer write to example-ops or the
  global scope until the conversation ends.* Into a sealed project the line
  adds that the project is sealed, so nothing enters it otherwise, and that
  approving lets in what the agent carries.
- **A grant**: *content from example-finance may be copied into example-legal*,
  and into every other project the conversation may write to. Between
  confidential projects you choose **read-only** (the default) or **read and
  write**, which adds *and it may change example-finance's tasks, memories and
  notes; content from example-legal may be copied into example-finance*.

A grant lasts as long as you choose: until the conversation's session ends, 1
hour or 4 hours, capped by `peer_auth.max_lease` (8 hours when unset). A move
lasts for good; into a locked project, the lock it opens lasts as long as you
choose.

### Moves {#moves}

An approved move takes the conversation into the project for good:

- the session it worked in ends, keeps the findings the agent wrote, and gains
  a pointer:
  `Moved to project example-finance; this conversation continues there as session cc/8dd2fd5b-55d96b8d15ff0104@example-finance, and its findings go there, not here.`
  Its task claims stay, for [receipts](#receipts);
- a child session opens in the project, linked to it, and becomes the
  conversation's own session, with the same client session and agent process;
- that project is the conversation's home from then on: its record goes there,
  and calls that name no project work there;
- it can no longer write to the home it left, or to the global scope, until the
  conversation ends (under `peer_auth`, until the agent process exits);
- its first tool result after the approval says so and carries the new home's
  briefing, once, even across a daemon restart. When it moved out of a
  confidential or sealed project, that notice also says where what it writes
  about that project goes: [into its new home, by reference](#source-references).

Before the move, the request tells the agent to record what belongs in its
current home first. Moving in is the only way into a sealed project, and it is
how an open project's conversation reaches a confidential one. A resumed
conversation resumes in the project it moved into
([details](/concepts/sessions/#home-record)).

### Grants: read-only, or read and write {#grants}

A grant opens one confidential project to a conversation working in another
confidential project. **Read-only**, the conversation reads the project, and
what it reads may be written only into the projects the approval names: its
home, and any project it already holds a read-and-write grant of. **Read and
write**, it also changes the project's tasks, memories and notes as the
project's own sessions do, and what it holds may be written into it. Nothing
that involves a sealed project is ever granted, on either side.

While a grant lives, the project also joins the conversation's `recall` scope,
its trials show in `trial_query`, its proposals show in `gardener_proposals`,
and prompt recall and a subagent's relevant memories read it; the briefing does
not change. A grant ends at its expiry, when you revoke it from the Access
requests page, or when the conversation's session ends. Ending it ends the
reads, never what the conversation holds.

The grant tells the agent to write up its work in its home and to refer to the
project's items by reference, leaving its confidential details out. That is an
instruction to the agent, not a filter, which is why the line you approve
still says content may be copied.

### Receipts {#receipts}

A move is one way: once you approve it, the conversation can no longer write
into the project it left. One write is still allowed there, because without it
the work it claimed before the move would stay in progress for good: a
**receipt**. The conversation closes a task its pre-move session claimed -
`done` or `dropped`, nothing else - and may point at one item in its new home
(`reference=example-finance:<id>`, the project slug and the item's id). That
is the only thing that crosses back: a receipt carries no title, body or other
text, and Seamless refuses one that tries, or whose reference names no item in
the home. The typical case is an orchestrator in an open project that claims a
build step, then moves to read a confidential project's live data: the receipt
closes the step without carrying any of that data back. The task's history
shows the receipt and its reference under the session that claimed the step.
A conversation whose home is sealed sends no receipts, and a task someone else
has claimed since, or one you released from the console, is no receipt's to
close. The move's request and its notice both tell the agent so.

### Where the conversation's record goes {#records-follow-the-home}

With the switch on, everything the hooks record for a conversation goes to its
home, wherever the agent works: the SessionEnd harvest (for Codex, each
`Stop`'s findings), plan captures and planning-subagent reports, and the
events about them. After a move that is the project it moved into. Prompt
recall and a subagent's briefing read the home and the projects its live grants
open. Where the rule refuses the home - an open home holding fenced content -
nothing is recorded, and the session's findings say why
([details](/concepts/sessions/#home-record)).

### Provenance stamps {#provenance-stamps}

Everything an agent writes while it holds content from a confidential or
sealed project other than the destination records *may contain content from
example-finance (grant R-7F3K)*: one stamp per such project, naming the grant
or move that let its content in. That covers memories, notes, tasks, trials,
session findings, and plan and subagent captures. Memories and notes keep it in
frontmatter (`may_contain`); the rest only in the database. No write removes
it, and a memory that supersedes another carries both memories' stamps. It
names projects and approvals, never content. It shows on `recall` hits,
`memory_read`, `notes_read`, `tasks_list` and `tasks_ready` answers, and the
console's item pages and search; on a project's page, **Came from** narrows
its items to the ones that name a project
([console](/reference/console/#projects)). Deleting `may_contain` from a file
by hand removes the warning. Off, nothing is stamped.

### Source references {#source-references}

Where the agent writes follows one placement rule: the destination must be
cleared first; then the project the item is about; and when that project is not
cleared, the item goes in the conversation's home, citing what it came from by
reference. A reference is body text in one form, the project slug, a colon and
the item's full id - `example-finance:01M7...` - so no tool takes an argument
for it. The grant's text, the refusals that send a conversation home and the
notice after a move out of a fenced project teach that form.

The [gardener](/concepts/gardener/) reads references back from stamped
memories, and never a source's content:

- **stale-copy** proposes archiving a copy whose cited memory, note or task
  changed after the copy was last written;
- **copy-relocate** proposes moving a copy whose stamps and references all name
  one other project into that project. Before you move a stamped memory into a
  project less fenced than one it may hold content from, the console warns you,
  and blocks nothing.

A reference that does not resolve in the project it names, or points into a
project the copy may not read, is skipped.

### How much an approval proves {#how-much-an-approval-proves}

Without `peer_auth` an approval is **advisory**. `seam approve` asks for a y/N
in a terminal (it refuses to run without one) and commits over the console
with the bearer key; the console's Approve does the same, and so does Approve
in the agent's in-session dialog. Any process that holds the key - which every
local agent can read - can make that call, so an approval guards against an
agent wandering into a fenced project, not against one set on it. The
conversation is the one the agent's process identity names, which is
self-reported; a client that sends none is held by the session a call speaks
for, so one that rebinds with `session_start`, or names another session with
`session=`, writes where that session may.

Under `peer_auth` a grant or a move needs the same proofs as a
[locked project's lease](/guides/peer-auth/): your terminal through `sudo` to
the daemon's account, a passkey, or - for a grant - a Touch ID key; Touch ID
approves no move yet. The conversation is then the asking agent's process tree,
and a move or grant into a locked project opens its lock too, in the same
approval.

In either tier, an agent whose conversation Seamless identifies cannot name a
fenced project as its own: `session_start` files a move or grant request
instead of binding, and `session=` acts as another project's session only
where the home may read and write. A client that sends no identity still can,
and so can an agent started in that project's repository, which is that
project's own; only a [lock](/guides/peer-auth/) holds against that.

## Honest limits

This matters more than the feature list, so it is stated plainly.

**What this is.** A policy fence at every Seamless surface - briefings, recall,
tool reads and writes, the gardener. That is where accidental cross-contamination
actually happens, and it is now closed.

**What this is not.** It is **not** a defense against a local agent reading
`~/.seamless/memory/<project>/*.md` directly. Without `peer_auth`, A2A's recall
names its scope in the request's `metadata.project`, and the fence does not
judge it there: the bearer key can read a confidential project that way. Under
`peer_auth` A2A speaks for no session, so it refuses a confidential or sealed
project. Every MCP caller shares one static
bearer key, and the fence keys off the **self-reported session binding** - an
explicit `session_start`, or the agent-process identity a Claude Code or Codex
agent's `seam` transport sends, which is just as self-reported
([how it binds](/concepts/sessions/#process-binding)). That automatic binding is
what lets a confidential project's own agents read it without a `session_start`,
and what keeps a confidential or sealed project's agents from writing outside
it. A connection bound to nothing - a hookless client that skipped
`session_start`, or an older `seam` that sends no identity - is judged as the
global scope, which fails closed against reading an isolated project but also
means the fence cannot recognize it as belonging to one.

**With access requests on**, the same holds, and more:

- Seamless guards its own surfaces only. An agent can still put what it read
  into files, commits, pull request text or another tool.
- Without `peer_auth` the conversation is the agent's word: the agent-process
  identity is self-reported, and `/clear` starts a new conversation with an
  empty label. A client that sends no identity is held by its session, as
  before.
- A fork, or a separate agent the conversation starts, is a new conversation to
  Seamless, and what it prints back is invisible to it.
- The event log records each tool call under the calling session's project,
  with its arguments and the start of its result, so a granted read's answer
  can appear in the asking project's history on the console. Agents read no
  events, and text already in the log stays there.

**What makes it stronger.** [peer_auth](/guides/peer-auth/) puts an
authentication boundary under the fence: the daemon runs as its own OS account,
names each caller by the uid the kernel reports for its Unix-socket connection,
and lets you **lock** a project, so that even your own agents reach it only
with an approval you prove, through a tool or through a file. Use it with
isolation, not instead of it: a locked project must also be confidential or
sealed.

**Without it**, the hard boundary is a separate Seamless instance - its own data
dir, port, and key, run as its own account.
