package main

// setup's import step (plan networked-relay): a first install of a Standalone
// computer or of the Server starts fresh, or from a backup -- the archive
// `seamlessd export` writes, or one Settings > Backups made -- restored into
// the empty data dir exactly as `seamlessd import --from <backup>` restores
// one there (archiveImportOptions), its lock gate (import_locks.go) included.
// A Client never runs it: its data is its Server's.
//
// When it runs. The step is a first install's (setupStep.fresh), in setup's
// first half: before the installer installs anything, and so before the
// daemon it starts has created a database in the data dir, which is then sure
// to be empty. Setup refuses the import, and says why, everywhere else: on a
// computer already set up, into a data dir that holds a database or a memory
// or note file, while another daemon answers where this one will, and when
// this run's environment moves the data dir away from the one that daemon
// reads.
//
// The order with peer_auth: the import first, as the owner, into the owner's
// data dir; locked projects after. Turning them on is an after step (the
// peer_auth setup engine; by hand until it is built, the peer-auth guide's
// "Move an existing install over"), which backs the data dir up and then
// renames it to the daemon's account, so what the import restored -- the
// backup's locks, fences and passkeys -- moves over unchanged. Importing as
// the daemon's account instead needs that account, and root, before there is
// a data dir for it: the engine's own steps. Until locked projects are on
// nothing enforces a lock, and setup says so of every lock the import keeps.
//
// Unattended means unchanged: with no SEAMLESS_IMPORT and no person to ask,
// the step does nothing and says nothing. With SEAMLESS_IMPORT and no person to
// ask, the gate decides as it does for `seamlessd import` without a terminal:
// locks with a passkey able to open them are kept, and locks no passkey can
// open stop the run with nothing changed until SEAMLESS_IMPORT_LOCKS answers,
// as --keep-locks and --remove-locks answer the command.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/arctop/seamless/internal/archive"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/llm"
	"github.com/arctop/seamless/internal/store"
)

// SEAMLESS_IMPORT_LOCKS's values: keep the backup's locks (--keep-locks), or
// import its locked projects without them (--remove-locks).
const (
	setupImportKeep   = "keep"
	setupImportRemove = "remove"
)

// setupLockAnswers are the lock gate's two answers as setup takes them, for
// the refusal it makes when it cannot ask.
var setupLockAnswers = lockAnswers{
	keep:   "Set " + setupKnobImportLocks + "=" + setupImportKeep + " to import them locked",
	remove: setupKnobImportLocks + "=" + setupImportRemove + " to import them without their locks",
}

// setupWindowsLocks is what a lock means on a computer without peer_auth.
const setupWindowsLocks = "Windows has no peer_auth, so nothing enforces a lock on this computer: every agent here can read a locked project"

// setupImportLocks parses SEAMLESS_IMPORT_LOCKS: "" when it is unset, else keep
// or remove (case and surrounding space aside), else an error naming both.
func setupImportLocks(r *setupRun) (string, error) {
	raw := r.env(setupKnobImportLocks)
	if raw == "" {
		return "", nil
	}
	switch v := strings.ToLower(strings.TrimSpace(raw)); v {
	case setupImportKeep, setupImportRemove:
		return v, nil
	}
	return "", setupRefusal("invalid %s=%q: valid values are %s, %s", setupKnobImportLocks, raw, setupImportKeep, setupImportRemove)
}

// setupImportKnobs refuses SEAMLESS_IMPORT and SEAMLESS_IMPORT_LOCKS where
// setup cannot do what they ask, before anything is decided or written: a lock
// answer with no backup, a backup for a Client (its data is its Server's), a
// backup for a computer already set up (its data is there), and a path setup
// cannot read as one. Whether the file is a backup setup can start from is
// the step's to check, just before it starts.
func setupImportKnobs(r *setupRun, in setupInstall, want setupKind) error {
	locks, err := setupImportLocks(r)
	if err != nil {
		return err
	}
	raw := r.env(setupKnobImport)
	if raw == "" {
		if locks != "" {
			return setupRefusal("%s=%s says what an import does with a backup's locked projects, and %s names no backup: set both, or neither",
				setupKnobImportLocks, locks, setupKnobImport)
		}
		return nil
	}
	switch {
	case r.serverKnob() != "" || want == setupClient || in.kind == setupClient:
		return setupRefusal("%s: a {client} keeps no data of its own -- its data is its {server}'s -- so setup imports no backup on it; "+
			"start the {server} from the backup instead (%s on the {server}'s first install)", setupKnobImport, setupKnobImport)
	case in.kind != setupNone:
		return setupRefusal("%s: this computer is set up as %s already and has its data, and setup starts from a backup only on a first install, "+
			"into an empty data dir. To bring the backup into this one: seamlessd restore %s (today's data is saved first), or seamlessd restore --merge %s",
			setupKnobImport, setupDescribe(in.kind, in.serverURL, in.locked), raw, raw)
	}
	if _, problem := setupImportPath(r, raw); problem != "" {
		return setupRefusal("%s=%s: %s", setupKnobImport, raw, problem)
	}
	return nil
}

