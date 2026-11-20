---
id: 01M4KMNER0HZGJBWSF7ZFV874K
name: cc/b3e82dab-c1ebfeedda1cdcf6
status: completed
started: 2026-10-10T19:29:31Z
---
Plan plan-background-update-check-automatic-updates-claude-code-style, 2026-10-10 evening. The Linux and Mac halves of 2.16 (live E2E) are DONE; the Windows half was not run (owner's choice: nobody was signed in at the test machine's desktop). Full results are in the 2.16 task body (01M4GMFQ4BS6JHQ2PAZJJBKQNE).

- Linux (Lima seamspike-u24): every step passed.
  - check:false: 0 packets on 443 over a 400s tcpdump.
  - Soak, hold + Resume, deadline path with a live session (update.started path "deadline"), mcp-proxy and session survival, the briefing line in Claude Code and Codex plus the banner, hand-run serve = unknown, clock 2 days ahead.
- Mac: a throwaway Lima macOS 26.6.2 guest seam-mac26 (kept stopped), so the owner's live daemon was never touched.
  - Idle path: ~0.5s launchd gap; no AMFI, no .ips, no new Background Items entry.
  - Rollback drill and a true restore; Homebrew, source-build and hand-run negatives.
  - Sleep could only be approximated with a wall-clock jump (VZ guests cannot sleep). That showed the check schedule follows the wall clock.
- F1 fixed in 902f44e (main, NOT pushed): doctor and update --check now judge the soak and the deadline on GitHub's clock (State.GitHubClock). Before, a skewed machine clock made them say "soaked". make check green.
- Filed the cosmetic follow-up 01M4KRCV31TGQCEXSB8Y5BRGWE.
- New memory lima-macos-guest-for-live-update-tests; testing-auto-update-on-the-lima-vm extended (the 30-min no-request window after a version change gates the pre-spawn re-check; console GETs count as activity).
- Left open:
  - the 2.16 Windows half (sign in at the test machine first);
  - task 01M4HCZR2W47BZAA5FRNRS1HT5;
  - push 902f44e;
  - optional: a lid-close test on a physical Mac release install.
