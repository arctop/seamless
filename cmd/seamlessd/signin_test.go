package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/console"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// signinConfig is a daemon's config with passkey sign-in on, the console on
// 127.0.0.1:9123 and its passkeys on localhost.
func signinConfig() config.Config {
	cfg := config.Defaults()
	cfg.Addr = "127.0.0.1:9123"
	cfg.Console.SignIn = config.ConsoleSignInPasskey
	return cfg
}

func signinDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// signinWaiting records a passkey waiting for approval as the Add a passkey
// page does, and returns its code.
func signinWaiting(t *testing.T, db *sql.DB, name string, key byte, now time.Time) string {
	t.Helper()
	_, code, err := store.BeginPasskeyEnrollment(context.Background(), db, store.ApproverCredential{
		Kind: store.ApproverPasskey, CredentialID: []byte{key, key}, PublicKey: []byte{0xa5, key}, Alg: -7,
		Name: name, RPID: "localhost", BackupEligible: true, BackupState: true,
	}, store.PasskeyEnrollmentTTL, now)
	require.NoError(t, err)
	return code
}

// signinTTY is a terminal for the approvers verbs: in is what the owner types.
func signinTTY(in string, interactive bool) (unlockTerminal, *bytes.Buffer) {
	var out bytes.Buffer
	return unlockTerminal{in: strings.NewReader(in), out: &out, interactive: interactive}, &out
}

func signinRun(t *testing.T, db *sql.DB, cfg config.Config, tty unlockTerminal, now time.Time, args ...string) error {
	t.Helper()
	cmd, err := parseApprovers(args)
	if err != nil {
		return err
	}
	return cmd.runWith(context.Background(), db, cfg, tty, now, "linux")
}

// Off, serve builds no sign-in: the console's interface is nil itself, so it
// signs browsers in with the key as before. On, it is built over the relying
// party config.RelyingParty names.
func TestNewConsoleSignIn_OffIsNil(t *testing.T) {
	db := signinDB(t)
	off, err := newConsoleSignIn(config.Defaults(), db, nil)
	require.NoError(t, err)
	require.Nil(t, off)
	require.True(t, off == nil, "a nil interface, not a typed nil")

	on, err := newConsoleSignIn(signinConfig(), db, nil)
	require.NoError(t, err)
	require.NotNil(t, on)
	require.Equal(t, "localhost", on.RPID())
	require.Equal(t, []string{"http://localhost:9123"}, on.Origins())
}

// `approvers add` prints the plain Add a passkey address -- no secret, the
// same for anyone -- and how the code is approved; with sign-in off it says
// how to turn it on.
func TestApproversAdd_PrintsThePlainAddress(t *testing.T) {
	db := signinDB(t)
	now := time.Now()
	tty, out := signinTTY("", false)
	require.NoError(t, signinRun(t, db, signinConfig(), tty, now, "add"))
	require.Contains(t, out.String(), "\n  http://localhost:9123/console/passkeys/add\n")
	require.Contains(t, out.String(), "It is not a secret")
	require.Contains(t, out.String(), "only a browser on this machine can make one")
	require.Contains(t, out.String(), "seamlessd approvers approve --code <code>")
	again, out2 := signinTTY("", false)
	require.NoError(t, signinRun(t, db, signinConfig(), again, now, "add"))
	require.Equal(t, out.String(), out2.String(), "the address carries nothing per run")

	served := signinConfig()
	served.PeerAuth.Passkeys = config.PeerPasskeys{RPID: "seamless.example.ts.net", Origins: []string{"https://seamless.example.ts.net:8443"}}
	tty, out = signinTTY("", false)
	require.NoError(t, signinRun(t, db, served, tty, now, "add"))
	require.Contains(t, out.String(), "\n  https://seamless.example.ts.net:8443/console/passkeys/add\n")
	require.NotContains(t, out.String(), "only a browser on this machine")

	off := signinConfig()
	off.Console.SignIn = config.ConsoleSignInKey
	err := signinRun(t, db, off, tty, now, "add")
	require.ErrorContains(t, err, "console.sign_in is key")

	_, err = parseApprovers([]string{"add", "extra"})
	require.Error(t, err)
}

