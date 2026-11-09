package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/config"
)

// testDaemonUID is the stand-in daemon's uid: by construction neither this
// test's own nor root's.
var testDaemonUID = uint32(os.Geteuid()) + 1000

// fakeDaemonPeer stands in for the kernel: uid answered, and the client is the
// config's own, so a TCP stand-in console can be read through it.
func fakeDaemonPeer(uid uint32) func(context.Context, config.Config) (uint32, *http.Client, error) {
	return func(_ context.Context, cfg config.Config) (uint32, *http.Client, error) {
		client, err := cfg.HTTPClient(consoleTimeout)
		return uid, client, err
	}
}

// fakeAccounts knows only the account the fixtures name for the daemon.
func fakeAccounts(name string) (uint32, error) {
	if name == testTerminal.Account {
		return testDaemonUID, nil
	}
	return 0, user.UnknownUserError(name)
}

// configFileFor loads a real config file whose server_url is the socket, and
// returns it with the file's path.
func configFileFor(t *testing.T, sock string) (config.Config, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "seamless.yaml")
	require.NoError(t, os.WriteFile(path, []byte("server_url: unix://"+sock+"\n"), 0o600))
	cfg, err := config.LoadFrom(path)
	require.NoError(t, err)
	return cfg, path
}

func requirePeerCred(t *testing.T) {
	t.Helper()
	if !access.PeerCredSupported {
		t.Skip("no Unix-socket peer credentials on this platform")
	}
}

// The review's proof of concept, on a real socket with the real kernel lookup:
// server_url repointed at a socket this process serves, whose answer would run
// the agent's own program as root. The socket is refused by who serves it,
// before a single request reaches it, so nothing it says is shown or run.
func TestApprove_RefusesAPlantedSocket(t *testing.T) {
	requirePeerCred(t)
	forged := factsOf(reqA, "R-7F3K", "harmless-docs")
	forged.Reason = "fix a typo in the readme"
	forged.Digest = strings.Repeat("a", 64)
	body := oneJSON(t, forged, func(o *accessOne) {
		o.Terminal = &terminalApprover{Account: "root", Seamlessd: "/tmp/agent/seamlessd", Config: "/tmp/agent/evil.yaml"}
	})
	var served atomic.Int32
	sock := serveTouchSocket(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served.Add(1)
		routes(t, map[string]string{"GET /console/access/" + reqA: body})(w, r)
	}))
	cfg, path := configFileFor(t, sock)

	rig := newApproveRig(t, routes(t, nil), "y\n")
	rig.e.loadConfig = func() (config.Config, error) { return cfg, nil }
	rig.e.dialDaemon = dialSocketDaemon

	require.Equal(t, 1, rig.run(reqA, "--for", "1h"))
	require.Contains(t, rig.errb.String(), "unix:"+sock, "the message names the socket")
	require.Contains(t, rig.errb.String(), path, "and the config file")
	require.Contains(t, rig.errb.String(), "your own uid")
	require.Contains(t, rig.errb.String(), "server_url in "+path+" may have been changed")
	require.Equal(t, 1, strings.Count(rig.errb.String(), "\n"), "one line")
	require.Empty(t, rig.out.String(), "nothing the planted socket said is shown as fact")
	require.Empty(t, rig.runner.recorded(), "no sudo runs")
	require.Zero(t, served.Load(), "no request reaches it")

	// The same with no request named: nothing is listed either.
	rig = newApproveRig(t, routes(t, nil), "")
	rig.e.loadConfig = func() (config.Config, error) { return cfg, nil }
	rig.e.dialDaemon = dialSocketDaemon
	require.Equal(t, 1, rig.run())
	require.Empty(t, rig.out.String())
	require.Zero(t, served.Load())
}

