package main

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
)

// seam touchid enroll on a client computer (role: client), whose relay serves
// the socket and forwards to the server: the account, seamlessd and config the
// daemon names are the server's, so the commands that mint a code and remove a
// key run in a terminal on the server, and nothing resolves the server's
// account here (the peer the kernel names is the relay, held to root's pin).

// tidServerTerminal is the terminal approver a server names: an account no
// account on the client resolves to.
var tidServerTerminal = &terminalApprover{Account: "_serveronly", Seamlessd: "/opt/seamless/bin/seamlessd", Config: "/opt/seamless/etc/seamless.yaml"}

// tidClientRig is newTouchRig on a client computer: role client, a relay on
// this computer serving the socket, and an account lookup that fails the test
// when it is asked anything.
func tidClientRig(t *testing.T, d *stubDaemon) *touchRig {
	t.Helper()
	rig := tidPeerAuthOwnerRig(t, d)
	rig.e.relayHere = func() bool { return true }
	rig.e.lookupAccount = func(name string) (uint32, error) {
		t.Errorf("a client resolves no account the server names: %q", name)
		return 0, errNoAccountLookup
	}
	return rig
}

// tidPeerAuthOwnerRig is newTouchRig as the owner's half of a peer_auth
// install sees it: role client, the socket the daemon serves on this computer,
// and no relay here.
func tidPeerAuthOwnerRig(t *testing.T, d *stubDaemon) *touchRig {
	t.Helper()
	rig := newTouchRig(t, d, "")
	cfg, err := rig.e.loadConfig()
	require.NoError(t, err)
	cfg.Role = config.RoleClient
	rig.e.loadConfig = func() (config.Config, error) { return cfg, nil }
	rig.e.relayHere = func() bool { return false }
	return rig
}

// The owner's config on a peer_auth server is role: client on the daemon's
// own socket: no relay, so seam touchid enroll is a server's there. The app
// is told no --role (an app from before that flag refuses it), the commands
// run on this Mac, and the account the daemon names is still held to the peer.
func TestTouchIDEnrollOnPeerAuthServer_IsAServersEnrollment(t *testing.T) {
	d := &stubDaemon{terminal: testTerminal}
	rig := tidPeerAuthOwnerRig(t, d)
	var shown string
	rig.runner.helper = func(argv []string) {
		shown = rig.out.String()
		tidEnrolls(t, d)(argv)
	}
	require.Equal(t, 0, rig.run("enroll", "--name", "Touch ID on server"), rig.errb.String())

	calls := rig.runner.recorded()
	require.Equal(t, []string{"open", "-W", "-n", "-a", rig.target(), "--args",
		"enroll", "--socket", rig.sock, "--name", "Touch ID on server"}, calls[len(calls)-1],
		"a server's argv: no --role")
	require.Contains(t, shown, "Run that command in another terminal on this Mac")
	require.NotContains(t, shown, "this computer is a client")
	require.Contains(t, rig.out.String(), `Enrolled the Touch ID key "Touch ID on server" (01KTOUCHIDKEY0000000000001)`)

	// The account the daemon names must still be the uid that answered.
	rig = tidPeerAuthOwnerRig(t, &stubDaemon{terminal: tidServerTerminal})
	require.Equal(t, 1, rig.run("enroll"))
	require.Contains(t, rig.errb.String(), `it names "_serveronly" as its account, which this machine cannot resolve`)
	require.Empty(t, rig.runner.recorded(), "nothing installs, nothing runs")
}

// tidEnrolls makes the fake helper enroll a key on d under its --name.
func tidEnrolls(t *testing.T, d *stubDaemon) func([]string) {
	return func(argv []string) {
		i := slices.Index(argv, "--name")
		require.Positive(t, i, "%v", argv)
		d.addKey("01KTOUCHIDKEY0000000000001", argv[i+1])
	}
}

func TestTouchIDEnrollOnClient_SaysTheCommandsRunOnTheServer(t *testing.T) {
	d := &stubDaemon{terminal: tidServerTerminal}
	d.addKey("01KPREVKEY0000000000000002", "Touch ID on laptop")
	rig := tidClientRig(t, d)
	var shown string
	rig.runner.helper = func(argv []string) {
		shown = rig.out.String()
		tidEnrolls(t, d)(argv)
	}
	require.Equal(t, 0, rig.run("enroll", "--name", "Touch ID on laptop"), rig.errb.String())

	calls := rig.runner.recorded()
	require.Equal(t, []string{"open", "-W", "-n", "-a", rig.target(), "--args",
		"enroll", "--socket", rig.sock, "--name", "Touch ID on laptop", "--role", "client"}, calls[len(calls)-1],
		"the app is told it is on a client, so it looks up no account and says where its commands run")
	require.Contains(t, shown, "Run that command in a terminal on the server, not on this Mac: this computer is a client (role: client)")
	require.Contains(t, shown, "at the server or over SSH")
	require.Contains(t, shown, "Type the code into the window here, then touch the sensor.")
	require.NotContains(t, shown, "another terminal on this Mac")

	out := rig.out.String()
	require.Contains(t, out, `Enrolled the Touch ID key "Touch ID on laptop" (01KTOUCHIDKEY0000000000001)`)
	require.Contains(t, out, "If it was this Mac's, remove it as the daemon's account in a terminal on the server (not on this computer, a client):\n\n"+
		"  /usr/bin/sudo -k -u _serveronly /usr/bin/env SEAMLESS_CONFIG=/opt/seamless/etc/seamless.yaml /opt/seamless/bin/seamlessd approvers remove 01KPREVKEY0000000000000002\n")
	tidNoSudoRan(t, rig)
}

