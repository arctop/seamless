package console

// The System screen: every part of the daemon drawn as a structure on one 3D
// deck (static/orrery.js), each with the state and the numbers this server
// knows about it. The server owns every state, word and number; the script
// owns the picture and the motion.
//
// Motion follows the event log. A structure animates only when an event says
// it worked, and light runs along a trace only along the path that event's
// data took (systemChains). With nothing recorded the deck is still and the
// page draws no frames.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/features"
	"github.com/arctop/seamless/internal/store"
)

// Deployment is what the console cannot read from a request about how this
// daemon runs: where it listens, whether the peer_auth socket is up, which OS
// accounts are involved, and which optional writers are configured. The
// daemon fills it at serve start; a zero value describes a default install
// (owner account, no socket), and the screens say only what it states.
type Deployment struct {
	// Addr is the address the HTTP listener bound.
	Addr string
	// PeerAuth reports peer_auth.enabled; SocketPath is its socket.
	PeerAuth   bool
	SocketPath string
	// Owner is the OS account the bearer key stands for (peer_auth.owner),
	// and DaemonUser the account running seamlessd. With peer_auth off both
	// are the account that started the daemon.
	Owner      string
	DaemonUser string
	// Restricted lists peer_auth.restricted accounts by name.
	Restricted []string
	// MCPTools is how many MCP tools the owner's connection is offered.
	MCPTools int
	// CommitsRecord reports commits.record; FeedSocket reports a configured
	// screening.feed_socket.
	CommitsRecord bool
	FeedSocket    bool
	// ChatModel names the chat model the gardener's requests use, when one is
	// configured.
	ChatModel string
}

// The rings, read from the rim inward, and the services off the deck.
const (
	ringDoors = "doors"
	ringWork  = "work"
	ringCore  = "core"
	ringSky   = "sky"
)

// Part states. "absent" is a part this build or deployment does not have at
// all (the update check on a source build), as opposed to one switched off.
const (
	partOn     = "on"
	partIdle   = "idle"
	partWarn   = "warn"
	partOff    = "off"
	partAbsent = "absent"
)

type systemStat struct {
	Label string `json:"label"`
	Value string `json:"value"`
	Unit  string `json:"unit,omitempty"`
}

type systemFact struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

type systemHow struct {
	Text string `json:"text"`
	Code string `json:"code,omitempty"`
}

// systemPart is one structure on the deck and its readout.
type systemPart struct {
	ID        string       `json:"id"`
	Label     string       `json:"label"`
	Ring      string       `json:"ring"`
	State     string       `json:"state"`
	Role      string       `json:"role"`
	What      string       `json:"what"`
	Hero      systemStat   `json:"hero"`
	Metrics   []systemStat `json:"metrics,omitempty"`
	Facts     []systemFact `json:"facts,omitempty"`
	Talks     []string     `json:"talks,omitempty"`
	TurnOn    *systemHow   `json:"turnOn,omitempty"`
	Href      string       `json:"href,omitempty"`
	HrefLabel string       `json:"hrefLabel,omitempty"`
	// Count is a quantity the structure shows (live agents, waiting
	// proposals, held writes); nil shows none.
	Count *int `json:"count,omitempty"`
	// Open lifts the approvals lock while a lease is live.
	Open bool `json:"open,omitempty"`
	// Spark is events per minute over the last SparkMinutes minutes, oldest
	// first; SparkLabel says what it counts.
	Spark      []int  `json:"spark,omitempty"`
	SparkLabel string `json:"sparkLabel,omitempty"`
}

// StateWord is the state as the readout says it.
func (p systemPart) StateWord() string { return partStateWord(p.State) }

func partStateWord(s string) string {
	switch s {
	case partOn:
		return "Running"
	case partIdle:
		return "Quiet"
	case partWarn:
		return "Needs attention"
	case partAbsent:
		return "Not in this build"
	}
	return "Off"
}

// systemChain is the path one kind of event's data takes across the deck:
// light runs hop to hop, and Also adds the side trips (an embedding to the
// provider).
type systemChain struct {
	Hops []string   `json:"hops"`
	Also [][]string `json:"also,omitempty"`
}

