package console

// The owner's decisions through a relay (owner decision R-D3). A request a
// paired relay forwarded from a process chain the transport judged the
// owner's own hands on the relay's computer (access.OwnerHandsThrough) passes
// the owner-decision guards (refusedFromDevice, unlock.go). Each such
// decision records one event naming the relay and the chain, so the owner
// can see afterwards which computer, and which programs there, made it.

import (
	"context"
	"net/http"
	"path"
	"strings"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/devices"
)

// relayDecision is one POST's record of whether an owner-decision guard let
// a relay's owner hands through (noteRelayDecision).
type relayDecision struct{ passed bool }

type relayDecisionKey struct{}

// noteRelayDecision marks r's decision as one a guard let through for the
// owner's own hands through a relay, for relayDecisions to record.
func noteRelayDecision(r *http.Request) {
	if d, ok := r.Context().Value(relayDecisionKey{}).(*relayDecision); ok {
		d.passed = true
	}
}

// relayDecisions wraps every console POST (Register's post): for a request
// the transport judged the owner's own hands through a relay, it records one
// devices.EventRelayDecision when a guard let the decision through and the
// route answered it (a status below 400). Any other request runs as it came.
func (s *Service) relayDecisions(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cred, ok := devices.CredentialOf(r.Context())
		if !ok || cred.Kind != devices.KindRelay {
			next(w, r)
			return
		}
		hands, ok := access.OwnerHandsThrough(r.Context(), cred.DeviceID)
		if !ok {
			next(w, r)
			return
		}
		d := &relayDecision{}
		sw := &statusWriter{ResponseWriter: w}
		next(sw, r.WithContext(context.WithValue(r.Context(), relayDecisionKey{}, d)))
		if d.passed && sw.status() < http.StatusBadRequest {
			s.recordRelayDecision(r, cred, hands)
		}
	}
}

// recordRelayDecision records the decision r made through the relay cred
// names, from the chain hands judged: the relay's name and device, the
// owner's uid there, the route, and the chain's programs by base name.
func (s *Service) recordRelayDecision(r *http.Request, cred devices.Credential, hands access.RelayHands) {
	if s.cfg.Events == nil {
		return
	}
	programs := make([]string, len(hands.Programs))
	for i, p := range hands.Programs {
		if p = strings.TrimSpace(p); p != "" {
			programs[i] = path.Base(p)
		}
	}
	v, _ := access.FromContext(r.Context())
	ctx := context.WithoutCancel(r.Context())
	if _, err := s.cfg.Events.Record(ctx, core.Event{Kind: devices.EventRelayDecision, ItemID: cred.DeviceID,
		Payload: map[string]any{"name": cred.Name, "uid": v.Principal().UID, "route": r.Pattern, "programs": programs}}); err != nil {
		s.logger.Warn("console: record a decision made through a relay", "relay", cred.Name, "error", err)
	}
}

// statusWriter keeps the status a handler answered with.
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the writer underneath.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// status is the status the handler answered with: 200 when it wrote none.
func (w *statusWriter) status() int {
	if w.code == 0 {
		return http.StatusOK
	}
	return w.code
}
