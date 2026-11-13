package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// approveGrant files and approves a read of target from the asking project,
// held by sessionID or, under peer_auth, by the process processKey names.
func approveGrant(t *testing.T, e *captureEnv, from, target, sessionID, processKey string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	req := store.GrantRequest{
		Project: target, From: from, Lease: time.Hour, TTL: 10 * time.Minute,
		SessionID: sessionID, SessionName: "cc/ops", CWD: "/work/demo", Repo: "demo",
		Model: "claude-opus-5-5", Reason: "check the invoice totals",
	}
	if processKey != "" {
		req.ProcessKey, req.PeerUID, req.PeerPID, req.Program = processKey, 501, 200, "/usr/local/bin/claude"
	}
	a, err := store.CreateGrantRequest(ctx, e.db, req, now)
	require.NoError(t, err)
	_, err = store.ApproveAccessGrant(ctx, e.db, a.ID, a.Challenge, "owner", time.Hour, now)
	require.NoError(t, err)
}

// What the hooks record for a conversation meets the clearance rule its tool
// calls meet: holding a confidential project's content (here by a read grant
// from the open project demo), it writes into no open project, so a plan
// captured in demo or from another project's directory is dropped with a
// notice. With read grants off nothing about a capture changes.
func TestPlanCapture_ReadGrantConfinesTheCapture(t *testing.T) {
	for _, tc := range []struct {
		name     string
		grants   bool
		wantLand bool
	}{
		{"read grants on", true, false},
		{"read grants off", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			e := newCaptureEnvWith(t, config.PlanCapture{Enabled: true}, func(c *Config) { c.ReadGrants = tc.grants })
			require.NoError(t, store.SetSetting(ctx, e.db, store.SettingRepoProjectMap,
				`{"/work/demo":"demo","/work/other":"other"}`))
			_, err := store.EnsureProject(ctx, e.db, "example-finance", "example-finance")
			require.NoError(t, err)
			require.NoError(t, store.SetProjectIsolation(ctx, e.db, "example-finance", core.IsolationConfidential))
			e.startSession(t)
			sess, ok, err := store.AmbientSessionByExternalIdentity(ctx, e.db, ClientClaudeCode.externalIdentity(), testSID)
			require.NoError(t, err)
			require.True(t, ok)
			approveGrant(t, e, "demo", "example-finance", sess.ID, "")

			e.planWrite(t, "Write", e.writePlanFile(t, "home-plan", "# Home\n\nStep one.\n"))
			_, found := e.loadNote(t, "cc-plan-home-plan")
			require.Equal(t, tc.wantLand, found, "a capture into the asking project")

			resp, out := post(t, e.ts.URL+"/api/hooks/post-tool-use", testKey, map[string]any{
				"session_id": testSID, "cwd": "/work/other", "tool_name": "Write",
				"tool_input": map[string]any{"file_path": e.writePlanFile(t, "away-plan", "# Away\n\nStep one.\n")},
			})
			require.Equal(t, http.StatusOK, resp.StatusCode)
			_, away, err := store.NoteBySlug(ctx, e.db, "other", "cc-plan-away-plan")
			require.NoError(t, err)
			require.Equal(t, tc.wantLand, away)
			raw, err := json.Marshal(out)
			require.NoError(t, err)
			const notice = "Seamless did not record this plan: this session holds content from example-finance, " +
				"so it writes only into example-finance, and this would land in project other."
			if tc.wantLand {
				require.NotContains(t, string(raw), "did not record")
			} else {
				require.Contains(t, string(raw), notice)
			}
		})
	}
}

// Under peer_auth the holder is the agent's process tree, so a pushed capture
// is judged by the chain, and only the tree the grant names is confined.
func TestFenceNotePush_ReadGrantConfinesTheProcessTree(t *testing.T) {
	ctx := context.Background()
	e := newCaptureEnvWith(t, config.PlanCapture{Enabled: true}, nil)
	h := NewHandler(Config{DB: e.db, ReadGrants: true})

	policy, err := access.New(config.PeerAuth{Enabled: true, Owner: "owner"}, func(n string) (access.Account, error) {
		if n == "owner" {
			return access.Account{UID: 501, Name: "owner"}, nil
		}
		return access.Account{}, fmt.Errorf("no account %s", n)
	})
	require.NoError(t, err)
	holder := access.ProcessToken{PID: 200, Version: 2000, Boot: "6c2f1a3e-0b7d-4c55-9a51-3d8f00a1b2c3"}
	stranger := access.ProcessToken{PID: 300, Version: 3000, Boot: holder.Boot}
	approveGrant(t, e, "demo", "example-finance", "", holder.Key())
	_, err = store.EnsureProject(ctx, e.db, "example-finance", "example-finance")
	require.NoError(t, err)
	require.NoError(t, store.SetProjectIsolation(ctx, e.db, "example-finance", core.IsolationConfidential))

	view := func(token access.ProcessToken) context.Context {
		v, ok := policy.PeerChainView(501, []access.ProcessToken{token})
		require.True(t, ok)
		return access.WithView(ctx, v)
	}

	project, verdict := h.fenceNotePush(view(holder), ClientClaudeCode, reachOK, "", "", captureRoute{project: "example-finance"}, "this subagent's report")
	require.True(t, verdict.ok, "the target it holds takes the capture")
	require.Equal(t, "example-finance", project)

	for _, where := range []string{"demo", "other"} {
		_, verdict = h.fenceNotePush(view(holder), ClientClaudeCode, reachOK, "", "", captureRoute{project: where}, "this subagent's report")
		require.False(t, verdict.ok, where)
		require.Equal(t, "Seamless did not record this subagent's report: this agent holds content from example-finance, "+
			"so it writes only into example-finance, and this would land in project "+where+".", verdict.notice)
	}

	_, verdict = h.fenceNotePush(view(stranger), ClientClaudeCode, reachOK, "", "", captureRoute{project: "other"}, "this subagent's report")
	require.True(t, verdict.ok, "another process tree holds no grant")
}
