package main

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/releasesig"
	"github.com/arctop/seamless/internal/sshsig"
	"github.com/arctop/seamless/internal/sshsig/sshsigtest"
)

func releaseKey(t *testing.T) *sshsigtest.Key {
	t.Helper()
	k, err := sshsigtest.NewKey(sshsig.KeySKEd25519)
	require.NoError(t, err)
	return k
}

func signersFile(keys map[string]*sshsigtest.Key) string {
	var lines []string
	for _, name := range slices.Sorted(maps.Keys(keys)) {
		lines = append(lines, keys[name].SignersLine(name, releasesig.OwnerNamespace))
	}
	return "# release keys\n" + strings.Join(lines, "\n") + "\n"
}

// fakeGit is a repository of tags, each holding its release_signers (or not).
func fakeGit(files map[string]string, tags ...string) gitSource {
	return gitSource{
		tags: func() ([]string, error) { return tags, nil },
		readFile: func(tag, path string) ([]byte, bool, error) {
			if path != releaseSignersFile {
				return nil, false, errors.New("unexpected path " + path)
			}
			body, ok := files[tag]
			return []byte(body), ok, nil
		},
	}
}

func TestPreviousRelease(t *testing.T) {
	tags := []string{"v0.8.2", "v0.8.3", "v0.9.0-rc.1", "v0.9.0", "v0.10.0", "v1.0.0-rc.1", "vnope", "site-20261113"}
	for tag, want := range map[string]string{
		"v0.9.0":      "v0.8.3",
		"v0.9.0-rc.1": "v0.8.3",
		"v0.9.1":      "v0.9.0",
		"v0.10.0":     "v0.9.0",
		"v1.0.0":      "v0.10.0",
		"v1.0.0-rc.2": "v0.10.0",
		"v0.8.3":      "v0.8.2",
		"v0.8.2":      "",
	} {
		got, ok, err := previousRelease(tags, tag)
		require.NoError(t, err, tag)
		require.Equal(t, want != "", ok, tag)
		require.Equal(t, want, got, tag)
	}
	_, _, err := previousRelease(tags, "0.9.0")
	require.Error(t, err)
}

