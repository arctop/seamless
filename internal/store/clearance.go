package store

// The conversation's clearance (isolation.requests, plan fenced-sessions P2):
// the loader behind the one rule. It reads P1's rows -- grants, the label,
// flows and moves -- for the holder a request speaks for and hands them to
// access.JudgeConversation, whose state store.CanWrite asks (WriteCleared,
// WriteGranted) and store.CanRead reads (ReadGranted).
//
// Who the holder is, and never anyone else:
//
//   - under peer_auth (a View in ctx): the owner's socket connection's
//     kernel-verified process chain, as read grants and leases key today. A
//     TCP request or a restricted account's holds nothing. The conversation
//     the surface names supplies only the home and the projects of the tree's
//     own sessions, and only when its ambient row is stamped with a process of
//     that chain (the stamp is the hook's claim; the chain is the kernel's);
//   - without peer_auth: the conversation the surface names (the agent client
//     and the session id it issued, ConversationRef), plus every session of
//     that conversation as a session holder, since a grant filed before
//     migration 039 (and every advisory grant the previous release filed)
//     names the session that asked;
//   - a client that sends no external identity: the one session it is bound
//     to, as before.
//
// The surface finds the conversation (the MCP edge from the agent-process
// identity, the hooks from the request body); nothing here infers one.

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
)

// ConversationRef names the conversation a request speaks for, as its surface
// identified it.
type ConversationRef struct {
	// Client and ExternalID are the conversation: the agent client and the
	// session id it issued (sessions.external_client and claude_session_id, a
	// restricted account's id already qualified), both or neither.
	Client, ExternalID string
	// SessionID is the session a client with no external identity is bound
	// to: its holder when Client and ExternalID are empty, as before
	// migration 039. Ignored under peer_auth.
	SessionID string
}

func (r ConversationRef) conversation() bool { return r.Client != "" && r.ExternalID != "" }

// conversationKey is the one encoding of a conversation that
// access.Conversation.Key and access.Grant.Conversation share.
func conversationKey(client, externalID string) string { return client + "/" + externalID }

// clearanceHolders is a conversation resolved for its clearance.
type clearanceHolders struct {
	// holders are every holder whose rows are the conversation's.
	holders []Holder
	// key and sessions are what access.JudgeConversation matches grant rows
	// against without peer_auth.
	key      string
	sessions []string
	// own are the conversation's own sessions (ambient row, links, a move's
	// children), for the native-home flows.
	own []core.Session
	// tree are the projects of every session of the agent process tree,
	// under peer_auth (a process holder's sessions are not keyed to it).
	tree []string
	home string
	// homeKnown: home is the project of the conversation's own ambient row
	// (access.Conversation.HomeKnown).
	homeKnown bool
}

// ConversationClearance judges the conversation ref names, for the holder ctx
// speaks for, at now (access.JudgeConversation): its grants, its label, the
// flows approved into fenced projects, and its home. A request that names no
// conversation and no session -- or a View that holds nothing -- judges to the
// zero access.Grants, which refuses nothing.
//
// The flows are P1's (HolderFlows: recorded by approvals, implied by grants
// and moves), kept only into fenced projects, plus one rule of P0's
// (RecordDestinationOf, 217c1414): a conversation native to a fenced project
// keeps its record in that home whatever its own sessions reach, so the
// fenced project of each of its own sessions flows into a fenced home. The
// home is the one place its record goes, and the clearance agrees.
func ConversationClearance(ctx context.Context, db *sql.DB, ref ConversationRef, now time.Time) (access.Grants, error) {
	const op = "store.ConversationClearance"
	c, ok, err := clearanceHoldersOf(ctx, db, ref)
	if err != nil {
		return access.Grants{}, fmt.Errorf("%s: %w", op, err)
	}
	if !ok {
		return access.Grants{}, nil
	}
	conv, err := c.read(ctx, db)
	if err != nil {
		return access.Grants{}, fmt.Errorf("%s: %w", op, err)
	}
	return access.JudgeConversation(ctx, now, conv), nil
}

