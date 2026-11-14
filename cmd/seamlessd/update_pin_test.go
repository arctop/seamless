package main

// update.release_signers in the updater's test world: a release installs
// only when the owner's signature over its checksums.txt verifies against the
// pinned keys, and each way it can fail refuses with its word. Without the
// pin, nothing changes.

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/sshsig"
	"github.com/arctop/seamless/internal/sshsig/sshsigtest"
	"github.com/arctop/seamless/internal/update"
)

func pinKey(t *testing.T, keyType string) *sshsigtest.Key {
	t.Helper()
	k, err := sshsigtest.NewKey(keyType)
	require.NoError(t, err)
	return k
}

// pinSigners writes an allowed_signers file listing keys (principal owner-N)
// and points the world's config at it as update.release_signers, returning
// the file's path.
func (w *world) pinSigners(keys ...*sshsigtest.Key) string {
	w.t.Helper()
	dir := filepath.Join(w.home, "etc")
	require.NoError(w.t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, "release_signers")
	var lines []string
	for i, k := range keys {
		lines = append(lines, k.SignersLine("owner-"+string(rune('a'+i)), "seamless-release"))
	}
	require.NoError(w.t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644))
	w.pinPath(path)
	return path
}

// pinPath sets update.release_signers in the world's config to path.
func (w *world) pinPath(path string) {
	w.t.Helper()
	raw, err := os.ReadFile(w.cfgPath)
	require.NoError(w.t, err)
	require.NoError(w.t, os.WriteFile(w.cfgPath, append(raw, []byte("update:\n  release_signers: "+path+"\n")...), 0o600))
	w.cfg, err = config.LoadFrom(w.cfgPath)
	require.NoError(w.t, err)
}

func readLog(t *testing.T, a update.Attempt) string {
	t.Helper()
	raw, err := os.ReadFile(a.LogPath)
	require.NoError(t, err)
	return string(raw)
}

func TestPinnedUpdate_AGoodSignatureInstalls(t *testing.T) {
	for _, keyType := range []string{sshsig.KeySKEd25519, sshsig.KeySKECDSAP256, sshsig.KeyEd25519} {
		t.Run(keyType, func(t *testing.T) {
			w := newWorld(t, "linux", "amd64", "0.7.2")
			k := pinKey(t, keyType)
			w.publish("0.7.2", releaseOpts{ownerKey: k})
			w.publish("0.7.3", releaseOpts{ownerKey: k})
			w.pinSigners(pinKey(t, sshsig.KeySKEd25519), k)

			a, err := w.runAuto("0.7.3")
			require.NoError(t, err)
			requireRecorded(t, w, a, update.StageDone, false)
			require.Empty(t, a.Signature)
			require.Len(t, w.installs, 1)
			log := readLog(t, a)
			require.Contains(t, log, "pin: "+filepath.Join(w.home, "etc", "release_signers")+" allows owner-a ")
			require.Contains(t, log, "v0.7.3's checksums.txt signed by owner-b ("+k.Fingerprint())
			require.Contains(t, log, "v0.7.2's checksums.txt signed by owner-b", "the rollback target is held to the pin too")
		})
	}
}

func TestPinnedUpdate_RefusesAReleaseTheOwnerDidNotSign(t *testing.T) {
	owner := pinKey(t, sshsig.KeySKEd25519)
	stranger := pinKey(t, sshsig.KeySKEd25519)
	for _, tt := range []struct {
		name    string
		opts    releaseOpts
		word    string
		inError string
	}{
		{"no signature at all", releaseOpts{}, update.SignatureMissing, "has no checksums.txt.sshsig"},
		{"a key the pin does not list", releaseOpts{ownerKey: stranger}, update.SignatureUnlisted, "not an allowed signer"},
		{"the wrong namespace", releaseOpts{ownerKey: owner, ownerNamespace: "git"}, update.SignatureInvalid, "wrong namespace"},
		{"a signature over another file", releaseOpts{ownerKey: owner, ownerSigns: []byte("the v0.7.2 checksums\n")}, update.SignatureInvalid, "does not verify"},
		{"touched but not verified (flags 01)", releaseOpts{ownerKey: owner, ownerOpts: sshsigtest.Options{Flags: sshsigtest.Flags(0x01)}}, update.SignatureInvalid, "user verification"},
		{"garbage", releaseOpts{ownerSig: []byte("-----BEGIN SSH SIGNATURE-----\nAAAA\n-----END SSH SIGNATURE-----\n")}, update.SignatureInvalid, "malformed"},
		{"checksums.txt does not vouch for the installer", releaseOpts{ownerKey: owner, unlistInstaller: true}, update.SignatureInvalid, "does not vouch for its install"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t, "linux", "amd64", "0.7.2")
			w.publish("0.7.2", releaseOpts{ownerKey: owner})
			w.publish("0.7.3", tt.opts)
			w.pinSigners(owner)

			a, err := w.runAuto("0.7.3")
			require.Error(t, err)
			requireRecorded(t, w, a, update.StageVerify, false)
			require.Equal(t, tt.word, a.Signature)
			require.Empty(t, a.Refusal)
			require.Contains(t, a.Error, tt.inError)
			require.Empty(t, w.installs, "nothing is installed")
			ver, _ := w.daemon.serving()
			require.Equal(t, "0.7.2", ver)

			// The daemon's fold blocks the release, says why, and records it.
			res := update.AttemptResult{Outcome: update.Classify(a, mustVersion(t, "0.7.2")), Stage: a.Stage, Signature: a.Signature}
			require.Equal(t, tt.word, res.SignatureRefused())
		})
	}
}

