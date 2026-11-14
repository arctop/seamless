package main

// The update of a peer_auth system daemon (update.KindSystem). Such a daemon
// runs as an account of its own under a LaunchDaemon or a system unit, which
// the installer's per-user service cannot host, so no installer runs here:
// the updater -- running as the daemon's account, outside the service
// (outsideServiceCheck) -- verifies the release's checksums.txt against its
// Sigstore bundle for the exact tag (and, with update.release_signers set,
// against the owner's own signature, checksums.txt.sshsig), checks the
// platform archive against it,
// keeps the binaries it replaces beside them, renames the release's seamlessd
// and seam into place, and ends the daemon with SIGTERM: launchd's KeepAlive
// or systemd's Restart=always then starts the binary on disk. The account
// cannot `launchctl kickstart system/...` or `systemctl restart`, but it can
// signal its own process. A rollback renames the kept binaries back and ends
// the daemon the same way.
//
// Everything else is the engine's, unchanged: the update lock, the attempt
// record, the gates, the pre-update backup, the confirmation by observation,
// and the database restore when the new release migrated past the backup.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/update"
)

// systemBinaries are what a system install's update replaces: the archive's
// top-level files of these names, into the directory seamlessd runs from.
// seam goes first and seamlessd last, so the binary the service runs changes
// only once its client is in place.
var systemBinaries = []string{"seam", "seamlessd"}

const (
	// maxArchiveBytes caps a release archive's download and maxBinaryBytes
	// one binary in it (the archive is ~30 MB, its seamlessd ~55 MB); both
	// only bound a misrouted reply or a hostile archive.
	maxArchiveBytes = 256 << 20
	maxBinaryBytes  = 512 << 20
	// archiveTimeout bounds the archive's download, which the update client's
	// 30s whole-request timeout would cut short on a slow link.
	archiveTimeout = 10 * time.Minute
)

// systemAssetNames lists what a system install's update, or its rollback
// target's check, needs of release v: checksums.txt, its bundle, and this
// platform's archive. The installer is never run.
func systemAssetNames(v update.Version, goos, goarch string) []string {
	return []string{checksumsAsset, checksumsAsset + bundleSuffix, platformArchive(v, goos, goarch)}
}

// systemDownload is the target release as fetched for a system install,
// before any of it is verified.
type systemDownload struct {
	version         update.Version
	checksums       []byte
	checksumsBundle []byte
	archiveName     string
	archive         []byte
	// ownerSig is the owner's checksums.txt.sshsig, fetched only while
	// update.release_signers pins the owner's keys.
	ownerSig []byte
}

// fetchArchive downloads release v's archive name: up to maxArchiveBytes,
// within archiveTimeout.
func (s releaseSource) fetchArchive(ctx context.Context, v update.Version, name string) ([]byte, error) {
	url := s.assetURL(v, name)
	src := s
	client := *s.client
	client.Timeout = archiveTimeout
	src.client = &client
	resp, err := src.get(ctx, http.MethodGet, url, false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxArchiveBytes+1))
	switch {
	case err != nil:
		return nil, &fetchError{url: url, class: errFetchNetwork, detail: "read: " + err.Error(), cause: err}
	case len(body) > maxArchiveBytes:
		return nil, &fetchError{url: url, class: errFetchNetwork, detail: fmt.Sprintf("response exceeds %d bytes", maxArchiveBytes)}
	case len(body) == 0:
		return nil, &fetchError{url: url, class: errFetchNetwork, detail: "empty response"}
	}
	return body, nil
}

