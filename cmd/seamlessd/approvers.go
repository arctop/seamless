package main

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/passkey"
	"github.com/arctop/seamless/internal/store"
)

// seamlessd approvers -- the owner's approver credentials for locked projects:
// the passkeys and Touch ID keys that can sign an access approval.
//
// Every verb opens the database directly, so it runs as the daemon's own
// account (under peer_auth, `sudo -k -u _seamless env SEAMLESS_CONFIG=...
// seamlessd approvers ...`): becoming that account is the proof. That is the
// whole reason enroll-code lives here and on no route -- a code is what lets
// every credential in, and nothing an agent can reach over the socket or the
// TCP key may mint one.

const approversUsage = `usage: seamlessd approvers <verb>

Approver credentials sign the owner's approvals of access to locked projects.
Run these as the daemon's own account.

  enroll-code <passkey|touchid> --key <fingerprint>
                                  mint a one-time enrollment code for one key, valid for 10 minutes
  list [--json]                   list the enrolled credentials
  remove <id>                     remove one credential
  reset --yes                     remove every credential and revoke every live lease

With console sign-in by passkey (console.sign_in: passkey):

  add                             print the Add a passkey address to open on a new device,
                                  then approve the code its page shows
  approve [--code <code>] [--yes] add the passkey whose page shows that code (asks to confirm)
`

func runApprovers(args []string) error {
	if len(args) == 0 || approversHelp(args) {
		fmt.Fprint(os.Stderr, approversUsage)
		if len(args) == 0 {
			return errors.New("seamlessd.approvers: a verb is required")
		}
		return nil
	}
	// The arguments are settled before anything is opened, so a typo or a --help
	// never reaches the database.
	cmd, err := parseApprovers(args)
	if err != nil {
		return fmt.Errorf("seamlessd.approvers: %w", err)
	}
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("seamlessd.approvers: %w", err)
	}
	db, err := openServedDB(cfg, os.Stderr)
	if err != nil {
		return fmt.Errorf("seamlessd.approvers: %w", err)
	}
	defer func() { _ = db.Close() }()
	if err := cmd.runWith(context.Background(), db, cfg, stdioUnlockTerminal(), time.Now(), runtime.GOOS); err != nil {
		return fmt.Errorf("seamlessd.approvers: %w", err)
	}
	return nil
}

// approversHelp says args ask for the usage: "help" in the verb's place, or a
// help flag anywhere after it.
func approversHelp(args []string) bool {
	return args[0] == "help" || slices.ContainsFunc(args, func(a string) bool {
		return a == "-h" || a == "-help" || a == "--help"
	})
}

// approvers runs one verb against db; the testable core of runApprovers.
func approvers(ctx context.Context, db *sql.DB, cfg config.Config, out io.Writer, args []string, now time.Time) error {
	return approversOn(ctx, db, cfg, out, args, now, runtime.GOOS)
}

// firstApproverAdvice is how to enroll a first approver, said the same way by
// `approvers list` and doctor: a passkey with a code minted as the daemon's
// account and, where the daemon runs on a Mac and serves the peer_auth socket
// the Touch ID app talks over, this Mac's Touch ID with `seam touchid enroll`
// in the owner's own terminal (its window shows the command that mints its code).
func firstApproverAdvice(goos string, peerAuth bool) string {
	s := "create a passkey on the console's Approvers page, which shows the command that mints its code"
	if goos == "darwin" && peerAuth {
		s += "; or, for this Mac's Touch ID, run `seam touchid enroll` in the owner's own terminal, whose window shows the command that mints its code"
	}
	return s
}

// approversOn is approvers on the platform goos.
func approversOn(ctx context.Context, db *sql.DB, cfg config.Config, out io.Writer, args []string, now time.Time, goos string) error {
	cmd, err := parseApprovers(args)
	if err != nil {
		return err
	}
	return cmd.run(ctx, db, cfg, out, now, goos)
}

// approversCmd is one verb with its arguments checked.
type approversCmd struct {
	verb        string
	kind        string // enroll-code
	fingerprint string // enroll-code
	asJSON      bool   // list
	id          string // remove
	code        string // approve: the code the device's page shows
	yes         bool   // approve: no question asked
}

