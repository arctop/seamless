package update

import (
	"path"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/shellcmd"
)

// The update command every surface prints for an install names sudo by
// absolute path wherever it goes through sudo: the owner's PATH can lead
// through a directory the owner's processes write (owner decision 2026-11-18,
// task L4).
func TestCLHints_NameSudoByPath(t *testing.T) {
	sudos := 0
	for _, k := range []Kind{KindInstaller, KindSystem, KindHomebrew, KindSource, KindClient, KindUnknown} {
		hint := Hint(k)
		first, _, _ := strings.Cut(hint, " ")
		if path.Base(first) != "sudo" {
			continue
		}
		sudos++
		require.Equal(t, shellcmd.Sudo, first, "%s: %s", k, hint)
	}
	require.Equal(t, 1, sudos, "the system daemon's hint alone runs as another account")
	require.Equal(t, "/usr/bin/sudo -u _seamless /opt/seamless/bin/seamlessd update --config /opt/seamless/etc/seamless.yaml", SystemHint)
}
