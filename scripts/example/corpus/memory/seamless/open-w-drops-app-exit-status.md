---
id: 01M6XA5XJ6A85G67FD25J8FM5Q
kind: gotcha
name: open-w-drops-app-exit-status
description: "macOS `open -W -n -a App` exits 0 whatever the app's exit code; to learn how a launched app ended, use `open --stderr <FIFO>` and read its last line"
project: seamless
created: 2026-11-08T10:10:04Z
invalid_at: null
superseded_by: null
source_session: 01M6X4YBARNN9XZTAXXRRPAF8C
model: claude-opus-5-5
tags: [macos, launchservices, touchid, approver, plan:seamless-sealed-unlock]
---
Measured 2026-11-08 by SEAL U1's agent A (session cc/c0412659), against a scratch app and real LaunchServices.
- **`open -W -n -a <app>` exits 0 even when the app exits 2.** LaunchServices doesn't pass the app's exit status back, so a caller can't tell Cancel from success by exit code. This is why `seam mcp-proxy`'s Touch ID cancel used to wait out the full 120 s.
- **What works:** `open --stderr <path>` with a FIFO (0600, in a 0700 temp dir). `open` returns in about 60 ms. The FIFO reaches its end when the app exits, and it carries the app's last stderr line. Hold a spare write end until `open` returns, and count an end only after the app has written something, or a silent app reads as a close.
- Seamless uses this in cmd/seam/mcpproxy_approver*.go: the line "SeamApprove: Not approved:" means cancelled; any other last line means closed. That couples the bridge to the helper's wording (follow-up task 01M6X9M1TYWXVKTDJ9BH1CK16P).
- **How to apply:** never rely on `open`'s exit code for a GUI helper's outcome on macOS. Use the stderr FIFO, or have the helper report over its own channel (for Seamless: the daemon socket).
