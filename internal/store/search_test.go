package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
)

// insertSearchTask inserts a task row directly, so a search test can pin
// plan_slug and updated_at without going through the queue's write path.
func insertSearchTask(t *testing.T, db *sql.DB, id, project, title, planSlug, updated string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO tasks (id, project_slug, title, body, status, created_by,
		    plan_slug, claimed_by, lease_expires_at, created_at, updated_at, closed_at)
		VALUES (?, ?, ?, '', 'open', 'test', ?, '', NULL, ?, ?, NULL)`,
		id, project, title, planSlug, updated, updated)
	require.NoError(t, err)
}

func insertSearchNote(t *testing.T, db *sql.DB, id, title, slug, project, tagsJSON, updated string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO notes_index
		    (id, title, slug, description, project, file_path, tags, source_url,
		     content_hash, created_at, updated_at)
		VALUES (?, ?, ?, '', ?, ?, ?, '', 'h', ?, ?)`,
		id, title, slug, project, "notes/"+dirOf(project)+"/"+slug+".md", tagsJSON, updated, updated)
	require.NoError(t, err)
}

func TestSearchTasks_TitleAndID(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := core.FormatTime(time.Now().UTC())

	insertSearchTask(t, db, "01TASKA", "seam", "wire the search palette", "", now)
	insertSearchTask(t, db, "01TASKB", "seam", "unrelated chore", "", now)

	got, err := SearchTasks(ctx, db, "palette", 20)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "01TASKA", got[0].ID)

	// Case-insensitive.
	got, err = SearchTasks(ctx, db, "PALETTE", 20)
	require.NoError(t, err)
	require.Len(t, got, 1)

	// An exact id is a lookup, even though it is not a title substring.
	got, err = SearchTasks(ctx, db, "01TASKB", 20)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "01TASKB", got[0].ID)
}

func TestSearchTasks_IDPrefixAndCaseInsensitiveExact(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := core.FormatTime(time.Now().UTC())

	const (
		first  = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
		second = "01ARZ3NDEKTSV4RRFFQ69G5FAW"
	)
	insertSearchTask(t, db, first, "seam", "unrelated first", "", now)
	insertSearchTask(t, db, second, "seam", "unrelated second", "", now)

	got, err := SearchTasks(ctx, db, strings.ToLower(first), 20)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, first, got[0].ID)

	got, err = SearchTasks(ctx, db, "01ARZ3ND", 20)
	require.NoError(t, err)
	require.Len(t, got, 2)

	got, err = SearchTasks(ctx, db, "01ARZ3N", 20)
	require.NoError(t, err)
	require.Empty(t, got, "a prefix shorter than eight characters must not fan out")
}

func TestSearchKnowledgeIdentifiers_PrioritizesNaturalIdentifiersAndIDs(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	at := func(min int) string { return core.FormatTime(base.Add(time.Duration(min) * time.Minute)) }

	const (
		exactID  = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
		prefixID = "01ARZ3NDEKTSV4RRFFQ69G5FAW"
	)
	insertSearchNote(t, db, exactID, "Deep code and security audit", "deep-code-and-security-audit-2026-07-31", "seam", `[]`, at(1))
	insertSearchNote(t, db, prefixID, "Newer audit", "another-deep-audit", "seam", `[]`, at(2))
	insertSearchNote(t, db, "01BXZ3NDEKTSV4RRFFQ69G5FAV", "Other project", "deep-code-and-security-audit-2026-07-31", "other", `[]`, at(3))

	hits, err := SearchKnowledgeIdentifiersSince(ctx, db, "deep-code-and-security-audit-2026-07-31", []string{"note"}, nil, time.Time{}, 20)
	require.NoError(t, err)
	require.Len(t, hits, 2)
	require.Equal(t, "01BXZ3NDEKTSV4RRFFQ69G5FAV", hits[0].ItemID,
		"same-slug matches across projects are newest-first")
	for _, hit := range hits {
		require.Equal(t, IdentifierMatchExactIdentifier, hit.Match)
	}

	hits, err = SearchKnowledgeIdentifiersSince(ctx, db, strings.ToLower(exactID), []string{"note"}, []string{"seam"}, time.Time{}, 20)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	require.Equal(t, IdentifierMatchExactID, hits[0].Match)

	hits, err = SearchKnowledgeIdentifiersSince(ctx, db, "01ARZ3ND", []string{"note"}, []string{"seam"}, time.Time{}, 20)
	require.NoError(t, err)
	require.Len(t, hits, 2)
	for _, hit := range hits {
		require.Equal(t, IdentifierMatchIDPrefix, hit.Match)
	}
}

