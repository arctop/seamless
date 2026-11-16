package store

// Presence (plan repo-locks, RL2b): which live sessions work in a checkout of
// a repository that holds a declared lock, and what each session was last told
// about that repository's locks.
//
// A session row records its host and cwd, not its repository, and a
// repository is one lock's aliases on every machine. So a session started
// while repo locks are on records the lock aliases of its checkout
// (RepoLockAliasesFor of the SessionStart identity) in its metadata JSON, under
// SessionMetaRepoLockAliases, with no migration: presence is then the live
// sessions whose recorded aliases meet a lock's. The same metadata keeps, under
// SessionMetaRepoLockSeen, the holder of each lock the session was last shown
// ("" for free), so the prompt hook repeats a lock's line only when its holder
// changed. Both keys are written only while the feature is on; a session that
// never recorded them is in no repository's presence and hears no change.
//
// The writes are targeted json_set/json_remove UPDATEs of the two keys alone:
// no other metadata key, no findings and no updated_at (a session's liveness)
// move.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
)

// The session metadata keys presence keeps. (Not named ...Key: the settings
// guard, TestSettingsKeys_EveryKeyIsClassifiedForRestore, reads every such
// constant as a settings key, and these are no settings.)
const (
	// SessionMetaRepoLockAliases holds the lock aliases of the session's
	// checkout, as a JSON array of alias strings in resolution order.
	SessionMetaRepoLockAliases = "repo_lock_aliases"
	// SessionMetaRepoLockSeen holds, per lock id, the holding session's ULID
	// the session was last shown, "" for a lock it was shown free.
	SessionMetaRepoLockSeen = "repo_lock_seen"
)

// prMaxPeers bounds how many live sessions one presence read returns: more
// sessions than that in one repository at once is no fleet a line can name.
const prMaxPeers = 500

// prMetadata is the metadata column as a JSON object json_set can write to: a
// row whose metadata is not valid JSON is treated as empty rather than failing
// every write after it.
const prMetadata = `CASE WHEN json_valid(metadata) THEN metadata ELSE '{}' END`

// RepoLockAliasStrings returns the alias strings of aliases, in their order.
func RepoLockAliasStrings(aliases []core.RepoLockAlias) []string {
	out := make([]string, 0, len(aliases))
	for _, a := range aliases {
		if a.Alias != "" && !slices.Contains(out, a.Alias) {
			out = append(out, a.Alias)
		}
	}
	return out
}

// RepoLockAliasesOf turns recorded alias strings back into the aliases a lock
// resolves by (RepoLocksFor reads only the alias string).
func RepoLockAliasesOf(aliases []string) []core.RepoLockAlias {
	out := make([]core.RepoLockAlias, 0, len(aliases))
	for _, a := range aliases {
		if a != "" {
			out = append(out, core.RepoLockAlias{Alias: a})
		}
	}
	return out
}

// RecordSessionRepoLocks records a session's checkout aliases and the holders
// its briefing showed, replacing what it recorded before. An empty aliases --
// the session's checkout is no repository a lock could name -- removes both
// keys instead, so a session that moved out of a repository leaves its
// presence. A session id that names no row is ErrSessionNotFound.
func RecordSessionRepoLocks(ctx context.Context, db *sql.DB, sessionID string, aliases []string, seen map[string]string) error {
	var (
		query string
		args  []any
	)
	if len(aliases) == 0 {
		query = `UPDATE sessions SET metadata = json_remove(` + prMetadata + `, '$.` + SessionMetaRepoLockAliases +
			`', '$.` + SessionMetaRepoLockSeen + `') WHERE id = ?`
		args = []any{sessionID}
	} else {
		rawAliases, err := json.Marshal(aliases)
		if err != nil {
			return fmt.Errorf("store.RecordSessionRepoLocks: aliases: %w", err)
		}
		rawSeen, err := prSeenJSON(seen)
		if err != nil {
			return fmt.Errorf("store.RecordSessionRepoLocks: %w", err)
		}
		query = `UPDATE sessions SET metadata = json_set(` + prMetadata + `, '$.` + SessionMetaRepoLockAliases +
			`', json(?), '$.` + SessionMetaRepoLockSeen + `', json(?)) WHERE id = ?`
		args = []any{string(rawAliases), rawSeen, sessionID}
	}
	if err := prExecOne(ctx, db, query, args...); err != nil {
		return fmt.Errorf("store.RecordSessionRepoLocks: %w", err)
	}
	return nil
}

