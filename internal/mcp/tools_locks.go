package mcp

// Repo locks (plan repo-locks): the four tools an agent declares, lists, holds
// and releases a lock on a repository's refs with. A lock has no project
// (internal/core/locks.go), so these tools take none and pass through no
// project fence: what they must never do instead is echo another project's
// content, so an answer names a project only where the caller may list it
// (access.View.Listed) and a holder or a waiter only where the caller may read
// its session.
//
// Who may see and hold a lock is store.RepoLockReachable's one answer: the
// owner principal every lock, on any host and from any conversation (a lock's
// state carries no content, owner decision D2); a restricted account, local or
// a relay's, a lock one of whose listed projects it may write. A lock a caller
// may not reach reads exactly like a missing one: listed nowhere, not found by
// id or by checkout. A restricted reader sees no alias at all, and no reader
// sees the aliases of a lock a restricted account declared: an alias is a
// caller's free text.
//
// A declaration and an alias record write a caller's text into the registry
// every caller reads, so they meet the clearance rule a write into the global
// scope does (fenceWriteQuiet with the global target): an owner conversation
// that may not write the global scope -- bound to a confidential project, or
// confined by isolation.requests -- declares nothing and records no alias, and
// still lists, acquires, heartbeats and releases. A restricted account is
// never judged by clearance (its reach holds one fenced project alone, or open
// ones): its declaration waits for the owner, and it records no alias.
//
// A lock with waiters is taken first come, first served (store.
// AcquireRepoLockWaiting): locks_acquire with wait joins the queue, or keeps
// the caller's place, and locks_release from a waiting session leaves it.
//
// A lock is named by the checkout's git identity -- the arguments session_start
// takes (cwd, host, repo_root, main_worktree_root, repo_origin) -- or by its id.
// The daemon reads a checkout from its own disk only when the identity is its
// own machine's and no View rides the request (peer_auth off): that is
// CWDIdentity's rule, and the hooks' localDiskOK. `seam lock` always sends the
// identity it resolved on its own machine.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/gitread"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/validate"
)

// lkDefaultLease is how long a hold lasts before it lapses, unless the caller
// asks for another lease (lease_seconds), as tasks_claim's default.
const lkDefaultLease = 15 * time.Minute

// lkToolSpec pairs one lock tool's definition with its handler.
type lkToolSpec struct {
	tool   func() mcp.Tool
	handle func(*Server) mcpserver.ToolHandlerFunc
}

// lkTools are the lock tools, in registration order: the one place they are
// listed, so registerTools and Catalog cannot disagree and a gate wraps them
// in one place.
var lkTools = []lkToolSpec{
	{lkListTool, func(s *Server) mcpserver.ToolHandlerFunc { return s.lkHandleList }},
	{lkDeclareTool, func(s *Server) mcpserver.ToolHandlerFunc { return s.lkHandleDeclare }},
	{lkAcquireTool, func(s *Server) mcpserver.ToolHandlerFunc { return s.lkHandleAcquire }},
	{lkReleaseTool, func(s *Server) mcpserver.ToolHandlerFunc { return s.lkHandleRelease }},
}

// lkCatalog is the lock tools' definitions, for Catalog.
func lkCatalog() []mcp.Tool {
	out := make([]mcp.Tool, 0, len(lkTools))
	for _, t := range lkTools {
		out = append(out, t.tool())
	}
	return out
}

// lkRegister registers the lock tools, for registerTools.
func (s *Server) lkRegister() {
	for _, t := range lkTools {
		s.addTool(t.tool(), t.handle(s))
	}
}

// lkIdentityOpts declares the checkout a lock tool names, as session_start
// declares it.
func lkIdentityOpts() []mcp.ToolOption {
	return []mcp.ToolOption{
		mcp.WithString("cwd", mcp.Description("Absolute working directory inside the repository's checkout. The daemon "+
			"reads the checkout's git identity from it only on its own machine with peer_auth off; otherwise pass repo_root "+
			"(and main_worktree_root, repo_origin) resolved on your machine. `seam lock` sends all of them")),
		mcp.WithString("host", mcp.Description("Machine the checkout is on (hostname). Defaults to the X-Seamless-Host "+
			"header, then to the daemon's own host")),
		mcp.WithString("repo_root", mcp.Description("Absolute root of the checkout, resolved on its own machine")),
		mcp.WithString("main_worktree_root", mcp.Description("Absolute root of the repository's MAIN checkout when "+
			"repo_root is a linked worktree; defaults to repo_root")),
		mcp.WithString("repo_origin", mcp.Description("The repository's origin remote URL: the key a lock is found by "+
			"from every machine, so the ssh and https forms of one repository are one lock. A side clone whose origin is a "+
			"local path is the repository at that path")),
	}
}

// lkNameOpt declares the lock's name.
func lkNameOpt() mcp.ToolOption {
	return mcp.WithString("name", mcp.Description("The lock's name on its repository, a token: main (the default), "+
		"release. A line naming a lock shows its repository and its name: LOCK seamless main"))
}

