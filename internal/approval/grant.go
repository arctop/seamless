package approval

// Read grants (isolation.requests, step G; off by default): a read refused at
// a confidential project's fence files a request, and the owner may approve
// it as a grant -- read-only, one project, to the one asker, for a limited
// time. A grant request is an access_approvals row of kind grant: it shares
// the short id, the facts and their digest, the pending list and the deny
// path with a lock's request. Only Grants.Approve decides one, and it writes a
// grant (store.ApproveAccessGrant), never a lease.
//
// Two tiers, by what the daemon can know about the asker:
//
//   - under peer_auth (the Grants hold the daemon's approval Service), the
//     asker is the agent process the request is anchored to in the socket
//     connection's kernel-verified chain, exactly as for a lock's lease, and
//     the decision needs the same proofs (the daemon's account through
//     seamlessd approve, a passkey, a Touch ID key);
//   - without peer_auth (advisory), the asker is the session the connection
//     is bound to, and the decision is the bearer key's (ProofKey): seam
//     approve's y/N in a terminal, or the console's Approve. Any process that
//     holds the key can make it, so this tier is advisory, and every surface
//     says so.
//
// Never offered: anything involving a sealed project, on either side, and a
// target that is not confidential. Every check runs again at approval, so a
// project tightened while the request waited is not opened.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/store"
)

// ProofKey is the advisory tier's decision: the bearer key, or the console
// cookie it mints. Only Grants without peer_auth accept it.
const ProofKey = "key"

// decidedByKey is access_grants.decided_by for the advisory tier.
const decidedByKey = "bearer-key"

// WarnCopyInto is the line every approval surface shows for a read grant.
const WarnCopyInto = "copy_into"

// CopyIntoLine is that line: what approving a read grant lets happen.
func CopyIntoLine(target, from string) string {
	return "content from " + target + " may be copied into " + from + "'s memories"
}

var (
	// ErrNotConfidential is a read grant asked for a project that is not
	// confidential: an open project needs none, and a sealed one is never
	// granted (ErrSealed).
	ErrNotConfidential = errors.New("project is not confidential")
	// ErrSealed is a read grant that would involve a sealed project, on
	// either side: that refusal is final, and offers no request.
	ErrSealed = errors.New("a sealed project is never granted, and never granted to")
	// ErrNoSession is a read grant asked by no identified session.
	ErrNoSession = errors.New("no session: ask from an agent session bound to its project")
	// ErrNoProject is a read grant asked from the global scope: a grant opens
	// a project to another project, never to the global scope every briefing
	// reads.
	ErrNoProject = errors.New("the asking session is bound to no project")
	// ErrNotAGrant is a grant decision on a lock's request.
	ErrNotAGrant = errors.New("this is a lock's request, not a read grant's")
	// ErrPeerProof is the bearer key offered as the proof under peer_auth,
	// where a read grant needs the same proofs as a lease.
	ErrPeerProof = errors.New("under peer_auth a read grant needs the owner's proof: seam approve in the owner's terminal, or a passkey")
)

// GrantsConfig wires the read grants.
type GrantsConfig struct {
	DB *sql.DB
	// Approvals is the daemon's approval service under peer_auth: its
	// anchors and proofs decide a grant. Nil is the advisory tier (no
	// peer_auth).
	Approvals *Service
	// Events records access.requested, granted, confined, denied and
	// revoked; nil records nothing.
	Events *events.Recorder
	// Host is this machine's name (the repository warning).
	Host string
	// LeaseCap caps every grant's length; 0 means 8 hours.
	LeaseCap time.Duration
	Now      func() time.Time
	Logger   *slog.Logger
}

// Grants files and decides read grants. It is safe for concurrent use.
type Grants struct {
	svc      *Service
	advisory bool
}

// NewGrants builds the read grants over cfg.Approvals, or over a passkey-less
// service of their own in the advisory tier.
func NewGrants(cfg GrantsConfig) *Grants {
	if cfg.Approvals != nil {
		return &Grants{svc: cfg.Approvals}
	}
	return &Grants{advisory: true, svc: New(Config{DB: cfg.DB, Events: cfg.Events, Host: cfg.Host,
		LeaseCap: cfg.LeaseCap, Now: cfg.Now, Logger: cfg.Logger})}
}

