package mcp_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
	mcpserver "github.com/arctop/seamless/internal/mcp"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// Blocker listings under the read fence: a task that blocks another, from a
// project the caller may not read (a locked project without a lease, a
// confidential one from a session bound elsewhere), is withheld in every task
// answer -- its title and its id -- while the count stays. With peer_auth off
// and nothing isolated, every answer is byte for byte what it always was.

const (
	bfAlpha = "example-alpha"
	bfBeta  = "example-beta"

	bfA1 = "01KBF000000000000000000AA1" // alpha, open: blocks A2
	bfB1 = "01KBF000000000000000000BB1" // beta, open: blocks A2
	bfD1 = "01KBF000000000000000000DD1" // beta, done: A3's closed dependency
	bfA2 = "01KBF000000000000000000AA2" // alpha, open: waits on A1 and B1
	bfA3 = "01KBF000000000000000000AA3" // alpha, open: ready (its only dependency is done)
)

// bfSeed writes the cross-project fixture straight through the store, with
// fixed ids and times so every answer is a literal.
func bfSeed(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	for _, slug := range []string{bfAlpha, bfBeta} {
		_, err := store.EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
	}
	t0 := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	closed := t0.Add(2 * time.Minute)
	for _, task := range []core.Task{
		{ID: bfA1, ProjectSlug: bfAlpha, Title: "alpha blocker", Status: core.TaskOpen, CreatedAt: t0},
		{ID: bfB1, ProjectSlug: bfBeta, Title: "beta blocker", Status: core.TaskOpen, CreatedAt: t0.Add(time.Minute)},
		{ID: bfD1, ProjectSlug: bfBeta, Title: "beta finished", Status: core.TaskDone, CreatedAt: closed, ClosedAt: &closed},
		{ID: bfA2, ProjectSlug: bfAlpha, Title: "alpha waits on both", Status: core.TaskOpen,
			DependsOn: []string{bfA1, bfB1}, CreatedAt: t0.Add(3 * time.Minute)},
		{ID: bfA3, ProjectSlug: bfAlpha, Title: "alpha after done", Status: core.TaskOpen,
			DependsOn: []string{bfD1}, CreatedAt: t0.Add(4 * time.Minute)},
	} {
		task.UpdatedAt = task.CreatedAt
		require.NoError(t, store.CreateTask(ctx, db, task))
	}
}

// bfText calls a tool and returns its first text item and whether it is an
// error.
func bfText(t *testing.T, cli *mcpclient.Client, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := cli.CallTool(context.Background(), mcp.CallToolRequest{Params: mcp.CallToolParams{Name: name, Arguments: args}})
	require.NoError(t, err)
	return resultText(t, res), res.IsError
}

// The exact answers, as main renders them: peer_auth off, no project
// isolated, blockers and dependencies across two open projects.
const (
	bfJSONA1 = `{"created_by":"","id":"` + bfA1 + `","project":"example-alpha","status":"open","title":"alpha blocker"}`
	bfJSONA2 = `{"created_by":"","depends_on":["` + bfA1 + `","` + bfB1 + `"],"id":"` + bfA2 +
		`","project":"example-alpha","status":"open","title":"alpha waits on both"}`
	bfJSONA3 = `{"created_by":"","depends_on":["` + bfD1 + `"],"id":"` + bfA3 +
		`","project":"example-alpha","status":"open","title":"alpha after done"}`

	bfReadyOff = `{"blocked":[{"blockers":[{"id":"` + bfA1 + `","status":"open","title":"alpha blocker"},` +
		`{"id":"` + bfB1 + `","status":"open","title":"beta blocker"}],"created_by":"","depends_on":["` + bfA1 + `","` + bfB1 +
		`"],"id":"` + bfA2 + `","project":"example-alpha","status":"open","title":"alpha waits on both"}],` +
		`"project":"example-alpha","ready":[` + bfJSONA1 + `,` + bfJSONA3 + `]}`
	bfListOff  = `{"project":"example-alpha","tasks":[` + bfJSONA3 + `,` + bfJSONA2 + `,` + bfJSONA1 + `]}`
	bfByIDOff  = `{"project":"example-alpha","tasks":[` + bfJSONA2 + `]}`
	bfClaimOff = `tasks_claim: store.ClaimTask: task blocked: task "` + bfA2 + `" waits on ` + bfA1 + ` (open), ` + bfB1 + ` (open)`
	bfUpdOff   = `{"body":"more detail","created_by":"","depends_on":["` + bfA1 + `","` + bfB1 + `"],"id":"` + bfA2 +
		`","project":"example-alpha","status":"open","title":"alpha waits on both"}`
)

