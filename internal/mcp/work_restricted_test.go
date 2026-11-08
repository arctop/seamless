package mcp_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
	mcpserver "github.com/arctop/seamless/internal/mcp"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// Restricted local accounts (peer_auth.restricted) at the tool surface's work
// half: scope funnels, sessions, tasks, trials, projects. A restricted account
// reaches the open, unlocked projects (read and write) and the global scope
// (read only); everything else -- confidential, sealed, locked, a slug nobody
// registered -- is refused in one text and created by nothing. A session
// belongs to the principal that started it, in both directions. Its writes
// carry its account; the owner's carry nothing new.

const (
	wkOpen   = "example-open"
	wkOther  = "example-other"
	wkConf   = "example-conf"
	wkSealed = "example-sealed"
	wkLocked = "example-locked"
	wkGhost  = "example-ghost"
	wkHost   = "laptop"
	wkAcct   = "_example_agent"

	wkOwnerUID = 501
	wkAcctUID  = 502

	// Agent-process identities (agentproc's pid.version shape).
	wkOwnerProc = "200.2000"
	wkAcctProc  = "300.3000"
)

// wkRig is one daemon's MCP server under a peer_auth policy with one locked
// project and one restricted account, and transports that attach the View
// cmd/seamlessd's handlers would build.
type wkRig struct {
	t      *testing.T
	db     *sql.DB
	policy *access.Policy
	srv    *mcpserver.Server
}

func newWkRig(t *testing.T) *wkRig {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mgr, err := files.NewManager(dir, db, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })
	for _, slug := range []string{wkOpen, wkOther, wkConf, wkSealed, wkLocked} {
		_, err := store.EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
	}
	require.NoError(t, store.SetProjectIsolation(ctx, db, wkConf, core.IsolationConfidential))
	require.NoError(t, store.SetProjectIsolation(ctx, db, wkSealed, core.IsolationSealed))
	require.NoError(t, store.SetSetting(ctx, db, store.SettingRepoProjectMap,
		`{"/work/open":"`+wkOpen+`","/work/conf":"`+wkConf+`","/work/locked":"`+wkLocked+`"}`))
	require.NoError(t, store.AdoptLocalHost(ctx, db, wkHost))

	policy, err := access.New(config.PeerAuth{
		Enabled: true, Owner: "owner", Locked: []string{wkLocked}, Restricted: []string{wkAcct},
	}, func(n string) (access.Account, error) {
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
	ret := retrieve.New(db, nil, config.Budgets{MaxBriefingTokens: 1500, RecallBudgetTokens: 1000}, nil)
	ret.SetBodyReader(mgr.Store())
	svc := approval.New(approval.Config{DB: db, Events: rec, Host: wkHost, LeaseCap: 8 * time.Hour})
	srv := mcpserver.New(mcpserver.Config{
		DB: db, Files: mgr, Retrieve: ret, Events: rec, APIKey: testKey, LocalHost: wkHost,
		Features: config.Features{Research: true}, Approvals: svc, OfferAccessRequest: true,
	})
	return &wkRig{t: t, db: db, policy: policy, srv: srv}
}

// serve exposes the server with view() attached to every request; view nil
// attaches none (peer_auth off).
func (r *wkRig) serve(view func() access.View) string {
	h := r.srv.Handler()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if view != nil {
			req = req.WithContext(access.WithView(req.Context(), view()))
		}
		h.ServeHTTP(w, req)
	}))
	r.t.Cleanup(ts.Close)
	return ts.URL
}

// restricted is the restricted account's socket transport: its reach is the
// projects open at each request, as peerHandler loads them.
func (r *wkRig) restricted() string {
	return r.serve(func() access.View {
		v, ok := r.policy.PeerView(wkAcctUID)
		require.True(r.t, ok)
		open, err := store.OpenProjectSlugs(context.Background(), r.db)
		require.NoError(r.t, err)
		return v.WithOpenProjects(open)
	})
}

// ownerTCP is the owner's TCP transport (the bearer key).
func (r *wkRig) ownerTCP() string {
	return r.serve(func() access.View { return r.policy.OwnerView() })
}

// off is the same server with peer_auth off: no View at all.
func (r *wkRig) off() string { return r.serve(nil) }

// dial connects to base. key sends the bearer key (the restricted account
// never holds it: the socket alone authenticates it); proc names an agent
// process when non-empty.
func (r *wkRig) dial(base string, key bool, proc string) *mcpclient.Client {
	r.t.Helper()
	headers := map[string]string{mcpserver.HostHeader: wkHost}
	if key {
		headers["Authorization"] = "Bearer " + testKey
	}
	if proc != "" {
		headers[mcpserver.AgentProcessHeader] = proc
	}
	cli, err := mcpclient.NewStreamableHttpClient(base, transport.WithHTTPHeaders(headers))
	require.NoError(r.t, err)
	r.t.Cleanup(func() { _ = cli.Close() })
	require.NoError(r.t, cli.Start(context.Background()))
	var initReq mcp.InitializeRequest
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "restricted-work", Version: "0"}
	_, err = cli.Initialize(context.Background(), initReq)
	require.NoError(r.t, err)
	return cli
}

func (r *wkRig) acct(proc string) *mcpclient.Client  { return r.dial(r.restricted(), false, proc) }
func (r *wkRig) owner(proc string) *mcpclient.Client { return r.dial(r.ownerTCP(), true, proc) }

