package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

// socketDaemon is the real hooks handler and transcript copies behind a real
// Unix socket, every request carrying view: the daemon under peer_auth, as
// seam hook meets it.
type socketDaemon struct {
	db      *sql.DB
	svc     *transcripts.Service
	cfg     config.Config
	uploads atomic.Int32 // requests to the upload routes
	pushes  atomic.Int32 // requests to the mid-session usage route
}

func newSocketDaemon(t *testing.T, f captureFixture, view access.View) *socketDaemon {
	t.Helper()
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
	svc, err := transcripts.New(transcripts.Config{DataDir: dir, DB: db, Events: rec, LocalHost: config.Hostname(), PeerAuth: true})
	require.NoError(t, err)
	h := hooks.NewHandler(hooks.Config{
		DB: db, Retrieve: retrieve.New(db, nil, config.Budgets{MaxBriefingTokens: 1500, RecallBudgetTokens: 1000}, nil),
		Events: rec, APIKey: "k", LocalHost: config.Hostname(), Transcripts: svc,
	})
	mux := http.NewServeMux()
	h.Register(mux)
	d := &socketDaemon{db: db, svc: svc}
	sock := serveSocket(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, harvest.TranscriptEndpoint) {
			d.uploads.Add(1)
		}
		if r.URL.Path == harvest.UsageEndpoint {
			d.pushes.Add(1)
		}
		mux.ServeHTTP(w, r.WithContext(access.WithView(r.Context(), view)))
	}))
	d.cfg = socketConfig(sock)
	return d
}

// ownerSocketView is the owner's View on the peer_auth socket.
func ownerSocketView(t *testing.T) access.View {
	t.Helper()
	policy, err := access.New(config.PeerAuth{Owner: "owner"},
		func(string) (access.Account, error) { return access.Account{UID: 501, Name: "owner"}, nil })
	require.NoError(t, err)
	view, ok := policy.PeerView(501)
	require.True(t, ok)
	return view
}

// inProcess runs each detached upload a hook starts right here, as `seam
// hook-transcript` would run with cfg, and records its command line.
func inProcess(t *testing.T, cfg config.Config, jobs *[][]string) func([]string) error {
	t.Helper()
	return func(args []string) error {
		*jobs = append(*jobs, args)
		p, err := parse(commands(), args)
		if err != nil {
			return err
		}
		e, _, errb := stubEnv()
		e.loadConfig = func() (config.Config, error) { return cfg, nil }
		err = p.cmd.run(context.Background(), e, p.opts, p.pos)
		if errb.Len() > 0 {
			t.Logf("%v: %s", args, errb.String())
		}
		return err
	}
}

func (d *socketDaemon) session(t *testing.T, client, id string) core.Session {
	t.Helper()
	sess, found, err := store.AmbientSessionByExternalIdentity(context.Background(), d.db, client, id)
	require.NoError(t, err)
	require.True(t, found, "%s %s", client, id)
	return sess
}

func (d *socketDaemon) reload(t *testing.T, id string) core.Session {
	t.Helper()
	sess, found, err := store.SessionByID(context.Background(), d.db, id)
	require.NoError(t, err)
	require.True(t, found)
	return sess
}

func (d *socketDaemon) copyOf(t *testing.T, id string) string {
	t.Helper()
	rc, err := d.svc.Open(id)
	require.NoError(t, err)
	defer func() { _ = rc.Close() }()
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	return string(got)
}

