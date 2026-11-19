//go:build darwin || linux

package main

// Per-account reach for a relay's restricted accounts (peer_auth.relays'
// restricted with exclude and only; plan networked-relay): the rows of
// TestPeerAuthMatrix_RestrictedReach, with every request forwarded by a paired
// relay over the real TLS path (newRelayvDaemon: the owner's view, the device
// edge, the relay judge, the real routes). One account on example-laptop per
// shape:
//
//	example-agent   502  plain: every open, unlocked project
//	_example_excl   510  exclude [example-docs, example-vault]
//	_example_multi  511  only [example-open, example-docs]
//	_example_desk   512  only [example-books]                 (confidential, alone)
//	_example_mixed  513  only [example-books, example-open]   (the fenced one is dropped)
//	_example_lockd  514  only [example-finance]               (a lock closes it)
//	_example_seal   515  only [example-sealed]                (sealed, alone)
//
// The relay example-desk maps uid 512 too, as a plain account: a rule is found
// by the relay and the uid together, never by the uid alone.

import (
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/devices"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

const (
	rrRelayDocs   = "example-docs"    // open
	rrRelayVault  = "example-vault"   // open
	rrRelayBooks  = "example-books"   // confidential: the desk's
	rrRelayOther  = "example-other"   // confidential: one no account names
	rrRelaySealed = "example-sealed"  // sealed
	rrRelayNone   = "example-nowhere" // never registered

	rrRelayPlain = "example-agent" // the rig's plain account, uid relay2RestrictedUID
	rrRelayExcl  = "_example_excl"
	rrRelayMulti = "_example_multi"
	rrRelayDesk  = "_example_desk"
	rrRelayMixed = "_example_mixed"
	rrRelayLockd = "_example_lockd"
	rrRelaySeal  = "_example_seal"

	// rrRelayDeskClone is the desk's clone on example-laptop.
	rrRelayDeskClone = "/Users/_example_desk/repos/books"
)

var rrRelayUIDs = map[string]uint32{
	rrRelayPlain: relay2RestrictedUID, rrRelayExcl: 510, rrRelayMulti: 511, rrRelayDesk: 512, rrRelayMixed: 513,
	rrRelayLockd: 514, rrRelaySeal: 515,
}

// rrRelayShaped gives example-laptop's restricted accounts their shapes, and
// the relay example-desk a plain account of the desk's uid.
func rrRelayShaped(p *config.PeerAuth) {
	p.Relays[relayvLaptop] = config.PeerRelay{OwnerUID: relayvOwnerUID, Restricted: map[string]config.RelayRestricted{
		rrRelayPlain: {UID: rrRelayUIDs[rrRelayPlain]},
		rrRelayExcl:  {UID: rrRelayUIDs[rrRelayExcl], Exclude: []string{rrRelayDocs, rrRelayVault}},
		rrRelayMulti: {UID: rrRelayUIDs[rrRelayMulti], Only: []string{relayvOpen, rrRelayDocs}},
		rrRelayDesk:  {UID: rrRelayUIDs[rrRelayDesk], Only: []string{rrRelayBooks}},
		rrRelayMixed: {UID: rrRelayUIDs[rrRelayMixed], Only: []string{rrRelayBooks, relayvOpen}},
		rrRelayLockd: {UID: rrRelayUIDs[rrRelayLockd], Only: []string{relayvFin}},
		rrRelaySeal:  {UID: rrRelayUIDs[rrRelaySeal], Only: []string{rrRelaySealed}},
	}}
	p.Relays[relayvDesk] = config.PeerRelay{OwnerUID: relayvOwnerUID, Restricted: map[string]config.RelayRestricted{
		rrRelayDesk: {UID: rrRelayUIDs[rrRelayDesk]},
	}}
}

// rrRelayTargets is every project the matrix probes, plus the global scope.
var rrRelayTargets = []string{relayvOpen, rrRelayDocs, rrRelayVault, rrRelayBooks, rrRelayOther, rrRelaySealed, relayvFin, rrRelayNone, ""}

// rrRelayReach is the matrix: each account's reach, read and write. Global is
// readable by every account but the sealed one (its binding's fence closes
// global) and writable by none.
var rrRelayReach = map[string][]string{
	rrRelayPlain: {relayvOpen, rrRelayDocs, rrRelayVault},
	rrRelayExcl:  {relayvOpen},
	rrRelayMulti: {relayvOpen, rrRelayDocs},
	rrRelayDesk:  {rrRelayBooks},
	rrRelayMixed: {relayvOpen},
	rrRelayLockd: {},
	rrRelaySeal:  {rrRelaySealed},
}

func rrRelayToken(project string) string {
	if project == "" {
		return "rrrqglobalz"
	}
	return "rrrq" + strings.ReplaceAll(project, "-", "") + "z"
}

func rrRelayName(project string) string {
	if project == "" {
		return "rrr-global"
	}
	return "rrr-" + project
}

// rrRelayWriteName is the memory each account writes in the matrix, as the
// write stores its name (an underscore becomes a dash).
func rrRelayWriteName(account string) string {
	return "rrrw-" + strings.ReplaceAll(strings.TrimPrefix(account, "_"), "_", "-")
}

func rrRelayLabel(account string) string { return access.RelayAccountLabel(account, relayvLaptop) }

func rrRelayRefusal(project, account string) string {
	return "project " + project + " is not reachable from account " + rrRelayLabel(account)
}

// rrRelayHeaders are the headers a relay of wire version 2 sets for one local
// caller of uid uid; a restricted account carries no chain on the server, so
// the chain is the same for every account.
func rrRelayHeaders(uid uint32) map[string]string {
	c := relayvCaller{uid: uid, chain: []access.ProcessToken{relayvTok(2710), relayvTok(2700)}}
	return c.relay2Headers(map[int32]string{2700: relayvClaude})
}

type rrRelayRig struct {
	*relayvDaemon
	laptop, desk *devices.RelayIdentity
	ids          map[string]string // planted memory ids, by project
}

func newRRRelayRig(t *testing.T) *rrRelayRig {
	t.Helper()
	d := &rrRelayRig{relayvDaemon: newRelayvDaemon(t, rrRelayShaped), ids: map[string]string{}}
	ctx := context.Background()
	for slug, iso := range map[string]core.Isolation{rrRelayDocs: core.IsolationOpen, rrRelayVault: core.IsolationOpen,
		rrRelayBooks: core.IsolationConfidential, rrRelayOther: core.IsolationConfidential, rrRelaySealed: core.IsolationSealed} {
		_, err := store.EnsureProject(ctx, d.db, slug, slug)
		require.NoError(t, err)
		require.NoError(t, store.SetProjectIsolation(ctx, d.db, slug, iso))
	}
	// The desk's clone on example-laptop maps to its project there, as
	// `seamlessd map-repo --host` maps it: the repo map is keyed by host, and a
	// relay's requests speak for its enrolled name.
	require.NoError(t, store.AddHostRepoMapping(ctx, d.db, relayvLaptop, rrRelayDeskClone, rrRelayBooks))
	now := time.Now().UTC()
	for _, p := range rrRelayTargets {
		if p == rrRelayNone {
			continue
		}
		id, err := core.NewID()
		require.NoError(t, err)
		_, err = d.mgr.WriteMemory(ctx, core.Memory{ID: id, Kind: core.KindGotcha, Name: rrRelayName(p),
			Description: "planted " + rrRelayToken(p), Project: p, Body: "body " + rrRelayToken(p),
			Created: now, Updated: now, ValidFrom: now})
		require.NoError(t, err)
		d.ids[p] = id
	}
	_, d.laptop = d.pair(t, relayvLaptop)
	_, d.desk = d.pair(t, relayvDesk)
	return d
}

// client is account's MCP client through example-laptop's relay; an account
// whose reach holds a fenced project binds to it first, as a session native to
// that project does.
func (d *rrRelayRig) client(t *testing.T, account string) *mcpclient.Client {
	t.Helper()
	cli := d.mcpVia(t, d.laptop, "", rrRelayHeaders(rrRelayUIDs[account]))
	for _, p := range rrRelayReach[account] {
		if p == rrRelayBooks || p == rrRelaySealed {
			paCallOK(t, cli, "session_start", map[string]any{"project": p, "name": "rrr-" + strings.TrimPrefix(account, "_")})
		}
	}
	return cli
}

// hook posts a hook event through the relay identity id for the caller of uid
// uid, and returns the answer's body.
func (d *rrRelayRig) hook(t *testing.T, id *devices.RelayIdentity, uid uint32, event string, query url.Values, body map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, d.tcp.URL+"/api/hooks/"+event+"?"+query.Encode(), strings.NewReader(string(raw)))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range rrRelayHeaders(uid) {
		req.Header.Set(k, v)
	}
	resp, err := d.httpClient(id).Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(out))
	return string(out)
}

