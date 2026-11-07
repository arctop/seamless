package hooks

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// A SessionEnd revokes the session's leases on locked projects through the
// revoker serve wires in while a project is locked; with none wired (the
// default), nothing is called.
func TestSessionEnd_RevokesTheSessionsLeases(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, store.SetSetting(ctx, db, store.SettingRepoProjectMap, `{"/work/demo":"demo"}`))
	require.NoError(t, store.AdoptLocalHost(ctx, db, localHostName))

	var mu sync.Mutex
	var revoked []string
	h := NewHandler(Config{
		DB: db, Retrieve: retrieve.New(db, nil, config.Budgets{MaxBriefingTokens: 1500, RecallBudgetTokens: 1000}, nil),
		Events: events.NewRecorder(db), APIKey: testKey, LocalHost: localHostName,
		RevokeSessionLeases: func(_ context.Context, id string) (int64, error) {
			mu.Lock()
			defer mu.Unlock()
			revoked = append(revoked, id)
			return 1, nil
		},
	})
	mux := http.NewServeMux()
	h.Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	e := &hostEnv{ts: ts, db: db}

	e.postFrom(t, "session-start", map[string]string{"host": localHostName, "repo_root": "/work/demo", "main_root": "/work/demo"},
		map[string]any{"session_id": "lock-1", "cwd": "/work/demo", "source": "startup"})
	sess := e.session(t, "lock-1")
	e.postFrom(t, "session-end", map[string]string{"host": localHostName}, map[string]any{"session_id": "lock-1", "reason": "exit"})

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{sess.ID}, revoked)
}

const (
	lkBoot = "6c2f1a3e-0b7d-4c55-9a51-3d8f00a1b2c3"
	lkFin  = "example-finance"
	lkOps  = "example-ops"
)

var (
	lkHookProc = access.ProcessToken{PID: 500, Version: 5000, Boot: lkBoot}
	lkAgent    = access.ProcessToken{PID: 200, Version: 2000, Boot: lkBoot}
	lkShortID  = regexp.MustCompile(`R-[0-9A-Z]{4}`)
)

// lockHooks is a hook server with example-finance locked (peer_auth.locked),
// the approval service serve wires in, and a fake clock. Requests carry the
// owner's socket View for the hook's process chain (the `seam hook` process
// under the agent), with the leases live on it -- what peerHandler builds --
// unless view is nil (peer_auth off).
type lockHooks struct {
	t      *testing.T
	db     *sql.DB
	svc    *approval.Service
	policy *access.Policy
	url    string

	mu   sync.Mutex
	now  time.Time
	view func() (access.View, bool)
}

func newLockHooks(t *testing.T, approvals bool) *lockHooks {
	t.Helper()
	return newLockHooksWith(t, approvals, nil)
}

// newLockHooksWith is newLockHooks with configure, when non-nil, adjusting
// the handler's Config (the files layer, plan capture) before it is built.
func newLockHooksWith(t *testing.T, approvals bool, configure func(db *sql.DB, cfg *Config)) *lockHooks {
	t.Helper()
	ctx := context.Background()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, store.AdoptLocalHost(ctx, db, localHostName))
	require.NoError(t, store.AddRepoMapping(ctx, db, "/work/finance", lkFin))
	require.NoError(t, store.AddRepoMapping(ctx, db, "/work/ops", lkOps))
	for _, slug := range []string{lkFin, lkOps} {
		_, err := store.EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
	}
	require.NoError(t, store.SetProjectIsolation(ctx, db, lkFin, core.IsolationConfidential))
	insertMemory(t, db, "01F", "constraint", "fin-secret-rule", "the finance team's secret rule", lkFin)
	insertMemory(t, db, "01O", "constraint", "ops-rule", "the ops team's rule", lkOps)
	insertMemory(t, db, "01P", "gotcha", "ops-deploy-race", "ops deploy pipeline health check race", lkOps)
	policy, err := access.New(config.PeerAuth{Owner: "owner", Locked: []string{lkFin}}, func(n string) (access.Account, error) {
		if n == "owner" {
			return access.Account{UID: 501, Name: "owner"}, nil
		}
		return access.Account{}, fmt.Errorf("no account %s", n)
	})
	require.NoError(t, err)
	e := &lockHooks{t: t, db: db, policy: policy, now: time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)}
	rec := events.NewRecorder(db)
	e.svc = approval.New(approval.Config{DB: db, Events: rec, Host: localHostName, LeaseCap: 8 * time.Hour, Now: e.clock})
	cfg := Config{
		DB: db, Retrieve: retrieve.New(db, nil, config.Budgets{MaxBriefingTokens: 1500, RecallBudgetTokens: 1000}, nil),
		Events: rec, APIKey: testKey, LocalHost: localHostName, Now: e.clock,
	}
	if approvals {
		cfg.Approvals, cfg.RevokeSessionLeases = e.svc, e.svc.RevokeSession
	}
	if configure != nil {
		configure(db, &cfg)
	}
	h := NewHandler(cfg)
	mux := http.NewServeMux()
	h.Register(mux)
	e.view = e.socketView
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		view := e.view
		e.mu.Unlock()
		if view != nil {
			if v, ok := view(); ok {
				r = r.WithContext(access.WithView(r.Context(), v))
			}
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	e.url = ts.URL
	return e
}

