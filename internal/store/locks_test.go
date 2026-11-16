package store

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// lkNow is the fixed clock the lock tests read.
var lkNow = time.Date(2026, 11, 15, 18, 0, 0, 0, time.UTC)

// lkAliases derives an identity's aliases, failing the test on an error.
func lkAliases(t *testing.T, id CWDIdentity) []core.RepoLockAlias {
	t.Helper()
	a, err := RepoLockAliasesFor(id, "example-daemon")
	require.NoError(t, err)
	return a
}

// lkTestDeclare declares an owner's lock named name for id.
func lkTestDeclare(t *testing.T, db *sql.DB, id CWDIdentity, name string, projects ...string) RepoLockDeclared {
	t.Helper()
	out, err := DeclareRepoLock(context.Background(), db, RepoLockDeclaration{
		Name: name, Aliases: lkAliases(t, id), Projects: projects, State: core.RepoLockActive, RecordAliases: true,
	}, lkNow)
	require.NoError(t, err)
	return out
}

func lkAliasStrings(as []core.RepoLockAlias) []string {
	out := make([]string, 0, len(as))
	for _, a := range as {
		out = append(out, string(a.Kind)+" "+a.Alias)
	}
	return out
}

func TestRepoLockAliasesFor(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   CWDIdentity
		want []string
	}{
		{"ssh origin", CWDIdentity{Host: "example-laptop", RepoRoot: "/work/app", Origin: "git@github.com:Example/App.git"},
			[]string{"origin github.com/example/app", "path example-laptop:/work/app"}},
		{"https origin is the same alias", CWDIdentity{Host: "example-laptop", RepoRoot: "/work/app", Origin: "https://github.com/example/app"},
			[]string{"origin github.com/example/app", "path example-laptop:/work/app"}},
		{"a worktree keys on its main checkout", CWDIdentity{Host: "example-laptop", RepoRoot: "/work/app-wt", MainRoot: "/work/app"},
			[]string{"path example-laptop:/work/app"}},
		{"no origin: host and path", CWDIdentity{Host: "Example-Laptop", RepoRoot: "/work/app/"},
			[]string{"path example-laptop:/work/app"}},
		{"no host: the daemon's own", CWDIdentity{RepoRoot: "/work/app"},
			[]string{"path example-daemon:/work/app"}},
		{"a side clone keys on its origin's path", CWDIdentity{Host: "h", RepoRoot: "/tmp/side", Origin: "/work/app"},
			[]string{"path h:/work/app", "path h:/tmp/side"}},
		{"a file URL origin", CWDIdentity{Host: "h", RepoRoot: "/tmp/side", Origin: "file:///work/app"},
			[]string{"path h:/work/app", "path h:/tmp/side"}},
		{"a relative origin joins the checkout", CWDIdentity{Host: "h", RepoRoot: "/work/side", Origin: "../app"},
			[]string{"path h:/work/app", "path h:/work/side"}},
		{"a Windows checkout", CWDIdentity{Host: "h", RepoRoot: `C:\repos\app`, Origin: `..\up`},
			[]string{`path h:C:\repos\up`, `path h:C:\repos\app`}},
		{"origin only", CWDIdentity{Host: "h", Origin: "ssh://git@example.com:2222/team/app.git"},
			[]string{"origin example.com:2222/team/app"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, lkAliasStrings(lkAliases(t, tc.id)))
		})
	}

	for _, tc := range []struct {
		name string
		id   CWDIdentity
	}{
		{"nothing at all", CWDIdentity{Host: "h"}},
		{"a cwd alone names no repository", CWDIdentity{Host: "h", CWD: "/work/app"}},
		{"a relative root", CWDIdentity{Host: "h", RepoRoot: "work/app"}},
		{"a control character", CWDIdentity{Host: "h", RepoRoot: "/work/a\x07pp"}},
		{"a format character", CWDIdentity{Host: "h", RepoRoot: "/work/a\u202epp"}},
		{"a host with a colon", CWDIdentity{Host: "h:1", RepoRoot: "/work/app"}},
	} {
		t.Run("refused: "+tc.name, func(t *testing.T) {
			_, err := RepoLockAliasesFor(tc.id, "example-daemon")
			require.ErrorIs(t, err, ErrRepoLockIdentity)
		})
	}
}

