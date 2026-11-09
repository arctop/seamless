package console

// The Privacy screen: what the owner's agents can reach on this machine, what
// one could do if it were compromised, and where each project's knowledge may
// go. Every project appears in exactly one of four fences -- open,
// confidential, sealed, locked -- and all four always show, empty or not, each
// with what it is for and how to put a project there.

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/features"
	"github.com/arctop/seamless/internal/store"
)

// Fence keys, loosest first. "locked" is the peer_auth lock, a separate
// setting from isolation: a locked project is normally confidential too.
const (
	fenceOpen   = "open"
	fenceConf   = "confidential"
	fenceSealed = "sealed"
	fenceLocked = "locked"
)

type privacyProject struct {
	Slug     string `json:"slug"`
	Name     string `json:"name"`
	Memories int    `json:"memories"`
	Notes    int    `json:"notes"`
	Href     string `json:"href"`
}

type privacyFence struct {
	Key      string           `json:"key"`
	Name     string           `json:"name"`
	Promise  string           `json:"promise"`
	Fits     string           `json:"fits"`
	How      string           `json:"how"`
	Code     string           `json:"code"`
	Needs    string           `json:"needs,omitempty"`
	Empty    string           `json:"empty"`
	Note     string           `json:"note,omitempty"`
	Projects []privacyProject `json:"projects"`
}

// Count is how many projects the fence holds.
func (f privacyFence) Count() int { return len(f.Projects) }

type privacyLayer struct {
	N     int    `json:"n"`
	Name  string `json:"name"`
	State string `json:"state"`
	What  string `json:"what"`
	On    bool   `json:"on"`
	Later bool   `json:"later"`
}

type privacyStep struct {
	State     string `json:"state"` // done | todo | warn | later
	Title     string `json:"title"`
	Sub       string `json:"sub"`
	Text      string `json:"text"`
	Code      string `json:"code,omitempty"`
	Link      string `json:"link,omitempty"`
	LinkLabel string `json:"linkLabel,omitempty"`
}

// privacyPlan is what the machine drawing needs (static/privacy.js).
type privacyPlan struct {
	Mode       string                      `json:"mode"`
	Owner      string                      `json:"owner"`
	DaemonUser string                      `json:"daemonUser"`
	DataDir    string                      `json:"dataDir"`
	Addr       string                      `json:"addr"`
	Restricted []string                    `json:"restricted"`
	Counts     map[string]int              `json:"counts"`
	Global     int                         `json:"global"`
	Projects   map[string][]privacyProject `json:"projects"`
	Locking    bool                        `json:"locking"`
}

type privacyData struct {
	Mode        string         `json:"mode"` // shared | dedicated
	Owner       string         `json:"owner"`
	DaemonUser  string         `json:"daemonUser"`
	DataDir     string         `json:"dataDir"`
	DataDirMode string         `json:"dataDirMode"`
	ConfigPath  string         `json:"configPath"`
	Addr        string         `json:"addr"`
	Loopback    bool           `json:"loopback"`
	TLS         bool           `json:"tls"`
	Screening   bool           `json:"screening"`
	Lockdown    bool           `json:"lockdown"`
	Requests    bool           `json:"requests"`
	Restricted  []string       `json:"restricted"`
	Global      int            `json:"global"`
	Fenced      int            `json:"fenced"`
	Layers      []privacyLayer `json:"layers"`
	Fences      []privacyFence `json:"fences"`
	Steps       []privacyStep  `json:"steps"`
	Plan        privacyPlan    `json:"plan"`
}

// Dedicated reports the daemon running as its own account (peer_auth).
func (d privacyData) Dedicated() bool { return d.Mode == "dedicated" }

// Total counts every project the page places.
func (d privacyData) Total() int {
	n := 0
	for _, f := range d.Fences {
		n += len(f.Projects)
	}
	return n
}

// Fence returns one fence by key.
func (d privacyData) Fence(key string) privacyFence {
	for _, f := range d.Fences {
		if f.Key == key {
			return f
		}
	}
	return privacyFence{}
}

const peerAuthGuide = "https://thereisnospoon.org/docs/guides/peer-auth/"

func (s *Service) privacyPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "privacy", pageData{Title: "Privacy", Active: "privacy", Data: s.privacyData(r.Context())})
}

