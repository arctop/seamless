package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/arctop/seamless/internal/archive"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/importer"
	"github.com/arctop/seamless/internal/llm"
	"github.com/arctop/seamless/internal/store"
)

// importSource is which of the two things --from names. They are different
// operations that happen to share a verb: one migrates a Seam v1 store, the
// other restores or merges a `seamlessd export` archive.
type importSource int

const (
	// sourceV1Dir is a Seam v1 data directory (the default, ~/.seam).
	sourceV1Dir importSource = iota
	// sourceArchive is a .tar.gz written by `seamlessd export`, or "-" for one
	// arriving on stdin.
	sourceArchive
)

// Flags that belong to exactly one source family. A flag from the other family
// is present-but-uninterpretable, so it is an error rather than a silent no-op
// (AGENTS.md > Required patterns): `--dry-run` against a v1 directory would
// otherwise read as "previewed, nothing happened" while the import ran.
var (
	v1OnlyFlags      = []string{"skip"}
	archiveOnlyFlags = []string{"dry-run", "force", "keep-locks", "remove-locks"}
)

// runImport brings another store's data into this instance, from either of two
// sources, chosen by what --from names:
//
//   - a DIRECTORY -- a Seam v1 data dir (~/.seam). Memory/note files are written
//     and indexed under the v2 data dir; trials, sessions, and tool-call events
//     are inserted into seam.db. Idempotent by id, so re-running it imports only
//     what is new (P6 delta re-import).
//   - a FILE, or "-" for stdin -- a `seamlessd export` archive. Whether that is
//     a bit-exact restore or a merge is a property of the DESTINATION, not a
//     flag: an empty data dir is restored into, a populated one is merged into
//     (first-writer-wins by ULID). The mode is printed in the report.
func runImport(args []string) error {
	fs, f := newImportFlagSet()
	if err := fs.Parse(args); err != nil {
		return err
	}
	set := flagsSet(fs)

	src, kind, err := classifyImportSource(f.from)
	if err != nil {
		return err
	}
	if err := checkImportFlagFamily(kind, set); err != nil {
		return err
	}
	if f.keepLocks && f.removeLocks {
		return errors.New("seamlessd.import: --keep-locks and --remove-locks contradict each other; pass one")
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("seamlessd.import: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if kind == sourceArchive {
		return importArchive(ctx, cfg, src, f)
	}
	return importV1(ctx, cfg, src, splitCSV(f.skip), f.embed)
}

// importFlags is runImport's parsed command line.
type importFlags struct {
	from   string
	skip   string
	embed  bool
	dryRun bool
	force  bool

	keepLocks   bool
	removeLocks bool
}

// newImportFlagSet defines the flags in one place so the family lists above are
// checkable against the real set rather than against a transcribed copy of it
// (AGENTS.md > Common pitfalls: a hand-transcribed canonical set drifts in
// silence -- and a family guard naming a renamed flag fails open).
func newImportFlagSet() (*flag.FlagSet, *importFlags) {
	f := &importFlags{}
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	fs.StringVar(&f.from, "from", "~/.seam", "v1 data directory, or a `seamlessd export` archive (a file, or - for stdin)")
	fs.StringVar(&f.skip, "skip", "briefings", "v1 only: comma-separated storage projects to skip")
	fs.BoolVar(&f.embed, "embed", true, "embed imported items for cosine search (uses the configured provider)")
	fs.BoolVar(&f.dryRun, "dry-run", false, "archive only: report what the import would do and change nothing")
	fs.BoolVar(&f.force, "force", false, "archive only: restore into a data dir a daemon is still answering for")
	fs.BoolVar(&f.keepLocks, "keep-locks", false, "archive only: keep the backup's locks without being asked, even with no passkey able to open them yet")
	fs.BoolVar(&f.removeLocks, "remove-locks", false, "archive only: import the backup's locked projects without their locks")
	return fs, f
}

// classifyImportSource resolves --from to a path and decides which importer
// owns it. "-" is stdin (an archive only -- there is no way to stream a
// directory), a directory is v1, and a regular file is an archive. Anything
// else (a device, a socket, a dangling symlink) is refused by name rather than
// guessed at.
func classifyImportSource(from string) (string, importSource, error) {
	if strings.TrimSpace(from) == "" {
		return "", 0, errors.New("seamlessd.import: --from is empty: pass a v1 data directory, an archive file, or - for an archive on stdin")
	}
	if from == "-" {
		return "-", sourceArchive, nil
	}
	src, err := expandHome(from)
	if err != nil {
		return "", 0, fmt.Errorf("seamlessd.import: %w", err)
	}
	info, err := os.Stat(src)
	if err != nil {
		return "", 0, fmt.Errorf("seamlessd.import: source %s: %w", src, err)
	}
	switch {
	case info.IsDir():
		return src, sourceV1Dir, nil
	case info.Mode().IsRegular():
		return src, sourceArchive, nil
	default:
		return "", 0, fmt.Errorf("seamlessd.import: source %s is neither a directory (v1 store) nor a regular file (archive)", src)
	}
}

// checkImportFlagFamily refuses a flag that belongs to the other source.
func checkImportFlagFamily(kind importSource, set map[string]bool) error {
	wrong, mine, theirs := archiveOnlyFlags, "a v1 data directory", "an archive file (or -)"
	if kind == sourceArchive {
		wrong, mine, theirs = v1OnlyFlags, "an archive", "a v1 data directory"
	}
	var bad []string
	for _, name := range wrong {
		if set[name] {
			bad = append(bad, "--"+name)
		}
	}
	if len(bad) == 0 {
		return nil
	}
	return fmt.Errorf("seamlessd.import: %s does not apply to %s; drop it, or point --from at %s",
		strings.Join(bad, ", "), mine, theirs)
}

// importV1 is the Seam v1 migration, unchanged: it opens the destination
// database and files manager itself and hands them to internal/importer.
func importV1(ctx context.Context, cfg config.Config, src string, skip []string, embed bool) error {
	db, err := store.Open(cfg.DBPath())
	if err != nil {
		return fmt.Errorf("seamlessd.import: %w", err)
	}
	defer func() { _ = db.Close() }()

	mgr, err := files.NewManager(cfg.DataDir, db, nil)
	if err != nil {
		return fmt.Errorf("seamlessd.import: %w", err)
	}
	defer func() { _ = mgr.Close() }()

	if embed {
		if embedder := newImportEmbedder(cfg); embedder != nil {
			mgr.SetEmbedder(embedder)
		}
	}

	opts := importer.Options{SourceDir: src, SkipProjects: skip}
	fmt.Fprintf(os.Stderr, "importing from %s into %s ...\n", src, cfg.DataDir)
	rep, err := importer.Import(ctx, mgr, db, opts)
	if rep != nil {
		fmt.Println(rep.String())
	}
	if err != nil {
		return fmt.Errorf("seamlessd.import: %w", err)
	}
	return nil
}

// importArchive restores or merges a `seamlessd export` archive.
//
// Unlike the v1 path this opens no database and no files manager: archive.Import
// opens both itself, because a fresh restore has to install seam.db BEFORE
// anything can open it, and a merge has to migrate the archive's staged
// snapshot to this build's schema before it can copy rows across. Passing in an
// already-open handle would make the first impossible.
func importArchive(ctx context.Context, cfg config.Config, src string, f *importFlags) error {
	mode, err := archive.DetectMode(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("seamlessd.import: %w", err)
	}
	// A fresh restore replaces seam.db wholesale, so a daemon still serving
	// this data dir would be holding a database that no longer exists. A merge
	// writes through the same files layer and store the daemon uses, and a dry
	// run writes nothing at all -- neither needs the daemon down.
	if mode == archive.ModeFresh && !f.dryRun && !f.force && serverReachable(urlHostPort(cfg.ServerURL())) {
		return fmt.Errorf("seamlessd.import: %w: a fresh restore replaces %s underneath it; stop the daemon (seamlessd stop) or pass --force",
			archive.ErrDaemonRunning, cfg.DBPath())
	}

	var r io.Reader
	if src == "-" {
		r = os.Stdin
	} else {
		f, err := os.Open(src)
		if err != nil {
			return fmt.Errorf("seamlessd.import: %w", err)
		}
		defer func() { _ = f.Close() }()
		r = f
	}

	gate := &lockGate{
		cfg: cfg, flags: f, in: bufio.NewReader(os.Stdin), out: os.Stderr, host: config.Hostname(),
		// "-" means stdin is the archive, so it cannot also be the answer.
		interactive: src != "-" && stdinIsTerminal(),
		daemonUp:    func() bool { return serverReachable(urlHostPort(cfg.ServerURL())) },
		local:       func(ctx context.Context) ([]store.PasskeyFact, error) { return localPasskeys(ctx, cfg) },
	}
	opts := archive.ImportOptions{
		DataDir: cfg.DataDir,
		Src:     r,
		DryRun:  f.dryRun,
		Logger:  slog.Default(),
		OnLocks: gate.decide,
	}
	if cfg.PeerAuth.Enabled {
		opts.ForceLocked = cfg.PeerAuth.ForceLocked
	}
	if f.embed {
		opts.Embedder = newImportEmbedder(cfg)
	}

	fmt.Fprintf(os.Stderr, "importing %s into %s (%s) ...\n", sourceLabel(src), cfg.DataDir, mode)
	rep, err := archive.Import(ctx, opts)
	if rep != nil {
		fmt.Println(rep.String())
	}
	if err != nil {
		return fmt.Errorf("seamlessd.import: %w", err)
	}
	gate.advise(os.Stdout, rep)
	return nil
}

// sourceLabel names the archive for the progress line: "-" reads as a path.
func sourceLabel(src string) string {
	if src == "-" {
		return "an archive on stdin"
	}
	return src
}

// newImportEmbedder builds the configured embedder, or warns and returns nil.
// Embeddings are a search quality improvement, not a correctness requirement:
// an import without them lands every item lexically searchable, and a later
// re-embed fills in the vectors. Failing the import instead would lose the run.
func newImportEmbedder(cfg config.Config) llm.Embedder {
	embedder, err := llm.NewEmbedder(cfg.LLM)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: embeddings disabled (%v); importing without vectors\n", err)
		return nil
	}
	fmt.Fprintf(os.Stderr, "embedding via %s/%s (this may take a minute)...\n", cfg.LLM.Provider, embedder.Model())
	return embedder
}

// splitCSV splits a comma-separated list, trimming spaces and dropping blanks.
func splitCSV(s string) []string {
	var out []string
	for p := range strings.SplitSeq(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// expandHome expands a leading ~ to the user's home directory.
func expandHome(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if p == "~" {
		return home, nil
	}
	return filepath.Join(home, p[2:]), nil
}
