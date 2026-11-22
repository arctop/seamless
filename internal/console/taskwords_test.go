package console

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// TestRwTaskWords_EveryStatusAndGate walks every task status through every
// gate and claim combination the console can hold and pins the word and its
// tone. "in progress" is ClaimLive's alone (constraint
// claimtask-claimless-inprogress-and-lease-boundary): a lapsed or claimless
// in_progress task reads as what it is, claimable by any agent.
func TestRwTaskWords_EveryStatusAndGate(t *testing.T) {
	type want struct{ key, text, tone string }
	ready := want{wordReady, "ready", "pop"}
	held := want{wordInProgress, "in progress", "accent"}
	lapsed := want{wordLapsed, "claim lapsed", "danger"}
	unclaimed := want{wordUnclaimed, "unclaimed", "warn"}
	blocked := want{wordBlocked, "blocked", ""}
	done := want{wordDone, "done", "ok"}
	dropped := want{wordDropped, "dropped", ""}
	waits := func(text string) want { return want{wordWaiting, text, "warn"} }

	cases := []struct {
		name string
		f    taskFacts
		want want
	}{
		{"open, nothing before it", taskFacts{Status: "open"}, ready},
		{"open, unfinished blockers", taskFacts{Status: "open", Blocked: 2}, blocked},
		{"open, blocked only from a locked project", taskFacts{Status: "open", Blocked: 1}, blocked},
		{"open, own gate open", taskFacts{Status: "open", WaitsOn: []string{"owner"}}, waits("waiting on owner")},
		{"open, own gate and blockers", taskFacts{Status: "open", WaitsOn: []string{"owner"}, Blocked: 1}, waits("waiting on owner")},
		{"open, behind two gates", taskFacts{Status: "open", WaitsOn: []string{"owner", "hardware"}, Blocked: 2},
			waits("waiting on owner and hardware")},
		{"open gate", taskFacts{Status: "open", Gate: true, WaitsOn: []string{"reviewer:dana"}}, waits("waiting on reviewer:dana")},
		{"open gate naming nobody", taskFacts{Status: "open", Gate: true}, waits("waiting on people")},
		{"open, a stale claim on an open row", taskFacts{Status: "open", Holder: true}, ready},
		{"in progress, live claim", taskFacts{Status: "in_progress", Holder: true, Live: true}, held},
		{"in progress, live claim, blockers", taskFacts{Status: "in_progress", Holder: true, Live: true, Blocked: 1}, held},
		{"in progress, live claim, own gate open", taskFacts{Status: "in_progress", Holder: true, Live: true, WaitsOn: []string{"owner"}}, held},
		{"in progress, lapsed claim", taskFacts{Status: "in_progress", Holder: true}, lapsed},
		{"in progress, lapsed claim, blockers", taskFacts{Status: "in_progress", Holder: true, Blocked: 1}, lapsed},
		{"in progress, no claim", taskFacts{Status: "in_progress"}, unclaimed},
		{"in progress, no claim, own gate open", taskFacts{Status: "in_progress", WaitsOn: []string{"owner"}}, waits("waiting on owner")},
		{"in progress gate", taskFacts{Status: "in_progress", Gate: true, WaitsOn: []string{"owner"}}, waits("waiting on owner")},
		{"done", taskFacts{Status: "done"}, done},
		{"done, a stale claim and an open gate", taskFacts{Status: "done", Holder: true, WaitsOn: []string{"owner"}, Blocked: 1}, done},
		{"done gate", taskFacts{Status: "done", Gate: true}, done},
		{"dropped", taskFacts{Status: "dropped"}, dropped},
		{"dropped, live claim", taskFacts{Status: "dropped", Holder: true, Live: true}, dropped},
		{"status only, open", taskFacts{Status: "open", StatusOnly: true}, want{wordOpen, "open", ""}},
		{"status only, in progress", taskFacts{Status: "in_progress", StatusOnly: true}, held},
		{"status only, done", taskFacts{Status: "done", StatusOnly: true}, done},
		{"unknown status", taskFacts{Status: "paused"}, want{"paused", "paused", ""}},
	}
	tones := map[string]string{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := wordFor(tc.f)
			require.Equal(t, tc.want, want{got.Key, got.Text, got.Tone})
			require.NotContains(t, got.Text, "_", "a word never prints the stored status")
		})
		// One tone per word, wherever it is reached from.
		got := wordFor(tc.f)
		if prev, ok := tones[got.Key]; ok {
			require.Equal(t, prev, got.Tone, "word %q carries one tone", got.Key)
		}
		tones[got.Key] = got.Tone
	}
}

