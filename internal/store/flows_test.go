package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFlows_CarryNormalizesAndLists(t *testing.T) {
	c, err := CarryOf("example-ops", []string{"example-legal", "example-finance", "example-legal"}, []string{"example-ops", "example-finance"})
	require.NoError(t, err)
	require.Equal(t, Carry{Home: "example-ops", Holds: []string{"example-finance", "example-legal"},
		WriteTo: []string{"example-finance", "example-ops"}}, c)
	require.Equal(t, []FlowPair{
		{From: "example-finance", To: "example-ops"},
		{From: "example-legal", To: "example-finance"},
		{From: "example-legal", To: "example-ops"},
	}, c.Flows(), "each held project into each destination but itself")

	for name, in := range map[string][3]any{
		"an empty held slug":     {"h", []string{""}, nil},
		"a padded destination":   {"h", nil, []string{" example-ops"}},
		"a padded home":          {"h ", nil, nil},
		"too many held projects": {"h", carryMany(carryMaxProjects + 1), nil},
	} {
		_, err := CarryOf(in[0].(string), carryList(in[1]), carryList(in[2]))
		require.ErrorIs(t, err, ErrInvalidAccessRequest, name)
	}

	zero, err := carryJSON(Carry{})
	require.NoError(t, err)
	require.Empty(t, zero, "the zero carry is the column's empty default")
	back, err := carryFromJSON("")
	require.NoError(t, err)
	require.True(t, back.IsZero())
	enc, err := carryJSON(Carry{Home: "example-ops", Holds: []string{"b", "a", "a"}, WriteTo: []string{"example-ops"}})
	require.NoError(t, err)
	require.Equal(t, `{"home":"example-ops","holds":["a","b"],"write_to":["example-ops"]}`, enc, "encoded normalized, so equal carries encode equally")
	back, err = carryFromJSON(enc)
	require.NoError(t, err)
	require.Equal(t, Carry{Home: "example-ops", Holds: []string{"a", "b"}, WriteTo: []string{"example-ops"}}, back)
	sparse, err := CarryOf("example-ops", nil, nil)
	require.NoError(t, err)
	enc, err = carryJSON(sparse)
	require.NoError(t, err)
	require.Equal(t, `{"home":"example-ops","holds":[],"write_to":[]}`, enc, "an empty list is [], never null")
	back, err = carryFromJSON(`{"home":"example-ops","holds":null,"write_to":null}`)
	require.NoError(t, err)
	require.Equal(t, sparse, back)
	_, err = carryFromJSON(`[1, 2]`)
	require.Error(t, err)
	_, err = carryJSON(Carry{Holds: []string{""}})
	require.ErrorIs(t, err, ErrInvalidAccessRequest)

	// The owner's access decides whether the target is written.
	asked := Carry{Home: "h", Holds: []string{"h"}, WriteTo: []string{"h", "t"}}
	require.Equal(t, Carry{Home: "h", Holds: []string{"h", "t"}, WriteTo: []string{"h"}}, asked.with("t", false))
	require.Equal(t, Carry{Home: "h", Holds: []string{"h", "t"}, WriteTo: []string{"h", "t"}}, asked.with("t", true))
}

func carryMany(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "example-" + strings.Repeat("p", i+1)
	}
	return out
}

func carryList(v any) []string {
	if v == nil {
		return nil
	}
	return v.([]string)
}

// flowGrantReq is a grant request of target from the fixture conversation homed
// in home, stating carry.
func flowGrantReq(t *testing.T, target, home, asked string, holds, writeTo []string) GrantRequest {
	t.Helper()
	c, err := CarryOf(home, holds, writeTo)
	require.NoError(t, err)
	return GrantRequest{
		Project: target, From: home, Lease: time.Hour, TTL: 10 * time.Minute, Access: asked, Carry: c,
		ConversationClient: "claude-code", ConversationID: "ext-conv-1",
		SessionID: "01SESSHOME", SessionName: "cc/home", CWD: "/work/" + home, Repo: home, Model: "claude-opus-5-5",
	}
}

func flowApproved(t *testing.T, db *sql.DB, req GrantRequest, grantAccess string, at time.Time) (AccessApproval, AccessGrant) {
	t.Helper()
	ctx := context.Background()
	a, err := CreateGrantRequest(ctx, db, req, at)
	require.NoError(t, err)
	g, err := GrantAccessApprove(ctx, db, a.ID, a.Challenge, "bearer-key", 0, grantAccess, at)
	require.NoError(t, err)
	return a, g
}

