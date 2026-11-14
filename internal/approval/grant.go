package approval

// Grants (isolation.requests, step G; off by default): a read refused at a
// confidential project's fence files a request, and the owner may approve it
// as a grant -- one project, to the one asker, for a limited time; read-only,
// or, between confidential projects and as the owner chooses, read and write
// (plan fenced-sessions). A grant request is an access_approvals row of kind
// grant: it shares the short id, the facts and their digest, the pending list
// and the deny path with a lock's request. Only Grants.Approve decides one,
// and it writes a grant (store.GrantAccessApproveWith) and the flows its carry
// lists -- with a locked target's lease under peer_auth, in the same
// transaction -- never a lease alone. A move request (ask.go) shares all of
// it too, and only Grants.ApproveMove decides one.
//
// Two tiers, by what the daemon can know about the asker:
//
//   - under peer_auth (the Grants hold the daemon's approval Service), the
//     asker is the agent process the request is anchored to in the socket
//     connection's kernel-verified chain, exactly as for a lock's lease, and
//     the decision needs the same proofs (the daemon's account through
//     seamlessd approve, a passkey, a Touch ID key);
//   - without peer_auth (advisory), the asker is the conversation of the
//     session the connection is bound to (its client and the session id it
//     issued), or that session when it names none, and the decision is the
//     bearer key's (ProofKey): seam approve's y/N in a terminal, or the
//     console's Approve. Any process that holds the key can make it, so this
//     tier is advisory, and every surface says so.
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
	"time"

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

