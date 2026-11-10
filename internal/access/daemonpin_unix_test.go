//go:build darwin || linux

package access

import (
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// A FIFO at the pin's path is refused before it is opened, so the read never
// waits on a writer.
func TestReadDaemonPin_FIFO(t *testing.T) {
	r := newPinRig(t)
	path := filepath.Join(r.root, "daemon-account")
	require.NoError(t, syscall.Mkfifo(path, 0o644))
	_, present, err := r.read(path)
	require.ErrorIs(t, err, ErrDaemonPin)
	require.True(t, present)
	require.Contains(t, err.Error(), "is not a regular file")

	// The host's open, asked directly, does not wait either.
	facts, data, err := hostPinFS.open(path, maxPinBytes+1)
	require.NoError(t, err)
	require.False(t, facts.mode.IsRegular())
	require.Nil(t, data)
}
