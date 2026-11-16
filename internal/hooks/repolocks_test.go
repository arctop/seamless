package hooks

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// The repo-lock fixture: hostEnv's demo repository (/work/demo on the daemon's
// own machine) has an origin, and its out-of-tree worktree sits on another
// machine.
const rlOrigin = "https://github.com/acme/demo.git"

// rlMainID is what `seam hook session-start` sends from the main checkout.
func rlMainID(hooks string) map[string]string {
	id := map[string]string{"host": localHostName, "repo_root": "/work/demo", "main_root": "/work/demo", "origin": rlOrigin}
	if hooks != "" {
		id["lock_hooks"] = hooks
	}
	return id
}

// rlWorktreeID is what it sends from the worktree on the other machine.
var rlWorktreeID = map[string]string{
	"host": remoteHostName, "repo_root": "/srv/demo-feature", "main_root": "/srv/demo",
	"origin": "git@github.com:acme/demo.git", "lock_hooks": "on",
}

// rlOn switches repo locks on (the console's override row).
func rlOn(t *testing.T, e *hostEnv) {
	t.Helper()
	require.NoError(t, store.SetFeaturesConfig(context.Background(), e.db, config.Features{RepoLocks: true}))
}

// rlDeclare declares the demo repository's lock named name.
func rlDeclare(t *testing.T, e *hostEnv, name string) core.RepoLock {
	t.Helper()
	aliases, err := store.RepoLockAliasesFor(store.CWDIdentity{Host: localHostName, RepoRoot: "/work/demo", Origin: rlOrigin}, localHostName)
	require.NoError(t, err)
	out, err := store.DeclareRepoLock(context.Background(), e.db, store.RepoLockDeclaration{
		Name: name, Aliases: aliases, Projects: []string{"demo"}, State: core.RepoLockActive, RecordAliases: true,
	}, time.Now().UTC())
	require.NoError(t, err)
	return out.Lock
}

// rlStart posts a SessionStart for external session sid from cwd with the
// identity id, and returns the briefing.
func (e *hostEnv) rlStart(t *testing.T, sid, cwd string, id map[string]string) string {
	t.Helper()
	return additionalContext(t, e.postFrom(t, "session-start", id,
		map[string]any{"session_id": sid, "cwd": cwd, "source": "startup"}))
}

// rlPrompt posts a UserPromptSubmit for sid, as Claude Code's http hook does
// (no identity), and returns the injection.
func (e *hostEnv) rlPrompt(t *testing.T, sid string) string {
	t.Helper()
	return additionalContext(t, e.postFrom(t, "user-prompt-submit", nil,
		map[string]any{"session_id": sid, "cwd": "/work/demo", "user_prompt": "what is next on the list"}))
}

// rlLockLines returns the LOCK and presence lines of an injection.
func rlLockLines(s string) []string {
	var out []string
	for line := range strings.SplitSeq(s, "\n") {
		line = strings.TrimPrefix(line, "Seamless: ")
		if strings.HasPrefix(line, "LOCK ") || strings.HasPrefix(line, "Also live in this repository:") {
			out = append(out, line)
		}
	}
	return out
}

// The whole path: a session in the declared repository is shown the lock (and
// "hooks off" when its client said so), one in a worktree of it on another
// machine is shown the lock and the first as a live peer, one outside the
// repository is shown neither; and a holder change reaches a session's next
// prompt once, "free" included.
func TestRepoLocks_BriefingPresenceAndOneNoticePerHolderChange(t *testing.T) {
	e := newHostEnv(t, config.PlanCapture{})
	rlOn(t, e)
	ctx := context.Background()
	l := rlDeclare(t, e, "main")

	b := e.rlStart(t, "rl-main", "/work/demo", rlMainID(lockHooksOff))
	require.Equal(t, []string{
		"LOCK demo main: free",
		"LOCK hooks off: this checkout runs no lock hook, so git will not check the lock before it moves a guarded ref",
	}, rlLockLines(b))
	mainSess := e.session(t, "rl-main")
	aliases, seen, ok := store.SessionRepoLocks(mainSess)
	require.True(t, ok, "the session records its checkout's aliases")
	require.Equal(t, []string{"github.com/acme/demo", localHostName + ":/work/demo"}, aliases)
	require.Equal(t, map[string]string{l.ID: ""}, seen)

	b = e.rlStart(t, "rl-wt", "/srv/demo-feature", rlWorktreeID)
	require.Equal(t, []string{
		"LOCK demo main: free",
		"Also live in this repository: " + mainSess.Name + " (idle 0 min)",
	}, rlLockLines(b), "a worktree on another machine is the same repository; its client reported the hook")
	wtSess := e.session(t, "rl-wt")

	b = e.rlStart(t, "rl-out", "/tmp/elsewhere", map[string]string{"host": localHostName})
	require.Empty(t, rlLockLines(b), "outside the repository: neither the lock nor the peers")
	_, _, ok = store.SessionRepoLocks(e.session(t, "rl-out"))
	require.False(t, ok)

	// Nothing changed: the prompts say nothing.
	require.Empty(t, rlLockLines(e.rlPrompt(t, "rl-main")))

	// The worktree's session takes the lock: the main checkout's next prompt
	// hears it once; the taker hears nothing of its own take.
	_, err := store.AcquireRepoLock(ctx, e.db, l.ID, wtSess.ID, 30*time.Minute, time.Now().UTC())
	require.NoError(t, err)
	got := rlLockLines(e.rlPrompt(t, "rl-main"))
	require.Len(t, got, 1)
	require.True(t, strings.HasPrefix(got[0], "LOCK demo main: held by "+wtSess.Name+" for 0 min, until "), got[0])
	require.Empty(t, rlLockLines(e.rlPrompt(t, "rl-main")), "once")
	require.Empty(t, rlLockLines(e.rlPrompt(t, "rl-wt")), "its own take is no news")

	// Released: the waiting session learns the lock is free, once.
	_, err = store.ReleaseRepoLock(ctx, e.db, l.ID, wtSess.ID, time.Now().UTC())
	require.NoError(t, err)
	require.Equal(t, []string{"LOCK demo main: free"}, rlLockLines(e.rlPrompt(t, "rl-main")))
	require.Empty(t, rlLockLines(e.rlPrompt(t, "rl-main")))
	require.Empty(t, rlLockLines(e.rlPrompt(t, "rl-wt")), "its own release is no news")
}

