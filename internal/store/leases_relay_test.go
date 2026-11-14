package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
)

// A relay's leases and grants (migration 043; plan networked-relay): every
// row a relay's process holds names the relay in its process key and in
// relay_id, and revoking the relay, turning a console lock back on and a
// restore each end them.

const judgeRelayBoot = "7d1c2b3a-4e5f-4a6b-8c7d-9e0f1a2b3c4d"

// judgeRelayTok is one of a relay's processes, relay "" for this machine's.
func judgeRelayTok(relay string, pid int32) access.ProcessToken {
	return access.ProcessToken{PID: pid, Version: uint64(pid) * 1000, Boot: judgeRelayBoot, Relay: relay}
}

// judgePairRelay pairs an active relay named name, with no passkey.
func judgePairRelay(t *testing.T, db *sql.DB, name string) Device {
	t.Helper()
	code, _, err := MintPairingCode(context.Background(), db, DeviceByConsole, deviceT0)
	require.NoError(t, err)
	d, err := PairDevice(context.Background(), db, code, deviceRelay(name, "key-"+name, false), deviceT0)
	require.NoError(t, err)
	return d
}

// judgeLease approves a lock's request of process for project at deviceT0,
// for an hour.
func judgeLease(t *testing.T, db *sql.DB, process access.ProcessToken, project string) AccessLease {
	t.Helper()
	ctx := context.Background()
	a, err := CreateAccessApproval(ctx, db, AccessRequest{Project: project, Kind: AccessKindRequest, Lease: time.Hour,
		TTL: 10 * time.Minute, PeerUID: 501, PeerPID: process.PID, ProcessKey: process.Key()}, deviceT0)
	require.NoError(t, err)
	l, err := ApproveAccess(ctx, db, a.ID, a.Challenge, "daemon-account", deviceT0)
	require.NoError(t, err)
	return l
}

// judgeGrant approves a read grant of project to process working from from.
func judgeGrant(t *testing.T, db *sql.DB, process access.ProcessToken, project, from string) AccessGrant {
	t.Helper()
	ctx := context.Background()
	a, err := CreateGrantRequest(ctx, db, GrantRequest{Project: project, From: from, Lease: time.Hour, TTL: 10 * time.Minute,
		PeerUID: 501, PeerPID: process.PID, ProcessKey: process.Key(), SessionID: "01SESSION" + project}, deviceT0)
	require.NoError(t, err)
	g, err := GrantAccessApprove(ctx, db, a.ID, a.Challenge, "daemon-account", 0, GrantAccessRead, deviceT0)
	require.NoError(t, err)
	return g
}

func judgeRelayColumn(t *testing.T, db *sql.DB, table, id string) string {
	t.Helper()
	var relay string
	require.NoError(t, db.QueryRowContext(context.Background(), `SELECT relay_id FROM `+table+` WHERE id = ?`, id).Scan(&relay))
	return relay
}

func TestRelayLeasesAndGrants_NameTheRelay(t *testing.T) {
	ctx := context.Background()
	db := deviceDB(t)
	laptop := judgePairRelay(t, db, "example-laptop")
	relayed, local := judgeRelayTok(laptop.ID, 2200), judgeRelayTok("", 2200)

	rl := judgeLease(t, db, relayed, "example-finance")
	require.Equal(t, laptop.ID, rl.RelayID)
	require.Equal(t, laptop.ID, judgeRelayColumn(t, db, "access_leases", rl.ID), "the row names the relay")
	ll := judgeLease(t, db, local, "example-finance")
	require.Empty(t, ll.RelayID)
	require.Empty(t, judgeRelayColumn(t, db, "access_leases", ll.ID), "'' is this machine's socket")

	got, err := ActiveAccessLeasesFor(ctx, db, []string{local.Key()}, deviceT0)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, ll.ID, got[0].ID, "this machine's process holds its own lease, never the relay's")
	got, err = ActiveAccessLeasesFor(ctx, db, []string{relayed.Key()}, deviceT0)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, rl.ID, got[0].ID)
	require.Equal(t, laptop.ID, got[0].RelayID)

	rg := judgeGrant(t, db, relayed, "example-legal", "example-finance")
	require.Equal(t, laptop.ID, rg.RelayID)
	require.Equal(t, laptop.ID, judgeRelayColumn(t, db, "access_grants", rg.ID))
	read, err := AccessGrantByID(ctx, db, rg.ID)
	require.NoError(t, err)
	require.Equal(t, laptop.ID, read.RelayID)
	lg := judgeGrant(t, db, local, "example-legal", "example-finance")
	require.Empty(t, judgeRelayColumn(t, db, "access_grants", lg.ID))
}

