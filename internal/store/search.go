// Human-facing search over stable identifiers and structured entities. Memories
// and notes still get their text candidates from FTSSearch (fused with semantic
// hits by internal/retrieve), but their ids, memory names, and note slugs are
// resolved here from the index mirrors so an exact reference cannot be lost in
// tokenized FTS results. Tasks, sessions, projects, plans, and trials match with
// LIKE over their own columns.
//
// Tasks, trials, and sessions DO have FTS rows now (store/index_work.go), which
// is what recall searches. The console keeps its LIKE queries anyway: they are
// per-entity sections that need whole typed rows with their structured filters
// and orderings, not a fused rank across kinds. The predicates here cover the
// same columns that indexer feeds FTS, so the two surfaces agree on what a query
// can find even though they rank differently.
//
// Every query takes the search text as a bound parameter escaped through
// escapeLikePrefix, so a literal % or _ matches itself rather than acting as a
// wildcard.
//
// The console's own calls go through the *Filtered forms, which take a
// SearchFilter: the time window and the starred rule run inside each query,
// before its LIMIT, so a cap can never cut a starred match in favor of an
// unstarred one, and "starred only" never narrows a list that was already cut.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
)

// IdentifierMatchKind describes how a search query matched an entity's stable
// identifier. The order is significant: lower priorities are stronger and are
// promoted ahead of ordinary text/semantic matches by the console search.
type IdentifierMatchKind string

const (
	IdentifierMatchExactID         IdentifierMatchKind = "exact_id"
	IdentifierMatchExactIdentifier IdentifierMatchKind = "exact_identifier"
	IdentifierMatchIDPrefix        IdentifierMatchKind = "id_prefix"
	IdentifierMatchIdentifier      IdentifierMatchKind = "identifier"
)

const minSearchIDPrefix = 8

// SearchStarred is how a console search treats starred rows. Every value is
// applied inside the query, before its LIMIT.
type SearchStarred int

const (
	// StarredAny keeps each query's own order: the plain and *Since forms.
	StarredAny SearchStarred = iota
	// StarredFirst orders starred rows ahead of the rest, then the query's own
	// order: the console's relevance rule, applied before the cap.
	StarredFirst
	// StarredOnly keeps starred rows alone (the page's "Starred only").
	StarredOnly
)

// SearchFilter narrows and orders the console's search queries. The zero
// value is all time, any star.
type SearchFilter struct {
	Since   time.Time // zero = all time; otherwise updated (a trial: created) at or after
	Starred SearchStarred
}

// starredSQL renders f.Starred over a trusted favorite column: the predicate
// StarredOnly appends to WHERE, and the leading ORDER BY key StarredFirst
// puts before the query's own order (with its trailing comma).
func starredSQL(column string, starred SearchStarred) (predicate, order string) {
	switch starred {
	case StarredOnly:
		return ` AND ` + column + ` = 1`, ""
	case StarredFirst:
		return "", column + ` DESC, `
	default:
		return "", ""
	}
}

// IdentifierMatchPriority returns the relevance tier for a match kind. The
// empty/unknown value is deliberately last so callers can stable-sort ordinary
// search results after every recognized identifier match.
func IdentifierMatchPriority(kind IdentifierMatchKind) int {
	switch kind {
	case IdentifierMatchExactID:
		return 0
	case IdentifierMatchExactIdentifier:
		return 1
	case IdentifierMatchIDPrefix:
		return 2
	case IdentifierMatchIdentifier:
		return 3
	default:
		return 4
	}
}

// IDIdentifierMatch classifies a query against an entity id. Full ids match
// case-insensitively. A partial match is accepted only for an 8-25 character
// Crockford-base32 ULID prefix; this prevents a query such as "01" from
// flooding search with nearly every recently-created entity.
func IDIdentifierMatch(query, id string) IdentifierMatchKind {
	query = strings.TrimSpace(query)
	if query == "" || id == "" {
		return ""
	}
	if strings.EqualFold(query, id) {
		return IdentifierMatchExactID
	}
	if prefix, ok := searchIDPrefix(query); ok && strings.HasPrefix(strings.ToUpper(id), prefix) {
		return IdentifierMatchIDPrefix
	}
	return ""
}

// NaturalIdentifierMatch classifies a query against a human-facing identifier
// such as a memory name, note slug, project slug, or plan slug.
func NaturalIdentifierMatch(query, identifier string) IdentifierMatchKind {
	query = strings.TrimSpace(query)
	if query == "" || identifier == "" {
		return ""
	}
	if strings.EqualFold(query, identifier) {
		return IdentifierMatchExactIdentifier
	}
	if strings.Contains(strings.ToLower(identifier), strings.ToLower(query)) {
		return IdentifierMatchIdentifier
	}
	return ""
}