// With repo locks off a declared lock changes nothing: no line, no presence,
// nothing recorded on the session, and no prompt notice when the holder moves.
func TestRepoLocks_OffChangesNothing(t *testing.T) {
	e := newHostEnv(t, config.PlanCapture{})
	l := rlDeclare(t, e, "main")

	b := e.rlStart(t, "rl-main", "/work/demo", rlMainID(lockHooksOff))
	require.NotContains(t, b, "LOCK")
	e.rlStart(t, "rl-wt", "/srv/demo-feature", rlWorktreeID)
	sess := e.session(t, "rl-main")
	require.NotContains(t, sess.Metadata, store.SessionMetaRepoLockAliases)
	require.NotContains(t, sess.Metadata, store.SessionMetaRepoLockSeen)

	_, err := store.AcquireRepoLock(context.Background(), e.db, l.ID, e.session(t, "rl-wt").ID, time.Hour, time.Now().UTC())
	require.NoError(t, err)
	require.NotContains(t, e.rlPrompt(t, "rl-main"), "LOCK")
}

// "hooks off" is the client's word: shown only when it said off, never when it
// said on or -- an older seam -- said nothing.
func TestRepoLocks_HooksOffOnlyWhenTheClientSaidSo(t *testing.T) {
	e := newHostEnv(t, config.PlanCapture{})
	rlOn(t, e)
	rlDeclare(t, e, "main")
	for _, tc := range []struct {
		name, hooks string
		want        bool
	}{{"off", lockHooksOff, true}, {"on", lockHooksOn, false}, {"not sent", "", false}} {
		t.Run(tc.name, func(t *testing.T) {
			b := e.rlStart(t, "rl-hooks-"+tc.name, "/work/demo", rlMainID(tc.hooks))
			require.Equal(t, tc.want, strings.Contains(b, "LOCK hooks off"), b)
			require.Contains(t, b, "LOCK demo main: free")
		})
	}
}

// A side clone on another machine, whose origin is a path on that machine, is
// its origin's repository only through the lock_origin its client followed:
// the daemon never reads another machine's path.
func TestRepoLocks_RemoteSideCloneResolvesThroughLockOrigin(t *testing.T) {
	e := newHostEnv(t, config.PlanCapture{})
	rlOn(t, e)
	rlDeclare(t, e, "main")
	side := map[string]string{"host": remoteHostName, "repo_root": "/srv/side", "main_root": "/srv/side", "origin": "/srv/demo"}

	require.Empty(t, rlLockLines(e.rlStart(t, "rl-side-raw", "/srv/side", side)),
		"without lock_origin its key is beta:/srv/demo, which no lock records")
	side["lock_origin"] = rlOrigin
	require.Equal(t, []string{"LOCK demo main: free"}, rlLockLines(e.rlStart(t, "rl-side", "/srv/side", side)))
}

// An older client on the daemon's own machine sends no roots: the daemon fills
// them from its own disk and follows a side clone's local-path origin, as the
// lock tools do.
func TestRepoLocks_LocalOlderClientIsFilledFromTheDaemonsDisk(t *testing.T) {
	e := newHostEnv(t, config.PlanCapture{})
	rlOn(t, e)
	rlDeclare(t, e, "main")
	upstream := rlRepo(t, "demo", rlOrigin)
	side := rlRepo(t, "side", upstream)
	require.Equal(t, []string{"LOCK demo main: free"},
		rlLockLines(e.rlStart(t, "rl-local", side, map[string]string{"host": localHostName})))
}

// The daemon reads a checkout for a lock only on its own machine and only with
// no View on the request.
func TestLockDiskOK(t *testing.T) {
	h := NewHandler(Config{APIKey: testKey, LocalHost: localHostName})
	ctx := context.Background()
	require.True(t, h.lockDiskOK(ctx, localHostName))
	require.True(t, h.lockDiskOK(ctx, ""), "an older client is on this machine")
	require.False(t, h.lockDiskOK(ctx, remoteHostName))
	p, err := access.New(config.PeerAuth{Enabled: true, Owner: "owner"}, func(string) (access.Account, error) {
		return access.Account{UID: 501, Name: "owner"}, nil
	})
	require.NoError(t, err)
	require.False(t, h.lockDiskOK(access.WithView(ctx, p.OwnerView()), localHostName), "never under peer_auth")
}

// rlRepo makes a git checkout named name with an origin remote and returns its
// root.
func rlRepo(t *testing.T, name, origin string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".git", "config"),
		[]byte("[remote \"origin\"]\n\turl = "+origin+"\n"), 0o644))
	return root
}
