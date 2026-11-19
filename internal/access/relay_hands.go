package access

import (
	"context"
	"slices"
)

// The owner's hands through a relay (owner decision R-D3, 2026-11-15). The
// owner's own decisions -- held writes, a deny, a revoke, a pairing code, the
// features, backups -- are refused from a process an agent started
// (FromAgent), and a paired device's credential makes none of them. A relay
// peer_auth.relays maps forwards, with every request, the process chain its
// own kernel named for the caller, and from the wire's version 2 the program
// each process of it runs. When that chain is the owner's own terminal there
// -- every process named, none of them an agent, the whole chain up to the
// session's root, the owner's uid there as both the mapping and the pairing
// name it -- the request is the owner's hands, judged as this machine's
// socket judges one. The server then trusts root on that computer to report
// the chain truthfully, as it trusts its own kernel here.
//
// The transport judges (cmd/seamlessd's relayHandler, with the anchor rule's
// classifier: approval.JudgeRelayHands) and carries the judgment on the
// request (WithRelayHands); the console asks OwnerHandsThrough. With peer_auth
// off there is no judge, no request carries a judgment, and a relay's
// credential stays refused. A key device is never judged.

// RelayHands is the judgment, for one request a mapped relay forwarded under
// peer_auth, of whether its caller is the owner's own hands on the relay's
// computer.
type RelayHands struct {
	// Relay is the relay's device id and Name its enrolled name.
	Relay string
	Name  string
	// Programs are the programs of the forwarded chain, the peer first, as
	// the relay's kernel named them ("" for a process it named none for; nil
	// from a relay of wire version 1).
	Programs []string
	// Why is "" when the caller is the owner's own hands; otherwise why it
	// is not, in words the refusal shows.
	Why string
}

// Owner reports whether h judged the caller the owner's own hands.
func (h RelayHands) Owner() bool { return h.Relay != "" && h.Why == "" }

type relayHandsKey struct{}

// WithRelayHands returns ctx carrying h. Only the transport's relay judge
// calls it, for a relay request it judged.
func WithRelayHands(ctx context.Context, h RelayHands) context.Context {
	h.Programs = slices.Clone(h.Programs)
	return context.WithValue(ctx, relayHandsKey{}, h)
}

// RelayHandsOf returns the judgment ctx carries, if the transport judged
// this request.
func RelayHandsOf(ctx context.Context) (RelayHands, bool) {
	h, ok := ctx.Value(relayHandsKey{}).(RelayHands)
	return h, ok && h.Relay != ""
}

// OwnerHandsThrough reports whether the request ctx carries came through the
// relay whose device id is relay as the owner's own hands: the transport
// judged it so (RelayHands.Owner), for that relay, and the request's View is
// the owner's socket view through that same relay. Anything else -- no
// judgment, another relay's, a restricted account, a view the transport
// never built -- is false: the refusal stands.
func OwnerHandsThrough(ctx context.Context, relay string) (RelayHands, bool) {
	h, ok := RelayHandsOf(ctx)
	if !ok || !h.Owner() || relay == "" || h.Relay != relay {
		return RelayHands{}, false
	}
	v, ok := FromContext(ctx)
	if !ok {
		return RelayHands{}, false
	}
	who := v.Principal()
	if who.Kind != KindOwner || !who.Socket || who.Relay != relay || !who.Named() {
		return RelayHands{}, false
	}
	return h, true
}
