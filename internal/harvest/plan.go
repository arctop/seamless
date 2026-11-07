package harvest

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/arctop/seamless/internal/validate"
)

// PlanFilePath validates that a tool-input path is a .md file directly under
// plansDir (Claude Code plan mode's ~/.claude/plans) and returns the cleaned
// absolute path. A leading ~/ expands to the home directory of the process
// asking, which is why the daemon asks only of its own disk and `seam hook`
// of the agent's. The file's basename (PlanBasename) must be a safe name: it
// becomes a note slug.
func PlanFilePath(plansDir, raw string) (string, bool) {
	if plansDir == "" || raw == "" {
		return "", false
	}
	path := raw
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		path = filepath.Join(home, path[2:])
	}
	path = filepath.Clean(path)
	if filepath.Dir(path) != filepath.Clean(plansDir) || filepath.Ext(path) != ".md" {
		return "", false
	}
	if validate.Name(PlanBasename(path)) != nil {
		return "", false
	}
	return path, true
}

// PlanBasename is a plan's correlation key: the file name without .md.
func PlanBasename(path string) string {
	return strings.TrimSuffix(filepath.Base(path), ".md")
}
