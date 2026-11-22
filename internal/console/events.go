package console

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/features"
	"github.com/arctop/seamless/internal/store"
)

// kvPair is one scalar payload field, rendered as a key/value row.
type kvPair struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// eventItem is a memory an event surfaced or touched, resolved from an item id to
// its current index entry. Missing is true when the id no longer resolves (the
// memory was deleted or the id predates the index).
type eventItem struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Description string `json:"description,omitempty"`
	Project     string `json:"project,omitempty"`
	Missing     bool   `json:"missing,omitempty"`
}

// eventDetailData is the payload for a single event's detail page.
type eventDetailData struct {
	Event eventRow `json:"event"`
	// Headline is what the page is titled with: the event's summary, naming
	// what a gardener action was about when its items resolved.
	Headline string         `json:"headline"`
	Trace    interactionRow `json:"trace"`             // rich request/response + session attribution
	Prompt   string         `json:"prompt,omitempty"`  // user prompt that triggered the event, if any
	Content  string         `json:"content,omitempty"` // verbatim injected/large text, if any
	Items    []eventItem    `json:"items,omitempty"`   // resolved memories the event referenced
	// Subjects are what the event is about (its ItemID, by kind), and for a
	// gardener proposal the items the proposal names: see eventSubject.
	Subjects []eventSubject `json:"subjects,omitempty"`
	// ItemLink is the subject the event's ItemID names, when it resolved to a
	// page: the inspector's Item row links it.
	ItemLink *eventSubject `json:"-"`
	Fields   []kvPair      `json:"fields,omitempty"` // remaining scalar payload fields
	RawJSON  string        `json:"rawJson"`          // pretty-printed full payload

	Eyebrow       string `json:"-"`
	RequestLabel  string `json:"-"`
	ResponseLabel string `json:"-"`
	ReturnHref    string `json:"-"`
	ReturnLabel   string `json:"-"`
	// Level is the console level, for detail-body's gates (the decoded fields
	// and the raw payload are Advanced): the body also renders as the ?peek=1
	// fragment, with no page around it to ask.
	Level level `json:"-"`
}

// eventDetail renders one event-log entry as a review workspace: promoted
// request/response evidence, surfaced memories, provenance, decoded payload
// fields, and the raw JSON. This is what activity and interaction rows link to.
func (s *Service) eventDetail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if s.cfg.Events == nil {
		s.notFound(w, r, "Event history is not available on this server.")
		return
	}
	id := r.PathValue("id")
	ev, ok, err := s.eventLog(ctx).ByID(ctx, id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if !ok {
		s.notFound(w, r, "No event with id "+id+".")
		return
	}

	requestLabel, responseLabel := eventBodyLabels(ev.Kind)
	returnHref, returnLabel := eventReturnLink(ev)
	data := eventDetailData{
		Event:   toEventRow(ev),
		Trace:   eventTrace(ev, s.sessionNamer(ctx)),
		Prompt:  payloadStr(ev.Payload, "prompt"),
		Content: payloadStr(ev.Payload, "content"),
		RawJSON: prettyPayload(ev.Payload),

		Eyebrow:       eventEyebrow(ev.Kind),
		RequestLabel:  requestLabel,
		ResponseLabel: responseLabel,
		ReturnHref:    returnHref,
		ReturnLabel:   returnLabel,
		Level:         s.consoleLevel(ctx).Level,
	}
	if items, err := s.resolveEventItems(ctx, ev); err != nil {
		s.serverError(w, r, err)
		return
	} else {
		data.Items = items
	}
	subjects, err := s.resolveEventSubjects(ctx, ev)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	data.Subjects = subjects
	for i := range subjects {
		if sub := &subjects[i]; ev.ItemID != "" && sub.ID == ev.ItemID && sub.Href != "" {
			data.ItemLink = sub
			break
		}
	}
	data.Headline = eventHeadline(ev, data.Event.Summary, subjects)
	data.Fields = scalarFields(ev.Payload, eventBodyPayloadKeys(ev.Kind)...)

	// No Active nav key: the event detail is a leaf reached from Overview/
	// Interactions via the crumb, so highlighting a nav item would mislead.
	s.renderDetail(w, r, "event", pageData{Title: data.Headline, Data: data})
}

