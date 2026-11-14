package main

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/console"
	"github.com/arctop/seamless/internal/passkey"
	"github.com/arctop/seamless/internal/store"
)

// Console sign-in with a passkey (console.sign_in: passkey): serve's wiring,
// and what console-open and `seamlessd approvers add` need to know of it.

// newConsoleSignIn builds the console's passkey sign-in over the main database
// handle, under the relying party config.RelyingParty names (with or without
// peer_auth), or returns nil while console.sign_in is key, the default. The
// nil is the interface's own, so the console sees no sign-in at all.
func newConsoleSignIn(cfg config.Config, db *sql.DB, logger *slog.Logger) (console.SignInView, error) {
	if !cfg.Console.PasskeySignIn() {
		return nil, nil
	}
	rpID, origins := cfg.RelyingParty()
	v, err := passkey.New(passkey.Config{RPID: rpID, Origins: origins})
	if err != nil {
		return nil, fmt.Errorf("console.sign_in: passkey: the relying party (peer_auth.passkeys): %w", err)
	}
	display := "Seamless owner"
	if host := config.Hostname(); host != "" {
		display += " on " + host
	}
	svc, err := approval.NewSignIn(approval.SignInConfig{
		DB: db, Passkeys: v, Origins: origins, UserName: "owner", UserDisplayName: display, Logger: logger,
	})
	if err != nil {
		return nil, fmt.Errorf("console.sign_in: %w", err)
	}
	return svc, nil
}

// passkeyAddAddress is the Add a passkey page on the relying party's first
// origin: the plain address a new device opens. It is no secret.
func passkeyAddAddress(cfg config.Config) string {
	_, origins := cfg.RelyingParty()
	base := cfg.ServerURL()
	if len(origins) > 0 {
		base = origins[0]
	}
	return strings.TrimSuffix(base, "/") + "/console/passkeys/add"
}

// daemonSignsInWithPasskey asks the daemon cfg dials whether its console
// signs browsers in with a passkey: its login page names the mode in a header
// (console.SignInHeader). It sends no credential and follows no redirect, and
// any failure is the key's sign-in, as every daemon before the header did.
func daemonSignsInWithPasskey(cfg config.Config) bool {
	client, err := cfg.HTTPClient(2 * time.Second)
	if err != nil {
		return false
	}
	client.CheckRedirect = config.RefuseRedirect
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, cfg.ServerURL()+"/console/login", nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.Header.Get(console.SignInHeader) == config.ConsoleSignInPasskey
}

// consoleSignInPage is where console-open sends a browser under passkey
// sign-in: the console's own login page on base, which takes no key. ok is
// false while the console signs browsers in with the key, which this config
// says (console.sign_in) or the daemon it dials does (signsIn).
func consoleSignInPage(cfg config.Config, base string, signsIn func(config.Config) bool) (string, bool) {
	if !cfg.Console.PasskeySignIn() && !signsIn(cfg) {
		return "", false
	}
	return strings.TrimSuffix(base, "/") + "/console/login?next=" + url.QueryEscape("/console/"), true
}

// signInOffAdvice is `approvers add` while the key signs browsers in: there
// is no Add a passkey page to send a device to.
const signInOffAdvice = "console.sign_in is key, so the console takes no passkey from a device: set console.sign_in: passkey " +
	"in the daemon's config and restart it. An approver passkey for locked projects is created on the console's Approvers page instead"

// approversAdd is `seamlessd approvers add`: it prints the plain Add a passkey
// address for the owner to open on the new device -- no secret: opening it
// adds nothing -- and, at a terminal, asks for the code that page shows and
// approves it (approveEnrollment).
func approversAdd(ctx context.Context, db *sql.DB, cfg config.Config, tty unlockTerminal, now time.Time) error {
	if !cfg.Console.PasskeySignIn() {
		return errors.New(signInOffAdvice)
	}
	rpID, _ := cfg.RelyingParty()
	fmt.Fprintf(tty.out, "Open this on the device that will hold the passkey:\n\n  %s\n\n", passkeyAddAddress(cfg))
	fmt.Fprintln(tty.out, "It is not a secret: anyone who reaches the console can open it, and nothing is added until you approve here.")
	if rpID == "localhost" {
		fmt.Fprintln(tty.out, "This console's passkeys belong to localhost, so only a browser on this machine can make one; serve the "+
			"console under its name (a TLS server_url) to add one from another device.")
	}
	fmt.Fprintf(tty.out, "The page creates the passkey and shows a code. Approve it with: seamlessd approvers approve --code <code>\n")
	if !tty.interactive {
		return nil
	}
	return approveEnrollment(ctx, db, cfg, tty, "", false, now)
}

