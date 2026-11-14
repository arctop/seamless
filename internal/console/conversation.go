package console

// A conversation on its sessions' pages (plan fenced-sessions P10). A
// conversation is one agent context -- the client's own session, found by its
// client and external session id -- and every session of it shows one card:
//
//   - the home chain: the session it started in and each move's child
//     (parent_session_id, access_moves), with the move that led there;
//   - every project it reached -- its homes, the projects session_start bound
//     it to, its grants (read, or read and write), its leases and the requests
//     that are still waiting or were refused -- each with the approval and who
//     decided it;
//   - what its sessions wrote, by project, each item with its provenance
//     stamps.
//
// Everything is read through the request's handle (s.db, s.eventLog), so a
// locked project this request does not read contributes no session, no item,
// no count and no stamp: a hop into it shows the project's name, the approval
// that moved the conversation there, and the way to unlock it in this browser
// -- nothing from inside (Addendum 7 item 5 of locked-projects-approval-design).
// The access tables the record comes from are metadata no handle hides
// (store.viewExempt), the same the Access requests page shows.
//
// The card is opt-in by its data: a conversation that never moved, asked for
// nothing and wrote nothing stamped -- every conversation of an install that
// never turned isolation.requests on -- renders no card, and the page is as it
// was.

import (
	"cmp"
	"context"
	"net/url"
	"slices"
	"time"

	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

const (
	// convEventScan bounds the write events read per session.
	convEventScan = 500
	// convItemsShown bounds the items one project's group lists.
	convItemsShown = 50
)

// convWriteKinds are the events that say a session wrote an item.
var convWriteKinds = []core.EventKind{
	core.EventMemoryWritten, core.EventNoteWritten, core.EventPlanCaptured, core.EventPlanApproved,
	core.EventSubagentCaptured, core.EventTrialRecorded, core.EventTaskTransition,
}

// What a project's way in was (convWay.Kind).
const (
	convWayHome     = "home"     // the conversation started here
	convWayMoved    = "moved"    // a move brought it here
	convWayBound    = "bound"    // session_start bound one of its sessions here
	convWayRead     = "read"     // a read-only grant
	convWayWrite    = "write"    // a read-and-write grant
	convWayUnlocked = "unlocked" // a lock's request, approved: a lease
	convWayAsked    = "asked"    // a request still waiting, refused, or expired
)

// The decided_by values the approval surfaces record besides an approver
// credential's id (internal/approval: the terminal's commit as the daemon's
// account, the advisory tier's bearer key and in-session dialog; this
// console's Deny).
const (
	convDecidedByDaemon  = "daemon-account"
	convDecidedByKey     = "bearer-key"
	convDecidedByConsole = "console"
)

// conversationView is the Conversation card. JSON callers get it under the
// session's "conversation" key, absent when there is no card.
type conversationView struct {
	// Hops is the home chain, oldest first.
	Hops []convHop `json:"hops"`
	// Reach is every project the conversation reached, in the order it
	// reached them.
	Reach []convReach `json:"reach"`
	// Written is what its sessions wrote, by project, in Reach's order then
	// by project.
	Written []convProject `json:"written"`
	// LockedOut are the locked projects among its homes and write
	// destinations this request does not read: what it wrote there stays
	// out until this browser unlocks them.
	LockedOut []convLocked `json:"lockedOut,omitempty"`
	// Err says the record could not be read; the card says so instead of
	// showing part of it.
	Err string `json:"error,omitempty"`
}

// convHop is one home of the conversation.
type convHop struct {
	Project string `json:"project"`
	// Locked marks a project this request does not read: the hop shows its
	// name and nothing inside -- no session, no status.
	Locked bool   `json:"locked,omitempty"`
	Unlock string `json:"unlock,omitempty"`
	// SessionID, Name and Status are the session the conversation worked in
	// there, "" when it is not here (a locked project, or a merge that
	// brought a child without its parent).
	SessionID string `json:"sessionId,omitempty"`
	Name      string `json:"name,omitempty"`
	Status    string `json:"status,omitempty"`
	// Current marks the session this page is about.
	Current bool `json:"current,omitempty"`
	// Move is the move that brought the conversation here; nil for the
	// first hop.
	Move *convDecision `json:"move,omitempty"`
}

// convDecision is an approval and who decided it.
type convDecision struct {
	ApprovalID string `json:"approvalId"`
	// Short is the request's short form, as the Access requests page and
	// seam approve name it (R- and its last four symbols).
	Short string `json:"short"`
	// Link is the request's page, "" while the console serves no access
	// pages.
	Link string `json:"link,omitempty"`
	// DecidedBy names who decided, in the owner's words; "" while undecided.
	DecidedBy string    `json:"decidedBy,omitempty"`
	At        time.Time `json:"at"`
}

// convReach is one project the conversation reached, and how.
type convReach struct {
	Project string    `json:"project"`
	Locked  bool      `json:"locked,omitempty"`
	Unlock  string    `json:"unlock,omitempty"`
	Ways    []convWay `json:"ways"`
}

// Key is the project's id fragment ("_global" for the global scope).
func (r convReach) Key() string { return convKey(r.Project) }

// convWay is one way the conversation reached a project.
type convWay struct {
	Kind  string `json:"kind"`
	Words string `json:"words"`
	// State is a grant's or a request's state ("live", "ended", "revoked",
	// "pending", "denied", "expired"); "" for a home. Until is when a live
	// grant ends.
	State string     `json:"state,omitempty"`
	Until *time.Time `json:"until,omitempty"`
	// Session names the session bound there (convWayBound).
	Session   string        `json:"session,omitempty"`
	SessionID string        `json:"sessionId,omitempty"`
	Decision  *convDecision `json:"decision,omitempty"`
}

// Tone is the way's chip tone (console.css .kind.*).
func (w convWay) Tone() string {
	switch w.Kind {
	case convWayHome, convWayMoved:
		return "brand"
	case convWayRead:
		return "ok"
	case convWayWrite:
		return "warn"
	case convWayUnlocked:
		return "pop"
	}
	return ""
}

// convProject is what the conversation wrote in one project.
type convProject struct {
	Project string     `json:"project"`
	Items   []convItem `json:"items"`
	// More counts the items past convItemsShown.
	More int `json:"more,omitempty"`
}

// Key is the project's id fragment ("_global" for the global scope).
func (p convProject) Key() string { return convKey(p.Project) }

// convItem is one item the conversation wrote.
type convItem struct {
	// Kind is the item's kind (core.ItemKind*), or "findings" for a
	// session's findings.
	Kind  string `json:"kind"`
	ID    string `json:"id"`
	Title string `json:"title"`
	Href  string `json:"href"`
	// MayContain is the item's provenance stamps (console stamp.go).
	MayContain []core.Stamp `json:"mayContain,omitempty"`
	at         time.Time
}

// convLocked is a locked project whose part of the record stays out.
type convLocked struct {
	Project string `json:"project"`
	Unlock  string `json:"unlock"`
}

// convFindings is the item kind of a session's findings.
const convFindings = "findings"

// convKey is a project's id fragment.
func convKey(project string) string {
	if project == "" {
		return "_global"
	}
	return project
}

// conversationFor builds the Conversation card for the page of sess, nil when
// the conversation has nothing to show: no move, no grant or move request, no
// grant, and nothing it wrote that carries a stamp (convReached). trials says
// whether the research feature's trials are shown.
func (s *Service) conversationFor(ctx context.Context, sess core.Session, trials bool) (*conversationView, error) {
	db := s.db(ctx)
	sessions, err := store.ConversationSessionsOf(ctx, db, sess)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(sessions))
	for i, x := range sessions {
		ids[i] = x.ID
	}
	rec, err := store.ConversationRecordOf(ctx, db, ids)
	if err != nil {
		return nil, err
	}
	reached := convReached(rec)
	if !reached {
		stamped, err := store.AnyProvenanceStamp(ctx, db)
		if err != nil || !stamped {
			return nil, err
		}
	}
	written, stamped, err := s.convWritten(ctx, sessions, trials)
	if err != nil {
		return nil, err
	}
	if !reached && !stamped {
		return nil, nil
	}
	decide, err := s.convDecider(ctx, rec)
	if err != nil {
		return nil, err
	}
	// A locked project's unlock page comes back here once this browser
	// unlocks it: this session's page, which the request reads.
	back := "/console/sessions/" + sess.ID
	v := &conversationView{Hops: s.convHops(ctx, sess, sessions, rec, decide, back)}
	v.Reach = s.convReach(ctx, sessions, v.Hops, rec, decide, back, time.Now())
	v.Written = convWrittenInOrder(written, v.Reach)
	v.LockedOut = convLockedOut(ctx, v.Hops, rec, back)
	return v, nil
}

