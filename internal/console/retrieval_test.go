package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

func TestRetrievalPage_RatesAndLists(t *testing.T) {
	db, mgr, mux := newConsoleWithFiles(t)
	ctx := context.Background()
	rec := events.NewRecorder(db)

	m := writeMemory(t, mgr, core.KindGotcha, "seamless", "watcher-race", "a pitfall")

	// The panel measures reach: how many distinct memories are surfaced, across how
	// many sessions. One memory surfaced in two sessions -> 2 injections, 1 memory
	// reached, 2 sessions.
	_, err := rec.Record(ctx, core.Event{Kind: core.EventInjected, SessionID: "sessA", Payload: map[string]any{"item_ids": []any{m.ID}, "emitted_estimated_tokens": 120}})
	require.NoError(t, err)
	_, err = rec.Record(ctx, core.Event{Kind: core.EventInjected, SessionID: "sessB", Payload: map[string]any{"item_ids": []any{m.ID}, "emitted_estimated_tokens": 80}})
	require.NoError(t, err)

	var data retrievalData
	getJSON(t, mux, "/console/retrieval?format=json", &data)

	require.Equal(t, 2, data.Injections)
	require.Equal(t, 200, data.InjectedTokens)
	require.Equal(t, 1, data.MemoriesSurfaced)
	require.Equal(t, 2, data.SessionsReached)
	require.Len(t, data.ByKind, 1)
	require.Equal(t, "gotcha", data.ByKind[0].Kind)
	require.Equal(t, 2, data.ByKind[0].Injects)
	require.Equal(t, 1, data.ByKind[0].Memories)
	require.Len(t, data.ByProject, 1)
	require.Equal(t, "seamless", data.ByProject[0].Project)
	require.Equal(t, 1, data.ByProject[0].Surfaced)
	require.Equal(t, 1, data.ByProject[0].Active)
	require.Equal(t, 100, data.ByProject[0].ReachRate)
	require.Equal(t, 2, data.ByProject[0].Injects)
	require.Equal(t, 1, data.ByProject[0].Created, "the just-written memory is in-window churn")
	require.Equal(t, 1, data.CreatedInWindow)
	require.Zero(t, data.RetiredInWindow)

	// The churn annotation renders beside the denominator (hero + project row).
	body := getHTMLBody(t, mux, "/console/retrieval")
	require.Contains(t, body, "+1 new")
	// The tokens card carries the same judged pair as its two siblings.
	require.Contains(t, body, "~200")
	require.NotContains(t, body, "retrieval-scope-quiet", "the only scope has traffic, so no quiet strip")
	require.Len(t, data.TopInjected, 1)
	require.Equal(t, m.ID, data.TopInjected[0].ID)
	require.Equal(t, 2, data.TopInjected[0].Sessions)
	require.NotEmpty(t, data.Trend, "today's injections should appear in the trend")
	require.Equal(t, "24h", data.Window, "defaults to the 24h window")
}

