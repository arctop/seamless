package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// mishapAt writes an agent.mishap row with a chosen id, session and time.
func mishapAt(t *testing.T, db *sql.DB, id, session, project, payload string, ts time.Time) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO events (id, ts, kind, session_id, project_slug, item_id, payload)
		VALUES (?, ?, ?, ?, ?, '', ?)`,
		id, core.FormatTime(ts), string(core.EventAgentMishap), session, project, payload)
	require.NoError(t, err)
}

// mishapFleet seeds the listing's fixture: typed reports, reports from before
// types (a description alone, and one with item_ids), a prefix-shaped old
// report with no recorded type, a type outside the vocabulary, degenerate
// payloads, another kind's event, and a report outside a 7-day window.
func mishapFleet(t *testing.T, db *sql.DB, now time.Time) {
	t.Helper()
	h := func(n int) time.Time { return now.Add(-time.Duration(n) * time.Hour) }
	mishapAt(t, db, "01MS0000000000000000000001", "S1", "demo", `{"description":"shell-trap: a bare == died","type":"shell-trap","item_ids":["MEM1"]}`, h(1))
	mishapAt(t, db, "01MS0000000000000000000002", "S1", "demo", `{"description":"shell-trap: unquoted var","type":"shell-trap","item_ids":["MEM1","MEM1"]}`, h(2))
	mishapAt(t, db, "01MS0000000000000000000003", "S2", "demo", `{"description":"wrong target: the main clone","type":"wrong-target","item_ids":["MEM2"]}`, h(3))
	mishapAt(t, db, "01MS0000000000000000000004", "S2", "", `{"description":"live-state: restarted the daemon","type":"live-state"}`, h(4))
	// Old reports: only a description; a description with item_ids; one
	// whose text opens like a prefix but carries no recorded type.
	mishapAt(t, db, "01MS0000000000000000000005", "S3", "demo", `{"description":"pkill matched the live daemon"}`, h(5))
	mishapAt(t, db, "01MS0000000000000000000006", "S3", "demo", `{"description":"violated chroma-boot-race","item_ids":["MEM1"]}`, h(6))
	mishapAt(t, db, "01MS0000000000000000000007", "S3", "other", `{"description":"other: written before types"}`, h(7))
	// A type outside the vocabulary and a non-string one read as untyped.
	mishapAt(t, db, "01MS0000000000000000000008", "S4", "other", `{"description":"odd","type":"not-a-type"}`, h(8))
	mishapAt(t, db, "01MS0000000000000000000009", "S4", "other", `{"description":"odder","type":7,"item_ids":"MEM3"}`, h(9))
	mishapAt(t, db, "01MS0000000000000000000010", "S4", "other", `{}`, h(10))
	mishapAt(t, db, "01MS0000000000000000000011", "S4", "other", `not json`, h(11))
	// Outside a 7-day window.
	mishapAt(t, db, "01MS0000000000000000000012", "S5", "demo", `{"description":"fabricated: a made-up flag","type":"fabricated","item_ids":["MEM2"]}`, now.AddDate(0, 0, -8))
	// Another kind is never a mishap.
	_, err := db.ExecContext(context.Background(), `INSERT INTO events (id, ts, kind, session_id, project_slug, item_id, payload)
		VALUES ('01EV0000000000000000000001', ?, 'session.ended', 'S1', 'demo', '', '{"type":"shell-trap","item_ids":["MEM1"]}')`,
		core.FormatTime(h(1)))
	require.NoError(t, err)
}

func mishapIDs(rs []MishapReport) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.ID)
	}
	return out
}

func TestListMishaps_FiltersAndOrder(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	mishapFleet(t, db, now)
	id := func(n string) string { return "01MS00000000000000000000" + n }

	all, err := ListMishaps(ctx, db, MishapFilter{})
	require.NoError(t, err)
	require.Equal(t, []string{id("01"), id("02"), id("03"), id("04"), id("05"), id("06"), id("07"), id("08"), id("09"),
		id("10"), id("11"), id("12")}, mishapIDs(all), "every report, newest first, and no other kind")

	// Old reports read with no type and their text unchanged.
	byID := map[string]MishapReport{}
	for _, r := range all {
		byID[r.ID] = r
	}
	require.Equal(t, MishapReport{ID: id("05"), At: now.Add(-5 * time.Hour), SessionID: "S3", Project: "demo",
		Description: "pkill matched the live daemon"}, byID[id("05")])
	require.Equal(t, MishapReport{ID: id("06"), At: now.Add(-6 * time.Hour), SessionID: "S3", Project: "demo",
		Description: "violated chroma-boot-race", ItemIDs: []string{"MEM1"}}, byID[id("06")])
	require.Equal(t, core.MishapType(""), byID[id("07")].Type, "a prefix in the text is not a recorded type")
	require.Equal(t, "other: written before types", byID[id("07")].Description)
	require.Equal(t, core.MishapType(""), byID[id("08")].Type, "a type outside the vocabulary reads as none")
	require.Equal(t, core.MishapType(""), byID[id("09")].Type)
	require.Empty(t, byID[id("09")].ItemIDs, "item_ids that is no list names nothing")
	require.Equal(t, MishapReport{ID: id("11"), At: now.Add(-11 * time.Hour), SessionID: "S4", Project: "other"}, byID[id("11")],
		"an unreadable payload keeps its row, with no text")
	require.Equal(t, core.MishapShellTrap, byID[id("01")].Type)
	require.Equal(t, "shell-trap: a bare == died", byID[id("01")].Description, "the stored description keeps its prefix")

	for _, tc := range []struct {
		name string
		f    MishapFilter
		want []string
	}{
		{"one type", MishapFilter{Type: "shell-trap"}, []string{id("01"), id("02")}},
		{"untyped", MishapFilter{Type: core.MishapUntyped}, []string{id("05"), id("06"), id("07"), id("08"), id("09"), id("10"), id("11")}},
		{"a project", MishapFilter{Project: "demo", ByProject: true}, []string{id("01"), id("02"), id("03"), id("05"), id("06"), id("12")}},
		{"the global scope", MishapFilter{ByProject: true}, []string{id("04")}},
		{"a window", MishapFilter{Since: now.AddDate(0, 0, -7), Type: "fabricated"}, nil},
		{"a type, a project and a window", MishapFilter{Type: "wrong-target", Project: "demo", ByProject: true, Since: now.AddDate(0, 0, -7)},
			[]string{id("03")}},
		{"a limit", MishapFilter{Limit: 2}, []string{id("01"), id("02")}},
		{"no such project", MishapFilter{Project: "nowhere", ByProject: true}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ListMishaps(ctx, db, tc.f)
			require.NoError(t, err)
			require.Equal(t, tc.want, nilIfEmpty(mishapIDs(got)))
		})
	}

	_, err = ListMishaps(ctx, db, MishapFilter{Type: "shell_trap"})
	require.ErrorContains(t, err, `invalid mishap type "shell_trap"`, "a filter takes the slug, never a spelling of it")
}

func TestCountMishaps_ByTypeAndByMemory(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	mishapFleet(t, db, now)

	got, err := CountMishaps(ctx, db, MishapFilter{})
	require.NoError(t, err)
	require.Equal(t, map[core.MishapType]int{"shell-trap": 2, "wrong-target": 1, "live-state": 1, "fabricated": 1, "": 7}, got.ByType)
	require.Equal(t, map[string]int{"MEM1": 3, "MEM2": 2}, got.ByMemory,
		"a report naming a memory twice counts once; another kind's item_ids never count")

	// The type filter narrows the memories block, never the chips.
	got, err = CountMishaps(ctx, db, MishapFilter{Type: "shell-trap", Since: now.AddDate(0, 0, -7)})
	require.NoError(t, err)
	require.Equal(t, map[core.MishapType]int{"shell-trap": 2, "wrong-target": 1, "live-state": 1, "": 7}, got.ByType)
	require.Equal(t, map[string]int{"MEM1": 2}, got.ByMemory)

	got, err = CountMishaps(ctx, db, MishapFilter{Type: core.MishapUntyped})
	require.NoError(t, err)
	require.Equal(t, map[string]int{"MEM1": 1}, got.ByMemory, "the old report's item_ids still link")

	got, err = CountMishaps(ctx, db, MishapFilter{Project: "nowhere", ByProject: true})
	require.NoError(t, err)
	require.Empty(t, got.ByType)
	require.Empty(t, got.ByMemory)

	_, err = CountMishaps(ctx, db, MishapFilter{Type: "bogus"})
	require.Error(t, err)
}

// Through the console's filtered handle a hidden project's reports are simply
// absent, from the rows and from every count.
func TestMishaps_ReadThroughTheView(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "seam.db")
	db, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	now := time.Now().UTC()
	mishapAt(t, db, "01MS0000000000000000000001", "S1", "example-ops", `{"description":"shell-trap: x","type":"shell-trap","item_ids":["MEM1"]}`, now.Add(-time.Hour))
	mishapAt(t, db, "01MS0000000000000000000002", "S2", "example-finance", `{"description":"live-state: y","type":"live-state","item_ids":["MEM2"]}`, now.Add(-time.Hour))

	view, err := OpenView(path, []string{"example-finance"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = view.Close() })
	rows, err := ListMishaps(ctx, view, MishapFilter{})
	require.NoError(t, err)
	require.Equal(t, []string{"01MS0000000000000000000001"}, mishapIDs(rows))
	rows, err = ListMishaps(ctx, view, MishapFilter{Project: "example-finance", ByProject: true})
	require.NoError(t, err)
	require.Empty(t, rows)
	tally, err := CountMishaps(ctx, view, MishapFilter{})
	require.NoError(t, err)
	require.Equal(t, map[core.MishapType]int{"shell-trap": 1}, tally.ByType)
	require.Equal(t, map[string]int{"MEM1": 1}, tally.ByMemory)
}
