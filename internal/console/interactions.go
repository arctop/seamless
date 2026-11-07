package console

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/store"
)

// interactionKinds are the event kinds the Interactions feed surfaces: live MCP
// tool calls, recall-miss prompts, hook injections, session lifecycle, and the
// plan-mode capture stream.
var interactionKinds = []core.EventKind{
	core.EventToolCall, core.EventHookPrompt, core.EventInjected,
	core.EventSessionStarted, core.EventSessionEnded,
	core.EventPlanCaptured, core.EventPlanPresented, core.EventPlanApproved,
	core.EventSubagentCaptured,
}

// isInteraction reports whether an event belongs on the Interactions feed.
func isInteraction(e core.Event) bool {
	switch e.Kind {
	case core.EventToolCall, core.EventHookPrompt, core.EventInjected,
		core.EventSessionStarted, core.EventSessionEnded,
		core.EventPlanCaptured, core.EventPlanPresented, core.EventPlanApproved,
		core.EventSubagentCaptured:
		return true
	}
	return false
}

// skipInteraction drops rows that would duplicate another feed row. A recall via
// the MCP tool records BOTH a retrieval.injected (source=recall) and a tool.call;
// the tool.call carries the same content plus its args, so the injected twin is
// dropped. Session lifecycle twins (an MCP session_start's session.started plus
// its tool.call) are kept deliberately, as feed markers.
func skipInteraction(e core.Event) bool {
	return e.Kind == core.EventInjected && payloadStr(e.Payload, "source") == "recall"
}

// interactionRow is a display-ready projection of one Interactions event: enough
// to render the summary line and the request/response bodies without a second
// fetch. It is JSON-tagged so the screen's JS (and the CLI) consume it directly.
type interactionRow struct {
	ID          string    `json:"id"`
	TS          time.Time `json:"ts"`
	Kind        string    `json:"kind"`
	Tone        string    `json:"tone"`
	KindLabel   string    `json:"kindLabel"` // evtLabel(kind): the human name the feed shows
	Icon        string    `json:"icon"`      // evtIcon(kind): the glyph, named once, server-side
	Label       string    `json:"label"`
	Summary     string    `json:"summary"`
	Project     string    `json:"project,omitempty"`
	SessionID   string    `json:"sessionId,omitempty"`
	SessionName string    `json:"sessionName,omitempty"`
	Ambient     bool      `json:"ambient,omitempty"`
	Harness     string    `json:"harness,omitempty"` // client discriminator (claude-code|codex)
	Model       string    `json:"model,omitempty"`   // model powering the session, verbatim
	IsError     bool      `json:"isError,omitempty"`
	DurationMS  int64     `json:"durationMs,omitempty"`
	Request     string    `json:"request,omitempty"`  // pretty-JSON tool args, or prompt text
	Response    string    `json:"response,omitempty"` // tool result / injected content / findings
	Items       int       `json:"items,omitempty"`    // count of surfaced memories
}

