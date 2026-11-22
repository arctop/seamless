package console

import (
	"bytes"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/backup"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/store/storetest"
)

// upSetupConsole is a console whose Your setup has every row: a config file,
// a data folder, an update check, a backup service, and a daemon running as
// an account of its own.
func upSetupConsole(t *testing.T) (*Service, *http.ServeMux) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "seam.db")
	db, err := storetest.OpenFresh(t, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	now := time.Now()
	st := releaseStatus(now)
	on := true
	fb := &fakeBackups{base: config.Backup{Auto: &on}, db: db, dataDir: t.TempDir()}
	fb.status.Settings = backup.Effective(fb.base, config.BackupOverride{}, false, fb.dataDir)
	fb.status.LastBackup = &backup.Record{At: now.Add(-2 * time.Hour)}
	fb.status.LastConfirmed = now.Add(-2 * time.Hour)
	svc, err := New(Config{
		DB: db, Events: events.NewRecorder(db), APIKey: testKey, Level: "basic", Version: "0.7.2",
		DataDir: "/home/owner/.seamless", DBPath: dbPath, ConfigPath: "/home/owner/.config/seamless/seamless.yaml",
		GardenerCfg: config.Gardener{Enabled: true, IntervalMinutes: 60},
		Embedding:   EmbeddingRuntime{Provider: "openai", Reason: "llm.NewEmbedder: openai selected but api_key is empty"},
		Updates:     &fakeUpdates{db: db, status: st},
		Backups:     fb,
		Deployment:  Deployment{PeerAuth: true, Owner: "owner", DaemonUser: "_seamless"},
	})
	require.NoError(t, err)
	mux := http.NewServeMux()
	svc.Register(mux)
	return svc, mux
}

// Your setup is one card of rows: each value a chip or a short path with a
// Copy button for the whole one, the detail one hint away, and each row that
// can change linking to the section where it does -- Updates, Knowledge
// engine for semantic recall, Backups, and the Gardener's own section.
func TestUpSetup_OneCardOfRows(t *testing.T) {
	_, mux := upSetupConsole(t)
	page := getPeek(t, mux, "/console/settings?s=setup").Body.String()
	sec := regexp.MustCompile(`(?s)<section class="settings-zone settings-section" id="setup">.*?</section>`).FindString(page)
	require.NotEmpty(t, sec)
	require.Equal(t, 1, strings.Count(sec, `<dl class="card setup-card">`), "one card")
	require.Equal(t, 8, strings.Count(sec, `<div class="setup-row">`), "version, host, config, data, agents, recall, backups, gardener")
	require.NotContains(t, sec, "setup-facts", "no box per fact")

	// The paths: whole in the title and the Copy button.
	require.Contains(t, sec, `<span class="mono setup-path" title="/home/owner/.config/seamless/seamless.yaml">`)
	require.Contains(t, sec, `data-copy="/home/owner/.config/seamless/seamless.yaml"`)
	require.Contains(t, sec, `<a class="setup-go" href="vscode://file/home/owner/.config/seamless/seamless.yaml">Open in editor</a>`)
	require.Contains(t, sec, `<span class="mono setup-path" title="/home/owner/.seamless">`)
	require.Contains(t, sec, `data-copy="/home/owner/.seamless"`)
	require.Regexp(t, `<span class="faint" title="[^"]*seam\.db">database [0-9.]+ [KMG]?B</span>`, sec, "the database's size, its path in the title")
	require.NotContains(t, sec, "<dt>Database</dt>")

	// The chips: the Updates and Backups sections' own state chips.
	require.Contains(t, sec, `<dt>Version</dt><dd><span class="mono">0.7.2</span><span class="badge compact ok">Up to date</span>`)
	require.Contains(t, sec, `<span class="badge compact ok">2h ago</span><span class="faint">every day, 7 kept</span>`)
	require.Contains(t, sec, `<span class="badge compact warn">keywords only</span>`)
	require.Contains(t, sec, "Add an OpenAI key (or point Ollama at a local model) in the config file, then restart.", "the fix, in a hint")
	require.NotContains(t, sec, "api_key is empty", "the daemon's error stays in its log")
	require.Contains(t, sec, `<span class="badge compact quiet">agents apply</span>`)
	require.Contains(t, sec, "The gardener suggests cleanups every 60 minutes.")
	require.Contains(t, sec, "as its own account, <span class=\"mono\">_seamless</span>")
	require.Contains(t, sec, "<span>None yet</span>")

	// Each row that can change links where it does.
	var links []string
	for _, m := range regexp.MustCompile(`<a class="setup-go" href="(/console/[^"]+)"`).FindAllStringSubmatch(sec, -1) {
		links = append(links, m[1])
	}
	require.Equal(t, []string{"/console/settings?s=updates", "/console/settings?s=engine", "/console/settings?s=backups",
		"/console/settings?s=gardener"}, links)
}

