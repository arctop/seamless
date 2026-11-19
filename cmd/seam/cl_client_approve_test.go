package main

import (
	"context"
	"net/http"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
)

// seam approve on a client computer (role: client), whose relay serves the
// socket and forwards to the server: no daemon runs there, so nothing runs
// sudo. d denies over the relay, where the server judges the caller (owner
// decision R-D3), and y opens the passkey page.

// clMarkTransport marks every request the verified daemon's client sends, so a
// test can tell it from one cfg's own client sent.
type clMarkTransport struct{ next http.RoundTripper }

func (m clMarkTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("X-Cl-Verified", "yes")
	return m.next.RoundTrip(r)
}

// clVerifiedPeer is fakeDaemonPeer whose client marks what it sends.
func clVerifiedPeer(uid uint32) func(context.Context, config.Config) (uint32, *http.Client, error) {
	return func(_ context.Context, cfg config.Config) (uint32, *http.Client, error) {
		base, err := cfg.HTTPClient(consoleTimeout)
		if err != nil {
			return 0, nil, err
		}
		next := base.Transport
		if next == nil {
			next = http.DefaultTransport
		}
		return uid, &http.Client{Timeout: base.Timeout, Transport: clMarkTransport{next}}, nil
	}
}

// clServerTerminal is the terminal approver a server names: its own account,
// which no account on the client resolves to.
var clServerTerminal = &terminalApprover{Account: "_serveronly", Seamlessd: "/opt/seamless/bin/seamlessd", Config: "/opt/seamless/etc/seamless.yaml"}

// clClientRig is newApproveRig on a client computer: role client, the relay's
// socket verified (clVerifiedPeer), and no account lookup at all.
func clClientRig(t *testing.T, h http.HandlerFunc, typed string) *approveRig {
	t.Helper()
	rig := newApproveRig(t, h, typed)
	cfg := rig.ac.config()
	cfg.Role = config.RoleClient
	rig.e.loadConfig = func() (config.Config, error) { return cfg, nil }
	rig.e.dialDaemon = clVerifiedPeer(testDaemonUID)
	rig.e.lookupAccount = func(name string) (uint32, error) {
		t.Errorf("a client resolves no account the server names: %q", name)
		return 0, errNoAccountLookup
	}
	return rig
}

// clDenyRoutes serves a's facts, as a server names them, and answers its deny
// with status and body.
func clDenyRoutes(t *testing.T, a accessFacts, status int, body string, mutate ...func(*accessOne)) http.HandlerFunc {
	mutate = append([]func(*accessOne){func(o *accessOne) { o.Terminal = clServerTerminal }}, mutate...)
	table := routes(t, byRef(t, a, mutate...))
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/console/access/"+a.ID+"/deny" {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
			return
		}
		table(w, r)
	}
}

// clPosts is every POST the stand-in console saw.
func clPosts(rig *approveRig) []seenRequest {
	var out []seenRequest
	for _, r := range rig.ac.requests() {
		if r.method == http.MethodPost {
			out = append(out, r)
		}
	}
	return out
}

// clNoSudo fails when anything rig ran was sudo.
func clNoSudo(t *testing.T, rig *approveRig) {
	t.Helper()
	for _, argv := range rig.runner.recorded() {
		require.NotEqual(t, "sudo", path.Base(argv[0]), "a client runs no sudo: %v", argv)
	}
}

const clDenied = `{"denied":"R-7F3K","project":"example-finance"}`

