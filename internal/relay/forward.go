package relay

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/devices"
)

// The headers of the wire contract between a relay and its server, which
// reads them only on a request its edge matched to an active relay's
// certificate. The relay sets all three, once each, on every request it
// forwards, after removing every header under devices.RelayAttributePrefix a
// local client sent.
const (
	// HeaderVersion is the contract's version, WireVersion.
	HeaderVersion = devices.RelayVersionHeader
	// HeaderUID is the local caller's uid, decimal.
	HeaderUID = devices.RelayUIDHeader
	// HeaderChain is the local caller's process chain: access.ProcessToken
	// keys ("<boot>:<pid>:<version>"), the peer first, joined by "," with no
	// spaces, 1 to access.MaxChainHops of them.
	HeaderChain = devices.RelayChainHeader
	// WireVersion is the contract version this relay speaks: the one the
	// server's edge reads (devices.ParseRelayAttributes).
	WireVersion = devices.RelayWireVersion
)

// idleConnsPerServer is how many idle connections to the server the relay
// keeps: each agent's bridge holds a stream open beside its calls.
const idleConnsPerServer = 16

// tlsHandshakeTimeout bounds a handshake with the server (net/http's
// DefaultTransport's bound): config.Config.HTTPClient bounds the dial alone.
const tlsHandshakeTimeout = 10 * time.Second

// Forwarder is the relay's one handler: every local request goes to the
// server it paired with, carrying the caller's uid and process chain, and the
// answer streams back as it arrives. It is safe for concurrent use.
type Forwarder struct {
	server *url.URL
	pin    string
	proxy  *httputil.ReverseProxy
	logger *slog.Logger
}

type attrKey struct{}

// NewForwarder forwards to server (https://host:port) over rt, which must
// present the relay's certificate and pin the server's (Transport builds it).
// pin is the pinned fingerprint, for the refusal's words. A ReverseProxy
// follows no redirect: a 3xx goes back to the local client as the server sent
// it.
func NewForwarder(server *url.URL, pin string, rt http.RoundTripper, logger *slog.Logger) *Forwarder {
	if logger == nil {
		logger = slog.Default()
	}
	f := &Forwarder{server: server, pin: pin, logger: logger}
	f.proxy = &httputil.ReverseProxy{
		Rewrite:   f.rewrite,
		Transport: rt,
		// Every write goes out at once: an SSE event, an elicitation, a
		// streamed tool result. The relay holds nothing back.
		FlushInterval: -1,
		ErrorHandler:  f.upstreamError,
		ErrorLog:      slog.NewLogLogger(logger.Handler(), slog.LevelDebug),
	}
	return f
}

// Forwarder builds the relay's forwarder from its pairing in dataDir.
func (s State) Forwarder(dataDir string, logger *slog.Logger) (*Forwarder, error) {
	rt, err := s.Transport(dataDir)
	if err != nil {
		return nil, err
	}
	u, err := ServerURLOf(s.ServerURL)
	if err != nil {
		return nil, fmt.Errorf("relay: %w", err)
	}
	return NewForwarder(u, s.ServerFingerprint, rt, logger), nil
}

// Transport is the relay's transport to the server it paired with: the one
// trust decision every surface makes (config.Config.HTTPClient: the system's
// roots, plus the CA copied at pairing), the relay's certificate presented,
// and the server's certificate pinned (devices.RelayTransport). No proxy
// environment can move it off TLS: whatever carries the connection, the
// handshake is the server's.
func (s State) Transport(dataDir string) (*http.Transport, error) {
	cfg := config.Defaults()
	cfg.AdvertisedURL = s.ServerURL
	cfg.TLS.CAFile = s.CAPath(dataDir)
	client, err := cfg.HTTPClient(0)
	if err != nil {
		return nil, fmt.Errorf("relay: the server's trust: %w", err)
	}
	id, err := s.Identity()
	if err != nil {
		return nil, err
	}
	tr, err := devices.RelayTransport(client, id, s.ServerFingerprint)
	if err != nil {
		return nil, fmt.Errorf("relay: %w", err)
	}
	tr.MaxIdleConnsPerHost = idleConnsPerServer
	tr.IdleConnTimeout = 90 * time.Second
	// A server that takes the connection and stalls the handshake fails the
	// call, as one that refuses it does; the answer itself may take as long
	// as the server takes (an LLM-backed tool, an elicitation's wait).
	tr.TLSHandshakeTimeout = tlsHandshakeTimeout
	return tr, nil
}

