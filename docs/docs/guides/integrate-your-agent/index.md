# Integrate your agent

> Wire another agent into the loop - the MCP handshake, stdio bridge, session binding, scope discipline, and seam CLI fallback.

Any MCP client can be wired into Seamless: the daemon serves streamable-HTTP
MCP at `http://127.0.0.1:8081/api/mcp` behind a static bearer key, with a stdio
bridge (`seam mcp-proxy`) for clients that only speak stdio. What a client
without hooks gives up is the ambient layer - no injected briefing at session
start, no prompt-matched recall injection, no automatic harvest of findings -
so the agent has to run that loop itself.

[Claude Code](https://thereisnospoon.org/docs/claude-code/) and [Codex](https://thereisnospoon.org/docs/codex-cli/) get Seamless mostly for
free: [hooks](https://thereisnospoon.org/docs/reference/hooks/) open a session, inject a briefing, and harvest
findings without the agent deciding to, and the agent's tool calls are
[bound to that session](https://thereisnospoon.org/docs/concepts/sessions/#process-binding) without a
`session_start`. Any other client is wired up by hand. This page is that loop.

## The loop

```text
The integration loop
1 · session_start Bind scope Read the project briefing.
2 · recall Pull detail Open what the briefing summarized.
3 · work Use the context Keep the session current while acting.
4 · durable writes Save what outlives the run memory_write or notes_create
5 · session_end Hand off findings Feed the next agent's briefing.
Binding, retrieval, durable writes, and a final handoff turn an MCP connection into a useful shared session.
```

Four of those five steps are optional in the narrow sense that the tools work
without them. They are not optional in the sense that matters: **`session_start`
is what makes every later call know its scope**, and `session_end` is the only
thing that turns this run into something the next agent is told about. An agent
that skips both still works and leaves no trace - which is the same as not
integrating at all.

## Connecting

Seamless serves streamable-HTTP MCP at `/api/mcp` behind one static bearer key
([`mcp.api_key`](https://thereisnospoon.org/docs/reference/configuration/)). If your client speaks MCP over
HTTP, point it here and you are done. If you are writing the transport yourself,
the handshake is two calls.

**Client requires or prefers MCP over stdio?** Bridge it with `seam mcp-proxy`,
which speaks stdio to the client and forwards each frame to `/api/mcp`, carrying
the bearer key from config and preserving `Mcp-Session-Id` so session binding
survives:

```bash
seam mcp-proxy --config /abs/path/seamless.yaml   # invoked by the MCP client, not by hand
```

Register it the way your client registers a stdio server. Codex supports both
stdio and direct Streamable HTTP; Seamless deliberately installs the bridge with
`codex mcp add seamless -- /abs/path/seam mcp-proxy --config /abs/path/seamless.yaml`,
because this keeps the bearer key in Seamless's 0600 config. `seamlessd
install-hooks --client codex` does that for you. See [Codex local
setup](https://thereisnospoon.org/docs/codex-cli/).
The rest of this page - session binding, scope, findings - applies to direct and
bridged clients unchanged.

```bash
KEY=<mcp.api_key>

curl -sD - -X POST http://127.0.0.1:8081/api/mcp \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize",
       "params":{"protocolVersion":"2025-06-18","capabilities":{},
                 "clientInfo":{"name":"my-agent","version":"1"}}}'
```

The response body carries `serverInfo` (name and build version) plus concise
server `instructions` describing the Seamless workflow. The part your transport
must keep is a **header**: `Mcp-Session-Id: mcp-session-<uuid>`. Send it on every
subsequent request. A `tools/call` without it - or with one the daemon does not
know - is refused by the transport with `Invalid session ID` before any tool
runs.

Acknowledge the handshake, then call tools:

```bash
SID=<the Mcp-Session-Id from above>

curl -s -X POST http://127.0.0.1:8081/api/mcp \
  -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" -H "Mcp-Session-Id: $SID" \
  -d '{"jsonrpc":"2.0","method":"notifications/initialized"}'

curl -s -X POST http://127.0.0.1:8081/api/mcp \
  -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" -H "Mcp-Session-Id: $SID" \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/call",
       "params":{"name":"session_start",
                 "arguments":{"cwd":"/abs/path/to/repo"}}}'
```

Every tool returns its payload as JSON **encoded into a text content block**, not
as a JSON-RPC result object:

```json
{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"{\"session_id\":\"01K...\",\"project\":\"myrepo\",\"briefing\":\"<seam-briefing>...\"}"}]}}
```

So parse twice: once for the envelope, once for the `text`.

### Auth is enforced at the tool, not the transport

`initialize` and `tools/list` answer without a key. Only `tools/call` checks it,
and a rejected call comes back as a **successful HTTP 200 carrying a tool error**:

```json
{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"unauthorized: valid bearer key required"}],"isError":true}}
```

A client that trusts HTTP status codes will read that as a working integration
returning odd text. Check `isError` on every result. This is the same shape every
other tool failure takes - errors are tool results with a `<tool>: <reason>`
message, never transport failures - so handling `isError` once handles all of
them.

## The connection is the session binding

`session_start` binds the session to your connection, keyed by the transport's
`Mcp-Session-Id`. That binding is what lets later calls omit `project` and
`session` entirely: `memory_write`, `recall`, `tasks_claim`, and the rest read the
scope off the connection.

The binding lives in the daemon's memory, keyed by an id it minted. Two
consequences you have to design around:

- **The id encodes nothing about you** - no working directory, no client
  identity. It is an opaque UUID. The only way scope reaches the server is the
  `cwd` or `project` you passed to `session_start`, or an explicit `project`
  argument on the call.
- **It does not reliably survive a daemon restart.** Sometimes the client
  re-initializes and is minted a new id, orphaning the old binding; sometimes it
  sends the old id and the fresh process accepts it. Which one happens is a race.

So: **re-run `session_start` on reconnect**, with the same `name` to resume the
same session rather than opening a second one. Treat a sudden run of
ambiguous-scope errors as a lost binding, not as a bug in your arguments.

Or carry the session on the call. Every tool except `session_start` also takes
`session`: the `name` that `session_start` returned. A call carrying it is scoped
to that session and credited to it, whatever the connection is bound to, for
that call only. A name that matches no session is refused, never ignored. It is
also how a session will survive MCP's stateless revision (`2026-07-28`), which
has no `Mcp-Session-Id` to bind to. Seamless does not serve that revision yet: a
client that asks for it is refused with the list of supported versions and
negotiates down to `2025-11-25`, the newest revision with a handshake.

Claude Code and Codex agents are spared all of this: their connection names the
agent process, and the daemon matches it to the agent's own session on every
request, so a restart or reconnect loses nothing
([how](https://thereisnospoon.org/docs/concepts/sessions/#process-binding)). That only covers the session their
hook opened. A connection they move with `session_start` holds an explicit
binding like yours, and when that binding is lost the calls drop back to the
agent's own session.

`session_start` no longer takes `source`, so send none. The legacy values (this
guide once showed `"source":"explicit"`) are accepted and ignored; anything else
is refused.

One more `session_start` argument worth passing from a custom client: `model`,
the model id powering your agent exactly as the provider names it
(`claude-fable-5`, `gpt-5.5`). Every memory and note the session writes is
stamped with it (`model` in the frontmatter), so the store records which model
produced each piece of knowledge. Claude Code and Codex sessions get this for
free from the hooks; a bare MCP client only has attribution if it self-reports.

## Scope discipline

Scope is the thing most integrations get wrong, and the failures are quiet -
knowledge lands somewhere nobody looks. The full precedence chain is in
[Projects & scope](https://thereisnospoon.org/docs/concepts/projects/); the operational summary is short.

| Situation | What resolves |
|---|---|
| `session_start` with a `cwd` inside a mapped repo | That repo's project |
| `session_start` with a `cwd` inside an **unmapped git repo** | A project is registered automatically, named after the repository root directory |
| `session_start` with `project=<slug>` | That project - registered if it is new, with a `warning` in the result saying so; `project=global` binds the global scope |
| `session_start` with a `cwd` and a `project` that disagree | An error - pass one or the other |
| `session_start` with no `project`, and a `cwd` outside any git repo (or none) | Nothing - the session is global |
| No session, no `project` argument | The durable write is **rejected** |

The global row is the one that surprises people. A session started outside a git
repo without a `project` is global, and its writes go global - silently, because
a bound session always resolves, even to the global scope. If your agent does not
run in a repo, pass `project` to `session_start`, or on every durable write.

Writes **fail closed**. A `memory_write` with nothing to infer scope from is
rejected as ambiguous rather than landing globally, because a global memory is
seen by every agent in every repo forever and that is not something the system
should do when it is *unsure*. Two distinct errors say so:

- *no bound or ambient session to infer the project from* - nothing to inherit.
  Call `session_start` (with `cwd` or `project`), or pass `project`.
- *active ambient sessions span multiple projects* - you are unbound and other
  agents are live in several repos, so inheriting would bleed your write into
  someone else's project. Pass `project`, or bind once with `session_start`.

Pass `project: global` when you mean global. It is a token you use on purpose,
never a default you fall into.

## When to write a memory, and when to skip

The budget is real: constraints are never dropped, so everything else competes for
what is left of a briefing. A memory that does not earn its line pushes out one
that would have.

Write one when the knowledge is **durable, general, and not already discoverable**
- a constraint the project cannot violate, a trap and its symptom, a decision and
the alternatives it rejected, a belief that turned out false.

Skip it when:

| The knowledge is | Why not a memory |
|---|---|
| Already in the code | The next agent can read the code. A store that mirrors the repo is a store that goes stale silently |
| Already in `CLAUDE.md` or `AGENTS.md` | It is injected anyway; a memory saying it again just spends budget twice |
| A narration of what you just did | That is `session_end` findings, or a note |
| A long artifact | That is a note - found via [recall](https://thereisnospoon.org/docs/concepts/recall/), not injected |

If it is durable but you cannot compress it to one line, it is a note with a
memory pointing at it. See [Write memories that get
recalled](https://thereisnospoon.org/docs/guides/write-good-memories/).

## A2A: recall without a session

An agent that speaks [A2A](https://a2a-protocol.org/) instead of MCP gets
recall over the same daemon and the same key. The live agent card is at
`http://127.0.0.1:8081/.well-known/agent-card.json` (this site publishes a
twin at the same path), the endpoint is `/api/a2a` (JSON-RPC), and the one
skill is `recall`: `message/send` with text parts as the query returns a
completed message - a text summary plus a data part carrying the same
`{"hits": [...]}` payload the MCP recall tool returns.