// setupImportStep is the import step (setupSteps["import"]): the backup
// SEAMLESS_IMPORT names, or the one a person at the terminal chooses, restored
// into the empty data dir. With neither it does nothing and says nothing.
func setupImportStep(r *setupRun, _ *setupState) error {
	var (
		path string
		v    archive.Verified
	)
	switch raw := r.env(setupKnobImport); {
	case raw != "":
		var problem string
		if path, v, problem = setupImportCheck(r, raw); problem != "" {
			return setupRefusal("%s=%s: %s", setupKnobImport, raw, problem)
		}
	case r.asks():
		var err error
		if path, v, err = setupAskImport(r); err != nil || path == "" {
			return err
		}
	default:
		return nil
	}
	return setupImportBackup(r, path, v)
}

// setupAskImport is the import step's question for a person at the terminal:
// start fresh (Enter), or from a backup, whose path it asks for next and
// checks, asking again until it names one setup can start from. "" is fresh.
func setupAskImport(r *setupRun) (string, archive.Verified, error) {
	fmt.Fprintf(r.out, "\n%s\n", bold("Start fresh, or from a backup?"))
	setupOption(r.out, 1, "Fresh", "an empty Seamless: your agents fill it as they work")
	setupOption(r.out, 2, "Backup", "a backup of another Seamless: its memories, notes, sessions and tasks, and its locks")
	n, err := setupChoose(r, "Enter 1 or 2 [1]: ", 1, [][]string{{"fresh"}, {"backup", "import"}})
	if err != nil || n == 1 {
		return "", archive.Verified{}, err
	}
	fmt.Fprintf(r.out, "  %s\n", dim("the .tar.gz that seamlessd export or Settings > Backups wrote: type its full path, or drop the file here"))
	for {
		fmt.Fprint(r.out, "Backup file: ")
		line, err := r.in.ReadString('\n')
		if answer := setupShellWord(r.goos, strings.TrimSpace(line)); answer != "" {
			path, v, problem := setupImportCheck(r, answer)
			if problem == "" {
				return path, v, nil
			}
			fmt.Fprintf(r.out, "  %s\n", problem)
		}
		if err != nil {
			return "", archive.Verified{}, setupRefusal("no backup: set %s to the backup's path for a run without questions", setupKnobImport)
		}
	}
}

