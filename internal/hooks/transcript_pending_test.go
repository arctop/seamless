package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/devices"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/harvest"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store"
)

// as serves e's database and copies to requests carrying view (nil: none,
// peer_auth off).
func (e *transcriptEnv) as(t *testing.T, view *access.View) *transcriptEnv {
	t.Helper()
	rec := events.NewRecorder(e.db)
	ret := retrieve.New(e.db, nil, config.Budgets{MaxBriefingTokens: 1500, RecallBudgetTokens: 1000}, nil)
	h := NewHandler(Config{DB: e.db, Retrieve: ret, Events: rec, APIKey: testKey, LocalHost: localHostName, Transcripts: e.svc})
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
	return &transcriptEnv{ts: ts, db: e.db, svc: e.svc}
}

// asks is how many times session id's transcript was asked for.
func (e *transcriptEnv) asks(t *testing.T, id string) int {
	t.Helper()
	n, _, err := store.TranscriptAsksOf(context.Background(), e.db, id)
	require.NoError(t, err)
	return n
}

// pendingOf decodes an answer's pending values: session id -> path.
func pendingOf(t *testing.T, resp *http.Response) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, raw := range resp.Header.Values(harvest.TranscriptPendingHeader) {
		q, err := url.ParseQuery(raw)
		require.NoError(t, err, raw)
		out[q.Get(harvest.TranscriptSessionParam)] = q.Get(harvest.TranscriptPathParam)
	}
	return out
}

// startPlain is a SessionStart of a fresh conversation id in /work/demo.
func (e *transcriptEnv) startPlain(t *testing.T, client, host, id string) *http.Response {
	t.Helper()
	return e.hook(t, "session-start", client, host, map[string]any{
		"session_id": id, "cwd": "/work/demo", "source": "startup", "transcript_path": "/elsewhere/" + id + ".jsonl",
	})
}

// Under peer_auth, a flagged session whose end signal never came is asked for
// at the same caller's later hooks -- SessionStart, SessionEnd, Codex's Stop
// -- naming the session and its recorded path, safely encoded. Another
// account, another client, another computer and the TCP key are asked
// nothing, and an answer to a caller with no pending session carries no
// header at all.
func TestTranscript_PendingAskedOfTheSameCallerOnly(t *testing.T) {
	owner, agent := peerViews(t)
	e := newTranscriptEnv(t, &owner, 0)
	odd := "/Users/o wner/.claude/projects/-work-demo/a&b=c ü.jsonl"
	sess := e.start(t, ClientClaudeCode, "cc-crashed", odd)

	resp := e.startPlain(t, "", localHostName, "cc-a")
	require.Empty(t, resp.Header.Values(harvest.TranscriptPendingHeader), "nothing flagged: no header")

	e.flag(t, sess)
	resp = e.startPlain(t, "", localHostName, "cc-b")
	require.Equal(t, map[string]string{"cc-crashed": odd}, pendingOf(t, resp))
	require.Equal(t, core.TranscriptPending, e.reload(t, sess.ID).Transcript)
	require.Equal(t, 1, e.asks(t, sess.ID))

	// Waiting out the wait after the ask: the next hooks carry nothing.
	resp = e.hook(t, "session-end", "", localHostName, map[string]any{"session_id": "cc-b"})
	require.Empty(t, resp.Header.Values(harvest.TranscriptPendingHeader))

	// Others are asked nothing, whatever the wait.
	_, err := e.db.Exec(`UPDATE session_transcript_asks SET asked_at = '2000-01-01T00:00:00Z' WHERE session_id = ?`, sess.ID)
	require.NoError(t, err)
	require.Empty(t, e.as(t, &agent).startPlain(t, "", localHostName, "cc-r").Header.Values(harvest.TranscriptPendingHeader),
		"a restricted account")
	require.Empty(t, e.startPlain(t, "codex", localHostName, "cx-c").Header.Values(harvest.TranscriptPendingHeader), "another client")
	require.Empty(t, e.startPlain(t, "", remoteHostName, "cc-far").Header.Values(harvest.TranscriptPendingHeader), "another computer")
	policy, err := access.New(config.PeerAuth{Owner: "owner"},
		func(string) (access.Account, error) { return access.Account{UID: 501, Name: "owner"}, nil })
	require.NoError(t, err)
	tcp := policy.OwnerView()
	require.Empty(t, e.as(t, &tcp).startPlain(t, "", localHostName, "cc-tcp").Header.Values(harvest.TranscriptPendingHeader),
		"the TCP key's hook sends nothing, so nothing asks it")
	require.Equal(t, 1, e.asks(t, sess.ID), "none of them counted as an ask")

	// The same caller's end signals ask too.
	resp = e.hook(t, "session-end", "", localHostName, map[string]any{"session_id": "cc-b"})
	require.Equal(t, map[string]string{"cc-crashed": odd}, pendingOf(t, resp))
	require.Equal(t, 2, e.asks(t, sess.ID))
}

