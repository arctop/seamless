package a2a

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// A request here speaks for no session, so under peer_auth it reads no
// confidential or sealed project, whichever transport it came in on. A lease
// opens a project, and with peer_auth off nothing changes.
func TestMessageSend_PeerAuthFencesConfidentialProjects(t *testing.T) {
	const key = "a2a-test-key"
	ctx := context.Background()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	for slug, iso := range map[string]core.Isolation{
		"example-ops": core.IsolationOpen, "example-finance": core.IsolationConfidential,
		"example-vault": core.IsolationSealed, "example-locked": core.IsolationConfidential,
	} {
		_, err := store.EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
		require.NoError(t, store.SetProjectIsolation(ctx, db, slug, iso))
		insertRecallMemory(t, db, "01"+slug, slug+"-ledger-note", "ledger reconcile note", slug, "")
	}

	policy, err := access.New(config.PeerAuth{Enabled: true, Owner: "owner", ForceLocked: []string{"example-locked"}},
		func(n string) (access.Account, error) {
			if n == "owner" {
				return access.Account{UID: 501, Name: "owner"}, nil
			}
			return access.Account{}, fmt.Errorf("no %s", n)
		})
	require.NoError(t, err)
	srv, err := New(Config{Retrieve: retrieve.New(db, nil, config.Budgets{MaxBriefingTokens: 1500, RecallBudgetTokens: 1000}, nil),
		Events: events.NewRecorder(db), APIKey: key, Version: "1.2.3", Endpoint: "http://127.0.0.1:8081/api/a2a"})
	require.NoError(t, err)
	serve := func(view func() access.View) string {
		h := srv.Handler()
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if view != nil {
				r = r.WithContext(access.WithView(r.Context(), view()))
			}
			h.ServeHTTP(w, r)
		}))
		t.Cleanup(ts.Close)
		return ts.URL
	}
	agent := access.ProcessToken{PID: 200, Version: 2000, Boot: "6c2f1a3e-0b7d-4c55-9a51-3d8f00a1b2c3"}
	socket := func() access.View {
		v, ok := policy.PeerChainView(501, []access.ProcessToken{agent})
		require.True(t, ok)
		return v
	}

	recall := func(url, project string) (bool, string) {
		result, rpcErr := rpc(t, url, key, sendBody("ledger reconcile", map[string]any{"project": project}))
		if rpcErr != nil {
			require.Nil(t, result)
			return false, rpcErr.Message
		}
		require.Contains(t, string(result), project+"-ledger-note")
		return true, ""
	}

	off := serve(nil)
	for _, project := range []string{"example-ops", "example-finance", "example-vault"} {
		ok, msg := recall(off, project)
		require.True(t, ok, "peer_auth off is as before: %s", msg)
	}

	for name, url := range map[string]string{
		"owner on TCP":        serve(func() access.View { return policy.OwnerView() }),
		"owner on the socket": serve(socket),
	} {
		t.Run(name, func(t *testing.T) {
			ok, msg := recall(url, "example-ops")
			require.True(t, ok, "an open project is still readable: %s", msg)
			for project, state := range map[string]string{"example-finance": "confidential", "example-vault": "sealed"} {
				ok, msg := recall(url, project)
				require.False(t, ok)
				require.Contains(t, msg, "project "+project+" is "+state+": reads require a session bound to it, and A2A speaks for no session")
				require.NotContains(t, msg, "ledger-note", "a refusal carries nothing from inside")
			}
		})
	}

	leased := serve(func() access.View {
		now := time.Now()
		return socket().WithLeases(now, []access.Lease{{Project: "example-locked", Process: agent.Key(), Expires: now.Add(time.Hour)}})
	})
	ok, msg := recall(leased, "example-locked")
	require.True(t, ok, "a lease opens the project it names: %s", msg)
}
