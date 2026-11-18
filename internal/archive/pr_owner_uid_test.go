package archive

// An archive from before migration 050 (a relay's owner uid, owner decision
// R-D1) imports as any older archive does: a fresh import migrates its
// snapshot forward, where every relay reads no owner uid, and then revokes
// every device it brought; a merge carries no device at all.

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

	"github.com/arctop/seamless/internal/store"
)

// prArchiveBefore050 is specA's instance with a relay paired naming uid 501,
// then put back in schema 49's shape: the owner_uid column gone. It returns
// the data dir and the relay.
func prArchiveBefore050(t *testing.T) (string, store.Device) {
	t.Helper()
	ctx := context.Background()
	src := seedIndexed(t, specA)
	db, err := store.Open(filepath.Join(src, DBName))
	require.NoError(t, err)
	code, _, err := store.MintPairingCode(ctx, db, store.DeviceByConsole, time.Now())
	require.NoError(t, err)
	sum := func(s string) string {
		h := sha256.Sum256([]byte(s))
		return hex.EncodeToString(h[:])
	}
	d, err := store.PairDevice(ctx, db, code, store.NewDevice{Kind: store.DeviceRelayKind, Name: "example-laptop",
		Fingerprint: store.ApproverKeyFingerprint([]byte("relay")), PublicKey: []byte("relay"), KeyHash: sum("pub:relay"),
		SecretHash: sum("secret:relay"), OwnerUID: 501}, time.Now())
	require.NoError(t, err)
	require.Equal(t, uint32(501), d.OwnerUID)
	for _, q := range []string{`ALTER TABLE devices DROP COLUMN owner_uid`, `DELETE FROM schema_migrations WHERE version >= 50`} {
		_, err := db.ExecContext(ctx, q)
		require.NoError(t, err, q)
	}
	v, err := store.SchemaVersion(db)
	require.NoError(t, err)
	require.Equal(t, 49, v)
	require.NoError(t, db.Close())
	return src, d
}

func TestPRImport_FreshFromBefore050(t *testing.T) {
	ctx := context.Background()
	src, d := prArchiveBefore050(t)
	dst := filepath.Join(t.TempDir(), "restored")
	_, err := Import(ctx, ImportOptions{DataDir: dst, Src: bytes.NewReader(exportOf(t, src))})
	require.NoError(t, err)
	db, err := store.Open(filepath.Join(dst, DBName))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	got, err := store.DeviceByID(ctx, db, d.ID)
	require.NoError(t, err)
	require.Equal(t, store.DeviceRevoked, got.Status, "no device survives a restore")
	require.Zero(t, got.OwnerUID, "migrated forward from a schema that had no owner uid")
}

func TestPRImport_MergeFromBefore050(t *testing.T) {
	ctx := context.Background()
	src, _ := prArchiveBefore050(t)
	dst := seedIndexed(t, specB)
	rep, err := Import(ctx, ImportOptions{DataDir: dst, Src: bytes.NewReader(exportOf(t, src))})
	require.NoError(t, err)
	require.Equal(t, ModeMerge, rep.Mode)
	out, err := store.OpenExisting(filepath.Join(dst, DBName))
	require.NoError(t, err)
	t.Cleanup(func() { _ = out.Close() })
	var n int
	require.NoError(t, out.QueryRowContext(ctx, `SELECT COUNT(*) FROM devices`).Scan(&n))
	require.Zero(t, n, "a merge carries no device")
	var col sql.NullString
	require.NoError(t, out.QueryRowContext(ctx, `SELECT name FROM pragma_table_info('devices') WHERE name = 'owner_uid'`).Scan(&col))
	require.Equal(t, "owner_uid", col.String, "the destination keeps its own schema")
}