func lkListTool() mcp.Tool {
	opts := []mcp.ToolOption{hintRead(),
		mcp.WithDescription("List repo locks: each lock's repository and name, the refs it guards, whether it is held, " +
			"by which session (when you may see that session), since and until when, whether you hold it, how many " +
			"sessions wait for it and which is next, the repository identities that resolve to it, and the projects " +
			"whose restricted accounts the declaration lists. Pass a checkout's identity (cwd, or repo_root and " +
			"repo_origin) to list only that repository's locks. A declaration waiting for the owner's approval is listed " +
			"with state requested. Read-only."),
	}
	return mcp.NewTool("locks_list", append(opts, lkIdentityOpts()...)...)
}

func lkDeclareTool() mcp.Tool {
	opts := []mcp.ToolOption{hintSet(),
		mcp.WithDescription("Declare a lock on a repository's refs, once per repository and name: the lock agents hold " +
			"while they land changes, so landings happen one at a time across machines. Name the repository by its " +
			"checkout (cwd, or repo_root, main_worktree_root and repo_origin): its origin URL is the key, so the ssh and " +
			"https forms of one repository are one lock, and a repository with no origin is locked by host and path. " +
			"Declaring again, from any project or machine, returns the existing lock unchanged and records this " +
			"checkout's identity on it, so a repository that gains or changes its origin keeps its lock. A restricted " +
			"account's declaration waits for the owner's approval on the console. refs defaults to refs/heads/<name>; " +
			"projects, the projects whose restricted accounts may hold the lock, defaults to the project the checkout " +
			"maps to."),
		lkNameOpt(),
		mcp.WithArray("refs", mcp.WithStringItems(), mcp.Description("The guarded refs: full ref names (refs/heads/main) "+
			"or patterns with one '*' (refs/tags/v*), at most 16 (a comma-separated string is also accepted). "+
			"Defaults to refs/heads/<name>")),
		mcp.WithArray("projects", mcp.WithStringItems(), mcp.Description("Project slugs whose restricted accounts may "+
			"hold the lock (a comma-separated string is also accepted). Defaults to the project the checkout maps to")),
		mcp.WithString("session", mcp.Description(actorSessionArgDesc)),
		mcp.WithString("session_id", mcp.Description(actorSessionIDArgDesc)),
	}
	return mcp.NewTool("locks_declare", append(opts, lkIdentityOpts()...)...)
}

func lkAcquireTool() mcp.Tool {
	opts := []mcp.ToolOption{hintAdd(),
		mcp.WithDescription("Take a repo lock for the current session, with a lease, before landing changes on the refs " +
			"it guards. Re-acquiring a lock you hold refreshes the lease: the heartbeat for a long landing. A lock another " +
			"session holds under a live lease is refused, naming the holder when you may see that session, since and " +
			"until; a lapsed lease is taken over. Sessions waiting for a lock take it first come, first served: while any " +
			"wait, only the next one may take it, and anyone else is refused with the next waiter and their own place. " +
			"With wait, a refusal joins the queue instead (or keeps your place) and answers queued, with your position; " +
			"ask again within two minutes to keep the place, or run `seam lock acquire --wait` in the background, which " +
			"asks every few seconds and returns once you hold the lock. A declaration still waiting for the owner cannot " +
			"be held. Name the lock by id, or by the checkout and name (default main). Release it with locks_release once " +
			"the refs have landed; a hold also lapses at the end of its lease."),
		mcp.WithString("id", mcp.Description("lock id (from locks_list or locks_declare); or name the checkout instead")),
		lkNameOpt(),
		mcp.WithBoolean("wait", mcp.Description("join the lock's queue, or keep your place in it, when the lock cannot be "+
			"yours now; the answer is queued with your position instead of a refusal (default false)")),
		// No Min/Max, as tasks_claim: the handler owns the range, whose upper end
		// is a task claim's ceiling too (core.MaxClaimLease).
		mcp.WithNumber("lease_seconds", mcp.Description(fmt.Sprintf("lease duration in seconds before the hold lapses (default 900, at most %d)",
			maxClaimLeaseSeconds))),
		mcp.WithString("session", mcp.Description(actorSessionArgDesc)),
		mcp.WithString("session_id", mcp.Description(actorSessionIDArgDesc)),
	}
	return mcp.NewTool("locks_acquire", append(opts, lkIdentityOpts()...)...)
}

func lkReleaseTool() mcp.Tool {
	opts := []mcp.ToolOption{hintSet(),
		mcp.WithDescription("Release a repo lock the current session holds, live or lapsed, so another session can take " +
			"it: the next waiter, when sessions wait for it. A session that waits for the lock and does not hold it " +
			"leaves the queue instead. Only the holder may release; the owner can force-release any hold from the " +
			"console. Name the lock by id, or by the checkout and name (default main)."),
		mcp.WithString("id", mcp.Description("lock id (from locks_list or locks_acquire); or name the checkout instead")),
		lkNameOpt(),
		mcp.WithString("session", mcp.Description(actorSessionArgDesc)),
		mcp.WithString("session_id", mcp.Description(actorSessionIDArgDesc)),
	}
	return mcp.NewTool("locks_release", append(opts, lkIdentityOpts()...)...)
}

