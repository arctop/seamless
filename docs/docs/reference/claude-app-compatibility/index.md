# Claude app compatibility matrix

> Versioned evidence for the Claude app's two surfaces - embedded code sessions (hooks, MCP, runtime skew) and the chat surface's stdio MCP bridge.

This matrix records what Seamless has **observed** in the Claude desktop app,
and what remains unverified. It is an evidence ledger, not a support claim: a
row says "this was seen working on this build, on this platform, on this date",
and its absence says exactly that. The app has two distinct surfaces -
[embedded code sessions](https://thereisnospoon.org/docs/claude-code/#the-claude-apps-code-surface) (real
Claude Code sharing `~/.claude`) and the [chat surface](https://thereisnospoon.org/docs/claude-app/) (a
hookless MCP client wired through `claude_desktop_config.json`) - and evidence
for one is never evidence for the other.

## Code surface (embedded Claude Code sessions)

| App runtime / platform | Hooks observed | MCP | Sessions |
|---|---|---|---|
| Bundled Claude Code 2.1.215 (PATH CLI 2.1.216 on the same machine) / Darwin arm64, 2026-07-21 | SessionStart briefing injected in 8/8 app sessions, correctly project-scoped (including global for cwd-less helper sessions); PostToolUse tool-call events recorded; SessionEnd auto-harvest fired on app close and completed the sessions with genuine findings. **UserPromptSubmit never fired** - no per-prompt recall on this surface | The shared user-scope `~/.claude.json` registration (http + `headersHelper`) worked unchanged from an app code session: a `notes_create` round trip landed a real note | Repo cwd resolved to the mapped project; ambient `cc/...` sessions behaved exactly as CLI sessions |

The UserPromptSubmit row is not a Seamless configuration problem: a firing
hook always leaves a trace (an injection or a prompt event), and none exists
for any of the eight observed app sessions. It was also the one http hook that
install wrote (against a plain-http daemon); every command hook fired. Seamless
has since made UserPromptSubmit a command hook like the rest
(`seam hook user-prompt-submit`), which is the shape the app was seen to run.
That it fires in an app code session in that shape is expected from this
evidence but not yet observed, so it stays on the list below until a live app
session shows it.

Managed worktrees did **not** materialize during this capture (the app ran its
code sessions directly in the repo root), so the worktree-to-project mapping -
a linked worktree resolves through its git common directory to the main
checkout's project - is verified by unit tests against real worktree layouts,
not yet by a live app session inside one.

## Chat surface (stdio MCP bridge)

| Bridge / daemon / platform | Protocol evidence | Tool round trips | App evidence |
|---|---|---|---|
| `seam mcp-proxy` (stdio) / Seamless 0.3.8+a189f0d / Darwin arm64, 2026-07-21 | Driven over stdio exactly as `claude_desktop_config.json` instructs the app to (newline-delimited JSON-RPC, protocol 2025-06-18): initialize handshake with server instructions delivered; capabilities are tools-only by design; `tools/list` matched the daemon's authoritative tool count | `memory_read` and a full `notes_create` round trip with explicit `project` (plan tag promoted, session model stamped, `created-by:agent` auto-tag, clean delete); session persistence across calls on one stdio connection - `session_end` with no arguments closed the exact session `session_start` had bound | A fresh app launch spawned two `seam mcp-proxy` instances, both holding established TCP connections to `127.0.0.1:8081` - process and socket evidence that this app build loads the registration, covering the "loaded state unverifiable" caveat `doctor` honestly reports. A conversation typed in the app UI invoked `memory_read` and got the memory back |

One caveat keeps the in-UI row narrow: on the test machine the code surface was
also installed, and the confirming conversation *also* fired a SessionStart
hook, so the daemon's event log cannot attribute that one tool call to the chat
stdio bridge rather than the code surface's HTTP registration. The chat stdio
protocol path is independently verified end to end; only the final in-UI hop
rests on owner confirmation plus process evidence.

The same capture probed the scope edges the
[chat setup page](https://thereisnospoon.org/docs/claude-app/#scope-discipline-in-a-chat) warns about, and
both quiet outcomes are real observations, not theory: a sessionless unscoped
durable write was **not** rejected while an ambient CLI session was live - it
landed in that session's project with that session's provenance - and a
cwd-less `session_start` bound the global scope, after which unscoped writes
landed global with no write-time flag. Passing a repo `cwd` delivered the full
project briefing and correctly scoped every later unscoped call.

## What remains unverified

- **Windows, entirely.** Detection there is the config file's existence (there
  is no documented install location to probe), and no live Windows run of
  either surface has been recorded.
- **Whether a running app has loaded the registration**, as a queryable fact.
  The app reads its config at startup and exposes no way to ask; the process
  and socket evidence above covers one observed build, and `doctor` keeps
  reporting the general case as unverifiable.
- **An app-only machine.** The tested machine had the PATH CLI and the code
  surface installed; chat-surface setup with nothing else present is untested.
- **The command UserPromptSubmit hook in an app code session**, per above.
- **A live app code session inside a managed worktree**, per above.
- **The chat surface since the agent-process binding.**
  `session_start project=<slug>`, and a sessionless-write fallback that skips
  sessions other agents' processes own, both postdate the capture above;
  neither has been observed in a live app conversation.

## Re-verifying after an app update

The chat surface's protocol path can be re-checked without the app: run the
installed `seam mcp-proxy` over stdio, speak newline-delimited JSON-RPC
(initialize, `notifications/initialized`, then tool calls), and compare against
the rows above - the [integration guide](https://thereisnospoon.org/docs/guides/integrate-your-agent/) shows
the same handshake over plain HTTP. For the app itself: restart it, confirm
`seam mcp-proxy` child processes with established connections to the daemon,
and have a chat conversation call `memory_read`. For the code surface: open an
app code session in a mapped repo, look for the `<seam-briefing>` block, then
prompt again and check whether recall injection ever appears - that is the
UserPromptSubmit re-test (after `seamlessd install-hooks`, so the hook is the
command form). `seamlessd doctor` reports the bundled app runtime
and the PATH CLI separately precisely so version skew between them is visible
while you do this.
