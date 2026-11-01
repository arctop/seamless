package feed

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// The content the events below carry, in every place an event can carry
// content. None of it may reach a frame.
var contentMarkers = []string{
	"SECRET-BODY-TEXT", "SECRET-PROMPT-TEXT", "SECRET-QUERY-TEXT", "SECRET-ARG-TEXT", "SECRET-RESULT-TEXT",
	"SECRET-FINDINGS-TEXT", "SECRET-NAME-TEXT", "SECRET-TITLE-TEXT", "SECRET-ERROR-TEXT", "SECRET-DESCRIPTION-TEXT",
	"vault-project",
}

// contentEvents is one event of every shape that carries content in its
// payload, plus the part-2 events.
func contentEvents() []core.Event {
	ts := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	id := func(n int) string { return fmt.Sprintf("01KX%022d", n) }
	return []core.Event{
		{ID: id(1), TS: ts, Kind: core.EventToolCall, SessionID: "S1", ProjectSlug: "demo", Payload: map[string]any{
			"tool": "memory_write", "duration_ms": 12, "args": map[string]any{"body": "SECRET-ARG-TEXT"},
			"result": "SECRET-RESULT-TEXT", "is_error": true, "error": "SECRET-ERROR-TEXT"}},
		{ID: id(2), TS: ts, Kind: core.EventInjected, SessionID: "S1", ProjectSlug: "vault-project", Payload: map[string]any{
			"hook": "session-start", "content": "SECRET-BODY-TEXT", "prompt": "SECRET-PROMPT-TEXT",
			"item_ids": []any{id(10), id(11), "not-an-id SECRET-NAME-TEXT"}, "original_item_count": 3, "truncated": false,
			"emitted_estimated_tokens": 420, "claude_session_id": "SECRET-ARG-TEXT"}},
		{ID: id(3), TS: ts, Kind: core.EventHookPrompt, Payload: map[string]any{"prompt": "SECRET-PROMPT-TEXT", "matched": false}},
		{ID: id(4), TS: ts, Kind: core.EventRecallMiss, Payload: map[string]any{"query": "SECRET-QUERY-TEXT", "scope": "all", "limit": 10}},
		{ID: id(5), TS: ts, Kind: core.EventMemoryWritten, ItemID: id(12), Payload: map[string]any{
			"name": "SECRET-NAME-TEXT", "kind": "constraint", "updated": true}},
		{ID: id(6), TS: ts, Kind: core.EventNoteWritten, ItemID: id(13), Payload: map[string]any{"title": "SECRET-TITLE-TEXT"}},
		{ID: id(7), TS: ts, Kind: core.EventSessionEnded, SessionID: "S1", Payload: map[string]any{"findings": "SECRET-FINDINGS-TEXT"}},
		{ID: id(8), TS: ts, Kind: core.EventAgentMishap, Payload: map[string]any{"description": "SECRET-DESCRIPTION-TEXT"}},
		{ID: id(9), TS: ts, Kind: core.EventMemoryScreened, ItemID: id(14), Payload: map[string]any{
			"item_kind": "memory", "kind": "gotcha", "tool": "memory_write", "size": 120,
			"verdicts": []any{"instruction.override", "SECRET BODY TEXT not a code"}, "held": true}},
		{ID: id(20), TS: ts, Kind: core.EventAccessRefused, ProjectSlug: "demo", Payload: map[string]any{
			"reason": "fence.write", "surface": "mcp", "tool": "memory_write", "target": "vault-project", "suppressed": 3}},
		{ID: id(21), TS: ts, Kind: core.EventCorpusDrift, ItemID: id(15), ProjectSlug: "vault-project", Payload: map[string]any{
			"change": "changed", "item_kind": "memory", "verdicts": []any{"secret.github_token"}, "quarantined": true}},
		{ID: id(22), TS: ts, Kind: core.EventLockdownChanged, Payload: map[string]any{"on": true, "source": "file", "by": "file"}},
		{ID: id(23), TS: ts, Kind: core.EventFeaturesChanged, Payload: map[string]any{
			"features": map[string]any{"screening": true, "research": false}, "by": "console"}},
		{ID: id(24), TS: ts, Kind: core.EventHookError, Payload: map[string]any{"stage": "x", "error": "SECRET-ERROR-TEXT"}},
		{ID: id(25), TS: ts, Kind: core.EventSessionTainted, SessionID: "S1", Payload: map[string]any{"reason": "SECRET PROMPT TEXT", "via": "hook"}},
	}
}

