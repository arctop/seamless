package console

// The session flag on the console's session views: a badge with the verdict
// and its reason on the Sessions list (and a filter, once any session is
// flagged), the flag and its transcript copy on the session page, and the
// copy itself at /console/sessions/{id}/transcript -- the owner's alone.

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/devices"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/transcripts"
)

// verdictFilterFlagged is the ?verdict= value that keeps every flagged
// session; the verdicts themselves keep their own.
const verdictFilterFlagged = "flagged"

// verdictFilterValues are the accepted ?verdict= values, for the error text.
func verdictFilterValues() []string {
	out := []string{verdictFilterFlagged}
	for _, v := range core.FlagVerdicts {
		out = append(out, string(v))
	}
	return out
}

// verdictFilter reads ?verdict= into the list's filter; ok is false for a
// value that is present but none of verdictFilterValues.
func verdictFilter(raw string, f *store.SessionFilter) bool {
	switch {
	case raw == "":
	case raw == verdictFilterFlagged:
		f.Flagged = true
	case slices.Contains(core.FlagVerdicts, core.SessionVerdict(raw)):
		f.Flagged, f.Verdict = true, core.SessionVerdict(raw)
	default:
		return false
	}
	return true
}

// verdictTone is the badge class a verdict takes.
func verdictTone(v core.SessionVerdict) string {
	switch v {
	case core.VerdictGood:
		return "ok"
	case core.VerdictBad:
		return "danger"
	default:
		return "pop"
	}
}

// sessionFlagView is a flagged session's flag and transcript copy, as the
// session page (and its JSON) shows them.
type sessionFlagView struct {
	Verdict   string    `json:"verdict"`
	Tone      string    `json:"-"`
	Reason    string    `json:"reason,omitempty"`
	FlaggedAt time.Time `json:"flaggedAt"`
	// TranscriptPath is the file the agent client wrote, on the agent's
	// computer; this page does not say whether it is still there (the daemon
	// opens no path an agent names under peer_auth): `seam sessions` does.
	TranscriptPath string `json:"transcriptPath,omitempty"`
	// Copy is the copy's record (core.TranscriptCopy, "" while none was
	// attempted), CopyText its words, and Kept whether the copy is on this
	// disk -- the file, not the record.
	Copy     string     `json:"copy,omitempty"`
	CopyText string     `json:"copyText"`
	Kept     bool       `json:"kept"`
	Bytes    int64      `json:"bytes,omitempty"`
	Size     string     `json:"-"`
	CopiedAt *time.Time `json:"copiedAt,omitempty"`
}

// flagView builds sess's flag view; nil when it is not flagged.
func (s *Service) flagView(sess core.Session) *sessionFlagView {
	if !sess.Flagged() {
		return nil
	}
	v := &sessionFlagView{
		Verdict: string(sess.Verdict), Tone: verdictTone(sess.Verdict), Reason: sess.VerdictReason,
		TranscriptPath: sess.TranscriptPath, Copy: string(sess.Transcript),
	}
	if sess.FlaggedAt != nil {
		v.FlaggedAt = *sess.FlaggedAt
	}
	v.Kept = s.cfg.Transcripts != nil && s.cfg.Transcripts.Kept(sess.ID)
	switch {
	case v.Kept:
		v.CopyText, v.Bytes, v.CopiedAt = core.TranscriptCopied.Explain(), sess.TranscriptBytes, sess.TranscriptCopiedAt
		if v.Bytes > 0 {
			v.Size = humanBytes(v.Bytes)
		}
	case sess.Transcript == core.TranscriptCopied:
		// The record names a copy this disk does not hold: a restored or
		// merged database, or one made on another instance.
		v.CopyText = "no copy on this server: the record names one made elsewhere, or before a restore"
	case s.cfg.Transcripts == nil:
		v.CopyText = "no copy: this server keeps no transcript copies"
	default:
		v.CopyText = sess.Transcript.Explain()
	}
	return v
}

// refusedTranscriptReader refuses a copy's reader that is not the owner's own
// key, cookie or socket account: a restricted account (auth refused it
// already: the console is the owner's), a request a paired relay forwarded
// from another computer, and a paired device's own credential. A copy holds
// a whole conversation, whatever projects and secrets it touched.
func refusedTranscriptReader(w http.ResponseWriter, r *http.Request) bool {
	ctx := r.Context()
	relayed := false
	if v, ok := access.FromContext(ctx); ok {
		relayed = v.Principal().Relay != ""
	}
	if _, device := devices.CredentialOf(ctx); !relayed && !device && access.OwnerOnly(ctx) {
		return false
	}
	writeJSON(w, http.StatusForbidden, map[string]string{"error": "a session's transcript copy is read only by the " +
		"owner on this server: the console's key or cookie, or the owner's own account on this server's socket, " +
		"never through a relay or a paired device"})
	return true
}

// sessionTranscript serves GET /console/sessions/{id}/transcript: a flagged
// session's transcript copy, decompressed, as the JSONL the agent client
// wrote. The session is read through the request's handle, so a locked
// project's session answers as locked.
func (s *Service) sessionTranscript(w http.ResponseWriter, r *http.Request) {
	if refusedTranscriptReader(w, r) {
		return
	}
	ctx := r.Context()
	id := r.PathValue("id")
	sess, ok, err := store.SessionByID(ctx, s.db(ctx), id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if !ok {
		if s.answerLockedSession(w, r, id) {
			return
		}
		s.notFound(w, r, "No session with id "+id+".")
		return
	}
	if !sess.Flagged() {
		s.notFound(w, r, "Session "+id+" is not flagged: only a flagged session's transcript is kept.")
		return
	}
	if s.cfg.Transcripts == nil {
		s.notFound(w, r, "This server keeps no transcript copies.")
		return
	}
	rc, err := s.cfg.Transcripts.Open(sess.ID)
	if errors.Is(err, transcripts.ErrNoCopy) {
		s.notFound(w, r, fmt.Sprintf("Session %s keeps no transcript copy: %s.", id, s.flagView(sess).CopyText))
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	defer func() { _ = rc.Close() }()
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Content-Disposition", `attachment; filename="`+strings.ToLower(sess.ID)+`.jsonl"`)
	w.Header().Set("Cache-Control", "no-store")
	// The status line goes before the copy is read through, so a copy that
	// turns out damaged (its gzip checksum is checked at the end) is said in
	// the trailer: `seam sessions --transcript` fails on it rather than hand
	// over a cut-off conversation as the whole one.
	w.Header().Set("Trailer", TranscriptErrorTrailer)
	if _, err := io.Copy(w, rc); err != nil {
		s.logger.Warn("console: stream transcript", "session", sess.ID, "error", err)
		w.Header().Set(TranscriptErrorTrailer, "the copy could not be read whole")
	}
}

// TranscriptErrorTrailer is the trailer a transcript stream sets when the
// copy could not be read whole.
const TranscriptErrorTrailer = "X-Seamless-Transcript-Error"
