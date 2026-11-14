package sshsig_test

// The installers' half of the owner's signature (docs/install,
// docs/install.ps1): the keys they carry are the repository's
// release_signers, their self-test signature is the one rebuilt here from its
// seed, and docs/install's verify_owner_signature -- run for real in sh, with
// the system's ssh-keygen -- installs a release signed by a key it carries
// and refuses every other.

import (
	"crypto/sha256"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/sshsig"
	"github.com/arctop/seamless/internal/sshsig/sshsigtest"
)

func readRepo(t *testing.T, parts ...string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(append([]string{"..", ".."}, parts...)...))
	require.NoError(t, err)
	return strings.ReplaceAll(string(raw), "\r\n", "\n")
}

// signerLines are a release_signers file's key lines: no comments, no blanks.
func signerLines(text string) []string {
	var out []string
	for line := range strings.SplitSeq(text, "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	return out
}

// between is the text between the lines begin and end.
func between(t *testing.T, text, begin, end string) string {
	t.Helper()
	i := strings.Index(text, begin+"\n")
	require.GreaterOrEqual(t, i, 0, "no %q", begin)
	rest := text[i+len(begin)+1:]
	j := strings.Index(rest, end+"\n")
	require.GreaterOrEqual(t, j, 0, "no %q", end)
	return rest[:j]
}

func TestInstallers_CarryTheRepositorysReleaseSigners(t *testing.T) {
	want := signerLines(readRepo(t, "release_signers"))
	require.NotEmpty(t, want)

	sh := between(t, readRepo(t, "docs", "install"), "# BEGIN release_signers", "# END release_signers")
	m := regexp.MustCompile(`(?s)^OWNER_SIGNERS='(.*)'\n$`).FindStringSubmatch(sh)
	require.NotNil(t, m, "docs/install's block is OWNER_SIGNERS='...'")
	require.Equal(t, want, signerLines(m[1]), "docs/install carries release_signers' keys")

	ps := between(t, readRepo(t, "docs", "install.ps1"), "# BEGIN release_signers", "# END release_signers")
	m = regexp.MustCompile(`(?s)^\$OwnerSigners = @'\n(.*)'@\n$`).FindStringSubmatch(ps)
	require.NotNil(t, m, "docs/install.ps1's block is $OwnerSigners = @'...'@")
	require.Equal(t, want, signerLines(m[1]), "docs/install.ps1 carries release_signers' keys")

	for _, l := range want {
		require.NotContains(t, l, "verify-required")
	}
}

// selftestKey rebuilds the installers' self-test key from its seed.
func selftestKey(t *testing.T) *sshsigtest.Key {
	t.Helper()
	seed := sha256.Sum256([]byte("seamless-release self-test key"))
	k, err := sshsigtest.FromEd25519Seed(sshsig.KeySKEd25519, seed[:])
	require.NoError(t, err)
	return k
}

func TestInstallers_SelfTestSignatureIsTheSeedsAndVerifies(t *testing.T) {
	k := selftestKey(t)
	line := k.SignersLine("selftest", "seamless-release")
	sig, err := k.Sign("seamless-release", []byte("seamless-release self-test\n"), sshsigtest.Options{})
	require.NoError(t, err)

	install := readRepo(t, "docs", "install")
	require.Contains(t, install, "SELFTEST_SIGNER='"+line+"'\n")
	require.Contains(t, install, "SELFTEST_SIG='"+strings.TrimSuffix(string(sig), "\n")+"'\n")
	ps := readRepo(t, "docs", "install.ps1")
	require.Contains(t, ps, "$SelftestSigner = '"+line+"'\n")
	require.Contains(t, ps, "$SelftestSig = @'\n"+string(sig)+"'@\n")

	s := signers(t, line)
	v, err := sshsig.Verify(s, releasePolicy, []byte("seamless-release self-test\n"), sig)
	require.NoError(t, err)
	require.Equal(t, byte(0x05), v.Flags)
	// The self-test key signs nothing a release could use: it is not one of
	// the owner's.
	owners := signers(t, signerLines(readRepo(t, "release_signers"))...)
	_, err = sshsig.Verify(owners, releasePolicy, []byte("seamless-release self-test\n"), sig)
	require.ErrorIs(t, err, sshsig.ErrUnlisted)
}

// installerHarness runs docs/install's owner-signature check in sh: its
// constants and functions, the keys overridden with a throwaway one's, and
// stand-ins for the installer's own helpers. fetch_to copies from assets.
func runOwnerCheck(t *testing.T, path string, signersLines, version string, assets map[string][]byte) (string, error) {
	t.Helper()
	install := readRepo(t, "docs", "install")
	consts := regexp.MustCompile(`(?s)OWNER_SIGNED_AFTER=.*?-----END SSH SIGNATURE-----'\n`).FindString(install)
	require.NotEmpty(t, consts)
	funcs := between(t, install, "# BEGIN owner signature", "# END owner signature")

	dir := t.TempDir()
	tmp := filepath.Join(dir, "tmp")
	src := filepath.Join(dir, "release")
	require.NoError(t, os.MkdirAll(tmp, 0o700))
	require.NoError(t, os.MkdirAll(src, 0o700))
	for name, body := range assets {
		require.NoError(t, os.WriteFile(filepath.Join(src, name), body, 0o644))
	}
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "checksums.txt"), assets["checksums.txt"], 0o644))
	script := "set -eu\n" + consts + funcs + `
OWNER_SIGNERS='` + signersLines + `'
REPO=example/seamless
TMP=$HARNESS_TMP
VERSION=` + version + `
step() { printf 'STEP %s %s\n' "$1" "$2"; }
warn() { printf 'WARN %s\n' "$*"; }
die() { printf 'DIE %s\n' "$*"; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }
fetch_to() { cp "$HARNESS_SRC/${1##*/}" "$2"; }
verify_owner_signature https://example.invalid/releases/download/v` + version + `
`
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = []string{"PATH=" + path, "HARNESS_TMP=" + tmp, "HARNESS_SRC=" + src}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestInstaller_VerifyOwnerSignature(t *testing.T) {
	keygen := sshKeygen(t)
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	path := filepath.Dir(keygen) + string(os.PathListSeparator) + "/usr/bin:/bin"
	owner := newKey(t, sshsig.KeySKEd25519)
	stranger := newKey(t, sshsig.KeySKEd25519)
	line := owner.SignersLine("owner-a", "seamless-release")
	sums := []byte("aaaa  seamless_0.9.0_linux_amd64.tar.gz\n")
	signed := func(k *sshsigtest.Key, msg []byte, o sshsigtest.Options) []byte {
		return sign(t, k, "seamless-release", msg, o)
	}

	for _, tt := range []struct {
		name    string
		version string
		assets  map[string][]byte
		want    string
		fails   bool
	}{
		{"signed by a key it carries", "0.9.0", map[string][]byte{"checksums.txt": sums, "checksums.txt.sshsig": signed(owner, sums, sshsigtest.Options{})},
			"STEP owner ok (owner-a)", false},
		{"no signature", "0.9.0", map[string][]byte{"checksums.txt": sums}, "DIE NO OWNER SIGNATURE", true},
		{"a stranger's key", "0.9.0", map[string][]byte{"checksums.txt": sums, "checksums.txt.sshsig": signed(stranger, sums, sshsigtest.Options{})},
			"DIE OWNER SIGNATURE VERIFICATION FAILED", true},
		{"another file", "0.9.0", map[string][]byte{"checksums.txt": sums, "checksums.txt.sshsig": signed(owner, []byte("other\n"), sshsigtest.Options{})},
			"DIE OWNER SIGNATURE VERIFICATION FAILED", true},
		{"the wrong namespace", "0.9.0", map[string][]byte{"checksums.txt": sums, "checksums.txt.sshsig": sign(t, owner, "git", sums, sshsigtest.Options{})},
			"DIE OWNER SIGNATURE VERIFICATION FAILED", true},
		{"touched, not unlocked (flags 01)", "0.9.0", map[string][]byte{"checksums.txt": sums,
			"checksums.txt.sshsig": signed(owner, sums, sshsigtest.Options{Flags: sshsigtest.Flags(0x01)})}, "(flags 1, want 5)", true},
		{"a prerelease after the floor is held to it too", "0.9.0-rc.1", map[string][]byte{"checksums.txt": sums}, "DIE NO OWNER SIGNATURE", true},
		{"a release before the owner's signatures", "0.8.3", map[string][]byte{"checksums.txt": sums}, "STEP owner none (v0.8.3 predates", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			out, err := runOwnerCheck(t, path, line, tt.version, tt.assets)
			require.Contains(t, out, tt.want)
			if tt.fails {
				require.Error(t, err, out)
			} else {
				require.NoError(t, err, out)
			}
		})
	}

	// A PATH of the tools the check uses, and no ssh-keygen.
	bare := t.TempDir()
	for _, tool := range []string{"sh", "sed", "tr", "od", "awk", "cp", "head", "base64", "cat"} {
		p, err := exec.LookPath(tool)
		if err != nil {
			continue
		}
		require.NoError(t, os.Symlink(p, filepath.Join(bare, tool)))
	}
	t.Run("no ssh-keygen: it warns and goes on", func(t *testing.T) {
		out, err := runOwnerCheck(t, bare, line, "0.9.0", map[string][]byte{"checksums.txt": sums, "checksums.txt.sshsig": signed(stranger, sums, sshsigtest.Options{})})
		require.NoError(t, err, out)
		require.Contains(t, out, "WARN ssh-keygen here cannot verify")
	})
	t.Run("no ssh-keygen still refuses a release with no signature", func(t *testing.T) {
		out, err := runOwnerCheck(t, bare, line, "0.9.0", map[string][]byte{"checksums.txt": sums})
		require.Error(t, err, out)
		require.Contains(t, out, "DIE NO OWNER SIGNATURE")
	})
	t.Run("an ssh-keygen that cannot verify a security key's signature: it warns", func(t *testing.T) {
		old := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(old, "ssh-keygen"), []byte("#!/bin/sh\necho 'unknown option -- Y' >&2\nexit 1\n"), 0o755))
		out, err := runOwnerCheck(t, old+string(os.PathListSeparator)+bare, line, "0.9.0",
			map[string][]byte{"checksums.txt": sums, "checksums.txt.sshsig": signed(stranger, sums, sshsigtest.Options{})})
		require.NoError(t, err, out)
		require.Contains(t, out, "WARN ssh-keygen here cannot verify")
	})
}
