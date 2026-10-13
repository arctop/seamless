package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
)

// fakeClock stands in for dialRetry's clock. wait returns at once and moves now
// forward by the wait it was asked for, so a test spends none of a real budget.
// onWait runs inside the n-th wait (1-based): the moment a test brings a daemon
// back. deadline is the real deadline of the context the last wait ran under,
// zero for none: the request's own, which the fake clock does not move.
type fakeClock struct {
	mu       sync.Mutex
	t        time.Time
	waits    []time.Duration
	deadline time.Time
	onWait   func(n int)
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) wait(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.waits = append(c.waits, d)
	c.deadline, _ = ctx.Deadline()
	n := len(c.waits)
	c.mu.Unlock()
	if c.onWait != nil {
		c.onWait(n)
	}
	return ctx.Err()
}

// policy is the production policy for budget -- the real backoff -- on this
// clock.
func (c *fakeClock) policy(budget time.Duration) *dialRetry {
	r := newDialRetry(budget)
	r.now, r.wait = c.now, c.wait
	return r
}

func (c *fakeClock) recorded() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.waits...)
}

func (c *fakeClock) lastDeadline() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.deadline
}

func (c *fakeClock) waited() time.Duration {
	var sum time.Duration
	for _, d := range c.recorded() {
		sum += d
	}
	return sum
}

// deadAddr returns a loopback address nothing listens on -- bound, then
// released -- so a dial to it is refused until a test serves there again.
func deadAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	return addr
}

// serveAt starts h on addr: the daemon coming back where it was.
func serveAt(t *testing.T, addr string, h http.Handler) *httptest.Server {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	require.NoError(t, err)
	srv := httptest.NewUnstartedServer(h)
	require.NoError(t, srv.Listener.Close())
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// hangUp drops the connection behind w without another byte: a daemon that dies
// with the request in hand. Whatever was already written stays written.
func hangUp(w http.ResponseWriter) {
	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		http.Error(w, "hijack: "+err.Error(), http.StatusInternalServerError)
		return
	}
	_ = conn.Close()
}

// counting answers "ok" to /ok and hangs up on /hang-up after reading the whole
// request, counting the requests it read.
func counting(hits *atomic.Int32) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		hits.Add(1)
		if r.URL.Path == "/hang-up" {
			hangUp(w)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
}

// daemonClient is the client production dials with.
func daemonClient(t *testing.T) *http.Client {
	t.Helper()
	c, err := config.Defaults().HTTPClient(0)
	require.NoError(t, err)
	return c
}

func postTo(t *testing.T, url string, body io.Reader) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, body)
	require.NoError(t, err)
	return req
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(b)
}

func TestDialBackoff_DoublesToACeiling(t *testing.T) {
	for _, tc := range []struct {
		retry int
		want  time.Duration
	}{
		{1, 100 * time.Millisecond},
		{2, 200 * time.Millisecond},
		{3, 400 * time.Millisecond},
		{4, 800 * time.Millisecond},
		{5, time.Second},
		{6, time.Second},
		{1000, time.Second},
	} {
		require.Equal(t, tc.want, dialBackoff(tc.retry), "retry %d", tc.retry)
	}
}

// The restart the retry exists for: refused while the daemon is down, served
// once it listens on the same address again -- and the daemon reads the request
// exactly once.
func TestDialRetry_RefusedUntilTheDaemonIsBack(t *testing.T) {
	addr := deadAddr(t)
	var hits atomic.Int32
	clk := &fakeClock{onWait: func(n int) {
		if n == 2 {
			serveAt(t, addr, counting(&hits))
		}
	}}

	resp, err := clk.policy(time.Minute).do(daemonClient(t), postTo(t, "http://"+addr+"/ok", strings.NewReader("payload")))
	require.NoError(t, err)
	require.Equal(t, "ok", readBody(t, resp))
	require.Equal(t, int32(1), hits.Load())
	require.Equal(t, []time.Duration{100 * time.Millisecond, 200 * time.Millisecond}, clk.recorded(),
		"two refused dials, then served")
}

// Past the budget the request is given up, with an error saying it never left.
// The waits add up to the budget exactly: the last is cut to what was left, and
// the attempt at the deadline is the final one.
func TestDialRetry_GivesUpAtTheBudget(t *testing.T) {
	clk := &fakeClock{}
	_, err := clk.policy(5*time.Second).do(daemonClient(t), postTo(t, "http://"+deadAddr(t), strings.NewReader("x")))
	require.ErrorIs(t, err, errNotSent)
	require.True(t, isDialError(err), "the last attempt's dial error is kept: %v", err)
	require.ErrorContains(t, err, "request not sent after 9 attempts over 5s")
	ms := time.Millisecond
	require.Equal(t, []time.Duration{100 * ms, 200 * ms, 400 * ms, 800 * ms, time.Second, time.Second, time.Second, 500 * ms},
		clk.recorded())
	require.Equal(t, 5*time.Second, clk.waited())
}

