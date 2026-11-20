---
id: 01M71QPJ9J7XW2XJ1CDBCRVJ8E
kind: gotcha
name: branch-seamlessd-without-config-migrates-live-db
description: A branch seamlessd run with no SEAMLESS_CONFIG opens and migrates the live DB, even on a bad argument. Live DB went to v36 this way on 2026-11-09.
project: seamless
created: 2026-11-10T03:23:19Z
invalid_at: null
superseded_by: null
source_session: 01M71Q2PZXSSM459Q7S5HF3GHC
model: claude-opus-5-5
tags: [store, migrations, live-state, plan:seamless-sealed-unlock]
---
Most `seamlessd` subcommands call store.Open, which runs migrations, against whatever the default config names (~/.config/seamless/seamless.yaml, so ~/.seamless/seam.db). A dev or branch build run from a repo or worktree with no SEAMLESS_CONFIG therefore migrates the live database under the running daemon. `seamlessd approvers <verb> --help` counts: it opens the database before it validates its arguments.

**How to apply:** always run a built seamlessd with SEAMLESS_CONFIG pointing at a scratch config, even to read its help, and prefer `/usr/bin/env -i HOME=<scratch> PATH=/usr/bin:/bin SEAMLESS_CONFIG=<scratch config>`. Read flags from the source instead when you can. To inspect the live database use `sqlite3 "file:$HOME/.seamless/seam.db?mode=ro"`. Code fix is filed under plan seamless-sealed-unlock.

**Incident, 2026-11-09 03:15:45Z (session cc/7238e932):** a locked-projects build applied migrations 29-36 to the live ~/.seamless/seam.db via `approvers enroll-code --help`. The live daemon (commit 13444ec) compiles only up to 28; main has up to 30; 31-36 exist only on locked-projects.

State after it, checked read-only: additive only (new empty tables; new columns that are nullable or NOT NULL DEFAULT ''), no rows changed or removed, no positional INSERT or SELECT * on the altered tables at 13444ec. The daemon kept serving and task writes at 03:20:51Z landed. A daemon restart on this schema was NOT tested; at 13444ec migrate() skips versions at or below the applied maximum and has no refusal for a newer schema.

Consequences while it holds:
- `seamlessd doctor` from an older binary warns that the schema is ahead.
- Archives taken from this database record schema v36, and a binary compiled below that refuses to restore them, by the newer-schema refusal in internal/store/migrate.go.
- Do NOT add a migration on main numbered 31-36 before locked-projects merges without repairing this database first: migrate() would skip it here. Renumbering the branch's migrations would re-run its CREATE TABLEs against tables that already exist and break startup.
- Editing migrations 31-36 on the branch before merge will not reach this database.

Open: the owner decides whether to leave it (it matches once locked-projects ships) or roll back, which means stopping the daemon and hand-reversing the DDL. Update this memory when that is settled.
**Update 2026-11-09:** commit cb8f246a on `locked-projects` fixes the approval commands: `approvers`, `approve` and `console-open --unlock` no longer create or migrate, and `approvers` checks its arguments before opening anything. Builds without that commit still behave as above. `doctor` (so `make doctor`), `family`, `map-repo`, `unmap-repo`, `retire-project` and `import` still migrate, so the SEAMLESS_CONFIG rule stands. The live database is still at v36 and the owner's decision is still open.

**Update 2026-11-09 19:51 PST (cc/863415c2):** the live daemon now runs a locked-projects build (0.8.0+cb8f246a, compiled to v36), so daemon and live DB match and a restart on v36 has been exercised. The owner's leave-or-roll-back decision is moot while that build stays installed. The warning against adding migrations 31-36 on main still stands. See [[live-runs-local-v0-8-0-from-locked-projects]].

**Update 2026-11-09 20:14 PST (cc/863415c2):** locked-projects is merged into local main at 89d7e931, so migrations 31-36 are on main and the warning against numbering new ones there no longer applies. The next migration is 037. The SEAMLESS_CONFIG rule still stands for `doctor`, `family`, `map-repo`, `unmap-repo`, `retire-project` and `import`.

**Update 2026-11-09 (cc/b343cc0e):** local commit 9887fc44 stops `doctor` (so `make doctor`), `family`, `map-repo`, `unmap-repo` and `retire-project` from migrating an existing database; `doctor` also creates nothing. `map-repo` still creates an absent database on a server install. Only `serve` and `import` still migrate. The SEAMLESS_CONFIG rule stands for those two and for any build older than that commit, and `map-repo` can still create a database at the default path.
