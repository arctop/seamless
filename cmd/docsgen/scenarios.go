// The /scenarios/ pages: one crawlable, answer-first page per landing-page
// terminal scene. The transcripts come from scenes.js (loadScenes) -- never
// re-typed -- and the framing prose (the answer-first opener, the memory file
// that made the difference, how to reproduce) is authored per page in
// docs-src/_scenarios/<slug>.md. The underscore prefix keeps those files out of
// the docs nav (they are not docs pages; they publish at the site root), and
// this generator is what turns the pair into docs/scenarios/<slug>/index.html.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// scenariosDirName is both the docs-src subdirectory holding the authored
// framing and the site-root output directory (docs/scenarios/).
const scenariosDirName = "scenarios"

// transcriptMarker splits a framing file's body: markdown above it renders
// before the transcripts (the answer-first opener), markdown below it renders
// after them (the memory file, how to reproduce).
const transcriptMarker = "<!-- transcript -->"

// scenarioRoot is a scenario page's relative prefix to the site root: pages
// publish at /scenarios/<slug>/, two segments deep. Framing markdown writes
// site-root-absolute links (/docs/concepts/memory/, /compare/) and the
// markdown renderer rewrites them against this prefix, mirroring how docs
// pages stay base-URL-free.
const scenarioRoot = "../../"

// Scenario is one published /scenarios/<slug>/ page.
type Scenario struct {
	Slug        string
	Title       string
	Description string
	SceneID     string
	Order       int
	Scene       *Scene

	// Intro and Tail are the authored framing halves around the transcripts.
	Intro, Tail         string
	IntroHTML, TailHTML template.HTML
	// TranscriptHTML is the scene's full static transcript markup, rendered
	// from scenes.js by renderScene.
	TranscriptHTML template.HTML
	// Links are the site-root-absolute paths the framing referenced;
	// checkScenarioLinks resolves them.
	Links []string
}

// scenarioMeta is a framing file's YAML frontmatter.
type scenarioMeta struct {
	Title       string `yaml:"title"`
	Description string `yaml:"description"`
	// Scene is the scenes.js id this page renders (page slugs and scene ids
	// differ: /scenarios/task-collision/ renders scene "coordination").
	Scene string `yaml:"scene"`
	// Order fixes the pages' sitemap/llms.txt/cross-link order; the landing
	// page's scene order, not the filesystem's.
	Order int `yaml:"order"`
}

// URL is the page's path relative to the site root.
func (s *Scenario) URL() string { return scenariosDirName + "/" + s.Slug + "/" }

// Canonical is the page's absolute URL on the published site.
func (s *Scenario) Canonical() string { return siteBaseURL + "/" + s.URL() }

// HeadTitle names the page in <title> and og:title. These are not docs pages,
// so no "- Seamless docs" suffix.
func (s *Scenario) HeadTitle() string { return s.Title + " - Seamless" }

// OGImage is the shared social card, same as every other page.
func (s *Scenario) OGImage() string { return siteBaseURL + ogImagePath }

// JSONLD returns the page's structured data: a BreadcrumbList from the site
// root and a TechArticle tied to the landing page's WebSite node -- the same
// shape and the same whole-element injection rule as Page.JSONLD (html/template
// treats ld+json as JS context, so the payload must arrive pre-built).
func (s *Scenario) JSONLD() (template.HTML, error) {
	doc := ldGraph{
		Context: "https://schema.org",
		Graph: []any{
			ldBreadcrumbList{Type: "BreadcrumbList", Items: []ldListItem{
				{Type: "ListItem", Position: 1, Name: "Seamless", Item: siteBaseURL + "/"},
				{Type: "ListItem", Position: 2, Name: s.Title, Item: s.Canonical()},
			}},
			ldTechArticle{
				Type:        "TechArticle",
				Headline:    s.HeadTitle(),
				Description: s.Description,
				URL:         s.Canonical(),
				InLanguage:  "en",
				IsPartOf:    ldRef{ID: websiteID},
			},
		},
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("%s: marshal JSON-LD: %w", s.URL(), err)
	}
	return template.HTML(`<script type="application/ld+json">` + string(raw) + `</script>`), nil //nolint:gosec // marshalled from static structs, no user input
}

