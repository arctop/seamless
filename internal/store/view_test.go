package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
)

// Every table in the schema is either shadowed by the filtered handle or listed
// as exempt with a reason. A migration that adds a table fails here until
// someone decides which: an unclassified table is one the owner's console would
// read unfiltered.
func TestViewCoversEveryTable(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table'
		AND name NOT LIKE 'sqlite_%' AND name NOT LIKE 'fts_%'`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	shadowed := map[string]bool{}
	for _, v := range viewTables {
		shadowed[v.name] = true
	}
	n := 0
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		_, exempt := viewExempt[name]
		require.True(t, shadowed[name] || exempt,
			"table %q is neither shadowed by store.OpenView (viewTables) nor listed in viewExempt with a reason", name)
		require.False(t, shadowed[name] && exempt, "table %q is both shadowed and exempt", name)
		n++
	}
	require.NoError(t, rows.Err())
	require.Positive(t, n)
}

func seedViewFixture(t *testing.T, db *sql.DB) (hiddenMem, shownMem, hiddenTask, hiddenSession, hiddenProposal string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	for _, slug := range []string{"arctop-finance", "arctop-ops"} {
		_, err := EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
	}
	ins := func(q string, args ...any) {
		t.Helper()
		_, err := db.ExecContext(ctx, q, args...)
		require.NoError(t, err)
	}
	ts := core.FormatTime(now)
	ins(`INSERT INTO memories_index (id, kind, name, description, project, file_path, content_hash, created_at, updated_at)
		VALUES ('m-hidden','gotcha','fin','secret','arctop-finance','memory/arctop-finance/fin.md','h',?,?),
		       ('m-shown','gotcha','ops','open','arctop-ops','memory/arctop-ops/ops.md','h',?,?),
		       ('m-global','gotcha','glob','global','','memory/_global/glob.md','h',?,?)`, ts, ts, ts, ts, ts, ts)
	ins(`INSERT INTO notes_index (id, title, slug, project, file_path, content_hash, created_at, updated_at)
		VALUES ('n-hidden','fin note','fin-note','arctop-finance','notes/arctop-finance/fin-note.md','h',?,?)`, ts, ts)
	ins(`INSERT INTO sessions (id, name, project_slug, created_at, updated_at)
		VALUES ('s-hidden','fin-sess','arctop-finance',?,?), ('s-shown','ops-sess','arctop-ops',?,?)`, ts, ts, ts, ts)
	ins(`INSERT INTO tasks (id, project_slug, title, created_at, updated_at)
		VALUES ('t-hidden','arctop-finance','fin task',?,?), ('t-shown','arctop-ops','ops task',?,?)`, ts, ts, ts, ts)
	ins(`INSERT INTO task_deps (task_id, depends_on) VALUES ('t-shown','t-hidden')`)
	ins(`INSERT INTO trials (id, lab, project_slug, session_id, created_at)
		VALUES ('tr-hidden','lab','arctop-finance','s-hidden',?), ('tr-by-hidden-session','lab','','s-hidden',?), ('tr-shown','lab','arctop-ops','',?)`, ts, ts, ts)
	ins(`INSERT INTO events (id, ts, kind, session_id, project_slug, item_id, payload) VALUES
		('e1',?,'tool.call','','arctop-finance','','{}'),
		('e2',?,'memory.read','','','m-hidden','{}'),
		('e3',?,'tool.call','s-hidden','','','{}'),
		('e4',?,'tool.call','s-shown','arctop-ops','m-shown','{}')`, ts, ts, ts, ts)
	ins(`INSERT INTO embeddings (item_id, kind, model, dims, vec, updated_at)
		VALUES ('m-hidden','memory','m',1,x'00000000',?), ('m-shown','memory','m',1,x'00000000',?)`, ts, ts)
	ins(`INSERT INTO retrieval_stats (item_id, inject_count) VALUES ('m-hidden', 3), ('m-shown', 2)`)
	ins(`INSERT INTO repo_map (host, path, slug, created_at) VALUES ('boron','/work/fin','arctop-finance',?), ('boron','/work/ops','arctop-ops',?)`, ts, ts)
	ph, err := CreateProposal(ctx, db, ProposalArchive, map[string]any{"id": "m-hidden", "project": "arctop-finance"})
	require.NoError(t, err)
	_, err = CreateProposal(ctx, db, ProposalMerge, map[string]any{"keep": map[string]any{"id": "m-shown"}, "drop": map[string]any{"id": "m-hidden"}})
	require.NoError(t, err)
	_, err = CreateProposal(ctx, db, ProposalArchive, map[string]any{"id": "m-shown", "project": "arctop-ops"})
	require.NoError(t, err)
	return "m-hidden", "m-shown", "t-hidden", "s-hidden", ph.ID
}

func TestOpenView_HidesTheSealedProjectEverywhere(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seam.db")
	db, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, _, _, _, hiddenProposal := seedViewFixture(t, db)

	_, err = OpenView(path, nil)
	require.Error(t, err, "a filtered handle that filters nothing is refused")

	view, err := OpenView(path, []string{"arctop-finance"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = view.Close() })
	ctx := context.Background()

	count := func(q string) int {
		t.Helper()
		var n int
		require.NoError(t, view.QueryRowContext(ctx, q).Scan(&n))
		return n
	}
	for _, c := range []struct {
		q    string
		want int
	}{
		{`SELECT COUNT(*) FROM projects WHERE slug = 'arctop-finance'`, 0},
		{`SELECT COUNT(*) FROM projects`, 1},
		{`SELECT COUNT(*) FROM memories_index`, 2}, // ops + global
		{`SELECT COUNT(*) FROM notes_index`, 0},
		{`SELECT COUNT(*) FROM sessions`, 1},
		{`SELECT COUNT(*) FROM tasks`, 1},
		{`SELECT COUNT(*) FROM task_deps`, 0}, // its depends_on is hidden
		{`SELECT COUNT(*) FROM trials`, 1},    // by project, and by session
		{`SELECT COUNT(*) FROM events`, 1},    // by project, item and session
		{`SELECT COUNT(*) FROM embeddings`, 1},
		{`SELECT COUNT(*) FROM retrieval_stats`, 1},
		{`SELECT COUNT(*) FROM repo_map`, 1},
		{`SELECT COUNT(*) FROM gardener_proposals`, 1}, // the merge naming a hidden memory is hidden too
	} {
		require.Equal(t, c.want, count(c.q), c.q)
	}

	// Through the store functions the console uses, not just raw SQL.
	_, found, err := MemoryByID(ctx, view, "m-hidden")
	require.NoError(t, err)
	require.False(t, found)
	_, found, err = ProposalByID(ctx, view, hiddenProposal)
	require.NoError(t, err)
	require.False(t, found)
	ps, err := ListProjects(ctx, view)
	require.NoError(t, err)
	require.Len(t, ps, 1)
	require.Equal(t, "arctop-ops", ps[0].Slug)

	// Writes fail loudly rather than landing.
	_, err = view.ExecContext(ctx, `UPDATE tasks SET title = 'x'`)
	require.Error(t, err)
	_, err = view.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES ('k','v')`)
	require.Error(t, err, "the handle is read-only, unshadowed tables included")

	// The view tracks rows written after it opened.
	_, err = db.ExecContext(ctx, `INSERT INTO tasks (id, project_slug, title, created_at, updated_at) VALUES ('t-late','arctop-finance','late','x','x')`)
	require.NoError(t, err)
	require.Equal(t, 1, count(`SELECT COUNT(*) FROM tasks`))
}

