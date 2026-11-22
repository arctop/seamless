package store

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
)

// The gardener override round-trips both fields, a stored false and an empty
// kinds list included (each is a choice, not an absent field); clearing hands
// both back to the file.
func TestAaGardenerOverride_RoundTrip(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	got, found, err := GardenerOverride(ctx, db)
	require.NoError(t, err)
	require.False(t, found)
	require.Equal(t, config.GardenerOverride{}, got)

	none := []string{}
	saved := config.GardenerOverride{AgentsMayApply: new(false), AutoApply: &none}
	require.NoError(t, SetGardenerOverride(ctx, db, saved))
	got, found, err = GardenerOverride(ctx, db)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, saved, got)

	require.NoError(t, SetGardenerOverride(ctx, db, config.GardenerOverride{AutoApply: &[]string{"memory_wanted"}}))
	raw, _, err := GetSetting(ctx, db, SettingGardenerConfig)
	require.NoError(t, err)
	require.JSONEq(t, `{"auto_apply":["memory_wanted"]}`, raw)

	require.NoError(t, ClearGardenerOverride(ctx, db))
	require.NoError(t, ClearGardenerOverride(ctx, db), "clearing twice is a no-op")
	_, found, err = GardenerOverride(ctx, db)
	require.NoError(t, err)
	require.False(t, found)
}

// A kind automatic apply may not take is never stored, and a row that names
// one (a hand edit) or does not decode reads as unreadable, never as a choice.
func TestAaGardenerOverride_RefusesAndUnreadable(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	err := SetGardenerOverride(ctx, db, config.GardenerOverride{AutoApply: &[]string{"merge"}})
	require.ErrorContains(t, err, `"merge" cannot be applied automatically`)
	_, found, err := GetSetting(ctx, db, SettingGardenerConfig)
	require.NoError(t, err)
	require.False(t, found, "nothing is stored")

	for _, raw := range []string{`{not json`, `{"agents_may_apply":"no"}`, `{"auto_apply":["split"]}`} {
		require.NoError(t, SetSetting(ctx, db, SettingGardenerConfig, raw))
		_, _, err := GardenerOverride(ctx, db)
		require.True(t, errors.Is(err, ErrGardenerOverrideInvalid), "%s: %v", raw, err)
	}
	require.NoError(t, SetSetting(ctx, db, SettingGardenerConfig, "  "))
	_, found, err = GardenerOverride(ctx, db)
	require.NoError(t, err)
	require.False(t, found, "a blank row is no row")
}