func (e *lockHooks) clock() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.now
}

func (e *lockHooks) set(t time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.now = t
}

func (e *lockHooks) socketView() (access.View, bool) {
	chain := []access.ProcessToken{lkHookProc, lkAgent}
	v, ok := e.policy.PeerChainView(501, chain)
	require.True(e.t, ok)
	now := e.clock()
	rows, err := store.ActiveAccessLeasesFor(context.Background(), e.db, []string{lkHookProc.Key(), lkAgent.Key()}, now)
	require.NoError(e.t, err)
	return v.WithLeases(now, store.AccessLeases(rows)), true
}

// hook posts to a hook endpoint as the agent's `seam hook` would, returning
// the additional context ("" for an ack).
func (e *lockHooks) hook(endpoint, client string, body map[string]any) string {
	e.t.Helper()
	q := url.Values{"host": {localHostName}, "agent_process": {lkAgent.AgentID()}}
	if client != "" {
		q.Set("client", client)
	}
	if cwd, _ := body["cwd"].(string); cwd != "" {
		q.Set("repo_root", cwd)
		q.Set("main_root", cwd)
	}
	resp, out := post(e.t, e.url+"/api/hooks/"+endpoint+"?"+q.Encode(), testKey, body)
	require.Equal(e.t, http.StatusOK, resp.StatusCode)
	if hso, ok := out["hookSpecificOutput"].(map[string]any); ok {
		s, _ := hso["additionalContext"].(string)
		return s
	}
	return ""
}

func (e *lockHooks) pending() []store.AccessApproval {
	e.t.Helper()
	rows, err := store.PendingAccessApprovals(context.Background(), e.db, e.clock())
	require.NoError(e.t, err)
	return rows
}

// grant records an approved lease for sessionID on project, ending at the
// clock plus length (the approve core's last step).
func (e *lockHooks) grant(sessionID, project string, length time.Duration) store.AccessLease {
	e.t.Helper()
	ctx := context.Background()
	a, err := store.CreateAccessApproval(ctx, e.db, store.AccessRequest{
		Project: project, Kind: store.AccessKindRequest, Lease: length, TTL: 15 * time.Minute,
		PeerUID: 501, PeerPID: lkAgent.PID, ProcessKey: lkAgent.Key(), SessionID: sessionID,
	}, e.clock())
	require.NoError(e.t, err)
	lease, err := store.ApproveAccessFor(ctx, e.db, a.ID, a.Challenge, "test-owner", length, e.clock())
	require.NoError(e.t, err)
	return lease
}

