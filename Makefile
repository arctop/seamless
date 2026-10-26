# Seamless Makefile. Go 1.26+, no CGO.
BINARY  := seamlessd
CLI     := seam
BIN_DIR := bin
PKG     := ./...
GO      ?= go

# golangci-lint major line. .golangci.yml declares `version: "2"`, and the two
# schema generations are mutually unreadable, so a v1 binary does not lint badly
# here -- it refuses to start. The `lint` target checks for that rather than
# letting the raw schema error surface. CI pins this same version; bump both.
GOLANGCI_VERSION ?= v2.14.0
GOLANGCI_INSTALL := github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION)

# Build metadata linked into the daemon (surfaced in /healthz, the MCP handshake,
# and the startup log so a stale running daemon is visible). `make build` stamps
# them, and any stamp wins wholesale; a build with no ldflags (`go build`,
# `go install ...@vX.Y.Z`) falls back to the binary's Go build info at startup
# (cmd/seamlessd/buildinfo.go).
#
# VERSION derives from the git tag exactly the way goreleaser does (nearest v*
# tag, leading v stripped) so `make install` and a released binary report the
# same number -- the tag is the single source of truth, never a hand-edited
# constant. With no reachable tag it falls back to the dev sentinel (a build
# past a tag reports that nearest tag); the +COMMIT suffix buildVersion()
# appends still pins the exact build.
COMMIT    := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILDDATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
VERSION   := $(shell git describe --tags --abbrev=0 2>/dev/null | sed 's/^v//' || true)
VERSION   := $(if $(VERSION),$(VERSION),0.0.0-dev)
LDFLAGS   := -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildDate=$(BUILDDATE)

# launchd service (macOS user LaunchAgent)
UID           := $(shell id -u)
SVC_LABEL     := org.thereisnospoon.seamless
SVC_TEMPLATE  := deploy/launchd/$(SVC_LABEL).plist
SVC_PLIST     := $(HOME)/Library/LaunchAgents/$(SVC_LABEL).plist
SVC_LOG       := $(HOME)/.seamless/seamlessd.log

# Install locations: stable paths, independent of this working tree. `make
# install` snapshots the binaries and config here and points launchd + the
# selected clients' hooks/MCP registrations at the copies, so `make build`, a
# branch switch, or a moved repo
# never change what the live daemon and every agent's hooks execute.
# Override the prefix with `make install PREFIX=/custom`.
PREFIX        ?= $(HOME)/.local
PREFIX_BIN    := $(PREFIX)/bin
CONFIG_DIR    := $(HOME)/.config/seamless
CONFIG        := $(CONFIG_DIR)/seamless.yaml
# detect = the agent clients present on this machine (codex CLI or ~/.codex,
# claude CLI or ~/.claude, the Claude app bundle or its desktop config),
# resolved by `seamlessd install-hooks` -- the same selection the curl
# installer makes. When nothing is found, install-hooks asks on a terminal
# (defaulting to no) and errors otherwise; there is no silent Claude Code
# fallback. Pass CLIENT=claude|codex|claude-desktop|all (or a comma list) to
# choose explicitly.
CLIENT        ?= detect

# gofmt over TRACKED files only. The go tool's ./... pattern skips dot-dirs, so
# build/vet/test/lint never see .claude/worktrees/ (other agents' checkouts of
# this same repo). gofmt takes paths, not packages, and walks the filesystem
# raw: `gofmt -l .` reports their drift as ours, and `gofmt -w .` rewrites
# their files underneath them mid-edit. Always go through this list.
GOFILES := git ls-files '*.go'

# The bind address lives in the config (`addr:`) -- that is the single source of
# truth, because the daemon reads it there and nothing else can override it
# without becoming a second one. So this Makefile *reads* the port, never owns
# it: change `addr:` in the config, `make install`, and the health poll and help
# text follow. Deliberately not a make variable: baking SEAMLESS_ADDR into the
# plist would let env silently beat the config file, so editing `addr:` would
# appear to do nothing.
#
# Recursive (=), not :=, so it is read at use time -- $(CONFIG) may not exist
# until _seed-config runs during the first install. DEFAULT_ADDR mirrors
# config.Defaults() for the pre-install case.
# (No `#` in the character class: make strips from an unescaped `#` to end of
# line even inside $(shell ...), which silently truncates the call. Excluding
# whitespace already drops any trailing YAML comment, since YAML requires a
# space before `#`.)
DEFAULT_ADDR := 127.0.0.1:8081
CONFIG_ADDR   = $(shell sed -n 's/^addr:[[:space:]]*"\{0,1\}\([^"[:space:]]*\).*/\1/p' $(CONFIG) 2>/dev/null | head -1)
ADDR          = $(or $(CONFIG_ADDR),$(DEFAULT_ADDR))

