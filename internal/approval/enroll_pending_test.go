package approval

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/passkey/passkeytest"
	"github.com/arctop/seamless/internal/store"
)

// Waiting passkey enrollments are capped per stage, and a begin never waits
// for room. A loop on begin, which needs no passkey, crowds out only
// enrollments still waiting for their browser: the owner's, whose passkey
// exists and waits for the code minted at a terminal, stays and enrolls. The
// control is a loop that creates passkeys too, which does crowd it out.
func TestPasskeyEnrollment_PendingCaps(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	begin := func() Begun {
		t.Helper()
		f.now = f.now.Add(time.Second)
		b, err := f.svc.BeginPasskey(ctx, "loop")
		require.NoError(t, err, "a begin never waits for room")
		return b
	}
	create := func(name string) (string, Finished) {
		t.Helper()
		f.now = f.now.Add(time.Second)
		a, err := passkeytest.New(true)
		require.NoError(t, err)
		return f.startByCode(t, a, name)
	}
	waiting := func(stage *enrollStage) int {
		f.svc.mu.Lock()
		defer f.svc.mu.Unlock()
		n := 0
		for _, p := range f.svc.pending {
			if stage == nil || p.scope() == *stage {
				n++
			}
		}
		return n
	}
	held := func(id string) bool {
		f.svc.mu.Lock()
		defer f.svc.mu.Unlock()
		_, ok := f.svc.pending[id]
		return ok
	}
	begun, created := enrollBegun, enrollCreated

	// The owner's passkey exists and waits for its code.
	owners, fin := create("owner's key")

	// A begin past its stage's cap displaces the oldest still begun.
	first := begin()
	for range maxPendingPerStage {
		begin()
	}
	require.False(t, held(first.ID))
	require.Equal(t, maxPendingPerStage, waiting(&begun))

	// The loop goes on; the owner's passkey stays and enrolls.
	for range 3 * maxPending {
		begin()
	}
	require.True(t, held(owners), "a loop on begin never displaces a passkey waiting for its code")
	require.Equal(t, maxPendingPerStage+1, waiting(nil))
	_, err := f.svc.ConfirmPasskeyCode(ctx, owners, f.codeFor(t, store.ApproverPasskey, fin.Fingerprint))
	require.NoError(t, err)

	// The control: a loop that creates passkeys crowds the owner's out, and
	// the overall cap holds.
	owners, _ = create("owner's second key")
	for range maxPendingPerStage {
		create("loop")
	}
	require.False(t, held(owners), "a loop that creates passkeys crowds a waiting one out")
	require.Equal(t, maxPendingPerStage, waiting(&created))
	require.LessOrEqual(t, waiting(nil), maxPending)
}
