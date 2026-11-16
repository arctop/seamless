package retrieve

// Repo locks in the briefing and the prompt injection (plan repo-locks, RL2b;
// features.RepoLocks, off by default). A session that starts in a checkout of
// a repository with a declared lock is told, before it does anything, the
// state of each of the repository's locks and who else works there:
//
//	LOCK seamless main: held by cc/1fa4b02d-55d96b8d15ff0104 for 12 min, until 11:02 (2 waiting)
//	LOCK hooks off: this checkout runs no lock hook, so git will not check the lock before it moves a guarded ref
//	Also live in this repository: cc/8dd2fd5b-0a1b2c3d4e5f6a7b (idle 3 min), and 2 other sessions
//
// and its prompts repeat a lock's line once whenever the lock's holder changed
// since the session last saw it, so a session waiting for a lock learns that
// it is free. With the feature off, or no lock declared for the checkout,
// nothing here runs and nothing renders.
//
// Every line names a session only where the reader may read it -- its own
// principal's (access.SessionAccountOK) in a project the reader's scope may
// read (store.CanRead), the rule the lock tools name a holder by -- and counts
// the rest. A lock the reader may not reach (store.RepoLockReachable) and a
// declaration still waiting for the owner get no line at all, so a restricted
// reader cannot learn that one exists.

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/features"
	"github.com/arctop/seamless/internal/store"
)

// RepoLockInput asks a main session's briefing for the repo locks of the
// checkout the session starts in. The briefing calls Aliases and Self only
// while repo locks are on, so a caller pays for neither while they are off, and
// reports what it did in Result.
type RepoLockInput struct {
	// Aliases resolves the checkout's lock aliases (store.RepoLockAliasesFor of
	// the session's identity); nil or empty is a checkout no lock can name.
	Aliases func() []core.RepoLockAlias
	// Self lists the reader's own sessions (its conversation's): a holder
	// among them is "you", and none of them is a peer.
	Self func() []string
	// HooksOff is the client's report that the checkout runs no lock hook
	// (gitread.LockHooksActive false). False is also "not reported".
	HooksOff bool
	// Result is set by the briefing.
	Result RepoLockResult
}

// RepoLockResult is what a briefing did with a RepoLockInput, for the hook to
// record on the session (store.RecordSessionRepoLocks).
type RepoLockResult struct {
	// On is true when repo locks were on: the session records Aliases and
	// Seen. False means nothing was read and nothing is to be recorded.
	On bool
	// Aliases are the checkout's lock alias strings (store.RepoLockAliasStrings).
	Aliases []string
	// Seen maps each lock the reader was told about to the holder it was
	// shown: the holding session's ULID, "" for free.
	Seen map[string]string
}

// The limits of the lock lines (assembleBriefing's fixed allowance).
const (
	// repoLockMaxLines is how many LOCK lines one briefing or prompt shows;
	// the rest of the repository's locks are counted on one more line.
	repoLockMaxLines = 4
	// presenceMaxNames is how many peers the presence line names; the rest,
	// and every peer the reader may not see, are counted.
	presenceMaxNames = 5
	// repoLockLabelRunes caps a lock's label (the repository's basename and
	// the lock's name).
	repoLockLabelRunes = 120
)

// repoLockHooksOffLine is the briefing's line for a checkout whose client
// reported no lock hook.
const repoLockHooksOffLine = "LOCK hooks off: this checkout runs no lock hook, so git will not check the lock before it moves a guarded ref\n"

// SetSessionIdle sets the liveness threshold the presence line counts a
// session live by (gardener.session_idle_minutes, core.Session.LiveAsOf).
// <= 0, the default, is core.SessionIdleTTL.
func (s *Service) SetSessionIdle(d time.Duration) { s.sessionIdle = d }

// repoLockView is a briefing's lock lines, rendered.
type repoLockView struct {
	locks    string // the LOCK lines (and the "+N more" line), each ending in "\n"
	hooksOff string // repoLockHooksOffLine, or ""
	presence string // the presence line, or ""
}

// pinned is the part of the view no budget drops: the lock lines and the hooks
// line.
func (v repoLockView) pinned() string { return v.locks + v.hooksOff }

