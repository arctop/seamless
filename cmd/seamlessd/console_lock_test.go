package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/console"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/plans"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// The console under a locked project, wired as serve wires it (a view pool,
// the console's base handle, peerHandler with the store's leases): a leased
// agent tree's `seam plan show` -- GET /console/plans/<slug> as JSON over the
// socket -- reads the locked project, while an unleased tree on the socket
// and the TCP key are refused.
func TestConsoleLock_LeasedTreeReadsThroughTheConsole(t *testing.T) {
	ctx := context.Background()
	const (
		boot   = "6c2f1a3e-0b7d-4c55-9a51-3d8f00a1b2c3"
		secret = "LEDGER-PLAN-SECRET"
		plan   = "ledger-plan"
	)
	agent := access.ProcessToken{PID: 200, Version: 2000, Boot: boot}
	cli := access.ProcessToken{PID: 320, Version: 3200, Boot: boot}
	stranger := access.ProcessToken{PID: 400, Version: 4000, Boot: boot}

	dir := t.TempDir()
	path := filepath.Join(dir, "seam.db")
	db, err := storetest.OpenFresh(t, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	for _, slug := range []string{"example-finance", "example-ops"} {
		_, err := store.EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
	}
	require.NoError(t, store.SetProjectIsolation(ctx, db, "example-finance", core.IsolationConfidential))
	mgr, err := files.NewManager(filepath.Join(dir, "data"), db, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })
	noteID, err := core.NewID()
	require.NoError(t, err)
	now := time.Now().UTC()
	_, err = mgr.WriteNote(ctx, core.Note{ID: noteID, Slug: "ledger-narrative", Title: "Ledger plan", Project: "example-finance",
		Body: "# Ledger\n\n" + secret, Tags: []string{plans.SlugTag(plan), "created-by:agent"}, Created: now, Updated: now})
	require.NoError(t, err)

	cfg := config.Defaults()
	cfg.PeerAuth = config.PeerAuth{Enabled: true, Owner: "owner", ForceLocked: []string{"example-finance"}}
	policy, err := access.New(cfg.PeerAuth, lockOwnerResolve)
	require.NoError(t, err)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rec := events.NewRecorder(db)
	svc, err := newApprovals(cfg, db, rec, logger)
	require.NoError(t, err)

	// The agent's tree holds a lease, approved on the terminal path.
	a, err := store.CreateAccessApproval(ctx, db, store.AccessRequest{Project: "example-finance", Kind: store.AccessKindRequest,
		Lease: time.Hour, TTL: time.Minute, PeerUID: 501, PeerPID: agent.PID, ProcessKey: agent.Key()}, now)
	require.NoError(t, err)
	_, err = store.ApproveAccessFor(ctx, db, a.ID, a.Challenge, "daemon-account", time.Hour, now)
	require.NoError(t, err)

	views, err := store.OpenViews(path, policy.OwnerLocked())
	require.NoError(t, err)
	t.Cleanup(func() { _ = views.Close() })
	base, err := views.For(nil)
	require.NoError(t, err)
	cs, err := console.New(console.Config{
		DB: base, WriteDB: db, Files: mgr, Events: rec.ReadingFrom(base), APIKey: "k", DataDir: filepath.Join(dir, "data"),
		Approvals: svc, Views: views, Logger: logger,
	})
	require.NoError(t, err)
	mux := http.NewServeMux()
	cs.Register(mux)

	serveAs := func(chain ...access.ProcessToken) *httptest.Server {
		hops := make([]access.ChainHop, len(chain))
		for i, c := range chain {
			hops[i] = access.ChainHop{Token: c}
		}
		cred := peerCred{uid: 501, chain: func() ([]access.ChainHop, error) { return hops, nil }}
		h := peerHandler(policy, storeLeases(db), logger, mux)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), peerCredKey{}, cred)))
		}))
		t.Cleanup(func() { closeServer(srv) })
		return srv
	}
	get := func(base, key string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, base+"/console/plans/"+url.PathEscape(plan)+"?format=json", nil)
		require.NoError(t, err)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return resp.StatusCode, string(body)
	}

	status, body := get(serveAs(cli, agent).URL, "")
	require.Equal(t, http.StatusOK, status, body)
	require.Contains(t, body, secret, "the leased tree's seam plan show reads the locked project")

	status, body = get(serveAs(stranger).URL, "")
	require.Equal(t, http.StatusForbidden, status, "another tree on the socket is refused")
	require.Contains(t, body, "is locked", "and told the lock, not not-found")
	require.NotContains(t, body, secret)

	tcp := httptest.NewServer(ownerHandler(policy, mux))
	t.Cleanup(func() { closeServer(tcp) })
	status, body = get(tcp.URL, "k")
	require.Equal(t, http.StatusForbidden, status, "the TCP key never reaches a locked project")
	require.Contains(t, body, "the TCP connection and its bearer key never reach a locked project")
	require.NotContains(t, body, secret)
	require.LessOrEqual(t, views.Open(), 2, "the base handle and the leased one")
}