// Advisory reports whether this is the advisory tier: no peer_auth, and the
// bearer key decides.
func (g *Grants) Advisory() bool { return g.advisory }

// GrantInput is one ask for a read grant.
type GrantInput struct {
	// Project is the confidential project asked for.
	Project string
	// From is the asking session's project, when the caller knows it (the
	// MCP binding); it must agree with the session's own.
	From string
	// SessionID is the session the call is bound to, "" when none; then the
	// session stamped with AgentProcess on Host stands in, when exactly one is
	// live.
	SessionID          string
	AgentProcess, Host string
	// Reason is the agent's own words, at most store.MaxAccessReasonRunes.
	Reason string
	// Via says how it was filed: "refusal" (automatic) or "request".
	Via string
}

// eligible is who may ask, judged now: a confidential target, asked from a
// project that is not sealed. Anything involving a sealed project is final.
func (g *Grants) eligible(ctx context.Context, target, from string) error {
	ti, err := store.IsolationOf(ctx, g.svc.cfg.DB, target)
	if err != nil {
		return err
	}
	fi, err := store.IsolationOf(ctx, g.svc.cfg.DB, from)
	if err != nil {
		return err
	}
	switch {
	case ti == core.IsolationSealed || fi == core.IsolationSealed:
		return ErrSealed
	case ti != core.IsolationConfidential:
		return fmt.Errorf("%w: %s", ErrNotConfidential, target)
	}
	return nil
}

