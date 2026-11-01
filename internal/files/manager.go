package files

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/llm"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/validate"
)

// ErrPathOccupied is returned when a write would land on a file owned by a
// different item (by id). In particular a superseded or archived memory keeps
// its tombstone file at memory/{project}/{name}.md, so that name stays occupied
// until the tombstone is deleted; a new memory silently overwriting it would
// destroy readable supersession history. Callers free the name (memory_delete)
// or pick another one.
var ErrPathOccupied = errors.New("target path belongs to a different item")

// ErrNoEmbedder is returned by StartReembed when no embedder is configured:
// there is nothing to embed with, so the request is refused rather than
// silently queued.
var ErrNoEmbedder = errors.New("no embedder configured")

// ErrReembedRunning is returned by StartReembed while a previous pass is still
// in flight; only one full re-embed runs at a time.
var ErrReembedRunning = errors.New("a re-embed pass is already running")

const (
	// defaultDebounce is how long a file must be quiet before the watcher
	// re-indexes it. Editors emit several writes per save; this coalesces them.
	defaultDebounce = 300 * time.Millisecond
	// maxEmbedRunes bounds the text sent to the embedder (roughly a few thousand
	// tokens). One vector per item; long bodies are truncated, not chunked.
	maxEmbedRunes = 8000
)

// Manager is the running files subsystem: it owns the filesystem Store and the
// SQLite Indexer, reconciles the trees against the index at startup, and watches
// them for out-of-band edits. Application writes go through it so their own
// writes are suppressed in the watcher (no re-index loop). An optional embedder
// keeps the vector index in sync with the file content (best-effort).
type Manager struct {
	store   *Store
	indexer *Indexer
	watcher *watcher
	db      *sql.DB
	// embedder is fixed before Start and read by handler goroutines after it;
	// see SetEmbedder for the ownership contract.
	embedder llm.Embedder
	logger   *slog.Logger

	// runDone is closed when the watcher event loop goroutine exits; nil until
	// Start launches it. Close waits on it so no handler (which touches the DB)
	// outlives the Manager. Start and Close are called from the owner's goroutine
	// (the daemon's serve path), never concurrently.
	runDone chan struct{}

	// locks serializes read-modify-write mutations per corpus file; see Mutate.
	// Zero value is ready, so a Manager built any way (including in tests) is
	// safe to mutate through without an extra construction step.
	locks pathLocks

	// reembedMu guards the re-embed pass state below. The pass runs in its own
	// goroutine (started by the console handler), so unlike the embedder field
	// its lifecycle is not fixed at startup.
	reembedMu     sync.Mutex
	reembed       ReembedProgress
	reembedCancel context.CancelFunc
	reembedDone   chan struct{}
}

// ReembedProgress reports the state of the current (or most recent) full
// re-embed pass. A zero value means no pass has run this process.
type ReembedProgress struct {
	Running    bool      `json:"running"`
	Total      int       `json:"total"`
	Done       int       `json:"done"`
	Failed     int       `json:"failed"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`
}

// NewManager builds a Manager over dataDir backed by db. It does not touch the
// filesystem or start watching until Start is called.
func NewManager(dataDir string, db *sql.DB, logger *slog.Logger) (*Manager, error) {
	if logger == nil {
		logger = slog.Default()
	}
	m := &Manager{
		store:   NewStore(dataDir),
		indexer: NewIndexer(db),
		db:      db,
		logger:  logger,
	}
	w, err := newWatcher(dataDir, m.handleFile, defaultDebounce, logger)
	if err != nil {
		return nil, fmt.Errorf("files.NewManager: %w", err)
	}
	m.watcher = w
	return m, nil
}

