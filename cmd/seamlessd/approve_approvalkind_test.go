package main

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// seamlessd approve decides the new kinds with the daemon account's proof: a
// grant read-only or, with --access, read and write; a move, into its child
// session, with its lock's lease when the project is locked.

func kindDB(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	for slug, state := range map[string]core.Isolation{"example-ops": core.IsolationOpen,
		"example-hr": core.IsolationConfidential, "example-payroll": core.IsolationConfidential} {
		_, err := store.EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
		require.NoError(t, store.SetProjectIsolation(ctx, db, slug, state))
	}
	return db
}

func TestApprove_ReadAndWriteGrantRequest(t *testing.T) {
	ctx := context.Background()
	db := kindDB(t)
	carry, err := store.CarryOf("example-hr", []string{"example-hr", "example-payroll"}, []string{"example-hr", "example-payroll"})
	require.NoError(t, err)
	a, err := store.CreateGrantRequest(ctx, db, store.GrantRequest{Project: "example-payroll", From: "example-hr",
		Lease: 8 * time.Hour, TTL: 10 * time.Minute, PeerUID: 501, PeerPID: 200, ProcessKey: "boot:200:2000",
		SessionID: "01KSESSION", SessionName: "cc/hr", Access: store.GrantAccessWrite, Carry: carry}, time.Now())
	require.NoError(t, err)
	svc := approval.New(approval.Config{DB: db, LeaseCap: 8 * time.Hour})
	grants := approval.NewGrants(approval.GrantsConfig{Approvals: svc})
	facts, err := svc.Facts(ctx, a.ID)
	require.NoError(t, err)
	short := approval.ShortID(a.ID)

	var out bytes.Buffer
	require.ErrorIs(t, approveWith(ctx, svc, grants, &out, []string{short, "--for", "1h", "--digest", facts.Digest, "--access", "write"}),
		approval.ErrAccess)
	require.ErrorContains(t, approveWith(ctx, svc, grants, &out, []string{short, "--deny", "--access", "read-write"}),
		"--deny takes no --for, --digest or --access")
	require.NoError(t, approveWith(ctx, svc, grants, &out, []string{short, "--for", "1h", "--digest", facts.Digest, "--access", "read-write"}))
	require.Contains(t, out.String(), "Approved "+short+": pid 200, working in example-hr, may read and write example-payroll until ")
	require.Contains(t, out.String(), "; content from example-payroll may be copied into example-hr, and it may change "+
		"example-payroll's tasks, memories and notes; content from example-hr may be copied into example-payroll.\n")
	live, err := store.LiveAccessGrants(ctx, db, time.Now())
	require.NoError(t, err)
	require.Len(t, live, 1)
	require.Equal(t, store.GrantAccessWrite, live[0].Access)
}

func TestApprove_MoveRequest(t *testing.T) {
	ctx := context.Background()
	db := kindDB(t)
	now := time.Now().UTC()
	require.NoError(t, store.CreateSession(ctx, db, core.Session{ID: "01KSESSHOME", Name: "cc/ops", ProjectSlug: "example-ops",
		Status: core.SessionActive, Ambient: true, ExternalClient: "claude-code", ExternalSessionID: "ext-1",
		CreatedAt: now, UpdatedAt: now}))
	carry, err := store.CarryOf("example-ops", []string{"example-hr"}, []string{"example-hr"})
	require.NoError(t, err)
	a, err := store.MoveRequestCreate(ctx, db, store.MoveRequest{Project: "example-hr", From: "example-ops", Carry: carry,
		Lease: 8 * time.Hour, TTL: 10 * time.Minute, PeerUID: 501, PeerPID: 200, ProcessKey: "boot:200:2000",
		SessionID: "01KSESSHOME", SessionName: "cc/ops"}, now)
	require.NoError(t, err)
	svc := approval.New(approval.Config{DB: db, LeaseCap: 8 * time.Hour, ForceLocked: []string{"example-hr"}})
	facts, err := svc.Facts(ctx, a.ID)
	require.NoError(t, err)
	require.True(t, facts.Locked)
	short := approval.ShortID(a.ID)
	args := []string{short, "--for", "1h", "--digest", facts.Digest}

	var out bytes.Buffer
	require.ErrorContains(t, approve(ctx, svc, &out, args), "moves are off here")
	grants := approval.NewGrants(approval.GrantsConfig{Approvals: svc})
	require.NoError(t, approveWith(ctx, svc, grants, &out, args))
	require.Contains(t, out.String(), "Approved "+short+": pid 200 moved from example-ops into example-hr for good, as session "+
		"cc/ops@example-hr; its lock is open to it until ")
	amb, found, err := store.AmbientSessionByExternalIdentity(ctx, db, "claude-code", "ext-1")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "example-hr", amb.ProjectSlug)
	leases, err := store.LiveAccessLeases(ctx, db, time.Now())
	require.NoError(t, err)
	require.Len(t, leases, 1)
	require.Equal(t, amb.ID, leases[0].SessionID)
}

func TestApprove_DenyAMove(t *testing.T) {
	ctx := context.Background()
	db := kindDB(t)
	carry, err := store.CarryOf("", []string{"example-hr"}, []string{"example-hr"})
	require.NoError(t, err)
	a, err := store.MoveRequestCreate(ctx, db, store.MoveRequest{Project: "example-hr", Carry: carry, Lease: 8 * time.Hour,
		TTL: 10 * time.Minute, PeerUID: 501, PeerPID: 200, ProcessKey: "boot:200:2000", SessionID: "01KSESSHOME"}, time.Now())
	require.NoError(t, err)
	svc := approval.New(approval.Config{DB: db, LeaseCap: 8 * time.Hour})
	var out bytes.Buffer
	require.NoError(t, approve(ctx, svc, &out, []string{approval.ShortID(a.ID), "--deny"}))
	require.Equal(t, "Denied "+approval.ShortID(a.ID)+": the agent stays in the global scope, and example-hr stays closed to it.\n", out.String())
}
