//go:build darwin || linux

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
)

// paLayout is a deployment laid out the way the runbook lays it out, under a
// short temp dir: data 0700, run 0755, etc/seamless.yaml 0600, bin/seamlessd
// 0755, all owned by the test's uid -- which plays the daemon's account.
type paLayout struct {
	base, data, run, sock, cfgPath, exe string
}

func newPALayout(t *testing.T, extra string) (paLayout, config.Config) {
	t.Helper()
	// Short: macOS caps a socket path at 104 bytes and t.TempDir() nests deep.
	base, err := os.MkdirTemp("", "pa")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	base, err = filepath.EvalSymlinks(base)
	require.NoError(t, err)
	l := paLayout{base: base, data: filepath.Join(base, "data"), run: filepath.Join(base, "run")}
	l.sock = filepath.Join(l.run, "s.sock")
	l.cfgPath = filepath.Join(base, "etc", "seamless.yaml")
	l.exe = filepath.Join(base, "bin", "seamlessd")
	require.NoError(t, os.Mkdir(l.data, 0o700))
	require.NoError(t, os.Mkdir(l.run, 0o755))
	require.NoError(t, os.Chmod(l.run, 0o755)) // past the umask
	require.NoError(t, os.MkdirAll(filepath.Dir(l.cfgPath), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Dir(l.exe), 0o755))
	require.NoError(t, os.WriteFile(l.exe, []byte("#!/bin/sh\n"), 0o755))
	require.NoError(t, os.Chmod(l.exe, 0o755))
	yaml := fmt.Sprintf("data_dir: %s\naddr: 127.0.0.1:0\nmcp:\n  api_key: k\npeer_auth:\n  enabled: true\n  owner: _example_owner\n  socket:\n    path: %s\n%s",
		l.data, l.sock, extra)
	require.NoError(t, os.WriteFile(l.cfgPath, []byte(yaml), 0o600))
	require.NoError(t, os.Chmod(l.cfgPath, 0o600))
	clearServeEnv(t)
	cfg, err := config.LoadFrom(l.cfgPath)
	require.NoError(t, err)
	return l, cfg
}

// clearServeEnv unsets the SEAMLESS_* variables a developer's shell may carry.
func clearServeEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "SEAMLESS_") {
			t.Setenv(k, "")
			require.NoError(t, os.Unsetenv(k))
		}
	}
}

// paTestEnv plays a daemon running as the test's own uid, with _example_owner a
// different account (the owner) and "me" resolving to the test's uid. The
// machine has no pin of the daemon's account (this one's real pin, if any, is
// not the test's), and the test's uid is an ordinary account, so the env takes
// it for a system account, as the daemon's is
// (TestPreparePeerAuth_RefusesAnAccountThatIsNotASystemAccount holds the real
// rule, TestPreparePeerAuth_DaemonAccountPin the pin).
func paTestEnv(l paLayout, ownerUID uint32) peerAuthEnv {
	me := uint32(os.Geteuid())
	return peerAuthEnv{
		euid:          os.Geteuid(),
		systemAccount: func(uint32) (bool, string) { return true, "the test's stand-in" },
		daemonAccount: paNoPin,
		resolve: func(name string) (access.Account, error) {
			switch name {
			case "_example_owner":
				return access.Account{UID: ownerUID, Name: "_example_owner"}, nil
			case "me":
				return access.Account{UID: me, Name: "me"}, nil
			case "_example_agent":
				return access.Account{UID: 470, Name: "_example_agent"}, nil
			}
			return access.Account{}, fmt.Errorf("unknown account %q", name)
		},
		exe:     func() (string, error) { return l.exe, nil },
		inspect: inspectPath,
	}
}

// paNoPin is a machine with no pin of the daemon's account.
func paNoPin() (access.DaemonPin, bool, error) { return access.DaemonPin{}, false, nil }

// paPinned is a machine whose trusted pin names uid.
func paPinned(uid uint32) func() (access.DaemonPin, bool, error) {
	return func() (access.DaemonPin, bool, error) {
		return access.DaemonPin{Name: "_seamless", UID: uid, Path: access.DaemonAccountPath}, true, nil
	}
}