// The kinds of thing an event can be about (eventSubject.Kind).
const (
	subjectMemory   = "memory"
	subjectNote     = "note"
	subjectTask     = "task"
	subjectTrial    = "trial"
	subjectProposal = "proposal"
	subjectSession  = "session"
	subjectPlan     = "plan"
)

// eventSubject is one thing an event is about, resolved from an id by the
// event's kind through the request's handle (s.db), so an item in a locked
// project is neither resolved nor named: the card says what kind of thing it
// was and shows its id, nothing more. Name and Href are set only when it
// resolved, and Href only to a page that opens.
type eventSubject struct {
	Kind    string `json:"kind"`
	ID      string `json:"id"`
	Name    string `json:"name,omitempty"`
	Detail  string `json:"detail,omitempty"` // one line under the name
	Project string `json:"project,omitempty"`
	Href    string `json:"href,omitempty"`
	// Peek says Href has a peek fragment, so the Interactions inspector can
	// open it in place (data-peek); a proposal's page has none.
	Peek bool `json:"-"`
	// Word is its state in the owner's words, where it has one: a task's
	// (wordFor) or a proposal's (proposalStateWord).
	Word *taskWord `json:"word,omitempty"`
	// Role is what a proposal does with an item it names ("the memory to
	// keep"), "" for the event's own subject.
	Role    string `json:"role,omitempty"`
	Missing bool   `json:"missing,omitempty"`
}

// KindLabel names the subject's kind for the card.
func (e eventSubject) KindLabel() string {
	if e.Kind == subjectProposal {
		return "gardener proposal"
	}
	return e.Kind
}

// Icon is the subject's glyph.
func (e eventSubject) Icon() string {
	switch e.Kind {
	case subjectMemory:
		return "brain"
	case subjectNote:
		return "file-text"
	case subjectTask:
		return "list-checks"
	case subjectTrial:
		return "flask-conical"
	case subjectProposal:
		return "sprout"
	case subjectSession:
		return "terminal"
	case subjectPlan:
		return "map"
	}
	return "activity"
}

// eventSubjectRef names what an event's own subject is: its kind and its id,
// by the event's kind -- the id each kind's recorder writes (ItemID, or the
// payload where the item is named there). ok is false for an event about no
// item the console has a page for (a record key, a lab, a hook run).
func eventSubjectRef(ev core.Event) (kind, id string, ok bool) {
	switch ev.Kind {
	case core.EventMemoryWritten, core.EventMemoryRead, core.EventMemorySuperseded,
		core.EventMemoryArchived, core.EventMemoryFirstReuse:
		kind = subjectMemory
	case core.EventNoteWritten, core.EventNoteRead, core.EventPlanCaptured,
		core.EventPlanPresented, core.EventPlanApproved, core.EventSubagentCaptured:
		// A plan capture's and a subagent cache's item is the note it wrote.
		kind = subjectNote
	case core.EventTaskTransition:
		kind = subjectTask
	case core.EventTrialRecorded:
		kind = subjectTrial
	case core.EventPlanShipped:
		kind = subjectPlan // ItemID is the plan's slug
	case core.EventGardenerAction:
		switch payloadStr(ev.Payload, "action") {
		case "digest_note", "undo_digest_note":
			kind = subjectNote
		default:
			kind = subjectProposal
		}
	case core.EventMemoryHeld, core.EventMemoryReleased, core.EventMemoryDiscarded:
		// A held write's item, when it changes one that exists.
		switch payloadStr(ev.Payload, "item_kind") {
		case subjectMemory, subjectNote:
			kind = payloadStr(ev.Payload, "item_kind")
		}
	case core.EventFavoriteChanged:
		switch k := payloadStr(ev.Payload, "kind"); k {
		case subjectMemory, subjectNote, subjectTask, subjectTrial, subjectSession:
			kind = k
		case subjectPlan:
			// A plan's star is kept on its note; the payload names its slug.
			return subjectPlan, payloadStr(ev.Payload, "id"), payloadStr(ev.Payload, "id") != ""
		}
	case core.EventSessionStarted, core.EventSessionEnded, core.EventSessionFlagged,
		core.EventSessionUnflagged, core.EventSessionTainted, core.EventSessionCleared,
		core.EventSessionTranscript:
		// A session's own event is about the session it ran in.
		return subjectSession, cmp.Or(ev.ItemID, ev.SessionID), cmp.Or(ev.ItemID, ev.SessionID) != ""
	}
	if kind == "" || ev.ItemID == "" {
		return "", "", false
	}
	return kind, ev.ItemID, true
}