func testMapper() *mapper {
	return &mapper{host: "testhost", key: bytes.Repeat([]byte{7}, 32),
		hashed: func(p string) bool { return p == "vault-project" }}
}

// The acceptance test: every frame passes the contract's rules, and no frame
// carries any of the content its event did -- bodies, prompts, queries,
// arguments, results, findings, names, titles, error text -- or an isolated
// project's slug in the clear.
func TestFrames_CarryNoContent(t *testing.T) {
	m := testMapper()
	var all strings.Builder
	for _, e := range contentEvents() {
		f := m.frame(e, session{name: "cc/ab12cd34", agentProcess: "4242.1730000000"})
		require.NoError(t, f.validate(), "%s", e.Kind)
		raw, err := json.Marshal(f)
		require.NoError(t, err)
		all.Write(raw)
		all.WriteByte('\n')
		for name := range f.Fields {
			require.False(t, contentShaped(name), "%s: field %q is content-shaped", e.Kind, name)
		}
	}
	text := all.String()
	for _, marker := range contentMarkers {
		require.NotContains(t, text, marker, "a frame carried content")
	}
	// The metadata that should be there is.
	require.Contains(t, text, `"verdicts":"instruction.override"`)
	require.Contains(t, text, `"item_ids":"01KX0000000000000000000010,01KX0000000000000000000011"`)
	require.Contains(t, text, `"target_project":"h:`)
	require.Contains(t, text, `"session":"cc/ab12cd34"`)
	require.Contains(t, text, `"feature_screening":true`)
}

func TestFrames_Shapes(t *testing.T) {
	m := testMapper()
	byKind := map[core.EventKind]Frame{}
	for _, e := range contentEvents() {
		byKind[e.Kind] = m.frame(e, session{})
	}
	require.Equal(t, "auth", byKind[core.EventAccessRefused].Class)
	require.Equal(t, "blocked", byKind[core.EventAccessRefused].Outcome)
	require.Equal(t, "file", byKind[core.EventCorpusDrift].Class)
	require.Equal(t, "high", byKind[core.EventCorpusDrift].Severity)
	require.Equal(t, "failure", byKind[core.EventToolCall].Outcome)
	require.Equal(t, "blocked", byKind[core.EventMemoryScreened].Outcome)
	require.Equal(t, "memory_screened", byKind[core.EventMemoryScreened].Activity)
	require.Equal(t, "user", byKind[core.EventFeaturesChanged].Actor.Kind)
	require.True(t, strings.HasPrefix(byKind[core.EventInjected].Target.Asset, "h:"), "an isolated project leaves as a keyed hash")
	require.Equal(t, "demo", byKind[core.EventToolCall].Target.Asset)
	require.Equal(t, "global", byKind[core.EventHookPrompt].Target.Asset)
	hb := m.heartbeat(time.Now(), 100*time.Second, SpoolStats{Events: 3})
	require.NoError(t, hb.validate())
	require.Equal(t, "health", hb.Class)
}

func TestContentShaped(t *testing.T) {
	for _, bad := range []string{"body", "prompt", "args", "result", "message_text", "raw", "content_preview"} {
		require.True(t, contentShaped(bad), bad)
	}
	for _, ok := range []string{"body_size", "item_ids", "prompt_tokens", "content_hash", "verdicts", "size", "message_id"} {
		require.False(t, contentShaped(ok), ok)
	}
}

// ---------------------------------------------------------------------------
// The spool
// ---------------------------------------------------------------------------

func spoolDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "spool")
	return dir
}

