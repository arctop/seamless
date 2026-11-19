package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/shellcmd"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// mrTestDB is a fresh database whose daemon adopted the host alpha.
func mrTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, store.AdoptLocalHost(context.Background(), db, "alpha"))
	return db
}

// mrPairRelay records an active relay named name, as a pairing the code
// approved.
func mrPairRelay(t *testing.T, db *sql.DB, name string) {
	t.Helper()
	ctx := context.Background()
	hash := func(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
	code, _, err := store.MintPairingCode(ctx, db, store.DeviceByConsole, time.Now())
	require.NoError(t, err)
	_, err = store.PairDevice(ctx, db, code, store.NewDevice{Kind: store.DeviceRelayKind, Name: name,
		Fingerprint: store.ApproverKeyFingerprint([]byte(name)), PublicKey: []byte(name), KeyHash: hash("key:" + name),
		SecretHash: hash("secret:" + name)}, time.Now())
	require.NoError(t, err)
}

// mrRow is the mapping host has for path, if any.
func mrRow(t *testing.T, db *sql.DB, host, path string) (store.RepoMapRow, bool) {
	t.Helper()
	rows, err := store.RepoMapRows(context.Background(), db)
	require.NoError(t, err)
	for _, r := range rows {
		if r.Host == host && r.Path == path {
			return r, true
		}
	}
	return store.RepoMapRow{}, false
}

func TestMapRepo_HostMapsAPathOnAnotherMachine(t *testing.T) {
	ctx := context.Background()
	db := mrTestDB(t)
	mrPairRelay(t, db, "example-laptop")
	var out, warn bytes.Buffer

	// The relay's name in any case, its path as given: that machine's row, never
	// stat'd, and the project registered.
	require.NoError(t, mapRepo(ctx, db, &out, &warn, "/Users/example-agent/repos/app/", "example-app", " Example-Laptop "))
	require.Equal(t, "mapped /Users/example-agent/repos/app (host \"example-laptop\") -> project \"example-app\"\n", out.String())
	require.Empty(t, warn.String(), "a paired relay's name is known")
	row, ok := mrRow(t, db, "example-laptop", "/Users/example-agent/repos/app")
	require.True(t, ok)
	require.Equal(t, "example-app", row.Slug)
	_, found, err := store.ProjectBySlug(ctx, db, "example-app")
	require.NoError(t, err)
	require.True(t, found, "mapping a new slug registers it")

	// It resolves for that host alone; this machine's view and the legacy mirror
	// are untouched.
	got, err := store.ResolveProjectForCWD(ctx, db, "example-laptop", "/Users/example-agent/repos/app/src")
	require.NoError(t, err)
	require.Equal(t, "example-app", got)
	got, err = store.ResolveProjectForCWD(ctx, db, "alpha", "/Users/example-agent/repos/app/src")
	require.NoError(t, err)
	require.Empty(t, got)
	local, err := store.LocalRepoMappings(ctx, db)
	require.NoError(t, err)
	require.Empty(t, local)

	// Mapping it again to another project re-points the same row.
	out.Reset()
	require.NoError(t, mapRepo(ctx, db, &out, &warn, "/Users/example-agent/repos/app", "example-other", "example-laptop"))
	row, _ = mrRow(t, db, "example-laptop", "/Users/example-agent/repos/app")
	require.Equal(t, "example-other", row.Slug)

	// A Windows computer's path is absolute there, whatever this one runs.
	require.NoError(t, mapRepo(ctx, db, &out, &warn, "C:/repos/app", "example-app", "example-laptop"))
	_, ok = mrRow(t, db, "example-laptop", `C:\repos\app`)
	require.True(t, ok)
}

func TestMapRepo_HostRefusesAPathItCannotPlace(t *testing.T) {
	ctx := context.Background()
	db := mrTestDB(t)
	var out, warn bytes.Buffer

	err := mapRepo(ctx, db, &out, &warn, "", "example-app", "example-laptop")
	require.ErrorContains(t, err, "--host needs --path")
	err = mapRepo(ctx, db, &out, &warn, "repos/app", "example-app", "example-laptop")
	require.ErrorContains(t, err, `--path must be absolute with --host: "repos/app" is a path on example-laptop, not here`)
	rows, err := store.RepoMapRows(ctx, db)
	require.NoError(t, err)
	require.Empty(t, rows, "a refusal writes nothing")
	require.Empty(t, out.String())
}

// A host nothing has named is mapped all the same (a relay may pair later),
// with a warning that names the paired devices.
func TestMapRepo_HostNothingNamedWarns(t *testing.T) {
	ctx := context.Background()
	db := mrTestDB(t)
	var out, warn bytes.Buffer

	require.NoError(t, mapRepo(ctx, db, &out, &warn, "/home/me/app", "example-app", "example-laptpo"))
	require.Contains(t, warn.String(), `no paired device and no session has named host "example-laptpo"`)
	require.Contains(t, warn.String(), "this server has no paired device.")
	_, ok := mrRow(t, db, "example-laptpo", "/home/me/app")
	require.True(t, ok)

	mrPairRelay(t, db, "example-laptop")
	warn.Reset()
	require.NoError(t, mapRepo(ctx, db, &out, &warn, "/home/me/other", "example-app", "example-desk"))
	require.Contains(t, warn.String(), "(paired here: example-laptop).")

	// A host a session came from, or one a mapping already names, is known,
	// paired or not.
	now := time.Now().UTC()
	require.NoError(t, store.CreateSession(ctx, db, core.Session{ID: "01KMRSESSKEY", Name: "cc/k", Host: "example-keyclient",
		Status: core.SessionActive, CreatedAt: now, UpdatedAt: now}))
	warn.Reset()
	require.NoError(t, mapRepo(ctx, db, &out, &warn, "/srv/y", "example-app", "example-keyclient"))
	require.Empty(t, warn.String(), "a host a session came from is known")
	_, _, err := store.RegisterProjectForCWD(ctx, db, store.CWDIdentity{Host: "example-other", CWD: "/srv/x", RepoRoot: "/srv/x"}, "alpha")
	require.NoError(t, err)
	require.NoError(t, mapRepo(ctx, db, &out, &warn, "/srv/z", "example-app", "example-other"))
	require.Empty(t, warn.String(), "a host a mapping already names is known")
}

// Without --host, or with this machine's own name, the path is this machine's,
// made absolute here. One this disk lacks is mapped, with a note that names
// --host and the paired relays.
func TestMapRepo_ThisMachine(t *testing.T) {
	ctx := context.Background()
	db := mrTestDB(t)
	var out, warn bytes.Buffer

	repo := t.TempDir()
	require.NoError(t, mapRepo(ctx, db, &out, &warn, repo, "example-app", ""))
	require.Empty(t, warn.String(), "a path on this disk needs no note")
	require.NoError(t, mapRepo(ctx, db, &out, &warn, repo, "example-app", "ALPHA"))
	local, err := store.LocalRepoMappings(ctx, db)
	require.NoError(t, err)
	require.Len(t, local, 1)
	require.Equal(t, "alpha", local[0].Host)

	mrPairRelay(t, db, "example-laptop")
	gone := filepath.Join(t.TempDir(), "gone")
	require.NoError(t, mapRepo(ctx, db, &out, &warn, gone, "example-app", ""))
	_, ok := mrRow(t, db, "alpha", gone)
	require.True(t, ok)
	require.Contains(t, warn.String(), "note: "+gone+" does not exist on this machine")
	require.Contains(t, warn.String(), "seamlessd map-repo --host NAME --path "+shellcmd.PosixArg(gone)+" --project example-app")
	require.Contains(t, warn.String(), "(paired here: example-laptop).")
	require.Contains(t, warn.String(), "seamlessd unmap-repo --path "+shellcmd.PosixArg(gone)+" removes this machine's mapping")
}