func TestBlockerFence_OffIsByteIdentical(t *testing.T) {
	url, db := newServer(t)
	bfSeed(t, db)
	ctx := context.Background()

	unbound := dialClient(t, ctx, url, testKey)
	bound := dialClient(t, ctx, url, testKey)
	callJSON(t, ctx, bound, "session_start", map[string]any{"project": bfBeta, "name": "cc/bf-beta"})

	for name, cli := range map[string]*mcpclient.Client{"unbound": unbound, "bound elsewhere": bound} {
		got, isErr := bfText(t, cli, "tasks_ready", map[string]any{"project": bfAlpha})
		require.False(t, isErr, got)
		require.Equal(t, bfReadyOff, got, name)

		got, isErr = bfText(t, cli, "tasks_list", map[string]any{"project": bfAlpha})
		require.False(t, isErr, got)
		require.Equal(t, bfListOff, got, name)

		got, isErr = bfText(t, cli, "tasks_list", map[string]any{"id": bfA2})
		require.False(t, isErr, got)
		require.Equal(t, bfByIDOff, got, name)
	}

	got, isErr := bfText(t, bound, "tasks_claim", map[string]any{"id": bfA2})
	require.True(t, isErr, got)
	require.Equal(t, bfClaimOff, got)

	got, isErr = bfText(t, bound, "tasks_update", map[string]any{"id": bfA2, "body": "more detail"})
	require.False(t, isErr, got)
	require.Equal(t, bfUpdOff, got)
}

// The same fixture with example-beta confidential and peer_auth off: a caller
// outside it (unbound, or bound to example-alpha) gets every beta task that
// blocks or precedes an alpha task withheld -- no title, no id, the count
// kept -- in tasks_ready, tasks_list (by project and by id), the claim's
// "task blocked" refusal, and a write's answer. A session bound to the
// confidential project reads all of it, byte for byte as before.
const (
	bfWithheldBlocker = `{"withheld":"a task in a project this connection may not reach blocks this one"}`

	bfJSONA2Fenced = `{"created_by":"","depends_on":["` + bfA1 + `"],"depends_on_withheld":1,"id":"` + bfA2 +
		`","project":"example-alpha","status":"open","title":"alpha waits on both"}`
	bfJSONA3Fenced = `{"created_by":"","depends_on_withheld":1,"id":"` + bfA3 +
		`","project":"example-alpha","status":"open","title":"alpha after done"}`

	bfReadyFenced = `{"blocked":[{"blockers":[{"id":"` + bfA1 + `","status":"open","title":"alpha blocker"},` +
		bfWithheldBlocker + `],"created_by":"","depends_on":["` + bfA1 + `"],"depends_on_withheld":1,"id":"` + bfA2 +
		`","project":"example-alpha","status":"open","title":"alpha waits on both"}],` +
		`"project":"example-alpha","ready":[` + bfJSONA1 + `,` + bfJSONA3Fenced + `]}`
	bfListFenced  = `{"project":"example-alpha","tasks":[` + bfJSONA3Fenced + `,` + bfJSONA2Fenced + `,` + bfJSONA1 + `]}`
	bfByIDFenced  = `{"project":"example-alpha","tasks":[` + bfJSONA2Fenced + `]}`
	bfClaimFenced = `tasks_claim: store.ClaimTask: task blocked: task "` + bfA2 + `" waits on ` + bfA1 +
		` (open), a task in a project this connection may not reach`
	bfUpdFenced = `{"body":"more detail","created_by":"","depends_on":["` + bfA1 + `"],"depends_on_withheld":1,"id":"` + bfA2 +
		`","project":"example-alpha","status":"open","title":"alpha waits on both"}`
)

