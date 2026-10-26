package console

import (
	"context"
	"database/sql"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

func TestOverview_IsAStatusPageNotAPitch(t *testing.T) {
	_, mux := newConsole(t)
	page := getPeek(t, mux, "/console/?w=7d")
	require.Equal(t, http.StatusOK, page.Code)
	body := page.Body.String()

	require.Contains(t, body, `class="overview-page" data-overview`)
	// The SSE refresher toggles `.topbar .live`; the titlebar carries both.
	require.Contains(t, body, `class="ov2-titlebar topbar"`)
	require.Contains(t, body, `class="live ov2-title-live"`)
	require.Contains(t, body, `class="active" aria-current="page" href="?w=7d"`)
	require.Contains(t, body, "sessions observed")

	// The landing-page furniture is gone: no pitch, no calls to action.
	require.NotContains(t, body, "Shared context,")
	require.NotContains(t, body, "alive between agents")
	require.NotContains(t, body, `class="btn primary"`)
	require.NotContains(t, body, "overview-stage-index")

	// Every screen the page summarizes stays one click away.
	for _, href := range []string{
		`href="/console/projects"`,
		`href="/console/interactions"`,
	} {
		require.Contains(t, body, href)
	}

	// The four vitals are labeled and judged.
	for _, label := range []string{"Memory reach", "Knowledge continuity", "Context injections", "Sessions reached"} {
		require.Contains(t, body, label)
	}

	// Each vital is a drill-down link into the screen that explains its number,
	// carrying the selected window: reach and sessions-reached to Retrieval's
	// hero, injections to its delivery funnel, continuity to the Sessions list
	// filtered to the sessions that retained nothing.
	for _, href := range []string{
		`<a class="ov2-vital" href="/console/retrieval?w=7d" title="Reach detail in Retrieval">`,
		`<a class="ov2-vital" href="/console/retrieval?w=7d#retrieval-delivery-title"`,
		`<a class="ov2-vital" href="/console/sessions?w=7d&amp;retained=no"`,
		`<a class="ov2-vital" href="/console/retrieval?w=7d" title="Sessions reached in Retrieval">`,
	} {
		require.Contains(t, body, href)
	}
}

func TestOverview_EmptyDatabaseRendersNoHollowPanels(t *testing.T) {
	_, mux := newConsole(t)
	page := getPeek(t, mux, "/console/")
	require.Equal(t, http.StatusOK, page.Code)
	body := page.Body.String()

	require.NotContains(t, body, `class="ov2-attn"`, "nothing needs attention, so the strip does not render")
	require.NotContains(t, body, `class="ov2-live"`, "no session is live, so the strip does not render")
	require.NotContains(t, body, `class="delta"`, "no prior window and no data means no comparison chip")
	require.NotContains(t, body, `class="ov2-spark"`, "a metric with no series draws no sparkline")
	// The vitals themselves stay, showing an em dash rather than a confident 0.
	require.Contains(t, body, `class="ov2-vitals"`)
	require.Contains(t, body, "&mdash;")
	require.Contains(t, body, "no active memories to reach yet")
	// Empty-state cards keep their drill-down link: the destination explains
	// why there is nothing, which a dead card cannot.
	require.Contains(t, body, `<a class="ov2-vital" href="/console/retrieval?w=24h"`)
}

func TestOverview_AttentionStripOrdersBySeverityAndLinksOut(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()

	sessID, err := core.NewID()
	require.NoError(t, err)
	require.NoError(t, store.CreateSession(ctx, db, core.Session{
		ID: sessID, Name: "cc/mishapsess", ProjectSlug: "seamless",
		Status: core.SessionActive, Ambient: true, Model: "claude-fable-5",
		CreatedAt: now, UpdatedAt: now,
	}))
	evID, err := events.NewRecorder(db).Record(ctx, core.Event{
		Kind: core.EventAgentMishap, SessionID: sessID, ProjectSlug: "seamless",
		Payload: map[string]any{"description": "ran gofmt -w . across other agents' worktrees"},
	})
	require.NoError(t, err)
	_, err = store.CreateProposal(ctx, db, store.ProposalArchive, map[string]any{"key": "k1"})
	require.NoError(t, err)

	page := getPeek(t, mux, "/console/")
	require.Equal(t, http.StatusOK, page.Code)
	body := page.Body.String()

	require.Contains(t, body, `class="ov2-attn"`)
	require.Contains(t, body, "1 agent-reported mishap")
	require.Contains(t, body, `href="/console/events/`+evID+`"`)
	require.Contains(t, body, "1 gardener proposal waiting")
	require.Contains(t, body, `href="/console/gardener"`)

	// Severity order: danger before warn, always.
	dangerAt := strings.Index(body, `data-sev="danger"`)
	warnAt := strings.Index(body, `data-sev="warn"`)
	require.NotEqual(t, -1, dangerAt)
	require.NotEqual(t, -1, warnAt)
	require.Less(t, dangerAt, warnAt)

	// The strip is one scroll-snap rail with chevron paging, not a wrapping
	// grid: the finish-line bucket is unbounded, and a second row would push
	// the vitals below the fold. The chevrons ship hidden -- the page script
	// reveals them only when the rail overflows.
	require.Contains(t, body, `class="ov2-attn-rail" data-attn-rail`)
	require.Contains(t, body, `class="ov2-attn-nav" hidden`)
	require.Contains(t, body, `data-attn-prev`)
	require.Contains(t, body, `data-attn-next`)
	require.Contains(t, body, "Needs attention<strong>2</strong>")
}

func TestOverview_LiveStripListsHeartbeatingSessions(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()

	live, err := core.NewID()
	require.NoError(t, err)
	require.NoError(t, store.CreateSession(ctx, db, core.Session{
		ID: live, Name: "cc/working-now", ProjectSlug: "seamless",
		Status: core.SessionActive, CreatedAt: now, UpdatedAt: now,
	}))
	// Active on paper but long past the idle threshold: awaiting the reaper, not live.
	idle, err := core.NewID()
	require.NoError(t, err)
	stale := now.Add(-6 * time.Hour)
	require.NoError(t, store.CreateSession(ctx, db, core.Session{
		ID: idle, Name: "cc/gone-quiet", ProjectSlug: "seamless",
		Status: core.SessionActive, CreatedAt: stale, UpdatedAt: stale,
	}))

	page := getPeek(t, mux, "/console/")
	require.Equal(t, http.StatusOK, page.Code)
	body := page.Body.String()

	require.Contains(t, body, `class="ov2-live"`)
	require.Contains(t, body, "cc/working-now")
	require.NotContains(t, body, "cc/gone-quiet", "an idle session is not live")
}

func TestOverview_LedgerRowsCarryTheSharedSeverityCode(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	rec := events.NewRecorder(db)

	_, err := rec.Record(ctx, core.Event{Kind: core.EventMemoryWritten, ProjectSlug: "seamless",
		Payload: map[string]any{"name": "outbox-writes-same-tx"}})
	require.NoError(t, err)
	_, err = rec.Record(ctx, core.Event{Kind: core.EventGardenerAction, ProjectSlug: "seamless",
		Payload: map[string]any{"action": "proposed", "kind": "merge"}})
	require.NoError(t, err)

	page := getPeek(t, mux, "/console/")
	require.Equal(t, http.StatusOK, page.Code)
	body := page.Body.String()

	require.Contains(t, body, `class="ov2-evt" data-sev="write"`)
	require.Contains(t, body, `class="ov2-evt" data-sev="system"`)
	require.Contains(t, body, "wrote memory outbox-writes-same-tx")
}

func TestOverview_StylesStayScopedAndStackResponsively(t *testing.T) {
	css := string(consoleCSS)
	at := strings.Index(css, "OVERVIEW -- STATUS PAGE")
	require.NotEqual(t, -1, at)
	block := css[at:]
	for _, rule := range []string{
		".ov2-titlebar {", ".ov2-attn {", ".ov2-attn-rail {", ".ov2-attn-btn {",
		".ov2-vitals {", ".ov2-live {",
		".ov2-grid {", ".ov2-panel {", ".ov2-ledger {",
	} {
		require.Contains(t, block, rule)
	}
	stackAt := strings.Index(block, "@media (max-width: 1100px)")
	require.NotEqual(t, -1, stackAt)
	// One column, floored at zero rather than at min-content: a bare 1fr let the
	// workspaces table widen the whole phone page past the screen.
	require.Contains(t, block[stackAt:], ".ov2-grid { grid-template-columns: minmax(0, 1fr); }")

	// The marketing block is gone, not merely unused.
	require.NotContains(t, css, "OVERVIEW FRONT DOOR")
	require.NotContains(t, css, ".overview-hero.topbar")
	require.NotContains(t, css, ".overview-atlas-node")
}

// newHealthConsole builds a console that knows its version and embedder, at
// the given level, over a fresh DB.
func newHealthConsole(t *testing.T, lvl string, emb EmbeddingRuntime) (*sql.DB, *http.ServeMux) {
	t.Helper()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	svc, err := New(Config{
		DB: db, Events: events.NewRecorder(db), APIKey: testKey,
		Level: lvl, Version: "0.5.2+abc1234", Embedding: emb,
	})
	require.NoError(t, err)
	mux := http.NewServeMux()
	svc.Register(mux)
	return db, mux
}

// The Basic Home answers "is it working?" first and carries nothing analytic:
// health strip, check-in, attention, live, sky, activity -- and nothing else.
func TestOverview_BasicHomeIsHealthFirst(t *testing.T) {
	db, mux := newHealthConsole(t, "basic", EmbeddingRuntime{Enabled: true, Provider: "openai", Model: "text-embedding-3-large"})
	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, store.CreateSession(ctx, db, core.Session{
		ID: mustID(t), Name: "cc/live1", ExternalClient: "claude-code", Status: core.SessionActive,
		CreatedAt: now.Add(-4 * time.Minute), UpdatedAt: now.Add(-4 * time.Minute),
	}))
	require.NoError(t, store.CreateSession(ctx, db, core.Session{
		ID: mustID(t), Name: "cx/old1", ExternalClient: "codex", Status: core.SessionCompleted,
		CreatedAt: now.Add(-72 * time.Hour), UpdatedAt: now.Add(-72 * time.Hour),
	}))
	rec := events.NewRecorder(db)
	evID, err := rec.Record(ctx, core.Event{Kind: core.EventInjected, Payload: map[string]any{"hook": "session-start"}})
	require.NoError(t, err)

	body := getPeek(t, mux, "/console/").Body.String()
	require.Contains(t, body, `<section class="ov-health lead" aria-label="Is it working">`)
	require.Contains(t, body, "Working with Claude Code (last active 4m ago) and Codex (3d ago)")
	require.Contains(t, body, `href="/console/sessions"`)
	require.Contains(t, body, "semantic recall on")
	require.Contains(t, body, `href="/console/events/`+evID+`"`)
	require.Contains(t, body, "last briefing delivered")
	require.Contains(t, body, "version 0.5.2&#43;abc1234", "html/template escapes the build suffix plus")
	require.Contains(t, body, `id="checkin"`)
	require.Contains(t, body, `aria-label="Recent activity"`)
	require.Contains(t, body, `aria-label="Sessions live now"`)

	for _, gone := range []string{
		`class="ov2-vitals"`, `class="ov2-window-tabs"`, "sessions observed",
		`aria-label="Recently active workspaces"`, `class="ov2-rail"`, "Most reused knowledge",
		`class="ov-health quiet"`,
	} {
		require.NotContains(t, body, gone, "basic Home hides %s", gone)
	}
	// The strip leads: it comes before the check-in.
	require.Less(t, strings.Index(body, "ov-health lead"), strings.Index(body, `id="checkin"`))
}

