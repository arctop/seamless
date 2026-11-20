package mcp

// The session flag on session_update and session_end: verdict and reason
// arguments (fields, not a tool of its own, so ToolCount is unchanged). A flag
// marks a whole session to re-read later and keeps its transcript for the
// owner (internal/transcripts); it is one of three "this mattered" signals,
// and the argument text says how it relates to the other two -- session_end's
// mishaps and favorite_set -- so an agent need not guess.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// The flag's argument names.
const (
	verdictParam       = "verdict"
	verdictReasonParam = "reason"
)

// sessionVerdictArgDesc is the verdict argument's text on both tools.
const sessionVerdictArgDesc = "Flag this whole session to re-read later: good (it went unusually well), bad " +
	"(unusually badly), notable (worth re-reading for another reason), or none (clear the flag). One flag per " +
	"session; the last one set wins. A flagged session's transcript is kept for the owner, copied at the " +
	"session's end (at once if it already ended); clearing the flag deletes the copy. Independent of " +
	"session_end's mishaps (one entry per incident, for recurrence review, whatever the verdict) and of " +
	"favorite_set (a star on any item, for briefing and recall weight)"

// sessionReasonArgDesc is the reason argument's text on both tools.
const sessionReasonArgDesc = "Why the verdict, one short line; only with a verdict other than none"

// sessionFlag is a call's flag arguments.
type sessionFlag struct {
	set     bool // verdict was sent
	verdict core.SessionVerdict
	reason  string
}

// sessionFlagArgs reads verdict and reason. validateMiddleware has already
// refused a verdict outside core.SessionVerdicts; this refuses what the enum
// cannot say: a reason without a verdict, a reason on the clearing none, and
// a reason too long for one line. A reason's line breaks and runs of spaces
// are folded into single spaces.
func sessionFlagArgs(req mcp.CallToolRequest) (sessionFlag, error) {
	f := sessionFlag{set: argPresent(req, verdictParam)}
	f.verdict = core.SessionVerdict(argString(req, verdictParam))
	f.reason = strings.Join(strings.Fields(argRaw(req, verdictReasonParam)), " ")
	switch {
	case !f.set && f.reason != "":
		return sessionFlag{}, errors.New("reason explains a verdict: pass verdict (good, bad or notable) with it")
	case !f.set:
		return f, nil
	case !f.verdict.Valid():
		return sessionFlag{}, fmt.Errorf("invalid verdict %q: valid values are good, bad, notable, none", f.verdict)
	case f.verdict == core.VerdictNone && f.reason != "":
		return sessionFlag{}, errors.New("verdict none clears the flag and takes no reason: omit reason")
	case utf8.RuneCountInString(f.reason) > core.MaxVerdictReasonRunes:
		return sessionFlag{}, fmt.Errorf("reason is %d characters: keep it to one line of at most %d",
			utf8.RuneCountInString(f.reason), core.MaxVerdictReasonRunes)
	}
	return f, nil
}

// applySessionFlag sets or clears sess's flag and answers what happened to it
// and to its transcript copy. endedBefore says the session had ended before
// this call: its copy is then made now, where the daemon may read the
// transcript (transcripts.Service.AfterEnd); otherwise the end signal makes it.
func (s *Server) applySessionFlag(ctx context.Context, sess core.Session, f sessionFlag, endedBefore bool) (map[string]any, error) {
	svc := s.cfg.Transcripts
	if f.verdict == core.VerdictNone {
		if svc == nil {
			_, cleared, err := store.ClearSessionFlag(ctx, s.cfg.DB, sess.ID)
			if err != nil {
				return nil, err
			}
			return map[string]any{"verdict": string(core.VerdictNone), "cleared": cleared}, nil
		}
		cleared, removed, err := svc.Unflag(ctx, sess.ID, "agent")
		if err != nil {
			return nil, err
		}
		answer := map[string]any{"verdict": string(core.VerdictNone), "cleared": cleared}
		if removed {
			answer["transcript"] = "copy deleted"
		}
		return answer, nil
	}
	answer := map[string]any{"verdict": string(f.verdict)}
	if svc == nil {
		if _, err := store.SetSessionFlag(ctx, s.cfg.DB, sess.ID, f.verdict, f.reason, s.now()); err != nil {
			return nil, err
		}
		answer["transcript"] = "not kept: this server keeps no transcript copies"
		return answer, nil
	}
	flagged, err := svc.Flag(ctx, sess, f.verdict, f.reason, "agent", endedBefore)
	if err != nil {
		return nil, err
	}
	if !endedBefore {
		// Ended in this call, or still running: the copy waits for the end
		// signal, after which the transcript is whole.
		flagged.Status = core.SessionActive
	}
	switch state := svc.AfterEnd(ctx, flagged); {
	case state != "":
		answer["transcript"] = state.Explain()
	case endedBefore:
		answer["transcript"] = "being copied now"
	default:
		answer["transcript"] = "copied at the session's end"
	}
	return answer, nil
}
