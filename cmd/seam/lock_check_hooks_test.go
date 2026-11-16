//go:build !windows

package main

// The repo-lock hooks end to end: real repositories under t.TempDir, the
// committed .githooks scripts, a seam binary built from this tree, and the
// stand-in daemon answering locks_list as each row needs. Nothing here reaches
// a real daemon or a real git config: SEAMLESS_CONFIG, HOME and every global
// git config point inside the test, and every inherited GIT_* variable is
// dropped (a test run from inside a git hook would otherwise inherit its
// index).

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/gitread"
)

// lcHarness is one end-to-end world: the binaries, the daemon, the remote.
type lcHarness struct {
	base    string // the test's folder
	binDir  string // seam, first on PATH
	gitDir  string // the folder git is in
	cfgPath string
	server  string // the daemon's URL, as the config names it
	env     []string

	fx       *daemonFixture
	requests atomic.Int64 // every HTTP request the daemon saw

	mu       sync.Mutex
	answer   string // locks_list's reply for the checkout with lcOrigin
	lastArgs map[string]any
}

// lcGitAtLeast228 skips the test without a git that runs reference-transaction.
func lcGitAtLeast228(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git executable")
	}
	out, err := exec.Command(path, "version").Output()
	require.NoError(t, err)
	m := regexp.MustCompile(`git version (\d+)\.(\d+)`).FindStringSubmatch(string(out))
	require.Len(t, m, 3, "unreadable git version %q", out)
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	if major < 2 || (major == 2 && minor < 28) {
		t.Skipf("git %s.%s predates the reference-transaction hook (2.28)", m[1], m[2])
	}
	return path
}

// lcNewHarness builds seam, starts the stand-in daemon, and makes the remote
// (a bare repository the origin URL lcOrigin is rewritten to) with one commit
// that carries this tree's .githooks scripts.
func lcNewHarness(t *testing.T) *lcHarness {
	t.Helper()
	gitPath := lcGitAtLeast228(t)
	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	h := &lcHarness{base: base, binDir: filepath.Join(base, "bin"), gitDir: filepath.Dir(gitPath)}

	build := exec.Command("go", "build", "-buildvcs=false", "-o", filepath.Join(h.binDir, "seam"), ".")
	build.Env = append(lcCleanEnv(os.Environ()), "PATH="+os.Getenv("PATH"))
	out, err := build.CombinedOutput()
	require.NoError(t, err, "go build ./cmd/seam: %s", out)

	h.answer = lcAnswer(lcTerminal)
	h.fx = newDaemonFixture(t, map[string]toolFunc{
		"locks_list": func(args map[string]any) toolReply {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.lastArgs = args
			if args["repo_origin"] != lcOrigin {
				return toolReply{text: lcAnswer(lcTerminal)}
			}
			return toolReply{text: h.answer}
		},
	}, nil, "")
	counting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.requests.Add(1)
		h.fx.mux.ServeHTTP(w, r)
	}))
	t.Cleanup(counting.Close)

	home := filepath.Join(base, "home")
	require.NoError(t, os.MkdirAll(home, 0o700))
	h.cfgPath = filepath.Join(home, "seamless.yaml")
	h.server = counting.URL
	h.writeConfig(t, h.server)
	remotes := filepath.Join(base, "remotes")
	gitconfig := "[url \"" + remotes + "/\"]\n\tinsteadOf = git@github.com:example/\n" +
		"[init]\n\tdefaultBranch = main\n[advice]\n\tdetachedHead = false\n"
	require.NoError(t, os.WriteFile(filepath.Join(home, "gitconfig"), []byte(gitconfig), 0o600))
	h.env = append(lcCleanEnv(os.Environ()),
		"HOME="+home, "USERPROFILE="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"GIT_CONFIG_GLOBAL="+filepath.Join(home, "gitconfig"), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=example", "GIT_AUTHOR_EMAIL=agent@example.com",
		"GIT_COMMITTER_NAME=example", "GIT_COMMITTER_EMAIL=agent@example.com",
		"SEAMLESS_CONFIG="+h.cfgPath)
	h.setPath(h.binDir, h.gitDir, "/usr/bin", "/bin")

	// The remote, seeded with the hook scripts this tree commits.
	seed := filepath.Join(base, "seed")
	h.mustGit(t, base, "init", "-q", seed)
	for _, name := range []string{"reference-transaction", "pre-push"} {
		body, err := os.ReadFile(filepath.Join("..", "..", ".githooks", name))
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(filepath.Join(seed, ".githooks"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(seed, ".githooks", name), body, 0o755))
	}
	h.mustGit(t, seed, "add", ".githooks")
	h.mustGit(t, seed, "commit", "-q", "-m", "seed")
	h.mustGit(t, base, "clone", "-q", "--bare", seed, filepath.Join(remotes, "example-app.git"))
	return h
}

