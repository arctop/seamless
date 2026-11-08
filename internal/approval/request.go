package approval

// Access requests and the approve core (step 6).
//
// A request names a locked project and the agent process asking for it. The
// agent process -- the anchor -- is the one the session is stamped with (or,
// with no session, the one the request claims), and only when it is a process
// of the connection's kernel-verified chain: the lease that results covers
// that process and everything it started, so the anchor is a fact the daemon
// checked, never a claim it took.
//
// Every approval path ends in Approve: the terminal (a commit by the daemon's
// own account), a passkey assertion, or a Touch ID signature. An acceptance
// without a proof grants nothing, and the facts the owner saw are part of the
// decision: a digest of them must match the facts as they stand.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/passkey"
	"github.com/arctop/seamless/internal/store"
)

// RequestTTL is how long a request waits for the owner's decision.
const RequestTTL = 15 * time.Minute

// Duration choices: until the session ends (the default), one hour, four
// hours. Every one is capped by peer_auth.max_lease.
const (
	ForSession = "session"
	For1h      = "1h"
	For4h      = "4h"
)

// Durations is the canonical set of duration choices, the default first.
var Durations = []string{ForSession, For1h, For4h}

// Proof kinds.
const (
	// ProofDaemon is a commit by the daemon's own account (the terminal path:
	// `seamlessd approve`, reached through sudo). No route can produce it.
	ProofDaemon = "daemon"
	// ProofPasskey is a WebAuthn assertion over the request's challenge.
	ProofPasskey = "passkey"
	// ProofTouchID is a Touch ID key's signature over the decision.
	ProofTouchID = "touchid"
)

// decidedByDaemon is access_approvals.decided_by for the terminal path.
const decidedByDaemon = "daemon-account"

var (
	// ErrNoProcess is a request from a connection whose process chain the
	// kernel did not report (the TCP key, or no chain): no lease could ever
	// apply to it, so none is asked for.
	ErrNoProcess = errors.New("this connection has no verified process to approve")
	// ErrNoAnchor is a request that names no agent process: no session
	// stamped with one, and no claim.
	ErrNoAnchor = errors.New("no agent process named: request access from an agent session")
	// ErrAnchorNotInChain is an agent process that is not a process of the
	// connection's verified chain.
	ErrAnchorNotInChain = errors.New("the agent process is not an ancestor of this connection")
	// ErrNotLocked is a request for a project that is not locked.
	ErrNotLocked = errors.New("project is not locked")
	// ErrFactsChanged is a decision over facts that changed since they were
	// shown: the owner must look again.
	ErrFactsChanged = errors.New("the request's facts changed since they were shown")
	// ErrNoProof is an acceptance without a proof the core can check.
	ErrNoProof = errors.New("an approval needs a proof")
	// ErrDuration is a duration that is not one of Durations.
	ErrDuration = errors.New("invalid duration")
	// ErrIsGrant is a lease approval of a read grant request: a grant is
	// decided by Grants.Approve and never becomes a lease.
	ErrIsGrant = errors.New("this is a read grant request, not a lock's")
	// ErrRestricted is a restricted local account (peer_auth.restricted)
	// filing or deciding an approval. Its reach is the open projects and
	// nothing else: no request of its is filed, no lease or read grant ever
	// names it, and it decides none.
	ErrRestricted = errors.New("a restricted local account files no access requests and holds no lease or read " +
		"grant: it reaches the open projects only, and the owner decides every approval")
)

// refuseRestricted is the restricted-account refusal at every entry point of
// the approval service -- filing a request or a read grant, deciding one,
// enrolling an approver, unlocking a console -- for a call whose context, or
// whose stated view, speaks for a restricted account. The access layer
// already keeps such an account out (View.Locked is the owner's alone,
// JudgeGrants keeps owner grants and WithLeases owner leases, the console is
// owner-only), so this is defense in depth: the caller gets a plain refusal,
// and no access_approvals row is written.
func refuseRestricted(ctx context.Context, views ...access.View) error {
	if access.AccountOf(ctx) != "" {
		return ErrRestricted
	}
	for _, v := range views {
		if v.Account() != "" {
			return ErrRestricted
		}
	}
	return nil
}