// A second declaration of the same origin and name, from any project and any
// host, resolves to the first lock: it changes none of its refs or projects
// and only adds the aliases it brings.
func TestDeclareRepoLock_SecondDeclarationResolvesToTheFirst(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	first := lkTestDeclare(t, db, CWDIdentity{Host: "example-laptop", RepoRoot: "/work/app", Origin: "git@github.com:example/app.git"},
		"main", "example-ops")
	require.True(t, first.Created)
	require.Equal(t, core.RepoLockActive, first.Lock.State)
	require.Equal(t, []string{"refs/heads/main"}, first.Lock.Refs, "the default refs are the branch of the lock's name")
	require.Equal(t, "app main", first.Lock.Label())

	out, err := DeclareRepoLock(ctx, db, RepoLockDeclaration{
		Name: "main", Refs: []string{"refs/heads/trunk"}, Projects: []string{"example-finance"}, State: core.RepoLockActive, RecordAliases: true,
		Aliases: lkAliases(t, CWDIdentity{Host: "example-desktop", RepoRoot: "/home/me/code/app", Origin: "https://github.com/Example/app"}),
	}, lkNow.Add(time.Minute))
	require.NoError(t, err)
	require.False(t, out.Created)
	require.Equal(t, first.Lock.ID, out.Lock.ID)
	require.Equal(t, []string{"refs/heads/main"}, out.Lock.Refs, "declared once: the first declaration's refs stand")
	require.Equal(t, []string{"example-ops"}, out.Lock.Projects)
	require.Equal(t, []string{"path example-desktop:/home/me/code/app"}, lkAliasStrings(out.Added))
	require.Equal(t, []string{"origin github.com/example/app", "path example-laptop:/work/app", "path example-desktop:/home/me/code/app"},
		lkAliasStrings(out.Lock.Aliases))

	// Another name on the same repository is another lock.
	release := lkTestDeclare(t, db, CWDIdentity{Host: "example-laptop", RepoRoot: "/work/app", Origin: "git@github.com:example/app.git"}, "release")
	require.True(t, release.Created)
	require.NotEqual(t, first.Lock.ID, release.Lock.ID)
	all, err := ListRepoLocks(ctx, db)
	require.NoError(t, err)
	require.Len(t, all, 2)
	mine, err := RepoLocksFor(ctx, db, lkAliases(t, CWDIdentity{Host: "x", Origin: "https://github.com/example/app"}))
	require.NoError(t, err)
	require.Len(t, mine, 2, "a repository's locks, every name")
}

// Two locks on one repository cannot exist: concurrent declarations of one
// identity and name make one lock, and the table refuses a second lock's alias.
func TestDeclareRepoLock_TwoLocksOnOneRepoCannotExist(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	id := CWDIdentity{Host: "example-laptop", RepoRoot: "/work/app", Origin: "git@github.com:example/app.git"}

	var wg sync.WaitGroup
	ids := make([]string, 8)
	errs := make([]error, 8)
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := DeclareRepoLock(ctx, db, RepoLockDeclaration{Name: "main", Aliases: lkAliases(t, id),
				State: core.RepoLockActive, RecordAliases: true}, lkNow)
			ids[i], errs[i] = out.Lock.ID, err
		}()
	}
	wg.Wait()
	for i := range ids {
		require.NoError(t, errs[i])
		require.Equal(t, ids[0], ids[i], "every declaration resolves to one lock")
	}
	n, _, err := CountRepoLocks(ctx, db)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	// The schema itself holds it: one (alias, name) belongs to one lock.
	_, err = db.ExecContext(ctx, `INSERT INTO repo_lock_aliases (lock_id, name, kind, alias, created_at) VALUES ('other','main','origin','github.com/example/app','x')`)
	require.Error(t, err, "UNIQUE (alias, name)")
}

