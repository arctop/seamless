package feed

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// A console lock change is a config change a monitor must not miss: high,
// the owner's when a passkey made it, with the access it ended. A failed one
// is an auth failure, high when it could only be an attack.
func TestFrames_ConsoleLocks(t *testing.T) {
	m := testMapper()
	ts := time.Date(2026, 11, 15, 3, 0, 0, 0, time.UTC)
	cred := "01KX0000000000000000000041"
	changed := m.frame(core.Event{ID: "01KX0000000000000000000040", TS: ts, Kind: core.EventConsoleLockChanged,
		ItemID: cred, ProjectSlug: "vault-project", Payload: map[string]any{
			"proof": "passkey", "action": "lock", "by": "console", "leases": 2, "grants": 1, "unlocks": 3}}, session{})
	require.NoError(t, changed.validate())
	require.Equal(t, "config", changed.Class)
	require.Equal(t, "high", changed.Severity)
	require.Equal(t, "success", changed.Outcome)
	require.Equal(t, "user", changed.Actor.Kind, "a passkey's change is the owner's")
	require.True(t, strings.HasPrefix(changed.Target.Asset, "h:"), "a locked project leaves as a keyed hash")
	for k, v := range map[string]any{"action": "lock", "proof": "passkey", "item_id": cred, "leases": 2, "grants": 1, "unlocks": 3} {
		require.EqualValues(t, v, changed.Fields[k], k)
	}

	imported := m.frame(core.Event{TS: ts, Kind: core.EventConsoleLockChanged, ProjectSlug: "vault-project",
		Payload: map[string]any{"proof": "import", "action": "remove"}}, session{})
	require.Equal(t, "high", imported.Severity)
	require.Equal(t, "system", imported.Actor.Kind, "an import's change is no passkey's")

	for reason, severity := range map[string]string{
		"assertion": "medium", "credential": "medium", "race": "medium", "state": "medium", "error": "medium",
		"mismatch": "high", "counter": "high",
	} {
		failed := m.frame(core.Event{TS: ts, Kind: core.EventConsoleLockFailed, ProjectSlug: "vault-project",
			Payload: map[string]any{"action": "remove", "reason": reason}}, session{})
		require.NoError(t, failed.validate(), reason)
		require.Equal(t, "auth", failed.Class, reason)
		require.Equal(t, "failure", failed.Outcome, reason)
		require.Equal(t, severity, failed.Severity, reason)
		require.Equal(t, reason, failed.Fields["reason"])
		require.NotEqual(t, "user", failed.Actor.Kind, "a failed change is never said to be the owner's")
	}
}
