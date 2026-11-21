package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/harvest"
	"github.com/arctop/seamless/internal/store"
)

// bfLine is one synthetic Claude Code assistant line.
func bfLine(id, ts, model string, sidechain bool, in, out int) string {
	return `{"type":"assistant","timestamp":"` + ts + `","isSidechain":` + strconv.FormatBool(sidechain) +
		`,"message":{"id":"` + id + `","role":"assistant","model":"` + model + `","content":[{"type":"text","text":"x"}],` +
		`"usage":{"input_tokens":` + strconv.Itoa(in) + `,"output_tokens":` + strconv.Itoa(out) + `}}}`
}

// bfMachine is the owner's computer for the backfill tests: a home whose
// Claude Code transcript folder holds some sessions' transcripts (one with a
// subagent's beside it), and a stand-in daemon that recorded them all.
type bfMachine struct {
	d           captureDB
	projects    string            // ~/.claude/projects/-work-ops
	sessions    map[string]string // external id -> session ULID
	transcripts map[string]string // external id -> the transcript on disk
}

// newBFMachine builds the machine:
//
//	bf-recorded  ended; its hooks recorded its transcript, which has a subagent's beside it
//	bf-byid      ended before its hooks recorded a path: found by its session id
//	bf-gone      ended; the recorded transcript is no longer there (a missing file)
//	bf-outside   ended; the recorded path is outside Claude Code's folder, and nothing there is named after it
//	bf-filled    ended; its ledger already holds its transcript's rows
//	bf-live      still live
//	bf-codex     a Codex session
//	bf-remote    recorded on another computer (a foreign host)
func newBFMachine(t *testing.T) *bfMachine {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	m := &bfMachine{projects: filepath.Join(home, ".claude", "projects", "-work-ops"),
		sessions: map[string]string{}, transcripts: map[string]string{}}
	require.NoError(t, os.MkdirAll(m.projects, 0o700))
	m.d = captureDaemon(t, captureFixture{plans: filepath.Join(home, ".claude", "plans"), cwd: filepath.Join(home, "work", "ops")})
	ctx := context.Background()
	day := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	write := func(ext string, lines ...string) string {
		path := filepath.Join(m.projects, ext+".jsonl")
		require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600))
		m.transcripts[ext] = path
		return path
	}
	recorded := write("bf-recorded",
		bfLine("r1", "2026-11-18T09:01:00Z", "claude-x", false, 100, 10),
		bfLine("r2", "2026-11-18T09:20:00Z", "claude-x", false, 20, 2))
	subagents := filepath.Join(strings.TrimSuffix(recorded, ".jsonl"), "subagents")
	require.NoError(t, os.MkdirAll(subagents, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(subagents, "agent-a1.jsonl"),
		[]byte(bfLine("s1", "2026-11-18T09:05:00Z", "claude-small", true, 7, 3)+"\n"), 0o600))
	write("bf-byid", bfLine("b1", "2026-11-18T10:01:00Z", "claude-x", false, 40, 4))
	write("bf-filled", bfLine("f1", "2026-11-18T11:01:00Z", "claude-x", false, 50, 5))
	write("bf-live", bfLine("l1", "2026-11-18T12:01:00Z", "claude-x", false, 60, 6))
	outside := filepath.Join(t.TempDir(), "bf-outside.jsonl")
	require.NoError(t, os.WriteFile(outside, []byte(bfLine("o1", "2026-11-18T13:01:00Z", "claude-x", false, 999, 9)+"\n"), 0o600))

	for i, s := range []struct {
		ext, client, host, path string
		status                  core.SessionStatus
	}{
		{"bf-recorded", "claude-code", config.Hostname(), recorded, core.SessionCompleted},
		{"bf-byid", "claude-code", config.Hostname(), "", core.SessionExpired},
		{"bf-gone", "claude-code", config.Hostname(), filepath.Join(m.projects, "bf-gone.jsonl"), core.SessionCompleted},
		{"bf-outside", "claude-code", config.Hostname(), outside, core.SessionCompleted},
		{"bf-filled", "claude-code", config.Hostname(), m.transcripts["bf-filled"], core.SessionCompleted},
		{"bf-live", "claude-code", config.Hostname(), m.transcripts["bf-live"], core.SessionActive},
		{"bf-codex", "codex", config.Hostname(), "", core.SessionExpired},
		{"bf-remote", "claude-code", "example-remote-host", "", core.SessionCompleted},
	} {
		id := fmt.Sprintf("01BFCLI%019d", i+1)
		require.NoError(t, store.CreateSession(ctx, m.d.db, core.Session{ID: id, Name: "cc/" + s.ext, ProjectSlug: "example-ops",
			Status: s.status, Ambient: true, ExternalClient: s.client, ExternalSessionID: s.ext, Host: s.host,
			CreatedAt: day.Add(time.Duration(i) * time.Minute), UpdatedAt: day.Add(time.Hour)}))
		require.NoError(t, store.SetSessionTranscriptPath(ctx, m.d.db, id, s.path))
		m.sessions[s.ext] = id
	}
	_, rows, ok := harvest.ClaudeUsage(m.transcripts["bf-filled"])
	require.True(t, ok)
	_, err := store.ReplaceConversationUsage(ctx, m.d.db, "claude-code", "bf-filled", rows, day.Add(2*time.Hour))
	require.NoError(t, err)
	return m
}

