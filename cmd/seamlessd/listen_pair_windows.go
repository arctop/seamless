//go:build windows

package main

import (
	"errors"
	"syscall"
)

// Winsock's codes for the listen failures addrInUse and addrUnheld read: the
// syscall package names only WSAEACCES of them, and its EADDRINUSE and
// friends are values Winsock never returns.
const (
	wsaeAddrInUse      = syscall.Errno(10048) // WSAEADDRINUSE
	wsaeAddrNotAvail   = syscall.Errno(10049) // WSAEADDRNOTAVAIL
	wsaeAFNoSupport    = syscall.Errno(10047) // WSAEAFNOSUPPORT
	wsaeProtoNoSupport = syscall.Errno(10043) // WSAEPROTONOSUPPORT
)

// addrInUse reports whether a listen failed because a socket holds the
// address and port already.
func addrInUse(err error) bool { return errors.Is(err, wsaeAddrInUse) }

// addrUnheld reports whether a listen failed because no process can hold the
// address now: it is not this machine's (or not usable yet), or its family
// is not supported here. Anything else -- in use, refused (WSAEACCES, another
// account's exclusive socket) -- may be another process's doing.
func addrUnheld(err error) bool {
	return errors.Is(err, wsaeAddrNotAvail) || errors.Is(err, wsaeAFNoSupport) || errors.Is(err, wsaeProtoNoSupport)
}
