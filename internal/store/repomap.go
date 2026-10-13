package store

// Host-scoped repo identity: which machine a working directory lives on, and
// which project the (host, path) pair maps to.
//
// Before several devices could share one daemon, "the machine" was always the
// daemon's own, so the map was a flat {path: slug} JSON setting and any mapped
// path missing from the daemon's disk meant the repo had moved. Neither holds
// once a client is remote: two machines can mount the same path on different
// repositories, and every remote path is "missing" from the daemon's disk. The
// repo_map table carries the host, and the moved-repo heal (deadOwnerPaths) is
// restricted to rows this daemon can actually stat.
//
// The repo_project_map JSON setting survives as a LOCAL-HOST MIRROR: the
// console and doctor still read it, and it is kept in step with every local
// mutation here until Phase 2 moves them onto RepoMapRows. It is also folded
// into the unnamed ("") host bucket on read, which is what makes a database
// written before this table -- or by a seeder that never named its machine --
// resolve exactly as it did before.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	pathpkg "path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/gitread"
)

// SettingLocalHost is the settings key holding the host name this daemon
// adopted at startup (store.AdoptLocalHost). It is what tells a later
// AddRepoMapping -- or a one-shot subcommand like `seamlessd map-repo`, which
// runs in its own process -- which bucket "this machine" means, and which host's
// rows the JSON mirror must reflect. Unset means the machine has not named
// itself yet, the same state the empty host column carries.
const SettingLocalHost = "local_host"

// ErrRemoteRootUnknown is returned by RegisterProjectForCWD when a session on
// another host asks to be placed but carries no repository roots. The daemon
// will not derive them: its own disk says nothing about the client's, and two
// devices with the same username and home layout would silently map to each
// other's repos. Nothing is written; the caller degrades to the global scope and
// says so (a hook.error event, a session_start warning).
var ErrRemoteRootUnknown = errors.New("store: remote host sent no repo root")

// ErrRepoMappingNotFound is returned by RemoveRepoMappings and
// RemoveHostRepoMappings when a path has no mapping on the host being edited.
// Nothing is removed: the call is all-or-nothing, so one mistyped path never
// half-applies a batch.
var ErrRepoMappingNotFound = errors.New("store: no repo mapping for that path")

// RepoMapRow is one (host, path) -> project mapping.
type RepoMapRow struct {
	Host string `json:"host"` // "" = the machine that has not named itself
	Path string `json:"path"`
	Slug string `json:"slug"`
	// Origin is the repository's origin remote URL, or "" when unknown. It is
	// the only piece of repo identity that survives a clone onto another machine
	// under a different path, which is what makes cross-host adoption possible
	// at all.
	Origin    string    `json:"origin"`
	CreatedAt time.Time `json:"createdAt"`
}

// CWDIdentity is everything the daemon needs to place an agent's working
// directory without reading that agent's disk. The client resolves its own git
// identity (seam hook, the session_start args) and sends it along; only a
// LOCAL-host identity may be filled in from the daemon's filesystem.
type CWDIdentity struct {
	Host     string // "" = the caller did not say, treated as the daemon's own host
	CWD      string // absolute working directory as the client reports it
	RepoRoot string // enclosing repository root ("" = unknown)
	MainRoot string // main checkout root for a linked worktree ("" = same as RepoRoot)
	Origin   string // origin remote URL ("" = unknown)
}

// RepoMapAdoption reports that RegisterProjectForCWD re-pointed an existing
// project at a repo's new location instead of minting a fresh -N slug: every
// LOCAL row owning the derived slug named a path that no longer exists on disk,
// so the repo was moved, not duplicated. The remap already happened; callers
// only surface it (an event, a console notice).
type RepoMapAdoption struct {
	Slug     string   // the adopted project slug
	NewPath  string   // the repo root that now owns the slug
	OldPaths []string // the dead rows that owned it, now removed
}

// repoMapExecutor is the read+write subset shared by *sql.DB and *sql.Tx that
// repo_map access needs, so a snapshot and a mirror rewrite run identically on
// the pool or inside one transaction.
type repoMapExecutor interface {
	settingsExecutor
	rowQuerier
}