// loadScenarios reads every framing file, binds each to its scene, and demands
// the two stay in lockstep: a framing file naming a scene that does not exist,
// or a scene no framing file covers, is a build error. The second direction is
// the one that rots silently -- a fifth scene added to scenes.js would publish
// on the landing page while /scenarios/ quietly stayed at four.
func loadScenarios(srcDir string, scenes []*Scene) ([]*Scenario, error) {
	dir := filepath.Join(srcDir, "_"+scenariosDirName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}

	byID := make(map[string]*Scene, len(scenes))
	for _, sc := range scenes {
		byID[sc.ID] = sc
	}
	covered := make(map[string]string) // scene id -> framing slug

	var out []*Scenario
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		src := "_" + scenariosDirName + "/" + e.Name()
		yamlSrc, body, err := splitFrontmatterRaw(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", src, err)
		}
		var meta scenarioMeta
		if err := yaml.Unmarshal(yamlSrc, &meta); err != nil {
			return nil, fmt.Errorf("%s: frontmatter: %w", src, err)
		}
		if meta.Title == "" || meta.Description == "" || meta.Scene == "" || meta.Order == 0 {
			return nil, fmt.Errorf("%s: frontmatter needs title, description, scene, and order", src)
		}
		scene, ok := byID[meta.Scene]
		if !ok {
			return nil, fmt.Errorf("%s: scene %q is not in %s", src, meta.Scene, scenesPath)
		}
		if prior, dup := covered[meta.Scene]; dup {
			return nil, fmt.Errorf("%s and %s both render scene %q", prior, src, meta.Scene)
		}
		covered[meta.Scene] = src

		intro, tail, found := strings.Cut(body, transcriptMarker)
		if !found {
			return nil, fmt.Errorf("%s: missing the %s marker separating the opener from the closing sections", src, transcriptMarker)
		}
		out = append(out, &Scenario{
			Slug:        strings.TrimSuffix(e.Name(), ".md"),
			Title:       meta.Title,
			Description: meta.Description,
			SceneID:     meta.Scene,
			Order:       meta.Order,
			Scene:       scene,
			Intro:       intro,
			Tail:        tail,
		})
	}

	var orphans []string
	for _, sc := range scenes {
		if _, ok := covered[sc.ID]; !ok {
			orphans = append(orphans, sc.ID)
		}
	}
	if len(orphans) > 0 {
		sort.Strings(orphans)
		return nil, fmt.Errorf("%s has scenes with no %s framing file: %s", scenesPath, dir, strings.Join(orphans, ", "))
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Order < out[j].Order })
	for i := 1; i < len(out); i++ {
		if out[i].Order == out[i-1].Order {
			return nil, fmt.Errorf("%s and %s share order %d", out[i-1].Slug, out[i].Slug, out[i].Order)
		}
	}
	return out, nil
}

// renderScenarios fills in each scenario's HTML, then resolves the framing's
// internal links against the site -- same rot-prevention as checkLinks, over
// the site-root URL space the scenario pages link from.
func renderScenarios(site *Site) error {
	for _, s := range site.Scenarios {
		intro, err := renderMarkdown(s.Intro, scenarioRoot, scenarioRoot)
		if err != nil {
			return fmt.Errorf("scenario %s: %w", s.Slug, err)
		}
		tail, err := renderMarkdown(s.Tail, scenarioRoot, scenarioRoot)
		if err != nil {
			return fmt.Errorf("scenario %s: %w", s.Slug, err)
		}
		s.IntroHTML, s.TailHTML = intro.HTML, tail.HTML
		s.Links = append(append([]string{}, intro.Links...), tail.Links...)
		s.TranscriptHTML = renderScene(s.Scene)
	}
	return checkScenarioLinks(site)
}

// checkScenarioLinks resolves the framing files' site-root-absolute links: the
// docs pages (under /docs/), the other scenario pages, and the hand-written
// root surfaces are linkable; anything else is a build error.
func checkScenarioLinks(site *Site) error {
	targets := map[string]bool{"/": true, "/compare/": true, "/security/": true}
	for _, p := range site.Pages {
		targets[docsPathPrefix+p.URL] = true
	}
	for _, s := range site.Scenarios {
		targets["/"+s.URL()] = true
	}
	var broken []string
	for _, s := range site.Scenarios {
		for _, link := range s.Links {
			target, _, _ := strings.Cut(link, "#")
			if target == "" || targets[target] || strings.HasPrefix(target, "/static/") {
				continue
			}
			broken = append(broken, fmt.Sprintf("%s -> %s", s.Slug, link))
		}
	}
	if len(broken) > 0 {
		return fmt.Errorf("scenario links to pages that do not exist:\n  %s", strings.Join(broken, "\n  "))
	}
	return nil
}