# The other two keys this Makefile reads, and for the same reason: `role:
# client` means this machine runs NO daemon of its own -- `seamlessd serve`
# refuses outright under it -- so the plist, the service and the local health
# poll all have to fall away, and the install's health question becomes the
# SERVER's /healthz at `server_url:`. Same recursive (=) treatment as ADDR, read
# at use time because _seed-config may create the file during this very run.
#
# IS_CLIENT is the flag every branch tests (non-empty = client), so the literal
# "client" is compared in exactly one place here -- the Makefile's version of
# the Go side's "read it through IsClient, never by comparing this string".
CONFIG_ROLE       = $(shell sed -n 's/^role:[[:space:]]*"\{0,1\}\([^"[:space:]]*\).*/\1/p' $(CONFIG) 2>/dev/null | head -1)
IS_CLIENT         = $(filter client,$(CONFIG_ROLE))
CONFIG_SERVER_URL = $(shell sed -n 's/^server_url:[[:space:]]*"\{0,1\}\([^"[:space:]]*\).*/\1/p' $(CONFIG) 2>/dev/null | head -1)

# Documentation site (cmd/docsgen). DOCS_OUT is committed; `make check` fails if
# it drifts from DOCS_SRC. SITE_ROOT is the directory GitHub Pages serves;
# docsgen additionally writes the crawler files there ($(SITE_FILES),
# overwriting those named files only, never deleting), and docs-check gates
# them all.
DOCS_SRC   := docs-src
DOCS_OUT   := docs/docs
SITE_ROOT  := docs
SITE_FILES := sitemap.xml robots.txt llms.txt llms-full.txt index.md auth.md \
              .well-known/api-catalog .well-known/mcp/server-card.json \
              .well-known/agent-card.json \
              .well-known/agent-skills/index.json \
              .well-known/agent-skills/seam-onboard/SKILL.md \
              .well-known/agent-skills/seam-research/SKILL.md
DOCS_ADDR ?= 127.0.0.1:8899

# The one-command installer (curl -fsSL https://thereisnospoon.org/install | sh).
# It lives at the site root because GitHub Pages serves docs/ verbatim, so this
# path IS the published URL -- there is no copy to keep in step. docsgen does
# not touch it: beyond $(DOCS_OUT) it owns only the two named crawler files.
# PS_INSTALLER is the Windows companion, served the same way at /install.ps1.
INSTALLER    := docs/install
PS_INSTALLER := docs/install.ps1

.PHONY: help build test test-race bench seambench lint vet cross-check vulncheck fmt fmt-check check check-fast tidy run doctor console console-chrome \
	docs docs-check docs-serve changelog installer-check site-check site-stamp indexnow metrics release-snapshot install-git-hooks uninstall-git-hooks \
	install uninstall update _seed-config _install-service _reload-service \
	_check-health _wait-healthy _check-server start stop restart status \
	logs install-onboard-skill uninstall-onboard-skill \
	install-research-skill uninstall-research-skill clean

help:
	@echo "Seamless targets:"
	@echo "  build      compile ./bin/$(BINARY) and ./bin/$(CLI) (touches nothing live)"
	@echo "  test       run unit tests"
	@echo "  test-race  run unit tests with the race detector"
	@echo "  bench      run hot-path Go micro-benchmarks, ns/op (BENCHTIME=1x for a quick smoke run)"
	@echo "  seambench  the AGENT-SCENARIO benchmark: runs a real agent over seeded fixtures and"
	@echo "             prints with-vs-without uplift. SPENDS REAL API TOKENS; needs a 'claude'"
	@echo "             binary + credentials. MANUAL ONLY -- not in check, CI, or any schedule."
	@echo "             Unrelated to 'bench' above (see cmd/seambench/README.md)"
	@echo "  check      the full gate: build + vet + cross-check + fmt + docs + installer + site + lint + vulncheck + test-race"
	@echo "  check-fast the pre-commit subset: same minus build and test-race"
	@echo "  lint       run golangci-lint"
	@echo "  vet        run go vet"
	@echo "  cross-check  go vet for $(CROSS_TARGETS) (part of check; CI tests only three packages on macOS/Windows)"
	@echo "  vulncheck  run govulncheck against the vuln DB (part of check; needs network)"
	@echo "  fmt        gofmt tracked files"
	@echo "  fmt-check  fail if tracked files have gofmt drift"
	@echo "  docs       regenerate the docs site (docs-src/ -> docs/docs/, committed)"
	@echo "  docs-check fail if the committed docs site is stale (part of check)"
	@echo "  docs-serve regenerate and serve the site at $(DOCS_ADDR)"
	@echo "  changelog  regenerate docs-src/changelog.md from the git tags + re-render"
	@echo "             (run at release time, after tagging; output is committed)"
	@echo "  installer-check   parse-check $(INSTALLER) (part of check)"
	@echo "  site-check        fail if the landing page drifts from the installer/CLI (part of check)"
	@echo "  site-stamp        restamp the landing page's static asset ?v= cache-busters"
	@echo "  indexnow          submit the sitemap's URLs to the IndexNow engines (manual,"
	@echo "                    run after a deploy is live; DRY=1 previews the payload)"
	@echo "  metrics           print GitHub stars + release download counts (needs gh)"
	@echo "  release-snapshot  dry-run the release build into dist/ (needs goreleaser)"
	@echo "  tidy       go mod tidy"
	@echo "  run        build and start the server in the foreground ($(ADDR))"
	@echo "  doctor     build and run config + DB self-checks"
	@echo "  console    open the console in a browser, pre-authenticated"
	@echo "  console-chrome  same, but force Google Chrome (for agent testing)"
	@echo "  Deploy (snapshots to stable paths; this is also the iterate loop):"
	@echo "    install            build + copy bin/config to $(PREFIX_BIN) + $(CONFIG_DIR),"
	@echo "                       point the service, hooks, MCP + skills there, and restart"
	@echo "                       (CLIENT=claude|codex|claude-desktop|all|detect, or a comma"
	@echo "                       list, selects the wired agent"
	@echo "                       client; default detect = the clients on this machine)"
	@echo "                       under 'role: client' the service is skipped and the"
	@echo "                       SERVER's /healthz is polled -- this machine runs no daemon"
	@echo "    uninstall          remove service, hooks, MCP, skills + binaries (config/data kept;"
	@echo "                       PURGE=1 also deletes config + ~/.seamless)"
	@echo "    update             upgrade in place to the latest release (CHECK=1 only reports)"
	@echo "    start              start the installed service (launchd/systemd/task)"
	@echo "    stop               stop the installed service"
	@echo "    restart            restart the installed service in place"
	@echo "    status             print the installed service's state"
	@echo "    logs               follow the service log ($(SVC_LOG))"
	@echo "  install-git-hooks        enable .githooks/ (pre-commit runs check-fast)"
	@echo "  uninstall-git-hooks      disable .githooks/"
	@echo "  install-onboard-skill    install seam-onboard from the checkout (CLIENT=claude|codex|all|detect;"
	@echo "                           'make install' already installs the embedded copy)"
	@echo "  uninstall-onboard-skill  remove seam-onboard for CLIENT (default: detect)"
	@echo "  install-research-skill   install seam-research from the checkout (CLIENT=claude|codex|all|detect)"
	@echo "  uninstall-research-skill remove seam-research for CLIENT (default: detect)"
	@echo "  clean      remove build artifacts"

