package archive

import (
	"archive/tar"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

func openLockDB(t *testing.T, dir string) *sql.DB {
	t.Helper()
	db, err := store.OpenExisting(filepath.Join(dir, DBName))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// lockInConsole fences a project and locks it the way the console does.
func lockInConsole(t *testing.T, dir, slug string) {
	t.Helper()
	ctx := context.Background()
	db := openLockDB(t, dir)
	_, err := store.EnsureProject(ctx, db, slug, slug)
	require.NoError(t, err)
	require.NoError(t, store.SetProjectIsolation(ctx, db, slug, core.IsolationSealed))
	require.NoError(t, store.AddConsoleLock(ctx, db, slug, "cred-1", time.Now()))
	require.NoError(t, db.Close())
}

func addPasskeyRow(t *testing.T, dir, name string, backupEligible bool, attachment string) {
	t.Helper()
	db := openLockDB(t, dir)
	_, err := db.Exec(`INSERT INTO approver_credentials (id, kind, credential_id, public_key, alg, backup_eligible, attachment, name, added_at, added_by)
		VALUES (?, 'passkey', ?, ?, -7, ?, ?, ?, '2026-11-09T00:00:00Z', 'code')`,
		"id-"+name, []byte("cred-"+name), []byte("SECRET-KEY-BYTES-"+name), backupEligible, attachment, name)
	require.NoError(t, err)
	require.NoError(t, db.Close())
}

func consoleLocksIn(t *testing.T, dir string) []string {
	t.Helper()
	got, err := store.ConsoleLockedSlugs(context.Background(), openLockDB(t, dir))
	require.NoError(t, err)
	return got
}

func isolationIn(t *testing.T, dir, slug string) core.Isolation {
	t.Helper()
	got, err := store.IsolationOf(context.Background(), openLockDB(t, dir), slug)
	require.NoError(t, err)
	return got
}

func lockEvents(t *testing.T, dir, slug string) []string {
	t.Helper()
	rows, err := openLockDB(t, dir).Query(`SELECT payload FROM events WHERE kind = ? AND project_slug = ? ORDER BY ts`,
		string(core.EventConsoleLockChanged), slug)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var p string
		require.NoError(t, rows.Scan(&p))
		out = append(out, p)
	}
	require.NoError(t, rows.Err())
	return out
}

func exportWith(t *testing.T, opts ExportOptions) ([]byte, Manifest) {
	t.Helper()
	var buf bytes.Buffer
	opts.Out, opts.Host = &buf, "exporthost"
	m, err := Export(context.Background(), opts)
	require.NoError(t, err)
	return buf.Bytes(), m
}

func importInto(t *testing.T, dir string, data []byte, mod func(*ImportOptions)) (*ImportReport, error) {
	t.Helper()
	opts := ImportOptions{DataDir: dir, Src: bytes.NewReader(data)}
	if mod != nil {
		mod(&opts)
	}
	return Import(context.Background(), opts)
}

func TestExport_RecordsLocksAndNoKeyMaterial(t *testing.T) {
	src := seedIndexed(t, specA)
	lockInConsole(t, src, specA.project)
	addPasskeyRow(t, src, "laptop", false, "platform")
	addPasskeyRow(t, src, "icloud", true, "platform")
	addPasskeyRow(t, src, "yubikey", false, "cross-platform")

	_, m := exportWith(t, ExportOptions{DataDir: src, ForceLocked: []string{"zeta"}, PasskeyRPID: "seamless.test"})

	require.Equal(t, []LockedProject{
		{Slug: specA.project, Isolation: string(core.IsolationSealed)},
		{Slug: "zeta", Isolation: string(core.IsolationOpen)},
	}, m.Locks.Projects, "console locks and the config's force locks are one sorted list")
	require.Equal(t, "seamless.test", m.Locks.PasskeyRPID)
	portable := map[string]bool{}
	for _, p := range m.Locks.Passkeys {
		portable[p.Name] = p.Portable
	}
	require.Equal(t, map[string]bool{"laptop": false, "icloud": true, "yubikey": true}, portable)

	body, err := json.Marshal(m)
	require.NoError(t, err)
	require.NotContains(t, string(body), "SECRET-KEY-BYTES")
}

func TestExport_NoLocksLeavesTheManifestAsItWas(t *testing.T) {
	_, m := exportWith(t, ExportOptions{DataDir: seedIndexed(t, specA)})
	require.Nil(t, m.Locks)
	body, err := json.Marshal(m)
	require.NoError(t, err)
	require.NotContains(t, string(body), `"locks"`)
}

func TestExport_NoDBStillRecordsLocks(t *testing.T) {
	src := seedIndexed(t, specA)
	lockInConsole(t, src, specA.project)
	_, m := exportWith(t, ExportOptions{DataDir: src, NoDB: true})
	require.False(t, m.IncludesDB)
	require.Equal(t, []LockedProject{{Slug: specA.project, Isolation: string(core.IsolationSealed)}}, m.Locks.Projects)
	require.Empty(t, m.LocksError)
}

func TestExport_NoDBWithoutADatabase(t *testing.T) {
	_, m := exportWith(t, ExportOptions{DataDir: t.TempDir(), NoDB: true, ForceLocked: []string{"zeta"}})
	require.Equal(t, []LockedProject{{Slug: "zeta"}}, m.Locks.Projects, "with no database only the config's locks are known")
	require.Empty(t, m.LocksError)
}

func TestImport_FreshKeepsConsoleLocksAndCarriesForceLocks(t *testing.T) {
	src := seedIndexed(t, specA)
	lockInConsole(t, src, specA.project)
	lockInConsole(t, src, "omega")
	db := openLockDB(t, src)
	require.NoError(t, store.RemoveConsoleLock(context.Background(), db, "omega")) // now only the file locks it
	require.NoError(t, db.Close())
	data, _ := exportWith(t, ExportOptions{DataDir: src, ForceLocked: []string{"omega"}})

	dest := filepath.Join(t.TempDir(), "restored")
	rep, err := importInto(t, dest, data, nil)
	require.NoError(t, err)

	require.Equal(t, []string{"alpha", "omega"}, consoleLocksIn(t, dest),
		"this machine's config does not hold omega's lock, so the import must")
	require.Equal(t, []string{"alpha", "omega"}, rep.Locks.Locked)
	require.Contains(t, rep.String(), "locked: alpha, omega")
	require.Equal(t, core.IsolationSealed, isolationIn(t, dest, "omega"))
	require.Len(t, lockEvents(t, dest, "omega"), 1, "the lock the import added is on the record")
	require.Empty(t, lockEvents(t, dest, "alpha"), "a lock that came in the snapshot was not changed")
}

func TestImport_DestinationForceLockWritesNoConsoleRow(t *testing.T) {
	src := seedIndexed(t, specA)
	data, _ := exportWith(t, ExportOptions{DataDir: src, ForceLocked: []string{specA.project}})
	dest := filepath.Join(t.TempDir(), "restored")
	rep, err := importInto(t, dest, data, func(o *ImportOptions) { o.ForceLocked = []string{specA.project} })
	require.NoError(t, err)
	require.Empty(t, consoleLocksIn(t, dest), "the file already holds it, and a console row could outlive the file's")
	require.Equal(t, []string{specA.project}, rep.Locks.Locked)
	require.True(t, isolationIn(t, dest, specA.project).FencesOutbound(),
		"the daemon refuses to start on a force locked project with no fence")
}

func TestImport_FreshRemoveLocks(t *testing.T) {
	src := seedIndexed(t, specA)
	lockInConsole(t, src, specA.project)
	data, _ := exportWith(t, ExportOptions{DataDir: src})
	dest := filepath.Join(t.TempDir(), "restored")
	rep, err := importInto(t, dest, data, func(o *ImportOptions) {
		o.OnLocks = func(context.Context, Mode, Manifest) (LockChoice, error) { return LocksRemove, nil }
	})
	require.NoError(t, err)
	require.Empty(t, consoleLocksIn(t, dest))
	require.Equal(t, []string{specA.project}, rep.Locks.Removed)
	require.Empty(t, rep.Locks.Locked)
	require.Contains(t, rep.String(), "locks removed: alpha")
	require.Equal(t, core.IsolationSealed, isolationIn(t, dest, specA.project), "the lock and the fence are two settings")
	require.Len(t, lockEvents(t, dest, specA.project), 1, "a lock taken off by an import is on the record")
}

func TestImport_FreshRemoveLeavesTheDestinationsForceLock(t *testing.T) {
	src := seedIndexed(t, specA)
	lockInConsole(t, src, specA.project)
	data, _ := exportWith(t, ExportOptions{DataDir: src})
	dest := filepath.Join(t.TempDir(), "restored")
	rep, err := importInto(t, dest, data, func(o *ImportOptions) {
		o.ForceLocked = []string{specA.project}
		o.OnLocks = func(context.Context, Mode, Manifest) (LockChoice, error) { return LocksRemove, nil }
	})
	require.NoError(t, err)
	require.Equal(t, []string{specA.project}, rep.Locks.StillLocked)
	require.Empty(t, rep.Locks.Removed)
}

func TestImport_NoDBArchiveFencesAndLocks(t *testing.T) {
	src := seedIndexed(t, specA)
	lockInConsole(t, src, specA.project)
	data, _ := exportWith(t, ExportOptions{DataDir: src, NoDB: true})

	for name, dest := range map[string]string{
		"fresh": filepath.Join(t.TempDir(), "restored"),
		"merge": seedIndexed(t, specB),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := importInto(t, dest, data, nil)
			require.NoError(t, err)
			require.Equal(t, []string{specA.project}, consoleLocksIn(t, dest))
			require.Equal(t, core.IsolationSealed, isolationIn(t, dest, specA.project),
				"a --no-db archive has no projects table, so its manifest carries the fence")
		})
	}
}