// lcCleanEnv drops every SEAMLESS_* and GIT_* variable, and PATH, from env.
func lcCleanEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(k, "SEAMLESS_") && !strings.HasPrefix(k, "GIT_") && k != "PATH" {
			out = append(out, kv)
		}
	}
	return out
}

func (h *lcHarness) writeConfig(t *testing.T, serverURL string) {
	cfg := "server_url: \"" + serverURL + "\"\nmcp:\n  api_key: \"lc-key\"\nupdate:\n  check: false\n"
	require.NoError(t, os.WriteFile(h.cfgPath, []byte(cfg), 0o600))
}

// setPath sets PATH to dirs.
func (h *lcHarness) setPath(dirs ...string) {
	env := h.env[:0:0]
	for _, kv := range h.env {
		if !strings.HasPrefix(kv, "PATH=") {
			env = append(env, kv)
		}
	}
	h.env = append(env, "PATH="+strings.Join(dirs, string(os.PathListSeparator)))
}

// setAnswer sets the lock locks_list reports for this repository.
func (h *lcHarness) setAnswer(answer string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.answer = answer
}

// lastArg is one argument of the stand-in's last locks_list call, read under
// the lock the handler writes it under (the daemon serves on its own
// goroutines).
func (h *lcHarness) lastArg(key string) any {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lastArgs[key]
}

// git runs git in dir and returns its exit code and combined output.
func (h *lcHarness) git(t *testing.T, dir string, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(filepath.Join(h.gitDir, "git"), args...)
	cmd.Dir = dir
	cmd.Env = h.env
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), string(out)
	}
	require.NoError(t, err)
	return 0, string(out)
}

func (h *lcHarness) mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	code, out := h.git(t, dir, args...)
	require.Equal(t, 0, code, "git %v: %s", args, out)
	return strings.TrimSpace(out)
}

// clone makes a checkout of lcOrigin at name, with the hooks on.
func (h *lcHarness) clone(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(h.base, name)
	h.mustGit(t, h.base, "clone", "-q", lcOrigin, dir)
	h.mustGit(t, dir, "config", "core.hooksPath", ".githooks")
	return dir
}

func (h *lcHarness) rev(t *testing.T, dir, ref string) string {
	t.Helper()
	return h.mustGit(t, dir, "rev-parse", "--verify", "-q", ref)
}

// lcHeldByAnother is the lock held by another session, refs/heads/main and
// every v* tag guarded.
func lcHeldByAnother(bound *bool) string {
	return lcAnswer(bound, lcLock(lcLockOpts{held: true, holder: "cc/lc-holder", refs: []string{"refs/heads/main", "refs/tags/v*"}}))
}

// lcRequireRefused asserts a refused git command whose output names the lock,
// its holder, since and until: a bare "refused" fails it.
func lcRequireRefused(t *testing.T, code int, out string) {
	t.Helper()
	require.NotEqual(t, 0, code, "refused: %s", out)
	for _, w := range []string{"LOCK example-app main", "session cc/lc-holder holds the lock",
		"since 2026-11-15T18:00:00Z", "until 2026-11-15T18:15:00Z", "seam lock acquire --wait"} {
		require.Contains(t, out, w)
	}
}

