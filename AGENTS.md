# AGENTS.md

Coding conventions for AI agents working in the Seamless repository. For the
project overview, structure, and commands, see `CLAUDE.md`.

## Go style rules

### General

- Go 1.26+. No CGO. Pure-Go SQLite driver (`modernc.org/sqlite`).
- Never use emojis in code or comments. Never include attribution/credit lines
  in commits, PRs, or code.
- Format with `gofmt`. No exceptions. Prefer modern stdlib (`slices`, `maps`,
  `strings.SplitSeq`, iterators) in new code.

### Package structure

Each domain package under `internal/` follows a consistent layout:

- `service.go` -- business logic (the public API of the package)
- `store.go` -- SQLite data access
- `handler.go` -- HTTP handlers (only for packages with an HTTP surface)
- `{feature}.go` -- pure functions (parsers, ranking, frontmatter, etc.)
- `*_test.go` -- tests alongside source

Strict layering, no circular imports. `cmd/` wires everything; no package imports
`cmd/` and no domain package imports another's HTTP layer:

```
cmd/seamlessd, cmd/seam, cmd/seambench, cmd/demoseed, cmd/docsgen
  -> internal/{mcp,hooks,console}         (API surfaces)
    -> internal/{retrieve,lifecycle,gardener,files,capture,bench,demokit,update,screening,feed,commitfeed,approval,backup} (domains)
      -> internal/{store,events,llm,core,config,validate,access}   (foundations)
```

(`access` imports only `config`; `store` and `events` import `access`.
`passkey` imports nothing internal; `approval` imports `store` and `passkey`.
`devices` imports `store`, `events` and `config`; `approval` and the API
  surfaces import it. `relay` imports `devices`, `access`, `config` and `files`;
  only `cmd/seamlessd` imports it.)

(The dependency-aware ready-queue and lease-based claiming live in
`internal/store/tasks*.go`; there is no `internal/tasks` package.)

### Naming

- Interfaces: role nouns (`Store`, `Recorder`, `Embedder`), not `IStore`.
- Constructors: `New{Type}(deps) *Type` (or `(*Type, error)` when it can fail).
- Domain errors: package-level `var Err{Condition} = errors.New(...)` sentinels,
  checked with `errors.Is`.
- Tests: `Test{Unit}_{Scenario}` or table-driven with `t.Run`.
- IDs: ULID everywhere (`github.com/oklog/ulid/v2`) via `core.NewID()`. Never UUID.

### Error handling

- Wrap with context and `%w`: `fmt.Errorf("pkg.Service.Method: %w", err)`.
- Sentinel errors via `errors.New`; reserve `fmt.Errorf` for wrapping (with `%w`).
- Either log or return an error, never both. Fatal to the operation -> return it;
  non-fatal (graceful degradation) -> `slog.Warn`/`Debug` and continue.

### Logging & context

- `log/slog` (stdlib). INFO for lifecycle, WARN for recoverable, ERROR for
  failures, DEBUG for per-request. Never log memory/note bodies -- IDs only.
- All service/store methods take `context.Context` first. Thread it through.
  Never `context.Background()` in a handler or a goroutine spawned by one; derive
  from the request ctx, or `context.WithoutCancel` for work that outlives it.

### Database

- SQLite with WAL mode and foreign keys ON, set by Go on connection open (never
  in migration SQL). Single `seam.db` at `{data_dir}/seam.db`. `SetMaxOpenConns(1)`.
- Migrations: numbered SQL files under `internal/store/migrations/`, embedded via
  `go:embed` and registered in the `Migrations()` list. Each runs once, inside a
  transaction, tracked in `schema_migrations`. NEVER edit an applied migration;
  append a new numbered one, and keep it additive (see "Release discipline"
  below). Adding a `.sql` file requires a matching `go:embed` line and
  `Migrations()` entry, or it silently never runs.
- Files are the source of truth for durable knowledge (memory/, notes/). The
  `*_index` tables and FTS are rebuildable mirrors kept in sync by the files
  watcher + startup reconciliation; use `content_hash` to skip unchanged files.
- Embeddings live in the `embeddings` table as little-endian float32 BLOBs.
  Similarity is brute-force cosine in Go -- do NOT add a vector database.
- Unified FTS5 (`fts`) spans both halves of the corpus, with `fts.kind` naming
  which (`core.ItemKind*`). It is maintained by explicit INSERT/DELETE, not
  triggers, because it is not external-content -- and each half is maintained
  next to its own writer: the file-backed knowledge kinds (memory, note) from
  the files layer, the DB-native work record (task, trial, session findings)
  from `store/index_work.go`. A new writer of an indexed column must refresh the
  mirror; a new indexed KIND must also appear in `recallScopeKinds`, or it is
  indexed and unreachable.
- The work record carries no embeddings. There is no `content_hash` reconcile
  loop behind a DB row, so vectorizing one would mean a provider round-trip
  inside `tasks_add`/`trial_record`/`session_end` with no way to backfill. Those
  kinds are lexical-only on purpose; do not "complete" them by embedding them.

### Testing

- `testify/require` (fail fast), table-driven for multi-case functions. (This
  overrides the global "stdlib testing only" preference -- the ported v1 code and
  this plan standardize on testify; stay consistent.)
- SQLite tests use a fresh on-disk DB in `t.TempDir()` or a named in-memory DB
  (`file:{t.Name()}?mode=memory&cache=shared`) -- never the unnamed shared-cache URI.
- Migrations are exercised against a fresh DB in tests.
- External services (Ollama, OpenAI) are mocked with `httptest.NewServer`. Never
  hit a real network service in a unit test.
- No `time.Sleep` for synchronization. Use channels, `sync.WaitGroup`, or polling
  with a deadline.
- `cmd/seamlessd`, `internal/update` and `internal/agentproc` also run on macOS
  and Windows in CI (`os-smoke`), so keep their tests portable: a test that
  fakes a home sets `USERPROFILE` beside `HOME` (`os.UserHomeDir` reads it on
  Windows, and a test that sets only `HOME` writes the real profile there);
  file-mode assertions go behind `if runtime.GOOS != "windows"` (Windows
  reports `0666`); POSIX fixture paths go through `absFixture`
  (`desktop_mcp_test.go`); expected paths use the OS separator.

### Security invariants

- Path traversal: reject any relative path with `..`, absolute paths, or null
  bytes (`internal/validate.Path`). Names that become filenames go through
  `validate.Name`; human titles through `validate.Title`.
- Auth: a single static bearer key guards `/api/mcp` and the console. Bind
  `127.0.0.1` by default. No JWT, no registration. The other credentials are the
  kernel's: with `peer_auth` on, a Unix-socket request is authenticated by its
  peer uid (see "Peer-user auth" below), and the TCP key is the owner; and, on a
  server that serves other computers, a paired device's own (a relay's client
  certificate, a key device's key; "A paired device is the owner, and no more"
  below), which every bearer check accepts through `devices.Bearer`.
- SSRF: URL capture must reject private IPs, localhost, and `file://`.
- Every JSON/body handler starts with `http.MaxBytesReader`.

## Domain invariants

Rules that plausible-looking code breaks. Each is enforced somewhere specific;
the pointer is where to look, not a substitute for reading it.

### Supersession (`internal/lifecycle`)

- `invalid_at` is the ONLY authoritative "has left the indexes" predicate
  (`core.Memory.Active()` is `InvalidAt == nil`; SQL filters `WHERE invalid_at
  IS NULL`). Never infer live-ness from an empty `superseded_by` -- an archived
  memory has `invalid_at` set and `superseded_by` empty, and is equally gone.
- `lifecycle.Supersede`/`Archive` are the only writers of
  `invalid_at`/`superseded_by` in non-test code; MCP, gardener, and console all
  route through them. Do not stamp those fields by hand.
- `valid_from` is never touched by supersession: `[valid_from, invalid_at)` is
  the memory's bi-temporal record.
- Stamped exactly once, guarded on the way in: already-invalid `old` ->
  `ErrAlreadyInvalid`, `old.ID == replacement.ID` -> `ErrSelfSupersede`,
  invalid/empty replacement -> `ErrInvalidReplacement`. Acyclicity falls out of
  those guards (every edge points invalid -> active, and nothing clears
  `invalid_at`); there is no cycle traversal to lean on.
- Pass the FULL body. Both functions append a tombstone to `old.Body` and
  rewrite the whole file, so handing them an index row -- which carries no body
  -- truncates the memory to just the tombstone. Re-read the file first.
- A superseded name stays occupied: reviving it is `ErrPathOccupied`, never a
  silent clobber. `memory_delete` is the only escape hatch, and it drops history.
- A supersede that fails after the new memory is written is a tool ERROR naming
  the still-active target, never a success payload with an error inside it (F9).

### Mutating a corpus file (`internal/files`)

- Every read-modify-write of a memory or note goes through
  `files.Manager.Mutate(path, fn)` / `MutateMemory` / `MutateNote`. Reading an
  item, editing the struct, and writing the whole file back is a lost update:
  two racers read the same starting content and the second rename wins, with no
  error anywhere -- the index upserts by id, so even `UNIQUE file_path` stays
  quiet and the loser is told it succeeded.
- The invariant is not "the write is locked", it is that **the read feeding the
  write happens inside the same lock**. Wrapping only the write fixes nothing
  and looks exactly like a fix.
- The lock is per path and NOT reentrant. `WriteMemory`/`WriteNote`/`Remove`
  deliberately take no lock, so calling them from inside `fn` is the point.
  `MutatePaths` locks several files in sorted order for a mutation spanning two
  of them (a note changing project); sorted order is the deadlock discipline.
- A callback returning `files.ErrNoChange` skips the write and reports the
  loaded item -- that is how an already-correct favorite flip avoids rewriting
  and re-indexing the file.
- `expect_hash` is compared against the FILE's hash read inside the lock, never
  the index row: the watcher re-indexes on a 300ms debounce, so the index
  confirms the stale hash for exactly the window the precondition exists to
  refuse.
- Edit vs supersede is a LIFECYCLE boundary, not a convenience call. `memory_edit`
  is for changes carrying no new claim (typo, formatting, stale path, stage
  `Status` flip, metadata); a changed MEANING goes through `memory_write`
  `supersedes` so the old memory retires into readable history. Both edit tools
  and `MCPInstructions` render `agentguide.EditVsSupersede` so the rule cannot
  drift into two versions.

### Session binding and scope (`internal/mcp`)

- Never read `project` from tool args directly. Route it through
  `resolveReadScope` (nothing to infer -> global) or `resolveWriteScope`
  (nothing to infer -> `errNoScope`). They apply `validateProjectArg`, which is
  the path-traversal defense: a project slug becomes a directory under
  `memory/`/`notes/`, and the data-dir boundary check alone does not catch
  `../notes/_global`.
- A durable create uses `resolveWriteScope` -- it fails closed. Reaching for
  `resolveReadScope` on a write silently lands it in global.
- Precedence: explicit `project` -> the bound session's project -> the sole
  unambiguous ambient. Ambients spanning more than one project are
  `errAmbiguousScope`, not a guess.
- A connection is bound two ways, and `getBinding` is the one reader of both.
  An explicit `session_start` binding (in memory, keyed by the MCP client
  session id; evicted on `session_end` and by the opportunistic sweep) wins over
  the process binding: the single active ambient session stamped with the agent
  process the request names in `X-Seamless-Agent-Process`
  (`agent_binding.go`). The process binding is identification, not inference --
  it lives in the session row and on every request, survives restarts and
  reconnects, and counts as a binding for the isolation fence.
- Two active sessions stamped with one process are a host running several
  sessions at once: leave the call unbound. Never pick between them by recency
  or cwd -- that is the cross-agent bleed in a new place.
- With `isolation.requests` on, a call that identifies its conversation (its
  agent-process identity names its own ambient row: `mcp.identify`,
  `access.Grants.HomeKnown`) is judged from the conversation's home whatever it
  is bound to (`callerScope`). A `session=` handle or an earlier `session_start`
  binding acts as a session outside the home only where the home may read and
  write (`speakAs`), else the call is refused. `session_start` binds it only to
  its home, or to an open project while its label is empty; into a fenced
  project it files the move or grant its home and the project call for
  (`sessionStartReach`), and into an open project or the global scope it is
  refused once the label holds anything. A client that sends no identity binds
  as before. On such a server `session_start`'s description says so
  (`sessionStartToolFor`).
- A caller that named its process is never inferred into a session stamped
  with a different one: `store.AmbientScope.Caller` filters every ambient
  fallback lookup. Once such a caller's own session has ended, the sole live
  ambient is by construction another agent's.
- The process binding revives a session the idle reaper EXPIRED (a call from the
  same live process is proof of life), never one that was COMPLETED on purpose.
- Revival, the active lookup before it, and the session an owner's access or
  grant request falls back on see only the caller's own principal's sessions
  (`store.ReviveExpiredAgentSession` reads the account from ctx;
  `mcp.ownProcessSessions`; `approval.ownAgentSessions`), filtered BEFORE
  counting: a restricted account revives its own expired session as the owner
  does, and another principal's row stamped with the same process is neither
  reopened nor a second candidate. The revived session must be `access.Writable`
  or `access.Locked`: a restricted account's session in a project it no longer
  reaches stays expired; the owner's in a locked project revives as before.
