package mcp

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store"
)

func sessionStartTool() mcp.Tool {
	return mcp.NewTool("session_start", hintSet(),
		mcp.WithDescription("Bind this connection to a work session and return its project briefing. "+
			"Claude Code and Codex agents running the Seamless hooks are bound to their own session automatically "+
			"(the 'Seam session' line in the briefing) and do not need this. Call it to bind a client without hooks, "+
			"to move this connection to another project, or to resume a named session. Later calls inherit the "+
			"session's project, so they need no project argument."),
		mcp.WithString("project", mcp.Description("Project slug to bind. Defaults to the project of the git repository "+
			"cwd is in, then to your own session's project. If cwd is in a repository too, the two must agree. "+
			"project=global binds the global scope; an unknown slug creates that project.")),
		mcp.WithString("cwd", mcp.Description("Absolute working directory. The git repository it is in decides the project, "+
			"and a repository maps itself to a project on its first session -- no setup step (`seamlessd map-repo` only "+
			"overrides the derived slug)")),
		mcp.WithString("name", mcp.Description("Resume the session with this name: a sess/* name from an earlier call, "+
			"or the cc/... or cx/... on your briefing's 'Seam session' line. A new name starts a separate session under "+
			"that name; omit it to use your own session, or a fresh one")),
		mcp.WithString("model", mcp.Description("Model id powering this agent, exactly as the provider names it (e.g. claude-fable-5, gpt-5.5). Stamped onto memories/notes this session writes; hooks keep it current for Claude Code/Codex sessions, so pass it mainly from other clients")),
		// Machine identity. Only a client on a DIFFERENT machine than the daemon
		// needs these: cwd alone is ambiguous across devices, and the daemon will
		// not read its own disk to resolve another machine's path. A local client
		// sends none of them and nothing changes.
		mcp.WithString("host", mcp.Description("Machine this agent runs on (hostname). Defaults to the X-Seamless-Host header, then to the daemon's own host; pass it only when dialling a daemon on another machine")),
		mcp.WithString("repo_root", mcp.Description("Absolute git repository root enclosing cwd, resolved on YOUR machine. Required when host is not the daemon's: the daemon cannot read your filesystem to find it")),
		mcp.WithString("main_worktree_root", mcp.Description("Absolute root of the repository's MAIN checkout when repo_root is a linked worktree; defaults to repo_root")),
		mcp.WithString("repo_origin", mcp.Description("The repository's origin remote URL, which is how the same repo checked out on two machines is recognized as one project")),
	)
}

// scopeSource says how session_start arrived at the project it bound, so the
// result can tell the agent -- in the one place it is guaranteed to look -- what
// it got and what it did not need to do.
type scopeSource int

const (
	scopeFromCWD     scopeSource = iota // the repository cwd is in (or no project at all)
	scopeFromArg                        // the project argument
	scopeFromSession                    // the session being resumed
	scopeFromOwn                        // the agent's own process-bound session
	scopeFromAccount                    // peer_auth: the caller's OS account's own project
)