// ledgers is every session's ledger rows, by external id.
func (m *bfMachine) ledgers(t *testing.T) map[string][]core.UsageBucket {
	t.Helper()
	out := map[string][]core.UsageBucket{}
	for ext, id := range m.sessions {
		out[ext] = m.d.ledger(t, id)
	}
	return out
}

// runBackfill runs `seam usage backfill` with args against cfg.
func runBackfill(t *testing.T, cfg config.Config, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	e, out, errb := stubEnv()
	e.loadConfig = func() (config.Config, error) { return cfg, nil }
	code = dispatch(context.Background(), e, append([]string{"usage", "backfill"}, args...))
	return out.String(), errb.String(), code
}

// bfTransports are the two ways the command reaches its daemon: the key over
// TCP with peer_auth off, and the owner's account on the peer_auth socket.
func bfTransports(t *testing.T, m *bfMachine) map[string]config.Config {
	t.Helper()
	srv := httptest.NewServer(m.d.mux)
	t.Cleanup(srv.Close)
	out := map[string]config.Config{"tcp": tcpConfig(strings.TrimPrefix(srv.URL, "http://"))}
	if runtime.GOOS == "windows" {
		return out // the peer_auth socket is a Unix deployment
	}
	policy, err := access.New(config.PeerAuth{Owner: "owner", ForceLocked: []string{"example-finance"}},
		func(string) (access.Account, error) { return access.Account{UID: 501, Name: "owner"}, nil })
	require.NoError(t, err)
	view, ok := policy.PeerView(501)
	require.True(t, ok)
	out["socket"] = socketConfig(serveSocket(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.d.mux.ServeHTTP(w, r.WithContext(access.WithView(r.Context(), view)))
	})))
	return out
}

// The dry run says what a run would write and writes nothing; the run writes
// it -- each session's whole transcript with its subagents', found where its
// hooks recorded it or by its session id -- and leaves alone a missing file, a
// path outside Claude Code's folder, another computer's session, a live one,
// a Codex one and an already-filled one; a second run writes nothing. The same
// on both transports: the key over TCP (the daemon could read the disk, and
// reads nothing) and the peer_auth socket (it may not, and does not).
func TestUsageBackfill_DryRunThenRunOnBothTransports(t *testing.T) {
	for _, transport := range []string{"tcp", "socket"} {
		t.Run(transport, func(t *testing.T) {
			m := newBFMachine(t)
			cfg, ok := bfTransports(t, m)[transport]
			if !ok {
				t.Skip("the peer_auth socket is a Unix deployment")
			}
			before := m.ledgers(t)
			wantRows := map[string][]core.UsageBucket{}
			for _, ext := range []string{"bf-recorded", "bf-byid"} {
				_, rows, ok := harvest.ClaudeUsage(m.transcripts[ext])
				require.True(t, ok)
				wantRows[ext] = rows
			}
			require.Len(t, wantRows["bf-recorded"], 3, "its two quarter-hours and its subagent's row")
			recordedTotal := core.SumUsage(wantRows["bf-recorded"]).Total
			byIDTotal := core.SumUsage(wantRows["bf-byid"]).Total
			require.Equal(t, 100+10+20+2+7+3, recordedTotal)

			out, stderr, code := runBackfill(t, cfg, "--dry-run")
			require.Equal(t, 0, code, stderr)
			require.Equal(t, strings.Join([]string{
				"usage backfill on " + config.Hostname() + " (dry run: nothing written)",
				"  sessions on this computer: 7 (1 more recorded on other computers)",
				"  with a transcript here:    3 (2 missing)",
				"  skipped:                   1 live, 1 Codex",
				"  already filled:            1",
				fmt.Sprintf("  %-26s %d", "would fill:", 2),
				fmt.Sprintf("  model tokens of those:     0 before, %d after", recordedTotal+byIDTotal),
				fmt.Sprintf("  would fill %-24s %d -> %d tokens", "cc/bf-recorded", 0, recordedTotal),
				fmt.Sprintf("  would fill %-24s %d -> %d tokens", "cc/bf-byid", 0, byIDTotal),
			}, "\n")+"\n", out)
			require.Equal(t, before, m.ledgers(t), "a dry run writes nothing")

			out, stderr, code = runBackfill(t, cfg, "--json")
			require.Equal(t, 0, code, stderr)
			var res backfillOut
			require.NoError(t, json.Unmarshal([]byte(out), &res))
			require.False(t, res.DryRun)
			require.Equal(t, 1, res.OtherHosts)
			got := map[string]string{}
			for _, s := range res.Sessions {
				got[strings.TrimPrefix(s.Name, "cc/")] = s.Result
			}
			require.Equal(t, map[string]string{
				"bf-recorded": harvest.BackfillWritten, "bf-byid": harvest.BackfillWritten,
				"bf-gone": backfillMissing, "bf-outside": backfillMissing, "bf-filled": harvest.BackfillUnchanged,
				"bf-live": harvest.BackfillActive, "bf-codex": backfillCodex,
			}, got)
			after := m.ledgers(t)
			require.Equal(t, wantRows["bf-recorded"], after["bf-recorded"], "the transcript's rows, its subagent's included")
			require.Equal(t, wantRows["bf-byid"], after["bf-byid"])
			for _, ext := range []string{"bf-gone", "bf-outside", "bf-filled", "bf-live", "bf-codex", "bf-remote"} {
				require.Equal(t, before[ext], after[ext], "%s is left as it was", ext)
			}
			for _, ext := range []string{"bf-recorded", "bf-byid"} {
				sess, ok, err := store.SessionByID(context.Background(), m.d.db, m.sessions[ext])
				require.NoError(t, err)
				require.True(t, ok)
				require.Equal(t, core.SumUsage(after[ext]), sess.Tokens, "%s: the totals are its ledger's sum", ext)
				require.NotEqual(t, core.SessionActive, sess.Status, "%s: never revived", ext)
			}

			out, stderr, code = runBackfill(t, cfg)
			require.Equal(t, 0, code, stderr)
			require.Contains(t, out, fmt.Sprintf("  %-26s %d\n", "already filled:", 3))
			require.Contains(t, out, fmt.Sprintf("  %-26s %d\n", "filled:", 0))
			require.NotContains(t, out, "model tokens of those")
			require.Equal(t, after, m.ledgers(t), "a second run writes nothing")
		})
	}
}