func hbFrame(n int) Frame {
	return Frame{Time: "2026-11-01T12:00:00.000Z", Host: "h", Class: "agent", Activity: "a", Severity: "info",
		Outcome: "success", Fields: map[string]any{"n": n}}
}

func TestSpool_BatchResentUnchangedUntilSettled(t *testing.T) {
	dir := spoolDir(t)
	sp, err := openSpool(dir)
	require.NoError(t, err)
	for i := range 3 {
		ok, err := sp.add(hbFrame(i))
		require.NoError(t, err)
		require.True(t, ok)
	}
	b1, err := sp.next()
	require.NoError(t, err)
	require.Equal(t, 3, b1.frames)
	// A retry keeps it; a restart keeps it, byte for byte.
	act, err := sp.settle(Receipt{Action: ActionRetry})
	require.NoError(t, err)
	require.Equal(t, ActionRetry, act)
	sp.close()
	sp, err = openSpool(dir)
	require.NoError(t, err)
	defer sp.close()
	b2, err := sp.next()
	require.NoError(t, err)
	require.Equal(t, b1.id, b2.id)
	require.Equal(t, b1.body, b2.body)
	head := strings.SplitN(string(b2.body), "\n", 2)[0]
	require.Equal(t, `{"v":1,"batch":"`+b1.id+`","frames":3,"dropped":0}`, head)
	act, err = sp.settle(Receipt{Action: ActionSettle})
	require.NoError(t, err)
	require.Equal(t, ActionSettle, act)
	b3, err := sp.next()
	require.NoError(t, err)
	require.Nil(t, b3)
}

func TestSpool_DropsCountedAndReported(t *testing.T) {
	sp, err := openSpool(spoolDir(t))
	require.NoError(t, err)
	defer sp.close()
	big := hbFrame(0)
	big.Fields["pad"] = strings.Repeat("x", spoolFrameBytes)
	ok, err := sp.add(big)
	require.NoError(t, err)
	require.False(t, ok, "a frame longer than frameBytes is never spooled")
	require.Equal(t, 1, sp.stats().Dropped)
	_, err = sp.add(hbFrame(1))
	require.NoError(t, err)
	b, err := sp.next()
	require.NoError(t, err)
	require.Equal(t, 1, b.dropped, "the header reports the drops")
	// A discarded batch's frames count as dropped; a settle takes off what the
	// consumer recorded.
	act, err := sp.settle(Receipt{Action: ActionDiscard, Rule: "frames"})
	require.NoError(t, err)
	require.Equal(t, ActionDiscard, act)
	require.Equal(t, 2, sp.stats().Dropped)
	_, err = sp.add(hbFrame(2))
	require.NoError(t, err)
	b, err = sp.next()
	require.NoError(t, err)
	require.Equal(t, 2, b.dropped)
	_, err = sp.settle(Receipt{Action: ActionSettle, Dropped: 2})
	require.NoError(t, err)
	require.Equal(t, 0, sp.stats().Dropped)
}

func TestSpool_BoundedOldestDroppedFirst(t *testing.T) {
	sp, err := openSpool(spoolDir(t))
	require.NoError(t, err)
	defer sp.close()
	for i := range spoolMaxEvents + spoolSegmentFrames {
		_, err := sp.add(hbFrame(i))
		require.NoError(t, err)
	}
	st := sp.stats()
	require.LessOrEqual(t, st.Events, spoolMaxEvents)
	require.Equal(t, spoolSegmentFrames, st.Dropped, "the oldest segment went whole, and was counted")
}

func TestSpool_HalfLineCutAndLock(t *testing.T) {
	dir := spoolDir(t)
	sp, err := openSpool(dir)
	require.NoError(t, err)
	_, err = sp.add(hbFrame(1))
	require.NoError(t, err)
	_, err = openSpool(dir)
	require.ErrorIs(t, err, errSpoolHeld, "one process holds a spool")
	sp.close()
	seg := filepath.Join(dir, segmentName(1))
	fh, err := os.OpenFile(seg, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = fh.WriteString(`{"half":`)
	require.NoError(t, err)
	require.NoError(t, fh.Close())
	sp, err = openSpool(dir)
	require.NoError(t, err)
	defer sp.close()
	require.Equal(t, 1, sp.stats().Dropped, "a half-written line is cut and counted")
	require.Equal(t, 1, sp.stats().Events)
	info, err := os.Stat(dir)
	require.NoError(t, err)
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("spool folder mode %o reaches group or others", info.Mode().Perm())
	}
}

