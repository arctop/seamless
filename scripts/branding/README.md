# Branding: how the marketing surface gets made

Everything that produces the GH-Pages marketing surface lives here. Two flows,
independent of each other and of the agent-scenario benchmark
(`cmd/seambench`, which shares the same fixture but nothing else):

| Flow | Produces | Entry point |
| --- | --- | --- |
| A. Terminal animation | `docs/static/scenes.js` + a `/scenarios/` page | `record.sh` -> `distill.py` |
| B. Console screenshots | `docs/static/shots/*.webp` | `cmd/demoseed` -> `console-shots.js` |

Neither flow ever touches the live `~/.seamless`, `~/.claude`, or `~/.codex`.
Both run against throwaway instances on non-live ports.

Files here:

```
record.sh          thin wrapper over scripts/fixture/harness.sh --mode record
distill.py         transcript (.jsonl) -> scene data, verbatim-audited
console-shots.js   Playwright capture of the console screenshots
README.md          this file
```

---

## The verbatim rule (read this before flow A)

The landing page says, on screen, *"transcripts verbatim, only the typing is
animated."* That claim is the reason the scenes are worth anything, and it is
load-bearing: if a line on the page is not exactly what the model produced, the
page is a mockup pretending to be evidence.

So the only curation allowed between a recorded take and `scenes.js` is:

- **drop** a whole step,
- **collapse** a run of dropped steps into one `ffwd` marker (rendered as a dim
  fast-forward line, visibly editorial),
- **clip** a step to a contiguous *prefix* (at a paragraph, line, or word
  boundary), optionally marking the cut with `" ..."`.

Never: reword, paraphrase, summarize, fix a typo, re-punctuate, normalize
whitespace, or splice two non-adjacent sentences together.

`distill.py` enforces this rather than asking you to remember it. Every emitted
`user`/`agent`/`inject`/`tool` string must be a contiguous **byte** span of the
transcript, checked twice: once as it is built, and once more by re-reading the
transcript from disk after the whole scene exists. Output is written only if
both pass, and there is no override flag. Try it:

```bash
uv run scripts/branding/distill.py selftest
```

That run proves the audit rejects a re-punctuated line, a paraphrased prompt, a
two-sentence splice, an invented tool result, an invented tool call, and a
focus line that is not in the injection it claims to highlight. It also
re-derives the scene schema from `cmd/docsgen/scenes.go` and fails on drift.

If a take needs a gloss the transcript does not contain, that gloss is an `ffwd`
line. `ffwd` text is the one editorial channel, and the page renders it as one.

---

## Flow A: make a new site animation

### A0. What you need

- A working `claude` on `PATH` (an interactive session, not `-p`: headless takes
  never fire `UserPromptSubmit`, so a `<seam-recall>` injection can never appear
  in one -- memory `headless-cc-p-skips-userpromptsubmit-hook`).
- `uv` (the distill step runs under it; never invoke `python`/`pip` directly).
- Nothing else. No node, no network.

### A1. Stand up the fixture

```bash
scripts/branding/record.sh
```

This builds `bin/seamlessd` + `bin/seam`, scaffolds the throwaway `myapp` demo
repo, seeds a throwaway Seamless instance with the scene fixture (a plan at 4/6
with one claimable step, the engineered memories, an 18h-old finding, a failed
trial), wires a **with** side (hooks + MCP + skills) and a bare **without** side,
self-checks that the with-side briefing actually carries the scene markers, and
prints the recording recipe plus the distill commands.

Useful flags, all passed straight through to
`scripts/fixture/harness.sh --mode record`:

- `--base DIR` where the fixture lives (default `$TMPDIR/seamless-scenes`)
- `--port N` the throwaway daemon's port (default 8099; live is 8081)
- `--model ID` the model both sides are pinned to (default `claude-opus-5`;
  the pin exists so a scene compares Seamless conditions, never model drift)
- `--race` seed the two-claimable variant used by the split-pane coordination
  scene
- `--no-build`, `--no-verify`

`record.sh` adds nothing to that behaviour and re-implements none of it. If you
need something it cannot do, add the flag to `harness.sh`.

**Re-run it between takes.** Leases, finding ages, and plan state are anchored
to seeding time and go stale on screen.

### A2. Record both sides

Follow the printed recipe. In one terminal:

