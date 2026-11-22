package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// briefingValues renders knobs the way the Briefing form submits them -- every
// field, from the same field list the parser reads -- so a test posts exactly
// what the page would.
func briefingValues(b config.Briefing) url.Values {
	v := url.Values{}
	for _, f := range briefingIntFields {
		v.Set(f.name, strconv.Itoa(*f.dst(&b)))
	}
	if b.IncludeParentMemories {
		v.Set("include_parent_memories", "1")
	}
	if b.IncludeSiblingMemories {
		v.Set("include_sibling_memories", "1")
	}
	v.Set("utility_mode", b.UtilityMode)
	v.Set("utility_weight", strconv.FormatFloat(b.UtilityWeight, 'f', -1, 64))
	return v
}

// seedPreviewProject registers project "orbital" with three index memories,
// newest first: enough for the memory_max_items knob to show on screen.
func seedPreviewProject(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	_, err := store.EnsureProject(ctx, db, "orbital", "Orbital")
	require.NoError(t, err)
	now := time.Now().UTC()
	for i, name := range []string{"newest-runbook", "middle-gotcha", "oldest-gotcha"} {
		stamp := core.FormatTime(now.Add(-time.Duration(i+1) * time.Hour))
		_, err := db.ExecContext(ctx, `
			INSERT INTO memories_index
			    (id, kind, name, description, project, file_path, tags, valid_from,
			     invalid_at, superseded_by, source_session, content_hash, created_at, updated_at)
			VALUES (?, 'gotcha', ?, 'what it says', 'orbital', ?, '[]', ?, NULL, NULL, '', 'h', ?, ?)`,
			"01PREVIEW"+strconv.Itoa(i), name, "memory/orbital/"+name+".md", stamp, stamp, stamp)
		require.NoError(t, err)
	}
}

func countRows(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM "+table).Scan(&n))
	return n
}

// A POST previews the form's values -- saved or not -- for the chosen project,
// as the panel's fragment: the meter against the budget, and the briefing.
func TestBriefingPreview_RendersTheFormKnobs(t *testing.T) {
	db, mux := newConsole(t)
	seedPreviewProject(t, db)

	form := briefingValues(config.Defaults().Briefing)
	form.Set("project", "orbital")
	rr := postForm(mux, "/console/settings/briefing/preview", form.Encode())
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	body := rr.Body.String()
	require.NotContains(t, body, "<html", "the panel gets a fragment, not a page")
	require.Contains(t, body, `class="brief-preview-text"`)
	require.Contains(t, body, "Seam project: orbital")
	require.Contains(t, body, "middle-gotcha")
	require.Contains(t, body, "of 1,500 tokens", "the meter reads against the configured budget")
	require.Contains(t, body, "hard cap 3,000")
	require.Equal(t, "no-store", rr.Header().Get("Cache-Control"))

	// An unsaved knob shows at once: capping the index is visible in the text.
	form.Set("memory_max_items", "1")
	body = postForm(mux, "/console/settings/briefing/preview", form.Encode()).Body.String()
	require.Contains(t, body, "newest-runbook")
	require.NotContains(t, body, "middle-gotcha")
	require.Contains(t, body, "&#43;2 older", "html/template writes + as &#43;")
}

// A preview is a read. Neither method writes the override row or records an
// event -- an injection event in particular would credit exposure no agent had
// (constraint closed-loop-utility-signal-contract).
func TestBriefingPreview_WritesNothing(t *testing.T) {
	db, mux := newConsole(t)
	seedPreviewProject(t, db)
	events, settings := countRows(t, db, "events"), countRows(t, db, "settings")

	form := briefingValues(config.Defaults().Briefing)
	form.Set("project", "orbital")
	form.Set("findings_count", "9")
	require.Equal(t, http.StatusOK, postForm(mux, "/console/settings/briefing/preview", form.Encode()).Code)
	require.Equal(t, http.StatusOK, getPeek(t, mux, "/console/settings/briefing/preview?project=orbital").Code)
	require.Equal(t, http.StatusOK, getPeek(t, mux, "/console/settings/briefing/preview?project=orbital&format=json").Code)

	require.Equal(t, events, countRows(t, db, "events"), "a preview records no event")
	require.Equal(t, settings, countRows(t, db, "settings"), "a preview stores no row")
	_, overridden, err := store.BriefingConfig(context.Background(), db, config.Defaults().Briefing)
	require.NoError(t, err)
	require.False(t, overridden, "the previewed knobs were not saved")
}

