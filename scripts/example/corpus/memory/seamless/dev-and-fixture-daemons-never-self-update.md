---
id: 01M4GMJ801VC4BNBZD9FRQGJ6C
kind: constraint
name: dev-and-fixture-daemons-never-self-update
description: Only seamlessd builds goreleaser stamps distribution=release may check GitHub by default, write update state, or apply updates; never stamp it…
project: seamless
created: 2026-10-09T15:30:02Z
invalid_at: null
superseded_by: null
source_session: 01M4GM7FZR3W5KHMNZE67XWCNE
tags: [update, build, fixture]
---
The background updater must never touch a developer's build, a fixture/seambench arm, `make run`, CI, demoseed or the Glama container. The guard is the build stamp `main.distribution`:

- default `"source"` in cmd/seamlessd/buildinfo.go; ONLY `.goreleaser.yaml` adds `-X main.distribution=release`, and only for seamlessd. Never add that flag to the Makefile, the fixture harness, deploy/glama or any test build.
- `make install` stamps the nearest TAG as `main.version`, so a dev build reads as a clean release by version alone. That is why the stamp, not the version, decides.
- A source build: `update.check` unset means OFF (no GitHub traffic); it never writes `<data_dir>/update/state.json`, never records `update.applied`/`update.available`, and is never kind Installer, so it can never auto-apply even with `check: true`.

Why: a dev or fixture daemon pointed at a live data dir would otherwise emit a false "updated" event, move `last_run_version`, or -- with Phase 2 -- replace the developer's build with the published release. Context: [[auto-update-architecture-and-policy]].
