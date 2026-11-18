//go:build darwin || linux

package main

// The relay's doctor, when the server presents another certificate than the
// pinned one (owner decision R-D2): the refusal's own words, with the pinned
// and the offered fingerprints, and the command that pairs again with the
// fingerprint the server shows, never the one offered.

import (
	"context"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/relay"
	"github.com/arctop/seamless/internal/store"
)

func TestPRRelayDoctor_ACertificateOtherThanThePinnedOne(t *testing.T) {
	d := deviceNewDaemon(t, false)
	l, env := relayCmdLayout(t)
	code, err := d.pairing.MintCode(context.Background(), store.DeviceByConsole)
	require.NoError(t, err)
	require.NoError(t, relayPair(context.Background(), relayPairOptions{dataDir: l.DataDir, env: env,
		pair: relay.PairOptions{OwnerUID: relayCmdOwner, Server: d.srv.URL, Code: code.Code, Name: "example-laptop",
			CA: relayCmdServerPEM(t, d.srv.URL, d.pool)}}, io.Discard, io.Discard))
	st, err := relay.LoadState(l.DataDir)
	require.NoError(t, err)
	require.NotNil(t, st.ServerNotAfter, "the pairing recorded the pinned certificate's notAfter")
	served := st.ServerFingerprint
	e := relayDoctorEnv{layout: l, env: env, account: func(uint32) string { return "_example_relay" }, timeout: 10 * time.Second}
	soon := relayCertificateRow(st, l.DataDir, time.Now())
	require.Equal(t, statusWarn, soon.status, "the test's certificate lasts an hour: well inside the 14 days")
	require.Contains(t, soon.detail, "in 0 day(s)")

	// As if the server had renewed: the relay pinned another certificate.
	st.ServerFingerprint = strings.TrimSuffix(strings.Repeat("AB:", 32), ":")
	row := relayServerRow(context.Background(), e, st)
	require.Equal(t, statusFail, row.status)
	require.Contains(t, row.detail, "pinned SHA-256 "+st.ServerFingerprint)
	require.Contains(t, row.detail, "offered SHA-256 "+served+", valid ")
	require.Contains(t, row.detail, "A renewed server certificate means pairing every relay again")
	require.NotContains(t, row.detail, "--fingerprint "+served, "never the offered fingerprint as the one to pin")
	require.True(t, strings.HasSuffix(row.detail, st.PairAgainCommand(l.DataDir)), row.detail)
	require.Contains(t, row.detail, "--fingerprint "+relay.PairAgainFingerprint)
	require.Contains(t, row.detail, "--owner-uid "+strconv.FormatUint(uint64(relayCmdOwner), 10)+" ", "the uid the pairing named")
}
