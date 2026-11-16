package gitread

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// lhHookScript is a reference-transaction hook that runs the repo-lock check.
const lhHookScript = "#!/bin/sh\n# the repo-lock check\n[ \"$1\" = prepared ] || exit 0\n" +
	"msg=$(seam lock check --hook reference-transaction -- \"$@\" 2>&1)\n"

// lhIsolate points every global git config this reader (and git) could read
// at files inside the test, so the machine's own ~/.gitconfig never leaks in.
// It returns the global config file and the home folder.
func lhIsolate(t *testing.T) (global, home string) {
	t.Helper()
	// t.TempDir can sit under a symlinked root (macOS /var); git reports real paths.
	home, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	global = filepath.Join(home, "global.gitconfig")
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	return global, home
}

// lhRepo makes a checkout at a fresh folder: a .git folder whose config is cfg.
func lhRepo(t *testing.T, cfg string) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".git", "HEAD"), "ref: refs/heads/main\n")
	writeFile(t, filepath.Join(root, ".git", "config"), "[core]\n\tbare = false\n"+cfg)
	return root
}

// lhHook writes an executable hook file at path.
func lhHook(t *testing.T, path, body string) {
	t.Helper()
	writeFile(t, path, body)
	require.NoError(t, os.Chmod(path, 0o755))
}

func TestLockHooksActive_TheHooksFolderGitRunsFrom(t *testing.T) {
	global, home := lhIsolate(t)
	for _, tc := range []struct {
		name   string
		cfg    string // the repository's config, after its [core] header
		global string
		hook   string // where the hook goes, relative to the root ("~/" = home)
		want   bool
	}{
		{name: "the repository's own hooks folder", hook: ".git/hooks/reference-transaction", want: true},
		{name: "a relative hooksPath, read against the root", cfg: "\thooksPath = .githooks\n",
			hook: ".githooks/reference-transaction", want: true},
		{name: "hooksPath set elsewhere skips the own folder", cfg: "\thooksPath = .githooks\n",
			hook: ".git/hooks/reference-transaction", want: false},
		{name: "an absolute hooksPath", cfg: "\thooksPath = " + filepath.Join(home, "abs-hooks") + "\n",
			hook: "~/abs-hooks/reference-transaction", want: true},
		{name: "a ~/ hooksPath", cfg: "\thooksPath = ~/tilde-hooks\n", hook: "~/tilde-hooks/reference-transaction", want: true},
		{name: "an empty hooksPath runs no hooks", cfg: "\thooksPath =\n", hook: ".git/hooks/reference-transaction", want: false},
		{name: "a quoted value with a comment", cfg: "\thooksPath = \"my hooks\" ; shared\n",
			hook: "my hooks/reference-transaction", want: true},
		{name: "section and key in any case", cfg: "[Core]\n\tHOOKSPATH = .githooks\n",
			hook: ".githooks/reference-transaction", want: true},
		{name: "a variable on the header's line", cfg: "[core] hooksPath = .githooks\n",
			hook: ".githooks/reference-transaction", want: true},
		{name: "a subsection is another section", cfg: "[core \"x\"]\n\thooksPath = .githooks\n",
			hook: ".githooks/reference-transaction", want: false},
		{name: "the last value wins", cfg: "\thooksPath = elsewhere\n\thooksPath = .githooks\n",
			hook: ".githooks/reference-transaction", want: true},
		{name: "the global config's hooksPath", global: "[core]\n\thooksPath = ~/global-hooks\n",
			hook: "~/global-hooks/reference-transaction", want: true},
		{name: "the repository's config wins over the global one", cfg: "\thooksPath = .githooks\n",
			global: "[core]\n\thooksPath = ~/global-hooks\n", hook: "~/global-hooks/reference-transaction", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, os.WriteFile(global, []byte(tc.global), 0o644))
			cfg := tc.cfg
			if strings.HasPrefix(cfg, "[") {
				cfg = "\n" + cfg
			}
			root := lhRepo(t, cfg)
			path := filepath.Join(root, filepath.FromSlash(tc.hook))
			if rest, ok := strings.CutPrefix(tc.hook, "~/"); ok {
				path = filepath.Join(home, filepath.FromSlash(rest))
				t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(path)) })
			}
			lhHook(t, path, lhHookScript)
			require.Equal(t, tc.want, LockHooksActive(root))
		})
	}
}

