# Console

> The read-mostly observability UI at /console - the complete list of what it can change, how sign-in works, the three experience levels, and what each page shows.

The console is the owner's window onto a system whose actual clients are agents.
It is `html/template` plus vanilla JS plus SSE, served by `internal/console` from
the same binary as everything else - no node, no npm, no React, no build step. It
is not how Seamless is driven; it is how you watch it.

## What the console can change

The console is **read-mostly**. That is a design claim, so here is the whole
list - every write it is capable of, taken from the `POST` routes in
`internal/console/console.go`. There are no others. A few of those routes write
nothing, and their rows say so.

| Action | Route | Effect |
|---|---|---|
| Archive a memory | `POST /console/memories/{id}/archive` | Routes through `lifecycle.Archive`: the memory is stamped invalid and leaves briefings, recall and every index. Its file stays on disk with a line saying when it was archived, its history stays readable here, and **Restore a memory** brings it back. Records `memory.archived` with `by: console`. Returns to the Memories list or the memory's own page `next=` names, its filters kept; any other `next` returns to the plain list. An archived memory is left as it is. |
| Restore a memory | `POST /console/memories/{id}/restore` | Routes through `lifecycle.Restore`, under the file's lock: an archived memory, or a superseded one that nothing active replaced, comes back into briefings, recall and the indexes, and the line its retirement added to the body comes off. Refused (409), with nothing written, while a memory that replaced it is still active (the answer names that memory when you can read it), and while [Held](#held) holds it: with write screening on, its file is quarantined or the review flagged it, and, whenever the held queue is served, an agent's change to it waits there. An active memory is left as it is. Records `memory.written` with `restored`. Returns to the memory's own page `next=` names, its filters kept. |
| Delete a note | `POST /console/notes/{id}/delete` | What `notes_delete` does, under the file's lock: the note's file and its index rows go for good, and `note.written` records it with `deleted`. A plan's narrative (the note a `plan:<slug>` composition leads with) also needs `narrative=<slug>` for each plan it narrates, the box beside **Delete**; without it the delete is refused (409), since a form posts on its first press without script. Refused too while [Held](#held) holds the note: its file is quarantined, or an agent's change to it waits there. Returns to the Notes list `next=` names, its filters kept. |
| Force-release a task claim | `POST /console/tasks/{id}/release` | The owner override, **Release claim** on a task in progress, which asks in the page first on a task's own page, the reading panel and the Tasks list (a lapsed claim's strip on a plan card, Now's loose ends and the Overview's card for one lapsed or claimless claim release at once: no live holder is interrupted): takes the task's claim from whoever holds it, live or lapsed, and tells the holder, or reopens a task marked in progress with no claim at all, so any agent may claim it. Records `task.transition` with `released` and `by: console`. A task no longer in progress has nothing to release: the page says so (a JSON caller gets 409). Not reachable from the agent MCP tools. |
| Mark a task done, drop it, or reopen it | `POST /console/tasks/{id}/status` | **Mark done**, **Drop** (which asks in the page first) or **Reopen** on a task: `to=done`, `dropped` or `open`, through the update `tasks_update status=` makes, so a closed task unblocks the tasks that depend on it, and the last step of a plan closed as done settles the plan as an agent's close does. Records `task.transition` with `by: console`. Refused, naming the holder, while a live claim holds the task: release the claim first. Reopen is for a done or dropped task (one in progress reopens through **Release claim**); a move to the status the task has writes nothing. |
| Mark a gate as handled | `POST /console/tasks/{id}/gate/clear` | **Mark as handled** on a task whose own gate is open, with an optional one-line `note=`: clears the gate as `tasks_update gate_clear` does, recording the owner as who cleared it and the note. A step's gate clears, and the step goes ahead once nothing else blocks it; a gate closes as done, which unblocks the tasks waiting on it. Records `task.transition` with `gate: cleared` and `by: console`. Refused (409) on a task with no gate, and refused while a live claim holds the task, from an agent's process on the peer_auth socket, and from a paired device; a gate already handled writes nothing. |
| Say what a task waits on | `POST /console/tasks/{id}/gate` | **Waits on someone** on an open or in-progress task whose own gate is not open: `who=` and `what=` (one line each), with optional `decision=` and `ready_when=`. Sets the task's gate as `tasks_update gate_who` and `gate_what` do, so a ready step leaves the ready queue and waits on people until its gate is marked as handled; a gate already cleared opens again. Records `task.transition` with `gate: set` and `by: console`. Refused, naming the holder, while a live claim holds the task, and on a closed task; setting the gate the task already has writes nothing. |
| Force-release a repo lock | `POST /console/locks/{id}/release` | [Repo locks](#locks): ends the hold a lock carries, live or lapsed, whoever holds it; the lock stays declared. What `seam lock release --force` sends. Refused when nobody holds it (409), from an agent's process on the peer_auth socket, and from a paired device. |
| Approve a lock declaration | `POST /console/locks/{id}/approve` | Makes a restricted account's waiting declaration an active lock, which agents may then hold. With `next=` a console page (the Overview's **Approve**), it returns there; a lock decision without one returns to Locks. Refused for a lock already active (409), from an agent's process on the peer_auth socket, and from a paired device. |
| Discard a lock declaration | `POST /console/locks/{id}/discard` | Deletes a restricted account's waiting declaration with its repository identities, so the repository may be declared again. An active lock is refused (409): no route removes one. Refused from an agent's process on the peer_auth socket, and from a paired device. |
| Approve a captured plan | `POST /console/plans/{slug}/approve` | The escape hatch for a Claude Code approval whose `PostToolUse` never fired: flips the `cc-plan` note to `plan-status:approved` and creates the tracking task, exactly as the hook would have. Only applies to a CC capture. `?project=<slug>` names the capture's project (every card's **Approve** sends it); without it the slug's only capture is approved, and a slug captured in two projects is refused (409) with both named. Returns to the plan's page, or with `next=` a console page (the Overview's **Approve**, a plan card's) there, with a notice. |
| Star / unstar an entity | `POST /console/favorites/{kind}/{id}` | Toggles the same starred flag the `favorite_set` MCP tool sets: a starred memory pins into briefings and gets a post-fusion recall boost. For memories and notes the star lives in frontmatter; it never bumps `updated`. A plan's star is its narrative note's, in the project `?project=` names (`global` for the global scope); without it, the slug's earliest note in any project. |
| Ask the gardener for proposals | `POST /console/gardener/request` | Interprets a natural-language maintenance request into **pending proposals**. It never mutates a memory. |
| Plan a project split | `POST /console/gardener/split` | Interprets a split request into a plan batch of **pending proposals** (one split setup plus one reproject per memory). Also never mutates a memory. |
| Apply one proposal | `POST /console/gardener/{id}/apply` | Carries out that proposal's effect. While agents may not apply proposals (Settings, Gardener), this and every other Gardener decision - dismiss, hide, unhide, retarget, undo, dismissing a group, applying a plan - refuse (403) a request from an agent's process on the peer_auth socket and one from a paired device; your browser, your own terminal and the bearer key keep working. |
| Dismiss a proposal | `POST /console/gardener/{id}/dismiss` | Drops it without acting. The pattern is raised again if new evidence for it arrives after the decision. |
| Hide a proposal forever | `POST /console/gardener/{id}/hide` | Drops it without acting and blocks the pattern permanently - no recurrence re-raises it. Listed under **Hidden forever**. |
| Unhide a pattern | `POST /console/gardener/{id}/unhide` | Lifts a forever block. The proposal stays resolved; the gardener may propose the pattern again the next time it recurs. |
| Retarget a reproject proposal | `POST /console/gardener/{id}/retarget` | Rewrites a **pending** reproject's destination project before it is applied, and records a `gardener.action` event with the destination before and after. Reproject proposals only. |
| Apply a whole plan batch | `POST /console/gardener/plan/{slug}/apply` | Applies every pending proposal in a plan, split setup first so the child projects exist before the memories move. Best-effort: it applies what it can, reports how many landed, and leaves the rest pending. |
| Dismiss a whole rail group | `POST /console/gardener/group/dismiss` | Dismisses every pending proposal in one rail section or split plan, one at a time, so each keeps its own event and stays individually undoable. `src=you` or `src=auto` narrows a section to the rows the Source filter shows. |
| Apply a whole rail section | `POST /console/gardener/group/apply` | Applies the proposals one rail section shows (`group=`, and one `id=` for each proposal shown), one at a time, so each keeps its own event and its own Undo in the decision history. A proposal raised after the page rendered is never applied unseen, and one decided meanwhile is skipped and counted. Offered only on a section whose every proposal can be undone and none of which asks to be read first (it touches a project fenced since it was raised, or moves a stamped memory into a less fenced project); a batch holding any other is refused before anything is applied. A consolidation or a split never takes part. Past that check it is best-effort: it applies what it can, reports how many landed, and leaves the rest pending. |
| Swap which memory a merge keeps | `POST /console/gardener/{id}/swap` | Rewrites a **pending** merge so it keeps the other memory (`keep=` names it, one of the two) and retires the one it kept. Nothing changes until the merge is applied. Records a `gardener.action` event. Merge proposals only. |
| Undo a decision | `POST /console/gardener/{id}/undo` | Returns a resolved proposal to the queue from **Recently decided** or the **decision history**, inverting whatever its apply did, for the kinds whose apply can be inverted. |
| Run a gardener pass now | `POST /console/gardener/run` | Runs the background proposal passes now instead of at the next pass, in the background. Like every pass they only propose, except that a new proposal of a kind you turned on under **Apply by themselves** (`gardener.auto_apply`, off by default) is applied by the pass that raised it. Refused while a pass runs, while `gardener.enabled` is false, and where the gardener's schedule is not running on this server. |
| Release a held write | `POST /console/held/{id}/release` | [Write screening](#held): lands a held agent write exactly as the agent sent it, and marks the memory owner-confirmed. Refused when its target changed after it was held. |
| Discard a held write | `POST /console/held/{id}/discard` | Lands nothing and erases the held text; the row stays as the record. |
| Clear a session's outside-text mark | `POST /console/held/sessions/{id}/clear` | The only way the mark comes off: the session's changes to binding rules land without a hold again. |
| Turn lockdown on or off | `POST /console/held/lockdown` | `on=1` or `on=0`. Your switch only: a lockdown the configured lockdown file holds stays on until that file goes. |
| Re-run the review | `POST /console/held/review` | Screens every memory and note again and marks the clean ones safe as they read now; your own marks on unchanged items stand. |
| Mark a flagged item safe | `POST /console/held/corpus/safe` | `path=` the item's file. Safe as it reads now; a memory is also stamped owner-confirmed. |
| Accept a file changed outside Seamless | `POST /console/held/corpus/accept` | `path=` the file. Records it as it stands, lifts its quarantine, and stamps a memory owner-confirmed. |
| Set a project's isolation | `POST /console/projects/{slug}/isolation` | Changes how far a project's knowledge travels. A tightening that would cut existing context first renders a confirmation step; nothing is written until it is confirmed. |
| Mark the mishaps reviewed | `POST /console/mishaps/reviewed` | [Mishaps](#mishaps): **Mark reviewed** stores when you reviewed the reports, through the newest one the page drew (`through=` its time; now when absent, never later than now). The Overview's mishap card then counts only the reports after it, and is gone until one arrives. The stamp never moves back: a mark through an older report writes nothing and says so. Records `mishaps.reviewed` with `by: console`. Refused from an agent's process on the peer_auth socket, and from a paired device. |
| Begin a lock change | `POST /console/lock/{project}/begin` | Only under [peer_auth](https://thereisnospoon.org/docs/guides/peer-auth/#lock-from-the-console-or-force-lock-in-the-file), and only from a signed-in browser. `action=lock` or `remove`. Refuses first a change that could not apply - a force-locked project, a lock already on, no console lock to remove, a project that is not confidential or sealed, no passkey enrolled - then returns a WebAuthn challenge naming this ceremony, the project and the direction. Writes nothing. A begin never waits for room: at most 4 ceremonies wait at once for one project and direction, and 32 in all, so a begin past 4 displaces the oldest of its project and direction, and one past 32 the oldest of the project and direction holding the most. A displaced ceremony's finish says it expired. |
| Lock a project, or remove its lock | `POST /console/lock/{project}/finish` | Verifies the passkey assertion for that ceremony, then stores the lock or removes it. Turning a lock on ends, in the same transaction, the project's agent leases, read grants and console unlocks, so every agent and browser asks again. It removes only a lock the console set: one `peer_auth.force_locked` holds stays. Seamless then restarts to apply the change. A finish for a ceremony that began and then failed records `access.console_lock_failed` under the ceremony's own project and direction, with why: the finish named another project or direction, the passkey's answer did not verify, the passkey is not enrolled, its counter did not increase, another use of it was recorded first, or the project changed since. A finish naming no waiting ceremony records nothing. |
| Begin a passkey unlock | `POST /console/unlock/{project}/begin` | Only for a project peer_auth locks, and only from the signed-in browser that proves its unlock key. Records a waiting [console unlock](https://thereisnospoon.org/docs/guides/peer-auth/#leases-and-the-process-tree) bound to this browser's cookie and key, sets the `seamless_unlock` cookie when the browser holds no live unlock yet, and returns the passkey challenge. Refused while no passkey is enrolled; the terminal path still works. Opens nothing yet. A begin never waits for room: at most 4 passkey unlocks wait at once for one project, and 32 in all, so a begin past 4 displaces the project's oldest, and one past 32 the oldest of the project holding the most. Terminal unlocks are counted apart. |
| Unlock a project in this browser | `POST /console/unlock/{project}/finish` | Verifies the passkey assertion for that waiting unlock, from the browser that began it, and makes it live: this browser reads the project for three hours, or less when `peer_auth.max_lease` is shorter. It is never a lease: no agent reads the project through it. |
| Begin a terminal unlock | `POST /console/unlock/{project}/terminal` | **Approve from a terminal**: the passkey begin's gates, without needing a passkey. Records a waiting unlock bound to this browser and returns a pairing code with the `seamlessd console-open --unlock` command that approves it, run as the daemon's account within ten minutes. The code only names that unlock, so it is not a secret. Opens nothing until the command runs. Capped as the passkey begin is, apart from it: at most 4 wait for one project, and 32 in all. |
| Ask for access | `POST /console/access/request` | The agent's ask, not your decision: `seam access request`, run from the agent's own shell, files an [access request](https://thereisnospoon.org/docs/guides/peer-auth/#requests) for a locked project or, with [access requests](https://thereisnospoon.org/docs/guides/peer-auth/#read-grants-and-the-advisory-tier) on, what the agent's conversation and a confidential or sealed project no lock covers call for: a grant, or a move into the project. With `write=true` it asks for a read-and-write grant or a move, a locked project's included, whose approval opens the lock too. It writes a waiting request, with the agent's reason of at most 200 characters, and opens nothing. An agent that already holds the access is answered with it. |
| Approve an access request | `POST /console/access/{id}/approve` | Your decision: writes the agent's lease on a locked project, or a grant, for `duration=` (until the session ends, 1 hour or 4 hours), and only while the request's facts still match `digest=`, the digest of the facts you were shown. A grant is read-only unless `access=read-write` on a request that offers the choice. A move request is approved for good: it ends the conversation's session and opens its continuation in the project, and `duration=` bounds only a locked project's lock. Under peer_auth it takes a proof: a passkey assertion from the request's page, or a Touch ID signature from Seamless Approve (`proof=touchid`), accepted on the peer_auth socket only and never for a move. In the advisory tier - access requests without peer_auth - a decision with no passkey is the bearer key's, which is what `seam approve` sends. A paired device's approval with no passkey or Touch ID proof is refused, peer_auth on or off: the bearer key's decision is never a device's. |
| Deny an access request | `POST /console/access/{id}/deny` | Closes a waiting request with no lease, grant or move; the project stays closed to that agent, and a move's agent stays where it works. Refused over the peer_auth socket and from a paired device, but for [your own hands through a relay](https://thereisnospoon.org/docs/guides/peer-auth/#your-own-decisions-through-a-relay): deny on the console, or with `seam approve` in your own terminal on the server. |
| Revoke an agent's lease | `POST /console/access/leases/{id}/revoke` | **Revoke** on a lease's row under Open right now on `/console/access`: ends that lease before its expiry and records an `access.revoked` event. Refused over the peer_auth socket and from a paired device, but for [your own hands through a relay](https://thereisnospoon.org/docs/guides/peer-auth/#your-own-decisions-through-a-relay). |
| Revoke a read grant | `POST /console/access/grants/{id}/revoke` | **Revoke** on a grant's row under Open right now on `/console/access`: ends a live grant, read-only or read and write, before its expiry. Its holder stays write-confined, since a revoke does not unread what it read. Refused over the peer_auth socket and from a paired device, but for [your own hands through a relay](https://thereisnospoon.org/docs/guides/peer-auth/#your-own-decisions-through-a-relay). |
| Lock a console unlock again | `POST /console/access/unlocks/{id}/end` | **Lock again** on an unlock's row under Open right now on `/console/access`, or **Lock it again now** on the unlock page: ends any browser's live console unlock, by id. Refused over the peer_auth socket and from a paired device, but for [your own hands through a relay](https://thereisnospoon.org/docs/guides/peer-auth/#your-own-decisions-through-a-relay). |
| Begin a passkey enrollment | `POST /console/approvers/passkey/begin` | Only under peer_auth: `name=` the passkey (1 to 64 characters on one line). Returns the WebAuthn creation options, excluding every passkey already [enrolled](https://thereisnospoon.org/docs/guides/peer-auth/#enroll-an-approver). Holds the enrollment in memory and stores nothing. Refused over the peer_auth socket and from a paired device, since a passkey is created in a browser. A begin never waits for room: at most 16 enrollments wait at once for their browser, and 16 more whose passkey exists and waits for its code, so a begin past 16 displaces the oldest still waiting for its browser, and never one waiting for its code. A displaced enrollment's next step says it expired. |
| Create the passkey | `POST /console/approvers/passkey/finish` | Verifies the browser's new passkey and returns its key's fingerprint with the `seamlessd approvers enroll-code` command that mints its code. Still stores nothing: the passkey waits for that code. Past 16 waiting for their code, it displaces the oldest of them. Refused from a paired device. |
| Enroll the passkey | `POST /console/approvers/passkey/confirm` | `code=` the one-time code that command minted for this key. Stores the passkey and uses up the code in one transaction. A code that is mistyped, expired, used or minted for another key leaves the passkey waiting, so it can be typed again. Refused from a paired device. |
| Enroll a Touch ID key | `POST /console/approvers/touchid/enroll` | Seamless Approve's call, from the app `seam touchid enroll` opens, accepted on the peer_auth socket only - never with the TCP key or the console cookie. Stores the Mac's new Touch ID key, proven by an enrollment code minted for that key and the key's signature over it. An earlier Touch ID key of the same Mac that signs its own retirement is retired in the same step. No console route removes any other credential: that is `seamlessd approvers remove`. |
| Begin a passkey sign-in | `POST /console/signin/begin` | Only with `console.sign_in: passkey`, and only from a browser that proves its key (the login page sets it up). Returns the WebAuthn challenge, bound to that key and to a ticket that works once. Writes nothing. A 404 while the key signs browsers in. |
| Sign in with a passkey | `POST /console/signin/finish` | Verifies the assertion for that challenge, from the browser that began it, records the passkey's use, and sets a fresh random sign-in cookie that counts only with this browser's key, for 7 days. A cookie the browser held before ends with it. |
| Begin adding a passkey | `POST /console/passkeys/add/begin` | Only with `console.sign_in: passkey`: the public Add a passkey page asks for the new passkey's `name=`. Returns the WebAuthn creation options. Writes nothing. |
| Add a passkey, waiting | `POST /console/passkeys/add/finish` | Verifies the device's new passkey and stores it as waiting, with a short code, for 10 minutes. It adds nothing: `seamlessd approvers approve --code`, on the server, adds exactly that passkey. It never waits for room: at most 4 wait at once from one address (an IPv6 address counts by its /64, and every loopback address is one), and 32 from every address but loopback, whose 4 are its own. One past its address's 4 displaces that address's oldest, and one past the 32 the oldest of the address holding the most; a displaced passkey's code names nothing, and its page says to add it again. So someone looping on the page from one computer crowds out only their own, and no number of other hosts reaches a passkey added on the server itself. Behind a proxy every request comes from the proxy's address. |
| Begin a terminal sign-in | `POST /console/signin/terminal/begin` | Only with `console.sign_in: passkey`, and only from a browser that proves its key: **Show a code for the server's terminal** on the login page. Sets a fresh random sign-in cookie bound to that key and stores it waiting, with a short code, for 10 minutes, with the address the request came from and its User-Agent (printable ASCII with each run of spaces as one, at most 256 characters), which the answer also carries for the page to show beside the code. It signs nothing in: `seamlessd console-open --sign-in --code`, on the server (as the daemon's account under peer_auth, at a terminal), shows those and when it began, asks, and on a yes signs in exactly the browser that showed that code, for 7 days. It never waits for room: at most 4 wait at once from one address (an IPv6 address counts by its /64, and every loopback address is one), and 32 from every address but loopback, whose 4 are its own. A begin past its address's 4 displaces that address's oldest code, and one past the 32 the oldest of the address holding the most; a displaced code approves nothing, and its page says to show a new one. So someone looping on **Show a code** from one computer crowds out only their own, and no number of other hosts reaches a code a browser on the server itself showed. Behind a proxy every request comes from the proxy's address. A 404 while the key signs browsers in. |
| Get a pairing code | `POST /console/devices/code` | Only on a server that serves other computers (TLS, a bind beyond loopback, or a `server_url` naming another host). Mints a one-time pairing code, valid for 10 minutes and for one computer, and shows it once, in this answer, with the client one-liners, which carry the server certificate's fingerprint when it serves TLS; only its hash is stored. Without peer_auth the code is the approval; under peer_auth the pairing it starts waits for a passkey. At most 16 codes wait at once. Refused (409, `pairing needs TLS here`) where the server's address is plain http to another host. Refused (409, `server_url names a user`) where it names a user. Refused from an agent's process on the peer_auth socket, and from a paired device, peer_auth on or off. |
| Begin a device approval | `POST /console/devices/{id}/approve/begin` | Only under peer_auth, from a signed-in browser, for a pairing that waits, or for a relay that waits for its approval in place: active, paired with its one-time code alone, and mapped in `peer_auth.relays`. Returns the WebAuthn challenge, bound to that device's id, kind, name, fingerprint and key (and, in place, to its status and that purpose), and a ticket. Writes nothing. |
| Approve a device | `POST /console/devices/{id}/approve/finish` | Verifies the passkey assertion for that ticket over the device as it stands, records the passkey's use, and pairs the device: a relay's certificate is accepted from its next request, and a key device collects its own key, once, on its next status. A relay waiting in place is approved where it stands, the same device id and certificate, in one compare-and-set that refuses a relay revoked meanwhile; its requests are judged from the next one on, and an `access.device_paired` event marked `in_place` records it. |
| Revoke a device, or deny a pairing | `POST /console/devices/{id}/revoke` | Revokes a paired device: its credential is refused from its next request, and the requests it is making end. A pairing that waits is denied instead. Refused from an agent's process on the peer_auth socket, and from a paired device, peer_auth on or off. |
| Save briefing settings | `POST /console/settings/briefing` | Writes the briefing knobs as a runtime **override row** in the DB. It never writes the config file. |
| Reset briefing settings | `POST /console/settings/briefing/reset` | Clears the override row, reverting to the file/env configuration. |
| Preview a briefing | `POST /console/settings/briefing/preview` | A read, not a write: it POSTs only to carry the Briefing form's unsaved values, which it checks with the save's own validation, and answers with the briefing a new session in the chosen project would start with. Writes no settings row and records no event. A locked project is not previewed. |
| Force a project's ranking by use | `POST /console/settings/utility` | Knowledge engine's **Auto / On / Off** on a project's row (or the global scope's, **Global context**), which saves at once with Undo: sets or clears the owner's per-project force. `on` and `off` win over the gardener's readiness latch, `auto` defers to it again. The answer returns to the row. |
| Switch embeddings off or back on | `POST /console/settings/embeddings/mode` | Stores or clears the embedder off switch. It is read at serve start, so the change applies from the next restart - the page says so. |
| Re-embed everything | `POST /console/settings/embeddings/reembed` | Starts a background pass that re-embeds every memory and note with the active model. It changes vectors, never content. |
| Choose the console level | `POST /console/settings/level` | Stores the [experience level](#choose-how-much-you-see) as a row that wins over file and env. Presentation only: nothing an agent receives changes. |
| Reset the console level | `POST /console/settings/level/reset` | Clears that row, back to the file/env level. |
| Dismiss the welcome card | `POST /console/settings/level/welcome` | Records that the Overview welcome card was seen, without choosing a level. With the card's write-screening box ticked, it also turns that feature on, as a level choice with the box ticked does. |
| Save optional features | `POST /console/settings/features` | Writes every feature switch as a stored **override row** in the DB; a switch posts the whole form as soon as it changes. It changes what is exposed - console screens and the matching agent tools - and deletes nothing. |
| Reset optional features | `POST /console/settings/features/reset` | Clears the override row, reverting to the file/env configuration - which, unless you set the keys there, means every optional feature is off again. Still deletes nothing. |
| Turn update checks on or off | `POST /console/settings/updates` | Stores the console's `update.check` override (`check=on` or `off`). It cannot turn checks on while the config file or environment says `update.check: false` - that is final, and the switch is locked. Off is your decision alone: refused (403) from an agent's process on the peer_auth socket and from a paired device, whatever checks are now; on is open to them. Installs nothing. |
| Reset update checks | `POST /console/settings/updates/reset` | Clears that override - both switches - handing them back to the config file (or, without one, the build's default). A build that is not a published release does not check by default, so there a reset that drops this console's `check=on` turns checks off, and automatic updates with them: it is refused like turning them off, unless the file or environment locks checks off already. It is refused even where the file's `update.check: true` would keep them on, since the console cannot see the file's update block under its own override. Every other reset, and every reset on a release build, is open to any caller. |
| Check for updates now | `POST /console/settings/updates/check` | Asks GitHub's release list once, right away - refused while checks are off or within a minute of the last check. Reads; installs nothing. |
| Turn automatic updates on or off | `POST /console/settings/updates/auto` | Stores the console's `update.auto` override (`auto=on` or `off`), keeping its `update.check`. It cannot turn automatic updates on while the config file or environment says `update.auto: false`, or locks `update.check: false` (nothing installs without checks). With checks merely off, the choice is kept and applies once they are back on. Off is your decision alone, refused as turning checks off is; on is open to any caller. Only an install that updates itself acts on it: the installer's, or a [peer_auth system daemon](https://thereisnospoon.org/docs/guides/peer-auth/#updates)'s. |
| Update now | `POST /console/settings/updates/apply` | Installs the newest release at once, after a confirm: it skips the soak, the wait for idle agents, skipped releases and the backoff, and lifts a hold, whether automatic updates are on, off or paused. Refused while checks are off, on an install that does not update itself, while an update is under way, within a minute of a failed release check, when nothing newer is known, or when the release carries no signed checksums bundle. The daemon restarts to finish. |
| Resume automatic updates | `POST /console/settings/updates/resume` | Lifts the pause automatic updates put on themselves after rollbacks, and the hold a deliberate downgrade set. Changes no setting; skipped releases stay skipped, and a running backoff runs out. |
| Save the gardener's switches | `POST /console/settings/gardener` | Stores the console's choice for **Agents may apply proposals** (`agents=on` or `off`, over `gardener.agents_may_apply`; the section's switch sends `agents_set=1`, with `agents=on` only while it is on, so its absence is off) or for **Apply by themselves** (`auto_set=1` and one `auto=<kind>` per kind switched on, over `gardener.auto_apply`); each control's form carries only its own fields, and each saves the moment a switch changes. It cannot turn agents' applying on while the config file or environment says `gardener.agents_may_apply: false` - that is final - and it refuses any kind but `memory_wanted`, `tool_error`, `abandon_plan` and `ship_plan`. Your decision alone in every direction: refused (403) from an agent's process on the peer_auth socket and from a paired device. Applies at once, with no restart; a proposal already waiting is not applied by it. Records a `gardener.action` event. |
| Reset the gardener's switches | `POST /console/settings/gardener/reset` | Clears that override - both switches - handing them back to the config file, and records a `gardener.action` event. Refused from an agent's process and from a paired device, as saving is. |
| Save the backup schedule and folder | `POST /console/settings/backups` | Stores the console's `backup:` override: only the values that differ from the settings in force. A new folder is created and tested with a write first. Applies from the next backup. |
| Reset the backup settings | `POST /console/settings/backups/reset` | Clears that override, handing the schedule and the folder back to the config file (or, without one, the build's default). |
| Back up now | `POST /console/settings/backups/now` | Writes a backup at once, marked **Made by you**: never skipped, never deleted automatically. With `run=retry` (the alert card's **Try again**) it runs the schedule's backup instead, which is skipped when nothing changed and prunes after itself. Refused while a backup runs or a restore is being prepared. |
| Delete a backup | `POST /console/settings/backups/delete` | `ref=` a backup from the list, never a path. Removes the file from its folder, after a confirm. |
| Begin a restore's passkey ceremony | `POST /console/settings/backups/restore/begin` | Only while [peer_auth](https://thereisnospoon.org/docs/guides/peer-auth/#restore) locks a project, and only from a signed-in browser. Returns a WebAuthn challenge bound to the backup file's SHA-256, which the server reads from the file itself, and to the mode. Writes nothing. A begin never waits for room: at most 4 ceremonies wait at once for one backup and mode, and 32 in all, so a begin past 4 displaces the oldest for its backup and mode, and one past 32 the oldest of the backup and mode holding the most. A displaced ceremony's finish says it expired. |
| Restore a backup | `POST /console/settings/backups/restore` | `ref=` and `mode=replace` or `merge`. Verifies the backup and prepares the restore while Seamless serves, then restarts to finish it. A replace saves today's data as a backup first. Refused while a backup, another restore or an update is under way. Under the passkey gate it also needs the ceremony's `id` and `response`, and the file's digest must still match. |
| Cancel a staged restore | `POST /console/settings/backups/restore/cancel` | Drops a restore that is waiting for its restart. Nothing changes. |
| Dismiss a restore's result | `POST /console/settings/backups/restore/dismiss` | Hides the last restore's result card. Changes no data. |
| Save a project family | `POST /console/settings/families/save` | Creates a family or replaces one family's name and member set - the same `project_families` setting `seamlessd family` manages. Members come from a closed picker of registered projects, so a typo cannot create an inert member. |
| Delete a project family | `POST /console/settings/families/delete` | Removes the whole family. Its projects lose the sibling-findings channel; nothing else about them changes. |
| Sign in / sign out | `POST /console/login`, `POST /console/logout` | Sets or clears the console cookie. Touches no data. |

**Your decisions come through a paired device only as your own hands.** On a
server that serves other computers, a request that comes with a paired device's
credential - a relay's certificate or a device key - is refused (403) on every
write that is yours to make: a pairing code and a device's revocation, Held's
release, discard, session marks, lockdown and corpus marks, the optional
features, turning update checks or automatic updates off (and a reset that would
turn checks off), every Backups route, bringing a project's fence down, a deny,
a revoke, Lock again, a passkey's enrollment, and an approval with no passkey or
Touch ID proof. That holds with peer_auth on or off, with one exception: under
peer_auth, a request through a relay you map whose process chain the server
judges your own hands there - every process named, none an agent, the chain
whole, and your uid there the one both the mapping and the relay's pairing name
([the rule](https://thereisnospoon.org/docs/guides/peer-auth/#your-own-decisions-through-a-relay)) - makes them,
and each one it makes records an `access.relay_decision` event (item: the relay's
device; payload: its name, your uid there, the route and the chain's programs by
name). Its approval with no passkey or Touch ID proof still goes to the approve
core, which refuses it: a chain is no proof. A key device, a relay to a server
without peer_auth, and any other chain carry their agents, never your hands, and
the refusal says why: make these decisions in the console of the server you sign
in to, in a terminal on the server itself, or in a terminal of your own behind a
relay you map. The reads stay open to a device, and so do the writes an agent
may make: `seam` on a client computer reads the console's JSON through its relay,
and the Touch ID helper there approves through it with its own signature.

Read the shape of that list. There is no "create memory", no "edit note", no
"delete memory", no "add task", no "start session". The direct writes to knowledge
state are **archive** and **restore a memory**, **delete a note**, **approve a
captured plan**, the **star**
flag, and - with write screening on - **releasing a held write**, which lands
exactly what an agent wrote, never text of the console's own; the rest either
manage gardener proposals - which are themselves
proposals, reviewed before they do anything - or move a task the way
`tasks_update` does (release its claim, mark it done, drop or reopen it, mark
its gate as handled or say what it waits on), or record a decision about the
work (a repo lock's declaration or hold, a project's isolation, the mishaps
marked reviewed), or set a configuration knob (briefing overrides, feature
switches, project families, the update switches, the gardener's switches, the
backup schedule and folder) that shapes future briefings, what is exposed, or
how the install runs, without touching any memory's content. **Back up now** and **Delete a backup** make and remove copies of the
data, never the data itself. The console level is presentation state of the same kind: it changes
what this console shows you and nothing else. The access, lock, unlock and
approver rows exist only with [peer_auth](https://thereisnospoon.org/docs/guides/peer-auth/) or access
requests on (both optional, and off by default; every one of those routes
answers 404 otherwise), and they are your decisions about who may reach a
project, not writes to knowledge: they record requests, leases, grants, moves
(a move ends the conversation's session and opens its continuation in the
project it moved into), console unlocks, locks and approver credentials, and
never a memory's or a note's content. The one an agent calls, **Ask for access**, only files a request for
you to decide. **Update now** and **Resume
automatic updates** act on the install rather than on knowledge: the first
starts the same backed-up update `seamlessd update` runs, the second lets
automatic updates run again. A **restore** is the one write that changes
knowledge wholesale, and it puts back what a backup of this install held,
never text of the console's own: the data from before is saved as a backup
first, so a restore is itself undone by restoring.

This is deliberate, and it is the same principle as
[the gardener's](https://thereisnospoon.org/docs/concepts/gardener/) propose-only default. The store is written
by agents doing work, with provenance attached. A console that could quietly edit
a memory would produce knowledge that came from nowhere, attributable to no
session, explaining nothing.

Every write action redirects back with a flash message (`?notice=` for success,
`?error=` for failure) rather than rendering a result page in place, so a reload
never repeats the action. With script, a success notice surfaces as a toast and
both parameters are stripped from the address bar, so a later live refresh never
replays a message about an action that already happened; an error also stays on
the page as a banner.

## Signing in

There is one credential in the whole system: the static bearer key
(`mcp.api_key`). It guards `/api/mcp`, the hook endpoints, and the console alike.

Two ways to present it:

- **A browser** trades the key for a cookie at `/console/login`. The cookie value
  is a SHA-256 hash of the key, not the key - so the raw credential never sits in
  the browser's cookie jar. It is `HttpOnly`, `SameSite=Lax`, and scoped to
  `/console`. The login page is one card: the key's field and **Sign in**, with
  **Where is my key?** folded under it (the config file that holds the key,
  `SEAMLESS_MCP_API_KEY`, and `seamlessd console-open`, which opens the console
  already signed in).
- **The `seam` CLI** sends the key as a bearer token on the `Authorization`
  header, and asks for JSON.

Unauthenticated browsers are redirected to the login page with a `?next=` that is
validated against an open-redirect (an off-site or absolute candidate becomes
`/console/`). Unauthenticated JSON callers get a 401.

Public routes are the login page, the static assets (`console.css`,
`interactions.js`, `search.js`, `favicon.svg` and the rest under
`/console/static/`) and the service worker at `/console/sw.js`. Everything else
requires the key.

A browser sends its cookies to every port of the same host, so a program that
can open your browser on another local port receives the console cookie too.
Today that adds nothing to what reading the key gives such a program. A
[console unlock](https://thereisnospoon.org/docs/guides/peer-auth/) of a locked project is worth more, and so
is a passkey sign-in, so neither is just a cookie: each also needs a key the
browser keeps for the console's own address, and a service worker signs each
console request with that key. The service worker runs in a browser that has
started an unlock, and under passkey sign-in in every signed-in browser.
Before an unlock or a sign-in the page asks the daemon for the worker again,
and when an upgrade brought a new one, it waits while the new worker takes
over, so the old one never signs for the new daemon. The worker and its key script
(`/console/static/browserkey.js`) are served `no-cache`, so the browser checks
with the daemon each time it loads them. The other static files are linked by
version ([After an upgrade](#after-an-upgrade)).

### Sign in with a passkey {#sign-in-with-a-passkey}

Set `console.sign_in: passkey` in the daemon's config (off by default; file
only) and restart it, and a browser signs in with a passkey you added, in any
browser, the server's included. No browser needs the key: the login page no
longer takes it, and a cookie derived from it signs nothing in. The `seam` CLI
keeps sending the key.

The sign-in page keeps its sky, and its right column is one card: **Sign in
with a passkey**, how long a sign-in lasts (its hint says which passkeys work
here), then under "no passkey here?" the two other ways in, **Show a code for
the server's terminal** and **Add a passkey on this device**.

- **Signing in.** The login page sets up the browser's key and service worker,
  then asks for your passkey: this device's, one your password manager syncs
  here, or your phone's through the browser's QR prompt. The daemon answers with
  a random cookie and keeps only a hash of it and the browser's key together, so
  the cookie read on another port opens nothing. A sign-in lasts 7 days in that
  browser, or until you sign out from the sidebar (which also drops its console
  unlocks), or until the passkey that made it is removed. A restore signs every
  browser out.
- **A page drawn without the proof** (a hard reload skips the worker) lands on
  the login page, which sets the worker up and asks for the page again. A
  browser that cannot run a service worker (some private windows) cannot sign
  in.
- **Adding a passkey for a new device.** On the server, `seamlessd approvers
  add` prints the Add a passkey address (`/console/passkeys/add` on the passkey
  origin). It is not a secret: anyone who reaches the console can open it, and
  nothing is added until you approve. Open it on the device and create the
  passkey; the page shows a short code and the key's fingerprint. Run
  `seamlessd approvers approve --code 7KQ2-M9XD` on the server (as the daemon's
  account under peer_auth; the page shows the exact command). It shows the same
  name and key and asks before it adds, so it runs only at a terminal (over
  ssh, `ssh -t`), and no flag skips the question. Approve only a code your own
  device shows.
- **No passkey at hand.** **Show a code for the server's terminal**, under "no
  passkey here?", begins a terminal sign-in: the browser gets a cookie bound to
  its key and shows, as steps, the code, the command that approves it and what
  that command will show of this browser,
  `/usr/bin/sudo -k -u _seamless /usr/bin/env SEAMLESS_CONFIG=/opt/seamless/etc/seamless.yaml /opt/seamless/bin/seamlessd console-open --sign-in --code 7KQ2-M9XD`
  under peer_auth (`seamlessd console-open --sign-in --code 7KQ2-M9XD`
  without it). Approved, that browser is signed in for 7 days, as with a
  passkey, and the page goes on by itself. The command first shows the browser the code names (when it began, its address and User-Agent) and asks; say yes only to a code your own browser shows you now. The code signs in no other browser,
  so reading it off your screen gives nothing away. It is the way back in when
  every passkey is lost: add a new one from the Add a passkey page.
- **Synced passkeys.** A passkey synced through iCloud Keychain, Google Password
  Manager or another password manager is one passkey: add it once, and every
  device it syncs to signs in, with nothing more to add. A device without it can
  use your phone's passkey through the browser's QR prompt.
- **The server's name.** A passkey works only for the name it was made for. One
  made for another site, or for `localhost` on another install, does not work
  here. With `localhost` passkeys only a browser on the server itself can make or
  use one. Renaming the server, or turning TLS off, strands every passkey made
  for the old name (doctor names them).

### Approvers {#approvers}

Under [peer_auth](https://thereisnospoon.org/docs/guides/peer-auth/), `/console/approvers` lists the passkeys
and Touch ID keys that approve: one card per key, with what it is (a passkey, a
synced passkey, or Touch ID in the agent's dialog), when it was last used and
the day it was added. A key's hint (its three dots) holds its id and the command
that removes it, run in your own terminal as the daemon's account. The page
never removes a key: the console's key is no proof of you.

**Add one** offers two tiles. **A passkey** takes three steps: name it and
create it in this browser, make its one-time code in your own terminal (the page
shows the command, with Copy; the code enrolls only the passkey you just
created), then enter the code. **Touch ID on a Mac** is `seam touchid enroll`,
with Copy. Under passkey sign-in a passkey is added from the device that will
hold it, so the passkey tile links the Add a passkey page and the command that
approves its code instead. With passkeys off, or on an address that is not the
passkey origin, the tile says so. The Add a passkey page walks the same way in
three steps: create it there, approve its code on the server, sign in with it.

### `make console`

```bash
make console          # open in the default browser, already signed in
make console-chrome   # same, but force Google Chrome
```

This builds, then runs `seamlessd console-open`, which renders a one-shot
self-submitting login page to a `0600` temp file and opens it. The page POSTs the
key to `/console/login`, which sets the cookie and 303s into the console - so you
land on an authenticated page with nothing to paste. It refuses to run if
`mcp.api_key` is empty or the server is not answering `/healthz`. Under passkey
sign-in, console-open opens the sign-in page instead and writes no page and no
key.

`make console-chrome` exists for agents: they drive Chrome, so this hands the auth
cookie to the browser they can actually see. (`--browser` is macOS-only.)

## Getting around

The sidebar groups the screens by the job they serve rather than by table,
in this order:

| Group | Screens |
|---|---|
| **Access** | Access requests, Approvers (only while `peer_auth` locks a project; Access requests alone while read grants are on). Whenever it exists it is the first group: a waiting request is an agent waiting on you. |
| **Live** | Overview, Now, Interactions, Mishaps |
| **Work** | Diary (only while that [optional feature](#optional-features) is on), Projects, Plans, Tasks, Sessions, Locks (only while that [optional feature](#optional-features) is on) |
| **Knowledge** | Memories, Notes, Retrieval, Gardener, Held (only while [write screening](#held) is on, or restricted accounts are configured) |
| **Research** | Labs, Trials (only while that [optional feature](#optional-features) is on) |
| **Server** | System, Privacy, Devices (only on a server that [serves other computers](#devices)) |

That is the full list, at the Advanced [level](#choose-how-much-you-see); a
lower level shows a subset, in the same order and under the same names.
Above the groups, a **Search or jump to** field opens the command palette;
Settings and the account row (the server the console runs on, by its machine
name, the theme, sign out) close the sidebar. Only the groups scroll, with a fade at an edge that has more
beyond it, so the search field, Settings and the account row stay on screen
at any window height. The sidebar collapses to an icon rail (the header
button, or `[`) and remembers that per browser; under 720px it becomes a
drawer behind the menu button, with every section's label and badge.

A number beside a section is one of two kinds:

| Kind | Sections | Shows |
|---|---|---|
| **Waiting on you** | Access requests, Mishaps (reports since you last marked them reviewed), Plans (captured plans awaiting your approval), Tasks (gates waiting on you, lapsed claims, and tasks in progress with no claim), Locks (declarations waiting for your approval), Gardener (proposals waiting for review), Held | A pill while something waits, and nothing at zero. Each counts what the Overview's matching **Needs you** cards count. |
| **Live** | Now | The agents live right now, dimmed at zero. |

The other sections carry no number: their totals are on the
[Overview](#overview) and at the head of each screen. On the collapsed rail a
section with something waiting carries a dot on its icon, and on a phone the
menu button carries one while anything in the drawer waits. The badges follow
the live refresh like the rest of the page.

Every screen title carries an (i) button that says, in one line, what the screen
is for.


### Read in place {#read-in-place}

A plain click on a link to a memory, a note, a task, a plan, a session, an
event or a trial opens it in a wide panel over the page. The page stays as it
was behind it: never reloaded, its scroll and open folds kept, its live
updates going on. A click on a row of Sessions, Search, Mishaps, the Tasks
list or a project's tabs does the same. A project link still opens the project.

- The panel's header says where the item lives (its project, then Memories,
  Notes, Tasks and so on, or a task's `plan:<slug>`), with **k** and **j**
  for the previous and next item of the list it was opened from, **Open full
  page**, and **Esc Close**. On a phone it takes the whole screen.
- `Esc`, **Close**, a click beside the panel or the browser's Back closes it,
  and focus goes back to the link that opened it. A link inside the panel
  opens that item in it, a step that Back takes back.
- The address carries `#peek=` and the item, so a reload opens the panel
  again.
- An action inside it (**Mark done**, the star, **Archive** and the rest)
  applies there: its message shows, and the panel shows the item as it is
  now, or closes when the item is gone (a deleted note). The page behind
  catches up through its live refresh.
- A `Cmd`/`Ctrl`-click, a middle click, or a link from outside the console (an
  agent's message, a new tab) opens [the item's own page](#item-page). When
  you go there from a console page (**Open full page**), its way back reads
  **Back to** that page's title and returns there.
- The [Diary](#diary) and [Interactions](#interactions) keep the pane they
  open beside the day or the feed.

### Hints, switches that save at once, and time left {#shared-parts}

Settings and the lock screens share a few parts.

- **A hint** is a small (i) beside the thing it explains. It opens on hover,
  on keyboard focus, or with a tap, which pins it open on a phone. `Esc`, a
  tap outside it, or opening another hint closes it, so one is open at a time,
  and an `Esc` that closes a hint leaves open what lies under it. Its words
  are in the page: a screen reader reads them with the button.
- **A switch that saves at once** posts as soon as it changes, through the
  same in-place path as every other console write, and waits, disabled, for
  Seamless to answer. The toast then names the change ("Momentum is off.")
  with **Undo**, which posts the values from before it. A save that fails puts
  the switch back and says why, with **Try again**. Typing never saves: a form
  with a typed value (a number, a folder) keeps its **Save** bar. A toast with
  a button stays for eight seconds, and for as long as the pointer or focus is
  on it.
- **Time left** on a lease, a grant or an unlock reads "2 h 41 m left". It
  counts down every 30 seconds while the page is in sight, turns amber from 30
  minutes, when renewal warnings start, and stops at **ended**. Its tooltip is
  the end, in your own clock. A lease with no end of its own reads **until the
  session ends**.
- **Steps** list a ceremony with more than one step, each done (a check), the
  one under way, or still to come, numbered: a restore's progress in
  [Backups](#backups) is one.
- **The source chip** in a [Settings](#settings) section's head says where its
  values come from: **Config file**, **Set here** with **Reset**, or **Config
  file decides** with a lock when the config file makes a value final. Its
  hint says more.

### The command palette

`Cmd/Ctrl+K`, the sidebar's search field, or `/` on a screen with no filter of
its own opens the palette. With nothing typed it offers **Recent** (the
memories, notes, tasks, plans, sessions, projects, labs and trials you last
opened in this browser, by their titles; an event page is never kept), **Jump
to** (every section the sidebar offers, with its shortcut, plus Search,
Context, and each Settings section the level shows), and **Actions** (switch
theme, collapse the sidebar, the shortcut sheet, change the experience level,
and, while the level shows Privacy, lock a project: Privacy's Locked card).
Typing matches sections and actions instantly;
from two characters on it also searches memories, notes, tasks, plans, trials,
projects, and sessions through the same route as [Search](#search), showing the
groups the level offers. Each group shows up to five, starred matches first and
marked starred; a group with more shows its count as **5+** and ends with
**More in Search**, which opens Search narrowed to that kind. An exact name of
a superseded or archived memory finds it here too, with its status.

### Keyboard

| Keys | Does |
|---|---|
| `Cmd/Ctrl+K` | Search or jump to a section |
| `/` | Focus this screen's filter; open the palette where there is none |
| `g` then a key | Go to a section (pressing `g` shows every key it can take, until your next key, `Esc` or a click): `a` Access requests, `k` Approvers, `o` Overview, `n` Now, `i` Interactions, `f` Mishaps, `d` Diary, `w` Projects, `p` Plans, `t` Tasks, `s` Sessions, `b` Locks, `m` Memories, `e` Notes, `r` Retrieval, `g` Gardener, `h` Held, `l` Labs, `x` Trials, `y` System, `v` Privacy, `c` Devices, `,` Settings |
| `j` / `k` | Next / previous item in a library rail (Gardener, Labs, Trials), the Memories list, the Notes list or the Tasks list, or, on [an item's own page](#item-page), in the list it is in |
| `Enter` | On the Memories, Notes or Tasks list, open the focused row's own page |
| `p` | Open the project switcher on Memories, Notes, Tasks and Plans |
| `a` / `x` | On the Gardener: apply / dismiss the open proposal. An apply that cannot be undone asks in the page first; press `a` again to confirm |
| `[` | Collapse or expand the sidebar |
| `?` | The shortcut sheet |
| `Esc` / `k` / `j` | In the [reading panel](#read-in-place): close it / the previous / the next item of the list it was opened from |
| `Esc` | Close the palette, a menu, a [hint](#shared-parts), the sheet, or the drawer |

The `g` map is read from the sidebar itself, so a section switched off in
Settings - or one the [level](#choose-how-much-you-see) leaves out of the
sidebar - takes its shortcut with it, and brings it back when it returns.

Moving between screens keeps the sidebar in place and settles the new content
in (cross-document view transitions, where the browser supports them), and
hovering a console link prefetches its page. Neither runs any code of the
destination page early; both degrade to a plain navigation.

### Ambient signals

The console shows activity without asking to be read. The line along the
sidebar's edge - the Seam - carries each event from the live stream as a spark
that travels to the section it belongs to and lights that section's icon:
cyan for context reaching an agent, green for knowledge written, coral for
something wrong, indigo for everything else. Each event also rings the daemon's
dot in the sidebar and the page's live dots once, and the sky behind the pages
brightens with the recent event rate and settles when the fleet is idle; the
dot dims while the live stream is down. Headline numbers count up when a page
opens; agents working right now carry a slow orbit of light on the Now screen.
The page settling in and the count-up play once per browser tab, on the first
page it shows; later pages in that tab appear at once.

All of it is decoration over data the page already states: nothing here changes
a number, and `prefers-reduced-motion` stills every part of it (the Seam sends no
sparks, numbers render at their value). Between events the sky and the sidebar
hold still, so a console left open on the Overview draws nothing while the
fleet is idle.

### After an upgrade {#after-an-upgrade}

Every page links its stylesheet and scripts by their version, a digest of the
file this build serves (`/console/static/console.css?v=...`). The browser
keeps each one until a build changes it, so the first page load after an
upgrade uses the new stylesheet and scripts, and loads after that fetch none
of them again. A static file asked for without its version, such as the
worker's import of `browserkey.js`, is checked with the daemon on each use.

A tab left open across an upgrade keeps the stylesheet and scripts it loaded
first while its views update in place. When the daemon now serves different
ones, the page says so once, in the corner: "Seamless was updated. Reload this
page to use the new version." The console never reloads the page itself;
Reload does, when you choose.

## Choose how much you see

The console has three **experience levels**. They change what the console shows
you and nothing else: your agents get the same briefing, the same MCP tools, the
same hooks, the same recall, and the same gardener at every level. The level is
presentation, stored for this installation's owner rather than per browser, so
your phone and your laptop agree.

**Basic** is the essentials, for someone who installed Seamless because they use
Claude Code or Codex and wants to know it works: the work the agents follow
(Projects, Plans, Tasks), what they did (Sessions) and what went wrong
(Mishaps), what they remember (Memories, Notes), cleanup suggestions to accept
or decline (Gardener), and an Overview whose health strip, right under what
needs you, says which agent clients are working here, whether semantic recall
is on, when the last briefing went out, and the version (which says when a
newer release is out) - plus the Experience, Features, Your setup, Updates,
Backups and Gardener settings.

**Standard** adds watching the agents live and looking after the server: Now
(and Locks, and Labs and Trials, when their features are on), System and Privacy, the
Overview's window, vitals, and projects table, the Briefing and Workspaces
settings, and the finer controls on the screens Basic already had.

**Advanced** is every screen and every knob: Interactions (the tool-call
transport), Retrieval (the analytics), Context, the Knowledge engine settings,
and raw event payloads.

A fresh installation starts at **Basic**, and Home carries a one-time welcome
card offering all three - pick one, or dismiss it; either way it does not come
back, on any device. Beside the levels the card offers one optional feature as
an unchecked box, **Also turn on write screening**: it rides with whichever
button you press, and unticked it changes nothing at any level. It is a
feature, not part of a level, because it changes what your agents receive (see
[Held](#held)); once the card is gone, Settings > Features is the way in.
**Settings > Experience** shows the level you are on, with the three levels side
by side and the list of what each one shows. The palette's **Change experience
level** action goes there too.

**Hidden, not locked.** A level removes screens from the sidebar, the palette,
and the shortcut map; it never locks them. A link to a screen above your level -
from these docs, an agent's finding, a briefing, a bookmark - still opens it,
under a note saying it is not in your sidebar, with a button to switch;
**Got it** sets the note aside for that screen in this browser. JSON
callers (`?format=json`, and so the `seam` CLI, `seam doctor` included) get the
same answer at every level.

**Upgrading keeps every screen.** An installation that already recorded sessions
before levels existed is set to Advanced by a one-time upgrade step, so nothing
you were using disappears; Settings > Experience says the level was set by the
upgrade, and the welcome card appears once to announce the choice.

Levels and [optional features](#optional-features) compose: a screen shows when
its feature is on **and** the level includes it. Features change what exists -
for the console and for agents alike; the level only changes how much of it you
see. The level can also come from the config file or environment (`console.level`,
`SEAMLESS_CONSOLE_LEVEL`) - a choice made in the console wins until you reset it
there. See [Configuration](https://thereisnospoon.org/docs/reference/configuration/).

What each level shows, generated from the console's own registries:

### Screens

| | Basic | Standard | Advanced |
|---|---|---|---|
| **Overview** | shown | shown | shown |
| **Now** | - | shown | shown |
| **Interactions** | - | - | shown |
| **Mishaps** | shown | shown | shown |
| **Diary** - while the diary feature is on | shown | shown | shown |
| **Projects** | shown | shown | shown |
| **Plans** | shown | shown | shown |
| **Tasks** | shown | shown | shown |
| **Sessions** | shown | shown | shown |
| **Locks** - while the repo_locks feature is on | - | shown | shown |
| **Memories** | shown | shown | shown |
| **Notes** | shown | shown | shown |
| **Retrieval** | - | - | shown |
| **Gardener** | shown | shown | shown |
| **Held** - while the screening feature is on | shown | shown | shown |
| **Labs** - while the research feature is on | - | shown | shown |
| **Trials** - while the research feature is on | - | shown | shown |
| **System** | - | shown | shown |
| **Privacy** | - | shown | shown |
| **Settings** | shown | shown | shown |
| **Search** - no sidebar entry: the palette and links reach it | shown | shown | shown |
| **Context** - no sidebar entry: the palette and links reach it | - | - | shown |

### Settings sections

| | Basic | Standard | Advanced |
|---|---|---|---|
| **Experience** - How much of the console you see, and how it looks | shown | shown | shown |
| **Features** - Optional features, for the console and agents alike | shown | shown | shown |
| **Your setup** - Version, files, and what is connected | shown | shown | shown |
| **Updates** - New releases, and whether this install checks for and installs them | shown | shown | shown |
| **Backups** - Automatic backups, the folder they go to, and restoring one | shown | shown | shown |
| **Gardener** - Who may apply what the gardener suggests, and what applies by itself | shown | shown | shown |
| **Briefing** - What every new agent session starts with | - | shown | shown |
| **Workspaces** - Projects, repo routes, and families | - | shown | shown |
| **Knowledge engine** - Semantic index, ranking, storage, and policy | - | - | shown |

### Within screens

| | Basic | Standard | Advanced |
|---|---|---|---|
| **Overview: the observation window** | - | shown | shown |
| **Overview: the vitals** | - | shown | shown |
| **Overview: the projects table and the knowledge rail** | - | shown | shown |
| **Sessions: the Retained filter** | - | shown | shown |
| **Sessions: the Sort menu** | - | shown | shown |
| **Sessions: a session's review signals** | - | shown | shown |
| **Memories: the Reach and Utility sorts** | - | shown | shown |
| **Memories: how often each memory surfaced** | - | shown | shown |
| **Memories: the Never surfaced filter** | - | shown | shown |
| **Memories: the utility score in the reader** | - | shown | shown |
| **Gardener: the Type and Source filters** | - | shown | shown |
| **Gardener: the project-split example** | - | shown | shown |
| **Gardener: the Hidden forever list** | - | shown | shown |
| **Gardener: Retarget on a proposal** | - | shown | shown |
| **Gardener: Hide forever on a proposal** | - | shown | shown |
| **Search: the Updated window** | - | shown | shown |
| **Search: the Sort control** | - | shown | shown |
| **Settings › Features: the source chip** | - | shown | shown |
| **Settings › Features: the agent tool names** | - | shown | shown |
| **Settings › Workspaces: each repo route's host** | - | - | shown |
| **Settings › Workspaces: the repo routes with no project** | - | - | shown |
| **Event pages: the decoded payload fields** | - | - | shown |
| **Event pages: the raw payload** | - | - | shown |

## Three ways to render a page

Every route answers in the shape the caller asked for:

- **HTML** by default - the full page, layout and all.
- **JSON** when the caller sets `?format=json` or an `Accept` header that wants
  JSON and not HTML. This is how `seam` reads the console's data.
- **An HTML fragment** for entity details when the caller passes `?peek=1` - the
  [reading panel](#read-in-place) (or the Diary's or Interactions' pane) loads
  it without a page navigation - or `?reader=1`, the richer
  reader fragment the screens that keep a rail and a reader (Gardener, Labs,
  Trials) swap in place. A memory, a note, a task and a plan answer `?reader=1` too.

The event page composes its full page from the same `detail-body` block its
peek fragment renders, so the two cannot drift. Session and project
deliberately do not: their fragments are compact summaries of much richer
bespoke pages. Lab and trial detail URLs, and a gardener proposal's
(`/console/gardener/{id}`), render their screen with that entity open in the
reader; a memory, a note, a task and a plan open on
[their own page](#item-page).

Strictly-validated query params (`?sort`, `?scope`, `?tab`, `?w`) return a 400
naming the bad param and listing the valid values, rather than silently falling
back to a default - so an agent driving the console by URL sees the fix.

`GET /console/events` is the SSE stream: every recorded event as one JSON `data:`
frame, with a ping every 25 seconds. A `retrieval.injected` frame also lists the
memories it surfaced as `itemIds` (capped at 48), which is what lets a page point
at exactly what reached an agent. `?feed=interactions` opts into the richer
transport-level rows the Interactions screen consumes. A console tab holds its
stream only while you can see it: 10 seconds after it goes out of sight (a
background tab or a minimized window) it lets the stream go, because a browser
opens only a few connections to a console served over plain http (six in
Chrome) and each open stream keeps one. Shown again, it opens the stream again
and refreshes once to catch up (the Interactions feed fills in the rows it
missed), and a live refresh with no answer after 15 seconds gives up and leaves
the tap-to-refresh pill. Under `peer_auth` the stream leaves out every event
about a locked project this browser has not unlocked, with one exception: an
agent's access request for one arrives as its kind, id and time alone, so the
open page refreshes its Access requests count.

A live refresh changes the page without a word; only a refresh you ask for
with the pill says **Updated**. Three events also raise a toast that links to
the screen that acts on them: an agent asking for access (**Access
requests**), a write held for review (**Held**) and a reported mishap
(**Mishaps**).

## Overview

`/console/`

The check-in page. It answers what waits on you first, then what is in motion,
then whether stored knowledge is reaching your agents. Top to bottom:

- **The level chooser** - on a fresh installation, the one-time welcome card
  (see [Choose how much you see](#choose-how-much-you-see)).
- **Needs you** - one card per thing that waits on you, most urgent first.
  First what an agent is blocked on right now: access requests waiting for
  you (while a project is locked or access requests are on), writes held for
  your review (with write screening on, or restricted accounts' proposals),
  lock declarations waiting for you (with repo locks on), open gates waiting
  on you (then gates waiting on someone else, named), captured plans
  presented and awaiting your approval, claims that lapsed, and tasks marked in
  progress with no claim. Then your own queues: mishaps to review, an update
  that failed, rolled back or paused itself, and backups that are failing,
  stopped, missing, or older than a week. Then the knowledge queue: gardener
  proposals, what automatic apply applied, memories going stale, and, with
  momentum on, plans at the finish line. Each card is one sentence with its
  count and a link to the screen that answers it; a card for a screen your
  level does not show names it without a link. Where one step settles a
  card, that step is on it - **Mark as handled** for one gate, **Approve**
  for one plan or one lock declaration, **Release claim** for one lapsed or
  claimless claim - posting to the same route the task, plan or lock page
  posts to, and coming back here: the strip redraws in place, without a
  reload. Access requests and held writes are decided on their own pages
  (an approval is a passkey ceremony; a held write is read first). A card
  whose optional feature is off does not exist.
- **Agent-reported mishaps** - the mishap card counts every report since you
  last marked them reviewed on the [Mishaps](#mishaps) screen (every report
  before your first review), at the true count, names the latest report's
  type, age and project, and opens the Mishaps screen. **Mark reviewed** there
  clears it until a new report arrives.
- **Going stale** - the knowledge card counts the active memories with no
  update, injection, or read in `gardener.staleness_days` (90 days by default,
  the gardener's own staleness horizon), exactly the rows
  [Retrieval](#retrieval)'s quiet-knowledge list shows, states the horizon,
  and opens that list. Changing the setting moves the card, the list and their
  labels together.
- **Work in motion** - the model [Now](#now) draws, as one row: the plans a
  live claim holds a step of, with the agents holding them; the loose ends
  (in progress, but no live agent carries them); and what an agent could claim
  next, three of each with their true counts.
- **Live now** - up to six live agents, each led by the task it holds with a
  live claim, else by its project, then how many more, and **All N live** to
  Now.
- **Vitals** (Standard and up) - see below - closed by the health line.
- **Recently active projects** (Standard and up) - the top projects by recent
  activity, drawn from the same batched query the Projects board uses, so a
  row here reconciles with a row there exactly, beside the most reused
  knowledge.
- **Knowledge sky** - a star chart of every active memory, with no cap,
  below the projects. **Fold** puts it away; this browser remembers, and a
  folded sky stays folded on every visit. Each
  project (and the global scope) owns a wedge sized by its share of memories,
  largest first. Distance from the centre is when the memory last surfaced to an
  agent: the core holds the last 24 hours, rings mark 7 days and the staleness
  horizon, `gardener.staleness_days` (90 days by default; the same fresh and
  stale lines the memory reader's "since surfaced" chip uses), and
  the rim belt holds memories that have never surfaced. Colour is kind; size is
  demand (the query-gated utility score, so a big star is one agents pull, not
  one the briefing merely shows). Hollow stars have never surfaced, a ring marks
  a starred memory, spikes mark one written today, and today's stars twinkle as
  the chart appears.
  Beside the chart, a readout states the same picture as text: each ring's
  count, the going-stale count (memories with no update, injection, or read
  within the staleness horizon: always the stale card's count and the rows
  Retrieval's quiet-knowledge list shows), and, under **Projects**, every
  project with its ring mix as a bar. Point at a star for its name and description
  (the pointer snaps to the nearest star); click it for a card with its facts and
  a link to open it; double-click to open it directly. Type in **Find a memory**
  to light only the matches (name, description, tag, project, or kind) and list
  them, with the arrow keys and Enter to step through; click a ring, the going-
  stale line, a project, or a kind chip to filter the same way, and point at one
  to preview it. When an injection or a read lands on the live stream, each
  memory it names sends a beam into the core, and the refresh that follows
  glides the star to its new ring; a newly written memory appears with a flare.
  A star keeps its place while its project and ring are unchanged, and the rings
  and wedges resize in steps rather than with every memory, so a refresh moves
  only what changed, and search, filters, and the selection survive it. The
  refresh downloads only what changed, too: the stars that changed, arrived, or
  left since the chart the page holds, and nothing of the sky when nothing in
  it did. It is HTML only - the JSON answer carries no sky.
- **Recent activity** - the latest events, each linking to its detail page.

At Basic the health line comes right after the cards: which agent clients are
working here, whether semantic recall is on, when the last briefing went out,
a warning while there is no backup or the newest is older than a week, and the
version.

The four judged vitals (memory reach, knowledge continuity, context
injections, sessions reached) are drill-down links carrying the selected
window: reach, injections, and sessions reached land on the Retrieval screen
whose hero and delivery funnel are the same numbers over the same report, and
continuity lands on the Sessions list filtered to `?retained=no` - the
sessions that kept nothing. An empty-state card stays linked, because the
destination explains why there is nothing. Below Advanced, where Retrieval is
not in the sidebar, the three Retrieval vitals are plain cards.

**No link above your level.** A link whose screen your level's sidebar does
not show - Locks and Now at Basic, Retrieval and Interactions
below Advanced - is not offered as a link anywhere on this page: the card,
the row or the count says the same thing as plain text. The screen still opens
by URL, under the level note.

Live sessions are counted TTL-aware (active *and* heartbeated within the idle
threshold, or holding a live claim), so the headline matches the Sessions
screen rather than the raw `active` count that an idle session inflates until
the reaper runs.

`?format=json` answers the page's model at every level: `Attention` is the
needs-you list, each item with its `Kind` (a stable key: `access`, `held`,
`lock-declarations`, `gates`, `gates-others`, `plan-approval`,
`claims-lapsed`, `claims-unclaimed`, `mishaps`, `updates`, `backups`,
`proposals`, `auto-applied`, `stale`, `finish-line`), `Count`, `Title`,
`Sub`, `Href`, `Blocking` when an agent is blocked on it, and its `Action`
(the route it posts and its fields) when one step settles it; `Motion` is
Work in motion; `Live`, `LiveTotal` and `LiveMore` are the live strip.

### Since you were last here

When you come back to the Overview after at least five minutes away, a line
under the cards answers the check-in question -- what changed while I was
gone? -- with counts: memories written, sessions started, tasks closed, notes
written, gardener proposals raised, and mishaps reported since you last
looked ("Quiet since you were last here 3h ago" when nothing was recorded).
Each count links its screen - the mishaps count the [Mishaps](#mishaps) screen
- where your level shows it.

"Last looked" is a per-browser stamp, set whenever a console tab is hidden or
left; the counts come from `GET /console/since?t=<unix ms>`, which answers JSON
only and refuses a missing, malformed, future, or older-than-90-days `t` with a
400 rather than substituting a window.

## Now

`/console/now`

The exploded live view: what every agent is doing right now, across every
project and plan at once. Where the project workspace is task-centric, Now is
agent-centric - the unit is the live session, and everything it holds rides its
card. The sidebar entry's badge is the live agent count, and the page refreshes
on **every** event kind (tool calls included - here they are the signal, not
noise), morphing in place like every other screen.

Top to bottom:

- **The titlebar** - live agent count and the fleet-wide pulse: an events-per-
  five-minutes sparkline over the last hour, all kinds, deliberately unfiltered
  by project.
- **A project strip** - **all**, then one chip per project with a live agent
  (`?scope=<slug>`; the global scope filters as `global`). Filtering narrows
  every zone except the pulse.
- **On duty** - one card per live session, headed by the title of the task it
  holds, else by its first prompt as the Sessions screen titles its rows, else
  by its session name, with the session name,
  harness+model pill and account under it; then its project, when it was last
  seen (cards are toned hot/warm/quiet by heartbeat age), how long it has been
  running, cumulative tokens, a star toggle, every claim it holds with a live
  lease countdown, and a short trail of what it just produced. A claim it let
  lapse reads on its card as it reads in Loose ends: "claim lapsed 12m ago:"
  and the task's title. A live agent holding nothing says so: "holds no
  claim".
- **Loose ends** - `in_progress` tasks no live agent is carrying: a lapsed
  lease, a claimless start (`tasks_update status=in_progress` without a
  claim), or a holder that went quiet mid-lease. Each row says its state once,
  in the words every task screen uses: "claim lapsed 12m ago", "unclaimed", or
  "in progress" with the quiet holder named and its lease counting down. A
  lapsed claim offers **release claim** with no confirmation - the lease is
  already dead, so there is no live holder to interrupt, only a stale claim to
  clear.
- **Plans in motion** - a horizontally scrolling rail of every incomplete plan
  across every project, done/in-flight progress bars and ready counts, cards
  dimmed once a plan has rested for 24h. Each links to its plan's page, or,
  for a plan with no narrative note in its project, to that project's Plans &
  tasks tab (the Plans board for the global scope).
- **Up next** - the cross-project ready queue: claimable this instant, plan
  steps included, each naming what closing it would unblock.
- **The wire** - the freshest business events, filtered by the project strip.

Everything links onward - sessions, tasks, plans, projects, events - and the
lease countdowns tick client-side between refreshes. A title wraps to two
lines rather than cutting at one, and one that two lines still cut keeps its
whole text on hover. With the
[gamification](#optional-features) feature on, the page also carries the day
tape, the personal-records rail, the hot-streak pulse, and celebration moments;
off (the default), none of that renders.

## Interactions

`/console/interactions`

The clean live feed of what agents are actually doing: MCP tool calls, hook
injections, recall-miss prompts, session lifecycle, and the plan-mode capture
stream. A fresh page starts empty and listens from that moment forward; merely
visiting never restores old rows.

History is explicit and additive. Choose a recent window and select **Add** when
earlier context is useful; live rows stay in place, and **Load older events**
paginates only inside that chosen window. Loaded history sits under a **Before
you arrived** divider, with what arrived live above it. The empty feed offers
the window you loaded last time in this browser as its first button, and the
other windows beside it; that choice is kept in this browser only and never
loads by itself. Filters operate over the rows already in memory, by event
category and session lane. Pausing buffers new arrivals rather than discarding
them.

Each compact row expands just enough to expose its request/result or injected
text. Selecting the inspector keeps that context beside the stream and links to
the full event page. A recall via the MCP tool records both a
`retrieval.injected` and a `tool.call`; the injected twin is dropped here because
the tool call carries the same content plus its arguments. Session lifecycle
twins are kept on purpose, as feed markers.

## Search

`/console/search`

One query across every entity the console can link to. Memories and notes come
through the same fused FTS + semantic retrieval that [recall](https://thereisnospoon.org/docs/concepts/recall/)
uses, with snippets; tasks, plans, trials, projects, and sessions have no FTS
mirror and match by `LIKE`.

Counts are honest. Each kind shows up to 20 results and is fetched one
further, so a kind with more says so: its count reads **20+** and its **Show
more** opens up to 200 of that kind (`?more=tasks`, comma-separated for
several). Show more is part of the address, so it survives the live refresh
and the Type, Updated, Starred, Archived, and Sort controls; a new query
starts with it closed. A kind still cut at 200 says how many it shows and asks
for a narrower search. The header's total carries the same **+** when any kind
was cut. Memories and notes are counted apart, so many matching memories never
squeeze the notes out.

Each kind wears one color wherever Search names it: its count in the results
header ("1 Session", "2 Sessions") is the legend for its rows, and the Type filter, the palette's group
heads and an empty page's type chip carry the same color as a dot. Archived
and superseded memories wear a quieter take of the memory color.

**Starred only** (`?fav=1`) runs inside each query, before the cut, so a starred
match ranked past the first 20 still appears. Without it, a kind's starred
matches take its first slots, then the best of the rest.

Superseded and archived memories, which every other search leaves out, have
their own lookup here. An exact memory name or ID always finds one, under
**Archived & superseded**, badged **superseded** (with a link to the memory
that replaced it) or **archived**. **Include archived** (`?archived=1`, offered
while memories are in scope) finds them by text as well. A locked project's
retired memories stay out, like the rest of its content.

A one-character query says to type at least two characters. A query that finds
nothing lists each restriction in force - the type, the Updated window,
Starred only - as a chip that removes it, with **Clear all filters** when there
are two or more; when archived or superseded memories match where nothing live
does, it says how many and offers **Include archived**.

Stable references get their own lookup lane. An exact memory name or note slug
ranks ahead of token-overlap matches and is shown beside the result's display
title. A full memory, note, task, session, or trial ULID works
case-insensitively; an 8-character-or-longer ULID prefix finds every match of
those kinds without
letting a short prefix such as `01` flood the page. Identifier matches keep the
canonical ID-based detail link and are labeled separately from keyword and
semantic matches.

The command palette (⌘K, available on every page) fetches this same route with
`?format=json&fast=1`, which drops the semantic leg - a query per keystroke must
never cost a remote embedding round-trip.

The semantic leg is nearest-neighbor: there is always a "nearest" memory,
however far, so a semantic-only hit must clear `search.semantic_floor` (cosine
similarity, default 0.3) to appear - without the floor any query, including
nonsense, would fill the page to its limit. A hit the keyword leg also matched
is exempt. Every hit the semantic leg found shows its similarity as a
percentage, so you can see where relevance falls off; keyword-only hits show a
highlighted snippet instead. Agent-facing recall applies no floor - an agent
can judge a weak hit for itself.

Coverage is deliberately partial in one place: events are excluded because the
telemetry stream has its own Interactions surface and would flood results.

## Diary {#diary}

`/console/diary`, `/console/diary?day=YYYY-MM-DD`

Only while the **Diary** [optional feature](#optional-features) is on. Then it
shows at every [level](#choose-how-much-you-see), as the first row of the
sidebar's Work group, and `g` then `d` opens it. It is a page for each day of
work: what was worked on, which agents did it, and what it took in agent time
and model tokens, with the meaningful work in the spotlight and short
question-and-answer sessions folded away. A day runs from midnight to midnight
in the daemon's time zone, so a daylight-saving day has 23 or 25 hours. The
page is composed from what Seamless already records (the
[usage ledger](https://thereisnospoon.org/docs/reference/storage/#what-lives-only-in-sqlite), the event log,
the sessions, tasks, memories, notes, trials and commit records), by stated
rules and no model.

`/console/diary` is today; `?day=` is another day, and `&m=YYYY-MM` shows
another month in the rail beside it. A `day` or `m` that does not parse is a
400 that names the format.

### What a day shows {#diary-day}

**The date block.** The weekday, the date, and the day's character as a stamp
whose tooltip states its rule. The character is the first of these the day
meets:

| Character | Rule |
|---|---|
| Quiet | No session, task or item recorded |
| Shipped | A plan shipped: its last open step closed as done |
| Closing day | 5 or more tasks closed as done |
| Planning day | A plan drafted (captured, presented or approved) or 6 or more tasks created, and fewer than 2 closed |
| Deep work | 2h or more of agent time, 60% or more of it in one project |
| Busy day | 3h or more of agent time, 12 or more sessions beyond the short ones, or work in 4 or more projects |
| Light day | Work recorded, under every threshold of a fuller day |

Under it, the headline: "Mostly *X*: ..." when one thing took at least half
the day's agent time (or was the only thing worked on), "Led by *X*: ..."
otherwise, saying what it did ("1 step closed and the plan shipped (5 of 5)").
Its second sentence counts the day's sessions, where they were, and how many
things they worked on. Then **Sessions started this day (N)**, and under
`peer_auth` a line naming the locked projects this browser has not unlocked,
whose work the page leaves out.

**Four tiles.** Agent time, model tokens, tasks closed (closed as done and
still done at the day's end) and kept (the distinct memories, notes and trials
written, new or updated). Each states the usual day and draws a trend line over
the last active days, then this one; past days also carry the change against
the usual day, in percent and never judged good or bad. The usual day is the
median of the active days before this one (a day with a session, a task closed
or an item kept): up to the 14 most recent within 90 days, and no comparison
at all with fewer than 3. Today's tiles show no change, since part of a day
against a whole one would read as a fall every morning, and its agent time and
tokens say "so far". Each tile jumps to the part of the entry that lists what
it counts.

**Day rhythm.** One column per hour, agent time stacked by agent, the busiest
hour's figure on its column. A column's full height stands for at least an
hour of agent time, so a light day looks light.

**What was worked on.** The day is written in threads. A **plan** thread is
one plan in one project: the steps that moved that day and the sessions that
moved them or captured, presented or approved the plan. A **project** thread is
one project's tasks outside any plan that moved, and its sessions whose work
named no plan or task; sessions in no project make a thread named after their
working directory's last folder. The spotlight's threads are cards: the plan's
progress before and after the day ("12 → 14 of 14 steps closed"), each task
that moved with its status before and after, each session with its agent, what
it was about, its agent time and tokens, then the hours it was worked in, the
thread's figures, what its sessions kept and the commits it recorded, and a
**Why this is here** disclosure listing the facts that scored. The other
threads are one line each under **Also that day**, and the short sessions one
collapsed row, **N short sessions**.

What a session was about is the first of these that exists: the first line of
the findings its agent wrote, its first prompt of the day, the first line of
the findings the hooks harvested from its transcript, or "Session in
*project*". The sentence links to the session.

**The usage panel.** **Agents**: a ring of model tokens by agent (one client
and one model, named as its pill reads), the day's total in the center and a
legend row per agent with its share and figure; a day with one agent draws its
legend line and no ring. **Token mix**: output, fresh input, cache write and
cache read, as one bar. **Projects**: bars by agent time, or by model tokens on
a day with none. Then **Tasks closed**, **Kept** (a superseded or archived
memory is marked), and **Moments**: plans shipped, commits recorded (with what
recorded them), flags set, and mishaps the agents reported. **Show as table**
lists every figure the charts draw.

**The rail.** The month as a heat grid shaded by agent time, then its days,
newest first: each with a bar of its agent time against the month's busiest
day, the figure, a dot for each of its top projects, and its sessions and
character in the tooltip. A run of quiet days is one thin row, and a day from
before the usage ledger shows its agent time as "—", not 0m.

### The spotlight {#diary-spotlight}

A thread scores its outcomes and its effort, each fact capped so that no single
one carries a thread alone:

| Fact | Points | At most |
|---|---|---|
| The plan shipped: its last open step closed as done | 10 | 10 |
| Each task or step closed as done | 3 | 12 |
| Each task or step created or claimed | 1 | 3 |
| Each memory, note or trial its sessions wrote | 1 | 4 |
| Each commit its tasks or sessions recorded | 1 | 4 |
| Each session that ended with findings its agent wrote | 1 | 2 |
| Agent time | 1 for every 10 minutes | 12 |
| Output tokens | 1 for every 80,000 | 6 |

A thread needs 4 points to be a card, and at most six are, most points first.
A thread holding a [flagged session](https://thereisnospoon.org/docs/guides/data/#flagged-transcripts) is
always a card, whatever it scored, and counts toward the six. **Why this is here** lists each fact with the points it
scored.

A session's part of a day is **short** when it has no outcome, under 10
minutes of agent time and under 30,000 output tokens, and is not flagged. An
outcome is something the record keeps: a task moved, a plan captured,
presented or approved, a memory, note or trial written, a mishap reported, a
commit recorded, or an end that day with findings its agent wrote (a summary
the hooks harvested does not count). A short session is never a card or part
of a thread: it is one line in the folded row, and the character's session and
project counts leave it out.

### Agent time and model tokens {#diary-usage}

**Agent time** is a count of minutes in which an agent sent at least one
model message, counted for each session, model and thread (the agent's own
turns, and the subagents it launched apart), and added up. Agents working in
parallel, and a session beside its subagents, make more agent time than the
clock passed.

**Model tokens** are the four kinds the token mix shows, added: output, fresh
input, cache writes and cache reads. They include cache reads, so they are not
a billing figure, and the Diary shows no cost.

A day's totals come from the usage ledger with each session counted once;
they are never the sum of the cards. Three words mark where a figure is less
than whole:

- **Unreported** - a session with no token record at all. It is named in a
  note under the tiles, counted as "(N unreported)" on the tokens tile and on
  its thread's card, and never counted as zero. A Claude Code session reports at its end,
  and while the Diary is on its prompts keep it current
  ([mid-session usage](https://thereisnospoon.org/docs/reference/hooks/#mid-session-usage)); a Codex session
  reports at each turn's `Stop`.
- **Approximate** - a session from before the usage ledger, which has token
  totals and no ledger rows. Its whole total counts on the day it ended and on
  no other ("counted on Nov 12"), with no hour, thread or agent time: the
  tokens tile says approximate, a note under the tiles says why, and agent
  time reads "not recorded before the usage ledger". `seam usage backfill`
  replaces it with real rows while the session's Claude Code transcript is
  still on its computer ([the command](https://thereisnospoon.org/docs/reference/cli-seam/#seam_usage_backfill)).
- **Shared** - a session that worked on more than one thing (two plans, or a
  plan and loose tasks) is in each thread, and counts in full in each: a
  card's figures say how many of its sessions are shared, and the cards never
  add up to the day.

### Links {#diary-links}

Every link is built from an id resolved, as the page renders, through what
this browser may read. An item that no longer exists is plain text titled "No
longer exists": the Diary never offers a link that answers 404. A link into
an optional feature exists only while that feature is on. A link to an entity
opens it beside the day as a peek; a modified click opens the full page.

| On the Diary | Opens |
|---|---|
| A day in the rail, a cell of the month grid, the previous or next day, Today | That day, in place. A day still to come is never a link; the month arrows keep the day and change the month the rail shows. |
| A task | The task's page. |
| A session's line, its flag badge | The session's page, where its flag and any kept transcript are. |
| A memory | The memory's page; a superseded or archived memory keeps its page, and the Kept list marks it. |
| A note | The note's page. |
| A trial | The trial's page, only while Research is on. With Research off, trials are not on the page at all. |
| A mishap | Its event's page. |
| A plan: a card's title, a shipped moment | The plan's page when it has one in that project; otherwise that project's Plans & tasks tab, as a page; a plan in the global scope with no page is text. |
| A project chip, a project bar | The project's page; the global scope and a working directory are text. |
| A commit | The page of the task or session that recorded it (a commit has no page of its own), with a button to copy its full id. |
| Sessions started this day (N) | The Sessions screen's list for that day, which lists the sessions created on it: hence "started", and N is that list's count. The day's own session count, in the headline, also holds sessions started earlier that worked on it. |
| A tile | The part of this entry that lists what it counts. |

The agents legend, the rhythm's columns and the token mix are not links: no
page lists "this agent's sessions that day", so they answer on hover only. The
other way in is the [Sessions](#sessions) screen: a day view there carries
**Open in Diary** while the Diary is on.

### Moving around {#diary-keys}

| Keys | Does |
|---|---|
| `←` / `→` | The previous / next day (today has no next) |
| `t` | Today |
| Arrows, `Home`, `End`, `Esc` in a chart | Walk its marks, jump to the first or last, rest |
| Arrows, `Enter` in the month grid | Move a day or a week, open the day |

The day keys stand down while you type, while a dialog is open, inside a chart
or a peek, and after `g`, which belongs to the section shortcuts.

**How the charts answer.** Every mark answers on hover or a tap: a ring
segment, an hour of the day rhythm, a project bar, a part of the token mix, a
day of the month, a stretch of a card's presence strip. The readout gives its
figures and the rest of the chart goes quiet. From the keyboard each chart is
one tab stop. Days and project names are real links, so a modified click opens
a new tab and the grid works without scripts. Colors name agents only: an
agent keeps one color for the whole month, and agents past the month's five
busiest share a neutral Other, whose readout names them. A card's presence
strip lights whole hours, so its readout gives a stretch of hours, not agent
minutes.

**Peeks.** A peek opens beside the day on a window 1380px wide or more, and
under it below that, and the item you clicked stays where it was. Back closes
it, and a `#peek=` link opens the day with it open. Changing the day happens in
place and keeps the browser's history.

**Today is live.** It refreshes in place from the console's live stream, at
most every 15 seconds, and an open peek stays open. A past day does not
refresh.

**On a phone** the entry comes first and the month after it, and the tiles go
two by two.

**With Research off**, trials appear nowhere: the day's kept counts, a
thread's kept list and its points, the headline and the character are
composed without them. The rail and the usual day still count trials among the
items kept (counts, never an item).

### Empty days {#diary-empty}

- **A quiet day**: "A quiet day.", with no tiles or charts: no session, task
  or item was recorded.
- **The first run**: a day with nothing recorded on it, in its month or in the
  90 days before it says "This is the Diary's first page." and draws no
  stamp.
- **A day still to come**: "This day has not begun." and "Still to come.",
  with **Back to today**.
- **A day of only short sessions**: "Only short sessions: they are folded
  below."
- **A day from before the usage ledger**: totals marked approximate, and agent
  time not recorded.

### The JSON answer {#diary-json}

`?format=json` answers with the day model, the same at every level: the day
and its bounds (`day`, `start`, `end`, `hours`, `today`), `headline`,
`character` (`word` and `rule`), `facts` (the tiles' and the character's
counts), `usage` (by agent, project and hour, with the token mix,
`unreported` and `approximate`), `spotlight` and `also` (the threads, each with
its `score` and `reasons`), `short`, `kept`, `moments`, `sessionsStarted` and
`projects`; then `month` (the rail's month, its days and its agents) and
`usual` (the usual day). Every item the day names carries its `kind`, `id`,
`title`, `resolved` and `href`, the link the page shows, empty when the item no
longer resolves or its feature is off. A day still to come answers
`"future": true` with every list empty.

## Projects

`/console/projects`

The board leads with work. Its summary tiles count the agents live, the open
tasks (in progress, ready, blocked, waiting on people, and any lapsed or
unclaimed claims), the plans in motion (a plan with a step still open that
moved in the last 7 days, out of every plan with a step open, the rest
stalled), then the knowledge base and its reach, judged against its 55% target
over the **Reach window** the header picks (`?w=`).

Each project is one compact row: its name, with its star and badges
(isolation, a lock, live agents, and what needs attention), a work line in the
[task words](#tasks) - "2 plans moving · 1 plan stalled · 1 in progress · 3
ready · 1 blocked · 2 waiting on people" - then its open tasks, its last activity (the later of its
last session update and its last task change; hover it for the moment in your
own time) and its reach as a small figure. The whole row opens the project.
Blocked and waiting on people count exactly what the Tasks page's JSON rails
and `counts` count with its plan steps shown (`?steps=1`): blocked waits on unfinished work
alone, and waiting on people is a gate or a step an open gate holds.

**Needs attention** means something waits on you or is stuck: a lapsed claim, a
task marked in progress that no claim holds, an open gate, a captured plan
awaiting approval, or data under a slug the project registry does not list. A
project whose tasks only wait on other work is not flagged, and neither is a
retired one. The header's chip counts them, and choosing it narrows the board
to them (`?show=attention`; `all`, the default, shows every project); narrowed,
the chip's **Show all** clears it.

The board has three views (`?group=activity|family|flat`):

- **Activity**, the default: **Active** (a live agent, or a session or task
  touched in the last 30 days), then **Quiet**, then **Retired**. Quiet and
  Retired are folded. A filter (`?q=`) that matches a project inside one opens
  it, and Quiet stands open when no project is Active.
- **Family**: each shared-briefing family, a parent with its children under
  it, then Standalone, then Retired by split, folded.
- **Flat**: every project in one list.

`?sort=recent|coverage|name|favorites` orders the rows inside each band. In
the Activity view, Recent puts starred projects first, then those with a live
agent, then the most recent.

The global (`""`) scope is not a project, so no view lists it. It is one line
under the summary tiles, "Shared context: N memories, N notes, reach N%",
linking to [Context](#context).

Under `peer_auth`, a locked project this browser has not unlocked is a row in
the same bands, sorted with the others, with a lock badge: its name, its
counts, its last activity (the latest session start or end in it, never a
title) and **Unlock**, which opens `/console/unlock/<project>`. Nothing else
about it is read: it has no work line, and it is never judged to need
attention, so the attention view leaves it out. A task that waits on a locked
project's task counts as blocked, as it does on the Tasks page. The JSON model
also lists these tallies as `locked`.

The board remembers the view and sort you pick, in this browser only (its
local storage). A link that names neither, such as the sidebar's, shows the
remembered pair, drawn in place without a reload; a URL that names a view or
a sort wins. A fresh browser starts on Activity, sorted by Recent.

Selecting a project opens the **project workspace** - a seven-tab page over that
project alone. It opens on Plans & tasks (`?tab=tasks`), and a load reads and
renders only the tab it shows: the header and the tab counts come from the
project's board row. A tab is a `?tab=` link that loads in place, so switching
keeps the page, back and forward walk the tabs, and a live refresh redraws
only the tab on screen. `?tab=` takes `tasks`, `overview`, `sessions`,
`memories`, `notes`, `interactions` and `context`; `?tab=plans` and the
Context tab's old name, `?tab=relations`, redirect to their tab.
`?from=<project>` with no tab opens Memories, and `?isolate=<state>` with no
tab opens Overview. The Interactions and Context tabs are those screens over
one project, so they follow their level: below Advanced they leave the tab
bar, and a `?tab=` link still opens them, under the level note.

| Tab | Shows |
|---|---|
| Plans & tasks | One card per plan. Its head is titled by the plan's narrative note (by its slug when it has none), with `plan:<slug>` beside it to copy, the status in words (draft, awaiting approval, approved), a **narrative** link to the note, and a line that counts the steps by their [status words](#tasks) ("2/6 done · 1 in progress · 1 blocked · 2 waiting on owner"). **stalled 10d** marks a plan with open steps, none in progress, that nothing moved for 7 days, with "ready work, nobody on it" when a step is claimable. A captured plan that is a draft or awaits approval, or is approved with no steps yet, is a card too, with no steps; **Approve**, on a capture that awaits it, lands back on its card. Then each plan's step timeline: each step's word, claiming session, lease countdown, and blocking dependency. A step a gate holds is *waiting*: it says who must act and what must happen ("waits on owner: approve the window") and is never claimable, and a gate's row is marked **Gate** and says what it holds ("holds 2 steps"). The completed plans fold below the cards; past the first 20, **N older** opens Plans for the project. Then **Off-plan tasks**: every task of the project outside a plan, once, grouped *In progress* (holder and lease), *Ready*, *Blocked* (the first blocker named and linked), *Waiting on people* (who must act, and what must happen), *Gates* (what each one holds: the unfinished tasks that depend on it) and, folded, the ten most recently closed, whose **all closed** opens the Tasks screen on the project's closed tasks. Its line ("8 open: 5 in plans, 3 off-plan") counts what the tab lists, the number on the tab's pill. Every row, a step or an off-plan task, expands into the task's dossier in place and copies its task's whole id, and each plan's head copies `plan:<slug>`. A task in progress that no live claim holds carries a strip under its row: **claim lapsed** offers *release claim*, which lands back on the row, **unclaimed** (marked in progress with no claim at all) opens the task under its row. |
| Overview | The project's metrics, memory kinds, injection trend, its Isolation card, and its own recent events. |
| Sessions | The project's sessions counted by state (live, idle, completed, expired), the newest 60 of them, and a link to all of them on the Sessions screen. Each row is titled as the Sessions screen titles it (the task a live session holds, else its first prompt), with its name under it, and a live session names each task it holds by title, linked to the task. |
| Memories | The project's memories with a lineage cell - provenance session, or a supersession pointer - plus the memories it *inherits* from a parent that a strict per-slug count excludes. |
| Notes | The project's notes. |
| Interactions | The project-scoped slice of the feed. |
| Context | The effective SessionStart flow into and out of the project: global and parent memory pools, sibling-family channels, and split lineage. |

**Came from.** With [access requests](https://thereisnospoon.org/docs/concepts/project-isolation/#provenance-stamps)
on, an item written by an agent that held a confidential or sealed project's
content carries a provenance stamp ("may contain content from example-finance
(grant R-7F3K)"). On a project with such items, the Memories, Notes and Plans &
tasks tabs show the stamp on each row and a **Came from** bar: one chip per
project the stamps name, with how many items of that tab's kind name it.
Choosing one (`?from=<project>`) narrows the three tabs to those items, and
Plans & tasks then lists the tasks themselves, any status. A locked project
this browser has not unlocked is named only; the items listed are this
project's own. A project with no stamped item shows no bar.

**Locks.** Under [`peer_auth`](https://thereisnospoon.org/docs/guides/peer-auth/#two-settings-isolation-and-the-lock),
a confidential or sealed project's Isolation card offers **Lock this project**,
which opens `/console/lock/<project>`. Locking it, and removing a lock set here,
each need a passkey already enrolled on the Approvers page; without one the card
links there. Seamless restarts to apply the change. A project listed under
`peer_auth.force_locked` shows as **Force locked** and cannot be changed here. A
locked project's isolation cannot be set to `open`.

**The lock page**, `/console/lock/<project>`, is one card. Its title asks the
change it offers ("Lock example-conf?", or "Remove the lock on example-conf?"
for a lock set here), and a ladder of Open, the project's fence (Confidential,
or Sealed) and Locked marks where the project stands and where the change takes
it. Two rows say what the change does to the project's own agents and to this
console, then one passkey button (in the danger tone to remove a lock), with a
chip saying Seamless restarts to apply it. After the passkey the card shows the
change's steps, approved, restarting and done, and the page then refreshes in
place. An open project's card links to its Isolation card instead, and with no
passkey enrolled it links to the Approvers page. A force lock names
`peer_auth.force_locked`, with a copy button and a link to unlock the project
in this browser instead. A change saved but not applied yet says it takes effect
when Seamless next restarts. The JSON model adds `isolation`, `rung` (`open`,
`confidential`, `sealed` or `locked`) and `rung_to`.

**The unlock page**, `/console/unlock/<project>`, is the same card for a locked
project: a lock, the project's name, a **Locked** chip beside the lock's kind
(**config file**, **from the console** with a link to remove it, or **lock
removed** while a removed lock waits for the next restart), and its counts as
tiles. With a passkey enrolled, its one primary action is **Unlock for
3 hours** (the console's lease, shorter when `peer_auth.max_lease` is), and
**Approve from a terminal** waits under "no passkey here?". With no passkey
enrolled, passkeys off, or this browser on another address than the passkey's,
**Approve from a terminal** is the primary action, with a line saying why. It
turns the card into numbered steps: this browser's code, large, with its time
left, then the command with a copy button, and **Cancel**. Once the command
approves the code, the card turns unlocked in place, without a reload. Unlocked
here, the lock opens, the chip reads **Unlocked here**, the time left shows as a
bar and in words, and **Open** leads (the project, or the plan, session or
conversation a locked page sent you from), with **Lock it again now** under it.
A caller that is not a browser is told an unlock is per browser. The JSON model
adds `lock_kind`: `file`, `console`, or empty for a removed lock the next
restart applies.

A retired project still renders, with its banner - kept for provenance. Only an
unknown slug is a 404. An error page under `/console/projects/<slug>` (a
mistyped tab, say) offers **Back to <project>** when that project exists.

## Sessions

`/console/sessions`, `/console/sessions/{id}`, `/console/sessions/{id}/transcript`

The list separates **active** (live: active and heartbeated within the idle TTL)
from **idle** (active but gone quiet past it, awaiting the reaper) from
**completed** and **expired**. Filterable by status, searchable, windowed, and
filterable by retention (`?retained=yes|no`): whether the session left a
durable artifact behind - non-empty findings, or a written memory, note, or
recorded trial, the same covered-ness test the coverage numbers apply. The
Overview's continuity vital links straight to `?retained=no`, so its click
answers "which sessions dropped knowledge".

A row is titled by what the session is doing: the task it holds with a live
claim (and how many more it holds), else its first prompt, chosen and
shortened as the [Diary](#diary)'s about line is. Findings its agent wrote come
first, so a session that has them keeps its name as the title and shows them.
A titled row's second line is the session's generated name (`cc/...`).

`?project=<slug>` lists one project's sessions (`global` lists the global
scope's), over all time unless a window is chosen, and every filter and sort
link keeps it. A value that names no project this console reads is refused
like any other bad value; a locked project's refusal says it is locked, and
its sessions stay out.

The list defaults to the last 24 hours (`?w=24h|7d|30d|all`; any other value is
refused). When that leaves most sessions out, the list ends by saying so ("3 of 212 sessions were active in the last 24h") and
offers the wider windows in place, so a quiet day never reads as an empty
system. The list shows the newest 200 sessions its filters keep, and past
that says so ("Newest 200 of 559"). The search and every filter apply before
that cap, so a session older than the newest 200 is still found; each word
searched for must appear in the session's name, project, findings or id.
A row names its host only when the session ran on a different machine
from the console's, and its source only when it was not a normal startup.

A day view (`?day=YYYY-MM-DD`, what a click on the momentum calendar opens)
lists the sessions started on that day, whatever the window. While the
[Diary](#diary) is on, its heading carries **Open in Diary**, that day's page.

A session's page is the workspace: its findings (rendered), its event
timeline as interaction rows, per-session counts over the events the timeline
holds (tool calls, memory reads and writes, items injected, and
read-after-inject), the tasks it currently claims with their lease countdowns,
and what it did: **Tasks moved** (each task it created, claimed, released,
closed or reopened, with those moves in order), the notes it wrote, and the
memories it produced. Only items this console still reads are listed, each
linking to its page. With none, the card says what fits the session: an
active one has nothing durable yet, a finished one moved no task and wrote
nothing. The timeline holds 500 events; a longer session's says "The first
500 of N events" and links to the latest 500 (`?timeline=latest`, which links
back; any other value is refused). The page's title, the browser tab's, is
the session's name and project.

A [flagged session](https://thereisnospoon.org/docs/guides/data/#flagged-transcripts) carries a badge with its
verdict (good, bad or notable) and its reason on the list, the peek and its
page. Its page adds a card: when it was flagged, the transcript file the agent
client wrote, and what became of the copy, with **Download JSONL** while a copy
is kept (`/console/sessions/{id}/transcript`, the owner's alone: never a
restricted account, a relay's caller or a paired device). Once any session is
flagged, the list offers a **Flag** filter (`?verdict=flagged|good|bad|notable`).
With no window chosen it lists the flagged sessions of all time, as `seam
sessions --flagged` does: a flag changes no session's last update, so the
24-hour default would hide a session flagged after its day. A window chosen
under **Updated** applies to the filter as to every other, and stays chosen
while the other filters change; until then the page's links carry none.

### The conversation {#conversation}

With [access requests](https://thereisnospoon.org/docs/concepts/project-isolation/#asking-for-access) on, a
conversation can span sessions: a move into a fenced project continues it as a
child session there. Every session of a conversation that moved, asked for or
holds a grant, or wrote a stamped item shows one **Conversation** card (also in
the page's JSON as `conversation`):

- **Home chain**: the session it started in and each move's child, oldest
  first, with the move's request, who decided it and when.
- **Reach**: every project it reached - its homes, projects `session_start`
  bound it to, read-only and read-and-write grants, leases, and requests still
  waiting, denied or expired - each with its request (R-...) and who decided
  it: a passkey or Touch ID key by name, the terminal (`seamlessd approve`), the
  bearer key (`seam approve` or the console, advisory), the agent's approval
  dialog (advisory), or a Deny on this console.
- **Written**: what its sessions wrote, by project, each with its provenance
  stamp.

A locked project this browser has not unlocked shows its name, the request that
reached it and an **Unlock** link back to the page, and nothing from inside. A
conversation that never moved, asked for nothing and wrote nothing stamped shows
no card.

## Mishaps {#mishaps}

`/console/mishaps`

Every mishap agents reported at `session_end`, newest first, for a recurrence
review: which kinds keep happening, and which memories fail to prevent them. Its
sidebar row is under Live, at every level, with a count of the reports since you
last marked them reviewed (nothing at zero), and `g` then `f` opens it; the
Overview's mishap card and the check-in's mishap count open it too.

- **Type chips** - All, then each type with its count - `shell-trap`,
  `wrong-target`, `live-state`, `ignored-warning`, `fabricated`, `other` - and
  **untyped**: reports that named no type, among them every report written
  before types existed. The counts cover the project and window in view
  whatever type is selected; point at a chip for what its type means.
- **Your review** - how many reports arrived since you last marked them
  reviewed, whatever the filters, with **Mark reviewed**, and a **new** mark
  on each of those reports. Marking them reviewed clears the Overview's
  mishap card until another report arrives; it marks through the newest
  report the page drew, so one that arrives while you read stays new.
- **Reports** - each with its type (or `untyped`), its text with the type
  prefix taken off, its project (click it to keep only that scope's reports),
  the session that reported it with its agent, and the memories it named.
  Click a row to read its event in the [reading panel](#read-in-place).
- **Memories named** - every memory the reports in view named, with how many
  named it. A memory many reports name is one that is not preventing its
  mishap.

Filters: `?type=` (a type, or `untyped`), `?project=` (a slug, or `global`),
and the window `?w=24h|7d|30d|all` (all time by default) or `?days=N`. A value
the page cannot read is a 400 naming what it takes. `?format=json` answers
`total`, `types` (each with its `count` and `meaning`), `shown` (the reports the
filters select), `mishaps` (each with `type`, `text`, the recorded
`description`, `project`, `sessionId`, `session` and `memories`), `memories`
(each with its `count`), `review` (`at`, when you last marked the reports
reviewed, and `new`, how many arrived since; absent while no report exists),
each report's `new`, and `locked` while peer_auth leaves locked projects
out. The list holds the newest 200.

## An item's own page {#item-page}

`/console/memories/{id}`, `/console/notes/{id}`, `/console/tasks/{id}`,
`/console/plans/{slug}?project=<slug>`

A link to one memory, note, task or plan opens that item on a page of its own:
the item and what is around it, never the list it lives in. The lists stay on
[Memories](#memories), [Notes](#notes), [Tasks](#tasks) and [Plans](#plans).
Every item reads the same way:

- **The way back**, top left: a link to the collection scoped to the item's
  project ("example-ops's memories"), or to the list the URL names. The browser's
  Back button works as always.
- **Its place**, top right: "3 of 12 gotchas in example-ops", with **previous**
  and **next** (`k` and `j`). When the URL carries a list's state (the
  parameters the list puts on its links: `?sort=`, `?kind=`, `?project=`,
  `?q=` and the rest), the place follows that list's order. Without one it is
  the memory's project and kind, newest first; the note's project, newest
  first (an agent's working copy among them with the working copies shown); a plan step's plan, its closed steps in the order they closed and
  then the rest as the plan card orders them; or an off-plan task's project,
  as the Tasks list orders it. A list that does not hold the item falls back
  to that. A row of the Memories or the Notes list always carries its list's
  state (`?sort=recent` on the default list), so a memory or a note opened
  from it is placed in that list.
- **Where it lives**: project › Memories › kind, project › Notes, project ›
  Plans › `plan:<slug>`, or project › Tasks, each a link.
- **The title row**: a memory's name in mono, or a note's or a task's title,
  with copy and the star, and the actions where the eye starts: **Edit in
  editor** and **Archive** for a memory, **Edit in editor** and **Delete**
  for a note, and for a task the actions its reader offers (**Mark done**,
  **Waits on someone**, **Drop**, **Reopen**, **Release claim**). Every action
  returns to this page with the list's state kept. A deleted note has no page
  left, so Delete returns to the way back.
- **The lead and the facts**: the description, then one line of facts. A
  task's line starts with its whole id and **Copy id**, the form you hand an
  agent.
- **The body** in one reading column, and **a context column** beside it,
  which moves under the body on a narrow screen:
  - a memory: **In use** (how often it surfaced, how long ago, its utility
    score, the sessions it surfaced to), what replaced it and what it
    replaces, up to five more of its kind in its project with a link to all,
    and its file path;
  - a note: **On this page** (its h2 and h3 headings as links, once there are
    two), the plans it is **part of** with their progress, more notes in its
    project (its working copies left out, as on the list), and its file path;
  - a task: its plan as a small card with every step and this one marked,
    what it depends on and what it blocks. Its history and earlier versions
    follow the body;
  - a plan (`/console/plans/{slug}?project=<slug>`, drawn by
    [Plans](#plans)): its narrative note, the notes attached to it and its
    project. A plan has no place in a list, so its page has no previous or
    next.

An archived or superseded memory keeps its banner and **Restore** at the top
of the page, and a note [Held](#held) keeps says so there. A locked project's
item answers as it always did, and its items are absent from every other
item's place. `?format=json`, `?peek=1` and `?reader=1` answer as before.

## Memories

`/console/memories`, `/console/memories/{id}`

One list, full width, with no rail and no reader: a memory's link opens [its
own page](#item-page). With **All projects** in scope the list is every match,
newest first, in local-day groups: **Today**, **Yesterday**, **Earlier this
week** (the seven days up to today), **Earlier this month** and **Older**,
each heading saying how many matches it holds. Days are counted in the
daemon's time zone, as the [Diary](#diary) counts them. The list draws 50 rows
at a time, and **Show 50 more** adds the next rows in place, without reloading
the page; the address keeps how many are shown (`?show=`). The other sorts,
**Name**, **Reach** (how often a memory surfaced), **Utility** and
**Starred**, draw the matches as one group in that order, 50 at a time. With a
project chosen in the switcher, the list is the whole project, grouped by kind
in the canonical order, each kind's heading with the one line that says what
that kind holds, and each kind in the sort's order.

A row is one link: the kind as a colored word, the name in mono, the
description on one line, the project (on the All projects list only), how long
since it changed, and, from the Standard level, "surfaced 43×" or "never
surfaced". A memory written by a restricted local account says so on its row.

The bar holds the project switcher, the text filter (`/` focuses it),
**Starred**, **Never surfaced** (memories that have never reached an agent's
context, from the Standard level), **Sort**, and **N archived and
superseded**. Under it the kind chips filter by kind: **All kinds**, then each
kind with a match. The filters combine, and every chip, toggle and switcher
row counts what choosing it would show, the other filters applied. The text
box matches a substring of the name, the description, the kind, a tag, or the
project's slug or name. While anything is filtered the header reads "3 of 128
active" beside a **Clear** link; when nothing matches, the list says so and
offers Clear and **Search everything** for the same text on [Search](#search).

**N archived and superseded** switches the list to the memories that were
archived or replaced (`?status=inactive`), laid out the same way: each row
says which it is and, when superseded, names the memory that replaced it.
Every filter, the switcher and the sort keep it, and pressed again it brings
back the active memories. Its **Search everything** reaches the archived
memories too.

The project is chosen with the switcher, the **In All projects** button
(`p` opens it). It lists **All projects**; **Recent**, the projects this
browser last chose or visited (up to five, kept in this browser only);
**Starred** projects; then every project that holds a match, most active first
by its latest session or task, as on [Projects](#projects), and **Shared
globally** last. Each row counts what choosing it would show, the other
filters applied. Projects with no activity in 30 days fold into one line
that says how many there are, and typing finds them. Typing filters by slug
or display name in the page, with no request; the arrow keys move, `Enter`
chooses, `Esc` closes, and `Backspace` in an empty field goes back to All
projects. A locked project this browser has not unlocked is listed with a
lock and no count, and links to its unlock page. Without script the switcher
is a plain menu of the same links.

The whole state is in the URL: `?sort=`, `?kind=`, `?project=` (a slug, or
`global`), `?fav=1`, `?surfaced=never`, `?q=`, `?status=inactive` and
`?show=`. A value the screen cannot read is a 400 naming the values it takes.
`?group=project`, from the list before, is still read and kept on links, and
changes nothing: choosing a project replaces it. Each row's link carries the
list's state (`?sort=recent` on the default list), so the memory's page places
it in this list and its `k` / `j` walk it; a new filter, sort or project
starts again at the first 50 rows. Starring, archiving and restoring on the
memory's page return to that page with the list's state kept. `?format=json`
answers every match, unpaged, in project and kind groups with the archived and
superseded ones beside them, whatever the page draws.

What reaches an agent's context is **surfaced** here, the word the Overview
and the knowledge sky use: a row reads "surfaced 43×" or "never surfaced", and
a memory's page leads with how long since it last surfaced, or says it never
has. A kind's tooltip, on a row or a chip, is the same one-line meaning its
group heading shows in a project's list.

A memory's [own page](#item-page) renders the body uncapped (through the
markdown layer, with raw HTML disabled and a sanitizer on the output), the
metadata (kind, project, tags, timestamps, the session that produced it), how
often it surfaced and was read, the sessions it surfaced to, its
[utility score](https://thereisnospoon.org/docs/concepts/recall/#the-utility-nudge) with the per-signal
demand breakdown behind it, the `vscode://` link straight to the file, and its
supersession neighbors in **both** directions: what replaced it, and what it
replaced. Its actions are the **star**, the flag that pins it into briefings
and boosts recall, and **archive**, which asks in the page first and says what
happens: the memory leaves briefings and recall, its history stays readable,
and it can be restored. An archived or superseded memory says so in a banner
at the top of the page, and the banner carries **Restore**, unless a memory
that replaced it is still active, which the banner links instead, or
[Held](#held) holds it, which the banner says.

`j` and `k` move a focus ring through the rows, `Enter` opens the focused
memory's page, `/` focuses the filter and `p` opens the switcher. A memory
written while the page is open joins its group as the page refreshes in place,
without a reload, and the row in focus keeps the ring.

## Notes

`/console/notes`, `/console/notes/{id}`

One list, full width, in the [Memories](#memories) list's shape, with no rail
and no reader: a note's link opens [its own page](#item-page). The list is
every match, newest first, in local-day groups (**Today**, **Yesterday**,
**Earlier this week**, **Earlier this month** and **Older**), each heading
saying how many matches it holds, across every project or, with a project
chosen in the switcher, in that one. It draws 50 rows at a time; **Show 50
more**, or **Show the other N** when fewer are left, adds the next rows in
place, and the address keeps how many are shown (`?show=`). **Name** (A to Z)
and **Starred** draw the matches as one group in that order.

A row is one link: the title, the description on one line, each plan the
note belongs to (its `plan:<slug>` tags) with the plan's status (a captured
plan's own, otherwise in progress, ready or done from its steps), up to two
of its tags and how many more, the project (on the All projects list only),
its length in words and how long since it changed. A note written by a
restricted local account says so on its row.

The bar holds the project switcher (the same one as on Memories, where the
global scope reads **Global inbox**), the text filter (`/` focuses it; a
substring of the title, the description, a tag, or the project's slug or
name), **Starred** and **Sort**. Under it the tag chips filter by tag:
**Everything**, **plan narratives** (the notes tagged `plan:<slug>`), then
the tags written for people that most notes in view carry, up to twelve; any
other tag is found by typing it. The tags software adds - `created-by:*`,
`plan-status:*`, `cc-plan` and `agent-cache` - never become chips. Every
chip, toggle and switcher row counts what choosing it would show, the other
filters applied, and the "N of M" header, Clear and the Search fallback work
as on Memories.

Planning subagents' cached runs (the notes tagged `agent-cache`, agents'
working copies) are folded away by default: they are left out of the list
and its counts, and a line after the chips says "N agent working copies
folded · show them". **show them** (`?caches=1`) lists them among the notes,
each marked as a working copy, and every chip, filter, sort and the switcher
keep that; **fold them** folds them again. A plan's page and its card on
[Plans](#plans) still list them under their plan.

The whole state is in the URL: `?sort=`, `?project=` (a slug, or `global`),
`?fav=1`, `?q=`, `?tag=`, `?plans=1`, `?caches=1` and `?show=`. A value the
screen cannot read is a 400 naming the values it takes. `?group=project`,
from the rail before, is still read and kept on links, and changes nothing.
Each row's link carries the list's state (`?sort=recent` on the default
list), so the note's page places it in this list and its `k` / `j` walk it.
`?format=json` answers every match of the filters, unpaged, in project
groups (global first), the working copies included, whatever the page draws.

A note's [own page](#item-page) renders it as a document: its plans with
their status, its tags (the ones software adds fold behind a **+N** chip),
its length and reading time, its source URL, and the file path with an
editor link. Its actions are the **star** and **Delete**. Delete asks in the
page first and removes the note for good. A note that is a plan's narrative
says so, and also needs the box beside Delete ticked; while [Held](#held)
holds the note, the page says why instead.

`j` and `k` move a focus ring through the rows, `Enter` opens the focused
note's page, `/` focuses the filter and `p` opens the switcher.

## Tasks

`/console/tasks`, `/console/tasks/{id}`

The backlog, as one list, full width, with no rail and no reader. By default
it lists every project's tasks outside a plan, which is what `tasks_ready`
means for an agent; **Plan steps too** adds the plan steps, each with its
`plan:<slug>` chip, and the header's count follows it. The list draws its
groups in this order, each heading saying how many it holds, and the pills
under the bar jump to them:

- **Needs you**: a gate waiting on you, a claim whose lease lapsed, and a
  task marked in progress that no claim holds, the tasks the Overview's
  **Needs you** cards and the sidebar's Tasks badge count. A plan step that
  waits on you is here whatever **Plan steps too** says.
- **In progress**: held by a live claim.
- **Ready**: the first 10 in the sort's order, then "N more ready - show
  them", which draws every one (`?ready=all`).
- **Blocked**: waiting on unfinished work, a step that only your own gates
  hold among them.
- **Waiting on others**: the gates on anyone else, then the steps such a
  gate holds, or whose own gate waits on someone else.
- **Recently closed**: done and dropped in the last 7 days, newest first (the
  newest 25 of them), folded; **All N closed tasks** pages through every
  closed task.

A row is the task's state mark and title, with one line that says what holds
it: a gate's who and what, and how many tasks it holds; when a claim lapsed
and whose it was; a live claim's holder, its lease and when it started; how
long a task has been ready, or **untouched** once nothing changed on it for
30 days; its first blocker, linked, and how many more; who it waits on; when
it closed. Then the last 8 characters of its id, which copy the whole id,
its project, its plan, and the one action the row needs: **Mark as handled**
on a gate waiting on you, **Release claim** on a lapsed claim (it asks in the
page first), and **Mark done** on a ready task or one in progress that no
live claim holds. A row a live claim holds offers no action. Each posts to
the route the task's own page posts to, and lands back on the list at the
row, wherever its new group puts it, with the filters kept and a line that
says what it did. The title, or a click on the row, opens the task in the
[reading panel](#read-in-place); `j` and `k` move a focus ring through the
titles, `Enter` opens the focused task's own page, `/` focuses the filter and
`p` opens the switcher.

The URL holds the filters, so a link keeps them: `?project=<slug>` (`global`
for the global scope; the project switcher, the same one as on
[Memories](#memories), lists the projects with open tasks and their counts),
`?plan=<slug>` (one plan's steps; `plan:<slug>` works too), `?q=<text>`
(title, body or id; a pasted short id finds its task), `?steps=1` (plan steps
too), `?ready=all` (every ready task), `?closed=all` (every closed task, 25 to
a page, with `&page=N`) and `?sort=oldest|newest` (oldest first, the order
agents claim in, is the default). A value the page cannot read, or a project
or plan it holds no task of (a locked one's included), is a 400 that names
what it takes. The JSON model (`?format=json`) carries the rails as before
(`ready`, `inProgress`, `blocked`, `gates`, `gated`, and `closed`, a page of
the newest closed tasks) with `projects`, `filter` and `counts`, which count
as the [project board](#projects) does: in progress is a live claim's alone,
lapsed claims and unclaimed starts are counted apart, and waiting on people is
the gates and the steps they hold together. `counts.open`, the head's **N open**,
is every unfinished row the list draws, so it also counts the plan steps that
wait on you, which Needs you draws while plan steps are toggled off. It adds the list's `needsYou`
(the Needs you group, in its order) and `closedRecent` (how many tasks closed
in the last 7 days).

A task's own page and the panel carry the task's body, claim and lease state,
both dependency directions, its **History**, and the owner's actions on the
task, the same as the peek (the step dossier on a project's Plans & tasks tab):
**Mark done** and **Drop** on an open or in-progress task (a gate whose gate is
still open closes by **Mark as handled** instead of Mark done), **Reopen** on a
closed one, **Mark as handled** with a note on an open gate, **Waits on
someone** on an open or in-progress task whose own gate is not open (who must
act and what must happen, the decision and when it is ready optional: the
task leaves the ready queue for the people's until its gate is marked as
handled), and **Release claim** on a task in progress, whoever holds it or
none. Drop and Release claim ask in the page: the button turns into a second
one to press. While a live claim holds the task the page names its holder and
offers Release claim first: the console never closes a task over its holder's
head. Each action returns to the page it came from, its filters kept, with a
line that says what it did. A task's own URL opens [its own page](#item-page).

The history lists what happened to the task, oldest first, from its
`task.transition` events: created, claimed, released, reopened, done or
dropped, its gate set and cleared, each with the session that did it (linked
when the console can read it, **you** for an action from this console) and
when. A holder's renewed claim reads once, with how many times.

Every task and plan step says where it stands in the same words, on Tasks,
the project workspace and Plans:

| Word | Means |
|---|---|
| **ready** | Open, with nothing unfinished before it: an agent can claim it now. |
| **in progress** | Held by a live claim. |
| **blocked** | Open, waiting on unfinished work before it. |
| **waiting on** *who* | A gate, or a step an open gate holds - its own, or a gate it depends on - named by who must act (`waiting on owner`). |
| **claim lapsed** | In progress, but its holder's lease ran out: any agent can claim it. |
| **unclaimed** | Marked in progress without a claim (`seam task start`, or `tasks_update status=in_progress`): any agent can claim it. |
| **done**, **dropped** | Closed. |

A task's holder and creator are named by their session and link to its page
when the console can read it. The lease reads `lease live, 12m left` while the
claim holds and `lapsed 3m ago` once it runs out.

Each task row - a row of the Tasks list, a plan step, an off-plan row, a
dependency - shows the last 8 characters of the task's id and copies the
whole id, the form you hand an agent (`run <id>`): on the Tasks list the
short id is the copy button itself; elsewhere a copy button beside it stays
quiet until you hover or focus the row, and on a touch screen it is always
there. A plan copies as `plan:<slug>`.

Hover a relative time (`3d`) to see the moment it names, in your browser's own
time; without JavaScript the hover shows it in UTC.

Under a lock, a task blocked by a task in a locked project you
haven't unlocked sits in the blocked rail, its blocker named only by project
on a line of its own; the task's reader and peek list it under **Depends on**
the same way. The project workspace's Plans & tasks tab marks such steps
blocked, and lists such an off-plan task under Blocked, never as ready.
Opening a plan or a session
that lives in a locked project takes you to that project's unlock page, which
says the item is there and links back once you unlock it.

## Locks {#locks}

`/console/locks`, `/console/locks/{id}`

Only while the **Repo locks** [optional feature](#optional-features) is on; a
Standard screen. Every [repo lock](https://thereisnospoon.org/docs/concepts/repo-locks/), shown with its
repository and its name (**LOCK seamless main**): the refs it guards, whether it
is free, held (in the accent tone) or lapsed (in the warning tone), the session
that holds it with since and until, or when it lapsed, each with its date (and
the restricted account that session belongs to, when it is one), the lock's
queue while sessions wait for it (how many, and the next one, with its account
when it is a restricted account's), the repository identities that resolve to
it, and the projects whose restricted accounts may hold it. A lock belongs to
no project, so the page lists every one; a holder or a next waiter in a project
this browser has not unlocked reads "a session this console does not show",
and such a listed project is left out.

- **Waiting for your approval** - the declarations restricted accounts filed:
  nobody can hold one until you **Approve** it, and **Discard** leaves the
  repository free to be declared again. The sidebar badge counts them.
- **Force release** - on a held or lapsed lock: ends the hold whoever holds it.
  The agent that held it is not told, so release a lock only when its holder is
  gone or stuck. On a live hold it asks in the page first; a lapsed one is
  released at once.

`/console/locks/{id}` is the same page with that lock marked; with
`?format=json`, the lock alone.

## Plans

`/console/plans`, `/console/plans/{slug}?project=<slug>`

One board of plan cards for every project: the card a project's
[Plans & tasks](#projects) tab shows, the same template, grouped by the state
the plan is in, top to bottom:

| Group | What puts a plan there |
| --- | --- |
| **Needs you** | A captured plan you can approve (a draft, or presented and awaiting approval), a step waiting on people (an open gate, its own or one it depends on), or a step in progress that no live claim holds (a lapsed claim, or one started with none). |
| **In progress** | A live claim holds a step. |
| **Stalled** | Open steps, none in progress, and nothing moved for 7 days. |
| **Ready** | Everything else still open: open steps that moved in the last 7 days, or a plan with no steps yet. |
| **Shipped** | Every step closed, done or dropped (a dropped step closes a plan as a done one does), or a captured plan the gardener settled. |

Needs you and In progress cards open with their steps; Stalled and Ready
cards show their head line ("3/5 done · 1 ready · 1 blocked · moved 9d ago")
and load their steps when opened. On the board a step names its nearest
blocker; the plan's page names the whole chain. Only **Shipped** is windowed
(24h, 7d, 30d, All time; 24h by default) and folded, with how many shipped
before the window and the wider windows: an active plan is never hidden by a
window. Every card carries its project as a chip that opens that project's
Plans & tasks tab at the plan, and **Approve** on a card returns to the card on
the board, its filters kept.

The project is chosen with the switcher [Memories](#memories) draws, the **In
All projects** button (`p` opens it): **All projects**, **Recent**,
**Starred**, then each project with a plan on the board, most active first,
and the global scope, **global**, last. Each row counts the plans choosing it
would show, and keeps the text filter and the window; projects with no
activity in 30 days fold away and typing finds them; a locked project this
browser has not unlocked is listed with a lock and no count, and links to its
unlock page. `?project=<slug>` narrows the board to one (`global` names the
global scope); a project that is not there, or that is locked and not
unlocked in this browser, is a 400. `?q=`
keeps the plans whose title or slug holds the text, in any case
(`plan:<slug>` works too). A title holding " -- " shows it as a dash that never
splits across lines; the stored title is untouched.

A plan is keyed by its project and slug: the same slug in two projects is two
plans, with their own steps and notes. `/console/plans/{slug}?project=<slug>`
is one plan's [own page](#item-page), read as a memory, a note or a task is:
the way back to the project's plans, project › Plans, then the title row (the
narrative note's title with copy and the star; `plan:<slug>` to copy; the
status in words, a capture's own or else the group the board puts the plan
in; the stalled mark; and **Approve** while a capture awaits it, which returns
to the page), then one line of facts: where it came from, its steps done of
all and the rest counted by their words, its model tokens and when it was
updated. The body column is its steps first, in dependency order (a step
below the steps it depends on, otherwise in the order they were written),
each with its holder, its blockers and the gates it waits on, then its
narrative; beside it, the narrative note, the notes attached to it
(supporting notes and agent caches) and the project, which opens its Plans &
tasks tab at the plan when it has a workspace. A plan whose steps exist with
no note tagged `plan:<slug>` in its project still opens: its page says it has
no narrative note yet, and has nothing to star. Every plan
link the console renders names its project (the board, the project tab, the
plan's page, Overview, Now, the Diary, Search, a task's own page, notes and the
Gardener); a slug that is a plan in more than one project, opened without `?project=`,
answers a chooser naming each project's plan (with `?format=json`, a 409 that
names them), never one page holding both. A plan's source reads "captured from
plan mode" (a Claude Code capture, with its plan file, iteration and agent
runs) or "from tasks" (a composed plan). **Approve** appears for captures
only.

With `?format=json` the board is `rows` (each plan the board lists, in its
order, with its `state` and `href`), the group counts (`needsYou`,
`inProgress`, `stalled`, `ready`, `done` for the shipped plans the window
lists, `shippedAll`), `total`, `projects` (the menu) and `filter`; a plan's
page is its row, body, attached notes and `tasks` (its steps in the page's
order, each with its word, holder and the gates it waits on).

Each plan also carries a **model tokens** rollup - the cumulative transcript
tokens of every session attributed to the plan (any session that moved a step,
or captured the plan) - compact on its card (`~483k tok`) and qualified on its
page (`~483k model tokens · 3 sessions (1 unreported)`). Attribution is
whole-session on purpose: tokens are only ever known per session, so a
session's full burn counts toward each plan it touched, counted once however
many steps it moved. Claude Code reports tokens at session end (and, while the
[Diary](#diary) is on, as its prompts keep its usage current), so a live
session stays *unreported* until then; a session that touched more than
one plan is disclosed as *shared* rather than split by guesswork - which is
also why plan totals must never be summed across plans.

## Labs

`/console/labs`, `/console/labs/{name}`

The research-lab surface (the console twin of `lab_open` / `trial_record` /
`trial_query`). Labs and Trials are one [optional feature](#optional-features)
and ship off; while it is off both screens answer with a short "switched off"
page and neither appears in the nav.

A lab is not a stored entity - it is the label its trials carry, a stable name
for one line of investigation - so this screen is an aggregation over the trials
table and there is nothing to write.

The same library shape: a rail of labs, most recently active first, each with
its trial count and pass/fail tallies. The reader shows one lab's whole
identity - outcome tallies (pass, fail, partial, inconclusive, and *other* for
free-form or empty outcomes), the projects and sessions its trials touched,
first and last activity - and its trial history, newest first, each entry
linking into the Trials screen. Long histories cap at 100 with a pointer to the
uncapped, filterable view.

## Trials

`/console/trials`, `/console/trials/{id}`

The flat, filterable view over every recorded trial - the console twin of the
`trial_query` MCP tool, and part of the same
[optional feature](#optional-features) as [Labs](#labs). The rail groups trials
by lab (a group sits where its newest trial does) and filters by `?lab=` and
`?outcome=`. Choosing an outcome opens the list anew, with its newest match in
the reader, never beside the trial that was open. Outcomes are
free-form by design, so `?outcome=` is an exact-match filter rather than a
validated enum; the seg offers the conventional values (`pass`, `fail`,
`partial`, `inconclusive`).

The reader shows one trial's full record: what changed, **expected vs actual**
side by side (the actual pane tinted by outcome), the structured metrics
`trial_record` captured, and its provenance - lab, project, and the recording
session, each linked. Trial hits also surface in [search](#search) and the
command palette, and a session's page lists the trials it recorded.

## Context

`/console/context`

The briefing topology that the plans board does not show: which knowledge pools
are eligible at SessionStart, which configured edges are currently enabled by
the effective briefing settings, and where project splits moved durable memory.
It covers global memory, one-way parent-memory inheritance, bidirectional sibling
families (findings and the opt-in memory channel), unregistered-project warnings,
and retired-project split lineage reconstructed from the memory-move event log.

`?scope=all` (**All projects**) renders every known project, with global memory shown as the
shared source pool; `?scope=project&project=<slug>` focuses the same topology on
one project. The legacy `/console/relations` route permanently redirects here
and preserves its query string.

Reachable from the Projects board.

## Retrieval

`/console/retrieval`

The circulation report: is stored knowledge actually reaching agents, and at
what cost? The hero pairs the **reach ring** (distinct active memories that
surfaced, over all active memories) with the window's volume and cost -
injections, sessions reached, and **estimated tokens injected**. Everything
follows the selectable observation window except where a panel says otherwise.

Five zones below it:

1. **Delivery path** - the funnel as a flow: injections → distinct memories →
   sessions reached, ending in the knowledge-base coverage meter (how many
   active memories are still waiting to surface).
2. **Circulation pattern** - the injection trend chart and the traffic-by-kind
   mix.
3. **Project coverage** - reach per project (the global scope first), each row with
   its own reach rate and injection count.
4. **Knowledge pressure** - the most-injected memories against **quiet
   knowledge**: active memories not updated, injected, or read within
   `gardener.staleness_days` (90 days by default), the horizon the gardener's
   staleness pass proposes archives by, and the label says the configured
   value. Unlike everything else on the page, the stale list is all-time, not
   windowed. Each row links to its memory and, when the gardener already holds
   a pending proposal for it (an archive, a merge, a move, a reclassification),
   to that proposal: **proposal waiting**. The staleness pass never proposes
   archiving a constraint, a stage, or a memory another memory links to, so
   such a row can stand without one.
5. **Loop health** - push versus pull: is what briefings push also what agents
   pull? **Demand rate** is the share of briefed memories that were also pulled
   by a query; **waste share** is the share of injected tokens spent on memories
   with no query-gated demand, judged against a fixed trailing 30 days whatever
   the window. Two miss stats sit side by side and measure different paths:
   the **recall-miss rate** is ambient - prompts that matched no memory on the
   [`<seam-recall>` path](https://thereisnospoon.org/docs/concepts/recall/#the-recall-triad) - while **agent
   search misses** are deliberate `recall` calls that found nothing; recurring
   ones feed the gardener's
   [memory-wanted pass](https://thereisnospoon.org/docs/concepts/gardener/#what-it-looks-for). **Funnel by
   surface** splits the read-after-inject funnel by injection surface -
   session-start briefings versus subagent-start child injections - each with
   its injections, distinct memories, and the share pulled by a query-gated
   read within the following 24 hours. The zone closes
   with the **dead weight** panel: memories briefings kept injecting without a
   single recall hit, prompt match, or read in 30 days (constraints and stages
   exempt as pinned-by-design) - the evidence behind the gardener's dead-weight
   archive proposals. Its rows link to a waiting proposal the same way.

Reachable from the Overview's reach, injections and sessions-reached vitals
and its going-stale card, at Advanced.

## Gardener

`/console/gardener`

The review queue. Each pending proposal renders as a card showing exactly what it
would do - the memory to archive and why (whether staleness, a dead stage, or
dead weight flagged it), the pair to merge with their similarity score, the
digest or consolidated memory with its body rendered, the reproject's source and
destination, the rekind's from and to kinds, the split's children and shared
parent, and the **knowledge gap**
card with the queries agents kept searching for in vain - applying that one
opens a task; nothing is written until someone writes the memory.

Split batches are grouped by plan and reviewed together, setup card first, with
**Apply whole plan** and **Dismiss all N**.

Under the head and its status line (below), one line says who may apply a
proposal and which kinds apply automatically, with a link to **Settings ›
Gardener**, and **Ask for a cleanup** is a button that unfolds the ask box
(with no chat model configured it is one line pointing at Settings); the queue
follows, as a rail of proposals beside the reader. From the Standard level,
**Type** (All, Split plans, Errors & gaps, Memory cleanup, Plan settlements,
Digests: `?group=`) and **Source** (All, Asked by you, Background: `?src=`)
narrow the rail. The
reader says why each proposal was raised in a plain sentence. A merge shows
both memories side by side - each body, when it last changed, when it was
last delivered and how often - and **Swap** keeps the other one instead; an
archive shows the memory's body, who wrote it, when it last changed and when
it was last delivered. A memory the request may not read is not shown. A rail
row says **asked by you** or **background**. Each rail section offers
**Dismiss all N** and, when every proposal in it can be undone and none asks
to be read first, **Apply all N**. Both ask in the page. Apply all takes
exactly the rows shown, so a proposal raised after the page rendered is never
applied unseen; Dismiss all takes the section as the Source filter narrows it.
Each proposal lands in the decision history with its own Undo. A section
holding a consolidation never offers Apply all. On the Gardener `a` applies
the open proposal and `x` dismisses it.

The decision history opens with **Applied automatically**: every proposal
automatic apply applied that is still applied, uncapped, each with its Undo
(`/console/gardener/decisions#gardener-auto`). Every other decision is paged
below it, so each appears once.

A status line under the head says what the background passes are doing: when
the last pass ran and when the next one is due, each check a pass skips and why
(dedup with no embeddings, digest with no chat model, a pass the config turns
off), and any check that failed. An empty queue reads "The garden is tidy" only
after a pass that ran every check; otherwise it says what did not run. **Run a
pass now** starts the passes without waiting for the next one; it is refused
while a pass runs. With `gardener.enabled: false` the line reads "Suggestions
are off", no pass is offered, and the upkeep the same schedule carries (idle
sessions reaped, old tool events pruned, retrieval stats refreshed) still runs.
The line carries no counts: a pass spans every project, a locked one included.

The reader's actions are **Apply change** (which asks in the page first when it
cannot be undone), **Dismiss suggestion**, **Hide forever** (from the Standard
level), **Retarget** (reproject cards only, from the Standard level) and
**Swap** (merge cards only); a
split plan's are **Apply whole plan** and **Dismiss all N**. Dismissing answers the evidence in
front of you - the pattern comes back if it recurs; hiding answers the pattern
itself. A proposal changes nothing until it is applied - by you here, by an
agent with `gardener_apply` while agents may apply proposals, or, for a kind
you turned on under **Apply by themselves**, by automatic apply - and every
decision records who made it. The newest ten land in **Recently decided** with
an Undo; the **decision history** (`/console/gardener/decisions`) pages
through all of them, newest first, with the outcome, who decided (you; an
agent, by its session's name when a session made the call; automatic apply;
the gardener settling a proposal whose memory had gone inactive; or unknown
for a decision made before this was recorded), the note or task an apply
produced, and Undo wherever it can be taken back. A hide is additionally listed under **Hidden forever**, where
**Unhide** lifts the block without returning the proposal to the queue. The
ask box under **Ask for a cleanup** only ever
produces more proposals for this same queue. A request recognized as a project
split is planned as a split directly - the plan batch appears below like any
other. When the split's source project cannot be matched, an inline follow-up
asks you to pick the project and plans the split from there; nothing is retyped.

**Who may decide.** Two switches on [Settings, **Gardener**](#settings-gardener)
govern this queue. **Agents may apply proposals** is on by default
(`gardener.agents_may_apply`). Turned off, `gardener_apply` refuses an agent's
apply, dismiss and hide, `gardener_proposals` says the switch is off, and the
decisions on this screen refuse an agent's process on the peer_auth socket and a
paired device; your browser, your own terminal and the bearer key keep working.
It stops an agent's ordinary calls and is not a wall: whoever holds the bearer
key passes. **Apply by themselves** is off by default (`gardener.auto_apply`).
It names the kinds - from `memory_wanted`, `tool_error`, `abandon_plan` and
`ship_plan`, each reversible and only opening a task or retagging a plan note -
that a pass applies by itself the moment it raises one. Such a decision is
recorded as automatic apply, listed in the decision history with Undo, and
counted on the Overview (**N proposals applied automatically**, since your last
decision here). It waits for you instead while write screening's lockdown is
on, when screening would hold the task text it lands, and for a proposal about a
locked or fenced project. Only a pass's new proposals are applied by
themselves: one already waiting, or one you undid, stays for you.

See [The gardener](https://thereisnospoon.org/docs/concepts/gardener/) for what each proposal type means.

## Held {#held}

`/console/held` - while [write screening](https://thereisnospoon.org/docs/guides/write-screening/) is on, at
every level (whoever turns it on must reach what it holds).

The owner's queue of agent writes kept out of the corpus. Each card says what
the write would do (a constraint memory, a note, a delete, a gardener apply),
which project, tool and session it came from (the project and the session
link to their pages), and why it was held - each
reason code with its meaning in words, the most serious first, each in its
kind's color (a secret red, an injection marker orange, hidden text violet,
hidden characters pink, an unlisted link indigo, an encoded blob teal, a
binding-rule policy amber, a restricted account's proposal blue); the card's
edge takes its most serious reason's color. Below that is the write exactly
as an agent would read it: every hidden character spelled out as `[U+XXXX]`,
and secrets redacted, so the page can be reviewed and captured without the
credential it holds. What each reason matched is marked in the text in that
reason's color (hover a mark for its reason), and where matches overlap - an
override phrase inside a hidden comment - the marks nest, so none hides
another. The Review and Changed outside Seamless cards mark their findings
the same way.

A held update of an existing memory or note opens on **What releasing
changes** instead: a line diff of the write against the file as it stands
now, each removed line marked `-` and each added line `+`, unchanged lines
beyond three either side of a change folded into a count, with the same
treatment and marks. The whole write follows, folded. A write that matches
the current file line for line says so. A held new item shows its text alone.

- **Release** lands it through the files layer as the agent wrote it, and the
  memory reads `[owner-confirmed]` in briefings from then on. A held supersede
  releases both halves together; a held delete removes the memory; a held
  gardener apply applies the proposal. When the target changed after the write
  was held, only **Discard** is offered: a release would silently undo that
  later change.
- **Discard** lands nothing and erases the held text.

At the top, **Lockdown** says whether lockdown is on and what holds it, with
the switch; a lockdown the configured lockdown file holds says so, and only
that file going lifts it.

Below the queue, **Changed outside Seamless** lists the memory and note files
the drift watch quarantined - changed or planted on disk by something other
than Seamless - with their verdict codes, the sessions they were injected into
before, and their text with the same treatment as a held write. **Accept**
records one as it stands and lets agents receive it again. **Review** counts
the review's verdicts (safe, flagged, not reviewed), lists each flagged item
the same way with **Mark safe**, and offers **Re-run review**.

**Sessions that read outside text** lists every session an agent guard
marked, with its reason and a **Clear mark** button - the only way a mark
comes off. **Recently decided** keeps the last twenty releases and discards,
without their text. The sidebar badge counts what is waiting.

**Proposals from restricted accounts.** Under `peer_auth`, a [restricted local
account](https://thereisnospoon.org/docs/guides/peer-auth/#restricted-local-accounts)'s change to a
constraint, convention, protocol or stage waits here too, as a proposal: its
card says "Proposed by account <name>", and its button reads **Promote**
instead of Release. Promoting lands it exactly as written, still labeled with
the account; `seam promote` does the same from your terminal. While write
screening is off, a daemon that lists restricted accounts still serves this
screen, showing the proposals only - no lockdown, review or session marks -
and the sidebar badge counts the proposals.

Elsewhere on the console, a memory, note, task, session or trial that a
restricted account wrote carries a small "written by account <name>" chip (a
task whose title or body the account rewrote, "updated by account <name>",
until you have rewritten all of its text; a status change or a claim moves
neither chip), including search
results, a lab's trial list and the project workspace's tabs; events and a
session's activity timeline carry none. Your own items show none.

## System

`/console/system`

Every part of Seamless on this server, drawn as one 3D base: the **doors**
where agents and you come in (MCP, hooks, the peer_auth socket, the listener,
the console, A2A), the **work** the daemon does (retrieval, tasks and plans,
sessions, labs, approvals, write screening, updates, commit records, the
metadata feed, URL capture, the gardener, lifecycle), the **core** that keeps
knowledge on disk (the database, the file watcher, memory and note files,
embeddings, the search index, the chat model, backups, the event log), and
the **outside** services it calls (the LLM provider, GitHub releases).

- **State is the server's.** Each part is running, quiet (on, with nothing in
  the event log for the last 15 minutes), needs attention, or off - drawn grey,
  with what turning it on takes. The header counts each state; click a count
  to light only those parts.
- **Motion is evidence.** A part moves only while it works, and light runs
  along a trace only along the path a recorded event's data took: a memory
  write runs from MCP through the memory files, the watcher and the database
  to the search index, and on to embeddings and the provider. With nothing
  recorded the view is still and draws no frames.
- **Pick a part** - on the deck or in the list under it - for its readout: what
  it does, its numbers for the last 24 hours, events per minute for the last 30,
  its settings, the parts it works with, and the screen that holds more.
  `#<part>` on the URL opens one directly.
- **Light on the GPU.** It draws only while something changes, never while the
  tab is hidden or the view is scrolled away, keeps to a pixel budget on very
  large displays, and asks for the low-power GPU. **Still** (and the system's
  reduced-motion setting) keeps everything in place.
- **Without WebGL** the deck is not drawn: the page says so in one line, the
  list of parts below is the page, and picking a part opens its readout above
  the list.

It is a Standard screen: Basic installs reach it by URL.

## Privacy

`/console/privacy`

What your agents can reach on this server, what one could do if it were
compromised, and where each project's knowledge may go.

- **Your setup** - whether Seamless runs as your own account or as its own
  ([peer_auth](https://thereisnospoon.org/docs/guides/peer-auth/)), and five protection layers: project
  fences, write screening, Seamless in its own account, locked projects, and a
  restricted agent account, each on, off, or waiting on an earlier one.
- **Who can reach what** - the machine drawn as a plan: rooms are OS accounts,
  walls are what the operating system enforces, doors are how Seamless is
  reached. Switch between your account, a dedicated account and a dedicated
  account with restricted agents, and turn on **Compromised agent** to see how
  far one gets in each: in your own account it reads every project's files
  directly, sealed ones too, and takes the bearer key; in a dedicated account
  the data folder is out of its reach and a locked project waits for your
  approval.
- **Where knowledge is allowed to go** - your projects around the global
  scope, with the flows each fence allows.
- **The four fences** - open, confidential, sealed and locked, always all
  four, empty or not: what each promises, what kind of project belongs there,
  the projects in it, and how to put a project there. A locked project shows
  by name and counts only. Under peer_auth the Locked card also lists every
  confidential or sealed project not locked yet, each linked to its lock page
  (`/console/lock/<project>`); without it the card says what locking needs.
- **Find the fence for a project** - three yes-or-no questions that end in a
  recommendation and its command.
- **What to do next** - the steps that would tighten this server, in order.

It reads; it changes nothing. It is a Standard screen: Basic installs reach it
by URL.

## Devices {#devices}

`/console/devices`

Only on a server that serves other computers (TLS, a bind beyond loopback, or a
`server_url` naming another host); elsewhere the page and its routes do not
exist. The computers paired with this server, and how to pair another
([the guide](https://thereisnospoon.org/docs/guides/network-install/#devices)):

- **Pair a computer** - this server's address and its certificate's
  fingerprint, and **Get a pairing code**: a code shown once, valid for 10
  minutes and for one computer, with the client one-liners, which carry the
  fingerprint for setup on the other computer to compare. The code works once,
  so the line left in that computer's shell history is dead after use. Without
  peer_auth the page says the code is the approval. Where this server's address
  is plain http to another host the page says pairing needs TLS, and offers no
  code. Where it names a user the page shows it without the user, says to
  take it out of `server_url`, and offers no code. Where it is this computer's loopback (no `server_url` on a wildcard
  bind) the page says the one-liners carry it, so another computer cannot use
  them, and that `server_url` is the fix; it still offers a code, for a client
  on this computer.
- **Waiting for your approval** - under peer_auth only: each computer that used
  a code, its fingerprint and until when it waits, with **Approve with a
  passkey** and **Deny**. Approving needs a signed-in browser on the
  [passkey origin](https://thereisnospoon.org/docs/guides/peer-auth/#the-passkey-origin) and an enrolled
  passkey; the page says which is missing. A relay paired with its one-time
  code alone, while peer_auth was off, waits here too once `peer_auth.relays`
  maps it: it is refused until **Approve with a passkey** approves it in place,
  the same device and certificate, so it need not pair again, and **Revoke**
  ends it. The JSON lists it under `in_place`, not `paired`. A revoked or
  expired relay is not offered.
- **Paired** - each relay and key device: its key's fingerprint or the start
  of its key, when it paired, whether your passkey or its one-time code
  approved it, when it was last seen, and **Revoke**. A relay shows the uid its
  pairing recorded as yours on its computer; without peer_auth one that
  recorded none (paired before, or by a relay of an earlier release) is warned
  about: re-pair it to bind the owner's uid. From 14 days before this server's
  certificate expires, and past it, the section names every relay paired here:
  a renewal means re-pairing each relay.
- **Revoked and expired** - what ended, when, and from where.

It shows at every [level](#choose-how-much-you-see), in the sidebar's Server
group; `g` then `c` opens it.

## Settings

`/console/settings?s=<section>`

Settings is one section at a time, each shaped by what you came to do rather
than by subsystem. A sub-nav on the left (a row of chips on a phone) switches
sections in place, without reloading the page. Each row names its section and
its state in a word or two, so you can tell from any section whether backups
run or an update waits: the level (Experience), how many features are on, the
version (Your setup, amber while semantic recall's settings have a mistake),
**Up to date**, **v0.7.3 ready**, **Checks off**, **Paused** or **Failed**
(Updates), when the last backup ran, **Off**, **None yet** or **Failing**
(Backups), what the gardener may change without you (Gardener), the briefing
preset or **Custom**, how many projects (Workspaces),
and **Semantic** or **Keywords only** (Knowledge engine). Green is fine, amber
is worth a look, red needs you. A row's tooltip is what the section is for.
`/console/settings` alone opens
**Experience**; an unknown `?s=` is a 400 that names the valid sections. Which
sections a level offers is in the [matrix above](#choose-how-much-you-see); a
section above your level still opens from a link, under the same note a hidden
screen gets. Old `/console/settings#...` bookmarks land on the matching section.

Each section opens with one line: its title, a [hint](#shared-parts) with the
longer why, one sentence, and - where its values can be set here - the source
chip, which says where they come from: **Config file** (the config file and its
environment variables, or the build's default), or **Set here** with **Reset**,
which hands them back to the config file. The chip's hint says more. A value
the config file makes final reads **Config file decides**, said once in its
section, and the control it governs is disabled. Switches save at once, with
**Undo** in the toast ([the shared part](#shared-parts)): Features, Updates,
Gardener and Knowledge engine's ranking rows. A form with a typed value
(Briefing, and the Backups schedule and folder) keeps a save bar that appears
at the bottom once it has unsaved changes, with **Save** and **Discard**
(which puts the form back exactly as the page loaded it); without JavaScript
the bar is simply always there. `GET
/console/settings?format=json` returns the whole payload whatever `?s=` says,
plus `consoleLevel`, `consoleLevelOverridden`, `consoleLevelSource`, and
`sectionStatus`: each section's state as `{"word", "tone"}` by section id
(`ok`, `accent`, `quiet`, `warn` or `danger`), every section at every level.

### Experience

`/console/settings?s=experience`

The three [levels](#choose-how-much-you-see) side by side - Basic, Standard,
Advanced - as cards of one height, the one in force marked **In use**. Each has
a one-line pitch and chips for what it shows (Basic) or adds (the others): its
screens, then its Settings sections, each with a gear. The finer controls a
level adds inside pages fold into a **+N details** chip, whose
[hint](#shared-parts) lists them. All of it is generated from the same
registries the sidebar and the page gates read, so a card cannot promise
something the level does not do. Choosing one is the card's **Use** button
(**Use Standard**, say), and it applies at once: the sidebar and the page update in place, and if the new level
hides the screen you were on you land on the Overview with a note. The source chip
says **Config file**, or **Set here** once a level is stored, whether you chose it
in this console or the upgrade set it (its hint says which); **Reset** hands the
choice back to `console.level`. Below the cards, one card holds two rows:
**Theme**, and **Keyboard** (`?` for every shortcut, `⌘K` to search or jump,
and **Show shortcuts**, which opens the shortcut sheet). The theme is
**System**, **Light**, or **Dark**, kept per browser. System is what a browser
starts on: it follows your computer's light or dark appearance, and follows it
live when it changes. Light and Dark pin one theme. The sidebar's sun and moon
switch the same setting: under System they pin the theme opposite the one on
screen, and picking System here goes back to following the computer.

### Features {#optional-features}

`/console/settings?s=features`

Optional features are the parts of Seamless you can switch on and off, and they
ship **off**: a fresh install exposes none of them until you turn one on. The
section is one row per feature: its icon, its name, a one-line description, a
chip for the data it keeps when it holds any ("8 trials kept", "1 write held"),
and its switch. The name's [hint](#shared-parts) holds the rest: what the
feature is, a generated sentence naming exactly what turning it off takes away
("Turning this off hides the Labs & Trials screens, ..."), and, while it holds
data, what is kept ("Data kept: 12 trials across 3 labs"). A switch saves as
soon as you flip it, with no Save button: the toast names the change
("Momentum is off. Agents see it from their next session.") and offers
**Undo**. Without JavaScript the section keeps a **Save features** button.

There are six today:

**Research labs & trials** owns the [Labs](#labs) and [Trials](#trials)
screens, the trials search scope, and the `lab_open`, `trial_record`, and
`trial_query` MCP tools. Screens and tools move together on purpose, so an
agent never sees a tool for a screen you switched off.

**Momentum** is gentle progress cues woven into existing screens -- plan finish
lines, capture streaks, knowledge payoffs, and project growth -- judged from
real activity, never invented. It owns no screens or tools of its own; turning
it on adds seven surfaces where you already look:

- **Plan finish-line cards** on the Overview attention strip: any plan at least
  80% done gets one positive card naming the exact remaining steps ("seambench
  -- one step from shipped"), linking to the plan, with a thin progress bar
  drawn to the plan's exact done/total percent. The agent briefing's plan
  line carries the same emphasis, so agents are nudged to close it too.
- **The capture calendar** on Sessions: a year of daily activity, cell
  intensity from sessions per day, a distinct dot on days that captured
  knowledge (a session left findings, a memory, a note, or a trial), and two
  quiet numbers -- the current capture streak and the longest ever. The streak
  counts covered days, so it rewards capture, not raw usage; a streak of seven
  covered days or more earns a small flame beside the number, and the number
  itself stays verbatim. The grid is an instrument, not wallpaper: hovering a
  cell reads out its day, clicking one focuses the session map on exactly that
  day (a clearable chip names the focus; picking a time window widens back
  out), and the grid is keyboard-walkable -- arrows move a day or a week,
  Enter focuses, all without a page reload.
- **Knowledge payoff moments**: the first time a memory is read by a session
  other than the one that wrote it, the moment lands in the activity ledger
  ("gotcha chroma-boot-race just paid off for the first time") -- once per
  memory, ever -- and the Overview rail gains a **Memory of the month** panel
  naming the last 30 days' top-utility memory with the counts behind the claim.
- **Maturity stages** on the project board and detail header: each project
  earns a latched stage -- seedling, sprouting, established, deep-rooted --
  from real thresholds over age, memories, event volume, and reach. Stages
  never regress, and the pill's tooltip states exactly what the next stage
  asks for.
- **The plan-shipped settle** on Plans: the task transition that closes a
  plan's last step -- wherever it lands, an agent shipping over MCP included --
  mints a once-ever plan.shipped moment. The plan's row settles with a single
  wash when it happens while you are watching, the ledger renders it under its
  own flag icon, and the Plans header counts the local month: "Plans shipped
  this month: N".
- **Milestone moments** in the activity ledger: a short latched set of honest
  firsts and counts -- a project's 100th/500th/1000th memory, its 1000th
  answered recall, its first supersession, its first shipped plan, and its
  birthdays -- each minted once ever and rendered under an award glyph with
  the exact claim ("100 memories written in seamless"). Milestones accumulate
  in the ledger only; there is no trophy screen.
- **Witnessed unlocks** on Settings: a ranking project's "since" note in
  Knowledge engine upgrades to the date it unlocked, so a latch that used to
  flip silently is witnessed.

Motion keeps one register: every animation plays once -- on a live arrival or
on first render -- and nothing loops except the existing pulse idioms. A moment
already on screen when the page loads renders plain; only a new arrival over
the live feed animates. `prefers-reduced-motion` stills all of it, and the
shipped-plan settle is the ceiling: no confetti and no toasts, which belong to
gamification's arcade on Now.

Momentum keeps the console's judged-numbers ethos: every number is real and
verifiable, empty states say so honestly, and there are no punishment mechanics
-- an inactive day is an empty cell, never a warning, and nothing nags or
expires. Off (the default), none of it renders and none of it is computed: no
latch moves and no moment is minted. Moments minted while it was on stay in
the event history (nothing is ever deleted), though milestone rows leave the
activity feeds until it returns.

**Gamification** is the arcade layer of the [Now](#now) screen. Where momentum
asks "is this knowledge practice building on itself?", gamification plays back
"how hard is the fleet running right now?" - and an owner who enjoys one
framing may find the other noisy, so they toggle separately. It owns no screens
or tools of its own; turning it on adds four surfaces to Now:

- **The day tape**: today's judged output - tasks closed, memories and notes
  written, plans touched, sessions started - each cell against its trailing
  7-day daily average.
- **The personal-records rail**: latched bests (most tasks closed in a day,
  most memories written in a day, most agents live at once). Records only ever
  move forward, like maturity stages; an unset record reads "today could be
  the day".
- **The hot-streak pulse**: the last ten minutes' event count beside the
  titlebar pulse, catching fire past a fixed floor. The count is shown
  verbatim either way, so the claim stays verifiable.
- **Celebration moments**: a record falling or a plan shipping its last step
  today renders a moment chip - and when one lands while you are watching, a
  toast and a brief confetti burst (skipped under reduced motion). A record
  crossing is minted into the event ledger at most once per record per day.

The same guardrails as momentum apply: every number is judged from recorded
activity, never invented, and there are no punishment mechanics - a quiet day
is an empty tape, never a warning. Off (the default), none of it is computed,
no record latch is written, and the Now page carries zero trace of it.

**Write screening** is the only feature that changes what agents receive
rather than what the console shows. On, every agent write to a memory or note
is screened, the suspicious ones - and any change to a binding rule by a
session that read outside text, and any supersede or delete of a constraint -
wait on the [Held](#held) screen instead of landing, and briefing and recall
lines carry owner-confirmed or agent-written marks. Its row's chip counts
the writes waiting; turning it off stops screening new writes and releases
none of them. With restricted local accounts configured, the row's hint says the
Held screen stays for their proposals and only its screening parts go. The
[write screening guide](https://thereisnospoon.org/docs/guides/write-screening/) has the screens, the reasons
and the events.

**Repo locks** owns the [Locks](#locks) screen, the `locks_list`,
`locks_declare`, `locks_acquire` and `locks_release` MCP tools, and `seam
lock`: [locks on a repository's refs](https://thereisnospoon.org/docs/concepts/repo-locks/) that agents
declare once and hold while they land changes. Turning it off deletes no lock
and no declaration.

**Diary** owns the [Diary](#diary) screen - a page for each day of work, at
every level, the first row of the Work group - and one in-page surface, the
**Open in Diary** link on a [Sessions](#sessions) day view. Turn it on here,
or with `features.diary: true` in the config file or
`SEAMLESS_FEATURES_DIARY=true`. Off (the default), there is no sidebar row,
palette entry, shortcut, link or script, and `/console/diary` answers with the
switched-off page. It owns no MCP tools. The token record underneath is kept
either way: the [usage ledger](https://thereisnospoon.org/docs/reference/storage/#what-lives-only-in-sqlite)
and the event log the Diary reads are written whether it is on or off, so the
history is there the moment you turn it on. Turning it on adds one cost that
is not presentation: while it is on, a Claude Code prompt keeps its session's
usage ledger current, read after the prompt is answered, at most once every
three minutes a conversation
([mid-session usage](https://thereisnospoon.org/docs/reference/hooks/#mid-session-usage)). With it off, every
hook answers and writes exactly as before.

**Nothing is ever deleted.** Switching a feature off gates exposure and nothing
else: the trials stay in the database, its screens answer with a short "switched
off" page that links back here (a JSON caller gets a 403), and switching it back
on restores every surface exactly as it was. That is what the data-kept chip on
each row is for - it reports the feature's own rows whether it is on or off.

**When the change lands** depends on the surface:

| Surface | When |
|---|---|
| The console | Immediately - nav entries, screens, the search scope, and the overview tiles appear or disappear on the next render. |
| A tool call | Immediately - a disabled feature's tool is refused as an unknown tool, whatever list the caller is holding. |
| A hook | Immediately - the Diary's mid-session usage read follows the switch from the next Claude Code prompt, with no restart. |
| The agent briefing | The next session start - momentum's finish-line emphasis reads the same stored override the console gates on. |
| A client's tool list | The next time it lists tools, in practice its next session. Seamless declares `listChanged: false` and sends no tool-list notification, so a connected client keeps the list it already has. |
| The client-side skill | The next `seamlessd install-hooks` run. The daemon does not reach into `~/.claude/skills` on a toggle, so `seamlessd doctor` raises an **info** line while a skill for a disabled feature is still sitting in a client's skill home. |

Each save writes a **stored override** row in the database - the same layer the
briefing form uses. It wins over file and env, never touches your config file,
and holds until **Reset** on the source chip clears it; reset means back to
the file/env configuration, which unless you set `features:` there is off.

One override can be in force without you having set it. Upgrading an
installation that already holds trial data seeds the override with research on,
so a feature that now ships off does not disappear from under data you were
already using. That is why the chip's hint says the choice is stored here, by a
save or by that upgrade, rather than crediting you with it - and reset clears it
like any other. At Basic the section keeps the rows, what each hides, and the
data-kept chip, and leaves out the agent tool names and the source chip.

### Your setup

`/console/settings?s=setup`

What runs where, as one card of rows. Each value is a chip or a short path, the
detail is one hover away in a hint, and each row whose value can change links
to the section that changes it:

- **Version**, with the Updates section's state chip (**Up to date**, **v0.7.3
  ready**, **Checks off** and so on) and a link to [Updates](#updates);
- **Running on**: the machine, and under [peer_auth](https://thereisnospoon.org/docs/guides/peer-auth/) the
  account the daemon runs as;
- **Config file**, with a copy button and **Open in editor** - or **None**,
  when the daemon runs on its defaults and environment variables;
- **Data folder**, with a copy button, and the database's size (its path in
  the hover);
- **Agents connected**: one chip per agent client that has recorded sessions,
  with when it was last active;
- **Semantic recall**: **semantic** and the model, or **keywords only**, with
  what to do about it in a hint and a link to the troubleshooting guide; the
  row links to [Knowledge engine](#knowledge-engine);
- **Backups**, where a backup service runs: the Backups section's state chip,
  how often they run and how many are kept, and a link to [Backups](#backups);
- **Gardener**: **agents apply** or **only you apply**, how many kinds apply by
  themselves, and **suggestions off** while its passes are off - its cadence in
  a hint - with a link to the Gardener section.

A path under your home folder reads `~`, and a long one is shortened in the
middle; its hover and its copy button hold the whole path. Links to these docs
close the section. No budgets or policy numbers: those live in Knowledge engine.

### Updates

`/console/settings?s=updates`

The [automatic update check](https://thereisnospoon.org/docs/updating/#automatic-update-checks) and
[automatic updates](https://thereisnospoon.org/docs/updating/#automatic-updates), what is true first.

While something needs you, one alert card opens the section, worst first, each
item with its action and its details in a hint: the update checker **stopped**
after an internal error (restart the daemon); and on an install that updates
itself, automatic updates **paused themselves** and releases **held back** after
you went back to an older one (one **Resume** lifts both), releases automatic
updates **skip**, and the **backoff** after a failed attempt, with when they try
again. Last comes the **last attempt**, while it
failed, rolled back or was interrupted below the release it tried, could not be
rolled back cleanly, or applied with warnings (with `seamlessd install-hooks`,
which re-wires your agents, and a copy button). Its line says what it meant -
and, when the updater refused to start, the fix - and a skip or a backoff for
the same release joins that line. Its log path has a copy button, and its hint
holds the updater's own error: the one place in the console either appears.

The first card shows the installed version, the kind of build and who installed
it; the newest release, with its date and release notes, or **Up to date**; and
an attempt that needs nothing from you, in one quiet line. On an install that
updates itself, **Update to v0.7.3 now** installs the newest release at once
after a confirm - skipping the soak, the idle wait, skipped releases and the
backoff - and the daemon restarts to finish. While an update runs, the card
says **Updating to v0.7.3** and its stage, and **Confirming v0.7.3** once the
new version serves. On an install that does not update itself, the card ends
with **To update**: the command that does, with a copy button, and why this
install is only told in a hint.

Then the switches, which save as you flip them, with **Undo** in the toast:

- **Check for new releases**. Its hint says what a check sends: one anonymous
  request to `api.github.com` every few hours, and nothing at all while it is
  off. Its line says when the daemon last checked and checks next, or a streak
  of failed checks and why, the error in a hint. **Check now** asks GitHub
  once, right away (not more than once a minute).
- **Install updates by themselves**, on an install that updates itself: the
  installer's, or a peer_auth system daemon in the guide's layout. Its line
  names the next update and what it waits for - the soak, live agent sessions,
  or a lull after its deadline - and its hint says how an update installs and
  the command that updates by hand.
- **Owner-signed releases**, with
  [`update.release_signers`](https://thereisnospoon.org/docs/updating/#owner-signed-releases) set in the
  config file or environment: a **required** chip, the file in its hint, and
  the last release it refused and why: it carried no owner's signature, was
  signed by a key the file does not list, or its signature did not verify.
  Without the key the row is not there. The console can neither set nor clear
  it.

The source chip says who decided the switches: **Config file** (the config
file, or the build's default), or **Set here** with **Reset** when this console
set either switch; its hint says which. Turning a switch off from here always
works for you; an agent's process on the peer_auth socket, and a paired device
but for your own hands through a relay, may turn checks and automatic updates
on, never off. `update.check: false` in the config file or
`SEAMLESS_UPDATE_CHECK` is final and decides the whole section: the head's chip
reads **Config file decides**, the switches are locked, and saving checks on is
refused - `false` promises no update traffic at all. `update.auto: false` is
final for its own switch, so its **Config file decides** chip sits beside it.

When a newer release is out, the Overview health strip's version fact turns into
`v0.7.2 -- v0.7.3 available` and links here - its hover says whether it installs
by itself, and when - and a toast announces it if the console is open when the
daemon finds it. While an update runs, the fact reads `v0.7.2 -- updating to
v0.7.3`, and once v0.7.3 is serving and the updater is only confirming it,
`v0.7.3 -- confirming the update` (the first card says **Confirming v0.7.3**
too). The daemon re-reads the updater's record every few seconds while an
update runs, so both clear soon after it is confirmed. For a day after the
daemon starts on a new version, every page carries
a banner - "Seamless updated to v0.7.3 (from v0.7.2)" - with the release notes
and **Got it**, which hides it in this browser. It also says when Codex's hooks
changed with the update (re-approve them in Codex's `/hooks`) or the installer
reported warnings (run `seamlessd install-hooks`). For a week after automatic
updates pause themselves, or after an update the daemon started does not apply,
every page also carries a warning banner pointing here. While automatic backups
are in trouble too, the two warnings share one row with one **Got it**.

### Backups

`/console/settings?s=backups`

Scheduled backups of the whole instance into one folder you choose, the
backups found there, and restore. [Backups in the console](https://thereisnospoon.org/docs/guides/data/#backups-in-the-console)
walks through it; this is what each card holds, what is true first.

While something needs you, one alert card opens the section, worst first: the
backup service **stopped** after an internal error; **Automatic backups are
failing** - how many automatic runs failed in a row and since when, the last
one's error, and its reason in a hint - with **Try again**, which runs the
schedule's backup now; and **Your backup did not finish**, with its error.

The first card says when the last backup ran - **Backed up 2h ago**, with its
size and what it holds in a hint - or **No backup yet**, or the backup under
way. Its line says when the next one runs (or **Nothing changed since**, with
when it checks next) or that automatic backups are off, how many are kept, and
the newest backup's size. The folder is a short chip with a copy button, the
whole path in its hover, and **Back up now** makes a backup marked **Made by
you** whatever the schedule says. Its mark is green while backups run on time,
amber while automatic backups have made none yet, are overdue or are failing,
red when the service stopped, and grey while they are off.

The schedule, a form with a Save bar, since it holds typed values:

- **Back up automatically**, a switch. A run is skipped when nothing has
  changed since the last backup, or when the disk is short of room.
- **How often**: Hourly, 6 hours, Daily or Weekly. An interval set in the
  config file (anything from `1h` to `720h`) shows among them as itself.
- **Keep**, 1 to 100 automatic backups. Its hint says older automatic backups
  are deleted after the next one, and backups you make yourself and safety
  backups never are.
- **Folder**, with a live check as you type (it only looks, never writes) and
  chips for the places on this machine this account can write to: iCloud
  Drive, cloud folders and external disks on macOS, Dropbox and external disks
  on Linux, OneDrive and other drives on Windows, and Seamless's data folder.

**Save** stores only what differs from the settings in force; a new folder is
created and tested with a write first. The source chip in the section's head
says where the values come from: **Config file** (the config file, or the
build's default), or **Set here**, whose hint names the fields you set and whose
**Reset** makes the settings follow the config file again.

**Available backups**, the list, newest first: the backups in the chosen folder
and in Seamless's data folder (`~/.seamless/backups`, where the safety and
pre-update backups stay). Each row has:

- when, the kind in words - **Automatic**, **Made by you**, **Before restore**,
  **Before updating from v0.8.1** or **Exported** - the size, and the machine
  it came from when another Seamless made it;
- **What it holds**: the file's path, and per project the memories, notes,
  tasks and sessions, beside **vs now**, how many more or fewer memories and
  notes the project holds now. A locked project shows its name and the backup's
  counts, never what it holds now. A project that is no longer here says so,
  and one that is only here now shows dashes;
- **Restore…** and **Delete**, which asks in place first. An iCloud
  placeholder (download it in Finder first) and a backup a newer Seamless made
  (update to restore it) cannot be restored and say why.

The list's title carries a hint: where these backups come from, and that a
locked project shows what the backup holds of it, never what it holds now.

**Restore…** opens a confirm card in the backup's row, with a link that works
without script:

- it compares the backup with now (how many more or fewer memories, notes and
  sessions this install holds);
- two choices: **Replace all data with this backup** (your current data is
  saved as a backup first, so you can undo it) or **Only bring back what's
  missing** (nothing you have now changes). A knowledge-only backup offers only
  the merge;
- what a restore leaves alone: locks, passkeys, repo routes and console
  settings stay as they are now, and no project's privacy is loosened;
- **Restore**, or **Restore with passkey** under the
  [peer_auth gate](https://thereisnospoon.org/docs/guides/peer-auth/#restore). Without a usable passkey, the
  card names why and shows the terminal command instead, with a copy button.

Then a progress card lists the [steps](#shared-parts), numbered, each done,
under way or still to come: **Checking the backup**, **Preparing the restored
data** (replace) or **Saving your current data** (merge), **Restarting
Seamless**, **Done**, and for a replace, **Saving your previous data as a
backup**. The page waits through the restart and draws the section again in
place. **Cancel restore** is offered while a restore is staged. On Windows, the
card names `seamlessd restart`, to run within an hour, after which a staged
restore is dropped.

After the restart, the result card says what was restored, where the previous
data was saved, and anything the restore kept of today (a stricter privacy
setting, locked projects the backup did not have), with **Undo this restore** -
the confirm card for the safety backup - and **Dismiss**. A restore that did
not finish, expired or stopped partway says so there instead.

Elsewhere:

- **Settings > Your setup** has a **Backups** row: the Backups section's state
  chip, how often they run, and how many are kept.
- Every page carries an alert while automatic backups are in trouble, linking
  here. **Automatic backups are failing.** appears when two automatic runs in a
  row did not finish, with the first failure's time and the reason.
  **Automatic backups are overdue.** appears when none has finished for three
  intervals. **Got it** hides it in this browser until a new episode begins.
- [System](#system) draws backups as a core part, on, off, failing or
  overdue.

### Gardener {#settings-gardener}

`/console/settings?s=gardener`

Who may act on the gardener's proposals, and which apply by themselves: a
Basic section, beside the [Gardener](#gardener) screen, whose sentence about
who may decide links here. Its source chip says **Config file** (the file, the
environment, or the default) or **Set here**, whose **Reset** hands both
switches back to the config file; the chip's hint says where each one comes
from. Both switches save the moment you flip them, with **Undo** in the toast
(without JavaScript, each card keeps a **Save** button), and apply at once,
with no restart.

- **Agents may apply proposals**, on by default (`gardener.agents_may_apply`).
  Off, only you decide: `gardener_apply` refuses an agent's apply, dismiss and
  hide, and so do the Gardener screen's decisions from an agent's process or a
  paired device. `agents_may_apply: false` in the config file or environment
  is final: the switch is disabled, with a **Config file decides** chip beside
  it.
- **Apply by themselves**: one switch per kind a pass may apply the moment it
  raises one - a task for a knowledge gap or for a recurring error, a finished
  plan marked shipped, a stale captured plan abandoned - each with how Undo
  takes it back, and a count of how many are on. Every other kind always waits
  for you. The group's hint says when a kind you turned on waits for you
  anyway: while lockdown is on, when write screening would hold it, and for a
  locked or fenced project.
- **Runs every N min**: the gardener's cadence, with **suggestions on** or
  **suggestions off** (`gardener.enabled`) and **upkeep always**: idle sessions
  are reaped, old tool events pruned and retrieval stats refreshed on the same
  clock either way. **Tuning, from the config file** folds the six values the
  passes use: the dedup threshold, when a memory and when a captured plan
  count as stale, the digest window, how long tool events are kept, and when
  a session counts as idle. They change in the config file or environment,
  then a restart.

The section's sub-nav state says what the gardener may change without you:
**Proposes only**, **Only you decide** (agents may not apply), **2 of 4
automatic**, **Suggestions off**, or **Unreadable** (amber) while the stored
switches cannot be read. An old link to the switches' place in Knowledge
engine (`?s=engine#settings-gardener`) opens this section. **Your setup**
shows both as chips, with a link to this section.

### Briefing

`/console/settings?s=briefing`

What every new agent session starts with. Where the section is wide enough
(a desktop window), the form is on the left and the preview beside it, in view
while you scroll the knobs; narrower, the preview follows the form.

Three **presets** lead the form under **Start from** - **Lean** (the binding
rules and the freshest memories), **Balanced** (exactly the defaults), and
**Rich** (more rules in full, more recent work, and the family) - each card
showing the numbers that set it apart until the preview prices it for its
project ("~780 tokens"). **in use** marks the preset your saved values equal.
Picking one fills the form; nothing is saved until you save. Below them,
**Fine-tune** holds every knob in compact groups - Memories, Recent work, Plans
and stages, Project family, Ranking, and Ceiling. Each knob is a label, an (i)
hint that says what it does, and its value with its unit; at 0 the unit says
what 0 means instead ("any age", "off", "budget", "always", "all in full"), and
follows as you type. The form marks the preset its values equal, or **Custom**
when they match none, and re-marks it as you edit.

Saving writes a runtime override row in the DB. It layers over the file/env
values and wins until reset, and it applies from the next session start - no
daemon restart. It never touches your config file, so `seamless.yaml` stays the
thing you wrote. The token budget (`budgets.max_briefing_tokens`) is not a
briefing knob and no preset moves it. The form validates: a non-numeric knob or a
value that fails `Briefing.Validate()` comes back as an error flash, not a
silently-dropped save.

The **preview** shows the `<seam-briefing>` a new session in one project would
start with, under the values the form holds right now, saved or not (**unsaved
values** says when they differ from what is saved). Pick a preset or change a
knob and it follows within a moment. It
also shows the token estimate against `budgets.max_briefing_tokens` and the hard
cap, and flags a briefing that runs over the budget (only constraints, pinned
stages, and starred rows can put it there) or would be cut at the cap. The project picker
lists the registered projects and opens on the one your agents worked in last.
When the page opens and when you pick another project, the preview also prices
each preset for that project: `presets=1` on the request adds one assembly per
preset and a `presets` list (`key`, `tokens`) to the JSON answer.
A preview is a read: it saves nothing, records no event, and no agent receives
it, so it never counts toward retrieval stats or utility. Values the save would
refuse, it refuses with the same message. For scripts,
`GET /console/settings/briefing/preview?project=<slug>&format=json` previews the
saved values. `POST` to the same path, with the form's fields plus `project`,
previews unsaved ones. It is the one POST in the console that writes nothing,
which is why it is not in the table of write routes above.

The **Ranking** group holds **Use weight** (`utility_weight`: utility's share
of the briefing sort key; 0 restores pure recency) and **Turns on**
(`utility_mode`: **Per project** arms each project as its demand history
matures, **Everywhere** is every project now, **Never** is none). Where each
project stands against the readiness gates is in Knowledge engine. See
[Sessions & briefings](https://thereisnospoon.org/docs/concepts/sessions/#the-budget-and-what-survives-it) for
how the blended order behaves once active.

### Workspaces

`/console/settings?s=workspaces`

Which project each repository resolves to, and which projects share context.
**Project families** are editable here: create, rename, edit, or delete the named
groupings that [`seamlessd family`](https://thereisnospoon.org/docs/reference/cli-seamlessd/#seamlessd_family)
manages from the CLI - the same `project_families` setting, so a change on either
surface shows up on the other. Members are chosen from a closed picker of
registered projects; the CLI is the route for pre-registering a slug that does
not exist yet. Below it, one bounded directory lists every project with its repo
routes, plus a Family column when any project has a family or a parent. A repo
route ties a repository folder to a project: an agent that starts in that folder,
or anywhere under it, works in that project. **Find a project** narrows the rows
as you type, matching a slug, a name, or a route's path, and **With repo routes**
keeps only the projects that have one. The switch is offered when some projects
have a route and some do not, and starts on when more than 12 have none. Both
narrow the rows in the page, with no reload; without JavaScript every row shows.
At Advanced each route also names the machine it lives on, and repo paths that
resolve to no project are listed under **Repo routes with no project**.

### Knowledge engine

`/console/settings?s=engine`

How recall finds knowledge and how briefings rank it, in three cards. The
gardener's switches and cadence have a section of their own,
[Gardener](#settings-gardener).

**Recall** - two rows. **Semantic recall** carries a state chip (**semantic**,
**keywords only**, or **misconfigured**), the reason in its sub line, and the
fix in its hint: switched off from this console; no embedding provider set up
(the hint is Your setup's sentence: add a key, then restart; the daemon's own
error stays in its log); or a config mistake, with the provider's error. The
off/auto switch is a settings row read once at serve start, so the row flags a
pending restart whenever the stored switch disagrees with the running process,
and **Switch off** is offered only while there is something to switch off:
embeddings running, or a cleared switch still waiting for that restart. The
second row is the stored vectors, named by the model: their counts in one line
(vectors, items not yet embedded, vectors on an old model), and **By model**,
folded, while there is more than one model or a stale one. **Re-embed all**
rewrites the corpus with the active model in the background, after a confirm
in the page, and the row follows the pass in place while the page is open.

**Ranking by use** - chips count the projects that rank by use now, any that
rank from the gardener's next pass, the forced ones, and the ones still
building. Only the projects that rank, are ready to, or are forced get a row:
its uses, memories used and days of history, a small meter of those three
gates against their thresholds (green once met), and **Auto / On / Off**, which
saves at once, with Undo, and returns to the row. A forced project stays listed
until it is set back to Auto. The rest fold under one row ("42 projects still
building their history"), each with what it still needs ("needs 7 more events,
3d more history") and the same segment. The hint gives the thresholds. When
**Turns on** in Briefing is Everywhere or Never, the card says it decides for
every project, and these choices wait until it is back to Per project.

**Storage** - read-only, with a lock by its name: chips for the data folder
(with Copy), the database (size on disk including the WAL, and schema
version), and the briefing and recall budgets. Its hint says they are set in
the config file or environment and read at start, and gives the database's
path and the tool-event cap.

See [Configuration](https://thereisnospoon.org/docs/reference/configuration/) for what each knob does, and
[Sessions & briefings](https://thereisnospoon.org/docs/concepts/sessions/) for what they tune.

## Event detail

`/console/events/{id}`

What a Recent-activity or timeline row links to: a compact event review workspace
with the event's agent/session attribution, verbatim injected or transport
content, surfaced memories resolved to their live index entries (or flagged
missing), remaining payload fields, and raw JSON (the last two at the Advanced
level; `?format=json` always carries them). The Interactions inspector
uses the same content model in a side pane; opening the full page adds context
without changing the underlying event.

**What it is about.** The item an event names is resolved by the event's kind
and linked: the memory it wrote or read, the note, the task (with its state),
the trial, the session, the plan, or the gardener proposal. A gardener event
also names what its proposal is about - the memory it would archive, the two
it would merge, the plan it would settle - and its title says so: "proposed
archiving a memory:" and the memory's name. A pending proposal links to it on
the Gardener screen, a decided one to the decision history. Everything
resolves through what this console reads: an item it cannot read - deleted, or
in a locked project - is shown by its kind and id alone, never named. Times
read in this browser's own clock, and the page's title is the event's
headline.

## Errors

A bad or stale URL renders a styled, layout-wrapped error page with a way back,
rather than dropping you on a bare `404 page not found` -- including any
`/console/` path no route claims, which gets a search box and links to the
common destinations. A 404 names the missing
entity and a 400 names the bad parameter and its valid values. A 500 stays
generic in the browser - the detail is in the log, not the response. Fragment
fetches (`?peek=1`) get a fragment-shaped error, since a full page injected into
the reading panel would nest the whole console inside itself.
