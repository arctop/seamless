# Seamless one-command installer for Windows (PowerShell).
#
#   irm https://thereisnospoon.org/install.ps1 | iex
#
# This file IS the published artifact -- the Windows companion to docs/install
# (the POSIX `curl | sh` path). GitHub Pages serves docs/ verbatim, so this file
# lands at https://thereisnospoon.org/install.ps1 unchanged. It is not generated
# (docsgen owns docs/docs/ only), and there is deliberately no second copy under
# scripts/: a script living at two paths is a script that drifts.
#
# What it does: fetch this platform's release zip from GitHub, verify its
# checksum, ask `seamlessd setup` how this computer will use Seamless (only a
# person at the console with no SEAMLESS_* set is asked; SEAMLESS_SETUP answers
# it for a script, and the default is everything on this computer, as it always
# was), install seamlessd.exe + seam.exe into ~\.local\bin, wire the
# detected agent clients -- hooks + MCP + maintained skills for Claude
# Code/Codex, and the MCP bridge for the Claude app chat surface (it has no
# hooks or skills) -- generating the bearer key on first run, then run the
# daemon as a per-user Scheduled Task
# -- an at-logon task running as you, no admin (falling back to a per-user
# Startup-folder shortcut where the task store is closed to standard users). Codex gets the secret-preserving
# stdio proxy by default; direct HTTP remains a supported manual Codex
# configuration. That is
# the Windows analog of launchd / systemd --user: the whole install is per-user,
# which is why it never elevates. Re-running it upgrades in place -- the task
# restarts on the new build before the clients are rewired -- and the config and
# ~\.seamless are never touched. A machine whose config already says `role:
# client` stays a client of its server_url. Uninstall anytime with
# `seamlessd.exe uninstall` (add --purge to also delete the config and data).
#
# Overrides (set as environment variables before running):
#   $env:SEAMLESS_VERSION             version to install (default: latest release)
#   $env:SEAMLESS_INSTALL_DIR         where the binaries go (default: ~\.local\bin)
#   $env:SEAMLESS_CHECKSUMS_SHA256    pin the release's checksums.txt to this
#                                     SHA-256 (64 hex digits) and refuse any
#                                     other. It is for a caller that verified the
#                                     manifest's signature itself (seamlessd's
#                                     updater), so without cosign the signature
#                                     step reports "verified by seamlessd"
#                                     instead of warning.
#   $env:SEAMLESS_CLIENT              claude|claude-desktop|codex|all, or a comma
#                                     list of targets (default: the detected
#                                     clients; prompts when several or none are
#                                     found, and aborts when none is found and
#                                     the session is non-interactive -- never a
#                                     silent default)
#   $env:SEAMLESS_SERVER_URL          install as a CLIENT of a Seamless server
#                                     running somewhere else: wire this
#                                     machine's agent clients to that URL,
#                                     install no service, and keep no data dir
#                                     here. Requires SEAMLESS_PAIR_CODE or
#                                     SEAMLESS_MCP_API_KEY. Wins over the role
#                                     and server_url an existing config names.
#   $env:SEAMLESS_MCP_API_KEY         the server's bearer key, for a client of
#                                     it paired by hand; `seamlessd
#                                     client-config` on the server prints the
#                                     whole command.
#   $env:SEAMLESS_NO_HOOKS=1          skip agent hooks, MCP registration, and
#                                     skills, and the client detection that
#                                     would pick them
#   $env:SEAMLESS_NO_ONBOARD_SKILL=1  skip the one-shot seam-onboard skill
#   $env:SEAMLESS_NO_RESEARCH_SKILL=1 skip the recurring seam-research skill
#   $env:SEAMLESS_NO_SERVICE=1        skip the service; install the binaries and stop
#   $env:SEAMLESS_SETUP               the answer to `seamlessd setup`'s question,
#                                     how this computer uses Seamless, for a
#                                     script (`seamlessd setup -h` lists the
#                                     values); with SEAMLESS_SERVER_URL set it
#                                     may only agree
#   $env:SEAMLESS_IMPORT              a backup a first install starts from (its
#                                     path; a relative one is this directory's):
#                                     setup restores it into the empty data dir
#                                     before anything is installed
#   $env:SEAMLESS_IMPORT_LOCKS        keep|remove: what that import does with
#                                     the backup's locked projects when no
#                                     passkey can open them here (without it, it
#                                     stops and says why)
#   $env:SEAMLESS_SERVER_NAME         the name the other computers reach a
#   $env:SEAMLESS_TLS_CERT            server at, and its certificate and key:
#   $env:SEAMLESS_TLS_KEY             answers for a server's setup, which is
#                                     never this computer -- Windows is a
#                                     client -- so setup refuses them, saying why
#   $env:SEAMLESS_PAIR_CODE           a client's pairing code, from the
#                                     server's Devices page (or `seamlessd
#                                     devices code` there): it pairs one
#                                     computer, once, within 10 minutes. With
#                                     SEAMLESS_SERVER_URL, setup pairs this
#                                     computer as a key device of its own and
#                                     writes its config before anything is
#                                     installed. A Windows computer never runs
#                                     the relay, so its agents never open a
#                                     locked project.
#   $env:SEAMLESS_SERVER_FINGERPRINT  the server certificate's SHA-256, as its
#                                     setup printed it: compared before the
#                                     code is sent
#   $env:SEAMLESS_PAIR_AS             key, the one kind a Windows client pairs
#                                     as (relay is macOS and Linux only)
#
# Any SEAMLESS_* set, or a session that is not interactive, means a script is
# running this: nothing asks how this computer will be used, and a run without
# the setup knobs does exactly what it did before they existed. Every update
# seamlessd makes runs this script that way.

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue' # no per-request progress bar spam

