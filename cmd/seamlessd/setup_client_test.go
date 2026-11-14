package main

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/devices"
)

// clientFP is the fingerprint the arranged server presents.
const clientFP = "AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89"

// clientWorld is what an arranged Client run did: the pairings it asked for,
// the files it trusted the server by, and the closing checks it ran.
type clientWorld struct {
	pairs  []devices.PairRequest
	urls   []string
	cas    []string
	checks []string
	pairFn func(devices.PairRequest) (devices.Paired, error)
}

// clientArrange stands a server up for r's Client steps: it presents cert,
// pairs every key device it is asked to (as the server would without
// peer_auth), and the closing check passes.
func clientArrange(t *testing.T, r *setupRun, cert clientServerCert) *clientWorld {
	t.Helper()
	w := &clientWorld{}
	r.client.serverCert = func(context.Context, string) (clientServerCert, error) { return cert, nil }
	r.client.pair = func(_ context.Context, url, _ string, req devices.PairRequest) (devices.Paired, error) {
		w.pairs, w.urls = append(w.pairs, req), append(w.urls, url)
		if w.pairFn != nil {
			return w.pairFn(req)
		}
		return devices.Paired{DeviceID: "01EXAMPLEDEVICE", Kind: devices.KindKey, Name: req.Name, Key: "seamless_dk_example"}, nil
	}
	r.client.writeFile = func(path string, data []byte) error {
		require.Contains(t, string(data), "BEGIN CERTIFICATE")
		w.cas = append(w.cas, path)
		return nil
	}
	r.client.check = func(_ context.Context, out io.Writer, cfgPath string) error {
		w.checks = append(w.checks, cfgPath)
		_, err := io.WriteString(out, "  [ok  ] server: ok\n")
		return err
	}
	return w
}

// clientTrusted is a certificate this computer's roots vouch for.
var clientTrusted = clientServerCert{tls: true, fingerprint: clientFP, pem: []byte("-----BEGIN CERTIFICATE-----\n"), trusted: true}

// clientFirstHalf is the installer's first call: the decision and the steps
// before the installer's own. It returns the state the installer reads.
func clientFirstHalf(t *testing.T, r *setupRun) (setupState, error) {
	t.Helper()
	st, err := setupDecide(r)
	if err != nil {
		return st, err
	}
	setupSummary(r, st)
	if err := setupHandOff(r, &st, setupStepsLeft(st, r.goos), false); err != nil {
		return st, err
	}
	// The state file carries it to --finish.
	raw, err := marshalSetupState(st)
	require.NoError(t, err)
	back, err := parseSetupState(raw)
	require.NoError(t, err)
	return back, nil
}

