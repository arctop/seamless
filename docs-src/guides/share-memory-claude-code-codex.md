---
title: Share memory between Claude Code and Codex
description: Claude Code and Codex do not share memory - each keeps its own store and reads neither's. How to give both one store, so what a Claude Code session learned is in the next Codex session's briefing, with a two-minute test.
---

Claude Code and Codex do not share memory. Each keeps its own store on your
computer and reads neither's, so a decision one of them made is unknown to the
other until you paste it across. To share what they learn, both have to read
and write one store. Seamless is that store: one command wires both clients,
and a memory saved in a Claude Code session is in the next Codex session's
briefing, and the other way round.

## What each one keeps by itself

Checked in November 2026 against
[Anthropic's memory documentation](https://code.claude.com/docs/en/memory) and
[OpenAI's Codex memories page](https://learn.chatgpt.com/docs/customization/memories).

| | Claude Code | Codex |
|---|---|---|
| Its own memory | Auto memory, on by default in local sessions | Memories, off by default |
| Stored | `~/.claude/projects/<project>/memory/` | Under `~/.codex` |
| Written | During the session, when Claude judges something worth keeping | In the background, after a chat goes idle |
| Reads the other's | No | No |

Both are plain files on your own disk, and both are worth leaving on.

## What `AGENTS.md` shares, and what it does not

One file already crosses the gap. Codex reads `AGENTS.md`, and Claude Code
(v2.1.277 and later) reads a repository's `AGENTS.md` too when the repository
has no `CLAUDE.md`. When it has both, a `CLAUDE.md` that holds the line
`@AGENTS.md` imports it, so one file still serves both tools. That shares the
instructions you wrote: conventions, commands, standing rules.

It does not share what the agents learn while they work. Nothing writes the
reason for Monday's fix into `AGENTS.md` unless you do, and neither client's
own memory reaches the other. That is the gap a shared store closes.

## The workarounds, and where they stop

Before a shared store, people bridge the two clients by hand. Each of these
works up to a point:

- **One instruction file for both.** `AGENTS.md`, with a `CLAUDE.md` that
  imports it or a symlink between the two. It shares the rules you wrote, and
  nothing an agent learned.
- **A notes or handoff file in the repository** that both agents are told to
  read and update. It works on a small project. Then it grows, nobody prunes
  it, each agent loads all of it every time, and two agents writing it at once
  can conflict in git.
- **Pasting.** You copy one session's summary into the other. Nothing is lost
  that way, and you do it every time.

A shared store takes over from the second and third. Each fact is its own
file, the agent that learned it writes it, and a session is handed the ones
that matter.

## One store for both

With Seamless installed, both clients talk to one local daemon:

- **The same briefing.** A hook injects a `<seam-briefing>` at the start of
  every Claude Code and Codex session: the project's constraints, the plans in
  motion, what recent sessions left open, and an index of its memories.
- **The same recall.** Each prompt is matched against the store in both
  clients, and a memory that fits arrives with the prompt.
- **The same tools.** Both call `memory_write`, `recall`, and the task tools on
  one MCP surface, so either can save what the other will read.
- **The same queue.** A Claude Code agent and a Codex agent can claim steps of
  one plan without picking the same one. See
  [Coordinate multiple agents](/guides/coordinate-agents/).

Everything lands as markdown files under `~/.seamless`, which you can open,
edit, or keep in git.

## Set it up

::: when os=macos,linux

```bash
curl -fsSL https://thereisnospoon.org/install | sh
```

:::

::: when os=windows

```powershell
irm https://thereisnospoon.org/install.ps1 | iex
```

:::

The installer wires every client it finds, so there is no step per client.
Restart both once. Codex asks you to approve its hooks the first time: open
`/hooks`, read the commands, and accept them
([why](/codex-cli/#trust-the-hooks-once)). Until you do, a Codex session shows
no briefing.

Client-specific detail is on [Claude Code setup](/claude-code/) and
[Codex local setup](/codex-cli/).

## The two-minute test

In Claude Code, in any repository:

```prompt
Save this to Seamless as a constraint: never force-push main.
```

Now open Codex in the same repository:

```bash
cd ~/code/myrepo && codex
```

The session opens with a `<seam-briefing>` block, and the rule is among its
constraints. Codex has not been told anything; it was briefed. Try it the other
way round and the result is the same.

## How the two clients differ

Seamless treats them alike wherever the client allows it, and the differences
are written down:

- Claude Code runs seven hooks and Codex five. Claude Code's plan mode is
  captured into notes and a tracking task; Codex has no plan-mode capture.
- Codex caps what a hook may put in front of the model, so Seamless holds each
  of its Codex responses to 2,400 estimated tokens.
- A session's findings are harvested when a Claude Code session ends, and at
  the end of each Codex turn.

The [hooks reference](/reference/hooks/) has both tables, and
[Codex compatibility](/reference/codex-compatibility/) has what was verified
against which Codex version.

## Chats in the Claude desktop app

The same store reaches chat conversations in the Claude desktop app, so a
decision made in a chat is in the briefing of the Claude Code or Codex session
that does the work. A chat is wired differently: it has no hooks, so nothing
is injected by itself, and Claude loads a project when you ask or when a
Claude Project's instructions tell it to. [Claude app chat setup](/claude-app/)
has the steps, and
[Seamless in the Claude app](https://thereisnospoon.org/claude-app/) shows it
for work that is not code.

## Other clients

Cursor, Cline, Windsurf, Zed, and any other MCP client reach the same store
through the same tools. They have no hooks either: the agent calls
`session_start` and `recall`. See
[Connect Cursor, Cline, Windsurf & Zed](/guides/mcp-clients/).