// Everything that fails the check fails it before any fact is on the screen,
// and runs nothing.
func TestApprove_NothingIsShownUntilThePeerIsVerified(t *testing.T) {
	a := factsOf(reqA, "R-7F3K", "example-finance")
	own := uint32(os.Geteuid())
	withAccount := func(account string) *terminalApprover {
		return &terminalApprover{Account: account, Seamlessd: "/opt/seamless/bin/seamlessd", Config: "/opt/seamless/seamless.yaml"}
	}
	for _, tc := range []struct {
		name     string
		peer     uint32
		dialErr  error
		terminal *terminalApprover
		want     string
	}{
		{name: "the caller's own uid", peer: own, want: "your own uid"},
		{name: "root", peer: 0, want: "root (uid 0)"},
		{name: "a socket nothing answers", dialErr: errors.New("connect: no such file or directory"), want: "no such file"},
		{name: "account root", peer: testDaemonUID, terminal: withAccount("root"), want: "which is root"},
		{name: "account ROOT", peer: testDaemonUID, terminal: withAccount("ROOT"), want: "which is root"},
		{name: "account #0", peer: testDaemonUID, terminal: withAccount("#0"), want: "which is root"},
		{name: "account #00", peer: testDaemonUID, terminal: withAccount("#00"), want: "which is root"},
		{name: "an account that is another uid", peer: testDaemonUID + 1, want: fmt.Sprintf("(uid %d), but uid %d answered", testDaemonUID, testDaemonUID+1)},
		{name: "a #uid that is another uid", peer: testDaemonUID, terminal: withAccount(fmt.Sprintf("#%d", testDaemonUID+1)),
			want: fmt.Sprintf("but uid %d answered", testDaemonUID)},
		{name: "an account nobody knows", peer: testDaemonUID, terminal: withAccount("_nobody"), want: "cannot resolve"},
		{name: "a # that is no uid", peer: testDaemonUID, terminal: withAccount("#abc"), want: "cannot resolve"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			table := byRef(t, a, func(o *accessOne) {
				if tc.terminal != nil {
					o.Terminal = tc.terminal
				}
			})
			rig := newApproveRig(t, routes(t, table), "y\n")
			peer := fakeDaemonPeer(tc.peer)
			rig.e.dialDaemon = func(ctx context.Context, cfg config.Config) (uint32, *http.Client, error) {
				if tc.dialErr != nil {
					return 0, nil, tc.dialErr
				}
				return peer(ctx, cfg)
			}
			require.Equal(t, 1, rig.run("R-7F3K", "--for", "1h"))
			require.Contains(t, rig.errb.String(), tc.want)
			require.Equal(t, 1, strings.Count(rig.errb.String(), "\n"), "one line: %s", rig.errb.String())
			require.Empty(t, rig.out.String(), "no fact shown")
			require.Empty(t, rig.runner.recorded(), "no sudo run")
		})
	}
}

// A TCP server_url says nothing of who answered, so the non-advisory path is
// refused, the request named or picked from the list, before it is shown.
func TestApprove_TCPIsRefusedOnTheNonAdvisoryPath(t *testing.T) {
	a := factsOf(reqA, "R-7F3K", "example-finance")
	table := byRef(t, a)
	table["GET /console/access"] = listJSON(t, a)
	for _, argv := range [][]string{{"R-7F3K", "--for", "1h"}, {}} {
		rig := newApproveRig(t, routes(t, table), "y\n")
		rig.e.dialDaemon = dialSocketDaemon
		require.Equal(t, 1, rig.run(argv...), "%v", argv)
		require.Equal(t, "error: "+errServerNotSocket.Error()+"\n", rig.errb.String(), "%v", argv)
		require.Contains(t, rig.errb.String(), "passkey page", "%v", argv)
		require.Empty(t, rig.out.String(), "%v: nothing shown", argv)
		require.Empty(t, rig.runner.recorded(), "%v: no sudo", argv)
	}
}