// toInteractionRow projects an event into a feed row. sessOf resolves a session
// id to its session (memoized by the caller; the zero Session means unknown).
// It tolerates import-shaped tool.call payloads that carry no args/result.
func toInteractionRow(e core.Event, sessOf func(string) core.Session) interactionRow {
	p := e.Payload
	row := interactionRow{
		ID: e.ID, TS: e.TS, Kind: string(e.Kind), KindLabel: evtLabel(string(e.Kind)), Icon: evtIcon(string(e.Kind)),
		Tone: evtTone(string(e.Kind)), Summary: eventSummary(e),
		Project: e.ProjectSlug, SessionID: e.SessionID,
		Items: len(injectedEventItemIDs(e)),
	}
	if isErr, _ := p["is_error"].(bool); isErr {
		row.IsError = true
		row.Tone = "danger"
	}
	if e.SessionID != "" && sessOf != nil {
		sess := sessOf(e.SessionID)
		row.SessionName, row.Ambient = sess.Name, sess.Ambient
		row.Harness, row.Model = harnessOf(sess), sess.Model
	}
	if d, ok := p["duration_ms"].(float64); ok {
		row.DurationMS = int64(d)
	}
	switch e.Kind {
	case core.EventToolCall:
		row.Label = payloadStr(p, "tool")
		row.Request = prettyArgs(p["args"])
		if r := payloadStr(p, "result"); r != "" {
			row.Response = r
		} else if row.IsError {
			row.Response = payloadStr(p, "error")
		}
	case core.EventHookPrompt:
		row.Label = payloadStr(p, "hook")
		row.Request = payloadStr(p, "prompt")
	case core.EventInjected:
		row.Label = payloadStr(p, "hook")
		row.Request = payloadStr(p, "prompt")
		row.Response = payloadStr(p, "content")
	case core.EventSessionStarted:
		row.Label = "session"
	case core.EventSessionEnded:
		row.Label = "session"
		row.Response = payloadStr(p, "findings")
	case core.EventPlanCaptured, core.EventPlanApproved:
		row.Label = payloadStr(p, "basename")
		row.Response = payloadStr(p, "content")
	case core.EventPlanPresented:
		row.Label = payloadStr(p, "basename")
	case core.EventSubagentCaptured:
		row.Label = payloadStr(p, "agent_type")
		row.Request = payloadStr(p, "prompt")
		row.Response = payloadStr(p, "content")
	}
	return row
}

// prettyArgs renders a tool.call's args map as indented JSON, or "" when absent.
func prettyArgs(v any) string {
	m, ok := v.(map[string]any)
	if !ok || len(m) == 0 {
		return ""
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return ""
	}
	return string(b)
}

// sessionNamer returns a memoized session-id -> session resolver bound to ctx,
// so a feed of many rows sharing a session costs one query. Unknown ids are
// negatively cached and resolve to the zero Session.
func (s *Service) sessionNamer(ctx context.Context) func(string) core.Session {
	cache := map[string]core.Session{}
	return func(id string) core.Session {
		if id == "" {
			return core.Session{}
		}
		if sess, ok := cache[id]; ok {
			return sess
		}
		sess, ok, err := store.SessionByID(ctx, s.db(ctx), id)
		if err != nil || !ok {
			cache[id] = core.Session{} // negative cache
			return core.Session{}
		}
		cache[id] = sess
		return sess
	}
}

// interactionsData is the Interactions screen's JSON payload. NextTS/NextID
// cursor the next (older) page, while AfterTS holds that history import's lower
// bound. Volume carries histogram buckets for embedded project/session traces.
type interactionsData struct {
	Rows    []interactionRow `json:"rows"`
	Volume  []volBucket      `json:"volume,omitempty"`
	Window  int              `json:"window,omitempty"`  // seconds the Volume spans (0 = all history)
	AfterTS string           `json:"afterTs,omitempty"` // fixed lower bound for an explicitly loaded history window
	NextTS  string           `json:"nextTs,omitempty"`
	NextID  string           `json:"nextId,omitempty"`
}

// volBucket is one column of the interaction-volume histogram: a time slice with
// its total plus a per-category split, so the bar can stack tool/injection/
// session/plan/prompt hues like Sentry's events-over-time chart.
type volBucket struct {
	T        string `json:"t"`                  // bucket start (canonical ts), for the tooltip
	N        int    `json:"n"`                  // total events in the bucket
	LatestID string `json:"latestId,omitempty"` // newest event represented by this bucket
	Tool     int    `json:"tool,omitempty"`
	Inject   int    `json:"inject,omitempty"`
	Session  int    `json:"session,omitempty"`
	Plan     int    `json:"plan,omitempty"`
	Prompt   int    `json:"prompt,omitempty"`
}

// interactionsPageLimit bounds one page / gap-fill batch of the feed. History
// imports are intentionally bounded too: a user can page within the selected
// window without one click flooding the browser with an unbounded event log.
const (
	interactionsPageLimit     = 200
	maxHistoryLookbackSeconds = 30 * 24 * 60 * 60
)