// A restricted account's own pending session is asked of it alone, by the id
// its agent gave (its namespace is the daemon's).
func TestTranscript_PendingOfARestrictedAccount(t *testing.T) {
	owner, agent := peerViews(t)
	e := newTranscriptEnv(t, &owner, 0)
	ae := e.as(t, &agent)
	ae.hook(t, "session-start", "", localHostName, map[string]any{
		"session_id": "cc-acct", "cwd": "/work/demo", "source": "startup", "transcript_path": "/home/a/.claude/projects/p/x.jsonl",
	})
	sess := requireAmbientSession(t, e.db, ClientClaudeCode, access.QualifiedSessionName("_example_agent", "cc-acct"))
	e.flag(t, sess)
	require.Empty(t, e.startPlain(t, "", localHostName, "cc-owner").Header.Values(harvest.TranscriptPendingHeader),
		"the owner is not asked for an account's session")
	resp := ae.startPlain(t, "", localHostName, "cc-acct-2")
	require.Equal(t, map[string]string{"cc-acct": "/home/a/.claude/projects/p/x.jsonl"}, pendingOf(t, resp))
}

// An answer asks for at most a few pending sessions, oldest flag first.
func TestTranscript_PendingIsBoundedPerAnswer(t *testing.T) {
	owner, _ := peerViews(t)
	e := newTranscriptEnv(t, &owner, 0)
	var started []core.Session
	for _, id := range []string{"cc-p1", "cc-p2", "cc-p3", "cc-p4"} {
		started = append(started, e.start(t, ClientClaudeCode, id, "/p/"+id+".jsonl"))
	}
	// Flagged after every start: each start would ask for the ones before it.
	for _, sess := range started {
		e.flag(t, sess)
	}
	resp := e.startPlain(t, "", localHostName, "cc-new")
	require.Equal(t, map[string]string{"cc-p1": "/p/cc-p1.jsonl", "cc-p2": "/p/cc-p2.jsonl", "cc-p3": "/p/cc-p3.jsonl"}, pendingOf(t, resp))
	resp = e.startPlain(t, "", localHostName, "cc-new-2")
	require.Equal(t, map[string]string{"cc-p4": "/p/cc-p4.jsonl"}, pendingOf(t, resp), "the rest at the next answer; the asked ones wait")
}

// A locked project's session is asked for by no hook before the owner's
// approval, as it takes no findings.
func TestTranscript_PendingSkipsALockedSession(t *testing.T) {
	policy, err := access.New(config.PeerAuth{Owner: "owner"},
		func(string) (access.Account, error) { return access.Account{UID: 501, Name: "owner"}, nil })
	require.NoError(t, err)
	open, ok := policy.PeerView(501)
	require.True(t, ok)
	e := newTranscriptEnv(t, &open, 0)
	sess := e.start(t, ClientClaudeCode, "cc-locked", "/p/locked.jsonl")
	e.flag(t, sess)
	locked, ok := policy.WithConsoleLocks([]string{"demo"}).PeerView(501)
	require.True(t, ok)
	resp := e.as(t, &locked).hook(t, "session-end", "", localHostName, map[string]any{"session_id": "cc-other"})
	require.Empty(t, resp.Header.Values(harvest.TranscriptPendingHeader))
	require.Zero(t, e.asks(t, sess.ID))
}

