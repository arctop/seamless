# Configuration

> Every configuration key, its type and default, plus the annotated example file and the four layers that resolve them.

Seamless reads a single YAML file. Every key also has a `SEAMLESS_*` environment
override.

## Where the config comes from

The file is looked up in this order, first hit wins:

1. `$SEAMLESS_CONFIG`
2. `~/.config/seamless/seamless.yaml`
3. `./seamless.yaml`

## Precedence

Four layers resolve each key. Later layers win:

1. **Defaults** - the built-in values in the table below.
2. **File** - whatever the YAML sets.
3. **Environment** - `SEAMLESS_*` overrides the file.
4. **Runtime override (DB)** - the console's Settings forms store some blocks in
   the database. They win over file *and* env, take effect without a daemon
   restart, and stay until reset.

That fourth layer covers three blocks where the stored row simply wins:

| Block | Written by | When it takes effect |
|---|---|---|
| `briefing:` | Settings → [Briefing](https://thereisnospoon.org/docs/reference/console/#briefing) | From the next session start. |
| `features:` | Settings → [Features](https://thereisnospoon.org/docs/reference/console/#optional-features) | Immediately in the console; an agent sees it from its next session (tool lists and briefings alike). |
| `console.level` | Settings → [Experience](https://thereisnospoon.org/docs/reference/console/#experience), or the Home welcome card | Immediately, in the console only. |

The `update:` block's two switches, `update.check` and `update.auto`, are merged
the other way round: the **more restrictive** value wins. Settings →
[Updates](https://thereisnospoon.org/docs/reference/console/#updates) can always turn either off, and can turn
it on when file and env do not say `false` - but an explicit `check: false` or
`auto: false` in the file (or `SEAMLESS_UPDATE_CHECK=false`,
`SEAMLESS_UPDATE_AUTO=false`) is final, and the console shows that switch locked
with where it was set. `check: false` promises no update traffic at all, so it
is never one click from untrue, and with checks off nothing installs by itself
either. See [the update block](#the-update-check).

It exists so you can change what agents get injected - and what they can reach -
while they are running, and it is the one place where the config file is not the
last word: check the console before concluding a `briefing:`, `features:`, or
`console.level` setting is being ignored. No form ever writes your config file,
and **Reset** on each clears the stored row and hands that block back to
file/env.

Overrides can be in force without you having set them. Upgrading an
installation that already holds trial data seeds a stored `features:` override
with research on, so a feature that now ships off does not disappear from under
data you were already using. The console labels it a stored override, not your
choice; **Reset** clears it like any other.

### The console level

`console.level` (`SEAMLESS_CONSOLE_LEVEL`) is how much of the
[console](https://thereisnospoon.org/docs/reference/console/#choose-how-much-you-see) you see: `basic`,
`standard`, or `advanced`. It is presentation only - briefings, MCP tools,
hooks, recall, and the gardener are identical at every level - and a screen a
level leaves out of the sidebar still opens from a link. A value outside those
three stops the daemon at startup with an error naming them; an empty value
means the default, `basic`.

A fresh installation starts at `basic`. Upgrading an installation that had
already recorded sessions stores `advanced` once, as the same kind of runtime
override (labeled "set by the upgrade" in Settings → Experience), so an upgrade
never hides a screen you were using. Like any override, it wins over this key
until **Reset** hands the level back to file/env.

### The update check {#the-update-check}

The `update:` block drives the daemon's
[automatic update checks](https://thereisnospoon.org/docs/updating/#automatic-update-checks) and
[automatic updates](https://thereisnospoon.org/docs/updating/#automatic-updates):

- `check` (`SEAMLESS_UPDATE_CHECK`) is optional. Left unset, a release build
  checks and a build from source does not; `true` or `false` decides either
  way. `false` means no request to GitHub at all, and so no automatic update
  either.
- `check_interval` (`SEAMLESS_UPDATE_CHECK_INTERVAL`) is the time between
  checks: a Go duration from `1h` to `720h`, default `6h`. Write the unit - a
  bare number other than `0` is refused rather than guessed at, since `6` could
  mean six seconds or six hours.
- `auto` (`SEAMLESS_UPDATE_AUTO`) is optional, and unset means on: an install
  made by the curl or PowerShell installer installs a newer release by itself.
  Homebrew, builds from source, client machines and layouts the daemon cannot
  vouch for are only told, whatever `auto` says
  ([who updates itself](https://thereisnospoon.org/docs/updating/#who-updates-itself)). `false` keeps the
  check and its notices and leaves installing to you (`seamlessd update`, or
  **Update now** in the console). Like `check: false`, it is final, and the
  console cannot turn it back on. With checks off, `auto` is off too.
- `max_defer` (`SEAMLESS_UPDATE_MAX_DEFER`) is how long a pending automatic
  update waits for a quiet daemon - no live agent session, no request in
  flight - before it takes the next lull in requests instead: a Go duration from
  `1m` to `720h`, default `24h`. `0` is refused. After 1.5 times `max_defer` it
  installs as soon as no request is in flight, and after twice `max_defer`
  whatever is in flight ([when it installs](https://thereisnospoon.org/docs/updating/#when-it-installs)).
- `min_age` (`SEAMLESS_UPDATE_MIN_AGE`) is the soak: how long a release must have
  been published before an automatic update takes it, measured on GitHub's
  clock. A Go duration from `0` (no wait) to `720h`, default `24h`. Installing by
  hand ignores it.

Set `auto`, `max_defer` and `min_age` in the file, not in the daemon's
environment. Any `SEAMLESS_*` variable besides `SEAMLESS_CONFIG` in the
daemon's environment makes the install notify-only - the installer rewrites
the service without it - so `SEAMLESS_UPDATE_AUTO=true` can never turn
automatic updates on, and the two durations set there switch them off rather
than tune them.

All five keys are new in v0.7.0. seamlessd releases before it refuse config
keys they do not know, so a file that sets one runs only with v0.7.0 or newer.
The example file spells out the keys whose default has a value and leaves
`check` and `auto`, whose default is unset, commented out.

### Peer-user authentication {#peer-auth}

The `peer_auth:` block turns on a Unix-socket listener whose callers the daemon
identifies by the uid the kernel reports, and maps OS accounts to the projects
they may reach. It is off by default, and off is exactly the behavior of every
earlier release. The guide is [Seal projects to OS accounts](https://thereisnospoon.org/docs/guides/peer-auth/);
in short:

- `enabled` (`SEAMLESS_PEER_AUTH_ENABLED`) switches it on.
- `owner` (`SEAMLESS_PEER_AUTH_OWNER`) is the OS account - a user name or a
  numeric uid - the TCP bearer key stands for, and whose socket connections act
  as the owner. Required when enabled.
- `socket.path` (`SEAMLESS_PEER_AUTH_SOCKET_PATH`) is the socket's absolute path,
  outside `data_dir`. Required when enabled.
- `socket.mode` (`SEAMLESS_PEER_AUTH_SOCKET_MODE`) is its permission bits in
  octal, default `0666`: every local account may knock, and the kernel still
  names each one. `socket.group` (`SEAMLESS_PEER_AUTH_SOCKET_GROUP`) hands the
  socket to a group, for a mode such as `0660`.
- `users` maps an OS account (name or uid) to `sealed` - projects reachable only
  from that account - and `projects` - projects it shares with the owner. It is
  file-only: a map of lists has no single-variable spelling.

A client of such a daemon names the socket as its `server_url`:
`unix:///absolute/path.sock`. It needs no `mcp.api_key`, and its hooks and MCP
registration go through the `seam` CLI (`install-hooks` does that by itself for
a socket URL).

The block is new after v0.7.4; releases before it refuse config keys they do
not know, so the example file leaves it commented out.

## Generating a key

`mcp.api_key` guards `/api/mcp` and the console. On a true first run - no
config file anywhere in the search order and no `SEAMLESS_MCP_API_KEY` in the
environment - `seamlessd serve` (or `install-hooks`) generates one and writes
it to `~/.config/seamless/seamless.yaml`, so a fresh install never handles the
key by hand. An existing config file is never edited, even when its key is
empty; set one yourself:

```bash
openssl rand -hex 32
```

The daemon still starts with an empty key, but every MCP and hook request is
rejected until one is set - `seamlessd doctor` reports it as a warning.

## Every key

| Key | Type | Default |
|---|---|---|
| `addr` | string | `127.0.0.1:8081` |
| `data_dir` | string | `~/.seamless` |
| `role` | string | `server` |
| `server_url` | string | - |
| `allowed_hosts` | []string | - |
| `tls.cert_file` | string | - |
| `tls.key_file` | string | - |
| `tls.ca_file` | string | - |
| `mcp.api_key` | string | - |
| `budgets.max_briefing_tokens` | int | `1500` |
| `budgets.recall_budget_tokens` | int | `1000` |
| `budgets.tool_event_max_chars` | int | - |
| `briefing.constraint_max_full` | int | `4` |
| `briefing.convention_max_full` | int | `4` |
| `briefing.memory_max_age_days` | int | - |
| `briefing.memory_max_items` | int | - |
| `briefing.findings_count` | int | `3` |
| `briefing.findings_max_age_days` | int | - |
| `briefing.ready_tasks_shown` | int | `3` |
| `briefing.pending_plan_max_days` | int | `7` |
| `briefing.stage_unknown_max_age_days` | int | `7` |
| `briefing.hard_cap_multiplier` | int | `2` |
| `briefing.include_parent_memories` | bool | `true` |
| `briefing.sibling_findings_count` | int | `2` |
| `briefing.include_sibling_memories` | bool | - |
| `briefing.utility_weight` | float64 | `0.4` |
| `briefing.utility_mode` | string | `auto` |
| `features.research` | bool | - |
| `features.momentum` | bool | - |
| `features.gamification` | bool | - |
| `console.level` | string | `basic` |
| `search.semantic_floor` | float64 | `0.3` |
| `llm.provider` | string | `openai` |
| `llm.openai.api_key` | string | - |
| `llm.openai.base_url` | string | `https://api.openai.com/v1` |
| `llm.openai.chat_model` | string | `gpt-4o` |
| `llm.openai.embedding_model` | string | `text-embedding-3-large` |
| `llm.openai.embedding_dims` | int | `3072` |
| `llm.ollama.base_url` | string | `http://127.0.0.1:11434` |
| `llm.ollama.chat_model` | string | `llama3.3:latest` |
| `llm.ollama.embedding_model` | string | `qwen3-embedding:8b` |
| `llm.ollama.embedding_dims` | int | - |
| `llm.anthropic.api_key` | string | - |
| `llm.anthropic.base_url` | string | `https://api.anthropic.com` |
| `llm.anthropic.chat_model` | string | `claude-sonnet-5` |
| `gardener.enabled` | bool | `true` |
| `gardener.interval_minutes` | int | `60` |
| `gardener.dedup_threshold` | float64 | `0.88` |
| `gardener.staleness_days` | int | `90` |
| `gardener.digest_days` | int | `30` |
| `gardener.tool_event_retention_days` | int | `30` |
| `gardener.stale_plan_days` | int | `14` |
| `gardener.stale_stage_days` | int | `14` |
| `gardener.session_idle_minutes` | int | `45` |
| `capture.allowed_ports` | []int | `[80, 443]` |
| `plan_capture.enabled` | bool | `true` |
| `plan_capture.auto_task` | bool | `true` |
| `plan_capture.inject_related` | bool | `true` |
| `update.check` | bool, optional | unset |
| `update.check_interval` | duration | `6h` |
| `update.auto` | bool, optional | unset |
| `update.max_defer` | duration | `24h` |
| `update.min_age` | duration | `24h` |
| `peer_auth.enabled` | bool | - |
| `peer_auth.owner` | string | - |
| `peer_auth.socket.path` | string | - |
| `peer_auth.socket.mode` | string | - |
| `peer_auth.socket.group` | string | - |
| `peer_auth.users` | map of string to {sealed []string, projects []string} | - |

## Annotated example

This is `seamless.yaml.example` from the repository, verbatim.

```yaml
# Seamless configuration example: every key, at its default. Safe to commit --
# it holds no secrets.
#
# `make install` seeds ~/.config/seamless/seamless.yaml from this file when that
# config does not exist yet, and never overwrites it afterwards. So fill in your
# secrets THERE, not here: the installed copy is the live one, and the daemon and
# hooks read it from any directory.
#
# Every key also has a SEAMLESS_* environment override, and env wins over the
# file. Config file search order: $SEAMLESS_CONFIG, ~/.config/seamless/seamless.yaml,
# ./seamless.yaml (gitignored -- the pre-install layout's config).

# HTTP bind address. Change it here and re-run `make install`: this file is the
# single source of truth for the bind, and the Makefile reads the port back out
# of it rather than assuming 8081.
# env: SEAMLESS_ADDR
addr: "127.0.0.1:8081"

# Data directory: SQLite database + markdown trees (memory/, notes/). A leading
# ~ is expanded to the home directory.
# env: SEAMLESS_DATA_DIR
data_dir: "~/.seamless"

# This install's part in a deployment. `server` (the default) runs the daemon
# here. `client` runs no daemon at all: serve refuses, there is no database and
# no corpus on this machine, and every surface -- the seam CLI, the hooks, the
# MCP registration -- dials server_url instead. A client REQUIRES server_url.
# env: SEAMLESS_ROLE
role: "server"

# The base URL clients dial, as a bare scheme://host[:port] with no path, query
# or fragment. Empty means "derive it from addr", which is what a loopback
# install wants. Set it when the bind address is not the address clients use: a
# wildcard bind, a LAN name, a TLS listener. Naming it also adds its host to the
# Host-header allowlist, which is what arms that guard on a wildcard bind.
# A wildcard host here is refused at load (0.0.0.0, ::, [::0], ...): that
# answers "where do I listen", not "where do I dial", so it belongs in addr
# instead.
# A client of a peer_auth daemon (see peer_auth below) names its socket here
# instead -- unix:///absolute/path.sock -- and needs no mcp.api_key: the daemon
# identifies the calling OS account by the uid the kernel reports.
# env: SEAMLESS_SERVER_URL
server_url: ""

# EXTRA Host-header values the daemon answers to, beyond the loopback names, a
# concrete bind host, and the host of server_url. Requests arriving under any
# other Host get 421 (the DNS-rebinding guard). Naming even one host turns the
# guard on for a wildcard bind, which otherwise has no allowlist to apply.
# env: SEAMLESS_ALLOWED_HOSTS (comma-separated, e.g. "studio.local,192.168.1.10")
allowed_hosts: []

tls:
  # PEM certificate chain and private key. Both set -> the daemon serves https
  # (TLS 1.2 floor) instead of http, and the console session cookie is marked
  # Secure. One without the other is refused at load: it cannot serve TLS. The
  # certificate's SANs must cover the host of server_url, which is what
  # `seamlessd doctor` checks. A leading ~ is expanded.
  # env: SEAMLESS_TLS_CERT_FILE / SEAMLESS_TLS_KEY_FILE
  cert_file: ""
  key_file: ""
  # CLIENT-side extra root CA: the certificate the `seam` CLI adds to the system
  # trust store when dialing an https server_url, for a private CA or a
  # self-signed server. It is the client half of the pair above -- setting
  # cert_file together with role: client is refused.
  # env: SEAMLESS_TLS_CA_FILE
  ca_file: ""

mcp:
  # Static bearer key protecting /api/mcp and the console. On a true first run
  # (no config file anywhere) `seamlessd serve` generates one into
  # ~/.config/seamless/seamless.yaml; a file you author yourself needs one set
  # by hand:
  #   openssl rand -hex 32
  # An empty key is reported as a warning by `seamlessd doctor`.
  # env: SEAMLESS_MCP_API_KEY
  api_key: ""

budgets:
  # Hard token budget for the SessionStart briefing.
  # env: SEAMLESS_MAX_BRIEFING_TOKENS
  max_briefing_tokens: 1500
  # Default token budget for a recall response.
  # env: SEAMLESS_RECALL_BUDGET_TOKENS
  recall_budget_tokens: 1000
  # Per-field cap (in runes) on captured Interactions transport events (tool-call
  # args/result, hook prompt, session findings). 0 = unlimited: content is stored
  # in full and the tool-event retention prune is the growth control. Set a
  # positive value only as an opt-in guard against pathological payloads.
  # env: SEAMLESS_TOOL_EVENT_MAX_CHARS
  tool_event_max_chars: 0

# Briefing injection tunables: what the SessionStart <seam-briefing> auto-injects.
# Defaults reproduce the built-in behavior; every knob is also editable live in
# the console (Settings -> Briefing injection), which stores a runtime override
# in the database that WINS over this file and env until reset. Constraints and
# active-plan rollups are never dropped by these knobs (constraint_max_full only
# shapes how constraints render); a pinned stage is exempt too while its Status
# header marks a live gate.
briefing:
  # Top-ranked constraints rendered as full bullets in the briefing's
  # Constraints section; the rest collapse into one compact "+N more, equally
  # binding" line that still names every one. 0 = no tiering (all full).
  # env: SEAMLESS_BRIEFING_CONSTRAINT_MAX_FULL
  constraint_max_full: 4
  # Top-ranked conventions rendered as full bullets in the budget-competing
  # Conventions section; the rest stay behind the section's count line
  # ("recall kind=convention"). Starred conventions always render full.
  # 0 = no tiering, like constraint_max_full.
  # env: SEAMLESS_BRIEFING_CONVENTION_MAX_FULL
  convention_max_full: 4
  # Drop memory-index lines not updated within this many days. 0 = no filter.
  # env: SEAMLESS_BRIEFING_MEMORY_MAX_AGE_DAYS
  memory_max_age_days: 0
  # Cap on memory-index lines before budget packing. 0 = budget-only.
  # env: SEAMLESS_BRIEFING_MEMORY_MAX_ITEMS
  memory_max_items: 0
  # Recent findings injected. 0 hides the section.
  # env: SEAMLESS_BRIEFING_FINDINGS_COUNT
  findings_count: 3
  # Drop findings older than this many days. 0 = no filter.
  # env: SEAMLESS_BRIEFING_FINDINGS_MAX_AGE_DAYS
  findings_max_age_days: 0
  # Ready-task titles listed in the "Ready tasks" section. 0 hides the section.
  # env: SEAMLESS_BRIEFING_READY_TASKS_SHOWN
  ready_tasks_shown: 3
  # Days a captured, unapproved Claude Code plan earns an "awaiting approval"
  # line. 0 = no age cutoff.
  # env: SEAMLESS_BRIEFING_PENDING_PLAN_MAX_DAYS
  pending_plan_max_days: 7
  # Grace window (days since last update) a stage memory stays pinned when its
  # Status header is missing or not a live gate (open|in_progress|blocked).
  # 0 = pin forever.
  # env: SEAMLESS_BRIEFING_STAGE_UNKNOWN_MAX_AGE_DAYS
  stage_unknown_max_age_days: 7
  # Times budgets.max_briefing_tokens = the absolute truncation ceiling. 0 = 2.
  # env: SEAMLESS_BRIEFING_HARD_CAP_MULTIPLIER
  hard_cap_multiplier: 2
  # A child project inherits its shared parent's active memories.
  # env: SEAMLESS_BRIEFING_INCLUDE_PARENT_MEMORIES
  include_parent_memories: true
  # Recent findings from family-member projects. 0 hides the section.
  # env: SEAMLESS_BRIEFING_SIBLING_FINDINGS_COUNT
  sibling_findings_count: 2
  # Fold family members' memories in as a low-priority "Sibling memories"
  # section (their constraints/stages never cross over). Off by default.
  # env: SEAMLESS_BRIEFING_INCLUDE_SIBLING_MEMORIES
  include_sibling_memories: false
  # Utility's share of the memory-index sort key: (1-w)*recency + w*utility,
  # where utility is the time-decayed demand score from retrieval_stats.
  # 0 = pure recency (legacy). Constraints/stages/favorites stay pinned.
  # env: SEAMLESS_BRIEFING_UTILITY_WEIGHT
  utility_weight: 0.4
  # Gate for the briefing's utility re-ordering: auto = per project once the
  # gardener's readiness latch trips; on = everywhere now; off = never. The
  # bounded recall/prompt-recall utility boosts are always on.
  # env: SEAMLESS_BRIEFING_UTILITY_MODE
  utility_mode: auto

# Optional features. These ship OFF: a fresh installation exposes none of them
# until you turn one on -- here, via env, or live in the console (Settings ->
# Features), which stores a runtime override in the database that WINS over this
# file and env until reset. Turning a feature off hides its console screens, its
# MCP tools, and its agent-facing mentions; it never deletes stored data, and
# re-enabling restores every surface. An installation that already holds research
# data keeps the feature on across the upgrade (a one-time seeded override).
features:
  # Research labs and trials: the Labs and Trials console screens, the trials
  # search scope, and the lab_open, trial_record, trial_query MCP tools.
  # env: SEAMLESS_FEATURES_RESEARCH
  research: false
  # Momentum surfaces woven into existing screens: the plan finish-line card
  # (and its agent-briefing emphasis), the activity calendar with capture
  # streaks, knowledge payoff moments, and project maturity stages.
  # env: SEAMLESS_FEATURES_MOMENTUM
  momentum: false
  # The arcade layer of the Now screen: the day tape, personal records, the
  # hot-streak pulse, and celebration moments. Every number is judged from
  # real recorded activity; off, the Now screen carries no trace of it.
  # env: SEAMLESS_FEATURES_GAMIFICATION
  gamification: false

# The observability console. Presentation only: nothing here changes what your
# agents receive -- briefings, MCP tools, hooks, recall, and the gardener are
# identical at every level.
console:
  # How much of the console you see. One of: basic | standard | advanced.
  #   basic    -- the essentials: what your agents remember (Memories, Notes),
  #               what they did (Sessions), cleanup suggestions (Gardener).
  #   standard -- adds following the work: Now, Projects, Plans, Tasks.
  #   advanced -- every screen and knob, including Interactions and Retrieval.
  # Hidden screens stay reachable by URL (they show a banner), so no link ever
  # breaks. The console's own picker (Settings -> Experience, or the welcome
  # card on Home) stores a choice in the database that WINS over this file and
  # env until reset. A fresh installation starts at basic; an installation that
  # already recorded sessions before levels existed starts at advanced (a
  # one-time seeded choice), so an upgrade never hides a screen you were using.
  # env: SEAMLESS_CONSOLE_LEVEL
  level: basic

# Console search (the full page's fused semantic+lexical retrieval). Agent
# recall is not affected by these knobs.
search:
  # Minimum cosine similarity a semantic-only hit needs to appear in results;
  # hits the keyword leg also matched are exempt. Without a floor the cosine leg
  # is pure nearest-neighbor, so any query -- including nonsense -- fills the
  # page to its limit. 0 disables the floor. Useful values depend on the
  # embedding model; 0.3 suits OpenAI text-embedding-3-*.
  # env: SEAMLESS_SEARCH_SEMANTIC_FLOOR
  semantic_floor: 0.3

llm:
  # Primary provider for chat (session digests) and embeddings. OpenAI is the
  # default and the first-class provider. One of: openai | ollama | anthropic.
  # env: SEAMLESS_LLM_PROVIDER
  provider: "openai"

  openai:
    # env: SEAMLESS_OPENAI_API_KEY
    api_key: ""
    base_url: "https://api.openai.com/v1"
    # Any OpenAI chat model; set to your preferred one.
    chat_model: "gpt-4o"
    embedding_model: "text-embedding-3-large"
    # Native dimensionality of the embedding model. 0 = auto-detect from the
    # first embedding response (the store records dims per row regardless).
    # Maximum: 65536.
    embedding_dims: 3072

  ollama:
    base_url: "http://127.0.0.1:11434"
    chat_model: "llama3.3:latest"
    embedding_model: "qwen3-embedding:8b"
    # 0 = auto-detect; maximum 65536.
    embedding_dims: 0

  anthropic:
    # env: SEAMLESS_ANTHROPIC_API_KEY
    api_key: ""
    # env: SEAMLESS_ANTHROPIC_BASE_URL
    base_url: "https://api.anthropic.com"
    chat_model: "claude-sonnet-5"
    # Anthropic has no embeddings API; when provider=anthropic, embeddings fall
    # back to OpenAI if its api_key is set, else Ollama.

gardener:
  # The gardener only ever proposes; it never mutates memory without an explicit
  # apply. Safe to leave enabled.
  # env: SEAMLESS_GARDENER_ENABLED
  enabled: true
  # Minutes between full gardener passes (dedup, staleness, stale-stage,
  # dead-weight, digest, stale-plan, memory-wanted).
  # env: SEAMLESS_GARDENER_INTERVAL_MINUTES
  interval_minutes: 60
  # Cosine similarity at/above which two active memories are proposed for a merge.
  # env: SEAMLESS_GARDENER_DEDUP_THRESHOLD
  dedup_threshold: 0.88
  # Days of no activity (no update, injection, or read) before a memory is
  # proposed for archiving. Constraints and stages are never archived this way
  # (dead stages are handled by stale_stage_days below).
  # env: SEAMLESS_GARDENER_STALENESS_DAYS
  staleness_days: 90
  # Trailing window of completed sessions rolled into a monthly digest proposal.
  # env: SEAMLESS_GARDENER_DIGEST_DAYS
  digest_days: 30
  # Age (days) beyond which transport-level Interactions events (tool.call,
  # hook.prompt) are pruned. 0 disables the prune. Domain events are never pruned.
  # env: SEAMLESS_TOOL_EVENT_RETENTION_DAYS
  tool_event_retention_days: 30
  # Days after which a captured Claude Code plan still in draft/presented is
  # proposed for abandonment (retag plan-status:abandoned). 0 disables the pass.
  # env: SEAMLESS_GARDENER_STALE_PLAN_DAYS
  stale_plan_days: 14
  # Days without an update after which a stage memory that is not a live gate
  # (Status done, missing, or unrecognized) is proposed for archiving.
  # 0 disables the pass.
  # env: SEAMLESS_GARDENER_STALE_STAGE_DAYS
  stale_stage_days: 14
  # Minutes of no activity before an active session counts as dead: the reaper
  # expires it and the console stops showing it as live. One knob drives both,
  # so they never drift. Must comfortably exceed a long single agent turn.
  # Must be positive; omit the key to retain the built-in 45m default.
  # env: SEAMLESS_GARDENER_SESSION_IDLE_MINUTES
  session_idle_minutes: 45

capture:
  # Destination ports the capture_url tool may dial, enforced in the dialer on
  # the initial URL and on every redirect hop (alongside the private-IP and
  # scheme guards). This is an SSRF control: widen it only for a host you trust,
  # and never to reach anything on your own network. An empty list or a missing
  # key means the 80/443 default -- it does NOT mean "any port". Ports outside
  # 1-65535 are rejected at startup.
  # env: SEAMLESS_CAPTURE_ALLOWED_PORTS (comma-separated, e.g. "80,443,8080")
  allowed_ports: [80, 443]

plan_capture:
  # Capture Claude Code plan-mode activity via the PostToolUse/SubagentStop/
  # PermissionRequest hooks: plan-file saves upsert cc-plan notes, planning
  # subagents cache as cc-agent notes, approval flips the plan note's status.
  # env: SEAMLESS_PLAN_CAPTURE_ENABLED
  enabled: true
  # Create an "Implement plan: ..." tracking task when a plan is approved.
  # env: SEAMLESS_PLAN_CAPTURE_AUTO_TASK
  auto_task: true
  # On a session's first captured plan iteration, return related prior plans and
  # memories to the planning agent as additionalContext.
  # env: SEAMLESS_PLAN_CAPTURE_INJECT_RELATED
  inject_related: true

# Update check and automatic updates. The daemon asks GitHub's releases API
# whether a newer Seamless release exists -- one anonymous request that carries
# no identifiers -- and tells you when one does. auto, max_defer and min_age
# decide whether and when it also installs one by itself, which only an install
# the curl or PowerShell installer made ever does: Homebrew, builds from source,
# client machines and layouts the daemon cannot vouch for are only told.
# Set auto, max_defer and min_age here, not in the daemon's environment: any
# SEAMLESS_* variable besides SEAMLESS_CONFIG there makes the install
# notify-only, so their env variables below can turn automatic updates off but
# never on.
update:
  # Unset (the default) means on for release builds and off for builds from
  # source. false means zero update traffic: no request to GitHub at all, and so
  # no automatic update either. The key is left commented out because seamlessd
  # releases before v0.7.0 reject config keys they do not know, so setting it
  # pins this file to v0.7.0 or newer.
  # env: SEAMLESS_UPDATE_CHECK
  # check: false
  # Time between checks, as a Go duration (6h, 90m, 1h30m) from 1h to 720h. A
  # bare number is refused rather than guessed at: write 6h, not 6.
  # env: SEAMLESS_UPDATE_CHECK_INTERVAL
  check_interval: 6h
  # Whether the daemon installs a newer release by itself. Unset (the default)
  # means on. false keeps the check and its notices and leaves installing to you
  # (seamlessd update); a false here or in the environment is final, and the
  # console cannot turn it back on. Commented out for the same reason as check.
  # env: SEAMLESS_UPDATE_AUTO
  # auto: false
  # How long a pending automatic update waits for a quiet daemon -- no live
  # agent session, no request in flight -- before it takes the next lull in
  # requests instead. A Go duration from 1m to 720h.
  # env: SEAMLESS_UPDATE_MAX_DEFER
  max_defer: 24h
  # How long a release must have been out before an automatic update takes it,
  # so a bad one can be pulled first. A Go duration from 0 (no wait) to 720h.
  # Installing by hand (seamlessd update) does not wait.
  # env: SEAMLESS_UPDATE_MIN_AGE
  min_age: 24h

# Peer-user authentication: a Unix socket whose callers the daemon identifies by
# the uid the kernel reports (never by anything the client sends), and the map of
# which OS accounts may reach which projects. Off by default, and off is exactly
# the behavior of every earlier release. On:
#   - the TCP bearer key is the owner account: it reaches every project except
#     those sealed to another account, and so does the owner on the socket;
#   - a configured account reaches only its own projects (sealed plus shared)
#     and reads -- never writes -- the global scope;
#   - any other uid on the socket is refused (403);
#   - the console is the owner's alone, and shows no project sealed to another
#     account;
#   - the daemon reads no agent files (transcripts, plan files): findings
#     harvest, token totals, model sniffing, plan and subagent capture are off.
# serve refuses to start unless the boundary would hold on disk too: the daemon
# runs as its own account (not the owner, not root), data_dir is that account's
# and 0700, the socket's directory and every directory above the data, the
# config file and the seamlessd binary are writable by nobody but root and the
# daemon's account, every named project exists, and every sealed project is
# confidential or sealed. `seamlessd doctor`, run as the daemon's account,
# reports each of those. See the peer_auth guide in the docs for the deployment.
# The block is new after v0.7.4 and releases before it refuse config keys they
# do not know, so it is left commented out here.
# peer_auth:
#   # env: SEAMLESS_PEER_AUTH_ENABLED
#   enabled: false
#   # The OS account (user name or numeric uid) the TCP bearer key stands for.
#   # env: SEAMLESS_PEER_AUTH_OWNER
#   owner: ""
#   socket:
#     # Absolute path. Its directory may be searchable by the accounts that
#     # connect, and writable by nobody but the daemon's account or root. Not
#     # inside data_dir, which is 0700.
#     # env: SEAMLESS_PEER_AUTH_SOCKET_PATH
#     path: ""
#     # Octal permission bits. Connecting needs write permission on the socket;
#     # the kernel still names every peer, so 0666 lets every local account
#     # knock and refuses the unconfigured ones. Empty means 0666.
#     # env: SEAMLESS_PEER_AUTH_SOCKET_MODE
#     mode: "0666"
#     # Optional group (name or gid) to hand the socket to, with a mode such as
#     # 0660 to let only that group's members connect at all.
#     # env: SEAMLESS_PEER_AUTH_SOCKET_GROUP
#     group: ""
#   # OS account (user name or numeric uid) -> its reach. File only (no env).
#   # sealed: reachable ONLY from this account's socket connections; a project
#   # is sealed to at most one account, and never also listed for another.
#   # projects: also reachable from this account, and still the owner's too.
#   # The first sealed project (else the first shared one) is the account's
#   # home: where its sessions and unplaced calls land.
#   users:
#     _example_role:
#       sealed: [example-project]
#       projects: []
```
