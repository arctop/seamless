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
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/devices"
	"github.com/arctop/seamless/internal/screening"
	"github.com/arctop/seamless/internal/store"
)

// relayHandler judges, under peer_auth, every request a paired relay
// forwarded. It runs inside the device edge, which matched the relay's
// certificate (devices.CredentialOf) and read the relay's report of its local
// caller (devices.RelayAttributesOf), and outside the routes. ownerHandler,
// outside the edge, gave every TCP request the owner's view; that is what the
// bearer key and a key device keep, and what no relay's request keeps.
//
// For a relay peer_auth.relays maps, by its enrolled name:
//
//   - a pairing no passkey approved (one made with the code alone, while
//     peer_auth was off) is refused (403) until the owner approves it with a
//     passkey on the Devices page, where it waits in place (owner decision
//     R-D6: the same device and certificate, no pairing again): its report of
//     who is calling would be trusted from a relay the owner never vetted;
//   - a request that forwards no attributes is refused (400), never served as
//     the owner;
//   - a uid that is neither the mapping's owner_uid nor one of its restricted
//     accounts is refused (403), as the socket refuses an account peer_auth
//     does not know;
//   - the owner's uid there gets the owner's socket view through the relay
//     (Policy.RelayChainViewNamed): the forwarded chain, every process the
//     relay's, the programs the relay named for them (wire version 2; none
//     from version 1), and the leases live on it at now, judged by the View
//     (WithLeases). The chain is the owner's own hands there
//     (approval.JudgeRelayHands, owner decision R-D3) only when the relay
//     named every process of it, none is an agent by the judgment this
//     socket makes, it is whole by its evidence, and the pairing recorded
//     the same owner's uid; the request carries that judgment
//     (access.WithRelayHands), and access.FromAgent answers it. Any other
//     chain counts as an agent's: the owner's own decisions are refused;
//   - a restricted account there (the mapping's restricted list) gets that
//     account's view, labeled "<name>@<relay>", with its reach loaded per
//     request as peerHandler loads a restricted account's on this socket: the
//     projects' states now, of which the View keeps what the account's entry
//     there reaches (View.WithReach) -- the open projects that are not locked
//     for a plain entry, less or other for one that sets exclude or only; a
//     lookup that fails answers 503. An account whose entry serve disabled
//     (an exclude naming a project that does not exist) is refused (403);
//
// A relay the policy does not map is refused (403), whatever it forwards: its
// local callers are every account on its computer that reaches its socket,
// and the server cannot tell which one is the owner until peer_auth.relays
// says (review H1, wave nt1113e; it passed as the owner over TCP before).
//
// Any other request -- the bearer key, a key device -- passes as it came: the
// owner over TCP, holding no lease. So does the health check, which reads no
// caller at all: a relay probing the server for itself forwards no local
// caller, and is answered as anyone is.
func relayHandler(policy *access.Policy, leases peerLeases, logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cred, ok := devices.CredentialOf(r.Context())
		if !ok || cred.Kind != devices.KindRelay || r.URL.Path == healthzPath {
			next.ServeHTTP(w, r)
			return
		}
		if !policy.RelayMapped(cred.Name) {
			logger.Warn("peer_auth: refused a relay peer_auth.relays does not map", "relay", cred.Name, "device", cred.DeviceID)
			access.NoteRefusal(r.Context(), screening.RefusedPeerUnknown)
			http.Error(w, fmt.Sprintf("forbidden: relay %s is paired, but peer_auth.relays does not map it, so this server cannot "+
				"tell which account on that computer is asking: map it in the server's config (peer_auth.relays: {%s: {owner_uid: "+
				"<your uid there>}}) and restart the server", cred.Name, cred.Name), http.StatusForbidden)
			return
		}
		if !cred.Approved {
			logger.Warn("peer_auth: refused a relay paired with a code alone", "relay", cred.Name, "device", cred.DeviceID)
			access.NoteRefusal(r.Context(), screening.RefusedPeerUnknown)
			http.Error(w, relayCodeOnlyRefusal(cred.Name), http.StatusForbidden)
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
		view, known := policy.RelayChainViewNamed(cred.DeviceID, cred.Name, attrs.UID, attrs.Chain, attrs.Programs)
		if !known && policy.RelayRestrictedDisabled(cred.Name, attrs.UID) {
			logger.Error("peer_auth: refused a relay's restricted account whose entry is disabled: its exclude names a project "+
				"that does not exist (seamlessd doctor names it)", "relay", cred.Name, "uid", attrs.UID)
			access.NoteRefusal(r.Context(), screening.RefusedPeerUnknown)
			http.Error(w, fmt.Sprintf("forbidden: uid %d on relay %s: its peer_auth.relays restricted entry is disabled until the "+
				"owner corrects it (seamlessd doctor on the server names the cause)", attrs.UID, cred.Name), http.StatusForbidden)
			return
		}
		if !known {
			logger.Warn("peer_auth: refused a relay's caller", "relay", cred.Name, "uid", attrs.UID)
			access.NoteRefusal(r.Context(), screening.RefusedPeerUnknown)
			http.Error(w, fmt.Sprintf("forbidden: uid %d on relay %s is neither the owner nor a restricted account "+
				"peer_auth.relays maps there", attrs.UID, cred.Name), http.StatusForbidden)
			return
		}
		ctx := r.Context()
		switch view.Principal().Kind {
		case access.KindRestricted:
			// A restricted account's reach is drawn from the projects' states
			// right now, judged per request like a lease, as on this machine's
			// socket: the open ones, and the confidential or sealed ones an
			// entry's single only project may name. The View applies the
			// account's rule. A lookup that fails refuses the request rather
			// than serving it a reach it would misread as "no projects".
			var states store.RestrictedReachStates
			if leases.reach != nil {
				var err error
				if states, err = leases.reach(ctx); err != nil {
					logger.Warn("peer_auth: open projects unavailable; refusing a relay's restricted account", "relay", cred.Name,
						"uid", attrs.UID, "error", err)
					http.Error(w, "service unavailable: the project list could not be read", http.StatusServiceUnavailable)
					return
				}
			}
			ctx = access.WithView(ctx, view.WithReach(states.Open, states.Fenced))
		default:
			if policy.HasLocked() {
				view = relayLeasedView(ctx, view, leases, logger)
			}
			// The owner's own hands there, or not (owner decision R-D3): the
			// chain judged as this machine's socket judges one, with the
			// programs the relay named, whole by its evidence, and the
			// pairing's uid agreeing with the mapping's.
			hands := approval.JudgeRelayHands(view.Principal(), cred.Name, cred.OwnerUID)
			ctx = access.WithView(ctx, view)
			ctx = access.WithAgentChain(ctx, func() bool { return !hands.Owner() })
			ctx = access.WithRelayHands(ctx, hands)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// relayCodeOnlyRefusal is relayHandler's answer to a mapped relay its
// one-time code alone paired: the rule, and the approval in place that ends
// it (owner decision R-D6), which keeps the relay's pairing.
func relayCodeOnlyRefusal(relay string) string {
	return fmt.Sprintf("forbidden: relay %s was paired with a one-time code alone, before peer_auth: approve it with a "+
		"passkey on the server's Devices page, where it waits for you; it keeps its pairing, and need not pair again", relay)
}

// healthzPath is the daemon's health check (daemonRoutes), the one route that
// answers every caller the same: the one the device edge's own judge of a
// relay's caller skips too (devices.HealthPath).
const healthzPath = devices.HealthPath

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
				"peer_auth: its requests are refused until you approve it with a passkey on the Devices page, where it "+
				"waits: it keeps its pairing, and need not pair again", n)})
		default:
			line := fmt.Sprintf("relay %s: its requests are judged as this socket's, and uid %d there is the owner", n, p.Relays[n].OwnerUID)
			if restricted := relayRestrictedWords(p.Relays[n]); restricted != "" {
				line += "; restricted there: " + restricted
			}
			out = append(out, check{statusOK, name, line})
			if mismatch, ok := relayOwnerMismatch(*dev, p.Relays[n].OwnerUID); ok {
				out = append(out, check{statusWarn, name, mismatch})
			}
		}
	}
	return out
}

