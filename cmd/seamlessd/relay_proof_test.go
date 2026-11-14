//go:build darwin || linux

package main

// Black-box proofs of the relay against the built seamlessd: the standard
// library and the binary alone, so the same file runs against a build without
// the relay and shows what it lacks.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var (
	relayProofMu  sync.Mutex
	relayProofBin string
)

// relayProofSeamlessd builds this package's seamlessd once per test run.
func relayProofSeamlessd(t *testing.T) string {
	t.Helper()
	relayProofMu.Lock()
	defer relayProofMu.Unlock()
	if relayProofBin != "" {
		if _, err := os.Stat(relayProofBin); err == nil {
			return relayProofBin
		}
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	out := filepath.Join(t.TempDir(), "seamlessd")
	start := time.Now()
	build := exec.Command("go", "build", "-o", out, "./cmd/seamlessd")
	build.Dir = root
	build.Env = relayProofEnv(os.Environ())
	msg, err := build.CombinedOutput()
	require.NoError(t, err, "go build ./cmd/seamlessd: %s", msg)
	t.Logf("built seamlessd in %s", time.Since(start).Round(time.Millisecond))
	relayProofBin = out
	return out
}

// relayProofEnv drops every SEAMLESS_* variable from env.
func relayProofEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); !strings.HasPrefix(k, "SEAMLESS_") {
			out = append(out, kv)
		}
	}
	return out
}

// The relay exists on macOS and Linux: `seamlessd relay` names its two
// commands, and refuses an unknown one or a pairing without its server and
// code, before anything reads a file or dials a server.
func TestRelayProof_TheRelayCommandExists(t *testing.T) {
	bin := relayProofSeamlessd(t)
	home := t.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command(bin, args...)
		cmd.Env = append(relayProofEnv(os.Environ()), "HOME="+home)
		out, err := cmd.CombinedOutput()
		require.Error(t, err, "%v must refuse: %s", args, out)
		return string(out)
	}
	out := run("relay")
	require.Contains(t, out, "seamlessd relay serve [--data-dir DIR] [--socket PATH]", "the relay names its commands")
	require.Contains(t, out, "seamlessd relay pair --server URL --code CODE")
	require.Contains(t, run("relay", "bogus"), `unknown command \"bogus\": valid commands are serve, pair`)
	require.Contains(t, run("relay", "pair"), "--server and --code are required")
	require.Contains(t, run("relay", "serve", "extra"), `unexpected argument \"extra\"`)
}
