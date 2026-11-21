package main

// seam mishaps -- the mishaps agents self-reported at session_end, counted by
// type, with the memories they named, via the console's JSON
// (/console/mishaps?format=json), the way seam sessions reads its list.

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"time"

	"github.com/arctop/seamless/internal/core"
)

var mishapsCmd = spec("mishaps", groupObservability, "list self-reported mishaps, counted by type",
	noArgs(), bindMishaps, runMishaps).
	withLong(`Lists the mishaps agents reported at session_end, newest first, under a
table of how many there are of each type and of the memories the reports named:
a memory many reports name is one that is not preventing its mishap. A report
names its type as a prefix ("shell-trap: ..."); one written before types
existed, or without a prefix, is untyped.

All time unless --days narrows it. --type keeps one type's reports (untyped:
those that named none) and --project one project's (global: the global
scope's); the type table always counts every type in the scope and window.`)

type mishapsOpts struct {
	typ     *string
	project *string
	days    *int
	asJSON  *bool
}

func bindMishaps(fs *flag.FlagSet) *mishapsOpts {
	return &mishapsOpts{
		typ: enumFlag(fs, "type", "", "list only the reports of `TYPE` (untyped: the ones that named none)",
			append(enumOf(core.MishapTypes), core.MishapUntyped)),
		project: fs.String("project", "", "list only the reports from project `SLUG` (global: the global scope)"),
		days:    posIntFlag(fs, "days", 0, "list only the last `N` days (default: all time)"),
		asJSON:  jsonFlag(fs, jsonUsage),
	}
}

// mishapsAnswer is the console's mishap list, as this command reads it.
type mishapsAnswer struct {
	WindowLabel string `json:"windowLabel"`
	Total       int    `json:"total"`
	Shown       int    `json:"shown"`
	Types       []struct {
		Type  string `json:"type"`
		Count int    `json:"count"`
	} `json:"types"`
	Mishaps []struct {
		ID        string    `json:"id"`
		When      time.Time `json:"ts"`
		Type      string    `json:"type"`
		Text      string    `json:"text"`
		Project   string    `json:"project"`
		SessionID string    `json:"sessionId"`
		Session   string    `json:"session"`
	} `json:"mishaps"`
	Memories []struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Missing bool   `json:"missing"`
		Count   int    `json:"count"`
	} `json:"memories"`
	// Locked names the locked projects whose reports the console left out
	// (peer_auth; absent otherwise).
	Locked []string `json:"locked"`
}

// mishapsPath is the console route for these filters.
func mishapsPath(o *mishapsOpts) string {
	v := url.Values{"format": {"json"}}
	if *o.typ != "" {
		v.Set("type", *o.typ)
	}
	if *o.project != "" {
		v.Set("project", *o.project)
	}
	if *o.days > 0 {
		v.Set("days", strconv.Itoa(*o.days))
	}
	return "/console/mishaps?" + v.Encode()
}

func runMishaps(_ context.Context, e *env, o *mishapsOpts, _ []string) error {
	cfg, err := e.loadConfig()
	if err != nil {
		return err
	}
	path := mishapsPath(o)
	if *o.asJSON {
		return printConsoleJSON(e, cfg, path)
	}
	var data mishapsAnswer
	if err := consoleJSON(cfg, path, &data); err != nil {
		return err
	}
	printMishaps(e.stdout, data, *o.typ)
	if line := lockedSkipped(data.Locked, *o.project); line != "" {
		fmt.Fprintln(e.stdout, line)
	}
	return nil
}

// printMishaps renders the answer: the count by type, the memories named,
// then the reports.
func printMishaps(out io.Writer, d mishapsAnswer, typ string) {
	fmt.Fprintf(out, "%s reported (%s)\n", countNoun(d.Total, "mishap", "mishaps"), d.WindowLabel)
	for _, tc := range d.Types {
		fmt.Fprintf(out, "  %-16s %d\n", tc.Type, tc.Count)
	}
	if len(d.Memories) > 0 {
		fmt.Fprintln(out, "\nmemories named:")
		for _, m := range d.Memories {
			name := printable(m.Name)
			if m.Missing {
				name = shortID(m.ID) + " (no longer in the index)"
			}
			fmt.Fprintf(out, "  %-32s %d\n", name, m.Count)
		}
	}
	if typ != "" {
		noun := "reports"
		if d.Shown == 1 {
			noun = "report"
		}
		fmt.Fprintf(out, "\n%d %s %s:\n", d.Shown, typ, noun)
	} else if len(d.Mishaps) > 0 {
		fmt.Fprintln(out)
	}
	for _, m := range d.Mishaps {
		t := m.Type
		if t == "" {
			t = core.MishapUntyped
		}
		project := m.Project
		if project == "" {
			project = "global"
		}
		line := fmt.Sprintf("  %-5s %-16s %-14s %s", agoShort(m.When), t, printable(project), printable(m.Text))
		if m.Session != "" {
			line += "  (" + printable(m.Session) + ")"
		}
		fmt.Fprintln(out, line)
	}
	if d.Shown > len(d.Mishaps) {
		fmt.Fprintf(out, "  ... the newest %d of %d\n", len(d.Mishaps), d.Shown)
	}
}

// countNoun is n with its noun ("1 mishap", "3 mishaps").
func countNoun(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}
