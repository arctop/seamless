package core

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// HoldLive is Task.ClaimLive's rule, pinned against it at every point that
// matters, so the two cannot drift: a lease is live only while it ends after
// now, strictly.
func TestRepoLock_HoldLiveIsClaimLive(t *testing.T) {
	now := time.Date(2026, 11, 15, 18, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		holder string
		state  RepoLockState
		lease  *time.Time
		live   bool
	}{
		{"a lease ending after now", "s", RepoLockActive, new(now.Add(time.Nanosecond)), true},
		{"a lease ending exactly now is dead", "s", RepoLockActive, new(now), false},
		{"a lapsed lease", "s", RepoLockActive, new(now.Add(-time.Second)), false},
		{"no lease stamped", "s", RepoLockActive, nil, false},
		{"no holder", "", RepoLockActive, new(now.Add(time.Hour)), false},
		{"a waiting declaration", "s", RepoLockRequested, new(now.Add(time.Hour)), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := RepoLock{Holder: tc.holder, State: tc.state, LeaseExpiresAt: tc.lease}
			require.Equal(t, tc.live, l.HoldLive(now))
			if tc.state == RepoLockActive {
				task := Task{ClaimedBy: tc.holder, Status: TaskInProgress, LeaseExpiresAt: tc.lease}
				require.Equal(t, task.ClaimLive(now), l.HoldLive(now), "the lock's rule is the claim's")
			}
		})
	}
}

func TestValidRepoLockName(t *testing.T) {
	for _, ok := range []string{"main", "release", "a", "release-1.2", "deploy_prod", "v2"} {
		require.NoError(t, ValidRepoLockName(ok), ok)
	}
	for _, bad := range []string{"", "Main", "-main", "main-", ".main", "main.", "re..lease", "main.lock", "main branch",
		"main/next", "ma\x00in", "é", string(make([]byte, 65))} {
		require.ErrorIs(t, ValidRepoLockName(bad), ErrRepoLockName, "%q", bad)
	}
}

func TestValidRepoLockRef(t *testing.T) {
	for _, ok := range []string{"refs/heads/main", "refs/tags/v*", "refs/heads/*", "refs/heads/release/1.2", "refs/notes/x"} {
		require.NoError(t, ValidRepoLockRef(ok), ok)
	}
	for _, bad := range []string{"", "main", "heads/main", "refs/", "refs/heads/", "refs//main", "refs/heads/a..b",
		"refs/heads/.hidden", "refs/heads/main.lock", "refs/heads/a b", "refs/heads/a~1", "refs/heads/a^", "refs/heads/a:b",
		"refs/heads/a?", "refs/heads/[a]", `refs/heads/a\b`, "refs/heads/a@{1}", "refs/heads/*/*", "refs/heads/main.",
		"refs/heads/\x01"} {
		require.ErrorIs(t, ValidRepoLockRef(bad), ErrRepoLockRef, "%q", bad)
	}
	require.Equal(t, []string{"refs/heads/main"}, DefaultRepoLockRefs("main"))
	for _, name := range []string{"main", "release-1.2", "a"} {
		for _, ref := range DefaultRepoLockRefs(name) {
			require.NoError(t, ValidRepoLockRef(ref), "every valid name's default ref is a valid ref")
		}
	}
}

func TestLocalOriginPath(t *testing.T) {
	for _, tc := range []struct {
		origin, path string
		local        bool
	}{
		{"/work/app", "/work/app", true},
		{"../app", "../app", true},
		{"app", "app", true},
		{"file:///work/app", "/work/app", true},
		{"FILE:///work/app", "/work/app", true},
		{"file://localhost/work/app", "/work/app", true},
		{`C:\repos\app`, `C:\repos\app`, true},
		{`\\srv\share\app`, `\\srv\share\app`, true},
		{"git@github.com:example/app.git", "", false},
		{"github.com:example/app", "", false},
		{"https://github.com/example/app", "", false},
		{"ssh://git@example.com:2222/team/app.git", "", false},
		{"file://otherhost/app", "", false},
		{"", "", false},
	} {
		p, ok := LocalOriginPath(tc.origin)
		require.Equal(t, tc.local, ok, tc.origin)
		require.Equal(t, tc.path, p, tc.origin)
	}
}

// A side clone keys on the repository it was cloned from: the chain is
// followed to the first URL, or to the last local repository, and a loop or a
// path that is no checkout ends it.
func TestFollowLocalOrigin(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "work")
	repos := map[string][2]string{ // path -> main root, origin
		filepath.Join(root, "upstream"): {filepath.Join(root, "upstream"), "https://example.com/team/app"},
		filepath.Join(root, "local"):    {filepath.Join(root, "local"), ""},
		filepath.Join(root, "middle"):   {filepath.Join(root, "middle"), filepath.Join(root, "upstream")},
		filepath.Join(root, "loop-a"):   {filepath.Join(root, "loop-a"), filepath.Join(root, "loop-b")},
		filepath.Join(root, "loop-b"):   {filepath.Join(root, "loop-b"), filepath.Join(root, "loop-a")},
	}
	read := func(p string) (string, string) { r := repos[p]; return r[0], r[1] }

	require.Equal(t, "", FollowLocalOrigin("", root, read))
	require.Equal(t, "git@github.com:example/app.git", FollowLocalOrigin("git@github.com:example/app.git", root, read),
		"a URL is returned as it is")
	require.Equal(t, "https://example.com/team/app", FollowLocalOrigin(filepath.Join(root, "middle"), root, read),
		"a clone of a clone of a URL keys on the URL")
	require.Equal(t, filepath.Join(root, "local"), FollowLocalOrigin("file://"+filepath.ToSlash(filepath.Join(root, "local")), root, read),
		"a repository with no origin keys on its path")
	require.Equal(t, "https://example.com/team/app", FollowLocalOrigin(filepath.Join("..", "upstream"), filepath.Join(root, "side"), read),
		"a relative path is read against the checkout that named it")
	require.Equal(t, filepath.Join(root, "bare.git"), FollowLocalOrigin(filepath.Join(root, "bare.git"), root, read),
		"a path that is no checkout keys on itself")
	got := FollowLocalOrigin(filepath.Join(root, "loop-a"), root, read)
	require.Contains(t, []string{filepath.Join(root, "loop-a"), filepath.Join(root, "loop-b")}, got, "a loop ends where it closes")
}

func TestRepoLock_RepoAndLabel(t *testing.T) {
	for _, tc := range []struct {
		aliases []RepoLockAlias
		repo    string
	}{
		{[]RepoLockAlias{{Kind: RepoLockAliasPath, Alias: "h:/work/other"}, {Kind: RepoLockAliasOrigin, Alias: "github.com/example/app"}}, "app"},
		{[]RepoLockAlias{{Kind: RepoLockAliasPath, Alias: "h:/work/app/"}}, "app"},
		{[]RepoLockAlias{{Kind: RepoLockAliasPath, Alias: `h:C:\repos\app`}}, "app"},
		{nil, ""},
	} {
		l := RepoLock{Name: "main", Aliases: tc.aliases}
		require.Equal(t, tc.repo, l.Repo())
		if tc.repo != "" {
			require.Equal(t, tc.repo+" main", l.Label())
		} else {
			require.Equal(t, "main", l.Label())
		}
	}
	require.True(t, RepoLockActive.Valid())
	require.False(t, RepoLockState("held").Valid())
}
