package gitread

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// LockHooksActive reports whether the checkout at root runs the repo-lock
// hooks: git would run a reference-transaction hook there (from core.hooksPath,
// or else the repository's own hooks folder) that checks the repository's
// locks with `seam lock check`. A checkout whose lock is declared but which
// runs no such hook shows "hooks off" in the briefing and in `seam doctor`.
// root is a checkout root as RepoRoot returns it; "" or a path outside any
// repository reports false.
//
// The hooks folder is found as git finds it. core.hooksPath is read from the
// user's global config, the repository's config and, with
// extensions.worktreeConfig on, the worktree's config.worktree; the last of
// them to set it wins. A relative value is read against root, as git reads it
// against the worktree it runs the hook in, and "~/" against the home folder.
// An empty value runs no hooks. With none set, the folder is the repository's
// own hooks folder, which a linked worktree shares with its main checkout. The
// hook there must be a file git can run (executable, on Unix) with a line, not
// a comment, that runs `seam lock check`.
//
// It does not follow these, so a checkout set up through one of them reports
// false: the system config (its place depends on how git was built), include
// and includeIf, a -c or GIT_CONFIG_* override on one git command, a value
// continued over a line with a trailing backslash, and the config-defined
// hooks (hook.<name>.command) that newer gits run beside the hooks folder.
func LockHooksActive(root string) bool {
	if root == "" {
		return false
	}
	gitDir := resolveGitDir(root)
	if gitDir == "" {
		return false
	}
	dir, ok := hooksDir(root, gitDir)
	if !ok {
		return false
	}
	return runsLockCheck(filepath.Join(dir, "reference-transaction"))
}

// lockCheckWords is what a reference-transaction hook runs to check the locks.
const lockCheckWords = "seam lock check"

// maxHookBytes bounds how much of a hook file is read: a hook is a short
// script, and a large binary there is not one of these.
const maxHookBytes = 64 << 10

// hooksDir is the folder git runs the checkout's hooks from; false when it
// runs none (core.hooksPath set to "", or a "~/" with no home to read it in).
func hooksDir(root, gitDir string) (string, bool) {
	common := commonDir(gitDir)
	files := globalConfigFiles()
	files = append(files, filepath.Join(common, "config"))
	if isTrue(lastConfigValue(filepath.Join(common, "config"), "extensions", "worktreeconfig")) {
		files = append(files, filepath.Join(gitDir, "config.worktree"))
	}
	var path *string
	for _, f := range files {
		if v := lastConfigValue(f, "core", "hookspath"); v != nil {
			path = v
		}
	}
	if path == nil {
		return filepath.Join(common, "hooks"), true
	}
	p := *path
	if p == "" {
		return "", false
	}
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return "", false
		}
		p = filepath.Join(home, rest)
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, p)
	}
	return filepath.Clean(p), true
}

// commonDir is the repository folder gitDir shares with its main checkout:
// gitDir itself, or the folder a linked worktree's commondir file names.
func commonDir(gitDir string) string {
	dirs := sharedDirs(gitDir)
	return dirs[len(dirs)-1]
}

// globalConfigFiles are the user's global git config files, in the order git
// reads them: $GIT_CONFIG_GLOBAL alone when it is set, else the XDG file
// ($XDG_CONFIG_HOME/git/config, or ~/.config/git/config) and then
// ~/.gitconfig.
func globalConfigFiles() []string {
	if f := os.Getenv("GIT_CONFIG_GLOBAL"); f != "" {
		return []string{f}
	}
	var files []string
	xdg := os.Getenv("XDG_CONFIG_HOME")
	if xdg != "" {
		files = append(files, filepath.Join(xdg, "git", "config"))
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return files
	}
	if xdg == "" {
		files = append(files, filepath.Join(home, ".config", "git", "config"))
	}
	return append(files, filepath.Join(home, ".gitconfig"))
}

// isTrue reports a git boolean that is set and true: a bare key, or true,
// yes, on or 1 in any case.
func isTrue(v *string) bool {
	if v == nil {
		return false
	}
	switch strings.ToLower(*v) {
	case "true", "yes", "on", "1":
		return true
	}
	return false
}

// lastConfigValue returns the last value the git config file at path gives
// section.key (both matched in any case, in a section with no subsection), or
// nil when the file is unreadable or never sets it. A key with no '=' is a
// bare boolean and reads as "true".
func lastConfigValue(path, section, key string) *string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out *string
	in := false
	for line := range strings.Lines(string(b)) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			end := strings.IndexByte(line, ']')
			if end < 0 {
				in = false
				continue
			}
			in = strings.EqualFold(strings.TrimSpace(line[1:end]), section)
			// git takes a variable on the header's own line: "[core] hooksPath = x".
			line = strings.TrimSpace(line[end+1:])
		}
		if !in || line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		name, value, hasValue := strings.Cut(line, "=")
		if !strings.EqualFold(strings.TrimSpace(name), key) {
			continue
		}
		v := "true"
		if hasValue {
			v = configValue(value)
		}
		out = &v
	}
	return out
}

// configValue decodes one git config value: blanks around it dropped, a
// comment (# or ;) outside quotes dropped, double quotes removed, and the
// escapes \\ \" \n \t \b decoded. Blanks inside quotes, and blanks between
// words, are kept.
func configValue(raw string) string {
	var b strings.Builder
	quoted, started := false, false
	pending := "" // blanks outside quotes, kept only once more text follows
	flush := func() {
		if started {
			b.WriteString(pending)
		}
		pending, started = "", true
	}
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch {
		case c == '\\' && i+1 < len(raw):
			flush()
			i++
			switch raw[i] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'b':
				b.WriteByte('\b')
			default:
				b.WriteByte(raw[i])
			}
		case c == '"':
			flush()
			quoted = !quoted
		case quoted:
			b.WriteByte(c)
		case c == '#' || c == ';':
			return b.String()
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			pending += string(c)
		default:
			flush()
			b.WriteByte(c)
		}
	}
	return b.String()
}

// runsLockCheck reports whether the hook at path is a file git can run whose
// text runs `seam lock check` on a line that is not a comment.
func runsLockCheck(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		return false // git does not run a hook it cannot execute
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxHookBytes))
	if err != nil || bytes.IndexByte(b, 0) >= 0 {
		return false
	}
	for line := range strings.Lines(string(b)) {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		if strings.Contains(strings.Join(strings.Fields(line), " "), lockCheckWords) {
			return true
		}
	}
	return false
}

// RefValue returns the object name the ref refName holds in the repository of
// the checkout at root: its loose ref file, else its packed-refs line, read in
// the checkout's own git folder and then the repository's common one. It is ""
// when the ref does not exist, holds a symbolic ref, or cannot be read (a
// repository on the reftable backend among them), so "" never says a ref is
// absent, only that its value is unknown here.
func RefValue(root, refName string) string {
	gitDir := resolveGitDir(root)
	if gitDir == "" || !strings.HasPrefix(refName, "refs/") || strings.Contains(refName, "..") {
		return ""
	}
	dirs := sharedDirs(gitDir)
	for _, d := range dirs {
		b, err := os.ReadFile(filepath.Join(d, filepath.FromSlash(refName)))
		if err != nil {
			continue
		}
		if v := strings.TrimSpace(string(b)); isHex(v) {
			return v
		}
		return "" // a symbolic ref, or not a ref file at all
	}
	for _, d := range dirs {
		if hash := packedRef(filepath.Join(d, "packed-refs"), refName); isHex(hash) {
			return hash
		}
	}
	return ""
}
