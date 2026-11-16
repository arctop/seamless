package main

import (
	"database/sql"

	"github.com/arctop/seamless/internal/config"
)

// Doctor run with a peer_auth daemon's own config: the guide runs it as the
// daemon's account (sudo -u), against /opt/seamless/etc/seamless.yaml, which
// only root and that account can read.
//
// The agent client rows (the Claude Code and Codex runtimes, their hooks and
// MCP registrations, the Claude app's bridge, the hooks' seam, the feature
// skills) describe the account doctor runs as, against the hooks install-hooks
// would write from the loaded config. With the daemon's config neither half is
// the owner's: the rows look in the daemon account's home, where no agent
// runs, and judge hooks built from a server config no client of the socket
// uses. Under peer_auth the owner's agents run from the owner's own client
// config (role: client, the socket's server_url), and `seamlessd doctor` run
// as the owner, with that config, judges their hooks and MCP registrations.

// daemonConfigClientsRow is the row that stands for the client rows when the
// loaded config is a peer_auth daemon's. It names no path: the owner's client
// config is not this one, and the reader of this report may not read it.
var daemonConfigClientsRow = check{statusInfo, "client setup",
	"not checked with this config: under peer_auth it is the daemon's, and the agent clients' runtimes, hooks, " +
		"MCP registrations and skills are set up from the owner's own client config; seamlessd doctor run as the " +
		"owner, with that config, checks the hooks and MCP registrations"}

// agentClientChecks is the server report's rows about the agent clients on
// this machine, each judged against what install-hooks would write from cfg;
// under peer_auth, the one row above instead.
func agentClientChecks(cfg config.Config, d doctorDB) []check {
	if cfg.PeerAuth.Enabled {
		return []check{daemonConfigClientsRow}
	}
	checks := claudeRuntimeChecks()
	checks = append(checks, hooksCheck(cfg))
	checks = append(checks, clientSeamChecks(cfg)...)
	checks = append(checks, claudeDesktopChecks(resolveSeamBin(""), absConfigPath(cfg.SourcePath()))...)
	// The events table codexChecks reads dates from migration 001, so an older
	// schema answers it; with no database it drops its activity line.
	checks = append(checks, codexChecks(cfg, d.handle)...)
	return append(checks, d.needsCurrent("feature skills", func(db *sql.DB) check { return featureSkillsCheck(db, cfg) }))
}