// The one-liner a server prints for macOS and Linux, run with no one to ask:
// the server's certificate shown, the owner's config pointed at the relay's
// socket with no key, and -- after the installer's own steps -- the relay's
// root step printed, never run: the account, a group of the owner's for the
// socket, root's copy of seamlessd checked against the binary setup ran as,
// the pin, the pairing as the relay's account with the code pasted, the
// service, the server's mapping, and where approvals happen.
func TestSetupClient_RelayUnattended(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			env := []string{"SEAMLESS_SETUP=client", "SEAMLESS_SERVER_URL=https://box.example:8081", "SEAMLESS_PAIR_CODE=ABCD-EFGH-JKMN-PQRS"}
			r, out, wrote := setupTestRun(t, goos, env, setupFactsFresh, false, "")
			w := clientArrange(t, r, clientTrusted)
			st, err := clientFirstHalf(t, r)
			require.NoError(t, err, out.String())
			require.Equal(t, config.RoleClient, st.role)
			require.Equal(t, config.RelaySocketURL, st.serverURL, "the installer wires the hooks to the relay's socket")
			require.Equal(t, setupDeviceRelay, st.device)
			require.Equal(t, "https://box.example:8081", st.pairServer)
			require.Len(t, st.binarySHA, 64)
			require.Equal(t, []setupClientCall{{config.RelaySocketURL, ""}}, *wrote, "the socket config, with no key")
			require.Empty(t, w.pairs, "the relay pairs as its own account, in the root step")
			require.Contains(t, out.String(), "fingerprint  "+clientFP)
			require.Contains(t, out.String(), "set SEAMLESS_SERVER_FINGERPRINT to have setup compare it")

			out.Reset()
			require.NoError(t, setupHandOff(r, &st, setupStepsLeft(st, goos), true))
			text := out.String()
			for _, want := range []string{
				"its one root step. Setup runs nothing as root",
				"sudo install -o root -g ",
				"/opt/seamless/bin/seamlessd' | ",
				"sudo -u _seamless /opt/seamless/bin/seamlessd relay pair --server https://box.example:8081 --fingerprint " + clientFP + " --code -",
				`echo "_seamless $(id -u _seamless)" | sudo tee /opt/seamless/etc/daemon-account`,
				"code         ABCD-EFGH-JKMN-PQRS",
				"it never rides in a command line",
				"peer_auth.relays: {here: {owner_uid: 501}}",
				"seamlessd approve runs on the Server alone",
				"once the relay serves: seam doctor",
			} {
				require.Contains(t, text, want)
			}
			for _, line := range strings.Split(text, "\n") {
				if strings.Contains(line, "sudo") || strings.Contains(line, "relay pair") {
					require.NotContains(t, line, "ABCD-EFGH-JKMN-PQRS", "the code never rides in a command: %s", line)
				}
			}
			if goos == "darwin" {
				require.Contains(t, text, "sudo dscl . -create /Users/_seamless UniqueID 480")
				require.Contains(t, text, "sudo dscl . -append /Groups/_seamlessowner GroupMembership example-owner _seamless")
				require.Contains(t, text, "sudo launchctl bootstrap system /Library/LaunchDaemons/com.arctop.seamless.relay.plist")
				require.Contains(t, text, "shasum -a 256 -c")
				require.Contains(t, text, "<string>--socket-mode</string>\n                     <string>0660</string>")
			} else {
				require.Contains(t, text, "sudo useradd --system --user-group --shell /usr/sbin/nologin --home-dir /opt/seamless/home _seamless")
				require.Contains(t, text, "sudo usermod -aG _seamlessowner example-owner")
				require.Contains(t, text, "sudo systemctl enable --now seamless-relay")
				require.Contains(t, text, "sha256sum -c")
				require.Contains(t, text, "relay serve --socket-mode 0660 --socket-group _seamlessowner")
				require.Contains(t, text, "log out and back in once")
			}
			require.Empty(t, w.checks, "a relay not yet serving is checked once its root step ran")
		})
	}
}

// A key device -- Windows always, macOS and Linux by SEAMLESS_PAIR_AS=key --
// pairs with the code in setup's first half, and its own key goes into the
// config the installer's hooks then read: never the server's key. The
// closing check runs once the installer's steps are done.
func TestSetupClient_KeyDeviceUnattended(t *testing.T) {
	for _, tt := range []struct {
		goos string
		env  []string
	}{
		{"windows", nil},
		{"linux", []string{"SEAMLESS_PAIR_AS=key"}},
		{"darwin", []string{"SEAMLESS_PAIR_AS=key", "SEAMLESS_SERVER_FINGERPRINT=" + strings.ToLower(clientFP)}},
	} {
		t.Run(tt.goos, func(t *testing.T) {
			env := append([]string{"SEAMLESS_SETUP=client", "SEAMLESS_SERVER_URL=https://box.example:8081", "SEAMLESS_PAIR_CODE=ABCD-EFGH"}, tt.env...)
			r, out, wrote := setupTestRun(t, tt.goos, env, setupFactsFresh, false, "")
			w := clientArrange(t, r, clientTrusted)
			st, err := clientFirstHalf(t, r)
			require.NoError(t, err, out.String())
			require.Equal(t, "https://box.example:8081", st.serverURL)
			require.Equal(t, setupDeviceKey, st.device)
			require.Len(t, w.pairs, 1)
			require.Equal(t, "ABCD-EFGH", w.pairs[0].Code)
			require.Equal(t, "here", w.pairs[0].Name, "the device is named for this computer")
			require.Equal(t, clientFP, w.pairs[0].ServerFingerprint, "the code goes only to the certificate shown")
			require.Nil(t, w.pairs[0].Relay)
			require.Equal(t, []setupClientCall{{"https://box.example:8081", "seamless_dk_example"}}, *wrote)
			require.Contains(t, out.String(), "a key device never opens a locked project")
			require.NotContains(t, out.String(), "seamless_dk_example", "the key is written, never printed")

			out.Reset()
			require.NoError(t, setupHandOff(r, &st, setupStepsLeft(st, tt.goos), true))
			require.Equal(t, []string{"/h/.config/seamless/seamless.yaml"}, w.checks)
			require.Contains(t, out.String(), "seam doctor, and a session the Server records")
			require.NotContains(t, out.String(), "sudo")
		})
	}
}