// approveEnrollment is `seamlessd approvers approve`: it shows the passkey the
// code names, the owner's to compare with the device's page, asks to confirm
// (unless yes), and enrolls exactly that passkey. Run as the daemon's account
// under peer_auth -- opening the database directly is the proof, as for every
// approvers verb -- and as the owner without it, whose every process is the
// owner anyway. A code names a waiting passkey and carries no key, so the one
// a device was handed by someone else adds their passkey only if the owner
// approves it here, having seen it.
func approveEnrollment(ctx context.Context, db *sql.DB, cfg config.Config, tty unlockTerminal, code string, yes bool, now time.Time) error {
	// One reader for both questions: a second one would lose what the first
	// buffered past its line.
	var in *bufio.Reader
	if tty.interactive {
		in = bufio.NewReader(tty.in)
	}
	if strings.TrimSpace(code) == "" {
		if !tty.interactive {
			return errors.New("approve needs --code: the Add a passkey page shows one, as XXXX-XXXX")
		}
		fmt.Fprint(tty.out, "Code the page shows: ")
		line, err := in.ReadString('\n')
		if err != nil && line == "" {
			return fmt.Errorf("no code was typed: nothing changed (%w)", err)
		}
		if code = strings.TrimSpace(line); code == "" {
			return errors.New("no code was typed: nothing changed")
		}
		if store.FormatUnlockCode(code) == "" {
			return &ownerError{msg: fmt.Sprintf("%q is not a code: the Add a passkey page shows one as XXXX-XXXX, 8 letters and digits. Nothing changed.", code),
				cause: store.ErrInvalidEnrollment}
		}
		// The code came from the owner's own typing: the question below
		// is still asked, as the passkey it names is shown first.
	}
	shown := store.FormatUnlockCode(code)
	e, err := store.PasskeyEnrollmentByCode(ctx, db, code, now)
	if errors.Is(err, store.ErrEnrollmentNotFound) {
		return &ownerError{msg: fmt.Sprintf("no passkey is waiting for code %s: it may be mistyped, already approved, or expired "+
			"(a code lasts %s). Nothing changed. Create the passkey again on %s.", shown,
			minutesWords(store.PasskeyEnrollmentTTL), passkeyAddAddress(cfg)), cause: err}
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(tty.out, "Passkey %q, key %s, made %s for %s%s; waits until %s.\n", e.Credential.Name, e.Fingerprint(),
		e.CreatedAt.Local().Format("15:04"), passkeyFor(e.Credential.RPID), syncedWord(e.Credential.BackupState),
		e.PendingUntil.Local().Format("15:04"))
	if !yes {
		if !tty.interactive {
			return errors.New("approve asks before it adds a passkey: run it at a terminal, or pass --yes once you have checked the name and key above against the device's page")
		}
		fmt.Fprint(tty.out, "Is this the passkey your device's page shows? Add it [y/N]: ")
		answer, err := in.ReadString('\n')
		if err != nil && answer == "" {
			return fmt.Errorf("no answer: nothing changed (%w)", err)
		}
		if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
			fmt.Fprintln(tty.out, "Not added. Nothing changed.")
			return nil
		}
	}
	cred, _, err := store.ApprovePasskeyEnrollment(ctx, db, code, now)
	if errors.Is(err, store.ErrEnrollmentNotFound) {
		return &ownerError{msg: fmt.Sprintf("the passkey for code %s stopped waiting before it was added (approved meanwhile, or expired). Nothing changed.", shown), cause: err}
	}
	if err != nil {
		return err
	}
	also := ""
	if cfg.PeerAuth.Enabled {
		also = ", and approves access to locked projects"
	}
	fmt.Fprintf(tty.out, "Added passkey %q (%s). It signs in to the console now%s.\n", cred.Name, cred.ID, also)
	return nil
}

// passkeyFor names the relying party a passkey was made for.
func passkeyFor(rpID string) string {
	if rpID == "" {
		return "an unknown name"
	}
	return rpID
}

