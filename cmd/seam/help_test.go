package main

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// stubEnv returns an env whose streams are buffers and whose world panics if a
// handler reaches for it. Everything tested here fails before dial by design:
// that is what the parse/execute split buys.
func stubEnv() (*env, *bytes.Buffer, *bytes.Buffer) {
	var out, errb bytes.Buffer
	return &env{stdin: strings.NewReader(""), stdout: &out, stderr: &errb}, &out, &errb
}

func TestSynopsis_RendersFromTheSpec(t *testing.T) {
	require.Equal(t, "seam capture [--project SLUG] <url>", synopsis(captureCmd))
	require.Equal(t, "seam prime [--cwd DIR] [--name NAME]", synopsis(primeCmd))
	require.Equal(t, "seam recall [--json] [--limit N] [--project SLUG] [--scope SCOPE] <word>...", synopsis(recallCmd))
}

// Every migrated command must reach the page. A spec whose group is not in
// groupOrder renders NOWHERE -- help would simply omit it, with nothing failing:
// the silent-drop shape this whole plan exists to kill, in the help layer.
func TestHelpText_RendersEveryMigratedCommand(t *testing.T) {
	help := helpText()
	for _, c := range commands() {
		require.Contains(t, groupOrder, c.group, "%s: group %q renders nowhere", c.name, c.group)
		require.Contains(t, help, "seam "+c.name, "%s is in the table but not on the page", c.name)
		require.NotEmpty(t, c.summary, "%s: the command list shows the summary", c.name)
	}
}

// Nothing on the page is hand-written any more: with hook migrated there is no
// second source for a line to come from, so the whole set is here.
func TestHelpText_CoversTheWholeCommandSet(t *testing.T) {
	help := helpText()
	for _, want := range []string{
		"seam plan approve <slug>",
		"seam access request [--json] [--reason TEXT] [--write] <project>",
		"seam approve [--access ACCESS] [--for DURATION] [--passkey] [--watch] [<request>]",
		"seam status",
		"seam hook",
	} {
		require.Contains(t, help, want)
	}
}

// The two halves of a locked project's access request sit together under their
// own heading, and seam approve's help says what proves the decision.
func TestHelpText_LockedProjects(t *testing.T) {
	help := helpText()
	section := help[strings.Index(help, "\n"+groupAccess+":\n"):]
	section = section[:strings.Index(section[1:], "\n\n")+1]
	require.Contains(t, section, "seam access request")
	require.Contains(t, section, "seam approve")

	page := commandHelp(approveCmd)
	for _, want := range []string{"sudo -k", "Touch ID or password", "an agent cannot use it", "--passkey", "--watch"} {
		require.Contains(t, page, want)
	}
	e, out, _ := stubEnv()
	require.Equal(t, 0, dispatch(context.Background(), e, []string{"approve", "--help"}), "help needs no terminal")
	require.Contains(t, out.String(), "usage: seam approve")
}

// Groups render in groupOrder regardless of where their content comes from.
func TestHelpText_GroupsRenderInOrder(t *testing.T) {
	help := helpText()
	at := make([]int, 0, len(groupOrder))
	for _, g := range groupOrder {
		i := strings.Index(help, "\n"+g+":\n")
		require.GreaterOrEqual(t, i, 0, "group %q is missing", g)
		at = append(at, i)
	}
	require.True(t, slices.IsSorted(at), "groups must render in groupOrder")
}

// The preamble is gone, and must not come back: it told the reader that
// "seam capture URL --project p" is an error, which the permuting parser makes
// false. B7 removes the guard it described; this pins the claim itself.
func TestHelpText_AdvertisesNoFlagOrder(t *testing.T) {
	help := strings.ToLower(helpText())
	for _, gone := range []string{"must precede", "flags go before", "flags before"} {
		require.NotContains(t, help, gone)
	}
}

func TestDispatch_HelpExitsZero(t *testing.T) {
	// The regression: --help propagated flag.ErrHelp into the same funnel as a
	// real failure, so it printed "error: flag: help requested" and exited 1.
	for _, argv := range [][]string{{"--help"}, {"-h"}, {"help"}} {
		e, out, _ := stubEnv()
		require.Equal(t, 0, dispatch(context.Background(), e, argv), "%v", argv)
		require.Contains(t, out.String(), "agent loop:", "help goes to stdout")
	}
}

func TestDispatch_CommandHelpExitsZero(t *testing.T) {
	e, out, _ := stubEnv()
	require.Equal(t, 0, dispatch(context.Background(), e, []string{"capture", "--help"}))
	require.Contains(t, out.String(), "usage: seam capture [--project SLUG] <url>")
	require.Contains(t, out.String(), "--project SLUG", "per-command help lists the flags")
}

func TestDispatch_NoArgsIsUsageError(t *testing.T) {
	e, _, errb := stubEnv()
	require.Equal(t, 2, dispatch(context.Background(), e, nil))
	require.Contains(t, errb.String(), "agent loop:")
}

func TestDispatch_UnknownCommand(t *testing.T) {
	e, _, errb := stubEnv()
	require.Equal(t, 2, dispatch(context.Background(), e, []string{"bogus"}))
	require.Contains(t, errb.String(), `unknown command "bogus"`)
}

