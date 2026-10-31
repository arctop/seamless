package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/validate"
)

// isolationStatesDesc renders the isolation vocabulary for the two project tool
// descriptions. Derived from core.Isolations rather than transcribed, so a
// fourth state cannot reach the enum while the prose still lists three.
var isolationStatesDesc = func() string {
	names := make([]string, len(core.Isolations))
	for i, v := range core.Isolations {
		names[i] = string(v)
	}
	return strings.Join(names, "|")
}()

func projectListTool() mcp.Tool {
	return mcp.NewTool("project_list", hintRead(),
		mcp.WithDescription("List every project (slug, name, description, isolation). Use it to learn the exact slug before a deliberate cross-project write or a project_create -- coining a near-duplicate of a slug that already exists is the failure this prevents -- and to see whether work already has a home. You usually do NOT need it to pick a scope: memory, note, and task calls inherit the project from the session binding, and passing project= is for writing outside that on purpose. Each row carries its isolation state ("+isolationStatesDesc+"), which is what a cross-project call has to respect: a confidential or sealed project is readable only from a session bound to it, and a sealed one takes no writes from outside either. It returns identity, not contents; to search what is inside a project, use recall."),
	)
}

func (s *Server) handleProjectList(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ps, err := store.ListProjects(ctx, s.cfg.DB)
	if err != nil {
		return errResult("project_list", err)
	}
	out := make([]map[string]any, 0, len(ps))
	for _, p := range ps {
		// peer_auth: a project this connection's OS account may not reach is not
		// listed. Unlike the isolation fence -- which keeps a fenced project's
		// existence visible so an agent can bind to it -- there is no binding
		// that would let this caller in, so its name is nothing to act on.
		if !access.Visible(ctx, p.Slug) {
			continue
		}
		// Always present, never omitempty: "open" is a real answer, and a missing
		// field would be indistinguishable from a server that does not fence at
		// all -- the one reading an agent must not get wrong.
		state := p.Isolation
		if state == "" {
			state = core.IsolationOpen // core.Project documents "" as open
		}
		out = append(out, map[string]any{
			"id": p.ID, "slug": p.Slug, "name": p.Name, "description": p.Description,
			"isolation": string(state),
		})
	}
	return jsonResult(map[string]any{"projects": out})
}

func projectCreateTool() mcp.Tool {
	return mcp.NewTool("project_create", hintSet(),
		mcp.WithDescription("Register a project up front, with a human-readable name and an optional description. You rarely need this: any durable write naming an unknown project slug (memory_write, notes_create, tasks_add, capture_url, trial_record) already registers that project, and a git repo maps itself to one on its first session. Reach for this only to give a project a proper name and description BEFORE anything is written into it, or to create one you will not write to yet -- an auto-registered project is named after its own slug until someone fixes it. Call project_list first: coining a near-duplicate of an existing slug is the failure mode here. To divide an existing project into children, use gardener_split rather than creating them by hand. The slug defaults to a slugified name; \"global\" and \"all\" are reserved, and an existing slug is an error, not an update -- this never renames or edits a project."),
		mcp.WithString("name", mcp.Required(), mcp.Description("human-readable project name")),
		mcp.WithString("slug", mcp.Description("optional explicit slug")),
		mcp.WithString("description", mcp.Description("optional one-line description")),
		mcp.WithString("isolation", enumOf(core.Isolations),
			mcp.Description("optional isolation state ("+isolationStatesDesc+"); omit for open. "+
				"open shares normally. confidential means nothing leaves: agents in other projects never read this one, "+
				"and agents bound to it cannot write outside it. sealed adds the inbound half -- agents here see only this "+
				"project: no global memories, no family, no cross-project reads. Set it at creation for work that is "+
				"sensitive from the start; there is no tool to change it afterwards, because tightening detaches family and "+
				"parent links and is an owner decision, made on an owner surface. "+
				"Isolation requires a standalone project, so do not create an isolated child of another project.")),
	)
}

func (s *Server) handleProjectCreate(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name := argString(req, "name")
	if name == "" {
		return errResult("project_create", errors.New("name is required"))
	}
	slug := argString(req, "slug")
	if slug == "" {
		slug = core.Slugify(name)
	}
	if normalizeProject(slug) == "" {
		return errResult("project_create", fmt.Errorf("slug %q is reserved for the global namespace", slug))
	}
	// Without this, a project could take the name of the widening token and then
	// be permanently unreachable through it -- gardener_request reads "all" before
	// it resolves anything as a slug.
	if slug == allProjectsToken {
		return errResult("project_create", fmt.Errorf("slug %q is reserved: gardener_request reads it as every project", slug))
	}
	// The slug becomes a directory under the memory/ and notes/ trees; reject
	// separators and ".." so no later write can escape its tree.
	if err := validate.Name(slug); err != nil {
		return errResult("project_create", fmt.Errorf("invalid slug %q: %w", slug, err))
	}
	// peer_auth: creating a project is a write into it. Judged before the
	// existence check, so a slug sealed to another OS account answers with the
	// seal whether or not it exists yet.
	if !access.Writable(ctx, slug) {
		return errResult("project_create", access.Refusal(ctx, slug))
	}
	// Absent -> the default; present but uninterpretable -> an error naming the
	// accepted values. validateMiddleware already enforces the declared enum, so
	// this is the same guard at the handler boundary rather than a second rule --
	// and validate.Isolation is where the accepted set is derived from
	// core.Isolations, never transcribed.
	isolation := core.IsolationOpen
	if raw := argString(req, "isolation"); raw != "" {
		state, ierr := validate.Isolation(raw)
		if ierr != nil {
			return errResult("project_create", ierr)
		}
		isolation = state
	}
	id, err := core.NewID()
	if err != nil {
		return errResult("project_create", err)
	}
	now := time.Now().UTC()
	p := core.Project{
		ID: id, Slug: slug, Name: name, Description: argString(req, "description"),
		Isolation: isolation, CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateProject(ctx, s.cfg.DB, p); err != nil {
		if errors.Is(err, store.ErrSlugExists) {
			return errResult("project_create", fmt.Errorf("project slug %q already exists", slug))
		}
		return errResult("project_create", err)
	}
	return jsonResult(map[string]any{
		"project_id": id, "slug": slug, "name": name, "isolation": string(isolation),
	})
}
