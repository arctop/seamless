package diary

import (
	"cmp"
	"slices"
)

// Significance: a thread scores its outcomes and its effort, from recorded
// facts only, each fact capped so no single one carries a thread alone.
// Calibrated on synthetic days built to the plan's patterns (a busy plan day,
// a day of many zero-minute sessions, a parallel-agent afternoon, a session
// across midnight: TestCalibration_*) at the shapes of this repository's own
// Claude Code transcripts, where a thinking model writes about 4,000 output
// tokens a minute of agent time (median 3,950 on the main thread, 5,051 in
// subagents); the table tests pin them.
const (
	// PointsShipped is a plan thread whose last open step closed as done.
	PointsShipped = 10
	// PointsClosed is each task or step closed as done, up to CapClosed.
	PointsClosed = 3
	CapClosed    = 12
	// PointsStarted is each task created or claimed, up to CapStarted.
	PointsStarted = 1
	CapStarted    = 3
	// PointsKept is each memory, note or trial its sessions wrote, up to
	// CapKept.
	PointsKept = 1
	CapKept    = 4
	// PointsCommit is each commit its tasks or sessions recorded, up to
	// CapCommits.
	PointsCommit = 1
	CapCommits   = 4
	// PointsFindings is each session that ended with findings it wrote itself
	// (session_end's; a harvested summary is not an outcome), up to
	// CapFindings.
	PointsFindings = 1
	CapFindings    = 2
	// MinutesPerPoint of agent time score a point, up to CapMinutes.
	MinutesPerPoint = 10
	CapMinutes      = 12
	// OutputPerPoint output tokens score a point, up to CapOutput: half the
	// weight of minutes, saturating with them at about two hours of a
	// thinking model's output. (At the first guess of 20,000 a point it
	// saturated after half an hour and added a constant six to every working
	// thread.)
	OutputPerPoint = 80000
	CapOutput      = 6
)

// The spotlight: at most SpotlightCap threads scoring at least SpotlightFloor,
// most significant first, plus every thread a flagged session pins.
const (
	SpotlightFloor = 4
	SpotlightCap   = 6
)

// The minor test: a session-day with no outcome, under MinorMinutes of agent
// time and under MinorOutput output tokens is short -- never a card, one line
// in the folded row. A flagged session is never minor. MinorOutput is about
// seven minutes of a thinking model's output: this repository's 3 to 6 minute
// question-and-answer sessions wrote 6,700 to 22,000 (the first guess, 4,000,
// is one minute's and folded only the sessions that did nothing).
const (
	MinorMinutes = 10
	MinorOutput  = 30000
)

// threadFacts is what a thread scores.
type threadFacts struct {
	shipped  bool
	closed   int // tasks closed as done
	started  int // tasks created or claimed
	kept     int // distinct items its sessions wrote
	commits  int
	findings int // sessions that ended with findings they wrote
	minutes  int
	output   int
	pinned   bool
}

// score is a thread's significance and the facts that scored, in the
// Reason constants' order.
func score(f threadFacts) (float64, []Reason) {
	var (
		total   float64
		reasons []Reason
	)
	add := func(kind string, count int, points float64) {
		if count <= 0 {
			return
		}
		total += points
		reasons = append(reasons, Reason{Kind: kind, Count: count, Points: points})
	}
	if f.shipped {
		add(ReasonShipped, 1, PointsShipped)
	}
	add(ReasonClosed, f.closed, float64(min(f.closed*PointsClosed, CapClosed)))
	add(ReasonStarted, f.started, float64(min(f.started*PointsStarted, CapStarted)))
	add(ReasonKept, f.kept, float64(min(f.kept*PointsKept, CapKept)))
	add(ReasonCommits, f.commits, float64(min(f.commits*PointsCommit, CapCommits)))
	add(ReasonFindings, f.findings, float64(min(f.findings*PointsFindings, CapFindings)))
	add(ReasonMinutes, f.minutes, min(float64(f.minutes)/MinutesPerPoint, CapMinutes))
	add(ReasonOutput, f.output, min(float64(f.output)/OutputPerPoint, CapOutput))
	if f.pinned {
		reasons = append(reasons, Reason{Kind: ReasonFlagged, Count: 1})
	}
	return round1(total), reasons
}

// round1 rounds to one decimal, so a score reads the same on every platform.
func round1(v float64) float64 {
	return float64(int64(v*10+0.5)) / 10
}

// minor reports whether a session-day is short: no outcome, under
// MinorMinutes and under MinorOutput, and not flagged.
func minor(outcome, flagged bool, minutes, output int) bool {
	return !outcome && !flagged && minutes < MinorMinutes && output < MinorOutput
}

// compareThreads orders threads most significant first: score, then agent
// time, tokens and key.
func compareThreads(a, b *Thread) int {
	return cmp.Or(cmp.Compare(b.Score, a.Score), cmp.Compare(b.Minutes, a.Minutes),
		cmp.Compare(b.Tokens.Total, a.Tokens.Total), cmp.Compare(a.Key, b.Key))
}

// spotlight splits threads (most significant first) into the spotlight and the
// rest: every pinned thread, then the unpinned ones at or above the floor
// until SpotlightCap threads are in.
func spotlight(threads []*Thread) (in, also []*Thread) {
	slices.SortFunc(threads, compareThreads)
	pinned := 0
	for _, t := range threads {
		if t.Pinned {
			pinned++
		}
	}
	room := max(SpotlightCap-pinned, 0)
	for _, t := range threads {
		switch {
		case t.Pinned:
			in = append(in, t)
		case room > 0 && t.Score >= SpotlightFloor:
			in = append(in, t)
			room--
		default:
			also = append(also, t)
		}
	}
	return in, also
}
