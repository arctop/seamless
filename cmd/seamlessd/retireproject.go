package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

var (
	// errProjectNotFound is a --project slug with no registered project.
	errProjectNotFound = errors.New("no such project")
	// errProjectInUse is a retire refused because something still lives in or
	// routes to the project. The blockers themselves are printed, not wrapped.
	errProjectInUse = errors.New("project still in use; nothing changed")
)

// runRetireProject marks a project retired -- the sanctioned cleanup for one
// minted by mistake (a moved-and-renamed repo, or a `<slug>-2` duplicate from
// before moved repos were adopted). It is the flag a split stamps on its emptied
// source: the row stays, the console lists it as retired, and --undo clears it.
// Like map-repo it writes straight to the database, so no running daemon is
// needed.
func runRetireProject(args []string) error {
	fs := flag.NewFlagSet("retire-project", flag.ContinueOnError)
	project := fs.String("project", "", "slug of the project to retire")
	undo := fs.Bool("undo", false, "clear the retired flag instead")
	dryRun := fs.Bool("dry-run", false, "print what would happen without changing anything")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("seamlessd.retire-project: unexpected argument %q (use --project)", fs.Arg(0))
	}
	slug := strings.TrimSpace(*project)
	if slug == "" {
		return fmt.Errorf("seamlessd.retire-project: --project is required")
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("seamlessd.retire-project: %w", err)
	}
	db, err := store.Open(cfg.DBPath())
	if err != nil {
		return fmt.Errorf("seamlessd.retire-project: %w", err)
	}
	defer func() { _ = db.Close() }()

	if err := retireProject(context.Background(), db, os.Stdout, slug, *undo, *dryRun, time.Now()); err != nil {
		return fmt.Errorf("seamlessd.retire-project: %w", err)
	}
	return nil
}

// retireProject applies (or with dryRun, reports) one retire or un-retire.
// Both directions are idempotent: a project already in the requested state is a
// printed no-op, not an error.
//
// Retiring refuses while the project is still in use, because a flag moves
// nothing: a repo mapping keeps routing new sessions into the project, and its
// active memories, notes and open tasks would sit where no briefing reaches.
// Every blocker is printed, each with its remedy, before the refusal. Un-retiring
// has no guard -- it only makes the project ordinary again.
func retireProject(ctx context.Context, db *sql.DB, w io.Writer, slug string, undo, dryRun bool, now time.Time) error {
	p, ok, err := store.ProjectBySlug(ctx, db, slug)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: %q", errProjectNotFound, slug)
	}

	if undo {
		if !p.Retired() {
			fmt.Fprintf(w, "project %q is not retired\n", slug)
			return nil
		}
		if dryRun {
			fmt.Fprintf(w, "would un-retire project %q\n", slug)
			return nil
		}
		if err := store.RetireProject(ctx, db, slug, time.Time{}); err != nil {
			return err
		}
		fmt.Fprintf(w, "un-retired project %q\n", slug)
		return nil
	}

	if p.Retired() {
		fmt.Fprintf(w, "project %q is already retired (since %s)\n", slug, core.FormatTime(*p.RetiredAt))
		return nil
	}
	quote := shellQuoter(runtime.GOOS)
	blockers, err := retireBlockers(ctx, db, slug, quote)
	if err != nil {
		return err
	}
	if len(blockers) > 0 {
		fmt.Fprintf(w, "project %q cannot be retired yet:\n", slug)
		for _, b := range blockers {
			fmt.Fprintf(w, "  - %s\n", b)
		}
		return errProjectInUse
	}
	if dryRun {
		fmt.Fprintf(w, "would retire project %q\n", slug)
		return nil
	}
	if err := store.RetireProject(ctx, db, slug, now); err != nil {
		return err
	}
	fmt.Fprintf(w, "retired project %q (undo: seamlessd retire-project --project %s --undo)\n", slug, quote(slug))
	return nil
}