func TestAccessFilter_FTSAndCosine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seam.db")
	db, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	for _, row := range []struct{ id, project string }{{"m-fin", "arctop-finance"}, {"m-ops", "arctop-ops"}, {"m-glob", ""}} {
		_, err := db.ExecContext(ctx, `INSERT INTO fts (item_id, kind, project, title, name, description, body) VALUES (?, 'memory', ?, '', 'ledger', '', 'ledger body')`, row.id, row.project)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, `INSERT INTO embeddings (item_id, kind, model, dims, vec, updated_at) VALUES (?, 'memory', 'm', 2, ?, 'x')`, row.id, EncodeVector([]float32{1, 0}))
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, `INSERT INTO memories_index (id, kind, name, project, file_path, content_hash, created_at, updated_at)
			VALUES (?, 'gotcha', ?, ?, ?, 'h', 'x', 'x')`, row.id, row.id, row.project, "memory/"+row.id+".md")
		require.NoError(t, err)
	}
	p := testPeerPolicy(t)
	fin, _ := p.PeerView(470)
	ids := func(ctx context.Context) []string {
		hits, err := FTSSearch(ctx, db, "ledger", nil, nil, 10)
		require.NoError(t, err)
		var out []string
		for _, h := range hits {
			out = append(out, h.ItemID)
		}
		cos, err := CosineSearch(ctx, db, []float32{1, 0}, "m", nil, nil, 10)
		require.NoError(t, err)
		var cout []string
		for _, h := range cos {
			cout = append(cout, h.ItemID)
		}
		require.ElementsMatch(t, out, cout, "the two legs agree on reach")
		return out
	}
	require.ElementsMatch(t, []string{"m-fin", "m-ops", "m-glob"}, ids(ctx), "no View: everything")
	require.ElementsMatch(t, []string{"m-ops", "m-glob"}, ids(access.WithView(ctx, p.OwnerView())), "the owner: all but the seal")
	require.ElementsMatch(t, []string{"m-fin", "m-glob"}, ids(access.WithView(ctx, fin)), "the account: its own plus global")
}

