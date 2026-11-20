package hooks

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/harvest"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
	"github.com/arctop/seamless/internal/transcripts"
)

// transcriptEnv is a hook server keeping transcript copies, with view (when
// set) as the peer_auth View every request carries.
type transcriptEnv struct {
	ts  *httptest.Server
	db  *sql.DB
	svc *transcripts.Service
}

func newTranscriptEnv(t *testing.T, view *access.View, maxBytes int64) *transcriptEnv {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, store.AdoptLocalHost(ctx, db, localHostName))
	_, err = store.EnsureProject(ctx, db, "demo", "demo")
	require.NoError(t, err)
	require.NoError(t, store.AddRepoMapping(ctx, db, "/work/demo", "demo"))
	rec := events.NewRecorder(db)
	svc, err := transcripts.New(transcripts.Config{DataDir: dir, DB: db, Events: rec, LocalHost: localHostName, MaxBytes: maxBytes})
	require.NoError(t, err)
	ret := retrieve.New(db, nil, config.Budgets{MaxBriefingTokens: 1500, RecallBudgetTokens: 1000}, nil)
	h := NewHandler(Config{DB: db, Retrieve: ret, Events: rec, APIKey: testKey, LocalHost: localHostName, Transcripts: svc})
	mux := http.NewServeMux()
	h.Register(mux)
	var handler http.Handler = mux
	if view != nil {
		v := *view
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mux.ServeHTTP(w, r.WithContext(access.WithView(r.Context(), v)))
		})
	}
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return &transcriptEnv{ts: ts, db: db, svc: svc}
}

// hook posts one hook event and returns the response (body read and closed).
func (e *transcriptEnv) hook(t *testing.T, event, client, host string, body map[string]any) *http.Response {
	t.Helper()
	q := url.Values{}
	if client != "" {
		q.Set("client", client)
	}
	if host != "" {
		q.Set("host", host)
	}
	resp, _ := post(t, e.ts.URL+"/api/hooks/"+event+"?"+q.Encode(), testKey, body)
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", event)
	return resp
}

// start opens the ambient session for id, naming transcript, and returns it.
func (e *transcriptEnv) start(t *testing.T, client Client, id, transcript string) core.Session {
	t.Helper()
	clientParam := ""
	if client == ClientCodex {
		clientParam = "codex"
	}
	e.hook(t, "session-start", clientParam, localHostName, map[string]any{
		"session_id": id, "cwd": "/work/demo", "source": "startup", "transcript_path": transcript,
	})
	sess := requireAmbientSession(t, e.db, client, id)
	require.Equal(t, transcript, sess.TranscriptPath, "every SessionStart records the transcript path")
	return sess
}

func (e *transcriptEnv) flag(t *testing.T, sess core.Session) {
	t.Helper()
	_, err := e.svc.Flag(context.Background(), sess, core.VerdictNotable, "", "agent", false)
	require.NoError(t, err)
}

func (e *transcriptEnv) reload(t *testing.T, id string) core.Session {
	t.Helper()
	sess, ok, err := store.SessionByID(context.Background(), e.db, id)
	require.NoError(t, err)
	require.True(t, ok)
	return sess
}

func (e *transcriptEnv) copyOf(t *testing.T, id string) string {
	t.Helper()
	rc, err := e.svc.Open(id)
	require.NoError(t, err)
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	require.NoError(t, err)
	return string(b)
}

