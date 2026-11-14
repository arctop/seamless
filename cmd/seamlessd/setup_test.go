package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"io"
	"io/fs"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/update"
)

// setupTestVersion parses v, failing the test if it is not a version.
func setupTestVersion(t *testing.T, v string) update.Version {
	t.Helper()
	got, ok := update.Parse(v)
	require.True(t, ok, v)
	return got
}

// setupNoRead is a terminal no question may read: the run is not supposed to
// ask anything.
type setupNoRead struct{ t *testing.T }

func (r setupNoRead) Read([]byte) (int, error) {
	r.t.Helper()
	r.t.Fatal("setup read an answer on a run that must not ask")
	return 0, io.EOF
}

// setupClientCall is one config write a Client answered at the terminal.
type setupClientCall struct{ url, key string }

// setupTestRun is a run over scripted answers. With input "" and interactive
// false it fails the test if anything reads the terminal.
func setupTestRun(t *testing.T, goos string, environ []string, facts setupFacts, interactive bool, input string) (*setupRun, *strings.Builder, *[]setupClientCall) {
	t.Helper()
	out := &strings.Builder{}
	var calls []setupClientCall
	var src io.Reader = strings.NewReader(input)
	if !interactive && input == "" {
		src = setupNoRead{t}
	}
	in := bufio.NewReader(src)
	r := &setupRun{
		goos: goos, environ: environ, facts: facts, interactive: interactive, in: in, out: out,
		secret: func() (string, error) { return in.ReadString('\n') },
		ensureClient: func(url, key string) (string, error) {
			calls = append(calls, setupClientCall{url, key})
			return "/h/.config/seamless/seamless.yaml", nil
		},
		// A step no test arranged must never load the machine's config, whose
		// data dir is the live one: this run's is a loopback install's, in a
		// scratch directory (setupImportTestRun arranges an import's).
		home: "/h", host: "here",
		loadConfig: func() (config.Config, error) {
			cfg := config.Defaults()
			cfg.DataDir = filepath.Join(t.TempDir(), "data")
			return cfg, nil
		},
		daemonUp: func(config.Config) bool { return false },
		// The address step reads no real network, tailnet or pin, and writes
		// no config, unless its test arranges one (setupAddrArrange).
		network: hostNet{
			lookup: func(_ context.Context, host string) ([]netip.Addr, error) {
				t.Helper()
				t.Fatalf("setup resolved %s on a run whose test arranged no network", host)
				return nil, nil
			},
			ifaces: func() ([][]netip.Addr, error) { return nil, nil },
		},
		tailscale:     setupAddrTailnet{err: errNoTailscale},
		now:           time.Now,
		daemonAccount: func() string { return "_seamless" },
		ensureServer: func(config.ServerAddress) (string, error) {
			t.Helper()
			t.Fatal("setup wrote a Server config on a run whose test arranged none")
			return "", nil
		},
	}
	return r, out, &calls
}

// The installed layouts the tests stand up.
var (
	setupFactsFresh      = setupFacts{configPath: "/h/.config/seamless/seamless.yaml"}
	setupFactsStandalone = setupFacts{configPath: "/h/.config/seamless/seamless.yaml", configFound: true, userService: true}
	setupFactsServer     = setupFacts{configPath: "/h/.config/seamless/seamless.yaml", configFound: true, userService: true,
		addr: "0.0.0.0:8081", serverURL: "https://box.example:8081"}
	setupFactsClient = setupFacts{configPath: "/h/.config/seamless/seamless.yaml", configFound: true,
		role: "client", serverURL: "https://box.example:8081/"}
	setupFactsLocked = setupFacts{configPath: "/h/.config/seamless/seamless.yaml", configFound: true,
		role: "client", serverURL: "unix:///opt/seamless/run/seamlessd.sock", systemService: true}
)

