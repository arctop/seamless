package console

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// seedTrial inserts one trial; seq spaces the timestamps a minute apart so
// newest-first orderings are deterministic.
func seedTrial(t *testing.T, db *sql.DB, lab, title string, outcome core.TrialOutcome, project, session string, metrics map[string]any, seq int) string {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	ts := time.Now().UTC().Add(time.Duration(seq-100) * time.Minute)
	require.NoError(t, store.CreateTrial(context.Background(), db, core.Trial{
		ID: id, Lab: lab, Title: title, Outcome: outcome, Metrics: metrics,
		ProjectSlug: project, SessionID: session,
		Changes: "swapped the driver", Expected: "clean boot", Actual: "boot loop",
		CreatedAt: ts,
	}))
	return id
}

func TestLabsLibrary_ListAndReader(t *testing.T) {
	db, mux := newConsole(t)

	seedTrial(t, db, "cold-lab", "old trial", core.OutcomePass, "demo", "", nil, 1)
	seedTrial(t, db, "hot-lab", "first", core.OutcomeFail, "demo", "", nil, 2)
	seedTrial(t, db, "hot-lab", "second", core.OutcomePass, "", "", nil, 3)

	// The list URL auto-opens the most recently active lab in the reader.
	page := getPeek(t, mux, "/console/labs")
	require.Equal(t, http.StatusOK, page.Code)
	body := page.Body.String()
	require.Contains(t, body, `id="lib-reader"`)
	require.Contains(t, body, `data-auto-url="/console/labs/hot-lab"`)
	require.Contains(t, body, "1 pass")
	require.Contains(t, body, "1 fail")
	require.Contains(t, body, "cold-lab")

	// JSON list.
	var list labsData
	getJSON(t, mux, "/console/labs?format=json", &list)
	require.Equal(t, 2, list.Count)
	require.Equal(t, 3, list.TrialsSum)
	require.Equal(t, "hot-lab", list.Rows[0].Lab, "most recently active first")

	// The detail URL renders the library page with this lab selected.
	detail := getPeek(t, mux, "/console/labs/cold-lab")
	require.Equal(t, http.StatusOK, detail.Code)
	require.Contains(t, detail.Body.String(), "old trial")
	require.NotContains(t, detail.Body.String(), "data-auto-url=", "an explicit selection is not client-pinned")

	// ?reader=1 returns just the reader fragment for the in-place swap.
	frag := getPeek(t, mux, "/console/labs/hot-lab?reader=1")
	require.Equal(t, http.StatusOK, frag.Code)
	require.NotContains(t, frag.Body.String(), "<html")
	require.Contains(t, frag.Body.String(), "reader-sheet")
	require.Contains(t, frag.Body.String(), "second")

	// JSON detail carries the trial history.
	var d labDetailData
	getJSON(t, mux, "/console/labs/hot-lab?format=json", &d)
	require.Equal(t, "hot-lab", d.Row.Lab)
	require.Len(t, d.Trials, 2)
	require.Equal(t, "second", d.Trials[0].Title, "newest first")

	// Unknown lab is a 404 naming it.
	missing := getPeek(t, mux, "/console/labs/nope?format=json")
	require.Equal(t, http.StatusNotFound, missing.Code)
	require.Contains(t, missing.Body.String(), "nope")
}

// A lab name is a free-form label: its console path escapes it as one segment
// and the wildcard route round-trips it.
func TestLabsLibrary_EscapedLabName(t *testing.T) {
	db, mux := newConsole(t)
	seedTrial(t, db, "my lab", "spaced", core.OutcomePass, "demo", "", nil, 1)

	require.Equal(t, "/console/labs/my%20lab", labPath("my lab"))
	page := getPeek(t, mux, "/console/labs/my%20lab")
	require.Equal(t, http.StatusOK, page.Code)
	require.Contains(t, page.Body.String(), "spaced")
}

