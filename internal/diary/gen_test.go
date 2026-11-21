package diary

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
)

// dmScale sizes a generated history.
type dmScale struct {
	sessions int       // sessions on an ordinary weekday
	prompts  bool      // record each session's prompts (retrieval.injected, hook.prompt)
	content  int       // bytes of injected text an injection carries
	tools    int       // tool.call events of a working session, on the recent days
	ledger   time.Time // days before this have no ledger: stored totals only
	recent   time.Time // tool.call and hook.prompt are kept from here on
}

// dmPlan is a generated plan with steps still open.
type dmPlan struct {
	project, slug string
	open          []string
	claimed       []string
}

// dmGen writes a deterministic history of days shaped like this repository's
// own: most sessions half an hour to two hours of agent time with a thinking
// model's output (about a thousand tokens a minute), a third of them short
// questions or zero-minute starts, a quarter with subagents running beside
// the main thread, some across midnight, plans whose steps are claimed and
// closed, items written and rewritten, prompts and the transport noise of the
// last thirty days.
type dmGen struct {
	f     *dmFixture
	r     *rand.Rand
	sc    dmScale
	plans map[string][]*dmPlan
	seq   int
}

var dmProjects = []struct {
	slug   string
	weight int
}{{"example-ops", 40}, {"example-finance", 20}, {"example-web", 20}, {"example-legal", 10}, {"", 10}}

// dmGenerate writes days of history from the local midnight from.
func dmGenerate(f *dmFixture, from time.Time, days int, seed uint64, sc dmScale) {
	g := &dmGen{f: f, r: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)), sc: sc, plans: map[string][]*dmPlan{}}
	for _, p := range dmProjects {
		if p.slug != "" {
			f.project(p.slug, false)
		}
	}
	day := midnight(from, f.loc)
	for range days {
		g.day(day)
		day = day.AddDate(0, 0, 1)
	}
}

func (g *dmGen) pick() string {
	n := g.r.IntN(100)
	for _, p := range dmProjects {
		if n < p.weight {
			return p.slug
		}
		n -= p.weight
	}
	return ""
}

// plan is an open plan of project, a new one when it has none.
func (g *dmGen) plan(project string, at time.Time, session string) *dmPlan {
	for _, p := range g.plans[project] {
		if len(p.open) > 0 {
			return p
		}
	}
	g.seq++
	p := &dmPlan{project: project, slug: fmt.Sprintf("plan-%d", g.seq)}
	steps := 4 + g.r.IntN(9)
	for i := range steps {
		p.open = append(p.open, g.f.task(project, p.slug, fmt.Sprintf("Step %d of %s", i+1, p.slug), at, session))
	}
	note := g.f.note("Plan "+p.slug, project, []string{"plan:" + p.slug}, at)
	// A capture's payload carries the whole plan, verbatim.
	g.f.event(core.EventPlanCaptured, at, session, project, note, map[string]any{"plan_slug": p.slug, "title": p.slug,
		"content": strings.Repeat("plan text ", 2000)})
	g.plans[project] = append(g.plans[project], p)
	return p
}

func (g *dmGen) day(day time.Time) {
	f := g.f
	n := g.sc.sessions
	switch day.Weekday() {
	case time.Saturday, time.Sunday:
		n /= 4
	}
	if g.r.IntN(10) == 0 {
		n = 0 // a quiet day
	}
	n = max(0, n+g.r.IntN(5)-2)
	for range n {
		project := g.pick()
		start := day.Add(time.Duration(8*60+g.r.IntN(14*60)) * time.Minute)
		kind := g.r.IntN(100)
		model, client := "claude-a", "claude-code"
		switch m := g.r.IntN(100); {
		case m >= 85:
			model, client = "gpt-c", "codex"
		case m >= 60:
			model = "claude-b"
		}
		cwd := ""
		if project == "" {
			cwd = "/home/owner/" + []string{"scratch", "tools", "notes"}[g.r.IntN(3)]
		}
		var minutes, output int
		findings := ""
		switch {
		case kind < 15: // a zero-minute start
			findings = core.FindingNoSummary
		case kind < 35: // a short question
			minutes = 1 + g.r.IntN(8)
			output = minutes * (2500 + g.r.IntN(1500))
			findings = "(auto-harvested) Answered a short question"
		default:
			if g.r.IntN(20) == 0 {
				start = day.Add(time.Duration(23*60+g.r.IntN(40)) * time.Minute) // runs past midnight
			}
			minutes = 10 + g.r.IntN(60) + g.r.IntN(90)
			output = minutes * (2500 + g.r.IntN(3800)) // the transcripts' p10 to p90 rate
			switch g.r.IntN(10) {
			case 0, 1, 2, 3, 4:
				findings = "Worked through the " + strings.TrimPrefix(project, "example-") + " backlog"
			case 5, 6, 7, 8:
				findings = "(auto-harvested) Summarized the session's work"
			}
		}
		span := time.Duration(minutes+g.r.IntN(60)) * time.Minute
		s := f.session(dmSession{Project: project, CWD: cwd, Client: client, Model: model, Created: start,
			Updated: start.Add(span), Findings: findings})
		if minutes > 0 {
			if start.Before(g.sc.ledger) {
				f.exec(`UPDATE sessions SET input_tokens = ?, cached_input_tokens = ?, output_tokens = ?, total_tokens = ?
					WHERE id = ?`, output/2, output*10, output, output/2+output*10+output, s)
			} else {
				f.work(s, start, minutes, output, model, core.UsageThreadMain)
				if kind >= 35 && g.r.IntN(4) == 0 {
					sub := minutes/2 + g.r.IntN(minutes+1)
					f.work(s, start.Add(5*time.Minute), sub, sub*5000, model, core.UsageThreadSubagent)
				}
			}
		}
		g.noise(s, project, start, minutes, kind < 35)
		if kind < 35 {
			continue
		}
		g.outcomes(s, project, start, minutes)
	}
}

