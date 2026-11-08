package main

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLaunchdCheckFor(t *testing.T) {
	const home = "/h"
	cur := "/h/Library/LaunchAgents/com.arctop.seamless.plist"
	old := "/h/Library/LaunchAgents/org.thereisnospoon.seamless.plist"
	tests := []struct {
		name       string
		plists     []string
		loaded     []string
		wantRow    bool
		wantStatus checkStatus
		wantDetail string
	}{
		{"nothing installed (Homebrew, serve by hand)", nil, nil, false, 0, ""},
		{"current, loaded", []string{cur}, []string{launchdLabel}, true, statusOK,
			"com.arctop.seamless (loaded)"},
		{"current, stopped", []string{cur}, nil, true, statusInfo,
			"com.arctop.seamless installed, not loaded (seamlessd start loads it)"},
		{"legacy only, loaded", []string{old}, []string{legacyLaunchdLabel}, true, statusInfo,
			"org.thereisnospoon.seamless (loaded), the label before the rename to com.arctop.seamless; re-run the installer or `make install` to move it"},
		{"both loaded: two daemons", []string{cur, old}, []string{launchdLabel, legacyLaunchdLabel}, true, statusWarn,
			"two jobs are loaded, com.arctop.seamless and org.thereisnospoon.seamless, each running seamlessd serve; re-run the installer"},
		{"legacy job loaded with its plist deleted", []string{cur}, []string{launchdLabel, legacyLaunchdLabel}, true, statusWarn,
			"two jobs are loaded"},
		{"current loaded, legacy plist left behind", []string{cur, old}, []string{launchdLabel}, true, statusWarn,
			"com.arctop.seamless loaded, and the legacy org.thereisnospoon.seamless is still installed; re-run the installer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exists := func(p string) bool { return slices.Contains(tt.plists, filepath.ToSlash(p)) }
			loaded := func(label string) bool { return slices.Contains(tt.loaded, label) }
			c, ok := launchdCheckFor(home, exists, loaded)
			require.Equal(t, tt.wantRow, ok)
			if !ok {
				return
			}
			require.Equal(t, "launchd", c.name)
			require.Equal(t, tt.wantStatus, c.status)
			require.Contains(t, c.detail, tt.wantDetail)
		})
	}
	require.Nil(t, launchdChecks("linux", home, 1000), "no launchd line off macOS")
}
