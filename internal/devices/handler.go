package devices

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"

	"github.com/arctop/seamless/internal/store"
)

// The pairing endpoints. They need no bearer key: the code (a pairing) and
// the device's secret (its status) are their only credentials, and the edge
// serves them before it judges any credential.
const (
	PairPath       = "/api/pair"
	PairStatusPath = "/api/pair/status"
)

// maxPairBody bounds a pairing request's body: a code, a kind, a name and a
// secret.
const maxPairBody = 4 << 10

// pairRequest is a pairing endpoint's JSON body. Unknown fields are refused:
// a misspelled name is present, and never silently becomes an absent one.
type pairRequest struct {
	Code   string `json:"code"`
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Secret string `json:"secret"`
}

// pairError is a refused pairing's JSON answer. Reason is a fixed word a
// client can act on: code, name, key, secret, kind, input, certificate,
// pairing, server, tls.
type pairError struct {
	Error  string `json:"error"`
	Reason string `json:"reason"`
}

// errPlainHTTPPairing is a pairing, or a pairing's status, asked over plain
// http from another computer (review L5): what it carries -- the code, the
// secret, and a key device's key in the answer -- would cross the network in
// the clear. The edge refuses it before anything reads it, so the code is not
// spent and no key is drawn. Loopback pairs over plain http as before: the
// request never left this computer (a TLS proxy in front of the daemon, a
// client on the same machine, the tests).
var errPlainHTTPPairing = errors.New("this server refuses a pairing over plain http from another computer: the code, and a " +
	"key device's key, would cross the network in the clear. Give the server TLS (on the server: seamlessd setup's address " +
	"step, or tls.cert_file and tls.key_file), then pair with its https address")

// clearPairing reports whether r, a pairing endpoint's request, would carry
// its secrets over the network in the clear: no TLS, from a peer that is not
// this computer's loopback. A peer address that does not parse counts as
// another computer's.
func clearPairing(r *http.Request) bool {
	if r.TLS != nil {
		return false
	}
	peer, err := netip.ParseAddrPort(r.RemoteAddr)
	return err != nil || !peer.Addr().Unmap().IsLoopback()
}

// Edge is the daemon's TCP edge for paired devices, around the routes:
//
//   - No relay attribute header (RelayAttributePrefix) reaches a route raw.
//     Under an active relay's Credential the edge reads them first
//     (ParseRelayAttributes): present, they must be well formed, or the
//     request is refused (400) before any route runs; what they say rides the
//     request's context (RelayAttributesOf), for the daemon's relay judge.
//     Every other request has them stripped unread, so a bearer request or a
//     key device carrying them is judged as the bearer request it is.
//   - It serves the pairing endpoints itself, and only over TLS or from this
//     computer's loopback: a pairing over plain http from another computer
//     is refused unread (errPlainHTTPPairing, 403, reason tls).
//   - A TLS connection that presented a client certificate is judged by that
//     certificate alone, never by a key it also sends: the certificate's key
//     must be an active relay's, else the request is refused (401) and the
//     connection closed. A pending, revoked or never-paired relay is refused.
//   - A bearer token shaped like a device key is matched against the active
//     key devices. A key that matches none passes on unmarked, and every
//     route refuses it exactly as it refuses a wrong key.
//   - A matched request carries its Credential (WithCredential), and speaks
//     for the device's enrolled host name: the X-Seamless-Host header and a
//     hook's host parameter are set to it, whatever the request said. Its
//     context ends when the device is revoked: the request is registered
//     (track) before the device's status is read a last time, so a
//     revocation through this daemon (Revoke), whenever it commits, either
//     finds the request registered and ends it, or is seen by that read and
//     refuses it (review I1). One from another process (the terminal) ends
//     it at the next sweep (Run).
//
// A request with no client certificate and no device key passes untouched
// but for the stripped headers, so the shared key is judged by the routes
// exactly as before.
func (s *Service) Edge(next http.Handler) http.Handler {
	return s.edge(next, nil)
}

