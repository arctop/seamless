//go:build darwin || linux

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// rootstepShells is the shells an owner pastes into, as this computer has
// them: sh always, and dash (Debian's sh), bash and zsh where present.
func rootstepShells(t *testing.T) []string {
	t.Helper()
	shells := []string{"/bin/sh"}
	for _, s := range []string{"/bin/dash", "/usr/bin/dash", "/bin/bash", "/bin/zsh", "/usr/bin/zsh"} {
		if _, err := os.Stat(s); err == nil {
			shells = append(shells, s)
		}
	}
	return shells
}

// Every block, for macOS and Linux, with a certificate and without, parses in
// every shell an owner pastes into.
func TestRootStepExec_EveryBlockParses(t *testing.T) {
	cert := rootstepCert(t)
	for _, goos := range []string{"darwin", "linux"} {
		for _, withCA := range []bool{false, true} {
			p := rootstepPlan(goos)
			if withCA {
				p.caPEM = string(cert.pem)
			}
			for i, b := range clientRelayRootStep(p) {
				text := rootstepChain(b[1:]) + "\n"
				for _, sh := range rootstepShells(t) {
					out, err := exec.Command(sh, "-n", "-c", text).CombinedOutput()
					require.NoError(t, err, "%s, %s, block %d:\n%s\n%s", sh, goos, i+1, out, text)
				}
			}
		}
	}
}

// Hostile values -- had any passed its check -- reach each command as the one
// word they are, and nothing they spell runs: every block runs under /bin/sh
// with stand-ins that record their words.
func TestRootStepExec_HostileValuesAreInert(t *testing.T) {
	scratch := t.TempDir()
	// Each spelling would make the file ran in the directory the blocks run in.
	marker := filepath.Join(scratch, "ran")
	inject := "$(touch ran);touch ran`touch ran`'\"*!&|<x>"
	src := filepath.Join(scratch, "my bin", inject)
	require.NoError(t, os.MkdirAll(filepath.Dir(src), 0o755))
	require.NoError(t, os.WriteFile(src, []byte("x"), 0o644))
	for _, goos := range []string{"darwin", "linux"} {
		p := rootstepPlan(goos)
		p.owner, p.server, p.fingerprint, p.source = "o"+inject, "https://a"+inject, "AB"+inject, src
		p.sha256 = strings.Repeat("0", 64) + inject
		blocks := clientRelayRootStep(p)
		for i, b := range blocks[:3] {
			text := rootstepChain(b[1:])
			// Run each command on its own, so a failing check does not hide
			// what the next one would do with its value.
			for _, c := range b[1:] {
				log, _ := rootstepRunBlock(t, c+"\n", scratch)
				_ = os.Remove(filepath.Join(scratch, "sudo.log"))
				for _, words := range strings.Split(strings.TrimSuffix(log, "\n"), "\n") {
					for _, w := range strings.Split(strings.TrimSuffix(words, "\037"), "\037") {
						if strings.Contains(w, inject) {
							require.Truef(t, w == p.owner || w == p.server || w == p.fingerprint || w == "https://a"+inject,
								"%s block %d: a word carries a value only whole: %q", goos, i+1, w)
						}
					}
				}
			}
			require.NoFileExists(t, marker, "%s block %d ran what a value spells:\n%s", goos, i+1, text)
		}
	}
}
