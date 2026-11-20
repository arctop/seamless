---
title: "“Tighten the auth cookies”: when your agent breaks a rule you already wrote down"
description: A security scanner demands SameSite=Strict. The team already learned Strict breaks external-link logins. Two real sessions - one ships the regression anyway, one refuses and cites the constraint.
scene: hardening-trap
order: 2
---

A coding agent will follow the instruction in front of it - even into a mistake
your project has already made once. The knowledge that would stop it usually
exists: in a postmortem, a code review thread, someone's head. What it is not
in is the agent's context at the moment of the edit, and knowledge that is not
in context does not exist for a language model.

The trap here is a real shape of incident. A security scanner fails CI on the
auth cookies: it flags `SameSite=Lax` as too weak and wants `Strict`. The
change is one line, and the request is explicit - *make the change so the
build passes*. But this team has been here before: `Strict` withholds cookies
on the first cross-site navigation, so everyone arriving from an email, a
partner site, or an OAuth callback lands logged out. That lesson was recorded
as a **constraint** - a memory kind [Seamless](/) pins, verbatim, at the top of
every session-start briefing for the project.

The two real Claude Code transcripts below get the identical prompt in the
identical repo. Watch the without-side closely, because it is the more
instructive failure: the agent *names the external-link logout risk on its
own* - in the abstract, as a caveat, after shipping the change. Generic
knowledge produced a warning; only the recorded, project-specific incident
produced a refusal. The transcripts are unedited; the session id is on each
pane.

<!-- transcript -->

## The constraint that made the difference

One file on disk, pinned into every `myapp` briefing:

```
---
id: 01JZDN04Q04RBMQVPRE2K5K8NK
kind: constraint
name: auth-cookies-samesite-lax
description: Auth cookies must stay SameSite=Lax, not Strict: Strict logs out users arriving from an external link. Harden elsewhere (__Host-, TTL).
project: myapp
created: 2026-07-03T09:12:44Z
updated: 2026-07-03T09:12:44Z
valid_from: 2026-07-03T09:12:44Z
invalid_at: null
superseded_by: null
source_session: 01JZFJ3QK7Y2M8D4R6T1VNB5XC
---

Keep the session and refresh cookies on SameSite=Lax; do not move them to
Strict. We shipped Strict once on a scanner's "harden the cookies" finding and
it broke login for everyone arriving from an external link: Strict withholds
the cookie on the first cross-site top-level navigation, so a user clicking in
from an email, a partner site, or an OAuth callback lands logged out, re-auths,
and files a bug. Lax already blocks the CSRF vector that matters here. If a
scanner flags Lax, suppress that rule or harden elsewhere -- add the __Host-
prefix, keep HttpOnly and Secure, shorten the token TTL -- but never set these
cookies to Strict.
```

Note what the file gives the agent beyond the rule itself: the incident that
created it and the approved alternatives. That is why the with-side does not
just refuse - it proposes `__Host-` prefixes, TTL hardening, and a documented
scanner waiver, then asks which one you want. A briefing line is not
enforcement; you can still tell the agent to ship `Strict` anyway. What changes
is that the rule is in context at the moment of decision, with its evidence one
`memory_read` away, instead of in a document nobody's context window has ever
seen. [How memory works](/docs/concepts/memory/) covers constraints and the
rest of the lifecycle.

## How to reproduce this

Seed the fixture from the repo - the `myapp` project with this constraint and
the rest of its memory set:

```sh
git clone https://github.com/arctop/seamless && cd seamless
go run ./cmd/demoseed -scenes -data /tmp/seamless-demo -repo /path/to/your/test/repo
SEAMLESS_DATA_DIR=/tmp/seamless-demo go run ./cmd/seamlessd serve
```

Then hand your agent a plausible instruction that contradicts a recorded
constraint, with and without the [hooks](/docs/claude-code/) installed. The
`myapp` source was a small fictional Go auth service and is not committed; any
repo where a written rule and a written instruction can collide works. For a
real setup, start at the [quickstart](/docs/quickstart/).
