---
id: 01M4HAFE73Y4V844VKGEY7GMVX
title: "owner decision: macOS shows a Background Items notice after every automatic update -- Developer ID-sign darwin binaries, or document it"
status: done
plan: plan-background-update-check-automatic-updates-claude-code-style
project: seamless
created_by: cc/ae17d44d-a8a09f8bffc1c7e5
---
Human gate. The finding is from 2.01 (lab detached-updater-os-spikes; gotcha memory macos-btm-notice-per-adhoc-binary-update).

What happens:
- macOS Background Task Management gives each legacy LaunchAgent an LWCR that follows the binary's identity.
- seamlessd's darwin release binaries are adhoc linker-signed (.goreleaser.yaml: not signed/notarized).
- So every update makes BTM log `updated item ... not notified` at its next re-walk of ~/Library/LaunchAgents, then post "“seamlessd” can run in the background. You can manage background items in…".
- The re-walk is lazy: hours later, when any other background item changes. Notices are rate-limited.
- It already happens on make install: live notices 2026-10-08 13:21 and 2026-10-09 11:17. Automatic updates make it routine and unprompted.

Tested and NOT enough:
- adhoc with a fixed `codesign -i` identifier;
- a stable self-signed certificate (identical designated requirement) -- still "updated item";
- plist rewrites, which are harmless ("same LWCR").

What works: Apple Team-ID signing. […]

Options:
(1) Accept it, and have 2.15 document the notice: when it appears, that it is expected, and where to check (System Settings > General > Login Items & Extensions).
(2) Developer ID Application signing of the darwin seamlessd/seam binaries in release.yml, e.g. rcodesign on the Linux runner. Needs an Apple Developer Program team, plus the certificate and key as CI secrets. Not verified here: signing would also let the Homebrew cask drop its quarantine workaround, which needs notarization as well.
(3) Both: document now, sign later.

Decide before 2.15 writes the macOS auto-update docs.

DECIDED 2026-10-09 by the owner (session cc/90aaef39): option (2), SIGN, with notarization.
[…]
