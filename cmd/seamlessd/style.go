package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/arctop/seamless/internal/termvt"
)

// colorEnabled reports whether ANSI styling should be emitted on stdout: it is a
// terminal that shows escape sequences as styling (termvt.Enable turns that on
// for a Windows console, whose default prints them as text) and NO_COLOR is
// unset (https://no-color.org -- presence disables, regardless of value).
// Computed once so styling is a cheap string wrap, and so a piped install
// (fixture harness, CI) stays plain text with no escape codes.
var colorEnabled = func() bool {
	if _, noColor := os.LookupEnv("NO_COLOR"); noColor {
		return false
	}
	return term.IsTerminal(int(os.Stdout.Fd())) && termvt.Enable(os.Stdout)
}()

// ANSI SGR codes for the small palette the installer uses.
const (
	ansiReset  = "\033[0m"
	ansiBold   = "\033[1m"
	ansiDim    = "\033[2m"
	ansiGreen  = "\033[32m"
	ansiYellow = "\033[33m"
)

// style wraps s in an SGR code when color is enabled, and is a no-op otherwise
// so callers never branch on colorEnabled themselves.
func style(code, s string) string {
	if !colorEnabled || s == "" {
		return s
	}
	return code + s + ansiReset
}

func bold(s string) string   { return style(ansiBold, s) }
func dim(s string) string    { return style(ansiDim, s) }
func green(s string) string  { return style(ansiGreen, s) }
func yellow(s string) string { return style(ansiYellow, s) }

// fieldLabelWidth is the padded width of a fieldRow label. It fits the widest
// label the installer prints ("version") so every block's values line up.
const fieldLabelWidth = 7

// fieldRow prints one aligned "  label  value" row under a block header. The
// label is padded to fieldLabelWidth so values line up across blocks.
func fieldRow(name, value string) {
	fieldRowTo(os.Stdout, name, value)
}

// fieldRowTo is fieldRow to an explicit writer, for output a test captures.
func fieldRowTo(w io.Writer, name, value string) {
	fmt.Fprintf(w, "  %s  %s\n", dim(fmt.Sprintf("%-*s", fieldLabelWidth, name)), value)
}

// fieldCont is the width of the fieldRow prefix (2 indent + label + 2 gap), so a
// continuation line's text aligns under the value column above it.
const fieldCont = "  " + "       " + "  " // 2 + fieldLabelWidth(7) + 2 = 11 spaces

// tildePath abbreviates a home-relative path with "~" for terse, portable
// output (~/.claude/settings.json rather than the full /Users/... path). A path
// outside home, or an undiscoverable home, is returned unchanged.
func tildePath(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if p == home {
		return "~"
	}
	if strings.HasPrefix(p, home+string(os.PathSeparator)) {
		return "~" + p[len(home):]
	}
	return p
}
