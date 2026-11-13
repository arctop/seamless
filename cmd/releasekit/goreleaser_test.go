package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const repoRoot = "../.."

func repoConfig(t *testing.T) grConfig {
	t.Helper()
	cfg, err := loadGoreleaser(repoRoot)
	require.NoError(t, err)
	return cfg
}

// The repository's own .goreleaser.yaml is what the darwin half reads: both
// builds, both architectures, one tar.gz archive.
func TestParseGoreleaser_RepoConfig(t *testing.T) {
	cfg := repoConfig(t)
	require.Equal(t, "seamless", cfg.ProjectName)
	var ids []string
	for _, b := range cfg.Builds {
		ids = append(ids, b.ID+"/"+b.Binary)
		require.Equal(t, []string{"amd64", "arm64"}, b.Goarch, b.ID)
		require.NotContains(t, b.Goos, "darwin", b.ID)
		require.Contains(t, b.Env, "CGO_ENABLED=0", b.ID)
		require.Contains(t, b.Flags, "-trimpath", b.ID)
	}
	require.Equal(t, []string{"seamlessd/seamlessd", "seam/seam"}, ids)

	names, err := cfg.darwinArchiveNames("0.9.0")
	require.NoError(t, err)
	require.Equal(t, []string{"seamless_0.9.0_darwin_amd64.tar.gz", "seamless_0.9.0_darwin_arm64.tar.gz"}, names)
	require.Equal(t, "seamless-approve_0.9.0_darwin.zip", approverAsset("0.9.0"))
}

func TestParseGoreleaser_RefusesWhatItDoesNotImplement(t *testing.T) {
	const build = `project_name: p
builds:
  - id: a
    main: ./cmd/a
    binary: a
    goos: [linux]
    goarch: [arm64]
`
	const archive = `archives:
  - formats: [tar.gz]
    name_template: "{{ .ProjectName }}_{{ .Version }}_{{ .Os }}_{{ .Arch }}"
    files:
      - LICENSE
`
	for _, tc := range []struct {
		name, yaml, want string
	}{
		{"a build setting it does not know", strings.Replace(build, "    binary: a\n", "    binary: a\n    gcflags: [-N]\n", 1) + archive, `"gcflags" is not implemented`},
		{"a build of darwin", strings.Replace(build, "goos: [linux]", "goos: [darwin, linux]", 1) + archive, "builds darwin itself"},
		{"an architecture darwin lacks", strings.Replace(build, "goarch: [arm64]", "goarch: [arm64, 386]", 1) + archive, "goarch 386 has no darwin build"},
		{"a wrapped archive", build + strings.Replace(archive, "    files:", "    wrap_in_directory: true\n    files:", 1), `"wrap_in_directory" is not implemented`},
		{"a file with info", build + strings.Replace(archive, "      - LICENSE\n", "      - src: LICENSE\n        info:\n          mode: 0600\n", 1), `"info" is not implemented`},
		{"a zip archive", build + strings.Replace(archive, "[tar.gz]", "[zip]", 1), "want [tar.gz]"},
		{"a darwin override", build + strings.Replace(archive, "    files:", "    format_overrides:\n      - goos: darwin\n        formats: [zip]\n    files:", 1), "darwin format override"},
		{"two archives", build + archive + "  - formats: [tar.gz]\n    name_template: x\n", "exactly one archives entry"},
		{"no builds", "project_name: p\n" + archive, "no builds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseGoreleaser([]byte(tc.yaml))
			require.ErrorContains(t, err, tc.want)
		})
	}
	_, err := parseGoreleaser([]byte(build + archive))
	require.NoError(t, err, "the base case parses")
}

// A template may name only the plain fields releasekit fills in: a function,
// or a field it lacks, fails rather than rendering something goreleaser
// would not.
func TestRender_RefusesWhatItCannotFill(t *testing.T) {
	data := map[string]string{"Version": "0.9.0"}
	got, err := render("t", "v{{ .Version }}", data)
	require.NoError(t, err)
	require.Equal(t, "v0.9.0", got)
	_, err = render("t", "{{ .Env.FOO }}", data)
	require.Error(t, err)
	_, err = render("t", `{{ envOrDefault "FOO" "" }}`, data)
	require.Error(t, err)
	_, err = render("t", "{{ .ShortCommit }}", data)
	require.Error(t, err)
}

