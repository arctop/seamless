package store

// Repo locks (plan repo-locks, migration 047): the registry of locks on
// repositories' refs, keyed by the repository's identity rather than by a
// project (see the migration and internal/core/locks.go).
//
// Holding a lock is the task claim's machinery, not a copy with its own rules:
// the same compare-and-set (ClaimTask's), the same boundary (core.RepoLock.
// HoldLive is Task.ClaimLive's rule, so a lease expiring at exactly now is dead
// and the SQL takes it with lease_expires_at <= now), the same way a hold ends
// (its holder's release, the owner's force-release, the holder's session
// ending, the lease lapsing -- lazily, inside the next acquire). Every column a
// hold writes reads '' (or NULL, for the times) once the lock is free.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
)

// ErrRepoLockNotFound is a lock id, or a repository identity and name, that
// names no lock.
var ErrRepoLockNotFound = errors.New("lock not found")

// ErrRepoLockHeld is an acquire refused because another session's hold is
// live. The refusal carries the lock as it stands, so a surface can name the
// holder, since and until to a caller allowed to see them.
var ErrRepoLockHeld = errors.New("lock held")

// ErrRepoLockRequested is a hold, or the owner principal's declaration, of a
// lock that is still a restricted account's declaration: the owner approves or
// discards it on the console first.
var ErrRepoLockRequested = errors.New("lock waits for the owner's approval")

// ErrRepoLockNotHolder is a release by a session that does not hold the lock.
var ErrRepoLockNotHolder = errors.New("lock not held by this session")

// ErrRepoLockFree is a force-release of a lock nobody holds.
var ErrRepoLockFree = errors.New("lock not held")

// ErrRepoLockNotRequested is an approve or a discard of a lock that is not a
// waiting declaration: it is already active.
var ErrRepoLockNotRequested = errors.New("lock is not a waiting declaration")

// ErrRepoLockLease is a hold's lease that is not positive or exceeds the
// ceiling a task claim's lease has too (core.MaxClaimLease), so a hold whose
// holder vanished frees itself within hours, not days.
var ErrRepoLockLease = errors.New("invalid lock lease")

// ErrRepoLockIdentity is a repository identity no alias can be derived from,
// or one carrying a value no alias may hold.
var ErrRepoLockIdentity = errors.New("no repository identity")

// lkMaxIdentityValue bounds a path or an origin a lock's alias is built from.
const lkMaxIdentityValue = 4096

// RepoLockDeclaration is one declaration of a lock, as DeclareRepoLock stores
// it. Refs and Projects are validated by the caller (core.ValidRepoLockRef,
// existing projects the caller may write); DeclareRepoLock refuses what it can
// check itself.
type RepoLockDeclaration struct {
	Name string
	// Refs are the guarded refs; empty means core.DefaultRepoLockRefs(Name).
	Refs []string
	// Aliases are the declaring checkout's identities, in resolution order
	// (RepoLockAliasesFor). At least one.
	Aliases []core.RepoLockAlias
	// Projects are the projects whose restricted accounts may hold the lock.
	Projects []string
	// State is active for the owner principal's declaration, requested for a
	// restricted account's.
	State core.RepoLockState
	// RecordAliases lets a declaration that resolves to an existing lock add
	// the aliases it brings (the owner principal's). A restricted account's
	// resolves and adds nothing.
	RecordAliases bool
	CreatedBy     string
	Account       string
}

// RepoLockDeclared is what a declaration did.
type RepoLockDeclared struct {
	Lock core.RepoLock
	// Created is true when the declaration stored a new lock (or request);
	// false when it resolved to an existing one, whose refs and projects it
	// left as they were.
	Created bool
	// Added are the aliases this declaration recorded.
	Added []core.RepoLockAlias
}

// RepoLockAcquired reports a successful AcquireRepoLock. Heartbeat is true when
// the session already held the lock and refreshed its lease. Reclaimed is true
// when a different session's lapsed hold was taken; PriorHolder then names that
// session (ClaimResult's Reclaimed and PriorHolder).
type RepoLockAcquired struct {
	Lock        core.RepoLock
	Heartbeat   bool
	Reclaimed   bool
	PriorHolder string
}

// RepoLockForceReleased reports a force-release: the lock as it now stands,
// free, and the session whose hold the owner ended.
type RepoLockForceReleased struct {
	Lock        core.RepoLock
	PriorHolder string
}

