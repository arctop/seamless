package retrieve

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// The repository the repo-lock fixture works in: its main checkout on the
// reader's laptop, an out-of-tree worktree of it on another machine, and a
// directory outside any repository.
var (
	rlMain = store.CWDIdentity{Host: "example-laptop", CWD: "/work/app", RepoRoot: "/work/app",
		Origin: "https://github.com/example/app.git"}
	rlWorktree = store.CWDIdentity{Host: "example-desk", CWD: "/srv/app-feature", RepoRoot: "/srv/app-feature",
		MainRoot: "/srv/app", Origin: "git@github.com:example/app.git"}
	rlOutside = store.CWDIdentity{Host: "example-laptop", CWD: "/tmp/scratch"}
)

// rlEnv is a store with the repo mapped to project app, one constraint in it,
// and repo locks on (the console's override row) unless off.
func rlEnv(t *testing.T, on bool) (*Service, *sql.DB) {
	t.Helper()
	db := setupDB(t)
	ctx := context.Background()
	require.NoError(t, store.SetSetting(ctx, db, store.SettingRepoProjectMap, `{"/work/app":"app"}`))
	_, err := store.EnsureProject(ctx, db, "app", "app")
	require.NoError(t, err)
	insMem(t, db, "01RLC", "constraint", "app-rule", "keep the build green", "app")
	if on {
		require.NoError(t, store.SetFeaturesConfig(ctx, db, config.Features{RepoLocks: true}))
	}
	return New(db, nil, budgets(), nil), db
}

// rlAliases derives an identity's lock aliases; outside a repository, none.
func rlAliases(id store.CWDIdentity) []core.RepoLockAlias {
	a, err := store.RepoLockAliasesFor(id, "example-daemon")
	if err != nil {
		return nil
	}
	return a
}

// rlDeclare declares an owner's lock on the repository, listing projects.
func rlDeclare(t *testing.T, db *sql.DB, name string, state core.RepoLockState, projects ...string) core.RepoLock {
	t.Helper()
	out, err := store.DeclareRepoLock(context.Background(), db, store.RepoLockDeclaration{
		Name: name, Aliases: rlAliases(rlMain), Projects: projects, State: state, RecordAliases: true,
	}, time.Now().UTC())
	require.NoError(t, err)
	return out.Lock
}

// rlSession starts an active ambient session in project, last active at
// updated, that recorded the checkout of id (as the SessionStart hook does),
// and returns its id. account is the restricted account that started it, ""
// for the owner.
func rlSession(t *testing.T, db *sql.DB, name, project, account string, id store.CWDIdentity, updated time.Time) string {
	t.Helper()
	ctx := context.Background()
	sid, err := core.NewID()
	require.NoError(t, err)
	require.NoError(t, store.CreateSession(ctx, db, core.Session{
		ID: sid, Name: name, ProjectSlug: project, Status: core.SessionActive, Ambient: true,
		Account: account, CreatedAt: updated, UpdatedAt: updated,
	}))
	require.NoError(t, store.RecordSessionRepoLocks(ctx, db, sid, store.RepoLockAliasStrings(rlAliases(id)), nil))
	return sid
}

// rlAcquire has session take lock l at since, for lease.
func rlAcquire(t *testing.T, db *sql.DB, l core.RepoLock, session string, since time.Time, lease time.Duration) {
	t.Helper()
	_, err := store.AcquireRepoLock(context.Background(), db, l.ID, session, lease, since)
	require.NoError(t, err)
}

// rlInput asks for the lock lines of the checkout id, the reader being self.
func rlInput(id store.CWDIdentity, self ...string) *RepoLockInput {
	return &RepoLockInput{
		Aliases: func() []core.RepoLockAlias { return rlAliases(id) },
		Self:    func() []string { return self },
	}
}

// rlBrief assembles a main session's briefing in the checkout id, placed by
// the daemon's own repo map (the fixture maps /work/app there): id's host
// matters to its lock aliases only.
func rlBrief(t *testing.T, ctx context.Context, svc *Service, id store.CWDIdentity, in *RepoLockInput) string {
	t.Helper()
	b, _, err := svc.Briefing(ctx, BriefingInput{CWD: id.CWD, Source: "startup", RepoLocks: in})
	require.NoError(t, err)
	return b
}

