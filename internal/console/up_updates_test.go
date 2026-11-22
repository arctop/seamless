package console

import (
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/update"
)

// Settings > Updates, what is true first (plan:console-ux-pass UX 36): one
// alert card for whatever needs the owner, the first card, the switches.

var (
	upHiddenRE   = regexp.MustCompile(`<input type="hidden" name="([a-z]+)" value="([a-z]+)">`)
	upCheckboxRE = regexp.MustCompile(`<input type="checkbox" role="switch" name="([a-z]+)" value="([a-z]+)"[^>]*>`)
)

// upToggled is what a switch's autosave form posts once the owner flips its
// switch: every hidden input, and the checkbox while it ends up checked --
// what FormData reads off the form the server drew. disabled reports a
// switch the owner cannot flip at all.
func upToggled(t *testing.T, form string) (posted url.Values, disabled bool) {
	t.Helper()
	require.NotEmpty(t, form)
	posted = url.Values{}
	for _, m := range upHiddenRE.FindAllStringSubmatch(form, -1) {
		posted.Add(m[1], m[2])
	}
	boxes := upCheckboxRE.FindAllStringSubmatch(form, -1)
	require.Len(t, boxes, 1, "one switch per form: %s", form)
	box := boxes[0][0]
	if strings.Contains(box, " disabled") {
		return posted, true
	}
	if !strings.Contains(box, " checked") {
		posted.Add(boxes[0][1], boxes[0][2])
	}
	return posted, false
}

// Each switch saves at once (data-autosave) and, flipped either way, posts
// exactly one value for its field: the form draws its hidden "off" only
// while the switch is on. So the routes keep their fields and switchedOff
// keeps judging the direction: off from an agent's process is refused with
// nothing stored, on is open to it, and the owner turns either off.
func TestUpSwitches_PostOneValueAndOffStaysTheOwners(t *testing.T) {
	now := time.Now()
	agent := uohCallers(t)[0]
	for _, sw := range uohSwitches {
		for _, on := range []bool{true, false} {
			name := sw.field + "/off then flipped on"
			if on {
				name = sw.field + "/on then flipped off"
			}
			t.Run(name, func(t *testing.T) {
				st := autoStatus(now)
				o := config.UpdateOverride{}
				if !on {
					o = config.UpdateOverride{Auto: boolPtr(false)}
					if sw.field == "check" {
						o = config.UpdateOverride{Check: boolPtr(false)}
					}
				}
				st.Settings = update.Effective(autoBase, o, true)
				db, mux, fake := newUpdatesConsole(t, autoBase, &st)
				form := formFor(updatesSection(t, mux), sw.path)
				require.Contains(t, form, " data-autosave>", "the switch saves at once")
				require.NotContains(t, form, `type="submit"`, "no button: the switch is the control")

				posted, disabled := upToggled(t, form)
				require.False(t, disabled)
				want := "on"
				if on {
					want = "off"
				}
				require.Equal(t, url.Values{sw.field: {want}}, posted, "exactly one value, the new direction")

				rr := uohPost(t, mux, agent, sw.path, posted.Encode())
				if want == "off" {
					uohRequireRefused(t, rr, agent, sw.field+" off from the switch")
					stored, _ := storedOverride(t, db)
					require.Equal(t, o, stored, "nothing stored")
					_, refreshes := fake.counts()
					require.Zero(t, refreshes)
					param, msg := locationOf(t, postForm(mux, sw.path, posted.Encode()))
					require.Equal(t, "notice", param, msg)
					require.Contains(t, msg, "turned off", "the owner turns it off")
					return
				}
				param, msg := locationOf(t, rr)
				require.Equal(t, "notice", param, msg)
				require.Contains(t, msg, "turned on", "on is open to an agent's process")
				stored, _ := storedOverride(t, db)
				require.Equal(t, boolPtr(true), sw.stored(stored))
			})
		}
	}
}