```bash
SEAMLESS_CONFIG=<base>/seamless.yaml <repo>/bin/seamlessd serve
```

In another, the **with** side, then (after re-seeding) the **without** side:

```bash
cd <base>/myapp
CLAUDE_CONFIG_DIR=<base>/home-with/.claude claude
```

Type the same prompt on both sides. **Quit each session cleanly** (`/exit`) --
an interactive `claude` only flushes its transcript to disk on a clean exit, so
a killed session loses the take entirely.

Rules that make a take usable:

- The demo repo must stay Seamless-free (memory
  `scene-demo-repo-must-be-seamless-free`). The scaffolder already handles this;
  do not add a file, comment, or `CLAUDE.md` that names Seamless, the plan slug,
  or "briefing" -- the without side reads the repo and will break character.
- Take several. Pick the one where the without-side pain happens *naturally*, not
  the one where you prompted it into happening.
- Note both session ids; they are published as each pane's `source`.

Transcripts land in `$CLAUDE_CONFIG_DIR/projects/<slug>/*.jsonl`, where `<slug>`
is the demo repo's path with every non-alphanumeric character replaced by `-`.
`record.sh` prints both exact paths.

### A3. Distill

**Inventory each take.** Every curatable step, numbered:

```bash
uv run scripts/branding/distill.py steps <take.jsonl>
```

```
   0  inject  seam-briefing       6014  <seam-briefing>\nSeam project: myapp -- 7 memories …
   1  user                          28  continue where we left off
   2  agent                        138  I'll check where we left off on the auth-refresh plan.
   3  tool    tasks_ready        -> 61 char result
```

Thinking blocks and subagent (sidechain) rows are never candidates; tool results
are attached to the call that produced them.

**Scaffold a spec.** It keeps every step, so you edit it *down*:

```bash
uv run scripts/branding/distill.py scaffold \
  --id cold-start --title "Continue where the last session left off" \
  --prompt "continue where we left off" \
  --pane without=<without take.jsonl> \
  --pane with=<with take.jsonl> \
  -o /tmp/cold-start.json
```

**Curate.** The spec is the whole recipe for the scene: it names the transcripts,
so the build is reproducible and `verify` can re-audit later. Per pane:

| Key | Meaning |
| --- | --- |
| `keep` | step indices to publish: `[0, 1, "5-9"]`. Absent = keep everything the `ffwd` spans did not swallow. |
| `ffwd` | contiguous spans to collapse: `[{"span": "3-6", "text": "reads auth.go"}]`. Omit `text` for a factual default like `"4 tool calls, 2 replies"`. |
| `clip` | keep a prefix of a step: `{"12": {"paragraphs": 1}}`, `{"lines": n}`, `{"chars": n, "ellipsis": true}`. |
| `focus` | lines to highlight inside an `inject` step: `{"0": ["PLAN: auth-refresh -- 4/6 done, 1 claimable"]}`. Each must be verbatim from that block. |
| `result` | which tool calls show their result, and clipped how: `{"3": {"chars": 120, "ellipsis": true}}`. Omit for no result. |
| `label_arg` | append one verbatim argument to a tool label: `{"7": {"key": "file_path", "basename": true}}` renders `Edit auth.go`. |
| `beat`, `emphasis` | split-layout only: the shared clock, and `"win"`/`"bounce"` for a claim collision. |
| `epilogue` | authored illustrative steps appended after the transcript (`comment`, `cmd`, `files`, `fm`). Verbatim roles are rejected here on purpose. |
| `outcome` | one sentence, what this session actually shipped. **Also published as the SSR fallback and gated by `site-check`.** |

`keep` and `ffwd` spans may not overlap; the build fails if the scene `prompt`
does not appear verbatim in one of the takes (the page shows it as what was
typed), or if any `TODO:` placeholder from the scaffold survives.

**Build, to a temp file:**

```bash
uv run scripts/branding/distill.py build /tmp/cold-start.json -o /tmp/scenes.js
```

Iterate `steps` -> edit spec -> `build` until the pane reads well.

### A4. Publish

`docs/static/scenes.js` is live marketing surface, and a build only writes the
scenes whose specs you passed. Writing there is therefore deliberate and refused
by default. Two ways in:

- **Adding a scene to the existing four** (the common case): build to a temp
  file and paste the new scene object into `docs/static/scenes.js` by hand,
  keeping the file's hand-written header comment.