// fetchSystemTarget re-reads the release the attempt installs by its tag and
// downloads its checksums.txt, that file's bundle and this platform's
// archive. As for an installer install, the re-read catches a release pulled
// since the daemon chose it, a release without an asset the update needs is
// refused, and one still uploading is errReleaseInFlight. There is no floor
// fallback: an in-place swap needs the signed checksums.
func (u *updater) fetchSystemTarget(ctx context.Context) (systemDownload, error) {
	to := u.job.req.To
	rel, err := u.d.releases.releaseByTag(ctx, to)
	if err != nil {
		return systemDownload{}, fmt.Errorf("re-read v%s: %w", to, err)
	}
	if err := checkPublished(rel, to, systemAssetNames(to, u.d.goos, u.d.goarch)); err != nil {
		return systemDownload{}, err
	}
	if u.pinned() {
		if err := u.ownerSignatureListed(rel, to); err != nil {
			return systemDownload{}, err
		}
	}
	dl := systemDownload{version: to, archiveName: platformArchive(to, u.d.goos, u.d.goarch)}
	for _, a := range []struct {
		name string
		dst  *[]byte
	}{{checksumsAsset, &dl.checksums}, {checksumsAsset + bundleSuffix, &dl.checksumsBundle}} {
		body, err := u.d.releases.fetchAsset(ctx, to, a.name)
		if err != nil {
			return systemDownload{}, fmt.Errorf("download v%s: %w", to, listedAssetGone(err))
		}
		*a.dst = body
	}
	if u.pinned() {
		if dl.ownerSig, err = u.d.releases.fetchAsset(ctx, to, update.OwnerSignatureAsset); err != nil {
			return systemDownload{}, fmt.Errorf("download v%s: %w", to, listedAssetGone(err))
		}
	}
	if dl.archive, err = u.d.releases.fetchArchive(ctx, to, dl.archiveName); err != nil {
		return systemDownload{}, fmt.Errorf("download v%s: %w", to, listedAssetGone(err))
	}
	return dl, nil
}

// verifySystemDownload checks a system download: checksums.txt against its
// bundle with the exact identity of the release's tag, the archive against
// the checksum that file lists for it exactly once, and the archive itself,
// which must hold every one of systemBinaries at its top level as a regular
// file, once. Nothing is written.
func verifySystemDownload(dl systemDownload, verify func(bundle, artifact []byte, v update.Version) error) error {
	if err := verify(dl.checksumsBundle, dl.checksums, dl.version); err != nil {
		return fmt.Errorf("%s of v%s: %w", checksumsAsset, dl.version, err)
	}
	if err := update.VerifyChecksum(dl.checksums, dl.archiveName, dl.archive); err != nil {
		return fmt.Errorf("%s of v%s: %w", dl.archiveName, dl.version, err)
	}
	if err := walkArchive(dl.archive, func(string, io.Reader) error { return nil }); err != nil {
		return fmt.Errorf("%s of v%s: %w", dl.archiveName, dl.version, err)
	}
	return nil
}

// errArchiveLayout is a release archive that does not hold the binaries as
// an update needs them.
var errArchiveLayout = errors.New("the archive does not hold the release's binaries")

// walkArchive reads a release's tar.gz and calls each once for every one of
// systemBinaries, with its contents. A binary missing, listed twice, not a
// regular file or larger than maxBinaryBytes refuses the whole archive, and
// every other entry is skipped unread. Names are matched at the top level
// only ("seam", "./seam"), exactly.
func walkArchive(archive []byte, each func(name string, r io.Reader) error) error {
	zr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return fmt.Errorf("%w: %w", errArchiveLayout, err)
	}
	tr := tar.NewReader(zr)
	seen := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("%w: %w", errArchiveLayout, err)
		}
		name := strings.TrimPrefix(path.Clean(hdr.Name), "./")
		if !slices.Contains(systemBinaries, name) {
			continue
		}
		switch {
		case seen[name]:
			return fmt.Errorf("%w: %s is listed twice", errArchiveLayout, name)
		case hdr.Typeflag != tar.TypeReg:
			return fmt.Errorf("%w: %s is not a regular file", errArchiveLayout, name)
		case hdr.Size <= 0 || hdr.Size > maxBinaryBytes:
			return fmt.Errorf("%w: %s is %d bytes", errArchiveLayout, name, hdr.Size)
		}
		seen[name] = true
		if err := each(name, tr); err != nil {
			return err
		}
	}
	for _, name := range systemBinaries {
		if !seen[name] {
			return fmt.Errorf("%w: no %s", errArchiveLayout, name)
		}
	}
	return nil
}

