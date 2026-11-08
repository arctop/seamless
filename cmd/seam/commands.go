package main

// The command table: ordering only.
//
// Every spec lives next to its handler (var captureCmd = spec(...) above
// runCapture), so one-file-per-command survives and a command's contract sits
// where its code does. This file decides only what order the reader meets them
// in, which is also the order help renders.
//
// This is now the whole command set: hook was the last migration, so there is no
// second place a command can be declared and nothing left for help and the parser
// to disagree about.

// Group headings, in help order. Declared as constants rather than written at
// each spec so a typo cannot silently open a second section.
const (
	groupAgentLoop     = "agent loop"
	groupKnowledge     = "memories and notes"
	groupTasks         = "tasks"
	groupPlans         = "captured plans (Claude Code plan mode)"
	groupAccess        = "locked projects (peer_auth)"
	groupRestricted    = "restricted accounts (peer_auth)"
	groupObservability = "observability"
	groupCall          = "any MCP tool"
	groupHooks         = "hooks (invoked by your agent client, not by hand)"
	groupBridge        = "mcp bridge (invoked by an MCP client, not by hand)"
)

// groupOrder is the order help renders the sections in. A group naming no
// command renders nothing; help_test pins the converse, that every group a
// command names is listed here, since a spec in an unlisted group renders
// nowhere at all.
var groupOrder = []string{groupAgentLoop, groupKnowledge, groupTasks, groupPlans, groupAccess, groupRestricted, groupObservability, groupCall, groupHooks, groupBridge}

// commands returns the migrated command table, in help order.
//
// A function rather than a package-level var: each spec closes over a bind that
// registers flags on a fresh FlagSet per parse, and a var would invite callers to
// share one table across parses when the point is that binding is per-call.
func commands() []cmd {
	return []cmd{
		primeCmd,
		rememberCmd,
		recallCmd,
		captureCmd,

		memoryReadCmd,
		memoryListCmd,
		noteReadCmd,
		noteListCmd,

		readyCmd,
		taskListCmd,
		taskAddCmd,
		taskDoneCmd,
		taskStartCmd,
		taskDropCmd,
		taskReopenCmd,
		taskClaimCmd,
		taskHeartbeatCmd,
		taskReleaseCmd,

		planListCmd,
		planShowCmd,
		planCheckCmd,
		planApproveCmd,

		accessRequestCmd,
		approveCmd,
		touchIDEnrollCmd,
		touchIDStatusCmd,

		proposalsCmd,
		promoteCmd,
		proposalsDiscardCmd,

		statusCmd,
		sessionsCmd,
		favCmd,
		projectIsolationCmd,
		usageCmd,
		doctorCmd,
		versionCmd,

		callCmd,

		hookCmd,

		mcpProxyCmd,
		mcpHeadersCmd,
	}
}
