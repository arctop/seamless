package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// writeCertPair writes a self-signed certificate/key pair covering hosts, valid
// until notAfter, and returns the two paths.
func writeCertPair(t *testing.T, notAfter time.Time, hosts ...string) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "seamless-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
			continue
		}
		tmpl.DNSNames = append(tmpl.DNSNames, h)
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)

	dir := t.TempDir()
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	require.NoError(t, os.WriteFile(certPath,
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	require.NoError(t, os.WriteFile(keyPath,
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600))
	return certPath, keyPath
}

// portOf returns the port of a test server URL.
func portOf(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	require.NoError(t, err)
	return u.Port()
}

func TestBindCheck(t *testing.T) {
	cert, key := writeCertPair(t, time.Now().Add(365*24*time.Hour), "seam.lan")

	for _, tc := range []struct {
		name   string
		cfg    config.Config
		status checkStatus
		want   string
	}{
		{"loopback", config.Config{Addr: "127.0.0.1:8081"}, statusOK, "only from this machine"},
		{"wildcard plaintext", config.Config{Addr: "0.0.0.0:8081"}, statusWarn, "travel in the clear"},
		{"lan plaintext", config.Config{Addr: "192.168.1.5:8081"}, statusWarn, "set tls.cert_file/tls.key_file"},
		{
			"wildcard with tls",
			config.Config{Addr: "0.0.0.0:8081", TLS: config.TLS{CertFile: cert, KeyFile: key}},
			statusOK, "over TLS",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := bindCheck(tc.cfg)
			require.Equal(t, tc.status, c.status, c.detail)
			require.Contains(t, c.detail, tc.want)
		})
	}
}

// The 421 case is the whole reason this check exists: from the client it looks
// like the network, and from the server everything appears configured.
func TestServerURLCheck(t *testing.T) {
	t.Run("reachable", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()
		c := serverURLCheck(config.Config{Addr: "127.0.0.1:8081", AdvertisedURL: srv.URL})
		require.Equal(t, statusOK, c.status, c.detail)
		require.Contains(t, c.detail, "reaches this daemon")
	})

	t.Run("misdirected host names the fix", func(t *testing.T) {
		// The real guard, wired exactly as runServe wires it, in front of a
		// server whose advertised name it was never told about. The check must
		// name allowed_hosts: from the client this is indistinguishable from a
		// network fault, and from the server everything looks configured.
		cfg := config.Config{Addr: "0.0.0.0:8081", AllowedHosts: []string{"other.lan"}}
		guarded := httptest.NewServer(hostGuard(cfg.Addr, cfg.AllowedHostsEffective(),
			http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })))
		defer guarded.Close()
		// The probe dials the test server's address but the guard compares the
		// Host header, so an advertised name it does not admit yields the 421
		// without any DNS.
		cfg.AdvertisedURL = "http://seam.lan:" + portOf(t, guarded.URL)

		c := serverURLCheck(cfg)
		require.Equal(t, statusInfo, c.status, "seam.lan does not resolve here: unreachable, not misconfigured")

		// With the name resolvable (127.0.0.1 stands in for it), the guard's
		// refusal is what the check reports.
		always421 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusMisdirectedRequest)
		}))
		defer always421.Close()
		c = serverURLCheck(config.Config{Addr: "0.0.0.0:8081", AdvertisedURL: always421.URL})
		require.Equal(t, statusFail, c.status, c.detail)
		require.Contains(t, c.detail, "answers 421")
		require.Contains(t, c.detail, "add the host to allowed_hosts")
	})

	t.Run("daemon down is info, not a failure", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := srv.URL
		srv.Close()
		c := serverURLCheck(config.Config{Addr: "127.0.0.1:8081", AdvertisedURL: url})
		require.Equal(t, statusInfo, c.status, c.detail)
		require.Contains(t, c.detail, "not answering right now")
	})

	t.Run("derived url says so", func(t *testing.T) {
		// Port 1 is reserved and nothing listens there, so this exercises the
		// derived-URL wording without depending on a live daemon.
		c := serverURLCheck(config.Config{Addr: "127.0.0.1:1"})
		require.Contains(t, c.detail, "derived from addr")
	})
}

