package main

// seam plan -- owner surface over captured Claude Code plans, backed by the
// console JSON endpoints (list/show/approve) plus a local git staleness check.

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"
)

// cliPlanRow mirrors the console's planRow JSON.
type cliPlanRow struct {
	NoteID     string    `json:"noteId"`
	Slug       string    `json:"slug"`
	Basename   string    `json:"basename"`
	Title      string    `json:"title"`
	Project    string    `json:"project"`
	Status     string    `json:"status"`
	Iteration  int       `json:"iteration"`
	Agents     int       `json:"agents"`
	TasksDone  int       `json:"tasksDone"`
	TasksTotal int       `json:"tasksTotal"`
	Updated    time.Time `json:"updated"`
}

// cliPlanDetail mirrors the console's planDetailData JSON.
type cliPlanDetail struct {
	Plan     cliPlanRow `json:"plan"`
	Body     string     `json:"body"`
	Attached []struct {
		ID      string    `json:"id"`
		Title   string    `json:"title"`
		Slug    string    `json:"slug"`
		IsAgent bool      `json:"isAgent"`
		Updated time.Time `json:"updated"`
	} `json:"attached"`
	Tasks []struct {
		ID     string `json:"id"`
		Title  string `json:"title"`
		Status string `json:"status"`
	} `json:"tasks"`
	CanApprove bool `json:"canApprove"`
}

// --- plan list ---

var planListCmd = spec("plan list", groupPlans, "list captured plans with status/iteration",
	// `seam plan list <slug>` listed EVERY plan: fs.NArg() was never checked, so the
	// slug went on the floor and the caller read a full listing as a filtered one.
	// The hint names the command that does take a slug, which no arity arithmetic
	// could guess.
	noArgs().withHint("to show one plan, use: seam plan show <slug>"),
	bindPlanList, runPlanList)

// planWindows is the selector set store.ResolveRetrievalWindow accepts
// (internal/store/retrieval_report.go).
//
// It is transcribed rather than imported because seam must not import
// internal/store: that pulls the ~30 modernc.org/sqlite packages into a binary
// that currently has none of them (doctor.go documents the same reasoning for not
// importing internal/mcp). A package-local slice of accepted values at the
// surface is house style here, not a compromise; console/search.go likewise
// owns its distinct, strict window enum at its HTTP boundary.
var planWindows = []string{"24h", "7d", "30d", "all"}

type planListOpts struct {
	project *string
	window  *string
	asJSON  *bool
}

func bindPlanList(fs *flag.FlagSet) *planListOpts {
	return &planListOpts{
		project: fs.String("project", "", "filter by project `SLUG`"),
		// --window bogus reaches ResolveRetrievalWindow's `default:` and comes back as
		// "24h". Nothing in the output says which window it answered, so the caller
		// reads a one-day list as the 30d one they asked for.
		window: enumFlag(fs, "window", "all", "time `WINDOW`", planWindows),
		asJSON: jsonFlag(fs, jsonUsage+"; not with --project, which filters client-side"),
	}
}

func runPlanList(_ context.Context, e *env, o *planListOpts, _ []string) error {
	if *o.asJSON && *o.project != "" {
		// --project is applied HERE, to rows the route returned for every
		// project, and --json prints the route's answer as-is. Honoring both
		// would mean re-marshalling the answer around a filtered row list whose
		// counts no longer add up, so the pair is refused rather than one of them
		// silently dropped -- with the filter that does both spelled out.
		return fmt.Errorf("--json prints the console's answer as-is and --project filters it client-side; "+
			"filter the JSON instead: seam plan list --json | jq '.rows[] | select(.project == %q)'", *o.project)
	}
	cfg, err := e.loadConfig()
	if err != nil {
		return err
	}
	path := "/console/plans?format=json&w=" + url.QueryEscape(*o.window)
	if *o.asJSON {
		return printConsoleJSON(e, cfg, path)
	}
	var data struct {
		Rows []cliPlanRow `json:"rows"`
		// Locked names the locked projects whose plans the console left out
		// (peer_auth.locked; absent otherwise).
		Locked []string `json:"locked"`
	}
	if err := consoleJSON(cfg, path, &data); err != nil {
		return err
	}
	shown := 0
	for _, r := range data.Rows {
		if *o.project != "" && r.Project != *o.project {
			continue
		}
		fmt.Fprintf(e.stdout, "  %-24s [%-9s] %s  (%s, iter %d, %d agents, tasks %d/%d, %s)\n",
			r.Slug, r.Status, r.Title, orDash(r.Project), r.Iteration, r.Agents,
			r.TasksDone, r.TasksTotal, agoShort(r.Updated))
		shown++
	}
	if shown == 0 {
		fmt.Fprintln(e.stdout, "(no captured plans)")
	}
	if line := lockedSkipped(data.Locked, *o.project); line != "" {
		fmt.Fprintln(e.stdout, line)
	}
	return nil
}

