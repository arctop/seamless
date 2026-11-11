---
title: Contributing
description: The make targets, the check gate and what CI runs, the conventions that matter, the forbidden APIs, and the three places a new MCP tool must be wired.
---

The conventions live in `AGENTS.md` at the repo root, and that file is the source
of truth. This page is the orientation: what to run, what the gate checks, and the
handful of rules that are worth knowing before you write a line.

## The targets

```bash
make build      # ./bin/seamlessd and ./bin/seam, with build metadata stamped in
make test       # unit tests
make test-race  # unit tests under the race detector
make vet        # go vet
make lint       # golangci-lint
make fmt        # gofmt, over tracked files
make fmt-check  # report gofmt drift instead of fixing it
make docs       # regenerate docs-src/ -> docs/docs/ (committed)
make docs-check # fail if the committed site is stale
make check      # the gate - everything above, in order
```

`make help` prints the full list, including the launchd dev-loop and prod-install
targets.

## The edit-test loop: make install

`make install` (macOS-only; it renders the launchd plist from `deploy/launchd/`)
is also the dev loop. It reloads the service with a launchd bootout/bootstrap
every time, even when the rendered plist is unchanged: each build is a new
binary, and kickstarting the loaded job in place after its binary was swapped
runs it against the code-signing identity launchd cached, which macOS 26 kills,
leaving the daemon down for launchd's 10s throttle. Its marginal cost over
`make build` is two file copies and a restart of about a second and a half,
nearly all of it the new binary starting up.

```bash
make build      # compile only; nothing live changes
make install    # ...and now it does
make logs       # follow ~/.seamless/seamlessd.log
```

That split is the point. `make build` on a half-finished edit is free, because
the daemon and hooks keep running the last thing you installed. Rebuilding is not
deploying.

On a machine the curl installer set up, `make install` refuses before it writes
anything, because a source build there ends the release's automatic updates.
Develop there with `make build` and a scratch daemon (its own config, data dir
and port), or take the install over with `make install FORCE=1`.

Note that `make build` is not the same as `go build`: it stamps the commit and
build date into the binary via `-ldflags`, and those show up in `/healthz`, the
MCP handshake, and the startup log. A plain `go build` leaves them `unknown`,
which makes a stale running daemon invisible.

## The `check` gate

```bash
make check
```

That is the one command that must be green before work is done. It runs ten steps
sequentially - as separate `$(MAKE)` invocations rather than prerequisites, so it
stops at the first red step and stays ordered under `make -j`:

