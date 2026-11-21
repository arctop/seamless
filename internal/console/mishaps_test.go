package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/store"
)

// msFleet seeds a session, a memory, and five mishaps: three typed (one
// naming the memory), and two from before types -- a description alone, and
// a description with item_ids -- plus one typed report nine days old in the
// global scope.
type msFleet struct {
	sess, memID                                     string
	trap, trapLinked, target, plain, linked, oldFab string
}

func seedMishapFleet(t *testing.T, db *sql.DB) msFleet {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	var f msFleet
	f.sess = mustID(t)
	require.NoError(t, store.CreateSession(ctx, db, core.Session{
		ID: f.sess, Name: "cc/mishap-review", ProjectSlug: "demo", Status: core.SessionCompleted,
		Ambient: true, Model: "claude-fable-5", CreatedAt: now, UpdatedAt: now,
	}))
	f.memID = mustID(t)
	insertMemoryRow(t, db, f.memID, "zsh-equals-expansion", "")
	rec := events.NewRecorder(db)
	at := func(d time.Duration) time.Time { return now.Add(-d) }
	record := func(project string, ts time.Time, payload map[string]any) string {
		id, err := rec.Record(ctx, core.Event{Kind: core.EventAgentMishap, SessionID: f.sess, ProjectSlug: project, TS: ts, Payload: payload})
		require.NoError(t, err)
		return id
	}
	f.trap = record("demo", at(time.Minute), map[string]any{"description": "shell-trap: a bare == in zsh died", "type": "shell-trap"})
	f.trapLinked = record("demo", at(2*time.Minute), map[string]any{"description": "shell-trap: zsh-equals-expansion bit again",
		"type": "shell-trap", "item_ids": []string{f.memID}})
	f.target = record("demo", at(3*time.Minute), map[string]any{"description": "wrong-target: edited the installed config", "type": "wrong-target"})
	f.plain = record("demo", at(4*time.Minute), map[string]any{"description": "pkill matched the live daemon"})
	f.linked = record("other", at(5*time.Minute), map[string]any{"description": "violated zsh-equals-expansion again",
		"item_ids": []string{f.memID}})
	f.oldFab = record("", at(9*24*time.Hour), map[string]any{"description": "fabricated: a flag that does not exist", "type": "fabricated"})
	return f
}