# Windows PowerShell 5.1 does not negotiate TLS 1.2 by default, and GitHub
# requires it. Harmless on PowerShell 7. Best-effort: some hosts lock it down.
try {
    [Net.ServicePointManager]::SecurityProtocol =
        [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
} catch {}

# Authenticating corporate proxies answer 407 unless the request carries the
# logged-in user's credentials, and Invoke-WebRequest does not send them on its
# own. Attach the default (domain) credentials to the system proxy: Windows
# PowerShell 5.1 routes requests through WebRequest.DefaultWebProxy, PowerShell 7
# through HttpClient.DefaultProxy (absent on 5.1, hence the second try). Harmless
# where there is no proxy; a proxy needing non-SSO credentials still 407s, which
# the download error reports as itself.
try {
    $sysProxy = [System.Net.WebRequest]::DefaultWebProxy
    if ($sysProxy) {
        $sysProxy.Credentials = [System.Net.CredentialCache]::DefaultNetworkCredentials
    }
} catch {}
try {
    [System.Net.Http.HttpClient]::DefaultProxy.Credentials =
        [System.Net.CredentialCache]::DefaultNetworkCredentials
} catch {}

$Repo = 'arctop/seamless'
$TaskName = 'Seamless'
# The task's Description, and the Windows analog of the comment docs/install
# writes into its plist and systemd unit: seamlessd's update check looks for it
# to tell a task this script registered from one set up by hand.
$TaskDescription = 'Written by https://thereisnospoon.org/install.ps1. Re-run it to update.'
$DocsUrl = 'https://thereisnospoon.org/docs/'

# Canonical paths, kept identical to the POSIX installer so the config the daemon
# searches, the task runs against, and the hooks resolve are one file. On Windows
# $HOME is %USERPROFILE%, so ~\.config\seamless is exactly what the Go config
# search path (os.UserHomeDir + .config\seamless) already looks for.
$ConfigDir = Join-Path $HOME '.config\seamless'
$Config = Join-Path $ConfigDir 'seamless.yaml'
$DataDir = Join-Path $HOME '.seamless'
$LogFile = Join-Path $DataDir 'seamlessd.log'

# Pre-config fallback for the health poll only; mirrors config.Defaults().
$DefaultAddr = '127.0.0.1:8081'

# How this computer uses Seamless is `seamlessd setup`'s question
# (cmd/seamlessd/setup.go), asked of the binary this run just verified, before
# anything is installed; mirrors run_setup in docs/install. $SetupProtocol is
# the line `seamlessd setup -h` prints when it speaks the hand-off, and
# $SetupHeader opens the answer it writes; a pinned release from before setup
# has neither, and installs exactly as it always did.
$SetupProtocol = 'installer protocol: 1'
$SetupHeader = 'seamless-setup 1'

# The owner's signature over checksums.txt (Test-OwnerSignature). Every
# release after $OwnerSignedAfter carries checksums.txt.sshsig, made by the
# owner with one of these keys, on a hardware key, in the namespace
# seamless-release: the repository's release_signers file, copied here when
# this script was released (scripts/site-check.sh holds the copy to the file).
# A fork that does not sign its releases empties $OwnerSigners.
$OwnerSignedAfter = '0.8.3'
$OwnerNamespace = 'seamless-release'
# BEGIN release_signers
$OwnerSigners = @'
owner-yk1 namespaces="seamless-release" sk-ssh-ed25519@openssh.com AAAAGnNrLXNzaC1lZDI1NTE5QG9wZW5zc2guY29tAAAAINd9uqZ0YLBjsCnvfvpExpYZXXWNQpK+afg2GUxlh9aUAAAAEnNzaDphcmN0b3AtcmVsZWFzZQ==
owner-yk2 namespaces="seamless-release" sk-ssh-ed25519@openssh.com AAAAGnNrLXNzaC1lZDI1NTE5QG9wZW5zc2guY29tAAAAIHaqnbLidFoOJONHvetlWqfvIF2xvMVWfWjWHyv9LFszAAAAEnNzaDphcmN0b3AtcmVsZWFzZQ==
'@
# END release_signers
# A signature ssh-keygen must accept before its verdict on the owner's counts,
# the same as docs/install's: "seamless-release self-test" plus a newline,
# signed with a throwaway hardware-key-type key (sk-ssh-ed25519, flags 0x05).
$SelftestSigner = 'selftest namespaces="seamless-release" sk-ssh-ed25519@openssh.com AAAAGnNrLXNzaC1lZDI1NTE5QG9wZW5zc2guY29tAAAAIKDAzRR54Ql19nfoFNO76tflVFVH/ww293fhTDSilc6AAAAABHNzaDo='
$SelftestSig = @'
-----BEGIN SSH SIGNATURE-----
U1NIU0lHAAAAAQAAAEoAAAAac2stc3NoLWVkMjU1MTlAb3BlbnNzaC5jb20AAAAgoMDNFH
nhCXX2d+gU07vq1+VUVUf/DDb3d+FMNKKVzoAAAAAEc3NoOgAAABBzZWFtbGVzcy1yZWxl
YXNlAAAAAAAAAAZzaGE1MTIAAABnAAAAGnNrLXNzaC1lZDI1NTE5QG9wZW5zc2guY29tAA
AAQKjplAWUdUiLpqz80MVuspayV8I1Ren6WyQFPX1VSd88KTUNvL+2gufMejbAPxpgazjE
Ch5rFPci7MLq8DZr6w4FAAAAAQ==
-----END SSH SIGNATURE-----
'@

function Say { param([string]$Message) Write-Host "  $Message" }
function Step { param([string]$Key, [string]$Message) Write-Host ('  {0,-12} {1}' -f $Key, $Message) }
function Warn { param([string]$Message) Write-Warning $Message }
function Die { param([string]$Message) [Console]::Error.WriteLine("`nerror: $Message"); exit 1 }

# Target selection, kept in step with docs/install and the interactive
# `seamlessd install-hooks` menu: the two hook clients plus the Claude app chat
# surface (MCP bridge only, no hooks or skills). Unlike the POSIX curl|sh pipe,
# `irm | iex` runs in the caller's session, so stdin is still the console and
# Read-Host works: with several targets detected the installer confirms which
# to wire (default: the detected set, multi-select via a comma list); with none
# it asks whether to install at all (default no), then which target (no
# default). Non-interactively, several resolves to the detected set and none
# aborts with guidance -- there is deliberately no silent Claude Code fallback.
# The resolved comma list is passed explicitly to install-hooks, which installs
# the matching hooks, MCP registrations, and skill packages together.
function Test-Interactive {
    return ([Environment]::UserInteractive -and -not [Console]::IsInputRedirected)
}

# ConvertTo-ClientList: map an answer -- numbers or target words, singly or
# comma-separated -- to the canonical comma list (claude, claude-desktop, codex
# in stable order). $null for any unrecognized token, so callers can re-prompt
# or die instead of guessing.
function ConvertTo-ClientList {
    param([string]$Raw)
    $claude = $false; $codex = $false; $desktop = $false; $any = $false
    foreach ($tok in ($Raw -split '[,\s]+')) {
        $t = $tok.ToLowerInvariant()
        if ($t -eq '') { continue }
        switch ($t) {
            { $_ -in '1', 'claude', 'claude-code', 'cc' } { $claude = $true; break }
            { $_ -in '2', 'claude-desktop', 'desktop' } { $desktop = $true; break }
            { $_ -in '3', 'codex', 'cx' } { $codex = $true; break }
            { $_ -in '4', 'all' } { $claude = $true; $codex = $true; $desktop = $true; break }
            'both' { $claude = $true; $codex = $true; break }
            default { return $null }
        }
        $any = $true
    }
    if (-not $any) { return $null }
    $targets = @()
    if ($claude) { $targets += 'claude' }
    if ($desktop) { $targets += 'claude-desktop' }
    if ($codex) { $targets += 'codex' }
    return ($targets -join ',')
}

function Test-ClientHas {
    param([string]$List, [string]$Target)
    return (($List -split ',') -contains $Target)
}

function Read-ClientChoice {
    param([string]$DefaultChoice) # '' = no default: an explicit answer is required
    # Keep the entry labels in step with install_hooks.go and docs/install (a
    # Go test pins them).
    Write-Host 'Wire Seamless to which agent client?'
    Write-Host ('  [1] Claude Code {0}' -f $(if ($script:ClaudeDetected) { '(detected)' } else { '(not detected)' }))
    Write-Host ('  [2] Claude app (chat) {0}' -f $(if ($script:DesktopDetected) { '(detected)' } else { '(not detected)' }))
    Write-Host ('  [3] Codex (app/CLI/IDE) {0}' -f $(if ($script:CodexDetected) { '(detected)' } else { '(not detected)' }))
    Write-Host '  [4] All'
    while ($true) {
        $prompt = if ($DefaultChoice) { "Enter choices like 1 or 1,3 [$DefaultChoice]" } else { 'Enter choices like 1 or 1,3' }
        $answer = Read-Host $prompt
        if (-not $answer) { $answer = $DefaultChoice }
        $list = ConvertTo-ClientList $answer
        if ($list) { return $list }
        Write-Host '  please enter numbers 1-4, comma-separated for several'
    }
}

function Resolve-AgentClient {
    # SEAMLESS_NO_HOOKS wires no client, so there is nothing to select: skip the
    # detection and the prompt along with the wiring. Otherwise a
    # non-interactive run (the updater's) on a machine with no agent client
    # dies below over a choice it would never use. The empty selection also
    # keeps the closing advice from naming a client.
    if ($env:SEAMLESS_NO_HOOKS) { return '' }
    if ($env:SEAMLESS_CLIENT) {
        $list = ConvertTo-ClientList $env:SEAMLESS_CLIENT
        if (-not $list) { Die "invalid SEAMLESS_CLIENT=$env:SEAMLESS_CLIENT (valid values: claude, claude-desktop, codex, all, or a comma list)" }
        return $list
    }
    $claude = (((Get-Command claude -ErrorAction SilentlyContinue) -ne $null) -or (Test-Path (Join-Path $HOME '.claude')))
    $codexHome = if ($env:CODEX_HOME) { $env:CODEX_HOME } else { Join-Path $HOME '.codex' }
    $codex = (((Get-Command codex -ErrorAction SilentlyContinue) -ne $null) -or (Test-Path $codexHome))
    # The chat surface: an existing desktop config is the only portable signal
    # on Windows (no documented app-install path to guess) -- the same gating
    # seamlessd's claudeDesktopSurfaceDetected uses.
    $desktop = ($env:APPDATA -and (Test-Path (Join-Path $env:APPDATA 'Claude\claude_desktop_config.json')))
    $script:ClaudeDetected = $claude
    $script:CodexDetected = $codex
    $script:DesktopDetected = $desktop
    $targets = @()
    if ($claude) { $targets += 'claude' }
    if ($desktop) { $targets += 'claude-desktop' }
    if ($codex) { $targets += 'codex' }
    if ($targets.Count -eq 1) { return $targets[0] }
    if ($targets.Count -gt 1) {
        $detected = $targets -join ','
        if (-not (Test-Interactive)) { return $detected }
        # The menu default is the detected set as numbers, collapsed to 4 (All)
        # when every target was detected.
        $default = if ($targets.Count -eq 3) { '4' } else {
            (($targets | ForEach-Object { @{ 'claude' = '1'; 'claude-desktop' = '2'; 'codex' = '3' }[$_] }) -join ',')
        }
        return Read-ClientChoice $default
    }
    if (-not (Test-Interactive)) {
        Die 'no agent client was detected on this machine (Claude Code, the Claude app, or Codex); set $env:SEAMLESS_CLIENT=claude|claude-desktop|codex|all to install anyway'
    }
    Warn 'no agent client was detected on this machine (Claude Code, the Claude app, or Codex)'
    $answer = Read-Host 'Install anyway? [y/N]'
    if ($answer -notmatch '^(y|yes)$') {
        Die 'aborted: no agent client detected (set $env:SEAMLESS_CLIENT=claude|claude-desktop|codex|all to force)'
    }
    return Read-ClientChoice ''
}

# Map the process architecture onto goreleaser's vocabulary. PROCESSOR_ARCHITEW6432
# is set when a 32-bit PowerShell runs on 64-bit Windows (WOW64); it names the real
# machine, so it wins over PROCESSOR_ARCHITECTURE.
function Get-Arch {
    $a = $env:PROCESSOR_ARCHITEW6432
    if (-not $a) { $a = $env:PROCESSOR_ARCHITECTURE }
    switch ($a) {
        'AMD64' { return 'amd64' }
        'ARM64' { return 'arm64' }
        default { Die "unsupported architecture: $a (Seamless ships amd64 and arm64 builds)" }
    }
}

# The GitHub API rather than parsing the /releases/latest redirect: a rate-limited
# API answers with JSON we can detect instead of a 302 we would silently misread.
function Resolve-Version {
    if ($env:SEAMLESS_VERSION) { return ($env:SEAMLESS_VERSION -replace '^v', '') }
    $tag = $null
    try {
        $rel = Invoke-RestMethod -UseBasicParsing "https://api.github.com/repos/$Repo/releases/latest"
        $tag = $rel.tag_name
    } catch { $tag = $null }
    if (-not $tag) {
        Die @"
could not resolve the latest release (GitHub API rate limit?); pin one instead:
  `$env:SEAMLESS_VERSION='0.3.0'; irm https://thereisnospoon.org/install.ps1 | iex
"@
    }
    return ($tag -replace '^v', '')
}

# Only a real 404 means the asset does not exist; anything else (proxy, TLS
# inspection, a firewall that allows github.com but blocks the asset CDN on
# *.githubusercontent.com) must surface as itself, not masquerade as a missing
# release -- that misdiagnosis has cost real support time.
function Format-DownloadError {
    param([string]$Url, $Err)
    $status = $null
    try { $status = [int]$Err.Exception.Response.StatusCode } catch { $status = $null }
    if ($status -eq 404) {
        return "no such release asset: $Url`ncheck the version at https://github.com/$Repo/releases"
    }
    if ($status -eq 407) {
        return @"
download failed: $Url
the proxy requires authentication (HTTP 407) and rejected the logged-in user's
credentials; ask IT to exempt *.githubusercontent.com (the GitHub release CDN)
from proxy authentication, or download the zip in a browser instead
"@
    }
    return @"
download failed: $Url
  $($Err.Exception.Message)
the asset likely exists but the download was blocked; a proxy or firewall that
allows github.com but blocks *.githubusercontent.com (the release asset CDN)
is the usual cause on managed machines
"@
}

# SEAMLESS_CHECKSUMS_SHA256 names the one checksums.txt this run may install
# from. A caller that sets it has verified that manifest's signature itself
# (seamlessd's updater does, in-process), so Get-Release refuses any other.
# Validated before anything is fetched, so a mangled pin fails fast: exactly 64
# hex digits in either case (Get-FileHash prints upper case, sha256sum lower).
# Present but anything else is an error, never "no pin". Returns the pin lower-
# cased, or '' when unset.
function Get-ChecksumsPin {
    $pin = $env:SEAMLESS_CHECKSUMS_SHA256
    if (-not $pin) { return '' }
    if ($pin -notmatch '^[0-9a-fA-F]{64}\z') {
        Die "invalid SEAMLESS_CHECKSUMS_SHA256=$pin (want the 64 hex digits of the SHA-256 of the release's checksums.txt)"
    }
    return $pin.ToLowerInvariant()
}

# Download the zip and checksums.txt, and verify the SHA-256 before unpacking
# anything. Match the filename exactly in checksums.txt -- a substring match would
# happily verify amd64 against arm64.
function Get-Release {
    param([string]$Version, [string]$Arch, [string]$Tmp, [string]$ChecksumsPin)
    $base = "https://github.com/$Repo/releases/download/v$Version"
    $zip = "seamless_${Version}_windows_${Arch}.zip"
    $zipPath = Join-Path $Tmp $zip
    $sumPath = Join-Path $Tmp 'checksums.txt'

    Step 'downloading' $zip
    try { Invoke-WebRequest -UseBasicParsing "$base/$zip" -OutFile $zipPath }
    catch { Die (Format-DownloadError "$base/$zip" $_) }
    try { Invoke-WebRequest -UseBasicParsing "$base/checksums.txt" -OutFile $sumPath }
    catch { Die (Format-DownloadError "$base/checksums.txt" $_) }

    # The pin is checked before anything is read OUT of the manifest: a
    # checksums.txt other than the one the caller verified vouches for nothing.
    if ($ChecksumsPin) {
        $pinned = (Get-FileHash -Algorithm SHA256 $sumPath).Hash.ToLowerInvariant()
        if ($pinned -ne $ChecksumsPin) {
            Die "checksums.txt does not match SEAMLESS_CHECKSUMS_SHA256`n  expected $ChecksumsPin`n  got      $pinned`nthis is not the release manifest the caller verified; do not install"
        }
    }

    $want = Get-Content $sumPath |
        Where-Object { ($_ -split '\s+')[1] -eq $zip } |
        ForEach-Object { ($_ -split '\s+')[0] } |
        Select-Object -First 1
    if (-not $want) { Die "$zip is not listed in checksums.txt" }
    $got = (Get-FileHash -Algorithm SHA256 $zipPath).Hash
    if ($want.ToLower() -ne $got.ToLower()) {
        Die "checksum mismatch for $zip`n  expected $want`n  got      $got"
    }
    $sumNote = if ($ChecksumsPin) { 'ok (checksums.txt matches SEAMLESS_CHECKSUMS_SHA256)' } else { 'ok' }
    Step 'checksum' $sumNote

    Test-Signature -Base $base -Tmp $Tmp -SumPath $sumPath -ChecksumsPin $ChecksumsPin
    Test-OwnerSignature -Base $base -Tmp $Tmp -SumPath $sumPath -Version $Version
    return $zipPath
}

# A matching checksum proves the zip is the one checksums.txt describes, not that
# checksums.txt came from us -- it is fetched from the same origin, so whoever
# could tamper with one could tamper with both. The signature closes that gap
# (audit M3); mirrors verify_signature in docs/install.
#
# Keyless Sigstore, so the IDENTITY is the check that matters: anyone can produce
# a valid signature under their own identity, and pinning to this repo's release
# workflow on a v* tag is what makes it mean "published by the Seamless
# pipeline". cosign missing warns (installing a signing tool first would be real
# friction, and a first install is trust-on-first-use over TLS regardless);
# cosign present and failing is fatal, because that is positive evidence.
#
# A pinned manifest (SEAMLESS_CHECKSUMS_SHA256, matched in Get-Release) had its
# signature checked before this script ran, by the seamlessd that pinned it, so
# a missing cosign -- the usual case -- loses nothing and says so rather than
# warning. A present cosign still verifies it here.
function Test-Signature {
    param([string]$Base, [string]$Tmp, [string]$SumPath, [string]$ChecksumsPin)

    if (-not (Get-Command cosign -ErrorAction SilentlyContinue)) {
        if ($ChecksumsPin) {
            Step 'signature' 'verified by seamlessd'
            return
        }
        Warn "cosign not found -- archive verified by checksum only.`n    For signature verification: https://docs.sigstore.dev/system_config/installation/"
        return
    }
    $sig = Join-Path $Tmp 'checksums.txt.sig'
    $cert = Join-Path $Tmp 'checksums.txt.pem'
    try {
        Invoke-WebRequest -UseBasicParsing "$Base/checksums.txt.sig" -OutFile $sig
        Invoke-WebRequest -UseBasicParsing "$Base/checksums.txt.pem" -OutFile $cert
    } catch {
        Warn 'this release predates artifact signing -- checksum only'
        return
    }
    # 0spoon = releases before the 2026 move to arctop (plan:move-to-arctop).
    # A pinned SEAMLESS_VERSION from before the move carries that signer, so
    # both orgs stay accepted for good; anything else is still foreign.
    $idRegex = '^https://github.com/(0spoon|arctop)/seamless/\.github/workflows/release\.yml@refs/tags/v'
    & cosign verify-blob $SumPath `
        --signature $sig `
        --certificate $cert `
        --certificate-identity-regexp $idRegex `
        --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' 2>&1 | Out-Null
    if ($LASTEXITCODE -ne 0) {
        Die "SIGNATURE VERIFICATION FAILED for checksums.txt.`n  The release artifacts are not signed by the $Repo release workflow.`n  Do not install. Please report this at https://github.com/$Repo/security"
    }
    Step 'signature' 'ok (sigstore)'
}

# The owner's signature over checksums.txt; mirrors verify_owner_signature in
# docs/install, which says why. The trust root is this script: served from the
# site, published from a release tag, carrying the keys it checks against.
#   - No checksums.txt.sshsig on a release after $OwnerSignedAfter: fatal.
#   - ssh-keygen missing, or unable to verify a hardware key's signature (it
#     must accept $SelftestSig; Windows' bundled OpenSSH before 8.2 cannot):
#     warn and go on, as a missing cosign does.
#   - ssh-keygen refuses it, or the key was not both touched and unlocked
#     (flags 0x05, which ssh-keygen reads but does not require): fatal.
#   - A pinned SEAMLESS_VERSION at or below $OwnerSignedAfter predates the
#     owner's signatures and is installed as before.
function Test-VersionAfter {
    param([string]$Version, [string]$Floor)
    $base = ($Version -split '-', 2)[0]
    if ($base -notmatch '^[0-9]+\.[0-9]+\.[0-9]+$') { return $true }
    return ([version]$base) -gt ([version]$Floor)
}

# Invoke-SshKeygen runs ssh-keygen with stdin from a file, byte for byte (a
# pipe would re-encode it), and returns its exit code and output.
function Invoke-SshKeygen {
    param([string]$Exe, [string[]]$Arguments, [string]$InputFile, [string]$Tmp)
    $out = Join-Path $Tmp 'ssh-keygen.out'
    $err = Join-Path $Tmp 'ssh-keygen.err'
    $quoted = $Arguments | ForEach-Object { '"' + $_ + '"' }
    $p = Start-Process -FilePath $Exe -ArgumentList $quoted -RedirectStandardInput $InputFile `
        -RedirectStandardOutput $out -RedirectStandardError $err -NoNewWindow -Wait -PassThru
    $text = ((Get-Content -Raw $out -ErrorAction SilentlyContinue), (Get-Content -Raw $err -ErrorAction SilentlyContinue)) -join "`n"
    return @{ Code = $p.ExitCode; Text = $text }
}

# Write-Ascii writes text with LF line endings and no BOM, the bytes
# ssh-keygen reads.
function Write-Ascii {
    param([string]$Path, [string]$Text)
    [IO.File]::WriteAllText($Path, ($Text -replace "`r`n", "`n"), (New-Object System.Text.UTF8Encoding $false))
}

function Test-OwnerSignature {
    param([string]$Base, [string]$Tmp, [string]$SumPath, [string]$Version)
    if (-not $OwnerSigners.Trim()) { return }
    if (-not (Test-VersionAfter $Version $OwnerSignedAfter)) {
        Step 'owner' "none (v$Version predates the owner's signatures)"
        return
    }
    $sig = Join-Path $Tmp 'checksums.txt.sshsig'
    try { Invoke-WebRequest -UseBasicParsing "$Base/checksums.txt.sshsig" -OutFile $sig }
    catch {
        Die "NO OWNER SIGNATURE for checksums.txt.`n  Every release after v$OwnerSignedAfter carries checksums.txt.sshsig, the owner's`n  signature over checksums.txt, and v$Version does not.`n  Do not install. Please report this at https://github.com/$Repo/security"
    }
    $keygen = Get-Command ssh-keygen -ErrorAction SilentlyContinue
    $capable = $false
    if ($keygen) {
        Write-Ascii (Join-Path $Tmp 'selftest.signers') ($SelftestSigner + "`n")
        Write-Ascii (Join-Path $Tmp 'selftest.sig') ($SelftestSig.Trim() + "`n")
        Write-Ascii (Join-Path $Tmp 'selftest.msg') "seamless-release self-test`n"
        $r = Invoke-SshKeygen $keygen.Path @('-Y', 'verify', '-f', (Join-Path $Tmp 'selftest.signers'), '-I', 'selftest',
            '-n', $OwnerNamespace, '-s', (Join-Path $Tmp 'selftest.sig')) (Join-Path $Tmp 'selftest.msg') $Tmp
        $capable = ($r.Code -eq 0)
    }
    if (-not $capable) {
        Warn "ssh-keygen here cannot verify a hardware key's signature (it needs OpenSSH 8.2 or newer) --`n    the owner's signature (checksums.txt.sshsig) was not checked"
        return
    }
    $signers = Join-Path $Tmp 'release_signers'
    Write-Ascii $signers ($OwnerSigners.Trim() + "`n")
    $failed = "OWNER SIGNATURE VERIFICATION FAILED for checksums.txt.`n  checksums.txt.sshsig is not the owner's signature over it, by a key this`n  installer carries. Do not install. Please report this at https://github.com/$Repo/security"
    # Each listed signer in turn, by its first principal: -Y find-principals
    # would name it in one call, but OpenSSH added it only in 8.6.
    $verdict = $null
    $signer = $null
    foreach ($line in ($OwnerSigners.Trim() -split "`n")) {
        $name = (($line.Trim() -split '\s+')[0] -split ',')[0]
        if (-not $name) { continue }
        $r = Invoke-SshKeygen $keygen.Path @('-Y', 'verify', '-f', $signers, '-I', $name, '-n', $OwnerNamespace, '-s', $sig) $SumPath $Tmp
        if ($r.Code -eq 0) { $verdict = $r; $signer = $name; break }
    }
    if (-not $verdict) { Die $failed }
    if ($verdict.Text -match '-SK key') {
        $b64 = ((Get-Content $sig) | Where-Object { $_ -notmatch '^-----' }) -join ''
        $blob = [Convert]::FromBase64String($b64)
        $flags = $blob[$blob.Length - 5]
        if (($flags -band 5) -ne 5) {
            Die "OWNER SIGNATURE VERIFICATION FAILED for checksums.txt.`n  The owner's hardware key signed it without being both touched and unlocked`n  (flags $flags, want 5). Do not install. Please report this at https://github.com/$Repo/security"
        }
    }
    Step 'owner' "ok ($signer)"
}

# Test-CurrentUser: whether a Scheduled Task principal's UserId names the account
# this script runs as. Task Scheduler hands one back as DOMAIN\name, a bare name,
# or a SID depending on how the task was registered, so a spelling that matches
# neither form directly is compared by SID. Anything unresolvable is not us.
function Test-CurrentUser {
    param([string]$UserId)
    if (-not $UserId) { return $false }
    $me = [Security.Principal.WindowsIdentity]::GetCurrent()
    if ($UserId -eq $me.Name -or $UserId -eq $me.User.Value) { return $true }
    try {
        $account = New-Object -TypeName Security.Principal.NTAccount -ArgumentList $UserId
        $sid = $account.Translate([Security.Principal.SecurityIdentifier])
        return ($sid.Value -eq $me.User.Value)
    } catch {
        return $false
    }
}

# Stop this install's daemon before swapping binaries: a live seamlessd.exe holds
# its own image locked, and the health check must answer from the NEW binary.
# Scoped twice over, because the task name and the process name are both
# machine-global while the install is per-user:
#
#  - the Seamless task is stopped only when its principal is this user; on a
#    shared box it can be another account's daemon, which an elevated run of
#    this script could otherwise stop;
#  - only `serve` processes of THIS install's seamlessd.exe are stopped, matched
#    on the CIM command line: the exe path, then the serve subcommand. That
#    catches a daemon from the Startup-shortcut fallback or a manual start, which
#    the task does not control, and leaves every other seamlessd.exe alone -- the
#    `seamlessd update` that launched this script, the updater, another user's
#    or another install's daemon.
#
# Then wait for them to exit: Stop-Process returns before the image is released.
function Stop-Daemon {
    param([string]$InstallDir)
    $task = Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
    if ($task -and (Test-CurrentUser $task.Principal.UserId)) {
        Stop-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
    }
    $exe = Join-Path $InstallDir 'seamlessd.exe'
    $pattern = '^\s*"?' + [regex]::Escape($exe) + '"?\s+serve(\s|$)'
    try {
        $procs = @(Get-CimInstance -ClassName Win32_Process -Filter "Name = 'seamlessd.exe'" -ErrorAction Stop |
            Where-Object { $_.CommandLine -and $_.CommandLine -match $pattern })
    } catch {
        Warn "could not list running seamlessd processes ($($_.Exception.Message)); a daemon still running from $exe keeps the old build until it restarts"
        return
    }
    if ($procs.Count -eq 0) { return }
    $ids = @($procs | ForEach-Object { [int]$_.ProcessId })
    Stop-Process -Id $ids -Force -ErrorAction SilentlyContinue
    try { Wait-Process -Id $ids -Timeout 15 -ErrorAction SilentlyContinue } catch {}
}

# A running .exe cannot be overwritten in place -- the image is locked -- and
# re-running this script over a live install IS the upgrade path. Windows does
# allow *renaming* a loaded exe, so move the old one aside first, then drop the
# new one into the freed name. The aside name must be UNIQUE per swap: seam.exe
# stays held by long-lived `seam mcp-proxy` processes while an agent app is
# open, so a fixed ".old" from the previous upgrade can still be locked -- its
# removal fails, the rename collides with it, and the copy then hits the live
# image ("in use by another process", seen live on the second Windows upgrade).
# Leftover asides are swept best-effort here and on every later run.
function Install-OneBinary {
    param([string]$Src, [string]$Dst)
    Get-ChildItem -Path "$Dst.old*" -ErrorAction SilentlyContinue |
        Remove-Item -Force -ErrorAction SilentlyContinue
    if (Test-Path $Dst) {
        $old = "$Dst.old-" + [Guid]::NewGuid().ToString('N')
        try { Rename-Item -Path $Dst -NewName ([IO.Path]::GetFileName($old)) -Force } catch {}
    }
    try { Copy-Item -Path $Src -Destination $Dst -Force }
    catch {
        Die "could not replace $Dst ($($_.Exception.Message))`nclose the agent apps using it (Claude/Codex run 'seam mcp-proxy' from it), then re-run"
    }
    Get-ChildItem -Path "$Dst.old*" -ErrorAction SilentlyContinue |
        Remove-Item -Force -ErrorAction SilentlyContinue
}

# Install-Binaries installs what Main expanded into $Tmp, where Invoke-SetupFront
# asked the new seamlessd.exe before anything was stopped or installed.
function Install-Binaries {
    param([string]$Tmp, [string]$InstallDir)
    New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
    Install-OneBinary (Join-Path $Tmp 'seamlessd.exe') (Join-Path $InstallDir 'seamlessd.exe')
    Install-OneBinary (Join-Path $Tmp 'seam.exe') (Join-Path $InstallDir 'seam.exe')
    Step 'installed' "$InstallDir\seamlessd.exe, $InstallDir\seam.exe"
    if (Get-ChildItem -Path (Join-Path $InstallDir '*.old-*') -ErrorAction SilentlyContinue) {
        Say 'note: a running agent app still holds the previous seam.exe; restart it to pick up the new version'
    }
}

# BEGIN setup
# Test-SetupKnobs: an answer only `seamlessd setup` reads is set. Setup reads
# SEAMLESS_SERVER_URL and SEAMLESS_MCP_API_KEY too, with the meaning this
# script has always given them, but they alone never make it run.
function Test-SetupKnobs {
    return [bool]($env:SEAMLESS_SETUP -or $env:SEAMLESS_PAIR_CODE -or $env:SEAMLESS_SERVER_FINGERPRINT -or $env:SEAMLESS_PAIR_AS -or
        $env:SEAMLESS_IMPORT -or $env:SEAMLESS_IMPORT_LOCKS -or $env:SEAMLESS_SERVER_NAME -or $env:SEAMLESS_TLS_CERT -or $env:SEAMLESS_TLS_KEY)
}

# Resolve-ImportPath: SEAMLESS_IMPORT names the backup a first install starts
# from, and setup runs from $Tmp, so a relative path is made absolute here,
# against the directory this installer was started in. A leading ~ is setup's
# to expand.
function Resolve-ImportPath {
    $path = $env:SEAMLESS_IMPORT
    if (-not $path -or $path.StartsWith('~', [StringComparison]::Ordinal) -or [IO.Path]::IsPathRooted($path)) { return }
    $env:SEAMLESS_IMPORT = Join-Path (Get-Location).ProviderPath $path
}

# Test-ScriptedRun: a SEAMLESS_* variable other than SEAMLESS_CONFIG (which the
# installer pins itself) is set, so a script is running this and nothing may
# ask it a new question. seamlessd setup holds itself to the same rule, and
# seamlessd's updater always sets SEAMLESS_VERSION and SEAMLESS_INSTALL_DIR
# (and runs this script with its input redirected), so no update starts setup.
function Test-ScriptedRun {
    $set = @(Get-ChildItem Env: | Where-Object { $_.Name -like 'SEAMLESS_*' -and $_.Name -ne 'SEAMLESS_CONFIG' })
    return ($set.Count -gt 0)
}

# Invoke-Setup runs `seamlessd setup` the way Invoke-WireHooks runs
# install-hooks -- from $Tmp, against $Config once it exists -- and dies with
# $Failure when it does not succeed. Called as a statement, never captured: a
# captured native command writes into a pipe instead of the console, and a
# question that ends without a newline would never show.
function Invoke-Setup {
    param([string]$Seamlessd, [string]$Tmp, [string[]]$SetupArgs, [string]$Failure)
    Push-Location $Tmp
    try {
        if (Test-Path $Config) {
            $env:SEAMLESS_CONFIG = $Config
        } else {
            Remove-Item Env:SEAMLESS_CONFIG -ErrorAction SilentlyContinue
        }
        & $Seamlessd setup @SetupArgs
        if ($LASTEXITCODE -ne 0) { Die "$Failure (exit $LASTEXITCODE)" }
    } finally {
        Pop-Location
        Remove-Item Env:SEAMLESS_CONFIG -ErrorAction SilentlyContinue
    }
}

# Read-SetupState: the two lines of setup's answer that are the installer's.
# role=server runs the daemon here (the install as it always was), role=client
# dials server_url.
function Read-SetupState {
    param([string]$StatePath)
    if (-not (Test-Path -LiteralPath $StatePath)) { Die "seamlessd setup wrote no answer to $StatePath" }
    $lines = @(Get-Content -LiteralPath $StatePath)
    if ($lines.Count -eq 0 -or $lines[0] -cne $SetupHeader) {
        Die 'seamlessd setup answered in a form this installer does not read'
    }
    $answer = @{ Role = ''; ServerUrl = '' }
    foreach ($line in $lines) {
        if ($line.StartsWith('role=', [StringComparison]::Ordinal)) { $answer.Role = $line.Substring(5) }
        elseif ($line.StartsWith('server_url=', [StringComparison]::Ordinal)) { $answer.ServerUrl = $line.Substring(11) }
    }
    return $answer
}

# Invoke-SetupFront: ask the verified, not yet installed seamlessd.exe how this
# computer uses Seamless. It sets $script:SetupServerUrl to the server a client
# set up at the console dials ('' for none) and $script:SetupStatePath to the
# state for --finish ('' when setup did not run), and returns nothing: Main
# calls it as a statement, so setup's questions reach the console (see
# Invoke-Setup). It runs only when it has something to decide: a setup knob,
# or a person at the console with no SEAMLESS_* set. Every other run -- a
# script's, an update's -- skips it and installs exactly as before. Setup only
# ever adds an answer this script did not have, so it never moves one that
# SEAMLESS_SERVER_URL or the config gave ($ServerUrl): that would be a bug, and
# stops the install before anything is stopped or installed.
function Invoke-SetupFront {
    param([string]$Tmp, [string]$Version, [string]$ServerUrl)
    $script:SetupServerUrl = ''
    $script:SetupStatePath = ''
    $knobs = Test-SetupKnobs
    if (-not $knobs -and (-not (Test-Interactive) -or (Test-ScriptedRun))) { return }
    $fresh = Join-Path $Tmp 'seamlessd.exe'
    $probe = ''
    $prevEap = $ErrorActionPreference
    try {
        $ErrorActionPreference = 'Continue'
        $probe = (& $fresh setup -h 2>&1 | Out-String)
    } catch {
        $probe = ''
    } finally {
        $ErrorActionPreference = $prevEap
    }
    if (-not $probe.Contains($SetupProtocol)) {
        if ($knobs) {
            if (-not $probe.Trim()) { Die "could not run $fresh to ask seamlessd setup; nothing was installed" }
            Die "seamless $Version predates seamlessd setup, which reads SEAMLESS_SETUP, SEAMLESS_PAIR_CODE, SEAMLESS_SERVER_FINGERPRINT, SEAMLESS_PAIR_AS, SEAMLESS_IMPORT, SEAMLESS_IMPORT_LOCKS, SEAMLESS_SERVER_NAME, SEAMLESS_TLS_CERT and SEAMLESS_TLS_KEY; unset them, or drop SEAMLESS_VERSION to get the latest"
        }
        return
    }
    Resolve-ImportPath
    $statePath = Join-Path $Tmp 'setup.state'
    Invoke-Setup $fresh $Tmp @('--state', $statePath) 'seamlessd setup stopped the install; nothing was installed'
    $answer = Read-SetupState $statePath
    switch -CaseSensitive ($answer.Role) {
        'server' {
            if ($ServerUrl) { Die "seamlessd setup has this computer run the daemon, but it is a client of $ServerUrl; please report this at https://github.com/$Repo/issues" }
        }
        'client' {
            if (-not $answer.ServerUrl) { Die 'seamlessd setup made this computer a client of no server' }
            if (-not $ServerUrl) { $script:SetupServerUrl = $answer.ServerUrl }
        }
        default { Die "seamlessd setup answered with no role ($($answer.Role))" }
    }
    $script:SetupStatePath = $statePath
}
# END setup

# install-hooks does the config bootstrap too: it calls config.EnsureAPIKey, which
# generates the bearer key into $Config when no config file exists. So on a first
# install it must run BEFORE the service starts; once $Config exists it runs AFTER
# the restart instead (Main says why). The Push-Location is not decoration:
# ./seamless.yaml is the last entry in the config search path, so running from a
# dir that had one would otherwise bind the install to it. Missing claude/seam is
# a warning inside install-hooks, not a failure, so a box without Claude Code
# still installs cleanly.
function Invoke-WireHooks {
    param([string]$Tmp, [string]$InstallDir, [string]$AgentClient, [string]$Version,
        [string]$ServerUrl, [string]$ApiKey)
    if ($env:SEAMLESS_NO_HOOKS) { Step 'hooks' 'skipped (SEAMLESS_NO_HOOKS)'; return }
    $seamlessd = Join-Path $InstallDir 'seamlessd.exe'
    $seam = Join-Path $InstallDir 'seam.exe'
    Push-Location $Tmp
    try {
        if (Test-Path $Config) {
            $env:SEAMLESS_CONFIG = $Config
        } else {
            # Unset, not pointed at $Config: the file does not exist yet, and
            # EnsureAPIKey only writes it when the config resolves to nothing.
            Remove-Item Env:SEAMLESS_CONFIG -ErrorAction SilentlyContinue
        }
        # --client first shipped in v0.3.3, but this script is always fetched from
        # main and can install any pinned $env:SEAMLESS_VERSION. Ask the binary we
        # just unpacked: an unknown flag fails flag parsing, and Die would abort the
        # install before the scheduled task is ever registered. -h parses flags and
        # returns before any config load, so the probe is side-effect free.
        # 2>&1 on a native command can surface as a terminating NativeCommandError
        # under $ErrorActionPreference='Stop', so the probe relaxes it and catches.
        $probe = ''
        $prevEap = $ErrorActionPreference
        try {
            $ErrorActionPreference = 'Continue'
            $probe = (& $seamlessd install-hooks -h 2>&1 | Out-String)
        } catch {
            $probe = ''
        } finally {
            $ErrorActionPreference = $prevEap
        }
        # --server-url (with --api-key) is the client-install path, and it is
        # newer than every flag checked below: a pinned $env:SEAMLESS_VERSION
        # that predates it would fail flag parsing with the binaries already in
        # place. An empty probe failed to run and proves nothing, so it assumes
        # the modern binary, exactly like the checks below.
        if ($ServerUrl -and $probe -and $probe -notmatch '-server-url') {
            Die "seamless $Version predates --server-url and cannot install as a client of $ServerUrl; drop SEAMLESS_VERSION to get the latest"
        }
        # The claude-desktop target and comma-separated --client lists shipped
        # in the same release, so one probe covers both. A pinned older binary:
        # drop the desktop target visibly (the closing advice keys off
        # $script:WiredClient, so it must not then mention the app) and fall
        # back to that binary's own vocabulary for what remains.
        if ($probe -and $probe -notmatch 'claude-desktop' -and (Test-ClientHas $AgentClient 'claude-desktop')) {
            Warn "seamless $Version predates the Claude app chat surface; skipping claude-desktop (drop SEAMLESS_VERSION to get the latest)"
            $AgentClient = ((($AgentClient -split ',') | Where-Object { $_ -ne 'claude-desktop' }) -join ',')
            if (-not $AgentClient) {
                Die "seamless $Version cannot wire the Claude app chat surface; rerun with `$env:SEAMLESS_CLIENT='claude|codex|all', or drop SEAMLESS_VERSION to get the latest"
            }
        }
        $script:WiredClient = $AgentClient
        $clientValue = $AgentClient
        if ($probe -and $probe -notmatch 'claude-desktop' -and $clientValue -eq 'claude,codex') {
            # Pre-list binaries only understand single words and "all".
            $clientValue = 'all'
        }
        $clientArgs = @('--client', $clientValue)
        # An empty probe failed to run; it did not prove the flag is absent. Assume
        # the modern binary rather than silently downgrading a current install.
        if ($probe -and $probe -notmatch '-client') {
            if ($AgentClient -ne 'claude') {
                Die "seamless $Version predates --client and cannot wire $AgentClient; rerun with `$env:SEAMLESS_CLIENT='claude', or drop SEAMLESS_VERSION to get the latest"
            }
            # Pre-0.3.3 is Claude-Code-only and bundles no skills; the old installer
            # passed no --client here, so this is byte-for-byte its behavior.
            Warn "seamless $Version predates --client and the bundled skills; wiring Claude Code only"
            $clientArgs = @()
        }
        # The embedded skills shipped together with the --skills flag: a pinned
        # v0.3.3-v0.3.6 binary accepts --client but bundles no skills, so the
        # closing seam-onboard advice would name a skill that was never installed.
        if ($probe -and $probe -match '-client' -and $probe -notmatch '-skills') {
            Warn "seamless $Version predates the bundled skills; the seam-onboard/seam-research skills will not be installed (drop SEAMLESS_VERSION to get the latest)"
        }
        if ($ApiKey) {
            # The client pair: the URL to dial and the key to dial it with. The
            # binary writes role: client + server_url + mcp.api_key from these
            # -- no data dir, because a client stores nothing locally. Only for
            # SEAMLESS_SERVER_URL: a client install read back from $Config
            # passes neither, and install-hooks wires against that file as it
            # stands.
            $clientArgs += @('--server-url', $ServerUrl, '--api-key', $ApiKey)
        }
        & $seamlessd install-hooks @clientArgs --seam $seam
        if ($LASTEXITCODE -ne 0) { Die "install-hooks failed (exit $LASTEXITCODE)" }
    } finally {
        Pop-Location
        Remove-Item Env:SEAMLESS_CONFIG -ErrorAction SilentlyContinue
    }
}

# Read the port back out of the config the way the Makefile and the sh installer
# do, so someone who edited addr: gets a health poll and a console URL that follow
# their change instead of an install that claims to have failed.
function Get-ConfiguredAddr {
    if (-not (Test-Path $Config)) { return $DefaultAddr }
    $m = Select-String -Path $Config -Pattern '^\s*addr:\s*"?([^"\s]+)' | Select-Object -First 1
    if ($m) { return $m.Matches[0].Groups[1].Value }
    return $DefaultAddr
}

# Get-ConfigValue: a top-level scalar out of the existing $Config, read the way
# the Makefile and the sh installer read role: and server_url: -- anchored at the
# start of the line (a nested key of the same name is not it), an optional double
# quote, no trailing comment, and case-sensitive like YAML. '' when the file or
# the key is absent.
function Get-ConfigValue {
    param([string]$Key)
    if (-not (Test-Path $Config)) { return '' }
    $pattern = '^' + [regex]::Escape($Key) + ':\s*"?([^"\s]*)'
    $m = Select-String -Path $Config -Pattern $pattern -CaseSensitive | Select-Object -First 1
    if ($m) { return $m.Matches[0].Groups[1].Value }
    return ''
}

# The Windows service: a per-user, at-logon Scheduled Task -- the analog of a
# launchd LaunchAgent / systemd --user unit. LogonType Interactive + RunLevel
# Limited means no admin and no stored password: it runs as you, while you are
# logged in. A task action is a bare exec, not a shell string, so --config and
# --log-file carry what a plist/unit would put in an env prefix: the config to
# pin and the log file the task's process would otherwise write nowhere.
function Register-Service {
    param([string]$InstallDir)
    $seamlessd = Join-Path $InstallDir 'seamlessd.exe'
    New-Item -ItemType Directory -Force -Path $DataDir | Out-Null

    $arg = 'serve --config "{0}" --log-file "{1}"' -f $Config, $LogFile
    $action = New-ScheduledTaskAction -Execute $seamlessd -Argument $arg
    # The canonical account identity (COMPUTERNAME\user, or DOMAIN\user on a
    # domain-joined box). $env:USERDOMAIN can read back as "WORKGROUP" on a local
    # account, which is not a valid principal, so ask Windows for the real name.
    $userId = [Security.Principal.WindowsIdentity]::GetCurrent().Name
    # -User is load-bearing: without it -AtLogOn means "any user logs on", and
    # registering an any-user logon trigger needs elevation (0x80070005 for a
    # standard user) -- the same rule that makes `schtasks /create /sc onlogon`
    # demand an admin prompt. Scoped to yourself it registers without admin.
    $trigger = New-ScheduledTaskTrigger -AtLogOn -User $userId
    $principal = New-ScheduledTaskPrincipal -UserId $userId `
        -LogonType Interactive -RunLevel Limited
    # ExecutionTimeLimit 0 = never auto-kill a long-running daemon (the default is
    # three days); restart-on-failure is the KeepAlive / Restart=always analog;
    # IgnoreNew stops a second copy if the trigger somehow double-fires.
    $settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries `
        -StartWhenAvailable -ExecutionTimeLimit ([TimeSpan]::Zero) `
        -RestartInterval (New-TimeSpan -Minutes 1) -RestartCount 3 -MultipleInstances IgnoreNew

    try {
        Register-ScheduledTask -TaskName $TaskName -Action $action -Trigger $trigger `
            -Principal $principal -Settings $settings -Description $TaskDescription -Force | Out-Null
    } catch {
        Register-ServiceFallback $seamlessd $_
        return
    }
    Start-ScheduledTask -TaskName $TaskName
    Step 'service' "$TaskName (Scheduled Task, at logon)"
}

