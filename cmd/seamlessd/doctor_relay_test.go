//go:build darwin || linux

package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/relay"
	"github.com/arctop/seamless/internal/store"
)

// relayDoctorRows indexes doctor's relay rows by name; a name may repeat
// (one row per preflight problem), so each maps to every row it has.
func relayDoctorRows(rows []check) map[string][]check {
	out := map[string][]check{}
	for _, r := range rows {
		out[r.name] = append(out[r.name], r)
	}
	return out
}

// relayDoctorOne is the one row named name.
func relayDoctorOne(t *testing.T, rows map[string][]check, name string) check {
	t.Helper()
	require.Len(t, rows[name], 1, "%s: %v", name, rows)
	return rows[name][0]
}

// A computer with no relay gets no relay rows at all; the default looks at
// the root layout.
func TestRelayDoctor_NoRelayNoRows(t *testing.T) {
	l, env := relayCmdLayout(t)
	require.Nil(t, relayDoctorChecks(context.Background(), relayDoctorEnv{layout: l, env: env}))
	def := defaultRelayDoctorEnv()
	require.Equal(t, relay.DefaultLayout(), def.layout)
	require.Equal(t, os.Geteuid(), def.env.EUID, "doctor judges as whoever runs it")
}

// A paired, running relay, seen as its own account: the account (no pin, so a
// warning with the command that pins it), the layout, the pairing (which
// server, which certificate), the socket and the server all reported; then the
// same relay seen as another account, which cannot read the pairing but sends
// /healthz through the socket; then with the server gone, and with the relay
// stopped.
func TestRelayDoctor_ReportsThePairingTheSocketAndTheServer(t *testing.T) {
	d := deviceNewDaemon(t, false)
	l, env := relayCmdLayout(t)
	code, err := d.pairing.MintCode(context.Background(), store.DeviceByConsole)
	require.NoError(t, err)
	require.NoError(t, relayPair(context.Background(), relayPairOptions{dataDir: l.DataDir, env: env,
		pair: relay.PairOptions{Server: d.srv.URL, Code: code.Code, Name: "example-laptop", CA: relayCmdServerPEM(t, d.srv.URL, d.pool)}},
		io.Discard, io.Discard))
	st, err := relay.LoadState(l.DataDir)
	require.NoError(t, err)
	relayCmdStart(t, l, env)
	e := relayDoctorEnv{layout: l, env: env, account: func(uint32) string { return "_example_relay" }, timeout: 10 * time.Second}

	rows := relayDoctorRows(relayDoctorChecks(context.Background(), e))
	account := relayDoctorOne(t, rows, "relay.account")
	require.Equal(t, statusWarn, account.status)
	require.Contains(t, account.detail, "no pin at "+access.DaemonAccountPath)
	require.Contains(t, account.detail, pinCommand("_example_relay"))
	layout := relayDoctorOne(t, rows, "relay")
	require.Equal(t, statusOK, layout.status, layout.detail)
	require.Contains(t, layout.detail, "and this binary", "doctor as the relay's account checks the binary too")
	pairing := relayDoctorOne(t, rows, "relay.pairing")
	require.Equal(t, statusOK, pairing.status)
	for _, want := range []string{d.srv.URL, st.ServerFingerprint, st.DeviceID, "example-laptop", st.Fingerprint} {
		require.Contains(t, pairing.detail, want)
	}
	require.Equal(t, statusOK, relayDoctorOne(t, rows, "relay.socket").status)
	server := relayDoctorOne(t, rows, "relay.server")
	require.Equal(t, statusOK, server.status, server.detail)

	// Pinned as the relay's account: the clients trust no other.
	pinned := e
	pinned.env.DaemonAccount = func() (access.DaemonPin, bool, error) {
		return access.DaemonPin{Name: "_example_relay", UID: uint32(os.Geteuid()), Path: access.DaemonAccountPath}, true, nil
	}
	require.Equal(t, statusOK, relayDoctorOne(t, relayDoctorRows(relayDoctorChecks(context.Background(), pinned)), "relay.account").status)
	pinnedOther := e
	pinnedOther.env.DaemonAccount = func() (access.DaemonPin, bool, error) {
		return access.DaemonPin{Name: "_seamless", UID: uint32(os.Geteuid()) + 1, Path: access.DaemonAccountPath}, true, nil
	}
	other := relayDoctorRows(relayDoctorChecks(context.Background(), pinnedOther))
	require.Equal(t, statusFail, relayDoctorOne(t, other, "relay.account").status)
	require.Equal(t, statusFail, relayDoctorOne(t, other, "relay").status, "the preflight refuses it too")

	// Another account (the owner's): the pairing is not its to read, the path
	// through the socket is checked end to end.
	if os.Geteuid() != 0 {
		require.NoError(t, os.Chmod(filepath.Join(l.DataDir, relay.StateName), 0o000))
		t.Cleanup(func() { _ = os.Chmod(filepath.Join(l.DataDir, relay.StateName), 0o600) })
		owner := e
		owner.env.EUID = os.Geteuid() + 1
		rows = relayDoctorRows(relayDoctorChecks(context.Background(), owner))
		require.Equal(t, statusOK, relayDoctorOne(t, rows, "relay").status, "%v", rows)
		require.NotContains(t, relayDoctorOne(t, rows, "relay").detail, "binary", "another account cannot tell the relay's binary")
		pairing = relayDoctorOne(t, rows, "relay.pairing")
		require.Equal(t, statusInfo, pairing.status)
		require.Contains(t, pairing.detail, "readable by the relay's account alone")
		socket := relayDoctorOne(t, rows, "relay.socket")
		require.Equal(t, statusOK, socket.status, socket.detail)
		require.Contains(t, socket.detail, "the server behind it answered /healthz")
		require.Empty(t, rows["relay.server"], "the server is probed through the socket alone")

		// The server gone: the relay says so through the socket.
		d.srv.Close()
		rows = relayDoctorRows(relayDoctorChecks(context.Background(), owner))
		socket = relayDoctorOne(t, rows, "relay.socket")
		require.Equal(t, statusFail, socket.status)
		require.True(t, strings.HasPrefix(socket.detail, "seamless relay: the server at "+d.srv.URL+" could not be reached"), socket.detail)
		require.NoError(t, os.Chmod(filepath.Join(l.DataDir, relay.StateName), 0o600))
	} else {
		d.srv.Close()
	}
	rows = relayDoctorRows(relayDoctorChecks(context.Background(), e))
	server = relayDoctorOne(t, rows, "relay.server")
	require.Equal(t, statusFail, server.status)
	require.Contains(t, server.detail, "does not answer")
}

