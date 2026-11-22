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

// heldDiffMaxRows bounds the rows of a held update's diff on the page; the
// rest is cut, as the preview is.
const heldDiffMaxRows = 400

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
	SessionID  string       `json:"sessionId,omitempty"` // when this console reads the session: its link
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
	// Diff is what releasing a held update would change in the file it
	// replaces: the current file's lines against the write's (heldDiff),
	// unchanged runs folded, with the same treatment and marks as the
	// preview. Empty for a new item, a delete, a gardener apply, and a
	// target that cannot be read. DiffSame says the write matches the
	// current file line for line; DiffMarked that a reason marked a line;
	// DiffFolded that an unchanged run was folded.
	Diff       []heldDiffLine `json:"diff,omitempty"`
	DiffSame   bool           `json:"diffSame,omitempty"`
	DiffMarked bool           `json:"-"`
	DiffFolded bool           `json:"-"`
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

// heldDiffLine is one row of a held update's diff.
type heldDiffLine struct {
	// Op is "same", "del" (releasing removes the line), "add" (releasing adds
	// it), "fold" (Folded unchanged lines, not shown) or "cut" (the rest of
	// the diff, past heldDiffMaxRows).
	Op     string `json:"op"`
	Text   string `json:"text,omitempty"`
	Folded int    `json:"folded,omitempty"`
	// HTML is Text escaped, with what each reason matched marked on the lines
	// the write carries.
	HTML template.HTML `json:"-"`
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
		data.Pending = append(data.Pending, s.heldRowOf(r, h, refs, true))
	}
	for _, h := range recent {
		data.Recent = append(data.Recent, s.heldRowOf(r, h, refs, false))
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

// heldRowOf renders one queue row. pending rows carry the preview, a held
// update's diff and the staleness check; resolved ones keep only the record
// (their text is gone). refs names the sessions this console can read.
func (s *Service) heldRowOf(r *http.Request, h store.HeldWrite, refs map[string]store.SessionRef, pending bool) heldRow {
	row := heldRow{
		ID: h.ID, CreatedAt: h.CreatedAt, ResolvedAt: h.ResolvedAt, Status: h.Status, Op: h.Op, Tool: h.Tool,
		ItemKind: h.ItemKind, ItemID: h.ItemID, Project: h.Project, Name: h.Name,
		MemoryKind: string(h.MemoryKind), Reasons: []heldReason{},
	}
	if ref, ok := refs[h.SessionID]; ok && h.SessionID != "" {
		row.Session, row.SessionID = ref.Name, h.SessionID
		if row.Session == "" {
			row.Session = h.SessionID
		}
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
		s.heldDiff(&row, h)
	}
	return row
}

// heldDiff fills a pending held update's diff: the file it would replace,
// read from disk as it stands now, against the held text, both with the
// preview's treatment (secrets redacted, hidden characters spelled out) so
// neither side shows what the preview would not. Only a write that replaces
// an existing file has one (a base hash); a new item shows its text alone. A
// target that cannot be read (gone, or no files layer) leaves no diff: the
// hold is stale then, and the preview still shows the write.
func (s *Service) heldDiff(row *heldRow, h store.HeldWrite) {
	if h.Op != store.HeldOpWrite || h.BaseHash == "" || h.Content == "" || s.cfg.Files == nil {
		return
	}
	base := h.DetailValue("path")
	if from := h.DetailValue("from_path"); from != "" {
		base = from // a note the write moves: it replaces the file where it is now
	}
	current, err := s.cfg.Files.Store().ReadFile(base)
	if err != nil {
		return
	}
	before, after := splitLines(heldText(current)), heldText(h.Content)
	afterLines := splitLines(after)
	marked := s.heldLineMarks(after, h.Reasons)
	if len(marked) != len(afterLines) { // never expected: the marks then go, never the escaping
		marked = make([]template.HTML, len(afterLines))
		for i, l := range afterLines {
			marked[i] = template.HTML(template.HTMLEscapeString(l))
		}
	}
	rows := lineDiff(before, afterLines)
	if !diffChanged(rows) {
		row.DiffSame = true
		return
	}
	for _, d := range foldUnchanged(rows, diffContext) {
		if len(row.Diff) == heldDiffMaxRows {
			row.Diff = append(row.Diff, heldDiffLine{Op: "cut"})
			break
		}
		line := heldDiffLine{}
		switch d.Op {
		case diffFold:
			line.Op, line.Folded = "fold", d.Folded
			row.DiffFolded = true
		case diffDel:
			line.Op, line.Text = "del", before[d.Old]
			line.HTML = template.HTML(template.HTMLEscapeString(line.Text))
		default:
			line.Op, line.Text, line.HTML = "same", afterLines[d.New], marked[d.New]
			if d.Op == diffAdd {
				line.Op = "add"
			}
			if strings.Contains(string(line.HTML), "<mark ") {
				row.DiffMarked = true
			}
		}
		row.Diff = append(row.Diff, line)
	}
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
	var b strings.Builder
	for _, seg := range screening.PreviewSegments(preview, codes, s.heldRules()) {
		for _, c := range seg.Codes {
			b.WriteString(heldMarkOpen(c))
			marked = true
		}
		b.WriteString(template.HTMLEscapeString(seg.Text))
		b.WriteString(strings.Repeat("</mark>", len(seg.Codes)))
	}
	return template.HTML(b.String()), marked
}

// heldLineMarks is heldMarks cut into the text's lines (splitLines): a match
// that spans lines is marked on each of them, and every mark opens and closes
// within its line, so a diff can show any line on its own. The matching runs
// over the whole text, as the preview's does, so a match is found whether or
// not it crosses a line.
func (s *Service) heldLineMarks(text string, codes []string) []template.HTML {
	var lines []template.HTML
	var b strings.Builder
	for _, seg := range screening.PreviewSegments(text, codes, s.heldRules()) {
		open := ""
		for _, c := range seg.Codes {
			open += heldMarkOpen(c)
		}
		closeMarks := strings.Repeat("</mark>", len(seg.Codes))
		for i, piece := range strings.Split(seg.Text, "\n") {
			if i > 0 {
				lines = append(lines, template.HTML(b.String()))
				b.Reset()
			}
			if piece != "" {
				b.WriteString(open + template.HTMLEscapeString(piece) + closeMarks)
			}
		}
	}
	lines = append(lines, template.HTML(b.String()))
	if strings.HasSuffix(text, "\n") {
		lines = lines[:len(lines)-1] // splitLines: a final newline opens no line
	}
	return lines
}

// heldRules are the screens' rules, for marking what each reason matched.
func (s *Service) heldRules() screening.Rules {
	if s.cfg.Screening != nil {
		return s.cfg.Screening.Rules()
	}
	return screening.Rules{}
}

// heldMarkOpen is the opening tag of one reason's mark: its family (the
// color) and its words (the tooltip), escaped.
func heldMarkOpen(c screening.Code) string {
	return `<mark class="held-mark" data-family="` + template.HTMLEscapeString(screening.Family(c)) +
		`" title="` + template.HTMLEscapeString(string(c)+": "+screening.Describe(c)) + `">`
}

// heldPreview is the held text as the owner reviews it: secrets redacted and
// every hidden character spelled out, so what an agent would read is exactly
// what the page shows.
func heldPreview(h store.HeldWrite) string {
	if h.Content == "" {
		return ""
	}
	text := heldText(h.Content)
	if r := []rune(text); len(r) > heldPreviewMaxRunes {
		text = string(r[:heldPreviewMaxRunes]) + "\n[... preview cut short]"
	}
	return text
}

// heldText is a text with the preview's treatment, uncut: secrets redacted
// and every hidden character spelled out.
func heldText(content string) string {
	return screening.RevealHidden(screening.RedactSecrets(content))
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