// Knobs the save would refuse, the preview refuses with the same message: one
// parse and one validation serve both.
func TestBriefingPreview_RefusesWhatTheSaveRefuses(t *testing.T) {
	db, mux := newConsole(t)
	seedPreviewProject(t, db)

	for name, value := range map[string]string{"utility_weight": "1.5", "findings_count": "many", "utility_mode": "sometimes"} {
		form := briefingValues(config.Defaults().Briefing)
		form.Set(name, value)

		save := postForm(mux, "/console/settings/briefing", form.Encode())
		require.Equal(t, http.StatusSeeOther, save.Code)
		target, err := url.Parse(save.Header().Get("Location"))
		require.NoError(t, err)
		flash := target.Query().Get("error")
		require.NotEmpty(t, flash, "the save refuses %s=%s", name, value)

		form.Set("project", "orbital")
		rr := postForm(mux, "/console/settings/briefing/preview", form.Encode())
		require.Equal(t, http.StatusBadRequest, rr.Code, name)
		require.Contains(t, rr.Body.String(), `role="alert"`)
		require.Contains(t, rr.Body.String(), htmlEscaped(flash), "the preview shows the save's own message")

		var got map[string]string
		rr = postForm(mux, "/console/settings/briefing/preview?format=json", form.Encode())
		require.Equal(t, http.StatusBadRequest, rr.Code)
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
		require.Equal(t, flash, got["error"])
	}
}

// htmlEscaped is a message as html/template writes it into text.
func htmlEscaped(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&#34;", "'", "&#39;", "+", "&#43;").Replace(s)
}

// The project must be given exactly once and name a live, registered project;
// every refusal names the valid values rather than falling back to one.
func TestBriefingPreview_ProjectBoundary(t *testing.T) {
	db, mux := newConsole(t)
	get := func(query string) (int, string) {
		var got map[string]string
		rr := getPeek(t, mux, "/console/settings/briefing/preview?format=json"+query)
		_ = json.Unmarshal(rr.Body.Bytes(), &got)
		return rr.Code, got["error"]
	}

	code, msg := get("&project=orbital")
	require.Equal(t, http.StatusBadRequest, code)
	require.Contains(t, msg, "no projects are registered yet")

	seedPreviewProject(t, db)
	_, err := store.EnsureProject(context.Background(), db, "homelab", "Homelab")
	require.NoError(t, err)
	_, err = store.EnsureProject(context.Background(), db, "old-split", "Old split")
	require.NoError(t, err)
	require.NoError(t, store.RetireProject(context.Background(), db, "old-split", time.Now()))

	code, msg = get("")
	require.Equal(t, http.StatusBadRequest, code)
	require.Equal(t, "project is required: registered projects are homelab, orbital", msg)
	code, msg = get("&project=nowhere")
	require.Equal(t, http.StatusBadRequest, code)
	require.Equal(t, `unknown project "nowhere": registered projects are homelab, orbital`, msg)
	code, msg = get("&project=old-split")
	require.Equal(t, http.StatusBadRequest, code, "a retired project briefs no one")
	require.Contains(t, msg, "registered projects are homelab, orbital")
	code, msg = get("&project=orbital&project=homelab")
	require.Equal(t, http.StatusBadRequest, code)
	require.Equal(t, "project must be given exactly once", msg)
	code, _ = get("&project=orbital")
	require.Equal(t, http.StatusOK, code)
}

// GET previews the saved knobs -- what the next session will get -- and says
// so; the JSON answer carries the numbers the meter draws.
func TestBriefingPreview_GETUsesTheSavedKnobs(t *testing.T) {
	db, mux := newConsole(t)
	seedPreviewProject(t, db)
	saved := config.Defaults().Briefing
	saved.MemoryMaxItems = 1
	require.NoError(t, store.SetBriefingConfig(context.Background(), db, saved))

	var got briefingPreview
	getJSON(t, mux, "/console/settings/briefing/preview?project=orbital&format=json", &got)
	require.Equal(t, "orbital", got.Project)
	require.Equal(t, previewKnobsSaved, got.Knobs)
	require.Contains(t, got.Text, "newest-runbook")
	require.NotContains(t, got.Text, "middle-gotcha", "the saved cap applies")
	require.Equal(t, 1500, got.Budget)
	require.Equal(t, 3000, got.HardCap)
	require.Positive(t, got.Tokens)

	form := briefingValues(config.Defaults().Briefing)
	form.Set("project", "orbital")
	rr := postForm(mux, "/console/settings/briefing/preview?format=json", form.Encode())
	require.Equal(t, http.StatusOK, rr.Code)
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	require.Equal(t, previewKnobsForm, got.Knobs)
	require.Contains(t, got.Text, "middle-gotcha", "the form's uncapped knobs win over the saved row")
}