// SetEmbedder enables vector indexing. When set, every (re)indexed item is
// embedded and its vector upserted; embedding failures are logged, not fatal, so
// a slow or down embedder never blocks a write or an edit. Nil disables it.
//
// It MUST be called before Start, from the goroutine that owns the Manager.
// After Start, the watcher's handler goroutines read m.embedder without
// synchronization, so setting it on a running Manager is a data race. The field
// is unguarded deliberately: the embedder is fixed at startup (main.go resolves
// it from config, then starts), so a lock would cost every indexed write to
// protect against a call that has no legitimate reason to happen.
func (m *Manager) SetEmbedder(e llm.Embedder) { m.embedder = e }

// Store exposes the filesystem layer (read-only helpers for other packages).
func (m *Manager) Store() *Store { return m.store }

// Indexer exposes the index layer.
func (m *Manager) Indexer() *Indexer { return m.indexer }

// Start creates the tree directories, begins watching them, reconciles the index
// against disk, and launches the event loop in a background goroutine. The loop
// stops when ctx is cancelled or Close is called.
func (m *Manager) Start(ctx context.Context) error {
	// Before anything watches or indexes: relocate legacy project-less notes.
	// Runs ahead of watchTree so the moves raise no watcher events, and ahead of
	// Reconcile so the walk sees the final layout and re-points each index row by
	// note id (project comes from frontmatter, not the directory).
	if err := m.migrateNotesInboxToGlobal(); err != nil {
		return fmt.Errorf("files.Start: %w", err)
	}
	for _, tree := range []string{memoryTree, notesTree} {
		dir := filepath.Join(m.store.DataDir(), tree)
		if err := m.store.ensureDir(tree); err != nil {
			return fmt.Errorf("files.Start: mkdir %s: %w", dir, err)
		}
		m.hardenTree(dir)
		if err := m.watcher.watchTree(dir); err != nil {
			return fmt.Errorf("files.Start: watch %s: %w", dir, err)
		}
	}
	if err := m.Reconcile(ctx); err != nil {
		return fmt.Errorf("files.Start: reconcile: %w", err)
	}
	m.runDone = make(chan struct{})
	go func() {
		defer close(m.runDone)
		if err := m.watcher.run(ctx); err != nil && ctx.Err() == nil {
			m.logger.Error("files.Manager: watcher stopped", "error", err)
		}
	}()
	return nil
}

// hardenTree narrows any group- or world-accessible permission bits under root
// to owner-only, in place.
//
// Tightening the modes new files are written with (fileMode/dirMode) only helps
// files written from now on; a corpus that predates that change would stay
// world-readable forever, which is most of the exposure the change was for.
// This is the one-time catch-up, and it is idempotent -- after the first run
// nothing matches, so it costs a walk over a few hundred small files at startup.
//
// Best-effort throughout: a file another process owns, or a tree on a
// filesystem with no meaningful permission model, must not stop the daemon from
// starting. Failures are counted and logged once rather than per file.
func (m *Manager) hardenTree(root string) {
	var tightened, failed int
	// The walk error is deliberately ignored per-entry (below) and in total: a
	// partially-hardened tree is still better than an unhardened one, and the
	// only way this returns non-nil is if the callback does, which it never does.
	//nolint:errcheck // hardening is best-effort; a walk failure must not block startup
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // an unreadable entry is skipped, not fatal
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return nil
		}
		perm := info.Mode().Perm()
		if perm&0o077 == 0 {
			return nil
		}
		want := perm &^ 0o077
		if cerr := os.Chmod(path, want); cerr != nil {
			failed++
			return nil
		}
		tightened++
		return nil
	})
	if tightened > 0 || failed > 0 {
		m.logger.Info("files: narrowed corpus permissions to owner-only",
			"root", root, "tightened", tightened, "failed", failed)
	}
}

// Close stops the watcher and drains its work: after Close returns, no debounce
// handler is running or will run, the event-loop goroutine has exited, and any
// re-embed pass has been cancelled and drained -- so the caller may close the DB
// they write to. Safe to call more than once, and without a prior Start.
func (m *Manager) Close() error {
	err := m.watcher.close()
	m.reembedMu.Lock()
	cancel, done := m.reembedCancel, m.reembedDone
	m.reembedMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
	if m.runDone != nil {
		<-m.runDone
	}
	return err
}

