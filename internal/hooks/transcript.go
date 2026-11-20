package hooks

// The session flag's transcript copy at the hooks (internal/transcripts holds
// the copies). Every SessionStart records the transcript path its payload
// names, for every session, flagged or not: a flag set later -- or after the
// session ended -- then knows which file to keep. The copy itself is made at
// the session's end signal, once the client has flushed the file (memory
// cc-stop-hooks-race-the-transcript-writer): Claude Code's SessionEnd (it
// installs no Stop hook), and each Stop of Codex (which has no SessionEnd),
// copied again only once the rollout grew.
//
// Who reads the file:
//
//   - With peer_auth off, on the agent's own computer, the daemon copies it
//     itself, in the background, as it reads the transcript for the harvest.
//   - Under peer_auth the daemon opens no path a hook names (localDiskOK). Its
//     answer to the end signal of a flagged session carries
//     harvest.TranscriptWantedHeader, and `seam hook` -- the owner's account,
//     beside the agent -- sends the file to transcriptUpload in a request of
//     its own. Only a flagged session's answer carries the header, so the
//     client needs no probe and keeps no state to learn whether to send.
//   - For an agent on another computer of a server without peer_auth, no copy
//     is made, and the record says so.
//
// The SessionEnd snapshots a flagged session whatever its status: an agent
// that closed its session with the session_end tool -- the common order --
// completed the row before the hook's end arrives.

import (
	"cmp"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/harvest"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/transcripts"
)

// maxTranscriptPath bounds a recorded transcript path.
const maxTranscriptPath = 4096

// uploadTimeout bounds one transcript upload. The client's own deadline (its
// hook's) ends it sooner; this one keeps a stalled sender from holding the
// handler for good.
const uploadTimeout = 5 * time.Minute

// recordablePath reports whether a hook's transcript_path is worth recording: a
// single line of printable text, of a sane length. Anything else is dropped,
// never cleaned: it is shown to the owner verbatim.
func recordablePath(path string) bool {
	if path == "" || len(path) > maxTranscriptPath || !utf8.ValidString(path) {
		return false
	}
	return !strings.ContainsFunc(path, unicode.IsControl)
}

// recordTranscriptPath records the transcript path a SessionStart names on the
// session it started or resumed. Best-effort, like every hook write.
func (h *Handler) recordTranscriptPath(ctx context.Context, client Client, sessionID, path string) {
	if h.db == nil || sessionID == "" || !recordablePath(path) {
		return
	}
	if err := store.SetSessionTranscriptPath(ctx, h.db, sessionID, path); err != nil {
		h.recordHookError(ctx, "transcript-path", client, err, "session", sessionID)
	}
}

// snapshotAtEnd makes the transcript copy of every flagged session the end
// signal's client session id names: the daemon copies the file itself where
// it may, under peer_auth it asks `seam hook` for it (the header on w), and
// for an agent on another computer it records that none is made. force copies
// even an unchanged file (Claude Code's SessionEnd); Codex's Stop copies
// again only once the rollout grew. Only a session the caller may act on
// (reachOK) is copied: a locked project's session keeps none before the
// owner's approval, as it takes no findings.
func (h *Handler) snapshotAtEnd(ctx context.Context, w http.ResponseWriter, client Client, host, externalID, path string, reach sessionReach, force bool) {
	if h.transcripts == nil || h.db == nil || externalID == "" || reach != reachOK {
		return
	}
	flagged, err := store.FlaggedSessionsByExternalIdentity(ctx, h.db, client.externalIdentity(), externalID)
	if err != nil {
		h.recordHookError(ctx, "transcript-lookup", client, err)
		return
	}
	if len(flagged) == 0 {
		return
	}
	if _, peerAuth := access.FromContext(ctx); peerAuth {
		if !access.PeerAuthenticated(ctx) {
			// The TCP key under peer_auth: its hook sends nothing (it pushes
			// only to the socket), so no answer asks it to.
			for _, sess := range flagged {
				h.transcripts.NotMade(ctx, sess, core.TranscriptNotPushed, transcripts.ViaClient)
			}
			return
		}
		w.Header().Set(harvest.TranscriptWantedHeader, strconv.FormatInt(h.wantedBytes(flagged, force), 10))
		return
	}
	if !h.sameHost(host) {
		h.recordRemoteSkip(ctx, "transcript-copy", client, host)
		for _, sess := range flagged {
			h.transcripts.NotMade(ctx, sess, core.TranscriptRemote, transcripts.ViaDaemon)
		}
		return
	}
	if !recordablePath(path) {
		path = ""
	}
	for _, sess := range flagged {
		src := cmp.Or(path, sess.TranscriptPath)
		if src == "" {
			if src, err = store.TranscriptPathOf(ctx, h.db, sess); err != nil {
				h.recordHookError(ctx, "transcript-path-lookup", client, err, "session", sess.ID)
			}
		}
		if src == "" {
			h.transcripts.NotMade(ctx, sess, core.TranscriptNoPath, transcripts.ViaDaemon)
			continue
		}
		h.transcripts.Snapshot(ctx, sess.ID, src, force)
	}
}