// parseApprovers checks a verb and its arguments and touches nothing, so a bad
// one is refused before any database is opened.
func parseApprovers(args []string) (approversCmd, error) {
	cmd := approversCmd{verb: args[0]}
	rest := args[1:]
	switch cmd.verb {
	case "enroll-code":
		if len(rest) == 0 {
			return cmd, fmt.Errorf("enroll-code takes one kind and --key: %s", strings.Join(store.ApproverKinds, " or "))
		}
		cmd.kind = rest[0]
		if !slices.Contains(store.ApproverKinds, cmd.kind) {
			return cmd, fmt.Errorf("invalid kind %q: valid kinds are %s", cmd.kind, strings.Join(store.ApproverKinds, ", "))
		}
		fs := flag.NewFlagSet("approvers enroll-code", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		key := fs.String("key", "", "the fingerprint of the key this code enrolls")
		if err := fs.Parse(rest[1:]); err != nil {
			return cmd, fmt.Errorf("enroll-code: %w", err)
		}
		if fs.NArg() > 0 || *key == "" {
			return cmd, errors.New("enroll-code needs --key <fingerprint>: a code enrolls one key. " +
				"The console's Approvers page shows the command for a new passkey, and the Seamless Approve window for a Touch ID key")
		}
		cmd.fingerprint = store.FormatKeyFingerprint(*key)
		if cmd.fingerprint == "" {
			return cmd, fmt.Errorf("invalid value %q for --key: a key fingerprint looks like XXXX-XXXX-XXXX-XXXX", *key)
		}
	case "list":
		fs := flag.NewFlagSet("approvers list", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		asJSON := fs.Bool("json", false, "print JSON")
		if err := fs.Parse(rest); err != nil {
			return cmd, err
		}
		if fs.NArg() > 0 {
			return cmd, fmt.Errorf("list takes no arguments, got %q", fs.Arg(0))
		}
		cmd.asJSON = *asJSON
	case "remove":
		if len(rest) != 1 || strings.TrimSpace(rest[0]) == "" {
			return cmd, errors.New("remove takes one credential id (see `seamlessd approvers list`)")
		}
		cmd.id = rest[0]
	case "reset":
		if !slices.Equal(rest, []string{"--yes"}) {
			return cmd, errors.New("reset removes every credential and revokes every live lease: pass --yes to confirm")
		}
	case "add":
		if len(rest) > 0 {
			return cmd, fmt.Errorf("add takes no arguments, got %q: it prints the address a new device opens, and asks for the code that page shows", rest[0])
		}
	case "approve":
		fs := flag.NewFlagSet("approvers approve", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		code := fs.String("code", "", "the code the device's Add a passkey page shows")
		yes := fs.Bool("yes", false, "add it without asking")
		if err := fs.Parse(rest); err != nil {
			return cmd, fmt.Errorf("approve: %w", err)
		}
		if fs.NArg() > 0 {
			return cmd, fmt.Errorf("approve takes --code and --yes, got %q", fs.Arg(0))
		}
		if *code != "" && store.FormatUnlockCode(*code) == "" {
			return cmd, fmt.Errorf("invalid value %q for --code: the Add a passkey page shows a code as XXXX-XXXX, 8 letters and digits", *code)
		}
		cmd.code, cmd.yes = *code, *yes
	default:
		return cmd, fmt.Errorf("unknown verb %q\n\n%s", cmd.verb, approversUsage)
	}
	return cmd, nil
}

// run carries out a parsed verb against db on the platform goos, with no
// terminal to ask at: what a script gets.
func (cmd approversCmd) run(ctx context.Context, db *sql.DB, cfg config.Config, out io.Writer, now time.Time, goos string) error {
	return cmd.runWith(ctx, db, cfg, unlockTerminal{out: out}, now, goos)
}

// runWith is run with tty, where add and approve ask for a code and a yes.
func (cmd approversCmd) runWith(ctx context.Context, db *sql.DB, cfg config.Config, tty unlockTerminal, now time.Time, goos string) error {
	out := tty.out
	switch cmd.verb {
	case "add":
		return approversAdd(ctx, db, cfg, tty, now)
	case "approve":
		return approveEnrollment(ctx, db, cfg, tty, cmd.code, cmd.yes, now)
	case "enroll-code":
		code, expires, err := store.MintEnrollCode(ctx, db, cmd.kind, cmd.fingerprint, now)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Enrollment code (%s) for key %s: %s\n", cmd.kind, cmd.fingerprint, code)
		fmt.Fprintf(out, "It works once, until %s (%d minutes), and only for that key. Check that key against the one on your screen.\n",
			expires.Local().Format("15:04"), int(store.EnrollCodeTTL/time.Minute))
		switch cmd.kind {
		case store.ApproverPasskey:
			fmt.Fprintln(out, "Enter it on the console's Approvers page, where the new passkey is waiting.")
		case store.ApproverTouchID:
			fmt.Fprintln(out, "Type it into the Seamless Approve window that showed this key, in the owner's session on this Mac.")
		}
		if locked, lerr := lockedProjects(ctx, cfg, db); lerr != nil {
			return lerr
		} else if len(locked) == 0 && !cfg.Isolation.Requests {
			fmt.Fprintln(out, "Note: no project is locked yet, so nothing asks for an approval.")
		}
		return nil
	case "list":
		creds, err := store.ApproverCredentials(ctx, db, "")
		if err != nil {
			return err
		}
		pending, err := store.PendingPasskeyEnrollments(ctx, db, now)
		if err != nil {
			return err
		}
		if cmd.asJSON {
			return writeApproversJSON(out, creds, pending)
		}
		defer writePendingEnrollments(out, pending)
		if len(creds) == 0 {
			advice := firstApproverAdvice(goos, cfg.PeerAuth.Enabled)
			if cfg.Console.PasskeySignIn() {
				// Under passkey sign-in a passkey is added from its device.
				advice = "run seamlessd approvers add, open the address it prints on the device that will hold the passkey, " +
					"and approve the code that page shows"
			}
			fmt.Fprintf(out, "No approver credentials are enrolled. To enroll one, %s.\n", advice)
			return nil
		}
		for _, c := range creds {
			used := "never used"
			if c.LastUsedAt != nil {
				used = "last used " + c.LastUsedAt.Local().Format("2006-01-02 15:04")
			}
			by := "an enrollment code"
			if c.AddedBy != store.AddedByCode {
				by = "credential " + c.AddedBy
			}
			fmt.Fprintf(out, "%s  %-7s  %q  added %s by %s, %s\n", c.ID, c.Kind, c.Name,
				c.AddedAt.Local().Format("2006-01-02 15:04"), by, used)
		}
		return nil
	case "remove":
		signedOut, err := store.RemoveApproverSigningOut(ctx, db, cmd.id, now)
		if err != nil {
			return err
		}
		n, err := store.CountApprovers(ctx, db)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Removed %s. %d credential(s) remain.\n", cmd.id, n)
		if signedOut > 0 {
			fmt.Fprintf(out, "Signed out the %d browser(s) it had signed in to the console.\n", signedOut)
		}
		if n == 0 {
			fmt.Fprintln(out, "Only the terminal approval path (seam approve) is left.")
		}
		return nil
	default: // reset: parseApprovers admits no other verb
		r, err := store.ResetApproversAll(ctx, db, now)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Removed %d credential(s) and revoked %d live lease(s).\n", r.Credentials, r.Leases)
		if r.SignIns > 0 || r.Enrollments > 0 {
			fmt.Fprintf(out, "Signed out %d browser(s) and dropped %d passkey(s) waiting for approval.\n", r.SignIns, r.Enrollments)
		}
		return nil
	}
}

type approverJSON struct {
	ID           string   `json:"id"`
	Kind         string   `json:"kind"`
	Name         string   `json:"name"`
	CredentialID string   `json:"credential_id"`
	Alg          int64    `json:"alg"`
	SignCount    uint32   `json:"sign_count"`
	Synced       bool     `json:"synced"`
	Transports   []string `json:"transports,omitempty"`
	AddedAt      string   `json:"added_at"`
	AddedBy      string   `json:"added_by"`
	LastUsedAt   string   `json:"last_used_at,omitempty"`
}

// pendingJSON is a passkey a device added, waiting for the owner's approval.
type pendingJSON struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Fingerprint  string `json:"fingerprint"`
	CreatedAt    string `json:"created_at"`
	PendingUntil string `json:"pending_until"`
}

func writeApproversJSON(out io.Writer, creds []store.ApproverCredential, pending []store.PasskeyEnrollment) error {
	rows := make([]approverJSON, 0, len(creds))
	for _, c := range creds {
		r := approverJSON{
			ID: c.ID, Kind: c.Kind, Name: c.Name, CredentialID: hex.EncodeToString(c.CredentialID), Alg: c.Alg,
			SignCount: c.SignCount, Synced: c.BackupState, Transports: c.Transports,
			AddedAt: core.FormatTime(c.AddedAt), AddedBy: c.AddedBy,
		}
		if c.LastUsedAt != nil {
			r.LastUsedAt = core.FormatTime(*c.LastUsedAt)
		}
		rows = append(rows, r)
	}
	doc := map[string]any{"approvers": rows}
	// Only while a passkey waits, so the answer is what it always was the
	// rest of the time.
	if len(pending) > 0 {
		waiting := make([]pendingJSON, 0, len(pending))
		for _, e := range pending {
			waiting = append(waiting, pendingJSON{ID: e.ID, Name: e.Credential.Name, Fingerprint: e.Fingerprint(),
				CreatedAt: core.FormatTime(e.CreatedAt), PendingUntil: core.FormatTime(e.PendingUntil)})
		}
		doc["pending"] = waiting
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

// newApprovals builds the daemon's approval service over the main database
// handle: the relying party from peer_auth.passkeys (or the console on
// localhost), and the owner's account name an authenticator shows.
func newApprovals(cfg config.Config, db *sql.DB, rec *events.Recorder, logger *slog.Logger) (*approval.Service, error) {
	rpID, origins := cfg.RelyingParty()
	v, err := passkey.New(passkey.Config{RPID: rpID, Origins: origins})
	if err != nil {
		return nil, fmt.Errorf("peer_auth.passkeys: %w", err)
	}
	host := config.Hostname()
	display := "Seamless owner"
	if host != "" {
		display += " on " + host
	}
	return approval.New(approval.Config{
		DB: db, Passkeys: v, Origins: origins, UserName: "owner", UserDisplayName: display,
		Events: rec, Host: host, LeaseCap: cfg.PeerAuth.LeaseCap(), ForceLocked: cfg.PeerAuth.ForceLocked, Logger: logger,
	}), nil
}
