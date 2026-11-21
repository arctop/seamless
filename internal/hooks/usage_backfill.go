package hooks

// The usage backfill's route (harvest.UsageBackfillEndpoint, `seam usage
// backfill`): the owner fills the usage ledger of the sessions whose Claude
// Code transcripts are still on the computer the request speaks for -- the
// days before the ledger existed, and the sessions whose end never harvested
// them. The client reads every file, with the daemon's own parser
// (harvest.ClaudeUsage, the subagents' transcripts included), and sends the
// rows; the daemon opens nothing. GET lists the sessions and POST fills one,
// and both are the owner's alone: a restricted local account is refused
// before anything is read (usageBackfillRefusal).
//
// A fill writes what a SessionEnd's pushed usage writes, through the same
// fences: the conversation's ambient row (and the sessions its moves ended),
// only where the caller may act on the session it names (sessionReachOf,
// whose rule refuses another principal's session, a session in a project the
// caller may not write, and another tree's leased one), and only rows that
// validate (harvest.Push.Validate). On top of them:
//
//   - only a session recorded on the computer the request speaks for: its
//     transcript is there and nowhere else (a paired device speaks for its
//     own enrolled name, whatever it sends);
//   - never a live session: its own end harvests it, and a write now would
//     move its updated_at, the heartbeat that keeps an idle session alive;
//   - only a Claude Code conversation: a Codex rollout keeps one running
//     total, which each Stop already credits as it grows;
//   - nothing when the ledger already holds exactly those rows, so a second
//     run writes nothing.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/harvest"
	"github.com/arctop/seamless/internal/store"
)

// usageBackfillOwnerRefusal answers a restricted local account at either
// half of the route, with nothing read.
const usageBackfillOwnerRefusal = "the usage backfill is the owner's: a restricted account's sessions take " +
	"their token usage from its own hooks, at each session's end"

// usageBackfillRefusal is why the request in ctx may not use the usage
// backfill, "" when it may: every caller but a restricted local account --
// the key, the owner's socket, and a paired device or a relay's owner, each of
// which speaks for its own computer only.
func usageBackfillRefusal(ctx context.Context) string {
	if access.AccountOf(ctx) != "" {
		return usageBackfillOwnerRefusal
	}
	return ""
}

// writeBackfillJSON writes one of the route's JSON answers.
func writeBackfillJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	//nolint:errcheck // the status is committed; a failed write means the client hung up
	_ = json.NewEncoder(w).Encode(v)
}

// usageBackfillList serves GET harvest.UsageBackfillEndpoint: the caller's
// conversations recorded on the computer the request speaks for, with the
// transcript path their hooks recorded and their totals now. A session in a
// project the request does not read (locked) is left out, its project named.
func (h *Handler) usageBackfillList(w http.ResponseWriter, r *http.Request) {
	if !verifyBearer(r, h.apiKey) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	ctx := r.Context()
	if refusal := usageBackfillRefusal(ctx); refusal != "" {
		writeBackfillJSON(w, http.StatusForbidden, harvest.BackfillResult{Error: refusal})
		return
	}
	if _, err := url.ParseQuery(r.URL.RawQuery); err != nil {
		writeBackfillJSON(w, http.StatusBadRequest, harvest.BackfillResult{Error: "invalid query: " + err.Error()})
		return
	}
	if h.db == nil {
		writeBackfillJSON(w, http.StatusServiceUnavailable, harvest.BackfillResult{Error: "this daemon has no database"})
		return
	}
	sessions, err := store.AmbientSessionsOf(ctx, h.db, access.AccountOf(ctx))
	if err != nil {
		h.recordHookError(ctx, "usage-backfill-list", "", err)
		writeBackfillJSON(w, http.StatusServiceUnavailable, harvest.BackfillResult{Error: "the sessions could not be read"})
		return
	}
	out := harvest.BackfillList{Host: h.hostOf(identityFromRequest(r)), Sessions: []harvest.BackfillSession{}}
	for _, s := range sessions {
		switch {
		case !access.Visible(ctx, s.ProjectSlug):
			if !slices.Contains(out.Locked, s.ProjectSlug) {
				out.Locked = append(out.Locked, s.ProjectSlug)
			}
		case !h.recordedOn(s, out.Host):
			out.OtherHosts++
		default:
			out.Sessions = append(out.Sessions, harvest.BackfillSession{
				ID: s.ID, Name: s.Name, Project: s.ProjectSlug, Client: s.ExternalClient, SessionID: s.ExternalSessionID,
				Status: string(s.Status), TranscriptPath: s.TranscriptPath, Tokens: s.Tokens, Created: s.CreatedAt,
			})
		}
	}
	slices.Sort(out.Locked)
	writeBackfillJSON(w, http.StatusOK, out)
}

// recordedOn reports whether sess was recorded on host (recordedHost).
func (h *Handler) recordedOn(sess core.Session, host string) bool {
	return h.recordedHost(sess) == host
}

