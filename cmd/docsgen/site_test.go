package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// writeSrc materializes a docs-src fixture: paths are slash-separated and
// relative to the returned temp dir.
func writeSrc(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		full := filepath.Join(dir, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(body), 0o644))
	}
	return dir
}

func page(title, body string) string {
	return "---\ntitle: " + title + "\n---\n\n" + body + "\n"
}

func TestPageURL(t *testing.T) {
	cases := []struct{ src, want string }{
		{"index.md", ""},
		{"quickstart.md", "quickstart/"},
		{"concepts/memory.md", "concepts/memory/"},
		{"reference/mcp/index.md", "reference/mcp/"},
		{"reference/mcp/tasks.md", "reference/mcp/tasks/"},
	}
	for _, tc := range cases {
		got, err := pageURL(tc.src)
		require.NoError(t, err, tc.src)
		require.Equal(t, tc.want, got, tc.src)
	}

	_, err := pageURL("notes.txt")
	require.Error(t, err, "non-markdown sources are rejected")
}

// TestRepoDocumentationContracts pins the hand-authored statements most likely
// to drift behind fast-moving code. Generated MCP/config material already reads
// its contracts from Go; these assertions cover narrative promises that cannot.
func TestRepoDocumentationContracts(t *testing.T) {
	repoRoot(t)

	read := func(path string) string {
		t.Helper()
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		return string(raw)
	}

	cli := read("docs-src/reference/cli-seam.md")
	require.Contains(t, cli, "Bare `seam task` is not an alias")
	require.NotContains(t, cli, "Bare `seam task` with no subcommand is the same as")

	console := read("docs-src/reference/console.md")
	for _, phrase := range []string{"fresh page opens with the five newest interactions", "Older history is explicit and additive", "Agent-reported mishaps"} {
		require.Contains(t, console, phrase)
	}

	gardener := read("docs-src/concepts/gardener.md")
	require.Contains(t, gardener, "**tool-error**")

	var figures, captions int
	labelRE := regexp.MustCompile(`aria-labelledby="([^"]+)"`)
	require.NoError(t, filepath.WalkDir("docs-src", func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".md" {
			return err
		}
		body := read(path)
		require.NotContains(t, body, "┌", "%s: use an explanatory figure, not a box drawing", path)
		require.NotContains(t, body, "│", "%s: use an explanatory figure, not a box drawing", path)
		require.NotContains(t, body, "└", "%s: use an explanatory figure, not a box drawing", path)
		require.NotContains(t, body, `class="step"`,
			"%s: .step is the landing page's numbered-chip class (site.css); figures use flow-step", path)
		figures += strings.Count(body, `<figure class="doc-figure"`)
		captions += strings.Count(body, "<figcaption")
		for _, block := range strings.Split(body, `<figure class="doc-figure"`)[1:] {
			figure := strings.SplitN(block, "</figure>", 2)[0]
			label := labelRE.FindStringSubmatch(figure)
			require.Len(t, label, 2, "%s: every explanatory figure names its caption", path)
			require.Contains(t, figure, `<figcaption id="`+label[1]+`">`,
				"%s: aria-labelledby must resolve to the figure caption", path)
		}
		return nil
	}))
	require.GreaterOrEqual(t, figures, 15)
	require.Equal(t, figures, captions, "every explanatory figure has one caption")

	for _, path := range []string{"README.md", "docs-src/index.md", "docs-src/install.md", "docs/index.html", "docs/compare/index.html", "docs/claude-app/index.html", "server.json"} {
		body := strings.ToLower(read(path))
		for _, stale := range []string{"one go binary", "single go binary", "delete the database and lose nothing"} {
			require.NotContains(t, body, stale, "%s: stale deployment/storage claim", path)
		}
	}
}

// TestLoadSiteDerivesNav pins the derived state every template depends on: the
// relative href prefixes and the prev/next chain, including a generated section
// index taking its place in the walk.
func TestLoadSiteDerivesNav(t *testing.T) {
	dir := writeSrc(t, map[string]string{
		"nav.yaml": `
sections:
  - title: Getting started
    slug: ""
    pages: [index.md, quickstart.md]
  - title: Reference
    slug: reference
    description: The surface.
    pages: [mcp/index.md, mcp/tasks.md]
`,
		"index.md":               page("Home", "hi"),
		"quickstart.md":          page("Quickstart", "go"),
		"reference/mcp/index.md": page("MCP", "overview"),
		"reference/mcp/tasks.md": page("Tasks", "tasks"),
	})

	site, err := loadSite(dir)
	require.NoError(t, err)

	// Nav order: home, quickstart, [generated /reference/], mcp, tasks.
	var urls []string
	for _, p := range site.Pages {
		urls = append(urls, p.URL)
	}
	require.Equal(t, []string{"", "quickstart/", "reference/", "reference/mcp/", "reference/mcp/tasks/"}, urls)

	require.Equal(t, "home", site.Home.Template)
	require.Equal(t, "section", site.Sections[1].Index.Template,
		"a section with no authored index.md gets a generated card grid")
	require.Equal(t, "The surface.", site.Sections[1].Index.Description)

	// Root/DocsRoot are what every href in every template is built from, so a
	// depth-by-depth check is the cheapest guard against a site that only works
	// at one nesting level.
	byURL := map[string]*Page{}
	for _, p := range site.Pages {
		byURL[p.URL] = p
	}
	require.Equal(t, "", byURL[""].DocsRoot)
	require.Equal(t, "../", byURL[""].Root)
	require.Equal(t, "../", byURL["quickstart/"].DocsRoot)
	require.Equal(t, "../../", byURL["quickstart/"].Root)
	require.Equal(t, "../../", byURL["reference/mcp/"].DocsRoot)
	require.Equal(t, "../../../", byURL["reference/mcp/"].Root)
	require.Equal(t, "../../../", byURL["reference/mcp/tasks/"].DocsRoot)
	require.Equal(t, "../../../../", byURL["reference/mcp/tasks/"].Root)

	require.Equal(t, "index.html", byURL[""].Out)
	require.Equal(t, "reference/mcp/tasks/index.html", byURL["reference/mcp/tasks/"].Out)

	// prev/next walk the flattened nav order and terminate at both ends.
	require.Nil(t, site.Pages[0].Prev)
	require.Equal(t, "quickstart/", site.Pages[0].Next.URL)
	require.Equal(t, "reference/mcp/", site.Pages[4].Prev.URL)
	require.Nil(t, site.Pages[4].Next)
}

