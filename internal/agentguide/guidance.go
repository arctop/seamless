// Package agentguide owns the concise, cross-surface guidance that teaches an
// agent how Seamless's tools compose. Keeping these strings below the MCP and
// retrieval surfaces prevents their shared workflow from drifting into two
// independently maintained summaries.
package agentguide

// MCPInstructions is returned in the MCP initialize result. Codex consumes this
// as server-wide guidance, including while it is deciding whether to expose or
// call an individual tool. Keep the first paragraph self-contained and under
// 512 characters; current Codex clients prioritize that prefix.
const MCPInstructions = "Use Seamless as durable context. Recall before guessing: call recall before asking users to re-find knowledge. memory_write stores compact durable knowledge; notes_create stores long artifacts. Do not duplicate code, AGENTS.md/CLAUDE.md, or this conversation. If scope is ambiguous, pass project explicitly; project=global is only for cross-project knowledge. Use sessions/findings for handoff. Compose plans as notes + tasks with plan:<slug>. Trust each tool's inputSchema required fields and enums over prose.\n\n" +
	"For non-trivial work, read injected briefing/recall context before searching again. Claude Code and Codex hooks start your session and bind your tool calls to it, so you rarely need session_start: call it only from a client without hooks (with cwd or project) or to move this connection to another project. Close with session_end and concise findings when the work needs a handoff. If this session caused any mishaps (an action a warning said not to take, live state touched by mistake), self-report them in session_end's mishaps field -- recurrence review is how they get fixed. " + MishapPrefix + ". Claim shared plan steps with tasks_claim before working them. When writing a memory, " + KindDiscriminator + "; " + StageContract + ". Task vs stage: " + TaskVsStage + ". " + EditVsSupersede

// MishapPrefix asks a session_end mishap entry to name its kind, so a
// recurrence review can count the reports by kind. session_end's mishaps
// argument and MCPInstructions both render it.
const MishapPrefix = "Begin each mishap entry with its type and a colon (" + MishapTypeList + ")"

// MishapTypeList is the mishap types as the guidance names them.
// core.MishapTypes is the vocabulary; this package imports nothing, so
// guidance_test holds this list to it, word for word and in order, and the
// onboarding skill names it too (RequiredWorkflowTerms).
const MishapTypeList = "shell-trap, wrong-target, live-state, ignored-warning, fabricated or other"

// KindDiscriminator is the constraint-vs-convention call an agent makes at
// memory_write time. The memory_write kind parameter and MCPInstructions both
// render this string so the write-side judgment cannot drift between surfaces.
const KindDiscriminator = "constraint = what any agent must or must not do regardless of task; convention = a project-local choice or layout fact (naming, branding, where things live or deploy, which files sync together)"

// StageContract is the header rule an agent applies when writing a kind=stage
// memory. The memory_write kind parameter and MCPInstructions both render this
// string so the write-side contract cannot drift between surfaces; the read
// side (core.ParseStageHeader) is what makes the header load-bearing.
const StageContract = "for kind=stage, open the body with Status: open|in_progress|blocked|done and optionally Gate: human|ai, and flip the status with memory_edit (or a full memory_write) -- append cannot change the header, which is parsed from the top of the body"

// TaskVsStage is the rule of thumb that keeps external waits out of the task
// queue and claimable work out of stage memories.
const TaskVsStage = "task = work someone here can claim and finish; stage = a state of the world you wait on that every session must know while it holds"

// EditVsSupersede is the lifecycle boundary the partial-edit tools have to
// police, rendered by MCPInstructions and by both edit tools' descriptions so
// the rule cannot drift into two versions of itself.
//
// It needs saying because a cheap in-place edit is exactly the tool an agent
// reaches for to change what a memory CLAIMS, and that quietly rewrites the
// corpus's history: a superseded memory leaves the indexes but stays readable
// pointing at its replacement, which is how a later reader learns the thing was
// reconsidered rather than that it was never believed. An edit leaves no such
// trace, so it must stay confined to changes that carry no new claim.
const EditVsSupersede = "Edit vs supersede: memory_edit and notes_edit are for corrections that do not change what an item CLAIMS -- typos, formatting, a stale path or command, a stage's Status flip, metadata. When the MEANING changes, write the new knowledge with memory_write supersedes so the outdated memory retires into readable history instead of being silently rewritten."

// BriefingFooter is the compact reminder appended to every ambient briefing.
// It deliberately names only the two retrieval entry points; MCPInstructions
// carries the fuller workflow once a client initializes the tool server.
const BriefingFooter = "Recall on demand with recall; read a memory with memory_read. Trust a tool's inputSchema (required + enums) over prose when building a call.\n"

// RequiredWorkflowTerms is the canonical cross-surface contract used by tests
// for MCP instructions and the maintained onboarding skill. Return a fresh
// slice so callers cannot mutate shared process state.
func RequiredWorkflowTerms() []string {
	return []string{
		"recall",
		"memory_write",
		"notes_create",
		"AGENTS.md",
		"CLAUDE.md",
		"project=global",
		"sessions",
		"findings",
		"plan:<slug>",
		"tasks_claim",
		"inputSchema",
		"mishaps",
		MishapTypeList,
		"constraint",
		"convention",
		"kind=stage",
		"Status: open|in_progress|blocked|done",
		"Gate: human|ai",
		"claim and finish",
		"state of the world",
	}
}

// RequiredToolNames is the subset of the guidance contract that must resolve
// to real MCP tools. Catalog tests keep prose from naming a stale workflow.
func RequiredToolNames() []string {
	return []string{"recall", "memory_write", "notes_create", "session_start", "session_end", "tasks_claim"}
}
