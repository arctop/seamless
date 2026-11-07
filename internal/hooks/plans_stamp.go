package hooks

// The plan-file vocabulary: which paths count as a plan, the correlation key
// derived from one, and the provenance stamp prepended to a captured body.

import (
	"fmt"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/harvest"
)

// planFilePath validates that a tool-input path is a .md file directly under
// the plans dir (defense in depth behind the CLI pre-filter) and returns the
// cleaned absolute path (harvest.PlanFilePath).
func (h *Handler) planFilePath(raw string) (string, bool) {
	return harvest.PlanFilePath(h.plansDir, raw)
}

// planBasename is the plan's correlation key: the file name without .md
// (harvest.PlanBasename).
func planBasename(path string) string {
	return harvest.PlanBasename(path)
}

// planStamp is the provenance blockquote prepended to every captured plan body.
// plans.StampHead (and cmd/seam's local transcription) parse the git field back
// out; keep the formats in step.
func planStamp(sessionName, basename string, iter int, head string, now time.Time) string {
	return fmt.Sprintf("> captured from %s | %s.md | iter %d | git %s | %s",
		stampSession(sessionName), basename, iter, shortHead(head), now.UTC().Format(time.RFC3339))
}

// stampSession formats an already-resolved capturing session name. Plan capture
// is Claude Code-only, so the unknown fallback remains a cc/ handle.
func stampSession(sessionName string) string {
	if sessionName == "" {
		return "cc/unknown"
	}
	return sessionName
}

// shortHead abbreviates a commit hash for stamps ("unknown" when absent).
func shortHead(head string) string {
	if head == "" {
		return "unknown"
	}
	if len(head) > 12 {
		return head[:12]
	}
	return head
}

// firstHeading returns the text of the first "# " heading line, or "".
func firstHeading(content string) string {
	for line := range strings.Lines(content) {
		if after, ok := strings.CutPrefix(strings.TrimSpace(line), "# "); ok {
			return strings.TrimSpace(after)
		}
	}
	return ""
}
