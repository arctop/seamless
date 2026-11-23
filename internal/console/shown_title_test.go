package console

import (
	"context"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/plans"
	"github.com/arctop/seamless/internal/store"
)

// A spaced double hyphen in a title shows as a colon; a hyphen pair inside a
// word or a flag is not one.
func TestShownTitle_SpacedDoubleHyphenIsAColon(t *testing.T) {
	require.Equal(t, "Retire the v1 port: after the cutover: then drop it",
		shownTitle("Retire the v1 port -- after the cutover -- then drop it"))
	for _, kept := range []string{"pass --force-with-lease", "a--b", "", "plain title"} {
		require.Equal(t, kept, shownTitle(kept))
	}
	require.Equal(t, "Plan: one &amp; two", string(planTitleHTML("Plan -- one & two")))
}

// shownDash is a spaced double hyphen left on a page, once the places that
// keep the stored title on purpose are set aside: what a copy button copies
// and the JSON a page embeds for its script.
var (
	shownDash      = regexp.MustCompile(`\s--\s`)
	shownCopyAttr  = regexp.MustCompile(`data-copy="[^"]*"`)
	shownScriptTag = regexp.MustCompile(`(?s)<script[^>]*>.*?</script>`)
)

// Every screen that draws an agent-written title (a task's, a plan's, a
// note's, a trial's) shows its " -- " as a colon, the owner's call; the copy
// button still copies the title as the agent wrote it.
func TestShownTitle_EveryScreenShowsTitlesWithoutDoubleHyphens(t *testing.T) {
	feats := ndAllOn
	feats.Diary, feats.Research = true, true
	c := ndNew(t, "advanced", feats)
	ctx := context.Background()
	now := time.Now().UTC()
	const proj = "example-ops"

	_, err := store.EnsureProject(ctx, c.db, proj, proj)
	require.NoError(t, err)
	live := ndSession(t, c.db, "cc/st-live", proj, true)
	gate := ndTask(t, c.db, core.Task{ProjectSlug: proj, Title: "Sign the renewal -- vendor side", Kind: core.TaskKindGate,
		Gate: core.TaskGate{Who: "owner", What: "sign it"}})
	lease := now.Add(time.Hour)
	held := ndTask(t, c.db, core.Task{ProjectSlug: proj, Title: "Write the migration -- step one", PlanSlug: "storage",
		Status: core.TaskInProgress, ClaimedBy: live, LeaseExpiresAt: &lease, UpdatedAt: now})
	ready := ndTask(t, c.db, core.Task{ProjectSlug: proj, Title: "Run the dry run -- staging", PlanSlug: "storage",
		DependsOn: []string{held}})
	loose := ndTask(t, c.db, core.Task{ProjectSlug: proj, Title: "Tidy the routes -- loose end", Status: core.TaskInProgress})
	note, err := c.mgr.WriteNote(ctx, core.Note{ID: mustID(t), Slug: "storage-move", Title: "Storage -- the move", Project: proj,
		Description: "the plan's narrative", Body: "# Storage", Tags: []string{plans.SlugTag("storage")}, Created: now, Updated: now})
	require.NoError(t, err)
	trial := mustID(t)
	require.NoError(t, store.CreateTrial(ctx, c.db, core.Trial{ID: trial, Lab: "boot-lab", Title: "Cold boot -- second try",
		Outcome: core.OutcomePass, ProjectSlug: proj, Expected: "boots", Actual: "boots", CreatedAt: now}))

	pages := []string{
		"/console/",
		"/console/now",
		"/console/tasks",
		"/console/tasks?plan=storage&project=" + proj,
		"/console/tasks/" + gate,
		"/console/tasks/" + held,
		"/console/tasks/" + ready + "?peek=1",
		"/console/tasks/" + loose,
		"/console/notes",
		"/console/notes/" + note.ID,
		"/console/notes/" + note.ID + "?peek=1",
		"/console/plans",
		"/console/plans/storage?project=" + proj,
		"/console/plans/storage?project=" + proj + "&peek=1",
		"/console/projects/" + proj,
		"/console/projects/" + proj + "?tab=tasks",
		"/console/projects/" + proj + "?tab=notes",
		"/console/sessions/" + live,
		"/console/trials",
		"/console/trials/" + trial,
		"/console/search?q=" + url.QueryEscape("migration"),
		"/console/search?q=" + url.QueryEscape("storage"),
		"/console/diary",
	}
	for _, p := range pages {
		body := getHTMLBody(t, c.mux, p)
		shown := shownScriptTag.ReplaceAllString(shownCopyAttr.ReplaceAllString(body, ""), "")
		if loc := shownDash.FindStringIndex(shown); loc != nil {
			from, to := max(loc[0]-120, 0), min(loc[1]+120, len(shown))
			t.Errorf("%s shows a double hyphen: ...%q...", p, shown[from:to])
		}
	}

	for p, want := range map[string]string{
		"/console/now":                             "Write the migration: step one",
		"/console/plans":                           "Storage: the move",
		"/console/notes/" + note.ID:                "Storage: the move",
		"/console/trials/" + trial:                 "Cold boot: second try",
		"/console/tasks/" + ready + "?peek=1":      "Run the dry run: staging",
		"/console/sessions/" + live:                "Write the migration: step one",
		"/console/search?q=migration":              "Write the migration: step one",
		"/console/projects/" + proj + "?tab=tasks": "Tidy the routes: loose end",
	} {
		require.Contains(t, getHTMLBody(t, c.mux, p), want, "%s draws the title", p)
	}

	body := getHTMLBody(t, c.mux, "/console/tasks/"+gate)
	require.Contains(t, body, "Sign the renewal: vendor side")
	require.Contains(t, body, `data-copy="Sign the renewal -- vendor side"`, "the copy button copies the title as written")
	require.True(t, strings.Contains(getHTMLBody(t, c.mux, "/console/"), "Sign the renewal: vendor side"),
		"the needs card names the gate as the console shows titles")
}