func TestSearchKnowledgeIdentifiers_FiltersBeforeLimit(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	old := core.FormatTime(now.Add(-48 * time.Hour))
	recent := core.FormatTime(now.Add(-time.Hour))

	insertMemory(t, db, "01OLD", "gotcha", "shared-identifier-old", "d", "seam", "body", old, "")
	insertMemory(t, db, "01DEAD", "gotcha", "shared-identifier-dead", "d", "seam", "body", recent, recent)
	insertMemory(t, db, "01LIVE", "gotcha", "shared-identifier-live", "d", "seam", "body", recent, "")
	insertMemory(t, db, "01OTHER", "gotcha", "shared-identifier-other", "d", "other", "body", recent, "")

	hits, err := SearchKnowledgeIdentifiersSince(ctx, db, "shared-identifier", []string{"memory"}, []string{"seam"}, now.Add(-24*time.Hour), 1)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	require.Equal(t, "01LIVE", hits[0].ItemID,
		"window, project, and active-memory filters must run before LIMIT")
}

// LIKE metacharacters in the needle must match themselves. Without ESCAPE, "%"
// would match every task and "_" would match any single character.
func TestSearchTasks_EscapesLikeMetacharacters(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := core.FormatTime(time.Now().UTC())

	insertSearchTask(t, db, "01PCT", "seam", "ship at 50% coverage", "", now)
	insertSearchTask(t, db, "01UND", "seam", "rename a_b to c", "", now)
	insertSearchTask(t, db, "01PLAIN", "seam", "nothing special", "", now)

	got, err := SearchTasks(ctx, db, "50%", 20)
	require.NoError(t, err)
	require.Len(t, got, 1, "%% must be literal, not a wildcard")
	require.Equal(t, "01PCT", got[0].ID)

	got, err = SearchTasks(ctx, db, "a_b", 20)
	require.NoError(t, err)
	require.Len(t, got, 1, "_ must be literal, not a single-char wildcard")
	require.Equal(t, "01UND", got[0].ID)

	// A bare wildcard is a literal search for "%": it finds the one title that
	// actually contains the character, not every row.
	got, err = SearchTasks(ctx, db, "%", 20)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "01PCT", got[0].ID)
}

func TestSearchTasks_LimitAndOrder(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	at := func(min int) string { return core.FormatTime(base.Add(time.Duration(min) * time.Minute)) }

	insertSearchTask(t, db, "01OLD", "seam", "search one", "", at(1))
	insertSearchTask(t, db, "01MID", "seam", "search two", "", at(2))
	insertSearchTask(t, db, "01NEW", "seam", "search three", "", at(3))

	got, err := SearchTasks(ctx, db, "search", 20)
	require.NoError(t, err)
	require.Equal(t, []string{"01NEW", "01MID", "01OLD"}, []string{got[0].ID, got[1].ID, got[2].ID},
		"newest-updated first")

	got, err = SearchTasks(ctx, db, "search", 2)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, "01NEW", got[0].ID)
}

func TestSearchTasksSince_FiltersBeforeLimit(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	insertSearchTask(t, db, "01OLD", "seam", "window search old", "", core.FormatTime(now.Add(-48*time.Hour)))
	insertSearchTask(t, db, "01NEW", "seam", "window search new", "", core.FormatTime(now.Add(-time.Hour)))

	got, err := SearchTasksSince(ctx, db, "window search", now.Add(-24*time.Hour), 1)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "01NEW", got[0].ID)
}

func TestSearchSessions_NameAndID(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	require.NoError(t, CreateSession(ctx, db, core.Session{
		ID: "01SESSA", Name: "cc/palette-work", ProjectSlug: "seam",
		Status: core.SessionActive, CreatedAt: now, UpdatedAt: now,
	}))
	require.NoError(t, CreateSession(ctx, db, core.Session{
		ID: "01SESSB", Name: "cc/other", ProjectSlug: "seam",
		Status: core.SessionActive, CreatedAt: now, UpdatedAt: now,
	}))

	got, err := SearchSessions(ctx, db, "palette", 20)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "01SESSA", got[0].ID)

	got, err = SearchSessions(ctx, db, "01SESSB", 20)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "01SESSB", got[0].ID)
}

