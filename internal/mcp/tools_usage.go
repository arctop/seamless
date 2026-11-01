package mcp

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/arctop/seamless/internal/store"
)

// The description carries the totals decision on purpose. usage_summary answers
// with two different kinds of number -- fenced named lists and unfenced counts --
// and an agent that cannot tell them apart will read a machine-wide total as the
// total of what it was shown. Guidance and behavior are one fix
// (write-scope-registers-the-project-it-names): if the sentence below and the
// filter beneath it ever disagree, the sentence is the lie.
func usageSummaryTool() mcp.Tool {
	return mcp.NewTool("usage_summary", hintRead(),
		mcp.WithDescription("Report a roll-up of activity: memory/note/session/task counts, retrieval "+
			"totals with the most-injected memories, pending gardener proposals, and events by kind. "+
			"Every count is machine-wide and identical for every caller; only the named memory lists "+
			"(topInjected, topUtility) are fenced, so memories this session may not read under project "+
			"isolation are dropped from them and those lists can come back shorter than the counts imply. "+
			"Read-only."),
	)
}

func (s *Server) handleUsageSummary(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// Refresh the retrieval_stats projection so injection/read totals are current
	// even between gardener passes.
	if err := store.RebuildRetrievalStats(ctx, s.cfg.DB); err != nil {
		s.logger.Warn("usage_summary: rebuild retrieval stats", "error", err)
	}
	summary, err := store.GetUsageSummary(ctx, s.cfg.DB)
	if err != nil {
		return errResult("usage_summary", err)
	}
	// topInjected/topUtility are the only fields in this payload that carry an
	// identifier rather than a count. Every other field is a count keyed by a
	// CLOSED vocabulary -- memory kinds, session and task statuses, gardener
	// proposal kinds, event kinds, injection surfaces -- and none of them names a
	// memory, a note, a title, or a project.
	//
	// Those counts stay machine-wide, deliberately, and the tool description says
	// so. They are volume, never content, and they are the same number for every
	// caller, so they say nothing about which project a given caller cannot see.
	// Recomputing them over readable rows would be strictly WORSE: the number
	// would then vary with the caller's binding, and differencing two sessions'
	// summaries would measure a fenced project's volume exactly. Project
	// existence is already public to agents by design (project_list reports every
	// project and its isolation state), so a machine-wide total discloses nothing
	// the fence promised to hide.
	readable := s.memoryReadable(ctx)
	summary.Retrieval.TopInjected = fenceTopMemories(summary.Retrieval.TopInjected,
		func(nc store.NamedCount) string { return nc.ID }, readable)
	summary.Retrieval.TopUtility = fenceTopMemories(summary.Retrieval.TopUtility,
		func(ns store.NamedScore) string { return ns.ID }, readable)
	return jsonResult(summary)
}

// fenceTopMemories drops the rows whose memory this caller may not read. A
// memory NAME is content, not metadata: a ranked list of a confidential
// project's most-injected memories is that project's table of contents, and it
// reached every caller because the ranking is machine-wide and the rows carry no
// project of their own.
//
// It DROPS rather than backfills from the next-ranked readable row. Three
// reasons, in order:
//
//  1. The store's query already applied its LIMIT, so the tool surface only ever
//     sees the top N -- backfilling would mean re-ranking inside internal/store,
//     which is where the policy would then have to be repeated.
//  2. Dropping is what the sibling per-row read fence does (trialsForCaller
//     filters a LIMIT-ed query the same way, and for the same reason), so the
//     fence tells one story rather than two.
//  3. The residual signal is weak and ambiguous: a short list is equally
//     explained by a machine with fewer than N injected memories. That is
//     exactly why no "N withheld" count is added here -- unlike a single
//     entity's write response, where the marker distinguishes "policy removed
//     this" from "the field is empty", a count beside an aggregate would be an
//     unambiguous readout of hidden volume, which is the one thing the ranking
//     must not report.
//
// An all-withheld list returns to nil rather than an empty slice, so it
// marshals as null exactly like a list that was empty to begin with -- []
// against null would reintroduce the distinction the missing count avoids. When
// nothing is dropped the input slice is returned untouched, so an open project's
// caller gets byte-for-byte the payload it got before the fence existed.
func fenceTopMemories[T any](rows []T, id func(T) string, readable func(string) bool) []T {
	keep := make([]T, 0, len(rows))
	for _, r := range rows {
		if readable(id(r)) {
			keep = append(keep, r)
		}
	}
	if len(keep) == len(rows) {
		return rows
	}
	if len(keep) == 0 {
		return nil
	}
	return keep
}

// memoryReadable answers "may this connection read the memory with this id",
// resolving the project from the row because a ranked stat row carries none.
// The answer is cached per PROJECT, not per memory: the ranking is over one
// machine's memories, so a handful of lookups covers both lists.
//
// It fails closed on every uncertainty -- a lookup error, a row deleted since
// the summary was computed, a fence that could not be evaluated. fenceRead
// returns non-nil for a refusal and for a failed lookup alike; here they mean
// the same thing, because a row this call cannot vouch for must not be named.
func (s *Server) memoryReadable(ctx context.Context) func(memoryID string) bool {
	byProject := make(map[string]bool, 2)
	return func(memoryID string) bool {
		m, found, err := store.MemoryByID(ctx, s.cfg.DB, memoryID)
		if err != nil {
			s.logger.Warn("usage_summary: top-list scope lookup", "id", memoryID, "error", err)
			return false
		}
		if !found {
			return false
		}
		if ok, seen := byProject[m.Project]; seen {
			return ok
		}
		ok := s.fenceReadQuiet(ctx, m.Project) == nil
		byProject[m.Project] = ok
		return ok
	}
}
