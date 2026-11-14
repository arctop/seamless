package main

import (
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// rootstepPlan is a relay plan whose every value holds its format.
func rootstepPlan(goos string) clientRelayPlan {
	return clientRelayPlan{goos: goos, owner: "example-owner", server: "https://box.example:8081", fingerprint: clientFP,
		source: "/h/.local/bin/seamlessd", sha256: strings.Repeat("ab", 32), uid: 480, gid: 479}
}

// A block is one AND-list: every command but the last ends its line with &&,
// a here-document's && on the line that opens it, and the document's lines
// and its end reach the shell at the margin, as written.
func TestRootStepChain(t *testing.T) {
	doc := rootstepHeredoc("/usr/bin/sudo /usr/bin/tee /x >/dev/null", "line one\n  line two\n")
	require.Equal(t, "/usr/bin/sudo /usr/bin/tee /x >/dev/null <<'EOF'\nline one\n  line two\nEOF", doc)
	require.Equal(t, "a &&\n/usr/bin/sudo /usr/bin/tee /x >/dev/null <<'EOF' &&\nline one\n  line two\nEOF\nb &&\nc",
		rootstepChain([]string{"a", doc, "b", "c"}))
	require.Equal(t, "a", rootstepChain([]string{"a"}))

	// A body holding the delimiter gets another.
	require.Equal(t, "cat <<'EOF2'\nEOF\nEOF1\nEOF2", rootstepHeredoc("cat", "EOF\nEOF1\n"))
}

// The tools are named by the paths macOS and Linux keep them at, and on this
// computer each of its own OS is there.
func TestRootStepTools(t *testing.T) {
	mac, lin := rootstepToolsFor("darwin"), rootstepToolsFor("linux")
	require.Equal(t, "/usr/sbin/chown", mac.chown)
	require.Equal(t, "/usr/bin/shasum -a 256", mac.sum)
	require.Equal(t, "/bin/chown", lin.chown)
	require.Equal(t, "/usr/bin/sha256sum", lin.sum)
	require.Equal(t, "/bin/systemctl", lin.systemctl)
	for _, tt := range []rootstepTools{mac, lin} {
		for _, p := range []string{tt.sudo, tt.env, tt.id, tt.install, tt.tee, tt.chmod, tt.chown, tt.mv, tt.sum} {
			require.True(t, strings.HasPrefix(p, "/"), p)
		}
	}
	var own rootstepTools
	var always, usual []string
	switch runtime.GOOS {
	case "darwin":
		own = mac
		always = []string{own.sudo, own.env, own.id, own.install, own.tee, own.chmod, own.chown, own.mv, strings.Fields(own.sum)[0], own.dscl, own.launchctl}
	case "linux":
		own = lin
		// coreutils is everywhere; sudo, shadow's tools and systemd are not
		// on every machine a test runs on.
		always = []string{own.env, own.id, own.install, own.tee, own.chmod, own.chown, own.mv, own.sum}
		usual = []string{own.sudo, own.useradd, own.groupadd, own.usermod, own.systemctl}
	default:
		t.Skip("the relay's root step is printed for macOS and Linux")
	}
	for _, p := range always {
		_, err := os.Stat(p)
		require.NoError(t, err, "%s is where the step says", p)
	}
	for _, p := range usual {
		if _, err := os.Stat(p); err != nil {
			t.Logf("%s is not on this machine: %v", p, err)
		}
	}
}

// A plan prints only when every value holds its format.
func TestRootStepPlanProblem(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		require.Empty(t, rootstepPlan(goos).problem(), goos)
	}
	for name, change := range map[string]func(*clientRelayPlan){
		"windows":            func(p *clientRelayPlan) { p.goos = "windows" },
		"no owner":           func(p *clientRelayPlan) { p.owner = "" },
		"an option as owner": func(p *clientRelayPlan) { p.owner = "-x" },
		"a space in owner":   func(p *clientRelayPlan) { p.owner = "a b" },
		"plain http":         func(p *clientRelayPlan) { p.server = "http://box.example:8081" },
		"a hostile host":     func(p *clientRelayPlan) { p.server = "https://a;id;b:8081" },
		"no fingerprint":     func(p *clientRelayPlan) { p.fingerprint = "" },
		"a bad fingerprint":  func(p *clientRelayPlan) { p.fingerprint = "AB;id" },
		"a relative source":  func(p *clientRelayPlan) { p.source = "bin/seamlessd" },
		"no hash":            func(p *clientRelayPlan) { p.sha256 = "" },
		"a short hash":       func(p *clientRelayPlan) { p.sha256 = "00" },
		"no free uid":        func(p *clientRelayPlan) { p.uid = -1 },
		"no free gid":        func(p *clientRelayPlan) { p.gid = -1 },
	} {
		p := rootstepPlan("darwin")
		change(&p)
		require.NotEmpty(t, p.problem(), name)
	}
	// Ids that are not made are not judged.
	p := rootstepPlan("darwin")
	p.accountExists, p.groupExists, p.uid, p.gid = true, true, -1, -1
	require.Empty(t, p.problem())
	p.goos = "linux"
	require.Empty(t, p.problem())
}

