package access

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
)

// The clearance rule as the access layer judges it (isolation.requests): a
// write's destination must be cleared for everything the conversation holds.

const (
	clOps     = "example-ops"     // open
	clFin     = "example-finance" // confidential
	clLegal   = "example-legal"   // confidential
	clVault   = "example-vault"   // sealed
	clConvKey = "claude-code/conv-1"
)

func clCtx(g Grants) context.Context { return WithGrants(context.Background(), g) }

func TestWriteCleared_TheRule(t *testing.T) {
	now := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		label   []string
		flows   []Flow
		cleared map[string]bool // dest -> cleared; "" is the global scope
	}{
		{"nothing held: every destination", nil, nil,
			map[string]bool{clOps: true, clFin: true, clVault: true, "": true}},
		{"a fenced home holds itself", []string{clFin}, nil,
			map[string]bool{clFin: true, clLegal: false, clOps: false, "": false}},
		{"a read of legal from finance flows into finance only", []string{clFin, clLegal},
			[]Flow{{From: clLegal, To: clFin}},
			map[string]bool{clFin: true, clLegal: false, clVault: false, clOps: false, "": false}},
		{"a write grant flows both ways", []string{clFin, clLegal},
			[]Flow{{From: clLegal, To: clFin}, {From: clFin, To: clLegal}},
			map[string]bool{clFin: true, clLegal: true, clOps: false, "": false}},
		{"a later read clears nothing new: legal no longer takes all of it", []string{clFin, clLegal, clVault},
			[]Flow{{From: clLegal, To: clFin}, {From: clFin, To: clLegal}, {From: clVault, To: clFin}},
			map[string]bool{clFin: true, clLegal: false, clVault: false, clOps: false, "": false}},
		{"a destination outside the label every member flows into", []string{clFin},
			[]Flow{{From: clFin, To: clLegal}},
			map[string]bool{clFin: true, clLegal: true, clOps: false, "": false}},
		{"a moved conversation holds its new home only", []string{clLegal}, nil,
			map[string]bool{clLegal: true, clOps: false, "": false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := JudgeConversation(context.Background(), now, Conversation{Key: clConvKey, Label: tc.label, Flows: tc.flows})
			require.True(t, g.Judged())
			ctx := clCtx(g)
			for dest, want := range tc.cleared {
				require.Equal(t, want, WriteCleared(ctx, dest), "WriteCleared(%q)", dest)
				require.Equal(t, !want, WriteConfined(ctx, dest), "WriteConfined(%q) is its wrapper", dest)
			}
		})
	}
}

// Open projects and the global scope are cleared only while the label is
// empty: a flow into either never clears it, even if one reached the judge.
func TestWriteCleared_OpenAndGlobalOnlyWhileEmpty(t *testing.T) {
	g := JudgeConversation(context.Background(), time.Now(), Conversation{Key: clConvKey, Label: []string{clFin},
		Flows: []Flow{{From: clFin, To: ""}, {From: "", To: clOps}, {From: clFin, To: clFin}}})
	require.False(t, WriteCleared(clCtx(g), ""), "no flow into the global scope survives the judge")
	require.Empty(t, g.Flows(), "self-flows and flows naming the global scope are dropped")
	require.Equal(t, []string{clFin}, g.ClearedProjects())
}

func TestJudgeConversation_GrantsNameTheHolder(t *testing.T) {
	now := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	rows := []Grant{
		{ID: "g1", Project: clFin, From: clOps, Conversation: clConvKey, Expires: now.Add(time.Hour)},                // the conversation's
		{ID: "g2", Project: clLegal, From: clOps, Session: "S-link", Expires: now.Add(time.Hour)},                    // an older grant of one of its sessions
		{ID: "g3", Project: clLegal, From: clOps, Conversation: "claude-code/other", Expires: now.Add(time.Hour)},    // another conversation's
		{ID: "g4", Project: clLegal, From: clOps, Session: "S-other", Expires: now.Add(time.Hour)},                   // another session's
		{ID: "g5", Project: clLegal, From: clOps, Process: "boot:1:1", Expires: now.Add(time.Hour)},                  // a process grant: never without peer_auth
		{ID: "g6", Project: clVault, From: clFin, Conversation: clConvKey, Expires: now.Add(-time.Minute)},           // expired: not live
		{ID: "g7", Project: clFin, From: clLegal, Conversation: clConvKey, Expires: now.Add(time.Hour), Write: true}, // a write grant
		{ID: "g8", Project: clFin, From: clFin, Conversation: clConvKey, Expires: now.Add(time.Hour)},                // a self-grant: nothing
		{ID: "g9", Project: clLegal, From: clFin, Conversation: clConvKey, Expires: now.Add(time.Hour), Ended: true}, // revoked: not live
	}
	g := JudgeConversation(context.Background(), now, Conversation{Key: clConvKey, Sessions: []string{"S-amb", "S-link"},
		Home: clLegal, Grants: rows})
	var ids []string
	for _, l := range g.Live() {
		ids = append(ids, l.ID)
	}
	require.ElementsMatch(t, []string{"g1", "g2", "g7"}, ids)
	ctx := clCtx(g)
	require.True(t, ReadGranted(ctx, clOps, clFin))
	require.True(t, ReadGranted(ctx, clOps, clLegal), "a session's grant from before migration 039 is the conversation's")
	require.True(t, ReadGranted(ctx, clLegal, clFin), "a live write grant reads too")
	require.False(t, ReadGranted(ctx, clFin, clVault), "an expired grant reads nothing")
	require.False(t, ReadGranted(ctx, clFin, clLegal), "a revoked grant reads nothing")

	// The write grant opens its home's outbound fence for its target, and
	// nothing else does.
	require.True(t, WriteGranted(ctx, clLegal, clFin))
	require.False(t, WriteGranted(ctx, clOps, clFin), "a read grant opens no write")
	require.False(t, WriteGranted(ctx, clLegal, clVault))
	require.False(t, WriteGranted(ctx, clLegal, ""), "never the global scope")
	away := JudgeConversation(context.Background(), now, Conversation{Key: clConvKey, Home: clOps, Grants: rows})
	require.False(t, WriteGranted(clCtx(away), clLegal, clFin), "only from the conversation's home")

	// Identified by nothing: nothing held, nothing refused.
	require.Equal(t, Grants{}, JudgeConversation(context.Background(), now, Conversation{Grants: rows, Label: []string{clFin}}))
	require.True(t, WriteCleared(clCtx(Grants{}), ""))
}

