package store

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

func TestHolder_KeysRoundTripAndStayApart(t *testing.T) {
	proc, err := HolderOfProcess("boot-1:4242:7")
	require.NoError(t, err)
	conv, err := HolderOfConversation("claude-code", "alice:9f2c-41")
	require.NoError(t, err)
	sess, err := HolderOfSession("01SESSIONX")
	require.NoError(t, err)
	for _, c := range []struct {
		h          Holder
		kind, key  string
		pk, sid    string
		cli, extID string
	}{
		{proc, HolderProcess, "process:boot-1:4242:7", "boot-1:4242:7", "", "", ""},
		{conv, HolderConversation, "conversation:claude-code/alice:9f2c-41", "", "", "claude-code", "alice:9f2c-41"},
		{sess, HolderSession, "session:01SESSIONX", "", "01SESSIONX", "", ""},
	} {
		require.Equal(t, c.kind, c.h.Kind())
		require.Equal(t, c.key, c.h.Key())
		require.Equal(t, c.key, c.h.String())
		require.Equal(t, c.pk, c.h.ProcessKey())
		require.Equal(t, c.sid, c.h.SessionID())
		cli, ext := c.h.Conversation()
		require.Equal(t, c.cli+"/"+c.extID, cli+"/"+ext)
		back, err := HolderFromKey(c.key)
		require.NoError(t, err)
		require.Equal(t, c.h, back, "a key parses back to its holder")
		require.False(t, back.IsZero())
	}

	// One identity under two kinds is two holders.
	asSession, err := HolderOfSession("boot-1:4242:7")
	require.NoError(t, err)
	require.NotEqual(t, proc.Key(), asSession.Key())
	// An external id may hold a slash: the client never does, so the first one
	// ends it and no two conversations share a key.
	slashed, err := HolderOfConversation("codex", "a/b")
	require.NoError(t, err)
	back, err := HolderFromKey(slashed.Key())
	require.NoError(t, err)
	require.Equal(t, slashed, back)

	var zero Holder
	require.True(t, zero.IsZero())
	require.Empty(t, zero.Key())

	for name, build := range map[string]func() (Holder, error){
		"an empty process":      func() (Holder, error) { return HolderOfProcess("") },
		"a process with space":  func() (Holder, error) { return HolderOfProcess("boot 1:2:3") },
		"an empty client":       func() (Holder, error) { return HolderOfConversation("", "x") },
		"a client with a slash": func() (Holder, error) { return HolderOfConversation("claude/code", "x") },
		"a long client":         func() (Holder, error) { return HolderOfConversation(strings.Repeat("c", holderMaxClientBytes+1), "x") },
		"an empty external id":  func() (Holder, error) { return HolderOfConversation("codex", "") },
		"a control character":   func() (Holder, error) { return HolderOfConversation("codex", "a\x00b") },
		"invalid UTF-8":         func() (Holder, error) { return HolderOfSession("\xff") },
		"a long session id":     func() (Holder, error) { return HolderOfSession(strings.Repeat("s", maxAccessFieldBytes+1)) },
		"no kind":               func() (Holder, error) { return HolderFromKey("01SESSIONX") },
		"an unknown kind":       func() (Holder, error) { return HolderFromKey("account:alice") },
		"no client":             func() (Holder, error) { return HolderFromKey("conversation:abc") },
		"an empty key":          func() (Holder, error) { return HolderFromKey("") },
	} {
		_, err := build()
		require.ErrorIs(t, err, ErrInvalidAccessRequest, name)
	}

	// A row's holder: the process, else the conversation, else the session.
	require.Equal(t, proc, holderOfRow("boot-1:4242:7", "claude-code", "x", "S"))
	require.Equal(t, HolderConversation, holderOfRow("", "claude-code", "x", "S").Kind())
	require.Equal(t, HolderSession, holderOfRow("", "", "", "S").Kind())
	require.True(t, holderOfRow("", "", "", "").IsZero())
	require.True(t, holderOfRow("", "claude/code", "x", "S").IsZero(), "a row whose holder does not parse holds nothing")
}

