package store

import (
	"context"
	"database/sql"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
)

func TestFeaturesConfigOverride(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	base := config.Defaults().Features

	// No override row: the base config passes through untouched, and the base
	// on a fresh install is every optional feature off.
	got, overridden, err := FeaturesConfig(ctx, db, base)
	require.NoError(t, err)
	require.False(t, overridden)
	require.Equal(t, base, got)
	require.False(t, got.Research)

	// A saved override layers over the base and round-trips.
	require.NoError(t, SetFeaturesConfig(ctx, db, config.Features{Research: true}))
	got, overridden, err = FeaturesConfig(ctx, db, base)
	require.NoError(t, err)
	require.True(t, overridden)
	require.True(t, got.Research)

	// The override wins in both directions: a stored off beats a file/env on.
	require.NoError(t, SetFeaturesConfig(ctx, db, config.Features{Research: false}))
	got, _, err = FeaturesConfig(ctx, db, config.Features{Research: true})
	require.NoError(t, err)
	require.False(t, got.Research, "the stored override wins over the file/env base")

	// A partial row (written before a feature existed) keeps base values for
	// absent fields instead of zeroing them off.
	require.NoError(t, SetSetting(ctx, db, SettingFeaturesConfig, `{}`))
	got, overridden, err = FeaturesConfig(ctx, db, config.Features{Research: true})
	require.NoError(t, err)
	require.True(t, overridden)
	require.True(t, got.Research, "an absent field keeps its base value")

	// A corrupt row errors (and reports the base) rather than silently zeroing
	// every feature off.
	require.NoError(t, SetSetting(ctx, db, SettingFeaturesConfig, `{not json`))
	_, _, err = FeaturesConfig(ctx, db, config.Features{Research: true})
	require.Error(t, err)

	// Clearing reverts to the base; clearing twice is a no-op.
	require.NoError(t, ClearFeaturesConfig(ctx, db))
	require.NoError(t, ClearFeaturesConfig(ctx, db))
	got, overridden, err = FeaturesConfig(ctx, db, base)
	require.NoError(t, err)
	require.False(t, overridden)
	require.Equal(t, base, got)
}

// TestFeaturesConfigStoredShapeMatchesMigration pins the JSON the grandfather
// migration writes to the shape SetFeaturesConfig produces: the migration is raw
// SQL and cannot see the struct tags.
func TestFeaturesConfigStoredShapeMatchesMigration(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	require.NoError(t, SetFeaturesConfig(ctx, db, config.Features{Research: true}))
	raw, found, err := GetSetting(ctx, db, SettingFeaturesConfig)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, `{"research":true,"momentum":false,"gamification":false,"screening":false}`, raw,
		"the migration's seeded {\"research\":true} decodes over this shape: absent keys stay at the base value")
}

// TestMigration022_GrandfathersExistingResearchData upgrades a database that
// already holds trials and verifies the seeded override keeps research on --
// the default is off, and an upgrade must never hide data in use.
func TestMigration022_GrandfathersExistingResearchData(t *testing.T) {
	for _, tc := range []struct {
		name       string
		seedTrial  bool
		existing   string // a features_config row present before the migration
		wantSeeded bool
		wantOn     bool
	}{
		{name: "trials present seeds research on", seedTrial: true, wantSeeded: true, wantOn: true},
		{name: "fresh database seeds nothing", seedTrial: false},
		{
			name:       "existing override is never overwritten",
			seedTrial:  true,
			existing:   `{"research":false}`,
			wantSeeded: true,
			wantOn:     false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := openPartialDB(t, 22)
			ctx := context.Background()

			if tc.seedTrial {
				_, err := db.Exec(`INSERT INTO trials (id, lab, created_at)
					VALUES ('01TRIAL', 'boot-race', '2026-07-01T00:00:00Z')`)
				require.NoError(t, err)
			}
			if tc.existing != "" {
				require.NoError(t, SetSetting(ctx, db, SettingFeaturesConfig, tc.existing))
			}

			require.NoError(t, migrate(db, migrationList()))

			got, overridden, err := FeaturesConfig(ctx, db, config.Defaults().Features)
			require.NoError(t, err)
			require.Equal(t, tc.wantSeeded, overridden)
			require.Equal(t, tc.wantOn, got.Research)
		})
	}
}

// TestMigration022_IsOneTime proves the seed does not resurrect a feature the
// owner turned off after upgrading: the row exists by then, so a re-run (a
// second daemon start on the same database) leaves it alone.
func TestMigration022_IsOneTime(t *testing.T) {
	db := openPartialDB(t, 22)
	ctx := context.Background()

	_, err := db.Exec(`INSERT INTO trials (id, lab, created_at)
		VALUES ('01TRIAL', 'boot-race', '2026-07-01T00:00:00Z')`)
	require.NoError(t, err)
	require.NoError(t, migrate(db, migrationList()))

	// The owner turns it off in the console.
	require.NoError(t, SetFeaturesConfig(ctx, db, config.Features{Research: false}))
	// A later start re-runs nothing (the version is recorded), and even a forced
	// re-apply of the statement is guarded by the row's existence.
	require.NoError(t, migrate(db, migrationList()))
	got, _, err := FeaturesConfig(ctx, db, config.Defaults().Features)
	require.NoError(t, err)
	require.False(t, got.Research)
}

// openPartialDB opens a raw database with every migration BEFORE version
// applied, so a test can seed pre-upgrade state and then run the migration under
// test. Cutting the list at the version rather than at a fixed index keeps later
// migrations from silently re-pointing the test.
func openPartialDB(t *testing.T, version int) *sql.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "seam.db")
	dsn := "file:" + url.PathEscape(dbPath) + "?_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	all := migrationList()
	before := 0
	for i, m := range all {
		if m.Version == version {
			before = i
			break
		}
	}
	require.NotZero(t, before, "migration %d must exist and must not be the first", version)
	require.NoError(t, migrate(db, all[:before]))
	return db
}