// A SessionStart in a locked project's repository records the session there,
// metadata only (Q4), files the access request for it, and answers with the
// locked notice naming the request -- never the project's briefing. The other
// hooks keep the session metadata-only: a prompt recalls nothing from the
// project, and the end completes it with no findings.
func TestLockedSessionStart_RecordsTheSessionAndFilesTheRequest(t *testing.T) {
	e := newLockHooks(t, true)

	notice := e.hook("session-start", "", map[string]any{"session_id": "fin-1", "cwd": "/work/finance", "source": "startup"})
	require.Contains(t, notice, "Seamless: project "+lkFin+" is locked")
	short := lkShortID.FindString(notice)
	require.NotEmpty(t, short, notice)
	require.Contains(t, notice, "Remedy: access request "+short+" is waiting")
	require.Contains(t, notice, "`seam approve "+short+"`")
	require.Contains(t, notice, "This session is recorded in "+lkFin)
	require.Contains(t, notice, "briefing arrives with the first Seamless tool result after the approval")
	require.Contains(t, notice, "Seam session: cc/")
	require.NotContains(t, notice, "fin-secret-rule", "nothing from inside the locked project")
	require.NotContains(t, notice, "sealed", "the lock is not called sealed")

	sess := requireAmbientSession(t, e.db, ClientClaudeCode, "fin-1")
	require.Equal(t, lkFin, sess.ProjectSlug, "the session is recorded in the locked project")
	procs, err := store.SessionAgentProcesses(context.Background(), e.db, []string{sess.ID})
	require.NoError(t, err)
	require.Equal(t, lkAgent.AgentID(), procs[sess.ID].AgentProcess, "stamped, so the agent's MCP connection finds it")
	pending := e.pending()
	require.Len(t, pending, 1)
	require.Equal(t, sess.ID, pending[0].SessionID)
	require.Equal(t, lkFin, pending[0].Project)

	// A resume keeps the session where it is and finds the same request.
	notice = e.hook("session-start", "", map[string]any{"session_id": "fin-1", "cwd": "/work/finance", "source": "resume"})
	require.Contains(t, notice, short)
	require.Equal(t, sess.ID, requireAmbientSession(t, e.db, ClientClaudeCode, "fin-1").ID)
	require.Len(t, e.pending(), 1)

	// A prompt recalls nothing from the locked project.
	ctxText := e.hook("user-prompt-submit", "", map[string]any{"session_id": "fin-1", "cwd": "/work/finance",
		"user_prompt": "what is the finance team secret rule"})
	require.NotContains(t, ctxText, "fin-secret-rule")

	// The end completes it, metadata only.
	e.hook("session-end", "", map[string]any{"session_id": "fin-1", "reason": "exit"})
	sess = requireAmbientSession(t, e.db, ClientClaudeCode, "fin-1")
	require.Equal(t, core.SessionCompleted, sess.Status)
	require.Empty(t, sess.Findings)
}

// A session already recorded in the locked project is never moved out of it
// by a start in another repository: that start records no session.
func TestLockedSessionStart_LockedSessionStaysPut(t *testing.T) {
	e := newLockHooks(t, true)
	e.hook("session-start", "", map[string]any{"session_id": "fin-1", "cwd": "/work/finance", "source": "startup"})
	brief := e.hook("session-start", "", map[string]any{"session_id": "fin-1", "cwd": "/work/ops", "source": "resume"})
	require.Contains(t, brief, "ops-rule", "the ops briefing")
	require.NotContains(t, brief, "Seam session:", "no session line: the locked session was not resumed here")
	require.Equal(t, lkFin, requireAmbientSession(t, e.db, ClientClaudeCode, "fin-1").ProjectSlug)
}

// With a lease, the repository is the agent's to work in: the briefing, not
// the notice, and no request.
func TestLockedSessionStart_LeasedGetsTheBriefing(t *testing.T) {
	e := newLockHooks(t, true)
	e.grant("", lkFin, time.Hour)
	brief := e.hook("session-start", "", map[string]any{"session_id": "fin-2", "cwd": "/work/finance", "source": "startup"})
	require.Contains(t, brief, "fin-secret-rule")
	require.NotContains(t, brief, "is locked")
	require.Empty(t, e.pending())
}

// The 6afda40 design is unchanged: the owner in a repository sealed to another
// OS account gets the sealed notice and no session.
func TestSealedToAccountSessionStart_KeepsItsNotice(t *testing.T) {
	e := newLockHooks(t, false)
	legacy, err := access.New(config.PeerAuth{Owner: "owner", Users: map[string]config.PeerUser{
		"_example_finance": {Sealed: []string{lkFin}},
	}}, func(n string) (access.Account, error) {
		switch n {
		case "owner":
			return access.Account{UID: 501, Name: "owner"}, nil
		case "_example_finance":
			return access.Account{UID: 470, Name: "_example_finance"}, nil
		}
		return access.Account{}, fmt.Errorf("no account %s", n)
	})
	require.NoError(t, err)
	e.mu.Lock()
	e.view = func() (access.View, bool) { return legacy.OwnerView(), true }
	e.mu.Unlock()
	notice := e.hook("session-start", "", map[string]any{"session_id": "fin-3", "cwd": "/work/finance", "source": "startup"})
	require.Equal(t, sealedNotice(lkFin), notice)
	_, ok, err := store.AmbientSessionByExternalIdentity(context.Background(), e.db, ClientClaudeCode.externalIdentity(), "fin-3")
	require.NoError(t, err)
	require.False(t, ok)
}