// TestRwTaskWords_CallersShareTheMapping keeps the console at one mapping:
// taskTone and the Diary's dyStatus answer through statusWord, and every
// status the store knows has a word.
func TestRwTaskWords_CallersShareTheMapping(t *testing.T) {
	for _, st := range []core.TaskStatus{core.TaskOpen, core.TaskInProgress, core.TaskDone, core.TaskDropped} {
		w := statusWord(string(st))
		require.Equal(t, w.Tone, taskTone(string(st)), "taskTone(%s)", st)
		require.Equal(t, w.Text, dyStatus(string(st)), "dyStatus(%s)", st)
		require.NotContains(t, w.Text, "_")
	}
	require.Equal(t, "new", dyStatus(""), "a task created on the day")
	require.Equal(t, "in progress", dyStatus("in_progress"))
}

// TestRwTaskWords_FactsOfATask reads facts off a stored task: blockers that
// are finished or unread count for nothing, a gate blocker names who it
// waits on, locked blockers count, and the claim is ClaimLive's -- a lease
// that ends exactly now is already lapsed.
func TestRwTaskWords_FactsOfATask(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	tasks := map[string]core.Task{
		"DONE":  {ID: "DONE", Status: core.TaskDone},
		"OPEN":  {ID: "OPEN", Status: core.TaskOpen},
		"WIP":   {ID: "WIP", Status: core.TaskInProgress},
		"GATE":  {ID: "GATE", Status: core.TaskOpen, Kind: core.TaskKindGate, Gate: core.TaskGate{Who: "owner", What: "sign"}},
		"GATE2": {ID: "GATE2", Status: core.TaskInProgress, Kind: core.TaskKindGate, Gate: core.TaskGate{Who: "owner", What: "again"}},
	}
	lookup := func(id string) (core.Task, bool) {
		tk, ok := tasks[id]
		return tk, ok
	}
	word := func(tk core.Task, locked int) taskWord { return wordFor(factsOf(tk, lookup, locked, now)) }

	open := core.Task{Status: core.TaskOpen}
	require.Equal(t, wordReady, word(open, 0).Key)
	open.DependsOn = []string{"DONE", "GONE"}
	require.Equal(t, wordReady, word(open, 0).Key, "a finished or unread blocker blocks nothing")
	require.Equal(t, wordBlocked, word(open, 1).Key, "a blocker in a locked project blocks")
	open.DependsOn = []string{"DONE", "OPEN", "WIP"}
	require.Equal(t, 2, factsOf(open, lookup, 0, now).Blocked)
	require.Equal(t, wordBlocked, word(open, 0).Key)
	open.DependsOn = []string{"OPEN", "GATE", "GATE2"}
	require.Equal(t, []string{"owner"}, factsOf(open, lookup, 0, now).WaitsOn, "who once, however many gates")
	require.Equal(t, "waiting on owner", word(open, 0).Text)

	own := core.Task{Status: core.TaskOpen, Gate: core.TaskGate{Who: "hardware", What: "the rig"}, DependsOn: []string{"GATE"}}
	require.Equal(t, []string{"hardware", "owner"}, factsOf(own, lookup, 0, now).WaitsOn, "its own gate first")
	cleared := now.Add(-time.Hour)
	own.Gate.ClearedAt = &cleared
	own.DependsOn = nil
	require.Equal(t, wordReady, word(own, 0).Key, "a cleared gate holds nothing")

	live := now.Add(10 * time.Minute)
	held := core.Task{Status: core.TaskInProgress, ClaimedBy: "01SESSION", LeaseExpiresAt: &live}
	require.True(t, held.ClaimLive(now))
	require.Equal(t, wordInProgress, word(held, 0).Key)
	edge := now
	held.LeaseExpiresAt = &edge
	require.False(t, held.ClaimLive(now), "ClaimLive's boundary is strict")
	require.Equal(t, wordLapsed, word(held, 0).Key, "a lease ending exactly now is lapsed, as ClaimLive says")
	held.LeaseExpiresAt = nil
	require.Equal(t, wordLapsed, word(held, 0).Key, "a holder with no lease holds nothing")
	held.ClaimedBy = ""
	require.Equal(t, wordUnclaimed, word(held, 0).Key, "a claimless start")
}

