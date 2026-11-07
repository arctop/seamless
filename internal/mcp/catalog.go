package mcp

import "github.com/mark3labs/mcp-go/mcp"

// Catalog returns every registered tool's definition, in registration order.
//
// It exists so documentation tooling (cmd/docsgen) can render the tool surface
// without constructing a Server: the constructors are plain data -- name,
// description, and input schema -- and need no DB, config, or listening port.
// The definitions here are the same values registerTools hands to addTool, so
// the docs describe exactly what the server serves.
//
// The order below MUST mirror registerTools. catalog_test enforces both halves
// of that contract (length against LockedToolCount, names against a live
// server's registration order), so a tool added to one and not the other fails
// the build rather than silently vanishing from the docs.
//
// It lists every tool any server can register, the ones only a server that
// locks a project registers (LockedOnlyTools) included, last: a default
// server registers Catalog minus those, in this order, and ToolCount counts
// exactly that. The docs say when each of them is present.
func Catalog() []mcp.Tool {
	tools := []mcp.Tool{
		sessionStartTool(),
		sessionUpdateTool(),
		sessionEndTool(),

		memoryWriteTool(),
		memoryAppendTool(),
		memoryEditTool(),
		memoryReadTool(),
		memoryDeleteTool(),

		recallTool(),

		notesCreateTool(),
		notesReadTool(),
		notesUpdateTool(),
		notesEditTool(),
		notesAppendTool(),
		notesDeleteTool(),

		projectListTool(),
		projectCreateTool(),

		tasksAddTool(),
		tasksUpdateTool(),
		tasksReadyTool(),
		tasksListTool(),
		tasksClaimTool(),
		tasksReleaseTool(),

		labOpenTool(),
		trialRecordTool(),
		trialQueryTool(),

		gardenerProposalsTool(),
		gardenerRequestTool(),
		gardenerSplitTool(),
		gardenerApplyTool(),

		captureURLTool(),
		usageSummaryTool(),

		favoriteSetTool(),

		// LockedOnlyTools, registered only while peer_auth locks a project.
		accessRequestTool(),
	}
	// addTool declares the session= handle on every tool it registers; the docs
	// must show the same schema.
	for i, t := range tools {
		tools[i] = withSessionHandle(t)
	}
	return tools
}

// LockedOnlyTools names the tools a server registers only while peer_auth locks
// a project (Config.OfferAccessRequest), in Catalog order. Catalog documents
// them; ToolCount does not count them.
func LockedOnlyTools() []string { return []string{AccessRequestTool} }

// ToolCountWith is the number of tools a server registers whose
// Config.OfferAccessRequest is offerAccessRequest: what doctor asserts.
func ToolCountWith(offerAccessRequest bool) int {
	if offerAccessRequest {
		return LockedToolCount
	}
	return ToolCount
}
