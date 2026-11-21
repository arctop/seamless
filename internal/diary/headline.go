package diary

import (
	"fmt"
	"strings"
)

// The day's character: the first of these words whose threshold the day's
// Facts meet, in this order. A word needs its threshold; a day under every
// one that recorded anything is a Light day, and one that recorded nothing is
// Quiet.
const (
	WordShipped  = "Shipped"
	WordClosing  = "Closing day"
	WordPlanning = "Planning day"
	WordDeep     = "Deep work"
	WordBusy     = "Busy day"
	WordLight    = "Light day"
	WordQuiet    = "Quiet"
)

// The thresholds, printed verbatim in each word's rule.
const (
	// ClosingDayTasks tasks closed as done make a Closing day.
	ClosingDayTasks = 5
	// A plan drafted (captured, presented or approved), or PlanningDayTasks
	// tasks created, with fewer than PlanningDayMaxClosed closed, make a
	// Planning day.
	PlanningDayTasks     = 6
	PlanningDayMaxClosed = 2
	// DeepWorkMinutes of agent time with DeepWorkShare percent of it in one
	// project make Deep work. Two hours: a working day of one agent reaches
	// it, and under it a day reads as light beside its tiles.
	DeepWorkMinutes = 120
	DeepWorkShare   = 60
	// BusyDayMinutes of agent time, BusyDaySessions sessions beyond the short
	// ones, or work (beyond short sessions) in BusyDayProjects projects make
	// a Busy day: a day of zero-minute starts is not busy. Three hours of
	// agent time spread over projects is a full day, not a light one.
	BusyDayMinutes  = 180
	BusyDaySessions = 12
	BusyDayProjects = 4
)

// Character is the day's character tag, chosen from its Facts by the
// thresholds above.
func Character(f Facts) Tag {
	switch {
	case quiet(f):
		return Tag{Word: WordQuiet, Rule: "No session, task or item recorded"}
	case f.PlansShipped > 0:
		return Tag{Word: WordShipped, Rule: "A plan shipped: its last open step closed as done"}
	case f.TasksClosed >= ClosingDayTasks:
		return Tag{Word: WordClosing, Rule: fmt.Sprintf("%d or more tasks closed as done", ClosingDayTasks)}
	case (f.PlansDrafted > 0 || f.TasksCreated >= PlanningDayTasks) && f.TasksClosed < PlanningDayMaxClosed:
		return Tag{Word: WordPlanning, Rule: fmt.Sprintf(
			"A plan drafted or %d or more tasks created, and fewer than %d closed", PlanningDayTasks, PlanningDayMaxClosed)}
	case f.Minutes >= DeepWorkMinutes && f.TopShare >= DeepWorkShare:
		return Tag{Word: WordDeep, Rule: fmt.Sprintf(
			"%s or more of agent time, %d%% or more of it in one project", Duration(DeepWorkMinutes), DeepWorkShare)}
	case f.Minutes >= BusyDayMinutes || f.Sessions-f.Short >= BusyDaySessions || f.WorkProjects >= BusyDayProjects:
		return Tag{Word: WordBusy, Rule: fmt.Sprintf(
			"%s or more of agent time, %d or more sessions beyond the short ones, or work in %d or more projects",
			Duration(BusyDayMinutes), BusyDaySessions, BusyDayProjects)}
	}
	return Tag{Word: WordLight, Rule: "Work recorded, under every threshold of a fuller day"}
}

// quiet reports whether the day recorded nothing at all.
func quiet(f Facts) bool {
	return f.Sessions == 0 && f.TasksClosed == 0 && f.TasksCreated == 0 && f.Kept == 0 &&
		f.PlansDrafted == 0 && f.PlansShipped == 0 && f.Minutes == 0 && f.Tokens == 0
}

// HeadlineMostlyShare is the share of the day's agent time, in percent, at
// which the headline calls the leading thread "mostly" the day.
const HeadlineMostlyShare = 50

// Headline is the day's sentence, composed from its facts: the leading
// thread and what it did, then how many sessions across how many projects
// (and, when no one thread was most of the day, how many things were worked
// on, the cards and lines the page lists); "so far" while the day is under
// way. It reads as a sentence, not a log line: "4 of 7", never "4/7", and a
// project the first half named is not named again.
func Headline(e Entry) string {
	soFar := ""
	if e.Today {
		soFar = " so far"
	}
	f := e.Facts
	if quiet(f) {
		return "Nothing recorded" + soFar + "."
	}
	var lead *Thread
	switch {
	case len(e.Spotlight) > 0:
		lead = &e.Spotlight[0]
	case len(e.Also) > 0:
		lead = &e.Also[0]
	}
	var b strings.Builder
	things := 0
	if lead == nil {
		fmt.Fprintf(&b, "Only short sessions: %s of agent time.", Duration(f.Minutes))
	} else {
		threads := len(e.Spotlight) + len(e.Also)
		name := threadName(*lead)
		if threads == 1 || mostlyLead(e, *lead) {
			fmt.Fprintf(&b, "Mostly %s: %s.", name, threadClause(*lead))
		} else {
			fmt.Fprintf(&b, "Led by %s: %s.", name, threadClause(*lead))
			things = threads
		}
	}
	b.WriteString(" ")
	b.WriteString(sessionsClause(e, lead, things, soFar))
	return b.String()
}

