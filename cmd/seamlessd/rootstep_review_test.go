package main

// The second security review of client setup (wave nt1113f, REVIEW2): what
// setup prints for root, and the values it prints, hold even when a process of
// the owner's rewrote every file the owner can write. Written against what
// setup does (the flows, not their insides), so each test also states the
// finding it closes.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/devices"
)

// rootstepHostile is server addresses Go's URL parser takes, whose host is
// no host name: the shell would read each one's metacharacters.
var rootstepHostile = []string{
	"https://a;id;b:8081", "https://a$(id):8081", "https://a&id&b:8081", "https://a'b:8081",
	"https://a(b):8081", "https://a*b:8081", "https://a!b:8081", "https://a`id`:8081",
	"https://a|id:8081", "https://a b:8081", "https://[fe80::1%25en0]:8081", "https://owner@box.example:8081",
	"https://box.example:0", "https://box.example:65536", "https://-box.example:8081",
}

// L1: setup refuses a Server address whose host is neither a host name nor an
// IP address -- from the environment, at the terminal, and in its own check --
// and takes every name the Server's address step takes.
func TestRootStepReview_TheServerAddressIsAHostNameOrAnIPAddress(t *testing.T) {
	for _, raw := range rootstepHostile {
		require.NotEmpty(t, setupServerURLProblem(raw), raw)
		r, _, wrote := setupTestRun(t, "linux", []string{"SEAMLESS_SERVER_URL=" + raw, "SEAMLESS_PAIR_CODE=ABCD"}, setupFactsFresh, false, "")
		_, err := setupDecide(r)
		require.ErrorIs(t, err, errSetupRefused, raw)
		require.Empty(t, *wrote, raw)
	}
	for _, raw := range []string{"https://box.example:8081", "https://Box.Example.:8081", "https://box-1.example", "https://192.0.2.10:8081",
		"https://[2001:db8::1]:8081", "http://127.0.0.1:8081", "https://localhost:8081", "https://xn--bcher-kva.example:8081"} {
		require.Empty(t, setupServerURLProblem(raw), raw)
	}
	// Every name the Server's address step takes, a Client takes.
	for _, name := range []string{"box.example", "seamless.example.com", "studio-1.tail1234.ts.net", "a.b"} {
		require.Empty(t, setupServerNameProblem(name), name)
		require.Empty(t, setupServerURLProblem("https://"+name+":8081"), name)
	}

	r, out, _ := setupTestRun(t, "linux", nil, setupFactsFresh, true, "2\n2\nhttps://a;id;b:8081\nhttps://box.example:8081\n")
	st, err := setupDecide(r)
	require.NoError(t, err, out.String())
	require.Equal(t, "https://box.example:8081", st.serverURL, "the terminal asks again")
	require.Contains(t, out.String(), "not a host name or an IP address")
}

// L1: every value the state file hands from setup's first half to --finish is
// checked against its format again, since the relay's root step prints them.
func TestRootStepReview_TheStateFileIsCheckedAgain(t *testing.T) {
	const relay = setupStateHeader + "\nrole=client\nserver_url=" + config.RelaySocketURL +
		"\nkind=client\ninstalled=none\nlocked=false\nanswer=env\npairing=true\ndevice=relay\n"
	good := map[string]string{
		"pair_server": "https://box.example:8081", "fingerprint": clientFP,
		"ca_file": "/h/.config/seamless/server-ca.crt", "binary_sha256": strings.Repeat("0a", 32),
	}
	body := func(over map[string]string) string {
		b := relay
		for _, k := range []string{"pair_server", "fingerprint", "ca_file", "binary_sha256"} {
			v := good[k]
			if o, ok := over[k]; ok {
				v = o
			}
			b += k + "=" + v + "\n"
		}
		return b
	}
	st, err := parseSetupState([]byte(body(nil)))
	require.NoError(t, err)
	require.Equal(t, "/h/.config/seamless/server-ca.crt", st.caFile)
	for _, bad := range []map[string]string{
		{"pair_server": "https://a;id;b:8081"}, {"pair_server": "https://a$(id):8081"}, {"pair_server": "box.example:8081"},
		{"fingerprint": "AB;id"}, {"fingerprint": "00"},
		{"ca_file": "/etc/master.passwd"}, {"ca_file": "server-ca.crt"}, {"ca_file": "/h/../etc/server-ca.crt"},
		{"binary_sha256": "00"}, {"binary_sha256": strings.Repeat("A", 64)}, {"binary_sha256": strings.Repeat("0", 63) + ";"},
	} {
		_, err := parseSetupState([]byte(body(bad)))
		require.Error(t, err, "%v", bad)
	}
	for name, b := range map[string]string{
		"a key twice":     body(nil) + "pair_server=https://other.example:8081\n",
		"a server_url":    strings.Replace(body(nil), "server_url="+config.RelaySocketURL, "server_url=unix://relative/x.sock", 1),
		"a change":        body(nil) + "change=server;id\n",
		"a control in ca": strings.Replace(body(nil), "ca_file=/h", "ca_file=/h\x1b", 1),
		"a path":          strings.Replace(body(nil), "https://box.example:8081", "https://box.example:8081/x", 1),
	} {
		_, err := parseSetupState([]byte(b))
		require.Error(t, err, name)
	}
}