func TestJudgeConversation_PeerAuthKeysOnTheChain(t *testing.T) {
	now := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	policy, err := New(config.PeerAuth{Owner: "owner"}, func(n string) (Account, error) {
		return Account{UID: 501, Name: "owner"}, nil
	})
	require.NoError(t, err)
	agent := ProcessToken{PID: 200, Version: 2000, Boot: grantBoot}
	bridge := ProcessToken{PID: 300, Version: 3000, Boot: grantBoot}
	stranger := ProcessToken{PID: 400, Version: 4000, Boot: grantBoot}
	rows := []Grant{
		{ID: "g1", Project: clLegal, From: clFin, Process: agent.Key(), Expires: now.Add(time.Hour), Write: true},
		{ID: "g2", Project: clLegal, From: clFin, Process: stranger.Key(), Expires: now.Add(time.Hour)},
		{ID: "g3", Project: clLegal, From: clFin, Conversation: clConvKey, Expires: now.Add(time.Hour)}, // never under peer_auth
		{ID: "g4", Project: clLegal, From: clFin, Session: "S1", Expires: now.Add(time.Hour)},           // never under peer_auth
	}
	c := Conversation{Key: clConvKey, Sessions: []string{"S1"}, Home: clFin, Label: []string{clFin, clLegal},
		Flows: []Flow{{From: clLegal, To: clFin}, {From: clFin, To: clLegal}}, Grants: rows}
	v, ok := policy.PeerChainView(501, []ProcessToken{bridge, agent})
	require.True(t, ok)
	ctx := WithView(context.Background(), v)
	g := JudgeConversation(ctx, now, c)
	require.Len(t, g.Live(), 1)
	require.Equal(t, "g1", g.Live()[0].ID)
	ctx = WithGrants(ctx, g)
	require.True(t, WriteGranted(ctx, clFin, clLegal))
	require.True(t, WriteCleared(ctx, clLegal))
	require.False(t, WriteCleared(ctx, clOps))

	// The TCP key and a restricted account hold nothing and are refused
	// nothing here.
	tcp := WithView(context.Background(), policy.OwnerView())
	require.Equal(t, Grants{}, JudgeConversation(tcp, now, c))
	rp := restrictedPolicy(t)
	rv, ok := rp.PeerChainView(502, []ProcessToken{agent})
	require.True(t, ok)
	require.Equal(t, Grants{}, JudgeConversation(WithView(context.Background(), rv), now, c))
}

// A legacy judged state (JudgeGrants) keeps the previous release's
// confinement, and WriteGranted never answers for it.
func TestWriteCleared_LegacyJudgeKeepsTheConfinement(t *testing.T) {
	now := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	g := JudgeGrants(context.Background(), "S1", now, []Grant{
		{ID: "g1", Project: clFin, From: clOps, Session: "S1", Expires: now.Add(time.Hour), Write: true}})
	require.False(t, g.Judged())
	ctx := clCtx(g)
	require.True(t, WriteCleared(ctx, clOps))
	require.False(t, WriteCleared(ctx, clFin))
	require.False(t, WriteCleared(ctx, ""))
	require.False(t, WriteGranted(ctx, clOps, clFin))
	require.Equal(t, []string{clOps}, g.ClearedProjects())
}

// What a request's text promises: the state once a read is approved.
func TestGrants_WithRead(t *testing.T) {
	now := time.Now()
	open := JudgeConversation(context.Background(), now, Conversation{Key: clConvKey, Home: clOps})
	after := open.WithRead(clFin, clOps, false)
	require.Equal(t, []string{clFin}, after.Label())
	require.False(t, after.Cleared(clOps), "an open home is not cleared once it holds fenced content")
	require.Equal(t, []string{clFin}, after.ClearedProjects())
	require.True(t, open.Cleared(clOps), "WithRead returns a copy")

	fenced := JudgeConversation(context.Background(), now, Conversation{Key: clConvKey, Home: clLegal, Label: []string{clLegal}})
	after = fenced.WithRead(clFin, clLegal, true)
	require.Equal(t, []string{clFin, clLegal}, after.Label())
	require.True(t, after.Cleared(clLegal))
	require.False(t, after.Cleared(clFin))
	require.Equal(t, []string{clLegal}, after.ClearedProjects())

	require.Equal(t, Grants{}, Grants{}.WithRead(clFin, clOps, false), "an unjudged state is unchanged")
}

// Nothing in ctx -- isolation.requests off -- clears everything and grants
// no write: the fence stays exactly as it was.
func TestClearance_OffChangesNothing(t *testing.T) {
	ctx := context.Background()
	for _, dest := range []string{clOps, clFin, clVault, ""} {
		require.True(t, WriteCleared(ctx, dest))
		require.False(t, WriteConfined(ctx, dest))
	}
	require.False(t, WriteGranted(ctx, clLegal, clFin))
}