// The previous release names its columns in every insert: its rows take the
// default, this machine's socket.
func TestMigration043_PreviousReleaseInsertsStillWork(t *testing.T) {
	ctx := context.Background()
	db := deviceDB(t)
	at := core.FormatTime(deviceT0)
	_, err := db.ExecContext(ctx, `INSERT INTO access_approvals (id, project_slug, kind, challenge, status, requested_at,
		expires_at, lease_seconds, peer_uid, peer_pid, process_key) VALUES ('01OLDAPPROVAL', 'example-finance', 'request',
		zeroblob(32), 'approved', ?, ?, 60, 501, 42, 'b:42:1')`, at, at)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO access_leases (id, project_slug, process_key, peer_uid, peer_pid, session_id,
		approval_id, granted_at, expires_at) VALUES ('01OLDLEASE', 'example-finance', 'b:42:1', 501, 42, '', '01OLDAPPROVAL', ?, ?)`, at, at)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO access_grants (id, project_slug, from_project, process_key, session_id,
		peer_uid, peer_pid, approval_id, decided_by, granted_at, expires_at) VALUES ('01OLDGRANT', 'example-legal',
		'example-finance', 'b:42:1', '', 501, 42, '01OLDAPPROVAL', 'daemon-account', ?, ?)`, at, at)
	require.NoError(t, err)
	require.Empty(t, judgeRelayColumn(t, db, "access_leases", "01OLDLEASE"))
	require.Empty(t, judgeRelayColumn(t, db, "access_grants", "01OLDGRANT"))
}

// Revoking a relay ends, in its own transaction, its processes' live leases
// and grants and the requests they still wait on -- and nothing of another
// relay's or of this machine's. A key device and a relay already ended end
// nothing more.
func TestRevokeDevice_EndsTheRelaysAccess(t *testing.T) {
	ctx := context.Background()
	db := deviceDB(t)
	laptop := judgePairRelay(t, db, "example-laptop")
	desk := judgePairRelay(t, db, "example-desk")
	mine := judgeLease(t, db, judgeRelayTok(laptop.ID, 2200), "example-finance")
	mineGrant := judgeGrant(t, db, judgeRelayTok(laptop.ID, 2200), "example-legal", "example-finance")
	waiting, err := CreateAccessApproval(ctx, db, AccessRequest{Project: "example-ops", Kind: AccessKindRequest, Lease: time.Hour,
		TTL: time.Hour, PeerUID: 501, PeerPID: 2300, ProcessKey: judgeRelayTok(laptop.ID, 2300).Key()}, deviceT0)
	require.NoError(t, err)
	theirs := judgeLease(t, db, judgeRelayTok(desk.ID, 2200), "example-finance")
	local := judgeLease(t, db, judgeRelayTok("", 2200), "example-finance")
	localWaiting, err := CreateAccessApproval(ctx, db, AccessRequest{Project: "example-ops", Kind: AccessKindRequest, Lease: time.Hour,
		TTL: time.Hour, PeerUID: 501, PeerPID: 2300, ProcessKey: judgeRelayTok("", 2300).Key()}, deviceT0)
	require.NoError(t, err)

	now := deviceT0.Add(time.Minute)
	d, ended, err := RevokeDeviceEndingAccess(ctx, db, laptop.ID, DeviceByConsole, now)
	require.NoError(t, err)
	require.Equal(t, DeviceRevoked, d.Status)
	require.Equal(t, DeviceAccessEnded{Leases: 1, Grants: 1, Requests: 1}, ended)
	require.EqualValues(t, 3, ended.Total())

	for id, want := range map[string]string{mine.ID: LeaseRevokedDevice, theirs.ID: "", local.ID: ""} {
		l, err := AccessLeaseByID(ctx, db, id)
		require.NoError(t, err)
		require.Equal(t, want, l.RevokedReason, id)
	}
	g, err := AccessGrantByID(ctx, db, mineGrant.ID)
	require.NoError(t, err)
	require.NotNil(t, g.RevokedAt)
	require.Equal(t, LeaseRevokedDevice, g.RevokedReason)
	w, err := AccessApprovalByID(ctx, db, waiting.ID)
	require.NoError(t, err)
	require.Equal(t, ApprovalExpired, w.Status, "a waiting request of the relay can no longer be approved")
	_, err = ApproveAccess(ctx, db, waiting.ID, waiting.Challenge, "daemon-account", now)
	require.ErrorIs(t, err, ErrApprovalNotPending)
	lw, err := AccessApprovalByID(ctx, db, localWaiting.ID)
	require.NoError(t, err)
	require.Equal(t, ApprovalPending, lw.Status, "this machine's requests wait on")

	_, ended, err = RevokeDeviceEndingAccess(ctx, db, laptop.ID, DeviceByConsole, now)
	require.ErrorIs(t, err, ErrDeviceEnded)
	require.Zero(t, ended.Total())

	code, _, err := MintPairingCode(ctx, db, DeviceByConsole, deviceT0)
	require.NoError(t, err)
	key, err := PairDevice(ctx, db, code, deviceKeyed("example-ci", "abcdefgh", false), deviceT0)
	require.NoError(t, err)
	_, ended, err = RevokeDeviceEndingAccess(ctx, db, key.ID, DeviceByTerminal, now)
	require.NoError(t, err)
	require.Zero(t, ended.Total(), "a key device holds no lease of its own")
	l, err := AccessLeaseByID(ctx, db, theirs.ID)
	require.NoError(t, err)
	require.Nil(t, l.RevokedAt, "another relay's lease is untouched")
}