- `session_start` adopts only the caller's OWN session when the request names a
  process. The sole-same-cwd adoption heuristic is for callers that name none;
  for one that does, a sole same-cwd ambient that is not its own belongs to
  another agent.
- Agent process identities (`internal/agentproc`) are opaque tokens: validate
  with `agentproc.Valid` at every boundary (the header, the hook query) and
  compare for equality. Never parse one for its pid or start time.
- A lost binding does NOT error -- the call degrades to the ambient fallback.
  Never assume the binding you started with is still there.
- Stamp provenance with `s.boundSession(ctx)`, not the raw binding.
- A new tool must be registered in `registerTools` AND bump `ToolCount`, or
  `doctor` fails its tool-count assertion.
- A tool whose words change with an optional feature is registered through a
  `*ToolFor` method (`sessionStartToolFor`, `tasksUpdateToolFor`,
  `accessRequestToolFor`) that returns the Catalog's tool word for word while
  the feature is off. `Catalog()` keeps the default text, which the docs site
  renders and the defaults golden (`tools_list.json`) pins: never edit a catalog
  constructor's words for an opt-in feature; the docs page explains the
  feature's words by hand.

### Peer-user auth (`internal/access`)

With `peer_auth` on, every request carries a `View`: the owner's on TCP
(`ownerHandler`), the kernel-named peer's on the socket (`peerConnContext` +
`peerHandler`, cmd/seamlessd/peerauth.go), and through a relay
`peer_auth.relays` maps, the peer its relay's kernel named (`relayHandler`,
cmd/seamlessd/peerauth_relay.go). Off, no request carries one, and
every check below answers yes -- which is how "off is exactly the old
behavior" holds. Read the guide (docs-src/guides/peer-auth.md) before touching
any of it.

- **The policy lives in `internal/access` and nowhere else.** `View.Visible`,
  `View.Writable` and `View.SessionScope` are the rules; surfaces ask them (or
  `access.Visible(ctx, p)`) and never compare principal kinds or slugs
  themselves. `store.CanRead`/`CanWrite` consult the View FIRST -- including
  for the caller's own bound project -- so every isolation fence inherits it.
- **The peer_auth matrix and the defaults row.** `cmd/seamlessd`'s
  `TestPeerAuthMatrix_{Leases,Console,CLI,Proofs,Anchors,Grants,Advisory}`
  drive every `mcp.Catalog()` tool (a tool without a probe fails the
  coverage check), every console route, the hooks, A2A, the live feed and a
  real `seam` binary over a real socket, with secrets planted in every closed
  project. A new tool or route gets its probe there. The fenced-sessions rows
  are `TestFencedMatrix` (tier x target x conversation x action: rebind,
  session=, receipt, write grant, stamps, subagent, captures, harvest, resume,
  /clear) and `TestFencedExample_DataRoom` (the owner's data-room example on a
  throwaway `seamlessd serve`); a new `isolation.requests` surface gets a row
  there. `make defaults-golden`
  (build tag `defaultsgolden`, not in check) compares every surface with
  peer_auth and isolation.requests off against a golden written on main: run
  it before branch locked-projects merges. A difference the owner accepts as a
  change for every install (not part of the opt-in feature) goes in
  `dgAccepted` with the decision -- the branch's exact text, main's, and how
  many times it occurs, so a stale entry fails; never regenerate the golden
  on the branch.
- **A restricted local account (`peer_auth.restricted`, `access.KindRestricted`)
  reaches the open projects that exist, per request** (`internal/access/restricted.go`).
  `peerHandler` loads `store.OpenProjectSlugs` for every request and
  `View.WithOpenProjects` drops the locked ones; a failed load answers 503. It
  reads and writes those, reads global, and nothing else: one refusal text for
  a confidential, sealed, locked or unregistered slug, none listed, and it
  CANNOT CREATE a project (every creation path -- write scope, session_start,
  project_create, the repo map -- refuses it first). No requests, grants or
  leases. `access.AccountOf(ctx) != ""` is the one test for it, and the
  account name is the label every write carries: sessions, tasks (`account` at
  creation; `updated_account` only when a title or body changes,
  `store.taskTextAccount`) and trials are stamped by the store from ctx; memory
  and note content by the MCP and hook write paths (frontmatter `account`,
  `mcp.accountAfter`). A restricted account's change of the labeled text (a
  memory's body or description; a note's body, title or description; a task's
  title or body) stamps the label, and nothing else moves it: status, claims,
  releases, moves, tags, stars and tombstones keep it. The owner's write clears
  it only once every labeled field is the owner's (`textOwned`: replaced with
  new text in that write, or empty), so the owner's append, search/replace edit
  or one-field rewrite keeps it. The owner's writes carry no label.