```bash
curl -s -X POST http://127.0.0.1:8081/api/a2a \
  -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"message/send",
       "params":{"message":{"kind":"message","messageId":"m1","role":"user",
                 "parts":[{"kind":"text","text":"why is the console theme split"}],
                 "metadata":{"project":"seamless","limit":5}}}}'
```

There is no session binding on this surface: scope comes entirely from message
metadata. `metadata.project` widens the search from global-only to a project
plus global, `metadata.scope` narrows the kinds (`all|memories|notes`), and
`metadata.limit` caps the hits. The rest of A2A - tasks, streaming, push
notifications - is deliberately absent, and the card declares every one of
those capabilities false rather than advertising an operation that would fail.

## Skills over the web

The maintained skills that `seamlessd install-hooks` delivers to a client's
skill home ([`seam-onboard`](https://thereisnospoon.org/docs/install/), [`seam-research`](https://thereisnospoon.org/docs/guides/research-trials/))
are also published for discovery: this site serves an
[Agent Skills](https://agentskills.io/) index at
[`/.well-known/agent-skills/index.json`](https://thereisnospoon.org/.well-known/agent-skills/index.json),
one entry per skill with a `sha256:` digest of its `SKILL.md`. The published
artifacts are the same embedded bytes the installer writes, so a fetched skill
verifies against its digest and matches what a local install would have
delivered.

## The seam CLI as a fallback client

An agent that can run a shell but cannot speak MCP still has the loop. `seam` is
a headless client for the same daemon, authenticating with the same key:

```bash
seam prime --cwd /abs/path/to/repo        # session_start; prints the briefing
seam recall "why is the console theme split"
seam memory read lease-steal-window       # memory_read; note read is notes_read
seam remember --name lease-steal-window --kind gotcha \
  --description "..." --body "..."        # or pipe the body on stdin
seam ready                                # the actionable queue
seam task claim --lease 1800 01K7ABCD     # flags on either side of the id
seam call session_end '{"findings":"..."}' # any tool by name, JSON in and out
```

Three things to know before you script it.

**Flags go on either side of a positional.** Every `seam` command parses flags
and positionals in any order, so `seam task claim 01K7ABCD --lease 1800` and
`seam task claim --lease 1800 01K7ABCD` are the same line. A typo'd flag is an
error rather than a silently different command. See the
[seam CLI reference](https://thereisnospoon.org/docs/reference/cli-seam/).

**Every read command takes `--json`, and `seam call` reaches the rest.** `--json`
prints the daemon's own answer byte for byte instead of the text rendering, and
`seam call <tool> '<json-args>'` calls any MCP tool by name and prints its
result, so a large or filtered read is a pipe into `jq` - through the same
project fences and limits as the MCP tools. That is also the one thing the CLI
has over an MCP client, which caps how much of a result its agent sees. See
[JSON output](https://thereisnospoon.org/docs/reference/cli-seam/#json-output) and
[seam call](https://thereisnospoon.org/docs/reference/cli-seam/#seam_call).

```bash
seam memory list --kind convention --limit 100 --json | jq -r '.hits[].name'
seam call tasks_list '{"status":"open","plan":"lease-rework"}' | jq '.tasks[] | {id, title}'
```

**Findings go through `session_end`.** There is no `seam session-end` verb, and
`seam prime` does not close what it opens: an agent driving Seamless through
`seam` hands off with `seam call session_end '{"findings":"..."}'`, adding
`"session":"<name or id>"` (`seam prime` prints the id on stderr) when the call
is not bound to its session - a terminal, or a client without hooks. Skip it
and the session is only ever closed by the idle reaper, contributing nothing to
the next briefing.

### Optional: tell your agent where to read from

Agents with a shell sometimes write scripts over the files under the data
directory (`~/.seamless`) to search or filter. Those reads skip the project
fences, miss everything that lives only in the database - tasks, sessions,
trials - and stop working under [peer_auth](https://thereisnospoon.org/docs/guides/peer-auth/), where the data
directory belongs to the daemon's account. Nothing in Seamless tells an agent
this on its own; if you want it said, paste a line like this into your agent's
instructions (`CLAUDE.md`, `AGENTS.md`, or your client's equivalent):

```markdown
Use the Seamless tools, or `seam ... --json | jq` for large or filtered reads;
never read files under the data dir.
```

## Verify the integration

```bash
seam doctor        # /healthz, key acceptance, tools/list count, project_list
```

A green `mcp_tools` line proves the endpoint answers *and* your key works, which
is most of what an integration can get wrong. `seamlessd doctor` covers the other
half - config, database, embedder, hooks - on the server side. If something is
wrong and nothing is complaining, start at
[Troubleshooting](https://thereisnospoon.org/docs/guides/troubleshooting/): the hooks fail open, so silence is
the failure mode.
