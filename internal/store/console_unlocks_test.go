package store

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
)

var unlockT0 = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

func TestMigration033_ConsoleUnlocks(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	var v, n int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v))
	require.GreaterOrEqual(t, v, 33)
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM console_unlocks`).Scan(&n))
	require.Zero(t, n, "the table starts empty")
	insert := func(status string, challenge []byte, secs int) error {
		_, err := db.ExecContext(ctx, `INSERT INTO console_unlocks (id, project_slug, status, challenge, lease_seconds,
			created_at, pending_until) VALUES (?, 'p', ?, ?, ?, 'x', 'x')`, core.FormatTime(time.Now())+status, status, challenge, secs)
		return err
	}
	require.Error(t, insert("granted", nil, 60), "the status is one of three")
	require.Error(t, insert("pending", make([]byte, 31), 60), "a challenge is exactly 32 bytes")
	require.Error(t, insert("pending", nil, 0), "an unlock lasts at least a second")
	require.NoError(t, insert("pending", make([]byte, 32), 60))
}

func TestConsoleToken(t *testing.T) {
	a, err := NewConsoleToken()
	require.NoError(t, err)
	b, err := NewConsoleToken()
	require.NoError(t, err)
	require.NotEqual(t, a, b)
	require.GreaterOrEqual(t, len(a), 43, "32 bytes of entropy, base64url")
	require.Len(t, HashConsoleToken(a), 64)
	require.Equal(t, HashConsoleToken(a), HashConsoleToken(a))
	require.NotEqual(t, HashConsoleToken(a), HashConsoleToken(b))
	require.Empty(t, HashConsoleToken(""), "no cookie has no hash")
	require.True(t, validTokenHash(HashConsoleToken(a)))
	require.False(t, validTokenHash("not-a-hash"))

	// The challenge binds the ceremony, the project and the browser.
	nonce := make([]byte, 32)
	base := ConsoleUnlockChallenge("01U", "example-finance", HashConsoleToken(a), nonce)
	require.Len(t, base, 32)
	require.NotEqual(t, base, ConsoleUnlockChallenge("01U", "example-ops", HashConsoleToken(a), nonce))
	require.NotEqual(t, base, ConsoleUnlockChallenge("01U", "example-finance", HashConsoleToken(b), nonce))
	require.NotEqual(t, base, ConsoleUnlockChallenge("01V", "example-finance", HashConsoleToken(a), nonce))
}

func TestConsoleUnlock_PasskeyCeremony(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	browser, other := HashConsoleToken("browser-a"), HashConsoleToken("browser-b")

	_, err := BeginConsoleUnlock(ctx, db, "example-finance", "not-a-hash", 3*time.Hour, 5*time.Minute, unlockT0)
	require.ErrorIs(t, err, ErrInvalidConsoleUnlock)
	_, err = BeginConsoleUnlock(ctx, db, "", browser, 3*time.Hour, 5*time.Minute, unlockT0)
	require.ErrorIs(t, err, ErrInvalidConsoleUnlock)

	u, err := BeginConsoleUnlock(ctx, db, "example-finance", browser, 3*time.Hour, 5*time.Minute, unlockT0)
	require.NoError(t, err)
	require.Equal(t, ConsoleUnlockPending, u.Status)
	require.Len(t, u.Challenge, 32)
	require.Equal(t, 3*time.Hour, u.Lease)
	require.Nil(t, u.ExpiresAt)

	live, err := LiveConsoleUnlocks(ctx, db, browser, unlockT0)
	require.NoError(t, err)
	require.Empty(t, live, "a ceremony in flight opens nothing")

	for name, c := range map[string]struct {
		id, project, token string
		at                 time.Time
	}{
		"another browser": {u.ID, "example-finance", other, unlockT0},
		"another project": {u.ID, "example-ops", browser, unlockT0},
		"past deadline":   {u.ID, "example-finance", browser, unlockT0.Add(5 * time.Minute)},
		"unknown id":      {"01NOPE", "example-finance", browser, unlockT0},
	} {
		_, err := PendingConsoleUnlock(ctx, db, c.id, c.project, c.token, c.at)
		require.ErrorIs(t, err, ErrConsoleUnlockNotFound, name)
	}
	got, err := PendingConsoleUnlock(ctx, db, u.ID, "example-finance", browser, unlockT0.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, u.Challenge, got.Challenge)

	_, err = GrantConsoleUnlock(ctx, db, u.ID, other, "cred-1", unlockT0)
	require.ErrorIs(t, err, ErrConsoleUnlockNotFound, "another browser cannot take the grant")
	_, err = GrantConsoleUnlock(ctx, db, u.ID, browser, "", unlockT0)
	require.ErrorIs(t, err, ErrInvalidConsoleUnlock)

	// Two racing grants: exactly one wins.
	var wg sync.WaitGroup
	results := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := GrantConsoleUnlock(ctx, db, u.ID, browser, "cred-1", unlockT0.Add(time.Minute))
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	wins := 0
	for err := range results {
		if err == nil {
			wins++
		} else {
			require.ErrorIs(t, err, ErrConsoleUnlockNotFound)
		}
	}
	require.Equal(t, 1, wins)

	live, err = LiveConsoleUnlocks(ctx, db, browser, unlockT0.Add(time.Minute))
	require.NoError(t, err)
	require.Len(t, live, 1)
	require.Equal(t, "example-finance", live[0].Project)
	require.Equal(t, "cred-1", live[0].Credential)
	require.Nil(t, live[0].Challenge, "the challenge is spent")
	require.Equal(t, unlockT0.Add(time.Minute+3*time.Hour), *live[0].ExpiresAt)

	none, err := LiveConsoleUnlocks(ctx, db, other, unlockT0.Add(time.Minute))
	require.NoError(t, err)
	require.Empty(t, none, "another browser holds nothing")
	none, err = LiveConsoleUnlocks(ctx, db, "", unlockT0)
	require.NoError(t, err)
	require.Empty(t, none, "no cookie holds nothing")
	expired, err := LiveConsoleUnlocks(ctx, db, browser, unlockT0.Add(time.Minute+3*time.Hour))
	require.NoError(t, err)
	require.Empty(t, expired, "an unlock ends at its expiry")
}

func TestConsoleUnlock_PendingCapAndPurge(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	browser := HashConsoleToken("browser-a")
	for range maxPendingConsoleUnlocks {
		_, err := BeginConsoleUnlock(ctx, db, "example-finance", browser, time.Hour, 5*time.Minute, unlockT0)
		require.NoError(t, err)
	}
	_, err := BeginConsoleUnlock(ctx, db, "example-finance", browser, time.Hour, 5*time.Minute, unlockT0)
	require.ErrorIs(t, err, ErrConsoleUnlockBusy)
	// Past their deadline they are purged, and a new one fits.
	_, err = BeginConsoleUnlock(ctx, db, "example-finance", browser, time.Hour, 5*time.Minute, unlockT0.Add(6*time.Minute))
	require.NoError(t, err)
	var n int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM console_unlocks`).Scan(&n))
	require.Equal(t, 1, n)
}