// clearanceHoldersOf resolves who ref's conversation is for the holder ctx
// speaks for. ok is false when nothing identifies one.
func clearanceHoldersOf(ctx context.Context, db *sql.DB, ref ConversationRef) (clearanceHolders, bool, error) {
	var c clearanceHolders
	if v, hasView := access.FromContext(ctx); hasView {
		p := v.Principal()
		if p.Kind != access.KindOwner || !p.Socket || !p.HasProcess {
			return clearanceHolders{}, false, nil
		}
		for _, k := range p.ChainKeys() {
			h, err := HolderOfProcess(k)
			if err != nil {
				return clearanceHolders{}, false, err
			}
			c.holders = append(c.holders, h)
		}
		if len(c.holders) == 0 {
			return clearanceHolders{}, false, nil
		}
		if !ref.conversation() {
			return c, true, nil
		}
		amb, found, err := AmbientSessionByExternalIdentity(ctx, db, ref.Client, ref.ExternalID)
		if err != nil {
			return clearanceHolders{}, false, err
		}
		if !found || !access.SessionAccountOK(ctx, amb.Account) {
			return c, true, nil
		}
		procs, err := SessionAgentProcesses(ctx, db, []string{amb.ID})
		if err != nil {
			return clearanceHolders{}, false, err
		}
		stamp := procs[amb.ID].AgentProcess
		if stamp == "" || !slices.ContainsFunc(p.Chain, func(t access.ProcessToken) bool { return t.AgentID() == stamp }) {
			// Another tree's conversation, or none of ours: it names no home.
			return c, true, nil
		}
		c.home, c.homeKnown = amb.ProjectSlug, true
		if c.own, err = conversationSessions(ctx, db, amb, ref); err != nil {
			return clearanceHolders{}, false, err
		}
		if c.tree, err = treeSessionProjects(ctx, db, amb.Host, stamp); err != nil {
			return clearanceHolders{}, false, err
		}
		return c, true, nil
	}

	if ref.conversation() {
		h, err := HolderOfConversation(ref.Client, ref.ExternalID)
		if err != nil {
			return clearanceHolders{}, false, err
		}
		c.holders, c.key = []Holder{h}, conversationKey(ref.Client, ref.ExternalID)
		amb, found, err := AmbientSessionByExternalIdentity(ctx, db, ref.Client, ref.ExternalID)
		if err != nil {
			return clearanceHolders{}, false, err
		}
		if found && access.SessionAccountOK(ctx, amb.Account) {
			c.home, c.homeKnown = amb.ProjectSlug, true
		}
		if c.own, err = conversationSessions(ctx, db, amb, ref); err != nil {
			return clearanceHolders{}, false, err
		}
		for _, s := range c.own {
			sh, err := HolderOfSession(s.ID)
			if err != nil {
				return clearanceHolders{}, false, err
			}
			c.holders = append(c.holders, sh)
			c.sessions = append(c.sessions, s.ID)
		}
		return c, true, nil
	}

	if ref.SessionID == "" {
		return clearanceHolders{}, false, nil
	}
	h, err := HolderOfSession(ref.SessionID)
	if err != nil {
		return clearanceHolders{}, false, err
	}
	c.holders, c.sessions = []Holder{h}, []string{ref.SessionID}
	sess, found, err := SessionByID(ctx, db, ref.SessionID)
	if err != nil {
		return clearanceHolders{}, false, err
	}
	if found {
		c.home, c.own = sess.ProjectSlug, []core.Session{sess}
	}
	return c, true, nil
}

// conversationSessions returns the conversation's own sessions: its ambient
// row (amb, when one was found) and every explicit session sharing its identity,
// in any status -- the links session_start made, and a move's pre-move row
// once the move flipped it.
func conversationSessions(ctx context.Context, db *sql.DB, amb core.Session, ref ConversationRef) ([]core.Session, error) {
	links, err := LinkedSessionsByExternalIdentity(ctx, db, ref.Client, ref.ExternalID)
	if err != nil {
		return nil, err
	}
	if amb.ID == "" {
		return links, nil
	}
	return append([]core.Session{amb}, links...), nil
}

// treeSessionProjects returns the projects of every session of the
// conversations whose ambient rows on host are stamped with the agent process
// stamp: under peer_auth the label stays with the process tree, so a
// conversation /clear ended still counts.
func treeSessionProjects(ctx context.Context, db *sql.DB, host, stamp string) ([]string, error) {
	var out []string
	err := eachRow(ctx, db, `SELECT DISTINCT s.project_slug FROM sessions s
		WHERE (s.external_client, s.claude_session_id) IN (
			SELECT a.external_client, a.claude_session_id FROM sessions a
			WHERE a.ambient = 1 AND a.host = ? AND a.agent_process = ? AND a.claude_session_id <> '')
		ORDER BY s.project_slug`, []any{host, stamp}, func(rows *sql.Rows) error {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return err
		}
		out = append(out, slug)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("sessions of the agent's tree: %w", err)
	}
	return out, nil
}

