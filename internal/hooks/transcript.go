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
//
// An end signal that never came leaves a flagged session pending
// (transcripts/pending.go). The same caller's next SessionStart, SessionEnd
// and Codex Stop then make up for it (keepPending): the daemon copies the
// file itself where it may, and under peer_auth the answer carries
// harvest.TranscriptPendingHeader, naming the session and its recorded path,
// which `seam hook` reads only inside its agent client's own transcript
// folder. Only an answer to a caller with a pending session carries it.
//
// The owner may keep a flagged session's transcript by hand
// (transcriptKeep, `seam sessions <id> --keep-transcript`), keyed by the
// session's ULID: the daemon copies it itself where it may, and under
// peer_auth names the path for the owner's own account to send.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/devices"
	"github.com/arctop/seamless/internal/harvest"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/transcripts"
)

// maxTranscriptPath bounds a recorded transcript path.
const maxTranscriptPath = 4096

// uploadTimeout bounds one transcript upload: the time the client's detached
// sender has (harvest.TranscriptUploadTimeout), so a stalled sender cannot
// hold the handler for good. The route lifts the server's per-request read
// timeout to it, which would otherwise end any body past half a minute.
const uploadTimeout = harvest.TranscriptUploadTimeout

// transcriptKeepPattern is the owner's route that keeps a flagged session's
// transcript by hand, keyed by the session's ULID (transcriptKeep).
const transcriptKeepPattern = harvest.TranscriptEndpoint + "/{id}"

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
		if h.transcripts.Uploading(flagged) {
			// The transcript is on its way already; the next end signal asks
			// again, for whatever it grew by since.
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

// keepPending makes up for the end signals that never came, at a hook of the
// caller's (SessionStart, SessionEnd, Codex's Stop; current is the agent
// session id the hook names, whose own end signal makes its copy): every
// pending flagged session of the same agent client, account and computer
// that the caller may act on (reachOK). Where the daemon reads the file it
// copies it now; under peer_auth, on the socket, the answer asks for at most
// harvest.MaxPendingPerAnswer of them (harvest.TranscriptPendingHeader). The
// TCP key's hook under peer_auth sends nothing, so nothing asks it; and an
// answer to a caller with no pending session carries nothing at all.
func (h *Handler) keepPending(ctx context.Context, w http.ResponseWriter, client Client, host, current string) {
	if h.transcripts == nil || h.db == nil {
		return
	}
	readable, why := h.transcripts.Readable(ctx, host)
	switch {
	case readable:
		// The daemon copies them itself.
	case why == core.TranscriptNotPushed && access.PeerAuthenticated(ctx):
		// peer_auth, on the socket: the answer asks for them.
	default:
		return // another computer's files, or the TCP key's hook, which sends nothing
	}
	pending, err := h.transcripts.PendingFor(ctx, transcripts.Caller{
		Client: client.externalIdentity(), Account: access.AccountOf(ctx), Host: host, Current: current,
	})
	if err != nil {
		h.recordHookError(ctx, "transcript-pending", client, err)
		return
	}
	ns := access.SessionNamespace(ctx)
	done := 0
	for _, p := range pending {
		if done == harvest.MaxPendingPerAnswer {
			return
		}
		if reach, _ := h.sessionReachOf(ctx, client, p.ExternalSessionID); reach != reachOK {
			continue // a locked project's session keeps none before the owner's approval
		}
		if readable {
			h.transcripts.CopyPending(ctx, p)
			done++
			continue
		}
		path, ok := h.transcripts.Ask(ctx, p)
		if !ok {
			continue
		}
		w.Header().Add(harvest.TranscriptPendingHeader, url.Values{
			harvest.TranscriptSessionParam: {strings.TrimPrefix(p.ExternalSessionID, ns)},
			harvest.TranscriptPathParam:    {path},
		}.Encode())
		done++
	}
}

// transcriptUpload serves POST harvest.TranscriptEndpoint: a flagged
// session's transcript as `seam hook` sent it under peer_auth, gzip-compressed,
// when the answer to the session's end signal asked for it, or a later answer
// asked for a pending one (keepPending). It is taken only on the peer_auth
// socket (this daemon's, or a paired relay's): with peer_auth off the daemon
// reads the file itself, and the TCP key's request carries no kernel-named
// caller. Like a pushed capture it is the caller's own claim, and it lands
// only on a session the caller may act on (reachOK) that is flagged; anything
// else is refused before the body is read. A request carrying
// harvest.TranscriptRefusedParam sends no body: it reports why the client did
// not read a pending session's path, and the asks for it end. One upload of a
// session runs at a time: a second is refused while the first is under way.
// The answer is JSON: what became of the copy.
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
	if q := r.URL.Query(); q.Has(harvest.TranscriptRefusedParam) {
		var state core.TranscriptCopy
		switch q.Get(harvest.TranscriptRefusedParam) {
		case harvest.TranscriptRefusedPath:
			state = core.TranscriptRefused
		case harvest.TranscriptRefusedMissing:
			state = core.TranscriptUnreadable
		default:
			writeUploadAnswer(w, http.StatusBadRequest, "", "invalid "+harvest.TranscriptRefusedParam+" "+
				strconv.Quote(q.Get(harvest.TranscriptRefusedParam))+": valid values are "+
				harvest.TranscriptRefusedPath+", "+harvest.TranscriptRefusedMissing)
			return
		}
		h.transcripts.Reported(ctx, flagged, state)
		writeUploadAnswer(w, http.StatusOK, state, "")
		return
	}
	end, ok := h.transcripts.BeginUpload(flagged)
	if !ok {
		writeUploadAnswer(w, http.StatusConflict, "", "this session's transcript is being sent already")
		return
	}
	defer end()
	h.receiveUpload(ctx, w, r, flagged, transcripts.ViaClient)
}