// CiteByReferenceLine is the instruction a read grant's holder gets for
// writing up its work in the project it asked from. CopyIntoLine stays the
// owner's warning, because nothing inspects what the agent writes; this is
// what the agent is asked to do.
func CiteByReferenceLine(target, from string) string {
	return "When you document this work in " + from + " (task details, memories, notes, findings), refer to " + target +
		"'s items by reference -- project slug plus the item's name or id -- and leave out " + target + "'s confidential details."
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
	svc := New(Config{DB: cfg.DB, Events: cfg.Events, Host: cfg.Host, LeaseCap: cfg.LeaseCap, Now: cfg.Now, Logger: cfg.Logger})
	// Without peer_auth nothing is locked, whatever a table remembers.
	svc.lockless = true
	return &Grants{advisory: true, svc: svc}
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
	// session stamped with AgentProcess on Host stands in, when exactly one the
	// owner's agents started is live (ownAgentSessions).
	SessionID          string
	AgentProcess, Host string
	// Reason is the agent's own words, at most store.MaxAccessReasonRunes.
	Reason string
	// Via says how it was filed: ViaRefusal (automatic) or ViaRequest (the
	// default).
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
// repeated refusal never piles up requests (an explicit request's reason
// fills in a missing one, as for a lock's). A refusal whose asker's last
// request for the pair the owner denied files nothing: the answer is a
// *DeniedError, until an explicit request files a new one. An asker that already holds a
// live grant of the pair files nothing while more than access.ExpiryWarning
// of it remains: the answer is an *AccessLiveError naming it, as for a
// lease (Service.Request).
//
// The asker is its agent process under peer_auth, else its conversation when
// the session names one (the client and the session id it issued), else the
// session. A session that names its conversation -- the agent's own, whose
// ambient row is live -- asks for the kind the conversation's home and
// in.Project call for, as Ask does without write (plan fenced-sessions P4):
// from an open home (or the global scope) and for any sealed project a move,
// from a confidential home a read grant. Any other session asks for a read
// grant from its own project, as before: a request from a confidential
// project states its carry, so the owner sees, and may choose, read-only or
// read and write; one from an open project states none.
func (g *Grants) RequestGrant(ctx context.Context, in GrantInput) (store.AccessApproval, error) {
	a, err := g.file(ctx, AskInput{GrantInput: in}, askAuto)
	if err != nil {
		return store.AccessApproval{}, fmt.Errorf("approval.RequestGrant: %w", err)
	}
	return a, nil
}

// liveGrant is the asker's live grant of target from from that covers a new
// ask -- more than access.ExpiryWarning from its end, and read and write when
// write is asked for -- as an *AccessLiveError; nil when there is none. held
// are the asker's holders (asker.held), every one of whose grants is its own.
func (g *Grants) liveGrant(ctx context.Context, held []store.Holder, target, from string, write bool, now time.Time) (*AccessLiveError, error) {
	rows, err := heldGrants(ctx, g.svc.cfg.DB, held)
	if err != nil {
		return nil, err
	}
	var live *AccessLiveError
	for _, r := range rows {
		if r.Project != target || r.FromProject != from || r.RevokedAt != nil || r.ExpiresAt.Sub(now) <= access.ExpiryWarning {
			continue
		}
		if write && r.Access != store.GrantAccessWrite {
			continue
		}
		if live == nil || r.ExpiresAt.After(live.Expires) {
			live = &AccessLiveError{Project: target, From: from, AccessID: r.ID, ApprovalID: r.ApprovalID, Expires: r.ExpiresAt}
		}
	}
	return live, nil
}

// Approve decides a grant request: the facts digest the owner saw, the
// access the owner chose (d.Access: read-only, the default, or read and write
// where the facts offer it), the fence as it stands now, and the proof --
// ProofKey only in the advisory tier, the approve core's proofs
// (Service.prove, over the chosen access) otherwise. The grant, the flows its
// carry lists and -- for a locked target under peer_auth -- the target's lease
// are one transaction (store.GrantAccessApproveWith: single use, bound to the
// challenge).
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
	word, err := accessWord(facts, d)
	if err != nil {
		return store.AccessGrant{}, fmt.Errorf("%s: %w", op, err)
	}
	// The fence as it stands now: a project tightened to sealed, or loosened
	// to open, while the request waited is not opened by it.
	if err := g.eligible(ctx, a.Project, a.FromProject); err != nil {
		return store.AccessGrant{}, fmt.Errorf("%s: %w", op, err)
	}
	now := g.svc.cfg.Now()
	decidedBy, err := g.decidedBy(ctx, a, facts, d, word, now)
	if err != nil {
		return store.AccessGrant{}, fmt.Errorf("%s: %w", op, err)
	}
	grantAccess := store.GrantAccessRead
	if word == accessReadWrite {
		grantAccess = store.GrantAccessWrite
	}
	grant, lease, err := store.GrantAccessApproveWith(ctx, db, store.GrantDecision{ID: a.ID, Challenge: a.Challenge,
		DecidedBy: decidedBy, Length: length, Access: grantAccess, Lease: !g.advisory && a.ProcessKey != "" && facts.Locked}, now)
	if err != nil {
		return store.AccessGrant{}, fmt.Errorf("%s: %w", op, err)
	}
	payload := map[string]any{"grant": grant.ID, "from_project": a.FromProject, "proof": d.Proof.Kind,
		"seconds": int64(grant.ExpiresAt.Sub(grant.GrantedAt) / time.Second), "duration": d.Duration}
	if grantAccess == store.GrantAccessWrite {
		payload["access"] = grantAccess
	}
	if lease != nil {
		payload["lease"] = lease.ID
	}
	g.svc.record(ctx, core.Event{Kind: core.EventAccessGranted, ItemID: a.ID, SessionID: a.SessionID, ProjectSlug: a.Project,
		Payload: payload})
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
// what "until the session ends" means. When the session is its
// conversation's own (ambient) row, its end is the conversation's, so every
// live grant the conversation holds ends too, whichever of its sessions asked.
// A move ends a session without this: the conversation goes on in the child.
// Flows and the label stay: ending a grant does not unread what it read. It
// returns how many grants ended.
func (g *Grants) RevokeSession(ctx context.Context, sessionID string) (int64, error) {
	if sessionID == "" {
		return 0, nil
	}
	db, now := g.svc.cfg.DB, g.svc.cfg.Now()
	n, err := store.RevokeAccessGrantsForSession(ctx, db, sessionID, store.LeaseRevokedSessionEnded, now)
	if err != nil {
		return 0, fmt.Errorf("approval.Grants.RevokeSession: %w", err)
	}
	sess, ok, err := store.SessionByID(ctx, db, sessionID)
	if err != nil {
		return n, fmt.Errorf("approval.Grants.RevokeSession: %w", err)
	}
	if ok && sess.Ambient && sess.ExternalClient != "" && sess.ExternalSessionID != "" {
		if holder, herr := store.HolderOfConversation(sess.ExternalClient, sess.ExternalSessionID); herr == nil {
			m, err := store.RevokeAccessGrantsForHolder(ctx, db, holder, store.LeaseRevokedSessionEnded, now)
			if err != nil {
				return n, fmt.Errorf("approval.Grants.RevokeSession: %w", err)
			}
			n += m
		}
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
