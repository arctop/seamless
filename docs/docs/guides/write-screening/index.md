# Screen writes and hold the suspicious ones

> Write screening - every agent write to a memory or note is screened for secrets, injection markers, hidden characters, encoded blobs and unlisted links; suspicious ones, and changes to binding rules by a session that read outside text, wait for your release.

Seamless injects its constraints and conventions into every session's
briefing, and recall serves memories and notes to any agent that asks. That
makes the corpus the most leveraged place to plant a prompt injection: a
poisoned constraint is read by every agent, in every session, until someone
notices. The gardener, which reads memories and proposes rewrites, can carry
one further.

**Write screening** is the optional feature that closes that door. It is off by
default, and off means writes land exactly as they always have. On, it does
three things:

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
can be reviewed - and captured - without the credential.

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

## Events

| Event | When | Payload |
|---|---|---|
| `memory.screened` | Every screened write, clean ones included (`verdicts: []`), so a monitor can tell a clean write from one never screened. | `item_kind`, `kind`, `tool`, `size`, `verdicts`, `held` |
| `memory.held` | A write went to the queue. | `hold`, `op`, `item_kind`, `kind`, `tool`, `reasons` |
| `memory.released` | You released a held write and it landed. | `hold`, `op`, `item_kind`, `kind`, `tool`, `by` |
| `memory.discarded` | You discarded a held write. | `hold`, `op`, `item_kind`, `kind`, `tool`, `by` |
| `session.tainted` | A session got the outside-text mark (once). | `reason`, `via` |
| `session.untainted` | You cleared a session's mark. | `by` |

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
  by this feature.
- **Edits that bypass Seamless.** A file changed on disk by another process is
  re-indexed as it always was; its provenance mark lapses, but it is not held.
