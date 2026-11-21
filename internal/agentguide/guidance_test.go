package agentguide

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

func TestMCPInstructions_FirstParagraphIsSelfContained(t *testing.T) {
	first, _, _ := strings.Cut(MCPInstructions, "\n")
	require.LessOrEqual(t, utf8.RuneCountInString(first), 512)

	for _, concept := range []string{
		"Recall before guessing",
		"memory_write",
		"notes_create",
		"AGENTS.md/CLAUDE.md",
		"project explicitly",
		"sessions/findings",
		"plan:<slug>",
		"inputSchema",
	} {
		require.Contains(t, first, concept)
	}
}

func TestBriefingFooter_PreservesRetrievalAndSchemaReminders(t *testing.T) {
	require.Contains(t, BriefingFooter, "recall")
	require.Contains(t, BriefingFooter, "memory_read")
	require.Contains(t, BriefingFooter, "inputSchema")
}

func TestMCPInstructions_ContainsCanonicalWorkflowTerms(t *testing.T) {
	for _, term := range RequiredWorkflowTerms() {
		require.Contains(t, MCPInstructions, term)
	}
	for _, tool := range RequiredToolNames() {
		require.Contains(t, MCPInstructions, tool)
	}
}

// The mishap sentence lists core's vocabulary word for word and in order: this
// package imports nothing, so the list is written out here, and this is what
// keeps it from drifting from the types a listing filters by.
func TestMishapPrefix_ListsTheVocabulary(t *testing.T) {
	slugs := make([]string, 0, len(core.MishapTypes))
	for _, typ := range core.MishapTypes {
		slugs = append(slugs, string(typ))
	}
	require.Equal(t, strings.Join(slugs[:len(slugs)-1], ", ")+" or "+slugs[len(slugs)-1], MishapTypeList)
	require.Contains(t, MishapPrefix, "type and a colon ("+MishapTypeList+")")
	require.Contains(t, MCPInstructions, MishapPrefix, "the server-wide guidance asks for the prefix too")
}
