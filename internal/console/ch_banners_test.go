package console

import (
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
	"github.com/arctop/seamless/internal/update"
)

// chTroubleConsole is a console whose automatic updates and automatic
// backups are both in trouble: the update to v0.7.3 rolled back, and the
// last two backups failed.
func chTroubleConsole(t *testing.T) *http.ServeMux {
	t.Helper()
	now := time.Now()
	st := autoStatus(now)
	withNewer(&st, now)
	st.LastAttempt = &update.AttemptResult{From: ver(0, 7, 2), To: ver(0, 7, 3), Why: update.WhyAuto,
		Outcome: update.OutcomeRolledBack, Stage: update.StageRollback, RolledBack: true,
		Error: attemptErrorText, LogPath: "/data/update/logs/x.log", FoldedAt: now.Add(-time.Hour)}
	st.Blocked = []update.Block{{Version: ver(0, 7, 3), Reason: update.BlockRolledBack, At: now.Add(-time.Hour)}}

	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	on := true
	dataDir := t.TempDir()
	fb := &fakeBackups{db: db, dataDir: dataDir, base: config.Backup{Auto: &on}, digest: strings.Repeat("ab", 32)}
	fb.status.Settings = backup.Effective(fb.base, config.BackupOverride{}, fb.release, dataDir)
	fb.status.Failures, fb.status.FailingSince = 2, now.Add(-49*time.Hour)
	fb.status.LastReason, fb.status.LastError = "folder-disconnected", "/Volumes/T7 private path"

	svc, err := New(Config{DB: db, Events: events.NewRecorder(db), APIKey: testKey, Level: "basic", Version: "0.7.2",
		DataDir: dataDir, Backups: fb, Updates: &fakeUpdates{db: db, base: autoBase, status: st}})
	require.NoError(t, err)
	mux := http.NewServeMux()
	svc.Register(mux)
	return mux
}

// While both are in trouble, the update and backup alerts are one row: a
// line each, each line's link, and one "Got it" that names both keys. Each
// line and its link carry their own key, so one set aside earlier stays
// hidden. The words are the alerts' own: parsed versions and fixed text,
// never an attempt's or a run's error text.
func TestChAlertsShareOneRowWhenBothShow(t *testing.T) {
	mux := chTroubleConsole(t)
	for _, path := range []string{"/console/", "/console/memories", "/console/settings?s=experience"} {
		page := getPeek(t, mux, path).Body.String()
		require.NotContains(t, page, `id="update-alert"`, path)
		require.NotContains(t, page, `id="backup-alert"`, path)
		require.Equal(t, 1, strings.Count(page, `class="section notice update-alert`), "%s: one alert row", path)

		row := regexp.MustCompile(`(?s)<div class="section notice update-alert alert-pair[^"]*" id="update-backup-alert" role="alert" data-update-banner="([^"]*)">.*?</button>\s*</div>\s*</div>`).FindStringSubmatch(page)
		require.Len(t, row, 2, "%s: the shared row", path)
		keys := strings.Fields(row[1])
		require.Len(t, keys, 2, "both keys: %q", row[1])
		require.True(t, strings.HasPrefix(keys[0], "failed-0.7.3-"), keys[0])
		require.True(t, strings.HasPrefix(keys[1], "backups-"), keys[1])

		html := row[0]
		require.Contains(t, html, `<p class="update-alert-copy" data-update-part="`+keys[0]+`">`)
		require.Contains(t, html, "<strong>The update to v0.7.3 rolled back.</strong> Seamless is back on v0.7.2, and automatic updates skip v0.7.3.")
		require.Contains(t, html, `<p class="update-alert-copy update-alert-warn" data-update-part="`+keys[1]+`">`)
		require.Contains(t, html, "<strong>Automatic backups are failing.</strong>")
		require.Contains(t, html, `<a class="btn small" href="/console/settings?s=updates" data-update-part="`+keys[0]+`">Settings &rsaquo; Updates</a>`)
		require.Contains(t, html, `<a class="btn small" href="/console/settings?s=backups" data-update-part="`+keys[1]+`">Settings &rsaquo; Backups</a>`)
		require.Equal(t, 1, strings.Count(html, "data-update-banner-dismiss"), "one Got it for the row")
		require.NotContains(t, html, "badly", "never the attempt's error text")
		require.NotContains(t, html, "private path", "never the run's error text")
		require.NotContains(t, html, "<form", "dismissing it changes nothing on the server")
	}
	require.NotContains(t, getPeek(t, mux, "/console/?format=json").Body.String(), "update-backup-alert")
}

// chShellSection is shell.js from the section headed start to the next one.
func chShellSection(t *testing.T, start string) string {
	t.Helper()
	js := string(shellJS)
	i := strings.Index(js, "/* ---- "+start)
	require.GreaterOrEqual(t, i, 0, "section %q", start)
	j := strings.Index(js[i+1:], "/* ---- ")
	require.GreaterOrEqual(t, j, 0, "a section after %q", start)
	return js[i : i+1+j]
}

// "Got it" holds in this browser: the dismissals live in localStorage, under
// the key they always had, through shell.js's store and read, which catch
// every failure, so a browser without storage still shows each banner and
// hides it for the page in view. The list keeps its newest keys only.
func TestChBannersDismissForTheBrowser(t *testing.T) {
	js := string(shellJS)
	require.Contains(t, js, "  function store(key, value) {\n    try {\n      if (value === null) localStorage.removeItem(key);\n      else localStorage.setItem(key, value);\n    } catch (e) {}\n  }")
	require.Contains(t, js, "  function read(key) {\n    try { return localStorage.getItem(key); } catch (e) { return null; }\n  }")

	sec := chShellSection(t, `Banners: "Got it"`)
	require.NotContains(t, sec, "sessionStorage.", "a banner set aside stays aside in every tab")
	require.NotContains(t, sec, "localStorage.", "only through store and read, which catch")
	require.Contains(t, sec, "var BANNERS = 'seamless-level-banners';", "the same key as before")
	require.Contains(t, sec, "JSON.parse(read(BANNERS) || '[]')")
	require.Contains(t, sec, "store(BANNERS, JSON.stringify(gone.slice(-BANNERS_KEPT)));")
	require.Equal(t, 50, lvConst(t, sec, "BANNERS_KEPT"))
	lvInOrder(t, lvBody(t, sec, "function applyBanners() {"),
		"if (bannerKeys(b).every(out)) b.hidden = true;",
		"b.querySelectorAll('[data-update-part]').forEach(function (part) {",
		"if (out('update:' + part.getAttribute('data-update-part'))) part.hidden = true;")
	// The click hides the banner whether or not the store took the keys.
	lvInOrder(t, sec,
		"bannerKeys(banner).forEach(function (id) { if (gone.indexOf(id) === -1) gone.push(id); });",
		"store(BANNERS,",
		"banner.hidden = true;")

	css := string(consoleCSS)
	require.Contains(t, css, ".update-alert [data-update-part][hidden] { display: none; }", "a line or link set aside is hidden despite its display")
	require.Contains(t, css, ".update-alert[hidden] { display: none; }")
}
