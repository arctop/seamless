//go:build darwin || linux

package main

// How the bridge sees the Touch ID helper end (mcpproxy_approver.go). The
// helper is opened through LaunchServices, never as the bridge's child, so
// its exit status (2 for a cancel) never reaches the bridge: open returns 0
// whatever the app does. What does reach it is the app's stderr, which open
// --stderr connects to a path. The path is a FIFO the bridge reads: the
// helper writes its outcome there as it finishes ("SeamApprove: <message>"),
// and the FIFO's end is the helper's exit.
//
// The bridge holds a write end of its own until open has handed the FIFO
// over, and an end counts only once the helper has written to it (it always
// writes its outcome before it exits): a helper that opens its stderr late is
// still heard, and one that never writes is never reported -- the dialog then
// waits out its bound, as before. The read end is non-blocking and polled:
// on Darwin kqueue reports no event
// when a FIFO's last writer closes (Go's poller leaves FIFOs alone there for
// that reason), and a blocked read could not be stopped when the bridge
// exits. Linux behaves the same way for these calls, so the tests run there.

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"
)

// approverStderrPoll is how often the bridge reads a helper's stderr.
const approverStderrPoll = 100 * time.Millisecond

// approverStderrTail bounds what the bridge keeps of a helper's stderr: the
// outcome is its last line, and nothing else is read.
const approverStderrTail = 16 << 10

// approverStderr is one helper's stderr: a FIFO in a directory of its own.
type approverStderr struct {
	dir  string
	path string
	rfd  int
	wfd  int // the bridge's own write end until handedOver; -1 after
	// heard is set once the helper wrote anything: only then is an end the
	// helper's exit.
	heard bool
}

// openApproverStderr makes the FIFO a launch hands the helper as its stderr.
func openApproverStderr() (*approverStderr, error) {
	dir, err := os.MkdirTemp("", "seam-approver-")
	if err != nil {
		return nil, err
	}
	s := &approverStderr{dir: dir, path: filepath.Join(dir, "stderr"), rfd: -1, wfd: -1}
	if err := syscall.Mkfifo(s.path, 0o600); err != nil {
		s.close()
		return nil, err
	}
	if s.rfd, err = syscall.Open(s.path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0); err != nil {
		s.rfd = -1
		s.close()
		return nil, err
	}
	// With a reader open, a non-blocking writer opens at once.
	if s.wfd, err = syscall.Open(s.path, syscall.O_WRONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0); err != nil {
		s.wfd = -1
		s.close()
		return nil, err
	}
	return s, nil
}

// stderrPath is the path open --stderr names.
func (s *approverStderr) stderrPath() string { return s.path }

// handedOver closes the bridge's own write end once open has returned: from
// then on the FIFO ends when the helper exits.
func (s *approverStderr) handedOver() {
	if s.wfd >= 0 {
		_ = syscall.Close(s.wfd) //nolint:errcheck // closing our own spare write end; a failure leaves only a slower watch, ended by its limit
		s.wfd = -1
	}
}

// wait reads the helper's stderr until it ends after the helper wrote to it
// (the helper exited: ended is true), quit closes, or limit passes. It
// returns the tail it read.
func (s *approverStderr) wait(quit <-chan struct{}, limit time.Duration) (tail []byte, ended bool) {
	tick := time.NewTicker(approverStderrPoll)
	defer tick.Stop()
	stop := time.NewTimer(limit)
	defer stop.Stop()
	buf := make([]byte, 4096)
	for {
		read, eof, err := s.drain(&tail, buf)
		s.heard = s.heard || read > 0
		if err != nil || (eof && s.heard) {
			return tail, err == nil
		}
		select {
		case <-quit:
			return tail, false
		case <-stop.C:
			return tail, false
		case <-tick.C:
		}
	}
}

// drain reads what the FIFO holds now onto tail: how many bytes, and whether
// every writer is gone (for now: a writer may still open it).
func (s *approverStderr) drain(tail *[]byte, buf []byte) (int, bool, error) {
	total := 0
	for {
		n, err := syscall.Read(s.rfd, buf)
		switch {
		case errors.Is(err, syscall.EAGAIN), errors.Is(err, syscall.EINTR):
			return total, false, nil
		case err != nil:
			return total, false, err
		case n == 0:
			return total, true, nil
		}
		total += n
		*tail = append(*tail, buf[:n]...)
		if len(*tail) > approverStderrTail {
			*tail = slices.Clone((*tail)[len(*tail)-approverStderrTail:])
		}
	}
}

// close releases the FIFO and its directory.
func (s *approverStderr) close() {
	s.handedOver()
	if s.rfd >= 0 {
		_ = syscall.Close(s.rfd) //nolint:errcheck // the read end of a FIFO this process made; nothing is left to read
		s.rfd = -1
	}
	_ = os.RemoveAll(s.dir) //nolint:errcheck // a leftover empty temp directory is harmless, and nothing else names it
}