// StartReembed launches a background pass that re-embeds every indexed memory
// and note with the current embedder, replacing each item's stored vector (the
// embeddings row is keyed by item id). This is the migration path after an
// embedding-model change: vectors from different models are not comparable, so
// items embedded under the old model are invisible to semantic search until
// they are re-embedded. It returns how many files were queued. Only one pass
// runs at a time (ErrReembedRunning), and with no embedder it refuses
// (ErrNoEmbedder) rather than queueing work that cannot run.
//
// The pass outlives the request that triggered it (context.WithoutCancel) and
// is cancelled by Close, which also waits for it -- so it never touches the DB
// after shutdown starts.
func (m *Manager) StartReembed(ctx context.Context) (int, error) {
	if m.embedder == nil {
		return 0, fmt.Errorf("files.StartReembed: %w", ErrNoEmbedder)
	}
	m.reembedMu.Lock()
	defer m.reembedMu.Unlock()
	if m.reembed.Running {
		return 0, fmt.Errorf("files.StartReembed: %w", ErrReembedRunning)
	}
	paths, err := m.indexer.AllFilePaths(ctx)
	if err != nil {
		return 0, fmt.Errorf("files.StartReembed: %w", err)
	}

	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	m.reembedCancel = cancel
	m.reembedDone = done
	m.reembed = ReembedProgress{Running: true, Total: len(paths), StartedAt: time.Now().UTC()}
	go m.runReembed(runCtx, done, paths)
	return len(paths), nil
}

// ReembedStatus returns a snapshot of the current or most recent re-embed pass.
func (m *Manager) ReembedStatus() ReembedProgress {
	m.reembedMu.Lock()
	defer m.reembedMu.Unlock()
	return m.reembed
}

// runReembed is the re-embed worker: it re-reads each indexed file and embeds
// it directly (bypassing the content-hash skip, whose whole point is to avoid
// exactly this work). Index rows are already current, so it does not re-index.
// Per-file failures are logged and counted, never fatal; a failed embed also
// clears the file's content hash so the regular reconcile path retries it.
func (m *Manager) runReembed(ctx context.Context, done chan struct{}, paths []string) {
	defer close(done)
	var doneN, failed int
	for _, rel := range paths {
		if ctx.Err() != nil {
			break
		}
		if !m.reembedOne(ctx, rel) {
			failed++
			m.clearHashForRetry(ctx, rel)
		}
		doneN++
		m.reembedMu.Lock()
		m.reembed.Done, m.reembed.Failed = doneN, failed
		m.reembedMu.Unlock()
	}
	m.reembedMu.Lock()
	m.reembed.Running = false
	m.reembed.FinishedAt = time.Now().UTC()
	m.reembedMu.Unlock()
	if ctx.Err() != nil {
		m.logger.Info("files: re-embed cancelled", "done", doneN, "total", len(paths), "failed", failed)
		return
	}
	m.logger.Info("files: re-embed finished", "total", len(paths), "failed", failed,
		"model", m.embedder.Model())
}

// reembedOne re-embeds a single indexed file, reporting success. A file that
// vanished, no longer parses, or has no id is skipped as a success -- it is not
// embeddable, so leaving it out of the vector index is the correct state.
func (m *Manager) reembedOne(ctx context.Context, relPath string) bool {
	tree, _, ok := treeAndRel(relPath)
	if !ok {
		return true
	}
	exists, err := m.store.fileExists(relPath)
	if err != nil {
		m.logger.Warn("files: reembed: unsafe file skipped", "file_path", relPath, "error", err)
		return false
	}
	if !exists {
		return true
	}
	content, err := m.store.readFile(relPath)
	if err != nil {
		m.logger.Warn("files: reembed: read failed", "file_path", relPath, "error", err)
		return false
	}
	switch tree {
	case memoryTree:
		mem, err := ParseMemory(content, relPath)
		if err != nil || mem.ID == "" {
			return true
		}
		return m.embedItem(ctx, kindMemory, mem.ID, memoryEmbedText(mem))
	case notesTree:
		note, err := ParseNote(content, relPath)
		if err != nil || note.ID == "" {
			return true
		}
		return m.embedItem(ctx, kindNote, note.ID, noteEmbedText(note))
	default:
		return true
	}
}

