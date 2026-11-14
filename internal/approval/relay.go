package approval

// Requests through a relay (plan networked-relay). A paired relay forwards,
// with every request, the uid and the process chain its own kernel reported
// for the local caller, and under peer_auth a relay peer_auth.relays maps is
// judged as this daemon's own socket judges a request: its leases and grants
// are keyed on the relay and the process (access.ProcessToken.Relay).
//
// What the relay does not forward is the program each process runs: the wire
// (version 1) carries each process as its key alone. The anchor rule (anchor.go,
// owner decision 2026-11-07) needs exactly that -- a request may be filed only
// for an agent program, or a process with one above it, and never for a
// shell, terminal or multiplexer, whose lease would cover every program it
// started -- and this daemon's kernel cannot name another computer's process
// (access.ProgramOf answers ""). For a process of this machine an unnamed
// anchor is one that exited, whose lease could apply to nothing; for a
// relay's it is merely one the server cannot see, and a lease on it could be
// a shell's.
//
// So no request is filed for a relay's process (Service.anchorFor, for a
// lock's request, a grant and a move alike), and the approve core refuses a
// stored one whose program is unknown (Service.Approve). A relay's agents
// reach a locked project once the relay forwards each process's program as
// its kernel names it, which the filing then judges as it judges this
// machine's.

import (
	"errors"
	"fmt"
	"strings"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/store"
)

// ErrRelayUnnamed is a request anchored at a process a paired relay reported
// from another computer, whose program the server cannot name: no approval
// may name a process the anchor rule cannot judge (see the file comment).
// Filing refuses it, and no request exists.
var ErrRelayUnnamed = errors.New("no access request is filed for an agent on another computer yet: its relay does not " +
	"say which program is asking, and a request is filed only for an agent program; work in the project from an agent " +
	"session on the server's own computer, which asks for itself")

// relayUnnamed reports whether chain is a relay's (its processes are another
// computer's): the anchor rule cannot judge any of its processes.
func relayUnnamed(chain []access.ProcessToken) bool {
	return len(chain) > 0 && chain[0].Relay != ""
}

// storedRelayRefusal is the approve core's refusal of a stored request whose
// process a relay reported and whose program nobody named: the anchor rule
// was never applied to it, so no proof makes a lease on it safe.
func storedRelayRefusal(a store.AccessApproval) error {
	if access.RelayOfKey(a.ProcessKey) == "" || strings.TrimSpace(a.Program) != "" {
		return nil
	}
	return fmt.Errorf("%w: deny it", ErrRelayUnnamed)
}
