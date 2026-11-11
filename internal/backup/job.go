package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/archive"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// The fixed words a failed run records (events, the status, doctor). A
// folder's own words are FolderCheck.Reason ("folder-<state>").
const (
	ReasonNoSpace   = "no-space"    // the disk lacks room for the backup and the reserve
	ReasonExport    = "export"      // the archive could not be written
	ReasonReadBack  = "read-back"   // the written file did not read back as the backup just made
	ReasonDatabase  = "database"    // the live database could not be read (install id, counts, fingerprint)
	ReasonCancelled = "cancelled"   // the daemon stopped mid-run
	ReasonInternal  = "internal"    // anything else
	ReasonUnsafe    = "unsafe-file" // under peer_auth, the written file did not stay private
)

// minReserve is the least free space a backup leaves behind on each disk it
// writes to, on top of its own size: the updater must always be able to take
// its pre-update backup, and a disk filled to the brim fails everything else.
const minReserve uint64 = 1 << 30

// Failure is why a run failed: a fixed word and the owner-facing sentence.
type Failure struct {
	Reason  string
	Message string
}

func (f *Failure) Error() string { return f.Message }

func failf(reason, format string, args ...any) *Failure {
	return &Failure{Reason: reason, Message: fmt.Sprintf(format, args...)}
}

// jobSpec is one run to make.
type jobSpec struct {
	id        string
	kind      string // archive.KindScheduled or archive.KindManual
	scheduled bool   // the schedule's run (or Try again): skips and prunes
	settings  Settings
	at        time.Time
	state     state // a copy of the record, for what it says about the folder
}

// jobResult is what a run came to.
type jobResult struct {
	spec    jobSpec
	outcome string
	record  *Record
	failure *Failure
	pruned  []Entry
	err     error // the underlying error behind failure, for the log
}

// run is one backup run, off the loop: it reads the live database only before
// it writes, and archive.Export holds its own snapshot handle only while it
// snapshots, so abandoning a run stalled on a slow disk leaves no database
// handle open.
func (s *Service) run(ctx context.Context, spec jobSpec) (res jobResult) {
	res = jobResult{spec: spec}
	defer func() {
		if res.failure != nil && ctx.Err() != nil {
			res.outcome, res.failure = OutcomeCancelled, nil
		}
	}()

	installID, err := s.installID(ctx)
	if err != nil {
		return failed(res, failf(ReasonDatabase, "the database could not be read: %v", err), err)
	}
	dir := spec.settings.Dir

	if spec.scheduled {
		empty, fp, err := s.liveContent(ctx)
		if err != nil {
			return failed(res, failf(ReasonDatabase, "the database could not be read: %v", err), err)
		}
		if empty {
			res.outcome = OutcomeEmpty
			return res
		}
		if newest, ok := s.newestOwn(dir, installID, spec.state); ok && newest == fp {
			res.outcome = OutcomeUnchanged
			return res
		}
	}

	create := spec.settings.DirIsLocal || !spec.state.heldBackups(dir)
	if c := s.folders.prepare(dir, create); !c.Usable() {
		return failed(res, &Failure{Reason: c.Reason(), Message: c.Message()}, nil)
	}
	local := filepath.Clean(LocalDir(s.d.DataDir))
	if !spec.settings.DirIsLocal {
		// The snapshot stages here whatever the folder: never on a cloud or
		// network disk.
		if lc := s.folders.prepare(local, true); !lc.Usable() {
			return failed(res, &Failure{Reason: lc.Reason(), Message: "Seamless's own backups folder: " + lc.Message()}, nil)
		}
	}
	s.sweepPartials(dir, installID)

	if f := s.checkSpace(dir, local); f != nil {
		return failed(res, f, nil)
	}

	staging := filepath.Join(local, stagingPrefix+spec.id)
	if err := os.Mkdir(staging, 0o700); err != nil {
		return failed(res, failf(ReasonExport, "could not stage the backup: %v", err), err)
	}
	defer func() { _ = os.RemoveAll(staging) }()

	rec, err := s.write(ctx, spec, dir, staging, installID)
	if err != nil {
		var f *Failure
		if errors.As(err, &f) {
			return failed(res, f, err)
		}
		return failed(res, failf(ReasonExport, "the backup could not be written to %s: %v", dir, err), err)
	}
	res.outcome, res.record = OutcomeCreated, rec

	if spec.scheduled {
		res.pruned = s.prune(ctx, dir, installID, spec.settings.Keep, spec.state, rec.Name)
	}
	return res
}

func failed(res jobResult, f *Failure, err error) jobResult {
	res.outcome, res.failure, res.err = OutcomeFailed, f, err
	return res
}