// The push path end to end, over a real peer_auth socket: the daemon opens no
// agent file, so a flagged session's SessionEnd answer asks for the
// transcript, `seam hook session-end` hands it to a detached `seam
// hook-transcript`, which sends it gzip-compressed beside the agent, and the
// daemon keeps it -- the same bytes the agent client wrote. An unflagged
// session's end starts nothing.
func TestRunHook_PushesAFlaggedTranscriptOverTheSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the peer_auth socket is a Unix deployment")
	}
	f := newCaptureFixture(t)
	ctx := context.Background()
	d := newSocketDaemon(t, f, ownerSocketView(t))
	var jobs [][]string
	spawn := inProcess(t, d.cfg, &jobs)

	start := map[string]any{"session_id": "cc-push", "cwd": f.cwd, "source": "startup", "transcript_path": f.transcript}
	require.Empty(t, runHookSpawning(t, d.cfg, hookCall{event: "session-start", payload: start}, spawn))
	sess := d.session(t, "claude-code", "cc-push")
	require.Equal(t, f.transcript, sess.TranscriptPath, "the path is recorded though the daemon never opens it")

	end := map[string]any{"session_id": "cc-push", "transcript_path": f.transcript, "reason": "other"}
	require.Empty(t, runHookSpawning(t, d.cfg, hookCall{event: "session-end", payload: end}, spawn))
	require.Empty(t, jobs, "an unflagged session's end starts no upload")
	require.Zero(t, d.uploads.Load())

	_, err := d.svc.Flag(ctx, sess, core.VerdictGood, "", "agent", true)
	require.NoError(t, err)
	require.Empty(t, runHookSpawning(t, d.cfg, hookCall{event: "session-end", payload: end}, spawn))
	require.Equal(t, [][]string{{"hook-transcript", "--session-id", "cc-push", "--path", f.transcript, "--wanted", "0"}}, jobs)
	require.Equal(t, int32(1), d.uploads.Load())
	want, err := os.ReadFile(f.transcript)
	require.NoError(t, err)
	require.Equal(t, string(want), d.copyOf(t, sess.ID))
	row := d.reload(t, sess.ID)
	require.Equal(t, core.TranscriptCopied, row.Transcript)
	require.Equal(t, int64(len(want)), row.TranscriptBytes)
}

// The hook returns before the transcript is sent: it starts the upload as a
// detached process of its own (here a fake that only records), with the
// hook's --config and --client, and sends nothing itself.
func TestRunHook_StartsTheUploadDetached(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the peer_auth socket is a Unix deployment")
	}
	f := newCaptureFixture(t)
	d := newSocketDaemon(t, f, ownerSocketView(t))
	start := map[string]any{"session_id": "cx-detach", "cwd": f.cwd, "source": "startup", "model": "gpt-5.5", "transcript_path": f.rollout}
	require.Empty(t, runHookCall(t, d.cfg, hookCall{event: "session-start", client: hookClientCodex, payload: start}))
	sess := d.session(t, "codex", "cx-detach")
	_, err := d.svc.Flag(context.Background(), sess, core.VerdictNotable, "", "agent", false)
	require.NoError(t, err)

	raw, err := json.Marshal(map[string]any{"session_id": "cx-detach", "cwd": f.cwd, "transcript_path": f.rollout})
	require.NoError(t, err)
	var started [][]string
	o := &hookOpts{config: "/etc/seamless/seamless.yaml", client: hookClientCodex, spawn: func(args []string) error {
		started = append(started, args)
		return nil
	}}
	stop := func() (stdout, stderr string) {
		t.Helper()
		e, out, errb := stubEnv()
		e.stdin = strings.NewReader(string(raw))
		e.loadConfig = func() (config.Config, error) { return d.cfg, nil }
		require.NoError(t, runHook(context.Background(), e, o, []string{"stop"}))
		return out.String(), errb.String()
	}
	out, errOut := stop()
	require.Empty(t, errOut)
	require.NotEmpty(t, out, "the reply went out")
	require.Equal(t, [][]string{{"hook-transcript", "--config", "/etc/seamless/seamless.yaml", "--client", "codex",
		"--session-id", "cx-detach", "--path", f.rollout, "--wanted", "0"}}, started)
	require.Zero(t, d.uploads.Load(), "the hook itself sends nothing")

	// A spawner that fails costs the copy, never the hook.
	o.spawn = func([]string) error { return os.ErrPermission }
	out, errOut = stop()
	require.NotEmpty(t, out)
	require.Contains(t, errOut, "seam hook: transcript not sent: ")
}