// lkLocalDisk reports whether the daemon may read a checkout named on host from
// its own disk: only its own machine's, and only with no View on the request
// (under peer_auth the daemon runs as its own account, so every path a caller
// names was chosen by another account).
func (s *Server) lkLocalDisk(ctx context.Context, host string) bool {
	if _, peerAuth := access.FromContext(ctx); peerAuth {
		return false
	}
	return host == "" || strings.EqualFold(host, s.cfg.LocalHost)
}

// lkReadRepo reports the main checkout root and the origin of the repository
// at path, or "" and "" when path is no checkout (a bare repository, or
// nothing): core.FollowLocalOrigin's reader, on this machine's disk.
func lkReadRepo(path string) (string, string) {
	if _, err := os.Lstat(filepath.Join(path, ".git")); err != nil {
		return "", ""
	}
	main := gitread.MainWorktreeRoot(path)
	return main, gitread.OriginURL(main)
}

// lkIdentity reads the checkout a call names, filling the git identity from
// the daemon's disk only where lkLocalDisk allows. named is false when the call
// names no checkout at all.
func (s *Server) lkIdentity(ctx context.Context, req mcp.CallToolRequest) (id store.CWDIdentity, named bool, err error) {
	id = store.CWDIdentity{
		Host: strings.ToLower(argString(req, "host")), CWD: argString(req, "cwd"),
		RepoRoot: argString(req, "repo_root"), MainRoot: argString(req, "main_worktree_root"), Origin: argString(req, "repo_origin"),
	}
	named = id.CWD != "" || id.RepoRoot != "" || id.MainRoot != "" || id.Origin != ""
	if !named {
		return id, false, nil
	}
	if id.CWD != "" && !store.IsAbsRepoPath(id.CWD) {
		return id, true, fmt.Errorf("cwd %q is not an absolute path", id.CWD)
	}
	if id.Host == "" {
		id.Host = s.callerHost(ctx)
	}
	if !s.lkLocalDisk(ctx, id.Host) {
		return id, true, nil
	}
	if id.RepoRoot == "" && id.MainRoot == "" && id.CWD != "" {
		if root := gitread.RepoRoot(id.CWD); root != "" {
			id.RepoRoot = root
			id.MainRoot = gitread.MainWorktreeRoot(root)
			if id.Origin == "" {
				id.Origin = gitread.OriginURL(id.MainRoot)
			}
		}
	}
	base := id.MainRoot
	if base == "" {
		base = id.RepoRoot
	}
	id.Origin = core.FollowLocalOrigin(id.Origin, base, lkReadRepo)
	return id, true, nil
}

// errLkNoCheckout is a hold or a release that names no lock.
var errLkNoCheckout = errors.New("name the lock: id=<lock id>, or the repository's checkout with cwd (or repo_root and " +
	"repo_origin) and the lock's name")

// lkName reads the lock's name, main when absent.
func lkName(req mcp.CallToolRequest) (string, error) {
	name := argString(req, "name")
	if name == "" {
		name = core.DefaultRepoLockName
	}
	return name, core.ValidRepoLockName(name)
}

// lkAliases derives the aliases of the checkout a call names.
func (s *Server) lkAliases(ctx context.Context, req mcp.CallToolRequest) ([]core.RepoLockAlias, store.CWDIdentity, bool, error) {
	id, named, err := s.lkIdentity(ctx, req)
	if err != nil || !named {
		return nil, id, named, err
	}
	aliases, err := store.RepoLockAliasesFor(id, s.cfg.LocalHost)
	return aliases, id, true, err
}

// lkTarget resolves the lock a hold or a release names: by id, or by the
// checkout and the name. It returns the checkout's aliases too (nil for an id).
func (s *Server) lkTarget(ctx context.Context, req mcp.CallToolRequest) (core.RepoLock, []core.RepoLockAlias, error) {
	byID := argString(req, "id")
	aliases, _, named, err := s.lkAliases(ctx, req)
	if byID != "" {
		if named || argString(req, "name") != "" {
			return core.RepoLock{}, nil, errors.New("pass id, or the checkout and the lock's name, not both")
		}
		l, ok, err := store.RepoLockByID(ctx, s.cfg.DB, byID)
		if err != nil {
			return core.RepoLock{}, nil, err
		}
		// A lock the caller may not reach is no lock at all: the same words as
		// an id that names none.
		if !ok || !store.RepoLockReachable(ctx, l) {
			return core.RepoLock{}, nil, fmt.Errorf("%w: no lock with id %q", store.ErrRepoLockNotFound, byID)
		}
		return l, nil, nil
	}
	if err != nil {
		return core.RepoLock{}, nil, err
	}
	if !named {
		return core.RepoLock{}, nil, errLkNoCheckout
	}
	name, err := lkName(req)
	if err != nil {
		return core.RepoLock{}, nil, err
	}
	l, ok, err := store.ResolveRepoLock(ctx, s.cfg.DB, aliases, name)
	if err != nil {
		return core.RepoLock{}, nil, err
	}
	if !ok || !store.RepoLockReachable(ctx, l) {
		return core.RepoLock{}, nil, fmt.Errorf("%w: no lock named %s is declared for this repository (%s): declare it "+
			"with locks_declare, or `seam lock declare`", store.ErrRepoLockNotFound, name, aliases[0].Alias)
	}
	return l, aliases, nil
}