// At a terminal the person gives the address, compares the fingerprint, says
// what this computer pairs as and types the code: a key device's in the first
// half, a relay's in the root step's.
func TestSetupClient_AtTheTerminal(t *testing.T) {
	r, out, wrote := setupTestRun(t, "linux", nil, setupFactsFresh, true, "2\n2\nhttps://box.example:8081\ny\n2\nABCD-EFGH\n")
	w := clientArrange(t, r, clientTrusted)
	st, err := clientFirstHalf(t, r)
	require.NoError(t, err, out.String())
	require.Equal(t, setupDeviceKey, st.device)
	require.Len(t, w.pairs, 1)
	require.Equal(t, "ABCD-EFGH", w.pairs[0].Code)
	require.Len(t, *wrote, 1)
	for _, want := range []string{"Does it match what the Server printed? [y/N]", "How will this computer reach the Server?", "Pairing code: "} {
		require.Contains(t, out.String(), want)
	}

	r, out, wrote = setupTestRun(t, "darwin", nil, setupFactsFresh, true, "2\n2\nhttps://box.example:8081\nyes\n\n")
	w = clientArrange(t, r, clientTrusted)
	st, err = clientFirstHalf(t, r)
	require.NoError(t, err, out.String())
	require.Equal(t, setupDeviceRelay, st.device, "Enter takes the relay")
	require.Empty(t, w.pairs)
	require.Equal(t, []setupClientCall{{config.RelaySocketURL, ""}}, *wrote)
	r2, out2, _ := setupTestRun(t, "darwin", nil, setupFactsFresh, true, "WXYZ-1234\n")
	r2.client.accounts = func() clientAccounts { return clientAccounts{accountExists: true, groupExists: true, pinned: true} }
	require.NoError(t, setupHandOff(r2, &st, setupStepsLeft(st, "darwin"), true))
	require.Contains(t, out2.String(), "Pairing code: ")
	require.Contains(t, out2.String(), "code         WXYZ-1234")
	require.NotContains(t, out2.String(), "dscl . -create /Users/_seamless", "an account that is there is not made again")
	require.NotContains(t, out2.String(), "/opt/seamless/etc/daemon-account", "nor a pin that names it")
}

// A fingerprint that is not the one compared sends nothing: no code, no
// config, at the terminal or by SEAMLESS_SERVER_FINGERPRINT.
func TestSetupClient_FingerprintMismatchSendsNothing(t *testing.T) {
	other := strings.Replace(clientFP, "AB", "00", 1)
	r, out, wrote := setupTestRun(t, "linux", []string{"SEAMLESS_SERVER_URL=https://box.example:8081", "SEAMLESS_PAIR_CODE=ABCD",
		"SEAMLESS_SERVER_FINGERPRINT=" + other}, setupFactsFresh, false, "")
	w := clientArrange(t, r, clientTrusted)
	_, err := clientFirstHalf(t, r)
	require.ErrorIs(t, err, errSetupRefused)
	require.ErrorContains(t, err, "nothing was sent to it")
	require.Empty(t, w.pairs)
	require.Empty(t, *wrote, out.String())

	r, _, wrote = setupTestRun(t, "linux", nil, setupFactsFresh, true, "2\n2\nhttps://box.example:8081\n\n")
	w = clientArrange(t, r, clientTrusted)
	_, err = clientFirstHalf(t, r)
	require.ErrorIs(t, err, errSetupRefused, "Enter is no")
	require.ErrorContains(t, err, "the fingerprints differ")
	require.Empty(t, w.pairs)
	require.Empty(t, *wrote)
}

