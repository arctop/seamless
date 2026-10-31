//go:build darwin || linux

package main

import (
	"fmt"
	"os"
	"syscall"
)

// inspectPath reports the owner and mode of path without following a final
// symlink, for the peer_auth preflight.
func inspectPath(path string) (fileFacts, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return fileFacts{}, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileFacts{}, fmt.Errorf("no owner information for %s", path)
	}
	mode := info.Mode()
	return fileFacts{
		uid:     st.Uid,
		mode:    mode,
		isDir:   info.IsDir(),
		symlink: mode&os.ModeSymlink != 0,
		socket:  mode&os.ModeSocket != 0,
	}, nil
}