// lkSeer judges, for one call, which projects an answer may name and which
// holders' sessions it may name, memoized.
type lkSeer struct {
	s        *Server
	ctx      context.Context
	caller   string
	scoped   bool
	readable map[string]bool
	sessions map[string]lkSession
	// bound is the session this connection is bound to (lkBound), read once.
	bound      string
	boundKnown bool
}

// lkSession is a holder as an answer may name it: shown is false when the
// caller may not read that session.
type lkSession struct {
	shown bool
	name  string
}

func (s *Server) lkSeer(ctx context.Context) *lkSeer {
	return &lkSeer{s: s, ctx: ctx, readable: map[string]bool{}, sessions: map[string]lkSession{}}
}

// listed reports whether the answer may name project: what project_list would
// show this caller (a project it reaches, or the owner's locked one, marked).
func (v *lkSeer) listed(project string) bool {
	view, _ := access.FromContext(v.ctx)
	return view.Listed(project)
}

// session resolves a holder's session id for the answer: its name, and only
// when the caller may read that session -- its account's own (a session
// belongs to the principal that started it) in a project the caller may read
// across every fence (store.CanRead from the caller's scope).
func (v *lkSeer) session(id string) (lkSession, error) {
	if got, ok := v.sessions[id]; ok {
		return got, nil
	}
	sess, ok, err := store.SessionByID(v.ctx, v.s.cfg.DB, id)
	if err != nil {
		return lkSession{}, err
	}
	out := lkSession{}
	if ok && access.SessionAccountOK(v.ctx, sess.Account) {
		readable, seen := v.readable[sess.ProjectSlug]
		if !seen {
			if !v.scoped {
				v.caller, v.scoped = v.s.callerScope(v.ctx), true
			}
			if readable, err = store.CanRead(v.ctx, v.s.cfg.DB, v.caller, sess.ProjectSlug); err != nil {
				return lkSession{}, err
			}
			v.readable[sess.ProjectSlug] = readable
		}
		if readable {
			out = lkSession{shown: true, name: sess.Name}
		}
	}
	v.sessions[id] = out
	return out, nil
}

// boundSession is the session this connection is bound to (lkBound),
// memoized for the call; "" when it is bound to none.
func (v *lkSeer) boundSession() string {
	if !v.boundKnown {
		v.bound, _ = v.s.lkBound(v.ctx)
		v.boundKnown = true
	}
	return v.bound
}

// lkShowsAliases reports whether an answer to the caller in ctx may show l's
// aliases: never to a restricted reader, and to nobody for a lock a restricted
// account declared. An alias is the declaring caller's free text (an origin
// URL, a host and a path), and a restricted account whose reach is one fenced
// project would otherwise write it where every owner agent reads it.
func lkShowsAliases(ctx context.Context, l core.RepoLock) bool {
	return access.AccountOf(ctx) == "" && l.Account == ""
}

// lkJSON renders a lock for an answer: no free text, a project only where the
// caller may list it, a holder or the next waiter only where it may read that
// session, and aliases only where lkShowsAliases allows.
func (v *lkSeer) lkJSON(l core.RepoLock, now time.Time) (map[string]any, error) {
	aliases := make([]map[string]any, 0, len(l.Aliases))
	if lkShowsAliases(v.ctx, l) {
		for _, a := range l.Aliases {
			aliases = append(aliases, map[string]any{"kind": string(a.Kind), "alias": a.Alias})
		}
	}
	projects := make([]string, 0, len(l.Projects))
	for _, p := range l.Projects {
		if v.listed(p) {
			projects = append(projects, p)
		}
	}
	refs := l.Refs
	if refs == nil {
		refs = []string{}
	}
	held := l.HoldLive(now)
	j := map[string]any{
		"id": l.ID, "label": l.Label(), "repo": l.Repo(), "name": l.Name, "refs": refs, "state": string(l.State),
		"held": held, "aliases": aliases, "projects": projects,
		// held_by_caller is this connection's own bound session (lkBound),
		// never an inference: a caller bound to no session reads false.
		"held_by_caller": held && l.Holder != "" && l.Holder == v.boundSession(),
		"waiting":        len(l.Waiters),
	}
	if next := l.NextWaiter(); next != "" {
		who, err := v.session(next)
		if err != nil {
			return nil, err
		}
		if who.shown {
			j["next_waiter_session"] = who.name
		}
	}
	if l.Holder != "" {
		if !l.HoldLive(now) {
			j["lapsed"] = true
		}
		if l.HeldSince != nil {
			j["since"] = core.FormatTime(*l.HeldSince)
		}
		if l.LeaseExpiresAt != nil {
			j["until"] = core.FormatTime(*l.LeaseExpiresAt)
		}
		who, err := v.session(l.Holder)
		if err != nil {
			return nil, err
		}
		if who.shown {
			j["holder"] = l.Holder
			j["holder_session"] = who.name
		}
	}
	return j, nil
}

// sessionWords names a session for a sentence: "session <name>" where the
// caller may read it, else "another session".
func (v *lkSeer) sessionWords(id string) string {
	if who, err := v.session(id); err == nil && who.shown {
		return "session " + who.name
	}
	return "another session"
}

