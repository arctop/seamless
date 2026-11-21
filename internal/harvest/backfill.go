package harvest

// The usage backfill (`seam usage backfill`): the owner fills the usage ledger
// of the sessions whose Claude Code transcripts are still on this computer --
// the days before the ledger existed, and the sessions whose end signal never
// harvested them. Like every client-side capture, the client reads the files
// with the daemon's own parser (ClaudeUsage) and sends what it found; here on
// every transport, because the command asks the daemon first, so no hook-time
// guess is needed. The daemon opens no transcript for it.
//
// Two requests on one route, UsageBackfillEndpoint:
//
//   - GET lists the caller's sessions on the computer the request speaks for
//     (its host query parameter, the one a hook sends): BackfillList.
//   - POST fills one session: a hook-shaped body naming the agent client's
//     session id, the push under Field (Version, Tokens and Usage), and
//     dry_run. The answer is a BackfillResult.
//
// Both are the owner's: a restricted local account is refused before
// anything is read, and a session another principal started is not listed
// and not filled. The format is additive, like the push's.

import (
	"time"

	"github.com/arctop/seamless/internal/core"
)

// UsageBackfillEndpoint is the usage backfill's route (see the file header).
const UsageBackfillEndpoint = "/api/hooks/usage-backfill"

// BackfillList is the daemon's answer to the backfill's listing.
type BackfillList struct {
	// Host is the computer the list is of: the request's host, or the
	// daemon's own when it named none.
	Host string `json:"host"`
	// Sessions are the caller's conversations recorded on Host (their
	// ambient rows), oldest first.
	Sessions []BackfillSession `json:"sessions"`
	// OtherHosts counts the caller's conversations recorded on other
	// computers, whose transcripts are there.
	OtherHosts int `json:"otherHosts,omitempty"`
	// Locked names the locked projects this request does not read, whose
	// sessions the list leaves out: names only, never a count.
	Locked []string `json:"locked,omitempty"`
}

// BackfillSession is one conversation a backfill may fill.
type BackfillSession struct {
	ID      string `json:"id"` // the session's ULID
	Name    string `json:"name"`
	Project string `json:"project"`
	// Client is the agent client that recorded it (claude-code, codex), and
	// SessionID that client's own session id: a Claude Code transcript is
	// named after it.
	Client    string `json:"client"`
	SessionID string `json:"sessionId"`
	Status    string `json:"status"`
	// TranscriptPath is the transcript its hooks recorded ("" before they
	// recorded one).
	TranscriptPath string          `json:"transcriptPath,omitempty"`
	Tokens         core.TokenUsage `json:"tokens"`
	Created        time.Time       `json:"created"`
}

// The words a BackfillResult's Result takes.
const (
	// BackfillWritten: the session's ledger now holds the transcript's rows.
	BackfillWritten = "written"
	// BackfillWouldWrite: a dry run's answer where the real run writes.
	BackfillWouldWrite = "would_write"
	// BackfillUnchanged: the ledger already holds exactly the transcript's
	// rows, and the totals their sum; nothing is written.
	BackfillUnchanged = "unchanged"
	// BackfillActive: the session is live; its own end harvests it, and
	// nothing is written now.
	BackfillActive = "active"
)

// BackfillResult is the daemon's answer to one session's fill. Before is the
// conversation's token totals as they stood, After as they stand (dry run: as
// they would).
type BackfillResult struct {
	Result string          `json:"result,omitempty"`
	Before core.TokenUsage `json:"before"`
	After  core.TokenUsage `json:"after"`
	Error  string          `json:"error,omitempty"`
}