// A certificate this computer's roots do not vouch for (a LAN server's own)
// is trusted only once compared, and then as that certificate alone: its copy
// is what the key device's config and the relay's pairing trust.
func TestSetupClient_UntrustedCertificateNeedsTheComparison(t *testing.T) {
	untrusted := clientTrusted
	untrusted.trusted, untrusted.why = false, "x509: certificate signed by unknown authority"
	base := []string{"SEAMLESS_SERVER_URL=https://box.example:8081", "SEAMLESS_PAIR_CODE=ABCD", "SEAMLESS_PAIR_AS=key"}

	r, _, wrote := setupTestRun(t, "linux", base, setupFactsFresh, false, "")
	w := clientArrange(t, r, untrusted)
	_, err := clientFirstHalf(t, r)
	require.ErrorIs(t, err, errSetupRefused)
	require.ErrorContains(t, err, "set SEAMLESS_SERVER_FINGERPRINT="+clientFP)
	require.Empty(t, w.pairs)
	require.Empty(t, *wrote)

	r, out, _ := setupTestRun(t, "linux", append(base, "SEAMLESS_SERVER_FINGERPRINT="+clientFP), setupFactsFresh, false, "")
	w = clientArrange(t, r, untrusted)
	var ca string
	r.client.ensureDevice = func(_, _, caFile string) (string, error) {
		ca = caFile
		return "/h/.config/seamless/seamless.yaml", nil
	}
	st, err := clientFirstHalf(t, r)
	require.NoError(t, err, out.String())
	wantCA := filepath.Join("/h", ".config", "seamless", "server-ca.crt")
	require.Equal(t, []string{wantCA}, w.cas)
	require.Equal(t, wantCA, ca)
	require.Equal(t, ca, st.caFile)
	require.Contains(t, out.String(), "trust        this certificate alone")
}

// Plain http pairs only on loopback: to another computer the code and the key
// would cross the network in the clear.
func TestSetupClient_PlainHTTPIsRefusedOffLoopback(t *testing.T) {
	plain := clientServerCert{}
	r, _, wrote := setupTestRun(t, "linux", []string{"SEAMLESS_SERVER_URL=http://box.example:8081", "SEAMLESS_PAIR_CODE=ABCD"}, setupFactsFresh, false, "")
	w := clientArrange(t, r, plain)
	_, err := clientFirstHalf(t, r)
	require.ErrorIs(t, err, errSetupRefused)
	require.ErrorContains(t, err, "is plain http, and a pairing crosses the network")
	require.Empty(t, w.pairs)
	require.Empty(t, *wrote)

	r, out, _ := setupTestRun(t, "linux", []string{"SEAMLESS_SERVER_URL=http://127.0.0.1:8081", "SEAMLESS_PAIR_CODE=ABCD"}, setupFactsFresh, false, "")
	w = clientArrange(t, r, plain)
	st, err := clientFirstHalf(t, r)
	require.NoError(t, err, out.String())
	require.Equal(t, setupDeviceKey, st.device, "a relay needs TLS: loopback http pairs a key device")
	require.Len(t, w.pairs, 1)
}

// A key device's key goes into a config of its own: with a config there,
// nothing is paired, since a key no file could take would be a key lost.
// A pairing that the server refuses writes nothing either.
func TestSetupClient_NothingIsPairedThatCannotBeKept(t *testing.T) {
	env := []string{"SEAMLESS_SERVER_URL=https://box.example:8081", "SEAMLESS_PAIR_CODE=ABCD", "SEAMLESS_PAIR_AS=key"}
	r, _, wrote := setupTestRun(t, "linux", env, setupFactsFresh, false, "")
	w := clientArrange(t, r, clientTrusted)
	r.client.configTarget = func() (string, string, error) {
		return "/h/.config/seamless/seamless.yaml", "/h/.config/seamless/seamless.yaml", nil
	}
	_, err := clientFirstHalf(t, r)
	require.ErrorIs(t, err, errSetupRefused)
	require.ErrorContains(t, err, "is here already, and setup never edits a config")
	require.Empty(t, w.pairs)
	require.Empty(t, *wrote)

	r, _, wrote = setupTestRun(t, "linux", env, setupFactsFresh, false, "")
	w = clientArrange(t, r, clientTrusted)
	w.pairFn = func(devices.PairRequest) (devices.Paired, error) { return devices.Paired{}, devices.ErrCodeRefused }
	_, err = clientFirstHalf(t, r)
	require.ErrorIs(t, err, errSetupRefused)
	require.ErrorContains(t, err, "the pairing did not finish")
	require.Empty(t, *wrote)

	r, _, _ = setupTestRun(t, "linux", env, setupFactsFresh, false, "")
	clientArrange(t, r, clientTrusted)
	r.client.ensureDevice = func(string, string, string) (string, error) { return "", errors.New("disk full") }
	_, err = clientFirstHalf(t, r)
	require.ErrorContains(t, err, "revoke it on the server's Devices page and pair again")
}

