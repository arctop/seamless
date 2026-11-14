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
	"sync"

	sqlite "modernc.org/sqlite"
)

// A filtered read handle: the owner's view of the database while peer_auth
// locks projects.
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
	// Write screening: a held write is its project's content (it carries the
	// text an agent tried to write) and its session's; a confirmation and a
	// taint mark are about one item or one session.
	{"held_writes", "project_slug NOT IN %[1]s AND session_id NOT IN %[2]s AND item_id NOT IN %[3]s"},
	{"owner_confirmations", "item_id NOT IN %[3]s"},
	{"session_taints", "session_id NOT IN %[2]s"},
	// The corpus record names one file of one project: hidden by the project
	// its path is under, and by the item it records.
	{"corpus_records", "project_slug NOT IN %[1]s AND item_id NOT IN %[3]s"},
	// A commit record belongs to one session or one task, and was recorded by
	// one session: hidden with either (the hidden items hold every session and
	// task id in a hidden project).
	{"commit_records", "owner_id NOT IN %[3]s AND recorded_by NOT IN %[2]s"},
	// A provenance stamp is about one item of one project: hidden with the item
	// and with the project it is in. The project whose content it may hold is
	// named, never hidden: a slug is no content.
	{"item_provenance", "project_slug NOT IN %[1]s AND item_id NOT IN %[3]s"},
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
	// a locked project's unlocks: the approval page must read a request for a
	// LOCKED project, which is exactly the project this handle would hide.
	"access_approvals": "unlock requests for locked projects: slugs, ids, the kernel's facts about the caller and a capped one-line reason; no corpus content, and the approval page reads it for a locked project",
	"access_leases":    "granted unlocks: slugs, process keys, relay ids, session ids and times; no corpus content",
	// isolation.requests' read grants: the access page lists them beside the
	// leases, metadata about a confidential project the console never hides.
	"access_grants": "read grants of confidential projects: slugs, process keys, relay ids, session ids and times; no corpus content",
	// fenced-sessions: a conversation's moves into fenced projects and the
	// flows between projects its approvals cleared, listed beside the grants.
	"access_moves": "approved moves of a conversation into a fenced project: slugs, holder keys, session ids, the deciding proof and times; no corpus content",
	"access_flows": "the flows between projects an approval cleared for one holder: slugs, holder keys, approval ids and times; no corpus content",
	// The owner's approver credentials and enrollment codes: key material and
	// hashes about no project at all.
	"approver_credentials":  "approver credentials (passkeys, Touch ID keys): public keys and labels, about no project; no corpus content",
	"approver_enroll_codes": "one-time enrollment code hashes and their expiry, about no project; no corpus content",
	// A console unlock names a LOCKED project, and the console reads it to
	// decide which handle a browser gets: the handle that would hide it.
	"console_unlocks": "per-browser console unlocks of locked projects: a slug, cookie and pairing-code hashes, a credential id and times; no corpus content, and the console reads it to choose the handle",
	// A console lock names a LOCKED project, and the lock page reads it for a
	// project this request's handle may hide.
	"console_locks": "locks the owner set from the console: a slug, a credential id and a time; no corpus content, and the console reads it for a project its handle may hide",
	// An MCP connection's declared capabilities, about no project: which
	// dialog modes its client can show.
	"mcp_sessions": "MCP connection ids and the elicitation modes their clients declared, about no project; no corpus content",
	// Console sign-in with a passkey: which browsers are signed in, and the
	// passkeys devices created that wait for the owner's approval. The console
	// reads them before it knows which handle a request gets.
	"console_signins":     "browsers signed in to the console: cookie and browser-key hashes, a credential id, times, and a terminal sign-in's address and User-Agent, about no project; no corpus content",
	"passkey_enrollments": "passkeys waiting for the owner's approval: public keys, a label, a code hash and times, about no project; no corpus content",
	// Device pairing (plan networked-relay): the computers paired with this
	// server and the one-time codes that pair them. The edge reads them
	// before a request reaches any route, and they are about no project.
	"devices":              "computers paired with this server (relays and key devices): a host name, a public key, key and secret hashes, an approver credential id and times, about no project; no corpus content",
	"device_pairing_codes": "one-time pairing code hashes and their expiry and use, about no project; no corpus content",
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
	db, err := openView(dbPath, hidden)
	if err != nil {
		return nil, fmt.Errorf("store.OpenView: %w", err)
	}
	return db, nil
}

