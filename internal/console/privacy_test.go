package console

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
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

func TestLoopbackAddr(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:8081": true, "localhost:8081": true, "[::1]:8081": true,
		"0.0.0.0:8081": false, ":8081": false, "100.64.1.2:8081": false, "": false,
	} {
		require.Equal(t, want, loopbackAddr(addr), addr)
	}
}
