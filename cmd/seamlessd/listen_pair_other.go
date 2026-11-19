//go:build !linux

package main

import "net"

// pairedListen is how serve listens on an IPv6 address paired beside its bind
// (listenPaired): a plain listen. Linux's sets IP_FREEBIND, so serve holds a
// tentative address (listen_pair_linux.go); other systems have no option an
// unprivileged process may set to bind one, so a tentative address is refused
// here and skipped.
func pairedListen(network, address string) (net.Listener, error) {
	return net.Listen(network, address)
}