func TestLockHooksActive_TheHookItself(t *testing.T) {
	lhIsolate(t)
	for _, tc := range []struct {
		name string
		body string
		mode os.FileMode
		want bool
	}{
		{name: "runs seam lock check", body: lhHookScript, mode: 0o755, want: true},
		{name: "spaced out", body: "#!/bin/sh\nexec   seam  lock   check --hook reference-transaction\n", mode: 0o755, want: true},
		{name: "only in a comment", body: "#!/bin/sh\n# seam lock check --hook reference-transaction\nexit 0\n", mode: 0o755},
		{name: "another hook", body: "#!/bin/sh\necho seam lock list\n", mode: 0o755},
		{name: "not executable", body: lhHookScript, mode: 0o644, want: runtime.GOOS == "windows"},
		{name: "binary content", body: "seam lock check\x00\x01", mode: 0o755},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := lhRepo(t, "")
			path := filepath.Join(root, ".git", "hooks", "reference-transaction")
			writeFile(t, path, tc.body)
			require.NoError(t, os.Chmod(path, tc.mode))
			require.Equal(t, tc.want, LockHooksActive(root))
		})
	}

	t.Run("a symlink to the script", func(t *testing.T) {
		root := lhRepo(t, "")
		target := filepath.Join(root, "scripts", "lock-hook")
		lhHook(t, target, lhHookScript)
		require.NoError(t, os.MkdirAll(filepath.Join(root, ".git", "hooks"), 0o755))
		require.NoError(t, os.Symlink(target, filepath.Join(root, ".git", "hooks", "reference-transaction")))
		require.True(t, LockHooksActive(root))
	})

	t.Run("no checkout", func(t *testing.T) {
		require.False(t, LockHooksActive(""))
		require.False(t, LockHooksActive(t.TempDir()))
	})

	t.Run("a pre-push hook alone is not enough", func(t *testing.T) {
		root := lhRepo(t, "")
		lhHook(t, filepath.Join(root, ".git", "hooks", "pre-push"), lhHookScript)
		require.False(t, LockHooksActive(root))
	})
}

// A linked worktree runs its hooks from the folder its own config names: a
// relative hooksPath reads against the worktree's root, and with none set the
// main repository's hooks folder serves every worktree.
func TestLockHooksActive_LinkedWorktrees(t *testing.T) {
	lhIsolate(t)

	t.Run("the shared hooks folder", func(t *testing.T) {
		main := lhRepo(t, "")
		wt := filepath.Join(t.TempDir(), "wt")
		mkLinkedWorktree(t, main, wt, "wt", true)
		require.False(t, LockHooksActive(wt))
		lhHook(t, filepath.Join(main, ".git", "hooks", "reference-transaction"), lhHookScript)
		require.True(t, LockHooksActive(wt))
	})

	t.Run("a relative hooksPath is the worktree's own copy", func(t *testing.T) {
		main := lhRepo(t, "\thooksPath = .githooks\n")
		wt := filepath.Join(t.TempDir(), "wt")
		mkLinkedWorktree(t, main, wt, "wt", false)
		lhHook(t, filepath.Join(main, ".githooks", "reference-transaction"), lhHookScript)
		require.True(t, LockHooksActive(main))
		require.False(t, LockHooksActive(wt), "a worktree checked out before the hooks existed runs none")
		lhHook(t, filepath.Join(wt, ".githooks", "reference-transaction"), lhHookScript)
		require.True(t, LockHooksActive(wt))
	})

	t.Run("config.worktree, only with extensions.worktreeConfig", func(t *testing.T) {
		main := lhRepo(t, "")
		wt := filepath.Join(t.TempDir(), "wt")
		mkLinkedWorktree(t, main, wt, "wt", false)
		admin := filepath.Join(main, ".git", "worktrees", "wt")
		writeFile(t, filepath.Join(admin, "config.worktree"), "[core]\n\thooksPath = .wthooks\n")
		lhHook(t, filepath.Join(wt, ".wthooks", "reference-transaction"), lhHookScript)
		require.False(t, LockHooksActive(wt), "git reads config.worktree only with the extension on")

		writeFile(t, filepath.Join(main, ".git", "config"), "[core]\n\tbare = false\n[extensions]\n\tworktreeConfig = true\n")
		require.True(t, LockHooksActive(wt))
	})
}

