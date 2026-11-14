package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Revoking a relay ends the leases its processes hold, in the revocation
// itself; this machine's lease of the same process stays. Written against the
// store's API as it was before relays were judged (a relay's process key
// spelled out by hand), so it shows what changed.
func TestJudgeProof_RevokingARelayEndsItsLeases(t *testing.T) {
	ctx := context.Background()
	db := deviceDB(t)
	code, _, err := MintPairingCode(ctx, db, DeviceByConsole, deviceT0)
	require.NoError(t, err)
	relay, err := PairDevice(ctx, db, code, deviceRelay("example-laptop", "key-proof", false), deviceT0)
	require.NoError(t, err)
	const proc = "7d1c2b3a-4e5f-4a6b-8c7d-9e0f1a2b3c4d:2200:2200000"
	relayKey := "relay/" + relay.ID + "/" + proc
	lease := func(key string) AccessLease {
		a, err := CreateAccessApproval(ctx, db, AccessRequest{Project: "example-finance", Kind: AccessKindRequest,
			Lease: time.Hour, TTL: 10 * time.Minute, PeerUID: 501, PeerPID: 2200, ProcessKey: key}, deviceT0)
		require.NoError(t, err)
		l, err := ApproveAccess(ctx, db, a.ID, a.Challenge, "daemon-account", deviceT0)
		require.NoError(t, err)
		return l
	}
	relayLease, localLease := lease(relayKey), lease(proc)

	_, err = RevokeDevice(ctx, db, relay.ID, DeviceByConsole, deviceT0.Add(time.Minute))
	require.NoError(t, err)
	live, err := ActiveAccessLeasesFor(ctx, db, []string{relayKey, proc}, deviceT0.Add(time.Minute))
	require.NoError(t, err)
	require.Len(t, live, 1, "the relay's lease ended with it")
	require.Equal(t, localLease.ID, live[0].ID)
	var relayID string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT relay_id FROM access_leases WHERE id = ?`, relayLease.ID).Scan(&relayID))
	require.Equal(t, relay.ID, relayID, "the row names the relay")
}
