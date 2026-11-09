package mcp

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/store"
)

// The dialog's first two lines say what is asked, warning count first:
// Claude Code collapses an elicitation to its first lines, cut at the
// terminal's edge ("... (+13 more lines)"), and does not expand the message.
func TestDialogLead_CountFirstThenWhatIsAsked(t *testing.T) {
	two := []approval.Warning{{Code: approval.WarnTainted, Text: "a"}, {Code: approval.WarnCWDOutside, Text: "b"}}
	base := approval.Facts{
		ID: "01M6VXQ62T1CQH11123076K71M", Short: "R-K71M", Project: "demo-vault", Isolation: "confidential",
		Access: "read-write", Kind: store.AccessKindRequest, Program: "/Users/owner/.local/share/claude/versions/2.1.319",
		Agent: "Claude Code 2.1.319", PID: 4242, Session: "cc/quiet-otter", SessionID: "01KSESSION", Warnings: two,
	}
	const untilEnd = "\nUntil its session ends (8 hours at most), unless you pick less below."
	for _, c := range []struct {
		name string
		edit func(*approval.Facts)
		want string
	}{
		{"a lock's request", func(*approval.Facts) {},
			"2 warnings. Claude Code 2.1.319 asks for read-write access to demo-vault." + untilEnd},
		{"a refresh, one warning", func(f *approval.Facts) { f.Kind, f.Warnings = store.AccessKindRefresh, two[:1] },
			"1 warning. Claude Code 2.1.319 asks again for read-write access to demo-vault." + untilEnd},
		{"a read grant", func(f *approval.Facts) {
			f.Kind, f.Access, f.From, f.Project, f.Warnings = store.AccessKindGrant, "read-only", "example-ops", "example-legal", nil
		}, "No warnings. Claude Code 2.1.319 asks for read-only access to example-legal from project example-ops." + untilEnd},
		{"no session names it", func(f *approval.Facts) { f.Session, f.SessionID = "", "" },
			"2 warnings. Claude Code 2.1.319 asks for read-write access to demo-vault.\nFor 8 hours, unless you pick less below."},
		{"not an agent the classifier knows: the program's base name", func(f *approval.Facts) {
			f.Agent, f.Program = "", "/opt/homebrew/bin/node"
		}, "2 warnings. node asks for read-write access to demo-vault." + untilEnd},
		{"no name at all: the pid", func(f *approval.Facts) { f.Agent, f.Program = "", "" },
			"2 warnings. pid 4242 asks for read-write access to demo-vault." + untilEnd},
		{"a value that tries to break the line", func(f *approval.Facts) { f.Project = "demo\nvault\u202e" },
			"2 warnings. Claude Code 2.1.319 asks for read-write access to demo vault." + untilEnd},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := base
			c.edit(&f)
			msg := dialogMessage(f, dialogShown{stopAt: time.Now(), capped: 8 * time.Hour})
			lead, rest, _ := strings.Cut(msg, "\n\n")
			require.Equal(t, c.want, lead)
			require.NotContains(t, lead, f.Short, "the request's id is further down")
			require.NotContains(t, lead, "confidential", "the isolation is on the Project line")
			require.Contains(t, rest, "Request: "+f.Short+", ")
			require.Contains(t, "\n"+rest, "\nProject: ")
			require.Contains(t, rest, "confidential")
		})
	}

	// The Agent process line: the agent's name, the pid only when nothing
	// better is known; the passkey page in the closing line only with ways
	// that hold one.
	f := base
	msg := dialogMessage(f, dialogShown{stopAt: time.Now(), capped: 90 * time.Minute,
		ways: access.Ways{Passkey: true, Page: "https://example.test/console/access/" + f.ID}})
	require.Contains(t, msg, "\nUntil its session ends (90 minutes at most), unless you pick less below.\n")
	require.Contains(t, msg, "\nAgent process: Claude Code 2.1.319\n")
	require.NotContains(t, msg, "pid 4242")
	require.True(t, strings.HasSuffix(msg, "Declining leaves the request pending for `seam approve R-K71M` in your own "+
		"terminal, or a passkey at https://example.test/console/access/"+f.ID+"."), msg)
	f.Agent, f.Program = "", ""
	require.Contains(t, dialogMessage(f, dialogShown{capped: time.Hour}), "\nAgent process: pid 4242\n")
	require.NotContains(t, dialogMessage(f, dialogShown{capped: time.Hour}), "\nProgram:")
}