// liveContent reports whether the instance is empty and its fingerprint.
func (s *Service) liveContent(ctx context.Context) (empty bool, fingerprint string, err error) {
	counts, err := store.ProjectContentCounts(ctx, s.d.DB)
	if err != nil {
		return false, "", err
	}
	total := 0
	for _, p := range counts {
		total += p.Total()
	}
	if total == 0 {
		return true, "", nil
	}
	fp, err := store.ContentFingerprint(ctx, s.d.DB)
	return false, fp, err
}

// newestOwn is the fingerprint of this install's newest backup in dir (an
// automatic one or the owner's; never a safety, pre-update or another
// install's backup), ok false when there is none or it is unknown.
func (s *Service) newestOwn(dir, installID string, st state) (string, bool) {
	entries, _ := s.lister.listDir(dir, WhereFolder, installID, st.wrote, map[string]bool{})
	var best *Entry
	for i := range entries {
		e := &entries[i]
		if !e.Ours || (e.Kind != archive.KindScheduled && e.Kind != archive.KindManual) {
			continue
		}
		if best == nil || e.CreatedAt.After(best.CreatedAt) {
			best = e
		}
	}
	if best == nil {
		return "", false
	}
	fp := best.Fingerprint()
	if fp == "" && best.Dataless {
		fp = st.writtenFingerprint(dir, best.Name)
	}
	return fp, fp != ""
}

// checkSpace refuses a run the disks lack room for: the archive goes to dir
// and the database snapshot is staged in local, and each must keep the
// reserve (the larger of minReserve and the backup itself) free afterwards.
func (s *Service) checkSpace(dir, local string) *Failure {
	if s.d.FreeBytes == nil {
		return nil
	}
	need, err := estimateNeed(s.d.DataDir)
	if err != nil {
		return failf(ReasonInternal, "could not size the data folder: %v", err)
	}
	want := need + max(minReserve, need)
	for _, d := range slices.Compact([]string{dir, local}) {
		free, err := s.d.FreeBytes(d)
		if err != nil {
			return failf(ReasonNoSpace, "could not read the free space on %s: %v", d, err)
		}
		if free < want {
			return failf(ReasonNoSpace, "not enough free space on %s: %s free, %s needed", d, humanBytes(free), humanBytes(want))
		}
	}
	return nil
}

// estimateNeed is the space one backup takes, at most: the database and its
// WAL (the snapshot, then the archive holding it) and the markdown trees.
func estimateNeed(dataDir string) (uint64, error) {
	var need uint64
	for _, name := range []string{archive.DBName, archive.DBName + "-wal"} {
		fi, err := os.Lstat(filepath.Join(dataDir, name))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, err
		}
		need += uint64(fi.Size())
	}
	for _, tree := range []string{archive.MemoryTree, archive.NotesTree} {
		err := filepath.WalkDir(filepath.Join(dataDir, tree), func(_ string, d fs.DirEntry, err error) error {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			if d.Type().IsRegular() {
				fi, err := d.Info()
				if err != nil {
					return err
				}
				need += uint64(fi.Size())
			}
			return nil
		})
		if err != nil {
			return 0, err
		}
	}
	return need, nil
}

// write exports the instance into a dot-prefixed temp file in dir, through a
// writer that checks ctx on every write, then syncs, checks it stayed private
// (under peer_auth), renames it into its name, and reads its manifest back.
func (s *Service) write(ctx context.Context, spec jobSpec, dir, staging, installID string) (*Record, error) {
	tmpPath := filepath.Join(dir, partialName(installID, spec.id))
	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if st := classify(s.folders.goos, err); st != FolderError {
			c := FolderCheck{Dir: dir, State: st}
			return nil, &Failure{Reason: c.Reason(), Message: c.Message()}
		}
		return nil, err
	}
	published := false
	defer func() {
		if !published {
			_ = f.Close()
			_ = os.Remove(tmpPath)
		}
	}()

	w := &ctxWriter{ctx: ctx, w: f}
	m, err := s.export(ctx, archive.ExportOptions{
		DataDir: s.d.DataDir, Out: w, SeamlessdVersion: s.d.Version, Host: s.d.Host,
		ForceLocked: s.d.ForceLocked, PasskeyRPID: s.d.PasskeyRPID,
		Kind: spec.kind, InstallID: installID, TmpDir: staging,
	})
	if err != nil {
		return nil, err
	}
	if err := f.Sync(); err != nil {
		return nil, fmt.Errorf("sync: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("close: %w", err)
	}
	if s.folders.guard != nil {
		if err := s.folders.privateFileProblem(tmpPath); err != nil {
			return nil, failf(ReasonUnsafe, "%v", err)
		}
	}

	name, err := freeName(dir, s.d.Host, spec.at, spec.kind)
	if err != nil {
		return nil, err
	}
	final := filepath.Join(dir, name)
	if err := os.Rename(tmpPath, final); err != nil {
		return nil, fmt.Errorf("publish %s: %w", name, err)
	}
	published = true
	syncDir(dir)

	back, err := readManifestFile(final)
	if err == nil && (back.Fingerprint != m.Fingerprint || back.InstallID != m.InstallID || !back.CreatedAt.Equal(m.CreatedAt)) {
		err = errors.New("its manifest is not the one just written")
	}
	if err != nil {
		_ = os.Remove(final)
		return nil, failf(ReasonReadBack, "the backup written to %s did not read back (%v): the disk may be failing or out of sync", dir, err)
	}

	rec := &Record{
		Name: name, Dir: dir, Kind: spec.kind, At: m.CreatedAt, Bytes: w.n,
		Fingerprint: m.Fingerprint, Projects: len(m.Projects) + m.ProjectsOmitted,
	}
	for _, p := range m.Projects {
		rec.Memories += p.Memories
		rec.Notes += p.Notes
	}
	return rec, nil
}