// The owner approves the code the device shows, in both tiers: as the
// daemon's account under peer_auth, as the owner without it. Either way the
// verb shows the passkey first, asks, and adds exactly it; a script must say
// --yes, having checked.
func TestApproversApprove_BothTiers(t *testing.T) {
	now := time.Now()
	for _, peerAuth := range []bool{false, true} {
		db := signinDB(t)
		cfg := signinConfig()
		cfg.PeerAuth.Enabled = peerAuth
		code := signinWaiting(t, db, "owner phone", 1, now)

		tty, out := signinTTY("", false)
		err := signinRun(t, db, cfg, tty, now, "approve", "--code", code)
		require.ErrorContains(t, err, "pass --yes", "a script must say yes")
		require.Contains(t, out.String(), `Passkey "owner phone", key `)
		n, err := store.CountApprovers(context.Background(), db)
		require.NoError(t, err)
		require.Zero(t, n)

		tty, out = signinTTY("n\n", true)
		require.NoError(t, signinRun(t, db, cfg, tty, now, "approve", "--code", code))
		require.Contains(t, out.String(), "Not added. Nothing changed.")
		n, err = store.CountApprovers(context.Background(), db)
		require.NoError(t, err)
		require.Zero(t, n)

		// Typed at the prompt, then confirmed: one reader carries both lines.
		tty, out = signinTTY(strings.ToLower(code)+"\ny\n", true)
		require.NoError(t, signinRun(t, db, cfg, tty, now, "approve"))
		require.Contains(t, out.String(), "Code the page shows: ")
		require.Contains(t, out.String(), "(synced: every device it syncs to signs in with it)")
		require.Contains(t, out.String(), `Added passkey "owner phone"`)
		if peerAuth {
			require.Contains(t, out.String(), "and approves access to locked projects")
		} else {
			require.NotContains(t, out.String(), "locked projects")
		}
		creds, err := store.ApproverCredentials(context.Background(), db, store.ApproverPasskey)
		require.NoError(t, err)
		require.Len(t, creds, 1)
		require.Equal(t, []byte{1, 1}, creds[0].CredentialID)

		// A code works once.
		tty, _ = signinTTY("", false)
		err = signinRun(t, db, cfg, tty, now, "approve", "--code", code, "--yes")
		require.ErrorIs(t, err, store.ErrEnrollmentNotFound)
		require.ErrorContains(t, err, "Nothing changed")

		other := signinWaiting(t, db, "laptop", 2, now)
		tty, _ = signinTTY("", false)
		require.NoError(t, signinRun(t, db, cfg, tty, now, "approve", "--code", other, "--yes"))
	}

	db := signinDB(t)
	for _, bad := range [][]string{{"approve", "--code", "nope"}, {"approve", "extra"}, {"approve", "--frob"}} {
		tty, _ := signinTTY("", false)
		require.Error(t, signinRun(t, db, signinConfig(), tty, now, bad...), "%v", bad)
	}
	tty, _ := signinTTY("", false)
	require.ErrorContains(t, signinRun(t, db, signinConfig(), tty, now, "approve"), "needs --code")
}

// `approvers add` at a terminal goes on to the approval of the code typed.
func TestApproversAdd_ThenApprovesTheCodeTyped(t *testing.T) {
	db := signinDB(t)
	now := time.Now()
	code := signinWaiting(t, db, "tablet", 3, now)
	tty, out := signinTTY(code+"\nyes\n", true)
	require.NoError(t, signinRun(t, db, signinConfig(), tty, now, "add"))
	require.Contains(t, out.String(), "/console/passkeys/add")
	require.Contains(t, out.String(), `Added passkey "tablet"`)
}

