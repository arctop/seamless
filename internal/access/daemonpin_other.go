//go:build !darwin && !linux

package access

import (
	"errors"
	"os"
	"path/filepath"
)

// errPinOwnerUnsupported is the trust check's answer where file owners cannot
// be read: a pin that is there is then refused, never believed.
var errPinOwnerUnsupported = errors.New("file owners cannot be read on this platform")

// hostPinFS here finds out only whether a pin is there; one that is is
// refused, since who wrote it cannot be told.
var hostPinFS = pinFS{
	lstat: func(path string) (pinFacts, error) {
		if _, err := os.Lstat(path); err != nil {
			return pinFacts{}, err
		}
		return pinFacts{}, errPinOwnerUnsupported
	},
	evalSymlinks: filepath.EvalSymlinks,
	open: func(string, int) (pinFacts, []byte, error) {
		return pinFacts{}, nil, errPinOwnerUnsupported
	},
}