// console-open --unlock's core: the project must be locked, --for is the whole
// console lease when absent and refused when present but not a duration up to
// it, and the code approves only the unlock its browser began, once, for its
// own project, before its deadline. A refused code changes nothing.
func TestConsoleOpenUnlock_ApproveCode(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	cfg := config.Defaults()
	cfg.PeerAuth = config.PeerAuth{Enabled: true, Owner: "owner", ForceLocked: []string{"example-finance", "example-legal"}}
	svc, err := newApprovals(cfg, db, events.NewRecorder(db), slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	var out strings.Builder
	tty := unlockTerminal{in: strings.NewReader(""), out: &out}
	approve := func(project, code, forFlag string) error {
		return approveConsoleUnlock(ctx, cfg, db, svc, tty, project, code, forFlag)
	}
	browser, other := store.HashConsoleToken("browser"), store.HashConsoleToken("other")
	begin := func(project, tokenHash string) (store.ConsoleUnlock, string) {
		u, code, err := store.BeginTerminalUnlock(ctx, db, project, tokenHash, svc.ConsoleLeaseLength(), 10*time.Minute, time.Now())
		require.NoError(t, err)
		return u, code
	}
	fin, finCode := begin("example-finance", browser)
	legal, legalCode := begin("example-legal", other)

	require.ErrorContains(t, approve("example-ops", finCode, ""), "not locked")
	for _, bad := range []string{"soon", "0s", "30s", "4h"} {
		require.ErrorContains(t, approve("example-finance", finCode, bad), "invalid --for", bad)
	}
	err = approve("example-finance", "nope", "")
	require.ErrorIs(t, err, store.ErrInvalidConsoleUnlock)
	require.EqualError(t, err, `"nope" is not an unlock code: the unlock page shows one as XXXX-XXXX, 8 letters and digits. Nothing changed.`)
	for name, c := range map[string]struct{ project, code string }{
		"a mistyped code":        {"example-finance", "ZZZZ-ZZZZ"},
		"another project's code": {"example-finance", legalCode},
		"this code, elsewhere":   {"example-legal", finCode},
	} {
		err := approve(c.project, c.code, "")
		require.ErrorIs(t, err, store.ErrConsoleUnlockNotFound, name)
		require.ErrorContains(t, err, "no unlock of "+c.project+" is waiting for code ", name)
		require.ErrorContains(t, err, "Nothing changed. Start again on "+consoleBase(cfg)+"/console/unlock/"+c.project+".", name)
	}
	require.Empty(t, out.String(), "a refusal says nothing on stdout")
	none, err := store.AllLiveConsoleUnlocks(ctx, db, time.Now())
	require.NoError(t, err)
	require.Empty(t, none, "no refused code opened anything")

	require.NoError(t, approve("example-finance", strings.ToLower(finCode), ""))
	require.Regexp(t, `^Unlocked example-finance for 3 hours, until \d\d:\d\d, in the browser that showed code `+finCode+`\.\n$`, out.String())
	live, err := store.LiveConsoleUnlocks(ctx, db, browser, time.Now())
	require.NoError(t, err)
	require.Len(t, live, 1)
	require.Equal(t, fin.ID, live[0].ID)
	require.Equal(t, 3*time.Hour, live[0].Lease, "absent --for is the whole console lease")
	require.Equal(t, store.ConsoleUnlockByDaemon, live[0].Credential)
	require.ErrorIs(t, approve("example-finance", finCode, ""), store.ErrConsoleUnlockNotFound, "a code works once")

	out.Reset()
	require.NoError(t, approve("example-legal", legalCode, "90m"))
	require.Contains(t, out.String(), "Unlocked example-legal for 90 minutes")
	live, err = store.LiveConsoleUnlocks(ctx, db, other, time.Now())
	require.NoError(t, err)
	require.Len(t, live, 1)
	require.Equal(t, legal.ID, live[0].ID)
	require.Equal(t, 90*time.Minute, live[0].Lease, "--for moved to approval time")

	// Expired before it was approved.
	_, late := begin("example-finance", browser)
	_, err = db.Exec(`UPDATE console_unlocks SET pending_until = ? WHERE status = 'pending'`, core.FormatTime(time.Now().Add(-time.Second)))
	require.NoError(t, err)
	err = approve("example-finance", late, "")
	require.ErrorIs(t, err, store.ErrConsoleUnlockNotFound)
	require.ErrorContains(t, err, "expired (a code lasts 10 minutes)")

	// It runs only as the daemon's account, with a project locked.
	off := config.Defaults()
	require.ErrorContains(t, runConsoleUnlock(off, "example-finance", finCode, "", ""), "peer_auth is off")
}

// Two terminals approving one code at once: exactly one unlock goes live.
func TestConsoleOpenUnlock_RacingApprovals(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	cfg := config.Defaults()
	cfg.PeerAuth = config.PeerAuth{Enabled: true, Owner: "owner", ForceLocked: []string{"example-finance"}}
	svc, err := newApprovals(cfg, db, events.NewRecorder(db), slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	_, code, err := store.BeginTerminalUnlock(ctx, db, "example-finance", store.HashConsoleToken("browser"), 3*time.Hour, 10*time.Minute, time.Now())
	require.NoError(t, err)
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- approveConsoleUnlock(ctx, cfg, db, svc, unlockTerminal{out: io.Discard}, "example-finance", code, "")
		}()
	}
	wg.Wait()
	close(errs)
	wins := 0
	for err := range errs {
		if err == nil {
			wins++
			continue
		}
		require.ErrorIs(t, err, store.ErrConsoleUnlockNotFound)
	}
	require.Equal(t, 1, wins)
	live, err := store.AllLiveConsoleUnlocks(ctx, db, time.Now())
	require.NoError(t, err)
	require.Len(t, live, 1)
}