// projectArgDesc is the description tools/list gives cli for memory_write's
// project argument.
func rrRelayProjectArgDesc(t *testing.T, cli *mcpclient.Client) string {
	t.Helper()
	res, err := cli.ListTools(context.Background(), mcpgo.ListToolsRequest{})
	require.NoError(t, err)
	for _, tool := range res.Tools {
		if tool.Name == "memory_write" {
			prop, ok := tool.InputSchema.Properties["project"].(map[string]any)
			require.True(t, ok)
			desc, _ := prop["description"].(string)
			return desc
		}
	}
	t.Fatal("tools/list has no memory_write")
	return ""
}

var rrRelaySlugRE = regexp.MustCompile(`"slug":"([^"]+)"`)

var rrRelayStateWord = regexp.MustCompile(`\b(open|confidential|sealed|locked)\b`)

func TestRRRelayReach_Matrix(t *testing.T) {
	d := newRRRelayRig(t)
	ctx := context.Background()

	t.Run("the matrix: reach per shape, read and write, through the relay", func(t *testing.T) {
		for account, reach := range rrRelayReach {
			cli := d.client(t, account)
			label := rrRelayLabel(account)
			for _, p := range rrRelayTargets {
				arg := p
				if arg == "" {
					arg = "global"
				}
				in := slices.Contains(reach, p)
				text, isErr := paCall(t, cli, "memory_read", map[string]any{"project": arg, "name": rrRelayName(p)})
				switch {
				case in || p == "" && account != rrRelaySeal:
					require.False(t, isErr, "%s reads %s: %s", account, arg, text)
					require.Contains(t, text, rrRelayToken(p))
				default:
					require.True(t, isErr, "%s reads %s: %s", account, arg, text)
					require.NotContains(t, text, rrRelayToken(p))
					if p != "" {
						require.Contains(t, text, rrRelayRefusal(p, account))
					}
				}
				text, isErr = paCall(t, cli, "memory_write", map[string]any{"project": arg, "name": rrRelayWriteName(account),
					"kind": "gotcha", "description": "written by " + account, "body": "b"})
				switch {
				case in:
					require.False(t, isErr, "%s writes %s: %s", account, arg, text)
				case p == "":
					require.True(t, isErr, "%s writes global: %s", account, text)
					if account == rrRelayPlain {
						require.Contains(t, text, "the global scope is read-only for account "+label)
					} else {
						require.Contains(t, text, "account "+label+" does not write the global scope")
					}
				default:
					require.True(t, isErr, "%s writes %s: %s", account, arg, text)
					require.Contains(t, text, rrRelayRefusal(p, account))
				}
			}
			// What it wrote is labeled with the account and the host.
			for _, p := range reach {
				m, found, err := store.MemoryByName(ctx, d.db, p, rrRelayWriteName(account))
				require.NoError(t, err)
				require.True(t, found, "%s wrote into %s", account, p)
				require.Equal(t, label, m.Account)
			}
			// project_list lists exactly the reach.
			raw, err := json.Marshal(paCallOK(t, cli, "project_list", map[string]any{}))
			require.NoError(t, err)
			var listed []string
			for _, m := range rrRelaySlugRE.FindAllStringSubmatch(string(raw), -1) {
				listed = append(listed, m[1])
			}
			slices.Sort(listed)
			want := slices.Clone(reach)
			slices.Sort(want)
			require.Equal(t, want, append([]string{}, listed...), "%s's project_list", account)
			// recall finds nothing planted outside the reach.
			for _, p := range rrRelayTargets {
				if p == "" || slices.Contains(reach, p) {
					continue
				}
				text, _ := paCall(t, cli, "recall", map[string]any{"query": rrRelayToken(p)})
				require.NotContains(t, text, rrRelayToken(p), "%s recalls %s", account, p)
			}
		}
		require.ElementsMatch(t, slices.Collect(maps.Keys(rrRelayUIDs)), slices.Collect(maps.Keys(rrRelayReach)))
	})

	t.Run("a shaped account reads the neutral texts, a plain one today's", func(t *testing.T) {
		excl := d.client(t, rrRelayExcl)
		label := rrRelayLabel(rrRelayExcl)
		var texts []string
		for _, p := range []string{rrRelayDocs, rrRelayVault, rrRelayBooks, rrRelaySealed, relayvFin, rrRelayNone} {
			text, isErr := paCall(t, excl, "memory_read", map[string]any{"project": p, "name": rrRelayName(p)})
			require.True(t, isErr, text)
			texts = append(texts, strings.ReplaceAll(text, p, "<p>"))
		}
		for _, other := range texts[1:] {
			require.Equal(t, texts[0], other, "one refusal for excluded, closed and unregistered projects")
		}
		require.Contains(t, texts[0], "the owner gives this account only the projects project_list shows")
		require.False(t, rrRelayStateWord.MatchString(strings.ReplaceAll(texts[0], "<p>", "")), texts[0])

		// An excluded project's id answers exactly as a missing one.
		missing, err := core.NewID()
		require.NoError(t, err)
		miss, isErr := paCall(t, excl, "memory_read", map[string]any{"id": missing})
		require.True(t, isErr)
		text, isErr := paCall(t, excl, "memory_read", map[string]any{"id": d.ids[rrRelayDocs]})
		require.True(t, isErr, text)
		require.Equal(t, strings.ReplaceAll(miss, missing, d.ids[rrRelayDocs]), text)

		// The project argument, the write-scope help, session_start's placement
		// refusal and access_request: the shaped set, keyed on the entry.
		desc := rrRelayProjectArgDesc(t, excl)
		require.Contains(t, desc, "This restricted account writes only into the projects project_list shows")
		require.False(t, rrRelayStateWord.MatchString(desc), desc)
		text, isErr = paCall(t, excl, "memory_write", map[string]any{"name": "rrr-unscoped", "kind": "gotcha", "description": "d", "body": "b"})
		require.True(t, isErr, text)
		require.Contains(t, text, "this connection is restricted account "+label+": it writes only into the projects project_list shows")
		text, isErr = paCall(t, excl, "session_start", map[string]any{"name": "rrr-nowhere", "cwd": "/Users/_example_excl/scratch"})
		require.True(t, isErr, text)
		require.Contains(t, text, "account "+label+" does not write the global scope: it works only in the projects project_list shows")
		text, isErr = paCall(t, excl, "access_request", map[string]any{"project": rrRelayDocs, "reason": "please"})
		require.True(t, isErr, text)
		require.Contains(t, text, "account "+label+" files no access requests: the owner gives this account only the projects project_list shows")

		// The plain account keeps today's texts.
		bare := d.client(t, rrRelayPlain)
		bareLabel := rrRelayLabel(rrRelayPlain)
		text, _ = paCall(t, bare, "memory_read", map[string]any{"project": rrRelayBooks, "name": rrRelayName(rrRelayBooks)})
		require.Contains(t, text, "a restricted account reaches open projects that are not locked")
		require.Contains(t, rrRelayProjectArgDesc(t, bare), "This restricted account writes only into open projects that already exist")
		text, _ = paCall(t, bare, "memory_write", map[string]any{"name": "rrr-unscoped", "kind": "gotcha", "description": "d", "body": "b"})
		require.Contains(t, text, "this connection is restricted account "+bareLabel+": it writes only into open projects that already exist")
		text, _ = paCall(t, bare, "session_start", map[string]any{"name": "rrr-nowhere", "cwd": "/Users/example-agent/scratch"})
		require.Contains(t, text, "the global scope is read-only for account "+bareLabel+": a restricted account works only in open projects")
		text, _ = paCall(t, bare, "access_request", map[string]any{"project": rrRelayBooks, "reason": "please"})
		require.Contains(t, text, access.RestrictedNoRequests(bareLabel))
		require.Zero(t, raCount(t, d.db, `SELECT COUNT(*) FROM access_approvals`), "a restricted account files nothing")
	})

	t.Run("the desk: its one confidential project, its records, its proposals", func(t *testing.T) {
		desk := d.client(t, rrRelayDesk)
		label := rrRelayLabel(rrRelayDesk)
		w := paCallOK(t, desk, "memory_write", map[string]any{"project": rrRelayBooks, "name": "rrr-desk-fact", "kind": "gotcha",
			"description": "desk fact", "body": "b"})
		require.Equal(t, rrRelayBooks, w["project"], "%v", w)
		got := paCallOK(t, desk, "memory_read", map[string]any{"project": rrRelayBooks, "name": "rrr-desk-fact"})
		require.Equal(t, label, got["account"], "a desk write carries its label: the account and the host")
		p := paCallOK(t, desk, "memory_write", map[string]any{"project": rrRelayBooks, "name": "rrr-desk-rule", "kind": "constraint",
			"description": "desk rule", "body": "always"})
		require.Equal(t, "proposed", p["status"], "a binding rule waits for the owner, in the desk's project too: %v", p)
		require.Equal(t, label, p["proposed_by"])

		// Its SessionStart hook through the relay: recorded in the clone mapped
		// to its project on its computer, the shaped notice anywhere else.
		start := func(id *devices.RelayIdentity, uid uint32, sessionID, cwd string) string {
			q := url.Values{"host": {"example-claimed-host"}, "agent_process": {"7100.71000"}, "repo_root": {cwd}, "main_root": {cwd}}
			return d.hook(t, id, uid, "session-start", q, map[string]any{"session_id": sessionID, "cwd": cwd, "source": "startup",
				"hook_event_name": "SessionStart"})
		}
		outside := start(d.laptop, rrRelayUIDs[rrRelayDesk], "rrr-desk-outside", "/Users/_example_desk/repos/elsewhere")
		require.Contains(t, outside, "this directory maps to no project account "+label+" may reach")
		require.Contains(t, outside, "It works only in the projects the owner gives it (project_list shows them)")
		require.False(t, rrRelayStateWord.MatchString(outside), outside)
		require.Zero(t, raCount(t, d.db, `SELECT COUNT(*) FROM sessions WHERE claude_session_id LIKE ?`, "%rrr-desk-outside%"))
		inside := start(d.laptop, rrRelayUIDs[rrRelayDesk], "rrr-desk-inside", rrRelayDeskClone)
		require.Contains(t, inside, "seam-briefing", inside)
		var project, account, host string
		require.NoError(t, d.db.QueryRow(`SELECT project_slug, account, host FROM sessions WHERE claude_session_id LIKE ?`,
			"%rrr-desk-inside%").Scan(&project, &account, &host))
		require.Equal(t, rrRelayBooks, project)
		require.Equal(t, label, account)
		require.Equal(t, relayvLaptop, host)

		// The plain account's notice is today's, word for word.
		bareNotice := start(d.laptop, rrRelayUIDs[rrRelayPlain], "rrr-bare-outside", rrRelayDeskClone)
		require.Contains(t, bareNotice, "A restricted account works only in the owner's open projects")
		require.Zero(t, raCount(t, d.db, `SELECT COUNT(*) FROM access_approvals`))
	})

	t.Run("one uid on two relays, and on this machine: each its own entry", func(t *testing.T) {
		// uid 512 on example-desk is a plain account: the open projects, never
		// example-books, and today's texts.
		other := d.mcpVia(t, d.desk, "", rrRelayHeaders(rrRelayUIDs[rrRelayDesk]))
		otherLabel := access.RelayAccountLabel(rrRelayDesk, relayvDesk)
		text, isErr := paCall(t, other, "memory_read", map[string]any{"project": rrRelayBooks, "name": rrRelayName(rrRelayBooks)})
		require.True(t, isErr, text)
		require.Contains(t, text, "project "+rrRelayBooks+" is not reachable from account "+otherLabel+": a restricted account reaches open projects")
		got := paCallOK(t, other, "memory_read", map[string]any{"project": rrRelayDocs, "name": rrRelayName(rrRelayDocs)})
		require.Contains(t, fmt.Sprint(got), rrRelayToken(rrRelayDocs))
		raw, err := json.Marshal(paCallOK(t, other, "project_list", map[string]any{}))
		require.NoError(t, err)
		var listed []string
		for _, m := range rrRelaySlugRE.FindAllStringSubmatch(string(raw), -1) {
			listed = append(listed, m[1])
		}
		require.ElementsMatch(t, []string{relayvOpen, rrRelayDocs, rrRelayVault}, listed)

		// This machine's socket knows no uid 512 at all.
		srv := d.socket(t, rrRelayUIDs[rrRelayDesk])
		resp, err := http.Post(srv.URL+"/api/mcp", "application/json", strings.NewReader(relayvInitialize()))
		require.NoError(t, err)
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusForbidden, resp.StatusCode, string(body))
	})

	t.Run("the owner through the relay reaches everything as before", func(t *testing.T) {
		own := d.mcpVia(t, d.laptop, "", relayvCallerA.relay2Headers(relay2Named))
		for _, p := range []string{relayvOpen, rrRelayDocs, rrRelayVault} {
			got := paCallOK(t, own, "memory_read", map[string]any{"project": p, "name": rrRelayName(p)})
			require.Contains(t, fmt.Sprint(got), rrRelayToken(p))
		}
		paCallOK(t, own, "session_start", map[string]any{"project": rrRelayBooks, "name": "rrr-owner-books"})
		got := paCallOK(t, own, "memory_read", map[string]any{"project": rrRelayBooks, "name": rrRelayName(rrRelayBooks)})
		require.Contains(t, fmt.Sprint(got), rrRelayToken(rrRelayBooks), "the desk's project is not the desk's alone")
	})

	t.Run("isolation changes apply on the next request", func(t *testing.T) {
		excl, multi, bare := d.client(t, rrRelayExcl), d.client(t, rrRelayMulti), d.client(t, rrRelayPlain)
		require.NoError(t, store.SetProjectIsolation(ctx, d.db, relayvOpen, core.IsolationConfidential))
		for account, cli := range map[string]*mcpclient.Client{rrRelayExcl: excl, rrRelayMulti: multi, rrRelayPlain: bare} {
			text, isErr := paCall(t, cli, "memory_read", map[string]any{"project": relayvOpen, "name": rrRelayName(relayvOpen)})
			require.True(t, isErr, "%s: %s", account, text)
			require.Contains(t, text, rrRelayRefusal(relayvOpen, account))
		}
		require.NoError(t, store.SetProjectIsolation(ctx, d.db, relayvOpen, core.IsolationOpen))
		_ = paCallOK(t, excl, "memory_read", map[string]any{"project": relayvOpen, "name": rrRelayName(relayvOpen)})

		// The desk's one project: reached sealed, and open.
		for _, iso := range []core.Isolation{core.IsolationSealed, core.IsolationOpen} {
			require.NoError(t, store.SetProjectIsolation(ctx, d.db, rrRelayBooks, iso))
			desk := d.client(t, rrRelayDesk)
			got := paCallOK(t, desk, "memory_read", map[string]any{"project": rrRelayBooks, "name": rrRelayName(rrRelayBooks)})
			require.Contains(t, fmt.Sprint(got), rrRelayToken(rrRelayBooks), "desk under %s", iso)
		}
		// Open now, example-books is in the plain reach, and in the mixed
		// entry's: an only of several projects reaches those that are open.
		mixed := d.client(t, rrRelayMixed)
		for _, cli := range []*mcpclient.Client{bare, mixed} {
			_ = paCallOK(t, cli, "memory_read", map[string]any{"project": rrRelayBooks, "name": rrRelayName(rrRelayBooks)})
		}
		require.NoError(t, store.SetProjectIsolation(ctx, d.db, rrRelayBooks, core.IsolationConfidential))
		for account, cli := range map[string]*mcpclient.Client{rrRelayPlain: bare, rrRelayMixed: mixed} {
			text, isErr := paCall(t, cli, "memory_read", map[string]any{"project": rrRelayBooks, "name": rrRelayName(rrRelayBooks)})
			require.True(t, isErr, "%s: %s", account, text)
			require.NotContains(t, text, rrRelayToken(rrRelayBooks))
		}

		// An excluded project stays excluded in every state.
		for _, iso := range []core.Isolation{core.IsolationConfidential, core.IsolationOpen} {
			require.NoError(t, store.SetProjectIsolation(ctx, d.db, rrRelayDocs, iso))
			text, isErr := paCall(t, excl, "memory_read", map[string]any{"project": rrRelayDocs, "name": rrRelayName(rrRelayDocs)})
			require.True(t, isErr, "under %s: %s", iso, text)
			require.Contains(t, text, rrRelayRefusal(rrRelayDocs, rrRelayExcl))
		}
	})
}

