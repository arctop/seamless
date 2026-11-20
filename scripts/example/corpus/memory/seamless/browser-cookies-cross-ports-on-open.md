---
id: 01M73VD5E7WR8ZBTD4QCD2XC9A
kind: gotcha
name: browser-cookies-cross-ports-on-open
description: "Browsers send a host's cookies to every port of that host, SameSite=Strict included, when a program runs `open <url>`: confirmed in Safari, Chrome…"
project: seamless
created: 2026-11-10T23:06:34Z
invalid_at: null
superseded_by: null
source_session: 01M73TNY2Z08Z0XZBE581VWG8Z
---
Measured 2026-11-10 (task 01M73SVP8HWD6KPF3ZC29RG6CT).
- **Safari:** a cookie set by localhost:18081 (Path=/console, HttpOnly) reached localhost:19999/console/x, both its Lax and its Strict copies.
- **Chrome and Firefox:** the live console's seamless_console cookie reached the other port the same way.
- **Path still scopes:** /elsewhere received nothing.

**Why:** cookies are scoped by host and path, never by port. A navigation that another program starts with `open` counts as browser-initiated (Sec-Fetch-Site: none), so SameSite does not stop it.

**How to apply:** a cookie is never enough to bind a console grant to one browser on a machine where agents run as the owner. Any of them can listen on another port and `open` the browser at it. Bind a console session or unlock to origin-scoped state instead, such as a non-extractable WebCrypto key in IndexedDB (scheme, host and port). Related: [[console-unlock-begun-by-the-browser-it-opens]].

Applied to console unlocks in commit 0ce83987 (internal/console/browserproof, static/browserkey.js, static/sw.js; AGENTS.md "A cookie never names a browser alone"). […]
