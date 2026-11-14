---
title: Guides
description: Task-shaped walkthroughs - wiring an agent into the loop, writing memories that get recalled, coordinating a fleet, capturing plans, running trials, and fixing what broke.
---

Where the [concepts](/concepts/) pages explain how Seamless thinks, these pages
are organized around jobs: each one starts from something you are trying to do
and walks the shortest honest path through it, including the failure modes.

If you are connecting a client, start with
[Integrate your agent](/guides/integrate-your-agent/) - the MCP handshake, the
stdio bridge, session binding, and the scope discipline that keeps writes
landing in the right project. If the client is Cursor, Cline, Windsurf, or
Zed, [Connect Cursor, Cline, Windsurf & Zed](/guides/mcp-clients/) has the
verified config block for each, plus the capability table that says plainly
what a client without hooks gives up. Once an agent is writing,
[Write memories that get recalled](/guides/write-good-memories/) is the guide
that pays for itself: the description line is the retrieval surface, and the
difference between a store that compounds and one that fills with noise is
mostly the four habits that page names.

Two guides cover multi-agent work.
[Coordinate multiple agents](/guides/coordinate-agents/) shows fan-out over the
ready queue, planner/executor splits via plan composition, and what happens
when a claim holder dies mid-task. [Run research trials](/guides/research-trials/)
is the lab loop for systematic debugging - recording every attempt so parallel
agents share dead ends instead of repeating them. Related:
[Capture Claude Code plans](/guides/plan-mode/) explains how plan mode is
captured into notes and tasks automatically, and which hook does what.

[Screen writes and hold the suspicious ones](/guides/write-screening/) is the
opt-in guard against a poisoned memory: every agent write is screened for
secrets, injection markers and hidden characters, and the suspicious ones -
along with any change to a binding rule by a session that read outside text -
wait for your release instead of reaching every briefing.
[Feeding a security monitor](/guides/security-monitor/) is its opt-in neighbor:
a metadata feed of events tells a monitor what agents did as it happens, and a
snapshot of sessions, tasks and the commits they recorded lets it flag the
commits no tracked work accounts for.

The last three are operational. [Import, back up & restore](/guides/data/) covers
putting `~/.seamless` in git, what deleting `seam.db` actually costs (an index
rebuild, not data loss), and moving to a new machine.
[Share one daemon across your computers](/guides/network-install/) is the
opt-in multi-computer shape: the Server's setup, each Client, pairing on the
Devices page, approvals from any browser, and the daemon-side captures a
remote device honestly does not get.
[Troubleshooting](/guides/troubleshooting/) is symptom-first, written for a
system whose hooks deliberately fail open - where a broken install looks like
silence, not an error message.