// convReached reports whether rec holds what only isolation.requests files: a
// move, a grant, or a grant's or a move's request. A lock's own requests and
// leases (peer_auth) are listed on a card that shows for these, but give a
// session no card on their own: with isolation.requests off a locked
// project's sessions render as they did.
func convReached(rec store.ConversationRecord) bool {
	if len(rec.Moves) > 0 || len(rec.Grants) > 0 {
		return true
	}
	return slices.ContainsFunc(rec.Approvals, func(a store.AccessApproval) bool {
		return a.Kind == store.AccessKindGrant || a.Kind == store.AccessKindMove
	})
}

// convUnlock is the unlock page for a locked project, naming the session page
// it came from (back) as the way back once this browser unlocks it.
func convUnlock(project, back string) string {
	return "/console/unlock/" + url.PathEscape(project) + "?item=" + lockedItemSession + "&next=" + url.QueryEscape(back)
}

// convHops builds the home chain: the moves between the conversation's
// sessions, and the parent_session_id links a move's child carries (a merge
// may bring a child without its move), walked from the session the
// conversation started in. A session the request's handle hides -- one in a
// locked project -- is a hop with its project's name only; a parent that is
// not here, with no move naming its project, is left out. Without a move or a
// link the chain is the conversation's ambient row, or -- for a session whose
// client sends no external identity, a conversation of its own -- the session
// itself.
func (s *Service) convHops(ctx context.Context, current core.Session, sessions []core.Session, rec store.ConversationRecord,
	decide func(string) string, back string) []convHop {
	byID := make(map[string]core.Session, len(sessions))
	for _, x := range sessions {
		byID[x.ID] = x
	}
	next, prev := map[string]string{}, map[string]string{}
	// where is a hop's project as a move names it (the session it left, the
	// child it entered), for a session the handle does not hold.
	where := map[string]string{}
	named := map[string]bool{}
	into := map[string]store.Move{}
	for _, m := range rec.Moves {
		_, left := next[m.FromSessionID]
		_, entered := prev[m.ToSessionID]
		if left || entered {
			continue // a session is left and entered once; another row is not this chain's
		}
		next[m.FromSessionID], prev[m.ToSessionID] = m.ToSessionID, m.FromSessionID
		into[m.ToSessionID] = m
		if !named[m.FromSessionID] {
			where[m.FromSessionID], named[m.FromSessionID] = m.FromProject, true
		}
		where[m.ToSessionID], named[m.ToSessionID] = m.ToProject, true
	}
	for _, x := range sessions {
		if x.ParentSessionID == "" || prev[x.ID] != "" {
			continue
		}
		if _, left := next[x.ParentSessionID]; left {
			continue
		}
		next[x.ParentSessionID], prev[x.ID] = x.ID, x.ParentSessionID
	}

	root := ""
	switch {
	case len(next) == 0:
		for _, x := range sessions {
			if x.Ambient {
				root = x.ID
			}
		}
		if root == "" && current.ExternalSessionID == "" {
			root = current.ID
		}
		if root == "" {
			return nil
		}
	case prev[current.ID] != "" || next[current.ID] != "":
		root = current.ID
	case len(rec.Moves) > 0:
		root = rec.Moves[0].FromSessionID
	default:
		for from := range next {
			if root == "" || from < root {
				root = from
			}
		}
	}
	for seen := map[string]bool{root: true}; prev[root] != "" && !seen[prev[root]]; {
		root = prev[root]
		seen[root] = true
	}

	var hops []convHop
	for id, seen := root, map[string]bool{}; id != "" && !seen[id]; id = next[id] {
		seen[id] = true
		x, here := byID[id]
		hop := convHop{Current: id == current.ID}
		switch {
		case here:
			hop.Project = x.ProjectSlug
		case named[id]:
			hop.Project = where[id]
		default:
			continue // a parent not here, and no move says where it was
		}
		if m, ok := into[id]; ok {
			hop.Move = s.convMoveDecision(m, decide)
		}
		if lockedHere(ctx, hop.Project) {
			hop.Locked, hop.Unlock = true, convUnlock(hop.Project, back)
		} else if here {
			hop.SessionID, hop.Name, hop.Status = x.ID, x.Name, string(x.Status)
			if hop.Name == "" {
				hop.Name = shortID(x.ID)
			}
		}
		hops = append(hops, hop)
	}
	return hops
}