// call returns a tool result's first text item and whether it is an error.
func (r *wkRig) call(cli *mcpclient.Client, name string, args map[string]any) (string, bool) {
	r.t.Helper()
	res, err := cli.CallTool(context.Background(), mcp.CallToolRequest{Params: mcp.CallToolParams{Name: name, Arguments: args}})
	require.NoError(r.t, err)
	require.NotEmpty(r.t, res.Content)
	tc, ok := res.Content[0].(mcp.TextContent)
	require.True(r.t, ok)
	return tc.Text, res.IsError
}

// ok requires a success and decodes its JSON.
func (r *wkRig) ok(cli *mcpclient.Client, name string, args map[string]any) map[string]any {
	r.t.Helper()
	text, isErr := r.call(cli, name, args)
	require.False(r.t, isErr, "%s: %s", name, text)
	var out map[string]any
	require.NoError(r.t, json.Unmarshal([]byte(text), &out), text)
	return out
}

// refused requires an error and returns its text.
func (r *wkRig) refused(cli *mcpclient.Client, name string, args map[string]any) string {
	r.t.Helper()
	text, isErr := r.call(cli, name, args)
	require.True(r.t, isErr, "%s: %s", name, text)
	return text
}

// count runs a COUNT(*) query.
func (r *wkRig) count(query string, args ...any) int {
	r.t.Helper()
	var n int
	require.NoError(r.t, r.db.QueryRowContext(context.Background(), query, args...).Scan(&n))
	return n
}

// ambient records an agent's ambient session the way the SessionStart hook
// does: account names the principal whose hook made it ("" the owner).
func (r *wkRig) ambient(name, project, account, proc string, status core.SessionStatus, updated time.Time) core.Session {
	r.t.Helper()
	ctx := context.Background()
	id, err := core.NewID()
	require.NoError(r.t, err)
	sess := core.Session{
		ID: id, Name: name, ProjectSlug: project, Status: status, Ambient: true, Host: wkHost,
		ExternalSessionID: name + "-x", ExternalClient: "claude-code", CWD: "/work/" + project,
		Account: account, CreatedAt: updated, UpdatedAt: updated,
	}
	require.NoError(r.t, store.CreateSession(ctx, r.db, sess))
	if proc != "" {
		require.NoError(r.t, store.SetSessionAgentProcess(ctx, r.db, id, proc))
	}
	// As stored: a restricted account's name inside its namespace.
	stored, ok, err := store.SessionByID(ctx, r.db, id)
	require.NoError(r.t, err)
	require.True(r.t, ok)
	return stored
}

// wkQual is a restricted account's session name as it is stored and as the
// owner names it: inside the account's namespace (SEAL A2).
func wkQual(name string) string { return access.QualifiedSessionName(wkAcct, name) }

func (r *wkRig) session(name string) core.Session {
	r.t.Helper()
	sess, ok, err := store.SessionByName(context.Background(), r.db, name)
	require.NoError(r.t, err)
	require.True(r.t, ok, "session %s", name)
	return sess
}

// wkOneText requires every refusal in texts to read the same once its own
// project slug is masked: the restricted account cannot tell a closed project
// from a missing one.
func wkOneText(t *testing.T, texts map[string]string) string {
	t.Helper()
	var first string
	for slug, text := range texts {
		masked := strings.ReplaceAll(text, slug, "<P>")
		if first == "" {
			first = masked
			continue
		}
		require.Equal(t, first, masked, "the refusal for %s differs", slug)
	}
	return first
}

// wkClosed are the projects a restricted account may not reach, plus a
// slug nobody registered.
var wkClosed = []string{wkConf, wkSealed, wkLocked, wkGhost}