func TestSetupDecide(t *testing.T) {
	type want struct {
		kind      setupKind
		role      string
		serverURL string
		answer    setupAnswer
		change    string
	}
	server, client := config.RoleServer, config.RoleClient
	for _, tt := range []struct {
		name        string
		goos        string
		env         []string
		facts       setupFacts
		interactive bool
		input       string
		want        want
		err         string
		out         []string
		noOut       []string
		wrote       []setupClientCall
	}{
		// A first install.
		{name: "no terminal: today's install, nothing asked", facts: setupFactsFresh,
			want: want{kind: setupStandalone, role: server, answer: setupDefault}},
		{name: "a terminal and a knob: nothing asked", facts: setupFactsFresh, interactive: true, env: []string{"SEAMLESS_VERSION=0.9.0"},
			want: want{kind: setupStandalone, role: server, answer: setupDefault}},
		{name: "a terminal, Enter: Standalone", facts: setupFactsFresh, interactive: true, input: "\n",
			want: want{kind: setupStandalone, role: server, answer: setupAsked},
			out:  []string{"How will you use Seamless?", "[1]", "Standalone", "Networked", "Enter 1 or 2 [1]"}},
		{name: "a terminal, end of input: Standalone", facts: setupFactsFresh, interactive: true, input: "",
			want: want{kind: setupStandalone, role: server, answer: setupAsked}},
		{name: "a terminal, a word: standalone", facts: setupFactsFresh, interactive: true, input: "standalone\n",
			want: want{kind: setupStandalone, role: server, answer: setupAsked}},
		{name: "a terminal, Networked then Server", facts: setupFactsFresh, interactive: true, input: "2\n1\n",
			want: want{kind: setupServer, role: server, answer: setupAsked},
			out:  []string{"Set up the Server first", "this computer keeps the data", "connects to the Server"}},
		{name: "a wrong answer is asked again", facts: setupFactsFresh, interactive: true, input: "3\nnetworked\n\nserver\n",
			want: want{kind: setupServer, role: server, answer: setupAsked},
			out:  []string{"please enter a number from 1 to 2"}},
		{name: "Networked has no default: end of input stops", facts: setupFactsFresh, interactive: true, input: "2\n\n",
			err: "no answer: set SEAMLESS_SETUP=standalone|server|client"},
		{name: "a terminal, Networked then Client", facts: setupFactsFresh, interactive: true,
			input: "2\n2\nbox.example:8081\nhttps://box.example:8081/\n\nsecret-key\n",
			want:  want{kind: setupClient, role: client, serverURL: "https://box.example:8081", answer: setupAsked},
			out:   []string{"run  seamlessd client-config  on the Server", "the address starts with https://", "wrote the client config"},
			noOut: []string{"secret-key"},
			wrote: []setupClientCall{{"https://box.example:8081", "secret-key"}}},
		{name: "a Client over plain http is warned", facts: setupFactsFresh, interactive: true, input: "2\n2\nhttp://10.0.0.5:8081\nk\n",
			want:  want{kind: setupClient, role: client, serverURL: "http://10.0.0.5:8081", answer: setupAsked},
			out:   []string{"plain http"},
			wrote: []setupClientCall{{"http://10.0.0.5:8081", "k"}}},
		{name: "a Client with no address stops", facts: setupFactsFresh, interactive: true, input: "2\n2\n",
			err: "no Server address"},
		{name: "Windows: Networked is the Client, never the Server", goos: "windows", facts: setupFactsFresh, interactive: true,
			input: "2\nhttps://box.example:8081\nk\n",
			want:  want{kind: setupClient, role: client, serverURL: "https://box.example:8081", answer: setupAsked},
			out:   []string{"Windows has no peer_auth and no relay", "as its Client"},
			noOut: []string{"Set up the Server first"},
			wrote: []setupClientCall{{"https://box.example:8081", "k"}}},

		// The environment's answers.
		{name: "SEAMLESS_SETUP=standalone", facts: setupFactsFresh, env: []string{"SEAMLESS_SETUP=standalone"},
			want: want{kind: setupStandalone, role: server, answer: setupFromEnv}},
		{name: "SEAMLESS_SETUP=Server: case and space aside", facts: setupFactsFresh, env: []string{"SEAMLESS_SETUP= Server "},
			want: want{kind: setupServer, role: server, answer: setupFromEnv}},
		{name: "SEAMLESS_SETUP=server, even at a terminal, asks nothing", facts: setupFactsFresh, interactive: true,
			env: []string{"SEAMLESS_SETUP=server"}, want: want{kind: setupServer, role: server, answer: setupFromEnv}},
		{name: "SEAMLESS_SETUP=client alone stops: no question for a script", facts: setupFactsFresh, interactive: true,
			env: []string{"SEAMLESS_SETUP=client"}, err: "SEAMLESS_SETUP=client needs the Server to connect to: set SEAMLESS_SERVER_URL and SEAMLESS_MCP_API_KEY"},
		{name: "SEAMLESS_SETUP=bogus", facts: setupFactsFresh, env: []string{"SEAMLESS_SETUP=bogus"},
			err: `invalid SEAMLESS_SETUP="bogus": valid values are standalone, server, client`},
		{name: "SEAMLESS_SETUP=server on Windows", goos: "windows", facts: setupFactsFresh, env: []string{"SEAMLESS_SETUP=server"},
			err: "Windows has no peer_auth and no relay"},
		{name: "SEAMLESS_SETUP=bogus on Windows names its two", goos: "windows", facts: setupFactsFresh, env: []string{"SEAMLESS_SETUP=x"},
			err: "valid values are standalone, client"},
		{name: "SEAMLESS_SERVER_URL and the key: a Client", facts: setupFactsFresh, interactive: true,
			env:  []string{"SEAMLESS_SERVER_URL=https://box.example:8081/", "SEAMLESS_MCP_API_KEY=k"},
			want: want{kind: setupClient, role: client, serverURL: "https://box.example:8081", answer: setupFromEnv}},
		{name: "SEAMLESS_SERVER_URL wins over what is installed, as it always did", facts: setupFactsStandalone,
			env:  []string{"SEAMLESS_SETUP=client", "SEAMLESS_SERVER_URL=https://box.example:8081", "SEAMLESS_MCP_API_KEY=k"},
			want: want{kind: setupClient, role: client, serverURL: "https://box.example:8081", answer: setupFromEnv}},
		{name: "SEAMLESS_SERVER_URL without the key", facts: setupFactsFresh, env: []string{"SEAMLESS_SERVER_URL=https://box.example:8081"},
			err: "SEAMLESS_SERVER_URL is set but SEAMLESS_MCP_API_KEY is not"},
		{name: "SEAMLESS_SERVER_URL loses one slash, as docs/install drops it", facts: setupFactsFresh,
			env:  []string{"SEAMLESS_SERVER_URL=https://box.example:8081//", "SEAMLESS_MCP_API_KEY=k"},
			want: want{kind: setupClient, role: client, serverURL: "https://box.example:8081/", answer: setupFromEnv}},
		{name: "SEAMLESS_SERVER_URL=/ is no server, as docs/install reads it", facts: setupFactsFresh,
			env: []string{"SEAMLESS_SERVER_URL=/", "SEAMLESS_MCP_API_KEY=k"}, want: want{kind: setupStandalone, role: server, answer: setupDefault}},
		{name: "on Windows every trailing slash goes, as install.ps1 drops them", goos: "windows", facts: setupFactsFresh,
			env: []string{"SEAMLESS_SETUP=client", "seamless_server_url=///", "SEAMLESS_MCP_API_KEY=k"},
			err: "SEAMLESS_SETUP=client needs the Server to connect to"},
		{name: "SEAMLESS_SETUP and SEAMLESS_SERVER_URL disagree", facts: setupFactsFresh,
			env: []string{"SEAMLESS_SETUP=server", "SEAMLESS_SERVER_URL=https://box.example:8081", "SEAMLESS_MCP_API_KEY=k"},
			err: "SEAMLESS_SETUP=server and SEAMLESS_SERVER_URL disagree"},
		// SEAMLESS_IMPORT: a first install's answer, and nobody else's.
		{name: "SEAMLESS_IMPORT alone: Standalone, answered by the environment", facts: setupFactsFresh, interactive: true,
			env: []string{"SEAMLESS_IMPORT=/b.tar.gz"}, want: want{kind: setupStandalone, role: server, answer: setupFromEnv}},
		{name: "SEAMLESS_IMPORT and SEAMLESS_SETUP=server: the Server", facts: setupFactsFresh,
			env: []string{"SEAMLESS_SETUP=server", "SEAMLESS_IMPORT=/b.tar.gz"}, want: want{kind: setupServer, role: server, answer: setupFromEnv}},
		{name: "SEAMLESS_IMPORT and SEAMLESS_IMPORT_LOCKS, case aside", facts: setupFactsFresh,
			env: []string{"SEAMLESS_IMPORT=/b.tar.gz", "SEAMLESS_IMPORT_LOCKS= Keep "}, want: want{kind: setupStandalone, role: server, answer: setupFromEnv}},
		{name: "SEAMLESS_IMPORT on Windows", goos: "windows", facts: setupFactsFresh,
			env: []string{`SEAMLESS_IMPORT=C:\Users\me\b.tar.gz`}, want: want{kind: setupStandalone, role: server, answer: setupFromEnv}},
		{name: "SEAMLESS_IMPORT for a Client by SEAMLESS_SERVER_URL is refused", facts: setupFactsFresh,
			env: []string{"SEAMLESS_SERVER_URL=https://box.example:8081", "SEAMLESS_MCP_API_KEY=k", "SEAMLESS_IMPORT=/b.tar.gz"},
			err: "SEAMLESS_IMPORT: a Client keeps no data of its own -- its data is its Server's -- so setup imports no backup on it"},
		{name: "SEAMLESS_IMPORT with SEAMLESS_SETUP=client is refused", facts: setupFactsFresh,
			env: []string{"SEAMLESS_SETUP=client", "SEAMLESS_IMPORT=/b.tar.gz"}, err: "a Client keeps no data of its own"},
		{name: "SEAMLESS_IMPORT on an installed Client is refused", facts: setupFactsClient,
			env: []string{"SEAMLESS_IMPORT=/b.tar.gz"}, err: "a Client keeps no data of its own"},
		{name: "SEAMLESS_IMPORT on a re-run is refused: the data is there", facts: setupFactsStandalone,
			env: []string{"SEAMLESS_IMPORT=/b.tar.gz"},
			err: "SEAMLESS_IMPORT: this computer is set up as Standalone already and has its data, and setup starts from a backup only on a first install"},
		{name: "SEAMLESS_IMPORT on a re-run names the restore instead", facts: setupFactsServer,
			env: []string{"SEAMLESS_SETUP=server", "SEAMLESS_IMPORT=/b.tar.gz"},
			err: "seamlessd restore /b.tar.gz (today's data is saved first), or seamlessd restore --merge /b.tar.gz"},
		{name: "SEAMLESS_IMPORT on peer_auth's Standalone is refused", facts: setupFactsLocked,
			env: []string{"SEAMLESS_IMPORT=/b.tar.gz"}, err: "set up as Standalone, with locked projects already"},
		{name: "SEAMLESS_IMPORT=- is refused", facts: setupFactsFresh, env: []string{"SEAMLESS_IMPORT=-"},
			err: "SEAMLESS_IMPORT=-: - would read the backup from stdin, which setup does not"},
		{name: "a relative SEAMLESS_IMPORT is refused", facts: setupFactsFresh, env: []string{"SEAMLESS_IMPORT=b.tar.gz"},
			err: "SEAMLESS_IMPORT=b.tar.gz: not a full path"},
		{name: "SEAMLESS_IMPORT_LOCKS alone is refused", facts: setupFactsFresh, env: []string{"SEAMLESS_IMPORT_LOCKS=keep"},
			err: "SEAMLESS_IMPORT_LOCKS=keep says what an import does with a backup's locked projects, and SEAMLESS_IMPORT names no backup"},
		{name: "SEAMLESS_IMPORT_LOCKS=maybe is refused", facts: setupFactsFresh, env: []string{"SEAMLESS_IMPORT=/b.tar.gz", "SEAMLESS_IMPORT_LOCKS=maybe"},
			err: `invalid SEAMLESS_IMPORT_LOCKS="maybe": valid values are keep, remove`},
		{name: "SEAMLESS_PAIR_CODE is not built", facts: setupFactsFresh,
			env: []string{"SEAMLESS_SETUP=client", "SEAMLESS_SERVER_URL=https://box.example:8081", "SEAMLESS_MCP_API_KEY=k", "SEAMLESS_PAIR_CODE=1234"},
			err: "SEAMLESS_PAIR_CODE: pairing with a one-time code is not built into setup yet"},

		// A re-run.
		{name: "a re-run keeps what is installed", facts: setupFactsStandalone,
			want: want{kind: setupStandalone, role: server, answer: setupKept}},
		{name: "a re-run at a terminal offers the changes; Enter keeps", facts: setupFactsStandalone, interactive: true, input: "\n",
			want: want{kind: setupStandalone, role: server, answer: setupKept},
			out:  []string{"This computer is set up as Standalone.", "[1] Keep it as it is", "Make it the Server", "Turn on locked projects"}},
		{name: "a re-run at a terminal: Standalone becomes the Server", facts: setupFactsStandalone, interactive: true, input: "2\n",
			want: want{kind: setupServer, role: server, answer: setupAsked, change: "server"}},
		{name: "a re-run at a terminal: locked projects", facts: setupFactsStandalone, interactive: true, input: "3\n",
			want: want{kind: setupStandalone, role: server, answer: setupAsked, change: "locked-projects"}},
		{name: "a re-run on Windows has nothing to offer", goos: "windows", facts: setupFactsStandalone, interactive: true,
			want: want{kind: setupStandalone, role: server, answer: setupKept}},
		{name: "SEAMLESS_SETUP=server makes the Standalone the Server", facts: setupFactsStandalone, env: []string{"SEAMLESS_SETUP=server"},
			want: want{kind: setupServer, role: server, answer: setupFromEnv, change: "server"}},
		{name: "SEAMLESS_SETUP=standalone keeps it", facts: setupFactsStandalone, env: []string{"SEAMLESS_SETUP=standalone"},
			want: want{kind: setupStandalone, role: server, answer: setupFromEnv}},
		{name: "SEAMLESS_SETUP=client on a Standalone is refused", facts: setupFactsStandalone, env: []string{"SEAMLESS_SETUP=client"},
			err: "this computer is set up as Standalone and keeps its own data; setup does not make it a Client"},
		{name: "a Server", facts: setupFactsServer, want: want{kind: setupServer, role: server, answer: setupKept}},
		{name: "SEAMLESS_SETUP=standalone on a Server is refused", facts: setupFactsServer, env: []string{"SEAMLESS_SETUP=standalone"},
			err: "setup does not make it Standalone; SEAMLESS_SETUP=server keeps it as it is"},
		{name: "a Client keeps its server", facts: setupFactsClient, interactive: true,
			want: want{kind: setupClient, role: client, serverURL: "https://box.example:8081", answer: setupKept}},
		{name: "SEAMLESS_SETUP=standalone on a Client is refused", facts: setupFactsClient, env: []string{"SEAMLESS_SETUP=standalone"},
			err: "setup does not change a Client into Standalone"},
		{name: "a client config with no server_url stops, as the installers do", facts: setupFacts{configFound: true, role: "client", configPath: "/h/c.yaml"},
			err: "/h/c.yaml says role: client but sets no server_url"},
		{name: "peer_auth's socket client is Standalone with locked projects, acted on as a client", facts: setupFactsLocked, interactive: true, input: "\n",
			want: want{kind: setupStandalone, role: client, serverURL: "unix:///opt/seamless/run/seamlessd.sock", answer: setupKept},
			out:  []string{"Standalone, with locked projects", "Make it the Server"}, noOut: []string{"Turn on locked projects"}},
		{name: "a config gone but the service there: Standalone", facts: setupFacts{userService: true},
			want: want{kind: setupStandalone, role: server, answer: setupKept}},
		{name: "a database there: Standalone", facts: setupFacts{localData: true},
			want: want{kind: setupStandalone, role: server, answer: setupKept}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			goos := tt.goos
			if goos == "" {
				goos = "linux"
			}
			r, out, wrote := setupTestRun(t, goos, tt.env, tt.facts, tt.interactive, tt.input)
			st, err := setupDecide(r)
			if tt.err != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.err)
				require.ErrorIs(t, err, errSetupRefused)
				require.Empty(t, *wrote, "a refused run writes nothing")
				return
			}
			require.NoError(t, err, out.String())
			require.Equal(t, tt.want, want{st.kind, st.role, st.serverURL, st.answer, st.change}, out.String())
			for _, s := range tt.out {
				require.Contains(t, out.String(), s)
			}
			for _, s := range tt.noOut {
				require.NotContains(t, out.String(), s)
			}
			require.Equal(t, tt.wrote, *wrote)
		})
	}
}