// setupShellWord undoes what a terminal does to a path dropped on it: one pair
// of surrounding quotes, and on macOS and Linux a backslash before a character
// the shell would otherwise read. A path typed plainly comes back as it is.
func setupShellWord(goos, s string) string {
	if len(s) >= 2 && (s[0] == '\'' || s[0] == '"') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	if goos == "windows" || !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// setupImportPath resolves a backup's path as setup takes it: a leading ~ is
// the home, and anything else must be a full path, since setup runs in a
// directory of its own (the installers make a relative SEAMLESS_IMPORT
// absolute first, against the directory they were started in). It returns the
// path, or what is wrong with it.
func setupImportPath(r *setupRun, raw string) (string, string) {
	p := raw
	switch {
	case p == "-":
		return "", "- would read the backup from stdin, which setup does not; give the backup's path"
	case strings.ContainsFunc(p, setupControl):
		return "", "the path holds a control character"
	}
	if rest, ok := setupHomeRelative(r.goos, p); ok {
		if r.home == "" {
			return "", "there is no home directory to read ~ from; give the backup's full path"
		}
		p = filepath.Join(r.home, rest)
	}
	if !setupFullPath(r.goos, p) {
		if r.goos == "windows" {
			return "", `not a full path; give the backup's full path (from a drive, like C:\, or ~\)`
		}
		return "", "not a full path; give the backup's full path (from / or ~/)"
	}
	return p, ""
}

// setupHomeRelative reports whether p starts at the home (~, ~/, and on
// Windows ~\), and what follows it.
func setupHomeRelative(goos, p string) (string, bool) {
	switch {
	case p == "~":
		return "", true
	case strings.HasPrefix(p, "~/"), goos == "windows" && strings.HasPrefix(p, `~\`):
		return p[2:], true
	}
	return "", false
}

// setupFullPath reports whether p is a full path on goos: from the root on
// macOS and Linux; on Windows from a drive's root (C:\) or a share
// (\\host\share).
func setupFullPath(goos, p string) bool {
	if goos != "windows" {
		return strings.HasPrefix(p, "/")
	}
	if strings.HasPrefix(p, `\\`) {
		return true
	}
	return len(p) >= 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/') &&
		('a' <= p[0] && p[0] <= 'z' || 'A' <= p[0] && p[0] <= 'Z')
}

// setupImportCheck resolves raw to a backup's full path and reads the whole
// backup through the import's own guards (archive.Verify), writing nothing:
// the path and what the backup holds, or what is wrong with it in a line the
// owner can act on.
func setupImportCheck(r *setupRun, raw string) (string, archive.Verified, string) {
	path, problem := setupImportPath(r, raw)
	if problem != "" {
		return "", archive.Verified{}, problem
	}
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", archive.Verified{}, "there is no file at " + path
	case err != nil:
		return "", archive.Verified{}, err.Error()
	case info.IsDir():
		return "", archive.Verified{}, "that is a folder; a backup is one file, the .tar.gz that seamlessd export or Settings > Backups writes"
	case !info.Mode().IsRegular():
		return "", archive.Verified{}, "that is not a regular file"
	}
	f, err := os.Open(path)
	if err != nil {
		return "", archive.Verified{}, err.Error()
	}
	defer func() { _ = f.Close() }()
	v, err := archive.Verify(context.Background(), f, nil)
	switch {
	case err == nil:
		return path, v, ""
	case errors.Is(err, archive.ErrNotArchive):
		return "", archive.Verified{}, "that is not a Seamless backup: a backup is the .tar.gz that seamlessd export or Settings > Backups writes"
	case errors.Is(err, archive.ErrSchemaTooNew):
		return "", archive.Verified{}, "a newer Seamless made this backup than this release can read: install the latest release, or the one that made it"
	case errors.Is(err, archive.ErrIncomplete):
		return "", archive.Verified{}, "this backup is not whole: it was cut short, or changed after it was written (" + err.Error() + ")"
	}
	return "", archive.Verified{}, err.Error()
}

// setupImportBackup restores the checked backup at path (v is what it holds)
// into the data dir the config names, as seamlessd import --from path does
// there: it refuses a data dir it must not import into, decides the backup's
// locked projects with the import's own gate, and says what came of them.
func setupImportBackup(r *setupRun, path string, v archive.Verified) error {
	cfg, err := r.loadConfig()
	if err != nil {
		return fmt.Errorf("seamlessd.setup: import: %w", err)
	}
	if err := setupImportDestination(r, cfg); err != nil {
		return err
	}
	locks, err := setupImportLocks(r)
	if err != nil {
		return err
	}
	setupImportDescribe(r, path, v)

	gate := &lockGate{
		cfg:   cfg,
		flags: &importFlags{keepLocks: locks == setupImportKeep, removeLocks: locks == setupImportRemove},
		// The gate asks only the person setup may ask, on setup's terminal;
		// with no one to ask it decides as seamlessd import does without one.
		interactive: r.asks(), in: r.in, out: r.out, host: r.host, answers: setupLockAnswers,
		daemonUp: func() bool { return r.daemonUp(cfg) },
		local:    func(ctx context.Context) ([]store.PasskeyFact, error) { return localPasskeys(ctx, cfg) },
	}
	onLocks := gate.decide
	if m := v.Manifest; m.Locks != nil && len(m.Locks.Projects) > 0 {
		// Decided from the verified manifest before the import opens the file,
		// so a person answers with no interrupt handler holding Ctrl-C back,
		// and the import below can still be interrupted cleanly.
		if !setupPeerAuthOS(r.goos) {
			fmt.Fprintf(r.out, "\n%s\n", dim(setupWindowsLocks+"."))
		}
		choice, err := gate.decide(context.Background(), archive.ModeFresh, m)
		if err != nil {
			return setupImportStopped(r, cfg, path, err)
		}
		onLocks = func(_ context.Context, mode archive.Mode, read archive.Manifest) (archive.LockChoice, error) {
			switch {
			case mode != archive.ModeFresh:
				return archive.LocksKeep, fmt.Errorf("%s stopped being empty while setup imported into it; nothing was changed", cfg.DataDir)
			case read.Locks == nil || !slices.Equal(read.Locks.Slugs(), m.Locks.Slugs()):
				return archive.LocksKeep, errors.New("the backup changed while setup imported it, so the answer about its locked projects may not be its own; nothing was changed")
			}
			return choice, nil
		}
	}

	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("seamlessd.setup: import: %w", err)
	}
	defer func() { _ = f.Close() }()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	opts := archiveImportOptions(cfg, f, gate)
	opts.OnLocks = onLocks
	// The report is setup's rows below; a warning the import logs on its way
	// is still the owner's to see, as one of them.
	opts.Logger = slog.New(&setupWarnings{out: r.out})
	// As seamlessd import embeds by default, with the configured provider. A
	// first install has none configured yet, which is no warning here: the
	// backup's own vectors come with its database.
	if embedder, err := llm.NewEmbedder(cfg.LLM); err == nil {
		opts.Embedder = embedder
	}
	rep, err := archive.Import(ctx, opts)
	if err != nil {
		return setupImportFailed(cfg, path, err)
	}
	setupImportReport(r, rep)
	setupImportAdvice(r, cfg, gate, rep)
	return nil
}

// setupImportDestination refuses a data dir setup must not import into: one
// the daemon this install starts would not read (SEAMLESS_DATA_DIR moves only
// this run's), one that holds a database or a memory or note file already (an
// import there would be a merge, not a fresh start), and one another daemon
// answers for, as seamlessd import refuses a fresh restore while one does.
func setupImportDestination(r *setupRun, cfg config.Config) error {
	if d := r.env("SEAMLESS_DATA_DIR"); d != "" {
		return setupRefusal("SEAMLESS_DATA_DIR=%s: the daemon this install starts keeps its data where its config says, not where this run's environment points, "+
			"so the backup would land where it never looks; unset SEAMLESS_DATA_DIR", d)
	}
	mode, err := archive.DetectMode(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("seamlessd.setup: import: %w", err)
	}
	if mode != archive.ModeFresh {
		dir := tildePath(cfg.DataDir)
		return setupRefusal("%s holds Seamless data already (a database, or memory or note files), and setup starts from a backup only in an empty data dir: "+
			"move it aside (mv %s %s.old) and run the install again", dir, dir, dir)
	}
	if r.daemonUp(cfg) {
		return setupRefusal("a Seamless daemon already answers at %s, and seamlessd import restores no backup while one does: "+
			"stop it (seamlessd stop), then run the install again", cfg.ServerURL())
	}
	return nil
}

// setupWarnings is the import's logger in setup: a warning or an error it
// logs is a continuation row of setup's output, and the rest is the report's.
type setupWarnings struct {
	out   io.Writer
	attrs []slog.Attr
}

func (h *setupWarnings) Enabled(_ context.Context, l slog.Level) bool { return l >= slog.LevelWarn }

func (h *setupWarnings) Handle(_ context.Context, rec slog.Record) error {
	var b strings.Builder
	b.WriteString(setupPrintable(rec.Message))
	add := func(a slog.Attr) bool {
		fmt.Fprintf(&b, " %s=%s", a.Key, setupPrintable(a.Value.String()))
		return true
	}
	for _, a := range h.attrs {
		add(a)
	}
	rec.Attrs(add)
	_, err := fmt.Fprintf(h.out, "%s%s %s\n", setupRowCont, yellow("warning:"), b.String())
	return err
}

func (h *setupWarnings) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &setupWarnings{out: h.out, attrs: append(slices.Clip(h.attrs), attrs...)}
}

func (h *setupWarnings) WithGroup(string) slog.Handler { return h }

// setupImportDescribe names the backup before anything is decided: where and
// when it was made, and what it holds.
func setupImportDescribe(r *setupRun, path string, v archive.Verified) {
	m := v.Manifest
	setupRow(r.out, "import", tildePath(path))
	what := setupCount(v.MemoryFiles, "memory", "memories") + ", " + setupCount(v.NoteFiles, "note", "notes")
	if !v.HasDB {
		what += ", and no database (a knowledge-only backup)"
	}
	if m.Locks != nil && len(m.Locks.Projects) > 0 {
		what += ", locked: " + setupPrintable(strings.Join(m.Locks.Slugs(), ", "))
	}
	var from []string
	if m.SourceHost != "" {
		from = append(from, "from "+setupPrintable(m.SourceHost))
	}
	if !m.CreatedAt.IsZero() {
		from = append(from, m.CreatedAt.UTC().Format("2006-01-02 15:04 UTC"))
	}
	if len(from) > 0 {
		what = strings.Join(from, ", ") + ": " + what
	}
	fmt.Fprintf(r.out, "%s%s\n", setupRowCont, dim(what))
}

// setupCount is n things, in the singular for one.
func setupCount(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

// setupPrintable is s from a backup's manifest with its control characters
// dropped, so a backup cannot write a terminal escape into setup's output.
func setupPrintable(s string) string {
	return strings.Map(func(c rune) rune {
		if setupControl(c) {
			return -1
		}
		return c
	}, s)
}

// setupImportReport is what the import did, as rows: what it restored and
// where, what became of the locked projects, and its warnings.
func setupImportReport(r *setupRun, rep *archive.ImportReport) {
	setupRow(r.out, "imported", setupCount(rep.Memories, "memory", "memories")+", "+setupCount(rep.Notes, "note", "notes")+
		" "+dim("-> "+tildePath(rep.DataDir)))
	if l := rep.Locks; l != nil {
		if len(l.Locked) > 0 {
			fmt.Fprintf(r.out, "%slocked: %s\n", setupRowCont, setupPrintable(strings.Join(l.Locked, ", ")))
		}
		if len(l.Removed) > 0 {
			fmt.Fprintf(r.out, "%slocks removed: %s\n", setupRowCont, setupPrintable(strings.Join(l.Removed, ", ")))
		}
	}
	for _, w := range rep.Warnings {
		fmt.Fprintf(r.out, "%s%s %s\n", setupRowCont, yellow("warning:"), setupPrintable(w))
	}
}

// setupImportAdvice says what a lock the import kept needs: nothing enforces it
// until locked projects (peer_auth) are on -- on macOS and Linux the locked
// step setup names once the install is done, and on Windows never.
func setupImportAdvice(r *setupRun, cfg config.Config, gate *lockGate, rep *archive.ImportReport) {
	if rep.Locks == nil || len(rep.Locks.Locked) == 0 {
		return
	}
	if cfg.PeerAuth.Enabled {
		gate.advise(r.out, rep)
		return
	}
	setupRow(r.out, "locks", "kept on "+setupPrintable(strings.Join(rep.Locks.Locked, ", ")))
	why := "nothing enforces a lock until locked projects (peer_auth) are on: until then every agent on this computer can read them"
	if !setupPeerAuthOS(r.goos) {
		why = setupWindowsLocks
	}
	fmt.Fprintf(r.out, "%s%s\n", setupRowCont, dim(why))
}

// setupImportStopped is the lock gate's stop in setup's words. Nothing was
// written, so nothing was installed: the owner stopped at the question, or
// there was no one to ask and nothing answered it, and the gate's refusal
// then names SEAMLESS_IMPORT_LOCKS where seamlessd import's names its flags.
func setupImportStopped(r *setupRun, cfg config.Config, path string, err error) error {
	if !errors.Is(err, errImportStopped) {
		return &ownerError{msg: "seamlessd setup: " + setupKnobImport + ": " + err.Error(), cause: fmt.Errorf("%w: %w", errSetupRefused, err)}
	}
	next := fmt.Sprintf("To keep them locked from the first moment a daemon serves them, install with %s=%s, %s=%s and SEAMLESS_NO_SERVICE=1, "+
		"turn on locked projects (%sguides/peer-auth/#move-an-existing-install-over), then start the daemon",
		setupKnobImport, path, setupKnobImportLocks, setupImportKeep, setupDocs)
	if !setupPeerAuthOS(r.goos) || cfg.PeerAuth.Enabled {
		next = fmt.Sprintf("Run the install again and choose to keep the locks or to remove them, or install with %s=%s and %s=%s|%s",
			setupKnobImport, path, setupKnobImportLocks, setupImportKeep, setupImportRemove)
	}
	return &ownerError{msg: "seamlessd setup: the import stopped at the backup's locked projects, and nothing was installed. " + next,
		cause: fmt.Errorf("%w: %w", errSetupRefused, err)}
}

// setupImportFailed is an import that started and failed. The import undoes a
// restore it could not finish where it can; when the data dir still holds
// part of the backup setup says so, since a re-run refuses a data dir that is
// not empty.
func setupImportFailed(cfg config.Config, path string, err error) error {
	msg := fmt.Sprintf("seamlessd setup: %s was not imported: %v", path, err)
	if mode, derr := archive.DetectMode(cfg.DataDir); derr == nil && mode != archive.ModeFresh {
		dir := tildePath(cfg.DataDir)
		msg += fmt.Sprintf("; %s holds part of it, so move that aside (mv %s %s.failed) before you run the install again", dir, dir, dir)
	}
	return &ownerError{msg: msg, cause: err}
}