// A repository with no origin locks by host and path, and keeps its lock when
// it gains an origin: the path still resolves, and declaring again adds the
// origin, which then resolves the lock from every checkout.
func TestDeclareRepoLock_NoOriginKeepsItsLockWhenAnOriginIsAdded(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	bare := lkTestDeclare(t, db, CWDIdentity{Host: "example-laptop", RepoRoot: "/work/app"}, "main")
	require.Equal(t, []string{"path example-laptop:/work/app"}, lkAliasStrings(bare.Lock.Aliases))

	grown := CWDIdentity{Host: "example-laptop", RepoRoot: "/work/app", Origin: "git@github.com:example/app.git"}
	got, ok, err := ResolveRepoLock(ctx, db, lkAliases(t, grown), "main")
	require.NoError(t, err)
	require.True(t, ok, "the path still resolves once an origin exists")
	require.Equal(t, bare.Lock.ID, got.ID)

	again := lkTestDeclare(t, db, grown, "main")
	require.False(t, again.Created)
	require.Equal(t, bare.Lock.ID, again.Lock.ID)
	require.Equal(t, []string{"origin github.com/example/app"}, lkAliasStrings(again.Added))

	elsewhere := CWDIdentity{Host: "example-desktop", RepoRoot: "/srv/app", Origin: "https://github.com/example/app.git"}
	got, ok, err = ResolveRepoLock(ctx, db, lkAliases(t, elsewhere), "main")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, bare.Lock.ID, got.ID, "another machine's checkout resolves through the origin")

	// The origin changes (a renamed remote): the checkout keeps its lock by path.
	moved := CWDIdentity{Host: "example-laptop", RepoRoot: "/work/app", Origin: "git@github.com:example/app-renamed.git"}
	got, ok, err = ResolveRepoLock(ctx, db, lkAliases(t, moved), "main")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, bare.Lock.ID, got.ID)
}

// An origin alias wins over a path alias (owner decision D5): a checkout whose
// path names one lock and whose origin another resolves to the origin's.
func TestResolveRepoLock_OriginWinsOverPath(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	byPath := lkTestDeclare(t, db, CWDIdentity{Host: "h", RepoRoot: "/work/app"}, "main")
	byOrigin := lkTestDeclare(t, db, CWDIdentity{Host: "other", RepoRoot: "/srv/app", Origin: "https://github.com/example/app"}, "main")
	require.NotEqual(t, byPath.Lock.ID, byOrigin.Lock.ID)

	both := CWDIdentity{Host: "h", RepoRoot: "/work/app", Origin: "https://github.com/example/app"}
	got, ok, err := ResolveRepoLock(ctx, db, lkAliases(t, both), "main")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, byOrigin.Lock.ID, got.ID)

	again := lkTestDeclare(t, db, both, "main")
	require.Equal(t, byOrigin.Lock.ID, again.Lock.ID)
	require.Empty(t, again.Added, "the path alias stays with the lock that holds it")
}

// A restricted account's declaration waits for the owner: it is stored as
// requested, nobody holds it, the owner principal's declaration of it is
// refused (approving is the console's decision), and the owner approves or
// discards it.
func TestDeclareRepoLock_RestrictedDeclarationWaitsForTheOwner(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	id := CWDIdentity{Host: "example-laptop", RepoRoot: "/work/app", Origin: "https://github.com/example/app"}

	asked, err := DeclareRepoLock(ctx, db, RepoLockDeclaration{Name: "main", Aliases: lkAliases(t, id), Projects: []string{"example-ops"},
		State: core.RepoLockRequested, CreatedBy: "sess-r", Account: "_example_agent"}, lkNow)
	require.NoError(t, err)
	require.True(t, asked.Created)
	require.Equal(t, core.RepoLockRequested, asked.Lock.State)
	require.Equal(t, "_example_agent", asked.Lock.Account)
	require.Equal(t, "sess-r", asked.Lock.CreatedBy)
	_, requested, err := CountRepoLocks(ctx, db)
	require.NoError(t, err)
	require.Equal(t, 1, requested)

	_, err = AcquireRepoLock(ctx, db, asked.Lock.ID, "sess-a", time.Minute, lkNow)
	require.ErrorIs(t, err, ErrRepoLockRequested, "a waiting declaration is held by nobody")

	_, err = DeclareRepoLock(ctx, db, RepoLockDeclaration{Name: "main", Aliases: lkAliases(t, id), State: core.RepoLockActive, RecordAliases: true}, lkNow)
	require.ErrorIs(t, err, ErrRepoLockRequested)

	other := CWDIdentity{Host: "example-desktop", RepoRoot: "/srv/app", Origin: "https://github.com/example/app"}
	again, err := DeclareRepoLock(ctx, db, RepoLockDeclaration{Name: "main", Aliases: lkAliases(t, other), State: core.RepoLockRequested}, lkNow)
	require.NoError(t, err)
	require.False(t, again.Created)
	require.Equal(t, asked.Lock.ID, again.Lock.ID)
	require.Empty(t, again.Added, "a restricted account's declaration records no alias")

	_, err = DiscardRepoLock(ctx, db, "no-such-lock")
	require.ErrorIs(t, err, ErrRepoLockNotFound)
	approved, err := ApproveRepoLock(ctx, db, asked.Lock.ID, lkNow)
	require.NoError(t, err)
	require.Equal(t, core.RepoLockActive, approved.State)
	_, err = ApproveRepoLock(ctx, db, asked.Lock.ID, lkNow)
	require.ErrorIs(t, err, ErrRepoLockNotRequested)
	_, err = DiscardRepoLock(ctx, db, asked.Lock.ID)
	require.ErrorIs(t, err, ErrRepoLockNotRequested, "an active lock is not discarded")

	// A second request, discarded: its rows go, and the repository may be
	// declared again.
	rel, err := DeclareRepoLock(ctx, db, RepoLockDeclaration{Name: "release", Aliases: lkAliases(t, id), Projects: []string{"example-ops"},
		State: core.RepoLockRequested}, lkNow)
	require.NoError(t, err)
	gone, err := DiscardRepoLock(ctx, db, rel.Lock.ID)
	require.NoError(t, err)
	require.Equal(t, rel.Lock.ID, gone.ID)
	for _, q := range []string{`SELECT COUNT(*) FROM repo_locks WHERE id = ?`, `SELECT COUNT(*) FROM repo_lock_aliases WHERE lock_id = ?`,
		`SELECT COUNT(*) FROM repo_lock_projects WHERE lock_id = ?`} {
		var n int
		require.NoError(t, db.QueryRowContext(ctx, q, rel.Lock.ID).Scan(&n))
		require.Zero(t, n, q)
	}
	fresh := lkTestDeclare(t, db, id, "release")
	require.True(t, fresh.Created)
}

