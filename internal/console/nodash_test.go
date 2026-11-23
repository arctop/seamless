package console

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/features"
)

// consoleDash finds a dash the console must not show: a spaced double hyphen,
// an em or en dash, or their entities and escapes. The owner's call
// (2026-11-22): no " -- ", and no em or en dash in its place either; copy uses
// a colon, a semicolon, a comma, parentheses or a new sentence, a missing
// value reads "none" or "n/a", and a range a plain hyphen or "to".
var consoleDash = regexp.MustCompile(`\s--\s|\x{2014}|\x{2013}|&mdash;|&ndash;|\\u2014|\\u2013`)

// noDashComments blanks what never reaches the screen: template, HTML, CSS
// and JS comments. Line structure is kept, so a hit names its line.
func noDashComments(s string) string {
	blank := func(m string) string {
		return strings.Map(func(r rune) rune {
			if r == '\n' {
				return r
			}
			return ' '
		}, m)
	}
	for _, re := range []*regexp.Regexp{
		regexp.MustCompile(`(?s)\{\{-?\s*/\*.*?\*/\s*-?\}\}`),
		regexp.MustCompile(`(?s)<!--.*?-->`),
		regexp.MustCompile(`(?s)/\*.*?\*/`),
		regexp.MustCompile(`(?m)^\s*//.*$`),
		regexp.MustCompile(`(?m)([;{}(),]\s*)//.*$`),
	} {
		s = re.ReplaceAllStringFunc(s, blank)
	}
	return s
}

func noDashHits(t *testing.T, name, src string) []string {
	t.Helper()
	var hits []string
	for i, line := range strings.Split(noDashComments(src), "\n") {
		if consoleDash.MatchString(line) {
			hits = append(hits, name+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
		}
	}
	return hits
}

// The console's templates and scripts show no double hyphen, em dash or en
// dash (comments aside).
func TestConsoleCopy_NoDashesInTemplatesOrScripts(t *testing.T) {
	var hits []string
	require.NoError(t, fs.WalkDir(templateFS, "templates", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := templateFS.ReadFile(path)
		require.NoError(t, err)
		hits = append(hits, noDashHits(t, path, string(b))...)
		return nil
	}))
	scripts, err := filepath.Glob("static/*.js")
	require.NoError(t, err)
	require.NotEmpty(t, scripts)
	for _, path := range scripts {
		b, err := os.ReadFile(path)
		require.NoError(t, err)
		hits = append(hits, noDashHits(t, path, string(b))...)
	}
	require.Empty(t, hits, "the console shows no \" -- \", em dash or en dash: reword with a colon, semicolon, comma or parentheses")
}

// The console's Go strings show none either. The only literals allowed to
// hold " -- " are the ones that read it out of stored text: the search key a
// title's " -- " is replaced at, and the gardener's patterns over the reasons
// its passes wrote.
func TestConsoleCopy_NoDashesInGoStrings(t *testing.T) {
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	fset := token.NewFileSet()
	var hits []string
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		require.NoError(t, err)
		ast.Inspect(f, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "MustCompile" {
					return false // a pattern over stored text, not copy
				}
			}
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			v, err := strconv.Unquote(lit.Value)
			if err != nil || v == " -- " {
				return true
			}
			if consoleDash.MatchString(v) || consoleDash.MatchString(lit.Value) {
				hits = append(hits, fset.Position(lit.Pos()).String()+": "+lit.Value)
			}
			return true
		})
	}
	require.Empty(t, hits, "the console shows no \" -- \", em dash or en dash")
}

// The feature blurbs and lines Settings > Features shows hold none either.
func TestConsoleCopy_NoDashesInFeatureCopy(t *testing.T) {
	for _, f := range features.Registry() {
		for _, s := range append([]string{f.Label, f.Blurb, f.Line}, f.Surfaces...) {
			require.False(t, consoleDash.MatchString(s), "feature %s: %q", f.Key, s)
		}
	}
}