// access_grants and access_approvals rows are found by the holder each one
// names: a conversation's grant never answers for the session that asked, and
// a session's never for a conversation.
func TestHolderAccessGrants_ByTheHolderEachRowNames(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	approve := func(req GrantRequest) AccessGrant {
		t.Helper()
		a, err := CreateGrantRequest(ctx, db, req, migration039Now)
		require.NoError(t, err)
		g, err := ApproveAccessGrant(ctx, db, a.ID, a.Challenge, "bearer-key", 0, migration039Now)
		require.NoError(t, err)
		return g
	}
	bySession := approve(grantReq("S1"))
	conv := grantReq("S1")
	conv.ConversationClient, conv.ConversationID = "claude-code", "ext-conv-1"
	conv.Project = "example-legal"
	byConv := approve(conv)
	proc := grantReq("S1")
	proc.ProcessKey, proc.PeerPID, proc.PeerUID = "boot-1:200:2000", 200, 501
	proc.Project = "example-hr"
	byProc := approve(proc)

	ids := func(holders ...Holder) []string {
		t.Helper()
		got, err := HolderAccessGrants(ctx, db, holders)
		require.NoError(t, err)
		var out []string
		for _, g := range got {
			out = append(out, g.ID)
		}
		return out
	}
	sess, err := HolderOfSession("S1")
	require.NoError(t, err)
	p, err := HolderOfProcess("boot-1:200:2000")
	require.NoError(t, err)
	other, err := HolderOfProcess("boot-1:300:3000")
	require.NoError(t, err)
	cv := migration039Conversation(t)
	require.Equal(t, []string{bySession.ID}, ids(sess))
	require.Equal(t, []string{byConv.ID}, ids(cv))
	require.Equal(t, []string{byProc.ID}, ids(other, p), "a chain finds its anchoring process's grant")
	require.ElementsMatch(t, []string{bySession.ID, byConv.ID, byProc.ID}, ids(sess, cv, p))
	require.Empty(t, ids())
	require.Empty(t, ids(Holder{}))
	require.Equal(t, cv, byConv.Holder())

	// The lookup today's grant judging uses is unchanged: it reads every row
	// with no process by the session that asked, a conversation's included,
	// exactly as the previous release does.
	legacy, err := AccessGrantsForHolder(ctx, db, nil, "S1")
	require.NoError(t, err)
	require.Len(t, legacy, 2)

	// Approving a conversation's request again replaces the conversation's
	// grant, never the session's.
	again := approve(conv)
	first, err := AccessGrantByID(ctx, db, byConv.ID)
	require.NoError(t, err)
	require.Equal(t, LeaseRevokedRefreshed, first.RevokedReason)
	kept, err := AccessGrantByID(ctx, db, bySession.ID)
	require.NoError(t, err)
	require.Nil(t, kept.RevokedAt)
	require.ElementsMatch(t, []string{again.ID, byConv.ID}, ids(cv))

	reqs, err := HolderAccessApprovals(ctx, db, []Holder{cv})
	require.NoError(t, err)
	require.Len(t, reqs, 2)
	for _, r := range reqs {
		require.Equal(t, cv, r.Holder())
		require.Equal(t, AccessKindGrant, r.Kind)
	}
	reqs, err = HolderAccessApprovals(ctx, db, nil)
	require.NoError(t, err)
	require.Empty(t, reqs)
}

