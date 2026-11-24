//go:build windows

package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDetachTaskConsole_NotUnderGoTest: the test binary's parent is go, not
// the Task Scheduler, so it keeps whatever console it has.
func TestDetachTaskConsole_NotUnderGoTest(t *testing.T) {
	require.False(t, detachTaskConsole())
}
