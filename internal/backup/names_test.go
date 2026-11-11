package backup

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/archive"
)

func TestFileName_RoundTrips(t *testing.T) {
	at := time.Date(2026, 11, 10, 3, 0, 0, 0, time.FixedZone("PST", -8*3600))
	for _, tc := range []struct {
		kind, want, wantKind string
	}{
		{archive.KindScheduled, "seamless-mac-auto-20261110T110000Z-auto.tar.gz", archive.KindScheduled},
		{archive.KindManual, "seamless-mac-auto-20261110T110000Z.tar.gz", archive.KindManual},
		{"", "seamless-mac-auto-20261110T110000Z.tar.gz", archive.KindManual},
		{archive.KindBeforeRestore, "seamless-mac-auto-20261110T110000Z-before-restore.tar.gz", archive.KindBeforeRestore},
	} {
		// A host that itself ends in -auto: the name parses from the right.
		name := FileName("Mac-Auto", at, tc.kind)
		require.Equal(t, tc.want, name)
		got, ok := ParseName(name)
		require.True(t, ok, name)
		require.Equal(t, Name{Kind: tc.wantKind, Host: "mac-auto", At: at.UTC()}, got)
	}
}

func TestHostForFilename(t *testing.T) {
	for in, want := range map[string]string{
		"example-remote":           "example-remote",
		"NUC.local":                "nuc.local",
		"host/with/slash":          "host-with-slash",
		`win\box`:                  "win-box",
		"host:1":                   "host-1",
		"a_b:c":                    "a_b-c",
		" The Studio's Mac.local ": "the-studio-s-mac.local",
		"":                         "unknown",
		"   ":                      "unknown",
		"///":                      "unknown",
		"..//":                     "unknown",
		"example-laptop13":         "example-laptop13",
	} {
		require.Equal(t, want, HostForFilename(in), "HostForFilename(%q)", in)
	}
}

func TestParseName(t *testing.T) {
	at := time.Date(2026, 11, 10, 3, 0, 0, 0, time.UTC)
	got, ok := ParseName("pre-update-v0.8.1-20261110T030000Z.tar.gz")
	require.True(t, ok)
	require.Equal(t, Name{Kind: archive.KindPreUpdate, At: at, FromVersion: "0.8.1"}, got)

	got, ok = ParseName("pre-update-v0.9.0-rc.1-20261110T030000Z.tar.gz")
	require.True(t, ok)
	require.Equal(t, "0.9.0-rc.1", got.FromVersion)

	for _, name := range []string{
		"",
		".seamless-partial-01X-01Y.tmp",
		".seamless-host-20261110T030000Z.tar.gz",
		"seamless-host-20261110T030000Z.tar",
		"seamless-host-20261110T030000Z.zip",
		"seamless-20261110T030000Z.tar.gz",      // no host
		"seamless--20261110T030000Z.tar.gz",     // empty host
		"seamless-host-2026111T030000Z.tar.gz",  // short stamp
		"seamless-host_20261110T030000Z.tar.gz", // no dash before the stamp
		"seamless-host-20261310T030000Z.tar.gz", // month 13
		"seamless-host-20261110T030000Z-weekly.tar.gz",
		"my-export.tar.gz",
		"pre-update-vlatest-20261110T030000Z.tar.gz",
		"pre-update-v-20261110T030000Z.tar.gz",
		"pre-update-v0.8.1.tar.gz",
		"dir/seamless-host-20261110T030000Z.tar.gz",
		`dir\seamless-host-20261110T030000Z.tar.gz`,
	} {
		_, ok := ParseName(name)
		require.False(t, ok, name)
	}
}

func TestPartialNames(t *testing.T) {
	name := partialName("01INSTALL", "01JOB")
	_, parsed := ParseName(name)
	require.False(t, parsed, "a partial is never listed")
	require.True(t, isOwnPartial(name, "01INSTALL"))
	require.False(t, isOwnPartial(name, "01OTHER"), "another install's partial is not ours to sweep")
	require.False(t, isOwnPartial(name, ""))
	require.False(t, isOwnPartial(".pre-update-123.partial", "01INSTALL"), "the updater's are never ours")
}