// The label is every fenced project a conversation is or was homed in, every
// grant target and every project it moved through -- fenced as the projects
// stand now -- and nothing takes an entry out of it.
func TestHolderLabel_HomesGrantsAndMoves(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := migration039Now
	for slug, state := range map[string]core.Isolation{
		"example-ops": core.IsolationOpen, "example-hr": core.IsolationOpen,
		"example-dataroom": core.IsolationConfidential, "example-finance": core.IsolationConfidential,
		"example-legal": core.IsolationConfidential, "example-vault": core.IsolationSealed,
	} {
		require.NoError(t, CreateProject(ctx, db, core.Project{ID: "01" + slug, Slug: slug, Name: slug, Isolation: state,
			CreatedAt: now, UpdatedAt: now}))
	}
	conv := migration039Conversation(t)
	cl, ext := conv.Conversation()
	// The conversation's ambient row is in the data room, and it once linked an
	// explicit session to finance.
	require.NoError(t, CreateSession(ctx, db, core.Session{ID: "01SESSHOME", Name: "cc/room", ProjectSlug: "example-dataroom",
		Status: core.SessionActive, ExternalClient: cl, ExternalSessionID: ext, Ambient: true, CreatedAt: now, UpdatedAt: now}))
	require.NoError(t, CreateSession(ctx, db, core.Session{ID: "01SESSLINKED", Name: "sess/fin", ProjectSlug: "example-finance",
		Status: core.SessionCompleted, ExternalClient: cl, ExternalSessionID: ext, CreatedAt: now, UpdatedAt: now}))

	label := func(home string, holders ...Holder) map[string]HolderLabelEntry {
		t.Helper()
		got, err := HolderLabel(ctx, db, holders, home)
		require.NoError(t, err)
		out := map[string]HolderLabelEntry{}
		var order []string
		for _, e := range got {
			out[e.Project] = e
			order = append(order, e.Project)
		}
		require.IsNonDecreasing(t, order, "sorted by project")
		return out
	}

	got := label("example-ops", conv)
	require.Equal(t, map[string]HolderLabelEntry{
		"example-dataroom": {Project: "example-dataroom", State: core.IsolationConfidential, Home: true},
		"example-finance":  {Project: "example-finance", State: core.IsolationConfidential, Home: true},
	}, got, "its sessions' fenced projects; the open home it names is not fenced")

	// A read grant of legal from the data room, then a move into the vault.
	_, lg := flowApproved(t, db, flowGrantReq(t, "example-legal", "example-dataroom", "",
		[]string{"example-dataroom", "example-legal"}, []string{"example-dataroom"}), GrantAccessRead, now)
	_, mv := moveApproved(t, db, moveReq("example-vault", "example-dataroom", "example-dataroom", "example-legal"))
	got = label("example-vault", conv)
	require.Equal(t, []string{"example-dataroom", "example-finance", "example-legal", "example-vault"}, holderLabelKeys(got))
	require.Equal(t, HolderLabelEntry{Project: "example-legal", State: core.IsolationConfidential, Grants: []string{lg.ID}}, got["example-legal"])
	require.Equal(t, HolderLabelEntry{Project: "example-vault", State: core.IsolationSealed, Home: true, Moves: []string{mv.ID}}, got["example-vault"])
	require.Equal(t, []string{mv.ID}, got["example-dataroom"].Moves)

	// Revoking the grant ends its reads, never its entry.
	require.NoError(t, RevokeAccessGrant(ctx, db, lg.ID, LeaseRevokedOwner, now.Add(time.Minute)))
	require.Contains(t, label("", conv), "example-legal")
	// A project loosened to open holds nothing fenced any more.
	require.NoError(t, SetProjectIsolation(ctx, db, "example-finance", core.IsolationOpen))
	require.NotContains(t, label("", conv), "example-finance")

	// A session holder's label is its own session's project and its grants';
	// a process holder's is its grants', its moves' and the home it is told.
	sess, err := HolderOfSession("01SESSLINKED")
	require.NoError(t, err)
	require.Empty(t, label("", sess), "finance is open now, and the session took no grant")
	proc, err := HolderOfProcess("boot-1:200:2000")
	require.NoError(t, err)
	require.Equal(t, []string{"example-legal"}, holderLabelKeys(label("example-legal", proc)))
	require.Empty(t, label("", proc))
	require.Empty(t, label(""))
}

func holderLabelKeys(m map[string]HolderLabelEntry) []string { return slices.Sorted(maps.Keys(m)) }
