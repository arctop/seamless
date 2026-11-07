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

// The dialog's first line is one sentence that says what is asked: Claude
// Code collapses an elicitation to it ("... (+13 more lines)").
func TestDialogLead_OneSentenceThatSaysWhatIsAsked(t *testing.T) {
	two := []approval.Warning{{Code: approval.WarnTainted, Text: "a"}, {Code: approval.WarnCWDOutside, Text: "b"}}
	base := approval.Facts{
		ID: "01M6VXQ62T1CQH11123076K71M", Short: "R-K71M", Project: "demo-vault", Isolation: "confidential",
		Access: "read-write", Kind: store.AccessKindRequest, Program: "/Users/owner/.local/share/claude/versions/2.1.319",
		Agent: "Claude Code 2.1.319", PID: 4242, Session: "cc/quiet-otter", SessionID: "01KSESSION", Warnings: two,
	}
	for _, c := range []struct {
		name string
		edit func(*approval.Facts)
		want string
	}{
		{"a lock's request", func(*approval.Facts) {},
			"Claude Code 2.1.319 asks for read-write access to demo-vault (confidential, locked) until its session ends " +
				"(8 hours at most) unless you pick less below; 2 warnings."},
		{"a refresh, one warning", func(f *approval.Facts) { f.Kind, f.Warnings = store.AccessKindRefresh, two[:1] },
			"Claude Code 2.1.319 asks again for read-write access to demo-vault (confidential, locked) until its session " +
				"ends (8 hours at most) unless you pick less below; 1 warning."},
		{"a read grant", func(f *approval.Facts) {
			f.Kind, f.Access, f.From, f.Project, f.Warnings = store.AccessKindGrant, "read-only", "example-ops", "example-legal", nil
		}, "Claude Code 2.1.319 asks for read-only access to example-legal (confidential) from project example-ops until " +
			"its session ends (8 hours at most) unless you pick less below; no warnings."},
		{"no session names it", func(f *approval.Facts) { f.Session, f.SessionID = "", "" },
			"Claude Code 2.1.319 asks for read-write access to demo-vault (confidential, locked) for 8 hours unless you " +
				"pick less below; 2 warnings."},
		{"not an agent the classifier knows: the program's base name", func(f *approval.Facts) {
			f.Agent, f.Program = "", "/opt/homebrew/bin/node"
		}, "node asks for read-write access to demo-vault (confidential, locked) until its session ends (8 hours at most) " +
			"unless you pick less below; 2 warnings."},
		{"no name at all: the pid", func(f *approval.Facts) { f.Agent, f.Program = "", "" },
			"pid 4242 asks for read-write access to demo-vault (confidential, locked) until its session ends (8 hours at " +
				"most) unless you pick less below; 2 warnings."},
		{"a value that tries to break the line", func(f *approval.Facts) { f.Project = "demo\nvault\u202e" },
			"Claude Code 2.1.319 asks for read-write access to demo vault (confidential, locked) until its session ends " +
				"(8 hours at most) unless you pick less below; 2 warnings."},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := base
			c.edit(&f)
			msg := dialogMessage(f, dialogShown{stopAt: time.Now(), capped: 8 * time.Hour})
			lead, rest, _ := strings.Cut(msg, "\n")
			require.Equal(t, c.want, lead)
			require.NotContains(t, lead, f.Short, "the request's id is further down")
			require.Contains(t, rest, "Request: "+f.Short+", ")
		})
	}

	// The Agent process line: the agent's name, the pid only when nothing
	// better is known; the passkey page in the closing line only with ways
	// that hold one.
	f := base
	msg := dialogMessage(f, dialogShown{stopAt: time.Now(), capped: 90 * time.Minute,
		ways: access.Ways{Passkey: true, Page: "https://example.test/console/access/" + f.ID}})
	require.Contains(t, msg, "until its session ends (90 minutes at most)")
	require.Contains(t, msg, "\nAgent process: Claude Code 2.1.319\n")
	require.NotContains(t, msg, "pid 4242")
	require.True(t, strings.HasSuffix(msg, "Declining leaves the request pending for `seam approve R-K71M` in your own "+
		"terminal, or a passkey at https://example.test/console/access/"+f.ID+"."), msg)
	f.Agent, f.Program = "", ""
	require.Contains(t, dialogMessage(f, dialogShown{capped: time.Hour}), "\nAgent process: pid 4242\n")
	require.NotContains(t, dialogMessage(f, dialogShown{capped: time.Hour}), "\nProgram:")
}
