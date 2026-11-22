package console

import (
	"context"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// UX 16 item 7: the embedder card says what to do in Your setup's words,
// offers Switch off only while there is something to switch off, and follows
// a re-embed pass in place.

// swEmbedPage renders Knowledge engine for a daemon that resolved rt at start,
// with the stored switch off when modeOff.
func swEmbedPage(t *testing.T, rt EmbeddingRuntime, modeOff bool) string {
	t.Helper()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	if modeOff {
		require.NoError(t, store.SetEmbedderMode(context.Background(), db, store.EmbedderModeOff))
	}
	svc, err := New(Config{DB: db, APIKey: testKey, BriefingCfg: config.Defaults().Briefing, Embedding: rt})
	require.NoError(t, err)
	mux := http.NewServeMux()
	svc.Register(mux)
	return getHTMLBody(t, mux, "/console/settings?s=engine")
}

const swSwitchOff = `<button class="btn small" type="submit" data-confirm="Switch embeddings off? Recall and search degrade to lexical-only after the next restart." data-confirm-label="Switch it off">Switch off</button>`

func TestSwEmbedderCardOffersOnlyWhatCanChange(t *testing.T) {
	const fix = "Search and recall use keywords only. Add an OpenAI key (or point Ollama at a local model) in the config file, then restart."
	for _, tc := range []struct {
		name              string
		rt                EmbeddingRuntime
		modeOff           bool
		switchOff, enable bool
		has, hasnt        []string
	}{
		{name: "running", rt: EmbeddingRuntime{Enabled: true, Provider: "openai", Model: "text-embedding-3-large"},
			switchOff: true},
		{name: "no key", rt: EmbeddingRuntime{Provider: "openai", Model: "text-embedding-3-large",
			Reason: "llm.NewEmbedder: openai selected but api_key is empty"},
			has: []string{fix}, hasnt: []string{"api_key is empty", "No usable embedding provider"}},
		{name: "a config mistake", rt: EmbeddingRuntime{Provider: "openai", Reason: "llm.NewEmbedder: base_url is not a URL", Misconfigured: true},
			has: []string{"needs a fix in the config file: <span class=\"mono\">llm.NewEmbedder: base_url is not a URL</span>"}, hasnt: []string{fix}},
		{name: "switched off here", rt: EmbeddingRuntime{Provider: "openai", Reason: "switched off in the console Settings", OverriddenOff: true},
			modeOff: true, enable: true, has: []string{"Switched off from this console"}},
		{name: "switch cleared, restart pending", rt: EmbeddingRuntime{Provider: "openai", Reason: "switched off in the console Settings", OverriddenOff: true},
			switchOff: true, has: []string{"restart the daemon to apply it"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page := swEmbedPage(t, tc.rt, tc.modeOff)
			card := page[strings.Index(page, `<div class="set-row eng-recall">`):]
			card = card[:strings.Index(card, `<div class="set-row eng-vectors"`)]
			require.Equal(t, tc.switchOff, strings.Contains(card, swSwitchOff), "Switch off")
			require.Equal(t, tc.enable, strings.Contains(card, ">Enable embeddings</button>"), "Enable")
			require.Equal(t, tc.switchOff || tc.enable, strings.Contains(card, `action="/console/settings/embeddings/mode"`),
				"no form without a button")
			for _, s := range tc.has {
				require.Contains(t, card, s)
			}
			for _, s := range tc.hasnt {
				require.NotContains(t, card, s)
			}
		})
	}
}

// swGateEmbedder embeds once its gate is open, so a re-embed pass stays
// under way while the test looks at the page.
type swGateEmbedder struct{ gate chan struct{} }

func (e swGateEmbedder) Model() string { return "sw-model" }
func (e swGateEmbedder) Embed(ctx context.Context, _ string) ([]float32, error) {
	select {
	case <-e.gate:
	case <-ctx.Done():
	}
	return []float32{1, 0, 0}, nil
}

// A pass under way marks the Stored vectors card, which the page's script
// follows in place (no "refresh the page"); once the pass ends the mark goes,
// and with it the following.
func TestSwReembedProgressFollowsThePass(t *testing.T) {
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	mgr, err := files.NewManager(dir, db, slog.Default())
	require.NoError(t, err)
	now := time.Now().UTC()
	_, err = mgr.WriteMemory(ctx, core.Memory{ID: "01K0SWMEMORY00000000000000", Kind: core.KindGotcha, Name: "sw-alpha",
		Description: "d", Project: "seam", Body: "b\n", Created: now, Updated: now, ValidFrom: now})
	require.NoError(t, err)
	gate := make(chan struct{})
	mgr.SetEmbedder(swGateEmbedder{gate: gate})
	opened := false
	open := func() {
		if !opened {
			opened = true
			close(gate)
		}
	}
	t.Cleanup(func() { open(); _ = mgr.Close() })

	svc, err := New(Config{DB: db, Files: mgr, APIKey: testKey, BriefingCfg: config.Defaults().Briefing,
		Embedding: EmbeddingRuntime{Enabled: true, Provider: "openai", Model: "sw-model"}})
	require.NoError(t, err)
	mux := http.NewServeMux()
	svc.Register(mux)

	_, err = mgr.StartReembed(ctx)
	require.NoError(t, err)
	page := getHTMLBody(t, mux, "/console/settings?s=engine")
	require.Contains(t, page, `<div class="set-row eng-vectors" data-vectors-card data-reembed-running>`)
	require.Contains(t, page, "re-embedding 0/1")
	require.Contains(t, page, "The count follows the pass while this page is open.")
	require.NotContains(t, page, "Refresh the page")
	// The follower: only the card, in place, never a reload.
	require.Contains(t, page, "document.querySelector('[data-vectors-card][data-reembed-running]')")
	require.Contains(t, page, "window.SeamConsole.morph(card, fresh);")
	require.NotContains(t, page, "location.reload")

	open()
	deadline := time.Now().Add(5 * time.Second)
	for mgr.ReembedStatus().Running && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	require.False(t, mgr.ReembedStatus().Running)
	page = getHTMLBody(t, mux, "/console/settings?s=engine")
	require.Contains(t, page, `<div class="set-row eng-vectors" data-vectors-card>`)
	require.Contains(t, page, `data-confirm="Re-embed every memory and note with the active model? This calls the embedding provider once per item." data-confirm-label="Re-embed all"`)
	require.Contains(t, page, ": 1/1 items.")
}