// The advisory path never runs sudo and only posts back to the daemon that
// asked, so it works over TCP, the real check notwithstanding, named or picked.
func TestApprove_AdvisoryStaysUnverifiedOverTCP(t *testing.T) {
	f := grantFacts()
	table := byRef(t, f, advisoryOne)
	table["POST /console/access/"+reqA+"/deny"] = `{"denied":"R-7F3K","project":"example-finance"}`
	table["GET /console/access"] = mustJSON(t, accessList{Requests: []accessFacts{f}, Durations: []string{"session", "1h", "4h"}, Advisory: true})
	for _, argv := range [][]string{{"R-7F3K"}, {}} {
		rig := newApproveRig(t, routes(t, table), "\nd\n")
		rig.e.dialDaemon = dialSocketDaemon
		require.Equal(t, 0, rig.run(argv...), rig.errb.String())
		out := rig.out.String()
		require.Contains(t, out, "Access request R-7F3K\n", "%v", argv)
		require.Contains(t, out, "This approval is advisory", "%v", argv)
		require.Contains(t, out, "Denied R-7F3K: example-finance stays closed to that session.\n", "%v", argv)
		require.Empty(t, rig.runner.recorded(), "%v: no sudo", argv)
	}
}

// --passkey opens a page and builds no sudo command line, so no peer is asked.
func TestApprove_PasskeyAsksNoPeer(t *testing.T) {
	a := factsOf(reqA, "R-7F3K", "example-finance")
	rig := newApproveRig(t, routes(t, byRef(t, a)), "")
	rig.e.dialDaemon = nil
	rig.e.lookupAccount = nil
	require.Equal(t, 0, rig.run("--passkey", reqA), rig.errb.String())
	require.Equal(t, [][]string{{"open", "https://seamless.example.org/console/access/" + reqA}}, rig.runner.recorded())
}

// An env that forgot to name a dialer refuses rather than believe anyone.
func TestApprove_NoDialerRefuses(t *testing.T) {
	a := factsOf(reqA, "R-7F3K", "example-finance")
	rig := newApproveRig(t, routes(t, byRef(t, a)), "y\n")
	rig.e.dialDaemon = nil
	require.Equal(t, 1, rig.run("R-7F3K", "--for", "1h"))
	require.Contains(t, rig.errb.String(), errNoDaemonDialer.Error())
	require.Empty(t, rig.out.String())
	require.Empty(t, rig.runner.recorded())
}

// The verified daemon's facts are the ones shown and acted on, the #uid form
// included: sudo -u takes it as it stands.
func TestApprove_AccountNamedByUIDMatchesThePeer(t *testing.T) {
	a := factsOf(reqA, "R-7F3K", "example-finance")
	account := fmt.Sprintf("#%d", testDaemonUID)
	table := byRef(t, a, func(o *accessOne) {
		o.Terminal = &terminalApprover{Account: account, Seamlessd: "/opt/seamless/bin/seamlessd", Config: "/opt/seamless/seamless.yaml"}
	})
	rig := newApproveRig(t, routes(t, table), "y\n")
	rig.e.lookupAccount = nil // a #uid needs no lookup
	require.Equal(t, 0, rig.run("R-7F3K", "--for", "1h"), rig.errb.String())
	require.Contains(t, rig.out.String(), "Access request R-7F3K\n")
	calls := rig.runner.recorded()
	require.Equal(t, []string{"sudo", "-k", "-u", account}, calls[len(calls)-1][:4])
}

// A socket the agent controls can be swapped between the check and the read, so
// every connection the client opens is held to the uid the check saw.
func TestPinSocketPeer(t *testing.T) {
	serve := func(t *testing.T) (string, *atomic.Int32) {
		var served atomic.Int32
		return serveTouchSocket(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			served.Add(1)
			_, _ = w.Write([]byte("{}"))
		})), &served
	}
	const url = "http://localhost/console/access"
	ctx := context.Background()

	t.Run("the same uid reads", func(t *testing.T) {
		sock, served := serve(t)
		peer, client, err := pinSocketPeer(ctx, sock, func(net.Conn) (uint32, error) { return 431, nil })
		require.NoError(t, err)
		require.Equal(t, uint32(431), peer)
		resp, err := client.Get(url)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		require.Equal(t, int32(1), served.Load())
	})
	t.Run("another uid on a later connection reads nothing", func(t *testing.T) {
		sock, served := serve(t)
		var calls atomic.Int32
		_, client, err := pinSocketPeer(ctx, sock, func(net.Conn) (uint32, error) {
			if calls.Add(1) == 1 {
				return 431, nil // the real daemon, for the check
			}
			return 501, nil // the planted one, behind the swapped path
		})
		require.NoError(t, err)
		resp, err := client.Get(url)
		if resp != nil {
			require.NoError(t, resp.Body.Close())
		}
		require.ErrorIs(t, err, errPeerChanged)
		require.Zero(t, served.Load())
	})
	t.Run("the kernel cannot be asked", func(t *testing.T) {
		sock, _ := serve(t)
		errBoom := errors.New("boom")
		_, client, err := pinSocketPeer(ctx, sock, func(net.Conn) (uint32, error) { return 0, errBoom })
		require.ErrorIs(t, err, errBoom)
		require.Contains(t, err.Error(), sock)
		require.Nil(t, client)
	})
	t.Run("a socket nothing serves", func(t *testing.T) {
		_, _, err := pinSocketPeer(ctx, filepath.Join(touchSocketDir(t), "gone.sock"), func(net.Conn) (uint32, error) {
			t.Fatal("no connection, no peer")
			return 0, nil
		})
		require.ErrorContains(t, err, "console unreachable at unix:")
	})
}