// RequestGrant files (or finds) the pending read grant request of the asker
// ctx and in name, for in.Project from the asking session's project. A
// pending request of that asker for that pair is returned as it is, so a
// repeated refusal never piles up requests. An asker that already holds a
// live grant of the pair files nothing while more than access.ExpiryWarning
// of it remains: the answer is an *AccessLiveError naming it, as for a
// lease (Service.Request).
func (g *Grants) RequestGrant(ctx context.Context, in GrantInput) (store.AccessApproval, error) {
	const op = "approval.RequestGrant"
	if err := refuseRestricted(ctx); err != nil {
		return store.AccessApproval{}, fmt.Errorf("%s: %w", op, err)
	}
	db, now := g.svc.cfg.DB, g.svc.cfg.Now()
	target := strings.TrimSpace(in.Project)
	if target == "" {
		return store.AccessApproval{}, fmt.Errorf("%s: %w: name the project", op, store.ErrInvalidAccessRequest)
	}
	req := store.GrantRequest{Project: target, Lease: g.svc.leaseCap(), TTL: RequestTTL}
	v, hasView := access.FromContext(ctx)
	if g.advisory {
		if hasView {
			// A View means peer_auth, whose asker is a process: an advisory
			// service never serves one.
			return store.AccessApproval{}, fmt.Errorf("%s: %w", op, ErrNoProcess)
		}
		sessionID := in.SessionID
		if sessionID == "" && in.AgentProcess != "" {
			if found, err := store.ActiveAmbientByAgentProcess(ctx, db, in.Host, in.AgentProcess); err == nil && len(found) == 1 {
				sessionID = found[0].ID
			}
		}
		req.SessionID = sessionID
	} else {
		chain := v.Principal().Chain
		if !hasView || v.Principal().Kind != access.KindOwner || !v.Principal().Socket || len(chain) == 0 {
			return store.AccessApproval{}, fmt.Errorf("%s: %w", op, ErrNoProcess)
		}
		got, err := g.svc.anchorFor(ctx, chain, in.SessionID, in.AgentProcess, in.Host)
		if err != nil {
			return store.AccessApproval{}, fmt.Errorf("%s: %w", op, err)
		}
		req.SessionID, req.ProcessKey, req.PeerPID, req.PeerUID, req.Program = got.sessionID, got.token.Key(), got.token.PID, v.Principal().UID, got.program
	}
	if req.SessionID == "" {
		return store.AccessApproval{}, fmt.Errorf("%s: %w", op, ErrNoSession)
	}
	sess, ok, err := store.SessionByID(ctx, db, req.SessionID)
	if err != nil {
		return store.AccessApproval{}, fmt.Errorf("%s: %w", op, err)
	}
	if !ok || sess.Status == core.SessionCompleted {
		// A session the idle reaper expired is still its agent's (a call
		// revives it); one completed on purpose asks nothing.
		return store.AccessApproval{}, fmt.Errorf("%s: %w", op, ErrNoSession)
	}
	from := sess.ProjectSlug
	switch {
	case in.From != "" && in.From != from:
		return store.AccessApproval{}, fmt.Errorf("%s: %w: the session works in %q, not %q", op, store.ErrInvalidAccessRequest, from, in.From)
	case from == "":
		return store.AccessApproval{}, fmt.Errorf("%s: %w", op, ErrNoProject)
	case from == target:
		return store.AccessApproval{}, fmt.Errorf("%s: %w: a session always reads its own project", op, ErrNotConfidential)
	}
	// peer_auth has the first word: neither side may be beyond this
	// connection's reach (a locked target is the lock's to open, never a
	// grant's).
	if !access.Visible(ctx, target) || !access.Writable(ctx, from) {
		return store.AccessApproval{}, fmt.Errorf("%s: %w", op, access.ErrNotReachable)
	}
	if err := g.eligible(ctx, target, from); err != nil {
		return store.AccessApproval{}, fmt.Errorf("%s: %w", op, err)
	}
	if live, err := g.liveGrant(ctx, req, target, from, now); err != nil {
		return store.AccessApproval{}, fmt.Errorf("%s: %w", op, err)
	} else if live != nil {
		return store.AccessApproval{}, fmt.Errorf("%s: %w", op, live)
	}
	if pending, ok, err := store.PendingGrantRequest(ctx, db, req.ProcessKey, req.SessionID, target, from, now); err != nil {
		return store.AccessApproval{}, fmt.Errorf("%s: %w", op, err)
	} else if ok {
		return pending, nil
	}
	req.From = from
	req.Reason = strings.TrimSpace(in.Reason)
	if utf8.RuneCountInString(req.Reason) > store.MaxAccessReasonRunes {
		return store.AccessApproval{}, fmt.Errorf("%s: %w: the reason is limited to %d characters", op,
			store.ErrInvalidAccessRequest, store.MaxAccessReasonRunes)
	}
	req.SessionName, req.CWD, req.Model = sess.Name, sess.CWD, sess.Model
	req.Repo = repoName(sess.CWD)
	a, err := store.CreateGrantRequest(ctx, db, req, now)
	if err != nil {
		return store.AccessApproval{}, fmt.Errorf("%s: %w", op, err)
	}
	via := in.Via
	if via == "" {
		via = "request"
	}
	g.svc.record(ctx, core.Event{Kind: core.EventAccessRequested, ItemID: a.ID, SessionID: a.SessionID, ProjectSlug: a.Project,
		Payload: map[string]any{"kind": store.AccessKindGrant, "via": via, "from_project": from}})
	return a, nil
}

// liveGrant is the asker's live grant of target from from that covers a new
// ask -- more than access.ExpiryWarning from its end -- as an
// *AccessLiveError; nil when there is none. The asker is req's: its agent
// process under peer_auth, else its session.
func (g *Grants) liveGrant(ctx context.Context, req store.GrantRequest, target, from string, now time.Time) (*AccessLiveError, error) {
	var keys []string
	sessionID := req.SessionID
	if req.ProcessKey != "" {
		keys, sessionID = []string{req.ProcessKey}, ""
	}
	rows, err := store.AccessGrantsForHolder(ctx, g.svc.cfg.DB, keys, sessionID)
	if err != nil {
		return nil, err
	}
	var live *AccessLiveError
	for _, r := range rows {
		if r.Project != target || r.FromProject != from || r.RevokedAt != nil || r.ExpiresAt.Sub(now) <= access.ExpiryWarning {
			continue
		}
		if live == nil || r.ExpiresAt.After(live.Expires) {
			live = &AccessLiveError{Project: target, From: from, AccessID: r.ID, ApprovalID: r.ApprovalID, Expires: r.ExpiresAt}
		}
	}
	return live, nil
}

