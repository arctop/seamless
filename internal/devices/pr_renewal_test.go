package devices

// Owner decision R-D2: the warning ahead of the served certificate's renewal.

import (
	"context"
	"crypto/tls"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/store"
)

func TestPRRenewalDue_TheWindow(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	list := []store.Device{
		{Kind: KindRelay, Name: "example-laptop", Status: store.DeviceActive},
		{Kind: KindRelay, Name: "example-desk", Status: store.DeviceActive},
		{Kind: KindRelay, Name: "example-old", Status: store.DeviceRevoked},
		{Kind: KindRelay, Name: "example-waiting", Status: store.DevicePending, PendingUntil: now.Add(time.Minute)},
		{Kind: KindKey, Name: "example-ci", Status: store.DeviceActive},
	}
	for name, tc := range map[string]struct {
		left    time.Duration
		due     bool
		days    int
		expired bool
	}{
		"a month out":             {left: 30 * 24 * time.Hour},
		"a minute before 14 days": {left: RelayRenewalWarning + time.Minute},
		"14 days exactly":         {left: RelayRenewalWarning, due: true, days: 14},
		"three and a half days":   {left: 84 * time.Hour, due: true, days: 3},
		"the last hours":          {left: time.Hour, due: true, days: 0},
		"expired":                 {left: -time.Hour, due: true, expired: true},
	} {
		r := RenewalDue(now.Add(tc.left), list, now)
		if !tc.due {
			require.Nil(t, r, name)
			continue
		}
		require.NotNil(t, r, name)
		require.Equal(t, tc.days, r.Days, name)
		require.Equal(t, tc.expired, r.Expired, name)
		require.Equal(t, []string{"example-laptop", "example-desk"}, r.Relays, "%s: the active relays, nothing else", name)
	}
	require.Nil(t, RenewalDue(time.Time{}, list, now), "no certificate, no renewal")
	require.Nil(t, RenewalDue(now.Add(time.Hour), list[2:], now), "no relay paired, nothing to pair again")
}

// A pin that does not match fails the handshake with ErrServerFingerprint,
// carrying what the server offered: its fingerprint and dates, for the
// refusal's words. Its text stays the sentinel's.
func TestPROfferedCertificate_TheMismatchCarriesWhatWasOffered(t *testing.T) {
	f := pairNewFixture(t, false)
	ts, pool, certFP := f.deviceServer(t, nil, nil)
	leaf := ts.TLS.Certificates[0].Leaf
	id := pairIdentity(t, "example-laptop")
	base := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	pin := strings.TrimSuffix(strings.Repeat("AB:", 32), ":")
	wrong, err := RelayTransport(base, id, pin)
	require.NoError(t, err)
	_, err = (&http.Client{Transport: wrong}).Get(ts.URL + "/api/mcp")
	require.ErrorIs(t, err, ErrServerFingerprint, "every reader of the sentinel still sees it")
	offered, ok := OfferedCertificate(err)
	require.True(t, ok, "%v", err)
	require.Equal(t, certFP, offered.Offered)
	require.True(t, leaf.NotBefore.Equal(offered.NotBefore))
	require.True(t, leaf.NotAfter.Equal(offered.NotAfter))
	require.Equal(t, ErrServerFingerprint.Error(), offered.Error())

	// A pairing that compared another fingerprint: the same, and nothing sent.
	_, err = Pair(context.Background(), deviceClient(pool, nil), ts.URL, PairRequest{Code: f.pairCode(t), Name: "example-ci",
		ServerFingerprint: pin})
	require.ErrorIs(t, err, ErrServerFingerprint)
	offered, ok = OfferedCertificate(err)
	require.True(t, ok)
	require.Equal(t, certFP, offered.Offered)
	_, ok = OfferedCertificate(ErrServerFingerprint)
	require.False(t, ok, "the bare sentinel offers nothing")
}

// A finished pairing reports the presented certificate's notAfter beside its
// fingerprint: a relay records both.
func TestPRPair_ReportsTheServerCertificatesNotAfter(t *testing.T) {
	f := pairNewFixture(t, false)
	ts, pool, certFP := f.deviceServer(t, nil, nil)
	paired, err := Pair(context.Background(), deviceClient(pool, nil), ts.URL, PairRequest{Code: f.pairCode(t), Name: "example-laptop",
		Relay: pairIdentity(t, "example-laptop"), OwnerUID: 501})
	require.NoError(t, err)
	require.Equal(t, certFP, paired.ServerFingerprint)
	require.True(t, ts.TLS.Certificates[0].Leaf.NotAfter.Equal(paired.ServerNotAfter))
}
