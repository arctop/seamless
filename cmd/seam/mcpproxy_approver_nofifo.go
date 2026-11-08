//go:build !(darwin || linux)

package main

// Where the bridge cannot make a FIFO, it launches the helper as before and
// cannot see it end: the dialog waits out its bound. The helper exists only
// on macOS, so this is a launch that is refused anyway (launchApprover).

import (
	"errors"
	"time"
)

type approverStderr struct{}

var errNoApproverStderr = errors.New("this platform has no FIFO for the Touch ID approver's stderr")

func openApproverStderr() (*approverStderr, error) { return nil, errNoApproverStderr }

func (s *approverStderr) stderrPath() string { return "" }

func (s *approverStderr) handedOver() {}

func (s *approverStderr) wait(<-chan struct{}, time.Duration) ([]byte, bool) { return nil, false }

func (s *approverStderr) close() {}