// read loads the conversation's rows for c's holders, in lookups of at most
// holderMaxLookup holders each (a conversation may have linked more sessions
// than a process chain has hops), and builds what access.JudgeConversation
// judges.
func (c clearanceHolders) read(ctx context.Context, db *sql.DB) (access.Conversation, error) {
	conv := access.Conversation{Key: c.key, Sessions: c.sessions, Home: c.home, HomeKnown: c.homeKnown}
	label := map[string]bool{}
	flows := map[FlowPair]flowRecord{}
	seen := map[string]bool{}
	for chunk := range slices.Chunk(c.holders, holderMaxLookup) {
		entries, err := HolderLabel(ctx, db, chunk, c.home)
		if err != nil {
			return access.Conversation{}, err
		}
		for _, e := range entries {
			label[e.Project] = true
		}
		hf, err := HolderFlows(ctx, db, chunk)
		if err != nil {
			return access.Conversation{}, err
		}
		for _, f := range hf {
			keepFlowRecord(flows, FlowPair{From: f.From, To: f.To},
				flowRecord{via: stampVia(f.ApprovalKind), ref: f.ApprovalID, at: f.At})
		}
		grants, err := HolderAccessGrants(ctx, db, chunk)
		if err != nil {
			return access.Conversation{}, err
		}
		for _, g := range grants {
			if !seen[g.ID] {
				seen[g.ID] = true
				conv.Grants = append(conv.Grants, clearanceGrant(g))
			}
		}
	}
	if len(c.tree) > 0 {
		fenced, err := IsolatedSlugs(ctx, db, c.tree)
		if err != nil {
			return access.Conversation{}, err
		}
		for _, f := range fenced {
			label[f.Slug] = true
		}
	}
	// The native home (RecordDestinationOf): a fenced home keeps its record
	// whatever the conversation's own sessions reach. No approval cleared such
	// a flow, so a stamp names the conversation's own session there.
	if c.home != "" && label[c.home] {
		for _, s := range c.own {
			if s.ProjectSlug != c.home && label[s.ProjectSlug] {
				keepFlowRecord(flows, FlowPair{From: s.ProjectSlug, To: c.home},
					flowRecord{via: core.StampViaSession, ref: s.ID, at: s.CreatedAt, native: true})
			}
		}
	}
	// Only a flow into a fenced project can clear anything.
	var dests []string
	for f := range flows {
		if !label[f.To] {
			dests = append(dests, f.To)
		}
	}
	slices.Sort(dests)
	fencedTo := maps.Clone(label)
	if len(dests) > 0 {
		fenced, err := IsolatedSlugs(ctx, db, slices.Compact(dests))
		if err != nil {
			return access.Conversation{}, err
		}
		for _, f := range fenced {
			fencedTo[f.Slug] = true
		}
	}
	for f, rec := range flows {
		if f.From != "" && f.To != "" && f.From != f.To && fencedTo[f.To] {
			conv.Flows = append(conv.Flows, access.Flow{From: f.From, To: f.To, Via: rec.via, Ref: rec.ref})
		}
	}
	slices.SortFunc(conv.Flows, func(a, b access.Flow) int {
		return strings.Compare(a.From+"\x00"+a.To, b.From+"\x00"+b.To)
	})
	conv.Label = slices.Sorted(maps.Keys(label))
	return conv, nil
}

// flowRecord is what cleared one flow, for the stamp a write by it carries:
// an approval's kind and id, or the conversation's own session (native).
type flowRecord struct {
	via, ref string
	at       time.Time
	native   bool
}

// keepFlowRecord records rec for pair unless a record that names it better is
// there already: an approval before a native flow, then the earliest, then
// the smallest reference -- so a stamp names one record whichever lookup
// chunk found it.
func keepFlowRecord(flows map[FlowPair]flowRecord, pair FlowPair, rec flowRecord) {
	cur, ok := flows[pair]
	switch {
	case !ok:
	case cur.native != rec.native:
		if rec.native {
			return
		}
	case rec.at.After(cur.at), rec.at.Equal(cur.at) && rec.ref >= cur.ref:
		return
	}
	flows[pair] = rec
}

// stampVia is the stamp word (core.StampVia*) for an approval kind; "" for a
// kind no stamp names.
func stampVia(kind string) string {
	switch kind {
	case AccessKindGrant:
		return core.StampViaGrant
	case AccessKindMove:
		return core.StampViaMove
	}
	return ""
}

// clearanceGrant is a grant row as the access layer judges it, by the holder
// the row names (AccessGrant.Holder): a conversation's grant is the
// conversation's, never the session that asked for it, and it carries the
// access the owner approved.
func clearanceGrant(g AccessGrant) access.Grant {
	out := access.Grant{ID: g.ID, Project: g.Project, From: g.FromProject, Expires: g.ExpiresAt,
		Ended: g.RevokedAt != nil, Write: g.Access == GrantAccessWrite}
	switch h := g.Holder(); h.Kind() {
	case HolderProcess:
		out.Process = h.ProcessKey()
	case HolderConversation:
		out.Conversation = conversationKey(h.Conversation())
	case HolderSession:
		out.Session = h.SessionID()
	}
	return out
}