// rlLines returns the briefing's LOCK and presence lines.
func rlLines(b string) []string {
	var out []string
	for line := range strings.SplitSeq(b, "\n") {
		if strings.HasPrefix(line, "LOCK ") || strings.HasPrefix(line, "Also live in this repository:") {
			out = append(out, line)
		}
	}
	return out
}

// Acceptance: a briefing inside a declared repo, and inside one of its
// out-of-tree worktrees on another machine, shows the lock and the live peers;
// outside the repo it shows neither.
func TestBriefingRepoLocks_DeclaredRepoAndItsWorktreeShowTheLockAndPeers(t *testing.T) {
	svc, db := rlEnv(t, true)
	ctx := context.Background()
	now := time.Now()
	l := rlDeclare(t, db, "main", core.RepoLockActive, "app")
	peer := rlSession(t, db, "cc/peer-desk", "app", "", rlWorktree, now.Add(-3*time.Minute-10*time.Second))
	since := now.Add(-12*time.Minute - 30*time.Second)
	rlAcquire(t, db, l, peer, since, 30*time.Minute)
	until := access.Clock(since.Add(30 * time.Minute))

	in := rlInput(rlMain)
	b := rlBrief(t, ctx, svc, rlMain, in)
	require.Equal(t, []string{
		"LOCK app main: held by cc/peer-desk for 12 min, until " + until,
		"Also live in this repository: cc/peer-desk (idle 3 min)",
	}, rlLines(b))
	// The lines sit under the header, ahead of the constraints.
	require.Less(t, strings.Index(b, "LOCK app main"), strings.Index(b, "Constraints (binding"))
	require.True(t, in.Result.On)
	require.Equal(t, store.RepoLockAliasStrings(rlAliases(rlMain)), in.Result.Aliases)
	require.Equal(t, map[string]string{l.ID: peer}, in.Result.Seen)

	// The worktree on the other machine resolves to the same lock; its reader
	// is the peer itself, so the peer is no peer of its own.
	in = rlInput(rlWorktree, peer)
	b = rlBrief(t, ctx, svc, rlWorktree, in)
	require.Equal(t, []string{"LOCK app main: held by you (cc/peer-desk) for 12 min, until " + until}, rlLines(b))

	// Outside the repository: no lock line, no presence line, and the session
	// records that it works in no repository a lock names.
	in = rlInput(rlOutside)
	b = rlBrief(t, ctx, svc, rlOutside, in)
	require.Empty(t, rlLines(b))
	require.NotContains(t, b, "LOCK")
	require.True(t, in.Result.On)
	require.Empty(t, in.Result.Aliases)
}

// With repo locks off nothing is asked, read, rendered or reported: the
// briefing is byte for byte the one a caller that asks for no locks gets.
func TestBriefingRepoLocks_OffChangesNothing(t *testing.T) {
	svc, db := rlEnv(t, false)
	ctx := context.Background()
	l := rlDeclare(t, db, "main", core.RepoLockActive, "app")
	peer := rlSession(t, db, "cc/peer", "app", "", rlMain, time.Now())
	rlAcquire(t, db, l, peer, time.Now(), time.Hour)

	in := &RepoLockInput{
		Aliases: func() []core.RepoLockAlias {
			t.Fatal("the aliases are resolved only while repo locks are on")
			return nil
		},
		Self:     func() []string { t.Fatal("the reader's sessions are read only while repo locks are on"); return nil },
		HooksOff: true,
	}
	with := rlBrief(t, ctx, svc, rlMain, in)
	without := rlBrief(t, ctx, svc, rlMain, nil)
	require.Equal(t, without, with)
	require.NotContains(t, with, "LOCK")
	require.False(t, in.Result.On, "nothing to record")
}