// outcomes is a working session's tasks, items, commits and moments.
func (g *dmGen) outcomes(s, project string, start time.Time, minutes int) {
	f := g.f
	at := func(frac int) time.Time { return start.Add(time.Duration(minutes*frac/10) * time.Minute) }
	switch w := g.r.IntN(10); {
	case w < 5 && project != "":
		p := g.plan(project, start, s)
		for i := 0; i < 1+g.r.IntN(2) && len(p.open) > 0; i++ {
			step := p.open[0]
			p.open = p.open[1:]
			f.move(step, at(2), s, "", "in_progress")
			if g.r.IntN(4) > 0 {
				f.move(step, at(8), s, "in_progress", "done")
			} else {
				p.claimed = append(p.claimed, step)
			}
		}
	case w < 7:
		task := f.task(project, "", "Loose fix", at(1), s)
		f.move(task, at(9), s, "open", "done")
	}
	for range g.r.IntN(4) {
		name := fmt.Sprintf("gotcha-%d", g.r.IntN(400))
		m := f.memory(fmt.Sprintf("%s-%d", name, g.seqNext()), project, at(5))
		f.event(core.EventMemoryWritten, at(5), s, project, m, map[string]any{"name": name})
		switch g.r.IntN(3) {
		case 0:
			f.event(core.EventMemoryWritten, at(6), s, project, m, map[string]any{"name": name, "appended": true})
		case 1: // an edit's payload carries its diff
			f.event(core.EventMemoryWritten, at(6), s, project, m, map[string]any{"name": name, "edited": true,
				"diff": strings.Repeat("-old line\n+new line\n", 100)})
		}
	}
	if g.r.IntN(3) == 0 {
		n := f.note(fmt.Sprintf("Notes %d", g.seqNext()), project, nil, at(4))
		f.event(core.EventNoteWritten, at(4), s, project, n, map[string]any{"title": "Notes"})
		for range g.r.IntN(5) {
			f.event(core.EventNoteWritten, at(7), s, project, n, map[string]any{"slug": "notes", "appended": true})
		}
	}
	if g.r.IntN(20) == 0 {
		f.trial("A trial", "lab-a", project, s, at(6))
	}
	if g.r.IntN(10) == 0 {
		f.commit("session", s, s, at(10))
	}
	if g.r.IntN(50) == 0 {
		f.event(core.EventAgentMishap, at(10), s, project, "", map[string]any{"description": "Ran the wrong target"})
	}
	if g.r.IntN(50) == 0 {
		f.exec(`UPDATE sessions SET verdict = 'notable', flagged_at = ? WHERE id = ?`, core.FormatTime(at(10)), s)
		f.event(core.EventSessionFlagged, at(10), s, project, "", map[string]any{"verdict": "notable"})
	}
}

func (g *dmGen) seqNext() int { g.seq++; return g.seq }

// noise is a session's prompts and, on the recent days, its tool calls.
func (g *dmGen) noise(s, project string, start time.Time, minutes int, short bool) {
	f := g.f
	if g.sc.prompts {
		prompts := 1 + g.r.IntN(3)
		if !short {
			prompts = 3 + g.r.IntN(25)
		}
		content := strings.Repeat("x", g.sc.content)
		recent := !start.Before(g.sc.recent)
		for i := range prompts {
			at := start.Add(time.Duration(i*max(minutes, 1)/prompts) * time.Minute)
			prompt := fmt.Sprintf("Prompt %d of the session: please look at the next thing", i+1)
			if g.r.IntN(3) == 0 && recent {
				f.event(core.EventHookPrompt, at, s, project, "", map[string]any{"prompt": prompt, "hook": "user-prompt-submit"})
				continue
			}
			f.event(core.EventInjected, at, s, project, "", map[string]any{"prompt": prompt, "hook": "user-prompt-submit",
				"content": content, "item_ids": []string{"01MEMO"}})
		}
	}
	if g.sc.tools > 0 && !start.Before(g.sc.recent) && !short {
		args := strings.Repeat("a", 200)
		for i := range g.sc.tools {
			at := start.Add(time.Duration(i*max(minutes, 1)/g.sc.tools) * time.Minute)
			f.event(core.EventToolCall, at, s, project, "", map[string]any{"tool": "recall", "args": args})
		}
	}
}
