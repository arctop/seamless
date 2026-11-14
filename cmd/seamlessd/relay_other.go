//go:build !darwin && !linux

package main

import (
	"fmt"

	"github.com/arctop/seamless/internal/relay"
)

// runRelay refuses: the relay runs on macOS and Linux only, whose kernels
// name a socket's peer. A computer here pairs as a key device.
func runRelay([]string) error {
	return fmt.Errorf("seamlessd relay: %w", relay.ErrUnsupported)
}
