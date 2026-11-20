---
id: 01M0AY37VN25KDE9MCNVFAP89F
kind: constraint
name: zsh-equals-expansion-breaks-bare-double-equals
description: "The shell is zsh: any unquoted word starting with = is equals-expanded, so a bare == argument dies with \"= not found\"."
project: ""
created: 2026-08-18T17:18:23Z
invalid_at: null
superseded_by: null
source_session: 01M0AXTP38Y6NKAZCD7NDW24JY
model: claude-opus-5-5
tags: [shell, zsh, gotcha]
---
The user's shell is zsh 5.9 on macOS, with the `EQUALS` option **on** (the zsh default). Equals-expansion rewrites any *unquoted word that begins with `=`* into the path of the command named after it: `=ls` -> `/bin/ls`. If no such command exists, zsh aborts the line before it ever runs.

So `==` is not a plain operator token in zsh -- it is a word starting with `=`, and it gets expanded (to a lookup of the command `=`), which always fails:

```
% echo ==foo
zsh: =foo not found
% [ a == a ] && echo ok
zsh: = not found          # the word `==` expands, nothing runs
```

Verified 2026-08-18 on zsh 5.9.

**How to apply** -- when writing a shell command for this machine:
- Prefer `[[ ... == ... ]]`. Inside `[[ ]]` zsh does not equals-expand, so `[[ $a == $b ]]` works (and `==` there is a glob match, not a string compare).
- With the `[` builtin or `test`, use `=` instead: `[ "$a" = "$b" ]`.
- If a literal `==` really must be an argument (an awk/jq/grep expression, a `--opt==value`, a URL or query string), **quote it**: `[ a '==' a ]`, `awk '$1 == "x"'`. Quoting suppresses the expansion entirely.
- The same trap applies to *any* argument beginning with `=`, not just `==`.

This is the same family of quoting hazard as the existing global rule "quote every glob and URL argument" in ~/.claude/CLAUDE.md.

**Why:** it is a repeating mistake -- an unquoted `==` looks like valid bash/POSIX but fails immediately under zsh, and the error message (`= not found`) does not obviously point at the real cause.

[…]