// resolveEventSubjects resolves what ev is about: its own subject first, then,
// for a gardener proposal, the memories (or the plan) the proposal names. Every
// lookup goes through the request's handle.
func (s *Service) resolveEventSubjects(ctx context.Context, ev core.Event) ([]eventSubject, error) {
	kind, id, ok := eventSubjectRef(ev)
	if !ok {
		return nil, nil
	}
	if kind == subjectProposal {
		return s.proposalSubjects(ctx, id)
	}
	sub, err := s.resolveSubject(ctx, kind, id, ev.ProjectSlug)
	if err != nil {
		return nil, err
	}
	return []eventSubject{sub}, nil
}

// resolveSubject resolves one id of a kind into its card. project is the
// event's own project, where a plan without a page of its own is read.
func (s *Service) resolveSubject(ctx context.Context, kind, id, project string) (eventSubject, error) {
	db := s.db(ctx)
	sub := eventSubject{Kind: kind, ID: id, Missing: true}
	esc := url.PathEscape(id)
	switch kind {
	case subjectMemory:
		m, ok, err := store.MemoryByID(ctx, db, id)
		if err != nil || !ok {
			return sub, err
		}
		sub.Name, sub.Detail, sub.Project = m.Name, m.Description, m.Project
		w := taskWord{Key: string(m.Kind), Text: string(m.Kind)}
		if m.InvalidAt != nil {
			w = taskWord{Key: "retired", Text: string(m.Kind) + ", retired", Tone: "warn",
				Hint: "Superseded or archived: it no longer reaches briefings or recall."}
		}
		sub.Word = &w
		sub.Href, sub.Peek = "/console/memories/"+esc, true
	case subjectNote:
		n, ok, err := store.NoteByID(ctx, db, id)
		if err != nil || !ok {
			return sub, err
		}
		sub.Name, sub.Detail, sub.Project = cmp.Or(n.Title, n.Slug), n.Description, n.Project
		sub.Href, sub.Peek = "/console/notes/"+esc, true
	case subjectTask:
		t, err := store.TaskByID(ctx, db, id)
		if errors.Is(err, store.ErrTaskNotFound) {
			return sub, nil
		}
		if err != nil {
			return sub, err
		}
		words, err := s.wordTasks(ctx, []core.Task{t}, time.Now().UTC())
		if err != nil {
			return sub, err
		}
		w := words[t.ID]
		sub.Name, sub.Project, sub.Word = t.Title, t.ProjectSlug, &w
		if t.PlanSlug != "" {
			sub.Detail = "a step of plan:" + t.PlanSlug
		}
		sub.Href, sub.Peek = "/console/tasks/"+esc, true
	case subjectTrial:
		tr, ok, err := store.TrialByID(ctx, db, id)
		if err != nil || !ok {
			return sub, err
		}
		sub.Name, sub.Project = cmp.Or(tr.Title, tr.Lab), tr.ProjectSlug
		if tr.Lab != "" {
			sub.Detail = "a trial in lab " + tr.Lab
		}
		// Trial pages are the research feature's: off, there is none to open.
		if features.Enabled(s.effectiveFeatures(ctx), features.Research) {
			sub.Href, sub.Peek = "/console/trials/"+esc, true
		}
	case subjectSession:
		sess, ok, err := store.SessionByID(ctx, db, id)
		if err != nil || !ok {
			return sub, err
		}
		sub.Name, sub.Project = sessionLabel(sess), sess.ProjectSlug
		w := taskWord{Key: string(sess.Status), Text: string(sess.Status)}
		sub.Word = &w
		sub.Href, sub.Peek = "/console/sessions/"+esc, true
	case subjectPlan:
		href, err := s.planPageHref(ctx, project, id)
		if err != nil {
			return sub, err
		}
		if href == "" && project != "" {
			// A task-only plan: its steps are its project's to show.
			steps, err := store.ListTasksForPlan(ctx, db, project, "", id)
			if err != nil {
				return sub, err
			}
			if len(steps) == 0 {
				return sub, nil
			}
			href = projectTasksHref(project)
		}
		if href == "" {
			return sub, nil
		}
		sub.Name, sub.Project, sub.Href = "plan:"+id, project, href
		sub.Peek = strings.HasPrefix(href, "/console/plans/")
	default:
		return sub, nil
	}
	sub.Missing = false
	return sub, nil
}