func TestTrialsLibrary_FiltersAndReader(t *testing.T) {
	db, mux := newConsole(t)

	failID := seedTrial(t, db, "hot-lab", "boot fails", core.OutcomeFail, "demo", "", map[string]any{"hz": 497}, 1)
	seedTrial(t, db, "hot-lab", "boot passes", core.OutcomePass, "demo", "", nil, 2)
	seedTrial(t, db, "cold-lab", "elsewhere", core.OutcomePass, "", "", nil, 3)

	// The list URL groups by lab and auto-opens the newest trial.
	page := getPeek(t, mux, "/console/trials")
	require.Equal(t, http.StatusOK, page.Code)
	body := page.Body.String()
	require.Contains(t, body, `id="lib-reader"`)
	require.Contains(t, body, "data-auto-url=")
	require.Contains(t, body, "elsewhere")

	var list trialsData
	getJSON(t, mux, "/console/trials?format=json", &list)
	require.Equal(t, 3, list.Count)
	require.Len(t, list.Groups, 2)
	require.Equal(t, "cold-lab", list.Groups[0].Lab, "groups follow the newest trial")

	// ?outcome= filters (exact match; outcomes are free-form by design).
	var fails trialsData
	getJSON(t, mux, "/console/trials?outcome=fail&format=json", &fails)
	require.Equal(t, 1, fails.Count)
	require.Equal(t, "boot fails", fails.Groups[0].Trials[0].Title)

	// ?lab= narrows to one lab.
	var byLab trialsData
	getJSON(t, mux, "/console/trials?lab=hot-lab&format=json", &byLab)
	require.Equal(t, 2, byLab.Count)
	require.Len(t, byLab.Groups, 1)

	// Rail links carry the active filters.
	filtered := getPeek(t, mux, "/console/trials?lab=hot-lab&outcome=fail")
	require.Equal(t, http.StatusOK, filtered.Code)
	require.Contains(t, filtered.Body.String(), "lab=hot-lab&amp;outcome=fail")

	// Trial detail: JSON carries the record and its metrics.
	var d trialDetailData
	getJSON(t, mux, "/console/trials/"+failID+"?format=json", &d)
	require.Equal(t, "boot fails", d.Title)
	require.Equal(t, "hot-lab", d.Lab)
	require.Equal(t, "swapped the driver", d.ChangesText)
	require.Equal(t, "clean boot", d.ExpText)
	require.Equal(t, "boot loop", d.ActText)
	require.Len(t, d.Metrics, 1)
	require.Equal(t, "hz", d.Metrics[0].Key)

	// The detail URL renders the library page with the trial selected.
	detail := getPeek(t, mux, "/console/trials/"+failID)
	require.Equal(t, http.StatusOK, detail.Code)
	require.Contains(t, detail.Body.String(), "boot loop")
	require.NotContains(t, detail.Body.String(), "data-auto-url=")

	// ?reader=1 returns the reader fragment; ?peek=1 the pane fragment.
	frag := getPeek(t, mux, "/console/trials/"+failID+"?reader=1")
	require.Equal(t, http.StatusOK, frag.Code)
	require.NotContains(t, frag.Body.String(), "<html")
	require.Contains(t, frag.Body.String(), "reader-sheet")
	peek := getPeek(t, mux, "/console/trials/"+failID+"?peek=1")
	require.Equal(t, http.StatusOK, peek.Code)
	require.NotContains(t, peek.Body.String(), "<html")
	require.Contains(t, peek.Body.String(), "peek-entity")
	require.Contains(t, peek.Body.String(), "boot loop")

	// Unknown trial is a 404 naming the id.
	missing := getPeek(t, mux, "/console/trials/nope?format=json")
	require.Equal(t, http.StatusNotFound, missing.Code)
	require.Contains(t, missing.Body.String(), "nope")
}

func TestSearch_IncludesTrials(t *testing.T) {
	db, mux := newConsole(t)
	seedTrial(t, db, "hot-lab", "dfu baseline sweep", core.OutcomeFail, "demo", "", nil, 1)

	var data searchData
	getJSON(t, mux, "/console/search?q=baseline&fast=1&format=json", &data)
	require.Equal(t, 1, data.Total)
	require.Len(t, data.Groups, 1)
	require.Equal(t, "trials", data.Groups[0].Kind)
	row := data.Groups[0].Rows[0]
	require.Equal(t, "dfu baseline sweep", row.Title)
	require.Contains(t, row.Href, "/console/trials/")
	require.Contains(t, row.Description, "hot-lab")

	// The trials scope narrows to trials only.
	var scoped searchData
	getJSON(t, mux, "/console/search?q=baseline&scope=trials&fast=1&format=json", &scoped)
	require.Equal(t, 1, scoped.Total)
}

func TestSessionDetail_ShowsTrialsRecorded(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()

	sessID, err := core.NewID()
	require.NoError(t, err)
	now := time.Now().UTC()
	require.NoError(t, store.CreateSession(ctx, db, core.Session{
		ID: sessID, Name: "cc/trialsess", ProjectSlug: "demo", Status: core.SessionCompleted,
		CreatedAt: now.Add(-time.Hour), UpdatedAt: now,
	}))
	trialID := seedTrial(t, db, "hot-lab", "session trial", core.OutcomeFail, "demo", sessID, nil, 1)

	var detail sessionDetail
	getJSON(t, mux, "/console/sessions/"+sessID+"?format=json", &detail)
	require.Len(t, detail.Trials, 1)
	require.Equal(t, "session trial", detail.Trials[0].Title)
	require.Equal(t, "hot-lab", detail.Trials[0].Lab)

	page := getPeek(t, mux, "/console/sessions/"+sessID)
	require.Equal(t, http.StatusOK, page.Code)
	require.Contains(t, page.Body.String(), "Trials recorded")
	require.Contains(t, page.Body.String(), "/console/trials/"+trialID)

	// The trial reader links back to the recording session by name.
	var d trialDetailData
	getJSON(t, mux, "/console/trials/"+trialID+"?format=json", &d)
	require.Equal(t, sessID, d.SessionID)
	require.Equal(t, "cc/trialsess", d.SessionName)
}

// Past the rail's cut the head counts every trial and says the rail holds the
// newest: it read "200 trials across 11 labs" while Labs counted 208 across 13.
func TestTrialsLibrary_HeadCountsPastTheCut(t *testing.T) {
	db, mux := newConsole(t)
	for i := range trialListLimit + 3 {
		lab := "lab-a"
		if i < 3 {
			lab = "lab-old" // the oldest three: only this lab falls past the cut
		}
		seedTrial(t, db, lab, fmt.Sprintf("trial %03d", i), core.OutcomePass, "demo", "", nil, i)
	}
	var list trialsData
	getJSON(t, mux, "/console/trials?format=json", &list)
	require.Equal(t, trialListLimit, list.Count)
	require.Equal(t, trialListLimit+3, list.Total)
	body := getPeek(t, mux, "/console/trials").Body.String()
	require.Contains(t, body, `<strong>203</strong>trials<small>newest 200 shown</small>`)
	require.NotContains(t, body, "across 1 lab", "a lab count of the cut would leave lab-old out")

	getJSON(t, mux, "/console/trials?lab=lab-old&format=json", &list)
	require.Equal(t, 3, list.Total, "under the cut the head is the rows")
	require.Contains(t, getPeek(t, mux, "/console/trials?lab=lab-old").Body.String(), `<strong>3</strong>trials<small>across 1 lab</small>`)
}
