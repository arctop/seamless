package screening

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Marks show the owner WHERE a held write's reasons sit: the console colors
// each span of the preview a screen matched, with the reason's color, so the
// issues read at first sight. They are computed over the preview the owner
// already reads -- secrets redacted to "[redacted secret]", hidden characters
// spelled out as [U+XXXX] -- never over the raw text, so a mark can only ever
// point at what the page shows anyway. Events and agents still see codes
// alone (Code's contract): marks are a console rendering, never recorded.

// Segment is one run of a preview and the reasons whose matches cover it,
// outermost (the widest match) first: a hidden comment holding an override
// phrase covers the phrase with both, so neither reason's mark is lost. A
// run no reason covers has no codes.
type Segment struct {
	Text  string
	Codes []Code
}

// Family groups a code for color coding: secret, injection, hidden, unicode,
// blob, url, proposal, or rule (the binding-rule policies: a session that
// read outside text, a constraint superseded, deleted or demoted, lockdown).
func Family(c Code) string {
	s := string(c)
	switch {
	case strings.HasPrefix(s, "secret."):
		return "secret"
	case strings.HasPrefix(s, "instruction."):
		return "injection"
	case strings.HasPrefix(s, "hidden."):
		return "hidden"
	case strings.HasPrefix(s, "unicode."):
		return "unicode"
	case strings.HasPrefix(s, "blob."):
		return "blob"
	case strings.HasPrefix(s, "url."):
		return "url"
	case c == ReasonRestrictedAccount:
		return "proposal"
	}
	return "rule"
}

// familyRank orders the families from the most serious: the reasons list
// in this order, a held card's accent takes its most serious reason's
// color, and of two equally wide marks the more serious one nests outside.
var familyRank = map[string]int{"secret": 0, "injection": 1, "hidden": 2, "unicode": 3, "url": 4, "blob": 5, "rule": 6, "proposal": 7}

// FamilyRank is a family's place from the most serious (0); an unknown
// family ranks last.
func FamilyRank(family string) int {
	if r, ok := familyRank[family]; ok {
		return r
	}
	return len(familyRank)
}

// MostSerious returns the family of the most serious code in codes ("" for
// none).
func MostSerious(codes []string) string {
	best := ""
	for _, c := range codes {
		f := Family(Code(c))
		if best == "" || familyRank[f] < familyRank[best] {
			best = f
		}
	}
	return best
}

// The phrase screens run on normalized text (lower case, one space); over the
// preview they run case-insensitively instead, so their spans are the
// preview's own bytes. \s+ in every pattern already spans the original
// whitespace runs.
var (
	overrideMarkRes    = foldCase(overrideRes)
	concealmentMarkRes = foldCase(concealmentRes)
	exfilVerbMarkRe    = foldOne(exfilVerbRe)
	exfilSecretMarkRe  = foldOne(exfilSecretRe)
	exfilDestMarkRe    = foldOne(exfilDestRe)
	exfilToURLMarkRe   = foldOne(exfilToURLRe)
	exfilCmdMarkRe     = foldOne(exfilCmdRe)
	exfilPathMarkRe    = foldOne(exfilPathRe)
	revealedRe         = regexp.MustCompile(`\[U\+([0-9A-F]{4,5})\]`)
)

func foldOne(re *regexp.Regexp) *regexp.Regexp { return regexp.MustCompile("(?i)" + re.String()) }

func foldCase(res []*regexp.Regexp) []*regexp.Regexp {
	out := make([]*regexp.Regexp, len(res))
	for i, re := range res {
		out[i] = foldOne(re)
	}
	return out
}

// redactedMarker is what RedactSecrets leaves in place of a secret.
const redactedMarker = "[redacted secret]"

type span struct {
	start, end int
	code       Code
}