func TestOwnerGate(t *testing.T) {
	sums := []byte(line("seamless_0.9.1_linux_amd64.tar.gz", "linux bytes") + "\n")
	yk1, yk2, yk3 := releaseKey(t), releaseKey(t), releaseKey(t)
	sign := func(k *sshsigtest.Key) []byte {
		sig, err := k.Sign(releasesig.OwnerNamespace, sums, sshsigtest.Options{})
		require.NoError(t, err)
		return sig
	}
	both := signersFile(map[string]*sshsigtest.Key{"owner-yk1": yk1, "owner-yk2": yk2})

	t.Run("a key both tags list signs", func(t *testing.T) {
		g, err := loadOwnerGate(fakeGit(map[string]string{"v0.9.0": both, "v0.9.1": both}, "v0.9.0", "v0.9.1"), "v0.9.1", true)
		require.NoError(t, err)
		v, err := g.verify(sums, sign(yk2))
		require.NoError(t, err)
		require.Equal(t, []string{"owner-yk2"}, v.Signer.Principals)
		require.Equal(t, []string{"may sign: owner-yk1 " + yk1.Fingerprint(), "may sign: owner-yk2 " + yk2.Fingerprint()}, g.notes())
	})

	// The test the plan names: a key added in the same release cannot sign
	// it, however the commit that added it got in.
	t.Run("a key added in this release is refused", func(t *testing.T) {
		added := signersFile(map[string]*sshsigtest.Key{"owner-yk1": yk1, "owner-yk2": yk2, "intruder": yk3})
		g, err := loadOwnerGate(fakeGit(map[string]string{"v0.9.0": both, "v0.9.1": added}, "v0.9.0", "v0.9.1"), "v0.9.1", true)
		require.NoError(t, err)
		_, err = g.verify(sums, sign(yk3))
		require.ErrorIs(t, err, sshsig.ErrUnlisted)
		require.ErrorContains(t, err, "was added to release_signers at v0.9.1, so it may sign only from the next release on")
		require.Contains(t, strings.Join(g.notes(), "\n"), "ADDED at v0.9.1, so it signs from the next release on, not this one: intruder "+yk3.Fingerprint())
		// The keys v0.9.0 already listed still sign it.
		_, err = g.verify(sums, sign(yk1))
		require.NoError(t, err)

		// And at the next release it signs: the release it was added in is
		// now the previous one.
		g, err = loadOwnerGate(fakeGit(map[string]string{"v0.9.0": both, "v0.9.1": added, "v0.9.2": added}, "v0.9.0", "v0.9.1", "v0.9.2"), "v0.9.2", true)
		require.NoError(t, err)
		_, err = g.verify(sums, sign(yk3))
		require.NoError(t, err)
	})

	t.Run("a key removed in this release is refused at once", func(t *testing.T) {
		only1 := signersFile(map[string]*sshsigtest.Key{"owner-yk1": yk1})
		g, err := loadOwnerGate(fakeGit(map[string]string{"v0.9.0": both, "v0.9.1": only1}, "v0.9.0", "v0.9.1"), "v0.9.1", true)
		require.NoError(t, err)
		_, err = g.verify(sums, sign(yk2))
		require.ErrorContains(t, err, "was removed from release_signers at v0.9.1")
		require.Contains(t, strings.Join(g.notes(), "\n"), "REMOVED at v0.9.1")
		_, err = g.verify(sums, sign(yk1))
		require.NoError(t, err)
	})

	t.Run("a stranger's key", func(t *testing.T) {
		g, err := loadOwnerGate(fakeGit(map[string]string{"v0.9.0": both, "v0.9.1": both}, "v0.9.0", "v0.9.1"), "v0.9.1", true)
		require.NoError(t, err)
		_, err = g.verify(sums, sign(yk3))
		require.ErrorIs(t, err, sshsig.ErrUnlisted)
		require.NotContains(t, err.Error(), "added")
	})

	t.Run("a signature over another file, or for another namespace", func(t *testing.T) {
		g, err := loadOwnerGate(fakeGit(map[string]string{"v0.9.0": both, "v0.9.1": both}, "v0.9.0", "v0.9.1"), "v0.9.1", true)
		require.NoError(t, err)
		other, err := yk1.Sign(releasesig.OwnerNamespace, []byte("other\n"), sshsigtest.Options{})
		require.NoError(t, err)
		_, err = g.verify(sums, other)
		require.ErrorIs(t, err, sshsig.ErrBadSignature)
		gitNS, err := yk1.Sign("git", sums, sshsigtest.Options{})
		require.NoError(t, err)
		_, err = g.verify(sums, gitNS)
		require.ErrorIs(t, err, sshsig.ErrNamespace)
	})

	t.Run("bootstrap: no file at the previous release, this tag's decides", func(t *testing.T) {
		g, err := loadOwnerGate(fakeGit(map[string]string{"v0.9.0": both}, "v0.8.3", "v0.9.0"), "v0.9.0", true)
		require.NoError(t, err)
		require.True(t, g.bootstrap)
		require.Contains(t, g.notes()[0], "BOOTSTRAP: the previous release (v0.8.3) has no release_signers")
		_, err = g.verify(sums, sign(yk1))
		require.NoError(t, err)
		_, err = g.verify(sums, sign(yk3))
		require.ErrorIs(t, err, sshsig.ErrUnlisted)
	})

	t.Run("bootstrap with no previous release at all", func(t *testing.T) {
		g, err := loadOwnerGate(fakeGit(map[string]string{"v0.1.0": both}, "v0.1.0"), "v0.1.0", true)
		require.NoError(t, err)
		require.True(t, g.bootstrap)
	})

	t.Run("a release cannot drop the file", func(t *testing.T) {
		_, err := loadOwnerGate(fakeGit(map[string]string{"v0.9.0": both}, "v0.9.0", "v0.9.1"), "v0.9.1", false)
		require.ErrorContains(t, err, "v0.9.1 removes release_signers, which v0.9.0 carries")
	})

	t.Run("two files with no key in common", func(t *testing.T) {
		_, err := loadOwnerGate(fakeGit(map[string]string{
			"v0.9.0": signersFile(map[string]*sshsigtest.Key{"owner-yk1": yk1}),
			"v0.9.1": signersFile(map[string]*sshsigtest.Key{"owner-yk3": yk3}),
		}, "v0.9.0", "v0.9.1"), "v0.9.1", true)
		require.ErrorContains(t, err, "share no key")
	})

	t.Run("no file anywhere: required refuses, else nothing to check", func(t *testing.T) {
		src := fakeGit(map[string]string{}, "v0.8.2", "v0.8.3")
		_, err := loadOwnerGate(src, "v0.8.3", true)
		require.ErrorIs(t, err, errNoSigners)
		g, err := loadOwnerGate(src, "v0.8.3", false)
		require.NoError(t, err)
		require.False(t, g.required)
		require.Contains(t, g.notes()[0], "not held to an owner's signature")
	})

	t.Run("an unreadable file refuses", func(t *testing.T) {
		_, err := loadOwnerGate(fakeGit(map[string]string{"v0.9.0": both, "v0.9.1": "owner verify-required " + yk1.AuthorizedKey()}, "v0.9.0", "v0.9.1"), "v0.9.1", true)
		require.ErrorIs(t, err, sshsig.ErrSigners)
		require.ErrorContains(t, err, "release_signers at v0.9.1")
	})

	t.Run("a tag the clone does not have", func(t *testing.T) {
		_, err := loadOwnerGate(fakeGit(map[string]string{}, "v0.9.0"), "v0.9.1", true)
		require.ErrorContains(t, err, "not in this clone")
	})
}

