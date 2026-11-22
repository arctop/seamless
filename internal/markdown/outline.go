package markdown

import (
	"bytes"
	"html/template"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
)

// HeadingPrefix leads every id RenderOutline gives a heading, so a body's
// headings never take an id the page around them uses: no console element id
// starts with it.
const HeadingPrefix = "doc-"

// Heading is one entry of a body's outline: an h2 or h3, its text, and the
// id its rendered element carries (HeadingPrefix and a slug of the text).
type Heading struct {
	Level int
	Text  string
	ID    string
}

// headingID is the one id shape an outline heading may carry through the
// sanitizer: the prefix, then lowercase letters, digits and single dashes.
var headingID = regexp.MustCompile(`^` + HeadingPrefix + `[a-z0-9]+(?:-[a-z0-9]+)*$`)

// outlinePolicy is policy plus an id on h2 and h3 of headingID's shape. It
// is RenderOutline's alone: Render's output keeps no id at all, so the peek
// and reader fragments stay as they were.
var outlinePolicy = func() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()
	p.AllowAttrs("data-peek").OnElements("a")
	p.AllowAttrs("align").OnElements("td", "th")
	p.RequireNoFollowOnLinks(false)
	p.AllowAttrs("id").Matching(headingID).OnElements("h2", "h3")
	return p
}()

// maxSlug caps a heading slug, so a long heading makes a readable anchor.
const maxSlug = 60

// RenderOutline is Render for a page that lists the body's sections: each h2
// and h3 gets an id (HeadingPrefix and a slug of its text, numbered when two
// headings would share one), and the headings come back in order for an "On
// this page" list. Raw HTML stays disabled and the same sanitizer runs, with
// only that id added on h2 and h3, so a heading's text can never set an
// attribute: the id is built here from letters and digits alone.
func RenderOutline(body string, resolve WikiResolver) (template.HTML, []Heading) {
	if strings.TrimSpace(body) == "" {
		return "", nil
	}
	src := []byte(body)
	pc := parser.NewContext()
	if resolve != nil {
		pc.Set(resolverKey, resolve)
	}
	doc := md.Parser().Parse(text.NewReader(src), parser.WithContext(pc))
	var (
		outline []Heading
		used    = map[string]bool{}
	)
	err := ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		h, ok := n.(*ast.Heading)
		if !entering || !ok {
			return ast.WalkContinue, nil
		}
		if h.Level != 2 && h.Level != 3 {
			return ast.WalkSkipChildren, nil
		}
		words := strings.Join(strings.Fields(inlineText(h, src)), " ")
		if words == "" {
			return ast.WalkSkipChildren, nil
		}
		// Two headings never share an id: a repeat takes the first number
		// free, whatever the other headings' own text says.
		base := headingSlug(words)
		slug := base
		for i := 2; used[slug]; i++ {
			slug = base + "-" + strconv.Itoa(i)
		}
		used[slug] = true
		id := HeadingPrefix + slug
		h.SetAttributeString("id", []byte(id))
		outline = append(outline, Heading{Level: h.Level, Text: words, ID: id})
		return ast.WalkSkipChildren, nil
	})
	if err != nil {
		// The walk above returns no error of its own; were one to come, the
		// body still renders, as Render renders it, with no outline.
		return Render(body, resolve), nil
	}
	var buf bytes.Buffer
	if err := md.Renderer().Render(&buf, src, doc); err != nil {
		// A bytes.Buffer never fails a write; degrade as Render does.
		return template.HTML(template.HTMLEscapeString(body)), nil
	}
	return template.HTML(outlinePolicy.SanitizeBytes(buf.Bytes())), outline
}

// inlineText is the text a heading reads as: its text and code spans, and a
// wiki link's anchor text.
func inlineText(n ast.Node, src []byte) string {
	var b strings.Builder
	err := ast.Walk(n, func(c ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch t := c.(type) {
		case *ast.Text:
			b.Write(t.Value(src))
			if t.SoftLineBreak() || t.HardLineBreak() {
				b.WriteByte(' ')
			}
		case *ast.String:
			b.Write(t.Value)
		case *wikiLink:
			b.WriteString(t.display)
			return ast.WalkSkipChildren, nil
		}
		return ast.WalkContinue, nil
	})
	if err != nil {
		return "" // no text: the heading is left out of the outline
	}
	return b.String()
}

// headingSlug is a heading's text as an anchor: lowercase letters and digits,
// each run of anything else one dash, at most maxSlug long; "section" when
// nothing is left (a heading of symbols, or in a script without ASCII).
func headingSlug(words string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(words) {
		if b.Len() >= maxSlug {
			break
		}
		if r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			b.WriteRune(r)
			dash = false
			continue
		}
		dash = true
	}
	if s := strings.Trim(b.String(), "-"); s != "" {
		return s
	}
	return "section"
}