# Managed machines can close the task store to a standard user: group policy can
# bar task creation outright, and the task name is global, so a \Seamless task
# registered by ANOTHER user of the machine cannot be replaced without admin.
# Neither should abort an otherwise complete install. Fall back to the per-user
# Startup folder -- always writable, no special rights -- and start the daemon
# directly so this session still comes up and the health check still means
# something. The shortcut routes through a hidden powershell because a console
# exe launched straight from a shortcut keeps a visible window for its lifetime.
function Register-ServiceFallback {
    param([string]$Seamlessd, $Err)
    $owner = $null
    try { $owner = (Get-ScheduledTask -TaskName $TaskName -ErrorAction Stop).Principal.UserId } catch {}
    if ($owner -and -not (Test-CurrentUser $owner)) {
        Warn ("the $TaskName Scheduled Task belongs to $owner and replacing it needs admin " +
            "(one-time fix: an elevated 'schtasks /delete /tn $TaskName /f', then re-run this installer)")
    } else {
        Warn ("could not register the $TaskName Scheduled Task ($($Err.Exception.Message)); " +
            'group policy may bar task creation for standard users')
    }

    $sq = { param([string]$s) "'" + ($s -replace "'", "''") + "'" }
    $inner = '& {0} serve --config {1} --log-file {2}' -f (& $sq $Seamlessd), (& $sq $Config), (& $sq $LogFile)
    $lnk = (New-Object -ComObject WScript.Shell).CreateShortcut(
        (Join-Path ([Environment]::GetFolderPath('Startup')) 'Seamless.lnk'))
    $lnk.TargetPath = Join-Path $env:SystemRoot 'System32\WindowsPowerShell\v1.0\powershell.exe'
    $lnk.Arguments = '-NoProfile -WindowStyle Hidden -Command "{0}"' -f $inner
    $lnk.WorkingDirectory = $HOME
    $lnk.Description = 'Seamless daemon (Scheduled Task unavailable at install time)'
    $lnk.Save()

    Start-Process -WindowStyle Hidden $Seamlessd `
        -ArgumentList ('serve --config "{0}" --log-file "{1}"' -f $Config, $LogFile)
    Step 'service' 'Startup shortcut (no Scheduled Task rights); daemon started directly'
}

# One liveness request, with certificate validation disabled for https. Windows
# PowerShell 5.1 has no -SkipCertificateCheck, where the global validation
# callback is the only lever; it is restored in finally so this cannot leak into
# the rest of the install (the download and checksum steps must keep verifying).
function Invoke-HealthProbe {
    param([string]$Url)
    if ($Url -notlike 'https:*') {
        return Invoke-WebRequest -UseBasicParsing -TimeoutSec 1 $Url
    }
    if ($PSVersionTable.PSVersion.Major -ge 6) {
        return Invoke-WebRequest -UseBasicParsing -TimeoutSec 1 -SkipCertificateCheck $Url
    }
    $prev = [System.Net.ServicePointManager]::ServerCertificateValidationCallback
    try {
        [System.Net.ServicePointManager]::ServerCertificateValidationCallback = { $true }
        return Invoke-WebRequest -UseBasicParsing -TimeoutSec 1 $Url
    } finally {
        [System.Net.ServicePointManager]::ServerCertificateValidationCallback = $prev
    }
}

# Get-ServedVersion: the release behind a /healthz body -- its version field with
# the build suffix stripped, since a release build serves "0.8.0+1a2b3c4"
# (version + commit). Every release serves that field, so '' means whatever
# answered is not a daemon this script could have installed.
function Get-ServedVersion {
    param($Body)
    if ($Body -is [byte[]]) { $Body = [Text.Encoding]::UTF8.GetString($Body) }
    if ([string]$Body -match '"version"\s*:\s*"([^"]*)"') {
        return ($Matches[1] -replace '\+.*$', '')
    }
    return ''
}

# The Scheduled Task reports success as soon as it has started the process, but the
# daemon binds its listener ~100ms later. Poll for about 30s until it actually
# answers -- AS the release just installed. The daemon this run replaced can still
# be the one answering (exiting, or one this script could not stop), and a green
# install must mean the new build is serving.
#
# Plaintext first, then TLS: once tls.cert_file and tls.key_file are set the
# listener refuses http entirely, so an http-only poll reports a healthy daemon
# as absent and fails an install that actually worked. Verification is skipped
# because this asks only "what is serving here" -- the one thing read back is
# the version, no credential is sent, and the certificate covers the host of
# server_url rather than the bind address being dialed. Matches wait_healthy in
# docs/install and serverReachable in cmd/seamlessd/console.go.
function Wait-Healthy {
    param([string]$Addr, [string]$Version)
    $urls = @("http://$Addr/healthz", "https://$Addr/healthz")
    $seen = ''
    for ($i = 0; $i -lt 150; $i++) {
        foreach ($u in $urls) {
            try {
                $r = Invoke-HealthProbe $u
                if ([int]$r.StatusCode -eq 200) {
                    $served = Get-ServedVersion $r.Content
                    if ($served -eq $Version) { Step 'healthz' "ok -- $u"; return }
                    $seen = if ($served) { $served } else { 'no version' }
                    break
                }
            } catch {}
        }
        Start-Sleep -Milliseconds 200
    }
    if ($seen) {
        Die "after 30s the daemon on $Addr reports $seen rather than $Version; check the log: $LogFile"
    }
    Die "no /healthz from $Addr over http or https after 30s; check the log: $LogFile"
}

# The client half of Wait-Healthy. Nothing was started here, so there is no
# listener race to wait out -- this asks the server whether it answers this
# machine, and a few tries absorb a slow link rather than a boot. A silent
# server is a warning and not a Die: the clients are wired either way, the
# install is complete, and re-running this script is not how you retry a server
# that was merely asleep.
function Test-ServerHealth {
    param([string]$ServerUrl)
    for ($i = 0; $i -lt 5; $i++) {
        try {
            $r = Invoke-WebRequest -UseBasicParsing -TimeoutSec 2 "$ServerUrl/healthz"
            if ([int]$r.StatusCode -eq 200) { Step 'healthz' "ok -- $ServerUrl"; return }
        } catch {}
        Start-Sleep -Milliseconds 200
    }
    Warn "no /healthz from $ServerUrl -- this machine is wired to it anyway. Check that seamlessd is serving there, that its addr is not loopback-only, and that this machine can reach that host and port."
}

function Main {
    $InstallDir = if ($env:SEAMLESS_INSTALL_DIR) { $env:SEAMLESS_INSTALL_DIR } else { Join-Path $HOME '.local\bin' }
    $checksumsPin = Get-ChecksumsPin
    # Client mode: no daemon here, so no Scheduled Task and no data dir. The
    # trailing slash is stripped because server_url is a base URL with no path,
    # and "http://host:8081/" is a path as far as the config validator is
    # concerned -- a URL pasted out of a browser should still work.
    $serverUrl = if ($env:SEAMLESS_SERVER_URL) { ($env:SEAMLESS_SERVER_URL).TrimEnd('/') } else { '' }
    $apiKey = if ($env:SEAMLESS_MCP_API_KEY) { $env:SEAMLESS_MCP_API_KEY } else { '' }
    # A pairing: setup pairs this computer and writes its config, and its
    # answer names the server_url it dials, so $serverUrl waits for it.
    $pairUrl = ''
    if ($serverUrl -and $env:SEAMLESS_PAIR_CODE -and -not $apiKey) {
        $pairUrl = $serverUrl
        $serverUrl = ''
    }
    if ($serverUrl -and -not $apiKey) {
        Die @'
SEAMLESS_SERVER_URL is set but SEAMLESS_MCP_API_KEY is not: a client needs the
server's bearer key as well as its URL. Run  seamlessd client-config  on the
server and use the line it prints.
'@
    }
    # Without SEAMLESS_SERVER_URL, a config that already says role: client keeps
    # this machine a client of its server_url, read the way the Makefile reads
    # it. A re-run that does not repeat the knob -- an update -- must not hand a
    # client the task `seamlessd serve` refuses under that role. An explicit
    # SEAMLESS_SERVER_URL wins, as a knob the user set always does; install-hooks
    # then refuses a config file that names a different server. The key pairs
    # only with SEAMLESS_SERVER_URL: a client config already holds its own, which
    # install-hooks reads there, so it never rides in argv.
    if (-not $serverUrl -and -not $pairUrl) {
        $apiKey = ''
        if ((Get-ConfigValue 'role') -ceq 'client') {
            $serverUrl = ([string](Get-ConfigValue 'server_url')).TrimEnd('/')
            if (-not $serverUrl) {
                Die "$Config says role: client but sets no server_url; add the URL of the server this machine dials (server_url: https://host:8081), then re-run"
            }
        }
    }
    $agentClient = Resolve-AgentClient
    # What actually got wired; Invoke-WireHooks narrows it when a pinned old
    # binary cannot carry the chat surface, so the closing advice stays honest.
    $script:WiredClient = $agentClient

    $arch = Get-Arch
    $version = Resolve-Version
    Write-Host ''
    Write-Host ('  seamless {0}  windows/{1}' -f $version, $arch)

    $tmp = Join-Path ([IO.Path]::GetTempPath()) ('seamless-' + [Guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Force -Path $tmp | Out-Null
    try {
        $zip = Get-Release $version $arch $tmp $checksumsPin
        Expand-Archive -Path $zip -DestinationPath $tmp -Force

        # How this computer uses Seamless: seamlessd setup's question, asked of
        # the verified binary before the daemon is stopped or anything is
        # installed. It may make this a client a person set up at the console;
        # with no setup knob and no person to ask, it is skipped and everything
        # below runs as it always did.
        Invoke-SetupFront $tmp $version $serverUrl
        if ($pairUrl) {
            # The config setup wrote is the client's whole answer: the server's
            # address in the environment would stand in for its server_url at
            # every load.
            if (-not $script:SetupStatePath) { Die 'SEAMLESS_PAIR_CODE needs seamlessd setup, which did not run' }
            $serverUrl = $script:SetupServerUrl
            Remove-Item Env:SEAMLESS_SERVER_URL -ErrorAction SilentlyContinue
        } elseif ($script:SetupServerUrl) { $serverUrl = $script:SetupServerUrl }

        # Best-effort: nothing is running on a first install.
        Stop-Daemon $InstallDir

        Install-Binaries $tmp $InstallDir

        # Restart the task and wait for it to answer as the new build BEFORE
        # rewiring the clients (make install restarts first too): a wiring
        # failure then exits non-zero with this release already serving, instead
        # of leaving the daemon stopped. That needs $Config: on a first install
        # install-hooks is what writes it, and the task's --config pins it, so a
        # first install wires first. A client install and SEAMLESS_NO_SERVICE
        # start nothing, so they keep wiring first as well.
        $hooksFirst = [bool]($serverUrl -or $env:SEAMLESS_NO_SERVICE -or -not (Test-Path $Config))
        if ($hooksFirst) {
            Invoke-WireHooks $tmp $InstallDir $agentClient $version $serverUrl $apiKey
        }

        if ($serverUrl) {
            # No service and no Get-ConfiguredAddr: a client starts no daemon
            # and binds no port here, so there is no local addr to read and
            # nothing to supervise. The health of the install is the server's.
            Step 'service' "skipped (client of $serverUrl)"
            Test-ServerHealth $serverUrl
        } elseif ($env:SEAMLESS_NO_SERVICE) {
            Step 'service' 'skipped (SEAMLESS_NO_SERVICE)'
            Say "start it yourself: & `"$InstallDir\seamlessd.exe`" serve --config `"$Config`""
        } else {
            Register-Service $InstallDir
            Wait-Healthy (Get-ConfiguredAddr) $version
        }
        if (-not $hooksFirst) {
            Invoke-WireHooks $tmp $InstallDir $agentClient $version $serverUrl $apiKey
        }
        # Once the install is done, the rest of setup's answer: today, each
        # step setup does not do yet, and how to do it by hand.
        if ($script:SetupStatePath) {
            Invoke-Setup (Join-Path $InstallDir 'seamlessd.exe') $tmp @('--state', $script:SetupStatePath, '--finish') `
                'the install is complete, but seamlessd setup could not finish; run  seamlessd setup  to see where it stands'
        }
    } finally {
        Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
    }

    # Put the install dir on the user PATH so `seam` works in new shells. The
    # service uses absolute paths, so this is only for interactive use; it takes
    # effect in the next terminal, not this one.
    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    if (($userPath -split ';') -notcontains $InstallDir) {
        $newPath = if ($userPath) { "$userPath;$InstallDir" } else { $InstallDir }
        [Environment]::SetEnvironmentVariable('Path', $newPath, 'User')
        Write-Host ''
        Say "added $InstallDir to your user PATH (open a new terminal to pick it up)"
    }

    Write-Host ''
    Say 'next:'
    $wired = $script:WiredClient
    $hasClaude = Test-ClientHas $wired 'claude'
    $hasCodex = Test-ClientHas $wired 'codex'
    $hasDesktop = Test-ClientHas $wired 'claude-desktop'
    if ($hasClaude -and $hasCodex) { Say '  open any git repo in Claude Code or the Codex app, CLI, or IDE -- Seamless maps it to a project on its own' }
    elseif ($hasClaude) { Say '  open any git repo in Claude Code -- Seamless maps it to a project on its own' }
    elseif ($hasCodex) { Say '  open any git repo in the Codex app, CLI, or IDE -- Seamless maps it to a project on its own' }
    if ($hasDesktop) { Say '  chat in the Claude app -- the seamless MCP tools load after an app restart' }
    if ($serverUrl) { Say "  this computer is a key client of $serverUrl -- Windows never runs the relay, so its agents never open a locked project; run those on the server" }
    Say "  & `"$InstallDir\seamlessd.exe`" console-open   # open the console, already logged in"
    Write-Host ''
    if ($hasClaude -and $hasCodex) {
        Say 'restart both clients. Codex CLI users: review/approve Seamless in  /hooks.'
        Say 'Codex desktop app users: hook trust is beta; confirm a <seam-briefing>.'
        Say 'then run  /seam-onboard  in Claude Code or  $seam-onboard  in Codex.'
    } elseif ($hasClaude) {
        Say 'restart Claude Code, then run  /seam-onboard  once.'
    } elseif ($hasCodex) {
        Say 'restart Codex. CLI users: review/approve Seamless in  /hooks.'
        Say 'desktop app users: hook trust is beta; confirm a <seam-briefing>, then run  $seam-onboard  once.'
    }
    if ($hasDesktop) {
        Say 'restart the Claude app to load the chat-surface MCP bridge (it reads its config at startup).'
    }
    Write-Host ''
    Say "uninstall anytime: & `"$InstallDir\seamlessd.exe`" uninstall"
    Say "docs: $DocsUrl"
    Write-Host ''
}

Main
