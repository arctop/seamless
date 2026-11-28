package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSitemapCoversEveryPage: the sitemap must name the landing page plus every
// docs page -- and nothing else, so a page dropped from the nav leaves the
// sitemap in the same regeneration that removes its HTML.
func TestSitemapCoversEveryPage(t *testing.T) {
	repoRoot(t)

	site, err := loadSite("docs-src")
	require.NoError(t, err)

	sitemap := string(sitemapXML(site))
	require.Equal(t, len(rootURLs)+len(site.Pages), strings.Count(sitemap, "<loc>"),
		"one <loc> per site-root URL and per docs page")
	require.Contains(t, sitemap, "<loc>"+siteBaseURL+"/</loc>", "the landing page is listed")
	for _, p := range site.Pages {
		require.Contains(t, sitemap, "<loc>"+p.Canonical()+"</loc>", "%s is listed", p.URL)
	}
}

// TestSitemapHasNoDateFields: there is no deterministic date source (see
// determinism_test.go), and a lastmod identical across every URL is what
// crawlers discard as unreliable. Absent beats fake.
func TestSitemapHasNoDateFields(t *testing.T) {
	repoRoot(t)

	site, err := loadSite("docs-src")
	require.NoError(t, err)

	sitemap := string(sitemapXML(site))
	for _, field := range []string{"lastmod", "changefreq", "priority"} {
		require.NotContains(t, sitemap, field)
	}
}

// TestRobotsTxtPointsAtSitemap: the marker line is how anyone can tell whether
// the live host serves this file or Cloudflare's managed one, and the Sitemap
// directive is how crawlers that never see Search Console find the sitemap.
func TestRobotsTxtPointsAtSitemap(t *testing.T) {
	for _, want := range []string{
		"# seamless-robots-v1",
		"User-agent: *",
		"Allow: /",
		"Content-Signal: search=yes, ai-input=yes, ai-train=yes",
		"Sitemap: " + siteBaseURL + "/sitemap.xml",
	} {
		require.Contains(t, robotsTxt, want+"\n")
	}
}

// TestAPICatalogIsWellFormedLinkset: /.well-known/api-catalog is RFC 9727's
// discovery document. It must parse as an RFC 9264 linkset whose hrefs stay on
// the canonical host and point at pages this generator actually publishes --
// an agent that trusts the catalog follows them blind.
func TestAPICatalogIsWellFormedLinkset(t *testing.T) {
	var doc struct {
		Linkset []struct {
			Anchor      string `json:"anchor"`
			ServiceDesc []struct {
				Href string `json:"href"`
				Type string `json:"type"`
			} `json:"service-desc"`
			ServiceDoc []struct {
				Href string `json:"href"`
				Type string `json:"type"`
			} `json:"service-doc"`
		} `json:"linkset"`
	}
	require.NoError(t, json.Unmarshal([]byte(apiCatalog), &doc))
	require.Len(t, doc.Linkset, 1)

	entry := doc.Linkset[0]
	require.Equal(t, siteBaseURL+"/", entry.Anchor)
	require.Len(t, entry.ServiceDesc, 1)
	require.Equal(t, siteBaseURL+"/llms.txt", entry.ServiceDesc[0].Href,
		"service-desc is the llms.txt this generator emits")
	require.Equal(t, "text/plain", entry.ServiceDesc[0].Type)
	require.Len(t, entry.ServiceDoc, 1)
	require.Equal(t, siteBaseURL+"/docs/", entry.ServiceDoc[0].Href,
		"service-doc is the docs site this generator renders")
	require.Equal(t, "text/html", entry.ServiceDoc[0].Type)
}

// TestAuthMdDocumentsTheRealAuthModel: /auth.md is the agent-readable auth
// statement. It must lead with the H1 the scanners key on, stay on the
// canonical host, and keep naming the facts that are true of this codebase:
// the localhost MCP endpoint (the same URL the server card carries), the
// bearer-key method, and the deliberate absence of OAuth discovery metadata.
func TestAuthMdDocumentsTheRealAuthModel(t *testing.T) {
	require.True(t, strings.HasPrefix(authMd, "# auth.md\n"),
		"the H1 is the literal filename -- what agent-readiness checks key on")
	for _, want := range []string{
		"http://127.0.0.1:8081/api/mcp",
		"http://127.0.0.1:8081/api/a2a",
		"Authorization: Bearer",
		siteBaseURL + "/install",
		"/.well-known/openid-configuration",
		"/.well-known/oauth-authorization-server",
		"SEAMLESS_MCP_API_KEY",
	} {
		require.Contains(t, authMd, want)
	}
}