// retireBlockers lists what keeps slug from being retired, one line per blocker
// naming its remedy; none means it is safe. Every mapping gets the exact command
// that clears it, each argument passed through quote so the line can be pasted
// as printed. Another host's row lives in this database, so it is dropped from
// here with unmap-repo --host; that host's own unmap-repo edits its own database
// and could never reach it.
func retireBlockers(ctx context.Context, db *sql.DB, slug string, quote func(string) string) ([]string, error) {
	local, err := store.LocalRepoMappings(ctx, db)
	if err != nil {
		return nil, err
	}
	type hostPath struct{ host, path string }
	isLocal := map[hostPath]bool{}
	for _, r := range local {
		isLocal[hostPath{r.Host, r.Path}] = true
	}
	all, err := store.RepoMapRows(ctx, db)
	if err != nil {
		return nil, err
	}

	var out []string
	for _, r := range all {
		if r.Slug != slug {
			continue
		}
		if isLocal[hostPath{r.Host, r.Path}] {
			out = append(out, fmt.Sprintf(
				"%s still maps here: move it with `seamlessd map-repo --path %s --project <slug>`, or drop it with `seamlessd unmap-repo --path %s`",
				r.Path, quote(r.Path), quote(r.Path)))
			continue
		}
		out = append(out, fmt.Sprintf(
			"%s on host %q still maps here: drop it with `seamlessd unmap-repo --host %s --path %s`",
			r.Path, r.Host, quote(r.Host), quote(r.Path)))
	}

	c, err := store.GetProjectCounts(ctx, db, slug)
	if err != nil {
		return nil, err
	}
	var held []string
	if c.Memories > 0 {
		held = append(held, countOf(c.Memories, "active memory", "active memories"))
	}
	if c.Notes > 0 {
		held = append(held, countOf(c.Notes, "note", "notes"))
	}
	if c.OpenTasks > 0 {
		held = append(held, countOf(c.OpenTasks, "open task", "open tasks"))
	}
	if len(held) > 0 {
		out = append(out, fmt.Sprintf(
			"it still holds %s: move them to the project they belong in, or archive/close them -- retiring moves nothing",
			strings.Join(held, ", ")))
	}
	return out, nil
}

func countOf(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// shellQuoter returns how a printed command quotes an argument for the shell an
// owner on goos pastes it into. retire-project opens this machine's database,
// so it runs where the daemon does and goos is runtime.GOOS: a remedy can name
// another machine's path, but it is run here.
//
//   - Everywhere but Windows, POSIX single quotes (posixShellArg), which sh,
//     bash and zsh all read literally -- so a Windows client's "C:\repos\app"
//     keeps its backslashes and a path with a space stays one argument.
//   - On Windows, PowerShell single quotes (powerShellArg). PowerShell is the
//     default shell there and the one the Windows installer runs in, and its
//     single-quoted string is its only literal one: a double-quoted string
//     expands "$name" and backtick escapes, which a path can carry
//     ("\\srv\c$\repos", any POSIX client's path). cmd.exe shares no quoting
//     with PowerShell, so a quoted word is PowerShell-only.
//
// Either way a word made only of characters every such shell, cmd.exe
// included, reads literally stays bare, so a plain path prints as it is.
func shellQuoter(goos string) func(string) string {
	if goos == "windows" {
		return powerShellArg
	}
	return posixShellArg
}

// posixShellArg quotes s for a POSIX shell: bare when it is all letters, digits
// and punctuation no shell reads specially anywhere in a word, else in single
// quotes, each embedded quote closing them, escaped, and reopening them. "=" and
// "~" are not bare: zsh expands a word starting with "=", and a leading "~" is a
// home directory.
func posixShellArg(s string) string {
	if isBareWord(s, "_@%+:,./-") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// powerShellQuotes escapes the inside of a PowerShell single-quoted string.
// PowerShell reads the typographic single quotes U+2018-U+201B as "'" too, and a
// doubled quote of any of them is that one character.
var powerShellQuotes = strings.NewReplacer(
	"'", "''",
	"\u2018", "\u2018\u2018", "\u2019", "\u2019\u2019",
	"\u201a", "\u201a\u201a", "\u201b", "\u201b\u201b",
)

// powerShellArg quotes s for PowerShell: bare when it is all letters, digits and
// punctuation PowerShell and cmd.exe both read literally, else in single quotes.
func powerShellArg(s string) string {
	if isBareWord(s, `\/.:_-`) {
		return s
	}
	return "'" + powerShellQuotes.Replace(s) + "'"
}

// isBareWord reports whether s is non-empty and made only of ASCII letters,
// digits and bytes in punct.
func isBareWord(s, punct string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte(punct, c) >= 0:
		default:
			return false
		}
	}
	return true
}