// The real lookup, through a real socket: the kernel names this process.
func TestDialSocketDaemon(t *testing.T) {
	requirePeerCred(t)
	sock := serveTouchSocket(t, http.NotFoundHandler())
	cfg := config.Defaults()
	cfg.AdvertisedURL = "unix://" + sock
	peer, client, err := dialSocketDaemon(context.Background(), cfg)
	require.NoError(t, err)
	require.Equal(t, uint32(os.Geteuid()), peer)
	resp, err := client.Get("http://localhost/x")
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusNotFound, resp.StatusCode)

	_, _, err = dialSocketDaemon(context.Background(), config.Defaults())
	require.ErrorIs(t, err, errServerNotSocket)
}

// A planted socket, a root account and a mismatched one are all refused before
// seam touchid enroll prints the command it would have the owner run.
func TestTouchIDEnroll_VerifiesTheDaemon(t *testing.T) {
	t.Run("a planted socket, by the real lookup", func(t *testing.T) {
		requirePeerCred(t)
		var served atomic.Int32
		d := happyDaemon()
		rig := newTouchRig(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			served.Add(1)
			d.ServeHTTP(w, r)
		}), "\n")
		cfg, path := configFileFor(t, rig.sock)
		rig.e.loadConfig = func() (config.Config, error) { return cfg, nil }
		rig.e.dialDaemon = dialSocketDaemon
		require.Equal(t, 1, rig.run("enroll"))
		require.Contains(t, rig.errb.String(), rig.sock)
		require.Contains(t, rig.errb.String(), path)
		require.Contains(t, rig.errb.String(), "may have been changed")
		require.Empty(t, rig.out.String())
		require.Empty(t, rig.runner.recorded(), "nothing installs, nothing runs")
		require.False(t, isDir(rig.target()))
		require.Zero(t, served.Load(), "no request reaches it")
	})
	for _, tc := range []struct {
		name     string
		peer     uint32
		terminal *terminalApprover
		want     string
	}{
		{name: "the caller's own uid", peer: uint32(os.Geteuid()), terminal: testTerminal, want: "your own uid"},
		{name: "root", peer: 0, terminal: testTerminal, want: "root (uid 0)"},
		{name: "account root", peer: testDaemonUID, want: "which is root",
			terminal: &terminalApprover{Account: "root", Seamlessd: "/x", Config: "/y"}},
		{name: "account #0", peer: testDaemonUID, want: "which is root",
			terminal: &terminalApprover{Account: "#0", Seamlessd: "/x", Config: "/y"}},
		{name: "an account that is another uid", peer: testDaemonUID + 1, terminal: testTerminal, want: "but uid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newTouchRig(t, &stubDaemon{terminal: tc.terminal}, "\n")
			rig.e.dialDaemon = fakeDaemonPeer(tc.peer)
			require.Equal(t, 1, rig.run("enroll"))
			require.Contains(t, rig.errb.String(), tc.want)
			require.NotContains(t, rig.out.String(), "enroll-code", "no command line built from it is shown")
			require.Empty(t, rig.runner.recorded(), "nothing installs, nothing runs")
		})
	}
}

