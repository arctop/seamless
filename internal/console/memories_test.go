package console

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
)

// newConsoleWithFiles builds a console backed by a real files.Manager over a
// temp data dir, so memory writes/archives round-trip through the source-of-truth
// files. It returns the DB (to seed events), the manager (to seed memories), and
// the mux.
func newConsoleWithFiles(t *testing.T) (*sql.DB, *files.Manager, *http.ServeMux) {
	t.Helper()
	dir := t.TempDir()
	db, err := openFreshDB(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	dataDir := filepath.Join(dir, "data")
	mgr, err := files.NewManager(dataDir, db, slog.Default())
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })

	// Advanced, like newConsoleFeatures: these tests exercise the screens'
	// full controls, not the level gates.
	svc, err := New(Config{
		DB: db, Files: mgr, Events: events.NewRecorder(db), DataDir: dataDir, APIKey: testKey,
		Level: "advanced",
	})
	require.NoError(t, err)
	mux := http.NewServeMux()
	svc.Register(mux)
	return db, mgr, mux
}

func writeMemory(t *testing.T, mgr *files.Manager, kind core.MemoryKind, project, name, desc string) core.Memory {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	now := time.Now().UTC()
	m, err := mgr.WriteMemory(context.Background(), core.Memory{
		ID: id, Kind: kind, Name: name, Description: desc, Project: project,
		Body: "body of " + name, Created: now, Updated: now, ValidFrom: now,
	})
	require.NoError(t, err)
	return m
}

func TestMemoriesPage_GroupsAndArchive(t *testing.T) {
	_, mgr, mux := newConsoleWithFiles(t)

	m1 := writeMemory(t, mgr, core.KindGotcha, "seamless", "watcher-race", "a surprising pitfall")
	writeMemory(t, mgr, core.KindConstraint, "", "no-cgo", "never enable cgo")

	// List
	var data memoriesData
	getJSON(t, mux, "/console/memories?format=json", &data)
	require.Equal(t, 2, data.ActiveCount)
	require.Equal(t, 0, data.InactiveCount)
	// global group ("") sorts first.
	require.Equal(t, "", data.Groups[0].Project)
	require.Equal(t, "seamless", data.Groups[1].Project)

	// HTML renders the memory name.
	reqH := httptest.NewRequest(http.MethodGet, "/console/memories", nil)
	reqH.Header.Set("Authorization", "Bearer "+testKey)
	rrH := do(mux, reqH)
	require.Equal(t, http.StatusOK, rrH.Code)
	require.Contains(t, rrH.Body.String(), "watcher-race")

	// Archive m1
	req := httptest.NewRequest(http.MethodPost, "/console/memories/"+m1.ID+"/archive", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	rr := do(mux, req)
	require.Equal(t, http.StatusSeeOther, rr.Code)

	// It is now inactive.
	var after memoriesData
	getJSON(t, mux, "/console/memories?format=json", &after)
	require.Equal(t, 1, after.ActiveCount)
	require.Equal(t, 1, after.InactiveCount)
	require.Equal(t, "archived", after.Inactive[0].Status)
	require.Equal(t, "watcher-race", after.Inactive[0].Name)

	// A historical memory cannot be mistaken for current guidance in the reader.
	page := getPeek(t, mux, "/console/memories/"+m1.ID)
	require.Equal(t, http.StatusOK, page.Code)
	require.Contains(t, page.Body.String(), "This memory is archived.")
	require.Contains(t, page.Body.String(), "It no longer enters agent context.")
}

func TestMemoriesPage_DefaultSortIsRecentWithinKind(t *testing.T) {
	_, mgr, mux := newConsoleWithFiles(t)
	base := time.Date(2026, time.July, 20, 12, 0, 0, 0, time.UTC)
	write := func(name string, updated time.Time) {
		t.Helper()
		id, err := core.NewID()
		require.NoError(t, err)
		_, err = mgr.WriteMemory(context.Background(), core.Memory{
			ID: id, Kind: core.KindGotcha, Name: name, Description: name,
			Project: "seamless", Body: "body", Created: updated,
			Updated: updated, ValidFrom: updated,
		})
		require.NoError(t, err)
	}

	write("alpha-old", base)
	write("zeta-new", base.Add(time.Hour))

	var recent memoriesData
	getJSON(t, mux, "/console/memories?format=json", &recent)
	require.Equal(t, "recent", recent.Sort)
	require.Len(t, recent.Groups, 1)
	require.Len(t, recent.Groups[0].Kinds, 1)
	require.Equal(t, []string{"zeta-new", "alpha-old"}, []string{
		recent.Groups[0].Kinds[0].Memories[0].Name,
		recent.Groups[0].Kinds[0].Memories[1].Name,
	})

	// An explicit alternate mode still overrides the default.
	var byName memoriesData
	getJSON(t, mux, "/console/memories?sort=name&format=json", &byName)
	require.Equal(t, []string{"alpha-old", "zeta-new"}, []string{
		byName.Groups[0].Kinds[0].Memories[0].Name,
		byName.Groups[0].Kinds[0].Memories[1].Name,
	})
}

func TestSurfacedStaleness_ThresholdsMatchTheOverviewBucket(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	old := now.AddDate(0, 0, -200)
	at := func(d time.Duration) *time.Time { v := now.Add(d); return &v }

	tests := []struct {
		name          string
		last          *time.Time
		created       time.Time
		wantAge, tone string
	}{
		{name: "surfaced minutes ago", last: at(-4 * time.Minute), created: old, wantAge: "4m", tone: "ok"},
		{name: "surfaced inside the fresh window", last: at(-6 * 24 * time.Hour), created: old, wantAge: "6d", tone: "ok"},
		{name: "surfaced in the middle band", last: at(-20 * 24 * time.Hour), created: old, wantAge: "20d", tone: ""},
		{name: "surfaced past the stale horizon", last: at(-47 * 24 * time.Hour), created: old, wantAge: "47d", tone: "warn"},
		{name: "never surfaced, long-lived", created: old, wantAge: "never", tone: "warn"},
		// Written this morning: it has not gone quiet, it has not had its turn.
		{name: "never surfaced, brand new", created: now.Add(-2 * time.Hour), wantAge: "never", tone: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			age, tone := surfacedStaleness(tc.last, tc.created, now)
			require.Equal(t, tc.tone, tone)
			if tc.last == nil {
				require.Equal(t, tc.wantAge, age)
			}
		})
	}
}