func TestSearchSessionsAndTrials_IDPrefixes(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	const (
		sessionID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
		trialID   = "01BXZ3NDEKTSV4RRFFQ69G5FAV"
	)

	require.NoError(t, CreateSession(ctx, db, core.Session{
		ID: sessionID, Name: "cc/unrelated", ProjectSlug: "seam",
		Status: core.SessionActive, CreatedAt: now, UpdatedAt: now,
	}))
	require.NoError(t, CreateTrial(ctx, db, core.Trial{
		ID: trialID, Lab: "unrelated-lab", Title: "unrelated trial",
		ProjectSlug: "seam", CreatedAt: now,
	}))

	sessions, err := SearchSessions(ctx, db, "01ARZ3ND", 20)
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	require.Equal(t, sessionID, sessions[0].ID)

	trials, err := SearchTrials(ctx, db, "01BXZ3ND", 20)
	require.NoError(t, err)
	require.Len(t, trials, 1)
	require.Equal(t, trialID, trials[0].ID)
}

func TestSearchProjects_SlugAndName(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	require.NoError(t, CreateProject(ctx, db, core.Project{
		ID: "01PA", Slug: "seamless", Name: "Seamless", CreatedAt: now, UpdatedAt: now,
	}))
	require.NoError(t, CreateProject(ctx, db, core.Project{
		ID: "01PB", Slug: "odysseus", Name: "Odysseus Stack", CreatedAt: now, UpdatedAt: now,
	}))

	// By slug.
	got, err := SearchProjects(ctx, db, "seam", 20)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "seamless", got[0].Slug)

	// By display name, case-insensitively.
	got, err = SearchProjects(ctx, db, "stack", 20)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "odysseus", got[0].Slug)
}

// A plan is bi-sourced: either its note or its tasks can carry the match, and
// the same (project, slug) reached through both must collapse to one row that
// prefers the note's real title over the slug fallback.
func TestSearchPlans_MergesNoteAndTaskSources(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	at := func(min int) string { return core.FormatTime(base.Add(time.Duration(min) * time.Minute)) }

	// Bi-sourced: note + tasks, same (project, slug).
	insertSearchNote(t, db, "01N1", "Console search plan", "console-search-plan", "seam",
		`["plan:console-search"]`, at(1))
	insertSearchTask(t, db, "01T1", "seam", "step one", "console-search", at(5))

	// Task-only plan.
	insertSearchTask(t, db, "01T2", "seam", "lonely step", "search-orphan", at(3))

	// Unrelated.
	insertSearchTask(t, db, "01T3", "seam", "chore", "billing", at(4))

	got, err := SearchPlans(ctx, db, "search", 20)
	require.NoError(t, err)
	require.Len(t, got, 2, "console-search must collapse to one row; billing must not match")

	bySlug := map[string]PlanSearchRow{}
	for _, r := range got {
		bySlug[r.Slug] = r
	}
	merged, ok := bySlug["console-search"]
	require.True(t, ok)
	require.Equal(t, "seam", merged.Project)
	require.Equal(t, "Console search plan", merged.Title, "the note title beats the slug fallback")
	require.Equal(t, at(5), core.FormatTime(merged.Updated), "the newer source wins Updated")

	orphan, ok := bySlug["search-orphan"]
	require.True(t, ok)
	require.Equal(t, "search-orphan", orphan.Title, "a task-only plan falls back to its slug")

	// Newest-updated first across the merged set.
	require.Equal(t, "console-search", got[0].Slug)
}

// The same slug under two projects is two plans, not one.
func TestSearchPlans_SameSlugDistinctProjects(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := core.FormatTime(time.Now().UTC())

	insertSearchTask(t, db, "01PA1", "alpha", "step", "shared-plan", now)
	insertSearchTask(t, db, "01PB1", "beta", "step", "shared-plan", now)

	got, err := SearchPlans(ctx, db, "shared", 20)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.ElementsMatch(t, []string{"alpha", "beta"}, []string{got[0].Project, got[1].Project})
}

