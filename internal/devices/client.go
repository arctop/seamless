package devices

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/store"
)

// The client side of a pairing: what the relay (task 01M73RFZ3XMKJGNE9QNX9XK5BS)
// and the client branch of `seamlessd setup` (task 01M73RGB4PQVSXCN4V8VT5XNBX)
// call on the other computer.

// defaultPoll is how often Pair asks after a pairing that waits.
const defaultPoll = 2 * time.Second

// maxPairAnswer bounds a pairing answer's body.
const maxPairAnswer = 64 << 10

var (
	// ErrCodeRefused is a code the server did not take: mistyped, expired or
	// already used.
	ErrCodeRefused = errors.New("the server refused the pairing code: it may be mistyped, expired or already used")
	// ErrNameTaken is a device name the server already pairs, or its own.
	ErrNameTaken = errors.New("the server already pairs a device of this name")
	// ErrPairingDenied is a pairing the owner denied.
	ErrPairingDenied = errors.New("the owner denied this pairing on the server")
	// ErrPairingExpired is a pairing nobody approved in time.
	ErrPairingExpired = errors.New("this pairing was not approved in time: get a new code and pair again")
	// ErrPairingRevoked is a pairing revoked before it finished.
	ErrPairingRevoked = errors.New("this pairing was revoked on the server")
	// ErrServerFingerprint is a server whose certificate is not the one the
	// owner compared: nothing was sent to it.
	ErrServerFingerprint = errors.New("the server's certificate does not have the fingerprint you compared; nothing was sent")
	// ErrKeyLost is a key device paired whose key never arrived: the answer
	// that carried it was lost. The owner revokes it and pairs again.
	ErrKeyLost = errors.New("the device paired, but its key was lost on the way: revoke it on the server's Devices page and pair again")
	// ErrPlainHTTP is a pairing asked of a server on another computer over
	// plain http: the code, and a key device's key in the answer, would cross
	// the network in the clear. Only loopback may pair over http.
	ErrPlainHTTP = errors.New("pairing with a server on another computer needs https: over plain http the code and this computer's key would cross the network in the clear")
)

// PairRequest is one pairing, as a device asks for it.
type PairRequest struct {
	// Code is the one-time code the owner minted on the server.
	Code string
	// Name is this machine's host name (config.Hostname): the host every
	// request of the paired device speaks for.
	Name string
	// Relay is the relay's identity; nil pairs a key device.
	Relay *RelayIdentity
	// ServerFingerprint, when set, is the server certificate's fingerprint
	// the owner compared (CertFingerprint): the code goes only over a
	// connection whose certificate has it, on top of the usual verification.
	ServerFingerprint string
	// Poll is how often to ask after a pairing that waits for the owner's
	// approval; 0 means every two seconds.
	Poll time.Duration
	// Waiting, when set, is told once that the pairing waits for the owner's
	// passkey, with the fingerprint the Devices page shows.
	Waiting func(fingerprint string)
}

// Paired is a finished pairing.
type Paired struct {
	DeviceID    string
	Kind        string
	Name        string
	Fingerprint string
	// Key is a key device's own key: it goes into the client's config (0600)
	// as its mcp.api_key, in place of the server's shared key.
	Key string
	// ServerFingerprint is the certificate the server presented
	// (CertFingerprint), "" over plain http: a relay pins it.
	ServerFingerprint string
}

