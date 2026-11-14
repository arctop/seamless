package relay

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"
)

// shutdownWait bounds a stop: in-flight requests end with the serve context
// (their base context), and Shutdown waits this long for them to return.
const shutdownWait = 5 * time.Second

// ErrSocketTaken is a socket path something else holds: a file that is not the
// relay's own socket, or a socket a running process answers (a peer_auth
// daemon, another relay).
var ErrSocketTaken = errors.New("the socket path is taken")

// NewServer is the relay's local HTTP server: h behind the kernel's account of
// every connection (ConnContext), with requests based on ctx, so a stop ends
// every stream at once. It sets no read or write deadline past the headers: a
// stream -- an MCP SSE response, an elicitation's wait, the console's live
// feed -- lasts as long as the server keeps it, and a ReadTimeout would end it
// once its request's body was read (Go's background read then cancels the
// request's context).
func NewServer(ctx context.Context, h http.Handler, logger *slog.Logger) *http.Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &http.Server{
		Handler:           h,
		ConnContext:       ConnContext,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelDebug),
	}
}

// Serve serves h on ln until ctx ends, then stops: it returns nil after a stop
// ctx asked for, or the error that ended serving.
func Serve(ctx context.Context, ln net.Listener, h http.Handler, logger *slog.Logger) error {
	srv := NewServer(ctx, h, logger)
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	select {
	case err := <-done:
		return fmt.Errorf("relay.Serve: %w", err)
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownWait)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		_ = srv.Close() //nolint:errcheck // the stop's own deadline passed; Close only ends what is left
	}
	if err := <-done; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("relay.Serve: %w", err)
	}
	return nil
}

// SocketAccess is who may connect to the relay's socket: its mode, and a group
// to hand it to, as peer_auth.socket's mode and group are for the daemon's.
// The zero value is SocketMode and the relay's own group: every local account
// may connect, and only a server under peer_auth tells their uids apart.
type SocketAccess struct {
	// Mode is the socket file's permission bits; 0 is SocketMode.
	Mode fs.FileMode
	// GID is the group the socket is handed to; -1 or 0 leaves the relay's
	// primary group (root's group is never handed a socket).
	GID int
}

// Listen binds the relay's socket at path, mode SocketMode: Env.ListenWith
// with the zero SocketAccess.
func (e Env) Listen(path string) (net.Listener, error) { return e.ListenWith(path, SocketAccess{}) }

// ListenWith binds the relay's socket at path, with a's mode and group. A
// stale socket of the relay's own account is removed first, once a dial shows
// nothing answers it; anything else at the path is ErrSocketTaken.
func (e Env) ListenWith(path string, a SocketAccess) (net.Listener, error) {
	mode := a.Mode
	if mode == 0 {
		mode = SocketMode
	}
	facts, err := e.Inspect(path)
	switch {
	case err == nil:
		if !facts.Socket || int(facts.UID) != e.EUID {
			return nil, fmt.Errorf("%w: %s exists and is not this relay's socket: remove it by hand if it is stale", ErrSocketTaken, path)
		}
		if answered(path) {
			return nil, fmt.Errorf("%w: a running process answers %s (a peer_auth daemon, or another relay): stop it first", ErrSocketTaken, path)
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("relay: remove the stale socket %s: %w", path, err)
		}
	case !errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("relay: socket %s: %w", path, err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("relay: listen on %s: %w", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		_ = ln.Close() //nolint:errcheck // the chmod's failure is the one reported
		return nil, fmt.Errorf("relay: chmod the socket %s: %w", path, err)
	}
	if a.GID > 0 {
		if err := os.Chown(path, -1, a.GID); err != nil {
			_ = ln.Close() //nolint:errcheck // the chown's failure is the one reported
			return nil, fmt.Errorf("relay: hand the socket %s to group %d (the relay's account must be a member): %w", path, a.GID, err)
		}
	}
	return ln, nil
}

// answered reports whether a process accepts connections on the socket.
func answered(path string) bool {
	c, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		return false
	}
	_ = c.Close() //nolint:errcheck // a probe: the connection carried nothing
	return true
}
