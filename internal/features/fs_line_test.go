package features

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

// Every feature has a Line: the one sentence its Settings row shows, at most
// lineMax runes, so it stays one line beside the name. It is its own
// sentence, never the Blurb cut short: the Blurb stays whole for the docs,
// the doctor and the row's hint.
func TestFsRegistry_EveryFeatureHasAShortLine(t *testing.T) {
	seen := map[string]Key{}
	for _, f := range Registry() {
		line := f.Line
		require.NotEmpty(t, line, "%s needs a Settings line", f.Key)
		require.LessOrEqual(t, utf8.RuneCountInString(line), lineMax,
			"%s: the line is %d runes; a Settings row holds %d", f.Key, utf8.RuneCountInString(line), lineMax)
		first, _ := utf8.DecodeRuneInString(line)
		require.True(t, unicode.IsUpper(first), "%s: the line is a sentence, so it starts with a capital: %q", f.Key, line)
		require.True(t, strings.HasSuffix(line, "."), "%s: the line is a sentence, so it ends with a period: %q", f.Key, line)
		require.Equal(t, strings.TrimSpace(line), line, "%s: no padding", f.Key)
		require.NotContains(t, line, "--", "%s: one plain sentence, no dashes", f.Key)
		require.NotEqual(t, f.Blurb, line, "%s: the line is not the blurb", f.Key)
		for _, r := range line {
			require.True(t, unicode.IsPrint(r), "%s: the line holds a control or format character %U", f.Key, r)
		}
		owner, dup := seen[line]
		require.False(t, dup, "%s and %s share a line", owner, f.Key)
		seen[line] = f.Key
	}
}

// The limit itself is the one the row is drawn for.
func TestFsRegistry_LineLimitIsEighty(t *testing.T) {
	require.Equal(t, 80, lineMax)
}
