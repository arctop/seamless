package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/arctop/seamless/internal/archive"
	"github.com/arctop/seamless/internal/config"
)

// exportTmpSuffix is appended to the destination while the archive is being
// written. The temp file is a sibling of the destination so the rename that
// publishes it stays within one filesystem, and so a crash leaves an obviously
// partial `*.tar.gz.tmp` next to the name it was going to take rather than a
// truncated archive under the real name.
const exportTmpSuffix = ".tmp"

// runExport writes a complete archive of this instance -- the markdown corpus,
// a consistent snapshot of seam.db, and a manifest -- to a file or to stdout.
//
// It never opens the database for writing: archive.Export snapshots through
// store.OpenExisting, so running a newer binary's export against a data dir a
// running older daemon serves cannot migrate its schema as a side effect of
// taking a backup. That is also why export does not need the daemon stopped.
//
// Config and the MCP API key are deliberately NOT in the archive (see the
// internal/archive package doc): an archive can be copied around without
// carrying this machine's credential.
func runExport(args []string) error {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	out := fs.String("o", "", `write the archive here; "-" streams it to stdout (default seamless-<host>-<UTC timestamp>.tar.gz in the current directory)`)
	noDB := fs.Bool("no-db", false, "export the markdown trees only, without the seam.db snapshot")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// An absent -o takes the derived default; a PRESENT but empty -o is
	// uninterpretable and must not quietly become that default (AGENTS.md >
	// Required patterns).
	dest := strings.TrimSpace(*out)
	if flagsSet(fs)["o"] && dest == "" {
		return errors.New(`seamlessd.export: -o is empty: pass a path, "-" for stdout, or omit -o for the default name`)
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("seamlessd.export: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	opts := archive.ExportOptions{
		DataDir:          cfg.DataDir,
		NoDB:             *noDB,
		SeamlessdVersion: buildVersion(),
		// The same hostname the sessions and the repo map are scoped by, so an
		// archive's source_host compares with the rows inside it.
		Host: config.Hostname(),
	}
	if cfg.PeerAuth.Enabled {
		// A force lock lives in the config, which no archive carries, so the
		// manifest has to name it or a restore elsewhere comes back open.
		opts.ForceLocked = cfg.PeerAuth.ForceLocked
		opts.PasskeyRPID, _ = cfg.RelyingParty()
	}

	if dest == "-" {
		// Report to stderr: stdout is the archive, and a summary line mixed
		// into it would corrupt the tar for whatever is reading the pipe.
		counter := &countingWriter{w: os.Stdout}
		opts.Out = counter
		m, err := archive.Export(ctx, opts)
		if err != nil {
			return fmt.Errorf("seamlessd.export: %w", err)
		}
		fmt.Fprintln(os.Stderr, exportSummary(m, "stdout", counter.n))
		return nil
	}

	if dest == "" {
		dest = defaultExportName(config.Hostname(), time.Now().UTC())
	}
	dest, err = expandHome(dest)
	if err != nil {
		return fmt.Errorf("seamlessd.export: %w", err)
	}

	m, size, err := exportToFile(ctx, opts, dest)
	if err != nil {
		return err
	}
	fmt.Println(exportSummary(m, dest, size))
	return nil
}

// exportToFile writes the archive to dest via a sibling temp file.
//
// The destination name is claimed with O_EXCL first: two exports started in the
// same second would otherwise derive the same default name and one would
// silently overwrite the other's backup. The archive itself is written to
// dest+".tmp" and renamed over the claim, so dest is either the empty claim (a
// run that failed and could not clean up) or a complete archive -- never a
// truncated one under a name that looks finished.
func exportToFile(ctx context.Context, opts archive.ExportOptions, dest string) (archive.Manifest, int64, error) {
	claim, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return archive.Manifest{}, 0, fmt.Errorf("seamlessd.export: %s already exists; refusing to overwrite it", dest)
		}
		return archive.Manifest{}, 0, fmt.Errorf("seamlessd.export: %w", err)
	}
	if err := claim.Close(); err != nil {
		return archive.Manifest{}, 0, fmt.Errorf("seamlessd.export: %w", err)
	}

	tmp := dest + exportTmpSuffix
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		_ = os.Remove(dest)
		return archive.Manifest{}, 0, fmt.Errorf("seamlessd.export: %w", err)
	}
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(tmp)
		_ = os.Remove(dest)
	}

	counter := &countingWriter{w: f}
	opts.Out = counter
	m, err := archive.Export(ctx, opts)
	if err != nil {
		cleanup()
		return archive.Manifest{}, 0, fmt.Errorf("seamlessd.export: %w", err)
	}
	// fsync before the rename: the rename is what makes the archive visible
	// under its final name, and a name that exists with unflushed contents
	// behind it is exactly the state a backup must never be in.
	if err := f.Sync(); err != nil {
		cleanup()
		return archive.Manifest{}, 0, fmt.Errorf("seamlessd.export: sync %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		_ = os.Remove(dest)
		return archive.Manifest{}, 0, fmt.Errorf("seamlessd.export: close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return archive.Manifest{}, 0, fmt.Errorf("seamlessd.export: publish %s: %w", dest, err)
	}
	return m, counter.n, nil
}

// defaultExportName is the archive name an -o-less export writes into the
// current directory: seamless-<host>-<UTC timestamp>.tar.gz.
//
// The timestamp is UTC with no separators so the name sorts chronologically and
// survives every filesystem; the host is sanitized because a hostname may carry
// characters (a path separator, a colon on Windows) that a filename may not.
func defaultExportName(host string, now time.Time) string {
	return fmt.Sprintf("seamless-%s-%s.tar.gz", sanitizeHostForFilename(host), now.UTC().Format("20060102T150405Z"))
}

// sanitizeHostForFilename reduces a hostname to lowercase letters, digits, dot,
// dash and underscore. An empty or fully-unusable hostname becomes "unknown"
// rather than an empty segment: config.Hostname returns "" for a machine whose
// name could not be read, and "seamless--20260101T000000Z.tar.gz" hides that.
func sanitizeHostForFilename(host string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(host)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	name := strings.Trim(b.String(), "-.")
	if name == "" {
		return "unknown"
	}
	return name
}

// exportSummary is the human report: what was written, where, and what the
// manifest says is inside it. Counts and names only -- never a body.
func exportSummary(m archive.Manifest, dest string, size int64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "wrote %s (%s)", dest, humanSize(size))
	fmt.Fprintf(&b, "\n  host %s, seamlessd %s, created %s",
		m.SourceHost, m.SeamlessdVersion, m.CreatedAt.Format(time.RFC3339))
	fmt.Fprintf(&b, "\n  corpus: %d memories, %d notes", m.Counts.MemoryFiles, m.Counts.NoteFiles)
	if m.IncludesDB {
		rows := 0
		for _, n := range m.Counts.Tables {
			rows += n
		}
		fmt.Fprintf(&b, "\n  database: schema v%d, %d rows across %d tables",
			m.SchemaVersion, rows, len(m.Counts.Tables))
		if len(m.EmbeddingModels) > 0 {
			fmt.Fprintf(&b, "\n  embeddings: %s", strings.Join(m.EmbeddingModels, ", "))
		}
	} else {
		fmt.Fprintf(&b, "\n  database: not included (--no-db); sessions, tasks, trials and events are NOT in this archive")
	}
	if m.Locks != nil {
		fmt.Fprintf(&b, "\n  locked: %s (they import locked; this file is not encrypted, so guard it like those projects)",
			strings.Join(m.Locks.Slugs(), ", "))
	}
	if m.LocksError != "" {
		fmt.Fprintf(&b, "\n  warning: could not read which projects the console locks (%s); any of them would import unlocked", m.LocksError)
	}
	return b.String()
}

// humanSize renders a byte count in the largest unit that keeps it readable.
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}

// countingWriter counts the bytes it passes through. The archive is written as
// a stream (it can go to stdout), so the only way to report its size is to
// measure it on the way past.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// flagsSet reports which flags were actually present on the command line, as
// opposed to sitting at their default. It is what separates "absent, so take
// the default" from "present but uninterpretable, so refuse" -- and what lets
// import reject a flag belonging to the other source family rather than
// ignoring it.
func flagsSet(fs *flag.FlagSet) map[string]bool {
	set := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	return set
}