const tLine = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}` + "\n"

// Claude Code's SessionEnd copies a flagged session's transcript when the
// daemon can read it -- also when the agent closed the session with the
// session_end tool first, the common order -- and copies nothing for an
// unflagged one.
func TestTranscript_SessionEndCopiesAFlaggedSession(t *testing.T) {
	e := newTranscriptEnv(t, nil, 0)
	path := writeTranscript(t, tLine)
	sess := e.start(t, ClientClaudeCode, "cc-flag-1", path)
	e.flag(t, sess)
	// session_end over MCP completed the row before the hook's end arrives.
	sess.Status, sess.UpdatedAt = core.SessionCompleted, time.Now().UTC()
	require.NoError(t, store.UpdateSession(context.Background(), e.db, sess))

	resp := e.hook(t, "session-end", "", localHostName, map[string]any{"session_id": "cc-flag-1", "transcript_path": path})
	require.Empty(t, resp.Header.Get(harvest.TranscriptWantedHeader), "the daemon reads it itself: nothing to ask for")
	e.svc.Wait()
	require.Equal(t, tLine, e.copyOf(t, sess.ID))
	require.Equal(t, core.TranscriptCopied, e.reload(t, sess.ID).Transcript)

	other := e.start(t, ClientClaudeCode, "cc-plain-1", path)
	e.hook(t, "session-end", "", localHostName, map[string]any{"session_id": "cc-plain-1", "transcript_path": path})
	e.svc.Wait()
	require.False(t, e.svc.Kept(other.ID), "an unflagged session keeps no copy")
}

// A flagged session's agent on another computer of a server without peer_auth:
// its file is not this daemon's to read, so no copy, and the record says so.
func TestTranscript_RemoteAgentIsRecordedNotCopied(t *testing.T) {
	e := newTranscriptEnv(t, nil, 0)
	path := writeTranscript(t, tLine)
	sess := e.start(t, ClientClaudeCode, "cc-remote-1", path)
	e.flag(t, sess)
	e.hook(t, "session-end", "", remoteHostName, map[string]any{"session_id": "cc-remote-1", "transcript_path": path})
	e.svc.Wait()
	require.False(t, e.svc.Kept(sess.ID))
	require.Equal(t, core.TranscriptRemote, e.reload(t, sess.ID).Transcript)
}

// Codex has no SessionEnd: each Stop copies a flagged session's rollout, and
// again only once it grew.
func TestTranscript_CodexStopRecopiesOnlyOnGrowth(t *testing.T) {
	e := newTranscriptEnv(t, nil, 0)
	path := writeTranscript(t, tLine)
	sess := e.start(t, ClientCodex, "cx-flag-1", path)
	e.flag(t, sess)
	stop := func() {
		e.hook(t, "stop", "codex", localHostName, map[string]any{"session_id": "cx-flag-1", "transcript_path": path})
		e.svc.Wait()
	}
	copiedAt := func() time.Time { return *e.reload(t, sess.ID).TranscriptCopiedAt }

	stop()
	first := copiedAt()
	require.Equal(t, int64(len(tLine)), e.reload(t, sess.ID).TranscriptBytes)
	stop()
	require.Equal(t, first, copiedAt(), "unchanged: not copied again")

	require.NoError(t, os.WriteFile(path, []byte(tLine+tLine), 0o600))
	stop()
	require.Equal(t, tLine+tLine, e.copyOf(t, sess.ID), "grown: copied again")
	require.Equal(t, int64(2*len(tLine)), e.reload(t, sess.ID).TranscriptBytes)
}

// peerViews builds the owner's socket View and a restricted account's.
func peerViews(t *testing.T) (owner, agent access.View) {
	t.Helper()
	const agentAccount = "_example_agent"
	accounts := map[string]access.Account{"owner": {UID: 501, Name: "owner"}, agentAccount: {UID: 470, Name: agentAccount}}
	policy, err := access.New(config.PeerAuth{Owner: "owner", Restricted: config.BareRestricted(agentAccount)},
		func(n string) (access.Account, error) {
			if a, ok := accounts[n]; ok {
				return a, nil
			}
			return access.Account{}, fmt.Errorf("no %s", n)
		})
	require.NoError(t, err)
	owner, ok := policy.PeerView(501)
	require.True(t, ok)
	agent, ok = policy.PeerView(470)
	require.True(t, ok)
	return owner, agent.WithOpenProjects([]string{"demo"})
}

// upload posts a gzip body to the upload route as `seam hook` does.
func (e *transcriptEnv) upload(t *testing.T, client, id string, body []byte, claimed int64) (int, string) {
	t.Helper()
	q := url.Values{harvest.TranscriptSessionParam: {id}}
	if client != "" {
		q.Set("client", client)
	}
	req, err := http.NewRequest(http.MethodPost, e.ts.URL+harvest.TranscriptEndpoint+"?"+q.Encode(), bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set(harvest.TranscriptSizeHeader, strconv.FormatInt(claimed, 10))
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(out)
}

func gzipped(t *testing.T, s string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	_, err := w.Write([]byte(s))
	require.NoError(t, err)
	require.NoError(t, w.Close())
	return b.Bytes()
}

// Under peer_auth the daemon opens nothing: the SessionEnd answer of a flagged
// session names the wanted transcript (an unflagged one's never does), and
// the upload `seam hook` then sends over the socket is kept -- for a session
// the caller may act on that is flagged, and only those.
func TestTranscript_PeerAuthPushOverTheSocket(t *testing.T) {
	owner, _ := peerViews(t)
	e := newTranscriptEnv(t, &owner, 0)
	path := writeTranscript(t, tLine)
	sess := e.start(t, ClientClaudeCode, "cc-peer-1", path)

	resp := e.hook(t, "session-end", "", localHostName, map[string]any{"session_id": "cc-peer-1", "transcript_path": path})
	require.Empty(t, resp.Header.Get(harvest.TranscriptWantedHeader), "an unflagged session asks for nothing")
	status, _ := e.upload(t, "", "cc-peer-1", gzipped(t, tLine), int64(len(tLine)))
	require.Equal(t, http.StatusNotFound, status, "an unflagged session takes no upload")

	e.flag(t, sess)
	resp = e.hook(t, "session-end", "", localHostName, map[string]any{"session_id": "cc-peer-1", "transcript_path": path})
	require.Equal(t, "0", resp.Header.Get(harvest.TranscriptWantedHeader))
	e.svc.Wait()
	require.False(t, e.svc.Kept(sess.ID), "the daemon read no file itself")

	status, body := e.upload(t, "", "cc-peer-1", gzipped(t, tLine), int64(len(tLine)))
	require.Equal(t, http.StatusOK, status, body)
	require.Equal(t, tLine, e.copyOf(t, sess.ID))
	got := e.reload(t, sess.ID)
	require.Equal(t, core.TranscriptCopied, got.Transcript)
	require.Equal(t, int64(len(tLine)), got.TranscriptBytes)

	status, _ = e.upload(t, "", "cc-peer-1", []byte("not gzip"), 8)
	require.Equal(t, http.StatusBadRequest, status)
	require.Equal(t, tLine, e.copyOf(t, sess.ID), "a bad upload leaves the copy kept")

	status, _ = e.upload(t, "", "cc-no-such", gzipped(t, tLine), int64(len(tLine)))
	require.Equal(t, http.StatusNotFound, status)

	// Codex: the Stop's answer names the size already kept, so the client
	// sends the rollout only once it grew.
	cx := e.start(t, ClientCodex, "cx-peer-1", path)
	e.flag(t, cx)
	resp = e.hook(t, "stop", "codex", localHostName, map[string]any{"session_id": "cx-peer-1", "transcript_path": path})
	require.Equal(t, "0", resp.Header.Get(harvest.TranscriptWantedHeader), "no copy yet")
	status, body = e.upload(t, "codex", "cx-peer-1", gzipped(t, tLine), int64(len(tLine)))
	require.Equal(t, http.StatusOK, status, body)
	resp = e.hook(t, "stop", "codex", localHostName, map[string]any{"session_id": "cx-peer-1", "transcript_path": path})
	require.Equal(t, strconv.Itoa(len(tLine)), resp.Header.Get(harvest.TranscriptWantedHeader))
}

// The upload is the socket's alone: without peer_auth the daemon reads the
// file itself and takes none.
func TestTranscript_UploadRefusedWithoutPeerAuth(t *testing.T) {
	e := newTranscriptEnv(t, nil, 0)
	path := writeTranscript(t, tLine)
	sess := e.start(t, ClientClaudeCode, "cc-tcp-1", path)
	e.flag(t, sess)
	status, body := e.upload(t, "", "cc-tcp-1", gzipped(t, tLine), int64(len(tLine)))
	require.Equal(t, http.StatusForbidden, status, body)
	require.False(t, e.svc.Kept(sess.ID))
}

// An upload past the cap is refused, unread when its claimed size says so,
// and the record says too large.
func TestTranscript_UploadSizeCap(t *testing.T) {
	owner, _ := peerViews(t)
	e := newTranscriptEnv(t, &owner, 256)
	path := writeTranscript(t, tLine)
	sess := e.start(t, ClientClaudeCode, "cc-cap-1", path)
	e.flag(t, sess)
	status, _ := e.upload(t, "", "cc-cap-1", gzipped(t, tLine), 257)
	require.Equal(t, http.StatusRequestEntityTooLarge, status)
	require.Equal(t, core.TranscriptTooLarge, e.reload(t, sess.ID).Transcript)

	big := bytes.Repeat([]byte(tLine), 10) // 940 bytes, claimed small
	status, _ = e.upload(t, "", "cc-cap-1", gzipped(t, string(big)), 10)
	require.Equal(t, http.StatusRequestEntityTooLarge, status, "the daemon counts what arrives, not what was claimed")
	require.False(t, e.svc.Kept(sess.ID))
}

// A restricted account's hook may not send a transcript for the owner's
// session, which reads exactly like one that is not flagged.
func TestTranscript_UploadRefusesAnotherPrincipalsSession(t *testing.T) {
	owner, agent := peerViews(t)
	ownerEnv := newTranscriptEnv(t, &owner, 0)
	path := writeTranscript(t, tLine)
	sess := ownerEnv.start(t, ClientClaudeCode, "cc-owner-1", path)
	ownerEnv.flag(t, sess)

	// The same database, served as the restricted account.
	rec := events.NewRecorder(ownerEnv.db)
	ret := retrieve.New(ownerEnv.db, nil, config.Budgets{MaxBriefingTokens: 1500, RecallBudgetTokens: 1000}, nil)
	h := NewHandler(Config{DB: ownerEnv.db, Retrieve: ret, Events: rec, APIKey: testKey, LocalHost: localHostName, Transcripts: ownerEnv.svc})
	mux := http.NewServeMux()
	h.Register(mux)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r.WithContext(access.WithView(r.Context(), agent)))
	}))
	t.Cleanup(ts.Close)
	agentEnv := &transcriptEnv{ts: ts, db: ownerEnv.db, svc: ownerEnv.svc}

	status, _ := agentEnv.upload(t, "", "cc-owner-1", gzipped(t, "forged\n"), 7)
	require.Equal(t, http.StatusNotFound, status)
	require.False(t, ownerEnv.svc.Kept(sess.ID))
}

// An agent on the TCP key under peer_auth sends no transcript (its hook pushes
// only to the socket): no answer asks it to, and the record says why.
func TestTranscript_TCPKeyUnderPeerAuthIsRecordedNotAsked(t *testing.T) {
	policy, err := access.New(config.PeerAuth{Owner: "owner"},
		func(string) (access.Account, error) { return access.Account{UID: 501, Name: "owner"}, nil })
	require.NoError(t, err)
	tcp := policy.OwnerView() // ownerHandler's view of a TCP request
	e := newTranscriptEnv(t, &tcp, 0)
	path := writeTranscript(t, tLine)
	sess := e.start(t, ClientClaudeCode, "cc-tcp-peer", path)
	e.flag(t, sess)
	resp := e.hook(t, "session-end", "", localHostName, map[string]any{"session_id": "cc-tcp-peer", "transcript_path": path})
	require.Empty(t, resp.Header.Get(harvest.TranscriptWantedHeader))
	e.svc.Wait()
	require.False(t, e.svc.Kept(sess.ID))
	require.Equal(t, core.TranscriptNotPushed, e.reload(t, sess.ID).Transcript)
}