- **The daemon is the account root pinned, else a system account.** Root pins
  the daemon's account in `access.DaemonAccountPath`
  (`/opt/seamless/etc/daemon-account`, one line `<name> <uid>`;
  `access.DaemonAccount`), believed only as a regular file owned by root
  under directories owned by root, none of them group- or world-writable; a
  pin that is there but fails that or does not parse refuses every daemon
  (`access.ErrDaemonPin`), never falls back. With a pin only its uid is the
  daemon (`access.DaemonUID`); with none, `access.SystemAccount` (macOS below
  uid 500, Linux below login.defs' UID_MIN, no range elsewhere). `seam`'s
  `verifyDaemon` (cmd/seam/daemon_peer.go), the Touch ID approver's
  `PeerGuard.admit` (Socket.swift, with DaemonPin.swift mirroring the pin;
  `< 500` alone, it is macOS only) serve's preflight (`filesystemProblems`, which doctor reports, refusing too a restricted account that is the pinned one) and, on a client computer, the relay's (`relay.Env.AccountProblems`, against that computer's own pin) apply it, so a restricted or other account serving a socket as itself is refused; keep the four in step. The same
  preflight judges the TLS pair like the config file: rewritable by no one but
  root or the daemon's account, and the key readable by no account but the
  daemon's (never world, group only the daemon's own) -- whoever reads it
  serves the console's own name.
- **A restricted account's binding rule is a proposal, never a write.** Any
  write, append, edit, overwrite, supersede or delete that touches a
  `screening.BriefingKinds` memory (constraint, convention, protocol, stage)
  goes to the held queue with `screening.ReasonRestrictedAccount` and
  `Detail["account"]`, whether or not write screening is on; no Screening
  service means the write is refused, never landed. Promote is the held
  queue's Release (console, `seam promote`, TTY only), the owner's alone. It
  may not star (favorite_set), apply gardener proposals, or plan them
  (gardener_request, gardener_split). It may not give a project memory the name
  of a live global briefing-kind memory (the shadow would answer by-name reads).
  One it wrote before such a global rule existed steps aside for the rule
  (`retrieve.ShadowedRule`): memory_read by a bare name, recall's link expansion
  and the console's wiki links answer with the rule, memory_read pointing at the
  other memory (`project_memory`, or `global_memory` on an explicit `project=`
  read), and a by-name write that names no project (memory_append, memory_edit,
  memory_delete, favorite_set, memory_write's supersedes) is refused, naming
  both (`refuseShadowedWrite`). Reads by id and calls with an explicit
  `project=` behave as before. A memory or note whose content it changes loses
  its star.
- **The owner's decisions are refused from an agent's process.** On the socket,
  bringing a fence down, deciding held writes and changing features go through
  `refusedFromAgent`/`ownerHands` (`access.FromAgent`, the kernel's process chain,
  true when the chain cannot be read). It stops ordinary tool calls and is not a
  wall: a detached process or the TCP key passes. The lock is the wall.
- **A session belongs to the principal that started it**
  (`core.Session.Account`, set once by `store.CreateSession`). Every surface
  that resumes, binds to, names or writes into a session checks
  `access.SessionAccountOK` -- MCP refuses a named one, implicit bindings and
  hooks treat a mismatch as foreign -- in both directions.
- **A restricted account's session names are a namespace of its own**
  (SEAL A2; `access.QualifiedSessionName`). Session names (and ambient
  identities) are unique instance-wide, so a name another principal holds
  would answer apart from a free one: an existence oracle. Every session it
  starts is stored as `<account>:<name>`, its agent's external session id
  too (`store.CreateSession` qualifies both); `store.SessionByName` reads a
  name inside the caller's namespace; the hooks qualify the client's
  `session_id` once, right after decoding (`namespacedExternalID`), and name
  its ambient session `<account>:cc/...` (`ambientNameIn`). Qualifying is
  idempotent, so the account may name its session either way. The owner's
  names are read as given; the owner names an account's session by its
  qualified name, and may not start one inside a listed account's namespace
  (`access.ReservedSessionName`). An OS account name never holds ":", so the
  first one ends the account. A new hook route that reads a session id from
  its body must qualify it the same way.
- **A lease is judged, never trusted** (`internal/access/lease.go`, migration
  031). A locked
  project is closed to the owner: `View.Listed` (name and counts) is true,
  `Visible`/`Writable` are false. A View opens it only through
  `View.WithLeases`, which keeps a lease only on the owner's socket view (this
  daemon's socket, or a mapped relay's) with a
  kernel-reported process token, and only one that names that process, a
  locked project and an expiry after now. A TCP view keeps none. Lease rows
  (`store.ActiveAccessLeasesFor`) are input to it and never a grant on their
  own. `store.ApproveAccess` is the only writer of a lease: it is single use
  and bound to the challenge, and it replaces a lease rather than extending
  one.
- **A lease covers the agent's process tree, as the kernel reports it**
  (`internal/access/proctree*.go`). `ReadPeer` pins the socket peer at accept
  (the audit token's pidversion on macOS; the start time under a pidfd on
  Linux) and `Peer.Chain` walks its parents per request, verifying every link
  (a parent starts no later than its child; on macOS the parent's unique id
  is the one the child records). A link that does not verify ends the chain:
  an orphan, or a reused pid, reaches nothing above it. `peerHandler` attaches
  the chain and its leases (`leasedView`) and fails closed on every error. A
  lease names the AGENT process (claude/codex), never a shell, terminal or
  multiplexer above it -- one of those would lease every process under it --
  and the self-reported agent-process header is never part of the chain. That
  rule is enforced at filing (`approval.judgeAnchor`) and again in the approve
  core (`approval.storedAnchorRefusal`, for a row an older daemon filed);
  `internal/approval/anchor.go` is the one classifier, and it also yields
  `Facts.Agent`. The residual risk is a same-uid debugger attaching to a
  process in the tree; the guide states it.
- **Locked is not sealed.** Isolation (open / confidential / sealed) says what
  flows in and out of a project; the lock says whether
  even its own agents need a fresh owner approval. Two settings, two words:
  never call the lock "sealed" in code, text or config. A locked project is
  normally confidential. `Policy.ConsoleHidden` is the set no owner surface
  shows without a lease (the locked projects).
- **A lock comes from the file or the console, and only the console's can be
  removed there** (`peer_auth.force_locked`; `store/console_locks.go`, migration
  037; `approval/console_lock.go`; `console/lock.go`). The policy is
  `access.New` over the file plus `Policy.WithConsoleLocks` over the table, once,
  at startup, and stays immutable. A change is one passkey ceremony
  (`approval.lockChangeChallenge` binds the ceremony id, the project and the
  direction; a ceremony is single use), from a browser only, then a clean
  shutdown and an in-place re-exec (`errServeRestart`, `reexecSelf`), so the
  pid does not change. Never add a route that writes `console_locks` without
  that proof. A lock needs a fence, judged inside the write
  (`store.AddConsoleLock`), and a fence cannot drop to open under a lock of
  either kind (`store.LoosenProjectIsolation`). A reader of the locked set goes
  through the policy, or `lockedProjects` in cmd/seamlessd for a one-shot
  command; `peer_auth.force_locked` alone is not the whole set. `locked` is the
  key's earlier name and keeps loading (`foldLegacyLocked`).
- **A backup carries its locks, and an import never drops one silently**
  (`archive.Manifest.Locks`, `archive/locks.go`, `cmd/seamlessd/import_locks.go`).
  The manifest names every locked project, the config's force locks and a
  `--no-db` export's included, because no archive carries a config. An import
  either puts each lock back (`applyArchiveLocks`; on a merge before the corpus
  pass, so a merge stopped partway leaves no locked content unlocked) or, on the
  owner's explicit answer through `OnLocks`, leaves it off and records that.
  `LocksKeep` is the zero value. Any new import path keeps both properties.
- **Approver credentials are enrolled by the daemon's account, never by a
  route** (`store/approvers.go`, `internal/approval`, migration 032). Every
  passkey or Touch ID key needs a one-time code that only
  `seamlessd approvers enroll-code --key <fingerprint>` mints (it opens the
  database directly, so running it is the proof of being the daemon's account).
  `approvers`, `approve` and `console-open --unlock` open it through
  `openServedDB`, as do `family`, `unmap-repo`, `retire-project` and `map-repo`
  (which alone creates an absent database on a server install), and `doctor`
  reads what it finds without creating or migrating; `openServedDB` never
  creates or migrates: the schema is the daemon's to
  move, so a database behind the binary is refused, and `approvers` settles its
  arguments before opening anything.
  The code is minted for that one key: its stored hash binds
  `store.ApproverKeyFingerprint`, so a code read off the owner's terminal cannot
  enroll another, and the key exists before the code does (the helper's window
  and the console page show the command). An enrolled passkey never vouches
  for a new one, however many are enrolled: an enrolled key can take a
  console lock off with no one present, so one tricked touch must not add a
  key an agent holds (F2 of the console-lock review). Never add an
  enrollment path whose proof is an existing credential's assertion (the store
  has none: `store.EnrollApprover` takes a code and nothing else). Removing
  and resetting are that command's too.
  A passkey is bound for life to the RP ID it was enrolled under, and records it
  (`approver_credentials.rp_id`, migration 038; '' for Touch ID keys and for
  rows from before it). The relying party comes from `config.RelyingParty`
  alone -- configured, else derived from a TLS `server_url` host
  (`config.ServedRelyingParty`, exact host, never a parent domain), else
  localhost -- so serve, export, import and console-open agree; never derive it
  anywhere else. serve's log and doctor name the passkeys a changed RP ID
  strands (`store.StrandedPasskeys`) and judge every origin against the
  listener: TLS and port, the certificate naming the host, and the daemon
  holding EVERY address the host resolves to
  (`cmd/seamlessd/passkey_origin.go`), since an address it does not hold is one
  another local process can serve the origin from. serve listens beside a
  concrete IPv4 `addr` on its own interface's IPv6 addresses that reach no
  further (`pairedIPv6` in `cmd/seamlessd/listen_pair.go`: ::1 beside loopback,
  only unique local ones beside a private or tailnet address), and doctor's
  held set (`boundAddrs`) reads the same function, as setup's address step does
  when it picks the bind (`setupBindFor`), so the three cannot disagree.
  Never pair a private address with a global one: that widens a LAN bind to the
  internet.
  The one exception is a Touch ID re-enrollment: a credential whose own key
  signs `approval.TouchIDReplaceMessage` (the code's hash, the new key, its id)
  is retired in the enrollment's transaction (`store.EnrollTouchIDRetiring`).
  A key retires only itself, never a passkey or another Mac's key; one that
  does not sign is left for `seamlessd approvers remove`.
  No socket or TCP route may mint a code (the matrix asserts the table stays
  empty). A credential's sign counter is recorded with a compare-and-set
  (`store.RecordApproverUse`), so of two racing assertions only one counts.
- **Requests are filed at the refusal; approvals need a proof**
  (`internal/approval`, `access/request.go`). `access.Refusal(ctx, p)` and
  `access.SessionScope` call the `LockRequester` the surface planted (MCP:
  `lockRequestMiddleware`) and name the request in the refusal; a requester
  runs with none in its own context, so resolving the session cannot file
  again. The lease's anchor is the session's stamped agent process (else the
  agent-process header), and only when it equals `AgentID()` of a hop in the
  kernel chain. A request is filed only for an agent program (Claude Code,
  Codex, the Claude app's disclaimer), a process with one above it in the
  kernel chain, or a stamped node, bun or deno. Broad ancestors (shells,
  multiplexers, terminals, editor helpers, login processes, pid 1, other app
  executables) are never filed or approved (`anchor.go`). `approval.Approve` is the one approve core: a proof
  (`ProofDaemon` only from `seamlessd approve`, run as the daemon's account
  and checked by `approverAccountCheck`; a passkey assertion over
  `approval.PasskeyApproveChallenge` (the request's challenge, the facts
  digest, the access and the duration, as a Touch ID key signs them): the
  passkey page issues options only once the duration is chosen (`?for=`), and
  the core recomputes the binding from the submitted duration and the facts
  as they stand; a Touch ID signature over
  `TouchIDApproveMessage`) and the facts digest the owner saw. No route
  constructs `ProofDaemon`. An agent's socket connection may not deny.
  An asker that already holds what it asks for (its tree's lease, or a read
  grant from the same project) more than `access.ExpiryWarning` from its end
  is answered with it (`approval.AccessLiveError`) and files nothing; from the
  warning on it files a new approval. A refusal (`approval.ViaRefusal`)
  whose asker's last request for the project (or the read grant's pair) the
  owner denied files nothing: it names the denial (`approval.DeniedError`,
  `access.DeniedRemedy`) and how to ask again (`access.AskAgain`). The
  agent's explicit request (`ViaRequest`: access_request, `seam access
  request`) always files, and its reason fills in a pending request's
  missing one (`store.FillAccessReason`, once, including a refresh filed
  with none: a refresh carries the reason it was given, and given none it
  shows "(no reason given)"). The reason is a fact, so an owner mid-review
  gets the facts-changed answer.
  `access_request` (`internal/mcp/access_request.go`) files one explicitly and
  is registered only while peer_auth locks a project, or while
  `isolation.requests` is on without peer_auth (`mcp.OffersAccessRequest`: the
  advisory tier's explicit ask; `Config.OfferAccessRequest`: the `offerCommits`
  pattern for a whole tool), so every other install's tools/list, `ToolCount`
  and doctor line are unchanged. `accessRequestToolFor` words it for its tier:
  the advisory tier's text (`advisoryAccessRequestTool`), the switch's under
  peer_auth with `isolation.requests` on (`grantsAccessRequestTool`), and the
  Catalog's, word for word, on every other server. `Catalog()` lists it last
  (`LockedOnlyTools`), `LockedToolCount`
  counts it, and both doctors assert `ToolCountWith(locked)` (seam doctor
  mirrors it as `expectedTools` + `lockedOnlyTool`, pinned by a test). Its
  `reason` never reaches a tool.call event. `seam access request` files one
  from a shell. With `isolation.requests` on, `access_request` (and `seam access
  request`, through `approval.Grants.RequestGrant`) asks for what the
  conversation's home and a project no lock covers call for: a read grant from a
  confidential home, a move from an open one or into a sealed project; a session
  that names no conversation keeps the legacy read grant.
- **The in-session approval dialog is a prompt, never a proof**
  (`internal/mcp/elicit.go`). A call on the peer_auth socket that files a
  request (refused at a lock; refused at a fenced project, or at
  `session_start`, with a read, read-and-write or move request; or
  `access_request`) shows the owner the request's facts as an MCP
  elicitation -- warnings first, every value stripped of control and format
  characters -- only when the client declared elicitation and a proof can
  finish in the dialog (an enrolled Touch ID key; or passkeys on, one
  enrolled, and a URL-capable client). It then polls the request's row until
  it leaves pending, up to `DialogWait` (110 seconds) from the start of the
  call. Any process of the owner's account, or a client hook, can answer an
  elicitation, so the answer only starts a proof: Touch ID sends
  `core.ApproverNotification` on the call's own reply, which the `seam
  mcp-proxy` bridge swallows and answers by opening the helper app; a passkey
  sends the request's page as a URL elicitation (with `?for=<duration>` when
  the form chose one). Only the approve core
  decides. Decline and cancel deny nothing. Without every precondition the
  call answers at once with the plain refusal, whose remedy offers only the
  paths that exist (a passkey URL only with a passkey enrolled, Touch ID only
  with a Touch ID key enrolled and a form-elicitation client), and there is
  at most one dialog per request per connection. A dialog that ends with the request
  still pending replaces the refusal's remedy with one saying how it ended
  and the paths left (`refusalAfterDialog`), never a second remedy after
  the first. mcp-go v1.1.1 drops a notification
  queued just before a handler returns, so the call stays open `notifyHold`
  after queueing one. The bridge reads stdin on its own goroutine for the same
  feature: the client's answer arrives behind the call that waits on it.
  The bridge watches the helper it launched through its stderr (`open
  --stderr` to a FIFO; the exit status never survives LaunchServices) and,
  when it exits having been seen there (it wrote, or held the FIFO's write
  end after the bridge closed its own, so a killed helper counts; one never
  seen is not reported), posts `core.ApproverEndedNotification` (request_id, outcome
  cancelled|closed) on its own MCP session -- a cancel is the helper's last
  stderr line beginning "SeamApprove: Not approved:", pinned on both sides
  (the app's FlowTests, cmd/seam's approverOutcome tests); the daemon takes it only
  on the socket, from a chain holding the request's process, while that
  dialog waits, and ends the wait at once. It grants and denies nothing.
- **In the advisory tier the dialog's answer decides** (`isolation.requests`
  without peer_auth; owner decision D7; `internal/mcp/advisory_dialog.go`,
  `approval/advisory.go`). A TCP call that files or finds a grant or move
  request (a refusal at a fenced project, `session_start` into one,
  `access_request`) shows a form-elicitation client the facts (warnings first,
  the carry, what read-only and read and write each let happen, the agent's
  reason as its own) with a Decision (Approve or Deny, no default), the access
  where offered (read-only by default) and, for a grant, the duration. Approve
  goes through `Grants.Approve`/`ApproveMove` with `approval.ProofDialog` over
  the digest the form showed; Deny through `Grants.DenyInDialog`; both record
  `decided_by = in-session-dialog` (`approval.DecidedByDialog`), never
  `bearer-key`. Declining, cancelling, a form with no decision and `DialogWait`
  decide nothing. Only advisory Grants accept the proof: under peer_auth it is
  `approval.ErrDialogPeerAuth` (`ErrNoProof` in the lock core), and
  `advisoryCall` is false for any request carrying a View, so the 2026-11-06
  rule stands there. The tier keeps a TCP client's declared capabilities
  (`capsKept`), so the dialog survives a restart.
- **The Touch ID approver is opt-in, socket-only, and keychain-bound**
  (`macos/`, `cmd/seam/touchid*.go`, `cmd/seam/approver_app.go`; console
  `approversTouchIDEnroll`, `touchIDProof`). Nothing installs or launches
  Seamless Approve but `seam touchid enroll`, which holds the release's
  checksums.txt to its Sigstore bundle for seam's own tag (the updater's
  check, `internal/releasesig`), checks the zip against it
  (`update.VerifyChecksum`), then `codesign --verify --strict --deep`, the
  identifier `com.arctop.seamless.approve`, `spctl` for a release asset, and
  seam's own Developer ID team, pinned with a `-R=anchor apple generic ...
  certificate leaf[subject.OU]` requirement. An unsigned seam pins a release
  asset to the team that signed it (the Sigstore check vouches for the zip),
  and asks the owner y/N only for a local `--app` build or a kept install.
  `seam touchid enroll` opens the app at once; its code dialog does not hide
  on deactivate and floats, since the owner mints the code in another
  terminal and the LSUIElement app has no Dock or Command-Tab entry.
  Only the macOS seam links the verifier (`touchid_release_darwin.go`, about
  +12 MB); keep `internal/releasesig` out of every file without a darwin
  build tag, or the Linux and Windows seam grow by as much. The key is a Secure Enclave key in the
  data-protection keychain under an access group only the signed app may
  claim -- never a CryptoKit blob in a file, which any process of the owner's
  could load and sign with behind a prompt of its own wording. The helper
  takes no fact from its arguments: it reads the request and its challenge
  over the socket and refuses a socket whose peer is its own uid or not the
  daemon's account. A key is enrolled (`POST
  /console/approvers/touchid/enroll`) and a proof accepted (`POST
  /console/access/{id}/approve`, `proof=touchid`) only when
  `access.PeerAuthenticated`; a request's `challenge` appears in its JSON only
  when pending and read over the socket (a nonce, not a secret). Never put a
  team ID in source: the pin is read at run time from seam's own signature,
  and the build substitutes it from the environment. Every release ships the
  app: `scripts/release/macos.sh` builds it on the owner's Mac with the
  Developer ID identity that signs the darwin binaries (the team seam pins),
  from the local Developer ID provisioning profile for
  `com.arctop.seamless.approve` (Keychain Sharing on; the team id is read
  from the profile), notarizes and staples it (`notarize.sh` with a notarytool
  keychain profile), and uploads `seamless-approve_<version>_darwin.zip` to
  the tag's draft release with the darwin archives. The signed tag's manifest
  names it, and release.yml lists it in the cosign-signed checksums.txt like
  them. A renewed certificate needs a regenerated profile: build.sh refuses a
  profile that does not list the signing certificate.
- **The locked view at the agent surfaces** (`internal/mcp/locked.go`,
  `internal/hooks/locked.go`). The SessionStart hook in a locked project's
  repository records the session there, metadata only, files its access
  request and answers with the locked notice (`lockedStart`) -- unless the
  agent process it would stamp fails the anchor rule
  (`approval.Service.JudgeStart`, judged as filing judges a stamped session):
  then nothing is recorded and the notice says why. The notice offers Touch
  ID to Claude Code and, hedged, to Codex (`formDialogClient`, `touchIDIf`), as
  MCP offers it only to a form-capable client (`callWays`). Hooks treat that
  session as `reachLocked` (heartbeat, model, completion; no findings or
  captures). Over MCP it binds nothing (`getBinding`), but `lockedBinding`
  finds it: a call resolving its scope, session or actor through it is refused
  at the lock, never inferred elsewhere, and `recall` without `project=`
  answers from the rest of its scope and names it (`<project> (locked) may
  hold results`) without filing. project_list and scope errors list locked
  projects, marked. Every lock refusal is two lines, the rule then the remedy.
  A leased connection's first tool result carries the project's briefing once
  per lease, recorded on the lease row (`access_leases.briefed_at`), so a
  restart does not send it again, and from `access.ExpiryWarning` every tool result and
  hook injection says when the lease ends. The notices ride as extra text
  items outside `logMiddleware`, and a leased connection's tool.call events
  keep no arguments, result or error text: the event is filed under the bound
  session's project, and must never copy a locked project's content there.
- **Grants are judged, never trusted; every write meets the clearance rule**
  (`isolation.requests`, off by default; `access/grant.go`,
  `access/clearance.go`, `approval/grant.go`, `store/grants.go`,
  `store/clearance.go`, migrations 034 and 039). A grant request is an
  `access_approvals` row of kind `grant` (with `from_project`);
  `store.ApproveAccessGrant` and `store.GrantAccessApprove(With)` are the only
  writers of an `access_grants` row -- single use, bound to the request's
  challenge -- and `ApproveAccess(For)` refuses a grant request, so a grant is
  never a lease. The MCP edge (`grantsMiddleware`) and the hooks
  (`captureConfined`) load the state of the conversation the call speaks for
  (`store.ConversationClearance`): the owner's socket chain under peer_auth;
  else the conversation (client + external session id) the agent-process
  identity names (`mcp.conversationRef`) or the hook body names, plus every
  session of it as a session holder (grants filed before migration 039 name
  the session that asked); else, for a client that sends no identity, the
  session `getBinding` names -- never an ambient inference.
  `access.JudgeConversation` keeps only that holder's grants and carries its
  label, its flows into fenced projects and its home (the ambient row's
  project). `store.CanRead` honors a live grant (read or write) after the
  View, for a confidential target and only from its `from_project`; never a
  sealed project on either side. `store.CanWrite` asks, in order: the View;
  clearance (`access.WriteCleared`, which only refuses: every fenced project
  in the label is the destination or has an approved flow into it, and an open
  project or the global scope only while the label is empty -- the caller's
  own project included); the caller's outbound fence, which only a live
  read-and-write grant of the destination from the conversation's home opens
  (`access.WriteGranted`; a confidential caller that is the home); the
  destination's inbound fence, which nothing opens. A fenced home is cleared
  for its own sessions' fenced projects (`RecordDestinationOf`'s native-home
  rule). The MCP write funnel applies the clearance to an inherited scope too
  (`resolveWriteScope`: the binding, an ambient session or the peer's home),
  so a conversation that rebinds with `session_start` or names another session
  with `session=` still holds what it holds. `access.WriteConfined` is
  `!WriteCleared`; `access.JudgeGrants`/`store.HolderGrants` are the previous
  release's judge and no surface calls them. Receipts (`store/receipt.go`,
  `mcp/receipt.go`) are the one content-free write past the rule: a
  `tasks_update` that closes (done or dropped) a task whose claim, live or
  lapsed, a session the conversation left by a move holds
  (`store.ClaimedBeforeMove`), or that names `reference`, is a receipt.
  `store.JudgeReceipt` decides it -- never from a sealed home, nothing but the
  status and at most one `<home>:<id>` reference to an item that exists in the
  home, any other field refused with the rule -- and `store.SendReceipt` writes
  the status alone as the pre-move session, only while it still holds the claim
  (`TaskPatch.IfClaimedBy`), which releases it. Its task.transition goes under
  the task's project and that session with `receipt` and `reference`, never
  `claimed_by` or `released`. `reference` is registered only while
  isolation.requests is on (`tasksUpdateToolFor`); the catalog documents it.
  Requests are filed by `fenceRead` (an explicit `project=` or a by-ULID read)
  and, for an identified conversation, by `fenceWrite` (asking for read and
  write) and `session_start` (`askRefusal`, `sessionStartReach`); never by a
  filter (`fenceReadQuiet`) or the read half of a move or apply
  (`fenceReadNoAsk`). Without peer_auth the decision is the bearer
  key's (`approval.ProofKey`, accepted only by advisory Grants -- the docs call
  it advisory), or the in-session dialog's (`approval.ProofDialog`, likewise);
  under it, the approve core's proofs (`Service.prove`).
  Event slugs ride `from_project`/`target_project`, never a key in
  `feed.frameFields`. Off, no request carries grants and every check answers
  as before.
- **Moves and read-and-write grants are one approval each**
  (`isolation.requests`; `approval/ask.go`, `approval/carry.go`,
  `store/approvalkind.go`). The kind follows from the conversation's home and
  the target (`approval.KindFor`): an open or global home reaching a
  confidential project, or any home reaching a sealed one, is a move; a
  confidential home reaching a confidential project is a grant; nothing else
  is offered. A grant between confidential projects is read-only unless the
  owner chooses read and write (`Decision.Access`, which a passkey or Touch ID
  proof signs beside the facts digest). The facts state the carry (home, held
  projects, write destinations), and an approval records exactly the flows it
  lists. `store.MoveApproveWith` ends the pre-move ambient row (ambient = 0,
  completed, a metadata-only pointer, its task claims kept for receipts) and
  inserts the child (`parent_session_id`) as the conversation's one ambient
  row, plus a locked target's lease bound to the child, in one transaction.
  `ReactivateAmbientSession` never re-scopes a move's child. The lock core
  refuses a move (`ErrIsMove`), and Touch ID approves none until the helper
  words one (`ErrTouchIDMove`). A move's dialog never offers Touch ID: it is the
  passkey page or none, and its remedies name only the terminal and the page. An
  explicit ask without `write` is a lock request where a lock covers the
  project, else what the kind rule calls for. The first result after an approved
  move announces it and carries the new home's briefing once
  (`moveNoticeMiddleware`, `access_moves.briefed_at`, migration 040); into a
  locked project it is the lease's briefing too, sent once. The move's remedy
  and that notice offer the receipt for a task the pre-move session claimed,
  except into a sealed project, whose conversation sends none
  (`receiptAfterMove`, `afterMoveLines`); a move out of a fenced home -- always
  into a sealed project, since a confidential home reaches a confidential one by
  grant -- is told to write about that home in its new one, citing it in
  `core.CitationForm`. A `session_end`
  refused only by the clearance rule still ends the conversation's own session,
  its findings not recorded (`unrecordedEnd`). A restore adds today's grants,
  moves and flows to the backup's (`store.ApplyKeptState`), so it never shrinks
  a label.
- **The hooks file the record at the home** (`isolation.requests`,
  internal/hooks/home.go): the harvest, the Codex Stop and pushed findings, the
  captures and the prompt events go to `RecordDestination.Home` after P0's
  routing, and only where the clearance clears them (`captureConfined`,
  `homeRefused`, `store.UnrecordedFindings`). Recall and subagent briefings read
  the home plus live grant targets (`conversationScope`,
  `retrieve.ProjectPromptRecall`, `BriefingInput.Also`). A resume re-scopes only
  where `store.CanWrite(home, cwd)` holds, and a locked home goes to
  `lockedStart`. Off, every hook is P0's.
- **Provenance stamps are set from the conversation and never removed by a
  write** (`core.Stamp`, `access.Grants.Stamps`, `store/stamp.go`,
  `mcp/stamp.go`, `hooks/stamp.go`). The frontmatter `may_contain` is mirrored
  exactly into `item_provenance` by `files.Indexer` (the file wins); tasks,
  trials and findings live in the index only. Every content write stamps before
  it writes; supersession unions through `lifecycle.UnionStamps` (memory_write,
  the gardener, screening release); flag flips stamp nothing; reads name only
  `View.Listed` projects; a gardener write that folds stamped items carries
  their union (merge, consolidate with retired sources, and the digest, whose
  proposal records its findings' stamps under `may_contain`).
- **Source references are body text in one form, and the gardener reads them
  only from stamped memories** (`isolation.requests`; `core.Citations`,
  `gardener/copies.go`). An item names another project's item as
  `<project>:<ULID>` (`core.CitationForm`, the form
  `approval.CiteByReferenceLine` teaches); no tool takes an argument for it.
  stale-copy files an `archive` proposal (`basis: source_changed`,
  `changed_sources`) when a cited memory, note or task changed after the copy
  was last written; copy-relocate files a `relocate` (`basis: cited_sources`)
  for a copy whose stamps and citations all name one other project. Neither
  reads a source's content. A stale copy's scope spans its sources' projects
  (`ResolveProposalScope`). A reference that does not resolve in the project it
  names, or points into a project the copy may not read
  (`sourceJudge.readable`), is skipped without trace. The console warns before a
  stamped memory moves into a less fenced project (`console.moveWarning`) and
  blocks nothing.
- **A conversation is one console page, read through the request's handle**
  (`console/conversation.go`, `store/conversation.go`). Every session of a
  conversation (client + external session id) shows one card: the home chain
  (`access_moves`, `parent_session_id`), every project it reached with the
  approval and who decided it, and what its sessions wrote (their write events)
  with the stamps. Its rows are found by the sessions that asked, never by
  holder. Everything but the access tables is read through `s.db(ctx)` and
  `s.eventLog(ctx)`, so a locked project this request does not read shows its
  name, the approval and an unlock link, and no session, item, count or stamp;
  never read a conversation's sessions or items through `s.writeDB()`. The card
  shows only for a move, a grant, a grant's or move's request, or a stamped item
  (`convReached`). A project page's `?from=<project>` narrows Memories, Notes
  and Plans & tasks to the items whose stamp names it (`store.CameFromSources`
  and `CameFromItems`, joined with the items); nothing renders without a stamped
  item.
- **The identity comes from the transport, never the client.** A header, a
  `session=` handle, a body field or an agent-process identity can only ever
  narrow what the View allows. `getBinding` drops a binding to an unreachable
  project; `sessionByRef`/`sessionByID` report another account's session as not
  found; the process binding, ambient inference and `soleAmbientByCWD` skip
  sessions the caller may not reach; hooks blank a session id that names one
  (`foreignSession`).
- **A restricted account never writes the global scope** and has no home
  project: a write or session nothing places in an open project it reaches is
  refused (`View.SessionScope`), never landed somewhere else. It never grows
  the repo map (`cwdProject`).
- **Lists filter.** Anything that enumerates projects or sessions applies
  `access.Visible` (project_list, scope-error project lists, ambient
  candidates). A project a restricted account cannot reach is not listed for
  it at all, and the owner's lists mark a locked project. A task answer is a read of every task it names: a blocker or a
  `depends_on` id in a project the caller may not read (`taskReader`, which is
  `store.CanRead` from `callerScope`) is withheld for every caller, the count
  kept (`internal/mcp/tools_tasks.go`; the store's `TaskBlockedError` carries
  the claim refusal's blockers for it). The exception is the owner at a locked
  project it holds no lease on (`mcp.lockedBlockerProject`): the blocker is
  still withheld, but the words name the locked project and the remedy, and
  the claim refusal ends in a `Remedy:` line (`TaskBlockedError.Remedy`).
  Restricted accounts, and blockers an isolation fence alone withholds, keep
  the generic words. The console does the same on Tasks, the task reader and
  peek ("Depends on"), and the Plans & tasks tab (`markLockedBlockers`,
  `taskDetailData`, `lockedBlockersOf`, `store.OpenBlockerProjects` on the
  write handle, slugs only); a template line for it must render nothing
  while nothing is locked (`make defaults-golden`). A console-backed CLI
  lookup of a locked item answers 403 `{error, locked, project}` in
  `access.View.Refusal`'s words (`answerLocked`), and lists carry `locked`;
  a browser page of a locked plan or session goes to the project's unlock
  page (`lockedItem`, `?item=` and a `safeNext` `?next=`), a fragment says
  so in place.
- **A blocker removal judges reach like an add.** `tasks_update
  remove_depends_on` applies the add path's rule inside the store
  (`store.removeDeps`, `access.Visible`): a blocker the caller may not reach is
  kept and named in `not_depends_on` exactly like an id that names nothing,
  behind the same holder lock, and a withheld answer carries neither
  `removed_depends_on` nor `not_depends_on`. Every `tasks_update` transition
  records `from` (the status before the call), so readers tell an edit from a
  status move; events without `from` read as before.
- **The console reads through `store.OpenView`**: TEMP VIEWs that shadow every
  project-scoped table on a read-only handle. While a project is locked it
  reads through a `store.Views` pool instead (one handle per hidden set),
  chosen per request (`console/reach.go`): the socket View's leases, plus --
  for a cookie-authenticated browser only -- the live rows behind its
  `seamless_unlock` cookie and browser key. Console reads go through `s.db(ctx)` and
  `s.eventLog(ctx)`, never `s.cfg.DB`. A console unlock is a browser's, never a
  lease: it never reaches an agent. Console writes go to
  `s.writeDB()`, after reading their target through `s.db(ctx)`, the handle
  the request's reach chose; a gardener action by id checks `proposalReachable`
  first. A store function that both
  reads and caches takes both handles (`CaptureStreakWith` and its siblings).
  A new table must be classified in `viewTables` or `viewExempt`
  (`TestViewCoversEveryTable`), and the one table a view cannot shadow -- `fts`
  -- is filtered by the request's View in its only reader (`accessFilter`).
  Work done for no caller (the gardener, the reaper, milestones) is hidden by
  what it is about: an event recorded without a project is still hidden by its
  session, its item, or -- for the gardener's own events, whose payload carries
  the proposal key -- the proposal it concerns.
- **A cookie never names a browser alone** (`console/browserproof`,
  `static/browserkey.js`, `static/sw.js`). A browser sends its cookies to every
  port of the host, so a program running as the owner can open the owner's
  browser on its own port and read them. An unlock's `token_hash` is
  `store.HashBrowserUnlock(cookie, key)`. The key is a non-extractable P-256 key
  in the console origin's IndexedDB, and `withReach` counts an unlock only for
  a request whose `Seamless-Browser-Proof` that key signed within
  `browserproof.Window`. Every unlock route takes the key from the proof
  (`browserKeyOf`) and refuses without one. The service worker signs same-origin
  page fetches and GET or HEAD navigations, and never re-sends any other
  navigation: re-sent from the worker, a form posted from another site would
  pass `sameOriginRequest` as same-origin. A new grant of console access that
  lives in the browser binds the same key.
- **A console sign-in is bound like an unlock, and the key signs no browser in
  under it** (`console.sign_in: passkey`, off by default; console/signin.go,
  approval/signin.go, store/signin.go, migration 041). `classify` counts the
  cookie only when `store.HashConsoleSignIn(cookie, key)` names a live
  `console_signins` row and the proof verifies under that key; a key-derived
  cookie is nothing. The ceremonies are stateless until they succeed (an HMAC'd
  ticket; a sign-in ticket works once; the challenge binds the browser key).
  Sign-out needs the cookie alone and the origin check. A passkey is added only
  through `store.ApprovePasskeyEnrollment`, which `seamlessd approvers approve`
  runs as the daemon's account: the Add a passkey page is public and enrolls
  nothing, its code names a waiting row and carries no key, and no route
  approves. Every sign-in route is a 404 while the key signs browsers in, and
  refuses a restricted account first. A restore ends every sign-in and waiting
  passkey (`ResetRestoredAccess`); a merge carries neither.
- **The terminal sign-in is an unlock's terminal pattern, for the sign-in**
  (store/signin_terminal.go, console/signin_terminal.go, `console-open
  --sign-in`). The browser begins it under a fresh cookie bound to its key; the
  daemon's account approves its code (`store.ApproveTerminalSignIn`), which
  signs in that browser and no other. A waiting row lives in `console_signins`
  with `credential` `terminal-code:<code hash>` and `expires_at` equal to
  `created_at`, so no reader of a live sign-in counts it; approval is its one
  transition, a compare-and-set to `SignInByDaemon`. `endSignIns` ends waiting
  rows too (sign-out, restore, reset). `PasskeySignInsSince`, setup's proof,
  counts a passkey's sign-ins only.
- **A paired device is the owner, and no more** (plan networked-relay;
  `internal/devices`, `approval/devices.go`, `store/devices.go`, migration 042,
  `console/devices.go`, `cmd/seamlessd/devices.go`, `cmd/seamlessd/peerauth_relay.go`, migration 043). Pairing exists only on a
  server that serves other computers (`pairingOn`: TLS, a bind beyond loopback,
  or a `server_url` naming another host); off, there is no Devices page, no
  `/api/pair` and no client-certificate request. A one-time code (80 bits, 10
  minutes, one computer, only its hash stored) comes from the console or
  `seamlessd devices code`. Without peer_auth the code is the approval; under it
  the pairing waits until the owner approves it with a passkey on the Devices
  page, whose challenge binds the device's id, kind, name, fingerprint and key,
  and `store.ApproveDevice` compares the fingerprint inside its write: the code
  alone opens nothing. A relay is matched by the key of the client certificate
  its TLS connection presented (`devices.WithRelayClientAuth` asks only a client
  offering `devices.RelayALPN`; no chain is ever verified, the handshake's
  CertificateVerify proves the key); a key device by its key's hash.
  `devices.Service.Edge`, in front of the TCP routes, marks a matched request
  (`devices.WithCredential`) and makes it speak for the enrolled name (the host
  header and a hook's `host`); every bearer check asks `devices.Bearer`, so a
  new surface never compares the key by hand. A device key never signs a browser
  in. Under peer_auth a key device is a TCP caller, and a relay
  `peer_auth.relays` does not map is refused (403, review H1): the owner's view,
  no lease, so it never opens a locked project. The `X-Seamless-Relay-*` headers
  (wire v1: version, uid, chain; v2 adds programs, one canonical percent-encoded
  entry per hop, `devices.EncodeRelayProgram`) are read only under an active
  relay
  `Credential` (`devices.ParseRelayAttributes`; present but malformed is 400)
  and never reach a route raw (routes read `devices.RelayAttributesOf`); every
  other request has them stripped unread. A mapped relay (its enrolled name to
  the owner's uid there, file only) is judged as the socket is (`relayHandler`,
  `Policy.RelayChainView`): owner_uid is the owner on a socket
  (`Principal.Relay`), any other uid 403, nothing forwarded 400, a relay no
  passkey approved 403 until it pairs again; `/healthz` is not judged. Every
  token of a relay's chain names the relay (`ProcessToken.Relay`, key
  `relay/<id>/<boot>:<pid>:<version>`), so leases, grants and requests match on
  (relay, process) wherever keys are compared, and
  `access_leases`/`access_grants.relay_id` (migration 043) record it, written
  from the key by their one writer each. A v2 relay's programs
  (`Principal.Programs`) name its processes for the anchor rule and `JudgeStart`
  (`approval.programNamer`); no request is filed for a v1 relay's process or an
  anchor its relay did not name (`approval.ErrRelayUnnamed`). The approve core
  refuses a stored relay row with no program in `prove`, so grants and moves are
  covered too. A relay's restricted map (`PeerRelay.Restricted`) makes a uid
  there `KindRestricted`, labeled `access.RelayAccountLabel`, and the approve
  core refuses a stored relay row with
  no program. A relay's chain counts as an agent's (`FromAgent`).
  Revoking refuses the credential from its next request, ends the requests it is making and, in the same transaction, a relay's live leases, grants and waiting requests (`store.RevokeDeviceEndingAccess`). A merge carries no device, a fresh import revokes the snapshot's,
  and a backup restore keeps today's (`keptTables`).
- **The relay forwards and judges nothing** (plan networked-relay;
  `internal/relay`, `cmd/seamlessd/relay.go`, `doctor_relay.go`). `seamlessd
  relay serve` serves the socket agents dial beside a peer_auth daemon on a
  macOS or Linux client, as the account root pinned (else a system account,
  never root); `relay.Env.Problems` holds its binary, the socket's directory,
  its 0700 data dir (`/opt/seamless/relay`: `relay.json` 0600, the pairing and
  key) and every directory above them to serve's preflight rules, and makes no
  data dir for an account that fails them. Per connection it reads the caller's
  uid and process from its own kernel (`relay.ConnContext`; the chain walked per
  request), removes every `X-Seamless-Relay-*` and `Authorization` header a
  local client sent, sets the wire contract's four (version 2), and forwards
  over its
  client certificate (`devices.RelayTransport`: `config.HTTPClient`'s trust, the
  server's certificate pinned on top). It interprets and caches nothing: an
  unnamed caller is 403, a protocol switch is refused (400) and a server's 101
  is a 502 (review L2), and with `RefusingAccount` its own account is no caller,
  an unreachable or re-certified server 502 with the
  reason, and the server's answer goes back as it came. Without peer_auth on the
  server a relayed request is the owner's whatever uid it names, so the socket's
  mode and group (`--socket-mode`, `--socket-group`) keep other accounts out;
  doctor says who may connect (`relay.access`).
- **Events reach the console through the filtered handle.** The console's live
  feed re-reads each event through it (`liveEventVisible`) and the event log's
  reads go through `events.ReadingFrom`, so an event about a locked project
  reaches no page without a lease. Never insert into `events` anywhere but the
  recorder.
- **No agent-disk reads under peer_auth; the client pushes the captures.**
  `localDiskOK` answers no for every hook: the daemon runs as its own account,
  so every path a hook names was chosen by another account, and the daemon
  could be made to read its own data directory on a caller's behalf. Do not
  "restore" transcript harvest by opening caller-supplied paths. The five disk
  captures (session-end findings and token totals, the model sniff, plan and
  subagent capture, the git stamp) are made by `seam hook` with the daemon's
  own parsers (`internal/harvest`, a leaf both sides import) and pushed under
  `harvest.Field`: only when server_url names the peer_auth socket
  (`cmd/seam/hook_capture.go`), and read only on a request carrying a View
  (`internal/hooks/pushed.go`). A push is the caller's claim and lands through
  `sessionReachOf` and `fenceNotePush`: a locked session takes the model only;
  a session in a project locked to the caller is the caller's own only when
  its stamped agent process is in the caller's kernel chain
  (`sessionInChain`), so another tree's hook -- or a TCP hook, which has no
  chain -- that names it is foreign, and can neither end it nor revoke its
  lease;
  a refused capture gets the one-line `harvest.RefusedHeader` notice and is
  never retried; a note lands where `View.SessionScope` places it; a leased
  locked project takes a note only from a session recorded in it. A field that
  does not validate refuses the whole push, and the format is additive.
- **Prove it in the matrix.** The `TestPeerAuthMatrix_*` tests above drive
  every tool in `mcp.Catalog()` (a tool without a probe row fails the coverage
  check), every console route (read from console.go), the hooks, A2A and the
  live feed, and check state after every write. A new surface gets a row
  there.

### Write screening (`internal/screening`)

The `screening` optional feature (off by default; off is exactly the previous
behavior). Guide: docs-src/guides/write-screening.md.

- **A held write never touches the corpus.** It lives in the `held_writes`
  queue, not under `memory/` or `notes/`, and that is the whole exclusion:
  briefings, recall, prompt injection and the gardener read only those trees,
  so no reconcile or rebuild can un-hold it. Never "hold" by writing the file
  and filtering it out of readers.
- **Judge right before the files layer.** Every MCP tool that lands content
  asks `screeningOn` and, inside the same lock as its read, calls
  `judgeMemoryWrite`/`judgeNoteWrite` just before `WriteMemory`/`WriteNote`; a
  `heldError` aborts the mutation. A new content-writing tool joins them.
  Deletes and gardener applies of a constraint are held by reason, not screen.
- **A held call is held whole.** A `memory_write` that supersedes a constraint
  writes neither half; the release applies both. A release refuses a stale
  target (`ErrStale`) rather than undo a later write.
- **Events carry codes, never text.** `memory.screened`, `memory.held`,
  `memory.released` and `memory.discarded` hold verdict and reason codes, ids,
  kinds and sizes; never a body, a description, or the matched substring.
- **Clearing a taint mark is the owner's.** The session-taint hook only sets
  it; only the console's Held screen clears it. Do not add an MCP or hook path
  that clears one.
- **Provenance names a session only inside the memory's own scope**
  (`retrieve.MemoryProvenance`): a global memory written from a project must
  not carry that project's session name into other briefings.

Part 2 of the same feature (migration 029, `internal/screening/corpus.go`,
`refused.go`, `internal/feed`):

- **One corpus record.** `corpus_records` (keyed by file path) holds the hash
  Seamless last wrote, the review's verdict and the digest it judged, and the
  drift quarantine. `files.Manager.WriteMemory`/`WriteNote`/`Remove` record
  every write, but only while the watch is ARMED (the `screening_corpus_armed`
  setting, checked in the same statement): the record never claims a write
  nobody recorded. A new corpus writer goes through the Manager, or its writes
  read as drift.
- **The review runs at the first turn-on, never in a migration.** Migrations
  are SQL; the review reads files and runs the screens. Arming the first time
  runs it (`Service.Sync`); re-arming after the feature was off resyncs hashes
  and leaves trust marks alone. Only the owner re-runs it.
- **Trust is a digest, never a flag.** A memory is trusted while
  `core.MemoryDigest` of its current content equals the review's digest
  (status safe) or its owner confirmation. Never mark an item trusted without
  stamping the digest it was judged at.
- **Drift is quarantine in place.** Never move, rewrite or delete a drifted
  file: the owner may have edited it in an editor, and its only copy is on
  disk. Only the owner's Accept (or the bytes returning to `drift_base`) lifts
  a quarantine; a Seamless write over it does not, and an edit of it is
  screened against no baseline (`Server.drifted`).
- **Withholding is at injection only.** `retrieve` drops drifted memories
  (feature on) and untrusted ones (lockdown) from briefings and prompt recall
  (`withholding`). Recall and memory_read are not narrowed.
- **Lockdown only reduces.** It holds writes of briefing-injected kinds
  (`ReasonLockdown`) and narrows injection; the flag file
  (`screening.lockdown_file`) cannot be lifted from the console, and a file
  that cannot be checked counts as present.
- **access.refused carries codes.** Fence refusals are recorded in
  `fenceRead`/`fenceWrite` (a caller using the fence as a filter calls the
  `...Quiet` variant); a 401 anywhere and a reason noted with
  `access.NoteRefusal` are recorded by `refusalWatch` (cmd/seamlessd).
  Rate-limited; never arguments.
- **The feed is an allowlist.** `feed.frameFields` lists every payload key a
  frame may carry, each checked against its type's pattern; nothing else in an
  event reaches a frame, and `TestFrames_CarryNoContent` scans for content. A
  new event payload key reaches the feed only by being listed there. The feed
  tails the event log by rowid (not by time, not through the lossy live
  fan-out) from a cursor in its spool.
- **A restricted account is the actor's, never a field.** `events.Recorder.Record`
  and `RecordOnce` stamp `events.PayloadAccount` from `access.AccountOf(ctx)` on
  every event a restricted account's request records, on every surface, unless
  the payload already names one (a copy; the caller's map is never written); the
  feed reads it, or the event's session account, into `actor.account`. Record
  with the request's ctx (`context.WithoutCancel` keeps it), never a fresh
  `context.Background()`, or the account is lost. A refusal recorded outside the
  handler that made it (`refusalWatch`) takes the account from the
  `access.RefusalNote` (`Refusal.Account`). The owner's events and frames never
  carry it (`TestFrames_OwnerGolden`).

### Commit records (`internal/commitfeed`)

The `commits:` option (off by default; off is exactly the previous behavior).
Guide: docs-src/guides/security-monitor.md.

- **Off, the argument does not exist.** While `commits.record` is off,
  `registerTools` registers session_end and tasks_update without `commits`
  (`offerCommits`), so tools/list and the unknown-parameter refusal are the old
  ones word for word; `Catalog` keeps it for the docs. An argument behind an
  option follows this pattern, never a handler that accepts and drops it.
- **Recording is an update.** A session's commits are recorded after
  session_end's fence and before the session completes; a task's ride in
  `TaskPatch.AddCommits`, inside UpdateTask's transaction and holder lock. A bad
  id refuses the whole call (`validate.CommitIDs`: full 40- or 64-digit hex,
  lowercased, deduped, at most `store.MaxCommitsPerCall`), and no owner holds
  more than `store.MaxRecordedCommits`, the snapshot contract's per-entry bound.
- **The snapshot is identifiers and times.** Never a finding, title, body,
  path or project slug; a fenced project's repository is null. `CheckJSON`
  mirrors the consumer's contract, and the writer never replaces the file with
  a snapshot that fails it.
- **A session ended at its row's updated_at.** The reaper stamps its
  session.ended at reap time, up to the idle threshold after the last sign of
  life it leaves in updated_at; a time window must not stretch to the reap. A
  row still active but idle past `gardener.session_idle_minutes` has ended
  there too (`settleIdle`), so a dead agent's session never reads as open
  while the reaper is off.
- **The owner's entries are byte-identical.** `account` (a session's starter; a
  task's `taskAccount`: recorder, then creator, then text author) and the
  qualified `<account>:<name>` id appear only for a restricted account; an
  owner's session is held to the bare pattern (`sessionNamed`).
  `TestBuild_OwnerGolden` pins the shape.

### FTS5 and LIKE escaping (`internal/store`)

- User text reaching `MATCH` goes through `ftsQuery` (`fts.go`): it splits on
  non-alphanumerics, drops single-rune tokens, quotes each term and ORs them, so
  `chroma-boot-race` is three literal terms rather than a subtraction. Never
  build a MATCH expression by concatenation.
- `ftsQuery` returning `""` means "no usable token"; callers treat that as no
  results, never as an unfiltered query.
- User text in `LIKE` goes through `escapeLikePrefix` (`\`, `%`, `_`, in that
  order, with `ESCAPE '\'`). Never LIKE-escape a value compared with `=`.

### LLM degradation: remote vs local (`internal/llm`, `internal/retrieve`)

- Remote -- `ErrUnavailable`, `ErrAuth`, `ErrRateLimited`: the provider answered
  badly or not at all and may recover. DEGRADE; `recall` drops to lexical-only,
  which is honest.
- Local -- `ErrConfig`: the request never got built. No provider was contacted,
  no retry helps, it will not clear. SURFACE it -- degrading trades one loud
  failure for quietly worse recall for the life of the daemon.
- Classify at the `do` call sites via `doErr(op, err)`. Never hand-wrap a
  request-build failure as `ErrUnavailable`.
- `base_url` is validated in `NewEmbedder`/`NewChatClient` (the single
  construction points) because `url.Parse` accepts a bare host:
  `"api.openai.com/v1"` builds a perfectly valid request and only fails inside
  `Do` as an opaque transport error indistinguishable from an outage. That
  validation is why `ErrConfig` should be unreachable at request time -- which
  is exactly why it must not hide when reached.
- `DedupHint` and `files.embedItem` swallow every embed error on purpose (dedup
  is advisory and must never block `memory_write`; indexing is best-effort with
  a hash-retry). Leave them.

### Background update check and automatic updates (`internal/update`)

The daemon asks GitHub which releases exist and tells the owner. It never
installs in-process. On an install the installer made (`update.Detect` kind
`installer`), with an updater spawner wired (`cmd/seamlessd/update_spawn*.go`,
on a release build only: `updateSpawnerFor`) and `update.auto` allowed, the
checker picks the release (`update.Target`: soaked by GitHub's clock, carrying
the signed checksums bundle, not blocked or held) and the moment
(`update.Decide`: idle; else past `max_defer` at a 90s lull, forced at 1.5x,
overdue at 2x). It then hands one attempt to `seamlessd update --auto`, which
verifies and runs the release's own installer, confirms the new daemon by
observation and rolls back when it does not come up. The checker folds
`attempt.json` into blocks, backoff, a hold and a pause, and never writes
attempt files. Every other install only notifies. Owner decisions: memory
`auto-update-architecture-and-policy`.

- **Only a release build counts.** `main.distribution` is stamped `release` by
  `.goreleaser.yaml` alone (`buildinfo_test.go` fails if another build script
  stamps it; the darwin build reads it from there), and
  `update.IsReleaseBuild` also wants a clean X.Y.Z version. A
  source build -- `make run`, `make install`, the fixtures, seambench, CI --
  never asks GitHub unless `update.check: true`, never writes
  `<data_dir>/update/state.json`, gets no updater spawner, and is never an
  installer install (constraint `dev-and-fixture-daemons-never-self-update`).
- **A release is built in two places, from one config.** goreleaser builds
  linux and windows in release.yml; the darwin binaries and the Touch ID
  approver are built, Developer ID-signed and notarized on the owner's Mac
  (`scripts/release/macos.sh`, through `cmd/releasekit`, which builds and packs
  `.goreleaser.yaml`'s own builds and archive and refuses any key it does not
  implement, so a config change cannot make darwin differ silently). They
  reach the release as the tag's draft; release.yml holds every draft asset
  to the sha256 manifest in the signed tag (`releasekit check-darwin`) before
  `checksum.extra_files` lists it, behind the owner's approval of the `release`
  environment. goreleaser leaves the release a draft; the owner signs its
  `checksums.txt` with a hardware key (`scripts/release/sign.sh`,
  `checksums.txt.sshsig`), and release.yml's publish job, behind a second
  approval, publishes only once `releasekit verify-owner -require` accepts it.
  CI holds no secret, and the Homebrew cask is pushed
  by the owner (`scripts/release/cask.sh`). The site is published from a tag
  too (`pages.yml`: the release's own, or an admin-only `site-*` tag), never
  from a main push, so main alone cannot change the installers it serves.
  Keep the darwin assets' names and layout goreleaser's: the installers, the
  updater and `seam touchid enroll` read them
  (`TestDarwinArchive_IsLaidOutAsV082`, `TestRenderCask_IsGoreleasersCask`).
  Steps: `scripts/release/README.md`.
- **Owner-signed releases (`checksums.txt.sshsig`, `internal/sshsig`).** The
  owner's SSHSIG over `checksums.txt` (namespace `seamless-release`), which
  lists every archive, the macOS assets and both installer scripts. Which keys
  may sign is `release_signers` at the repository root: the publish job, cask.sh
  and `releasekit owner-keys` require a key listed at both the previous
  release's tag (the newest plain `vX.Y.Z` below this one) and this tag, so a
  key added in a release signs from the next one on and a removed key stops at
  once; the bootstrap (no file at the previous tag) is decided by this tag's
  file alone, loudly. `docs/install` and `docs/install.ps1` carry a copy, which
  `site-check` assertion 14 and
  `TestInstallers_CarryTheRepositorysReleaseSigners` hold to the file, and
  require the signature for every release after `OWNER_SIGNED_AFTER`; an
  ssh-keygen that fails the embedded self-test only warns. On the client the pin
  is opt-in: `update.release_signers` makes every update verify it in-process
  (`releasesig.VerifyOwner`) -- automatic, Update now, `seamlessd update`, the
  rollback target and the system updater -- and the installer it runs must be
  one `checksums.txt` lists. Verification is `internal/sshsig`, Go only: never
  shell out, never honor or require `verify-required` on a line (ssh-keygen
  rejects it), refuse `cert-authority`, `valid-*` and unknown options, and
  require sk flags 0x05 from the signature, which ssh-keygen does not (it
  accepts flags 00). Under peer_auth the pin file must be root's through root's
  directories (`filesystemProblems`). The workflow is read from the tag it
  releases, so the release-side gate keeps the path honest; the guarantees
  against a hostile tag pusher are the pin and the installers.
- **Text from parsed versions only.** Every notice, banner, row and event
  payload is built from `update.Version` values, `update.ReleaseURL` and the
  fixed `update.Hint` for the install kind -- never from the release API's
  `tag_name`, `name`, `html_url` or `body` (constraint
  `update-surfaces-render-parsed-versions-only`). The briefing line is worded
  as an owner action and must survive `sanitizeField` unchanged; the status
  tests pin both.
- **One home for the words.** Automatic-update wording lives in
  `internal/update/words.go` (`BlockWords`, `PauseWords`, `SpanWords`,
  `Plural`, `HeldBack`). The console, doctor and `update --check` frame those
  words and never keep their own copy; new wording goes there.
- **The more restrictive setting wins.** `update.Effective` merges the
  file/env `update:` block with the console's `update_config` row the other
  way round from the briefing and features rows: an explicit file/env
  `check: false` is final and locks the console toggle; the console may always
  turn checking off. `check: false` means no update traffic at all.
  `auto: false` is final the same way; `Settings.Auto` is false whenever
  `Check` is (`SourceCheck`), and it is a permission, not a promise: only an
  install with `Install.NotifyOnly()` false acts on it. The updater re-reads it
  before an automatic attempt (`autoUpdateAllowed`).
- **One writer.** The checker's loop goroutine owns the state; `CheckNow`,
  `Refresh`, `ApplyNow` and `Resume` are commands it serves, and readers get a
  copied `Status`. It schedules on wall-clock deadlines saved in the state (a
  1-minute tick compares them), so sleep cannot stall it, and it saves a
  pessimistic `next_check_at` BEFORE each request, so a crash loop cannot
  hammer GitHub.
- **Wall time only.** The checker strips the monotonic reading from every
  `Now()` (`update.New`; the monotonic clock stops during sleep). The soak and
  the `max_defer` deadline run on GitHub's clock (`serverClock`) once a check
  has carried a `Date`, and a spawn always rests on a release list fetched
  within the last minute (`recheckFresh`).
- **Never spawn without the record on disk.** `State.Spawn` is saved before
  `Spawner.Spawn` (`Checker.spawn`), and `update.started` is recorded between
  the two. A state file that cannot be saved means no spawn (`WaitSave`).
- **Manual attempts are shown, never acted on.** Only `WhyAuto` and `WhyNow`
  attempts feed blocks, backoff and the pause (`spawnedByDaemon`); only
  `WhyAuto` rollbacks count toward the two-rollback pause. A `seamlessd update`
  run by hand (`WhyManual`) is folded as the last attempt and nothing more.
- **Attempt files are a frozen, additive format.**
  `<data_dir>/update/attempt.json` and `attempts.jsonl`
  (`internal/update/attempt.go`) are read by every release from the floor
  (v0.7.0, the first with automatic updates) on, in both directions: add a
  field, never rename, retype or repurpose one, and there is no schema number
  to bump. Writers validate strictly (`ErrInvalidAttempt`); readers ignore
  unknown fields and skip a history line that does not decode. Only the holder
  of `update.LockPath` writes them; the daemon only reads. The Codex
  `hooks.json` fingerprint (`codexHooksFingerprint`) is frozen the same way.
- **Act on typed reasons, never on error text.** An attempt's `Error` is the
  updater's free text for the owner's eyes; no surface branches on it. A gate
  refusal is `Attempt.Refusal`, a fixed word `gates()` sets through
  `refusalError`; surfaces read it with `AttemptResult.Refused()` and render
  `update.RefusalWords`/`update.RefusalAction`, which return "" for a word this
  release does not know (a plain failure). A release-signers refusal is
  `Attempt.Signature` (`update.Signature*`, set only at StageVerify, attended
  runs too): surfaces read it with `AttemptResult.SignatureRefused()` and render
  `update.SignatureWords`; the fold blocks the release as `BlockSignature` and
  keeps `State.SignatureRefusal`, and `update.failed` carries it as `signature`.
  An unusable pin file is the gate refusal `release_signers`.
- **The updater runs outside the service, per OS.** The daemon starts
  `seamlessd update --auto` only through its `update.Spawner`
  (`update_spawn_<os>.go`): a Setsid child on darwin, a `systemd-run --user`
  transient unit on Linux, a detached `CREATE_NO_WINDOW` child on Windows with
  the on-demand Scheduled Task `SeamlessUpdate-<SID>` as the fallback, and no
  spawner elsewhere. Never spawn it as a plain child, never tie it to the
  daemon's context, and never hold `update.lock` in the daemon
  (`updaterLockHeld` only try-locks it and lets go). The updater refuses to run
  inside the service (`outsideServiceCheck`), and `seamlessd uninstall`
  removes what the spawners leave (`serviceTeardown`).
- **Not `config.HTTPClient`.** The release fetcher is an anonymous request to
  a public host: default TLS, proxy from the environment, https-only
  redirects, fixed User-Agent, never a token. `config.HTTPClient` stays the
  constructor for dials to a Seamless daemon.
- **Startup order.** `runServe` binds the listener, THEN starts the checker,
  THEN serves: the checker records this process as the running daemon, which
  must never describe one that lost the port. `/healthz` serves the process's
  `instance` id for the same reason.
- **Detection is a pure function.** `update.Detect` classifies from an
  `update.Probe`. `cmd/seamlessd/install_probe.go` gathers it through an
  injected `installProbeEnv`, and its parsers (plist, systemd unit, Task
  Scheduler XML, cgroup) are tested on Linux with fixtures. The Windows system
  calls behind gate 9 (`parentImageName`, `isCurrentUser`) live in
  `update_selfcheck_windows.go`, with stubs in `update_selfcheck_unix.go`.
  Compare paths with `os.SameFile`, never strings.

### Backups and restore (`internal/backup`)

Settings > Backups and `seamlessd backups`/`restore`. Guide:
docs-src/guides/data.md; owner decisions: memory
`console-backups-owner-decisions`.

- **A restore never reduces the locked set or loosens a fence.**
  `store.ReadKeptState` reads today's state before anything moves, and
  `store.ApplyKeptState` writes it into the restored database before the swap:
  `RestoreKeptSettings` and the `keptTables` (approvers, enrollment codes,
  repo_map) become exactly today's, console locks are the backup's plus
  today's (today's verbatim), and each project keeps the stricter of its two
  isolations. A new settings key goes into `RestoreKeptSettings` or
  `restoredSettings`, or `TestSettingsKeys_EveryKeyIsClassifiedForRestore`
  fails.
- **No route stages a restore under a peer_auth lock without
  `approval.FinishRestore`** (`restoreGated`: a force lock, a lockable project,
  or a console lock; an unreadable lock table counts as locked). `/restore/begin`
  is cookie-only, and the digest is the server's own read of the file, read
  again at finish. The CLI's proof is the daemon's account, never root.
- **The live data dir changes only through the recorded swap.** Staging runs
  while serving and writes only under `restore/<id>/`. `applyPendingRestore`
  (`backup.Applier`) runs in `runServe` after `lockDataDir` and before
  `store.Open`; renames happen only
  after `pending.json` says `swapping`, `applied` is the commit point, and a
  rollback that cannot complete is serve's error. Nothing under `restore/` is
  deleted before its data is verified elsewhere. While `pending.json` exists,
  the updater refuses (`restore_pending`) and the checker holds.
- **Pruning touches only this install's `-auto` backups** (manifest
  `InstallID`), never manual, before-restore, pre-update or another install's
  files. Startup sweeps only `.staging-*` and the service's own partials, never
  the updater's `.export-*`.
- **One writer, bounded shutdown.** The service's loop goroutine owns
  `.schedule.json` and serves commands; a run never holds a live database
  handle while it writes to the destination, so `Wait` (bounded) can abandon a
  stalled cloud or network write.
- **Under peer_auth a backup is the daemon's alone**: `0600`, owned by its
  uid, in a folder only it and root can change (`backup.Guard`). The folder
  check answers "inside Seamless's data folder" before any other probe for any
  path in the data dir but `backups/`, so it is no oracle into the corpus.

### The setup front door (`cmd/seamlessd/setup*.go`)

`seamlessd setup` decides how a computer uses Seamless -- Standalone, or
Networked: the Server or a Client of it (plan networked-relay). The installers
run it on the release they just verified, before they install anything, and
act on its answer; the binaries, the hooks, the per-user service and the
health check stay theirs.

- **Unattended means unchanged.** Setup asks only a person at a terminal with
  no `SEAMLESS_*` set but `SEAMLESS_CONFIG` (`setupRun.asks`). The installers
  apply the same rule (`scripted_run`, `Test-ScriptedRun`) and do not even run
  setup unless a setup knob (`SEAMLESS_SETUP`, `SEAMLESS_PAIR_CODE`,
  `SEAMLESS_IMPORT`, `SEAMLESS_IMPORT_LOCKS`, `SEAMLESS_SERVER_NAME`,
  `SEAMLESS_TLS_CERT`, `SEAMLESS_TLS_KEY`) is set or that person is there.
  Every updater run sets
  `SEAMLESS_VERSION` and `SEAMLESS_INSTALL_DIR`, so no update starts it; never
  add a setup knob to `installerKnobs`. `TestSetupInstaller_UnattendedRunsAreUnchanged`
  runs docs/install for real and holds its `seamlessd` calls to the old ones.
- **Setup only adds an answer.** The installers keep their own client decision
  (`SEAMLESS_SERVER_URL`, a config's `role: client`) and stop if setup's
  state disagrees; its `role=client` counts only where they had none -- a
  Client set up at the terminal, whose config setup wrote with
  `config.EnsureClientConfig`, so the key never rides in argv.
  `installerConfigValue` reads the config as `config_value` does
  (`TestInstallerConfigValue_IsTheInstallersRead` runs the sh).
- **The kinds' words live in `setupWords`.** Messages spell a kind only
  through `setupText`'s `{standalone}`, `{networked}`, `{server}` and
  `{client}`, and the installers never spell one; `TestSetupWords_OnePlace`
  renames every kind and fails on a stray spelling.
- **A branch step plugs in as a `setupSteps` entry** (its `run`, phase and
  knob); the question tree, the state file (`setupStateHeader`) and the
  hand-off (`setupProtocolLine`, `--state`, `--finish`) stay as they are. An
  unbuilt step's knob refuses the run. A built step that changes the daemon's
  config must not run inside `seamlessd update`, whose confirmation dials the
  config it loaded before the install.
- **The import step restores only into an empty data dir, and first**
  (`setup_import.go`). It is a first install's before step, Standalone and
  Server only: the backup `SEAMLESS_IMPORT` names, or the one a person at the
  terminal picks, is read whole by `archive.Verify`, then imported with
  `seamlessd import`'s own options (`archiveImportOptions`) and lock gate,
  which asks only the person setup may ask; with no one to ask, locks no
  passkey can open stop the run until `SEAMLESS_IMPORT_LOCKS` answers, as the
  flags answer the command. It refuses a re-run, a Client, a data dir
  `archive.DetectMode` does not call fresh, a daemon answering where the new
  one will, and `SEAMLESS_DATA_DIR`. With peer_auth the import comes first and
  locked projects after: the engine's (or the guide's) data-dir move carries
  what it restored. Its tests inject the config (`setupRun.loadConfig`) and
  never dial (`setupRun.daemonUp`); the installer helper refuses a data dir
  outside the world's home.
- **The address step writes a first install's config, and no other**
  (`setup_address.go`). The Server's before step: the name (this computer's
  MagicDNS name when the tailnet issues it a certificate, via `tailscale cert`;
  else a domain's or LAN name's own pair, checked as a pair for the name with a
  key no other account reads), and the bind that holds every address the name
  resolves to, picked by `boundAddrs` (serve's pairing, doctor's read): one IPv4
  address of the name with the IPv6 serve pairs beside it, else the wildcard;
  a name resolving to nothing, loopback, or an address this computer does not
  hold is refused. `config.EnsureServerConfig` writes it with the bearer key
  after every before step (`setupWriteServer`), so a stopped import leaves no
  config to make the retry a re-run, and only when nothing is installed, so
  never inside `seamlessd update`; every other run prints the lines, and under
  peer_auth root's steps. Knobs `SEAMLESS_SERVER_NAME`, `SEAMLESS_TLS_CERT`,
  `SEAMLESS_TLS_KEY` (not `SEAMLESS_TLS_*_FILE`, which the service never sees).
  Tests fake the resolver, interfaces and tailnet (`setupAddrArrange`): no test
  runs `tailscale` or resolves a name.
