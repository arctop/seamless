# Guides

> Task-shaped walkthroughs - wiring an agent into the loop, writing memories that get recalled, coordinating a fleet, capturing plans, running trials, and fixing what broke.

Where the [concepts](https://thereisnospoon.org/docs/concepts/) pages explain how Seamless thinks, these pages
are organized around jobs: each one starts from something you are trying to do
and walks the shortest honest path through it, including the failure modes.

If you are connecting a client, start with
[Integrate your agent](https://thereisnospoon.org/docs/guides/integrate-your-agent/) - the MCP handshake, the
stdio bridge, session binding, and the scope discipline that keeps writes
landing in the right project. If the client is Cursor, Cline, Windsurf, or
Zed, [Connect Cursor, Cline, Windsurf & Zed](https://thereisnospoon.org/docs/guides/mcp-clients/) has the
verified config block for each, plus the capability table that says plainly
what a client without hooks gives up. Once an agent is writing,
[Write memories that get recalled](https://thereisnospoon.org/docs/guides/write-good-memories/) is the guide
that pays for itself: the description line is the retrieval surface, and the
difference between a store that compounds and one that fills with noise is
mostly the four habits that page names.

Two guides cover multi-agent work.
[Coordinate multiple agents](https://thereisnospoon.org/docs/guides/coordinate-agents/) shows fan-out over the
ready queue, planner/executor splits via plan composition, and what happens
when a claim holder dies mid-task. [Run research trials](https://thereisnospoon.org/docs/guides/research-trials/)
is the lab loop for systematic debugging - recording every attempt so parallel
agents share dead ends instead of repeating them. Related:
[Capture Claude Code plans](https://thereisnospoon.org/docs/guides/plan-mode/) explains how plan mode is
captured into notes and tasks automatically, and which hook does what.

The last three are operational. [Import, back up & restore](https://thereisnospoon.org/docs/guides/data/) covers
putting `~/.seamless` in git, what deleting `seam.db` actually costs (an index
rebuild, not data loss), and moving to a new machine.
[Share one daemon across a LAN](https://thereisnospoon.org/docs/guides/network-install/) is the opt-in
multi-device shape: the server config, TLS, the pairing command, and the
daemon-side captures a remote device honestly does not get.
[Troubleshooting](https://thereisnospoon.org/docs/guides/troubleshooting/) is symptom-first, written for a
system whose hooks deliberately fail open - where a broken install looks like
silence, not an error message.

- [Integrate your agent](https://thereisnospoon.org/docs/guides/integrate-your-agent/): Wire another agent into the loop - the MCP handshake, stdio bridge, session binding, scope discipline, and seam CLI fallback.
- [Connect Cursor, Cline, Windsurf & Zed](https://thereisnospoon.org/docs/guides/mcp-clients/): Point any MCP client at Seamless's local streamable-HTTP endpoint with bearer auth - a verified config block per client, and an honest account of what a client without hooks does and does not get.
- [Write memories that get recalled](https://thereisnospoon.org/docs/guides/write-good-memories/): The description is the retrieval surface, not a label - how to write one, how to pick a kind, and the four habits that fill a store with noise.
- [Coordinate multiple agents](https://thereisnospoon.org/docs/guides/coordinate-agents/): Fan-out over the ready queue, planner/executor splits via plan composition, shared-lab investigation, and what happens when a claim holder dies.
- [Capture Claude Code plans](https://thereisnospoon.org/docs/guides/plan-mode/): How plan mode is captured automatically - which hook does what, a plan's life from draft to approved, and the escape hatches.
- [Run research trials](https://thereisnospoon.org/docs/guides/research-trials/): The lab loop for systematic debugging - recording what was tried, letting parallel agents share dead ends, and distilling the result into memory.
- [Import, back up & restore](https://thereisnospoon.org/docs/guides/data/): One archive with seamlessd export, restoring or merging it with seamlessd import, putting ~/.seamless in git, what deleting seam.db actually costs, and moving to a new machine.
- [Share one daemon across a LAN](https://thereisnospoon.org/docs/guides/network-install/): Run one seamlessd for several devices - the server config, TLS with mkcert or openssl, the client-config pairing command, and the captures a remote device honestly does not get.
- [Seal projects to OS accounts](https://thereisnospoon.org/docs/guides/peer-auth/): peer_auth - one daemon, a Unix socket that names each caller by its uid, projects only their own OS account can reach, and the deployment that makes the files hold too.
- [Troubleshooting](https://thereisnospoon.org/docs/guides/troubleshooting/): Symptom-first fixes for a system whose hooks fail open - where silence, not an error, is what a broken install looks like.
