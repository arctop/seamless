//go:build darwin || linux

package main

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// redactLogBuffer is the log serve writes while the test reads it.
type redactLogBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *redactLogBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *redactLogBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// review3 LOW-3, serve's log: "seamlessd listening" names server_url without
// its user and password, in every daemon log. serve still starts with one.
func TestRedact_ServeLogsServerURLWithoutItsUser(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	root := t.TempDir()
	port, release := frelayDualLoopbackPort(t)
	yaml := fmt.Sprintf("addr: '127.0.0.1:%d'\nserver_url: 'http://%s127.0.0.1:%d'\ndata_dir: '%s'\nmcp:\n  api_key: '%s'\n"+
		"update:\n  check: false\ngardener:\n  enabled: false\n", port, redactUser, port, filepath.Join(root, "data"),
		"redact-serve-key-0123456789")
	cfgPath := filepath.Join(root, "seamless.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(yaml), 0o600))
	t.Setenv("SEAMLESS_CONFIG", cfgPath)

	logs := &redactLogBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	done := make(chan error, 1)
	release() // serve binds both halves itself
	go func() { done <- runServe([]string{"--config", cfgPath}) }()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		require.NoError(t, syscall.Kill(syscall.Getpid(), syscall.SIGTERM))
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(20 * time.Second):
			t.Error("serve did not stop")
		}
	}
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(20 * time.Second)
	for {
		resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		select {
		case err := <-done:
			stopped = true
			t.Fatalf("serve stopped before it answered: %v\n%s", err, logs.String())
		default:
		}
		require.True(t, time.Now().Before(deadline), "serve never answered /healthz")
		<-time.After(50 * time.Millisecond)
	}
	t.Cleanup(stop)
	stop()

	out := logs.String()
	require.Contains(t, out, "seamlessd listening")
	require.Contains(t, out, fmt.Sprintf("url=http://127.0.0.1:%d ", port))
	redactRequireNoUser(t, out)
}
