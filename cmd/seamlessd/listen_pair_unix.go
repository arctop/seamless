//go:build !windows

package main

import (
	"errors"
	"syscall"
)

// addrInUse reports whether a listen failed because a socket holds the
// address and port already.
func addrInUse(err error) bool { return errors.Is(err, syscall.EADDRINUSE) }

// addrTaken reports whether a listen failed because another socket holds the
// address: what stops a running serve beside an address it must hold
// (pairWatch). A refusal of another kind (out of descriptors, a policy) says
// nothing about another process, so serve logs it and tries again.
func addrTaken(err error) bool { return addrInUse(err) }

// addrUnheld reports whether a listen failed because no process can hold the
// address now: it is not this machine's (or not usable yet), or its family
// is not supported here. Anything else -- in use, refused -- may be another
// process's doing.
func addrUnheld(err error) bool {
	return errors.Is(err, syscall.EADDRNOTAVAIL) || errors.Is(err, syscall.EAFNOSUPPORT) ||
		errors.Is(err, syscall.EPROTONOSUPPORT)
}
