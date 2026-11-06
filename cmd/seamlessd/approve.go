package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/console"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/store"
)

// seamlessd approve -- the terminal approval path's commit.
//
// The owner runs `seam approve` in their own terminal; it shows the request's
// facts, asks for a duration, and then runs
//
//	sudo -k -u <daemon account> seamlessd approve <id> --for <d> --digest <x>
//
// The sudo is the proof: it asks for the owner's Touch ID or password every
// time (-k leaves no cached ticket an agent could reuse), and becoming the
// daemon's account is what opens the database at all -- under peer_auth the
// data directory is that account's and 0700. This command checks it really
// runs as that account before it commits anything.

const approveUsage = `usage: seamlessd approve <request> --for <session|1h|4h> --digest <facts digest>
       seamlessd approve <request> --deny

Approves (or denies) an access request for a locked project. Run it as the
daemon's own account: seam approve, in the owner's terminal, runs it through
sudo -k with the digest of the facts it showed.
`

func runApprove(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("seamlessd.approve: %w", err)
	}
	if err := approverAccountCheck(cfg, defaultPeerAuthEnv()); err != nil {
		return fmt.Errorf("seamlessd.approve: %w", err)
	}
	db, err := store.Open(cfg.DBPath())
	if err != nil {
		return fmt.Errorf("seamlessd.approve: %w", err)
	}
	defer func() { _ = db.Close() }()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	svc, err := newApprovals(cfg, db, events.NewRecorder(db), logger)
	if err != nil {
		return fmt.Errorf("seamlessd.approve: %w", err)
	}
	if err := approve(context.Background(), svc, os.Stdout, args); err != nil {
		return fmt.Errorf("seamlessd.approve: %w", err)
	}
	return nil
}

// approverAccountCheck refuses unless this process runs as the daemon's own
// account while peer_auth locks a project: the data directory must be this
// account's and 0700, and this account must not be the owner's.
func approverAccountCheck(cfg config.Config, env peerAuthEnv) error {
	pa := cfg.PeerAuth
	if !pa.Enabled || len(pa.Locked) == 0 {
		return errors.New("peer_auth locks no project, so there is nothing to approve")
	}
	facts, err := env.inspect(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("data_dir %s: %w (run this as the daemon's account)", cfg.DataDir, err)
	}
	if int(facts.uid) != env.euid || facts.mode.Perm() != 0o700 {
		return fmt.Errorf("this process is not the daemon's account (data_dir %s belongs to uid %d, mode %o): run it through `seam approve`, which uses sudo -k -u <daemon account>",
			cfg.DataDir, facts.uid, facts.mode.Perm())
	}
	owner, err := env.resolve(strings.TrimSpace(pa.Owner))
	if err == nil && int(owner.UID) == env.euid {
		return errors.New("this is the owner's account, not the daemon's: the terminal approval needs the sudo to the daemon's account")
	}
	return nil
}

// approve is the testable core of runApprove.
func approve(ctx context.Context, svc *approval.Service, out io.Writer, args []string) error {
	fs := flag.NewFlagSet("approve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	duration := fs.String("for", "", "the lease's duration: "+strings.Join(approval.Durations, ", "))
	digest := fs.String("digest", "", "the digest of the facts the owner was shown")
	deny := fs.Bool("deny", false, "deny the request instead")
	var ref string
	rest := args
	for len(rest) > 0 {
		if err := fs.Parse(rest); err != nil {
			return fmt.Errorf("%w; usage:\n%s", err, strings.TrimSpace(approveUsage))
		}
		rest = fs.Args()
		if len(rest) > 0 {
			if ref != "" {
				return fmt.Errorf("one request at a time, got %q and %q", ref, rest[0])
			}
			ref, rest = rest[0], rest[1:]
		}
	}
	if ref == "" {
		return fmt.Errorf("name the request (R-XXXX or its id); usage:\n%s", strings.TrimSpace(approveUsage))
	}
	a, err := svc.Resolve(ctx, ref)
	if err != nil {
		return err
	}
	short := approval.ShortID(a.ID)
	if *deny {
		if *duration != "" || *digest != "" {
			return errors.New("--deny takes no --for or --digest")
		}
		if err := svc.Deny(ctx, a.ID, "daemon-account"); err != nil {
			return err
		}
		fmt.Fprintf(out, "Denied %s: %s stays locked for that session.\n", short, a.Project)
		return nil
	}
	if *duration == "" || *digest == "" {
		return errors.New("an approval needs --for and --digest (seam approve supplies both)")
	}
	lease, err := svc.Approve(ctx, approval.Decision{ID: a.ID, Duration: *duration, Digest: *digest, Proof: approval.Proof{Kind: approval.ProofDaemon}})
	if errors.Is(err, approval.ErrFactsChanged) {
		return fmt.Errorf("the facts of %s changed since they were shown: run seam approve again and look at them first", short)
	}
	if err != nil {
		return err
	}
	until := "until the session ends (at the latest " + lease.ExpiresAt.Local().Format("15:04") + ")"
	if *duration != approval.ForSession {
		until = "until " + lease.ExpiresAt.Local().Format("15:04")
	}
	fmt.Fprintf(out, "Approved %s: %s, read and write, %s.\n", short, a.Project, until)
	return nil
}

// terminalApprover tells seam approve how to run this daemon's terminal
// commit: the account it runs as, its own executable, and the config it
// loaded. Unknown parts are left empty, and seam approve says so.
func terminalApprover(cfg config.Config) *console.TerminalApprover {
	t := &console.TerminalApprover{Config: absConfigPath(cfg.SourcePath())}
	if u, err := user.Current(); err == nil {
		t.Account = u.Username
	}
	if exe, err := os.Executable(); err == nil {
		if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
			exe = resolved
		}
		t.Seamlessd = exe
	}
	return t
}
