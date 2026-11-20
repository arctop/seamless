# The example page

`thereisnospoon.org/example/` is a read-only copy of part of the Seamless
project's own Seamless: the briefing a session starts from, some of its
memories, one plan with its steps, a research lab, and a few session handoffs.
It is the site's use case, and the landing page's "in use" section leads to it.

```
corpus/      the copy, as plain files in a data dir's layout
page.html    the page around it: head, hero, the explorer's frame, the trail
build.py     corpus + page.html -> docs/example/index.html
```

```bash
python3 scripts/example/build.py            # rebuild the page
python3 scripts/example/build.py --check    # fail if the page is stale
```

`make site-check` runs the check (assertion 16), so a corpus edit without a
rebuild, or a hand edit of the built page, turns the gate red. Python 3.9 or
later, standard library only.

## What the corpus holds

| Path | What it is |
|---|---|
| `briefing.txt` | a SessionStart briefing, as the hook printed it |
| `memory/<project>/<name>.md` | memories, frontmatter and body (`_global` for global ones) |
| `notes/seamless/*.md` | the plan's narrative note, cut after its opening |
| `plan.json` | the plan's facts and every step's title and status |
| `tasks/<id>.md` | the steps shown in full |
| `lab.json` | every trial's title and outcome, and the trials shown in full |
| `sessions/*.md` | session handoffs (findings) |

Records were read with the `seam` command (`seam memory read --json`,
`seam plan show --json`, `seam task list --id`, `seam sessions`) and the
`trial_query` tool. A memory's `created` is the time in its id.

## The rules for a record

This page shows real records under a promise printed on it, so the rules are
strict.

- **Copy, never retype.** A record goes in as it was read. The only changes
  allowed are a cut, which leaves `[…]` where the text was, and a neutral
  phrase in place of a name ("the owner's Mac", "a Windows 11 test machine").
  Nothing is reworded to read better.
- **No deployment's names.** No host, account, home path, address, person, or
  other project. The repository's public-names test reads the corpus like any
  tracked file, and `build.py`'s guard reads the corpus and the built page.
  Neither knows every name, so read each record yourself as well.
- **No open weakness.** A record that describes a security problem still open,
  or how a protection can be passed, stays out, however good the story.
- **Nothing that is someone else's to publish.** Signing identities, account
  details, and notes made by inspecting another vendor's software stay out.
- **Keep the page's numbers true.** The counts in the hero and on the landing
  page (plans, steps, trials, agent runs, commits, the two times) come from
  the project's records and from git on the copy's date. A refresh rechecks
  each one and moves the date in `page.html`, the landing page's "in use"
  section, and the meta description together.

## Changing the page

- The trail under "Start here" and the briefing's notes are written in
  `page.html`. A link into the explorer is `data-open="<record id>"`; the
  build fails if the id is not on the page.
- Record ids: `m-<name>` a memory, `t-<task id>` a step, `tr-<n>` a trial by
  its place in the lab, `s-<8 hex>` a handoff, `p-plan`, `lab-about`.
- `build.py` renders the markdown the corpus uses and no more. If a new record
  uses something else, look at it in the browser.
- The page stamps its own assets. After editing `home.css` or
  `static/example.js`, run the build again or `make site-stamp`.
- Styles for the explorer live in `page.html`'s `<style>`; its behavior is
  `docs/static/example.js`. Without scripts the page reads top to bottom.