// The access a decision opens, as the facts name it.
const (
	accessReadWrite = "read-write"
	accessReadOnly  = "read-only"
)

// ShortID is a request's short form for a person: R- and the last four
// symbols of its id.
func ShortID(id string) string {
	if len(id) < 4 {
		return "R-" + id
	}
	return "R-" + id[len(id)-4:]
}

// Facts is everything the owner is shown about one request -- the daemon's
// facts, never the agent's, except Reason, which is labeled as the agent's.
// Agent is the readable name of the agent Program is ("Claude Code 2.1.319",
// "Codex"), derived from it by the anchor rule's classifier (anchor.go); ""
// for any other program, and a surface then shows the program's base name,
// or the pid when there is none.
type Facts struct {
	ID          string    `json:"id"`
	Short       string    `json:"short_id"`
	Project     string    `json:"project"`
	Isolation   string    `json:"isolation"`
	Access      string    `json:"access"`
	Kind        string    `json:"kind"`
	Program     string    `json:"program"`
	Agent       string    `json:"agent,omitempty"`
	PID         int32     `json:"pid"`
	UID         uint32    `json:"uid"`
	Session     string    `json:"session"`
	SessionID   string    `json:"session_id"`
	CWD         string    `json:"cwd"`
	Repo        string    `json:"repo"`
	Model       string    `json:"model"`
	Reason      string    `json:"reason"`
	Warnings    []Warning `json:"warnings"`
	RequestedAt time.Time `json:"requested_at"`
	ExpiresAt   time.Time `json:"expires_at"`
	Digest      string    `json:"digest"`
	// From is a read grant request's asking project (isolation.requests):
	// the grant would open Project to sessions working there. Empty, and
	// absent from the JSON, for a lock's request.
	From string `json:"from,omitempty"`
}

// Warning is one thing the owner should weigh before approving: a code and
// its text.
type Warning struct {
	Code string `json:"code"`
	Text string `json:"text"`
}

// Warning codes. WarnAnchorIsShell is a request whose anchor is a broad
// ancestor (a shell, multiplexer, terminal, login process: anchor.go).
// Filing refuses such an anchor and the approve core never approves one, so
// it is shown only on a request an older daemon filed: it says why the
// approval will be refused. The code is kept for the surfaces that know it.
const (
	WarnCWDOutside     = "cwd_outside_repo"
	WarnTainted        = "session_tainted"
	WarnOtherPending   = "other_pending"
	WarnAnchorIsShell  = "anchor_is_shell"
	WarnNoSession      = "no_session"
	WarnRefreshOfLease = "refresh"
)

// WarningOrder is the order the facts serve their warnings in, most serious
// first: a surface that shows only the first few (the Touch ID prompt shows
// three) shows these. Reach comes first, then whether the ask is the agent's
// own, then how much is known about the asker:
//
//   - anchor_is_shell: a lease on that process would cover every program it
//     started, other agent sessions included (and is refused);
//   - copy_into: an approved read lets the project's content leave, into
//     another project's memories;
//   - session_tainted: the session read text from outside, so the request and
//     its reason may be an injection rather than the agent's own;
//   - other_pending: the same agent asks for another project too, so
//     approving both widens its reach further;
//   - cwd_outside_repo: the session works outside the project's repository,
//     so the work may not need the project at all;
//   - no_session: no session names the agent, so its cwd and model are
//     unknown;
//   - refresh: the agent holds a lease on the project already; approving
//     replaces it and adds no reach.
var WarningOrder = []string{
	WarnAnchorIsShell, WarnCopyInto, WarnTainted, WarnOtherPending, WarnCWDOutside, WarnNoSession, WarnRefreshOfLease,
}

// warningRank is a warning's place in WarningOrder; a code it does not list
// sorts last.
func warningRank(code string) int {
	if i := slices.Index(WarningOrder, code); i >= 0 {
		return i
	}
	return len(WarningOrder)
}