func testPeerPolicy(t *testing.T) *access.Policy {
	t.Helper()
	accounts := map[string]access.Account{"katata": {UID: 501, Name: "katata"}, "_arctop_finance": {UID: 470, Name: "_arctop_finance"}}
	p, err := access.New(config.PeerAuth{Owner: "katata", Users: map[string]config.PeerUser{
		"_arctop_finance": {Sealed: []string{"arctop-finance"}},
	}}, func(n string) (access.Account, error) {
		if a, ok := accounts[n]; ok {
			return a, nil
		}
		return access.Account{}, fmt.Errorf("no %s", n)
	})
	require.NoError(t, err)
	return p
}

// CanRead/CanWrite give the View the first word: an account's own confidential
// project reads and writes; the owner's open project, and global writes, do not.
func TestCanReadCanWrite_PeerAuth(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	for _, slug := range []string{"arctop-finance", "arctop-ops"} {
		_, err := EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
	}
	require.NoError(t, SetProjectIsolation(ctx, db, "arctop-finance", core.IsolationConfidential))
	p := testPeerPolicy(t)
	fin, _ := p.PeerView(470)
	finCtx := access.WithView(ctx, fin)
	ownCtx := access.WithView(ctx, p.OwnerView())

	for _, tc := range []struct {
		name           string
		ctx            context.Context
		caller, target string
		read, write    bool
	}{
		{"account in its own project", finCtx, "arctop-finance", "arctop-finance", true, true},
		{"account reading global", finCtx, "arctop-finance", "", true, false},
		{"account reaching the owner's open project", finCtx, "arctop-finance", "arctop-ops", false, false},
		{"owner bound to its project reaching the seal", ownCtx, "arctop-ops", "arctop-finance", false, false},
		{"owner bound INTO the seal still reads nothing", ownCtx, "arctop-finance", "arctop-finance", false, false},
		{"owner in its own project", ownCtx, "arctop-ops", "arctop-ops", true, true},
		{"no View: the isolation matrix alone", ctx, "arctop-ops", "arctop-finance", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := CanRead(tc.ctx, db, tc.caller, tc.target)
			require.NoError(t, err)
			require.Equal(t, tc.read, r, "read")
			w, err := CanWrite(tc.ctx, db, tc.caller, tc.target)
			require.NoError(t, err)
			require.Equal(t, tc.write, w, "write")
		})
	}
}