// Reconcile brings the index into agreement with the trees on disk: it re-indexes
// changed/new files and drops index rows whose file has been deleted.
func (m *Manager) Reconcile(ctx context.Context) error {
	seen := make(map[string]bool)

	for _, tree := range []string{memoryTree, notesTree} {
		root := filepath.Join(m.store.DataDir(), tree)
		rootInfo, err := os.Lstat(root)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("files.Reconcile: lstat %s: %w", root, err)
		}
		if rootInfo.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("files.Reconcile: %w: %s", ErrSymlink, root)
		}
		if !rootInfo.IsDir() {
			return fmt.Errorf("files.Reconcile: %w: %s is not a directory", ErrNotRegular, root)
		}
		err = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return nil // WalkDir does not follow it; final links are not corpus files.
			}
			if info.IsDir() {
				return nil
			}
			if filepath.Ext(path) != ".md" {
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			rel, err := filepath.Rel(m.store.DataDir(), path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			seen[rel] = true
			return m.handleFile(ctx, rel)
		})
		if err != nil {
			return fmt.Errorf("files.Reconcile: walk %s: %w", root, err)
		}
	}

	// Drop index rows whose file no longer exists on disk.
	indexed, err := m.indexer.AllFilePaths(ctx)
	if err != nil {
		return err
	}
	for _, rel := range indexed {
		if seen[rel] {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := m.indexer.DeleteByFilePath(ctx, rel); err != nil {
			m.logger.Warn("files.Reconcile: delete orphan", "file_path", rel, "error", err)
		}
	}
	return nil
}

