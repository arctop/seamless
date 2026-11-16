package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// lkRepo makes a checkout at dir whose origin is origin ("" for none).
func lkRepo(t *testing.T, dir, origin string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".git"), 0o755))
	cfg := "[core]\n\tbare = false\n"
	if origin != "" {
		cfg += "[remote \"origin\"]\n\turl = " + origin + "\n"
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".git", "config"), []byte(cfg), 0o644))
	// t.TempDir can sit under a symlinked root (macOS /var): resolve it, as
	// filepath.Abs in the command does not.
	real, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	return real
}

const (
	lkLockJSON = `{"id":"01KLOCKLOCKLOCKLOCKLOCKLOC","label":"app main","repo":"app","name":"main",` +
		`"refs":["refs/heads/main"],"state":"active","held":true,"holder":"01KHOLDER","holder_session":"cc/example-agent",` +
		`"since":"2026-11-15T18:00:00.000000000Z","until":"2026-11-15T18:15:00.000000000Z","aliases":[],"projects":[]}`
	lkFreeJSON = `{"id":"01KLOCKLOCKLOCKLOCKLOCKLOC","label":"app main","repo":"app","name":"main",` +
		`"refs":["refs/heads/main"],"state":"active","held":false,"aliases":[],"projects":[]}`
	lkRequestedJSON = `{"id":"01KLOCKREQUESTEDREQUESTEDR","label":"app release","repo":"app","name":"release",` +
		`"refs":["refs/heads/release"],"state":"requested","held":false,"aliases":[],"projects":[]}`
)

func TestLock_DeclareSendsTheCheckoutResolvedHere(t *testing.T) {
	repo := lkRepo(t, filepath.Join(t.TempDir(), "app"), "git@github.com:example/app.git")
	sub := filepath.Join(repo, "pkg")
	require.NoError(t, os.MkdirAll(sub, 0o755))
	fx := newDaemonFixture(t, map[string]toolFunc{
		"locks_declare": fixedReply(`{"lock":` + lkFreeJSON + `,"declared":true,"recorded_aliases":[]}`),
	}, nil, "")

	stdout, stderr, code := fx.run(t, "lock", "declare", "--cwd", sub, "--refs", "refs/heads/main,refs/tags/v*")
	require.Equal(t, 0, code, stderr)
	require.Equal(t, "declared LOCK app main (refs/heads/main)\n", stdout)
	args := fx.lastCall(t).args
	require.Equal(t, sub, args["cwd"])
	require.Equal(t, repo, args["repo_root"])
	require.Equal(t, repo, args["main_worktree_root"])
	require.Equal(t, "git@github.com:example/app.git", args["repo_origin"])
	require.Equal(t, "refs/heads/main,refs/tags/v*", args["refs"])
	require.NotContains(t, args, "name", "an absent flag sends nothing: the server's default applies")
}

// A side clone whose origin is a local path is a checkout of that path's
// repository: seam follows the chain on this machine to a URL, or to the last
// local repository, which the lock then keys on by host and path.
func TestLock_ASideCloneSendsTheRepositoryItWasClonedFrom(t *testing.T) {
	base := t.TempDir()
	upstream := lkRepo(t, filepath.Join(base, "upstream"), "https://github.com/example/app")
	local := lkRepo(t, filepath.Join(base, "local"), "")
	viaURL := lkRepo(t, filepath.Join(base, "side-a"), upstream)
	viaPath := lkRepo(t, filepath.Join(base, "side-b"), "file://"+local)
	relative := lkRepo(t, filepath.Join(base, "side-c"), "../upstream")

	fx := newDaemonFixture(t, map[string]toolFunc{
		"locks_acquire": fixedReply(`{"lock":` + lkLockJSON + `,"acquired":true,"heartbeat":false,"reclaimed":false}`),
	}, nil, "")
	for _, tc := range []struct {
		name, dir, want string
	}{
		{"a clone of a checkout with a URL", viaURL, "https://github.com/example/app"},
		{"a clone of a checkout with no origin", viaPath, local},
		{"a relative origin", relative, "https://github.com/example/app"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, stderr, code := fx.run(t, "lock", "acquire", "--cwd", tc.dir)
			require.Equal(t, 0, code, stderr)
			require.Equal(t, tc.want, fx.lastCall(t).args["repo_origin"])
		})
	}
}