func TestPinnedUpdate_AnOwnerSignatureStillUploadingBacksOff(t *testing.T) {
	owner := pinKey(t, sshsig.KeySKEd25519)
	w := newWorld(t, "linux", "amd64", "0.7.2")
	w.publish("0.7.2", releaseOpts{ownerKey: owner})
	w.publish("0.7.3", releaseOpts{ownerKey: owner})
	w.setAssetState("0.7.3", update.OwnerSignatureAsset, "open")
	w.pinSigners(owner)
	a, err := w.runAuto("0.7.3")
	require.Error(t, err)
	requireRecorded(t, w, a, update.StageFetch, false)
	require.Empty(t, a.Signature, "transient, not the release's refusal")
}

func TestPinnedUpdate_TheRollbackTargetMustBeSignedToo(t *testing.T) {
	owner := pinKey(t, sshsig.KeySKEd25519)
	w := newWorld(t, "linux", "amd64", "0.7.2")
	w.publish("0.7.2", releaseOpts{}) // the release running: no owner's signature
	w.publish("0.7.3", releaseOpts{ownerKey: owner})
	w.pinSigners(owner)
	a, err := w.runAuto("0.7.3")
	require.Error(t, err)
	requireRecorded(t, w, a, update.StagePreflight, false)
	require.Empty(t, a.Signature, "the rollback target's state backs off; it never blocks the release")
	require.Contains(t, a.Error, "v0.7.2, the rollback target, carries no checksums.txt.sshsig")
	require.Empty(t, w.installs)
}

func TestPinnedUpdate_APinThatCannotBeUsedRefusesAtTheGates(t *testing.T) {
	owner := pinKey(t, sshsig.KeySKEd25519)
	for _, tt := range []struct {
		name  string
		setup func(t *testing.T, w *world)
		want  string
	}{
		{"missing", func(_ *testing.T, w *world) { w.pinPath(filepath.Join(w.home, "etc", "nope")) }, filepath.Join("etc", "nope")},
		{"no key for the release namespace", func(t *testing.T, w *world) {
			path := w.pinSigners(owner)
			require.NoError(t, os.WriteFile(path, []byte(owner.SignersLine("owner-a", "git")+"\n"), 0o644))
		}, "no key for namespace seamless-release"},
		{"an option the pin refuses", func(t *testing.T, w *world) {
			path := w.pinSigners(owner)
			require.NoError(t, os.WriteFile(path, []byte("owner-a verify-required "+owner.AuthorizedKey()+"\n"), 0o644))
		}, "verify-required"},
		{"writable by every account", func(t *testing.T, w *world) {
			if runtime.GOOS == "windows" {
				t.Skip("no POSIX modes on Windows")
			}
			path := w.pinSigners(owner)
			require.NoError(t, os.Chmod(path, 0o666))
		}, "group- or world-writable"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t, "linux", "amd64", "0.7.2")
			w.publish("0.7.2", releaseOpts{ownerKey: owner})
			w.publish("0.7.3", releaseOpts{ownerKey: owner})
			tt.setup(t, w)
			a, err := w.runAuto("0.7.3")
			require.Error(t, err)
			requireRecorded(t, w, a, update.StageGates, false)
			require.Equal(t, update.RefusalReleaseSigners, a.Refusal)
			require.Contains(t, a.Error, tt.want)
			require.Empty(t, w.installs)
		})
	}
}