// Loop health: demand rate counts briefed memories that were also pulled,
// waste share prices the ones that never are, the miss rate tracks prompts the
// knowledge base could not answer, and dead weight names the offenders.
func TestRetrievalPage_LoopHealth(t *testing.T) {
	db, mgr, mux := newConsoleWithFiles(t)
	ctx := context.Background()
	rec := events.NewRecorder(db)

	wanted := writeMemory(t, mgr, core.KindGotcha, "seamless", "wanted-memory", "gets pulled")
	noise := writeMemory(t, mgr, core.KindGotcha, "seamless", "noise-memory", "never pulled")

	// Both briefed (120 + 80 tokens); only wanted-memory earns demand.
	_, err := rec.Record(ctx, core.Event{Kind: core.EventInjected, SessionID: "sessA",
		Payload: map[string]any{"item_ids": []any{wanted.ID}, "hook": "session-start", "emitted_estimated_tokens": 120}})
	require.NoError(t, err)
	_, err = rec.Record(ctx, core.Event{Kind: core.EventInjected, SessionID: "sessA",
		Payload: map[string]any{"item_ids": []any{noise.ID}, "hook": "session-start", "emitted_estimated_tokens": 80}})
	require.NoError(t, err)
	_, err = rec.Record(ctx, core.Event{Kind: core.EventInjected, SessionID: "sessA",
		Payload: map[string]any{"item_ids": []any{wanted.ID}, "source": "recall"}})
	require.NoError(t, err)
	_, err = rec.Record(ctx, core.Event{Kind: core.EventHookPrompt, SessionID: "sessA",
		Payload: map[string]any{"prompt": "a question nothing matched", "matched": false}})
	require.NoError(t, err)

	var data retrievalData
	getJSON(t, mux, "/console/retrieval?format=json", &data)
	require.Equal(t, 2, data.BriefingSurfaced)
	require.Equal(t, 1, data.DemandedOfSurfaced)
	require.Equal(t, 50, data.DemandRate)
	require.Equal(t, 40, data.WasteShare, "80 of 200 injected tokens went to the undemanded memory")
	require.Equal(t, 1, data.RecallMisses)
	require.Zero(t, data.PromptMatches)
	require.Equal(t, 100, data.MissRate)
	require.Len(t, data.DeadWeight, 1)
	require.Equal(t, noise.ID, data.DeadWeight[0].ID)

	body := getHTMLBody(t, mux, "/console/retrieval")
	require.Contains(t, body, "Demand rate")
	require.Contains(t, body, "Waste share")
	require.Contains(t, body, "Dead weight")
	require.Contains(t, body, "noise-memory")

	// The memories library accepts the utility sort and, once the page-load
	// rebuild has run, the detail payload carries the score with its breakdown.
	var mems memoriesData
	getJSON(t, mux, "/console/memories?sort=utility&format=json", &mems)
	var detail memoryDetail
	getJSON(t, mux, "/console/memories/"+wanted.ID+"?format=json", &detail)
	require.Greater(t, detail.Utility, 0.0)
	require.NotNil(t, detail.UtilityParts)
	require.Greater(t, detail.UtilityParts.Recall, 0.0)
}

// Read-after-inject funnel: each injection surface reports what it pushed and
// the share agents pulled back, and the split renders inside the loop-health
// zone -- server-rendered, so window changes morph it in place like the rest of
// the page (no document reload).
func TestRetrievalPage_FunnelBySurface(t *testing.T) {
	db, mgr, mux := newConsoleWithFiles(t)
	ctx := context.Background()
	rec := events.NewRecorder(db)

	briefed := writeMemory(t, mgr, core.KindGotcha, "seamless", "briefed-only", "never pulled")
	pulled := writeMemory(t, mgr, core.KindGotcha, "seamless", "pulled-back", "read after inject")

	_, err := rec.Record(ctx, core.Event{Kind: core.EventInjected, SessionID: "sessA",
		Payload: map[string]any{"item_ids": []any{briefed.ID, pulled.ID}, "hook": "session-start"}})
	require.NoError(t, err)
	_, err = rec.Record(ctx, core.Event{Kind: core.EventInjected, SessionID: "sessB",
		Payload: map[string]any{"item_ids": []any{pulled.ID}, "hook": "subagent-start"}})
	require.NoError(t, err)
	_, err = rec.Record(ctx, core.Event{Kind: core.EventMemoryRead, SessionID: "sessB", ItemID: pulled.ID})
	require.NoError(t, err)

	var data retrievalData
	getJSON(t, mux, "/console/retrieval?format=json", &data)
	require.Len(t, data.FunnelBySurface, 2)

	ss := data.FunnelBySurface[0]
	require.Equal(t, "session-start", ss.Surface)
	require.Equal(t, 2, ss.Injections)
	require.Equal(t, 2, ss.Items)
	require.Equal(t, 1, ss.ItemsRead)
	require.Equal(t, 50, ss.ReadRate)

	sub := data.FunnelBySurface[1]
	require.Equal(t, "subagent-start", sub.Surface)
	require.Equal(t, 1, sub.Injections)
	require.Equal(t, 1, sub.Items)
	require.Equal(t, 1, sub.ItemsRead)
	require.Equal(t, 100, sub.ReadRate)
	require.Positive(t, data.FunnelFollowHours)

	body := getHTMLBody(t, mux, "/console/retrieval")
	require.Contains(t, body, "Funnel by surface")
	require.Contains(t, body, `class="retrieval-panel retrieval-funnel-panel"`)
	require.Contains(t, body, "session-start")
	require.Contains(t, body, "subagent-start")
	require.Contains(t, body, "1 of 2 pulled")
	require.Contains(t, body, "1 of 1 pulled")
}