// receiveUpload reads an upload's body into flagged's copy (Receive) and
// answers what became of it, the server's read timeout lifted to
// uploadTimeout for the body.
func (h *Handler) receiveUpload(ctx context.Context, w http.ResponseWriter, r *http.Request, flagged []core.Session, via string) {
	if err := http.NewResponseController(w).SetReadDeadline(time.Now().Add(uploadTimeout)); err != nil &&
		!errors.Is(err, http.ErrNotSupported) {
		h.logger.Warn("hooks: transcript upload read deadline", "error", err)
	}
	claimed := int64(-1)
	if raw := r.Header.Get(harvest.TranscriptSizeHeader); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n >= 0 {
			claimed = n
		}
	}
	r.Body = http.MaxBytesReader(w, r.Body, h.transcripts.MaxBytes()+(1<<20))
	state := h.transcripts.Receive(ctx, flagged, r.Body, claimed, via)
	switch state {
	case core.TranscriptCopied:
		writeUploadAnswer(w, http.StatusOK, state, "")
	case core.TranscriptTooLarge:
		writeUploadAnswer(w, http.StatusRequestEntityTooLarge, state, state.Explain())
	default:
		writeUploadAnswer(w, http.StatusBadRequest, state, state.Explain())
	}
}

// keepAnswer is transcriptKeep's JSON answer: what became of the copy, or,
// under peer_auth, the transcript the owner's own account is to send.
type keepAnswer struct {
	Copy  string    `json:"copy,omitempty"`
	Error string    `json:"error,omitempty"`
	Send  *keepSend `json:"send,omitempty"`
}

// keepSend names the transcript to send: the path the session's hooks
// recorded, the computer it is on, and the agent client that wrote it (whose
// transcript folder the sender checks the path against).
type keepSend struct {
	Path   string `json:"path"`
	Host   string `json:"host"`
	Client string `json:"client"`
}

// writeKeepAnswer writes transcriptKeep's answer.
func writeKeepAnswer(w http.ResponseWriter, status int, a keepAnswer) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	//nolint:errcheck // the status is committed; a failed write means the client hung up
	_ = json.NewEncoder(w).Encode(a)
}

// transcriptOwnerRefusal is the answer to a caller other than the owner on
// this server, the same callers the console's copy read refuses: a
// restricted account, a request a paired relay forwarded, a paired device.
const transcriptOwnerRefusal = "a session's transcript is kept by hand only by the owner on this server: " +
	"the key, or the owner's own account on this server's socket -- never through a relay or a paired device"

// transcriptOwner reports whether the request is the owner's own on this
// server (refusedTranscriptReader's rule, internal/console).
func transcriptOwner(ctx context.Context) bool {
	relayed := false
	if v, ok := access.FromContext(ctx); ok {
		relayed = v.Principal().Relay != ""
	}
	_, device := devices.CredentialOf(ctx)
	return !relayed && !device && access.OwnerOnly(ctx)
}