// TestRwClaimLease words a lease: time left while live, how long ago once
// lapsed. A live lease never reads as expiring "now" (the old peek did: ago
// answers "now" for any future time).
func TestRwClaimLease(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { x := now.Add(d); return &x }
	require.Equal(t, "lease live, 12m left", claimLease(at(12*time.Minute+30*time.Second), true, now))
	require.Equal(t, "lease live, 3h left", claimLease(at(3*time.Hour+time.Minute), true, now))
	require.Equal(t, "lease live, <1m left", claimLease(at(20*time.Second), true, now), "never 0m left")
	require.Equal(t, "lapsed 3m ago", claimLease(at(-3*time.Minute), false, now))
	require.Equal(t, "lapsed just now", claimLease(at(-10*time.Second), false, now))
	require.Equal(t, "lapsed 2h ago", claimLease(at(-2*time.Hour), false, now))
	require.Equal(t, "lapsed just now", claimLease(at(0), false, now), "the strict boundary: ending now is lapsed")
	require.Equal(t, "", claimLease(nil, false, now))
	require.Equal(t, "lease not live", claimLease(at(time.Hour), false, now),
		"an unexpired lease on a task that is not in progress holds nothing")
	for _, d := range []time.Duration{time.Second, time.Minute, time.Hour, 4 * time.Hour} {
		require.NotContains(t, claimLease(at(d), true, now), "now")
	}
	require.Equal(t, "12m left", stepLease(now.Add(12*time.Minute+30*time.Second), true, now))
	require.Equal(t, "lapsed 5m ago", stepLease(now.Add(-5*time.Minute), false, now))
}

// TestRwAgoTime pins the <time> agoTime and timeAs draw: the instant in
// datetime, the UTC stamp as the no-JS title, data-clock="title" for
// shell.js, the relative age (or the given text) as the text.
func TestRwAgoTime(t *testing.T) {
	at := time.Now().Add(-3 * 24 * time.Hour).UTC().Truncate(time.Second)
	got := string(agoTime(at))
	require.Equal(t, `<time datetime="`+at.Format(time.RFC3339)+`" data-clock="title" title="`+ts(at)+`">3d</time>`, got)
	require.Contains(t, got, " UTC\"", "the fallback title names its zone")
	require.Equal(t, got, string(agoTime(&at)))
	require.Equal(t, "-", string(agoTime(nil)))
	require.Equal(t, "-", string(agoTime((*time.Time)(nil))))
	require.Equal(t, "-", string(agoTime(time.Time{})))
	require.Equal(t, "-", string(agoTime("yesterday")))

	exp := time.Date(2026, 11, 21, 12, 30, 0, 0, time.UTC)
	require.Equal(t, `<time datetime="2026-11-21T12:30:00Z" data-clock="title" title="2026-11-21 12:30 UTC">lease live, 4m left</time>`,
		string(timeAs(&exp, "lease live, 4m left")))
	require.Equal(t, "a &lt;b&gt;", string(timeAs(nil, "a <b>")), "no time: the text alone, escaped")
	require.Contains(t, string(timeAs(exp, `"x"`)), `>&#34;x&#34;</time>`)
}

// TestRwWordMarks renders a word as a badge and as a chip: tone, key, hint,
// all escaped.
func TestRwWordMarks(t *testing.T) {
	w := wordFor(taskFacts{Status: "open"})
	require.Equal(t, `<span class="badge pop" data-word="ready" title="`+w.Hint+`">ready</span>`, string(taskBadge(w)))
	require.Equal(t, `<span class="kind pop" data-word="ready" title="`+w.Hint+`">ready</span>`, string(taskChip(w)))
	require.Equal(t, `<span class="badge ok" data-word="done">done</span>`, string(taskBadge(statusWord("done"))))
	odd := waitingWord([]string{`a"<b>`})
	require.Contains(t, string(taskBadge(odd)), `>waiting on a&#34;&lt;b&gt;</span>`)
}

