//go:build darwin || linux

package main

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// The helper's stderr FIFO ends when the helper's write end closes -- not
// before, while only the bridge's own spare end is open -- and a watch ends
// unreported when the bridge quits.
func TestApproverStderr_EndsWithTheHelper(t *testing.T) {
	s, err := openApproverStderr()
	require.NoError(t, err)
	defer s.close()
	quit := make(chan struct{})

	// Before the hand-over, nothing has ended: the bridge's own end is open.
	tail, ended := s.wait(quit, 3*approverStderrPoll)
	require.False(t, ended)
	require.Empty(t, tail)

	helper, err := os.OpenFile(s.stderrPath(), os.O_WRONLY, 0)
	require.NoError(t, err)
	s.handedOver()
	_, err = helper.WriteString("SeamApprove: Not approved: the Touch ID prompt was cancelled.\n")
	require.NoError(t, err)
	tail, ended = s.wait(quit, 3*approverStderrPoll)
	require.False(t, ended, "written, not yet exited")
	require.Equal(t, "SeamApprove: Not approved: the Touch ID prompt was cancelled.\n", string(tail))

	require.NoError(t, helper.Close())
	tail, ended = s.wait(quit, time.Minute)
	require.True(t, ended, "the helper exited")
	require.Empty(t, tail, "everything was read before")

	dir := s.dir
	s.close()
	_, err = os.Lstat(dir)
	require.True(t, os.IsNotExist(err), "the FIFO's directory is removed")
}

// A helper may open its stderr only after open returned: a FIFO with no
// writer and nothing written has not ended, and the helper's word is heard
// when it comes.
func TestApproverStderr_ALateHelperIsHeard(t *testing.T) {
	s, err := openApproverStderr()
	require.NoError(t, err)
	defer s.close()
	quit := make(chan struct{})
	s.handedOver()
	_, ended := s.wait(quit, 3*approverStderrPoll)
	require.False(t, ended, "no writer, nothing written: not an end")

	helper, err := os.OpenFile(s.stderrPath(), os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = helper.WriteString("SeamApprove: Not approved: the Touch ID prompt was cancelled.\n")
	require.NoError(t, err)
	require.NoError(t, helper.Close())
	tail, ended := s.wait(quit, time.Minute)
	require.True(t, ended)
	require.Equal(t, core.ApproverEndedCancelled, approverOutcome(tail))
}

// A helper killed before it said anything still ends the watch, as closed: a
// real process holds the write end and dies of each signal, and the one that
// cannot be caught is why the bridge cannot rely on a last word.
func TestApproverStderr_AKilledHelperThatSaidNothingEnds(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		t.Run(sig.String(), func(t *testing.T) {
			s, err := openApproverStderr()
			require.NoError(t, err)
			defer s.close()
			quit := make(chan struct{})

			stderr, err := os.OpenFile(s.stderrPath(), os.O_WRONLY, 0)
			require.NoError(t, err)
			helper := exec.Command("sleep", "60")
			helper.Stderr = stderr
			require.NoError(t, helper.Start())
			t.Cleanup(func() { _ = helper.Process.Kill() })
			require.NoError(t, stderr.Close())
			s.handedOver()

			tail, ended := s.wait(quit, 3*approverStderrPoll)
			require.False(t, ended, "alive and silent is not an end")
			require.Empty(t, tail)

			require.NoError(t, helper.Process.Signal(sig))
			var exit *exec.ExitError
			require.ErrorAs(t, helper.Wait(), &exit, "the helper died of the signal")

			tail, ended = s.wait(quit, time.Minute)
			require.True(t, ended, "the helper is gone")
			require.Empty(t, tail)
			require.Equal(t, core.ApproverEndedClosed, approverOutcome(tail))
		})
	}
}

func TestApproverStderr_QuitEndsTheWatch(t *testing.T) {
	s, err := openApproverStderr()
	require.NoError(t, err)
	defer s.close()
	helper, err := os.OpenFile(s.stderrPath(), os.O_WRONLY, 0)
	require.NoError(t, err)
	defer func() { _ = helper.Close() }()
	s.handedOver()
	quit := make(chan struct{})
	close(quit)
	_, ended := s.wait(quit, time.Minute)
	require.False(t, ended)
}
