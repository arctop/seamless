package main

// Doctor's peer_auth devices section (plan networked-relay; review I2, wave
// nt1113f): the key devices peer_auth never vetted.

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/store"
)

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
