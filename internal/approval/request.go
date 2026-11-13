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
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
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
	// ProofPasskey is a WebAuthn assertion over PasskeyApproveChallenge: the
	// request's challenge, the facts digest, the access and the duration.
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
	// ErrBindingChanged is a passkey assertion made for this request with
	// another duration than the decision names, or over the request's bare
	// challenge (a page an older daemon served): what it signed is not what
	// is being approved. It is always returned beside ErrFactsChanged, so a
	// surface that knows only that one refuses it the same way.
	ErrBindingChanged = errors.New("the passkey approved another duration, or other facts, than this decision names")
	// ErrAccessLive is a request from an asker that already holds live access
	// to the project -- an agent tree's lease, or a read grant from the same
	// project -- more than access.ExpiryWarning from its end: it covers what
	// is asked, so nothing is filed (*AccessLiveError names it). From the
	// warning on, a request files a new one -- a lease's refresh, a new read
	// grant request -- as the expiry notices say: nothing extends access
	// silently.
	ErrAccessLive = errors.New("this agent already holds access to the project")
	// ErrDenied is a refusal's request (ViaRefusal) from an asker whose last
	// request for the project the owner denied: nothing new is filed
	// (*DeniedError names the denial), so a refused call never asks the owner
	// again behind their back, and the agent hears that it was denied. The
	// asker's explicit request (ViaRequest: access_request, seam access
	// request) always files a new one -- what the owner, having denied a wrong
	// request, asks the agent to send.
	ErrDenied = errors.New("the owner denied this agent's last request for the project")
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

// AccessLiveError is ErrAccessLive with the access that answers the request:
// a lock's lease, or (From set) a read grant; its project, when it ends, and
// the approved request that opened it.
type AccessLiveError struct {
	Project string
	// From is a read grant's asking project; "" for a lease.
	From string
	// AccessID is the lease's or the grant's id.
	AccessID string
	// ApprovalID is the approved request behind it, "" when it could not be
	// read.
	ApprovalID string
	Expires    time.Time
}

func (e *AccessLiveError) Error() string {
	return fmt.Sprintf("%s: project %s is open to it until %s", ErrAccessLive, e.Project, e.Expires.UTC().Format(time.RFC3339))
}

func (e *AccessLiveError) Unwrap() error { return ErrAccessLive }

// AskAgainAt is when a request from the same asker files a new one again:
// access.ExpiryWarning before the access ends, when the expiry notices start.
func (e *AccessLiveError) AskAgainAt() time.Time { return e.Expires.Add(-access.ExpiryWarning) }

// Answer is the request's answer for the agent that asked: what is open,
// until when, and from when asking again files a new approval.
func (e *AccessLiveError) Answer() string {
	by := ""
	if e.ApprovalID != "" {
		by = " (approved as " + ShortID(e.ApprovalID) + ")"
	}
	open := "project " + e.Project + " is already open to this agent"
	if e.From != "" {
		open = "project " + e.Project + " is already readable from project " + e.From
	}
	return fmt.Sprintf("%s until %s%s, so nothing was filed. Nothing extends it silently: from %s, %d minutes before "+
		"it ends, asking again files a new approval", open, access.Clock(e.Expires), by, access.Clock(e.AskAgainAt()),
		int(access.ExpiryWarning/time.Minute))
}

// DeniedError is ErrDenied with the request the owner denied.
type DeniedError struct {
	Request store.AccessApproval
}

func (e *DeniedError) Error() string {
	return fmt.Sprintf("%s: %s %s was denied", ErrDenied, e.What(), ShortID(e.Request.ID))
}

func (e *DeniedError) Unwrap() error { return ErrDenied }