// A row with nothing set says so in a word, with the rest in its hint; a
// daemon that is the owner's own account names no account.
func TestUpSetup_EmptyRows(t *testing.T) {
	_, mux := newConsoleLevel(t, config.Features{}, "basic")
	sec := getPeek(t, mux, "/console/settings?s=setup").Body.String()
	require.Contains(t, sec, `<dt>Config file</dt><dd><span>None</span>`)
	require.Contains(t, sec, "This daemon runs on its defaults and environment variables.")
	require.Contains(t, sec, "An agent appears here once it starts a session with the hooks installed.")
	require.NotContains(t, sec, "as its own account")
	require.NotContains(t, sec, "<dt>Backups</dt>", "no backup service, no row")
	require.NotRegexp(t, `<dt>Version</dt><dd><span class="mono">[^<]*</span><span class="badge`, sec, "no update check, no chip")
}

// The hint's fix is the engine's own sentence, word for word.
func TestUpSetup_RecallFixIsTheEnginesSentence(t *testing.T) {
	svc, _ := upSetupConsole(t)
	var b bytes.Buffer
	require.NoError(t, svc.pages["settings"].ExecuteTemplate(&b, "embed-off-fix", nil))
	require.Equal(t, b.String(), embedOffFix)

	for _, tc := range []struct {
		e    EmbeddingRuntime
		want setupRecall
	}{
		{EmbeddingRuntime{Enabled: true, Model: "text-embedding-3-small"}, setupRecall{Word: "semantic", Tone: "ok", Model: "text-embedding-3-small"}},
		{EmbeddingRuntime{}, setupRecall{Word: "keywords only", Tone: "warn", Fix: embedOffFix}},
		{EmbeddingRuntime{OverriddenOff: true}, setupRecall{Word: "keywords only", Tone: "warn",
			Fix: "Switched off in this console. Search and recall use keywords only."}},
		{EmbeddingRuntime{Misconfigured: true, Reason: "llm.model is empty"}, setupRecall{Word: "keywords only", Tone: "warn",
			Fix: "The model settings in the config file have a mistake: llm.model is empty"}},
	} {
		require.Equal(t, tc.want, settingsData{Embeddings: embeddingsPanel{EmbeddingRuntime: tc.e}}.SetupRecall())
	}
}

// The Gardener row's chips: suggestions off, who applies, how many kinds
// apply by themselves; the cadence and the kinds wait in its hint.
func TestUpSetup_GardenerChips(t *testing.T) {
	d := settingsData{
		Gardener:         config.Gardener{Enabled: false, IntervalMinutes: 30},
		GardenerSwitches: gardenerPanel{gardenerSwitches: gardenerSwitches{AutoApply: []string{"abandon_plan", "ship_plan"}}},
	}
	g := d.SetupGardener()
	require.Equal(t, []sectionState{{Word: "suggestions off", Tone: "quiet"}, {Word: "only you apply", Tone: "quiet"},
		{Word: "2 by themselves", Tone: "quiet"}}, g.Chips)
	require.Equal(t, "No background suggestions are made; upkeep still runs every 30 minutes (idle sessions reaped, old tool events pruned, "+
		"retrieval stats refreshed). Applied by themselves: abandon plan, ship plan.", g.Hint)
}

// A path a row prints: the home folder reads ~, and a long one keeps its
// first folder and its last names around an ellipsis.
func TestUpShortPath(t *testing.T) {
	home := filepath.FromSlash("/home/owner")
	for _, tc := range []struct{ in, want string }{
		{"/home/owner/.config/seamless/seamless.yaml", "~/.config/seamless/seamless.yaml"},
		{"/home/owner", "~"},
		{"/home/owner2/data", "/home/owner2/data"},
		{"/opt/seamless/data", "/opt/seamless/data"},
		{"/home/owner/Library/Mobile Documents/com~apple~CloudDocs/Seamless backups", "~/…/com~apple~CloudDocs/Seamless backups"},
		{"/Volumes/Backup Disk/Seamless/backups/2026/november", "/Volumes/…/2026/november"},
		{"/Volumes/a/b/c/an-extremely-long-folder-name/and-another-long-folder-name", "/Volumes/…/and-another-long-folder-name"},
		{"/a-folder-name-that-is-long/second-folder-name-also-long", "/a-folder-name-that-is-long/second-folder-name-also-long"},
	} {
		require.Equal(t, filepath.FromSlash(tc.want), shortPath(filepath.FromSlash(tc.in), home), tc.in)
	}
	require.Equal(t, filepath.FromSlash("/home/owner/x"), shortPath(filepath.FromSlash("/home/owner/x"), ""), "no home, no ~")
	require.Equal(t, pathView{}, viewPath(""))
}