// Above Basic the page is today's Overview, with the health facts as a quiet
// line closing the vitals.
func TestOverview_AdvancedKeepsTheAnalyticsAndAQuietHealthLine(t *testing.T) {
	_, mux := newHealthConsole(t, "advanced", EmbeddingRuntime{Provider: "openai", Reason: "api_key is empty"})
	body := getPeek(t, mux, "/console/?w=7d").Body.String()
	for _, want := range []string{
		`class="ov2-vitals"`, `class="ov2-window-tabs"`, "sessions observed",
		`aria-label="Recently active workspaces"`, `class="ov2-rail"`, "Most reused knowledge",
		`<footer class="ov-health quiet" aria-label="Is it working">`,
	} {
		require.Contains(t, body, want)
	}
	require.NotContains(t, body, "ov-health lead")
	require.Contains(t, body, "semantic recall off")
	require.Contains(t, body, `title="Recall is keyword-only: api_key is empty"`)
	require.Less(t, strings.Index(body, `class="ov2-vitals"`), strings.Index(body, "ov-health quiet"))
}

// An unknown fact is omitted, never printed as a zero: a fresh install with no
// clients, no briefing, no version, and no embedder report shows no strip.
func TestOverview_HealthStripOmitsUnknownFacts(t *testing.T) {
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	svc, err := New(Config{DB: db, APIKey: testKey})
	require.NoError(t, err)
	mux := http.NewServeMux()
	svc.Register(mux)

	body := getPeek(t, mux, "/console/").Body.String()
	require.NotContains(t, body, "ov-health")
	require.NotContains(t, body, "Working with")
	require.NotContains(t, body, "last briefing")
	require.NotContains(t, body, "version ")
}
