package main

// Doctor's devices sections (plan networked-relay): under peer_auth, the key
// devices peer_auth never vetted (review I2, wave nt1113f); without it, which
// uid each relay is served for (owner decision R-D1).

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/store"
)

// relayOwnerDeviceChecks is doctor's relay owners section, while peer_auth is
// off: the edge serves a relay whose pairing recorded the owner's uid there
// for that uid alone (devices.Service.Edge), and one whose pairing recorded
// none -- paired before this release, or by a relay of an earlier one -- for
// every caller, as before. Each of those is a warn of its own, naming the
// remedy; the bound ones are one info row. Nothing under peer_auth, where
// peer_auth.relays judges (relayMappingChecks compares the two), while this
// install pairs nothing, or without a current database.
func relayOwnerDeviceChecks(ctx context.Context, cfg config.Config, db *sql.DB) []check {
	const name = "relay owners"
	if cfg.PeerAuth.Enabled || !pairingOn(cfg) || db == nil {
		return nil
	}
	list, err := store.ListDevices(ctx, db)
	if err != nil {
		return []check{{statusFail, name, "paired devices: " + err.Error()}}
	}
	var out []check
	var bound []string
	for _, d := range list {
		if d.Kind != store.DeviceRelayKind || d.Status != store.DeviceActive {
			continue
		}
		if !d.OwnerUnbound() {
			bound = append(bound, fmt.Sprintf("%s (uid %d)", d.Name, d.OwnerUID))
			continue
		}
		out = append(out, check{statusWarn, name, relayUnboundWords(d)})
	}
	if len(bound) > 0 {
		out = append(out, check{statusInfo, name, "served for the owner's uid there alone, recorded when each paired: " +
			strings.Join(bound, ", ")})
	}
	return out
}

// relayUnboundWords says what a relay whose pairing recorded no owner's uid
// means on a server without peer_auth, and the remedy: re-pair to bind the
// owner's uid.
func relayUnboundWords(d store.Device) string {
	return fmt.Sprintf("relay %s (%s) paired before this server recorded the owner's uid there (or through a relay of an earlier "+
		"release), so every account on that computer that reaches its socket is you here: re-pair it to bind the owner's uid -- "+
		"revoke it (Revoke on the Devices page, or seamlessd devices revoke %s), then on that computer, with a relay of this "+
		"release, seamlessd relay pair --replace --owner-uid <your uid there>", d.Name, d.ID, d.ID)
}

// codeOnlyDeviceChecks names, under peer_auth, each key device paired with a
// one-time code alone, while peer_auth was off: a warn apiece. Under peer_auth
// every pairing waits for the owner's passkey, and a relay no passkey
// approved is refused (relayHandler: unmapped, or mapped and code-only; doctor
// says so for a mapped one in relayMappingChecks). A key device is no relay:
// it is the owner over TCP, as the bearer key is (no lease, so never a locked
// project), and one paired by its code alone keeps that after peer_auth is
// turned on, though no passkey ever approved it. Revoking it and pairing it
// again puts it behind the owner's passkey. Nothing while peer_auth is off,
// while this install pairs nothing, or without a current database.
func codeOnlyDeviceChecks(ctx context.Context, cfg config.Config, db *sql.DB) []check {
	const name = "peer_auth devices"
	if !cfg.PeerAuth.Enabled || !pairingOn(cfg) || db == nil {
		return nil
	}
	list, err := store.ListDevices(ctx, db)
	if err != nil {
		return []check{{statusFail, name, "paired devices: " + err.Error()}}
	}
	var out []check
	for _, d := range list {
		if d.Kind != store.DeviceKeyKind || d.Status != store.DeviceActive || d.ApprovedBy != "" {
			continue
		}
		out = append(out, check{statusWarn, name, fmt.Sprintf("key device %s (%s) was paired with a one-time code alone, "+
			"before peer_auth, and is still you over TCP, as the bearer key is, though no passkey approved it: revoke and pair "+
			"again, so a passkey approves it (Revoke on the Devices page, or seamlessd devices revoke %s)", d.Name, d.ID, d.ID)})
	}
	return out
}