// paBadPin is a machine whose pin is there but is not believed.
func paBadPin(problem string) func() (access.DaemonPin, bool, error) {
	return func() (access.DaemonPin, bool, error) {
		return access.DaemonPin{}, true, fmt.Errorf("%w %s %s", access.ErrDaemonPin, access.DaemonAccountPath, problem)
	}
}

// paLockedAndRestricted is the rest of a fixture's peer_auth block: one locked
// project, one restricted account.
const paLockedAndRestricted = "  locked: [example-finance]\n  restricted: [_example_agent]\n"

func TestPreparePeerAuth_AcceptsTheRunbookLayout(t *testing.T) {
	l, cfg := newPALayout(t, paLockedAndRestricted)
	rt, err := preparePeerAuth(cfg, paTestEnv(l, uint32(os.Geteuid())+1000))
	require.NoError(t, err)
	require.Equal(t, []string{"example-finance"}, rt.policy.ConsoleHidden())
}

func TestPreparePeerAuth_Refusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(t *testing.T, l paLayout, env *peerAuthEnv)
		want   string
	}{
		{"the daemon runs as the owner", func(t *testing.T, l paLayout, env *peerAuthEnv) {
			env.resolve = paTestEnv(l, uint32(os.Geteuid())).resolve
		}, "runs as _example_owner, the owner"},
		{"the daemon runs as root", func(t *testing.T, l paLayout, env *peerAuthEnv) {
			env.euid = 0
		}, "runs as root"},
		{"the data dir is group-readable", func(t *testing.T, l paLayout, env *peerAuthEnv) {
			require.NoError(t, os.Chmod(l.data, 0o750))
		}, "must be 0700"},
		{"the data dir is a symlink", func(t *testing.T, l paLayout, env *peerAuthEnv) {
			real := filepath.Join(l.base, "elsewhere")
			require.NoError(t, os.Mkdir(real, 0o700))
			require.NoError(t, os.Remove(l.data))
			require.NoError(t, os.Symlink(real, l.data))
		}, "is a symlink"},
		{"the socket dir is world-writable", func(t *testing.T, l paLayout, env *peerAuthEnv) {
			require.NoError(t, os.Chmod(l.run, 0o777))
		}, "must not be group- or world-writable"},
		{"the socket dir is missing", func(t *testing.T, l paLayout, env *peerAuthEnv) {
			require.NoError(t, os.Remove(l.run))
		}, "socket directory"},
		{"the config file is world-writable", func(t *testing.T, l paLayout, env *peerAuthEnv) {
			require.NoError(t, os.Chmod(l.cfgPath, 0o666))
		}, "config file"},
		{"the binary is group-writable", func(t *testing.T, l paLayout, env *peerAuthEnv) {
			require.NoError(t, os.Chmod(l.exe, 0o775))
		}, "seamlessd binary"},
		{"a directory above the data belongs to another account", func(t *testing.T, l paLayout, env *peerAuthEnv) {
			env.inspect = func(p string) (fileFacts, error) {
				f, err := inspectPath(p)
				if p == l.base {
					f.uid = 4242 // the owner's home, say
				}
				return f, err
			}
		}, "owned by uid 4242, who could rename it away"},
		{"a directory above the socket is world-writable", func(t *testing.T, l paLayout, env *peerAuthEnv) {
			env.inspect = func(p string) (fileFacts, error) {
				f, err := inspectPath(p)
				if p == l.base {
					f.mode = os.ModeDir | 0o777
				}
				return f, err
			}
		}, "writable by other accounts"},
		{"an account does not resolve", func(t *testing.T, l paLayout, env *peerAuthEnv) {
			base := env.resolve
			env.resolve = func(n string) (access.Account, error) {
				if n == "_example_agent" {
					return access.Account{}, fmt.Errorf("no such user")
				}
				return base(n)
			}
		}, "peer_auth.restricted _example_agent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, cfg := newPALayout(t, paLockedAndRestricted)
			env := paTestEnv(l, uint32(os.Geteuid())+1000)
			tc.mutate(t, l, &env)
			_, err := preparePeerAuth(cfg, env)
			require.ErrorIs(t, err, errPeerAuthPreflight)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

// serve refuses to run as an account that is not a system account: seam
// approve and the Touch ID approver would refuse the daemon, so the install
// fails at start instead, naming the guide's step that creates the account.
// An env that sets no rule is held to the real one.
func TestPreparePeerAuth_RefusesAnAccountThatIsNotASystemAccount(t *testing.T) {
	l, cfg := newPALayout(t, paLockedAndRestricted)
	env := paTestEnv(l, uint32(os.Geteuid())+1000)
	var judged []uint32
	env.systemAccount = func(uid uint32) (bool, string) {
		judged = append(judged, uid)
		return false, "macOS system accounts are below uid 500"
	}
	_, err := preparePeerAuth(cfg, env)
	require.ErrorIs(t, err, errPeerAuthPreflight)
	require.Equal(t, []uint32{uint32(os.Geteuid())}, judged, "the daemon's own euid is judged")
	require.Equal(t, fmt.Sprintf("peer_auth preflight: the daemon runs as uid %d, which is not a system account "+
		"(macOS system accounts are below uid 500), and with no pin at /opt/seamless/etc/daemon-account, seam approve and the "+
		"Touch ID approver refuse a daemon that is not one: run it as a system account, created and pinned as the peer_auth guide "+
		`does under "Deploy it: the daemon runs as its own account" (dscl with a uid below 500 on macOS, useradd --system on Linux)`, os.Geteuid()), err.Error(),
		"the only problem in an otherwise passing layout")

	// The owner and root keep their own refusal, alone.
	env.resolve = paTestEnv(l, uint32(os.Geteuid())).resolve
	_, err = preparePeerAuth(cfg, env)
	require.ErrorContains(t, err, "runs as _example_owner, the owner")
	require.NotContains(t, err.Error(), "not a system account")
	env = paTestEnv(l, uint32(os.Geteuid())+1000)
	env.euid = 0
	env.systemAccount = func(uint32) (bool, string) { return false, "x" }
	_, err = preparePeerAuth(cfg, env)
	require.ErrorContains(t, err, "runs as root")
	require.NotContains(t, err.Error(), "not a system account")

	// No rule set: the real one, which this test's own (ordinary) account fails.
	if system, _ := access.SystemAccount(uint32(os.Geteuid())); system {
		t.Skipf("uid %d is a system account here", os.Geteuid())
	}
	env = paTestEnv(l, uint32(os.Geteuid())+1000)
	env.systemAccount = nil
	_, err = preparePeerAuth(cfg, env)
	require.ErrorIs(t, err, errPeerAuthPreflight)
	require.Contains(t, err.Error(), fmt.Sprintf("the daemon runs as uid %d, which is not a system account", os.Geteuid()))
}

// With a trusted pin, serve runs only as the pinned account (the system-account
// rule is not asked); a pin that is there but not believed refuses the start,
// whoever runs it; and a restricted account that is the pinned one is refused.
func TestPreparePeerAuth_DaemonAccountPin(t *testing.T) {
	me := uint32(os.Geteuid())
	notSystem := func(uint32) (bool, string) { return false, "not asked under a pin" }
	l, cfg := newPALayout(t, paLockedAndRestricted)

	env := paTestEnv(l, me+1000)
	env.systemAccount = notSystem
	env.daemonAccount = paPinned(me)
	_, err := preparePeerAuth(cfg, env)
	require.NoError(t, err, "the pinned account, whatever its range")

	env.daemonAccount = paPinned(480)
	_, err = preparePeerAuth(cfg, env)
	require.ErrorIs(t, err, errPeerAuthPreflight)
	require.Equal(t, fmt.Sprintf("peer_auth preflight: the daemon runs as uid %d, but /opt/seamless/etc/daemon-account pins the daemon's "+
		"account as _seamless (uid 480), and seam approve and the Touch ID approver accept no other: run the daemon as _seamless, "+
		`or have root rewrite the pin as the peer_auth guide does under "Deploy it: the daemon runs as its own account"`, me), err.Error())

	env.daemonAccount = paBadPin("is owned by uid 501: it must belong to root")
	_, err = preparePeerAuth(cfg, env)
	require.ErrorIs(t, err, errPeerAuthPreflight)
	require.Equal(t, "peer_auth preflight: the daemon account pin /opt/seamless/etc/daemon-account is owned by uid 501: it must belong "+
		"to root -- serve and its clients trust no daemon while it stands: have root rewrite it as the peer_auth guide does under "+
		`"Deploy it: the daemon runs as its own account"`, err.Error(), "refused, not the system-account fallback")
	env.euid = 0
	_, err = preparePeerAuth(cfg, env)
	require.ErrorContains(t, err, "runs as root")
	require.ErrorContains(t, err, "the daemon account pin /opt/seamless/etc/daemon-account is owned by uid 501", "whoever runs it")

	// The restricted account is the pinned one.
	l, cfg = newPALayout(t, "  restricted: [me]\n")
	env = paTestEnv(l, me+1000)
	env.daemonAccount = paPinned(me)
	_, err = preparePeerAuth(cfg, env)
	require.ErrorIs(t, err, errPeerAuthPreflight)
	require.Equal(t, fmt.Sprintf("peer_auth preflight: peer_auth.restricted lists me (uid %d), the account /opt/seamless/etc/daemon-account "+
		"pins as the daemon's: the clients would trust a socket it serves -- list another account, or have root correct the pin", me), err.Error())
	env.daemonAccount = paNoPin
	_, err = preparePeerAuth(cfg, env)
	require.NoError(t, err, "with no pin there is nothing to collide with")
}

// serve says at start which account the pin names, or warns that there is
// none, with the command that writes it.
func TestLogPolicy_DaemonAccountPin(t *testing.T) {
	l, cfg := newPALayout(t, paLockedAndRestricted)
	env := paTestEnv(l, uint32(os.Geteuid())+1000)
	logged := func(env peerAuthEnv) string {
		rt, err := preparePeerAuth(cfg, env)
		require.NoError(t, err)
		var b strings.Builder
		rt.logPolicy(slog.New(slog.NewTextHandler(&b, nil)))
		return b.String()
	}
	out := logged(env)
	require.Contains(t, out, "level=WARN")
	require.Contains(t, out, "no pin at /opt/seamless/etc/daemon-account names the daemon's account")
	require.Contains(t, out, `echo \"_seamless $(id -u _seamless)\" | sudo tee /opt/seamless/etc/daemon-account >/dev/null && sudo chmod 0644 /opt/seamless/etc/daemon-account`)

	env.daemonAccount = paPinned(uint32(os.Geteuid()))
	out = logged(env)
	require.NotContains(t, out, "level=WARN")
	require.Contains(t, out, fmt.Sprintf("peer_auth daemon account pinned\" pin=/opt/seamless/etc/daemon-account user=_seamless uid=%d", os.Geteuid()))
}

// Doctor's row for each state of the pin: absent (a warning with the command,
// naming the account doctor runs as), trusted and this account (ok), trusted
// and another (said, and a failure), not believed or not parsed (a failure).
func TestPeerAuthChecks_DaemonAccountRows(t *testing.T) {
	me := uint32(os.Geteuid())
	l, cfg := newPALayout(t, paLockedAndRestricted)
	rows := func(env peerAuthEnv) (pin []check, fails []string) {
		for _, r := range peerAuthChecks(context.Background(), cfg, nil, env) {
			if r.name == "peer_auth.daemon_account" {
				pin = append(pin, r)
			}
			if r.status == statusFail {
				fails = append(fails, r.detail)
			}
		}
		return pin, fails
	}

	env := paTestEnv(l, me+1000)
	base := env.resolve
	env.resolve = func(name string) (access.Account, error) {
		if name == fmt.Sprint(me) {
			return access.Account{UID: me, Name: "_seamless_test"}, nil
		}
		return base(name)
	}
	pin, fails := rows(env)
	require.Empty(t, fails)
	require.Equal(t, []check{{statusWarn, "peer_auth.daemon_account", "no pin at /opt/seamless/etc/daemon-account names the daemon's account, " +
		"so seam approve, seam touchid enroll and the Touch ID approver fall back to accepting any system account as the daemon's, " +
		"another service's or a restricted role account included: pin this one, as an administrator: " +
		`echo "_seamless_test $(id -u _seamless_test)" | sudo tee /opt/seamless/etc/daemon-account >/dev/null && sudo chmod 0644 /opt/seamless/etc/daemon-account`}}, pin)

	env.daemonAccount = paPinned(me)
	pin, fails = rows(env)
	require.Empty(t, fails)
	require.Equal(t, []check{{statusOK, "peer_auth.daemon_account", fmt.Sprintf("/opt/seamless/etc/daemon-account pins _seamless (uid %d), "+
		"the account this runs as: seam approve, seam touchid enroll and the Touch ID approver trust no other daemon", me)}}, pin)

	env.daemonAccount = paPinned(480)
	pin, fails = rows(env)
	require.Equal(t, []check{{statusInfo, "peer_auth.daemon_account", "/opt/seamless/etc/daemon-account pins _seamless (uid 480): seam approve, " +
		"seam touchid enroll and the Touch ID approver trust no other daemon, so it must run as that account (run doctor as it to check)"}}, pin)
	require.Len(t, fails, 1)
	require.Contains(t, fails[0], fmt.Sprintf("the daemon runs as uid %d, but /opt/seamless/etc/daemon-account pins the daemon's account as _seamless (uid 480)", me))

	for problem, want := range map[string]string{
		"is mode 0664: it must be writable by root alone (0644)":                                                        "untrusted",
		`has 3 space-separated fields, not 2: it must be one line, the daemon's account name and uid ("_seamless 480")`: "unparsable",
	} {
		env.daemonAccount = paBadPin(problem)
		pin, fails = rows(env)
		require.Empty(t, pin, "%s: no row of its own", want)
		require.Equal(t, []string{"the daemon account pin /opt/seamless/etc/daemon-account " + problem + " -- serve and its clients trust " +
			`no daemon while it stands: have root rewrite it as the peer_auth guide does under "Deploy it: the daemon runs as its own account"`}, fails, want)
	}

	// Run as the owner, the command names the guide's account, never the owner.
	env = paTestEnv(l, me)
	env.resolve = func(name string) (access.Account, error) {
		if name == "_example_owner" || name == fmt.Sprint(me) {
			return access.Account{UID: me, Name: "_example_owner"}, nil
		}
		return base(name)
	}
	pin, _ = rows(env)
	require.Len(t, pin, 1)
	require.Contains(t, pin[0].detail, `echo "_seamless $(id -u _seamless)"`)
	require.NotContains(t, pin[0].detail, "_example_owner")
}

// A missing data dir is created 0700 by serve once its parents pass; doctor
// only reports it.
func TestPreparePeerAuth_CreatesAMissingDataDir(t *testing.T) {
	l, cfg := newPALayout(t, paLockedAndRestricted)
	require.NoError(t, os.Remove(l.data))
	env := paTestEnv(l, uint32(os.Geteuid())+1000)
	policy, err := access.New(cfg.PeerAuth, env.resolve)
	require.NoError(t, err)
	rt := &peerAuthRuntime{policy: policy, cfg: cfg.PeerAuth, env: env}
	require.NotEmpty(t, rt.filesystemProblems(cfg, false), "doctor reports a missing data dir")
	_, err = os.Stat(l.data)
	require.ErrorIs(t, err, os.ErrNotExist, "and creates nothing")

	_, err = preparePeerAuth(cfg, env)
	require.NoError(t, err)
	info, err := os.Stat(l.data)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), info.Mode().Perm())
}

