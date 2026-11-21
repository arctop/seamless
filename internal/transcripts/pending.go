package transcripts

// Pending transcripts: a flagged session whose end signal never made its
// copy. The end signal is the one point the whole conversation is known to be
// on disk, but it does not always come -- an agent that crashed, a terminal
// closed, a session the reaper expired -- and under peer_auth a flag set after
// the end has no hook left to send the file. Such a session is not left
// without a copy:
//
//   - With the daemon reading the file (no peer_auth, the agent's own
//     computer), the reaper's pass copies every ended flagged session still
//     without one (KeepEnded), and so does the next hook of the same caller
//     (CopyPending).
//   - Under peer_auth the session is pending: the next hooks of the same
//     caller -- the same agent client, account and computer -- are asked for
//     it (PendingFor, Ask), naming the session and the path its hooks
//     recorded, a few sessions an answer, with a growing wait between asks.
//     The ask ends with a copy, with the hook's report that the path is
//     refused or gone (Reported), or after core.TranscriptAskLimit asks
//     nobody answered (core.TranscriptNotSent).
//
// "The same caller" is the identity the hooks key ambient sessions on: the
// agent client and the restricted account the request speaks for (the owner
// is ""), and the computer the session ran on, since the path names a file
// there. Nothing here asks a caller about a session another principal
// started, or a session on another computer.