func (s *Service) privacyData(ctx context.Context) privacyData {
	dep := s.cfg.Deployment
	feats := s.effectiveFeatures(ctx)
	d := privacyData{
		Mode: "shared", Owner: dep.Owner, DaemonUser: dep.DaemonUser, DataDir: s.cfg.DataDir,
		ConfigPath: s.cfg.ConfigPath, Addr: dep.Addr, Loopback: loopbackAddr(dep.Addr), TLS: s.cfg.SecureCookies,
		Screening: features.Enabled(feats, features.Screening), Requests: s.cfg.Grants != nil,
		Restricted: slices.Clone(dep.Restricted),
	}
	if dep.PeerAuth {
		d.Mode = "dedicated"
	}
	if d.Owner == "" {
		d.Owner = "you"
	}
	if d.DaemonUser == "" {
		d.DaemonUser = d.Owner
	}
	if s.cfg.Screening != nil && d.Screening {
		d.Lockdown = s.cfg.Screening.Lockdown(ctx).On
	}
	if s.cfg.DataDir != "" {
		if info, err := os.Lstat(s.cfg.DataDir); err == nil {
			d.DataDirMode = fmt.Sprintf("%04o", info.Mode().Perm())
		}
	}
	d.Fences = s.privacyFences(ctx, dep.PeerAuth)
	for _, f := range d.Fences {
		if f.Key != fenceOpen {
			d.Fenced += len(f.Projects)
		}
	}
	if g, err := store.ProjectTallies(ctx, s.db(ctx), []string{""}); err == nil && len(g) == 1 {
		d.Global = g[0].Memories
	} else if err != nil {
		s.logger.Warn("console: privacy global tally", "error", err)
	}
	d.Layers = privacyLayers(d)
	d.Steps = privacySteps(d)
	counts := map[string]int{}
	projects := map[string][]privacyProject{}
	for _, f := range d.Fences {
		counts[f.Key] = len(f.Projects)
		projects[f.Key] = f.Projects
	}
	d.Plan = privacyPlan{
		Mode: d.Mode, Owner: d.Owner, DaemonUser: d.DaemonUser, DataDir: d.DataDir, Addr: d.Addr,
		Restricted: d.Restricted, Counts: counts, Global: d.Global, Projects: projects, Locking: dep.PeerAuth,
	}
	if d.Plan.Restricted == nil {
		d.Plan.Restricted = []string{}
	}
	return d
}

