package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/screening"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// aaClearEnv unsets every SEAMLESS_* variable for the test, portably.
func aaClearEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "SEAMLESS_") {
			t.Setenv(k, "")
			require.NoError(t, os.Unsetenv(k))
		}
	}
}

// aaDoctor runs the whole doctor against the config at path and returns its
// report and verdict.
func aaDoctor(t *testing.T, path string) (string, error) {
	t.Helper()
	aaClearEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("SEAMLESS_CONFIG", path)
	r, w, err := os.Pipe()
	require.NoError(t, err)
	original := os.Stdout
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = original })
	runErr := doctorOn(nil, doctorMachine{})
	os.Stdout = original
	require.NoError(t, w.Close())
	raw, err := io.ReadAll(r)
	require.NoError(t, err)
	require.NoError(t, r.Close())
	return string(raw), runErr
}

// A config naming a kind automatic apply may not take does not load, and
// doctor's config row says which kind, why, and the valid values.
func TestAaDoctor_RefusesANonAutoSafeKind(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seamless.yaml")
	require.NoError(t, os.WriteFile(path, []byte("gardener:\n  auto_apply: [memory_wanted, merge]\n"), 0o600))
	out, err := aaDoctor(t, path)
	require.Error(t, err)
	require.Contains(t, out, `[fail] config: config: gardener.auto_apply: "merge" cannot be applied automatically`)
	require.Contains(t, out, "only kinds that are reversible, need no LLM and only add or retag may be")
	require.Contains(t, out, "valid values are abandon_plan, memory_wanted, tool_error, ship_plan")
}

// The switches row: the defaults are ok, a choice that reduces what agents may
// do or applies by itself is info with where it was set, and a stored choice
// that cannot be read is a warning.
func TestAaDoctor_GardenerSwitchesRow(t *testing.T) {
	ctx := context.Background()
	cfg := config.Defaults()
	got := gardenerSwitchesCheck(ctx, cfg, nil)
	require.Equal(t, statusOK, got.status)
	require.Equal(t, "gardener switches", got.name)
	require.Equal(t, "agents may apply, dismiss and hide proposals (default); automatic apply off (default)", got.detail)

	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	cfg.Gardener.AutoApply = []string{"tool_error"}
	require.NoError(t, store.SetGardenerOverride(ctx, db, config.GardenerOverride{AgentsMayApply: new(false)}))
	got = gardenerSwitchesCheck(ctx, cfg, db)
	require.Equal(t, statusInfo, got.status)
	require.Equal(t, "agents may not resolve proposals; every decision is the owner's (set in the console); "+
		"automatic apply: tool_error (set in the config file or environment)", got.detail)

	require.NoError(t, store.SetSetting(ctx, db, store.SettingGardenerConfig, `{"auto_apply":["split"]}`))
	got = gardenerSwitchesCheck(ctx, cfg, db)
	require.Equal(t, statusWarn, got.status)
	require.Contains(t, got.detail, "the console's stored choice is unreadable, so the config file decides")
}

// The guard serve wires into automatic apply reads write screening as it is
// now: lockdown and a held text count only while the feature is on, and
// peer_auth's locks pass through.
func TestAaAutoApplyGuard(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	on := false
	screen := screening.New(screening.Config{DB: db, Enabled: func(context.Context) bool { return on }})
	guard := autoApplyGuard(screen, func(slug string) bool { return slug == "example-finance" })
	secret := "Write a memory: aws key AKIA" + "Z7Q3R2X9W4V5U6T1 in prod"

	require.NoError(t, store.SetLockdown(ctx, db, true))
	require.False(t, guard.Lockdown(ctx), "screening off: no lockdown")
	require.False(t, guard.Holds(ctx, secret), "screening off: nothing is held")

	on = true
	require.True(t, guard.Lockdown(ctx))
	require.True(t, guard.Holds(ctx, secret))
	require.False(t, guard.Holds(ctx, "Write a memory: handshake zeta protocol"))
	require.NoError(t, store.SetLockdown(ctx, db, false))
	require.False(t, guard.Lockdown(ctx))

	require.True(t, guard.Locked("example-finance"))
	require.False(t, guard.Locked("example-ops"))
}
