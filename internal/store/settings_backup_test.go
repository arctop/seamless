package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
)

func TestBackupOverride(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	dir := t.TempDir()

	// No row: nothing is overridden, and there is no error to fall back from.
	got, found, err := BackupOverride(ctx, db)
	require.NoError(t, err)
	require.False(t, found)
	require.Equal(t, config.BackupOverride{}, got)

	// A saved override round-trips every pointer, false and the empty dir
	// included: each is a statement, not an absent field.
	every := config.Duration(6 * time.Hour)
	saved := config.BackupOverride{Auto: new(false), Every: &every, Keep: new(14), Dir: new(dir)}
	require.NoError(t, SetBackupOverride(ctx, db, saved))
	got, found, err = BackupOverride(ctx, db)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, saved, got)

	// every is stored as a duration string, never nanoseconds; a nil field is
	// left out of the row and reads back nil. The row itself still exists.
	require.NoError(t, SetBackupOverride(ctx, db, config.BackupOverride{Every: &every, Dir: new("")}))
	raw, _, err := GetSetting(ctx, db, SettingBackupConfig)
	require.NoError(t, err)
	require.JSONEq(t, `{"every":"6h","dir":""}`, raw)
	got, found, err = BackupOverride(ctx, db)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, &every, got.Every)
	require.Equal(t, new(""), got.Dir)
	require.Nil(t, got.Auto)
	require.Nil(t, got.Keep)

	// An override Validate refuses is never stored: the row stays as it was.
	for _, bad := range []config.BackupOverride{
		{Keep: new(0)},
		{Keep: new(101)},
		{Every: new(config.Duration(30 * time.Minute))},
		{Dir: new("relative/backups")},
	} {
		require.ErrorContains(t, SetBackupOverride(ctx, db, bad), "store.SetBackupOverride: config: backup.")
	}
	raw, _, err = GetSetting(ctx, db, SettingBackupConfig)
	require.NoError(t, err)
	require.JSONEq(t, `{"every":"6h","dir":""}`, raw)

	// Clearing removes the row; clearing twice is a no-op.
	require.NoError(t, ClearBackupOverride(ctx, db))
	require.NoError(t, ClearBackupOverride(ctx, db))
	got, found, err = BackupOverride(ctx, db)
	require.NoError(t, err)
	require.False(t, found)
	require.Equal(t, config.BackupOverride{}, got)
}

func TestBackupOverride_StoredRows(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		want      config.BackupOverride
		wantFound bool
		wantErr   string // "" = no error
	}{
		{name: "blank row reads as no row", raw: "  ", wantFound: false},
		{name: "empty object overrides nothing", raw: `{}`, wantFound: true},
		{name: "field written by a newer version is ignored", raw: `{"keep":3,"copy_to":"/elsewhere"}`,
			want: config.BackupOverride{Keep: new(3)}, wantFound: true},
		// A corrupt row is an error, never a zero override that looks real: the
		// caller logs it and falls back to the file/env values.
		{name: "corrupt row", raw: `{not json`, wantErr: "store.BackupOverride: decode"},
		{name: "wrong shape", raw: `[true]`, wantErr: "store.BackupOverride: decode"},
		{name: "wrong field type", raw: `{"auto":"yes"}`, wantErr: "store.BackupOverride: decode"},
		{name: "every as nanoseconds", raw: `{"every":86400000000000}`, wantErr: "store.BackupOverride: decode"},
		{name: "every as a bare number string", raw: `{"every":"24"}`, wantErr: "store.BackupOverride: decode"},
		// So is a row that decodes but holds a value the daemon cannot use.
		{name: "keep out of range", raw: `{"keep":0}`, wantErr: "store.BackupOverride: config: backup.keep: 0 is outside [1, 100]"},
		{name: "every out of range", raw: `{"every":"10m"}`, wantErr: "store.BackupOverride: config: backup.every: 10m is outside [1h, 720h]"},
		{name: "relative dir", raw: `{"dir":"backups"}`, wantErr: "store.BackupOverride: config: backup.dir must be an absolute path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := openTestDB(t)
			ctx := context.Background()
			require.NoError(t, SetSetting(ctx, db, SettingBackupConfig, tt.raw))

			got, found, err := BackupOverride(ctx, db)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				require.False(t, found)
				require.Equal(t, config.BackupOverride{}, got)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.wantFound, found)
			require.Equal(t, tt.want, got)
		})
	}
}