| Order | Step | Catches |
|---|---|---|
| 1 | `build` | It compiles. |
| 2 | `vet` | The stdlib's own suspicious-construct checks. |
| 3 | `cross-check` | `go vet` for darwin/arm64 and windows/amd64, so code behind an OS build tag type-checks before a release has to build it. |
| 4 | `fmt-check` | gofmt drift in tracked files. |
| 5 | `docs-check` | A committed docs site that no longer matches `docs-src/`. |
| 6 | `installer-check` | An installer script that does not parse (`sh -n`, and PowerShell's parser when `pwsh` is installed). |
| 7 | `site-check` | A hand-written landing page that drifted from the installer or the CLI. |
| 8 | `lint` | golangci-lint, including the repo's custom bans. |
| 9 | `vulncheck` | A known vulnerability in code this repo actually calls (govulncheck). |
| 10 | `test-race` | Everything, under the race detector. |

The cheap, likely-to-fail steps run first, and `test-race` is last because it is
by far the slowest. The individual targets exist for iterating; `check` is the
thing you run before you claim to be done.

`docs-check` regenerates into a temp dir and diffs, rather than rewriting your
working tree and running `git diff`. That means it never mutates the file you are
editing, and `diff -r` catches untracked drift that `git diff` cannot see - a page
deleted from `docs-src/` but still committed under `docs/docs/`. Two docs pages
are generated from the code (the MCP tool reference reads `mcp.Catalog()`, the
configuration reference reflects `config.Defaults()`), so changing a tool or a
config key makes the committed output stale, and this is the step that says so.

## What CI runs

`.github/workflows/ci.yml` runs three jobs on every push to `main` and every
pull request:

- **`check`**, on Linux: `make check`, the gate above.
- **`os-smoke`**, on `macos-latest` and `windows-latest`: `go test -count=1`
  over `./cmd/seamlessd`, `./internal/update` and `./internal/agentproc`. These
  are the packages with code behind darwin and windows build tags (file
  locking, the detached updater, the process table), which `cross-check` only
  vets. It builds with `CGO_ENABLED=0`, as the release does, and without
  `-race`. Each OS reports on its own (`fail-fast: false`). Tests that fail on
  Windows because of a known product bug are skipped there by name
  (`known_windows_failures` in the workflow); delete a name when its fix lands.
- **`config-lint`**: actionlint over the workflows, and `goreleaser check` over
  `.goreleaser.yaml` with the goreleaser version range the release workflow
  uses. Without it, a broken release config would first fail on a pushed tag.

Since those packages' tests run on Windows, write them to pass there:

- A test that fakes a home sets `USERPROFILE` beside `HOME`. `os.UserHomeDir`
  reads `USERPROFILE` on Windows, so a test that sets only `HOME` writes to the
  real profile of whoever runs it.
- Windows reports file modes as `0666`, so an assertion on `0600` or `0700`
  goes behind `if runtime.GOOS != "windows"`.
- A POSIX fixture path such as `/opt/seam` is not absolute on Windows.
  `absFixture` (`cmd/seamlessd/desktop_mcp_test.go`) makes it absolute on the
  OS under test.
- Expect the OS's path separator: build expected paths with `filepath.Join`,
  never with `/`.

## Use `make fmt`, never `gofmt -w .`

This one bites in a specific way, so it is worth stating plainly.

The go tool's `./...` pattern skips dot-directories, so `build`, `vet`, `test`,
and `lint` never see `.claude/worktrees/` - other agents' checkouts of this same
repo. **gofmt takes paths, not packages, and walks the filesystem raw.** So:

- `gofmt -l .` descends into those worktrees and reports *their* drift as yours.
- `gofmt -w .` **rewrites their files underneath them, mid-edit.**

`make fmt` and `make fmt-check` scope to tracked files (`git ls-files '*.go'`).
Always go through them.

## Style essentials

- **Go 1.26+, no CGO.** Pure-Go SQLite (`modernc.org/sqlite`). Prefer modern
  stdlib (`slices`, `maps`, `strings.SplitSeq`, iterators) in new code.
- **No emojis in code or comments. No attribution lines** in commits, PRs, or
  code.
- **Package layout is consistent** across `internal/`: `service.go` for the
  package's public API, `store.go` for SQLite access, `handler.go` for an HTTP
  surface, `{feature}.go` for pure functions, `*_test.go` alongside.
- **Interfaces are role nouns** - `Store`, `Recorder`, `Embedder`, not `IStore`.
- **Constructors are `New{Type}(deps) *Type`**, or `(*Type, error)` when they can
  fail.
- **Domain errors are package-level sentinels** (`var Err{Condition} =
  errors.New(...)`), checked with `errors.Is`.
- **IDs are ULIDs**, via `core.NewID()`. Never UUID.
- **Wrap errors with context and `%w`**: `fmt.Errorf("pkg.Service.Method: %w",
  err)`. Reserve `fmt.Errorf` for wrapping; sentinels come from `errors.New`.
- **Either log or return an error, never both.** Fatal to the operation → return
  it. Non-fatal (graceful degradation) → `slog.Warn`/`Debug` and continue.
- **`log/slog`, and never log memory or note bodies** - IDs only.
- **Every service and store method takes `context.Context` first.** Never
  `context.Background()` in a handler or in a goroutine spawned by one: derive
  from the request context, or use `context.WithoutCancel` for work that must
  outlive it.

## Database rules

- **WAL and foreign keys are set by Go on connection open**, never in migration
  SQL. One `seam.db` at `{data_dir}/seam.db`, with `SetMaxOpenConns(1)`.
- **Migrations are numbered SQL files** under `internal/store/migrations/`,
  embedded with `go:embed` and registered in the `Migrations()` list. Each runs
  once, inside a transaction, tracked in `schema_migrations`.
- **Never edit an applied migration.** Append a new numbered one.
- **A new `.sql` file needs both a `go:embed` line and a `Migrations()` entry**,
  or it silently never runs. This is the failure mode that looks like a working
  build and a mysteriously absent column.
- **Files are the source of truth** for `memory/` and `notes/`. The `*_index`
  tables and FTS are rebuildable mirrors, kept in sync by the files watcher and
  startup reconciliation; `content_hash` is what lets an unchanged file be
  skipped.
- **Embeddings are little-endian float32 BLOBs** in the `embeddings` table, and
  similarity is brute-force cosine in Go. Do not add a vector database.
- **The unified FTS5 table (`fts`) spans both halves of the corpus**, with
  `fts.kind` naming which. It is managed with explicit INSERT/DELETE - not
  triggers, because it is not an external-content table - and each half is
  maintained next to its own writer: the file-backed knowledge kinds (memory,
  note) from the files layer, the DB-native work record (task, trial, session
  findings) from `store/index_work.go`.
- **The work record carries no embeddings.** There is no `content_hash`
  reconcile loop behind a DB row, so vectorizing one would put a provider
  round-trip inside every `tasks_add` and leave older rows unbackfillable. Those
  kinds are lexical-only by design.

## Testing rules

- **`testify/require`** (fail fast), table-driven for multi-case functions. This
  overrides any global "stdlib testing only" preference: the ported code and this
  project standardize on testify. Stay consistent.
- **SQLite tests use a fresh on-disk DB** in `t.TempDir()`, or a *named* in-memory
  DB (`file:{t.Name()}?mode=memory&cache=shared`). Never the unnamed shared-cache
  URI.
- **Migrations are exercised against a fresh DB.**
- **External services are mocked with `httptest.NewServer`.** A unit test never
  hits a real network service.
- **No `time.Sleep` for synchronization.** Channels, `sync.WaitGroup`, or polling
  with a deadline. This one is enforced by lint.
- **Tests are named `Test{Unit}_{Scenario}`**, or table-driven with `t.Run`.

## Forbidden APIs

Each row is a pattern that compiles, looks fine in review, and is wrong.

| Pattern | Why | Use instead |
|---|---|---|
| `ulid.MustNew` | Panics on entropy failure. | `core.NewID()` |
| `os.WriteFile` on a `.md` file | Non-atomic; a crash mid-write corrupts a source-of-truth file. | `files.AtomicWrite` (temp file in the same dir + fsync + rename) |
| `_, _ = time.Parse(...)` | Silently yields zero-value timestamps. | Capture the error, `slog.Warn`, do not emit zero times. |
| `err == ErrXxx`, `err == sql.ErrNoRows` | Breaks the moment the error is wrapped. | `errors.Is(err, ErrXxx)` |
| `_ = json.Marshal/Unmarshal(...)` | Marshal can fail; Unmarshal silently zeroes. | Check the error; warn and propagate. |
| Unchecked `RowsAffected()` on UPDATE/DELETE | Not-found looks like success, and a driver failure looks like not-found. | Check the error, *then* `if n == 0 { return ErrNotFound }`. |
| `close(ch)` outside `sync.Once` in `Close()` | Double-close panics. | `closeOnce.Do(func(){ close(done) })` |
| `a + "/" + b` for filesystem paths | Breaks portability and traversal guards. | `filepath.Join` |
| `context.Background()` in a handler/goroutine | Leaks request scope, disconnects shutdown. | Derive from the request ctx, or `WithoutCancel`. |
| `os.Stat`/`WalkDir` following symlinks across user data | Leaks files outside the tree. | `os.Lstat`; skip `ModeSymlink` in `WalkDir`. |
| `strings.Contains(err.Error(), ...)` for control flow | Fragile across message changes. | Typed sentinels + `errors.Is` |
| Returning `(nil, nil)` for a missing row | Callers forget the nil check. | Return `(nil, ErrNotFound)`. |
| New `migrations/NNN_*.sql` without a `Migrations()` entry | Silently never runs. | Add the `go:embed` + `Migrations()` entry; verify on a fresh DB. |
| `time.Sleep` for synchronization | Flaky tests; masks ordering bugs. | Channels / `WaitGroup` / deadline poll. |

`make lint` catches the grep-detectable subset: `forbidigo` bans `ulid.MustNew`
and `time.Sleep`, `rowserrcheck` catches a missing `rows.Err()`, `errorlint`
catches `err ==` sentinel comparisons, and `errcheck` runs with `check-blank: true`
so errors discarded into `_` are reported too.

`.golangci.yml` is schema **v2**, so `make lint` needs golangci-lint v2 - note the
`/v2` in the module path, since v1's path installs a v1 binary:

```bash
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0
```

The two schema generations are mutually unreadable, so a mismatched binary does
not lint badly - it refuses to start, and the gate silently stops being a gate.
`make lint` checks the major version up front and says so rather than letting the
raw schema error surface. CI pins the same version; bump both together.

That last one is the guardrail worth understanding. `n, _ := res.RowsAffected()`
in front of `if n == 0 { return ErrNotFound }` turns a driver failure into a
confident "not found" - the caller believes it, and there is no way to tell the
two apart afterwards. Every surviving discard is either listed in
`.golangci.yml`'s `exclude-functions` (structurally uninteresting: a deferred
`Tx.Rollback`, a write to an already-committed HTTP response) or carries a
`//nolint:errcheck` with a reason at the site. **There is no third category.** If
you add a discard, say why in one of those two places rather than reaching for a
blanket exclusion.