// TestLoadSiteValidation covers the drift the manifest exists to prevent. Each
// case is a mistake that would otherwise ship silently.
func TestLoadSiteValidation(t *testing.T) {
	const minimalNav = `
sections:
  - title: Getting started
    slug: ""
    pages: [index.md]
`
	cases := []struct {
		name  string
		files map[string]string
		msg   string
	}{
		{
			name: "unlisted file",
			files: map[string]string{
				"nav.yaml":  minimalNav,
				"index.md":  page("Home", "hi"),
				"orphan.md": page("Orphan", "invisible"),
			},
			msg: "does not list: orphan.md",
		},
		{
			name: "dangling nav entry",
			files: map[string]string{
				"nav.yaml": "sections:\n  - title: A\n    slug: \"\"\n    pages:\n      - index.md\n      - ghost.md\n",
				"index.md": page("Home", "hi"),
			},
			msg: "ghost.md",
		},
		{
			name: "duplicate page",
			files: map[string]string{
				"nav.yaml": "sections:\n  - title: A\n    slug: \"\"\n    pages: [index.md, index.md]\n",
				"index.md": page("Home", "hi"),
			},
			msg: "listed twice",
		},
		{
			name: "missing frontmatter",
			files: map[string]string{
				"nav.yaml": minimalNav,
				"index.md": "no frontmatter here\n",
			},
			msg: "missing frontmatter",
		},
		{
			name: "no title",
			files: map[string]string{
				"nav.yaml": minimalNav,
				"index.md": "---\ndescription: untitled\n---\n\nbody\n",
			},
			msg: "title is required",
		},
		{
			name: "no home page",
			files: map[string]string{
				"nav.yaml":        "sections:\n  - title: A\n    slug: guides\n    pages: [start.md]\n",
				"guides/start.md": page("Start", "hi"),
			},
			msg: "no page resolves to the docs root",
		},
		{
			// Two sources claiming one URL: reference/mcp.md and
			// reference/mcp/index.md both want /reference/mcp/.
			name: "colliding urls",
			files: map[string]string{
				"nav.yaml":         "sections:\n  - title: A\n    slug: \"\"\n    pages: [index.md, ref/mcp.md, ref/mcp/index.md]\n",
				"index.md":         page("Home", "hi"),
				"ref/mcp.md":       page("MCP", "a"),
				"ref/mcp/index.md": page("MCP again", "b"),
			},
			msg: "both resolve to",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadSite(writeSrc(t, tc.files))
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.msg)
		})
	}
}

// TestLoadSiteIgnoresPartials: generator includes live under _tools/ and must not
// trip the every-file-is-in-the-nav rule.
func TestLoadSiteIgnoresPartials(t *testing.T) {
	dir := writeSrc(t, map[string]string{
		"nav.yaml":                      "sections:\n  - title: A\n    slug: \"\"\n    pages: [index.md]\n",
		"index.md":                      page("Home", "hi"),
		"reference/_tools/tasks_add.md": "### Example\n\nnot a page\n",
	})
	site, err := loadSite(dir)
	require.NoError(t, err)
	require.Len(t, site.Pages, 1)
}

// TestSectionIndexesKeepCardGrid: a section landing must always carry the card
// grid of its children. A generated index renders the grid as its whole body;
// an authored index goes through the "page" template, which appends the same
// grid via IsSectionIndex -- without that guard, authoring orientation prose
// would silently cost the section its navigation. Ordinary pages must never
// grow the grid.
func TestSectionIndexesKeepCardGrid(t *testing.T) {
	repoRoot(t)

	files := renderRepoSite(t)
	site, err := loadSite("docs-src")
	require.NoError(t, err)

	for _, sec := range site.Sections {
		if sec.Slug == "" {
			continue
		}
		require.NotNil(t, sec.Index, "%s has a landing page", sec.Slug)
		require.True(t, sec.Index.IsSectionIndex())
		body, ok := files[sec.Index.Out]
		require.True(t, ok, "%s landing was rendered", sec.Slug)
		require.Contains(t, body, `class="card-grid"`, "%s landing keeps the card grid", sec.Slug)
		for _, child := range sec.Pages {
			require.Contains(t, body, `href="`+sec.Index.DocsRoot+child.URL+`"`,
				"%s landing links its child %s", sec.Slug, child.URL)
		}
	}

	for _, p := range site.Pages {
		if p.IsSectionIndex() || p.IsHome() {
			continue
		}
		require.NotContains(t, files[p.Out], `class="card-grid"`,
			"%s is not a section landing and must not render the grid", p.URL)
	}
}
