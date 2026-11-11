package main

// Settings > Backups' daemon side and its CLI: the startup step that finishes
// a staged restore, the backup service serve runs, `seamlessd backups` (the
// list), `seamlessd restore` (the console's no-passkey fallback, and the way
// back without a browser), and doctor's backups row.

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/archive"
	"github.com/arctop/seamless/internal/backup"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/update"
)

// restoreForceLocked is the config's force locks while peer_auth is on: only
// then does a lock hold them.
func restoreForceLocked(cfg config.Config) []string {
	if !cfg.PeerAuth.Enabled {
		return nil
	}
	return cfg.PeerAuth.ForceLocked
}

// restoreValidator is a restore's check of a prepared database under
// peer_auth: serve's own project preflight, which would otherwise refuse the
// start after the swap. nil without a policy.
func restoreValidator(policy *access.Policy) func(context.Context, *sql.DB) error {
	if policy == nil {
		return nil
	}
	return func(ctx context.Context, db *sql.DB) error {
		problems, err := projectProblems(ctx, db, policy)
		if err != nil {
			return err
		}
		if len(problems) > 0 {
			return errors.New(strings.Join(problems, "; "))
		}
		return nil
	}
}

// applyPendingRestore is serve's startup step for a staged restore, run once
// it holds the data dir lock and before anything opens the database. lockOK
// false (a file system that cannot lock) drops the restore unapplied: no swap
// is safe without the lock. An error stops serve: a swap that could not be
// rolled back, naming where today's data is.
func applyPendingRestore(ctx context.Context, cfg config.Config, policy *access.Policy, lockOK bool, logger *slog.Logger) error {
	if !backup.RestorePending(cfg.DataDir) {
		return nil
	}
	if !lockOK {
		res, err := backup.DropPending(cfg.DataDir, backup.RestoreReasonUnsupported,
			"the data folder's file system cannot be locked, so no restore can swap the data safely; nothing changed", time.Now())
		if err != nil {
			return fmt.Errorf("restore: %w", err)
		}
		if res != nil {
			logger.Warn("a staged restore was not applied: the data dir cannot be locked", "data_dir", cfg.DataDir)
		}
		return nil
	}
	a := backup.Applier{DataDir: cfg.DataDir, ForceLocked: restoreForceLocked(cfg), Validate: restoreValidator(policy), Log: logger}
	out, err := a.Apply(ctx)
	if err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	if r := out.Result; r != nil {
		if r.Outcome == backup.RestoreDone {
			logger.Info("restore applied", "mode", r.Mode, "backup", r.Backup.Name, "previous", r.PreviousDir)
		} else {
			logger.Warn("a staged restore was not applied", "outcome", r.Outcome, "reason", r.Reason, "detail", r.Message)
		}
	}
	return nil
}

// newBackupService builds the daemon's backup service. Nothing runs until
// Start, which runServe calls once the listener is bound. restart asks serve
// for a clean restart; it is wired only where serve can restart in place (not
// Windows, where a staged restore waits for the next start).
func newBackupService(cfg config.Config, db *sql.DB, rec backup.Recorder, policy *access.Policy, upd *update.Checker,
	restart func(), logger *slog.Logger) *backup.Service {
	d := backup.Deps{
		DataDir: cfg.DataDir, DB: db, Base: cfg.Backup,
		Override: func(ctx context.Context) (config.BackupOverride, error) {
			o, _, err := store.BackupOverride(ctx, db)
			return o, err
		},
		Release: update.IsReleaseBuild(distribution, version),
		Version: buildVersion(), Host: config.Hostname(),
		FreeBytes: freeBytes, Events: rec, Log: logger,
		UpdateBusy: func() bool {
			if held, err := updaterLockHeld(cfg.DataDir); err == nil && held {
				return true
			}
			return upd != nil && upd.Status().Applying != nil
		},
	}
	if runtime.GOOS != "windows" {
		d.RequestRestart = restart
	}
	if cfg.PeerAuth.Enabled {
		d.ForceLocked = cfg.PeerAuth.ForceLocked
		d.PasskeyRPID, _ = cfg.RelyingParty()
		d.Guard = &backup.Guard{EUID: os.Geteuid()}
		d.Validate = restoreValidator(policy)
	}
	return backup.New(d)
}