// The security review's first attack, ported (it was
// TestPoC_StolenUnlockLinkOpensLockedProjectToAgent): an agent running as the
// owner reads everything the owner's terminal shows -- the command with its
// code, and what it printed -- and holds the console's TCP key in its own
// cookie jar. On the real daemon wiring it gets no live unlock: what crossed
// the terminal names the owner's browser's row, and approving it opened only
// that browser.
func TestConsoleUnlock_TerminalScrollbackOpensNothing(t *testing.T) {
	ctx := context.Background()
	d := cmNewDaemon(t, access.Account{Name: cmOwnerName, UID: cmUIDOwner}, nil)
	owner := d.newBrowser(t, "the owner's browser")
	status, body, _ := owner.do(t, http.MethodPost, "/console/unlock/"+cmFinance+"/terminal", url.Values{}, true)
	require.Equal(t, http.StatusOK, status, body)
	var begun struct {
		ID, Code, Command string
	}
	require.NoError(t, json.Unmarshal([]byte(body), &begun))
	require.NotEmpty(t, d.unlockCookie(t, owner))

	// The owner runs the command; the agent reads the whole terminal.
	var terminal strings.Builder
	terminal.WriteString(begun.Command + "\n")
	require.NoError(t, approveConsoleUnlock(ctx, d.cfg, d.db, d.svc, unlockTerminal{out: &terminal}, cmFinance, begun.Code, ""))
	scrollback := terminal.String()
	require.Contains(t, scrollback, begun.Code)
	require.NotContains(t, scrollback, d.unlockCookie(t, owner), "the cookie never crosses the terminal")
	require.NotContains(t, scrollback, "/console/unlock?t=")

	agent := d.newBrowser(t, "the agent's cookie jar, built from the key in console.yaml")
	for _, target := range []string{
		"/console/unlock/" + cmFinance + "/terminal?id=" + url.QueryEscape(begun.ID), "/console/projects/" + cmFinance,
	} {
		status, body, _ := agent.do(t, http.MethodGet, target, nil, true)
		require.Less(t, status, 500, target)
		require.NotEqual(t, http.StatusOK, status, "%s: %s", target, firstN(body, 200))
	}
	status, body, _ = agent.do(t, http.MethodGet, "/console/projects/"+cmFinance, nil, true)
	require.Equal(t, http.StatusForbidden, status)
	require.Contains(t, body, `"locked": true`)
	require.Empty(t, d.unlockCookie(t, agent), "the agent's jar holds no unlock cookie")
	// Its own begin draws its own code, which nobody approved.
	status, body, _ = agent.do(t, http.MethodPost, "/console/unlock/"+cmFinance+"/terminal", url.Values{"code": {begun.Code}}, true)
	require.Equal(t, http.StatusOK, status, body)
	require.NotContains(t, body, begun.Code)
	live, err := store.LiveConsoleUnlocks(ctx, d.db, store.HashConsoleToken(d.unlockCookie(t, agent)), time.Now())
	require.NoError(t, err)
	require.Empty(t, live)
	// Replaying the approval opens nothing more.
	require.ErrorIs(t, approveConsoleUnlock(ctx, d.cfg, d.db, d.svc, unlockTerminal{out: io.Discard}, cmFinance, begun.Code, ""),
		store.ErrConsoleUnlockNotFound)

	status, body, _ = owner.do(t, http.MethodGet, "/console/projects/"+cmFinance, nil, true)
	require.Equal(t, http.StatusOK, status, "the owner's browser reads it: %s", firstN(body, 200))
	all, err := store.AllLiveConsoleUnlocks(ctx, d.db, time.Now())
	require.NoError(t, err)
	require.Len(t, all, 1)
}