// convDecision words an approval: its short form, its page, who decided and
// when.
func (s *Service) convDecision(approvalID, decidedBy string, at time.Time, decide func(string) string) *convDecision {
	d := &convDecision{ApprovalID: approvalID, Short: approval.ShortID(approvalID), DecidedBy: decide(decidedBy), At: at}
	if s.cfg.Approvals != nil || s.cfg.Grants != nil {
		d.Link = "/console/access/" + url.PathEscape(approvalID)
	}
	return d
}

// convMoveDecision is a move's approval.
func (s *Service) convMoveDecision(m store.Move, decide func(string) string) *convDecision {
	return s.convDecision(m.ApprovalID, m.DecidedBy, m.MovedAt, decide)
}

// convDecider returns how a decided_by value reads: the approval surfaces'
// fixed values in words, an approver credential by its kind and name. The
// credentials are read only when the record names one.
func (s *Service) convDecider(ctx context.Context, rec store.ConversationRecord) (func(string) string, error) {
	need := false
	check := func(by string) {
		switch by {
		case "", convDecidedByDaemon, convDecidedByKey, convDecidedByConsole, approval.DecidedByDialog:
		default:
			need = true
		}
	}
	for _, m := range rec.Moves {
		check(m.DecidedBy)
	}
	for _, a := range rec.Approvals {
		check(a.DecidedBy)
	}
	for _, g := range rec.Grants {
		check(g.DecidedBy)
	}
	creds := map[string]store.ApproverCredential{}
	if need {
		all, err := store.ApproverCredentials(ctx, s.db(ctx), "")
		if err != nil {
			return nil, err
		}
		for _, c := range all {
			creds[c.ID] = c
		}
	}
	return func(by string) string {
		switch by {
		case "":
			return ""
		case convDecidedByDaemon:
			return "the terminal (seamlessd approve)"
		case convDecidedByKey:
			return "the bearer key: seam approve or the console (advisory)"
		case approval.DecidedByDialog:
			return "the agent's approval dialog (advisory)"
		case convDecidedByConsole:
			return "this console"
		}
		c, ok := creds[by]
		switch {
		case !ok:
			return "a passkey or key since removed"
		case c.Kind == store.ApproverTouchID:
			return "Touch ID key " + c.Name
		default:
			return "passkey " + c.Name
		}
	}, nil
}

