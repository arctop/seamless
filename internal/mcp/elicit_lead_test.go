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
		{"a move", func(f *approval.Facts) { f.Kind, f.From, f.Warnings = store.AccessKindMove, "example-ops", two[:1] },
			"1 warning. Claude Code 2.1.319 asks to move into demo-vault.\nFor good; the duration below bounds only the " +
				"approval, unless you pick less below."},
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

	// The note on the agent's own claims comes with them: a dialog showing none
	// of the three has nothing for it to describe.
	require.NotContains(t, dialogMessage(base, dialogShown{capped: time.Hour}), approval.SelfReportedNote)
	withClaim := base
	withClaim.CWD = "/work/demo-vault"
	claimed := dialogMessage(withClaim, dialogShown{capped: time.Hour})
	require.Contains(t, claimed, "\nWorking directory: /work/demo-vault\n"+approval.SelfReportedNote+"\n")
}

// The dialog's durations say what each grants under the lease cap: a fixed
// length the cap cuts short reads as the cap, and a longer one that would
// grant the same is not offered. The 8h default offers every duration as it
// is; the answer is read against what was offered.
func TestDialogDurations_FollowTheLeaseCap(t *testing.T) {
	for _, c := range []struct {
		name   string
		capped time.Duration
		values []string
		names  []string
	}{
		{"the 8h default", 8 * time.Hour, []string{"session", "1h", "4h"},
			[]string{"Until this session ends", "1 hour", "4 hours"}},
		{"a 2h cap", 2 * time.Hour, []string{"session", "1h", "4h"},
			[]string{"Until this session ends", "1 hour", "2 hours (the max_lease cap)"}},
		{"a 1h cap: 1 hour is the cap itself", time.Hour, []string{"session", "1h"},
			[]string{"Until this session ends", "1 hour"}},
		{"a 20m cap", 20 * time.Minute, []string{"session", "1h"},
			[]string{"Until this session ends", "20 minutes (the max_lease cap)"}},
		{"no cap known", 0, []string{"session", "1h", "4h"},
			[]string{"Until this session ends", "1 hour", "4 hours"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			values, names := durationChoices(c.capped)
			require.Equal(t, c.values, values)
			require.Equal(t, c.names, names)
			req := formElicitation(approval.Facts{Short: "R-K71M", Project: "demo-vault"}, []string{approval.ProofTouchID},
				dialogShown{capped: c.capped})
			field := req.Params.RequestedSchema.(map[string]any)["properties"].(map[string]any)["duration"].(map[string]any)
			require.Equal(t, c.values, field["enum"])
			require.Equal(t, c.names, field["enumNames"])
			require.Equal(t, approval.ForSession, field["default"])
		})
	}

	offered, _ := durationChoices(20 * time.Minute)
	_, _, ok := dialogChoice(map[string]any{"duration": "4h", "method": "touchid"}, offered, []string{"touchid"})
	require.False(t, ok, "a duration the dialog did not offer")
	d, _, ok := dialogChoice(map[string]any{"duration": "1h", "method": "touchid"}, offered, []string{"touchid"})
	require.True(t, ok)
	require.Equal(t, "1h", d)

	require.Equal(t, "for 20 minutes (the max_lease cap)", durationPhrase("1h", 20*time.Minute))
	require.Equal(t, "for 1 hour", durationPhrase("1h", 8*time.Hour))
	require.Equal(t, "for 1 hour", durationPhrase("1h", time.Hour), "the cap itself is not cut short")
	require.Equal(t, "until its session ends", durationPhrase("session", 20*time.Minute))
	require.Empty(t, durationPhrase("", 20*time.Minute))
}

// The passkey page's dialog leads as the form does: a move asks to move, never
// for an access level (it read "asks for read-write access to" on the first
// released move, 2026-11-14), a refresh asks again, a grant names where from.
func TestPasskeyMessage_SaysWhatIsAsked(t *testing.T) {
	base := approval.Facts{
		Short: "R-QBC0", Project: "example-finance", Access: "read-write", Kind: store.AccessKindMove,
		From: "example-ops", Agent: "Claude Code 2.1.325",
		Warnings: []approval.Warning{{Code: "move", Text: "This agent has worked in example-ops."}},
	}
	for _, c := range []struct {
		name string
		edit func(*approval.Facts)
		want string
	}{
		{"a move", func(*approval.Facts) {}, "1 warning. Claude Code 2.1.325 asks to move into example-finance.\n"},
		{"a refresh", func(f *approval.Facts) { f.Kind, f.From = store.AccessKindRefresh, "" },
			"1 warning. Claude Code 2.1.325 asks again for read-write access to example-finance.\n"},
		{"a read grant", func(f *approval.Facts) { f.Kind, f.Access = store.AccessKindGrant, "read-only" },
			"1 warning. Claude Code 2.1.325 asks for read-only access to example-finance from project example-ops.\n"},
		{"a lock's request", func(f *approval.Facts) { f.Kind, f.From = store.AccessKindRequest, "" },
			"1 warning. Claude Code 2.1.325 asks for read-write access to example-finance.\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := base
			c.edit(&f)
			msg := passkeyMessage(f, "Approve it")
			require.True(t, strings.HasPrefix(msg, c.want), msg)
			require.True(t, strings.HasSuffix(msg, ".\nApprove it with your passkey, or deny it, on the page this opens, "+
				"which shows the request (R-QBC0) in full. Opening it grants nothing by itself."), msg)
		})
	}
}

