package store

import (
	"context"
	"database/sql"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// addApprover inserts one approver credential row the way enrollment leaves it.
func addApprover(t *testing.T, db *sql.DB, id, name string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `INSERT INTO approver_credentials
		(id, kind, credential_id, public_key, alg, name, added_at, added_by)
		VALUES (?, 'passkey', ?, X'0102', -7, ?, '2026-11-01T00:00:00Z', 'test')`, id, []byte(id), name)
	require.NoError(t, err)
}

func approverIDs(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), `SELECT id FROM approver_credentials ORDER BY id`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		out = append(out, id)
	}
	require.NoError(t, rows.Err())
	return out
}

func lockRow(t *testing.T, db *sql.DB, slug string) (credential, lockedAt string) {
	t.Helper()
	err := db.QueryRowContext(context.Background(),
		`SELECT credential, locked_at FROM console_locks WHERE project_slug = ?`, slug).Scan(&credential, &lockedAt)
	require.NoError(t, err, "console lock on %s", slug)
	return credential, lockedAt
}

func setting(t *testing.T, db *sql.DB, key string) (string, bool) {
	t.Helper()
	v, ok, err := GetSetting(context.Background(), db, key)
	require.NoError(t, err)
	return v, ok
}

func ensureFenced(t *testing.T, db *sql.DB, slug string, state core.Isolation) {
	t.Helper()
	ctx := context.Background()
	_, err := EnsureProject(ctx, db, slug, slug)
	require.NoError(t, err)
	require.NoError(t, SetProjectIsolation(ctx, db, slug, state))
}

func TestApplyKeptState_TodaysSecurityAndSettingsWin(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 11, 14, 0, 0, 0, time.UTC)

	// Today: the console's settings, one passkey, a lock on alpha set from the
	// console, alpha sealed, beta confidential, and one repo route.
	today := openTestDB(t)
	require.NoError(t, SetSetting(ctx, today, SettingFeaturesConfig, `{"today":true}`))
	require.NoError(t, SetSetting(ctx, today, approverUserHandleKey, "handle-today"))
	require.NoError(t, SetSetting(ctx, today, SettingBackupConfig, `{"dir":"/today"}`))
	addApprover(t, today, "01APPTODAY", "today's key")
	ensureFenced(t, today, "alpha", core.IsolationSealed)
	ensureFenced(t, today, "beta", core.IsolationConfidential)
	_, err := EnsureProject(ctx, today, "gamma", "gamma")
	require.NoError(t, err)
	require.NoError(t, AddConsoleLock(ctx, today, "alpha", "cred-today", now.Add(-time.Hour)))
	_, err = today.ExecContext(ctx, `INSERT INTO repo_map (host, path, slug, origin, created_at)
		VALUES ('mac', '/repos/alpha', 'alpha', '', ?)`, core.FormatTime(now))
	require.NoError(t, err)
	kept, err := ReadKeptState(ctx, today)
	require.NoError(t, err)

	// The backup: older settings, a passkey removed since and another one, its
	// own locks (alpha's by another credential, delta's), alpha only
	// confidential with a child, beta open, gamma sealed, its own repo route.
	restored := openTestDB(t)
	require.NoError(t, SetSetting(ctx, restored, SettingFeaturesConfig, `{"backup":true}`))
	require.NoError(t, SetSetting(ctx, restored, approverUserHandleKey, "handle-backup"))
	require.NoError(t, SetSetting(ctx, restored, SettingProjectFamilies, `{"fam":["gamma","omega"]}`))
	addApprover(t, restored, "01APPREMOVED", "removed since")
	addApprover(t, restored, "01APPTODAY", "today's key, as it was")
	ensureFenced(t, restored, "alpha", core.IsolationConfidential)
	_, err = EnsureProject(ctx, restored, "alpha-child", "alpha child")
	require.NoError(t, err)
	_, err = restored.ExecContext(ctx, `UPDATE projects SET parent_slug = 'alpha' WHERE slug = 'alpha-child'`)
	require.NoError(t, err)
	_, err = EnsureProject(ctx, restored, "beta", "beta")
	require.NoError(t, err)
	ensureFenced(t, restored, "gamma", core.IsolationSealed)
	ensureFenced(t, restored, "delta", core.IsolationConfidential)
	_, err = restored.ExecContext(ctx, `INSERT INTO console_locks (project_slug, credential, locked_at) VALUES
		('alpha', 'cred-backup', '2026-10-01T00:00:00Z'), ('delta', 'cred-delta', '2026-10-01T00:00:00Z')`)
	require.NoError(t, err)
	_, err = restored.ExecContext(ctx, `INSERT INTO repo_map (host, path, slug, origin, created_at)
		VALUES ('mac', '/repos/old', 'gamma', '', '2026-10-01T00:00:00Z')`)
	require.NoError(t, err)

	rep, err := ApplyKeptState(ctx, restored, kept, ApplyKeptOptions{Now: now})
	require.NoError(t, err)

	v, _ := setting(t, restored, SettingFeaturesConfig)
	require.Equal(t, `{"today":true}`, v, "a console setting stays today's")
	v, _ = setting(t, restored, approverUserHandleKey)
	require.Equal(t, "handle-today", v, "the passkey user handle stays today's")
	v, _ = setting(t, restored, SettingBackupConfig)
	require.Equal(t, `{"dir":"/today"}`, v)
	v, _ = setting(t, restored, SettingProjectFamilies)
	require.Equal(t, `{"fam":["gamma","omega"]}`, v, "families come back with the data")

	require.Equal(t, []string{"01APPTODAY"}, approverIDs(t, restored), "a passkey removed since the backup stays removed")
	var name string
	require.NoError(t, restored.QueryRowContext(ctx, `SELECT name FROM approver_credentials WHERE id = '01APPTODAY'`).Scan(&name))
	require.Equal(t, "today's key", name, "the approver row is today's, not the backup's")

	cred, at := lockRow(t, restored, "alpha")
	require.Equal(t, "cred-today", cred, "today's lock row is kept verbatim")
	require.Equal(t, core.FormatTime(now.Add(-time.Hour)), at)
	cred, _ = lockRow(t, restored, "delta")
	require.Equal(t, "cred-delta", cred, "a lock only the backup had stays: a restore never reduces the locked set")

	isolation := func(slug string) core.Isolation {
		t.Helper()
		got, err := IsolationOf(ctx, restored, slug)
		require.NoError(t, err)
		return got
	}
	require.Equal(t, core.IsolationSealed, isolation("alpha"), "sealed today stays sealed")
	require.Equal(t, core.IsolationConfidential, isolation("beta"), "a restore never loosens a fence")
	require.Equal(t, core.IsolationSealed, isolation("gamma"), "the backup's stricter fence stays")
	require.Equal(t, core.IsolationConfidential, isolation("delta"))
	child, ok, err := ProjectBySlug(ctx, restored, "alpha-child")
	require.NoError(t, err)
	require.True(t, ok)
	require.Empty(t, child.ParentSlug, "a fenced project cannot keep a child")

	var routes []string
	rows, err := restored.QueryContext(ctx, `SELECT path FROM repo_map ORDER BY path`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var p string
		require.NoError(t, rows.Scan(&p))
		routes = append(routes, p)
	}
	require.NoError(t, rows.Err())
	require.Equal(t, []string{"/repos/alpha"}, routes, "repo routes are today's")

	require.Equal(t, []string{"alpha: confidential -> sealed", "beta: open -> confidential"}, rep.Tightened)
	require.Equal(t, []string{"alpha-child (from alpha)"}, rep.Detached)
	require.Empty(t, rep.Registered)
}