// syncedWord notes a passkey its provider syncs to the owner's other devices.
func syncedWord(synced bool) string {
	if synced {
		return " (synced: every device it syncs to signs in with it)"
	}
	return ""
}

// writePendingEnrollments lists the passkeys waiting for approval under `approvers
// list`, and nothing when none waits.
func writePendingEnrollments(out io.Writer, pending []store.PasskeyEnrollment) {
	if len(pending) == 0 {
		return
	}
	fmt.Fprintln(out, "Waiting for approval (seamlessd approvers approve --code <the code its page shows>):")
	for _, e := range pending {
		fmt.Fprintf(out, "  %q  key %s  made %s, waits until %s\n", e.Credential.Name, e.Fingerprint(),
			e.CreatedAt.Local().Format("2006-01-02 15:04"), e.PendingUntil.Local().Format("15:04"))
	}
}

// consoleSignInChecks is doctor's console.sign_in section: nothing while the
// key signs browsers in (the default, so a default install's report is
// unchanged); under passkey sign-in, the mode, whether any passkey can sign
// in, and -- when peer_auth's section does not already report them -- the
// relying party's checks (passkeyOriginChecks), since the passkeys that sign
// in belong to it with or without peer_auth.
func consoleSignInChecks(ctx context.Context, cfg config.Config, db *sql.DB, hn hostNet) []check {
	if !cfg.Console.PasskeySignIn() {
		return nil
	}
	const name = "console.sign_in"
	out := []check{{statusOK, name, "passkey: a browser signs in to the console with an enrolled passkey, and the key signs no " +
		"browser in (the seam CLI still sends it)"}}
	if db != nil {
		creds, err := store.ApproverCredentials(ctx, db, store.ApproverPasskey)
		switch {
		case err != nil:
			out = append(out, check{statusFail, name, "enrolled passkeys: " + err.Error()})
		case len(creds) == 0:
			out = append(out, check{statusWarn, name, "no passkey is enrolled, so no browser can sign in: run seamlessd approvers add " +
				"on this machine, open the address it prints on the device, and approve the code that page shows"})
		default:
			out = append(out, check{statusOK, name, fmt.Sprintf("%d passkey(s) sign in", len(creds))})
		}
	}
	if !cfg.PeerAuth.Enabled {
		out = append(out, passkeyOriginChecks(ctx, cfg, cfg.Addr, db, hn)...)
	}
	return out
}

// errSignInBrowser refuses --browser beside --sign-in: present, it would mean
// nothing, since console-open --sign-in launches no browser.
var errSignInBrowser = errors.New("--browser does not combine with --sign-in: a terminal sign-in begins on the console's sign-in page, " +
	"in the browser it signs in, and console-open --sign-in only approves the code that page shows")

// signInPageFor is the console's sign-in page on the relying party's first
// origin: where a terminal sign-in begins. It is no secret.
func signInPageFor(cfg config.Config) string {
	return strings.TrimSuffix(passkeyAddAddress(cfg), "/console/passkeys/add") + "/console/login"
}

// runConsoleSignIn is console-open --sign-in: the terminal's half of a
// terminal sign-in, the way in for an owner with no passkey left. The browser
// begins it on the sign-in page ("Show a code") under a cookie bound to its
// key; this shows the browser that code names -- when it began, the address
// it came from, its User-Agent -- and asks, and once the owner says yes it
// approves the code, as the daemon's account under peer_auth (as the owner
// without it, whose every process is the owner), and that browser, and no
// other, is signed in. Nothing it reads or prints is a credential, and it
// launches nothing.
func runConsoleSignIn(cfg config.Config, code, browser string) error {
	return consoleSignIn(cfg, code, browser, stdioUnlockTerminal(), time.Now)
}

// errSignInNoTerminal refuses console-open --sign-in where no one can answer
// its question. Anyone who reaches the console can show a code, so the
// approval shows which browser a code names and asks first; a script that
// approves codes it was handed, unseen, is the very risk the question is
// there for, so there is no flag that skips it.
var errSignInNoTerminal = errors.New("console-open --sign-in shows the browser a code would sign in and asks you before it " +
	"signs it in, so it runs only at a terminal: run it in one (over ssh, ssh -t). Nothing changed")

