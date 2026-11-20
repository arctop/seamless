---
id: 01KXMWAX1NBH62PTNQ02QG6GFQ
kind: protocol
name: stage-memories-need-a-live-status-gate
description: "A stage pins only while its body head parses Status: open|in_progress|blocked; gateless stages leave briefings after 7d and get archive proposals at 1"
project: seamless
created: 2026-07-16T07:12:55Z
invalid_at: null
superseded_by: null
source_session: 01KXMTHZTFQW2SYAYYV8FPM6SG
---
Since 2026-07-16 the briefing no longer grants stage memories an unconditional permanent pin. The rules (core.ParseStageHeader / retrieve.pinnedStages / gardener stale-stage pass):

- A stage body must OPEN (first 12 lines) with `Status: open|in_progress|blocked|done` and optionally `Gate: human|ai`. Markdown prefixes (`- `, `**`, `## `) are tolerated.
- `Status: done` unpins immediately (unchanged).
- A stage whose status is missing, unparseable, or unrecognized renders as `status unknown` only for `briefing.stage_unknown_max_age_days` (default 7) after its last update, then silently leaves the briefing. Recall still finds it.
- The gardener's stale-stage pass proposes archiving any non-live-gated stage unchanged for `gardener.stale_stage_days` (default 14). `[[links]]` from another memory protect it, same as staleness.

Practical rule: **a milestone breadcrumb ("X landed") is a note or session finding, not a stage.** Write `kind: stage` only for work that is genuinely gating future sessions, and keep its Status line current -- flip it to `done` when the gate clears, or the gardener will eventually propose archiving it for you.

Why: by 2026-07-15 the seamless briefing carried nine permanent `STAGE: ... -- status unknown` lines (milestone breadcrumbs with no parsable header), which crowded out the budget-participating sections -- the header claimed "3 recent findings" while zero rendered. Findings now also reserve their budget first, so a fat index can no longer evict them.
