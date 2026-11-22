package console

// Your setup's facts (Settings > Your setup).

import (
	"context"
	"html/template"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/store"
)

// setupPanel is the Your setup section: what a non-technical owner can check
// and act on -- the version, where things live, which agents are connected --
// and none of the budgets or policy numbers (those stay in Knowledge engine).
type setupPanel struct {
	Version       string
	Host          string
	ConfigPath    string
	ConfigEditURL template.URL
	Clients       []setupClient
	// Backups is the Backups row, nil when no backup service runs here.
	Backups *setupBackups
}

// setupClient is one agent client that has recorded sessions here.
type setupClient struct {
	Name     string
	LastSeen time.Time
}

// setupData builds the Your setup facts. The clients line is best-effort: a
// read failure costs that line, never the section.
func (s *Service) setupData(ctx context.Context) setupPanel {
	p := setupPanel{Version: strings.TrimSpace(s.cfg.Version), Host: s.host, ConfigPath: strings.TrimSpace(s.cfg.ConfigPath),
		Backups: s.setupBackupsRow(time.Now())}
	if p.ConfigPath != "" {
		_, p.ConfigEditURL = absAndEditURL("", p.ConfigPath)
	}
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
