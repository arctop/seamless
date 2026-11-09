package main

// The bridge's half of in-session approval (plan step E): when the owner
// accepts an access request's dialog and chooses Touch ID, the daemon asks the
// agent's bridge to launch the Touch ID approver (core.ApproverNotification),
// because the daemon runs as its own account with no GUI session and the
// bridge runs as the owner, in the login session.
//
// The ask rides the tool call's own SSE reply. The bridge swallows it -- the
// agent's client never sees it -- and opens the helper on a side goroutine, so
// the call's reply is relayed while the owner touches the sensor. Launching
// grants nothing (core/approver.go says why), so the bridge applies no trust
// of its own beyond launchApprover's argument checks: the helper is given the
// request id and duration those checks passed and the socket from this
// bridge's own config, and nothing else.
//
// When the helper exits, the bridge tells the daemon, on this connection's
// MCP session, so a call that waits on the approval can answer at once rather
// than wait out its dialog: core.ApproverEndedNotification, a notification
// with the request_id and an outcome -- cancelled when the helper's last word
// is that the owner cancelled ("Not approved: ...", pinned by the app's
// FlowTests too), closed for any other end (it failed, it was killed before it
// said anything, or it approved, which the daemon reads from the request
// itself).
// The helper's exit status never reaches the bridge through LaunchServices;
// its stderr does (mcpproxy_approver_fifo.go). A helper the bridge never saw
// holding that stderr is not reported, and the call waits as before. The word
// grants and denies nothing.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/arctop/seamless/internal/core"
)

// approverWatchLimit bounds how long the bridge watches one helper: a
// request waits at most 15 minutes for the owner (approval.RequestTTL), so
// past that nothing is waiting on the word.
const approverWatchLimit = 15 * time.Minute

// The helper's last line on stderr is "SeamApprove: " and its outcome's
// message (macos/SeamApprove main.swift); a cancelled approval's message
// begins "Not approved:" (Flows.approve). Its exit status (2 for a cancel)
// does not survive LaunchServices, so this line is how a cancel is told.
const (
	approverLinePrefix   = "SeamApprove: "
	approverCancelPrefix = "Not approved:"
)

// approverOutcome reads how a helper that has ended ended, from its stderr:
// cancelled when its last word says the owner cancelled, closed otherwise,
// including when it was killed before it said anything.
func approverOutcome(stderr []byte) string {
	last := ""
	for line := range strings.SplitSeq(string(stderr), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), approverLinePrefix); ok {
			last = rest
		}
	}
	if strings.HasPrefix(last, approverCancelPrefix) {
		return core.ApproverEndedCancelled
	}
	return core.ApproverEndedClosed
}

// approverLaunchTimeout bounds one launch. open returns once LaunchServices
// has the app, well under a second; past this it is wedged, and a bridge whose
// stdin has closed waits for its launches before it exits.
const approverLaunchTimeout = 30 * time.Second

// approverHost is what the bridge launches the Touch ID approver with. Each
// field is injectable for tests; the zero value runs no program.
type approverHost struct {
	run    approverRunner         // runs open; nil runs nothing (errNoRunner)
	goos   string                 // the platform; "" is runtime.GOOS
	home   func() (string, error) // the owner's home, where the app is installed; nil is os.UserHomeDir
	socket string                 // the peer_auth socket this bridge dials; "" when server_url names none
	self   func() (string, error) // this seam binary, whose team the app is pinned to; nil is selfPath
}

