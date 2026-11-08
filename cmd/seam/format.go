package main

// Terminal formatting shared by every seam subcommand.

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
)

func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func pctOf(n, d float64) int {
	if d == 0 {
		return 0
	}
	return int(n/d*100 + 0.5)
}

// agoShort renders a compact age for the CLI (mirrors the console's ago helper).
func agoShort(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// fieldWidth aligns a record's labelled lines (memory read, note read): the
// longest label, "description:", plus one space.
const fieldWidth = 13

// printField prints one labelled line of a record, or nothing when the value is
// empty: an absent tag list or model is not worth a line reading "-".
func printField(w io.Writer, label, value string) {
	if value == "" {
		return
	}
	fmt.Fprintf(w, "%-*s%s\n", fieldWidth, label+":", value)
}

// writtenBy labels an item a restricted local account (peer_auth.restricted)
// wrote: "written by account <name>", the wording every surface uses. The
// owner's items carry no account, and get no label -- "" here, which
// printField skips -- so their rendering is what it always was.
func writtenBy(account string) string {
	if account == "" {
		return ""
	}
	return "written by account " + printable(account)
}

// lockedSkipped is the line a console-backed list adds for the locked projects
// (peer_auth.locked) its answer left out: "(not shown: locked project
// acme-finance)". It names them and never counts what they hold -- a count
// would say what a locked project holds -- and it is "" when the answer left
// nothing out, so a list on an install without locked projects reads exactly
// as before. only, when set, is the list's own --project filter: a locked
// project the filter would have dropped anyway is not named.
func lockedSkipped(locked []string, only string) string {
	kept := lockedFor(locked, only)
	names := make([]string, 0, len(kept))
	for _, p := range kept {
		names = append(names, printable(p))
	}
	switch len(names) {
	case 0:
		return ""
	case 1:
		return "(not shown: locked project " + names[0] + ")"
	}
	return "(not shown: locked projects " + strings.Join(names, ", ") + ")"
}

// lockedFor is the locked projects a list's --project filter leaves in: all
// of them without a filter, the filter's own project when it is locked, none
// otherwise.
func lockedFor(locked []string, only string) []string {
	if only == "" {
		return locked
	}
	if slices.Contains(locked, only) {
		return []string{only}
	}
	return nil
}

// printBody prints a record's markdown body under a blank line, as plan show
// prints a plan's, ending in exactly one newline whatever the file ended in.
func printBody(w io.Writer, body string) {
	if strings.TrimSpace(body) == "" {
		return
	}
	fmt.Fprintf(w, "\n%s\n", strings.TrimRight(body, "\n"))
}

// strs reads a decoded JSON array of strings, skipping anything else.
func strs(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, x := range list {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