// empty reports whether the view renders nothing.
func (v repoLockView) empty() bool { return v.locks == "" }

// sessionNamer names sessions to one reader, memoized: a session's name only
// where the reader may read it (store.CanRead from scope, the project the
// reader is briefed for, and access.SessionAccountOK), else nothing.
type sessionNamer struct {
	s        *Service
	ctx      context.Context
	scope    string
	readable map[string]bool
	byID     map[string]string
}

func (s *Service) newSessionNamer(ctx context.Context, scope string) *sessionNamer {
	return &sessionNamer{s: s, ctx: ctx, scope: scope, readable: map[string]bool{}, byID: map[string]string{}}
}

// name returns sess's name for the reader, "" when the reader may not see it.
func (n *sessionNamer) name(sess core.Session) (string, error) {
	if !access.SessionAccountOK(n.ctx, sess.Account) {
		return "", nil
	}
	readable, seen := n.readable[sess.ProjectSlug]
	if !seen {
		var err error
		if readable, err = store.CanRead(n.ctx, n.s.db, n.scope, sess.ProjectSlug); err != nil {
			return "", err
		}
		n.readable[sess.ProjectSlug] = readable
	}
	if !readable {
		return "", nil
	}
	return sanitizeName(sess.Name, 80), nil
}

// nameOf is name for a session id; "" too for an id that names no session.
func (n *sessionNamer) nameOf(id string) (string, error) {
	if got, ok := n.byID[id]; ok {
		return got, nil
	}
	sess, ok, err := store.SessionByID(n.ctx, n.s.db, id)
	if err != nil {
		return "", err
	}
	name := ""
	if ok {
		if name, err = n.name(sess); err != nil {
			return "", err
		}
	}
	n.byID[id] = name
	return name, nil
}

// visibleRepoLocks returns the locks aliases resolve to that the reader is
// told about: active ones (a declaration waiting for the owner has no line),
// within the reader's reach (store.RepoLockReachable).
func (s *Service) visibleRepoLocks(ctx context.Context, aliases []core.RepoLockAlias) ([]core.RepoLock, error) {
	if len(aliases) == 0 {
		return nil, nil
	}
	all, err := store.RepoLocksFor(ctx, s.db, aliases)
	if err != nil {
		return nil, err
	}
	out := make([]core.RepoLock, 0, len(all))
	for _, l := range all {
		if l.State == core.RepoLockActive && store.RepoLockReachable(ctx, l) {
			out = append(out, l)
		}
	}
	return out, nil
}

// repoLockHolder is the holder a lock's line shows: the holding session's
// ULID while the hold is live, "" (free) otherwise -- a lapsed hold is free to
// take.
func repoLockHolder(l core.RepoLock, now time.Time) string {
	if l.HoldLive(now) {
		return l.Holder
	}
	return ""
}

// holderWords is how a lock line names the session in l's Holder column: "you"
// (with its name) for one of the reader's own sessions, its name where the
// reader may see it, else "another session".
func (n *sessionNamer) holderWords(l core.RepoLock, self []string) (string, error) {
	name, err := n.nameOf(l.Holder)
	if err != nil {
		return "", err
	}
	switch {
	case slices.Contains(self, l.Holder) && name != "":
		return "you (" + name + ")", nil
	case slices.Contains(self, l.Holder):
		return "you", nil
	case name != "":
		return name, nil
	default:
		return "another session", nil
	}
}

// repoLockLine renders one lock's line, without a newline: "LOCK seamless
// main: free", "LOCK seamless main: held by <who> for 12 min, until 11:02",
// "LOCK seamless main: free (the hold by <who> lapsed at 11:02)", each with
// " (N waiting)" while the queue holds anyone. who names l's holder
// (holderWords); times are the reader's local clock (access.Clock).
func repoLockLine(l core.RepoLock, who string, now time.Time) string {
	var b strings.Builder
	b.WriteString("LOCK " + sanitizeName(l.Label(), repoLockLabelRunes) + ": ")
	switch {
	case l.HoldLive(now):
		b.WriteString("held by " + who)
		if l.HeldSince != nil {
			b.WriteString(" for " + wholeMinutes(now.Sub(*l.HeldSince)))
		}
		if l.LeaseExpiresAt != nil {
			b.WriteString(", until " + access.Clock(*l.LeaseExpiresAt))
		}
	case l.Holder != "" && l.LeaseExpiresAt != nil:
		b.WriteString("free (the hold by " + who + " lapsed at " + access.Clock(*l.LeaseExpiresAt) + ")")
	default:
		b.WriteString("free")
	}
	if n := len(l.Waiters); n > 0 {
		fmt.Fprintf(&b, " (%d waiting)", n)
	}
	return b.String()
}