// transcriptJobs reads the end signal's wanted size and each pending value,
// for the events that carry them, and skips what does not parse.
func TestTranscriptJobs(t *testing.T) {
	pend := func(id, path string) string {
		return url.Values{harvest.TranscriptSessionParam: {id}, harvest.TranscriptPathParam: {path}}.Encode()
	}
	payload := []byte(`{"session_id":"cc-a","transcript_path":"/t/a.jsonl"}`)
	for _, tc := range []struct {
		name    string
		o       hookOpts
		event   string
		payload []byte
		wanted  string
		pending []string
		want    [][]string
	}{
		{"nothing asked", hookOpts{}, "session-end", payload, "", nil, nil},
		{"the end signal's", hookOpts{}, "session-end", payload, "120", nil,
			[][]string{{"hook-transcript", "--session-id", "cc-a", "--path", "/t/a.jsonl", "--wanted", "120"}}},
		{"wanted on an event that is no end signal", hookOpts{}, "session-start", payload, "0", nil, nil},
		{"wanted that does not parse", hookOpts{}, "stop", payload, "-1", nil, nil},
		{"a payload naming no transcript", hookOpts{}, "session-end", []byte(`{"session_id":"cc-a"}`), "0", nil, nil},
		{"pending, with the hook's flags", hookOpts{config: "/c.yaml", client: hookClientCodex}, "session-start", payload, "",
			[]string{pend("cx-old", "/s/old.jsonl")},
			[][]string{{"hook-transcript", "--config", "/c.yaml", "--client", "codex", "--named", "--session-id", "cx-old", "--path", "/s/old.jsonl"}}},
		{"both, and the bad values skipped", hookOpts{}, "session-end", payload, "0",
			[]string{"%zz", pend("", "/s/x.jsonl"), pend("cc-b", "/p/b.jsonl")},
			[][]string{
				{"hook-transcript", "--session-id", "cc-a", "--path", "/t/a.jsonl", "--wanted", "0"},
				{"hook-transcript", "--named", "--session-id", "cc-b", "--path", "/p/b.jsonl"},
			}},
		{"at most a few pending an answer", hookOpts{}, "session-start", payload, "",
			[]string{pend("1", "/p/1.jsonl"), pend("2", "/p/2.jsonl"), pend("3", "/p/3.jsonl"), pend("4", "/p/4.jsonl")},
			[][]string{
				{"hook-transcript", "--named", "--session-id", "1", "--path", "/p/1.jsonl"},
				{"hook-transcript", "--named", "--session-id", "2", "--path", "/p/2.jsonl"},
				{"hook-transcript", "--named", "--session-id", "3", "--path", "/p/3.jsonl"},
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, transcriptJobs(&tc.o, tc.event, tc.payload, tc.wanted, tc.pending))
		})
	}
}

// The transcript folders follow the client's own override, else the home.
func TestTranscriptRoots(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, tc := range []struct {
		name, client, claudeDir, codexHome string
		want                               []string
	}{
		{"claude code, default", "", "", "", []string{filepath.Join(home, ".claude", "projects")}},
		{"claude code, CLAUDE_CONFIG_DIR", hookClientClaudeCode, filepath.Join(home, "cc"), "", []string{filepath.Join(home, "cc", "projects")}},
		{"claude code, ~ in CLAUDE_CONFIG_DIR", "", "~/cc", "", []string{filepath.Join(home, "cc", "projects")}},
		{"claude code, a relative override names none", "", "cc", "", nil},
		{"codex, default", hookClientCodex, "", "", []string{filepath.Join(home, ".codex", "sessions")}},
		{"codex, CODEX_HOME", hookClientCodex, filepath.Join(home, "cc"), filepath.Join(home, "cx"), []string{filepath.Join(home, "cx", "sessions")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", tc.claudeDir)
			t.Setenv("CODEX_HOME", tc.codexHome)
			require.Equal(t, tc.want, transcriptRoots(tc.client))
		})
	}
}