// migrateNotesInboxToGlobal moves project-less notes from the legacy notes/inbox
// directory to notes/_global, matching memory/_global. Idempotent and cheap: one
// Lstat per start, and after the first run there is nothing to find.
//
// The index needs no repair -- a note's project lives in its frontmatter, so the
// following Reconcile re-parses each moved file and IndexNote upserts by id,
// repointing file_path. Nothing is orphaned, because the row that used to name
// notes/inbox/x.md IS the row now naming notes/_global/x.md.
func (m *Manager) migrateNotesInboxToGlobal() (retErr error) {
	root, err := m.store.openRoot(true)
	if err != nil {
		return err
	}
	defer closeRoot(root, &retErr)

	notesRel := notesTree
	oldRel := filepath.Join(notesRel, notesLegacyGlobalDir)
	newRel := filepath.Join(notesRel, notesGlobalDir)
	notesInfo, err := inspectNoSymlink(root, notesRel, true)
	if err != nil {
		return fmt.Errorf("legacy notes migration: %w", err)
	}
	if notesInfo == nil {
		return nil // fresh data dir; Start creates notes/ after this migration.
	}
	if !notesInfo.IsDir() {
		return fmt.Errorf("legacy notes migration: %w: %s is not a directory", ErrNotRegular, notesRel)
	}
	oldInfo, err := inspectNoSymlink(root, oldRel, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil // the common case: already migrated
	}
	if err != nil {
		return fmt.Errorf("legacy notes migration: %w", err)
	}
	if !oldInfo.IsDir() {
		return fmt.Errorf("legacy notes migration: %w: %s is not a directory", ErrNotRegular, oldRel)
	}
	newInfo, err := inspectNoSymlink(root, newRel, true)
	if err != nil {
		return fmt.Errorf("legacy notes migration: %w", err)
	}

	// No destination yet: one atomic rename, no window where a note is missing.
	if newInfo == nil {
		if err := root.Rename(oldRel, newRel); err != nil {
			return fmt.Errorf("move %s -> %s: %w", oldRel, newRel, err)
		}
		movedInfo, err := inspectNoSymlink(root, newRel, false)
		if err != nil {
			return fmt.Errorf("verify moved legacy notes directory: %w", err)
		}
		if !movedInfo.IsDir() {
			return fmt.Errorf("verify moved legacy notes directory: %w", ErrNotRegular)
		}
		m.logger.Info("files: moved legacy notes/inbox to notes/_global")
		return nil
	}
	if !newInfo.IsDir() {
		return fmt.Errorf("legacy notes migration: %w: %s is not a directory", ErrNotRegular, newRel)
	}

	// Both exist: merge file by file. A name owned by both trees is a real
	// conflict, so refuse it rather than clobber a note -- same rule as
	// notes_update's project move.
	oldDir, err := root.Open(oldRel)
	if err != nil {
		return fmt.Errorf("open %s: %w", oldRel, err)
	}
	entries, readErr := oldDir.ReadDir(-1)
	closeErr := oldDir.Close()
	if readErr != nil {
		return fmt.Errorf("read %s: %w", oldRel, readErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close %s: %w", oldRel, closeErr)
	}
	for _, e := range entries {
		if e.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("legacy notes migration: %w: %s", ErrSymlink, filepath.Join(oldRel, e.Name()))
		}
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return fmt.Errorf("legacy notes migration: inspect %s: %w", e.Name(), err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("legacy notes migration: %w: %s", ErrNotRegular, filepath.Join(oldRel, e.Name()))
		}
		src, dst := filepath.Join(oldRel, e.Name()), filepath.Join(newRel, e.Name())
		dstInfo, err := inspectNoSymlink(root, dst, true)
		if err != nil {
			return fmt.Errorf("legacy notes migration: %w", err)
		}
		if dstInfo != nil {
			m.logger.Warn("files: legacy inbox note left in place, target name taken",
				"note", e.Name(), "target", filepath.Join(m.store.DataDir(), dst))
			continue
		}
		if err := root.Rename(src, dst); err != nil {
			return fmt.Errorf("move %s -> %s: %w", src, dst, err)
		}
	}
	// Succeeds only when everything moved; a conflict or stray file keeps the
	// directory, which is the honest outcome -- do not report a partial move as
	// complete.
	if err := root.Remove(oldRel); err != nil {
		m.logger.Warn("files: legacy notes/inbox not removed (not empty)",
			"dir", filepath.Join(m.store.DataDir(), oldRel))
		return nil
	}
	m.logger.Info("files: merged legacy notes/inbox into notes/_global")
	return nil
}

