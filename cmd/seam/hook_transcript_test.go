package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/harvest"
	"github.com/arctop/seamless/internal/hooks"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
	"github.com/arctop/seamless/internal/transcripts"
)

// The push path end to end, over a real peer_auth socket: the daemon opens no
// agent file, so a flagged session's SessionEnd answer asks for the
// transcript, `seam hook session-end` sends it gzip-compressed beside the
// agent, and the daemon keeps it -- the same bytes the agent client wrote.
// An unflagged session's end sends nothing.
func TestRunHook_PushesAFlaggedTranscriptOverTheSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the peer_auth socket is a Unix deployment")
	}
	f := newCaptureFixture(t)
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, store.AdoptLocalHost(ctx, db, config.Hostname()))
	_, err = store.EnsureProject(ctx, db, "example-ops", "example-ops")
	require.NoError(t, err)
	require.NoError(t, store.AddRepoMapping(ctx, db, f.cwd, "example-ops"))
	rec := events.NewRecorder(db)
	svc, err := transcripts.New(transcripts.Config{DataDir: dir, DB: db, Events: rec, LocalHost: config.Hostname()})
	require.NoError(t, err)
	h := hooks.NewHandler(hooks.Config{
		DB: db, Retrieve: retrieve.New(db, nil, config.Budgets{MaxBriefingTokens: 1500, RecallBudgetTokens: 1000}, nil),
		Events: rec, APIKey: "k", LocalHost: config.Hostname(), Transcripts: svc,
	})
	mux := http.NewServeMux()
	h.Register(mux)
	policy, err := access.New(config.PeerAuth{Owner: "owner"},
		func(string) (access.Account, error) { return access.Account{UID: 501, Name: "owner"}, nil })
	require.NoError(t, err)
	view, ok := policy.PeerView(501)
	require.True(t, ok)
	uploads := 0
	sock := serveSocket(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == harvest.TranscriptEndpoint {
			uploads++
		}
		mux.ServeHTTP(w, r.WithContext(access.WithView(r.Context(), view)))
	}))
	cfg := socketConfig(sock)

	start := map[string]any{"session_id": "cc-push", "cwd": f.cwd, "source": "startup", "transcript_path": f.transcript}
	require.Empty(t, runHookCall(t, cfg, hookCall{event: "session-start", payload: start}))
	sess, found, err := store.AmbientSessionByExternalIdentity(ctx, db, "claude-code", "cc-push")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, f.transcript, sess.TranscriptPath, "the path is recorded though the daemon never opens it")

	end := map[string]any{"session_id": "cc-push", "transcript_path": f.transcript, "reason": "other"}
	require.Empty(t, runHookCall(t, cfg, hookCall{event: "session-end", payload: end}))
	require.Zero(t, uploads, "an unflagged session's end sends nothing")

	_, err = svc.Flag(ctx, sess, core.VerdictGood, "", "agent", true)
	require.NoError(t, err)
	require.Empty(t, runHookCall(t, cfg, hookCall{event: "session-end", payload: end}))
	require.Equal(t, 1, uploads)
	rc, err := svc.Open(sess.ID)
	require.NoError(t, err)
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	want, err := os.ReadFile(f.transcript)
	require.NoError(t, err)
	require.Equal(t, string(want), string(got))
	row, _, err := store.SessionByID(ctx, db, sess.ID)
	require.NoError(t, err)
	require.Equal(t, core.TranscriptCopied, row.Transcript)
	require.Equal(t, int64(len(want)), row.TranscriptBytes)
}

// Off the socket the client sends no transcript, whatever an answer says: the
// rule every client-side capture keeps.
func TestRunHook_SendsNoTranscriptOffTheSocket(t *testing.T) {
	f := newCaptureFixture(t)
	uploads := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, harvest.TranscriptEndpoint) {
			uploads++
		}
		w.Header().Set(harvest.TranscriptWantedHeader, "0")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"continue":true,"suppressOutput":true}`)
	}))
	t.Cleanup(srv.Close)
	cfg := tcpConfig(strings.TrimPrefix(srv.URL, "http://"))
	require.Empty(t, runHookCall(t, cfg, hookCall{event: "session-end",
		payload: map[string]any{"session_id": "cc-x", "transcript_path": f.transcript}}))
	require.Zero(t, uploads)
}