func TestDeclareRepoLock_RefusesWhatItCannotStore(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	aliases := lkAliases(t, CWDIdentity{Host: "h", RepoRoot: "/work/app"})
	for _, tc := range []struct {
		name string
		d    RepoLockDeclaration
	}{
		{"a name that is not a token", RepoLockDeclaration{Name: "Main", Aliases: aliases, State: core.RepoLockActive}},
		{"a ref outside refs/", RepoLockDeclaration{Name: "main", Refs: []string{"main"}, Aliases: aliases, State: core.RepoLockActive}},
		{"no alias", RepoLockDeclaration{Name: "main", State: core.RepoLockActive}},
		{"no state", RepoLockDeclaration{Name: "main", Aliases: aliases}},
		{"a project slug with a slash", RepoLockDeclaration{Name: "main", Aliases: aliases, State: core.RepoLockActive, Projects: []string{"a/b"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DeclareRepoLock(ctx, db, tc.d, lkNow)
			require.Error(t, err)
		})
	}
	refs := make([]string, core.MaxRepoLockRefs+1)
	for i := range refs {
		refs[i] = "refs/heads/b" + string(rune('a'+i))
	}
	_, err := DeclareRepoLock(ctx, db, RepoLockDeclaration{Name: "main", Refs: refs, Aliases: aliases, State: core.RepoLockActive}, lkNow)
	require.ErrorIs(t, err, core.ErrRepoLockRef)
	n, _, err := CountRepoLocks(ctx, db)
	require.NoError(t, err)
	require.Zero(t, n)
}

// lkFreeColumns reads a lock's hold columns straight from the row.
func lkFreeColumns(t *testing.T, db *sql.DB, id string) (holder string, since, until sql.NullString) {
	t.Helper()
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT holder, held_since, lease_expires_at FROM repo_locks WHERE id = ?`, id).Scan(&holder, &since, &until))
	return holder, since, until
}

// Acquire, heartbeat, release, force-release and the session's release behave
// as task claims do, and a free lock's hold columns read ” and NULL.
func TestRepoLock_HoldLikeATaskClaim(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	l := lkTestDeclare(t, db, CWDIdentity{Host: "h", RepoRoot: "/work/app"}, "main").Lock
	holder, since, until := lkFreeColumns(t, db, l.ID)
	require.Equal(t, "", holder)
	require.False(t, since.Valid)
	require.False(t, until.Valid)

	got, err := AcquireRepoLock(ctx, db, l.ID, "sessA", time.Minute, lkNow)
	require.NoError(t, err)
	require.False(t, got.Heartbeat)
	require.False(t, got.Reclaimed, "a free lock is taken from nobody")
	require.Equal(t, "sessA", got.Lock.Holder)
	require.Equal(t, lkNow, *got.Lock.HeldSince)
	require.Equal(t, lkNow.Add(time.Minute), *got.Lock.LeaseExpiresAt)
	require.True(t, got.Lock.HoldLive(lkNow))

	// The holder's heartbeat refreshes the lease and keeps when the hold began.
	beat, err := AcquireRepoLock(ctx, db, l.ID, "sessA", 2*time.Minute, lkNow.Add(30*time.Second))
	require.NoError(t, err)
	require.True(t, beat.Heartbeat)
	require.Equal(t, lkNow, *beat.Lock.HeldSince)
	require.Equal(t, lkNow.Add(150*time.Second), *beat.Lock.LeaseExpiresAt)

	// Another session is refused while the hold is live, and told by whom.
	refused, err := AcquireRepoLock(ctx, db, l.ID, "sessB", time.Minute, lkNow.Add(time.Minute))
	require.ErrorIs(t, err, ErrRepoLockHeld)
	require.Equal(t, "sessA", refused.Lock.Holder)

	// Only the holder releases.
	_, err = ReleaseRepoLock(ctx, db, l.ID, "sessB", lkNow)
	require.ErrorIs(t, err, ErrRepoLockNotHolder)
	freed, err := ReleaseRepoLock(ctx, db, l.ID, "sessA", lkNow.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, "", freed.Holder)
	require.Nil(t, freed.HeldSince)
	require.Nil(t, freed.LeaseExpiresAt)
	holder, since, until = lkFreeColumns(t, db, l.ID)
	require.Equal(t, "", holder)
	require.False(t, since.Valid)
	require.False(t, until.Valid)

	// The owner's force-release ends anyone's hold and names it.
	_, err = AcquireRepoLock(ctx, db, l.ID, "sessB", time.Minute, lkNow)
	require.NoError(t, err)
	forced, err := ForceReleaseRepoLock(ctx, db, l.ID, lkNow)
	require.NoError(t, err)
	require.Equal(t, "sessB", forced.PriorHolder)
	require.Equal(t, "", forced.Lock.Holder)
	_, err = ForceReleaseRepoLock(ctx, db, l.ID, lkNow)
	require.ErrorIs(t, err, ErrRepoLockFree)
	_, err = ForceReleaseRepoLock(ctx, db, "no-such-lock", lkNow)
	require.ErrorIs(t, err, ErrRepoLockNotFound)

	// A departing session's locks return: every one it holds, live or lapsed.
	other := lkTestDeclare(t, db, CWDIdentity{Host: "h", RepoRoot: "/work/other"}, "main").Lock
	_, err = AcquireRepoLock(ctx, db, l.ID, "sessC", time.Minute, lkNow)
	require.NoError(t, err)
	_, err = AcquireRepoLock(ctx, db, other.ID, "sessC", time.Second, lkNow.Add(-time.Hour))
	require.NoError(t, err)
	n, err := ReleaseRepoLocksForSession(ctx, db, "sessC", lkNow)
	require.NoError(t, err)
	require.Equal(t, 2, n)
	n, err = ReleaseRepoLocksForSession(ctx, db, "sessC", lkNow)
	require.NoError(t, err)
	require.Zero(t, n)
	n, err = ReleaseRepoLocksForSession(ctx, db, "", lkNow)
	require.NoError(t, err)
	require.Zero(t, n)
	for _, id := range []string{l.ID, other.ID} {
		holder, since, until = lkFreeColumns(t, db, id)
		require.Equal(t, "", holder)
		require.False(t, since.Valid)
		require.False(t, until.Valid)
	}

	_, err = AcquireRepoLock(ctx, db, "no-such-lock", "sessA", time.Minute, lkNow)
	require.ErrorIs(t, err, ErrRepoLockNotFound)
	_, err = AcquireRepoLock(ctx, db, l.ID, "", time.Minute, lkNow)
	require.Error(t, err)
}

// The boundary is Task.ClaimLive's, pinned against it in the same test so the
// two cannot drift (constraint claimtask-claimless-inprogress-and-lease-boundary):
// a lease is live only while lease_expires_at > now, so at exactly its expiry
// another session takes it, as a reclaim from the prior holder.
func TestRepoLock_ExactLeaseBoundaryIsNotLive(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	l := lkTestDeclare(t, db, CWDIdentity{Host: "h", RepoRoot: "/work/app"}, "main").Lock

	first, err := AcquireRepoLock(ctx, db, l.ID, "sessA", time.Minute, lkNow)
	require.NoError(t, err)
	expiry := *first.Lock.LeaseExpiresAt
	task := core.Task{ClaimedBy: "sessA", Status: core.TaskInProgress, LeaseExpiresAt: &expiry}

	before := expiry.Add(-time.Nanosecond)
	require.True(t, first.Lock.HoldLive(before))
	require.Equal(t, task.ClaimLive(before), first.Lock.HoldLive(before))
	_, err = AcquireRepoLock(ctx, db, l.ID, "sessB", time.Minute, before)
	require.ErrorIs(t, err, ErrRepoLockHeld, "a hold is live right up to its expiry instant")

	require.False(t, first.Lock.HoldLive(expiry), "HoldLive uses After: an equal lease is not live")
	require.Equal(t, task.ClaimLive(expiry), first.Lock.HoldLive(expiry), "the two rules agree at the boundary")
	took, err := AcquireRepoLock(ctx, db, l.ID, "sessB", time.Minute, expiry)
	require.NoError(t, err, "a hold expiring exactly at now is free to take, matching ClaimLive")
	require.True(t, took.Reclaimed)
	require.Equal(t, "sessA", took.PriorHolder)
	require.Equal(t, "sessB", took.Lock.Holder)
	require.Equal(t, expiry, *took.Lock.HeldSince, "a take begins a new hold")

	// A hold with no lease stamped holds nothing: a take from a dead holder.
	_, err = db.ExecContext(ctx, `UPDATE repo_locks SET lease_expires_at = NULL WHERE id = ?`, l.ID)
	require.NoError(t, err)
	stale, ok, err := RepoLockByID(ctx, db, l.ID)
	require.NoError(t, err)
	require.True(t, ok)
	require.False(t, stale.HoldLive(expiry))
	took, err = AcquireRepoLock(ctx, db, l.ID, "sessC", time.Minute, expiry)
	require.NoError(t, err)
	require.True(t, took.Reclaimed)
	require.Equal(t, "sessB", took.PriorHolder)
}

func TestAcquireRepoLock_LeaseCeiling(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	l := lkTestDeclare(t, db, CWDIdentity{Host: "h", RepoRoot: "/work/app"}, "main").Lock
	for _, lease := range []time.Duration{0, -time.Second, core.MaxClaimLease + time.Nanosecond} {
		_, err := AcquireRepoLock(ctx, db, l.ID, "sessA", lease, lkNow)
		require.ErrorIs(t, err, ErrRepoLockLease, "%s", lease)
	}
	got, err := AcquireRepoLock(ctx, db, l.ID, "sessA", core.MaxClaimLease, lkNow)
	require.NoError(t, err, "the ceiling itself is allowed")
	require.Equal(t, lkNow.Add(4*time.Hour), *got.Lock.LeaseExpiresAt)
}

// Every checkout seen is recorded, and an alias another lock of the name holds
// stays where it is.
func TestRecordRepoLockAliases(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	l := lkTestDeclare(t, db, CWDIdentity{Host: "h", RepoRoot: "/work/app", Origin: "https://github.com/example/app"}, "main").Lock
	other := lkTestDeclare(t, db, CWDIdentity{Host: "h", RepoRoot: "/work/other"}, "main").Lock

	added, err := RecordRepoLockAliases(ctx, db, l.ID,
		lkAliases(t, CWDIdentity{Host: "laptop", RepoRoot: "/srv/app", Origin: "https://github.com/example/app"}), lkNow)
	require.NoError(t, err)
	require.Equal(t, []string{"path laptop:/srv/app"}, lkAliasStrings(added))

	added, err = RecordRepoLockAliases(ctx, db, l.ID, lkAliases(t, CWDIdentity{Host: "h", RepoRoot: "/work/other"}), lkNow)
	require.NoError(t, err)
	require.Empty(t, added)
	got, ok, err := ResolveRepoLock(ctx, db, lkAliases(t, CWDIdentity{Host: "h", RepoRoot: "/work/other"}), "main")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, other.ID, got.ID)

	_, err = RecordRepoLockAliases(ctx, db, "no-such-lock", nil, lkNow)
	require.ErrorIs(t, err, ErrRepoLockNotFound)
}
