package mcp

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/store"
)

// The advisory form's fields reach the client in reading order -- the
// decision, then the access, then how long -- since encoding/json would sort a
// map's keys and a client shows the fields in the order they arrive.
func TestAdvisoryForm_FieldsInReadingOrder(t *testing.T) {
	grant := newAdvisoryForm(approval.Facts{Kind: store.AccessKindGrant, Access: "read-only",
		AccessChoices: []string{"read-only", "read-write"}}, 8*time.Hour)
	raw, err := grant.schema()
	require.NoError(t, err)
	s := string(raw)
	require.True(t, strings.HasPrefix(s, `{"type":"object","properties":{"decision":{`), s)
	require.Less(t, strings.Index(s, `"decision":{`), strings.Index(s, `"access":{`))
	require.Less(t, strings.Index(s, `"access":{`), strings.Index(s, `"duration":{`))
	require.True(t, strings.HasSuffix(s, `},"required":["decision","access","duration"]}`), s)

	move := newAdvisoryForm(approval.Facts{Kind: store.AccessKindMove, Access: "read-write"}, 8*time.Hour)
	raw, err = move.schema()
	require.NoError(t, err)
	require.NotContains(t, string(raw), `"access"`, "a move has no access choice")
	require.NotContains(t, string(raw), `"duration"`, "a move no lock covers lasts for good")
	require.True(t, strings.HasSuffix(string(raw), `"required":["decision"]}`), string(raw))

	locked := newAdvisoryForm(approval.Facts{Kind: store.AccessKindMove, Access: "read-write", Locked: true}, 8*time.Hour)
	require.NotEmpty(t, locked.durations, "a locked target's lease is the one thing a move's duration bounds")
}

// What an accepted form decides: a decision it offered, and for an Approve a
// duration and an access it offered (the default when none came back).
// Anything else decides nothing.
func TestAdvisoryForm_Choice(t *testing.T) {
	grant := newAdvisoryForm(approval.Facts{Kind: store.AccessKindGrant, Access: "read-only",
		AccessChoices: []string{"read-only", "read-write"}}, 8*time.Hour)
	move := newAdvisoryForm(approval.Facts{Kind: store.AccessKindMove, Access: "read-write"}, 8*time.Hour)
	for _, c := range []struct {
		name    string
		form    advisoryForm
		content any
		want    advisoryChoice
		ok      bool
	}{
		{"approve with the defaults", grant, map[string]any{"decision": "approve"},
			advisoryChoice{decision: decisionApprove, duration: approval.ForSession, access: "read-only"}, true},
		{"approve read and write for an hour", grant, map[string]any{"decision": "approve", "access": "read-write", "duration": "1h"},
			advisoryChoice{decision: decisionApprove, duration: approval.For1h, access: "read-write"}, true},
		{"deny, whatever else came back", grant, map[string]any{"decision": "deny", "access": "admin", "duration": "9h"},
			advisoryChoice{decision: decisionDeny}, true},
		{"a move: the decision alone", move, map[string]any{"decision": "approve", "duration": "4h"},
			advisoryChoice{decision: decisionApprove, duration: approval.ForSession}, true},
		{"no decision", grant, map[string]any{"access": "read-write"}, advisoryChoice{}, false},
		{"a decision it did not offer", grant, map[string]any{"decision": "yes"}, advisoryChoice{}, false},
		{"an access it did not offer", grant, map[string]any{"decision": "approve", "access": "admin"}, advisoryChoice{}, false},
		{"a duration it did not offer", grant, map[string]any{"decision": "approve", "duration": "9h"}, advisoryChoice{}, false},
		{"no content at all", grant, nil, advisoryChoice{}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, ok := c.form.choice(c.content)
			require.Equal(t, c.ok, ok)
			require.Equal(t, c.want, got)
		})
	}
}

// The lead says what is asked, warnings counted first, and that Approve below
// decides it -- for a move, a read grant and a read-and-write ask.
func TestAdvisoryLead_SaysApproveDecides(t *testing.T) {
	one := []approval.Warning{{Code: approval.WarnMove, Text: "m"}}
	require.Equal(t, "1 warning. Session cc/a asks to move into example-finance.\nFor good: a move does not end. "+
		"Approve below decides it.", advisoryLead(approval.Facts{Kind: store.AccessKindMove, Project: "example-finance",
		Session: "cc/a", SessionID: "s", Warnings: one}, 8*time.Hour))
	require.Equal(t, "No warnings. Session cc/a asks for read-only access to example-finance from project example-legal.\n"+
		"Until its session ends (8 hours at most), unless you pick less below. Approve below decides it.",
		advisoryLead(approval.Facts{Kind: store.AccessKindGrant, Access: "read-only", Project: "example-finance",
			From: "example-legal", Session: "cc/a", SessionID: "s"}, 8*time.Hour))
	require.Contains(t, advisoryLead(approval.Facts{Kind: store.AccessKindGrant, Access: "read-only", AskedAccess: "read-write",
		Project: "example-finance", From: "example-legal", Session: "cc/a", SessionID: "s"}, 8*time.Hour),
		" asks for read-write access to example-finance from project example-legal.\n", "what the agent asked for")
}

// A move's first line is a sentence, so what follows it starts one.
func TestSentenceStart(t *testing.T) {
	require.Equal(t, "The owner approves it", sentenceStart("the owner approves it"))
	require.Equal(t, "Touch ID was cancelled", sentenceStart("Touch ID was cancelled"))
	require.Equal(t, "", sentenceStart(""))
	require.Equal(t, "`seam approve`", sentenceStart("`seam approve`"))
}