func TestLock_HumanLines(t *testing.T) {
	repo := lkRepo(t, filepath.Join(t.TempDir(), "app"), "https://github.com/example/app")
	fx := newDaemonFixture(t, map[string]toolFunc{
		"locks_list": func(a map[string]any) toolReply {
			if a["repo_root"] != nil {
				return toolReply{text: `{"locks":[]}`}
			}
			return toolReply{text: `{"locks":[` + lkLockJSON + `,` + lkRequestedJSON + `]}`}
		},
		"locks_acquire": fixedReply(`{"lock":` + lkLockJSON + `,"acquired":true,"heartbeat":true,"reclaimed":true}`),
		"locks_release": fixedReply(`{"lock":` + lkFreeJSON + `,"released":true}`),
		"locks_declare": fixedReply(`{"lock":` + lkRequestedJSON + `,"declared":true,"recorded_aliases":[]}`),
	}, map[string]string{
		"POST /console/locks/01KLOCKLOCKLOCKLOCKLOCKLOC/release?format=json": `{"lock":` + lkFreeJSON +
			`,"prior_holder":"01KHOLDER","prior_holder_session":"cc/example-agent"}`,
	}, "")

	stdout, stderr, code := fx.run(t, "lock", "list")
	require.Equal(t, 0, code, stderr)
	require.Equal(t, "LOCK app main  held by cc/example-agent until 2026-11-15T18:15:00Z  refs/heads/main\n"+
		"LOCK app release  waits for the owner's approval  refs/heads/release\n", stdout)
	require.Empty(t, fx.lastCall(t).args, "a plain list names no checkout")

	stdout, _, code = fx.run(t, "lock", "list", "--here", "--cwd", repo)
	require.Equal(t, 0, code)
	require.Equal(t, "(no repo locks)\n", stdout)

	stdout, _, code = fx.run(t, "lock", "list", "--json")
	require.Equal(t, 0, code)
	require.Equal(t, `{"locks":[`+lkLockJSON+`,`+lkRequestedJSON+`]}`+"\n", stdout, "--json is the daemon's answer as sent")

	stdout, stderr, code = fx.run(t, "lock", "acquire", "--cwd", repo, "--lease", "600", "--session", "cc/example-agent")
	require.Equal(t, 0, code, stderr)
	require.Equal(t, "refreshed LOCK app main until 2026-11-15T18:15:00Z (taken over from a lapsed hold)\n", stdout)
	args := fx.lastCall(t).args
	require.EqualValues(t, 600, args["lease_seconds"])
	require.Equal(t, "cc/example-agent", args["session"])

	stdout, _, code = fx.run(t, "lock", "release", "--id", "01KLOCKLOCKLOCKLOCKLOCKLOC")
	require.Equal(t, 0, code)
	require.Equal(t, "released LOCK app main\n", stdout)
	require.Equal(t, map[string]any{"id": "01KLOCKLOCKLOCKLOCKLOCKLOC"}, fx.lastCall(t).args)

	stdout, stderr, code = fx.run(t, "lock", "release", "--force", "--id", "01KLOCKLOCKLOCKLOCKLOCKLOC")
	require.Equal(t, 0, code, stderr)
	require.Equal(t, "force-released LOCK app main (it was held by cc/example-agent)\n", stdout)

	stdout, _, code = fx.run(t, "lock", "declare", "--cwd", repo, "--name", "release")
	require.Equal(t, 0, code)
	require.Equal(t, "requested LOCK app release (refs/heads/release): it waits for the owner's approval on the console's Locks page\n", stdout)
}

// A server that does not serve the lock tools -- repo locks switched off
// there, or one that predates them -- is said in those words.
func TestLock_AServerWithoutTheToolsSaysSo(t *testing.T) {
	fx := newDaemonFixture(t, map[string]toolFunc{"tasks_list": fixedReply(`{"tasks":[]}`)}, nil, "")
	_, stderr, code := fx.run(t, "lock", "list")
	require.Equal(t, 1, code)
	require.Contains(t, stderr, "does not offer the repo lock tools: repo locks are switched off there")
	require.Contains(t, stderr, "Settings -> Features")

	// A tool the server does serve keeps its own refusal.
	fx = newDaemonFixture(t, map[string]toolFunc{
		"locks_release": func(map[string]any) toolReply {
			return toolReply{text: "locks_release: lock not held by this session", isError: true}
		},
	}, nil, "")
	_, stderr, code = fx.run(t, "lock", "release", "--id", "01KLOCK")
	require.Equal(t, 1, code)
	require.Contains(t, stderr, "lock not held by this session")
	require.NotContains(t, stderr, "does not offer")
}

func TestLock_ParseAndCheckoutRefusals(t *testing.T) {
	outside := t.TempDir()
	for _, tc := range []struct {
		name string
		argv []string
		code int
		want string
	}{
		{"no positional", []string{"lock", "declare", "main"}, 2, "takes no positional arguments"},
		{"a zero lease", []string{"lock", "acquire", "--lease", "0"}, 2, ""},
		{"id and a checkout", []string{"lock", "acquire", "--id", "01K", "--name", "main"}, 1, "not both"},
		{"outside a repository", []string{"lock", "declare", "--cwd", outside}, 1, "is not inside a git repository"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _, errb := stubEnv()
			require.Equal(t, tc.code, dispatch(context.Background(), e, tc.argv), errb.String())
			require.Contains(t, errb.String(), tc.want)
		})
	}
}

// The CLI reference's synopses are the specs' own, and bare `seam lock` names
// its subcommands like bare `seam task`.
func TestLock_SynopsesAndTheBareFamily(t *testing.T) {
	require.Equal(t, "seam lock list [--cwd DIR] [--here] [--json]", synopsis(lockListCmd))
	require.Equal(t, "seam lock declare [--cwd DIR] [--json] [--name NAME] [--projects SLUGS] [--refs REFS] [--session NAME]",
		synopsis(lockDeclareCmd))
	require.Equal(t, "seam lock acquire [--cwd DIR] [--id ID] [--json] [--lease SECONDS] [--name NAME] [--session NAME] [--timeout D] [--wait]",
		synopsis(lockAcquireCmd))
	require.Equal(t, "seam lock release [--cwd DIR] [--force] [--id ID] [--json] [--name NAME] [--session NAME]",
		synopsis(lockReleaseCmd))

	e, _, errb := stubEnv()
	require.Equal(t, 2, dispatch(context.Background(), e, []string{"lock"}))
	require.Contains(t, errb.String(), "valid values are lock list, lock declare, lock acquire, lock release")
}