// rootstepCommandWords is the words of a printed command line that the shell
// looks up as commands: each command's first word, the command a sudo or an
// env runs, and a $(...)'s first. Assignments before a command are skipped.
func rootstepCommandWords(line string) []string {
	var out []string
	cmd, under := true, ""
	for _, tok := range strings.Fields(line) {
		switch {
		case tok == "|" || tok == "&&" || tok == "||" || tok == ";":
			cmd, under = true, ""
			continue
		case strings.Contains(tok, "$("):
			// A command substitution: its first word is a command, and the
			// words after it are that command's.
			out = append(out, tok[strings.Index(tok, "$(")+2:])
			cmd, under = false, ""
			continue
		case !cmd:
			continue
		case under == "sudo" && strings.HasPrefix(tok, "-"):
			if tok == "-u" {
				under = "sudo-u"
			}
			continue
		case under == "sudo-u":
			under = "sudo"
			continue
		case under == "env" && strings.Contains(tok, "="):
			continue
		case under == "" && regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`).MatchString(tok):
			continue
		}
		out = append(out, tok)
		switch {
		case strings.HasSuffix(tok, "sudo"):
			under = "sudo"
		case strings.HasSuffix(tok, "env"):
			under = "env"
		default:
			cmd, under = false, ""
		}
	}
	return out
}

// rootstepCommandLines is the command lines of printed text: those from the
// root step's numbered blocks (the line after a header up to a blank one),
// and any other line that runs sudo; never a here-document's body.
func rootstepCommandLines(text string) []string {
	var out []string
	header := regexp.MustCompile(`^\s+\d\. `)
	delim, inBlock := "", false
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case delim != "":
			if trimmed == delim {
				delim = ""
			}
			continue
		case header.MatchString(line):
			inBlock = true
			continue
		case trimmed == "":
			inBlock = false
			continue
		}
		if m := regexp.MustCompile(`<<'([^']+)'`).FindStringSubmatch(line); m != nil {
			delim = m[1]
		}
		if inBlock || strings.HasPrefix(trimmed, "sudo ") || strings.HasPrefix(trimmed, "/usr/bin/sudo ") ||
			strings.Contains(line, "| sudo ") || strings.Contains(line, "| /usr/bin/sudo ") ||
			strings.HasPrefix(trimmed, "echo ") || strings.HasPrefix(trimmed, "printf ") || strings.Contains(line, "$(") {
			out = append(out, trimmed)
		}
	}
	return out
}

// rootstepPATHFree fails for every command word of text's command lines that
// the shell would find on the owner's PATH: anything but a full path, and
// echo and printf, which every shell runs itself.
func rootstepPATHFree(t *testing.T, text string) {
	t.Helper()
	lines := rootstepCommandLines(text)
	require.NotEmpty(t, lines)
	for _, line := range lines {
		for _, w := range rootstepCommandWords(line) {
			if w != "echo" && w != "printf" && !strings.HasPrefix(w, "/") {
				t.Errorf("%q is found on the owner's PATH in: %s", w, line)
			}
		}
	}
}

// L4: every command setup prints for root, or for the relay's or the
// daemon's account, names each tool by its full path: neither the owner's
// shell nor sudo looks any of them up on the owner's PATH.
func TestRootStepReview_NoCommandIsFoundOnTheOwnersPATH(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			r, out, _ := setupTestRun(t, goos, []string{"SEAMLESS_SETUP=client", "SEAMLESS_SERVER_URL=https://box.example:8081",
				"SEAMLESS_PAIR_CODE=ABCD-EFGH-JKMN-PQRS"}, setupFactsFresh, false, "")
			clientArrange(t, r, clientTrusted)
			st, err := clientFirstHalf(t, r)
			require.NoError(t, err, out.String())
			out.Reset()
			require.NoError(t, setupHandOff(r, &st, setupStepsLeft(st, goos), true))
			rootstepPATHFree(t, out.String())
		})
	}

	// Retiring the key: the pin, the edits, the restarts and the daemon's
	// account's own commands.
	r, out := retireTestRun(t, "")
	f := retireNewFake()
	f.arrive = []func(*retireFake){(*retireFake).signInOn, (*retireFake).ownerSigns, (*retireFake).keyOut}
	require.NoError(t, retireRun(r, &setupState{}, f.env(retireConsoleFile(t, t.TempDir(), retireOldKey))))
	rootstepPATHFree(t, out.String())
	r, out = retireTestRun(t, "")
	f = retireNewFake()
	f.pinOK = false
	require.Error(t, retireRun(r, &setupState{}, f.env(retireConsoleFile(t, t.TempDir(), retireOldKey))))
	require.Contains(t, out.String(), "/opt/seamless/etc/daemon-account")
	rootstepPATHFree(t, out.String())
}

// I1: a pairing code from the environment is not printed back: the owner has
// it, and the installer's output may be kept where the environment was not.
func TestRootStepReview_TheCodeFromTheEnvironmentIsNotPrinted(t *testing.T) {
	r, out, _ := setupTestRun(t, "darwin", []string{"SEAMLESS_SETUP=client", "SEAMLESS_SERVER_URL=https://box.example:8081",
		"SEAMLESS_PAIR_CODE=ABCD-EFGH-JKMN-PQRS"}, setupFactsFresh, false, "")
	clientArrange(t, r, clientTrusted)
	st, err := clientFirstHalf(t, r)
	require.NoError(t, err, out.String())
	require.NoError(t, setupHandOff(r, &st, setupStepsLeft(st, "darwin"), true))
	require.NotContains(t, out.String(), "ABCD-EFGH-JKMN-PQRS")
	require.Contains(t, out.String(), "SEAMLESS_PAIR_CODE")
}

// rootstepCert is a certificate this computer's roots do not vouch for (a LAN
// Server's own), which setup trusts once its fingerprint is compared.
func rootstepCert(t *testing.T) clientServerCert {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "box.example"}, DNSNames: []string{"box.example"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	return clientServerCert{tls: true, fingerprint: devices.CertFingerprint(der), trusted: false, why: "x509: certificate signed by unknown authority",
		pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}
