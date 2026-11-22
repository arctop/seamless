package console

// Task words: the one vocabulary the console speaks for a task or a plan step
// (plan:console-ux-pass). A stored status is the machine's word -- "open" is
// the same for a step an agent could take now and one that waits on three
// others -- so every task and plan surface prints wordFor instead:
//
//	ready             open, nothing unfinished before it, no open gate
//	in progress       held by a live claim
//	claim lapsed      in progress, but its holder's lease ran out
//	unclaimed         in progress with no claim at all (a claimless start)
//	blocked           open, waiting on unfinished work
//	waiting on <who>  a gate, or a step an open gate holds
//	done, dropped     closed
//
// core.Task.ClaimLive is the only authority on "held" (constraint
// claimtask-claimless-inprogress-and-lease-boundary): a lapsed or claimless
// in_progress task is claimable by any agent, so it never reads as in
// progress. Each word has one tone, here and nowhere else; taskTone and the
// Diary's dyStatus are callers.

import (
	"context"
	"html/template"
	"slices"
	"time"

	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// taskWord is a task's state in the owner's words. Key is its stable name
// (a data-word attribute, the JSON), Text what the owner reads, Tone its badge
// tone class (console.css .badge.*, .kind.*), Hint what it means, for a title.
type taskWord struct {
	Key  string `json:"key"`
	Text string `json:"text"`
	Tone string `json:"tone,omitempty"`
	Hint string `json:"-"`
}

// The word keys.
const (
	wordReady      = "ready"
	wordInProgress = "in-progress"
	wordLapsed     = "claim-lapsed"
	wordUnclaimed  = "unclaimed"
	wordBlocked    = "blocked"
	wordWaiting    = "waiting"
	wordDone       = "done"
	wordDropped    = "dropped"
	// wordOpen is an open task whose blockers and gates were not read: a
	// day's record (the Diary) knows only the status it had then.
	wordOpen = "open"
)

// taskFacts is what wording a task reads from its row: the stored status,
// whether it is a gate, who the open gates holding it wait on (its own first,
// then its gate blockers'), how many unfinished tasks block it (those in
// locked projects included), and its claim. StatusOnly says the row carries
// the status and nothing else (a day's record), so open and in progress read
// as the status it had.
type taskFacts struct {
	Status     string
	Gate       bool
	WaitsOn    []string
	Blocked    int
	Holder     bool // claimed_by names a session
	Live       bool // core.Task.ClaimLive as of the read
	StatusOnly bool
}

// wordFor is the mapping: a task's facts in the owner's words, with the tone.
func wordFor(f taskFacts) taskWord {
	switch core.TaskStatus(f.Status) {
	case core.TaskDone:
		return taskWord{Key: wordDone, Text: "done", Tone: "ok"}
	case core.TaskDropped:
		return taskWord{Key: wordDropped, Text: "dropped", Hint: "Closed without being done."}
	case core.TaskInProgress:
		switch {
		case f.StatusOnly, f.Live:
			return taskWord{Key: wordInProgress, Text: "in progress", Tone: "accent", Hint: "Held by a live claim."}
		case f.Gate || len(f.WaitsOn) > 0:
			return waitingWord(f.WaitsOn)
		case f.Holder:
			return taskWord{Key: wordLapsed, Text: "claim lapsed", Tone: "danger",
				Hint: "Its holder's lease ran out: any agent can claim it."}
		default:
			return taskWord{Key: wordUnclaimed, Text: "unclaimed", Tone: "warn",
				Hint: "Marked in progress without a claim: any agent can claim it."}
		}
	case core.TaskOpen:
		switch {
		case f.StatusOnly:
			return taskWord{Key: wordOpen, Text: "open"}
		case f.Gate || len(f.WaitsOn) > 0:
			return waitingWord(f.WaitsOn)
		case f.Blocked > 0:
			return taskWord{Key: wordBlocked, Text: "blocked", Hint: "Waits on unfinished work before it."}
		default:
			return taskWord{Key: wordReady, Text: "ready", Tone: "pop",
				Hint: "Nothing unfinished before it: an agent can claim it now."}
		}
	}
	// A status this build does not know reads as itself.
	return taskWord{Key: f.Status, Text: f.Status}
}

// waitingWord names who a task waits on, verbatim ("waiting on owner and
// hardware"); a gate that names nobody waits on people.
func waitingWord(who []string) taskWord {
	text := "waiting on people"
	if len(who) > 0 {
		text = "waiting on " + approval.JoinAnd(who)
	}
	return taskWord{Key: wordWaiting, Text: text, Tone: "warn",
		Hint: "Waits on a person, or an event outside Seamless."}
}

// statusWord words a bare status, a row that carries nothing else.
func statusWord(status string) taskWord {
	return wordFor(taskFacts{Status: status, StatusOnly: true})
}

// factsOf reads a task's facts: blockerOf answers the tasks it depends on
// (one it cannot answer counts as finished), locked counts its unfinished
// blockers in locked projects the request does not read.
func factsOf(t core.Task, blockerOf func(string) (core.Task, bool), locked int, now time.Time) taskFacts {
	f := taskFacts{
		Status: string(t.Status), Gate: t.IsGate(), Blocked: locked,
		Holder: t.ClaimedBy != "", Live: t.ClaimLive(now),
	}
	if t.Gate.Open() {
		f.WaitsOn = append(f.WaitsOn, t.Gate.Who)
	}
	for _, id := range t.DependsOn {
		b, ok := blockerOf(id)
		if !ok || b.Status.Closed() {
			continue
		}
		f.Blocked++
		if b.IsGate() && b.Gate.Who != "" && !slices.Contains(f.WaitsOn, b.Gate.Who) {
			f.WaitsOn = append(f.WaitsOn, b.Gate.Who)
		}
	}
	return f
}

// wordTasks words each of tasks (read with their DependsOn), by id. The
// dependencies they name that are not among them are read in one query
// through the request's handle; their blockers in locked projects through the
// main handle, by project only (markLockedBlockers' rule), and only while
// something is locked.
func (s *Service) wordTasks(ctx context.Context, tasks []core.Task, now time.Time) (map[string]taskWord, error) {
	known := make(map[string]core.Task, len(tasks))
	for _, t := range tasks {
		known[t.ID] = t
	}
	var missing, open []string
	for _, t := range tasks {
		if t.Status.Closed() {
			continue
		}
		open = append(open, t.ID)
		for _, id := range t.DependsOn {
			if _, ok := known[id]; !ok && !slices.Contains(missing, id) {
				missing = append(missing, id)
			}
		}
	}
	extra, err := store.TasksByIDs(ctx, s.db(ctx), missing)
	if err != nil {
		return nil, err
	}
	var lockedBy map[string][]string
	if locked := lockedProjects(ctx); len(locked) > 0 && len(open) > 0 {
		if lockedBy, err = store.OpenBlockerProjects(ctx, s.writeDB(), open, locked); err != nil {
			return nil, err
		}
	}
	lookup := func(id string) (core.Task, bool) {
		if t, ok := known[id]; ok {
			return t, true
		}
		t, ok := extra[id]
		return t, ok
	}
	out := make(map[string]taskWord, len(tasks))
	for _, t := range tasks {
		out[t.ID] = wordFor(factsOf(t, lookup, len(lockedBy[t.ID]), now))
	}
	return out, nil
}

// taskBadge renders a word as a badge: the tone, the key for scripts and
// tests, the hint on hover.
func taskBadge(w taskWord) template.HTML { return wordMark("badge", w) }

// taskChip renders a word as a kind chip, for the rows that carry chips.
func taskChip(w taskWord) template.HTML { return wordMark("kind", w) }

func wordMark(class string, w taskWord) template.HTML {
	if w.Tone != "" {
		class += " " + w.Tone
	}
	out := `<span class="` + class + `" data-word="` + template.HTMLEscapeString(w.Key) + `"`
	if w.Hint != "" {
		out += ` title="` + template.HTMLEscapeString(w.Hint) + `"`
	}
	return template.HTML(out + `>` + template.HTMLEscapeString(w.Text) + `</span>`)
}

// claimLease says where a claim's lease stands as of now: "lease live, 12m
// left" while the claim is live (core.Task.ClaimLive), "lapsed 3m ago" once it
// ran out, "" with no lease. A live lease never reads as expiring "now".
func claimLease(exp *time.Time, live bool, now time.Time) string {
	if exp == nil || exp.IsZero() {
		return ""
	}
	if live {
		return "lease live, " + leaseLeft(*exp, now)
	}
	if !exp.After(now) {
		return "lapsed " + agoPhraseAt(*exp, now)
	}
	// An unexpired lease on a task that is not in progress holds nothing.
	return "lease not live"
}

// leaseLeft is durUntil for a live lease, whose last minute reads "<1m left"
// rather than "0m left".
func leaseLeft(exp, now time.Time) string {
	if d := exp.Sub(now); d > 0 && d < time.Minute {
		return "<1m left"
	}
	return durUntil(exp, now)
}

// agoPhraseAt is agoPhrase measured from now: "3m ago", or "just now".
func agoPhraseAt(t, now time.Time) string {
	a := agoAt(t, now)
	if a == "just now" || a == "now" || a == "-" {
		return a
	}
	return a + " ago"
}

// taskSession is a session a task names, as the console shows it: its name
// and, when it resolved, its id for the /console/sessions/{id} link. A value
// that resolved to no session the request reads keeps its raw text and no id,
// so the page never links to a session it cannot open.
type taskSession struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name"`
}