func TestCLClientApprove_DenyPostsOverTheRelayAndRunsNoSudo(t *testing.T) {
	a := factsOf(reqA, "R-7F3K", "example-finance")
	for _, typed := range []string{"\nd\n", "\nDENY\n", "deny\n"} {
		rig := clClientRig(t, clDenyRoutes(t, a, http.StatusOK, clDenied), typed)
		require.Equal(t, 0, rig.run("R-7F3K"), "%q: %s", typed, rig.errb.String())
		require.Empty(t, rig.runner.recorded(), "%q: a client's deny runs nothing", typed)
		posts := clPosts(rig)
		require.Len(t, posts, 1, "%q", typed)
		require.Equal(t, "/console/access/"+reqA+"/deny?format=json", posts[0].uri, "%q", typed)
		require.Equal(t, "yes", posts[0].header.Get("X-Cl-Verified"), "%q: through the verified socket's client", typed)
		out := rig.out.String()
		require.True(t, strings.HasSuffix(out, "Denied R-7F3K: example-finance stays locked for that session.\n"), "%q: %s", typed, out)
		require.NotContains(t, out, "sudo asks", "%q", typed)
		require.Empty(t, rig.errb.String(), "%q", typed)
	}
}

func TestCLClientApprove_SaysWhatYAndDDoBeforeAsking(t *testing.T) {
	a := factsOf(reqA, "R-7F3K", "example-finance")
	rig := clClientRig(t, clDenyRoutes(t, a, http.StatusOK, clDenied), "\nn\n")
	require.Equal(t, 0, rig.run("R-7F3K"), rig.errb.String())
	require.Contains(t, rig.out.String(), "until the session ends, at most 8h.\n"+
		"On this computer nothing runs sudo: seamlessd approve runs on the server. y opens the request's passkey page, "+
		"for your passkey to approve it there (Touch ID approves it from the agent's dialog too), and d denies it from here.\n"+
		"Approve R-7F3K? [y/N/d=deny] Nothing changed.\n")
	require.Empty(t, rig.runner.recorded())
	require.Empty(t, clPosts(rig))

	// Linux has no Touch ID, and the dialog approves no read and write.
	rig = clClientRig(t, clDenyRoutes(t, a, http.StatusOK, clDenied), "\nn\n")
	rig.e.goos = "linux"
	require.Equal(t, 0, rig.run("R-7F3K"), rig.errb.String())
	require.Contains(t, rig.out.String(), "y opens the request's passkey page, for your passkey to approve it there, and d denies it from here.\n")
	require.NotContains(t, rig.out.String(), "Touch ID")
}

// The server's refusal -- a relay of an earlier release, an agent above the
// terminal, a chain that may be cut -- is what the owner reads, word for word,
// and nothing else is tried.
func TestCLClientApprove_TheServersRefusalIsShownAsItCame(t *testing.T) {
	a := factsOf(reqA, "R-7F3K", "example-finance")
	refusal := "this is the owner's decision, and this request came through the relay paired as example-laptop, whose caller " +
		"this server does not take for the owner's own hands: a process of the chain is an agent (claude). Make it in the " +
		"console of the server you sign in to, in a terminal on the server itself, or in a terminal of your own on that " +
		"computer, with no agent above it"
	rig := clClientRig(t, clDenyRoutes(t, a, http.StatusForbidden, `{"error":"`+refusal+`"}`), "\nd\n")
	require.Equal(t, 1, rig.run("R-7F3K"))
	require.Equal(t, "error: the decision on R-7F3K did not complete: "+refusal+"\n", rig.errb.String())
	require.Empty(t, rig.runner.recorded(), "no sudo after a refusal either")
	require.Len(t, clPosts(rig), 1)
	require.NotContains(t, rig.out.String(), "Denied")
}