// PreviewSegments splits preview into runs, each covered by at most one of
// codes (a held write's reasons), for the screens that can say where they
// matched. Only codes the write was held for are marked: a link in a write
// held for a secret stays plain. A reason with no place in the text (a
// proposal, a session's outside-text mark, a constraint supersede) marks
// nothing; its chip says it.
func PreviewSegments(preview string, codes []string, rules Rules) []Segment {
	held := make(map[Code]bool, len(codes))
	for _, c := range codes {
		held[Code(c)] = true
	}
	var spans []span
	add := func(code Code, locs [][]int) {
		if !held[code] {
			return
		}
		for _, l := range locs {
			if l[1] > l[0] {
				spans = append(spans, span{l[0], l[1], code})
			}
		}
	}

	// Secrets: the redaction markers. The kinds are gone from the preview,
	// so every marker takes the first secret reason.
	for _, c := range codes {
		if Family(Code(c)) == "secret" {
			add(Code(c), indexAll(preview, redactedMarker))
			break
		}
	}

	// Hidden characters, as RevealHidden spelled them out.
	for _, m := range revealedRe.FindAllStringSubmatchIndex(preview, -1) {
		cp, err := strconv.ParseUint(preview[m[2]:m[3]], 16, 32)
		if err != nil {
			continue
		}
		switch r := rune(cp); {
		case isTag(r):
			add(UnicodeTag, [][]int{m[:2]})
		case isZeroWidth(r):
			add(UnicodeZeroWidth, [][]int{m[:2]})
		case isBidi(r):
			add(UnicodeBidi, [][]int{m[:2]})
		}
	}

	// Injection markers.
	for _, re := range overrideMarkRes {
		add(InstructionOverride, re.FindAllStringIndex(preview, -1))
	}
	unquoted := blankCode(preview)
	for _, re := range roleMarkerRes {
		add(InstructionRoleMarker, re.FindAllStringIndex(unquoted, -1))
	}
	for _, re := range concealmentMarkRes {
		add(InstructionConcealment, re.FindAllStringIndex(preview, -1))
	}
	if held[InstructionExfiltration] {
		add(InstructionExfiltration, exfiltrationSentences(preview, rules.URLAllowlist))
	}

	// Hidden text: the whole comment or invisible element. It holds only for
	// an imperative inside (HiddenInstruction); the record-only codes mark
	// it when a write carries them as reasons too.
	hiddenCode := Code("")
	for _, c := range []Code{HiddenInstruction, HiddenComment, HiddenStyle} {
		if held[c] {
			hiddenCode = c
			break
		}
	}
	if hiddenCode != "" {
		held[hiddenCode] = true
		for _, re := range []*regexp.Regexp{htmlCommentRe, mdCommentRe, hiddenStyleRe, hiddenAttrRe} {
			add(hiddenCode, re.FindAllStringIndex(preview, -1))
		}
	}

	// Encoded blobs and unlisted links.
	if held[BlobBase64] || held[BlobHex] {
		for _, b := range blobSpans(preview, rules.blobMin()) {
			add(b.code, [][]int{{b.start, b.end}})
		}
	}
	if held[URLUnlisted] && len(rules.URLAllowlist) > 0 {
		for _, l := range urlRe.FindAllStringIndex(preview, -1) {
			if !hostAllowed(urlHost(preview[l[0]:l[1]]), rules.URLAllowlist) {
				add(URLUnlisted, [][]int{l})
			}
		}
	}
	return paint(preview, spans)
}

