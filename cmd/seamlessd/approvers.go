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
// whole reason enroll-code lives here and on no route -- a code is what lets a
// first credential in, and nothing an agent can reach over the socket or the
// TCP key may mint one.

const approversUsage = `usage: seamlessd approvers <verb>

Approver credentials sign the owner's approvals of access to locked projects
(peer_auth.locked). Run these as the daemon's own account.

  enroll-code <passkey|touchid>   mint a one-time enrolment code, valid for 10 minutes
  list [--json]                   list the enrolled credentials
  remove <id>                     remove one credential
  reset --yes                     remove every credential and revoke every live lease
`

func runApprovers(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(os.Stderr, approversUsage)
		if len(args) == 0 {
			return errors.New("seamlessd.approvers: a verb is required")
		}
		return nil
	}
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("seamlessd.approvers: %w", err)
	}
	db, err := store.Open(cfg.DBPath())
	if err != nil {
		return fmt.Errorf("seamlessd.approvers: %w", err)
	}
	defer func() { _ = db.Close() }()
	if err := approvers(context.Background(), db, cfg, os.Stdout, args, time.Now()); err != nil {
		return fmt.Errorf("seamlessd.approvers: %w", err)
	}
	return nil
}

// approvers runs one verb against db; the testable core of runApprovers.
func approvers(ctx context.Context, db *sql.DB, cfg config.Config, out io.Writer, args []string, now time.Time) error {
	verb, rest := args[0], args[1:]
	switch verb {
	case "enroll-code":
		if len(rest) != 1 {
			return fmt.Errorf("enroll-code takes one kind: %s", strings.Join(store.ApproverKinds, " or "))
		}
		kind := rest[0]
		if !slices.Contains(store.ApproverKinds, kind) {
			return fmt.Errorf("invalid kind %q: valid kinds are %s", kind, strings.Join(store.ApproverKinds, ", "))
		}
		code, expires, err := store.MintEnrolCode(ctx, db, kind, now)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Enrolment code (%s): %s\n", kind, code)
		fmt.Fprintf(out, "It works once, until %s (%d minutes).\n", expires.Local().Format("15:04"), int(store.EnrolCodeTTL/time.Minute))
		switch kind {
		case store.ApproverPasskey:
			_, origins := cfg.RelyingParty()
			fmt.Fprintf(out, "Open %s/console/approvers in the owner's browser and enter it there.\n", origins[0])
		case store.ApproverTouchID:
			fmt.Fprintln(out, "Run `seam touchid enroll` in the owner's terminal on this Mac and enter it there.")
		}
		if len(cfg.PeerAuth.Locked) == 0 {
			fmt.Fprintln(out, "Note: peer_auth.locked lists no project yet, so nothing asks for an approval.")
		}
		return nil
	case "list":
		fs := flag.NewFlagSet("approvers list", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		asJSON := fs.Bool("json", false, "print JSON")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if fs.NArg() > 0 {
			return fmt.Errorf("list takes no arguments, got %q", fs.Arg(0))
		}
		creds, err := store.ApproverCredentials(ctx, db, "")
		if err != nil {
			return err
		}
		if *asJSON {
			return writeApproversJSON(out, creds)
		}
		if len(creds) == 0 {
			fmt.Fprintln(out, "No approver credentials are enrolled. Mint a code with `seamlessd approvers enroll-code passkey`.")
			return nil
		}
		for _, c := range creds {
			used := "never used"
			if c.LastUsedAt != nil {
				used = "last used " + c.LastUsedAt.Local().Format("2006-01-02 15:04")
			}
			by := "an enrolment code"
			if c.AddedBy != store.AddedByCode {
				by = "credential " + c.AddedBy
			}
			fmt.Fprintf(out, "%s  %-7s  %q  added %s by %s, %s\n", c.ID, c.Kind, c.Name,
				c.AddedAt.Local().Format("2006-01-02 15:04"), by, used)
		}
		return nil
	case "remove":
		if len(rest) != 1 || strings.TrimSpace(rest[0]) == "" {
			return errors.New("remove takes one credential id (see `seamlessd approvers list`)")
		}
		if err := store.RemoveApprover(ctx, db, rest[0]); err != nil {
			return err
		}
		n, err := store.CountApprovers(ctx, db)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Removed %s. %d credential(s) remain.\n", rest[0], n)
		if n == 0 {
			fmt.Fprintln(out, "Only the terminal approval path (seam approve) is left.")
		}
		return nil
	case "reset":
		if !slices.Equal(rest, []string{"--yes"}) {
			return errors.New("reset removes every credential and revokes every live lease: pass --yes to confirm")
		}
		creds, leases, err := store.ResetApprovers(ctx, db, now)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Removed %d credential(s) and revoked %d live lease(s).\n", creds, leases)
		return nil
	default:
		return fmt.Errorf("unknown verb %q\n\n%s", verb, approversUsage)
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

func writeApproversJSON(out io.Writer, creds []store.ApproverCredential) error {
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
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(map[string]any{"approvers": rows})
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
		Events: rec, Host: host, LeaseCap: cfg.PeerAuth.LeaseCap(), Logger: logger,
	}), nil
}
