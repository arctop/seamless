package harvest

// The pushed transcript (peer_auth): a flagged session's transcript copy when
// the daemon may not read the file itself. Like every pushed capture it is
// made by `seam hook` beside the agent (cmd/seam hook_capture.go), and only
// when server_url names the peer_auth socket; the daemon takes it only on a
// request that carries a peer_auth View (internal/hooks transcript.go).
//
// It is not a field of the hook body (Field): a transcript runs to hundreds of
// megabytes, past every hook body's cap, so it rides a request of its own.
// What tells the client to send one is the daemon's answer to the session's
// end signal (SessionEnd for Claude Code, each Stop for Codex): the
// TranscriptWantedHeader appears on that answer only for a flagged session,
// so no probe request, no field in a default answer and no state kept between
// hooks is needed to learn it. The client strips nothing from what it prints:
// the header never reaches the agent client.
//
// A flagged session whose end signal never sent it (a crash, a closed
// terminal, a flag set after the end) is pending: the same caller's later
// answers -- SessionStart, SessionEnd, Codex's Stop -- carry
// TranscriptPendingHeader, naming the session and the path its hooks
// recorded, and only while that caller has one. The client reads the path
// only when it is a regular *.jsonl file inside its agent client's own
// transcript folder, and otherwise reports it (TranscriptRefusedParam).
//
// The upload outlives the hook: `seam hook` hands it to a detached process of
// its own, which has TranscriptUploadTimeout to send it, so the hook returns
// at once.

import (
	"net/url"
	"time"
)

// TranscriptWantedHeader is the response header a daemon sets on a SessionEnd
// or Stop answer whose session is flagged and wants its transcript sent. Its
// value is a decimal byte count: send the transcript only when the file is
// larger than that (the copy already kept is that size; "0" asks for it
// whatever its size).
const TranscriptWantedHeader = "X-Seamless-Transcript-Wanted"

// TranscriptSizeHeader is the request header the client's upload names the
// transcript's size in, as it read it, so a transcript over
// MaxTranscriptBytes is refused before any of it is sent. It is the client's
// claim: the daemon counts what arrives.
const TranscriptSizeHeader = "X-Seamless-Transcript-Bytes"

// TranscriptEndpoint is the hook route the upload posts to: the gzip-compressed
// transcript as the body, the client and session id in the query, as every
// hook names them.
const TranscriptEndpoint = "/api/hooks/transcript"

// TranscriptKeepPath is the owner's route that keeps flagged session id's
// transcript by hand (`seam sessions <id> --keep-transcript`): a POST,
// keyed by the session's ULID.
func TranscriptKeepPath(id string) string { return TranscriptEndpoint + "/" + url.PathEscape(id) }

// TranscriptFromDaemonParam is the owner route's query flag (value "1")
// asking the daemon to read the recorded path itself, as it may with
// peer_auth off. Without it, under peer_auth, the answer names the path, the
// computer and the agent client for the owner's own account to read and send.
const TranscriptFromDaemonParam = "from_daemon"

// TranscriptSessionParam is the upload's query key for the agent client's own
// session id (the hook body's session_id, which the upload has no body for).
// It is also the session's key in a TranscriptPendingHeader value.
const TranscriptSessionParam = "session_id"

// TranscriptPendingHeader is the response header that asks the client for a
// pending flagged session's transcript (see the file header). Each value is
// one session, URL-query-encoded: TranscriptSessionParam, the agent client's
// own session id, and TranscriptPathParam, the transcript path its hooks
// recorded. An answer carries at most MaxPendingPerAnswer values.
const TranscriptPendingHeader = "X-Seamless-Transcript-Pending"

// TranscriptPathParam is a TranscriptPendingHeader value's key for the
// recorded transcript path.
const TranscriptPathParam = "path"

// MaxPendingPerAnswer bounds the pending sessions one answer asks for.
const MaxPendingPerAnswer = 3

// TranscriptRefusedParam is the upload route's query key by which the client
// reports, instead of sending a body, why it did not read a path a
// TranscriptPendingHeader named: TranscriptRefusedPath or
// TranscriptRefusedMissing.
const TranscriptRefusedParam = "refused"

// The TranscriptRefusedParam values.
const (
	// TranscriptRefusedPath: the path is not a regular *.jsonl file inside the
	// agent client's own transcript folder (a symlink, another folder, another
	// kind of file).
	TranscriptRefusedPath = "path"
	// TranscriptRefusedMissing: the file is not there, or could not be opened.
	TranscriptRefusedMissing = "missing"
)

// TranscriptUploadTimeout bounds one transcript upload: the client's detached
// sender gives up at it, and the daemon's upload route reads the body for no
// longer. A local socket moves the 256 MB cap in seconds; a relay over a slow
// link may need minutes.
const TranscriptUploadTimeout = 30 * time.Minute

// MaxTranscriptBytes is the largest transcript a copy keeps, uncompressed. Real
// transcripts reach tens of megabytes; one past this is refused whole, its
// copy recorded as too large, never cut short.
const MaxTranscriptBytes int64 = 256 << 20
