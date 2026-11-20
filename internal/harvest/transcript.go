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

// TranscriptSessionParam is the upload's query key for the agent client's own
// session id (the hook body's session_id, which the upload has no body for).
const TranscriptSessionParam = "session_id"

// MaxTranscriptBytes is the largest transcript a copy keeps, uncompressed. Real
// transcripts reach tens of megabytes; one past this is refused whole, its
// copy recorded as too large, never cut short.
const MaxTranscriptBytes int64 = 256 << 20