build:
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY) ./cmd/seamlessd
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(CLI) ./cmd/seam

test:
	$(GO) test $(PKG)

test-race:
	$(GO) test -race $(PKG)

# Hot-path baseline benchmarks (recall, briefing, prompt matcher, event
# append + SSE fan-out). -run '^$' skips unit tests so only benchmarks run.
# Override BENCHTIME (e.g. BENCHTIME=1x) for a quick compile/smoke check.
BENCHTIME ?= 1s
bench:
	$(GO) test -run '^$$' -bench . -benchmem -benchtime $(BENCHTIME) $(PKG)

# The AGENT-SCENARIO benchmark (cmd/seambench): stand up the condition arms,
# run every scenario under each of them, grade the captured runs, and print the
# with-vs-without uplift plus a baseline-vs-candidate version delta.
#
# THIS IS NOT `make bench`. That target above is the Go hot-path
# micro-benchmarks -- ns/op, offline, free, part of nobody's bill. This one
# drives a real `claude` through real sessions.
#
# MANUAL ONLY. It is deliberately absent from `check`, `check-fast`,
# .githooks/pre-commit, CI, the release flow, and every schedule, and it must
# stay that way until the owner decides otherwise: each run spends real API
# tokens, needs a `claude` binary with working credentials, and is nondeterministic
# enough that one run is a data point rather than a gate (plan:seambench, owner
# decision 2026-07-28 -- several monitored manual runs first, automation is a
# separate decision).
#
#   make seambench                             the default matrix, 1 run per cell
#   make seambench N=5                         5 runs per scenario x condition cell
#   make seambench BASELINE=v0.4.5             + the version-delta table against that ref
#   make seambench CONDITIONS=vanilla,mechanism SCENARIO=cookie-hardening
#   make seambench REPORT_ARGS=--no-trials     keep the results on disk only
#   make seambench RUN_ARGS='--timeout 25m' REPORT_ARGS=--regrade
#
# Everything runs under a throwaway base dir with its own homes, demo repos,
# data dirs, keys, and non-live ports; the live ~/.seamless, ~/.claude, and
# ~/.codex are never touched. The one live write is the results trial the report
# records in the `seambench` lab (REPORT_ARGS=--no-trials skips it).
#
# OUT is passed to both halves so `report` reads the tree `run` just wrote. If
# you point RUN_ARGS at another --base, set OUT too or the two will disagree.
N           ?= 1
SCENARIO    ?=
CONDITIONS  ?=
BASELINE    ?=
OUT         ?=
RUN_ARGS    ?=
REPORT_ARGS ?=
SEAMBENCH_OUT = $(if $(strip $(OUT)),--out $(OUT),)
seambench:
	@command -v claude >/dev/null 2>&1 \
	    || { echo "ERROR: seambench needs a 'claude' binary on PATH with working credentials."; \
	         echo "       (looking for the free ns/op micro-benchmarks? that is 'make bench')"; exit 1; }
	@echo "==> seambench: the AGENT-SCENARIO benchmark. This runs a real agent and SPENDS REAL API TOKENS."
	@echo "    Cost scales with scenarios x conditions x N (currently N=$(N)). Ctrl-C now if you meant 'make bench'."
	$(GO) run ./cmd/seambench run -n $(N) $(SEAMBENCH_OUT) \
	    $(if $(strip $(SCENARIO)),--scenario $(SCENARIO),) \
	    $(if $(strip $(CONDITIONS)),--conditions $(CONDITIONS),) \
	    $(if $(strip $(BASELINE)),--baseline $(BASELINE),) \
	    $(RUN_ARGS)
	$(GO) run ./cmd/seambench report $(SEAMBENCH_OUT) $(REPORT_ARGS)