func TestBlockerFence_ConfidentialProject(t *testing.T) {
	url, db := newServer(t)
	bfSeed(t, db)
	ctx := context.Background()
	require.NoError(t, store.SetProjectIsolation(ctx, db, bfBeta, core.IsolationConfidential))

	unbound := dialClient(t, ctx, url, testKey)
	alpha := dialClient(t, ctx, url, testKey)
	callJSON(t, ctx, alpha, "session_start", map[string]any{"project": bfAlpha, "name": "cc/bf-alpha"})
	inside := dialClient(t, ctx, url, testKey)
	callJSON(t, ctx, inside, "session_start", map[string]any{"project": bfBeta, "name": "cc/bf-beta"})

	for name, c := range map[string]struct {
		cli               *mcpclient.Client
		ready, list, byID string
	}{
		"unbound":        {unbound, bfReadyFenced, bfListFenced, bfByIDFenced},
		"bound to alpha": {alpha, bfReadyFenced, bfListFenced, bfByIDFenced},
		"bound inside":   {inside, bfReadyOff, bfListOff, bfByIDOff},
	} {
		got, isErr := bfText(t, c.cli, "tasks_ready", map[string]any{"project": bfAlpha})
		require.False(t, isErr, got)
		require.Equal(t, c.ready, got, name)
		got, isErr = bfText(t, c.cli, "tasks_list", map[string]any{"project": bfAlpha})
		require.False(t, isErr, got)
		require.Equal(t, c.list, got, name)
		got, isErr = bfText(t, c.cli, "tasks_list", map[string]any{"id": bfA2})
		require.False(t, isErr, got)
		require.Equal(t, c.byID, got, name)
	}

	got, isErr := bfText(t, alpha, "tasks_claim", map[string]any{"id": bfA2})
	require.True(t, isErr, got)
	require.Equal(t, bfClaimFenced, got)

	// A confidential task takes a claim from outside (a write in), and its own
	// project's blockers are withheld from that caller; inside, the refusal
	// names every blocker.
	b2, err := core.NewID()
	require.NoError(t, err)
	now := time.Now().UTC()
	require.NoError(t, store.CreateTask(ctx, db, core.Task{ID: b2, ProjectSlug: bfBeta, Title: "beta waits on both",
		Status: core.TaskOpen, DependsOn: []string{bfA1, bfB1}, CreatedAt: now, UpdatedAt: now}))
	got, isErr = bfText(t, alpha, "tasks_claim", map[string]any{"id": b2})
	require.True(t, isErr, got)
	require.Equal(t, `tasks_claim: store.ClaimTask: task blocked: task "`+b2+`" waits on `+bfA1+
		` (open), a task in a project this connection may not reach`, got)
	got, isErr = bfText(t, inside, "tasks_claim", map[string]any{"id": b2})
	require.True(t, isErr, got)
	require.Equal(t, `tasks_claim: store.ClaimTask: task blocked: task "`+b2+`" waits on `+bfA1+` (open), `+bfB1+` (open)`, got)

	got, isErr = bfText(t, alpha, "tasks_update", map[string]any{"id": bfA2, "body": "more detail"})
	require.False(t, isErr, got)
	require.Equal(t, bfUpdFenced, got)

	// tasks_add answers with the dependencies the caller may read, too.
	added := callJSON(t, ctx, alpha, "tasks_add", map[string]any{"project": bfAlpha, "title": "alpha after beta",
		"depends_on": []string{bfB1, bfA1}})
	require.Equal(t, []any{bfA1}, added["depends_on"])
	require.Equal(t, float64(1), added["depends_on_withheld"])

	// With the blockers closed the task is claimable: the fence hides the
	// edge, not the queue.
	_, err = db.ExecContext(ctx, `UPDATE tasks SET status = 'done' WHERE id IN (?, ?)`, bfA1, bfB1)
	require.NoError(t, err)
	claimed := callJSON(t, ctx, alpha, "tasks_claim", map[string]any{"id": bfA2})
	require.Equal(t, []any{bfA1}, claimed["depends_on"])
	require.Equal(t, float64(1), claimed["depends_on_withheld"])
	released := callJSON(t, ctx, alpha, "tasks_release", map[string]any{"id": bfA2})
	require.NotContains(t, released, "withheld", "the task itself is alpha's")
	require.Equal(t, float64(1), released["depends_on_withheld"])
}

