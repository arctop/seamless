package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	arm64Archive = "seamless_0.9.0_darwin_arm64.tar.gz"
	amd64Archive = "seamless_0.9.0_darwin_amd64.tar.gz"
	approverZip  = "seamless-approve_0.9.0_darwin.zip"
)

// draft writes files into a fresh directory, the way release.yml downloads a
// draft release's assets, and returns it.
func draft(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644))
	}
	return dir
}

func sum(body string) string {
	s := sha256.Sum256([]byte(body))
	return hex.EncodeToString(s[:])
}

// tagObject is a signed annotated tag as `git cat-file tag` prints it, its
// message carrying lines.
func tagObject(lines ...string) []byte {
	return []byte("object 0123456789abcdef0123456789abcdef01234567\ntype commit\ntag v0.9.0\n" +
		"tagger Owner <owner@example.com> 1790000000 +0000\n\nv0.9.0\n\n" +
		"macOS assets, built, signed and notarized by scripts/release/macos.sh (sha256):\n" +
		strings.Join(lines, "\n") + "\n" +
		"-----BEGIN SSH SIGNATURE-----\nU1NIU0lHAAAAAQAAADMAAAALc3NoLWVkMjU1MTkAAAAg\n" +
		strings.Repeat("ab", 32) + "  not-a-manifest-line\n-----END SSH SIGNATURE-----\n")
}

func line(name, body string) string { return sum(body) + "  " + name }

var goodAssets = map[string]string{arm64Archive: "arm64 bytes", amd64Archive: "amd64 bytes", approverZip: "approver bytes"}

func goodTag() []byte {
	return tagObject(line(amd64Archive, "amd64 bytes"), line(arm64Archive, "arm64 bytes"), line(approverZip, "approver bytes"))
}

// What macos.sh writes into the tag is what release.yml accepts: the
// manifest round-trips, and the signature block (even a line in it that
// looks like a manifest line) is not read.
func TestManifest_RoundTrip(t *testing.T) {
	dir := draft(t, goodAssets)
	var buf bytes.Buffer
	paths := []string{filepath.Join(dir, arm64Archive), filepath.Join(dir, approverZip), filepath.Join(dir, amd64Archive)}
	require.NoError(t, writeManifest(&buf, paths))
	require.Equal(t, line(approverZip, "approver bytes")+"\n"+line(amd64Archive, "amd64 bytes")+"\n"+line(arm64Archive, "arm64 bytes")+"\n", buf.String())

	checked, err := checkDarwin(repoConfig(t), "v0.9.0", tagObject(strings.Split(strings.TrimSpace(buf.String()), "\n")...), dir)
	require.NoError(t, err)
	require.Equal(t, []string{filepath.Join(dir, approverZip), filepath.Join(dir, amd64Archive), filepath.Join(dir, arm64Archive)}, checked)
}

func TestWriteManifest_Refuses(t *testing.T) {
	dir := draft(t, map[string]string{"a b.tar.gz": "x"})
	require.ErrorContains(t, writeManifest(&bytes.Buffer{}, []string{filepath.Join(dir, "a b.tar.gz")}), "not a name")
	dir = draft(t, map[string]string{"a.zip": "x"})
	require.ErrorContains(t, writeManifest(&bytes.Buffer{}, []string{filepath.Join(dir, "a.zip"), filepath.Join(dir, "a.zip")}), "twice")
}

// The approver is optional, the darwin archives are not.
func TestCheckDarwin_ApproverIsOptional(t *testing.T) {
	dir := draft(t, map[string]string{arm64Archive: "arm64 bytes", amd64Archive: "amd64 bytes"})
	_, err := checkDarwin(repoConfig(t), "v0.9.0", tagObject(line(amd64Archive, "amd64 bytes"), line(arm64Archive, "arm64 bytes")), dir)
	require.NoError(t, err)
}

// Every way a draft or a tag can disagree is refused, each with its own
// reason, and nothing passes on a partial match.
func TestCheckDarwin_Refuses(t *testing.T) {
	cfg := repoConfig(t)
	withAsset := func(name, body string) map[string]string {
		m := map[string]string{}
		for k, v := range goodAssets {
			m[k] = v
		}
		if body == "" {
			delete(m, name)
		} else {
			m[name] = body
		}
		return m
	}
	for _, tc := range []struct {
		name   string
		tag    string
		object []byte
		assets map[string]string
		want   string
	}{
		{"a tag with no manifest", "v0.9.0", tagObject(), goodAssets, "carries no macOS manifest"},
		{"an asset swapped on the draft", "v0.9.0", goodTag(), withAsset(arm64Archive, "other bytes"), "the draft's " + arm64Archive + " has sha256"},
		{"an asset the manifest does not name", "v0.9.0", goodTag(), withAsset("seamless_0.9.0_linux_amd64.tar.gz", "x"), "the draft holds seamless_0.9.0_linux_amd64.tar.gz, which the tag's manifest does not name"},
		{"an asset missing from the draft", "v0.9.0", goodTag(), withAsset(approverZip, ""), "the draft lacks " + approverZip},
		{"a manifest naming a foreign file", "v0.9.0",
			tagObject(line(amd64Archive, "amd64 bytes"), line(arm64Archive, "arm64 bytes"), line("evil.tar.gz", "x")),
			withAsset("evil.tar.gz", "x"), "names evil.tar.gz, which is no macOS asset of v0.9.0"},
		{"a manifest without a darwin archive", "v0.9.0",
			tagObject(line(arm64Archive, "arm64 bytes"), line(approverZip, "approver bytes")),
			withAsset(amd64Archive, ""), "does not name " + amd64Archive},
		{"another version's manifest", "v0.9.1", goodTag(), goodAssets, "no macOS asset of v0.9.1"},
		{"a malformed manifest line", "v0.9.0",
			tagObject(line(amd64Archive, "amd64 bytes"), sum("arm64 bytes")+" "+arm64Archive), goodAssets, "is not a manifest line"},
		{"an uppercase digest", "v0.9.0",
			tagObject(strings.ToUpper(sum("amd64 bytes")) + "  " + amd64Archive), goodAssets, "is not a manifest line"},
		{"a name given twice", "v0.9.0",
			tagObject(line(amd64Archive, "amd64 bytes"), line(amd64Archive, "amd64 bytes")), goodAssets, "names " + amd64Archive + " twice"},
		{"a tag that is no release tag", "v0.9", goodTag(), goodAssets, "is not a release tag"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := draft(t, tc.assets)
			checked, err := checkDarwin(cfg, tc.tag, tc.object, dir)
			require.ErrorContains(t, err, tc.want)
			require.Nil(t, checked)
		})
	}

	// A directory in the draft's download is not an asset.
	dir := draft(t, goodAssets)
	require.NoError(t, os.Mkdir(filepath.Join(dir, "sub"), 0o755))
	_, err := checkDarwin(cfg, "v0.9.0", goodTag(), dir)
	require.ErrorContains(t, err, "not a regular file")
}

func TestTagMessage(t *testing.T) {
	require.Equal(t, "v0.9.0\n\nbody\n", string(tagMessage([]byte("object x\ntype commit\ntag v0.9.0\n\nv0.9.0\n\nbody\n-----BEGIN SSH SIGNATURE-----\nAAA\n-----END SSH SIGNATURE-----\n"))))
	require.Equal(t, "plain message\n", string(tagMessage([]byte("plain message\n"))))
	require.Empty(t, tagMessage([]byte("object x\ntype commit\n")))
}
