package console

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/console/browserproof"
	"github.com/arctop/seamless/internal/core"
)

// chStreamRows opens the event stream at target on lc's console as the
// owner's browser (with its console unlock when unlock is set), records each
// event once the stream is open, and returns the rows that reached the page,
// in order, each as its JSON object. The last event recorded is sentinel, a
// kind the stream carries: once it lands, every event before it has either
// arrived or been dropped.
func chStreamRows(t *testing.T, lc *lockedConsole, target, unlock string, evs []core.Event, sentinel core.Event) []map[string]any {
	t.Helper()
	srv := httptest.NewServer(lc.mux)
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+target, nil)
	require.NoError(t, err)
	req.AddCookie(consoleCookie())
	if unlock != "" {
		req.AddCookie(&http.Cookie{Name: unlockCookieName, Value: unlock})
	}
	proof, err := browserproof.Sign(lc.key, time.Now())
	require.NoError(t, err)
	req.Header.Set(browserproof.Header, proof)
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	lines := make(chan string, 64)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	next := func() string {
		select {
		case l, ok := <-lines:
			require.True(t, ok, "the stream ended early")
			return l
		case <-ctx.Done():
			t.Fatal("no answer from the stream")
			return ""
		}
	}
	require.Equal(t, ": connected", next(), "the stream subscribes before it says so")

	for _, e := range append(evs, sentinel) {
		_, err := lc.rec.Record(context.Background(), e)
		require.NoError(t, err)
	}
	var rows []map[string]any
	for {
		l, ok := strings.CutPrefix(next(), "data: ")
		if !ok {
			continue
		}
		var row map[string]any
		require.NoError(t, json.Unmarshal([]byte(l), &row))
		rows = append(rows, row)
		if row["kind"] == string(sentinel.Kind) {
			return rows
		}
	}
}

func chKinds(rows []map[string]any) []string {
	var out []string
	for _, r := range rows {
		k, _ := r["kind"].(string)
		out = append(out, k)
	}
	return out
}

// chToastEvents are the three kinds the live toasts name: an access request
// for the locked project, a held write and a mishap in the open one, and,
// between them, events of the locked project no page may see.
func chToastEvents() []core.Event {
	return []core.Event{
		{Kind: core.EventAccessRequested, ProjectSlug: lockedSlug, SessionID: "01M8000000000000000000SESS",
			ItemID: "01M8000000000000000000REQ1", Payload: map[string]any{"kind": "read", "via": "request"}},
		{Kind: core.EventMemoryWritten, ProjectSlug: lockedSlug, ItemID: "01M8000000000000000000MEM1",
			Payload: map[string]any{"name": "ledger-" + lockedSecret}},
		{Kind: core.EventAgentMishap, ProjectSlug: lockedSlug, Payload: map[string]any{"description": "other: " + lockedSecret}},
		{Kind: core.EventMemoryHeld, ProjectSlug: openSlug, Payload: map[string]any{"hold": "x", "op": "write", "item_kind": "memory"}},
		{Kind: core.EventAgentMishap, ProjectSlug: openSlug, Payload: map[string]any{"description": "other: a test mishap"}},
	}
}

// An agent asking for a locked project this browser has not unlocked still
// tells the open page so (the toast, the Access requests count), by the
// request's kind, id and time alone; every other event of that project stays
// off the stream, and the open project's held write and mishap arrive whole.
func TestChStreamSignalsALockedAccessRequestByItsKindAlone(t *testing.T) {
	lc := newLockedConsole(t)
	sentinel := core.Event{Kind: core.EventNoteWritten, ProjectSlug: openSlug, Payload: map[string]any{"slug": "sentinel"}}
	rows := chStreamRows(t, lc, "/console/events", "", chToastEvents(), sentinel)

	require.Equal(t, []string{"access.requested", "memory.held", "agent.mishap", "note.written"}, chKinds(rows),
		"the locked project's write and mishap never reach the page")
	signal := rows[0]
	keys := make([]string, 0, len(signal))
	for k := range signal {
		keys = append(keys, k)
	}
	require.ElementsMatch(t, []string{"id", "ts", "kind"}, keys, "no project, session, item or summary: %v", signal)
	require.NotEmpty(t, signal["id"])
	for _, r := range rows[1:] {
		require.Equal(t, openSlug, r["project"], "an open project's event is the whole row")
		require.NotEmpty(t, r["summary"])
	}
	for _, r := range rows {
		raw, err := json.Marshal(r)
		require.NoError(t, err)
		require.NotContains(t, string(raw), lockedSlug)
		require.NotContains(t, string(raw), lockedSecret)
	}

	// The Interactions feed (the Now screen's) is sent nothing of it.
	start := core.Event{Kind: core.EventSessionStarted, ProjectSlug: openSlug}
	feed := chStreamRows(t, lc, "/console/events?feed=interactions", "", chToastEvents(), start)
	require.Equal(t, []string{"session.started"}, chKinds(feed))
}

// Once this browser unlocks the project, its access request flows as the
// whole row again, as does the rest of the project's activity.
func TestChStreamUnlockedAccessRequestIsTheWholeRow(t *testing.T) {
	lc := newLockedConsole(t)
	unlock := lc.unlockByTerminal(t)
	sentinel := core.Event{Kind: core.EventNoteWritten, ProjectSlug: openSlug, Payload: map[string]any{"slug": "sentinel"}}
	rows := chStreamRows(t, lc, "/console/events", unlock, chToastEvents(), sentinel)

	require.Equal(t, []string{"access.requested", "memory.written", "agent.mishap", "memory.held", "agent.mishap", "note.written"}, chKinds(rows))
	require.Equal(t, lockedSlug, rows[0]["project"])
	require.Equal(t, "01M8000000000000000000REQ1", rows[0]["itemId"])
	require.Contains(t, rows[0], "summary")
}
