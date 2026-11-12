package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The console reference claims its writes table is the whole list of POST
// routes Register mounts. These name both halves of that claim.
const (
	consoleRoutesFile    = "internal/console/console.go"
	consoleRoutesFunc    = "Register"
	consoleWritesPage    = "docs-src/reference/console.md"
	consoleWritesHeading = "## What the console can change"
)

// routeWildcard matches a ServeMux wildcard ({id}, {name...}) but not the
// end-of-path anchor {$}, so a renamed path parameter is not drift.
var routeWildcard = regexp.MustCompile(`\{[A-Za-z_][A-Za-z0-9_]*(\.\.\.)?\}`)

// normalizeRoute collapses whitespace and wildcard names, so the code and the
// table compare by shape: "POST /console/memories/{id}/archive" and
// "POST  /console/memories/{memory}/archive" are the same route.
func normalizeRoute(s string) string {
	return routeWildcard.ReplaceAllString(strings.Join(strings.Fields(s), " "), "{$1}")
}

// registeredConsolePOSTs parses path and returns every "POST /console..."
// pattern passed as the first argument of a call inside the function named
// fn: the post and handle helpers Register mounts its routes through.
func registeredConsolePOSTs(t *testing.T, path, fn string) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	require.NoError(t, err)
	var routes []string
	found := false
	for _, decl := range file.Decls {
		f, ok := decl.(*ast.FuncDecl)
		if !ok || f.Name.Name != fn || f.Body == nil {
			continue
		}
		found = true
		ast.Inspect(f.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			pattern, err := strconv.Unquote(lit.Value)
			require.NoError(t, err)
			if strings.HasPrefix(pattern, "POST /console") {
				routes = append(routes, normalizeRoute(pattern))
			}
			return true
		})
	}
	require.True(t, found, "%s declares no func %s: the console's routes moved, so point this test at them", path, fn)
	slices.Sort(routes)
	return slices.Compact(routes)
}

// tableCells splits one markdown table row on its unescaped pipes, as GFM
// does, and drops the empty cells outside the leading and trailing pipes.
func tableCells(row string) []string {
	var cells []string
	var b strings.Builder
	for i := 0; i < len(row); i++ {
		switch {
		case row[i] == '\\' && i+1 < len(row) && row[i+1] == '|':
			b.WriteString(`\|`)
			i++
		case row[i] == '|':
			cells = append(cells, strings.TrimSpace(b.String()))
			b.Reset()
		default:
			b.WriteByte(row[i])
		}
	}
	cells = append(cells, strings.TrimSpace(b.String()))
	if len(cells) > 0 && cells[0] == "" {
		cells = cells[1:]
	}
	if len(cells) > 0 && cells[len(cells)-1] == "" {
		cells = cells[:len(cells)-1]
	}
	return cells
}

// documentedConsolePOSTs reads the Route column of every table row in the
// section under heading, up to the next h2, and returns each backticked
// "POST /console..." route it names.
func documentedConsolePOSTs(t *testing.T, path, heading string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	lines := strings.Split(string(raw), "\n")
	start := slices.IndexFunc(lines, func(l string) bool { return strings.TrimSpace(l) == heading })
	require.GreaterOrEqual(t, start, 0, "%s has no %q section", path, heading)
	codeSpan := regexp.MustCompile("`(POST /console[^`]*)`")
	var routes []string
	rows := 0
	for _, line := range lines[start+1:] {
		if strings.HasPrefix(line, "## ") {
			break
		}
		if !strings.HasPrefix(strings.TrimSpace(line), "|") {
			continue
		}
		cells := tableCells(strings.TrimSpace(line))
		if len(cells) < 2 {
			continue
		}
		for _, m := range codeSpan.FindAllStringSubmatch(cells[1], -1) {
			routes = append(routes, normalizeRoute(m[1]))
		}
		rows++
	}
	require.NotZero(t, rows, "the %q section of %s holds no table", heading, path)
	slices.Sort(routes)
	return slices.Compact(routes)
}

// TestConsoleWritesTable_ListsEveryPOSTRoute holds the console reference to
// its claim: the writes table lists every POST route console.go registers, and
// no route console.go does not. A new route needs a row there, describing what
// its handler does; a removed one loses its row.
func TestConsoleWritesTable_ListsEveryPOSTRoute(t *testing.T) {
	repoRoot(t)
	code := registeredConsolePOSTs(t, consoleRoutesFile, consoleRoutesFunc)
	require.Contains(t, code, "POST /console/login", "the parse must see routes mounted through handle as well as post")
	require.Contains(t, code, "POST /console/memories/{}/archive")
	doc := documentedConsolePOSTs(t, consoleWritesPage, consoleWritesHeading)

	var missing, stale []string
	for _, r := range code {
		if !slices.Contains(doc, r) {
			missing = append(missing, r)
		}
	}
	for _, r := range doc {
		if !slices.Contains(code, r) {
			stale = append(stale, r)
		}
	}
	require.Empty(t, missing, "%s registers POST routes the table under %q in %s does not list; add a row for each, from its handler",
		consoleRoutesFile, consoleWritesHeading, consoleWritesPage)
	require.Empty(t, stale, "the table under %q in %s lists POST routes %s does not register; drop or correct those rows",
		consoleWritesHeading, consoleWritesPage, consoleRoutesFile)
}

// TestNormalizeRoute pins what the comparison above treats as the same route.
func TestNormalizeRoute(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"POST /console/memories/{id}/archive", "POST /console/memories/{}/archive"},
		{"POST  /console/gardener/plan/{slug}/apply ", "POST /console/gardener/plan/{}/apply"},
		{"GET /console/labs/{name...}", "GET /console/labs/{...}"},
		{"GET /console/{$}", "GET /console/{$}"},
	} {
		require.Equal(t, tc.want, normalizeRoute(tc.in), tc.in)
	}
	require.Equal(t, []string{"Action", "Route", "Effect"}, tableCells("| Action | Route | Effect |"))
	require.Equal(t, []string{"a", `b \| c`, "d"}, tableCells(`| a | b \| c | d |`))
}