// restoreHolding is the update checker's hold while a restore is staged or
// under way.
func restoreHolding(dataDir string) func() string {
	return func() string {
		if backup.RestorePending(dataDir) {
			return "a restore is waiting to finish; updates wait until it has"
		}
		return ""
	}
}

// restorePendingUpdateError is the attended update's refusal while a restore
// waits.
func restorePendingUpdateError(dataDir string) error {
	return fmt.Errorf("seamlessd.update: a restore is waiting to finish (%s): restart Seamless to finish it, or cancel it in the console's Settings > Backups, then update",
		backup.PendingPath(dataDir))
}

// backupFolder is the folder `seamlessd backups` lists and the install id it
// tells this install's backups by: what the daemon last recorded (the
// console's choice included), else the config's.
func backupFolder(cfg config.Config) (dir, installID string) {
	dir, installID = backup.ReadFolderRecord(cfg.DataDir)
	if dir == "" {
		dir = backup.Effective(cfg.Backup, config.BackupOverride{}, update.IsReleaseBuild(distribution, version), cfg.DataDir).Dir
	}
	return dir, installID
}

// runBackups is `seamlessd backups [--json]`: the backups in the chosen folder
// and in <data_dir>/backups, newest first. It opens no database.
func runBackups(args []string) error {
	fs := flag.NewFlagSet("backups", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print the listing as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("seamlessd.backups: unexpected argument %q", fs.Arg(0))
	}
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("seamlessd.backups: %w", err)
	}
	if cfg.IsClient() {
		return errors.New("seamlessd.backups: role: client keeps no data here; list the backups on the server")
	}
	dir, id := backupFolder(cfg)
	l := backup.ListBackups(cfg.DataDir, dir, id)
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(l)
	}
	printListing(os.Stdout, l)
	return nil
}

// printListing renders a listing for a terminal.
func printListing(w io.Writer, l backup.Listing) {
	fmt.Fprintf(w, "folder: %s\n", l.Dir)
	if !l.DirCheck.Usable() {
		fmt.Fprintf(w, "        %s\n", l.DirCheck.Message())
	}
	if l.Local != "" {
		fmt.Fprintf(w, "also:   %s (safety and pre-update backups)\n", l.Local)
	}
	if len(l.Entries) == 0 {
		fmt.Fprintln(w, "\nno backups yet")
		return
	}
	fmt.Fprintln(w)
	for _, e := range l.Entries {
		fmt.Fprintf(w, "%s  %-16s %9s  %s\n", e.CreatedAt.Local().Format(timeLayout), kindWord(e), sizeWord(e.Size), e.Name)
		if what := holdsWord(e); what != "" {
			fmt.Fprintf(w, "%s  %s\n", strings.Repeat(" ", len(timeLayout)), what)
		}
	}
}

// kindWord names why a backup was written, as the console does.
func kindWord(e backup.Entry) string {
	switch e.Kind {
	case archive.KindScheduled:
		return "automatic"
	case archive.KindManual:
		return "made by you"
	case archive.KindBeforeRestore:
		return "before restore"
	case archive.KindPreUpdate:
		if e.Parsed.FromVersion != "" {
			return "before v" + e.Parsed.FromVersion + " update"
		}
		return "before update"
	}
	return "exported"
}