// searchIDPrefix returns an upper-case, validated partial ULID. Full 26-byte
// ids are exact matches and therefore intentionally return ok=false here.
func searchIDPrefix(query string) (string, bool) {
	prefix := strings.ToUpper(strings.TrimSpace(query))
	if len(prefix) < minSearchIDPrefix || len(prefix) >= 26 {
		return "", false
	}
	if prefix[0] < '0' || prefix[0] > '7' {
		return "", false
	}
	const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	for _, r := range prefix {
		if !strings.ContainsRune(crockford, r) {
			return "", false
		}
	}
	return prefix, true
}

// idSearchSQL returns the ID predicate and relevance-order expression for a
// trusted column name. Arguments are split because WHERE placeholders occur
// before ORDER BY placeholders in the final query.
func idSearchSQL(column, query string) (predicate string, predicateArgs []any, order string, orderArgs []any) {
	exact := column + ` COLLATE NOCASE = ?`
	predicate = exact
	predicateArgs = append(predicateArgs, query)
	order = `CASE WHEN ` + exact + ` THEN 0`
	orderArgs = append(orderArgs, query)
	if prefix, ok := searchIDPrefix(query); ok {
		prefixPattern := escapeLikePrefix(prefix) + "%"
		prefixExpr := column + ` COLLATE NOCASE LIKE ? ESCAPE '\'`
		predicate += ` OR ` + prefixExpr
		predicateArgs = append(predicateArgs, prefixPattern)
		order += ` WHEN ` + prefixExpr + ` THEN 1`
		orderArgs = append(orderArgs, prefixPattern)
	}
	order += ` ELSE 2 END`
	return predicate, predicateArgs, order, orderArgs
}

// KnowledgeIdentifierHit is a memory/note candidate matched through its stable
// id or natural identifier rather than through FTS or an embedding.
type KnowledgeIdentifierHit struct {
	ItemID  string
	Kind    string
	Match   IdentifierMatchKind
	Updated time.Time
}

