package approval

// The owner's hands through a relay (owner decision R-D3, 2026-11-15; the
// rule is access.RelayHands'). A mapped relay of wire version 2 names the
// program of every process of the chain it forwards, as its own kernel names
// it, and the anchor rule's one classifier (anchor.go) judges them as it
// judges this machine's: a request is the owner's own hands there only when
// no process of its chain is an agent (IsAgentProgram, the predicate the
// daemon's own socket asks: chainHasAgent).
//
// A relay sends no word on whether its walk reached the top of the tree: the
// walk stops below pid 1, at access.MaxChainHops, or at a link it cannot
// verify, and the wire carries the chain alone. So a whole chain is judged by
// what it shows: shorter than MaxChainHops (the bound cuts a longer one at
// exactly that many), and ending at a session's root -- a terminal, a
// terminal multiplexer, an editor that hosts terminals, an application, a
// login or remote-login process, or the system's process manager, the
// processes the owner's terminal hangs from. A chain that ends at a shell, a
// privilege switch (su, sudo, doas) or any other program may have been cut
// short above it, or detached from the session that started it, and is
// refused: a cut chain could hide the agent above it.

import (
	"fmt"
	"strings"

	"github.com/arctop/seamless/internal/access"
)

// notSessionRoot are the broad ancestors that are never the top of a whole
// chain: each runs under something else (a terminal, a login), so a chain
// that ends at one stopped short of it.
var notSessionRoot = map[string]bool{"su": true, "sudo": true, "doas": true}

// sessionRoot reports whether prog, the topmost process of a relay's chain,
// is a session's root (see the file comment).
func sessionRoot(prog string) bool {
	c := classify(prog)
	return c.kind == programBroad && c.category != broadShell && !notSessionRoot[strings.ToLower(c.base)]
}

// JudgeRelayHands judges whether who, the principal of a request a mapped
// relay forwarded under peer_auth (access.Policy.RelayChainViewNamed), is the
// owner's own hands on the relay's computer. name is the relay's enrolled
// name and recordedUID the owner's uid there its pairing recorded
// (store.Device.OwnerUID; 0 for none). All must hold:
//
//   - who is the owner's view through that relay: the mapping's owner_uid,
//     the forwarded chain attached;
//   - the relay named every process of the chain (wire version 2, no entry
//     empty);
//   - no process of it is an agent (IsAgentProgram);
//   - the chain is whole by its evidence (file comment);
//   - the pairing recorded the owner's uid there, and it is owner_uid: a
//     pairing that recorded none, or another, leaves one of the two in doubt.
//
// Anything else is refused, and Why says why. The judgment is the transport's
// to carry (access.WithRelayHands).
func JudgeRelayHands(who access.Principal, name string, recordedUID uint32) access.RelayHands {
	return access.RelayHands{Relay: who.Relay, Name: name, Programs: who.Programs, Why: relayHandsRefusal(who, name, recordedUID)}
}

// relayHandsRefusal is why who is not the owner's own hands through the relay
// enrolled as name; "" when it is.
func relayHandsRefusal(who access.Principal, name string, recordedUID uint32) string {
	switch {
	case who.Relay == "" || who.Kind != access.KindOwner || !who.Socket:
		return "it is not the owner's account on that computer"
	case !who.HasProcess:
		return "the process chain it forwarded is not one this server can attach"
	case !who.Named():
		return "relay " + name + " names no program for the processes it forwards (a relay of wire version 1): " +
			"update Seamless on that computer"
	}
	for i, prog := range who.Programs {
		if !IsAgentProgram(prog) {
			continue
		}
		label := AgentLabelOn(prog, who.Chain[i].PID, name)
		if agentName(prog) == "" {
			return label + " is in its process chain, and an agent may run as it (Claude Code installed with npm runs as node)"
		}
		return label + " is in its process chain: an agent started it"
	}
	for i, prog := range who.Programs {
		if strings.TrimSpace(prog) == "" {
			return fmt.Sprintf("its relay named no program for pid %d there, so this server cannot tell that no agent started it",
				who.Chain[i].PID)
		}
	}
	if len(who.Chain) >= access.MaxChainHops {
		return fmt.Sprintf("its process chain is %d processes long, the most a relay forwards, so it may have been cut short",
			len(who.Chain))
	}
	if top := len(who.Chain) - 1; !sessionRoot(who.Programs[top]) {
		return fmt.Sprintf("its process chain ends at %s, not at a terminal, a terminal multiplexer, a login session or the "+
			"system's process manager, so it may have been cut short or detached from the session that started it",
			AgentLabelOn(who.Programs[top], who.Chain[top].PID, name))
	}
	switch {
	case recordedUID == 0:
		return "relay " + name + "'s pairing recorded no owner's uid there (a relay of an earlier release): pair it again " +
			"on that computer with seamlessd relay pair --replace --owner-uid <your uid there>"
	case recordedUID != who.UID:
		return fmt.Sprintf("relay %s's pairing recorded uid %d there as the owner's, and peer_auth.relays maps owner_uid %d: "+
			"correct the one that is wrong", name, recordedUID, who.UID)
	}
	return ""
}