// TestCanonicalHostMatchesCNAME: siteBaseURL is a constant and docs/CNAME is
// what GitHub Pages actually serves; if they diverge, every sitemap URL points
// at a host the site no longer lives on.
func TestCanonicalHostMatchesCNAME(t *testing.T) {
	repoRoot(t)

	raw, err := os.ReadFile(filepath.Join("docs", "CNAME"))
	require.NoError(t, err)
	require.Equal(t, "https://"+strings.TrimSpace(string(raw)), siteBaseURL)
}

// TestLlmsTxtListsEveryPage: llms.txt is the site as a linked outline -- the
// site-root pages, then one H2 per section and one entry per page with its
// canonical URL, led by the home page's description and the intro. A page
// missing here is invisible to the consumers the file exists for.
func TestLlmsTxtListsEveryPage(t *testing.T) {
	repoRoot(t)

	site, err := loadSite("docs-src")
	require.NoError(t, err)

	llms := string(llmsTxt(site))
	require.True(t, strings.HasPrefix(llms, "# "+siteName+"\n\n> "+site.Home.Description+"\n"),
		"llmstxt.org shape: H1 project name, then a one-line blockquote summary")
	listed := 0
	for _, p := range rootPages {
		if p.Title == "" {
			continue
		}
		listed++
		require.NotEmpty(t, p.Note, "%s is listed, so it needs a note", p.URL)
		require.Contains(t, llms, "["+p.Title+"]("+siteBaseURL+p.URL+"): "+p.Note+"\n", "%s is listed", p.URL)
	}
	require.Positive(t, listed, "the site-root pages are listed")
	require.Equal(t, listed+len(site.Pages), strings.Count(llms, "\n- ["),
		"one list entry per listed site-root page and per docs page")
	require.Contains(t, llms, "\n## "+rootSectionTitle+"\n")
	for _, sec := range site.Sections {
		require.Contains(t, llms, "\n## "+sec.Title+"\n")
	}
	for _, p := range site.Pages {
		require.Contains(t, llms, "["+p.Title+"]("+p.Canonical()+"): "+p.Description+"\n", "%s is listed", p.URL)
	}
}

// TestLlmsIntroKeepsTheShape: the intro sits between the summary and the link
// lists, where the llmstxt.org shape allows any markdown but a heading -- an
// H2 there would read as a link section with no links. It also has to answer
// what an assistant is asked: what the tool is and how to install it, with the
// two canonical commands site-check holds every teaching surface to.
func TestLlmsIntroKeepsTheShape(t *testing.T) {
	repoRoot(t)

	intro := strings.TrimSpace(llmsIntro)
	require.NotEmpty(t, intro)
	for _, line := range strings.Split(intro, "\n") {
		require.False(t, strings.HasPrefix(line, "#"), "the intro carries no heading: %q", line)
	}
	require.Contains(t, intro, "curl -fsSL "+siteBaseURL+"/install | sh")
	require.Contains(t, intro, "irm "+siteBaseURL+"/install.ps1 | iex")

	site, err := loadSite("docs-src")
	require.NoError(t, err)
	llms := string(llmsTxt(site))
	at := strings.Index(llms, intro)
	require.Positive(t, at, "the intro is in llms.txt")
	require.Less(t, at, strings.Index(llms, "\n## "), "and comes before the first link list")
}

// TestRootPagesExist: a site-root page the sitemap and llms.txt name must be a
// page the site actually serves, or both point crawlers at a 404.
func TestRootPagesExist(t *testing.T) {
	repoRoot(t)

	require.Equal(t, "/", rootPages[0].URL, "the landing page leads")
	for _, p := range rootPages {
		require.FileExists(t, filepath.Join("docs", filepath.FromSlash(p.URL), "index.html"), p.URL)
	}
}