// AdoptLocalHost stamps this daemon's host onto everything that predates
// host-scoped identity, and records the host so later local writes land in the
// same bucket. It runs at startup, after store.Open and before anything reads
// the map, and is idempotent.
//
// Four things happen, in order: sessions still carrying the empty host become
// this machine's; repo_map rows in the unnamed bucket are re-homed here (a
// mapping written by a seeder or a `map-repo` that ran before the daemon ever
// did); other hosts' POSIX paths that an older Windows daemon stored with
// backslashes get their slashes back (reshapeMangledPOSIXRows); and the legacy
// repo_project_map JSON is imported as this host's rows. The mirror is then
// rewritten from those rows, so the console and doctor -- which still read the
// JSON until Phase 2 -- see exactly the local view.
//
// An empty host (an unreadable hostname) adopts nothing and re-homes nothing:
// "" already IS the unnamed bucket, and claiming rows for a machine that cannot
// name itself would only move them sideways.
func AdoptLocalHost(ctx context.Context, db *sql.DB, host string) error {
	host = strings.TrimSpace(host)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store.AdoptLocalHost: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	if host != "" {
		if _, err := tx.ExecContext(ctx,
			`UPDATE sessions SET host = ? WHERE host = ''`, host); err != nil {
			return fmt.Errorf("store.AdoptLocalHost: sessions: %w", err)
		}
		// Re-home the unnamed bucket. INSERT OR IGNORE first so a path this host
		// already owns keeps its own row (and its origin) rather than being
		// overwritten by the older, origin-less one.
		if _, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO repo_map (host, path, slug, origin, created_at)
			     SELECT ?, path, slug, origin, created_at FROM repo_map WHERE host = ''`,
			host); err != nil {
			return fmt.Errorf("store.AdoptLocalHost: re-home: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM repo_map WHERE host = ''`); err != nil {
			return fmt.Errorf("store.AdoptLocalHost: re-home cleanup: %w", err)
		}
	}
	if err := reshapeMangledPOSIXRows(ctx, tx, host); err != nil {
		return fmt.Errorf("store.AdoptLocalHost: %w", err)
	}

	mirror, err := repoMapMirror(ctx, tx)
	if err != nil {
		return fmt.Errorf("store.AdoptLocalHost: %w", err)
	}
	now := core.FormatTime(time.Now().UTC())
	paths := make([]string, 0, len(mirror))
	for p := range mirror {
		paths = append(paths, p)
	}
	slices.Sort(paths) // deterministic import order: a stable log and a stable test
	for _, path := range paths {
		if _, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO repo_map (host, path, slug, origin, created_at)
			VALUES (?, ?, ?, '', ?)`, host, path, mirror[path], now); err != nil {
			return fmt.Errorf("store.AdoptLocalHost: import %q: %w", path, err)
		}
	}
	if err := setSettingTx(ctx, tx, SettingLocalHost, host); err != nil {
		return fmt.Errorf("store.AdoptLocalHost: %w", err)
	}
	if err := rewriteRepoMapMirror(ctx, tx, host); err != nil {
		return fmt.Errorf("store.AdoptLocalHost: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store.AdoptLocalHost: commit: %w", err)
	}
	return nil
}

// mangledPOSIXRow selects another host's row whose path is a POSIX path a
// Windows daemon rewrote with backslashes: until 2026-10 normPath ran every
// backslash-free path through filepath.Clean, so a Linux or macOS client's
// "/home/me/app" was stored as "\home\me\app". The shape cannot come from a
// client -- a POSIX one sends "/...", a Windows one a drive or UNC path -- so a
// single leading backslash is the mangling and nothing else. "" and the local
// host are excluded: those rows are this machine's own, never a client's.
const mangledPOSIXRow = `host NOT IN ('', ?) AND substr(path, 1, 1) = '\' AND substr(path, 2, 1) <> '\'`

// reshapeMangledPOSIXRows turns every mangledPOSIXRow back into the POSIX path
// its client sent. Left alone, such a row never matches that client's cwd again
// (normPath now keeps "/home/me/app" as it is), so its next session would mint a
// -N project beside the one it already has. Idempotent: a reshaped row no longer
// matches. INSERT OR IGNORE first, like the re-home, so a row already stored in
// the right shape keeps its own slug and origin.
func reshapeMangledPOSIXRows(ctx context.Context, tx *sql.Tx, localHost string) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO repo_map (host, path, slug, origin, created_at)
		     SELECT host, replace(path, '\', '/'), slug, origin, created_at
		       FROM repo_map WHERE `+mangledPOSIXRow, localHost); err != nil {
		return fmt.Errorf("reshape POSIX paths: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM repo_map WHERE `+mangledPOSIXRow, localHost); err != nil {
		return fmt.Errorf("reshape POSIX paths: cleanup: %w", err)
	}
	return nil
}

// RepoMapRows returns every mapping, ordered by host then path. The legacy JSON
// mirror is folded into the unnamed ("") host bucket, so a database that has not
// been adopted yet still reports its mappings.
func RepoMapRows(ctx context.Context, db *sql.DB) ([]RepoMapRow, error) {
	rows, err := repoMapSnapshot(ctx, db)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(rows, func(a, b RepoMapRow) int {
		if c := strings.Compare(a.Host, b.Host); c != 0 {
			return c
		}
		return strings.Compare(a.Path, b.Path)
	})
	return rows, nil
}

