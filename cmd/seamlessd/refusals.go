package main

import (
	"net/http"
	"strings"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/screening"
)

// refusalWatch records write screening's access.refused for the refusals the
// daemon makes before any surface runs, and for the ones a surface makes for
// who the caller is rather than what it asked:
//
//   - a 401 on any route: no valid bearer key (every 401 the daemon answers is
//     a bearer refusal -- the MCP, hook, A2A and console handlers alike);
//   - a reason a handler noted in the request (access.NoteRefusal): a socket
//     caller whose OS account peer_auth does not know, a restricted account
//     at the owner's console -- named by the note, since this handler's own
//     context never held the request's View.
//
// The fence refusals inside the MCP tools are recorded where they happen,
// with the tool and the target project. Off -- the feature's default --
// RecordRefused records nothing, and this wrapper only reads a status code.
func refusalWatch(screen *screening.Service, next http.Handler) http.Handler {
	if screen == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, note := access.WithRefusalNote(r.Context())
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r.WithContext(ctx))
		reason := note.Reason()
		if reason == "" && sw.status == http.StatusUnauthorized {
			reason = screening.RefusedAuthFailed
		}
		if reason == "" {
			return
		}
		screen.RecordRefused(ctx, screening.Refusal{Reason: reason, Surface: surfaceOf(r.URL.Path), Account: note.Account()})
	})
}

// surfaceOf names the surface a path belongs to.
func surfaceOf(path string) string {
	switch {
	case path == "/api/mcp" || strings.HasPrefix(path, "/api/mcp/"):
		return "mcp"
	case strings.HasPrefix(path, "/api/hooks/"):
		return "hooks"
	case strings.HasPrefix(path, "/api/a2a") || strings.HasPrefix(path, "/.well-known/"):
		return "a2a"
	case path == "/console" || strings.HasPrefix(path, "/console/"):
		return "console"
	}
	return "api"
}

// statusWriter remembers the status a handler sent. It forwards Flush (the
// console's live feed and MCP's streams flush) and unwraps for
// http.ResponseController.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