// digestOf is the SHA-256 of the facts' canonical JSON, the digest blanked.
func digestOf(f Facts) string {
	f.Digest = ""
	b, err := json.Marshal(f)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// RequestInput is one request for access to a locked project.
type RequestInput struct {
	Project string
	// View is the request's view: the owner's socket view with its kernel
	// chain (access.Peer.Chain).
	View access.View
	// SessionID is the session the call is bound to, "" when none.
	SessionID string
	// AgentProcess is the agent process the request claims (the
	// X-Seamless-Agent-Process header), used only when no session names one,
	// and only if it is a process of the chain.
	AgentProcess string
	// Host is the caller's machine (X-Seamless-Host), used with AgentProcess
	// to find the session that agent stamped when no binding names one.
	Host string
	// Reason is the agent's own words, at most store.MaxAccessReasonRunes.
	Reason string
	// Via says how it was filed: "refusal" (automatic) or "request".
	Via string
}

// Request files (or finds) the pending request for in.Project from the
// anchor in.View's chain names. A pending request of that anchor for that
// project is returned as it is, so a repeated refusal never piles up
// requests. A request from a tree that already holds a live lease is a
// refresh, and carries the earlier reason rather than new text.
func (s *Service) Request(ctx context.Context, in RequestInput) (store.AccessApproval, error) {
	const op = "approval.Request"
	if err := refuseRestricted(ctx, in.View); err != nil {
		return store.AccessApproval{}, fmt.Errorf("%s: %w", op, err)
	}
	now := s.cfg.Now()
	chain := in.View.Principal().Chain
	if len(chain) == 0 || !in.View.Principal().Socket {
		return store.AccessApproval{}, fmt.Errorf("%s: %w", op, ErrNoProcess)
	}
	_, leased := in.View.Lease(in.Project)
	if !in.View.Locked(in.Project) && !leased {
		return store.AccessApproval{}, fmt.Errorf("%s: %w: %s", op, ErrNotLocked, in.Project)
	}
	got, err := s.anchorFor(ctx, chain, in.SessionID, in.AgentProcess, in.Host)
	if err != nil {
		return store.AccessApproval{}, fmt.Errorf("%s: %w", op, err)
	}
	anchor, sess := got.token, got.sess
	in.SessionID = got.sessionID

	pending, ok, err := store.PendingAccessApproval(ctx, s.cfg.DB, anchor.Key(), in.Project, now)
	if err != nil {
		return store.AccessApproval{}, fmt.Errorf("%s: %w", op, err)
	}
	if ok {
		return pending, nil
	}

	kind, reason := store.AccessKindRequest, strings.TrimSpace(in.Reason)
	if leased {
		kind = store.AccessKindRefresh
		if reason, err = store.LastAccessReason(ctx, s.cfg.DB, anchor.Key(), in.Project); err != nil {
			return store.AccessApproval{}, fmt.Errorf("%s: %w", op, err)
		}
	}
	if utf8.RuneCountInString(reason) > store.MaxAccessReasonRunes {
		return store.AccessApproval{}, fmt.Errorf("%s: %w: the reason is limited to %d characters", op,
			store.ErrInvalidAccessRequest, store.MaxAccessReasonRunes)
	}
	// The program is the kernel's path for the anchor, else its name for it
	// (another account's path may be unreadable); the facts show the pid
	// alone when neither is known.
	program := got.program
	var cwd, model string
	if in.SessionID != "" {
		if row, ok, err := store.SessionByID(ctx, s.cfg.DB, in.SessionID); err == nil && ok {
			cwd, model = row.CWD, row.Model
		}
	}
	req := store.AccessRequest{
		Project: in.Project, Kind: kind, Lease: s.leaseCap(), TTL: RequestTTL,
		PeerUID: in.View.Principal().UID, PeerPID: anchor.PID, ProcessKey: anchor.Key(), Program: program,
		SessionID: in.SessionID, SessionName: sess.Name, CWD: cwd, Repo: repoName(cwd), Model: model, Reason: reason,
	}
	a, err := store.CreateAccessApproval(ctx, s.cfg.DB, req, now)
	if err != nil {
		return store.AccessApproval{}, fmt.Errorf("%s: %w", op, err)
	}
	via := in.Via
	if via == "" {
		via = "request"
	}
	s.record(ctx, core.Event{Kind: core.EventAccessRequested, ItemID: a.ID, SessionID: in.SessionID, ProjectSlug: in.Project,
		Payload: map[string]any{"kind": kind, "via": via}})
	return a, nil
}

// resolvedAnchor is a request's anchor as filing settles it: the process,
// the session it settled on ("" for none) and that session's process row,
// and the program the kernel names for the process.
type resolvedAnchor struct {
	token     access.ProcessToken
	sessionID string
	sess      store.SessionProcess
	program   string
}

// anchorFor resolves a request's anchor (resolveAnchor) and applies the
// anchor rule to it (anchor.go): an anchor that is not an agent's is an
// *AnchorError, and nothing is filed.
func (s *Service) anchorFor(ctx context.Context, chain []access.ProcessToken, sessionID, agentProcess, host string) (resolvedAnchor, error) {
	i, sessionID, sess, err := s.resolveAnchor(ctx, chain, sessionID, agentProcess, host)
	if err != nil {
		return resolvedAnchor{}, err
	}
	tok := chain[i]
	stamped := sess.AgentProcess != "" && sess.AgentProcess == tok.AgentID()
	program, err := judgeAnchor(chain, i, s.cfg.Programs, stamped)
	if err != nil {
		return resolvedAnchor{}, err
	}
	return resolvedAnchor{token: tok, sessionID: sessionID, sess: sess, program: program}, nil
}

// resolveAnchor finds the agent process a request is anchored to: the one
// the session is stamped with, else the one the request claims, and only when
// it is a process of the connection's kernel-verified chain. With no session
// named, the session the agent's SessionStart hook stamped with the claimed
// process stands in, when exactly one is live. It returns the anchor's index
// in the chain, the session it settled on ("" for none) and that session's
// process row.
func (s *Service) resolveAnchor(ctx context.Context, chain []access.ProcessToken, sessionID, agentProcess, host string) (int, string, store.SessionProcess, error) {
	if sessionID == "" && agentProcess != "" {
		// No binding: the session the agent's SessionStart hook stamped with
		// this process, when exactly one is live.
		if found, err := store.ActiveAmbientByAgentProcess(ctx, s.cfg.DB, host, agentProcess); err == nil && len(found) == 1 {
			sessionID = found[0].ID
		}
	}
	var sess store.SessionProcess
	anchorID := agentProcess
	if sessionID != "" {
		got, err := store.SessionAgentProcesses(ctx, s.cfg.DB, []string{sessionID})
		if err != nil {
			return 0, "", store.SessionProcess{}, err
		}
		if p, ok := got[sessionID]; ok {
			sess = p
			if p.AgentProcess != "" {
				anchorID = p.AgentProcess
			}
		}
	}
	if anchorID == "" {
		return 0, "", store.SessionProcess{}, ErrNoAnchor
	}
	for i, t := range chain {
		if t.AgentID() == anchorID {
			return i, sessionID, sess, nil
		}
	}
	return 0, "", store.SessionProcess{}, ErrAnchorNotInChain
}

// repoName is the repository a cwd sits in, for display: its last element.
func repoName(cwd string) string {
	if cwd == "" {
		return ""
	}
	return filepath.Base(cwd)
}

func (s *Service) leaseCap() time.Duration {
	if s.cfg.LeaseCap > 0 {
		return s.cfg.LeaseCap
	}
	return 8 * time.Hour
}

// LeaseLength turns a duration choice into the lease's length, capped by
// peer_auth.max_lease. "session" is the cap itself: the session's end
// revokes the lease earlier (RevokeSession).
func (s *Service) LeaseLength(choice string) (time.Duration, error) {
	capped := s.leaseCap()
	var d time.Duration
	switch choice {
	case ForSession:
		return capped, nil
	case For1h:
		d = time.Hour
	case For4h:
		d = 4 * time.Hour
	default:
		return 0, fmt.Errorf("%w %q: valid values are %s", ErrDuration, choice, strings.Join(Durations, ", "))
	}
	return min(d, capped), nil
}

// Facts returns what the owner is shown about request id, with its digest.
// The warnings are computed now, so a fact that changed -- another request
// from the same tree, a mark on the session -- changes the digest.
func (s *Service) Facts(ctx context.Context, id string) (Facts, error) {
	const op = "approval.Facts"
	a, err := store.AccessApprovalByID(ctx, s.cfg.DB, id)
	if err != nil {
		return Facts{}, fmt.Errorf("%s: %w", op, err)
	}
	return s.factsOf(ctx, a)
}

func (s *Service) factsOf(ctx context.Context, a store.AccessApproval) (Facts, error) {
	const op = "approval.Facts"
	iso, err := store.IsolationOf(ctx, s.cfg.DB, a.Project)
	if err != nil {
		return Facts{}, fmt.Errorf("%s: %w", op, err)
	}
	isoWord := string(iso)
	if isoWord == "" {
		isoWord = string(core.IsolationOpen)
	}
	grant := a.Kind == store.AccessKindGrant
	f := Facts{
		ID: a.ID, Short: ShortID(a.ID), Project: a.Project, Isolation: isoWord, Access: accessReadWrite, Kind: a.Kind,
		Program: a.Program, Agent: agentName(a.Program), PID: a.PeerPID, UID: a.PeerUID, Session: a.SessionName, SessionID: a.SessionID,
		CWD: a.CWD, Repo: a.Repo, Model: a.Model, Reason: a.Reason, RequestedAt: a.RequestedAt, ExpiresAt: a.ExpiresAt,
	}
	// A read grant opens the project read-only to the asking project, and its
	// repository is the one the session should be working in.
	home := a.Project
	if grant {
		f.Access, f.From, home = accessReadOnly, a.FromProject, a.FromProject
	}
	warn := func(code, text string) { f.Warnings = append(f.Warnings, Warning{Code: code, Text: text}) }
	if grant {
		warn(WarnCopyInto, CopyIntoLine(a.Project, a.FromProject))
	}
	if a.Kind == store.AccessKindRefresh {
		warn(WarnRefreshOfLease, "this session already holds a lease on the project; approving replaces it")
	}
	if what, broad := storedBroad(a.PeerPID, a.Program); broad {
		warn(WarnAnchorIsShell, "the process asking is "+what+", not an agent: a lease on it would cover every program "+
			"it started, so Seamless refuses to approve it")
	}
	if a.SessionID == "" {
		warn(WarnNoSession, "no session named this agent; its cwd and model are unknown")
	}
	if a.CWD != "" && s.cfg.Host != "" {
		roots, err := store.RepoRootsForProject(ctx, s.cfg.DB, s.cfg.Host)
		if err != nil {
			return Facts{}, fmt.Errorf("%s: %w", op, err)
		}
		inside := false
		for _, root := range roots[home] {
			if rel, err := filepath.Rel(root, a.CWD); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				inside = true
			}
		}
		if !inside {
			warn(WarnCWDOutside, "the session works outside "+home+"'s repository")
		}
	}
	if a.SessionID != "" {
		if _, tainted, err := store.SessionTaintOf(ctx, s.cfg.DB, a.SessionID); err != nil {
			return Facts{}, fmt.Errorf("%s: %w", op, err)
		} else if tainted {
			warn(WarnTainted, "the session read text from outside (a web page, a file it was handed): its reason may not be its own")
		}
	}
	pending, err := store.PendingAccessApprovals(ctx, s.cfg.DB, s.cfg.Now())
	if err != nil {
		return Facts{}, fmt.Errorf("%s: %w", op, err)
	}
	for _, p := range pending {
		if p.ID != a.ID && sameAsker(p, a) {
			warn(WarnOtherPending, "the same agent is also asking for "+p.Project)
			break
		}
	}
	slices.SortStableFunc(f.Warnings, func(x, y Warning) int { return warningRank(x.Code) - warningRank(y.Code) })
	f.Digest = digestOf(f)
	return f, nil
}