// A write's grant between confidential projects (P13, 2026-11-14): the lead
// names the access asked, read-write, and its second line says that accepting
// approves read-only -- the owner who accepts the visible lines must not read
// the write as approved -- and where read and write is approved instead.
func TestDialogLead_AWritesGrantSaysAcceptingIsReadOnly(t *testing.T) {
	f := approval.Facts{
		Short: "R-7P7Y", Project: "example-conf-b", Isolation: "confidential", Access: "read-only",
		Kind: store.AccessKindGrant, From: "example-conf-a", Agent: "Claude Code 2.1.325",
		Session: "cc/a", SessionID: "01KSESSION", AccessChoices: []string{"read-only", "read-write"},
		AskedAccess: "read-write", Warnings: []approval.Warning{{Code: approval.WarnCopyInto, Text: "copy"}},
	}
	lead := func(ways access.Ways) string {
		l, _, _ := strings.Cut(dialogMessage(f, dialogShown{stopAt: time.Now(), capped: 8 * time.Hour, ways: ways}), "\n\n")
		return l
	}
	require.Equal(t, "1 warning. Claude Code 2.1.325 asks for read-write access to example-conf-b from project example-conf-a.\n"+
		"Accepting here approves read-only, which clears no write; for read and write, use Passkey below and choose "+
		"read and write on its page, or `seam approve R-7P7Y --access read-write` in your own terminal.",
		lead(access.Ways{TouchID: true, Passkey: true, Page: "https://example.test/console/access/x"}))
	require.Equal(t, "1 warning. Claude Code 2.1.325 asks for read-write access to example-conf-b from project example-conf-a.\n"+
		"Accepting here approves read-only, which clears no write; for read and write, use `seam approve R-7P7Y "+
		"--access read-write` in your own terminal.", lead(access.Ways{TouchID: true}), "no passkey enrolled")

	read := f
	read.AskedAccess = "read-only"
	require.Equal(t, "1 warning. Claude Code 2.1.325 asks for read-only access to example-conf-b from project example-conf-a.\n"+
		"Until its session ends (8 hours at most), unless you pick less below.",
		strings.SplitN(dialogMessage(read, dialogShown{capped: 8 * time.Hour}), "\n\n", 2)[0], "a read asks nothing more")
}

// Approved read-only, a write's call is told it stays refused, never to retry:
// the retry would be refused again and file another request, whose dialog
// approves read-only again (P13, 2026-11-14).
func TestRefusalAfterDialog_AWriteApprovedReadOnly(t *testing.T) {
	until := time.Date(2026, 11, 14, 17, 29, 0, 0, time.Local)
	target := dialogTarget{ID: "01KREQ", Short: "R-7P7Y", Project: "example-conf-b", Grant: true, Write: true}
	end := dialogEnd{state: stateApproved, request: store.AccessApproval{FromProject: "example-conf-a"}, until: until}
	text := "memory_write: rule\nRemedy: remedy"

	got := refusalAfterDialog("memory_write", text, target, end, access.Ways{}, true)
	require.Equal(t, "memory_write: the owner approved read-and-write request R-7P7Y as read-only: project example-conf-b is "+
		"readable from project example-conf-a, nothing else until "+access.Clock(until)+".\nRemedy: a read grant clears no "+
		"write, so this write stays refused. Write in example-conf-a instead; if the task needs to change example-conf-b, "+
		"ask the owner first whether to make it again: that files a new request, which they approve as read and write on "+
		"its passkey page or with `seam approve <its id> --access read-write` (accepting its dialog approves read-only).", got)
	require.NotContains(t, got, "retry it now")

	end.write = true
	require.Contains(t, refusalAfterDialog("memory_write", text, target, end, access.Ways{}, true),
		"readable and writable from project example-conf-a, nothing else until "+access.Clock(until)+
			".\nRemedy: this call ran before the approval and did nothing -- retry it now.", "approved read and write")
}
