package main

import (
	_ "embed"
	"maps"
	"path/filepath"
	"strings"
)

// rootURLs are the site-root pages docsgen does not render but the sitemap must
// still name -- the hand-written landing page, the hand-written comparison hub,
// the hand-written Claude app page (Seamless for chat work that is not code),
// and the hand-written security page. Deliberately a constant rather than a
// directory scan, which would sweep in install, install.ps1, and static assets.
var rootURLs = []string{"/", "/compare/", "/claude-app/", "/security/"}

// robotsTxt is written verbatim to the site root. The marker line is
// load-bearing: the live host sits behind Cloudflare, which can serve its own
// managed robots.txt, and grepping for the marker is the only way to tell from
// outside whose file is being served. Content-Signal ai-input=yes opts into
// inference-time retrieval (being fetched and cited by AI answers).
const robotsTxt = `# seamless-robots-v1
User-agent: *
Allow: /
Content-Signal: search=yes, ai-input=yes, ai-train=yes

Sitemap: ` + siteBaseURL + `/sitemap.xml
`

// apiCatalog is /.well-known/api-catalog (RFC 9727): an RFC 9264 linkset
// naming the site's machine-readable entry points for agents that discover
// APIs by well-known URI. One entry, anchored at the site itself: Seamless has
// no public API endpoint (the MCP server binds to each install's localhost),
// so the catalog advertises the agent-readable description and the docs, not
// an endpoint that does not exist. GitHub Pages serves extensionless files as
// application/octet-stream; the required application/linkset+json content type
// is set by a Cloudflare response-header rule on the zone, not from this repo.
const apiCatalog = `{
  "linkset": [
    {
      "anchor": "` + siteBaseURL + `/",
      "service-desc": [
        { "href": "` + siteBaseURL + `/llms.txt", "type": "text/plain" }
      ],
      "service-doc": [
        { "href": "` + siteBaseURL + `/docs/", "type": "text/html" }
      ]
    }
  ]
}
`

// authMd is /auth.md: the agent-readable statement of how authentication
// works. The honest content is mostly that it does not: the site has no
// protected APIs and no authorization server, so there is deliberately no
// OAuth/OIDC discovery metadata to publish -- fabricated endpoints would send
// agents into flows that 404. What an agent can actually use is documented
// instead: the MCP endpoint is per-install on localhost, and the credential is
// the static bearer key the installer provisions. Embedded from the sibling
// file rather than declared as a const because the markdown needs backticks,
// which a raw string literal cannot carry.
//
//go:embed auth.md
var authMd string

// xmlEscaper covers the characters XML 1.0 cannot carry raw in text content.
// Page URLs are slug-derived and never contain them today; escaping anyway
// keeps a future odd filename from producing a silently invalid sitemap.
var xmlEscaper = strings.NewReplacer(
	"&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;",
)

// sitemapLocs is every published URL: the site-root pages first, then the
// generated scenario pages, then the docs pages in nav order (deterministic by
// construction).
func sitemapLocs(site *Site) []string {
	locs := make([]string, 0, len(rootURLs)+len(site.Scenarios)+len(site.Pages))
	for _, u := range rootURLs {
		locs = append(locs, siteBaseURL+u)
	}
	for _, s := range site.Scenarios {
		locs = append(locs, s.Canonical())
	}
	for _, p := range site.Pages {
		locs = append(locs, p.Canonical())
	}
	return locs
}

// sitemapXML renders the sitemap. No lastmod, changefreq, or priority: the
// render has no deterministic date source (see determinism_test.go), an
// identical lastmod across every URL is what crawlers discard as unreliable,
// and at this page count the other two fields change nothing.
func sitemapXML(site *Site) []byte {
	var b strings.Builder
	b.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n")
	b.WriteString("<urlset xmlns=\"http://www.sitemaps.org/schemas/sitemap/0.9\">\n")
	for _, loc := range sitemapLocs(site) {
		b.WriteString("  <url><loc>")
		b.WriteString(xmlEscaper.Replace(loc))
		b.WriteString("</loc></url>\n")
	}
	b.WriteString("</urlset>\n")
	return []byte(b.String())
}