// freeName is the backup's name, a second later for every name already taken
// in dir: two backups in one second (Back up now twice) never overwrite each
// other.
func freeName(dir, host string, at time.Time, kind string) (string, error) {
	for i := range 60 {
		name := FileName(host, at.Add(time.Duration(i)*time.Second), kind)
		if _, err := os.Lstat(filepath.Join(dir, name)); errors.Is(err, fs.ErrNotExist) {
			return name, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("every name for a backup at %s is taken", at.UTC().Format(nameStamp))
}

// prune removes this install's automatic backups in dir past the newest keep,
// by the time in their names; just is the one this run wrote. Nothing else is
// ever removed: no backup the owner made, no safety or pre-update backup, and
// no backup another install wrote into a folder they share. A placeholder
// whose manifest stays unread counts as ours only when the service recorded
// writing it. A file that will not go is logged and left for the next run.
func (s *Service) prune(ctx context.Context, dir, installID string, keep int, st state, just string) []Entry {
	entries, _ := s.lister.listDir(dir, WhereFolder, installID, st.wrote, map[string]bool{})
	var ours []Entry
	for _, e := range entries {
		if !e.Named || e.Parsed.Kind != archive.KindScheduled || !e.Ours {
			continue
		}
		if e.Manifest != nil && e.Manifest.Kind != archive.KindScheduled {
			continue
		}
		ours = append(ours, e)
	}
	slices.SortFunc(ours, func(a, b Entry) int {
		if a.Name == just {
			return -1
		}
		if b.Name == just {
			return 1
		}
		if c := b.Parsed.At.Compare(a.Parsed.At); c != 0 {
			return c
		}
		return strings.Compare(b.Name, a.Name)
	})
	var removed []Entry
	for _, e := range ours[min(keep, len(ours)):] {
		if ctx.Err() != nil {
			break
		}
		if err := os.Remove(e.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			s.log.Warn("backup: could not remove an old automatic backup; the next run tries again", "name", e.Name, "err", err)
			continue
		}
		s.lister.cache.forget(e.Path)
		removed = append(removed, e)
	}
	return removed
}

// sweepPartials removes the temp files this install's earlier runs left in
// dir (a crash, an abandoned stalled write). Another install's, and the
// updater's, are never touched.
func (s *Service) sweepPartials(dir, installID string) {
	des, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, de := range des {
		if de.Type().IsRegular() && isOwnPartial(de.Name(), installID) {
			_ = os.Remove(filepath.Join(dir, de.Name()))
		}
	}
}

// sweepStaging removes the snapshot staging directories earlier runs left in
// LocalDir. One daemon serves a data dir, so at startup every one is a
// leftover; the updater's .export-* staging is not ours and stays.
func sweepStaging(dataDir string) {
	local := LocalDir(dataDir)
	des, err := os.ReadDir(local)
	if err != nil {
		return
	}
	for _, de := range des {
		if de.IsDir() && strings.HasPrefix(de.Name(), stagingPrefix) {
			_ = os.RemoveAll(filepath.Join(local, de.Name()))
		}
	}
}

// ctxWriter stops a write once ctx is done, so a stopping daemon's run ends
// at its next write instead of finishing a long archive.
type ctxWriter struct {
	ctx context.Context
	w   io.Writer
	n   int64
}

func (c *ctxWriter) Write(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// event is the backup.created payload for a record.
func createdEvent(r *Record, where string) core.Event {
	return core.Event{Kind: core.EventBackupCreated, ItemID: r.Name, Payload: map[string]any{
		"kind": r.Kind, "where": where, "bytes": r.Bytes,
		"memories": r.Memories, "notes": r.Notes, "projects": r.Projects,
	}}
}

// humanBytes is a size the way the console says it.
func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