- **Regenerating the whole file**: pass *every* scene's spec at once, reuse the
  existing header, and force the write:

  ```bash
  uv run scripts/branding/distill.py build \
    /tmp/cold-start.json /tmp/hardening-trap.json \
    /tmp/token-safety.json /tmp/coordination.json \
    --header docs/static/scenes.js -o docs/static/scenes.js --force
  ```

  Only do this if you re-recorded all of them. Regenerating replaces animations
  that are already published and linked.

Then wire the scene into the site (`scenes.js` alone publishes nothing):

1. **Landing page** -- add a mount and its server-rendered fallback in
   `docs/index.html`:

   ```html
   <div class="term-scene" data-scene="<scene id>">
     <div class="scenes-fallback"> ... kicker, title, prompt, both outcomes ... </div>
   </div>
   ```

   The fallback is what crawlers and JS-off visitors read. `site-check`
   assertion 9 requires every pane `outcome` in `scenes.js` to appear verbatim in
   it, so copy the strings; do not retype them. Outcomes cannot contain `<`, `>`,
   or escaped double quotes -- that gate parses them textually.

2. **Scenario page** -- add `docs-src/_scenarios/<slug>.md` with frontmatter
   `title`, `description`, `scene` (the scene id), `order`, and a body split by
   `<!-- transcript -->` into opener and closing sections. Coverage is
   bidirectional and gated: a scene with no framing file, or a framing file
   naming a scene that does not exist, is a docs build error (memory
   `scenario-pages-render-from-scenes-js`).

3. Regenerate and gate:

   ```bash
   make docs        # docs-src/ -> docs/docs/ + docs/scenarios/
   make site-stamp  # restamp the ?v= cache-buster on scenes.js
   make check       # includes docs-check and site-check
   ```

4. Sanity-check the animation in a browser: autoplay on scroll-in, the tab
   toggle, replay, both themes, `prefers-reduced-motion` (full static
   transcript, no autoplay), and no horizontal overflow at 320px.

Commit `docs/static/scenes.js`, `docs/index.html`, the framing markdown, and the
regenerated `docs/` output together. Raw takes are **not** committed; record the
session ids in the pane `source` fields, which is the whole provenance trail.

### A5. Re-auditing later

While the takes still exist, any published scene can be re-checked against them:

```bash
uv run scripts/branding/distill.py verify /tmp/cold-start.json \
  --scenes docs/static/scenes.js
```

This re-derives the corpus from the transcripts and re-checks every verbatim
field in the *published* file, so it catches a hand-edit to `scenes.js` as well
as a bad build. It cannot run for the four scenes already on the page: their
takes were never committed, by design.

---

## Flow B: console screenshots

`docs/static/shots/` holds six console pages x dark/light, plus the Privacy
screen's machine plan in two setups for the /security/ page, all WebP. They
come from a throwaway instance seeded with the invented six-week fleet history
in `internal/demokit/data.go` -- never from a live data dir.

The Privacy screen prints the account the daemon runs as, and the data
directory's path. Run the demo as a build that cannot see your account name:
`-tags osusergo` makes Go's user lookup fall back to `$USER` on macOS (a plain
build reads the real account through libSystem, cgo or not), and a scratch
`HOME` gives the data directory a neutral path.

