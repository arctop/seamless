package mcp

import (
	"context"
	"errors"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store"
)

// recallMissQueryMax bounds the query text stored on a recall.miss event; the
// miss log is transport-class telemetry, not a transcript.
const recallMissQueryMax = 500

func recallTool() mcp.Tool {
	return mcp.NewTool("recall", hintRead(),
		mcp.WithDescription("Search durable knowledge (memories, notes) and the work record (tasks, trials, session findings) by meaning and keyword (fused), scoped to the current project plus global items. This is the single search entry point. Work-record hits carry a status (a task's status, a trial's outcome) and match on keyword only. With kind set and no query it lists that memory kind newest-first instead (browse): every match up to limit, and more counts the matches past it."),
		mcp.WithString("query", mcp.Description("what you are looking for; required unless kind is set (kind alone lists that kind newest-first)")),
		mcp.WithString("scope", enumOf(retrieve.RecallScopes), mcp.Description("what to search (default all)")),
		mcp.WithString("kind", enumOf(core.MemoryKinds), mcp.Description("only memories of this frontmatter kind (e.g. convention), not a hit's item type: scope selects notes, tasks, trials or sessions; implies memories-only, so any scope that excludes memories is rejected; with no query, lists the kind newest-first")),
		mcp.WithString("project", mcp.Description("project slug; defaults to the bound session's project")),
		mcp.WithInteger("limit", mcp.Min(1), mcp.Max(retrieve.MaxRecallLimit),
			mcp.Description("maximum results (default 10, max 100); a kind listing returns this many when that many match")),
	)
}

func (s *Server) handleRecall(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query, kind := argString(req, "query"), argString(req, "kind")
	// No query + kind is the browse mode (retrieve.Recall lists the kind
	// newest-first); no query and no kind has nothing to search or list.
	if query == "" && kind == "" {
		return errResult("recall", errors.New("query is required unless kind is set"))
	}
	browse := query == ""
	// A recall whose scope is a locked project it did not name -- the
	// connection's own session lies there, waiting for the owner's approval --
	// is a filter, not an ask: it searches what remains in scope (the global
	// scope, unless the locked project is sealed and reads nothing outside
	// itself), names the locked project, and files no request. Naming the
	// project outright (project=) is the ask, and is refused at the lock.
	locked, globalOK := "", true
	if argString(req, "project") == "" {
		// lockedBinding answers only for a binding getBinding drops (a locked
		// project is never Visible), and nothing at all with peer_auth off.
		if b, ok := s.lockedBinding(ctx); ok {
			locked = b.project
			ctx = access.WithLockRequester(ctx, nil)
			iso, err := store.IsolationOf(ctx, s.cfg.DB, locked)
			if err != nil {
				return errResult("recall", err)
			}
			globalOK = !iso.FencesInbound()
		}
	}
	project := ""
	if locked == "" {
		var err error
		if project, err = s.resolveReadScope(ctx, argString(req, "project")); err != nil {
			return errResult("recall", err)
		}
	}
	// A project a live read grant opens to this one joins the scope while
	// the grant lives (isolation.requests; nil while it is off).
	also, err := s.grantedScope(ctx, project)
	if err != nil {
		return errResult("recall", err)
	}
	hits := []retrieve.Hit{}
	limit := argInt(req, "limit", retrieve.DefaultRecallLimit)
	more := 0
	if globalOK {
		var err error
		hits, more, err = s.cfg.Retrieve.RecallCounted(ctx, retrieve.RecallInput{
			Query: query, Project: project, Scope: argString(req, "scope"),
			Kind: kind, Limit: limit, Also: also,
		})
		if err != nil {
			return errResult("recall", err)
		}
	}
	// Record what was surfaced so read-after-inject stats can be derived later.
	// item_scores rides along (parallel to item_ids, which is already in rank
	// order) so future scoring can weigh how confidently each hit ranked --
	// dropping the score here would lose it forever, the log being append-only.
	// A browse's hits record under their own source: injectedUtilityWeight
	// counts only source "recall" as query-gated demand, so a kind listing
	// stays passive-class exposure like the briefing it resembles
	// (closed-loop-utility-signal-contract).
	switch {
	case len(hits) > 0:
		ids := make([]string, len(hits))
		scores := make([]float64, len(hits))
		for i, h := range hits {
			ids[i] = h.ID
			scores[i] = h.Score
		}
		source := "recall"
		if browse {
			source = "recall-browse"
		}
		s.record(ctx, core.EventInjected, s.boundSession(ctx), project, "",
			map[string]any{"query": query, "item_ids": ids, "item_scores": scores, "source": source})
	case !browse:
		// A zero-hit recall is demand for knowledge that does not exist -- the
		// signal the gardener's memory-wanted pass clusters into proposals. A
		// kind-filtered miss is still demand (the pass reads only query); kind
		// rides along only when set, so the unfiltered payload shape is stable.
		// An empty browse records nothing: with no query text there is no
		// demand for the pass to cluster, and "this project has no conventions
		// yet" is not a missing memory.
		payload := map[string]any{"query": events.Truncate(query, recallMissQueryMax), "scope": argString(req, "scope"),
			"limit": limit, "source": "recall"}
		if kind != "" {
			payload["kind"] = kind
		}
		s.record(ctx, core.EventRecallMiss, s.boundSession(ctx), project, "", payload)
	}
	out := map[string]any{"hits": hits}
	if more > 0 {
		out["more"] = more
		out["note"] = browseMoreNote(kind, limit, more)
	}
	if locked != "" {
		// Name only, never a count of what it holds (owner decision Q7).
		out["locked"] = []string{lockedMayHold(locked)}
	}
	return jsonResult(out)
}

// browseMoreNote tells a kind listing's caller that the hits are not the whole
// kind, and how to get the rest: a bigger limit while one fits, else a query
// that searches within the kind, since a listing stops at MaxRecallLimit.
func browseMoreNote(kind string, limit, more int) string {
	if total := limit + more; total <= retrieve.MaxRecallLimit {
		return fmt.Sprintf("%d more %s memories match past limit=%d; pass limit=%d to list all of them", more, kind, limit, total)
	}
	return fmt.Sprintf("%d more %s memories match past limit=%d, and a listing stops at %d; add a query to search within kind=%s, or memory_read name=<name> for one you know",
		more, kind, limit, retrieve.MaxRecallLimit, kind)
}
