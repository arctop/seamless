package main

// Per-account reach for restricted accounts (peer_auth.restricted's exclude
// and only; plan agent-accounts-and-releases, step B; memory
// restricted-account-reach-decisions) on the restricted matrix's daemon
// (newRADaemonFor): the real route table, services and peer handler, which
// draws each account's reach from the projects table per request. One account
// per shape:
//
//	bare      raAccount    every open, unlocked project
//	exclude   rrExcl       exclude [raDocs, rrVault]
//	multi     rrMulti      only [raOpen, raDocs]
//	desk      rrDesk       only [raConf]              (confidential, alone)
//	mixed     rrMixed      only [raConf, raOpen]      (the fenced one is dropped)
//	locked    rrLockd      only [raLockedOpen]        (a lock closes it)
//	sealed    rrSeal       only [raSealed]            (sealed, alone)

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

const (
	rrVault = "example-vault" // open: a third open project
	rrOther = "example-other" // confidential: one no account names

	rrExcl  = "_example_excl"
	rrMulti = "_example_multi"
	rrDesk  = "_example_desk"
	rrMixed = "_example_mixed"
	rrLockd = "_example_lockd"
	rrSeal  = "_example_seal"
)

var rrUIDs = map[string]uint32{
	raAccount: raUIDAgent, rrExcl: 520, rrMulti: 521, rrDesk: 522, rrMixed: 523, rrLockd: 524, rrSeal: 525,
}

func init() {
	for name, uid := range rrUIDs {
		if _, ok := raAccounts[name]; !ok {
			raAccounts[name] = access.Account{UID: uid, Name: name, Home: "/Users/" + name}
		}
	}
}

func rrPolicy() config.PeerAuth {
	cfg := raPolicy()
	cfg.Restricted = []config.RestrictedAccount{
		{Account: raAccount},
		{Account: rrExcl, Exclude: []string{raDocs, rrVault}},
		{Account: rrMulti, Only: []string{raOpen, raDocs}},
		{Account: rrDesk, Only: []string{raConf}},
		{Account: rrMixed, Only: []string{raConf, raOpen}},
		{Account: rrLockd, Only: []string{raLockedOpen}},
		{Account: rrSeal, Only: []string{raSealed}},
	}
	return cfg
}

// rrTargets is every project the matrix probes, plus the global scope ("").
var rrTargets = []string{raOpen, raDocs, rrVault, raConf, rrOther, raSealed, raLocked, raLockedOpen, raNone, ""}

// rrReach is the matrix: each account's reach, read and write. Global is
// readable by every account but the sealed one (whose binding the sealed
// fence closes to global) and writable by none.
var rrReach = map[string][]string{
	raAccount: {raOpen, raDocs, rrVault},
	rrExcl:    {raOpen},
	rrMulti:   {raOpen, raDocs},
	rrDesk:    {raConf},
	rrMixed:   {raOpen},
	rrLockd:   {},
	rrSeal:    {raSealed},
}

func rrToken(project string) string {
	if project == "" {
		return "rrqglobalz"
	}
	return "rrq" + strings.ReplaceAll(project, "-", "") + "z"
}

func rrName(project string) string {
	if project == "" {
		return "rr-global"
	}
	return "rr-" + project
}

type rrDaemon struct {
	*raDaemon
	ids   map[string]string // planted memory ids, by project
	socks map[string]*httptest.Server
}

