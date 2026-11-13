package approval

// Asking for a fenced project (isolation.requests): one filing path for a
// read grant, a read-and-write grant and a move, through the request core
// every request shares -- one waiting request per asker and project, a
// refusal's ask after the owner's denial files nothing (Addendum 7), an
// explicit ask always files, and its reason fills in a waiting request's
// missing one.
//
// Two entry points:
//
//   - RequestGrant (grant.go) asks for a read grant from the asking session's
//     own project, whatever that project is, as before migration 039. A
//     request from a confidential home states its carry; one from an open home
//     states none, as before.
//   - Ask files the kind the conversation's home and the target call for
//     (KindFor): a move, or a grant that may ask for write too. The home is
//     the conversation's: its ambient row's project, whichever of its
//     sessions the call is bound to.
//
// The asker is the holder of what an approval opens (store.Holder): the
// agent's process under peer_auth, else the conversation (the client and the
// session id it issued) when the session carries one, else the session.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

var (
	// ErrIsHome is an ask for the project the conversation already works in:
	// nothing to move into or grant (a lock there still needs its lease).
	ErrIsHome = errors.New("this conversation already works in that project: nothing to ask")
	// ErrNoConversation is a move asked by a session no conversation names (a
	// client that sends no agent-process identity, or a session that is not
	// its conversation's own): a move's child takes over the conversation's
	// identity, so there must be one.
	ErrNoConversation = errors.New("a move needs the agent's own session (the one its SessionStart hook recorded): " +
		"this session names no conversation")
	// ErrNotAMove is a move decision on a request of another kind.
	ErrNotAMove = errors.New("this is not a move request")
	// ErrIsMove is a lease or grant decision on a move request: only
	// Grants.ApproveMove decides one.
	ErrIsMove = errors.New("this is a move request, not a lock's or a grant's")
	// ErrAccess is a decision naming an access the request does not offer.
	ErrAccess = errors.New("invalid access")
	// ErrTouchIDMove is a Touch ID proof offered for a move. The Touch ID
	// approver of this release words every request as access for a duration,
	// so its prompt cannot say what a move does (for good, and out of every
	// place the agent could write before); what the owner confirms with a
	// touch must be what is granted. A passkey, whose page shows the move, or
	// the owner's terminal decides one.
	ErrTouchIDMove = errors.New("a move is approved with a passkey or seam approve in the owner's own terminal: " +
		"the Touch ID approver does not word moves yet")
)

// AskInput is one ask for a fenced project (Ask).
type AskInput struct {
	GrantInput
	// Write asks for read and write: a grant between confidential projects
	// asks for it (the owner still chooses), and a move always writes its
	// target.
	Write bool
}

// Ask files (or finds) the request the asker's home and in.Project call for
// (KindFor): a move, or a grant -- read-only, or asking for read and write
// with in.Write. Under peer_auth a locked target may be asked for too: its
// approval then carries the target's lease. The rules are RequestGrant's
// otherwise: one waiting request per asker and project, a refusal's ask after
// a denial files nothing, a live grant that covers the ask answers with
// itself.
func (g *Grants) Ask(ctx context.Context, in AskInput) (store.AccessApproval, error) {
	a, err := g.file(ctx, in, askKind)
	if err != nil {
		return store.AccessApproval{}, fmt.Errorf("approval.Ask: %w", err)
	}
	return a, nil
}

// askMode is which rules a filing follows.
type askMode int

const (
	// askGrant is RequestGrant's: a read grant from the asking session's own
	// project.
	askGrant askMode = iota
	// askKind is Ask's: the kind the conversation's home and the target call
	// for.
	askKind
)

// asker is who files a request, as filing settles it.
type asker struct {
	// sess is the session the call named or the agent's own; home is the
	// conversation's home session (askKind), else sess.
	sess, home core.Session
	holder     store.Holder
	// held are every holder whose rows are the conversation's, as the
	// clearance reads them (store.ConversationClearance): under peer_auth the
	// process chain, else the conversation and each of its own sessions (a
	// grant one of them asked for before migration 039 is the session's).
	// The carry's label and the live grants are read over all of them.
	held []store.Holder
	// Under peer_auth: the anchoring agent process.
	processKey, program string
	pid                 int32
	uid                 uint32
}