// A sealed caller reads nothing outside its project: an open project's task
// that blocks a sealed one is withheld from it, as a confidential one is from
// outside.
func TestBlockerFence_SealedCaller(t *testing.T) {
	url, db := newServer(t)
	bfSeed(t, db)
	ctx := context.Background()
	const vault = "example-vault"
	_, err := store.EnsureProject(ctx, db, vault, vault)
	require.NoError(t, err)
	require.NoError(t, store.SetProjectIsolation(ctx, db, vault, core.IsolationSealed))
	id, err := core.NewID()
	require.NoError(t, err)
	now := time.Now().UTC()
	require.NoError(t, store.CreateTask(ctx, db, core.Task{ID: id, ProjectSlug: vault, Title: "vault waits",
		Status: core.TaskOpen, DependsOn: []string{bfA1}, CreatedAt: now, UpdatedAt: now}))

	sealed := dialClient(t, ctx, url, testKey)
	callJSON(t, ctx, sealed, "session_start", map[string]any{"project": vault, "name": "cc/bf-vault"})
	text, isErr := bfText(t, sealed, "tasks_ready", nil)
	require.False(t, isErr, text)
	require.NotContains(t, text, bfA1)
	require.NotContains(t, text, "alpha blocker")
	require.Contains(t, text, bfWithheldBlocker)
	require.Contains(t, text, `"depends_on_withheld":1`)
}

// bfNoBlocker requires that text names neither the locked task's id nor its
// title.
func bfNoBlocker(t *testing.T, text, id, title, what string) {
	t.Helper()
	require.NotContains(t, text, id, "%s names the locked task's id", what)
	require.NotContains(t, text, title, "%s names the locked task's title", what)
}