// msJSON is the page's answer as seam mishaps reads it.
type msJSON struct {
	Type        string `json:"type"`
	Project     string `json:"project"`
	Window      string `json:"window"`
	Days        int    `json:"days"`
	WindowLabel string `json:"windowLabel"`
	Total       int    `json:"total"`
	Shown       int    `json:"shown"`
	Types       []struct {
		Type    string `json:"type"`
		Meaning string `json:"meaning"`
		Count   int    `json:"count"`
	} `json:"types"`
	Mishaps []struct {
		ID          string `json:"id"`
		Type        string `json:"type"`
		Text        string `json:"text"`
		Description string `json:"description"`
		Project     string `json:"project"`
		SessionID   string `json:"sessionId"`
		Session     string `json:"session"`
		Memories    []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"memories"`
	} `json:"mishaps"`
	Memories []struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Count int    `json:"count"`
	} `json:"memories"`
}

func (d msJSON) ids() []string {
	out := []string{}
	for _, m := range d.Mishaps {
		out = append(out, m.ID)
	}
	return out
}

func (d msJSON) counts() map[string]int {
	out := map[string]int{}
	for _, tc := range d.Types {
		out[tc.Type] = tc.Count
	}
	return out
}

func TestMishapsPage_JSONListsByType(t *testing.T) {
	db, mux := newConsole(t)
	f := seedMishapFleet(t, db)

	var all msJSON
	getJSON(t, mux, "/console/mishaps?format=json", &all)
	require.Equal(t, "all", all.Window, "all time unless a window is named")
	require.Equal(t, 6, all.Total)
	require.Equal(t, 6, all.Shown)
	require.Equal(t, []string{f.trap, f.trapLinked, f.target, f.plain, f.linked, f.oldFab}, all.ids(), "newest first")
	require.Equal(t, map[string]int{"shell-trap": 2, "wrong-target": 1, "live-state": 0, "ignored-warning": 0,
		"fabricated": 1, "other": 0, "untyped": 2}, all.counts(), "every type, zeros included, then the untyped")
	var order []string
	for _, tc := range all.Types {
		order = append(order, tc.Type)
		require.NotEmpty(t, tc.Meaning, tc.Type)
	}
	require.Equal(t, []string{"shell-trap", "wrong-target", "live-state", "ignored-warning", "fabricated", "other", "untyped"}, order)

	// A typed report: the text without its prefix, the description as recorded.
	require.Equal(t, "shell-trap", all.Mishaps[0].Type)
	require.Equal(t, "a bare == in zsh died", all.Mishaps[0].Text)
	require.Equal(t, "shell-trap: a bare == in zsh died", all.Mishaps[0].Description)
	require.Equal(t, "cc/mishap-review", all.Mishaps[0].Session)
	// Old reports: untyped, their text unchanged, their links kept.
	require.Empty(t, all.Mishaps[3].Type)
	require.Equal(t, "pkill matched the live daemon", all.Mishaps[3].Text)
	require.Equal(t, all.Mishaps[3].Description, all.Mishaps[3].Text)
	require.Empty(t, all.Mishaps[4].Type)
	require.Equal(t, "violated zsh-equals-expansion again", all.Mishaps[4].Text)
	require.Len(t, all.Mishaps[4].Memories, 1)
	require.Equal(t, "zsh-equals-expansion", all.Mishaps[4].Memories[0].Name)
	// The memories block: how many reports named each memory.
	require.Len(t, all.Memories, 1)
	require.Equal(t, f.memID, all.Memories[0].ID)
	require.Equal(t, "zsh-equals-expansion", all.Memories[0].Name)
	require.Equal(t, 2, all.Memories[0].Count)

	for _, tc := range []struct {
		path     string
		want     []string
		memories int
	}{
		{"/console/mishaps?format=json&type=shell-trap", []string{f.trap, f.trapLinked}, 1},
		{"/console/mishaps?format=json&type=untyped", []string{f.plain, f.linked}, 1},
		{"/console/mishaps?format=json&type=live-state", []string{}, 0},
		{"/console/mishaps?format=json&project=demo", []string{f.trap, f.trapLinked, f.target, f.plain}, 1},
		{"/console/mishaps?format=json&project=global", []string{f.oldFab}, 0},
		{"/console/mishaps?format=json&w=7d", []string{f.trap, f.trapLinked, f.target, f.plain, f.linked}, 1},
		{"/console/mishaps?format=json&days=10", []string{f.trap, f.trapLinked, f.target, f.plain, f.linked, f.oldFab}, 1},
		{"/console/mishaps?format=json&type=wrong-target&project=demo&w=24h", []string{f.target}, 0},
		{"/console/mishaps?format=json&type=&project=", []string{f.trap, f.trapLinked, f.target, f.plain, f.linked, f.oldFab}, 1},
	} {
		t.Run(tc.path, func(t *testing.T) {
			var got msJSON
			getJSON(t, mux, tc.path, &got)
			require.Equal(t, tc.want, got.ids())
			require.Len(t, got.Memories, tc.memories)
			require.Equal(t, len(tc.want), got.Shown)
		})
	}

	// The chips count the scope and window whatever type is selected.
	var trap msJSON
	getJSON(t, mux, "/console/mishaps?format=json&type=shell-trap&w=7d", &trap)
	require.Equal(t, 5, trap.Total)
	require.Equal(t, 2, trap.Shown)
	require.Equal(t, 0, trap.counts()["fabricated"], "the nine-day-old report is outside the window")
	require.Equal(t, 2, trap.counts()["untyped"])
	var days msJSON
	getJSON(t, mux, "/console/mishaps?format=json&days=3", &days)
	require.Equal(t, 3, days.Days)
	require.Empty(t, days.Window)
	require.Equal(t, "last 3 days", days.WindowLabel)
}

// A filter the page cannot read is a 400 naming what it takes, never a
// quiet default.
func TestMishapsPage_RefusesWhatItCannotRead(t *testing.T) {
	_, mux := newConsole(t)
	for path, want := range map[string]string{
		"/console/mishaps?type=bogus":       "valid values are shell-trap, wrong-target, live-state, ignored-warning, fabricated, other, untyped",
		"/console/mishaps?type=shell_trap":  `invalid type "shell_trap"`,
		"/console/mishaps?w=1y":             `invalid w "1y"`,
		"/console/mishaps?days=0":           `invalid days "0"`,
		"/console/mishaps?days=three":       `invalid days "three"`,
		"/console/mishaps?w=7d&days=3":      "name two windows",
		"/console/mishaps?project=all":      `invalid project "all"`,
		"/console/mishaps?project=..%2Fetc": `invalid project "../etc"`,
	} {
		req := httptest.NewRequest(http.MethodGet, path+"&format=json", nil)
		req.Header.Set("Authorization", "Bearer "+testKey)
		rr := do(mux, req)
		require.Equal(t, http.StatusBadRequest, rr.Code, path)
		var answer struct {
			Error string `json:"error"`
		}
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &answer), path)
		require.Contains(t, answer.Error, want, path)
		// A browser gets the styled error page, at the same status.
		page := getPeek(t, mux, path)
		require.Equal(t, http.StatusBadRequest, page.Code, path)
	}
}

func TestMishapsPage_RendersChipsRowsAndMemories(t *testing.T) {
	db, mux := newConsole(t)
	f := seedMishapFleet(t, db)

	body := getHTMLBody(t, mux, "/console/mishaps")
	require.Contains(t, body, `<h1>`)
	require.Contains(t, body, "Mishaps")
	// Chips: all, each type with its count, untyped; in place, no reload.
	require.Contains(t, body, `<nav class="mishap-chips" aria-label="Filter by type" data-seam-query>`)
	require.Contains(t, body, `<a class="mishap-chip on" href="/console/mishaps" data-mishap-type="" title="Every report, whatever its type" aria-current="true"><span>All</span><strong>6</strong></a>`)
	require.Contains(t, body, `href="/console/mishaps?type=shell-trap" data-mishap-type="shell-trap" title="`+core.MishapShellTrap.Meaning()+`"><span>shell-trap</span><strong>2</strong></a>`)
	require.Contains(t, body, `href="/console/mishaps?type=untyped" data-mishap-type="untyped"`)
	// Rows: a typed report's chip and its text without the prefix; an old
	// report reads untyped, as written.
	require.Contains(t, body, `<span class="badge mishap-type" data-mishap-type="shell-trap" title="`+core.MishapShellTrap.Meaning()+`">shell-trap</span>`)
	require.Contains(t, body, `<a class="mishap-text" href="/console/events/`+f.trap+`" data-peek>a bare == in zsh died</a>`)
	require.Contains(t, body, `<span class="badge mishap-type is-untyped" data-mishap-type="untyped" title="This report named no type">untyped</span>`)
	require.Contains(t, body, `<a class="mishap-text" href="/console/events/`+f.plain+`" data-peek>pkill matched the live daemon</a>`)
	require.Contains(t, body, `href="/console/sessions/`+f.sess+`" data-peek`)
	require.Contains(t, body, `<a href="/console/mishaps?project=demo" data-seam-query title="Only this scope's mishaps">`)
	require.Contains(t, body, `<a href="/console/mishaps?project=global" data-seam-query`)
	// The memories block links the memory and counts its reports.
	require.Contains(t, body, `<a href="/console/memories/`+f.memID+`" data-peek>zsh-equals-expansion</a>`)
	require.Contains(t, body, `<strong title="Reports that named it">2</strong>`)
	// The window links keep the type and project.
	typed := getHTMLBody(t, mux, "/console/mishaps?type=shell-trap&project=demo")
	require.Contains(t, typed, `href="/console/mishaps?project=demo&amp;type=shell-trap&amp;w=7d"`)
	require.Contains(t, typed, `<a href="/console/mishaps?type=shell-trap" data-seam-query>Clear</a>`)
	require.NotContains(t, typed, f.plain, "the untyped reports are filtered out")

	empty := getHTMLBody(t, mux, "/console/mishaps?type=live-state")
	require.Contains(t, empty, "No live-state mishaps reported yet.")
}

// The Overview's attention card names the latest report's type, and opens
// the Mishaps screen once there is more than one report.
func TestOverview_MishapCardOpensTheMishapsScreen(t *testing.T) {
	db, mux := newConsole(t)
	f := seedMishapFleet(t, db)
	body := getHTMLBody(t, mux, "/console/")
	require.Contains(t, body, `<a class="ov2-attn-card" data-sev="danger" href="/console/mishaps">`)
	require.Contains(t, body, "latest shell-trap ")
	_ = f

	var ov struct {
		Mishaps []struct {
			ID, Type, Description string
		}
	}
	getJSON(t, mux, "/console/?format=json", &ov)
	require.NotEmpty(t, ov.Mishaps)
	require.Equal(t, "shell-trap", ov.Mishaps[0].Type)
	require.Equal(t, "shell-trap: a bare == in zsh died", ov.Mishaps[0].Description, "the overview keeps the text as recorded")
}

// The activity line shows a report's type; an untyped one reads as before.
func TestEventSummary_MishapType(t *testing.T) {
	require.Equal(t, "mishap (shell-trap): a bare == died", eventSummary(core.Event{Kind: core.EventAgentMishap,
		Payload: map[string]any{"description": "shell-trap: a bare == died", "type": "shell-trap"}}))
	require.Equal(t, "mishap: pkill matched the live daemon", eventSummary(core.Event{Kind: core.EventAgentMishap,
		Payload: map[string]any{"description": "pkill matched the live daemon"}}))
	require.Equal(t, "mishap: other: old", eventSummary(core.Event{Kind: core.EventAgentMishap,
		Payload: map[string]any{"description": "other: old"}}), "a prefix with no recorded type is plain text")
	require.Equal(t, "mishap: odd", eventSummary(core.Event{Kind: core.EventAgentMishap,
		Payload: map[string]any{"description": "odd", "type": "not-a-type"}}))
}

// The screen is in the registry with no sidebar row, at Basic like the card
// that links to it, so the palette and the docs matrix know it.
func TestMishapsScreen_Registered(t *testing.T) {
	sc, ok := screenByID("mishaps")
	require.True(t, ok)
	require.False(t, sc.NavRow)
	require.Equal(t, levelBasic, sc.Min)
	require.Equal(t, mishapsPath, sc.Href)
	require.Empty(t, sc.Feature)

	_, mux := newConsoleLevel(t, config.Features{}, "basic")
	page := getHTMLBody(t, mux, "/console/memories")
	require.Contains(t, page, `<a href="/console/mishaps" data-jump="Mishaps" data-hint="what agents reported going wrong, by type"`)
	require.NotContains(t, getHTMLBody(t, mux, "/console/mishaps"), "data-level-banner", "a Basic screen carries no banner at Basic")
}

// The Diary's mishap moment wears its type chip, and its JSON carries the
// type beside the event; a report from before types carries none.
func TestDiary_MishapMomentShowsItsType(t *testing.T) {
	db, mux := newConsoleFeatures(t, config.Features{Diary: true})
	ctx := context.Background()
	noon := dyMidnight(time.Now()).AddDate(0, 0, -1).Add(12 * time.Hour)
	sess := mustID(t)
	require.NoError(t, store.CreateSession(ctx, db, core.Session{
		ID: sess, Name: "cc/diary-mishap", ProjectSlug: "demo", Status: core.SessionCompleted,
		Ambient: true, CreatedAt: noon.UTC(), UpdatedAt: noon.Add(time.Hour).UTC(),
	}))
	rec := events.NewRecorder(db)
	for i, p := range []map[string]any{
		{"description": "shell-trap: a bare == in zsh died", "type": "shell-trap"},
		{"description": "pkill matched the live daemon"},
	} {
		_, err := rec.Record(ctx, core.Event{Kind: core.EventAgentMishap, SessionID: sess, ProjectSlug: "demo",
			TS: noon.Add(time.Duration(i+1) * time.Minute).UTC(), Payload: p})
		require.NoError(t, err)
	}
	path := "/console/diary?day=" + noon.Format("2006-01-02")
	body := getHTMLBody(t, mux, path)
	require.Contains(t, body, `<span class="badge mishap-type" data-mishap-type="shell-trap">shell-trap</span>`)
	require.Equal(t, 1, strings.Count(body, `class="badge mishap-type"`), "the untyped report wears no chip")

	var day struct {
		Moments struct {
			Mishaps []struct {
				Type  string `json:"type"`
				Event struct {
					Title string `json:"title"`
				} `json:"event"`
			} `json:"mishaps"`
		} `json:"moments"`
	}
	getJSON(t, mux, path+"&format=json", &day)
	types := map[string]string{}
	for _, m := range day.Moments.Mishaps {
		types[m.Event.Title] = m.Type
	}
	require.Equal(t, map[string]string{"a bare == in zsh died": "shell-trap", "pkill matched the live daemon": ""}, types)
}