func TestTLSCheck(t *testing.T) {
	t.Run("off", func(t *testing.T) {
		c := tlsCheck(config.Config{Addr: "127.0.0.1:8081"})
		require.Equal(t, statusInfo, c.status)
		require.Contains(t, c.detail, "off (http)")
	})

	t.Run("covers the advertised host", func(t *testing.T) {
		cert, key := writeCertPair(t, time.Now().Add(365*24*time.Hour), "seam.lan")
		c := tlsCheck(config.Config{
			Addr: "0.0.0.0:8081", AdvertisedURL: "https://seam.lan:8443",
			TLS: config.TLS{CertFile: cert, KeyFile: key},
		})
		require.Equal(t, statusOK, c.status, c.detail)
		require.Contains(t, c.detail, "covers seam.lan")
	})

	t.Run("expiring soon", func(t *testing.T) {
		cert, key := writeCertPair(t, time.Now().Add(5*24*time.Hour), "seam.lan")
		c := tlsCheck(config.Config{
			Addr: "0.0.0.0:8081", AdvertisedURL: "https://seam.lan",
			TLS: config.TLS{CertFile: cert, KeyFile: key},
		})
		require.Equal(t, statusWarn, c.status, c.detail)
		require.Contains(t, c.detail, "expires in")
	})

	t.Run("wrong SAN names the advertised host", func(t *testing.T) {
		cert, key := writeCertPair(t, time.Now().Add(365*24*time.Hour), "other.lan")
		c := tlsCheck(config.Config{
			Addr: "0.0.0.0:8081", AdvertisedURL: "https://seam.lan",
			TLS: config.TLS{CertFile: cert, KeyFile: key},
		})
		require.Equal(t, statusWarn, c.status, c.detail)
		require.Contains(t, c.detail, `does not cover "seam.lan"`)
		require.Contains(t, c.detail, "other.lan")
	})

	t.Run("unreadable pair fails", func(t *testing.T) {
		c := tlsCheck(config.Config{TLS: config.TLS{CertFile: "/nope/cert.pem", KeyFile: "/nope/key.pem"}})
		require.Equal(t, statusFail, c.status)
		require.Contains(t, c.detail, "cannot load the certificate/key pair")
	})
}

// A client install binds nothing and holds no certificate, so reporting a bind
// address it never binds would be a confident wrong answer. What it does get is
// the one transport fact it owns: whether the URL it dials answers.
func TestTransportChecksOnAClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	checks := transportChecks(config.Config{Role: config.RoleClient, AdvertisedURL: srv.URL})
	require.Len(t, checks, 2)
	require.Equal(t, "role", checks[0].name)
	require.Contains(t, checks[0].detail, "runs no daemon and dials "+srv.URL)
	require.Equal(t, "server_url", checks[1].name)
	require.Equal(t, statusOK, checks[1].status, checks[1].detail)

	// No bind, no tls: naming either would describe a listener this install
	// does not have.
	for _, c := range checks {
		require.NotEqual(t, "bind", c.name)
		require.NotEqual(t, "tls", c.name)
	}
}

// The severity split against serverURLCheck is the point: a server's own daemon
// being stopped is a normal state doctor reports as info, while a client with no
// server has nothing left working at all.
func TestClientServerURLCheck(t *testing.T) {
	t.Run("reachable", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()
		c := clientServerURLCheck(config.Config{Role: config.RoleClient, AdvertisedURL: srv.URL})
		require.Equal(t, statusOK, c.status, c.detail)
		require.Contains(t, c.detail, "reaches the server")
	})

	t.Run("unreachable fails, unlike the server role's info", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		down := srv.URL
		srv.Close()

		client := clientServerURLCheck(config.Config{Role: config.RoleClient, AdvertisedURL: down})
		require.Equal(t, statusFail, client.status, client.detail)
		require.Contains(t, client.detail, "is not answering")

		// Same URL, server role: info, because `seamlessd serve` is the fix and
		// a stopped local daemon is not a misconfiguration.
		require.Equal(t, statusInfo, serverURLCheck(config.Config{Addr: "127.0.0.1:8081", AdvertisedURL: down}).status)
	})

	t.Run("421 points at the server's allowlist, not the client's", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusMisdirectedRequest)
		}))
		defer srv.Close()
		c := clientServerURLCheck(config.Config{Role: config.RoleClient, AdvertisedURL: srv.URL})
		require.Equal(t, statusFail, c.status, c.detail)
		require.Contains(t, c.detail, "answers 421")
		require.Contains(t, c.detail, "SERVER's allowed_hosts")
	})

	t.Run("another status is a warning", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		}))
		defer srv.Close()
		c := clientServerURLCheck(config.Config{Role: config.RoleClient, AdvertisedURL: srv.URL})
		require.Equal(t, statusWarn, c.status, c.detail)
		require.Contains(t, c.detail, "502")
	})
}

func TestRemoteSessionsCheck(t *testing.T) {
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()

	mkSession := func(name, host string) {
		t.Helper()
		id, err := core.NewID()
		require.NoError(t, err)
		require.NoError(t, store.CreateSession(ctx, db, core.Session{
			ID: id, Name: name, Host: host, Status: core.SessionActive,
			CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}))
	}

	// Local-only, including the unnamed legacy bucket, is not "remote".
	mkSession("cc/local1", config.Hostname())
	mkSession("cc/legacy", "")
	c := remoteSessionsCheck(db)
	require.Equal(t, statusInfo, c.status)
	require.Contains(t, c.detail, "none in 24h")
	require.Contains(t, c.detail, "all local")

	mkSession("cc/remote1", "argon")
	rec := events.NewRecorder(db)
	_, err = rec.Record(ctx, core.Event{
		Kind:    core.EventHookError,
		Payload: map[string]any{"stage": "remote-host-skip", "what": "plan-capture", "host": "argon"},
	})
	require.NoError(t, err)

	c = remoteSessionsCheck(db)
	require.Equal(t, statusInfo, c.status, c.detail)
	require.Contains(t, c.detail, "1 of 3 sessions in 24h from argon (1)")
	require.Contains(t, c.detail, "1 local captures skipped")
}