// transcriptKeep serves POST transcriptKeepPattern: the owner keeping a
// flagged session's transcript by hand, when its end signal never sent it
// (`seam sessions <id> --keep-transcript`). It is the owner's alone -- never a
// restricted account, a relay's caller or a paired device -- and a session in
// a project locked to the caller answers as locked.
//
//   - With harvest.TranscriptFromDaemonParam the daemon reads the recorded path itself,
//     where it may (Readable: no peer_auth, the session's own computer), while
//     the request waits, and answers what became of the copy.
//   - Without it and without a body, under peer_auth on the socket, the answer
//     names the path, computer and client (keepSend) for the owner's own
//     account to read and send.
//   - With a body (harvest.TranscriptSizeHeader set), on the socket, it is that
//     upload, taken as transcriptUpload takes one.
func (h *Handler) transcriptKeep(w http.ResponseWriter, r *http.Request) {
	if !verifyBearer(r, h.apiKey) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	ctx := r.Context()
	if !transcriptOwner(ctx) {
		writeKeepAnswer(w, http.StatusForbidden, keepAnswer{Error: transcriptOwnerRefusal})
		return
	}
	if h.transcripts == nil || h.db == nil {
		writeKeepAnswer(w, http.StatusNotFound, keepAnswer{Error: "this daemon keeps no transcript copies"})
		return
	}
	id := r.PathValue("id")
	sess, found, err := store.SessionByID(ctx, h.db, id)
	if err != nil {
		h.logger.Warn("hooks: transcript keep: load session", "error", err)
		writeKeepAnswer(w, http.StatusServiceUnavailable, keepAnswer{Error: "the session could not be looked up"})
		return
	}
	switch {
	case !found:
		writeKeepAnswer(w, http.StatusNotFound, keepAnswer{Error: "no session with id " + strconv.Quote(id)})
		return
	case !access.Visible(ctx, sess.ProjectSlug):
		writeKeepAnswer(w, http.StatusForbidden, keepAnswer{Error: "session " + id + " is in a locked project: " +
			"unlock the project, then keep its transcript"})
		return
	case !sess.Flagged():
		writeKeepAnswer(w, http.StatusConflict, keepAnswer{Error: "session " + id +
			" is not flagged: only a flagged session's transcript is kept"})
		return
	}
	_, peerAuth := access.FromContext(ctx)
	upload := r.Header.Get(harvest.TranscriptSizeHeader) != ""
	switch {
	case r.URL.Query().Has(harvest.TranscriptFromDaemonParam):
		h.keepFromDaemon(ctx, w, sess)
	case peerAuth && !access.PeerAuthenticated(ctx):
		writeKeepAnswer(w, http.StatusForbidden, keepAnswer{Error: "under peer_auth the daemon reads no agent file: " +
			"the owner's own account sends it, over this server's socket"})
	case !peerAuth:
		writeKeepAnswer(w, http.StatusBadRequest, keepAnswer{Error: "on this server the daemon reads the transcript itself: " +
			"ask it to (" + harvest.TranscriptFromDaemonParam + "=1)"})
	case upload:
		end, ok := h.transcripts.BeginUpload([]core.Session{sess})
		if !ok {
			writeKeepAnswer(w, http.StatusConflict, keepAnswer{Error: "this session's transcript is being sent already"})
			return
		}
		defer end()
		ctx, cancel := context.WithTimeout(ctx, uploadTimeout)
		defer cancel()
		h.receiveUpload(ctx, w, r, []core.Session{sess}, transcripts.ViaOwner)
	default:
		path, err := store.TranscriptPathOf(ctx, h.db, sess)
		if err != nil {
			h.logger.Warn("hooks: transcript keep: transcript path", "session", sess.ID, "error", err)
			writeKeepAnswer(w, http.StatusServiceUnavailable, keepAnswer{Error: "the session's transcript path could not be looked up"})
			return
		}
		if path == "" {
			writeKeepAnswer(w, http.StatusNotFound, keepAnswer{Copy: string(core.TranscriptNoPath), Error: core.TranscriptNoPath.Explain()})
			return
		}
		writeKeepAnswer(w, http.StatusOK, keepAnswer{Send: &keepSend{
			Path: path, Host: cmp.Or(strings.ToLower(strings.TrimSpace(sess.Host)), h.localHost), Client: sess.ExternalClient,
		}})
	}
}

// keepFromDaemon is transcriptKeep's daemon read: the copy made while the
// request waits, where the daemon may read the file, and what became of it.
func (h *Handler) keepFromDaemon(ctx context.Context, w http.ResponseWriter, sess core.Session) {
	if ok, why := h.transcripts.Readable(ctx, sess.Host); !ok {
		msg := why.Explain()
		if why == core.TranscriptNotPushed {
			msg = "under peer_auth the daemon reads no agent file: the owner's own account sends it, over this server's socket"
		}
		writeKeepAnswer(w, http.StatusConflict, keepAnswer{Copy: string(why), Error: msg})
		return
	}
	path, err := store.TranscriptPathOf(ctx, h.db, sess)
	if err != nil {
		h.logger.Warn("hooks: transcript keep: transcript path", "session", sess.ID, "error", err)
		writeKeepAnswer(w, http.StatusServiceUnavailable, keepAnswer{Error: "the session's transcript path could not be looked up"})
		return
	}
	if path == "" {
		h.transcripts.NotMade(ctx, sess, core.TranscriptNoPath, transcripts.ViaDaemon)
		writeKeepAnswer(w, http.StatusNotFound, keepAnswer{Copy: string(core.TranscriptNoPath), Error: core.TranscriptNoPath.Explain()})
		return
	}
	switch state := h.transcripts.CopyNow(ctx, sess.ID, path); state {
	case core.TranscriptCopied:
		writeKeepAnswer(w, http.StatusOK, keepAnswer{Copy: string(state)})
	case "":
		writeKeepAnswer(w, http.StatusConflict, keepAnswer{Error: "session " + sess.ID + " is no longer flagged"})
	default:
		writeKeepAnswer(w, http.StatusUnprocessableEntity, keepAnswer{Copy: string(state), Error: state.Explain()})
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