// binarySwap is a system install's binaries while an update replaces them.
// keep links (else copies) each binary there is to a rollback name beside it
// (.seamlessd.rollback), which putBack renames over the binary again and drop
// removes once the new release has confirmed.
type binarySwap struct {
	dir string
	// had names the binaries that were there before; one that was not is
	// removed again by putBack.
	had map[string]bool
}

func rollbackName(dir, name string) string { return filepath.Join(dir, "."+name+".rollback") }
func stagedName(dir, name string) string   { return filepath.Join(dir, "."+name+".new") }

// keepBinaries keeps the binaries in dir, as the rollback of the update about
// to replace them. seamlessd must be there; seam may not be.
func keepBinaries(dir string) (binarySwap, error) {
	s := binarySwap{dir: dir, had: map[string]bool{}}
	for _, name := range systemBinaries {
		cur := filepath.Join(dir, name)
		if _, err := os.Lstat(cur); errors.Is(err, os.ErrNotExist) && name != "seamlessd" {
			continue
		} else if err != nil {
			return binarySwap{}, errors.Join(fmt.Errorf("keep %s: %w", cur, err), s.drop())
		}
		if err := keepFile(cur, rollbackName(dir, name)); err != nil {
			return binarySwap{}, errors.Join(err, s.drop())
		}
		s.had[name] = true
	}
	return s, nil
}

// keepFile makes kept a second name for cur: a hard link, which shares cur's
// bytes and signature, else a copy of them at cur's mode. A kept file left by
// an earlier attempt is replaced.
func keepFile(cur, kept string) error {
	if err := os.Remove(kept); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("replace %s: %w", kept, err)
	}
	if err := os.Link(cur, kept); err == nil {
		return nil
	}
	info, err := os.Stat(cur)
	if err != nil {
		return fmt.Errorf("keep %s: %w", cur, err)
	}
	src, err := os.Open(cur)
	if err != nil {
		return fmt.Errorf("keep %s: %w", cur, err)
	}
	defer func() { _ = src.Close() }()
	if err := writeFileSynced(kept, src, info.Mode().Perm()); err != nil {
		return fmt.Errorf("keep %s: %w", cur, err)
	}
	return nil
}

// writeFileSynced writes r to a new file at name with mode perm (whatever the
// umask), synced to disk before it closes, and removes it again on failure.
func writeFileSynced(name string, r io.Reader, perm os.FileMode) error {
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, r)
	if err == nil {
		err = f.Chmod(perm)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(name)
	}
	return err
}

// stageNew writes archive's binaries beside the current ones under their
// staged names (.seamlessd.new), 0755 and synced, ready for swapIn. Nothing the
// service runs changes; a failure removes what it wrote.
func (s binarySwap) stageNew(archive []byte) error {
	err := walkArchive(archive, func(name string, r io.Reader) error {
		staged := stagedName(s.dir, name)
		if err := os.Remove(staged); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("replace %s: %w", staged, err)
		}
		if err := writeFileSynced(staged, io.LimitReader(r, maxBinaryBytes), 0o755); err != nil {
			return fmt.Errorf("write %s: %w", staged, err)
		}
		return nil
	})
	if err != nil {
		s.unstage()
	}
	return err
}

// unstage removes the staged binaries, whatever is left of them.
func (s binarySwap) unstage() {
	for _, name := range systemBinaries {
		_ = os.Remove(stagedName(s.dir, name))
	}
}

// swapIn renames each staged binary over its name, seam first and seamlessd
// last, each rename atomic. A rename that fails puts the kept binaries back
// (putBack); when that fails too, the error wraps errPutBack.
func (s binarySwap) swapIn() error {
	for _, name := range systemBinaries {
		if err := os.Rename(stagedName(s.dir, name), filepath.Join(s.dir, name)); err != nil {
			err = fmt.Errorf("move the new %s into %s: %w", name, s.dir, err)
			if perr := s.putBack(); perr != nil {
				err = errors.Join(err, perr)
			}
			return err
		}
	}
	return nil
}