// Once a connection existed nothing is re-sent, whatever the failure looks like:
// the daemon read this request before the connection dropped and may have acted
// on it. A reused keep-alive connection is the case net/http has its own retry
// for, and it must not resend a POST either.
func TestDialRetry_NeverResendsOnceConnected(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reused bool
	}{
		{"fresh connection", false},
		{"reused keep-alive connection", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hits atomic.Int32
			srv := httptest.NewServer(counting(&hits))
			t.Cleanup(srv.Close)
			client := daemonClient(t)
			clk := &fakeClock{}

			if tc.reused {
				resp, err := clk.policy(time.Minute).do(client, postTo(t, srv.URL+"/ok", strings.NewReader("warm")))
				require.NoError(t, err)
				require.Equal(t, "ok", readBody(t, resp))
				hits.Store(0)
			}
			_, err := clk.policy(time.Minute).do(client, postTo(t, srv.URL+"/hang-up", strings.NewReader("x")))
			require.Error(t, err)
			require.NotErrorIs(t, err, errNotSent, "the request reached the daemon")
			require.Empty(t, clk.recorded())
			require.Equal(t, int32(1), hits.Load(), "the daemon must see the request once")
		})
	}
}

// A body that cannot be rewound is sent at most once: a retry would have
// nothing to send.
func TestDialRetry_NeedsARewindableBody(t *testing.T) {
	req := postTo(t, "http://"+deadAddr(t), io.MultiReader(strings.NewReader("x")))
	require.Nil(t, req.GetBody, "precondition: net/http cannot rewind this body")
	clk := &fakeClock{}
	_, err := clk.policy(time.Minute).do(daemonClient(t), req)
	require.ErrorIs(t, err, errNotSent)
	require.Empty(t, clk.recorded())
}

// A failure before any connection that is not the dialer's -- a certificate the
// client does not trust -- sent nothing, and is not retried either: it is a
// configuration fault no retry clears.
func TestDialRetry_DoesNotRetryATLSFailure(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewUnstartedServer(counting(&hits))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // the refused handshake is the point
	srv.StartTLS()
	t.Cleanup(srv.Close)

	clk := &fakeClock{}
	_, err := clk.policy(time.Minute).do(daemonClient(t), postTo(t, srv.URL+"/ok", strings.NewReader("x")))
	require.ErrorIs(t, err, errNotSent)
	require.False(t, isDialError(err), "%v", err)
	require.Empty(t, clk.recorded())
	require.Zero(t, hits.Load())
}

// Cancelling the request's context ends a retry wait at once, not at the next
// tick of the backoff.
func TestDialRetry_CancelEndsTheWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+deadAddr(t), strings.NewReader("x"))
	require.NoError(t, err)

	r := newDialRetry(time.Hour)
	r.backoff = func(int) time.Duration { return time.Hour }
	waiting := make(chan struct{})
	var once sync.Once
	r.wait = func(ctx context.Context, d time.Duration) error {
		once.Do(func() { close(waiting) })
		return waitCtx(ctx, d)
	}
	done := make(chan error, 1)
	go func() {
		_, err := r.do(daemonClient(t), req)
		done <- err
	}()

	select {
	case <-waiting:
	case err := <-done:
		t.Fatalf("returned before waiting to retry: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the refused dial was never retried")
	}
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
		require.ErrorIs(t, err, errNotSent)
	case <-time.After(10 * time.Second):
		t.Fatal("cancel did not end the retry wait")
	}
}

func TestWaitCtx(t *testing.T) {
	require.NoError(t, waitCtx(context.Background(), time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, waitCtx(ctx, time.Hour), context.Canceled)
}

// The type decides, never the text: a refused connect reads
// "connect: connection refused" on Unix and "connectex: ..." on Windows.
func TestIsDialError(t *testing.T) {
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"refused dial", refused, true},
		{"as http.Client returns it", &url.Error{Op: "Post", URL: "http://127.0.0.1:8081/api/mcp", Err: refused}, true},
		{"dial to a proxy", &net.OpError{Op: "proxyconnect", Net: "tcp", Err: refused}, true},
		{"read on a live connection", &net.OpError{Op: "read", Net: "tcp", Err: io.EOF}, false},
		{"peer hung up", &url.Error{Op: "Post", URL: "http://127.0.0.1:8081/api/mcp", Err: io.EOF}, false},
		{"no error", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, isDialError(tc.err))
		})
	}
}