// edge is Edge. lookedUp, when set, is called once a request's credential
// matched an active device, just before the request is registered: the test
// seam for the revocation window (review I1). Edge passes nil.
func (s *Service) edge(next http.Handler, lookedUp func(Credential)) http.Handler {
	pair := http.NewServeMux()
	pair.HandleFunc("POST "+PairPath, s.servePair)
	pair.HandleFunc("POST "+PairStatusPath, s.serveStatus)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == PairPath || r.URL.Path == PairStatusPath {
			StripRelayAttributes(r.Header)
			if clearPairing(r) {
				s.cfg.Logger.Warn("devices: refused a pairing over plain http from another computer", "path", r.URL.Path,
					"peer", r.RemoteAddr)
				writePairJSON(w, http.StatusForbidden, pairError{Error: errPlainHTTPPairing.Error(), Reason: "tls"})
				return
			}
			pair.ServeHTTP(w, r)
			return
		}
		cred, ok, err := s.authenticate(r)
		if err != nil {
			StripRelayAttributes(r.Header)
			s.refuse(w, r, err)
			return
		}
		var attrs RelayAttributes
		forwarded := false
		if ok && cred.Kind == KindRelay {
			// The relay's own report of its local caller: read before the
			// headers go, and refused whole when it does not parse.
			if attrs, forwarded, err = ParseRelayAttributes(r.Header); err != nil {
				s.cfg.Logger.Warn("devices: a relay's forwarded attributes are malformed", "device", cred.DeviceID, "path", r.URL.Path, "error", err)
				http.Error(w, "bad request: "+plainError(err), http.StatusBadRequest)
				return
			}
		}
		StripRelayAttributes(r.Header)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		if err := store.TouchDevice(r.Context(), s.cfg.DB, cred.DeviceID, s.cfg.Now()); err != nil {
			s.cfg.Logger.Warn("devices: record a device's use", "device", cred.DeviceID, "error", err)
		}
		if lookedUp != nil {
			lookedUp(cred)
		}
		ctx, done := s.track(r.Context(), cred.DeviceID)
		defer done()
		// A revocation that committed after authenticate read the device, and
		// ended the device's requests before this one was registered, did not
		// end this one. Registered now, it is ended by any revocation to come;
		// one that came already is in the device's status (review I1).
		switch active, err := s.stillActive(r.Context(), cred.DeviceID); {
		case err != nil:
			s.refuse(w, r, err)
			return
		case !active && cred.Kind == KindRelay:
			s.refuse(w, r, ErrRelayRevoked)
			return
		case !active:
			// A revoked key is no credential: it passes on unmarked, and the
			// routes refuse it as a wrong key.
			next.ServeHTTP(w, r)
			return
		}
		ctx = WithCredential(ctx, cred)
		if forwarded {
			ctx = withRelayAttributes(ctx, attrs)
		}
		next.ServeHTTP(w, speakFor(r.Clone(ctx), cred.Name))
	})
}

// authenticate matches r's own paired credential: ok is false for a request
// that presents none (no client certificate, no device key), or a device key
// no active device holds. An error refuses the request.
func (s *Service) authenticate(r *http.Request) (Credential, bool, error) {
	ctx := r.Context()
	spki, presented, err := RelayKeyOf(r.TLS)
	if presented {
		if err != nil {
			return Credential{}, false, err
		}
		d, err := store.DeviceByKey(ctx, s.cfg.DB, KindRelay, PublicKeyHash(spki))
		switch {
		case errors.Is(err, store.ErrDeviceNotFound):
			return Credential{}, false, ErrRelayNotPaired
		case err != nil:
			return Credential{}, false, err
		}
		switch d.Status {
		case store.DeviceActive:
			return Credential{DeviceID: d.ID, Kind: d.Kind, Name: d.Name, Approved: d.ApprovedBy != ""}, true, nil
		case store.DevicePending:
			return Credential{}, false, ErrRelayPending
		case store.DeviceRevoked:
			return Credential{}, false, ErrRelayRevoked
		}
		return Credential{}, false, ErrRelayNotPaired
	}
	token, ok := bearerToken(r)
	if !ok || !IsKey(token) {
		return Credential{}, false, nil
	}
	d, err := store.DeviceByKey(ctx, s.cfg.DB, KindKey, KeyHash(token))
	switch {
	case errors.Is(err, store.ErrDeviceNotFound):
		return Credential{}, false, nil
	case err != nil:
		return Credential{}, false, err
	}
	if d.Status != store.DeviceActive {
		return Credential{}, false, nil
	}
	return Credential{DeviceID: d.ID, Kind: d.Kind, Name: d.Name, Approved: d.ApprovedBy != ""}, true, nil
}

// stillActive reports whether the device id is active, read now: after the
// request it makes was registered.
func (s *Service) stillActive(ctx context.Context, id string) (bool, error) {
	d, err := store.DeviceByID(ctx, s.cfg.DB, id)
	switch {
	case errors.Is(err, store.ErrDeviceNotFound):
		return false, nil
	case err != nil:
		return false, err
	}
	return d.Status == store.DeviceActive, nil
}

// refuse answers a request whose credential the edge refused: a relay's as
// an invalid credential (401, which write screening records as auth.failed
// like a wrong key) with the connection closed, a store failure with 503.
func (s *Service) refuse(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrRelayNotPaired), errors.Is(err, ErrRelayPending), errors.Is(err, ErrRelayRevoked),
		errors.Is(err, ErrRelayCertificate):
		w.Header().Set("Connection", "close")
		http.Error(w, "unauthorized: "+err.Error(), http.StatusUnauthorized)
	default:
		s.cfg.Logger.Error("devices: check a device's credential", "path", r.URL.Path, "error", err)
		http.Error(w, "the server could not check this device's pairing; try again", http.StatusServiceUnavailable)
	}
}

