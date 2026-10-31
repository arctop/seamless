package console

// Project isolation, the console half: the lock pills, the Overview control,
// and the two-step tighten flow.
//
// The asymmetry is the whole design. Loosening is immediate and ceremony-free
// -- nothing leaked while the fence was up, so the change is fully reversible.
// Tightening changes what every OTHER agent can see and detaches this project
// from its family and parent, so it goes through a server-rendered confirm panel
// that states exactly what will change (the gardener's
// confirm-only-when-consequential precedent). No JS dialogs: the panel is a
// GET-addressable page state, so it survives the fetch+morph path, a reload, and
// a shared link alike.

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/validate"
)

// isolationPromises is the one-line promise each state makes. It is the exact
// copy the Overview control renders under its option, the pill's tooltip, the
// confirm panel's headline, and the `promise` field the CLI prints -- one
// sentence per state, quoted from one place so the surfaces cannot drift.
var isolationPromises = map[core.Isolation]string{
	core.IsolationOpen:         "Open — shares normally. Global knowledge flows in; family surfaces may include it.",
	core.IsolationConfidential: "Confidential — nothing leaves. Agents in other projects never see this project's knowledge, and agents here cannot write outside it.",
	core.IsolationSealed:       "Sealed — nothing in or out. Agents here see only this project: no global memories, no family, no cross-project reads.",
}

// isolationLabels are the option labels on the segmented control.
var isolationLabels = map[core.Isolation]string{
	core.IsolationOpen:         "Open",
	core.IsolationConfidential: "Confidential",
	core.IsolationSealed:       "Sealed",
}

// isolationTones map a state to its pill tone class (see console.css .kind.*).
// Open has no pill at all: the absence of a lock is the honest rendering of the
// default, and badging every project would make the fenced ones harder to spot.
var isolationTones = map[core.Isolation]string{
	core.IsolationConfidential: "accent",
	core.IsolationSealed:       "warn",
}

// isolationOf normalizes a project's stored state. core.Project documents "" as
// open, and every console surface reads through here so a project written by an
// older binary renders as open rather than as a blank pill. An unrecognized
// value degrades the same way, and that is honest rather than a fallback:
// core.Isolation.FencesOutbound is false for it too, so the fence really is
// down -- the console would be lying if it drew a lock.
func isolationOf(p core.Project) core.Isolation {
	if !p.Isolation.Valid() {
		return core.IsolationOpen
	}
	return p.Isolation
}

// isolationPromise returns the promise sentence for a state ("" reads as open).
func isolationPromise(state core.Isolation) string {
	if promise, ok := isolationPromises[state]; ok {
		return promise
	}
	return isolationPromises[core.IsolationOpen]
}

// isolationPill renders the lock pill for a fenced project, and nothing at all
// for an open one. Both surfaces that badge isolation -- the project detail
// title row and the board row -- call this one helper, so the pill, the glyph,
// and the tooltip cannot drift apart between them.
func isolationPill(state core.Isolation) template.HTML {
	tone, fenced := isolationTones[state]
	if !fenced {
		return ""
	}
	return template.HTML(`<span class="kind ` + tone + ` iso-pill" title="` +
		template.HTMLEscapeString(isolationPromise(state)) + `">` +
		string(icon("lock")) + template.HTMLEscapeString(string(state)) + `</span>`)
}

// isolationOptionVM is one option of the Overview segmented control: the button
// label plus the promise line rendered under it.
type isolationOptionVM struct {
	State   core.Isolation
	Label   string
	Promise string
	On      bool
}

// isolationOptions builds the three options loosest-first (core.Isolations'
// order), marking the one the project currently holds.
func isolationOptions(current core.Isolation) []isolationOptionVM {
	out := make([]isolationOptionVM, 0, len(core.Isolations))
	for _, state := range core.Isolations {
		out = append(out, isolationOptionVM{
			State:   state,
			Label:   isolationLabels[state],
			Promise: isolationPromises[state],
			On:      state == current,
		})
	}
	return out
}

// isolationProvenanceVM carries the confirm panel's provenance line: how many
// global memories originated in this project's sessions, and therefore how many
// relocation proposals a tighten will file.
//
// Known is what keeps the panel honest: it separates a PROVEN zero ("nothing
// leaked", worth stating) from an unanswered question, which prints nothing at
// all rather than a zero the owner would read as a finding.
type isolationProvenanceVM struct {
	Known bool
	Count int
}