// Without the pin a default install is what it was: a release with no
// owner's signature installs, and one signed by any key at all is not looked
// at.
func TestUnpinnedUpdate_IgnoresTheOwnersSignature(t *testing.T) {
	for _, opts := range []releaseOpts{{}, {ownerKey: pinKey(t, sshsig.KeySKEd25519), ownerSigns: []byte("anything")}} {
		w := newWorld(t, "linux", "amd64", "0.7.2")
		w.publish("0.7.2", releaseOpts{})
		w.publish("0.7.3", opts)
		a, err := w.runAuto("0.7.3")
		require.NoError(t, err)
		requireRecorded(t, w, a, update.StageDone, false)
		require.NotContains(t, readLog(t, a), "pin:")
	}
}

func TestPinnedUpdate_Attended(t *testing.T) {
	owner := pinKey(t, sshsig.KeySKEd25519)

	t.Run("a signed release installs", func(t *testing.T) {
		w := newWorld(t, "darwin", "arm64", "0.7.2")
		w.publish("0.7.2", releaseOpts{ownerKey: owner})
		w.publish("0.7.3", releaseOpts{ownerKey: owner})
		w.pinSigners(owner)
		var out strings.Builder
		d := w.deps(&out)
		d.environ = []string{"PATH=/usr/bin:/bin", "HOME=" + w.home}
		require.NoError(t, runAttendedUpdate(t.Context(), updateFlags{config: w.cfgPath}, d))
		require.Contains(t, out.String(), "checksums.txt.sshsig, by a key")
		require.Contains(t, out.String(), "v0.7.3 installed and serving")
	})
	t.Run("an unsigned release is refused, and the record says why", func(t *testing.T) {
		w := newWorld(t, "darwin", "arm64", "0.7.2")
		w.publish("0.7.2", releaseOpts{ownerKey: owner})
		w.publish("0.7.3", releaseOpts{})
		w.pinSigners(owner)
		var out strings.Builder
		d := w.deps(&out)
		d.environ = []string{"PATH=/usr/bin:/bin", "HOME=" + w.home}
		err := runAttendedUpdate(t.Context(), updateFlags{config: w.cfgPath}, d)
		require.ErrorContains(t, err, "v0.7.3 not installed (stopped at verify)")
		a, rerr := update.ReadAttempt(w.dataDir)
		require.NoError(t, rerr)
		require.Equal(t, update.WhyManual, a.Why)
		require.Equal(t, update.SignatureMissing, a.Signature)
		require.Empty(t, w.installs)
	})
	t.Run("--url is refused while a pin is set", func(t *testing.T) {
		w := newWorld(t, "darwin", "arm64", "0.7.2")
		w.pinSigners(owner)
		var out strings.Builder
		err := runAttendedUpdate(t.Context(), updateFlags{config: w.cfgPath, url: "https://example.com/install"}, w.deps(&out))
		require.ErrorContains(t, err, "--url runs an installer nobody signed")
	})
	t.Run("the floor fallback is refused while a pin is set", func(t *testing.T) {
		w := newWorld(t, "darwin", "arm64", "0.7.2")
		w.publish("0.7.2", releaseOpts{ownerKey: owner})
		w.publish("0.7.3", releaseOpts{ownerKey: owner})
		w.publish("0.6.0", releaseOpts{noChecksumsSign: true})
		w.pinSigners(owner)
		var out strings.Builder
		d := w.deps(&out)
		d.environ = []string{"PATH=/usr/bin:/bin", "HOME=" + w.home, "SEAMLESS_VERSION=0.6.0"}
		err := runAttendedUpdate(t.Context(), updateFlags{config: w.cfgPath}, d)
		require.ErrorContains(t, err, "v0.6.0 predates signed checksums, so update.release_signers")
		require.Empty(t, w.installs)
	})
}

