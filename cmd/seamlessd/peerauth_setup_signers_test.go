package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/arctop/seamless/internal/releasesig"
	"github.com/arctop/seamless/internal/shellcmd"
	"github.com/arctop/seamless/internal/sshsig"
	"github.com/arctop/seamless/internal/sshsig/sshsigtest"
	"github.com/arctop/seamless/internal/update"
)

// pinOwner writes an owner's release signers file listing key, points the
// fake's config at it as update.release_signers, and returns the file's path
// and the sha256 of what it holds.
func (f *pasFake) pinOwner(key *sshsigtest.Key) (string, string) {
	f.t.Helper()
	dir := filepath.Join(f.h.owner.home, ".config", "seamless")
	path := filepath.Join(dir, "release_signers")
	data := []byte(key.SignersLine("owner", releasesig.OwnerNamespace) + "\n")
	require.NoError(f.t, os.WriteFile(path, data, 0o644))
	raw, err := os.ReadFile(f.cfgPath)
	require.NoError(f.t, err)
	require.NoError(f.t, os.WriteFile(f.cfgPath, append(raw, []byte("update:\n  release_signers: "+path+"\n")...), 0o600))
	sum := sha256.Sum256(data)
	return path, hex.EncodeToString(sum[:])
}

// signChecksums is key's checksums.txt.sshsig over checksums.
func signChecksums(t *testing.T, key *sshsigtest.Key, checksums string) []byte {
	t.Helper()
	sig, err := key.Sign(releasesig.OwnerNamespace, []byte(checksums), sshsigtest.Options{})
	require.NoError(t, err)
	return sig
}

// With update.release_signers pinned, the release setup installs is held to
// the owner's signature before anything is staged, and the daemon keeps a
// copy of the pin, root's, that its config names: every update after the
// move is held to the same keys.
func TestPeerAuthSetup_ReleaseSignersHoldTheReleaseAndMoveWithTheDaemon(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			f := newPasFake(t, goos)
			owner := pinKey(t, sshsig.KeySKEd25519)
			path, sum := f.pinOwner(owner)
			f.ownerSig = signChecksums(t, owner, pasTestChecksums)

			require.NoError(t, pasSetup(context.Background(), f.h, pasOptions{yes: true}))
			require.True(t, f.fetchedOwnerSig, "the owner's signature was asked for")
			out := f.out()
			require.Contains(t, out, "signers   "+pasReleaseSigners+", root's copy of ")
			require.Contains(t, out, owner.Fingerprint(), "the keys the owner confirms are shown")
			require.Contains(t, out, "owner     signature verified -- v0.10.4's checksums.txt signed by owner")

			plan := f.st().plan
			require.Equal(t, path, plan.ReleaseSigners)
			require.Equal(t, sum, plan.SignersSHA256)

			// Root's copy is checked against the plan's hash, then made root's 0644.
			all := f.all()
			require.Contains(t, all, "/usr/bin/sudo /usr/bin/tee "+pasEtc+"/.release_signers.new >/dev/null < "+path)
			require.Contains(t, all, sum+"  "+pasEtc+"/.release_signers.new")
			require.Contains(t, all, "/bin/mv -f "+pasEtc+"/.release_signers.new "+pasReleaseSigners)
			require.Contains(t, all, ":"+map[string]string{"darwin": "wheel", "linux": "root"}[goos]+" "+pasEtc+"/.release_signers.new")
			require.Less(t, strings.Index(all, "/opt/seamless/stage/seamlessd"), strings.Index(all, ".release_signers.new"),
				"the binaries step held the release to the pin before the config step installs it")

			// The daemon's config points at the copy.
			raw, err := os.ReadFile(f.cfgPath + pasKeptSuffix)
			require.NoError(t, err)
			daemon, err := pasDaemonConfig(raw, *plan, f.h.now())
			require.NoError(t, err)
			var got struct {
				Update struct {
					ReleaseSigners string `yaml:"release_signers"`
				} `yaml:"update"`
			}
			require.NoError(t, yaml.Unmarshal(daemon, &got))
			require.Equal(t, pasReleaseSigners, got.Update.ReleaseSigners)
		})
	}
}