// paint cuts preview at every span's edges and lists, for each run, the
// codes whose spans cover it: widest span first, so the marks nest from the
// outside in (and the more serious of two equally wide ones outside). A code
// is listed once per run however many of its spans cover it. Runs whose
// covering codes match are merged.
func paint(preview string, spans []span) []Segment {
	if preview == "" {
		return nil
	}
	if len(spans) == 0 {
		return []Segment{{Text: preview}}
	}
	slices.SortStableFunc(spans, func(a, b span) int {
		if wa, wb := a.end-a.start, b.end-b.start; wa != wb {
			return wb - wa
		}
		return familyRank[Family(a.code)] - familyRank[Family(b.code)]
	})
	cuts := []int{0, len(preview)}
	for _, s := range spans {
		cuts = append(cuts, min(s.start, len(preview)), min(s.end, len(preview)))
	}
	slices.Sort(cuts)
	cuts = slices.Compact(cuts)
	var out []Segment
	for i := 0; i+1 < len(cuts); i++ {
		from, to := cuts[i], cuts[i+1]
		var codes []Code
		for _, s := range spans {
			if s.start <= from && to <= s.end && !slices.Contains(codes, s.code) {
				codes = append(codes, s.code)
			}
		}
		if n := len(out); n > 0 && slices.Equal(out[n-1].Codes, codes) {
			out[n-1].Text += preview[from:to]
			continue
		}
		out = append(out, Segment{Text: preview[from:to], Codes: codes})
	}
	return out
}

// indexAll returns the byte spans of every occurrence of sub in s.
func indexAll(s, sub string) [][]int {
	var out [][]int
	for from := 0; ; {
		i := strings.Index(s[from:], sub)
		if i < 0 {
			return out
		}
		out = append(out, []int{from + i, from + i + len(sub)})
		from += i + len(sub)
	}
}

// blankCode is stripCode keeping every byte offset: markdown code becomes
// spaces byte for byte, so a role tag's span in the result is its span in s.
func blankCode(s string) string {
	b := []byte(s)
	for _, re := range []*regexp.Regexp{fencedCodeRe, inlineCodeRe} {
		for _, l := range re.FindAllStringIndex(string(b), -1) {
			for i := l[0]; i < l[1]; i++ {
				if b[i] != '\n' {
					b[i] = ' '
				}
			}
		}
	}
	return string(b)
}

// exfiltrationSentences is exfiltrations with places: the spans of the
// sentences that direct secret material somewhere outside.
func exfiltrationSentences(text string, allow []string) [][]int {
	var out [][]int
	start := 0
	cuts := append(sentenceSplit.FindAllStringIndex(text, -1), []int{len(text), len(text)})
	for _, cut := range cuts {
		sentence := text[start:cut[0]]
		hit := false
		if exfilVerbMarkRe.MatchString(sentence) && exfilSecretMarkRe.MatchString(sentence) &&
			outsideDestinationIn(sentence, allow) {
			hit = true
		} else if exfilCmdMarkRe.MatchString(sentence) {
			for _, p := range exfilPathMarkRe.FindAllString(sentence, -1) {
				if !strings.HasSuffix(strings.ToLower(p), ".pub") {
					hit = true
					break
				}
			}
		}
		if hit {
			out = append(out, []int{start, cut[0]})
		}
		start = cut[1]
	}
	return out
}

// outsideDestinationIn is outsideDestination for text in any case.
func outsideDestinationIn(sentence string, allow []string) bool {
	if exfilDestMarkRe.MatchString(sentence) {
		return true
	}
	for _, m := range exfilToURLMarkRe.FindAllStringSubmatch(sentence, -1) {
		if host := urlHost(m[1]); host != "" && !hostAllowed(host, allow) {
			return true
		}
	}
	return false
}

// blobSpans is scanBlobs with places.
func blobSpans(text string, minLen int) []span {
	var out []span
	start, n := -1, 0
	flush := func(end int) {
		if start >= 0 && n >= minLen {
			run := strings.TrimRight(text[start:end], "=")
			switch {
			case strings.IndexFunc(run, func(r rune) bool { return !isHexRune(r) }) < 0 && mixedClasses(run, true, false):
				out = append(out, span{start, end, BlobHex})
			case mixedClasses(run, true, true):
				out = append(out, span{start, end, BlobBase64})
			}
		}
		start, n = -1, 0
	}
	for i, r := range text {
		if isBase64Rune(r) {
			if start < 0 {
				start = i
			}
			n++
			continue
		}
		flush(i)
	}
	flush(len(text))
	return out
}
