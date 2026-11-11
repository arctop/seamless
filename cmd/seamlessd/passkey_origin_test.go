package main

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/store"
)

// fakeLookup answers from a fixed table and fails every other name.
func fakeLookup(table map[string][]string) originLookup {
	return func(_ context.Context, host string) ([]netip.Addr, error) {
		raw, ok := table[host]
		if !ok {
			return nil, errors.New("no such host")
		}
		out := make([]netip.Addr, len(raw))
		for i, s := range raw {
			out[i] = netip.MustParseAddr(s)
		}
		return out, nil
	}
}

func rowsText(rows []check) string {
	var b strings.Builder
	for _, r := range rows {
		fmt.Fprintf(&b, "[%d] %s\n", r.status, r.detail)
	}
	return b.String()
}

func worst(rows []check) checkStatus {
	w := statusOK
	for _, r := range rows {
		switch r.status {
		case statusFail:
			return statusFail
		case statusWarn:
			w = statusWarn
		}
	}
	return w
}

// The origin a passkey ceremony runs on must be one this daemon serves on every
// address of its host: an address it does not hold is one another local
// process can listen on and serve the same origin from.
func TestPasskeyOriginChecks_EveryAddressOfTheName(t *testing.T) {
	const name = "studio.example.ts.net"
	cert, key := writeCertPair(t, time.Now().Add(24*time.Hour), name)
	other, otherKey := writeCertPair(t, time.Now().Add(24*time.Hour), "elsewhere.example.com")
	tailnet := fakeLookup(map[string][]string{name: {"100.64.0.7", "fd7a:115c:a1e0::7"}})
	elsewhere := fakeLookup(map[string][]string{name: {"100.64.0.7", "fd7a:115c:a1e0::99"}})
	tls := func(c config.Config, certFile, keyFile string) config.Config {
		c.TLS.CertFile, c.TLS.KeyFile = certFile, keyFile
		return c
	}
	base := config.Defaults()
	base.AdvertisedURL = "https://" + name + ":8081"
	ifaces := func(i ...[]netip.Addr) ifaceAddrs { return func() ([][]netip.Addr, error) { return i, nil } }
	dualStack := ifaces(addrs("127.0.0.1", "::1"), addrs("100.64.0.7", "fd7a:115c:a1e0::7"))
	// The tailnet's IPv6 address on an interface of its own: nothing pairs it.
	split := ifaces(addrs("127.0.0.1", "::1"), addrs("100.64.0.7"), addrs("fd7a:115c:a1e0::7"))

	for _, tc := range []struct {
		name string
		cfg  config.Config
		bind string
		hn   hostNet
		want checkStatus
		says []string
	}{
		{"the tailnet IPv4 holds its interface's IPv6 beside it", tls(base, cert, key), "100.64.0.7:8081", hostNet{tailnet, dualStack},
			statusOK, []string{"derived from server_url https://" + name + ":8081", "100.64.0.7, fd7a:115c:a1e0::7, every address of it held"}},
		{"an IPv6 address of the name on another interface is left to anyone", tls(base, cert, key), "100.64.0.7:8081",
			hostNet{tailnet, split}, statusWarn, []string{"does not listen on fd7a:115c:a1e0::7", "another local process can listen there"}},
		{"an address of the name this machine does not hold", tls(base, cert, key), "100.64.0.7:8081", hostNet{elsewhere, dualStack},
			statusWarn, []string{"this machine does not hold fd7a:115c:a1e0::99", "reaches another machine, or nothing"}},
		{"a wildcard bind holds every address", tls(base, cert, key), "0.0.0.0:8081", hostNet{tailnet, split},
			statusOK, []string{"every address of it held by this daemon on port 8081"}},
		{"a certificate for another name", tls(base, other, otherKey), "0.0.0.0:8081", hostNet{tailnet, dualStack},
			statusWarn, []string{"does not name the host " + name, "a browser refuses the origin"}},
		{"a name that does not resolve", tls(base, cert, key), "0.0.0.0:8081", hostNet{fakeLookup(nil), dualStack},
			statusWarn, []string{"does not resolve here"}},
		{"a listener on another port", tls(base, cert, key), "0.0.0.0:9443", hostNet{tailnet, dualStack},
			statusWarn, []string{"names port 8081, but this daemon listens on port 9443"}},
		{"the localhost default on a tailnet bind cannot be reached", config.Defaults(), "100.64.0.7:8081", hostNet{tailnet, dualStack},
			statusWarn, []string{"the localhost default", "does not listen on localhost"}},
		{"the localhost default on 127.0.0.1 holds ::1 beside it", config.Defaults(), "127.0.0.1:8081", hostNet{tailnet, dualStack},
			statusOK, []string{"127.0.0.1, ::1, every address of it held"}},
		{"the localhost default on [::1] leaves 127.0.0.1, said as information", config.Defaults(), "[::1]:8081",
			hostNet{tailnet, dualStack}, statusOK, []string{"does not listen on 127.0.0.1 port 8081"}},
		{"a machine with no ::1 leaves nothing for a process to take", config.Defaults(), "127.0.0.1:8081",
			hostNet{tailnet, ifaces(addrs("127.0.0.1"))}, statusOK, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := passkeyOriginChecks(context.Background(), tc.cfg, tc.bind, nil, tc.hn)
			text := rowsText(rows)
			require.Equal(t, tc.want, worst(rows), text)
			for _, s := range tc.says {
				require.Contains(t, text, s)
			}
		})
	}
}

