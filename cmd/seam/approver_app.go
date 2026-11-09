package main

// The Touch ID approver app (plan step T) as the seam CLI sees it: where
// `seam touchid enroll` installs it, and how the mcp-proxy bridge launches it
// when the daemon asks (core.ApproverNotification, plan step E).
//
// The app is opened through LaunchServices (open -n -a), never run as a child
// of the bridge: a process started under tmux, or under an agent with no
// terminal, may not reach the login session's Touch ID prompt, and an app that
// LaunchServices starts always does.
//
// Nothing here is trusted by the approval. The helper takes the socket path and
// the request id from its arguments, but reads the request's facts and
// challenge from the daemon itself and refuses a socket whose peer is not the
// daemon's account; the key it signs with is one only the signed helper can
// use. A wrong argument therefore yields a refusal, or a truthful prompt the
// owner may decline. What the launch does check is the app itself, right before
// open (verifyApproverForLaunch): a substitute under ~/Applications could
// capture an enrollment code or phish a password before any of that applies.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
)

// approverAppName is the app bundle's name under ~/Applications.
const approverAppName = "Seamless Approve.app"

// approverAppPath is where seam touchid enroll installs the app for the
// account whose home is home.
func approverAppPath(home string) string {
	return filepath.Join(home, "Applications", approverAppName)
}

// The shapes the launch's arguments must have. They come from a daemon
// notification and become a program's arguments: an id or a duration that
// began with "-" would be read by the helper as a flag.
var (
	approverRequestID = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)
	approverDuration  = regexp.MustCompile(`^[0-9A-Za-z]{1,16}$`)
)

// errNoApproverSocket is a launch from a bridge whose server_url does not name
// the peer_auth socket: the helper talks to the daemon only through it.
var errNoApproverSocket = errors.New("the Touch ID approver needs server_url to name the peer_auth socket (unix:///path)")

// approverApproveArgs is the helper's command line for one approval: the
// socket to read the request from and answer on, the request, and the
// duration the owner chose in the dialog.
func approverApproveArgs(socket, requestID, duration string) []string {
	return []string{"approve", "--socket", socket, "--request", requestID, "--for", duration}
}

// approverRunner runs a command line; exec.Command in production, a fake in
// tests. It matches env.exec.
type approverRunner func(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) (int, error)

// launchApprover opens the Touch ID helper at app for one approval, through
// LaunchServices, and returns once open has handed it over (open does not wait
// for the app). goos is the platform: the helper exists only on macOS. self is
// this seam binary, whose Developer ID team the app is pinned to.
func launchApprover(ctx context.Context, run approverRunner, self func() (string, error), goos, app, socket, requestID, duration string) error {
	switch {
	case goos != "darwin":
		return errors.New("the Touch ID approver exists only on macOS")
	case socket == "":
		return errNoApproverSocket
	case !approverRequestID.MatchString(requestID):
		return fmt.Errorf("refusing to launch the Touch ID approver for %q, which is not a request id", requestID)
	case !approverDuration.MatchString(duration):
		return fmt.Errorf("refusing to launch the Touch ID approver for duration %q", duration)
	case !filepath.IsAbs(socket):
		return fmt.Errorf("the peer_auth socket %q is not an absolute path", socket)
	}
	if err := verifyApproverForLaunch(ctx, run, self, app); err != nil {
		return err
	}
	argv := append([]string{"open", "-n", "-a", app, "--args"}, approverApproveArgs(socket, requestID, duration)...)
	code, err := run(ctx, argv, nil, io.Discard, io.Discard)
	if err != nil {
		return fmt.Errorf("open the Touch ID approver: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("open the Touch ID approver at %s: exit status %d (is it installed? seam touchid enroll)", app, code)
	}
	return nil
}
