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
    -> internal/{retrieve,lifecycle,gardener,files,capture,bench,demokit,update} (domains)
      -> internal/{store,events,llm,core,config,validate,access}   (foundations)
```

(`access` imports only `config`; `store` and `events` import `access`.)

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
  `127.0.0.1` by default. No JWT, no registration. The one other credential is
  the kernel's: with `peer_auth` on, a Unix-socket request is authenticated by
  its peer uid (see "Peer-user auth" below), and the TCP key is the owner.
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
- A caller that named its process is never inferred into a session stamped
  with a different one: `store.AmbientScope.Caller` filters every ambient
  fallback lookup. Once such a caller's own session has ended, the sole live
  ambient is by construction another agent's.
- The process binding revives a session the idle reaper EXPIRED (a call from the
  same live process is proof of life), never one that was COMPLETED on purpose.
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

### Peer-user auth (`internal/access`)

With `peer_auth` on, every request carries a `View`: the owner's on TCP
(`ownerHandler`), the kernel-named peer's on the socket (`peerConnContext` +
`peerHandler`, cmd/seamlessd/peerauth.go). Off, no request carries one, and
every check below answers yes -- which is how "off is exactly the old
behavior" holds. Read the guide (docs-src/guides/peer-auth.md) before touching
any of it.

- **The policy lives in `internal/access` and nowhere else.** `View.Visible`,
  `View.Writable` and `View.SessionScope` are the rules; surfaces ask them (or
  `access.Visible(ctx, p)`) and never compare principal kinds or slugs
  themselves. `store.CanRead`/`CanWrite` consult the View FIRST -- including
  for the caller's own bound project -- so every isolation fence inherits it.
- **The identity comes from the transport, never the client.** A header, a
  `session=` handle, a body field or an agent-process identity can only ever
  narrow what the View allows. `getBinding` drops a binding to an unreachable
  project; `sessionByRef`/`sessionByID` report another account's session as not
  found; the process binding, ambient inference and `soleAmbientByCWD` skip
  sessions the caller may not reach; hooks blank a session id that names one
  (`foreignSession`).
- **A configured account never writes the global scope**, and anything nothing
  else places lands in its home project (`peerHome`, `View.SessionScope`). It
  never grows the repo map (`cwdProject`).
- **Lists filter.** Anything that enumerates projects or sessions applies
  `access.Visible` (project_list, scope-error project lists, ambient
  candidates). A sealed project's name is not shown to an account that cannot
  reach it.
- **The console reads through `store.OpenView`**: TEMP VIEWs that shadow every
  project-scoped table on a read-only handle. Console writes go to
  `s.writeDB()`, after reading their target through `s.cfg.DB`; a gardener
  action by id checks `proposalReachable` first. A store function that both
  reads and caches takes both handles (`CaptureStreakWith` and its siblings).
  A new table must be classified in `viewTables` or `viewExempt`
  (`TestViewCoversEveryTable`), and the one table a view cannot shadow -- `fts`
  -- is filtered by the request's View in its only reader (`accessFilter`).
  Work done for no caller (the gardener, the reaper, milestones) is hidden by
  what it is about: an event recorded without a project is still hidden by its
  session, its item, or -- for the gardener's own events, whose payload carries
  the proposal key -- the proposal it concerns.
- **The event log files a configured account's events under its own project**
  (`events.refile` in `Record`/`RecordOnce`), so nothing it does lands in the
  global scope the owner reads. Never insert into `events` anywhere else; the
  console's live feed re-reads each event through the filtered handle
  (`liveEventVisible`).
- **No agent-disk reads under peer_auth.** `localDiskOK` answers no for every
  hook: the daemon runs as its own account, so every path a hook names was
  chosen by another account, and the daemon could be made to read its own data
  directory on a caller's behalf. Do not "restore" transcript harvest by
  opening caller-supplied paths.
- **Prove it in the matrix.** `cmd/seamlessd/peerauth_matrix_test.go` drives
  every tool in `mcp.Catalog()` (a tool without a probe row fails it), every
  console route (read from console.go), the hooks, A2A and the live feed, in
  both directions, and checks state after every write. A new surface gets a
  row there.

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
  stamps it), and `update.IsReleaseBuild` also wants a clean X.Y.Z version. A
  source build -- `make run`, `make install`, the fixtures, seambench, CI --
  never asks GitHub unless `update.check: true`, never writes
  `<data_dir>/update/state.json`, gets no updater spawner, and is never an
  installer install (constraint `dev-and-fixture-daemons-never-self-update`).
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
  release does not know (a plain failure).
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

### Release discipline: the previous release must still run

An automatic update rolls back to the release it replaced when the new one does
not come up, and an owner can pin an older release (`SEAMLESS_VERSION`). The
rollback restores the database only when the new release had migrated it, and
never touches memories, notes or the config. These rules exist so a rollback
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
back to the config), check for updates now, update now, and resume automatic
updates. Keep pages self-contained and dependency-free so an agent can edit
them without a toolchain.