// The security review's second attack, ported (it was
// TestPoC_UnlockLinkClaimedWithForgedCookie): a plain HTTP client with the key
// forges the console cookie (sha256 of the prefix and the key, no login) and
// plants a seamless_unlock value it knows. With the code the owner's page
// shows it gets no live unlock, under its planted value or any cookie the
// daemon hands it, and the approval lands on the owner's browser alone.
func TestConsoleUnlock_ForgedCookieCannotTakeTheApproval(t *testing.T) {
	ctx := context.Background()
	d := cmNewDaemon(t, access.Account{Name: cmOwnerName, UID: cmUIDOwner}, nil)
	owner := d.newBrowser(t, "the owner's browser")
	status, body, _ := owner.do(t, http.MethodPost, "/console/unlock/"+cmFinance+"/terminal", url.Values{}, true)
	require.Equal(t, http.StatusOK, status, body)
	var begun struct{ ID, Code string }
	require.NoError(t, json.Unmarshal([]byte(body), &begun))

	sum := sha256.Sum256([]byte("seamless-console\x00" + cmKey))
	forgedConsole := hex.EncodeToString(sum[:])
	planted, err := store.NewConsoleToken()
	require.NoError(t, err)
	forged := func(method, target string, unlock string) (*http.Response, string) {
		t.Helper()
		var rd io.Reader
		if method == http.MethodPost {
			rd = strings.NewReader(url.Values{"code": {begun.Code}, "id": {begun.ID}}.Encode())
		}
		req, err := http.NewRequest(method, d.owner.URL+target, rd)
		require.NoError(t, err)
		req.Header.Set("Cookie", "seamless_console="+forgedConsole+"; seamless_unlock="+unlock)
		req.Header.Set("Accept", "application/json")
		if method == http.MethodPost {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("Origin", d.owner.URL)
		}
		resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
		require.NoError(t, err)
		raw, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		return resp, string(raw)
	}
	resp, _ := forged(http.MethodGet, "/console/unlock/"+cmFinance+"/terminal?id="+url.QueryEscape(begun.ID), planted)
	require.Equal(t, http.StatusNotFound, resp.StatusCode, "the forged client cannot read the owner's row")
	resp, body = forged(http.MethodPost, "/console/unlock/"+cmFinance+"/terminal", planted)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var issued string
	for _, c := range resp.Cookies() {
		if c.Name == "seamless_unlock" {
			issued = c.Value
		}
	}
	require.NotEmpty(t, issued)
	require.NotEqual(t, planted, issued, "a planted cookie naming no live unlock is never adopted")

	require.NoError(t, approveConsoleUnlock(ctx, d.cfg, d.db, d.svc, unlockTerminal{out: io.Discard}, cmFinance, begun.Code, ""))
	for _, c := range []string{planted, issued} {
		live, err := store.LiveConsoleUnlocks(ctx, d.db, store.HashConsoleToken(c), time.Now())
		require.NoError(t, err)
		require.Empty(t, live, "the forged client holds no unlock")
		resp, body := forged(http.MethodGet, "/console/projects/"+cmFinance, c)
		require.Equal(t, http.StatusForbidden, resp.StatusCode)
		require.Contains(t, body, `"locked": true`)
	}
	live, err := store.LiveConsoleUnlocks(ctx, d.db, store.HashConsoleToken(d.unlockCookie(t, owner)), time.Now())
	require.NoError(t, err)
	require.Len(t, live, 1, "the approval landed on the browser that began it")
}
