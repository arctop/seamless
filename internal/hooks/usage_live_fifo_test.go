//go:build unix

package hooks

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/harvest"
	"github.com/arctop/seamless/internal/store"
)

// The mid-session harvest's reads, watched through named pipes: opening one
// for reading blocks until a writer opens it, so a read the daemon makes is
// seen waiting, and a hook that answers while it waits proves the prompt
// never waits for it.

// ledgerLiveFeed waits for a reader to open the pipe at path (a writer's
// non-blocking open succeeds only then), then writes lines to it and closes
// it, ending that reader's read.
func ledgerLiveFeed(t *testing.T, path, lines string) {
	t.Helper()
	var w *os.File
	require.Eventually(t, func() bool {
		f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return false
		}
		w = f
		return true
	}, 5*time.Second, 5*time.Millisecond, "nobody opened %s for reading", path)
	_, err := w.WriteString(lines)
	require.NoError(t, err)
	require.NoError(t, w.Close())
}

// ledgerLiveUnread holds that nobody has the pipe at path open for reading.
func ledgerLiveUnread(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err == nil {
		_ = f.Close() // releases the reader the daemon should never have become
		t.Fatalf("%s was opened for reading", path)
	}
	require.True(t, errors.Is(err, syscall.ENXIO), "%s: %v", path, err)
}

// ledgerLiveRelease ends every read still waiting on the pipe at path, as a
// cleanup for a test that failed with one there: it opens the pipe for
// writing while anyone has it open for reading, and holds it open a moment
// each time, so a reader woken in open(2) gets past it before the close
// reads as the end of the file.
func ledgerLiveRelease(path string) {
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		w, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return // nobody has it open for reading
		}
		<-time.After(20 * time.Millisecond)
		_ = w.Close()
	}
}

// ledgerLiveOnPipe starts conversation id with its transcript recorded as a
// new pipe, and returns the pipe. The hooks name no path, so the session
// start's and the prompt's model sniff open nothing: only the harvest's
// read, which falls back on the recorded path, can.
func (e *ledgerLiveEnv) ledgerLiveOnPipe(id string) string {
	e.t.Helper()
	path := fifo(e.t, filepath.Join(e.t.TempDir(), id+".jsonl"))
	e.t.Cleanup(func() { ledgerLiveRelease(path) })
	sess := e.start(id, "")
	require.NoError(e.t, store.SetSessionTranscriptPath(context.Background(), e.db, sess.ID, path))
	return path
}

// ledgerLiveAsk submits a prompt, without waiting for the read it starts.
func (e *ledgerLiveEnv) ledgerLiveAsk(id string) *http.Response {
	e.t.Helper()
	done := make(chan *http.Response, 1)
	go func() {
		resp, _ := e.hook("user-prompt-submit", "", ledgerLivePromptBody(id, "/work/demo", ""))
		done <- resp
	}()
	select {
	case resp := <-done:
		return resp
	case <-time.After(5 * time.Second):
		e.t.Fatalf("the prompt of %s never answered: it waited for the read", id)
		return nil
	}
}

// The daemon reads the transcript only once it has answered: the prompt is
// answered while the read still waits to open the file, and the ledger then
// holds what the read found.
func TestPromptUsage_TheReadComesAfterTheAnswer(t *testing.T) {
	e := newLedgerLiveEnv(t, true)
	pipe := e.ledgerLiveOnPipe("live-pipe-1")
	resp := e.ledgerLiveAsk("live-pipe-1")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.False(t, e.h.WaitFor(20*time.Millisecond), "the read is still waiting for the file")
	ledgerLiveFeed(t, pipe, ledgerLiveTurn1)
	e.h.Wait()
	require.Equal(t, ledgerLiveRows(t, writeTranscript(t, ledgerLiveTurn1)), e.ledger("live-pipe-1"))
}

// With the Diary off the daemon never opens the transcript; under peer_auth
// it never opens it with the Diary on either: the answer asks instead.
func TestPromptUsage_NoReadWhenOffOrUnderPeerAuth(t *testing.T) {
	off := newLedgerLiveEnv(t, false)
	pipe := off.ledgerLiveOnPipe("live-pipe-off")
	off.ledgerLiveAsk("live-pipe-off")
	// Bounded: a read that started would wait on the pipe for good.
	require.True(t, off.h.WaitFor(time.Second), "a read started with the Diary off")
	ledgerLiveUnread(t, pipe)
	require.Empty(t, off.ledger("live-pipe-off"))

	sock := newLedgerLiveSocket(t)
	path := fifo(t, filepath.Join(t.TempDir(), "socket.jsonl"))
	done := make(chan *http.Response, 1)
	go func() {
		ledgerLiveHook(sock, "session-start", "", map[string]any{"session_id": "live-pipe-sock", "cwd": "/work/ops",
			"source": "startup", "transcript_path": path})
		resp, _ := ledgerLiveHook(sock, "user-prompt-submit", "", ledgerLivePromptBody("live-pipe-sock", "/work/ops", path))
		done <- resp
	}()
	select {
	case resp := <-done:
		require.Equal(t, harvest.UsageWanted, resp.Header.Get(harvest.UsageWantedHeader))
	case <-time.After(5 * time.Second):
		t.Fatal("under peer_auth a hook opened the transcript it named")
	}
	ledgerLiveUnread(t, path)
}

// One read at a time per conversation, and usageLiveReads in all: a prompt
// past its interval while its conversation's read still runs starts none,
// and one that finds every read busy starts none either -- and is not held
// by the throttle for it, so its next prompt reads.
func TestPromptUsage_OneReadAtATime(t *testing.T) {
	require.Equal(t, 2, usageLiveReads, "the test fills the reads one by one")
	e := newLedgerLiveEnv(t, true)
	a, b, c := e.ledgerLiveOnPipe("live-one-a"), e.ledgerLiveOnPipe("live-one-b"), e.ledgerLiveOnPipe("live-one-c")
	reads := func() int {
		e.h.live.mu.Lock()
		defer e.h.live.mu.Unlock()
		return e.h.live.reads
	}

	e.ledgerLiveAsk("live-one-a")
	require.Equal(t, 1, reads())
	e.advance(usageLiveInterval)
	e.ledgerLiveAsk("live-one-a")
	require.Equal(t, 1, reads(), "its read is under way: no second one")

	e.ledgerLiveAsk("live-one-b")
	require.Equal(t, 2, reads())
	e.ledgerLiveAsk("live-one-c")
	require.Equal(t, 2, reads(), "every read is busy: none started")
	e.h.live.mu.Lock()
	_, held := e.h.live.convs[liveKey(ClientClaudeCode, "live-one-c")]
	e.h.live.mu.Unlock()
	require.False(t, held, "and the throttle holds nothing against it")

	ledgerLiveFeed(t, a, ledgerLiveTurn1)
	ledgerLiveFeed(t, b, ledgerLiveTurn1)
	e.h.Wait()
	require.Zero(t, reads())
	e.ledgerLiveAsk("live-one-c")
	ledgerLiveFeed(t, c, ledgerLiveTurn1)
	e.h.Wait()
	want := ledgerLiveRows(t, writeTranscript(t, ledgerLiveTurn1))
	for _, id := range []string{"live-one-a", "live-one-b", "live-one-c"} {
		require.Equal(t, want, e.ledger(id), id)
	}
	require.Equal(t, core.SessionActive, requireAmbientSession(t, e.db, ClientClaudeCode, "live-one-c").Status)
}