// TestSetupDecide_ScriptedRunsNeverRead: no terminal, or any SEAMLESS_* set,
// and setup reads nothing -- whatever is installed, whatever it is asked.
func TestSetupDecide_ScriptedRunsNeverRead(t *testing.T) {
	envs := [][]string{
		nil,
		{"SEAMLESS_SETUP=standalone"}, {"SEAMLESS_SETUP=server"}, {"SEAMLESS_SETUP=client"},
		{"SEAMLESS_VERSION=0.9.0", "SEAMLESS_INSTALL_DIR=/h/.local/bin"},
		{"SEAMLESS_SERVER_URL=https://box.example:8081", "SEAMLESS_MCP_API_KEY=k"},
		{"SEAMLESS_NO_SERVICE=1"}, {"SEAMLESS_CLIENT=claude"},
		{"SEAMLESS_IMPORT=/b.tar.gz"}, {"SEAMLESS_IMPORT=/b.tar.gz", "SEAMLESS_IMPORT_LOCKS=remove"},
		{"SEAMLESS_SETUP=server", "SEAMLESS_SERVER_NAME=a.example"}, {"SEAMLESS_SERVER_NAME=a.example"},
		{"SEAMLESS_SETUP=server", "SEAMLESS_SERVER_NAME=a.example", "SEAMLESS_TLS_CERT=/c.crt", "SEAMLESS_TLS_KEY=/c.key"},
	}
	for _, facts := range []setupFacts{setupFactsFresh, setupFactsStandalone, setupFactsServer, setupFactsClient, setupFactsLocked} {
		for _, env := range envs {
			for _, interactive := range []bool{false, true} {
				if interactive && len(env) == 0 {
					continue // a person at a terminal with nothing set is the one run that asks
				}
				r, _, wrote := setupTestRun(t, "linux", env, facts, interactive, "")
				if interactive {
					r.in = bufio.NewReader(setupNoRead{t})
				}
				r.secret = func() (string, error) { t.Fatal("setup asked for a key on a scripted run"); return "", nil }
				_, err := setupDecide(r)
				if err != nil {
					require.ErrorIs(t, err, errSetupRefused)
				}
				require.Empty(t, *wrote)
			}
		}
	}
}

