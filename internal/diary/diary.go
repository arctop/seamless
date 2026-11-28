// Package diary is the day model behind the console's Diary: one local day
// told as what was worked on (plans, tasks and the sessions tied to neither),
// which agents did it, and what it took in agent time and model tokens, with
// the meaningful work in the spotlight and short question-and-answer sessions
// folded away.
//
// It composes facts the store already records -- the usage ledger, the
// durable event log, the sessions, tasks, memories, notes, trials and commit
// records -- and invents none. Every read takes the *sql.DB it is given, so
// the console can pass its filtered handle (store.OpenView) and a locked
// project's work is simply absent from a day. The package knows no URLs and no
// feature state: it returns facts and references (Ref), and the console builds
// the links and decides what an off feature hides.
//
// A local day D is [D 00:00, D+1 00:00) in the daemon's zone, the next
// midnight found with AddDate, never 24h arithmetic: a daylight-saving day has
// 23 or 25 hours.
//
// Tokens follow the plan-token attribution contract: a day's totals come from
// the ledger with each session counted once and are never the sum of thread
// cards (a session working two plans counts toward both cards, marked
// shared); a session with no token record at all is unreported and named,
// never a zero; a session from before the ledger has its stored total counted
// on the day it ended, marked approximate. "Model tokens" include cache reads
// and are not a billing figure.
package diary

import (
	"errors"
	"time"

	"github.com/arctop/seamless/internal/core"
)

// ErrFutureDay is a day, or a month, that has not begun.
var ErrFutureDay = errors.New("diary: the day has not begun")

// RefKind is the kind of item a Ref names: the console's route for it follows
// from the kind and the id.
type RefKind string

// The kinds of Ref.
const (
	RefTask    RefKind = "task"
	RefSession RefKind = "session"
	RefMemory  RefKind = "memory"
	RefNote    RefKind = "note"
	RefTrial   RefKind = "trial"
	RefEvent   RefKind = "event"
	RefPlan    RefKind = "plan"
	RefProject RefKind = "project"
	RefCommit  RefKind = "commit"
)

// The memory states a memory Ref's Status carries.
const (
	MemoryActive     = "active"
	MemorySuperseded = "superseded"
	MemoryArchived   = "archived"
)

// Ref is one item the day names, as a link to it needs it. Every Ref is
// resolved through the handle the day was read with, in one batched read per
// kind.
//
// Resolved is false when the handle no longer returns the id (deleted since,
// or hidden): Title is then what the event recorded (a memory's name, a
// note's title or slug, a trial's title; a task.transition records no title
// and no event a session's name, so an unresolved task's or session's is ""),
// and the screen shows the item as text. Through the console's filtered handle
// an event about an item in a hidden project is hidden with it, so most
// unresolved items are deleted ones: memories and notes (a task or a session
// is never deleted).
type Ref struct {
	Kind RefKind `json:"kind"`
	// ID is what the item's route takes: a ULID, a plan's or a project's slug,
	// or a commit's full id.
	ID string `json:"id"`
	// Title is the display title: a task's, note's or trial's title (a trial
	// with none: its lab), a memory's name, a session's name, a mishap's
	// description, a plan's or a project's slug, a commit's first 12 digits.
	Title string `json:"title"`
	// Project is the project the item is in now ("" for the global scope): a
	// moved memory names its new project. A project Ref names itself.
	Project  string `json:"project"`
	Resolved bool   `json:"resolved"`
	// Status is the state the screen marks: a task's status now (open,
	// in_progress, done, dropped), a memory's lifecycle (active, superseded,
	// archived), a session's status (active, completed, expired), a trial's
	// outcome.
	Status string `json:"status,omitempty"`
	// Retired is set on a project Ref whose project was retired (emptied by a
	// split).
	Retired bool `json:"retired,omitempty"`
	// HasPage is set on a plan Ref when the plan has a page in the Ref's
	// project: plans.Composition resolves the slug and its primary note is in
	// that project. The plan route resolves by slug alone, a task-only plan
	// has no page, and two projects may run the same slug.
	HasPage bool `json:"hasPage,omitempty"`
	// Owner is a commit Ref's owner: the task or session that recorded it (a
	// commit has no page of its own).
	Owner *Ref `json:"owner,omitempty"`
	// Href is the item's link on the screen that shows the day. The day
	// model knows no URLs and leaves it empty; the console fills it when it
	// renders a day, and leaves it empty for an item with no page to offer
	// (one that no longer resolves, or whose feature is off).
	Href string `json:"href"`
}