// An unpaired relay, a stopped one, and a socket someone else holds.
func TestRelayDoctor_UnpairedStoppedAndForeign(t *testing.T) {
	l, env := relayCmdLayout(t)
	require.NoError(t, os.Mkdir(l.DataDir, 0o700))
	e := relayDoctorEnv{layout: l, env: env, account: func(uint32) string { return "_example_relay" }, timeout: 5 * time.Second}
	rows := relayDoctorRows(relayDoctorChecks(context.Background(), e))
	pairing := relayDoctorOne(t, rows, "relay.pairing")
	require.Equal(t, statusFail, pairing.status)
	require.Contains(t, pairing.detail, "not paired: pair it as _example_relay with seamlessd relay pair")
	socket := relayDoctorOne(t, rows, "relay.socket")
	require.Equal(t, statusFail, socket.status)
	require.Contains(t, socket.detail, "no socket at "+l.Socket)
	require.Empty(t, rows["relay.server"])

	require.NoError(t, os.WriteFile(l.Socket, []byte("x"), 0o600))
	socket = relayDoctorOne(t, relayDoctorRows(relayDoctorChecks(context.Background(), e)), "relay.socket")
	require.Equal(t, statusFail, socket.status)
	require.Contains(t, socket.detail, "is not a socket")

	require.NoError(t, os.Chmod(l.DataDir, 0o755))
	rows = relayDoctorRows(relayDoctorChecks(context.Background(), e))
	var found bool
	for _, r := range rows["relay"] {
		found = found || (r.status == statusFail && strings.Contains(r.detail, "must be 0700"))
	}
	require.True(t, found, "the preflight's refusal is a failing row: %v", rows["relay"])
}