func TestRestricted_ReachMatrix(t *testing.T) {
	r := newWkRig(t)
	ctx := context.Background()
	owner := r.owner("")
	// The fixture: a note of one slug in the open project and in the global
	// scope, for notes_read by slug.
	openNote := r.ok(owner, "notes_create", map[string]any{"project": wkOpen, "title": "seed note", "body": "open body"})
	r.ok(owner, "notes_create", map[string]any{"project": "global", "title": "seed note", "body": "global body"})
	slug, _ := openNote["slug"].(string)
	require.NotEmpty(t, slug)

	acct := r.acct("")
	reads := map[string]func(p string) map[string]any{
		"tasks_list":  func(p string) map[string]any { return map[string]any{"project": p} },
		"tasks_ready": func(p string) map[string]any { return map[string]any{"project": p} },
		"recall":      func(p string) map[string]any { return map[string]any{"project": p, "query": "seed note"} },
		"notes_read":  func(p string) map[string]any { return map[string]any{"project": p, "slug": slug} },
	}
	for tool, args := range reads {
		t.Run("read/"+tool, func(t *testing.T) {
			texts := map[string]string{}
			for _, p := range wkClosed {
				texts[p] = r.refused(acct, tool, args(p))
			}
			masked := wkOneText(t, texts)
			require.Equal(t, tool+": project <P> is not reachable from account "+wkAcct+": a restricted account "+
				"reaches open projects that are not locked, and reads the global scope; it cannot create a project "+
				"either -- the owner creates and maps them", masked)
			r.ok(acct, tool, args(wkOpen))
			r.ok(acct, tool, args("global"))
		})
	}

	writes := map[string]func(p string) map[string]any{
		"tasks_add":    func(p string) map[string]any { return map[string]any{"project": p, "title": "probe"} },
		"notes_create": func(p string) map[string]any { return map[string]any{"project": p, "title": "probe", "body": "b"} },
		"trial_record": func(p string) map[string]any {
			return map[string]any{"project": p, "title": "probe", "lab": "lab-probe"}
		},
		"session_start": func(p string) map[string]any { return map[string]any{"project": p} },
	}
	for tool, args := range writes {
		t.Run("write/"+tool, func(t *testing.T) {
			texts := map[string]string{}
			for _, p := range wkClosed {
				texts[p] = r.refused(acct, tool, args(p))
			}
			masked := wkOneText(t, texts)
			require.True(t, strings.HasPrefix(masked, tool+": project <P> is not reachable from account "+wkAcct+": "), masked)
			global := r.refused(acct, tool, args("global"))
			require.Equal(t, tool+": the global scope is read-only for account "+wkAcct+": a restricted account reads "+
				"it and writes only into open projects -- name one with project=<slug>", global)
			out := r.ok(acct, tool, args(wkOpen))
			if tool != "notes_create" {
				// notes_create's result is the other agent's (A1) surface.
				require.Equal(t, wkAcct, out["account"], "%s: %v", tool, out)
			}
		})
	}

	// Nothing a refused write named was created: no project, no session, no
	// task, no trial, no note, no access request.
	_, exists, err := store.ProjectBySlug(ctx, r.db, wkGhost)
	require.NoError(t, err)
	require.False(t, exists, "a refused write must not register the slug it named")
	for _, p := range wkClosed {
		require.Zero(t, r.count(`SELECT COUNT(*) FROM tasks WHERE project_slug = ?`, p), p)
		require.Zero(t, r.count(`SELECT COUNT(*) FROM trials WHERE project_slug = ?`, p), p)
		require.Zero(t, r.count(`SELECT COUNT(*) FROM sessions WHERE project_slug = ?`, p), p)
		require.Zero(t, r.count(`SELECT COUNT(*) FROM notes_index WHERE project = ?`, p), p)
	}
	require.Zero(t, r.count(`SELECT COUNT(*) FROM sessions WHERE project_slug = ''`), "no global session either")
	require.Equal(t, 1, r.count(`SELECT COUNT(*) FROM sessions WHERE account = ?`, wkAcct),
		"only the session_start into the open project made a session")
	pending, err := store.PendingAccessApprovals(ctx, r.db, time.Now())
	require.NoError(t, err)
	require.Empty(t, pending, "a restricted account files no access request, at a lock or anywhere")
}

func TestRestricted_ProjectListAndCreate(t *testing.T) {
	r := newWkRig(t)
	ctx := context.Background()
	acct := r.acct("")

	list := r.ok(acct, "project_list", nil)
	require.ElementsMatch(t, []string{wkOpen, wkOther}, projectSlugs(list),
		"confidential, sealed and locked projects are not listed for a restricted account")
	for _, p := range list["projects"].([]any) {
		require.NotContains(t, p.(map[string]any), "locked")
	}
	// The owner's list is as before: everything, the locked project marked.
	ownerList := r.ok(r.owner(""), "project_list", nil)
	require.ElementsMatch(t, []string{wkOpen, wkOther, wkConf, wkSealed, wkLocked}, projectSlugs(ownerList))

	texts := map[string]string{}
	for _, p := range wkClosed {
		texts[p] = r.refused(acct, "project_create", map[string]any{"name": "Probe " + p, "slug": p})
	}
	masked := wkOneText(t, texts)
	require.True(t, strings.HasPrefix(masked, "project_create: project <P> is not reachable from account "+wkAcct+": "), masked)
	require.Contains(t, masked, "it cannot create a project either -- the owner creates and maps them")
	_, exists, err := store.ProjectBySlug(ctx, r.db, wkGhost)
	require.NoError(t, err)
	require.False(t, exists)

	// An open project it reaches exists already: that is the only answer.
	got := r.refused(acct, "project_create", map[string]any{"name": "Open", "slug": wkOpen})
	require.Equal(t, `project_create: project slug "`+wkOpen+`" already exists`, got)
	n := r.count(`SELECT COUNT(*) FROM projects`)
	require.Equal(t, 5, n, "a restricted account created no project")
}