// systemChains maps every event kind the deck shows to the path its data
// took. A kind not listed still lights the event log, and nothing else.
var systemChains = map[core.EventKind]systemChain{
	core.EventToolCall:            {Hops: []string{"listener", "mcp"}},
	core.EventRecallMiss:          {Hops: []string{"mcp", "retrieval", "index"}},
	core.EventInjected:            {Hops: []string{"hooks", "retrieval", "database"}},
	core.EventHookPrompt:          {Hops: []string{"hooks", "retrieval"}},
	core.EventHookError:           {Hops: []string{"hooks"}},
	core.EventSessionStarted:      {Hops: []string{"hooks", "sessions", "database"}},
	core.EventSessionEnded:        {Hops: []string{"hooks", "sessions", "database"}},
	core.EventMemoryWritten:       {Hops: []string{"mcp", "memories", "watcher", "database", "index"}, Also: [][]string{{"database", "embeddings"}, {"embeddings", "provider"}}},
	core.EventMemoryRead:          {Hops: []string{"mcp", "retrieval", "database"}},
	core.EventMemorySuperseded:    {Hops: []string{"lifecycle", "memories", "watcher", "database"}},
	core.EventMemoryArchived:      {Hops: []string{"lifecycle", "memories", "watcher", "database"}},
	core.EventMemoryMoved:         {Hops: []string{"memories", "watcher", "database"}},
	core.EventNoteWritten:         {Hops: []string{"notes", "watcher", "database", "index"}, Also: [][]string{{"database", "embeddings"}, {"embeddings", "provider"}}},
	core.EventNoteRead:            {Hops: []string{"mcp", "retrieval", "database"}},
	core.EventTaskTransition:      {Hops: []string{"mcp", "tasks", "database"}},
	core.EventTrialRecorded:       {Hops: []string{"labs", "database"}},
	core.EventGardenerAction:      {Hops: []string{"gardener", "memories"}},
	core.EventFavoriteChanged:     {Hops: []string{"database"}},
	core.EventPlanCaptured:        {Hops: []string{"hooks", "retrieval"}},
	core.EventPlanPresented:       {Hops: []string{"hooks"}},
	core.EventPlanApproved:        {Hops: []string{"hooks", "tasks", "database"}},
	core.EventPlanShipped:         {Hops: []string{"tasks", "database"}},
	core.EventSubagentCaptured:    {Hops: []string{"hooks", "notes"}},
	core.EventMemoryScreened:      {Hops: []string{"screening", "memories"}},
	core.EventMemoryHeld:          {Hops: []string{"screening"}},
	core.EventMemoryReleased:      {Hops: []string{"screening", "memories", "watcher", "database"}},
	core.EventMemoryDiscarded:     {Hops: []string{"screening"}},
	core.EventCorpusDrift:         {Hops: []string{"watcher", "screening"}},
	core.EventCorpusReviewed:      {Hops: []string{"screening"}},
	core.EventCorpusConfirmed:     {Hops: []string{"screening"}},
	core.EventLockdownChanged:     {Hops: []string{"screening"}},
	core.EventSessionTainted:      {Hops: []string{"screening"}},
	core.EventSessionCleared:      {Hops: []string{"screening"}},
	core.EventAccessRefused:       {Hops: []string{"approvals"}},
	core.EventAccessRequested:     {Hops: []string{"socket", "approvals", "database"}},
	core.EventAccessApproved:      {Hops: []string{"approvals", "database"}},
	core.EventAccessDenied:        {Hops: []string{"approvals"}},
	core.EventAccessRevoked:       {Hops: []string{"approvals", "database"}},
	core.EventAccessGranted:       {Hops: []string{"approvals", "database"}},
	core.EventAccessConfined:      {Hops: []string{"approvals"}},
	core.EventUpdateAvailable:     {Hops: []string{"updates"}, Also: [][]string{{"updates", "github"}}},
	core.EventUpdateStarted:       {Hops: []string{"updates", "backups"}},
	core.EventUpdateApplied:       {Hops: []string{"updates"}},
	core.EventUpdateFailed:        {Hops: []string{"updates"}},
	core.EventBackupCreated:       {Hops: []string{"backups"}},
	core.EventBackupFailed:        {Hops: []string{"backups"}},
	core.EventBackupDeleted:       {Hops: []string{"backups"}},
	core.EventBackupRestored:      {Hops: []string{"backups", "database"}},
	core.EventBackupRestoreFailed: {Hops: []string{"backups"}},
}

// systemData is the System screen's payload: the parts in deck order, the
// tally, and the event paths the script animates live events along.
type systemData struct {
	Version      string                 `json:"version"`
	Host         string                 `json:"host"`
	Parts        []systemPart           `json:"parts"`
	Tally        map[string]int         `json:"tally"`
	Chains       map[string]systemChain `json:"chains"`
	SparkMinutes int                    `json:"sparkMinutes"`
	At           time.Time              `json:"at"`
}

// Rings lists the rings from the rim inward, then the services off the deck.
func (d systemData) Rings() []string { return []string{ringDoors, ringWork, ringCore, ringSky} }

// RingParts lists the parts of one ring, for the server-rendered index.
func (d systemData) RingParts(ring string) []systemPart {
	var out []systemPart
	for _, p := range d.Parts {
		if p.Ring == ring {
			out = append(out, p)
		}
	}
	return out
}

// RingOn counts the running parts of one ring.
func (d systemData) RingOn(ring string) int {
	n := 0
	for _, p := range d.RingParts(ring) {
		if p.State == partOn || p.State == partIdle || p.State == partWarn {
			n++
		}
	}
	return n
}

const systemSparkMinutes = 30

func (s *Service) systemPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "system", pageData{Title: "System", Active: "system", Data: s.systemData(r.Context(), time.Now())})
}

// partActivity is one part's recent event activity, read from the log.
type partActivity struct {
	spark  []int
	recent int // events in the last 15 minutes
	day    int // events in the last 24 hours
}

// systemActivity attributes every recent event to the parts its data
// crossed, as per-minute counts. A read failure costs the lines, never the
// page.
func (s *Service) systemActivity(ctx context.Context, now time.Time) (map[string]*partActivity, map[string]int) {
	act := map[string]*partActivity{}
	get := func(id string) *partActivity {
		a := act[id]
		if a == nil {
			a = &partActivity{spark: make([]int, systemSparkMinutes)}
			act[id] = a
		}
		return a
	}
	db := s.db(ctx)
	day, err := store.EventCountsByKindSince(ctx, db, now.Add(-24*time.Hour))
	if err != nil {
		s.logger.Warn("console: system activity", "error", err)
		day = map[string]int{}
	}
	for kind, n := range day {
		get("events").day += n
		for _, id := range chainParts(core.EventKind(kind)) {
			get(id).day += n
		}
	}
	ticks, err := store.EventKindTimesSince(ctx, db, now.Add(-systemSparkMinutes*time.Minute), 20000)
	if err != nil {
		s.logger.Warn("console: system activity", "error", err)
	}
	for _, t := range ticks {
		ago := int(now.Sub(t.At) / time.Minute)
		if ago < 0 || ago >= systemSparkMinutes {
			continue
		}
		ids := append([]string{"events"}, chainParts(core.EventKind(t.Kind))...)
		for _, id := range ids {
			a := get(id)
			a.spark[systemSparkMinutes-1-ago]++
			if ago < 15 {
				a.recent++
			}
		}
	}
	return act, day
}

