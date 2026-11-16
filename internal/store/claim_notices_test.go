package store

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
)

// clNotice is a valid reclaim notice for session, lost at lost.
func clNotice(session string, lost time.Time) ClaimNotice {
	lease := lost.Add(-time.Minute)
	return ClaimNotice{
		SessionID: session, Kind: ClaimResourceTask, ResourceID: "01TASKAAAAAAAAAAAAAAAAAAAA", Project: "example-ops",
		NewHolder: "01SESSIONBBBBBBBBBBBBBBBBB", Reason: ClaimLostReclaimed, LeaseExpiresAt: &lease, LostAt: lost,
	}
}

func TestRecordClaimNotice_RefusesANoticeThatCannotSayWhatHappened(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	now := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		spoil func(*ClaimNotice)
	}{
		{"no session", func(n *ClaimNotice) { n.SessionID = " " }},
		{"unknown kind", func(n *ClaimNotice) { n.Kind = "file" }},
		{"no resource", func(n *ClaimNotice) { n.ResourceID = "" }},
		{"unknown reason", func(n *ClaimNotice) { n.Reason = "stolen" }},
		{"no time", func(n *ClaimNotice) { n.LostAt = time.Time{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := clNotice("sessA", now)
			tc.spoil(&n)
			require.ErrorIs(t, RecordClaimNotice(ctx, db, n), ErrInvalidClaimNotice)
		})
	}
	var rows int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM claim_notices`).Scan(&rows))
	require.Zero(t, rows)

	n := clNotice("sessA", now)
	n.LeaseExpiresAt = nil
	require.NoError(t, RecordClaimNotice(ctx, db, n), "a claim that carried no lease is still news")
	pending, err := PendingClaimNotices(ctx, db, "sessA")
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.NotEmpty(t, pending[0].ID, "the id is minted")
	require.Nil(t, pending[0].LeaseExpiresAt)
}

// A notice is delivered once: the claim of its row is the delivery, so a
// second call, a call for several sessions, and a reopened database find
// nothing more to carry.
func TestDeliverClaimNotices_Once(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "seam.db")
	db, err := OpenFresh(t, path)
	require.NoError(t, err)
	now := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	require.NoError(t, RecordClaimNotice(ctx, db, clNotice("sessA", now.Add(2*time.Minute))))
	require.NoError(t, RecordClaimNotice(ctx, db, clNotice("sessA", now.Add(time.Minute))))
	require.NoError(t, RecordClaimNotice(ctx, db, clNotice("sessB", now)))

	none, err := DeliverClaimNotices(ctx, db, nil, now)
	require.NoError(t, err)
	require.Empty(t, none)

	got, err := DeliverClaimNotices(ctx, db, []string{"sessA", "sessA", ""}, now.Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.True(t, got[0].LostAt.Before(got[1].LostAt), "oldest loss first")
	for _, n := range got {
		require.Equal(t, "sessA", n.SessionID)
		require.NotNil(t, n.DeliveredAt)
		require.True(t, now.Add(time.Hour).Equal(*n.DeliveredAt))
	}
	again, err := DeliverClaimNotices(ctx, db, []string{"sessA"}, now.Add(2*time.Hour))
	require.NoError(t, err)
	require.Empty(t, again, "a delivered notice is not delivered again")

	both, err := DeliverClaimNotices(ctx, db, []string{"sessA", "sessB"}, now.Add(2*time.Hour))
	require.NoError(t, err)
	require.Len(t, both, 1)
	require.Equal(t, "sessB", both[0].SessionID)

	// A restart: the claims are rows, so a new handle finds nothing pending.
	require.NoError(t, RecordClaimNotice(ctx, db, clNotice("sessC", now)))
	require.NoError(t, db.Close())
	reopened, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = reopened.Close() })
	after, err := DeliverClaimNotices(ctx, reopened, []string{"sessA", "sessB", "sessC"}, now.Add(3*time.Hour))
	require.NoError(t, err)
	require.Len(t, after, 1, "only the notice no one carried yet")
	require.Equal(t, "sessC", after[0].SessionID)
	pending, err := PendingClaimNotices(ctx, reopened, "sessC")
	require.NoError(t, err)
	require.Empty(t, pending)
}

// Of concurrent deliveries for one session -- a prompt hook racing a tool
// call -- each notice reaches exactly one.
func TestDeliverClaimNotices_ConcurrentCallsCarryEachOnce(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	now := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	const notices = 5
	for i := range notices {
		require.NoError(t, RecordClaimNotice(ctx, db, clNotice("sessA", now.Add(time.Duration(i)*time.Second))))
	}
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		seen  = map[string]int{}
		start = make(chan struct{})
	)
	for range 8 {
		wg.Go(func() {
			<-start
			got, err := DeliverClaimNotices(ctx, db, []string{"sessA"}, now)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				t.Errorf("deliver: %v", err)
				return
			}
			for _, n := range got {
				seen[n.ID]++
			}
		})
	}
	close(start)
	wg.Wait()
	require.Len(t, seen, notices)
	for id, n := range seen {
		require.Equal(t, 1, n, "notice %s", id)
	}
}

// The words: the claim, the time, the new holder or "another session", and
// what to do. Identifiers and times only.
func TestClaimNoticeText(t *testing.T) {
	lost := time.Date(2026, 11, 15, 9, 20, 0, 0, time.UTC)
	lease := time.Date(2026, 11, 15, 9, 5, 0, 0, time.UTC)
	base := ClaimNotice{SessionID: "sessA", Kind: ClaimResourceTask, ResourceID: "01TASKAAAAAAAAAAAAAAAAAAAA",
		Project: "example-ops", LeaseExpiresAt: &lease, LostAt: lost}

	reclaimed := base
	reclaimed.Reason, reclaimed.NewHolder = ClaimLostReclaimed, "01SESSIONB"
	require.Equal(t, "Claim lost: your claim on task 01TASKAAAAAAAAAAAAAAAAAAAA in project example-ops lapsed at "+
		access.Clock(lease)+", and session cc/b claimed it at "+access.Clock(lost)+". Pick other work, or claim it "+
		"again with tasks_claim once it is free. To keep a claim through long work, re-claim it before its lease lapses.",
		reclaimed.Text("cc/b"))
	require.Contains(t, reclaimed.Text(""), ", and another session claimed it at ")

	released := base
	released.Reason = ClaimLostReleased
	require.Equal(t, "Claim lost: the owner released your claim on task 01TASKAAAAAAAAAAAAAAAAAAAA in project "+
		"example-ops at "+access.Clock(lost)+". Claim it again with tasks_claim if it is still free, or pick other work.",
		released.Text(""))

	reaped := base
	reaped.Reason, reaped.Project, reaped.LeaseExpiresAt = ClaimLostReaped, "", nil
	require.Equal(t, "Claim lost: your claim on task 01TASKAAAAAAAAAAAAAAAAAAAA in the global scope had no lease, "+
		"and it was released at "+access.Clock(lost)+", when this session had gone idle. Claim it again with "+
		"tasks_claim if it is still free, or pick other work. To keep a claim through long work, re-claim it before "+
		"its lease lapses.", reaped.Text(""))

	labeled := base
	labeled.Reason, labeled.Kind, labeled.Label = ClaimLostReleased, "lock", "example-label"
	require.Contains(t, labeled.Text(""), "your claim on lock 01TASKAAAAAAAAAAAAAAAAAAAA (example-label) in project example-ops")
	require.Contains(t, labeled.Text(""), "Claim it again if it is still free", "only a task names tasks_claim")
}

// The new holder is named only to a request that may see its session: one its
// own principal started, in a project it may see. Any other -- and one that
// cannot be found -- is "another session".
func TestClaimNoticeTexts_NamesTheNewHolderOnlyWhenVisible(t *testing.T) {
	db := openTestDB(t)
	owner := context.Background()
	acct := restrictedCtx(t, "example-open")
	now := time.Now().UTC()
	mk := func(ctx context.Context, name, project string) string {
		t.Helper()
		id, err := core.NewID()
		require.NoError(t, err)
		require.NoError(t, CreateSession(ctx, db, core.Session{ID: id, Name: name, ProjectSlug: project,
			Status: core.SessionActive, CreatedAt: now, UpdatedAt: now}))
		return id
	}
	ownersSession := mk(owner, "cc/owner-agent", "example-open")
	acctOpen := mk(acct, "cc/acct-open", "example-open")
	acctClosed := mk(acct, "cc/acct-closed", "example-closed")
	notice := func(holder string) []ClaimNotice {
		n := clNotice("sessA", now)
		n.NewHolder = holder
		return []ClaimNotice{n}
	}
	for _, tc := range []struct {
		name   string
		ctx    context.Context
		holder string
		want   string
	}{
		{"the owner sees the owner's session", owner, ownersSession, "session cc/owner-agent claimed it"},
		{"an account sees its own session", acct, acctOpen, "session _example_agent:cc/acct-open claimed it"},
		{"an account does not see the owner's", acct, ownersSession, "another session claimed it"},
		{"nor its own in a project it may not see", acct, acctClosed, "another session claimed it"},
		{"nor a session that is not there", owner, "01NOSUCHSESSIONAAAAAAAAAAA", "another session claimed it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			texts := ClaimNoticeTexts(tc.ctx, db, notice(tc.holder))
			require.Len(t, texts, 1)
			require.Contains(t, texts[0], tc.want)
		})
	}
}

// The question every tool call and prompt asks -- is anything waiting? --
// searches the partial index of pending notices rather than scanning the table.
func TestDeliverClaimNotices_AsksThePendingIndex(t *testing.T) {
	db := openTestDB(t)
	rows, err := db.Query(`EXPLAIN QUERY PLAN SELECT EXISTS (SELECT 1 FROM claim_notices
		WHERE session_id IN (?, ?) AND delivered_at IS NULL)`, "sessA", "sessB")
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var plan []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &notused, &detail))
		plan = append(plan, detail)
	}
	require.NoError(t, rows.Err())
	joined := strings.Join(plan, "\n")
	require.Contains(t, joined, "idx_claim_notices_pending", joined)
	require.NotContains(t, joined, "SCAN claim_notices", joined)
}

// The console's filtered handle hides a notice about a hidden project, about a
// task in one, and one whose holder or taker is a session there.
func TestClaimNotices_HiddenByTheFilteredView(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "seam.db")
	db, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	now := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	hiddenSession := clSession(t, db, "cc/fin", "example-finance", core.SessionActive, now)
	shownSession := clSession(t, db, "cc/ops", "example-ops", core.SessionActive, now)
	hiddenTask := clClaim(t, db, "example-finance", shownSession, time.Minute, now.Add(-time.Hour))

	record := func(n ClaimNotice) {
		t.Helper()
		require.NoError(t, RecordClaimNotice(ctx, db, n))
	}
	shown := clNotice(shownSession, now)
	shown.NewHolder = ""
	record(shown) // about example-ops, nobody hidden
	inHidden := clNotice(shownSession, now)
	inHidden.Project, inHidden.NewHolder = "example-finance", ""
	record(inHidden) // about a hidden project
	ofHiddenTask := clNotice(shownSession, now)
	ofHiddenTask.ResourceID, ofHiddenTask.NewHolder = hiddenTask, ""
	record(ofHiddenTask) // about a hidden project's task
	heldThere := clNotice(hiddenSession, now)
	heldThere.NewHolder = ""
	record(heldThere) // its holder is a hidden session
	takenThere := clNotice(shownSession, now)
	takenThere.NewHolder = hiddenSession
	record(takenThere) // its taker is a hidden session

	view, err := OpenView(path, []string{"example-finance"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = view.Close() })
	var n int
	require.NoError(t, view.QueryRowContext(ctx, `SELECT COUNT(*) FROM claim_notices`).Scan(&n))
	require.Equal(t, 1, n, "only the notice about nothing hidden")
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM claim_notices`).Scan(&n))
	require.Equal(t, 5, n)
}