lint:
	@command -v golangci-lint >/dev/null \
	    || { echo "ERROR: golangci-lint not found (go install $(GOLANGCI_INSTALL))"; exit 1; }
	@golangci-lint version 2>&1 | grep -q 'version v\{0,1\}2\.' \
	    || { echo "ERROR: .golangci.yml is schema v2 and needs golangci-lint v2; found:"; \
	         golangci-lint version 2>&1 | head -1 | sed 's/^/  /'; \
	         echo "  go install $(GOLANGCI_INSTALL)   (note the /v2 in the path)"; exit 1; }
	golangci-lint run

vet:
	$(GO) vet $(PKG)

# Cross-OS vet. CI runs on ubuntu only, so a darwin- or windows-tagged file
# (internal/agentproc/proc_{darwin,windows}.go, the service and update code
# that branches per OS) would otherwise compile for the first time inside the
# release workflow, after the tag is already pushed. vet type-checks test
# files too, which a plain cross-build would not. CGO off matches the release
# builds (.goreleaser.yaml) and keeps a missing cross C toolchain out of it.
CROSS_TARGETS := darwin/arm64 windows/amd64

cross-check:
	@for t in $(CROSS_TARGETS); do \
	    os=$${t%/*}; arch=$${t#*/}; \
	    echo "cross-check: go vet for $$os/$$arch"; \
	    GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 $(GO) vet $(PKG) || exit 1; \
	done

# Known-vulnerability gate. Reachability-based (govulncheck reports only vulns
# whose vulnerable symbols this code actually calls), so it stays quiet about
# the long tail in transitive deps and fails only on something real.
#
# A hard requirement, like golangci-lint -- an advisory "skip if missing" gate
# is the kind that is green on your laptop and red nowhere, which is how the
# 16 hits in the 2026-07-19 audit accumulated unseen in the first place. It is
# in `check` but NOT `check-fast`: it queries vuln.go.dev, and the pre-commit
# hook has to work on a plane.
vulncheck:
	@command -v govulncheck >/dev/null \
	    || { echo "ERROR: govulncheck not found (go install golang.org/x/vuln/cmd/govulncheck@latest)"; exit 1; }
	govulncheck $(PKG)

fmt:
	@$(GOFILES) | xargs gofmt -w

# Report drift instead of fixing it, so `make check` can fail on it.
fmt-check:
	@drift=$$($(GOFILES) | xargs gofmt -l); \
	    test -z "$$drift" || { echo "gofmt drift (run 'make fmt'):"; echo "$$drift"; exit 1; }

# The documentation site: markdown in docs-src/ -> static HTML in docs/docs/,
# committed and served by the same GitHub Pages config as the landing page.
# Two pages are generated from the code (the MCP tool reference reads
# mcp.Catalog(), the config reference reflects config.Defaults()), so a tool or
# key that changes makes the committed output stale -- which docs-check catches.
docs:
	$(GO) run ./cmd/docsgen -src $(DOCS_SRC) -out $(DOCS_OUT) -site $(SITE_ROOT)

# Staleness gate. Regenerates into a temp dir and diffs, rather than rewriting
# the working tree and running `git diff`: this never mutates what you are
# editing, and -r catches untracked files that git diff cannot see (a page
# deleted from docs-src/ still committed under docs/docs/). The site-root files
# are diffed by name -- the rest of $(SITE_ROOT) is hand-written and not ours to
# compare -- so a stale committed sitemap fails the gate like any stale page.
docs-check:
	@scripts/changelog.sh --check
	@tmp=$$(mktemp -d) || exit 1; \
	    trap 'rm -rf "$$tmp"' EXIT; \
	    $(GO) run ./cmd/docsgen -src $(DOCS_SRC) -out "$$tmp/docs" -site "$$tmp" >/dev/null || exit 1; \
	    ( diff -r "$$tmp/docs" $(DOCS_OUT) && \
	      diff -r "$$tmp/scenarios" $(SITE_ROOT)/scenarios && \
	      for f in $(SITE_FILES); do diff "$$tmp/$$f" $(SITE_ROOT)/$$f || exit 1; done ) > "$$tmp/diff" 2>&1 \
	        || { echo "docs drift: $(DOCS_OUT), $(SITE_ROOT)/scenarios, or the site-root crawler files do not match $(DOCS_SRC) (run 'make docs' and commit)"; \
	             head -40 "$$tmp/diff"; exit 1; }

docs-serve: docs
	$(GO) run ./cmd/docsgen -src $(DOCS_SRC) -out $(DOCS_OUT) -serve $(DOCS_ADDR)

# The /docs/changelog/ page, regenerated from the git tags. Deliberately a
# separate, release-time step rather than part of `make docs`: release dates
# are real wall-clock data, and the docsgen render must stay byte-deterministic
# (TestRenderIsDeterministic, docs-check) -- a date injected there would drift
# the committed site on every unrelated PR. Run this after tagging a release,
# then commit the rewritten docs-src/changelog.md with the re-rendered site;
# between releases `docs-check` verifies the committed source against the tags.
changelog:
	@scripts/changelog.sh
	@$(MAKE) docs

