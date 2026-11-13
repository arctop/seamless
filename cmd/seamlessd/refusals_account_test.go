package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/feed"
	"github.com/arctop/seamless/internal/screening"
)

// doorConsumer is a minimal metadata-feed consumer on a Unix socket: it
// settles every batch and keeps each frame line once.
type doorConsumer struct {
	mu    sync.Mutex
	lines []string
	seen  map[string]bool
}

func startDoorConsumer(t *testing.T, sock string) *doorConsumer {
	t.Helper()
	c := &doorConsumer{seen: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/limits", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"v":1,"heartbeat_s":60}`)
	})
	mux.HandleFunc("POST /v1/batch", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sc := bufio.NewScanner(bytes.NewReader(body))
		sc.Buffer(make([]byte, 0, 64<<10), 2<<20)
		var header struct {
			Batch string `json:"batch"`
		}
		var frames []string
		for first := true; sc.Scan(); first = false {
			if first {
				_ = json.Unmarshal(sc.Bytes(), &header)
				continue
			}
			frames = append(frames, sc.Text())
		}
		c.mu.Lock()
		if !c.seen[header.Batch] {
			c.seen[header.Batch] = true
			c.lines = append(c.lines, frames...)
		}
		c.mu.Unlock()
		_, _ = fmt.Fprintf(w, `{"v":1,"action":"settle","status":"stored","batch":%q,"frames":%d,"accepted":%d,"refused_count":0,"refused":[],"dropped":0}`,
			header.Batch, len(frames), len(frames))
	})
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return c
}

// refusedFrames are the access.refused frames received, by reason.
func (c *doorConsumer) refusedFrames() map[string]map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]map[string]any{}
	for _, line := range c.lines {
		var f map[string]any
		if json.Unmarshal([]byte(line), &f) != nil || f["activity"] != "access_refused" {
			continue
		}
		if fields, ok := f["fields"].(map[string]any); ok {
			if reason, ok := fields["reason"].(string); ok {
				out[reason] = f
			}
		}
	}
	return out
}

// A restricted account at the owner's console is refused at the door, where
// refusalWatch -- whose own context never held the request's View -- records
// the refusal: the access.refused frame still names the account, from the
// refusal note. A socket peer nobody listed is refused with no account.
func TestRefusalWatch_NamesARestrictedAccountAtTheConsoleDoor(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := newRADaemon(t)
	rec := events.NewRecorder(d.db)
	screen := screening.New(screening.Config{DB: d.db, Events: rec, Enabled: func(context.Context) bool { return true }})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	socket := func(uid uint32) string {
		srv := httptest.NewServer(refusalWatch(screen, injectPeer(uid, peerHandler(d.policy, storeLeases(d.db), logger, d.mux))))
		t.Cleanup(func() { closeServer(srv) })
		return srv.URL
	}

	dir, err := os.MkdirTemp("", "sf") // a socket path is capped near 104 bytes on macOS
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	c := startDoorConsumer(t, filepath.Join(dir, "c.sock"))
	fd := feed.New(feed.Config{DB: d.db, Events: rec, Socket: filepath.Join(dir, "c.sock"), Spool: filepath.Join(dir, "spool"),
		Host: "build-host", Enabled: func(context.Context) bool { return true }})
	require.NoError(t, fd.Run(ctx))
	t.Cleanup(fd.Wait)
	t.Cleanup(cancel)
	require.Eventually(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return len(c.lines) > 0
	}, 20*time.Second, 20*time.Millisecond, "the feed took its cursor and heartbeat")

	for _, base := range []string{socket(raUIDAgent), socket(raUIDStranger)} {
		resp, err := http.Get(base + "/console/")
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusForbidden, resp.StatusCode)
	}

	var got map[string]map[string]any
	require.Eventually(t, func() bool {
		got = c.refusedFrames()
		return got["peer.owner_only"] != nil && got["peer.unknown"] != nil
	}, 30*time.Second, 50*time.Millisecond, "both refusals reach the consumer")
	door := got["peer.owner_only"]
	require.Equal(t, map[string]any{"kind": "agent", "account": raAccount}, door["actor"], "the account refused at the door")
	require.Equal(t, "console", door["fields"].(map[string]any)["surface"])
	require.Equal(t, map[string]any{"kind": "system", "id": "seamless"}, got["peer.unknown"]["actor"],
		"a peer nobody listed has no account")
}
