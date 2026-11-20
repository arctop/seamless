---
title: First prompts to try
description: Requests to paste into Claude Code, Codex, or a Claude app chat right after installing - rules, notes, plans, imported history, recall, and handoffs.
---

You never call a Seamless tool yourself. You ask in plain words and the agent
picks the tool. These are requests worth trying in your first few sessions, each
with what the agent does and where the result ends up. Change the details to
your own work.

Two habits make them land:

- **Say "Seamless" when the request could go either way.** Claude Code and the
  Claude app have memory features of their own, and a bare "remember this" may
  go there instead. "Save this to Seamless as a constraint" cannot.
- **In a Claude app chat, name the project.** A chat has no hooks, so nothing is
  loaded until you ask (see [the Claude app guide](/claude-app/)). In Claude
  Code and Codex, the repo you start in is the project.

Memories and notes the agent saves are files under `~/.seamless`: memories in
`memory/<project>/`, notes in `notes/<project>/`. Tasks and session findings are
rows in the database beside them, `seam.db`. `seamlessd console-open` shows all
of it in the browser.

## Remember a rule

::: when client=claude,codex

```prompt
Save this to Seamless as a constraint: migrations run before the deploy, never after.
```

:::

::: when client=claude-desktop

```prompt
Save this to Harbor as a rule: drafts go to the other side as PDFs, never as Word files.
```

:::

The agent calls `memory_write` with `kind: constraint`. A constraint is pinned
into every briefing for its project, so the next session reads it before your
first prompt. To check, start a fresh session and look for the rule among the
constraints in its `<seam-briefing>` block, or in the app, load the project in a
new chat and ask what its rules are.

Other kinds work the same way: say "decision", "gotcha", "runbook", or
"refuted" and the agent files it under that kind. [Memory &
notes](/concepts/memory/) describes each one, and [Write memories that get
recalled](/guides/write-good-memories/) covers what makes one worth keeping.

## Save a note

::: when client=claude,codex

```prompt
Save what we just worked out about webhook retries as a Seamless note.
```

:::

::: when client=claude-desktop

```prompt
Save a summary of this call as a note in Harbor, dated today.
```

:::

The agent calls `notes_create`. Notes hold the long version of something: a
design, a call summary, a page of research. Memories hold the one line that
should change what the next session does. `recall` searches both, so a later
question about retries finds the note next to any memories about them.

To keep a web page, hand the agent the link:

```prompt
Save this page to Seamless as a note: https://example.com/the-article
```

`capture_url` fetches the page, stores its readable text as a note, and records
where it came from.

## Plan work as tasks

::: when client=claude,codex

```prompt
Make a Seamless plan with tasks: move sessions to Redis, then migrate the existing sessions, then update the deploy runbook.
```

:::

::: when client=claude-desktop

```prompt
Make a plan for closing Harbor, with tasks in order: the KYC list, then the disclosure schedule, then the signing call.
```

:::

A plan is a note plus tasks that share a `plan:<slug>` tag. The agent writes the
narrative with `notes_create`, adds each step with `tasks_add`, and chains the
steps with `depends_on`, so a step becomes ready only when the one before it is
done. It tells you the slug it picked; that is how you come back to the plan.

Later, in any session:

::: when client=claude,codex

```prompt
Pick up the next step of the sessions-to-redis plan.
```

:::

::: when client=claude-desktop

```prompt
What is next on Harbor?
```

:::

Picking up a step means `tasks_claim`, which holds it under a lease, so a second
agent asking the same thing is told who has it instead of doing the work twice.
Until the plan is finished, every briefing for the project shows it as a
`PLAN:` line with its progress. [Tasks & plans](/concepts/tasks-and-plans/)
explains the queue, and [Coordinate agents](/guides/coordinate-agents/) covers
running several at once.

::: when client=claude

Claude Code's plan mode feeds the same thing: approve a plan there and it
becomes a tracked plan without asking. See [Plan mode](/guides/plan-mode/).

