---
id: 01M4GMJ8RQ7VFRC6P2QQA46VQG
kind: constraint
name: update-surfaces-render-parsed-versions-only
description: "Update notices (briefing, console, CLI) are built from parsed X.Y.Z + the constant release URL, never from GitHub's tag_name/html_url/body"
project: seamless
created: 2026-10-09T15:30:03Z
invalid_at: null
superseded_by: null
source_session: 01M4GM7FZR3W5KHMNZE67XWCNE
tags: [update, briefing, security]
---
Every surface that tells someone about an update -- the SessionStart briefing line, the console Updates section and banner, doctor, `update --check`, `seam version/status`, event summaries -- renders ONLY:

- versions that passed `update.Parse` (strict X.Y.Z, three integers), and
- the constant template `https://github.com/arctop/seamless/releases/tag/v<X.Y.Z>` (`update.ReleaseURL`).

Never interpolate the API's `tag_name`, `name`, `html_url`, `body` or asset names. Release text is attacker-influenced from the briefing's point of view: whoever can edit a release (or MITM a misconfigured proxy) would otherwise inject instructions into every agent session on every machine.

The briefing line is also worded as an OWNER action ("Owner action, not a task for this session: ...") so an agent with Bash does not decide to run `seamlessd update` and restart the daemon under every other session, and it must pass `sanitizeField` unchanged (no "ignore", "disregard", "from now on", "you must", "override"). Context: [[auto-update-architecture-and-policy]].