// Entry is one local day.
type Entry struct {
	// Day is the local date, YYYY-MM-DD; Start and End are its bounds (End is
	// the next midnight) and Hours how many hours it has: 23, 24 or 25.
	Day   string    `json:"day"`
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	Hours int       `json:"hours"`
	// Today is set while the day is still under way: every number is "so
	// far".
	Today     bool   `json:"today"`
	Headline  string `json:"headline"`
	Character Tag    `json:"character"`
	Facts     Facts  `json:"facts"`
	Usage     Usage  `json:"usage"`
	// Spotlight is the day's meaningful work, most significant first, at most
	// SpotlightCap threads unless more are pinned; Also is every other thread,
	// in the same order.
	Spotlight []Thread `json:"spotlight"`
	Also      []Thread `json:"also"`
	// Short is the minor session-days, folded into one row.
	Short   Short   `json:"short"`
	Kept    Kept    `json:"kept"`
	Moments Moments `json:"moments"`
	// SessionsStarted counts the sessions created on the day (the Sessions
	// screen's day filter lists exactly those); Facts.Sessions counts every
	// session active on it.
	SessionsStarted int `json:"sessionsStarted"`
	// Projects is every project the entry names, each once, by slug.
	Projects []Ref `json:"projects"`
}

// Facts are the day's counts: the stat tiles' numbers, and what Character and
// Headline read. Month computes the same Facts for each day from the same
// rule, so the rail and the entry agree.
type Facts struct {
	// Minutes is the day's agent time: active minutes summed over every ledger
	// row of the day, so parallel agents add up past wall-clock time.
	Minutes int `json:"minutes"`
	// Tokens and Output are the model tokens counted on the day (Usage.Tokens'
	// total and output).
	Tokens int `json:"tokens"`
	Output int `json:"output"`
	// Sessions counts the session-days: the sessions with ledger buckets,
	// credited events, or a start or an end on the day. Short counts the
	// minor ones among them (Entry.Short).
	Sessions int `json:"sessions"`
	Short    int `json:"short"`
	// Projects counts the projects of those sessions and of the tasks that
	// moved; WorkProjects leaves out the projects only minor sessions were in.
	Projects     int `json:"projects"`
	WorkProjects int `json:"workProjects"`
	// TasksClosed counts the tasks closed as done on the day (and still done
	// at its end); TasksCreated the tasks created on it.
	TasksClosed  int `json:"tasksClosed"`
	TasksCreated int `json:"tasksCreated"`
	// PlansShipped counts the plans whose last open step closed as done on the
	// day; PlansDrafted the plans captured, presented or approved on it.
	PlansShipped int `json:"plansShipped"`
	PlansDrafted int `json:"plansDrafted"`
	// Kept counts the distinct memories, notes and trials written on the day.
	Kept int `json:"kept"`
	// TopShare is the share of the day's agent time in its leading project
	// (Usage.Projects' first), in whole percent; 0 with no agent time.
	TopShare int `json:"topShare"`
	// Unreported counts the session-days with no token record; Approximate is
	// set when a session's stored total from before the ledger is counted.
	Unreported  int  `json:"unreported"`
	Approximate bool `json:"approximate"`
}

// Tag is the day's character: one word chosen by stated thresholds, and the
// rule it met, worded for the tag's tooltip.
type Tag struct {
	Word string `json:"word"`
	Rule string `json:"rule"`
}

// Usage is the day's agent and token usage, each session counted once.
type Usage struct {
	// Minutes is the agent time (Facts.Minutes).
	Minutes int `json:"minutes"`
	// Tokens is the token mix: fresh input, cache reads (cached), cache
	// writes (cacheCreation) and output, and their total.
	Tokens core.TokenUsage `json:"tokens"`
	// Sessions counts the session-days, Reported those whose tokens the day
	// counts.
	Sessions int `json:"sessions"`
	Reported int `json:"reported"`
	// Unreported names the session-days with no token record (a live Claude
	// Code session reports as its prompts are answered and at its end; one
	// ended without its SessionEnd once the daemon reads the usage it owes:
	// store usage_owed.go).
	Unreported []Ref `json:"unreported"`
	// Approximate is set when a session from before the ledger has its stored
	// total counted on the day it ended, with no hour, thread or minutes.
	Approximate bool `json:"approximate"`
	// Agents splits the day by harness and model, most agent time first.
	Agents []AgentUsage `json:"agents"`
	// Projects splits it by project, most agent time first.
	Projects []ProjectUsage `json:"projects"`
	// Hours splits it by local hour, one entry per hour of the day (23, 24 or
	// 25).
	Hours []HourUsage `json:"hours"`
}

