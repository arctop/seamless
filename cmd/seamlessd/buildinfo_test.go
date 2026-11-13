package main

import (
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/arctop/seamless/internal/update"
	"github.com/stretchr/testify/require"
)

// fakeBuildInfo is a synthetic build info: main module version plus vcs
// settings as alternating key/value pairs.
func fakeBuildInfo(mainVersion string, kv ...string) *debug.BuildInfo {
	info := &debug.BuildInfo{Main: debug.Module{Path: "github.com/arctop/seamless", Version: mainVersion}}
	for i := 0; i+1 < len(kv); i += 2 {
		info.Settings = append(info.Settings, debug.BuildSetting{Key: kv[i], Value: kv[i+1]})
	}
	return info
}

const (
	testRevision = "2d3e1be73770fa365727bbf88648ee15a030de4f"
	testVCSTime  = "2026-10-09T04:33:12Z"
)

// gitBuildInfo is what `go build` stamps in a git checkout: Main.Version as
// the toolchain derives it, plus the vcs settings.
func gitBuildInfo(mainVersion string, modified bool) *debug.BuildInfo {
	mod := "false"
	if modified {
		mod = "true"
	}
	return fakeBuildInfo(mainVersion,
		"vcs", "git", "vcs.revision", testRevision, "vcs.time", testVCSTime, "vcs.modified", mod)
}

func TestResolveBuildMeta(t *testing.T) {
	tests := []struct {
		name    string
		stamped buildMeta
		info    *debug.BuildInfo
		want    buildMeta
	}{
		{
			name:    "module proxy install of a release",
			stamped: unstampedMeta,
			info:    fakeBuildInfo("v0.6.0"),
			want:    buildMeta{version: "0.6.0", commit: "unknown", date: "unknown"},
		},
		{
			name:    "module proxy install of an untagged commit",
			stamped: unstampedMeta,
			info:    fakeBuildInfo("v0.6.1-0.20261009043312-2d3e1be73770"),
			want:    buildMeta{version: "0.6.1-0.20261009043312-2d3e1be73770", commit: "unknown", date: "unknown"},
		},
		{
			name:    "go build at a clean tag",
			stamped: unstampedMeta,
			info:    gitBuildInfo("v0.6.0", false),
			want:    buildMeta{version: "0.6.0", commit: "2d3e1be", date: testVCSTime},
		},
		{
			name:    "go build at a dirty tag",
			stamped: unstampedMeta,
			info:    gitBuildInfo("v0.6.0+dirty", true),
			want:    buildMeta{version: "0.6.0-dirty", commit: "2d3e1be", date: "unknown"},
		},
		{
			name:    "go build past a tag, clean",
			stamped: unstampedMeta,
			info:    gitBuildInfo("v0.6.1-0.20261009043312-2d3e1be73770", false),
			want:    buildMeta{version: "0.6.1-0.20261009043312-2d3e1be73770", commit: "2d3e1be", date: testVCSTime},
		},
		{
			name:    "go build past a tag, dirty",
			stamped: unstampedMeta,
			info:    gitBuildInfo("v0.6.1-0.20261009043312-2d3e1be73770+dirty", true),
			want:    buildMeta{version: "0.6.1-0.20261009043312-2d3e1be73770-dirty", commit: "2d3e1be", date: "unknown"},
		},
		{
			// What both `go test` and `go build -buildvcs=false` record.
			name:    "devel build keeps the defaults",
			stamped: unstampedMeta,
			info:    fakeBuildInfo("(devel)"),
			want:    unstampedMeta,
		},
		{
			name:    "vcs settings without a module version",
			stamped: unstampedMeta,
			info:    gitBuildInfo("(devel)", false),
			want:    buildMeta{version: devVersion, commit: "2d3e1be", date: testVCSTime},
		},
		{
			name:    "revision shorter than the short length is kept whole",
			stamped: unstampedMeta,
			info:    fakeBuildInfo("", "vcs.revision", "abc"),
			want:    buildMeta{version: devVersion, commit: "abc", date: "unknown"},
		},
		{
			name:    "no build info",
			stamped: unstampedMeta,
			info:    nil,
			want:    unstampedMeta,
		},
		// The stamped routes win wholesale, whatever the build info says.
		{
			name:    "goreleaser release stamp wins",
			stamped: buildMeta{version: "0.6.0", commit: "1a2b3c4", date: "2026-10-09T05:00:00Z"},
			info:    gitBuildInfo("v0.6.1-0.20261009043312-2d3e1be73770+dirty", true),
			want:    buildMeta{version: "0.6.0", commit: "1a2b3c4", date: "2026-10-09T05:00:00Z"},
		},
		{
			name:    "goreleaser snapshot stamp wins",
			stamped: buildMeta{version: "0.6.1-SNAPSHOT-1a2b3c4", commit: "1a2b3c4", date: "2026-10-09T05:00:00Z"},
			info:    fakeBuildInfo("v0.6.0"),
			want:    buildMeta{version: "0.6.1-SNAPSHOT-1a2b3c4", commit: "1a2b3c4", date: "2026-10-09T05:00:00Z"},
		},
		{
			name:    "make build off a tag keeps its stamped dev sentinel",
			stamped: buildMeta{version: devVersion, commit: "1a2b3c4", date: "2026-10-09T05:00:00Z"},
			info:    gitBuildInfo("v0.0.0-20261009043312-2d3e1be73770", false),
			want:    buildMeta{version: devVersion, commit: "1a2b3c4", date: "2026-10-09T05:00:00Z"},
		},
		{
			name:    "make build without git stamps only the date",
			stamped: buildMeta{version: devVersion, commit: "unknown", date: "2026-10-09T05:00:00Z"},
			info:    fakeBuildInfo("v0.6.0"),
			want:    buildMeta{version: devVersion, commit: "unknown", date: "2026-10-09T05:00:00Z"},
		},
		{
			name:    "a lone version stamp wins",
			stamped: buildMeta{version: "1.2.3", commit: "unknown", date: "unknown"},
			info:    gitBuildInfo("v0.6.0", false),
			want:    buildMeta{version: "1.2.3", commit: "unknown", date: "unknown"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, resolveBuildMeta(tt.stamped, tt.info))
		})
	}
}

