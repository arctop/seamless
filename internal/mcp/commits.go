package mcp

import (
	"fmt"
	"slices"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/validate"
)

// Commit records (config commits.record, off by default): session_end and
// tasks_update take an optional commits argument, the full ids of the commits
// a session made or a task's work produced, recorded on that session or task
// for the commit-matching snapshot (internal/commitfeed).
//
// Off, the argument does not exist. registerTools registers both tools
// without it (offerCommits), so tools/list advertises exactly the schemas they
// had before commit records, and the validator refuses a commits argument as
// an unknown parameter, word for word as it always did; the handlers never see
// one. Catalog keeps the argument, so the reference documents it, saying when
// it is offered.

// commitsParam is the argument's name on both tools.
const commitsParam = "commits"

// The parts of the argument's description both tools share: the format, and
// the bound with when the argument exists at all.
var (
	commitIDFormatDesc = "the 40- or 64-character hex id git prints (git rev-parse HEAD), never an abbreviation"
	commitIDBoundDesc  = fmt.Sprintf("At most %d per call; repeats are ignored. Offered only while this server "+
		"records commits (commits.record)", store.MaxCommitsPerCall)
)

// sessionCommitsArgDesc documents session_end's commits argument.
var sessionCommitsArgDesc = "Full ids of the commits this session made, recorded on the session for an integrity " +
	"monitor that matches a branch's commits to the work that made them: " + commitIDFormatDesc + ". " +
	commitIDBoundDesc

// taskCommitsArgDesc documents tasks_update's commits argument.
var taskCommitsArgDesc = "Full ids of the commits that did this task's work, recorded on the task for an integrity " +
	"monitor that matches a branch's commits to the work that made them: " + commitIDFormatDesc + ". Recording " +
	"them is an update like any other: a task another session holds refuses it. " + commitIDBoundDesc

// offerCommits is t as this server registers it: unchanged while commit
// records are on, without the commits argument while they are off.
func (s *Server) offerCommits(t mcp.Tool) mcp.Tool {
	if s.cfg.RecordCommits {
		return t
	}
	return withoutParam(t, commitsParam)
}

// withoutParam returns t with one parameter taken out of its input schema. The
// maps and slices are copied rather than edited, so a schema another value of
// the tool shares is never changed under it.
func withoutParam(t mcp.Tool, name string) mcp.Tool {
	props := make(map[string]any, len(t.InputSchema.Properties))
	for k, v := range t.InputSchema.Properties {
		if k != name {
			props[k] = v
		}
	}
	t.InputSchema.Properties = props
	drop := func(p string) bool { return p == name }
	if t.InputSchema.Required != nil {
		t.InputSchema.Required = slices.DeleteFunc(slices.Clone(t.InputSchema.Required), drop)
	}
	if t.InputSchema.PropertyOrder != nil {
		t.InputSchema.PropertyOrder = slices.DeleteFunc(slices.Clone(t.InputSchema.PropertyOrder), drop)
	}
	return t
}

// commitsArg reads the commits argument: nil when absent, the normalized ids
// (lowercase, distinct, in first-seen order) when present, and an error naming
// the bad entry otherwise. It runs before the call changes anything, so a bad
// id refuses the whole call.
func (s *Server) commitsArg(req mcp.CallToolRequest) ([]string, error) {
	raw := argStrings(req, commitsParam)
	if raw == nil {
		return nil, nil
	}
	if !s.cfg.RecordCommits {
		// Unreachable: the registered schema has no commits property while
		// recording is off, so the validator refused the call. Kept so a
		// handler can never record one anyway.
		return nil, fmt.Errorf("unknown parameter %q: this server does not record commits", commitsParam)
	}
	ids, err := validate.CommitIDs(raw, store.MaxCommitsPerCall)
	if err != nil {
		return nil, fmt.Errorf("invalid %s: %w", commitsParam, err)
	}
	return ids, nil
}