func newRRDaemon(t *testing.T) *rrDaemon {
	t.Helper()
	d := &rrDaemon{raDaemon: newRADaemonFor(t, rrPolicy()), ids: map[string]string{}, socks: map[string]*httptest.Server{}}
	ctx := context.Background()
	for slug, iso := range map[string]core.Isolation{rrVault: core.IsolationOpen, rrOther: core.IsolationConfidential} {
		_, err := store.EnsureProject(ctx, d.db, slug, slug)
		require.NoError(t, err)
		require.NoError(t, store.SetProjectIsolation(ctx, d.db, slug, iso))
	}
	require.NoError(t, store.AddRepoMapping(ctx, d.db, "/Users/"+rrDesk+"/repos/desk", raConf))
	now := time.Now().UTC()
	for _, p := range rrTargets {
		if p == raNone {
			continue
		}
		id, err := core.NewID()
		require.NoError(t, err)
		_, err = d.mgr.WriteMemory(ctx, core.Memory{ID: id, Kind: core.KindGotcha, Name: rrName(p),
			Description: "planted " + rrToken(p), Project: p, Body: "body " + rrToken(p),
			Created: now, Updated: now, ValidFrom: now})
		require.NoError(t, err)
		d.ids[p] = id
	}
	for name, uid := range rrUIDs {
		d.socks[name] = d.socketFor(t, uid)
	}
	return d
}

// client connects as account; an account whose reach holds a fenced project
// binds to it first, as a session native to that project does.
func (d *rrDaemon) client(t *testing.T, account string) *mcpclient.Client {
	t.Helper()
	cli := paClient(t, d.socks[account].URL, "")
	for _, p := range rrReach[account] {
		if p == raConf || p == raSealed {
			paCallOK(t, cli, "session_start", map[string]any{"project": p, "name": "rr-" + strings.TrimPrefix(account, "_")})
		}
	}
	return cli
}

func rrRefusal(project, account string) string {
	return "project " + project + " is not reachable from account " + account
}

var rrSlugRE = regexp.MustCompile(`"slug":"([^"]+)"`)

