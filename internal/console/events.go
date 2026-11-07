package console

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/arctop/seamless/internal/core"
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
	Event   eventRow       `json:"event"`
	Trace   interactionRow `json:"trace"`             // rich request/response + session attribution
	Prompt  string         `json:"prompt,omitempty"`  // user prompt that triggered the event, if any
	Content string         `json:"content,omitempty"` // verbatim injected/large text, if any
	Items   []eventItem    `json:"items,omitempty"`   // resolved memories the event referenced
	Fields  []kvPair       `json:"fields,omitempty"`  // remaining scalar payload fields
	RawJSON string         `json:"rawJson"`           // pretty-printed full payload

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
	data.Fields = scalarFields(ev.Payload, eventBodyPayloadKeys(ev.Kind)...)

	// No Active nav key: the event detail is a leaf reached from Overview/
	// Interactions via the crumb, so highlighting a nav item would mislead.
	s.renderDetail(w, r, "event", pageData{Title: "Event " + shortID(ev.ID), Data: data})
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