// proposalSubjects resolves a gardener proposal into its card and the cards of
// what it names: the memories it would archive, merge, move or reclassify, or
// the plan it would settle. A proposal the request cannot read -- the view
// hides one whose payload names anything in a locked project -- is one
// unnamed card.
func (s *Service) proposalSubjects(ctx context.Context, id string) ([]eventSubject, error) {
	p, ok, err := store.ProposalByID(ctx, s.db(ctx), id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return []eventSubject{{Kind: subjectProposal, ID: id, Missing: true}}, nil
	}
	work, known := proposalWork[p.Kind]
	if !known {
		work = cmp.Or(strings.ReplaceAll(p.Kind, "_", " "), "a proposal")
	}
	w := proposalStateWord(p.Status)
	card := eventSubject{
		Kind: subjectProposal, ID: p.ID, Name: strings.ToUpper(work[:1]) + work[1:], Word: &w,
		Detail:  cmp.Or(payloadStr(p.Payload, "reason"), payloadStr(p.Payload, "rationale")),
		Project: payloadStr(p.Payload, "project"), Href: "/console/gardener/decisions",
	}
	if p.Status == store.ProposalPending {
		card.Href = "/console/gardener/" + url.PathEscape(p.ID)
	}
	out := []eventSubject{card}

	for _, named := range proposalNames(p) {
		sub, err := s.resolveSubject(ctx, named.kind, named.id, payloadStr(p.Payload, "project"))
		if err != nil {
			return nil, err
		}
		sub.Role = named.role
		out = append(out, sub)
	}
	return out, nil
}

// proposalName is one item a proposal's payload names, and what the proposal
// does with it.
type proposalName struct{ kind, id, role string }

// proposalNames reads the items a proposal is about from its payload, by kind:
// the same keys gardener.ResolveProposalScope decodes. Kinds about a project
// or a pattern (digest, split, memory_wanted, tool_error) name no item.
func proposalNames(p store.Proposal) []proposalName {
	var out []proposalName
	add := func(kind, id, role string) {
		if id != "" {
			out = append(out, proposalName{kind: kind, id: id, role: role})
		}
	}
	switch p.Kind {
	case store.ProposalArchive:
		add(subjectMemory, payloadStr(p.Payload, "id"), "the memory to archive")
	case store.ProposalRekind:
		add(subjectMemory, payloadStr(p.Payload, "id"), "the memory to reclassify")
	case store.ProposalReproject, store.ProposalRelocate:
		add(subjectMemory, payloadStr(p.Payload, "id"), "the memory to move")
	case store.ProposalMerge:
		add(subjectMemory, payloadStr(payloadMap(p.Payload, "keep"), "id"), "the memory to keep")
		add(subjectMemory, payloadStr(payloadMap(p.Payload, "drop"), "id"), "the memory to retire into it")
	case store.ProposalConsolidate:
		sources, _ := p.Payload["sources"].([]any)
		for _, src := range sources {
			if m, ok := src.(map[string]any); ok {
				add(subjectMemory, payloadStr(m, "id"), "a memory it folds in")
			}
		}
	case store.ProposalAbandonPlan, store.ProposalShipPlan, store.ProposalMergePlans:
		add(subjectPlan, payloadStr(p.Payload, "slug"), "the plan it settles")
	}
	return out
}

