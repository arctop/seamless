package diary

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The console reference states the Diary's character thresholds and its
// spotlight's weights, caps, floor and short-session test in a reader's words
// (docs-src/reference/console.md, #diary-day and #diary-spotlight). Nothing
// generates those tables, so this holds them to the consts: a change to a
// threshold or a weight that leaves the page behind fails here, and the fix
// is to edit the page in the same change and run make docs.
func TestConsoleReference_StatesTheDiarysThresholdsAndWeights(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs-src", "reference", "console.md"))
	require.NoError(t, err)
	// Prose is wrapped at 80 columns: compare with every run of white space
	// read as one space.
	page := strings.Join(strings.Fields(string(raw)), " ")
	section := func(anchor string) string {
		t.Helper()
		start := strings.Index(page, "{#"+anchor+"}")
		require.GreaterOrEqual(t, start, 0, "console.md has a section {#%s}", anchor)
		rest := page[start:]
		if end := strings.Index(rest[1:], "{#"); end >= 0 {
			rest = rest[:end+1]
		}
		return rest
	}
	day, spotlight := section("diary-day"), section("diary-spotlight")

	for _, phrase := range []string{
		fmt.Sprintf("| Closing day | %d or more tasks closed as done |", ClosingDayTasks),
		fmt.Sprintf("or %d or more tasks created, and fewer than %d closed |", PlanningDayTasks, PlanningDayMaxClosed),
		fmt.Sprintf("| Deep work | %s or more of agent time, %d%% or more of it in one project |",
			Duration(DeepWorkMinutes), DeepWorkShare),
		fmt.Sprintf("| Busy day | %s or more of agent time, %d or more sessions beyond the short ones, or work in %d or more projects |",
			Duration(BusyDayMinutes), BusyDaySessions, BusyDayProjects),
		fmt.Sprintf("at least %s the day's agent time", shareWords(HeadlineMostlyShare)),
	} {
		require.Contains(t, day, phrase, "#diary-day states the rule as the code applies it")
	}

	for _, phrase := range []string{
		fmt.Sprintf("| The plan shipped: its last open step closed as done | %d | %d |", PointsShipped, PointsShipped),
		fmt.Sprintf("| Each task or step closed as done | %d | %d |", PointsClosed, CapClosed),
		fmt.Sprintf("| Each task or step created or claimed | %d | %d |", PointsStarted, CapStarted),
		fmt.Sprintf("| Each memory, note or trial its sessions wrote | %d | %d |", PointsKept, CapKept),
		fmt.Sprintf("| Each commit its tasks or sessions recorded | %d | %d |", PointsCommit, CapCommits),
		fmt.Sprintf("| Each session that ended with findings its agent wrote | %d | %d |", PointsFindings, CapFindings),
		fmt.Sprintf("| Agent time | 1 for every %d minutes | %d |", MinutesPerPoint, CapMinutes),
		fmt.Sprintf("| Output tokens | 1 for every %s | %d |", thousands(OutputPerPoint), CapOutput),
		fmt.Sprintf("A thread needs %d points to be a card, and at most %s are", SpotlightFloor, countWords(SpotlightCap)),
		fmt.Sprintf("under %d minutes of agent time and under %s output tokens", MinorMinutes, thousands(MinorOutput)),
	} {
		require.Contains(t, spotlight, phrase, "#diary-spotlight states the weight as the code applies it")
	}
}

// thousands writes n with comma separators, as the page does (80,000).
func thousands(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// countWords writes a small count as the page's prose does.
func countWords(n int) string {
	words := []string{"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten"}
	if n >= 0 && n < len(words) {
		return words[n]
	}
	return fmt.Sprint(n)
}

// shareWords writes a share in percent as the page's prose does.
func shareWords(percent int) string {
	if percent == 50 {
		return "half"
	}
	return fmt.Sprintf("%d%% of", percent)
}