// RepoLockAliasesFor derives the aliases a checkout's identity resolves a lock
// by, in resolution order: the origin first -- a URL's normalized form, or, for
// a side clone whose origin is a local path, that path's (host, path) -- then
// the checkout's own (host, main checkout root). It reads nothing from any
// disk: the identity is the client's (seam lock, or the MCP arguments), filled
// from the daemon's own filesystem only by a caller on the daemon's machine.
// An identity that names no host is the daemon's own (localHost). It refuses an
// identity that yields no alias, and a value no alias may hold.
func RepoLockAliasesFor(id CWDIdentity, localHost string) ([]core.RepoLockAlias, error) {
	host := strings.ToLower(strings.TrimSpace(id.Host))
	if host == "" {
		host = strings.ToLower(strings.TrimSpace(localHost))
	}
	for _, v := range []struct{ field, value string }{
		{"host", host}, {"cwd", id.CWD}, {"repo_root", id.RepoRoot}, {"main_worktree_root", id.MainRoot}, {"repo_origin", id.Origin},
	} {
		if err := lkCheckIdentityValue(v.field, v.value); err != nil {
			return nil, err
		}
	}
	if strings.ContainsRune(host, ':') {
		return nil, fmt.Errorf("%w: host %q holds a ':'", ErrRepoLockIdentity, host)
	}
	for _, p := range []struct{ field, value string }{{"repo_root", id.RepoRoot}, {"main_worktree_root", id.MainRoot}} {
		if strings.TrimSpace(p.value) != "" && !IsAbsRepoPath(p.value) {
			return nil, fmt.Errorf("%w: %s %q is not an absolute path", ErrRepoLockIdentity, p.field, p.value)
		}
	}
	main := normPath(id.MainRoot)
	if main == "" {
		main = normPath(id.RepoRoot)
	}
	var out []core.RepoLockAlias
	add := func(a core.RepoLockAlias) {
		if a.Alias != "" && !slices.ContainsFunc(out, func(b core.RepoLockAlias) bool { return b.Alias == a.Alias }) {
			out = append(out, a)
		}
	}
	if origin := strings.TrimSpace(id.Origin); origin != "" {
		if p, local := core.LocalOriginPath(origin); local {
			if !IsAbsRepoPath(p) && main != "" {
				p = lkJoin(main, p)
			}
			if IsAbsRepoPath(p) {
				add(lkPathAlias(host, p))
			}
		} else {
			add(core.RepoLockAlias{Kind: core.RepoLockAliasOrigin, Alias: normalizeOrigin(origin)})
		}
	}
	if main != "" {
		add(lkPathAlias(host, main))
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: name the repository with repo_root (and main_worktree_root, repo_origin) "+
			"resolved on its own machine, or a repo_origin", ErrRepoLockIdentity)
	}
	return out, nil
}

// lkPathAlias is the (host, main checkout root) alias.
func lkPathAlias(host, path string) core.RepoLockAlias {
	return core.RepoLockAlias{Kind: core.RepoLockAliasPath, Alias: host + ":" + normPath(path)}
}