// ResolveProjectForCWD maps a host's absolute working directory to a project
// slug via that host's repo_map rows, using a longest-prefix match. It is
// read-only and failure-soft: an unresolvable cwd (or an unmapped host) returns
// "" (the global scope), never an error from the matching itself. Use it on read
// paths (briefing, prompt recall); use RegisterProjectForCWD on session-start
// paths that should grow the map.
//
// An empty host resolves against the unnamed bucket, which carries the legacy
// JSON mirror -- so a caller that does not know its own machine still sees this
// daemon's own map, exactly as before host scoping.
func ResolveProjectForCWD(ctx context.Context, db *sql.DB, host, cwd string) (string, error) {
	rows, err := repoMapSnapshot(ctx, db)
	if err != nil {
		return "", err
	}
	return matchProjectPath(cwd, pathSlugs(rows, host)), nil
}

// RepoRootsForProject inverts one host's mappings: every repo path that host has
// mapped, per project slug (a project can own several -- the main checkout plus
// out-of-tree worktrees). The gardener's ship-evidence pass reads it with the
// LOCAL host, because it goes on to open those paths on this machine.
func RepoRootsForProject(ctx context.Context, db *sql.DB, host string) (map[string][]string, error) {
	rows, err := repoMapSnapshot(ctx, db)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for path, slug := range pathSlugs(rows, host) {
		out[slug] = append(out[slug], path)
	}
	return out, nil
}

// AddRepoMapping records repoPath -> slug for THIS machine (the host recorded by
// AdoptLocalHost, or the unnamed bucket when the daemon has never run here) and
// keeps the JSON mirror in step. It is the owner-facing override behind
// `seamlessd map-repo` and the seeders; agents grow the map through
// RegisterProjectForCWD instead. Re-recording the same entry is a no-op.
func AddRepoMapping(ctx context.Context, db *sql.DB, repoPath, slug string) error {
	host, err := localHostSetting(ctx, db)
	if err != nil {
		return fmt.Errorf("store.AddRepoMapping: %w", err)
	}
	return putRepoMapRow(ctx, db, RepoMapRow{Host: host, Path: repoPath, Slug: slug})
}

// LocalRepoMappings returns THIS machine's mappings: the rows of the host
// recorded by AdoptLocalHost plus the unnamed ("") bucket, which carries rows
// written before the machine named itself and entries that only ever existed in
// the legacy JSON mirror. They are the rows `seamlessd unmap-repo` can remove.
func LocalRepoMappings(ctx context.Context, db *sql.DB) ([]RepoMapRow, error) {
	host, err := localHostSetting(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("store.LocalRepoMappings: %w", err)
	}
	rows, err := RepoMapRows(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("store.LocalRepoMappings: %w", err)
	}
	var out []RepoMapRow
	for _, r := range rows {
		if r.Host == host || r.Host == "" {
			out = append(out, r)
		}
	}
	return out, nil
}

// RemoveRepoMappings deletes THIS machine's mapping for each path -- the
// owner-facing inverse of AddRepoMapping, behind `seamlessd unmap-repo`. The
// table row and the legacy JSON mirror entry go in one transaction: removing
// only the row would be undone at the next daemon start, when AdoptLocalHost
// imports the mirror back into the table. Paths match exactly after
// normalization, never by prefix, so unmapping a repo leaves the mappings of
// repos nested under it alone. Other hosts' rows are never touched.
//
// Only the route goes: the project and its memories, notes and tasks stay, and a
// repo that is still on disk maps itself again on its next session. A path with
// no mapping here fails the whole call with ErrRepoMappingNotFound. The removed
// rows are returned in the order the paths were given.
func RemoveRepoMappings(ctx context.Context, db *sql.DB, paths []string) ([]RepoMapRow, error) {
	return removeRepoMappings(ctx, db, "store.RemoveRepoMappings", "", paths)
}

// RemoveHostRepoMappings is RemoveRepoMappings for a named host: the owner's
// override behind `seamlessd unmap-repo --host`. On a shared daemon a client
// machine's rows live in THIS database, so that machine's own unmap-repo, which
// edits its own database, can never reach them. Naming this machine's host (or
// "") is exactly RemoveRepoMappings.
//
// Nothing is stat'd: whether a path still exists on another machine is
// unknowable from here (constraint network-install-identity-is-host-scoped), so
// each mapping is named exactly. Another host's removal never touches the legacy
// JSON mirror, which holds only the local view -- two machines with the same
// home layout map the same paths, and dropping the remote row must leave the
// local route alone.
func RemoveHostRepoMappings(ctx context.Context, db *sql.DB, host string, paths []string) ([]RepoMapRow, error) {
	return removeRepoMappings(ctx, db, "store.RemoveHostRepoMappings", strings.TrimSpace(host), paths)
}

