package console

import (
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/features"
	"github.com/arctop/seamless/internal/screening"
	"github.com/arctop/seamless/internal/store"
)

// The Held screen (the screening optional feature, and the queue of restricted
// accounts' proposals: held_proposals.go): the owner's queue of agent writes
// kept out of the corpus, and the sessions marked as having read outside text.
// Releasing lands a write exactly as the agent sent it and marks the memory
// owner-confirmed; discarding erases its text and lands nothing.
// Both are owner writes, through the same form-POST path as every other
// console action, and both read their target through the console's (possibly
// filtered) handle first, so a project hidden from this console cannot be
// resolved from here either.

// heldRecentLimit bounds the "recently decided" list.
const heldRecentLimit = 20

// heldPreviewMaxRunes bounds a held write's preview on the page.
const heldPreviewMaxRunes = 4000

type heldReason struct {
	Code  string `json:"code"`
	Words string `json:"words"`
	// Family is the code's color on the page (screening.Family): the chip
	// and the preview's marks of one reason share it.
	Family string `json:"-"`
}

// heldRow is one held write as the Held screen shows it.
type heldRow struct {
	ID         string       `json:"id"`
	CreatedAt  time.Time    `json:"createdAt"`
	ResolvedAt *time.Time   `json:"resolvedAt,omitempty"`
	Status     string       `json:"status"`
	Op         string       `json:"op"`
	Tool       string       `json:"tool"`
	ItemKind   string       `json:"itemKind"`
	ItemID     string       `json:"itemId,omitempty"`
	Project    string       `json:"project"`
	Name       string       `json:"name"`
	MemoryKind string       `json:"memoryKind,omitempty"`
	Session    string       `json:"session,omitempty"`
	Reasons    []heldReason `json:"reasons"`
	// What names the write in a few words ("constraint memory", "note").
	What string `json:"what"`
	// Preview is the held text, hidden characters made visible and secrets
	// redacted ("" for a delete or a gardener apply). Marks is the same text
	// with what each reason matched marked in its color (heldMarks); Marked
	// says any reason did. Accent is the most serious reason's family, the
	// card's color.
	Preview string        `json:"preview,omitempty"`
	Marks   template.HTML `json:"-"`
	Marked  bool          `json:"-"`
	Accent  string        `json:"-"`
	// Note says what releasing does when there is no text to preview.
	Note string `json:"note,omitempty"`
	// Link points at what the hold acts on (a memory, a proposal).
	Link string `json:"link,omitempty"`
	// Stale: the target moved on after the write was held; only Discard is
	// offered, since a release would undo the later change.
	Stale bool `json:"stale,omitempty"`
	// Proposal marks a restricted account's proposal (screening.IsProposal):
	// its release is a promotion. Account names the account that proposed it.
	// Both are absent on every other hold, so a daemon without restricted
	// accounts answers exactly as before.
	Proposal bool   `json:"proposal,omitempty"`
	Account  string `json:"account,omitempty"`
}

// taintRow is one session carrying the outside-text mark.
type taintRow struct {
	SessionID string    `json:"sessionId"`
	Name      string    `json:"name"`
	Project   string    `json:"project"`
	Status    string    `json:"status"`
	Reason    string    `json:"reason"`
	Since     time.Time `json:"since"`
}

type heldPageData struct {
	Pending []heldRow  `json:"pending"`
	Recent  []heldRow  `json:"recent"`
	Tainted []taintRow `json:"tainted"`
	// Part 2: lockdown, the review, and drift quarantine.
	Lockdown lockdownView `json:"lockdown"`
	Review   reviewView   `json:"review"`
	Flagged  []corpusRow  `json:"flagged"`
	Drifted  []corpusRow  `json:"drifted"`
	// ProposalsOnly: screening is off and the screen serves restricted
	// accounts' proposals alone (held_proposals.go); the screening-only
	// sections are not rendered. Restricted: restricted accounts are
	// configured, so the copy names proposals. Presentation only.
	ProposalsOnly bool `json:"-"`
	Restricted    bool `json:"-"`
	// Locked names the locked projects this request does not read, whose
	// holds the queue leaves out (seam proposals prints them): names only,
	// never a count. Empty, and absent from the JSON, when nothing is locked.
	Locked []string `json:"locked,omitempty"`
}