// isolationGlobalProvenance reports how many global memories originated in this
// project's sessions -- the ones a tighten files relocation proposals for.
//
// It reports "not known" rather than zero whenever the number could not be
// PROVED: no database to ask, or a query that failed. A real zero and an
// unanswered question look identical as an int, and only one of them is a
// finding -- collapsing them is exactly the plausible dummy the no-fake-results
// rule exists to prevent.
func (s *Service) isolationGlobalProvenance(ctx context.Context, slug string) isolationProvenanceVM {
	if s.cfg.DB == nil {
		return isolationProvenanceVM{}
	}
	leaked, err := store.GlobalMemoriesFromProjectSessions(ctx, s.cfg.DB, slug)
	if err != nil {
		// Degrade rather than fail the page: the confirm panel's job is the
		// topology change, and the audit re-runs on the apply either way.
		s.logger.Warn("console: isolation provenance audit", "project", slug, "error", err)
		return isolationProvenanceVM{}
	}
	return isolationProvenanceVM{Known: true, Count: len(leaked)}
}

// fileIsolationRelocations runs the provenance audit as gardener proposals,
// returning how many were filed.
//
// It runs AFTER TightenProjectIsolation has committed, deliberately outside that
// transaction: the fence is the thing that must land atomically, and generating
// proposals is propose-only follow-up work that may safely fail. A missing
// gardener or a failed audit therefore costs the proposals, never the tighten --
// and the count is reported so the flash can say what actually happened rather
// than repeat the confirm panel's promise.
func (s *Service) fileIsolationRelocations(ctx context.Context, slug string) int {
	if s.cfg.Gardener == nil {
		return 0
	}
	n, err := s.cfg.Gardener.ProposeIsolationRelocations(ctx, slug)
	if err != nil {
		s.logger.Warn("console: file isolation relocations", "project", slug, "error", err)
		return 0
	}
	return n
}

// isolationConfirmVM is the server-rendered confirm panel. It states exactly
// what the tighten will change -- or, when child projects block it, what stands
// in the way and where to fix it.
type isolationConfirmVM struct {
	Slug     string
	From     core.Isolation
	To       core.Isolation
	Promise  string
	Families []string
	Parent   string
	Children []string
	Blocked  bool
	Detaches bool

	Provenance isolationProvenanceVM
	// Cancel is the same page with the pending choice dropped.
	Cancel string
}

// newIsolationConfirmVM projects the store's tighten report into the panel.
func newIsolationConfirmVM(eff store.IsolationTightenEffects, prov isolationProvenanceVM) *isolationConfirmVM {
	return &isolationConfirmVM{
		Slug: eff.Project, From: eff.From, To: eff.To, Promise: isolationPromise(eff.To),
		Families: eff.Families, Parent: eff.Parent, Children: eff.Children,
		Blocked: eff.Blocked(), Detaches: eff.DetachesTopology(),
		Provenance: prov,
		Cancel:     "/console/projects/" + url.PathEscape(eff.Project) + "?tab=overview#isolation",
	}
}

// isolationResponse is the JSON body of POST /console/projects/{slug}/isolation,
// and the contract the `seam project isolation` command renders from. Every key
// is always present (Effects excepted: a loosen detaches nothing), so the CLI
// can print the outcome without inspecting the status code first:
//
//	Applied         the state was written
//	ConfirmRequired a tighten whose consequences the owner has not confirmed yet
//	Blocked         child projects refuse the tighten; nothing was written
type isolationResponse struct {
	Project          string         `json:"project"`
	Requested        core.Isolation `json:"requested"`
	State            core.Isolation `json:"state"`   // what the project holds NOW
	Promise          string         `json:"promise"` // the promise for State
	RequestedPromise string         `json:"requestedPromise"`
	Applied          bool           `json:"applied"`
	ConfirmRequired  bool           `json:"confirmRequired"`
	Blocked          bool           `json:"blocked"`
	// GlobalMemories is the provenance count behind the relocation line: how many
	// global memories originated in this project's sessions. It is -1 when the
	// audit could not be run at all -- "not computed" must not read as "none".
	GlobalMemories int                            `json:"globalMemories"`
	Effects        *store.IsolationTightenEffects `json:"effects,omitempty"`
	// Filed is how many relocation proposals the tighten actually filed, as a
	// number rather than only a clause inside Message -- a client that must report
	// what it caused should not have to parse prose back out of a sentence. It is
	// 0 on every branch that wrote nothing, so it reads as "none filed" only where
	// filing was actually attempted.
	Filed int `json:"filed"`
	// Message is a one-line summary safe to print verbatim.
	Message string `json:"message"`
	// Error is set only on a refusal (409), so consolePOST surfaces the reason.
	Error string `json:"error,omitempty"`
}

