package mcp_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/features"
	mcpserver "github.com/arctop/seamless/internal/mcp"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// lkCheckout is a remote checkout's identity, as `seam lock` sends it.
func lkCheckout(extra map[string]any) map[string]any {
	args := map[string]any{"host": "example-laptop", "repo_root": "/work/app", "repo_origin": "git@github.com:example/app.git"}
	for k, v := range extra {
		args[k] = v
	}
	return args
}

// The declared-once path, end to end: a second declaration of the same origin
// from another machine resolves to the first lock, and acquire, heartbeat and
// release behave as task claims do, naming the holder to a caller who may see
// it.
func TestLocksTools_DeclareListAcquireRelease(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	a := seedAmbient(t, ctx, db, "cc/lk-agent-a", "demo")
	b := seedAmbient(t, ctx, db, "cc/lk-agent-b", "demo")
	cli := dialClient(t, ctx, url, testKey)

	// No host: the daemon's own machine, whose repo map places /work/demo in
	// demo, so the declaration lists demo by default.
	first := callJSON(t, ctx, cli, "locks_declare", map[string]any{
		"repo_root": "/work/demo", "repo_origin": "git@github.com:example/demo.git", "session": "cc/lk-agent-a",
	})
	require.Equal(t, true, first["declared"])
	lock := first["lock"].(map[string]any)
	require.Equal(t, "demo main", lock["label"])
	require.Equal(t, "active", lock["state"])
	require.Equal(t, []any{"refs/heads/main"}, lock["refs"])
	require.Equal(t, []any{"demo"}, lock["projects"], "the project the checkout maps to")
	id := lock["id"].(string)

	again := callJSON(t, ctx, cli, "locks_declare", map[string]any{
		"host": "example-desktop", "repo_root": "/srv/demo", "repo_origin": "https://github.com/Example/demo",
		"refs": []any{"refs/heads/trunk"},
	})
	require.Equal(t, false, again["declared"])
	require.Equal(t, id, again["lock"].(map[string]any)["id"], "the ssh and https forms of one origin are one lock")
	require.Equal(t, []any{"refs/heads/main"}, again["lock"].(map[string]any)["refs"], "declared once")
	require.Contains(t, again["note"], "already declared")
	require.Len(t, again["recorded_aliases"], 1, "the second checkout's host and path")

	list := callJSON(t, ctx, cli, "locks_list", map[string]any{})
	require.Len(t, list["locks"], 1)
	mine := callJSON(t, ctx, cli, "locks_list", map[string]any{"host": "x", "repo_origin": "ssh://git@github.com/example/demo"})
	require.Len(t, mine["locks"], 1, "a checkout's own locks")
	none := callJSON(t, ctx, cli, "locks_list", map[string]any{"host": "x", "repo_origin": "https://github.com/example/other"})
	require.Empty(t, none["locks"])

	checkout := map[string]any{"host": "example-desktop", "repo_root": "/srv/demo", "repo_origin": "https://github.com/example/demo"}
	with := func(extra map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range checkout {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	got := callJSON(t, ctx, cli, "locks_acquire", with(map[string]any{"session": "cc/lk-agent-a"}))
	require.Equal(t, true, got["acquired"])
	require.Equal(t, false, got["heartbeat"])
	held := got["lock"].(map[string]any)
	require.Equal(t, true, held["held"])
	require.Equal(t, a, held["holder"])
	require.Equal(t, "cc/lk-agent-a", held["holder_session"])
	require.NotEmpty(t, held["since"])
	require.NotEmpty(t, held["until"])

	isErr, text := callErr(t, ctx, cli, "locks_acquire", with(map[string]any{"session": "cc/lk-agent-b"}))
	require.True(t, isErr)
	require.Contains(t, text, "LOCK demo main is held by session cc/lk-agent-a since ")

	beat := callJSON(t, ctx, cli, "locks_acquire", map[string]any{"id": id, "session": "cc/lk-agent-a", "lease_seconds": 1200})
	require.Equal(t, true, beat["heartbeat"])

	isErr, text = callErr(t, ctx, cli, "locks_release", map[string]any{"id": id, "session": "cc/lk-agent-b"})
	require.True(t, isErr)
	require.Contains(t, text, "not held by this session")
	rel := callJSON(t, ctx, cli, "locks_release", with(map[string]any{"session": "cc/lk-agent-a"}))
	require.Equal(t, true, rel["released"])
	require.Equal(t, false, rel["lock"].(map[string]any)["held"])
	require.Nil(t, rel["lock"].(map[string]any)["holder"])

	took := callJSON(t, ctx, cli, "locks_acquire", map[string]any{"id": id, "session": "cc/lk-agent-b"})
	require.Equal(t, b, took["lock"].(map[string]any)["holder"])

	var n int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE kind = ? AND item_id = ?`,
		string(core.EventRepoLockChanged), id).Scan(&n))
	require.GreaterOrEqual(t, n, 5, "declared, aliased, acquired, heartbeat, released, acquired")
}

// A call that names its lock badly is refused with the rule, and never guesses.
func TestLocksTools_RefusesWhatItCannotResolve(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	seedAmbient(t, ctx, db, "cc/lk-agent-a", "demo")
	cli := dialClient(t, ctx, url, testKey)
	declared := callJSON(t, ctx, cli, "locks_declare", lkCheckout(nil))
	id := declared["lock"].(map[string]any)["id"].(string)

	for _, tc := range []struct {
		name, tool, want string
		args             map[string]any
	}{
		{"a remote cwd alone names no repository", "locks_declare", "no repository identity",
			map[string]any{"host": "example-laptop", "cwd": "/work/app"}},
		{"nothing at all", "locks_declare", "name the repository", map[string]any{}},
		{"an acquire naming nothing", "locks_acquire", "name the lock", map[string]any{"session": "cc/lk-agent-a"}},
		{"id and a checkout", "locks_acquire", "not both", lkCheckout(map[string]any{"id": id, "session": "cc/lk-agent-a"})},
		{"a name that is not a token", "locks_acquire", "invalid lock name", lkCheckout(map[string]any{"name": "Main!"})},
		{"no such lock", "locks_acquire", "no lock named release is declared for this repository (github.com/example/app)",
			lkCheckout(map[string]any{"name": "release", "session": "cc/lk-agent-a"})},
		{"no such id", "locks_release", "no lock with id", map[string]any{"id": "01NOSUCHLOCK", "session": "cc/lk-agent-a"}},
		{"a ref outside refs/", "locks_declare", "invalid guarded ref", lkCheckout(map[string]any{"name": "rel", "refs": []any{"main"}})},
		{"a relative root", "locks_declare", "not an absolute path", map[string]any{"host": "h", "repo_root": "work/app"}},
		{"a lease past the ceiling", "locks_acquire", "a lock's lease is at most 14400 seconds (4 hours)",
			map[string]any{"id": id, "session": "cc/lk-agent-a", "lease_seconds": 5 * 3600}},
		{"a zero lease", "locks_acquire", "invalid lease_seconds", map[string]any{"id": id, "session": "cc/lk-agent-a", "lease_seconds": 0}},
		{"a project that does not exist", "locks_declare", `no project "example-nothere"`,
			lkCheckout(map[string]any{"name": "rel", "projects": []any{"example-nothere"}})},
		{"global is no project", "locks_declare", "is not one", lkCheckout(map[string]any{"name": "rel", "projects": []any{"global"}})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isErr, text := callErr(t, ctx, cli, tc.tool, tc.args)
			require.True(t, isErr, text)
			require.Contains(t, text, tc.want)
		})
	}
	// No session at all to hold it as: refused, never guessed.
	_, err := db.ExecContext(ctx, `UPDATE sessions SET status = 'completed'`)
	require.NoError(t, err)
	isErr, text := callErr(t, ctx, cli, "locks_acquire", map[string]any{"id": id})
	require.True(t, isErr)
	require.Contains(t, text, "no active session to hold the lock as")
}

// The daemon reads a checkout from its own disk only for its own machine with
// no View on the request; otherwise the caller must send the identity.
func TestLocksTools_DaemonReadsOnlyItsOwnDisk(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(repo, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(repo, ".git", "config"),
		[]byte("[remote \"origin\"]\n\turl = git@github.com:example/disk.git\n"), 0o644))
	sub := filepath.Join(repo, "pkg")
	require.NoError(t, os.MkdirAll(sub, 0o755))

	url, _ := newServer(t)
	cli := dialClient(t, ctx, url, testKey)
	got := callJSON(t, ctx, cli, "locks_declare", map[string]any{"cwd": sub})
	aliases := got["lock"].(map[string]any)["aliases"].([]any)
	require.Equal(t, map[string]any{"kind": "origin", "alias": "github.com/example/disk"}, aliases[0], "read from this machine's disk")

	isErr, text := callErr(t, ctx, cli, "locks_declare", map[string]any{"cwd": sub, "host": "example-elsewhere"})
	require.True(t, isErr, "another machine's path is never read here")
	require.Contains(t, text, "no repository identity")

	view := lkOwnerView(t)
	ownerURL := lkServe(t, func() (access.View, bool) { return view, true })
	owner := dialClient(t, ctx, ownerURL, testKey)
	isErr, text = callErr(t, ctx, owner, "locks_declare", map[string]any{"cwd": sub})
	require.True(t, isErr, "under peer_auth no path a caller names is read")
	require.Contains(t, text, "no repository identity")
}

// lkPolicy is a peer_auth policy with the owner and one restricted account.
func lkPolicy(t *testing.T) *access.Policy {
	t.Helper()
	p, err := access.New(config.PeerAuth{Enabled: true, Owner: "owner", Restricted: config.BareRestricted("_example_agent")},
		func(name string) (access.Account, error) {
			switch name {
			case "owner":
				return access.Account{UID: 501, Name: "owner", Home: "/Users/owner"}, nil
			case "_example_agent":
				return access.Account{UID: 502, Name: "_example_agent", Home: "/Users/_example_agent"}, nil
			}
			return access.Account{}, fmt.Errorf("unknown account %q", name)
		})
	require.NoError(t, err)
	return p
}

func lkOwnerView(t *testing.T) access.View {
	t.Helper()
	return lkPolicy(t).OwnerView()
}

// lkRig is one store served three ways: the owner's View, a restricted
// account's, and no View at all.
type lkRig struct {
	db                 *sql.DB
	owner, agent, none *mcpclient.Client
}

// lkServe serves a server over a fresh store behind view.
func lkServe(t *testing.T, view func() (access.View, bool)) string {
	t.Helper()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	h := mcpserver.New(mcpserver.Config{DB: db, Events: events.NewRecorder(db), APIKey: testKey,
		Features: config.Features{RepoLocks: true}}).Handler()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if v, ok := view(); ok {
			req = req.WithContext(access.WithView(req.Context(), v))
		}
		h.ServeHTTP(w, req)
	}))
	t.Cleanup(ts.Close)
	return ts.URL
}

func lkNewRig(t *testing.T) *lkRig {
	t.Helper()
	ctx := context.Background()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	for _, slug := range []string{"example-open", "example-ops"} {
		_, err := store.EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
	}
	require.NoError(t, store.AddRepoMapping(ctx, db, "/work/open", "example-open"))
	require.NoError(t, store.AddRepoMapping(ctx, db, "/work/ops", "example-ops"))
	h := mcpserver.New(mcpserver.Config{DB: db, Events: events.NewRecorder(db), APIKey: testKey,
		Features: config.Features{RepoLocks: true}}).Handler()
	p := lkPolicy(t)
	serve := func(view func() (access.View, bool)) string {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if view != nil {
				if v, ok := view(); ok {
					req = req.WithContext(access.WithView(req.Context(), v))
				}
			}
			h.ServeHTTP(w, req)
		}))
		t.Cleanup(ts.Close)
		return ts.URL
	}
	agentURL := serve(func() (access.View, bool) {
		v, ok := p.PeerView(502)
		require.True(t, ok)
		return v.WithOpenProjects([]string{"example-open"}), true
	})
	ownerURL := serve(func() (access.View, bool) { return p.OwnerView(), true })
	return &lkRig{db: db, owner: dialClient(t, ctx, ownerURL, testKey), agent: dialClient(t, ctx, agentURL, testKey),
		none: dialClient(t, ctx, serve(nil), testKey)}
}

// A restricted account's declaration is stored as requested and waits for the
// owner; nobody holds it, and the owner principal's declaration of it is
// refused, since approving it is the console's decision.
func TestLocksTools_RestrictedDeclarationWaitsForTheOwner(t *testing.T) {
	ctx := context.Background()
	r := lkNewRig(t)
	checkout := map[string]any{"repo_root": "/work/open", "repo_origin": "git@github.com:example/open.git"}

	asked := callJSON(t, ctx, r.agent, "locks_declare", checkout)
	require.Equal(t, true, asked["declared"])
	lock := asked["lock"].(map[string]any)
	require.Equal(t, "requested", lock["state"])
	require.Equal(t, []any{"example-open"}, lock["projects"])
	require.Contains(t, asked["note"], "waits for the owner")
	l, ok, err := store.RepoLockByID(ctx, r.db, lock["id"].(string))
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "_example_agent", l.Account, "the declaration carries its account's label")

	isErr, text := callErr(t, ctx, r.owner, "locks_declare", checkout)
	require.True(t, isErr)
	require.Contains(t, text, "the owner approves or discards it on the console")

	agentSess := lkSession(t, r.db, "_example_agent:cc/lk-agent", "example-open", "_example_agent")
	isErr, text = callErr(t, ctx, r.agent, "locks_acquire", map[string]any{"id": l.ID, "session": "cc/lk-agent"})
	require.True(t, isErr, text)
	require.Contains(t, text, "has not approved yet")

	// The owner approves (the console's decision, here straight in the store);
	// the account may then hold it as its own session.
	_, err = store.ApproveRepoLock(ctx, r.db, l.ID, l.CreatedAt)
	require.NoError(t, err)
	got := callJSON(t, ctx, r.agent, "locks_acquire", map[string]any{"id": l.ID, "session": "cc/lk-agent"})
	require.Equal(t, agentSess, got["lock"].(map[string]any)["holder"], "its own session is named to it")

	// A restricted account may not list a project it does not reach.
	isErr, text = callErr(t, ctx, r.agent, "locks_declare", map[string]any{"repo_root": "/work/ops", "projects": []any{"example-ops"}})
	require.True(t, isErr)
	require.Contains(t, text, "project example-ops is not reachable from account _example_agent")
	require.NotContains(t, text, "no project", "one refusal text, never an existence oracle")

	// The project a checkout maps to is the repo map's answer, not the
	// caller's: one the account may not reach is not listed, and not named.
	isErr, text = callErr(t, ctx, r.agent, "locks_declare", map[string]any{"repo_root": "/work/ops", "name": "release"})
	require.False(t, isErr, text)
	require.NotContains(t, text, "example-ops", "a mapped path reads as an unmapped one")
	unmapped := callJSON(t, ctx, r.agent, "locks_declare", map[string]any{"repo_root": "/work/nothere", "name": "release"})
	require.Empty(t, unmapped["lock"].(map[string]any)["projects"])
}

// lkSession creates an active session of account in project.
func lkSession(t *testing.T, db *sql.DB, name, project, account string) string {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	now := lkNowUTC()
	require.NoError(t, store.CreateSession(access.WithView(context.Background(), lkAccountView(t, account)), db, core.Session{
		ID: id, Name: name, ProjectSlug: project, Status: core.SessionActive, CreatedAt: now, UpdatedAt: now,
	}))
	return id
}

func lkAccountView(t *testing.T, account string) access.View {
	t.Helper()
	p := lkPolicy(t)
	if account == "" {
		return p.OwnerView()
	}
	v, ok := p.PeerView(502)
	require.True(t, ok)
	return v.WithOpenProjects([]string{"example-open"})
}

// A holder is named only to a caller who may read its session: the owner's
// session is no restricted account's to see, and the answer says held, with
// since and until, and names nobody.
func TestLocksTools_HolderNamedOnlyWhereItsSessionIsReadable(t *testing.T) {
	ctx := context.Background()
	r := lkNewRig(t)
	ownerSess := lkSession(t, r.db, "cc/lk-owner", "example-open", "")
	declared := callJSON(t, ctx, r.owner, "locks_declare", map[string]any{"repo_root": "/work/open", "repo_origin": "https://example.com/team/open.git"})
	id := declared["lock"].(map[string]any)["id"].(string)
	callJSON(t, ctx, r.owner, "locks_acquire", map[string]any{"id": id, "session": "cc/lk-owner"})

	seen := callJSON(t, ctx, r.owner, "locks_list", map[string]any{})["locks"].([]any)[0].(map[string]any)
	require.Equal(t, ownerSess, seen["holder"])
	hidden := callJSON(t, ctx, r.agent, "locks_list", map[string]any{})["locks"].([]any)[0].(map[string]any)
	require.Equal(t, true, hidden["held"])
	require.NotEmpty(t, hidden["until"])
	require.Nil(t, hidden["holder"], "another principal's session is not named")
	require.Nil(t, hidden["holder_session"])

	lkSession(t, r.db, "_example_agent:cc/lk-agent", "example-open", "_example_agent")
	isErr, text := callErr(t, ctx, r.agent, "locks_acquire", map[string]any{"id": id, "session": "cc/lk-agent"})
	require.True(t, isErr)
	require.Contains(t, text, "is held by another session since ")
	require.NotContains(t, text, ownerSess)
	require.NotContains(t, text, "cc/lk-owner")
}

// With the feature off -- the default -- the four tools are neither listed
// nor callable; on, they are back, live, from the next call.
func TestLocksTools_FollowTheFeature(t *testing.T) {
	ctx := context.Background()
	f, ok := features.Get(features.RepoLocks)
	require.True(t, ok)
	require.Equal(t, []string{"locks_list", "locks_declare", "locks_acquire", "locks_release"}, f.Tools)

	url, db := newServerCfg(t, defaultFeatures)
	cli := dialClient(t, ctx, url, testKey)
	requireGated(t, ctx, cli, f.Tools)
	require.NoError(t, store.SetFeaturesConfig(ctx, db, config.Features{RepoLocks: true}))
	requireExposed(t, ctx, cli, f.Tools)
	callJSON(t, ctx, cli, "locks_declare", lkCheckout(nil))
	require.NoError(t, store.SetFeaturesConfig(ctx, db, config.Features{}))
	requireGated(t, ctx, cli, f.Tools)
	n, _, err := store.CountRepoLocks(ctx, db)
	require.NoError(t, err)
	require.Equal(t, 1, n, "turning the feature off deletes no lock")
}

// lkNowUTC is the wall clock, for rows the tools compare with time.Now.
func lkNowUTC() (now time.Time) { return time.Now().UTC() }