// wholeMinutes renders a duration as whole minutes ("12 min"), never negative.
func wholeMinutes(d time.Duration) string {
	m := int(d / time.Minute)
	if m < 0 {
		m = 0
	}
	return fmt.Sprintf("%d min", m)
}

// repoLockMoreLine counts the locks past repoLockMaxLines, without a newline.
func repoLockMoreLine(n int) string {
	if n == 1 {
		return "LOCK +1 more lock on this repository: seam lock list --here"
	}
	return fmt.Sprintf("LOCK +%d more locks on this repository: seam lock list --here", n)
}

// presenceLine renders the live peers: up to presenceMaxNames named with their
// idle minutes, the rest counted; "" when there is no peer. Without a newline.
func presenceLine(named []string, others int) string {
	if len(named) == 0 && others == 0 {
		return ""
	}
	parts := slices.Clone(named)
	switch {
	case others == 1:
		parts = append(parts, "1 other session")
	case others > 1:
		parts = append(parts, fmt.Sprintf("%d other sessions", others))
	}
	if others > 0 && len(named) > 0 {
		parts[len(parts)-1] = "and " + parts[len(parts)-1]
	}
	return "Also live in this repository: " + strings.Join(parts, ", ")
}

// presence renders the presence line for the live sessions other than the
// reader's own whose checkouts resolve to one of locks.
func (s *Service) presence(ctx context.Context, n *sessionNamer, locks []core.RepoLock, self []string, now time.Time) (string, error) {
	var aliases []string
	for _, l := range locks {
		for _, a := range l.Aliases {
			if !slices.Contains(aliases, a.Alias) {
				aliases = append(aliases, a.Alias)
			}
		}
	}
	peers, err := store.LiveRepoPeers(ctx, s.db, aliases, self, now, s.sessionIdle)
	if err != nil {
		return "", err
	}
	var named []string
	others := 0
	for _, p := range peers {
		if !p.LiveAsOf(now, s.sessionIdle) {
			continue
		}
		name := ""
		if len(named) < presenceMaxNames {
			if name, err = n.name(p); err != nil {
				return "", err
			}
		}
		if name == "" {
			others++
			continue
		}
		named = append(named, name+" (idle "+wholeMinutes(now.Sub(p.UpdatedAt))+")")
	}
	return presenceLine(named, others), nil
}

// featuresOnce returns the effective optional features (effectiveFeatures),
// read on the first call and remembered after it, so one assembly that asks
// about two features reads the override row once -- and one that asks about
// none reads nothing.
func (s *Service) featuresOnce(ctx context.Context) func() config.Features {
	var (
		f    config.Features
		read bool
	)
	return func() config.Features {
		if !read {
			f, read = s.effectiveFeatures(ctx), true
		}
		return f
	}
}

// repoLocksOn reports whether repo locks are on for this request, as every
// surface judges them: the file/env base with the console's override.
func repoLocksOn(f func() config.Features) bool { return features.Enabled(f(), features.RepoLocks) }

// briefingRepoLocks is a main session's lock lines, for the project it is
// briefed for (scope, the caller's scope the lines name sessions from). It
// fills in.Result. A failed read costs the lines, never the briefing: it is
// logged, and the session still records its aliases.
func (s *Service) briefingRepoLocks(ctx context.Context, scope string, in *RepoLockInput, now time.Time) repoLockView {
	var aliases []core.RepoLockAlias
	if in.Aliases != nil {
		aliases = in.Aliases()
	}
	in.Result = RepoLockResult{On: true, Aliases: store.RepoLockAliasStrings(aliases), Seen: map[string]string{}}
	view, err := s.repoLockLines(ctx, scope, aliases, in, now)
	if err != nil {
		s.logger.Warn("retrieve: repo locks unavailable, briefing omits the lock lines", "error", err)
		in.Result.Seen = map[string]string{}
		return repoLockView{}
	}
	return view
}

