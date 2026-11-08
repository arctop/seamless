package access

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The expiry warning starts at ExpiryWarning before a lease's end and names
// the absolute end, the minutes left and the way to ask again; a later lease
// on the same project (a refresh) silences it.
func TestExpiryNotice(t *testing.T) {
	end := lockNow.Add(time.Hour)
	fin := lockLease(lockFin, lockProc, end)
	for _, tc := range []struct {
		name   string
		now    time.Time
		leases []Lease
		want   string // "" = no notice
	}{
		{"T-31m: none", end.Add(-31 * time.Minute), []Lease{fin}, ""},
		{"T-30m: from here on", end.Add(-30 * time.Minute), []Lease{fin}, "(30 minutes left)"},
		{"T-29m", end.Add(-29 * time.Minute), []Lease{fin}, "(29 minutes left)"},
		{"T-29m30s rounds down", end.Add(-29*time.Minute - 30*time.Second), []Lease{fin}, "(29 minutes left)"},
		{"T-1m", end.Add(-time.Minute), []Lease{fin}, "(1 minute left)"},
		{"T-10s", end.Add(-10 * time.Second), []Lease{fin}, "(less than a minute left)"},
		{"ended: none", end, []Lease{fin}, ""},
		{"a later lease on the project carries on", end.Add(-10 * time.Minute),
			[]Lease{fin, {ID: "lease-2", Project: lockFin, Process: lockProc.Key(), Expires: end.Add(4 * time.Hour)}}, ""},
		{"no leases", lockNow, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ExpiryNotice(tc.leases, tc.now)
			if tc.want == "" {
				require.Empty(t, got)
				return
			}
			require.Contains(t, got, "Access to "+lockFin+" ends at "+Clock(end)+" "+tc.want)
			require.Contains(t, got, "run `seam access request "+lockFin+"` from your shell to ask the owner again (a new approval)")
			require.Contains(t, got, "Nothing extends it silently")
		})
	}

	// One line per expiring project, by project.
	both := ExpiryNotice([]Lease{
		lockLease(lockLegal, lockProc, lockNow.Add(20*time.Minute)),
		lockLease(lockFin, lockProc, lockNow.Add(10*time.Minute)),
		lockLease(lockOps, lockProc, lockNow.Add(2*time.Hour)),
	}, lockNow)
	ls := strings.Split(both, "\n")
	require.Len(t, ls, 2)
	require.Contains(t, ls[0], lockFin)
	require.Contains(t, ls[1], lockLegal)
}

// Every refusal at a lock is two lines: the rule, then the remedy -- naming
// the request when one was filed, and when access ended when it had.
func TestLockedRefusal_TwoLines(t *testing.T) {
	p := lockPolicy(t)
	sock := socketView(t, p, lockProc)
	ended := lockNow.Add(-5 * time.Minute)
	for _, tc := range []struct {
		name       string
		err        error
		rule, cure string
	}{
		{"the TCP key", p.OwnerView().Refusal(lockFin), "project " + lockFin + " is locked: the TCP connection and its bearer key never reach",
			"Remedy: work in it from an agent session"},
		{"the socket, no request", sock.Refusal(lockFin), "project " + lockFin + " is locked: even the owner's own agents",
			"Remedy: run `seam access request " + lockFin + "`"},
		{"a request", sock.RefusalFor(lockFin, LockRequest{ID: "01REQ", Short: "R-7F3K"}),
			"project " + lockFin + " is locked: even the owner's own agents need the owner's approval, and this session holds none.",
			"Remedy: access request R-7F3K is waiting -- the owner approves it in their own terminal with `seam approve R-7F3K`"},
		{"a refresh, with a page", sock.RefusalFor(lockFin, LockRequest{ID: "01REQ", Short: "R-7F3K", Refresh: true, Page: "https://example.test/console/access/01REQ"}),
			"is locked", "Remedy: refresh request R-7F3K is waiting -- the owner approves it in their own terminal with `seam approve R-7F3K`, or with a passkey at https://example.test/console/access/01REQ"},
		{"after a lease ended", sock.RefusalFor(lockFin, LockRequest{ID: "01REQ", Short: "R-7F3K", Ended: ended}),
			"project " + lockFin + " is locked (access ended at " + Clock(ended), "Remedy: access request R-7F3K is waiting"},
		{"no id: the plain refusal", sock.RefusalFor(lockFin, LockRequest{}), "even the owner's own agents", "Remedy: run `seam access request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.ErrorIs(t, tc.err, ErrNotReachable)
			ls := strings.Split(tc.err.Error(), "\n")
			require.Len(t, ls, 2, tc.err.Error())
			require.Contains(t, ls[0], tc.rule)
			require.Contains(t, ls[1], tc.cure)
			require.NotContains(t, tc.err.Error(), "sealed", "the lock is never called sealed")
		})
	}

	// RefusalFor on a project that is not locked is the plain refusal.
	legacy := lockLegacyPolicy(t)
	v, ok := legacy.PeerProcessView(501, lockProc)
	require.True(t, ok)
	require.Contains(t, v.RefusalFor(lockFin, LockRequest{ID: "01REQ", Short: "R-7F3K"}).Error(), "sudo -u")

	// Through the context, a refusal carries what the requester reported.
	ctx := WithLockRequester(WithView(context.Background(), sock), func(context.Context, string) (LockRequest, error) {
		return LockRequest{ID: "01REQ", Short: "R-7F3K", Ended: ended}, nil
	})
	require.Contains(t, Refusal(ctx, lockFin).Error(), "(access ended at "+Clock(ended))
	require.True(t, Locked(ctx, lockFin))
	require.False(t, Locked(ctx, lockOps))
	require.False(t, Locked(context.Background(), lockFin), "no View: nothing is locked")
}

// Every agent-facing time is the machine's wall clock with no zone named
// (owner decision 2026-11-08): "14:05", never "14:05 PST".
func TestClock_NamesNoZone(t *testing.T) {
	at := time.Date(2026, 11, 8, 22, 5, 0, 0, time.UTC)
	require.Equal(t, at.Local().Format("15:04"), Clock(at))
	require.Equal(t, at.Local().Format("15:04")+" on "+at.Local().Format("Jan 2"), clockDay(at))
	require.Regexp(t, `^\d\d:\d\d$`, Clock(at))
}
