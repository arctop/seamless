package console

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/screening"
	"github.com/arctop/seamless/internal/store"
)

// The Held screen (the screening optional feature): the owner's queue of agent
// writes kept out of the corpus, and the sessions marked as having read
// outside text. Releasing lands a write exactly as the agent sent it and marks
// the memory owner-confirmed; discarding erases its text and lands nothing.
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
	// redacted ("" for a delete or a gardener apply).
	Preview string `json:"preview,omitempty"`
	// Note says what releasing does when there is no text to preview.
	Note string `json:"note,omitempty"`
	// Link points at what the hold acts on (a memory, a proposal).
	Link string `json:"link,omitempty"`
	// Stale: the target moved on after the write was held; only Discard is
	// offered, since a release would undo the later change.
	Stale bool `json:"stale,omitempty"`
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
}

func (s *Service) heldPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pending, err := store.HeldWrites(ctx, s.cfg.DB, store.HeldStatusHeld, 0)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	var recent []store.HeldWrite
	for _, status := range []string{store.HeldStatusReleased, store.HeldStatusDiscarded} {
		rows, rerr := store.HeldWrites(ctx, s.cfg.DB, status, heldRecentLimit)
		if rerr != nil {
			s.serverError(w, r, rerr)
			return
		}
		recent = append(recent, rows...)
	}
	slices.SortFunc(recent, func(a, b store.HeldWrite) int { return resolvedAt(b).Compare(resolvedAt(a)) })
	if len(recent) > heldRecentLimit {
		recent = recent[:heldRecentLimit]
	}
	tainted, err := store.TaintedSessions(ctx, s.cfg.DB)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	var sessionIDs []string
	for _, h := range append(slices.Clone(pending), recent...) {
		if h.SessionID != "" && !slices.Contains(sessionIDs, h.SessionID) {
			sessionIDs = append(sessionIDs, h.SessionID)
		}
	}
	refs, err := store.SessionRefs(ctx, s.cfg.DB, sessionIDs)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	data := heldPageData{Pending: []heldRow{}, Recent: []heldRow{}, Tainted: []taintRow{}, Flagged: []corpusRow{}, Drifted: []corpusRow{}}
	if err := s.corpusSections(r, &data); err != nil {
		s.serverError(w, r, err)
		return
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
	for _, code := range h.Reasons {
		row.Reasons = append(row.Reasons, heldReason{Code: code, Words: screening.Describe(screening.Code(code))})
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
	}
	return row
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
	h, ok, err := store.HeldWriteByID(r.Context(), s.cfg.DB, id)
	if err != nil {
		s.serverError(w, r, err)
		return store.HeldWrite{}, false
	}
	if !ok {
		s.notFound(w, r, "No held write with id "+id+".")
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

// heldUntaint clears a session's outside-text mark: the owner's call, and the
// only way the mark ever comes off.
func (s *Service) heldUntaint(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Screening == nil {
		s.serverError(w, r, errors.New("console: write screening is not wired on this daemon"))
		return
	}
	id := r.PathValue("id")
	sess, ok, err := store.SessionByID(r.Context(), s.cfg.DB, id)
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
