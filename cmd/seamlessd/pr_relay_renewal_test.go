package main

// Owner decision R-D2 (2026-11-15): a relay keeps pinning the server
// certificate's exact SHA-256, so a renewal means pairing every relay again.
// The server's doctor says so ahead of time, naming the relays; the relay's
// doctor shows the pinned certificate's notAfter.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/devices"
	"github.com/arctop/seamless/internal/relay"
)

// prRenewalCfg serves TLS with a certificate valid until notAfter.
func prRenewalCfg(t *testing.T, notAfter time.Time) config.Config {
	t.Helper()
	cert, key := writeCertPair(t, notAfter, "example-server.example.ts.net")
	cfg := config.Defaults()
	cfg.AdvertisedURL = "https://example-server.example.ts.net:8081"
	cfg.TLS.CertFile, cfg.TLS.KeyFile = cert, key
	return cfg
}

func TestPRRelayRenewalDoctor(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	d := deviceNewDaemon(t, false)
	_, _ = d.devicePairRelay(t, "example-laptop")
	_, _ = d.prPairUnboundRelay(t, "example-desk")
	_, _ = d.devicePairKey(t, "example-ci")

	for name, tc := range map[string]struct {
		left time.Duration
		want string
	}{
		"ten days out": {left: 10*24*time.Hour + time.Hour, want: "expires on "},
		"expired":      {left: -24 * time.Hour, want: "expired on "},
	} {
		cfg := prRenewalCfg(t, now.Add(tc.left))
		fp, notAfter, err := certFactsFile(cfg.TLS.CertFile)
		require.NoError(t, err)
		rows := relayRenewalChecks(ctx, cfg, d.db, now)
		require.Len(t, rows, 1, name)
		require.Equal(t, statusWarn, rows[0].status, name)
		require.Equal(t, "relay renewal", rows[0].name)
		detail := rows[0].detail
		require.Contains(t, detail, tc.want+notAfter.UTC().Format(time.DateOnly), name)
		require.Contains(t, detail, "every relay paired here must pair again (example-desk, example-laptop)", name)
		require.NotContains(t, detail, "example-ci", "%s: a key device pins nothing", name)
		require.Contains(t, detail, "/usr/bin/sudo -u _seamless -H /opt/seamless/bin/seamlessd relay pair --server "+
			"https://example-server.example.ts.net:8081 --fingerprint "+relay.PairAgainFingerprint+" --owner-uid <your uid there> "+
			"--replace --code -", name)
		require.NotContains(t, detail, "--fingerprint "+fp, "%s: never the certificate that is about to go", name)
	}
	rows := relayRenewalChecks(ctx, prRenewalCfg(t, now.Add(10*24*time.Hour-time.Minute)), d.db, now)
	require.Len(t, rows, 1)
	require.Contains(t, rows[0].detail, "in 9 day(s)", "whole days left")

	require.Nil(t, relayRenewalChecks(ctx, prRenewalCfg(t, now.Add(devices.RelayRenewalWarning+time.Hour)), d.db, now),
		"before the window the tls row alone says the certificate is due")
	require.Nil(t, relayRenewalChecks(ctx, prRenewalCfg(t, now.Add(time.Hour)), deviceNewDaemon(t, false).db, now),
		"no relay paired, nothing to pair again")
	require.Nil(t, relayRenewalChecks(ctx, prRenewalCfg(t, now.Add(time.Hour)), nil, now))
	plain := config.Defaults()
	plain.Addr = "0.0.0.0:8081"
	require.Nil(t, relayRenewalChecks(ctx, plain, d.db, now), "no TLS, no certificate to renew")

	// The two windows: the tls row warns from 30 days, this one from 14.
	require.Less(t, devices.RelayRenewalWarning, tlsExpiryWarning)
}

// The served certificate's notAfter is read with its fingerprint, as serve
// starts.
func TestPRPairingFacts_CarryTheCertificatesNotAfter(t *testing.T) {
	notAfter := time.Now().Add(40 * 24 * time.Hour).UTC().Truncate(time.Second)
	cfg := prRenewalCfg(t, notAfter)
	facts, err := pairingFacts(cfg)
	require.NoError(t, err)
	require.True(t, facts.CertNotAfter.Equal(notAfter), "%v", facts.CertNotAfter)
	require.NotEmpty(t, facts.CertFingerprint)
	raw, err := json.Marshal(facts)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "not_after", "not a fact a client pairs by")
}

// The relay's doctor shows the pinned certificate's notAfter, warns from 14
// days before it with the command that pairs again, and says a pairing from an
// earlier release recorded none, without failing.
func TestPRRelayCertificateRow(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	st := relay.State{ServerURL: "https://example-server.example.ts.net:8081", OwnerUID: 501}
	at := func(d time.Duration) *time.Time {
		t := now.Add(d)
		return &t
	}
	row := relayCertificateRow(st, relay.DefaultDataDir, now)
	require.Equal(t, statusInfo, row.status)
	require.Equal(t, "relay.certificate", row.name)
	require.Contains(t, row.detail, "the pinned certificate's expiry is unknown")
	require.Contains(t, row.detail, "re-pair to record it")
	require.Contains(t, row.detail, st.PairAgainCommand(relay.DefaultDataDir))

	st.ServerNotAfter = at(60 * 24 * time.Hour)
	row = relayCertificateRow(st, relay.DefaultDataDir, now)
	require.Equal(t, statusInfo, row.status)
	require.Equal(t, "the pinned certificate is valid until 2027-01-16; a renewal means pairing this relay again", row.detail)

	st.ServerNotAfter = at(5*24*time.Hour + time.Hour)
	row = relayCertificateRow(st, relay.DefaultDataDir, now)
	require.Equal(t, statusWarn, row.status)
	require.Contains(t, row.detail, "valid until 2026-11-22, in 5 day(s)")
	require.True(t, strings.HasSuffix(row.detail, st.PairAgainCommand(relay.DefaultDataDir)))

	st.ServerNotAfter = at(-time.Hour)
	row = relayCertificateRow(st, relay.DefaultDataDir, now)
	require.Equal(t, statusWarn, row.status)
	require.Contains(t, row.detail, "the pinned certificate expired on 2026-11-17")
}

// The command a refusal prints to pair a relay again is the root step's
// pairing line, from the same pieces: the root step's sudo, the relay's
// account, root's copy of seamlessd, the code pasted.
func TestPRPairAgainCommand_IsTheRootStepsLine(t *testing.T) {
	p := clientRelayPlan{goos: "linux", owner: "example-owner", ownerUID: 501, server: "https://box.example:8081", fingerprint: clientFP,
		source: "/h/.local/bin/seamlessd", sha256: strings.Repeat("a", 64)}
	require.Empty(t, p.problem())
	var line string
	for _, b := range clientRelayRootStep(p) {
		for _, c := range b[1:] {
			if strings.Contains(c, " relay pair ") {
				line = c
			}
		}
	}
	require.NotEmpty(t, line)
	again := relay.State{ServerURL: "https://box.example:8081", OwnerUID: 501}.PairAgainCommand(relay.DefaultDataDir)
	prefix := rootstepToolsFor("linux").sudo + " -u " + clientRelayAccount + " -H " + relay.DefaultBinary + " relay pair --server https://box.example:8081 --fingerprint "
	require.True(t, strings.HasPrefix(line, prefix), line)
	require.True(t, strings.HasPrefix(again, prefix), again)
	require.True(t, strings.HasSuffix(line, "--owner-uid 501 --code -"), line)
	require.True(t, strings.HasSuffix(again, "--owner-uid 501 --replace --code -"), again)
}