/* ---- static transcript rendering ----------------------------------------

The markup below mirrors what scenes-player.js builds in the DOM, class for
class, so the published pages inherit the landing page's terminal styling from
site.css and a transcript reads identically animated or static. The `show`
class every step carries is what the player adds after its reveal animation;
emitting it directly is the static equivalent. The .term-scene-term class is
deliberately absent -- it scopes the player's reveal-from-opacity-0 and the
fixed-height scrolling pane, neither of which a static page wants. */

// sceneEsc matches the player's esc(): the entities site-check normalizes over
// are never introduced here, keeping outcome strings byte-comparable.
func sceneEsc(s string) string { return html.EscapeString(s) }

var (
	sceneCodeRe   = regexp.MustCompile("`([^`]+)`")
	sceneStrongRe = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	sceneEmRe     = regexp.MustCompile(`\*([^*]+)\*`)
	sceneHeadRe   = regexp.MustCompile(`^(#{1,4})\s+(.*)$`)
	sceneOlRe     = regexp.MustCompile(`^\s*\d+\.\s+(.*)$`)
	sceneUlRe     = regexp.MustCompile(`^\s*[-*]\s+(.*)$`)
)

// sceneInline is the player's inline markdown-lite: code, strong, em.
func sceneInline(s string) string {
	out := sceneEsc(s)
	out = sceneCodeRe.ReplaceAllString(out, "<code>$1</code>")
	out = sceneStrongRe.ReplaceAllString(out, "<strong>$1</strong>")
	return sceneEmRe.ReplaceAllString(out, "<em>$1</em>")
}

// sceneMarkdown is the player's block markdown-lite for agent prose: headings
// flatten to h4, blockquotes, ordered/unordered lists, paragraphs. Full
// goldmark is wrong here on purpose -- the player renders transcripts this way,
// and the static page must show the same text the animation shows.
func sceneMarkdown(text string) string {
	var out strings.Builder
	list := ""
	closeList := func() {
		if list != "" {
			out.WriteString("</" + list + ">")
			list = ""
		}
	}
	for line := range strings.SplitSeq(text, "\n") {
		switch {
		case strings.TrimSpace(line) == "":
			closeList()
		case sceneHeadRe.MatchString(line):
			closeList()
			out.WriteString("<h4>" + sceneInline(sceneHeadRe.FindStringSubmatch(line)[2]) + "</h4>")
		case strings.HasPrefix(line, ">"):
			closeList()
			body := strings.TrimPrefix(strings.TrimPrefix(line, ">"), " ")
			out.WriteString("<blockquote>" + sceneInline(body) + "</blockquote>")
		case sceneOlRe.MatchString(line):
			if list != "ol" {
				closeList()
				out.WriteString("<ol>")
				list = "ol"
			}
			out.WriteString("<li>" + sceneInline(sceneOlRe.FindStringSubmatch(line)[1]) + "</li>")
		case sceneUlRe.MatchString(line):
			if list != "ul" {
				closeList()
				out.WriteString("<ul>")
				list = "ul"
			}
			out.WriteString("<li>" + sceneInline(sceneUlRe.FindStringSubmatch(line)[1]) + "</li>")
		default:
			closeList()
			out.WriteString("<p>" + sceneInline(line) + "</p>")
		}
	}
	closeList()
	return out.String()
}

// injectLabels mirrors the player's INJECT_LABEL map.
var injectLabels = map[string]string{
	"seam-briefing": "injected · session start",
	"seam-recall":   "injected · prompt recall",
}