// Scopes without any window activity collapse into the quiet strip instead of
// stacking as 0%-reach rows; scopes with traffic keep their full row. The flat
// ByProject list in the JSON payload is unchanged by the split.
func TestRetrievalPage_QuietScopesCollapse(t *testing.T) {
	db, mgr, mux := newConsoleWithFiles(t)
	ctx := context.Background()
	rec := events.NewRecorder(db)

	hot := writeMemory(t, mgr, core.KindGotcha, "alpha", "hot-memory", "gets injected")
	cold := writeMemory(t, mgr, core.KindGotcha, "beta", "cold-memory", "never injected")
	// Backdate the cold memory below the 24h window so beta carries no churn
	// annotation -- a scope is quiet only when the window did nothing to it.
	_, err := db.ExecContext(ctx, `UPDATE memories_index SET created_at = ? WHERE id = ?`,
		core.FormatTime(time.Now().UTC().Add(-48*time.Hour)), cold.ID)
	require.NoError(t, err)

	_, err = rec.Record(ctx, core.Event{Kind: core.EventInjected, SessionID: "sessA", Payload: map[string]any{"item_ids": []any{hot.ID}}})
	require.NoError(t, err)

	body := getHTMLBody(t, mux, "/console/retrieval")
	require.Contains(t, body, `class="retrieval-scope-quiet"`)
	require.Contains(t, body, "1 quiet scope")
	require.Contains(t, body, `title="0 of 1 active memories surfaced">beta<small>1</small></a>`)
	require.Contains(t, body, `href="/console/projects/alpha"`, "the touched scope keeps its full row")

	var data retrievalData
	getJSON(t, mux, "/console/retrieval?format=json", &data)
	require.Len(t, data.ByProject, 2, "the JSON contract stays flat")
}

func TestRetrievalPage_EmptyRenders(t *testing.T) {
	_, _, mux := newConsoleWithFiles(t)
	var data retrievalData
	getJSON(t, mux, "/console/retrieval?format=json", &data)
	require.Zero(t, data.Injections)
	// StaleMemories over an empty store is empty; ensure no error path.
	require.Empty(t, data.Stale)
	// The funnel keeps its stable two-surface shape even with no events.
	require.Len(t, data.FunnelBySurface, 2)
	require.Equal(t, "session-start", data.FunnelBySurface[0].Surface)
	require.Equal(t, "subagent-start", data.FunnelBySurface[1].Surface)
	_ = store.MemoryStat{}
}

// stConsole is a console over real files whose gardener staleness horizon is
// days (0 leaves the setting unset). hidden, when given, are projects the
// console's read handle does not see, as under peer_auth's locked projects.
func stConsole(t *testing.T, days int, hidden ...string) (*sql.DB, *files.Manager, *http.ServeMux) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "seam.db")
	db, err := storetest.OpenFresh(t, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	dataDir := filepath.Join(dir, "data")
	mgr, err := files.NewManager(dataDir, db, slog.Default())
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })
	read := db
	if len(hidden) > 0 {
		for _, slug := range hidden {
			_, err := store.EnsureProject(context.Background(), db, slug, slug)
			require.NoError(t, err)
		}
		read, err = store.OpenView(path, hidden)
		require.NoError(t, err)
		t.Cleanup(func() { _ = read.Close() })
	}
	svc, err := New(Config{
		DB: read, WriteDB: db, Files: mgr, Events: events.NewRecorder(db), DataDir: dataDir, APIKey: testKey,
		Level: "advanced", GardenerCfg: config.Gardener{StalenessDays: days},
	})
	require.NoError(t, err)
	mux := http.NewServeMux()
	svc.Register(mux)
	return db, mgr, mux
}

// stWrite writes a memory created and last updated at the given times.
func stWrite(t *testing.T, mgr *files.Manager, project, name string, created, updated time.Time) core.Memory {
	t.Helper()
	m, err := mgr.WriteMemory(context.Background(), core.Memory{
		ID: mustID(t), Kind: core.KindGotcha, Name: name, Description: "about " + name, Project: project,
		Body: "body of " + name, Created: created, Updated: updated, ValidFrom: created,
	})
	require.NoError(t, err)
	return m
}