// projectIsolationSet is the owner mutation behind the Overview isolation
// control: POST /console/projects/{slug}/isolation with state=<enum> and, for a
// consequential tighten, confirm=1.
//
// Modeled on favoriteToggle -- strict allowlist, store call, event, JSON for
// bearer callers and 303 + morph for the browser -- with one extra hop: a
// tighten that has not been confirmed redirects to the page's confirm state
// instead of writing, so what the owner approves is rendered by the same code
// that would render it afterwards.
func (s *Service) projectIsolationSet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	slug := r.PathValue("slug")
	p, found, err := store.ProjectBySlug(ctx, s.cfg.DB, slug)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if !found {
		s.notFound(w, r, "No project with slug "+slug+".")
		return
	}

	raw, once, _ := formOnce(r, "state")
	if !once {
		s.badRequest(w, r, "state must be provided exactly once")
		return
	}
	state, verr := validate.Isolation(raw)
	if verr != nil {
		s.badRequest(w, r, verr.Error())
		return
	}
	confirmed := false
	if value, once, present := formOnce(r, "confirm"); present {
		if !once || value != "1" {
			s.badRequest(w, r, `confirm must be exactly one value of "1" when present`)
			return
		}
		confirmed = true
	}

	eff, perr := store.PreviewIsolationTighten(ctx, s.cfg.DB, slug, state)
	switch {
	case errors.Is(perr, store.ErrNotATighten):
		// Loosening (open, or sealed -> confidential): immediate, no confirm.
		// Nothing leaked while the fence was up, so the change is reversible and
		// ceremony would only train the owner to click through panels.
		s.isolationLoosen(w, r, slug, isolationOf(p), state)
		return
	case errors.Is(perr, store.ErrProjectNotFound):
		s.notFound(w, r, "No project with slug "+slug+".")
		return
	case perr != nil:
		s.serverError(w, r, perr)
		return
	}

	if eff.Blocked() {
		s.isolationRefused(w, r, eff, isolationBlockedMessage(eff))
		return
	}
	// Confirm only when the tighten is consequential -- it moves the fence, or it
	// detaches topology. Re-asserting the state a standalone project already
	// holds changes nothing, so a double submit applies again instead of
	// demanding a second confirmation for nothing.
	if (eff.From != eff.To || eff.DetachesTopology()) && !confirmed {
		s.isolationConfirm(w, r, eff)
		return
	}

	applied, aerr := store.TightenProjectIsolation(ctx, s.writeDB(), slug, state, time.Now().UTC())
	switch {
	case errors.Is(aerr, store.ErrIsolationHasChildren):
		// The world moved between the preview and the confirm; the apply wrote
		// nothing, so report the new blocker rather than a partial success.
		s.isolationRefused(w, r, applied, isolationBlockedMessage(applied))
		return
	case errors.Is(aerr, store.ErrNotATighten):
		s.isolationRefused(w, r, store.IsolationTightenEffects{Project: slug, From: eff.From, To: state},
			slug+" is no longer "+string(eff.From)+": its isolation changed since this page was rendered.")
		return
	case aerr != nil:
		s.serverError(w, r, aerr)
		return
	}

	msg := "Isolation set to " + string(applied.To) + "."
	if detach := isolationDetachSummary(applied); detach != "" {
		msg += " " + detach
	}
	// The fence is up; now ask about the knowledge that got out before it was.
	// Propose-only and post-commit -- see fileIsolationRelocations.
	filed := s.fileIsolationRelocations(ctx, applied.Project)
	if filed > 0 {
		msg += " Filed " + plural(filed, "relocation proposal", "relocation proposals") +
			" in the Gardener inbox."
	}
	s.recordIsolationEvent(ctx, applied)
	s.isolationApplied(w, r, applied, msg, filed)
}