func TestImport_MergeCarriesLocks(t *testing.T) {
	src := seedIndexed(t, specA)
	lockInConsole(t, src, specA.project)
	addPasskeyRow(t, src, "laptop", true, "platform")
	data, _ := exportWith(t, ExportOptions{DataDir: src})
	dest := seedIndexed(t, specB)

	rep, err := importInto(t, dest, data, nil)
	require.NoError(t, err)
	require.Equal(t, []string{specA.project}, consoleLocksIn(t, dest),
		"a merge must not bring a locked project in open")
	require.Equal(t, core.IsolationSealed, isolationIn(t, dest, specA.project))
	require.Equal(t, []string{specA.project}, rep.Locks.Locked)

	creds, err := store.ApproverCredentials(context.Background(), openLockDB(t, dest), "")
	require.NoError(t, err)
	require.Empty(t, creds, "a passkey enrolled elsewhere was never shown to this owner, so a merge does not bring it")

	// The same archive again is a no-op, not a duplicate-lock failure.
	_, err = importInto(t, dest, data, nil)
	require.NoError(t, err)
	require.Len(t, lockEvents(t, dest, specA.project), 1)
}

func TestImport_MergeRaisesAnOpenProject(t *testing.T) {
	src := seedIndexed(t, specA)
	lockInConsole(t, src, specA.project)
	data, _ := exportWith(t, ExportOptions{DataDir: src})

	dest := seedIndexed(t, specB)
	db := openLockDB(t, dest)
	_, err := store.EnsureProject(context.Background(), db, specA.project, specA.project)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	rep, err := importInto(t, dest, data, nil)
	require.NoError(t, err)
	require.Equal(t, core.IsolationSealed, isolationIn(t, dest, specA.project),
		"locked content must not land in an open project")
	require.Equal(t, []string{specA.project}, consoleLocksIn(t, dest))
	require.Contains(t, strings.Join(rep.Warnings, "\n"), "was open here and becomes sealed")
}

