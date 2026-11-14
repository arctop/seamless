//go:build darwin || linux

package main

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A FIFO given as the key is refused before setup opens it: setup never
// waits on a writer, and never checks bytes root's tee would read again
// differently.
func TestAddrStepCheckPairRefusesAFIFO(t *testing.T) {
	cert, _ := setupAddrPair(t, setupAddrDomain)
	fifo := filepath.Join(t.TempDir(), "server.key")
	require.NoError(t, syscall.Mkfifo(fifo, 0o600))
	done := make(chan string, 1)
	go func() {
		_, problem := setupCheckPair(setupAddrDomain, cert, fifo, time.Now())
		done <- problem
	}()
	select {
	case problem := <-done:
		require.Contains(t, problem, "is not a regular file")
	case <-time.After(10 * time.Second):
		t.Fatal("setup waited on a FIFO")
	}
}

// The exposure check has teeth: the same step without root's empty 0600
// copy first -- tee creating the staged key itself, with whatever umask sudo
// runs it under -- leaves the key readable by every account until its chmod.
func TestAddrStepReview_ANaiveStagedKeyIsCaught(t *testing.T) {
	scratch := t.TempDir()
	cert, key, _, _ := addrstepScratchPair(t, filepath.Join(scratch, "owner"), "server.crt", "server.key")
	lines := addrstepRootLines(t, addrstepOutput(t, runtime.GOOS, cert, key))
	var naive []string
	dropped := 0
	for _, l := range lines {
		if strings.Contains(l, "install -m 0600 /dev/null ") && strings.Contains(l, ".key.new") {
			dropped++
			continue
		}
		naive = append(naive, l)
	}
	require.Equal(t, 1, dropped, "the step makes the staged key root's alone first:\n%s", strings.Join(lines, "\n"))
	exposed := addrstepKeyExposures(t, naive, scratch)
	require.NotEmpty(t, exposed)
	require.Contains(t, exposed[0], "every account reads it")
}

// The commands that make a LAN name's pair run as the owner: every path is
// quoted, a ~/ in front still the home.
func TestAddrStepLANCertCommandsQuotePaths(t *testing.T) {
	got := setupLANCertCommands("here.local", "/srv/my certs/here.local.crt", "/srv/my certs/here.local.key")
	require.Equal(t, "mkdir -p '/srv/my certs' && mkcert -install && mkcert -cert-file '/srv/my certs/here.local.crt' "+
		"-key-file '/srv/my certs/here.local.key' here.local", got[0])
	require.Contains(t, got[1], "-keyout '/srv/my certs/here.local.key' -out '/srv/my certs/here.local.crt'")
	require.True(t, strings.HasSuffix(got[1], "&& chmod 600 '/srv/my certs/here.local.key'"), got[1])
	for _, sh := range rootstepShells(t) {
		for _, c := range got {
			out, err := exec.Command(sh, "-n", "-c", c).CombinedOutput()
			require.NoError(t, err, "%s: %s\n%s", sh, out, c)
		}
	}
}