// wantedBytes is the header's value: the size past which the client sends the
// transcript. 0 when forced or when any of the sessions keeps no copy yet;
// otherwise the smallest copy kept, so the file is sent once it grew past it.
func (h *Handler) wantedBytes(flagged []core.Session, force bool) int64 {
	if force {
		return 0
	}
	var least int64 = -1
	for _, sess := range flagged {
		if sess.Transcript != core.TranscriptCopied || !h.transcripts.Kept(sess.ID) {
			return 0
		}
		if least < 0 || sess.TranscriptBytes < least {
			least = sess.TranscriptBytes
		}
	}
	return max(least, 0)
}

// transcriptUpload serves POST harvest.TranscriptEndpoint: a flagged
// session's transcript as `seam hook` sent it under peer_auth, gzip-compressed,
// when the answer to the session's end signal asked for it. It is taken only
// on the peer_auth socket (this daemon's, or a paired relay's): with peer_auth
// off the daemon reads the file itself, and the TCP key's request carries no
// kernel-named caller. Like a pushed capture it is the caller's own claim,
// and it lands only on a session the caller may act on (reachOK) that is
// flagged; anything else is refused before the body is read. The answer is
// JSON: what became of the copy.
func (h *Handler) transcriptUpload(w http.ResponseWriter, r *http.Request) {
	if !verifyBearer(r, h.apiKey) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	client, ok := requireRequestClient(w, r)
	if !ok {
		return
	}
	if !access.PeerAuthenticated(r.Context()) {
		writeUploadAnswer(w, http.StatusForbidden, "",
			"a transcript is sent only over the peer_auth socket; on this server the daemon reads it itself")
		return
	}
	if h.transcripts == nil || h.db == nil {
		writeUploadAnswer(w, http.StatusNotFound, "", "this daemon keeps no transcript copies")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), uploadTimeout)
	defer cancel()
	externalID := namespacedExternalID(r, strings.TrimSpace(r.URL.Query().Get(harvest.TranscriptSessionParam)))
	reach, _ := h.sessionReachOf(ctx, client, externalID)
	var flagged []core.Session
	if externalID != "" && reach == reachOK {
		var err error
		if flagged, err = store.FlaggedSessionsByExternalIdentity(ctx, h.db, client.externalIdentity(), externalID); err != nil {
			h.recordHookError(ctx, "transcript-lookup", client, err)
			writeUploadAnswer(w, http.StatusServiceUnavailable, "", "the session could not be looked up")
			return
		}
	}
	if len(flagged) == 0 {
		// A session the caller may not act on reads exactly like one that is
		// not flagged: nothing about it is the caller's to learn.
		writeUploadAnswer(w, http.StatusNotFound, "", "no flagged session of this client's session id wants a transcript")
		return
	}
	claimed := int64(-1)
	if raw := r.Header.Get(harvest.TranscriptSizeHeader); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n >= 0 {
			claimed = n
		}
	}
	r.Body = http.MaxBytesReader(w, r.Body, h.transcripts.MaxBytes()+(1<<20))
	state := h.transcripts.Receive(ctx, flagged, r.Body, claimed)
	switch state {
	case core.TranscriptCopied:
		writeUploadAnswer(w, http.StatusOK, state, "")
	case core.TranscriptTooLarge:
		writeUploadAnswer(w, http.StatusRequestEntityTooLarge, state, state.Explain())
	default:
		writeUploadAnswer(w, http.StatusBadRequest, state, state.Explain())
	}
}

// writeUploadAnswer writes transcriptUpload's JSON answer.
func writeUploadAnswer(w http.ResponseWriter, status int, state core.TranscriptCopy, msg string) {
	out := map[string]string{}
	if state != "" {
		out["copy"] = string(state)
	}
	if msg != "" {
		out["error"] = msg
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	//nolint:errcheck // the status is committed; a failed write means the client hung up
	_ = json.NewEncoder(w).Encode(out)
}