// --- plan show ---

// A plan in a locked project (peer_auth.locked) the calling tree holds no
// lease on is refused with the lock named -- the console answers the lock
// refusal the tool-backed commands print (answerLocked) -- never "not found";
// plan check and plan approve read the same route family and say the same.
var planShowCmd = spec("plan show", groupPlans, "one plan: body, attached notes, tasks",
	exactly(1, "slug"), bindPlanShow, runPlanShow)

type planShowOpts struct {
	asJSON *bool
}

func bindPlanShow(fs *flag.FlagSet) *planShowOpts {
	return &planShowOpts{asJSON: jsonFlag(fs, jsonUsage)}
}

func runPlanShow(_ context.Context, e *env, o *planShowOpts, pos []string) error {
	cfg, err := e.loadConfig()
	if err != nil {
		return err
	}
	path := "/console/plans/" + url.PathEscape(pos[0]) + "?format=json"
	if *o.asJSON {
		return printConsoleJSON(e, cfg, path)
	}
	var d cliPlanDetail
	if err := consoleJSON(cfg, path, &d); err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "%s  [%s]  %s\n", d.Plan.Slug, d.Plan.Status, orDash(d.Plan.Project))
	fmt.Fprintf(e.stdout, "title:    %s\n", d.Plan.Title)
	fmt.Fprintf(e.stdout, "file:     %s.md (iteration %d)\n", d.Plan.Basename, d.Plan.Iteration)
	if len(d.Attached) > 0 {
		fmt.Fprintf(e.stdout, "attached: %d note(s)\n", len(d.Attached))
		for _, a := range d.Attached {
			kind := "note"
			if a.IsAgent {
				kind = "agent"
			}
			fmt.Fprintf(e.stdout, "  [%-5s] %s  %s (%s)\n", kind, shortID(a.ID), a.Title, agoShort(a.Updated))
		}
	}
	if len(d.Tasks) > 0 {
		fmt.Fprintf(e.stdout, "tasks:    %d\n", len(d.Tasks))
		for _, tk := range d.Tasks {
			fmt.Fprintf(e.stdout, "  %s  [%-11s] %s\n", shortID(tk.ID), tk.Status, tk.Title)
		}
	}
	if strings.TrimSpace(d.Body) != "" {
		fmt.Fprintf(e.stdout, "\n%s\n", d.Body)
	}
	return nil
}

// --- plan approve ---

var planApproveCmd = spec("plan approve", groupPlans,
	"escape hatch: flip to approved + create the task",
	exactly(1, "slug"), bindNoOpts, runPlanApprove)

func runPlanApprove(_ context.Context, e *env, _ *noOpts, pos []string) error {
	cfg, err := e.loadConfig()
	if err != nil {
		return err
	}
	var out struct {
		Slug        string `json:"slug"`
		Status      string `json:"status"`
		TaskCreated bool   `json:"taskCreated"`
		TaskID      string `json:"taskId"`
	}
	if err := consolePOST(cfg, "/console/plans/"+url.PathEscape(pos[0])+"/approve?format=json", &out); err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "plan %s -> %s\n", out.Slug, out.Status)
	if out.TaskCreated {
		fmt.Fprintf(e.stdout, "created task %s\n", shortID(out.TaskID))
	} else {
		fmt.Fprintln(e.stdout, "(tracking task already exists)")
	}
	return nil
}

// --- plan check ---

var planCheckCmd = spec("plan check", groupPlans,
	"FRESH/STALE per note vs the repo's git history",
	exactly(1, "slug"), bindPlanCheck, runPlanCheck)

type planCheckOpts struct {
	cwd *string
}

func bindPlanCheck(fs *flag.FlagSet) *planCheckOpts {
	return &planCheckOpts{
		cwd: fs.String("cwd", "", "repo `DIR` to check against (default: current)"),
	}
}