// removeRepoMappings deletes host's mapping for each path; host "" (or the local
// host's own name) is this machine, whose rows include the unnamed bucket and
// whose removals are mirrored into the legacy JSON.
func removeRepoMappings(ctx context.Context, db *sql.DB, op, host string, paths []string) ([]RepoMapRow, error) {
	var removed []RepoMapRow
	err := inRepoMapTx(ctx, db, op, func(tx *sql.Tx) error {
		localHost, err := localHostSetting(ctx, tx)
		if err != nil {
			return err
		}
		local := host == "" || host == localHost
		owns := func(r RepoMapRow) bool {
			if local {
				return r.Host == localHost || r.Host == ""
			}
			return r.Host == host
		}
		rows, err := repoMapSnapshot(ctx, tx)
		if err != nil {
			return err
		}
		done := map[string]bool{}
		var del []string
		for _, p := range paths {
			want := normPath(p)
			if done[want] {
				continue // the same path twice in one call is one removal
			}
			found := false
			for _, r := range rows {
				if !owns(r) || normPath(r.Path) != want {
					continue
				}
				// A mirror-only entry has no table row, so this can legitimately
				// delete nothing; the mirror mutation below is what removes it.
				if _, err := tx.ExecContext(ctx,
					`DELETE FROM repo_map WHERE host = ? AND path = ?`, r.Host, r.Path); err != nil {
					return err
				}
				removed = append(removed, r)
				del = append(del, r.Path)
				found = true
			}
			if !found {
				if local {
					return fmt.Errorf("%w on this machine: %s", ErrRepoMappingNotFound, p)
				}
				return fmt.Errorf("%w on host %q: %s", ErrRepoMappingNotFound, host, p)
			}
			done[want] = true
		}
		if !local {
			return nil
		}
		return mirrorRepoMapMutation(ctx, tx, localHost, nil, del)
	})
	if err != nil {
		return nil, err
	}
	return removed, nil
}