// Approve decides a read grant request: the facts digest the owner saw, the
// fence as it stands now, and the proof -- ProofKey only in the advisory
// tier, the approve core's proofs (Service.prove) otherwise. The grant is
// written by store.ApproveAccessGrant (single use, bound to the challenge).
func (g *Grants) Approve(ctx context.Context, d Decision) (store.AccessGrant, error) {
	const op = "approval.Grants.Approve"
	if err := refuseRestricted(ctx); err != nil {
		return store.AccessGrant{}, fmt.Errorf("%s: %w", op, err)
	}
	db := g.svc.cfg.DB
	length, err := g.svc.LeaseLength(d.Duration)
	if err != nil {
		return store.AccessGrant{}, fmt.Errorf("%s: %w", op, err)
	}
	a, err := store.AccessApprovalByID(ctx, db, d.ID)
	if err != nil {
		return store.AccessGrant{}, fmt.Errorf("%s: %w", op, err)
	}
	if a.Kind != store.AccessKindGrant {
		return store.AccessGrant{}, fmt.Errorf("%s: %w: %s", op, ErrNotAGrant, ShortID(a.ID))
	}
	if a.Status != store.ApprovalPending {
		return store.AccessGrant{}, fmt.Errorf("%s: %w: %s is %s", op, store.ErrApprovalNotPending, ShortID(a.ID), a.Status)
	}
	// The anchor rule (anchor.go), as for a lease: a grant to a broad
	// ancestor's tree would reach every program it started. A request of the
	// advisory tier names no process, and passes.
	if err := storedAnchorRefusal(a.PeerPID, a.Program); err != nil {
		return store.AccessGrant{}, fmt.Errorf("%s: %s: %w", op, ShortID(a.ID), err)
	}
	facts, err := g.svc.factsOf(ctx, a)
	if err != nil {
		return store.AccessGrant{}, fmt.Errorf("%s: %w", op, err)
	}
	if d.Digest == "" || d.Digest != facts.Digest {
		return store.AccessGrant{}, fmt.Errorf("%s: %w", op, ErrFactsChanged)
	}
	// The fence as it stands now: a project tightened to sealed, or loosened
	// to open, while the request waited is not opened by it.
	if err := g.eligible(ctx, a.Project, a.FromProject); err != nil {
		return store.AccessGrant{}, fmt.Errorf("%s: %w", op, err)
	}
	now := g.svc.cfg.Now()
	var decidedBy string
	switch {
	case d.Proof.Kind == ProofKey && g.advisory:
		decidedBy = decidedByKey
	case d.Proof.Kind == ProofKey:
		return store.AccessGrant{}, fmt.Errorf("%s: %w", op, ErrPeerProof)
	default:
		if decidedBy, err = g.svc.prove(ctx, a, facts, d, now); err != nil {
			return store.AccessGrant{}, fmt.Errorf("%s: %w", op, err)
		}
	}
	grant, err := store.ApproveAccessGrant(ctx, db, a.ID, a.Challenge, decidedBy, length, now)
	if err != nil {
		return store.AccessGrant{}, fmt.Errorf("%s: %w", op, err)
	}
	g.svc.record(ctx, core.Event{Kind: core.EventAccessGranted, ItemID: a.ID, SessionID: a.SessionID, ProjectSlug: a.Project,
		Payload: map[string]any{"grant": grant.ID, "from_project": a.FromProject, "proof": d.Proof.Kind,
			"seconds": int64(grant.ExpiresAt.Sub(grant.GrantedAt) / time.Second), "duration": d.Duration}})
	if iso, err := store.IsolationOf(ctx, db, a.FromProject); err == nil && iso == core.IsolationOpen {
		g.svc.record(ctx, core.Event{Kind: core.EventAccessConfined, ItemID: a.ID, SessionID: a.SessionID, ProjectSlug: a.FromProject,
			Payload: map[string]any{"grant": grant.ID, "target_project": a.Project}})
	}
	return grant, nil
}