// The kernel path end to end: a real socket, peerConnContext reading the
// peer's uid at accept, peerHandler mapping it to a View -- and the shared
// client constructor dialing a socket server_url.
func TestPeerAuthSocket_KernelNamesThePeer(t *testing.T) {
	whoami := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v, ok := access.FromContext(r.Context())
		if !ok {
			http.Error(w, "no view", http.StatusInternalServerError)
			return
		}
		p := v.Principal()
		_ = json.NewEncoder(w).Encode(map[string]any{"kind": p.Kind.String(), "uid": p.UID, "socket": p.Socket, "user": p.User})
	})
	me := uint32(os.Geteuid())
	for _, tc := range []struct {
		name     string
		extra    string
		ownerUID uint32
		status   int
		kind     string
	}{
		{"this uid is the owner", "", me, http.StatusOK, "owner"},
		{"this uid is a restricted account", "  restricted: [me]\n", me + 1000, http.StatusOK, "restricted"},
		{"this uid is unknown", paLockedAndRestricted, me + 1000, http.StatusForbidden, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, cfg := newPALayout(t, tc.extra)
			env := paTestEnv(l, tc.ownerUID)
			policy, err := access.New(cfg.PeerAuth, env.resolve)
			require.NoError(t, err)
			rt := &peerAuthRuntime{policy: policy, cfg: cfg.PeerAuth, env: env}
			ln, err := rt.listenSocket()
			require.NoError(t, err)
			info, err := os.Stat(l.sock)
			require.NoError(t, err)
			require.Equal(t, os.FileMode(0o666), info.Mode().Perm(), "the configured (default) socket mode")

			srv := &http.Server{
				Handler:     peerHandler(policy, peerLeases{}, slog.New(slog.NewTextHandler(io.Discard, nil)), whoami),
				ConnContext: peerConnContext,
			}
			go func() { _ = srv.Serve(ln) }()
			t.Cleanup(func() { _ = srv.Close() })

			client := config.Config{Role: config.RoleClient, AdvertisedURL: "unix://" + l.sock}
			hc, err := client.HTTPClient(0)
			require.NoError(t, err)
			req, err := http.NewRequest(http.MethodGet, client.ServerURL()+"/whoami", nil)
			require.NoError(t, err)
			// A forged credential travels nowhere: the kernel answers instead.
			req.Header.Set("Authorization", "Bearer forged")
			req.Header.Set("X-Peer-Uid", "0")
			resp, err := hc.Do(req)
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()
			require.Equal(t, tc.status, resp.StatusCode)
			if tc.status != http.StatusOK {
				return
			}
			var got map[string]any
			require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
			require.Equal(t, tc.kind, got["kind"])
			require.EqualValues(t, me, got["uid"])
			require.Equal(t, true, got["socket"])
		})
	}
}