func TestBodyEchoesDescription(t *testing.T) {
	desc := "Outbox rows are written in the SAME transaction as the domain change."
	require.True(t, bodyEchoesDescription("", desc), "an absent body adds nothing")
	require.True(t, bodyEchoesDescription("   \n\n  ", desc))
	require.True(t, bodyEchoesDescription(desc, desc))
	require.True(t, bodyEchoesDescription("Outbox rows are written in the **SAME** transaction as the domain change",
		desc), "markdown emphasis and trailing punctuation are not a difference")
	require.False(t, bodyEchoesDescription(desc+"\n\nUse an advisory lock per account.", desc))
	require.False(t, bodyEchoesDescription("Something else entirely.", desc))
	// With no description to echo, a real body is still a real body.
	require.False(t, bodyEchoesDescription("a genuine body", ""))
	require.True(t, bodyEchoesDescription("", ""))
}

func TestMemoryReader_RanksFactsAndRefusesToEchoTheDescription(t *testing.T) {
	_, mgr, mux := newConsoleWithFiles(t)

	// A memory whose body says something the description does not.
	full := writeMemory(t, mgr, core.KindGotcha, "seamless", "has-body", "a real description")
	page := getPeek(t, mux, "/console/memories/"+full.ID)
	require.Equal(t, http.StatusOK, page.Code)
	body := page.Body.String()
	require.Contains(t, body, `class="mv2-facts"`)
	require.Contains(t, body, "<span>utility</span>")
	require.Contains(t, body, "<span>since surfaced</span>")
	require.Contains(t, body, `class="mv2-facts-rest"`)
	require.NotContains(t, body, `class="reader-facts"`, "the memory reader no longer uses the flat fact row")
	require.Contains(t, body, "body of has-body")
	require.NotContains(t, body, "mv2-nobody")
	// Never surfaced and brand new: the chip states the fact without a verdict.
	require.Contains(t, body, `<span class="mv2-fact-hi" title="Has never entered an agent context">never`)

	// A memory whose body is only the description again.
	id, err := core.NewID()
	require.NoError(t, err)
	now := time.Now().UTC()
	echo, err := mgr.WriteMemory(context.Background(), core.Memory{
		ID: id, Kind: core.KindRunbook, Name: "echo-mem", Project: "seamless",
		Description: "Backfill the dedupe store from the outbox after an outage.",
		Body:        "Backfill the dedupe store from the outbox after an outage",
		Created:     now, Updated: now, ValidFrom: now,
	})
	require.NoError(t, err)

	page = getPeek(t, mux, "/console/memories/"+echo.ID)
	require.Equal(t, http.StatusOK, page.Code)
	body = page.Body.String()
	require.Contains(t, body, `class="mv2-nobody"`)
	require.Contains(t, body, "No body beyond the description yet")
	require.Contains(t, body, "Edit in editor")
	// Inside the reader sheet the sentence appears once -- as the description,
	// not again below it wearing a body's clothes. (The rail row shows it too;
	// that is a different pane answering a different question.)
	sheet := body[strings.Index(body, `<article class="reader-sheet"`):]
	require.Equal(t, 1, strings.Count(sheet, "Backfill the dedupe store from the outbox after an outage"))
}