```bash
# 1. seed a throwaway data dir under a scratch home (the console-fleet seed,
#    not -scenes)
mkdir -p /tmp/demo-home
go run ./cmd/demoseed -data /tmp/demo-home/.seamless
CGO_ENABLED=0 go build -tags osusergo -o /tmp/seamlessd-demo ./cmd/seamlessd

# 2. serve it on a port that is NOT your live daemon. The three SEAMLESS_FEATURES_*
#    flags are load-bearing: optional features ship off and this throwaway data
#    dir is a new installation, so without them the seeded history is hidden --
#    research takes the Overview's Labs/Trials tiles and Trials coverage row,
#    momentum takes the Overview memory-of-the-month spotlight and the Plans
#    shipped-this-month count, and gamification takes the Now screen's records
#    rail and hot-streak pulse. SEAMLESS_CONSOLE_LEVEL=advanced is load-bearing
#    for the same reason: a new installation's console starts at basic, which
#    drops Now, Interactions, Retrieval, and the Overview vitals from what the
#    shots capture. (This flow does not go through scripts/fixture/harness.sh,
#    which sets research and the level for its own instances.)
#    SEAMLESS_CONFIG must name a throwaway file (it can be empty): env vars only
#    override keys, so without it serve still loads your LIVE
#    ~/.config/seamless/seamless.yaml and inherits its llm key, tls, and briefing.
#    SEAMLESS_FEATURES_SCREENING puts write screening on, so the Privacy screen
#    shows that layer in use; USER and HOME keep your account name and home
#    path off the shots.
touch /tmp/seamless-demo.yaml
HOME=/tmp/demo-home USER=you SEAMLESS_CONFIG=/tmp/seamless-demo.yaml \
  SEAMLESS_DATA_DIR=/tmp/demo-home/.seamless SEAMLESS_ADDR=127.0.0.1:8090 \
  SEAMLESS_FEATURES_RESEARCH=1 SEAMLESS_FEATURES_MOMENTUM=1 \
  SEAMLESS_FEATURES_GAMIFICATION=1 SEAMLESS_FEATURES_SCREENING=1 \
  SEAMLESS_CONSOLE_LEVEL=advanced SEAMLESS_MCP_API_KEY=<any key> \
  /tmp/seamlessd-demo serve

# 2b. fence one project, so the Privacy screen has a fence to draw
SEAMLESS_CONFIG=/tmp/seamless-demo.yaml SEAMLESS_SERVER_URL=http://127.0.0.1:8090 \
  SEAMLESS_MCP_API_KEY=<same key> ./bin/seam project isolation homelab confidential --yes

# 3. capture both themes at 1440x900 @2x (Playwright driving installed Chrome)
pnpm add playwright-core     # once, anywhere; NODE_PATH must point at its
                             # node_modules -- node resolves from the script's
                             # directory, not the cwd
SEAMLESS_SHOT_BASE=http://127.0.0.1:8090 SEAMLESS_MCP_API_KEY=<same key> \
  node scripts/branding/console-shots.js /tmp/shots
# SEAMLESS_SHOT_CHANNEL= (empty) drives the Chromium build playwright-core
# installs itself (npx playwright-core install --only-shell chromium) instead
# of the installed Chrome.

# 4. convert into place
for f in /tmp/shots/*.png; do
  cwebp -q 84 "$f" -o "docs/static/shots/$(basename "${f%.png}").webp"
done

# 5. restamp and gate
make site-stamp && make site-check
```

`console-shots.js` mints the console cookie from the API key itself (the same
`sha256("seamless-console\0" + key)` digest `internal/console` expects), so no
login step is needed. Before capturing it dismisses the Overview's first-run
level-choice card through the card's own Dismiss button (a submit button whose
`formaction` posts to `/console/settings/level/welcome`): a freshly seeded instance has never
been welcomed, and the card pushes the Knowledge sky below the fold. It waits on `load` rather than `networkidle` because the
console's SSE stream never goes idle.

A page entry may carry a `prepare(page)` hook, run after load and before the
shot. Interactions needs one: its feed is client-rendered and starts empty by
design -- it shows live arrivals only until an owner adds a history window, and
the server-side `history=` param cannot help because the page ships an empty
`#ix-feed` div for the JS to fill. The hook drives the control instead.

**Capture within about an hour of seeding.** The fleet's "live" sessions and
leases are anchored to the seeding time and start reading as stale on screen.
This binds the momentum and gamification surfaces too: the Now screen's records
rail reads "set today" off the same anchor.

The `relations` capture deliberately keeps its legacy filename while pointing at
`/console/context` -- published and cached landing-page URLs reference the old
basename.

---

## Related

- `scripts/fixture/harness.sh` -- the shared fixture both branding and the
  benchmark stand on (`--mode record` here, `--mode bench` for `cmd/seambench`).
- `SITE.md` -- the site's own operational notes (deploy, OG card, IndexNow).
- `cmd/docsgen/scenes.go` -- parses `scenes.js` with `DisallowUnknownFields`; a
  new step field must be added there and in `distill.py`'s schema constants in
  the same change, or the docs build fails.
- `scripts/site-check.sh` -- gates the hand-written landing page. `docs-check`
  never reads it (memory `landing-page-gate-is-site-check`).
