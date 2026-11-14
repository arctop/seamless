package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/devices"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// judgePolicyWith is a peer_auth policy locking example-finance and mapping
// relays, the owner being uid 501.
func judgePolicyWith(t *testing.T, relays map[string]config.PeerRelay) *access.Policy {
	t.Helper()
	p, err := access.New(config.PeerAuth{Enabled: true, Owner: "owner", ForceLocked: []string{relayvFin}, Relays: relays},
		func(string) (access.Account, error) { return access.Account{UID: relayvOwnerUID, Name: "owner"}, nil })
	require.NoError(t, err)
	return p
}

// judgeServe runs one request through ownerHandler and relayHandler to a
// probe, with cred (when set) on its context as the device edge marks a
// matched device, and returns the status, whether the probe ran, and the
// view it saw.
func judgeServe(t *testing.T, p *access.Policy, cred *devices.Credential) (int, bool, access.View) {
	t.Helper()
	var view access.View
	reached := false
	probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		view, _ = access.FromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := ownerHandler(p, relayHandler(p, peerLeases{}, logger, probe))
	req := httptest.NewRequest(http.MethodPost, "https://127.0.0.1/api/mcp", nil)
	if cred != nil {
		req = req.WithContext(devices.WithCredential(req.Context(), *cred))
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr.Code, reached, view
}

// The relay judge touches only a relay's requests: the bearer key and a key
// device keep the owner's TCP view, unread. A relay the policy does not map is
// refused (review H1: its callers are every account on its computer). A mapped
// relay that forwards nothing is refused, never served as the owner, and so is
// one paired with a code alone.
func TestRelayHandler_OnlyAMappedRelaysRequests(t *testing.T) {
	p := judgePolicyWith(t, map[string]config.PeerRelay{relayvLaptop: {OwnerUID: relayvOwnerUID}})
	for name, cred := range map[string]*devices.Credential{
		"the bearer key": nil,
		"a key device":   {DeviceID: "01KEYDEVICE", Kind: devices.KindKey, Name: "example-ci", Approved: true},
	} {
		status, reached, view := judgeServe(t, p, cred)
		require.Equal(t, http.StatusOK, status, name)
		require.True(t, reached, name)
		require.False(t, view.Principal().Socket, "%s: the owner over TCP", name)
		require.Empty(t, view.Principal().Relay, name)
		require.True(t, view.Locked(relayvFin), "%s: holding no lease", name)
	}
	for _, approved := range []bool{true, false} {
		status, reached, _ := judgeServe(t, p, &devices.Credential{DeviceID: "01STUDIORELAY", Kind: devices.KindRelay,
			Name: relayvStudio, Approved: approved})
		require.Equal(t, http.StatusForbidden, status, "an unmapped relay (approved %v) is never the owner over TCP", approved)
		require.False(t, reached)
	}

	status, reached, _ := judgeServe(t, p, &devices.Credential{DeviceID: "01LAPTOPRELAY", Kind: devices.KindRelay,
		Name: relayvLaptop, Approved: true})
	require.Equal(t, http.StatusBadRequest, status, "a mapped relay that forwards no caller")
	require.False(t, reached)
	status, reached, _ = judgeServe(t, p, &devices.Credential{DeviceID: "01LAPTOPRELAY", Kind: devices.KindRelay,
		Name: relayvLaptop})
	require.Equal(t, http.StatusForbidden, status, "a mapped relay paired with a code alone")
	require.False(t, reached)
}

// Doctor's peer_auth.relays rows: none unless a relay is mapped; each mapping
// must name a relay paired here and approved with a passkey.
func TestRelayMappingChecks(t *testing.T) {
	ctx := context.Background()
	cfg := config.Defaults()
	require.Empty(t, relayMappingChecks(ctx, cfg, nil), "a default install has no such rows")
	cfg.PeerAuth = config.PeerAuth{Enabled: true, Owner: "owner", Socket: config.PeerSocket{Path: "/opt/seamless/run/s.sock"}}
	require.Empty(t, relayMappingChecks(ctx, cfg, nil), "peer_auth with no relay mapped has none either")

	cfg.PeerAuth.Relays = map[string]config.PeerRelay{relayvLaptop: {OwnerUID: 501}, relayvOld: {OwnerUID: 501}, "example-gone": {OwnerUID: 501}}
	rows := relayMappingChecks(ctx, cfg, nil)
	require.Len(t, rows, 1)
	require.Equal(t, statusWarn, rows[0].status, "no listener another computer reaches")

	cfg.Addr = "0.0.0.0:8081"
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	judgeStoreRelay(t, db, relayvLaptop, "01PASSKEYEXAMPLE")
	judgeStoreRelay(t, db, relayvOld, "")
	byName := map[string]check{}
	for _, r := range relayMappingChecks(ctx, cfg, db) {
		require.Equal(t, "peer_auth relays", r.name)
		for _, n := range []string{relayvLaptop, relayvOld, "example-gone"} {
			if strings.Contains(r.detail, n) {
				byName[n] = r
			}
		}
	}
	require.Equal(t, statusOK, byName[relayvLaptop].status, byName[relayvLaptop].detail)
	require.Equal(t, statusWarn, byName[relayvOld].status)
	require.Contains(t, byName[relayvOld].detail, "pairs again")
	require.Equal(t, statusWarn, byName["example-gone"].status)
	require.Contains(t, byName["example-gone"].detail, "no paired relay holds")
}

// judgeStoreRelay records an active relay named name, approved by approvedBy
// ("" for a pairing the code alone approved).
func judgeStoreRelay(t *testing.T, db *sql.DB, name, approvedBy string) {
	t.Helper()
	ctx := context.Background()
	code, _, err := store.MintPairingCode(ctx, db, store.DeviceByConsole, time.Now())
	require.NoError(t, err)
	d, err := store.PairDevice(ctx, db, code, store.NewDevice{Kind: store.DeviceRelayKind, Name: name,
		Fingerprint: store.ApproverKeyFingerprint([]byte(name)), PublicKey: []byte(name), KeyHash: judgeHash("key:" + name),
		SecretHash: judgeHash("secret:" + name), Pending: approvedBy != ""}, time.Now())
	require.NoError(t, err)
	if approvedBy != "" {
		_, err = store.ApproveDevice(ctx, db, d.ID, d.Fingerprint, approvedBy, time.Now())
		require.NoError(t, err)
	}
}

// peer_auth.relays names a relay by the name the store gives a device: the
// config's rule (a file load) and store.ValidDeviceName agree on every name.
func TestRelayNames_TheConfigAndTheStoreAgree(t *testing.T) {
	for _, name := range []string{
		"example-laptop", "build-01.example.lan", "a", "a_b", "x1", strings.Repeat("a", 253), strings.Repeat("a", 254),
		"Example-Laptop", "example laptop", "-example", "example-", "example..lan", ".example", "example.", "ex/ample",
		"ex:ample", "exämple", "",
	} {
		cfg := config.Defaults()
		cfg.PeerAuth = config.PeerAuth{Enabled: true, Owner: "owner", Socket: config.PeerSocket{Path: "/opt/seamless/run/s.sock"},
			Relays: map[string]config.PeerRelay{name: {OwnerUID: 501}}}
		cfgErr := cfg.Validate()
		storeErr := store.ValidDeviceName(name)
		require.Equal(t, storeErr == nil, cfgErr == nil, "%q: store %v, config %v", name, storeErr, cfgErr)
	}
}

// judgeHash is a stand-in hex SHA-256.
func judgeHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// Doctor names a relay's restricted accounts beside its owner.
func TestRelayMappingChecks_V2NameTheRestrictedAccounts(t *testing.T) {
	ctx := context.Background()
	cfg := config.Defaults()
	cfg.Addr = "0.0.0.0:8081"
	cfg.PeerAuth = config.PeerAuth{Enabled: true, Owner: "owner", Socket: config.PeerSocket{Path: "/opt/seamless/run/s.sock"},
		Relays: map[string]config.PeerRelay{relayvLaptop: {OwnerUID: 501, Restricted: map[string]uint32{"example-ci": 503, "_example_agent": 480}}}}
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	judgeStoreRelay(t, db, relayvLaptop, "01PASSKEYEXAMPLE")
	rows := relayMappingChecks(ctx, cfg, db)
	require.Len(t, rows, 1)
	require.Equal(t, statusOK, rows[0].status)
	require.Equal(t, "relay example-laptop: its requests are judged as this socket's, and uid 501 there is the owner; "+
		"restricted there: _example_agent (uid 480), example-ci (uid 503)", rows[0].detail)
}