// SearchKnowledgeIdentifiersSince searches the index mirrors for memory names,
// note slugs, and memory/note ids. Identifier predicates, project scope,
// validity, the caller's peer_auth reach, and the time window all run before
// the limit so an exact match cannot be crowded out by unrelated FTS
// candidates.
func SearchKnowledgeIdentifiersSince(ctx context.Context, db *sql.DB, query string, kinds, projects []string, since time.Time, limit int) ([]KnowledgeIdentifierHit, error) {
	limit = searchLimit(limit)
	wants := func(kind string) bool {
		return len(kinds) == 0 || slices.Contains(kinds, kind)
	}

	f := SearchFilter{Since: since}
	var hits []KnowledgeIdentifierHit
	if wants("memory") {
		rows, err := searchKnowledgeIdentifierTable(ctx, db, knowledgeIdentifierTable{
			table: "memories_index", kind: "memory", identifier: "name",
			validity: "invalid_at IS NULL",
		}, query, projects, f, limit)
		if err != nil {
			return nil, fmt.Errorf("store.SearchKnowledgeIdentifiersSince: memories: %w", err)
		}
		hits = append(hits, rows...)
	}
	if wants("note") {
		rows, err := searchKnowledgeIdentifierTable(ctx, db, knowledgeIdentifierTable{
			table: "notes_index", kind: "note", identifier: "slug",
		}, query, projects, f, limit)
		if err != nil {
			return nil, fmt.Errorf("store.SearchKnowledgeIdentifiersSince: notes: %w", err)
		}
		hits = append(hits, rows...)
	}

	sort.Slice(hits, func(i, j int) bool {
		pi := IdentifierMatchPriority(hits[i].Match)
		pj := IdentifierMatchPriority(hits[j].Match)
		if pi != pj {
			return pi < pj
		}
		if !hits[i].Updated.Equal(hits[j].Updated) {
			return hits[i].Updated.After(hits[j].Updated)
		}
		return hits[i].ItemID < hits[j].ItemID
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

type knowledgeIdentifierTable struct {
	table      string
	kind       string
	identifier string
	validity   string
	// exact keeps only the two exact lanes (the full id, the whole name): the
	// retired-memory lookup that runs on every search.
	exact bool
}

func searchKnowledgeIdentifierTable(ctx context.Context, db *sql.DB, table knowledgeIdentifierTable, query string, projects []string, f SearchFilter, limit int) ([]KnowledgeIdentifierHit, error) {
	prefixExpr := ""
	prefixPattern := ""
	if prefix, ok := searchIDPrefix(query); ok && !table.exact {
		prefixExpr = ` OR id COLLATE NOCASE LIKE ? ESCAPE '\'`
		prefixPattern = escapeLikePrefix(prefix) + "%"
	}

	nameExpr, nameArg := table.identifier+` LIKE ? ESCAPE '\'`, likeContains(query)
	if table.exact {
		nameExpr, nameArg = table.identifier+` = ? COLLATE NOCASE`, query
	}
	sqlStr := `SELECT id, ` + table.identifier + `, updated_at FROM ` + table.table + ` WHERE (` +
		`id COLLATE NOCASE = ? OR ` + nameExpr + prefixExpr + `)`
	args := []any{query, nameArg}
	if prefixExpr != "" {
		args = append(args, prefixPattern)
	}
	if table.validity != "" {
		sqlStr += ` AND ` + table.validity
	}
	if len(projects) > 0 {
		sqlStr += ` AND project IN (` + placeholders(len(projects)) + `)`
		for _, project := range projects {
			args = append(args, project)
		}
	}
	// The caller's peer_auth reach, before the LIMIT like every other
	// predicate here, so a locked project's matches cannot take the depth (the
	// fts leg filters the same way, accessFilter).
	if clause, extra := accessFilter(ctx, "project"); clause != "" {
		sqlStr += clause
		args = append(args, extra...)
	}
	starredWhere, starredOrder := starredSQL("favorite", f.Starred)
	sqlStr += starredWhere
	sqlStr, args = addSearchSince(sqlStr, args, "updated_at", f.Since)
	sqlStr += ` ORDER BY ` + starredOrder + `CASE WHEN id COLLATE NOCASE = ? THEN 0 WHEN ` +
		table.identifier + ` COLLATE NOCASE = ? THEN 1`
	args = append(args, query, query)
	if prefixExpr != "" {
		sqlStr += ` WHEN id COLLATE NOCASE LIKE ? ESCAPE '\' THEN 2`
		args = append(args, prefixPattern)
	}
	sqlStr += ` ELSE 3 END, updated_at DESC, id`
	// Each kind is independently bounded before the final cross-kind merge.
	// The shared final limit still decides the externally visible result set.
	sqlStr += ` LIMIT ?`
	args = append(args, limit)

	rows, err := db.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []KnowledgeIdentifierHit
	for rows.Next() {
		var id, identifier, updated string
		if err := rows.Scan(&id, &identifier, &updated); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		at, err := core.ParseTime(updated)
		if err != nil {
			return nil, fmt.Errorf("updated_at: %w", err)
		}
		match := IDIdentifierMatch(query, id)
		if match == "" {
			match = NaturalIdentifierMatch(query, identifier)
		}
		if match == "" || (table.exact && match != IdentifierMatchExactID && match != IdentifierMatchExactIdentifier) {
			continue
		}
		out = append(out, KnowledgeIdentifierHit{ItemID: id, Kind: table.kind, Match: match, Updated: at})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// likeContains builds the bound argument for a case-insensitive "contains"
// LIKE, with the needle's metacharacters escaped so it matches literally under
// `ESCAPE '\'`.
func likeContains(s string) string {
	return "%" + escapeLikePrefix(s) + "%"
}

// searchLimit floors an unset/negative limit at a sane page size.
func searchLimit(limit int) int {
	if limit <= 0 {
		return 20
	}
	return limit
}

// addSearchSince appends an inclusive timestamp predicate for a trusted column
// name. Callers pass only schema constants from this file; user input remains a
// bound argument.
func addSearchSince(sqlStr string, args []any, column string, since time.Time) (string, []any) {
	if since.IsZero() {
		return sqlStr, args
	}
	return sqlStr + " AND " + column + " >= ?", append(args, core.FormatTime(since.UTC()))
}

// SearchTasks returns tasks whose title or body contains q, newest-updated
// first. An exact id also matches, so pasting a task id from a log finds its
// task.
//
// The body is searched because that is where a task's acceptance criteria and
// its record of what was already tried live; recall reaches that text through
// FTS (store/index_work.go), and a console section that saw only titles would
// quietly find less than the agent-facing tool on the same query.
func SearchTasks(ctx context.Context, db *sql.DB, q string, limit int) ([]core.Task, error) {
	return searchTasks(ctx, db, q, SearchFilter{}, limit, "store.SearchTasks")
}

// SearchTasksSince is SearchTasks restricted to tasks updated at or after
// since. A zero since keeps the all-time behavior.
func SearchTasksSince(ctx context.Context, db *sql.DB, q string, since time.Time, limit int) ([]core.Task, error) {
	return searchTasks(ctx, db, q, SearchFilter{Since: since}, limit, "store.SearchTasksSince")
}

// SearchTasksFiltered is SearchTasks under a console filter: the window and
// the starred rule run before the LIMIT.
func SearchTasksFiltered(ctx context.Context, db *sql.DB, q string, f SearchFilter, limit int) ([]core.Task, error) {
	return searchTasks(ctx, db, q, f, limit, "store.SearchTasksFiltered")
}

func searchTasks(ctx context.Context, db *sql.DB, q string, f SearchFilter, limit int, op string) ([]core.Task, error) {
	idPredicate, idArgs, idOrder, idOrderArgs := idSearchSQL("id", q)
	needle := likeContains(q)
	starredWhere, starredOrder := starredSQL("favorite", f.Starred)
	sqlStr := `SELECT ` + taskCols + ` FROM tasks
		WHERE (title LIKE ? ESCAPE '\' OR body LIKE ? ESCAPE '\' OR ` + idPredicate + `)` + starredWhere
	args := append([]any{needle, needle}, idArgs...)
	sqlStr, args = addSearchSince(sqlStr, args, "updated_at", f.Since)
	sqlStr += ` ORDER BY ` + starredOrder + idOrder + `, updated_at DESC, id DESC LIMIT ?`
	args = append(args, idOrderArgs...)
	args = append(args, searchLimit(limit))
	rows, err := db.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	tasks, err := scanTasksWithDeps(ctx, db, rows)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	return tasks, nil
}

// SearchSessions returns sessions whose name or findings contain q,
// newest-updated first. An exact id also matches.
//
// Findings are the whole reason a session is worth searching -- the handoff
// prose the next agent inherits -- and recall now reaches them; matching on the
// name alone would leave the console able to find only sessions whose generated
// handle happened to contain the query.
func SearchSessions(ctx context.Context, db *sql.DB, q string, limit int) ([]core.Session, error) {
	return searchSessions(ctx, db, q, SearchFilter{}, limit, "store.SearchSessions")
}

// SearchSessionsSince is SearchSessions restricted to sessions updated at or
// after since. A zero since keeps the all-time behavior.
func SearchSessionsSince(ctx context.Context, db *sql.DB, q string, since time.Time, limit int) ([]core.Session, error) {
	return searchSessions(ctx, db, q, SearchFilter{Since: since}, limit, "store.SearchSessionsSince")
}

// SearchSessionsFiltered is SearchSessions under a console filter: the window
// and the starred rule run before the LIMIT.
func SearchSessionsFiltered(ctx context.Context, db *sql.DB, q string, f SearchFilter, limit int) ([]core.Session, error) {
	return searchSessions(ctx, db, q, f, limit, "store.SearchSessionsFiltered")
}

func searchSessions(ctx context.Context, db *sql.DB, q string, f SearchFilter, limit int, op string) ([]core.Session, error) {
	idPredicate, idArgs, idOrder, idOrderArgs := idSearchSQL("id", q)
	needle := likeContains(q)
	starredWhere, starredOrder := starredSQL("favorite", f.Starred)
	sqlStr := `SELECT ` + sessionCols + ` FROM sessions
		WHERE (name LIKE ? ESCAPE '\' OR findings LIKE ? ESCAPE '\' OR ` + idPredicate + `)` + starredWhere
	args := append([]any{needle, needle}, idArgs...)
	sqlStr, args = addSearchSince(sqlStr, args, "updated_at", f.Since)
	sqlStr += ` ORDER BY ` + starredOrder + idOrder + `, updated_at DESC, id DESC LIMIT ?`
	args = append(args, idOrderArgs...)
	args = append(args, searchLimit(limit))
	rows, err := db.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	defer func() { _ = rows.Close() }()
	var out []core.Session
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: scan: %w", op, err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	return out, nil
}

// SearchProjects returns projects whose slug or display name contains q,
// alphabetically by slug (projects are few and stable, so a name order reads
// better than a recency one).
func SearchProjects(ctx context.Context, db *sql.DB, q string, limit int) ([]core.Project, error) {
	return searchProjects(ctx, db, q, SearchFilter{}, limit, "store.SearchProjects")
}

// SearchProjectsSince is SearchProjects restricted to projects updated at or
// after since. A zero since keeps the all-time behavior.
func SearchProjectsSince(ctx context.Context, db *sql.DB, q string, since time.Time, limit int) ([]core.Project, error) {
	return searchProjects(ctx, db, q, SearchFilter{Since: since}, limit, "store.SearchProjectsSince")
}

// SearchProjectsFiltered is SearchProjects under a console filter: the window
// and the starred rule run before the LIMIT.
func SearchProjectsFiltered(ctx context.Context, db *sql.DB, q string, f SearchFilter, limit int) ([]core.Project, error) {
	return searchProjects(ctx, db, q, f, limit, "store.SearchProjectsFiltered")
}

func searchProjects(ctx context.Context, db *sql.DB, q string, f SearchFilter, limit int, op string) ([]core.Project, error) {
	needle := likeContains(q)
	starredWhere, starredOrder := starredSQL("favorite", f.Starred)
	sqlStr := `SELECT ` + projectCols + ` FROM projects
		WHERE (slug LIKE ? ESCAPE '\' OR name LIKE ? ESCAPE '\')` + starredWhere
	args := []any{needle, needle}
	sqlStr, args = addSearchSince(sqlStr, args, "updated_at", f.Since)
	sqlStr += ` ORDER BY ` + starredOrder + `CASE WHEN slug COLLATE NOCASE = ? THEN 0 ELSE 1 END, slug LIMIT ?`
	args = append(args, q)
	args = append(args, searchLimit(limit))
	rows, err := db.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	defer func() { _ = rows.Close() }()
	var out []core.Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: scan: %w", op, err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	return out, nil
}

// SearchTrials returns trials whose title, lab, or expected-vs-actual prose
// contains q, newest first. An exact id also matches, so pasting a trial id from
// a log finds its trial.
//
// The prose fields are searched for the same reason recall indexes them: a lab's
// value to a later reader is what was tried and what happened, and neither is in
// the title.
func SearchTrials(ctx context.Context, db *sql.DB, q string, limit int) ([]core.Trial, error) {
	return searchTrials(ctx, db, q, SearchFilter{}, limit, "store.SearchTrials")
}

// SearchTrialsSince is SearchTrials restricted to trials created at or after
// since. A zero since keeps the all-time behavior.
func SearchTrialsSince(ctx context.Context, db *sql.DB, q string, since time.Time, limit int) ([]core.Trial, error) {
	return searchTrials(ctx, db, q, SearchFilter{Since: since}, limit, "store.SearchTrialsSince")
}

// SearchTrialsFiltered is SearchTrials under a console filter: the window (on
// a trial's creation time) and the starred rule run before the LIMIT.
func SearchTrialsFiltered(ctx context.Context, db *sql.DB, q string, f SearchFilter, limit int) ([]core.Trial, error) {
	return searchTrials(ctx, db, q, f, limit, "store.SearchTrialsFiltered")
}

func searchTrials(ctx context.Context, db *sql.DB, q string, f SearchFilter, limit int, op string) ([]core.Trial, error) {
	needle := likeContains(q)
	idPredicate, idArgs, idOrder, idOrderArgs := idSearchSQL("id", q)
	starredWhere, starredOrder := starredSQL("favorite", f.Starred)
	sqlStr := `SELECT ` + trialCols + ` FROM trials
		WHERE (title LIKE ? ESCAPE '\' OR lab LIKE ? ESCAPE '\'
		       OR changes LIKE ? ESCAPE '\' OR expected LIKE ? ESCAPE '\'
		       OR actual LIKE ? ESCAPE '\' OR ` + idPredicate + `)` + starredWhere
	args := append([]any{needle, needle, needle, needle, needle}, idArgs...)
	sqlStr, args = addSearchSince(sqlStr, args, "created_at", f.Since)
	sqlStr += ` ORDER BY ` + starredOrder + idOrder + `, created_at DESC, id DESC LIMIT ?`
	args = append(args, idOrderArgs...)
	args = append(args, searchLimit(limit))
	rows, err := db.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	defer func() { _ = rows.Close() }()
	var out []core.Trial
	for rows.Next() {
		tr, err := scanTrial(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: scan: %w", op, err)
		}
		out = append(out, tr)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	return out, nil
}

// PlanSearchRow is one plan hit. A plan is a composition, not a table, so a row
// is identified by (Project, Slug); Title is the best label found -- a matching
// note's title, or the slug itself when only tasks carry it.
type PlanSearchRow struct {
	Slug    string
	Project string
	Title   string
	// Favorite is true when any of the plan's tagged notes is favorited. The
	// authoritative flag lives on the plan's primary note, but this query cannot
	// cheaply identify the primary; the two only disagree after deliberate
	// hand-editing of a secondary note's frontmatter.
	Favorite bool
	Updated  time.Time
	// Account is the restricted local account (peer_auth.restricted) that
	// wrote the plan, "" for the owner: the account of the note whose title
	// labels it, or -- a plan only tasks carry -- of its tasks when one
	// account wrote them all.
	Account string
}

// SearchPlans returns plans whose slug or narrative-note title contains q,
// newest-updated first.
//
// Plans are bi-sourced (a note tagged plan:<slug>, and tasks carrying
// plan_slug), and either source alone can carry a match: a plan whose steps
// exist but whose note does not, or vice versa. Both are queried and merged
// deduped by (project, slug), keeping the newer Updated and preferring a note's
// title over the slug fallback -- the same merge the Plans screen does, done
// here so a search hit cannot disagree with the page it links to.
func SearchPlans(ctx context.Context, db *sql.DB, q string, limit int) ([]PlanSearchRow, error) {
	return searchPlans(ctx, db, q, SearchFilter{}, limit, "store.SearchPlans")
}

// SearchPlansSince is SearchPlans restricted to matching plan sources updated
// at or after since. Plans are merged before the bound is applied, so either a
// matching narrative or matching task can keep the composition in-window.
func SearchPlansSince(ctx context.Context, db *sql.DB, q string, since time.Time, limit int) ([]PlanSearchRow, error) {
	return searchPlans(ctx, db, q, SearchFilter{Since: since}, limit, "store.SearchPlansSince")
}

// SearchPlansFiltered is SearchPlans under a console filter. A plan is merged
// from its sources before any bound, so the window, the starred rule (a plan
// is starred when any of its tagged notes is, PlanSearchRow.Favorite) and the
// LIMIT all apply to the merged row.
func SearchPlansFiltered(ctx context.Context, db *sql.DB, q string, f SearchFilter, limit int) ([]PlanSearchRow, error) {
	return searchPlans(ctx, db, q, f, limit, "store.SearchPlansFiltered")
}

func searchPlans(ctx context.Context, db *sql.DB, q string, f SearchFilter, limit int, op string) ([]PlanSearchRow, error) {
	since := f.Since
	lim := searchLimit(limit)
	needle := likeContains(q)

	// Notes: a plan:<slug> tag, matched on the note's title or on the tag's own
	// slug suffix. json_each is the tag-array reader NotesByTagPrefix uses.
	//
	// Both this query and the tasks one below are drained inside a closure whose
	// defer closes them, so the note rows are released before the task query
	// opens. sqlclosecheck only recognizes a defer in the same function scope as
	// the query and reports both as leaks; a plain defer would instead hold the
	// note rows open across the second query, which is the thing worth avoiding.
	//nolint:sqlclosecheck // closed by the closure's defer, see above
	noteRows, err := db.QueryContext(ctx, `
		SELECT je.value, n.project, n.title, n.favorite, n.updated_at, n.account
		FROM notes_index n, json_each(n.tags) je
		WHERE je.value LIKE 'plan:%' ESCAPE '\'
		  AND (n.title LIKE ? ESCAPE '\' OR je.value LIKE ? ESCAPE '\')
		ORDER BY n.updated_at DESC, n.id DESC`,
		needle, needle)
	if err != nil {
		return nil, fmt.Errorf("%s: notes: %w", op, err)
	}
	byKey := make(map[string]*PlanSearchRow)
	var order []string
	upsert := func(project, slug, title string, favorite bool, updated time.Time, account string) {
		if slug == "" {
			return
		}
		key := project + "\x00" + slug
		row, ok := byKey[key]
		if !ok {
			byKey[key] = &PlanSearchRow{Slug: slug, Project: project, Title: title, Favorite: favorite, Updated: updated,
				Account: account}
			order = append(order, key)
			return
		}
		if updated.After(row.Updated) {
			row.Updated = updated
		}
		row.Favorite = row.Favorite || favorite
		// A note title is a real label; the task path can only offer the slug.
		// The writer is the labeling note's.
		if row.Title == row.Slug && title != "" && title != slug {
			row.Title, row.Account = title, account
		}
	}
	err = func() error {
		defer func() { _ = noteRows.Close() }()
		for noteRows.Next() {
			var tag, project, title, updated, account string
			var favorite bool
			if err := noteRows.Scan(&tag, &project, &title, &favorite, &updated, &account); err != nil {
				return fmt.Errorf("scan: %w", err)
			}
			u, err := core.ParseTime(updated)
			if err != nil {
				return fmt.Errorf("updated_at: %w", err)
			}
			slug := planSlugFromTag(tag)
			if title == "" {
				title = slug
			}
			upsert(project, slug, title, favorite, u, account)
		}
		return noteRows.Err()
	}()
	if err != nil {
		return nil, fmt.Errorf("%s: notes: %w", op, err)
	}

	// Tasks: the plan_slug column itself.
	//nolint:sqlclosecheck // closed by the closure's defer, as above
	taskRows, err := db.QueryContext(ctx, `
		SELECT plan_slug, project_slug, MAX(updated_at),
		       CASE WHEN COUNT(DISTINCT account) = 1 THEN MAX(account) ELSE '' END
		FROM tasks
		WHERE plan_slug != '' AND plan_slug LIKE ? ESCAPE '\'
		GROUP BY plan_slug, project_slug`, needle)
	if err != nil {
		return nil, fmt.Errorf("%s: tasks: %w", op, err)
	}
	err = func() error {
		defer func() { _ = taskRows.Close() }()
		for taskRows.Next() {
			var slug, project, updated, account string
			if err := taskRows.Scan(&slug, &project, &updated, &account); err != nil {
				return fmt.Errorf("scan: %w", err)
			}
			u, err := core.ParseTime(updated)
			if err != nil {
				return fmt.Errorf("updated_at: %w", err)
			}
			upsert(project, slug, slug, false, u, account)
		}
		return taskRows.Err()
	}()
	if err != nil {
		return nil, fmt.Errorf("%s: tasks: %w", op, err)
	}

	out := make([]PlanSearchRow, 0, len(order))
	for _, key := range order {
		row := *byKey[key]
		if !since.IsZero() && row.Updated.Before(since) {
			continue
		}
		if f.Starred == StarredOnly && !row.Favorite {
			continue
		}
		out = append(out, row)
	}
	sortPlanSearchRows(out, q, f.Starred == StarredFirst)
	if len(out) > lim {
		out = out[:lim]
	}
	return out, nil
}

// sortPlanSearchRows promotes natural-identifier matches before title-only
// matches, then orders newest-updated first with a deterministic project/slug
// tiebreak. This ordering runs before LIMIT so an exact slug cannot be crowded
// out by newer notes whose titles merely contain the same text. starredFirst
// puts starred plans ahead of all of that (StarredFirst).
func sortPlanSearchRows(rows []PlanSearchRow, query string, starredFirst bool) {
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if starredFirst && a.Favorite != b.Favorite {
			return a.Favorite
		}
		pi := IdentifierMatchPriority(NaturalIdentifierMatch(query, a.Slug))
		pj := IdentifierMatchPriority(NaturalIdentifierMatch(query, b.Slug))
		if pi != pj {
			return pi < pj
		}
		if !a.Updated.Equal(b.Updated) {
			return a.Updated.After(b.Updated)
		}
		if a.Project != b.Project {
			return a.Project < b.Project
		}
		return a.Slug < b.Slug
	})
}

// planSlugFromTag strips the "plan:" prefix from a tag value. It duplicates
// nothing from internal/plans on purpose: store must not import a package that
// imports store.
func planSlugFromTag(tag string) string {
	const prefix = "plan:"
	if len(tag) <= len(prefix) || tag[:len(prefix)] != prefix {
		return ""
	}
	return tag[len(prefix):]
}

// StarredItem is one starred memory or note a console search may rank: what
// the starred lane needs to place it without loading the item.
type StarredItem struct {
	ID         string
	Kind       string // core.ItemKindMemory | core.ItemKindNote
	Identifier string // the memory's name, the note's slug
	Project    string
	Updated    time.Time
}

// StarredKnowledge lists the starred items of one knowledge kind that a search
// may rank: active memories (or notes), in projects (nil = every project),
// within the caller's peer_auth reach, updated at or after since (zero = all
// time). The owner stars a handful of items, so the list is the whole starred
// set, not a top-K: console search ranks it on its own, which is how a starred
// match can never be cut by a cap (retrieve.SearchInput.Starred).
func StarredKnowledge(ctx context.Context, db *sql.DB, kind string, projects []string, since time.Time) ([]StarredItem, error) {
	var table, identifier, validity string
	switch kind {
	case core.ItemKindMemory:
		table, identifier, validity = "memories_index", "name", " AND invalid_at IS NULL"
	case core.ItemKindNote:
		table, identifier = "notes_index", "slug"
	default:
		return nil, fmt.Errorf("store.StarredKnowledge: unknown kind %q", kind)
	}
	sqlStr := `SELECT id, ` + identifier + `, project, updated_at FROM ` + table + ` WHERE favorite = 1` + validity
	var args []any
	if len(projects) > 0 {
		sqlStr += ` AND project IN (` + placeholders(len(projects)) + `)`
		for _, p := range projects {
			args = append(args, p)
		}
	}
	if clause, extra := accessFilter(ctx, "project"); clause != "" {
		sqlStr += clause
		args = append(args, extra...)
	}
	sqlStr, args = addSearchSince(sqlStr, args, "updated_at", since)
	sqlStr += ` ORDER BY updated_at DESC, id`
	rows, err := db.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return nil, fmt.Errorf("store.StarredKnowledge: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []StarredItem
	for rows.Next() {
		var it StarredItem
		var updated string
		if err := rows.Scan(&it.ID, &it.Identifier, &it.Project, &updated); err != nil {
			return nil, fmt.Errorf("store.StarredKnowledge: scan: %w", err)
		}
		if it.Updated, err = core.ParseTime(updated); err != nil {
			return nil, fmt.Errorf("store.StarredKnowledge: updated_at: %w", err)
		}
		it.Kind = kind
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.StarredKnowledge: %w", err)
	}
	return out, nil
}

// FTSSearchSnippetsWithin is FTSSearchSnippets over the given items alone,
// best-first, with every match among them: the starred lane's lexical leg.
// The caller has already judged each id (validity, scope, window, reach), so
// the query adds nothing but the match.
func FTSSearchSnippetsWithin(ctx context.Context, db *sql.DB, query string, ids []string) ([]SnippetHit, error) {
	match := ftsQuery(query)
	if match == "" || len(ids) == 0 {
		return nil, nil
	}
	args := []any{SnippetStartMark, SnippetEndMark, snippetTokens, match}
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := db.QueryContext(ctx, `SELECT item_id, kind, bm25(fts) AS rank,
		    snippet(fts, -1, ?, ?, ' ... ', ?)
		FROM fts WHERE fts MATCH ? AND item_id IN (`+placeholders(len(ids))+`)
		ORDER BY rank, item_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("store.FTSSearchSnippetsWithin: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var hits []SnippetHit
	for rows.Next() {
		var h SnippetHit
		var rank float64
		if err := rows.Scan(&h.ItemID, &h.Kind, &rank, &h.Snippet); err != nil {
			return nil, fmt.Errorf("store.FTSSearchSnippetsWithin: scan: %w", err)
		}
		h.Score = -rank
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.FTSSearchSnippetsWithin: %w", err)
	}
	return hits, nil
}

// EmbeddingsFor returns the stored vectors of model for the given items, by
// id; an item with no vector (or a corrupt one) is absent. The starred lane's
// semantic leg scores these few directly instead of scanning the corpus.
func EmbeddingsFor(ctx context.Context, db *sql.DB, model string, ids []string) (map[string][]float32, error) {
	out := make(map[string][]float32, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	args := []any{model}
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := db.QueryContext(ctx, `SELECT item_id, vec FROM embeddings
		WHERE model = ? AND item_id IN (`+placeholders(len(ids))+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("store.EmbeddingsFor: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		var blob []byte
		if err := rows.Scan(&id, &blob); err != nil {
			return nil, fmt.Errorf("store.EmbeddingsFor: scan: %w", err)
		}
		if vec := DecodeVector(blob); len(vec) > 0 {
			out[id] = vec
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.EmbeddingsFor: %w", err)
	}
	return out, nil
}

// RetiredMemoryHit is a superseded or archived memory console Search found.
type RetiredMemoryHit struct {
	Memory core.Memory
	// Match is how the query named the memory (its full id, its whole name,
	// an id prefix, part of its name); "" for a text match.
	Match IdentifierMatchKind
	// Snippet is a text match's FTS snippet, sentinel-marked like
	// SnippetHit.Snippet: raw item text, to escape before marking up.
	Snippet string
}

// SearchRetiredMemories finds the inactive memories -- superseded or archived
// -- that console Search shows apart from live knowledge. Every other search
// and recall query drops them by design (FTSSearch); this one exists only for
// the console, so the agent-facing candidate queries never loosen.
//
// exactOnly is the lookup that runs on every search: a memory whose full id
// or whole name is the query, because a direct reference to a retired item
// should find it, with its status. Otherwise ("Include archived") it also
// matches id prefixes, part of a name, and the text through FTS. Identifier
// matches come first, then text matches best-first; f's window and starred
// rule run in each query, before its LIMIT, and the merged list is cut at
// limit. db is the request's handle: a locked project's memories are not on
// it, so they are absent here as everywhere else.
func SearchRetiredMemories(ctx context.Context, db *sql.DB, q string, f SearchFilter, exactOnly bool, limit int) ([]RetiredMemoryHit, error) {
	limit = searchLimit(limit)
	idHits, err := searchKnowledgeIdentifierTable(ctx, db, knowledgeIdentifierTable{
		table: "memories_index", kind: core.ItemKindMemory, identifier: "name",
		validity: "invalid_at IS NOT NULL", exact: exactOnly,
	}, q, nil, f, limit)
	if err != nil {
		return nil, fmt.Errorf("store.SearchRetiredMemories: identifiers: %w", err)
	}
	ids := make([]string, 0, len(idHits))
	match := make(map[string]IdentifierMatchKind, len(idHits))
	for _, h := range idHits {
		ids = append(ids, h.ItemID)
		match[h.ItemID] = h.Match
	}
	snippets := map[string]string{}
	if !exactOnly {
		text, err := searchRetiredText(ctx, db, q, f, limit)
		if err != nil {
			return nil, err
		}
		for _, h := range text {
			if _, seen := match[h.ItemID]; seen {
				continue
			}
			ids = append(ids, h.ItemID)
			snippets[h.ItemID] = h.Snippet
		}
	}
	mems, err := MemoriesByIDs(ctx, db, ids)
	if err != nil {
		return nil, fmt.Errorf("store.SearchRetiredMemories: %w", err)
	}
	out := make([]RetiredMemoryHit, 0, len(ids))
	for _, id := range ids {
		m, ok := mems[id]
		if !ok || m.Active() {
			continue // reactivated, or gone, between the two reads
		}
		out = append(out, RetiredMemoryHit{Memory: m, Match: match[id], Snippet: snippets[id]})
	}
	// Each lane already put its starred rows first; the merge keeps that across
	// the two lanes.
	if f.Starred == StarredFirst {
		sort.SliceStable(out, func(i, j int) bool { return out[i].Memory.Favorite && !out[j].Memory.Favorite })
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// searchRetiredText is the FTS leg of SearchRetiredMemories: the shared fts
// table keeps a full row for an inactive memory, and the join to
// memories_index (on the request's handle, the filtered view) keeps the
// inactive ones the caller may read.
func searchRetiredText(ctx context.Context, db *sql.DB, q string, f SearchFilter, limit int) ([]SnippetHit, error) {
	match := ftsQuery(q)
	if match == "" {
		return nil, nil
	}
	starredWhere, starredOrder := starredSQL("mi.favorite", f.Starred)
	sqlStr := `SELECT fts.item_id, bm25(fts) AS rank, snippet(fts, -1, ?, ?, ' ... ', ?)
		FROM fts JOIN memories_index mi ON mi.id = fts.item_id
		WHERE fts MATCH ? AND fts.kind = 'memory' AND mi.invalid_at IS NOT NULL` + starredWhere
	args := []any{SnippetStartMark, SnippetEndMark, snippetTokens, match}
	sqlStr, args = addSearchSince(sqlStr, args, "mi.updated_at", f.Since)
	sqlStr += ` ORDER BY ` + starredOrder + `rank, fts.item_id LIMIT ?`
	args = append(args, limit)
	rows, err := db.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return nil, fmt.Errorf("store.SearchRetiredMemories: text: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var hits []SnippetHit
	for rows.Next() {
		var h SnippetHit
		var rank float64
		if err := rows.Scan(&h.ItemID, &rank, &h.Snippet); err != nil {
			return nil, fmt.Errorf("store.SearchRetiredMemories: text: scan: %w", err)
		}
		h.Kind, h.Score = core.ItemKindMemory, -rank
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.SearchRetiredMemories: text: %w", err)
	}
	return hits, nil
}
