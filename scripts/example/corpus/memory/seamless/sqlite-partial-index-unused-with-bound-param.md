---
id: 01M79FMV7QA6C7FVWTVG0H85J6
kind: gotcha
name: sqlite-partial-index-unused-with-bound-param
description: "SQLite never uses a partial index WHERE col <> '' for a query binding col = ?: it can't prove the bound value. Check EXPLAIN QUERY PLAN"
project: seamless
created: 2026-11-13T03:36:29Z
invalid_at: null
superseded_by: null
source_session: 01M799YS11WQNQSVFZ5DMKTTTP
model: claude-opus-5-5
tags: [sqlite, migrations, indexes, performance]
---
SQLite uses a partial index only when it can prove, at prepare time, that the query's WHERE implies the index's WHERE.

**What fails:** a query that binds the column, `claude_session_id = ?`, never implies `claude_session_id <> ''`, because the parameter is unknown when the statement is prepared. The planner reports `SCAN sessions` exactly as if the index did not exist.

**Found:** 2026-11-12, migration 039 (plan fenced-sessions P1). The suggested index `ON sessions(external_client, claude_session_id, created_at) WHERE ambient = 0 AND claude_session_id <> ''` was refuted by EXPLAIN QUERY PLAN on the real query. Migration 010's `idx_sessions_ambient_external_identity` has the same `<> ''` shape. It is unique for integrity, so it still enforces correctly, but it cannot serve `WHERE ambient = 0` lookups.

**How to apply:**
- For a lookup index, make the partial WHERE use only conditions the query states as literals (for example `ambient = 1` with a query that says `ambient = 1`), or make the index non-partial. 039 chose non-partial: `idx_sessions_conversation (external_client, claude_session_id, created_at, id)`.
- Pin it in a test: `EXPLAIN QUERY PLAN` on the exact production query must show `USING INDEX <name>`. TestMigration039_ConversationLookupsUseTheirIndexes is the model.