// Updates says the config-file case once: update.check: false in the file
// decides the whole section, so the head's chip says it and the check switch
// is locked with no chip or sentence of its own (and no auto switch can be
// flipped); update.auto: false decides one switch, so its locked chip sits
// beside that switch and the head says where the rest comes from. Each is
// final: turning it on is refused with nothing stored.
func TestUpConfigFile_SaidOnceAndFinal(t *testing.T) {
	now := time.Now()
	t.Run("update.check: false", func(t *testing.T) {
		locked := autoBase
		locked.Check = boolPtr(false)
		st := autoStatus(now)
		st.Settings = update.Effective(locked, config.UpdateOverride{}, true)
		db, mux, _ := newUpdatesConsole(t, locked, &st)
		sec := updatesSection(t, mux)
		head := regexp.MustCompile(`(?s)<header class="settings-section-head">.*?</header>`).FindString(sec)
		require.Contains(t, head, `<span class="src-chip is-locked">`)
		for _, once := range []string{"Config file decides", "update.check", "SEAMLESS_UPDATE_CHECK", "config file", `<span class="src-chip `} {
			require.Equal(t, 1, strings.Count(sec, once), "%q is said once", once)
		}
		for _, sw := range uohSwitches {
			_, disabled := upToggled(t, formFor(sec, sw.path))
			require.True(t, disabled, "%s cannot be flipped on", sw.field)
		}
		param, msg := locationOf(t, postForm(mux, "/console/settings/updates", "check=on"))
		require.Equal(t, "error", param)
		require.Equal(t, "Update checks stay off: "+updatesLockedText, msg)
		_, found := storedOverride(t, db)
		require.False(t, found)
	})
	t.Run("update.auto: false", func(t *testing.T) {
		noAuto := autoBase
		noAuto.Auto = boolPtr(false)
		st := autoStatus(now)
		st.Settings = update.Effective(noAuto, config.UpdateOverride{}, true)
		db, mux, _ := newUpdatesConsole(t, noAuto, &st)
		sec := updatesSection(t, mux)
		for _, once := range []string{"Config file decides", "update.auto", "SEAMLESS_UPDATE_AUTO"} {
			require.Equal(t, 1, strings.Count(sec, once), "%q is said once", once)
		}
		auto := formFor(sec, "/console/settings/updates/auto")
		require.Contains(t, auto, `<span class="src-chip is-locked">`, "beside the switch it decides")
		_, disabled := upToggled(t, auto)
		require.True(t, disabled)
		_, disabled = upToggled(t, formFor(sec, "/console/settings/updates"))
		require.False(t, disabled, "checks stay the console's to switch")
		head := regexp.MustCompile(`(?s)<header class="settings-section-head">.*?</header>`).FindString(sec)
		require.Contains(t, head, `<span class="src-chip is-file">`)
		param, msg := locationOf(t, postForm(mux, "/console/settings/updates/auto", "auto=on"))
		require.Equal(t, "error", param)
		require.Equal(t, "Automatic updates stay off: update.auto: false is set in the config file or SEAMLESS_UPDATE_AUTO; change it there.", msg)
		_, found := storedOverride(t, db)
		require.False(t, found)
	})
}