func TestConsoleUnlock_LinkWorksOnce(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	browser, other := HashConsoleToken("browser-a"), HashConsoleToken("browser-b")

	_, _, err := CreateConsoleUnlockLink(ctx, db, "example-finance", 0, 10*time.Minute, unlockT0)
	require.ErrorIs(t, err, ErrInvalidConsoleUnlock)
	secret, u, err := CreateConsoleUnlockLink(ctx, db, "example-finance", 3*time.Hour, 10*time.Minute, unlockT0)
	require.NoError(t, err)
	require.Equal(t, ConsoleUnlockByDaemon, u.Credential)
	var stored string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT claim_hash FROM console_unlocks WHERE id = ?`, u.ID).Scan(&stored))
	require.Equal(t, HashConsoleToken(secret), stored)
	require.NotContains(t, stored, secret, "the secret itself is never stored")

	_, err = ClaimConsoleUnlockLink(ctx, db, "wrong", browser, unlockT0)
	require.ErrorIs(t, err, ErrConsoleUnlockNotFound)
	_, err = ClaimConsoleUnlockLink(ctx, db, secret, browser, unlockT0.Add(10*time.Minute))
	require.ErrorIs(t, err, ErrConsoleUnlockNotFound, "a link past its deadline is void")

	at := unlockT0.Add(2 * time.Minute)
	got, err := ClaimConsoleUnlockLink(ctx, db, secret, browser, at)
	require.NoError(t, err)
	require.Equal(t, ConsoleUnlockLive, got.Status)
	require.Equal(t, browser, got.TokenHash)
	require.Equal(t, at.Add(3*time.Hour), *got.ExpiresAt, "the lease runs from the claim")

	_, err = ClaimConsoleUnlockLink(ctx, db, secret, other, at)
	require.ErrorIs(t, err, ErrConsoleUnlockNotFound, "a link works once")
	live, err := LiveConsoleUnlocks(ctx, db, browser, at)
	require.NoError(t, err)
	require.Len(t, live, 1)
}

func TestConsoleUnlock_EndAndReset(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	browser := HashConsoleToken("browser-a")
	claim := func(project string) ConsoleUnlock {
		t.Helper()
		secret, _, err := CreateConsoleUnlockLink(ctx, db, project, time.Hour, time.Minute, unlockT0)
		require.NoError(t, err)
		u, err := ClaimConsoleUnlockLink(ctx, db, secret, browser, unlockT0)
		require.NoError(t, err)
		return u
	}
	fin, legal := claim("example-finance"), claim("example-legal")
	all, err := AllLiveConsoleUnlocks(ctx, db, unlockT0)
	require.NoError(t, err)
	require.Len(t, all, 2)

	_, err = EndConsoleUnlock(ctx, db, fin.ID, "bored", unlockT0)
	require.ErrorIs(t, err, ErrInvalidConsoleUnlock)
	ended, err := EndConsoleUnlock(ctx, db, fin.ID, ConsoleUnlockEndLocked, unlockT0.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, ConsoleUnlockEnded, ended.Status)
	require.Equal(t, ConsoleUnlockEndLocked, ended.EndedReason)
	_, err = EndConsoleUnlock(ctx, db, fin.ID, ConsoleUnlockEndLocked, unlockT0.Add(time.Minute))
	require.ErrorIs(t, err, ErrConsoleUnlockNotLive)
	_, err = EndConsoleUnlock(ctx, db, legal.ID, ConsoleUnlockEndLocked, unlockT0.Add(time.Hour))
	require.ErrorIs(t, err, ErrConsoleUnlockNotLive, "an expired unlock is not live")

	live, err := LiveConsoleUnlocks(ctx, db, browser, unlockT0.Add(time.Minute))
	require.NoError(t, err)
	require.Len(t, live, 1)
	require.Equal(t, "example-legal", live[0].Project)

	_, _, err = CreateConsoleUnlockLink(ctx, db, "example-ops", time.Hour, time.Minute, unlockT0)
	require.NoError(t, err)
	n, err := EndAllConsoleUnlocks(ctx, db, ConsoleUnlockEndReset, unlockT0.Add(time.Minute))
	require.NoError(t, err)
	require.EqualValues(t, 2, n, "the live unlock and the unclaimed link")
	live, err = LiveConsoleUnlocks(ctx, db, browser, unlockT0.Add(time.Minute))
	require.NoError(t, err)
	require.Empty(t, live)
}

func TestLiveAccessLeasesAndTallies(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	leases, err := LiveAccessLeases(ctx, db, now)
	require.NoError(t, err)
	require.Empty(t, leases)
	_, err = AccessLeaseByID(ctx, db, "01NOPE")
	require.ErrorIs(t, err, ErrLeaseNotLive)
	a, err := CreateAccessApproval(ctx, db, leaseReq("example-finance", access.ProcessToken{PID: 200, Version: 2000, Boot: "6c2f1a3e-0b7d-4c55-9a51-3d8f00a1b2c3"}), now)
	require.NoError(t, err)
	granted, err := ApproveAccessFor(ctx, db, a.ID, a.Challenge, "daemon-account", time.Hour, now)
	require.NoError(t, err)
	leases, err = LiveAccessLeases(ctx, db, now)
	require.NoError(t, err)
	require.Len(t, leases, 1)
	got, err := AccessLeaseByID(ctx, db, granted.ID)
	require.NoError(t, err)
	require.Equal(t, "example-finance", got.Project)
	require.NoError(t, RevokeAccessLease(ctx, db, granted.ID, LeaseRevokedOwner, now))
	leases, err = LiveAccessLeases(ctx, db, now)
	require.NoError(t, err)
	require.Empty(t, leases, "a revoked lease is not live")

	_, err = EnsureProject(ctx, db, "example-finance", "Example Finance")
	require.NoError(t, err)
	ts := core.FormatTime(now)
	for _, q := range []string{
		`INSERT INTO memories_index (id, kind, name, project, file_path, content_hash, created_at, updated_at) VALUES ('m1','gotcha','a','example-finance','memory/example-finance/a.md','h','` + ts + `','` + ts + `')`,
		`INSERT INTO memories_index (id, kind, name, project, file_path, content_hash, created_at, updated_at, invalid_at) VALUES ('m2','gotcha','b','example-finance','memory/example-finance/b.md','h','` + ts + `','` + ts + `','` + ts + `')`,
		`INSERT INTO notes_index (id, title, slug, project, file_path, content_hash, created_at, updated_at) VALUES ('n1','t','t','example-finance','notes/example-finance/t.md','h','` + ts + `','` + ts + `')`,
		`INSERT INTO sessions (id, name, project_slug, created_at, updated_at) VALUES ('s1','cc/fin','example-finance','` + ts + `','` + ts + `')`,
		`INSERT INTO tasks (id, project_slug, title, status, created_at, updated_at) VALUES ('t1','example-finance','x','open','` + ts + `','` + ts + `'), ('t2','example-finance','y','done','` + ts + `','` + ts + `')`,
	} {
		_, err := db.ExecContext(ctx, q)
		require.NoError(t, err)
	}
	tallies, err := ProjectTallies(ctx, db, []string{"example-finance", "example-legal"})
	require.NoError(t, err)
	require.Equal(t, []ProjectTally{
		{Slug: "example-finance", Name: "Example Finance", Registered: true, Memories: 1, Notes: 1, Sessions: 1, OpenTasks: 1},
		{Slug: "example-legal", Name: "example-legal"},
	}, tallies)
	n, err := CountRegisteredProjects(ctx, db, []string{"example-finance", "example-legal"})
	require.NoError(t, err)
	require.Equal(t, 1, n)
	n, err = CountRegisteredProjects(ctx, db, nil)
	require.NoError(t, err)
	require.Zero(t, n)
}