func (s *Service) heldPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	proposalsOnly := s.heldModeFor(s.effectiveFeatures(ctx)) == heldProposals
	pending, err := store.HeldWrites(ctx, s.db(ctx), store.HeldStatusHeld, 0)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	var recent []store.HeldWrite
	for _, status := range []string{store.HeldStatusReleased, store.HeldStatusDiscarded} {
		rows, rerr := store.HeldWrites(ctx, s.db(ctx), status, heldRecentLimit)
		if rerr != nil {
			s.serverError(w, r, rerr)
			return
		}
		recent = append(recent, rows...)
	}
	if proposalsOnly {
		// Screening's own holds wait for screening to come back on.
		pending, recent = onlyProposals(pending), onlyProposals(recent)
	}
	slices.SortFunc(recent, func(a, b store.HeldWrite) int { return resolvedAt(b).Compare(resolvedAt(a)) })
	if len(recent) > heldRecentLimit {
		recent = recent[:heldRecentLimit]
	}
	var tainted []store.TaintedSession
	if !proposalsOnly {
		if tainted, err = store.TaintedSessions(ctx, s.db(ctx)); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	var sessionIDs []string
	for _, h := range append(slices.Clone(pending), recent...) {
		if h.SessionID != "" && !slices.Contains(sessionIDs, h.SessionID) {
			sessionIDs = append(sessionIDs, h.SessionID)
		}
	}
	refs, err := store.SessionRefs(ctx, s.db(ctx), sessionIDs)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	data := heldPageData{Pending: []heldRow{}, Recent: []heldRow{}, Tainted: []taintRow{}, Flagged: []corpusRow{}, Drifted: []corpusRow{},
		ProposalsOnly: proposalsOnly, Restricted: s.cfg.RestrictedAccounts, Locked: lockedProjects(ctx)}
	if !proposalsOnly {
		if err := s.corpusSections(r, &data); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	for _, h := range pending {
		data.Pending = append(data.Pending, s.heldRowOf(r, h, refs[h.SessionID].Name, true))
	}
	for _, h := range recent {
		data.Recent = append(data.Recent, s.heldRowOf(r, h, refs[h.SessionID].Name, false))
	}
	for _, t := range tainted {
		data.Tainted = append(data.Tainted, taintRow{SessionID: t.SessionID, Name: t.Name, Project: t.Project,
			Status: string(t.Status), Reason: t.Reason, Since: t.TaintedAt})
	}
	s.render(w, r, "held", pageData{Title: "Held", Active: "held", Data: data})
}

func resolvedAt(h store.HeldWrite) time.Time {
	if h.ResolvedAt != nil {
		return *h.ResolvedAt
	}
	return h.CreatedAt
}

// heldRowOf renders one queue row. pending rows carry the preview and the
// staleness check; resolved ones keep only the record (their text is gone).
func (s *Service) heldRowOf(r *http.Request, h store.HeldWrite, session string, pending bool) heldRow {
	row := heldRow{
		ID: h.ID, CreatedAt: h.CreatedAt, ResolvedAt: h.ResolvedAt, Status: h.Status, Op: h.Op, Tool: h.Tool,
		ItemKind: h.ItemKind, ItemID: h.ItemID, Project: h.Project, Name: h.Name,
		MemoryKind: string(h.MemoryKind), Session: session, Reasons: []heldReason{},
	}
	row.Reasons, row.Accent = heldReasons(h.Reasons), screening.MostSerious(h.Reasons)
	if screening.IsProposal(h.Reasons) {
		row.Proposal, row.Account = true, h.DetailValue(screening.DetailAccount)
	}
	row.What = h.ItemKind
	if h.MemoryKind != "" {
		row.What = string(h.MemoryKind) + " " + h.ItemKind
	}
	switch h.Op {
	case store.HeldOpDelete:
		row.What += " delete"
		row.Note = "Releasing deletes this memory. Until then it stays exactly where it is, in every briefing."
		row.Link = "/console/memories/" + h.ItemID
	case store.HeldOpGardener:
		row.What += " gardener apply"
		row.Note = "Releasing applies the gardener proposal (" + h.DetailValue("proposal_kind") +
			"). Until then the proposal stays pending and nothing changes."
		row.Link = "/console/gardener/" + h.DetailValue("proposal")
	default:
		if h.ItemID != "" && h.BaseHash != "" {
			row.Link = itemLink(h.ItemKind, h.ItemID)
		}
		if target := h.DetailValue("supersedes_name"); target != "" {
			row.Note = "Releasing also supersedes " + refOf(h.DetailValue("supersedes_project"), target) +
				", which stays valid and in every briefing until then."
		} else if h.BaseHash != "" {
			row.Note = "Releasing replaces the current content of this " + h.ItemKind + "."
		}
	}
	if pending {
		if s.cfg.Screening != nil {
			row.Stale = s.cfg.Screening.Stale(r.Context(), h)
		}
		row.Preview = heldPreview(h)
		row.Marks, row.Marked = s.heldMarks(row.Preview, h.Reasons)
	}
	return row
}

// heldReasons are a hold's reason codes as the page lists them: each with
// its words and its color, the most serious first.
func heldReasons(codes []string) []heldReason {
	out := make([]heldReason, 0, len(codes))
	for _, code := range codes {
		out = append(out, heldReason{Code: code, Words: screening.Describe(screening.Code(code)),
			Family: screening.Family(screening.Code(code))})
	}
	slices.SortStableFunc(out, func(a, b heldReason) int {
		return screening.FamilyRank(a.Family) - screening.FamilyRank(b.Family)
	})
	return out
}

// heldMarks renders a preview with what each of its reasons matched marked
// (screening.PreviewSegments): a run's covering reasons become nested
// <mark>s, the widest outside, each carrying its family (the color) and its
// words (the tooltip). The held text is an agent's and may be hostile, so
// every run of it and every attribute value is escaped here; nothing else
// reaches the markup. marked reports whether any reason marked something.
func (s *Service) heldMarks(preview string, codes []string) (marks template.HTML, marked bool) {
	var rules screening.Rules
	if s.cfg.Screening != nil {
		rules = s.cfg.Screening.Rules()
	}
	var b strings.Builder
	for _, seg := range screening.PreviewSegments(preview, codes, rules) {
		for _, c := range seg.Codes {
			b.WriteString(`<mark class="held-mark" data-family="` + template.HTMLEscapeString(screening.Family(c)) +
				`" title="` + template.HTMLEscapeString(string(c)+": "+screening.Describe(c)) + `">`)
			marked = true
		}
		b.WriteString(template.HTMLEscapeString(seg.Text))
		b.WriteString(strings.Repeat("</mark>", len(seg.Codes)))
	}
	return template.HTML(b.String()), marked
}

// heldPreview is the held text as the owner reviews it: secrets redacted and
// every hidden character spelled out, so what an agent would read is exactly
// what the page shows.
func heldPreview(h store.HeldWrite) string {
	if h.Content == "" {
		return ""
	}
	text := screening.RevealHidden(screening.RedactSecrets(h.Content))
	if r := []rune(text); len(r) > heldPreviewMaxRunes {
		text = string(r[:heldPreviewMaxRunes]) + "\n[... preview cut short]"
	}
	return text
}

func itemLink(itemKind, id string) string {
	if itemKind == core.ItemKindNote {
		return "/console/notes/" + id
	}
	return "/console/memories/" + id
}

func refOf(project, name string) string {
	if project == "" {
		return "global/" + name
	}
	return project + "/" + name
}

// heldOne is a held write's own URL: the queue, scrolled to its card.
func (s *Service) heldOne(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/console/held#held-"+url.PathEscape(r.PathValue("id")), http.StatusSeeOther)
}

// heldTarget reads a held write through the console's handle: a hold the
// filtered view hides is not found from here.
func (s *Service) heldTarget(w http.ResponseWriter, r *http.Request) (store.HeldWrite, bool) {
	if s.cfg.Screening == nil {
		s.serverError(w, r, errors.New("console: write screening is not wired on this daemon"))
		return store.HeldWrite{}, false
	}
	id := r.PathValue("id")
	h, ok, err := store.HeldWriteByID(r.Context(), s.db(r.Context()), id)
	if err != nil {
		s.serverError(w, r, err)
		return store.HeldWrite{}, false
	}
	if !ok {
		s.notFound(w, r, "No held write with id "+id+".")
		return store.HeldWrite{}, false
	}
	// With screening off the queue serves proposals alone: a write screening
	// held before it was switched off waits for it to come back on.
	if !screening.IsProposal(h.Reasons) && s.heldModeFor(s.effectiveFeatures(r.Context())) == heldProposals {
		s.featureOff(w, r, features.Screening)
		return store.HeldWrite{}, false
	}
	return h, true
}

func (s *Service) heldRelease(w http.ResponseWriter, r *http.Request) {
	h, ok := s.heldTarget(w, r)
	if !ok {
		return
	}
	rel, err := s.cfg.Screening.Release(r.Context(), h.ID)
	switch {
	case errors.Is(err, store.ErrHoldNotPending):
		heldFlash(w, r, "That write was already released or discarded.")
		return
	case errors.Is(err, screening.ErrStale):
		heldFlash(w, r, "Not released: the "+h.ItemKind+" changed after this write was held, and releasing it would undo that change. Discard it instead.")
		return
	case err != nil:
		s.serverError(w, r, fmt.Errorf("console: release held write: %w", err))
		return
	}
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, map[string]any{"id": h.ID, "status": store.HeldStatusReleased, "item": rel.ItemID})
		return
	}
	msg := "Released: " + h.Name + " landed as written."
	if screening.IsProposal(h.Reasons) {
		msg = "Promoted: " + h.Name + " landed as " + proposerOf(h) + " wrote it."
	}
	if rel.Superseded != "" {
		msg += " " + rel.Superseded + " is now superseded."
	}
	heldNotice(w, r, msg)
}