// file is the one filing path of a grant or a move request.
func (g *Grants) file(ctx context.Context, in AskInput, mode askMode) (store.AccessApproval, error) {
	if err := refuseRestricted(ctx); err != nil {
		return store.AccessApproval{}, err
	}
	db, now := g.svc.cfg.DB, g.svc.cfg.Now()
	target := strings.TrimSpace(in.Project)
	if target == "" {
		return store.AccessApproval{}, fmt.Errorf("%w: name the project", store.ErrInvalidAccessRequest)
	}
	who, err := g.askerOf(ctx, in.GrantInput, mode)
	if err != nil {
		return store.AccessApproval{}, err
	}
	home := who.home.ProjectSlug
	if in.From != "" && in.From != home {
		return store.AccessApproval{}, fmt.Errorf("%w: the session works in %q, not %q", store.ErrInvalidAccessRequest, home, in.From)
	}
	kind, write := store.AccessKindGrant, false
	switch mode {
	case askKind:
		if kind, err = kindFor(ctx, g.svc, home, target); err != nil {
			return store.AccessApproval{}, err
		}
		write = in.Write && kind == store.AccessKindGrant
	default:
		switch home {
		case "":
			return store.AccessApproval{}, ErrNoProject
		case target:
			return store.AccessApproval{}, fmt.Errorf("%w: a session always reads its own project", ErrNotConfidential)
		}
	}
	if err := g.reachable(ctx, kind, mode, home, target); err != nil {
		return store.AccessApproval{}, err
	}
	if kind == store.AccessKindMove {
		if who.holder.Kind() == store.HolderSession || !who.home.Ambient || who.home.ExternalClient == "" || who.home.ExternalSessionID == "" {
			return store.AccessApproval{}, ErrNoConversation
		}
	} else if live, err := g.liveGrant(ctx, who.held, target, home, write, now); err != nil {
		return store.AccessApproval{}, err
	} else if live != nil {
		return store.AccessApproval{}, live
	}
	if pending, ok, err := holderRequest(ctx, g.svc, who.holder, target, home, true, now); err != nil {
		return store.AccessApproval{}, err
	} else if ok {
		return g.svc.explained(ctx, pending, in.Via, in.Reason, now)
	}
	if in.Via == ViaRefusal {
		last, ok, err := holderRequest(ctx, g.svc, who.holder, target, home, false, now)
		if err != nil {
			return store.AccessApproval{}, err
		}
		if ok && last.Status == store.ApprovalDenied {
			return store.AccessApproval{}, &DeniedError{Request: last}
		}
	}
	reason := strings.TrimSpace(in.Reason)
	if utf8.RuneCountInString(reason) > store.MaxAccessReasonRunes {
		return store.AccessApproval{}, fmt.Errorf("%w: the reason is limited to %d characters",
			store.ErrInvalidAccessRequest, store.MaxAccessReasonRunes)
	}
	var carry store.Carry
	if hi, err := store.IsolationOf(ctx, db, home); err != nil {
		return store.AccessApproval{}, err
	} else if kind == store.AccessKindMove || hi == core.IsolationConfidential {
		// An open home's read grant (RequestGrant) states no carry, as every
		// request did before migration 039: what it confines is the grant's own.
		if carry, err = g.svc.carryFor(ctx, who.held, home, target, kind, write, now); err != nil {
			return store.AccessApproval{}, err
		}
	}
	client, conversation := who.holder.Conversation()
	sess := who.home
	var a store.AccessApproval
	if kind == store.AccessKindMove {
		a, err = store.MoveRequestCreate(ctx, db, store.MoveRequest{
			Project: target, From: home, Carry: carry, Lease: g.svc.leaseCap(), TTL: RequestTTL,
			PeerUID: who.uid, PeerPID: who.pid, ProcessKey: who.processKey, Program: who.program,
			ConversationClient: client, ConversationID: conversation,
			SessionID: sess.ID, SessionName: sess.Name, CWD: sess.CWD, Repo: repoName(sess.CWD), Model: sess.Model, Reason: reason,
		}, now)
	} else {
		access := store.GrantAccessRead
		if write {
			access = store.GrantAccessWrite
		}
		a, err = store.CreateGrantRequest(ctx, db, store.GrantRequest{
			Project: target, From: home, Lease: g.svc.leaseCap(), TTL: RequestTTL,
			PeerUID: who.uid, PeerPID: who.pid, ProcessKey: who.processKey, Program: who.program,
			SessionID: sess.ID, SessionName: sess.Name, CWD: sess.CWD, Repo: repoName(sess.CWD), Model: sess.Model, Reason: reason,
			Access: access, Carry: carry, ConversationClient: client, ConversationID: conversation,
		}, now)
	}
	if err != nil {
		return store.AccessApproval{}, err
	}
	via := in.Via
	if via == "" {
		via = ViaRequest
	}
	payload := map[string]any{"kind": kind, "via": via, "from_project": home}
	if write {
		payload["access"] = store.GrantAccessWrite
	}
	g.svc.record(ctx, core.Event{Kind: core.EventAccessRequested, ItemID: a.ID, SessionID: a.SessionID, ProjectSlug: a.Project,
		Payload: payload})
	return a, nil
}