// A Client of its server's own key (SEAMLESS_SERVER_URL with
// SEAMLESS_MCP_API_KEY) pairs nothing and is checked by nothing new: it runs
// as it always did.
func TestSetupClient_TheServersKeyPairsNothing(t *testing.T) {
	r, out, wrote := setupTestRun(t, "linux", []string{"SEAMLESS_SETUP=client", "SEAMLESS_SERVER_URL=https://box.example:8081",
		"SEAMLESS_MCP_API_KEY=k"}, setupFactsFresh, false, "")
	st, err := clientFirstHalf(t, r)
	require.NoError(t, err, out.String())
	require.False(t, st.pairing)
	require.NoError(t, setupHandOff(r, &st, setupStepsLeft(st, "linux"), true))
	require.Empty(t, *wrote)
	require.NotContains(t, out.String(), "by hand")
}

// The root step's commands, as pure text: an existing account and group are
// not made again, a pin already there is not rewritten, a server certificate
// copy is installed root's and handed to the pairing, and the code never
// appears.
func TestClientRelayRootStep(t *testing.T) {
	p := clientRelayPlan{goos: "darwin", owner: "example-owner", server: "https://box.example:8081", fingerprint: clientFP,
		caFile: "/h/.config/seamless/server-ca.crt", source: "/h/.local/bin/seamlessd", sha256: strings.Repeat("a", 64), uid: 480, gid: 479}
	text := clientRootText(clientRelayRootStep(p))
	require.Contains(t, text, "sudo install -o root -g wheel -m 0644 /h/.config/seamless/server-ca.crt "+clientRelayCAFile)
	require.Contains(t, text, "--ca-file "+clientRelayCAFile+" --code -")
	require.Contains(t, text, "sudo dscl . -create /Groups/_seamlessowner PrimaryGroupID 479")
	require.Contains(t, text, "<key>UserName</key>          <string>_seamless</string>")
	require.Contains(t, text, "<string>--socket-group</string>")

	p.goos, p.accountExists, p.groupExists, p.pinned = "linux", true, true, true
	text = clientRootText(clientRelayRootStep(p))
	require.NotContains(t, text, "useradd")
	require.NotContains(t, text, "groupadd")
	require.NotContains(t, text, "daemon-account")
	require.Contains(t, text, "sudo usermod -aG _seamlessowner _seamless")
	require.Contains(t, text, "sudo install -d -o root -g root -m 0755 /opt/seamless /opt/seamless/bin /opt/seamless/etc")
	require.Contains(t, text, "ExecStart=/opt/seamless/bin/seamlessd relay serve --socket-mode 0660 --socket-group _seamlessowner")
	require.Contains(t, text, "UMask=0077")
}

func clientRootText(blocks [][]string) string {
	var b strings.Builder
	for _, blk := range blocks {
		b.WriteString(strings.Join(blk, "\n") + "\n")
	}
	return b.String()
}

// The macOS ids are free as user and group ids alike, searched down from the
// guide's 480.
func TestClientRelayFreeIDs(t *testing.T) {
	uid, gid := clientRelayFreeIDs(func(id int) bool { return id == 480 || id == 478 })
	require.Equal(t, 479, uid)
	require.Equal(t, 477, gid)
}