// convReach lists every project the conversation reached, in the order it
// reached them: its homes along the chain, then -- by when they happened --
// the projects session_start bound its sessions to, its grants, its leases
// and its requests that were never approved. A grant's and a move's own
// requests are shown as the grant and the move.
func (s *Service) convReach(ctx context.Context, sessions []core.Session, hops []convHop, rec store.ConversationRecord,
	decide func(string) string, back string, now time.Time) []convReach {
	var out []convReach
	add := func(project string, w convWay) {
		i := slices.IndexFunc(out, func(r convReach) bool { return r.Project == project })
		if i < 0 {
			r := convReach{Project: project}
			if lockedHere(ctx, project) {
				r.Locked, r.Unlock = true, convUnlock(project, back)
			}
			out = append(out, r)
			i = len(out) - 1
		}
		out[i].Ways = append(out[i].Ways, w)
	}
	for i, h := range hops {
		if i == 0 {
			add(h.Project, convWay{Kind: convWayHome, Words: "home"})
			continue
		}
		add(h.Project, convWay{Kind: convWayMoved, Words: "moved in", Decision: h.Move})
	}

	type timed struct {
		at      time.Time
		project string
		way     convWay
	}
	var later []timed
	onChain := map[string]bool{}
	for _, m := range rec.Moves {
		onChain[m.FromSessionID], onChain[m.ToSessionID] = true, true
	}
	for _, h := range hops {
		if h.SessionID != "" {
			onChain[h.SessionID] = true
		}
	}
	for _, x := range sessions {
		if x.Ambient || x.ParentSessionID != "" || onChain[x.ID] {
			continue
		}
		name := x.Name
		if name == "" {
			name = shortID(x.ID)
		}
		later = append(later, timed{at: x.CreatedAt, project: x.ProjectSlug,
			way: convWay{Kind: convWayBound, Words: "bound with session_start", Session: name, SessionID: x.ID}})
	}
	shown := map[string]bool{}
	for _, m := range rec.Moves {
		shown[m.ApprovalID] = true
	}
	for _, g := range rec.Grants {
		shown[g.ApprovalID] = true
		w := convWay{Kind: convWayRead, Words: "read-only grant", Decision: s.convDecision(g.ApprovalID, g.DecidedBy, g.GrantedAt, decide)}
		if g.Access == store.GrantAccessWrite {
			w.Kind, w.Words = convWayWrite, "read and write grant"
		}
		switch {
		case g.RevokedAt != nil:
			w.State = "revoked"
		case !g.ExpiresAt.After(now):
			w.State = "ended"
		default:
			w.State = "live"
			until := g.ExpiresAt
			w.Until = &until
		}
		later = append(later, timed{at: g.GrantedAt, project: g.Project, way: w})
	}
	for _, a := range rec.Approvals {
		if shown[a.ID] {
			continue
		}
		w := convWay{Kind: convWayAsked, State: a.Status}
		if a.Status == store.ApprovalPending && !now.Before(a.ExpiresAt) {
			w.State = store.ApprovalExpired
		}
		at := a.RequestedAt
		switch {
		case a.Status == store.ApprovalApproved && (a.Kind == store.AccessKindRequest || a.Kind == store.AccessKindRefresh):
			w = convWay{Kind: convWayUnlocked, Words: "unlocked for the agent (lease)"}
		case a.Kind == store.AccessKindMove:
			w.Words = "asked to move in"
		case a.Kind == store.AccessKindGrant && a.RequestedAccess == store.GrantAccessWrite:
			w.Words = "asked to read and write"
		case a.Kind == store.AccessKindGrant:
			w.Words = "asked to read"
		default:
			w.Words = "asked to unlock"
		}
		if a.DecidedAt != nil {
			at = *a.DecidedAt
		}
		w.Decision = s.convDecision(a.ID, a.DecidedBy, at, decide)
		later = append(later, timed{at: at, project: a.Project, way: w})
	}
	slices.SortStableFunc(later, func(a, b timed) int { return a.at.Compare(b.at) })
	for _, t := range later {
		add(t.project, t.way)
	}
	return out
}