func TestListenSocket_StaleAndForeignFiles(t *testing.T) {
	l, cfg := newPALayout(t, paLockedAndRestricted)
	env := paTestEnv(l, uint32(os.Geteuid())+1000)
	policy, err := access.New(cfg.PeerAuth, env.resolve)
	require.NoError(t, err)
	rt := &peerAuthRuntime{policy: policy, cfg: cfg.PeerAuth, env: env}

	// A stale socket of the daemon's own (a crash left it) is replaced.
	stale, err := net.Listen("unix", l.sock)
	require.NoError(t, err)
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	require.NoError(t, stale.Close())
	ln, err := rt.listenSocket()
	require.NoError(t, err)
	require.NoError(t, ln.Close())
	_, err = os.Stat(l.sock)
	require.ErrorIs(t, err, os.ErrNotExist, "closing the listener unlinks the socket")

	// Anything else at the path is refused, never removed.
	require.NoError(t, os.WriteFile(l.sock, []byte("not a socket"), 0o600))
	_, err = rt.listenSocket()
	require.ErrorIs(t, err, errPeerAuthPreflight)
	_, err = os.Stat(l.sock)
	require.NoError(t, err, "the foreign file is left alone")

	// A configured mode lands on the socket.
	require.NoError(t, os.Remove(l.sock))
	rt.cfg.Socket.Mode = "0660"
	ln, err = rt.listenSocket()
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	info, err := os.Stat(l.sock)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o660), info.Mode().Perm())
}

