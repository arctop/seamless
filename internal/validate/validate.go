// Package validate provides shared input-validation helpers enforcing the
// filesystem-safety invariants in AGENTS.md:
//   - Reject file paths containing "..", absolute paths, or null bytes.
//   - Sanitize names (memory names, project slugs, note slugs) for filesystem safety.
//
// Ported from Seam v1 (internal/validate) with one deliberate change: Title no
// longer rejects "..". Titles are slugified before any filesystem use, and the
// old check bounced legitimate titles such as "summary 2026-07-05..08" (a 37%
// error rate on create in v1). Name and Path keep the ".." guard because their
// inputs feed filesystem operations directly.
package validate

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/arctop/seamless/internal/core"
)

var (
	// ErrPathTraversal is returned when a path contains "..", is absolute, or
	// contains null bytes.
	ErrPathTraversal = errors.New("path contains traversal sequence, absolute path, or null bytes")

	// ErrUnsafeName is returned when a name contains filesystem-unsafe
	// characters or patterns.
	ErrUnsafeName = errors.New("name contains unsafe characters")
)

const (
	// GlobalProject is the human-facing token for the project-less scope.
	GlobalProject = "global"
	// AllProjects is reserved for operations that deliberately widen across
	// every project; it can never be an ordinary project slug.
	AllProjects = "all"
)

// Path rejects file paths that could escape a base directory. It checks for
// ".." components, absolute paths, and null bytes.
func Path(path string) error {
	if path == "" {
		return fmt.Errorf("validate.Path: empty path")
	}
	if strings.ContainsRune(path, 0) {
		return fmt.Errorf("validate.Path: %w: null byte", ErrPathTraversal)
	}
	if filepath.IsAbs(path) {
		return fmt.Errorf("validate.Path: %w: absolute path", ErrPathTraversal)
	}

	cleaned := filepath.Clean(path)
	if slices.Contains(strings.Split(cleaned, string(filepath.Separator)), "..") {
		return fmt.Errorf("validate.Path: %w: dot-dot component", ErrPathTraversal)
	}
	return nil
}

// PathWithinDir validates a path and then verifies the resolved absolute path
// stays within the given base directory.
func PathWithinDir(relPath, baseDir string) error {
	if err := Path(relPath); err != nil {
		return err
	}
	absPath := filepath.Clean(filepath.Join(baseDir, relPath))
	baseDir = filepath.Clean(baseDir)

	if !strings.HasPrefix(absPath, baseDir+string(filepath.Separator)) && absPath != baseDir {
		return fmt.Errorf("validate.PathWithinDir: %w: resolved path escapes base directory", ErrPathTraversal)
	}
	return nil
}

// Title validates human-facing titles and descriptions. It is more permissive
// than Name: it allows "/" (e.g. "TCP/IP", "A/B testing") and ".." (e.g. date
// ranges like "2026-07-05..08") because titles are slugified before any
// filesystem use. It rejects empty strings, null bytes, backslashes, and
// lengths over 255.
func Title(title string) error {
	if title == "" {
		return fmt.Errorf("validate.Title: title is empty")
	}
	if len(title) > 255 {
		return fmt.Errorf("validate.Title: %w: exceeds 255 characters", ErrUnsafeName)
	}
	if strings.ContainsRune(title, 0) {
		return fmt.Errorf("validate.Title: %w: null byte", ErrUnsafeName)
	}
	if strings.Contains(title, "\\") {
		return fmt.Errorf("validate.Title: %w: backslash", ErrUnsafeName)
	}
	return nil
}

// Name rejects names used to build filenames (memory names, project/note slugs)
// that are unsafe on any supported platform. The corpus is routinely synced or
// restored across operating systems, so accepting a name only Unix can create
// would turn a later Windows checkout into a partial, misleading corpus.
func Name(name string) error {
	if name == "" {
		return fmt.Errorf("validate.Name: name is empty")
	}
	if len(name) > 255 {
		return fmt.Errorf("validate.Name: %w: exceeds 255 characters", ErrUnsafeName)
	}
	if strings.ContainsRune(name, 0) {
		return fmt.Errorf("validate.Name: %w: null byte", ErrUnsafeName)
	}
	if strings.Contains(name, "/") {
		return fmt.Errorf("validate.Name: %w: forward slash", ErrUnsafeName)
	}
	if strings.Contains(name, "\\") {
		return fmt.Errorf("validate.Name: %w: backslash", ErrUnsafeName)
	}
	if strings.ContainsAny(name, `<>:"|?*`) {
		return fmt.Errorf("validate.Name: %w: Windows-forbidden punctuation", ErrUnsafeName)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("validate.Name: %w: ASCII control character U+%04X", ErrUnsafeName, r)
		}
	}
	if strings.Contains(name, "..") {
		return fmt.Errorf("validate.Name: %w: dot-dot sequence", ErrUnsafeName)
	}
	// A leading dot makes the file invisible to `ls` and to anyone reviewing
	// their own corpus -- and `.` alone becomes the file "..md", which reads as
	// a traversal attempt to every human who sees it even though it is not one.
	// Neither is ever a name someone means.
	if strings.HasPrefix(name, ".") {
		return fmt.Errorf("validate.Name: %w: leading dot", ErrUnsafeName)
	}
	if strings.HasSuffix(name, ".") {
		return fmt.Errorf("validate.Name: %w: trailing dot", ErrUnsafeName)
	}
	if strings.HasSuffix(name, " ") {
		return fmt.Errorf("validate.Name: %w: trailing space", ErrUnsafeName)
	}
	if reservedDeviceName(name) {
		return fmt.Errorf("validate.Name: %w: %q is a reserved device name on Windows", ErrUnsafeName, name)
	}
	return nil
}

