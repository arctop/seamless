package hooks

// Owed usage at the hooks (store usage_owed.go). A Claude Code conversation
// whose session ended without its SessionEnd harvest -- the agent closed it
// with session_end, or the idle reaper expired it -- owes the turns since its
// ledger's last write, and its SessionEnd may come late or never (the Claude
// app runs none for a session it stops). With peer_auth off the gardener's
// upkeep reads the transcript itself (internal/gardener owed_usage.go). On
// the peer_auth socket the daemon opens no file a hook named, so the same
// caller's later hooks are asked for it instead, as a pending transcript is
// (transcript.go keepPending):
//
//   - At the caller's next Claude Code SessionStart and SessionEnd
//     (askOwedUsage), for the conversations of the same agent client, account
//     and computer that the caller may act on (reachOK), save its own: the
//     answer carries harvest.UsageOwedHeader, naming the conversation and the
//     transcript its hooks recorded, for at most harvest.MaxPendingPerAnswer
//     of them, each with a growing wait between asks (usageOwedWait).
//   - `seam hook-usage --owed` answers at usagePush: the rows, and when the
//     transcript last changed. The push is taken only for a conversation
//     asked about within usageOwedAnswerWindow, whether or not the Diary is on
//     (the SessionEnd harvest it stands in for never asks), and written with
//     store.RefreshConversationUsage, the rule of every Claude Code harvest.
//     An answer that brought a change starts the asks over; one that did not
//     leaves them counting, and after usageOwedAskLimit asks with no change
//     (or no answer: an older seam ignores the header) the owed usage is
//     dropped. A report that the path is refused or gone drops it too.
//   - The TCP key's hook under peer_auth pushes nothing, so nothing asks it;
//     the upkeep drops what nobody can read at store.UsageOwedHorizon.
//
// A SessionEnd harvest settles the conversation it names
// (harvestEndUsage): it read the whole transcript.

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/harvest"
	"github.com/arctop/seamless/internal/store"
)

// usageOwedBackoff is the wait after an owed conversation's first ask before
// the next; each later ask waits twice the one before (usageOwedWait).
const usageOwedBackoff = 2 * time.Minute

// usageOwedAskLimit is how many asks in a row an owed conversation gets with
// no change brought: about eight and a half hours of waits, enough for a
// conversation that ended on its row and goes on after a break.
const usageOwedAskLimit = 8

// usageOwedAnswerWindow is how long after an ask a push answers it: the
// detached sender gives up at harvest.UsagePushTimeout.
const usageOwedAnswerWindow = 2 * harvest.UsagePushTimeout

// usageOwedWait is how long after its asks-th ask an owed conversation waits
// before it is asked again: usageOwedBackoff after an answer that brought a
// change (asks 0), doubling with each ask since.
func usageOwedWait(asks int) time.Duration {
	return usageOwedBackoff << min(max(asks-1, 0), 16)
}

// askOwedUsage asks a Claude Code SessionStart's or SessionEnd's answer for
// the caller's owed usage, on the peer_auth socket only (see the file
// header); current is the conversation the hook itself names, whose own end
// harvests it.
func (h *Handler) askOwedUsage(ctx context.Context, w http.ResponseWriter, client Client, host, current string) {
	if h.db == nil || client != ClientClaudeCode || !access.PeerAuthenticated(ctx) {
		return // with peer_auth off the upkeep reads it; the TCP key's hook sends nothing
	}
	owed, err := store.OwedUsagesFor(ctx, h.db, client.externalIdentity(), access.AccountOf(ctx))
	if err != nil {
		h.recordHookError(ctx, "usage-owed", client, err)
		return
	}
	ns := access.SessionNamespace(ctx)
	host = strings.ToLower(strings.TrimSpace(host))
	now := h.now()
	asked := 0
	for _, o := range owed {
		if asked == harvest.MaxPendingPerAnswer {
			return
		}
		sess := o.Session
		if sess.ExternalSessionID == current || sess.Status == core.SessionActive || sess.TranscriptPath == "" ||
			h.hostOf(hookIdentity{Host: sess.Host}) != host {
			continue
		}
		if o.AskedAt != nil && now.Before(o.AskedAt.Add(usageOwedWait(o.Asks))) {
			continue
		}
		if o.Asks >= usageOwedAskLimit {
			h.clearUsageOwed(ctx, client, sess.ID)
			continue
		}
		if reach, _ := h.sessionReachOf(ctx, client, sess.ExternalSessionID); reach != reachOK {
			continue // a locked project's session takes no counts before the owner's approval
		}
		if _, recorded, err := store.RecordUsageOwedAsk(ctx, h.db, sess.ID, now); err != nil {
			h.recordHookError(ctx, "usage-owed-ask", client, err, "session", sess.ID)
			continue
		} else if !recorded {
			continue
		}
		w.Header().Add(harvest.UsageOwedHeader, url.Values{
			harvest.TranscriptSessionParam: {strings.TrimPrefix(sess.ExternalSessionID, ns)},
			harvest.TranscriptPathParam:    {sess.TranscriptPath},
		}.Encode())
		asked++
	}
}