// TestSetupDecide_UpdaterRunsAreScripted: every installer run the updater
// makes, attended or not, is a scripted run -- it always sets SEAMLESS_VERSION
// and SEAMLESS_INSTALL_DIR -- so setup asks it nothing; and an unattended one
// can never carry a setup knob, since it drops every SEAMLESS_* but the
// installerKnobs, which list none.
func TestSetupDecide_UpdaterRunsAreScripted(t *testing.T) {
	shape, err := installShape{dir: "/h/.local/bin", clients: []string{"claude"}}.knobs()
	require.NoError(t, err)
	knobs := mergeKnobs(shape, map[string]string{"SEAMLESS_VERSION": "0.9.0"})
	owner := []string{"PATH=/usr/bin", "HOME=/h", "SEAMLESS_SETUP=server", "SEAMLESS_IMPORT=/b", "SEAMLESS_IMPORT_LOCKS=remove", "SEAMLESS_PAIR_CODE=1",
		"SEAMLESS_SERVER_NAME=a.example", "SEAMLESS_TLS_CERT=/c.crt", "SEAMLESS_TLS_KEY=/c.key"}
	setupKnobs := []string{setupKnobKind, setupKnobImport, setupKnobImportLocks, setupKnobPairCode, setupKnobServerName, setupKnobTLSCert, setupKnobTLSKey}

	unattended, err := unattendedInstallerEnv("linux", owner, knobs)
	require.NoError(t, err)
	for _, kv := range unattended {
		for _, knob := range setupKnobs {
			require.False(t, strings.HasPrefix(kv, knob+"="), "an unattended update passes no setup knob: %s", kv)
		}
	}
	attended, err := attendedInstallerEnv("linux", []string{"PATH=/usr/bin", "HOME=/h"}, mergeKnobs(
		map[string]string{"SEAMLESS_INSTALL_DIR": "/h/.local/bin"}, releaseKnobs(releaseAssets{version: setupTestVersion(t, "0.9.0")})))
	require.NoError(t, err)
	for _, env := range [][]string{unattended, attended} {
		r, _, _ := setupTestRun(t, "linux", env, setupFactsStandalone, true, "")
		r.in = bufio.NewReader(setupNoRead{t})
		require.False(t, r.asks(), "an update's installer run is a scripted run: %v", env)
		st, err := setupDecide(r)
		require.NoError(t, err)
		require.Equal(t, setupKept, st.answer)
	}
	for _, knob := range setupKnobs {
		require.NotContains(t, installerKnobs, knob, "the updater must never be able to pass a setup knob")
	}

	// An attended update keeps what the owner exported, a SEAMLESS_IMPORT
	// included; on a computer already set up, setup refuses it before the
	// installer installs anything, as it refused it before the import step
	// was built.
	exported, err := attendedInstallerEnv("linux", []string{"PATH=/usr/bin", "HOME=/h", "SEAMLESS_IMPORT=/b.tar.gz"}, mergeKnobs(
		map[string]string{"SEAMLESS_INSTALL_DIR": "/h/.local/bin"}, releaseKnobs(releaseAssets{version: setupTestVersion(t, "0.9.0")})))
	require.NoError(t, err)
	require.Contains(t, exported, "SEAMLESS_IMPORT=/b.tar.gz")
	r, _, _ := setupTestRun(t, "linux", exported, setupFactsStandalone, true, "")
	r.in = bufio.NewReader(setupNoRead{t})
	_, err = setupDecide(r)
	require.ErrorIs(t, err, errSetupRefused)
	require.ErrorContains(t, err, "setup starts from a backup only on a first install")
}

