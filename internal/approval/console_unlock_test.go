package approval

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/passkey/passkeytest"
	"github.com/arctop/seamless/internal/store"
)

func TestConsoleLeaseLength_CappedByMaxLease(t *testing.T) {
	f := newFixture(t)
	require.Equal(t, ConsoleLease, f.svc.ConsoleLeaseLength(), "three hours under the default cap")
	f.svc.cfg.LeaseCap = time.Hour
	require.Equal(t, time.Hour, f.svc.ConsoleLeaseLength(), "a shorter peer_auth.max_lease wins")
}

func TestConsoleUnlock_Passkey(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rec := events.NewRecorder(f.db)
	f.svc.cfg.Events = rec
	browser, other := store.HashConsoleToken("browser-a"), store.HashConsoleToken("browser-b")

	_, err := f.svc.BeginConsoleUnlock(ctx, "example-finance", browser)
	require.ErrorIs(t, err, ErrNoPasskey, "nothing to assert with until a passkey is enrolled")

	key, err := passkeytest.New(true)
	require.NoError(t, err)
	cred := f.enrollWithCode(t, key, "security key")

	begun, err := f.svc.BeginConsoleUnlock(ctx, "example-finance", browser)
	require.NoError(t, err)
	resp, err := key.Get(begun.Options, origin)
	require.NoError(t, err)

	// The assertion is bound to the project and to the browser that began it.
	_, err = f.svc.FinishConsoleUnlock(ctx, begun.ID, "example-legal", browser, resp)
	require.ErrorIs(t, err, store.ErrConsoleUnlockNotFound, "another project")
	_, err = f.svc.FinishConsoleUnlock(ctx, begun.ID, "example-finance", other, resp)
	require.ErrorIs(t, err, store.ErrConsoleUnlockNotFound, "another browser")
	_, err = f.svc.FinishConsoleUnlock(ctx, begun.ID, "example-finance", browser, []byte(`{"id":"x"}`))
	require.Error(t, err, "garbage is not an assertion")

	f.now = f.now.Add(time.Minute)
	u, err := f.svc.FinishConsoleUnlock(ctx, begun.ID, "example-finance", browser, resp)
	require.NoError(t, err)
	require.Equal(t, store.ConsoleUnlockLive, u.Status)
	require.Equal(t, cred.ID, u.Credential)
	require.Equal(t, f.now.Add(ConsoleLease), *u.ExpiresAt)

	used, err := store.ApproverByID(ctx, f.db, cred.ID)
	require.NoError(t, err)
	require.NotNil(t, used.LastUsedAt, "the passkey's use is recorded")

	_, err = f.svc.FinishConsoleUnlock(ctx, begun.ID, "example-finance", browser, resp)
	require.ErrorIs(t, err, store.ErrConsoleUnlockNotFound, "a ceremony is used once")

	live, err := store.LiveConsoleUnlocks(ctx, f.db, browser, f.now)
	require.NoError(t, err)
	require.Len(t, live, 1)

	evs, err := rec.ByKinds(ctx, []core.EventKind{EventConsoleUnlocked}, "", "", 10)
	require.NoError(t, err)
	require.Len(t, evs, 1)
	require.Equal(t, "example-finance", evs[0].ProjectSlug)
	require.Equal(t, u.ID, evs[0].ItemID)

	// Late: the ceremony waits ConsoleUnlockTTL.
	late, err := f.svc.BeginConsoleUnlock(ctx, "example-finance", browser)
	require.NoError(t, err)
	resp, err = key.Get(late.Options, origin)
	require.NoError(t, err)
	f.now = f.now.Add(ConsoleUnlockTTL)
	_, err = f.svc.FinishConsoleUnlock(ctx, late.ID, "example-finance", browser, resp)
	require.ErrorIs(t, err, store.ErrConsoleUnlockNotFound)
}