func TestRestricted_SessionStartPlacement(t *testing.T) {
	r := newWkRig(t)
	ctx := context.Background()
	acct := r.acct("")

	// Unmapped, mapped to a confidential project, mapped to a locked one: one
	// text, and none of them names the project a mapped cwd belongs to.
	texts := map[string]string{}
	for _, cwd := range []string{"/work/nowhere", "/work/conf", "/work/locked"} {
		text := r.refused(acct, "session_start", map[string]any{"cwd": cwd})
		require.NotContains(t, text, wkConf)
		require.NotContains(t, text, wkLocked)
		texts[cwd] = text
	}
	masked := wkOneText(t, texts)
	require.Equal(t, "session_start: this cwd is not in a repository the owner has mapped to a project account "+
		wkAcct+" reaches, and the global scope is read-only for account "+wkAcct+": a restricted account works only "+
		"in open projects that already exist -- name one with project=<slug> (project_list lists the ones it reaches), "+
		"or ask the owner to map this repository to one (`seamlessd map-repo`)", masked)
	none := r.refused(acct, "session_start", nil)
	require.Equal(t, "session_start: no cwd or project was given, and the global scope is read-only for account "+
		wkAcct+": a restricted account works only in open projects that already exist -- name one with "+
		"project=<slug> (project_list lists the ones it reaches)", none)

	// A repository on another machine named with its roots registers a
	// project for the owner; for a restricted account it registers nothing and
	// the repo map does not grow.
	remote := map[string]any{"host": "otherbox", "cwd": "/src/newrepo", "repo_root": "/src/newrepo"}
	r.refused(acct, "session_start", remote)
	_, exists, err := store.ProjectBySlug(ctx, r.db, "newrepo")
	require.NoError(t, err)
	require.False(t, exists)
	mapped, err := store.ResolveProjectForCWD(ctx, r.db, "otherbox", "/src/newrepo")
	require.NoError(t, err)
	require.Empty(t, mapped, "the repo map did not grow")
	require.Zero(t, r.count(`SELECT COUNT(*) FROM sessions`), "no refused session_start made a session")
	ownerOut := r.ok(r.owner(""), "session_start", remote)
	require.Equal(t, "newrepo", ownerOut["project"], "the same call registers the project for the owner")

	// A cwd mapped to an open project places it there, labeled.
	out := r.ok(acct, "session_start", map[string]any{"cwd": "/work/open", "name": "sess/acct-one"})
	require.Equal(t, wkOpen, out["project"])
	require.Equal(t, wkAcct, out["account"])
	require.Equal(t, wkQual("sess/acct-one"), out["name"], "named inside its namespace")
	require.Equal(t, wkAcct, r.session(wkQual("sess/acct-one")).Account)

	// A project argument it reaches places it whatever a closed project the
	// cwd maps to: no conflict error that would name that project. The owner
	// still gets the conflict.
	out = r.ok(acct, "session_start", map[string]any{"cwd": "/work/conf", "project": wkOpen})
	require.Equal(t, wkOpen, out["project"])
	conflict := r.refused(r.owner(""), "session_start", map[string]any{"cwd": "/work/conf", "project": wkOpen})
	require.Contains(t, conflict, `which is in project "`+wkConf+`"`)

	// Resuming its own session by name needs no cwd: the session places it.
	r.ok(acct, "session_end", map[string]any{"session": "sess/acct-one", "findings": "done"})
	out = r.ok(acct, "session_start", map[string]any{"name": "sess/acct-one"})
	require.Equal(t, true, out["resumed"])
	require.Equal(t, wkOpen, out["project"])
	require.Equal(t, wkAcct, out["account"])
}

