package core

import (
	"strings"

	"github.com/oklog/ulid/v2"
)

// Source references (isolation.requests, plan fenced-sessions P7; owner
// decision N2 (a) and placement rule 3). An item names an item of another
// project it copied from in one fixed form, written in its body: the project
// slug, a colon and the item's full id -- "example-legal:01M7..." -- the form a
// receipt's reference takes too (store.ReceiptRef). Nothing registers a
// reference and no tool takes an argument for one: the body is the record, so
// every write path carries references as it carries text, and a default
// install's tool surface stays as it was. The agent text that asks for them
// renders CitationForm, so the form it teaches is the one Citations reads.
//
// The gardener reads them from the bodies of stamped memories (Memory.
// MayContain): it flags a copy whose source changed after the copy was last
// written, and proposes moving a copy whose sources all lie in one other
// project into that project.

// MaxCitations bounds the distinct references Citations returns for one body,
// so a body listing thousands of ids costs the gardener a bounded number of
// lookups.
const MaxCitations = 64

// Citation is one source reference: the item ID in project Project.
type Citation struct {
	Project string
	ID      string
}

// String renders the reference in its fixed form, "<project>:<id>".
func (c Citation) String() string { return c.Project + ":" + c.ID }

// CitationForm is the fixed form of a reference to one of project's items,
// with the id left to fill in: "example-legal:<id>".
func CitationForm(project string) string { return project + ":<id>" }

// Citations returns the distinct source references in body, in the order
// they first appear, at most MaxCitations.
//
// A reference is a project slug -- a letter or a digit, then letters, digits,
// '.', '_' or '-' -- with a colon and a 26-character ULID right after it, the
// ULID ending the token (no letter or digit follows). The id comes back in its
// canonical upper-case spelling. "global" is the global scope's spelling and
// no project, so it names nothing here. A slug with any other character cannot
// be written in this form; the form is what Citations reads, nothing looser.
func Citations(body string) []Citation {
	var out []Citation
	seen := map[Citation]bool{}
	rest := 0
	for len(out) < MaxCitations {
		i := strings.IndexByte(body[rest:], ':')
		if i < 0 {
			break
		}
		colon := rest + i
		rest = colon + 1
		c, ok := citationAt(body, colon)
		if !ok || seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	return out
}

// citationAt reads the reference whose colon sits at body[colon], if one does.
func citationAt(body string, colon int) (Citation, bool) {
	start := colon
	for start > 0 && slugByte(body[start-1]) {
		start--
	}
	// The token starts at its first letter or digit: punctuation the slug
	// alphabet allows ("-", ".", "_") cannot open one.
	for start < colon && !alnumByte(body[start]) {
		start++
	}
	end := colon + 1 + ulid.EncodedSize
	if start == colon || end > len(body) || (end < len(body) && alnumByte(body[end])) {
		return Citation{}, false
	}
	project := body[start:colon]
	if project == "global" || project == "_global" {
		return Citation{}, false
	}
	id, err := ulid.ParseStrict(body[colon+1 : end])
	if err != nil {
		return Citation{}, false
	}
	return Citation{Project: project, ID: id.String()}, true
}

// slugByte reports whether b may appear in a citable project slug.
func slugByte(b byte) bool { return alnumByte(b) || b == '-' || b == '_' || b == '.' }

// alnumByte reports whether b is an ASCII letter or digit.
func alnumByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}