// y on a client opens the passkey page with the duration chosen, and runs
// nothing else; with no page it says where an approval is made instead.
func TestCLClientApprove_YesOpensThePasskeyPageAndBuildsNoSudo(t *testing.T) {
	a := factsOf(reqA, "R-7F3K", "example-finance")
	rig := clClientRig(t, clDenyRoutes(t, a, http.StatusOK, clDenied), "1h\ny\n")
	require.Equal(t, 0, rig.run("R-7F3K"), rig.errb.String())
	page := "https://seamless.example.org/console/access/" + reqA + "?for=1h"
	require.Equal(t, [][]string{{"open", page}}, rig.runner.recorded(), "the browser opener alone")
	require.Contains(t, rig.out.String(), "Opened "+page+" in the browser: decide R-7F3K there with a passkey (approving there grants it for 1 hour).\n")
	require.Empty(t, clPosts(rig), "nothing is approved without a proof")

	for goos, ways := range map[string]string{
		"darwin": "approve it with Touch ID from the agent's dialog, or with seam approve in a terminal on the server itself",
		"linux":  "with seam approve in a terminal on the server itself",
	} {
		rig := clClientRig(t, clDenyRoutes(t, a, http.StatusOK, clDenied, func(o *accessOne) { o.Page = "" }), "\ny\n")
		rig.e.goos = goos
		require.Equal(t, 1, rig.run("R-7F3K"), goos)
		require.Equal(t, "error: nothing was approved: the daemon names no passkey page for R-7F3K, so "+ways+"\n", rig.errb.String(), goos)
		require.Contains(t, rig.out.String(), "d denies R-7F3K from here; the daemon names no passkey page for R-7F3K, so "+ways+".\n", goos)
		require.Empty(t, rig.runner.recorded(), goos)
		require.Empty(t, clPosts(rig), goos)
	}
}

// A move is approved with a passkey only, and a grant approved read and write
// is not the Touch ID dialog's either: neither is offered Touch ID.
func TestCLClientApprove_TouchIDOnlyWhereTheDialogApproves(t *testing.T) {
	move := factsOf(reqA, "R-7F3K", "example-finance")
	move.Kind = kindMove
	require.False(t, touchIDApproves(&env{goos: "darwin"}, move, ""))
	grant := factsOf(reqA, "R-7F3K", "example-finance")
	grant.Kind = kindGrant
	require.True(t, touchIDApproves(&env{goos: "darwin"}, grant, accessReadOnly))
	require.False(t, touchIDApproves(&env{goos: "darwin"}, grant, accessReadWrite))
	require.False(t, touchIDApproves(&env{goos: "linux"}, grant, accessReadOnly))
}

// A client whose server_url is no socket (a key device's https) is refused as
// before: nothing it says is verified, so nothing is shown, posted or run.
func TestCLClientApprove_UnverifiedServerIsRefusedAsBefore(t *testing.T) {
	a := factsOf(reqA, "R-7F3K", "example-finance")
	rig := clClientRig(t, clDenyRoutes(t, a, http.StatusOK, clDenied), "\nd\n")
	rig.e.dialDaemon = func(context.Context, config.Config) (uint32, *http.Client, error) { return 0, nil, errServerNotSocket }
	require.Equal(t, 1, rig.run("R-7F3K"))
	require.Equal(t, "error: "+errServerNotSocket.Error()+"\n", rig.errb.String())
	require.Empty(t, rig.out.String())
	require.Empty(t, rig.runner.recorded())
	require.Empty(t, clPosts(rig))
}

// The server's own terminal is unchanged: role server builds the sudo commit,
// verifies the account the daemon names, and posts nothing.
func TestCLClientApprove_TheServersTerminalIsUnchanged(t *testing.T) {
	a := factsOf(reqA, "R-7F3K", "example-finance")
	rig := newApproveRig(t, clDenyRoutes(t, a, http.StatusOK, clDenied, func(o *accessOne) { o.Terminal = testTerminal }), "\nd\n")
	require.Equal(t, 0, rig.run("R-7F3K"), rig.errb.String())
	require.Equal(t, append(slices.Clone(passwordlessProbe), denyArgvFor(reqA)), rig.runner.recorded())
	require.Empty(t, clPosts(rig))
	require.NotContains(t, rig.out.String(), "On this computer")
	ranSudo := false
	for _, argv := range rig.runner.recorded() {
		ranSudo = ranSudo || path.Base(argv[0]) == "sudo"
	}
	require.True(t, ranSudo, "the server's terminal commits through sudo")

	// A server naming its own account to a client is never resolved there.
	rig = clClientRig(t, clDenyRoutes(t, a, http.StatusOK, clDenied), "\nd\n")
	require.Equal(t, 0, rig.run("R-7F3K"), rig.errb.String())
	clNoSudo(t, rig)
}