// The folder this reader finds is the one git itself reports, for each
// config shape git can be asked about. Skipped without a git executable.
func TestLockHooksActive_AgreesWithGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git executable")
	}
	global, home := lhIsolate(t)
	for _, tc := range []struct{ name, local, global string }{
		{name: "unset"},
		{name: "relative", local: ".githooks"},
		{name: "absolute", local: filepath.Join(home, "abs")},
		{name: "tilde", local: "~/tilde"},
		{name: "global", global: "~/global"},
		{name: "local over global", local: ".githooks", global: "~/global"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			git := func(args ...string) string {
				t.Helper()
				cmd := exec.Command("git", args...)
				cmd.Dir = root
				out, err := cmd.CombinedOutput()
				require.NoError(t, err, "git %v: %s", args, out)
				return strings.TrimSpace(string(out))
			}
			git("init", "-q")
			gcfg := ""
			if tc.global != "" {
				gcfg = "[core]\n\thooksPath = " + tc.global + "\n"
			}
			require.NoError(t, os.WriteFile(global, []byte(gcfg), 0o644))
			if tc.local != "" {
				git("config", "core.hooksPath", tc.local)
			}
			cmd := exec.Command("git", "rev-parse", "--path-format=absolute", "--git-path", "hooks")
			cmd.Dir = root
			out, err := cmd.Output()
			if err != nil {
				t.Skipf("this git cannot report its hooks folder (--path-format needs 2.31): %v", err)
			}
			want := strings.TrimSpace(string(out))
			real, err := filepath.EvalSymlinks(root)
			require.NoError(t, err)
			gitDir, err := filepath.EvalSymlinks(filepath.Join(root, ".git"))
			require.NoError(t, err)
			got, ok := hooksDir(real, gitDir)
			require.True(t, ok)
			wantReal, err := filepath.EvalSymlinks(filepath.Dir(want))
			require.NoError(t, err)
			require.Equal(t, filepath.Join(wantReal, filepath.Base(want)), got)
		})
	}
}

func TestConfigValue(t *testing.T) {
	for raw, want := range map[string]string{
		" .githooks":                ".githooks",
		" .githooks ; comment":      ".githooks",
		" .githooks # comment":      ".githooks",
		` "with space" `:            "with space",
		` " lead and trail " `:      " lead and trail ",
		` a  b `:                    "a  b",
		` "a;b#c"`:                  "a;b#c",
		` path\\with\"quote`:        `path\with"quote`,
		` tab\there`:                "tab\there",
		"":                          "",
		" ;only a comment":          "",
		` "unterminated ; not cut`:  "unterminated ; not cut",
		` /dev/null`:                "/dev/null",
		"\t~/hooks\r":               "~/hooks",
		` "quoted" and "more" text`: "quoted and more text",
	} {
		require.Equal(t, want, configValue(raw), "configValue(%q)", raw)
	}
}

func TestRefValue(t *testing.T) {
	const a = "0123456789abcdef0123456789abcdef01234567"
	const b = "feedfacefeedfacefeedfacefeedfacefeedface"
	main := lhRepo(t, "")
	writeFile(t, filepath.Join(main, ".git", "refs", "heads", "main"), a+"\n")
	writeFile(t, filepath.Join(main, ".git", "refs", "heads", "sym"), "ref: refs/heads/main\n")
	writeFile(t, filepath.Join(main, ".git", "packed-refs"),
		"# pack-refs with: peeled fully-peeled sorted\n"+b+" refs/tags/v1\n^"+a+"\n"+b+" refs/heads/main\n")
	wt := filepath.Join(t.TempDir(), "wt")
	mkLinkedWorktree(t, main, wt, "wt", true)

	for _, root := range []string{main, wt} {
		require.Equal(t, a, RefValue(root, "refs/heads/main"), "the loose ref wins over packed-refs")
		require.Equal(t, b, RefValue(root, "refs/tags/v1"), "packed-refs")
		require.Empty(t, RefValue(root, "refs/heads/sym"), "a symbolic ref's value is unknown here")
		require.Empty(t, RefValue(root, "refs/heads/none"))
		require.Empty(t, RefValue(root, "HEAD"), "only refs under refs/")
		require.Empty(t, RefValue(root, "refs/heads/../../config"))
	}
	require.Empty(t, RefValue(t.TempDir(), "refs/heads/main"))
}