// holdsWord says what a backup holds, or why it cannot be restored.
func holdsWord(e backup.Entry) string {
	var parts []string
	switch {
	case e.Dataless:
		parts = append(parts, "in iCloud: download it in Finder first")
	case e.Unreadable != "":
		parts = append(parts, e.Unreadable)
	case e.TooNew:
		parts = append(parts, "made by a newer Seamless: update to restore it")
	}
	if m := e.Manifest; m != nil {
		var mem, notes, sessions int
		for _, p := range m.Projects {
			mem += p.Memories
			notes += p.Notes
			sessions += p.Sessions
		}
		if len(m.Projects) == 0 {
			mem, notes = m.Counts.MemoryFiles, m.Counts.NoteFiles
		}
		holds := fmt.Sprintf("%d memories, %d notes", mem, notes)
		if n := len(m.Projects) + m.ProjectsOmitted; n > 0 {
			holds += fmt.Sprintf(", %d projects", n)
		}
		if sessions > 0 {
			holds += fmt.Sprintf(", %d sessions", sessions)
		}
		if !m.IncludesDB {
			holds += " (memories and notes only)"
		}
		parts = append(parts, holds)
	}
	if e.FromAnother {
		host := "another machine"
		if e.Manifest != nil && e.Manifest.SourceHost != "" {
			host = e.Manifest.SourceHost
		}
		parts = append(parts, "from another Seamless on "+host)
	}
	return strings.Join(parts, "; ")
}