func TestPeerAuthChecks_DoctorRows(t *testing.T) {
	l, cfg := newPALayout(t, paLockedAndRestricted)
	env := paTestEnv(l, uint32(os.Geteuid())+1000)
	rows := peerAuthChecks(context.Background(), cfg, nil, env)
	var text strings.Builder
	for _, r := range rows {
		require.NotEqual(t, statusFail, r.status, "%s: %s", r.name, r.detail)
		fmt.Fprintf(&text, "%s %s\n", r.name, r.detail)
	}
	require.Contains(t, text.String(), "_example_agent (uid 470): restricted")
	require.Contains(t, text.String(), "locked: [example-finance]")
	require.Contains(t, text.String(), "the files and accounts pass serve's preflight", "no database: the project checks did not run")
	require.NotContains(t, text.String(), "preflight passes")
	require.Contains(t, text.String(), "reads no agent files")

	require.NoError(t, os.Chmod(l.data, 0o755))
	failed := false
	for _, r := range peerAuthChecks(context.Background(), cfg, nil, env) {
		if r.status == statusFail && strings.Contains(r.detail, "must be 0700") {
			failed = true
		}
	}
	require.True(t, failed, "doctor fails on what serve would refuse")

	// Run as an account that is not a system account, doctor fails the row serve
	// would refuse on, and does not say the preflight passes.
	require.NoError(t, os.Chmod(l.data, 0o700))
	env.systemAccount = func(uint32) (bool, string) { return false, "macOS system accounts are below uid 500" }
	var fails []string
	text.Reset()
	for _, r := range peerAuthChecks(context.Background(), cfg, nil, env) {
		if r.status == statusFail {
			fails = append(fails, r.name+": "+r.detail)
		}
		fmt.Fprintf(&text, "%s %s\n", r.name, r.detail)
	}
	require.Len(t, fails, 1, text.String())
	require.Contains(t, fails[0], fmt.Sprintf("peer_auth: the daemon runs as uid %d, which is not a system account", os.Geteuid()))
	require.Contains(t, fails[0], `"Deploy it: the daemon runs as its own account"`)
	require.NotContains(t, text.String(), "pass serve's preflight")
}

