package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// testUpdateNotice is the example notice from the update plan's design: parsed
// versions and an owner-action command.
const testUpdateNotice = "Seamless v0.7.3 is available (running v0.7.2). Owner action, not a task for this session: brew upgrade --cask arctop/tap/seamless"

// The Codex hook cap (truncateCodexContext) keeps a prefix of an oversized
// briefing and carries the ambient "Seam session:" line over as a preserved
// suffix. The update notice is pinned at the head -- under the header line, and
// under the isolation line where one renders -- so an over-cap Codex
// SessionStart keeps both lines: the cap removes the middle, never the notice.
// The hook also hands the provider the host it resolved for the session, which
// for a local agent on a daemon that has named its machine is that name, not "".
func TestSessionStart_CodexCapKeepsUpdateNoticeAndAmbientLine(t *testing.T) {
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	ctx := context.Background()
	require.NoError(t, store.SetSetting(ctx, db,
		store.SettingRepoProjectMap, `{"/work/demo":"demo"}`))
	// A live daemon names its machine (config.Hostname), so a hook that sends no
	// host identity speaks for that name.
	require.NoError(t, store.AdoptLocalHost(ctx, db, localHostName))

	// The seeding of TestSessionStart_CodexCapsPinnedContextAfterAmbientLine: a
	// tiered constraint head plus 95 pinned plan rollups overflow the cap.
	for i := range 25 {
		insertMemory(t, db, fmt.Sprintf("01C%03d", i), "constraint",
			fmt.Sprintf("pinned-constraint-%03d", i), strings.Repeat("bounded pinned detail ", 12), "demo")
	}
	now := time.Now().UTC()
	for i := range 95 {
		require.NoError(t, store.CreateTask(ctx, db, core.Task{
			ID:          fmt.Sprintf("01T%03d", i),
			ProjectSlug: "demo",
			Title:       fmt.Sprintf("plan step %03d", i),
			Status:      core.TaskOpen,
			PlanSlug:    fmt.Sprintf("codex-notice-plan-%03d-%s", i, strings.Repeat("x", 32)),
			CreatedAt:   now,
			UpdatedAt:   now,
		}))
	}

	ret := retrieve.New(db, nil, config.Budgets{
		MaxBriefingTokens:  1500,
		RecallBudgetTokens: 1000,
	}, nil)
	// The hook serves the request on this goroutine (mux.ServeHTTP below), so
	// the provider's host log needs no lock.
	var hosts []string
	ret.SetUpdateNotice(func(_ context.Context, host string) string {
		hosts = append(hosts, host)
		return testUpdateNotice
	})
	h := NewHandler(Config{
		DB: db, Retrieve: ret, Events: events.NewRecorder(db), APIKey: testKey,
		LocalHost: localHostName,
	})
	mux := http.NewServeMux()
	h.Register(mux)
	body, err := json.Marshal(map[string]any{
		"session_id": "019f7291-40f1-7311-8997-0d497579d27b",
		"cwd":        "/work/demo",
		"source":     "startup",
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost,
		"/api/hooks/session-start?client=codex", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+testKey)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	var response hookResponse
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&response))
	require.NotNil(t, response.HookSpecificOutput)
	emitted := response.HookSpecificOutput.AdditionalContext

	// The cap fired: the assembled briefing was over it, the emitted one is not.
	injected := eventsOfKind(t, events.NewRecorder(db), core.EventInjected)
	require.Len(t, injected, 1)
	require.Equal(t, emitted, injected[0].Payload["content"])
	require.Equal(t, true, injected[0].Payload["truncated"])
	require.Greater(t, injected[0].Payload["original_estimated_tokens"].(float64),
		float64(codexContextMaxTokens))
	require.LessOrEqual(t, retrieve.EstimateTokens(emitted), codexContextMaxTokens)
	require.Contains(t, emitted, contextTruncationMarker)

	// Both lines survive it: the notice still in its slot under the header,
	// ahead of the cut, and the ambient line after it.
	require.Contains(t, emitted,
		"recent findings.\n"+testUpdateNotice+"\n\nConstraints (binding for every session):\n")
	require.Contains(t, emitted, "\nSeam session: cx/019f7291-")
	require.True(t, strings.HasSuffix(emitted, "</seam-briefing>"))
	notice := strings.Index(emitted, testUpdateNotice)
	marker := strings.Index(emitted, contextTruncationMarker)
	ambient := strings.Index(emitted, "\nSeam session: ")
	require.Less(t, notice, marker, "the notice rides in the preserved prefix")
	require.Less(t, marker, ambient, "the ambient line rides in the preserved suffix")

	require.Equal(t, []string{localHostName}, hosts,
		"asked once, for the daemon's own host by name -- a provider must read it as local")
}