// holderLink resolves a claim's holder, a session id, through namer.
func holderLink(id string, namer func(string) core.Session) *taskSession {
	if id == "" {
		return nil
	}
	if sess := namer(id); sess.ID != "" {
		return &taskSession{ID: sess.ID, Name: sessionLabel(sess)}
	}
	return &taskSession{Name: id}
}

// creatorLink resolves who created a task. created_by holds the creating
// session's name (tasks_add), "console" for a plan the console approved, or
// "" for none; an older row may carry the session's id.
func (s *Service) creatorLink(ctx context.Context, by string, namer func(string) core.Session) *taskSession {
	if by == "" {
		return nil
	}
	if sess, ok, err := store.SessionByName(ctx, s.db(ctx), by); err == nil && ok {
		return &taskSession{ID: sess.ID, Name: sessionLabel(sess)}
	}
	if sess := namer(by); sess.ID != "" {
		return &taskSession{ID: sess.ID, Name: sessionLabel(sess)}
	}
	return &taskSession{Name: by}
}

// sessionLabel is a session's name, or its short id when it has none.
func sessionLabel(sess core.Session) string {
	if sess.Name != "" {
		return sess.Name
	}
	return shortID(sess.ID)
}

// taskHistoryLimit is how many of a task's newest task.transition events its
// history reads.
const taskHistoryLimit = 100

