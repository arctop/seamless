---
title: Projects & scope
description: How Seamless decides which project a call belongs to - the precedence chain, the fail-closed rule, and project families.
---

A **project** is the scope everything else inherits. Get scope right and agents
never think about it again; get it wrong and knowledge lands where nobody will
find it.

The design goal is that agents almost never pass `project`. In a mapped repo,
scope resolves from where the agent already is.

## The precedence chain

For a durable write, scope resolves in this order - first hit wins:

<figure class="doc-figure" data-tone="warn" aria-labelledby="scope-precedence-caption">
  <span class="figure-kicker">First match wins</span>
  <div class="doc-flow cols-4">
    <div class="flow-node emphasis"><span class="flow-step">1 · strongest</span><strong>Explicit project</strong><small>The caller named the scope.</small></div>
    <div class="flow-node"><span class="flow-step">2</span><strong>Bound session</strong><small>The MCP connection inherited a project.</small></div>
    <div class="flow-node"><span class="flow-step">3</span><strong>Sole ambient</strong><small>The local hook session is unambiguous.</small></div>
    <div class="flow-node error"><span class="flow-step">4 · no match</span><strong>Reject the write</strong><small>Never guess global when nothing resolved.</small></div>
  </div>
  <figcaption id="scope-precedence-caption"><strong>Precedence is also the safety model.</strong> Broader fallback stops before it could silently create machine-wide knowledge.</figcaption>
</figure>

Worked through, rung by rung:

**1. An explicit `project` argument.** You said it, so it happens. This is also
how you deliberately reach another project (`project: otherrepo`) or the global
scope (`project: global`). A slug Seamless has never seen is **not** an error: the
write registers it, and the new project appears in `project_list` and the console
like any other. Naming a project into existence is an ordinary thing to do.

**2. The bound session.** A connection bound to a session passes that session's
project to every later call on it. A Claude Code or Codex agent is bound
automatically: its SessionStart hook opens an ambient session in the project its
cwd maps to (via the `repo_project_map` setting), and the agent process the two
share joins its tool calls to that session
([how](/concepts/sessions/#process-binding)). Any client can bind with
`session_start` instead, which takes precedence. This is the rung that makes
mapped repos feel like magic: an agent that never calls a single session tool
still writes to the right project, because its cwd said which one - even while
other agents work in other repos.

**3. The sole ambient session.** No binding at all - a hookless client that
skipped `session_start`, `seam` run by hand, an agent whose `seam` predates the
automatic binding? Seamless falls back to the live ambient sessions: if they all
sit in one project, the call inherits it. A caller that names its process -
every current `seam` transport does - skips sessions stamped with a different
one, so an agent whose own session has ended never inherits someone else's; when
other agents' sessions are all that is live, its call is refused rather than
answered from the global scope.

If that lookup is **ambiguous** - live sessions in more than one project - the
call is rejected rather than guessed.

**4. Nothing.** With no session and no explicit project, a durable write is
**rejected as ambiguous**.

## Why writes fail closed

This is the rule worth understanding, because it is the one that produces
confusing errors.

The tempting alternative is: no scope resolved, so write it globally. That is
wrong. A global memory is seen by **every agent in every repo, forever**. It is a
strong claim, and the last thing you want is for it to be what happens when the
system is *unsure*. Silence should not be consent to the broadest possible scope.

So a write with no resolvable scope errors out and says so. `project: global` is
a token you pass on purpose, never a default you fall into.

The way out of that error is to **name the project** - and if the right one does
not exist yet, name it anyway. This is worth stating plainly because the opposite
guess is so natural: an agent unsure whether an unmapped slug will be rejected
reads `project: global` as the cautious choice, when it is the only choice with
machine-wide blast radius. A new project is cheap, local, and reversible. Global
is forever and everywhere. When in doubt, invent the project.

Reads are more relaxed than writes - a search with no scope has an obvious safe
answer (search what you can see), while a write does not.

## Mapping a repo

The ambient sessions behind rungs 2 and 3 are placed by the `repo_project_map`,
but you rarely write an entry into it. The map grows itself: on session start in
an unmapped cwd, Seamless finds the enclosing git repository, derives a slug from
the repo root's directory name, registers the project, and records
`repoRoot -> slug`. No recompile, no setup step. A cwd outside any git repo
registers nothing and stays global.

The map also heals itself when a repo moves. A session starting from the new
location derives the same slug, and when every mapped path that owns that slug
no longer exists on disk, Seamless treats it as the same repo at a new path:
the existing project is adopted, the dead entries are replaced by the new root,
and a `repo.moved` event records the remap in the console feed. Two *live*
repos sharing a directory name still get distinct projects (`backend`,
`backend-2`) so unrelated repos never inherit each other's memories. The one
case the healing cannot recognize is a repo that was moved *and renamed* - it
derives a different slug - and that is what the override below is for.
`seamlessd doctor` lists mapped paths that no longer exist.

Map by hand only to override the derived slug - an `ios` directory that should be
the `example-ios` project, or a renamed repo that should keep its old project:

```bash
seamlessd map-repo --path ~/code/ios --project example-ios
```

Either way, agents in that repo inherit its scope from the session their hook
opens, without any tool call at all.

## Families: parents and siblings

Projects can have parents, which makes **families**. A family exists so that
related projects can share what is genuinely shared without merging into one
undifferentiated pile.

Two briefing knobs control the cross-over:

- `briefing.include_parent_memories` - a child's briefing carries the parent's
  memories. On by default: a rule that holds for the parent usually holds for the
  child.
- `briefing.sibling_findings_count` / `briefing.include_sibling_memories` - how
  much a sibling's recent work bleeds into yours. Findings cross over by default
  (two per briefing); sibling *memories* do not, because what is true of one
  sibling often is not true of another.

Splitting one project into children is planned as a unit - see
[The gardener](/concepts/gardener/), which handles it as a `split` rather than a
pile of individual moves.

## When scope goes wrong

The symptom is almost always "the agent wrote it somewhere I can't find it" or
"the write was rejected as ambiguous". Both are the chain above:

- **Rejected as ambiguous** - no session bound, and nothing to infer scope from.
  Run `session_start` with your `cwd` (inside a git repo that also maps the repo
  automatically) or `project=<slug>`, or pass `project=<slug>` on the call.
  `map-repo` is not the fix here - it only overrides an already-derived slug, and
  is never a setup step. From Claude Code or Codex, check that the agent's `seam`
  is current - see [Troubleshooting](/guides/troubleshooting/).
- **Landed in the wrong project** - the cwd mapped somewhere unexpected, or an
  explicit `project` overrode what you meant. Rung 1 beats everything.
- **A tool insists a task is claimed by your own session** - the connection lost
  its `session_start` binding. Re-run `session_start` with the same name to
  rebind.