func TestRestricted_SessionBelongsToItsPrincipal(t *testing.T) {
	r := newWkRig(t)
	ctx := context.Background()
	owner := r.owner("")
	acct := r.acct("")
	r.ok(owner, "session_start", map[string]any{"project": wkOpen, "name": "sess/owner-one"})
	ownerSess := r.session("sess/owner-one")
	out := r.ok(acct, "session_start", map[string]any{"project": wkOpen, "name": "sess/acct-one"})
	require.Equal(t, wkQual("sess/acct-one"), out["name"], "its session is named inside its namespace")
	acctSess := r.session(wkQual("sess/acct-one"))
	task := r.ok(owner, "tasks_add", map[string]any{"project": wkOpen, "title": "shared work"})
	taskID := task["id"].(string)

	refusal := func(name string) string {
		return "session " + name + " is not reachable from this connection: a session belongs to the account that " +
			"started it -- start your own with session_start"
	}

	t.Run("restricted to owner", func(t *testing.T) {
		// By name, the owner's session is not the account's to find: every
		// call naming it answers exactly as for a name nobody holds (SEAL A2).
		for _, c := range []struct {
			tool string
			args func(name string) map[string]any
		}{
			{"session_update", func(n string) map[string]any { return map[string]any{"session": n, "findings": "injected"} }},
			{"session_end", func(n string) map[string]any { return map[string]any{"session": n, "findings": "ended by another"} }},
			{"session_start", func(n string) map[string]any { return map[string]any{"name": n} }},
			{"tasks_claim", func(n string) map[string]any { return map[string]any{"id": taskID, "session": n} }},
			// The session= handle on any other tool: refused before it runs.
			{"tasks_add", func(n string) map[string]any {
				return map[string]any{"project": wkOpen, "title": "as the owner", "session": n}
			}},
			{"tasks_list", func(n string) map[string]any { return map[string]any{"project": wkOpen, "session": n} }},
			{"trial_record", func(n string) map[string]any {
				return map[string]any{"project": wkOpen, "title": "t", "lab": "lab-x", "session": n}
			}},
		} {
			taken := r.refused(acct, c.tool, c.args("sess/owner-one"))
			free := r.refused(acct, c.tool, c.args("sess/never-was"))
			require.Equal(t, strings.ReplaceAll(free, "sess/never-was", "sess/owner-one"), taken,
				"%s: a name the owner holds reads like a free one", c.tool)
			require.NotContains(t, taken, "not reachable", c.tool)
		}
		// By its ULID the session is the owner's, and refused as such: an id
		// names one session instance-wide.
		require.Equal(t, "session_update: "+refusal(ownerSess.ID), r.refused(acct, "session_update",
			map[string]any{"session_id": ownerSess.ID, "findings": "injected"}))

		after := r.session("sess/owner-one")
		require.Equal(t, core.SessionActive, after.Status)
		require.Empty(t, after.Findings)
		got, err := store.TaskByID(ctx, r.db, taskID)
		require.NoError(t, err)
		require.Equal(t, core.TaskOpen, got.Status)
		require.Empty(t, got.ClaimedBy)
		require.Zero(t, r.count(`SELECT COUNT(*) FROM tasks WHERE title = 'as the owner'`))

		// The same name, where the account may write, is a session of its own.
		before := r.session("sess/owner-one")
		mine := r.ok(acct, "session_start", map[string]any{"project": wkOpen, "name": "sess/owner-one"})
		require.Equal(t, wkQual("sess/owner-one"), mine["name"])
		require.NotEqual(t, ownerSess.ID, mine["session_id"])
		require.Nil(t, mine["resumed"], "a fresh session, never the owner's resumed")
		require.Equal(t, before, r.session("sess/owner-one"), "the owner's is untouched")
		r.ok(acct, "session_end", map[string]any{"session": "sess/owner-one", "findings": "its own"})
		require.Equal(t, "its own", r.session(wkQual("sess/owner-one")).Findings)
		require.Empty(t, r.session("sess/owner-one").Findings)
	})

	t.Run("restricted in its own", func(t *testing.T) {
		claim := r.ok(acct, "tasks_claim", map[string]any{"id": taskID, "session": "sess/acct-one"})
		require.Equal(t, acctSess.ID, claim["claimed_by"])
		require.Equal(t, wkAcct, claim["updated_account"])
		require.NotContains(t, claim, "account", "the owner created the task")
		// By the name it gave, or the qualified name its session reports.
		upd := r.ok(acct, "session_update", map[string]any{"session": "sess/acct-one", "findings": "progress"})
		require.Equal(t, wkAcct, upd["account"])
		upd = r.ok(acct, "session_update", map[string]any{"session": wkQual("sess/acct-one"), "findings": "progress"})
		require.Equal(t, acctSess.ID, upd["session_id"])
	})

	t.Run("owner to restricted", func(t *testing.T) {
		// The owner names the account's session by its qualified name, and is
		// refused it.
		qual := wkQual("sess/acct-one")
		require.Equal(t, "session_update: "+refusal(qual), r.refused(owner, "session_update",
			map[string]any{"session": qual, "findings": "overwritten"}))
		require.Equal(t, "session_end: "+refusal(acctSess.ID), r.refused(owner, "session_end",
			map[string]any{"session_id": acctSess.ID, "findings": "ended by the owner"}))
		require.Equal(t, "session_start: "+refusal(qual), r.refused(owner, "session_start",
			map[string]any{"name": qual}))
		require.Equal(t, "tasks_release: "+refusal(qual), r.refused(owner, "tasks_release",
			map[string]any{"id": taskID, "session": qual}))
		require.Equal(t, "recall: "+refusal(qual), r.refused(owner, "recall",
			map[string]any{"query": "anything", "session": qual}))
		// The bare name is the owner's own namespace, where nothing holds it.
		require.Contains(t, r.refused(owner, "session_update", map[string]any{"session": "sess/acct-one", "findings": "x"}),
			`session "sess/acct-one" not found`)
		// Nor may the owner start a session inside the account's namespace,
		// where it would collide with the account's own name.
		require.Equal(t, "session_start: session name "+wkQual("sess/squat")+" is in restricted account "+wkAcct+
			"'s namespace: names beginning "+wkAcct+": are its sessions -- choose another name",
			r.refused(owner, "session_start", map[string]any{"project": wkOpen, "name": wkQual("sess/squat")}))
		require.Zero(t, r.count(`SELECT COUNT(*) FROM sessions WHERE name = ?`, wkQual("sess/squat")))
		// Nor with peer_auth off: the session is the account's whoever asks.
		require.Equal(t, "session_update: "+refusal(qual), r.refused(r.dial(r.off(), true, ""),
			"session_update", map[string]any{"session": qual, "findings": "overwritten"}))

		after := r.session(qual)
		require.Equal(t, core.SessionActive, after.Status)
		require.Equal(t, "progress", after.Findings)
		got, err := store.TaskByID(ctx, r.db, taskID)
		require.NoError(t, err)
		require.Equal(t, acctSess.ID, got.ClaimedBy, "the claim is still the account's")
	})

	t.Run("each ends its own", func(t *testing.T) {
		end := r.ok(acct, "session_end", map[string]any{"session": "sess/acct-one", "findings": "done"})
		require.Equal(t, wkAcct, end["account"])
		require.Equal(t, float64(1), end["claims_released"])
		ownerEnd := r.ok(owner, "session_end", map[string]any{"session": "sess/owner-one", "findings": "done"})
		require.NotContains(t, ownerEnd, "account")
	})
}

