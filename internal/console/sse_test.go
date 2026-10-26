package console

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/store/storetest"
)

func TestSSE_StreamsRecordedEvents(t *testing.T) {
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	rec := events.NewRecorder(db)

	svc, err := New(Config{DB: db, Events: rec, APIKey: testKey})
	require.NoError(t, err)
	mux := http.NewServeMux()
	svc.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/console/events", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, resp.Header.Get("Content-Type"), "text/event-stream")

	// Headers are flushed after Subscribe + the ": connected" write, so the
	// subscription is live now; record an event and read its data frame.
	_, err = rec.Record(context.Background(), core.Event{
		Kind: core.EventMemoryWritten, Payload: map[string]any{"name": "watcher-race"},
	})
	require.NoError(t, err)

	sc := bufio.NewScanner(resp.Body)
	var dataLine string
	for sc.Scan() {
		if line := sc.Text(); strings.HasPrefix(line, "data:") {
			dataLine = line
			break
		}
	}
	require.NotEmpty(t, dataLine, "expected a data frame")
	require.Contains(t, dataLine, "memory.written")
	require.Contains(t, dataLine, "watcher-race")
}

func TestSSE_InteractionsFeedFiltersKinds(t *testing.T) {
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	rec := events.NewRecorder(db)

	svc, err := New(Config{DB: db, Events: rec, APIKey: testKey})
	require.NoError(t, err)
	mux := http.NewServeMux()
	svc.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/console/events?feed=interactions", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// memory.written is not an interaction kind -> no frame; tool.call -> a frame.
	_, err = rec.Record(context.Background(), core.Event{Kind: core.EventMemoryWritten, Payload: map[string]any{"name": "skip-me"}})
	require.NoError(t, err)
	_, err = rec.Record(context.Background(), core.Event{Kind: core.EventToolCall, Payload: map[string]any{"tool": "recall"}})
	require.NoError(t, err)

	sc := bufio.NewScanner(resp.Body)
	var dataLine string
	for sc.Scan() {
		if line := sc.Text(); strings.HasPrefix(line, "data:") {
			dataLine = line
			break
		}
	}
	require.NotEmpty(t, dataLine, "expected an interaction data frame")
	require.Contains(t, dataLine, "tool.call")
	require.NotContains(t, dataLine, "memory.written")
	require.NotContains(t, dataLine, "skip-me")
}

func TestSSE_UnauthorizedRedirects(t *testing.T) {
	_, mux := newConsole(t)
	rr := do(mux, httptest.NewRequest(http.MethodGet, "/console/events", nil))
	require.Equal(t, http.StatusSeeOther, rr.Code)
}