// convWritten reads what the conversation's sessions wrote -- the items their
// write events name, as the request's handle holds them now, and each
// session's findings -- by the project each item is in, newest first, each
// with its stamps. stamped reports whether any of them carries one. A claim,
// a release and a reclaim move who holds a task, not what it says, so they
// are not writes of it.
func (s *Service) convWritten(ctx context.Context, sessions []core.Session, trials bool) (map[string][]convItem, bool, error) {
	type ref struct {
		kind, id string
		at       time.Time
	}
	var refs []ref
	byKind := map[string][]string{}
	seen := map[string]bool{}
	if log := s.eventLog(ctx); log != nil {
		for _, x := range sessions {
			evs, err := log.LatestBySession(ctx, x.ID, convWriteKinds, convEventScan)
			if err != nil {
				return nil, false, err
			}
			for _, e := range evs {
				kind := convWrittenKind(e)
				if kind == "" || e.ItemID == "" || (kind == core.ItemKindTrial && !trials) || seen[kind+"/"+e.ItemID] {
					continue
				}
				seen[kind+"/"+e.ItemID] = true
				refs = append(refs, ref{kind: kind, id: e.ItemID, at: e.TS})
				byKind[kind] = append(byKind[kind], e.ItemID)
			}
		}
	}

	db := s.db(ctx)
	items := map[string]convItem{}
	project := map[string]string{}
	put := func(kind, id, proj string, it convItem) {
		it.Kind, it.ID = kind, id
		items[kind+"/"+id], project[kind+"/"+id] = it, proj
	}
	if ids := byKind[core.ItemKindMemory]; len(ids) > 0 {
		got, err := store.MemoriesByIDs(ctx, db, ids)
		if err != nil {
			return nil, false, err
		}
		for id, m := range got {
			put(core.ItemKindMemory, id, m.Project, convItem{Title: m.Name, Href: "/console/memories/" + id})
		}
	}
	if ids := byKind[core.ItemKindNote]; len(ids) > 0 {
		got, err := store.NotesByIDs(ctx, db, ids)
		if err != nil {
			return nil, false, err
		}
		for id, n := range got {
			put(core.ItemKindNote, id, n.Project, convItem{Title: n.Title, Href: "/console/notes/" + id})
		}
	}
	if ids := byKind[core.ItemKindTask]; len(ids) > 0 {
		got, err := store.TasksByIDs(ctx, db, ids)
		if err != nil {
			return nil, false, err
		}
		for id, t := range got {
			put(core.ItemKindTask, id, t.ProjectSlug, convItem{Title: t.Title, Href: "/console/tasks/" + id})
		}
	}
	if ids := byKind[core.ItemKindTrial]; len(ids) > 0 {
		got, err := store.TrialsByIDs(ctx, db, ids)
		if err != nil {
			return nil, false, err
		}
		for id, tr := range got {
			put(core.ItemKindTrial, id, tr.ProjectSlug, convItem{Title: tr.Title, Href: "/console/trials/" + id})
		}
	}
	stamps := map[string]map[string][]core.Stamp{}
	for kind, ids := range byKind {
		stamps[kind] = s.stampsByID(ctx, kind, ids)
	}
	var sessionIDs []string
	for _, x := range sessions {
		if x.Findings != "" {
			sessionIDs = append(sessionIDs, x.ID)
		}
	}
	findingStamps := s.stampsByID(ctx, core.ItemKindSession, sessionIDs)

	out := map[string][]convItem{}
	stamped := false
	keep := func(proj string, it convItem) {
		if lockedHere(ctx, proj) {
			return // the handle hides a locked project's items; nothing of one is listed
		}
		if len(it.MayContain) > 0 {
			stamped = true
		}
		out[proj] = append(out[proj], it)
	}
	for _, r := range refs {
		key := r.kind + "/" + r.id
		it, ok := items[key]
		if !ok {
			continue // deleted since, or in a project this request does not read
		}
		it.MayContain = core.StampsShown(stamps[r.kind][r.id], project[key])
		it.at = r.at
		keep(project[key], it)
	}
	for _, x := range sessions {
		if x.Findings == "" {
			continue
		}
		name := x.Name
		if name == "" {
			name = shortID(x.ID)
		}
		keep(x.ProjectSlug, convItem{Kind: convFindings, ID: x.ID, Title: "findings of " + name,
			Href: "/console/sessions/" + x.ID, MayContain: core.StampsShown(findingStamps[x.ID], x.ProjectSlug), at: x.UpdatedAt})
	}
	for proj := range out {
		slices.SortStableFunc(out[proj], func(a, b convItem) int {
			if c := b.at.Compare(a.at); c != 0 {
				return c
			}
			return cmp.Compare(a.ID, b.ID)
		})
	}
	return out, stamped, nil
}

