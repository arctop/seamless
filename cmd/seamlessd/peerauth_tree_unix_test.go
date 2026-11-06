//go:build darwin || linux

package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
)

// selfToken is the test process's own token, as the kernel reports it to the
// peer of a socket: what a lease anchored at "the agent" names when the test
// plays the agent.
func selfToken(t *testing.T) access.ProcessToken {
	t.Helper()
	dir, err := os.MkdirTemp("", "st")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		if c, derr := net.Dial("unix", path); derr == nil {
			t.Cleanup(func() { _ = c.Close() })
		}
	}()
	conn, err := ln.Accept()
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	peer, err := access.ReadPeer(conn)
	require.NoError(t, err)
	hops, err := peer.Chain()
	require.NoError(t, err)
	require.Equal(t, int32(os.Getpid()), hops[0].Token.PID)
	return hops[0].Token
}

// The lease follows the kernel's process tree over a real socket: the test
// plays the agent, a lease names it, and both its own requests and a child
// process's (curl) open the locked project; a lease naming another process
// opens nothing for either.
func TestPeerAuthSocket_LeaseFollowsTheProcessTree(t *testing.T) {
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("curl is not installed")
	}
	l, cfg := newPALayout(t, "  locked: [example-finance]\n")
	env := paTestEnv(l, uint32(os.Geteuid()))
	policy, err := access.New(cfg.PeerAuth, env.resolve)
	require.NoError(t, err)
	rt := &peerAuthRuntime{policy: policy, cfg: cfg.PeerAuth, env: env}
	ln, err := rt.listenSocket()
	require.NoError(t, err)

	agent := selfToken(t)
	stranger := access.ProcessToken{PID: agent.PID + 1, Version: agent.Version, Boot: agent.Boot}
	var leaseFor atomic.Value // the process the lease names; the server goroutine reads it
	leaseFor.Store(agent.Key())
	leases := peerLeases{load: func(_ context.Context, _ []string, now time.Time) ([]access.Lease, error) {
		return []access.Lease{{ID: "l1", Project: "example-finance", Process: leaseFor.Load().(string), Expires: now.Add(time.Hour)}}, nil
	}}
	visible := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v, _ := access.FromContext(r.Context())
		if v.Visible("example-finance") {
			_, _ = io.WriteString(w, "open")
			return
		}
		_, _ = io.WriteString(w, "locked")
	})
	srv := &http.Server{
		Handler:     peerHandler(policy, leases, slog.New(slog.NewTextHandler(io.Discard, nil)), visible),
		ConnContext: peerConnContext,
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	client := config.Config{Role: config.RoleClient, AdvertisedURL: "unix://" + l.sock}
	hc, err := client.HTTPClient(0)
	require.NoError(t, err)
	hc.Transport.(*http.Transport).DisableKeepAlives = true
	fromSelf := func() string {
		resp, err := hc.Get(client.ServerURL() + "/v")
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return string(b)
	}
	fromChild := func() string {
		out, err := exec.Command(curl, "-sS", "--unix-socket", l.sock, "http://seamless/v").Output()
		require.NoError(t, err)
		return strings.TrimSpace(string(out))
	}

	require.Equal(t, "open", fromSelf(), "the agent's own request")
	require.Equal(t, "open", fromChild(), "a child of the agent shares its lease")

	leaseFor.Store(stranger.Key())
	require.Equal(t, "locked", fromSelf(), "a lease naming another process opens nothing")
	require.Equal(t, "locked", fromChild())
}