// Acceptance: a restricted reader sees counts, not names, for the sessions it
// may not see -- another principal's -- and no line at all for a lock outside
// its reach. The owner sees that lock, and counts the account's session.
func TestBriefingRepoLocks_RestrictedReaderCountsWhatItCannotSee(t *testing.T) {
	svc, db := rlEnv(t, true)
	setIsolation(t, db, "conf-x", core.IsolationConfidential)
	now := time.Now()
	main := rlDeclare(t, db, "main", core.RepoLockActive, "app")
	rlDeclare(t, db, "release", core.RepoLockActive, "conf-x")
	owners := rlSession(t, db, "cc/owner-peer", "app", "", rlWorktree, now.Add(-time.Minute))
	rlSession(t, db, "cc/acct-peer", "app", acctName, rlMain, now.Add(-2*time.Minute))
	rlAcquire(t, db, main, owners, now.Add(-5*time.Minute), time.Hour)

	b := rlBrief(t, restrictedCtx(t, db), svc, rlMain, rlInput(rlMain))
	require.Equal(t, []string{
		"LOCK app main: held by another session for 5 min, until " + access.Clock(now.Add(55*time.Minute)),
		"Also live in this repository: " + acctName + ":cc/acct-peer (idle 2 min), and 1 other session",
	}, rlLines(b))
	require.NotContains(t, b, "release", "a lock outside the account's reach has no line")
	require.NotContains(t, b, "owner-peer")

	b = rlBrief(t, ownerCtx(t), svc, rlMain, rlInput(rlMain))
	require.Equal(t, []string{
		"LOCK app main: held by cc/owner-peer for 5 min, until " + access.Clock(now.Add(55*time.Minute)),
		"LOCK app release: free",
		"Also live in this repository: cc/owner-peer (idle 1 min), and 1 other session",
	}, rlLines(b))
}

// A session bound to a project the reader's scope may not read is counted too,
// with no View at all: the owner's own session in a confidential project,
// read from an open one.
func TestBriefingRepoLocks_PeerInAnUnreadableProjectIsCounted(t *testing.T) {
	svc, db := rlEnv(t, true)
	setIsolation(t, db, "conf-x", core.IsolationConfidential)
	l := rlDeclare(t, db, "main", core.RepoLockActive, "app")
	hidden := rlSession(t, db, "cc/conf-peer", "conf-x", "", rlWorktree, time.Now())
	rlAcquire(t, db, l, hidden, time.Now(), time.Hour)

	b := rlBrief(t, context.Background(), svc, rlMain, rlInput(rlMain))
	lines := rlLines(b)
	require.Len(t, lines, 2)
	require.True(t, strings.HasPrefix(lines[0], "LOCK app main: held by another session for 0 min, until "), lines[0])
	require.Equal(t, "Also live in this repository: 1 other session", lines[1])
	require.NotContains(t, b, "conf-peer")
}

// A declaration still waiting for the owner has no line, and makes no
// presence line either.
func TestBriefingRepoLocks_RequestedDeclarationHasNoLine(t *testing.T) {
	svc, db := rlEnv(t, true)
	rlDeclare(t, db, "main", core.RepoLockRequested, "app")
	rlSession(t, db, "cc/peer", "app", "", rlWorktree, time.Now())
	b := rlBrief(t, context.Background(), svc, rlMain, rlInput(rlMain))
	require.Empty(t, rlLines(b))
}

// The budget: the lock lines are pinned and the presence line is the first row
// to pack, so a short budget drops the presence line and never a lock line.
func TestBriefingRepoLocks_ShortBudgetDropsPresenceBeforeALockLine(t *testing.T) {
	_, db := rlEnv(t, true)
	rlDeclare(t, db, "main", core.RepoLockActive, "app")
	rlSession(t, db, "cc/peer", "app", "", rlWorktree, time.Now())
	tight := New(db, nil, config.Budgets{MaxBriefingTokens: 40, RecallBudgetTokens: 1000}, nil)
	b := rlBrief(t, context.Background(), tight, rlMain, rlInput(rlMain))
	require.Equal(t, []string{"LOCK app main: free"}, rlLines(b))

	roomy := New(db, nil, budgets(), nil)
	b = rlBrief(t, context.Background(), roomy, rlMain, rlInput(rlMain))
	require.Equal(t, []string{"LOCK app main: free", "Also live in this repository: cc/peer (idle 0 min)"}, rlLines(b))
}

