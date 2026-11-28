package main

// siteBaseURL is the canonical origin of the published site. A constant rather
// than a flag: docs/docs/ is committed and docs-check diffs it, so a flag would
// create two byte-different render paths and phantom drift, and the tests call
// loadSite/renderPages/writeSite directly with no flags. This does not touch the
// base-URL-free href invariant (Root/DocsRoot stay relative); only metadata that
// is absolute by definition -- sitemap locations, canonical URLs -- uses it.
const siteBaseURL = "https://thereisnospoon.org"

// docsPathPrefix is where the generated docs tree is served, relative to the
// site root (GitHub Pages serves docs/ verbatim, so docs/docs/ is /docs/).
const docsPathPrefix = "/docs/"

// Canonical returns the page's absolute canonical URL on the published site.
func (p *Page) Canonical() string { return siteBaseURL + docsPathPrefix + p.URL }

// ogImagePath is the one shared social card, served from the site root. No
// per-page images: one static og.png on every page beats 38 nearly identical
// files nothing regenerates.
const ogImagePath = "/static/og.png"

// ogImageAlt describes the card for screen readers and unfurlers; it mirrors
// the headline rendered inside the image (static/og-source.html).
const ogImageAlt = "Seamless - every agent you run should know what the others learned."

// HeadTitle is the single source for every place a page's full title appears
// (<title>, og:title, a future JSON-LD headline), so they cannot drift. It was
// previously inline template logic in layout.html.
func (p *Page) HeadTitle() string {
	if p.IsHome() {
		return "Seamless documentation"
	}
	return p.Title + " - Seamless docs"
}

// OGType marks the docs home as the site-shaped entry point and every content
// page as an article, which is how Open Graph consumers expect docs to unfurl.
func (p *Page) OGType() string {
	if p.IsHome() {
		return "website"
	}
	return "article"
}

// OGImage is the absolute URL of the shared social card.
func (p *Page) OGImage() string { return siteBaseURL + ogImagePath }