// TestSetupWords_OnePlace renames every kind in setupWords and runs the
// questions, the summaries, the hand-offs, the refusals and the usage: no
// message may still spell a kind by its old name.
func TestSetupWords_OnePlace(t *testing.T) {
	saved, savedNet := maps.Clone(setupWords), setupNetworkedWord
	t.Cleanup(func() { setupWords, setupNetworkedWord = saved, savedNet })
	setupWords = map[setupKind]setupWord{
		setupStandalone: {env: "solo", name: "Solo", line: saved[setupStandalone].line},
		setupServer:     {env: "hub", name: "Hub", line: saved[setupServer].line},
		setupClient:     {env: "spoke", name: "Spoke", line: saved[setupClient].line},
	}
	setupNetworkedWord = setupWord{name: "Shared", line: savedNet.line}
	old := regexp.MustCompile(`\b(Standalone|Networked|Server|Client)\b|\b(standalone|server|client)\b[|,]`)

	var all strings.Builder
	run := func(goos string, env []string, facts setupFacts, interactive bool, input string) {
		r, out, _ := setupTestRun(t, goos, env, facts, interactive, input)
		st, err := setupDecide(r)
		if err != nil {
			all.WriteString(err.Error() + "\n")
		} else {
			setupSummary(r, st)
			require.NoError(t, setupHandOff(r, &st, setupStepsLeft(st, goos), true))
		}
		all.WriteString(out.String())
	}
	run("linux", nil, setupFactsFresh, true, "1\n")
	run("linux", nil, setupFactsFresh, true, "2\n1\n")
	run("linux", nil, setupFactsFresh, true, "2\n2\nhttps://b.example:8081\nk\n")
	run("windows", nil, setupFactsFresh, true, "2\nhttps://b.example:8081\nk\n")
	run("linux", nil, setupFactsStandalone, true, "2\n")
	run("linux", nil, setupFactsLocked, true, "\n")
	run("linux", []string{"SEAMLESS_SETUP=hub"}, setupFactsFresh, false, "")
	run("linux", []string{"SEAMLESS_SETUP=spoke"}, setupFactsFresh, false, "")
	run("linux", []string{"SEAMLESS_SETUP=server"}, setupFactsFresh, false, "")
	run("windows", []string{"SEAMLESS_SETUP=hub"}, setupFactsFresh, false, "")
	run("linux", []string{"SEAMLESS_SETUP=spoke"}, setupFactsStandalone, false, "")
	run("linux", []string{"SEAMLESS_SETUP=solo"}, setupFactsClient, false, "")
	run("linux", []string{"SEAMLESS_SETUP=solo"}, setupFactsServer, false, "")
	run("linux", []string{"SEAMLESS_SERVER_URL=https://b", "SEAMLESS_MCP_API_KEY=k", "SEAMLESS_SETUP=hub"}, setupFactsFresh, false, "")
	run("linux", []string{"SEAMLESS_IMPORT=/b"}, setupFactsFresh, false, "")
	run("linux", []string{"SEAMLESS_SERVER_URL=https://b", "SEAMLESS_MCP_API_KEY=k", "SEAMLESS_IMPORT=/b"}, setupFactsFresh, false, "")
	run("linux", []string{"SEAMLESS_IMPORT=/b"}, setupFactsStandalone, false, "")
	run("linux", []string{"SEAMLESS_IMPORT=/b"}, setupFactsLocked, false, "")
	run("linux", []string{"SEAMLESS_IMPORT_LOCKS=keep"}, setupFactsFresh, false, "")
	run("linux", []string{"SEAMLESS_PAIR_CODE=1"}, setupFactsFresh, false, "")
	run("linux", nil, setupFactsFresh, true, "2\n")
	// The address knobs' refusals, and the address step's own words: its
	// refusal, its question, its rows, the lines for a config that exists,
	// the root steps, and the closing.
	run("linux", []string{"SEAMLESS_SERVER_NAME=a.example"}, setupFactsFresh, false, "")
	run("linux", []string{"SEAMLESS_SERVER_URL=https://b", "SEAMLESS_MCP_API_KEY=k", "SEAMLESS_SERVER_NAME=a.example"}, setupFactsFresh, false, "")
	run("linux", []string{"SEAMLESS_SERVER_NAME=a.example"}, setupFactsServer, false, "")
	run("windows", []string{"SEAMLESS_SERVER_NAME=a.example"}, setupFactsFresh, false, "")
	if runtime.GOOS != "windows" {
		first := func(env []string, facts setupFacts, interactive bool, input string, ts tailscaleStatus, tsErr error) {
			r, out, _ := setupTestRun(t, runtime.GOOS, env, facts, interactive, input)
			setupAddrArrange(t, r, map[string][]string{setupAddrTSName: {setupAddrTailv4, setupAddrTailv6},
				"lan.example": {setupAddrLANv4}}, setupAddrIfaces, ts, tsErr)
			st, err := setupDecide(r)
			if err == nil {
				setupSummary(r, st)
				err = setupHandOff(r, &st, setupStepsLeft(st, runtime.GOOS), false)
			}
			if err == nil {
				err = setupWriteServer(r, &st)
			}
			if err != nil {
				all.WriteString(err.Error() + "\n")
			}
			all.WriteString(out.String())
		}
		first([]string{"SEAMLESS_SETUP=hub"}, setupFactsFresh, false, "", setupAddrTailnetUp, nil)
		first([]string{"SEAMLESS_SETUP=hub", "SEAMLESS_SERVER_NAME=" + setupAddrTSName}, setupFactsFresh, false, "", setupAddrTailnetUp, nil)
		first([]string{"SEAMLESS_SETUP=hub", "SEAMLESS_SERVER_NAME=" + setupAddrTSName}, setupFactsStandalone, false, "", setupAddrTailnetUp, nil)
		first([]string{"SEAMLESS_SETUP=hub", "SEAMLESS_SERVER_NAME=" + setupAddrTSName}, setupFactsLocked, false, "", setupAddrTailnetUp, nil)
		first([]string{"SEAMLESS_SETUP=hub", "SEAMLESS_SERVER_NAME=lan.example"}, setupFactsFresh, false, "", tailscaleStatus{}, errNoTailscale)
		first(nil, setupFactsFresh, true, "2\n1\n", tailscaleStatus{}, errNoTailscale)
		first(nil, setupFactsFresh, true, "2\n1\n\n", setupAddrTailnetUp, nil)
		for _, cfg := range []func() config.Config{
			config.Defaults,
			func() config.Config {
				c := config.Defaults()
				c.Role, c.AdvertisedURL = config.RoleClient, "unix:///opt/seamless/run/seamlessd.sock"
				return c
			},
		} {
			r, out, _ := setupTestRun(t, "linux", nil, setupFactsFresh, false, "")
			r.loadConfig = func() (config.Config, error) { return cfg(), nil }
			require.NoError(t, setupClientsStep(r, &setupState{}))
			all.WriteString(out.String())
		}
	}
	// The import step's question, asked in setup's first half.
	for _, goos := range []string{"linux", "windows"} {
		r, out, _ := setupTestRun(t, goos, nil, setupFactsFresh, true, "1\n\n")
		st, err := setupDecide(r)
		require.NoError(t, err)
		require.NoError(t, setupHandOff(r, &st, setupStepsLeft(st, goos), false))
		require.Contains(t, out.String(), "Start fresh, or from a backup?")
		all.WriteString(out.String())
	}
	setupUsage(&all, flagSetForSetupUsage(), "linux")

	text := all.String()
	require.Contains(t, text, "Solo")
	require.Contains(t, text, "Hub")
	require.Contains(t, text, "Spoke")
	require.Contains(t, text, "Shared")
	require.Contains(t, text, "SEAMLESS_SETUP=solo|hub|spoke")
	require.Contains(t, text, `invalid SEAMLESS_SETUP="server": valid values are solo, hub, spoke`)
	if m := old.FindAllString(text, -1); len(m) > 0 {
		t.Fatalf("a kind spelled outside setupWords: %q in\n%s", m, text)
	}
}

