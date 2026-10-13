package main

// Riding out a daemon restart: the dial-phase retry that seam mcp-proxy and
// seam hook share.
//
// An automatic update restarts seamlessd while agent sessions are open (on
// macOS a ~2s launchd bootout/bootstrap), and both surfaces have to survive it:
// the bridge is a stdio MCP server its client will not restart, and a hook that
// fails loses its payload. While the daemon is down its port refuses
// connections, so both re-dial for a bounded window.
//
// The rule they share: a request is re-sent only when it provably never left
// this process. Tool calls and hook payloads are not idempotent -- memory_write,
// tasks_add and session-start all create something -- so once a connection
// existed a failure is final, whatever it looks like: the daemon may have read
// the request and acted on it before the connection died.
//
// "Provably" is read from httptrace, not from the error text:
//
//   - The failing attempt never held a connection: GetConn opened it and no
//     GotConn followed. With no connection there is nothing to write on. The
//     write-side hooks cannot carry this: WroteHeaders and WroteRequest fire
//     when the request reaches the transport's buffer, before its write loop
//     flushes it, so they prove neither that bytes left nor that they did not.
//   - No attempt read a response byte. A redirect is a response, and the hop
//     after it can fail to dial after the first hop already has the request.
//
// Of what was not sent, only the dialer's own failure is retried: a
// *net.OpError with Op "dial" (refused, unreachable, timed out, unresolvable),
// matched by type because a refused connect reads differently on each OS. A
// TLS verification failure also comes before any connection, but it is a
// configuration fault no retry clears, so it is reported at once.
//
// net/http's own retry is the other half and stays net/http's: on a reused
// keep-alive connection it re-sends a POST only when not one byte was written
// (which needs Request.GetBody; a bytes.Reader body has it), and never after
// bytes went out (transport.go, shouldRetryRequest). After a restart the dead
// pooled connection is normally gone before the next request -- the transport
// drops it when the old daemon's FIN arrives, measured well under a millisecond
// on loopback -- so that request dials, is refused, and lands here.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"
	"time"
)

// errNotSent marks a failed exchange whose request provably never left this
// process. A caller may say so, and the request is safe to try again; any other
// failure may already have been acted on.
var errNotSent = errors.New("request not sent")

// dialRetry is one surface's retry policy. Its clock is injectable so a test
// never waits out a real budget.
type dialRetry struct {
	// budget is how long after the first attempt a retry may still START. An
	// attempt in flight when it runs out ends on its own dial timeout
	// (config.DialTimeout) or at the request context's deadline, whichever
	// comes first; a refused dial ends at once.
	budget time.Duration
	// backoff is the wait before retry n (1-based), cut to what is left of the
	// budget.
	backoff func(n int) time.Duration
	now     func() time.Time
	// wait blocks for d or until ctx is done, and then returns ctx's error.
	wait func(ctx context.Context, d time.Duration) error
}

// newDialRetry is the production policy for a budget: dialBackoff on the real
// clock.
func newDialRetry(budget time.Duration) *dialRetry {
	return &dialRetry{budget: budget, backoff: dialBackoff, now: time.Now, wait: waitCtx}
}

// dialBackoff doubles from 100ms to a 1s ceiling: a restarted daemon is picked
// up within a second of listening again, and a 30s outage costs about 35
// refused dials, each answered by the kernel in well under a millisecond.
func dialBackoff(n int) time.Duration {
	return min(100*time.Millisecond<<min(max(n-1, 0), 4), time.Second)
}

// waitCtx blocks for d, or until ctx is done.
func waitCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// do sends req through client as client.Do would, re-sending it after a
// dial-phase failure until the budget is spent and never after anything else.
// req's context bounds the whole run, waits included. A request with a body is
// re-sent only if it has GetBody (http.NewRequest sets it for a bytes.Reader).
//
// An error wraps errNotSent when the request never left; otherwise the request
// may have been delivered and acted on.
func (r *dialRetry) do(client *http.Client, req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	start := r.now()
	for n := 1; ; n++ {
		var p sendProbe
		attempt := req.WithContext(p.trace(ctx))
		if n > 1 && req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return nil, notSent(n, r.now().Sub(start), fmt.Errorf("rewind request body: %w", err))
			}
			attempt.Body = body
		}
		resp, err := client.Do(attempt)
		if err == nil {
			return resp, nil
		}
		if !p.unsent() {
			return nil, err
		}
		elapsed := r.now().Sub(start)
		if !isDialError(err) || !rewindable(req) || ctx.Err() != nil || elapsed >= r.budget {
			return nil, notSent(n, elapsed, err)
		}
		if werr := r.wait(ctx, min(r.backoff(n), r.budget-elapsed)); werr != nil {
			return nil, notSent(n, r.now().Sub(start), fmt.Errorf("%w (last attempt: %w)", werr, err))
		}
	}
}

// notSent marks err as a failure before anything was sent, adding the retry
// summary once there was more than one attempt.
func notSent(attempts int, elapsed time.Duration, err error) error {
	if attempts == 1 {
		return fmt.Errorf("%w: %w", errNotSent, err)
	}
	return fmt.Errorf("%w after %d attempts over %s: %w", errNotSent, attempts, elapsed.Round(100*time.Millisecond), err)
}

// rewindable reports whether req's body can be sent again: it has none, or it
// has GetBody.
func rewindable(req *http.Request) bool {
	return req.Body == nil || req.Body == http.NoBody || req.GetBody != nil
}

// isDialError reports whether err is the dialer's own failure, at any depth: a
// dial to a proxy fails inside a "proxyconnect" OpError.
func isDialError(err error) bool {
	var op *net.OpError
	for errors.As(err, &op) {
		if op.Op == "dial" {
			return true
		}
		err = op.Err
	}
	return false
}

// sendProbe follows one request through httptrace (see the file comment). The
// transport calls GetConn at the start of every attempt, its own internal
// retries included, so connected describes the attempt whose error Do returned;
// a response byte on any attempt means a server received the request. The
// hooks run on transport goroutines, hence the atomics.
type sendProbe struct {
	connected atomic.Bool
	answered  atomic.Bool
}

func (p *sendProbe) trace(ctx context.Context) context.Context {
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GetConn:              func(string) { p.connected.Store(false) },
		GotConn:              func(httptrace.GotConnInfo) { p.connected.Store(true) },
		GotFirstResponseByte: func() { p.answered.Store(true) },
	})
}

// unsent reports whether the request provably never left: the failing attempt
// held no connection and no attempt was answered.
func (p *sendProbe) unsent() bool { return !p.connected.Load() && !p.answered.Load() }
