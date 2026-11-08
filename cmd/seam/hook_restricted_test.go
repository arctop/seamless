package main

import (
	"fmt"
	"net/http"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// A restricted local account (peer_auth.restricted) runs the same `seam hook`
// as the owner: a config whose server_url names the peer_auth socket and holds
// no key, nothing account-specific on the client. The kernel names the caller
// (here the stand-in daemon attaches the restricted account's View, as
// peerHandler would for its uid), the hooks push their captures, and every one
// lands in the account's own sessions and notes, labeled with its name.
func TestRunHook_RestrictedAccountOnTheSocketNeedsNoKey(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the peer_auth socket is a Unix deployment")
	}
	const acct = "_example_agent"
	f := newCaptureFixture(t)
	d := captureDaemon(t, f)
	policy, err := access.New(config.PeerAuth{Owner: "owner", Restricted: []string{acct}},
		func(n string) (access.Account, error) {
			switch n {
			case "owner":
				return access.Account{UID: 501, Name: "owner"}, nil
			case acct:
				return access.Account{UID: 502, Name: acct}, nil
			}
			return access.Account{}, fmt.Errorf("no %s", n)
		})
	require.NoError(t, err)
	sock := serveSocket(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v, ok := policy.PeerView(502)
		open, err := store.OpenProjectSlugs(r.Context(), d.db)
		if !ok || err != nil {
			http.Error(w, "no view", http.StatusServiceUnavailable)
			return
		}
		d.mux.ServeHTTP(w, r.WithContext(access.WithView(r.Context(), v.WithOpenProjects(open))))
	}))
	cfg := socketConfig(sock)
	require.Empty(t, cfg.MCP.APIKey, "a restricted account holds no bearer key")
	require.True(t, pushesCaptures(cfg))

	for _, c := range f.calls() {
		require.Empty(t, runHookCall(t, cfg, c), "%s (%s): no refusal, no error", c.event, c.client)
	}

	// Its sessions live in its own namespace (SEAL A2): the agent's id and
	// the session's name both carry the account.
	cc := d.ambient(t, "claude-code", access.QualifiedSessionName(acct, "cc-1"))
	require.Equal(t, acct, cc.Account)
	require.Regexp(t, `^`+acct+`:cc/cc-1-[0-9a-f]+$`, cc.Name)
	require.Equal(t, "example-ops", cc.ProjectSlug)
	require.Equal(t, core.SessionCompleted, cc.Status)
	require.Equal(t, "(auto-harvested) Deployed the ops service; the health check race is fixed.", cc.Findings)
	require.Equal(t, "claude-late", cc.Model)
	require.False(t, cc.Tokens.Empty())

	cx := d.ambient(t, "codex", access.QualifiedSessionName(acct, "cx-1"))
	require.Equal(t, acct, cx.Account)
	require.NotEmpty(t, cx.Findings)
	require.False(t, cx.Tokens.Empty())

	for _, slug := range []string{"cc-plan-example-plan", "cc-agent-a1"} {
		n := d.note(t, slug)
		require.Equal(t, acct, n.Account, slug)
		require.Contains(t, n.Body, "git "+captureHead[:12], "%s: the pushed head", slug)
	}
}