func TestSearchPlans_Limit(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)

	for i, slug := range []string{"plan-a", "plan-b", "plan-c"} {
		insertSearchTask(t, db, "01L"+slug, "seam", "step",
			slug, core.FormatTime(base.Add(time.Duration(i)*time.Minute)))
	}
	got, err := SearchPlans(ctx, db, "plan-", 2)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, "plan-c", got[0].Slug, "the limit cuts the tail, not the head")
}

func TestSearchPlans_ExactSlugRanksBeforeTitleMatchAndLimit(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)

	insertSearchTask(t, db, "01EXACT", "seam", "old exact step", "target-plan", core.FormatTime(base))
	insertSearchNote(t, db, "01TITLE", "Newer target-plan narrative", "other-plan-note", "seam",
		`["plan:other-plan"]`, core.FormatTime(base.Add(time.Hour)))

	got, err := SearchPlans(ctx, db, "target-plan", 1)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "target-plan", got[0].Slug,
		"an exact stable slug must survive a limit ahead of a newer title-only match")
}

// The console's starred rule runs in the query, before LIMIT: StarredOnly
// keeps a starred row the plain order ranks last, and StarredFirst gives it the
// first slot, whatever the window and order would have cut.
func TestSearchFiltered_StarredRuleRunsBeforeLimit(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	at := func(minutes int) string { return core.FormatTime(now.Add(-time.Duration(minutes) * time.Minute)) }
	for i := range 5 {
		insertSearchTask(t, db, fmt.Sprintf("01STAR%d", i), "seam", "starred rule task", "", at(i))
	}
	_, err := db.ExecContext(ctx, `UPDATE tasks SET favorite = 1 WHERE id = '01STAR4'`)
	require.NoError(t, err)

	plain, err := SearchTasksFiltered(ctx, db, "starred rule", SearchFilter{}, 2)
	require.NoError(t, err)
	require.Equal(t, []string{"01STAR0", "01STAR1"}, []string{plain[0].ID, plain[1].ID})

	only, err := SearchTasksFiltered(ctx, db, "starred rule", SearchFilter{Starred: StarredOnly}, 2)
	require.NoError(t, err)
	require.Len(t, only, 1)
	require.Equal(t, "01STAR4", only[0].ID)

	first, err := SearchTasksFiltered(ctx, db, "starred rule", SearchFilter{Starred: StarredFirst}, 2)
	require.NoError(t, err)
	require.Equal(t, []string{"01STAR4", "01STAR0"}, []string{first[0].ID, first[1].ID})

	none, err := SearchTasksFiltered(ctx, db, "starred rule", SearchFilter{Since: now.Add(-2 * time.Minute), Starred: StarredOnly}, 2)
	require.NoError(t, err)
	require.Empty(t, none, "the window still applies to a starred row")

	// Plans merge their sources first, then take the rule.
	insertSearchNote(t, db, "01PN1", "Rule plan", "rule-plan-a", "seam", `["plan:rule-plan-a"]`, at(0))
	insertSearchNote(t, db, "01PN2", "Rule plan starred", "rule-plan-b", "seam", `["plan:rule-plan-b"]`, at(9))
	_, err = db.ExecContext(ctx, `UPDATE notes_index SET favorite = 1 WHERE id = '01PN2'`)
	require.NoError(t, err)
	plans, err := SearchPlansFiltered(ctx, db, "rule plan", SearchFilter{Starred: StarredFirst}, 1)
	require.NoError(t, err)
	require.Equal(t, "rule-plan-b", plans[0].Slug)
	plans, err = SearchPlansFiltered(ctx, db, "rule plan", SearchFilter{Starred: StarredOnly}, 5)
	require.NoError(t, err)
	require.Len(t, plans, 1)

	// The other kinds take the same rule.
	for _, stmt := range []string{
		`INSERT INTO sessions (id, name, project_slug, status, findings, created_at, updated_at) VALUES ('01SS1', 'cc/rule', 'seam', 'active', 'starred rule', '` + at(0) + `', '` + at(0) + `')`,
		`INSERT INTO projects (id, slug, name, created_at, updated_at) VALUES ('01PR1', 'rule-project', 'Rule', '` + at(0) + `', '` + at(0) + `')`,
	} {
		_, err := db.ExecContext(ctx, stmt)
		require.NoError(t, err)
	}
	sessions, err := SearchSessionsFiltered(ctx, db, "starred rule", SearchFilter{Starred: StarredOnly}, 5)
	require.NoError(t, err)
	require.Empty(t, sessions)
	projects, err := SearchProjectsFiltered(ctx, db, "rule-project", SearchFilter{Starred: StarredOnly}, 5)
	require.NoError(t, err)
	require.Empty(t, projects)
	trials, err := SearchTrialsFiltered(ctx, db, "rule", SearchFilter{Starred: StarredOnly}, 5)
	require.NoError(t, err)
	require.Empty(t, trials)
}