// AgentUsage is one agent's share of a day: one harness and one model.
type AgentUsage struct {
	// Key names the agent in HourUsage.Agents: harness and model joined by a
	// slash.
	Key string `json:"key"`
	// Harness is the client discriminator (claude-code, codex), "" when the
	// session recorded none (an explicit session).
	Harness string `json:"harness"`
	// Model is verbatim as the provider names it, "" when no harvest named
	// one.
	Model    string          `json:"model"`
	Minutes  int             `json:"minutes"`
	Tokens   core.TokenUsage `json:"tokens"`
	Sessions int             `json:"sessions"`
	// Main and Subagent split the agent's ledger rows by thread, where the
	// rows say; Approximate is the stored totals of sessions from before the
	// ledger, which say no thread.
	Main        Part `json:"main"`
	Subagent    Part `json:"subagent"`
	Approximate Part `json:"approximate"`
}

// Part is the agent time and total tokens of one part of an agent's usage.
type Part struct {
	Minutes int `json:"minutes"`
	Tokens  int `json:"tokens"`
}

// ProjectUsage is one project's share of a day. Project is the slug, "" for
// sessions in no project, which Label names by their working directory's last
// path element ("global" when they name none).
type ProjectUsage struct {
	Project  string          `json:"project"`
	Label    string          `json:"label"`
	Minutes  int             `json:"minutes"`
	Tokens   core.TokenUsage `json:"tokens"`
	Sessions int             `json:"sessions"`
}

// HourUsage is one local hour of a day.
type HourUsage struct {
	// Start is the hour's start; Label its local clock time ("15:00"). On a
	// daylight-saving day a label repeats or is skipped.
	Start   time.Time `json:"start"`
	Label   string    `json:"label"`
	Minutes int       `json:"minutes"`
	Tokens  int       `json:"tokens"`
	// Agents is the hour's agent time and tokens by agent (AgentUsage.Key), in
	// Usage.Agents' order, agents with none left out.
	Agents []AgentHour `json:"agents"`
}

// AgentHour is one agent's part of an hour.
type AgentHour struct {
	Key     string `json:"key"`
	Minutes int    `json:"minutes"`
	Tokens  int    `json:"tokens"`
}

// ThreadKind is what a thread is about.
type ThreadKind string

// The kinds of thread.
const (
	// ThreadPlan is one plan in one project: the steps that moved and the
	// sessions that moved them or captured, presented or approved the plan.
	ThreadPlan ThreadKind = "plan"
	// ThreadProject is one project: its tasks outside any plan that moved and
	// its sessions attributed to no plan. Sessions in no project form a
	// project thread per working directory.
	ThreadProject ThreadKind = "project"
)

