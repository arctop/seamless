package console

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// The "written by account" chip on the rows the account-label sweep left
// bare: console search results, a lab's trial list, and the project
// workspace's tabs. Each row a restricted local account (peer_auth.restricted)
// wrote carries the one chip markup every other row uses (accountChip); an
// owner row renders exactly as before. Events and the timeline stay unlabeled
// on purpose (owner decision D6).

// between returns the markup from the last open before the first ">name<"
// (or the first occurrence of name when it is not a text node) to the next
// close after it.
func between(t *testing.T, page, name, open, closer string) string {
	t.Helper()
	at := strings.Index(page, ">"+name+"<")
	if at < 0 {
		at = strings.Index(page, name)
	}
	require.GreaterOrEqual(t, at, 0, "the page names no %q", name)
	start := strings.LastIndex(page[:at], open)
	require.GreaterOrEqual(t, start, 0, "no %q before %q", open, name)
	end := strings.Index(page[at:], closer)
	require.GreaterOrEqual(t, end, 0, "no %q after %q", closer, name)
	return page[start : at+end]
}

// tabPanel returns one workspace tab's panel: from its data-panel attribute to
// the next panel (or the end of the page).
func tabPanel(t *testing.T, page, tab string) string {
	t.Helper()
	at := strings.Index(page, `data-panel="`+tab+`"`)
	require.GreaterOrEqual(t, at, 0, "no %s panel", tab)
	rest := page[at+1:]
	if end := strings.Index(rest, `<div class="tabpanel`); end >= 0 {
		rest = rest[:end]
	}
	return rest
}

func TestAccountChips_SearchResults(t *testing.T) {
	f := newRestrictedConsole(t, config.Features{Research: true}, true)
	seedLabeledItems(t, f)
	chip := accountChip(exampleAccount)

	for _, tc := range []struct {
		query, scope, labeled, owner string
	}{
		{"gotcha", "memories", "account-gotcha", "owner-gotcha"},
		{"note", "notes", "Note account-note", "Note owner-note"},
		{"task", "tasks", "account task", "owner task"},
		{"trial", "trials", "account trial", "owner trial"},
		// The account's session by its qualified name (SEAL A2).
		{"cc/", "sessions", exampleAccount + ":cc/account01", "cc/owner001"},
	} {
		t.Run(tc.scope, func(t *testing.T) {
			path := "/console/search?scope=" + tc.scope + "&q=" + tc.query
			page := getHTMLBody(t, f.mux, path)
			require.Contains(t, between(t, page, tc.labeled, `<article class="search-result`, "</article>"), chip)
			require.NotContains(t, between(t, page, tc.owner, `<article class="search-result`, "</article>"), "data-account")

			var data struct {
				Groups []struct {
					Rows []struct {
						Title   string  `json:"title"`
						Account *string `json:"account"`
					} `json:"rows"`
				} `json:"groups"`
			}
			getJSON(t, f.mux, path+"&format=json", &data)
			seen := map[string]*string{}
			for _, g := range data.Groups {
				for _, r := range g.Rows {
					seen[r.Title] = r.Account
				}
			}
			require.Contains(t, seen, tc.labeled)
			require.NotNil(t, seen[tc.labeled], "the account's row carries its account")
			require.Equal(t, exampleAccount, *seen[tc.labeled])
			require.Contains(t, seen, tc.owner)
			require.Nil(t, seen[tc.owner], "the owner's row carries no account key")
		})
	}
}

func TestAccountChips_LabTrialList(t *testing.T) {
	f := newRestrictedConsole(t, config.Features{Research: true}, true)
	seedLabeledItems(t, f)
	chip := accountChip(exampleAccount)

	for _, path := range []string{"/console/labs/boot-lab", "/console/labs/boot-lab?reader=1"} {
		page := getHTMLBody(t, f.mux, path)
		require.Contains(t, between(t, page, "account trial", "<li>", "</li>"), chip, path)
		require.NotContains(t, between(t, page, "owner trial", "<li>", "</li>"), "data-account", path)
	}

	var d struct {
		Trials []map[string]any `json:"trials"`
	}
	getJSON(t, f.mux, "/console/labs/boot-lab?format=json", &d)
	require.Len(t, d.Trials, 2)
	for _, tr := range d.Trials {
		if tr["title"] == "account trial" {
			require.Equal(t, exampleAccount, tr["account"])
		} else {
			require.NotContains(t, tr, "account", "the owner's trial carries no account key")
		}
	}
}