// The client's report ends the asks with its word; a word it does not know is
// refused, and a report is the caller's own like an upload.
func TestTranscript_UploadRouteTakesTheReport(t *testing.T) {
	owner, agent := peerViews(t)
	e := newTranscriptEnv(t, &owner, 0)
	report := func(env *transcriptEnv, id, word string) int {
		t.Helper()
		q := url.Values{harvest.TranscriptSessionParam: {id}, harvest.TranscriptRefusedParam: {word}}
		req, err := http.NewRequest(http.MethodPost, env.ts.URL+harvest.TranscriptEndpoint+"?"+q.Encode(), nil)
		require.NoError(t, err)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	refused := e.start(t, ClientClaudeCode, "cc-refused", "/etc/passwd.jsonl")
	gone := e.start(t, ClientClaudeCode, "cc-gone", "/p/gone.jsonl")
	e.flag(t, refused)
	e.flag(t, gone)

	require.Equal(t, http.StatusBadRequest, report(e, "cc-refused", "maybe"))
	require.Equal(t, http.StatusNotFound, report(e.as(t, &agent), "cc-refused", harvest.TranscriptRefusedPath),
		"another principal's session reads as one not flagged")
	require.Empty(t, e.reload(t, refused.ID).Transcript)
	require.Equal(t, http.StatusOK, report(e, "cc-refused", harvest.TranscriptRefusedPath))
	require.Equal(t, http.StatusOK, report(e, "cc-gone", harvest.TranscriptRefusedMissing))
	require.Equal(t, core.TranscriptRefused, e.reload(t, refused.ID).Transcript)
	require.Equal(t, core.TranscriptUnreadable, e.reload(t, gone.ID).Transcript)
	require.Empty(t, e.startPlain(t, "", localHostName, "cc-after").Header.Values(harvest.TranscriptPendingHeader),
		"reported: asked no more")
}

// One upload of a session at a time: a second is refused while the first is
// under way, and the end signal asks for none meanwhile.
func TestTranscript_OneUploadAtATime(t *testing.T) {
	owner, _ := peerViews(t)
	e := newTranscriptEnv(t, &owner, 0)
	cx := e.start(t, ClientCodex, "cx-busy", "/s/busy.jsonl")
	e.flag(t, cx)
	end, ok := e.svc.BeginUpload([]core.Session{e.reload(t, cx.ID)})
	require.True(t, ok)
	status, body := e.upload(t, "codex", "cx-busy", gzipped(t, tLine), int64(len(tLine)))
	require.Equal(t, http.StatusConflict, status, body)
	resp := e.hook(t, "stop", "codex", localHostName, map[string]any{"session_id": "cx-busy", "transcript_path": "/s/busy.jsonl"})
	require.Empty(t, resp.Header.Get(harvest.TranscriptWantedHeader), "on its way already")
	end()
	resp = e.hook(t, "stop", "codex", localHostName, map[string]any{"session_id": "cx-busy", "transcript_path": "/s/busy.jsonl"})
	require.Equal(t, "0", resp.Header.Get(harvest.TranscriptWantedHeader))
	status, body = e.upload(t, "codex", "cx-busy", gzipped(t, tLine), int64(len(tLine)))
	require.Equal(t, http.StatusOK, status, body)
}

// With peer_auth off, the same caller's next hook on this computer makes up
// for the end signal that never came: the daemon copies the file itself, and
// no answer carries a header.
func TestTranscript_PendingCopiedByTheDaemonWithoutPeerAuth(t *testing.T) {
	e := newTranscriptEnv(t, nil, 0)
	path := writeTranscript(t, tLine)
	sess := e.start(t, ClientClaudeCode, "cc-crash-local", path)
	e.flag(t, sess)
	e.startPlain(t, "", remoteHostName, "cc-far")
	e.svc.Wait()
	require.False(t, e.svc.Kept(sess.ID), "a hook from another computer copies nothing here")
	resp := e.startPlain(t, "", localHostName, "cc-next")
	require.Empty(t, resp.Header.Values(harvest.TranscriptPendingHeader))
	e.svc.Wait()
	require.Equal(t, tLine, e.copyOf(t, sess.ID))
	require.Equal(t, core.TranscriptCopied, e.reload(t, sess.ID).Transcript)
}

// keep posts to the owner's keep route as `seam sessions --keep-transcript`
// does: no body, or a gzip body, and the daemon read on request.
func (e *transcriptEnv) keep(t *testing.T, id string, fromDaemon bool, body []byte) (int, map[string]any) {
	t.Helper()
	target := e.ts.URL + harvest.TranscriptKeepPath(id)
	if fromDaemon {
		target += "?" + harvest.TranscriptFromDaemonParam + "=1"
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(http.MethodPost, target, rd)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+testKey)
	if body != nil {
		req.Header.Set(harvest.TranscriptSizeHeader, strconv.Itoa(len(tLine)))
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return resp.StatusCode, out
}

// The owner's keep route is the owner's alone: a restricted account, a
// relay's caller and a paired device's credential are refused it, as the
// console's copy read refuses them.
func TestTranscriptKeep_RefusesAllButTheOwner(t *testing.T) {
	policy, err := access.New(config.PeerAuth{Owner: "owner", Restricted: config.BareRestricted("_example_agent"),
		Relays: map[string]config.PeerRelay{"laptop": {OwnerUID: 501}}},
		func(n string) (access.Account, error) {
			if n == "owner" {
				return access.Account{UID: 501, Name: n}, nil
			}
			return access.Account{UID: 470, Name: n}, nil
		})
	require.NoError(t, err)
	owner, ok := policy.PeerView(501)
	require.True(t, ok)
	restricted, ok := policy.PeerView(470)
	require.True(t, ok)
	restricted = restricted.WithOpenProjects([]string{"demo"})
	relayed, ok := policy.RelayChainViewNamed("01MRELAYAAAAAAAAAAAAAAAAAA", "laptop", 501, nil, nil)
	require.True(t, ok)
	e := newTranscriptEnv(t, &owner, 0)
	sess := e.start(t, ClientClaudeCode, "cc-keep", "/p/keep.jsonl")
	e.flag(t, sess)
	for name, v := range map[string]access.View{"restricted account": restricted, "relayed caller": relayed} {
		status, out := e.as(t, &v).keep(t, sess.ID, false, nil)
		require.Equal(t, http.StatusForbidden, status, "%s: %v", name, out)
		require.NotContains(t, out, "send", name)
		status, _ = e.as(t, &v).keep(t, sess.ID, false, gzipped(t, tLine))
		require.Equal(t, http.StatusForbidden, status, name)
	}
	require.False(t, e.svc.Kept(sess.ID))
	// A paired device's own credential, as the daemon's edge marks it.
	mux := http.NewServeMux()
	NewHandler(Config{DB: e.db, APIKey: testKey, LocalHost: localHostName, Transcripts: e.svc}).Register(mux)
	device := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cred := devices.Credential{DeviceID: "01DEVICEEXAMPLE", Kind: devices.KindKey, Name: "example-ci"}
		mux.ServeHTTP(w, r.WithContext(devices.WithCredential(r.Context(), cred)))
	}))
	t.Cleanup(device.Close)
	status, out := (&transcriptEnv{ts: device, db: e.db, svc: e.svc}).keep(t, sess.ID, true, nil)
	require.Equal(t, http.StatusForbidden, status, "a paired device: %v", out)

	status, out = e.keep(t, sess.ID, false, nil)
	require.Equal(t, http.StatusOK, status, "%v", out)
	require.Equal(t, map[string]any{"path": "/p/keep.jsonl", "host": localHostName, "client": "claude-code"}, out["send"])
}