func TestImport_MergeThatCannotLockWritesNothing(t *testing.T) {
	src := seedIndexed(t, specA)
	lockInConsole(t, src, specA.project)
	data, _ := exportWith(t, ExportOptions{DataDir: src})

	ctx := context.Background()
	dest := seedIndexed(t, specB)
	db := openLockDB(t, dest)
	for _, slug := range []string{specA.project, "kid"} {
		_, err := store.EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
	}
	require.NoError(t, store.SetProjectParent(ctx, db, "kid", specA.project, time.Now()))
	require.NoError(t, db.Close())
	before := countsOf(t, dest)
	files := corpusBytes(t, dest)

	_, err := importInto(t, dest, data, nil)
	require.ErrorIs(t, err, ErrLockCannotApply)
	require.Contains(t, err.Error(), "kid")
	require.Equal(t, before, countsOf(t, dest))
	require.Equal(t, files, corpusBytes(t, dest))
}

func TestImport_MergeRemoveLocks(t *testing.T) {
	src := seedIndexed(t, specA)
	lockInConsole(t, src, specA.project)
	lockInConsole(t, src, "shared")
	data, _ := exportWith(t, ExportOptions{DataDir: src})

	dest := seedIndexed(t, specB)
	lockInConsole(t, dest, "shared")

	rep, err := importInto(t, dest, data, func(o *ImportOptions) {
		o.OnLocks = func(context.Context, Mode, Manifest) (LockChoice, error) { return LocksRemove, nil }
	})
	require.NoError(t, err)
	require.Equal(t, []string{"shared"}, consoleLocksIn(t, dest), "the destination's own lock is not the archive's to remove")
	require.Equal(t, []string{specA.project}, rep.Locks.Removed)
	require.Equal(t, []string{"shared"}, rep.Locks.StillLocked)
}

