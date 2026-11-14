package devices

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
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
// pairing, server.
type pairError struct {
	Error  string `json:"error"`
	Reason string `json:"reason"`
}

// Edge is the daemon's TCP edge for paired devices, around the routes:
//
//   - It strips every relay attribute header (RelayAttributePrefix) from every
//     request: nothing honors one yet, and a bearer request carrying one is
//     judged as the bearer request it is.
//   - It serves the pairing endpoints itself.
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
//     context ends when the device is revoked.
//
// A request with no client certificate and no device key passes untouched
// but for the stripped headers, so the shared key is judged by the routes
// exactly as before.
func (s *Service) Edge(next http.Handler) http.Handler {
	pair := http.NewServeMux()
	pair.HandleFunc("POST "+PairPath, s.servePair)
	pair.HandleFunc("POST "+PairStatusPath, s.serveStatus)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		StripRelayAttributes(r.Header)
		if r.URL.Path == PairPath || r.URL.Path == PairStatusPath {
			pair.ServeHTTP(w, r)
			return
		}
		cred, ok, err := s.authenticate(r)
		if err != nil {
			s.refuse(w, r, err)
			return
		}
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		if err := store.TouchDevice(r.Context(), s.cfg.DB, cred.DeviceID, s.cfg.Now()); err != nil {
			s.cfg.Logger.Warn("devices: record a device's use", "device", cred.DeviceID, "error", err)
		}
		ctx, done := s.track(r.Context(), cred.DeviceID)
		defer done()
		next.ServeHTTP(w, speakFor(r.Clone(WithCredential(ctx, cred)), cred.Name))
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
			return Credential{DeviceID: d.ID, Kind: d.Kind, Name: d.Name}, true, nil
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
	return Credential{DeviceID: d.ID, Kind: d.Kind, Name: d.Name}, true, nil
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
