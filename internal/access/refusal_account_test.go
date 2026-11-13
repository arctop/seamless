package access

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// A refusal note keeps the restricted account of the request that noted it,
// beside the reason: the handler that records the refusal reads both after
// the request, from a context that never held its View.
func TestNoteRefusal_KeepsTheAccount(t *testing.T) {
	p := restrictedPolicy(t)
	agent, ok := p.PeerView(502)
	require.True(t, ok)

	outer, note := WithRefusalNote(context.Background())
	NoteRefusal(WithView(outer, agent), "peer.owner_only")
	NoteRefusal(WithView(outer, p.OwnerView()), "auth.failed")
	require.Equal(t, "peer.owner_only", note.Reason())
	require.Equal(t, "_example_agent", note.Account(), "the first reason stands, and with it its account")
	require.Empty(t, AccountOf(outer), "the outer context holds no View")

	outer, note = WithRefusalNote(context.Background())
	NoteRefusal(WithView(outer, p.OwnerView()), "peer.owner_only")
	require.Empty(t, note.Account(), "the owner is no account")
	outer, note = WithRefusalNote(context.Background())
	NoteRefusal(outer, "peer.unknown")
	require.Equal(t, "peer.unknown", note.Reason())
	require.Empty(t, note.Account(), "a peer the policy does not know has none")

	var none *RefusalNote
	require.Empty(t, none.Account())
	NoteRefusal(WithView(context.Background(), agent), "peer.owner_only") // no note planted: nothing to write
}