// An exclude slug that names no project disables that relay entry alone
// (B-D7): serve logs it, refuses that account through its relay, and serves
// every other caller -- the same uid and name on another relay included; an
// only slug that names none, that is locked, or a fenced one beside others,
// warns, and the entry is served.
func TestRRRelayReach_DisabledEntry(t *testing.T) {
	d := newRelayvDaemon(t, func(p *config.PeerAuth) {
		p.Relays[relayvLaptop] = config.PeerRelay{OwnerUID: relayvOwnerUID, Restricted: map[string]config.RelayRestricted{
			rrRelayPlain: {UID: rrRelayUIDs[rrRelayPlain]},
			rrRelayExcl:  {UID: rrRelayUIDs[rrRelayExcl], Exclude: []string{relayvFin, "example-typo"}},
			rrRelayMulti: {UID: rrRelayUIDs[rrRelayMulti], Only: []string{relayvOpen, "example-later"}},
			rrRelayMixed: {UID: rrRelayUIDs[rrRelayMixed], Only: []string{rrRelayBooks, relayvOpen}},
			rrRelayLockd: {UID: rrRelayUIDs[rrRelayLockd], Only: []string{relayvFin}},
		}}
		p.Relays[relayvDesk] = config.PeerRelay{OwnerUID: relayvOwnerUID, Restricted: map[string]config.RelayRestricted{
			rrRelayExcl: {UID: rrRelayUIDs[rrRelayExcl], Exclude: []string{relayvFin}},
		}}
	})
	ctx := context.Background()
	_, err := store.EnsureProject(ctx, d.db, rrRelayBooks, rrRelayBooks)
	require.NoError(t, err)
	require.NoError(t, store.SetProjectIsolation(ctx, d.db, rrRelayBooks, core.IsolationConfidential))
	_, laptop := d.pair(t, relayvLaptop)
	_, desk := d.pair(t, relayvDesk)

	var log strings.Builder
	policy, err := applyRestrictedReach(ctx, d.db, d.policy, slog.New(slog.NewTextHandler(&log, nil)))
	require.NoError(t, err)
	excl := rrRelayUIDs[rrRelayExcl]
	require.True(t, policy.RelayRestrictedDisabled(relayvLaptop, excl))
	require.False(t, policy.RelayRestrictedDisabled(relayvDesk, excl), "the same name and uid on another relay is another entry")
	require.False(t, policy.RelayRestrictedDisabled(relayvLaptop, rrRelayUIDs[rrRelayMulti]))
	require.False(t, d.policy.RelayRestrictedDisabled(relayvLaptop, excl), "applyRestrictedReach copies the policy")
	out := log.String()
	require.Contains(t, out, `level=ERROR msg="peer_auth: peer_auth.relays.example-laptop.restricted _example_excl excludes project \"example-typo\", `+
		`which does not exist: if that is a typo, the project it meant is NOT excluded, so serve refuses this account (uid 510 on example-laptop) `+
		`until the entry is corrected; every other caller is served"`)
	require.Contains(t, out, `level=INFO msg="peer_auth: peer_auth.relays.example-laptop.restricted _example_excl excludes project \"example-finance\", which is confidential now`)
	require.Contains(t, out, `level=WARN msg="peer_auth: peer_auth.relays.example-laptop.restricted _example_multi names project \"example-later\" under only, which does not exist`)
	require.Contains(t, out, `level=WARN msg="peer_auth: peer_auth.relays.example-laptop.restricted _example_mixed names project \"example-books\" under only beside other projects, and it is confidential`)
	require.Contains(t, out, `level=WARN msg="peer_auth: peer_auth.relays.example-laptop.restricted _example_lockd names project \"example-finance\" under only, which is locked`)
	require.NotContains(t, out, "peer_auth.relays.example-desk.restricted _example_excl excludes project \"example-typo\"")

	// The startup log names each relay account's shape, and the disabled one
	// as an error. It walks the relays the config maps; their accounts are
	// the policy's.
	log.Reset()
	rt := &peerAuthRuntime{policy: policy, env: peerAuthEnv{daemonAccount: paNoPin}}
	rt.cfg.Relays = map[string]config.PeerRelay{relayvLaptop: {OwnerUID: relayvOwnerUID}, relayvDesk: {OwnerUID: relayvOwnerUID},
		relayvOld: {OwnerUID: relayvOwnerUID}}
	rt.logPolicy(slog.New(slog.NewTextHandler(&log, nil)))
	out = log.String()
	require.Contains(t, out, `level=ERROR msg="peer_auth relay restricted account disabled: its exclude names a project that does not exist, so `+
		`serve refuses it until the entry is corrected (seamlessd doctor names it)" relay=example-laptop user=_example_excl@example-laptop uid=510 `+
		`exclude=example-finance,example-typo`)
	require.Contains(t, out, `msg="peer_auth relay restricted account" relay=example-laptop user=_example_multi@example-laptop uid=511 only=example-later,example-open`)
	require.Contains(t, out, `msg="peer_auth relay restricted account" relay=example-laptop user=example-agent@example-laptop uid=502`+"\n",
		"a plain account's line is as before")
	require.Contains(t, out, `msg="peer_auth relay restricted account" relay=example-desk user=_example_excl@example-desk uid=510 exclude=example-finance`)

	// The relay judge under the policy serve applied.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cert, pool := deviceServerCert(t)
	ts := httptest.NewUnstartedServer(ownerHandler(policy, d.pairing.Edge(relayHandler(policy, storeLeases(d.db), logger, d.mux))))
	ts.TLS = devices.WithRelayClientAuth(&tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}})
	ts.StartTLS()
	t.Cleanup(func() { closeServer(ts) })
	d.pool = pool
	d.tcp = ts

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/mcp", strings.NewReader(relayvInitialize()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range rrRelayHeaders(excl) {
		req.Header.Set(k, v)
	}
	resp, err := d.httpClient(laptop).Do(req)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	require.Equal(t, "forbidden: uid "+strconv.Itoa(int(excl))+" on relay example-laptop: its peer_auth.relays restricted entry is disabled "+
		"until the owner corrects it (seamlessd doctor on the server names the cause)\n", string(body))

	multi := d.mcpVia(t, laptop, "", rrRelayHeaders(rrRelayUIDs[rrRelayMulti]))
	_ = paCallOK(t, multi, "memory_write", map[string]any{"project": relayvOpen, "name": "rrr-still-served", "kind": "gotcha",
		"description": "d", "body": "b"})
	plain := d.mcpVia(t, laptop, "", rrRelayHeaders(rrRelayUIDs[rrRelayPlain]))
	_ = paCallOK(t, plain, "project_list", map[string]any{})
	twin := d.mcpVia(t, desk, "", rrRelayHeaders(excl))
	_ = paCallOK(t, twin, "memory_read", map[string]any{"project": relayvOpen, "name": d.openMem})
	own := d.mcpVia(t, laptop, "", relayvCallerA.relay2Headers(relay2Named))
	_ = paCallOK(t, own, "project_list", map[string]any{})
}