// lkPlaceWords is the caller's own place in a lock's queue, for a refusal.
func lkPlaceWords(position int) string {
	if position > 0 {
		return fmt.Sprintf("you are number %d in its queue (acquire with wait, or `seam lock acquire --wait`, to keep "+
			"your place)", position)
	}
	return "you are not in its queue: acquire with wait, or run `seam lock acquire --wait`, to join it"
}

// lkHeldRefusal is an acquire refused at another session's live hold, in words
// naming the holder only where the caller may read its session.
func (v *lkSeer) lkHeldRefusal(l core.RepoLock, position int) error {
	when := ""
	if l.HeldSince != nil && l.LeaseExpiresAt != nil {
		when = fmt.Sprintf(" since %s, until %s", core.FormatTime(*l.HeldSince), core.FormatTime(*l.LeaseExpiresAt))
	}
	queue := ""
	if len(l.Waiters) > 0 {
		queue = fmt.Sprintf("; %d %s for it and %s", len(l.Waiters), lkSessionsWait(len(l.Waiters)), lkPlaceWords(position))
	}
	return fmt.Errorf("%w: LOCK %s is held by %s%s; wait for its release or for the lease to lapse%s",
		store.ErrRepoLockHeld, l.Label(), v.sessionWords(l.Holder), when, queue)
}

// lkQueuedRefusal is an acquire refused because the lock's queue puts another
// session first, naming the next waiter only where the caller may read its
// session, and the caller's own place.
func (v *lkSeer) lkQueuedRefusal(l core.RepoLock, position int) error {
	return fmt.Errorf("%w: LOCK %s is queued: %d %s for it, %s is next, and %s",
		store.ErrRepoLockQueued, l.Label(), len(l.Waiters), lkSessionsWait(len(l.Waiters)), v.sessionWords(l.NextWaiter()),
		lkPlaceWords(position))
}

// lkSessionsWait is "session waits" or "sessions wait" for n.
func lkSessionsWait(n int) string {
	if n == 1 {
		return "session waits"
	}
	return "sessions wait"
}

// lkBound is the session this connection is bound to, by the session= handle
// a call names, a session_start on this connection, or the agent-process
// identity its requests carry (resolveBinding): never the sole-active-ambient
// fallback resolveActor has, and never a (host, cwd) guess. A binding to a
// session in a project locked to the caller counts (lockedBinding): the lock
// tools act through it (lkActor). It must name an active session.
func (s *Server) lkBound(ctx context.Context) (string, bool) {
	b, ok := s.resolveBinding(ctx)
	if !ok || b.sessionID == "" {
		return "", false
	}
	sess, found, err := store.SessionByID(ctx, s.cfg.DB, b.sessionID)
	if err != nil {
		s.logger.Warn("locks: bound session lookup", "session_id", b.sessionID, "error", err)
		return "", false
	}
	if !found || sess.Status != core.SessionActive {
		return "", false
	}
	return sess.ID, true
}

// lkActor is the session a hold, a heartbeat, a place in the queue or a
// release acts as: resolveActor's, except that a binding to the caller's own
// session in a project locked to it is that session rather than a refusal at
// the lock. A lock's state carries no content (owner decision D2), so holding
// one reads and writes nothing of the locked project, and files no request
// for it. A session the call names (session=, session_id=) is resolveActor's
// to judge, locked or not.
func (s *Server) lkActor(ctx context.Context, req mcp.CallToolRequest) (string, bool, error) {
	if argString(req, "session_id") == "" && argString(req, "session") == "" {
		if _, ok := s.getBinding(ctx); !ok {
			if b, ok := s.lockedBinding(ctx); ok {
				return b.sessionID, true, nil
			}
		}
	}
	return s.resolveActor(ctx, req)
}

// lkRegistryRefusal is the refusal for a call that would write the caller's
// own text into the lock registry -- a declaration, or an alias record -- that
// the conversation may not write into the global scope: the fence's own
// refusal, nil when it may. A restricted account is judged by its reach
// instead (lkProjects, and its declaration waits for the owner): clearance
// never judges one.
func (s *Server) lkRegistryRefusal(ctx context.Context) error {
	if access.AccountOf(ctx) != "" {
		return nil
	}
	return s.fenceWriteQuiet(ctx, "")
}

// lkEvent records a lock change, with the restricted account that acted (none
// for the owner principal).
func (s *Server) lkEvent(ctx context.Context, sessionID, lockID string, payload map[string]any) {
	if account := access.AccountOf(ctx); account != "" {
		payload["account"] = account
	}
	s.record(ctx, core.EventRepoLockChanged, sessionID, "", lockID, payload)
}