// stRecord records an injection (read=false) or a read of m at at.
func stRecord(t *testing.T, db *sql.DB, m core.Memory, at time.Time, read bool) {
	t.Helper()
	ev := core.Event{Kind: core.EventInjected, TS: at, Payload: map[string]any{"item_ids": []any{m.ID}, "hook": "session-start"}}
	if read {
		ev = core.Event{Kind: core.EventMemoryRead, TS: at, ItemID: m.ID}
	}
	_, err := events.NewRecorder(db).Record(context.Background(), ev)
	require.NoError(t, err)
}

// stOverview is the part of the Overview's JSON answer about staleness.
type stOverview struct {
	StaleDays   int
	StaleUnseen int
	Attention   []attnCard
}

var stChipRe = regexp.MustCompile(`<span class="mv2-fact-hi"(?: data-tone="(\w+)")? title="[^"]*">([^<]*)<span>since surfaced</span>`)

// stChipTone reads the memory reader's "since surfaced" chip off its page.
func stChipTone(t *testing.T, mux *http.ServeMux, id string) (age, tone string) {
	t.Helper()
	m := stChipRe.FindStringSubmatch(getHTMLBody(t, mux, "/console/memories/"+id))
	require.NotNil(t, m, "the reader shows the since-surfaced chip")
	return m[2], m[1]
}

// "Stale" means one thing: the configured gardener.staleness_days. Retrieval's
// list and its label, the Overview's count and its labels, the knowledge sky's
// stale ring and the memory reader's chip all follow the setting, and the
// card counts exactly the rows the list shows.
func TestStStaleness_FollowsTheConfiguredHorizon(t *testing.T) {
	now := time.Now().UTC()
	daysAgo := func(n int) time.Time { return now.AddDate(0, 0, -n) }
	for _, tc := range []struct {
		days      int
		stale     []string
		chipTones map[string]string // memory name -> the reader chip's tone
	}{
		{days: 30, stale: []string{"quiet-60d", "quiet-150d", "never-200d", "updated-40d"},
			chipTones: map[string]string{"quiet-60d": "warn", "read-10d": "warn", "fresh": "ok"}},
		{days: 120, stale: []string{"quiet-150d", "never-200d"},
			chipTones: map[string]string{"quiet-60d": "", "read-10d": "", "fresh": "ok"}},
	} {
		t.Run(strconv.Itoa(tc.days)+"d", func(t *testing.T) {
			db, mgr, mux := stConsole(t, tc.days)
			old := daysAgo(300)
			byName := map[string]core.Memory{}
			add := func(name string, created, updated time.Time) core.Memory {
				m := stWrite(t, mgr, "example-ops", name, created, updated)
				byName[name] = m
				return m
			}
			stRecord(t, db, add("quiet-60d", old, old), daysAgo(60), false)
			stRecord(t, db, add("quiet-150d", old, old), daysAgo(150), false)
			add("never-200d", daysAgo(200), daysAgo(200))
			add("updated-40d", old, daysAgo(40))
			read := add("read-10d", old, old)
			stRecord(t, db, read, daysAgo(10), true)
			stRecord(t, db, read, daysAgo(100), false)
			stRecord(t, db, add("fresh", old, old), now.Add(-time.Hour), false)
			add("new", now.Add(-time.Hour), now.Add(-time.Hour))

			var ret retrievalData
			getJSON(t, mux, "/console/retrieval?format=json", &ret)
			require.Equal(t, tc.days, ret.StaleDays)
			var listed []string
			for _, row := range ret.Stale {
				listed = append(listed, row.Name)
			}
			require.ElementsMatch(t, tc.stale, listed, "Retrieval lists what has had no update, injection, or read in %dd", tc.days)
			page := getHTMLBody(t, mux, "/console/retrieval")
			require.Contains(t, page, fmt.Sprintf("stale = no update, injection, or read in %dd</span>", tc.days))

			var ov stOverview
			getJSON(t, mux, "/console/?format=json", &ov)
			require.Equal(t, tc.days, ov.StaleDays)
			require.Equal(t, len(tc.stale), ov.StaleUnseen, "the card counts the rows Retrieval lists")
			var card *attnCard
			for i := range ov.Attention {
				if ov.Attention[i].Icon == "timer" {
					card = &ov.Attention[i]
				}
			}
			require.NotNil(t, card)
			require.Equal(t, plural(len(tc.stale), "memory going stale", "memories going stale"), card.Title)
			require.Equal(t, fmt.Sprintf("no update, injection, or read in %dd · consider retiring", tc.days), card.Sub)

			home := getHTMLBody(t, mux, "/console/")
			require.Contains(t, home, fmt.Sprintf("<strong>%d</strong> going stale</span><small>no update, injection, or read in %d days</small>",
				len(tc.stale), tc.days), "the sky's readout")
			require.Contains(t, home, fmt.Sprintf("Over %d days ago", tc.days), "the sky's stale ring")

			for name, tone := range tc.chipTones {
				_, got := stChipTone(t, mux, byName[name].ID)
				require.Equal(t, tone, got, "the reader chip of %s at %dd", name, tc.days)
			}
		})
	}
}