// Without a pin nothing changes: no owner's signature is asked for, and the
// daemon's config names no pin.
func TestPeerAuthSetup_NoReleaseSignersNoOwnerCheck(t *testing.T) {
	f := newPasFake(t, "darwin")
	require.NoError(t, pasSetup(context.Background(), f.h, pasOptions{yes: true}))
	require.False(t, f.fetchedOwnerSig)
	require.NotContains(t, f.out(), "signers ")
	require.NotContains(t, f.all(), "release_signers")
	plan := f.st().plan
	require.Empty(t, plan.ReleaseSigners)
	raw, err := os.ReadFile(f.cfgPath + pasKeptSuffix)
	require.NoError(t, err)
	daemon, err := pasDaemonConfig(raw, *plan, f.h.now())
	require.NoError(t, err)
	require.NotContains(t, string(daemon), "release_signers")
}

// A release the pin's keys do not vouch for is refused before anything is
// staged: signed by a key the pin does not list, signed over other checksums,
// or not signed at all.
func TestPeerAuthSetup_ReleaseSignersRefuseAReleaseTheKeysDoNotVouchFor(t *testing.T) {
	stranger := pinKey(t, sshsig.KeySKEd25519)
	for _, tc := range []struct {
		name string
		sig  func(owner *sshsigtest.Key) []byte
		want string
		is   error
		// rerun: whether the advice is to run setup again (a signature
		// still to come), not to undo and plan afresh (a pin that refuses).
		rerun bool
	}{
		{"a key the pin does not list", func(*sshsigtest.Key) []byte { return signChecksums(t, stranger, pasTestChecksums) },
			"checksums.txt.sshsig of v0.10.4, against update.release_signers", sshsig.ErrUnlisted, false},
		{"another checksums.txt", func(owner *sshsigtest.Key) []byte { return signChecksums(t, owner, "ffff  other.tar.gz\n") },
			"checksums.txt.sshsig of v0.10.4, against update.release_signers", nil, false},
		{"no signature at all", func(*sshsigtest.Key) []byte { return nil },
			"v0.10.4 has no checksums.txt.sshsig, and update.release_signers", errOwnerSignatureMissing, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPasFake(t, "darwin")
			owner := pinKey(t, sshsig.KeySKEd25519)
			f.pinOwner(owner)
			f.ownerSig = tc.sig(owner)
			err := pasSetup(context.Background(), f.h, pasOptions{yes: true})
			require.ErrorContains(t, err, tc.want)
			if tc.is != nil {
				require.ErrorIs(t, err, tc.is)
			}
			var refusal *ownerSignatureError
			require.True(t, errors.As(err, &refusal), "the updater's own refusal: %v", err)
			if tc.rerun {
				require.ErrorContains(t, err, "it continues from this step")
			} else {
				require.ErrorContains(t, err, "run seamlessd peer-auth undo, set the pin right")
				require.NotContains(t, err.Error(), "it continues from this step")
			}
			require.NotContains(t, f.all(), pasStage+"/seamlessd", "nothing was staged")
			require.NotEqual(t, pasLineDone, f.st().status["binaries"])
		})
	}
}

// A pin's file that changed after the move was planned stops the move where
// it is: the keys the owner confirmed are the only keys it pins.
func TestPeerAuthSetup_ReleaseSignersChangedSinceThePlanStopTheMove(t *testing.T) {
	f := newPasFake(t, "linux")
	owner := pinKey(t, sshsig.KeySKEd25519)
	path, _ := f.pinOwner(owner)
	f.ownerSig = signChecksums(t, owner, pasTestChecksums)
	f.fetchErr = errors.New("network down")
	require.ErrorContains(t, pasSetup(context.Background(), f.h, pasOptions{yes: true}), "network down")

	other := pinKey(t, sshsig.KeySKEd25519)
	require.NoError(t, os.WriteFile(path, []byte(other.SignersLine("owner", releasesig.OwnerNamespace)+"\n"), 0o644))
	f.fetchErr = nil
	f.ownerSig = signChecksums(t, other, pasTestChecksums)
	err := pasSetup(context.Background(), f.h, pasOptions{yes: true})
	require.ErrorContains(t, err, "changed since the move was planned")
	require.NotContains(t, f.all(), pasStage+"/seamlessd")
}

