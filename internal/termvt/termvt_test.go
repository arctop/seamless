package termvt

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

// A file is no console: Windows has no mode to turn on for it and says no,
// while elsewhere Enable has nothing to turn on and leaves the question of
// whether f is a terminal to its caller.
func TestEnable_File(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "out"))
	require.NoError(t, err)
	defer func() { require.NoError(t, f.Close()) }()
	require.Equal(t, runtime.GOOS != "windows", Enable(f))
}
