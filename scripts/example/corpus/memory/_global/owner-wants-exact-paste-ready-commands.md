---
id: 01M7984YKZ1XHPDDE0PWZSJNE4
kind: convention
name: owner-wants-exact-paste-ready-commands
description: "When the owner must run something, hand exact, complete, paste-ready commands with full paths, never prose like \"run ls -l on the two outboxes\""
project: ""
created: 2026-11-13T01:25:28Z
invalid_at: null
superseded_by: null
source_session: 01M790VSBJWV4HBQ9J7SQ4XZGT
model: claude-opus-5-5
tags: [owner-preference, commands, handoff]
---
The owner, 2026-11-12, during a go-live in another project (session cc/0f2126bd): "please give me exact full commands as a routine (as opposed to 'then run ls -l on the two outboxes' for example)".

**How to apply:**
- Every step the owner runs is a fenced block they can paste unchanged.
- Use full absolute paths and real file names (look them up first, e.g. ls the folder), not placeholders or descriptions.
- One block per step, in order, with what to send back stated after it.
- A placeholder is allowed only when the value is genuinely unknown to the agent (a secret, a path only the owner knows), and then it is named in a variable at the top of the block (`H=<path>`).
- Browser-only steps get the exact URL to `open`, and the button to press.