// askerOf settles who asks: the session (the call's binding, else the one
// session stamped with the agent process), the holder, and under peer_auth
// the anchoring agent process -- exactly as RequestGrant always did. With
// askKind the request's session is the conversation's home session: its
// ambient row, when the session names a conversation that has one still
// running as the caller's own.
func (g *Grants) askerOf(ctx context.Context, in GrantInput, mode askMode) (asker, error) {
	db := g.svc.cfg.DB
	var who asker
	sessionID := in.SessionID
	v, hasView := access.FromContext(ctx)
	if g.advisory {
		if hasView {
			// A View means peer_auth, whose asker is a process: an advisory
			// service never serves one.
			return asker{}, ErrNoProcess
		}
		if sessionID == "" && in.AgentProcess != "" {
			if found, err := ownAgentSessions(ctx, db, in.Host, in.AgentProcess); err == nil && len(found) == 1 {
				sessionID = found[0].ID
			}
		}
	} else {
		chain := v.Principal().Chain
		if !hasView || v.Principal().Kind != access.KindOwner || !v.Principal().Socket || len(chain) == 0 {
			return asker{}, ErrNoProcess
		}
		got, err := g.svc.anchorFor(ctx, chain, sessionID, in.AgentProcess, in.Host)
		if err != nil {
			return asker{}, err
		}
		sessionID, who.processKey, who.pid, who.uid, who.program = got.sessionID, got.token.Key(), got.token.PID, v.Principal().UID, got.program
	}
	if sessionID == "" {
		return asker{}, ErrNoSession
	}
	sess, ok, err := store.SessionByID(ctx, db, sessionID)
	if err != nil {
		return asker{}, err
	}
	if !ok || sess.Status == core.SessionCompleted {
		// A session the idle reaper expired is still its agent's (a call
		// revives it); one completed on purpose asks nothing.
		return asker{}, ErrNoSession
	}
	who.sess, who.home = sess, sess
	if mode == askKind && sess.ExternalClient != "" && sess.ExternalSessionID != "" {
		amb, found, err := store.AmbientSessionByExternalIdentity(ctx, db, sess.ExternalClient, sess.ExternalSessionID)
		if err != nil {
			return asker{}, err
		}
		if found && amb.Status != core.SessionCompleted && access.SessionAccountOK(ctx, amb.Account) {
			who.home = amb
		}
	}
	switch {
	case who.processKey != "":
		who.holder, err = store.HolderOfProcess(who.processKey)
	case sess.ExternalClient != "" && sess.ExternalSessionID != "":
		who.holder, err = store.HolderOfConversation(sess.ExternalClient, sess.ExternalSessionID)
		if err != nil {
			// A client name the holder key cannot carry: the session holds it,
			// as before migration 039.
			who.holder, err = store.HolderOfSession(sess.ID)
		}
	default:
		who.holder, err = store.HolderOfSession(sess.ID)
	}
	if err != nil {
		return asker{}, err
	}
	if who.held, err = heldBy(ctx, db, v, who.holder, sess); err != nil {
		return asker{}, err
	}
	return who, nil
}

// heldBy lists every holder whose rows are the asking conversation's, the way
// the clearance reads it (store.ConversationClearance): under peer_auth each
// process of the verified chain; else the conversation and the session of each
// of its own sessions -- its ambient row and the sessions linked to it, a
// move's pre-move row among them; else the session alone.
func heldBy(ctx context.Context, db *sql.DB, v access.View, holder store.Holder, sess core.Session) ([]store.Holder, error) {
	switch holder.Kind() {
	case store.HolderProcess:
		out := []store.Holder{holder}
		for _, k := range v.Principal().ChainKeys() {
			h, err := store.HolderOfProcess(k)
			if err != nil {
				return nil, err
			}
			if !slices.Contains(out, h) {
				out = append(out, h)
			}
		}
		return out, nil
	case store.HolderConversation:
		client, external := holder.Conversation()
		own, err := store.LinkedSessionsByExternalIdentity(ctx, db, client, external)
		if err != nil {
			return nil, err
		}
		if amb, found, err := store.AmbientSessionByExternalIdentity(ctx, db, client, external); err != nil {
			return nil, err
		} else if found {
			own = append(own, amb)
		}
		out := []store.Holder{holder}
		for _, s := range own {
			if !access.SessionAccountOK(ctx, s.Account) {
				continue
			}
			h, err := store.HolderOfSession(s.ID)
			if err != nil {
				return nil, err
			}
			if !slices.Contains(out, h) {
				out = append(out, h)
			}
		}
		return out, nil
	}
	return []store.Holder{holder}, nil
}