// errPutBack is a putBack that could not restore every binary: the bin dir
// may hold a mix of two releases, and the kept files are all that is left of
// the previous one.
var errPutBack = errors.New("the previous binaries could not all be put back")

// putBack renames each kept binary over its name again and removes one that
// was not there before the update; the kept names are gone afterwards. It
// carries on past a failure and reports every one, wrapping errPutBack.
func (s binarySwap) putBack() error {
	var errs []error
	for _, name := range systemBinaries {
		cur, kept := filepath.Join(s.dir, name), rollbackName(s.dir, name)
		if !s.had[name] {
			if err := os.Remove(cur); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, fmt.Errorf("remove the new %s: %w", name, err))
			}
			continue
		}
		if err := os.Rename(kept, cur); err != nil {
			errs = append(errs, fmt.Errorf("put the previous %s back: %w", name, err))
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %w", errPutBack, errors.Join(errs...))
}

// drop removes the kept binaries once they are no longer the way back: the
// new release confirmed, or nothing was swapped.
func (s binarySwap) drop() error {
	var errs []error
	for name := range s.had {
		if err := os.Remove(rollbackName(s.dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// runSystem is the engine past the gates for a system install: fetch, verify,
// preflight, backup, install. Everything before the install changes nothing
// the service runs, so a failure there -- the daemon not where the update
// state says, a full disk -- only backs off; it never counts toward blocking
// the release the way a failed install does.
func (u *updater) runSystem(ctx context.Context) outcome {
	u.stage(update.StageFetch)
	dl, err := u.fetchSystemTarget(ctx)
	if err != nil {
		return u.fail(fetchFailureStage(err), err)
	}

	u.stage(update.StageVerify)
	if err := verifySystemDownload(dl, u.d.verify); err != nil {
		return u.fail(update.StageVerify, err)
	}
	if u.pinned() {
		// No installer runs here: the archive is checked against the
		// checksums.txt the owner signed.
		if err := u.verifyPinned(dl.version, dl.checksums, dl.ownerSig, nil); err != nil {
			return u.fail(update.StageVerify, err)
		}
	}
	u.note("verify", "%s", green("signature verified")+dim(fmt.Sprintf(" -- checksums.txt signed by the release workflow on v%s; %s matches it", dl.version, dl.archiveName)))

	u.stage(update.StagePreflight)
	if err := u.systemPreflight(dl); err != nil {
		return u.fail(update.StagePreflight, err)
	}
	u.note("rollback", "v%s's binaries kept beside the new ones in %s, should v%s not come up", u.job.req.From, tildePath(u.swap.dir), dl.version)

	u.stage(update.StageBackup)
	if err := u.takeBackup(ctx); err != nil {
		u.abandonSwap()
		return u.fail(update.StageBackup, err)
	}

	u.stage(update.StageInstall)
	return u.installSystem(ctx, dl)
}

// systemPreflight proves the way back and readies the way forward before
// anything changes: the daemon the service runs is the one the update state
// names (daemonPID), the binaries it runs are kept as the rollback
// (keepBinaries), and the release's are staged beside them (stageNew).
func (u *updater) systemPreflight(dl systemDownload) error {
	if st, err := update.ReadState(u.dataDir()); err != nil {
		u.logf("read the update state before the install: %v", err)
	} else {
		u.prevInstance, u.prevPID = st.Running.Instance, st.Running.PID
	}
	if pid, why := u.daemonPID(); pid == 0 {
		return errors.New(why)
	}
	swap, err := keepBinaries(u.shape.dir)
	if err != nil {
		return err
	}
	if err := swap.stageNew(dl.archive); err != nil {
		return errors.Join(fmt.Errorf("stage v%s's binaries: %w", dl.version, err), swap.drop())
	}
	u.swap = swap
	return nil
}

// abandonSwap removes what the preflight staged and kept, when nothing was
// swapped after all.
func (u *updater) abandonSwap() {
	u.swap.unstage()
	if err := u.swap.drop(); err != nil {
		u.logf("remove the kept binaries: %v", err)
	}
}

// daemonPID is the process the service manager runs as this daemon: the one
// holding the data dir's lock, which must be the one the update state names
// as running when it names one. 0 and why not when there is none.
func (u *updater) daemonPID() (int, string) {
	held, pid := dataDirLockHolder(u.dataDir())
	switch {
	case !held:
		return 0, "no daemon holds the data dir, so nothing would start the new release"
	case pid <= 0:
		return 0, "the data dir's lock names no process"
	case u.prevPID != 0 && pid != u.prevPID:
		return 0, fmt.Sprintf("the data dir is held by pid %d, not the daemon the update state names (pid %d)", pid, u.prevPID)
	}
	return pid, ""
}

// installSystem swaps the staged binaries in and ends the daemon so its
// service manager starts them, then confirms (plan steps 6-8 for a system
// install). A daemon that has gone since the preflight, a rename that fails,
// or a daemon that cannot be ended leaves the previous binaries in place:
// StageInstall, nothing to roll back -- unless putting them back failed too,
// which the record says.
//
// On macOS this restart is launchd re-running the loaded job in place after
// its binary changed, which macOS 26 refused once for an ad-hoc-signed binary
// (memory macos-26-amfi-kills-first-launchd-spawn-after-binary-swap). The
// release binaries are Developer ID-signed, and a refused start is retried by
// KeepAlive after its 10s throttle, well inside the confirmation's deadline.
func (u *updater) installSystem(ctx context.Context, dl systemDownload) outcome {
	from := u.job.req.From
	notInstalled := func(reason string) outcome {
		u.note("install", "%s", yellow("not installed")+dim(" -- "+reason))
		return outcome{stage: update.StageInstall, err: "install: " + reason}
	}
	pid, why := u.daemonPID()
	if pid == 0 {
		u.abandonSwap()
		return notInstalled(why)
	}
	if err := u.swap.swapIn(); err != nil {
		u.swap.unstage()
		if errors.Is(err, errPutBack) {
			return notInstalled(fmt.Sprintf("v%s's binaries could not all be put in place, nor v%s's all back (%v): "+
				"check seamlessd and seam in %s, and the .rollback copies beside them", dl.version, from, err, u.swap.dir))
		}
		u.abandonSwap()
		return notInstalled(fmt.Sprintf("v%s's binaries could not be put in place, and v%s's are as they were: %v", dl.version, from, err))
	}
	u.logf("v%s's %s in place in %s; ending the daemon (pid %d) for its service to start them", dl.version, strings.Join(systemBinaries, " and "), u.swap.dir, pid)
	if err := u.d.stopDaemon(pid); err != nil {
		reason := fmt.Sprintf("the daemon (pid %d) could not be ended to start v%s: %v", pid, dl.version, err)
		if perr := u.swap.putBack(); perr != nil {
			reason += fmt.Sprintf("; putting v%s's binaries back failed too (%v): check seamlessd and seam in %s, and the .rollback copies beside them", from, perr, u.swap.dir)
		} else {
			reason += fmt.Sprintf("; v%s's binaries are back", from)
		}
		return notInstalled(reason)
	}
	u.note("install", "v%s in place; the service starts it", dl.version)

	u.stage(update.StageConfirm)
	res := u.confirm(ctx, dl.version, true)
	if res.ok {
		if err := u.swap.drop(); err != nil {
			u.logf("remove the kept binaries: %v", err)
		}
		u.note("confirm", "%s", green(fmt.Sprintf("v%s serving", dl.version))+dim(" (instance "+res.instance+")"))
		return outcome{stage: update.StageDone, ok: true}
	}
	u.note("confirm", "%s", yellow(fmt.Sprintf("v%s not confirmed", dl.version))+dim(" -- "+res.reason))
	return u.rollbackSystem(ctx, dl.version, res.reason)
}

// rollbackSystem restores the release the attempt updated from (plan step 8
// for a system install): rename the kept binaries back, end the daemon the
// service manager is running (or restarting) from the new ones, and -- while
// this process holds the data dir's lock, so the restarted daemon waits for
// it -- put the backup's database back if the new release migrated it
// (restoreDBIfMigrated, with its every rule). Then confirm the release
// serving again the same way.
func (u *updater) rollbackSystem(ctx context.Context, target update.Version, cause string) outcome {
	back := u.job.req.From
	u.stage(update.StageRollback)
	u.note("rollback", "restoring v%s", back)
	var notes []string
	if err := u.swap.putBack(); err != nil {
		return u.rollbackSystemFailed(target, cause, "its binaries could not be put back: "+err.Error(), notes)
	}
	if held, pid := dataDirLockHolder(u.dataDir()); held && pid > 0 {
		if err := u.d.stopDaemon(pid); err != nil {
			notes = append(notes, fmt.Sprintf("ending the daemon (pid %d) failed: %v", pid, err))
		}
	}
	if lock := u.holdDataDir(ctx); lock != nil {
		if n := u.restoreDBIfMigrated(ctx); n != "" {
			notes = append(notes, n)
		}
		if err := lock.Close(); err != nil {
			u.logf("release the data dir's lock: %v", err)
		}
	} else {
		notes = append(notes, "the daemon did not let go of the data dir, so the database was left as it is")
	}
	for _, n := range notes {
		u.note("rollback", "%s", dim(n))
	}
	res := u.confirm(ctx, back, false)
	if !res.ok {
		return u.rollbackSystemFailed(target, cause, res.reason, notes)
	}
	u.note("rollback", "%s", green(fmt.Sprintf("v%s serving again", back))+dim(" (instance "+res.instance+")"))
	msg := fmt.Sprintf("v%s did not come up (%s); rolled back to v%s", target, cause, back)
	if len(notes) > 0 {
		msg += "; " + strings.Join(notes, "; ")
	}
	return outcome{stage: update.StageRollback, rolledBack: true, err: msg}
}

// holdDataDirPoll is how often a rollback tries the data dir's lock while the
// daemon it ended exits. Much faster than a starting serve's 100ms
// (dataDirLockPoll), so the updater, not the daemon the service manager
// starts again at once, is the next to take it.
const holdDataDirPoll = 5 * time.Millisecond

// holdDataDir takes the data dir's lock as soon as the daemon lets go of it,
// trying for up to timing.daemonStop, and returns nil when it could not. While
// it is held a starting serve waits for it (lockDataDir), so the database can
// move.
func (u *updater) holdDataDir(ctx context.Context) *fileLock {
	path := filepath.Join(u.dataDir(), dataDirLockName)
	deadline := u.d.now().Add(u.d.timing.daemonStop)
	for {
		l, err := tryLockFile(path)
		if err == nil {
			return l
		}
		if !errors.Is(err, errLockHeld) || !u.d.now().Before(deadline) {
			u.logf("hold the data dir: %v", err)
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(holdDataDirPoll):
		}
	}
}

// rollbackSystemFailed is a rollback of a system install that could not
// confirm its target: the daemon may be down, and the record says how to
// recover, as an administrator.
func (u *updater) rollbackSystemFailed(target update.Version, cause, reason string, notes []string) outcome {
	back := u.job.req.From
	moved := u.dataDir() + ".broken"
	msg := fmt.Sprintf("v%s did not come up (%s), and the rollback to v%s was not confirmed (%s); the daemon may be down: "+
		"check it with sudo launchctl print system/%s on macOS or systemctl status %s on Linux; "+
		"put v%s's seamlessd and seam back in %s if they are not, and to bring the data back as it was, stop the daemon, "+
		"move the data dir aside to %s and run seamlessd import --from %s as the daemon's account",
		target, cause, back, reason, systemDaemonLabel, systemDaemonUnit, back, u.swap.dir, moved, movedBackupPath(u.dataDir(), moved, u.backup.path))
	if len(notes) > 0 {
		msg += "; " + strings.Join(notes, "; ")
	}
	u.note("rollback", "%s", yellow("not confirmed")+dim(" -- "+reason))
	return outcome{stage: update.StageRollback, err: msg}
}