// A parse failure reports the error and the command's own synopsis, exits 2, and
// never reaches the handler -- e.dial is nil here, so a dispatch that got that far
// would panic rather than pass.
//
// 2 is the parse/execute split: the caller typed it wrong (2) rather than it went
// wrong (1), which is flag's own convention and makes seam scriptable. hook is
// the one exemption, pinned in hook_test.go.
func TestDispatch_ParseErrorNeverReachesTheHandler(t *testing.T) {
	tests := []struct {
		name string
		argv []string
		want string
	}{
		{"unknown flag", []string{"recall", "foo", "--projct", "typo"}, "not defined"},
		{"bad enum", []string{"recall", "foo", "--scope", "bogus"}, "valid values are all, memories, notes, tasks, trials, sessions"},
		{"non positive limit", []string{"recall", "foo", "--limit", "0"}, "must be a positive integer"},
		{"missing query", []string{"recall"}, "missing argument"},
		{"missing url", []string{"capture"}, "missing argument"},
		{"too many urls", []string{"capture", "a", "b"}, "unexpected argument"},
		{"bad kind", []string{"remember", "--kind", "bogus"}, "valid values are constraint"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, _, errb := stubEnv()
			require.Equal(t, 2, dispatch(context.Background(), e, tt.argv))
			require.Contains(t, errb.String(), tt.want)
			require.Contains(t, errb.String(), "usage: seam "+tt.argv[0])
		})
	}
}

// The headline behavior, at the layer that decides it: a migrated command binds
// its flags from either side of the positional. Before this, "seam capture URL
// --project p" dropped --project (and requireFlagsFirst then rejected the whole
// line rather than let it file to the wrong scope).
func TestParse_MigratedCommandsPermute(t *testing.T) {
	const url = "https://example.com"
	for _, argv := range [][]string{
		{"capture", "--project", "p", url},
		{"capture", url, "--project", "p"},
		{"capture", url, "--project=p"},
	} {
		p, err := parse(commands(), argv)
		require.NoError(t, err, "%v", argv)
		require.Equal(t, "p", *p.opts.(*captureOpts).project, "%v", argv)
		require.Equal(t, []string{url}, p.pos, "%v", argv)
	}
}

// recall's query words survive as a tail, flags bind from either side, and the
// terminator makes a leading-dash word literal -- the one case the hand-rolled
// parser could not express.
func TestParse_RecallQueryTail(t *testing.T) {
	p, err := parse(commands(), []string{"recall", "chroma", "boot", "--limit", "3", "race"})
	require.NoError(t, err)
	require.Equal(t, []string{"chroma", "boot", "race"}, p.pos)
	require.Equal(t, 3, *p.opts.(*recallOpts).limit)

	p, err = parse(commands(), []string{"recall", "--", "-foo"})
	require.NoError(t, err)
	require.Equal(t, []string{"-foo"}, p.pos)
}

// The defaults the handlers rely on, which no flag.Value may quietly change.
func TestBindRecall_Defaults(t *testing.T) {
	p, err := parse(commands(), []string{"recall", "q"})
	require.NoError(t, err)
	o := p.opts.(*recallOpts)
	require.Equal(t, "all", *o.scope)
	require.Equal(t, 10, *o.limit)
}

// call's help lists every tool it can reach. The list is the literal callTools,
// which TestCallTools_MatchTheCatalog pins to the server's catalog, so between
// them the help names exactly the catalog.
func TestCommandHelp_CallListsEveryTool(t *testing.T) {
	help := commandHelp(callCmd)
	require.Contains(t, help, "usage: seam call <tool> [<json>]")
	for _, name := range callTools {
		require.Contains(t, help, " "+name, "%s is missing from seam call --help", name)
	}
	for _, line := range strings.Split(help, "\n") {
		require.LessOrEqual(t, len(line), 100, "the tool list wraps: %q", line)
	}
}

// Every read command takes --json. A read verb added without it would leave an
// agent scraping text again; one listed here and since removed fails too.
func TestReadCommands_TakeJSON(t *testing.T) {
	read := []string{
		"recall", "memory read", "memory list", "note read", "note list",
		"ready", "task list", "plan list", "plan show",
		"status", "sessions", "project isolation", "usage", "version",
	}
	seen := 0
	for _, c := range commands() {
		if !slices.Contains(read, c.name) {
			continue
		}
		seen++
		require.NotNil(t, bindTo(c).Lookup("json"), "%s is a read command and must take --json", c.name)
		require.Contains(t, synopsis(c), "[--json]", c.name)
	}
	require.Equal(t, len(read), seen, "every listed read command is in the table")
}

// The new families are commands of their own groups, and a bare family name
// names its members like task and plan do.
func TestDispatch_KnowledgeFamiliesListTheirMembers(t *testing.T) {
	for family, want := range map[string]string{
		"memory": "valid values are memory read, memory list",
		"note":   "valid values are note read, note list",
	} {
		e, _, errb := stubEnv()
		require.Equal(t, 2, dispatch(context.Background(), e, []string{family}))
		require.Contains(t, errb.String(), want)
	}
}