# The full gate: everything that must be green before declaring work done
# (AGENTS.md > "Verification before declaring done"). Sequential $(MAKE) calls
# rather than prerequisites so it fails at the first red step and stays ordered
# under `make -j`. Cheapest and most-likely-to-fail steps come first; test-race
# is last because it is the slowest by far.
check:
	@$(MAKE) build
	@$(MAKE) vet
	@$(MAKE) cross-check
	@$(MAKE) fmt-check
	@$(MAKE) docs-check
	@$(MAKE) installer-check
	@$(MAKE) site-check
	@$(MAKE) lint
	@$(MAKE) vulncheck
	@$(MAKE) test-race
	@echo "check: all green"

# The fast half of `check`: everything except test-race, which is ~37s against
# ~3s for these six combined. This is what .githooks/pre-commit runs, and it is
# the quick iterate loop by hand. `build` is omitted as redundant -- vet
# type-checks, so it already fails on anything that would not compile.
# Cheapest-first, same as `check`. NOT a substitute for the full gate.
check-fast:
	@$(MAKE) fmt-check
	@$(MAKE) vet
	@$(MAKE) docs-check
	@$(MAKE) installer-check
	@$(MAKE) site-check
	@$(MAKE) lint
	@echo "check-fast: all green (test-race not run -- 'make check' is the full gate)"

# $(INSTALLER) is served verbatim at https://thereisnospoon.org/install and is
# the first command a new user ever runs, so a syntax error in it is a broken
# front door that no Go test can see. `sh -n` is the gate because it needs
# nothing installed. shellcheck is the better tool and runs when it happens to
# be present, but only advisory: a gate that fails based on what you have
# installed is a gate that is green in CI and red on your laptop.
installer-check:
	@sh -n $(INSTALLER) || { echo "ERROR: $(INSTALLER) has a syntax error"; exit 1; }
	@if command -v shellcheck >/dev/null; then shellcheck -s sh $(INSTALLER) || true; fi
	@echo "installer-check: $(INSTALLER) parses"
	@# The Windows installer is served the same way, so a syntax error in it is the
	@# same broken front door. pwsh is the parser (the .ps1 analog of `sh -n`);
	@# unlike shellcheck this fails on a real parse error, but only when pwsh is
	@# present (CI's ubuntu runner has it), so the gate is never tool-dependent.
	@if command -v pwsh >/dev/null 2>&1; then \
	    pwsh -NoProfile -Command '$$err=$$null; [void][System.Management.Automation.Language.Parser]::ParseFile("$(PS_INSTALLER)", [ref]$$null, [ref]$$err); if ($$err.Count) { $$err | ForEach-Object { [Console]::Error.WriteLine($$_.Message) }; exit 1 }' \
	        || { echo "ERROR: $(PS_INSTALLER) has a syntax error"; exit 1; }; \
	    echo "installer-check: $(PS_INSTALLER) parses"; \
	else \
	    echo "installer-check: pwsh not found; $(PS_INSTALLER) parse skipped (advisory)"; \
	fi

# docs/index.html is the one page with no generator behind it, so it is the one
# page that can lie indefinitely: the curl|sh installer shipped in d1e926b with
# every generated surface correct and this gate green, while the hero pill still
# said `go install`. The script checks only what is mechanically verifiable --
# the install command each surface teaches, the subcommands the page names, and
# whether the copy buttons copy what they show. The prose is still on you.
# Paths and the canonical install command live in the script, not here, so it
# stays runnable on its own and there is one place to change them.
site-check:
	@scripts/site-check.sh

# Restamp static/site.css and static/site.js with a content-hash ?v= in the
# hand-written landing page, so a changed asset gets a fresh URL that the CDN
# edge cache has never seen instead of serving stale behind its TTL. Run after
# editing either asset; site-check fails until the stamped hash matches.
site-stamp:
	@scripts/site-stamp.sh

# Submit the committed sitemap's URL list to the IndexNow-federated engines
# (Bing, Naver, Seznam, Yandex; Google does not participate). Manual by design:
# run once after a deploy has reached the live site -- the script verifies the
# served key file first, the same check the engines make. Never wire this into
# CI or a hook: a ping per commit is the over-submission pattern the engines
# deprioritize, and a failing HTTP call must not redden a build. DRY=1 prints
# the payload without touching the network.
DRY ?=
indexnow:
	@scripts/indexnow.sh $(if $(strip $(DRY)),--dry-run,)

# The search-visibility north star: stars plus per-release asset download
# counts. The installer pulls binaries from GitHub Releases, so the download
# counter tracks installs rather than a proxy -- inflated by CI runs, mirrors,
# and scanners, so read the trend, not the absolute number.
metrics:
	@scripts/metrics.sh

# Git hooks. Committed under .githooks/ because .git/hooks is neither committed
# nor shared between worktrees. core.hooksPath is repo-local config, so enabling
# them is a per-clone opt-in rather than something a checkout imposes on you.
# The path stays relative: git resolves it against each worktree's top level, so
# linked worktrees under .claude/worktrees/ run their own checkout's copy.
# (Distinct from `install-hooks`, which installs agent-client hooks.)
install-git-hooks:
	@git config core.hooksPath .githooks
	@echo "git hooks enabled: pre-commit runs 'make check-fast' (bypass: git commit --no-verify)"