// Doctor's rows for a relay's restricted entries (B-D8, as for this machine's):
// a plain account keeps the words the relay's own row gave it before shapes
// existed, and no row of its own; a shaped one gets a row with its shape and
// what it reaches now; FAIL for an entry an exclude typo disables (serve
// still starts), WARN for an only slug it cannot reach, INFO for an excluded
// project that is not open now.
func TestRRRelayReach_DoctorRows(t *testing.T) {
	l, cfg := newPALayout(t, `  relays:
    example-laptop:
      owner_uid: 501
      restricted:
        example-agent: 502
        _example_excl: {uid: 510, exclude: [example-docs, example-typo]}
        _example_desk:
          uid: 512
          only: [example-books]
        _example_mixed: {uid: 513, only: [example-books, example-ops]}
        _example_later: {uid: 516, only: [example-later]}
`)
	env := paTestEnv(l, uint32(os.Geteuid())+1000)
	ctx := context.Background()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	for slug, iso := range map[string]core.Isolation{"example-ops": core.IsolationOpen, "example-docs": core.IsolationSealed,
		rrRelayBooks: core.IsolationConfidential} {
		_, err := store.EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
		require.NoError(t, store.SetProjectIsolation(ctx, db, slug, iso))
	}

	text := func(db *sql.DB) string {
		var b strings.Builder
		for _, r := range peerAuthChecks(ctx, cfg, db, env) {
			fmt.Fprintf(&b, "%v %s %s\n", r.status, r.name, r.detail)
		}
		return b.String()
	}
	out := text(db)
	row := func(status checkStatus, detail string) string {
		return fmt.Sprintf("%v peer_auth.relays.restricted %s", status, detail)
	}
	tail := "its writes are labeled with its name, and its constraints, conventions, protocols and stages wait for the owner's promotion"
	require.Contains(t, out, row(statusOK, "_example_excl@example-laptop (uid 510 there): restricted, exclude [example-docs, example-typo] -- "+
		"through relay example-laptop only; reads and writes every open project that is not locked but those, reads global (now 1 project); "+tail))
	require.Contains(t, out, row(statusOK, "_example_desk@example-laptop (uid 512 there): restricted, only [example-books] -- through relay "+
		"example-laptop only; reads and writes that project while it is open, confidential or sealed and not locked, and reads global "+
		"unless it is sealed (now example-books (confidential)); "+tail))
	require.Contains(t, out, row(statusOK, "_example_mixed@example-laptop (uid 513 there): restricted, only [example-books, example-ops] -- "+
		"through relay example-laptop only; reads and writes those of them that are open and not locked, reads global (now example-ops (open)); "+tail))
	require.Contains(t, out, row(statusOK, "_example_later@example-laptop (uid 516 there): restricted, only [example-later]"))
	require.Contains(t, out, "(now none)")
	require.NotContains(t, out, "peer_auth.relays.restricted example-agent@", "a plain account has no row of its own")
	require.NotContains(t, out, "peer_auth.restricted ", "no entry of this machine's, so no row of one")

	require.Contains(t, out, row(statusFail, `peer_auth.relays.example-laptop.restricted _example_excl excludes project "example-typo", `+
		`which does not exist: if that is a typo, the project it meant is NOT excluded, so serve refuses this account (uid 510 on example-laptop) `+
		`until the entry is corrected; every other caller is served`))
	require.Contains(t, out, row(statusInfo, `peer_auth.relays.example-laptop.restricted _example_excl excludes project "example-docs", which is sealed now`))
	require.Contains(t, out, row(statusWarn, `peer_auth.relays.example-laptop.restricted _example_mixed names project "example-books" under only beside other projects`))
	require.Contains(t, out, row(statusWarn, `peer_auth.relays.example-laptop.restricted _example_later names project "example-later" under only, which does not exist`))
	require.Contains(t, out, "preflight passes: serve will start with the boundary up", "a disabled entry never stops serve")

	// Without a database doctor names the shapes and checks no slug.
	out = text(nil)
	require.Contains(t, out, row(statusOK, "_example_desk@example-laptop (uid 512 there): restricted, only [example-books] -- through relay "+
		"example-laptop only; reads and writes that project while it is open, confidential or sealed and not locked, and reads global "+
		"unless it is sealed; "+tail))
	require.NotContains(t, out, `"example-typo", which does not exist`)

	// The relay's own row names every account there, shaped or plain, as it
	// named them before shapes existed.
	cfg.Addr = "0.0.0.0:8081"
	judgeStoreRelay(t, db, relayvLaptop, "01PASSKEYEXAMPLE")
	rows := relayMappingChecks(ctx, cfg, db)
	require.Len(t, rows, 1)
	require.Equal(t, "relay example-laptop: its requests are judged as this socket's, and uid 501 there is the owner; restricted there: "+
		"_example_desk (uid 512), _example_excl (uid 510), _example_later (uid 516), _example_mixed (uid 513), example-agent (uid 502)",
		rows[0].detail)
}
