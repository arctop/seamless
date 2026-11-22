package console

import "strings"

// A small line diff for the Held screen: what releasing a held update would
// change in the file it replaces, removed lines and added lines. It is the
// console's own (no dependency), server-side, and sized for memory and note
// files, which run to tens of lines: a longest common subsequence over the
// lines that differ once the common head and tail are set aside.

// diffOp says what a diff row is.
type diffOp uint8

const (
	diffSame diffOp = iota // a line both sides have
	diffDel                // a line only the old side has: releasing removes it
	diffAdd                // a line only the new side has: releasing adds it
	diffFold               // a run of unchanged lines, folded away
)

// diffRow is one row of an edit script. Old and New index the old and new
// lines (-1 where a side has none); a fold carries the number of unchanged
// lines it stands for in Folded instead.
type diffRow struct {
	Op       diffOp
	Old, New int
	Folded   int
}

// diffCellBudget bounds the subsequence table: past it, the lines between the
// common head and tail are reported as removed and added whole, which is still
// a true account of the change, only a coarser one.
const diffCellBudget = 1 << 20

// diffContext is how many unchanged lines stay beside a change when the rest
// of an unchanged run is folded.
const diffContext = 3

// splitLines cuts text into lines. A final newline ends the last line rather
// than opening an empty one, so "a\nb\n" and "a\nb" are both two lines.
func splitLines(text string) []string {
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n")
}

// lineDiff returns the edit script that turns the lines before into the lines
// after: every line either kept, removed or added, in order, keeping as many
// lines as possible. Within a changed stretch the removals come before the
// additions.
func lineDiff(before, after []string) []diffRow {
	head := 0
	for head < len(before) && head < len(after) && before[head] == after[head] {
		head++
	}
	tail := 0
	for tail < len(before)-head && tail < len(after)-head && before[len(before)-1-tail] == after[len(after)-1-tail] {
		tail++
	}
	out := make([]diffRow, 0, len(before)+len(after)-head-tail)
	for i := range head {
		out = append(out, diffRow{Op: diffSame, Old: i, New: i})
	}
	out = append(out, lcsRows(before[head:len(before)-tail], after[head:len(after)-tail], head)...)
	for k := tail; k > 0; k-- {
		out = append(out, diffRow{Op: diffSame, Old: len(before) - k, New: len(after) - k})
	}
	return out
}

// lcsRows diffs the middle stretch, whose first line is line off on both sides.
func lcsRows(a, b []string, off int) []diffRow {
	n, m := len(a), len(b)
	var out []diffRow
	whole := func() []diffRow {
		for i := range n {
			out = append(out, diffRow{Op: diffDel, Old: off + i, New: -1})
		}
		for j := range m {
			out = append(out, diffRow{Op: diffAdd, Old: -1, New: off + j})
		}
		return out
	}
	if n == 0 || m == 0 || n > diffCellBudget/m {
		return whole()
	}
	// lcs[i*(m+1)+j] is the longest common subsequence of a[i:] and b[j:].
	w := m + 1
	lcs := make([]int32, (n+1)*w)
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			switch {
			case a[i] == b[j]:
				lcs[i*w+j] = lcs[(i+1)*w+j+1] + 1
			case lcs[(i+1)*w+j] >= lcs[i*w+j+1]:
				lcs[i*w+j] = lcs[(i+1)*w+j]
			default:
				lcs[i*w+j] = lcs[i*w+j+1]
			}
		}
	}
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			out = append(out, diffRow{Op: diffSame, Old: off + i, New: off + j})
			i, j = i+1, j+1
		case lcs[(i+1)*w+j] >= lcs[i*w+j+1]:
			out = append(out, diffRow{Op: diffDel, Old: off + i, New: -1})
			i++
		default:
			out = append(out, diffRow{Op: diffAdd, Old: -1, New: off + j})
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, diffRow{Op: diffDel, Old: off + i, New: -1})
	}
	for ; j < m; j++ {
		out = append(out, diffRow{Op: diffAdd, Old: -1, New: off + j})
	}
	return out
}

// diffChanged reports whether the script removes or adds any line.
func diffChanged(rows []diffRow) bool {
	for _, r := range rows {
		if r.Op == diffDel || r.Op == diffAdd {
			return true
		}
	}
	return false
}

// foldUnchanged keeps context unchanged lines on each side of every change and
// folds each longer unchanged run into one diffFold row. A fold that would
// stand for a single line keeps the line instead: a marker no shorter than
// what it hides helps nobody.
func foldUnchanged(rows []diffRow, context int) []diffRow {
	out := make([]diffRow, 0, len(rows))
	for start := 0; start < len(rows); {
		if rows[start].Op != diffSame {
			out = append(out, rows[start])
			start++
			continue
		}
		end := start
		for end < len(rows) && rows[end].Op == diffSame {
			end++
		}
		keepHead, keepTail := context, context
		if start == 0 {
			keepHead = 0 // nothing changed above this run
		}
		if end == len(rows) {
			keepTail = 0 // nothing changed below it
		}
		if folded := end - start - keepHead - keepTail; folded > 1 {
			out = append(out, rows[start:start+keepHead]...)
			out = append(out, diffRow{Op: diffFold, Old: -1, New: -1, Folded: folded})
			out = append(out, rows[end-keepTail:end]...)
		} else {
			out = append(out, rows[start:end]...)
		}
		start = end
	}
	return out
}