// proposalStateWord is a proposal's status in the owner's words.
func proposalStateWord(status string) taskWord {
	switch status {
	case store.ProposalPending:
		return taskWord{Key: status, Text: "waiting for review", Tone: "warn",
			Hint: "Waiting for the owner to apply or dismiss it on the Gardener screen."}
	case store.ProposalApplied:
		return taskWord{Key: status, Text: "applied", Tone: "ok"}
	case store.ProposalHidden:
		return taskWord{Key: status, Text: "hidden forever"}
	default:
		return taskWord{Key: status, Text: status}
	}
}

// eventHeadline is the page's title line: the event's summary, and for a
// gardener action about a proposal, the names of what the proposal is about --
// "proposed archiving a memory: <name>", where the ledger, which reads no
// proposal, says only "a memory".
func eventHeadline(ev core.Event, summary string, subjects []eventSubject) string {
	line := cmp.Or(summary, string(ev.Kind))
	if ev.Kind != core.EventGardenerAction {
		return line
	}
	var names []string
	for _, sub := range subjects {
		if sub.Role != "" && sub.Name != "" {
			names = append(names, sub.Name)
		}
	}
	if len(names) == 0 {
		return line
	}
	if strings.Contains(line, ": ") {
		return line + " (" + approval.JoinAnd(names) + ")"
	}
	return line + ": " + approval.JoinAnd(names)
}

// eventTrace starts with the shared Interactions projection, then promotes the
// few diagnostic event kinds that do not appear in that feed. Their primary
// evidence should still be readable without digging through raw JSON.
func eventTrace(ev core.Event, sessOf func(string) core.Session) interactionRow {
	row := toInteractionRow(ev, sessOf)
	switch ev.Kind {
	case core.EventRecallMiss:
		row.Label = payloadStr(ev.Payload, "source")
		row.Request = payloadStr(ev.Payload, "query")
	case core.EventHookError:
		row.Label = payloadStr(ev.Payload, "stage")
		row.Response = payloadStr(ev.Payload, "error")
		row.IsError = true
		row.Tone = "danger"
	case core.EventAgentMishap:
		row.Label = payloadStr(ev.Payload, "type")
		row.Response = payloadStr(ev.Payload, "description")
		row.Tone = "warn"
	}
	return row
}

// eventBodyLabels names the rich request/response panels in the event review
// workspace. The event feed already extracts the right bodies; these labels keep
// the detail page conversational instead of exposing transport field names.
func eventBodyLabels(kind core.EventKind) (request, response string) {
	switch kind {
	case core.EventToolCall:
		return "Request", "Response"
	case core.EventHookPrompt:
		return "Prompt", ""
	case core.EventInjected:
		return "Prompt", "Injected context"
	case core.EventRecallMiss:
		return "Query", ""
	case core.EventHookError:
		return "", "Error"
	case core.EventAgentMishap:
		return "", "Report"
	case core.EventSessionEnded:
		return "", "Session findings"
	case core.EventPlanCaptured, core.EventPlanApproved:
		return "", "Plan content"
	case core.EventSubagentCaptured:
		return "Prompt", "Agent report"
	default:
		return "Request", "Response"
	}
}

// eventBodyPayloadKeys lists payload fields already promoted into the primary
// request/response surface. Leaving them in the inspector would duplicate the
// longest, least scannable values; the complete object remains under Raw payload.
func eventBodyPayloadKeys(kind core.EventKind) []string {
	switch kind {
	case core.EventToolCall:
		return []string{"args", "result"}
	case core.EventHookPrompt:
		return []string{"prompt"}
	case core.EventInjected:
		return []string{"content", "item_ids", "prompt"}
	case core.EventRecallMiss:
		return []string{"query"}
	case core.EventHookError:
		return []string{"error"}
	case core.EventAgentMishap:
		return []string{"description"}
	case core.EventSessionEnded:
		return []string{"findings"}
	case core.EventPlanCaptured, core.EventPlanApproved:
		return []string{"content"}
	case core.EventSubagentCaptured:
		return []string{"content", "prompt"}
	default:
		return nil
	}
}