// Step 8 at the hooks, on a fake clock: a lease of the session's ending within
// 30 minutes puts the warning in the prompt injection and the briefing; at
// T-31m there is none; with no project locked (no approval service) nothing
// is looked up and nothing is said.
func TestLeaseExpiry_HookInjections(t *testing.T) {
	for _, on := range []bool{true, false} {
		t.Run(fmt.Sprintf("locked=%v", on), func(t *testing.T) {
			e := newLockHooks(t, on)
			t0 := e.clock()
			e.hook("session-start", "", map[string]any{"session_id": "ops-1", "cwd": "/work/ops", "source": "startup"})
			sess := requireAmbientSession(t, e.db, ClientClaudeCode, "ops-1")
			lease := e.grant(sess.ID, lkFin, time.Hour)
			prompt := map[string]any{"session_id": "ops-1", "cwd": "/work/ops", "user_prompt": "why does the ops deploy pipeline health check race"}

			e.set(t0.Add(29 * time.Minute)) // T-31m
			require.NotContains(t, e.hook("user-prompt-submit", "", prompt), "Access to")

			e.set(t0.Add(31 * time.Minute)) // T-29m
			want := "Access to " + lkFin + " ends at " + access.Clock(lease.ExpiresAt) + " (29 minutes left)"
			got := e.hook("user-prompt-submit", "", prompt)
			brief := e.hook("session-start", "", map[string]any{"session_id": "ops-1", "cwd": "/work/ops", "source": "resume"})
			if !on {
				require.NotContains(t, got, "Access to")
				require.NotContains(t, brief, "Access to")
				return
			}
			require.Contains(t, got, want)
			require.Contains(t, got, "`seam access request "+lkFin+"`")
			require.Contains(t, got, "ops-deploy-race", "the recall still follows")
			require.True(t, strings.HasPrefix(brief, "<seam-briefing>\nSeamless: "+want), brief)
			require.Contains(t, brief, "ops-rule")

			// T+1s: the lease has ended, and the warning with it.
			e.set(lease.ExpiresAt.Add(time.Second))
			require.NotContains(t, e.hook("user-prompt-submit", "", prompt), "Access to")
		})
	}
}

// A SessionStart whose agent process is a broad ancestor -- the hook run by
// hand from a tmux pane, so the process it names is the tmux server -- files
// nothing: the notice says why and what to do, and names no request.
func TestLockedSessionStart_ABroadAnchorFilesNothing(t *testing.T) {
	e := newLockHooksWith(t, true, func(db *sql.DB, cfg *Config) {
		svc := approval.New(approval.Config{DB: db, Events: cfg.Events, Host: localHostName, LeaseCap: 8 * time.Hour, Now: cfg.Now,
			Programs: func(t access.ProcessToken) string {
				if t == lkAgent {
					return "/opt/homebrew/Cellar/tmux/3.8/bin/tmux"
				}
				return ""
			}})
		cfg.Approvals = svc
	})
	notice := e.hook("session-start", "", map[string]any{"session_id": "fin-1", "cwd": "/work/finance", "source": "startup"})
	require.Contains(t, notice, "Seamless: project "+lkFin+" is locked: even the owner's own agents need the owner's "+
		"approval, and Seamless files no access request for this process: it is tmux, a terminal multiplexer, not an agent")
	require.Contains(t, notice, "\nRemedy: run the work from an agent session (Claude Code or Codex), which asks for itself")
	require.Empty(t, lkShortID.FindString(notice), "no request id: none was filed")
	require.Empty(t, e.pending())
}

// The notice offers Touch ID once a key is enrolled, and says where it
// applies: the approval dialog the agent's client shows.
func TestLockedSessionStart_OffersTouchIDWhenEnrolled(t *testing.T) {
	e := newLockHooks(t, true)
	ctx := context.Background()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	pub, err := key.PublicKey.Bytes()
	require.NoError(t, err)
	code, _, err := store.MintEnrollCode(ctx, e.db, store.ApproverTouchID, e.clock())
	require.NoError(t, err)
	d := sha256.Sum256(approval.TouchIDEnrollMessage(code, pub))
	sig, err := ecdsa.SignASN1(rand.Reader, key, d[:])
	require.NoError(t, err)
	_, err = e.svc.EnrollTouchID(ctx, "this Mac", code, pub, sig)
	require.NoError(t, err)

	notice := e.hook("session-start", "", map[string]any{"session_id": "fin-1", "cwd": "/work/finance", "source": "startup"})
	short := lkShortID.FindString(notice)
	require.NotEmpty(t, short, notice)
	require.Contains(t, notice, "Remedy: access request "+short+" is waiting -- the owner approves it with Touch ID in the "+
		"approval dialog (where this agent's client shows one), or in their own terminal with `seam approve "+short+"`;")
	require.NotContains(t, notice, "passkey")
}
