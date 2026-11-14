package main

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

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