func TestReadReceipt(t *testing.T) {
	ok := []byte(`{"v":1,"action":"settle","status":"stored","batch":"e.1","frames":2,"accepted":2,"refused_count":0,"refused":[],"dropped":1}`)
	r := readReceipt(200, ok, "e.1")
	require.Equal(t, ActionSettle, r.Action)
	require.Equal(t, 1, r.Dropped)
	require.Equal(t, ActionRetry, readReceipt(200, ok, "e.2").Action, "another batch's receipt is a retry")
	require.Equal(t, ActionRetry, readReceipt(500, []byte("oops"), "e.1").Action)
	disc := []byte(`{"v":1,"action":"discard","status":"refused","batch":"e.1","rule":"frames"}`)
	require.Equal(t, ActionDiscard, readReceipt(400, disc, "e.1").Action)
	require.Equal(t, ActionRetry, readReceipt(503, disc, "e.1").Action, "a status that does not fit the rule")
	retry := []byte(`{"v":1,"action":"retry","status":"refused","batch":null,"rule":"store","retry_after_s":30}`)
	require.Equal(t, 30, readReceipt(503, retry, "e.1").RetryAfterSec)
}

// ---------------------------------------------------------------------------
// Delivery, end to end, over a Unix socket
// ---------------------------------------------------------------------------

// consumer is a minimal consumer: it answers the limits and settles every
// batch, keeping each frame line it received.
type consumer struct {
	mu     sync.Mutex
	lines  []string
	ids    map[string]bool
	down   atomic.Bool
	server *http.Server
}