func testRelease() release {
	return release{
		Tag: "v0.9.0", Version: "0.9.0",
		FullCommit: "0123456789abcdef0123456789abcdef01234567", Short: "0123456",
		CommitTime: time.Unix(1_790_000_000, 0).UTC(),
		Date:       "2026-11-13T12:00:00Z",
		Toolchain:  "go1.26.11",
		repoRoot:   repoRoot,
	}
}

// The darwin build is goreleaser's build of the same entry: its flags, its
// ldflags rendered and joined into one -ldflags, its env, plus darwin and
// go.mod's toolchain. Anything in the caller's environment that changes the
// code is dropped, as CI's runner does not set it.
func TestBuildCommand_IsGoreleasersBuild(t *testing.T) {
	t.Setenv("GOFLAGS", "-race")
	t.Setenv("GOAMD64", "v3")
	t.Setenv("CGO_ENABLED", "1")
	cfg := repoConfig(t)
	r := testRelease()

	cmd, err := buildCommand(r, cfg.Builds[0], "arm64", "/out/seamlessd")
	require.NoError(t, err)
	require.Equal(t, repoRoot, cmd.Dir)
	require.Equal(t, "build", cmd.Args[1])
	require.Equal(t, "-trimpath", cmd.Args[2])
	ld, ok := strings.CutPrefix(cmd.Args[3], "-ldflags=")
	require.True(t, ok, cmd.Args)
	require.Equal(t, []string{"-o", "/out/seamlessd", "./cmd/seamlessd"}, cmd.Args[4:])
	require.NotContains(t, ld, "{{")
	for _, want := range []string{"-s -w", "-X main.version=0.9.0", "-X main.commit=0123456", "-X main.buildDate=2026-11-13T12:00:00Z"} {
		require.Contains(t, ld, want)
	}
	// The darwin build is a release build, which the update check and the
	// updater rely on (internal/update.IsReleaseBuild).
	require.Contains(t, ld, "main.distribution=release")

	env := map[string][]string{}
	for _, kv := range cmd.Env {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = append(env[k], v)
	}
	require.Equal(t, []string{"0"}, env["CGO_ENABLED"])
	require.Equal(t, []string{"darwin"}, env["GOOS"])
	require.Equal(t, []string{"arm64"}, env["GOARCH"])
	require.Equal(t, []string{"go1.26.11"}, env["GOTOOLCHAIN"])
	require.Empty(t, env["GOFLAGS"])
	require.Empty(t, env["GOAMD64"])

	seam, err := buildCommand(r, cfg.Builds[1], "amd64", "/out/seam")
	require.NoError(t, err)
	require.Equal(t, []string{"build", "-trimpath", "-ldflags=-s -w", "-o", "/out/seam", "./cmd/seam"}, seam.Args[1:])
}

func TestToolchainOf(t *testing.T) {
	tc, err := toolchainOf("1.26.0", "go1.26.11")
	require.NoError(t, err)
	require.Equal(t, "go1.26.11", tc)
	tc, err = toolchainOf("1.26.0", "")
	require.NoError(t, err)
	require.Equal(t, "go1.26.0", tc)
	_, err = toolchainOf("", "")
	require.Error(t, err)

	// The repository's go.mod, which CI's setup-go reads the same way.
	tc, err = goModToolchain(repoRoot)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(tc, "go1."), tc)
}

func TestVersionOf(t *testing.T) {
	for tag, want := range map[string]string{"v0.9.0": "0.9.0", "v1.2.3-rc.1": "1.2.3-rc.1"} {
		got, err := versionOf(tag)
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
	for _, bad := range []string{"0.9.0", "v0.9", "v0.9.0 ", "v0.9.0/x", "v0.9.0-", "site-1"} {
		_, err := versionOf(bad)
		require.Error(t, err, bad)
	}
}

type tarEntry struct {
	name  string
	mode  int64
	mtime time.Time
	uid   int
	body  string
}

func readTarGz(t *testing.T, p string) []tarEntry {
	t.Helper()
	raw, err := os.ReadFile(p)
	require.NoError(t, err)
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	require.NoError(t, err)
	tr := tar.NewReader(zr)
	var out []tarEntry
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		require.Equal(t, byte(tar.TypeReg), hdr.Typeflag, hdr.Name)
		body, err := io.ReadAll(tr)
		require.NoError(t, err)
		out = append(out, tarEntry{hdr.Name, hdr.Mode, hdr.ModTime, hdr.Uid, string(body)})
	}
	return out
}

