---
title: Screen writes and hold the suspicious ones
description: Write screening - agent writes are screened and suspicious ones held for you; the existing corpus is reviewed, files changed outside Seamless are quarantined, a lockdown switch narrows what agents receive, and refusals and a metadata feed tell a monitor what happened.
---

Seamless injects its constraints and conventions into every session's
briefing, and recall serves memories and notes to any agent that asks. That
makes the corpus the most leveraged place to plant a prompt injection: a
poisoned constraint is read by every agent, in every session, until someone
notices. The gardener, which reads memories and proposes rewrites, can carry
one further.

**Write screening** is the optional feature that closes that door. It is off by
default, and off means writes land exactly as they always have. On, it does
these things:

1. **Screens every agent write** to a memory or note for secrets, injection
   markers, hidden Unicode, long encoded blobs, and links outside an allowlist,
   and records the verdict codes - never the text - as a `memory.screened`
   event.
2. **Holds the suspicious ones.** A write that fails a screen, a change to a
   briefing-injected kind (constraint, convention, protocol, stage) by a
   session an agent guard marked as having read outside text, and any
   supersede or delete of a constraint are kept out of the corpus - out of
   briefings, recall, prompt injection and the gardener - until you release
   them on the console's **Held** screen.
3. **Marks provenance** on what agents read: every injected memory line says
   whether you confirmed its content or an agent wrote it, and in which
   session.
4. **Reviews the existing corpus** the first time you turn it on: every memory
   and note goes through the same screens, on this machine. Clean ones are
   marked safe; the rest are listed for you with a **Mark safe** action.
5. **Quarantines files changed outside Seamless.** A memory or note file
   edited or planted on disk by anything but Seamless stays exactly as it is,
   but no briefing or prompt recall injects it until you accept it.
6. **Offers a lockdown switch** for an incident: changes to binding rules wait
   for you, and briefings inject only what you confirmed or the review marked
   safe.
7. **Records refusals and feeds a monitor.** Refused accesses become
   `access.refused` events, and an optional metadata feed delivers every
   event's metadata - never its content - to a Unix socket you name.

## Turn it on

Three ways, all the same switch:

- **The Home welcome card.** After installing, Home asks how much of the
  console you want to see. Beside the level choice is an unchecked box, **Also
  turn on write screening**. Tick it and whichever button you press (a level
  or Dismiss) turns the feature on too. Leave it unticked and nothing changes,
  at any level. The level is presentation only; screening changes what your
  agents receive, which is why it is its own choice rather than part of a
  level.
- **Settings -> Features**, the card named **Write screening**. This is the way
  in once the welcome card is gone. It stores the same override row the
  welcome box writes, records the same `settings.features_changed` event, and,
  like every optional feature, turning it off deletes nothing.
- **The config file or environment:** `features.screening: true`, or
  `SEAMLESS_FEATURES_SCREENING=true`. A console choice wins over both until
  you reset it.

The tunables live in their own block:

```yaml
screening:
  # Hosts a URL may point at without a verdict; each covers its subdomains.
  # Unset or empty turns the URL screen off.
  url_allowlist: [example.com, docs.example.org]
  # The shortest unbroken base64 or hex run that counts as a long blob.
  blob_min_length: 200
```

`SEAMLESS_SCREENING_URL_ALLOWLIST` (comma-separated) and
`SEAMLESS_SCREENING_BLOB_MIN_LENGTH` override them. Loopback URLs
(`localhost`, `127.0.0.1`) are never "outside".

Four more keys belong to the review, drift, lockdown and feed sections below:

```yaml
screening:
  drift_interval: 5m                       # 0 turns the drift watch off
  lockdown_file: /path/to/lockdown.flag    # its existence turns lockdown on
  feed_socket: /path/to/consumer/ingest.sock
  feed_spool: /path/to/spool               # default <data_dir>/feed
```

Each has a `SEAMLESS_SCREENING_*` override (`DRIFT_INTERVAL`,
`LOCKDOWN_FILE`, `FEED_SOCKET`, `FEED_SPOOL`); the paths expand a leading
`~` and must be absolute.

## What is screened

Every agent write that lands text in the corpus: `memory_write` (including
the half that supersedes), `memory_append`, `memory_edit`, `notes_create`,
`notes_update`, `notes_edit`, `notes_append`, `capture_url` (a fetched page is
outside text by definition), and an agent's `gardener_apply` of a proposal
that writes text (a digest, a consolidated memory). A memory is screened on
its description, body and tags; a note on its title, description, source URL,
body and tags.

