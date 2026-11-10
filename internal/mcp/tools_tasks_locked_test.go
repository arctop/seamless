package mcp_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// A blocker in a locked project, told to the owner (owner
// decision D7): tasks_ready and the claim refusal name the locked project and
// how to reach it -- by the owner's own approval on the socket, from the
// socket on TCP -- where every other caller, and every blocker an isolation
// fence alone withholds, keeps the generic words. The blocker's title and id
// are withheld either way.

const (
	lbSocketReady = "a task in locked project " + lkFin + " blocks this one: bind to " + lkFin + ", or approve a request for it"
	lbTCPReady    = "a task in locked project " + lkFin + " blocks this one: the TCP connection never reaches a locked " +
		"project, so work in it from an agent session on the peer_auth Unix socket"
	lbSocketRemedy = "\nRemedy: bind to " + lkFin + ", or approve a request for it."
	lbTCPRemedy    = "\nRemedy: the TCP connection never reaches a locked project, so work in it from an agent " +
		"session on the peer_auth Unix socket."
	lbGeneric = "a task in a project this connection may not reach blocks this one"
)

func TestLockedBlocker_NamedToTheOwner(t *testing.T) {
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
	// example-vault is confidential and NOT locked: the fence alone.
	const vault = "example-vault"
	_, err := store.EnsureProject(ctx, r.db, vault, vault)
	require.NoError(t, err)
	require.NoError(t, store.SetProjectIsolation(ctx, r.db, vault, core.IsolationConfidential))

	const secret = "reconcile the quarter ledger"
	fin := newTask(lkFin, secret)
	fin2 := newTask(lkFin, secret+" twice")
	prep := newTask(lkOps, "ops prep")
	vaultTask := newTask(vault, "vault secret task")
	waits := newTask(lkOps, "ops waits on finance", fin)
	mixed := newTask(lkOps, "ops waits on everything", fin, fin2, prep, vaultTask)
	fenced := newTask(lkOps, "ops waits on the vault", vaultTask)

	tcp := r.dial(r.tcp(), false)
	r.ok(tcp, "session_start", map[string]any{"project": lkOps, "name": "cc/lb-tcp"})
	stranger := r.dial(r.socket(lkStranger), false)
	r.ok(stranger, "session_start", map[string]any{"project": lkOps, "name": "cc/lb-stranger"})

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
	w := func(s string) map[string]any { return map[string]any{"withheld": s} }

	for _, tc := range []struct {
		name          string
		cli           *mcpclient.Client
		ready, remedy string
	}{
		{"tcp", tcp, lbTCPReady, lbTCPRemedy},
		{"socket without a lease", stranger, lbSocketReady, lbSocketRemedy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text, blocked := readyOf(tc.cli)
			for _, id := range []string{fin, fin2, vaultTask} {
				require.NotContains(t, text, id, "no withheld blocker's id")
			}
			require.NotContains(t, text, secret)
			require.NotContains(t, text, "vault secret task")
			require.Equal(t, []any{w(tc.ready)}, blocked[waits]["blockers"])
			require.Equal(t, []any{w(tc.ready), w(tc.ready), map[string]any{"id": prep, "status": "open", "title": "ops prep"},
				w(lbGeneric)}, blocked[mixed]["blockers"], "the count is kept, and the fence alone stays generic")
			require.Equal(t, []any{w(lbGeneric)}, blocked[fenced]["blockers"],
				"a confidential project that is not locked is not named")

			got := r.refused(tc.cli, "tasks_claim", map[string]any{"id": waits})
			require.Equal(t, `tasks_claim: store.ClaimTask: task blocked: task "`+waits+`" waits on a task in locked project `+
				lkFin+tc.remedy, got)
			got = r.refused(tc.cli, "tasks_claim", map[string]any{"id": mixed})
			require.Equal(t, `tasks_claim: store.ClaimTask: task blocked: task "`+mixed+`" waits on `+prep+
				` (open), 2 tasks in locked project `+lkFin+`, a task in a project this connection may not reach`+tc.remedy, got)
			got = r.refused(tc.cli, "tasks_claim", map[string]any{"id": fenced})
			require.Equal(t, `tasks_claim: store.ClaimTask: task blocked: task "`+fenced+
				`" waits on a task in a project this connection may not reach`, got, "no remedy for a fence")
		})
	}
	require.Empty(t, r.pending(), "naming a blocker files no request")

	// The approved tree, bound in the locked project, reads the blocker.
	r.session("cc/fin", lkFin)
	agent := r.dial(r.socket(lkBridge, lkAgent), true)
	r.refused(agent, "tasks_list", nil)
	pending := r.pending()
	require.Len(t, pending, 1)
	r.approve(pending[0].ID, time.Hour)
	text, blocked := readyOf(agent)
	require.Contains(t, text, secret)
	require.Equal(t, []any{map[string]any{"id": fin, "status": "open", "title": secret}}, blocked[waits]["blockers"])
	require.NotContains(t, text, "locked project")
}