// Pair pairs this machine with the server at serverURL, through client: an
// *http.Client from config.Config.HTTPClient, the one TLS trust decision.
// Pair works on a copy of it that follows no redirect (an answer carrying a
// code or a key must never be re-sent elsewhere), presents the relay's
// certificate for a relay, and pins req.ServerFingerprint when set. A pairing
// that waits for the owner's approval is asked after every req.Poll until it
// is decided or ctx ends.
func Pair(ctx context.Context, client *http.Client, serverURL string, req PairRequest) (Paired, error) {
	const op = "devices.Pair"
	code := store.FormatPairingCode(req.Code)
	if code == "" {
		return Paired{}, fmt.Errorf("%s: %w", op, ErrCodeRefused)
	}
	name := NormalizeName(req.Name)
	if err := store.ValidDeviceName(name); err != nil {
		return Paired{}, fmt.Errorf("%s: %w", op, err)
	}
	base, err := url.Parse(strings.TrimRight(serverURL, "/"))
	if err != nil || (base.Scheme != "https" && base.Scheme != "http") || base.Host == "" {
		return Paired{}, fmt.Errorf("%s: %q is not a server address (https://host:port)", op, serverURL)
	}
	if req.Relay != nil && base.Scheme != "https" {
		return Paired{}, fmt.Errorf("%s: %w: the server's address must be https", op, ErrRelayNeedsCertificate)
	}
	if base.Scheme == "http" && !loopbackHost(base.Hostname()) {
		return Paired{}, fmt.Errorf("%s: %w", op, ErrPlainHTTP)
	}
	hc, err := pairingClient(client, req)
	if err != nil {
		return Paired{}, fmt.Errorf("%s: %w", op, err)
	}
	secret, err := NewSecret()
	if err != nil {
		return Paired{}, fmt.Errorf("%s: %w", op, err)
	}
	kind, want := KindKey, ""
	if req.Relay != nil {
		kind, want = KindRelay, req.Relay.Fingerprint()
	} else if want, err = SecretFingerprint(secret); err != nil {
		return Paired{}, fmt.Errorf("%s: %w", op, err)
	}
	answer, serverFP, err := postPair(ctx, hc, base.String()+PairPath, pairRequest{Code: code, Kind: kind, Name: name, Secret: secret})
	var transport *transportError
	if errors.As(err, &transport) {
		// The request may have arrived and its answer been lost: the secret
		// asks after it, so a pairing the server took is not lost with it.
		answer, serverFP, err = postPair(ctx, hc, base.String()+PairStatusPath, pairRequest{Secret: secret})
		if errors.Is(err, ErrUnknownPairing) {
			err = transport
		}
	}
	if err != nil {
		return Paired{}, fmt.Errorf("%s: %w", op, err)
	}
	if answer.Device.Fingerprint != want {
		return Paired{}, fmt.Errorf("%s: the server shows fingerprint %s for this pairing, and this machine %s", op, answer.Device.Fingerprint, want)
	}
	told := false
	poll := req.Poll
	if poll <= 0 {
		poll = defaultPoll
	}
	for answer.Status == StatusPending {
		if !told && req.Waiting != nil {
			req.Waiting(want)
			told = true
		}
		select {
		case <-ctx.Done():
			return Paired{}, fmt.Errorf("%s: waiting for the owner's approval: %w", op, ctx.Err())
		case <-time.After(poll):
		}
		next, fp, err := postPair(ctx, hc, base.String()+PairStatusPath, pairRequest{Secret: secret})
		if err != nil {
			var t *transportError
			if errors.As(err, &t) {
				continue // the server is unreachable for now: ask again
			}
			return Paired{}, fmt.Errorf("%s: %w", op, err)
		}
		answer, serverFP = next, fp
	}
	switch answer.Status {
	case StatusPaired:
	case store.DeviceDenied:
		return Paired{}, fmt.Errorf("%s: %w", op, ErrPairingDenied)
	case store.DeviceExpired:
		return Paired{}, fmt.Errorf("%s: %w", op, ErrPairingExpired)
	case store.DeviceRevoked:
		return Paired{}, fmt.Errorf("%s: %w", op, ErrPairingRevoked)
	default:
		return Paired{}, fmt.Errorf("%s: the server answered status %q", op, answer.Status)
	}
	if kind == KindKey && !IsKey(answer.Key) {
		return Paired{}, fmt.Errorf("%s: %w", op, ErrKeyLost)
	}
	return Paired{DeviceID: answer.Device.ID, Kind: answer.Device.Kind, Name: answer.Device.Name,
		Fingerprint: answer.Device.Fingerprint, Key: answer.Key, ServerFingerprint: serverFP}, nil
}

// loopbackHost reports whether host names this computer's loopback alone:
// localhost, or a loopback address.
func loopbackHost(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	ip, err := netip.ParseAddr(strings.Trim(h, "[]"))
	return err == nil && ip.Unmap().IsLoopback()
}

// pairingClient is a copy of client for one pairing: no redirect followed,
// the relay's certificate presented, the server's fingerprint pinned.
func pairingClient(client *http.Client, req PairRequest) (*http.Client, error) {
	if client == nil {
		return nil, errors.New("a pairing needs the client config.Config.HTTPClient builds")
	}
	out := *client
	out.CheckRedirect = config.RefuseRedirect
	if req.Relay == nil && req.ServerFingerprint == "" {
		return &out, nil
	}
	tr, err := pinnedTransport(client, req.Relay, req.ServerFingerprint)
	if err != nil {
		return nil, fmt.Errorf("a pairing: %w", err)
	}
	out.Transport = tr
	return &out, nil
}