- **Retiring the key judges every step from what is there**
  (`setup_retire.go`, the `retire-key` change, offered on a Standalone install
  with locked projects). It reads the daemon over the owner's socket (`GET
  /console/signin/state`), never the daemon's config; prints each root step
  and waits for its effect; removes nothing before a passkey sign-in recorded
  in this run (until the key is gone); and takes the key out of
  `console.yaml` only once the daemon serves without it. A retired key is
  `retiredKey` (peer_auth, `console.sign_in: passkey`, no `mcp.api_key`): MCP
  over TCP then answers 403 with the socket's address, and doctor reports it as
  meant. Bringing a key back mints a fresh one, written to `console.yaml`
  (marked, so a stopped run resumes the wait) and never printed. Tests fake the
  daemon (`retireEnv`): none dials, runs sudo, or reads the real home.
- **dash.** Debian's and Ubuntu's `/bin/sh` is dash, where a failed
  redirection on a special built-in (`: </dev/tty`) ends the script: probe in
  a subshell (`tty_available`). The installer tests run every scenario under
  `/bin/sh` and `/bin/dash`.

### Release discipline: the previous release must still run

An automatic update rolls back to the release it replaced when the new one does
not come up, and an owner can pin an older release (`SEAMLESS_VERSION`). The
rollback restores the database only when the new release had migrated it, and
never touches memories, notes or the config. A restored database -- the
rollback's or `seamlessd import`'s -- ends every live access lease, console
unlock and read grant it held (`store.ResetRestoredAccess`): a snapshot would
otherwise reopen what the owner closed after it was taken, so when that fails
the rollback puts the migrated database back instead of serving it. A rollback
also never reduces the set of locked projects: it puts the backup's database in
place only if the extracted snapshot holds every console lock the migrated one
does (both read before any file moves), and otherwise, or when either side's
locks cannot be read, the migrated database stays. These rules exist so a rollback
to the previous release keeps working; the last keeps an update from silently
disabling Codex's hooks.

- **Migrations stay additive.** Add tables, columns and indexes; never drop,
  rename or retype what the previous release reads. A rollback whose database
  restore fails, and every deliberate downgrade, leaves the previous release
  on the newer schema (`serve` and doctor's `schema version` row warn), and it
  must still work.
- **Startup file rewrites stay readable by the previous release.** A file a
  daemon rewrites at startup (`update/state.json` is one) only gains fields,
  and its readers ignore fields they do not know.
- **New frontmatter fields are optional.** The previous release reads files a
  newer one wrote and keeps unknown keys across a rewrite (`Extra` in
  `internal/files/frontmatter.go`), but writes new files without the field.
- **Config keys are only ever added.** The updater never edits the config and
  `config.LoadFrom` decodes with `KnownFields`, so a renamed or removed key makes
  the new release refuse a config the previous one accepted: its daemon never
  comes up, and every update to it rolls back.
- **A release that changes Codex hook definitions says so in its notes.**
  Codex runs a changed hook only once it is re-approved in Codex's `/hooks`;
  the updated daemon says so only on its own machine, after the fact.

### Benchmark scenarios and graders (`internal/bench`, `cmd/seambench`)

The agent-scenario benchmark produces a NUMBER the owner acts on, so its
invariants are about the number meaning the same thing on every arm and in six
months. Workflow and flags: `cmd/seambench/README.md`. (`make seambench`, not
`make bench` -- the latter is the unrelated ns/op micro-benchmarks.)

- **The run directory is the whole handoff.** `run` writes artifacts and grades
  nothing; `report` reads artifacts and runs nothing. A grader takes a
  `RunArtifacts` whose every path points inside one preserved run dir -- never a
  live arm, never the runner's process state. Keep it that way: it is what makes
  `report --regrade` re-apply a grader fix to runs that already cost their
  tokens, and what lets a run tree be graded on another machine. The layout is
  frozen in `internal/bench/artifacts.go`; adding a file is fine, repurposing one
  breaks the other half. Never read a run's coordinates out of its path -- the
  `run.json` manifest carries them (a version comparison nests one level deeper).
- **`Metrics` has two disjoint halves.** The runner owns turns/tokens/cost/
  duration; the grader owns everything derived from the artifacts.
  `MergeMetrics` recombines them FIELD-WISE, not "whichever is non-zero", so an
  agent that genuinely made no tool calls stays at zero.
- **Gate vs observed.** Every check reports; only a `gate: true` check can fail
  the run. Outcome checks (the repo-state assertions) gate on every arm. Of the
  event-log checks, only DEFECTS gate -- the mechanism failing to fire at all,
  or a durable write landing outside the scenario's project. "The agent read the
  memory / moved the plan step / left a finding" is recorded and measured but
  MUST NOT gate: gating it would fail a Seamless arm that solved the task while
  the vanilla arm doing the same thing passes, which understates the very uplift
  being measured. The LLM judge never gates; its absence degrades the run
  (constraint `llm-degradation-remote-vs-local`), it does not fail it.
- **Only the condition dimension may run concurrently.** The N runs of a cell
  share an arm and re-establish its starting state (git-restore + wipe and
  re-seed) on every run, so overlapping them would reset each other mid-flight;
  scenarios stay serial so a systemic failure surfaces after one scenario rather
  than after the whole token-metered matrix. Condition arms own separate repos,
  data dirs, HOMEs and ports, which is why `--parallel-conditions` is safe --
  and why a run made under it is stamped `Concurrent`, since its wall clock
  carries contention the serial runs it would be compared against do not.
- **Three outcomes, never two.** `graded` / `failed to run` / `ungradeable`.
  Only graded runs enter a pass-rate or a trial verdict; an infrastructure flake
  recorded as a `fail` is how a later reader mistakes it for a regression. An
  unrecognized status falls to ungradeable, not to a verdict. Rates are
  `bench.Rate` (passed + graded) so a rate can never be quoted without its
  counts.
- **Never string-match briefing layout.** Not in a grader, not in a seed test.
  The briefing is the fastest-churning surface in the repo AND is itself prime
  regression surface for this benchmark, so a layout assertion both breaks
  constantly and measures the wrong thing. Assert DB rows, event-log entries,
  and stable markers instead: memory names, tool names, task ids, session
  findings, project scope. `internal/bench/eventlog.go` is the vocabulary, and
  the seed tests assert through store queries and files on disk.
- **Seeds write to the data dir, never into the demo repo working tree.** The
  repo must stay Seamless-free (memory `scene-demo-repo-must-be-seamless-free`)
  or the vanilla arm reads its way out of character; the `full` arm's CLAUDE.md
  block is the one deliberate exception and the harness writes it. A `SeedFunc`
  gets a `*demokit.Seeder` already opened on a throwaway dir -- never point
  `demokit.New` at a live instance.
- **Bench scenario fixtures are FORKED from `internal/demokit`'s scene specs, not
  imported.** The scene defs are branding surface that must stay stable while
  this suite churns. Reuse demokit's seeding PRIMITIVES; copy its data.
- **The arm env file is the only interface to the shell harness.** `harness.sh
  --mode bench` owns what an arm contains (throwaway HOME, demo-repo copy, data
  dir, config, key, port, `install-hooks --client claude` under the arm's own
  HOME); the runner reads that env file and reimplements none of it -- in
  particular it never runs `install-hooks` itself, which is what keeps the live
  `~/.claude`/`~/.codex`/`~/.seamless` out of reach (memory
  `fixture-install-hooks-needs-home-override`). The condition grammar
  `name[:profile[:client]]` is parsed on both sides on purpose; a mismatch
  between the requested and the built arm is fatal, not adaptive.
- **One live write, and it is the results trial.** Everything else seambench
  touches is throwaway. `results.json` is written BEFORE any trial, an
  unreachable instance is a warning with a retry line rather than an error, and
  recording is idempotent per run (the trial id is stamped into `grade.json`).
  Do not add a second live write, and pass `--no-trials` for dry runs -- fake-agent
  results in the live lab are indistinguishable from measurements later.

## Common pitfalls (checklist before declaring done)

### Meta-rules

1. **Propagate every fix.** When you fix a buggy pattern, grep the repo for other
   instances and fix them all in the same change.
2. **After any interface/store/schema/migration change, run `make build && make test`.**
   Update mocks/fakes for changed signatures in the same change.
3. **No fake results on error.** Never swallow an error and return a plausible
   dummy value; the LLM cannot distinguish it from a real result. Return the error.

### Forbidden APIs

| Pattern | Why | Use instead |
| --- | --- | --- |
| `ulid.MustNew` | Panics on entropy failure. | `core.NewID()` (`ulid.New(ulid.Now(), rand.Reader)` + return the error). |
| `os.WriteFile` on a `.md` file | Non-atomic; a crash mid-write corrupts the source-of-truth file. | `files.AtomicWrite` (temp file in same dir + fsync + rename). |
| `_, _ = time.Parse(...)` (discarded error) | Silently yields zero-value timestamps. | Capture the error, `slog.Warn`, do not emit zero times. |
| `err == ErrXxx`, `err == sql.ErrNoRows` | Breaks when wrapped. | `errors.Is(err, ErrXxx)`. |
| `_ = json.Marshal/Unmarshal(...)` | Marshal can fail; Unmarshal silently zeroes. | Check the error; warn and propagate. |
| Unchecked `RowsAffected()` on UPDATE/DELETE | Not-found looks like success, and a driver failure looks like not-found. | Check both: the error, then `if n == 0 { return ErrNotFound }`. (`errcheck`) |
| `close(ch)` outside `sync.Once` in `Close()` | Double-close panics. | `closeOnce.Do(func(){ close(done) })`. |
| `a + "/" + b` for filesystem paths | Breaks portability/traversal. | `filepath.Join`. |
| `context.Background()` in a handler/goroutine | Leaks request scope, disconnects shutdown. | Derive from request ctx / `WithoutCancel`. |
| `os.Stat`/`WalkDir` following symlinks across user data | Leaks files outside the tree. | `os.Lstat`; skip `ModeSymlink` in `WalkDir`. |
| `strings.Contains(err.Error(), ...)` for control flow | Fragile across message changes. | Typed sentinels + `errors.Is`. |
| Returning `(nil, nil)` when a row is missing | Callers forget the nil check. | Return `(nil, ErrNotFound)`. |
| New `migrations/NNN_*.sql` without a `Migrations()` entry | Silently never runs. | Add the `go:embed` + `Migrations()` entry; verify on a fresh DB. |
| `time.Sleep` for synchronization | Flaky tests, masks ordering bugs. | Channels / `WaitGroup` / deadline poll. |
| Bare `default:` in a switch over caller-supplied input | The unrecognized value silently becomes the default -- a plausible dummy the caller cannot tell from a real result (meta-rule 3). `recall{scope:"memoires"}` searched everything and reported success. | Validate at the boundary and name the accepted values: `invalid %q: valid values are %s`. A *library* may keep a permissive default where the zero value legitimately means "unset" -- only the boundary can tell absent from wrong. |
| `mcp.Enum(...)` / `mcp.Required()` as the only guard | Advertisement, not enforcement: mcp-go's `handleToolCall` looks the tool up and dispatches with no schema pass. The declaration reads like a check and is not. | Declare via `enumOf(canonicalSet)`; `validateMiddleware` (`internal/mcp/argspec.go`) is what enforces it. |
| Hand-transcribing a canonical set into a schema, help string, or prompt | Drifts in silence. `tools_gardener.go` once listed all of `store.ProposalKinds` but one, so `abandon_plan` was undiscoverable for months while working fine everywhere else. | Derive from the canonical slice (`core.MemoryKinds`, `core.TaskStatuses`, `core.SessionSources`, `store.ProposalKinds`, `retrieve.RecallScopes`) + a same-package test asserting schema == set (`catalog_test.go`'s `TestArgsEnumsDeriveFromCanonicalSets`). |

### Required patterns

- **A default is only legitimate when a parameter is ABSENT, never when it is
  present-but-uninterpretable.** Absent -> default; present but uninterpretable ->
  error. One sentence covering every finding in the input-boundary sweep: a typo'd
  enum, a wrong JSON type, a misspelled parameter name, and `limit:0` are all
  *present*, so none of them may quietly become the default. This is the owner's
  standing "no automatic fallbacks for ambiguous requests" directive applied to
  arguments, and it is why `resolveWriteScope` refuses to guess a project rather
  than picking one.
- **Atomic markdown writes**: every `.md` write goes through the files layer's
  atomic writer (temp + fsync + rename), including rollback paths.
- **DB-then-file ordering**: operations touching both commit the DB transaction
  first, then perform the filesystem mutation in a post-commit step; undo partial
  filesystem state on rollback.
- **`rows.Err()` after every `for rows.Next()`** (enforced by `rowserrcheck`).
- **FTS5 MATCH sanitization**: user text fed to `MATCH` is sanitized (strip
  operators, quote terms) to avoid SQLite errors and injection.
- **LIKE escaping**: user input in `LIKE` escapes `\`, `%`, `_` in that order, with
  `ESCAPE '\'`; never apply LIKE-escaping to `=` comparisons.