// openView is OpenView without the empty-set refusal: the pool (Views) opens
// a read-only handle that hides nothing for a request whose every locked
// project is open, so even that request reads a handle no write can land on.
func openView(dbPath string, hidden []string) (*sql.DB, error) {
	if _, err := os.Stat(dbPath); err != nil {
		return nil, err
	}
	hidden = normalizeSlugs(hidden)
	dsn := "file:" + url.PathEscape(dbPath) +
		"?mode=ro&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=mmap_size(268435456)"
	db := sql.OpenDB(viewConnector{dsn: dsn, setup: viewSetup(hidden)})
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	// Build the first connection now: a schema the views cannot be built over
	// fails here, at serve start, rather than inside the first console page.
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// normalizeSlugs returns slugs sorted and de-duplicated, as a copy.
func normalizeSlugs(slugs []string) []string {
	out := slices.Clone(slugs)
	slices.Sort(out)
	return slices.Compact(out)
}

// Views is a pool of filtered read handles over one database, one per hidden
// set. The console reads each request through the handle
// that hides exactly what that request may not read: every locked project the
// request holds nothing open on -- an agent tree's lease on the socket, or a
// browser's console unlock. A request with nothing open reads the base handle,
// the one serve built before (OpenView over the whole set).
//
// Handles open lazily, on the first request that needs a given set, and stay
// cached until Close: a set is a subset of lockable, so the pool never holds
// more than 2^len(lockable) handles. A request can only ever narrow lockable --
// a project For is asked to open that is not lockable was never hidden at all.
type Views struct {
	path     string
	lockable []string

	mu      sync.Mutex
	handles map[string]*sql.DB
	closed  bool
}

// ErrViewsClosed is a handle asked of a pool after Close.
var ErrViewsClosed = errors.New("store.Views: closed")

// OpenViews opens the pool over the existing database at dbPath and builds its
// base handle -- every lockable project hidden -- now, so a schema the views
// cannot be built over fails at serve start. A pool that would hide nothing at
// all is refused, as OpenView refuses one.
func OpenViews(dbPath string, lockable []string) (*Views, error) {
	lockable = normalizeSlugs(lockable)
	if len(lockable) == 0 {
		return nil, errors.New("store.OpenViews: no hidden projects: use the main handle")
	}
	v := &Views{path: dbPath, lockable: lockable, handles: map[string]*sql.DB{}}
	if _, err := v.For(nil); err != nil {
		return nil, fmt.Errorf("store.OpenViews: %w", err)
	}
	return v, nil
}

// Lockable returns the projects a request may open (the locked ones), sorted.
func (v *Views) Lockable() []string { return slices.Clone(v.lockable) }

// Hidden returns the set the handle For(open) hides, sorted: every lockable
// project open does not name.
func (v *Views) Hidden(open []string) []string {
	var out []string
	for _, slug := range v.lockable {
		if !slices.Contains(open, slug) {
			out = append(out, slug)
		}
	}
	return out
}

// For returns the read-only handle on which Hidden(open) does not exist,
// opening it on first use. Every handle is read-only, including the one a
// request with every locked project open gets (it hides nothing).
func (v *Views) For(open []string) (*sql.DB, error) {
	hidden := v.Hidden(open)
	key := strings.Join(hidden, "\x00")
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return nil, ErrViewsClosed
	}
	if db, ok := v.handles[key]; ok {
		return db, nil
	}
	db, err := openView(v.path, hidden)
	if err != nil {
		return nil, fmt.Errorf("store.Views.For: %w", err)
	}
	v.handles[key] = db
	return db, nil
}

// Open reports how many handles the pool holds (at most 2^len(Lockable)).
func (v *Views) Open() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.handles)
}

// Close closes every handle. A request already holding one finishes on a
// closed handle and fails; serve closes the pool only after its listeners.
func (v *Views) Close() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return nil
	}
	v.closed = true
	var errs []error
	for _, db := range v.handles {
		errs = append(errs, db.Close())
	}
	v.handles = nil
	return errors.Join(errs...)
}
