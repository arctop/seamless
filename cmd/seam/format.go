package main

// Terminal formatting shared by every seam subcommand.

import (
	"fmt"
	"io"
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