- **`filepath.Join` everywhere**, including tests.

## Verification before declaring done

1. **`make check`** -- build + vet + cross-check + fmt-check + docs-check +
   installer-check + site-check + lint + vulncheck + test-race, in that order. This is the gate;
   the individual targets exist for iterating. `make seambench` is NOT part of
   it and must never be added: it spends real API tokens (see the benchmark
   section above).
2. Update `*_test.go` in the same change if a signature changed.
3. For any change touching a recurring pattern above, grep for siblings and fix
   them together.

CI (`.github/workflows/ci.yml`) runs `make check` on Linux, plus two jobs
`check` cannot stand in for: `os-smoke` runs `go test -count=1` over
`./cmd/seamlessd ./internal/update ./internal/agentproc` on `macos-latest` and
`windows-latest` (`CGO_ENABLED=0`, no `-race`, `fail-fast: false`; Windows
skips the names in `known_windows_failures`, so delete a name when its fix
lands), and `config-lint` runs actionlint and `goreleaser check` with the
release workflow's goreleaser range.

`make install-git-hooks` (once per clone) enables `.githooks/pre-commit`, which
runs **`make check-fast`** -- `check` minus build, vulncheck, and test-race (~3s
against ~39s). It is a
convenience, not the gate: git runs hooks against the working tree rather than
the index, so under partial staging it describes the tree you are in and not the
commit you are making. It catches gofmt/docs/lint drift early; `make check` and
CI still decide whether the work is done. Bypass with `git commit --no-verify`.