// flagSetForSetupUsage is setup's own flag set, for its usage.
func flagSetForSetupUsage() *flag.FlagSet {
	fs, _, _ := setupFlags(io.Discard, "linux")
	return fs
}

func TestSetupState_RoundTrip(t *testing.T) {
	for _, st := range []setupState{
		{kind: setupStandalone, role: config.RoleServer, installed: setupNone, answer: setupAsked},
		{kind: setupClient, role: config.RoleClient, serverURL: "https://box.example:8081", installed: setupNone, answer: setupFromEnv},
		{kind: setupServer, role: config.RoleServer, installed: setupStandalone, locked: true, answer: setupAsked, change: "server"},
		{kind: setupStandalone, role: config.RoleClient, serverURL: "unix:///opt/seamless/run/seamlessd.sock", installed: setupStandalone, locked: true, answer: setupKept},
	} {
		path := filepath.Join(t.TempDir(), "setup.state")
		require.NoError(t, writeSetupState(path, st))
		info, err := os.Stat(path)
		require.NoError(t, err)
		if runtime.GOOS != "windows" {
			require.Equal(t, fs.FileMode(0o600), info.Mode().Perm())
		}
		got, err := readSetupState(path)
		require.NoError(t, err)
		require.Equal(t, st, got)
	}
	_, err := marshalSetupState(setupState{kind: setupClient, role: config.RoleClient, serverURL: "https://a\nrole=server"})
	require.Error(t, err)
	for name, body := range map[string]string{
		"no header":    "role=server\nkind=standalone\n",
		"other header": "seamless-setup 2\nrole=server\nkind=standalone\n",
		"unknown key":  setupStateHeader + "\nrole=server\nkind=standalone\nkey=1\n",
		"bad role":     setupStateHeader + "\nrole=both\nkind=standalone\n",
		"bad kind":     setupStateHeader + "\nrole=server\nkind=solo\n",
		"no kind":      setupStateHeader + "\nrole=server\n",
		"bad answer":   setupStateHeader + "\nrole=server\nkind=standalone\nanswer=maybe\n",
	} {
		_, err := parseSetupState([]byte(body))
		require.Error(t, err, name)
	}
}

