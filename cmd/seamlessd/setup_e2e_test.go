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
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
)

// TestSetupHelperSeamlessd is not a test: the installer tests' stand-in
// seamlessd runs this binary with SETUP_E2E_HELPER_RUN=1 for every setup call,
// and this is that setup. It sees only $HOME: every path outside it reads as
// absent, so a peer_auth daemon or a LaunchDaemon on the machine running the
// tests is not one of the world's; and it dials no daemon, since one listening
// on the machine running the tests is not the world's either. Its import loads
// the config as seamlessd import does, which in the world resolves under $HOME.
// Its address step asks no tailnet and reads no pin, and resolves only the
// names SETUP_E2E_HOSTS lists, on the interfaces SETUP_E2E_IFACES lists
// (setupE2ENet).
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
		r.daemonUp = func(config.Config) bool { return false }
		r.tailscale = setupAddrTailnet{err: errNoTailscale}
		r.daemonAccount = func() string { return "_seamless" }
		r.network = setupE2ENet(os.Getenv("SETUP_E2E_HOSTS"), os.Getenv("SETUP_E2E_IFACES"))
		r.loadConfig = func() (config.Config, error) {
			cfg, err := config.Load()
			if err == nil && (r.home == "" || !strings.HasPrefix(cfg.DataDir, r.home+string(filepath.Separator))) {
				return cfg, fmt.Errorf("the setup helper's data dir %s is outside the world's home %s", cfg.DataDir, r.home)
			}
			return cfg, err
		}
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