// TestLlmsFullTxtIsUntruncated: llms-full.txt must carry each page's composed
// source markdown (authored plus generator output), not the plainText form the
// search index uses, which is capped at 2000 runes -- a silently truncated page
// would be worse than none.
func TestLlmsFullTxtIsUntruncated(t *testing.T) {
	repoRoot(t)

	site := loadRepoSite(t)
	require.NoError(t, renderPages(site))

	full := string(llmsFullTxt(site))
	for _, p := range site.Pages {
		require.Contains(t, full, "\n# "+p.Title+"\n\nURL: "+p.Canonical()+"\n", "%s has a header", p.URL)
		if p.Src != "" {
			require.NotEmpty(t, p.FullMarkdown, "%s carries its composed markdown", p.URL)
		}
		require.Contains(t, full, strings.TrimRight(textifyEmbeddedHTML(p.FullMarkdown), "\n"),
			"%s content is present whole", p.URL)
	}

	// This file exists for text consumers: authored figures and card grids
	// must arrive as their flattened text form, never as markup.
	require.NotContains(t, full, `<figure class="doc-figure"`)
	require.NotContains(t, full, `class="flow-node"`)
	require.NotContains(t, full, `class="card-grid"`)
	require.NotContains(t, full, `class="doc-card"`)

	// The generated references are the longest pages; if the untruncated
	// composed markdown ever regresses to the 2000-rune plainText, this fails.
	for _, p := range site.Pages {
		if p.Generate != "" {
			require.Greater(t, len(p.FullMarkdown), 2000,
				"%s: generated pages exceed the plainText cap, proving FullMarkdown is not truncated", p.URL)
		}
	}
}

// TestWriteSiteRootNeverDeletes: the site root holds the hand-written landing
// page, CNAME, and fonts. writeSiteRoot must only add its own files, never
// clean the directory the way writeSite does.
func TestWriteSiteRootNeverDeletes(t *testing.T) {
	dir := t.TempDir()
	landing := filepath.Join(dir, "index.html")
	cname := filepath.Join(dir, "CNAME")
	require.NoError(t, os.WriteFile(landing, []byte("<h1>hand-written landing page</h1>"), 0o644))
	require.NoError(t, os.WriteFile(cname, []byte("thereisnospoon.org"), 0o644))

	home := &Page{Description: "a test site"}
	reg := &registryMeta{Name: "x/y", Version: "1.0.0", Description: "a test card"}
	card, err := serverCard(reg)
	require.NoError(t, err)
	acard, err := agentCard(reg)
	require.NoError(t, err)
	site := &Site{Home: home, Pages: []*Page{home, {URL: "quickstart/"}}, ServerCard: card, AgentCard: acard}
	require.NoError(t, writeSiteRoot(dir, site))

	require.FileExists(t, landing, "the landing page survives")
	require.FileExists(t, cname, "CNAME survives")
	for name := range siteRootFiles(site) {
		require.FileExists(t, filepath.Join(dir, name))
	}
}

// TestMakefileGatesEverySiteRootFile: docs-check diffs only the files
// SITE_FILES names, so a site-root file the Makefile does not list is
// published ungated -- exactly the gap that once left index.md and
// api-catalog undiffed while SITE.md claimed otherwise. Set equality both
// ways: everything emitted is gated, everything gated is emitted.
func TestMakefileGatesEverySiteRootFile(t *testing.T) {
	repoRoot(t)

	site := loadRepoSite(t)
	var emitted []string
	for name := range siteRootFiles(site) {
		emitted = append(emitted, name)
	}
	require.ElementsMatch(t, emitted, makefileSiteFiles(t))
}

// makefileSiteFiles parses the SITE_FILES assignment out of the Makefile,
// following backslash continuations.
func makefileSiteFiles(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile("Makefile")
	require.NoError(t, err)

	lines := strings.Split(string(raw), "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, "SITE_FILES") {
			continue
		}
		_, value, ok := strings.Cut(line, ":=")
		require.True(t, ok, "SITE_FILES must be a := assignment")
		var files []string
		for {
			value = strings.TrimSpace(value)
			continued := strings.HasSuffix(value, `\`)
			files = append(files, strings.Fields(strings.TrimSuffix(value, `\`))...)
			if !continued {
				return files
			}
			i++
			require.Less(t, i, len(lines), "SITE_FILES continuation runs off the Makefile")
			value = lines[i]
		}
	}
	t.Fatal("Makefile has no SITE_FILES assignment")
	return nil
}

// TestPageCanonical: canonical URLs are absolute, directory-style, and rooted
// under the published /docs/ prefix.
func TestPageCanonical(t *testing.T) {
	tests := []struct{ url, want string }{
		{"", "https://thereisnospoon.org/docs/"},
		{"quickstart/", "https://thereisnospoon.org/docs/quickstart/"},
		{"reference/mcp/tasks/", "https://thereisnospoon.org/docs/reference/mcp/tasks/"},
	}
	for _, tt := range tests {
		p := &Page{URL: tt.url}
		require.Equal(t, tt.want, p.Canonical())
	}
}