// A path the daemon named is read only when it is a regular *.jsonl file
// inside the client's own transcript folder; anything else is refused, or
// reported missing, unread.
func TestOpenNamedTranscript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows, and peer_auth serves macOS and Linux")
	}
	base := t.TempDir()
	root := filepath.Join(base, ".claude", "projects")
	proj := filepath.Join(root, "-work-ops")
	require.NoError(t, os.MkdirAll(proj, 0o700))
	outside := filepath.Join(base, "elsewhere")
	require.NoError(t, os.MkdirAll(outside, 0o700))
	write := func(path, body string) string {
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
		return path
	}
	good := write(filepath.Join(proj, "a.jsonl"), "{}\n")
	secret := write(filepath.Join(outside, "secret.jsonl"), "secret\n")
	link := filepath.Join(proj, "link.jsonl")
	require.NoError(t, os.Symlink(secret, link))
	escape := filepath.Join(root, "escape")
	require.NoError(t, os.Symlink(outside, escape))
	notJSONL := write(filepath.Join(proj, "a.json"), "{}\n")
	require.NoError(t, os.MkdirAll(filepath.Join(proj, "dir.jsonl"), 0o700))
	// The client's folder itself behind a symlink, as a dotfiles checkout
	// puts it: what counts is the folder the file resolves into.
	linkedHome := filepath.Join(base, "linked")
	require.NoError(t, os.Symlink(filepath.Join(base, ".claude"), linkedHome))

	for _, tc := range []struct {
		name, path string
		roots      []string
		want       string
	}{
		{"inside the folder", good, []string{root}, ""},
		{"inside, through a symlinked folder", filepath.Join(linkedHome, "projects", "-work-ops", "a.jsonl"),
			[]string{filepath.Join(linkedHome, "projects")}, ""},
		{"another folder", secret, []string{root}, harvest.TranscriptRefusedPath},
		{"a symlink to another folder's file", link, []string{root}, harvest.TranscriptRefusedPath},
		{"through a symlink out of the folder", filepath.Join(escape, "secret.jsonl"), []string{root}, harvest.TranscriptRefusedPath},
		{"not *.jsonl", notJSONL, []string{root}, harvest.TranscriptRefusedPath},
		{"a folder named *.jsonl", filepath.Join(proj, "dir.jsonl"), []string{root}, harvest.TranscriptRefusedPath},
		{"relative", filepath.Join("projects", "a.jsonl"), []string{root}, harvest.TranscriptRefusedPath},
		{"not clean", proj + "/../-work-ops/a.jsonl", []string{root}, harvest.TranscriptRefusedPath},
		{"missing", filepath.Join(proj, "gone.jsonl"), []string{root}, harvest.TranscriptRefusedMissing},
		{"no folder to read in", good, nil, harvest.TranscriptRefusedPath},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, size, refusal := openNamedTranscript(tc.path, tc.roots)
			require.Equal(t, tc.want, refusal)
			if tc.want != "" {
				require.Nil(t, f)
				return
			}
			defer func() { _ = f.Close() }()
			require.Equal(t, int64(3), size)
		})
	}
}