`make lint` catches `ulid.MustNew`, `time.Sleep`, missing `rows.Err()`,
`err ==` sentinel comparisons, and -- via `errcheck` with `check-blank` --
errors discarded into `_`. That last one is the guardrail for the meta-rule
above: `n, _ := res.RowsAffected()` in front of an `if n == 0 { return
ErrNotFound }` turns a driver failure into a confident "not found".

Every surviving discard is either listed in `.golangci.yml`'s
`exclude-functions` (structurally uninteresting: deferred `Tx.Rollback`, writes
to an already-committed HTTP response) or carries a `//nolint:errcheck` with a
reason at the site. There is no third category -- if you add a discard, say why
in one of those two places rather than reaching for a blanket exclusion.

Note for anyone touching gofmt: the go tool's `./...` skips dot-dirs, but gofmt
walks the filesystem, so a bare `gofmt -l .` descends into `.claude/worktrees/`
(other agents' checkouts) and reports their drift as yours -- and `gofmt -w .`
rewrites their files mid-edit. `make fmt` and `make fmt-check` scope to tracked
files; use them.

## Console (server-rendered)

The observability console is `html/template` + vanilla JS + SSE, served by
`internal/console` -- no node, npm, React, or build step. It is read-mostly: the
writes are the owner's overrides and curation actions, among them -- archive a
memory, approve a plan, force-release a task's claim lock,
ask/split/apply/dismiss/retarget a gardener proposal, save or reset the
briefing settings, switch update checks and automatic updates (or hand both
back to the config), check for updates now, update now, resume automatic
updates, save or reset the backup schedule and folder, back up now, delete a
backup, restore one (a passkey ceremony bound to the backup's digest and the
mode while peer_auth locks a project), cancel a staged restore or dismiss its
result, and -- with write screening on -- release or discard a held write,
clear a session's mark, switch lockdown, re-run the review, mark a flagged item
safe and accept a file changed outside Seamless. Keep pages self-contained and dependency-free so an agent can edit
them without a toolchain.