uninstall-git-hooks:
	@git config --unset core.hooksPath 2>/dev/null || true
	@echo "git hooks disabled"

# Local dry-run of the release pipeline (.goreleaser.yaml): validates the
# config and cross-compiles every platform into dist/ without tagging or
# publishing. Real releases run in CI on a v* tag (.github/workflows/release.yml).
# --skip=sign: the `signs:` stage is keyless cosign, which needs the GitHub OIDC
# token that only exists inside the release workflow. Off a runner it falls back
# to an interactive browser flow, so a local dry-run would hang instead of
# validating the build. Signing is exercised for real on a v* tag.
# Needs goreleaser v2.19.0+: older versions refuse the cask's
# hooks.post.install_steps ("field install_steps not found").
release-snapshot:
	@command -v goreleaser >/dev/null \
	    || { echo "ERROR: goreleaser not found (brew install goreleaser, v2.19.0+)"; exit 1; }
	goreleaser release --snapshot --clean --skip=sign

tidy:
	$(GO) mod tidy

run: build
	$(BIN_DIR)/$(BINARY) serve

doctor: build
	$(BIN_DIR)/$(BINARY) doctor

# Open the console pre-authenticated: build, then have the daemon render a
# one-shot self-submitting login page (POSTs the static key, sets the cookie)
# and open it in the default browser. Requires the server to be running.
console: build
	$(BIN_DIR)/$(BINARY) console-open

# Same, but force Google Chrome regardless of the default browser -- agents
# drive Chrome (Claude-in-Chrome), so this hands the auth cookie to the browser
# they can actually see.
console-chrome: build
	$(BIN_DIR)/$(BINARY) console-open --browser "Google Chrome"

# Boot the (freshly rendered) $(SVC_PLIST): evict any old instance, bootstrap
# with retry, and verify. bootout is asynchronous: it returns before the old
# daemon has exited, the label lingers until it has, and bootstrapping too soon
# fails with "Bootstrap failed: 5: Input/output error". The retry runs every
# 0.1s, so a daemon that takes a few dozen ms to drain costs one retry rather
# than a whole second per install; the 100 tries (10s and up) outlast launchd's
# 5s exit timeout, after which a daemon that ignored SIGTERM is SIGKILLed and
# its label freed.
#
# The plist sets RunAtLoad, so a bootstrap that succeeds has already started the
# daemon -- kickstart is the fallback for the path where it did not (a wedged job
# that bootout could not evict, leaving every bootstrap to fail as already
# loaded). Kicking unconditionally would kill a process launchd spawned
# milliseconds earlier, and the replacement is then held for the full 10s
# ThrottleInterval: a correct-but-10s-late restart, which is what _wait-healthy
# was tripping over.
#
# `install` always routes through here, including when the plist is unchanged.
# launchd caches the code-signing identity of a loaded job's executable, so
# re-execing in place (kickstart -k) after the binary underneath it was replaced
# runs against a stale identity: macOS 26 AMFI launch constraints are enforcing,
# and the spawn is killed after ~14ms with OS_REASON_CODESIGNING. KeepAlive then
# retries, but only once the 10s throttle interval elapses -- so the daemon
# returns ~10s late (blowing _wait-healthy) and every install leaves a crash
# report. bootout/bootstrap re-registers the job against the new binary, so the
# first spawn survives and the restart costs about a second and a half, nearly
# all of it the new binary's own startup (1.42s from the shutdown signal to
# listening, measured on a make install on 2026-10-09). Go binaries are adhoc
# linker-signed with no CMS blob, which is what the constraint rejects; nothing
# here can be signed away.
_reload-service:
	@launchctl bootout gui/$(UID)/$(SVC_LABEL) 2>/dev/null || true
	@booted=0; \
	for i in $$(seq 1 100); do \
	    if launchctl bootstrap gui/$(UID) $(SVC_PLIST) 2>/dev/null; then booted=1; break; fi; \
	    sleep 0.1; \
	done; \
	[ $$booted -eq 1 ] || launchctl kickstart -k gui/$(UID)/$(SVC_LABEL) 2>/dev/null || true
	@launchctl print gui/$(UID)/$(SVC_LABEL) >/dev/null 2>&1 \
	    || { echo "ERROR: $(SVC_LABEL) failed to bootstrap; check $(SVC_LOG)"; exit 1; }

# The one deploy path: snapshot the binaries + config to stable locations
# independent of this working tree, then point launchd AND the selected clients'
# hooks/MCP registrations at the copies. Because nothing live resolves through
# ./bin, `make build`, a
# branch switch, or a moved/cleaned repo cannot change what the running daemon
# and every agent's hooks execute -- swapping them is this target, deliberately.
#
# It is also the iterate loop: the marginal cost over `make build` is two file
# copies and a service reload (see _reload-service for why that reload is
# unconditional rather than a kickstart in place).
#
# One instance per machine (data dir ~/.seamless; bind from the config's addr).
# Override the location with `make install PREFIX=/opt`.
#
# Both halves that assume a local daemon -- the service and the health poll --
# go through a role branch (_install-service, _check-health), because a `role:
# client` config means there is no daemon on this machine to render, supervise
# or poll. Everything else here is role-blind: a client gets the same binaries,
# the same config location and the same wired hooks.
#
# Each of those runs as a sub-make so its $(shell ...) reads the config AFTER
# _seed-config may have written it, the same reason _wait-healthy has always
# been invoked that way.
install: build
	@mkdir -p $(PREFIX_BIN) $(CONFIG_DIR)
	@install -m 0755 $(BIN_DIR)/$(BINARY) $(PREFIX_BIN)/$(BINARY)
	@install -m 0755 $(BIN_DIR)/$(CLI) $(PREFIX_BIN)/$(CLI)
	@$(MAKE) _seed-config
	@$(MAKE) _install-service
	@SEAMLESS_CONFIG=$(CONFIG) $(PREFIX_BIN)/$(BINARY) install-hooks --client $(CLIENT) --seam $(PREFIX_BIN)/$(CLI)
	@$(MAKE) _check-health
	@$(PREFIX_BIN)/$(BINARY) install-summary --bin-dir $(PREFIX_BIN) --config $(CONFIG) --bins $(BINARY),$(CLI)

