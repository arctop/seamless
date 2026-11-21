package mcp

import (
	"strings"

	"github.com/arctop/seamless/internal/agentguide"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
)

// mishapsArgDesc is session_end's mishaps argument. It asks for the type
// prefix in agentguide's words, which MCPInstructions renders too, so the two
// surfaces cannot ask for it two ways.
const mishapsArgDesc = "Self-report mishaps this session caused: an action a warning or convention said not to take, live state touched by mistake, a command that hit the wrong target. Pass an array with one short entry per incident; omit when none happened. " +
	agentguide.MishapPrefix + ". When a mishap violated a stored memory, name that memory by its exact slug in the entry " +
	"(e.g. \"ignored-warning: violated chroma-boot-race by ...\") -- the report is then linked to it. " +
	"Recorded for recurrence review, not blame -- report them even when fully recovered."

// mishapPayload is the agent.mishap event payload for one reported entry. The
// description is the entry as the agent wrote it (truncated to maxChars), type
// prefix and all, exactly as before types existed; an entry that opens with a
// type (core.ParseMishap) adds "type", and only then. The memory scan runs over
// the whole entry, so a slug named anywhere in it links, prefix or not. An
// untyped entry is recorded as it always was: never refused.
func mishapPayload(entry string, maxChars int, corpus []core.Memory) map[string]any {
	payload := map[string]any{"description": events.Truncate(entry, maxChars)}
	if typ, _ := core.ParseMishap(entry); typ != "" {
		payload["type"] = string(typ)
	}
	if ids := mishapMemoryIDs(entry, corpus); len(ids) > 0 {
		payload["item_ids"] = ids
	}
	return payload
}

// mishapMemoryIDs returns the ids of memories whose name appears verbatim in a
// mishap's text. It is the ingestion half of mishap-memory linkage: the scan
// runs exactly once, when session_end records the report, and the matches
// persist in the agent.mishap event payload (item_ids) -- the event log is
// durable, so nothing ever re-matches later against a corpus that has moved on.
//
// A name matches only as a whole slug token: the occurrence must be bounded on
// both sides by the text's edge or a non-slug byte, so a name that is a prefix
// of a longer name (chroma-boot vs chroma-boot-race) never false-matches inside
// it. Matching is case-sensitive -- the tool description asks agents to name
// the violated memory by its exact slug -- and the caller controls scope by
// passing only the memories the report may name (the session project's own plus
// global; never another project's).
func mishapMemoryIDs(text string, memories []core.Memory) []string {
	var ids []string
	for _, m := range memories {
		if m.Name == "" || m.ID == "" {
			continue
		}
		if containsSlugToken(text, m.Name) {
			ids = append(ids, m.ID)
		}
	}
	return ids
}

// containsSlugToken reports whether slug occurs in text delimited by non-slug
// bytes (or the text's edges). Slug bytes are ASCII letters, digits, '-' and
// '_' -- the alphabet memory names are written in -- so every byte of a
// multi-byte rune is a delimiter and the byte-level boundary check is UTF-8
// safe.
func containsSlugToken(text, slug string) bool {
	for from := 0; ; {
		i := strings.Index(text[from:], slug)
		if i < 0 {
			return false
		}
		start := from + i
		end := start + len(slug)
		if (start == 0 || !isSlugByte(text[start-1])) && (end == len(text) || !isSlugByte(text[end])) {
			return true
		}
		from = start + 1
	}
}

// isSlugByte reports whether b belongs to the slug alphabet.
func isSlugByte(b byte) bool {
	switch {
	case 'a' <= b && b <= 'z', 'A' <= b && b <= 'Z', '0' <= b && b <= '9':
		return true
	case b == '-', b == '_':
		return true
	}
	return false
}