func (s *Server) lkHandleList(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	aliases, _, named, err := s.lkAliases(ctx, req)
	if err != nil {
		return errResult("locks_list", err)
	}
	var locks []core.RepoLock
	if named {
		locks, err = store.RepoLocksFor(ctx, s.cfg.DB, aliases)
	} else {
		locks, err = store.ListRepoLocks(ctx, s.cfg.DB)
	}
	if err != nil {
		return errResult("locks_list", err)
	}
	now := time.Now().UTC()
	seer := s.lkSeer(ctx)
	out := make([]map[string]any, 0, len(locks))
	for _, l := range locks {
		// A lock the caller may not reach is not listed at all: a missing one.
		if !store.RepoLockReachable(ctx, l) {
			continue
		}
		j, err := seer.lkJSON(l, now)
		if err != nil {
			return errResult("locks_list", err)
		}
		out = append(out, j)
	}
	// caller_bound says whether this connection is bound to a session at all
	// (lkBound), so a client tells "not held by me" from "nobody to be".
	return jsonResult(map[string]any{"locks": out, "caller_bound": seer.boundSession() != ""})
}

// lkDeclarer is the session a declaration records as its declarer: one the
// call names (session_id, session) or the connection is bound to, never one
// inferred from other agents' activity; "" when there is none. A binding that
// is the caller's session in a project locked to it acts as nothing, as in
// resolveActor: refused at the lock.
func (s *Server) lkDeclarer(ctx context.Context, req mcp.CallToolRequest) (string, error) {
	if argString(req, "session_id") != "" || argString(req, "session") != "" {
		id, _, err := s.resolveActor(ctx, req)
		return id, err
	}
	if b, ok := s.getBinding(ctx); ok {
		return b.sessionID, nil
	}
	if b, ok := s.lockedBinding(ctx); ok {
		return "", s.lockedScopeRefusal(ctx, b.project)
	}
	return "", nil
}

// lkProjects reads the projects a declaration lists: the named ones, or the
// project the checkout maps to. A named one must exist and be writable by the
// caller; a restricted account is refused a project it may not reach in the
// one text that never tells an unreachable project from a missing one. The
// mapped project is the repo map's own answer, never the caller's words: it is
// listed only where the caller may write it, and otherwise nothing is -- a
// refusal there would name a project the caller never named, and tell a
// mapped path from an unmapped one. It is listed even where the map predates
// its registry row.
func (s *Server) lkProjects(ctx context.Context, req mcp.CallToolRequest, id store.CWDIdentity) ([]string, error) {
	named := argStrings(req, "projects")
	if len(named) == 0 {
		where := id.CWD
		if where == "" {
			if where = id.MainRoot; where == "" {
				where = id.RepoRoot
			}
		}
		if where == "" {
			return nil, nil
		}
		project, err := store.ResolveProjectForCWD(ctx, s.cfg.DB, id.Host, where)
		if err != nil || project == "" || !access.Writable(ctx, project) {
			return nil, err
		}
		return []string{project}, nil
	}
	out := make([]string, 0, len(named))
	for _, raw := range named {
		slug, err := validate.Project(raw)
		if err != nil {
			return nil, err
		}
		if slug == "" {
			return nil, fmt.Errorf("projects lists projects whose restricted accounts may hold the lock: %q is not one", raw)
		}
		if !access.Writable(ctx, slug) {
			return nil, access.Refusal(ctx, slug)
		}
		_, ok, err := store.ProjectBySlug(ctx, s.cfg.DB, slug)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("no project %q: projects names existing projects (project_list)", slug)
		}
		if !slices.Contains(out, slug) {
			out = append(out, slug)
		}
	}
	return out, nil
}

