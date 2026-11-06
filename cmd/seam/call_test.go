package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/agentproc"
	"github.com/arctop/seamless/internal/config"
)

// noDialEnv is the fixture's env with a dial that fails the test: an argument
// refusal must happen before a connection is opened.
func noDialEnv(t *testing.T, fx *daemonFixture) (*env, func() string, func() string) {
	t.Helper()
	e, out, errb := fx.env()
	e.dial = func(context.Context) (*mcpclient.Client, config.Config, error) {
		t.Fatal("dialed before the arguments were checked")
		return nil, config.Config{}, nil
	}
	return e, out.String, errb.String
}

// The result text is printed exactly as the daemon returned it, and the
// arguments reach the tool exactly as typed.
func TestCall_PrintsTheRawResult(t *testing.T) {
	fx := newGoldenFixture(t)
	stdout, stderr, code := fx.run(t, "call", "recall", `{"query":"lease grace","scope":"all","limit":50}`)
	require.Equal(t, 0, code, stderr)
	require.Equal(t, fxRecall+"\n", stdout)

	got := fx.lastCall(t)
	require.Equal(t, "recall", got.name)
	require.Equal(t, map[string]any{"query": "lease grace", "scope": "all", "limit": float64(50)}, got.args)
}

// No arguments means an empty object, sent as one -- not null, and not omitted.
func TestCall_NoArgumentsIsAnEmptyObject(t *testing.T) {
	fx := newGoldenFixture(t)
	stdout, stderr, code := fx.run(t, "call", "usage_summary")
	require.Equal(t, 0, code, stderr)
	require.Equal(t, fxUsage+"\n", stdout)
	require.Contains(t, string(fx.lastCall(t).params), `"arguments":{}`)
}

// "-" reads the arguments from stdin.
func TestCall_ReadsArgumentsFromStdin(t *testing.T) {
	fx := newGoldenFixture(t)
	e, out, errb := fx.env()
	e.stdin = strings.NewReader("  {\"name\": \"lease-steal-window\"}\n")
	require.Equal(t, 0, dispatch(context.Background(), e, []string{"call", "memory_read", "-"}), errb.String())
	require.Equal(t, fxMemoryRead+"\n", out.String())
	require.Equal(t, map[string]any{"name": "lease-steal-window"}, fx.lastCall(t).args)
}

// An integer keeps the digits it was written with: a float64 round trip would
// hand the daemon a different number than the one typed.
func TestCall_NumbersTravelAsWritten(t *testing.T) {
	fx := newGoldenFixture(t)
	_, stderr, code := fx.run(t, "call", "recall", `{"query":"x","limit":12345678901234567890}`)
	require.Equal(t, 0, code, stderr)
	require.Contains(t, string(fx.lastCall(t).params), `12345678901234567890`)
}

// Arguments that are not exactly one JSON object are refused before dialing,
// with the reason; nothing is defaulted to {}.
func TestCall_RefusesArgumentsThatAreNotOneObject(t *testing.T) {
	fx := newGoldenFixture(t)
	for _, tc := range []struct {
		name  string
		arg   string
		stdin string
		want  string
	}{
		{"bad json", `{"query":`, "", "arguments are not valid JSON"},
		{"not json at all", `query=lease`, "", "arguments are not valid JSON"},
		{"array", `[{"query":"x"}]`, "", "arguments must be a JSON object, not an array"},
		{"string", `"lease"`, "", "arguments must be a JSON object, not a string"},
		{"number", `7`, "", "arguments must be a JSON object, not a number"},
		{"boolean", `true`, "", "arguments must be a JSON object, not a boolean"},
		{"null", `null`, "", "arguments must be a JSON object, not null"},
		{"two objects", `{} {}`, "", "arguments must be one JSON object, but more follows it"},
		{"trailing text", `{"query":"x"} junk`, "", "arguments must be one JSON object, but more follows it"},
		{"empty", ``, "", "arguments are empty"},
		{"empty stdin", `-`, "", "arguments on stdin are empty"},
		{"stdin array", `-`, `[]`, "arguments on stdin must be a JSON object, not an array"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, out, errb := noDialEnv(t, fx)
			e.stdin = strings.NewReader(tc.stdin)
			require.Equal(t, 1, dispatch(context.Background(), e, []string{"call", "recall", tc.arg}))
			require.Empty(t, out())
			require.Contains(t, errb(), tc.want)
		})
	}
	require.Zero(t, fx.callCount(), "no refused call reached the daemon")
}

