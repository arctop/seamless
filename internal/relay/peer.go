package relay

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/arctop/seamless/internal/access"
)

// peerConn is the kernel's account of one local connection, read once at
// accept (ConnContext): the caller's uid, and the process that connected,
// whose chain is walked per request.
type peerConn struct {
	uid    uint32
	uidErr error
	// chain walks the connecting process's parent chain (access.Peer.Chain):
	// the tree as it is when the request runs. Nil when the kernel named no
	// process for the connection (chainErr says why).
	chain    func() ([]access.ChainHop, error)
	chainErr error
}

type peerKey struct{}

// withPeer returns ctx carrying p.
func withPeer(ctx context.Context, p peerConn) context.Context {
	return context.WithValue(ctx, peerKey{}, p)
}

// ConnContext is the relay server's http.Server.ConnContext: it reads the
// connecting process's uid and process from this machine's kernel, once per
// connection, at accept (access.PeerUID, access.ReadPeer), as serve's
// peerConnContext does for the daemon's own socket. It never fails the
// connection: the Forwarder refuses each request of one the kernel named no
// caller for.
func ConnContext(ctx context.Context, c net.Conn) context.Context {
	p := peerConn{}
	p.uid, p.uidErr = access.PeerUID(c)
	if p.uidErr == nil {
		if peer, err := access.ReadPeer(c); err != nil {
			p.chainErr = err
		} else {
			p.chain = peer.Chain
		}
	}
	return withPeer(ctx, p)
}

// Attributes are what the relay tells the server about one request's local
// caller, read from its own kernel: the uid, and the kernel-verified process
// chain as access.ProcessToken keys, the peer first.
type Attributes struct {
	UID   uint32
	Chain []string
}

var (
	// errNoPeer is a request whose connection carries no kernel account at
	// all: not one the relay's own listener accepted.
	errNoPeer = errors.New("this connection has no peer credential")
	// errEmptyChain is a walk that named no process.
	errEmptyChain = errors.New("the kernel named no process for this connection")
)

// attributesOf reads the request's local caller from ctx: the uid read at
// accept, and the chain walked now. Every failure refuses the request: a
// relay request without them is one the server refuses, and the relay never
// sends one it knows to be incomplete.
func attributesOf(ctx context.Context) (Attributes, error) {
	p, ok := ctx.Value(peerKey{}).(peerConn)
	switch {
	case !ok:
		return Attributes{}, errNoPeer
	case p.uidErr != nil:
		return Attributes{}, fmt.Errorf("%w: %w", errNoPeer, p.uidErr)
	case p.chain == nil:
		if p.chainErr != nil {
			return Attributes{}, fmt.Errorf("%w: %w", errEmptyChain, p.chainErr)
		}
		return Attributes{}, errEmptyChain
	}
	hops, err := p.chain()
	if err != nil {
		return Attributes{}, fmt.Errorf("the process chain of this connection could not be read: %w", err)
	}
	if len(hops) == 0 {
		return Attributes{}, errEmptyChain
	}
	if len(hops) > access.MaxChainHops {
		hops = hops[:access.MaxChainHops]
	}
	keys := make([]string, 0, len(hops))
	for _, h := range hops {
		k := h.Token.Key()
		if k == "" || strings.Contains(k, ",") {
			return Attributes{}, fmt.Errorf("the kernel named a process with no valid token (pid %d)", h.Token.PID)
		}
		keys = append(keys, k)
	}
	return Attributes{UID: p.uid, Chain: keys}, nil
}