// The daemon's real route table over a real socket, the kernel naming the
// peer: the test's own uid is listed as the restricted account, and an MCP
// client built the way the seam CLI builds one (the shared client constructor,
// a socket server_url, no key) reads and writes an open project and is refused
// a closed one.
func TestPeerAuthSocket_MCPOverTheRealSocket(t *testing.T) {
	d := newRADaemon(t)
	d.plantClosed(t)
	own := paClient(t, d.owner.URL, raKey)
	paCallOK(t, own, "memory_write", map[string]any{"project": raOpen, "name": "open-fact", "kind": "gotcha",
		"description": "open " + raOpenToken, "body": "open body " + raOpenToken})

	me := uint32(os.Geteuid())
	accounts := map[string]access.Account{
		"_example_owner": {UID: me + 1000, Name: "_example_owner"},
		raAccount:        {UID: me, Name: raAccount},
	}
	policy, err := access.New(raPolicy(), func(n string) (access.Account, error) {
		if a, ok := accounts[n]; ok {
			return a, nil
		}
		return access.Account{}, fmt.Errorf("no %s", n)
	})
	require.NoError(t, err)

	dir, err := os.MkdirTemp("", "pa")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	srv := &http.Server{Handler: peerHandler(policy, storeLeases(d.db), slog.New(slog.NewTextHandler(io.Discard, nil)), d.mux), ConnContext: peerConnContext}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	cfg := config.Config{Role: config.RoleClient, AdvertisedURL: "unix://" + sock}
	hc, err := cfg.HTTPClient(0)
	require.NoError(t, err)
	ctx := context.Background()
	cli, err := mcpclient.NewStreamableHttpClient(cfg.ServerURL()+"/api/mcp", transport.WithHTTPBasicClient(hc))
	require.NoError(t, err)
	t.Cleanup(func() { _ = cli.Close() })
	require.NoError(t, cli.Start(ctx))
	var init mcpgo.InitializeRequest
	init.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
	init.Params.ClientInfo = mcpgo.Implementation{Name: "socket-test", Version: "0"}
	_, err = cli.Initialize(ctx, init)
	require.NoError(t, err)

	text, isErr := paCall(t, cli, "memory_read", map[string]any{"project": raOpen, "name": "open-fact"})
	require.False(t, isErr, text)
	require.Contains(t, text, raOpenToken, "the restricted account reads an open project over the socket")
	text, isErr = paCall(t, cli, "memory_read", map[string]any{"project": raConf, "name": "closed-" + raConf})
	require.True(t, isErr, text)
	require.NotContains(t, text, raSecret)
	out := paCallOK(t, cli, "memory_write", map[string]any{"project": raOpen, "name": "over-the-socket", "kind": "gotcha", "description": "d", "body": "b"})
	require.Equal(t, raOpen, out["project"], "a write lands where it says")
}