// Thread is the unit the day is written in: what was worked on.
type Thread struct {
	// Key is stable across renders: plan:<project>:<slug>, project:<project>,
	// or cwd:<directory> for sessions in no project.
	Key  string     `json:"key"`
	Kind ThreadKind `json:"kind"`
	// Label is the plan's slug, the project's slug, the directory's name, or
	// "global".
	Label   string `json:"label"`
	Project string `json:"project"`
	// Plan is a plan thread's plan (HasPage says whether it has a page).
	Plan *Ref `json:"plan,omitempty"`
	// Moved is the thread's tasks that changed on the day, in the order they
	// last changed.
	Moved []Move `json:"moved"`
	// Progress is a plan thread's steps closed of all, at the day's start and
	// its end; Shipped is set when its last open step closed as done on the
	// day.
	Progress *Progress `json:"progress,omitempty"`
	Shipped  bool      `json:"shipped,omitempty"`
	// Sessions is the thread's sessions, minor ones folded into Entry.Short.
	Sessions []SessionDay `json:"sessions"`
	// Minutes and Tokens are its sessions' usage on the day; a shared session
	// counts in full toward every thread it is in, so threads never sum to the
	// day. Shared counts its sessions that are in another thread too, and
	// Unreported those with no token record.
	Minutes    int             `json:"minutes"`
	Tokens     core.TokenUsage `json:"tokens"`
	Shared     int             `json:"shared"`
	Unreported int             `json:"unreported"`
	// Hours is its agent time by local hour, aligned with Usage.Hours.
	Hours []int `json:"hours"`
	// Kept is what its sessions wrote; Commits what its tasks and sessions
	// recorded.
	Kept    []Ref `json:"kept"`
	Commits []Ref `json:"commits"`
	// Score is its significance (the weights below); Reasons the facts that
	// scored, for the screen's "why this is here". Pinned is set when a
	// flagged session pins it into the spotlight.
	Score   float64  `json:"score"`
	Reasons []Reason `json:"reasons"`
	Pinned  bool     `json:"pinned,omitempty"`
}

// Move is one task that changed on the day.
type Move struct {
	Task Ref `json:"task"`
	// From is its status at the day's start, "" when it was created on the
	// day; To its status at the day's end (so far, today). They are equal
	// when the day changed it and changed it back, or only edited it.
	From string `json:"from"`
	To   string `json:"to"`
	// At is when it last changed on the day.
	At time.Time `json:"at"`
	// Created, Claimed and Closed say it was created, claimed, or closed as
	// done on the day.
	Created bool `json:"created,omitempty"`
	Claimed bool `json:"claimed,omitempty"`
	Closed  bool `json:"closed,omitempty"`
	// Sessions is the sessions that changed it on the day, by id.
	Sessions []string `json:"sessions"`
}

// Progress is a plan's steps closed of all at the day's start and end.
type Progress struct {
	Start Count `json:"start"`
	End   Count `json:"end"`
}

// Count is a plan's closed steps (done or dropped) of all its steps.
type Count struct {
	Closed int `json:"closed"`
	Total  int `json:"total"`
}

// SessionDay is one session's part of a day.
type SessionDay struct {
	Session Ref `json:"session"`
	// Harness and Model are the session's client discriminator and model, as
	// the Sessions screen shows them.
	Harness string `json:"harness"`
	Model   string `json:"model"`
	// Minutes, Tokens and Hours are its usage on the day (Hours aligned with
	// Usage.Hours).
	Minutes int             `json:"minutes"`
	Tokens  core.TokenUsage `json:"tokens"`
	Hours   []int           `json:"hours"`
	// Unreported is set when the session has no token record at all.
	// Approximate when it is from before the ledger: its stored total counts
	// on the day it ended (EndedOn) and on no other.
	Unreported  bool   `json:"unreported,omitempty"`
	Approximate bool   `json:"approximate,omitempty"`
	EndedOn     string `json:"endedOn,omitempty"`
	// Started and Ended say it began or ended on the day.
	Started bool `json:"started,omitempty"`
	Ended   bool `json:"ended,omitempty"`
	// About is what the session was about, and AboutFrom where that came
	// from (About's constants).
	About     string `json:"about"`
	AboutFrom string `json:"aboutFrom"`
	// Flag is the session's verdict (good, bad, notable), "" when unflagged.
	Flag string `json:"flag,omitempty"`
	// Shared is set when the session is in more than one thread.
	Shared bool `json:"shared,omitempty"`
	// Minor is set when the session-day is short: no outcome, under
	// MinorMinutes of agent time and under MinorOutput output tokens.
	Minor bool `json:"minor,omitempty"`
}

// Where SessionDay.About came from, first that exists.
const (
	// AboutFindings is the first line of findings the agent wrote.
	AboutFindings = "findings"
	// AboutPrompt is the session's first prompt of the day.
	AboutPrompt = "prompt"
	// AboutHarvested is the first line of findings harvested from its
	// transcript, without the harvest's prefix.
	AboutHarvested = "harvested"
	// AboutDefault is "Session in <project>".
	AboutDefault = "default"
)

// Short is the day's minor session-days, folded into one collapsed row.
type Short struct {
	Count    int          `json:"count"`
	Minutes  int          `json:"minutes"`
	Sessions []SessionDay `json:"sessions"`
}