// A fresh restore revokes every device, each relay's access with it.
func TestRevokeAllDevices_EndsEveryRelaysAccess(t *testing.T) {
	ctx := context.Background()
	db := deviceDB(t)
	laptop := judgePairRelay(t, db, "example-laptop")
	relayLease := judgeLease(t, db, judgeRelayTok(laptop.ID, 2200), "example-finance")
	local := judgeLease(t, db, judgeRelayTok("", 2200), "example-finance")
	n, err := RevokeAllDevices(ctx, db, DeviceByRestore, deviceT0.Add(time.Minute))
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	l, err := AccessLeaseByID(ctx, db, relayLease.ID)
	require.NoError(t, err)
	require.Equal(t, LeaseRevokedDevice, l.RevokedReason)
	l, err = AccessLeaseByID(ctx, db, local.ID)
	require.NoError(t, err)
	require.Nil(t, l.RevokedAt)
}

// The re-lock rule (a console lock turned back on ends the project's access
// from before) and the restore rule (a restored database's live access ends)
// cover a relay's leases and grants as they cover this machine's.
func TestRelayAccess_RelockAndRestoreEndIt(t *testing.T) {
	ctx := context.Background()
	db := deviceDB(t)
	_, err := EnsureProject(ctx, db, "example-legal", "example-legal")
	require.NoError(t, err)
	require.NoError(t, SetProjectIsolation(ctx, db, "example-legal", core.IsolationConfidential))
	laptop := judgePairRelay(t, db, "example-laptop")
	relayed := judgeRelayTok(laptop.ID, 2200)

	onLegal := judgeLease(t, db, relayed, "example-legal")
	grantLegal := judgeGrant(t, db, relayed, "example-legal", "example-finance")
	ended, err := AddConsoleLockEndingAccess(ctx, db, "example-legal", "01PASSKEY", deviceT0.Add(time.Minute))
	require.NoError(t, err)
	require.EqualValues(t, 1, ended.Leases)
	require.EqualValues(t, 1, ended.Grants)
	l, err := AccessLeaseByID(ctx, db, onLegal.ID)
	require.NoError(t, err)
	require.Equal(t, LeaseRevokedLocked, l.RevokedReason, "the re-lock rule ends a relay's lease")
	g, err := AccessGrantByID(ctx, db, grantLegal.ID)
	require.NoError(t, err)
	require.Equal(t, LeaseRevokedLocked, g.RevokedReason)

	onFin := judgeLease(t, db, relayed, "example-finance")
	grantOps := judgeGrant(t, db, relayed, "example-ops", "example-finance")
	got, err := ResetRestoredAccess(ctx, db, deviceT0.Add(2*time.Minute))
	require.NoError(t, err)
	require.EqualValues(t, 1, got.Leases)
	require.EqualValues(t, 1, got.Grants)
	l, err = AccessLeaseByID(ctx, db, onFin.ID)
	require.NoError(t, err)
	require.Equal(t, LeaseRevokedReset, l.RevokedReason, "the restore rule ends a relay's lease")
	g, err = AccessGrantByID(ctx, db, grantOps.ID)
	require.NoError(t, err)
	require.Equal(t, LeaseRevokedReset, g.RevokedReason)
}