// RegisterProjectForCWD resolves a working directory to a project slug like
// ResolveProjectForCWD, but grows the map when the cwd falls outside every repo
// that host has mapped: it keys identity on the repository's MAIN checkout,
// derives a slug from its directory name, records (host, path) -> slug, and
// ensures a projects-table row. This is how the map evolves as agents work in
// new repos -- no recompile, no manual map-repo. For an already-mapped cwd it
// still backfills the registry row, so project_list stays complete. A blank cwd,
// or a local cwd outside any git repo, resolves to the global scope ("") and
// registers nothing.
//
// The identity's roots are used as given; they are only derived from this
// daemon's filesystem when the identity's host IS the local host. A remote host
// that sends no roots gets ErrRemoteRootUnknown and NOTHING is written -- the
// daemon's disk cannot answer a question about another machine, and guessing
// would map two devices with the same home layout onto each other's repos.
// An identity that names no host at all is this machine's (an older client).
//
// Collisions, in the order they are decided:
//
//   - A moved repo heals itself: when every LOCAL row owning the derived slug
//     names a path that no longer exists, the repo moved rather than duplicated,
//     so the existing project is adopted (the dead rows are replaced by the new
//     root) instead of minting a -N slug that would split its history. Only local
//     rows are stat'd, so a remote client can never trigger the remap.
//   - A slug owned on ANOTHER host is adopted when the origins match, or when
//     either side's origin is unknown: that is the same repository checked out on
//     a second device, which is the whole point of a shared daemon.
//   - Anything else -- a same-host collision, or two known and different origins
//     -- mints base-2, base-3, ... so two unrelated repos that happen to share a
//     directory name never inherit each other's memories.
//
// The non-nil *RepoMapAdoption reports the moved-repo remap; it is nil on every
// other path.
func RegisterProjectForCWD(ctx context.Context, db *sql.DB, id CWDIdentity, localHost string) (string, *RepoMapAdoption, error) {
	cwd := strings.TrimSpace(id.CWD)
	if cwd == "" {
		return "", nil, nil
	}
	localHost = strings.TrimSpace(localHost)
	host := strings.TrimSpace(id.Host)
	if host == "" {
		host = localHost // an older client sends no host: it is talking to its own daemon
	}

	rows, err := repoMapSnapshot(ctx, db)
	if err != nil {
		return "", nil, err
	}
	hostPaths := pathSlugs(rows, host)
	if slug := matchProjectPath(cwd, hostPaths); slug != "" {
		// Already mapped: backfill the registry row (idempotent) and return.
		if _, err := EnsureProject(ctx, db, slug, slug); err != nil {
			return "", nil, err
		}
		return slug, nil, nil
	}

	root, main := normPath(id.RepoRoot), normPath(id.MainRoot)
	origin := strings.TrimSpace(id.Origin)
	switch {
	case root == "" && main == "":
		if host != localHost {
			return "", nil, fmt.Errorf("store.RegisterProjectForCWD: %w: host %q, cwd %q",
				ErrRemoteRootUnknown, host, cwd)
		}
		root = gitread.RepoRoot(cwd)
		if root == "" {
			return "", nil, nil // not inside a git repo: stay global, register nothing
		}
		// A linked worktree (git worktree add, the Claude/Codex apps' managed
		// worktrees) is a checkout of the main repository, not a repository of
		// its own: key project identity on the main checkout so a session whose
		// first contact is a worktree inherits the repo's project instead of
		// registering a transient project named after the worktree directory.
		main = gitread.MainWorktreeRoot(root)
		if origin == "" {
			origin = gitread.OriginURL(main)
		}
	case root == "":
		root = main
	case main == "":
		main = root // a regular checkout IS its own main worktree
	}

	if main != root {
		if slug := matchProjectPath(main, hostPaths); slug != "" {
			// Main checkout already mapped: adopt its project. An out-of-tree
			// worktree also gets its own row so read paths (briefing, prompt
			// recall) resolve it without re-deriving git identity.
			if _, err := EnsureProject(ctx, db, slug, slug); err != nil {
				return "", nil, err
			}
			if !pathHasPrefix(root, main) {
				if err := putRepoMapRow(ctx, db,
					RepoMapRow{Host: host, Path: root, Slug: slug, Origin: origin}); err != nil {
					return "", nil, err
				}
			}
			return slug, nil, nil
		}
	}

	name := lastSegment(main)
	base := core.Slugify(name)
	if base == "" {
		return "", nil, nil // no nameable directory: stay global rather than mint ""
	}

	if host == localHost {
		if dead := deadOwnerPaths(base, main, hostPaths); len(dead) > 0 {
			if _, err := EnsureProject(ctx, db, base, name); err != nil {
				return "", nil, err
			}
			if err := adoptRepoMapRows(ctx, db,
				RepoMapRow{Host: host, Path: main, Slug: base, Origin: origin}, dead); err != nil {
				return "", nil, err
			}
			if main != root && !pathHasPrefix(root, main) {
				if err := putRepoMapRow(ctx, db,
					RepoMapRow{Host: host, Path: root, Slug: base, Origin: origin}); err != nil {
					return "", nil, err
				}
			}
			return base, &RepoMapAdoption{Slug: base, NewPath: main, OldPaths: dead}, nil
		}
	}

	slug := slugForNewRepo(base, host, main, origin, rows)
	if _, err := EnsureProject(ctx, db, slug, name); err != nil {
		return "", nil, err
	}
	if err := putRepoMapRow(ctx, db,
		RepoMapRow{Host: host, Path: main, Slug: slug, Origin: origin}); err != nil {
		return "", nil, err
	}
	if main != root && !pathHasPrefix(root, main) {
		if err := putRepoMapRow(ctx, db,
			RepoMapRow{Host: host, Path: root, Slug: slug, Origin: origin}); err != nil {
			return "", nil, err
		}
	}
	return slug, nil, nil
}

// slugForNewRepo picks the slug a newly seen repo registers under: base when
// nothing owns it, base when only OTHER hosts own it and the origins allow the
// adoption (the same repository on a second device), and a -N mint otherwise.
func slugForNewRepo(base, host, main, origin string, rows []RepoMapRow) string {
	main = normPath(main)
	var sameHost, otherHost []RepoMapRow
	for _, r := range rows {
		if r.Slug != base {
			continue
		}
		if r.Host == host {
			if normPath(r.Path) == main {
				continue // this very repo, not a collision
			}
			sameHost = append(sameHost, r)
			continue
		}
		otherHost = append(otherHost, r)
	}
	switch {
	case len(sameHost) == 0 && len(otherHost) == 0:
		return base
	case len(sameHost) == 0 && originsAllowAdoption(origin, otherHost):
		return base
	}
	return uniqueProjectSlug(base, host, main, rows)
}

// originsAllowAdoption reports whether a repo with this origin may adopt a
// project already owned on another host. An unknown origin on EITHER side is
// permissive on purpose: a repo with no remote is the common local case, and
// refusing would split a project that a second device is legitimately joining.
// Two known origins that disagree are two different repositories, and those mint
// their own slug.
func originsAllowAdoption(origin string, owners []RepoMapRow) bool {
	mine := normalizeOrigin(origin)
	if mine == "" {
		return true
	}
	for _, r := range owners {
		if o := normalizeOrigin(r.Origin); o == "" || o == mine {
			return true
		}
	}
	return false
}