// What names the denied request's kind for an agent: "access request",
// "refresh request", "read request", "read-and-write request" (a grant that
// asked for write) or "move request".
func (e *DeniedError) What() string {
	switch e.Request.Kind {
	case store.AccessKindRefresh:
		return "refresh request"
	case store.AccessKindGrant:
		if e.Request.RequestedAccess == store.GrantAccessWrite {
			return "read-and-write request"
		}
		return "read request"
	case store.AccessKindMove:
		return "move request"
	}
	return "access request"
}

// At is when the owner denied it; zero when the row does not say.
func (e *DeniedError) At() time.Time {
	if e.Request.DecidedAt == nil {
		return time.Time{}
	}
	return *e.Request.DecidedAt
}

// LockRequest is the denial as a lock refusal names it (access.LockRequest's
// Denied): the request, its kind, and when it was denied.
func (e *DeniedError) LockRequest() access.LockRequest {
	return access.LockRequest{ID: e.Request.ID, Short: ShortID(e.Request.ID), Denied: true, DeniedAt: e.At(), DeniedWhat: e.What()}
}

// How a request was filed (RequestInput.Via, GrantInput.Via).
const (
	// ViaRefusal is a request a refused call filed by itself: it has no
	// reason to give, and after a denial it files nothing (ErrDenied).
	ViaRefusal = "refusal"
	// ViaRequest is the agent's explicit ask (access_request, seam access
	// request), with its reason: it always files, a denial or not, and fills
	// in the reason of a pending request a refusal filed without one.
	ViaRequest = "request"
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
	// the grant would open Project to sessions working there. For a move it
	// is the home the conversation leaves (empty for the global scope).
	// Empty, and absent from the JSON, for a lock's request.
	From string `json:"from,omitempty"`
	// Carry is what a grant or move request states (isolation.requests,
	// carry.go): the home, every fenced project the conversation holds once
	// approved and every project it may write to once approved -- for a
	// grant as its default, read-only, approval binds it (ReadWrite says what
	// read and write adds). Absent for a lock's request, and for a grant that
	// states none (one asked from an open project, or filed before migration
	// 039).
	Carry *CarryFacts `json:"carry,omitempty"`
	// AccessChoices are the accesses the owner may approve, the default
	// first: read-only and read-write for a grant between confidential
	// projects that states its carry. Absent where the access is Access alone.
	AccessChoices []string `json:"access_choices,omitempty"`
	// AskedAccess is the access the agent asked for, where the owner chooses
	// (AccessChoices): read-only or read-write. The default is read-only
	// whatever was asked.
	AskedAccess string `json:"asked_access,omitempty"`
	// ReadWrite is what approving read and write lets happen, as one line
	// (ReadWriteLine), where AccessChoices offers it.
	ReadWrite string `json:"read_write,omitempty"`
	// Locked marks a grant or move whose target is locked: approving it also
	// opens the lock to this agent's process tree (the target's lease) for
	// the duration chosen. Absent for a lock's own request, which is nothing
	// else.
	Locked bool `json:"locked,omitempty"`
}

// SelfReportedNote says, on every approval surface that shows them, that the
// working directory, the repository and the model are the agent's own report:
// the daemon has no way to check them, so an agent can claim the locked
// project's repository, and a missing cwd_outside_repo or no_session warning is
// not evidence that the claim is true. What the daemon does know is the project,
// the access, the process and the program the kernel names.
const SelfReportedNote = "The working directory, repository and model are what the agent reported. The daemon cannot check them."

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
	WarnAnchorRuntime  = "anchor_is_runtime"
	WarnNoSession      = "no_session"
	WarnRefreshOfLease = "refresh"
	// WarnMove is a move request's line (MoveLine): what the move carries
	// and where the agent can no longer write.
	WarnMove = "move"
)

