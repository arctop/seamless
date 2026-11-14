package main

// update.release_signers: the opt-in pin that holds every release this
// install's updater acts on -- the one it installs, by itself or by hand, and
// the one it would roll back to -- to the owner's own signature over the
// release's checksums.txt (checksums.txt.sshsig, verified in-process by
// internal/sshsig against the keys the file lists). It is the client-side
// guarantee of owner-signed releases: GitHub's settings, the release
// workflow and the Sigstore identity can all be steered by whoever holds an
// admin token, but none of them can make the owner's hardware key sign.
//
// The file itself must be beyond the reach of whatever it guards against:
//
//   - Under peer_auth it must be root's, through directories only root can
//     write -- not the owner's, whose agents it fences, and not the daemon's
//     account's, whose own updater it binds. serve refuses to start
//     otherwise (filesystemProblems), and doctor says why.
//   - On a user-service install everything is the owner's account's, so the
//     file may be that account's or root's, and no one else may write it or a
//     directory above it. The pin then guards against a release the owner did
//     not sign, not against the account itself, which can edit its own config.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/releasesig"
	"github.com/arctop/seamless/internal/sshsig"
	"github.com/arctop/seamless/internal/update"
)

// releaseSignersWhat is how the checks name the file.
const releaseSignersWhat = "update.release_signers"

// signersFS is what reading the pin needs of the machine, injectable so a
// test can play root's files without being root.
type signersFS struct {
	euid int
	// inspect is the owner and mode of a path without following a final
	// symlink; nil skips the ownership checks (an OS without them).
	inspect func(string) (fileFacts, error)
	open    func(string) (io.ReadCloser, error)
	// resolve is filepath.EvalSymlinks; nil is that.
	resolve func(string) (string, error)
}

func (fsys signersFS) evalSymlinks(p string) (string, error) {
	if fsys.resolve == nil {
		return filepath.EvalSymlinks(p)
	}
	return fsys.resolve(p)
}

// hostSignersFS reads this machine as this process.
func hostSignersFS() signersFS {
	fsys := signersFS{euid: os.Geteuid(), open: func(p string) (io.ReadCloser, error) { return os.Open(p) }}
	if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
		fsys.inspect = inspectPath
	}
	return fsys
}

// errReleaseSigners wraps every reason the pin cannot be used.
var errReleaseSigners = errors.New(releaseSignersWhat)

// loadReleaseSigners reads the pin at path and returns the keys it allows to
// sign a release. rootOnly is peer_auth's rule (root's file under root's
// directories); otherwise the file and every directory above it may be
// root's or this account's, and no one else may write them. A file that is
// missing, unreadable, open to another account, unparseable, or that names no
// key for the release namespace is an error wrapping errReleaseSigners --
// never "no pin".
func loadReleaseSigners(path string, rootOnly bool, fsys signersFS) ([]sshsig.Signer, error) {
	if p := releaseSignersFileProblem(path, rootOnly, fsys); p != "" {
		return nil, fmt.Errorf("%w: %s", errReleaseSigners, p)
	}
	f, err := fsys.open(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", errReleaseSigners, path, err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, sshsig.MaxSignersBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read %s: %w", errReleaseSigners, path, err)
	}
	signers, err := releasesig.ParseOwnerSigners(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", errReleaseSigners, path, err)
	}
	return signers, nil
}