// Project normalizes the two global-scope spellings and validates an ordinary
// project slug with Name. The cross-project widening token is reserved rather
// than accepted as a project that other surfaces could never address.
func Project(explicit string) (string, error) {
	explicit = strings.TrimSpace(explicit)
	switch explicit {
	case GlobalProject, "_global":
		return "", nil
	case "":
		return "", nil
	case AllProjects:
		return "", fmt.Errorf("project %q is reserved for operations over every project", explicit)
	}
	if err := Name(explicit); err != nil {
		return "", fmt.Errorf("invalid project %q: %w", explicit, err)
	}
	return explicit, nil
}

// Isolation validates a project isolation state at an input boundary. The
// accepted values derive from core.Isolations (never hand-transcribed). The
// empty string is rejected: absence is the caller's decision (default open); a
// present-but-empty value is not interpretable.
func Isolation(s string) (core.Isolation, error) {
	iso := core.Isolation(strings.TrimSpace(s))
	if !iso.Valid() {
		names := make([]string, len(core.Isolations))
		for i, v := range core.Isolations {
			names[i] = string(v)
		}
		return "", fmt.Errorf("invalid isolation %q: valid values are %s", s, strings.Join(names, ", "))
	}
	return iso, nil
}

// CommitIDs reads a list of commit ids at an input boundary. Each entry is
// trimmed and lowercased -- hex is case-insensitive, so that reading is
// lossless -- and must then be a full commit id (core.IsCommitID: 40 or 64 hex
// digits). An abbreviation is refused, never expanded: it names a different
// commit in every repository that grows a colliding prefix. A blank entry is
// dropped, a repeat keeps its first place, and more than max distinct ids is
// refused rather than cut (max <= 0 sets no bound). Nothing valid is nil.
func CommitIDs(raw []string, max int) ([]string, error) {
	var out []string
	seen := make(map[string]bool, len(raw))
	for i, r := range raw {
		id := strings.ToLower(strings.TrimSpace(r))
		if id == "" {
			continue
		}
		if !core.IsCommitID(id) {
			return nil, fmt.Errorf("entry %d (%s) is not a full commit id: pass the 40- or 64-character hex "+
				"name git prints for the commit (git rev-parse HEAD), not an abbreviation", i, quoteShort(r))
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	if max > 0 && len(out) > max {
		return nil, fmt.Errorf("%d distinct commit ids: one call records at most %d", len(out), max)
	}
	return out, nil
}

// quoteShort quotes a caller's value for an error message, cut to a readable
// length: the message names the entry, it does not echo a whole paste back.
func quoteShort(s string) string {
	const limit = 72
	if r := []rune(s); len(r) > limit {
		return fmt.Sprintf("%q...", string(r[:limit]))
	}
	return fmt.Sprintf("%q", s)
}

// windowsReservedNames are the legacy DOS device names. Windows resolves them
// as devices in EVERY directory, so `memory/proj/con.md` is not a file there --
// the create either fails or opens the console. Rejected on all platforms
// rather than only on Windows, because the corpus is plain markdown that gets
// synced, backed up, and cloned between machines: a name that works on the
// laptop it was written on and breaks on a Windows box is worse than a name
// that was never allowed. (Audit I11.)
var windowsReservedNames = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com0": true, "com1": true, "com2": true, "com3": true, "com4": true,
	"com5": true, "com6": true, "com7": true, "com8": true, "com9": true,
	"lpt0": true, "lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true,
	"lpt5": true, "lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
}

// reservedDeviceName reports whether name is a Windows device name. The check is
// case-insensitive and ignores anything from the first dot, because Windows
// treats "NUL.md" and "nul.txt.md" as the device too -- the extension does not
// rescue the name.
func reservedDeviceName(name string) bool {
	base, _, _ := strings.Cut(name, ".")
	return windowsReservedNames[strings.ToLower(strings.TrimSpace(base))]
}
