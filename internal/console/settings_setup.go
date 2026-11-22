package console

// Your setup's facts (Settings > Your setup): one card of rows, each linking
// to the section where its value changes.

import (
	"context"
	"html/template"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/arctop/seamless/internal/store"
)

// setupPanel is the Your setup section: what a non-technical owner can check
// and act on -- the version, where things live, which agents are connected --
// and none of the budgets or policy numbers (those stay in Knowledge engine).
type setupPanel struct {
	Version string
	Host    string
	// Account is the OS account the daemon runs as when it has one of its
	// own (peer_auth), "" when it runs as the owner.
	Account       string
	ConfigPath    string
	ConfigEditURL template.URL
	// Config and Data are the config file and the data folder as their rows
	// show them: the whole path for the title and Copy, and a short one.
	Config  pathView
	Data    pathView
	Clients []setupClient
	// Backups is the Backups row, nil when no backup service runs here.
	Backups *setupBackups
	// UpdatesRunning reports an update check in this process: the Version
	// row then carries the Updates section's state chip.
	UpdatesRunning bool
}

// setupClient is one agent client that has recorded sessions here.
type setupClient struct {
	Name     string
	LastSeen time.Time
}

// pathView is a file or folder path as a row shows it: Short in the row, Full
// in its title and its Copy button.
type pathView struct {
	Full, Short string
}

// shortPathMax is the longest path a row prints whole.
const shortPathMax = 40

// viewPath is p as a row shows it, against this daemon's home folder.
func viewPath(p string) pathView {
	if p == "" {
		return pathView{}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	return pathView{Full: p, Short: shortPath(p, home)}
}

// shortPath shortens p for a row: the home folder reads "~", and a path still
// longer than shortPathMax keeps its first folder and its last two names
// around an ellipsis ("~/…/com~apple~CloudDocs/Seamless backups"), or its last
// name alone when that is still too long.
func shortPath(p, home string) string {
	sep := string(filepath.Separator)
	p = filepath.Clean(p)
	disp := p
	if home != "" {
		if home = filepath.Clean(home); home != sep && home != "." {
			switch {
			case p == home:
				return "~"
			case strings.HasPrefix(p, home+sep):
				disp = "~" + sep + p[len(home)+len(sep):]
			}
		}
	}
	if utf8.RuneCountInString(disp) <= shortPathMax {
		return disp
	}
	parts := strings.Split(disp, sep)
	head, rest := parts[0], parts[1:]
	if head == "" && len(rest) > 0 {
		// An absolute path keeps its first folder ("/Volumes").
		head, rest = sep+rest[0], rest[1:]
	}
	if len(rest) <= 2 {
		return disp
	}
	short := head + sep + "…" + sep + strings.Join(rest[len(rest)-2:], sep)
	if utf8.RuneCountInString(short) > shortPathMax {
		short = head + sep + "…" + sep + rest[len(rest)-1]
	}
	return short
}

// setupData builds the Your setup facts. The clients line is best-effort: a
// read failure costs that line, never the section.
func (s *Service) setupData(ctx context.Context) setupPanel {
	p := setupPanel{Version: strings.TrimSpace(s.cfg.Version), Host: s.host, ConfigPath: strings.TrimSpace(s.cfg.ConfigPath),
		Backups: s.setupBackupsRow(time.Now()), UpdatesRunning: s.cfg.Updates != nil}
	if d := s.cfg.Deployment; d.PeerAuth && d.DaemonUser != "" && d.DaemonUser != d.Owner {
		p.Account = d.DaemonUser
	}
	if p.ConfigPath != "" {
		_, p.ConfigEditURL = absAndEditURL("", p.ConfigPath)
		p.Config = viewPath(p.ConfigPath)
	}
	p.Data = viewPath(strings.TrimSpace(s.cfg.DataDir))
	facts, err := store.GetHealthFacts(ctx, s.db(ctx))
	if err != nil {
		s.logger.Warn("console: setup clients", "error", err)
	}
	for _, c := range facts.Clients {
		_, _, name := agentDisplay(c.Client)
		p.Clients = append(p.Clients, setupClient{Name: name, LastSeen: c.LastSeen})
	}
	return p
}

// embedOffFix is how semantic recall comes on where no provider is set up:
// the hint on Your setup's Semantic recall row and on Knowledge engine's.
const embedOffFix = "Search and recall use keywords only. Add an OpenAI key (or point Ollama at a local model) in the config file, then restart."

// setupRecall is Your setup's Semantic recall row: its chip, the model in
// use, and while it is off the fix, one hint away.
type setupRecall struct {
	Word, Tone string
	Model      string
	Fix        string
}

// SetupRecall builds the Semantic recall row from the engine's own panel.
func (d settingsData) SetupRecall() setupRecall {
	e := d.Embeddings
	if e.Enabled {
		return setupRecall{Word: "semantic", Tone: "ok", Model: e.Model}
	}
	r := setupRecall{Word: "keywords only", Tone: "warn", Fix: embedOffFix}
	switch {
	case e.OverriddenOff:
		r.Fix = "Switched off in this console. Search and recall use keywords only."
	case e.Misconfigured:
		r.Fix = "The model settings in the config file have a mistake: " + e.Reason
	}
	return r
}

// setupGardener is Your setup's Gardener row: who applies its proposals and
// how many kinds apply by themselves, as chips, with its cadence in the hint.
type setupGardener struct {
	Chips []sectionState
	Hint  string
}

// SetupGardener builds the Gardener row from the gardener's settings and its
// two switches.
func (d settingsData) SetupGardener() setupGardener {
	g, sw := d.Gardener, d.GardenerSwitches
	var out setupGardener
	every := strconv.Itoa(g.IntervalMinutes) + " minutes"
	if g.Enabled {
		out.Hint = "The gardener suggests cleanups every " + every + "."
	} else {
		out.Chips = append(out.Chips, sectionState{Word: "suggestions off", Tone: "quiet"})
		out.Hint = "No background suggestions are made; upkeep still runs every " + every +
			" (idle sessions reaped, old tool events pruned, retrieval stats refreshed)."
	}
	if sw.AgentsMayApply {
		out.Chips = append(out.Chips, sectionState{Word: "agents apply", Tone: "quiet"})
	} else {
		out.Chips = append(out.Chips, sectionState{Word: "only you apply", Tone: "quiet"})
	}
	if n := len(sw.AutoApply); n > 0 {
		out.Chips = append(out.Chips, sectionState{Word: strconv.Itoa(n) + " by themselves", Tone: "quiet"})
		kinds := make([]string, n)
		for i, k := range sw.AutoApply {
			kinds[i] = strings.ReplaceAll(k, "_", " ")
		}
		out.Hint += " Applied by themselves: " + strings.Join(kinds, ", ") + "."
	}
	return out
}
