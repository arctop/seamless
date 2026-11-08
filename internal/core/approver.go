package core

// The in-session approver contract (plan step E with step T). It lives in core
// because both binaries need it and the seam CLI must not import the approval
// service (which would pull the database into the CLI).
//
// When the owner accepts an access request's dialog and chooses Touch ID, the
// daemon asks the agent's `seam mcp-proxy` bridge to launch the Touch ID
// helper for that request. The daemon runs as its own account with no GUI
// session, so it cannot raise the prompt itself; the bridge runs as the owner,
// in the login session, and is in every locked call (a lock applies only on
// the peer_auth socket, which only the bridge dials for an MCP client).
//
// The ask is a notification on the tool call's own SSE reply, which the bridge
// swallows: it never reaches the agent's client. Launching the helper grants
// nothing. The helper reads the request's facts and challenge from the daemon
// itself, verifies the socket belongs to the daemon's account, shows them in
// the Touch ID prompt, and signs approval.TouchIDApproveMessage with a key only
// it can use; the approve core verifies that signature. So an agent that sends
// or fakes this notification, or launches the helper itself, gains nothing but
// a truthful prompt the owner may decline.

// ApproverNotification is the method of the daemon's ask to launch the Touch
// ID helper. Both binaries pin it in a test.
const ApproverNotification = "notifications/seamless/approver"

// ApproverLaunch is ApproverNotification's params: the request to approve and
// the duration the owner chose in the dialog (one of the durations the daemon
// offers: "session", "1h", "4h"). The helper signs over the duration and says
// it in the prompt, so the owner confirms it with the touch.
type ApproverLaunch struct {
	RequestID string `json:"request_id"`
	Duration  string `json:"duration"`
}

// ApproverEndedNotification is the bridge's word to the daemon that the Touch
// ID helper it launched for a request has exited (cmd/seam sends it on its
// own MCP session; internal/mcp's dialog takes it). Its params are the
// request_id and the outcome: ApproverEndedCancelled when the helper's last
// line says the owner cancelled, ApproverEndedClosed for any other exit -- a
// failure, or the approval itself, which the request's row then shows. It
// grants and denies nothing: all it does is end a dialog's wait early, which
// the agent could do anyway by cancelling its own call. Both binaries pin the
// method in a test.
const ApproverEndedNotification = "notifications/seamless/approver_ended"

// The outcomes ApproverEndedNotification reports.
const (
	ApproverEndedCancelled = "cancelled"
	ApproverEndedClosed    = "closed"
)