// repoLockLines renders the view and records each lock's shown holder in
// in.Result.Seen.
func (s *Service) repoLockLines(ctx context.Context, scope string, aliases []core.RepoLockAlias, in *RepoLockInput, now time.Time) (repoLockView, error) {
	locks, err := s.visibleRepoLocks(ctx, aliases)
	if err != nil || len(locks) == 0 {
		return repoLockView{}, err
	}
	var self []string
	if in.Self != nil {
		self = in.Self()
	}
	n := s.newSessionNamer(ctx, scope)
	var b strings.Builder
	for i, l := range locks {
		in.Result.Seen[l.ID] = repoLockHolder(l, now)
		if i == repoLockMaxLines {
			b.WriteString(repoLockMoreLine(len(locks)-repoLockMaxLines) + "\n")
			continue
		}
		if i > repoLockMaxLines {
			continue
		}
		who := ""
		if l.Holder != "" {
			if who, err = n.holderWords(l, self); err != nil {
				return repoLockView{}, err
			}
		}
		b.WriteString(repoLockLine(l, who, now) + "\n")
	}
	view := repoLockView{locks: b.String()}
	if in.HooksOff {
		view.hooksOff = repoLockHooksOffLine
	}
	line, err := s.presence(ctx, n, locks, self, now)
	if err != nil {
		return repoLockView{}, err
	}
	if line != "" {
		view.presence = line + "\n"
	}
	return view, nil
}

// RepoLockChanges is the prompt hook's half: the lines of the locks whose
// holder changed since the session was last told, for the session that
// recorded aliases (its checkout's lock aliases) and seen (the holder it was
// last shown per lock, store.SessionRepoLocks), read from scope (the session's
// project, which the lines name sessions from). next is every visible lock's
// holder as of now, for the hook to record when it differs from seen; nil
// next -- repo locks are off -- records nothing.
//
// "Free" is a holder too, so a waiting session hears "LOCK seamless main:
// free". A lock the session was never told about (declared since) is news
// once. Two changes say nothing the session does not know: a lock one of its
// own sessions (self) just took, and one it held that is now free with no
// holder at all -- its own release, or the owner's force-release, which the
// claim notices already carry as "Lock lost". self is called only when a
// holder changed.
func (s *Service) RepoLockChanges(ctx context.Context, scope string, aliases []string, seen map[string]string, self func() []string) (lines []string, next map[string]string, err error) {
	if len(aliases) == 0 || !repoLocksOn(s.featuresOnce(ctx)) {
		return nil, nil, nil
	}
	locks, err := s.visibleRepoLocks(ctx, store.RepoLockAliasesOf(aliases))
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	next = make(map[string]string, len(locks))
	var (
		selfIDs []string
		selfOK  bool
		n       *sessionNamer
		more    int
	)
	for _, l := range locks {
		holder := repoLockHolder(l, now)
		next[l.ID] = holder
		prev, known := seen[l.ID]
		if known && prev == holder {
			continue
		}
		if !selfOK {
			if self != nil {
				selfIDs = self()
			}
			selfOK = true
		}
		if holder != "" && slices.Contains(selfIDs, holder) {
			continue // it just took the lock itself
		}
		if holder == "" && l.Holder == "" && known && slices.Contains(selfIDs, prev) {
			continue // its own hold ended: a release, or the owner's (a Lock lost notice)
		}
		if len(lines) == repoLockMaxLines {
			more++
			continue
		}
		if n == nil {
			n = s.newSessionNamer(ctx, scope)
		}
		who := ""
		if l.Holder != "" {
			if who, err = n.holderWords(l, selfIDs); err != nil {
				return nil, nil, err
			}
		}
		lines = append(lines, repoLockLine(l, who, now))
	}
	if more > 0 {
		lines = append(lines, repoLockMoreLine(more))
	}
	return lines, next, nil
}