// Unset, the horizon is the gardener's default, as the gardener's own is.
func TestStStaleness_UnsetFallsBackToTheDefault(t *testing.T) {
	_, _, mux := stConsole(t, 0)
	var ret retrievalData
	getJSON(t, mux, "/console/retrieval?format=json", &ret)
	require.Equal(t, config.Defaults().Gardener.StalenessDays, ret.StaleDays)
	var ov stOverview
	getJSON(t, mux, "/console/?format=json", &ov)
	require.Equal(t, config.Defaults().Gardener.StalenessDays, ov.StaleDays)
}

// A stale or dead-weight row links to its memory and, when the gardener
// already holds a pending proposal acting on that memory, to the proposal. An
// archive wins over another kind; a decided proposal links nothing.
func TestStRetrievalRows_LinkTheWaitingProposal(t *testing.T) {
	db, mgr, mux := stConsole(t, 30)
	ctx := context.Background()
	old := time.Now().UTC().AddDate(0, 0, -200)
	archived := stWrite(t, mgr, "example-ops", "archive-me", old, old)
	merged := stWrite(t, mgr, "example-ops", "merge-me", old, old)
	decided := stWrite(t, mgr, "example-ops", "decided-already", old, old)
	plain := stWrite(t, mgr, "", "nothing-waiting", old, old)
	keep := stWrite(t, mgr, "example-ops", "merge-keeper", time.Now().UTC(), time.Now().UTC())

	propose := func(kind string, payload map[string]any) store.Proposal {
		p, err := store.CreateProposal(ctx, db, kind, payload)
		require.NoError(t, err)
		return p
	}
	archive := propose(store.ProposalArchive, map[string]any{"id": archived.ID, "name": archived.Name, "project": "example-ops", "key": "archive:" + archived.ID})
	// A newer merge naming the same memory: the archive still wins.
	propose(store.ProposalMerge, map[string]any{"keep": map[string]any{"id": keep.ID}, "drop": map[string]any{"id": archived.ID}, "key": "merge:a"})
	merge := propose(store.ProposalMerge, map[string]any{"keep": map[string]any{"id": keep.ID}, "drop": map[string]any{"id": merged.ID}, "key": "merge:b"})
	gone := propose(store.ProposalArchive, map[string]any{"id": decided.ID, "key": "archive:" + decided.ID})
	require.NoError(t, store.ResolveProposal(ctx, db, gone.ID, store.ProposalDismissed, "owner", time.Now().UTC()))

	var ret retrievalData
	getJSON(t, mux, "/console/retrieval?format=json", &ret)
	got := map[string]*waitingProposal{}
	for _, row := range ret.Stale {
		got[row.Name] = row.Proposal
	}
	require.Len(t, got, 4)
	require.Equal(t, &waitingProposal{ID: archive.ID, Kind: store.ProposalArchive, Label: "Archive a memory", Href: "/console/gardener/" + archive.ID}, got["archive-me"])
	require.Equal(t, merge.ID, got["merge-me"].ID, "either side of a merge")
	require.Nil(t, got["decided-already"], "a dismissed proposal is not waiting")
	require.Nil(t, got[plain.Name])

	page := getHTMLBody(t, mux, "/console/retrieval")
	require.Contains(t, page, `href="/console/memories/`+archived.ID+`"`, "the row links to its memory")
	require.Contains(t, page, `<a class="retrieval-proposal" href="/console/gardener/`+archive.ID+`" title="Waiting in the gardener: Archive a memory">`)
	require.Contains(t, page, `href="/console/gardener/`+merge.ID+`"`)
	require.NotContains(t, page, `href="/console/gardener/`+gone.ID+`"`)
	require.Equal(t, 2, strings.Count(page, "proposal waiting</a>"))
}