// The darwin archives hold what goreleaser's held for v0.8.2, in the same
// order: the archive files sorted by name, then the two binaries, at the top
// level, regular files, 0644 and 0755. The updater and both installers read
// the binaries from the top level by name.
func TestDarwinArchive_IsLaidOutAsV082(t *testing.T) {
	cfg := repoConfig(t)
	r := testRelease()
	bin := t.TempDir()
	for _, b := range cfg.Builds {
		for _, arch := range []string{"amd64", "arm64"} {
			p := binaryPath(bin, b, arch)
			require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
			require.NoError(t, os.WriteFile(p, []byte(b.Binary+" "+arch), 0o755))
		}
	}
	out := t.TempDir()
	written, err := darwinArchive(cfg, r, bin, out)
	require.NoError(t, err)
	require.Equal(t, []string{
		filepath.Join(out, "seamless_0.9.0_darwin_amd64.tar.gz"),
		filepath.Join(out, "seamless_0.9.0_darwin_arm64.tar.gz"),
	}, written)

	entries := readTarGz(t, written[1])
	var names []string
	for _, e := range entries {
		names = append(names, e.name)
		require.True(t, e.mtime.Equal(r.CommitTime), e.name)
		require.Zero(t, e.uid, e.name)
	}
	// `tar -tzf seamless_0.8.2_darwin_arm64.tar.gz`, in its order.
	require.Equal(t, []string{
		"LICENSE",
		"README.md",
		"seam-onboard/SKILL.md",
		"seam-onboard/agents/openai.yaml",
		"seam-research/SKILL.md",
		"seam-research/agents/openai.yaml",
		"seamless.yaml.example",
		"seamlessd",
		"seam",
	}, names)
	require.Equal(t, "seamlessd arm64", entries[7].body)
	require.Equal(t, "seam arm64", entries[8].body)
	require.Equal(t, int64(0o755), entries[7].mode)
	require.Equal(t, int64(0o644), entries[0].mode)
	license, err := os.ReadFile(filepath.Join(repoRoot, "LICENSE"))
	require.NoError(t, err)
	require.Equal(t, string(license), entries[0].body)

	// The archive is a function of its inputs.
	again := t.TempDir()
	_, err = darwinArchive(cfg, r, bin, again)
	require.NoError(t, err)
	first, err := os.ReadFile(written[1])
	require.NoError(t, err)
	second, err := os.ReadFile(filepath.Join(again, "seamless_0.9.0_darwin_arm64.tar.gz"))
	require.NoError(t, err)
	require.Equal(t, first, second)

	// A binary missing from the build directory fails the archive.
	require.NoError(t, os.Remove(binaryPath(bin, cfg.Builds[1], "amd64")))
	_, err = darwinArchive(cfg, r, bin, t.TempDir())
	require.Error(t, err)
}

func TestArchiveFiles_GoreleaserSemantics(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{"LICENSE", "deep/dir/SKILL.md", "deep/dir/other.md"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, filepath.Dir(p)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(root, p), []byte(p), 0o644))
	}
	got, err := archiveFiles(root, []grFile{
		{Src: "LICENSE"},
		{Src: "deep/dir/SKILL.md", Dst: "skill", StripParent: true},
		{Src: "deep/dir/other.md", Dst: "kept"},
	})
	require.NoError(t, err)
	var names []string
	for _, e := range got {
		names = append(names, e.Name)
	}
	require.Equal(t, []string{"LICENSE", "kept/deep/dir/other.md", "skill/SKILL.md"}, names)

	_, err = archiveFiles(root, []grFile{{Src: "missing.md"}})
	require.ErrorContains(t, err, "matches nothing")
	require.NoError(t, os.Symlink("LICENSE", filepath.Join(root, "link")))
	_, err = archiveFiles(root, []grFile{{Src: "link"}})
	require.ErrorContains(t, err, "not a regular file")
	require.True(t, slices.IsSortedFunc(got, func(a, b archiveEntry) int { return strings.Compare(a.Name, b.Name) }))
}