func (s *Server) lkHandleDeclare(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name, err := lkName(req)
	if err != nil {
		return errResult("locks_declare", err)
	}
	aliases, id, named, err := s.lkAliases(ctx, req)
	if err != nil {
		return errResult("locks_declare", err)
	}
	if !named {
		return errResult("locks_declare", errors.New("name the repository: cwd inside its checkout, or repo_root "+
			"(and main_worktree_root, repo_origin) resolved on its own machine"))
	}
	projects, err := s.lkProjects(ctx, req, id)
	if err != nil {
		return errResult("locks_declare", err)
	}
	declarer, err := s.lkDeclarer(ctx, req)
	if err != nil {
		return errResult("locks_declare", err)
	}
	// A declaration writes the caller's text (its refs and aliases) where every
	// caller reads it: the clearance rule of a write into the global scope.
	if err := s.lkRegistryRefusal(ctx); err != nil {
		s.noteRefusal(ctx, err, "")
		return errResult("locks_declare", err)
	}
	// The owner principal declares directly; a restricted account's
	// declaration waits for the owner, and records no alias on a lock it
	// resolves to (judged per request, from the transport's View).
	owner := access.OwnerOnly(ctx)
	state := core.RepoLockActive
	if !owner {
		state = core.RepoLockRequested
		// A lock reaches a restricted account only through a listed project
		// it may write: a declaration listing none would be a lock its own
		// declarer could not see. Unmapped and mapped-elsewhere read alike.
		if len(projects) == 0 {
			return errResult("locks_declare", errLkRestrictedDeclaration(ctx))
		}
	}
	res, err := store.DeclareRepoLock(ctx, s.cfg.DB, store.RepoLockDeclaration{
		Name: name, Refs: argStrings(req, "refs"), Aliases: aliases, Projects: projects, State: state,
		RecordAliases: owner, CreatedBy: declarer, Account: access.AccountOf(ctx),
	}, time.Now().UTC())
	if errors.Is(err, store.ErrRepoLockRequested) {
		return errResult("locks_declare", fmt.Errorf("%w: LOCK %s is a restricted account's declaration; the owner "+
			"approves or discards it on the console's Locks page", store.ErrRepoLockRequested, res.Lock.Label()))
	}
	if err != nil {
		return errResult("locks_declare", err)
	}
	// A declaration that resolved to a lock the caller may not reach changed
	// nothing (a restricted account records no alias), and must not confirm
	// that lock exists: the same words as a declaration that lists no project.
	if !store.RepoLockReachable(ctx, res.Lock) {
		return errResult("locks_declare", errLkRestrictedDeclaration(ctx))
	}
	action := ""
	switch {
	case res.Created && state == core.RepoLockActive:
		action = core.RepoLockActionDeclared
	case res.Created:
		action = core.RepoLockActionRequested
	case len(res.Added) > 0:
		action = core.RepoLockActionAliased
	}
	if action != "" {
		s.lkEvent(ctx, declarer, res.Lock.ID, map[string]any{"action": action, "name": res.Lock.Name, "aliases": len(res.Added)})
	}
	j, err := s.lkSeer(ctx).lkJSON(res.Lock, time.Now().UTC())
	if err != nil {
		return errResult("locks_declare", err)
	}
	recorded := make([]map[string]any, 0, len(res.Added))
	if lkShowsAliases(ctx, res.Lock) {
		for _, a := range res.Added {
			recorded = append(recorded, map[string]any{"kind": string(a.Kind), "alias": a.Alias})
		}
	}
	out := map[string]any{"lock": j, "declared": res.Created, "recorded_aliases": recorded}
	switch {
	case res.Lock.State == core.RepoLockRequested:
		out["note"] = "this declaration waits for the owner, who approves or discards it on the console's Locks page; " +
			"nobody can hold it until then"
	case !res.Created:
		out["note"] = "this repository's lock was already declared: its refs and projects are unchanged"
	}
	return jsonResult(out)
}

// lkLease reads lease_seconds, lkDefaultLease when absent. A lease past the
// ceiling a task claim has too (core.MaxClaimLease) is refused with the
// ceiling named, as tasks_claim refuses one; the store refuses it again.
func lkLease(req mcp.CallToolRequest) (time.Duration, error) {
	if !argPresent(req, "lease_seconds") {
		return lkDefaultLease, nil
	}
	secs := argInt(req, "lease_seconds", 0)
	if secs <= 0 {
		return 0, fmt.Errorf("invalid lease_seconds %d", secs)
	}
	if int64(secs) > maxClaimLeaseSeconds {
		return 0, fmt.Errorf("invalid lease_seconds %d: a lock's lease is at most %s; to hold the lock longer, acquire it again (locks_acquire) before the lease lapses",
			secs, claimLeaseCeiling())
	}
	return time.Duration(secs) * time.Second, nil
}

func (s *Server) lkHandleAcquire(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	target, aliases, err := s.lkTarget(ctx, req)
	if err != nil {
		return errResult("locks_acquire", err)
	}
	lease, err := lkLease(req)
	if err != nil {
		return errResult("locks_acquire", err)
	}
	wait := argBool(req, "wait", false)
	sessionID, ok, err := s.lkActor(ctx, req)
	if err != nil {
		return errResult("locks_acquire", err)
	}
	if !ok {
		return errResult("locks_acquire", errors.New("no active session to hold the lock as: start a session "+
			"(session_start), or name yourself with session=<cc/... or cx/... or ULID>"))
	}
	now := time.Now().UTC()
	if wait {
		// Asking for the lock is proof the session is alive, and a place counts
		// only for a live session: the heartbeat every call gets, made before
		// the queue reads it rather than after.
		if err := store.TouchSession(ctx, s.cfg.DB, sessionID, now); err != nil {
			s.logger.Warn("locks_acquire: session heartbeat", "session_id", sessionID, "error", err)
		}
	}
	seer := s.lkSeer(ctx)
	got, err := store.AcquireRepoLockWaiting(ctx, s.cfg.DB, target.ID, sessionID, lease, now, wait)
	switch {
	case errors.Is(err, store.ErrRepoLockHeld) && got.Lock.Holder != "":
		return errResult("locks_acquire", seer.lkHeldRefusal(got.Lock, got.Position))
	case errors.Is(err, store.ErrRepoLockQueued):
		return errResult("locks_acquire", seer.lkQueuedRefusal(got.Lock, got.Position))
	case errors.Is(err, store.ErrRepoLockRequested):
		return errResult("locks_acquire", fmt.Errorf("%w: LOCK %s is a declaration the owner has not approved yet",
			store.ErrRepoLockRequested, target.Label()))
	case err != nil:
		return errResult("locks_acquire", err)
	}
	if got.Queued {
		return s.lkQueuedAnswer(ctx, seer, target, got, sessionID, now)
	}
	// Every checkout seen is recorded on the lock it holds, so it keeps the
	// lock when its origin changes -- the owner principal's checkouts only, as
	// with a declaration, and only from a conversation that may write the
	// global scope: an alias is the caller's text.
	if len(aliases) > 0 && access.OwnerOnly(ctx) && s.lkRegistryRefusal(ctx) == nil {
		added, aerr := store.RecordRepoLockAliases(ctx, s.cfg.DB, got.Lock.ID, aliases, now)
		if aerr != nil {
			s.logger.Warn("locks_acquire: record aliases", "lock", got.Lock.ID, "error", aerr)
		} else if len(added) > 0 {
			got.Lock.Aliases = append(got.Lock.Aliases, added...)
			s.lkEvent(ctx, sessionID, got.Lock.ID,
				map[string]any{"action": core.RepoLockActionAliased, "name": got.Lock.Name, "aliases": len(added)})
		}
	}
	action := core.RepoLockActionAcquired
	if got.Heartbeat {
		action = core.RepoLockActionHeartbeat
	}
	payload := map[string]any{"action": action, "name": got.Lock.Name, "lease_seconds": int(lease / time.Second)}
	if got.Reclaimed {
		payload["reclaimed"] = true
	}
	s.lkEvent(ctx, sessionID, got.Lock.ID, payload)
	j, err := seer.lkJSON(got.Lock, now)
	if err != nil {
		return errResult("locks_acquire", err)
	}
	out := map[string]any{"lock": j, "acquired": true, "heartbeat": got.Heartbeat, "reclaimed": got.Reclaimed}
	if got.Reclaimed {
		if who, err := seer.session(got.PriorHolder); err == nil && who.shown {
			out["prior_holder"] = got.PriorHolder
			out["prior_holder_session"] = who.name
		}
	}
	return jsonResult(out)
}

