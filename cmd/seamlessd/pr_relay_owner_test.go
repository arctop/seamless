package main

// The relay-owner rows of the matrix (owner decision R-D1, 2026-11-15):
// serve's TCP stack without peer_auth, with pairing on, and relays paired
// through the real flow. A relay whose pairing named the owner's uid on its
// computer is the owner for that uid, in every place, and refused for every
// other uid with the remedy; one that forwards no caller is refused; the
// health check is not judged; a relay whose pairing named none (paired before,
// or by a relay of an earlier release) is served as before, and doctor and
// the Devices page say to re-pair it.

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/devices"
	"github.com/arctop/seamless/internal/store"
)

// prPairUnboundRelay pairs a relay named name as a relay of an earlier
// release pairs: naming no owner's uid.
func (d *deviceDaemon) prPairUnboundRelay(t *testing.T, name string) (store.Device, *devices.RelayIdentity) {
	t.Helper()
	ctx := context.Background()
	id, err := devices.NewRelayIdentity(name)
	require.NoError(t, err)
	code, err := d.pairing.MintCode(ctx, store.DeviceByConsole)
	require.NoError(t, err)
	secret, err := devices.NewSecret()
	require.NoError(t, err)
	answer, err := d.pairing.Pair(ctx, devices.PairInput{Code: code.Code, Kind: devices.KindRelay, Name: name, Secret: secret,
		RelayKey: id.PublicKey()})
	require.NoError(t, err)
	require.True(t, answer.Device.OwnerUnbound())
	return answer.Device, id
}

// prDeviceDoBody is deviceDo returning the body as well.
func (d *deviceDaemon) prDeviceDoBody(t *testing.T, client *http.Client, p devicePlace) (int, string) {
	t.Helper()
	req, err := http.NewRequest(p.method, d.srv.URL+p.path, strings.NewReader(p.body))
	require.NoError(t, err)
	for k, v := range p.header {
		req.Header[k] = v
	}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(body)
}

func TestPRRelayOwnerMatrix_WithoutPeerAuth(t *testing.T) {
	d := deviceNewDaemon(t, false)
	ctx := context.Background()
	bound, boundID := d.devicePairRelay(t, "example-laptop")
	require.Equal(t, uint32(deviceRelayOwner), bound.OwnerUID, "the pairing recorded the owner's uid it named")
	via := d.deviceHTTP(boundID)

	t.Run("the owner's uid through the relay is the owner, in every place", func(t *testing.T) {
		for _, p := range devicePlaces() {
			require.Equal(t, http.StatusOK, d.deviceDo(t, via, p.withHeaders(deviceRelayCaller(deviceRelayOwner)), ""), p.name)
		}
	})

	t.Run("another uid through the same relay is refused, naming the remedy, in every place", func(t *testing.T) {
		var sessions int
		require.NoError(t, d.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&sessions))
		for _, uid := range []uint32{502, 0, 65534} {
			for _, p := range devicePlaces() {
				status, body := d.prDeviceDoBody(t, via, p.withHeaders(deviceRelayCaller(uid)))
				require.Equal(t, http.StatusForbidden, status, "uid %d, %s: %s", uid, p.name, body)
				require.Contains(t, body, "is not the owner's uid there", "uid %d, %s", uid, p.name)
				require.Contains(t, body, "turn peer_auth on and map it as restricted", "uid %d, %s: the remedy", uid, p.name)
			}
		}
		var after int
		require.NoError(t, d.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&after))
		require.Equal(t, sessions, after, "nothing a refused caller sent was recorded")
		status, body := d.prDeviceDoBody(t, via, devicePlace{"console", http.MethodPost, "/console/devices/code", "",
			http.Header{"Accept": {"application/json"}}}.withHeaders(deviceRelayCaller(502)))
		require.Equal(t, http.StatusForbidden, status, "no pairing code is minted through it: %s", body)
	})

	t.Run("a request that forwards no caller is refused", func(t *testing.T) {
		for _, p := range devicePlaces() {
			status, body := d.prDeviceDoBody(t, via, p)
			require.Equal(t, http.StatusBadRequest, status, "%s: %s", p.name, body)
			require.Contains(t, body, "this server serves uid 501 there, the owner's, and no one else through it", p.name)
		}
		status, body := d.prDeviceDoBody(t, via, devicePlace{"healthz", http.MethodGet, "/healthz", "", nil})
		require.Equal(t, http.StatusOK, status, "the relay's own probe forwards no caller, and is not judged: %s", body)
	})

	t.Run("a relay paired naming no uid is served as before, for every uid", func(t *testing.T) {
		_, oldID := d.prPairUnboundRelay(t, "example-desk")
		for _, h := range []http.Header{deviceRelayCaller(deviceRelayOwner), deviceRelayCaller(502), nil} {
			for _, p := range devicePlaces() {
				require.Equal(t, http.StatusOK, d.deviceDo(t, d.deviceHTTP(oldID), p.withHeaders(h), ""), "%s %v", p.name, h)
			}
		}
	})

	t.Run("a key device and the shared key are judged as before", func(t *testing.T) {
		_, key := d.devicePairKey(t, "example-ci")
		for _, p := range devicePlaces() {
			require.Equal(t, http.StatusOK, d.deviceDo(t, d.deviceHTTP(nil), p.withHeaders(deviceRelayCaller(502)), "Bearer "+key), p.name)
			require.Equal(t, http.StatusOK, d.deviceDo(t, d.deviceHTTP(nil), p, "Bearer "+deviceSharedKey), p.name)
		}
	})
}

