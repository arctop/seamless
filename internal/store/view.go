package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"

	sqlite "modernc.org/sqlite"
)

// A filtered read handle: the owner's view of the database while peer_auth seals
// projects to other OS accounts.
//
// The console reads the database through roughly a hundred and fifty store
// functions -- lists, details, counts, charts, timelines -- and filtering each by
// project would leave a hundred and fifty places to forget one. Instead the
// console gets a handle on which those projects are simply not there: every
// connection it opens shadows each project-scoped table with a TEMP VIEW of the
// same name that leaves the hidden projects out. SQLite resolves an unqualified
// table name in the temp schema before main, so every query the store already
// has reads the view without a line of it changing. The handle is opened
// read-only as well, so a write that reached it by mistake fails loudly instead
// of landing.
//
// What a view cannot cover is listed, and covered elsewhere:
//
//   - the fts virtual table: MATCH, bm25() and snippet() need the table itself,
//     so its one reader (ftsSearchMatch) filters by the request's View instead
//     (accessFilter);
//   - writes: the console sends them to the main handle, after reading the
//     target through this one (console.Config.WriteDB);
//   - the in-memory live feed (the console's SSE): filtered per event against
//     the request's View.
//
// viewTables below is the inventory, and TestViewCoversEveryTable fails when a
// migration adds a table nobody classified.

// viewTables shadows each project-scoped table. %[1]s is the hidden-slug set,
// %[2]s the hidden sessions, %[3]s the hidden items (every memory, note, task,
// trial and session id in a hidden project), %[4]s the hidden gardener
// proposals. Each is a subquery, so the views track rows written after the
// connection opened.
var viewTables = []struct {
	name  string
	where string
}{
	{"projects", "slug NOT IN %[1]s"},
	{"memories_index", "project NOT IN %[1]s"},
	{"notes_index", "project NOT IN %[1]s"},
	{"sessions", "project_slug NOT IN %[1]s"},
	{"tasks", "project_slug NOT IN %[1]s"},
	{"trials", "project_slug NOT IN %[1]s AND session_id NOT IN %[2]s"},
	{"task_deps", "task_id NOT IN %[3]s AND depends_on NOT IN %[3]s"},
	// An event is hidden by its project, by the session it ran in, or by the item
	// it is about -- an event recorded under the global scope about a sealed
	// memory is still about that memory, and the gardener's own events (recorded
	// for no caller, with the proposal's key in the payload -- a memory-wanted
	// key embeds the query an agent missed on) are about their proposal.
	{"events", "project_slug NOT IN %[1]s AND session_id NOT IN %[2]s AND item_id NOT IN %[3]s AND item_id NOT IN %[4]s"},
	{"embeddings", "item_id NOT IN %[3]s"},
	{"retrieval_stats", "item_id NOT IN %[3]s"},
	{"repo_map", "slug NOT IN %[1]s"},
	// Proposal and job payloads are untyped JSON that names projects and items
	// under many keys (gardener.ResolveProposalScope decodes eleven kinds). The
	// view does not decode them: any text value anywhere in the payload equal to
	// a hidden slug or a hidden item id hides the row. That can only ever hide
	// more than the precise answer, never less.
	{"gardener_proposals", payloadVisible},
	{"jobs", payloadVisible},
}

// payloadVisible keeps a row whose JSON payload mentions no hidden slug and no
// hidden item id. A payload that is not valid JSON is hidden rather than
// guessed at.
const payloadVisible = `json_valid(payload) AND NOT EXISTS (
	SELECT 1 FROM json_tree(CASE WHEN json_valid(payload) THEN payload ELSE '{}' END) t
	WHERE t.type = 'text' AND (t.atom IN %[1]s OR t.atom IN %[3]s))`

// viewExempt lists the tables the views deliberately leave alone, each with the
// reason. A table in neither list fails TestViewCoversEveryTable.
var viewExempt = map[string]string{
	"settings":          "owner settings, counts and slugs (families, stages, the repo-map mirror); no corpus content",
	"schema_migrations": "migration bookkeeping",
	"fts":               "a virtual table cannot be shadowed by a view; its one reader filters by the request's View (accessFilter)",
}

const (
	hiddenSlugsSQL     = `(SELECT slug FROM temp.peer_hidden_slugs)`
	hiddenSessionsSQL  = `(SELECT id FROM main.sessions WHERE project_slug IN (SELECT slug FROM temp.peer_hidden_slugs) AND id IS NOT NULL)`
	hiddenItemsSQL     = `(SELECT id FROM temp.peer_hidden_items)`
	hiddenProposalsSQL = `(SELECT id FROM temp.peer_hidden_proposals)`
)