func TestImport_SnapshotLockWithoutAManifestRecord(t *testing.T) {
	src := seedIndexed(t, specA)
	lockInConsole(t, src, specA.project)
	stale := exportStrippedOfLocks(t, src)

	dest := seedIndexed(t, specB)
	_, err := importInto(t, dest, stale, nil)
	require.NoError(t, err)
	require.Equal(t, []string{specA.project}, consoleLocksIn(t, dest), "the snapshot's lock still counts")
}

func TestImport_DryRunChangesNothing(t *testing.T) {
	src := seedIndexed(t, specA)
	lockInConsole(t, src, specA.project)
	data, _ := exportWith(t, ExportOptions{DataDir: src})

	t.Run("fresh", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "restored")
		rep, err := importInto(t, dest, data, func(o *ImportOptions) { o.DryRun = true })
		require.NoError(t, err)
		require.Equal(t, []string{specA.project}, rep.Locks.Locked)
		require.NoDirExists(t, dest)
	})
	t.Run("merge", func(t *testing.T) {
		dest := seedIndexed(t, specB)
		db := openLockDB(t, dest)
		_, err := store.EnsureProject(context.Background(), db, specA.project, specA.project)
		require.NoError(t, err)
		require.NoError(t, db.Close())
		before := countsOf(t, dest)

		rep, err := importInto(t, dest, data, func(o *ImportOptions) { o.DryRun = true })
		require.NoError(t, err)
		require.Equal(t, []string{specA.project}, rep.Locks.Locked)
		require.Contains(t, strings.Join(rep.Warnings, "\n"), "was open here", "the dry run names the change the real run makes")
		require.Equal(t, before, countsOf(t, dest))
		require.Equal(t, core.IsolationOpen, isolationIn(t, dest, specA.project))
	})
}

func TestImport_ARefusalAtTheGateWritesNothing(t *testing.T) {
	src := seedIndexed(t, specA)
	lockInConsole(t, src, specA.project)
	data, _ := exportWith(t, ExportOptions{DataDir: src})
	refused := errors.New("no passkey")

	for name, dest := range map[string]string{
		"fresh": filepath.Join(t.TempDir(), "restored"),
		"merge": seedIndexed(t, specB),
	} {
		t.Run(name, func(t *testing.T) {
			var before map[string]string
			if name == "merge" {
				before = corpusBytes(t, dest)
			}
			var asked Mode
			rep, err := importInto(t, dest, data, func(o *ImportOptions) {
				o.OnLocks = func(_ context.Context, mode Mode, m Manifest) (LockChoice, error) {
					asked = mode
					require.Equal(t, []string{specA.project}, m.Locks.Slugs())
					return LocksKeep, refused
				}
			})
			require.ErrorIs(t, err, refused)
			require.Nil(t, rep, "nothing happened, so there is nothing to report")
			require.Equal(t, Mode(name), asked)
			if name == "fresh" {
				require.NoDirExists(t, dest)
			} else {
				require.Equal(t, before, corpusBytes(t, dest))
			}
		})
	}
}