func TestPeerAuthMatrix_RestrictedReach(t *testing.T) {
	d := newRRDaemon(t)
	own := paClient(t, d.owner.URL, raKey)

	t.Run("the matrix: reach per shape, read and write", func(t *testing.T) {
		for account, reach := range rrReach {
			cli := d.client(t, account)
			for _, p := range rrTargets {
				arg := p
				if arg == "" {
					arg = "global"
				}
				in := slices.Contains(reach, p)
				text, isErr := paCall(t, cli, "memory_read", map[string]any{"project": arg, "name": rrName(p)})
				switch {
				case in || p == "" && account != rrSeal:
					require.False(t, isErr, "%s reads %s: %s", account, arg, text)
					require.Contains(t, text, rrToken(p))
				default:
					require.True(t, isErr, "%s reads %s: %s", account, arg, text)
					require.NotContains(t, text, rrToken(p))
					if p != "" {
						require.Contains(t, text, rrRefusal(p, account))
					}
				}
				text, isErr = paCall(t, cli, "memory_write", map[string]any{"project": arg, "name": "rrw-" + strings.TrimPrefix(account, "_"),
					"kind": "gotcha", "description": "written by " + account, "body": "b"})
				switch {
				case in:
					require.False(t, isErr, "%s writes %s: %s", account, arg, text)
				case p == "":
					require.True(t, isErr, "%s writes global: %s", account, text)
					if account == raAccount {
						require.Contains(t, text, "the global scope is read-only for account "+account)
					} else {
						require.Contains(t, text, "account "+account+" does not write the global scope")
					}
				default:
					require.True(t, isErr, "%s writes %s: %s", account, arg, text)
					require.Contains(t, text, rrRefusal(p, account))
				}
			}
			// project_list lists exactly the reach.
			raw, err := json.Marshal(paCallOK(t, cli, "project_list", map[string]any{}))
			require.NoError(t, err)
			var listed []string
			for _, m := range rrSlugRE.FindAllStringSubmatch(string(raw), -1) {
				listed = append(listed, m[1])
			}
			slices.Sort(listed)
			want := slices.Clone(reach)
			slices.Sort(want)
			require.Equal(t, want, append([]string{}, listed...), "%s's project_list", account)
			// recall finds nothing planted outside the reach.
			for _, p := range rrTargets {
				if p == "" || slices.Contains(reach, p) {
					continue
				}
				text, _ := paCall(t, cli, "recall", map[string]any{"query": rrToken(p)})
				require.NotContains(t, text, rrToken(p), "%s recalls %s", account, p)
			}
		}
		// The rows must have been the shapes' doing, not the account list's.
		require.ElementsMatch(t, slices.Collect(maps.Keys(rrUIDs)), slices.Collect(maps.Keys(rrReach)))
	})

	t.Run("a shaped refusal reads the same for excluded, closed and unregistered projects", func(t *testing.T) {
		cli := d.client(t, rrExcl)
		var texts []string
		for _, p := range []string{raDocs, rrVault, raConf, raSealed, raLocked, raLockedOpen, raNone} {
			text, isErr := paCall(t, cli, "memory_read", map[string]any{"project": p, "name": rrName(p)})
			require.True(t, isErr, text)
			texts = append(texts, strings.ReplaceAll(text, p, "<p>"))
		}
		for _, other := range texts[1:] {
			require.Equal(t, texts[0], other)
		}
		require.Contains(t, texts[0], "the owner gives this account only the projects project_list shows")
		stateWord := regexp.MustCompile(`\b(open|confidential|sealed|locked)\b`)
		require.False(t, stateWord.MatchString(strings.ReplaceAll(texts[0], "<p>", "")), texts[0])

		// An excluded project's id answers exactly as a missing one.
		missing, err := core.NewID()
		require.NoError(t, err)
		miss, isErr := paCall(t, cli, "memory_read", map[string]any{"id": missing})
		require.True(t, isErr)
		text, isErr := paCall(t, cli, "memory_read", map[string]any{"id": d.ids[raDocs]})
		require.True(t, isErr, text)
		require.Equal(t, strings.ReplaceAll(miss, missing, d.ids[raDocs]), text)

		// The bare account keeps today's text.
		bare := d.client(t, raAccount)
		text, _ = paCall(t, bare, "memory_read", map[string]any{"project": raConf, "name": rrName(raConf)})
		require.Contains(t, text, "a restricted account reaches open projects that are not locked")
	})

	t.Run("the desk: its one confidential project, its records, its proposals", func(t *testing.T) {
		desk := d.client(t, rrDesk)
		w := paCallOK(t, desk, "memory_write", map[string]any{"project": raConf, "name": "desk-fact", "kind": "gotcha",
			"description": "desk fact", "body": "b"})
		require.Equal(t, raConf, w["project"], "%v", w)
		got := paCallOK(t, desk, "memory_read", map[string]any{"project": raConf, "name": "desk-fact"})
		require.Equal(t, rrDesk, got["account"], "a desk write carries its label")
		p := paCallOK(t, desk, "memory_write", map[string]any{"project": raConf, "name": "desk-rule", "kind": "constraint",
			"description": "desk rule", "body": "always"})
		require.Equal(t, "proposed", p["status"], "a binding rule waits for the owner, in the desk's project too: %v", p)
		require.Equal(t, rrDesk, p["proposed_by"])
		// No requests, and the answer is the shaped one.
		text, isErr := paCall(t, desk, "access_request", map[string]any{"project": rrOther, "reason": "please"})
		require.True(t, isErr, text)
		require.Contains(t, text, "account "+rrDesk+" files no access requests: the owner gives this account only the projects project_list shows")
		require.Zero(t, raCount(t, d.db, `SELECT COUNT(*) FROM access_approvals`))

		// Its SessionStart hook: recorded in the clone mapped to its project,
		// a shaped notice anywhere else.
		start := func(sessionID, cwd string) string {
			q := url.Values{"host": {paHost}, "agent_process": {"7100.71000"}, "repo_root": {cwd}, "main_root": {cwd}}
			return paHook(t, d.socks[rrDesk].URL+"/api/hooks/session-start?"+q.Encode(), "", "", map[string]any{
				"session_id": sessionID, "cwd": cwd, "source": "startup", "hook_event_name": "SessionStart"})
		}
		outside := start("rr-desk-outside", "/work/open")
		require.Contains(t, outside, "It works only in the projects the owner gives it (project_list shows them)")
		require.NotContains(t, outside, raOpen)
		require.Zero(t, raCount(t, d.db, `SELECT COUNT(*) FROM sessions WHERE claude_session_id LIKE ?`, "%rr-desk-outside%"))
		inside := start("rr-desk-inside", "/Users/"+rrDesk+"/repos/desk")
		require.Contains(t, inside, "seam-briefing", inside)
		var project, account string
		require.NoError(t, d.db.QueryRow(`SELECT project_slug, account FROM sessions WHERE claude_session_id LIKE ?`,
			"%rr-desk-inside%").Scan(&project, &account))
		require.Equal(t, raConf, project)
		require.Equal(t, rrDesk, account)

		// The bare account's notice is today's, word for word.
		q := url.Values{"host": {paHost}, "agent_process": {"7200.72000"}, "repo_root": {"/w"}, "main_root": {"/w"}}
		bareNotice := paHook(t, d.socks[raAccount].URL+"/api/hooks/session-start?"+q.Encode(), "", "", map[string]any{
			"session_id": "rr-bare-outside", "cwd": "/Users/" + raAccount + "/repos/conf", "source": "startup", "hook_event_name": "SessionStart"})
		require.Contains(t, bareNotice, "A restricted account works only in the owner's open projects")
	})

	t.Run("the owner reaches everything as before, under every shape", func(t *testing.T) {
		for _, p := range []string{raOpen, raDocs, rrVault, raLockedOpen} {
			if p == raLockedOpen {
				text, isErr := paCall(t, own, "memory_read", map[string]any{"project": p, "name": rrName(p)})
				require.True(t, isErr, "a lock still closes it to the key: %s", text)
				continue
			}
			got := paCallOK(t, own, "memory_read", map[string]any{"project": p, "name": rrName(p)})
			require.Contains(t, fmt.Sprint(got), rrToken(p))
		}
		paCallOK(t, own, "session_start", map[string]any{"project": raConf, "name": "rr-owner-desk"})
		got := paCallOK(t, own, "memory_read", map[string]any{"project": raConf, "name": rrName(raConf)})
		require.Contains(t, fmt.Sprint(got), rrToken(raConf), "the desk's project is not the desk's alone")
	})

	t.Run("isolation changes apply on the next request", func(t *testing.T) {
		ctx := context.Background()
		excl, multi, bare := d.client(t, rrExcl), d.client(t, rrMulti), d.client(t, raAccount)
		require.NoError(t, store.SetProjectIsolation(ctx, d.db, raOpen, core.IsolationConfidential))
		for account, cli := range map[string]*mcpclient.Client{rrExcl: excl, rrMulti: multi, raAccount: bare} {
			text, isErr := paCall(t, cli, "memory_read", map[string]any{"project": raOpen, "name": rrName(raOpen)})
			require.True(t, isErr, "%s: %s", account, text)
			require.Contains(t, text, rrRefusal(raOpen, account))
		}
		require.NoError(t, store.SetProjectIsolation(ctx, d.db, raOpen, core.IsolationOpen))
		_ = paCallOK(t, excl, "memory_read", map[string]any{"project": raOpen, "name": rrName(raOpen)})

		// The desk's one project: reached sealed, and open.
		for _, iso := range []core.Isolation{core.IsolationSealed, core.IsolationOpen} {
			require.NoError(t, store.SetProjectIsolation(ctx, d.db, raConf, iso))
			desk := d.client(t, rrDesk)
			got := paCallOK(t, desk, "memory_read", map[string]any{"project": raConf, "name": rrName(raConf)})
			require.Contains(t, fmt.Sprint(got), rrToken(raConf), "desk under %s", iso)
		}
		// Open now, raConf is in the bare account's reach and still outside the
		// excluding one's: exclude names what it leaves out, not a state.
		_ = paCallOK(t, bare, "memory_read", map[string]any{"project": raConf, "name": rrName(raConf)})
		require.NoError(t, store.SetProjectIsolation(ctx, d.db, raConf, core.IsolationConfidential))
		text, isErr := paCall(t, bare, "memory_read", map[string]any{"project": raConf, "name": rrName(raConf)})
		require.True(t, isErr, text)

		// An excluded project stays excluded in every state.
		for _, iso := range []core.Isolation{core.IsolationConfidential, core.IsolationOpen} {
			require.NoError(t, store.SetProjectIsolation(ctx, d.db, raDocs, iso))
			text, isErr := paCall(t, excl, "memory_read", map[string]any{"project": raDocs, "name": rrName(raDocs)})
			require.True(t, isErr, "under %s: %s", iso, text)
			require.Contains(t, text, rrRefusal(raDocs, rrExcl))
		}
	})
}

