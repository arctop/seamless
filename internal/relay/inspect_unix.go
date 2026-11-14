//go:build darwin || linux

package relay

import (
	"fmt"
	"os"
	"syscall"
)

// Supported reports whether a relay runs on this platform: macOS and Linux,
// whose kernels name a socket's peer (access.PeerCredSupported).
const Supported = true

// inspectPath reports the owner and mode of path without following a final
// symlink.
func inspectPath(path string) (FileFacts, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return FileFacts{}, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return FileFacts{}, fmt.Errorf("no owner information for %s", path)
	}
	mode := info.Mode()
	return FileFacts{
		UID:     st.Uid,
		GID:     st.Gid,
		Mode:    mode,
		IsDir:   info.IsDir(),
		Symlink: mode&os.ModeSymlink != 0,
		Socket:  mode&os.ModeSocket != 0,
	}, nil
}
