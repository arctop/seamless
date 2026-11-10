package main

// The injectable world a command handler runs against: everything it touches that
// a test cannot supply for itself. Handlers take *env rather than reaching for
// os.Stdout / dial / config.Load directly, so their output and their server can
// both be substituted.
//
// parse deliberately takes none of this. Argument handling must be testable
// without a network or a config file, which is why the parse/execute split is
// also the exit-code boundary (see spec.go).

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"golang.org/x/term"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
)

// env is the world a handler is given.
type env struct {
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer

	// dial mirrors the package-level dial (main.go): an initialized MCP client
	// plus the config it was built from, so a handler that needs both does not
	// load config twice.
	dial func(context.Context) (*mcpclient.Client, config.Config, error)

	// loadConfig mirrors config.Load, for the commands that speak to the console
	// JSON surface (client.go) and need the address and key without an MCP
	// session.
	loadConfig func() (config.Config, error)

	// The rest serves seam approve, the owner's terminal path, and each is nil
	// in a test env unless the test sets it. A nil terminal, getenv or exec
	// degrades to the safe answer, never to the real world: not a terminal, an
	// empty environment, and no program run at all -- so a test that forgets to
	// fake sudo fails rather than running it. A nil dialDaemon, daemonAccount or
	// lookupAccount refuses, so no daemon is believed unchecked. A nil now or
	// tick is the real clock, which runs nothing.

	// terminal reports whether stdin and stdout are both terminals.
	terminal func() bool
	// exec runs a program (sudo, the browser opener, the notifier).
	exec execFunc
	// getenv reads the environment (NO_COLOR, TERM).
	getenv func(string) string
	// goos picks the browser opener and the notifier; "" is runtime.GOOS.
	goos string
	// now is the clock the facts' "waits until" is measured against.
	now func() time.Time
	// tick paces seam approve --watch: a channel that fires every d, and its stop.
	tick func(d time.Duration) (<-chan time.Time, func())
	// dialDaemon asks the kernel who answers at cfg's server_url: the peer's
	// uid and a client to read from it (daemon_peer.go). It refuses a server_url
	// that is not a socket, with errServerNotSocket.
	dialDaemon func(ctx context.Context, cfg config.Config) (uint32, *http.Client, error)
	// lookupAccount resolves an OS account's name to its uid.
	lookupAccount func(name string) (uint32, error)
	// daemonAccount reads the pin root writes of the daemon's account
	// (access.DaemonAccount): present false when there is none.
	daemonAccount func() (pin access.DaemonPin, present bool, err error)
}

// newEnv returns the real world.
func newEnv() *env {
	return &env{
		stdin:      os.Stdin,
		stdout:     os.Stdout,
		stderr:     os.Stderr,
		dial:       dial,
		loadConfig: config.Load,
		terminal:   stdioIsTerminal,
		exec:       runProgram,
		getenv:     os.Getenv,

		dialDaemon:    dialSocketDaemon,
		lookupAccount: lookupAccountUID,
		daemonAccount: access.DaemonAccount,
	}
}

// execFunc runs argv[0] with argv[1:] and the given stdio (a nil stdin reads
// nothing), returning the program's exit code, or an error when it could not be
// started at all.
type execFunc func(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) (int, error)

// errNoRunner is a test env that runs no programs.
var errNoRunner = errors.New("no program runner in this environment")

// stdioIsTerminal reports whether stdin and stdout are both terminals. It asks
// the terminal driver (x/term) rather than checking for a character device,
// because /dev/null is one: `seam approve </dev/null >/dev/null` must not pass
// for the owner's terminal.
func stdioIsTerminal() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

// runProgram is the real execFunc.
func runProgram(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if len(argv) == 0 {
		return -1, errors.New("no program to run")
	}
	c := exec.CommandContext(ctx, argv[0], argv[1:]...)
	c.Stdin, c.Stdout, c.Stderr = stdin, stdout, stderr
	err := c.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}

// isTerminal is e.terminal, false when unset.
func (e *env) isTerminal() bool {
	return e.terminal != nil && e.terminal()
}

// run is e.exec, refusing when unset.
func (e *env) run(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if e.exec == nil {
		return -1, errNoRunner
	}
	return e.exec(ctx, argv, stdin, stdout, stderr)
}

// envVar reads one environment variable, "" when e.getenv is unset.
func (e *env) envVar(key string) string {
	if e.getenv == nil {
		return ""
	}
	return e.getenv(key)
}

// platform names the OS, runtime.GOOS unless a test set it.
func (e *env) platform() string {
	if e.goos != "" {
		return e.goos
	}
	return runtime.GOOS
}

// clock is e.now, time.Now when unset.
func (e *env) clock() time.Time {
	if e.now != nil {
		return e.now()
	}
	return time.Now()
}

// ticker is e.tick, a real ticker when unset.
func (e *env) ticker(d time.Duration) (<-chan time.Time, func()) {
	if e.tick != nil {
		return e.tick(d)
	}
	t := time.NewTicker(d)
	return t.C, t.Stop
}

// The HTTP client every command here dials with is config.Config.HTTPClient,
// not a constructor of this CLI's own. It moved to internal/config so that
// seamlessd's client-role surfaces -- install-hooks reading the server's feature
// state, the client-role doctor's live tools/list count -- share the ONE TLS
// trust decision rather than reimplementing it: an https server_url with a
// private CA either verifies everywhere or the install half-works in a way that
// reads as an intermittent network fault. config.DialTimeout is the connection
// deadline it applies; the argument to HTTPClient is the whole-request one
// (0 = none: the mcp-proxy bridge, whose tool calls can be LLM-slow, and seam
// hook, whose one deadline rides on its ctx so a dial retry cannot restart it).

// healthTimeout bounds a /healthz probe: it pings SQLite and returns, so a
// slower answer is a wedged daemon, not a busy one.
const healthTimeout = 3 * time.Second