func TestApplyKeptState_LockedProjectsTheBackupLacksComeBackLocked(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 11, 14, 0, 0, 0, time.UTC)

	today := openTestDB(t)
	ensureFenced(t, today, "epsilon", core.IsolationSealed)
	require.NoError(t, AddConsoleLock(ctx, today, "epsilon", "cred-today", now))
	kept, err := ReadKeptState(ctx, today)
	require.NoError(t, err)
	require.Equal(t, []LockedProject{{Slug: "epsilon", Isolation: core.IsolationSealed}}, kept.Locked(nil))
	require.Equal(t, []LockedProject{
		{Slug: "epsilon", Isolation: core.IsolationSealed},
		{Slug: "zeta", Isolation: core.IsolationOpen},
	}, kept.Locked([]string{"zeta"}), "a force lock with no project today reads as open")

	restored := openTestDB(t)
	_, err = EnsureProject(ctx, restored, "zeta", "zeta")
	require.NoError(t, err)

	rep, err := ApplyKeptState(ctx, restored, kept, ApplyKeptOptions{ForceLocked: []string{"zeta"}, Now: now})
	require.NoError(t, err)
	require.Equal(t, []string{"epsilon"}, rep.Registered)

	got, err := IsolationOf(ctx, restored, "epsilon")
	require.NoError(t, err)
	require.Equal(t, core.IsolationSealed, got, "registered with today's fence")
	cred, _ := lockRow(t, restored, "epsilon")
	require.Equal(t, "cred-today", cred)
	got, err = IsolationOf(ctx, restored, "zeta")
	require.NoError(t, err)
	require.Equal(t, core.IsolationConfidential, got, "a force-locked project is fenced like the console's")
}