// usageBackfillFill serves POST harvest.UsageBackfillEndpoint: one
// conversation's rows, read by the client from its transcripts, in place of
// those its ledger holds (see the file header for what it refuses). With
// dry_run it writes nothing and answers what it would write.
func (h *Handler) usageBackfillFill(w http.ResponseWriter, r *http.Request) {
	if !verifyBearer(r, h.apiKey) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	client, ok := requireRequestClient(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	if refusal := usageBackfillRefusal(ctx); refusal != "" {
		writeBackfillJSON(w, http.StatusForbidden, harvest.BackfillResult{Error: refusal})
		return
	}
	if h.db == nil {
		writeBackfillJSON(w, http.StatusServiceUnavailable, harvest.BackfillResult{Error: "this daemon has no database"})
		return
	}
	body := readHookBody(w, r)
	var in struct {
		SessionID string `json:"session_id"`
		DryRun    bool   `json:"dry_run"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		writeBackfillJSON(w, http.StatusBadRequest, harvest.BackfillResult{Error: "the body must be one JSON object: " +
			"session_id, dry_run, and the session's usage under " + harvest.Field})
		return
	}
	push, pushed, err := harvest.DecodePush(body)
	switch {
	case err != nil:
		writeBackfillJSON(w, http.StatusBadRequest, harvest.BackfillResult{Error: err.Error()})
		return
	case !pushed || push.Tokens == nil || len(push.Usage) == 0:
		writeBackfillJSON(w, http.StatusBadRequest, harvest.BackfillResult{Error: "a fill carries the session's usage rows " +
			"and the totals they sum to, under " + harvest.Field})
		return
	case client == ClientCodex:
		writeBackfillJSON(w, http.StatusBadRequest, harvest.BackfillResult{Error: "a Codex session is not backfilled: its " +
			"rollout keeps one running total, which each Stop already credits as it grows"})
		return
	}
	externalID := namespacedExternalID(r, strings.TrimSpace(in.SessionID))
	if externalID == "" {
		writeBackfillJSON(w, http.StatusBadRequest, harvest.BackfillResult{Error: "session_id names no session"})
		return
	}
	h.fillUsage(ctx, w, client, externalID, h.hostOf(identityFromRequest(r)), push, in.DryRun)
}

// fillUsage is a fill once the request holds: externalID's conversation takes
// push's rows, where it may.
func (h *Handler) fillUsage(ctx context.Context, w http.ResponseWriter, client Client, externalID, host string, push harvest.Push, dryRun bool) {
	notFound := harvest.BackfillResult{Error: "no session of this client's session id is this caller's to fill"}
	reach, lockedProject := h.sessionReachOf(ctx, client, externalID)
	amb, found, err := store.AmbientSessionByExternalIdentity(ctx, h.db, client.externalIdentity(), externalID)
	switch {
	case err != nil:
		h.recordHookError(ctx, "usage-backfill-lookup", client, err)
		writeBackfillJSON(w, http.StatusServiceUnavailable, harvest.BackfillResult{Error: "the session could not be looked up"})
		return
	case !found || reach == reachForeign:
		// Another principal's session, or one out of the caller's reach,
		// reads exactly like one that does not exist.
		writeBackfillJSON(w, http.StatusNotFound, notFound)
		return
	case reach == reachLocked:
		writeBackfillJSON(w, http.StatusForbidden, harvest.BackfillResult{
			Error: captureRefusedNotice("this session's token usage", lockedProject)})
		return
	case !h.recordedOn(amb, host):
		writeBackfillJSON(w, http.StatusConflict, harvest.BackfillResult{Error: "session " + amb.ID + " was recorded on " +
			h.recordedHost(amb) + ": its transcript is there, and a fill comes from that computer"})
		return
	case amb.Status == core.SessionActive:
		writeBackfillJSON(w, http.StatusOK, harvest.BackfillResult{Result: harvest.BackfillActive, Before: amb.Tokens, After: amb.Tokens})
		return
	}
	held, found, err := store.ConversationUsageOf(ctx, h.db, client.externalIdentity(), externalID, push.Usage)
	switch {
	case errors.Is(err, store.ErrInvalidUsage):
		writeBackfillJSON(w, http.StatusBadRequest, harvest.BackfillResult{Error: err.Error()})
		return
	case err != nil:
		h.recordHookError(ctx, "usage-backfill-read", client, err)
		writeBackfillJSON(w, http.StatusServiceUnavailable, harvest.BackfillResult{Error: "the session's ledger could not be read"})
		return
	case !found:
		writeBackfillJSON(w, http.StatusNotFound, notFound)
		return
	case held.Holds:
		writeBackfillJSON(w, http.StatusOK, harvest.BackfillResult{Result: harvest.BackfillUnchanged, Before: held.Totals, After: held.Totals})
		return
	case dryRun:
		writeBackfillJSON(w, http.StatusOK, harvest.BackfillResult{Result: harvest.BackfillWouldWrite, Before: held.Totals, After: *push.Tokens})
		return
	}
	// A session that turned live since the read above (a resume) takes the
	// write as a heartbeat it has just earned.
	if found, err = store.ReplaceConversationUsage(ctx, h.db, client.externalIdentity(), externalID, push.Usage, time.Now().UTC()); err != nil {
		h.recordHookError(ctx, "usage-backfill-write", client, err, "session", amb.ID)
		writeBackfillJSON(w, http.StatusServiceUnavailable, harvest.BackfillResult{Error: "the session's ledger could not be written"})
		return
	}
	if !found {
		writeBackfillJSON(w, http.StatusNotFound, notFound)
		return
	}
	h.logger.Info("hooks: usage backfill filled a session", "session", amb.ID,
		"before_tokens", held.Totals.Total, "after_tokens", push.Tokens.Total, "rows", len(push.Usage))
	writeBackfillJSON(w, http.StatusOK, harvest.BackfillResult{Result: harvest.BackfillWritten, Before: held.Totals, After: *push.Tokens})
}

// recordedHost is the computer sess was recorded on: its host, or for a row
// from before sessions named their computer, this daemon's.
func (h *Handler) recordedHost(sess core.Session) string {
	if host := strings.ToLower(strings.TrimSpace(sess.Host)); host != "" {
		return host
	}
	return h.localHost
}