// SetSessionRepoLockSeen replaces the holders a session was last shown,
// leaving its recorded aliases as they are. A session id that names no row is
// ErrSessionNotFound.
func SetSessionRepoLockSeen(ctx context.Context, db *sql.DB, sessionID string, seen map[string]string) error {
	raw, err := prSeenJSON(seen)
	if err != nil {
		return fmt.Errorf("store.SetSessionRepoLockSeen: %w", err)
	}
	if err := prExecOne(ctx, db, `UPDATE sessions SET metadata = json_set(`+prMetadata+`, '$.`+
		SessionMetaRepoLockSeen+`', json(?)) WHERE id = ?`, raw, sessionID); err != nil {
		return fmt.Errorf("store.SetSessionRepoLockSeen: %w", err)
	}
	return nil
}

// prSeenJSON encodes a seen map, nil as the empty object.
func prSeenJSON(seen map[string]string) (string, error) {
	if seen == nil {
		seen = map[string]string{}
	}
	raw, err := json.Marshal(seen)
	if err != nil {
		return "", fmt.Errorf("seen holders: %w", err)
	}
	return string(raw), nil
}

// prExecOne runs an UPDATE of one session row, refusing one that names none.
func prExecOne(ctx context.Context, db *sql.DB, query string, args ...any) error {
	res, err := db.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return ErrSessionNotFound
	}
	return nil
}

// SessionRepoLocks reads what a session recorded (RecordSessionRepoLocks) from
// its metadata: the aliases of its checkout and the holders it was last shown.
// ok is false when it recorded no aliases -- repo locks were off when it
// started, or it works in no repository -- and then the session has no lock
// state at all. A value of the wrong shape reads as absent.
func SessionRepoLocks(sess core.Session) (aliases []string, seen map[string]string, ok bool) {
	raw, found := sess.Metadata[SessionMetaRepoLockAliases].([]any)
	if !found {
		return nil, nil, false
	}
	for _, v := range raw {
		if s, isStr := v.(string); isStr && s != "" && !slices.Contains(aliases, s) {
			aliases = append(aliases, s)
		}
	}
	if len(aliases) == 0 {
		return nil, nil, false
	}
	seen = map[string]string{}
	if m, isMap := sess.Metadata[SessionMetaRepoLockSeen].(map[string]any); isMap {
		for id, v := range m {
			if s, isStr := v.(string); isStr {
				seen[id] = s
			}
		}
	}
	return aliases, seen, true
}

// LiveRepoPeers returns the sessions live at now (core.Session.LiveAsOf with
// idle as the threshold; <= 0 is core.SessionIdleTTL) whose recorded checkout
// aliases meet aliases, other than the sessions exclude names: who else works
// in a checkout of the repository those aliases name, on any machine. Most
// recently active first, at most prMaxPeers. No alias matches nobody.
func LiveRepoPeers(ctx context.Context, db *sql.DB, aliases, exclude []string, now time.Time, idle time.Duration) ([]core.Session, error) {
	if len(aliases) == 0 {
		return []core.Session{}, nil
	}
	if idle <= 0 {
		idle = core.SessionIdleTTL
	}
	now = now.UTC()
	args := []any{core.FormatTime(now.Add(-idle)), core.FormatTime(now)}
	query := `SELECT ` + sessionCols + ` FROM sessions WHERE ` + sessionLive + `
		AND EXISTS (SELECT 1 FROM json_each(CASE WHEN json_valid(sessions.metadata) THEN sessions.metadata ELSE '{}' END,
			'$.` + SessionMetaRepoLockAliases + `') j WHERE j.type = 'text' AND j.value IN (` + placeholders(len(aliases)) + `))`
	for _, a := range aliases {
		args = append(args, a)
	}
	if ids := slices.DeleteFunc(slices.Clone(exclude), func(s string) bool { return strings.TrimSpace(s) == "" }); len(ids) > 0 {
		query += ` AND id NOT IN (` + placeholders(len(ids)) + `)`
		for _, id := range ids {
			args = append(args, id)
		}
	}
	query += ` ORDER BY updated_at DESC, id DESC LIMIT ?`
	args = append(args, prMaxPeers)
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store.LiveRepoPeers: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []core.Session{}
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("store.LiveRepoPeers: %w", err)
		}
		out = append(out, sess)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.LiveRepoPeers: %w", err)
	}
	return out, nil
}
