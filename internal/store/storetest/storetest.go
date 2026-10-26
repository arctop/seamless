// Package storetest is test support for anything that needs a fresh Seamless
// database. Only _test.go files import it, so none of it reaches a binary.
package storetest

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/arctop/seamless/internal/store"
)

// OpenFresh is store.Open on a path that does not exist yet, without paying
// for the migrations on every call.
//
// Under -race a fresh store.Open costs about half a second (every embedded
// migration runs through the instrumented, transpiled SQLite), against about
// 10ms for opening an already-migrated file, and most tests that touch the
// database open at least one. So each test binary migrates one template
// database once and each caller gets its own byte copy of it at path: every
// test still owns a separate, fresh database file, and nothing is shared but
// the starting bytes. store.Open on the copy still runs its migrate step, which
// finds every version applied and returns. TestOpenFresh_MatchesAFreshMigration
// pins the copy to what a fresh store.Open produces.
//
// path must not exist: an existing database is refused, never overwritten. A
// test that is about opening itself (migrating, reopening a file, an older or
// newer schema) calls store.Open.
func OpenFresh(tb testing.TB, path string) (*sql.DB, error) {
	tb.Helper()
	tpl, err := templateBytes(tb)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("storetest.OpenFresh: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("storetest.OpenFresh: %w", err)
	}
	if _, err := f.Write(tpl); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("storetest.OpenFresh: write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("storetest.OpenFresh: close %s: %w", path, err)
	}
	return store.Open(path)
}

var fresh struct {
	once sync.Once
	tpl  []byte
	err  error
}

// templateBytes returns the bytes of a freshly migrated database, built on first
// use in the first caller's temp dir. The bytes are all it keeps, so that
// dir's ordinary cleanup (which retries Windows' late file locks) takes the
// build files with it.
func templateBytes(tb testing.TB) ([]byte, error) {
	tb.Helper()
	fresh.once.Do(func() { fresh.tpl, fresh.err = buildTemplate(tb.TempDir()) })
	return fresh.tpl, fresh.err
}

// buildTemplate migrates a database in dir with store.Open and snapshots it
// with VACUUM INTO, which writes one self-contained file whatever the source's
// WAL holds, so the copy never depends on a checkpoint having run.
func buildTemplate(dir string) ([]byte, error) {
	db, err := store.Open(filepath.Join(dir, "template.db"))
	if err != nil {
		return nil, fmt.Errorf("storetest: template: %w", err)
	}
	snap := filepath.Join(dir, "snapshot.db")
	if _, err := db.Exec(`VACUUM INTO ?`, snap); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("storetest: template: snapshot: %w", err)
	}
	if err := db.Close(); err != nil {
		return nil, fmt.Errorf("storetest: template: %w", err)
	}
	tpl, err := os.ReadFile(snap)
	if err != nil {
		return nil, fmt.Errorf("storetest: template: %w", err)
	}
	return tpl, nil
}
