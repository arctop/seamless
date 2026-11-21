package hooks

// The Diary's mid-session usage harvest (features.Diary, plan:diary step 2).
// Claude Code reports a session's usage at SessionEnd, and sessions stay open
// for hours or days, so without this a live session's ledger is empty, and a
// killed one's stays empty. While the Diary is on, UserPromptSubmit -- the
// hook Claude Code fires on every prompt; it installs no Stop hook -- keeps
// the conversation's ledger current: by then the previous turn is on disk.
//
// It adds nothing to the prompt's wait. The daemon decides, then answers
// first (askLiveUsage). It asks only:
//
//   - while the Diary is on, read live: the file/env base with the console's
//     override over it, so the toggle needs no restart;
//   - for the caller's own Claude Code conversation (reachOK) with an
//     ambient row: a session in a locked project takes no counts before the
//     owner approves, as at its end;
//   - at most once per usageLiveInterval for a conversation, and never while
//     a read of it is under way (the throttle, which lives here, so the client
//     keeps no state).
//
// How the rows come is the transport's:
//
//   - With peer_auth off, on the agent's own computer (localDiskOK's rule),
//     the daemon reads the transcript itself after answering, in the
//     background, with a context of its own: one read at a time per
//     conversation, at most usageLiveReads at once in all. Wait and WaitFor
//     wait for them, so serve can let them finish before the database closes.
//   - On the peer_auth socket the answer carries harvest.UsageWantedHeader, and
//     `seam hook` hands the read to a detached `seam hook-usage`, which pushes
//     the rows to usagePush (harvest.UsageEndpoint). The route takes a push
//     only on the socket, for a session the caller may act on, and only to
//     answer an ask still open, once.
//   - Anywhere else -- an agent on another computer, the TCP key under
//     peer_auth -- nothing is read, asked or recorded: the session's end
//     harvests it, and records its skip there, never once a prompt.
//
// Both write through store.RefreshConversationUsage: the replace rule of every
// Claude Code harvest, so it is idempotent with itself and with SessionEnd,
// except that rows counting fewer messages than the ledger holds (a read a
// later harvest overtook, the end's included) write nothing, and rows the
// ledger holds already write nothing either. A session with no SessionEnd
// keeps a ledger up to the turn before the last prompt that asked.
//
// The write heartbeats an active ambient row, Replace's rule. That is the
// prompt's own sign of life -- which touchAmbient also records, when the hook
// reads the prompt's text -- moved by the follow-up's delay; it never revives
// a row the agent or the reaper ended, nor moves its time.
//
// Off, none of it runs: no header, no read, no write, no throttle state, and
// the answer is the one the hook always gave.

import (
	"cmp"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/features"
	"github.com/arctop/seamless/internal/harvest"
	"github.com/arctop/seamless/internal/store"
)

// usageLiveInterval is the throttle: a conversation's usage is asked for at
// most once in this long. It was picked from a measurement of
// harvest.ClaudeUsage on one machine's real transcripts (2026-11-21, warm
// cache, about 1 GiB/s): 2.5 ms for a 2.6 MB transcript, 150 ms for the
// largest conversation there (139 MiB with its 35 subagents' transcripts),
// and 1.5 ms or less for the write. At one read every three minutes the
// largest costs under 0.1% of one core while its prompts never stop, and
// three minutes is a fifth of a ledger bucket, so the ledger is never more
// than that behind a conversation's last prompt.
const usageLiveInterval = 3 * time.Minute

// usageLiveReads caps the daemon's own mid-session reads under way at once,
// across conversations: a prompt that finds them all busy asks nothing, and
// the conversation's next prompt asks again.
const usageLiveReads = 2

// usageLiveTimeout bounds a daemon read's write. The read has a context of
// its own: the request's ends when the hook answers.
const usageLiveTimeout = 30 * time.Second

// liveUsage is the mid-session harvest's state: each conversation asked
// within usageLiveInterval, and the daemon's reads under way.
type liveUsage struct {
	mu    sync.Mutex
	convs map[string]liveConv
	reads int // daemon reads under way
	wg    sync.WaitGroup
}