func TestFlows_GrantRequestValidates(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	for name, mutate := range map[string]func(*GrantRequest){
		"an unknown access":         func(r *GrantRequest) { r.Access = "admin" },
		"half a conversation":       func(r *GrantRequest) { r.ConversationID = "" },
		"a conversation with space": func(r *GrantRequest) { r.ConversationID = "ext conv" },
		"another home":              func(r *GrantRequest) { r.Carry.Home = "example-hr" },
		"not holding the target":    func(r *GrantRequest) { r.Carry.Holds = []string{"example-dataroom"} },
		"write, not writing to it": func(r *GrantRequest) {
			r.Access, r.Carry.WriteTo = GrantAccessWrite, []string{"example-dataroom"}
		},
	} {
		req := flowGrantReq(t, "example-legal", "example-dataroom", "", []string{"example-dataroom", "example-legal"}, []string{"example-dataroom"})
		mutate(&req)
		_, err := CreateGrantRequest(ctx, db, req, migration039Now)
		require.ErrorIs(t, err, ErrInvalidAccessRequest, name)
	}
	req := flowGrantReq(t, "example-legal", "example-dataroom", GrantAccessWrite,
		[]string{"example-dataroom", "example-legal"}, []string{"example-dataroom", "example-legal"})
	a, err := CreateGrantRequest(ctx, db, req, migration039Now)
	require.NoError(t, err)
	require.Equal(t, GrantAccessWrite, a.RequestedAccess)
	require.Equal(t, req.Carry, a.Carry)
	require.Equal(t, migration039Conversation(t), a.Holder())
	require.Equal(t, "01SESSHOME", a.SessionID, "the session that asked is still named")
}

// The data room example: a conversation homed in a confidential data room with
// a write grant into finance asks to read legal, and the approval clears
// legal's content into both the data room and finance.
func TestFlows_GrantAccessApproveRecordsTheCarry(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	conv := migration039Conversation(t)
	room, fin, legal := "example-dataroom", "example-finance", "example-legal"

	writeReq, writeGrant := flowApproved(t, db, flowGrantReq(t, fin, room, GrantAccessWrite, []string{room, fin}, []string{room, fin}),
		GrantAccessWrite, migration039Now)
	require.Equal(t, GrantAccessWrite, writeGrant.Access)
	require.Equal(t, conv, writeGrant.Holder())
	stored, err := AccessGrantByID(ctx, db, writeGrant.ID)
	require.NoError(t, err)
	require.Equal(t, writeGrant, stored)

	readReq, _ := flowApproved(t, db, flowGrantReq(t, legal, room, "", []string{room, fin, legal}, []string{room, fin}),
		GrantAccessRead, migration039Now.Add(time.Minute))

	flows, err := FlowsForHolder(ctx, db, []Holder{conv})
	require.NoError(t, err)
	got := map[FlowPair]string{}
	for _, f := range flows {
		got[FlowPair{From: f.From, To: f.To}] = f.ApprovalID
	}
	require.Equal(t, map[FlowPair]string{
		{From: room, To: fin}:   writeReq.ID,
		{From: fin, To: room}:   writeReq.ID,
		{From: legal, To: room}: readReq.ID,
		{From: legal, To: fin}:  readReq.ID,
	}, got, "a flow listed again keeps the approval that first cleared it")

	judged, err := HolderFlows(ctx, db, []Holder{conv})
	require.NoError(t, err)
	require.Len(t, judged, 4, "the grants' own flows coincide with what their carries listed")
	for _, f := range judged {
		require.Equal(t, AccessKindGrant, f.ApprovalKind)
		require.Equal(t, got[FlowPair{From: f.From, To: f.To}], f.ApprovalID)
	}

	// The owner approves read where the agent asked write: the target is no
	// write destination, so nothing flows into it.
	askedWrite := flowGrantReq(t, "example-hr", room, GrantAccessWrite, []string{room, "example-hr"}, []string{room, "example-hr"})
	askedWrite.ConversationID = "ext-conv-2"
	_, readOnly := flowApproved(t, db, askedWrite, GrantAccessRead, migration039Now)
	require.Equal(t, GrantAccessRead, readOnly.Access)
	other, err := HolderOfConversation("claude-code", "ext-conv-2")
	require.NoError(t, err)
	flows, err = FlowsForHolder(ctx, db, []Holder{other})
	require.NoError(t, err)
	require.Len(t, flows, 1)
	require.Equal(t, FlowPair{From: "example-hr", To: room}, FlowPair{From: flows[0].From, To: flows[0].To})

	// ApproveAccessGrant binds no access and records no flow, carry or not.
	plain := flowGrantReq(t, legal, room, GrantAccessWrite, []string{room, legal}, []string{room, legal})
	plain.ConversationID = "ext-conv-3"
	pa, err := CreateGrantRequest(ctx, db, plain, migration039Now)
	require.NoError(t, err)
	pg, err := ApproveAccessGrant(ctx, db, pa.ID, pa.Challenge, "bearer-key", 0, migration039Now)
	require.NoError(t, err)
	require.Equal(t, GrantAccessRead, pg.Access, "a write request approved by the core that binds no access reads only")
	third, err := HolderOfConversation("claude-code", "ext-conv-3")
	require.NoError(t, err)
	flows, err = FlowsForHolder(ctx, db, []Holder{third})
	require.NoError(t, err)
	require.Empty(t, flows)

	_, err = GrantAccessApprove(ctx, db, pa.ID, pa.Challenge, "bearer-key", 0, "admin", migration039Now)
	require.ErrorIs(t, err, ErrInvalidAccessRequest)
}

