package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// The conversation page's store half (plan fenced-sessions P10): a
// conversation's sessions, and what they asked for and were given.

// convNow is the fixture's clock.
var convNow = time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)

// convDB opens a fresh database at a path (OpenView needs one) with the
// fixture's projects: example-ops open, example-finance and example-legal
// confidential.
func convDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "seam.db")
	db, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	for slug, state := range map[string]core.Isolation{"example-ops": core.IsolationOpen,
		"example-finance": core.IsolationConfidential, "example-legal": core.IsolationConfidential} {
		_, err := EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
		require.NoError(t, SetProjectIsolation(ctx, db, slug, state))
	}
	return db, path
}

// convSession records a session of the conversation external (client
// claude-code; "" for a session with no external identity) in project,
// created at convNow plus offset.
func convSession(t *testing.T, db *sql.DB, id, name, project, external string, ambient bool, offset time.Duration) core.Session {
	t.Helper()
	s := core.Session{ID: id, Name: name, ProjectSlug: project, Status: core.SessionActive, Ambient: ambient,
		CreatedAt: convNow.Add(offset), UpdatedAt: convNow.Add(offset)}
	if external != "" {
		s.ExternalClient, s.ExternalSessionID = "claude-code", external
	}
	require.NoError(t, CreateSession(context.Background(), db, s))
	return s
}

// convMove moves the conversation conv-a from its ambient row in example-ops
// into example-finance, as the approve core does, and returns the child.
func convMove(t *testing.T, db *sql.DB) (AccessApproval, MoveApproved) {
	t.Helper()
	ctx := context.Background()
	carry, err := CarryOf("example-ops", []string{"example-finance"}, []string{"example-finance"})
	require.NoError(t, err)
	a, err := MoveRequestCreate(ctx, db, MoveRequest{Project: "example-finance", From: "example-ops", Carry: carry,
		Lease: time.Hour, TTL: 10 * time.Minute, ConversationClient: "claude-code", ConversationID: "conv-a",
		SessionID: "01CONVHOME", SessionName: "cc/conv-a"}, convNow.Add(2*time.Minute))
	require.NoError(t, err)
	done, err := MoveApproveWith(ctx, db, MoveDecision{ID: a.ID, Challenge: a.Challenge, DecidedBy: "bearer-key"}, convNow.Add(3*time.Minute))
	require.NoError(t, err)
	return a, done
}

func convIDs(sessions []core.Session) []string {
	out := make([]string, len(sessions))
	for i, s := range sessions {
		out[i] = s.ID
	}
	return out
}

func TestConversationSessionsOf_EverySessionOfItsIdentityOldestFirst(t *testing.T) {
	db, path := convDB(t)
	ctx := context.Background()
	home := convSession(t, db, "01CONVHOME", "cc/conv-a", "example-ops", "conv-a", true, 0)
	link := convSession(t, db, "01CONVLINK", "sess/link", "example-legal", "conv-a", false, time.Minute)
	other := convSession(t, db, "01CONVOTHER", "cc/conv-b", "example-ops", "conv-b", true, time.Minute)
	bare := convSession(t, db, "01CONVBARE", "sess/bare", "example-ops", "", false, time.Minute)

	got, err := ConversationSessionsOf(ctx, db, home)
	require.NoError(t, err)
	require.Equal(t, []string{home.ID, link.ID}, convIDs(got))
	got, err = ConversationSessionsOf(ctx, db, link)
	require.NoError(t, err)
	require.Equal(t, []string{home.ID, link.ID}, convIDs(got), "a link's conversation is its ambient row's")
	got, err = ConversationSessionsOf(ctx, db, other)
	require.NoError(t, err)
	require.Equal(t, []string{other.ID}, convIDs(got))
	got, err = ConversationSessionsOf(ctx, db, bare)
	require.NoError(t, err)
	require.Equal(t, []string{bare.ID}, convIDs(got), "a session with no external identity is a conversation of its own")

	_, done := convMove(t, db)
	got, err = ConversationSessionsOf(ctx, db, done.Child)
	require.NoError(t, err)
	require.Equal(t, []string{home.ID, link.ID, done.Child.ID}, convIDs(got), "a move's child shares the identity")
	require.Equal(t, home.ID, got[2].ParentSessionID)

	view, err := OpenView(path, []string{"example-finance"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = view.Close() })
	got, err = ConversationSessionsOf(ctx, view, home)
	require.NoError(t, err)
	require.Equal(t, []string{home.ID, link.ID}, convIDs(got), "a hidden project's session is not there")
}