func TestRestricted_ImplicitBindingsNeverCross(t *testing.T) {
	now := time.Now().UTC()

	t.Run("restricted is not bound or inferred into the owner's session", func(t *testing.T) {
		r := newWkRig(t)
		ownerAmb := r.ambient("cc/owner-amb", wkOpen, "", wkOwnerProc, core.SessionActive, now)
		touched := r.session(ownerAmb.Name).UpdatedAt
		// A forged agent-process header naming the owner's agent binds nothing,
		// and the owner's live ambient is no candidate for inference: the
		// account's unplaced write is refused with its own help.
		for _, proc := range []string{wkOwnerProc, ""} {
			text := r.refused(r.acct(proc), "tasks_add", map[string]any{"title": "unplaced"})
			require.True(t, strings.HasPrefix(text, "tasks_add: ambiguous scope: no bound or ambient session to infer "+
				"the project from;"), text)
			require.Contains(t, text, "this connection is restricted account "+wkAcct+": it writes only into open "+
				"projects that already exist -- an unknown slug is refused, never created")
			require.NotContains(t, text, "CREATES that project")
			require.NotContains(t, text, wkConf, "the known-projects list is the account's reach")
			require.Contains(t, r.refused(r.acct(proc), "session_update", map[string]any{"findings": "x"}),
				"no active session")
			// Reads land in the global scope, which it reads.
			r.ok(r.acct(proc), "recall", map[string]any{"query": "anything"})
		}
		require.Zero(t, r.count(`SELECT COUNT(*) FROM tasks`))
		require.Empty(t, r.session(ownerAmb.Name).Findings)
		require.Equal(t, touched, r.session(ownerAmb.Name).UpdatedAt, "no call heartbeat the owner's session")
	})

	t.Run("restricted binds and infers only its own", func(t *testing.T) {
		r := newWkRig(t)
		acctAmb := r.ambient("cc/acct-amb", wkOther, wkAcct, wkAcctProc, core.SessionActive, now.Add(-time.Minute))
		// The owner's sessions are newer, in the account's project and in
		// another: neither makes the account's inference ambiguous or takes it.
		r.ambient("cc/owner-amb", wkOpen, "", wkOwnerProc, core.SessionActive, now)
		r.ambient("cc/owner-amb2", wkOther, "", "", core.SessionActive, now)
		for _, proc := range []string{wkAcctProc, ""} {
			out := r.ok(r.acct(proc), "tasks_add", map[string]any{"title": "mine " + proc})
			require.Equal(t, wkOther, out["project"])
			require.Equal(t, acctAmb.Name, out["created_by"])
			require.Equal(t, wkAcct, out["account"])
		}
		upd := r.ok(r.acct(""), "session_update", map[string]any{"findings": "ambient progress"})
		require.Equal(t, acctAmb.ID, upd["session_id"])
		require.Equal(t, wkAcct, upd["account"])
	})

	t.Run("owner is not bound or inferred into the account's session", func(t *testing.T) {
		r := newWkRig(t)
		acctAmb := r.ambient("cc/acct-amb", wkOther, wkAcct, wkAcctProc, core.SessionActive, now)
		touched := r.session(acctAmb.Name).UpdatedAt
		clients := map[string]*mcpclient.Client{
			"tcp":           r.owner(""),
			"tcp, forged":   r.owner(wkAcctProc),
			"peer_auth off": r.dial(r.off(), true, ""),
		}
		for label, cli := range clients {
			text := r.refused(cli, "tasks_add", map[string]any{"title": "owner unplaced"})
			require.Contains(t, text, "ambiguous scope: no bound or ambient session", label)
			require.Contains(t, text, "an unknown slug CREATES that project", "%s: the owner's help is as before", label)
			require.Contains(t, r.refused(cli, "session_end", map[string]any{"findings": "x"}), "no active session", label)
		}
		require.Zero(t, r.count(`SELECT COUNT(*) FROM tasks`))
		after := r.session(acctAmb.Name)
		require.Equal(t, core.SessionActive, after.Status)
		require.Empty(t, after.Findings)
		require.Equal(t, touched, after.UpdatedAt, "no call heartbeat the account's session")
	})

	t.Run("an explicit binding is its principal's", func(t *testing.T) {
		r := newWkRig(t)
		owner := r.owner("")
		r.ok(owner, "session_start", map[string]any{"project": wkOpen, "name": "sess/owner-bound"})
		mcpID := owner.GetSessionId()
		require.NotEmpty(t, mcpID)
		// The transport accepts any MCP session id it is shown, and the
		// session_start binding is keyed by that id alone: a restricted account
		// presenting the owner's id is still not bound by the owner's binding.
		withID := func(base string, key bool) *mcpclient.Client {
			headers := map[string]string{mcpserver.HostHeader: wkHost}
			if key {
				headers["Authorization"] = "Bearer " + testKey
			}
			tr, err := transport.NewStreamableHTTP(base, transport.WithHTTPHeaders(headers), transport.WithSession(mcpID))
			require.NoError(t, err)
			cli := mcpclient.NewClient(tr, mcpclient.WithSession())
			t.Cleanup(func() { _ = cli.Close() })
			require.NoError(t, cli.Start(context.Background()))
			return cli
		}
		text := r.refused(withID(r.restricted(), false), "tasks_add", map[string]any{"title": "via the owner's binding"})
		require.Contains(t, text, "ambiguous scope")
		require.Zero(t, r.count(`SELECT COUNT(*) FROM tasks`))
		// The id is what binds: the owner presenting it is bound as before.
		out := r.ok(withID(r.ownerTCP(), true), "tasks_add", map[string]any{"title": "the owner's own"})
		require.Equal(t, wkOpen, out["project"])
		require.Equal(t, "sess/owner-bound", out["created_by"])
	})

	t.Run("a restricted call revives no expired session", func(t *testing.T) {
		r := newWkRig(t)
		expired := r.ambient("cc/owner-gone", wkOpen, "", wkOwnerProc, core.SessionExpired, now.Add(-2*time.Hour))
		r.ok(r.acct(wkOwnerProc), "tasks_list", map[string]any{"project": wkOpen})
		require.Equal(t, core.SessionExpired, r.session(expired.Name).Status, "the forged header reopened nothing")
		// The owner's own agent still revives it, as before.
		r.ok(r.owner(wkOwnerProc), "tasks_list", map[string]any{"project": wkOpen})
		require.Equal(t, core.SessionActive, r.session(expired.Name).Status)
	})

	t.Run("a sole same-cwd ambient of the owner's is not adopted", func(t *testing.T) {
		r := newWkRig(t)
		ownerAmb := r.ambient("cc/owner-amb", wkOpen, "", "", core.SessionActive, now)
		out := r.ok(r.acct(""), "session_start", map[string]any{"cwd": "/work/" + wkOpen, "project": wkOpen})
		require.NotEqual(t, ownerAmb.ID, out["session_id"])
		require.NotContains(t, out, "resumed")
		require.Equal(t, wkAcct, out["account"])
		s, ok, err := store.SessionByID(context.Background(), r.db, out["session_id"].(string))
		require.NoError(t, err)
		require.True(t, ok)
		require.Empty(t, s.ExternalSessionID, "nor linked to the owner's client session")
	})
}

