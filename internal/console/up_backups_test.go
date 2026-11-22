package console

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/backup"
	"github.com/arctop/seamless/internal/config"
)

// Backups' first card reads its words from the service's status: when the
// last backup ran, when the next one does, how many the schedule keeps and
// the newest one's size -- each from backup.Status, never from the list.
func TestUpBackupsFirstCard_FromTheStatus(t *testing.T) {
	now := time.Now()
	on := true
	for _, tc := range []struct {
		name   string
		base   config.Backup
		status func(st *backup.Status)
		title  string
		sub    string
		tone   string
	}{
		{name: "on, made one, next due", base: config.Backup{Auto: &on, Keep: 12, Every: config.Duration(6 * time.Hour)},
			status: func(st *backup.Status) {
				st.LastBackup = &backup.Record{At: now.Add(-2 * time.Hour), Bytes: 3 << 20}
				st.LastConfirmed = now.Add(-2 * time.Hour)
				st.NextAt = now.Add(4 * time.Hour)
			}, title: `Backed up <time [^>]*>2h ago</time>`, sub: `<time [^>]*>Next in [34]h[^<]*</time> &middot; 12 kept &middot; 3.0 MB`, tone: "ok"},
		{name: "on, none yet", base: config.Backup{Auto: &on},
			status: func(st *backup.Status) { st.NextAt = now.Add(time.Hour) },
			title:  `No backup yet`, sub: `<time [^>]*>Next in [^<]*</time> &middot; 7 kept`, tone: "warn"},
		{name: "off, one made by hand", status: func(st *backup.Status) {
			st.LastBackup = &backup.Record{At: now.Add(-26 * time.Hour), Bytes: 2 << 20}
		}, title: `Backed up <time [^>]*>1d ago</time>`, sub: `Automatic backups are off &middot; 2.0 MB`, tone: "quiet"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bc := newBackupsConsole(t, &fakeBackups{base: tc.base})
			tc.status(&bc.fb.status)
			sec := sectionOf(t, bc.page(t, ""))
			card := regexp.MustCompile(`(?s)<article class="card bk-hero is-([a-z]+)" id="backup-status".*?</article>`).FindStringSubmatch(sec)
			require.NotNil(t, card)
			require.Equal(t, tc.tone, card[1])
			require.Regexp(t, `<h3 class="bk-hero-title" id="bk-status-title">`+tc.title, card[0])
			require.Regexp(t, `<p class="bk-hero-sub">`+tc.sub+`</p>`, card[0])
			require.Contains(t, card[0], `action="/console/settings/backups/now"`, "Back up now is the card's action")
			require.Contains(t, card[0], `<span class="bk-folder-chip" title="`+bc.local()+`">`)
		})
	}
}

// The schedule stays a form with a Save bar (D9: it has typed values, a
// number and a folder): nothing in it saves at once, its bar is the form's
// own child (so it shows only while the form is dirty), and its rows are
// the switch, How often as segments, Keep with its unit and hint, and the
// folder with its chips and live check.
func TestUpBackupsSchedule_KeepsItsSaveBar(t *testing.T) {
	bc := newBackupsConsole(t, &fakeBackups{})
	sec := sectionOf(t, bc.page(t, ""))
	form := regexp.MustCompile(`(?s)<form method="post" action="/console/settings/backups" [^>]*>.*?</form>`).FindString(sec)
	require.NotEmpty(t, form)
	require.Contains(t, form, "data-backups-form")
	require.NotContains(t, form, "data-autosave", "a form with a typed value keeps its Save bar")
	require.Regexp(t, `(?s)</div>\s*<footer class="brief-actions settings-savebar">.*<button class="btn primary" type="submit">.*Save</button>.*</footer>\s*</form>`, form)
	require.Contains(t, form, `<input id="bk-auto" type="checkbox" role="switch" name="auto" value="on">`)
	require.Contains(t, form, `<div class="set-seg" role="radiogroup" aria-labelledby="bk-every-label">`)
	require.Equal(t, 4, strings.Count(form, `type="radio" name="every"`), "hourly, 6 hours, daily, weekly")
	require.Contains(t, form, `<input type="radio" name="every" value="24h" checked><span>Daily</span>`)
	require.Contains(t, form, `<span class="set-unit"><input id="bk-keep" type="number" name="keep" min="1" max="100" value="7" required><span>automatic</span></span>`)
	require.Contains(t, form, `aria-label="About Keep"`)
	require.Contains(t, form, `data-backups-dir`)
	require.Contains(t, form, `data-backups-check`)
	require.NotContains(t, sec, "<select", "How often is segments now")
}

// The list keeps its rows and Delete keeps its in-place confirm; the lines
// under it -- where the backups come from, and what a locked project shows
// -- moved into the hint on its title.
func TestUpBackupsList_FootLinesInTheHint(t *testing.T) {
	l := backupsList{Local: "/data/backups", Locked: true}
	require.Equal(t, "The backup folder's backups, and the safety and pre-update backups in Seamless's data folder, /data/backups. "+
		"A locked project shows its name and what the backup holds of it, never what it holds now.", listHint(l))
	require.Empty(t, listHint(backupsList{}))

	src, err := templateFS.ReadFile("templates/settings_backups.html")
	require.NoError(t, err)
	require.Contains(t, string(src), `Available backups{{with .Hint}}{{hintFor "Available backups" .}}{{end}}`)
	require.NotContains(t, string(src), "A locked project shows its name", "the line is the hint's now")
}