// WarningOrder is the order the facts serve their warnings in, most serious
// first: a surface that shows only the first few (the Touch ID prompt shows
// three) shows these. Reach comes first, then whether the ask is the agent's
// own, then how much is known about the asker:
//
//   - anchor_is_shell: a lease on that process would cover every program it
//     started, other agent sessions included (and is refused);
//   - anchor_is_runtime: the process is a JS runtime that only its own hook
//     calls an agent, and an editor's server is one too, so one approval could
//     cover everything that server started;
//   - move: an approved move takes the agent into the project for good, with
//     everything it carries, and out of every place it could write before;
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
	WarnAnchorIsShell, WarnAnchorRuntime, WarnMove, WarnCopyInto, WarnTainted, WarnOtherPending, WarnCWDOutside, WarnNoSession, WarnRefreshOfLease,
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
	// Via says how it was filed: ViaRefusal (automatic) or ViaRequest (the
	// default).
	Via string
}

// Request files (or finds) the pending request for in.Project from the
// anchor in.View's chain names. A pending request of that anchor for that
// project is returned as it is, so a repeated refusal never piles up
// requests -- except that an explicit request's reason fills in a pending
// one's missing reason (store.FillAccessReason). A refusal whose anchor's
// last request for the project the owner denied files nothing: the answer is
// a *DeniedError, until an explicit request files a new one. A tree that
// already holds a live lease on the project files nothing while more than
// access.ExpiryWarning of it remains: the answer is a *AccessLiveError naming
// the lease. Within the warning it files a refresh, which carries the reason
// this ask gave, labeled as the agent's own words like any request (owner
// decision 2026-11-09); given none, it shows "(no reason given)".
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
	lease, leased := in.View.Lease(in.Project)
	if !in.View.Locked(in.Project) && !leased {
		return store.AccessApproval{}, fmt.Errorf("%s: %w: %s", op, ErrNotLocked, in.Project)
	}
	if leased && lease.Expires.Sub(now) > access.ExpiryWarning {
		// The lease covers what is asked (a lock's lease is read-write), and
		// its end is not near: a refresh now would only ask the owner again.
		return store.AccessApproval{}, fmt.Errorf("%s: %w", op, s.liveLease(ctx, lease, now))
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
		if pending, err = s.explained(ctx, pending, in.Via, in.Reason, now); err != nil {
			return store.AccessApproval{}, fmt.Errorf("%s: %w", op, err)
		}
		return pending, nil
	}
	if in.Via == ViaRefusal {
		last, ok, err := store.LastAccessApproval(ctx, s.cfg.DB, anchor.Key(), in.Project)
		if err != nil {
			return store.AccessApproval{}, fmt.Errorf("%s: %w", op, err)
		}
		if ok && last.Status == store.ApprovalDenied {
			return store.AccessApproval{}, fmt.Errorf("%s: %w", op, &DeniedError{Request: last})
		}
	}

	kind, reason := store.AccessKindRequest, strings.TrimSpace(in.Reason)
	if leased {
		kind = store.AccessKindRefresh
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
		via = ViaRequest
	}
	s.record(ctx, core.Event{Kind: core.EventAccessRequested, ItemID: a.ID, SessionID: in.SessionID, ProjectSlug: in.Project,
		Payload: map[string]any{"kind": kind, "via": via}})
	return a, nil
}

// explained is pending as an ask via found it: an explicit request
// (ViaRequest) with a reason fills in the reason a refusal's request lacks,
// so the agent's words reach the owner rather than "(no reason given)"
// (owner decision 2026-11-08). A request that has its reason, and a
// refusal's ask, leave it as it is. It reports what became of the ask's
// reason to the Asked in ctx, if any.
func (s *Service) explained(ctx context.Context, pending store.AccessApproval, via, reason string, now time.Time) (store.AccessApproval, error) {
	reason = strings.TrimSpace(reason)
	if via == ViaRefusal || reason == "" {
		noteFound(ctx, ReasonNone)
		return pending, nil
	}
	if pending.Reason != "" {
		noteFound(ctx, ReasonKept)
		return pending, nil
	}
	filled, err := store.FillAccessReason(ctx, s.cfg.DB, pending.ID, reason, now)
	if err != nil {
		return store.AccessApproval{}, err
	}
	if filled.Reason == reason {
		noteFound(ctx, ReasonAdded)
		s.cfg.Logger.Debug("access request: reason filled in", "request", ShortID(filled.ID))
		return filled, nil
	}
	// Not stored: the row left pending meanwhile, or FillAccessReason declined.
	noteFound(ctx, ReasonKept)
	return filled, nil
}

