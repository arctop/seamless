package core

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseMishap(t *testing.T) {
	cases := []struct {
		name, text string
		typ        MishapType
		rest       string
	}{
		{"typed", "shell-trap: a bare == in a zsh command died with = not found", MishapShellTrap,
			"a bare == in a zsh command died with = not found"},
		{"every type", "wrong-target: edited the installed config", MishapWrongTarget, "edited the installed config"},
		{"live state", "live-state: restarted the running daemon", MishapLiveState, "restarted the running daemon"},
		{"ignored warning", "ignored-warning: violated chroma-boot-race by booting first", MishapIgnoredWarning,
			"violated chroma-boot-race by booting first"},
		{"fabricated", "fabricated: passed a --force flag the tool does not have", MishapFabricated,
			"passed a --force flag the tool does not have"},
		{"other", "other: something else", MishapOther, "something else"},
		{"upper case", "Shell-Trap: quoting", MishapShellTrap, "quoting"},
		{"spaces for dashes", "wrong target: the main clone", MishapWrongTarget, "the main clone"},
		{"underscores for dashes", "ignored_warning: pushed", MishapIgnoredWarning, "pushed"},
		{"spaced words", "  live   state  :  touched the data dir  ", MishapLiveState, "touched the data dir"},
		{"leading space", "   fabricated: an id", MishapFabricated, "an id"},
		{"no space after colon", "other:tight", MishapOther, "tight"},
		{"empty rest", "shell-trap:", MishapShellTrap, ""},
		// The colon may sit at byte 31 (the 32nd), and no further.
		{"colon at the last place a prefix may end", "shell-trap" + strings.Repeat(" ", 21) + ": late", MishapShellTrap, "late"},
		{"colon one byte too far", "shell-trap" + strings.Repeat(" ", 22) + ": late", "",
			"shell-trap" + strings.Repeat(" ", 22) + ": late"},
		{"the bound counts after the leading space", "   shell-trap" + strings.Repeat(" ", 21) + ": late", MishapShellTrap, "late"},

		// Untyped: never refused, the text as it was.
		{"old free text", "pkill -f matched the live daemon as well as the scratch one", "",
			"pkill -f matched the live daemon as well as the scratch one"},
		{"old linked report", "violated pkill-target-check: pkill -f matched the live daemon", "",
			"violated pkill-target-check: pkill -f matched the live daemon"},
		{"unknown word", "note: the build was slow", "", "note: the build was slow"},
		{"a path", "/Users/owner/repos/app:12 was edited in the wrong clone", "",
			"/Users/owner/repos/app:12 was edited in the wrong clone"},
		{"a lock line", "LOCK seamless main: held without a claim", "", "LOCK seamless main: held without a claim"},
		{"a url", "https://example.com/x was fetched twice", "", "https://example.com/x was fetched twice"},
		{"a type word past the prefix bound", "this sentence runs on for a while, shell-trap: no", "",
			"this sentence runs on for a while, shell-trap: no"},
		{"type word, no colon", "shell-trap a bare ==", "", "shell-trap a bare =="},
		{"colon first", ": shell-trap", "", ": shell-trap"},
		{"blank", "", "", ""},
		{"the filter word is no type", "untyped: a report", "", "untyped: a report"},
		{"near miss", "shell-traps: two of them", "", "shell-traps: two of them"},
		{"joined words", "shelltrap: x", "", "shelltrap: x"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			typ, rest := ParseMishap(c.text)
			require.Equal(t, c.typ, typ)
			require.Equal(t, c.rest, rest)
		})
	}
}

// The vocabulary is the six frozen slugs, in order, each with a meaning, and
// the untyped filter word is none of them.
func TestMishapTypes_Vocabulary(t *testing.T) {
	var slugs []string
	for _, typ := range MishapTypes {
		slugs = append(slugs, string(typ))
		require.True(t, typ.Valid(), typ)
		require.NotEmpty(t, typ.Meaning(), typ)
		require.True(t, strings.HasSuffix(typ.Meaning(), "."), "%s: a meaning is a sentence", typ)
		parsed, _ := ParseMishap(string(typ) + ": x")
		require.Equal(t, typ, parsed, "every type parses as its own prefix")
	}
	require.Equal(t, []string{"shell-trap", "wrong-target", "live-state", "ignored-warning", "fabricated", "other"}, slugs)
	require.False(t, MishapType(MishapUntyped).Valid())
	require.False(t, MishapType("").Valid())
	require.Empty(t, MishapType("note").Meaning())
}

func TestMishapText(t *testing.T) {
	require.Equal(t, "a bare == died", MishapText("shell-trap: a bare == died", MishapShellTrap))
	require.Equal(t, "a bare == died", MishapText("Shell trap: a bare == died", MishapShellTrap))
	// An untyped report reads as it was written, prefix-shaped or not.
	require.Equal(t, "shell-trap: a bare == died", MishapText("shell-trap: a bare == died", ""))
	require.Equal(t, "free text: as written", MishapText("free text: as written", ""))
	// A recorded type the text does not open with leaves the text alone.
	require.Equal(t, "other: x", MishapText("other: x", MishapShellTrap))
	require.Equal(t, "no prefix at all", MishapText("no prefix at all", MishapFabricated))
	require.Equal(t, "untyped: x", MishapText("untyped: x", MishapUntyped))
}
