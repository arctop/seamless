package harvest

// The mid-session usage push (the Diary's, peer_auth): a live Claude Code
// session's usage ledger rows, while the daemon may not read the transcript
// itself. A Claude Code session reports its usage at SessionEnd, and sessions
// stay open for hours or days, so while the Diary is on the daemon keeps the
// ledger current from UserPromptSubmit, the hook Claude Code fires on every
// prompt (internal/hooks usage_live.go).
//
// The daemon decides when: only while the Diary is on, for the caller's own
// conversation, at most once per the daemon's own throttle. Where it may read
// the transcript it does so itself, after answering. On the peer_auth socket
// its answer to the prompt carries UsageWantedHeader instead, and `seam hook`
// -- the owner's account, beside the agent -- hands the read to a detached
// `seam hook-usage`, which reads the transcript the prompt's payload named
// with ClaudeUsage and posts the rows to UsageEndpoint: a hook-shaped body
// naming the agent client's session_id, the push under Field (Version, Tokens
// and Usage, validated as a SessionEnd push is). The answer is a
// UsagePushAnswer. Only an answer that asked carries the header, so the
// client needs no probe and keeps no state, and the prompt's own answer waits
// for none of it.
//
// The format is additive, like the push's.

import "time"

// UsageWantedHeader is the response header a daemon sets on a UserPromptSubmit
// answer that asks for the session's usage (see the file header). Its value
// is UsageWanted; a client reads any other value as no ask.
const UsageWantedHeader = "X-Seamless-Usage-Wanted"

// UsageWanted is UsageWantedHeader's value.
const UsageWanted = "1"

// UsageEndpoint is the route the mid-session usage push posts to.
const UsageEndpoint = "/api/hooks/usage"

// UsagePushTimeout bounds one mid-session usage push: the detached sender
// reads the transcript and sends the rows within it, and gives up at it. The
// daemon keeps its ask open for at least as long.
const UsagePushTimeout = time.Minute

// The words a UsagePushAnswer's Result takes.
const (
	// UsageWritten: the conversation's ledger now holds the rows pushed.
	UsageWritten = "written"
	// UsageUnchanged: the ledger already held exactly those rows.
	UsageUnchanged = "unchanged"
	// UsageBehind: the rows count fewer messages than the ledger holds -- a
	// later harvest landed first -- so nothing is written.
	UsageBehind = "behind"
)

// UsagePushAnswer is the daemon's answer to a mid-session usage push.
type UsagePushAnswer struct {
	Result string `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}
