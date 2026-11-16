package console

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// A lock change, a failed one, and the access a lock ended each read as a
// line of their own on the activity feed, never as the bare event kind.
func TestEventSummary_ConsoleLocks(t *testing.T) {
	for _, tc := range []struct {
		name    string
		kind    core.EventKind
		payload map[string]any
		want    string
	}{
		{"a lock that ended access", core.EventConsoleLockChanged,
			map[string]any{"proof": approval.ProofPasskey, "action": "lock", "by": "console", "leases": 2, "unlocks": 1},
			"project locked with a passkey; ended 2 agent leases, 1 browser unlock"},
		{"a lock removed", core.EventConsoleLockChanged,
			map[string]any{"proof": approval.ProofPasskey, "action": "remove", "by": "console"},
			"console lock removed with a passkey"},
		{"an import's lock", core.EventConsoleLockChanged,
			map[string]any{"proof": "import", "action": "lock", "grants": 1},
			"project locked by an import; ended 1 read grant"},
		{"a failed removal", core.EventConsoleLockFailed,
			map[string]any{"action": "remove", "reason": approval.LockFailMismatch},
			"console lock removal failed: its approval was sent for another project or direction"},
		{"a cloned key", core.EventConsoleLockFailed,
			map[string]any{"action": "lock", "reason": approval.LockFailCounter},
			"console lock failed: the passkey's counter did not move on, as a cloned key's does not"},
		{"an unknown reason is shown as it is", core.EventConsoleLockFailed,
			map[string]any{"action": "lock", "reason": "newer"},
			"console lock failed: newer"},
		{"leases a lock ended", core.EventAccessRevoked,
			map[string]any{"reason": store.LeaseRevokedLocked, "leases": 2, "grants": 1},
			"ended 2 agent leases, 1 read grant early: the project was locked"},
		{"a session's grants", core.EventAccessRevoked,
			map[string]any{"reason": store.LeaseRevokedSessionEnded, "grants": 3},
			"ended 3 read grants early: its session ended"},
		{"no counts", core.EventAccessRevoked,
			map[string]any{"reason": "other"},
			"ended access early (other)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, eventSummary(core.Event{Kind: tc.kind, Payload: tc.payload}))
		})
	}
}