// TestVerifyOwner_RealGit drives verify-owner end to end over a real git
// repository: annotated tags, the file read at each, the bootstrap, and a
// key added in the release that tries to sign it.
func TestVerifyOwner_RealGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=Owner", "-c", "user.email=owner@example.com",
			"-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)...)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
	}
	write := func(name, body string) {
		require.NoError(t, os.WriteFile(filepath.Join(repo, name), []byte(body), 0o644))
	}
	git("init", "-q")
	write("README.md", "seamless\n")
	git("add", ".")
	git("commit", "-q", "-m", "one")
	git("tag", "-a", "v0.8.3", "-m", "v0.8.3")

	yk1, intruder := releaseKey(t), releaseKey(t)
	write(releaseSignersFile, signersFile(map[string]*sshsigtest.Key{"owner-yk1": yk1}))
	git("add", ".")
	git("commit", "-q", "-m", "two")
	git("tag", "-a", "v0.9.0", "-m", "v0.9.0")

	write(releaseSignersFile, signersFile(map[string]*sshsigtest.Key{"owner-yk1": yk1, "intruder": intruder}))
	git("commit", "-q", "-am", "three")
	git("tag", "-a", "v0.9.1", "-m", "v0.9.1")

	dir := t.TempDir()
	sums := filepath.Join(dir, "checksums.txt")
	require.NoError(t, os.WriteFile(sums, []byte(line("seamless_0.9.1_linux_amd64.tar.gz", "x")+"\n"), 0o644))
	n := 0
	sigFile := func(k *sshsigtest.Key) string {
		body, err := os.ReadFile(sums)
		require.NoError(t, err)
		sig, err := k.Sign(releasesig.OwnerNamespace, body, sshsigtest.Options{})
		require.NoError(t, err)
		n++
		p := filepath.Join(dir, fmt.Sprintf("sig-%d", n))
		require.NoError(t, os.WriteFile(p, sig, 0o644))
		return p
	}
	verify := func(tag string, k *sshsigtest.Key) (string, error) {
		var out bytes.Buffer
		err := run([]string{"verify-owner", "-root", repo, "-tag", tag, "-checksums", sums, "-sig", sigFile(k), "-require"}, &out)
		return out.String(), err
	}

	out, err := verify("v0.9.0", yk1)
	require.NoError(t, err, "the bootstrap release is signed by its own file's key")
	require.Contains(t, out, "is signed by owner-yk1 ("+yk1.Fingerprint())

	out, err = verify("v0.9.1", yk1)
	require.NoError(t, err)
	require.Contains(t, out, "owner-yk1")

	_, err = verify("v0.9.1", intruder)
	require.ErrorContains(t, err, "was added to release_signers at v0.9.1")

	_, err = verify("v0.8.3", yk1)
	require.ErrorIs(t, err, errNoSigners, "v0.8.3 predates the file: with -require there is nothing to hold it to")

	var keys bytes.Buffer
	require.NoError(t, run([]string{"owner-keys", "-root", repo, "-tag", "v0.9.1"}, &keys))
	require.Contains(t, keys.String(), "ADDED at v0.9.1")
}

func TestCheckDraft(t *testing.T) {
	cfg := repoConfig(t)
	linux := line("seamless_0.9.0_linux_amd64.tar.gz", "linux bytes")
	good := []byte(strings.Join([]string{
		line(approverZip, "approver bytes"), line(amd64Archive, "amd64 bytes"), line(arm64Archive, "arm64 bytes"), linux, line("install", "script"),
	}, "\n") + "\n")
	okBundle := func(bundle, artifact []byte, tag string) error { return nil }

	names, err := checkDraft(cfg, "v0.9.0", goodTag(), good, nil, okBundle)
	require.NoError(t, err)
	require.Len(t, names, 5)

	_, err = checkDraft(cfg, "v0.9.0", goodTag(), good, nil, func([]byte, []byte, string) error { return errors.New("bad bundle") })
	require.ErrorContains(t, err, "checksums.txt is not v0.9.0's: bad bundle")

	swapped := bytes.Replace(good, []byte(sum("arm64 bytes")), []byte(sum("evil")), 1)
	_, err = checkDraft(cfg, "v0.9.0", goodTag(), swapped, nil, okBundle)
	require.ErrorContains(t, err, "checksums.txt lists "+arm64Archive+" as")

	missing := []byte(strings.Join([]string{line(amd64Archive, "amd64 bytes"), line(arm64Archive, "arm64 bytes"), linux}, "\n") + "\n")
	_, err = checkDraft(cfg, "v0.9.0", goodTag(), missing, nil, okBundle)
	require.ErrorContains(t, err, "does not list "+approverZip)

	extra := append(bytes.Clone(good), []byte(line("seamless_0.9.0_darwin_riscv.tar.gz", "x")+"\n")...)
	_, err = checkDraft(cfg, "v0.9.0", goodTag(), extra, nil, okBundle)
	require.ErrorContains(t, err, "a macOS asset the tag's manifest does not name")

	_, err = checkDraft(cfg, "v0.9.0", goodTag(), []byte("not a line\n"), nil, okBundle)
	require.ErrorContains(t, err, "not a sha256sum line")
}