func (s *Server) handleSessionStart(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name := argString(req, "name")
	cwd := argString(req, "cwd")
	model := strings.TrimSpace(argString(req, "model"))
	// Explicit arg beats the connection header beats the daemon's own host: the
	// arg is the only one an agent can correct when its transport strips headers.
	host := strings.ToLower(strings.TrimSpace(argString(req, "host")))
	if host == "" {
		host = s.callerHost(ctx)
	}
	// Resolve the cwd to a project, registering a new repo->project mapping and
	// projects-table row when the agent works in a not-yet-mapped git repo. A
	// moved repo adopts its existing project here; surface that remap as an
	// event so the console shows the map healed itself.
	project, moved, err := s.cwdProject(ctx, store.CWDIdentity{
		Host:     host,
		CWD:      cwd,
		RepoRoot: strings.TrimSpace(argString(req, "repo_root")),
		MainRoot: strings.TrimSpace(argString(req, "main_worktree_root")),
		Origin:   strings.TrimSpace(argString(req, "repo_origin")),
	})
	warning := ""
	switch {
	case errors.Is(err, store.ErrRemoteRootUnknown):
		// A session on another machine that named no repo root. The session is
		// still worth having -- it just cannot be placed in a project, and saying
		// so is far better than failing the call or inventing a scope.
		warning = "host " + host + " is not this daemon's machine and sent no repo_root, " +
			"so this cwd could not be mapped to a project: the session is global. " +
			"Pass repo_root (and main_worktree_root/repo_origin) resolved on your own machine."
		s.logger.Warn("session_start: remote cwd without a repo root", "host", host, "cwd", cwd)
		project = ""
	case err != nil:
		return errResult("session_start", err)
	}
	if moved != nil {
		s.record(ctx, core.EventRepoMoved, "", project, "", map[string]any{
			"slug": moved.Slug, "new_path": moved.NewPath, "old_paths": moved.OldPaths,
		})
	}

	// A project argument names the scope outright -- the way agents expect to
	// start a session, and the only way for a client with no meaningful cwd (the
	// Claude app's chat surface). It is checked against the cwd rather than
	// silently preferred: the two disagreeing is a caller mistake worth one
	// round-trip, not a coin flip.
	how := scopeFromCWD
	if raw := strings.TrimSpace(argString(req, "project")); raw != "" {
		named, created, err := s.sessionProjectArg(ctx, raw, cwd, project)
		if err != nil {
			return errResult("session_start", err)
		}
		project, how = named, scopeFromArg
		warning = "" // the project argument placed the session, so an unplaceable cwd no longer matters
		if created {
			warning = "project " + raw + " did not exist and was created; if that was a typo, " +
				"session_start again with the project you meant"
		}
	}
	// peer_auth: a session lands only where this connection's OS account may
	// write. A configured account's session outside its own projects -- an
	// unmapped cwd, another repository -- lands in its own project instead; the
	// owner is refused a project sealed to another account outright.
	if placed, perr := access.SessionScope(ctx, project); perr != nil {
		return errResult("session_start", perr)
	} else if placed != project {
		project, how = placed, scopeFromAccount
		warning = ""
	}
	// Whether the caller chose a scope at all. A cwd outside any repository chose
	// nothing, and neither did a call with no cwd and no project.
	pinned := how == scopeFromArg || how == scopeFromAccount || project != ""

	// Resume a named session if it already exists.
	if name != "" {
		existing, ok, err := store.SessionByName(ctx, s.cfg.DB, name)
		if err != nil {
			return errResult("session_start", err)
		}
		if ok && !access.Visible(ctx, existing.ProjectSlug) {
			return errResult("session_start", fmt.Errorf("session %q is not reachable from this connection: "+
				"it belongs to a project this OS account may not reach -- choose another name", name))
		}
		if ok {
			if !pinned {
				project, how = existing.ProjectSlug, scopeFromSession
			}
			// Reactivate: a resumed session is live again. Without this a
			// completed/expired session stays terminal -- the per-call heartbeat
			// (TouchSession) only touches active sessions, so the idle reaper and
			// every active-session surface would treat the resumed agent as gone.
			existing.Status = core.SessionActive
			existing.UpdatedAt = time.Now().UTC()
			if err := store.UpdateSession(ctx, s.cfg.DB, existing); err != nil {
				return errResult("session_start", err)
			}
			s.stampSessionModel(ctx, existing.ID, model)
			s.setBinding(ctx, existing.ID, project)
			s.record(ctx, core.EventSessionStarted, existing.ID, project, "", map[string]any{"resumed": true})
			return jsonResult(s.withStatelessHint(ctx, withWarning(map[string]any{
				"session_id": existing.ID, "name": existing.Name,
				"project": project, "resumed": true, "scope": scopeNote(project, how),
				"briefing": s.briefing(ctx, host, project, "resume"),
			}, warning), existing.Name, project, existing.ProjectSlug))
		}
	}

	// The caller's own session, identified by the agent process that launched
	// this connection (agent_binding.go). Claude Code and Codex agents are already
	// bound to it, so this call only hands the connection back to it -- dropping a
	// session chosen by an earlier session_start -- and says the call was not
	// needed. A caller pinning a DIFFERENT project falls through to a fresh
	// session there instead: re-scoping its own session would leave the hooks,
	// which keep describing the repository it runs in, at odds with the binding.
	own, hasOwn := s.processSession(ctx)
	if name == "" && hasOwn && (!pinned || project == own.ProjectSlug) {
		s.clearBinding(ctx)
		stashAttribution(ctx, own.ID, own.ProjectSlug)
		s.stampSessionModel(ctx, own.ID, model)
		s.record(ctx, core.EventSessionStarted, own.ID, own.ProjectSlug, "",
			map[string]any{"resumed": true, "adopted": true, "agent_process": true})
		return jsonResult(withWarning(map[string]any{
			"session_id": own.ID, "name": own.Name,
			"project": own.ProjectSlug, "resumed": true, "scope": scopeNote(own.ProjectSlug, scopeFromOwn),
			"briefing": s.briefing(ctx, host, own.ProjectSlug, "explicit"),
		}, warning))
	}

	// Without an agent process to identify the caller (an older seam, or a
	// client Seamless did not launch), fall back to adopting the sole active
	// ambient session sharing the cwd: with no explicit name and exactly one
	// candidate, the SessionStart hook most likely created it for this agent, so
	// resume that row instead of minting a second sess/* one. Zero or many
	// candidates fall through to a fresh session. A caller that DID name its
	// process never gets here: it owns no live ambient (or several), and a sole
	// same-cwd ambient would then be some other agent's.
	if name == "" && callerAgentProcess(ctx) == "" {
		if ambient, ok := s.soleAmbientByCWD(ctx, host, cwd); ok {
			if project == "" {
				project = ambient.ProjectSlug
			}
			ambient.ProjectSlug = project
			ambient.UpdatedAt = time.Now().UTC()
			if err := store.UpdateSession(ctx, s.cfg.DB, ambient); err != nil {
				return errResult("session_start", err)
			}
			s.stampSessionModel(ctx, ambient.ID, model)
			s.setBinding(ctx, ambient.ID, project)
			s.record(ctx, core.EventSessionStarted, ambient.ID, project, "",
				map[string]any{"resumed": true, "adopted": true})
			return jsonResult(s.withStatelessHint(ctx, withWarning(map[string]any{
				"session_id": ambient.ID, "name": ambient.Name,
				"project": project, "resumed": true, "scope": scopeNote(project, how),
				"briefing": s.briefing(ctx, host, project, "explicit"),
			}, warning), ambient.Name, project, ambient.ProjectSlug))
		}
	}

	id, err := core.NewID()
	if err != nil {
		return errResult("session_start", err)
	}
	if name == "" {
		name = "sess/" + shortID(id)
	}
	now := time.Now().UTC()
	externalSessionID, externalClient := s.linkedExternalIdentity(ctx, host, cwd)
	sess := core.Session{
		ID: id, Name: name, ProjectSlug: project, Status: core.SessionActive,
		CWD: cwd, Host: host, Source: "explicit", Model: model,
		ExternalSessionID: externalSessionID, ExternalClient: externalClient,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateSession(ctx, s.cfg.DB, sess); err != nil {
		return errResult("session_start", err)
	}
	s.setBinding(ctx, id, project)
	s.record(ctx, core.EventSessionStarted, id, project, "", nil)
	return jsonResult(s.withStatelessHint(ctx, withWarning(map[string]any{
		"session_id": id, "name": name, "project": project, "scope": scopeNote(project, how),
		"briefing": s.briefing(ctx, host, project, "explicit"),
	}, warning), name, project, project))
}

// sessionProjectArg resolves session_start's project argument: validated like
// every project argument (validateProjectArg is the path-traversal defense),
// refused when it contradicts the repository cwd is in, and registered when new
// -- the same rule a durable write follows (constraint
// write-scope-registers-the-project-it-names), so naming a new project is an
// ordinary choice rather than an error that sends the agent to the global scope.
// created reports a registration, which the caller surfaces so a typo is seen.
func (s *Server) sessionProjectArg(ctx context.Context, raw, cwd, cwdProject string) (project string, created bool, err error) {
	project, err = validateProjectArg(raw)
	if err != nil {
		return "", false, err
	}
	// Named outright, so refused outright rather than redirected: before the
	// conflict check (which would name the cwd's project) and before
	// EnsureProject (which would register a slug this account may not create).
	if !access.Writable(ctx, project) {
		return "", false, access.Refusal(ctx, project)
	}
	if cwdProject != "" && project != cwdProject {
		return "", false, fmt.Errorf("project %q conflicts with cwd %q, which is in project %q: "+
			"pass one or the other (cwd places the session in its repository's project)", raw, cwd, cwdProject)
	}
	if project == "" {
		return "", false, nil
	}
	_, exists, err := store.ProjectBySlug(ctx, s.cfg.DB, project)
	if err != nil {
		return "", false, err
	}
	if _, err := store.EnsureProject(ctx, s.cfg.DB, project, project); err != nil {
		return "", false, err
	}
	return project, !exists, nil
}

// cwdProject resolves a session_start cwd to its project. Normally that
// registers a not-yet-mapped repository as a project (store.RegisterProjectForCWD).
// A configured peer_auth account only ever works inside its own projects, so
// for it the map is read and never grown: a repository it opens must not mint a
// project, and a mapping the owner made is all it needs.
func (s *Server) cwdProject(ctx context.Context, id store.CWDIdentity) (string, *store.RepoMapAdoption, error) {
	if v, ok := access.FromContext(ctx); ok && v.Principal().Kind == access.KindUser {
		project, err := store.ResolveProjectForCWD(ctx, s.cfg.DB, id.Host, id.CWD)
		return project, nil, err
	}
	return store.RegisterProjectForCWD(ctx, s.cfg.DB, id, s.cfg.LocalHost)
}

// stampSessionModel records a self-reported model id on a resumed/adopted
// session. Best-effort: attribution must never fail a session_start, so an
// error logs and the session simply keeps its previous (possibly empty) model.
// No-op on an empty model -- the hooks' sniffed value survives an agent that
// does not pass one.
func (s *Server) stampSessionModel(ctx context.Context, sessionID, model string) {
	if model == "" {
		return
	}
	if err := store.SetSessionModel(ctx, s.cfg.DB, sessionID, model); err != nil {
		s.logger.Warn("session_start: set model", "error", err)
	}
}

// scopeNote explains, in the session_start result, how project scope was
// resolved -- so an agent sees what it got and what it did not need to do. The
// own-session case says outright that the call was unnecessary: that is the
// lesson that keeps a Claude Code or Codex agent from calling it again. The cwd
// case says the repo->project mapping is automatic, so `seamlessd map-repo` is
// never mistaken for a required setup step (the map grows itself on a repo's
// first session; see store.RegisterProjectForCWD).
func scopeNote(project string, how scopeSource) string {
	switch {
	case how == scopeFromOwn:
		where := fmt.Sprintf("project %q", project)
		if project == "" {
			where = "the global scope"
		}
		return "This is your own session (" + where + "). Claude Code and Codex bind it to your tool calls " +
			"automatically, so you did not need session_start for it; call session_start only to move to " +
			"another project or resume a named session."
	case project == "" && how == scopeFromArg:
		return "global scope, as named: unscoped writes from this connection land in EVERY project's briefing."
	case project == "":
		return "global scope: no project argument, and this cwd is not inside a git repo, so nothing mapped. " +
			"Pass project=<slug> here, or on each durable write, to target a project."
	case how == scopeFromArg:
		return fmt.Sprintf("project %q, as named.", project)
	case how == scopeFromAccount:
		return fmt.Sprintf("project %q: this connection's OS account works only in its own projects "+
			"(peer_auth), so its sessions land here whatever the cwd.", project)
	case how == scopeFromSession:
		return fmt.Sprintf("project %q, the resumed session's.", project)
	default:
		return fmt.Sprintf("project %q. Repo->project mapping is automatic -- a repo maps itself to a "+
			"project on its first session, so there is no setup step. `seamlessd map-repo` only "+
			"overrides a repo's derived slug.", project)
	}
}

// briefing assembles the session_start briefing for the project the session was
// bound to, degrading to "" on error. The project is passed rather than
// re-derived from the cwd, because a project argument, a resumed session and the
// agent's own session all bind projects the cwd does not name. The failure is
// logged (it was previously discarded silently): a broken briefing should never
// fail a session_start, but it must not vanish without a trace.
func (s *Server) briefing(ctx context.Context, host, project, source string) string {
	briefing, _, err := s.cfg.Retrieve.ProjectBriefing(ctx, project, retrieve.BriefingInput{Host: host, Source: source})
	if err != nil {
		s.logger.Warn("session_start: briefing", "error", err)
		return ""
	}
	return briefing
}

// linkedExternalIdentity resolves the client/id pair to stamp on a freshly
// created explicit session, so a graceful SessionEnd closes it alongside its
// ambient rather than leaving it for the idle reaper. The agent's own
// process-bound session is the exact answer. A caller that named its process but
// owns no single live session gets no link -- any other candidate would be some
// other agent's. Only a caller that named no process falls back to the sole
// same-cwd ambient. Ambiguity yields empty values so the session falls back to
// the reaper instead of risking a link to the wrong agent. Best-effort.
func (s *Server) linkedExternalIdentity(ctx context.Context, host, cwd string) (externalSessionID, externalClient string) {
	if own, ok := s.processSession(ctx); ok {
		return own.ExternalSessionID, own.ExternalClient
	}
	if callerAgentProcess(ctx) != "" {
		return "", ""
	}
	ambient, ok := s.soleAmbientByCWD(ctx, host, cwd)
	if !ok {
		return "", ""
	}
	return ambient.ExternalSessionID, ambient.ExternalClient
}

// soleAmbientByCWD returns the single active ambient (cc/* or cx/*) session sharing cwd --
// the unambiguous this-agent case. Zero or many candidates (no ambient yet, or two
// agents in one cwd) report ok=false so callers fall back to a fresh session rather
// than risking a cross-agent match. Best-effort: a lookup error logs and reports no
// match.
func (s *Server) soleAmbientByCWD(ctx context.Context, host, cwd string) (core.Session, bool) {
	ambients, err := store.ActiveAmbientByCWD(ctx, s.cfg.DB, host, cwd)
	if err != nil {
		s.logger.Warn("session_start: ambient lookup", "error", err)
		return core.Session{}, false
	}
	// Another OS account's sessions are never candidates (peer_auth).
	ambients = slices.DeleteFunc(ambients, func(a core.Session) bool { return !access.Visible(ctx, a.ProjectSlug) })
	if len(ambients) != 1 {
		return core.Session{}, false
	}
	return ambients[0], true
}

// sessionArgDesc documents the `session` argument of session_update/session_end.
// The default is the connection's own session -- bound automatically for a
// Claude Code or Codex agent -- so the argument is the exception, and it says
// so: the old wording ("pass it whenever you have not run session_start") taught
// agents that session_start was a prerequisite.
const sessionArgDesc = "Session to operate on: a name (the cc/<id> or cx/<id> on your briefing's 'Seam session' line, " +
	"or a sess/* name) or a session ULID. Defaults to this connection's session -- bound automatically for Claude Code " +
	"and Codex agents -- so pass it only to act on another session, or when a call reports the session as ambiguous"

func sessionUpdateTool() mcp.Tool {
	return mcp.NewTool("session_update", hintSet(),
		mcp.WithDescription("Record interim progress on the current session (working findings so far). Uses the bound session unless you pass one."),
		mcp.WithString("findings", mcp.Required(), mcp.Description("Working findings / progress note so far")),
		mcp.WithString("session", mcp.Description(sessionArgDesc)),
		mcp.WithString("session_id", mcp.Description("Session ULID to operate on; takes precedence over session and the bound session")),
	)
}

func (s *Server) handleSessionUpdate(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	findings := argString(req, "findings")
	if findings == "" {
		return errResult("session_update", errors.New("findings is required"))
	}
	sess, ok, err := s.resolveSession(ctx, req)
	if err != nil {
		return errResult("session_update", err)
	}
	if !ok {
		return errResult("session_update", errNoSession)
	}
	// Findings are project knowledge, not just caller identity: they persist into
	// the session's project and its future briefings. So writing them into a
	// session resolved by an explicit session_id/session -- or inherited from an
	// ambient the connection never bound to -- is a write into THAT project, and
	// takes the same fence a note or a task does. Both directions bite: an outside
	// agent must not inject text into a sealed project's briefings, and an agent
	// inside a confidential project must not park its findings in another
	// project's session, which is the project=global leak wearing a session id.
	// favorite_set kind=session already fences on this same project, and
	// overwriting a session's findings is strictly more consequential than
	// starring it. resolveSession's precedence is untouched: the fence judges what
	// it resolved, it does not resolve anything itself.
	if err := s.fenceWrite(ctx, sess.ProjectSlug); err != nil {
		return errResult("session_update", err)
	}
	// The resolved session -- possibly an explicit session_id/session naming a
	// session this connection is not bound to -- is what this call operates on;
	// attribute its tool.call there, not to the binding/ambient guess.
	stashAttribution(ctx, sess.ID, sess.ProjectSlug)
	sess.Findings = findings
	sess.UpdatedAt = time.Now().UTC()
	if err := store.UpdateSession(ctx, s.cfg.DB, sess); err != nil {
		return errResult("session_update", err)
	}
	return jsonResult(map[string]any{"session_id": sess.ID, "status": string(sess.Status)})
}

func sessionEndTool() mcp.Tool {
	return mcp.NewTool("session_end", hintSet(),
		mcp.WithDescription("Complete the current session, persisting its findings for future briefings. Uses the bound session unless you pass one."),
		mcp.WithString("findings", mcp.Required(), mcp.Description("Final findings: what was learned, decided, or left open. Prefer a tight summary (briefings show a short preview), but long findings are stored in full -- they are not rejected.")),
		mcp.WithArray("mishaps", mcp.WithStringItems(), mcp.Description("Self-report mishaps this session caused: an action a warning or convention said not to take, live state touched by mistake, a command that hit the wrong target. Pass an array with one short entry per incident; omit when none happened. When a mishap violated a stored memory, name that memory by its exact slug in the entry (e.g. \"violated chroma-boot-race by ...\") -- the report is then linked to it. Recorded for recurrence review, not blame -- report them even when fully recovered.")),
		mcp.WithString("session", mcp.Description(sessionArgDesc)),
		mcp.WithString("session_id", mcp.Description("Session ULID to operate on; takes precedence over session and the bound session")),
	)
}

func (s *Server) handleSessionEnd(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	findings := argString(req, "findings")
	if findings == "" {
		return errResult("session_end", errors.New("findings is required and must not be empty"))
	}
	sess, ok, err := s.resolveSession(ctx, req)
	if err != nil {
		return errResult("session_end", err)
	}
	if !ok {
		return errResult("session_end", errNoSession)
	}
	// Fenced exactly like session_update, and for more: an end also completes the
	// session and releases every task claim it holds, so an unfenced one lets a
	// caller outside a sealed project reach into it and drop another agent's work
	// back on the queue. The fence runs before the attribution stash so a refused
	// call is attributed to the caller, not to the session it was refused.
	if err := s.fenceWrite(ctx, sess.ProjectSlug); err != nil {
		return errResult("session_end", err)
	}
	// Stash before completing: once the session flips to completed and its
	// bindings are evicted, neither the binding nor the active-only ambient
	// fallback can see it, and the tool.call (plus heartbeat) would land on a
	// surviving sibling session.
	stashAttribution(ctx, sess.ID, sess.ProjectSlug)
	now := time.Now().UTC()
	sess.Status = core.SessionCompleted
	sess.Findings = findings
	sess.UpdatedAt = now
	if err := store.UpdateSession(ctx, s.cfg.DB, sess); err != nil {
		return errResult("session_end", err)
	}
	// The session is over: drop every connection binding pointing at it, so the
	// bindings map does not grow one dead entry per ended session on a
	// long-lived daemon (sessions ended by the hook or the reaper are swept
	// separately -- see maybeSweepBindings).
	s.evictSessionBindings(sess.ID)
	// Release any task claims this session still holds so its in-flight work
	// returns to the queue rather than sitting claimed by a departed agent.
	// Keyed off the resolved sess.ID (not the connection binding) because
	// session_end may complete an ambient session this connection isn't bound to.
	released, err := store.ReleaseClaimsForSession(ctx, s.cfg.DB, sess.ID, now)
	if err != nil {
		return errResult("session_end", err)
	}
	// Self-reported mishaps land as one durable agent.mishap event each -- the
	// only record of an action no telemetry can observe (the agent confessing it
	// is the signal). Recorded before session.ended so the session's timeline
	// reads in order. Each text is scanned once, here at ingestion, for active
	// memories it names (mishapMemoryIDs); matches persist in the payload as
	// item_ids, so nothing ever re-matches against a corpus that has since
	// changed. The linkage feeds the briefing's mishap promotion
	// (store.RecentMishapItemIDs) and deliberately NOT the utility score.
	mishaps := argStrings(req, "mishaps")
	corpus := s.mishapMatchCorpus(ctx, sess.ProjectSlug, len(mishaps))
	for _, m := range mishaps {
		payload := map[string]any{"description": events.Truncate(m, s.cfg.ToolEventMaxChars)}
		if ids := mishapMemoryIDs(m, corpus); len(ids) > 0 {
			payload["item_ids"] = ids
		}
		s.record(ctx, core.EventAgentMishap, sess.ID, sess.ProjectSlug, "", payload)
	}
	s.record(ctx, core.EventSessionEnded, sess.ID, sess.ProjectSlug, "",
		map[string]any{"claims_released": released, "findings": events.Truncate(findings, s.cfg.ToolEventMaxChars)})
	return jsonResult(map[string]any{"status": "completed", "session_id": sess.ID, "claims_released": released, "mishaps_recorded": len(mishaps)})
}

// mishapMatchCorpus loads the active memories a mishap report may name: the
// session project's own plus global scope, the same visibility rule every other
// read uses -- another project's memory never matches. Skipped entirely when
// there are no mishaps. Best-effort: linkage must never fail a session_end, so
// a load error logs and the reports come through unlinked.
func (s *Server) mishapMatchCorpus(ctx context.Context, project string, mishapCount int) []core.Memory {
	if mishapCount == 0 {
		return nil
	}
	mems, err := store.ActiveMemories(ctx, s.cfg.DB, project)
	if err != nil {
		s.logger.Warn("session_end: load memories for mishap linkage", "error", err)
		return nil
	}
	return mems
}

// resolveSession loads the session the request targets: an explicit session_id
// (ULID) first, then a session name, then the connection's bound session, and
// only then a single unambiguous active ambient session. Accepting an id as well
// as a name stops a session_id= argument from being silently ignored and dropping
// to the fallback -- the call-site mistake behind an overwrite of the wrong
// agent's session. The fallback is stricter than for reads/writes: it refuses
// (errAmbiguousSession) whenever more than one active ambient session could be the
// one meant -- including two agents' ambients in the SAME repo -- because
// completing the wrong session is destructive, not merely mis-scoped.
func (s *Server) resolveSession(ctx context.Context, req mcp.CallToolRequest) (core.Session, bool, error) {
	if id := argString(req, "session_id"); id != "" {
		sess, ok, err := sessionByID(ctx, s.cfg.DB, id)
		if err == nil && !ok {
			err = fmt.Errorf("session_id %q not found", id)
		}
		return sess, ok, err
	}
	if ref := argString(req, "session"); ref != "" {
		sess, ok, err := sessionByRef(ctx, s.cfg.DB, ref)
		if err == nil && !ok {
			err = errSessionRefNotFound(ref)
		}
		return sess, ok, err
	}
	if b, ok := s.getBinding(ctx); ok {
		return sessionByID(ctx, s.cfg.DB, b.sessionID)
	}
	sess, ok, ambiguous, err := s.ambientSessionTarget(ctx)
	if err != nil {
		return core.Session{}, false, err
	}
	if ambiguous {
		return core.Session{}, false, errAmbiguousSession
	}
	return sess, ok, nil
}

// ambientSessionTarget resolves the single active ambient session an unbound
// session_update/end may target, or reports ambiguity. It is stricter than
// ambientFallback: that one collapses a project's ambients to the most recent for
// provenance, which is fine for stamping an event but not for *completing* a
// session. Here more than one candidate -- across projects, or two agents' cc/* or
// cx/* ambients in one project -- yields ambiguous=true and no session, so the caller
// must name the session. Exactly one candidate (the solo-agent case) resolves.
func (s *Server) ambientSessionTarget(ctx context.Context) (sess core.Session, ok bool, ambiguous bool, err error) {
	projects, scope, _, err := s.ambientProjectsNearestFirst(ctx)
	if err != nil {
		return core.Session{}, false, false, err
	}
	switch len(projects) {
	case 0:
		return core.Session{}, false, false, nil
	case 1:
		// Single project: still ambiguous if two agents left ambients in it.
	default:
		return core.Session{}, false, true, nil
	}
	sessions, err := store.ActiveAmbientSessionsForProject(ctx, s.cfg.DB, scope, projects[0], ambientFallbackWindow)
	if err != nil {
		return core.Session{}, false, false, err
	}
	if len(sessions) != 1 {
		return core.Session{}, false, len(sessions) > 1, nil
	}
	return sessions[0], true, false, nil
}

// sessionByRef resolves a session= argument: a session name, or -- when shaped
// like one -- a session ULID. Agents routinely pass the session_id a
// session_start returned as session=, and reading that only as a name turned a
// correct reference into "no active session: call session_start first", sending
// an agent that had just called it to call it again.
//
// Under peer_auth a session in a project this connection's OS account may not
// reach is reported as not found, exactly like a name that matches nothing: a
// handle is how a call adopts a session's project, so honoring one would be a
// way around every fence.
func sessionByRef(ctx context.Context, db *sql.DB, ref string) (core.Session, bool, error) {
	sess, ok, err := store.SessionByName(ctx, db, ref)
	if err == nil && !ok && store.LooksLikeSessionULID(ref) {
		sess, ok, err = store.SessionByID(ctx, db, ref)
	}
	if err != nil || !ok {
		return core.Session{}, false, err
	}
	if !access.Visible(ctx, sess.ProjectSlug) {
		return core.Session{}, false, nil
	}
	return sess, true, nil
}

// sessionByID is store.SessionByID under the same peer_auth rule as
// sessionByRef: a session this connection's OS account may not reach is not
// found.
func sessionByID(ctx context.Context, db *sql.DB, id string) (core.Session, bool, error) {
	sess, ok, err := store.SessionByID(ctx, db, id)
	if err != nil || !ok {
		return core.Session{}, false, err
	}
	if !access.Visible(ctx, sess.ProjectSlug) {
		return core.Session{}, false, nil
	}
	return sess, true, nil
}

// errSessionRefNotFound is the refusal for a session= that names nothing. It
// says what the argument takes, because the usual cause is passing the wrong
// kind of value rather than a wrong one.
func errSessionRefNotFound(ref string) error {
	return fmt.Errorf("session %q not found: session= takes a session name -- the cc/... or cx/... on your "+
		"briefing's 'Seam session' line, or a sess/* name -- or a session ULID", ref)
}

// withWarning attaches a non-fatal warning to a tool result. It is only ever
// added when something the caller can FIX went wrong (an unplaceable remote
// cwd); a result with no warning key is unchanged, so no existing caller sees a
// new field.
func withWarning(out map[string]any, warning string) map[string]any {
	if warning != "" {
		out["warning"] = warning
	}
	return out
}

// shortID returns the last 8 characters of a ULID, lowercased, for a readable
// generated session name.
func shortID(id string) string {
	if len(id) <= 8 {
		return strings.ToLower(id)
	}
	return strings.ToLower(id[len(id)-8:])
}
