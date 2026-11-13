# thereisnospoon.org -- landing page + docs site

The `docs/` directory is the Seamless public site, served by GitHub Pages: a
hand-written landing page at `/`, and the generated documentation site at
`/docs/`. This file lives at the repo root on purpose -- everything under
`docs/` is published verbatim, and maintainer notes have no business being
served at thereisnospoon.org.

## Contents (paths relative to `docs/`)

```
index.html              the landing page (single file, no build step)
claude-app/index.html   hand-written page for people who use Claude in the
                        desktop app for work that isn't code (law, investing,
                        research, writing, consulting). Built on the landing's
                        home.css (same header, rail, chapters, steps, FAQ,
                        finale, footer) plus an inline <style> for its own
                        pieces and an inline script for the shared motion;
                        it does not load home.js, which drives the landing's
                        own sections
CNAME                   custom domain: thereisnospoon.org
.nojekyll               disable Jekyll (it would skip our _-prefixed paths); the
                        Actions deploy runs no Jekyll either
sitemap.xml             GENERATED (docsgen): the landing page + every docs page
robots.txt              GENERATED (docsgen): crawl policy + the sitemap pointer
llms.txt                GENERATED (docsgen): the nav as a linked outline for LLMs
llms-full.txt           GENERATED (docsgen): every page's full source markdown
index.md                GENERATED (docsgen): the landing page's markdown twin
                        (llms.txt under the name the Accept: text/markdown
                        rewrite expects; see "Markdown for agents" below)
auth.md                 GENERATED (docsgen): the agent-readable auth statement
                        (embedded from cmd/docsgen/auth.md)
.well-known/api-catalog GENERATED (docsgen): RFC 9727 linkset for API discovery
.well-known/mcp/server-card.json
                        GENERATED (docsgen): MCP Server Card (SEP-1649),
                        generated from the repo-root server.json
.well-known/agent-card.json
                        GENERATED (docsgen): A2A Agent Card -- the twin of the
                        card each install's daemon serves live (internal/a2a)
.well-known/agent-skills/
                        GENERATED (docsgen): Agent Skills discovery index
                        (index.json) plus each maintained skill's SKILL.md,
                        published verbatim from the embedded internal/skills
                        assets
<64-hex>.txt            the IndexNow key file (see "Ping IndexNow" below)
scenarios/              GENERATED scenario pages -- do not edit (see below)
static/home.css         the landing page's own stylesheet: the "knowledge sky"
                        identity (night sky dark / star-atlas light), tokens,
                        every section, motion states, reduced-motion rules
static/home.js          the landing page's motion and interactive pieces: the
                        hero sky canvas, word reveals, the chapter rail, the
                        week/orbit/board/folder animations, the gardener card,
                        console tabs, the day-one asks index (the request
                        index and say-boxes are styled in home.css because
                        claude-app/'s first-chats section reuses them), and
                        the local-only "welcome back"
                        briefing (localStorage key seamless.visit)
static/scenes-player.js animates the verbatim scenes.js transcripts (landing only)
static/site.css         shared stylesheet for the docs, /compare/ and the
                        scenario pages (the landing page no longer loads it)
static/site.js          theme toggle, OS switch, copy buttons, phone menu,
                        scroll reveals (no deps; shared by landing + compare +
                        scenario pages)
static/webmcp.js        WebMCP tools for browser agents: search the docs, read
                        any page as markdown, list the agent endpoints; loaded
                        by every page, a no-op without a model context
static/favicon.svg      the Seamless mark (an empty set)
static/shots/           console screenshots, dark + light (see below)
static/og.png           1200x630 social preview card
static/og-source.html   source for og.png (see below)
static/fonts/           self-hosted woff2 (OFL, see OFL-NOTICE.txt): Fraunces,
                        Instrument Sans, IBM Plex Mono (current identity) plus
                        the previous Space Grotesk / Hanken / JetBrains Mono
docs/                   GENERATED docs site -- do not edit (see below)
```

Fully self-contained: no CDNs, no webfont requests, no analytics, no cookies.
Light + dark themes (follows the OS; the toggle stores an override). The docs
share this design system and the same `localStorage("theme")` key, so a theme
chosen on either surface holds across both.

## The docs site is generated

**Never edit `docs/docs/` by hand.** It is build output, committed so GitHub
Pages can serve it without a CI build step:

```
docs-src/           markdown sources + nav.yaml (the information architecture)
cmd/docsgen/        the generator: templates, assets, and the two code generators
docs/docs/          generated HTML (committed; every file carries a marker)
```

```bash
make docs           # regenerate docs/docs/ from docs-src/
make docs-serve     # regenerate and serve at 127.0.0.1:8899/docs/
make docs-check     # fail if the committed output is stale (part of `make check`)
```

docsgen also renders `docs/scenarios/` -- one crawlable, answer-first page per
landing-page terminal scene, at `/scenarios/<slug>/`. The transcripts come from
`docs/static/scenes.js` (the same single source of truth the scene player and
the landing-page fallbacks use; no line is ever re-typed) and the framing prose
is authored in `docs-src/_scenarios/<slug>.md`, split by a
`<!-- transcript -->` marker into the opener and the closing sections. The two
must stay in lockstep: a scene without a framing file, or a framing file naming
a ghost scene, fails the build. The output is committed and diffed by
`make docs-check` like the docs tree.

docsgen also writes the crawler files at the site root: `sitemap.xml` (the
landing page plus every docs page, no lastmod -- there is no deterministic date
source), `robots.txt` (whose `# seamless-robots-v1` marker is how you can tell
from outside whether the live host serves this file or Cloudflare's managed
one), `llms.txt` (the nav as a linked outline, in the llmstxt.org shape),
`llms-full.txt` (every page's full source markdown, untruncated), and
`.well-known/api-catalog` (an RFC 9727 linkset naming the machine-readable
entry points; GitHub Pages serves it as application/octet-stream, so the
required application/linkset+json content type comes from a Cloudflare
response-header rule on the zone, not from this repo), and
`.well-known/mcp/server-card.json` (the MCP Server Card, SEP-1649: identity
and version generated from the repo-root `server.json` so the registry listing
and the card cannot disagree, plus the streamable-HTTP endpoint every install
serves on its own machine -- Seamless has no hosted remote, and the card says
so; the `.json` extension gets application/json from GitHub Pages natively, no
edge rule needed), `.well-known/agent-card.json` (the A2A Agent Card: the site
twin of the card each install's daemon serves live at the same path, both
rendered by `internal/a2a.CardJSON` so the two cannot drift -- the twin carries
the `server.json` version and the default bind address),
`.well-known/agent-skills/` (the Agent Skills Discovery RFC surface: an
`index.json` naming each maintained skill with a `sha256:` digest, plus each
skill's `SKILL.md` -- both artifact and digest come from the same embedded
`internal/skills` bytes `seamlessd install-hooks` delivers, so the published
skill, its hash, and the installed one are one set of bytes; `.json` and `.md`
both get their content types from GitHub Pages natively, no edge rule), and
`auth.md` (the agent-readable auth statement, embedded from
`cmd/docsgen/auth.md`). Unlike `docs/docs/`
these are written in place, never deleted, and the rest of this directory is
not docsgen's to touch. All twelve (with the root `index.md` twin, see below)
are diffed by `make docs-check` -- `SITE_FILES` in the Makefile is the list,
and a docsgen test pins it to exactly the set the generator emits -- so adding
or removing a page keeps them current automatically, and a release's
`server.json` bump makes the committed cards stale until `make docs` is rerun.

## The context picker and variant containers

The docs carry one OS/client reading context with three presentations:

- **The header chip + popover**, on every docs page: the chip states the
  active context (`macOS · Claude Code`) and opens a popover with the same
  segmented controls the bar renders, so the filter is visible and editable
  on pages that render no variants. On narrow widths the chip shortens to its
  OS half but stays.
- **The `Steps for` bar**, rendered only on pages that vary, below the
  h1/lede (the `ctxbar` template, invoked by `page.html`/`home.html` -- it is
  point-of-use chrome, not a banner). It also counts variant blocks the
  current filter hides ("2 blocks for other setups hidden"; clicking shows
  everything). On phones the bar is non-sticky and collapses to a one-row
  summary plus a Change button that opens the header popover.
- **The docs-home router cards** ("Which agent do you run?"), which are plain
  links -- they route, they never set context as a side effect.

Authors gate a span of markdown with a fence-style container:

```text
::: when os=windows client=codex
...markdown (no `##`/`###` headings -- build error; the chips are the label)...
:::
```

`cmd/docsgen/variants.go` expands that at build time into a labeled
`.ctx-variant` div; visibility is pure CSS off `data-os` / `data-clients` on
`<html>`, set pre-paint by the layout's head script (UA detect, then
`localStorage`, then `?os=` / `?client=` query params, which win -- use them
for deep links, e.g. `/docs/quickstart/?os=windows&client=codex`). Picking is
one delegated handler in `docs.js` shared by the bar and the popover; a pick
rewrites `<html>` and `syncCtx` repaints every control, the chip, the bar
summary, the hidden count, and the aria-live status. Unknown values, nesting,
an unclosed container, or a heading inside one fail `make docs`. Value sets
live in `variantDimensions`; extending them means extending the CSS
visibility rules in `assets/docs.css` and BOTH segmented-control copies in
`templates/layout.html` (the ctxbar template and the header popover) too.

Degradation is part of the contract: without JS no attribute is set, so every
variant shows (labeled by its chips) and all picker chrome -- chip, popover,
and bar -- hides; the markdown twins, `llms-full.txt`, and the search index
run `textifyVariants`, so text consumers get every variant under a bold label
and never the `:::` syntax.

The `localStorage("os")` key is shared with the landing page's coarser
unix/windows switch: the docs write `macos|linux|windows|all`; the landing
bootstrap folds `macos|linux` back to `unix` for display (site-check
assertion 12); and a landing pick stores the UA-refined canonical value --
`macos` or `linux`, never the coarse `unix` (assertion 13) -- while the page
keeps its two-way display, so a pick made on the landing survives into the
docs instead of being re-UA-detected there. The docs bootstrap still
UA-refines a legacy stored `unix`. Change any side only in lockstep. The setup page's configurator widget (`class="cfg"`) composes the
canonical install command from this same picker state; its `data-cmd-*`
attributes are pinned to the canonical commands by site-check assertion 11.

Single-home rule for instructions: a command sequence has one home page; a
fact may be *named* anywhere but is *explained* once (the OS service
commands live on `reference/service.md` -- site-check assertion 10; the
per-event hook tables live on `reference/hooks.md` -- pinned by
`internal/hooks/codex_contract_test.go`). The two canonical install one-liners
are the deliberate exception: every teaching surface carries them verbatim,
and site-check assertion 2 owns that.

## Markdown for agents (content negotiation)

Every docs page is emitted twice: `index.html` and a markdown twin at
`index.md` -- the page's full source markdown (the same content `llms-full.txt`
aggregates, split back out per page) led by its title and description, with
root-absolute links rewritten to canonical URLs (see `cmd/docsgen/twin.go`).
The twins are committed and diffed by `make docs-check` like everything else in
`docs/docs/`.

The landing page's twin is the site-root `index.md` -- the llms.txt outline
under the twin name (see `siteRootFiles`), so `/` negotiates exactly like a
docs page.

They exist so agents sending `Accept: text/markdown` get markdown while
browsers keep getting HTML. GitHub Pages cannot vary on request headers and the
zone's Cloudflare plan has no Markdown for Agents setting, so the negotiation
is one URL rewrite Transform Rule on the zone (like the api-catalog
content-type rule, zone config, not repo config -- this is its documentation).
Custom filter expression:

```
any(http.request.headers["accept"][*] contains "text/markdown")
and ends_with(http.request.uri.path, "/")
and (starts_with(http.request.uri.path, "/docs/") or http.request.uri.path eq "/")
```

with the dynamic path rewrite `concat(http.request.uri.path, "index.md")`. No
Content-Type rule is needed: GitHub Pages serves `.md` files as text/markdown
natively. (A response-header transform was tried first and turned out not to
fire for requests whose URL a rewrite rule had already changed -- serving a
real `.md` file from origin sidesteps that entirely.)

Scenario pages and `/compare/` are outside the rule's scope on purpose: they
have no twins, and a rewrite without a target file would turn a working HTML
response into a 404.

**The drift rule:** because the output is committed and `make check` runs
`docs-check`, any change to `docs-src/` must be followed by `make docs` and the
result committed in the same change, or the build goes red. That applies to
changes in the *code* too: the MCP tool reference is generated from
`mcp.Catalog()` and the configuration reference from `config.Defaults()`, so
adding a tool or a config key makes the committed docs stale by design -- that is
the mechanism that stops the reference from quietly lying.

Upgrading chroma, goldmark, or Go can shift the generated HTML for the same
reason. Regenerate in the same PR as the upgrade. Never auto-regenerate in CI:
the point of committing the output is that a human saw it.

One page has a second, release-time generator: `docs-src/changelog.md` is
rewritten from the git tags by `make changelog` (scripts/changelog.sh), then
rendered and committed like any authored page. The split is deliberate --
release dates are real wall-clock data, and injecting them during `make docs`
would break the byte-determinism everything above rests on. Between releases
the page is ordinary committed source; the release skill runs the refresh as
its final step.

## The landing page is not

`index.html` is hand-written, and that cuts both ways: no build step, but also
no generator to keep it honest. `docs-check` never reads it, so for a while
nothing did -- the `curl | sh` installer shipped with the README and every docs
page correct and the whole gate green, while the hero pill still told visitors
to run `go install`.

`make site-check` (in `check` and `check-fast`) closes the part of that a
machine can see:

```bash
make site-check     # scripts/site-check.sh
```

1. the hero pill runs the canonical install command
2. `index.html`, `README.md`, `docs-src/quickstart.md`, and `docs-src/install.md`
   all teach that same command
3. every `seamlessd <sub>` the page names in a `data-copy` or `<code>` is a real
   subcommand in `cmd/seamlessd/main.go`
4. every copy button's `data-copy` matches the text beside it -- the button reads
   the attribute, not the DOM, so the two drift silently and nobody proof-reads
   an attribute

The canonical install command lives in one place, `INSTALL_CMD` in
`scripts/site-check.sh`. Change it there and the check names every file that
still disagrees.

It does not read prose. "One binary, no ceremony" is still yours to keep true.

The Claude app page (`claude-app/index.html`) teaches the same install, so
assertions 2-6 cover it too: both install commands, real `seamlessd`
subcommands (a command spelled with its install path, like
`~/.local/bin/seamlessd console-open`, is folded to the bare name first),
copy buttons that copy what they show, asset stamps, canonical, and head tags.
Its claims about Claude's own memory and the app's surfaces are prose, dated
"checked October 2026" on the page; recheck them when Anthropic changes the
app.

### Editing the landing page without fooling the gate

- The FAQ assertion reads every bare `<summary>` on the page as a question. A
  disclosure that is not an FAQ entry needs an attribute on its summary
  (`<summary class="what-s">`), or the gate demands a JSON-LD question for it.
- The copy-button assertion only sees `data-copy="X">X<`, so `data-copy` must
  be the element's last attribute (`<span class="cmd" data-copy="...">`).
  Put it anywhere else and the pair is silently skipped, not checked.
- A new asset the page loads by name joins `ASSETS` in `scripts/site-stamp.sh`
  and the `stamped "$PAGE" ...` list in `scripts/site-check.sh`, in the same
  change.
- Honesty rules carry over from the scenes: transcripts stay verbatim
  (`scenes.js`, `distill.py`); every other terminal on the page is
  illustrative, built from the seeded demo's real memories where possible,
  and the one invented narrative (the Mon/Tue/Wed week) is labelled as an
  illustration on screen.

## Preview locally

```
make docs-serve     # landing page at /, docs at /docs/
```

Or any static file server pointed at `docs/`:

```
cd docs && python3 -m http.server 8899
```

then open http://127.0.0.1:8899/.

## Go live (manual, owner-gated)

The site is published from a tag, never from a push to main
(`.github/workflows/pages.yml`):
- Every release publishes it from its own tag: release.yml's `pages` job,
  after the release is published and verified. A prerelease does not.
- Between releases, an admin-only `site-*` tag publishes it, for example
  the changelog refresh after a release or a docs fix:
  `git tag -s site-YYYYMMDD -m site-YYYYMMDD && git push origin site-YYYYMMDD`.

Pushing main therefore changes nothing at thereisnospoon.org, the installers
included.

1. GitHub repo Settings -> Pages -> Build and deployment -> Source: **GitHub
   Actions**. (It was the legacy `main` branch, `/docs` folder build.) Then:
   - The `github-pages` environment admits the tags `v*` and `site-*`, and
     no branch.
   - The tag ruleset that keeps `v*` tags admin-only also covers `site-*`.
   - Under Actions the `CNAME` file is not read; the custom domain is the
     one set in the Pages settings.
2. DNS for thereisnospoon.org: apex A records to GitHub Pages IPs
   (185.199.108.153 .. 185.199.111.153) + AAAA if desired; `www` CNAME to
   `arctop.github.io` (optional). The `CNAME` file here is already in place.
3. In Pages settings, set the custom domain and enable Enforce HTTPS once the
   cert is issued.

## Ping IndexNow after a deploy (manual)

```
make indexnow           # submit the sitemap's URL list; DRY=1 previews the payload
```

`api.indexnow.org` forwards to Bing, Naver, Seznam and Yandex; Google does not
participate. The script takes its URL list from the committed `sitemap.xml`
(docs-check keeps that current) and refuses to ping until the live site serves
the key file -- the same fetch the engines use to verify host ownership -- so
run it after a push has actually deployed. Deliberately manual, never CI: a
ping per commit is the over-submission pattern the engines deprioritize.

The key is public by construction (it proves control of the host, it is not a
secret) and lives in two places that ship as a pair: `KEY` in
`scripts/indexnow.sh` and the committed `docs/<key>.txt` it names. Rotate both
together; the script refuses to run when they disagree.

## Regenerate the OG card

```
"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" --headless \
  --disable-gpu --screenshot=docs/static/og.png --window-size=1200,630 \
  --hide-scrollbars 'http://127.0.0.1:8899/static/og-source.html'
```

## Regenerate the console shots

`static/shots/` holds the console screenshots (6 pages x dark/light, WebP).
They come from a THROWAWAY instance seeded with fictional data by
`cmd/demoseed` -- never from a live data dir. To re-capture after a console
change:

```bash
# 1. seed a throwaway data dir (numbers are tuned; see internal/demokit/data.go)
go run ./cmd/demoseed -data /tmp/seamless-demo

# 2. serve it on a port that is NOT your live daemon. The optional features ship
#    off, and a fresh data dir is a new installation -- without all three the
#    seeded trials, momentum cues, and the Now screen's arcade never render.
#    A new installation's console also starts at level basic, which hides Now,
#    Interactions, and Retrieval; the level flag puts every screen back.
SEAMLESS_DATA_DIR=/tmp/seamless-demo SEAMLESS_ADDR=127.0.0.1:8090 \
  SEAMLESS_FEATURES_RESEARCH=1 SEAMLESS_FEATURES_MOMENTUM=1 \
  SEAMLESS_FEATURES_GAMIFICATION=1 SEAMLESS_CONSOLE_LEVEL=advanced \
  SEAMLESS_MCP_API_KEY=<any key> ./bin/seamlessd serve

# 3. capture both themes at 1440x900 @2x (Playwright driving installed Chrome)
SEAMLESS_SHOT_BASE=http://127.0.0.1:8090 SEAMLESS_MCP_API_KEY=<same key> \
  node scripts/branding/console-shots.js /tmp/shots

# 4. convert into place
for f in /tmp/shots/*.png; do
  cwebp -q 84 "$f" -o "docs/static/shots/$(basename "${f%.png}").webp"
done
```

Capture within ~an hour of seeding: the demo's "live" sessions and leases are
anchored to the seeding time and go stale on screen after that.

`scripts/branding/README.md` is the operator-facing version of this recipe, next
to the other branding flow (record -> distill -> `docs/static/scenes.js`).

## Regenerate the terminal scenes

`docs/static/scenes.js` holds the verbatim with/without transcripts the scene
player animates and `/scenarios/` renders. It is produced by recording two real
interactive Claude Code sessions against the throwaway fixture and distilling
them; the whole flow lives in `scripts/branding/` (`record.sh` -> `distill.py`)
and is documented in `scripts/branding/README.md`. Regenerating it replaces live
marketing surface, so it is a deliberate act, never a side effect of a build.

## Pending (owner approval required)

- Spot-check on a real phone. Narrow widths are no longer unexercised: the page
  was swept at 320/360/390/414/600/768/900/1024/1280/1440 under Chrome device
  emulation and has no horizontal overflow at any of them. That sweep caught a
  real bug (the hero and quickstart grids each declared a bare `1fr`, whose
  min-content floor was the nowrap install command -- 157px of overflow on a
  390px phone); both now use `minmax(0, 1fr)`. A real handset would still add
  touch-target and font-rendering confidence that emulation can't.
