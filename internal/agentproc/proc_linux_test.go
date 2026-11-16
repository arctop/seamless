//go:build linux

package agentproc

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseStat(t *testing.T) {
	// 52 fields as the kernel writes them; starttime (field 22) is 98765.
	const tail = " S 4242 100 100 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 1 0 98765 1000000 200 18446744073709551615 0 0 0 0 0 0 0 0 0 0 0 0 17 3 0 0 0 0 0 0 0 0 0 0 0 0 0"
	for _, tc := range []struct {
		name string
		line string
		want procInfo
		ok   bool
	}{
		{"plain", "777 (claude)" + tail, procInfo{ppid: 4242, name: "claude", start: 98765}, true},
		{"comm with spaces and parens", "777 (a (b) c)" + tail, procInfo{ppid: 4242, name: "a (b) c", start: 98765}, true},
		{"truncated", "777 (claude) S 4242", procInfo{}, false},
		{"no comm", "777 claude S 4242", procInfo{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseStat(tc.line)
			if !tc.ok {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestAnchor_PassesOverAHookScript runs this test binary again under a
// #!/bin/sh script named like a git hook, and the child must name this process:
// the walk takes the script for the shell running it. stat's comm names such a
// script after itself ("reference-trans"), and a walk that read only comm
// stopped at the hook (ExeName).
func TestAnchor_PassesOverAHookScript(t *testing.T) {
	if os.Getenv("AGENTPROC_ANCHOR_HELPER") == "1" {
		anchor, ok := Anchor()
		fmt.Printf("anchor=%s ok=%v\n", anchor, ok)
		return
	}
	exe, err := os.Executable()
	require.NoError(t, err)
	script := filepath.Join(t.TempDir(), "reference-transaction")
	// The exit after "$@" keeps the shell from exec'ing the helper in its place.
	require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\n\"$@\"\nexit $?\n"), 0o755))
	cmd := exec.Command(script, exe, "-test.run=^TestAnchor_PassesOverAHookScript$")
	cmd.Env = append(os.Environ(), "AGENTPROC_ANCHOR_HELPER=1")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)

	self, err := lookupLinux(os.Getpid())
	require.NoError(t, err)
	require.Contains(t, string(out), "anchor="+format(os.Getpid(), self.start)+" ok=true",
		"the anchor passes over the hook script to this process")
}