// The same daemon, its account unknown here, is refused on a server: there the
// account it names must be the uid that answered.
func TestTouchIDEnrollOnServer_StillHoldsTheAccountToThePeer(t *testing.T) {
	rig := newTouchRig(t, &stubDaemon{terminal: tidServerTerminal}, "")
	require.Equal(t, 1, rig.run("enroll"))
	require.Contains(t, rig.errb.String(), `it names "_serveronly" as its account, which this machine cannot resolve`)
	require.Empty(t, rig.runner.recorded(), "nothing installs, nothing runs")
}

// On a client the relay is still verified: a socket answered by the caller's
// own uid, root, or an account the pin does not name is refused before the
// app opens, and the server's account shapes are still checked.
func TestTouchIDEnrollOnClient_StillVerifiesTheRelay(t *testing.T) {
	for _, tc := range []struct {
		name     string
		peer     uint32
		pin      uint32
		terminal *terminalApprover
		want     string
	}{
		{name: "the caller's own uid", peer: uint32(os.Geteuid()), terminal: tidServerTerminal, want: "your own uid"},
		{name: "root", peer: 0, terminal: tidServerTerminal, want: "root (uid 0)"},
		{name: "not the pinned relay", peer: testDaemonUID + 1, pin: testDaemonUID, terminal: tidServerTerminal, want: "pins the daemon's account"},
		{name: "account root", peer: testDaemonUID, terminal: &terminalApprover{Account: "root", Seamlessd: "/x", Config: "/y"}, want: "which is root"},
		{name: "not an account name", peer: testDaemonUID, terminal: &terminalApprover{Account: "-oops", Seamlessd: "/x", Config: "/y"}, want: "not an account name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := tidClientRig(t, &stubDaemon{terminal: tc.terminal})
			rig.e.dialDaemon = fakeDaemonPeer(tc.peer)
			if tc.pin != 0 {
				rig.e.daemonAccount = pinnedTo(tc.pin)
			}
			require.Equal(t, 1, rig.run("enroll"))
			require.Contains(t, rig.errb.String(), tc.want)
			require.NotContains(t, rig.out.String(), "Opening Seamless Approve", "refused before the app opens")
			require.Empty(t, rig.runner.recorded(), "nothing installs, nothing runs")
		})
	}
}

// An app older than this seam, kept at the y/N, refuses --role in its own
// alert and enrolls nothing: on a client the refusal says how to replace it.
func TestTouchIDEnrollOnClient_AnOlderAppSaysHowToReplaceIt(t *testing.T) {
	for _, onClient := range []bool{false, true} {
		d := &stubDaemon{terminal: testTerminal}
		var rig *touchRig
		if onClient {
			d.terminal = tidServerTerminal
			rig = tidClientRig(t, d)
		} else {
			rig = newTouchRig(t, d, "")
		}
		require.Equal(t, 1, rig.run("enroll"))
		errs := rig.errb.String()
		require.Contains(t, errs, "no new Touch ID key is enrolled", "onClient %v", onClient)
		hint := "If Seamless Approve said it was started with arguments it does not take, the installed app is older than this seam: " +
			"run seam touchid enroll again and answer y to replace it"
		if onClient {
			require.Contains(t, errs, hint)
		} else {
			require.NotContains(t, errs, hint, "a server's argv is what an older app takes")
		}
	}
}

func TestTouchID_ReportReplacedOnClient(t *testing.T) {
	added := []approverKey{{ID: "01KTOUCHIDKEY0000000000001", Name: "Touch ID on laptop"}}
	after := append([]approverKey{{ID: "not-a-ulid", Name: "Touch ID on laptop"}}, added...)
	var out strings.Builder
	reportReplacedOn(&out, *tidServerTerminal, true, after, after, added)
	require.Contains(t, out.String(), "remove it with `seamlessd approvers remove`, run as the daemon's account in a terminal on the server (not on this computer, a client); "+
		"if it is another Mac's, keep it.")
	out.Reset()
	reportReplacedOn(&out, *tidServerTerminal, false, after, after, added)
	require.Contains(t, out.String(), "run as the daemon's account; if it is another Mac's, keep it.")
	require.NotContains(t, out.String(), "server")
}

// tidNoSudoRan fails when anything rig ran was sudo: a client runs none.
func tidNoSudoRan(t *testing.T, rig *touchRig) {
	t.Helper()
	for _, argv := range rig.runner.recorded() {
		require.NotContains(t, argv[0], "sudo", "a client runs no sudo: %v", argv)
	}
}