// taskHistoryEntry is one line of a task's history: what happened (Verb,
// with its icon and tone), when, and who did it -- the session, named and
// linked where the request reads it, or the owner (Owner, a console write).
// Times counts the events in a row it stands for: a holder renewing its claim
// re-claims, which reads as one claim.
type taskHistoryEntry struct {
	Verb  string       `json:"verb"`
	Icon  string       `json:"-"`
	Tone  string       `json:"tone,omitempty"`
	At    time.Time    `json:"at"`
	Who   *taskSession `json:"who,omitempty"`
	Owner bool         `json:"owner,omitempty"`
	Times int          `json:"times,omitempty"`
}

// taskHistory reads task id's history (taskHistoryEntry), oldest first, from
// its newest taskHistoryLimit task.transition events, through the request's
// handle: an event a locked project's session made is absent with it. more
// says older events were left out.
func (s *Service) taskHistory(ctx context.Context, id string, namer func(string) core.Session) ([]taskHistoryEntry, bool, error) {
	evs, err := store.TaskTransitions(ctx, s.db(ctx), id, taskHistoryLimit)
	if err != nil {
		return nil, false, err
	}
	var out []taskHistoryEntry
	for _, e := range evs {
		verb, iconName, tone := historyVerb(e.Payload)
		owner := payloadStr(e.Payload, "by") == "console"
		// A claim stamps its holder in the payload; every other transition
		// carries the acting session on the event itself.
		actor := e.SessionID
		if actor == "" {
			actor = payloadStr(e.Payload, "claimed_by")
		}
		var who *taskSession
		if !owner {
			who = holderLink(actor, namer)
		}
		if n := len(out); n > 0 && out[n-1].Verb == verb && out[n-1].Owner == owner && sameSession(out[n-1].Who, who) {
			out[n-1].Times++
			continue
		}
		out = append(out, taskHistoryEntry{Verb: verb, Icon: iconName, Tone: tone, At: e.TS, Who: who, Owner: owner, Times: 1})
	}
	return out, len(evs) >= taskHistoryLimit, nil
}

// sameSession reports two history actors as one: both none, or the same
// session as written.
func sameSession(a, b *taskSession) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.ID == b.ID && a.Name == b.Name
}

// historyVerb reads a task.transition payload as its history's verb: the
// ledger's (transitionVerb), and the gate's own moves, which an update that
// changes no status otherwise reads as an edit -- set, cleared (a gate-kind
// task's clearing closes it too) and removed.
func historyVerb(payload map[string]any) (verb, iconName, tone string) {
	if !payloadBool(payload, "created") {
		switch payloadStr(payload, "gate") {
		case "set":
			return "gate set", "clock", "warn"
		case "cleared":
			return "gate cleared", "check", "ok"
		case "removed":
			return "gate removed", "x", "muted"
		}
	}
	return transitionVerb(payload)
}
