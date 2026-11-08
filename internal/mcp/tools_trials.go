package mcp

import (
	"context"
	"errors"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// trialContextLimit is how many recent trials lab_open returns as context.
const trialContextLimit = 10

func labOpenTool() mcp.Tool {
	return mcp.NewTool("lab_open", hintSet(),
		mcp.WithDescription("Open a research lab and get its recent trial history for context -- including trials other agents recorded, so parallel agents can share one investigation. Use a lab for systematic investigations whose expected-vs-actual results must outlive the session. Binds the lab to this connection so later trial_record calls inherit it. A lab is just a label -- a new one needs no setup; it exists once its first trial is recorded."),
		mcp.WithString("lab", mcp.Required(), mcp.Description("lab name (a stable label for a line of investigation)")),
		mcp.WithString("goal", mcp.Description("optional note on what this lab is investigating")),
	)
}

func (s *Server) handleLabOpen(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	lab := argString(req, "lab")
	if lab == "" {
		return errResult("lab_open", errors.New("lab is required"))
	}
	s.setBindingLab(ctx, lab)
	// A lab's history crosses projects by design, so it carries the same leak
	// trial_query does and takes the same fence.
	trials, err := s.trialsForCaller(ctx, store.TrialFilter{Lab: lab, Limit: trialContextLimit})
	if err != nil {
		return errResult("lab_open", err)
	}
	recent := make([]map[string]any, 0, len(trials))
	for _, tr := range trials {
		recent = append(recent, trialJSON(tr))
	}
	out := map[string]any{"lab": lab, "trial_count": len(trials), "recent_trials": recent}
	if s.statelessConnection(ctx) {
		out["stateless"] = "This connection keeps no MCP session, so the lab is not bound to it: pass lab=" + lab +
			" to every trial_record."
	}
	return jsonResult(out)
}

func trialRecordTool() mcp.Tool {
	return mcp.NewTool("trial_record", hintAdd(),
		mcp.WithDescription("Record one experiment in a research lab: what changed, expected vs actual, outcome, and optional structured metrics for later querying. Inherits the lab from lab_open unless you pass one."),
		mcp.WithString("title", mcp.Required(), mcp.Description("short trial title")),
		mcp.WithString("lab", mcp.Description("lab name; defaults to the lab opened on this connection")),
		mcp.WithString("project", mcp.Description(writeProjectArgDesc)),
		mcp.WithString("changes", mcp.Description("what was changed for this trial")),
		mcp.WithString("expected", mcp.Description("expected result")),
		mcp.WithString("actual", mcp.Description("observed result")),
		mcp.WithString("outcome", mcp.Description("suggested values pass|fail|partial|inconclusive; free text accepted")),
		mcp.WithObject("metrics", mcp.Description(`optional object of structured metrics, e.g. {"hz":497,"err_pct":0.2} (a JSON-object string is also accepted)`)),
	)
}

func (s *Server) handleTrialRecord(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	title := argString(req, "title")
	if title == "" {
		return errResult("trial_record", errors.New("title is required"))
	}
	lab := argString(req, "lab")
	if lab == "" {
		lab = s.boundLab(ctx)
	}
	if lab == "" && s.statelessConnection(ctx) {
		return errResult("trial_record", errors.New("no lab: pass lab -- this connection keeps no MCP session, so lab_open cannot bind one"))
	}
	if lab == "" {
		return errResult("trial_record", errors.New("no lab: call lab_open first or pass lab"))
	}
	id, err := core.NewID()
	if err != nil {
		return errResult("trial_record", err)
	}
	project, err := s.resolveWriteScope(ctx, argString(req, "project"))
	if err != nil {
		return errResult("trial_record", err)
	}
	tr := core.Trial{
		ID: id, Lab: lab, Title: title,
		Changes:  argRaw(req, "changes"),
		Expected: argRaw(req, "expected"),
		Actual:   argRaw(req, "actual"),
		Outcome:  core.TrialOutcome(argString(req, "outcome")),
		Metrics:  argObject(req, "metrics"), SessionID: s.boundSession(ctx), ProjectSlug: project,
		// The recording principal, which store.CreateTrial would stamp anyway;
		// set here so the result can say so.
		Account:   access.AccountOf(ctx),
		CreatedAt: time.Now().UTC(),
	}
	if err := store.CreateTrial(ctx, s.cfg.DB, tr); err != nil {
		return errResult("trial_record", err)
	}
	s.record(ctx, core.EventTrialRecorded, tr.SessionID, project, id,
		map[string]any{"lab": lab, "title": title, "outcome": string(tr.Outcome)})
	return jsonResult(trialJSON(tr))
}

func trialQueryTool() mcp.Tool {
	return mcp.NewTool("trial_query", hintRead(),
		mcp.WithDescription("Query recorded trials, filtered by lab, outcome, and/or an exact-match metrics filter over the metrics recorded by trial_record."),
		mcp.WithString("lab", mcp.Description("lab name; defaults to the lab opened on this connection")),
		mcp.WithString("outcome", mcp.Description("filter by outcome (e.g. fail)")),
		mcp.WithObject("metrics_filter", mcp.Description(`optional object; trials whose metrics equal every given key match, e.g. {"hz":497} (a JSON-object string is also accepted)`)),
		mcp.WithInteger("limit", mcp.Min(1), mcp.Max(store.MaxTrialQueryLimit),
			mcp.Description("max results (default 20, max 200)")),
	)
}

func (s *Server) handleTrialQuery(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	lab := argString(req, "lab")
	if lab == "" {
		lab = s.boundLab(ctx)
	}
	// Trials were the one read that never saw a project at all: the query is by
	// lab, so an isolated project's trials answered any agent's question.
	trials, err := s.trialsForCaller(ctx, store.TrialFilter{
		Lab:           lab,
		Outcome:       argString(req, "outcome"),
		Limit:         argInt(req, "limit", store.DefaultTrialQueryLimit),
		MetricsEquals: argObject(req, "metrics_filter"),
	})
	if err != nil {
		return errResult("trial_query", err)
	}
	out := make([]map[string]any, 0, len(trials))
	for _, tr := range trials {
		out = append(out, trialJSON(tr))
	}
	return jsonResult(map[string]any{"lab": lab, "trials": out})
}

// trialJSON renders a trial for a tool response, omitting empty fields.
func trialJSON(tr core.Trial) map[string]any {
	j := map[string]any{"id": tr.ID, "lab": tr.Lab, "title": tr.Title}
	if tr.Changes != "" {
		j["changes"] = tr.Changes
	}
	if tr.Expected != "" {
		j["expected"] = tr.Expected
	}
	if tr.Actual != "" {
		j["actual"] = tr.Actual
	}
	if tr.Outcome != "" {
		j["outcome"] = string(tr.Outcome)
	}
	if len(tr.Metrics) > 0 {
		j["metrics"] = tr.Metrics
	}
	// Provenance (peer_auth.restricted): the restricted account that recorded
	// it. The owner's trials carry no such key.
	if tr.Account != "" {
		j["account"] = tr.Account
	}
	return j
}