// A source build carries no owner's signature, so a pinned install refuses it
// as the updater refuses --url.
func TestPeerAuthSetup_ReleaseSignersRefuseASourceBuild(t *testing.T) {
	f := newPasFake(t, "darwin")
	f.pinOwner(pinKey(t, sshsig.KeySKEd25519))
	src := t.TempDir()
	for _, name := range systemBinaries {
		require.NoError(t, os.WriteFile(filepath.Join(src, name), []byte(name), 0o755))
		f.files[filepath.Join(src, name)] = fileFacts{uid: 501, mode: 0o755}
	}
	f.h.exe = filepath.Join(src, "seamlessd")
	err := pasSetup(context.Background(), f.h, pasOptions{yes: true, sourceBuild: true})
	require.ErrorIs(t, err, errPasRefused)
	require.ErrorContains(t, err, "--source-build copies binaries nobody signed")
	require.Empty(t, f.scripts, "nothing ran")
}

// The dry run shows the pin: the keys, and root's copy in the config step.
func TestPeerAuthSetup_ReleaseSignersInTheDryRun(t *testing.T) {
	f := newPasFake(t, "darwin")
	owner := pinKey(t, sshsig.KeySKEd25519)
	path, sum := f.pinOwner(owner)
	require.NoError(t, pasSetup(context.Background(), f.h, pasOptions{dryRun: true}))
	out := f.out()
	require.Contains(t, out, "signers   "+pasReleaseSigners)
	require.Contains(t, out, "installs only once its "+update.OwnerSignatureAsset+" verifies against owner "+owner.Fingerprint())
	require.Contains(t, out, "and its release signers pin "+pasReleaseSigners)
	require.Contains(t, out, "< "+path)
	require.Contains(t, out, sum+"  "+pasEtc+"/.release_signers.new")
	require.Empty(t, f.state, "a dry run records nothing")
}

// pasReadSigners holds the owner's file to the updater's rules for a pin on a
// per-user install, and returns exactly what it checked.
func TestPasReadSigners_TheUpdatersRules(t *testing.T) {
	dir := t.TempDir()
	key := pinKey(t, sshsig.KeySKEd25519)
	path := filepath.Join(dir, "release_signers")
	data := []byte(key.SignersLine("owner", releasesig.OwnerNamespace) + "\n")
	require.NoError(t, os.WriteFile(path, data, 0o644))
	got, err := pasReadSigners(path)
	require.NoError(t, err)
	require.Equal(t, data, got)

	// The mode check is darwin's and linux's (hostSignersFS): Windows has no
	// group or world write bits to refuse.
	if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
		require.NoError(t, os.Chmod(path, 0o666))
		_, err = pasReadSigners(path)
		require.ErrorIs(t, err, errReleaseSigners)
		require.ErrorContains(t, err, "group- or world-writable")
		require.NoError(t, os.Chmod(path, 0o644))

		// A group-writable directory above it -- a Linux home under umask 002,
		// found in a live run on Ubuntu -- passes when its group holds this
		// account alone (Linux's local account files say so), and is refused
		// with the fix named otherwise.
		require.NoError(t, os.Chmod(dir, 0o775))
		real, rerr := filepath.EvalSymlinks(dir) // the check names the directory it inspected
		require.NoError(t, rerr)
		facts, ierr := inspectPath(real)
		require.NoError(t, ierr)
		_, err = pasReadSigners(path)
		if own := hostSignersFS().ownGroup; own != nil && own(facts.gid) {
			require.NoError(t, err, "the directory's group %d holds this account alone", facts.gid)
		} else {
			require.ErrorIs(t, err, errReleaseSigners)
			require.ErrorContains(t, err, "is mode 0775, writable by other accounts: chmod go-w "+shellcmd.PosixArg(real))
		}
		require.NoError(t, os.Chmod(dir, 0o700))
	}

	require.NoError(t, os.WriteFile(path, []byte(key.SignersLine("owner", "file")+"\n"), 0o644))
	_, err = pasReadSigners(path)
	require.ErrorIs(t, err, releasesig.ErrNoOwnerKeys)

	_, err = pasReadSigners("relative/release_signers")
	require.ErrorIs(t, err, errReleaseSigners)
}
