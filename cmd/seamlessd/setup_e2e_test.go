//go:build unix

package main

// The whole front door: docs/install run for real (setup_installer_test.go's
// world), with every `seamlessd setup` call -- the probe included -- served by
// the real setup in this test binary (TestSetupHelperSeamlessd), reading the
// world's scratch home and nothing outside it.

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
)

// TestSetupHelperSeamlessd is not a test: the installer tests' stand-in
// seamlessd runs this binary with SETUP_E2E_HELPER_RUN=1 for every setup call,
// and this is that setup. It sees only $HOME: every path outside it reads as
// absent, so a peer_auth daemon or a LaunchDaemon on the machine running the
// tests is not one of the world's.
func TestSetupHelperSeamlessd(t *testing.T) {
	if os.Getenv("SETUP_E2E_HELPER_RUN") != "1" {
		t.Skip("the stand-in seamlessd setup of the installer tests; not a test of its own")
	}
	args := flag.Args()
	if len(args) == 0 || args[0] != "setup" {
		fmt.Fprintf(os.Stderr, "the setup helper wants setup's arguments, got %q\n", args)
		os.Exit(9)
	}
	err := runSetupWith(args[1:], func() *setupRun {
		r := realSetupRun()
		r.facts = setupFactsOf(setupHomeOnly(realSetupHost()))
		return r
	})
	if err != nil {
		var said *ownerError
		if errors.As(err, &said) {
			fmt.Fprintln(os.Stderr, said.Error())
		} else {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(1)
	}
	os.Exit(0)
}

// setupHomeOnly narrows h to its home: a path outside it reads as absent.
func setupHomeOnly(h setupHost) setupHost {
	home := h.probe.home + string(filepath.Separator)
	inside := func(p string) bool { return strings.HasPrefix(p, home) }
	read, exists := h.probe.readFile, h.exists
	h.probe.readFile = func(p string) ([]byte, error) {
		if !inside(p) {
			return nil, fs.ErrNotExist
		}
		return read(p)
	}
	h.exists = func(p string) bool { return inside(p) && exists(p) }
	return h
}

func TestSetupInstaller_RealSetup(t *testing.T) {
	setupEachShell(t, func(t *testing.T, shell string) {
		installer := filepath.Join("..", "..", "docs", "install")
		scripted := []string{"SETUP_E2E_SETUP=real", "SEAMLESS_VERSION=" + setupE2EVersion, "SEAMLESS_CLIENT=claude", "SEAMLESS_NO_SERVICE=1"}

		t.Run("SEAMLESS_SETUP=server: the install, then what is by hand", func(t *testing.T) {
			w := newSetupInstallerWorld(t, shell, installer)
			out, err := w.run(t, append(scripted, "SEAMLESS_SETUP=server")...)
			require.NoError(t, err, out)
			require.Contains(t, out, "Networked: Server (SEAMLESS_SETUP)")
			require.Contains(t, out, "setup does not do these Server steps yet:")
			require.Contains(t, out, "guides/network-install/#step-1-widen-the-server")
			require.Contains(t, w.calls(t), w.hooksCall("claude"))
			require.FileExists(t, filepath.Join(w.installDir, "seamlessd"))
			require.Less(t, strings.Index(out, "Networked: Server"), strings.Index(out, "installed "), "setup answers before anything is installed")
			require.Greater(t, strings.Index(out, "by hand"), strings.Index(out, "skipped (SEAMLESS_NO_SERVICE)"), "the steps by hand close the install")
		})

		t.Run("SEAMLESS_SETUP=client with the existing client knobs", func(t *testing.T) {
			w := newSetupInstallerWorld(t, shell, installer)
			out, err := w.run(t, append(scripted, "SEAMLESS_SETUP=client", "SEAMLESS_SERVER_URL=https://box.example:8081",
				"SEAMLESS_MCP_API_KEY=k1")...)
			require.NoError(t, err, out)
			require.Contains(t, w.calls(t), w.hooksCall("claude", "--server-url", "https://box.example:8081", "--api-key", "k1"))
			require.Contains(t, out, "skipped (client of https://box.example:8081)")
			require.Contains(t, out, "this Client uses its Server's key")
		})

		for _, tt := range []struct {
			name, knob, want string
		}{
			{"SEAMLESS_SETUP=client alone", "SEAMLESS_SETUP=client", "needs the Server to connect to"},
			{"SEAMLESS_SETUP=bogus", "SEAMLESS_SETUP=bogus", `invalid SEAMLESS_SETUP="bogus"`},
			{"SEAMLESS_IMPORT", "SEAMLESS_IMPORT=/tmp/b.tar.gz", "SEAMLESS_IMPORT: setup cannot start from a backup"},
			{"SEAMLESS_PAIR_CODE", "SEAMLESS_PAIR_CODE=1234", "SEAMLESS_PAIR_CODE: pairing with a one-time code is not built"},
		} {
			t.Run(tt.name+" stops the install before anything is installed", func(t *testing.T) {
				w := newSetupInstallerWorld(t, shell, installer)
				out, err := w.run(t, append(scripted, tt.knob)...)
				require.Error(t, err, out)
				require.Contains(t, out, tt.want)
				require.Contains(t, out, "seamlessd setup stopped the install; nothing was installed")
				require.NoFileExists(t, filepath.Join(w.installDir, "seamlessd"))
				for _, c := range w.calls(t) {
					require.NotContains(t, c, "install-hooks")
				}
			})
		}

		t.Run("a re-run with SEAMLESS_SETUP=server makes the Standalone the Server", func(t *testing.T) {
			w := newSetupInstallerWorld(t, shell, installer)
			w.config(t, "mcp:\n  api_key: \"k\"\n")
			out, err := w.run(t, append(scripted, "SEAMLESS_SETUP=server")...)
			require.NoError(t, err, out)
			require.Contains(t, out, "Standalone, becoming the Server")
			require.Contains(t, out, "guides/network-install/#step-1-widen-the-server")
			require.NotContains(t, out, "seamlessd restore", "a re-run keeps its data: no import")
		})

		t.Run("a Client's config refuses SEAMLESS_SETUP=standalone", func(t *testing.T) {
			w := newSetupInstallerWorld(t, shell, installer)
			w.config(t, "role: client\nserver_url: \"https://box.example:8081\"\nmcp:\n  api_key: \"k\"\n")
			out, err := w.run(t, append(scripted, "SEAMLESS_SETUP=standalone")...)
			require.Error(t, err, out)
			require.Contains(t, out, "setup does not change a Client into Standalone")
			require.NoFileExists(t, filepath.Join(w.installDir, "seamlessd"))
		})
	})
}

// TestSetupState_IsWhatTheInstallersRead: the answers the installer tests'
// stand-in writes are states setup writes itself.
func TestSetupState_IsWhatTheInstallersRead(t *testing.T) {
	got, err := marshalSetupState(setupState{kind: setupStandalone, role: config.RoleServer, installed: setupNone, answer: setupFromEnv})
	require.NoError(t, err)
	require.Equal(t, setupStateServer, string(got))
	got, err = marshalSetupState(setupState{kind: setupClient, role: config.RoleClient, serverURL: "https://box.example:8081",
		installed: setupNone, answer: setupAsked})
	require.NoError(t, err)
	require.Equal(t, setupStateClient, string(got))
}