// consoleSignIn is runConsoleSignIn with its terminal and clock given.
func consoleSignIn(cfg config.Config, code, browser string, tty unlockTerminal, now func() time.Time) error {
	if browser != "" {
		return errSignInBrowser
	}
	if !cfg.Console.PasskeySignIn() {
		return errors.New("console.sign_in is key in this config, so the console's key signs a browser in and there is no terminal sign-in " +
			"to approve: run console-open as the owner instead, or set SEAMLESS_CONFIG to the daemon's config")
	}
	if cfg.PeerAuth.Enabled {
		if err := approverAccountCheck(cfg, defaultPeerAuthEnv()); err != nil {
			return err
		}
	}
	// Settled before the database is opened: no one to ask.
	if !tty.interactive {
		if strings.TrimSpace(code) == "" {
			return fmt.Errorf("console-open --sign-in needs --code, at a terminal: open %s in the browser to sign in, choose Show a code, "+
				"and run the command it shows", signInPageFor(cfg))
		}
		return errSignInNoTerminal
	}
	db, err := openServedDB(cfg, os.Stderr)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	return approveConsoleSignIn(context.Background(), cfg, db, tty, code, now)
}

// approveConsoleSignIn is console-open --sign-in's testable core: it asks for
// the code when none was given, shows the browser that code names
// (writeTerminalSignIn) and asks whether to sign it in, and on a yes approves
// exactly that sign-in and says so. A code that is malformed, mistyped, used,
// expired or ended is refused before anything is asked; any answer but yes --
// no, Enter, the end of input -- changes nothing, and so does no terminal at
// all (errSignInNoTerminal). now is read at each step: the owner may take a
// while to answer, and a code that expires meanwhile is not approved.
func approveConsoleSignIn(ctx context.Context, cfg config.Config, db *sql.DB, tty unlockTerminal, code string, now func() time.Time) error {
	if !tty.interactive {
		return errSignInNoTerminal
	}
	// One reader for both questions: a second one would lose what the first
	// buffered past its line.
	in := bufio.NewReader(tty.in)
	if strings.TrimSpace(code) == "" {
		fmt.Fprintf(tty.out, "Open %s in the browser to sign in, and choose Show a code.\nCode it shows: ", signInPageFor(cfg))
		line, err := in.ReadString('\n')
		if err != nil && line == "" {
			return fmt.Errorf("no code was typed: nothing changed (%w)", err)
		}
		if code = strings.TrimSpace(line); code == "" {
			return errors.New("no code was typed: nothing changed")
		}
	}
	shown := store.FormatUnlockCode(code)
	if shown == "" {
		return &ownerError{msg: fmt.Sprintf("%q is not a sign-in code: the sign-in page shows one as XXXX-XXXX, 8 letters and digits. Nothing changed.", code),
			cause: store.ErrInvalidSignIn}
	}
	notWaiting := func(err error) error {
		return &ownerError{msg: fmt.Sprintf("no sign-in is waiting for code %s: it may be mistyped, already used, or expired (a code lasts %s). "+
			"Nothing changed. Start again on %s.", shown, minutesWords(store.TerminalSignInTTL), signInPageFor(cfg)), cause: err}
	}
	w, err := store.TerminalSignInByCode(ctx, db, code, now())
	switch {
	case errors.Is(err, store.ErrTerminalSignInNotFound):
		return notWaiting(err)
	case err != nil:
		return err
	}
	writeTerminalSignIn(tty.out, w, shown, now(), thisComputersAddrs)
	fmt.Fprint(tty.out, "Sign in this browser? [y/N]: ")
	answer, err := in.ReadString('\n')
	if err != nil && answer == "" {
		fmt.Fprintln(tty.out)
		return fmt.Errorf("no answer: nothing changed (%w)", err)
	}
	if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
		fmt.Fprintln(tty.out, "Not signed in. Nothing changed.")
		return nil
	}
	s, err := store.ApproveTerminalSignIn(ctx, db, w.ID, code, approval.SignInLifetime, now())
	switch {
	case errors.Is(err, store.ErrTerminalSignInNotFound):
		return notWaiting(err)
	case err != nil:
		return err
	}
	fmt.Fprintf(tty.out, "Signed in the browser that showed code %s, until %s. Add a passkey there (Add a passkey) before that ends.\n",
		shown, s.ExpiresAt.Local().Format("2006-01-02 15:04"))
	return nil
}