// handleFile is the watcher/reconciler callback: it (re)indexes a changed file or
// deletes the index row when the file is gone. Unchanged files (same content
// hash) are skipped, so re-indexing is idempotent and cheap.
//
// Reconciliation deliberately keeps legacy non-portable names readable: an
// older Unix corpus may already contain punctuation a Windows filesystem cannot
// represent. Such an item is indexed with a warning, but every managed rewrite
// uses validate.Name and remains blocked until the owner renames the file and
// its name/slug/project frontmatter to a portable value. The next reconcile
// repoints the same item id, so that manual migration preserves identity.
func (m *Manager) handleFile(ctx context.Context, relPath string) error {
	tree, _, ok := treeAndRel(relPath)
	if !ok {
		return nil
	}
	exists, err := m.store.fileExists(relPath)
	if errors.Is(err, ErrSymlink) || errors.Is(err, ErrNotRegular) {
		m.logger.Warn("files.handleFile: unsafe corpus entry ignored", "file_path", relPath, "error", err)
		return m.indexer.DeleteByFilePath(ctx, relPath)
	}
	if err != nil {
		return fmt.Errorf("files.handleFile: inspect %s: %w", relPath, err)
	}
	if !exists {
		return m.indexer.DeleteByFilePath(ctx, relPath)
	}

	content, err := m.store.readFile(relPath)
	if err != nil {
		return fmt.Errorf("files.handleFile: read %s: %w", relPath, err)
	}
	newHash := ContentHash(content)
	if oldHash, found, err := m.indexer.ContentHashByFilePath(ctx, relPath); err != nil {
		return err
	} else if found && oldHash == newHash {
		return nil // unchanged
	}

	switch tree {
	case memoryTree:
		mem, err := ParseMemory(content, relPath)
		if err != nil {
			return fmt.Errorf("files.handleFile: %w", err)
		}
		if mem.ID == "" {
			m.logger.Warn("files.handleFile: memory file has no id, skipping", "file_path", relPath)
			return nil
		}
		m.warnLegacyName(kindMemory, mem.ID, relPath, "name", mem.Name, mem.Project)
		if err := m.indexer.IndexMemory(ctx, mem); err != nil {
			return err
		}
		if !m.embedItem(ctx, kindMemory, mem.ID, memoryEmbedText(mem)) {
			m.clearHashForRetry(ctx, relPath)
		}
		return nil
	case notesTree:
		note, err := ParseNote(content, relPath)
		if err != nil {
			return fmt.Errorf("files.handleFile: %w", err)
		}
		if note.ID == "" {
			m.logger.Warn("files.handleFile: note file has no id, skipping", "file_path", relPath)
			return nil
		}
		m.warnLegacyName(kindNote, note.ID, relPath, "slug", note.Slug, note.Project)
		if err := m.indexer.IndexNote(ctx, note); err != nil {
			return err
		}
		if !m.embedItem(ctx, kindNote, note.ID, noteEmbedText(note)) {
			m.clearHashForRetry(ctx, relPath)
		}
		return nil
	default:
		return nil
	}
}

func (m *Manager) warnLegacyName(kind, id, relPath, itemField, itemName, project string) {
	if err := validate.Name(itemName); err != nil {
		m.logger.Warn("files: legacy non-portable corpus name indexed read-only; rename the file and frontmatter before a managed rewrite",
			"kind", kind, "id", id, "file_path", relPath, "field", itemField, "error", err)
	}
	if project != "" {
		if err := validate.Name(project); err != nil {
			m.logger.Warn("files: legacy non-portable corpus project indexed read-only; rename the directory and frontmatter before a managed rewrite",
				"kind", kind, "id", id, "file_path", relPath, "field", "project", "error", err)
		}
	}
}

// embedItem embeds text and upserts the vector for an item, reporting whether
// the item's vector is now in sync with its content. It is best-effort: any
// embedding/store failure is logged rather than propagated, so a down embedder
// never blocks indexing. A false return means the vector is missing or stale;
// the caller clears the recorded content hash so a later reconcile retries the
// embed. With no embedder configured there is nothing to retry, so it reports
// true.
func (m *Manager) embedItem(ctx context.Context, kind, id, text string) bool {
	if m.embedder == nil {
		return true
	}
	vec, err := m.embedder.Embed(ctx, text)
	if err != nil {
		m.logger.Warn("files: embed failed", "kind", kind, "id", id, "error", err)
		return false
	}
	if err := store.UpsertEmbedding(ctx, m.db, id, kind, m.embedder.Model(), vec); err != nil {
		m.logger.Warn("files: upsert embedding failed", "kind", kind, "id", id, "error", err)
		return false
	}
	return true
}

// clearHashForRetry blanks the indexed content hash after a failed embed, so
// the skip-unchanged check does not pin the item to a missing or stale vector:
// the next reconcile (or watcher event) sees the file as changed, re-indexes
// it, and retries the embed. Best-effort; the embed failure itself was already
// logged, so a failure here only adds its own warning.
func (m *Manager) clearHashForRetry(ctx context.Context, relPath string) {
	if err := m.indexer.ClearContentHash(ctx, relPath); err != nil {
		m.logger.Warn("files: clear content hash for embed retry", "file_path", relPath, "error", err)
	}
}