// eventEyebrow gives the hero a stable domain cue above the event summary.
func eventEyebrow(kind core.EventKind) string {
	s := string(kind)
	switch {
	case kind == core.EventToolCall:
		return "Tool execution"
	case kind == core.EventInjected:
		return "Context delivery"
	case kind == core.EventHookPrompt, kind == core.EventRecallMiss:
		return "Retrieval check"
	case kind == core.EventHookError:
		return "Hook failure"
	case kind == core.EventAgentMishap:
		return "Agent report"
	case strings.HasPrefix(s, "session."):
		return "Session lifecycle"
	case strings.HasPrefix(s, "plan."), kind == core.EventSubagentCaptured:
		return "Plan workflow"
	case strings.HasPrefix(s, "memory."):
		return "Knowledge lifecycle"
	case strings.HasPrefix(s, "note."):
		return "Note lifecycle"
	case strings.HasPrefix(s, "task."):
		return "Task lifecycle"
	case strings.HasPrefix(s, "trial."):
		return "Research lifecycle"
	case strings.HasPrefix(s, "gardener."):
		return "Gardener action"
	case strings.HasPrefix(s, "favorite."):
		return "Curation action"
	default:
		return "Activity event"
	}
}

// eventReturnLink sends interaction events back to the trace they came from and
// durable domain events back to the overview activity ledger. Recall twins are
// hidden from Interactions, so their honest parent remains Overview.
func eventReturnLink(ev core.Event) (href, label string) {
	if isInteraction(ev) && !skipInteraction(ev) {
		return "/console/interactions", "Interactions"
	}
	return "/console/", "Overview"
}

// eventSurfacesMemories reports whether an event's item ids point at memories in
// the index. Only injection events (retrieval.injected) carry surfaced-memory ids
// -- recall and the briefing hook record them in the payload's item_ids array.
// Every other kind's ItemID points at a task, note, trial, or plan, so it must not
// be resolved against the memory index: doing so renders a phantom "removed" row
// (e.g. a task.transition echoing its own task id as a deleted memory). hook.prompt
// is a recall miss and carries no item ids, so it is excluded too.
func eventSurfacesMemories(k core.EventKind) bool {
	return k == core.EventInjected
}

// resolveEventItems turns the memory ids an injection event surfaced (its payload
// item_ids) into display rows, looking each up in the memory index. Order follows
// first appearance; unresolved ids are kept and marked Missing. Non-injection kinds
// surface nothing (see eventSurfacesMemories).
func (s *Service) resolveEventItems(ctx context.Context, ev core.Event) ([]eventItem, error) {
	if !eventSurfacesMemories(ev.Kind) {
		return nil, nil
	}
	ids := injectedEventItemIDs(ev)
	if len(ids) == 0 {
		return nil, nil
	}
	byID, err := store.MemoriesByIDs(ctx, s.db(ctx), ids)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(ids))
	out := make([]eventItem, 0, len(ids))
	for _, id := range ids {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		if m, ok := byID[id]; ok {
			out = append(out, eventItem{
				ID: m.ID, Name: m.Name, Kind: string(m.Kind),
				Description: m.Description, Project: m.Project,
			})
			continue
		}
		out = append(out, eventItem{ID: id, Missing: true})
	}
	return out, nil
}

// scalarFields projects a payload's remaining fields into sorted key/value rows,
// skipping the named keys (handled elsewhere) and empty values. Non-string values
// are compact-JSON encoded so nested data still shows.
func scalarFields(p map[string]any, skip ...string) []kvPair {
	if len(p) == 0 {
		return nil
	}
	skipped := make(map[string]struct{}, len(skip))
	for _, k := range skip {
		skipped[k] = struct{}{}
	}
	keys := make([]string, 0, len(p))
	for k := range p {
		if _, ok := skipped[k]; ok {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]kvPair, 0, len(keys))
	for _, k := range keys {
		v := stringifyValue(p[k])
		if v == "" {
			continue
		}
		out = append(out, kvPair{Key: k, Value: v})
	}
	return out
}

// stringifyValue renders a payload value for a key/value row: strings verbatim,
// everything else as compact JSON.
func stringifyValue(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

// prettyPayload renders a payload as indented JSON for the raw disclosure, or ""
// when there is nothing to show.
func prettyPayload(p map[string]any) string {
	if len(p) == 0 {
		return ""
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return ""
	}
	return string(b)
}
