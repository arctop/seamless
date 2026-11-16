package hooks

// Repo locks at the hooks (plan repo-locks, RL2b; features.RepoLocks, off by
// default). SessionStart asks the briefing for the lock lines of the checkout
// the session starts in (retrieve.RepoLockInput) and, while the feature is on,
// records on the ambient session the checkout's lock aliases and the holder
// of each lock it was shown (store.RecordSessionRepoLocks). UserPromptSubmit
// then reads that record off the ambient row it already loads and repeats a
// lock's line once whenever its holder changed (retrieve.RepoLockChanges).
//
// The checkout's identity is the client's: `seam hook` resolves it on the
// agent's machine and sends it on session-start's query string, lock_origin
// included where a side clone's local-path origin leads elsewhere. The daemon
// fills or follows it from its own disk only where it may read that disk at
// all -- the checkout is on this machine and no View rides the request
// (lockDiskOK, the rule of CWDIdentity and of localDiskOK) -- never for a path
// another machine named.

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/gitread"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store"
)

// repoLockInput is a main session's request for its checkout's lock lines, or
// nil for a subagent's start (which a child briefing serves) and with no
// database. Its functions run only while repo locks are on.
func (h *Handler) repoLockInput(ctx context.Context, client Client, host string, p hookPayload) *retrieve.RepoLockInput {
	if h.db == nil || p.AgentType != "" {
		return nil
	}
	return &retrieve.RepoLockInput{
		Aliases: func() []core.RepoLockAlias { return h.repoLockAliases(ctx, host, p) },
		Self: func() []string {
			if p.SessionID == "" {
				return nil
			}
			return h.conversationSessionIDs(ctx, client, p.SessionID)
		},
		HooksOff: p.Identity.LockHooks == lockHooksOff,
	}
}

// repoLockAliases derives the lock aliases of the checkout a SessionStart
// names (store.RepoLockAliasesFor), nil for a cwd no lock can name. The
// client's lock_origin stands in for its origin; without one, the daemon
// fills the roots and follows a local-path origin on its own disk where
// lockDiskOK allows, as the lock tools do.
func (h *Handler) repoLockAliases(ctx context.Context, host string, p hookPayload) []core.RepoLockAlias {
	id := p.identity(host)
	switch {
	case p.Identity.LockOrigin != "":
		id.Origin = p.Identity.LockOrigin
	case h.lockDiskOK(ctx, host):
		if id.RepoRoot == "" && id.MainRoot == "" && store.IsAbsRepoPath(id.CWD) {
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
		id.Origin = core.FollowLocalOrigin(id.Origin, base, readLockRepo)
	}
	aliases, err := store.RepoLockAliasesFor(id, h.localHost)
	if err != nil {
		// Outside any repository, or an identity no alias may hold: no lock
		// names this checkout, and the session records none.
		h.logger.Debug("hooks: no repo-lock identity for the checkout", "error", err)
		return nil
	}
	return aliases
}

// lockDiskOK reports whether the daemon may read the checkout from its own
// disk for a repo lock: the checkout is on this machine and no View rides the
// request (under peer_auth every path a hook names was chosen by another
// account). It is localDiskOK's rule without its skip event: a lock identity
// the client did not complete is no capture the owner needs told about.
func (h *Handler) lockDiskOK(ctx context.Context, host string) bool {
	if _, peerAuth := access.FromContext(ctx); peerAuth {
		return false
	}
	return h.sameHost(host)
}

// readLockRepo reports the main checkout root and the origin of the repository
// at path, or "" and "" when path is no checkout: core.FollowLocalOrigin's
// reader, on this machine's disk (lockDiskOK).
func readLockRepo(path string) (string, string) {
	if _, err := os.Lstat(filepath.Join(path, ".git")); err != nil {
		return "", ""
	}
	main := gitread.MainWorktreeRoot(path)
	return main, gitread.OriginURL(main)
}

// recordRepoLocks records on the session a SessionStart created or resumed
// what its briefing did with in (retrieve.RepoLockResult): nothing unless repo
// locks were on. Best-effort: a failure costs the session its presence and
// its change notices, never its start.
func (h *Handler) recordRepoLocks(ctx context.Context, client Client, sessionID string, in *retrieve.RepoLockInput) {
	if in == nil || !in.Result.On || sessionID == "" || h.db == nil {
		return
	}
	if err := store.RecordSessionRepoLocks(ctx, h.db, sessionID, in.Result.Aliases, in.Result.Seen); err != nil {
		h.recordHookError(ctx, "repo-lock-record", client, err)
	}
}

// repoLockNotice is the prompt's repeat of the lock lines whose holder changed
// since the session was last told (retrieve.RepoLockChanges), each once: the
// new holders are recorded before a line is returned, and a line whose record
// failed is held for the next prompt. It is "" -- reading nothing past the
// ambient row the prompt hook already loaded -- for a session that recorded no
// lock aliases (repo locks were off when it started, or it works in no
// repository), and for one that is not this caller's own to brief (reach
// other than reachOK).
func (h *Handler) repoLockNotice(ctx context.Context, client Client, externalID string, amb core.Session, found bool, reach sessionReach) string {
	if !found || reach != reachOK || h.db == nil || h.retrieve == nil || !access.SessionAccountOK(ctx, amb.Account) {
		return ""
	}
	aliases, seen, ok := store.SessionRepoLocks(amb)
	if !ok {
		return ""
	}
	lines, next, err := h.retrieve.RepoLockChanges(ctx, amb.ProjectSlug, aliases, seen,
		func() []string { return h.conversationSessionIDs(ctx, client, externalID) })
	if err != nil {
		h.recordHookError(ctx, "repo-lock-notice", client, err)
		return ""
	}
	if next != nil && !maps.Equal(next, seen) {
		if err := store.SetSessionRepoLockSeen(ctx, h.db, amb.ID, next); err != nil {
			h.recordHookError(ctx, "repo-lock-notice", client, err)
			return ""
		}
	}
	return strings.Join(lines, "\n")
}