func TestRestricted_NoLockOrRequestPath(t *testing.T) {
	r := newWkRig(t)
	ctx := context.Background()
	// The account's own agent recorded a session in the locked project (a
	// hook ran there): it binds nothing, and no call is refused "at the lock"
	// or files a request for it.
	r.ambient("cc/acct-locked", wkLocked, wkAcct, wkAcctProc, core.SessionActive, time.Now().UTC())
	acct := r.acct(wkAcctProc)

	texts := map[string]string{}
	for _, p := range []string{wkLocked, wkConf, wkGhost} {
		texts[p] = r.refused(acct, "access_request", map[string]any{"project": p, "reason": "needed"})
	}
	require.Equal(t, "access_request: account "+wkAcct+" files no access requests: a restricted account reaches the "+
		"open projects that are not locked, and reads the global scope; the owner's approval never opens anything "+
		"else to it, so there is nothing to request -- keep to the projects project_list shows", wkOneText(t, texts))

	write := r.refused(acct, "tasks_add", map[string]any{"title": "into the lock"})
	require.NotContains(t, write, "is locked")
	require.Contains(t, write, "ambiguous scope")
	named := r.refused(acct, "tasks_add", map[string]any{"title": "into the lock", "project": wkLocked})
	require.NotContains(t, named, "is locked")
	recall := r.ok(acct, "recall", map[string]any{"query": "anything"})
	require.NotContains(t, recall, "locked", "recall names no locked project for a restricted account")
	list := r.ok(acct, "project_list", nil)
	require.NotContains(t, projectSlugs(list), wkLocked)

	pending, err := store.PendingAccessApprovals(ctx, r.db, time.Now())
	require.NoError(t, err)
	require.Empty(t, pending)
	require.Zero(t, r.count(`SELECT COUNT(*) FROM access_approvals`))
}

func TestRestricted_ProvenanceInResults(t *testing.T) {
	r := newWkRig(t)
	ctx := context.Background()
	acct := r.acct("")
	owner := r.owner("")
	off := r.dial(r.off(), true, "")

	// Tasks: the creator, then the last changer.
	created := r.ok(acct, "tasks_add", map[string]any{"project": wkOther, "title": "account task"})
	require.Equal(t, wkAcct, created["account"])
	require.NotContains(t, created, "updated_account", "nothing changed it yet")
	id := created["id"].(string)
	changed := r.ok(acct, "tasks_update", map[string]any{"id": id, "title": "account task, edited"})
	require.Equal(t, wkAcct, changed["account"])
	require.Equal(t, wkAcct, changed["updated_account"])
	ownerChanged := r.ok(owner, "tasks_update", map[string]any{"id": id, "title": "account task, edited by the owner"})
	require.Equal(t, wkAcct, ownerChanged["account"], "the creator stays")
	require.NotContains(t, ownerChanged, "updated_account", "the owner's change carries no label")
	listed := r.ok(owner, "tasks_list", map[string]any{"id": id})
	require.Equal(t, wkAcct, listed["tasks"].([]any)[0].(map[string]any)["account"])

	// The owner's own results are exactly the old shape, on TCP and with
	// peer_auth off.
	for label, cli := range map[string]*mcpclient.Client{"tcp": owner, "off": off} {
		task := r.ok(cli, "tasks_add", map[string]any{"project": wkOpen, "title": "owner task " + label})
		require.ElementsMatch(t, []string{"id", "title", "status", "project", "created_by"}, wkKeys(task), label)
		tr := r.ok(cli, "trial_record", map[string]any{"project": wkOpen, "title": "owner trial", "lab": "lab-owner"})
		require.ElementsMatch(t, []string{"id", "lab", "title"}, wkKeys(tr), label)
		name := "sess/owner-" + strings.ReplaceAll(label, " ", "-")
		start := r.ok(cli, "session_start", map[string]any{"project": wkOpen, "name": name})
		require.NotContains(t, start, "account", label)
		upd := r.ok(cli, "session_update", map[string]any{"session": name, "findings": "f"})
		require.ElementsMatch(t, []string{"session_id", "status"}, wkKeys(upd), label)
		end := r.ok(cli, "session_end", map[string]any{"session": name, "findings": "f"})
		require.ElementsMatch(t, []string{"status", "session_id", "claims_released", "mishaps_recorded"}, wkKeys(end), label)
	}

	// Trials: recorded and queried.
	tr := r.ok(acct, "trial_record", map[string]any{"project": wkOpen, "title": "account trial", "lab": "lab-acct"})
	require.Equal(t, wkAcct, tr["account"])
	q := r.ok(acct, "trial_query", map[string]any{"lab": "lab-acct"})
	require.Equal(t, wkAcct, q["trials"].([]any)[0].(map[string]any)["account"])
	stored, err := store.QueryTrials(ctx, r.db, store.TrialFilter{Lab: "lab-acct", Limit: 5})
	require.NoError(t, err)
	require.Equal(t, wkAcct, stored[0].Account)

	// A withheld task response drops the provenance with the rest of the
	// task's description, and its marker names exactly the fields it always
	// named.
	require.NoError(t, store.SetProjectIsolation(ctx, r.db, wkOther, core.IsolationConfidential))
	r.ok(owner, "session_start", map[string]any{"project": wkOpen, "name": "sess/owner-claim"})
	claim := r.ok(owner, "tasks_claim", map[string]any{"id": id, "session": "sess/owner-claim"})
	require.Contains(t, claim, "withheld")
	require.NotContains(t, claim, "account")
	require.NotContains(t, claim, "updated_account")
	withheld := claim["withheld"].(map[string]any)
	require.Equal(t, []any{"title", "body", "plan", "depends_on", "created_by", "favorite"}, withheld["fields"])
}