// The keep route under peer_auth: the owner's socket account is named the
// path and sends it; the TCP key, which reads no agent file and has no
// socket, is told so; a session not flagged, or in a project locked to the
// caller, keeps nothing.
func TestTranscriptKeep_UnderPeerAuth(t *testing.T) {
	policy, err := access.New(config.PeerAuth{Owner: "owner"},
		func(string) (access.Account, error) { return access.Account{UID: 501, Name: "owner"}, nil })
	require.NoError(t, err)
	owner, ok := policy.PeerView(501)
	require.True(t, ok)
	e := newTranscriptEnv(t, &owner, 0)
	sess := e.start(t, ClientClaudeCode, "cc-keep-pa", "/p/keep.jsonl")
	plain := e.start(t, ClientClaudeCode, "cc-keep-plain", "/p/plain.jsonl")
	e.flag(t, sess)

	status, out := e.keep(t, plain.ID, false, nil)
	require.Equal(t, http.StatusConflict, status, "%v", out)
	status, _ = e.keep(t, "01MNOSUCHAAAAAAAAAAAAAAAAA", false, nil)
	require.Equal(t, http.StatusNotFound, status)
	status, out = e.keep(t, sess.ID, true, nil)
	require.Equal(t, http.StatusConflict, status, "the daemon reads no agent file under peer_auth: %v", out)
	tcp := policy.OwnerView()
	status, out = e.as(t, &tcp).keep(t, sess.ID, false, nil)
	require.Equal(t, http.StatusForbidden, status, "%v", out)
	locked, ok := policy.WithConsoleLocks([]string{"demo"}).PeerView(501)
	require.True(t, ok)
	status, out = e.as(t, &locked).keep(t, sess.ID, false, nil)
	require.Equal(t, http.StatusForbidden, status, "%v", out)
	require.Contains(t, out["error"], "locked")

	status, out = e.keep(t, sess.ID, false, gzipped(t, tLine))
	require.Equal(t, http.StatusOK, status, "%v", out)
	require.Equal(t, "copied", out["copy"])
	require.Equal(t, tLine, e.copyOf(t, sess.ID))
}