// TestRwTemplatesPrintNoRawTaskStatus is the "one vocabulary" guard over the
// task, plan and project templates: no task status prints as a label outside
// the word helper, and taskTone (a bare status's tone) is gone from every
// template. A plan's own lifecycle status (draft, approved, shipped) is a
// word already: the project tab prints it beside planTone, and the Plans
// screen's task data is its steps (range .Tasks) alone.
func TestRwTemplatesPrintNoRawTaskStatus(t *testing.T) {
	files, err := fs.Glob(templateFS, "templates/*.html")
	require.NoError(t, err)
	for _, f := range files {
		b, err := templateFS.ReadFile(f)
		require.NoError(t, err)
		require.NotContains(t, string(b), "taskTone", "%s: a task's tone comes with its word", f)
	}
	raw := regexp.MustCompile(`\{\{\s*\.(Row\.)?Status\s*\}\}`)
	read := func(name string) string {
		b, err := templateFS.ReadFile("templates/" + name + ".html")
		require.NoError(t, err)
		return string(b)
	}
	for _, name := range []string{"tasks", "peek_task", "task_shared"} {
		require.NotRegexp(t, raw, read(name), "%s.html prints a raw task status", name)
	}
	for _, name := range []string{"plans", "peek_plan"} {
		steps := regexp.MustCompile(`(?s)\{\{range \.Tasks\}\}.*?\{\{end\}\}`).FindAllString(read(name), -1)
		require.NotEmpty(t, steps, "%s.html lists the plan's steps", name)
		for _, block := range steps {
			require.NotRegexp(t, raw, block, "%s.html prints a step's raw status", name)
			require.Contains(t, block, "{{taskBadge .Word}}")
			require.Contains(t, block, "{{copyBtn .ID}}", "a step copies its whole id")
		}
	}
	for _, name := range []string{"projectdetail"} {
		for i, line := range strings.Split(read(name), "\n") {
			for _, loc := range raw.FindAllStringIndex(line, -1) {
				before := line[:loc[0]]
				open := strings.LastIndex(before, "<span")
				require.True(t, open >= 0 && strings.Contains(before[open:], "planTone"),
					"%s.html:%d prints a raw status outside the word helper: %s", name, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// TestRwShellLocalizesTimeTitles pins shell.js's half of the clock titles: a
// data-clock="title" element trades its UTC title for this browser's time,
// and it does so again after every live refresh, which morphs the server's
// title back and drops data-clocked.
func TestRwShellLocalizesTimeTitles(t *testing.T) {
	js := string(shellJS)
	require.Contains(t, js, `if (kind === 'title') el.setAttribute('title', date + ' ' + text);`)
	require.Contains(t, js, `document.addEventListener('seam:content-updated', function () { localClocks(document); });`)
	require.Contains(t, js, "new MutationObserver", "fragments swapped in later localize too")
	// The morph resets attributes from the server's markup: data-clocked must
	// be one of them, or the refresh hook would skip every refreshed node.
	require.NotContains(t, string(agoTime(time.Now().Add(-time.Hour))), "data-clocked")
}

// TestRwCopyInsideARowCancelsItsDefault pins what keeps a copy button inside
// a <summary> or a row link from also toggling or opening the row: the shared
// copy handler cancels the click's default, and shell.js's recents skip a
// click that landed on a copy button.
func TestRwCopyInsideARowCancelsItsDefault(t *testing.T) {
	layout, err := templateFS.ReadFile("templates/layout.html")
	require.NoError(t, err)
	src := string(layout)
	at := strings.Index(src, ".copy-btn[data-copy]') : null;")
	require.Greater(t, at, 0, "the shared copy handler")
	handler := src[at : at+300]
	require.Contains(t, handler, "e.preventDefault();", "no toggle of the summary, no navigation of the link")
	require.Contains(t, string(shellJS), "if (!a || e.target.closest('.copy-btn')) return;")
}