// setupE2ENet is the world's network: hosts is "name=addr,addr;name=addr" and
// ifaces "addr,addr;addr", one interface each; every other name fails.
func setupE2ENet(hosts, ifaces string) hostNet {
	table := map[string][]string{}
	for entry := range strings.SplitSeq(hosts, ";") {
		if name, list, ok := strings.Cut(entry, "="); ok {
			table[name] = strings.Split(list, ",")
		}
	}
	var all [][]netip.Addr
	for entry := range strings.SplitSeq(ifaces, ";") {
		var list []netip.Addr
		for a := range strings.SplitSeq(entry, ",") {
			if ip, err := netip.ParseAddr(a); err == nil {
				list = append(list, ip)
			}
		}
		all = append(all, list)
	}
	return hostNet{lookup: fakeLookup(table), ifaces: func() ([][]netip.Addr, error) { return all, nil }}
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

		t.Run("SEAMLESS_SETUP=server alone: the address it needs stops the install", func(t *testing.T) {
			w := newSetupInstallerWorld(t, shell, installer)
			out, err := w.run(t, append(scripted, "SEAMLESS_SETUP=server")...)
			require.Error(t, err, out)
			require.Contains(t, out, "Networked: Server (SEAMLESS_SETUP)")
			require.Contains(t, out, "SEAMLESS_SETUP=server needs the address the other computers reach this one at: set SEAMLESS_SERVER_NAME")
			require.Contains(t, out, "seamlessd setup stopped the install; nothing was installed")
			require.NoFileExists(t, filepath.Join(w.installDir, "seamlessd"))
			require.NoFileExists(t, filepath.Join(w.home, ".config", "seamless", "seamless.yaml"))
		})

		t.Run("SEAMLESS_SETUP=server with its name and certificate: the config, the install, then the closing", func(t *testing.T) {
			w := newSetupInstallerWorld(t, shell, installer)
			cert, key := filepath.Join(w.home, "certs", "server.crt"), filepath.Join(w.home, "certs", "server.key")
			setupAddrWritePair(t, cert, key, time.Now().Add(90*24*time.Hour), setupAddrDomain)
			out, err := w.run(t, append(scripted, "SEAMLESS_SETUP=server", "SEAMLESS_SERVER_NAME="+setupAddrDomain,
				"SEAMLESS_TLS_CERT="+cert, "SEAMLESS_TLS_KEY="+key,
				"SETUP_E2E_HOSTS="+setupAddrDomain+"="+setupAddrLANv4, "SETUP_E2E_IFACES=127.0.0.1,::1;"+setupAddrLANv4)...)
			require.NoError(t, err, out)
			cfg, err := config.LoadFrom(filepath.Join(w.home, ".config", "seamless", "seamless.yaml"))
			require.NoError(t, err, out)
			require.Equal(t, setupAddrLANv4+":8081", cfg.Addr)
			require.Equal(t, "https://"+setupAddrDomain+":8081", cfg.ServerURL())
			require.Equal(t, cert, cfg.TLS.CertFile)
			require.Len(t, cfg.MCP.APIKey, 64, "the first config carries the key install-hooks would have generated")
			require.Contains(t, out, "config       wrote addr, server_url, allowed_hosts and tls")
			require.Contains(t, w.calls(t), w.hooksCall("claude"))
			require.Less(t, strings.Index(out, "config       wrote"), strings.Index(out, "installed "), "the config is there before anything is installed")
			fp, err := certFingerprintFile(cert)
			require.NoError(t, err)
			require.Contains(t, out, "Certificate fingerprint: "+fp)
			require.Greater(t, strings.Index(out, "To connect another computer"), strings.Index(out, "skipped (SEAMLESS_NO_SERVICE)"),
				"the closing ends the install")
			require.Contains(t, out, "setup does not do this Server step yet:")
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
			{"SEAMLESS_IMPORT of no file", "SEAMLESS_IMPORT=/nonexistent-seamless-e2e/b.tar.gz",
				"SEAMLESS_IMPORT=/nonexistent-seamless-e2e/b.tar.gz: there is no file at"},
			{"SEAMLESS_IMPORT_LOCKS alone", "SEAMLESS_IMPORT_LOCKS=keep", "SEAMLESS_IMPORT names no backup"},
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

		t.Run("a re-run with SEAMLESS_SETUP=server makes the Standalone the Server, its config left as it is", func(t *testing.T) {
			w := newSetupInstallerWorld(t, shell, installer)
			w.config(t, "mcp:\n  api_key: \"k\"\n")
			cert, key := filepath.Join(w.home, "certs", "server.crt"), filepath.Join(w.home, "certs", "server.key")
			setupAddrWritePair(t, cert, key, time.Now().Add(90*24*time.Hour), setupAddrDomain)
			out, err := w.run(t, append(scripted, "SEAMLESS_SETUP=server", "SEAMLESS_SERVER_NAME="+setupAddrDomain,
				"SEAMLESS_TLS_CERT="+cert, "SEAMLESS_TLS_KEY="+key,
				"SETUP_E2E_HOSTS="+setupAddrDomain+"="+setupAddrLANv4, "SETUP_E2E_IFACES=127.0.0.1,::1;"+setupAddrLANv4)...)
			require.NoError(t, err, out)
			require.Contains(t, out, "Standalone, becoming the Server")
			require.Contains(t, out, "setup does not edit")
			require.Contains(t, out, `addr: "`+setupAddrLANv4+`:8081"`)
			require.NotContains(t, out, "seamlessd restore", "a re-run keeps its data: no import")
			raw, err := os.ReadFile(filepath.Join(w.home, ".config", "seamless", "seamless.yaml"))
			require.NoError(t, err)
			require.Equal(t, "mcp:\n  api_key: \"k\"\n", string(raw), "the owner's config is never edited")
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

// TestSetupInstaller_Import runs docs/install for real with SEAMLESS_IMPORT:
// the backup lands in the world's empty data dir before anything is
// installed, and a run with no one to ask decides its locked projects as
// seamlessd import does without a terminal.
func TestSetupInstaller_Import(t *testing.T) {
	plain, locked := impBackup(t, impSpec{}), impBackup(t, impSpec{locked: true})
	setupEachShell(t, func(t *testing.T, shell string) {
		installer := filepath.Join("..", "..", "docs", "install")
		scripted := []string{"SETUP_E2E_SETUP=real", "SEAMLESS_VERSION=" + setupE2EVersion, "SEAMLESS_CLIENT=claude", "SEAMLESS_NO_SERVICE=1"}

		t.Run("a plain backup is restored before anything is installed", func(t *testing.T) {
			w := newSetupInstallerWorld(t, shell, installer)
			out, err := w.run(t, append(scripted, "SEAMLESS_IMPORT="+plain)...)
			require.NoError(t, err, out)
			impRestored(t, filepath.Join(w.home, ".seamless"))
			require.Contains(t, out, "setup        Standalone (SEAMLESS_IMPORT)")
			require.Contains(t, out, "imported     1 memory, 1 note")
			require.Less(t, strings.Index(out, "imported "), strings.Index(out, "installed "), "the backup is in place before anything is installed")
			require.Contains(t, w.calls(t), w.hooksCall("claude"))
			require.Contains(t, out, "setup does not do this Standalone step yet:", "locked projects close the install, by hand")
			require.NotContains(t, out, "seamlessd restore")
		})

		t.Run("a relative SEAMLESS_IMPORT is the directory the installer started in", func(t *testing.T) {
			w := newSetupInstallerWorld(t, shell, installer)
			impCopy(t, plain, filepath.Join(w.tmp, "backup.tar.gz"))
			out, err := w.run(t, append(scripted, "SEAMLESS_IMPORT=backup.tar.gz")...)
			require.NoError(t, err, out)
			impRestored(t, filepath.Join(w.home, ".seamless"))
		})

		t.Run("a locked backup and no answer: nothing is installed or imported, as seamlessd import stops", func(t *testing.T) {
			w := newSetupInstallerWorld(t, shell, installer)
			out, err := w.run(t, append(scripted, "SEAMLESS_IMPORT="+locked)...)
			require.Error(t, err, out)
			require.Contains(t, out, "this backup holds 2 locked projects (example-legal, example-sealed) and no passkey can open them here")
			require.Contains(t, out, "Nothing was changed. Set SEAMLESS_IMPORT_LOCKS=keep to import them locked, or SEAMLESS_IMPORT_LOCKS=remove")
			require.Contains(t, out, "seamlessd setup stopped the install; nothing was installed")
			require.NoFileExists(t, filepath.Join(w.installDir, "seamlessd"))
			require.NoDirExists(t, filepath.Join(w.home, ".seamless"))
		})

		for _, answer := range []string{"keep", "remove"} {
			t.Run("a locked backup with SEAMLESS_IMPORT_LOCKS="+answer, func(t *testing.T) {
				w := newSetupInstallerWorld(t, shell, installer)
				out, err := w.run(t, append(scripted, "SEAMLESS_IMPORT="+locked, "SEAMLESS_IMPORT_LOCKS="+answer)...)
				require.NoError(t, err, out)
				data := filepath.Join(w.home, ".seamless")
				impRestored(t, data)
				st := impStateOf(t, data)
				require.Equal(t, []string{"laptop", "yubikey"}, st.passkeys, "a restore brings the backup's passkeys")
				if answer == "keep" {
					require.Equal(t, impLocked, st.locks)
					require.Contains(t, out, "nothing enforces a lock until locked projects (peer_auth) are on")
				} else {
					require.Empty(t, st.locks)
					require.Contains(t, out, "locks removed: example-legal, example-sealed")
				}
				require.Contains(t, out, "peer-auth/#move-an-existing-install-over", "locked projects, the next step, close the install")
			})
		}

		t.Run("a re-run refuses SEAMLESS_IMPORT before anything is installed", func(t *testing.T) {
			w := newSetupInstallerWorld(t, shell, installer)
			w.config(t, "mcp:\n  api_key: \"k\"\n")
			out, err := w.run(t, append(scripted, "SEAMLESS_IMPORT="+plain)...)
			require.Error(t, err, out)
			require.Contains(t, out, "setup starts from a backup only on a first install")
			require.NoFileExists(t, filepath.Join(w.installDir, "seamlessd"))
			require.NoDirExists(t, filepath.Join(w.home, ".seamless"))
		})

		t.Run("a Client refuses SEAMLESS_IMPORT before anything is installed", func(t *testing.T) {
			w := newSetupInstallerWorld(t, shell, installer)
			out, err := w.run(t, append(scripted, "SEAMLESS_SERVER_URL=https://box.example:8081", "SEAMLESS_MCP_API_KEY=k1",
				"SEAMLESS_IMPORT="+plain)...)
			require.Error(t, err, out)
			require.Contains(t, out, "a Client keeps no data of its own")
			require.NoFileExists(t, filepath.Join(w.installDir, "seamlessd"))
			require.NoDirExists(t, filepath.Join(w.home, ".seamless"))
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