// convWrittenKind is the kind of item a write event wrote, "" for an event
// that wrote none.
func convWrittenKind(e core.Event) string {
	switch e.Kind {
	case core.EventMemoryWritten:
		return core.ItemKindMemory
	case core.EventNoteWritten, core.EventPlanCaptured, core.EventPlanApproved, core.EventSubagentCaptured:
		return core.ItemKindNote
	case core.EventTrialRecorded:
		return core.ItemKindTrial
	case core.EventTaskTransition:
		if payloadStr(e.Payload, "claimed_by") != "" || payloadBool(e.Payload, "released") || payloadBool(e.Payload, "reclaimed") {
			return ""
		}
		return core.ItemKindTask
	}
	return ""
}

// convWrittenInOrder groups written by project in reach's order, then the
// rest by project, each capped at convItemsShown.
func convWrittenInOrder(written map[string][]convItem, reach []convReach) []convProject {
	var order []string
	for _, r := range reach {
		if _, ok := written[r.Project]; ok {
			order = append(order, r.Project)
		}
	}
	var rest []string
	for proj := range written {
		if !slices.Contains(order, proj) {
			rest = append(rest, proj)
		}
	}
	slices.Sort(rest)
	out := make([]convProject, 0, len(order)+len(rest))
	for _, proj := range append(order, rest...) {
		items := written[proj]
		p := convProject{Project: proj, Items: items}
		if len(items) > convItemsShown {
			p.Items, p.More = items[:convItemsShown], len(items)-convItemsShown
		}
		out = append(out, p)
	}
	return out
}

// convLockedOut lists the locked projects, among the conversation's homes and
// the destinations of its read-and-write grants, that this request does not
// read: names only, never whether it wrote anything there.
func convLockedOut(ctx context.Context, hops []convHop, rec store.ConversationRecord, back string) []convLocked {
	var out []convLocked
	add := func(project string) {
		if !lockedHere(ctx, project) || slices.ContainsFunc(out, func(l convLocked) bool { return l.Project == project }) {
			return
		}
		out = append(out, convLocked{Project: project, Unlock: convUnlock(project, back)})
	}
	for _, h := range hops {
		add(h.Project)
	}
	for _, g := range rec.Grants {
		if g.Access == store.GrantAccessWrite {
			add(g.Project)
		}
	}
	return out
}