# Render the plist and (re)boot the service -- unless this is a client install.
#
# Under `role: client` there is nothing to supervise: serve() refuses before it
# opens a file or a port, so bootstrapping it anyway hands launchd a job that
# exits immediately, KeepAlive restarts it on the 10s throttle, and the install
# then fails at the health poll -- a crash loop and a red install on a machine
# that is in fact correctly configured. The curl installer prints this step as
# skipped and names the server; so does this.
#
# The launchd-only directories are created here rather than in `install` for the
# same reason: ~/Library/LaunchAgents holds a plist a client never renders, and
# ~/.seamless is the data dir a client deliberately does not have (it holds no
# corpus and opens no database), so neither should appear on one.
#
# The reload is a separate recipe line, and deliberately not folded into the
# shell block above it: make RUNS any line containing $(MAKE) even under -n, so
# a combined line would have `make -n install` render and install the plist for
# real. Split, the dry run prints the render and only recurses for the reload,
# which is what this file did before the role branch existed.
_install-service:
	@if [ -n "$(IS_CLIENT)" ]; then \
	    echo "service skipped (client of $(CONFIG_SERVER_URL)) -- no daemon on this machine"; \
	    exit 0; \
	fi; \
	mkdir -p $(HOME)/Library/LaunchAgents $(HOME)/.seamless; \
	tmp=$$(mktemp) || exit 1; \
	sed -e 's#__BINARY__#$(PREFIX_BIN)/$(BINARY)#g' \
	    -e 's#__CONFIG__#$(CONFIG)#g' \
	    -e 's#__LOG__#$(SVC_LOG)#g' \
	    $(SVC_TEMPLATE) > $$tmp || exit 1; \
	if cmp -s $$tmp $(SVC_PLIST); then \
	    rm -f $$tmp; \
	    echo "reloading $(SVC_LABEL) (plist unchanged)"; \
	else \
	    install -m 0644 $$tmp $(SVC_PLIST) || exit 1; \
	    rm -f $$tmp; \
	    echo "reloading $(SVC_LABEL) (plist updated)"; \
	fi
	@if [ -z "$(IS_CLIENT)" ]; then $(MAKE) _reload-service; fi

# Which end to ask "is it serving?" -- this machine's listener, or the server a
# client install dials.
_check-health:
	@if [ -n "$(IS_CLIENT)" ]; then $(MAKE) _check-server; else $(MAKE) _wait-healthy; fi

# launchd returns as soon as it has *started* the process, but the daemon binds
# its listener ~100ms later. Without this, `make install && make doctor` -- the
# documented upgrade sequence -- races a server that is not accepting yet and
# reports a failure that fixes itself. Poll until it actually answers, so a green
# install means it is serving.
#
# Plaintext first, then TLS with verification disabled. Once tls.cert_file and
# tls.key_file are set the listener refuses http entirely, so an http-only poll
# can never succeed and `make install` fails on a TLS install that in fact
# worked -- every step before this one has already run. The Makefile deliberately
# does not parse the tls keys to pick a scheme: it greps only `addr:` out of the
# config, and re-deriving more of it here is the transcription that drifts.
# Probing both is the same answer without the second copy.
#
# Verification is skipped because this asks "is something serving here" and
# nothing else: no body is read and no credential is sent, and the certificate
# covers the host of `server_url` rather than the bind address being dialed, so
# a verifying probe would report a correctly configured daemon as down. This
# mirrors serverReachable in cmd/seamlessd/console.go, which fails the same way
# for the same reason.
_wait-healthy:
	@for i in $$(seq 1 50); do \
	    curl -sf  --max-time 1 -o /dev/null "http://$(ADDR)/healthz"  2>/dev/null && exit 0; \
	    curl -sfk --max-time 1 -o /dev/null "https://$(ADDR)/healthz" 2>/dev/null && exit 0; \
	    sleep 0.1; \
	done; \
	echo "ERROR: no /healthz from $(ADDR) over http or https after 50 attempts; check $(SVC_LOG)"; exit 1