// reachable is peer_auth's first word on an ask: neither side may lie beyond
// this connection's reach. A locked target is the lock's to open, so
// RequestGrant never asks for one; an ask of a kind (askKind) may, and its
// approval carries the lease.
func (g *Grants) reachable(ctx context.Context, kind string, mode askMode, home, target string) error {
	targetOK := access.Visible(ctx, target)
	if !targetOK && mode == askKind {
		v, ok := access.FromContext(ctx)
		targetOK = ok && v.Locked(target)
	}
	homeOK := home == "" || access.Writable(ctx, home)
	if kind == store.AccessKindMove {
		// A move leaves its home: it need not write there, only work there.
		homeOK = home == "" || access.Visible(ctx, home) || access.Writable(ctx, home)
	}
	if !targetOK || !homeOK {
		return access.ErrNotReachable
	}
	if kind == store.AccessKindGrant {
		return g.eligible(ctx, target, home)
	}
	return nil
}

// holderRequest is holder's newest grant or move request for target from
// home: only one still waiting at now (pending), else whatever its status (a
// refusal reads it to tell the owner's denial from a request that lapsed).
// One waiting request per asker and project, whichever kind it is.
func holderRequest(ctx context.Context, s *Service, holder store.Holder, target, home string, pending bool, now time.Time) (store.AccessApproval, bool, error) {
	rows, err := store.HolderAccessApprovals(ctx, s.cfg.DB, []store.Holder{holder})
	if err != nil {
		return store.AccessApproval{}, false, err
	}
	for _, a := range rows {
		if a.Project != target || a.FromProject != home || !slices.Contains([]string{store.AccessKindGrant, store.AccessKindMove}, a.Kind) {
			continue
		}
		if pending && (a.Status != store.ApprovalPending || !a.ExpiresAt.After(now)) {
			continue
		}
		return a, true, nil
	}
	return store.AccessApproval{}, false, nil
}

// lockedNow reports whether project is locked by this daemon's policy as the
// store holds it: a force lock from the config, or the console's. The advisory
// tier's service knows no lock (peer_auth is off: nothing is locked).
func (s *Service) lockedNow(ctx context.Context, project string) (bool, error) {
	if s.lockless || project == "" {
		return false, nil
	}
	if slices.Contains(s.cfg.ForceLocked, project) {
		return true, nil
	}
	return store.HasConsoleLock(ctx, s.cfg.DB, project)
}

// eligibleMove is who may move, judged now: into a sealed project from any
// home, or into a confidential one from a home that is not fenced.
func (g *Grants) eligibleMove(ctx context.Context, home, target string) error {
	kind, err := kindFor(ctx, g.svc, home, target)
	if err != nil {
		return err
	}
	if kind != store.AccessKindMove {
		return fmt.Errorf("%w: %s now works in a confidential project, so it asks for a grant of %s instead", ErrNotAMove, home, target)
	}
	return nil
}

// MoveApproved is what an approved move did (Grants.ApproveMove).
type MoveApproved = store.MoveApproved

