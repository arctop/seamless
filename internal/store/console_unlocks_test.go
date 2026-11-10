package store

import (
	"context"
	"database/sql"
	"strings"
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

// Each kind of pending unlock has its own cap: a holder of the console's key
// filling the terminal unlocks' (no passkey needed) leaves a passkey ceremony
// free to begin, and the reverse. Past their deadline both are purged.
func TestConsoleUnlock_PendingCapAndPurge(t *testing.T) {
	ctx := context.Background()
	browser := HashConsoleToken("browser-a")
	passkey := func(db *sql.DB, at time.Time) error {
		_, err := BeginConsoleUnlock(ctx, db, "example-finance", browser, time.Hour, 5*time.Minute, at)
		return err
	}
	terminal := func(db *sql.DB, at time.Time) error {
		_, _, err := BeginTerminalUnlock(ctx, db, "example-finance", browser, time.Hour, 5*time.Minute, at)
		return err
	}
	for name, c := range map[string]struct {
		fill, other func(*sql.DB, time.Time) error
	}{
		"terminal unlocks full":   {terminal, passkey},
		"passkey ceremonies full": {passkey, terminal},
	} {
		t.Run(name, func(t *testing.T) {
			db := leaseDB(t)
			for range maxPendingConsoleUnlocks {
				require.NoError(t, c.fill(db, unlockT0))
			}
			require.ErrorIs(t, c.fill(db, unlockT0), ErrConsoleUnlockBusy, "this kind's cap is full")
			for range maxPendingConsoleUnlocks {
				require.NoError(t, c.other(db, unlockT0), "the other kind has its own cap")
			}
			require.ErrorIs(t, c.other(db, unlockT0), ErrConsoleUnlockBusy, "and it is capped too")
			// Past their deadline every pending unlock is purged, and a new
			// one of either kind fits.
			later := unlockT0.Add(6 * time.Minute)
			require.NoError(t, c.fill(db, later))
			require.NoError(t, c.other(db, later))
			var n int
			require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM console_unlocks`).Scan(&n))
			require.Equal(t, 2, n)
		})
	}
}

func TestFormatUnlockCode(t *testing.T) {
	for in, want := range map[string]string{
		"ABCD-EFGH": "ABCD-EFGH", "abcdefgh": "ABCD-EFGH", " abcd efgh ": "ABCD-EFGH", "Abcd-eFgh": "ABCD-EFGH",
		"0O1I-L234": "0011-1234", // Crockford's look-alikes fold
		"ABCD-EFG":  "", "ABCD-EFGHJ": "", "ABCD-EFGU": "", "ABCD*EFG": "", "": "",
	} {
		require.Equal(t, want, FormatUnlockCode(in), in)
	}
	for range 50 {
		c, err := newUnlockCode()
		require.NoError(t, err)
		require.Regexp(t, `^[0-9A-HJKMNP-TV-Z]{4}-[0-9A-HJKMNP-TV-Z]{4}$`, c)
	}
}

// A terminal unlock: the browser begins it, bound to its cookie hash; the
// daemon's account approves its code; only that browser reads the project.
func TestConsoleUnlock_TerminalApproval(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	browser, other := HashConsoleToken("browser-a"), HashConsoleToken("browser-b")

	_, _, err := BeginTerminalUnlock(ctx, db, "example-finance", "not-a-hash", 3*time.Hour, 10*time.Minute, unlockT0)
	require.ErrorIs(t, err, ErrInvalidConsoleUnlock)
	_, _, err = BeginTerminalUnlock(ctx, db, "", browser, 3*time.Hour, 10*time.Minute, unlockT0)
	require.ErrorIs(t, err, ErrInvalidConsoleUnlock)

	u, code, err := BeginTerminalUnlock(ctx, db, "example-finance", browser, 3*time.Hour, 10*time.Minute, unlockT0)
	require.NoError(t, err)
	require.Equal(t, code, FormatUnlockCode(code), "the code comes printed")
	require.Equal(t, ConsoleUnlockPending, u.Status)
	require.Equal(t, browser, u.TokenHash, "bound to the browser that began it")
	require.Nil(t, u.Challenge)
	require.Empty(t, u.Credential)
	require.Equal(t, unlockT0.Add(10*time.Minute), u.PendingUntil)
	var stored string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT claim_hash FROM console_unlocks WHERE id = ?`, u.ID).Scan(&stored))
	require.Equal(t, unlockCodeHash(code), stored)
	require.True(t, validTokenHash(stored))
	require.NotContains(t, stored, NormalizeEnrollCode(code))
	require.NotEqual(t, HashConsoleToken(code), stored, "a code hashes under its own prefix")

	live, err := LiveConsoleUnlocks(ctx, db, browser, unlockT0)
	require.NoError(t, err)
	require.Empty(t, live, "a terminal unlock waiting opens nothing")
	_, err = PendingConsoleUnlock(ctx, db, u.ID, "example-finance", browser, unlockT0)
	require.ErrorIs(t, err, ErrConsoleUnlockNotFound, "a terminal unlock is no passkey ceremony")
	_, err = GrantConsoleUnlock(ctx, db, u.ID, browser, "cred-1", unlockT0)
	require.ErrorIs(t, err, ErrConsoleUnlockNotFound, "nor can a passkey grant take it")

	// Another browser's unlock of the same project, waiting beside it.
	theirs, theirCode, err := BeginTerminalUnlock(ctx, db, "example-finance", other, 3*time.Hour, 10*time.Minute, unlockT0)
	require.NoError(t, err)
	require.NotEqual(t, code, theirCode)
	// And one of another project, by the same browser.
	legal, legalCode, err := BeginTerminalUnlock(ctx, db, "example-legal", browser, 3*time.Hour, 10*time.Minute, unlockT0)
	require.NoError(t, err)

	at := unlockT0.Add(2 * time.Minute)
	for name, c := range map[string]struct {
		project, code string
		at            time.Time
		err           error
	}{
		"malformed":              {"example-finance", "ABC", at, ErrInvalidConsoleUnlock},
		"no such code":           {"example-finance", "ZZZZ-ZZZZ", at, ErrConsoleUnlockNotFound},
		"another project":        {"example-ops", code, at, ErrConsoleUnlockNotFound},
		"another project's code": {"example-finance", legalCode, at, ErrConsoleUnlockNotFound},
		"past deadline":          {"example-finance", code, unlockT0.Add(10 * time.Minute), ErrConsoleUnlockNotFound},
	} {
		_, err := ApproveTerminalUnlock(ctx, db, c.project, c.code, time.Hour, c.at)
		require.ErrorIs(t, err, c.err, name)
	}
	_, err = ApproveTerminalUnlock(ctx, db, "example-finance", code, 0, at)
	require.ErrorIs(t, err, ErrInvalidConsoleUnlock, "a lease lasts at least a second")

	// Typed loosely, it still names the row; the lease is the approval's.
	got, err := ApproveTerminalUnlock(ctx, db, "example-finance", strings.ToLower(strings.ReplaceAll(code, "-", "")), 90*time.Minute, at)
	require.NoError(t, err)
	require.Equal(t, u.ID, got.ID)
	require.Equal(t, ConsoleUnlockLive, got.Status)
	require.Equal(t, browser, got.TokenHash, "approval never names a browser: it opens the one that began it")
	require.Equal(t, ConsoleUnlockByDaemon, got.Credential)
	require.Equal(t, 90*time.Minute, got.Lease)
	require.Equal(t, at.Add(90*time.Minute), *got.ExpiresAt, "the lease runs from the approval")
	require.NoError(t, db.QueryRowContext(ctx, `SELECT claim_hash FROM console_unlocks WHERE id = ?`, u.ID).Scan(&stored))
	require.Empty(t, stored, "the code is spent")

	_, err = ApproveTerminalUnlock(ctx, db, "example-finance", code, time.Hour, at)
	require.ErrorIs(t, err, ErrConsoleUnlockNotFound, "a code works once")

	live, err = LiveConsoleUnlocks(ctx, db, browser, at)
	require.NoError(t, err)
	require.Len(t, live, 1)
	require.Equal(t, "example-finance", live[0].Project)
	none, err := LiveConsoleUnlocks(ctx, db, other, at)
	require.NoError(t, err)
	require.Empty(t, none, "another browser's unlock still waits for its own code")
	for _, id := range []string{theirs.ID, legal.ID} {
		w, err := consoleUnlockByID(ctx, db, id)
		require.NoError(t, err)
		require.Equal(t, ConsoleUnlockPending, w.Status, "approving one code touches no other row")
	}
}

// Two approvals of one code racing: exactly one wins.
func TestConsoleUnlock_TerminalApprovalRace(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	browser := HashConsoleToken("browser-a")
	_, code, err := BeginTerminalUnlock(ctx, db, "example-finance", browser, 3*time.Hour, 10*time.Minute, unlockT0)
	require.NoError(t, err)
	var wg sync.WaitGroup
	results := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := ApproveTerminalUnlock(ctx, db, "example-finance", code, time.Hour, unlockT0.Add(time.Minute))
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
	live, err := LiveConsoleUnlocks(ctx, db, browser, unlockT0.Add(time.Minute))
	require.NoError(t, err)
	require.Len(t, live, 1)
}

// A code another pending unlock shows is drawn again.
func TestConsoleUnlock_TerminalCodeRerolls(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	draws := []string{"AAAA-AAAA", "AAAA-AAAA", "BBBB-BBBB"}
	orig := newUnlockCode
	t.Cleanup(func() { newUnlockCode = orig })
	newUnlockCode = func() (string, error) {
		c := draws[0]
		if len(draws) > 1 {
			draws = draws[1:]
		}
		return c, nil
	}
	_, first, err := BeginTerminalUnlock(ctx, db, "example-finance", HashConsoleToken("a"), time.Hour, time.Minute, unlockT0)
	require.NoError(t, err)
	require.Equal(t, "AAAA-AAAA", first)
	_, second, err := BeginTerminalUnlock(ctx, db, "example-legal", HashConsoleToken("b"), time.Hour, time.Minute, unlockT0)
	require.NoError(t, err)
	require.Equal(t, "BBBB-BBBB", second, "unique among pending unlocks, whatever their project")
	// Every draw taken: the begin fails rather than share a code.
	_, _, err = BeginTerminalUnlock(ctx, db, "example-ops", HashConsoleToken("c"), time.Hour, time.Minute, unlockT0)
	require.ErrorContains(t, err, "no free pairing code")
}

// The page waiting on a terminal approval reads its own row, and only its own.
func TestConsoleUnlockFor(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	browser, other := HashConsoleToken("browser-a"), HashConsoleToken("browser-b")
	u, code, err := BeginTerminalUnlock(ctx, db, "example-finance", browser, time.Hour, 10*time.Minute, unlockT0)
	require.NoError(t, err)
	for name, c := range map[string]struct{ id, project, token string }{
		"another browser": {u.ID, "example-finance", other},
		"no cookie":       {u.ID, "example-finance", ""},
		"another project": {u.ID, "example-legal", browser},
		"unknown id":      {"01NOPE", "example-finance", browser},
	} {
		_, err := ConsoleUnlockFor(ctx, db, c.id, c.project, c.token)
		require.ErrorIs(t, err, ErrConsoleUnlockNotFound, name)
	}
	got, err := ConsoleUnlockFor(ctx, db, u.ID, "example-finance", browser)
	require.NoError(t, err)
	require.Equal(t, ConsoleUnlockPending, got.Status)
	_, err = ApproveTerminalUnlock(ctx, db, "example-finance", code, time.Hour, unlockT0)
	require.NoError(t, err)
	got, err = ConsoleUnlockFor(ctx, db, u.ID, "example-finance", browser)
	require.NoError(t, err)
	require.Equal(t, ConsoleUnlockLive, got.Status)
}

// terminalUnlock begins and approves a terminal unlock of project for browser.
func terminalUnlock(t *testing.T, db *sql.DB, project, browser string, lease time.Duration, now time.Time) ConsoleUnlock {
	t.Helper()
	ctx := context.Background()
	_, code, err := BeginTerminalUnlock(ctx, db, project, browser, lease, time.Minute, now)
	require.NoError(t, err)
	u, err := ApproveTerminalUnlock(ctx, db, project, code, lease, now)
	require.NoError(t, err)
	return u
}

func TestConsoleUnlock_EndAndReset(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	browser := HashConsoleToken("browser-a")
	claim := func(project string) ConsoleUnlock {
		t.Helper()
		return terminalUnlock(t, db, project, browser, time.Hour, unlockT0)
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

	_, code, err := BeginTerminalUnlock(ctx, db, "example-ops", HashConsoleToken("browser-b"), time.Hour, 10*time.Minute, unlockT0)
	require.NoError(t, err)
	n, err := EndAllConsoleUnlocks(ctx, db, ConsoleUnlockEndReset, unlockT0.Add(time.Minute))
	require.NoError(t, err)
	require.EqualValues(t, 2, n, "the live unlock and the terminal unlock still waiting")
	live, err = LiveConsoleUnlocks(ctx, db, browser, unlockT0.Add(time.Minute))
	require.NoError(t, err)
	require.Empty(t, live)
	_, err = ApproveTerminalUnlock(ctx, db, "example-ops", code, time.Hour, unlockT0.Add(time.Minute))
	require.ErrorIs(t, err, ErrConsoleUnlockNotFound, "a reset unlock's code no longer approves it")
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
