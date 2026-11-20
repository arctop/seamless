---
id: 01M4GMHTKQQSH9SQQ2RAKD5HBS
kind: gotcha
name: github-releases-latest-is-not-max-version
description: "GitHub /releases/latest follows created_at/make_latest, not semver: a backport can be \"latest\". Pick the max version over the release list."
project: seamless
created: 2026-10-09T15:29:49Z
invalid_at: null
superseded_by: null
source_session: 01M4GM7FZR3W5KHMNZE67XWCNE
tags: [github, update, release]
---
GitHub's "latest release" (`GET /repos/{owner}/{repo}/releases/latest`, and the `releases/latest/download/<asset>` redirect) is the most recently CREATED non-draft, non-prerelease release unless `make_latest` says otherwise. It is not the highest version: cut v0.5.9 as a backport after v0.6.0 and "latest" becomes v0.5.9, so anything that reads it as "newest" would downgrade.

Rule for Seamless update code: list releases (`/releases?per_page=20`), drop drafts, prereleases, non-strict `vX.Y.Z` tags and releases whose assets are still uploading, then take the max by semver (`update.Newest`). The manual `seamlessd update`, console "Update now" and auto-update all target that same selector and pin `SEAMLESS_VERSION` explicitly; none of them may rely on `/releases/latest` or `releases/latest/download`.

Context: plan:plan-background-update-check-automatic-updates-claude-code-style, [[auto-update-architecture-and-policy]].
