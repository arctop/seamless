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
and nothing inside it until you unlock it in that browser.

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
  leave it alone otherwise.
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
  ([details](/concepts/sessions/#moved-record)).
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

## Asking for access

A refusal at a confidential project's fence can become a request you approve,
when the daemon's config turns it on (`isolation.requests: true`; see
[the configuration](/reference/configuration/#isolation-requests)). It is off by
default, and off the refusal reads exactly as above and nothing is recorded.

Isolation itself is unchanged by it: a confidential project's sessions still
read open projects and global, and nothing leaves the project on its own. What
changes is that a session refused a read of a confidential project - naming it
with `project=` (in `recall` or any other read) or reading one of its items by
ULID - files an access request, and the refusal names it. A read that only
filters, such as `recall` without `project=` or `trial_query`, asks for
nothing:

```text
project example-finance is confidential: reads require a session bound to it -- ...
Remedy: read request R-7F3K is waiting -- the owner approves it in their own terminal with `seam approve R-7F3K`, or with Approve on the console's Access requests page; retry once it is approved. An approved read is read-only, and confines this session's writes to example-finance for the rest of its life: once it holds example-finance's content, it writes into no open project, example-ops included, and not into the global scope.
```

An agent can also ask explicitly, with its reason, from its own shell:
`seam access request example-finance --reason "..."`.

**Asking again.** One request waits per asker and project: a repeated refusal
names the request already waiting rather than filing another, and an explicit
ask with a reason fills in the reason of a waiting request that has none (the
refusal never has one to give). That reason is one of the facts you approve,
so if you were looking at the request when it arrived, the approval asks you
to look again. A session that already holds a live read grant of the project
from the same project is answered with it, and nothing is filed, until 30
minutes before it ends; from then on asking again files a new request.

**After you deny one.** The agent's next refused read of that project says you
denied it, and when, and files nothing: a refused call never asks you again
behind your back. If you denied it because it asked for the wrong thing, tell
the agent what to ask for instead; its explicit request
(`seam access request ... --reason "..."`) always files a new one.

**Who may ask:** a session bound to an open or a confidential project. Nothing
that involves a sealed project is ever offered, on either side: a sealed
project's refusal stays final, and a sealed project's session cannot ask for
anything outside it.

**What you approve.** `seam approve` (or the console's Access requests page)
shows the request's facts, including the line *content from example-finance
may be copied into example-ops's memories*: an approved session can read the
project, and nothing stops it from writing what it read into its own project.
You choose how long: until the session ends, 1 hour or 4 hours.

**What a grant is.** Read-only, one project, to the one session that asked
(under `peer_auth`, to the asking agent and the programs it started), until it
expires, you revoke it from the Access requests page, or the session ends.
While it lives, the granted project also joins that session's `recall` scope,
its trials show in `trial_query`, and its proposals show in `gardener_proposals`;
the briefing does not change. Another session is still refused.

**Writes follow what the session holds.** Every write must be cleared for
everything the session holds: each confidential or sealed project whose
content it may hold is the destination, or you approved its content into the
destination. An open project and the global scope are cleared only while the
session holds nothing fenced. So a session in a confidential project that
receives a grant writes only into its own project, and a session in an open
project that receives one writes only into the project it read - no open
project, its own included, and not the global scope. Its next tool result says
so, and the confinement is recorded as an event. Expiry and revocation end the
read, never what the session holds - what was read stays read.

**Writing up the work.** A session in a confidential project documents what
it did in its own project: task details, memories, notes and findings there
stay open to it, during the grant and after it. The grant tells the agent to cite the
confidential project by reference, with the project slug and the item's name or
id, and to leave that project's confidential details out. That is an instruction
to the agent, not a filter, which is why the line you approve still says content
may be copied. A session in an open project has no such place: it writes only
into the project it read, its `session_end` findings included. A plan or
planning-subagent report captured where the session may not write is dropped,
and the agent is told why.

**How much an approval proves.** Without `peer_auth` the approval is
**advisory**: `seam approve` asks for a y/N in a terminal (it refuses to run
without one) and commits over the console with the bearer key, and the
console's Approve does the same. Any process that holds the key - which every
local agent can read - can make that call, so an approval guards against an
agent wandering into a confidential project, not against one set on it. The
holder of such a grant is the conversation the agent's process identity
names, which is self-reported; a client that sends none is held by the session
a call speaks for, so one that rebinds with `session_start`, or names another
session with `session=`, writes where that session may. Under `peer_auth` a grant needs the same proofs as a
[locked project's lease](/guides/peer-auth/): your terminal through `sudo` to
the daemon's account, a passkey, or a Touch ID key. Its holder is then the
asking agent's process tree, and those moves are refused. An agent whose
conversation Seamless identifies cannot name the confidential project as its
own with `session_start` either: that files a move or grant request instead of
binding. A client that sends no identity still can, in either tier; only a
[lock](/guides/peer-auth/) holds against that.

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

**What makes it stronger.** [peer_auth](/guides/peer-auth/) puts an
authentication boundary under the fence: the daemon runs as its own OS account,
names each caller by the uid the kernel reports for its Unix-socket connection,
and lets you **lock** a project, so that even your own agents reach it only
with an approval you prove, through a tool or through a file. Use it with
isolation, not instead of it: a locked project must also be confidential or
sealed.

**Without it**, the hard boundary is a separate Seamless instance - its own data
dir, port, and key, run as its own account.