// chainParts lists every part a kind's data crosses, once each.
func chainParts(kind core.EventKind) []string {
	c, ok := systemChains[kind]
	if !ok {
		return nil
	}
	out := slices.Clone(c.Hops)
	for _, pair := range c.Also {
		for _, id := range pair {
			if !slices.Contains(out, id) {
				out = append(out, id)
			}
		}
	}
	return out
}

func intPtr(n int) *int { return &n }

func countWord(n int) string {
	if n >= 10000 {
		return compactNumber(n)
	}
	return fmt.Sprintf("%d", n)
}

// compactNumber writes 12400 as "12.4k".
func compactNumber(n int) string {
	switch {
	case n >= 1000000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprintf("%d", n)
}

// systemData assembles every part. Each read is best-effort: a failed one
// costs the numbers it feeds, never the screen.
func (s *Service) systemData(ctx context.Context, now time.Time) systemData {
	db := s.db(ctx)
	dep := s.cfg.Deployment
	feats := s.effectiveFeatures(ctx)
	act, day := s.systemActivity(ctx, now)
	activity := func(id string) *partActivity {
		if a := act[id]; a != nil {
			return a
		}
		return &partActivity{spark: make([]int, systemSparkMinutes)}
	}
	nav, err := store.GetNavCounts(ctx, db)
	if err != nil {
		s.logger.Warn("console: system counts", "error", err)
	}
	// running: on while the log shows work in the last 15 minutes, quiet
	// otherwise. warn and off/absent are decided by each part.
	running := func(id string) string {
		if activity(id).recent > 0 {
			return partOn
		}
		return partIdle
	}

	var parts []systemPart
	add := func(p systemPart) {
		if p.State == partOn || p.State == partIdle || p.State == partWarn {
			if a := act[p.ID]; a != nil && p.SparkLabel != "" {
				p.Spark = a.spark
			} else if p.SparkLabel != "" {
				p.Spark = make([]int, systemSparkMinutes)
			}
		} else {
			p.SparkLabel = ""
		}
		parts = append(parts, p)
	}

	// ---- doors -------------------------------------------------------------

	tools := dep.MCPTools
	mcp := systemPart{
		ID: "mcp", Label: "MCP", Ring: ringDoors, State: running("mcp"),
		Role:       "The tools your agents call.",
		What:       "Memory, notes, recall, tasks, plans, labs and the gardener, served to Claude Code, Codex and any MCP client at /api/mcp. Every call is checked against the project its session is bound to before it runs.",
		Hero:       systemStat{Value: countWord(day[string(core.EventToolCall)]), Label: "tool calls in the last 24 hours"},
		SparkLabel: "Tool calls per minute",
		Talks:      []string{"listener", "retrieval", "tasks", "memories"},
		Facts:      []systemFact{{"Endpoint", "/api/mcp"}, {"Transport", "streamable HTTP"}},
		Href:       "/console/interactions", HrefLabel: "Open Interactions",
	}
	if tools > 0 {
		mcp.Metrics = append(mcp.Metrics, systemStat{Label: "Tools offered", Value: fmt.Sprint(tools)})
	}
	mcp.Metrics = append(mcp.Metrics, systemStat{Label: "Missed recalls today", Value: countWord(day[string(core.EventRecallMiss)])})
	if s.cfg.APIKey == "" && !dep.PeerAuth {
		mcp.State = partWarn
		mcp.Role = "No bearer key is set, so it refuses every call."
		mcp.TurnOn = &systemHow{Text: "Set mcp.api_key in the config file and restart the daemon.", Code: "mcp:\n  api_key: \"<a long random key>\""}
	}
	add(mcp)

	facts, ferr := store.GetHealthFacts(ctx, db)
	if ferr != nil {
		s.logger.Warn("console: system hooks", "error", ferr)
	}
	hooks := systemPart{
		ID: "hooks", Label: "Hooks", Ring: ringDoors, State: running("hooks"),
		Role:       "Claude Code and Codex check in here.",
		What:       "At session start the hook delivers a briefing; on every prompt it adds the memories that prompt needs; at the end it files the session's findings. Plan-mode saves are captured here too.",
		Hero:       systemStat{Value: countWord(day[string(core.EventInjected)]), Label: "briefings and prompt matches in the last 24 hours"},
		SparkLabel: "Hook calls per minute",
		Talks:      []string{"sessions", "retrieval", "listener"},
		Metrics: []systemStat{
			{Label: "Sessions started today", Value: countWord(day[string(core.EventSessionStarted)])},
			{Label: "Prompts with no match", Value: countWord(day[string(core.EventHookPrompt)])},
			{Label: "Hook errors today", Value: countWord(day[string(core.EventHookError)])},
		},
		Href: "/console/sessions", HrefLabel: "Open Sessions",
	}
	if len(facts.Clients) > 0 {
		names := make([]string, 0, len(facts.Clients))
		for _, c := range facts.Clients {
			_, _, name := agentDisplay(c.Client)
			names = append(names, name)
		}
		hooks.Facts = append(hooks.Facts, systemFact{"Clients seen", joinWithAnd(names)})
	}
	if b := facts.LastBriefing; b != nil {
		hooks.Facts = append(hooks.Facts, systemFact{"Last briefing", agoPhrase(b.At)})
	}
	if day[string(core.EventHookError)] > 0 {
		hooks.State = partWarn
	}
	add(hooks)

	socket := systemPart{
		ID: "socket", Label: "Socket", Ring: ringDoors, State: partOff,
		Role:  "A Unix socket that knows who is calling.",
		What:  "With peer_auth on, Seamless also listens on a Unix socket and names each caller by the OS account the kernel reports. That is what lets a project be locked, or a second account work only in open projects.",
		Hero:  systemStat{Value: "Off", Label: "peer_auth is not configured"},
		Talks: []string{"approvals"},
		Facts: []systemFact{{"Setting", "peer_auth.enabled: false"}},
		TurnOn: &systemHow{Text: "Run the daemon as its own account and turn peer_auth on. The Privacy screen shows what changes.",
			Code: "peer_auth:\n  enabled: true\n  owner: <your account>\n  socket:\n    path: /opt/seamless/run/seamlessd.sock"},
		Href: "/console/privacy", HrefLabel: "Open Privacy",
	}
	if dep.PeerAuth {
		socket.State, socket.TurnOn = partIdle, nil
		socket.Hero = systemStat{Value: "On", Label: "callers are named by their OS account"}
		socket.Facts = []systemFact{{"Path", dep.SocketPath}, {"Owner account", dep.Owner}}
		if len(dep.Restricted) > 0 {
			socket.Facts = append(socket.Facts, systemFact{"Restricted accounts", strings.Join(dep.Restricted, ", ")})
		}
	}
	add(socket)

	listener := systemPart{
		ID: "listener", Label: "Listener", Ring: ringDoors, State: running("listener"),
		Role:       "HTTP on this machine.",
		What:       "Every request from agents, hooks and this console arrives here, and only for the hosts it was allowed to answer.",
		Hero:       systemStat{Value: countWord(day[string(core.EventToolCall)] + day[string(core.EventInjected)] + day[string(core.EventHookPrompt)]), Label: "agent requests in the last 24 hours"},
		SparkLabel: "Requests per minute",
		Talks:      []string{"mcp", "hooks", "console", "a2a"},
	}
	if dep.Addr != "" {
		listener.Facts = append(listener.Facts, systemFact{"Address", dep.Addr})
	}
	listener.Facts = append(listener.Facts, systemFact{"TLS", onOff(s.cfg.SecureCookies)})
	add(listener)

	lvl := s.consoleLevel(ctx)
	add(systemPart{
		ID: "console", Label: "Console", Ring: ringDoors, State: partOn,
		Role:  "This screen, and every other page you read.",
		What:  "Read-mostly: the writes are your own overrides and curation, like archiving a memory or approving a plan. The live stream sends each recorded event to the tabs you have open.",
		Hero:  systemStat{Value: lvl.Level.Label(), Label: "console level"},
		Talks: []string{"listener", "events"},
		Facts: []systemFact{{"Address", "/console"}, {"Version", orDash(s.cfg.Version)}},
		Href:  "/console/settings?s=experience", HrefLabel: "Open Settings",
	})

	add(systemPart{
		ID: "a2a", Label: "A2A", Ring: ringDoors, State: partIdle,
		Role:  "Other agents ask Seamless as a peer.",
		What:  "The agent-to-agent endpoint: a published agent card and a JSON-RPC surface any A2A client can query for briefings and recall, under the same key and the same fences.",
		Hero:  systemStat{Value: "Ready", Label: "answers at /api/a2a"},
		Talks: []string{"listener", "retrieval"},
		Facts: []systemFact{{"Agent card", "/.well-known/agent-card.json"}, {"Endpoint", "/api/a2a"}},
	})

	// ---- work --------------------------------------------------------------

	add(systemPart{
		ID: "retrieval", Label: "Retrieval", Ring: ringWork, State: running("retrieval"),
		Role:       "Finds what each agent needs to know.",
		What:       "Builds the briefing at session start, matches every prompt to the memories it needs, and answers recall by fusing keyword and meaning search.",
		Hero:       systemStat{Value: countWord(day[string(core.EventInjected)]), Label: "deliveries in the last 24 hours"},
		SparkLabel: "Deliveries per minute",
		Metrics: []systemStat{
			{Label: "Memories read today", Value: countWord(day[string(core.EventMemoryRead)])},
			{Label: "Missed recalls", Value: countWord(day[string(core.EventRecallMiss)])},
		},
		Talks: []string{"mcp", "hooks", "database", "embeddings"},
		Href:  "/console/retrieval", HrefLabel: "Open Retrieval",
	})

	plans := nav.Plans
	add(systemPart{
		ID: "tasks", Label: "Tasks & plans", Ring: ringWork, State: running("tasks"),
		Role:       "The ready queue, with claims that expire.",
		What:       "Tasks with no unfinished blocker are ready. An agent claims one with a lease; a lease that runs out frees the task for someone else.",
		Hero:       systemStat{Value: countWord(nav.OpenTasks), Label: "open tasks"},
		SparkLabel: "Task changes per minute",
		Metrics: []systemStat{
			{Label: "Changes today", Value: countWord(day[string(core.EventTaskTransition)])},
			{Label: "Plans", Value: countWord(plans)},
		},
		Talks: []string{"mcp", "database"},
		Href:  "/console/tasks", HrefLabel: "Open Tasks",
	})

	live, lerr := store.LiveSessionCount(ctx, db, now.Add(-s.idleTTL()))
	if lerr != nil {
		s.logger.Warn("console: system sessions", "error", lerr)
	}
	add(systemPart{
		ID: "sessions", Label: "Sessions", Ring: ringWork, State: running("sessions"), Count: intPtr(live),
		Role:       "Every agent conversation, start to finish.",
		What:       "A session is one agent conversation. Its findings feed the next briefing, and the reaper ends one that stays idle past the threshold.",
		Hero:       systemStat{Value: fmt.Sprint(live), Label: "agents live now"},
		SparkLabel: "Session starts and ends per minute",
		Metrics: []systemStat{
			{Label: "Started today", Value: countWord(day[string(core.EventSessionStarted)])},
			{Label: "All sessions", Value: countWord(nav.Sessions)},
			{Label: "Idle after", Value: fmt.Sprint(int(s.idleTTL().Minutes())), Unit: "min"},
		},
		Talks: []string{"hooks", "database"},
		Href:  "/console/now", HrefLabel: "Open Now",
	})

	labs := systemPart{
		ID: "labs", Label: "Labs", Ring: ringWork, State: running("labs"),
		Role:       "Experiments you can compare across agents.",
		What:       "A lab holds repeated trials of one investigation, each with an expected and an actual outcome, so a later agent sees what was already ruled out.",
		Hero:       systemStat{Value: countWord(nav.Labs), Label: "labs"},
		SparkLabel: "Trials recorded per minute",
		Metrics:    []systemStat{{Label: "Trials", Value: countWord(nav.Trials)}},
		Talks:      []string{"database"},
		Href:       "/console/labs", HrefLabel: "Open Labs",
	}
	if !features.Enabled(feats, features.Research) {
		labs.State, labs.Hero, labs.Metrics, labs.Href = partOff, systemStat{Value: "Off", Label: "the research feature is off"}, nil, ""
		labs.TurnOn = &systemHow{Text: "Turn on Research in Settings, Features.", Code: "features:\n  research: true"}
	}
	add(labs)

	add(s.approvalsPart(ctx, now))
	add(s.screeningPart(ctx, feats, running("screening")))
	add(s.updatesPart(running("updates")))

	commits := systemPart{
		ID: "commits", Label: "Commit records", Ring: ringWork, State: partOff,
		Role:   "Records the commits agents made.",
		What:   "Lets session_end and tasks_update record commit ids, and writes a snapshot an integrity monitor can match commits against.",
		Hero:   systemStat{Value: "Off", Label: "commit records are not configured"},
		Talks:  []string{"database"},
		TurnOn: &systemHow{Text: "Turn recording on in the config file and restart the daemon.", Code: "commits:\n  record: true"},
	}
	if dep.CommitsRecord {
		commits.State, commits.TurnOn = partIdle, nil
		commits.Hero = systemStat{Value: "On", Label: "sessions and tasks record their commits"}
	}
	add(commits)

	feed := systemPart{
		ID: "feed", Label: "Metadata feed", Ring: ringWork, State: partOff,
		Role:   "Streams event metadata to a monitor.",
		What:   "Sends each event as a metadata frame (ids, kinds and sizes, never content) to a Unix socket a security monitor reads.",
		Hero:   systemStat{Value: "Off", Label: "no feed socket is configured"},
		Talks:  []string{"events"},
		TurnOn: &systemHow{Text: "Needs write screening, then a socket path.", Code: "screening:\n  feed_socket: /path/to/monitor.sock"},
	}
	if dep.FeedSocket {
		feed.State, feed.TurnOn = partIdle, nil
		feed.Hero = systemStat{Value: "On", Label: "frames go to the monitor's socket"}
	}
	add(feed)

	add(systemPart{
		ID: "capture", Label: "URL capture", Ring: ringWork, State: partIdle,
		Role:  "Saves a web page as a note.",
		What:  "Fetches a URL for capture_url and stores it as a note with its source. It refuses private addresses, localhost and file links.",
		Hero:  systemStat{Value: "Ready", Label: "agents call capture_url"},
		Talks: []string{"notes"},
		Facts: []systemFact{{"Guard", "no private or local addresses"}},
	})

	gard := systemPart{
		ID: "gardener", Label: "Gardener", Ring: ringWork, State: running("gardener"), Count: intPtr(min(nav.PendingProposals, 5)),
		Role:       "Tidies knowledge, but only proposes.",
		What:       "On a schedule it looks for duplicates, stale memories, dead weight and finished plans, and files proposals. Nothing changes until you apply one.",
		Hero:       systemStat{Value: countWord(nav.PendingProposals), Label: "proposals waiting for you"},
		SparkLabel: "Gardener actions per minute",
		Metrics:    []systemStat{{Label: "Runs every", Value: fmt.Sprint(s.cfg.GardenerCfg.IntervalMinutes), Unit: "min"}},
		Talks:      []string{"memories", "a2a"},
		Href:       "/console/gardener", HrefLabel: "Open Gardener",
	}
	if !s.cfg.GardenerCfg.Enabled {
		gard.State, gard.Count, gard.Metrics = partOff, nil, nil
		gard.Hero = systemStat{Value: "Off", Label: "the gardener is switched off"}
		gard.TurnOn = &systemHow{Text: "Switch it on in the config file and restart the daemon.", Code: "gardener:\n  enabled: true"}
	}
	add(gard)

	add(systemPart{
		ID: "lifecycle", Label: "Lifecycle", Ring: ringWork, State: running("lifecycle"),
		Role:       "Supersession and archive.",
		What:       "When a memory is replaced, the old one leaves every index but stays readable on disk, pointing at its replacement.",
		Hero:       systemStat{Value: countWord(day[string(core.EventMemorySuperseded)] + day[string(core.EventMemoryArchived)]), Label: "memories retired in the last 24 hours"},
		SparkLabel: "Retirements per minute",
		Talks:      []string{"memories"},
	})

	// ---- core --------------------------------------------------------------

	dbp := s.databasePanel()
	add(systemPart{
		ID: "database", Label: "Database", Ring: ringCore, State: running("database"),
		Role:       "SQLite, the record of everything that moves.",
		What:       "Holds sessions, tasks, trials, events and embeddings, and indexes the memory and note files. The files stay the source of truth; this can be rebuilt from them.",
		Hero:       systemStat{Value: dbp.SizeHuman, Label: "seam.db on disk, with its write-ahead log"},
		SparkLabel: "Writes per minute",
		Metrics: []systemStat{
			{Label: "Schema", Value: fmt.Sprint(dbp.SchemaVersion)},
			{Label: "Events today", Value: countWord(activity("events").day)},
		},
		Facts: []systemFact{{"Path", orDash(dbp.Path)}, {"Mode", "WAL"}},
		Talks: []string{"retrieval", "index", "events", "watcher"},
		Href:  "/console/settings?s=engine", HrefLabel: "Open Settings",
	})

	watcher := systemPart{
		ID: "watcher", Label: "File watcher", Ring: ringCore, State: running("watcher"),
		Role:       "Notices every change to the files.",
		What:       "Watches memory and notes on disk and re-indexes a file 300 ms after it changes, including edits you make in an editor.",
		Hero:       systemStat{Value: countWord(nav.Memories + nav.Notes), Label: "files it watches"},
		SparkLabel: "File changes per minute",
		Facts:      []systemFact{{"Debounce", "300 ms"}},
		Talks:      []string{"memories", "notes", "database"},
	}
	if s.cfg.Files == nil {
		watcher.State, watcher.SparkLabel = partOff, ""
		watcher.Hero = systemStat{Value: "Off", Label: "no files layer in this process"}
	}
	add(watcher)

	add(systemPart{
		ID: "notes", Label: "Note files", Ring: ringCore, State: running("notes"),
		Role:       "Long-form notes, one file each.",
		What:       "Research, meeting summaries and plan narratives as markdown files under notes/, one folder per project.",
		Hero:       systemStat{Value: countWord(nav.Notes), Label: "notes on disk"},
		SparkLabel: "Note writes per minute",
		Metrics:    []systemStat{{Label: "Written today", Value: countWord(day[string(core.EventNoteWritten)])}},
		Facts:      []systemFact{{"Folder", filepath.Join(orDash(s.cfg.DataDir), "notes")}},
		Talks:      []string{"watcher", "capture"},
		Href:       "/console/notes", HrefLabel: "Open Notes",
	})

	add(s.embeddingsPart(ctx, running("embeddings")))

	add(systemPart{
		ID: "index", Label: "Search index", Ring: ringCore, State: running("index"),
		Role:       "Keyword search across everything.",
		What:       "One full-text index spans memories, notes, tasks, trials and session findings, so recall can find any of them by keyword.",
		Hero:       systemStat{Value: countWord(nav.Memories + nav.Notes + nav.Sessions + nav.Trials), Label: "memories, notes, sessions and trials it covers"},
		SparkLabel: "Index updates per minute",
		Metrics: []systemStat{
			{Label: "Memories", Value: countWord(nav.Memories)}, {Label: "Notes", Value: countWord(nav.Notes)},
			{Label: "Sessions", Value: countWord(nav.Sessions)}, {Label: "Trials", Value: countWord(nav.Trials)},
		},
		Facts: []systemFact{{"Engine", "SQLite FTS5"}},
		Talks: []string{"database"},
	})

	chat := systemPart{
		ID: "llm", Label: "Chat model", Ring: ringCore, State: partIdle,
		Role:  "Writes digests and answers gardener requests.",
		What:  "A chat model summarizes sessions for the gardener and turns your plain-language gardener requests into proposals. Without one those passes are skipped.",
		Hero:  systemStat{Value: "Ready", Label: "for the gardener's digests and requests"},
		Talks: []string{"provider", "gardener"},
	}
	if dep.ChatModel != "" {
		chat.Facts = append(chat.Facts, systemFact{"Model", dep.ChatModel})
	}
	if s.cfg.Gardener == nil || !s.cfg.Gardener.CanRequest() {
		chat.State, chat.Facts = partOff, nil
		chat.Hero = systemStat{Value: "Off", Label: "no chat model is configured"}
		chat.TurnOn = &systemHow{Text: "Set a provider key in the config file (llm block) and restart the daemon."}
	}
	add(chat)

	add(s.backupsPart())

	add(systemPart{
		ID: "memories", Label: "Memory files", Ring: ringCore, State: running("memories"),
		Role:       "Durable knowledge, one file each.",
		What:       "Constraints, conventions, gotchas and decisions as markdown files with frontmatter. These files are the source of truth; you can open and edit them.",
		Hero:       systemStat{Value: countWord(nav.Memories), Label: "memories in force"},
		SparkLabel: "Memory writes per minute",
		Metrics:    []systemStat{{Label: "Written today", Value: countWord(day[string(core.EventMemoryWritten)])}, {Label: "Read today", Value: countWord(day[string(core.EventMemoryRead)])}},
		Facts:      []systemFact{{"Folder", filepath.Join(orDash(s.cfg.DataDir), "memory")}, {"Format", "markdown + YAML"}},
		Talks:      []string{"watcher", "gardener", "lifecycle", "screening"},
		Href:       "/console/memories", HrefLabel: "Open Memories",
	})

	evDay := activity("events").day
	add(systemPart{
		ID: "events", Label: "Event log", Ring: ringCore, State: running("events"),
		Role:       "Every recorded change, in order.",
		What:       "An append-only log of what happened: writes, reads, sessions, approvals. It drives this screen, the activity feeds and the live stream.",
		Hero:       systemStat{Value: countWord(evDay), Label: "events in the last 24 hours"},
		SparkLabel: "Events per minute",
		Talks:      []string{"database", "console", "feed"},
		Href:       "/console/interactions", HrefLabel: "Open Interactions",
	})

	// ---- outside -----------------------------------------------------------

	emb := s.cfg.Embedding
	provider := systemPart{
		ID: "provider", Label: "LLM provider", Ring: ringSky, State: partOff,
		Role:  "Where vectors and digests come from.",
		What:  "The model provider Seamless calls for embeddings and chat. Memory and note text goes out to be embedded; nothing else leaves this machine through it.",
		Hero:  systemStat{Value: "None", Label: "no provider is in use"},
		Talks: []string{"embeddings", "llm"},
	}
	if emb.Enabled || (s.cfg.Gardener != nil && s.cfg.Gardener.CanRequest()) {
		provider.State = partIdle
		provider.Hero = systemStat{Value: orDash(titleWord(emb.Provider)), Label: "the configured provider"}
		if emb.Model != "" {
			provider.Facts = append(provider.Facts, systemFact{"Embedding model", emb.Model})
		}
	}
	add(provider)

	gh := systemPart{
		ID: "github", Label: "GitHub releases", Ring: ringSky, State: partAbsent,
		Role:  "Release information for updates.",
		What:  "The update check asks GitHub which releases exist. It sends nothing about you or your data.",
		Hero:  systemStat{Value: "Unused", Label: "this daemon does not check for updates"},
		Talks: []string{"updates"},
	}
	if s.cfg.Updates != nil {
		if st := s.cfg.Updates.Status(); st.Settings.Check {
			gh.State = partIdle
			gh.Hero = systemStat{Value: "In use", Label: "asked on each update check"}
		} else {
			gh.State = partOff
			gh.Hero = systemStat{Value: "Unused", Label: "update checks are switched off"}
		}
	}
	add(gh)

	tally := map[string]int{partOn: 0, partIdle: 0, partWarn: 0, partOff: 0}
	for _, p := range parts {
		if p.State == partAbsent {
			tally[partOff]++
		} else {
			tally[p.State]++
		}
	}
	chains := make(map[string]systemChain, len(systemChains))
	for k, c := range systemChains {
		chains[string(k)] = c
	}
	return systemData{
		Version: s.cfg.Version, Host: s.hostName, Parts: parts, Tally: tally, Chains: chains,
		SparkMinutes: systemSparkMinutes, At: now,
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// idleTTL is the live/idle threshold the session displays use.
func (s *Service) idleTTL() time.Duration {
	if s.cfg.SessionIdleTTL > 0 {
		return s.cfg.SessionIdleTTL
	}
	return core.SessionIdleTTL
}

func (s *Service) approvalsPart(ctx context.Context, now time.Time) systemPart {
	p := systemPart{
		ID: "approvals", Label: "Approvals", Ring: ringWork, State: partOff,
		Role:  "Your approvals for locked projects.",
		What:  "When a project is locked, even its own agents ask you first. You approve with Touch ID, a passkey or your terminal, and the approval becomes a lease for that one agent.",
		Hero:  systemStat{Value: "Off", Label: "no project is locked"},
		Talks: []string{"socket", "database"},
		Facts: []systemFact{{"Setting", "peer_auth: off"}},
		TurnOn: &systemHow{Text: "Locks need the daemon in its own account (peer_auth). Then lock a project from its page, or force lock it in the config file.",
			Code: "peer_auth:\n  force_locked: [example-finance]"},
		Href: "/console/privacy", HrefLabel: "Open Privacy",
	}
	if s.cfg.Approvals == nil && s.cfg.Grants == nil {
		return p
	}
	p.State, p.TurnOn, p.Facts = partIdle, nil, nil
	p.SparkLabel = "Approval events per minute"
	pending, err := store.PendingAccessApprovals(ctx, s.writeDB(), now)
	if err != nil {
		s.logger.Warn("console: system approvals", "error", err)
	}
	leases, err := store.LiveAccessLeases(ctx, s.writeDB(), now)
	if err != nil {
		s.logger.Warn("console: system approvals", "error", err)
	}
	p.Hero = systemStat{Value: fmt.Sprint(len(pending)), Label: "requests waiting for you"}
	p.Metrics = []systemStat{{Label: "Live leases", Value: fmt.Sprint(len(leases))}}
	if s.cfg.Approvals != nil {
		if n, err := store.CountApprovers(ctx, s.writeDB()); err == nil {
			p.Metrics = append(p.Metrics, systemStat{Label: "Approvers enrolled", Value: fmt.Sprint(n)})
		}
	}
	if s.cfg.Views != nil {
		p.Facts = append(p.Facts, systemFact{"Locked projects", fmt.Sprint(len(s.cfg.Views.Lockable()))})
	}
	p.Open = len(leases) > 0
	if len(pending) > 0 {
		p.State = partWarn
	}
	p.Href, p.HrefLabel = "/console/access", "Open Access requests"
	return p
}

func (s *Service) screeningPart(ctx context.Context, feats config.Features, state string) systemPart {
	p := systemPart{
		ID: "screening", Label: "Write screening", Ring: ringWork, State: partOff,
		Role:   "Checks agent writes before they land.",
		What:   "Holds a write that looks like an injected instruction or a secret until you release it, and quarantines memory files edited outside Seamless.",
		Hero:   systemStat{Value: "Off", Label: "write screening is an optional feature"},
		Talks:  []string{"memories"},
		TurnOn: &systemHow{Text: "Turn it on in Settings, Features, or in the config file.", Code: "features:\n  screening: true"},
		Href:   "/console/settings?s=features", HrefLabel: "Open Settings",
	}
	if !features.Enabled(feats, features.Screening) {
		return p
	}
	p.State, p.TurnOn = state, nil
	p.SparkLabel = "Screened writes per minute"
	held, err := store.CountPendingHolds(ctx, s.db(ctx))
	if err != nil {
		s.logger.Warn("console: system screening", "error", err)
	}
	p.Count = intPtr(min(held, 4))
	p.Hero = systemStat{Value: fmt.Sprint(held), Label: "writes held for you"}
	if s.cfg.Screening != nil && s.cfg.Screening.Lockdown(ctx).On {
		p.Facts = append(p.Facts, systemFact{"Lockdown", "on"})
		p.State = partWarn
	}
	if held > 0 {
		p.State = partWarn
	}
	p.Href, p.HrefLabel = "/console/held", "Open Held"
	return p
}

func (s *Service) updatesPart(state string) systemPart {
	p := systemPart{
		ID: "updates", Label: "Updates", Ring: ringWork, State: partAbsent,
		Role:  "Checks GitHub for new releases.",
		What:  "Release builds check for new versions and can update themselves when nothing is running. A build from source never checks.",
		Hero:  systemStat{Value: "None", Label: "this daemon runs no update check"},
		Talks: []string{"github", "backups"},
		Facts: []systemFact{{"Version", orDash(s.cfg.Version)}},
		TurnOn: &systemHow{Text: "Install a release with the installer to get update checks.",
			Code: "curl -fsSL https://thereisnospoon.org/install | sh"},
	}
	if s.cfg.Updates == nil {
		return p
	}
	st := s.cfg.Updates.Status()
	if !st.Settings.Check {
		p.State = partOff
		p.Hero = systemStat{Value: "Off", Label: "update checks are switched off"}
		p.TurnOn = &systemHow{Text: "Switch checks on in Settings, Updates."}
		p.Href, p.HrefLabel = "/console/settings?s=updates", "Open Settings"
		return p
	}
	p.State, p.TurnOn = state, nil
	p.SparkLabel = "Update events per minute"
	p.Hero = systemStat{Value: "Current", Label: "no newer release seen"}
	if st.Available && st.Newest != nil {
		p.Hero = systemStat{Value: "v" + st.Newest.Version.String(), Label: "is available"}
		p.State = partWarn
	}
	if !st.CheckedAt.IsZero() {
		p.Facts = append(p.Facts, systemFact{"Last check", agoPhrase(st.CheckedAt)})
	}
	if st.CheckError != nil {
		p.State = partWarn
	}
	p.Href, p.HrefLabel = "/console/settings?s=updates", "Open Settings"
	return p
}

func (s *Service) embeddingsPart(ctx context.Context, state string) systemPart {
	emb := s.cfg.Embedding
	p := systemPart{
		ID: "embeddings", Label: "Embeddings", Ring: ringCore, State: partOff,
		Role:  "Meaning search, one vector per item.",
		What:  "Each memory and note gets a vector so recall can match by meaning. When the provider fails, recall falls back to keywords and the gap is filled later.",
		Hero:  systemStat{Value: "Off", Label: "recall is keyword-only"},
		Talks: []string{"provider", "retrieval", "database"},
		Href:  "/console/settings?s=engine", HrefLabel: "Open Settings",
		TurnOn: &systemHow{Text: "Set a provider key in the config file (llm block) and restart the daemon.",
			Code: "llm:\n  provider: openai\n  openai:\n    api_key: \"<your key>\""},
	}
	if emb.Reason != "" {
		p.Facts = append(p.Facts, systemFact{"Why", emb.Reason})
	}
	if !emb.Enabled {
		if emb.Misconfigured {
			p.State = partWarn
		}
		return p
	}
	p.State, p.TurnOn = state, nil
	p.SparkLabel = "Vectors written per minute"
	p.Facts = []systemFact{{"Provider", titleWord(emb.Provider)}, {"Model", emb.Model}}
	panel, err := s.embeddingsPanel(ctx)
	if err != nil {
		s.logger.Warn("console: system embeddings", "error", err)
		return p
	}
	covered := panel.Total - panel.Missing
	if panel.Total > 0 {
		p.Hero = systemStat{Value: fmt.Sprintf("%.1f", float64(covered)*100/float64(panel.Total)), Unit: "%", Label: "of memories and notes have a vector"}
	} else {
		p.Hero = systemStat{Value: "0", Label: "items to embed"}
	}
	p.Metrics = []systemStat{{Label: "Waiting", Value: fmt.Sprint(panel.Missing), Unit: "items"}, {Label: "Vectors", Value: countWord(panel.Total)}}
	for _, m := range panel.Models {
		if m.Model == emb.Model && m.Dims > 0 {
			p.Metrics = append(p.Metrics, systemStat{Label: "Dimensions", Value: fmt.Sprint(m.Dims)})
			break
		}
	}
	if panel.Stale > 0 {
		p.State = partWarn
		p.Metrics = append(p.Metrics, systemStat{Label: "From an older model", Value: fmt.Sprint(panel.Stale)})
	}
	return p
}

// backupsArchivesPart reads the updater's pre-update archives from the data
// directory, for a process that runs no backup service (backupsPart): the
// updater writes them, the daemon only lists them.
func (s *Service) backupsArchivesPart() systemPart {
	p := systemPart{
		ID: "backups", Label: "Backups", Ring: ringCore, State: partAbsent,
		Role:  "Archives taken before an update.",
		What:  "Before an automatic update installs, the updater archives the database and the files, and keeps the newest two.",
		Hero:  systemStat{Value: "None", Label: "no pre-update archive yet"},
		Talks: []string{"updates"},
		TurnOn: &systemHow{Text: "Export an archive yourself any time.",
			Code: "seamlessd export -o ~/seamless-backup.tar.gz"},
	}
	if s.cfg.DataDir == "" {
		return p
	}
	dir := filepath.Join(s.cfg.DataDir, "backups")
	p.Facts = []systemFact{{"Folder", dir}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return p
	}
	var newest time.Time
	n := 0
	for _, e := range entries {
		if e.Type()&os.ModeSymlink != 0 || !strings.HasPrefix(e.Name(), "pre-update-") || !strings.HasSuffix(e.Name(), ".tar.gz") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		n++
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	if n == 0 {
		return p
	}
	p.State, p.TurnOn = partIdle, nil
	p.Hero = systemStat{Value: fmt.Sprint(n), Label: "pre-update archives kept"}
	p.Facts = append(p.Facts, systemFact{"Newest", agoPhrase(newest)})
	return p
}
