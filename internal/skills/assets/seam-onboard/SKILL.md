---
name: seam-onboard
description: "Install Seamless awareness for the current agent client. Use only when the user explicitly asks to onboard Seamless or invokes this skill. Configure the client's hooks and MCP server if needed, ask whether guidance belongs globally or in this project, append a marker-wrapped block to AGENTS.md for Codex or CLAUDE.md for Claude Code, then self-remove."
disable-model-invocation: true
---

# Onboard Seamless

Install concise Seamless awareness for the agent client running this skill. Do
not edit an instruction file until the user chooses the target and sees what
will be added.

## 1. Resolve the current client

Use the client you are currently running in, not whichever client directories
happen to exist on disk:

| Client | Install profile | Global instructions | Project instructions | Invocation | Skill directory |
| --- | --- | --- | --- | --- | --- |
| Codex | `codex` | `$CODEX_HOME/AGENTS.md` when set; otherwise `~/.codex/AGENTS.md` | `./AGENTS.md` | `$seam-onboard` | `$CODEX_HOME/skills/seam-onboard` when set; otherwise `~/.codex/skills/seam-onboard` |
| Claude Code | `claude` | `$HOME/.claude/CLAUDE.md` | `./CLAUDE.md` | `/seam-onboard` | `$HOME/.claude/skills/seam-onboard` |

If the runtime does not make the current client clear, ask the user which of
these two clients is running. Do not infer it from the presence of both homes.

## 2. Confirm the integration

Check the selected client's registration:

- Codex: `codex mcp get seamless`
- Claude Code: `claude mcp get seamless`

If it is absent, explain that the next command installs that client's Seamless
hooks, MCP registration, and maintained skills, then run:

```text
seamlessd install-hooks --client <profile>
```

Use `codex` or `claude` for `<profile>`. Stop and surface the error if the
command fails. Never discover, print, paste, or place the bearer key in a client
command: the installer uses `seam mcp-proxy` for Codex and `seam mcp-headers`
for Claude Code so the key stays in Seamless's config.

For Claude Code, inspect `claude mcp get seamless`. If it reports a project or
local registration instead of user scope, tell the user it must be migrated,
run `claude mcp remove seamless`, then run the install command above with the
`claude` profile. Codex's registration is already user-level in its config.

## 3. Inspect and ask for the target

Before asking, inspect both candidate instruction files for the current client.
Report whether each exists and whether it already contains
`<!-- seam-onboard:start -->`. Explain that you will append one marker-wrapped
section describing when to use shared memories, notes, sessions, plans, and
tasks; no other instruction text will change.

Then ask exactly one location question and wait:

> Where should I install Seamless awareness?
>
> 1. **Global** — the current client's global instruction file. Every session in every project will know about Seamless.
> 2. **This project** — the current client's instruction file in this working directory. Only this project.

Use the concrete path from the table when asking. This choice authorizes adding
the described block to that path; it does not authorize replacing unrelated
content.

## 4. Prepare the file safely

- For a missing global file, create its parent directory and an empty file.
- For a missing project file, report `pwd` and ask whether to create the file
  there. Stop without writing if the user declines.
- If the target already contains the start marker, explain that awareness is
  already installed and ask whether to replace the existing marked block. If
  confirmed, remove only the content from the start marker through the matching
  end marker. If no matching end marker exists, stop and report the malformed
  block instead of guessing at a deletion boundary.

## 5. Write the awareness block

Append this exact block. Preserve a blank line before the start marker when the
file was non-empty.

```markdown
<!-- seam-onboard:start -->
## Seamless (shared agent knowledge)

Seamless is a local-first knowledge and coordination system shared by the user's agents. Its MCP tools provide durable memories, long-form notes, sessions and findings, dependency-aware tasks, plan composition, and research trials. Treat writes like a shared team wiki: make them specific, dated when time matters, and useful to a future agent that lacks this conversation.

Use Seamless for non-trivial work that should outlive this conversation or cross between agents. Skip it for trivial edits, throwaway work, facts already in the codebase or current conversation, and instructions already present in AGENTS.md or CLAUDE.md.

Ambient hooks inject a `<seam-briefing>` at session start and prompt-matched `<seam-recall>` blocks. Read injected context before searching again. Call `recall` before asking the user to re-find stored knowledge; use `memory_read` to load a full memory.

- Use `memory_write` for compact durable knowledge: constraints, conventions, decisions, gotchas, protocols, runbooks, references, refuted beliefs, and stage state. A constraint is what any agent must or must not do regardless of task; a convention is a project-local choice or layout fact (naming, branding, where things live or deploy). Use `notes_create` for long artifacts such as research, meeting summaries, and design narratives. Do not copy a long note into memory.
- For `kind=stage` memories, open the body with `Status: open|in_progress|blocked|done` and optionally `Gate: human|ai`; flip the status with `memory_edit` or a full `memory_write` (append cannot change the header, which is parsed from the top of the body). A task is work someone here can claim and finish; a stage is a state of the world you wait on that every session must know while it holds.
- To fix part of a memory or note, use `memory_edit` / `notes_edit` rather than resending the whole body: they take exact `{old_string, new_string}` edits, refuse an ambiguous or missing match, and apply all-or-nothing. Reserve them for corrections that do not change what the item CLAIMS -- typos, formatting, a stale path, a `Status` flip, metadata. When the MEANING changes, write the new knowledge with `memory_write` `supersedes` so the outdated memory retires into readable history instead of being silently rewritten. `memory_read`/`notes_read` return a `content_hash` you can pass back as `expect_hash` to have a stale edit refused rather than overwrite someone else's.
- Scope normally comes from the bound or ambient session. If it is ambiguous, pass `project` explicitly. An unknown slug creates that project. Use `project=global` only for genuinely cross-project knowledge because global memories enter every project's briefing.
- Claude Code and Codex hooks start your session and bind your Seamless tool calls to it, so do not call `session_start` there unless you need to move this conversation to another project (`session_start project=<slug>`). From a client without hooks, call `session_start` once with `cwd` or `project`. Close with `session_end` and concise findings when the work needs an explicit handoff. If a call reports an ambiguous session or agent, pass `session=` with the `cc/...` or `cx/...` name from your briefing's `Seam session` line.
- If the session caused any mishaps (an action a warning said not to take, live state touched by mistake, a command that hit the wrong target), self-report them in `session_end`'s `mishaps` field, one short entry per incident, even when fully recovered. Begin each entry with its type and a colon (shell-trap, wrong-target, live-state, ignored-warning, fabricated or other), as in `shell-trap: a bare == in zsh died`, and name a memory it violated by its exact slug. They are recorded for recurrence review, not blame.
- Compose a plan from a narrative note and tasks sharing `plan:<slug>`. Claim a ready step with `tasks_claim` before working it; heartbeat or release the lease as appropriate.
- Use the research lab tools for repeated experiments whose expected and actual outcomes must be compared across agents.

Trust each tool's `inputSchema` required fields and enums over prose when building a call. Never invent a fallback for a present-but-invalid argument.
<!-- seam-onboard:end -->
```

## 6. Confirm and self-remove

Report the absolute path, whether the block was added or replaced, and the
number of lines added. Then remove only this client's `seam-onboard` skill
directory from the table in step 1. Use the exact resolved path; do not use a
broad glob or remove the sibling `seam-research` skill.

Tell the user:

> Seamless onboarding is complete. The one-shot onboarding skill removed itself. To install it again from a Seamless checkout, run `make install-onboard-skill CLIENT=<profile>` (`codex` or `claude`).