func TestLockCheckHooks_EndToEnd(t *testing.T) {
	h := lcNewHarness(t)
	main := h.clone(t, "main")
	other := h.clone(t, "other-host")
	require.True(t, gitread.LockHooksActive(main), "the committed hooks under core.hooksPath=.githooks are on")
	h.mustGit(t, other, "config", "--unset", "core.hooksPath")
	require.False(t, gitread.LockHooksActive(other), "the control: the same checkout with the hooks off")
	h.mustGit(t, other, "config", "core.hooksPath", ".githooks")

	// Work to land: a topic branch, which no lock guards.
	h.mustGit(t, main, "checkout", "-q", "-b", "topic")
	h.mustGit(t, main, "commit", "-q", "--allow-empty", "-m", "topic work")
	h.mustGit(t, main, "checkout", "-q", "main")
	mainAt := h.rev(t, main, "refs/heads/main")

	t.Run("held by another session", func(t *testing.T) {
		h.setAnswer(lcHeldByAnother(lcAgent))

		code, out := h.git(t, main, "commit", "--allow-empty", "-m", "on main")
		lcRequireRefused(t, code, out)
		require.Equal(t, mainAt, h.rev(t, main, "refs/heads/main"), "main did not move")
		require.Equal(t, lcOrigin, h.lastArg("repo_origin"))

		code, out = h.git(t, other, "commit", "--allow-empty", "-m", "on main elsewhere")
		lcRequireRefused(t, code, out)
		require.Equal(t, filepath.Join(h.base, "other-host"), h.lastArg("repo_root"),
			"a second clone at another path finds the lock by its origin alone")

		code, out = h.git(t, main, "commit", "--no-verify", "--allow-empty", "-m", "skipping hooks")
		lcRequireRefused(t, code, out)
		require.Contains(t, out, "--no-verify does not skip the reference-transaction hook")

		code, out = h.git(t, main, "merge", "--ff-only", "topic")
		lcRequireRefused(t, code, out)
		code, out = h.git(t, main, "update-ref", "refs/heads/main", "topic")
		lcRequireRefused(t, code, out)
		require.Equal(t, mainAt, h.rev(t, main, "refs/heads/main"))

		code, out = h.git(t, main, "tag", "v1.0")
		lcRequireRefused(t, code, out)
		h.mustGit(t, main, "tag", "nightly") // a tag no lock guards

		code, out = h.git(t, main, "push", "origin", "topic:main")
		lcRequireRefused(t, code, out)
		require.Contains(t, out, "this push moves refs/heads/main on origin")
		require.Equal(t, mainAt, h.rev(t, filepath.Join(h.base, "remotes", "example-app.git"), "refs/heads/main"))

		code, out = h.git(t, main, "pack-refs", "--all")
		require.Equal(t, 0, code, "pack-refs rewrites refs without moving them: %s", out)

		// The control for every row above: the same lock held by a session the
		// caller may not see is refused too, in words that name no session.
		h.setAnswer(lcAnswer(lcAgent, lcLock(lcLockOpts{held: true})))
		code, out = h.git(t, main, "commit", "--allow-empty", "-m", "on main")
		require.NotEqual(t, 0, code)
		require.Contains(t, out, "another session holds the lock")
		require.NotContains(t, out, "cc/lc-holder")
	})

	t.Run("held by the caller", func(t *testing.T) {
		h.setAnswer(lcAnswer(lcAgent, lcLock(lcLockOpts{held: true, mine: true, holder: "cc/lc-me",
			refs: []string{"refs/heads/main", "refs/tags/v*"}})))
		h.mustGit(t, other, "commit", "-q", "--allow-empty", "-m", "on main elsewhere")
		h.mustGit(t, main, "merge", "-q", "--ff-only", "topic")
		h.mustGit(t, main, "update-ref", "refs/heads/main", "topic")
		h.mustGit(t, main, "commit", "-q", "--allow-empty", "-m", "on main")
		h.mustGit(t, main, "tag", "v1.0")
		h.mustGit(t, main, "push", "-q", "origin", "main", "v1.0")
		require.Equal(t, h.rev(t, main, "refs/heads/main"), h.rev(t, filepath.Join(h.base, "remotes", "example-app.git"), "refs/heads/main"))
	})

	t.Run("free", func(t *testing.T) {
		h.setAnswer(lcAnswer(lcAgent, lcLock(lcLockOpts{refs: []string{"refs/heads/main", "refs/tags/v*"}})))
		code, out := h.git(t, main, "commit", "--allow-empty", "-m", "an agent, unlocked")
		require.NotEqual(t, 0, code, out)
		require.Contains(t, out, "this session does not hold it")

		h.setAnswer(lcAnswer(lcTerminal, lcLock(lcLockOpts{refs: []string{"refs/heads/main", "refs/tags/v*"}})))
		h.mustGit(t, main, "commit", "-q", "--allow-empty", "-m", "the owner at a terminal")
		h.mustGit(t, main, "tag", "v1.1")
	})

	t.Run("a fetch brings guarded tags without asking", func(t *testing.T) {
		h.setAnswer(lcHeldByAnother(lcAgent))
		h.mustGit(t, main, "push", "-q", "--no-verify", "origin", "main", "v1.1") // the holder's release, as it were
		before := h.requests.Load()
		code, out := h.git(t, other, "fetch", "-q", "origin")
		require.Equal(t, 0, code, out)
		require.Equal(t, h.rev(t, main, "refs/tags/v1.1"), h.rev(t, other, "refs/tags/v1.1"))
		require.Equal(t, before, h.requests.Load(), "a fetch never asks the daemon")
	})

	t.Run("a detached-HEAD commit in a worktree asks nothing", func(t *testing.T) {
		h.setAnswer(lcHeldByAnother(lcAgent))
		wt := filepath.Join(h.base, "wt")
		h.mustGit(t, main, "worktree", "add", "-q", "--detach", wt)
		before := h.requests.Load()
		h.mustGit(t, wt, "commit", "-q", "--allow-empty", "-m", "detached work")
		require.Equal(t, before, h.requests.Load(), "no daemon call")

		// The control: the same worktree moving main does call, and is refused.
		code, out := h.git(t, wt, "update-ref", "refs/heads/main", "HEAD")
		lcRequireRefused(t, code, out)
		require.Greater(t, h.requests.Load(), before)
	})

	t.Run("the daemon down", func(t *testing.T) {
		closed := httptest.NewServer(http.NotFoundHandler())
		closed.Close()
		h.writeConfig(t, closed.URL)
		t.Cleanup(func() { h.writeConfig(t, h.server) })
		code, out := h.git(t, main, "commit", "--allow-empty", "-m", "daemon down")
		require.Equal(t, 0, code, out)
		require.Contains(t, out, "seam lock check: warning: seamlessd is unreachable")
	})
}