// TestResolveBuildMeta_UpdateCheck pins how `seamlessd update --check` reads a
// build-info version: only a clean release compares, so a pseudo-version or a
// dirty build is reported as a development build rather than "up to date".
func TestResolveBuildMeta_UpdateCheck(t *testing.T) {
	const latest = "v0.6.0"
	tests := []struct {
		name    string
		info    *debug.BuildInfo
		wantCmp int
		wantOK  bool
	}{
		{"proxy install of the latest release", fakeBuildInfo("v0.6.0"), 0, true},
		{"proxy install of an older release", fakeBuildInfo("v0.5.4"), -1, true},
		{"proxy install of an untagged commit", fakeBuildInfo("v0.6.1-0.20261009043312-2d3e1be73770"), 0, false},
		{"go build at a clean tag", gitBuildInfo("v0.6.0", false), 0, true},
		{"go build at a dirty tag", gitBuildInfo("v0.6.0+dirty", true), 0, false},
		{"go build past a tag", gitBuildInfo("v0.6.1-0.20261009043312-2d3e1be73770+dirty", true), 0, false},
		{"go test binary", fakeBuildInfo("(devel)"), 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := resolveBuildMeta(unstampedMeta, tt.info)
			cmp, ok := update.Compare(m.version, latest)
			require.Equal(t, tt.wantOK, ok, "version %q", m.version)
			require.Equal(t, tt.wantCmp, cmp)
		})
	}
}

// TestResolveBuildMeta_NoPlusInVersion guards the seam CLI contract: seam
// rebuilds `seamlessd version` from /healthz by cutting buildVersion() at the
// first "+", so a resolved version must not carry one of its own.
func TestResolveBuildMeta_NoPlusInVersion(t *testing.T) {
	for _, mv := range []string{"v0.6.0+dirty", "v0.6.1-0.20261009043312-2d3e1be73770+dirty", "v2.0.0+incompatible"} {
		m := resolveBuildMeta(unstampedMeta, gitBuildInfo(mv, true))
		require.NotContains(t, m.version, "+", mv)
	}
}

// TestDistributionDefaultsToSource: an unstamped build (go test, go build,
// make install) is a source build, which keeps the background update check
// off GitHub and out of the data dir (constraint
// dev-and-fixture-daemons-never-self-update).
func TestDistributionDefaultsToSource(t *testing.T) {
	require.Equal(t, update.DistributionSource, distribution)
}

// TestOnlyGoreleaserStampsRelease guards the same constraint from the other
// side: the release pipeline is the one build that stamps
// main.distribution, and no other script that links seamlessd may. The
// darwin half of a release (scripts/release/macos.sh) builds through
// cmd/releasekit, which reads the stamp from .goreleaser.yaml's ldflags
// rather than spelling it.
func TestOnlyGoreleaserStampsRelease(t *testing.T) {
	root := filepath.Join("..", "..")
	gr, err := os.ReadFile(filepath.Join(root, ".goreleaser.yaml"))
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(string(gr), "-X main.distribution=release"))

	for _, f := range []string{
		"Makefile",
		filepath.Join("deploy", "glama", "build.sh"),
		filepath.Join("deploy", "glama", "Dockerfile"),
		filepath.Join("scripts", "fixture", "harness.sh"),
		filepath.Join("scripts", "release", "macos.sh"),
		filepath.Join("cmd", "releasekit", "darwin.go"),
		filepath.Join("cmd", "releasekit", "goreleaser.go"),
	} {
		b, err := os.ReadFile(filepath.Join(root, f))
		require.NoError(t, err, f)
		require.NotContains(t, string(b), "main.distribution", "%s must not stamp the release distribution", f)
	}
}