func TestSetupHandOff_SaysWhatSetupDoesNotDoYet(t *testing.T) {
	for _, tt := range []struct {
		name string
		goos string
		st   setupState
		want []string
		none bool
	}{
		{name: "a first Server: the address, the import and the closing are setup's own now", st: setupState{kind: setupServer, role: config.RoleServer, answer: setupAsked},
			want: []string{"by hand", "setup does not do this Server step yet:", "peer-auth/#move-an-existing-install-over",
				"This Server serves no other computer yet"}},
		{name: "a first Standalone: locked projects", st: setupState{kind: setupStandalone, role: config.RoleServer, answer: setupFromEnv},
			want: []string{"setup does not do this Standalone step yet:", "locked"}},
		{name: "a first Standalone on Windows: no locked projects there, and the import is built", goos: "windows",
			st: setupState{kind: setupStandalone, role: config.RoleServer, answer: setupAsked}, none: true},
		{name: "a Client", st: setupState{kind: setupClient, role: config.RoleClient, serverURL: "https://b", answer: setupFromEnv},
			want: []string{"pairing", "this Client uses its Server's key"}},
		{name: "a change to the Server: no import on a re-run",
			st:   setupState{kind: setupServer, role: config.RoleServer, installed: setupStandalone, answer: setupAsked, change: "server"},
			want: []string{"locked", "This Server serves no other computer yet"}},
		{name: "kept: nothing", st: setupState{kind: setupStandalone, role: config.RoleServer, installed: setupStandalone, answer: setupKept}, none: true},
		{name: "the default: nothing", st: setupState{kind: setupStandalone, role: config.RoleServer, answer: setupDefault}, none: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			goos := tt.goos
			if goos == "" {
				goos = "linux"
			}
			r, out, _ := setupTestRun(t, goos, nil, setupFactsFresh, false, "")
			require.NoError(t, setupHandOff(r, &tt.st, setupStepsLeft(tt.st, goos), true))
			if tt.none {
				require.Empty(t, out.String())
				return
			}
			for _, s := range tt.want {
				require.Contains(t, out.String(), s)
			}
			require.NotContains(t, out.String(), "seamlessd restore", "the import is not a step by hand")
			require.NotContains(t, out.String(), "start from a backup")
			require.NotContains(t, out.String(), "widen-the-server", "the address is not a step by hand")
			if goos == "windows" {
				require.NotContains(t, out.String(), "peer_auth")
			}
		})
	}

	// A built step runs in its phase, and is no longer listed as by hand.
	saved := setupSteps["address"]
	t.Cleanup(func() { setupSteps["address"] = saved })
	ran := 0
	built := saved
	built.run = func(*setupRun, *setupState) error { ran++; return nil }
	setupSteps["address"] = built
	r, out, _ := setupTestRun(t, "linux", nil, setupFactsFresh, false, "")
	st := setupState{kind: setupServer, role: config.RoleServer, answer: setupAsked}
	require.NoError(t, setupHandOff(r, &st, setupStepsLeft(st, "linux"), false))
	require.Equal(t, 1, ran)
	require.NoError(t, setupHandOff(r, &st, setupStepsLeft(st, "linux"), true))
	require.Equal(t, 1, ran, "a before step runs once")
	require.NotContains(t, out.String(), "widen-the-server")
	built.run = func(*setupRun, *setupState) error { return errors.New("no certificate") }
	setupSteps["address"] = built
	require.ErrorContains(t, setupHandOff(r, &st, setupStepsLeft(st, "linux"), false), "address: no certificate")
}

func TestSetupForInstallerAndFinish(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "setup.state")
	// The Server without its address is refused before anything is written,
	// naming the knob that answers it.
	r, out, _ := setupTestRun(t, "linux", []string{"SEAMLESS_SETUP=server"}, setupFactsFresh, false, "")
	err := setupForInstaller(r, path)
	require.ErrorIs(t, err, errSetupRefused)
	require.ErrorContains(t, err, "set SEAMLESS_SERVER_NAME")
	require.Contains(t, out.String(), "setup        Networked: Server (SEAMLESS_SETUP)")
	require.NoFileExists(t, path)

	if runtime.GOOS != "windows" {
		cert, key := setupAddrPair(t, setupAddrDomain)
		r, out, _ = setupTestRun(t, runtime.GOOS, []string{"SEAMLESS_SETUP=server", "SEAMLESS_SERVER_NAME=" + setupAddrDomain,
			"SEAMLESS_TLS_CERT=" + cert, "SEAMLESS_TLS_KEY=" + key}, setupFactsFresh, false, "")
		w := setupAddrArrange(t, r, map[string][]string{setupAddrDomain: {setupAddrLANv4}}, setupAddrIfaces, tailscaleStatus{}, errNoTailscale)
		require.NoError(t, setupForInstaller(r, path))
		require.Contains(t, out.String(), "setup        Networked: Server (SEAMLESS_SETUP)")
		require.NotContains(t, out.String(), "by hand", "the steps by hand come after the installer's own")
		require.Len(t, w.wrote, 1, "the first half writes the Server's config")
		st, err := readSetupState(path)
		require.NoError(t, err)
		require.Equal(t, config.RoleServer, st.role)

		// --finish: what is left by hand, and the closing, read from the
		// config the installer's own steps ran with.
		r, out, _ = setupTestRun(t, runtime.GOOS, nil, setupFactsFresh, false, "")
		r.loadConfig = func() (config.Config, error) {
			cfg := config.Defaults()
			cfg.Addr, cfg.AdvertisedURL, cfg.TLS.CertFile, cfg.TLS.KeyFile = w.wrote[0].Addr, w.wrote[0].ServerURL, cert, key
			return cfg, nil
		}
		require.NoError(t, setupFinish(r, path))
		require.Contains(t, out.String(), "setup does not do this Server step yet:")
		require.Contains(t, out.String(), "To connect another computer to this server:")
		require.Contains(t, out.String(), "Address:                 https://"+setupAddrDomain+":8081")
	}

	// A refusal writes no state.
	refused := filepath.Join(dir, "refused.state")
	r, _, _ = setupTestRun(t, "linux", []string{"SEAMLESS_SETUP=client"}, setupFactsFresh, false, "")
	require.ErrorIs(t, setupForInstaller(r, refused), errSetupRefused)
	require.NoFileExists(t, refused)
}

