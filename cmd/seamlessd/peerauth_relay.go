package main

// peer_auth through a relay (plan networked-relay). A relay on another
// computer serves the same Unix socket path agents use beside a peer_auth
// daemon, reads each local caller's uid and process chain from its own
// kernel, and forwards the request here over TLS with its client
// certificate. The device edge (devices.Service.Edge) matched that
// certificate to the paired relay and read what it forwarded; this file
// turns it into the request's View, exactly as peerHandler turns this
// machine's socket peer into one -- for a relay peer_auth.relays maps, and
// only under peer_auth. The policy is internal/access's
// (Policy.RelayChainView); this is the transport's half.

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/devices"
	"github.com/arctop/seamless/internal/screening"
	"github.com/arctop/seamless/internal/store"
)

// relayHandler judges, under peer_auth, every request a paired relay
// forwarded. It runs inside the device edge, which matched the relay's
// certificate (devices.CredentialOf) and read the relay's report of its local
// caller (devices.RelayAttributesOf), and outside the routes. ownerHandler,
// outside the edge, gave every TCP request the owner's view; that is what
// every request but a mapped relay's keeps.
//
// For a relay peer_auth.relays maps, by its enrolled name:
//
//   - a pairing no passkey approved (one made with the code alone, while
//     peer_auth was off) is refused (403) until the relay pairs again: its
//     report of who is calling would be trusted from a relay the owner never
//     vetted;
//   - a request that forwards no attributes is refused (400), never served as
//     the owner;
//   - a uid other than the mapping's owner_uid is refused (403), as the socket
//     refuses an account peer_auth does not know;
//   - the owner's uid there gets the owner's socket view through the relay
//     (Policy.RelayChainView): the forwarded chain, every process the
//     relay's, and the leases live on it at now, judged by the View
//     (WithLeases). A relay names no program, so the chain counts as an
//     agent's (access.FromAgent): the owner's own decisions are refused from
//     it, as from a socket chain that cannot be walked.
//
// Any other request -- the bearer key, a key device, a relay the policy does
// not map -- passes as it came: the owner over TCP, holding no lease. So does
// the health check, which reads no caller at all: a relay probing the server
// for itself forwards no local caller, and is answered as anyone is.
func relayHandler(policy *access.Policy, leases peerLeases, logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cred, ok := devices.CredentialOf(r.Context())
		if !ok || cred.Kind != devices.KindRelay || !policy.RelayMapped(cred.Name) || r.URL.Path == healthzPath {
			next.ServeHTTP(w, r)
			return
		}
		if !cred.Approved {
			logger.Warn("peer_auth: refused a relay paired with a code alone", "relay", cred.Name, "device", cred.DeviceID)
			access.NoteRefusal(r.Context(), screening.RefusedPeerUnknown)
			http.Error(w, fmt.Sprintf("forbidden: relay %s was paired with a one-time code alone, before peer_auth: pair it "+
				"again, and approve it with a passkey on the server's Devices page", cred.Name), http.StatusForbidden)
			return
		}
		attrs, forwarded := devices.RelayAttributesOf(r.Context())
		if !forwarded {
			logger.Warn("peer_auth: a relay request forwarded no caller", "relay", cred.Name)
			access.NoteRefusal(r.Context(), screening.RefusedPeerUnknown)
			http.Error(w, "bad request: "+devices.ErrRelayAttributes.Error()+": a relay forwards its caller's "+
				devices.RelayVersionHeader+", "+devices.RelayUIDHeader+" and "+devices.RelayChainHeader+" with every request",
				http.StatusBadRequest)
			return
		}
		view, known := policy.RelayChainView(cred.DeviceID, cred.Name, attrs.UID, attrs.Chain)
		if !known {
			logger.Warn("peer_auth: refused a relay's caller", "relay", cred.Name, "uid", attrs.UID)
			access.NoteRefusal(r.Context(), screening.RefusedPeerUnknown)
			http.Error(w, fmt.Sprintf("forbidden: uid %d on relay %s is not the owner peer_auth.relays maps there", attrs.UID, cred.Name),
				http.StatusForbidden)
			return
		}
		if policy.HasLocked() {
			view = relayLeasedView(r.Context(), view, leases, logger)
		}
		ctx := access.WithView(r.Context(), view)
		ctx = access.WithAgentChain(ctx, func() bool { return true })
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// healthzPath is the daemon's health check (daemonRoutes), the one route that
// answers every caller the same.
const healthzPath = "/healthz"

// relayLeasedView attaches the leases live on a relay view's chain at now. It
// fails closed as leasedView does: no chain, or a lookup that fails, leaves
// the view without a lease, on which every locked project stays locked.
func relayLeasedView(ctx context.Context, view access.View, leases peerLeases, logger *slog.Logger) access.View {
	if !view.Principal().HasProcess || leases.load == nil {
		return view
	}
	now := time.Now
	if leases.now != nil {
		now = leases.now
	}
	at := now()
	rows, err := leases.load(ctx, view.Principal().ChainKeys(), at)
	if err != nil {
		logger.Warn("peer_auth: a relay's lease lookup failed; locked projects stay locked", "error", err)
		return view
	}
	return view.WithLeases(at, rows)
}

// relayMappingChecks is doctor's peer_auth.relays section: nothing unless
// peer_auth maps a relay. Each mapping must name a relay paired here and
// approved with a passkey, or its requests are refused.
func relayMappingChecks(ctx context.Context, cfg config.Config, db *sql.DB) []check {
	const name = "peer_auth relays"
	p := cfg.PeerAuth
	if !p.Enabled || len(p.Relays) == 0 {
		return nil
	}
	names := make([]string, 0, len(p.Relays))
	for n := range p.Relays {
		names = append(names, n)
	}
	slices.Sort(names)
	if !pairingOn(cfg) {
		return []check{{statusWarn, name, "peer_auth.relays maps " + strings.Join(names, ", ") + ", but this install serves " +
			"no other computer, so no relay reaches it: give it a TLS listener its relays can reach"}}
	}
	if db == nil {
		return []check{{statusInfo, name, "maps " + strings.Join(names, ", ") + "; no database to read the pairings from"}}
	}
	paired, err := store.ListDevices(ctx, db)
	if err != nil {
		return []check{{statusFail, name, "paired devices: " + err.Error()}}
	}
	var out []check
	for _, n := range names {
		var dev *store.Device
		for i := range paired {
			if d := paired[i]; d.Kind == store.DeviceRelayKind && d.Name == n && d.Status == store.DeviceActive {
				dev = &paired[i]
			}
		}
		switch {
		case dev == nil:
			out = append(out, check{statusWarn, name, fmt.Sprintf("peer_auth.relays maps %s, which no paired relay holds: "+
				"pair it, or correct the name to the one the Devices page shows", n)})
		case dev.ApprovedBy == "":
			out = append(out, check{statusWarn, name, fmt.Sprintf("relay %s was paired with a one-time code alone, before "+
				"peer_auth: its requests are refused until it pairs again and you approve it with a passkey", n)})
		default:
			out = append(out, check{statusOK, name, fmt.Sprintf("relay %s: its requests are judged as this socket's, and uid %d "+
				"there is the owner", n, p.Relays[n].OwnerUID)})
		}
	}
	return out
}