func TestPinnedSystemUpdate(t *testing.T) {
	owner := pinKey(t, sshsig.KeySKEd25519)
	publishSigned := func(t *testing.T, w *world, opts releaseOpts, versions ...string) {
		for _, v := range versions {
			o := opts
			o.archive = releaseArchive(t, v)
			w.publish(v, o)
		}
	}
	t.Run("a signed release is swapped in", func(t *testing.T) {
		w, s := newSystemWorld(t, "0.7.2", "0.7.3")
		publishSigned(t, w, releaseOpts{ownerKey: owner}, "0.7.2", "0.7.3")
		w.pinSigners(owner)
		a, err := runSystemAuto(t, w, systemDeps(w, s), "0.7.3")
		require.NoError(t, err)
		requireRecorded(t, w, a, update.StageDone, false)
		requireBinaries(t, w, "0.7.3")
		require.Contains(t, readLog(t, a), "v0.7.3's checksums.txt signed by owner-a")
	})
	for _, tt := range []struct {
		name string
		opts releaseOpts
		word string
	}{
		{"unsigned", releaseOpts{}, update.SignatureMissing},
		{"a stranger's key", releaseOpts{ownerKey: pinKey(t, sshsig.KeySKEd25519)}, update.SignatureUnlisted},
		{"another file", releaseOpts{ownerKey: owner, ownerSigns: []byte("x")}, update.SignatureInvalid},
	} {
		t.Run(tt.name+" is refused, the binaries untouched", func(t *testing.T) {
			w, s := newSystemWorld(t, "0.7.2", "0.7.3")
			publishSigned(t, w, tt.opts, "0.7.3")
			w.pinSigners(owner)
			a, err := runSystemAuto(t, w, systemDeps(w, s), "0.7.3")
			require.Error(t, err)
			requireRecorded(t, w, a, update.StageVerify, false)
			require.Equal(t, tt.word, a.Signature)
			requireBinaries(t, w, "0.7.2")
			require.Empty(t, s.signals, "the daemon is never ended")
		})
	}
}

// The peer_auth rule for the pin's file: root's, through root's
// directories, and so out of the daemon's account's reach as well as the
// owner's. Played with a fake file system: the test cannot own root's files.
func TestReleaseSignersFileProblem(t *testing.T) {
	const daemon = 480
	type node struct {
		uid  uint32
		mode os.FileMode
	}
	tree := func(file node, dirs map[string]node) signersFS {
		return signersFS{euid: daemon, open: func(string) (io.ReadCloser, error) { return nil, os.ErrNotExist },
			resolve: func(p string) (string, error) { return p, nil },
			inspect: func(p string) (fileFacts, error) {
				if n, ok := dirs[p]; ok {
					return fileFacts{uid: n.uid, mode: n.mode | os.ModeDir, isDir: true}, nil
				}
				if p == "/opt/seamless/etc/release_signers" {
					return fileFacts{uid: file.uid, mode: file.mode}, nil
				}
				return fileFacts{}, os.ErrNotExist
			}}
	}
	rootDirs := map[string]node{"/": {0, 0o755}, "/opt": {0, 0o755}, "/opt/seamless": {0, 0o755}, "/opt/seamless/etc": {0, 0o755}}
	daemonEtc := map[string]node{"/": {0, 0o755}, "/opt": {0, 0o755}, "/opt/seamless": {0, 0o755}, "/opt/seamless/etc": {daemon, 0o755}}
	path := "/opt/seamless/etc/release_signers"
	if runtime.GOOS == "windows" {
		t.Skip("POSIX paths: filepath.IsAbs and Dir read them as Windows paths")
	}
	for _, tt := range []struct {
		name     string
		fsys     signersFS
		rootOnly bool
		want     string
	}{
		{"peer_auth: root's file under root's directories", tree(node{0, 0o644}, rootDirs), true, ""},
		{"peer_auth: the daemon's own file", tree(node{daemon, 0o644}, rootDirs), true, "under peer_auth it must be root's"},
		{"peer_auth: under the daemon's directory", tree(node{0, 0o644}, daemonEtc), true, "owned by uid 480"},
		{"peer_auth: group-writable", tree(node{0, 0o664}, rootDirs), true, "group- or world-writable"},
		{"user install: the account's own file", tree(node{daemon, 0o644}, daemonEtc), false, ""},
		{"user install: another account's file", tree(node{501, 0o644}, rootDirs), false, "must be root's or this account's"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := releaseSignersFileProblem(path, tt.rootOnly, tt.fsys)
			if tt.want == "" {
				require.Empty(t, p)
				return
			}
			require.Contains(t, p, tt.want)
		})
	}
	require.Contains(t, releaseSignersFileProblem("release_signers", false, signersFS{}), "not an absolute path")
}

func TestReleaseSignersFor_ThePinIsOptIn(t *testing.T) {
	signers, err := releaseSignersFor(config.Defaults(), hostSignersFS())
	require.NoError(t, err)
	require.Nil(t, signers)
}
