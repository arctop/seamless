package config

// The one HTTP client both binaries use to reach a Seamless daemon.
//
// It lives in config, rather than in either command, because both need it and
// neither can import the other (both are package main), and because config is
// already where its inputs are: ServerURL, ServerHost and TLS.CAFile. Putting it
// anywhere else is what produced the second and third copies this file replaced
// -- each written to dodge an import edge, and each a place where the trust
// decision could quietly differ from the others.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// DialTimeout bounds how long a client waits to ESTABLISH a connection to the
// daemon. It is separate from the whole-request deadline because a down daemon
// must fail fast even on the surfaces where the response may legitimately take
// minutes (the mcp-proxy bridge, whose tool calls can be LLM-backed).
const DialTimeout = 5 * time.Second

// HTTPClient is the one HTTP client constructor for talking to a Seamless
// daemon. Every surface that carries the bearer key goes through it -- the seam
// CLI's hook, mcp-proxy, doctor, status, version, console JSON and MCP dial, and
// seamlessd's own client-role surfaces (install-hooks reading the server's
// feature state, the client-role doctor's live tools/list count) -- because the
// TLS trust decision must be identical on all of them: an https server_url with
// a private CA either verifies everywhere or the install half-works in a way
// that reads as an intermittent network fault.
//
// That sharing is the point, and it is load-bearing beyond tidiness: the
// alternative a caller reaches for when this constructor is out of import range
// is its own client, and the shortest such client is one that skips
// verification. A bearer key must never travel over a connection whose
// certificate was not verified, so the constructor has to be reachable from
// every binary that holds the key.
//
// timeout is the whole-request deadline; 0 means none (the mcp-proxy bridge,
// whose tool calls can be LLM-slow, and seam hook, whose one deadline rides on
// its ctx so a dial retry cannot restart it). DialTimeout applies either way.
//
// A configured-but-unusable tls.ca_file is an ERROR, never a silent fall back to
// the system pool: the request would then fail at the TLS handshake and look
// exactly like an outage, which is the local-vs-remote distinction AGENTS.md
// requires be kept (llm-degradation-remote-vs-local, same rule). The error names
// the config key, because the repair is in this machine's file and nowhere else.
func (c Config) HTTPClient(timeout time.Duration) (*http.Client, error) {
	if sock := c.SocketPath(); sock != "" {
		// server_url names the daemon's peer_auth socket: every request goes to
		// it whatever its URL says, and the kernel -- not a bearer key --
		// authenticates the caller. No proxy ever applies (an HTTP proxy would
		// move the dial off the socket and with it the peer identity), and there
		// is no TLS to trust: the socket never leaves this machine.
		dialer := &net.Dialer{Timeout: DialTimeout}
		tr := &http.Transport{
			Proxy: nil,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, "unix", sock)
			},
		}
		return &http.Client{Timeout: timeout, Transport: tr}, nil
	}
	tr := &http.Transport{
		Proxy:       http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{Timeout: DialTimeout}).DialContext,
	}
	if ca := strings.TrimSpace(c.TLS.CAFile); ca != "" {
		pem, err := os.ReadFile(ca)
		if err != nil {
			return nil, fmt.Errorf("tls.ca_file %s: %w", ca, err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			// Windows has historically returned an error here; an empty pool
			// plus the configured root is still a working trust store for the
			// one server this install talks to.
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("tls.ca_file %s: no PEM certificate found", ca)
		}
		// Trust is ADDED, never switched off: RootCAs gains the configured root
		// and InsecureSkipVerify stays false, so a certificate this pool cannot
		// chain is still refused.
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &http.Client{Timeout: timeout, Transport: tr}, nil
}