// isolationLoosen applies a loosening (or an already-held open state) directly.
func (s *Service) isolationLoosen(w http.ResponseWriter, r *http.Request, slug string, from, to core.Isolation) {
	ctx := r.Context()
	if err := store.SetProjectIsolation(ctx, s.writeDB(), slug, to); err != nil {
		if errors.Is(err, store.ErrProjectNotFound) {
			s.notFound(w, r, "No project with slug "+slug+".")
			return
		}
		s.serverError(w, r, err)
		return
	}
	eff := store.IsolationTightenEffects{Project: slug, From: from, To: to}
	if from != to {
		s.recordIsolationEvent(ctx, eff)
	}
	// A loosen files nothing: the fence coming down leaks nothing that was not
	// already shared, so there is no provenance to repair.
	s.isolationApplied(w, r, eff, "Isolation set to "+string(to)+".", 0)
}

// recordIsolationEvent appends the state change to the log so it shows up in the
// project's Recent activity -- the change needs no scary chrome on the page, but
// it must be traceable afterwards.
func (s *Service) recordIsolationEvent(ctx context.Context, eff store.IsolationTightenEffects) {
	if s.cfg.Events == nil {
		return
	}
	payload := map[string]any{
		"slug": eff.Project, "from": string(eff.From), "to": string(eff.To), "by": "console",
	}
	if len(eff.Families) > 0 {
		payload["families"] = eff.Families
	}
	if eff.Parent != "" {
		payload["parent"] = eff.Parent
	}
	if _, err := s.cfg.Events.Record(ctx, core.Event{
		Kind: core.EventProjectIsolationChanged, ProjectSlug: eff.Project,
		ItemID: eff.Project, Payload: payload,
	}); err != nil {
		s.logger.Warn("console: record isolation event", "project", eff.Project, "error", err)
	}
}