func TestRestricted_UsageSummaryNamesOnlyItsReach(t *testing.T) {
	r := newWkRig(t)
	ctx := context.Background()
	owner := r.owner("")
	secret := r.ok(owner, "memory_write", map[string]any{"project": wkConf, "name": "conf-secret", "kind": "reference",
		"description": "a confidential fact", "body": "secret body\n"})
	fact := r.ok(owner, "memory_write", map[string]any{"project": wkOpen, "name": "open-fact", "kind": "reference",
		"description": "an open fact", "body": "open body\n"})
	seedInjections(t, ctx, r.db, secret["id"].(string), 3)
	seedInjections(t, ctx, r.db, fact["id"].(string), 2)

	out := r.ok(r.acct(""), "usage_summary", nil)
	require.Equal(t, []string{"open-fact"}, topNames(t, out, "topInjected"))
	require.NotContains(t, topNames(t, out, "topUtility"), "conf-secret")
	// Counts are machine-wide by design (the tool's description says so).
	require.Equal(t, float64(2), out["memories"].(map[string]any)["active"])
}

// wkKeys returns a JSON object's keys.
func wkKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func TestRestricted_TasksByIDAndDependencies(t *testing.T) {
	r := newWkRig(t)
	owner := r.owner("")
	acct := r.acct("")
	closed := r.ok(owner, "tasks_add", map[string]any{"project": wkConf, "title": "confidential work"})["id"].(string)
	missing, err := core.NewID()
	require.NoError(t, err)

	// By id, a task in a project the account may not reach is a task that does
	// not exist: the same words, and no project named.
	byID := map[string]func(id string) map[string]any{
		"tasks_list":    func(id string) map[string]any { return map[string]any{"id": id} },
		"tasks_update":  func(id string) map[string]any { return map[string]any{"id": id, "title": "renamed"} },
		"tasks_claim":   func(id string) map[string]any { return map[string]any{"id": id} },
		"tasks_release": func(id string) map[string]any { return map[string]any{"id": id} },
	}
	for tool, args := range byID {
		got := wkOneText(t, map[string]string{
			closed:  r.refused(acct, tool, args(closed)),
			missing: r.refused(acct, tool, args(missing)),
		})
		require.NotContains(t, got, wkConf, tool)
		require.Contains(t, got, "task not found", tool)
	}

	// Nor may it hang its work on such a task.
	for tool, args := range map[string]map[string]any{
		"tasks_add":    {"project": wkOpen, "title": "dependent", "depends_on": []string{"<ID>"}},
		"tasks_update": {"id": "<OWN>", "add_depends_on": []string{"<ID>"}},
	} {
		own := r.ok(acct, "tasks_add", map[string]any{"project": wkOpen, "title": "own " + tool})["id"].(string)
		texts := map[string]string{}
		for _, id := range []string{closed, missing} {
			a := map[string]any{}
			for k, v := range args {
				switch v := v.(type) {
				case []string:
					a[k] = []string{id}
				case string:
					a[k] = strings.ReplaceAll(v, "<OWN>", own)
				}
			}
			texts[id] = r.refused(acct, tool, a)
		}
		require.Contains(t, wkOneText(t, texts), "task not found", tool)
	}
	require.Zero(t, r.count(`SELECT COUNT(*) FROM tasks WHERE title = 'dependent'`))
	require.Zero(t, r.count(`SELECT COUNT(*) FROM task_deps WHERE depends_on = ?`, closed))

	// A dependency the owner made from an open task onto the confidential one
	// shows the account that something blocks the open task, never its title
	// or its id.
	blocked := r.ok(owner, "tasks_add", map[string]any{"project": wkOpen, "title": "waits on the vault",
		"depends_on": []string{closed}})["id"].(string)
	blockerOf := func(cli *mcpclient.Client) (task, blocker map[string]any) {
		out := r.ok(cli, "tasks_ready", map[string]any{"project": wkOpen})
		for _, b := range out["blocked"].([]any) {
			if b.(map[string]any)["id"] == blocked {
				bl := b.(map[string]any)["blockers"].([]any)
				require.Len(t, bl, 1)
				return b.(map[string]any), bl[0].(map[string]any)
			}
		}
		t.Fatalf("task %s not in the blocked list", blocked)
		return nil, nil
	}
	withheld := map[string]any{"withheld": "a task in a project this connection may not reach blocks this one"}
	task, blocker := blockerOf(acct)
	require.Equal(t, withheld, blocker)
	require.NotContains(t, task, "depends_on", "nor the blocker's id")
	require.Equal(t, float64(1), task["depends_on_withheld"])
	// The confidential fence covers blocker listings for every caller: the
	// owner unbound (the global scope) is withheld the same way, and a session
	// bound to the confidential project reads it.
	task, blocker = blockerOf(owner)
	require.Equal(t, withheld, blocker)
	require.NotContains(t, task, "depends_on")
	inside := r.owner("")
	r.ok(inside, "session_start", map[string]any{"project": wkConf, "name": "cc/inside-conf"})
	task, blocker = blockerOf(inside)
	require.Equal(t, "confidential work", blocker["title"])
	require.Equal(t, []any{closed}, task["depends_on"])
	require.NotContains(t, task, "depends_on_withheld")
}
