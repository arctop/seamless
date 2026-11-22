package diary

import (
	"strings"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/harvest"
	"github.com/arctop/seamless/internal/markdown"
)

// AboutMaxRunes caps SessionDay.About, cut on a word with an ellipsis.
const AboutMaxRunes = 160

// harvestMark is the auto-harvest prefix without its trailing space, as the
// hooks test findings for it (hooks.hookFindings).
var harvestMark = strings.TrimSpace(harvest.Prefix)

// explicitFindings reports whether findings are the agent's own: not empty,
// not the no-summary sentinel, and not an auto-harvest -- the hooks' own
// findings, which a session's end harvest may replace (hooks.hookFindings).
func explicitFindings(findings string) bool {
	f := strings.TrimSpace(findings)
	return f != "" && f != harvest.Fallback && !strings.HasPrefix(f, harvestMark)
}

// about is what a session was about, the first of these that exists: the
// first line of findings the agent wrote; its first prompt of the day; the
// first line of its harvested findings, without the harvest's prefix; and
// "Session in <label>". It returns the text and where it came from.
func about(findings, prompt, label string) (string, string) {
	f := strings.TrimSpace(findings)
	if explicitFindings(f) {
		if line := firstLine(f); line != "" {
			return line, AboutFindings
		}
	}
	if line := firstLine(prompt); line != "" {
		return line, AboutPrompt
	}
	if rest, ok := strings.CutPrefix(f, harvestMark); ok {
		if line := firstLine(rest); line != "" {
			return line, AboutHarvested
		}
	}
	return "Session in " + label, AboutDefault
}

// firstLine is text's first line with words in it, as plain text (markdown
// syntax stripped, whitespace collapsed), capped at AboutMaxRunes.
func firstLine(text string) string {
	for line := range strings.Lines(text) {
		if plain := markdown.PlainText(line); plain != "" {
			return core.TruncateWords(plain, AboutMaxRunes)
		}
	}
	return ""
}

// ExplicitFindings reports whether findings are the agent's own, the test
// SessionDay.About applies before it reads a first prompt: the console's
// Sessions list titles a row by its first prompt only when they are not.
func ExplicitFindings(findings string) bool { return explicitFindings(findings) }

// FirstLine is text's first line with words in it, as plain text, capped at
// AboutMaxRunes: how SessionDay.About shows a prompt or findings, and how the
// console's Sessions list shows a session's first prompt.
func FirstLine(text string) string { return firstLine(text) }