func TestImport_NoLocksNeverAsks(t *testing.T) {
	data, _ := exportWith(t, ExportOptions{DataDir: seedIndexed(t, specA)})
	rep, err := importInto(t, filepath.Join(t.TempDir(), "restored"), data, func(o *ImportOptions) {
		o.OnLocks = func(context.Context, Mode, Manifest) (LockChoice, error) {
			t.Fatal("an archive with no locks must not ask")
			return LocksKeep, nil
		}
	})
	require.NoError(t, err)
	require.Nil(t, rep.Locks)
	require.NotContains(t, rep.String(), "locked")
}

func TestImport_ANonSlugIsNeverShownOrApplied(t *testing.T) {
	src := seedIndexed(t, specA)
	data, _ := exportWith(t, ExportOptions{DataDir: src, ForceLocked: []string{"\x1b[31mevil"}})
	rep, err := importInto(t, filepath.Join(t.TempDir(), "restored"), data, func(o *ImportOptions) {
		o.OnLocks = func(context.Context, Mode, Manifest) (LockChoice, error) {
			t.Fatal("no real project is locked, so there is nothing to ask")
			return LocksKeep, nil
		}
	})
	require.NoError(t, err)
	require.Contains(t, strings.Join(rep.Warnings, "\n"), "non-slug project")
}

func TestImport_ReportsAnExportThatCouldNotReadLocks(t *testing.T) {
	src := seedIndexed(t, specA)
	data := exportStrippedOfLocks(t, src, func(m *Manifest) { m.LocksError = "database disk image is malformed" })
	rep, err := importInto(t, filepath.Join(t.TempDir(), "restored"), data, nil)
	require.NoError(t, err)
	require.Contains(t, strings.Join(rep.Warnings, "\n"), "could not read its locks")
}

// exportStrippedOfLocks is an archive whose manifest does not record the locks,
// as one written before the manifest did, edited by mods.
func exportStrippedOfLocks(t *testing.T, dir string, mods ...func(*Manifest)) []byte {
	t.Helper()
	data, _ := exportWith(t, ExportOptions{DataDir: dir})
	entries := readArchive(t, bytes.NewReader(data))
	var m Manifest
	require.NoError(t, json.Unmarshal(entries[0].data, &m))
	m.Locks = nil
	for _, mod := range mods {
		mod(&m)
	}
	var (
		hdrs   []tar.Header
		bodies []string
	)
	for _, e := range entries[1:] {
		hdrs = append(hdrs, e.hdr)
		bodies = append(bodies, string(e.data))
	}
	return craftArchive(t, m, hdrs, bodies)
}

// cancellingEmbedder stops the import from inside the corpus pass.
type cancellingEmbedder struct{ cancel context.CancelFunc }

func (e cancellingEmbedder) Embed(context.Context, string) ([]float32, error) {
	e.cancel()
	return nil, context.Canceled
}

func (e cancellingEmbedder) Model() string { return testEmbedModel }

func TestImport_AMergeStoppedInTheCorpusPassLeavesTheLockOn(t *testing.T) {
	src := seedIndexed(t, specA)
	lockInConsole(t, src, specA.project)
	data, _ := exportWith(t, ExportOptions{DataDir: src})
	dest := seedIndexed(t, specB)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := Import(ctx, ImportOptions{DataDir: dest, Src: bytes.NewReader(data), Embedder: cancellingEmbedder{cancel}})
	require.Error(t, err)
	require.Equal(t, []string{specA.project}, consoleLocksIn(t, dest),
		"content that arrived before the stop must already be behind the lock")
}

func TestImport_FreshWarnsThatApproversCameBack(t *testing.T) {
	src := seedIndexed(t, specA)
	addPasskeyRow(t, src, "lost-key", false, "platform")
	data, _ := exportWith(t, ExportOptions{DataDir: src})
	rep, err := importInto(t, filepath.Join(t.TempDir(), "restored"), data, nil)
	require.NoError(t, err)
	require.Contains(t, strings.Join(rep.Warnings, "\n"), "seamlessd approvers list")

	rep, err = importInto(t, filepath.Join(t.TempDir(), "restored"), exportOf(t, seedIndexed(t, specA)), nil)
	require.NoError(t, err)
	require.Empty(t, rep.Warnings, "a backup with no approvers has nothing to warn about")
}