// launch opens the approver for one ask, and returns the helper's stderr to
// watch for its end (nil when none could be made: the launch goes ahead
// without one).
func (h approverHost) launch(ctx context.Context, ask core.ApproverLaunch) (*approverStderr, error) {
	run := h.run
	if run == nil {
		run = func(context.Context, []string, io.Reader, io.Writer, io.Writer) (int, error) {
			return -1, errNoRunner
		}
	}
	goos := h.goos
	if goos == "" {
		goos = runtime.GOOS
	}
	homeDir := h.home
	if homeDir == nil {
		homeDir = os.UserHomeDir
	}
	home, err := homeDir()
	if err != nil {
		return nil, fmt.Errorf("find the home the Touch ID approver is installed under: %w", err)
	}
	self := h.self
	if self == nil {
		self = selfPath
	}
	ctx, cancel := context.WithTimeout(ctx, approverLaunchTimeout)
	defer cancel()
	// The FIFO is made only once launchApprover's checks passed and it runs
	// open: its stderr goes there. codesign, run before that, gets none.
	var watch *approverStderr
	withStderr := func(ctx context.Context, argv []string, in io.Reader, out, errw io.Writer) (int, error) {
		if argv[0] == "open" {
			if s, err := openApproverStderr(); err == nil {
				watch = s
				argv = withStderrArg(argv, s.stderrPath())
			}
		}
		return run(ctx, argv, in, out, errw)
	}
	err = launchApprover(ctx, withStderr, self, goos, approverAppPath(home), h.socket, ask.RequestID, ask.Duration)
	if watch != nil {
		watch.handedOver()
		if err != nil {
			watch.close()
			watch = nil
		}
	}
	return watch, err
}

// withStderrArg adds open's --stderr path to an open command line, before
// the arguments it passes to the app.
func withStderrArg(argv []string, path string) []string {
	i := slices.Index(argv, "--args")
	if i < 0 {
		return argv
	}
	return slices.Concat(argv[:i], []string{"--stderr", path}, argv[i:])
}

// takeApproverAsk reports whether msg is the daemon's ask to launch the Touch
// ID approver, which is never relayed to the client, and if so acts on it:
// one launch per request id at a time, and any failure, a malformed ask
// included, reported as one line on stderr.
func (b *bridge) takeApproverAsk(ctx context.Context, msg []byte) bool {
	var m struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if json.Unmarshal(msg, &m) != nil || m.Method != core.ApproverNotification {
		return false
	}
	var ask core.ApproverLaunch
	if err := json.Unmarshal(m.Params, &ask); err != nil {
		b.logf("seam mcp-proxy: %s: malformed params: %v", core.ApproverNotification, err)
		return true
	}
	b.goLaunch(ctx, ask)
	return true
}

// goLaunch starts ask's launch on a side goroutine, unless one for the same
// request is still running (the daemon asks again; the owner sees one prompt)
// or run is returning.
func (b *bridge) goLaunch(ctx context.Context, ask core.ApproverLaunch) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, running := b.launching[ask.RequestID]; running || b.stopped {
		return
	}
	if b.launching == nil {
		b.launching = make(map[string]struct{})
	}
	b.launching[ask.RequestID] = struct{}{}
	b.side.Add(1)
	go func() {
		defer b.side.Done()
		watch, err := b.approver.launch(ctx, ask)
		if err != nil {
			b.logf("seam mcp-proxy: %s: %v", core.ApproverNotification, err)
		}
		b.mu.Lock()
		delete(b.launching, ask.RequestID)
		b.mu.Unlock()
		if watch == nil {
			return
		}
		defer watch.close()
		stderr, ended := watch.wait(b.quit, approverWatchLimit)
		if !ended {
			return
		}
		if err := b.sendApproverEnded(ctx, ask.RequestID, approverOutcome(stderr)); err != nil && ctx.Err() == nil {
			b.logf("seam mcp-proxy: %s: %v", core.ApproverEndedNotification, err)
		}
	}()
}

// sendApproverEnded tells the daemon, on this bridge's MCP session, that the
// helper launched for request id ended with outcome. It is a notification:
// the daemon answers 202 and nothing reaches the client.
func (b *bridge) sendApproverEnded(ctx context.Context, id, outcome string) error {
	frame, err := json.Marshal(map[string]any{
		"jsonrpc": mcp.JSONRPC_VERSION, "method": core.ApproverEndedNotification,
		"params": map[string]string{"request_id": id, "outcome": outcome},
	})
	if err != nil {
		return err
	}
	return b.exchange(ctx, frame, io.Discard)
}