// lkJoin joins a relative path to base in base's own spelling: a Windows
// client's checkout root takes a backslash, any other a slash. It is lexical:
// nothing is read from a disk.
func lkJoin(base, rel string) string {
	if !strings.ContainsRune(base, '\\') {
		return normPath(strings.TrimRight(base, "/") + "/" + rel)
	}
	return lkCleanWindows(strings.TrimRight(base, `/\`) + `\` + strings.ReplaceAll(rel, "/", `\`))
}

// lkCleanWindows resolves "." and ".." in a Windows path lexically, as
// path.Clean does a POSIX one (normPath only trims a Windows path's trailing
// separators): the drive ("C:\") or the share ("\\srv\share\") stays, and a ".."
// never climbs above it.
func lkCleanWindows(p string) string {
	prefix, rest := "", p
	switch {
	case isDrivePath(p):
		prefix, rest = p[:3], p[3:]
	case strings.HasPrefix(p, `\\`):
		parts := strings.SplitN(p[2:], `\`, 3)
		if len(parts) < 2 {
			return normPath(p)
		}
		prefix = `\\` + parts[0] + `\` + parts[1] + `\`
		rest = ""
		if len(parts) == 3 {
			rest = parts[2]
		}
	}
	var out []string
	for seg := range strings.SplitSeq(rest, `\`) {
		switch seg {
		case "", ".":
		case "..":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
		default:
			out = append(out, seg)
		}
	}
	return normPath(prefix + strings.Join(out, `\`))
}

// lkCheckIdentityValue refuses a value no alias may hold: one longer than
// lkMaxIdentityValue, or one carrying a control or format character.
func lkCheckIdentityValue(field, value string) error {
	if len(value) > lkMaxIdentityValue {
		return fmt.Errorf("%w: %s is longer than %d bytes", ErrRepoLockIdentity, field, lkMaxIdentityValue)
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == 0xfeff || (r >= 0x200b && r <= 0x200f) ||
			(r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
			return fmt.Errorf("%w: %s holds a control or format character", ErrRepoLockIdentity, field)
		}
	}
	return nil
}

// lkLockCols is the locks columns every reader scans, in lkScanLock's order.
const lkLockCols = `id, name, refs, state, holder, held_since, lease_expires_at, created_by, account, created_at, updated_at`

// lkScanner is the Scan shared by *sql.Row and *sql.Rows.
type lkScanner interface{ Scan(dest ...any) error }

// lkScanLock reads one locks row (without its aliases and projects).
func lkScanLock(sc lkScanner) (core.RepoLock, error) {
	var (
		l                   core.RepoLock
		refs, state         string
		since, until        sql.NullString
		created, updated    string
		parseErr, decodeErr error
	)
	if err := sc.Scan(&l.ID, &l.Name, &refs, &state, &l.Holder, &since, &until, &l.CreatedBy, &l.Account, &created, &updated); err != nil {
		return core.RepoLock{}, err
	}
	l.State = core.RepoLockState(state)
	if decodeErr = json.Unmarshal([]byte(refs), &l.Refs); decodeErr != nil {
		return core.RepoLock{}, fmt.Errorf("store: lock %s: refs: %w", l.ID, decodeErr)
	}
	stamp := func(ns sql.NullString) *time.Time {
		if !ns.Valid || ns.String == "" || parseErr != nil {
			return nil
		}
		t, err := core.ParseTime(ns.String)
		if err != nil {
			parseErr = err
			return nil
		}
		return &t
	}
	l.HeldSince, l.LeaseExpiresAt = stamp(since), stamp(until)
	if c := stamp(sql.NullString{String: created, Valid: true}); c != nil {
		l.CreatedAt = *c
	}
	if u := stamp(sql.NullString{String: updated, Valid: true}); u != nil {
		l.UpdatedAt = *u
	}
	if parseErr != nil {
		return core.RepoLock{}, fmt.Errorf("store: lock %s: time: %w", l.ID, parseErr)
	}
	return l, nil
}

// lkQuerier is the read and write subset shared by *sql.DB and *sql.Tx.
type lkQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// ListRepoLocks returns every lock, waiting declarations included, oldest
// first, each with its aliases (origins first) and its listed projects.
func ListRepoLocks(ctx context.Context, db *sql.DB) ([]core.RepoLock, error) {
	locks, err := lkLocks(ctx, db, `SELECT `+lkLockCols+` FROM repo_locks ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("store.ListRepoLocks: %w", err)
	}
	return locks, nil
}

// RepoLockByID returns one lock with its aliases and projects; found is false
// when the id names none.
func RepoLockByID(ctx context.Context, db *sql.DB, id string) (core.RepoLock, bool, error) {
	l, ok, err := lkLockByID(ctx, db, id)
	if err != nil {
		return core.RepoLock{}, false, fmt.Errorf("store.RepoLockByID: %w", err)
	}
	return l, ok, nil
}

func lkLockByID(ctx context.Context, q lkQuerier, id string) (core.RepoLock, bool, error) {
	locks, err := lkLocks(ctx, q, `SELECT `+lkLockCols+` FROM repo_locks WHERE id = ?`, id)
	if err != nil || len(locks) == 0 {
		return core.RepoLock{}, false, err
	}
	return locks[0], true, nil
}

// lkLocks runs a locks query and attaches each row's aliases and projects.
func lkLocks(ctx context.Context, q lkQuerier, query string, args ...any) ([]core.RepoLock, error) {
	out, err := lkLockRows(ctx, q, query, args...)
	if err != nil || len(out) == 0 {
		return out, err
	}
	at := make(map[string]int, len(out))
	ids := make([]any, 0, len(out))
	for i, l := range out {
		at[l.ID] = i
		ids = append(ids, l.ID)
	}
	in := "(" + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + ")"
	arows, err := q.QueryContext(ctx, `SELECT lock_id, kind, alias, created_at FROM repo_lock_aliases WHERE lock_id IN `+in+`
		ORDER BY CASE kind WHEN 'origin' THEN 0 ELSE 1 END, created_at, alias`, ids...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = arows.Close() }()
	for arows.Next() {
		var lockID, kind, alias, created string
		if err := arows.Scan(&lockID, &kind, &alias, &created); err != nil {
			return nil, err
		}
		t, err := core.ParseTime(created)
		if err != nil {
			return nil, fmt.Errorf("lock %s: alias time: %w", lockID, err)
		}
		if i, ok := at[lockID]; ok {
			out[i].Aliases = append(out[i].Aliases, core.RepoLockAlias{Kind: core.RepoLockAliasKind(kind), Alias: alias, CreatedAt: t})
		}
	}
	if err := arows.Err(); err != nil {
		return nil, err
	}
	prows, err := q.QueryContext(ctx, `SELECT lock_id, project_slug FROM repo_lock_projects WHERE lock_id IN `+in+`
		ORDER BY project_slug`, ids...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = prows.Close() }()
	for prows.Next() {
		var lockID, slug string
		if err := prows.Scan(&lockID, &slug); err != nil {
			return nil, err
		}
		if i, ok := at[lockID]; ok {
			out[i].Projects = append(out[i].Projects, slug)
		}
	}
	if err := prows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// lkLockRows runs a locks query, without the aliases and projects. The rows
// are closed before it returns, so the caller may query again on a
// one-connection pool.
func lkLockRows(ctx context.Context, q lkQuerier, query string, args ...any) ([]core.RepoLock, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []core.RepoLock{}
	for rows.Next() {
		l, err := lkScanLock(rows)
		if err != nil {
			return nil, err
		}
		l.Aliases, l.Projects = []core.RepoLockAlias{}, []string{}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ResolveRepoLock returns the lock named name that the first of aliases (in
// resolution order) resolves to. An origin alias wins over a path alias, so a
// checkout keys on its repository's origin wherever one is known, and on its
// host and path only where none is (owner decision D5). found is false when no
// alias resolves.
func ResolveRepoLock(ctx context.Context, db *sql.DB, aliases []core.RepoLockAlias, name string) (core.RepoLock, bool, error) {
	l, ok, err := lkResolve(ctx, db, aliases, name)
	if err != nil {
		return core.RepoLock{}, false, fmt.Errorf("store.ResolveRepoLock: %w", err)
	}
	return l, ok, nil
}

func lkResolve(ctx context.Context, q lkQuerier, aliases []core.RepoLockAlias, name string) (core.RepoLock, bool, error) {
	for _, a := range aliases {
		var id string
		err := q.QueryRowContext(ctx, `SELECT lock_id FROM repo_lock_aliases WHERE alias = ? AND name = ?`, a.Alias, name).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return core.RepoLock{}, false, err
		}
		l, ok, err := lkLockByID(ctx, q, id)
		if err != nil {
			return core.RepoLock{}, false, err
		}
		if ok {
			return l, true, nil
		}
	}
	return core.RepoLock{}, false, nil
}

// RepoLocksFor returns every lock, of any name, that one of aliases resolves
// to: a repository's locks. Oldest first.
func RepoLocksFor(ctx context.Context, db *sql.DB, aliases []core.RepoLockAlias) ([]core.RepoLock, error) {
	if len(aliases) == 0 {
		return []core.RepoLock{}, nil
	}
	args := make([]any, 0, len(aliases))
	for _, a := range aliases {
		args = append(args, a.Alias)
	}
	in := "(" + strings.TrimSuffix(strings.Repeat("?,", len(args)), ",") + ")"
	locks, err := lkLocks(ctx, db, `SELECT `+lkLockCols+` FROM repo_locks
		WHERE id IN (SELECT lock_id FROM repo_lock_aliases WHERE alias IN `+in+`) ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("store.RepoLocksFor: %w", err)
	}
	return locks, nil
}

// CountRepoLocks reports how many locks exist, waiting declarations included,
// and how many of them wait for the owner.
func CountRepoLocks(ctx context.Context, db *sql.DB) (total, requested int, err error) {
	err = db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(state = 'requested'), 0) FROM repo_locks`).Scan(&total, &requested)
	if err != nil {
		return 0, 0, fmt.Errorf("store.CountRepoLocks: %w", err)
	}
	return total, requested, nil
}

// lkCheckDeclaration refuses a declaration DeclareRepoLock cannot store as it
// is: a name or a ref that is not valid, too many refs, no alias, an unknown
// state, or a malformed project slug. It returns the refs to store, defaults
// applied and duplicates dropped, in the declaration's order.
func lkCheckDeclaration(d RepoLockDeclaration) ([]string, error) {
	if err := core.ValidRepoLockName(d.Name); err != nil {
		return nil, err
	}
	if !d.State.Valid() {
		return nil, fmt.Errorf("invalid lock state %q", d.State)
	}
	if len(d.Aliases) == 0 {
		return nil, ErrRepoLockIdentity
	}
	for _, a := range d.Aliases {
		if !slices.Contains(core.RepoLockAliasKinds, a.Kind) || a.Alias == "" {
			return nil, fmt.Errorf("%w: alias %q of kind %q", ErrRepoLockIdentity, a.Alias, a.Kind)
		}
	}
	refs := d.Refs
	if len(refs) == 0 {
		refs = core.DefaultRepoLockRefs(d.Name)
	}
	var out []string
	for _, r := range refs {
		if err := core.ValidRepoLockRef(r); err != nil {
			return nil, err
		}
		if !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	if len(out) > core.MaxRepoLockRefs {
		return nil, fmt.Errorf("%w: %d refs, and a lock guards at most %d", core.ErrRepoLockRef, len(out), core.MaxRepoLockRefs)
	}
	for _, p := range d.Projects {
		if p == "" || strings.ContainsAny(p, "/\\\x00") {
			return nil, fmt.Errorf("invalid project %q", p)
		}
	}
	return out, nil
}

// errLkRaced is a declaration whose new lock lost its primary alias to a
// declaration that committed after this one looked: it is retried once, and
// then resolves to that lock.
var errLkRaced = errors.New("store: a concurrent declaration took the alias")

// DeclareRepoLock declares a lock on the repository the declaration's aliases
// name, once per (repository, name) (owner decision D7). A declaration that
// resolves to an existing lock -- from any project, any host -- changes none of
// its refs, state or projects: it returns that lock and, for the owner
// principal (RecordAliases), records the aliases it brings that no lock of this
// name has yet, so a repository that gained or changed its origin keeps its
// lock. One that resolves to nothing stores a new lock in the declaration's
// state: active for the owner principal, requested for a restricted account.
//
// The owner principal's declaration of a lock that still waits for the owner
// is refused (ErrRepoLockRequested): the console's approve and discard are the
// owner's decision on a restricted account's declaration, never a side effect
// of another one.
func DeclareRepoLock(ctx context.Context, db *sql.DB, d RepoLockDeclaration, now time.Time) (RepoLockDeclared, error) {
	refs, err := lkCheckDeclaration(d)
	if err != nil {
		return RepoLockDeclared{}, fmt.Errorf("store.DeclareRepoLock: %w", err)
	}
	out, err := lkDeclare(ctx, db, d, refs, now)
	if errors.Is(err, errLkRaced) {
		out, err = lkDeclare(ctx, db, d, refs, now)
	}
	if err != nil {
		return out, fmt.Errorf("store.DeclareRepoLock: %w", err)
	}
	return out, nil
}

func lkDeclare(ctx context.Context, db *sql.DB, d RepoLockDeclaration, refs []string, now time.Time) (RepoLockDeclared, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return RepoLockDeclared{}, err
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit
	nowStr := core.FormatTime(now.UTC())

	existing, found, err := lkResolve(ctx, tx, d.Aliases, d.Name)
	if err != nil {
		return RepoLockDeclared{}, err
	}
	if found {
		if existing.State == core.RepoLockRequested && d.State == core.RepoLockActive {
			return RepoLockDeclared{Lock: existing}, fmt.Errorf("%w: lock %s (%s) is a restricted account's declaration: "+
				"approve or discard it on the console's Repo locks page", ErrRepoLockRequested, existing.ID, existing.Label())
		}
		var added []core.RepoLockAlias
		if d.RecordAliases {
			if added, err = lkRecordAliases(ctx, tx, existing.ID, existing.Name, d.Aliases, nowStr); err != nil {
				return RepoLockDeclared{}, err
			}
			if len(added) > 0 {
				if _, err := tx.ExecContext(ctx, `UPDATE repo_locks SET updated_at = ? WHERE id = ?`, nowStr, existing.ID); err != nil {
					return RepoLockDeclared{}, err
				}
			}
		}
		l, ok, err := lkLockByID(ctx, tx, existing.ID)
		if err != nil {
			return RepoLockDeclared{}, err
		}
		if !ok {
			return RepoLockDeclared{}, fmt.Errorf("%w: %s vanished during the declaration", ErrRepoLockNotFound, existing.ID)
		}
		if err := tx.Commit(); err != nil {
			return RepoLockDeclared{}, err
		}
		return RepoLockDeclared{Lock: l, Added: added}, nil
	}

	id, err := core.NewID()
	if err != nil {
		return RepoLockDeclared{}, err
	}
	rawRefs, err := json.Marshal(refs)
	if err != nil {
		return RepoLockDeclared{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO repo_locks (id, name, refs, state, created_by, account, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, id, d.Name, string(rawRefs), string(d.State), d.CreatedBy, d.Account, nowStr, nowStr); err != nil {
		return RepoLockDeclared{}, err
	}
	added, err := lkRecordAliases(ctx, tx, id, d.Name, d.Aliases, nowStr)
	if err != nil {
		return RepoLockDeclared{}, err
	}
	// The primary alias is the lock's identity: had another lock of this name
	// taken it after the resolve above, this would be the second lock on one
	// repository. Roll back; the retry resolves to that lock instead.
	if len(added) == 0 || added[0].Alias != d.Aliases[0].Alias {
		return RepoLockDeclared{}, errLkRaced
	}
	for _, p := range d.Projects {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO repo_lock_projects (lock_id, project_slug) VALUES (?, ?)`, id, p); err != nil {
			return RepoLockDeclared{}, err
		}
	}
	l, ok, err := lkLockByID(ctx, tx, id)
	if err != nil {
		return RepoLockDeclared{}, err
	}
	if !ok {
		return RepoLockDeclared{}, fmt.Errorf("%w: %s, just stored", ErrRepoLockNotFound, id)
	}
	if err := tx.Commit(); err != nil {
		return RepoLockDeclared{}, err
	}
	return RepoLockDeclared{Lock: l, Created: true, Added: added}, nil
}

// lkRecordAliases adds each alias a lock of this name does not have yet,
// leaving one another lock of this name already holds where it is, and
// returns the ones it added, in order.
func lkRecordAliases(ctx context.Context, q lkQuerier, lockID, name string, aliases []core.RepoLockAlias, nowStr string) ([]core.RepoLockAlias, error) {
	var added []core.RepoLockAlias
	for _, a := range aliases {
		res, err := q.ExecContext(ctx, `INSERT OR IGNORE INTO repo_lock_aliases (lock_id, name, kind, alias, created_at)
			VALUES (?, ?, ?, ?, ?)`, lockID, name, string(a.Kind), a.Alias, nowStr)
		if err != nil {
			return nil, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("alias rows affected: %w", err)
		}
		if n == 1 {
			t, err := core.ParseTime(nowStr)
			if err != nil {
				return nil, err
			}
			added = append(added, core.RepoLockAlias{Kind: a.Kind, Alias: a.Alias, CreatedAt: t})
		}
	}
	return added, nil
}

// RecordRepoLockAliases records, for a lock a checkout resolved to, the
// aliases of that checkout the lock does not have yet -- every checkout seen,
// so it keeps the lock when its origin changes. An alias another lock of the
// same name holds stays with that lock. It returns the aliases it added.
func RecordRepoLockAliases(ctx context.Context, db *sql.DB, lockID string, aliases []core.RepoLockAlias, now time.Time) ([]core.RepoLockAlias, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store.RecordRepoLockAliases: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit
	l, ok, err := lkLockByID(ctx, tx, lockID)
	if err != nil {
		return nil, fmt.Errorf("store.RecordRepoLockAliases: %w", err)
	}
	if !ok {
		return nil, fmt.Errorf("store.RecordRepoLockAliases: %w: %q", ErrRepoLockNotFound, lockID)
	}
	nowStr := core.FormatTime(now.UTC())
	added, err := lkRecordAliases(ctx, tx, l.ID, l.Name, aliases, nowStr)
	if err != nil {
		return nil, fmt.Errorf("store.RecordRepoLockAliases: %w", err)
	}
	if len(added) > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE repo_locks SET updated_at = ? WHERE id = ?`, nowStr, l.ID); err != nil {
			return nil, fmt.Errorf("store.RecordRepoLockAliases: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store.RecordRepoLockAliases: commit: %w", err)
	}
	return added, nil
}

// AcquireRepoLock takes the lock for sessionID, or refreshes the lease of the
// session already holding it (a heartbeat), stamping a lease that lapses at
// now+lease. It is ClaimTask's compare-and-set: the write lands only on an
// active lock that is
//
//	(a) held by sessionID (a heartbeat; its held_since stays), or
//	(b) free (holder ''), or
//	(c) held by a hold that is not live -- no lease stamped, or a lapsed one
//	    (lease_expires_at <= now): a take from a dead holder.
//
// (b) and (c) are exactly core.RepoLock.HoldLive's negation, as ClaimTask's
// branches are Task.ClaimLive's: After is strict, so a lease expiring at
// exactly now is already dead, and the SQL takes it with <=. Expiry is lazy --
// there is no sweeper. Otherwise it returns ErrRepoLockHeld (another session's
// live hold, with the lock as it stands), ErrRepoLockRequested (a declaration
// waiting for the owner), or ErrRepoLockNotFound. lease must be positive and
// at most the ceiling (ErrRepoLockLease).
func AcquireRepoLock(ctx context.Context, db *sql.DB, id, sessionID string, lease time.Duration, now time.Time) (RepoLockAcquired, error) {
	if sessionID == "" {
		return RepoLockAcquired{}, errors.New("store.AcquireRepoLock: empty session id")
	}
	if lease <= 0 || lease > core.MaxClaimLease {
		return RepoLockAcquired{}, fmt.Errorf("store.AcquireRepoLock: %w: %s, and a lease is at least a second and at most %s",
			ErrRepoLockLease, lease, core.MaxClaimLease)
	}
	// One transaction, as ClaimTask's: the prior holder's read, the
	// compare-and-set and the lost-hold notice land together, so a reclaim
	// names exactly the hold this write ended.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return RepoLockAcquired{}, fmt.Errorf("store.AcquireRepoLock: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	prior, ok, err := lkLockByID(ctx, tx, id)
	if err != nil {
		return RepoLockAcquired{}, fmt.Errorf("store.AcquireRepoLock: %w", err)
	}
	if !ok {
		return RepoLockAcquired{}, fmt.Errorf("store.AcquireRepoLock: %w: %q", ErrRepoLockNotFound, id)
	}
	nowStr := core.FormatTime(now.UTC())
	expStr := core.FormatTime(now.Add(lease).UTC())
	// SET reads the row as it was, so held_since stays only for the session
	// that already holds it.
	res, err := tx.ExecContext(ctx, `
		UPDATE repo_locks
		   SET holder = ?, lease_expires_at = ?, updated_at = ?,
		       held_since = CASE WHEN holder = ? AND held_since IS NOT NULL THEN held_since ELSE ? END
		 WHERE id = ? AND state = 'active'
		   AND (
		          holder = ?
		       OR holder = ''
		       OR lease_expires_at IS NULL
		       OR lease_expires_at <= ?)`,
		sessionID, expStr, nowStr, sessionID, nowStr, id, sessionID, nowStr)
	if err != nil {
		return RepoLockAcquired{}, fmt.Errorf("store.AcquireRepoLock: %w", err)
	}
	// A zero is the compare-and-set losing, which reads as another session
	// holding the lock: a driver failure must not reach that answer.
	n, err := res.RowsAffected()
	if err != nil {
		return RepoLockAcquired{}, fmt.Errorf("store.AcquireRepoLock: rows affected: %w", err)
	}
	if n == 0 {
		// Give the one connection back before the diagnosis reads on the pool.
		_ = tx.Rollback()
		return lkAcquireRefusal(ctx, db, id, now)
	}
	// Only a take from a real prior holder counts as a reclaim: a free lock
	// was taken from nobody. The holder it was taken from hears of it once
	// (ClaimLostReclaimed), as a task claim's does.
	reclaimed := prior.Holder != "" && prior.Holder != sessionID
	if reclaimed {
		if err := RecordClaimNotice(ctx, tx, ClaimNotice{
			SessionID: prior.Holder, Kind: ClaimResourceLock, ResourceID: id, Label: prior.Name,
			NewHolder: sessionID, Reason: ClaimLostReclaimed, LeaseExpiresAt: prior.LeaseExpiresAt, LostAt: now,
		}); err != nil {
			return RepoLockAcquired{}, fmt.Errorf("store.AcquireRepoLock: %w", err)
		}
	}
	updated, ok, err := lkLockByID(ctx, tx, id)
	if err != nil {
		return RepoLockAcquired{}, fmt.Errorf("store.AcquireRepoLock: %w", err)
	}
	if !ok {
		return RepoLockAcquired{}, fmt.Errorf("store.AcquireRepoLock: %w: %q", ErrRepoLockNotFound, id)
	}
	if err := tx.Commit(); err != nil {
		return RepoLockAcquired{}, fmt.Errorf("store.AcquireRepoLock: commit: %w", err)
	}
	out := RepoLockAcquired{Lock: updated, Heartbeat: prior.Holder == sessionID, Reclaimed: reclaimed}
	if reclaimed {
		out.PriorHolder = prior.Holder
	}
	return out, nil
}

// lkAcquireRefusal diagnoses an acquire whose compare-and-set missed, from the
// row as it stands now (the snapshot before the write is what a racing winner
// just invalidated). HoldLive stays the one held/not-held authority here.
func lkAcquireRefusal(ctx context.Context, db *sql.DB, id string, now time.Time) (RepoLockAcquired, error) {
	cur, ok, err := lkLockByID(ctx, db, id)
	switch {
	case err != nil:
		return RepoLockAcquired{}, fmt.Errorf("store.AcquireRepoLock: %w", err)
	case !ok:
		return RepoLockAcquired{}, fmt.Errorf("store.AcquireRepoLock: %w: %q", ErrRepoLockNotFound, id)
	case cur.State == core.RepoLockRequested:
		return RepoLockAcquired{Lock: cur}, fmt.Errorf("store.AcquireRepoLock: %w: lock %q", ErrRepoLockRequested, id)
	case cur.HoldLive(now):
		return RepoLockAcquired{Lock: cur}, fmt.Errorf("store.AcquireRepoLock: %w: lock %q held by %q", ErrRepoLockHeld, id, cur.Holder)
	}
	// The row changed between the write and this look (a holder released, a
	// lease lapsed): report the transient loss rather than invent a cause.
	return RepoLockAcquired{Lock: cur}, fmt.Errorf("store.AcquireRepoLock: %w: lock %q changed during the acquire; retry",
		ErrRepoLockHeld, id)
}

// ReleaseRepoLock lets go of a lock sessionID holds, live or lapsed, leaving it
// free. Only the holder may (ErrRepoLockNotHolder); ErrRepoLockNotFound when
// the id names no lock.
func ReleaseRepoLock(ctx context.Context, db *sql.DB, id, sessionID string, now time.Time) (core.RepoLock, error) {
	if sessionID == "" {
		return core.RepoLock{}, errors.New("store.ReleaseRepoLock: empty session id")
	}
	res, err := db.ExecContext(ctx, `
		UPDATE repo_locks SET holder = '', held_since = NULL, lease_expires_at = NULL, updated_at = ?
		 WHERE id = ? AND holder = ? AND state = 'active'`,
		core.FormatTime(now.UTC()), id, sessionID)
	if err != nil {
		return core.RepoLock{}, fmt.Errorf("store.ReleaseRepoLock: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return core.RepoLock{}, fmt.Errorf("store.ReleaseRepoLock: rows affected: %w", err)
	}
	cur, ok, err := lkLockByID(ctx, db, id)
	if err != nil {
		return core.RepoLock{}, fmt.Errorf("store.ReleaseRepoLock: %w", err)
	}
	if !ok {
		return core.RepoLock{}, fmt.Errorf("store.ReleaseRepoLock: %w: %q", ErrRepoLockNotFound, id)
	}
	if n == 0 {
		return cur, fmt.Errorf("store.ReleaseRepoLock: %w: lock %q, session %q", ErrRepoLockNotHolder, id, sessionID)
	}
	return cur, nil
}

// lkForceTries bounds ForceReleaseRepoLock's compare-and-set retries: each
// retry follows a hold that changed hands between its look and its write.
const lkForceTries = 4

// ForceReleaseRepoLock ends whatever hold the lock carries, live or lapsed,
// whoever holds it: the owner's override (the console's force-release and
// `seam lock release --force`), which no agent tool reaches. It returns the
// lock, free, and the session whose hold it ended. ErrRepoLockFree when nobody
// holds it, ErrRepoLockNotFound when the id names no lock.
func ForceReleaseRepoLock(ctx context.Context, db *sql.DB, id string, now time.Time) (RepoLockForceReleased, error) {
	for range lkForceTries {
		out, done, err := lkForceReleaseOnce(ctx, db, id, now)
		if err != nil || done {
			return out, err
		}
	}
	return RepoLockForceReleased{}, fmt.Errorf("store.ForceReleaseRepoLock: lock %q changed hands %d times during the release; retry",
		id, lkForceTries)
}

// lkForceReleaseOnce is one try of ForceReleaseRepoLock, in one transaction:
// the look, the compare-and-set on the holder it read, and the lost-hold
// notice to that holder (ClaimLostReleased), as ForceReleaseTask's. done is
// false when the hold changed hands between the look and the write.
func lkForceReleaseOnce(ctx context.Context, db *sql.DB, id string, now time.Time) (RepoLockForceReleased, bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return RepoLockForceReleased{}, false, fmt.Errorf("store.ForceReleaseRepoLock: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	cur, ok, err := lkLockByID(ctx, tx, id)
	if err != nil {
		return RepoLockForceReleased{}, false, fmt.Errorf("store.ForceReleaseRepoLock: %w", err)
	}
	if !ok {
		return RepoLockForceReleased{}, false, fmt.Errorf("store.ForceReleaseRepoLock: %w: %q", ErrRepoLockNotFound, id)
	}
	if cur.Holder == "" {
		return RepoLockForceReleased{Lock: cur}, false, fmt.Errorf("store.ForceReleaseRepoLock: %w: lock %q", ErrRepoLockFree, id)
	}
	// Conditioned on the holder just read, so the prior holder reported is
	// exactly the hold this write ended.
	res, err := tx.ExecContext(ctx, `
		UPDATE repo_locks SET holder = '', held_since = NULL, lease_expires_at = NULL, updated_at = ?
		 WHERE id = ? AND holder = ?`, core.FormatTime(now.UTC()), id, cur.Holder)
	if err != nil {
		return RepoLockForceReleased{}, false, fmt.Errorf("store.ForceReleaseRepoLock: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return RepoLockForceReleased{}, false, fmt.Errorf("store.ForceReleaseRepoLock: rows affected: %w", err)
	}
	if n == 0 {
		return RepoLockForceReleased{}, false, nil // the hold changed hands since the look: look again
	}
	if err := RecordClaimNotice(ctx, tx, ClaimNotice{
		SessionID: cur.Holder, Kind: ClaimResourceLock, ResourceID: id, Label: cur.Name,
		Reason: ClaimLostReleased, LeaseExpiresAt: cur.LeaseExpiresAt, LostAt: now,
	}); err != nil {
		return RepoLockForceReleased{}, false, fmt.Errorf("store.ForceReleaseRepoLock: %w", err)
	}
	freed, ok, err := lkLockByID(ctx, tx, id)
	if err != nil {
		return RepoLockForceReleased{}, false, fmt.Errorf("store.ForceReleaseRepoLock: %w", err)
	}
	if !ok {
		return RepoLockForceReleased{}, false, fmt.Errorf("store.ForceReleaseRepoLock: %w: %q", ErrRepoLockNotFound, id)
	}
	if err := tx.Commit(); err != nil {
		return RepoLockForceReleased{}, false, fmt.Errorf("store.ForceReleaseRepoLock: commit: %w", err)
	}
	return RepoLockForceReleased{Lock: freed, PriorHolder: cur.Holder}, true, nil
}

// ReleaseRepoLocksForSession frees every lock sessionID holds, live or lapsed:
// a departing session's locks return, as its task claims do
// (ReleaseClaimsForSession). It returns how many it freed.
func ReleaseRepoLocksForSession(ctx context.Context, db *sql.DB, sessionID string, now time.Time) (int, error) {
	if sessionID == "" {
		return 0, nil
	}
	res, err := db.ExecContext(ctx, `
		UPDATE repo_locks SET holder = '', held_since = NULL, lease_expires_at = NULL, updated_at = ?
		 WHERE holder = ?`, core.FormatTime(now.UTC()), sessionID)
	if err != nil {
		return 0, fmt.Errorf("store.ReleaseRepoLocksForSession: %w", err)
	}
	// The count is the whole answer, and 0 an ordinary one: a driver failure
	// must not read as "freed nothing".
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store.ReleaseRepoLocksForSession: rows affected: %w", err)
	}
	return int(n), nil
}

// lkLapsedHoldOf matches a lock the bound session holds under a hold that is
// not live at the bound instant: core.RepoLock.HoldLive's negation for a row
// whose holder is that session. Two binds: the session id, then now.
const lkLapsedHoldOf = `holder = ? AND (lease_expires_at IS NULL OR lease_expires_at <= ?)`

// ReleaseReapedRepoLocks is the reaper's release of repo locks, as
// ReleaseReapedClaims is of task claims: it frees every lock sessionID still
// holds under a lapsed lease -- the session the reaper just expired, which it
// expires only once no hold of its is live -- and records a notice for each
// (ClaimLostReaped), in one transaction. Without the notice the holder would
// never hear of it: a later acquire takes a free lock, which is no reclaim. A
// hold live at now is left alone: one the holder took after the reap, through
// a call that revived its session, is its own again. It returns how many
// locks it freed.
func ReleaseReapedRepoLocks(ctx context.Context, db *sql.DB, sessionID string, now time.Time) (int, error) {
	const op = "store.ReleaseReapedRepoLocks"
	if sessionID == "" {
		return 0, nil
	}
	nowStr := core.FormatTime(now.UTC())
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("%s: begin: %w", op, err)
	}
	defer func() { _ = tx.Rollback() }()
	held, err := lkLockRows(ctx, tx, `SELECT `+lkLockCols+` FROM repo_locks WHERE `+lkLapsedHoldOf, sessionID, nowStr)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", op, err)
	}
	released := 0
	for _, l := range held {
		res, err := tx.ExecContext(ctx, `
			UPDATE repo_locks SET holder = '', held_since = NULL, lease_expires_at = NULL, updated_at = ?
			 WHERE id = ? AND `+lkLapsedHoldOf, nowStr, l.ID, sessionID, nowStr)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", op, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("%s: rows affected: %w", op, err)
		}
		if n == 0 {
			continue
		}
		if err := RecordClaimNotice(ctx, tx, ClaimNotice{
			SessionID: sessionID, Kind: ClaimResourceLock, ResourceID: l.ID, Label: l.Name,
			Reason: ClaimLostReaped, LeaseExpiresAt: l.LeaseExpiresAt, LostAt: now,
		}); err != nil {
			return 0, fmt.Errorf("%s: %w", op, err)
		}
		released++
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("%s: commit: %w", op, err)
	}
	return released, nil
}

// ApproveRepoLock makes a restricted account's waiting declaration an active
// lock: the owner's decision, on the console. ErrRepoLockNotRequested when it
// is already active, ErrRepoLockNotFound when the id names no lock.
func ApproveRepoLock(ctx context.Context, db *sql.DB, id string, now time.Time) (core.RepoLock, error) {
	res, err := db.ExecContext(ctx, `UPDATE repo_locks SET state = 'active', updated_at = ? WHERE id = ? AND state = 'requested'`,
		core.FormatTime(now.UTC()), id)
	if err != nil {
		return core.RepoLock{}, fmt.Errorf("store.ApproveRepoLock: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return core.RepoLock{}, fmt.Errorf("store.ApproveRepoLock: rows affected: %w", err)
	}
	cur, ok, err := lkLockByID(ctx, db, id)
	if err != nil {
		return core.RepoLock{}, fmt.Errorf("store.ApproveRepoLock: %w", err)
	}
	if !ok {
		return core.RepoLock{}, fmt.Errorf("store.ApproveRepoLock: %w: %q", ErrRepoLockNotFound, id)
	}
	if n == 0 {
		return cur, fmt.Errorf("store.ApproveRepoLock: %w: lock %q is %s", ErrRepoLockNotRequested, id, cur.State)
	}
	return cur, nil
}

// DiscardRepoLock deletes a restricted account's waiting declaration with its
// aliases and projects, so the repository may be declared again: the owner's
// decision, on the console. It returns the declaration as it was. An active
// lock is refused (ErrRepoLockNotRequested): no surface removes one yet.
func DiscardRepoLock(ctx context.Context, db *sql.DB, id string) (core.RepoLock, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return core.RepoLock{}, fmt.Errorf("store.DiscardRepoLock: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit
	cur, ok, err := lkLockByID(ctx, tx, id)
	if err != nil {
		return core.RepoLock{}, fmt.Errorf("store.DiscardRepoLock: %w", err)
	}
	if !ok {
		return core.RepoLock{}, fmt.Errorf("store.DiscardRepoLock: %w: %q", ErrRepoLockNotFound, id)
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM repo_locks WHERE id = ? AND state = 'requested'`, id)
	if err != nil {
		return core.RepoLock{}, fmt.Errorf("store.DiscardRepoLock: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return core.RepoLock{}, fmt.Errorf("store.DiscardRepoLock: rows affected: %w", err)
	}
	if n == 0 {
		return cur, fmt.Errorf("store.DiscardRepoLock: %w: lock %q is %s", ErrRepoLockNotRequested, id, cur.State)
	}
	for _, q := range []string{`DELETE FROM repo_lock_aliases WHERE lock_id = ?`, `DELETE FROM repo_lock_projects WHERE lock_id = ?`} {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			return core.RepoLock{}, fmt.Errorf("store.DiscardRepoLock: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return core.RepoLock{}, fmt.Errorf("store.DiscardRepoLock: commit: %w", err)
	}
	return cur, nil
}