func TestSetupDirect(t *testing.T) {
	r, out, _ := setupTestRun(t, "linux", nil, setupFactsFresh, true, "")
	require.NoError(t, setupDirect(r))
	require.Contains(t, out.String(), "nothing is set up on this computer yet")
	require.Contains(t, out.String(), "curl -fsSL https://thereisnospoon.org/install | sh")

	r, out, _ = setupTestRun(t, "windows", nil, setupFactsFresh, true, "")
	require.NoError(t, setupDirect(r))
	require.Contains(t, out.String(), "irm https://thereisnospoon.org/install.ps1 | iex")

	r, _, _ = setupTestRun(t, "linux", []string{"SEAMLESS_SETUP=bogus"}, setupFactsFresh, false, "")
	require.ErrorIs(t, setupDirect(r), errSetupRefused)

	// The import is the installer's, on a first install; run by itself, setup
	// refuses what it could never do and imports nothing.
	r, out, _ = setupTestRun(t, "linux", []string{"SEAMLESS_IMPORT=/b.tar.gz"}, setupFactsFresh, false, "")
	require.NoError(t, setupDirect(r))
	require.Contains(t, out.String(), "nothing is set up on this computer yet")
	for _, tt := range []struct {
		env   []string
		facts setupFacts
		want  string
	}{
		{[]string{"SEAMLESS_IMPORT_LOCKS=keep"}, setupFactsFresh, "SEAMLESS_IMPORT names no backup"},
		{[]string{"SEAMLESS_IMPORT=b.tar.gz"}, setupFactsFresh, "not a full path"},
		{[]string{"SEAMLESS_IMPORT=/b.tar.gz"}, setupFactsStandalone, "setup starts from a backup only on a first install"},
		{[]string{"SEAMLESS_IMPORT=/b.tar.gz"}, setupFactsClient, "keeps no data of its own"},
	} {
		r, _, _ = setupTestRun(t, "linux", tt.env, tt.facts, false, "")
		err := setupDirect(r)
		require.ErrorIs(t, err, errSetupRefused, "%v", tt.env)
		require.ErrorContains(t, err, tt.want)
	}

	// Installed, no terminal: the summary, and where the changes are.
	r, out, _ = setupTestRun(t, "linux", nil, setupFactsStandalone, false, "")
	require.NoError(t, setupDirect(r))
	require.Contains(t, out.String(), "Standalone, as installed")
	require.Contains(t, out.String(), "to change it, run seamlessd setup at a terminal")

	r, out, _ = setupTestRun(t, "linux", []string{"SEAMLESS_DATA_DIR=/d"}, setupFactsStandalone, true, "")
	r.in = bufio.NewReader(setupNoRead{t})
	require.NoError(t, setupDirect(r))
	require.Contains(t, out.String(), "with no SEAMLESS_* set (SEAMLESS_DATA_DIR is)")

	// At a terminal: the change, handed off at once -- the address step's
	// question first, whose end of input names the knobs that answer it.
	r, out, _ = setupTestRun(t, "linux", nil, setupFactsStandalone, true, "2\n")
	err := setupDirect(r)
	require.ErrorIs(t, err, errSetupRefused)
	require.ErrorContains(t, err, "SEAMLESS_SERVER_NAME")
	require.Contains(t, out.String(), "Standalone, becoming the Server")
	require.Contains(t, out.String(), "Where will the other computers reach this one?")
}

func TestSetupUsage_EndsWithTheProtocolLine(t *testing.T) {
	var w strings.Builder
	setupUsage(&w, flagSetForSetupUsage(), "linux")
	lines := strings.Split(strings.TrimRight(w.String(), "\n"), "\n")
	require.Equal(t, setupProtocolLine, lines[len(lines)-1])
	require.Contains(t, w.String(), "SEAMLESS_SETUP=standalone|server|client")
	require.Contains(t, w.String(), "SEAMLESS_SERVER_NAME")
	require.Contains(t, w.String(), "SEAMLESS_TLS_CERT, SEAMLESS_TLS_KEY")

	var win strings.Builder
	setupUsage(&win, flagSetForSetupUsage(), "windows")
	require.Contains(t, win.String(), "SEAMLESS_SETUP=standalone|client")
}

// TestSetupProtocol_TheInstallersSpeakIt holds both installers to the
// hand-off: the probe line, the state's header, the two lines they read, and
// the setup knobs they look for.
func TestSetupProtocol_TheInstallersSpeakIt(t *testing.T) {
	sh, err := os.ReadFile(filepath.Join("..", "..", "docs", "install"))
	require.NoError(t, err)
	ps, err := os.ReadFile(filepath.Join("..", "..", "docs", "install.ps1"))
	require.NoError(t, err)
	require.Contains(t, string(sh), "SETUP_PROTOCOL='"+setupProtocolLine+"'\n")
	require.Contains(t, string(sh), "SETUP_HEADER='"+setupStateHeader+"'\n")
	require.Contains(t, string(sh), "role=*) rs_role=${rs_line#role=} ;;")
	require.Contains(t, string(sh), "server_url=*) rs_url=${rs_line#server_url=} ;;")
	require.Contains(t, string(ps), "$SetupProtocol = '"+setupProtocolLine+"'\n")
	require.Contains(t, string(ps), "$SetupHeader = '"+setupStateHeader+"'\n")
	require.Contains(t, string(ps), "$line.StartsWith('role='")
	require.Contains(t, string(ps), "$line.StartsWith('server_url='")
	for _, knob := range []string{setupKnobKind, setupKnobPairCode, setupKnobImport, setupKnobImportLocks,
		setupKnobServerName, setupKnobTLSCert, setupKnobTLSKey} {
		require.Contains(t, string(sh), "${"+knob+":-}", "docs/install looks for %s", knob)
		// \b: $env:SEAMLESS_IMPORT must not pass on $env:SEAMLESS_IMPORT_LOCKS alone.
		require.Regexp(t, `\$env:`+knob+`\b`, string(ps), "docs/install.ps1 looks for %s", knob)
	}
	// Neither installer spells a kind: setup's words are setup's.
	for _, script := range [][]byte{sh, ps} {
		for _, k := range []setupKind{setupStandalone, setupServer, setupClient} {
			require.NotContains(t, string(script), "SEAMLESS_SETUP="+setupWords[k].env)
		}
		require.NotContains(t, string(script), setupWords[setupStandalone].name)
		require.NotContains(t, string(script), setupNetworkedWord.name)
	}
	for _, fn := range []string{"setup_knobs", "scripted_run", "run_setup", "finish_setup"} {
		require.True(t, slices.ContainsFunc(strings.Split(string(sh), "\n"), func(l string) bool { return strings.HasPrefix(l, fn+"() {") }), fn)
	}
}
