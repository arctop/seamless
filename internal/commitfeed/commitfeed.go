// Package commitfeed builds the commit-matching snapshot (config
// commits.snapshot_file): which sessions ran when, and in which repository,
// which tasks a commit may name, and which commits each of them recorded
// (commit records, commits.record). An integrity monitor matches a branch's
// commits against it -- a commit a session or task recorded, one whose message
// names a session or task listed here, one made while a session in its
// repository was open -- and flags the commits nothing accounts for.
//
// The snapshot is identifiers and times: session names, task ids, repository
// names, start and end times, commit ids. Never a finding, a title, a body, a
// path or a project slug. The repository of a confidential or sealed project,
// or of a locked one, is never named (its repo is null),
// the way the metadata feed sends such a project's slug only as a keyed hash.
//
// One JSON object, contract version 1:
//
//	kind      "seamless_feed"
//	version   1
//	host      the daemon's host name, lowercase
//	taken_at  when it was built (ISO-8601 UTC, to the millisecond)
//	since     the start of what it covers: the window before taken_at, or the
//	          start of the first session when that is later; a commit seen
//	          before since is outside it
//	sessions  [{id, repo, started_at, ended_at, commits?}]: every session live
//	          at some point since since. id is its name (cc/..., cx/...,
//	          sess/...), or its ULID when the name is outside the contract's
//	          pattern; repo the repository its working directory is in, or
//	          null; ended_at null while it is active, and a session idle past
//	          the liveness threshold ended at its last activity
//	tasks     [{id, repo?, commits?}]: every open or in-progress task and every
//	          task changed since since; repo its project's repository, when
//	          the project maps exactly one
//
// commits are the full lowercase ids recorded on the session or task, oldest
// first. A repository's name is the last segment of its origin remote
// (owner/name.git gives name), or of its path when it has no origin.
package commitfeed

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// The contract's identity.
const (
	Kind    = "seamless_feed"
	Version = 1
)

// The contract's bounds: entries per list. One entry's commits are bounded by
// store.MaxRecordedCommits, which the record itself enforces.
const (
	MaxSessions = 100000
	MaxTasks    = 100000
)

// maxEntryCommits is the contract's bound on one entry's commits; the record
// holds every owner to it.
const maxEntryCommits = store.MaxRecordedCommits

// timeLayout is every time the snapshot carries: UTC, to the millisecond.
const timeLayout = "2006-01-02T15:04:05.000Z"

