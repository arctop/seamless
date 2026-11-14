package console

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

func privacyJSON(t *testing.T, mux *http.ServeMux) privacyData {
	t.Helper()
	rec := getPeek(t, mux, "/console/privacy?format=json")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var d privacyData
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &d))
	return d
}

func TestPrivacy_FourFencesAlwaysShow(t *testing.T) {
	_, mux := newConsole(t)
	d := privacyJSON(t, mux)
	require.Len(t, d.Fences, 4)
	for i, key := range []string{fenceOpen, fenceConf, fenceSealed, fenceLocked} {
		f := d.Fences[i]
		require.Equal(t, key, f.Key)
		require.NotNil(t, f.Projects, "%s lists its projects, even when there are none", key)
		require.NotEmpty(t, f.Empty, "%s says what an empty fence means", key)
		require.NotEmpty(t, f.How, "%s says how to put a project there", key)
		require.NotEmpty(t, f.Code, key)
	}
	require.Equal(t, "shared", d.Mode, "no peer_auth: Seamless runs as the owner")
	require.NotEmpty(t, d.Fences[3].Needs, "a lock needs the daemon in its own account")
	require.Len(t, d.Layers, 5)
	require.Len(t, d.Steps, 6)

	page := getPeek(t, mux, "/console/privacy")
	require.Equal(t, http.StatusOK, page.Code)
	body := page.Body.String()
	for _, key := range []string{fenceOpen, fenceConf, fenceSealed, fenceLocked} {
		require.Contains(t, body, `data-fence-art="`+key+`"`, "the %s card renders, empty or not", key)
	}
	require.Contains(t, body, "Needs Seamless in its own account")
	require.Contains(t, body, `<script src="/console/static/privacy.js"></script>`)
}

func TestPrivacy_PlacesEachProjectInItsFence(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	for slug, state := range map[string]core.Isolation{
		"web-app": core.IsolationOpen, "client-work": core.IsolationConfidential, "clean-room": core.IsolationSealed,
	} {
		_, err := store.EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
		if state != core.IsolationOpen {
			require.NoError(t, store.SetProjectIsolation(ctx, db, slug, state))
		}
	}
	d := privacyJSON(t, mux)
	slugs := func(key string) []string {
		var out []string
		for _, p := range d.Fence(key).Projects {
			out = append(out, p.Slug)
			require.Equal(t, "/console/projects/"+p.Slug, p.Href)
		}
		return out
	}
	require.Equal(t, []string{"web-app"}, slugs(fenceOpen))
	require.Equal(t, []string{"client-work"}, slugs(fenceConf))
	require.Equal(t, []string{"clean-room"}, slugs(fenceSealed))
	require.Empty(t, slugs(fenceLocked))
	require.Equal(t, 2, d.Fenced)
	require.True(t, d.Layers[0].On, "fenced projects turn the first layer on")
}

func TestPrivacy_PromiseBodyDropsTheName(t *testing.T) {
	require.Equal(t, "Nothing leaves. Agents in other projects never see this project's knowledge, and agents here cannot write outside it.",
		promiseBody(core.IsolationConfidential))
	require.Equal(t, "Shares normally. Global knowledge flows in; family surfaces may include it.", promiseBody(core.IsolationOpen))
}

// p11PrivacyMux is a console whose daemon runs in its own account
// (peer_auth), with access requests (isolation.requests) on or off.
func p11PrivacyMux(t *testing.T, requests bool) *http.ServeMux {
	t.Helper()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	rec := events.NewRecorder(db)
	cfg := Config{DB: db, Events: rec, APIKey: testKey, Level: "advanced",
		Deployment: Deployment{PeerAuth: true, Owner: "alice", DaemonUser: "_seamless"}}
	if requests {
		cfg.Grants = approval.NewGrants(approval.GrantsConfig{DB: db, Events: rec, LeaseCap: 8 * time.Hour})
	}
	cs, err := New(cfg)
	require.NoError(t, err)
	mux := http.NewServeMux()
	cs.Register(mux)
	return mux
}

// The binding limit the Privacy page states in its own-account setup: with
// access requests off, word for word what it always said; on, that an agent
// Seamless identifies asks first, and what still lets an agent work in a
// fenced project as its own (an agent started in its repository, a client
// that does not identify its conversation), so a lock is still the answer.
func TestPrivacy_P11BindingLimitFollowsTheSwitch(t *testing.T) {
	const off = "A confidential or sealed fence keeps a careful agent out, but an agent running as you can name that " +
		"project as its own, so lock what must stay shut.</p>"
	page := func(requests bool) string {
		rec := getPeek(t, p11PrivacyMux(t, requests), "/console/privacy")
		require.Equal(t, http.StatusOK, rec.Code)
		return rec.Body.String()
	}
	require.Contains(t, page(false), "There a lock holds against any agent. "+off)

	on := page(true)
	require.NotContains(t, on, off)
	require.Contains(t, on, "There a lock holds against any agent. A confidential or sealed fence keeps a careful agent out, "+
		"and with access requests on an agent Seamless identifies asks you before it works in one. But an agent running as "+
		"you can still start in that project&rsquo;s repository, or use a client that does not identify its conversation, "+
		"and work there as its own, so lock what must stay shut.</p>")
}

func TestLoopbackAddr(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:8081": true, "localhost:8081": true, "[::1]:8081": true,
		"0.0.0.0:8081": false, ":8081": false, "100.64.1.2:8081": false, "": false,
	} {
		require.Equal(t, want, loopbackAddr(addr), addr)
	}
}