// RevokeGrant ends one live grant (the owner's Revoke). Its holder stays
// write-confined: ending a grant does not unread what it read.
func (g *Grants) RevokeGrant(ctx context.Context, id string) (store.AccessGrant, error) {
	db, now := g.svc.cfg.DB, g.svc.cfg.Now()
	grant, err := store.AccessGrantByID(ctx, db, id)
	if err != nil {
		return store.AccessGrant{}, fmt.Errorf("approval.RevokeGrant: %w", err)
	}
	if err := store.RevokeAccessGrant(ctx, db, grant.ID, store.LeaseRevokedOwner, now); err != nil {
		return store.AccessGrant{}, fmt.Errorf("approval.RevokeGrant: %w", err)
	}
	g.svc.record(ctx, core.Event{Kind: core.EventAccessRevoked, ItemID: grant.ApprovalID, SessionID: grant.SessionID,
		ProjectSlug: grant.Project, Payload: map[string]any{"reason": store.LeaseRevokedOwner, "grants": 1, "grant": grant.ID}})
	return grant, nil
}

// RevokeSession ends every live grant a session asked for: its end, which is
// what "until the session ends" means. It returns how many.
func (g *Grants) RevokeSession(ctx context.Context, sessionID string) (int64, error) {
	if sessionID == "" {
		return 0, nil
	}
	n, err := store.RevokeAccessGrantsForSession(ctx, g.svc.cfg.DB, sessionID, store.LeaseRevokedSessionEnded, g.svc.cfg.Now())
	if err != nil {
		return 0, fmt.Errorf("approval.Grants.RevokeSession: %w", err)
	}
	if n > 0 {
		g.svc.record(ctx, core.Event{Kind: core.EventAccessRevoked, SessionID: sessionID,
			Payload: map[string]any{"reason": store.LeaseRevokedSessionEnded, "grants": n}})
	}
	return n, nil
}

// Live returns every live grant, soonest to end first.
func (g *Grants) Live(ctx context.Context) ([]store.AccessGrant, error) {
	rows, err := store.LiveAccessGrants(ctx, g.svc.cfg.DB, g.svc.cfg.Now())
	if err != nil {
		return nil, fmt.Errorf("approval.Grants.Live: %w", err)
	}
	return rows, nil
}

// The request side the advisory tier has no approval Service for: the same
// rows, facts and decisions as a lock's requests.

// Pending is Service.Pending.
func (g *Grants) Pending(ctx context.Context) ([]Facts, error) { return g.svc.Pending(ctx) }

// Facts is Service.Facts.
func (g *Grants) Facts(ctx context.Context, id string) (Facts, error) { return g.svc.Facts(ctx, id) }

// Resolve is Service.Resolve.
func (g *Grants) Resolve(ctx context.Context, ref string) (store.AccessApproval, error) {
	return g.svc.Resolve(ctx, ref)
}

// Deny is Service.Deny.
func (g *Grants) Deny(ctx context.Context, id, by string) error { return g.svc.Deny(ctx, id, by) }

// LeaseLength is Service.LeaseLength: a grant's length for a duration choice.
func (g *Grants) LeaseLength(choice string) (time.Duration, error) { return g.svc.LeaseLength(choice) }

// Page is Service.Page: the passkey page under peer_auth, "" in the advisory
// tier.
func (g *Grants) Page(id string) string { return g.svc.Page(id) }

// Ways is Service.Ways under peer_auth. The advisory tier has neither Touch
// ID nor passkeys (its decision is the bearer key's), so none.
func (g *Grants) Ways(ctx context.Context, id string) access.Ways {
	if g.advisory {
		return access.Ways{}
	}
	return g.svc.Ways(ctx, id)
}
