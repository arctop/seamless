# Make Claude Code remember between sessions

> A new Claude Code session starts with no memory of the last one. Three ways to give it persistent memory across sessions - CLAUDE.md, the built-in auto memory, and a shared memory every session is briefed from - and where each one stops.

A new Claude Code session starts with an empty context window. It knows
nothing of the last one unless something loads it again. There are three ways
to do that: a `CLAUDE.md` you write, the auto memory Claude Code keeps for
itself, and a shared memory, such as Seamless, that every session, subagent,
and client is briefed from. This page says what each one carries into the next
session, where it stops, and how to set up the third.

(`claude --continue` reopens the last conversation itself. This page is about
what a fresh session knows.)

The first two are built in and free, and worth using whatever else you add.
What this page says about them was checked against
[Anthropic's memory documentation](https://code.claude.com/docs/en/memory) in
November 2026.

## What Claude Code remembers by itself

**`CLAUDE.md`** is a file of instructions you write. Claude Code reads it at
the start of every session, and re-reads the project-root file from disk after
a compaction, so it is the right home for standing rules: build commands,
conventions, the shape of the codebase. It is yours to maintain. Anthropic
suggests keeping each file under 200 lines, because a longer one uses more
context and is followed less closely.

**Auto memory** is notes Claude Code writes for itself, from your corrections
and preferences. It is on by default in local sessions and stored as plain
markdown under `~/.claude/projects/<project>/memory/`: a `MEMORY.md` index and
one topic file per memory. The first 200 lines or 25KB of the index load at
session start, and Claude opens a topic file when it needs one. You can read
and edit all of it with `/memory`.

If you work in one repository, on one computer, with Claude Code alone, those
two may be all you need.

### Where each one is stored

| | Where it lives | Who writes it | Loaded |
|---|---|---|---|
| `CLAUDE.md` | In the repository, or `~/.claude/CLAUDE.md` for every project | You | Every session, in full |
| Auto memory | `~/.claude/projects/<project>/memory/` | Claude | The first 200 lines or 25KB of `MEMORY.md`, at session start |
| Seamless | `~/.seamless/memory/<project>/`, one file per memory | Your agents | A briefing within a token budget at session start, then what matches each prompt |

## Where the built-in memory stops

Each of these is from Anthropic's page, not a guess:

- **One computer.** Auto memory is machine-local. It is not shared across
  machines or cloud environments.
- **One repository.** Every repository has its own memory directory, so a rule
  learned in one project is unknown in the next.
- **Not subagents.** The main conversation's auto memory is not loaded into
  subagents. A fork is the exception, and a subagent can be given a memory of
  its own, which is a separate directory.
- **A limit on the index.** Only the first 200 lines or 25KB of `MEMORY.md`
  load. Anything past the limit is dropped at the next load, so Claude Code
  asks Claude to shorten the file as it gets close.
- **Its own judgment.** Claude decides what is worth saving and does not save
  something every session, so whether a lesson was kept is something you find
  out later.
- **One client.** It is Claude Code's own store. Codex keeps a separate one
  and reads neither.

None of this is a defect. It is a memory for one client in one place, and it
does that job.

## What a shared memory adds

Seamless keeps the same kind of knowledge in one store that every agent you
run reads and writes:

- **Every client is briefed from it.** A hook injects a `<seam-briefing>` into
  each new Claude Code and Codex session before the first prompt. What a Claude
  Code session saved on Monday is in a Codex session's briefing on Tuesday. See
  [Share memory between Claude Code and Codex](https://thereisnospoon.org/docs/guides/share-memory-claude-code-codex/).
- **Rules cannot fall out.** A memory of kind `constraint` is never dropped
  from its project's briefing to fit the token budget that trims everything
  else. The first four print in full and the rest are listed by name. See
  [Sessions & briefings](https://thereisnospoon.org/docs/concepts/sessions/#the-budget-and-what-survives-it).
- **Subagents are briefed too.** Every Task subagent starts with the pinned
  constraints and the memories that match its task.
- **Each prompt pulls what fits.** Your prompt is matched against the store,
  and a memory that fits arrives with it as a `<seam-recall>` block.
- **A compaction does not lose it.** The session-start hook also fires after a
  compaction, so the briefing lands again.
- **The work is remembered, not only the knowledge.** Plans, their steps, and
  what the last session left open are in the briefing, so *continue where we
  left off* has something to continue from. See
  [Tasks & plans](https://thereisnospoon.org/docs/concepts/tasks-and-plans/).
- **More than one computer, when you want it.** By default everything stays on
  one computer. In a [networked](https://thereisnospoon.org/docs/concepts/networked/) install one computer
  keeps the data and the others connect to it, so a laptop and a desktop share
  one memory.
- **The handoff is written for you.** What a session learned is harvested when
  it ends and listed under *Recent findings* in the next briefing, so the next
  session sees where the last one stopped without a `HANDOFF.md` you keep by
  hand.
- **Forgetting is yours to approve.** A memory is replaced by a newer one and
  stays readable, and the [gardener](https://thereisnospoon.org/docs/concepts/gardener/) only proposes merges
  and retirements.

It runs beside `CLAUDE.md` and auto memory rather than instead of them. Keep
your standing instructions in `CLAUDE.md`; Seamless holds what gets learned and
decided along the way.

One thing no memory changes: a rule that is loaded can still be ignored.
Anthropic says of `CLAUDE.md` that Claude "reads it and tries to follow it, but
there's no guarantee of strict compliance", and the same goes for a briefing.
What Seamless adds is that the rule, or at least its name, is in front of every
session and subagent, and again after a compaction. For a step that must never be
skipped, write a [hook](https://code.claude.com/docs/en/hooks-guide), which
runs whatever the model decides.

## Set it up

**macOS · Linux:**

```bash
curl -fsSL https://thereisnospoon.org/install | sh
```


**Windows:**

```powershell
irm https://thereisnospoon.org/install.ps1 | iex
```


The installer wires the clients it finds. Restart Claude Code once and open it
in a repository:

```bash
cd ~/code/myrepo && claude
```

The session opens with a `<seam-briefing>` block. The
[Quickstart](https://thereisnospoon.org/docs/quickstart/) shows what it looks like and what to check if it is
missing.

## Check that it remembers

Tell the session one rule:

```prompt
Save this to Seamless as a constraint: migrations run before the deploy, never after.
```

Then end the session, start a fresh one in the same repository, and read the
top of its briefing. The rule is there, before you have typed anything, and it
is also a file you can open:

```bash
ls ~/.seamless/memory/
```

From here the agent writes on its own when it learns something worth keeping.
[First prompts to try](https://thereisnospoon.org/docs/first-prompts/) has more requests to paste, including
one that reads your past Claude Code sessions for the decisions worth keeping.

## Which one to use

| If you want | Use |
|---|---|
| Standing instructions you wrote, loaded every session | `CLAUDE.md` |
| Claude Code to note your preferences without setup | Auto memory |
| The same knowledge in Claude Code, Codex, and subagents | Seamless |
| Rules that are in front of the agent every session | Seamless constraints, or a short `CLAUDE.md` |
| Plans and open work carried into the next session | Seamless |
| Parallel agents that do not pick the same task | Seamless: see [Coordinate multiple agents](https://thereisnospoon.org/docs/guides/coordinate-agents/) |

For the same comparison across other memory tools, see
[Seamless and the alternatives](https://thereisnospoon.org/compare/).