// liveConv is one conversation's ask.
type liveConv struct {
	asked   time.Time // when its usage was last asked for
	open    bool      // a push may still answer the ask (the socket)
	reading bool      // the daemon's own read of it is under way
}

// liveWay is how a conversation's usage is asked for.
type liveWay uint8

const (
	// liveRead: the daemon reads the transcript itself, after answering.
	liveRead liveWay = iota + 1
	// liveAsk: the answer asks `seam hook` for it (harvest.UsageWantedHeader).
	liveAsk
)

func newLiveUsage() *liveUsage { return &liveUsage{convs: map[string]liveConv{}} }

// liveKey names a conversation in liveUsage: its client and the agent
// client's own session id, namespaced (namespacedExternalID). The client's
// name holds no slash, so the first one ends it.
func liveKey(client Client, externalID string) string {
	return client.externalIdentity() + "/" + externalID
}

// asked is how many conversations the state holds.
func (l *liveUsage) asked() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.convs)
}

// claim records that key's usage is asked for at now, the way way says, when
// the throttle allows it: no ask of it within usageLiveInterval, no read of
// it under way, and for a read, a free slot. It reports whether it may be
// asked. An entry past its interval throttles nothing, so each claim drops
// those first: the state holds only the conversations asked in the last
// interval.
func (l *liveUsage) claim(key string, way liveWay, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, c := range l.convs {
		if !c.reading && now.Sub(c.asked) >= usageLiveInterval {
			delete(l.convs, k)
		}
	}
	if _, throttled := l.convs[key]; throttled {
		return false
	}
	if way == liveRead {
		if l.reads >= usageLiveReads {
			return false
		}
		l.reads++
	}
	l.convs[key] = liveConv{asked: now, open: way == liveAsk, reading: way == liveRead}
	return true
}

// done ends key's read.
func (l *liveUsage) done(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reads--
	if c, ok := l.convs[key]; ok {
		c.reading = false
		l.convs[key] = c
	}
}

// answer reports whether a push for key answers an ask still open -- made
// within usageLiveInterval, which outlasts harvest.UsagePushTimeout -- and
// closes it: one push an ask.
func (l *liveUsage) answer(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	c, ok := l.convs[key]
	if !ok || !c.open || now.Sub(c.asked) >= usageLiveInterval {
		return false
	}
	c.open = false
	l.convs[key] = c
	return true
}

// askLiveUsage is the mid-session harvest's decision at a prompt (see the
// file header). On the peer_auth socket it sets the ask on w, which must not
// be written yet; where the daemon reads the agent's disk it returns the
// read, to start once the answer is written. It returns nil otherwise, and
// always with the Diary off. amb and found are the conversation's ambient
// row (ambientRow), reach the session's (sessionReachOf).
func (h *Handler) askLiveUsage(ctx context.Context, w http.ResponseWriter, client Client, p promptPayload, amb core.Session, found bool, reach sessionReach) (read func()) {
	way, path, ok := h.liveWayOf(ctx, client, p, amb, found, reach)
	if !ok || !h.featureOn(ctx, features.Diary) {
		return nil
	}
	return h.claimLiveUsage(ctx, w, client, p.SessionID, way, path)
}

// askLiveUsageBare is askLiveUsage for a prompt the hook answers without
// reading its text: a body naming none in the field decodePrompt reads. The
// harvest needs no prompt text, so it does not depend on that field. The
// Diary comes first: with it off nothing is looked up, and the answer is the
// empty one the hook always gave such a prompt.
func (h *Handler) askLiveUsageBare(ctx context.Context, w http.ResponseWriter, client Client, p promptPayload) (read func()) {
	if h.db == nil || client != ClientClaudeCode || p.SessionID == "" || !h.featureOn(ctx, features.Diary) {
		return nil
	}
	reach, _ := h.sessionReachOf(ctx, client, p.SessionID)
	amb, found := h.ambientRow(ctx, client, p.SessionID)
	p.Host = amb.Host
	way, path, ok := h.liveWayOf(ctx, client, p, amb, found, reach)
	if !ok {
		return nil
	}
	return h.claimLiveUsage(ctx, w, client, p.SessionID, way, path)
}