// A passkey enrolled under another rp_id is named as stranded; one from before
// the column is counted as unknown; one another passkey enrolled without a
// code is named for the owner to check.
func TestPasskeyOriginChecks_EnrolledPasskeys(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	enroll := func(id, rpID string) store.ApproverCredential {
		c := store.ApproverCredential{Kind: store.ApproverPasskey, CredentialID: []byte(id), PublicKey: []byte(id + "-key"),
			Alg: -7, Name: "key " + id, RPID: rpID}
		code, _, err := store.MintEnrollCode(ctx, db, store.ApproverPasskey, store.ApproverKeyFingerprint(c.PublicKey), now)
		require.NoError(t, err)
		got, err := store.EnrollApprover(ctx, db, c, code, now)
		require.NoError(t, err)
		return got
	}
	first := enroll("a", "localhost")
	enroll("b", "studio.example.ts.net")
	enroll("c", "")
	// An earlier build enrolled a passkey on another one's approval: its row
	// names the approving credential in added_by.
	_, err = db.ExecContext(ctx, `INSERT INTO approver_credentials (id, kind, credential_id, public_key, alg, name, added_at, added_by, rp_id)
		VALUES ('legacy', 'passkey', X'64', X'642d6b6579', -7, 'key d', ?, ?, 'localhost')`, "2026-11-09T12:00:00Z", first.ID)
	require.NoError(t, err)

	text := rowsText(enrolledPasskeyChecks(ctx, db, "localhost"))
	require.Contains(t, text, `"key b"`)
	require.Contains(t, text, "enrolled for studio.example.ts.net) cannot approve under rp_id localhost")
	require.Contains(t, text, "1 passkey(s) enrolled before Seamless recorded")
	require.Contains(t, text, `passkeys "key d"`)
	require.Contains(t, text, "with no enrollment code")
	require.NotContains(t, text, `"key a"`)

	text = rowsText(enrolledPasskeyChecks(ctx, db, "studio.example.ts.net"))
	require.Contains(t, text, `"key a"`)
	require.NotContains(t, text, `"key b" (`)
}

// boundAddrs reads what serve listens on: a wildcard holds everything, an IP
// itself, a name its first IPv4 address, and an IPv4 address the IPv6 ones
// serve pairs with it.
func TestBoundAddrs(t *testing.T) {
	hn := hostNet{
		lookup: fakeLookup(map[string][]string{"studio.local": {"fe80::1", "192.168.1.9"}}),
		ifaces: func() ([][]netip.Addr, error) { return studioIfaces, nil },
	}
	for bind, want := range map[string]heldAddrs{
		":8081":             {every: true},
		"0.0.0.0:8081":      {every: true},
		"[::]:8081":         {every: true},
		"100.64.0.7:8081":   {addrs: addrs("100.64.0.7", "fd7a:115c:a1e0::7")},
		"10.9.9.9:8081":     {addrs: addrs("10.9.9.9")},
		"[::1]:8081":        {addrs: addrs("::1")},
		"studio.local:8081": {addrs: addrs("192.168.1.9", "fd00:1::9")},
	} {
		got, err := boundAddrs(context.Background(), bind, hn)
		require.NoError(t, err, bind)
		require.Equal(t, want, got, bind)
	}
	_, err := boundAddrs(context.Background(), "nowhere.local:8081", hn)
	require.Error(t, err)
	require.True(t, heldAddrs{addrs: addrs("100.64.0.7")}.covers(netip.MustParseAddr("::ffff:100.64.0.7")),
		"a v4-mapped answer is the same address")
}