// The keep route with peer_auth off: the daemon reads the recorded path
// itself while the request waits, on its own computer only, and a body is
// never taken.
func TestTranscriptKeep_PeerAuthOff(t *testing.T) {
	e := newTranscriptEnv(t, nil, 0)
	path := writeTranscript(t, tLine)
	sess := e.start(t, ClientClaudeCode, "cc-keep-off", path)
	_, err := store.SetSessionFlag(context.Background(), e.db, sess.ID, core.VerdictGood, "", time.Now())
	require.NoError(t, err)

	status, out := e.keep(t, sess.ID, false, gzipped(t, tLine))
	require.Equal(t, http.StatusBadRequest, status, "%v", out)
	require.False(t, e.svc.Kept(sess.ID))
	status, out = e.keep(t, sess.ID, true, nil)
	require.Equal(t, http.StatusOK, status, "%v", out)
	require.Equal(t, "copied", out["copy"])
	require.Equal(t, tLine, e.copyOf(t, sess.ID))

	_, err = e.db.Exec(`UPDATE sessions SET host = ? WHERE id = ?`, remoteHostName, sess.ID)
	require.NoError(t, err)
	status, out = e.keep(t, sess.ID, true, nil)
	require.Equal(t, http.StatusConflict, status, "%v", out)
	require.Equal(t, string(core.TranscriptRemote), out["copy"])
}