// releaseSignersFileProblem says why the pin's file would not hold, "" when
// it would: not absolute, not a regular file, or writable by an account it
// must not be (see loadReleaseSigners). Without inspect it checks only that
// the path is absolute.
func releaseSignersFileProblem(path string, rootOnly bool, fsys signersFS) string {
	if !filepath.IsAbs(path) {
		return fmt.Sprintf("%s %q is not an absolute path", releaseSignersWhat, path)
	}
	if fsys.inspect == nil {
		return ""
	}
	who := "root or this account (uid " + fmt.Sprint(fsys.euid) + ")"
	trusted := func(uid uint32) bool { return uid == 0 || (!rootOnly && int(uid) == fsys.euid) }
	if rootOnly {
		who = "root"
	}
	real, err := fsys.evalSymlinks(path)
	if err != nil {
		return fmt.Sprintf("%s %s: %v", releaseSignersWhat, path, err)
	}
	// The directory the path names decides what a symlink at it points to,
	// and the directory the file is really in decides what the file is.
	dirs := []string{filepath.Dir(real)}
	if parent := filepath.Dir(filepath.Clean(path)); parent != dirs[0] {
		dirs = append(dirs, parent)
	}
	for _, start := range dirs {
		start, err := fsys.evalSymlinks(start)
		if err != nil {
			return fmt.Sprintf("%s %s: %v", releaseSignersWhat, path, err)
		}
		for dir := start; ; dir = filepath.Dir(dir) {
			facts, err := fsys.inspect(dir)
			if err != nil {
				return fmt.Sprintf("%s: directory %s: %v", releaseSignersWhat, dir, err)
			}
			switch {
			case !trusted(facts.uid):
				return fmt.Sprintf("%s: directory %s above %s is owned by uid %d, who could replace the file: keep it under directories owned by %s",
					releaseSignersWhat, dir, path, facts.uid, who)
			case facts.mode.Perm()&0o022 != 0 && (rootOnly || facts.mode&os.ModeSticky == 0):
				return fmt.Sprintf("%s: directory %s above %s is mode %04o, writable by other accounts", releaseSignersWhat, dir, path, facts.mode.Perm())
			}
			if next := filepath.Dir(dir); next == dir {
				break
			}
		}
	}
	facts, err := fsys.inspect(real)
	if err != nil {
		return fmt.Sprintf("%s %s: %v", releaseSignersWhat, real, err)
	}
	switch {
	case facts.isDir || facts.socket || facts.symlink:
		return fmt.Sprintf("%s %s is not a regular file", releaseSignersWhat, real)
	case !trusted(facts.uid):
		if rootOnly {
			return fmt.Sprintf("%s %s is owned by uid %d: under peer_auth it must be root's, so that neither your account nor the daemon's can change "+
				"which keys sign a release -- sudo chown root %s", releaseSignersWhat, real, facts.uid, real)
		}
		return fmt.Sprintf("%s %s is owned by uid %d: it must be root's or this account's", releaseSignersWhat, real, facts.uid)
	case facts.mode.Perm()&0o022 != 0:
		return fmt.Sprintf("%s %s is mode %04o: it must not be group- or world-writable (0644)", releaseSignersWhat, real, facts.mode.Perm())
	}
	return ""
}

// releaseSignersFor is the pin for cfg on this machine: nil and no error
// when cfg pins nothing.
func releaseSignersFor(cfg config.Config, fsys signersFS) ([]sshsig.Signer, error) {
	if cfg.Update.ReleaseSigners == "" {
		return nil, nil
	}
	return loadReleaseSigners(cfg.Update.ReleaseSigners, cfg.PeerAuth.Enabled, fsys)
}

// ownerSignatureError is update.release_signers refusing a release: word is
// its update.Signature* word, which the attempt record carries.
type ownerSignatureError struct {
	word string
	err  error
}

func (e *ownerSignatureError) Error() string { return e.err.Error() }
func (e *ownerSignatureError) Unwrap() error { return e.err }

// errOwnerSignatureMissing is a release with no checksums.txt.sshsig.
var errOwnerSignatureMissing = errors.New("the release carries no owner's signature")

// ownerSignatureMissing is the refusal of release v, which carries no
// checksums.txt.sshsig.
func ownerSignatureMissing(v update.Version, path string) error {
	return &ownerSignatureError{word: update.SignatureMissing, err: fmt.Errorf("%w: v%s has no %s, and %s (%s) installs only a release the owner signed",
		errOwnerSignatureMissing, v, update.OwnerSignatureAsset, releaseSignersWhat, path)}
}

// verifyOwnerSignature holds release v's checksums.txt to the owner's
// signature over it (releasesig.VerifyOwner) against signers, the pin at
// path, and returns a line saying who signed it. A failure is an
// ownerSignatureError: unlisted for a key the pin does not list, invalid for
// everything else.
func verifyOwnerSignature(signers []sshsig.Signer, path string, v update.Version, checksums, sig []byte) (string, error) {
	got, err := releasesig.VerifyOwner(signers, checksums, sig)
	if err != nil {
		word := update.SignatureInvalid
		if errors.Is(err, sshsig.ErrUnlisted) {
			word = update.SignatureUnlisted
		}
		return "", &ownerSignatureError{word: word, err: fmt.Errorf("%s of v%s, against %s (%s): %w", update.OwnerSignatureAsset, v, releaseSignersWhat, path, err)}
	}
	return releasesig.Describe(got), nil
}

// listedAsset reports whether rel lists the asset name at all, and whether
// its upload has finished ("" reads as uploaded, as update.Filter reads it).
func listedAsset(rel update.APIRelease, name string) (listed, uploaded bool) {
	for _, a := range rel.Assets {
		if a.Name == name {
			listed = true
			if a.State == "" || a.State == "uploaded" {
				return true, true
			}
		}
	}
	return listed, false
}