// Kept is what the day kept: the distinct memories, notes and trials written
// on it. An item written several times is one item, its Writes counting them.
type Kept struct {
	Memories []KeptItem `json:"memories"`
	Notes    []KeptItem `json:"notes"`
	Trials   []KeptItem `json:"trials"`
	// New and Updated split them: created on the day, or written again.
	New     int `json:"new"`
	Updated int `json:"updated"`
}

// KeptItem is one item written on the day.
type KeptItem struct {
	Item Ref `json:"item"`
	// New is set when the item was created on the day.
	New bool `json:"new"`
	// Writes counts its writes on the day; At is the last.
	Writes int       `json:"writes"`
	At     time.Time `json:"at"`
	// Sessions is the sessions that wrote it, by id (none for the owner's or
	// the gardener's writes).
	Sessions []string `json:"sessions"`
}

// Moments are the day's events worth a mark of their own.
type Moments struct {
	// Shipped is the plans shipped on the day.
	Shipped []Ref `json:"shipped"`
	// Commits is the commits recorded on the day (the commits.record option),
	// each with its owner.
	Commits []Ref `json:"commits"`
	// Flags is the flags set on the day.
	Flags []Flag `json:"flags"`
	// Mishaps is what the agents self-reported at session_end.
	Mishaps []Mishap `json:"mishaps"`
}

// Flag is one session flag set on the day.
type Flag struct {
	Session Ref       `json:"session"`
	Verdict string    `json:"verdict"`
	Reason  string    `json:"reason,omitempty"`
	At      time.Time `json:"at"`
}

// Mishap is one self-reported mishap: the event (its page), and the session
// that reported it. Type is the kind the report named (a core.MishapTypes
// slug), "" for one that named none; a typed report's event title is its
// text without the type prefix.
type Mishap struct {
	Event   Ref       `json:"event"`
	Session Ref       `json:"session"`
	At      time.Time `json:"at"`
	Type    string    `json:"type,omitempty"`
}

// Reason is one fact that scored a thread.
type Reason struct {
	// Kind is the fact (the Reason constants), Count how many there were and
	// Points what they scored after the cap.
	Kind   string  `json:"kind"`
	Count  int     `json:"count"`
	Points float64 `json:"points"`
}

// The facts a Reason names.
const (
	ReasonShipped  = "plan_shipped"
	ReasonClosed   = "tasks_closed"
	ReasonStarted  = "tasks_started"
	ReasonKept     = "items_kept"
	ReasonCommits  = "commits"
	ReasonFindings = "findings"
	ReasonMinutes  = "agent_minutes"
	ReasonOutput   = "output_tokens"
	ReasonFlagged  = "flagged"
)

// MonthView is one local month for the rail and the heat grid: a row per day
// that has begun, oldest first.
type MonthView struct {
	// Month is YYYY-MM; Start and End its bounds.
	Month string     `json:"month"`
	Start time.Time  `json:"start"`
	End   time.Time  `json:"end"`
	Days  []MonthDay `json:"days"`
	// Agents is the month's usage by agent, keyed and ordered as each day's
	// Usage.Agents: every day of it summed, an agent's sessions counted once
	// over the month. The screen picks each agent's color from it, so an
	// agent wears one color on every day of the month.
	Agents []AgentUsage `json:"agents"`
}

// MonthDay is one day of a month: its Facts, its top projects by agent time
// (at most MonthProjects, ranked as Usage.Projects), and its character.
type MonthDay struct {
	Day       string         `json:"day"`
	Facts     Facts          `json:"facts"`
	Projects  []ProjectUsage `json:"projects"`
	Character Tag            `json:"character"`
}

// UsualDay is the owner's usual day before a day, for the stat tiles' deltas:
// the median over the trailing active days (at most UsualDays of them, within
// UsualLookbackDays). Has is false with fewer than UsualMinDays: no
// comparison, never a zeroed one.
type UsualDay struct {
	Has bool `json:"has"`
	// Days is how many active days the comparison reads, oldest first in
	// Series.
	Days        int        `json:"days"`
	Minutes     int        `json:"minutes"`
	Tokens      int        `json:"tokens"`
	TasksClosed int        `json:"tasksClosed"`
	Kept        int        `json:"kept"`
	Series      []MonthDay `json:"series"`
}