:::

## Bring in what you already know

A new store is empty, and the quickest way to fill it is from history you
already have. Seamless has no importer for this. The agent reads the history
with its own tools and saves what you approve. Ask for a list first: a bulk
import fills the store with entries nobody recalls, and each of them competes
for a place in the briefing.

::: when client=claude

```prompt
Go through my past Claude Code sessions for this repo in ~/.claude/projects and list the decisions and gotchas worth keeping. Save the ones I pick to Seamless.
```

Claude Code keeps each repo's transcripts as JSONL files in a folder under
`~/.claude/projects/` named after the repo's path.

:::

::: when client=codex

```prompt
Go through my past Codex sessions for this repo in ~/.codex/sessions and list the decisions and gotchas worth keeping. Save the ones I pick to Seamless.
```

Codex keeps the transcripts for every repo together under `~/.codex/sessions/`,
filed by date, so the agent picks out this repo's by working directory.

:::

::: when client=claude,codex

The repo's own history works too:

```prompt
Read the last three months of git log and list the decisions a new teammate could not get from the code. Save the ones I pick to Seamless.
```

Skip anything `CLAUDE.md` or `AGENTS.md` already says. Those files load into
every session anyway, and a memory that repeats them only takes up room.

:::

::: when client=claude-desktop

```prompt
Search my past chats about Harbor and list the decisions we made, with dates. Then save the ones I pick to Seamless.
```

Searching past chats is a Claude app feature, separate from Seamless, and it
only works when it is turned on in Claude's settings. Without it, paste an old
conversation into the chat and ask Claude to pull the decisions out of it.

:::

## Ask what is already known

::: when client=claude,codex

```prompt
What do we know about the rate limiter?
```

In Claude Code and Codex you often won't need to ask. Each prompt is matched
against the store, and the memories that fit arrive with it in a
`<seam-recall>` block.

:::

::: when client=claude-desktop

```prompt
What do we have on the indemnity cap in Harbor?
```

A chat has no hook that matches your prompts against the store, so in the app
you ask.

:::

The agent calls `recall`, one search over memories, notes, tasks, research
trials, and what past sessions reported. [Recall](/concepts/recall/) explains
how the results are ranked.

## Change your mind

::: when client=claude,codex

```prompt
We moved background jobs from Redis to SQS. Save that to Seamless as replacing the old decision.
```

:::

::: when client=claude-desktop

```prompt
The client agreed to a 1x cap. Save that to Harbor as replacing the 2x decision.
```

:::

The agent writes the new memory with `supersedes` naming the old one. The old
memory leaves briefings and search but keeps its file, marked `superseded_by`
the new one, so "what was it before it changed?" still has an answer:
`memory_read` on the old name returns it with a note saying what replaced it.
[Memory supersession](/concepts/memory-supersession/) has the details.

## Hand off at the end

::: when client=claude,codex

```prompt
Wrap up for today: record what is done and what is left for the next session.
```

If you forget, the session-end hook keeps the agent's last message as the
session's finding instead, marked "(auto-harvested)".

:::

::: when client=claude-desktop

```prompt
Before I go, save where we left off on Harbor.
```

A chat has no session-end hook, so in the app this request is the only way a
summary gets written.

:::

The agent ends its session with `session_end` and a short findings summary.
The next briefing for the project lists it under recent findings, whichever
agent or chat opens it.

## More to try

- `Star the deploy-runbook memory in Seamless.` `favorite_set` pins a starred
  memory in its project's briefings: a runbook like this one heads the Memories
  list, and a starred constraint or convention heads its own section. No trim
  or budget drops it.
- `What does Seamless have on this project?` A quick inventory, by kind.
- `Ask the Seamless gardener to find duplicate memories about deploys.`
  `gardener_request` turns the request into proposals that wait for your
  approval in the console. It needs an LLM configured
  ([Configuration](/reference/configuration/)); see [the
  gardener](/concepts/gardener/) for what it can propose.
