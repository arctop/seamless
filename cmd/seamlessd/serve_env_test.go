package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// applyServeEnv is what lets a Windows Scheduled Task -- which cannot carry an
// env prefix the way a launchd plist or systemd unit does -- pin the config and
// reach a log file. These pin both halves.

func TestApplyServeEnv_ConfigExportsEnv(t *testing.T) {
	t.Setenv("SEAMLESS_CONFIG", "") // isolate; t.Setenv restores after the test
	want := filepath.Join(t.TempDir(), "seamless.yaml")

	cleanup, err := applyServeEnv(want, "", false)
	require.NoError(t, err)
	defer cleanup()

	require.Equal(t, want, os.Getenv("SEAMLESS_CONFIG"),
		"--config must export $SEAMLESS_CONFIG so config.Load's search order stays the one code path")
}

func TestApplyServeEnv_EmptyConfigLeavesEnvUntouched(t *testing.T) {
	t.Setenv("SEAMLESS_CONFIG", "/pre/existing.yaml")

	cleanup, err := applyServeEnv("", "", false)
	require.NoError(t, err)
	defer cleanup()

	require.Equal(t, "/pre/existing.yaml", os.Getenv("SEAMLESS_CONFIG"),
		"an empty --config must not clobber an inherited $SEAMLESS_CONFIG")
}

func TestApplyServeEnv_LogFileReceivesLogs(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "seamlessd.log")

	cleanup, err := applyServeEnv("", logPath, false)
	require.NoError(t, err)

	slog.Info("hello from serve", "marker", "windows-log-file")
	cleanup() // close the file before reading it back

	data, err := os.ReadFile(logPath)
	require.NoError(t, err)
	require.Contains(t, string(data), "windows-log-file",
		"--log-file must tee the daemon's logs to the path the Scheduled Task points at")

	// Restore the default logger so a file handle in a temp dir does not linger
	// as the process-wide slog sink for later tests.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
}

func TestApplyServeEnv_NoLogFileReturnsSafeCleanup(t *testing.T) {
	cleanup, err := applyServeEnv("", "", false)
	require.NoError(t, err)
	require.NotPanics(t, cleanup, "the no-op cleanup must be safe to call")
}

// TestApplyServeEnv_DetachedLogsToTheFileAlone: a serve that gave its console
// back has no stderr to tee to, so the logs go to the file alone, and stdout
// and stderr move to the file too (a panic then lands in the log).
func TestApplyServeEnv_DetachedLogsToTheFileAlone(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "seamlessd.log")
	var moved *os.File
	prev := serveStdioTo
	serveStdioTo = func(f *os.File) { moved = f }
	t.Cleanup(func() {
		serveStdioTo = prev
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	})

	cleanup, err := applyServeEnv("", logPath, true)
	require.NoError(t, err)
	require.NotNil(t, moved, "stdout and stderr move to the log file")
	require.Equal(t, logPath, moved.Name())

	slog.Info("hello from a detached serve", "marker", "detached-log")
	cleanup()
	data, err := os.ReadFile(logPath)
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(string(data), "detached-log"), "each line once: no tee into a stderr that is the same file")
}

func TestApplyServeEnv_AttachedKeepsStdio(t *testing.T) {
	prev := serveStdioTo
	serveStdioTo = func(*os.File) { t.Fatal("a serve that kept its console keeps its stdio") }
	t.Cleanup(func() {
		serveStdioTo = prev
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	})
	cleanup, err := applyServeEnv("", filepath.Join(t.TempDir(), "seamlessd.log"), false)
	require.NoError(t, err)
	cleanup()
}