// HolderFlows is what a fence judges: the recorded flows plus those every
// grant (live or ended) and every move implies, one per pair, never another
// holder's.
func TestFlows_HolderFlowsAddsWhatGrantsAndMovesImply(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	conv := migration039Conversation(t)

	// A grant from before migration 039: session-held, read, no carry.
	legacy, err := CreateGrantRequest(ctx, db, grantReq("S1"), migration039Now)
	require.NoError(t, err)
	lg, err := ApproveAccessGrant(ctx, db, legacy.ID, legacy.Challenge, "bearer-key", 0, migration039Now)
	require.NoError(t, err)
	sess, err := HolderOfSession("S1")
	require.NoError(t, err)
	got, err := HolderFlows(ctx, db, []Holder{sess})
	require.NoError(t, err)
	require.Equal(t, []HolderFlow{{From: grFin, To: grOps, ApprovalID: legacy.ID, ApprovalKind: AccessKindGrant,
		Via: HolderFlowGrant, At: lg.GrantedAt}}, got)

	// A write grant with no carry implies both directions between its two projects.
	wr := flowGrantReq(t, "example-legal", "example-dataroom", GrantAccessWrite, nil, nil)
	wr.Carry = Carry{}
	wa, err := CreateGrantRequest(ctx, db, wr, migration039Now)
	require.NoError(t, err)
	_, err = GrantAccessApprove(ctx, db, wa.ID, wa.Challenge, "bearer-key", 0, GrantAccessWrite, migration039Now)
	require.NoError(t, err)
	// A move from the data room into a sealed project, holding legal.
	ma, mv := moveApproved(t, db, moveReq("example-vault", "example-dataroom", "example-dataroom", "example-legal"))

	got, err = HolderFlows(ctx, db, []Holder{conv})
	require.NoError(t, err)
	pairs := map[FlowPair]string{}
	for _, f := range got {
		pairs[FlowPair{From: f.From, To: f.To}] = f.Via + " " + f.ApprovalID
	}
	require.Equal(t, map[FlowPair]string{
		{From: "example-legal", To: "example-dataroom"}: HolderFlowGrant + " " + wa.ID,
		{From: "example-dataroom", To: "example-legal"}: HolderFlowGrant + " " + wa.ID,
		{From: "example-dataroom", To: "example-vault"}: HolderFlowRecorded + " " + ma.ID,
		{From: "example-legal", To: "example-vault"}:    HolderFlowRecorded + " " + ma.ID,
	}, pairs)
	require.Equal(t, "example-dataroom", got[0].From, "sorted by source, then destination")
	require.Equal(t, mv.ApprovalID, ma.ID)

	// Revocation and expiry end reads, never flows.
	require.NoError(t, RevokeAccessGrant(ctx, db, lg.ID, LeaseRevokedOwner, migration039Now.Add(time.Minute)))
	got, err = HolderFlows(ctx, db, []Holder{sess})
	require.NoError(t, err)
	require.Len(t, got, 1, "a revoked grant does not unread what it read")

	// Holders never share flows; no holder has none.
	stranger, err := HolderOfConversation("codex", "ext-conv-1")
	require.NoError(t, err)
	got, err = HolderFlows(ctx, db, []Holder{stranger})
	require.NoError(t, err)
	require.Empty(t, got, "the same session id from another client is another conversation")
	got, err = HolderFlows(ctx, db, nil)
	require.NoError(t, err)
	require.Empty(t, got)
}