// Retired memories have their own console-only lookup. On every search it
// finds one only by exact id or whole name; with text it also finds them by
// body. A live memory never answers here, and the window and starred rule hold.
func TestSearchRetiredMemories_ExactAndText(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	recent, old := core.FormatTime(now.Add(-time.Hour)), core.FormatTime(now.Add(-72*time.Hour))
	insertMemory(t, db, "01RTSUP", "gotcha", "boot-order", "old order", "seam", "the zeppelin order", recent, recent)
	_, err := db.ExecContext(ctx, `UPDATE memories_index SET superseded_by = '01RTLIVE' WHERE id = '01RTSUP'`)
	require.NoError(t, err)
	insertMemory(t, db, "01RTLIVE", "gotcha", "boot-order-v2", "live order", "seam", "the zeppelin order", recent, "")
	insertMemory(t, db, "01RTOLD", "gotcha", "boot-order-older", "older", "seam", "the zeppelin order", old, old)

	exact, err := SearchRetiredMemories(ctx, db, "BOOT-ORDER", SearchFilter{}, true, 20)
	require.NoError(t, err)
	require.Len(t, exact, 1, "the whole name, any case; not a name that only contains it")
	require.Equal(t, "01RTSUP", exact[0].Memory.ID)
	require.Equal(t, IdentifierMatchExactIdentifier, exact[0].Match)
	require.Equal(t, "01RTLIVE", exact[0].Memory.SupersededBy)

	byID, err := SearchRetiredMemories(ctx, db, "01rtold", SearchFilter{}, true, 20)
	require.NoError(t, err)
	require.Len(t, byID, 1)
	require.Equal(t, IdentifierMatchExactID, byID[0].Match)

	none, err := SearchRetiredMemories(ctx, db, "zeppelin", SearchFilter{}, true, 20)
	require.NoError(t, err)
	require.Empty(t, none, "no text match without Include archived")

	text, err := SearchRetiredMemories(ctx, db, "zeppelin", SearchFilter{}, false, 20)
	require.NoError(t, err)
	ids := []string{}
	for _, h := range text {
		ids = append(ids, h.Memory.ID)
		require.Contains(t, h.Snippet, SnippetStartMark+"zeppelin"+SnippetEndMark)
	}
	require.ElementsMatch(t, []string{"01RTSUP", "01RTOLD"}, ids, "never the live one")

	named, err := SearchRetiredMemories(ctx, db, "boot-order", SearchFilter{}, false, 20)
	require.NoError(t, err)
	require.Len(t, named, 2, "with text on, part of a name counts too")
	require.Equal(t, "01RTSUP", named[0].Memory.ID, "the exact name first")

	windowed, err := SearchRetiredMemories(ctx, db, "zeppelin", SearchFilter{Since: now.Add(-24 * time.Hour)}, false, 20)
	require.NoError(t, err)
	require.Len(t, windowed, 1)
	require.Equal(t, "01RTSUP", windowed[0].Memory.ID)

	_, err = db.ExecContext(ctx, `UPDATE memories_index SET favorite = 1 WHERE id = '01RTOLD'`)
	require.NoError(t, err)
	starred, err := SearchRetiredMemories(ctx, db, "zeppelin", SearchFilter{Starred: StarredOnly}, false, 20)
	require.NoError(t, err)
	require.Len(t, starred, 1)
	require.Equal(t, "01RTOLD", starred[0].Memory.ID)
	first, err := SearchRetiredMemories(ctx, db, "zeppelin", SearchFilter{Starred: StarredFirst}, false, 1)
	require.NoError(t, err)
	require.Equal(t, "01RTOLD", first[0].Memory.ID)
}