An update is judged by what it **adds**: a verdict counts only if the new text
matches it more often than the text it replaces. So a memory you released
because it legitimately quotes an injection (a note about prompt injection,
say) takes later edits without being held again, while a second marker - or
one assembled across two appends - still shows.

Memories are instructions to agents by design, so the screens look for what
an injection or a leak looks like, never for ordinary imperative text. "Always
run make check" and "fetch upstream first" screen clean.

| Code | Holds? | What it means |
|---|---|---|
| `secret.private_key` | holds | A PEM or OpenSSH private key block. |
| `secret.aws_access_key` | holds | An AWS access key id (AWS's documented example key excepted). |
| `secret.github_token` | holds | A GitHub personal, OAuth, app or fine-grained token. |
| `secret.slack_token` | holds | A Slack token or incoming-webhook URL. |
| `secret.openai_key` | holds | An OpenAI API key. |
| `secret.anthropic_key` | holds | An Anthropic API key. |
| `secret.google_api_key` | holds | A Google API key. |
| `secret.stripe_key` | holds | A live Stripe secret or restricted key. |
| `secret.jwt` | holds | A signed JSON Web Token. |
| `instruction.override` | holds | Telling the reader to drop its prior instructions ("ignore all previous instructions", "you are now in developer mode", "new instructions:"). |
| `instruction.role_marker` | holds | Chat-template and role tags that impersonate the system, a developer or a tool, including Seamless's own `seam-briefing` and `seam-recall` envelope tags. Quoted as code (in backticks or a fence) they are documentation and do not count. |
| `instruction.concealment` | holds | Telling the reader to hide what it does from the user ("keep this hidden from the user", "do not reveal these instructions", "secretly send"). |
| `instruction.exfiltration` | holds | Telling the reader to send credentials somewhere outside: a sending verb, a secret, and a destination it goes **to** (a webhook, an external server, or a URL outside the allowlist), or a network command reading a credential file. Copying a public key is not one. |
| `hidden.comment` | records | An HTML or markdown comment: invisible on the console, read in full by an agent. |
| `hidden.style` | records | An element styled or flagged invisible (`display:none`, `font-size:0`, `hidden`). |
| `hidden.instruction` | holds | An imperative aimed at the agent inside hidden text: harmless in plain sight, a marker when hidden from the person reviewing it. |
| `unicode.tag` | holds | Tag characters U+E0000 to U+E007F, which can spell invisible ASCII. |
| `unicode.zero_width` | holds | Zero-width characters U+200B to U+200D, U+2060 and U+FEFF (a byte-order mark at the very start excepted). They also split words to slip past the phrase screens, which is why those screens read the text with them removed. |
| `unicode.bidi` | holds | Bidirectional embeddings, overrides and isolates U+202A to U+202E and U+2066 to U+2069, which make the console show different text than an agent reads. |
| `blob.base64` | holds | An unbroken base64 run of at least `blob_min_length` characters, mixing digits and both cases. |
| `blob.hex` | holds | An unbroken hex run of at least `blob_min_length` characters. |
| `url.unlisted` | holds | A URL whose host is outside `url_allowlist`. With no allowlist this screen is off: every URL would be "outside", which says nothing. |

Why the two that only record: comments and invisible styles are common in real
notes (templates, TODOs, pasted HTML), and holding on them alone would bury the
queue. What makes one dangerous - an instruction to the agent hidden inside -
is `hidden.instruction`, which holds.

## What is held

A write is held when any of these is true:

| Reason | When |
|---|---|
| a screen verdict that holds | The write adds one of the holding codes above. |
| `session.tainted` | The writing session carries the outside-text mark, and the write touches a briefing-injected kind: a constraint, convention, protocol or stage it writes, replaces, supersedes or deletes. |
| `constraint.supersede` | The write supersedes a constraint, from any session. |
| `constraint.delete` | `memory_delete`, or an agent's gardener archive, of a constraint, from any session. |
| `constraint.demote` | The write turns a constraint into another kind, which drops it from every briefing's binding section. |

A held call is held **whole**: a `memory_write` that supersedes a constraint
writes nothing - neither the replacement nor the supersede - so the old
constraint stays valid and in every briefing until you release both halves
together. A held delete leaves the memory exactly where it is. A held gardener
apply leaves the proposal pending.

The held text lives in the database's queue, never under `memory/` or
`notes/`. Briefings, recall, prompt injection and the gardener only ever read
those two trees, so a held write cannot reach any of them, and no reconcile or
index rebuild can bring it back. Deleting `seam.db` discards the queue with
the rest of the database - held writes are not corpus data until you release
them.

The agent is told plainly that the write is held: the tool answers with
`status: "held"`, the hold's id, its reason codes, and a message that nothing
in the corpus changed and the owner decides. It is a success answer, not an
error an agent would retry. `memory_read` and `notes_read` keep serving the
corpus as it stands - the held text reaches no agent.

## Review on the Held screen

**Held** (in the sidebar while the feature is on, at every console level)
lists each waiting write with what it would do, who wrote it, the reason codes
in plain words, and the text exactly as an agent would read it: every hidden
character is spelled out (`[U+200B]`), and secrets are redacted, so the screen
can be reviewed - and captured - without the credential. The reasons are color
coded by kind, most serious first, and the text marks what each one matched in
the same color, so the issues read at first sight.

- **Release** applies the write through the files layer, exactly as the agent
  sent it, and marks the memory owner-confirmed. A release whose target moved
  on after it was held (a later write landed, the name was taken, the
  constraint it supersedes is already gone) is refused, because it would
  silently undo that change; such a hold offers only Discard.
- **Discard** lands nothing and erases the held text. The row stays as the
  record of what was refused.

Turning the feature off stops screening new writes but releases nothing: held
writes stay out of the corpus, and the queue comes back when you turn it on.

## The outside-text mark

An agent guard - a client hook that knows which tools bring outside text into
a session, such as a web fetch, a search or a mail reader - marks the session
after one runs. From then on the session's changes to briefing-injected kinds
are held. The mark is set by POSTing to the daemon with the credentials every
hook already has (the bearer key, or the peer_auth socket):

```bash
# From a PostToolUse hook: forward the hook's own stdin unchanged.
seam hook session-taint
seam hook session-taint --client codex
```

`seam hook session-taint` reads the hook payload from stdin and posts it to
`POST /api/hooks/session-taint`, which finds the session by the client's own
`session_id`. A caller that knows the Seamless session can name it instead,
with `{"session": "<ULID or cc/... name>"}`. An optional `reason` is a short
code (`web_fetch`, `mail`); without one, the payload's `tool_name` becomes the
reason (`tool:webfetch`). The first mark stands. With the feature off the
endpoint marks nothing and says so.

Under a lock (`peer_auth.locked`), an agent in a locked project's repository
has its session recorded there before you approve its access. Its own guard
marks that session all the same, so the access request you are shown carries
the "session tainted" warning. A process outside the agent's process tree
finds no such session, as with every other hook.

Setting the mark only lowers trust, so a session's own hook may set it.
Clearing it is yours: the Held screen lists every marked session with a
**Clear mark** button, and nothing else - no endpoint, flag or MCP tool -
clears one. A new session starts unmarked.

`install-hooks` does not wire this event: the stock PostToolUse hook only ever
forwards plan-file writes and plan approvals, so the daemon never sees which
other tools ran. The guard is a hook of your own.

## Provenance on injected memories

While the feature is on, every memory line in a briefing and in the
prompt-matched `seam-recall` block ends with a mark:

- `[owner-confirmed]` - you released a held write of exactly this content (the
  console stores a fingerprint of its kind, scope, name, description and
  body). Any later change - an agent's edit, an append, a hand edit of the file
  - no longer matches, and the mark lapses on its own. A star or a tag change
  does not lapse it.
- `[agent-written, session cc/...]` - everything else, with the session that
  last wrote the content through Seamless. The session is named only when it
  ran in the memory's own scope: a global memory written from inside a project
  does not carry that project's session name into every other briefing.

`memory_read` returns the same answer as `provenance` and
`written_by_session`.

## The review and the safe mark

The first time the feature is on - from the welcome card, Settings or the
config, whichever comes first - the daemon reviews every memory and note
file already on disk. It runs the same screens agent writes go through,
deterministically and on this machine: nothing is sent anywhere. Each item
gets a verdict:

- **safe** - the screens found nothing that holds. The mark is stamped with a
  fingerprint of what the item says (its kind, scope, name, description and
  body), and holds only while it still says exactly that: an agent's later
  change lapses it on its own. A star or a tag change does not.
- **flagged** - a holding code fired. The Held screen lists flagged items
  under **Review**, each with its codes, the sessions it was already injected
  into, and its text as an agent reads it, so you can decide.

**Mark safe** on a flagged item marks it safe as it reads now and, for a
memory, stamps it owner-confirmed - the same mark a released write carries.
**Re-run review** screens the whole corpus again and marks every clean item
safe as it reads now; an item you marked safe keeps your mark while it is
unchanged. The review runs once by itself; after that it runs when you ask.

The review does not run when the database is migrated: the migration only
adds the table, so an installation that never turns the feature on does no
review and stores nothing.

## Changed outside Seamless

The review also records the content hash of every file, and while the
feature is on every write Seamless makes updates that record. Every
`drift_interval` (5 minutes by default) the drift watch compares each file on
disk with it:

- a file whose bytes no longer match was **changed** outside Seamless - an
  editor, a script, a sync tool, another program;
- a memory or note file Seamless has no record of **appeared** there;
- a recorded file that is gone went **missing**.

A changed or appeared file is quarantined **in place**: it stays on disk
exactly as it is (Seamless never moves, rewrites or deletes it), its text goes
through the screens and the codes are recorded, `corpus.drift` is logged, and
while the feature is on no briefing or prompt recall injects it - lockdown or
not, because its text never passed the write screens. An agent's edit of a
quarantined memory is screened against nothing, so the drifted text cannot
ride in as if it were already there, and the quarantine stays until you act.
A missing file is reported and nothing is restored.

The Held screen lists quarantined files under **Changed outside Seamless**,
with their codes, the sessions they reached before they changed, and their
text as an agent reads it. **Accept** records the file as it stands, lifts the
quarantine and, for a memory, stamps it owner-confirmed. If it should not
stand, edit it back or delete it: a file put back to the exact bytes Seamless
recorded leaves the quarantine by itself.

Editing memory files in your own editor is fine; each edit lands in the list
until you accept it. Changes made while the feature was off are not watched:
turning it back on adopts the files as they are (it does not re-run the
review, so their safe marks stay lapsed until you do).

## Lockdown

Lockdown is for an incident: something wrote where it should not have, and you
want agents working only from knowledge you trust until you have looked. It
only ever narrows what agents receive. While it is on:

- every change that touches a constraint, convention, protocol or stage - a
  write, an edit, a supersede, a delete, a gardener apply - is **held** for you
  with the reason `lockdown`, from any session;
- briefings and prompt recall inject only memories you confirmed or the review
  marked safe and that still say what they said then. An agent-written memory
  created or changed since waits for you or the next review. The briefing
  carries one line saying lockdown is on and how many memories it withheld.

Two things turn it on, and either keeps it on:

- the switch on the Held screen (`lockdown.changed`, source `console`);
- the file `screening.lockdown_file` existing. Another program - a security
  monitor during an incident - can create it. The console cannot lift a
  lockdown the file holds; it lifts when the file goes. A file that exists but
  cannot be checked counts as present.

Lockdown does nothing while the feature is off. `recall` and `memory_read`,
which an agent asks for, are not narrowed.

## Refusals

Every refused access becomes an `access.refused` event while the feature is
on, with a reason code:

| Reason | When |
|---|---|
| `fence.read`, `fence.write` | A read or write across a [project isolation](/concepts/project-isolation/) fence. `fence.write` is also every cross-project write attempt. |
| `peer.unreachable` | Under [peer_auth](/guides/peer-auth/), an account reaching a project it may not. |
| `peer.unknown` | A socket caller whose OS account peer_auth does not know, or with no peer credential. |
| `peer.owner_only` | A restricted account reaching the owner's console. |
| `auth.failed` | A request with no valid bearer key, on any surface. |

The event names the surface (`mcp`, `hooks`, `a2a`, `console`), the tool and
the target project when there is one - never the call's arguments. It is
rate-limited: one event per reason, surface, tool, target and session every
ten seconds, with the refusals in between counted in `suppressed`.

## The metadata feed

With `screening.feed_socket` set, the daemon delivers every event's metadata
to that Unix socket, for a monitor that should see what agents do without
reading Seamless. A frame carries the event's kind (as `activity`), time,
class, severity and outcome; the session's name and agent process; the
project - **a confidential or sealed project, or one locked under
peer_auth, as a keyed hash** (`h:` and 24 hex characters, keyed by a random key
in the spool folder); the item kind and id; and the codes, counts, sizes and
flags the event carries. It never carries arguments, results, bodies,
prompts, queries, names or titles: each frame is built from an allowlist of
metadata keys, and a value that does not fit its pattern (free text where a
code belongs) is dropped.

The protocol is HTTP/1.1 on the socket: `GET /v1/limits`, then
`POST /v1/batch` with `Content-Type: application/x-ndjson`, a header line
`{"v":1,"batch":"<id>","frames":<n>,"dropped":<n>}` and one frame per line.
The consumer answers each batch with a receipt whose `action` is `settle`,
`retry` or `discard`. While it is down the feed spools to `feed_spool`
(0700): bounded at 10,000 frames and 8 MiB, the oldest dropped first and
counted in the next header's `dropped`; one batch in flight, resent unchanged
under its id until a receipt settles or discards it, across restarts too. When
nothing else is waiting it sends a heartbeat frame every third of the
consumer's `heartbeat_s`.

The feed reads the event log itself, in the order events landed, from a
cursor it keeps in the spool, so a burst never loses an event. Turned off and
on again, it starts from the newest event rather than replaying the gap.

## Forensics

`retrieval.injected` - the record of which memories a briefing, a prompt
recall or a recall answer gave which session - is never pruned: the retention
pass only ever removes transport events (`tool.call`, `hook.prompt`,
`recall.miss`, `hook.error`). So when a memory turns out poisoned, the
sessions it reached are on record back to its first injection. The Held
screen shows them beside each flagged or quarantined memory.

## Events

| Event | When | Payload |
|---|---|---|
| `memory.screened` | Every screened write, clean ones included (`verdicts: []`), so a monitor can tell a clean write from one never screened. | `item_kind`, `kind`, `tool`, `size`, `verdicts`, `held` |
| `memory.held` | A write went to the queue. | `hold`, `op`, `item_kind`, `kind`, `tool`, `reasons` |
| `memory.released` | You released a held write and it landed. | `hold`, `op`, `item_kind`, `kind`, `tool`, `by` |
| `memory.discarded` | You discarded a held write. | `hold`, `op`, `item_kind`, `kind`, `tool`, `by` |
| `session.tainted` | A session got the outside-text mark (once). | `reason`, `via` |
| `session.untainted` | You cleared a session's mark. | `by` |
| `access.refused` | An access was refused (rate-limited). | `reason`, `surface`, `tool`, `target`, `suppressed` |
| `corpus.reviewed` | The review read the corpus. | `items`, `safe`, `flagged`, `by`, `baseline` |
| `corpus.drift` | A file changed, appeared or went missing outside Seamless. | `change`, `item_kind`, `verdicts`, `quarantined` |
| `corpus.confirmed` | You marked a flagged item safe or accepted a quarantined one. | `item_kind`, `via`, `by` |
| `lockdown.changed` | Lockdown went on or off. | `on`, `source`, `by` |

Each carries the session, project and item ids in the event's own columns.
None carries a body, a description, or the text a screen matched.

## Calibrate before you turn it on

The screens ship with a calibration test that reads a folder of memory and
note markdown files and prints how often each verdict would fire, per code and
per file - never the matched text:

```bash
SEAMLESS_SCREEN_CORPUS=/path/to/a/copy/of/memory go test ./internal/screening \
  -run TestCalibration_Corpus -v
```

`SEAMLESS_SCREEN_ALLOWLIST` and `SEAMLESS_SCREEN_BLOB_MIN_LENGTH` set the rules
it runs with. Run it on a copy of a corpus you are allowed to read.

## What it does not cover

- **Your own writes.** Console actions and the owner CLIs (`seamlessd import`,
  merges, the v1 importer) are yours, and are not screened.
- **Other text agents leave.** Session findings and task titles also reach
  briefings, and plan-mode captures land as notes; none of them is screened
  by this feature, and lockdown does not narrow them.
- **What agents ask for.** Drift quarantine and lockdown narrow what is
  injected; `recall` and `memory_read` still answer an agent that asks.