// normalizeOrigin reduces a git remote URL to a comparable host/path form:
// scheme, user info, a trailing slash and a trailing ".git" all vary between two
// checkouts of the same repository, and the scp-like "git@host:owner/repo" form
// is the same remote as "https://host/owner/repo". Case is folded because
// hosting providers treat it as insignificant. "" stays "" -- unknown, never a
// guess.
func normalizeOrigin(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return ""
	}
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.LastIndex(s, "@"); i >= 0 {
		s = s[i+1:]
	}
	// scp-like host:path -> host/path. A numeric port keeps its colon, so
	// ssh://host:2222/a/b does not turn into a different path.
	if i := strings.IndexByte(s, ':'); i >= 0 {
		rest := s[i+1:]
		seg := rest
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			seg = rest[:j]
		}
		if !isAllDigits(seg) {
			s = s[:i] + "/" + rest
		}
	}
	s = strings.TrimSuffix(s, "/")
	s = strings.TrimSuffix(s, ".git")
	return strings.TrimSuffix(s, "/")
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// deadOwnerPaths returns the map entries owning slug when every one of them
// names a path that no longer exists on disk -- the moved-repo signal. It
// returns nil when nothing owns the slug (no collision at all) or when any
// owning path is still present or merely unknowable (a genuine same-name
// collision, or a stat failure that is not a clean not-exist): only provably
// dead owners justify adopting their project. Sorted, so the surfaced
// event/notice is deterministic.
//
// It is only ever called with the LOCAL host's rows. Stat'ing a remote host's
// path would read this daemon's disk to answer a question about another
// machine's, which is the whole remap hazard host scoping exists to close.
func deadOwnerPaths(slug, root string, m map[string]string) []string {
	root = normPath(root)
	var dead []string
	for path, s := range m {
		if s != slug || normPath(path) == root {
			continue
		}
		if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		dead = append(dead, path)
	}
	slices.Sort(dead)
	return dead
}

// uniqueProjectSlug returns base unless another repo already owns it, in which
// case it appends -2, -3, ... until free. Ownership spans every host: minting a
// slug another machine already uses for a different repository would merge the
// two the moment that machine's agent next registers. The guard keeps a newly
// seen repo whose directory name collides with an existing project (e.g. two
// distinct "backend" repos) from silently inheriting that project's memories;
// the owner can still merge them later with map-repo.
func uniqueProjectSlug(base, host, main string, rows []RepoMapRow) string {
	main = normPath(main)
	takenByOther := func(slug string) bool {
		for _, r := range rows {
			if r.Slug != slug {
				continue
			}
			if r.Host == host && normPath(r.Path) == main {
				continue
			}
			return true
		}
		return false
	}
	if !takenByOther(base) {
		return base
	}
	for i := 2; ; i++ {
		if cand := base + "-" + strconv.Itoa(i); !takenByOther(cand) {
			return cand
		}
	}
}

// matchProjectPath returns the slug of the longest key that is a path-prefix of
// cwd, or "" when none matches. Prefixes must align on a path separator, so
// "/a/foo" never matches key "/a/foobar".
func matchProjectPath(cwd string, m map[string]string) string {
	if cwd == "" || len(m) == 0 {
		return ""
	}
	cwd = normPath(cwd)
	best, bestSlug := "", ""
	for prefix, slug := range m {
		p := normPath(prefix)
		if pathHasPrefix(cwd, p) && len(p) > len(best) {
			best, bestSlug = p, slug
		}
	}
	return bestSlug
}

// pathHasPrefix reports whether path is prefix itself or lies beneath it, with
// the match aligned on a separator boundary. BOTH separators count: a Windows
// client's paths are matched by a daemon whose own filepath.Separator is "/".
func pathHasPrefix(path, prefix string) bool {
	if path == prefix {
		return true
	}
	if prefix == "" || !strings.HasPrefix(path, prefix) {
		return false
	}
	switch path[len(prefix)] {
	case '/', '\\':
		return true
	}
	return false
}

// NormRepoPath is the normalization every repo-map comparison and every stored
// row goes through. It is exported so a caller matching a path against
// RepoMapRows (unmap-repo --host) compares exactly as the store does instead of
// restating the rules.
func NormRepoPath(p string) string { return normPath(p) }