var (
	// hostRe is the envelope's host: a lowercase host name.
	hostRe = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?$`)
	// sessionIDRe is a session id the contract accepts: a session's name.
	sessionIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)
	// repoNameRe is a repository name, as a code host allows one.
	repoNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
	// ulidRe is a task id.
	ulidRe = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)
)

// Snapshot is one snapshot, in the contract's shape.
type Snapshot struct {
	Kind     string    `json:"kind"`
	Version  int       `json:"version"`
	Host     string    `json:"host"`
	TakenAt  string    `json:"taken_at"`
	Since    string    `json:"since"`
	Sessions []Session `json:"sessions"`
	Tasks    []Task    `json:"tasks"`
}

// Session is one session: repo and ended_at are always present, null when
// unknown (repo) or while the session is active (ended_at).
type Session struct {
	ID        string   `json:"id"`
	Repo      *string  `json:"repo"`
	StartedAt string   `json:"started_at"`
	EndedAt   *string  `json:"ended_at"`
	Commits   []string `json:"commits,omitempty"`
}

// Task is one task: repo is left out when its project names no single
// repository.
type Task struct {
	ID      string   `json:"id"`
	Repo    *string  `json:"repo,omitempty"`
	Commits []string `json:"commits,omitempty"`
}

// Options shape one snapshot.
type Options struct {
	// Host is the envelope's host: a lowercase host name.
	Host string
	// Window is how far back since reaches.
	Window time.Duration
	// Now is when the snapshot is taken.
	Now time.Time
	// Idle is the liveness threshold (gardener.session_idle_minutes): a
	// session still marked active but idle longer than this is reported ended
	// at its last activity, as the idle reaper would expire it, whether or not
	// the reaper runs. 0 trusts the status alone.
	Idle time.Duration
	// Locked reports whether peer_auth locks a project; its repository is
	// never named. nil means none.
	Locked func(project string) bool
}

// Trimmed counts what a snapshot left out to stay inside the contract.
type Trimmed struct {
	Sessions int // past MaxSessions: the earliest-ended, with since moved past them
	Tasks    int // past MaxTasks: the least recently changed
	Commits  int // recorded ids that are not a full lowercase commit id
	Unnamed  int // sessions whose name and id both fall outside the contract
}

// Build reads one snapshot from db.
func Build(ctx context.Context, db *sql.DB, opts Options) (Snapshot, Trimmed, error) {
	var trim Trimmed
	if len(opts.Host) > 128 || !hostRe.MatchString(opts.Host) {
		return Snapshot{}, trim, fmt.Errorf("commitfeed.Build: host %q is not a lowercase host name", opts.Host)
	}
	if opts.Window <= 0 {
		return Snapshot{}, trim, fmt.Errorf("commitfeed.Build: window must be positive, got %s", opts.Window)
	}
	// The query bound is the instant itself; only what is written is cut to
	// the millisecond, so a session that started within taken's millisecond
	// is still listed.
	until := opts.Now.UTC()
	taken := until.Truncate(time.Millisecond)
	since := taken.Add(-opts.Window)
	first, recorded, err := store.EarliestSessionStart(ctx, db)
	if err != nil {
		return Snapshot{}, trim, fmt.Errorf("commitfeed.Build: %w", err)
	}
	switch {
	case !recorded:
		since = taken // nothing recorded yet, so nothing is covered
	case first.After(since):
		since = first.UTC().Truncate(time.Millisecond)
	}

	sessions, err := store.FeedSessions(ctx, db, since, until)
	if err != nil {
		return Snapshot{}, trim, fmt.Errorf("commitfeed.Build: %w", err)
	}
	sessions = settleIdle(sessions, since, until, opts.Idle)
	sessions, since, trim.Sessions = trimSessions(sessions, since, taken)
	tasks, err := store.FeedTasks(ctx, db, since, MaxTasks+1)
	if err != nil {
		return Snapshot{}, trim, fmt.Errorf("commitfeed.Build: %w", err)
	}
	if len(tasks) > MaxTasks {
		// FeedTasks reads one past the bound, so the count left out is a lower
		// bound: enough to say the list was cut.
		trim.Tasks = len(tasks) - MaxTasks
		tasks = tasks[:MaxTasks]
	}

	rows, err := store.RepoMapRows(ctx, db)
	if err != nil {
		return Snapshot{}, trim, fmt.Errorf("commitfeed.Build: %w", err)
	}
	fence := newFence(db, opts.Locked)

	sessionIDs := make([]string, len(sessions))
	for i, s := range sessions {
		sessionIDs[i] = s.ID
	}
	sessionCommits, err := store.RecordedCommits(ctx, db, store.CommitOwnerSession, sessionIDs)
	if err != nil {
		return Snapshot{}, trim, fmt.Errorf("commitfeed.Build: %w", err)
	}
	taskIDs := make([]string, len(tasks))
	for i, t := range tasks {
		taskIDs[i] = t.ID
	}
	taskCommits, err := store.RecordedCommits(ctx, db, store.CommitOwnerTask, taskIDs)
	if err != nil {
		return Snapshot{}, trim, fmt.Errorf("commitfeed.Build: %w", err)
	}

	snap := Snapshot{
		Kind: Kind, Version: Version, Host: opts.Host,
		TakenAt: taken.Format(timeLayout), Since: since.Format(timeLayout),
		Sessions: make([]Session, 0, len(sessions)), Tasks: make([]Task, 0, len(tasks)),
	}
	seen := make(map[string]bool, len(sessions))
	for _, s := range sessions {
		id := s.Name
		if !sessionIDRe.MatchString(id) || seen[id] {
			id = s.ID
		}
		if !sessionIDRe.MatchString(id) || seen[id] {
			trim.Unnamed++
			continue
		}
		seen[id] = true
		out := Session{ID: id, StartedAt: s.CreatedAt.UTC().Format(timeLayout)}
		if !s.Active {
			ended := s.UpdatedAt.UTC().Format(timeLayout)
			out.EndedAt = &ended
		}
		// The repository stays unnamed when either the session's project or
		// the project the repository maps to is fenced.
		if row, ok := store.RepoRowForCWD(rows, s.Host, s.CWD); ok &&
			!fence.hides(ctx, s.ProjectSlug) && !fence.hides(ctx, row.Slug) {
			out.Repo = repoName(row)
		}
		out.Commits = validCommits(sessionCommits[s.ID], &trim)
		snap.Sessions = append(snap.Sessions, out)
	}
	byProject := projectRepos(rows)
	for _, t := range tasks {
		if !ulidRe.MatchString(t.ID) {
			continue // not an id the contract can name; tasks are minted as ULIDs
		}
		out := Task{ID: t.ID, Commits: validCommits(taskCommits[t.ID], &trim)}
		if !fence.hides(ctx, t.ProjectSlug) {
			out.Repo = byProject[t.ProjectSlug]
		}
		snap.Tasks = append(snap.Tasks, out)
	}
	slices.SortFunc(snap.Tasks, func(a, b Task) int { return strings.Compare(a.ID, b.ID) })
	return snap, trim, nil
}

// settleIdle applies the liveness threshold: a session still marked active
// but idle past idle has ended at its last activity -- the end the reaper
// would give it -- so a dead agent's session never reads as open forever, and
// one that ended so before since leaves the window.
func settleIdle(sessions []store.FeedSession, since, until time.Time, idle time.Duration) []store.FeedSession {
	if idle <= 0 {
		return sessions
	}
	kept := sessions[:0]
	for _, s := range sessions {
		if s.Active && until.Sub(s.UpdatedAt) > idle {
			s.Active = false
		}
		if !s.Active && s.UpdatedAt.Before(since) {
			continue
		}
		kept = append(kept, s)
	}
	return kept
}

// trimSessions keeps the snapshot inside MaxSessions. Past it, the sessions
// that ended earliest are left out and since moves to the latest end among
// them, so the snapshot still covers everything it claims to: every session
// live after since is listed. The result keeps FeedSessions' order.
func trimSessions(sessions []store.FeedSession, since, taken time.Time) ([]store.FeedSession, time.Time, int) {
	if len(sessions) <= MaxSessions {
		return sessions, since, 0
	}
	end := func(s store.FeedSession) time.Time {
		if s.Active {
			return taken
		}
		return s.UpdatedAt
	}
	byEnd := slices.Clone(sessions)
	slices.SortStableFunc(byEnd, func(a, b store.FeedSession) int { return end(b).Compare(end(a)) })
	dropped := byEnd[MaxSessions:]
	cut := end(dropped[0]).UTC().Truncate(time.Millisecond)
	if cut.After(since) {
		since = cut
	}
	keep := make(map[string]bool, MaxSessions)
	for _, s := range byEnd[:MaxSessions] {
		keep[s.ID] = true
	}
	kept := slices.DeleteFunc(slices.Clone(sessions), func(s store.FeedSession) bool { return !keep[s.ID] })
	return kept, since, len(dropped)
}

// validCommits is an owner's recorded ids that the contract can carry: full
// lowercase commit ids (the record refuses anything else; a row edited by hand
// is counted and left out rather than making the whole snapshot unreadable).
func validCommits(ids []string, trim *Trimmed) []string {
	var out []string
	for _, id := range ids {
		if !core.IsCommitID(id) {
			trim.Commits++
			continue
		}
		out = append(out, id)
	}
	if len(out) > maxEntryCommits {
		trim.Commits += len(out) - maxEntryCommits
		out = out[:maxEntryCommits]
	}
	return out
}

// repoName is the repository a mapping names: the last segment of its origin
// remote with any .git dropped, else the last segment of its path. nil when
// neither is a repository name the contract accepts.
func repoName(r store.RepoMapRow) *string {
	for _, cand := range []string{originName(r.Origin), lastSegment(r.Path)} {
		if cand != "" && repoNameRe.MatchString(cand) {
			return &cand
		}
	}
	return nil
}

// originName is the repository name in a remote URL: the segment after the
// last "/" or ":" (scp-like host:name), with a trailing ".git" dropped.
func originName(origin string) string {
	s := strings.TrimRight(strings.TrimSpace(origin), "/")
	s = strings.TrimSuffix(s, ".git")
	if i := strings.LastIndexAny(s, "/:"); i >= 0 {
		s = s[i+1:]
	}
	return s
}

// lastSegment is a path's final element, splitting on both separators so a
// Windows client's root names the same directory on any daemon.
func lastSegment(p string) string {
	p = strings.TrimRight(strings.TrimSpace(p), `/\`)
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

// projectRepos maps each project to its repository, for the projects whose
// mappings, on every host, name exactly one.
func projectRepos(rows []store.RepoMapRow) map[string]*string {
	names := map[string]map[string]bool{}
	for _, r := range rows {
		n := repoName(r)
		if n == nil {
			continue
		}
		if names[r.Slug] == nil {
			names[r.Slug] = map[string]bool{}
		}
		names[r.Slug][*n] = true
	}
	out := make(map[string]*string, len(names))
	for slug, set := range names {
		if len(set) != 1 {
			continue
		}
		for n := range set {
			out[slug] = &n
		}
	}
	return out
}

// fence decides which projects' repositories the snapshot never names: a
// confidential or sealed project, or a locked one. An
// isolation state that cannot be read hides, and each project is read once.
type fence struct {
	db     *sql.DB
	locked func(string) bool
	known  map[string]bool
}

func newFence(db *sql.DB, locked func(string) bool) *fence {
	return &fence{db: db, locked: locked, known: map[string]bool{}}
}

func (f *fence) hides(ctx context.Context, project string) bool {
	if project == "" {
		return false
	}
	if v, ok := f.known[project]; ok {
		return v
	}
	hidden := f.locked != nil && f.locked(project)
	if !hidden {
		iso, err := store.IsolationOf(ctx, f.db, project)
		hidden = err != nil || iso.FencesOutbound()
	}
	f.known[project] = hidden
	return hidden
}