// Every value root's commands carry is quoted: a space, a quote or a shell
// metacharacter is one word, and an ordinary value prints as it is.
func TestRootStepQuotesEveryValue(t *testing.T) {
	p := rootstepPlan("linux")
	p.owner, p.source, p.server = "o'wner$(id)", "/h/my bin/seamless;d", "https://[2001:db8::1]:8081"
	text := clientRootText(clientRelayRootStep(p))
	require.Contains(t, text, `/usr/sbin/usermod -aG _seamlessowner 'o'\''wner$(id)' &&`)
	require.Contains(t, text, `< '/h/my bin/seamless;d' &&`)
	require.Contains(t, text, `--server 'https://[2001:db8::1]:8081' --fingerprint `+clientFP+` --code -`)
	require.Contains(t, clientRootText(clientRelayRootStep(rootstepPlan("linux"))), "--server https://box.example:8081 --fingerprint")
}

// The certificate printed for root is the one compared, one certificate,
// re-encoded; anything else is refused, the file is read as the owner.
func TestRootStepCAPEM(t *testing.T) {
	cert := rootstepCert(t)
	read := func(data string) func(string) ([]byte, error) {
		return func(string) ([]byte, error) { return []byte(data), nil }
	}
	got, err := rootstepCAPEM(read(string(cert.pem)), "/h/x", strings.ToLower(cert.fingerprint))
	require.NoError(t, err)
	require.Equal(t, string(cert.pem), got)

	other := rootstepCert(t)
	for name, tc := range map[string]struct{ data, fp string }{
		"another certificate": {string(other.pem), cert.fingerprint},
		"a line after it":     {string(cert.pem) + "EOF\n/usr/bin/sudo /bin/rm -rf /\n", cert.fingerprint},
		"two certificates":    {string(cert.pem) + string(other.pem), cert.fingerprint},
		"a key":               {"-----BEGIN PRIVATE KEY-----\nQUJD\n-----END PRIVATE KEY-----\n", cert.fingerprint},
		"not PEM":             {"root:*:0:0::0:0:System Administrator:/var/root:/bin/sh\n", cert.fingerprint},
	} {
		_, err := rootstepCAPEM(read(tc.data), "/h/x", tc.fp)
		require.Error(t, err, name)
	}
	_, err = rootstepCAPEM(func(string) ([]byte, error) { return nil, errors.New("permission denied") }, "/h/x", cert.fingerprint)
	require.ErrorContains(t, err, "permission denied")
}

// The relay step prints nothing from a certificate copy that is not the one
// compared, or from a state whose values do not hold.
func TestRootStepRelayStepRefusesWhatDoesNotHold(t *testing.T) {
	cert := rootstepCert(t)
	run := func(t *testing.T) (*setupRun, *strings.Builder, setupState) {
		t.Helper()
		r, out, _ := setupTestRun(t, "linux", []string{"SEAMLESS_SETUP=client", "SEAMLESS_SERVER_URL=https://box.example:8081",
			"SEAMLESS_PAIR_CODE=ABCD", "SEAMLESS_SERVER_FINGERPRINT=" + cert.fingerprint}, setupFactsFresh, false, "")
		clientArrange(t, r, cert)
		st, err := clientFirstHalf(t, r)
		require.NoError(t, err, out.String())
		require.NotEmpty(t, st.caFile)
		out.Reset()
		return r, out, st
	}

	r, out, st := run(t)
	require.NoError(t, setupHandOff(r, &st, setupStepsLeft(st, "linux"), true))
	require.Contains(t, out.String(), "<<'EOF' &&\n"+string(cert.pem)+"EOF\n")

	r, out, st = run(t)
	other := rootstepCert(t)
	require.NoError(t, r.client.writeFile(st.caFile, other.pem), "a process of the owner's swaps the copy")
	err := setupHandOff(r, &st, setupStepsLeft(st, "linux"), true)
	require.ErrorIs(t, err, errSetupRefused)
	require.ErrorContains(t, err, "is not the one compared")
	require.NotContains(t, out.String(), "/usr/bin/sudo", "nothing is printed")

	r, out, st = run(t)
	r.client.owner = func() string { return "" }
	err = setupHandOff(r, &st, setupStepsLeft(st, "linux"), true)
	require.ErrorIs(t, err, errSetupRefused)
	require.ErrorContains(t, err, "setup prints no root step from these values")
	require.NotContains(t, out.String(), "/usr/bin/sudo")
}

// A typed code is shown beside the step that takes it: the person's own
// input, on the same screen.
func TestRootStepTypedCodeIsShown(t *testing.T) {
	r, out, _ := setupTestRun(t, "darwin", nil, setupFactsFresh, true, "2\n2\nhttps://box.example:8081\nyes\n\n")
	clientArrange(t, r, clientTrusted)
	st, err := clientFirstHalf(t, r)
	require.NoError(t, err, out.String())
	r2, out2, _ := setupTestRun(t, "darwin", nil, setupFactsFresh, true, "WXYZ-1234\n")
	require.NoError(t, setupHandOff(r2, &st, setupStepsLeft(st, "darwin"), true))
	require.Contains(t, out2.String(), "code         WXYZ-1234 -- paste it when step 3 waits for it")
}