// IsAbsRepoPath reports whether p is absolute on SOME operating system: a POSIX
// path ("/home/me/app"), a Windows drive path ("C:\repos\app", "C:/repos/app")
// or a UNC path ("\\srv\share\app"). It is the test for a path naming a
// directory on another machine, which filepath.IsAbs cannot make: that judges by
// this machine's rules, so a Windows daemon refuses "/home/me/app" and a POSIX
// one refuses "C:\repos\app". A driveless "\repos\app" is relative to a current
// drive, and is refused like any relative path.
func IsAbsRepoPath(p string) bool {
	p = strings.TrimSpace(p)
	switch {
	case strings.HasPrefix(p, "/"), strings.HasPrefix(p, `\\`):
		return true
	case len(p) >= 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/'):
		c := p[0] | 0x20 // ASCII lower case
		return c >= 'a' && c <= 'z'
	}
	return false
}

// normPath puts a repo path in its one comparable form. The rule follows the
// path's own shape, not just this daemon's OS, because a shared daemon stores
// paths from machines that are not it:
//
//   - A POSIX absolute path ("/home/me/app") is path.Clean'd, on every daemon.
//     filepath.Clean on a Windows daemon would rewrite it to "\home\me\app", a
//     path that exists on no machine.
//   - A path containing a backslash is a Windows client's ("C:\repos\app",
//     "\\srv\share\app"), and only loses its trailing separators. filepath.Clean
//     on a POSIX daemon would keep the backslashes while treating the whole
//     string as one segment, which is worse than not normalizing at all.
//   - Anything else -- "C:/repos/app", a "//srv/share" UNC path, a relative
//     path -- is shaped like this daemon's own and is filepath.Clean'd.
//
// Case is NOT folded -- Windows path comparison is case-insensitive and this is
// not, which is a known Phase 1 limit rather than an oversight.
func normPath(p string) string { return normPathWith(filepath.Clean, p) }

// normPathWith is normPath with this daemon's own Clean injected, so a test on
// any OS can run the rules as a Windows daemon applies them.
func normPathWith(ownClean func(string) string, p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if strings.ContainsRune(p, '\\') {
		for len(p) > 1 && (p[len(p)-1] == '\\' || p[len(p)-1] == '/') {
			p = p[:len(p)-1]
		}
		return p
	}
	if p[0] == '/' && !strings.HasPrefix(p, "//") {
		return pathpkg.Clean(p)
	}
	return ownClean(p)
}

