---
id: 01M7252K4NGBENJ8PH5YF7GP6G
kind: gotcha
name: git-grep-e-word-boundary-silently-matches-nothing
description: "On this macOS, \\b in `git grep -E` matches nothing, so a scan reports clean when it is not. Use `git grep -w` for word matches."
project: seamless
created: 2026-11-10T07:17:04Z
invalid_at: null
superseded_by: null
source_session: 01M721SHNMEXX86WW9ATFM3BFN
model: claude-opus-5-5
tags: [git, macos, search]
---
`git grep -E '\bword\b'` returns no hits on macOS while `git grep -w word` finds them. The failure is silent, so a "nothing left" scan looks like a pass. On 2026-11-09 it hid a host name in three files during a deployment-name cleanup, and the task's own check command had the same flaw. Use `-w` for word-bounded searches, or check the pattern against a known hit first. Go's regexp handles `\b` correctly, so the guard test in cmd/docsgen/public_names_test.go is not affected.
