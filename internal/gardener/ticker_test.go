package gardener

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/store/storetest"
)

// Regression: the daemon must be able to drain the gardener goroutine before it
// closes the DB. Start tracks the goroutine and Wait blocks until ctx cancel
// stops it; before this contract existed the loop was fire-and-forget and a
// mid-shutdown pass could race the DB close.
func TestStartWait_DrainsOnCancel(t *testing.T) {
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	g := New(db, nil, nil, nil, nil, Config{}, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	g.Start(ctx)
	cancel()

	done := make(chan struct{})
	go func() {
		g.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return after ctx cancel")
	}
}

// Wait on a service whose Start never ran must return immediately (the daemon
// calls Wait unconditionally; the gardener may be disabled).
func TestWait_NoStartIsNoOp(t *testing.T) {
	g := New(nil, nil, nil, nil, nil, Config{}, slog.Default())
	done := make(chan struct{})
	go func() {
		g.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Wait without Start must not block")
	}
}