// lastSegment is the final path element of p, splitting on BOTH separators so a
// Windows root ("C:\repos\myapp") yields the same directory name on a POSIX
// daemon that it would on its own machine.
func lastSegment(p string) string {
	p = strings.TrimRight(p, `/\`)
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

// pathSlugs projects a snapshot down to one host's {path: slug} map -- the shape
// the prefix matcher works on.
func pathSlugs(rows []RepoMapRow, host string) map[string]string {
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		if r.Host == host {
			out[r.Path] = r.Slug
		}
	}
	return out
}

// repoMapSnapshot loads every repo_map row, folding the legacy repo_project_map
// JSON mirror into the unnamed ("") host bucket -- which is what makes a
// database that has not been adopted yet (a seeded fixture, a test, an install
// mid-upgrade) resolve exactly as it did before the table existed.
//
// A row wins over a mirror entry for the same PATH on any host: the mirror is
// downstream of the rows, so an entry that also has a row is a copy of it rather
// than a second mapping, and folding it in would report a phantom mapping on a
// machine that has already named itself.
func repoMapSnapshot(ctx context.Context, q repoMapExecutor) ([]RepoMapRow, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT host, path, slug, origin, created_at FROM repo_map`)
	if err != nil {
		return nil, fmt.Errorf("store: repo map: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []RepoMapRow
	seen := map[string]struct{}{}
	for rows.Next() {
		var r RepoMapRow
		var created string
		if err := rows.Scan(&r.Host, &r.Path, &r.Slug, &r.Origin, &created); err != nil {
			return nil, fmt.Errorf("store: repo map: %w", err)
		}
		if t, terr := core.ParseTime(created); terr == nil {
			r.CreatedAt = t
		}
		seen[r.Path] = struct{}{}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: repo map: %w", err)
	}
	mirror, err := repoMapMirror(ctx, q)
	if err != nil {
		return nil, err
	}
	for path, slug := range mirror {
		if _, dup := seen[path]; dup {
			continue
		}
		out = append(out, RepoMapRow{Path: path, Slug: slug})
	}
	return out, nil
}

// repoMapMirror decodes the legacy repo_project_map JSON setting. An unset or
// blank value yields an empty map (not an error).
func repoMapMirror(ctx context.Context, q settingsExecutor) (map[string]string, error) {
	raw, found, err := getSettingTx(ctx, q, SettingRepoProjectMap)
	if err != nil {
		return nil, err
	}
	if !found || strings.TrimSpace(raw) == "" {
		return map[string]string{}, nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("store: repo map mirror: decode: %w", err)
	}
	return m, nil
}

// localHostSetting reads the host this daemon adopted. "" means the machine has
// not named itself -- the same bucket the empty host column carries.
func localHostSetting(ctx context.Context, q settingsExecutor) (string, error) {
	v, _, err := getSettingTx(ctx, q, SettingLocalHost)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(v), nil
}

// putRepoMapRow upserts one mapping and keeps the local mirror in step, in one
// transaction (the pool is capped at a single connection, so the whole mutation
// is serialized against concurrent registrars and two agents registering
// different repos at once cannot clobber each other's entry).
func putRepoMapRow(ctx context.Context, db *sql.DB, row RepoMapRow) error {
	return inRepoMapTx(ctx, db, "store.putRepoMapRow", func(tx *sql.Tx) error {
		if err := upsertRepoMapRow(ctx, tx, row); err != nil {
			return err
		}
		return mirrorRepoMapMutation(ctx, tx, row.Host,
			map[string]string{normPath(row.Path): row.Slug}, nil)
	})
}

// adoptRepoMapRows re-points a project slug at a repo's new location: one
// transaction removes the dead rows that owned the slug on that host and records
// the new one in their place, so a concurrent registrar can never observe the
// slug half-moved.
func adoptRepoMapRows(ctx context.Context, db *sql.DB, row RepoMapRow, deadPaths []string) error {
	return inRepoMapTx(ctx, db, "store.adoptRepoMapRows", func(tx *sql.Tx) error {
		for _, p := range deadPaths {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM repo_map WHERE host = ? AND path = ?`, row.Host, p); err != nil {
				return err
			}
		}
		if err := upsertRepoMapRow(ctx, tx, row); err != nil {
			return err
		}
		return mirrorRepoMapMutation(ctx, tx, row.Host,
			map[string]string{normPath(row.Path): row.Slug}, deadPaths)
	})
}

func inRepoMapTx(ctx context.Context, db *sql.DB, op string, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("%s: begin: %w", op, err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit
	if err := fn(tx); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s: commit: %w", op, err)
	}
	return nil
}

func upsertRepoMapRow(ctx context.Context, tx *sql.Tx, row RepoMapRow) error {
	path := normPath(row.Path)
	if path == "" || row.Slug == "" {
		return fmt.Errorf("repo map row needs a path and a slug")
	}
	created := row.CreatedAt
	if created.IsZero() {
		created = time.Now().UTC()
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO repo_map (host, path, slug, origin, created_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(host, path) DO UPDATE SET
			slug = excluded.slug,
			-- A later registration that could not resolve the origin must not
			-- erase one an earlier one did.
			origin = CASE WHEN excluded.origin = '' THEN repo_map.origin ELSE excluded.origin END`,
		row.Host, path, row.Slug, strings.TrimSpace(row.Origin), core.FormatTime(created))
	return err
}

// mirrorRepoMapMutation applies the same add/delete to the legacy JSON mirror
// when the mutated host is the local one, and leaves it untouched otherwise. The
// mirror is MUTATED rather than regenerated so an entry that only ever existed
// there (a hand-written setting, a fixture seed) survives a registration that
// has nothing to do with it.
func mirrorRepoMapMutation(ctx context.Context, tx *sql.Tx, host string, add map[string]string, del []string) error {
	local, err := localHostSetting(ctx, tx)
	if err != nil {
		return err
	}
	if host != local {
		return nil
	}
	m, err := repoMapMirror(ctx, tx)
	if err != nil {
		return err
	}
	changed := false
	for _, p := range del {
		if _, ok := m[p]; ok {
			delete(m, p)
			changed = true
		}
	}
	for p, slug := range add {
		if m[p] != slug {
			m[p] = slug
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return writeRepoMapMirror(ctx, tx, m)
}

// rewriteRepoMapMirror replaces the mirror with exactly one host's rows. Only
// AdoptLocalHost does this: after the import, the rows ARE the local view, and
// regenerating is what drops an entry a previous install left behind.
func rewriteRepoMapMirror(ctx context.Context, tx *sql.Tx, host string) error {
	rows, err := tx.QueryContext(ctx, `SELECT path, slug FROM repo_map WHERE host = ?`, host)
	if err != nil {
		return fmt.Errorf("repo map mirror: %w", err)
	}
	defer func() { _ = rows.Close() }()
	m := map[string]string{}
	for rows.Next() {
		var path, slug string
		if err := rows.Scan(&path, &slug); err != nil {
			return fmt.Errorf("repo map mirror: %w", err)
		}
		m[path] = slug
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("repo map mirror: %w", err)
	}
	return writeRepoMapMirror(ctx, tx, m)
}

func writeRepoMapMirror(ctx context.Context, tx *sql.Tx, m map[string]string) error {
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("repo map mirror: %w", err)
	}
	return setSettingTx(ctx, tx, SettingRepoProjectMap, string(b))
}