// liveWayOf is how a prompt's conversation may have its usage kept current,
// ok false where it may not: only the caller's own Claude Code conversation
// with an ambient row; on the peer_auth socket the answer asks for it; with
// peer_auth off, on the agent's own computer -- localDiskOK's rule, without
// its record, since a skip is never recorded once a prompt -- the daemon
// reads the transcript the prompt names, else the one its session recorded.
// Another computer's agent, or the TCP key under peer_auth (whose hook pushes
// nothing), is neither: its end harvests it.
func (h *Handler) liveWayOf(ctx context.Context, client Client, p promptPayload, amb core.Session, found bool, reach sessionReach) (way liveWay, path string, ok bool) {
	if h.db == nil || client != ClientClaudeCode || p.SessionID == "" || !found || reach != reachOK {
		return 0, "", false
	}
	_, peerAuth := access.FromContext(ctx)
	switch {
	case peerAuth && access.PeerAuthenticated(ctx):
		return liveAsk, "", true
	case !peerAuth && h.sameHost(p.Host):
		path = cmp.Or(p.TranscriptPath, amb.TranscriptPath)
		return liveRead, path, path != ""
	}
	return 0, "", false
}

// claimLiveUsage asks for the conversation's usage when the throttle allows
// it: the header on w, or the read to start once the answer is written.
func (h *Handler) claimLiveUsage(ctx context.Context, w http.ResponseWriter, client Client, externalID string, way liveWay, path string) (read func()) {
	key := liveKey(client, externalID)
	if !h.live.claim(key, way, h.now()) {
		return nil
	}
	if way == liveAsk {
		w.Header().Set(harvest.UsageWantedHeader, harvest.UsageWanted)
		return nil
	}
	return func() { h.readLiveUsage(ctx, client, key, externalID, path) }
}

// readLiveUsage reads the conversation's transcript and refreshes its ledger,
// in the background. The request's context ends when the hook answers, so the
// read keeps only its values, and the write gets a timeout of its own.
func (h *Handler) readLiveUsage(ctx context.Context, client Client, key, externalID, path string) {
	ctx = context.WithoutCancel(ctx)
	h.live.wg.Go(func() {
		defer h.live.done(key)
		_, rows, ok := claudeTranscriptUsage(path)
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(ctx, usageLiveTimeout)
		defer cancel()
		h.refreshUsage(ctx, client, externalID, rows)
	})
}

// refreshUsage writes a live conversation's rows
// (store.RefreshConversationUsage). ok is false when the write failed, which
// it records.
func (h *Handler) refreshUsage(ctx context.Context, client Client, externalID string, rows []core.UsageBucket) (got store.UsageRefresh, ok bool) {
	got, err := store.RefreshConversationUsage(ctx, h.db, client.externalIdentity(), externalID, rows, time.Now().UTC())
	if err != nil {
		h.recordHookError(ctx, "prompt-usage-harvest", client, err, "external_session_id", externalID)
		return got, false
	}
	return got, true
}

// Wait blocks until every mid-session read the daemon started has finished.
func (h *Handler) Wait() { h.live.wg.Wait() }

// WaitFor is Wait for at most d, reporting whether they all finished: serve
// gives the reads under way this long before the database closes.
func (h *Handler) WaitFor(d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		h.live.wg.Wait()
		close(done)
	}()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		h.logger.Warn("hooks: a mid-session usage read was still running at shutdown", "waited", d)
		return false
	}
}

// usageNotAsked answers every push the route does not take for a session:
// one the caller may not act on reads exactly like one never asked.
const usageNotAsked = "no session of this client's session id was asked for its usage"

