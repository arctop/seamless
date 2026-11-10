# Quickstart

> Install Seamless with one command, start Claude Code or Codex in a repo, and confirm the first briefing lands.

This is the one happy path: install, start a session, and watch it open with a
briefing. It is one command and a check - everything between them is automatic.
Every fork in the road is a link, not a branch in these steps.

## Install

**macOS · Linux:**

```bash
curl -fsSL https://thereisnospoon.org/install | sh
```


**Windows:**

The same install, in PowerShell:

```powershell
irm https://thereisnospoon.org/install.ps1 | iex
```



One command does the lot: it fetches the checksum-verified release archive for
your platform (macOS, Linux, and Windows; amd64 and arm64), installs `seamlessd`
and `seam`, generates the bearer key, wires the detected clients - Claude Code,
the Claude app chat surface, Codex - with hooks, MCP, and skills, and starts
the daemon as a per-user service on `127.0.0.1:8081` with data in `~/.seamless`.
No Go, no CGO toolchain, no database server, no Node; the installer needs
`curl` and `tar`. [Install & deploy](https://thereisnospoon.org/docs/install/) has the override knobs and the
other routes (Homebrew, a clone, `go install`, prebuilt archives);
[The service & where things live](https://thereisnospoon.org/docs/reference/service/) has the service
controls, logs, and every path.

Seamless is early in its development cycle, and releases with improvements and
bug fixes land often. The installer's install keeps itself current: from v0.7.0
it installs each new release by itself, a day after it is published and at a
moment no agent is using it. To update right now, `seamlessd update` is the one
command, on every OS. See [Update & uninstall](https://thereisnospoon.org/docs/updating/#automatic-updates).

Piping a stranger's script into a shell deserves a read first - it is
[one file](https://thereisnospoon.org/install).

On a true first run - no config file anywhere - the bearer key is generated and
written to `~/.config/seamless/seamless.yaml`. Nothing to copy, nothing to
paste. No LLM key is required either: without one, recall degrades to plain
full-text search. Add OpenAI or Ollama in the
[configuration](https://thereisnospoon.org/docs/reference/configuration/) when you want semantic recall.

`seamlessd doctor` is the checkpoint: it validates the config, reports the
database and its schema without changing either, and asserts the tool count. If
it is green, the daemon will start.

## Now open your client

The installer already wired the clients it found, so there is no second setup
step.

**Claude Code:**

Start Claude Code in any git repo - restart it first if it was already
running, so it reloads hooks, MCP, and skills:

```bash
cd ~/code/myrepo && claude
```

The same setup covers code sessions inside the Claude desktop app - they
share `~/.claude`. See [Claude Code setup](https://thereisnospoon.org/docs/claude-code/).


**Codex:**

Start Codex in any git repo - restart it first if it was already running, so
it reloads hooks, MCP, and skills:

```bash
cd ~/code/myrepo && codex
```

Codex asks you to approve the hooks once: open `/hooks`, review the exact
Seamless commands, and accept them - until then, no briefing appears
([why](https://thereisnospoon.org/docs/codex-cli/#trust-the-hooks-once)). See
[Codex local setup](https://thereisnospoon.org/docs/codex-cli/).


**Claude app chat:**

Restart the Claude app once - it reads its config only at startup. A chat has
no hooks and no working directory, so have it run `session_start` with
`project=<slug>` - or with the repo's absolute path as `cwd`. See
[Claude app chat setup](https://thereisnospoon.org/docs/claude-app/).


The first session inside a git repo also registers it: the SessionStart hook
resolves your cwd to its git root, derives a project slug from that
directory's name, and records the mapping - so agents inherit project scope
without passing `project` on every call, and without you creating a project
first. See [Projects & scope](https://thereisnospoon.org/docs/concepts/projects/) for the precedence chain
and the `seamlessd map-repo` override.

## Confirm it works

That session's context now opens with an injected briefing:

```text
Injected context SessionStart
<seam-briefing>
Seam project: seamless -- 58 memories (4 constraints), 3 recent findings.
Constraints (binding for every session):
- errcheck-check-blank-two-category-rule: errcheck runs with check-blank ...
...
</seam-briefing>
Seeing this envelope proves the hook resolved the repository and delivered a budgeted briefing.
```

If you see that block, the loop is closed: the hook resolved your cwd to a
project, the daemon assembled a briefing within its token budget, and the agent
started with your knowledge already in context.

Open the console to watch it happen:

```bash
seamlessd console-open                               # opens pre-authenticated
```

It starts with the essentials; the card on its Home lets you pick how much of it
you want to see.

## Next steps

- Try the [first prompts](https://thereisnospoon.org/docs/first-prompts/): plain requests that save a rule, a
  note, or a plan, pull in what past sessions learned, and hand off at the end.
- Run `/seam-onboard` in Claude Code or `$seam-onboard` in Codex once - it
  shows the Seamless-awareness block it can add to global or project
  instructions (`CLAUDE.md` or `AGENTS.md`) and edits only with your approval.
- Wire a client the installer did not find:
  [Add or remove one client](https://thereisnospoon.org/docs/updating/#add-or-remove-one-client).
- Look up any tool, key, or command in the [Reference](https://thereisnospoon.org/docs/reference/).
