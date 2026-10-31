package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
)

// ssePingInterval keeps the stream and any intermediary connections alive during
// quiet periods.
const ssePingInterval = 25 * time.Second

// sse streams the live event feed as Server-Sent Events. Each recorded event is
// sent as one JSON `data:` frame (the display projection, matching the overview
// table). The stream stays open until the client disconnects.
func (s *Service) sse(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Events == nil {
		http.Error(w, "live feed unavailable", http.StatusServiceUnavailable)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	// Opt out of the server's per-request ReadTimeout for the life of this
	// stream. That timeout exists to bound slow request bodies (audit L7), but
	// it is a deadline on the *connection*: once it expires, the net/http
	// background read that watches for client disconnects fails, and Go cancels
	// the request context -- silently killing an idle feed after 30s. An SSE
	// request has no body left to read, so dropping the deadline costs nothing.
	// A server without SetReadDeadline support (a test recorder) just skips it.
	if err := http.NewResponseController(w).SetReadDeadline(time.Time{}); err != nil &&
		!errors.Is(err, http.ErrNotSupported) {
		s.logger.Warn("console: sse clear read deadline", "error", err)
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch, unsubscribe := s.cfg.Events.Subscribe()
	defer unsubscribe()

	ctx := r.Context()

	// Opt-in richer feed for the Interactions screen: transport-level rows with
	// full request/response bodies, instead of the default summary rows every
	// page's layout EventSource consumes.
	interactions := r.URL.Query().Get("feed") == "interactions"

	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	ping := time.NewTicker(ssePingInterval)
	defer ping.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case e, open := <-ch:
			if !open {
				return
			}
			if !s.liveEventVisible(ctx, e) {
				continue
			}
			var payload []byte
			var err error
			if interactions {
				if !isInteraction(e) || skipInteraction(e) {
					continue
				}
				// A fresh resolver per event, not one memo per connection:
				// Session.Model is mutable (ambient sessions start without one
				// and /model switches change it mid-session), so a
				// connection-lifetime cache would pin stale pills that disagree
				// with gap-filled rows built from fresh lookups.
				payload, err = json.Marshal(toInteractionRow(e, s.sessionNamer(ctx)))
			} else {
				payload, err = json.Marshal(toEventRow(e))
			}
			if err != nil {
				s.logger.Warn("console: sse marshal", "error", err)
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", payload)
			flusher.Flush()
		}
	}
}

// liveEventVisible applies the console's peer_auth filter to the live feed. The
// fan-out is in memory and carries every event the daemon records, while the
// console's pages read through a filtered handle (Config.WriteDB set). The
// event row is already committed when it is published, so the question "may
// this console see it" is asked of the same filtered view the pages read: the
// row is fetched back through it, and an event a sealed project hides -- by its
// project, its session or the item it is about -- is simply not there. With no
// filter (peer_auth off) every event passes, as before.
func (s *Service) liveEventVisible(ctx context.Context, e core.Event) bool {
	if s.cfg.WriteDB == nil {
		return true
	}
	if !access.Visible(ctx, e.ProjectSlug) {
		return false
	}
	_, ok, err := s.cfg.Events.ByID(ctx, e.ID)
	if err != nil {
		s.logger.Warn("console: sse visibility", "error", err)
		return false
	}
	return ok
}