// mostlyLead reports whether the leading thread is "mostly" the day: at
// least HeadlineMostlyShare percent of its agent time, and no other thread
// that much too (a session shared by two threads counts in full toward both,
// so neither is mostly the day).
func mostlyLead(e Entry, lead Thread) bool {
	total := e.Facts.Minutes
	if total == 0 || lead.Minutes*100 < total*HeadlineMostlyShare {
		return false
	}
	for _, ts := range [][]Thread{e.Spotlight, e.Also} {
		for _, t := range ts {
			if t.Key != lead.Key && t.Minutes*100 >= total*HeadlineMostlyShare {
				return false
			}
		}
	}
	return true
}

// threadName names a thread in a sentence: a plan in its project, or the
// project or directory itself.
func threadName(t Thread) string {
	if t.Kind == ThreadPlan {
		project := t.Project
		if project == "" {
			project = globalLabel
		}
		return t.Label + " in " + project
	}
	return t.Label
}

// threadClause says what a thread did, from the most telling fact it has.
func threadClause(t Thread) string {
	closedN, movedN := 0, 0
	for _, m := range t.Moved {
		if m.Closed {
			closedN++
		}
		if m.From != m.To || m.Created {
			movedN++
		}
	}
	noun := "task"
	if t.Kind == ThreadPlan {
		noun = "step"
	}
	var parts []string
	switch {
	case closedN > 0:
		parts = append(parts, fmt.Sprintf("%s closed", plural(closedN, noun)))
	case movedN > 0:
		parts = append(parts, fmt.Sprintf("%s moved", plural(movedN, noun)))
	}
	if p := t.Progress; p != nil && p.End.Total > 0 {
		switch {
		case t.Shipped && len(parts) > 0:
			// "1 step closed and the plan shipped (5 of 5)": one clause, so the
			// closing reads as what shipped it.
			parts[len(parts)-1] += fmt.Sprintf(" and the plan shipped (%d of %d)", p.End.Closed, p.End.Total)
		case t.Shipped:
			parts = append(parts, fmt.Sprintf("the plan shipped (%d of %d)", p.End.Closed, p.End.Total))
		case closedN > 0:
			parts = append(parts, fmt.Sprintf("the plan is now at %d of %d", p.End.Closed, p.End.Total))
		default:
			parts = append(parts, fmt.Sprintf("the plan is at %d of %d", p.End.Closed, p.End.Total))
		}
	}
	if len(parts) == 0 && len(t.Kept) > 0 {
		parts = append(parts, fmt.Sprintf("%s kept", plural(len(t.Kept), "item")))
	}
	if len(parts) == 0 {
		parts = append(parts, fmt.Sprintf("%s of agent time", Duration(t.Minutes)))
	}
	return strings.Join(parts, ", ")
}

// sessionsClause is the headline's second sentence: the sessions and where
// they were, and how many things they worked on when there were several
// (things > 1); it leads with the sessions, so no figure follows the first
// sentence's last one across the full stop. A day in one project names it,
// unless the leading thread already did.
func sessionsClause(e Entry, lead *Thread, things int, soFar string) string {
	f := e.Facts
	if f.Sessions == 0 {
		return "No sessions" + soFar + "."
	}
	where := ""
	switch f.Projects {
	case 0:
	case 1:
		switch p := oneProject(e); {
		case p == "":
			where = " in one project"
		case lead == nil || lead.Project != p:
			where = " in " + p
		}
	default:
		where = fmt.Sprintf(" across %d projects", f.Projects)
	}
	sessions := plural(f.Sessions, "session")
	if things > 1 {
		return fmt.Sprintf("%s%s worked on %d things%s.", sessions, where, things, soFar)
	}
	return sessions + where + soFar + "."
}

// oneProject is the project of a day with one: the first its usage or its
// threads name, "" when none does.
func oneProject(e Entry) string {
	for _, p := range e.Usage.Projects {
		if p.Project != "" {
			return p.Project
		}
	}
	for _, ts := range [][]Thread{e.Spotlight, e.Also} {
		for _, t := range ts {
			if t.Project != "" {
				return t.Project
			}
		}
	}
	return ""
}

// plural is n and the noun, pluralized with an s.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// Duration renders agent minutes as the Diary writes them: 45m, 3h, 6h 40m.
func Duration(minutes int) string {
	h, m := minutes/60, minutes%60
	switch {
	case h == 0:
		return fmt.Sprintf("%dm", m)
	case m == 0:
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh %dm", h, m)
}