// The identifier lane applies the caller's peer_auth reach before its LIMIT,
// like the fts leg: a locked project's exact match neither answers nor takes
// the depth.
func TestSearchKnowledgeIdentifiers_ReachBeforeLimit(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC()
	insertMemory(t, db, "01LOCKED", "gotcha", "reach-name-locked", "d", "example-locked", "body", core.FormatTime(now), "")
	insertMemory(t, db, "01OPEN", "gotcha", "reach-name-open", "d", "example-open", "body", core.FormatTime(now.Add(-time.Hour)), "")
	p, err := access.New(config.PeerAuth{Enabled: true, Owner: "owner", ForceLocked: []string{"example-locked"}},
		func(name string) (access.Account, error) { return access.Account{UID: 501, Name: name}, nil })
	require.NoError(t, err)
	ctx := access.WithView(context.Background(), p.OwnerView())

	hits, err := SearchKnowledgeIdentifiersSince(ctx, db, "reach-name", []string{"memory"}, nil, time.Time{}, 1)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	require.Equal(t, "01OPEN", hits[0].ItemID)

	hits, err = SearchKnowledgeIdentifiersSince(context.Background(), db, "reach-name", []string{"memory"}, nil, time.Time{}, 5)
	require.NoError(t, err)
	require.Len(t, hits, 2, "with no View, both")
}

// The starred lane's reads: the starred set of one kind (active, in the
// window, in reach), a match restricted to it, and its vectors.
func TestStarredLaneReads(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	insertMemory(t, db, "01SLA", "gotcha", "lane-a", "d", "seam", "starred lane text", core.FormatTime(now), "")
	insertMemory(t, db, "01SLB", "gotcha", "lane-b", "d", "seam", "starred lane text", core.FormatTime(now), core.FormatTime(now))
	insertMemory(t, db, "01SLC", "gotcha", "lane-c", "d", "seam", "starred lane text", core.FormatTime(now), "")
	insertSearchNote(t, db, "01SLN", "Lane note", "lane-note", "seam", `[]`, core.FormatTime(now))
	_, err := db.ExecContext(ctx, `UPDATE memories_index SET favorite = 1 WHERE id IN ('01SLA', '01SLB')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `UPDATE notes_index SET favorite = 1 WHERE id = '01SLN'`)
	require.NoError(t, err)

	mems, err := StarredKnowledge(ctx, db, core.ItemKindMemory, nil, time.Time{})
	require.NoError(t, err)
	require.Len(t, mems, 1, "the archived starred memory is not live")
	require.Equal(t, StarredItem{ID: "01SLA", Kind: "memory", Identifier: "lane-a", Project: "seam", Updated: mems[0].Updated}, mems[0])
	notes, err := StarredKnowledge(ctx, db, core.ItemKindNote, []string{"seam"}, now.Add(-time.Hour))
	require.NoError(t, err)
	require.Len(t, notes, 1)
	require.Equal(t, "lane-note", notes[0].Identifier)
	_, err = StarredKnowledge(ctx, db, "task", nil, time.Time{})
	require.Error(t, err)

	hits, err := FTSSearchSnippetsWithin(ctx, db, "starred lane", []string{"01SLA", "01SLN"})
	require.NoError(t, err)
	require.Len(t, hits, 1, "only the given ids, and only those that match")
	require.Equal(t, "01SLA", hits[0].ItemID)
	require.Contains(t, hits[0].Snippet, SnippetStartMark)
	hits, err = FTSSearchSnippetsWithin(ctx, db, "x", []string{"01SLA"})
	require.NoError(t, err)
	require.Empty(t, hits)

	require.NoError(t, UpsertEmbedding(ctx, db, "01SLA", "memory", "m1", []float32{1, 0}))
	require.NoError(t, UpsertEmbedding(ctx, db, "01SLC", "memory", "m2", []float32{0, 1}))
	vecs, err := EmbeddingsFor(ctx, db, "m1", []string{"01SLA", "01SLC", "01NONE"})
	require.NoError(t, err)
	require.Equal(t, map[string][]float32{"01SLA": {1, 0}}, vecs, "one model's vectors, for the given ids")
}