// A declared lock is news in itself: in a project with nothing else to say the
// briefing still renders, carrying the lock; without the lock it is empty, as
// always.
func TestBriefingRepoLocks_ALockAloneMakesABriefing(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()
	require.NoError(t, store.SetSetting(ctx, db, store.SettingRepoProjectMap, `{"/work/app":"app"}`))
	require.NoError(t, store.SetFeaturesConfig(ctx, db, config.Features{RepoLocks: true}))
	svc := New(db, nil, budgets(), nil)
	require.Empty(t, rlBrief(t, ctx, svc, rlMain, rlInput(rlMain)))

	rlDeclare(t, db, "main", core.RepoLockActive, "app")
	b := rlBrief(t, ctx, svc, rlMain, rlInput(rlMain))
	require.Contains(t, b, "<seam-briefing>")
	require.Equal(t, []string{"LOCK app main: free"}, rlLines(b))
}

// The hooks line follows the lock lines when the client reported no lock hook,
// and past four locks the rest are counted on one line.
func TestBriefingRepoLocks_HooksOffAndTheLineCap(t *testing.T) {
	svc, db := rlEnv(t, true)
	for _, name := range []string{"main", "release", "docs", "site", "beta"} {
		rlDeclare(t, db, name, core.RepoLockActive, "app")
	}
	in := rlInput(rlMain)
	in.HooksOff = true
	b := rlBrief(t, context.Background(), svc, rlMain, in)
	require.Equal(t, []string{
		"LOCK app main: free", "LOCK app release: free", "LOCK app docs: free", "LOCK app site: free",
		"LOCK +1 more lock on this repository: seam lock list --here",
		strings.TrimSuffix(repoLockHooksOffLine, "\n"),
	}, rlLines(b))
	require.Len(t, in.Result.Seen, 5, "every lock's holder is recorded, shown or counted")
}

func TestRepoLockLine(t *testing.T) {
	now := time.Date(2026, 11, 15, 18, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) *time.Time { t := now.Add(-d); return &t }
	in := func(d time.Duration) *time.Time { t := now.Add(d); return &t }
	aliases := []core.RepoLockAlias{{Kind: core.RepoLockAliasOrigin, Alias: "github.com/example/seamless"}}
	base := core.RepoLock{ID: "L", Name: "main", State: core.RepoLockActive, Aliases: aliases}
	held := base
	held.Holder, held.HeldSince, held.LeaseExpiresAt = "S", ago(12*time.Minute+59*time.Second), in(time.Minute)
	queued := held
	queued.Waiters = []string{"W1", "W2"}
	lapsed := base
	lapsed.Holder, lapsed.HeldSince, lapsed.LeaseExpiresAt = "S", ago(time.Hour), ago(time.Second)
	freeQueue := base
	freeQueue.Waiters = []string{"W1"}
	for _, tc := range []struct {
		name string
		l    core.RepoLock
		want string
	}{
		{"free", base, "LOCK seamless main: free"},
		{"held", held, "LOCK seamless main: held by cc/x for 12 min, until " + access.Clock(now.Add(time.Minute))},
		{"held, with a queue", queued, "LOCK seamless main: held by cc/x for 12 min, until " +
			access.Clock(now.Add(time.Minute)) + " (2 waiting)"},
		{"a lapsed hold is free", lapsed, "LOCK seamless main: free (the hold by cc/x lapsed at " +
			access.Clock(now.Add(-time.Second)) + ")"},
		{"free, with a queue", freeQueue, "LOCK seamless main: free (1 waiting)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, repoLockLine(tc.l, "cc/x", now))
		})
	}
}

func TestPresenceLine(t *testing.T) {
	require.Empty(t, presenceLine(nil, 0))
	require.Equal(t, "Also live in this repository: cc/a (idle 3 min)", presenceLine([]string{"cc/a (idle 3 min)"}, 0))
	require.Equal(t, "Also live in this repository: cc/a (idle 3 min), cc/b (idle 0 min), and 2 other sessions",
		presenceLine([]string{"cc/a (idle 3 min)", "cc/b (idle 0 min)"}, 2))
	require.Equal(t, "Also live in this repository: 1 other session", presenceLine(nil, 1))
}

