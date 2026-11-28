package harvest

// Owed usage under peer_auth. A Claude Code conversation reports its whole
// usage at its SessionEnd, and that hook may come late or never (the Claude
// app keeps a session open until it is archived or the app quits, and runs no
// SessionEnd for one it stops). Once the agent closed the conversation's
// session with session_end, or the idle reaper expired it, the daemon holds
// its usage as owed (internal/store usage_owed.go) until the transcript is
// read. With peer_auth off the daemon reads it itself, at its upkeep. On the
// peer_auth socket it opens no file a hook named, so its answer to a later
// hook of the same caller -- the same agent client, account and computer --
// carries UsageOwedHeader, naming the conversation and the transcript its
// hooks recorded, and `seam hook` hands the read to a detached
// `seam hook-usage --owed`. That reads the path only inside the agent
// client's own transcript folder, as a pending transcript is read, and posts
// to UsageEndpoint the mid-session push's body with UsageOwedField set, the
// transcript's latest change under UsageChangedField -- or, instead of the
// rows, why it did not read the path under UsageRefusedField. The answer is a
// UsagePushAnswer.
//
// The format is additive, like the push's: an older `seam` ignores the header,
// and after the daemon's ask limit it stops asking.

import (
	"os"
	"time"
)

// UsageOwedHeader is the response header that asks the client for an owed
// conversation's usage (see the file header). Each value is one conversation,
// URL-query-encoded as a TranscriptPendingHeader value is:
// TranscriptSessionParam, the agent client's own session id, and
// TranscriptPathParam, the transcript path its hooks recorded. An answer
// carries at most MaxPendingPerAnswer values.
const UsageOwedHeader = "X-Seamless-Usage-Owed"

// UsageOwedField is the usage push body's field (true) that makes it the
// answer to a UsageOwedHeader ask rather than to a prompt's
// UsageWantedHeader.
const UsageOwedField = "owed"

// UsageChangedField is an owed push's field for the transcript's latest
// modification, its subagents' transcripts included (ClaudeTranscriptChanged),
// in RFC 3339 with nanoseconds: the daemon keeps asking until it is old
// enough that nothing more is coming.
const UsageChangedField = "changed_at"

// UsageRefusedField is an owed push's field, sent instead of the rows, saying
// why the client did not read the path it was asked about:
// TranscriptRefusedPath or TranscriptRefusedMissing. The asks end with it.
const UsageRefusedField = "refused"

// UsageSettled is a UsagePushAnswer Result: the owed conversation's asks
// ended with the report, and nothing was written.
const UsageSettled = "settled"

// ClaudeTranscriptChanged is when a Claude Code conversation's transcript last
// changed: the latest modification of the transcript and of the subagent
// transcripts beside it (SubagentTranscripts), which a turn waiting on a
// subagent writes while the main transcript waits. ok is false when path is
// not a regular file now: a named pipe would block whoever opened it.
func ClaudeTranscriptChanged(path string) (changed time.Time, ok bool) {
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return time.Time{}, false
	}
	changed = fi.ModTime()
	for _, sub := range SubagentTranscripts(path) {
		if at := sub.Info.ModTime(); at.After(changed) {
			changed = at
		}
	}
	return changed.UTC(), true
}
