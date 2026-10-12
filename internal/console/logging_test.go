package console

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
)

// A query that failed because the browser stopped waiting logs at debug, under
// every wrapping the store and the With chain add; a deadline, and anything
// that is not a cancellation, keeps its level.
func TestClientGoneHandler(t *testing.T) {
	var buf bytes.Buffer
	svc, err := New(Config{APIKey: testKey,
		Logger: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	require.NoError(t, err)

	gone := fmt.Errorf("store.ProjectsWithCounts: sessions: scan: %w", context.Canceled)
	svc.logger.Error("console: request failed", "path", "/console/projects/x", "error", gone)
	svc.logger.With("screen", "home").Warn("console: nav counts", "error", gone)
	require.NotContains(t, buf.String(), "level=ERROR")
	require.NotContains(t, buf.String(), "level=WARN")
	require.Contains(t, buf.String(),
		`level=DEBUG msg="console: request failed" path=/console/projects/x error="store.ProjectsWithCounts: sessions: scan: context canceled"`)
	require.Contains(t, buf.String(), `level=DEBUG msg="console: nav counts" screen=home error=`)

	buf.Reset()
	svc.logger.Error("console: request failed", "error", fmt.Errorf("q: %w", context.DeadlineExceeded))
	svc.logger.Warn("console: nav counts", "error", fmt.Errorf("disk I/O error"))
	require.Contains(t, buf.String(), `level=ERROR msg="console: request failed" error="q: context deadline exceeded"`)
	require.Contains(t, buf.String(), `level=WARN msg="console: nav counts"`)

	// Above debug, a cancellation is dropped rather than demoted into view.
	var info bytes.Buffer
	quiet := slog.New(clientGoneHandler{slog.NewTextHandler(&info, nil)})
	quiet.Error("console: request failed", "error", gone)
	require.Empty(t, info.String())
}