// Acceptance: a holder change reaches the prompt hook once. "Free" is a holder
// too; a lock the session itself just took is no news, nor is its own hold
// ending with no holder at all (a release, or a force-release the claim
// notices carry); its own hold lapsing is.
func TestRepoLockChanges_OncePerHolderChange(t *testing.T) {
	svc, db := rlEnv(t, true)
	ctx := context.Background()
	now := time.Now()
	l := rlDeclare(t, db, "main", core.RepoLockActive, "app")
	reader := rlSession(t, db, "cc/reader", "app", "", rlMain, now)
	peer := rlSession(t, db, "cc/peer", "app", "", rlWorktree, now)
	aliases := store.RepoLockAliasStrings(rlAliases(rlMain))
	self := func() []string { return []string{reader} }
	changes := func(seen map[string]string) ([]string, map[string]string) {
		t.Helper()
		lines, next, err := svc.RepoLockChanges(ctx, "app", aliases, seen, self)
		require.NoError(t, err)
		return lines, next
	}

	seen := map[string]string{l.ID: ""} // the briefing showed it free
	lines, next := changes(seen)
	require.Empty(t, lines)
	require.Equal(t, seen, next)

	rlAcquire(t, db, l, peer, now, 30*time.Minute)
	lines, next = changes(seen)
	require.Len(t, lines, 1)
	require.True(t, strings.HasPrefix(lines[0], "LOCK app main: held by cc/peer for 0 min, until "), lines[0])
	require.Equal(t, map[string]string{l.ID: peer}, next)
	seen = next
	lines, _ = changes(seen)
	require.Empty(t, lines, "told once")

	_, err := store.ReleaseRepoLock(ctx, db, l.ID, peer, now)
	require.NoError(t, err)
	lines, next = changes(seen)
	require.Equal(t, []string{"LOCK app main: free"}, lines, "a waiting session hears that the lock is free")
	seen = next
	lines, _ = changes(seen)
	require.Empty(t, lines)

	// Its own take, and its own release: nothing it does not know.
	rlAcquire(t, db, l, reader, time.Now(), 30*time.Minute)
	lines, next = changes(seen)
	require.Empty(t, lines)
	require.Equal(t, map[string]string{l.ID: reader}, next)
	seen = next
	_, err = store.ReleaseRepoLock(ctx, db, l.ID, reader, time.Now())
	require.NoError(t, err)
	lines, next = changes(seen)
	require.Empty(t, lines)
	require.Equal(t, map[string]string{l.ID: ""}, next)

	// Its own hold lapsing is news: it may think it still holds the lock.
	rlAcquire(t, db, l, reader, time.Now().Add(-time.Hour), 30*time.Minute)
	lines, next = changes(map[string]string{l.ID: reader})
	require.Len(t, lines, 1)
	require.True(t, strings.HasPrefix(lines[0], "LOCK app main: free (the hold by you (cc/reader) lapsed at "), lines[0])
	require.Equal(t, map[string]string{l.ID: ""}, next)

	// A lock declared since the session was last told is news once.
	other := rlDeclare(t, db, "release", core.RepoLockActive, "app")
	lines, next = changes(next)
	require.Equal(t, []string{"LOCK app release: free"}, lines)
	require.Equal(t, map[string]string{l.ID: "", other.ID: ""}, next)
}

// Off -- or a session that recorded no aliases -- the prompt half says nothing
// and asks to record nothing.
func TestRepoLockChanges_OffSaysNothing(t *testing.T) {
	svc, db := rlEnv(t, false)
	rlDeclare(t, db, "main", core.RepoLockActive, "app")
	aliases := store.RepoLockAliasStrings(rlAliases(rlMain))
	self := func() []string { t.Fatal("nothing is read while repo locks are off"); return nil }
	lines, next, err := svc.RepoLockChanges(context.Background(), "app", aliases, map[string]string{}, self)
	require.NoError(t, err)
	require.Nil(t, lines)
	require.Nil(t, next)

	on, _ := rlEnv(t, true)
	lines, next, err = on.RepoLockChanges(context.Background(), "app", nil, nil, self)
	require.NoError(t, err)
	require.Nil(t, lines)
	require.Nil(t, next)
}