// lkQueuedAnswer is an acquire with wait that did not take the lock: the
// session holds a place in its queue, at position, until place_until unless it
// asks again. Joining is an event; keeping a place is not (a waiter asks every
// few seconds).
func (s *Server) lkQueuedAnswer(ctx context.Context, seer *lkSeer, target core.RepoLock, got store.RepoLockAcquired, sessionID string,
	now time.Time) (*mcp.CallToolResult, error) {
	if !slices.Contains(target.Waiters, sessionID) {
		s.lkEvent(ctx, sessionID, got.Lock.ID,
			map[string]any{"action": core.RepoLockActionQueued, "name": got.Lock.Name, "position": got.Position})
	}
	j, err := seer.lkJSON(got.Lock, now)
	if err != nil {
		return errResult("locks_acquire", err)
	}
	out := map[string]any{"lock": j, "acquired": false, "queued": true, "position": got.Position}
	if got.PlaceUntil != nil {
		out["place_until"] = core.FormatTime(*got.PlaceUntil)
	}
	return jsonResult(out)
}

func (s *Server) lkHandleRelease(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	target, _, err := s.lkTarget(ctx, req)
	if err != nil {
		return errResult("locks_release", err)
	}
	sessionID, ok, err := s.lkActor(ctx, req)
	if err != nil {
		return errResult("locks_release", err)
	}
	if !ok {
		return errResult("locks_release", errors.New("no active session; nothing to release"))
	}
	now := time.Now().UTC()
	freed, err := store.ReleaseRepoLock(ctx, s.cfg.DB, target.ID, sessionID, now)
	if errors.Is(err, store.ErrRepoLockNotHolder) {
		// A session that waits for the lock and does not hold it leaves the
		// queue: what `seam lock acquire --wait` does when its timeout ends.
		cur, left, lerr := store.LeaveRepoLockQueue(ctx, s.cfg.DB, target.ID, sessionID, now)
		if lerr != nil {
			return errResult("locks_release", lerr)
		}
		if !left {
			return errResult("locks_release", fmt.Errorf("%w: LOCK %s is not held by this session, which does not wait "+
				"for it either", store.ErrRepoLockNotHolder, target.Label()))
		}
		s.lkEvent(ctx, sessionID, cur.ID, map[string]any{"action": core.RepoLockActionLeftQueue, "name": cur.Name})
		j, err := s.lkSeer(ctx).lkJSON(cur, now)
		if err != nil {
			return errResult("locks_release", err)
		}
		return jsonResult(map[string]any{"lock": j, "released": false, "left_queue": true})
	}
	if err != nil {
		return errResult("locks_release", err)
	}
	s.lkEvent(ctx, sessionID, freed.ID, map[string]any{"action": core.RepoLockActionReleased, "name": freed.Name})
	j, err := s.lkSeer(ctx).lkJSON(freed, now)
	if err != nil {
		return errResult("locks_release", err)
	}
	return jsonResult(map[string]any{"lock": j, "released": true})
}

// errLkRestrictedDeclaration is a restricted account's declaration that cannot
// be filed: it lists no project the account may write, or the repository's
// lock of that name is one the account may not reach. One text for both, which
// names no lock, project, holder or alias, so a refusal never confirms that a
// lock the account cannot reach exists.
func errLkRestrictedDeclaration(ctx context.Context) error {
	return fmt.Errorf("this declaration cannot be filed from account %s: a restricted account's declaration lists a "+
		"project the account may write (projects=, or a checkout that maps to one; project_list) and names a lock "+
		"the account may hold; ask the owner to declare the repository's lock, or to list your project on it",
		access.AccountOf(ctx))
}