// prPairingCfg is a config that pairs devices (a TLS listener), with
// peer_auth as asked; peer_auth maps example-laptop's owner as uid 501.
func prPairingCfg(peerAuth bool) config.Config {
	cfg := config.Defaults()
	cfg.TLS.CertFile, cfg.TLS.KeyFile = "/example/cert.pem", "/example/key.pem"
	if peerAuth {
		cfg.PeerAuth = config.PeerAuth{Enabled: true, Owner: "example-owner",
			Relays: map[string]config.PeerRelay{"example-laptop": {OwnerUID: 501}}}
	}
	return cfg
}

// Doctor names, without peer_auth, each relay whose pairing named no uid,
// with the remedy, and lists the bound ones; under peer_auth it says nothing
// of them, but warns where the uid a pairing named is not the one
// peer_auth.relays maps.
func TestPRRelayOwnerDoctor(t *testing.T) {
	d := deviceNewDaemon(t, false)
	ctx := context.Background()
	bound, _ := d.devicePairRelay(t, "example-laptop")
	unbound, _ := d.prPairUnboundRelay(t, "example-desk")
	d.devicePairKey(t, "example-ci")

	off := prPairingCfg(false)
	require.True(t, pairingOn(off))
	rows := relayOwnerDeviceChecks(ctx, off, d.db)
	require.Len(t, rows, 2, "%v", rows)
	require.Equal(t, statusWarn, rows[0].status)
	require.Contains(t, rows[0].detail, "relay example-desk ("+unbound.ID+") paired before this server recorded the owner's uid")
	require.Contains(t, rows[0].detail, "re-pair it to bind the owner's uid")
	require.Contains(t, rows[0].detail, "seamlessd relay pair --replace --owner-uid <your uid there>")
	require.Equal(t, statusInfo, rows[1].status)
	require.Equal(t, "served for the owner's uid there alone, recorded when each paired: example-laptop (uid 501)", rows[1].detail)
	require.Nil(t, relayOwnerDeviceChecks(ctx, prPairingCfg(true), d.db), "under peer_auth peer_auth.relays judges")
	require.Nil(t, relayOwnerDeviceChecks(ctx, off, nil))

	// Under peer_auth a mapped relay whose pairing named another uid.
	on := prPairingCfg(true)
	// Approved with a passkey, as every pairing under peer_auth is.
	_, err := d.db.ExecContext(ctx, `UPDATE devices SET approved_by = '01PASSKEYEXAMPLE' WHERE id = ?`, bound.ID)
	require.NoError(t, err)
	mapping := relayMappingChecks(ctx, on, d.db)
	require.Len(t, mapping, 1, "the pairing and the mapping agree: %v", mapping)
	require.Equal(t, statusOK, mapping[0].status)
	_, err = d.db.ExecContext(ctx, `UPDATE devices SET owner_uid = 777 WHERE id = ?`, bound.ID)
	require.NoError(t, err)
	mapping = relayMappingChecks(ctx, on, d.db)
	require.Len(t, mapping, 2, "%v", mapping)
	require.Equal(t, statusOK, mapping[0].status, "the mapping still judges")
	require.Equal(t, statusWarn, mapping[1].status)
	require.Contains(t, mapping[1].detail, "relay example-laptop paired naming uid 777 there as the owner, and peer_auth.relays maps owner_uid 501")
}