// A flagged session whose end signal never came is asked for at the same
// caller's next start, over the socket: the detached upload reads the
// recorded path inside the client's own folder and the copy is kept. A path
// outside it is reported, never read, and the asks for it end.
func TestRunHook_SendsAPendingTranscriptAtTheNextStart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the peer_auth socket is a Unix deployment")
	}
	f := newCaptureFixture(t)
	ctx := context.Background()
	d := newSocketDaemon(t, f, ownerSocketView(t))
	var jobs [][]string
	spawn := inProcess(t, d.cfg, &jobs)
	proj := filepath.Join(f.home, ".claude", "projects", "-work-ops")
	require.NoError(t, os.MkdirAll(proj, 0o700))
	crashed := filepath.Join(proj, "crashed.jsonl")
	require.NoError(t, os.WriteFile(crashed, []byte(`{"type":"user","message":{"content":"crashed"}}`+"\n"), 0o600))
	start := func(id, transcript string) {
		t.Helper()
		require.Empty(t, runHookSpawning(t, d.cfg, hookCall{event: "session-start",
			payload: map[string]any{"session_id": id, "cwd": f.cwd, "source": "startup", "transcript_path": transcript}}, spawn))
	}

	// The session that crashed: started, flagged, never ended.
	start("cc-crashed", crashed)
	sess := d.session(t, "claude-code", "cc-crashed")
	_, err := d.svc.Flag(ctx, sess, core.VerdictBad, "it crashed", "agent", false)
	require.NoError(t, err)
	require.Empty(t, jobs)

	// Its owner's next start: asked for, sent, kept.
	start("cc-next", f.transcript)
	require.Equal(t, [][]string{{"hook-transcript", "--named", "--session-id", "cc-crashed", "--path", crashed}}, jobs)
	want, err := os.ReadFile(crashed)
	require.NoError(t, err)
	require.Equal(t, string(want), d.copyOf(t, sess.ID))
	require.Equal(t, core.TranscriptCopied, d.reload(t, sess.ID).Transcript)

	// Kept: the next start asks for nothing.
	jobs = nil
	start("cc-third", f.transcript)
	require.Empty(t, jobs)

	// A session whose recorded path lies outside the client's folder (here the
	// fixture's $HOME/session.jsonl): reported, not read, and asked no more.
	start("cc-out", f.transcript)
	out := d.session(t, "claude-code", "cc-out")
	_, err = d.svc.Flag(ctx, out, core.VerdictNotable, "", "agent", false)
	require.NoError(t, err)
	uploads := d.uploads.Load()
	start("cc-fourth", f.transcript)
	require.Equal(t, [][]string{{"hook-transcript", "--named", "--session-id", "cc-out", "--path", f.transcript}}, jobs)
	require.Equal(t, uploads+1, d.uploads.Load(), "one report, no body")
	require.False(t, d.svc.Kept(out.ID))
	require.Equal(t, core.TranscriptRefused, d.reload(t, out.ID).Transcript)
	jobs = nil
	start("cc-fifth", f.transcript)
	require.Empty(t, jobs, "a refused path is not asked for again")
}

// Codex's per-turn Stop still sends a flagged rollout again only once it
// grew, through the detached upload.
func TestRunHook_CodexStopResendsOnlyOnGrowth(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the peer_auth socket is a Unix deployment")
	}
	f := newCaptureFixture(t)
	// A copy of the fixture rollout, which this test grows: never the
	// committed fixture itself.
	fixture, err := os.ReadFile(f.rollout)
	require.NoError(t, err)
	f.rollout = filepath.Join(t.TempDir(), "rollout.jsonl")
	require.NoError(t, os.WriteFile(f.rollout, fixture, 0o600))
	d := newSocketDaemon(t, f, ownerSocketView(t))
	var jobs [][]string
	spawn := inProcess(t, d.cfg, &jobs)
	require.Empty(t, runHookSpawning(t, d.cfg, hookCall{event: "session-start", client: hookClientCodex,
		payload: map[string]any{"session_id": "cx-grow", "cwd": f.cwd, "source": "startup", "model": "gpt-5.5", "transcript_path": f.rollout}}, spawn))
	sess := d.session(t, "codex", "cx-grow")
	_, err = d.svc.Flag(context.Background(), sess, core.VerdictGood, "", "agent", false)
	require.NoError(t, err)
	stop := func() {
		t.Helper()
		require.Empty(t, runHookSpawning(t, d.cfg, hookCall{event: "stop", client: hookClientCodex,
			payload: map[string]any{"session_id": "cx-grow", "cwd": f.cwd, "transcript_path": f.rollout, "model": "gpt-5.5"}}, spawn))
	}
	stop()
	first, err := os.ReadFile(f.rollout)
	require.NoError(t, err)
	require.Equal(t, string(first), d.copyOf(t, sess.ID))
	uploads := d.uploads.Load()

	stop()
	require.Len(t, jobs, 2, "every turn's end starts the upload")
	require.Equal(t, uploads, d.uploads.Load(), "unchanged: the upload finds nothing new and sends nothing")

	grown := string(first) + `{"type":"event_msg","payload":{"type":"agent_message","message":"more"}}` + "\n"
	require.NoError(t, os.WriteFile(f.rollout, []byte(grown), 0o600))
	stop()
	require.Equal(t, uploads+1, d.uploads.Load())
	require.Equal(t, grown, d.copyOf(t, sess.ID), "grown: sent again")
	require.Equal(t, int64(len(grown)), d.reload(t, sess.ID).TranscriptBytes)
}