// volBuckets is the histogram column count; volTickCap bounds the "all" window.
const (
	volBuckets = 40
	volTickCap = 20000
)

// volCategory maps an event kind to a histogram stack key (mirrored by the CSS
// .ix-vbar-<cat> hues and the JS renderer).
func volCategory(kind string) string {
	switch {
	case kind == string(core.EventToolCall):
		return "tool"
	case kind == string(core.EventInjected):
		return "inject"
	case kind == string(core.EventHookPrompt):
		return "prompt"
	case strings.HasPrefix(kind, "session."):
		return "session"
	case strings.HasPrefix(kind, "plan."), kind == string(core.EventSubagentCaptured):
		return "plan"
	default:
		return ""
	}
}

// buildVolume buckets ticks (newest-first) into volBuckets columns spanning the
// window ending at now. A non-positive window spans from the oldest tick to now
// ("all"). Empty input yields nil so the client hides the chart.
func buildVolume(ticks []events.KindTick, window time.Duration, now time.Time) []volBucket {
	if len(ticks) == 0 {
		return nil
	}
	if window <= 0 {
		window = now.Sub(ticks[len(ticks)-1].TS) // ticks are newest-first
	}
	window = max(window, time.Minute)
	slice := max(window/volBuckets, time.Nanosecond)
	start := now.Add(-window)
	out := make([]volBucket, volBuckets)
	for i := range out {
		out[i].T = core.FormatTime(start.Add(time.Duration(i) * slice))
	}
	for _, tk := range ticks {
		if tk.TS.Before(start) {
			continue
		}
		idx := int(tk.TS.Sub(start) / slice)
		if idx < 0 {
			idx = 0
		}
		if idx >= volBuckets {
			idx = volBuckets - 1
		}
		b := &out[idx]
		b.N++
		// KindTimeline and the session-detail caller both supply newest-first
		// ticks, so the first id placed in a bucket is its preferred row target.
		if b.LatestID == "" {
			b.LatestID = tk.ID
		}
		switch volCategory(tk.Kind) {
		case "tool":
			b.Tool++
		case "inject":
			b.Inject++
		case "session":
			b.Session++
		case "plan":
			b.Plan++
		case "prompt":
			b.Prompt++
		}
	}
	return out
}

// interactionVolume computes the histogram for the given window (seconds; 0 = all
// history) optionally scoped to a project slug.
func (s *Service) interactionVolume(ctx context.Context, project string, windowSecs int) ([]volBucket, error) {
	now := time.Now().UTC()
	var sinceTS string
	window := time.Duration(windowSecs) * time.Second
	if windowSecs > 0 {
		sinceTS = core.FormatTime(now.Add(-window))
	}
	ticks, err := s.eventLog(ctx).KindTimeline(ctx, interactionKinds, project, sinceTS, volTickCap)
	if err != nil {
		return nil, err
	}
	return buildVolume(ticks, window, now), nil
}