// isolationApplied answers a write that landed: the new state as JSON for a
// bearer caller, a 303 back to the Overview block for the browser.
func (s *Service) isolationApplied(w http.ResponseWriter, r *http.Request, eff store.IsolationTightenEffects, msg string, filed int) {
	if wantsJSON(r) {
		out := s.newIsolationResponse(r, eff.Project, eff.To, eff.To, msg)
		out.Applied = true
		out.Filed = filed
		if eff.DetachesTopology() {
			out.Effects = &eff
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	s.isolationRedirect(w, r, eff.Project, "notice", msg)
}

// isolationConfirm answers an unconfirmed tighten. The browser is redirected to
// the page's confirm state (?isolate=<state>), which renders the panel from the
// same preview this handler just ran; a bearer caller gets the effects to print
// and re-submits with confirm=1.
func (s *Service) isolationConfirm(w http.ResponseWriter, r *http.Request, eff store.IsolationTightenEffects) {
	if wantsJSON(r) {
		msg := "Confirm required: " + eff.Project + " " + string(eff.From) + " -> " + string(eff.To) + "."
		if detach := isolationDetachSummary(eff); detach != "" {
			msg += " " + detach
		}
		out := s.newIsolationResponse(r, eff.Project, eff.To, eff.From, msg)
		out.ConfirmRequired = true
		out.Effects = &eff
		writeJSON(w, http.StatusOK, out)
		return
	}
	http.Redirect(w, r, "/console/projects/"+url.PathEscape(eff.Project)+
		"?tab=overview&isolate="+url.QueryEscape(string(eff.To))+"#isolation", http.StatusSeeOther)
}

// isolationRefused answers a tighten that cannot happen. Nothing was written, so
// the status says so (409) and the reason names the remedy; the browser lands on
// the page whose panel lists the children to re-parent.
func (s *Service) isolationRefused(w http.ResponseWriter, r *http.Request, eff store.IsolationTightenEffects, msg string) {
	if wantsJSON(r) {
		out := s.newIsolationResponse(r, eff.Project, eff.To, eff.From, msg)
		out.Blocked = eff.Blocked()
		out.Error = msg
		if eff.Blocked() {
			out.Effects = &eff
		}
		writeJSON(w, http.StatusConflict, out)
		return
	}
	if eff.Blocked() {
		http.Redirect(w, r, "/console/projects/"+url.PathEscape(eff.Project)+
			"?tab=overview&isolate="+url.QueryEscape(string(eff.To))+"#isolation", http.StatusSeeOther)
		return
	}
	s.isolationRedirect(w, r, eff.Project, "error", msg)
}

// isolationRedirect sends the browser back to the project's isolation block with
// a flash, inside .main so navigation.js fetches and morphs it in place.
func (s *Service) isolationRedirect(w http.ResponseWriter, r *http.Request, slug, key, msg string) {
	http.Redirect(w, r, "/console/projects/"+url.PathEscape(slug)+"?tab=overview&"+
		key+"="+url.QueryEscape(msg)+"#isolation", http.StatusSeeOther)
}

// newIsolationResponse seeds the JSON body's always-present fields. The
// provenance count comes from the same helper the confirm panel reads, so step
// 8 lights up the CLI and the console together.
func (s *Service) newIsolationResponse(r *http.Request, slug string, requested, state core.Isolation, msg string) isolationResponse {
	return isolationResponse{
		Project: slug, Requested: requested, State: state,
		Promise: isolationPromise(state), RequestedPromise: isolationPromise(requested),
		GlobalMemories: isolationProvenanceCount(s.isolationGlobalProvenance(r.Context(), slug)),
		Message:        msg,
	}
}

// isolationProvenanceCount renders the provenance count for the wire: the count
// when it is known, -1 while step 8's audit is unbuilt. A plain 0 would tell the
// CLI "no memories need relocating", which nothing has established.
func isolationProvenanceCount(prov isolationProvenanceVM) int {
	if !prov.Known {
		return -1
	}
	return prov.Count
}

// isolationBlockedMessage states the refusal and its remedy: the rule, then the
// exact slugs standing in the way. Never the project's contents -- a fence error
// says what to do, not what is behind it.
func isolationBlockedMessage(eff store.IsolationTightenEffects) string {
	return "Isolation requires a standalone project. Re-parent " +
		strings.Join(eff.Children, ", ") + " before making " + eff.Project + " " + string(eff.To) + "."
}

// isolationDetachSummary renders the topology a tighten gives up, for the flash
// line and the CLI message. Empty when the project was already standalone.
func isolationDetachSummary(eff store.IsolationTightenEffects) string {
	var parts []string
	if len(eff.Families) > 0 {
		parts = append(parts, "Left family "+strings.Join(eff.Families, ", ")+".")
	}
	if eff.Parent != "" {
		parts = append(parts, "Detached from parent "+eff.Parent+".")
	}
	return strings.Join(parts, " ")
}

// formOnce reads a parameter that must appear exactly once. once reports whether
// it did; present reports whether it appeared at all, so an optional parameter
// can tell "absent" (legal) from "supplied twice" (a 400).
//
// It reads r.Form, not r.PostForm: unlike the browser-only favorite toggle this
// route is also the CLI's, and `seam`'s consolePOST sends no body, so the query
// string carries the arguments there. Merging the two sources stays strict
// because r.Form concatenates them -- a value supplied in both places is two
// values and fails the count rather than one silently winning.
func formOnce(r *http.Request, key string) (value string, once, present bool) {
	values, present := r.Form[key]
	if len(values) != 1 {
		return "", false, present
	}
	return values[0], true, true
}

// isolationPendingConfirm builds the Overview confirm panel for a ?isolate=
// page state, or nil when there is nothing to confirm.
//
// A state that is no longer a tighten (the owner hand-typed ?isolate=open, or
// another agent moved the fence since the redirect) yields no panel: the
// segmented control below it already shows the truth, and inventing a panel for
// a change that will not happen is the fake result the strict-param policy
// exists to prevent. An unknown slug or an unrecognized state never reaches
// here -- the caller validates both.
func (s *Service) isolationPendingConfirm(ctx context.Context, slug string, state core.Isolation) (*isolationConfirmVM, error) {
	eff, err := store.PreviewIsolationTighten(ctx, s.cfg.DB, slug, state)
	switch {
	case errors.Is(err, store.ErrNotATighten), errors.Is(err, store.ErrProjectNotFound):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("console.isolationPendingConfirm: %w", err)
	}
	return newIsolationConfirmVM(eff, s.isolationGlobalProvenance(ctx, slug)), nil
}