// clearUsageOwed forgets a conversation's owed usage, recording a failure.
func (h *Handler) clearUsageOwed(ctx context.Context, client Client, id string) {
	if err := store.ClearUsageOwed(ctx, h.db, id); err != nil {
		h.recordHookError(ctx, "usage-owed-clear", client, err, "session", id)
	}
}

// owedPush is an owed answer's fields beside the rows (harvest.UsageOwedField).
type owedPush struct {
	Owed      bool   `json:"owed"`
	ChangedAt string `json:"changed_at"`
	Refused   string `json:"refused"`
}

// usageOwedPush takes an answer to an owed ask (usagePush, the socket, a
// Claude Code session the caller may act on, decoded): the asked conversation
// externalID's rows, or the report that its path was not read. push is nil
// for a report.
func (h *Handler) usageOwedPush(ctx context.Context, w http.ResponseWriter, client Client, externalID string,
	in owedPush, push *harvest.Push) {
	o, found, err := store.OwedUsageOf(ctx, h.db, client.externalIdentity(), externalID)
	if err != nil {
		h.recordHookError(ctx, "usage-owed", client, err, "external_session_id", externalID)
		writeUsageAnswer(w, http.StatusServiceUnavailable, harvest.UsagePushAnswer{Error: "the session could not be looked up"})
		return
	}
	now := h.now()
	if !found || o.AskedAt == nil || now.Sub(*o.AskedAt) >= usageOwedAnswerWindow {
		writeUsageAnswer(w, http.StatusNotFound, harvest.UsagePushAnswer{Error: usageNotAsked})
		return
	}
	if push == nil {
		switch in.Refused {
		case harvest.TranscriptRefusedPath, harvest.TranscriptRefusedMissing:
			h.clearUsageOwed(ctx, client, o.Session.ID)
			writeUsageAnswer(w, http.StatusOK, harvest.UsagePushAnswer{Result: harvest.UsageSettled})
		default:
			writeUsageAnswer(w, http.StatusBadRequest, harvest.UsagePushAnswer{Error: "invalid " + harvest.UsageRefusedField +
				" " + strconv.Quote(in.Refused) + ": valid values are " + harvest.TranscriptRefusedPath + ", " +
				harvest.TranscriptRefusedMissing})
		}
		return
	}
	changed, err := time.Parse(time.RFC3339Nano, in.ChangedAt)
	if err != nil {
		writeUsageAnswer(w, http.StatusBadRequest, harvest.UsagePushAnswer{Error: "an owed push names when the transcript " +
			"last changed, under " + harvest.UsageChangedField + " (RFC 3339)"})
		return
	}
	got, ok := h.refreshUsage(ctx, client, externalID, push.Usage)
	if !ok {
		writeUsageAnswer(w, http.StatusServiceUnavailable, harvest.UsagePushAnswer{Error: "the session's ledger could not be written"})
		return
	}
	changed = changed.UTC()
	switch {
	case now.Sub(changed) >= store.UsageOwedHorizon:
		h.clearUsageOwed(ctx, client, o.Session.ID) // nothing more is coming
	case o.ChangedAt == nil || changed.After(*o.ChangedAt):
		if _, err := store.RecordUsageOwedRead(ctx, h.db, o.Session.ID, changed); err != nil {
			h.recordHookError(ctx, "usage-owed-read", client, err, "session", o.Session.ID)
		}
	}
	switch got {
	case store.RefreshWritten:
		writeUsageAnswer(w, http.StatusOK, harvest.UsagePushAnswer{Result: harvest.UsageWritten})
	case store.RefreshUnchanged:
		writeUsageAnswer(w, http.StatusOK, harvest.UsagePushAnswer{Result: harvest.UsageUnchanged})
	case store.RefreshBehind:
		writeUsageAnswer(w, http.StatusOK, harvest.UsagePushAnswer{Result: harvest.UsageBehind})
	default:
		writeUsageAnswer(w, http.StatusNotFound, harvest.UsagePushAnswer{Error: usageNotAsked})
	}
}
