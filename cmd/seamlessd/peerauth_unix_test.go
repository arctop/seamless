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

func newPALayout(t *testing.T, users string) (paLayout, config.Config) {
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
	yaml := fmt.Sprintf("data_dir: %s\naddr: 127.0.0.1:0\nmcp:\n  api_key: k\npeer_auth:\n  enabled: true\n  owner: katata\n  socket:\n    path: %s\n%s",
		l.data, l.sock, users)
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

// paTestEnv plays a daemon running as the test's own uid, with katata a
// different account (the owner) and "me" resolving to the test's uid.
func paTestEnv(l paLayout, ownerUID uint32) peerAuthEnv {
	me := uint32(os.Geteuid())
	return peerAuthEnv{
		euid: os.Geteuid(),
		resolve: func(name string) (access.Account, error) {
			switch name {
			case "katata":
				return access.Account{UID: ownerUID, Name: "katata"}, nil
			case "me":
				return access.Account{UID: me, Name: "me"}, nil
			case "_arctop_finance":
				return access.Account{UID: 470, Name: "_arctop_finance"}, nil
			}
			return access.Account{}, fmt.Errorf("unknown account %q", name)
		},
		exe:     func() (string, error) { return l.exe, nil },
		inspect: inspectPath,
	}
}

const paFinanceUsers = "  users:\n    _arctop_finance:\n      sealed: [arctop-finance]\n"

func TestPreparePeerAuth_AcceptsTheRunbookLayout(t *testing.T) {
	l, cfg := newPALayout(t, paFinanceUsers)
	rt, err := preparePeerAuth(cfg, paTestEnv(l, uint32(os.Geteuid())+1000))
	require.NoError(t, err)
	require.Equal(t, []string{"arctop-finance"}, rt.policy.SealedProjects())
}

func TestPreparePeerAuth_Refusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(t *testing.T, l paLayout, env *peerAuthEnv)
		want   string
	}{
		{"the daemon runs as the owner", func(t *testing.T, l paLayout, env *peerAuthEnv) {
			env.resolve = paTestEnv(l, uint32(os.Geteuid())).resolve
		}, "runs as katata, the owner"},
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
				if n == "_arctop_finance" {
					return access.Account{}, fmt.Errorf("no such user")
				}
				return base(n)
			}
		}, "peer_auth.users._arctop_finance"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, cfg := newPALayout(t, paFinanceUsers)
			env := paTestEnv(l, uint32(os.Geteuid())+1000)
			tc.mutate(t, l, &env)
			_, err := preparePeerAuth(cfg, env)
			require.ErrorIs(t, err, errPeerAuthPreflight)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

// A missing data dir is created 0700 by serve once its parents pass; doctor
// only reports it.
func TestPreparePeerAuth_CreatesAMissingDataDir(t *testing.T) {
	l, cfg := newPALayout(t, paFinanceUsers)
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
		users    string
		ownerUID uint32
		status   int
		kind     string
	}{
		{"this uid is the owner", "", me, http.StatusOK, "owner"},
		{"this uid is a configured account", "  users:\n    me:\n      sealed: [arctop-finance]\n", me + 1000, http.StatusOK, "user"},
		{"this uid is unknown", paFinanceUsers, me + 1000, http.StatusForbidden, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, cfg := newPALayout(t, tc.users)
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
				Handler:     peerHandler(policy, slog.New(slog.NewTextHandler(io.Discard, nil)), whoami),
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
	l, cfg := newPALayout(t, paFinanceUsers)
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
	l, cfg := newPALayout(t, paFinanceUsers)
	env := paTestEnv(l, uint32(os.Geteuid())+1000)
	rows := peerAuthChecks(context.Background(), cfg, nil, env)
	var text strings.Builder
	for _, r := range rows {
		require.NotEqual(t, statusFail, r.status, "%s: %s", r.name, r.detail)
		fmt.Fprintf(&text, "%s %s\n", r.name, r.detail)
	}
	require.Contains(t, text.String(), "_arctop_finance (uid 470): sealed [arctop-finance]")
	require.Contains(t, text.String(), "preflight passes")
	require.Contains(t, text.String(), "reads no agent files")

	require.NoError(t, os.Chmod(l.data, 0o755))
	failed := false
	for _, r := range peerAuthChecks(context.Background(), cfg, nil, env) {
		if r.status == statusFail && strings.Contains(r.detail, "must be 0700") {
			failed = true
		}
	}
	require.True(t, failed, "doctor fails on what serve would refuse")
}

// The daemon's real route table over a real socket, the kernel naming the
// peer: the test's own uid is configured as the sealed account, and an MCP
// client built the way the seam CLI builds one (the shared client constructor,
// a socket server_url, no key) works in its project and is refused the owner's.
func TestPeerAuthSocket_MCPOverTheRealSocket(t *testing.T) {
	d := newPADaemon(t)
	d.plant(t)

	me := uint32(os.Geteuid())
	accounts := map[string]access.Account{
		"katata":          {UID: me + 1000, Name: "katata"},
		"_arctop_finance": {UID: me, Name: "_arctop_finance"},
		"_arctop_legal":   {UID: paUIDLegal, Name: "_arctop_legal"},
	}
	policy, err := access.New(paPolicyConfig(), func(n string) (access.Account, error) {
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
	srv := &http.Server{Handler: peerHandler(policy, slog.New(slog.NewTextHandler(io.Discard, nil)), d.mux), ConnContext: peerConnContext}
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

	text, isErr := paCall(t, cli, "memory_read", map[string]any{"name": d.sealed.memName})
	require.False(t, isErr, text)
	require.Contains(t, text, paSecret, "the sealed account reads its own project over the socket")
	text, isErr = paCall(t, cli, "recall", map[string]any{"query": paSearch, "project": paOpen})
	require.True(t, isErr)
	require.NotContains(t, text, paOwnSecret)
	out := paCallOK(t, cli, "memory_write", map[string]any{"name": "over-the-socket", "kind": "gotcha", "description": "d", "body": "b"})
	require.Equal(t, paSealed, out["project"], "an unbound write lands in the account's own project")
}
