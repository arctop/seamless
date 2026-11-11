// Package backup is Settings > Backups' service: scheduled backups of the
// whole instance into one folder the owner chooses, the list of the backups
// there, and the commands the console and the CLI call.
//
// What it does:
//
//   - Settings (settings.go): the file/env backup: block with the console's
//     override layered on, and where each value came from. Unset auto is on
//     for a release build and off for a build from source, so a throwaway
//     daemon never writes backups.
//   - Names (names.go): seamless-<host>-<stamp>[-auto|-before-restore].tar.gz,
//     parsed from the right; the updater's pre-update-v<version>-<stamp>.tar.gz
//     is recognized.
//   - Folders (folder.go): a check that only looks (the console's live check
//     while the owner types), a prepare that makes the folder and probes it
//     with a write (saving it, and every run), the data-dir guard that
//     answers "inside Seamless's data folder" before any other probe, macOS's
//     privacy refusal told apart from a permission one, a disconnected disk
//     never written under, and peer_auth's ownership rule (Guard).
//   - The list (list.go): the chosen folder plus <data_dir>/backups, each
//     backup's manifest read once (cached by path, size and time), iCloud
//     placeholders listed from their names and never opened.
//   - Runs (job.go): one at a time. The database snapshot stages under
//     <data_dir>/backups/.staging-*, never on the destination disk; the
//     archive goes to a dot-prefixed temp file there, written through a
//     writer that stops when the daemon does, then synced, renamed, and read
//     back. A run needs the backup's size free on each disk plus a reserve
//     (the larger of 1 GiB and the backup), so the updater can always take
//     its own backup. Pruning removes only this install's automatic backups
//     past the newest Keep -- never one the owner made, a safety or
//     pre-update backup, or another install's in a shared folder.
//   - The scheduler (service.go): one goroutine owns the schedule's record
//     (<data_dir>/backups/.schedule.json, so a restart keeps the failure
//     streak), ticks each minute against the wall clock, runs the first
//     scheduled backup no sooner than ten minutes after start, skips a run
//     when the instance is empty or nothing changed since this install's
//     newest backup in the folder (a skip confirms the backups current), and
//     notices a restore the CLI staged (restore/pending.json) every two
//     seconds. Wait is bounded, so a run stalled on a slow disk never holds
//     the daemon's shutdown.
//   - Restore (restore*.go): Stager prepares a restore while the daemon
//     serves (the backup verified and copied under <data_dir>/restore/<id>/,
//     a replacement imported fresh beside today's data with today's locks
//     kept and checked, a merge's safety backup of today's data taken) and
//     records restore/pending.json; Applier finishes it at the next start,
//     under the data dir lock and before anything opens the database: a
//     replacement through a write-ahead swap record (staged, swapping,
//     applied = the commit point) rolled back entry by entry from wherever a
//     stop left it, a merge idempotently, at most three times. Archiver turns
//     the replaced data into a before-restore backup once the daemon serves,
//     reading it back before it removes the original: the owner's undo.
//     Today's locks, approvers, kept settings and repo routes survive every
//     restore, and nothing under restore/ is deleted before its data is
//     verified elsewhere.
//
// The package is a domain package over archive, store, events (through the
// Recorder interface), core and config. The disks' free space, the restart,
// the clock and the tickers are injected; cmd/seamlessd wires them.
package backup