## Required patterns

- **Atomic markdown writes.** Every `.md` write goes through the files layer's
  atomic writer, including rollback paths.
- **DB-then-file ordering.** An operation touching both commits the DB
  transaction first, then performs the filesystem mutation in a post-commit step,
  and undoes partial filesystem state on rollback.
- **`rows.Err()` after every `for rows.Next()`.**
- **FTS5 MATCH sanitization** and **LIKE escaping** - see
  [Domain invariants](/internals/invariants/).
- **`filepath.Join` everywhere**, tests included.

## Adding an MCP tool

A tool has to be wired in **three** places inside `internal/mcp`, and each one is
guarded so that missing it fails a check rather than shipping quietly:

1. **`registerTools`** - the actual registration. Without it the server does not
   serve the tool.
2. **`ToolCount`** (in `server.go`) - bump it. `Server.NumTools()` counts what was
   registered, and `seamlessd doctor` asserts the two are equal, so a tool written
   but never wired in fails the doctor check.
3. **`Catalog()`** (in `catalog.go`) - add the tool's constructor, **in the same
   order as `registerTools`**. `Catalog` exists so `cmd/docsgen` can render the
   tool reference without constructing a `Server`: the constructors are plain data
   (name, description, input schema) and need no DB, config, or listening port.

`internal/mcp/catalog_test.go` enforces the parity, and it is a same-package test
so it can read the server's private `toolNames` - the registration record itself,
not a second hand-maintained list:

