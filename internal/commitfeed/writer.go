package commitfeed

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/arctop/seamless/internal/files"
)

// FileMode is the snapshot file's mode: the daemon's account reads and
// writes it, its group (a monitor's, by the directory the deployment chose)
// reads it, and nobody else does.
const FileMode = 0o640

// WriterConfig wires a Writer.
type WriterConfig struct {
	DB *sql.DB
	// File is the snapshot file (commits.snapshot_file); "" means the writer
	// never starts. Its directory must exist: the writer does not create one.
	File string
	// Every is how often the file is rewritten (commits.snapshot_interval).
	Every time.Duration
	// Window is how far back each snapshot reaches (commits.snapshot_window).
	Window time.Duration
	// Idle is the liveness threshold (gardener.session_idle_minutes); see
	// Options.Idle.
	Idle time.Duration
	// Host is the envelope's host: a lowercase host name.
	Host string
	// Sealed reports whether a project is sealed to another OS account
	// (peer_auth). nil means none.
	Sealed func(project string) bool
	Logger *slog.Logger
	Now    func() time.Time
}

// Writer keeps the snapshot file current: it writes one snapshot as it
// starts and another every Every, each replacing the file whole (written
// beside it, then renamed into place), so a reader never sees half of one.
type Writer struct {
	cfg     WriterConfig
	runDone chan struct{}
}

// NewWriter builds a Writer. It touches nothing until Run.
func NewWriter(cfg WriterConfig) *Writer {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Writer{cfg: cfg}
}

// Run starts the loop when a file is configured, and refuses a writer it
// could not run (no interval, no window, a host the contract cannot carry);
// Wait blocks until the loop has returned after ctx ends.
func (w *Writer) Run(ctx context.Context) error {
	if w.cfg.File == "" {
		return nil
	}
	switch {
	case w.cfg.Every <= 0:
		return fmt.Errorf("commitfeed: the snapshot interval must be positive, got %s", w.cfg.Every)
	case w.cfg.Window <= 0:
		return fmt.Errorf("commitfeed: the snapshot window must be positive, got %s", w.cfg.Window)
	case len(w.cfg.Host) > 128 || !hostRe.MatchString(w.cfg.Host):
		return fmt.Errorf("commitfeed: host %q is not a lowercase host name", w.cfg.Host)
	}
	w.runDone = make(chan struct{})
	w.cfg.Logger.Info("commit snapshot writer started", "file", w.cfg.File,
		"interval", w.cfg.Every, "window", w.cfg.Window)
	go func() {
		defer close(w.runDone)
		tick := time.NewTicker(w.cfg.Every)
		defer tick.Stop()
		for {
			if err := w.WriteOnce(ctx); err != nil && ctx.Err() == nil {
				w.cfg.Logger.Warn("commit snapshot not written", "file", w.cfg.File, "error", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	return nil
}

// Wait blocks until the loop has returned (at once when it never ran).
func (w *Writer) Wait() {
	if w.runDone != nil {
		<-w.runDone
	}
}

// WriteOnce builds a snapshot and writes it to the file, replacing it whole.
func (w *Writer) WriteOnce(ctx context.Context) error {
	if w.cfg.File == "" {
		return fmt.Errorf("commitfeed: no snapshot file configured")
	}
	now := w.cfg.Now()
	snap, trim, err := Build(ctx, w.cfg.DB, Options{
		Host: w.cfg.Host, Window: w.cfg.Window, Idle: w.cfg.Idle, Now: now, Sealed: w.cfg.Sealed,
	})
	if err != nil {
		return err
	}
	data, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("commitfeed: encode snapshot: %w", err)
	}
	// Its consumer refuses a snapshot that breaks the contract whole, so one
	// that would be refused is never written: the last good file stays.
	if err := CheckJSON(data, now); err != nil {
		return fmt.Errorf("commitfeed: the snapshot breaks its contract and was not written: %w", err)
	}
	if err := files.AtomicWrite(w.cfg.File, append(data, '\n'), FileMode); err != nil {
		return fmt.Errorf("commitfeed: %w", err)
	}
	if trim != (Trimmed{}) {
		w.cfg.Logger.Warn("commit snapshot trimmed to the contract's bounds",
			"sessions_left_out", trim.Sessions, "tasks_left_out", trim.Tasks,
			"commits_left_out", trim.Commits, "sessions_unnamed", trim.Unnamed)
	}
	w.cfg.Logger.Debug("commit snapshot written", "file", w.cfg.File,
		"sessions", len(snap.Sessions), "tasks", len(snap.Tasks), "since", snap.Since)
	return nil
}