// memoryEmbedText is the text embedded for a memory: name and one-line
// description carry the most signal, followed by the body.
func memoryEmbedText(m core.Memory) string {
	return truncateRunes(m.Name+"\n"+m.Description+"\n\n"+m.Body, maxEmbedRunes)
}

// noteEmbedText is the text embedded for a note.
func noteEmbedText(n core.Note) string {
	return truncateRunes(n.Title+"\n"+n.Description+"\n\n"+n.Body, maxEmbedRunes)
}

// truncateRunes caps s at max runes (never splitting a multi-byte rune).
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

// WriteMemory writes a memory through the Store (suppressing the watcher's view
// of its own write) and indexes it synchronously. It returns the stored memory
// with FilePath and ContentHash populated. A path already owned by a different
// memory -- notably the tombstone file of a superseded memory whose name the
// write would revive -- is refused with ErrPathOccupied rather than overwritten.
func (m *Manager) WriteMemory(ctx context.Context, mem core.Memory) (core.Memory, error) {
	relPath := MemoryRelPath(mem.Project, mem.Name)
	if err := m.ensurePathFree(ctx, relPath, mem.ID); err != nil {
		return core.Memory{}, fmt.Errorf("files.WriteMemory: %w", err)
	}
	m.suppress(relPath)
	written, err := m.store.WriteMemory(mem)
	if err != nil {
		return core.Memory{}, err
	}
	m.recordWrite(ctx, kindMemory, written.ID, written.Project, written.FilePath, written.ContentHash)
	if err := m.indexer.IndexMemory(ctx, written); err != nil {
		return core.Memory{}, err
	}
	if !m.embedItem(ctx, kindMemory, written.ID, memoryEmbedText(written)) {
		m.clearHashForRetry(ctx, written.FilePath)
	}
	return written, nil
}

// MoveMemory relocates a memory to another project, keeping its ULID. It mirrors
// the fixed note-move recipe: refuse when a different memory already owns the
// target path (WriteMemory's occupancy guard), and write the new file BEFORE
// removing the old one -- the index row is keyed by id, so the write repoints
// its file_path and a failed write leaves the memory intact at its old path
// instead of deleting it outright. The memory keeps its name; inbound [[name]]
// wiki-links resolve globally by bare name, so a move needs no link rewrite. The
// caller is responsible for bumping Updated. It is a no-op when toProject already
// equals the memory's project (idempotent for a retried apply).
func (m *Manager) MoveMemory(ctx context.Context, mem core.Memory, toProject string) (core.Memory, error) {
	if mem.Project == toProject {
		return m.WriteMemory(ctx, mem) // already home; re-write refreshes the index
	}
	oldPath := MemoryRelPath(mem.Project, mem.Name)
	mem.Project = toProject
	written, err := m.WriteMemory(ctx, mem)
	if err != nil {
		return core.Memory{}, fmt.Errorf("files.MoveMemory: write new path: %w", err)
	}
	// The index row now points at the new path, so this only deletes the old file.
	if err := m.Remove(ctx, oldPath); err != nil {
		return core.Memory{}, fmt.Errorf("files.MoveMemory: drop old path: %w", err)
	}
	return written, nil
}

// WriteNote writes a note through the Store and indexes it synchronously. As
// with WriteMemory, a path owned by a different note (a slug collision) is
// refused with ErrPathOccupied rather than overwritten.
func (m *Manager) WriteNote(ctx context.Context, note core.Note) (core.Note, error) {
	relPath := NoteRelPath(note.Project, note.Slug)
	if err := m.ensurePathFree(ctx, relPath, note.ID); err != nil {
		return core.Note{}, fmt.Errorf("files.WriteNote: %w", err)
	}
	m.suppress(relPath)
	written, err := m.store.WriteNote(note)
	if err != nil {
		return core.Note{}, err
	}
	m.recordWrite(ctx, kindNote, written.ID, written.Project, written.FilePath, written.ContentHash)
	if err := m.indexer.IndexNote(ctx, written); err != nil {
		return core.Note{}, err
	}
	if !m.embedItem(ctx, kindNote, written.ID, noteEmbedText(written)) {
		m.clearHashForRetry(ctx, written.FilePath)
	}
	return written, nil
}