// ApproveMove decides a move request: the facts digest the owner saw, the
// fence as it stands now, and the proof -- ProofKey only in the advisory
// tier, the approve core's proofs (Service.prove) otherwise. The move, the
// session it ends, the child it opens and, for a locked target under
// peer_auth, the target's lease for the chosen duration are one transaction
// (store.MoveApproveWith). A move into a project no lock covers lasts for good:
// the duration chosen binds the proof and nothing else.
func (g *Grants) ApproveMove(ctx context.Context, d Decision) (MoveApproved, error) {
	const op = "approval.Grants.ApproveMove"
	if err := refuseRestricted(ctx); err != nil {
		return MoveApproved{}, fmt.Errorf("%s: %w", op, err)
	}
	db := g.svc.cfg.DB
	length, err := g.svc.LeaseLength(d.Duration)
	if err != nil {
		return MoveApproved{}, fmt.Errorf("%s: %w", op, err)
	}
	a, err := store.AccessApprovalByID(ctx, db, d.ID)
	if err != nil {
		return MoveApproved{}, fmt.Errorf("%s: %w", op, err)
	}
	if a.Kind != store.AccessKindMove {
		return MoveApproved{}, fmt.Errorf("%s: %w: %s", op, ErrNotAMove, ShortID(a.ID))
	}
	if a.Status != store.ApprovalPending {
		return MoveApproved{}, fmt.Errorf("%s: %w: %s is %s", op, store.ErrApprovalNotPending, ShortID(a.ID), a.Status)
	}
	if err := storedAnchorRefusal(a.PeerPID, a.Program); err != nil {
		return MoveApproved{}, fmt.Errorf("%s: %s: %w", op, ShortID(a.ID), err)
	}
	facts, err := g.svc.factsOf(ctx, a)
	if err != nil {
		return MoveApproved{}, fmt.Errorf("%s: %w", op, err)
	}
	if d.Digest == "" || d.Digest != facts.Digest {
		return MoveApproved{}, fmt.Errorf("%s: %w", op, ErrFactsChanged)
	}
	word, err := accessWord(facts, d)
	if err != nil {
		return MoveApproved{}, fmt.Errorf("%s: %w", op, err)
	}
	if d.Proof.Kind == ProofTouchID {
		return MoveApproved{}, fmt.Errorf("%s: %w", op, ErrTouchIDMove)
	}
	// The fence as it stands now: a home made confidential, or a target
	// loosened to open, while the request waited is not moved by it.
	if err := g.eligibleMove(ctx, a.FromProject, a.Project); err != nil {
		return MoveApproved{}, fmt.Errorf("%s: %w", op, err)
	}
	now := g.svc.cfg.Now()
	decidedBy, err := g.decidedBy(ctx, a, facts, d, word, now)
	if err != nil {
		return MoveApproved{}, fmt.Errorf("%s: %w", op, err)
	}
	var lease time.Duration
	if !g.advisory && a.ProcessKey != "" && facts.Locked {
		lease = length
	}
	done, err := store.MoveApproveWith(ctx, db, store.MoveDecision{ID: a.ID, Challenge: a.Challenge, DecidedBy: decidedBy, Lease: lease}, now)
	if err != nil {
		return MoveApproved{}, fmt.Errorf("%s: %w", op, err)
	}
	payload := map[string]any{"kind": store.AccessKindMove, "move": done.Move.ID, "from_project": a.FromProject,
		"session": done.Child.ID, "proof": d.Proof.Kind, "duration": d.Duration}
	if done.Lease != nil {
		payload["lease"] = done.Lease.ID
		payload["seconds"] = int64(done.Lease.ExpiresAt.Sub(done.Lease.GrantedAt) / time.Second)
	}
	g.svc.record(ctx, core.Event{Kind: core.EventAccessApproved, ItemID: a.ID, SessionID: done.Child.ID, ProjectSlug: a.Project,
		Payload: payload})
	// The sessions the move ended and opened, as the console's session pages
	// read them: metadata only, never what the conversation did.
	g.svc.record(ctx, core.Event{Kind: core.EventSessionEnded, SessionID: done.Left.ID, ProjectSlug: done.Left.ProjectSlug,
		Payload: map[string]any{"moved_to": done.Child.ID, "target_project": a.Project, "ambient": true}})
	g.svc.record(ctx, core.Event{Kind: core.EventSessionStarted, SessionID: done.Child.ID, ProjectSlug: done.Child.ProjectSlug,
		Payload: map[string]any{"moved_from": done.Left.ID, "from_project": a.FromProject, "ambient": true}})
	return done, nil
}

// decidedBy checks a grant's or a move's proof and names who decided: the
// bearer key in the advisory tier (ProofKey, refused under peer_auth),
// otherwise the approve core's proof over word, the access chosen.
func (g *Grants) decidedBy(ctx context.Context, a store.AccessApproval, facts Facts, d Decision, word string, now time.Time) (string, error) {
	switch {
	case d.Proof.Kind == ProofKey && g.advisory:
		return decidedByKey, nil
	case d.Proof.Kind == ProofKey:
		return "", ErrPeerProof
	}
	return g.svc.prove(ctx, a, facts, d, word, now)
}