// viewSetup returns the statements that build one connection's shadow schema.
func viewSetup(hidden []string) []string {
	stmts := []string{`CREATE TEMP TABLE peer_hidden_slugs (slug TEXT PRIMARY KEY) WITHOUT ROWID`}
	for _, slug := range hidden {
		stmts = append(stmts, `INSERT INTO temp.peer_hidden_slugs (slug) VALUES (`+sqlQuote(slug)+`)`)
	}
	in := `(SELECT slug FROM temp.peer_hidden_slugs)`
	// NOT IN against a set holding a NULL is never true, which would hide every
	// row; the ids are primary keys and never NULL, and the guard keeps it so.
	stmts = append(stmts, `CREATE TEMP VIEW peer_hidden_items AS
		SELECT id FROM main.memories_index WHERE project IN `+in+` AND id IS NOT NULL
		UNION ALL SELECT id FROM main.notes_index WHERE project IN `+in+` AND id IS NOT NULL
		UNION ALL SELECT id FROM main.tasks WHERE project_slug IN `+in+` AND id IS NOT NULL
		UNION ALL SELECT id FROM main.trials WHERE project_slug IN `+in+` AND id IS NOT NULL
		UNION ALL SELECT id FROM main.sessions WHERE project_slug IN `+in+` AND id IS NOT NULL`)
	// The proposals the gardener_proposals view hides, by id, for the events
	// about them. The complement of the same predicate, over the same rows.
	stmts = append(stmts, `CREATE TEMP VIEW peer_hidden_proposals AS SELECT id FROM main.gardener_proposals WHERE NOT (`+
		fmt.Sprintf(payloadVisible, hiddenSlugsSQL, hiddenSessionsSQL, hiddenItemsSQL)+`) AND id IS NOT NULL`)
	for _, t := range viewTables {
		where := fmt.Sprintf(t.where, hiddenSlugsSQL, hiddenSessionsSQL, hiddenItemsSQL, hiddenProposalsSQL)
		stmts = append(stmts, `CREATE TEMP VIEW `+t.name+` AS SELECT * FROM main.`+t.name+` WHERE `+where)
	}
	return stmts
}

// sqlQuote renders s as a SQL string literal. The hidden slugs reach the setup
// as literals because the setup runs through the driver connection, before
// database/sql's placeholder handling; they are validated project slugs, and the
// quoting is the standard doubling all the same.
func sqlQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// viewConnector opens read-only connections that each build the shadow schema
// before database/sql hands them out, so no connection in the pool ever reads
// the unfiltered tables -- including one the pool reopens later.
type viewConnector struct {
	dsn   string
	setup []string
}

func (c viewConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := (&sqlite.Driver{}).Open(c.dsn)
	if err != nil {
		return nil, err
	}
	ex, ok := conn.(driver.ExecerContext)
	if !ok {
		_ = conn.Close()
		return nil, errors.New("store.OpenView: sqlite driver connection cannot exec")
	}
	for _, stmt := range c.setup {
		if _, err := ex.ExecContext(ctx, stmt, nil); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("store.OpenView: build view: %w", err)
		}
	}
	return conn, nil
}

func (c viewConnector) Driver() driver.Driver { return &sqlite.Driver{} }

// OpenView opens a read-only handle on the existing database at dbPath on which
// the projects in hidden do not exist: see the file comment. It never migrates
// (the daemon's main handle already did) and reports os.ErrNotExist when the
// file is absent. An empty hidden set is refused: a filtered handle that filters
// nothing would be the unfiltered database under a reassuring name.
func OpenView(dbPath string, hidden []string) (*sql.DB, error) {
	if len(hidden) == 0 {
		return nil, errors.New("store.OpenView: no hidden projects: use the main handle")
	}
	if _, err := os.Stat(dbPath); err != nil {
		return nil, fmt.Errorf("store.OpenView: %w", err)
	}
	hidden = slices.Clone(hidden)
	slices.Sort(hidden)
	hidden = slices.Compact(hidden)
	dsn := "file:" + url.PathEscape(dbPath) +
		"?mode=ro&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=mmap_size(268435456)"
	db := sql.OpenDB(viewConnector{dsn: dsn, setup: viewSetup(hidden)})
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	// Build the first connection now: a schema the views cannot be built over
	// fails here, at serve start, rather than inside the first console page.
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store.OpenView: %w", err)
	}
	return db, nil
}