```go
require.Equal(t, srv.toolNames, names, "Catalog order/content must mirror registerTools")
require.Len(t, names, ToolCount)
```

A second test, `TestCatalogToolsAreDocumentable`, guards what docsgen renders: a
tool with no description would emit an empty docs section, and a duplicate name
would collide on the page anchor.

Two places outside `internal/mcp` also track the surface:

- **`cmd/seam/doctor.go`** has an `expectedTools` constant that mirrors
  `ToolCount` without importing the mcp package (which would pull its whole
  dependency tree into the CLI). It is the **registered** count, and `seam
  doctor` reports registered against exposed: it reads the effective feature
  state from `GET /console/settings?format=json` and subtracts the tools of the
  features that are off, derived from the registry rather than from a second
  number to keep current (`33 registered, 30 exposed (research disabled)`; `33
  tools (expected 33)` when everything is on). With that state unreadable - an
  unreachable endpoint, or a pre-features daemon whose settings JSON carries no
  `featuresConfig` - it judges a **range** instead of a number and says why,
  rather than failing a healthy daemon over a fact it could not read.
  `seamlessd doctor`'s **server-role** `mcp_tools` check stays a bare equality
  against registration: the two doctors measure different things, and making
  the server-side one feature-aware would lose the "written but never wired in"
  signal it exists for. Its **client-role** check is the opposite case - the
  local binary serves no MCP at all there, so counting its own registrations
  would report green against a server that is down or rejecting the key. That
  path runs the same feature-aware live count `seam doctor` does, and both go
  through `features.ToolCountVerdict` so the arithmetic exists once.
- **A docs page's `tools:` frontmatter list** (under `docs-src/reference/mcp/`)
  decides where the generated reference for the tool appears. A page listing a
  name that is not in `Catalog()` is a docsgen error.

## Before declaring done

1. `make check` is green.
2. `*_test.go` is updated in the same change if a signature changed. After any
   interface, store, schema, or migration change, run `make build && make test`
   and update mocks and fakes for changed signatures in that same change.
3. **Propagate every fix.** When you fix a buggy pattern, grep the repo for other
   instances and fix them all together.
4. **No fake results on error.** Never swallow an error and return a plausible
   dummy value - an agent cannot distinguish it from a real result. Return the
   error.