// A restricted local account running the command on the socket is refused
// before anything is listed, and it exits non-zero with the daemon's words.
func TestUsageBackfill_ARestrictedAccountIsRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the peer_auth socket is a Unix deployment")
	}
	const acct = "_example_agent"
	m := newBFMachine(t)
	policy, err := access.New(config.PeerAuth{Owner: "owner", Restricted: config.BareRestricted(acct)},
		func(n string) (access.Account, error) {
			if n == acct {
				return access.Account{UID: 502, Name: acct}, nil
			}
			return access.Account{UID: 501, Name: "owner"}, nil
		})
	require.NoError(t, err)
	sock := serveSocket(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v, ok := policy.PeerView(502)
		open, err := store.OpenProjectSlugs(r.Context(), m.d.db)
		if !ok || err != nil {
			http.Error(w, "no view", http.StatusServiceUnavailable)
			return
		}
		m.d.mux.ServeHTTP(w, r.WithContext(access.WithView(r.Context(), v.WithOpenProjects(open))))
	}))
	before := m.ledgers(t)
	out, stderr, code := runBackfill(t, socketConfig(sock))
	require.Equal(t, 1, code)
	require.Empty(t, out, "nothing listed")
	require.Contains(t, stderr, "the usage backfill is the owner's")
	require.Equal(t, before, m.ledgers(t))
}

// A daemon that cannot be reached, or that refuses one session, makes the
// command exit non-zero: a script never reads a half-done backfill as done.
func TestUsageBackfill_FailuresExitNonZero(t *testing.T) {
	m := newBFMachine(t)
	_, stderr, code := runBackfill(t, tcpConfig("127.0.0.1:1"))
	require.Equal(t, 1, code)
	require.Contains(t, stderr, "daemon unreachable")

	// A daemon older than the route.
	older := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(older.Close)
	_, stderr, code = runBackfill(t, tcpConfig(strings.TrimPrefix(older.URL, "http://")))
	require.Equal(t, 1, code)
	require.Contains(t, stderr, "has no usage backfill: it is older than this seam")

	// A daemon that lists the sessions and refuses every fill.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"the session's ledger could not be written"}`))
			return
		}
		m.d.mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	out, stderr, code := runBackfill(t, tcpConfig(strings.TrimPrefix(srv.URL, "http://")))
	require.Equal(t, 1, code)
	require.Contains(t, out, "  not filled cc/bf-recorded: the session's ledger could not be written\n")
	require.Contains(t, stderr, "3 sessions were not filled", "bf-recorded, bf-byid and bf-filled were each sent")
}