func TestConsoleUnlock_PasskeysOff(t *testing.T) {
	f := newFixture(t)
	off := New(Config{DB: f.db})
	_, err := off.BeginConsoleUnlock(context.Background(), "example-finance", store.HashConsoleToken("b"))
	require.ErrorIs(t, err, ErrPasskeysOff)
	_, err = off.FinishConsoleUnlock(context.Background(), "x", "example-finance", store.HashConsoleToken("b"), nil)
	require.ErrorIs(t, err, ErrPasskeysOff)
}

// The terminal path: the browser begins it, with no passkey needed; the
// daemon's account approves its code for a lease no longer than the console's;
// the approval is recorded.
func TestConsoleUnlock_Terminal(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rec := events.NewRecorder(f.db)
	f.svc.cfg.Events = rec
	browser := store.HashConsoleToken("browser-a")

	begun, err := f.svc.BeginTerminalUnlock(ctx, "example-finance", browser)
	require.NoError(t, err, "no passkey is enrolled, and none is needed")
	require.Equal(t, begun.Code, store.FormatUnlockCode(begun.Code))
	require.Equal(t, f.now.Add(ConsoleTerminalUnlockTTL), begun.PendingUntil)

	_, err = f.svc.ApproveTerminalUnlock(ctx, "example-finance", begun.Code, ConsoleLease+time.Minute)
	require.ErrorIs(t, err, store.ErrInvalidConsoleUnlock, "no longer than the console lease")
	_, err = f.svc.ApproveTerminalUnlock(ctx, "example-legal", begun.Code, time.Hour)
	require.ErrorIs(t, err, store.ErrConsoleUnlockNotFound, "a code names one project's unlock")
	require.Empty(t, f.unlockEvents(t, rec), "a refused approval records nothing")

	f.now = f.now.Add(time.Minute)
	u, err := f.svc.ApproveTerminalUnlock(ctx, "example-finance", begun.Code, time.Hour)
	require.NoError(t, err)
	require.Equal(t, begun.ID, u.ID)
	require.Equal(t, browser, u.TokenHash)
	require.Equal(t, store.ConsoleUnlockByDaemon, u.Credential)
	require.Equal(t, f.now.Add(time.Hour), *u.ExpiresAt)
	evs := f.unlockEvents(t, rec)
	require.Len(t, evs, 1)
	require.Equal(t, u.ID, evs[0].ItemID)
	require.Equal(t, "example-finance", evs[0].ProjectSlug)
	require.Equal(t, ProofDaemon, evs[0].Payload["proof"])

	_, err = f.svc.ApproveTerminalUnlock(ctx, "example-finance", begun.Code, time.Hour)
	require.ErrorIs(t, err, store.ErrConsoleUnlockNotFound, "a code works once")

	// Late: the code waits ConsoleTerminalUnlockTTL.
	late, err := f.svc.BeginTerminalUnlock(ctx, "example-finance", browser)
	require.NoError(t, err)
	f.now = f.now.Add(ConsoleTerminalUnlockTTL)
	_, err = f.svc.ApproveTerminalUnlock(ctx, "example-finance", late.Code, time.Hour)
	require.ErrorIs(t, err, store.ErrConsoleUnlockNotFound)
}

func (f *fixture) unlockEvents(t *testing.T, rec *events.Recorder) []core.Event {
	t.Helper()
	evs, err := rec.ByKinds(context.Background(), []core.EventKind{EventConsoleUnlocked}, "", "", 10)
	require.NoError(t, err)
	return evs
}

// A restricted account begins and approves nothing: the console already keeps
// it out, and the approval service refuses it again.
func TestConsoleUnlock_TerminalRefusesRestricted(t *testing.T) {
	f := newGrantFixture(t)
	rctx := access.WithView(context.Background(), restrictedView(t, f, bridge, agent, shell))
	_, err := f.svc.BeginTerminalUnlock(rctx, lockFin, store.HashConsoleToken("browser-a"))
	require.ErrorIs(t, err, ErrRestricted)
	_, err = f.svc.ApproveTerminalUnlock(rctx, lockFin, "ABCD-EFGH", time.Hour)
	require.ErrorIs(t, err, ErrRestricted)
	require.Zero(t, countRows(t, f, "console_unlocks"))
}