// relayOwnerMismatch is doctor's word, under peer_auth, on a mapped relay
// whose pairing recorded another owner's uid there than peer_auth.relays maps
// (owner decision R-D1): the mapping judges, so nothing changes, but one of the
// two is wrong. ok is false when they agree, or the pairing recorded none.
func relayOwnerMismatch(d store.Device, mapped uint32) (string, bool) {
	if d.OwnerUID == 0 || d.OwnerUID == mapped {
		return "", false
	}
	return fmt.Sprintf("relay %s paired naming uid %d there as the owner, and peer_auth.relays maps owner_uid %d: one of them is "+
		"wrong. This server judges by owner_uid, so correct it there if %d is the account your agents run as on that computer "+
		"(id -u there), or pair the relay again with --replace --owner-uid <the right uid>", d.Name, d.OwnerUID, mapped, d.OwnerUID), true
}

// relayRestrictedWords lists a relay's restricted accounts for doctor: each
// "<name> (uid N)", sorted by name; "" for none. An account whose entry
// shapes its reach is listed the same way: its shape, and what it reaches
// now, are its own row (relayRestrictedRows).
func relayRestrictedWords(r config.PeerRelay) string {
	names := slices.Sorted(maps.Keys(r.Restricted))
	words := make([]string, 0, len(names))
	for _, n := range names {
		words = append(words, fmt.Sprintf("%s (uid %d)", n, r.Restricted[n].UID))
	}
	return strings.Join(words, ", ")
}

// relayMappedIn reports, for device pairing (devices.Config.RelayMapped),
// whether p maps the relay enrolled as name in peer_auth.relays: the keys
// Policy.RelayMapped reads, as the config names them.
func relayMappedIn(p config.PeerAuth) func(name string) bool {
	return func(name string) bool {
		_, ok := p.Relays[name]
		return ok
	}
}
