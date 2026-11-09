package mcp

import (
	"context"
	"errors"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/features"
	"github.com/arctop/seamless/internal/store"
)

// errResearchDisabled is the refusal for an exposed tool that reaches data owned
// by the research feature while that feature is off. The three research tools
// themselves never produce it: the tool filter removes them from tools/list and
// rejects a call as tool-not-found, so only a tool that stays exposed for its
// OTHER kinds -- favorite_set -- needs an in-handler gate.
var errResearchDisabled = errors.New("the research feature is disabled, so trials are not available: " +
	"enable it in the console under Settings -> Features (no trial data was deleted)")

// effectiveFeatures resolves the optional-features config for ONE request: the
// file/env base carried on Config, with the stored override row (the console
// toggle, or the grandfather migration) decoded over it.
//
// Resolution is LIVE -- read at the point of use rather than frozen at
// construction -- so a toggle in the console applies from the very next tool call
// without restarting the daemon. The cost is one single-row primary-key lookup on
// a single-connection pool.
//
// It is failure-soft by contract: a store error logs and falls back to the base
// config. A corrupt override row must never fail an agent's tool call.
func (s *Server) effectiveFeatures(ctx context.Context) config.Features {
	if s.cfg.DB == nil {
		return s.cfg.Features
	}
	cfg, _, err := store.FeaturesConfig(ctx, s.cfg.DB, s.cfg.Features)
	if err != nil {
		s.logger.Warn("features config unreadable; falling back to file/env config", "error", err)
		return s.cfg.Features
	}
	return cfg
}

// researchEnabled reports whether the research feature is on for this request.
func (s *Server) researchEnabled(ctx context.Context) bool {
	return features.Enabled(s.effectiveFeatures(ctx), features.Research)
}

// exposedTools is the mcpserver.ToolFilterFunc registered in New: it drops every
// tool owned by an optional feature that is currently off. The tool->feature
// mapping comes from features.HiddenTools, never from a hand-written list here,
// so a future optional feature gates itself by being registered.
//
// mcp-go applies a tool filter at BOTH tools/list (filteredTools) and tools/call
// (passesToolFilters, which passes only the requested tool), so there is exactly
// one gate and no drift between what is advertised and what is callable.
//
// A gated call therefore returns the mcp-go "tool 'lab_open' not found" JSON-RPC
// error. That is the intended semantics -- from this server's perspective the
// tool does not exist -- and it is what an agent holding a stale cached tool list
// should see. Accepted tradeoff: mcp-go rejects inside handleToolCall, BEFORE the
// handler middleware chain, so a gated call records no tool.call event in
// Interactions. Do not add a redundant middleware layer to recover the event: it
// would double the gate and give two places for the answer to drift.
//
// Registration is untouched by all of this. ToolCount stays the registered count
// and Catalog() stays full; only the live list shrinks.
func (s *Server) exposedTools(ctx context.Context, tools []mcp.Tool) []mcp.Tool {
	if access.AccountOf(ctx) != "" {
		tools = restrictedToolText(tools)
	}
	hidden := features.HiddenTools(s.effectiveFeatures(ctx))
	if len(hidden) == 0 {
		return tools
	}
	out := make([]mcp.Tool, 0, len(tools))
	for _, t := range tools {
		if hidden[t.Name] {
			continue
		}
		out = append(out, t)
	}
	return out
}