// privacyFences places every project this request may see in one fence. A
// locked project shows by name and counts only, read the way the projects
// board reads it.
func (s *Service) privacyFences(ctx context.Context, peerAuth bool) []privacyFence {
	fences := []privacyFence{
		{Key: fenceOpen, Name: "Open", Promise: promiseBody(core.IsolationOpen),
			Fits: "Your own apps, open source and side projects: work where shared conventions help.",
			How:  "New projects start open. Loosening a fenced project is immediate:", Code: "seam project isolation <slug> open",
			Empty: "No open projects."},
		{Key: fenceConf, Name: "Confidential", Promise: promiseBody(core.IsolationConfidential),
			Fits: "Client work, an employer's code, private research. Its agents still get your conventions; no other project's agents see its knowledge.",
			How:  "Use Isolation on the project's Overview tab, or:", Code: "seam project isolation <slug> confidential --yes",
			Empty: "No confidential projects yet."},
		{Key: fenceSealed, Name: "Sealed", Promise: promiseBody(core.IsolationSealed),
			Fits: "A clean room: an evaluation that must not see your other work, or a matter that must stay apart in both directions.",
			How:  "The same control. Tightening detaches the project from its family and parent first:", Code: "seam project isolation <slug> sealed --yes",
			Empty: "No sealed projects yet."},
		{Key: fenceLocked, Name: "Locked", Promise: "Even its own agents ask you first.",
			Fits: "Money, health, credentials: projects where you want to say yes before an agent reads them, every time.",
			How:  "Make the project confidential, list it in the daemon's config, and restart the daemon:", Code: "peer_auth:\n  locked: [example-finance]",
			Empty: "No locked projects yet."},
	}
	if !peerAuth {
		fences[3].Needs = "Needs Seamless in its own account"
		fences[3].Empty = "Locking needs Seamless in its own account first."
	} else {
		fences[3].Note = "A locked project shows by name and counts only. Its content opens with your approval."
	}
	idx := map[string]int{fenceOpen: 0, fenceConf: 1, fenceSealed: 2, fenceLocked: 3}
	var locked []string
	if s.cfg.Views != nil {
		locked = s.cfg.Views.Lockable()
	}
	projects, err := store.ListProjects(ctx, s.db(ctx))
	if err != nil {
		s.logger.Warn("console: privacy projects", "error", err)
	}
	var slugs []string
	state := map[string]core.Isolation{}
	for _, p := range projects {
		if p.Slug == "" || p.Retired() || slices.Contains(locked, p.Slug) {
			continue
		}
		slugs = append(slugs, p.Slug)
		state[p.Slug] = isolationOf(p)
	}
	tallies, err := store.ProjectTallies(ctx, s.db(ctx), slugs)
	if err != nil {
		s.logger.Warn("console: privacy tallies", "error", err)
	}
	for _, t := range tallies {
		key := fenceOpen
		switch state[t.Slug] {
		case core.IsolationConfidential:
			key = fenceConf
		case core.IsolationSealed:
			key = fenceSealed
		}
		fences[idx[key]].Projects = append(fences[idx[key]].Projects, privacyProject{
			Slug: t.Slug, Name: t.Name, Memories: t.Memories, Notes: t.Notes, Href: "/console/projects/" + t.Slug,
		})
	}
	if len(locked) > 0 {
		lt, err := store.ProjectTallies(ctx, s.writeDB(), locked)
		if err != nil {
			s.logger.Warn("console: privacy locked tallies", "error", err)
		}
		for _, t := range lt {
			fences[3].Projects = append(fences[3].Projects, privacyProject{
				Slug: t.Slug, Name: t.Name, Memories: t.Memories, Notes: t.Notes, Href: "/console/unlock/" + t.Slug,
			})
		}
	}
	for i := range fences {
		if fences[i].Projects == nil {
			fences[i].Projects = []privacyProject{}
		}
	}
	return fences
}

func privacyLayers(d privacyData) []privacyLayer {
	locked := len(d.Fence(fenceLocked).Projects)
	layers := []privacyLayer{
		{N: 1, Name: "Project fences", What: "Isolation stops knowledge crossing between projects through Seamless."},
		{N: 2, Name: "Write screening", What: "Holds suspicious writes and quarantines files changed outside Seamless."},
		{N: 3, Name: "Seamless in its own account", What: "The operating system keeps your agents out of the data folder."},
		{N: 4, Name: "Locked projects", What: "Even a project's own agents wait for your Touch ID, passkey or password."},
		{N: 5, Name: "Restricted agent account", What: "Unattended agents work as a second account that reaches only open projects."},
	}
	layers[0].On = d.Fenced > 0
	layers[0].State = plural(d.Fenced, "project", "projects")
	if d.Fenced == 0 {
		layers[0].State = "None yet"
	}
	layers[1].On, layers[1].State = d.Screening, onOff(d.Screening)
	if d.Lockdown {
		layers[1].State = "lockdown"
	}
	layers[2].On, layers[2].State = d.Dedicated(), onOff(d.Dedicated())
	switch {
	case locked > 0:
		layers[3].On, layers[3].State = true, plural(locked, "project", "projects")
	case !d.Dedicated():
		layers[3].Later, layers[3].State = true, "Needs 3"
	default:
		layers[3].State = "None yet"
	}
	switch {
	case len(d.Restricted) > 0:
		layers[4].On, layers[4].State = true, strings.Join(d.Restricted, ", ")
	case !d.Dedicated():
		layers[4].Later, layers[4].State = true, "Needs 3"
	default:
		layers[4].State = "None yet"
	}
	return layers
}

