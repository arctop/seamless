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

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"time"

	"github.com/arctop/seamless/internal/core"
)

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
}

// launch opens the approver for one ask.
func (h approverHost) launch(ctx context.Context, ask core.ApproverLaunch) error {
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
		return fmt.Errorf("find the home the Touch ID approver is installed under: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, approverLaunchTimeout)
	defer cancel()
	return launchApprover(ctx, run, goos, approverAppPath(home), h.socket, ask.RequestID, ask.Duration)
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
		if err := b.approver.launch(ctx, ask); err != nil {
			b.logf("seam mcp-proxy: %s: %v", core.ApproverNotification, err)
		}
		b.mu.Lock()
		delete(b.launching, ask.RequestID)
		b.mu.Unlock()
	}()
}