// The Briefing section carries the panel: its project picker belongs to a
// never-submitted form (so a save never carries it) and opens on the project
// the owner's agents worked in last.
func TestSettingsBriefing_PreviewPanel(t *testing.T) {
	db, mux := newConsole(t)
	page := getPeek(t, mux, "/console/settings?s=briefing").Body.String()
	// Markup, not bare attribute names: the page's script names the same
	// selectors whether or not the elements exist.
	require.Contains(t, page, `<aside class="brief-preview" data-brief-preview`)
	require.Contains(t, page, "No projects yet.")
	require.NotContains(t, page, `name="project" form="briefing-preview-picker"`)

	seedPreviewProject(t, db)
	_, err := store.EnsureProject(context.Background(), db, "homelab", "Homelab")
	require.NoError(t, err)
	page = getPeek(t, mux, "/console/settings?s=briefing").Body.String()
	require.Contains(t, page, `<form id="briefing-preview-picker" hidden></form>`)
	require.Contains(t, page, `<select id="brief-preview-project" name="project" form="briefing-preview-picker" data-brief-preview-project>`)
	require.Contains(t, page, `<label for="brief-preview-project">A new session in</label>`)
	require.Contains(t, page, `<option value="homelab" selected>`, "no session yet: the first project")

	now := time.Now().UTC()
	require.NoError(t, store.CreateSession(context.Background(), db, core.Session{
		ID: mustID(t), Name: "cc/preview", ProjectSlug: "orbital", Status: core.SessionActive,
		CreatedAt: now, UpdatedAt: now,
	}))
	page = getPeek(t, mux, "/console/settings?s=briefing").Body.String()
	require.Contains(t, page, `<option value="orbital" selected>`, "the project worked in last")
	require.NotContains(t, page, `<option value="homelab" selected>`)

	other := getPeek(t, mux, "/console/settings?s=features").Body.String()
	require.NotContains(t, other, `<aside class="brief-preview"`, "only the Briefing section carries it")
}

// The meter says where a briefing stands against its numbers: under budget is
// plain, over it is flagged (only pinned sections can put it there), and past
// the hard cap it says agents get it truncated.
func TestBriefingPreview_MeterStates(t *testing.T) {
	svc, err := New(Config{APIKey: testKey})
	require.NoError(t, err)
	render := func(p briefingPreview) string {
		var buf strings.Builder
		require.NoError(t, svc.pages["settings"].ExecuteTemplate(&buf, "briefing-preview", p))
		return buf.String()
	}
	base := briefingPreview{Project: "orbital", Text: "<seam-briefing>\n</seam-briefing>", Budget: 1500, HardCap: 3000}

	under := base
	under.Tokens = 750
	html := render(under)
	require.Contains(t, html, "<strong>750</strong> of 1,500 tokens")
	require.Contains(t, html, `style="width:50%"`)
	require.NotContains(t, html, "brief-preview-flag")

	over := base
	over.Tokens = 1740
	html = render(over)
	require.Contains(t, html, `class="brief-preview-meter is-over"`)
	require.Contains(t, html, `style="width:100%"`, "the bar fills, never overflows")
	require.Contains(t, html, "Over the budget")

	capped := base
	capped.Tokens = 3001
	html = render(capped)
	require.Contains(t, html, "is-over is-capped")
	require.Contains(t, html, "agents in orbital would get this briefing truncated")
	require.NotContains(t, html, "Over the budget", "one flag: the cap says more")

	empty := base
	empty.Text = ""
	require.Contains(t, render(empty), "a new session there starts without a briefing")
}

func TestGroupDigits(t *testing.T) {
	for n, want := range map[int]string{0: "0", 999: "999", 1000: "1,000", 1500: "1,500", 123456: "123,456", 1234567: "1,234,567"} {
		require.Equal(t, want, groupDigits(n), n)
	}
}
