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
| Archive a memory | `POST /console/memories/{id}/archive` | Routes through `lifecycle.Archive`: the memory is stamped invalid and leaves every index, its file stays on disk with a tombstone. |
| Force-release a task claim | `POST /console/tasks/{id}/release` | The owner override: releases a claimed task's lock regardless of who holds it, reopening it for any agent. Not reachable from the agent MCP tools. |
| Approve a captured plan | `POST /console/plans/{slug}/approve` | The escape hatch for a Claude Code approval whose `PostToolUse` never fired: flips the `cc-plan` note to `plan-status:approved` and creates the tracking task, exactly as the hook would have. Only applies to a CC capture. |
| Star / unstar an entity | `POST /console/favorites/{kind}/{id}` | Toggles the same starred flag the `favorite_set` MCP tool sets: a starred memory pins into briefings and gets a post-fusion recall boost. For memories and notes the star lives in frontmatter; it never bumps `updated`. |
| Ask the gardener for proposals | `POST /console/gardener/request` | Interprets a natural-language maintenance request into **pending proposals**. It never mutates a memory. |
| Plan a project split | `POST /console/gardener/split` | Interprets a split request into a plan batch of **pending proposals** (one split setup plus one reproject per memory). Also never mutates a memory. |
| Apply one proposal | `POST /console/gardener/{id}/apply` | Carries out that proposal's effect. |
| Dismiss a proposal | `POST /console/gardener/{id}/dismiss` | Drops it without acting. The pattern is raised again if new evidence for it arrives after the decision. |
| Hide a proposal forever | `POST /console/gardener/{id}/hide` | Drops it without acting and blocks the pattern permanently - no recurrence re-raises it. Listed under **Hidden forever**. |
| Unhide a pattern | `POST /console/gardener/{id}/unhide` | Lifts a forever block. The proposal stays resolved; the gardener may propose the pattern again the next time it recurs. |
| Retarget a reproject proposal | `POST /console/gardener/{id}/retarget` | Rewrites a **pending** reproject's destination project before it is applied. Reproject proposals only. |
| Apply a whole plan batch | `POST /console/gardener/plan/{slug}/apply` | Applies every pending proposal in a plan, split setup first so the child projects exist before the memories move. Best-effort: it applies what it can, reports how many landed, and leaves the rest pending. |
| Dismiss a whole rail group | `POST /console/gardener/group/dismiss` | Dismisses every pending proposal in one rail section or split plan, one at a time, so each keeps its own event and stays individually undoable. Only Dismiss is offered in bulk. |
| Undo a decision | `POST /console/gardener/{id}/undo` | Returns a resolved proposal to the queue from **Recently decided**, inverting whatever its apply did, for the kinds whose apply can be inverted. |
| Release a held write | `POST /console/held/{id}/release` | [Write screening](#held): lands a held agent write exactly as the agent sent it, and marks the memory owner-confirmed. Refused when its target changed after it was held. |
| Discard a held write | `POST /console/held/{id}/discard` | Lands nothing and erases the held text; the row stays as the record. |
| Clear a session's outside-text mark | `POST /console/held/sessions/{id}/clear` | The only way the mark comes off: the session's changes to binding rules land without a hold again. |
| Turn lockdown on or off | `POST /console/held/lockdown` | `on=1` or `on=0`. Your switch only: a lockdown the configured lockdown file holds stays on until that file goes. |
| Re-run the review | `POST /console/held/review` | Screens every memory and note again and marks the clean ones safe as they read now; your own marks on unchanged items stand. |
| Mark a flagged item safe | `POST /console/held/corpus/safe` | `path=` the item's file. Safe as it reads now; a memory is also stamped owner-confirmed. |
| Accept a file changed outside Seamless | `POST /console/held/corpus/accept` | `path=` the file. Records it as it stands, lifts its quarantine, and stamps a memory owner-confirmed. |
| Set a project's isolation | `POST /console/projects/{slug}/isolation` | Changes how far a project's knowledge travels. A tightening that would cut existing context first renders a confirmation step; nothing is written until it is confirmed. |
| Begin a lock change | `POST /console/lock/{project}/begin` | Only under [peer_auth](https://thereisnospoon.org/docs/guides/peer-auth/#lock-from-the-console-or-force-lock-in-the-file), and only from a signed-in browser. `action=lock` or `remove`. Refuses first a change that could not apply - a force-locked project, a lock already on, no console lock to remove, a project that is not confidential or sealed, no passkey enrolled - then returns a WebAuthn challenge naming this ceremony, the project and the direction. Writes nothing. |
| Lock a project, or remove its lock | `POST /console/lock/{project}/finish` | Verifies the passkey assertion for that ceremony, then stores the lock or removes it. Turning a lock on ends, in the same transaction, the project's agent leases, read grants and console unlocks, so every agent and browser asks again. It removes only a lock the console set: one `peer_auth.force_locked` holds stays. Seamless then restarts to apply the change. |
| Begin a passkey unlock | `POST /console/unlock/{project}/begin` | Only for a project peer_auth locks, and only from the signed-in browser that proves its unlock key. Records a waiting [console unlock](https://thereisnospoon.org/docs/guides/peer-auth/#leases-and-the-process-tree) bound to this browser's cookie and key, sets the `seamless_unlock` cookie when the browser holds no live unlock yet, and returns the passkey challenge. Refused while no passkey is enrolled; the terminal path still works. Opens nothing yet. |
| Unlock a project in this browser | `POST /console/unlock/{project}/finish` | Verifies the passkey assertion for that waiting unlock, from the browser that began it, and makes it live: this browser reads the project for three hours, or less when `peer_auth.max_lease` is shorter. It is never a lease: no agent reads the project through it. |
| Begin a terminal unlock | `POST /console/unlock/{project}/terminal` | **Approve from a terminal**: the passkey begin's gates, without needing a passkey. Records a waiting unlock bound to this browser and returns a pairing code with the `seamlessd console-open --unlock` command that approves it, run as the daemon's account within ten minutes. The code only names that unlock, so it is not a secret. Opens nothing until the command runs. |
| Ask for access | `POST /console/access/request` | The agent's ask, not your decision: `seam access request`, run from the agent's own shell, files an [access request](https://thereisnospoon.org/docs/guides/peer-auth/#requests) for a locked project or, with [access requests](https://thereisnospoon.org/docs/guides/peer-auth/#read-grants-and-the-advisory-tier) on, what the agent's conversation and a confidential or sealed project no lock covers call for: a grant, or a move into the project. With `write=true` it asks for a read-and-write grant or a move, a locked project's included, whose approval opens the lock too. It writes a waiting request, with the agent's reason of at most 200 characters, and opens nothing. An agent that already holds the access is answered with it. |
| Approve an access request | `POST /console/access/{id}/approve` | Your decision: writes the agent's lease on a locked project, or a grant, for `duration=` (until the session ends, 1 hour or 4 hours), and only while the request's facts still match `digest=`, the digest of the facts you were shown. A grant is read-only unless `access=read-write` on a request that offers the choice. A move request is approved for good: it ends the conversation's session and opens its continuation in the project, and `duration=` bounds only a locked project's lock. Under peer_auth it takes a proof: a passkey assertion from the request's page, or a Touch ID signature from Seamless Approve (`proof=touchid`), accepted on the peer_auth socket only and never for a move. In the advisory tier - access requests without peer_auth - a decision with no passkey is the bearer key's, which is what `seam approve` sends. A paired device's approval with no passkey or Touch ID proof is refused, peer_auth on or off: the bearer key's decision is never a device's. |
| Deny an access request | `POST /console/access/{id}/deny` | Closes a waiting request with no lease, grant or move; the project stays closed to that agent, and a move's agent stays where it works. Refused over the peer_auth socket and from a paired device: deny on the console, or with `seam approve` in your own terminal on the server. |
| Revoke an agent's lease | `POST /console/access/leases/{id}/revoke` | **Revoke** under Agent leases on `/console/access`: ends that lease before its expiry and records an `access.revoked` event. Refused over the peer_auth socket and from a paired device. |
| Revoke a read grant | `POST /console/access/grants/{id}/revoke` | **Revoke** under Read grants: ends a live grant, read-only or read and write, before its expiry. Its holder stays write-confined, since a revoke does not unread what it read. Refused over the peer_auth socket and from a paired device. |
| Lock a console unlock again | `POST /console/access/unlocks/{id}/end` | **Lock again** on `/console/access`, or **Lock it again** on the unlock page: ends any browser's live console unlock, by id. Refused over the peer_auth socket and from a paired device. |
| Begin a passkey enrollment | `POST /console/approvers/passkey/begin` | Only under peer_auth: `name=` the passkey (1 to 64 characters on one line). Returns the WebAuthn creation options, excluding every passkey already [enrolled](https://thereisnospoon.org/docs/guides/peer-auth/#enroll-an-approver). Holds the enrollment in memory and stores nothing. Refused over the peer_auth socket and from a paired device, since a passkey is created in a browser. |
| Create the passkey | `POST /console/approvers/passkey/finish` | Verifies the browser's new passkey and returns its key's fingerprint with the `seamlessd approvers enroll-code` command that mints its code. Still stores nothing: the passkey waits for that code. Refused from a paired device. |
| Enroll the passkey | `POST /console/approvers/passkey/confirm` | `code=` the one-time code that command minted for this key. Stores the passkey and uses up the code in one transaction. A code that is mistyped, expired, used or minted for another key leaves the passkey waiting, so it can be typed again. Refused from a paired device. |
| Enroll a Touch ID key | `POST /console/approvers/touchid/enroll` | Seamless Approve's call, from the app `seam touchid enroll` opens, accepted on the peer_auth socket only - never with the TCP key or the console cookie. Stores the Mac's new Touch ID key, proven by an enrollment code minted for that key and the key's signature over it. An earlier Touch ID key of the same Mac that signs its own retirement is retired in the same step. No console route removes any other credential: that is `seamlessd approvers remove`. |
| Begin a passkey sign-in | `POST /console/signin/begin` | Only with `console.sign_in: passkey`, and only from a browser that proves its key (the login page sets it up). Returns the WebAuthn challenge, bound to that key and to a ticket that works once. Writes nothing. A 404 while the key signs browsers in. |
| Sign in with a passkey | `POST /console/signin/finish` | Verifies the assertion for that challenge, from the browser that began it, records the passkey's use, and sets a fresh random sign-in cookie that counts only with this browser's key, for 7 days. A cookie the browser held before ends with it. |
| Begin adding a passkey | `POST /console/passkeys/add/begin` | Only with `console.sign_in: passkey`: the public Add a passkey page asks for the new passkey's `name=`. Returns the WebAuthn creation options. Writes nothing. |
| Add a passkey, waiting | `POST /console/passkeys/add/finish` | Verifies the device's new passkey and stores it as waiting, with a short code, for 10 minutes. It adds nothing: `seamlessd approvers approve --code`, on the server, adds exactly that passkey. At most 32 wait at once. |
| Begin a terminal sign-in | `POST /console/signin/terminal/begin` | Only with `console.sign_in: passkey`, and only from a browser that proves its key: **Show a code** on the login page. Sets a fresh random sign-in cookie bound to that key and stores it waiting, with a short code, for 10 minutes, with the address the request came from and its User-Agent (printable ASCII, at most 256 characters), which the answer also carries for the page to show beside the code. It signs nothing in: `seamlessd console-open --sign-in --code`, on the server (as the daemon's account under peer_auth, at a terminal), shows those and when it began, asks, and on a yes signs in exactly the browser that showed that code, for 7 days. At most 32 wait at once. A 404 while the key signs browsers in. |
| Get a pairing code | `POST /console/devices/code` | Only on a server that serves other computers (TLS, a bind beyond loopback, or a `server_url` naming another host). Mints a one-time pairing code, valid for 10 minutes and for one computer, and shows it once, in this answer, with the client one-liners, which carry the server certificate's fingerprint when it serves TLS; only its hash is stored. Without peer_auth the code is the approval; under peer_auth the pairing it starts waits for a passkey. At most 16 codes wait at once. Refused (409, `pairing needs TLS here`) where the server's address is plain http to another host. Refused from an agent's process on the peer_auth socket, and from a paired device, peer_auth on or off. |
| Begin a device approval | `POST /console/devices/{id}/approve/begin` | Only under peer_auth, from a signed-in browser, for a pairing that waits. Returns the WebAuthn challenge, bound to that device's id, kind, name, fingerprint and key, and a ticket. Writes nothing. |
| Approve a device | `POST /console/devices/{id}/approve/finish` | Verifies the passkey assertion for that ticket over the device as it stands, records the passkey's use, and pairs the device: a relay's certificate is accepted from its next request, and a key device collects its own key, once, on its next status. |
| Revoke a device, or deny a pairing | `POST /console/devices/{id}/revoke` | Revokes a paired device: its credential is refused from its next request, and the requests it is making end. A pairing that waits is denied instead. Refused from an agent's process on the peer_auth socket, and from a paired device, peer_auth on or off. |
| Save briefing settings | `POST /console/settings/briefing` | Writes the briefing knobs as a runtime **override row** in the DB. It never writes the config file. |
| Reset briefing settings | `POST /console/settings/briefing/reset` | Clears the override row, reverting to the file/env configuration. |
| Preview a briefing | `POST /console/settings/briefing/preview` | A read, not a write: it POSTs only to carry the Briefing form's unsaved values, which it checks with the save's own validation, and answers with the briefing a new session in the chosen project would start with. Writes no settings row and records no event. A locked project is not previewed. |
| Force utility ranking for a scope | `POST /console/settings/utility` | Sets or clears the owner's per-scope force: `on` and `off` win over the gardener's readiness latch, `auto` defers to it again. |
| Switch embeddings off or back on | `POST /console/settings/embeddings/mode` | Stores or clears the embedder off switch. It is read at serve start, so the change applies from the next restart - the page says so. |
| Re-embed everything | `POST /console/settings/embeddings/reembed` | Starts a background pass that re-embeds every memory and note with the active model. It changes vectors, never content. |
| Choose the console level | `POST /console/settings/level` | Stores the [experience level](#choose-how-much-you-see) as a row that wins over file and env. Presentation only: nothing an agent receives changes. |
| Reset the console level | `POST /console/settings/level/reset` | Clears that row, back to the file/env level. |
| Dismiss the welcome card | `POST /console/settings/level/welcome` | Records that the Home welcome card was seen, without choosing a level. With the card's write-screening box ticked, it also turns that feature on, as a level choice with the box ticked does. |
| Save optional features | `POST /console/settings/features` | Writes the feature switches as a stored **override row** in the DB. It changes what is exposed - console screens and the matching agent tools - and deletes nothing. |
| Reset optional features | `POST /console/settings/features/reset` | Clears the override row, reverting to the file/env configuration - which, unless you set the keys there, means every optional feature is off again. Still deletes nothing. |
| Turn update checks on or off | `POST /console/settings/updates` | Stores the console's `update.check` override (`check=on` or `off`). It can always turn checks off; it cannot turn them on while the config file or environment says `update.check: false` - that is final, and the form says so. Installs nothing. |
| Reset update checks | `POST /console/settings/updates/reset` | Clears that override - both switches - handing them back to the config file (or, without one, the build's default). |
| Check for updates now | `POST /console/settings/updates/check` | Asks GitHub's release list once, right away - refused while checks are off or within a minute of the last check. Reads; installs nothing. |
| Turn automatic updates on or off | `POST /console/settings/updates/auto` | Stores the console's `update.auto` override (`auto=on` or `off`), keeping its `update.check`. It can always turn automatic updates off; it cannot turn them on while the config file or environment says `update.auto: false`, or locks `update.check: false` (nothing installs without checks). With checks merely off, the choice is kept and applies once they are back on. Only an install that updates itself acts on it: the installer's, or a [peer_auth system daemon](https://thereisnospoon.org/docs/guides/peer-auth/#updates)'s. |
| Update now | `POST /console/settings/updates/apply` | Installs the newest release at once, after a confirm: it skips the soak, the wait for idle agents, skipped releases and the backoff, and lifts a hold, whether automatic updates are on, off or paused. Refused while checks are off, on an install that does not update itself, while an update is under way, within a minute of a failed release check, when nothing newer is known, or when the release carries no signed checksums bundle. The daemon restarts to finish. |
| Resume automatic updates | `POST /console/settings/updates/resume` | Lifts the pause automatic updates put on themselves after rollbacks, and the hold a deliberate downgrade set. Changes no setting; skipped releases stay skipped, and a running backoff runs out. |
| Save the backup schedule and folder | `POST /console/settings/backups` | Stores the console's `backup:` override: only the values that differ from the settings in force. A new folder is created and tested with a write first. Applies from the next backup. |
| Reset the backup settings | `POST /console/settings/backups/reset` | Clears that override, handing the schedule and the folder back to the config file (or, without one, the build's default). |
| Back up now | `POST /console/settings/backups/now` | Writes a backup at once, marked **Made by you**: never skipped, never deleted automatically. With `run=retry` (the failing card's **Try again**) it runs the schedule's backup instead, which is skipped when nothing changed and prunes after itself. Refused while a backup runs or a restore is being prepared. |
| Delete a backup | `POST /console/settings/backups/delete` | `ref=` a backup from the list, never a path. Removes the file from its folder, after a confirm. |
| Begin a restore's passkey ceremony | `POST /console/settings/backups/restore/begin` | Only while [peer_auth](https://thereisnospoon.org/docs/guides/peer-auth/#restore) locks a project, and only from a signed-in browser. Returns a WebAuthn challenge bound to the backup file's SHA-256, which the server reads from the file itself, and to the mode. Writes nothing. |
| Restore a backup | `POST /console/settings/backups/restore` | `ref=` and `mode=replace` or `merge`. Verifies the backup and prepares the restore while Seamless serves, then restarts to finish it. A replace saves today's data as a backup first. Refused while a backup, another restore or an update is under way. Under the passkey gate it also needs the ceremony's `id` and `response`, and the file's digest must still match. |
| Cancel a staged restore | `POST /console/settings/backups/restore/cancel` | Drops a restore that is waiting for its restart. Nothing changes. |
| Dismiss a restore's result | `POST /console/settings/backups/restore/dismiss` | Hides the last restore's result card. Changes no data. |
| Save a project family | `POST /console/settings/families/save` | Creates a family or replaces one family's name and member set - the same `project_families` setting `seamlessd family` manages. Members come from a closed picker of registered projects, so a typo cannot create an inert member. |
| Delete a project family | `POST /console/settings/families/delete` | Removes the whole family. Its projects lose the sibling-findings channel; nothing else about them changes. |
| Sign in / sign out | `POST /console/login`, `POST /console/logout` | Sets or clears the console cookie. Touches no data. |

**Your decisions never come through a paired device.** On a server that serves
other computers, a request that comes with a paired device's credential - a
relay's certificate or a device key - is refused (403) on every write that is
yours to make: a pairing code and a device's revocation, Held's release, discard,
session marks, lockdown and corpus marks, the optional features, every Backups
route, bringing a project's fence down, a deny, a revoke, Lock again, a passkey's
enrollment, and an approval with no passkey or Touch ID proof. That holds with
peer_auth on or off. A paired computer carries its agents, never your hands: make
these decisions in the console of the server you sign in to, or in a terminal on
the server itself. The reads stay open to a device, and so do the writes an agent
may make: `seam` on a client computer reads the console's JSON through its relay,
and the Touch ID helper there approves through it with its own signature.

Read the shape of that list. There is no "create memory", no "edit note", no
"delete memory", no "add task", no "start session". The direct writes to knowledge
state are **archive a memory**, **approve a captured plan**, the **star**
flag, and - with write screening on - **releasing a held write**, which lands
exactly what an agent wrote, never text of the console's own; the rest either
manage gardener proposals - which are themselves
proposals, reviewed before they do anything - or free a task claim, or set a
configuration knob (briefing overrides, feature switches, project families, the
update switches, the backup schedule and folder) that shapes future briefings,
what is exposed, or how the install runs, without touching any memory's
content. **Back up now** and **Delete a backup** make and remove copies of the
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
[the gardener's](https://thereisnospoon.org/docs/concepts/gardener/) propose-only contract. The store is written
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
  `/console`.
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

### Sign in with a passkey {#sign-in-with-a-passkey}

Set `console.sign_in: passkey` in the daemon's config (off by default; file
only) and restart it, and a browser signs in with a passkey you added, in any
browser, the server's included. No browser needs the key: the login page no
longer takes it, and a cookie derived from it signs nothing in. The `seam` CLI
keeps sending the key.

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
  name and key and asks before it adds. Approve only a code your own device
  shows.
- **No passkey at hand.** Under **Sign in from a terminal on the server**, the
  login page's **Show a code** begins a terminal sign-in: the browser gets a
  cookie bound to its key and shows a code with the command that approves it,
  `sudo -k -u _seamless env SEAMLESS_CONFIG=/opt/seamless/etc/seamless.yaml /opt/seamless/bin/seamlessd console-open --sign-in --code 7KQ2-M9XD`
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

The sidebar groups the screens by the job they serve rather than by table:

| Group | Screens |
|---|---|
| **Live** | Overview, Now, Interactions |
| **Knowledge** | Memories, Notes, Retrieval, Gardener |
| **Work** | Projects, Plans, Tasks, Sessions |
| **Research** | Labs, Trials (only while that [optional feature](#optional-features) is on) |

That is the full list, at the Advanced [level](#choose-how-much-you-see); a
lower level shows a subset, in the same order and under the same names.
Settings closes the list. Above the groups, a **Search or jump to** field opens
the command palette. The sidebar collapses to an icon rail (the header button, or
`[`) and remembers that per browser; under 720px it becomes a drawer behind the
menu button, with every section's label and count.

Every screen title carries an (i) button that says, in one line, what the screen
is for.

### The command palette

`Cmd/Ctrl+K`, the sidebar's search field, or `/` on a screen with no filter of
its own opens the palette. With nothing typed it offers **Recent** (the entities
you last opened in this browser), **Jump to** (every section the sidebar offers,
with its shortcut, plus Search, Context, and each Settings section the level
shows), and **Actions** (switch theme, collapse the sidebar, the shortcut sheet,
change the experience level). Typing matches sections and actions instantly;
from two characters on it also searches memories, notes, tasks, plans, trials,
projects, and sessions through the same route as [Search](#search), showing the
groups the level offers.

### Keyboard

| Keys | Does |
|---|---|
| `Cmd/Ctrl+K` | Search or jump to a section |
| `/` | Focus this screen's filter; open the palette where there is none |
| `g` then a key | Go to a section: `o` Overview, `n` Now, `i` Interactions, `m` Memories, `e` Notes, `r` Retrieval, `g` Gardener, `w` Projects, `p` Plans, `t` Tasks, `s` Sessions, `l` Labs, `x` Trials, `,` Settings |
| `j` / `k` | Next / previous item in a library rail |
| `[` | Collapse or expand the sidebar |
| `?` | The shortcut sheet |
| `Esc` | Close the palette, a menu, the sheet, or the drawer |

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

All of it is decoration over data the page already states: nothing here changes
a number, and `prefers-reduced-motion` stills every part of it (the Seam sends no
sparks, numbers render at their value). Between events the sky and the sidebar
hold still, so a console left open on the Overview draws nothing while the
fleet is idle.

## Choose how much you see

The console has three **experience levels**. They change what the console shows
you and nothing else: your agents get the same briefing, the same MCP tools, the
same hooks, the same recall, and the same gardener at every level. The level is
presentation, stored for this installation's owner rather than per browser, so
your phone and your laptop agree.

**Basic** is the essentials, for someone who installed Seamless because they use
Claude Code or Codex and wants to know it works: what the agents remember
(Memories, Notes), what they did (Sessions), cleanup suggestions to accept or
decline (Gardener), and a Home that opens on a health strip - which agent clients
are working here, whether semantic recall is on, when the last briefing went
out, and the version (which says when a newer release is out) - plus the
Updates and Backups settings.

**Standard** adds following the work: Now, Projects, Plans, and Tasks (and Labs
and Trials when that feature is on), the Overview's window, vitals, and
workspaces, the Briefing and Workspaces settings, and the finer controls on the
screens Basic already had.

**Advanced** is every screen and every knob: Interactions (the tool-call
transport), Retrieval (the analytics), Context, the Knowledge engine settings,
and raw event payloads.

A fresh installation starts at **Basic**, and Home carries a one-time welcome
card offering all three - pick one, or dismiss it; either way it does not come
back, on any device. Beside the levels the card offers one optional feature as
an unchecked box, **Also turn on write screening**: it rides with whichever
button you press, and unticked it changes nothing at any level. It is a
feature, not part of a level, because it changes what your agents receive (see
[Held](#held)); once the card is gone, Settings > Features is the way in. The sidebar's account row names the level you are on and
links to **Settings > Experience**, where the three levels sit side by side with
the list of what each one shows. The palette's **Change experience level** action
goes there too.

**Hidden, not locked.** A level removes screens from the sidebar, the palette,
and the shortcut map; it never locks them. A link to a screen above your level -
from these docs, an agent's finding, a briefing, a bookmark - still opens it,
under a note saying it is not in your sidebar, with a button to switch. JSON
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
| **Memories** | shown | shown | shown |
| **Notes** | shown | shown | shown |
| **Retrieval** | - | - | shown |
| **Gardener** | shown | shown | shown |
| **Held** - while the screening feature is on | shown | shown | shown |
| **Projects** | - | shown | shown |
| **Plans** | - | shown | shown |
| **Tasks** | - | shown | shown |
| **Sessions** | shown | shown | shown |
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
| **Briefing** - What every new agent session starts with | - | shown | shown |
| **Workspaces** - Projects, repo routes, and families | - | shown | shown |
| **Knowledge engine** - Semantic index, ranking, storage, and policy | - | - | shown |

### Within screens

| | Basic | Standard | Advanced |
|---|---|---|---|
| **Overview: the observation window** | - | shown | shown |
| **Overview: the vitals** | - | shown | shown |
| **Overview: the workspaces table and the knowledge rail** | - | shown | shown |
| **Memories: the Reach and Utility sorts** | - | shown | shown |
| **Memories: how often each memory surfaced** | - | shown | shown |
| **Memories: the utility score in the reader** | - | shown | shown |
| **Gardener: the Type and Source filters** | - | shown | shown |
| **Gardener: the project-split example** | - | shown | shown |
| **Gardener: the Hidden forever list** | - | shown | shown |
| **Gardener: Retarget on a proposal** | - | shown | shown |
| **Gardener: Hide forever on a proposal** | - | shown | shown |
| **Sessions: the Retained filter** | - | shown | shown |
| **Sessions: the Sort menu** | - | shown | shown |
| **Sessions: a session's review signals** | - | shown | shown |
| **Search: the Updated window** | - | shown | shown |
| **Search: the Sort control** | - | shown | shown |
| **Settings › Features: the precedence line** | - | shown | shown |
| **Settings › Features: the agent tool names** | - | shown | shown |
| **Settings › Workspaces: each repo route's host** | - | - | shown |
| **Settings › Workspaces: the unbound repo routes** | - | - | shown |
| **Event pages: the decoded payload fields** | - | - | shown |
| **Event pages: the raw payload** | - | - | shown |

## Three ways to render a page

Every route answers in the shape the caller asked for:

- **HTML** by default - the full page, layout and all.
- **JSON** when the caller sets `?format=json` or an `Accept` header that wants
  JSON and not HTML. This is how `seam` reads the console's data.
- **An HTML fragment** for entity details when the caller passes `?peek=1` - the
  detail pane loads it without a page navigation - or `?reader=1`, the richer
  reader fragment the library screens (memories, notes, tasks, plans, labs,
  trials) swap in place.

The event page composes its full page from the same `detail-body` block its
peek fragment renders, so the two cannot drift. Session and project
deliberately do not: their fragments are compact summaries of much richer
bespoke pages. Memory, note, task, plan, lab, and trial detail URLs render
their library screen with that entity open in the reader.

Strictly-validated query params (`?sort`, `?scope`, `?tab`, `?w`) return a 400
naming the bad param and listing the valid values, rather than silently falling
back to a default - so an agent driving the console by URL sees the fix.

`GET /console/events` is the SSE stream: every recorded event as one JSON `data:`
frame, with a ping every 25 seconds. A `retrieval.injected` frame also lists the
memories it surfaced as `itemIds` (capped at 48), which is what lets a page point
at exactly what reached an agent. `?feed=interactions` opts into the richer
transport-level rows the Interactions screen consumes.

## Overview

`/console/`

The landing page and the health check. It carries:

- **Counts** - active memories (broken down by kind), notes, sessions, and tasks
  by status.
- **Retrieval health** over a selectable window (`?w=24h|7d|30d|all`): injection
  volume, a trend chart, the **reach rate** (distinct active memories that
  actually surfaced, over all active memories), sessions reached, and the
  most-injected memories.
- **Coverage** - the share of in-window sessions that retained anything, with a
  per-channel breakdown (findings, memories, notes, trials) and a windowed trend.
  The channels overlap, so the shares need not sum to 100%.
- **Projects at a glance** - the top projects by recent activity, drawn from the
  same batched query the Projects board uses, so a row here reconciles with a row
  there exactly.
- **Agent-reported mishaps** - recent incidents agents explicitly supplied to
  `session_end`, attributed through the reporting session's harness and model.
  Warning tones appear only when reports exist; an empty rail is a positive
  "No mishaps reported" state.
- **Knowledge sky** - a star chart of every active memory, with no cap. Each
  project scope (global included) owns a wedge sized by its share of memories,
  largest first. Distance from the centre is when the memory last surfaced to an
  agent: the core holds the last 24 hours, rings mark 7 and 45 days (the same
  fresh and stale lines the memory reader and the "going stale" card use), and
  the rim belt holds memories that have never surfaced. Colour is kind; size is
  demand (the query-gated utility score, so a big star is one agents pull, not
  one the briefing merely shows). Hollow stars have never surfaced, a ring marks
  a starred memory, spikes mark one written today, and today's stars twinkle as
  the chart appears.
  Beside the chart, a readout states the same picture as text: each ring's
  count, the going-stale count (always equal to the attention card's), and every
  scope with its ring mix as a bar. Point at a star for its name and description
  (the pointer snaps to the nearest star); click it for a card with its facts and
  a link to open it; double-click to open it directly. Type in **Find a memory**
  to light only the matches (name, description, tag, scope, or kind) and list
  them, with the arrow keys and Enter to step through; click a ring, the going-
  stale line, a scope, or a kind chip to filter the same way, and point at one
  to preview it. When an injection or a read lands on the live stream, each
  memory it names sends a beam into the core, and the refresh that follows
  glides the star to its new ring; a newly written memory appears with a flare.
  A star keeps its place while its scope and ring are unchanged, and the rings
  and wedges resize in steps rather than with every memory, so a refresh moves
  only what changed, and search, filters, and the selection survive it. The
  refresh downloads only what changed, too: the stars that changed, arrived, or
  left since the chart the page holds, and nothing of the sky when nothing in
  it did. It is HTML only - the JSON answer carries no sky.
- **Recent activity** - the last twelve events, each linking to its detail page.

The four judged vitals at the top (memory reach, knowledge continuity, context
injections, sessions reached) are drill-down links carrying the selected
window: reach, injections, and sessions reached land on the Retrieval screen
whose hero and delivery funnel are the same numbers over the same report, and
continuity lands on the Sessions list filtered to `?retained=no` - the
sessions that kept nothing. An empty-state card stays linked, because the
destination explains why there is nothing.

Live sessions are counted TTL-aware (active *and* heartbeated within the idle
threshold), so the headline matches the Sessions screen rather than the raw
`active` count that an idle session inflates until the reaper runs.

### Since you were last here

When you come back to the Overview after at least five minutes away, a line
above everything else answers the check-in question -- what changed while I was
gone? -- with linked counts: memories written, sessions started, tasks closed,
notes written, gardener proposals raised, and mishaps reported since you last
looked ("Quiet since you were last here 3h ago" when nothing was recorded).

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
  by scope.
- **A scope strip** - one chip per project with a live agent (`?scope=<slug>`;
  the empty project scope filters as `global`). Filtering narrows every zone
  except the pulse.
- **On duty** - one card per live session: harness+model pill, project, last
  heartbeat (cards are toned hot/warm/quiet by heartbeat age), session wall
  clock, cumulative tokens, a star toggle, every claim it holds with a live
  lease countdown, and a short trail of what it just produced. A live agent
  holding nothing says so: "no claim held - roaming".
- **Loose ends** - `in_progress` tasks no live agent is carrying: a lapsed
  lease, a claimless start (`tasks_update status=in_progress` without a
  claim), or a holder that went quiet mid-lease. A lapsed claim offers
  **release claim** with no confirmation - the lease is already dead, so there
  is no live holder to interrupt, only a stale lock to clear.
- **Plans in motion** - a horizontally scrolling rail of every incomplete plan
  across every project, done/in-flight progress bars and ready counts, cards
  dimmed once a plan has rested for 24h. Each links to its project's Plans &
  tasks tab.
- **Up next** - the cross-project ready queue: claimable this instant, plan
  steps included, each naming what closing it would unblock.
- **The wire** - the freshest business events, scope-filtered.

Everything links onward - sessions, tasks, plans, projects, events - and the
lease countdowns tick client-side between refreshes. With the
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
paginates only inside that chosen window. Filters operate over the rows already
in memory, by event category and session lane. Pausing buffers new arrivals
rather than discarding them.

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

## Projects

`/console/projects`

**Locks.** Under [`peer_auth`](https://thereisnospoon.org/docs/guides/peer-auth/#two-settings-isolation-and-the-lock),
a confidential or sealed project's Isolation card offers **Lock this project**,
which opens `/console/lock/<project>`. Locking it, and removing a lock set here,
each need a passkey already enrolled on the Approvers page; without one the card
links there. Seamless restarts to apply the change. A project listed under
`peer_auth.force_locked` shows as **Force locked** and cannot be changed here. A
locked project's isolation cannot be set to `open`.

The board: one row per project, with live sessions, total sessions, open and
blocked tasks, memory count, inherited memories, reach rate, and last activity.
Grouped by family (`?group=family|flat`) and sortable (`?sort=recent|coverage|name`).
The global (`""`) scope appears as a row but is not a project and has no detail
link.

Selecting a project opens the **project workspace** - a seven-tab page over that
project alone:

| Tab | Shows |
|---|---|
| Overview | The project's metrics, memory kinds, injection trend, recent events. |
| Plans & tasks | Per-plan step timelines with each step's status, claiming session, lease countdown, and blocking dependency; plus the ready queue. |
| Sessions | The project's sessions, with the tasks each currently holds. |
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

A retired project still renders, with its banner - kept for provenance. Only an
unknown slug is a 404.

## Sessions

`/console/sessions`, `/console/sessions/{id}`

The list separates **active** (live: active and heartbeated within the idle TTL)
from **idle** (active but gone quiet past it, awaiting the reaper) from
**completed** and **expired**. Filterable by status, searchable, windowed, and
filterable by retention (`?retained=yes|no`): whether the session left a
durable artifact behind - non-empty findings, or a written memory, note, or
recorded trial, the same covered-ness test the coverage numbers apply. The
Overview's continuity vital links straight to `?retained=no`, so its click
answers "which sessions dropped knowledge".

The list defaults to the last 24 hours. When that leaves most sessions out, the
list ends by saying so ("3 of 212 sessions were active in the last 24h") and
offers the wider windows in place, so a quiet day never reads as an empty
system. A row names its host only when the session ran on a different machine
from the console's, and its source only when it was not a normal startup.

A session's page is the workspace: its findings (rendered), its full event
timeline as interaction rows, per-session counts (tool calls, memory reads and
writes, items injected, and read-after-inject), the tasks it currently claims with
their lease countdowns, and the memories it produced.

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

## Memories

`/console/memories`, `/console/memories/{id}`

A two-pane library: a rail of memories grouped by project (global first, kinds
in canonical order, each dot colored by kind) beside a full-height reader.
Sortable by name, recency, reach, utility, or starred; filterable by a substring
of name, description, kind, or tag. Inactive memories collapse into an
archived-and-superseded group at the rail's end, each carrying its status and,
when superseded, what replaced it.

The reader renders the body uncapped (through the markdown layer, with raw HTML
disabled and a sanitizer on the output), the metadata - kind, project, tags,
timestamps, the session that produced it - its reach counts, its
[utility score](https://thereisnospoon.org/docs/concepts/recall/#the-utility-nudge) with the per-signal
demand breakdown behind it, the `vscode://` link straight to the file, and its
supersession neighbors in **both** directions: what replaced it, and what it
replaced. The actions here are
**star** - the flag that pins it into briefings and boosts recall - and
**archive**.

Opening `/console/memories` auto-opens the most recently updated match; a
memory's own URL opens the same screen with it selected. Clicking rail items
swaps the reader in place (real URLs, browser Back works), and `j` / `k` step
through the rail.

## Notes

`/console/notes`, `/console/notes/{id}`

The same library shape for notes: a project-grouped rail (global `""` first),
sortable by recency, title, or starred, filterable by title, description, or
tag. The
reader renders the note as a document - uncapped body in a measured reading
column, description, tags, word count, source URL, and the file path with an
editor link.

## Tasks

`/console/tasks`, `/console/tasks/{id}`

The same library shape, with the rail grouped into four buckets: **ready** (no
unfinished blocker), **in progress**, **blocked**, and **closed** (done and
dropped merged, newest first, capped at 25 with a count of the rest, collapsed
by default). The reader carries the task's body, claim and lease state, and
both dependency directions; **force-release** is the action, and it is the
owner override - it takes the lock from whoever holds it.

Under a lock, a task blocked by a task in a locked project you
haven't unlocked sits in the blocked rail, its blocker named only by project
on a line of its own; the task's reader and peek list it under **Depends on**
the same way. The project workspace's Plans & tasks tab marks such steps
blocked and leaves them out of the ready queue. Opening a plan or a session
that lives in a locked project takes you to that project's unlock page, which
says the item is there and links back once you unlock it.

## Plans

`/console/plans`, `/console/plans/{slug}`

The same library shape, with the rail grouped by phase (**in progress**,
**ready**, **done**) and scoped by the window selector in the rail's tools
(24h by default). Both kinds of plan share the rail:

- **captures** - Claude Code plan-mode captures (`cc-plan` notes), with their
  lifecycle status, iteration count, and cached subagent runs.
- **composed** - plain [plans-as-composition](https://thereisnospoon.org/docs/concepts/tasks-and-plans/) plans (a
  note tagged `plan:<slug>` plus its tasks), which have none of the capture-only
  fields.

When the window hides plans, the rail ends with how many last moved before it
and the wider windows to switch to.

A capture owns its slug; composed plans fill only the rest. The reader shows
the rendered plan body, the step tasks, and the notes attached to the
composition (supporting notes and agent caches). **Approve** appears here, for
captures only.

Each plan also carries a **model tokens** rollup - the cumulative transcript
tokens of every session attributed to the plan (any session that moved a step,
or captured the plan) - compact in the rail (`~483k tok`) and qualified in the
reader (`~483k model tokens · 3 sessions (1 unreported)`). Attribution is
whole-session on purpose: tokens are only ever known per session, so a
session's full burn counts toward each plan it touched, counted once however
many steps it moved. Claude Code reports tokens at session end, so a live
session stays *unreported* until it finishes; a session that touched more than
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
`?outcome=`. Outcomes are
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
families (findings and the opt-in memory channel), unregistered-scope warnings,
and retired-project split lineage reconstructed from the memory-move event log.

`?scope=all` renders every known project scope, with global memory shown as the
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
3. **Scope coverage** - reach per project scope (global first), each row with
   its own reach rate and injection count.
4. **Knowledge pressure** - the most-injected memories against **quiet
   knowledge**: active memories not updated, injected, or read in 90 days,
   mirroring the gardener's default staleness horizon. Unlike everything else
   on the page, the stale list is all-time, not windowed.
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
   archive proposals.

Reachable from the Overview's retrieval-health card.

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

Split batches are grouped by plan and reviewed together, setup card first, with an
apply-the-whole-plan action.

The actions are **apply**, **dismiss**, **hide forever**, **retarget**
(reproject cards only), and **apply plan**. Dismissing answers the evidence in
front of you - the pattern comes back if it recurs; hiding answers the pattern
itself. Everything you decide lands in **Recently decided** with an Undo, and a
hide is additionally listed under **Hidden forever**, where **Unhide** lifts the
block without returning the proposal to the queue. Above them sits a single
ask-in-words box, and it only ever
produces more proposals for this same queue. A request recognized as a project
split is planned as a split directly - the plan batch appears below like any
other. When the split's source project cannot be matched, an inline follow-up
asks you to pick the project and plans the split from there; nothing is retyped.

See [The gardener](https://thereisnospoon.org/docs/concepts/gardener/) for what each proposal type means.

## Held {#held}

`/console/held` - while [write screening](https://thereisnospoon.org/docs/guides/write-screening/) is on, at
every level (whoever turns it on must reach what it holds).

The owner's queue of agent writes kept out of the corpus. Each card says what
the write would do (a constraint memory, a note, a delete, a gardener apply),
which project, tool and session it came from, and why it was held - each
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
  by name and counts only.
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
  code.
- **Waiting for your approval** - under peer_auth only: each computer that used
  a code, its fingerprint and until when it waits, with **Approve with a
  passkey** and **Deny**. Approving needs a signed-in browser on the
  [passkey origin](https://thereisnospoon.org/docs/guides/peer-auth/#the-passkey-origin) and an enrolled
  passkey; the page says which is missing.
- **Paired** - each relay and key device: its key's fingerprint or the start
  of its key, when it paired, whether your passkey or its one-time code
  approved it, when it was last seen, and **Revoke**.
- **Revoked and expired** - what ended, when, and from where.

It shows at every [level](#choose-how-much-you-see), in the sidebar's Server
group; `g` then `d` opens it.

## Settings

`/console/settings?s=<section>`

Settings is one section at a time, each shaped by what you came to do rather
than by subsystem. A sub-nav on the left (a row of chips on a phone) switches
sections in place, without reloading the page. `/console/settings` alone opens
**Experience**; an unknown `?s=` is a 400 that names the valid sections. Which
sections a level offers is in the [matrix above](#choose-how-much-you-see); a
section above your level still opens from a link, under the same note a hidden
screen gets. Old `/console/settings#...` bookmarks land on the matching section.

The editable sections share two habits. A one-line note at the top says where
the values come from - **Following file + env**, or an override in force, with
**Reset to file + env** beside it. And a save bar appears at the bottom once a
form has unsaved changes, with **Save** and **Discard** (which puts the form back
exactly as the page loaded it); without JavaScript the bar is simply always
there. `GET /console/settings?format=json` returns the whole payload whatever
`?s=` says, plus `consoleLevel`, `consoleLevelOverridden`, and
`consoleLevelSource`.

### Experience

`/console/settings?s=experience`

The three [levels](#choose-how-much-you-see) side by side - Basic, Standard,
Advanced - the current one marked, each with a one-line pitch and a list of what
it shows, generated from the same registries the sidebar and the page gates
read, so the list cannot promise something the level does not do. Choosing one
applies at once: the sidebar and the page update in place, and if the new level
hides the screen you were on you land on Home with a note. The note at the top
says whether the level was **chosen in the console** or **set by the upgrade**,
and **Reset to file + env** hands the choice back to `console.level`. The same
section holds the theme and a button for the shortcut sheet. The theme is
**System**, **Light**, or **Dark**, kept per browser. System is what a browser
starts on: it follows your computer's light or dark appearance, and follows it
live when it changes. Light and Dark pin one theme. The sidebar's sun and moon
switch the same setting: under System they pin the theme opposite the one on
screen, and picking System here goes back to following the computer.

### Optional features

`/console/settings?s=features`

Optional features are the parts of Seamless you can switch on and off, and they
ship **off**: a fresh install exposes none of them until you turn one on. The
section renders one card per feature - a toggle, an Enabled/Disabled pill, what the
feature is, a generated line naming exactly what switching it off hides, and a
live count of the data it holds either way ("Data kept: 12 trials across 3
labs").

There are four today:

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
- **Witnessed unlocks** on Settings: the utility-activation table's armed
  note upgrades to the date each stage unlocked, so a latch that used to flip
  silently is witnessed.

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
lines carry owner-confirmed or agent-written marks. Its card's data line counts
the writes waiting; turning it off stops screening new writes and releases
none of them. With restricted local accounts configured, the card says the
Held screen stays for their proposals and only its screening parts go. The
[write screening guide](https://thereisnospoon.org/docs/guides/write-screening/) has the screens, the reasons
and the events.

**Nothing is ever deleted.** Switching a feature off gates exposure and nothing
else: the trials stay in the database, its screens answer with a short "switched
off" page that links back here (a JSON caller gets a 403), and switching it back
on restores every surface exactly as it was. That is what the data-kept line on
each card is for - it reports the feature's own rows whether it is on or off.

**When the change lands** has four different answers:

| Surface | When |
|---|---|
| The console | Immediately - nav entries, screens, the search scope, and the overview tiles appear or disappear on the next render. |
| A tool call | Immediately - a disabled feature's tool is refused as an unknown tool, whatever list the caller is holding. |
| The agent briefing | The next session start - momentum's finish-line emphasis reads the same stored override the console gates on. |
| A client's tool list | The next time it lists tools, in practice its next session. Seamless declares `listChanged: false` and sends no tool-list notification, so a connected client keeps the list it already has. |
| The client-side skill | The next `seamlessd install-hooks` run. The daemon does not reach into `~/.claude/skills` on a toggle, so `seamlessd doctor` raises an **info** line while a skill for a disabled feature is still sitting in a client's skill home. |

Saving writes a **stored override** row in the database - the same layer the
briefing form uses. It wins over file and env, never touches your config file,
and holds until **Reset to file + env** clears it; reset means back to the
file/env configuration, which unless you set `features:` there is off.

One override can be in force without you having set it. Upgrading an
installation that already holds trial data seeds the override with research on,
so a feature that now ships off does not disappear from under data you were
already using. That is why the section states that a stored override is in force
rather than crediting you with the choice - and reset clears it like any other.
At Basic the section keeps the cards, what each hides, and the data-kept line,
and leaves out the agent tool names and the precedence line.

### Your setup

`/console/settings?s=setup`

Read-only facts in plain words: the version, the machine the daemon runs on, the
config file (with a link that opens it in your editor, or a note that the daemon
runs on defaults and environment variables), the data folder, the database file
and its size, which agent clients have recorded sessions and when each was last
active, whether semantic recall is on - and when it is off, what to do about it -
whether the gardener is running and how often, how often backups run, how many
are kept and when the last one was made, and links to these docs. No
budgets or policy numbers: those live in Knowledge engine.

### Updates

`/console/settings?s=updates`

The [automatic update check](https://thereisnospoon.org/docs/updating/#automatic-update-checks) and
[automatic updates](https://thereisnospoon.org/docs/updating/#automatic-updates) in plain words: the **Mode** -
whether this install checks for new releases, and whether it installs them by
itself or only tells you, and why (a release build checks by default, a build
from source does not, and only an install the installer made, or a peer_auth
system daemon in the guide's layout, updates itself) -
what a check sends (one anonymous request to `api.github.com` every few hours;
nothing when checks are off), the installed version and kind of build, the
command that updates this install (with a copy button, labeled **To update by
hand** on an install that updates itself), the newest release with its date and
release notes - or "up to date" - and when the daemon last checked and checks
next, including a streak of failed checks and why.

The note at the top says who decided the switches: **Default for this build**,
**Set in the config file or environment**, **Set in this console**, or
**Automatic updates set in this console**, with **Reset to the config file**
beside a console choice. Turning checks off from here always works. Turning them
on does not when the config file or `SEAMLESS_UPDATE_CHECK` says `false`: that
is final, the switch shows locked, and the form refuses - `false` promises no
update traffic at all. **Check now** asks GitHub once, right away (not more than
once a minute).

On an install that updates itself, the section adds **Automatic updates** - on
or off, who decided, and **Turn automatic updates off** / **on**, which
`update.auto: false` in the config file or environment locks off - and, while
they apply, **Next update** (the release it installs next and what it waits
for: the soak, live agent sessions, or a lull after its deadline), **Paused**,
**Held back**, **Skipped releases** and **Next try**. **Updating now** and
**Last attempt** appear whenever there is one; **Last attempt** carries the
outcome, the updater's own error and its log path - the one place in the
console either appears - and, when the updater refused to start, the fix.
**Update to v0.7.3 now** installs the newest release at once after a confirm -
skipping the soak, the idle wait, skipped releases and the backoff - and the
daemon restarts to finish. **Resume automatic updates** appears while they are
paused or holding releases back.

With [`update.release_signers`](https://thereisnospoon.org/docs/updating/#owner-signed-releases) set in the
config file or environment, an **Owner-signed releases** row names the file
the updater holds every release to, and the last release it refused and why:
it carried no owner's signature, was signed by a key the file does not list,
or its signature did not verify. Without the key the row is not there. The
console can neither set nor clear it.

When a newer release is out, the Home health strip's version fact turns into
`v0.7.2 -- v0.7.3 available` and links here - its hover says whether it installs
by itself, and when - and a toast announces it if the console is open when the
daemon finds it. While an update runs, the fact reads `v0.7.2 -- updating to
v0.7.3`, and once v0.7.3 is serving and the updater is only confirming it,
`v0.7.3 -- confirming the update` (**Updating now** says **confirming** too).
The daemon re-reads the updater's record every few seconds while an update
runs, so both clear soon after it is confirmed. For a day after the daemon
starts on a new version, every page carries
a banner - "Seamless updated to v0.7.3 (from v0.7.2)" - with the release notes
and **Got it**, which hides it for that tab. It also says when Codex's hooks
changed with the update (re-approve them in Codex's `/hooks`) or the installer
reported warnings (run `seamlessd install-hooks`). For a week after automatic
updates pause themselves, or after an update the daemon started does not apply,
every page also carries a warning banner pointing here.

### Backups

`/console/settings?s=backups`

Scheduled backups of the whole instance into one folder you choose, the
backups found there, and restore. [Backups in the console](https://thereisnospoon.org/docs/guides/data/#backups-in-the-console)
walks through it; this is what each card holds.

**Automatic backups**, the status card:

- whether automatic backups are on, how often they run and how many are kept -
  or why they are off: turned off here, `backup.auto: false` in the config
  file, or a build from source, which backs up only when you turn it on;
- the newest backup: when, its size, and how many memories and notes in how
  many projects it holds;
- when the next backup runs, or **Up to date: nothing has changed since the
  last backup**, with when it checks next;
- a backup under way, while it runs;
- the automatic runs' failure streak - how many failed in a row, since when,
  and the last one's reason - with **Try again**, which runs the schedule's
  backup now;
- the folder, with a copy button, and **Back up now**, which makes a backup
  marked **Made by you** whatever the schedule says.

**Schedule and folder**, the form:

- **Back up automatically**. A run is skipped when nothing has changed since
  the last backup, or when the disk is short of room.
- **How often**: Every hour, Every 6 hours, Every day or Every week. An
  interval set in the config file (anything from `1h` to `720h`) shows among
  them as itself.
- **Automatic backups to keep**, 1 to 100. Older automatic backups are deleted
  after the next one. Backups you make yourself and safety backups are never
  deleted automatically.
- **Folder**, with a live check as you type (it only looks, never writes) and
  chips for the places on this machine this account can write to: iCloud
  Drive, cloud folders and external disks on macOS, Dropbox and external disks
  on Linux, OneDrive and other drives on Windows, and Seamless's data folder.

**Save** stores only what differs from the settings in force; a new folder is
created and tested with a write first. **Reset to the config file** makes the
settings follow the config file again. The bar at the top says where the values
come from: **Default for this build**, **Following the config file**, or **Set
in this console** with the fields you set.

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
- **Restore…** and **Delete**. An iCloud placeholder (download it in Finder
  first) and a backup a newer Seamless made (update to restore it) cannot be
  restored and say why.

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

Then a progress card lists the steps: **Checking the backup**, **Preparing the
restored data** (replace) or **Saving your current data** (merge), **Restarting
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

- **Settings > Your setup** has a **Backups** row: how often, how many are
  kept, and the last backup.
- Every page carries an alert while automatic backups are in trouble, linking
  here. **Automatic backups are failing.** appears when two automatic runs in a
  row did not finish, with the first failure's time and the reason.
  **Automatic backups are overdue.** appears when none has finished for three
  intervals. **Got it** hides it for that tab until a new episode begins.
- [System](#system) draws backups as a core part, on, off, failing or
  overdue.

### Briefing

`/console/settings?s=briefing`

What every new agent session starts with. Three **presets** lead the section -
**Lean** (a short briefing: the binding rules, the freshest memories, little
else), **Balanced** (exactly the defaults), and **Rich** (more rules in full,
more recent work, and family memories) - each card showing the numbers that set
it apart. Picking one fills the form; nothing is saved until you save. Below
them, **Customize** holds every knob, grouped by what it shapes - the memory
index, recent work, the planning horizon, the project family, utility ranking,
and the safety ceiling - with each knob's "0 means" note under it. Customize is
open by default at Advanced. The section marks the preset your saved values
equal, or **Custom** when they match none, and re-marks it as you edit.

Saving writes a runtime override row in the DB. It layers over the file/env
values and wins until reset, and it applies from the next session start - no
daemon restart. It never touches your config file, so `seamless.yaml` stays the
thing you wrote. The token budget (`budgets.max_briefing_tokens`) is not a
briefing knob and no preset moves it. The form validates: a non-numeric knob or a
value that fails `Briefing.Validate()` comes back as an error flash, not a
silently-dropped save.

The **preview** shows the `<seam-briefing>` a new session in one project would
start with, under the values the form holds right now, saved or not. It sits
between the presets and Customize, or beside the form when the section is wide
enough. Pick a preset or change a knob and it follows within a moment. It
also shows the token estimate against `budgets.max_briefing_tokens` and the hard
cap, and flags a briefing that runs over the budget (only constraints, pinned
stages, and starred rows can put it there) or would be cut at the cap. The project picker
lists the registered projects and opens on the one your agents worked in last.
A preview is a read: it saves nothing, records no event, and no agent receives
it, so it never counts toward retrieval stats or utility. Values the save would
refuse, it refuses with the same message. For scripts,
`GET /console/settings/briefing/preview?project=<slug>&format=json` previews the
saved values. `POST` to the same path, with the form's fields plus `project`,
previews unsaved ones. It is the one POST in the console that writes nothing,
which is why it is not in the table of write routes above.

The **utility ranking** group holds `utility_weight` (utility's share of the
briefing sort key; 0 restores pure recency) and `utility_mode` (`auto` arms each
project as its demand history matures, `on` everywhere now, `off` never). Where
each scope stands against the readiness gates is in Knowledge engine. See
[Sessions & briefings](https://thereisnospoon.org/docs/concepts/sessions/#the-budget-and-what-survives-it) for
how the blended order behaves once active.

### Workspaces

`/console/settings?s=workspaces`

How repositories resolve to project scopes, and which projects share context.
**Project families** are editable here: create, rename, edit, or delete the named
groupings that [`seamlessd family`](https://thereisnospoon.org/docs/reference/cli-seamlessd/#seamlessd_family)
manages from the CLI - the same `project_families` setting, so a change on either
surface shows up on the other. Members are chosen from a closed picker of
registered projects; the CLI is the route for pre-registering a slug that does
not exist yet. Below it, one bounded directory lists every scope with its repo
routes and family tags. At Advanced each route also names the machine it lives
on, and repo paths that resolve to no project are listed as unbound.

### Knowledge engine

`/console/settings?s=engine`

The machinery, in three groups.

**Recall** - the embedder card shows the active provider and model, and when
embeddings are off, the exact cause, with distinct copy for the owner off switch,
the no-key lexical fallback, and a config error. The off/auto switch is a
settings row read once at serve start, so the page flags a pending restart
whenever the stored switch disagrees with the running process. Beside it, the
stored-vector counts: totals, the not-yet-embedded backlog, and a per-model
table that badges models the running embedder no longer writes as stale -
**re-embed everything** rewrites the corpus with the active model in the
background.

**Ranking** - the **utility activation by scope** table lists every scope with
each readiness gate against its threshold (demand events, memories touched,
history age - met gates turn green) and, for scopes still building, spells out
exactly what remains before auto arms ("needs 7 more events, 3d more history").
A per-scope **force** overrides the latch in either direction; when the global
mode is `on` or `off`, the table says the per-scope state is dormant until auto
returns.

**Storage & policy** - read-only: the data folder, the context budgets, the
database (path, size on disk including the WAL, schema version), and the
gardener's cadence and policy. Change these through the config file or
environment, then restart.

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

## Errors

A bad or stale URL renders a styled, layout-wrapped error page with a way back,
rather than dropping you on a bare `404 page not found` -- including any
`/console/` path no route claims, which gets a search box and links to the
common destinations. A 404 names the missing
entity and a 400 names the bad parameter and its valid values. A 500 stays
generic in the browser - the detail is in the log, not the response. Fragment
fetches (`?peek=1`) get a fragment-shaped error, since a full page injected into
the detail pane would nest the whole console inside itself.