// What became of an explicit ask's reason (Asked.Outcome).
const (
	// ReasonAdded: this ask's reason is on the request, because it filed a
	// new one, or filled a waiting request's blank.
	ReasonAdded = "added"
	// ReasonKept: this ask gave a reason, but the request stays as it was (it
	// had its own), so this one was not stored.
	ReasonKept = "kept"
	// ReasonNone: this ask's reason reached no request: it gave none, or
	// nothing was filed for it to go on.
	ReasonNone = "none"
)

// Asked is what one explicit ask found, for the surface answering the agent
// (access_request, seam access request): Request and RequestGrant fill it in
// when their ctx carries one (WithAsked), so the answer can say whether it
// filed a new request and what became of its reason. It rides the context so
// the Approvals and Grants interfaces every surface holds keep their shape.
type Asked struct {
	found  bool
	reason string
}

// Outcome is what the ask did, given the request it returned: filed is true
// when it filed a new one, false when it found one already waiting; reason
// is ReasonAdded, ReasonKept or ReasonNone. A new request carries its ask's
// reason, so the row alone says it; only a found request needs the report.
func (a Asked) Outcome(row store.AccessApproval) (filed bool, reason string) {
	if a.found {
		return false, a.reason
	}
	if row.Reason != "" {
		return true, ReasonAdded
	}
	return true, ReasonNone
}

type askedKey struct{}

// WithAsked returns ctx asking Request or RequestGrant to report into a.
func WithAsked(ctx context.Context, a *Asked) context.Context {
	return context.WithValue(ctx, askedKey{}, a)
}

// noteFound reports into ctx's Asked that an ask found a request already
// waiting, and what became of its reason.
func noteFound(ctx context.Context, reason string) {
	if a, ok := ctx.Value(askedKey{}).(*Asked); ok && a != nil {
		a.found, a.reason = true, reason
	}
}