import (
	"context"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// Caller is who a hook speaks for, as pending transcripts are matched to it.
type Caller struct {
	// Client is the agent client as sessions record it (external_client).
	Client string
	// Account is the restricted account the request speaks for, "" for the
	// owner (access.AccountOf).
	Account string
	// Host is the hook's computer; "" is this daemon's own.
	Host string
	// Current is the agent session id the hook itself names (namespaced as
	// stored): never pending to it, since its own end signal makes its copy.
	Current string
}

// Pending is one agent session's flagged sessions whose transcript is still
// asked for: its ambient session and any explicit one linked to it, which
// share its transcript.
type Pending struct {
	// ExternalSessionID is the agent client's session id, as stored.
	ExternalSessionID string
	Sessions          []core.Session
}

// PendingFor returns caller's pending transcripts, oldest flag first: the
// flagged sessions of its agent client and account, on its computer, that
// hold no copy and are still asked for (core.TranscriptCopy.Asked), save the
// caller's own session and any whose upload is under way.
func (s *Service) PendingFor(ctx context.Context, c Caller) ([]Pending, error) {
	rows, err := store.PendingTranscriptSessions(ctx, s.db, c.Client, c.Account)
	if err != nil {
		return nil, err
	}
	host := s.hostOf(c.Host)
	var out []Pending
	at := map[string]int{}
	for _, sess := range rows {
		if sess.ExternalSessionID == c.Current || s.hostOf(sess.Host) != host || s.Kept(sess.ID) {
			continue
		}
		i, seen := at[sess.ExternalSessionID]
		if !seen {
			i = len(out)
			at[sess.ExternalSessionID] = i
			out = append(out, Pending{ExternalSessionID: sess.ExternalSessionID})
		}
		out[i].Sessions = append(out[i].Sessions, sess)
	}
	kept := out[:0]
	for _, p := range out {
		if !s.Uploading(p.Sessions) {
			kept = append(kept, p)
		}
	}
	return kept, nil
}

// path is the transcript p's sessions share: the first that names one
// (store.TranscriptPathOf). A session that names none -- and no session
// before it does -- is recorded as having none.
func (s *Service) path(ctx context.Context, p Pending, via string) string {
	for _, sess := range p.Sessions {
		path, err := store.TranscriptPathOf(ctx, s.db, sess)
		if err != nil {
			s.logger.Warn("transcripts: transcript path", "session", sess.ID, "error", err)
			continue
		}
		if path != "" {
			return path
		}
	}
	s.end(ctx, p.Sessions, core.TranscriptNoPath, via, 0)
	return ""
}

// Ask decides whether this hook's answer asks for p's transcript (peer_auth),
// and records the ask: the path to name and true, or false when p is waiting
// out the wait after its last ask, was just given up on (after
// core.TranscriptAskLimit asks, the wait after the last one passed), or names
// no transcript.
func (s *Service) Ask(ctx context.Context, p Pending) (string, bool) {
	if len(p.Sessions) == 0 {
		return "", false
	}
	asks, last := 0, time.Time{}
	for _, sess := range p.Sessions {
		asks = max(asks, sess.TranscriptAsks)
		if sess.TranscriptAskedAt != nil && sess.TranscriptAskedAt.After(last) {
			last = *sess.TranscriptAskedAt
		}
	}
	now := s.now()
	if asks > 0 && now.Before(last.Add(askWait(asks))) {
		return "", false
	}
	if asks >= core.TranscriptAskLimit {
		s.end(ctx, p.Sessions, core.TranscriptNotSent, ViaClient, asks)
		return "", false
	}
	path := s.path(ctx, p, ViaClient)
	if path == "" {
		return "", false
	}
	asked := false
	for _, sess := range p.Sessions {
		n, recorded, err := store.RecordTranscriptAsk(ctx, s.db, sess.ID, now)
		if err != nil {
			s.logger.Warn("transcripts: record ask", "session", sess.ID, "error", err)
			continue
		}
		if recorded {
			asked = true
			s.record(ctx, core.EventSessionTranscript, sess, map[string]any{
				"copy": string(core.TranscriptPending), "via": ViaClient, "asks": n,
			})
		}
	}
	return path, asked
}

// CopyPending copies p's transcript in the background where the daemon reads
// it itself (the caller judged Readable): the next hook of the same caller
// after an end signal that never came. A copy the daemon itself failed to
// write is not retried at every hook.
func (s *Service) CopyPending(ctx context.Context, p Pending) {
	var todo []core.Session
	for _, sess := range p.Sessions {
		if sess.Transcript != core.TranscriptFailed {
			todo = append(todo, sess)
		}
	}
	if len(todo) == 0 {
		return
	}
	p.Sessions = todo
	path := s.path(ctx, p, ViaDaemon)
	if path == "" {
		return
	}
	for _, sess := range todo {
		s.Snapshot(ctx, sess.ID, path, false)
	}
}

// Reported records what the agent's hook said of the path it was asked for
// (peer_auth): refused (core.TranscriptRefused) or gone
// (core.TranscriptUnreadable). The asks end with it.
func (s *Service) Reported(ctx context.Context, sessions []core.Session, state core.TranscriptCopy) {
	s.end(ctx, sessions, state, ViaClient, 0)
}

// end records state on each of sessions whose copy is still asked for, with
// the event; asks, when set, rides in its payload.
func (s *Service) end(ctx context.Context, sessions []core.Session, state core.TranscriptCopy, via string, asks int) {
	for _, sess := range sessions {
		recorded, err := store.EndTranscriptAsks(ctx, s.db, sess.ID, state)
		if err != nil {
			s.logger.Warn("transcripts: record copy", "session", sess.ID, "error", err)
			continue
		}
		if !recorded {
			continue
		}
		payload := map[string]any{"copy": string(state), "via": via}
		if asks > 0 {
			payload["asks"] = asks
		}
		s.record(ctx, core.EventSessionTranscript, sess, payload)
	}
}

// KeepEnded settles the flagged sessions that ended before cutoff without a
// copy, their transcript still asked for: the reaper's pass, after it
// expired the sessions no end signal closed. Where the daemon reads the file
// it copies each now; under peer_auth one never attempted is recorded
// pending, for its agent's later hooks to be asked; on another computer the
// record says so.
func (s *Service) KeepEnded(ctx context.Context, cutoff time.Time) {
	ended, err := store.EndedFlaggedSessions(ctx, s.db, cutoff)
	if err != nil {
		s.logger.Warn("transcripts: ended flagged sessions", "error", err)
		return
	}
	for _, sess := range ended {
		if s.Kept(sess.ID) {
			continue
		}
		ok, why := s.Readable(ctx, sess.Host)
		switch {
		case !ok && why == core.TranscriptNotPushed:
			if sess.Transcript == "" {
				s.MarkPending(ctx, sess)
			}
		case !ok:
			s.NotMade(ctx, sess, why, ViaDaemon)
		case sess.Transcript != core.TranscriptFailed:
			s.CopyPending(ctx, Pending{ExternalSessionID: sess.ExternalSessionID, Sessions: []core.Session{sess}})
		}
	}
}
