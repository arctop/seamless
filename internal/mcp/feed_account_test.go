package mcp_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/commitfeed"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/feed"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/hooks"
	mcpserver "github.com/arctop/seamless/internal/mcp"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/screening"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// The security monitor's view of a restricted local account, end to end: the
// account's agent and the owner's each start a session, write a memory,
// propose or write a rule, add and update a task with a recorded commit,
// reach for a project they may not, end the session with a commit, and send a
// prompt through the hooks outside any session. The metadata feed's frames and
// the commit snapshot name the account -- with its qualified session name -- on
// everything it did, bound session or not, and carry nothing new for the owner.

var (
	commitA = strings.Repeat("a", 40)
	commitB = strings.Repeat("b", 40)
	commitC = strings.Repeat("c", 40)
	commitD = strings.Repeat("d", 64)
	commitE = strings.Repeat("e", 40)
)

// newFeedAccountRig is newWkRig's daemon with write screening on (so refusals are
// recorded and the feed runs) and commit records offered, and its hook routes.
func newFeedAccountRig(t *testing.T) (*wkRig, *events.Recorder, http.Handler) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mgr, err := files.NewManager(dir, db, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })
	for _, slug := range []string{wkOpen, wkConf, wkSealed} {
		_, err := store.EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
	}
	require.NoError(t, store.SetProjectIsolation(ctx, db, wkConf, core.IsolationConfidential))
	require.NoError(t, store.SetProjectIsolation(ctx, db, wkSealed, core.IsolationSealed))
	require.NoError(t, store.SetSetting(ctx, db, store.SettingRepoProjectMap, `{"/work/open":"`+wkOpen+`"}`))
	require.NoError(t, store.AdoptLocalHost(ctx, db, wkHost))
	policy, err := access.New(config.PeerAuth{Enabled: true, Owner: "owner", Restricted: []string{wkAcct}},
		func(n string) (access.Account, error) {
			switch n {
			case "owner":
				return access.Account{UID: wkOwnerUID, Name: "owner", Home: "/home/owner"}, nil
			case wkAcct:
				return access.Account{UID: wkAcctUID, Name: wkAcct, Home: "/home/" + wkAcct}, nil
			}
			return access.Account{}, fmt.Errorf("no account %s", n)
		})
	require.NoError(t, err)

	rec := events.NewRecorder(db)
	feats := config.Features{Research: true, Screening: true}
	ret := retrieve.New(db, nil, config.Budgets{MaxBriefingTokens: 1500, RecallBudgetTokens: 1000}, nil)
	ret.SetBodyReader(mgr.Store())
	ret.SetFeaturesConfig(feats)
	scr := screening.New(screening.Config{DB: db, Files: mgr, Events: rec, Enabled: func(context.Context) bool { return true }})
	svc := approval.New(approval.Config{DB: db, Events: rec, Host: wkHost, LeaseCap: 8 * time.Hour})
	srv := mcpserver.New(mcpserver.Config{
		DB: db, Files: mgr, Retrieve: ret, Events: rec, APIKey: testKey, LocalHost: wkHost,
		Features: feats, Approvals: svc, Screening: scr, RecordCommits: true,
	})
	hookRoutes := http.NewServeMux()
	hooks.NewHandler(hooks.Config{DB: db, Retrieve: ret, Events: rec, Files: mgr, APIKey: testKey, LocalHost: wkHost,
		Features: feats, Screening: scr}).Register(hookRoutes)
	return &wkRig{t: t, db: db, policy: policy, srv: srv}, rec, hookRoutes
}

// hookAs posts one hook event to the hook routes as view would reach them: the
// restricted account on the socket (no key), the owner with the bearer key.
func hookAs(t *testing.T, routes http.Handler, view access.View, event string, body map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/hooks/"+event, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if view.Account() == "" {
		req.Header.Set("Authorization", "Bearer "+testKey)
	}
	rr := httptest.NewRecorder()
	routes.ServeHTTP(rr, req.WithContext(access.WithView(req.Context(), view)))
	require.Equal(t, http.StatusOK, rr.Code, "hook %s: %s", event, rr.Body.String())
}

// consumer is a minimal feed consumer on a Unix socket: it answers the
// limits and settles every batch, keeping each frame line once.
type consumer struct {
	mu    sync.Mutex
	lines []string
	seen  map[string]bool
}

func startConsumer(t *testing.T, sock string) *consumer {
	t.Helper()
	c := &consumer{seen: map[string]bool{}}
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

func (c *consumer) received() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.lines...)
}

// frame is a received frame as a monitor reads it.
type frame struct {
	Activity string         `json:"activity"`
	Actor    map[string]any `json:"actor"`
	Fields   map[string]any `json:"fields"`
	raw      string
}

// event is one row of the event log.
type event struct {
	id, kind, session string
	payload           map[string]any
}

