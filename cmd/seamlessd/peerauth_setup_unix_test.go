//go:build darwin || linux

package main

import (
	"context"
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/shellcmd"
)

// Every block of the move and of its undo parses in every shell an owner
// has, with a home whose name the shell would read.
func TestPeerAuthSetup_EveryBlockParses(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		f := newPasFake(t, goos)
		p := pasTestPlan(t, f)
		p.OldDataDir = "/Users/o'brien $(id)/.seamless"
		p.Home = "/Users/o'brien $(id)"
		p.TLSCert, p.TLSKey = f.cfgPath, f.cfgPath+".key"
		require.NoError(t, os.WriteFile(p.TLSKey, []byte("key"), 0o600))
		x := &pasRun{ctx: context.Background(), h: f.h, plan: p, t: shellcmd.ToolsFor(goos), tmp: t.TempDir()}
		f.files[p.OldDataDir] = fileFacts{uid: 501, isDir: true}
		blocks := [][]string{pasBeginCommands(x)}
		for _, list := range [][]pasStepDef{pasSteps, pasUndoSteps} {
			for i := range list {
				if list[i].build == nil {
					continue
				}
				if list[i].id == "data" {
					// The undo moves the data back from the layout.
					f.files[pasData] = fileFacts{uid: 479, isDir: true}
					delete(f.files, p.OldDataDir)
				}
				cmds, err := list[i].build(x)
				require.NoError(t, err, "%s %s", goos, list[i].id)
				blocks = append(blocks, cmds)
			}
		}
		for i, b := range blocks {
			if len(b) == 0 {
				continue
			}
			text := rootstepChain(b) + "\n"
			for _, sh := range rootstepShells(t) {
				out, err := exec.Command(sh, "-n", "-c", text).CombinedOutput()
				require.NoError(t, err, "%s, %s, block %d:\n%s\n%s", sh, goos, i, out, text)
			}
		}
	}
}
