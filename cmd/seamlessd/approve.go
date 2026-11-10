package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/access"
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

Approves (or denies) an access request for a locked project, or -- with
isolation.requests on -- a read grant request for a confidential one. Run it
as the daemon's own account: seam approve, in the owner's terminal, runs it
through sudo -k with the digest of the facts it showed.
`

func runApprove(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("seamlessd.approve: %w", err)
	}
	if err := approverAccountCheck(cfg, defaultPeerAuthEnv()); err != nil {
		// Said to the owner's terminal as one line, like every other outcome
		// here (ownerError), not as a log record.
		return &ownerError{msg: err.Error(), cause: err}
	}
	db, err := openServedDB(cfg, os.Stderr)
	if err != nil {
		return &ownerError{msg: err.Error(), cause: err}
	}
	defer func() { _ = db.Close() }()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	rec := events.NewRecorder(db)
	svc, err := newApprovals(cfg, db, rec, logger)
	if err != nil {
		return fmt.Errorf("seamlessd.approve: %w", err)
	}
	// Read grants (isolation.requests): the same approve core and proof.
	var grants *approval.Grants
	if cfg.Isolation.Requests {
		grants = approval.NewGrants(approval.GrantsConfig{DB: db, Approvals: svc, Events: rec})
	}
	if err := approveWith(context.Background(), svc, grants, os.Stdout, args); err != nil {
		return fmt.Errorf("seamlessd.approve: %w", err)
	}
	return nil
}

// approverAccountCheck refuses unless this process runs as the daemon's own
// account under peer_auth: the data directory must be this account's and 0700,
// and this account must not be the owner's.
func approverAccountCheck(cfg config.Config, env peerAuthEnv) error {
	pa := cfg.PeerAuth
	if !pa.Enabled && cfg.Isolation.Requests {
		return errors.New("peer_auth is off, so a read grant is approved over the console with the bearer key: run seam approve, which does that")
	}
	if !pa.Enabled {
		return errors.New("peer_auth is off, so there is nothing to approve")
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

// openServedDB opens the database a daemon serves, for a one-shot command:
// approvers, approve and console-open --unlock, run as the daemon's account, and
// family, unmap-repo, retire-project and map-repo. Unlike store.Open it never
// creates or migrates. The schema is the daemon's to move,
// at its own startup, and a newer binary run by hand must not move it under an
// older running daemon as a side effect of something else.
//
// So a database behind this binary is refused. One ahead of it needs no
// migration and is opened with a warning, as serve does, which keeps the
// terminal approval path and `approvers remove` working after a downgrade.
func openServedDB(cfg config.Config, warnings io.Writer) (*sql.DB, error) {
	path := cfg.DBPath()
	db, err := store.OpenExisting(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no database at %s: the daemon creates it when it first starts, so start the daemon, or set SEAMLESS_CONFIG to its config", path)
	}
	if err != nil {
		return nil, err
	}
	applied, err := store.SchemaVersion(db)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("read the schema version of %s: %w", path, err)
	}
	compiled := store.LatestSchemaVersion()
	if applied < compiled {
		_ = db.Close()
		return nil, fmt.Errorf("the database at %s is at schema v%d and this seamlessd expects v%d. "+
			"This command never migrates a database, so nothing changed: "+
			"run the seamlessd the daemon runs, or restart the daemon on this version first", path, applied, compiled)
	}
	if applied > compiled {
		fmt.Fprintf(warnings, "warning: the database at %s is at schema v%d, newer than this seamlessd's v%d: %s; %s\n",
			path, applied, compiled, schemaAheadCause, schemaAheadFix)
	}
	return db, nil
}

// approve is the testable core of runApprove, with read grants off.
func approve(ctx context.Context, svc *approval.Service, out io.Writer, args []string) error {
	return approveWith(ctx, svc, nil, out, args)
}

// approveWith is approve with the read grants (nil while isolation.requests
// is off): a read grant request is decided by them, with the same proof.
func approveWith(ctx context.Context, svc *approval.Service, grants *approval.Grants, out io.Writer, args []string) error {
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
	grant := a.Kind == store.AccessKindGrant
	if *deny {
		if *duration != "" || *digest != "" {
			return errors.New("--deny takes no --for or --digest")
		}
		if err := svc.Deny(ctx, a.ID, "daemon-account"); err != nil {
			return settled(ctx, svc, a, err)
		}
		fmt.Fprintf(out, "Denied %s: %s stays %s that session.\n", approval.ShortID(a.ID), a.Project, closedWord(grant))
		return nil
	}
	if *duration == "" || *digest == "" {
		return errors.New("an approval needs --for and --digest (seam approve supplies both)")
	}
	// Who asks, as the owner was shown: read before the decision, from the
	// facts the digest covers. A failure costs only the name.
	who := askerOf(ctx, svc, a)
	if grant {
		return approveGrant(ctx, svc, grants, out, a, who, *duration, *digest)
	}
	lease, err := svc.Approve(ctx, approval.Decision{ID: a.ID, Duration: *duration, Digest: *digest, Proof: approval.Proof{Kind: approval.ProofDaemon}})
	if err != nil {
		return settled(ctx, svc, a, err)
	}
	fmt.Fprintf(out, "Approved %s: %s may read and write %s %s.\n", approval.ShortID(a.ID), who, a.Project,
		untilWords(*duration, lease.ExpiresAt))
	return nil
}

// approveGrant commits a read grant with the daemon account's proof.
func approveGrant(ctx context.Context, svc *approval.Service, grants *approval.Grants, out io.Writer, a store.AccessApproval,
	who, duration, digest string) error {
	short := approval.ShortID(a.ID)
	if grants == nil {
		return fmt.Errorf("%s is a read grant request, and read grants are off here (isolation.requests): nothing to approve", short)
	}
	g, err := grants.Approve(ctx, approval.Decision{ID: a.ID, Duration: duration, Digest: digest, Proof: approval.Proof{Kind: approval.ProofDaemon}})
	if err != nil {
		return settled(ctx, svc, a, err)
	}
	fmt.Fprintf(out, "Approved %s: %s, working in %s, may read %s (read-only) %s; %s.\n", short, who, a.FromProject,
		a.Project, untilWords(duration, g.ExpiresAt), approval.CopyIntoLine(a.Project, a.FromProject))
	return nil
}

// untilWords says when an approval for duration, ending at expires, ends.
func untilWords(duration string, expires time.Time) string {
	if duration == approval.ForSession {
		return "until the session ends, at the latest " + access.Clock(expires)
	}
	return "until " + access.Clock(expires)
}

// askerOf names who asked, as the facts do: the agent the daemon names and
// its pid, else the pid alone. The program's own path is left out: it is the
// asking process's to choose, and this line goes to the owner's terminal
// unescaped. A request without a process (none under peer_auth) is its
// session's.
func askerOf(ctx context.Context, svc *approval.Service, a store.AccessApproval) string {
	agent := ""
	if f, err := svc.Facts(ctx, a.ID); err == nil {
		agent = f.Agent
	}
	switch {
	case a.PeerPID == 0:
		return "the asking session"
	case agent == "":
		return fmt.Sprintf("pid %d", a.PeerPID)
	default:
		return fmt.Sprintf("%s (pid %d)", agent, a.PeerPID)
	}
}

// closedWord is what a denial leaves the project: a lock's stays locked for
// the session, a confidential project's fence (a read grant) closed to it.
func closedWord(grant bool) string {
	if grant {
		return "closed to"
	}
	return "locked for"
}

// ownerError is a refusal in the owner's own words that still wraps its
// cause, so errors.Is finds the sentinel under the reworded text.
type ownerError struct {
	msg   string
	cause error
}

func (e *ownerError) Error() string { return e.msg }
func (e *ownerError) Unwrap() error { return e.cause }

// settled rewords the decisions the owner can act on, one line each: facts
// that changed since seam approve showed them, a request that expired before
// the decision reached it, one decided meanwhile (it is read again to say
// how). Any other error is returned as it is.
func settled(ctx context.Context, svc *approval.Service, a store.AccessApproval, err error) error {
	short := approval.ShortID(a.ID)
	status := a.Status
	if errors.Is(err, store.ErrApprovalNotPending) {
		if now, rerr := svc.Resolve(ctx, a.ID); rerr == nil {
			status = now.Status
		}
	}
	switch {
	case errors.Is(err, approval.ErrFactsChanged):
		return &ownerError{msg: "the facts of " + short + " changed since they were shown: run seam approve again and look at them first", cause: err}
	case errors.Is(err, store.ErrApprovalExpired), errors.Is(err, store.ErrApprovalNotPending) && status == store.ApprovalExpired:
		return &ownerError{msg: fmt.Sprintf("%s expired at %s, before this decision: nothing changed (the agent asks again with its next call to %s)",
			short, access.Clock(a.ExpiresAt), a.Project), cause: err}
	case errors.Is(err, store.ErrApprovalNotPending) && status != store.ApprovalPending:
		return &ownerError{msg: fmt.Sprintf("%s was already %s: nothing changed", short, status), cause: err}
	}
	return err
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