func loggedEvents(t *testing.T, r *wkRig, afterRow int64) []event {
	t.Helper()
	rows, err := r.db.Query(`SELECT id, kind, session_id, payload FROM events WHERE rowid > ? ORDER BY rowid`, afterRow)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var out []event
	for rows.Next() {
		var e event
		var raw string
		require.NoError(t, rows.Scan(&e.id, &e.kind, &e.session, &raw))
		require.NoError(t, json.Unmarshal([]byte(raw), &e.payload))
		out = append(out, e)
	}
	require.NoError(t, rows.Err())
	return out
}

func TestRestricted_SecurityFeedAndSnapshotNameTheAccount(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, rec, hookRoutes := newFeedAccountRig(t)

	sockDir, err := os.MkdirTemp("", "sf") // a socket path is capped near 104 bytes on macOS
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	c := startConsumer(t, filepath.Join(sockDir, "c.sock"))
	fd := feed.New(feed.Config{DB: r.db, Events: rec, Socket: filepath.Join(sockDir, "c.sock"),
		Spool: filepath.Join(sockDir, "spool"), Host: "build-host", Enabled: func(context.Context) bool { return true }})
	require.NoError(t, fd.Run(ctx))
	t.Cleanup(fd.Wait)
	t.Cleanup(cancel)
	// The first turn takes the cursor at the newest event, then heartbeats.
	require.Eventually(t, func() bool { return len(c.received()) > 0 }, 20*time.Second, 20*time.Millisecond)
	startRow, _, err := rec.NewestRow(ctx)
	require.NoError(t, err)

	// The owner's work.
	owner := r.owner(wkOwnerProc)
	ownerName := r.ok(owner, "session_start", map[string]any{"name": "sess/feed-owner", "project": wkOpen})["name"]
	require.Equal(t, "sess/feed-owner", ownerName)
	r.ok(owner, "memory_write", map[string]any{"name": "owner-gotcha", "kind": "gotcha", "description": "the owner's",
		"body": "Owner text.\n", "project": wkOpen})
	ownerTask := r.ok(owner, "tasks_add", map[string]any{"title": "the owner's task", "project": wkOpen})["id"].(string)
	r.ok(owner, "tasks_update", map[string]any{"id": ownerTask, "commits": []any{commitC}})
	vouched := r.ok(owner, "tasks_add", map[string]any{"title": "the owner's, vouched by the account", "project": wkOpen})["id"].(string)
	r.refused(owner, "memory_write", map[string]any{"name": "x", "kind": "gotcha", "description": "x", "body": "x\n", "project": wkSealed})

	// The account's work, in a session of its own.
	acct := r.acct(wkAcctProc)
	acctName := r.ok(acct, "session_start", map[string]any{"name": "sess/feed-acct", "project": wkOpen})["name"].(string)
	require.Equal(t, wkAcct+":sess/feed-acct", acctName, "stored and reported inside its namespace")
	r.ok(acct, "memory_write", map[string]any{"name": "acct-gotcha", "kind": "gotcha", "description": "the account's",
		"body": "Account text.\n", "project": wkOpen})
	held := r.ok(acct, "memory_write", map[string]any{"name": "acct-rule", "kind": "constraint", "description": "a proposal",
		"body": "Account rule.\n", "project": wkOpen})
	require.Equal(t, "proposed", held["status"], "a restricted account's rule is a proposal")
	acctTask := r.ok(acct, "tasks_add", map[string]any{"title": "the account's task", "project": wkOpen})["id"].(string)
	r.ok(acct, "tasks_update", map[string]any{"id": acctTask, "status": "done", "commits": []any{commitA}})
	r.ok(acct, "tasks_update", map[string]any{"id": vouched, "commits": []any{commitE}})
	r.refused(acct, "memory_write", map[string]any{"name": "x", "kind": "gotcha", "description": "x", "body": "x\n", "project": wkConf})
	r.ok(acct, "session_end", map[string]any{"findings": "done", "commits": []any{commitB}})

	// The account with no session bound: its write and its refused reach.
	bare := r.acct("")
	r.ok(bare, "memory_write", map[string]any{"name": "acct-unbound", "kind": "gotcha", "description": "no session",
		"body": "Unbound.\n", "project": wkOpen})
	r.refused(bare, "memory_read", map[string]any{"name": "anything", "project": wkConf})

	r.ok(owner, "session_end", map[string]any{"findings": "done", "commits": []any{commitD}})

	// A prompt through the hooks outside any session, from each: an event the
	// hooks record with no session, named by the recorder alone.
	acctView, ok := r.policy.PeerView(wkAcctUID)
	require.True(t, ok)
	open, err := store.OpenProjectSlugs(ctx, r.db)
	require.NoError(t, err)
	prompt := func(sessionID string) map[string]any {
		return map[string]any{"session_id": sessionID, "cwd": "/work/open", "user_prompt": "xylophone quartz zeppelin"}
	}
	hookAs(t, hookRoutes, acctView.WithOpenProjects(open), "user-prompt-submit", prompt("acct-hook-1"))
	hookAs(t, hookRoutes, r.policy.OwnerView(), "user-prompt-submit", prompt("owner-hook-1"))

	// Every event reaches the consumer.
	evs := loggedEvents(t, r, startRow)
	frames := map[string]frame{}
	require.Eventually(t, func() bool {
		for _, line := range c.received() {
			var f frame
			if json.Unmarshal([]byte(line), &f) != nil {
				continue
			}
			f.raw = line
			if id, ok := f.Fields["event_id"].(string); ok {
				frames[id] = f
			}
		}
		for _, e := range evs {
			if _, ok := frames[e.id]; !ok {
				return false
			}
		}
		return true
	}, 30*time.Second, 50*time.Millisecond, "every event since the feed started is framed")

	acctSess, ok, err := store.SessionByName(ctx, r.db, acctName)
	require.NoError(t, err)
	require.True(t, ok)
	ownerSess, ok, err := store.SessionByName(ctx, r.db, "sess/feed-owner")
	require.NoError(t, err)
	require.True(t, ok)

	seen := map[string]bool{} // "<who> <activity>[ <reason>]" for the account's frames
	for _, e := range evs {
		f := frames[e.id]
		_, stamped := e.payload[events.PayloadAccount]
		switch {
		case e.session == acctSess.ID:
			require.Equal(t, acctName, f.Actor["session"], "%s: the qualified session name", e.kind)
			require.Equal(t, wkAcct, f.Actor["account"], "%s", e.kind)
			require.Equal(t, "agent", f.Actor["kind"])
			require.True(t, stamped, "%s: the request's account is in the event too", e.kind)
			key := "session " + f.Activity
			if reason, ok := f.Fields["reason"].(string); ok {
				key += " " + reason
			}
			seen[key] = true
		case e.session == ownerSess.ID || (e.session == "" && !stamped):
			require.NotContains(t, f.raw, `"account"`, "%s: the owner's frame carries no account", e.kind)
			require.False(t, stamped, "%s: the owner's event carries no account", e.kind)
		case e.session == "":
			require.Equal(t, map[string]any{"kind": "agent", "account": wkAcct}, f.Actor, "%s: no session, still the account", e.kind)
			key := "unbound " + f.Activity
			if reason, ok := f.Fields["reason"].(string); ok {
				key += " " + reason
			}
			seen[key] = true
		default:
			require.Failf(t, "an event of no session in this test", "%s in %s", e.kind, e.session)
		}
	}
	for _, want := range []string{
		"session session_started", "session memory_written", "session memory_held", "session task_transition",
		"session access_refused peer.unreachable", "session session_ended", "session tool_call",
		"unbound memory_written", "unbound access_refused peer.unreachable",
	} {
		require.True(t, seen[want], "the account's %s frame is named; saw %v", want, seen)
	}
	// The two prompts: the account's names it, the owner's does not.
	hookFrames := map[string]frame{}
	for _, e := range evs {
		if id, _ := e.payload["claude_session_id"].(string); id != "" && e.session == "" {
			hookFrames[id] = frames[e.id]
		}
	}
	require.Len(t, hookFrames, 2, "one session-less hook event each: %v", hookFrames)
	acctHook := hookFrames[wkAcct+":acct-hook-1"]
	require.Contains(t, []string{"hook_prompt", "retrieval_injected"}, acctHook.Activity)
	require.Equal(t, map[string]any{"kind": "agent", "account": wkAcct}, acctHook.Actor, "the account's hook event, no session")
	require.NotContains(t, hookFrames["owner-hook-1"].raw, `"account"`, "the owner's hook event carries none")

	// The snapshot: the account's session by its qualified name, its task and
	// the owner's task it vouched for with the account; the owner's without.
	snap, _, err := commitfeed.Build(ctx, r.db, commitfeed.Options{Host: "build-host", Window: 24 * time.Hour, Now: time.Now()})
	require.NoError(t, err)
	raw, err := json.Marshal(snap)
	require.NoError(t, err)
	require.NoError(t, commitfeed.CheckJSON(raw, time.Now()), "%s", raw)
	sessions := map[string]commitfeed.Session{}
	for _, s := range snap.Sessions {
		sessions[s.ID] = s
	}
	require.Equal(t, wkAcct, sessions[acctName].Account)
	require.Equal(t, []string{commitB}, sessions[acctName].Commits)
	require.Contains(t, sessions, "sess/feed-owner")
	require.Empty(t, sessions["sess/feed-owner"].Account)
	require.Equal(t, []string{commitD}, sessions["sess/feed-owner"].Commits)
	tasks := map[string]commitfeed.Task{}
	for _, x := range snap.Tasks {
		tasks[x.ID] = x
	}
	repo := "open" // the repository /work/open names
	require.Equal(t, commitfeed.Task{ID: acctTask, Account: wkAcct, Repo: &repo, Commits: []string{commitA}}, tasks[acctTask])
	require.Equal(t, commitfeed.Task{ID: vouched, Account: wkAcct, Repo: &repo, Commits: []string{commitE}}, tasks[vouched],
		"the owner's task names the account that recorded its commit")
	require.Equal(t, commitfeed.Task{ID: ownerTask, Repo: &repo, Commits: []string{commitC}}, tasks[ownerTask])
	t.Logf("snapshot: %s", raw)
}