func TestConversationRecordOf_WhatItsSessionsAskedForAndWereGiven(t *testing.T) {
	db, _ := convDB(t)
	ctx := context.Background()
	home := convSession(t, db, "01CONVHOME", "cc/conv-a", "example-ops", "conv-a", true, 0)
	other := convSession(t, db, "01CONVOTHER", "cc/conv-b", "example-legal", "conv-b", true, 0)

	rec, err := ConversationRecordOf(ctx, db, []string{home.ID})
	require.NoError(t, err)
	require.True(t, rec.IsZero(), "nothing asked, nothing moved")
	rec, err = ConversationRecordOf(ctx, db, nil)
	require.NoError(t, err)
	require.True(t, rec.IsZero())

	move, done := convMove(t, db)
	child := done.Child

	// The child asks for a read of example-legal, approved read and write.
	carry, err := CarryOf("example-finance", []string{"example-finance", "example-legal"}, []string{"example-finance", "example-legal"})
	require.NoError(t, err)
	ask, err := CreateGrantRequest(ctx, db, GrantRequest{Project: "example-legal", From: "example-finance", Lease: time.Hour,
		TTL: 10 * time.Minute, SessionID: child.ID, SessionName: child.Name, Access: GrantAccessWrite, Carry: carry,
		ConversationClient: "claude-code", ConversationID: "conv-a"}, convNow.Add(4*time.Minute))
	require.NoError(t, err)
	grant, err := GrantAccessApprove(ctx, db, ask.ID, ask.Challenge, "in-session-dialog", 0, GrantAccessWrite, convNow.Add(5*time.Minute))
	require.NoError(t, err)
	// It asks again, and that one waits.
	pending, err := CreateGrantRequest(ctx, db, GrantRequest{Project: "example-legal", From: "example-finance", Lease: time.Hour,
		TTL: 10 * time.Minute, SessionID: child.ID, SessionName: child.Name, ConversationClient: "claude-code", ConversationID: "conv-a"},
		convNow.Add(6*time.Minute))
	require.NoError(t, err)
	// Another conversation's grant is not this one's.
	theirs, err := CreateGrantRequest(ctx, db, GrantRequest{Project: "example-finance", From: "example-legal", Lease: time.Hour,
		TTL: 10 * time.Minute, SessionID: other.ID, SessionName: other.Name, ConversationClient: "claude-code", ConversationID: "conv-b"},
		convNow.Add(4*time.Minute))
	require.NoError(t, err)
	_, err = GrantAccessApprove(ctx, db, theirs.ID, theirs.Challenge, "bearer-key", 0, GrantAccessRead, convNow.Add(5*time.Minute))
	require.NoError(t, err)

	sessions, err := ConversationSessionsOf(ctx, db, child)
	require.NoError(t, err)
	rec, err = ConversationRecordOf(ctx, db, convIDs(sessions))
	require.NoError(t, err)
	require.Len(t, rec.Moves, 1)
	require.Equal(t, done.Move.ID, rec.Moves[0].ID)
	require.Equal(t, "example-finance", rec.Moves[0].ToProject)
	require.Equal(t, "bearer-key", rec.Moves[0].DecidedBy)
	var asked []string
	for _, a := range rec.Approvals {
		asked = append(asked, a.ID)
	}
	require.Equal(t, []string{move.ID, ask.ID, pending.ID}, asked, "every request its sessions filed, oldest first")
	require.Len(t, rec.Grants, 1)
	require.Equal(t, grant.ID, rec.Grants[0].ID)
	require.Equal(t, GrantAccessWrite, rec.Grants[0].Access)
	require.Equal(t, "in-session-dialog", rec.Grants[0].DecidedBy)

	// Read with the child out of reach -- a locked project the handle hides --
	// the move into it and its request stay (they name the session it left),
	// and what the child asked for there does not.
	rec, err = ConversationRecordOf(ctx, db, []string{home.ID})
	require.NoError(t, err)
	require.Len(t, rec.Moves, 1)
	require.Len(t, rec.Approvals, 1)
	require.Equal(t, move.ID, rec.Approvals[0].ID)
	require.Empty(t, rec.Grants)
	// From the child alone, the move that entered it and its request.
	rec, err = ConversationRecordOf(ctx, db, []string{child.ID})
	require.NoError(t, err)
	require.Len(t, rec.Moves, 1)
	require.Equal(t, move.ID, rec.Approvals[0].ID, "the request behind the move, though the session that asked is not listed")
}

func TestAnyProvenanceStamp(t *testing.T) {
	db, path := convDB(t)
	ctx := context.Background()
	got, err := AnyProvenanceStamp(ctx, db)
	require.NoError(t, err)
	require.False(t, got)
	require.NoError(t, ProvenanceUpsert(ctx, db, ProvenanceStamp{ItemKind: core.ItemKindTask, ItemID: "t-1",
		Project: "example-finance", Source: "example-legal", ApprovalID: "01A"}, convNow))
	got, err = AnyProvenanceStamp(ctx, db)
	require.NoError(t, err)
	require.True(t, got)
	view, err := OpenView(path, []string{"example-finance"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = view.Close() })
	got, err = AnyProvenanceStamp(ctx, view)
	require.NoError(t, err)
	require.False(t, got, "the only stamp is in a project the handle hides")
}