func TestClearHashesWithoutVectors(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ts := core.FormatTime(time.Date(2026, 11, 11, 3, 0, 0, 0, time.UTC))
	insertMemory(t, db, "01WITH", "gotcha", "with", "", "alpha", "body", ts, "")
	insertMemory(t, db, "01WITHOUT", "gotcha", "without", "", "alpha", "body", ts, "")
	insertNote(t, db, "01NOTE", "n", "n", "alpha", ts)
	require.NoError(t, UpsertEmbedding(ctx, db, "01WITH", "memory", "m", []float32{0.1}))

	n, err := ClearHashesWithoutVectors(ctx, db)
	require.NoError(t, err)
	require.Equal(t, int64(2), n)
	var hash string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT content_hash FROM memories_index WHERE id = '01WITH'`).Scan(&hash))
	require.Equal(t, "h", hash, "an item with a vector keeps its hash")
	require.NoError(t, db.QueryRowContext(ctx, `SELECT content_hash FROM memories_index WHERE id = '01WITHOUT'`).Scan(&hash))
	require.Empty(t, hash)

	n, err = ClearHashesWithoutVectors(ctx, db)
	require.NoError(t, err)
	require.Zero(t, n, "already blank rows are not counted again")
}

// settingsKeyFuncs are the functions whose third argument is a settings key.
var settingsKeyFuncs = []string{"GetSetting", "SetSetting", "DeleteSetting", "SetSettingTime", "SettingTime", "getSettingTx", "setSettingTx"}

// TestSettingsKeys_EveryKeyIsClassifiedForRestore holds every settings key the
// store uses to a decision: kept as it is today by a restore
// (RestoreKeptSettings), or restored with the data (restoredSettings). A key
// in neither list would be restored silently, and one that should have been
// kept -- a security switch, an approver's identity -- would come back old.
func TestSettingsKeys_EveryKeyIsClassifiedForRestore(t *testing.T) {
	paths, err := filepath.Glob("*.go")
	require.NoError(t, err)
	fset := token.NewFileSet()
	var files []*ast.File
	consts := map[string]string{}
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, p, nil, 0)
		require.NoError(t, err)
		files = append(files, f)
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						if v, err := strconv.Unquote(lit.Value); err == nil {
							consts[name.Name] = v
						}
					}
				}
			}
		}
	}

	keys := map[string]bool{}
	resolve := func(e ast.Expr) (string, bool) {
		switch x := e.(type) {
		case *ast.Ident:
			v, ok := consts[x.Name]
			return v, ok
		case *ast.BasicLit:
			if x.Kind == token.STRING {
				v, err := strconv.Unquote(x.Value)
				return v, err == nil
			}
		}
		return "", false
	}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := ""
			switch fn := call.Fun.(type) {
			case *ast.Ident:
				name = fn.Name
			case *ast.SelectorExpr:
				name = fn.Sel.Name
			}
			if slices.Contains(settingsKeyFuncs, name) && len(call.Args) >= 3 {
				if k, ok := resolve(call.Args[2]); ok {
					keys[k] = true
				}
			}
			// Raw SQL on the settings table: every string constant passed with
			// it is a key (the approver user handle is written this way).
			if len(call.Args) > 0 {
				for i, arg := range call.Args {
					sql, ok := resolve(arg)
					if !ok || !strings.Contains(sql, "settings") || !strings.Contains(sql, "key") {
						continue
					}
					for _, rest := range call.Args[i+1:] {
						if k, ok := resolve(rest); ok && !strings.Contains(k, " ") {
							keys[k] = true
						}
					}
				}
			}
			return true
		})
	}
	// Every key constant counts, used here or only by another package.
	for name, v := range consts {
		if strings.HasPrefix(name, "Setting") || strings.HasSuffix(name, "Key") {
			keys[v] = true
		}
	}

	classified := map[string]bool{}
	for _, k := range RestoreKeptSettings {
		require.False(t, classified[k], "%q is listed twice", k)
		classified[k] = true
	}
	for _, k := range restoredSettings {
		require.False(t, classified[k], "%q is both kept and restored", k)
		classified[k] = true
	}
	for k := range keys {
		require.Truef(t, classified[k],
			"settings key %q is neither kept by a restore (RestoreKeptSettings) nor restored with the data (restoredSettings): classify it", k)
	}
	for k := range classified {
		require.Truef(t, keys[k], "%q is classified, but no settings key has that value", k)
	}
	require.Contains(t, keys, approverUserHandleKey, "the scan sees keys written by raw SQL")
}