// ensurePathFree refuses a write to relPath when a different item (by id)
// already owns it, checking the index row first (file_path is UNIQUE, so a
// collision would fail the upsert after the file was already clobbered) and
// then the file on disk (the source of truth; it may exist unindexed after an
// out-of-band write). A file whose id cannot be established is treated as
// occupied: overwriting content of unknown ownership loses data.
func (m *Manager) ensurePathFree(ctx context.Context, relPath, id string) error {
	if ownerID, found, err := m.indexer.IDByFilePath(ctx, relPath); err != nil {
		return err
	} else if found && ownerID != id {
		return fmt.Errorf("%w: %s is held by %s", ErrPathOccupied, relPath, ownerID)
	}
	exists, err := m.store.fileExists(relPath)
	if err != nil {
		return fmt.Errorf("inspect target path: %w", err)
	}
	if !exists {
		return nil
	}
	tree, _, _ := treeAndRel(relPath)
	var ownerID string
	switch tree {
	case memoryTree:
		mem, err := m.store.ReadMemory(relPath)
		if err != nil {
			return fmt.Errorf("%w: %s exists but its owner is unreadable: %w", ErrPathOccupied, relPath, err)
		}
		ownerID = mem.ID
	case notesTree:
		note, err := m.store.ReadNote(relPath)
		if err != nil {
			return fmt.Errorf("%w: %s exists but its owner is unreadable: %w", ErrPathOccupied, relPath, err)
		}
		ownerID = note.ID
	}
	if ownerID != id {
		return fmt.Errorf("%w: %s is held by %q", ErrPathOccupied, relPath, ownerID)
	}
	return nil
}

// Remove deletes a memory/note file (suppressing the watcher) and its index row.
func (m *Manager) Remove(ctx context.Context, relPath string) error {
	m.suppress(relPath)
	if err := m.store.Remove(relPath); err != nil {
		return err
	}
	m.recordRemove(ctx, relPath)
	return m.indexer.DeleteByFilePath(ctx, relPath)
}

// recordWrite records the hash of a file this Manager just wrote in the
// corpus record (write screening's drift watch). The statement is a no-op
// while the watch is disarmed, which is every installation that never turned
// write screening on. Every corpus writer -- the MCP tools, the gardener, the
// hooks' captures, a release, the owner's import and merge -- comes through
// WriteMemory and WriteNote, so a file whose bytes stop matching its record
// was changed by something other than Seamless.
//
// A failure is logged, never returned: the write already landed, and a
// missing record only makes the drift watch quarantine the file (in place,
// touching nothing) until the owner accepts it.
func (m *Manager) recordWrite(ctx context.Context, kind, id, project, relPath, hash string) {
	if m.db == nil {
		return
	}
	if err := store.RecordCorpusWrite(ctx, m.db, kind, id, project, relPath, hash, time.Now().UTC()); err != nil {
		m.logger.Warn("files: corpus record", "file_path", relPath, "error", err)
	}
}

// recordRemove drops the corpus record of a file this Manager just removed.
func (m *Manager) recordRemove(ctx context.Context, relPath string) {
	if m.db == nil {
		return
	}
	if err := store.RecordCorpusRemove(ctx, m.db, relPath); err != nil {
		m.logger.Warn("files: corpus record removal", "file_path", relPath, "error", err)
	}
}

// suppress tells the watcher to ignore its own imminent write to relPath.
func (m *Manager) suppress(relPath string) {
	if m.watcher == nil {
		return
	}
	if abs, err := m.store.abs(relPath); err == nil {
		m.watcher.ignoreNext(abs)
	}
}