// siteName is the project name llms.txt leads with -- the llms.txt convention
// wants the project, not the home page's question-shaped title.
const siteName = "Seamless"

// llmsTxt renders /llms.txt in the llmstxt.org shape: an H1, a one-line
// blockquote summary, then one H2 per nav section with `- [title](url): note`
// entries. It is built from the same Site data the card grids render, so it
// has no source of truth of its own to drift.
func llmsTxt(site *Site) []byte {
	var b strings.Builder
	b.WriteString("# " + siteName + "\n\n")
	b.WriteString("> " + site.Home.Description + "\n")
	for _, sec := range site.Sections {
		b.WriteString("\n## " + sec.Title + "\n\n")
		for _, p := range sectionPages(sec) {
			b.WriteString("- [" + p.Title + "](" + p.Canonical() + "): " + p.Description + "\n")
		}
	}
	if len(site.Scenarios) > 0 {
		b.WriteString("\n## Scenarios\n\n")
		for _, s := range site.Scenarios {
			b.WriteString("- [" + s.Title + "](" + s.Canonical() + "): " + s.Description + "\n")
		}
	}
	return []byte(b.String())
}

// llmsFullTxt renders /llms-full.txt: every page's full source markdown in nav
// order, each under its title with its canonical URL, so an LLM that fetches
// one file gets the whole site untruncated. Authored raw HTML flattens
// (textifyEmbeddedHTML: figures to fenced text, card grids to link lists) --
// this file exists for text consumers, who should never see the markup.
func llmsFullTxt(site *Site) []byte {
	var b strings.Builder
	b.WriteString("# " + siteName + "\n\n")
	b.WriteString("> " + site.Home.Description + "\n")
	for _, p := range site.Pages {
		b.WriteString("\n---\n\n")
		b.WriteString("# " + p.Title + "\n\n")
		b.WriteString("URL: " + p.Canonical() + "\n\n")
		b.WriteString(strings.TrimRight(textifyEmbeddedHTML(p.FullMarkdown), "\n") + "\n")
	}
	return []byte(b.String())
}

// sectionPages is a section's pages in nav order, its index page (authored or
// generated) first -- the same order site.Pages carries them.
func sectionPages(sec *Section) []*Page {
	if sec.Index == nil {
		return sec.Pages
	}
	return append([]*Page{sec.Index}, sec.Pages...)
}

// writeSiteRoot writes the crawler files into the site root (docs/, the
// directory GitHub Pages serves). Unlike writeSite it owns only the files it
// names and never deletes anything: the site root also holds the hand-written
// landing page, CNAME, .nojekyll, and fonts, none of which are docsgen's to
// touch.
func writeSiteRoot(siteDir string, site *Site) error {
	for name, content := range siteRootFiles(site) {
		if err := writeFile(filepath.Join(siteDir, name), content); err != nil {
			return err
		}
	}
	return nil
}

// siteRootFiles is the complete set writeSiteRoot owns, by name. index.md is
// the landing page's markdown twin -- the llms.txt outline under the name the
// `Accept: text/markdown` edge rewrite expects (see twin.go): every negotiable
// directory URL, `/` included, maps uniformly to its sibling index.md, which
// GitHub Pages serves as text/markdown natively.
func siteRootFiles(site *Site) map[string][]byte {
	files := map[string][]byte{
		"sitemap.xml":             sitemapXML(site),
		"robots.txt":              []byte(robotsTxt),
		"llms.txt":                llmsTxt(site),
		"llms-full.txt":           llmsFullTxt(site),
		"index.md":                llmsTxt(site),
		"auth.md":                 []byte(authMd),
		".well-known/api-catalog": []byte(apiCatalog),
	}
	// The cards are attached by run() (and loadRepoSite in tests); a Site built
	// without them omits the files rather than publishing empty cards. The real
	// render can not silently lose them: docs-check diffs the committed cards
	// against a fresh render by name (SITE_FILES in the Makefile).
	if len(site.ServerCard) > 0 {
		files[serverCardPath] = site.ServerCard
	}
	if len(site.AgentCard) > 0 {
		files[agentCardPath] = site.AgentCard
	}
	maps.Copy(files, site.AgentSkills)
	return files
}
