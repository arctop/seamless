---
id: 01M79FMW4W7B0X331VYCWA9C5D
kind: gotcha
name: zsh-unquoted-var-does-not-word-split
description: "zsh never word-splits an unquoted $VAR: `git add -- $FILES` passes one bogus path. Feed path lists through a file and xargs"
project: ""
created: 2026-11-13T03:36:30Z
invalid_at: null
superseded_by: null
source_session: 01M799YS11WQNQSVFZ5DMKTTTP
model: claude-opus-5-5
tags: [zsh, shell, git]
---
The owner's shell is zsh. Unlike bash, zsh does not split an unquoted parameter expansion on spaces. With FILES="a b c", `git add -- $FILES` receives one argument "a b c", and `set -- $spec` does not split either.

**What happens:** the command fails or matches nothing. A script with no `set -e` carries on: on 2026-11-12, during a /seam-fanout commit gate, a temporary index stayed at HEAD, and a worktree was built at the base tree instead of the intended commit tree. A path-set diff caught it before any gate ran.

**How to apply:**
- Keep path lists in a file, one per line, and run `xargs -n 50 git add -- < list.txt`.
- Or use a real array, or `${=VAR}` (zsh-only), or run the snippet under `bash -c`.
- After building a tree to gate, assert its changed path set equals the intended list (`git diff --name-only A B | sort | diff - list`) before trusting the result.

Related: [[zsh-equals-expansion-breaks-bare-double-equals]].
