package console

import (
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// stDiff renders an edit script one row per entry: " x" kept, "-x" removed,
// "+x" added, "~N" a fold of N unchanged lines.
func stDiff(before, after []string, rows []diffRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		switch r.Op {
		case diffSame:
			out = append(out, " "+after[r.New])
		case diffDel:
			out = append(out, "-"+before[r.Old])
		case diffAdd:
			out = append(out, "+"+after[r.New])
		case diffFold:
			out = append(out, "~"+strconv.Itoa(r.Folded))
		}
	}
	return out
}

func TestStSplitLines(t *testing.T) {
	require.Equal(t, []string{"a", "b"}, splitLines("a\nb\n"), "a final newline ends the last line")
	require.Equal(t, []string{"a", "b"}, splitLines("a\nb"))
	require.Equal(t, []string{"a", ""}, splitLines("a\n\n"), "a blank last line is still a line")
	require.Equal(t, []string{""}, splitLines(""))
}

func TestStLineDiff_Cases(t *testing.T) {
	for _, tc := range []struct {
		name          string
		before, after string
		want          []string
	}{
		{"unchanged", "a\nb\n", "a\nb\n", []string{" a", " b"}},
		{"one line replaced", "a\nb\nc", "a\nB\nc", []string{" a", "-b", "+B", " c"}},
		{"a line added", "a\nc", "a\nb\nc", []string{" a", "+b", " c"}},
		{"a line removed", "a\nb\nc", "a\nc", []string{" a", "-b", " c"}},
		{"the first and last lines changed", "a\nb\nc", "A\nb\nC", []string{"-a", "+A", " b", "-c", "+C"}},
		{"everything new", "x\ny", "a\nb", []string{"-x", "-y", "+a", "+b"}},
		{"from nothing", "", "a", []string{"-", "+a"}},
		{"a moved line keeps the rest", "a\nb\nc\nd", "b\nc\nd\na", []string{"-a", " b", " c", " d", "+a"}},
		{"removals come before additions", "a\nx\ny\nb", "a\np\nq\nb", []string{" a", "-x", "-y", "+p", "+q", " b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, after := splitLines(tc.before), splitLines(tc.after)
			require.Equal(t, tc.want, stDiff(before, after, lineDiff(before, after)))
		})
	}
}

// For any two short texts, the script replays both sides exactly, and keeps a
// longest common subsequence: as many lines as an independent brute-force
// count says can be kept.
func TestStLineDiff_ReplaysBothSidesAndKeepsTheMost(t *testing.T) {
	rng := rand.New(rand.NewPCG(20, 1121))
	gen := func() []string {
		lines := make([]string, rng.IntN(7))
		for i := range lines {
			lines[i] = string(rune('a' + rng.IntN(3)))
		}
		return lines
	}
	var lcs func(a, b []string) int
	lcs = func(a, b []string) int {
		if len(a) == 0 || len(b) == 0 {
			return 0
		}
		if a[0] == b[0] {
			return 1 + lcs(a[1:], b[1:])
		}
		return max(lcs(a[1:], b), lcs(a, b[1:]))
	}
	for range 2000 {
		before, after := gen(), gen()
		rows := lineDiff(before, after)
		var gotBefore, gotAfter []string
		kept := 0
		for _, r := range rows {
			switch r.Op {
			case diffSame:
				require.Equal(t, before[r.Old], after[r.New])
				gotBefore, gotAfter = append(gotBefore, before[r.Old]), append(gotAfter, after[r.New])
				kept++
			case diffDel:
				gotBefore = append(gotBefore, before[r.Old])
			case diffAdd:
				gotAfter = append(gotAfter, after[r.New])
			}
		}
		require.Equal(t, strings.Join(before, ","), strings.Join(gotBefore, ","), "%v -> %v", before, after)
		require.Equal(t, strings.Join(after, ","), strings.Join(gotAfter, ","), "%v -> %v", before, after)
		require.Equal(t, lcs(before, after), kept, "%v -> %v", before, after)
		require.Equal(t, kept != len(before) || kept != len(after), diffChanged(rows))
	}
}

// Past the table's budget the changed middle is reported removed and added
// whole: still true, only coarser, and the common head and tail stay kept.
func TestStLineDiff_PastTheBudgetReportsTheMiddleWhole(t *testing.T) {
	n := 1100
	require.Greater(t, n*n, diffCellBudget)
	before, after := []string{"head"}, []string{"head"}
	for i := range n {
		before = append(before, "old "+strconv.Itoa(i))
		after = append(after, "new "+strconv.Itoa(i))
	}
	before, after = append(before, "tail"), append(after, "tail")
	got := stDiff(before, after, lineDiff(before, after))
	require.Len(t, got, 2*n+2)
	require.Equal(t, " head", got[0])
	require.Equal(t, "-old 0", got[1])
	require.Equal(t, "-old 1099", got[n])
	require.Equal(t, "+new 0", got[n+1])
	require.Equal(t, " tail", got[2*n+1])
}

func TestStFoldUnchanged(t *testing.T) {
	var before []string
	for i := range 20 {
		before = append(before, "l"+strconv.Itoa(i))
	}
	after := append([]string(nil), before...)
	after[10] = "L10"
	got := stDiff(before, after, foldUnchanged(lineDiff(before, after), diffContext))
	require.Equal(t, []string{"~7", " l7", " l8", " l9", "-l10", "+L10", " l11", " l12", " l13", "~6"}, got,
		"three lines of context either side; nothing kept above the first change or below the last")

	// Between two changes, a run one line longer than both contexts keeps
	// that line rather than fold one line into a marker.
	after = append([]string(nil), before...)
	after[2], after[10] = "L2", "L10"
	got = stDiff(before, after, foldUnchanged(lineDiff(before, after), diffContext))
	require.Equal(t, []string{" l0", " l1", "-l2", "+L2", " l3", " l4", " l5", " l6", " l7", " l8", " l9", "-l10", "+L10",
		" l11", " l12", " l13", "~6"}, got)
}