func startConsumer(t *testing.T, sock string) *consumer {
	t.Helper()
	c := &consumer{ids: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/limits", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"v":1,"frame_bytes":16384,"batch_frames":500,"batch_bytes":1048576,"heartbeat_s":60}`)
	})
	mux.HandleFunc("POST /v1/batch", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if c.down.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"v":1,"action":"retry","status":"refused","batch":null,"rule":"store","retry_after_s":0}`)
			return
		}
		sc := bufio.NewScanner(bytes.NewReader(body))
		sc.Buffer(make([]byte, 0, 64<<10), 2<<20)
		var header struct {
			Batch  string `json:"batch"`
			Frames int    `json:"frames"`
		}
		first := true
		var frames []string
		for sc.Scan() {
			if first {
				_ = json.Unmarshal(sc.Bytes(), &header)
				first = false
				continue
			}
			frames = append(frames, sc.Text())
		}
		c.mu.Lock()
		if !c.ids[header.Batch] {
			c.ids[header.Batch] = true
			c.lines = append(c.lines, frames...)
		}
		c.mu.Unlock()
		_, _ = fmt.Fprintf(w, `{"v":1,"action":"settle","status":"stored","batch":%q,"frames":%d,"accepted":%d,"refused_count":0,"refused":[],"dropped":0}`,
			header.Batch, len(frames), len(frames))
	})
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	c.server = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = c.server.Serve(ln) }()
	t.Cleanup(func() { _ = c.server.Close() })
	return c
}

func (c *consumer) received() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.lines...)
}

// countActivities counts the frames that are not heartbeats.
func countActivities(lines []string) int {
	n := 0
	for _, l := range lines {
		if !strings.Contains(l, `"activity":"heartbeat"`) {
			n++
		}
	}
	return n
}

func shortDir(t *testing.T) string {
	t.Helper()
	// A Unix socket path is capped near 104 bytes on macOS; t.TempDir can be
	// longer than that.
	dir, err := os.MkdirTemp("", "sf")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func feedDB(t *testing.T) (*sql.DB, *events.Recorder) {
	t.Helper()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db, events.NewRecorder(db)
}

func TestFeed_DeliversMetadataOnlyAndSpoolsWhileDown(t *testing.T) {
	db, rec := feedDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sockDir := shortDir(t)
	sock := filepath.Join(sockDir, "c.sock")
	c := startConsumer(t, sock)
	c.down.Store(true)
	_, err := store.EnsureProject(ctx, db, "vault-project", "vault-project")
	require.NoError(t, err)
	require.NoError(t, store.SetProjectIsolation(ctx, db, "vault-project", core.IsolationConfidential))

	on := &atomic.Bool{}
	on.Store(true)
	f := New(Config{DB: db, Events: rec, Socket: sock, Spool: filepath.Join(sockDir, "spool"), Host: "testhost",
		Enabled: func(context.Context) bool { return on.Load() }})
	f.pullEvery, f.backoffFloor, f.backoffCeil = 100*time.Millisecond, 50*time.Millisecond, 300*time.Millisecond
	require.NoError(t, f.Run(ctx))
	// Let the feed take its starting cursor before anything happens.
	require.Eventually(t, func() bool {
		st, ok, _ := readSpoolState(filepath.Join(sockDir, "spool", "state.json"))
		return ok && st.Active
	}, 10*time.Second, 20*time.Millisecond)

	for _, e := range contentEvents() {
		e.ID, e.SessionID = "", ""
		_, err := rec.Record(ctx, e)
		require.NoError(t, err)
	}
	// Down: everything waits in the spool.
	require.Eventually(t, func() bool {
		st, ok, _ := readSpoolState(filepath.Join(sockDir, "spool", "state.json"))
		return ok && st.Inflight != nil
	}, 10*time.Second, 20*time.Millisecond)
	require.Empty(t, c.received())

	c.down.Store(false)
	require.Eventually(t, func() bool { return countActivities(c.received()) >= len(contentEvents()) }, 30*time.Second, 50*time.Millisecond,
		"every event reaches the consumer, in spite of their identical timestamps")
	got := strings.Join(c.received(), "\n")
	for _, marker := range contentMarkers {
		require.NotContains(t, got, marker, "the consumer received content")
	}
	require.Contains(t, got, `"activity":"access_refused"`)
	require.Contains(t, got, `"activity":"corpus_drift"`)
	require.Contains(t, got, `"source_seq":`)

	// Off: nothing more is spooled.
	on.Store(false)
	n := len(c.received())
	_, err = rec.Record(ctx, core.Event{Kind: core.EventMemoryWritten, Payload: map[string]any{"kind": "gotcha"}})
	require.NoError(t, err)
	require.Never(t, func() bool { return len(c.received()) > n }, 2*time.Second, 100*time.Millisecond)
	cancel()
	f.Wait()
}

// A rewritten log (a restore, a VACUUM) can renumber rows: the cursor is
// re-found by its event's id, or moved to the newest event when that is gone.
func TestFeed_CursorSurvivesARewrittenLog(t *testing.T) {
	ctx := context.Background()
	db, rec := feedDB(t)
	var ids []string
	for range 3 {
		id, err := rec.Record(ctx, core.Event{Kind: core.EventMemoryWritten})
		require.NoError(t, err)
		ids = append(ids, id)
	}
	f := New(Config{DB: db, Events: rec, Socket: "/nonexistent.sock"})
	sp, err := openSpool(spoolDir(t))
	require.NoError(t, err)
	defer sp.close()

	require.NoError(t, sp.setCursor(99, ids[1], true))
	require.NoError(t, f.checkCursor(ctx, sp))
	row, found, err := rec.RowOf(ctx, ids[1])
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, row, sp.st.CursorRow, "re-found by id")

	require.NoError(t, sp.setCursor(99, "01GONE0000000000000000000A", true))
	require.NoError(t, f.checkCursor(ctx, sp))
	newest, newestID, err := rec.NewestRow(ctx)
	require.NoError(t, err)
	require.Equal(t, newest, sp.st.CursorRow)
	require.Equal(t, ids[2], newestID)
}
