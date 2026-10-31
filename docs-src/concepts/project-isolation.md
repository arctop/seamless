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

## The console is exempt

The threat model is **agent-to-agent leakage, not hiding from you.** Console
search and views still see everything; isolated projects are simply badged with a
lock pill. Nothing about an isolated project's pages turns scary or
half-rendered - the pill and the Overview control are the whole story.

The one exception is a project [sealed to another OS account](/guides/peer-auth/)
under `peer_auth`: there the threat *is* your own account, so the console shows
no trace of it.

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

## Honest limits

This matters more than the feature list, so it is stated plainly.

**What this is.** A policy fence at every Seamless surface - briefings, recall,
tool reads and writes, the gardener. That is where accidental cross-contamination
actually happens, and it is now closed.

**What this is not.** It is **not** a defense against a local agent reading
`~/.seamless/memory/<project>/*.md` directly. Every MCP caller shares one static
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
and reserves a project for one account - so a project's own agents run as that
account, and nothing running as you reaches it, through a tool or through a
file. Use it with isolation, not instead of it: a sealed-to-an-account project
must also be confidential or sealed.

**Without it**, the hard boundary is a separate Seamless instance - its own data
dir, port, and key, run as its own account.