// renderSceneStep is the player's renderStep, emitted statically.
func renderSceneStep(b *strings.Builder, step SceneStep) {
	switch step.Role {
	case "user":
		b.WriteString(`<div class="ln user show"><span class="p">&gt;</span> <span class="typed">` + sceneEsc(step.Text) + `</span></div>`)
	case "inject":
		body := sceneEsc(step.Text)
		for _, f := range step.Focus {
			ef := sceneEsc(f)
			body = strings.ReplaceAll(body, ef, `<mark class="foc">`+ef+`</mark>`)
		}
		label, ok := injectLabels[step.Tag]
		if !ok {
			label = "injected · " + step.Tag
		}
		b.WriteString(`<div class="ln inject show"><span class="inject-tag">` + sceneEsc(label) + `</span><span class="inject-body">` + body + `</span></div>`)
	case "agent":
		b.WriteString(`<div class="ln agent show">` + sceneMarkdown(step.Text) + `</div>`)
	case "tool":
		cls := "ln tool show"
		if step.Emphasis != "" {
			cls += " tool-" + step.Emphasis
		}
		b.WriteString(`<div class="` + cls + `"><span class="tool-dot" aria-hidden="true">●</span> <span class="tool-label">` + sceneEsc(step.Label) + `</span>`)
		if step.Result != "" {
			b.WriteString(` <span class="tool-res">` + sceneEsc(step.Result) + `</span>`)
		}
		b.WriteString(`</div>`)
	case "ffwd":
		b.WriteString(`<div class="ffwd show"><span class="ffwd-chip">4×</span> <span>` + sceneEsc(step.Text) + `</span></div>`)
	case "comment":
		b.WriteString(`<div class="cl-ln cl-comment show"><span class="p">$</span> <span class="c">` + sceneEsc(step.Text) + `</span></div>`)
	case "cmd":
		b.WriteString(`<div class="cl-ln cl-cmd show"><span class="p">$</span> ` + sceneEsc(step.Text) + `</div>`)
	case "files":
		b.WriteString(`<div class="cl-ln cl-files show"><div class="cl-filegrid">`)
		for _, f := range step.Files {
			cls := "cl-file"
			if f.Tag != "" {
				cls += " new"
			}
			b.WriteString(`<span class="` + cls + `">` + sceneEsc(f.Name))
			if f.Tag != "" {
				b.WriteString(` <span class="cl-tag">· ` + sceneEsc(f.Tag) + `</span>`)
			}
			b.WriteString(`</span>`)
		}
		b.WriteString(`</div></div>`)
	case "fm":
		if step.K != "" {
			b.WriteString(`<div class="cl-ln cl-fm show"><span class="k">` + sceneEsc(step.K) + `</span> <span class="s">` + sceneEsc(step.V) + `</span></div>`)
		} else {
			b.WriteString(`<div class="cl-ln cl-fm show"><span class="dim">` + sceneEsc(step.V) + `</span></div>`)
		}
	default:
		b.WriteString(`<div class="ln show">` + sceneEsc(step.Text) + `</div>`)
	}
	b.WriteString("\n")
}

// renderTerm wraps rendered steps in the shared .term component.
func renderTerm(b *strings.Builder, bar string, steps func(*strings.Builder)) {
	b.WriteString(`<div class="term scn-term"><div class="term-bar"><i></i><i></i><i></i><span>` + sceneEsc(bar) + `</span></div><div class="term-body">` + "\n")
	steps(b)
	b.WriteString(`</div></div>` + "\n")
}

// renderScene renders a scene's full static transcript block: the prompt line,
// then one titled section per pane (with-without) or one merged beat-ordered
// timeline (split), each pane's outcome quoted verbatim beside its transcript.
func renderScene(scene *Scene) template.HTML {
	var b strings.Builder
	b.WriteString(`<p class="ts-ask"><span class="ts-ask-label">prompt</span><span>` + sceneEsc(scene.Prompt) + `</span></p>` + "\n")
	if scene.Layout == "split" {
		renderSplitScene(&b, scene)
	} else {
		renderWithWithoutScene(&b, scene)
	}
	if scene.Caption != "" {
		b.WriteString(`<p class="scn-caption">` + sceneEsc(scene.Caption) + `</p>` + "\n")
	}
	return template.HTML(b.String()) //nolint:gosec // built from committed scenes.js data, escaped above
}

func renderWithWithoutScene(b *strings.Builder, scene *Scene) {
	// scenes.js orders panes without-first already, but the page's argument
	// depends on it, so pin the order rather than trusting the data.
	panes := append([]ScenePane{}, scene.Panes...)
	sort.SliceStable(panes, func(i, j int) bool { return panes[i].Key == "without" && panes[j].Key != "without" })
	for _, p := range panes {
		b.WriteString(`<section class="scn-pane scn-` + sceneEsc(p.Key) + `" id="` + sceneEsc(p.Key) + `-seamless">` + "\n")
		b.WriteString(`<h2>` + sceneEsc(p.Label) + `</h2>` + "\n")
		b.WriteString(`<p class="scn-source">headless Claude Code, session <code>` + sceneEsc(p.Source) + `</code></p>` + "\n")
		renderTerm(b, "~/code/myapp", func(b *strings.Builder) {
			for _, step := range p.Steps {
				renderSceneStep(b, step)
			}
		})
		b.WriteString(`<p class="scn-outcome scn-outcome-` + sceneEsc(p.Key) + `"><strong>Outcome.</strong> ` + sceneEsc(p.Outcome) + `</p>` + "\n")
		b.WriteString(`</section>` + "\n")
	}
}