// writeUsageAnswer writes usagePush's JSON answer.
func writeUsageAnswer(w http.ResponseWriter, status int, a harvest.UsagePushAnswer) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	//nolint:errcheck // the status is committed; a failed write means the client hung up
	_ = json.NewEncoder(w).Encode(a)
}

// usagePush serves POST harvest.UsageEndpoint: a live Claude Code session's
// usage rows, as `seam hook-usage` sent them under peer_auth because the
// answer to its prompt asked. Like every pushed capture it is the caller's
// own claim, taken only through the fences (see the file header): only on
// the peer_auth socket (this daemon's, or a paired relay's), only while the
// Diary is on, and only for a session the caller may act on (reachOK) whose
// ask is still open, once. Anything else writes nothing; every session it
// refuses answers alike.
func (h *Handler) usagePush(w http.ResponseWriter, r *http.Request) {
	if !verifyBearer(r, h.apiKey) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	client, ok := requireRequestClient(w, r)
	if !ok {
		return
	}
	if !access.PeerAuthenticated(r.Context()) {
		writeUsageAnswer(w, http.StatusForbidden, harvest.UsagePushAnswer{
			Error: "a session's usage is sent only over the peer_auth socket; on this server the daemon reads it itself"})
		return
	}
	if h.db == nil {
		writeUsageAnswer(w, http.StatusServiceUnavailable, harvest.UsagePushAnswer{Error: "this daemon has no database"})
		return
	}
	if client != ClientClaudeCode {
		writeUsageAnswer(w, http.StatusBadRequest, harvest.UsagePushAnswer{Error: "a Codex session's usage is not pushed: " +
			"its rollout keeps one running total, which each Stop already credits as it grows"})
		return
	}
	body := readHookBody(w, r)
	var in struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		writeUsageAnswer(w, http.StatusBadRequest, harvest.UsagePushAnswer{Error: "the body must be one JSON object: " +
			"session_id, and the session's usage under " + harvest.Field})
		return
	}
	push, pushed, err := harvest.DecodePush(body)
	switch {
	case err != nil:
		writeUsageAnswer(w, http.StatusBadRequest, harvest.UsagePushAnswer{Error: err.Error()})
		return
	case !pushed || push.Tokens == nil || len(push.Usage) == 0:
		writeUsageAnswer(w, http.StatusBadRequest, harvest.UsagePushAnswer{Error: "a push carries the session's usage rows " +
			"and the totals they sum to, under " + harvest.Field})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), hookTimeout)
	defer cancel()
	externalID := namespacedExternalID(r, strings.TrimSpace(in.SessionID))
	if externalID == "" || !h.featureOn(ctx, features.Diary) {
		writeUsageAnswer(w, http.StatusNotFound, harvest.UsagePushAnswer{Error: usageNotAsked})
		return
	}
	// The reach first: a caller that may not act on the session never closes
	// its ask.
	if reach, _ := h.sessionReachOf(ctx, client, externalID); reach != reachOK || !h.live.answer(liveKey(client, externalID), h.now()) {
		writeUsageAnswer(w, http.StatusNotFound, harvest.UsagePushAnswer{Error: usageNotAsked})
		return
	}
	got, ok := h.refreshUsage(ctx, client, externalID, push.Usage)
	switch {
	case !ok:
		writeUsageAnswer(w, http.StatusServiceUnavailable, harvest.UsagePushAnswer{Error: "the session's ledger could not be written"})
	case got == store.RefreshWritten:
		writeUsageAnswer(w, http.StatusOK, harvest.UsagePushAnswer{Result: harvest.UsageWritten})
	case got == store.RefreshUnchanged:
		writeUsageAnswer(w, http.StatusOK, harvest.UsagePushAnswer{Result: harvest.UsageUnchanged})
	case got == store.RefreshBehind:
		writeUsageAnswer(w, http.StatusOK, harvest.UsagePushAnswer{Result: harvest.UsageBehind})
	default:
		writeUsageAnswer(w, http.StatusNotFound, harvest.UsagePushAnswer{Error: usageNotAsked})
	}
}