func (s *Service) heldDiscard(w http.ResponseWriter, r *http.Request) {
	h, ok := s.heldTarget(w, r)
	if !ok {
		return
	}
	if _, err := s.cfg.Screening.Discard(r.Context(), h.ID); err != nil {
		if errors.Is(err, store.ErrHoldNotPending) {
			heldFlash(w, r, "That write was already released or discarded.")
			return
		}
		s.serverError(w, r, fmt.Errorf("console: discard held write: %w", err))
		return
	}
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, map[string]any{"id": h.ID, "status": store.HeldStatusDiscarded})
		return
	}
	heldNotice(w, r, "Discarded: nothing landed, and its text is gone.")
}

// proposerOf names who proposed a hold, for the owner: "account <name>", or
// "the restricted account" when the hold does not say.
func proposerOf(h store.HeldWrite) string {
	if a := h.DetailValue(screening.DetailAccount); a != "" {
		return "account " + a
	}
	return "the restricted account"
}

// heldUntaint clears a session's outside-text mark: the owner's call, and the
// only way the mark ever comes off.
func (s *Service) heldUntaint(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Screening == nil {
		s.serverError(w, r, errors.New("console: write screening is not wired on this daemon"))
		return
	}
	id := r.PathValue("id")
	sess, ok, err := store.SessionByID(r.Context(), s.db(r.Context()), id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if !ok {
		s.notFound(w, r, "No session with id "+id+".")
		return
	}
	cleared, err := s.cfg.Screening.ClearTaint(r.Context(), sess)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, map[string]any{"session": sess.ID, "cleared": cleared})
		return
	}
	if !cleared {
		heldNotice(w, r, sess.Name+" carried no outside-text mark.")
		return
	}
	heldNotice(w, r, "Cleared: "+sess.Name+" can write binding rules again without a hold.")
}

func heldNotice(w http.ResponseWriter, r *http.Request, msg string) {
	http.Redirect(w, r, withNotice(&url.URL{Path: "/console/held"}, msg), http.StatusSeeOther)
}

func heldFlash(w http.ResponseWriter, r *http.Request, msg string) {
	if wantsJSON(r) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": msg})
		return
	}
	q := url.Values{"error": {msg}}
	http.Redirect(w, r, "/console/held?"+q.Encode(), http.StatusSeeOther)
}