// renderSplitScene merges both agents' steps into the player's beat order and
// renders them as one labeled timeline -- on a page, a single readable race
// beats two side-by-side scrolling panes.
func renderSplitScene(b *strings.Builder, scene *Scene) {
	type item struct {
		pane      ScenePane
		paneIndex int
		step      SceneStep
		beat      int
	}
	var items []item
	for pi, p := range scene.Panes {
		for si, step := range p.Steps {
			beat := si
			if step.Beat != nil {
				beat = *step.Beat
			}
			items = append(items, item{pane: p, paneIndex: pi, step: step, beat: beat})
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].beat != items[j].beat {
			return items[i].beat < items[j].beat
		}
		return items[i].paneIndex < items[j].paneIndex
	})

	b.WriteString(`<section class="scn-pane scn-split-timeline" id="timeline">` + "\n")
	b.WriteString(`<h2>One timeline, both agents</h2>` + "\n")
	var sources []string
	for _, p := range scene.Panes {
		sources = append(sources, p.Label+" <code>"+sceneEsc(p.Source)+"</code>")
	}
	b.WriteString(`<p class="scn-source">headless Claude Code, sessions ` + strings.Join(sources, " and ") + `</p>` + "\n")
	renderTerm(b, "~/code/myapp · one shared queue", func(b *strings.Builder) {
		for _, it := range items {
			b.WriteString(`<div class="scn-beat scn-beat-` + sceneEsc(it.pane.Key) + `"><span class="scn-who">` + sceneEsc(it.pane.Label) + `</span><div class="scn-beat-step">` + "\n")
			renderSceneStep(b, it.step)
			b.WriteString(`</div></div>` + "\n")
		}
	})
	for _, p := range scene.Panes {
		b.WriteString(`<p class="scn-outcome scn-outcome-` + sceneEsc(p.Key) + `"><strong>` + sceneEsc(p.Label) + `.</strong> ` + sceneEsc(p.Outcome) + `</p>` + "\n")
	}
	b.WriteString(`</section>` + "\n")
}

/* ---- output -------------------------------------------------------------- */

// scenarioTemplates is the standalone page template: scenario pages live at the
// site root and share the landing page's design system, not the docs shell.
var scenarioTemplates = template.Must(template.ParseFS(templateFS, "templates/scenario.html"))

// scenarioView is what the template receives.
type scenarioView struct {
	*Scenario
	// Others is every other scenario page, in order, for the cross-link rail.
	Others []*Scenario
	Assets assetVersions
}

// writeScenarios renders every scenario page under <siteDir>/scenarios/. Like
// writeSite it owns the whole directory and replaces it; cleanScenarios holds
// the same marker interlock so a mistyped path cannot delete hand-written
// files.
func writeScenarios(siteDir string, site *Site) error {
	outDir := filepath.Join(siteDir, scenariosDirName)
	if err := cleanScenarios(outDir); err != nil {
		return err
	}
	assets, err := loadAssetVersions()
	if err != nil {
		return err
	}
	for _, s := range site.Scenarios {
		var others []*Scenario
		for _, o := range site.Scenarios {
			if o != s {
				others = append(others, o)
			}
		}
		var buf bytes.Buffer
		buf.WriteString(generatedMarker + "\n")
		if err := scenarioTemplates.ExecuteTemplate(&buf, "scenario", scenarioView{Scenario: s, Others: others, Assets: assets}); err != nil {
			return fmt.Errorf("scenario %s: %w", s.Slug, err)
		}
		if err := writeFile(filepath.Join(outDir, s.Slug, "index.html"), buf.Bytes()); err != nil {
			return err
		}
	}
	return nil
}

// cleanScenarios removes a previously generated scenarios tree. The directory
// holds one subdirectory per page, each with a marked index.html; any entry
// that does not look like ours aborts the whole removal.
func cleanScenarios(outDir string) error {
	entries, err := os.ReadDir(outDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", outDir, err)
	}
	for _, e := range entries {
		index := filepath.Join(outDir, e.Name(), "index.html")
		raw, err := os.ReadFile(index)
		if !e.IsDir() || err != nil || !bytes.Contains(raw, []byte(generatedMarker)) {
			return fmt.Errorf("refusing to replace %s: %s is missing or was not generated by docsgen "+
				"(delete the directory by hand if this is really the output dir)", outDir, index)
		}
	}
	if err := os.RemoveAll(outDir); err != nil {
		return fmt.Errorf("clean %s: %w", outDir, err)
	}
	return nil
}
