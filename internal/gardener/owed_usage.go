package gardener

// Owed usage at the upkeep. Claude Code reports a conversation's model tokens
// at its SessionEnd (internal/hooks usage.go), and while the Diary is on its
// prompts keep them current up to the turn before the last prompt
// (usage_live.go). A conversation whose session ended another way -- the
// agent closed it with session_end, or the reaper expired it: a closed
// terminal, a killed process, a Claude app that stops a session without its
// SessionEnd -- may never report the turns after that, and those are the
// ones a long agentic turn spends its time in. So its usage is owed
// (internal/store usage_owed.go) from that end: the reaper marks the Claude
// Code sessions it expires (markReapedUsageOwed), session_end marks the one it
// ends (internal/mcp), and a SessionEnd harvest settles it.
//
// Each upkeep then settles what it may (settleOwedUsage). Where the daemon may
// read an agent's file -- the rule every daemon-side harvest keeps
// (transcripts.Service.Readable, the hooks' localDiskOK): no peer_auth, the
// session's own computer, the owner's session -- it reads the transcript
// whenever it changed since the last read, the subagents' transcripts beside
// it counted, and records what it holds: the conversation may go on on its
// ended row (a prompt revives no row), so one read at the end is not enough.
// The row goes once nothing changed for store.UsageOwedHorizon, the file is
// gone, or the session is active again (its own next end marks it again). Under
// peer_auth the daemon opens no path a hook named: the caller's later hooks
// are asked for the usage instead (internal/hooks usage_owed.go), and the
// upkeep only drops what went active or past the horizon. A session recorded
// on another computer has its transcript there, and is dropped.
//
// The write is store.RefreshConversationUsage: the replace rule of every
// Claude Code harvest, so it is idempotent with the others and never moves the
// ledger back. It lands on the ended row without reviving it or moving its
// updated_at (store ledger.go), and a SessionEnd that does arrive later
// replaces it from the whole transcript, as always. Like every ledger write it
// checks no feature: the Diary only shows the ledger.

import (
	"context"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/harvest"
	"github.com/arctop/seamless/internal/store"
)

// markReapedUsageOwed marks the usage of the Claude Code sessions the reaper
// just expired as owed (store.MarkUsageOwed marks only a Claude Code
// conversation's ambient row): settleOwedUsage reads it.
func (s *Service) markReapedUsageOwed(ctx context.Context, reaped []core.Session, now time.Time) {
	for _, sess := range reaped {
		if _, err := store.MarkUsageOwed(ctx, s.db, sess.ID, now); err != nil {
			s.logger.Warn("gardener: mark a reaped session's usage owed", "session", sess.ID, "error", err)
		}
	}
}

// settleOwedUsage is the upkeep's pass over the owed usage (see the file
// comment). Best-effort like the rest of the upkeep: a failed write is
// logged, and the next upkeep tries again.
func (s *Service) settleOwedUsage(ctx context.Context) {
	owed, err := store.OwedUsages(ctx, s.db)
	if err != nil {
		s.logger.Warn("gardener: owed usage", "error", err)
		return
	}
	now := s.now().UTC()
	written := 0
	for _, o := range owed {
		if ctx.Err() != nil {
			return
		}
		if s.settleOwed(ctx, o, now) {
			written++
		}
	}
	if written > 0 {
		s.logger.Info("gardener recorded owed sessions' usage", "count", written)
	}
}

// settleOwed settles one conversation's owed usage, reporting whether its
// ledger was written.
func (s *Service) settleOwed(ctx context.Context, o store.OwedUsage, now time.Time) (wrote bool) {
	sess := o.Session
	if sess.Status == core.SessionActive {
		s.clearOwed(ctx, sess.ID) // its own next end marks it again
		return false
	}
	if s.cfg.PeerAuth {
		// The caller's hooks are asked for it; the upkeep only ends the wait.
		if now.Sub(o.LastChange()) >= store.UsageOwedHorizon {
			s.clearOwed(ctx, sess.ID)
		}
		return false
	}
	path, ok := s.owedTranscript(sess)
	if !ok {
		s.clearOwed(ctx, sess.ID)
		return false
	}
	changed, ok := harvest.ClaudeTranscriptChanged(path)
	if !ok {
		s.clearOwed(ctx, sess.ID) // gone, or not a regular file: opening a pipe would wait
		return false
	}
	if o.ChangedAt == nil || changed.After(*o.ChangedAt) {
		if _, rows, ok := harvest.ClaudeUsage(path); ok {
			got, err := store.RefreshConversationUsage(ctx, s.db, sess.ExternalClient, sess.ExternalSessionID, rows, now)
			if err != nil {
				s.logger.Warn("gardener: owed session's usage", "session", sess.ID, "error", err)
				return false
			}
			wrote = got == store.RefreshWritten
		}
		if _, err := store.RecordUsageOwedRead(ctx, s.db, sess.ID, changed); err != nil {
			s.logger.Warn("gardener: record owed usage read", "session", sess.ID, "error", err)
		}
	}
	if now.Sub(changed) >= store.UsageOwedHorizon {
		s.clearOwed(ctx, sess.ID)
	}
	return wrote
}

// clearOwed forgets a conversation's owed usage, logging a failure.
func (s *Service) clearOwed(ctx context.Context, id string) {
	if err := store.ClearUsageOwed(ctx, s.db, id); err != nil {
		s.logger.Warn("gardener: clear owed usage", "session", id, "error", err)
	}
}

// owedTranscript is the transcript an owed conversation's usage is read from:
// the one its SessionStart recorded on its ambient row, when the session was
// recorded on this computer (an empty host is an older client's, on this one)
// by the owner. ok is false for any other: a restricted account's transcript
// is in that account's home, and another computer's is on that computer.
func (s *Service) owedTranscript(sess core.Session) (string, bool) {
	host := strings.ToLower(strings.TrimSpace(sess.Host))
	local := strings.ToLower(strings.TrimSpace(s.cfg.LocalHost))
	if sess.Account != "" || sess.TranscriptPath == "" || (host != "" && host != local) {
		return "", false
	}
	return sess.TranscriptPath, true
}