// A locked project (peer_auth.locked) without a lease: the owner on TCP, and
// a socket owner whose tree holds no lease, are told something blocks the
// open project's work, never what. The agent whose tree the owner approved
// reads the blocker in every answer.
func TestBlockerFence_LockedProject(t *testing.T) {
	r := newLockRig(t)
	ctx := context.Background()
	now := r.clock()
	newTask := func(project, title string, deps ...string) string {
		id, err := core.NewID()
		require.NoError(t, err)
		require.NoError(t, store.CreateTask(ctx, r.db, core.Task{ID: id, ProjectSlug: project, Title: title,
			Status: core.TaskOpen, DependsOn: deps, CreatedAt: now, UpdatedAt: now}))
		now = now.Add(time.Second)
		return id
	}
	const secret = "reconcile the quarter ledger"
	fin := newTask(lkFin, secret)
	prep := newTask(lkOps, "ops prep")
	waits := newTask(lkOps, "ops waits on finance", fin)
	both := newTask(lkOps, "ops waits on both", fin, prep)
	finWaits := newTask(lkFin, "finance waits on both", fin, prep)

	tcp := r.dial(r.tcp(), false)
	r.ok(tcp, "session_start", map[string]any{"project": lkOps, "name": "cc/tcp-ops"})
	stranger := r.dial(r.socket(lkStranger), false)
	r.ok(stranger, "session_start", map[string]any{"project": lkOps, "name": "cc/stranger-ops"})
	r.session("cc/fin", lkFin)
	agent := r.dial(r.socket(lkBridge, lkAgent), true)

	readyOf := func(cli *mcpclient.Client) (string, map[string]map[string]any) {
		text, _ := r.ok(cli, "tasks_ready", map[string]any{"project": lkOps})
		var out struct {
			Blocked []map[string]any `json:"blocked"`
		}
		require.NoError(t, json.Unmarshal([]byte(text), &out), text)
		byID := map[string]map[string]any{}
		for _, b := range out.Blocked {
			byID[b["id"].(string)] = b
		}
		return text, byID
	}
	withheld := map[string]any{"withheld": "a task in a project this connection may not reach blocks this one"}

	for name, cli := range map[string]*mcpclient.Client{
		"tcp": tcp, "socket without a lease": stranger, "agent before approval": agent,
	} {
		text, blocked := readyOf(cli)
		bfNoBlocker(t, text, fin, secret, name+" tasks_ready")
		require.Equal(t, []any{withheld}, blocked[waits]["blockers"], name)
		require.NotContains(t, blocked[waits], "depends_on", name)
		require.Equal(t, float64(1), blocked[waits]["depends_on_withheld"], name)
		require.Equal(t, []any{withheld, map[string]any{"id": prep, "status": "open", "title": "ops prep"}},
			blocked[both]["blockers"], "%s: the count is kept", name)
		require.Equal(t, []any{prep}, blocked[both]["depends_on"], name)
		require.Equal(t, float64(1), blocked[both]["depends_on_withheld"], name)

		list, _ := r.ok(cli, "tasks_list", map[string]any{"project": lkOps})
		bfNoBlocker(t, list, fin, secret, name+" tasks_list")
		require.Contains(t, list, `"depends_on_withheld":1`, name)
		byID, _ := r.ok(cli, "tasks_list", map[string]any{"id": waits})
		bfNoBlocker(t, byID, fin, secret, name+" tasks_list id=")
	}
	for name, cli := range map[string]*mcpclient.Client{"tcp": tcp, "socket without a lease": stranger} {
		got := r.refused(cli, "tasks_claim", map[string]any{"id": both})
		bfNoBlocker(t, got, fin, secret, name+" tasks_claim")
		require.Contains(t, got, "task blocked", name)
		require.Contains(t, got, "waits on "+prep+" (open), a task in a project this connection may not reach", name)
	}
	require.Empty(t, r.pending(), "withholding a blocker is a filter, not an ask")

	// The owner approves the agent's request; its tree reads the blocker.
	r.refused(agent, "tasks_list", nil)
	pending := r.pending()
	require.Len(t, pending, 1)
	r.approve(pending[0].ID, time.Hour)
	text, blocked := readyOf(agent)
	require.Contains(t, text, secret)
	require.Equal(t, []any{map[string]any{"id": fin, "status": "open", "title": secret}}, blocked[waits]["blockers"])
	require.Equal(t, []any{fin}, blocked[waits]["depends_on"])
	require.NotContains(t, blocked[waits], "depends_on_withheld")
	got := r.refused(agent, "tasks_claim", map[string]any{"id": finWaits})
	require.Contains(t, got, "waits on "+fin+" (open), "+prep+" (open)")

	// Another tree stays locked out of it.
	text, _ = readyOf(stranger)
	bfNoBlocker(t, text, fin, secret, "stranger after the approval")
}