// ValidCertFingerprint reports whether s has the shape CertFingerprint gives a
// certificate's SHA-256 (32 bytes of hex; case, colons, spaces and dashes
// aside, as sameCertFingerprint compares them): what a relay may pin.
func ValidCertFingerprint(s string) bool {
	hexOnly := strings.NewReplacer(":", "", " ", "", "-", "").Replace(strings.TrimSpace(s))
	if len(hexOnly) != 64 {
		return false
	}
	for _, c := range hexOnly {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// ErrNoServerPin is a relay transport asked for without the fingerprint of the
// server certificate the relay pinned at pairing.
var ErrNoServerPin = errors.New("a relay forwards only to the server certificate it pinned at pairing, and none is recorded")

// RelayTransport returns the transport a paired relay forwards over (plan
// networked-relay; package internal/relay): a copy of client's -- the one
// config.Config.HTTPClient builds, so the trust decision is every other
// surface's -- that presents id's certificate whenever the server asks
// (ClientTLS: RelayALPN offered, so the server asks) and accepts only a
// server certificate whose CertFingerprint is fingerprint, the one recorded at
// pairing, on top of the chain's verification and never instead of it. Any
// other certificate fails the handshake with ErrServerFingerprint before a
// request is sent.
func RelayTransport(client *http.Client, id *RelayIdentity, fingerprint string) (*http.Transport, error) {
	const op = "devices.RelayTransport"
	if client == nil {
		return nil, fmt.Errorf("%s: no client: build it with config.Config.HTTPClient", op)
	}
	if id == nil {
		return nil, fmt.Errorf("%s: %w: none", op, ErrRelayIdentity)
	}
	if strings.TrimSpace(fingerprint) == "" {
		return nil, fmt.Errorf("%s: %w", op, ErrNoServerPin)
	}
	tr, err := pinnedTransport(client, id, fingerprint)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	return tr, nil
}

// pinnedTransport is a copy of client's transport with relay's certificate
// presented (when relay is set) and the server's certificate pinned to
// fingerprint (when it is set).
func pinnedTransport(client *http.Client, relay *RelayIdentity, fingerprint string) (*http.Transport, error) {
	var tr *http.Transport
	switch t := client.Transport.(type) {
	case nil:
		tr = http.DefaultTransport.(*http.Transport).Clone()
	case *http.Transport:
		tr = t.Clone()
	default:
		return nil, fmt.Errorf("an *http.Transport is needed to set TLS on, not %T", client.Transport)
	}
	tlsCfg := tr.TLSClientConfig
	if relay != nil {
		tlsCfg = relay.ClientTLS(tlsCfg)
	} else if tlsCfg == nil {
		tlsCfg = &tls.Config{MinVersion: tls.VersionTLS12}
	} else {
		tlsCfg = tlsCfg.Clone()
	}
	if want := fingerprint; want != "" {
		// On top of the chain's verification, never instead of it.
		prior := tlsCfg.VerifyConnection
		tlsCfg.VerifyConnection = func(cs tls.ConnectionState) error {
			if prior != nil {
				if err := prior(cs); err != nil {
					return err
				}
			}
			if len(cs.PeerCertificates) == 0 || !sameCertFingerprint(CertFingerprint(cs.PeerCertificates[0].Raw), want) {
				return ErrServerFingerprint
			}
			return nil
		}
	}
	tr.TLSClientConfig = tlsCfg
	return tr, nil
}

// transportError is a request that got no answer: nothing says whether the
// server took it.
type transportError struct{ err error }

func (e *transportError) Error() string { return "the server could not be reached: " + e.err.Error() }
func (e *transportError) Unwrap() error { return e.err }

// postPair sends one pairing request and reads its answer, with the
// fingerprint of the certificate the server presented.
func postPair(ctx context.Context, hc *http.Client, target string, body pairRequest) (PairAnswer, string, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return PairAnswer{}, "", err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(raw))
	if err != nil {
		return PairAnswer{}, "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	resp, err := hc.Do(httpReq)
	if err != nil {
		if errors.Is(err, ErrServerFingerprint) {
			return PairAnswer{}, "", ErrServerFingerprint
		}
		if ctx.Err() != nil {
			return PairAnswer{}, "", ctx.Err()
		}
		return PairAnswer{}, "", &transportError{err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxPairAnswer))
	if err != nil {
		return PairAnswer{}, "", &transportError{err: err}
	}
	fp := ""
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		fp = CertFingerprint(resp.TLS.PeerCertificates[0].Raw)
	}
	if resp.StatusCode != http.StatusOK {
		var refused pairError
		if json.Unmarshal(data, &refused) != nil || refused.Reason == "" {
			return PairAnswer{}, fp, fmt.Errorf("the server answered %s", resp.Status)
		}
		switch refused.Reason {
		case "code":
			return PairAnswer{}, fp, ErrCodeRefused
		case "name":
			return PairAnswer{}, fp, fmt.Errorf("%w: %s", ErrNameTaken, refused.Error)
		case "pairing":
			return PairAnswer{}, fp, ErrUnknownPairing
		}
		return PairAnswer{}, fp, fmt.Errorf("the server refused the pairing (%s): %s", refused.Reason, refused.Error)
	}
	var answer PairAnswer
	if err := json.Unmarshal(data, &answer); err != nil {
		return PairAnswer{}, fp, fmt.Errorf("the server's answer is not a pairing: %w", err)
	}
	return answer, fp, nil
}