func privacySteps(d privacyData) []privacyStep {
	var out []privacyStep
	if d.Loopback {
		out = append(out, privacyStep{State: "done", Title: "Seamless listens only on this machine", Sub: d.Addr,
			Text: "Agents and browsers on other machines can't reach it. If you open it to your network later, the bearer key is what guards it."})
	} else {
		out = append(out, privacyStep{State: "warn", Title: "Seamless answers on your network", Sub: orDash(d.Addr),
			Text: "Anything on the network that holds the bearer key acts as you. Keep TLS on (it is " + onOff(d.TLS) + " here) and allow only the hosts you use.",
			Link: "https://thereisnospoon.org/docs/guides/network-install/", LinkLabel: "Network install guide"})
	}
	if d.Fenced > 0 {
		out = append(out, privacyStep{State: "done", Title: plural(d.Fenced, "project", "projects") + " fenced", Sub: "confidential, sealed or locked",
			Text: "Fence any other project another agent shouldn't read: its Overview tab has the control.", Code: "seam project isolation <slug> confidential --yes"})
	} else {
		out = append(out, privacyStep{State: "todo", Title: "Fence your client work", Sub: "no project is fenced yet",
			Text: "Make projects other agents shouldn't read confidential: the Isolation control on a project's Overview tab, or:", Code: "seam project isolation <slug> confidential --yes"})
	}
	if d.Screening {
		out = append(out, privacyStep{State: "done", Title: "Write screening is on", Sub: "held writes wait on the Held screen",
			Text: "Suspicious writes wait for you, and files changed outside Seamless are quarantined until you accept them."})
	} else {
		out = append(out, privacyStep{State: "todo", Title: "Turn on write screening", Sub: "holds suspicious writes, quarantines files edited outside Seamless",
			Text: "Settings, Features, Write screening, or in the config file:", Code: "features:\n  screening: true"})
	}
	if d.Dedicated() {
		out = append(out, privacyStep{State: "done", Title: "Seamless runs as its own account", Sub: d.DaemonUser,
			Text: "The data folder belongs to " + d.DaemonUser + ", and the operating system keeps every other account out of it."})
	} else {
		out = append(out, privacyStep{State: "todo", Title: "Run Seamless as its own account", Sub: "the operating system then keeps agents out of the data folder",
			Text: "Create a service account, move the data folder to it (mode 0700), run the daemon as that account and turn on peer_auth. The guide walks through every command, with a rollback.",
			Link: peerAuthGuide, LinkLabel: "Lock projects behind your approval"})
	}
	locked := len(d.Fence(fenceLocked).Projects)
	switch {
	case locked > 0:
		out = append(out, privacyStep{State: "done", Title: plural(locked, "project", "projects") + " locked", Sub: "opened only with your approval",
			Text: "Approve requests on the Access requests screen, or in your terminal with seam approve."})
	case d.Dedicated():
		out = append(out, privacyStep{State: "todo", Title: "Lock your most sensitive project", Sub: "its agents then wait for your approval",
			Text: "Make the project confidential, list it under peer_auth.locked in the daemon's config, restart, and enroll an approver: a passkey, or Touch ID with seam touchid enroll.",
			Code: "peer_auth:\n  locked: [example-finance]"})
	default:
		out = append(out, privacyStep{State: "later", Title: "Lock your most sensitive project", Sub: "after Seamless has its own account",
			Text: "A lock needs the daemon in its own account first."})
	}
	switch {
	case len(d.Restricted) > 0:
		out = append(out, privacyStep{State: "done", Title: "Unattended agents have a restricted account", Sub: strings.Join(d.Restricted, ", "),
			Text: "Their constraints and conventions arrive as proposals on the Held screen for you to promote."})
	case d.Dedicated():
		out = append(out, privacyStep{State: "todo", Title: "Give unattended agents a restricted account", Sub: "they reach only open, unlocked projects",
			Text: "List the account under peer_auth.restricted and install its hooks as that account.", Code: "peer_auth:\n  restricted: [_example_agent]",
			Link: peerAuthGuide + "#restricted-local-accounts", LinkLabel: "Restricted local accounts"})
	default:
		out = append(out, privacyStep{State: "later", Title: "Give unattended agents a restricted account", Sub: "after Seamless has its own account",
			Text: "A restricted account connects through the peer_auth socket, so it needs the daemon in its own account first."})
	}
	return out
}

// promiseBody is a state's canonical promise without its leading name
// ("Open — shares normally. ..." reads "Shares normally. ..."), for a card
// that already names the state.
func promiseBody(state core.Isolation) string {
	body := strings.TrimPrefix(isolationPromise(state), isolationLabels[state]+" — ")
	if body == "" {
		return body
	}
	return strings.ToUpper(body[:1]) + body[1:]
}

// loopbackAddr reports whether a listen address answers only on this
// machine. An empty or wildcard host listens everywhere.
func loopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}