// An exclude slug that names no project disables that entry alone (B-D7):
// serve logs it, refuses that account at the door, and serves every other
// caller; an only slug that names none is a warning, and the entry is served.
func TestPeerAuth_RestrictedReachDisabledEntry(t *testing.T) {
	cfg := raPolicy()
	cfg.Restricted = []config.RestrictedAccount{
		{Account: raAccount},
		{Account: rrExcl, Exclude: []string{raDocs, "example-typo"}},
		{Account: rrMulti, Only: []string{raOpen, "example-later"}},
		{Account: rrMixed, Only: []string{raConf, raOpen}},
		{Account: rrLockd, Only: []string{raLockedOpen}},
	}
	d := newRADaemonFor(t, cfg)
	var log strings.Builder
	logger := slog.New(slog.NewTextHandler(&log, nil))
	policy, err := applyRestrictedReach(context.Background(), d.db, d.policy, logger)
	require.NoError(t, err)
	require.True(t, policy.RestrictedDisabled(rrUIDs[rrExcl]))
	require.False(t, policy.RestrictedDisabled(rrUIDs[rrMulti]))
	require.False(t, d.policy.RestrictedDisabled(rrUIDs[rrExcl]), "applyRestrictedReach copies the policy")
	out := log.String()
	require.Contains(t, out, `level=ERROR msg="peer_auth: peer_auth.restricted `+rrExcl+` excludes project \"example-typo\", which does not exist`)
	require.Contains(t, out, `level=WARN msg="peer_auth: peer_auth.restricted `+rrMulti+` names project \"example-later\" under only, which does not exist`)
	require.Contains(t, out, `level=WARN msg="peer_auth: peer_auth.restricted `+rrMixed+` names project \"`+raConf+`\" under only beside other projects, and it is confidential`)
	require.Contains(t, out, `level=WARN msg="peer_auth: peer_auth.restricted `+rrLockd+` names project \"`+raLockedOpen+`\" under only, which is locked`)

	serve := func(uid uint32) *httptest.Server {
		srv := httptest.NewServer(injectPeer(uid, peerHandler(policy, storeLeases(d.db), slog.New(slog.NewTextHandler(io.Discard, nil)), d.mux)))
		t.Cleanup(func() { closeServer(srv) })
		return srv
	}
	resp, err := http.Post(serve(rrUIDs[rrExcl]).URL+"/mcp", "application/json", strings.NewReader(`{}`))
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	require.Contains(t, string(body), "peer_auth.restricted entry is disabled until the owner corrects it")

	multi := paClient(t, serve(rrUIDs[rrMulti]).URL, "")
	_ = paCallOK(t, multi, "memory_write", map[string]any{"project": raOpen, "name": "still-served", "kind": "gotcha",
		"description": "d", "body": "b"})
	bare := paClient(t, serve(raUIDAgent).URL, "")
	_ = paCallOK(t, bare, "project_list", map[string]any{})

	// With no typo, nothing is disabled.
	cfg.Restricted[1].Exclude = []string{raDocs}
	clean := newRADaemonFor(t, cfg)
	policy, err = applyRestrictedReach(context.Background(), clean.db, clean.policy, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	require.False(t, policy.RestrictedDisabled(rrUIDs[rrExcl]))
}