// list names the passkeys waiting for approval (JSON only while one waits, so
// its answer is what it was the rest of the time); remove and reset say what
// they signed out, and say nothing more when there was nothing.
func TestApprovers_ListRemoveResetAndSignIns(t *testing.T) {
	db := signinDB(t)
	ctx := context.Background()
	now := time.Now()
	cfg := signinConfig()
	tty, out := signinTTY("", false)
	require.NoError(t, signinRun(t, db, cfg, tty, now, "list", "--json"))
	require.NotContains(t, out.String(), "pending")

	signinWaiting(t, db, "phone", 4, now)
	tty, out = signinTTY("", false)
	require.NoError(t, signinRun(t, db, cfg, tty, now, "list"))
	require.Contains(t, out.String(), "No approver credentials are enrolled. To enroll one, run seamlessd approvers add")
	require.Contains(t, out.String(), "Waiting for approval")
	require.Contains(t, out.String(), `"phone"  key `)
	tty, out = signinTTY("", false)
	require.NoError(t, signinRun(t, db, cfg, tty, now, "list", "--json"))
	var doc struct {
		Pending []pendingJSON `json:"pending"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &doc))
	require.Len(t, doc.Pending, 1)
	require.Equal(t, "phone", doc.Pending[0].Name)

	code := signinWaiting(t, db, "laptop", 5, now)
	cred, _, err := store.ApprovePasskeyEnrollment(ctx, db, code, now)
	require.NoError(t, err)
	_, err = store.CreateConsoleSignIn(ctx, db, "cookie", "key", cred.ID, time.Hour, now)
	require.NoError(t, err)
	tty, out = signinTTY("", false)
	require.NoError(t, signinRun(t, db, cfg, tty, now, "remove", cred.ID))
	require.Contains(t, out.String(), "Signed out the 1 browser(s) it had signed in to the console.")

	tty, out = signinTTY("", false)
	require.NoError(t, signinRun(t, db, cfg, tty, now, "reset", "--yes"))
	require.Contains(t, out.String(), "Removed 0 credential(s) and revoked 0 live lease(s).\n")
	require.Contains(t, out.String(), "dropped 1 passkey(s) waiting for approval")
	tty, out = signinTTY("", false)
	require.NoError(t, signinRun(t, db, cfg, tty, now, "reset", "--yes"))
	require.Equal(t, "Removed 0 credential(s) and revoked 0 live lease(s).\n", out.String(), "nothing more when nothing was signed in")
}

// The decision for console-open under passkey sign-in: it opens the
// console's own sign-in page and writes nothing -- no temp page, no key, no
// link that signs a browser in -- whether this config says so or the daemon
// it dials does. With the key's sign-in it is not handled here, and the
// pre-signed key page works as before.
func TestConsoleOpen_PasskeySignInOpensTheSignInPage(t *testing.T) {
	before, err := filepath.Glob(filepath.Join(os.TempDir(), consoleLoginPattern))
	require.NoError(t, err)
	var opened []string
	var out bytes.Buffer
	deps := signInOpen{
		signsIn:   func(config.Config) bool { return false },
		reachable: func(string) bool { return true },
		open:      func(target, app string) error { opened = append(opened, target+"|"+app); return nil },
		out:       &out,
	}
	cfg := signinConfig()
	cfg.MCP.APIKey = "signin-console-open-key"
	handled, err := openSignInPage(cfg, "Example Browser", deps)
	require.NoError(t, err)
	require.True(t, handled)
	require.Equal(t, []string{cfg.ServerURL() + "/console/login?next=%2Fconsole%2F|Example Browser"}, opened)
	require.Contains(t, out.String(), "sign in there with your passkey")
	require.Contains(t, out.String(), "/console/passkeys/add")
	require.NotContains(t, out.String(), cfg.MCP.APIKey, "nothing it prints is the key")
	after, err := filepath.Glob(filepath.Join(os.TempDir(), consoleLoginPattern))
	require.NoError(t, err)
	require.Equal(t, len(before), len(after), "no pre-signed page is written")

	// A client config says nothing; the daemon it dials does.
	client := config.Defaults()
	client.Addr = "127.0.0.1:9123"
	opened = nil
	handled, err = openSignInPage(client, "", deps)
	require.NoError(t, err)
	require.False(t, handled, "the key's sign-in: the pre-signed page, as before")
	require.Empty(t, opened)
	deps.signsIn = func(config.Config) bool { return true }
	handled, err = openSignInPage(client, "", deps)
	require.NoError(t, err)
	require.True(t, handled)
	require.Len(t, opened, 1)

	deps.reachable = func(string) bool { return false }
	handled, err = openSignInPage(cfg, "", deps)
	require.True(t, handled)
	require.ErrorContains(t, err, "console not reachable")
	deps.reachable = func(string) bool { return true }
	deps.open = func(string, string) error { return errors.New("no browser here") }
	_, err = openSignInPage(cfg, "", deps)
	require.ErrorContains(t, err, "no browser here")
}

// The daemon names passkey sign-in on its login page, and console-open asks
// it with no credential and follows no redirect.
func TestDaemonSignsInWithPasskey_AsksTheLoginPage(t *testing.T) {
	var mu sync.Mutex
	mode, redirect := "", false
	var paths, auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		paths, auths = append(paths, r.URL.Path), append(auths, r.Header.Get("Authorization"))
		if redirect {
			http.Redirect(w, r, "https://elsewhere.example/console/login", http.StatusFound)
			return
		}
		if mode != "" {
			w.Header().Set(console.SignInHeader, mode)
		}
		_, _ = w.Write([]byte("<!doctype html>"))
	}))
	t.Cleanup(srv.Close)
	cfg := config.Defaults()
	cfg.Addr = strings.TrimPrefix(srv.URL, "http://")
	cfg.MCP.APIKey = "signin-probe-key"
	require.False(t, daemonSignsInWithPasskey(cfg), "no header: the key's sign-in")
	set := func(m string, r bool) {
		mu.Lock()
		defer mu.Unlock()
		mode, redirect = m, r
	}
	set(config.ConsoleSignInPasskey, false)
	require.True(t, daemonSignsInWithPasskey(cfg))
	set("something-else", false)
	require.False(t, daemonSignsInWithPasskey(cfg))
	set(config.ConsoleSignInPasskey, true)
	require.False(t, daemonSignsInWithPasskey(cfg), "a redirect is not followed")
	srv.Close()
	require.False(t, daemonSignsInWithPasskey(cfg), "no daemon: the key's sign-in")
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, paths, 4, "one request per question, the redirect's target never asked")
	for i := range paths {
		require.Equal(t, "/console/login", paths[i])
		require.Empty(t, auths[i], "no credential is sent")
	}
}

// doctor says nothing of sign-in while the key signs browsers in; under
// passkey sign-in it names the mode, warns while no passkey can sign in, and
// checks the relying party when peer_auth's section does not.
func TestConsoleSignInChecks(t *testing.T) {
	db := signinDB(t)
	ctx := context.Background()
	hn := hostNet{lookup: fakeLookup(nil), ifaces: func() ([][]netip.Addr, error) { return [][]netip.Addr{addrs("127.0.0.1", "::1")}, nil }}
	require.Empty(t, consoleSignInChecks(ctx, config.Defaults(), db, hn), "off: the report is unchanged")

	cfg := signinConfig()
	rows := consoleSignInChecks(ctx, cfg, db, hn)
	text := rowsText(rows)
	require.Contains(t, text, "passkey: a browser signs in to the console")
	require.Contains(t, text, "no passkey is enrolled, so no browser can sign in")
	require.Contains(t, text, "relying party localhost", "the relying party is checked without peer_auth")

	code := signinWaiting(t, db, "phone", 6, time.Now())
	_, _, err := store.ApprovePasskeyEnrollment(ctx, db, code, time.Now())
	require.NoError(t, err)
	cfg.PeerAuth.Enabled = true
	text = rowsText(consoleSignInChecks(ctx, cfg, db, hn))
	require.Contains(t, text, "1 passkey(s) sign in")
	require.NotContains(t, text, "relying party", "peer_auth's own section reports it")
}