// The alert card, one table row per exceptional state over update.Status
// fixtures: the item it draws, its tone, its action, the words its hint
// keeps, and the order, worst first. A state that needs nothing draws no
// card, and the section is no list of rows.
func TestUpAlertCard_EachExceptionalState(t *testing.T) {
	now := time.Now()
	failed := func(outcome string, to update.Version) *update.AttemptResult {
		return &update.AttemptResult{From: ver(0, 7, 2), To: to, Why: update.WhyAuto, Outcome: outcome,
			Stage: update.StageInstall, Error: "the updater's own words", LogPath: "/data/update/logs/01M5.log", FoldedAt: now.Add(-time.Hour)}
	}
	for _, tc := range []struct {
		name   string
		mutate func(st *update.Status)
		// kinds are the items, in order; tone the card's.
		kinds []string
		tone  string
		want  []string
	}{
		{name: "up to date: no card", mutate: func(*update.Status) {}},
		{name: "an update that applied is the first card's quiet line", mutate: func(st *update.Status) {
			st.LastAttempt = &update.AttemptResult{From: ver(0, 7, 1), To: ver(0, 7, 2), Why: update.WhyAuto,
				Outcome: update.OutcomeApplied, Stage: update.StageDone, FoldedAt: now.Add(-time.Hour)}
		}, want: []string{"Last update v0.7.1 &rarr; v0.7.2, applied", "v0.7.2 is installed, and it answered as the running version."}},
		{name: "a failure the install moved past is history too", mutate: func(st *update.Status) {
			st.LastAttempt = failed(update.OutcomeFailed, ver(0, 7, 1))
		}, want: []string{"Last update v0.7.2 &rarr; v0.7.1, failed", "The updater said: the updater&#39;s own words"}},
		{name: "stopped", mutate: func(st *update.Status) { st.Stopped = true },
			kinds: []string{"stopped"}, tone: "danger", want: []string{"The daemon keeps serving; restart it to revive the check."}},
		{name: "paused, with Resume", mutate: func(st *update.Status) {
			st.Paused = &update.Pause{Reason: update.PauseRollbacks, At: now}
		}, kinds: []string{"paused"}, tone: "warn", want: []string{`action="/console/settings/updates/resume"`, "Nothing installs by itself until you resume them."}},
		{name: "held back, with Resume", mutate: func(st *update.Status) {
			st.Hold = &update.Hold{Through: ver(0, 7, 3), From: ver(0, 7, 3), At: now}
		}, kinds: []string{"held"}, tone: "warn", want: []string{`action="/console/settings/updates/resume"`}},
		{name: "paused and held: one Resume lifts both", mutate: func(st *update.Status) {
			st.Paused = &update.Pause{Reason: update.PauseRollbacks, At: now}
			st.Hold = &update.Hold{Through: ver(0, 7, 3), From: ver(0, 7, 3), At: now}
		}, kinds: []string{"paused", "held"}, tone: "warn"},
		{name: "skipped releases", mutate: func(st *update.Status) {
			st.Blocked = []update.Block{{Version: ver(0, 7, 3), Reason: update.BlockRolledBack, At: now},
				{Version: ver(0, 7, 4), Reason: update.BlockRolledBack, At: now}}
		}, kinds: []string{"skipped"}, tone: "warn", want: []string{"Automatic updates skip 2 releases.", skippedHint}},
		{name: "a backoff", mutate: func(st *update.Status) {
			st.Backoff = &update.Backoff{Until: now.Add(time.Hour), Count: 1, Version: ver(0, 7, 4), Reason: update.OutcomeFailed}
		}, kinds: []string{"backoff"}, tone: "warn", want: []string{"The attempt on v0.7.4 failed.", "Automatic updates try again"}},
		{name: "a failed attempt, with its log", mutate: func(st *update.Status) {
			st.LastAttempt = failed(update.OutcomeFailed, ver(0, 7, 3))
		}, kinds: []string{"attempt"}, tone: "warn", want: []string{`data-copy="/data/update/logs/01M5.log"`, "The updater said: the updater&#39;s own words"}},
		{name: "applied with warnings, with the rewire command", mutate: func(st *update.Status) {
			st.LastAttempt = &update.AttemptResult{From: ver(0, 7, 1), To: ver(0, 7, 2), Why: update.WhyAuto,
				Outcome: update.OutcomeApplied, Stage: update.StageDone, Warnings: true, FoldedAt: now}
		}, kinds: []string{"attempt"}, tone: "warn", want: []string{`data-copy="seamlessd install-hooks"`}},
		{name: "worst first: stopped, paused, held, skipped, backoff, the attempt", mutate: func(st *update.Status) {
			st.Stopped = true
			st.Paused = &update.Pause{Reason: update.PauseBroken, At: now}
			st.Hold = &update.Hold{Through: ver(0, 7, 3), From: ver(0, 7, 3), At: now}
			st.Blocked = []update.Block{{Version: ver(0, 7, 5), Reason: update.BlockRolledBack, At: now}}
			st.Backoff = &update.Backoff{Until: now.Add(time.Hour), Count: 1, Version: ver(0, 7, 6), Reason: update.OutcomeFailed}
			st.LastAttempt = failed(update.OutcomeBroken, ver(0, 7, 3))
		}, kinds: []string{"stopped", "paused", "held", "skipped", "backoff", "attempt"}, tone: "danger",
			want: []string{`disabled title="The update checker is not running"`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := autoStatus(now)
			tc.mutate(&st)
			_, mux, _ := newUpdatesConsole(t, autoBase, &st)
			sec := updatesSection(t, mux)
			require.NotContains(t, sec, "<dl", "no list of rows")
			require.NotContains(t, sec, "<dt>")
			var kinds []string
			for _, m := range regexp.MustCompile(`data-alert="([a-z]+)"`).FindAllStringSubmatch(sec, -1) {
				kinds = append(kinds, m[1])
			}
			require.Equal(t, tc.kinds, kinds)
			if tc.tone == "" {
				require.NotContains(t, sec, `role="alert"`)
			} else {
				require.Equal(t, 1, strings.Count(sec, `role="alert"`), "one card")
				require.Contains(t, sec, `<div class="up-alert is-`+tc.tone+`" role="alert">`)
				head := strings.Index(sec, `id="updates-installed"`)
				require.Less(t, strings.Index(sec, `role="alert"`), head, "the card opens the section")
			}
			require.LessOrEqual(t, strings.Count(sec, `action="/console/settings/updates/resume"`), 1, "one Resume")
			for _, want := range tc.want {
				require.Contains(t, sec, want)
			}
		})
	}
}

// The alert card and the first card render parsed versions and the constant
// release URL only: a release's own text never reaches the section (memory
// update-surfaces-render-parsed-versions-only).
func TestUpFirstCard_ParsedVersionsOnly(t *testing.T) {
	now := time.Now()
	st := autoStatus(now)
	withNewer(&st, now)
	_, mux, _ := newUpdatesConsole(t, autoBase, &st)
	sec := updatesSection(t, mux)
	require.Contains(t, sec, `<span class="up-version mono">v0.7.2</span>`)
	require.Contains(t, sec, `href="`+update.ReleaseURL(ver(0, 7, 3))+`"`)
	links := regexp.MustCompile(`href="(https?://[^"]+)"`).FindAllStringSubmatch(sec, -1)
	require.Len(t, links, 1)
	require.Equal(t, "https://github.com/arctop/seamless/releases/tag/v0.7.3", links[0][1])
}
