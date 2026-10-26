---
title: Install & deploy
description: The one-command install and every other route to the same place - Homebrew, a clone, go install - plus the override knobs and the security posture you are accepting.
---

Installing Seamless is one command: a script downloads a release archive
containing the `seamlessd` daemon and `seam` CLI for macOS, Linux, or Windows,
verifies its SHA-256, and registers the daemon as a per-user service - launchd,
systemd, or a Scheduled Task - bound to loopback. No Docker, database server,
or Go toolchain. The [Quickstart](/quickstart/) runs that command and moves on;
this page is what it did, and what to do when you want to steer it yourself.
Where everything lands - the service, logs, ports, and every path - is on
[The service & where things live](/reference/service/).

## Install in one command

::: when os=macos,linux

```bash
curl -fsSL https://thereisnospoon.org/install | sh
```

:::

::: when os=windows

The [PowerShell installer](https://thereisnospoon.org/install.ps1) does the
same steps with a Scheduled Task in place of launchd/systemd:

```powershell
irm https://thereisnospoon.org/install.ps1 | iex
```

:::

This is the path for using Seamless (as opposed to working on it). The POSIX
script needs `curl` and `tar`; the PowerShell one needs nothing beyond Windows
itself. No Go toolchain is involved. In order, it:

1. resolves the latest release and downloads the archive for your platform
   (macOS, Linux, and Windows; amd64 and arm64), **verifying its SHA-256**
   against the release's `checksums.txt` before unpacking anything; when
   `cosign` is available it also verifies that manifest's keyless signature
   against this repository's release workflow identity;
2. installs `seamlessd` and `seam` into `~/.local/bin`;
3. runs `seamlessd install-hooks`, which generates the bearer key into
   `~/.config/seamless/seamless.yaml` on first run, detects Claude Code, the
   Claude app chat surface, and Codex, and installs that set's hooks, MCP
   registrations, and skills;
4. installs and starts the [per-user service](/reference/service/) - launchd
   on macOS, systemd `--user` on Linux, an at-logon Scheduled Task on
   Windows - and polls `/healthz` for about 30 seconds until the daemon
   answers as the release it just installed.

That is the order of a first install. Once a config exists, steps 3 and 4
swap: the service restarts on the new build and must answer before the
clients are rewired, so a failure while wiring them still leaves the new
release serving.

Step 3 detects three install targets - **Claude Code**, the **Claude app chat
surface** (`claude-desktop`, the app's `mcpServers` bridge; it has no hooks or
skills), and **Codex** - and wires the detected set. That one selection
drives hooks, MCP registrations, and the maintained skills together -
`seam-onboard` always, `seam-research` only while its
[optional feature](/reference/console/#optional-features) is on. On a terminal
the run confirms the selection with a multi-select menu - answers are numbers or names, comma-separated
(`1,3`), defaulting to the detected set; headless, the detected set is wired
as-is. With nothing detected, a run on a terminal warns and asks whether to
install at all (defaulting to no), and a headless run aborts - the installer
never silently wires a client that is not there. `SEAMLESS_NO_HOOKS` skips the
detection along with the wiring, so a headless run with it never stops here.
Set `SEAMLESS_CLIENT` to make
the choice explicit: one target, a comma list, or `all` (every target the
platform can host - the chat surface exists only where the Claude app runs, so
`all` never fails on Linux over it). See [Codex local setup](/codex-cli/) for
the shared app/CLI/IDE profile and Codex's trust gate, and
[Claude app chat setup](/claude-app/) for what the chat surface does and does
not get.

On every OS, Claude's copies live under `$HOME/.claude/skills`; Codex's live
under `$CODEX_HOME/skills` when set, otherwise `$HOME/.codex/skills` (the same
paths are `%USERPROFILE%`-relative on Windows). Invoke `/seam-onboard` in Claude
Code or `$seam-onboard` in Codex. The skill asks before adding its marked block
to global/project `CLAUDE.md` or `AGENTS.md`; it never silently edits either.

::: when os=windows

The Windows installer is per-user by the same principle as the others: it runs
as **you**, never elevates, and registers the Scheduled Task under your own
account (`LogonType Interactive`), so a single signed-in user is all it needs and
no administrator prompt ever appears. `~/.config/seamless` and `~/.seamless`
resolve under `%USERPROFILE%`, exactly the paths the daemon already searches.

:::

Re-running it upgrades in place: binaries are swapped by rename (safe while the
daemon holds them open), the service restarts on the new build, and your config
and `~/.seamless` are preserved. The selected clients are reconciled to those
new stable paths: owned stale hooks and the Codex stdio registration are
repaired, current definitions are untouched, foreign hooks are preserved, and
the recurring skill is refreshed. A machine whose config already says
`role: client` stays a client of its `server_url` - no knobs needed, and no
service registered. It is [one shell
script](https://thereisnospoon.org/install) with no dependencies to audit.

| Override | Effect |
|---|---|
| `SEAMLESS_VERSION=0.3.0` | install that version instead of the latest |
| `SEAMLESS_INSTALL_DIR=~/bin` | put the binaries somewhere else |
| `SEAMLESS_CHECKSUMS_SHA256=<sha256>` | pin the release's `checksums.txt` to this SHA-256 (64 hex digits, either case): any other manifest is refused before anything is unpacked. Meant for a caller that has verified the manifest's signature itself (seamlessd's updater), so without `cosign` the run reports the signature as verified by seamlessd instead of warning |
| `SEAMLESS_CLIENT=claude\|claude-desktop\|codex\|all` | choose which target(s) to wire instead of auto-detection; comma lists work (`claude,claude-desktop`) |
| `SEAMLESS_SERVER_URL=http://studio.local:8081` | install as a **client** of a Seamless daemon running elsewhere: wire this machine's agent clients to that URL, install no service, and keep no data dir here. Requires `SEAMLESS_MCP_API_KEY`, and wins over the `role` and `server_url` an existing config names. See [Share one daemon across a LAN](/guides/network-install/) |
| `SEAMLESS_MCP_API_KEY=<key>` | the server's bearer key, required alongside `SEAMLESS_SERVER_URL` - `SEAMLESS_SERVER_URL` without it is a hard error, because a client needs both. `seamlessd client-config` on the server prints the whole command |
| `SEAMLESS_NO_HOOKS=1` | skip agent hooks, MCP registration, and skills, and the client detection that would choose them |
| `SEAMLESS_NO_ONBOARD_SKILL=1` | skip the selected client(s)' one-shot onboarding skill |
| `SEAMLESS_NO_RESEARCH_SKILL=1` | skip the selected client(s)' recurring research skill |
| `SEAMLESS_NO_SERVICE=1` | install the binaries only; run `seamlessd serve` yourself |
| `SEAMLESS_ALLOW_ROOT=1` | permit running as root (single-user containers) |

::: when os=macos,linux

Set them ahead of the shell, not the curl:
`curl -fsSL https://thereisnospoon.org/install | SEAMLESS_VERSION=0.3.0 sh`.

:::

::: when os=windows

The same knobs are environment variables you set before the pipe -
`$env:SEAMLESS_VERSION='0.3.0'; irm https://thereisnospoon.org/install.ps1 | iex`
- with the one exception of `SEAMLESS_ALLOW_ROOT`, which is POSIX-only (the
Windows task is per-user by construction, so there is no root case to allow).

:::

Everything here is per-user by construction - `~/.local/bin`, `~/.config`,
`~/.seamless`, a user service - so run it as yourself. Under `curl | sudo sh` it
would all land in root's home where your agents will never look, which is why
the script refuses root unless you insist.

## Installing without the one-liner

The other routes end up in the same place with more of the steps left to you.

### Homebrew

```bash
brew install arctop/tap/seamless
```

Every release publishes a cask to the `arctop/homebrew-tap` tap, on macOS and
Linux alike. It delivers the **binaries only** - `seamlessd` and `seam` on your
PATH, with the Gatekeeper quarantine attribute stripped on macOS (a fallback:
the binaries are notarized, but an offline first launch could still be
refused). It does not wire clients or register a service, so
finish with the two commands the cask's caveats print:

```bash
seamlessd install-hooks   # bearer key on first run, hooks, MCP, skills
seamlessd serve           # or set up the service yourself
```

Installed from the old `0spoon/tap`? `brew update` will not move you (Homebrew
does not re-home a cask to a third-party tap on its own), so switch once:

```bash
brew uninstall --cask seamless
brew untap 0spoon/tap
brew install arctop/tap/seamless
```

From then on `brew upgrade` follows `arctop/tap`.

`brew upgrade` moves you to the latest release. The hooks keep working - they
resolve `seam` through brew's stable bin path - but restart the daemon
yourself so it runs the new build.

### From a clone

```bash
make install                    # -> ~/.local/bin + ~/.config/seamless/seamless.yaml
make install PREFIX=/opt/seam   # custom prefix (binaries land in $PREFIX/bin)
make uninstall                  # remove service, hooks, MCP, skills + binaries (data kept)
```

`make install` is the same destination from your own build, and it is macOS-only
(it renders the launchd plist from `deploy/launchd/`). It snapshots the binaries
and config to stable locations, then points launchd **and** the selected clients'
hooks/MCP definitions at the copies. Nothing live resolves through your working
tree, so `make build`, a branch switch, and a moved or cleaned repo cannot change
what the running daemon and every agent's hooks execute. Swapping them is `make
install`, deliberately - [Contributing](/internals/contributing/) covers using
it as the edit-test loop.

The config lands in `~/.config/seamless/`, one of the paths Seamless already
searches ahead of `./seamless.yaml`, so the hooks resolve it from any directory.
It is seeded **only when absent** - an install never clobbers a config holding
your bearer key. Delete it to re-seed.

On a `role: client` config, `make install` branches the same way the one-liner
does: it installs the binaries and wires the agent clients, prints the service
step as skipped naming the server, and polls **that server's** `/healthz`
instead of a local one - an unanswered server is a warning, not a failed
install. A client runs no daemon here, so rendering the plist would hand launchd
a job `seamlessd serve` refuses by design.

### Go install and release archives

The remaining routes end up in the same place with less done for you:
`go install github.com/arctop/seamless/cmd/...@latest` needs Go 1.26+ and
reports the release it was installed at (`seamlessd 0.6.0 (commit unknown, built
unknown, source build)` - the module proxy records no commit or build date), so
`seamlessd update --check` compares it like any release, though as a source
build it does not check for updates by itself; the
[GitHub releases](https://github.com/arctop/seamless/releases) carry the same
prebuilt archives the installer fetches. From a bare binary, `seamlessd serve`
covers the essentials - first run seeds the config - and `seamlessd install-hooks`
wires the detected Claude Code/Codex clients; what you take on yourself is the
service.

## The service {#the-service}

The installer registered `seamlessd` as a per-user service - launchd on macOS,
systemd `--user` on Linux, an at-logon Scheduled Task on Windows - and
`seamlessd start|stop|restart|status` controls it the same way everywhere. The
native commands, log locations, and every path Seamless touches are on
[The service & where things live](/reference/service/).

## Upgrading {#upgrading}

From v0.7.0, an install the installer made updates itself: each new release is
installed a day after it is published, at a moment no agent is using the daemon
([automatic updates](/updating/#automatic-updates)). `seamlessd update` upgrades
in place to the newest release right now, on every OS - backed up, confirmed,
and rolled back if the new release does not come up. An install on v0.6.0 or
older has no update check: run `seamlessd update` once by hand.
[Update & uninstall](/updating/) has the full procedure, the pinning knobs,
and how the release's installer and checksums are signature-verified first.

## Uninstalling {#uninstalling}

`seamlessd uninstall` reverses the whole install and keeps your knowledge by
default. See [Update & uninstall](/updating/#uninstall).

## Security posture

What you are accepting when you run this:

- **One static bearer key** guards `/api/mcp` and the console. Not JWT, not
  OAuth, no user accounts. It is a single-user local tool and the key is in your
  config file with `0600` permissions.
- **Default agent registrations do not copy that key.** Claude Code calls
  `seam mcp-headers` through `headersHelper`; Codex launches `seam mcp-proxy`.
  Both read the 0600 Seamless config at connection time, and neither puts the
  bearer value in client config or subprocess argv. A manual Codex direct-HTTP
  registration with `http_headers` does copy it into `config.toml`; use that
  tradeoff deliberately.
- **Loopback bind** by default (`127.0.0.1:8081`). Nothing off your machine can
  reach it. Widening it is an explicit opt-in, and the daemon logs a `SECURITY`
  warning every time it starts on a non-loopback address.
- **A Host-header allowlist** guards against DNS rebinding: the daemon answers
  the loopback names, a concrete bind host, the host of `server_url`, and
  anything in `allowed_hosts`. Every other `Host` gets `421 Misdirected
  Request`. A wildcard bind (`0.0.0.0`) with no host named anywhere is the one
  unguarded case - naming even one host arms the guard there too.
- **SSRF guards on capture.** `capture_url` is the one tool that makes an
  outbound request on an agent's behalf, and its destination ports are restricted
  to `capture.allowed_ports` (80 and 443 by default) - never "any port".
- **No product telemetry.** Seamless sends no usage or analytics data. The
  outbound traffic is: calls to a configured OpenAI or Anthropic provider (use
  Ollama to keep model calls local), URLs an agent explicitly asks
  `capture_url` to fetch, the GitHub release check and download that an
  explicit `seamlessd update` performs, and - on a release build - the
  [background update check](/updating/#automatic-update-checks): one anonymous
  request to GitHub's releases API every 6 hours, carrying no identifiers. On
  an install the installer made, an
  [automatic update](/updating/#automatic-updates) adds the same downloads
  `seamlessd update` makes. `update.check: false` turns both off and means no
  update traffic at all.
- **Release authenticity has two layers.** Every installer verifies the
  archive's SHA-256 against `checksums.txt`. When `cosign` is installed it also
  verifies the manifest's keyless signature against this repository's release
  workflow identity; without cosign it warns clearly and continues with checksum
  integrity only - unless `SEAMLESS_CHECKSUMS_SHA256` pins the manifest to one
  its caller already verified, in which case any other manifest is refused and
  the run says the signature was verified by seamlessd. `seamlessd update` -
  and every automatic update -
  separately verifies the target release's installer script and `checksums.txt`
  against their Sigstore bundles, in-process and by that release's exact tag,
  before executing anything, and pins the installer to that `checksums.txt`. `curl | sh` still means trusting the bytes served by the
  site, so read the script first if that boundary is not acceptable;
  `go install` lands in the same place.

### Going beyond loopback, deliberately

A static bearer key is adequate *because* the listener is on loopback; the two
are a matched pair. Widen `addr` and that key becomes the only thing between a
network peer and your entire knowledge store - so widening it is three
deliberate steps, not one:

1. **Bind wide and say what you are called.** `addr: 0.0.0.0:8081` plus
   `server_url: https://studio.local:8081`. The second key is what clients
   dial, and its host joins the allowlist, so the guard stays on.
2. **Add TLS.** `tls.cert_file` + `tls.key_file` makes the listener https
   (TLS 1.2 floor) and marks the console session cookie `Secure`. Without it
   the bearer key, every hook payload, and the whole console travel in the
   clear; `seamlessd doctor` reports that as a warning on the `bind` line.
3. **Keep the blast radius small.** There is still no rate limiting, no user
   accounts, and no per-client authorization: one key admits the whole corpus.
   A trusted LAN is the supported boundary. The public internet is not.

[Share one daemon across a LAN](/guides/network-install/) is the walkthrough -
certificates, the `seamlessd client-config` pairing command, and what a remote
device does *not* get. If you only need to reach your own daemon from
elsewhere, a tunnel (Tailscale, SSH forwarding, Cloudflare Tunnel) over a
loopback bind is still the smaller change and the safer one.

See [Configuration](/reference/configuration/) for every key.