// A tool error result exits non-zero with the tool's own text on stderr, and
// nothing on stdout for a pipe to mistake for an answer.
func TestCall_ToolErrorExitsNonZero(t *testing.T) {
	fx := newGoldenFixture(t)
	stdout, stderr, code := fx.run(t, "call", "tasks_release", `{"id":"01KDDDDDDDDDDDDDDDDDDDDDDD"}`)
	require.Equal(t, 1, code)
	require.Empty(t, stdout)
	require.Contains(t, stderr, "tasks_release: task 01KDDDDD is not claimed by this session")
}

// A tool the daemon does not serve is the daemon's refusal, named on stderr.
// The CLI does not gate names on its own list: a newer daemon may serve tools
// this binary predates.
func TestCall_UnknownToolIsRefusedByTheDaemon(t *testing.T) {
	fx := newGoldenFixture(t)
	stdout, stderr, code := fx.run(t, "call", "no_such_tool")
	require.Equal(t, 1, code)
	require.Empty(t, stdout)
	require.Contains(t, stderr, "no_such_tool")
	require.Contains(t, stderr, "not found")

	stdout, stderr, code = fx.run(t, "call", "")
	require.Equal(t, 1, code)
	require.Empty(t, stdout)
	require.Contains(t, stderr, "tool name is empty")
}

// The positional contract is the parser's: a tool is required, and arguments are
// one positional (quote the object). Both are usage errors, exit 2.
func TestCall_Arity(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"call"}, "missing argument: seam call requires <tool> [<json>]"},
		{[]string{"call", "recall", "{", "}"}, `unexpected argument "}"`},
	} {
		e, _, errb := stubEnv()
		require.Equal(t, 2, dispatch(context.Background(), e, tc.argv), "%v", tc.argv)
		require.Contains(t, errb.String(), tc.want)
		require.Contains(t, errb.String(), `arguments are one JSON object, inline or "-" for stdin`)
	}
}

// call dials exactly as every other command does: the bearer key, the machine,
// and the agent process that binds the call to its agent's session.
func TestCall_SendsTheSameHeadersAsEveryCommand(t *testing.T) {
	fx := newGoldenFixture(t)
	_, stderr, code := fx.run(t, "call", "usage_summary")
	require.Equal(t, 0, code, stderr)
	viaCall := fx.lastCall(t).header

	_, stderr, code = fx.run(t, "usage")
	require.Equal(t, 0, code, stderr)
	viaUsage := fx.lastCall(t).header

	require.Equal(t, "Bearer fixture-key", viaCall.Get("Authorization"))
	for _, h := range []string{"Authorization", hostHeader, agentProcessHeader} {
		require.Equal(t, viaUsage.Get(h), viaCall.Get(h), h)
	}
	if host := config.Hostname(); host != "" {
		require.Equal(t, host, viaCall.Get(hostHeader))
	}
	if proc, ok := agentproc.Anchor(); ok {
		require.Equal(t, proc, viaCall.Get(agentProcessHeader))
	}
}

// With server_url naming a Unix socket (the peer_auth client), call goes over
// that socket like every other command, because it dials through the same
// constructor.
func TestCall_DialsThePeerAuthSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the peer_auth socket is a Unix deployment")
	}
	fx := newGoldenFixture(t)
	// A short directory: a socket path is capped near 104 bytes on macOS, and
	// t.TempDir() there is longer than that allows.
	dir, err := os.MkdirTemp("", "seam")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	srv := &http.Server{Handler: fx.mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	e, out, errb := stubEnv()
	cfg := config.Defaults()
	cfg.Addr = "127.0.0.1:1" // nothing listens: only the socket can answer
	cfg.AdvertisedURL = "unix://" + sock
	e.dial = func(ctx context.Context) (*mcpclient.Client, config.Config, error) { return dialWith(ctx, cfg) }

	require.Equal(t, 0, dispatch(context.Background(), e, []string{"call", "usage_summary"}), errb.String())
	require.Equal(t, fxUsage+"\n", out.String())
}