// speakFor makes r speak for the host name: the MCP host header, and a hook's
// host parameter. A hook query that does not parse is left as it is, for the
// hook to refuse as before.
func speakFor(r *http.Request, name string) *http.Request {
	r.Header.Set(hostHeader, name)
	if strings.HasPrefix(r.URL.Path, hookPathStart) {
		if q, err := url.ParseQuery(r.URL.RawQuery); err == nil {
			q.Set(hookHostParam, name)
			r.URL.RawQuery = q.Encode()
		}
	}
	return r
}

// decodePair reads a pairing endpoint's body, strictly.
func decodePair(w http.ResponseWriter, r *http.Request) (pairRequest, bool) {
	var in pairRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPairBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writePairJSON(w, http.StatusBadRequest, pairError{Error: "the request must be one JSON object of code, kind, name and secret: " + err.Error(), Reason: "input"})
		return in, false
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writePairJSON(w, http.StatusBadRequest, pairError{Error: "the request must be one JSON object", Reason: "input"})
		return in, false
	}
	return in, true
}

// servePair is POST /api/pair: {code, kind, name, secret}. A relay's key is
// the certificate its TLS connection presented.
func (s *Service) servePair(w http.ResponseWriter, r *http.Request) {
	in, ok := decodePair(w, r)
	if !ok {
		return
	}
	spki, _, err := RelayKeyOf(r.TLS)
	if err != nil {
		s.pairRefused(w, r, err)
		return
	}
	answer, err := s.Pair(r.Context(), PairInput{Code: in.Code, Kind: in.Kind, Name: in.Name, Secret: in.Secret, RelayKey: spki})
	if err != nil {
		s.pairRefused(w, r, err)
		return
	}
	writePairJSON(w, http.StatusOK, answer)
}

// serveStatus is POST /api/pair/status: {secret}. Every other field must be
// absent.
func (s *Service) serveStatus(w http.ResponseWriter, r *http.Request) {
	in, ok := decodePair(w, r)
	if !ok {
		return
	}
	if in.Code != "" || in.Kind != "" || in.Name != "" {
		writePairJSON(w, http.StatusBadRequest, pairError{Error: "a status request carries the secret alone", Reason: "input"})
		return
	}
	answer, err := s.Status(r.Context(), in.Secret)
	if err != nil {
		s.pairRefused(w, r, err)
		return
	}
	writePairJSON(w, http.StatusOK, answer)
}

// pairRefused answers a refused pairing or status in words a device can act
// on.
func (s *Service) pairRefused(w http.ResponseWriter, r *http.Request, err error) {
	status, reason, msg := http.StatusBadRequest, "input", plainError(err)
	switch {
	case errors.Is(err, store.ErrPairingCode):
		status, reason = http.StatusForbidden, "code"
		msg = "that pairing code is not valid: it may be mistyped, expired (codes last 10 minutes) or already used; get a new one on the server"
	case errors.Is(err, store.ErrDeviceNameTaken):
		status, reason, msg = http.StatusConflict, "name", "a device of that name is already paired or waiting: revoke it on the server's Devices page first"
	case errors.Is(err, ErrServerName):
		status, reason, msg = http.StatusConflict, "name", ErrServerName.Error()
	case errors.Is(err, store.ErrDeviceKeyTaken):
		status, reason, msg = http.StatusConflict, "key", "this relay's key is already paired or waiting"
	case errors.Is(err, store.ErrDeviceSecretUsed):
		status, reason, msg = http.StatusConflict, "secret", store.ErrDeviceSecretUsed.Error()
	case errors.Is(err, ErrKind):
		reason, msg = "kind", ErrKind.Error()
	case errors.Is(err, ErrRelayNeedsCertificate), errors.Is(err, ErrKeyWithCertificate), errors.Is(err, ErrRelayCertificate):
		reason = "certificate"
	case errors.Is(err, ErrUnknownPairing):
		status, reason, msg = http.StatusNotFound, "pairing", ErrUnknownPairing.Error()
	case errors.Is(err, store.ErrInvalidDevice), errors.Is(err, ErrSecret):
		// An input the store or the secret's shape refused: its own words.
	default:
		s.cfg.Logger.Error("devices: pairing failed", "path", r.URL.Path, "error", err)
		status, reason, msg = http.StatusInternalServerError, "server", "the server could not take this pairing; try again"
	}
	writePairJSON(w, status, pairError{Error: msg, Reason: reason})
}

// opPrefix matches the "pkg.Func: " a wrapped error carries.
var opPrefix = regexp.MustCompile(`^[a-z]+\.[A-Za-z.]+: `)

// plainError is err's text without the operation names wrapping put in front
// of it: what a device is told of a refused input.
func plainError(err error) string {
	msg := err.Error()
	for {
		loc := opPrefix.FindStringIndex(msg)
		if loc == nil {
			return msg
		}
		msg = msg[loc[1]:]
	}
}

// writePairJSON writes v as the whole answer. A pairing answer can carry a
// device's key, so nothing may keep it.
func writePairJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "internal error: encode response", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	// The answer is committed: a write error is the client gone.
	_, _ = w.Write(append(body, '\n'))
}
