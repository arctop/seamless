//go:build darwin || linux

package access

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// hostPinFS reads a pin from this machine's file system.
var hostPinFS = pinFS{
	lstat: func(path string) (pinFacts, error) {
		info, err := os.Lstat(path)
		if err != nil {
			return pinFacts{}, err
		}
		return factsOf(info)
	},
	evalSymlinks: filepath.EvalSymlinks,
	open: func(path string, n int) (pinFacts, []byte, error) {
		// No final symlink (the path is resolved), and no wait on a FIFO.
		f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return pinFacts{}, nil, err
		}
		defer func() { _ = f.Close() }()
		info, err := f.Stat()
		if err != nil {
			return pinFacts{}, nil, err
		}
		facts, err := factsOf(info)
		if err != nil {
			return pinFacts{}, nil, err
		}
		if !info.Mode().IsRegular() {
			return facts, nil, nil // refused by the caller, unread
		}
		data, err := io.ReadAll(io.LimitReader(f, int64(n)))
		return facts, data, err
	},
}

func factsOf(info os.FileInfo) (pinFacts, error) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return pinFacts{}, fmt.Errorf("no owner information for %s", info.Name())
	}
	return pinFacts{uid: st.Uid, mode: info.Mode()}, nil
}
