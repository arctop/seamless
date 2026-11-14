package access

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A write's stamps: one per label project other than the destination, named
// by the flow that clears it there.
func TestGrants_StampsNameTheFlowIntoTheDestination(t *testing.T) {
	now := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	const (
		data  = "example-data"
		fin   = "example-finance"
		legal = "example-legal"
	)
	g := JudgeConversation(context.Background(), now, Conversation{Key: "claude-code/c1", Home: data, HomeKnown: true,
		Label: []string{data, fin, legal},
		Flows: []Flow{
			{From: fin, To: data, Via: "grant", Ref: "01GRANTFIN"},
			{From: legal, To: data, Via: "grant", Ref: "01GRANTLEGAL"},
			// A second record for one pair: the first the store listed names it.
			{From: fin, To: data, Via: "move", Ref: "01LATER"},
			{From: data, To: fin, Via: "grant", Ref: "01WRITEGRANT"},
		}})
	require.True(t, g.Cleared(data))
	require.Equal(t, []Stamp{
		{Project: fin, Via: "grant", Ref: "01GRANTFIN"},
		{Project: legal, Via: "grant", Ref: "01GRANTLEGAL"},
	}, g.Stamps(data), "the home itself is no stamp of an item written there")
	require.False(t, g.Cleared(fin), "legal has no flow into finance")
	require.Equal(t, []Stamp{{Project: data, Via: "grant", Ref: "01WRITEGRANT"}}, g.Stamps(fin),
		"a project with no flow into the destination gets none: its write is refused before it lands")
	require.Nil(t, g.Stamps(""), "the global scope is never cleared for fenced content")

	// The label only the destination: nothing to stamp.
	own := JudgeConversation(context.Background(), now, Conversation{Key: "claude-code/c2", Home: legal, HomeKnown: true,
		Label: []string{legal}})
	require.True(t, own.Cleared(legal))
	require.Nil(t, own.Stamps(legal))

	// Off: nothing in ctx, nothing stamped; the zero and the legacy judged state
	// stamp nothing either.
	require.Nil(t, StampsFor(context.Background(), data))
	require.Nil(t, Grants{}.Stamps(data))
	require.Nil(t, JudgeGrants(context.Background(), "s", now, []Grant{{ID: "g", Project: fin, From: data, Session: "s",
		Expires: now.Add(time.Hour)}}).Stamps(data))
	ctx := WithGrants(context.Background(), g)
	require.Equal(t, g.Stamps(data), StampsFor(ctx, data))
}