// The scripts refuse on exit 77 alone: an old seam (no lock check: its
// --help exits 2), a failing one (exit 1) and none at all warn and allow;
// only the "prepared" state reaches seam, and a transaction with no ref under
// refs/ (a commit on a detached HEAD) runs no seam at all.
func TestLockCheckHooks_ExitCodeContract(t *testing.T) {
	h := lcNewHarness(t)
	main := h.clone(t, "main")
	fake := filepath.Join(h.base, "fake")
	require.NoError(t, os.MkdirAll(fake, 0o755))
	calls := filepath.Join(h.base, "fake-calls")
	writeFake := func(help, code int) {
		script := "#!/bin/sh\necho \"$*\" >> " + calls + "\n" +
			"if [ \"$3\" = --help ]; then echo 'usage from the fake seam' >&2; exit " + strconv.Itoa(help) + "; fi\n" +
			"cat >/dev/null\necho 'error: from the fake seam' >&2\nexit " + strconv.Itoa(code) + "\n"
		require.NoError(t, os.WriteFile(filepath.Join(fake, "seam"), []byte(script), 0o755))
	}
	readCalls := func() []string {
		got, err := os.ReadFile(calls)
		if os.IsNotExist(err) {
			return nil
		}
		require.NoError(t, err)
		return strings.Split(strings.TrimSpace(string(got)), "\n")
	}
	h.setPath(fake, h.gitDir, "/usr/bin", "/bin")

	for _, tc := range []struct {
		name       string
		help, code int
		refused    bool
		warn       string
		calls      []string
	}{
		{name: "an older seam", help: 2, warn: "this seam has no lock check (it predates repo locks)",
			calls: []string{"lock check --help"}},
		{name: "a failing seam", code: 1, warn: "seam lock check exited 1",
			calls: []string{"lock check --help", "lock check --hook reference-transaction -- prepared"}},
		{name: "allowed", calls: []string{"lock check --help", "lock check --hook reference-transaction -- prepared"}},
		{name: "refused", code: 77, refused: true,
			calls: []string{"lock check --help", "lock check --hook reference-transaction -- prepared"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeFake(tc.help, tc.code)
			_ = os.Remove(calls)
			code, out := h.git(t, main, "commit", "--allow-empty", "-m", tc.name)
			if tc.refused {
				require.NotEqual(t, 0, code, out)
				require.Contains(t, out, "error: from the fake seam", "a refusal prints seam's own words")
			} else {
				require.Equal(t, 0, code, out)
				if tc.warn != "" {
					require.Contains(t, out, tc.warn)
				}
			}
			require.NotContains(t, out, "usage from the fake seam", "the probe is silent")
			require.Equal(t, tc.calls, readCalls(), "only the prepared transaction that moves a ref under refs/ reaches seam")
		})
	}

	t.Run("a detached-HEAD commit runs no seam", func(t *testing.T) {
		writeFake(0, 77)
		h.mustGit(t, main, "checkout", "-q", "--detach")
		_ = os.Remove(calls)
		h.mustGit(t, main, "commit", "-q", "--allow-empty", "-m", "detached")
		require.Empty(t, readCalls())
		h.mustGit(t, main, "checkout", "-q", "main")
	})

	t.Run("no seam on PATH", func(t *testing.T) {
		require.NoError(t, os.Remove(filepath.Join(fake, "seam")))
		code, out := h.git(t, main, "commit", "--allow-empty", "-m", "no seam")
		require.Equal(t, 0, code, out)
		require.Contains(t, out, "reference-transaction: seam is not on PATH")
	})
}

// The hook's connection names the agent that ran git, not git: the
// X-Seamless-Agent-Process header seam sends from a hook is this test's own
// process, which ran git (internal/agentproc passes over git and the hook's
// shell).
func TestLockCheckHooks_TheHookNamesTheProcessThatRanGit(t *testing.T) {
	h := lcNewHarness(t)
	main := h.clone(t, "main")
	h.setAnswer(lcAnswer(lcAgent, lcLock(lcLockOpts{held: true, mine: true})))
	h.mustGit(t, main, "commit", "-q", "--allow-empty", "-m", "on main")
	got := h.fx.lastCall(t).header.Get(agentProcessHeader)
	require.True(t, strings.HasPrefix(got, strconv.Itoa(os.Getpid())+"."),
		"the anchor is the test process that ran git (%d), not git: got %q", os.Getpid(), got)
}