// Off the socket the client sends no transcript, whatever an answer says: the
// rule every client-side capture keeps. Nothing is started either.
func TestRunHook_SendsNoTranscriptOffTheSocket(t *testing.T) {
	f := newCaptureFixture(t)
	uploads := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, harvest.TranscriptEndpoint) {
			uploads++
		}
		w.Header().Set(harvest.TranscriptWantedHeader, "0")
		w.Header().Add(harvest.TranscriptPendingHeader, url.Values{harvest.TranscriptSessionParam: {"cc-old"},
			harvest.TranscriptPathParam: {f.transcript}}.Encode())
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"continue":true,"suppressOutput":true}`)
	}))
	t.Cleanup(srv.Close)
	cfg := tcpConfig(strings.TrimPrefix(srv.URL, "http://"))
	started := 0
	spawn := func([]string) error { started++; return nil }
	for _, event := range []string{"session-end", "session-start"} {
		require.Empty(t, runHookSpawning(t, cfg, hookCall{event: event,
			payload: map[string]any{"session_id": "cc-x", "transcript_path": f.transcript}}, spawn))
	}
	require.Zero(t, uploads)
	require.Zero(t, started)

	// The detached command itself sends nothing off the socket either.
	e, _, _ := stubEnv()
	e.loadConfig = func() (config.Config, error) { return cfg, nil }
	require.NoError(t, runHookTranscript(context.Background(), e, &hookTranscriptOpts{sessionID: "cc-x", path: f.transcript}, nil))
	require.Zero(t, uploads)
}

// The owner keeps a flagged session's transcript by hand. Under peer_auth the
// daemon names the recorded path and this account sends it, only from inside
// the client's own folder; with peer_auth off the daemon copies it itself.
func TestKeepTranscript_BothModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the peer_auth socket is a Unix deployment")
	}
	f := newCaptureFixture(t)
	ctx := context.Background()
	proj := filepath.Join(f.home, ".claude", "projects", "-work-ops")
	require.NoError(t, os.MkdirAll(proj, 0o700))
	inside := filepath.Join(proj, "kept.jsonl")
	const content = `{"type":"user","message":{"content":"keep me"}}` + "\n"
	require.NoError(t, os.WriteFile(inside, []byte(content), 0o600))

	t.Run("peer_auth: this account sends it", func(t *testing.T) {
		d := newSocketDaemon(t, f, ownerSocketView(t))
		var jobs [][]string
		spawn := inProcess(t, d.cfg, &jobs)
		start := func(id, transcript string) core.Session {
			t.Helper()
			require.Empty(t, runHookSpawning(t, d.cfg, hookCall{event: "session-start",
				payload: map[string]any{"session_id": id, "cwd": f.cwd, "source": "startup", "transcript_path": transcript}}, spawn))
			return d.session(t, "claude-code", id)
		}
		sess := start("cc-keep", inside)
		_, err := d.svc.Flag(ctx, sess, core.VerdictGood, "", "owner", false)
		require.NoError(t, err)
		var out strings.Builder
		require.NoError(t, keepTranscript(ctx, &out, d.cfg, sess.ID, nil))
		require.Equal(t, "copy kept; read it with `seam sessions "+sess.ID+" --transcript`\n", out.String())
		require.Equal(t, content, d.copyOf(t, sess.ID))
		evs, err := events.NewRecorder(d.db).BySession(ctx, sess.ID, 50)
		require.NoError(t, err)
		var via []any
		for _, ev := range evs {
			if ev.Kind == core.EventSessionTranscript {
				via = append(via, ev.Payload["via"])
			}
		}
		require.Equal(t, []any{transcripts.ViaOwner}, via, "the owner's own account read it")

		// A recorded path outside the client's folder is not read.
		outSess := start("cc-keep-out", f.transcript)
		_, err = d.svc.Flag(ctx, outSess, core.VerdictGood, "", "owner", false)
		require.NoError(t, err)
		err = keepTranscript(ctx, io.Discard, d.cfg, outSess.ID, nil)
		require.ErrorContains(t, err, "is not a regular *.jsonl file inside "+filepath.Join(f.home, ".claude", "projects")+" (claude-code's own transcript folder for this account)")
		require.False(t, d.svc.Kept(outSess.ID))

		// An unflagged session keeps nothing.
		plain := start("cc-plain", inside)
		require.ErrorContains(t, keepTranscript(ctx, io.Discard, d.cfg, plain.ID, nil), "is not flagged")
		require.False(t, d.svc.Kept(plain.ID))
	})

	t.Run("peer_auth off: the daemon reads it", func(t *testing.T) {
		dir := t.TempDir()
		db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = db.Close() })
		require.NoError(t, store.AdoptLocalHost(ctx, db, config.Hostname()))
		rec := events.NewRecorder(db)
		svc, err := transcripts.New(transcripts.Config{DataDir: dir, DB: db, Events: rec, LocalHost: config.Hostname()})
		require.NoError(t, err)
		h := hooks.NewHandler(hooks.Config{
			DB: db, Retrieve: retrieve.New(db, nil, config.Budgets{MaxBriefingTokens: 1500, RecallBudgetTokens: 1000}, nil),
			Events: rec, APIKey: "k", LocalHost: config.Hostname(), Transcripts: svc,
		})
		mux := http.NewServeMux()
		h.Register(mux)
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		cfg := tcpConfig(strings.TrimPrefix(srv.URL, "http://"))
		session := func(id, ext, host string) core.Session {
			t.Helper()
			now := time.Now().UTC()
			sess := core.Session{ID: id, Name: "cc/" + ext, Status: core.SessionExpired, Ambient: true,
				ExternalClient: "claude-code", ExternalSessionID: ext, Host: host, Source: "startup", CreatedAt: now, UpdatedAt: now}
			require.NoError(t, store.CreateSession(ctx, db, sess))
			require.NoError(t, store.SetSessionTranscriptPath(ctx, db, id, inside))
			// Flagged in the store directly: a flag set after the end would
			// copy it at once (AfterEnd), and this is the copy by hand.
			_, err := store.SetSessionFlag(ctx, db, id, core.VerdictGood, "", now)
			require.NoError(t, err)
			return sess
		}
		sess := session("01MKEEPOFFAAAAAAAAAAAAAAAA", "cc-keep-off", config.Hostname())
		var out strings.Builder
		require.NoError(t, keepTranscript(ctx, &out, cfg, sess.ID, nil))
		require.Contains(t, out.String(), "copy kept")
		require.True(t, svc.Kept(sess.ID))
		row, _, err := store.SessionByID(ctx, db, sess.ID)
		require.NoError(t, err)
		require.Equal(t, core.TranscriptCopied, row.Transcript)

		// A session on another computer is not the daemon's to read.
		far := session("01MKEEPFARAAAAAAAAAAAAAAAA", "cc-keep-far", "example-far-host")
		require.ErrorContains(t, keepTranscript(ctx, io.Discard, cfg, far.ID, nil), "another computer")
		require.False(t, svc.Kept(far.ID))
	})
}