func TestAccountChips_ProjectWorkspaceTabs(t *testing.T) {
	f := newRestrictedConsole(t, config.Features{Research: true}, true)
	ctx := context.Background()
	_, err := store.EnsureProject(ctx, f.db, "app", "app")
	require.NoError(t, err)
	seedLabeledItems(t, f)
	now := time.Now().UTC()
	// A plan step each, so the Plans & tasks tab renders a timeline.
	for _, st := range []struct{ title, account string }{
		{"account step", exampleAccount}, {"owner step", ""},
	} {
		require.NoError(t, store.CreateTask(ctx, f.db, core.Task{ID: mustID(t), ProjectSlug: "app", Title: st.title,
			PlanSlug: "boot-plan", Status: core.TaskOpen, Account: st.account, CreatedAt: now, UpdatedAt: now}))
	}
	chip := accountChip(exampleAccount)

	for _, tc := range []struct {
		tab, open, closer, labeled, owner string
	}{
		{"tasks", `<details class="step`, "</summary>", "account step", "owner step"},
		{"tasks", "<tr id=", "</tr>", "account task", "owner task"},
		{"sessions", `<div id="`, `<span class="when">`, exampleAccount + ":cc/account01", "cc/owner001"},
		{"memories", "<tr id=", "</tr>", "account-gotcha", "owner-gotcha"},
		{"notes", "<tr id=", "</tr>", "Note account-note", "Note owner-note"},
	} {
		t.Run(tc.tab+" "+tc.labeled, func(t *testing.T) {
			page := getHTMLBody(t, f.mux, "/console/projects/app?tab="+tc.tab)
			panel := tabPanel(t, page, tc.tab)
			require.Contains(t, between(t, panel, tc.labeled, tc.open, tc.closer), chip)
			require.NotContains(t, between(t, panel, tc.owner, tc.open, tc.closer), "data-account")
		})
	}
}

// Without restricted accounts nothing on these surfaces grows a chip, and the
// search and lab JSON grow no key.
func TestAccountChips_SearchLabsAndTabsUnchangedWithoutAccounts(t *testing.T) {
	f := newRestrictedConsole(t, config.Features{Research: true}, false)
	ctx := context.Background()
	_, err := store.EnsureProject(ctx, f.db, "app", "app")
	require.NoError(t, err)
	now := time.Now().UTC()
	seedOwnerWork(t, f.db, now)
	for _, path := range []string{"/console/search?scope=all&q=owner", "/console/labs/boot-lab",
		"/console/projects/app?tab=tasks", "/console/projects/app?tab=sessions"} {
		page := getHTMLBody(t, f.mux, path)
		require.NotContains(t, page, "data-account", path)
		require.NotContains(t, page, "by account", path)
	}
	for _, path := range []string{"/console/search?scope=all&q=owner&format=json", "/console/labs/boot-lab?format=json"} {
		require.NotContains(t, getPeek(t, f.mux, path).Body.String(), `"account"`, path)
	}
}

// A plan row in search carries its writer (D6): the account of the note that
// labels it, or -- a plan only tasks carry -- of its tasks when one account
// wrote them all. The owner's, and a mixed one, carry none.
func TestAccountChips_SearchPlans(t *testing.T) {
	f := newRestrictedConsole(t, config.Features{Research: true}, true)
	ctx := context.Background()
	_, err := store.EnsureProject(ctx, f.db, "app", "app")
	require.NoError(t, err)
	now := time.Now().UTC()
	note := func(slug, title, account string) {
		_, err := f.mgr.WriteNote(ctx, core.Note{ID: mustID(t), Title: title, Slug: slug, Project: "app",
			Body: "The narrative.", Tags: []string{"plan:" + slug}, Created: now, Updated: now, Account: account})
		require.NoError(t, err)
	}
	note("account-plan", "Account plan narrative", exampleAccount)
	note("owner-plan", "Owner plan narrative", "")
	task := func(plan, account string) {
		require.NoError(t, store.CreateTask(ctx, f.db, core.Task{ID: mustID(t), ProjectSlug: "app", Title: "a step of " + plan,
			PlanSlug: plan, Status: core.TaskOpen, Account: account, CreatedAt: now, UpdatedAt: now}))
	}
	task("account-steps-plan", exampleAccount)
	task("account-steps-plan", exampleAccount)
	task("mixed-steps-plan", exampleAccount)
	task("mixed-steps-plan", "")
	// The account's narrative labels its plan even though the owner added a
	// step to it.
	task("account-plan", "")

	var data struct {
		Groups []struct {
			Rows []struct {
				Title   string  `json:"title"`
				Account *string `json:"account"`
			} `json:"rows"`
		} `json:"groups"`
	}
	getJSON(t, f.mux, "/console/search?scope=plans&q=plan&format=json", &data)
	seen := map[string]string{}
	for _, g := range data.Groups {
		for _, r := range g.Rows {
			seen[r.Title] = ""
			if r.Account != nil {
				seen[r.Title] = *r.Account
			}
		}
	}
	require.Equal(t, map[string]string{
		"Account plan narrative": exampleAccount, "Owner plan narrative": "",
		"account-steps-plan": exampleAccount, "mixed-steps-plan": "",
	}, seen)

	page := getHTMLBody(t, f.mux, "/console/search?scope=plans&q=plan")
	chip := accountChip(exampleAccount)
	require.Contains(t, between(t, page, "Account plan narrative", `<article class="search-result`, "</article>"), chip)
	require.Contains(t, between(t, page, "account-steps-plan", `<article class="search-result`, "</article>"), chip)
	require.NotContains(t, between(t, page, "Owner plan narrative", `<article class="search-result`, "</article>"), "data-account")
	require.NotContains(t, between(t, page, "mixed-steps-plan", `<article class="search-result`, "</article>"), "data-account")
}