// interactions serves the live transport feed: an empty HTML shell (default)
// that receives new rows over SSE. JSON callers can explicitly add a bounded
// history window via history=<seconds>, page within its returned afterTs bound,
// or gap-fill newer rows via since/sinceTs after an SSE drop.
func (s *Service) interactions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if s.cfg.Events == nil {
		http.Error(w, "interactions unavailable", http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()

	// Volume-only refresh: the client re-fetches just the histogram when the
	// window changes, so skip the heavier row query. Always JSON (a browser
	// hitting this URL falls through to the full page).
	if vs := q.Get("volume"); vs != "" && wantsJSON(r) {
		// 0 is the legitimate "all time" window, so only an unparseable or
		// negative value is rejected. Falling back to 0 would answer a request
		// the client never made -- the widest possible query -- and echo back
		// Window: 0 as though that had been asked for.
		secs, err := strconv.Atoi(vs)
		if err != nil || secs < 0 {
			s.badRequest(w, r, "volume must be a non-negative number of seconds (0 = all time)")
			return
		}
		vol, err := s.interactionVolume(ctx, "", secs)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		s.render(w, r, "interactions", pageData{Data: interactionsData{Volume: vol, Window: secs}})
		return
	}

	// Cursors are compound: accepting only one half quietly turns a bounded page
	// into a different query. since may be empty for the page-load timestamp
	// anchor, but sinceTs must always name the boundary.
	hasSince := q.Has("since") || q.Has("sinceTs")
	hasBefore := q.Has("before") || q.Has("beforeTs")
	if hasSince && (!q.Has("since") || q.Get("sinceTs") == "") {
		s.badRequest(w, r, "since and sinceTs must be provided together")
		return
	}
	if hasBefore && (!q.Has("before") || !q.Has("beforeTs") || q.Get("before") == "" || q.Get("beforeTs") == "") {
		s.badRequest(w, r, "before and beforeTs must be provided together")
		return
	}
	if hasBefore {
		if parsed, err := core.ParseTime(q.Get("beforeTs")); err != nil || parsed.IsZero() {
			s.badRequest(w, r, "beforeTs must be an RFC3339 timestamp")
			return
		}
	}
	if hasSince && (hasBefore || q.Has("history") || q.Has("afterTs")) {
		s.badRequest(w, r, "since cursor cannot be combined with history paging")
		return
	}
	if hasSince {
		if _, err := core.ParseTime(q.Get("sinceTs")); err != nil {
			s.badRequest(w, r, "sinceTs must be an RFC3339 timestamp")
			return
		}
	}

	var afterTS string
	if q.Has("history") {
		if q.Has("afterTs") || hasBefore {
			s.badRequest(w, r, "history starts a window; page it with the returned afterTs and before cursor")
			return
		}
		secs, err := strconv.ParseInt(q.Get("history"), 10, 64)
		if err != nil || secs <= 0 || secs > maxHistoryLookbackSeconds {
			s.badRequest(w, r, "history must be a positive number of seconds up to 30 days")
			return
		}
		afterTS = core.FormatTime(time.Now().UTC().Add(-time.Duration(secs) * time.Second))
	} else if q.Has("afterTs") {
		parsed, err := core.ParseTime(q.Get("afterTs"))
		if err != nil || parsed.IsZero() {
			s.badRequest(w, r, "afterTs must be an RFC3339 timestamp")
			return
		}
		afterTS = core.FormatTime(parsed)
	}

	name := s.sessionNamer(ctx)

	gapFill := hasSince
	var evs []core.Event
	var err error
	if gapFill {
		evs, err = s.eventLog(ctx).ByKindsSince(ctx, interactionKinds, q.Get("sinceTs"), q.Get("since"), interactionsPageLimit)
	} else if afterTS != "" {
		evs, err = s.eventLog(ctx).ByKindsAfter(ctx, interactionKinds, afterTS, q.Get("beforeTs"), q.Get("before"), interactionsPageLimit)
	} else {
		evs, err = s.eventLog(ctx).ByKinds(ctx, interactionKinds, q.Get("beforeTs"), q.Get("before"), interactionsPageLimit)
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	rows := make([]interactionRow, 0, len(evs))
	for _, e := range evs {
		if skipInteraction(e) {
			continue
		}
		rows = append(rows, toInteractionRow(e, name))
	}
	data := interactionsData{Rows: rows, AfterTS: afterTS}
	// Older-page cursor: the last (oldest) event of a full descending page. Based
	// on the raw fetch, not the filtered rows, so paging never stalls on a page
	// that was all-skipped.
	if !gapFill && len(evs) == interactionsPageLimit {
		last := evs[len(evs)-1]
		data.NextTS = core.FormatTime(last.TS)
		data.NextID = last.ID
	}
	s.render(w, r, "interactions", pageData{Title: "Interactions", Active: "interactions", Data: data})
}
