//go:build unix

package gardener

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// A recorded transcript path that names a pipe is never opened: opening one
// for reading waits for a writer, and the upkeep would wait with it. The
// session is reaped all the same, and its owed usage dropped.
func TestOwedUsage_NeverOpensAPipe(t *testing.T) {
	e := newOwedEnv(t)
	path := filepath.Join(t.TempDir(), "pipe.jsonl")
	require.NoError(t, syscall.Mkfifo(path, 0o600))
	sess := e.session("cc/piped", store.UsageOwedClient, owedHost, path, e.now.Add(-2*time.Hour))

	done := make(chan struct{})
	go func() {
		defer close(done)
		e.upkeep(false)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		// A writer's non-blocking open succeeds only while a reader waits:
		// end that read, so the pass can finish, and fail.
		if w, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = w.Close()
		}
		<-done
		t.Fatal("the upkeep opened the pipe and waited on it")
	}

	rows, got := e.usage(sess.ID)
	require.Equal(t, core.SessionExpired, got.Status)
	require.Empty(t, rows)
	require.False(t, e.owed(sess))
}