// A probe the relay sends as itself names no caller: once the server judges
// relayed callers it refuses the probe itself (400, 403), which is still the
// edge accepting the relay's certificate. Only 401 is the pairing refused.
func TestRelayProbeVerdict(t *testing.T) {
	for status, want := range map[int]bool{
		http.StatusOK: true, http.StatusBadRequest: true, http.StatusForbidden: true, http.StatusNotFound: true,
		http.StatusUnauthorized: false, http.StatusInternalServerError: false, http.StatusServiceUnavailable: false,
	} {
		ok, why := relayProbeVerdict(status, nil)
		require.Equal(t, want, ok, status)
		require.Equal(t, want, why == "", status)
	}
	ok, _ := relayProbeVerdict(0, errors.New("example: connection refused"))
	require.False(t, ok)
}

// Doctor says who may connect to the socket, and what that means without
// peer_auth on the server.
func TestRelayDoctor_SaysWhoMayConnect(t *testing.T) {
	l, env := relayCmdLayout(t)
	require.NoError(t, os.Mkdir(l.DataDir, 0o700))
	e := relayDoctorEnv{layout: l, env: env, account: func(uint32) string { return "_example_relay" }, timeout: 5 * time.Second}
	_, ok := relayAccessRow(e)
	require.False(t, ok, "no socket, no row")
	for mode, want := range map[os.FileMode]struct {
		status checkStatus
		detail string
	}{
		// A socket every account reaches is every account as the owner,
		// unless a peer_auth server maps the relay: a warning, with the fix.
		0o666: {statusWarn, "every local account may connect"},
		0o660: {statusInfo, "only the relay's account and group"},
	} {
		ln, err := env.ListenWith(l.Socket, relay.SocketAccess{Mode: mode})
		require.NoError(t, err)
		row, ok := relayAccessRow(e)
		require.True(t, ok)
		require.Equal(t, want.status, row.status, "%04o", mode)
		require.Contains(t, row.detail, want.detail)
		if mode&0o006 != 0 {
			require.Contains(t, row.detail, "--socket-mode 0660 --socket-group "+clientRelayGroup)
			require.Contains(t, row.detail, "refuses this relay until it maps it in peer_auth.relays")
		}
		require.NoError(t, ln.Close())
	}
}

// --socket-mode and --socket-group read as peer_auth.socket's mode and
// group do.
func TestRelayCmd_SocketAccess(t *testing.T) {
	a, err := relaySocketAccess(config.DefaultPeerSocketMode, "")
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o666), a.Mode)
	require.Zero(t, a.GID)
	a, err = relaySocketAccess("0660", strconv.Itoa(os.Getegid()))
	require.NoError(t, err)
	require.Equal(t, relay.SocketAccess{Mode: 0o660, GID: os.Getegid()}, a)
	for _, bad := range [][2]string{{"bogus", ""}, {"0066", ""}, {"01666", ""}, {"0660", "example-no-such-group"}, {"0660", "0"}} {
		_, err := relaySocketAccess(bad[0], bad[1])
		require.Error(t, err, "%v", bad)
	}

	l, env := relayCmdLayout(t)
	ln, err := env.ListenWith(l.Socket, relay.SocketAccess{Mode: 0o660, GID: os.Getegid()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	f, err := env.Inspect(l.Socket)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o660), f.Mode.Perm())
	require.Equal(t, uint32(os.Getegid()), f.GID)
}