// An app that passed its install is held to the same checks right before it
// opens: one replaced in between is refused, and the app does not run.
func TestTouchIDEnroll_ChecksTheAppAgainBeforeOpening(t *testing.T) {
	d := happyDaemon()
	rig := newTouchRig(t, d, "")
	rig.runner.helper = enrolls(d)
	rig.e.exec = func(ctx context.Context, argv []string, in io.Reader, out, errb io.Writer) (int, error) {
		if argv[0] == "codesign" && slices.Contains(argv, "--verify") && argv[len(argv)-1] == rig.target() {
			_, _ = io.WriteString(errb, "a sealed resource is missing or invalid")
			return 1, nil // the staged copy verified; what now sits at the target does not
		}
		return rig.runner.exec(ctx, argv, in, out, errb)
	}
	require.Equal(t, 1, rig.run("enroll"))
	require.Contains(t, rig.errb.String(), "code signature does not verify")
	require.Contains(t, rig.errb.String(), "seam touchid enroll")
	for _, c := range rig.runner.recorded() {
		require.NotEqual(t, "open", c[0], "the refused app never runs")
	}
}

func TestSelfReportedNote_IsTheDaemonsSentence(t *testing.T) {
	require.Equal(t, approval.SelfReportedNote, selfReportedNote)
}

// The note follows the facts it qualifies, once, on the advisory tier too.
func TestApprove_SelfReportedNoteIsShownOnce(t *testing.T) {
	a := factsOf(reqA, "R-7F3K", "example-finance")
	rig := newApproveRig(t, routes(t, byRef(t, a)), "\nn\n")
	require.Equal(t, 0, rig.run("R-7F3K"))
	out := rig.out.String()
	require.Equal(t, 1, strings.Count(out, selfReportedNote), out)
	require.Contains(t, out, "\n"+selfReportedNote+"\n", "a line of its own")
	require.Less(t, strings.Index(out, "Working in"), strings.Index(out, selfReportedNote))
	require.Less(t, strings.Index(out, "(claude-opus-5-5)"), strings.Index(out, selfReportedNote))

	rig = newApproveRig(t, routes(t, byRef(t, grantFacts(), advisoryOne)), "\nn\n")
	require.Equal(t, 0, rig.run("R-7F3K"), rig.errb.String())
	require.Equal(t, 1, strings.Count(rig.out.String(), selfReportedNote))
}

func TestAccountUID(t *testing.T) {
	e := &env{lookupAccount: fakeAccounts}
	uid, err := e.accountUID("#431")
	require.NoError(t, err)
	require.Equal(t, uint32(431), uid)

	uid, err = e.accountUID(testTerminal.Account)
	require.NoError(t, err)
	require.Equal(t, testDaemonUID, uid)

	_, err = e.accountUID("#x")
	require.ErrorContains(t, err, "not a uid")

	_, err = e.accountUID("#99999999999")
	require.ErrorContains(t, err, "not a uid", "past a uid's range")

	_, err = e.accountUID("_nobody")
	require.ErrorAs(t, err, new(user.UnknownUserError))

	_, err = (&env{}).accountUID("_seamless")
	require.ErrorIs(t, err, errNoAccountLookup, "an unset lookup refuses")
}

func TestLookupAccountUID(t *testing.T) {
	me, err := user.Current()
	require.NoError(t, err)
	uid, err := lookupAccountUID(me.Username)
	if err != nil {
		t.Skipf("this account is not resolvable by name here: %v", err)
	}
	require.Equal(t, me.Uid, fmt.Sprint(uid))

	_, err = lookupAccountUID("seamless-no-such-account-x")
	require.Error(t, err)
}

func TestIsRootAccount(t *testing.T) {
	for account, want := range map[string]bool{
		"root": true, "ROOT": true, "Root": true, "#0": true, "#00": true,
		"_seamless": false, "#1": false, "#501": false, "#abc": false, "rootish": false, "": false,
	} {
		require.Equal(t, want, isRootAccount(account), account)
	}
}