// sizeWord is a file size for the list.
func sizeWord(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// restoreWaitFor bounds how long `seamlessd restore` waits for a running
// daemon to pick a staged restore up, restart and finish it.
const restoreWaitFor = 3 * time.Minute

// restoreDeps is what runRestore reads and runs, injected for tests.
type restoreDeps struct {
	in          io.Reader
	out         io.Writer
	interactive bool
	euid        int
	inspect     func(string) (fileFacts, error)
	// restartService restarts the installed service (Windows, which cannot
	// restart in place).
	restartService func() error
	waitFor, poll  time.Duration
	now            func() time.Time
}

func realRestoreDeps() restoreDeps {
	return restoreDeps{
		in: os.Stdin, out: os.Stdout, interactive: stdinIsTerminal(), euid: os.Geteuid(), inspect: inspectPath,
		restartService: func() error { return runServiceAction(actionRestart, nil) },
		waitFor:        restoreWaitFor, poll: 500 * time.Millisecond, now: time.Now,
	}
}

// runRestore is `seamlessd restore <file|name> [--merge] [--yes]`.
func runRestore(args []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return restoreCmd(ctx, args, realRestoreDeps())
}

// restoreUsage is the command's usage.
const restoreUsage = `seamlessd restore <file|name> [--merge] [--yes]

Puts a backup back. <name> is a backup's file name as seamlessd backups lists
it; <file> is any backup file. Without --merge the backup replaces all data,
and today's is saved as a backup first ("before restore"), so the restore can
be undone. With --merge only what the backup has and this install lacks is
added; nothing here changes. Locks, passkeys, project privacy, repo routes and
console settings stay as they are now.

With the daemon running, the restore is prepared here and the daemon restarts
to finish it; with none, it is applied here. Under peer_auth, run it as the
daemon's account.`

// restoreCmd is runRestore's testable core.
func restoreCmd(ctx context.Context, args []string, d restoreDeps) error {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	merge := fs.Bool("merge", false, "only bring back what is missing; change nothing here")
	yes := fs.Bool("yes", false, "restore without asking")
	var target string
	rest := args
	for len(rest) > 0 {
		if err := fs.Parse(rest); err != nil {
			return fmt.Errorf("seamlessd.restore: %w\n\n%s", err, restoreUsage)
		}
		rest = fs.Args()
		if len(rest) > 0 {
			if target != "" {
				return fmt.Errorf("seamlessd.restore: one backup at a time, got %q and %q", target, rest[0])
			}
			target, rest = rest[0], rest[1:]
		}
	}
	if target == "" {
		return fmt.Errorf("seamlessd.restore: name the backup to restore\n\n%s", restoreUsage)
	}
	mode := backup.ModeReplace
	if *merge {
		mode = backup.ModeMerge
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("seamlessd.restore: %w", err)
	}
	if cfg.IsClient() {
		return errors.New("seamlessd.restore: role: client keeps no data here; restore on the server")
	}
	// The database first: one behind this binary, or none yet, is refused
	// before anything else is looked at (the one-shot rule).
	db, err := openServedDB(cfg, os.Stderr)
	if err != nil {
		return fmt.Errorf("seamlessd.restore: %w", err)
	}
	dbOpen := true
	closeDB := func() error {
		if !dbOpen {
			return nil
		}
		dbOpen = false
		return db.Close()
	}
	// The direct apply closes the handle itself, checked, before the swap;
	// this covers every return before that.
	defer func() { _ = closeDB() }() //nolint:errcheck // a read handle closed on an early return; nothing was written through it that a close could lose

	var policy *access.Policy
	proof := "owner"
	if cfg.PeerAuth.Enabled {
		if err := restoreAccountCheck(cfg, d); err != nil {
			return fmt.Errorf("seamlessd.restore: %w", err)
		}
		if policy, err = access.New(cfg.PeerAuth, access.LookupAccount); err != nil {
			return fmt.Errorf("seamlessd.restore: peer_auth: %w", err)
		}
		proof = "account"
	}

	if held, herr := updaterLockHeld(cfg.DataDir); herr == nil && held {
		return errors.New("seamlessd.restore: an update is running; restore once it has finished")
	}
	if backup.RestorePending(cfg.DataDir) {
		return fmt.Errorf("seamlessd.restore: another restore is waiting to finish (%s): restart Seamless to finish it, or cancel it in the console",
			backup.PendingPath(cfg.DataDir))
	}
	installID, err := store.InstallID(ctx, db)
	if err != nil {
		return fmt.Errorf("seamlessd.restore: %w", err)
	}
	path, ref, err := resolveRestoreTarget(cfg, target, installID)
	if err != nil {
		return fmt.Errorf("seamlessd.restore: %w", err)
	}
	m, err := readBackupManifest(path)
	if err != nil {
		return fmt.Errorf("seamlessd.restore: %s is not a backup Seamless can read: %w", path, err)
	}
	if mode == backup.ModeReplace && !m.IncludesDB {
		return errors.New("seamlessd.restore: this backup holds memories and notes only, not the database, so it cannot replace everything: pass --merge to bring back what is missing")
	}
	describeRestore(d.out, path, m, mode)
	if !*yes {
		if !d.interactive {
			return errors.New("seamlessd.restore: confirm on a terminal, or pass --yes")
		}
		if !confirmRestore(d.in, d.out, mode) {
			return errors.New("seamlessd.restore: not restored")
		}
	}

	// A running daemon holds the data dir lock: it finishes the restore at
	// its restart. With none, this process takes the lock and applies it.
	lock, lerr := tryLockFile(filepath.Join(cfg.DataDir, dataDirLockName))
	switch {
	case errors.Is(lerr, errLockHeld):
	case errors.Is(lerr, errLockUnsupported):
		return fmt.Errorf("seamlessd.restore: %s cannot be locked on this file system, so no restore can swap the data safely: %w", cfg.DataDir, lerr)
	case lerr != nil:
		return fmt.Errorf("seamlessd.restore: %w", lerr)
	default:
		defer func() { _ = lock.Close() }()
	}

	st := backup.Stager{
		DataDir: cfg.DataDir, DB: db, ForceLocked: restoreForceLocked(cfg), Validate: restoreValidator(policy),
		FreeBytes: freeBytes, Version: buildVersion(), Host: config.Hostname(),
		Log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Step: func(step string) { fmt.Fprintf(d.out, "  %s\n", stepWord(step)) },
	}
	if cfg.PeerAuth.Enabled {
		st.PasskeyRPID, _ = cfg.RelyingParty()
	}
	p, err := st.Stage(ctx, backup.StageRequest{Path: path, Backup: ref, Mode: mode, RequestedBy: "cli", Proof: proof})
	if err != nil {
		return fmt.Errorf("seamlessd.restore: %w", err)
	}

	if lock == nil {
		return finishThroughDaemon(ctx, cfg, p, d)
	}
	if err := closeDB(); err != nil {
		return fmt.Errorf("seamlessd.restore: %w", err)
	}
	fmt.Fprintf(d.out, "  %s\n", "applying")
	a := backup.Applier{DataDir: cfg.DataDir, ForceLocked: restoreForceLocked(cfg), Validate: restoreValidator(policy),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	out, err := a.Apply(ctx)
	if err != nil {
		return fmt.Errorf("seamlessd.restore: %w", err)
	}
	res := out.Result
	if res != nil && res.NeedsArchive() {
		fmt.Fprintf(d.out, "  %s\n", "saving your previous data as a backup")
		ar := backup.Archiver{DataDir: cfg.DataDir, Version: buildVersion(), Host: config.Hostname(), InstallID: installID,
			ForceLocked: restoreForceLocked(cfg), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
		if cfg.PeerAuth.Enabled {
			ar.PasskeyRPID, _ = cfg.RelyingParty()
		}
		if got, aerr := ar.ArchivePrevious(ctx); got != nil {
			res = got
		} else if aerr != nil {
			fmt.Fprintf(d.out, "  warning: %v\n", aerr)
		}
	}
	return reportRestore(d.out, res)
}

// restoreAccountCheck refuses a restore under peer_auth unless it runs as the
// daemon's own account (the data dir's owner), never root: the account is
// the proof, as for the terminal approval.
func restoreAccountCheck(cfg config.Config, d restoreDeps) error {
	if d.euid == 0 {
		return errors.New("run it as the daemon's account (sudo -u <account> seamlessd restore ...), never as root")
	}
	facts, err := d.inspect(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("data_dir %s: %w (run this as the daemon's account)", cfg.DataDir, err)
	}
	if int(facts.uid) != d.euid {
		return fmt.Errorf("this is not the daemon's account (data_dir %s belongs to uid %d): run it as the daemon's account (sudo -u <account> seamlessd restore ...)",
			cfg.DataDir, facts.uid)
	}
	return nil
}

// resolveRestoreTarget finds the backup target names: a file, or a backup's
// name in the listing.
func resolveRestoreTarget(cfg config.Config, target, installID string) (string, backup.BackupRef, error) {
	// A path: anything with a separator or a leading ~, or a file here.
	if strings.ContainsAny(target, `/\`) || strings.HasPrefix(target, "~") {
		p, err := expandHome(target)
		if err != nil {
			return "", backup.BackupRef{}, err
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			return "", backup.BackupRef{}, err
		}
		return abs, backup.BackupRef{Name: filepath.Base(abs)}, nil
	}
	if fi, err := os.Stat(target); err == nil && fi.Mode().IsRegular() {
		abs, err := filepath.Abs(target)
		if err != nil {
			return "", backup.BackupRef{}, err
		}
		return abs, backup.BackupRef{Name: filepath.Base(abs)}, nil
	}
	dir, _ := backupFolder(cfg)
	l := backup.ListBackups(cfg.DataDir, dir, installID)
	for _, e := range l.Entries {
		if e.Name == target || e.Ref == target {
			if f := backup.Restorable(e, backup.ModeMerge); f != nil {
				return "", backup.BackupRef{}, f
			}
			return e.Path, backup.BackupRef{Name: e.Name, Where: e.Where, Kind: e.Kind}, nil
		}
	}
	return "", backup.BackupRef{}, fmt.Errorf("no backup named %q in %s or %s: seamlessd backups lists them, or name the file's path",
		target, dir, backup.LocalDir(cfg.DataDir))
}

// readBackupManifest reads a backup's manifest, refusing one this build cannot
// restore.
func readBackupManifest(path string) (archive.Manifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return archive.Manifest{}, err
	}
	defer func() { _ = f.Close() }()
	m, err := archive.ReadManifest(f)
	if err != nil {
		return m, err
	}
	if m.FormatVersion > archive.FormatVersion || m.SchemaVersion > store.LatestSchemaVersion() {
		return m, errors.New("a newer Seamless made it: update Seamless to restore it")
	}
	return m, nil
}

// describeRestore says what the restore will do.
func describeRestore(w io.Writer, path string, m archive.Manifest, mode string) {
	e := backup.Entry{Manifest: &m, Kind: m.Kind}
	fmt.Fprintf(w, "backup:  %s\n", path)
	fmt.Fprintf(w, "made:    %s on %s by Seamless %s (%s)\n", m.CreatedAt.Local().Format(timeLayout), m.SourceHost, m.SeamlessdVersion, kindWord(e))
	fmt.Fprintf(w, "holds:   %s\n", holdsWord(e))
	if mode == backup.ModeMerge {
		fmt.Fprintln(w, "restore: only bring back what is missing: nothing here changes")
	} else {
		fmt.Fprintf(w, "restore: replace all data with this backup; today's is saved as a backup first, so you can undo this\n")
	}
	fmt.Fprintln(w, "kept:    locks, passkeys, project privacy, repo routes and console settings stay as they are now")
}

// confirmRestore asks on the terminal.
func confirmRestore(in io.Reader, out io.Writer, mode string) bool {
	q := "Replace all data with this backup?"
	if mode == backup.ModeMerge {
		q = "Bring back what this backup has and this install is missing?"
	}
	fmt.Fprintf(out, "\n%s [y/N] ", q)
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}

// stepWord is a staging step as the progress line says it.
func stepWord(step string) string {
	switch step {
	case backup.StepChecking:
		return "checking the backup"
	case backup.StepPreparing:
		return "preparing the restored data"
	case backup.StepSaving:
		return "saving your current data"
	}
	return step
}

// finishThroughDaemon hands a staged restore to the running daemon: it
// notices the record and restarts (Windows: the service is restarted here),
// then this waits for its result.
func finishThroughDaemon(ctx context.Context, cfg config.Config, p *backup.Pending, d restoreDeps) error {
	if runtime.GOOS == "windows" {
		fmt.Fprintf(d.out, "  %s\n", "restarting Seamless")
		if err := d.restartService(); err != nil {
			return fmt.Errorf("seamlessd.restore: the restore is staged, but Seamless could not be restarted (%w): restart it within an hour to finish (seamlessd restart)", err)
		}
	} else {
		fmt.Fprintf(d.out, "  %s\n", "waiting for Seamless to restart and finish")
	}
	deadline := d.now().Add(d.waitFor)
	t := time.NewTicker(d.poll)
	defer t.Stop()
	for {
		if res, err := backup.ReadResult(cfg.DataDir); err == nil && res != nil && res.ID == p.ID {
			return reportRestore(d.out, res)
		}
		if d.now().After(deadline) {
			return fmt.Errorf("seamlessd.restore: the restore is staged, but Seamless has not finished it yet: restart it within an hour to finish (seamlessd restart); the result shows in Settings > Backups")
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("seamlessd.restore: stopped waiting; the restore is staged and finishes when Seamless restarts (within an hour): %w", ctx.Err())
		case <-t.C:
		}
	}
}

// reportRestore prints a restore's result, and fails unless it was restored.
func reportRestore(w io.Writer, res *backup.Result) error {
	if res == nil {
		return errors.New("seamlessd.restore: the restore left no result")
	}
	if res.Outcome != backup.RestoreDone {
		return fmt.Errorf("seamlessd.restore: the restore did not finish: %s", res.Message)
	}
	when := res.Backup.CreatedAt.Local().Format(timeLayout)
	if res.Mode == backup.ModeMerge {
		fmt.Fprintf(w, "\nbrought back what the backup from %s had and this install was missing: %d items added\n", when, res.Added)
	} else {
		fmt.Fprintf(w, "\nrestored the backup from %s (%d memories, %d notes)\n", when, res.Memories, res.Notes)
	}
	switch {
	case res.Previous != "":
		fmt.Fprintf(w, "your previous data is saved as %s; restore it to undo this\n", filepath.Join("backups", res.Previous))
	case res.PreviousDir != "":
		msg := "your previous data is in " + res.PreviousDir + " until it is saved as a backup"
		if res.PreviousError != "" {
			msg += " (" + res.PreviousError + ")"
		}
		fmt.Fprintln(w, msg)
	}
	return nil
}

// backupsCheck is doctor's backups row: the settings, how current the backups
// are, and a restore that is staged, stopped, or left data behind.
func backupsCheck(ctx context.Context, cfg config.Config, db *sql.DB, now time.Time) check {
	const name = "backups"
	if p, err := backup.ReadPending(cfg.DataDir); err != nil {
		return check{statusWarn, name, fmt.Sprintf("cannot read the staged restore's record: %v", err)}
	} else if p != nil {
		switch p.Stage {
		case backup.StageSwapping:
			return check{statusWarn, name, fmt.Sprintf("a restore stopped part way through its swap (%s); the next start undoes it", backup.PendingPath(cfg.DataDir))}
		case backup.StageApplied:
			return check{statusInfo, name, "a restore is applied; the next start finishes its bookkeeping"}
		case backup.StageMerging:
			return check{statusWarn, name, fmt.Sprintf("a merge stopped part way (attempt %d); the next start runs it again", p.Attempts)}
		default:
			return check{statusInfo, name, fmt.Sprintf("a restore of %s is staged since %s and applies when Seamless restarts (it expires an hour after it was staged)",
				p.Backup.Name, p.RequestedAt.Local().Format(timeLayout))}
		}
	}
	res, err := backup.ReadResult(cfg.DataDir)
	if err != nil {
		return check{statusWarn, name, fmt.Sprintf("cannot read the last restore's result: %v", err)}
	}
	if res.NeedsArchive() {
		detail := "the data the last restore replaced is still in " + res.PreviousDir + ", waiting to be saved as a backup"
		if res.PreviousError != "" {
			detail += ": " + res.PreviousError
		}
		return check{statusWarn, name, detail}
	}
	if left := leftoverRestores(cfg.DataDir, res); len(left) > 0 {
		return check{statusWarn, name, "a restore left data in " + strings.Join(left, ", ") + ": check it holds nothing you need, then remove it"}
	}

	var o config.BackupOverride
	var oerr error
	if db != nil {
		o, _, oerr = store.BackupOverride(ctx, db)
	}
	set := backup.Effective(cfg.Backup, o, update.IsReleaseBuild(distribution, version), cfg.DataDir)
	note := ""
	if oerr != nil {
		note = fmt.Sprintf(" (the console's backup settings cannot be read: %v)", oerr)
	}
	if res != nil && !res.Dismissed && res.Outcome != backup.RestoreDone {
		return check{statusWarn, name, fmt.Sprintf("the last restore did not finish (%s): %s", res.Outcome, res.Message)}
	}
	if !set.Auto {
		return check{statusInfo, name, "automatic backups are off; turn them on in the console (Settings > Backups), or back up with seamlessd export" + note}
	}
	sum, serr := backup.ReadSchedule(cfg.DataDir)
	if serr != nil {
		return check{statusWarn, name, fmt.Sprintf("cannot read the schedule's record: %v", serr)}
	}
	every := update.SpanWords(set.Every)
	base := fmt.Sprintf("automatic backups every %s, keeping %d, in %s", every, set.Keep, set.Dir)
	switch {
	case sum.Failures >= 2:
		return check{statusWarn, name, fmt.Sprintf("%s: the last %d failed, the first at %s: %s", base, sum.Failures,
			sum.FailingSince.Local().Format(timeLayout), sum.LastError)}
	case sum.LastConfirmed.IsZero():
		return check{statusInfo, name, base + "; none made yet" + note}
	case now.Sub(sum.LastConfirmed) > 3*set.Every:
		return check{statusWarn, name, fmt.Sprintf("%s: nothing has confirmed them current since %s; is the daemon running? (seamlessd status)",
			base, sum.LastConfirmed.Local().Format(timeLayout))}
	}
	detail := base + "; last confirmed " + sum.LastConfirmed.Local().Format(timeLayout)
	if sum.LastBackup != "" {
		detail += " (" + sum.LastBackup + ")"
	}
	return check{statusOK, name, detail + note}
}

// leftoverRestores lists restore/<id>/ folders that hold replaced data no
// record accounts for.
func leftoverRestores(dataDir string, res *backup.Result) []string {
	des, err := os.ReadDir(backup.RestoreRoot(dataDir))
	if err != nil {
		return nil
	}
	var out []string
	for _, de := range des {
		if !de.IsDir() || (res != nil && res.ID == de.Name()) {
			continue
		}
		prev := filepath.Join(backup.RestoreRoot(dataDir), de.Name(), "previous")
		if held, err := os.ReadDir(prev); err == nil && len(held) > 0 {
			out = append(out, prev)
		}
	}
	return out
}