// The lock alone withholds: with the locked project's isolation open (which
// serve's preflight refuses, so only the View stands here), the owner on TCP
// is still withheld the blocker, and with peer_auth off it reads it.
func TestBlockerFence_LockAloneWithholds(t *testing.T) {
	r := newLockRig(t)
	ctx := context.Background()
	require.NoError(t, store.SetProjectIsolation(ctx, r.db, lkFin, core.IsolationOpen))
	now := r.clock()
	fin, err := core.NewID()
	require.NoError(t, err)
	waits, err := core.NewID()
	require.NoError(t, err)
	const secret = "reconcile the quarter ledger"
	require.NoError(t, store.CreateTask(ctx, r.db, core.Task{ID: fin, ProjectSlug: lkFin, Title: secret,
		Status: core.TaskOpen, CreatedAt: now, UpdatedAt: now}))
	require.NoError(t, store.CreateTask(ctx, r.db, core.Task{ID: waits, ProjectSlug: lkOps, Title: "ops waits",
		Status: core.TaskOpen, DependsOn: []string{fin}, CreatedAt: now.Add(time.Second), UpdatedAt: now.Add(time.Second)}))

	tcp := r.dial(r.tcp(), false)
	text, _ := r.ok(tcp, "tasks_ready", map[string]any{"project": lkOps})
	bfNoBlocker(t, text, fin, secret, "tcp")
	require.Contains(t, text, bfWithheldBlocker)

	off := r.dial(r.serve(r.srv, nil), false)
	text, _ = r.ok(off, "tasks_ready", map[string]any{"project": lkOps})
	require.Contains(t, text, secret)
	require.Contains(t, text, `"depends_on":["`+fin+`"]`)
}

// The users map (peer_auth.users, the model locked projects replace): an
// account is withheld a blocker from a project outside its reach, and the
// owner is withheld one from a project sealed to that account.
func TestBlockerFence_UsersMap(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mgr, err := files.NewManager(dir, db, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })
	const (
		role  = "example-role"
		vault = "example-vault"
	)
	for _, slug := range []string{bfAlpha, role, vault} {
		_, err := store.EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
	}
	policy, err := access.New(config.PeerAuth{Enabled: true, Owner: "owner", Users: map[string]config.PeerUser{
		"_example_role": {Sealed: []string{vault}, Projects: []string{role}},
	}}, func(n string) (access.Account, error) {
		switch n {
		case "owner":
			return access.Account{UID: 501, Name: "owner"}, nil
		case "_example_role":
			return access.Account{UID: 502, Name: "_example_role"}, nil
		}
		return access.Account{}, fmt.Errorf("no account %s", n)
	})
	require.NoError(t, err)
	now := time.Now().UTC()
	newTask := func(project, title string, deps ...string) string {
		id, err := core.NewID()
		require.NoError(t, err)
		require.NoError(t, store.CreateTask(ctx, db, core.Task{ID: id, ProjectSlug: project, Title: title,
			Status: core.TaskOpen, DependsOn: deps, CreatedAt: now, UpdatedAt: now}))
		now = now.Add(time.Second)
		return id
	}
	ownerTask := newTask(bfAlpha, "the owner's alpha work")
	vaultTask := newTask(vault, "the vault's sealed work")
	newTask(role, "role waits on both", ownerTask, vaultTask)

	srv := mcpserver.New(mcpserver.Config{DB: db, Files: mgr, Events: events.NewRecorder(db), APIKey: testKey,
		Retrieve: retrieve.New(db, nil, config.Budgets{MaxBriefingTokens: 1500, RecallBudgetTokens: 1000}, nil)})
	serve := func(view access.View) string {
		h := srv.Handler()
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			h.ServeHTTP(w, req.WithContext(access.WithView(req.Context(), view)))
		}))
		t.Cleanup(ts.Close)
		return ts.URL
	}
	roleView, ok := policy.PeerView(502)
	require.True(t, ok)

	for name, c := range map[string]struct {
		view         access.View
		hidden, seen []string
	}{
		"the account": {roleView, []string{ownerTask, "the owner's alpha work"}, []string{vaultTask, "the vault's sealed work"}},
		"the owner":   {policy.OwnerView(), []string{vaultTask, "the vault's sealed work"}, []string{ownerTask, "the owner's alpha work"}},
	} {
		cli := dialClient(t, ctx, serve(c.view), testKey)
		text, isErr := bfText(t, cli, "tasks_ready", map[string]any{"project": role})
		require.False(t, isErr, "%s: %s", name, text)
		for _, s := range c.hidden {
			require.NotContains(t, text, s, name)
		}
		for _, s := range c.seen {
			require.Contains(t, text, s, name)
		}
		require.Contains(t, text, bfWithheldBlocker, name)
		require.Contains(t, text, `"depends_on_withheld":1`, name)
	}
}