// ServeHTTP reads the local caller from the connection (ConnContext) and
// forwards the request, or refuses it when the kernel named no caller.
func (f *Forwarder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a, err := attributesOf(r.Context())
	if err != nil {
		f.logger.Warn("relay: refused a request whose caller the kernel did not name", "error", err)
		writeRefusal(w, http.StatusForbidden, err.Error())
		return
	}
	f.logger.Debug("relay: forwarding", "method", r.Method, "path", r.URL.Path, "uid", a.UID, "hops", len(a.Chain))
	f.proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), attrKey{}, a)))
}

// rewrite is the outgoing request: the server's address, the caller's
// attributes, and nothing a local client said about either. ReverseProxy has
// already removed the hop-by-hop headers and every X-Forwarded-* header.
func (f *Forwarder) rewrite(pr *httputil.ProxyRequest) {
	pr.SetURL(f.server)
	for _, h := range []http.Header{pr.Out.Header, pr.Out.Trailer} {
		if h == nil {
			continue
		}
		devices.StripRelayAttributes(h)
		h.Del("Authorization")
	}
	a, _ := pr.In.Context().Value(attrKey{}).(Attributes)
	pr.Out.Header.Set(HeaderVersion, WireVersion)
	pr.Out.Header.Set(HeaderUID, strconv.FormatUint(uint64(a.UID), 10))
	pr.Out.Header.Set(HeaderChain, strings.Join(a.Chain, ","))
}

// upstreamError answers a request the server did not: unreachable, or a TLS
// handshake that failed (another certificate than the pinned one, one that
// does not verify, or the server refusing the relay's). Nothing is ever
// answered from memory: the caller gets 502 and the one-line reason.
func (f *Forwarder) upstreamError(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		// The local caller went away; nobody reads an answer.
		f.logger.Debug("relay: the caller ended the request", "path", r.URL.Path, "error", err)
		return
	}
	msg := f.reason(err)
	f.logger.Warn("relay: the server did not answer", "server", f.server.Redacted(), "path", r.URL.Path, "error", err)
	writeRefusal(w, http.StatusBadGateway, msg)
}

// reason words an upstream failure for the local caller.
func (f *Forwarder) reason(err error) string {
	server := f.server.Redacted()
	var verify *tls.CertificateVerificationError
	switch {
	case errors.Is(err, devices.ErrServerFingerprint):
		return fmt.Sprintf("the server at %s presented a certificate other than the one this relay pinned at pairing (SHA-256 %s), "+
			"so nothing was sent: if the server's certificate was renewed, pair the relay again (seamlessd relay pair --replace)", server, f.pin)
	case errors.As(err, &verify):
		return fmt.Sprintf("the server at %s presented a certificate that does not verify (%v), so nothing was sent", server, verify.Err)
	}
	return fmt.Sprintf("the server at %s could not be reached (%v); nothing is answered from memory, so every call fails until it answers", server, err)
}

// writeRefusal answers with the relay's own refusal: a JSON error, the shape
// the console's API answers with, so seam shows the reason on every surface.
func writeRefusal(w http.ResponseWriter, status int, msg string) {
	body, err := json.Marshal(struct {
		Error string `json:"error"`
	}{Error: "seamless relay: " + msg})
	if err != nil {
		http.Error(w, "seamless relay: "+msg, status)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	// The answer is committed: a write error is the caller gone.
	_, _ = w.Write(append(body, '\n'))
}