// The rows behind a dead-weight panel link the same way.
func TestStRetrievalRows_DeadWeightLinksTheWaitingProposal(t *testing.T) {
	rows := memRows([]store.MemoryStat{{ID: "a", Name: "loud"}, {ID: "b", Name: "quiet"}},
		map[string]waitingProposal{"a": {ID: "p1", Kind: store.ProposalArchive, Href: "/console/gardener/p1"}})
	require.Equal(t, "p1", rows[0].Proposal.ID)
	require.Nil(t, rows[1].Proposal)
	raw, err := json.Marshal(rows)
	require.NoError(t, err)
	require.JSONEq(t, `[{"id":"a","name":"loud","kind":"","project":"","injects":0,"reads":0,"sessions":0,"updated":"0001-01-01T00:00:00Z",
		"proposal":{"id":"p1","kind":"archive","label":"","href":"/console/gardener/p1"}},
		{"id":"b","name":"quiet","kind":"","project":"","injects":0,"reads":0,"sessions":0,"updated":"0001-01-01T00:00:00Z"}]`,
		string(raw), "a row keeps the memory's keys, and adds the proposal only when one waits")
}

// Every memory a proposal acts on is found, by kind; a kind that acts on no
// existing memory names none.
func TestStProposalMemoryIDs_ByKind(t *testing.T) {
	for _, tc := range []struct {
		kind    string
		payload map[string]any
		want    []string
	}{
		{store.ProposalArchive, map[string]any{"id": "m1"}, []string{"m1"}},
		{store.ProposalRekind, map[string]any{"id": "m1"}, []string{"m1"}},
		{store.ProposalReproject, map[string]any{"id": "m1", "to": "x"}, []string{"m1"}},
		{store.ProposalRelocate, map[string]any{"id": "m1", "to": "x"}, []string{"m1"}},
		{store.ProposalMerge, map[string]any{"keep": map[string]any{"id": "k"}, "drop": map[string]any{"id": "d"}}, []string{"k", "d"}},
		{store.ProposalConsolidate, map[string]any{"sources": []any{map[string]any{"id": "s1"}, map[string]any{"id": "s2"}}}, []string{"s1", "s2"}},
		{store.ProposalDigest, map[string]any{"project": "x"}, nil},
		{store.ProposalShipPlan, map[string]any{"id": "note-1"}, nil},
		{store.ProposalArchive, map[string]any{}, nil},
	} {
		require.Equal(t, tc.want, proposalMemoryIDs(store.Proposal{Kind: tc.kind, Payload: tc.payload}), tc.kind)
	}
}

// Under a lock, a locked project's stale memories are absent from Retrieval's
// list and from the Overview's count, read through the request's handle, and
// a proposal naming one of them links nothing.
func TestStStaleness_LockedProjectIsAbsent(t *testing.T) {
	const locked = "example-finance"
	db, mgr, mux := stConsole(t, 30, locked)
	ctx := context.Background()
	old := time.Now().UTC().AddDate(0, 0, -200)
	hidden := stWrite(t, mgr, locked, "ledger-quiet", old, old)
	shown := stWrite(t, mgr, "example-ops", "ops-quiet", old, old)
	p, err := store.CreateProposal(ctx, db, store.ProposalArchive, map[string]any{"id": hidden.ID, "project": locked, "key": "archive:" + hidden.ID})
	require.NoError(t, err)

	var ret retrievalData
	getJSON(t, mux, "/console/retrieval?format=json", &ret)
	require.Len(t, ret.Stale, 1)
	require.Equal(t, shown.ID, ret.Stale[0].ID)
	var ov stOverview
	getJSON(t, mux, "/console/?format=json", &ov)
	require.Equal(t, 1, ov.StaleUnseen)
	for _, path := range []string{"/console/retrieval", "/console/retrieval?format=json", "/console/", "/console/?format=json"} {
		body := getHTMLBody(t, mux, path)
		for _, absent := range []string{hidden.ID, hidden.Name, p.ID} {
			require.NotContains(t, body, absent, "%s names the locked project's %s", path, absent)
		}
	}

	all, err := store.CountStaleMemories(ctx, db, staleCutoff(time.Now(), 30))
	require.NoError(t, err)
	require.Equal(t, 2, all, "the unfiltered store holds both: the absence is the view's")
}