// sameAsker reports whether two requests come from one asker: one agent
// process under peer_auth, else one session (a read grant request without
// peer_auth names no process). Two requests that name neither are not known
// to share anything.
func sameAsker(a, b store.AccessApproval) bool {
	if a.ProcessKey != "" || b.ProcessKey != "" {
		return a.ProcessKey == b.ProcessKey
	}
	return a.SessionID != "" && a.SessionID == b.SessionID
}

// Pending returns the facts of every pending request, oldest first.
func (s *Service) Pending(ctx context.Context) ([]Facts, error) {
	rows, err := store.PendingAccessApprovals(ctx, s.cfg.DB, s.cfg.Now())
	if err != nil {
		return nil, fmt.Errorf("approval.Pending: %w", err)
	}
	out := make([]Facts, 0, len(rows))
	for _, a := range rows {
		f, err := s.factsOf(ctx, a)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}

// Resolve finds a pending request by its id or short id (R-7F3K, any case).
// A short id two pending requests share is refused as ambiguous.
func (s *Service) Resolve(ctx context.Context, ref string) (store.AccessApproval, error) {
	ref = strings.TrimSpace(ref)
	if a, err := store.AccessApprovalByID(ctx, s.cfg.DB, ref); err == nil {
		return a, nil
	}
	rows, err := store.PendingAccessApprovals(ctx, s.cfg.DB, s.cfg.Now())
	if err != nil {
		return store.AccessApproval{}, fmt.Errorf("approval.Resolve: %w", err)
	}
	var hits []store.AccessApproval
	for _, a := range rows {
		if strings.EqualFold(ShortID(a.ID), ref) {
			hits = append(hits, a)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return store.AccessApproval{}, fmt.Errorf("approval.Resolve: %w: %q", store.ErrApprovalNotFound, ref)
	default:
		return store.AccessApproval{}, fmt.Errorf("approval.Resolve: %q names %d pending requests: use the full id", ref, len(hits))
	}
}

// Proof is what decides an approval.
type Proof struct {
	Kind string
	// Assertion is a passkey's response JSON (ProofPasskey).
	Assertion []byte
	// CredentialID is the Touch ID key's store id, Signature its ASN.1 ECDSA
	// signature over TouchIDApproveMessage (ProofTouchID).
	CredentialID string
	Signature    []byte
}

// Decision is one approval: the request, the duration chosen, the digest of
// the facts the owner saw, and the proof.
type Decision struct {
	ID       string
	Duration string
	Digest   string
	Proof    Proof
}

// TouchIDApproveMessage is what a Touch ID key signs to approve a request:
// the request, its challenge, the facts digest, the access and the duration.
func TouchIDApproveMessage(id string, challenge []byte, digest, accessWord, duration string) []byte {
	msg := []byte("seamless touchid approval v1\x00")
	for _, part := range [][]byte{[]byte(id), challenge, []byte(digest), []byte(accessWord), []byte(duration)} {
		msg = append(msg, part...)
		msg = append(msg, 0)
	}
	return msg
}

// Approve is the approve core: every path ends here. It checks the facts
// digest against the facts as they stand, verifies the proof, and grants the
// lease through store.ApproveAccessFor (single use, bound to the challenge).
func (s *Service) Approve(ctx context.Context, d Decision) (store.AccessLease, error) {
	const op = "approval.Approve"
	if err := refuseRestricted(ctx); err != nil {
		return store.AccessLease{}, fmt.Errorf("%s: %w", op, err)
	}
	length, err := s.LeaseLength(d.Duration)
	if err != nil {
		return store.AccessLease{}, fmt.Errorf("%s: %w", op, err)
	}
	a, err := store.AccessApprovalByID(ctx, s.cfg.DB, d.ID)
	if err != nil {
		return store.AccessLease{}, fmt.Errorf("%s: %w", op, err)
	}
	if a.Kind == store.AccessKindGrant {
		// A read grant is decided by Grants.Approve, and never becomes a lease.
		return store.AccessLease{}, fmt.Errorf("%s: %w: %s", op, ErrIsGrant, ShortID(a.ID))
	}
	if a.Status != store.ApprovalPending {
		return store.AccessLease{}, fmt.Errorf("%s: %w: %s is %s", op, store.ErrApprovalNotPending, ShortID(a.ID), a.Status)
	}
	// The anchor rule, again: a request an older daemon filed may name a
	// broad ancestor, and no proof makes a lease on one safe.
	if err := storedAnchorRefusal(a.PeerPID, a.Program); err != nil {
		return store.AccessLease{}, fmt.Errorf("%s: %s: %w", op, ShortID(a.ID), err)
	}
	facts, err := s.factsOf(ctx, a)
	if err != nil {
		return store.AccessLease{}, fmt.Errorf("%s: %w", op, err)
	}
	if d.Digest == "" || d.Digest != facts.Digest {
		return store.AccessLease{}, fmt.Errorf("%s: %w", op, ErrFactsChanged)
	}
	now := s.cfg.Now()
	decidedBy, err := s.prove(ctx, a, facts, d, now)
	if err != nil {
		return store.AccessLease{}, fmt.Errorf("%s: %w", op, err)
	}
	lease, err := store.ApproveAccessFor(ctx, s.cfg.DB, a.ID, a.Challenge, decidedBy, length, now)
	if err != nil {
		return store.AccessLease{}, fmt.Errorf("%s: %w", op, err)
	}
	s.record(ctx, core.Event{Kind: core.EventAccessApproved, ItemID: a.ID, SessionID: a.SessionID, ProjectSlug: a.Project,
		Payload: map[string]any{"lease": lease.ID, "proof": d.Proof.Kind, "seconds": int64(length / time.Second), "duration": d.Duration}})
	return lease, nil
}

// prove verifies a decision's proof over request a, whose facts (and their
// digest) the owner was shown, and returns who decided: the daemon's account,
// or the approver credential that signed. It is the one proof check of the
// approve core, for a lock's lease and a read grant alike.
func (s *Service) prove(ctx context.Context, a store.AccessApproval, facts Facts, d Decision, now time.Time) (string, error) {
	switch d.Proof.Kind {
	case ProofDaemon:
		return decidedByDaemon, nil
	case ProofPasskey:
		if !s.PasskeysOn() {
			return "", ErrPasskeysOff
		}
		rows, err := store.ApproverCredentials(ctx, s.cfg.DB, store.ApproverPasskey)
		if err != nil {
			return "", err
		}
		allowed := make([]passkey.Credential, len(rows))
		for i, r := range rows {
			allowed[i] = ToPasskey(r)
		}
		used, err := s.cfg.Passkeys.FinishAssertion(a.Challenge, allowed, d.Proof.Assertion)
		if err != nil {
			return "", err
		}
		row, ok := rowFor(rows, used.ID)
		if !ok {
			return "", errors.New("unknown passkey")
		}
		if err := store.RecordApproverUse(ctx, s.cfg.DB, row.ID, row.SignCount, used.SignCount, used.BackupState, now); err != nil {
			return "", err
		}
		return row.ID, nil
	case ProofTouchID:
		row, err := store.ApproverByID(ctx, s.cfg.DB, d.Proof.CredentialID)
		if err != nil || row.Kind != store.ApproverTouchID {
			return "", fmt.Errorf("%w: no such Touch ID key", ErrTouchIDKey)
		}
		msg := TouchIDApproveMessage(a.ID, a.Challenge, d.Digest, facts.Access, d.Duration)
		if err := VerifyTouchID(row.PublicKey, msg, d.Proof.Signature); err != nil {
			return "", err
		}
		if err := store.RecordApproverUse(ctx, s.cfg.DB, row.ID, row.SignCount, row.SignCount, false, now); err != nil {
			return "", err
		}
		return row.ID, nil
	default:
		return "", ErrNoProof
	}
}

// Deny ends a pending request without a lease. by names who decided (a
// proof kind); a denial needs no cryptographic proof -- it only ever closes.
func (s *Service) Deny(ctx context.Context, id, by string) error {
	if err := refuseRestricted(ctx); err != nil {
		return fmt.Errorf("approval.Deny: %w", err)
	}
	a, err := store.AccessApprovalByID(ctx, s.cfg.DB, id)
	if err != nil {
		return fmt.Errorf("approval.Deny: %w", err)
	}
	if err := store.DenyAccess(ctx, s.cfg.DB, a.ID, by, s.cfg.Now()); err != nil {
		return fmt.Errorf("approval.Deny: %w", err)
	}
	s.record(ctx, core.Event{Kind: core.EventAccessDenied, ItemID: a.ID, SessionID: a.SessionID, ProjectSlug: a.Project,
		Payload: map[string]any{"proof": by}})
	return nil
}

// RevokeSession ends every live lease a session holds: its session_end. It
// returns how many.
func (s *Service) RevokeSession(ctx context.Context, sessionID string) (int64, error) {
	if sessionID == "" {
		return 0, nil
	}
	n, err := store.RevokeAccessLeasesForSession(ctx, s.cfg.DB, sessionID, store.LeaseRevokedSessionEnded, s.cfg.Now())
	if err != nil {
		return 0, fmt.Errorf("approval.RevokeSession: %w", err)
	}
	if n > 0 {
		s.record(ctx, core.Event{Kind: core.EventAccessRevoked, SessionID: sessionID,
			Payload: map[string]any{"reason": store.LeaseRevokedSessionEnded, "leases": n}})
	}
	return n, nil
}

// AssertionOptions returns the navigator.credentials.get options that
// approve request id with a passkey: its own challenge, every enrolled
// passkey allowed, user verification required.
func (s *Service) AssertionOptions(ctx context.Context, id string) ([]byte, error) {
	if !s.PasskeysOn() {
		return nil, fmt.Errorf("approval.AssertionOptions: %w", ErrPasskeysOff)
	}
	a, err := store.AccessApprovalByID(ctx, s.cfg.DB, id)
	if err != nil {
		return nil, fmt.Errorf("approval.AssertionOptions: %w", err)
	}
	creds, err := s.passkeys(ctx)
	if err != nil {
		return nil, fmt.Errorf("approval.AssertionOptions: %w", err)
	}
	opts, err := s.cfg.Passkeys.BeginAssertion(a.Challenge, creds)
	if err != nil {
		return nil, fmt.Errorf("approval.AssertionOptions: %w", err)
	}
	return opts, nil
}

// Page returns the passkey approval page for a request, "" when passkeys are
// not set up.
func (s *Service) Page(id string) string {
	if !s.PasskeysOn() || len(s.cfg.Origins) == 0 {
		return ""
	}
	return s.cfg.Origins[0] + "/console/access/" + id
}

// Ways returns the approval paths this daemon offers the owner for request
// id, as the remedies name them (access.Ways): Touch ID while a Touch ID key
// is enrolled, and the request's passkey page while passkeys are set up and
// a passkey is enrolled -- a page no passkey can sign is no way in. With id
// "" (no request yet) Passkey says whether a page would be one. The owner's
// own terminal is always a way and is not part of it. A lookup that fails
// leaves its path out: a remedy may offer less, never a path that is not
// there.
func (s *Service) Ways(ctx context.Context, id string) access.Ways {
	var w access.Ways
	if keys, err := store.ApproverCredentials(ctx, s.cfg.DB, store.ApproverTouchID); err == nil && len(keys) > 0 {
		w.TouchID = true
	}
	if s.PasskeysOn() && len(s.cfg.Origins) > 0 {
		if keys, err := store.ApproverCredentials(ctx, s.cfg.DB, store.ApproverPasskey); err == nil && len(keys) > 0 {
			w.Passkey = true
			if id != "" {
				w.Page = s.Page(id)
			}
		}
	}
	return w
}

func (s *Service) record(ctx context.Context, e core.Event) {
	if s.cfg.Events == nil {
		return
	}
	if _, err := s.cfg.Events.Record(context.WithoutCancel(ctx), e); err != nil {
		s.cfg.Logger.Warn("approval: record event", "kind", e.Kind, "error", err)
	}
}
