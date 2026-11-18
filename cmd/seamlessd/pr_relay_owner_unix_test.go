//go:build darwin || linux

package main

// The relay-owner matrix's peer_auth rows (owner decision R-D1): under
// peer_auth nothing changes. peer_auth.relays judges a relay's caller, and the
// owner's uid its pairing recorded is not consulted: a restricted account
// there is served as that account, the mapping's owner as the owner -- even
// where the pairing named another uid -- and a uid the mapping does not name
// is refused in the relay judge's words, never the edge's.

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/relay"
	"github.com/arctop/seamless/internal/store"
)

func TestPRRelayOwnerMatrix_UnderPeerAuthNothingChanges(t *testing.T) {
	d := newRelayvDaemon(t)
	ctx := context.Background()
	laptop, laptopID := d.pair(t, relayvLaptop)
	require.Equal(t, uint32(relayvOwnerUID), laptop.OwnerUID, "the pairing recorded the uid it named")

	restricted := relayvCaller{uid: relay2RestrictedUID, chain: relayvCallerA.chain}
	require.Equal(t, http.StatusOK, d.initStatus(t, d.httpClient(laptopID), restricted.relay2Headers(relay2Named)),
		"a restricted account there is served, as the mapping says, though it is not the uid the pairing named")
	require.Equal(t, http.StatusOK, d.initStatus(t, d.httpClient(laptopID), relayvCallerA.relay2Headers(relay2Named)))

	stranger := relayvCaller{uid: 503, chain: relayvCallerA.chain}
	status, body := d.do(t, d.httpClient(laptopID), http.MethodGet, "/console/?format=json", nil, stranger.relay2Headers(relay2Named))
	require.Equal(t, http.StatusForbidden, status, body)
	require.Contains(t, body, "is neither the owner nor a restricted account peer_auth.relays maps there", "the relay judge's words")
	require.NotContains(t, body, "This server runs without peer_auth", "never the edge's")

	// A pairing that named another uid: the mapping still judges.
	_, err := d.db.ExecContext(ctx, `UPDATE devices SET owner_uid = 777 WHERE id = ?`, laptop.ID)
	require.NoError(t, err)
	got, err := store.DeviceByID(ctx, d.db, laptop.ID)
	require.NoError(t, err)
	require.Equal(t, uint32(777), got.OwnerUID)
	require.Equal(t, http.StatusOK, d.initStatus(t, d.httpClient(laptopID), relayvCallerA.relay2Headers(relay2Named)),
		"owner_uid 501 in peer_auth.relays is the owner")
	require.Equal(t, http.StatusForbidden, d.initStatus(t, d.httpClient(laptopID),
		relayvCaller{uid: 777, chain: relayvCallerA.chain}.relay2Headers(relay2Named)),
		"the uid the pairing named is nobody the mapping knows")
}

// The relay's doctor warns about a pairing an earlier release made, which
// names no owner's uid, and says nothing more of one that names one.
func TestPRRelayOwnerRow(t *testing.T) {
	row, ok := relayOwnerRow(relay.State{DeviceID: "01EXAMPLEDEVICE"})
	require.True(t, ok)
	require.Equal(t, statusWarn, row.status)
	require.Equal(t, "relay.owner", row.name)
	require.Contains(t, row.detail, "re-pair to bind the owner's uid")
	require.Contains(t, row.detail, "revoke device 01EXAMPLEDEVICE on the server's Devices page")
	require.Contains(t, row.detail, "seamlessd relay pair --replace --owner-uid <your uid here>")
	_, ok = relayOwnerRow(relay.State{DeviceID: "01EXAMPLEDEVICE", OwnerUID: 501})
	require.False(t, ok)
}

// relay pair's owner uid: --owner-uid always wins; else sudo's caller; else
// it is refused, and root is never the owner.
func TestPRRelayOwnerUID_Sources(t *testing.T) {
	for _, tc := range []struct {
		flag, sudo string
		uid        uint32
		from, err  string
	}{
		{flag: "501", sudo: "", uid: 501, from: relayOwnerFlag},
		{flag: "501", sudo: "502", uid: 501, from: relayOwnerFlag},
		{flag: " 503 ", sudo: "0", uid: 503, from: relayOwnerFlag},
		{flag: "", sudo: "502", uid: 502, from: relayOwnerSudo},
		{flag: "", sudo: "", err: "--owner-uid is required"},
		{flag: "0", sudo: "502", err: "uid 0 is root, which is never the owner"},
		{flag: "", sudo: "0", err: "SUDO_UID: uid 0 is root"},
		{flag: "-1", sudo: "", err: `"-1" is not a uid`},
		{flag: "4294967296", sudo: "", err: "is not a uid"},
		{flag: "", sudo: "abc", err: `SUDO_UID: "abc" is not a uid`},
	} {
		uid, from, err := relayOwnerUID(tc.flag, tc.sudo)
		if tc.err != "" {
			require.ErrorContains(t, err, tc.err, "%+v", tc)
			continue
		}
		require.NoError(t, err, "%+v", tc)
		require.Equal(t, tc.uid, uid, "%+v", tc)
		require.Equal(t, tc.from, from, "%+v", tc)
	}
}

// Setup prints the relay's root step for the account it runs as, whose uid the
// pairing records as the owner's: run as root, it prints none.
func TestPRSetupRelayStep_RefusesRoot(t *testing.T) {
	env := []string{"SEAMLESS_SETUP=client", "SEAMLESS_SERVER_URL=https://box.example:8081", "SEAMLESS_PAIR_CODE=ABCD-EFGH-JKMN-PQRS"}
	r, out, _ := setupTestRun(t, "linux", env, setupFactsFresh, false, "")
	clientArrange(t, r, clientTrusted)
	st, err := clientFirstHalf(t, r)
	require.NoError(t, err, out.String())
	r.client.ownerUID = func() int { return 0 }
	out.Reset()
	err = setupHandOff(r, &st, setupStepsLeft(st, "linux"), true)
	require.ErrorContains(t, err, "setup runs as root here")
	require.NotContains(t, out.String(), "relay pair", "no root step is printed")
}