# The client half of _wait-healthy. Nothing was started here, so there is no
# listener race to wait out: this asks the server whether it answers this
# machine, and a few tries absorb a slow link rather than a boot. server_url
# carries its own scheme, so unlike _wait-healthy there is no scheme to guess.
#
# A silent server is a WARNING, not a failure, matching the curl installer: the
# agent clients are wired either way and the install is complete, and re-running
# `make install` is not how you retry a server that was merely asleep.
#
# -k for the reason reachabilityProbe gives in cmd/seamlessd/doctor.go: no
# credential is sent and no body is read, so this asks only "is something
# serving there", and a verifying probe would report a server behind a private
# CA as down. Whether this machine TRUSTS that certificate is tls.ca_file's
# question, asked on the surfaces that carry the bearer key (seam doctor, the
# hooks, the MCP bridge), all of which fail loudly when it is wrong.
_check-server:
	@for i in 1 2 3 4 5; do \
	    curl -sfk --max-time 2 -o /dev/null "$(CONFIG_SERVER_URL)/healthz" 2>/dev/null \
	        && { echo "healthz ok -- $(CONFIG_SERVER_URL)"; exit 0; }; \
	    sleep 0.2; \
	done; \
	echo "WARNING: no /healthz from $(CONFIG_SERVER_URL) -- this machine is wired to it anyway."; \
	echo "         Check that seamlessd is serving there, that its addr is not loopback-only,"; \
	echo "         and that this machine can reach that host and port."

# Seed $(CONFIG) on first install only -- never clobber a config that may hold an
# edited bearer key. ./seamless.yaml (gitignored, the pre-install layout's live
# config) wins over the committed template so an existing setup keeps its keys;
# a fresh clone gets the example with a generated mcp.api_key, ready to run.
_seed-config:
	@if [ -f $(CONFIG) ]; then \
	    echo "config kept at $(CONFIG) (delete it to re-seed)"; \
	elif [ -f seamless.yaml ]; then \
	    install -m 0600 seamless.yaml $(CONFIG) && echo "seeded $(CONFIG) from ./seamless.yaml"; \
	else \
	    install -m 0600 seamless.yaml.example $(CONFIG); \
	    KEY=$$(openssl rand -hex 32); \
	    sed -i '' -e "/^mcp:/,/api_key:/ s/api_key: \"\"/api_key: \"$$KEY\"/" $(CONFIG); \
	    echo "seeded $(CONFIG) from seamless.yaml.example with a generated mcp.api_key"; \
	fi

# Full uninstall, delegated to the Go command so there is ONE cross-OS
# implementation (service + hooks + MCP + skills + binaries) instead of the old
# macOS-only launchctl+rm, which reversed neither the hooks/MCP/skills nor the
# Linux/Windows services. --yes because make is non-interactive; --install-dir
# honors `make install PREFIX=...`. $(CONFIG) and ~/.seamless are kept (your
# memories and notes are markdown that outlive the program) unless you opt in:
# `make uninstall PURGE=1`. `build` first so a clean tree still has a binary to
# run -- it tears down the INSTALLED service and $(PREFIX_BIN) binaries, a
# different file from ./bin, so it never deletes the one running it.
PURGE ?=
uninstall: build
	@$(BIN_DIR)/$(BINARY) uninstall --yes --install-dir $(PREFIX_BIN) $(if $(strip $(PURGE)),--purge,)

# Self-update to the latest published release, delegated to the Go binary so the
# release-fetch + checksum + swap logic keeps ONE home (the installer scripts the
# binary re-runs) -- the same delegation `uninstall` and the lifecycle verbs use.
# `build` first so a clean tree has a binary to run. Note this installs the latest
# RELEASE into $(PREFIX_BIN), which may be OLDER than this clone's HEAD -- to
# deploy your local build instead, use `make install`. CHECK=1 only reports
# installed vs latest and changes nothing.
CHECK ?=
update: build
	@$(BIN_DIR)/$(BINARY) update $(if $(strip $(CHECK)),--check,)

# Service lifecycle, delegated to the Go binary so there is ONE cross-OS
# implementation (launchd / systemd --user / Scheduled Task) instead of the old
# macOS-only launchctl targets -- the same move `uninstall` made. `build` first
# so a clean tree has a binary to run; it drives the INSTALLED service wherever
# the installer placed it. A not-installed service prints a hint, not a crash.
start: build
	@$(BIN_DIR)/$(BINARY) start

stop: build
	@$(BIN_DIR)/$(BINARY) stop

restart: build
	@$(BIN_DIR)/$(BINARY) restart

status: build
	@$(BIN_DIR)/$(BINARY) status

# The other target that assumes a local daemon: there is no log on a client
# because there is no process writing one. Say where it is instead of pointing
# at a path on this machine that will never exist.
logs:
	@if [ -n "$(IS_CLIENT)" ]; then \
	    echo "no local log: this machine is a client of $(CONFIG_SERVER_URL); the daemon logs there"; \
	    exit 1; \
	fi; \
	test -f $(SVC_LOG) || { echo "no log yet at $(SVC_LOG)"; exit 1; }
	@tail -f $(SVC_LOG)

install-onboard-skill:
	@scripts/install-skill.sh seam-onboard $(CLIENT)

uninstall-onboard-skill:
	@scripts/uninstall-skill.sh seam-onboard $(CLIENT)

install-research-skill:
	@scripts/install-skill.sh seam-research $(CLIENT)

uninstall-research-skill:
	@scripts/uninstall-skill.sh seam-research $(CLIENT)

clean:
	rm -rf $(BIN_DIR) dist coverage.*