// writeTerminalSignIn shows the owner the browser a terminal sign-in's code
// would sign in, as its begin recorded it: when it began, the address it came
// from (placed by signInAddrWords, over own) and the User-Agent it sent. The
// store hands both back as bounded printable ASCII
// (store.TerminalSignInBrowser), so nothing a browser sent can write an
// escape into this terminal.
func writeTerminalSignIn(out io.Writer, w store.WaitingTerminalSignIn, code string, now time.Time, own func() ([]netip.Addr, error)) {
	agent := w.Browser.UserAgent
	switch {
	case agent != "":
	case w.Browser.Addr != "":
		agent = "none sent"
	default:
		agent = "not recorded"
	}
	fmt.Fprintf(out, "Code %s names a browser waiting to sign in to this console:\n", code)
	fmt.Fprintf(out, "  began    %s, %s\n", access.Clock(w.CreatedAt), agoWords(now.Sub(w.CreatedAt)))
	fmt.Fprintf(out, "  from     %s\n", signInAddrWords(w.Browser.Addr, own))
	fmt.Fprintf(out, "  browser  %s\n", agent)
	fmt.Fprintf(out, "Yes signs that browser in as you for %d days. Say yes only to a code your own browser shows you now, "+
		"never to one someone sent you: anyone who reaches the console can show one.\n", int(approval.SignInLifetime/(24*time.Hour)))
}

// signInAddrWords is the address a terminal sign-in came from as the owner
// reads it: the address, and whose it is -- this computer's (loopback, which
// a proxy in front of the console also is, or one of own's addresses) or
// another computer's. When own cannot be read, the address alone.
func signInAddrWords(addr string, own func() ([]netip.Addr, error)) string {
	if addr == "" {
		return "not recorded"
	}
	a, err := netip.ParseAddr(addr)
	if err != nil {
		return addr
	}
	a = a.Unmap().WithZone("")
	if a.IsLoopback() {
		return addr + " (this computer, or a proxy in front of the console)"
	}
	mine, err := own()
	if err != nil {
		return addr
	}
	for _, m := range mine {
		if m.Unmap().WithZone("") == a {
			return addr + " (this computer)"
		}
	}
	return addr + " (another computer)"
}

// thisComputersAddrs is the addresses of this computer's own interfaces.
func thisComputersAddrs() ([]netip.Addr, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	var out []netip.Addr
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(n.IP); ok {
				out = append(out, ip)
			}
		}
	}
	return out, nil
}

// agoWords says how long ago something began, in whole minutes.
func agoWords(d time.Duration) string {
	switch m := int(d / time.Minute); {
	case m < 1:
		return "less than a minute ago"
	case m == 1:
		return "1 minute ago"
	default:
		return fmt.Sprintf("%d minutes ago", m)
	}
}

// retiredKey reports a daemon whose key was retired for passkey sign-in
// (setup's "Sign in with passkeys and retire the key"): peer_auth on, the
// console signing browsers in with a passkey, and no mcp.api_key. TCP then
// serves the console's sign-in, its pages and /healthz, and the agents use
// the socket.
func retiredKey(cfg config.Config) bool {
	return strings.TrimSpace(cfg.MCP.APIKey) == "" && cfg.PeerAuth.Enabled && cfg.Console.PasskeySignIn() && !cfg.IsClient()
}

// retiredKeySocketURL is the socket's server_url an MCP request over TCP is
// pointed to once the key is retired, "" otherwise: every other daemon
// answers a missing key as it always did.
func retiredKeySocketURL(cfg config.Config) string {
	if !retiredKey(cfg) || strings.TrimSpace(cfg.PeerAuth.Socket.Path) == "" {
		return ""
	}
	return "unix://" + cfg.PeerAuth.Socket.Path
}

// retiredKeyCheck is doctor's mcp.api_key row for a retired key, which is
// meant, not missing; ok is false for every other daemon.
func retiredKeyCheck(cfg config.Config) (check, bool) {
	if !retiredKey(cfg) {
		return check{}, false
	}
	return check{statusOK, "mcp.api_key", "retired: a browser signs in to the console with a passkey, the agents use the socket, and TCP " +
		"serves only the console's sign-in, its pages and /healthz (seamlessd setup brings a fresh key back)"}, true
}
