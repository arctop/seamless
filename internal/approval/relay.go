package approval

// Requests through a relay (plan networked-relay). A paired relay forwards,
// with every request, the uid and the process chain its own kernel reported
// for the local caller, and under peer_auth a relay peer_auth.relays maps is
// judged as this daemon's own socket judges a request: its leases and grants
// are keyed on the relay and the process (access.ProcessToken.Relay).
//
// The anchor rule (anchor.go, owner decision 2026-11-07) needs the program
// each process runs -- a request may be filed only for an agent program, or a
// process with one above it, and never for a shell, terminal or multiplexer,
// whose lease would cover every program it started -- and this daemon's
// kernel cannot name another computer's process (access.ProgramOf answers
// ""). From the wire's version 2 the relay names them: the program of every
// process of the chain, as its own kernel names it
// (access.Principal.Programs). The anchor rule then judges a relay's chain
// exactly as it judges this machine's, with those names (programNamer), and
// the stored request carries the program like any other.
//
// A relay of version 1 names none, and a version 2 relay names none for a
// process whose program its kernel could not read (it exited). For a process
// of this machine an unnamed anchor is one that exited, whose lease could
// apply to nothing; for a relay's it is merely one the server cannot judge,
// and a lease on it could be a shell's. So no request is filed anchored at a
// relay's process nobody named (ErrRelayUnnamed, for a lock's request, a grant
// and a move alike), and the approve core refuses a stored one whose program
// is unknown on every path (Service.Approve, and Service.prove for a grant's
// and a move's).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/store"
)

// ErrRelayUnnamed is a request anchored at a process a paired relay reported
// from another computer whose program nobody named -- a relay of wire version
// 1 names none -- so the anchor rule cannot judge it (see the file comment).
// Filing refuses it, and no request exists.
var ErrRelayUnnamed = errors.New("no access request is filed for this agent on another computer: its relay did not say " +
	"which program is asking (a relay of wire version 1 never does), and a request is filed only for an agent program; " +
	"update Seamless on that computer, or work in the project from an agent session on the server's own computer, which " +
	"asks for itself")

// programNamer is how the anchor rule names the processes of chain, a chain
// the request's principal who carries: this daemon's kernel (cfg.Programs)
// for this machine's socket, the relay's own report for a relay's
// (access.Principal.Programs, wire version 2). ok is false for a relay's
// chain no report names: a relay of wire version 1, or a chain that is not
// the principal's own.
func (s *Service) programNamer(who access.Principal, chain []access.ProcessToken) (func(access.ProcessToken) string, bool) {
	if len(chain) == 0 || chain[0].Relay == "" {
		return s.cfg.Programs, true
	}
	if who.Relay != chain[0].Relay || !who.Named() || len(who.Chain) != len(chain) {
		return nil, false
	}
	named := make(map[string]string, len(chain))
	for i, t := range chain {
		if t.Key() != who.Chain[i].Key() {
			return nil, false
		}
		named[t.Key()] = who.Programs[i]
	}
	return func(t access.ProcessToken) string { return named[t.Key()] }, true
}

// storedRelayRefusal is the approve core's refusal of a stored request whose
// process a relay reported and whose program nobody named: the anchor rule
// was never applied to it, so no proof makes a lease, a grant or a move on it
// safe.
func storedRelayRefusal(a store.AccessApproval) error {
	if access.RelayOfKey(a.ProcessKey) == "" || strings.TrimSpace(a.Program) != "" {
		return nil
	}
	return fmt.Errorf("%w: deny it", ErrRelayUnnamed)
}

// RelayName is the enrolled name of the paired relay whose id is relay -- the
// computer its processes run on -- for the surfaces that say which relay
// asked; "" for this machine ("") and for an id no pairing records.
func RelayName(ctx context.Context, db *sql.DB, relay string) string {
	if relay == "" || db == nil {
		return ""
	}
	d, err := store.DeviceByID(ctx, db, relay)
	if err != nil {
		return ""
	}
	return d.Name
}

// relayOf is the computer request a's process runs on: the relay's id and
// its enrolled name ("" for both on this machine). A relay no pairing records
// keeps its id, and is named by it.
func (s *Service) relayOf(ctx context.Context, a store.AccessApproval) (id, name string) {
	id = access.RelayOfKey(a.ProcessKey)
	if id == "" {
		return "", ""
	}
	if name = RelayName(ctx, s.cfg.DB, id); name == "" {
		name = "relay " + id
	}
	return id, name
}