// liveLease is the answer to a request a live lease already covers: the
// lease, and the approved request behind it when its row can be read.
func (s *Service) liveLease(ctx context.Context, l access.Lease, now time.Time) *AccessLiveError {
	out := &AccessLiveError{Project: l.Project, AccessID: l.ID, Expires: l.Expires}
	rows, err := store.ActiveAccessLeasesFor(ctx, s.cfg.DB, []string{l.Process}, now)
	if err != nil {
		s.cfg.Logger.Debug("approval: read the live lease", "lease", l.ID, "error", err)
		return out
	}
	for _, r := range rows {
		if r.ID == l.ID {
			out.ApprovalID = r.ApprovalID
		}
	}
	return out
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
		// this process, when exactly one of the caller's own is live.
		if found, err := ownAgentSessions(ctx, s.cfg.DB, host, agentProcess); err == nil && len(found) == 1 {
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

// ownAgentSessions is store.ActiveAmbientByAgentProcess without the sessions
// another principal started (access.SessionAccountOK), the rule the MCP
// process binding applies too. Request and RequestGrant refuse a restricted
// account before they ask (refuseRestricted), so the caller here is the owner,
// and only sessions no restricted account started count. A process runs as one
// OS account, so a restricted account's row stamped with the owner's agent --
// a stamp its own hook may put on its own session -- is never that agent's
// second session: left in, it would leave the owner's request with no session
// (two rows) or file it under the account's session (the sole row).
func ownAgentSessions(ctx context.Context, db *sql.DB, host, proc string) ([]core.Session, error) {
	found, err := store.ActiveAmbientByAgentProcess(ctx, db, host, proc)
	if err != nil {
		return nil, err
	}
	own := found[:0]
	for _, sess := range found {
		if access.SessionAccountOK(ctx, sess.Account) {
			own = append(own, sess)
		}
	}
	return own, nil
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
	grant, move := a.Kind == store.AccessKindGrant, a.Kind == store.AccessKindMove
	f := Facts{
		ID: a.ID, Short: ShortID(a.ID), Project: a.Project, Isolation: isoWord, Access: accessReadWrite, Kind: a.Kind,
		Program: a.Program, Agent: agentName(a.Program), PID: a.PeerPID, UID: a.PeerUID, Session: a.SessionName, SessionID: a.SessionID,
		CWD: a.CWD, Repo: a.Repo, Model: a.Model, Reason: a.Reason, RequestedAt: a.RequestedAt, ExpiresAt: a.ExpiresAt,
	}
	// A read grant opens the project read-only to the asking project, and its
	// repository is the one the session should be working in; a move's is the
	// home it leaves.
	home := a.Project
	if grant {
		f.Access, f.From, home = accessReadOnly, a.FromProject, a.FromProject
	}
	if move {
		f.From, home = a.FromProject, a.FromProject
	}
	warn := func(code, text string) { f.Warnings = append(f.Warnings, Warning{Code: code, Text: text}) }
	if grant || move {
		if err := s.kindFacts(ctx, a, iso, &f); err != nil {
			return Facts{}, fmt.Errorf("%s: %w", op, err)
		}
	}
	if a.Kind == store.AccessKindRefresh {
		warn(WarnRefreshOfLease, "this session already holds a lease on the project; approving replaces it")
	}
	if what, broad := storedBroad(a.PeerPID, a.Program); broad {
		warn(WarnAnchorIsShell, "the process asking is "+what+", not an agent: a lease on it would cover every program "+
			"it started, so Seamless refuses to approve it")
	}
	if base, runtime := jsRuntimeBase(a.Program); runtime {
		warn(WarnAnchorRuntime, "the process asking is "+base+", and only its own hook says it is an agent: every program it "+
			"started would share this approval, so approve only if this is the agent you started")
	}
	if a.SessionID == "" {
		warn(WarnNoSession, "no session named this agent; its cwd and model are unknown")
	}
	if a.CWD != "" && s.cfg.Host != "" && home != "" {
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

// ShortIDMemory is how long a short id keeps naming a request after it stops
// waiting: the owner reruns `seam approve R-7F3K` from a notification, or a
// page opened by its short id reloads after a Deny, and should read what
// became of it (denied, approved, expired) rather than "no such request".
const ShortIDMemory = 24 * time.Hour

// Resolve finds a request by its id or short id (R-7F3K, any case). A short
// id names a pending request first; failing that, the newest request filed
// within ShortIDMemory that carries it, whatever became of it. A short id two
// pending requests share, or two decided ones with none pending, is refused
// as ambiguous.
func (s *Service) Resolve(ctx context.Context, ref string) (store.AccessApproval, error) {
	ref = strings.TrimSpace(ref)
	if a, err := store.AccessApprovalByID(ctx, s.cfg.DB, ref); err == nil {
		return a, nil
	}
	now := s.cfg.Now()
	rows, err := store.PendingAccessApprovals(ctx, s.cfg.DB, now)
	if err != nil {
		return store.AccessApproval{}, fmt.Errorf("approval.Resolve: %w", err)
	}
	hits := withShortID(rows, ref)
	if len(hits) == 0 {
		recent, err := store.AccessApprovalsSince(ctx, s.cfg.DB, now.Add(-ShortIDMemory))
		if err != nil {
			return store.AccessApproval{}, fmt.Errorf("approval.Resolve: %w", err)
		}
		hits = withShortID(recent, ref)
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return store.AccessApproval{}, fmt.Errorf("approval.Resolve: %w: %q", store.ErrApprovalNotFound, ref)
	default:
		return store.AccessApproval{}, fmt.Errorf("approval.Resolve: %q names %d requests: use the full id", ref, len(hits))
	}
}

// withShortID keeps the requests whose short id is ref (any case).
func withShortID(rows []store.AccessApproval, ref string) []store.AccessApproval {
	var hits []store.AccessApproval
	for _, a := range rows {
		if strings.EqualFold(ShortID(a.ID), ref) {
			hits = append(hits, a)
		}
	}
	return hits
}

// Proof is what decides an approval.
type Proof struct {
	Kind string
	// Assertion is a passkey's response JSON (ProofPasskey), over
	// PasskeyApproveChallenge for the decision's duration.
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
	// Access is the access approved: "" for the facts' own (Facts.Access, the
	// default), or one of Facts.AccessChoices -- a grant's read-only or
	// read-write. A passkey or Touch ID proof signs it.
	Access string
}

// accessWord is the access decision d approves over facts f: f.Access when d
// names none, else d.Access, which must be f.Access or one of f's choices.
func accessWord(f Facts, d Decision) (string, error) {
	switch {
	case d.Access == "" || d.Access == f.Access:
		return f.Access, nil
	case slices.Contains(f.AccessChoices, d.Access):
		return d.Access, nil
	}
	valid := f.AccessChoices
	if len(valid) == 0 {
		valid = []string{f.Access}
	}
	return "", fmt.Errorf("%w %q for %s: valid values are %s", ErrAccess, d.Access, f.Short, strings.Join(valid, ", "))
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

// PasskeyApproveChallenge is the challenge a passkey signs to approve a
// request: a SHA-256 over the request, its own challenge, the facts digest,
// the access and the duration -- what TouchIDApproveMessage carries for a
// Touch ID key -- so an assertion cannot be lifted onto another duration,
// other facts or another request. It is 32 bytes, the size the passkey
// package requires of every challenge.
func PasskeyApproveChallenge(id string, challenge []byte, digest, accessWord, duration string) []byte {
	h := sha256.New()
	h.Write([]byte("seamless passkey approval v1\x00"))
	for _, part := range [][]byte{[]byte(id), challenge, []byte(digest), []byte(accessWord), []byte(duration)} {
		h.Write(part)
		h.Write([]byte{0})
	}
	return h.Sum(nil)
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
	switch a.Kind {
	case store.AccessKindGrant:
		// A read grant is decided by Grants.Approve, and never becomes a lease.
		return store.AccessLease{}, fmt.Errorf("%s: %w: %s", op, ErrIsGrant, ShortID(a.ID))
	case store.AccessKindMove:
		// A move is decided by Grants.ApproveMove: a lease is only its lock's
		// half, never the move.
		return store.AccessLease{}, fmt.Errorf("%s: %w: %s", op, ErrIsMove, ShortID(a.ID))
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
	word, err := accessWord(facts, d)
	if err != nil {
		return store.AccessLease{}, fmt.Errorf("%s: %w", op, err)
	}
	now := s.cfg.Now()
	decidedBy, err := s.prove(ctx, a, facts, d, word, now)
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
// digest) the owner was shown, for word, the access chosen (accessWord), and
// returns who decided: the daemon's account, or the approver credential that
// signed. It is the one proof check of the approve core, for a lock's lease, a
// grant and a move alike.
func (s *Service) prove(ctx context.Context, a store.AccessApproval, facts Facts, d Decision, word string, now time.Time) (string, error) {
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
		// The assertion signs the decision, not only the request: the
		// duration as submitted and the facts as they stand now.
		//
		// Facts that changed after the options were issued (a taint, a
		// reason filled in) are answered by Approve's digest check before
		// this, as ErrFactsChanged, for a client that posts the digest it
		// showed -- the console's page does. A client that posts the FRESH
		// digest with an assertion over the old one fails here as a plain
		// verification failure instead: the old digest is gone, so a stale
		// assertion cannot be told from a forged one without keeping the
		// digest each options were issued for, and the daemon keeps no such
		// state (owner decision 2026-11-08). signedOtherwise recognizes only
		// what it can recompute: another duration, or the bare challenge.
		binding := PasskeyApproveChallenge(a.ID, a.Challenge, facts.Digest, word, d.Duration)
		used, err := s.cfg.Passkeys.FinishAssertion(binding, allowed, d.Proof.Assertion)
		if err != nil {
			if signedOtherwise(a, facts, d, word) {
				return "", fmt.Errorf("%w: %w", ErrFactsChanged, ErrBindingChanged)
			}
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
		msg := TouchIDApproveMessage(a.ID, a.Challenge, d.Digest, word, d.Duration)
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

// signedOtherwise reports whether a passkey assertion that failed against
// decision d's binding was made for this request with another duration or
// another access (one of the facts' choices), or over its bare challenge (an
// older daemon's page): a decision changed after it was signed, which is
// refused as changed facts rather than as a bad passkey. Anything else --
// another request's assertion, a forged or unreadable one -- is not, and
// keeps the passkey's own refusal.
func signedOtherwise(a store.AccessApproval, facts Facts, d Decision, word string) bool {
	got, err := passkey.AssertionChallenge(d.Proof.Assertion)
	if err != nil {
		return false
	}
	if bytes.Equal(got, a.Challenge) {
		return true
	}
	words := append([]string{facts.Access}, facts.AccessChoices...)
	for _, w := range words {
		for _, other := range Durations {
			if (other != d.Duration || w != word) && bytes.Equal(got, PasskeyApproveChallenge(a.ID, a.Challenge, facts.Digest, w, other)) {
				return true
			}
		}
	}
	return false
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

// AssertionOptionsFor returns the navigator.credentials.get options that
// approve pending request id with a passkey for duration, over the facts
// whose digest the page shows: every enrolled passkey allowed, user
// verification required, and the challenge PasskeyApproveChallenge, so the
// assertion signs the duration and the facts as well as the request. A
// duration that is not one of Durations is ErrDuration, and a digest that is
// not the facts' as they stand now is ErrFactsChanged: the options are issued
// only for a decision the owner is looking at.
func (s *Service) AssertionOptionsFor(ctx context.Context, id, duration, digest string) ([]byte, error) {
	return s.AssertionOptionsForAccess(ctx, id, duration, "", digest)
}

// AssertionOptionsForAccess is AssertionOptionsFor for the access the page
// chose as well (Decision.Access: "" for the facts' own): the assertion
// signs it, so a passkey that approved read-only cannot be lifted onto read
// and write. An access the request does not offer is ErrAccess.
func (s *Service) AssertionOptionsForAccess(ctx context.Context, id, duration, accessChoice, digest string) ([]byte, error) {
	const op = "approval.AssertionOptionsFor"
	if err := refuseRestricted(ctx); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	if !s.PasskeysOn() {
		return nil, fmt.Errorf("%s: %w", op, ErrPasskeysOff)
	}
	if _, err := s.LeaseLength(duration); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	a, err := store.AccessApprovalByID(ctx, s.cfg.DB, id)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	if a.Status != store.ApprovalPending {
		return nil, fmt.Errorf("%s: %w: %s is %s", op, store.ErrApprovalNotPending, ShortID(a.ID), a.Status)
	}
	facts, err := s.factsOf(ctx, a)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	if digest == "" || digest != facts.Digest {
		return nil, fmt.Errorf("%s: %w", op, ErrFactsChanged)
	}
	word, err := accessWord(facts, Decision{Access: accessChoice})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	creds, err := s.passkeys(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	opts, err := s.cfg.Passkeys.BeginAssertion(PasskeyApproveChallenge(a.ID, a.Challenge, facts.Digest, word, duration), creds)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
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