// runPlanCheck compares each captured note's git stamp against the current
// HEAD of --cwd: a note is STALE when files it mentions changed since its
// stamped commit, FRESH otherwise, UNKNOWN when the stamp or commit cannot be
// resolved. It reads note bodies via the console and runs git locally.
func runPlanCheck(_ context.Context, e *env, o *planCheckOpts, pos []string) error {
	slug := pos[0]
	cwd := *o.cwd
	if cwd == "" {
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		cwd = wd
	}
	cfg, err := e.loadConfig()
	if err != nil {
		return err
	}
	var d cliPlanDetail
	if err := consoleJSON(cfg, "/console/plans/"+url.PathEscape(slug)+"?format=json", &d); err != nil {
		return err
	}

	head, err := gitOut(cwd, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("%s is not a git repo (or git failed): %w", cwd, err)
	}
	fmt.Fprintf(e.stdout, "plan %s vs %s @ %.12s\n", slug, cwd, head)

	type entry struct{ label, body string }
	entries := []entry{{"plan " + d.Plan.Basename + ".md", d.Body}}
	for _, a := range d.Attached {
		var n struct {
			Body string `json:"body"`
		}
		if err := consoleJSON(cfg, "/console/notes/"+url.PathEscape(a.ID)+"?format=json", &n); err != nil {
			fmt.Fprintf(e.stdout, "  [UNKNOWN] %-40s note unreadable: %v\n", a.Slug, err)
			continue
		}
		entries = append(entries, entry{a.Slug, n.Body})
	}

	stale := 0
	for _, en := range entries {
		verdict, detail := checkEntry(cwd, head, en.body)
		if verdict == "STALE" {
			stale++
		}
		fmt.Fprintf(e.stdout, "  [%-7s] %-40s %s\n", verdict, en.label, detail)
	}
	if stale > 0 {
		return fmt.Errorf("%d note(s) stale -- re-verify before trusting them", stale)
	}
	return nil
}

// checkEntry resolves one note body to a verdict against the repo head.
func checkEntry(cwd, head, body string) (verdict, detail string) {
	stamp := stampHead(body)
	switch stamp {
	case "":
		return "UNKNOWN", "no git stamp"
	case "unknown":
		return "UNKNOWN", "captured outside a git repo"
	}
	if strings.HasPrefix(head, stamp) {
		return "FRESH", "stamped at current HEAD"
	}
	diff, err := gitOut(cwd, "diff", "--name-only", stamp+".."+head)
	if err != nil {
		return "UNKNOWN", fmt.Sprintf("commit %s not found (rebased away?)", stamp)
	}
	changed := strings.Fields(diff)
	if len(changed) == 0 {
		return "FRESH", "no changes since " + stamp
	}
	touched := overlap(changed, mentionedPaths(body))
	if len(touched) == 0 {
		return "FRESH", fmt.Sprintf("%d file(s) changed since %s, none mentioned here", len(changed), stamp)
	}
	if len(touched) > 5 {
		touched = append(touched[:5], "...")
	}
	return "STALE", "mentioned files changed: " + strings.Join(touched, ", ")
}

// stampHead extracts the short git head from a capture stamp line
// ("> captured from ... | git <head> | ..."), or "". A local transcription of
// plans.StampHead -- seam must not link internal/store, which internal/plans
// imports (see the planWindows comment above for the same reasoning).
func stampHead(body string) string {
	for line := range strings.Lines(body) {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "> captured from") {
			continue
		}
		for part := range strings.SplitSeq(line, "|") {
			part = strings.TrimSpace(part)
			if after, ok := strings.CutPrefix(part, "git "); ok {
				return strings.TrimSpace(after)
			}
		}
	}
	return ""
}

// pathToken matches file-path-looking tokens in prose: either containing a
// directory separator, or a bare filename with a code-ish extension.
var pathToken = regexp.MustCompile(`[A-Za-z0-9_~][A-Za-z0-9_./~-]*/[A-Za-z0-9_.-]+\.[A-Za-z0-9]{1,8}|\b[A-Za-z0-9_-]+\.(?:go|md|ts|tsx|js|jsx|py|rs|swift|kt|java|c|h|cpp|cc|yaml|yml|json|sql|html|css|sh|proto)\b`)

// mentionedPaths extracts the distinct path-like tokens from a note body,
// stripping trailing :line references and punctuation.
func mentionedPaths(body string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, m := range pathToken.FindAllString(body, -1) {
		p := strings.TrimRight(m, ".,;:)`'\"")
		if _, dup := seen[p]; dup || p == "" {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}

// overlap returns the changed paths that a note mentions. Mentions may be
// absolute or repo-relative, so paths match when one ends with the other
// (component-aligned via the / boundary).
func overlap(changed, mentioned []string) []string {
	hit := map[string]struct{}{}
	for _, c := range changed {
		for _, m := range mentioned {
			if pathSuffixMatch(c, m) {
				hit[c] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(hit))
	for c := range hit {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// pathSuffixMatch reports whether a and b name the same file allowing one to
// carry a longer prefix (repo-relative vs absolute vs bare filename).
func pathSuffixMatch(a, b string) bool {
	if a == b {
		return true
	}
	return strings.HasSuffix(a, "/"+strings.TrimPrefix(b, "/")) ||
		strings.HasSuffix(b, "/"+strings.TrimPrefix(a, "/"))
}

// gitOut runs a git command in dir and returns its trimmed stdout.
func gitOut(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
