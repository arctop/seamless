package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
)

const sessionCols = `id, name, project_slug, status, findings, claude_session_id,
	external_client, cwd, host, source, model, ambient, favorite, metadata, created_at, updated_at,
	input_tokens, cached_input_tokens, cache_creation_tokens, output_tokens, total_tokens, account,
	parent_session_id`

// ErrSessionNameExists is returned by CreateSession when the name is already
// taken (sessions.name is UNIQUE). It mirrors ErrSlugExists so a caller racing
// to create a named session can tell "someone beat me to this name" (resume it)
// apart from a real database failure, instead of having to match on error text.
//
// It reads any uniqueness failure on the insert as a name collision. The table's
// other unique constraint is the id primary key, and callers mint a fresh ULID
// per call, so a PK collision cannot happen without an id-generation bug -- the
// same assumption ErrSlugExists makes.
var ErrSessionNameExists = errors.New("store: session name already exists")

// ErrSessionIdentityExists is returned when an ambient row already owns the
// same full external session id and client discriminator. It is distinct from a
// display-name collision so callers can safely converge a concurrent create on
// the existing authoritative identity.
var ErrSessionIdentityExists = errors.New("store: ambient session identity already exists")

// CreateSession inserts a session. The caller mints the ULID id; a duplicate
// name returns ErrSessionNameExists, while a duplicate authoritative ambient
// identity returns ErrSessionIdentityExists.
//
// The session's account is the restricted account the request in ctx speaks
// for (access.AccountOf) unless the caller set one: a session belongs to the
// principal that started it, whichever surface created the row, so no caller
// has to remember to stamp it. Nothing ever changes it afterwards.
//
// A restricted account's session is stored inside its namespace
// (access.QualifiedSessionName): its name and its agent's external session id
// both carry "<account>:", so no name or id it gives can collide with, or
// find, a session another principal started. Callers that report the name
// qualify it first; qualifying again here changes nothing.
func CreateSession(ctx context.Context, db *sql.DB, s core.Session) error {
	if s.Account == "" {
		s.Account = access.AccountOf(ctx)
	}
	s.Name = access.QualifiedSessionName(s.Account, s.Name)
	s.ExternalSessionID = access.QualifiedSessionName(s.Account, s.ExternalSessionID)
	meta, err := marshalMetadata(s.Metadata)
	if err != nil {
		return fmt.Errorf("store.CreateSession: %w", err)
	}
	// A session continues no other unless it names one: NULL, not "", is the
	// column's "none" (migration 039), so a row from before it reads the same.
	var parent sql.NullString
	if s.ParentSessionID != "" {
		parent = sql.NullString{String: s.ParentSessionID, Valid: true}
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO sessions
		    (id, name, project_slug, status, findings, claude_session_id,
		     external_client, cwd, host, source, model, ambient, metadata, created_at, updated_at, account,
		     parent_session_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		// claude_session_id column holds core.Session.ExternalSessionID (the column
		// name predates Codex; see the field's doc for the intentional mismatch).
		s.ID, s.Name, s.ProjectSlug, string(s.Status), s.Findings, s.ExternalSessionID,
		s.ExternalClient, s.CWD, s.Host, s.Source, s.Model, boolToInt(s.Ambient), meta,
		core.FormatTime(s.CreatedAt), core.FormatTime(s.UpdatedAt), s.Account, parent)
	if err != nil {
		if isUniqueViolation(err) {
			if s.Ambient && s.ExternalClient != "" && s.ExternalSessionID != "" {
				_, found, lookupErr := AmbientSessionByExternalIdentity(
					ctx, db, s.ExternalClient, s.ExternalSessionID)
				if lookupErr != nil {
					return fmt.Errorf("store.CreateSession: classify identity conflict: %w", lookupErr)
				}
				if found {
					return fmt.Errorf("store.CreateSession: %w: client %q", ErrSessionIdentityExists, s.ExternalClient)
				}
			}
			return fmt.Errorf("store.CreateSession: %w: %q", ErrSessionNameExists, s.Name)
		}
		return fmt.Errorf("store.CreateSession: %w", err)
	}
	return nil
}

// UpdateSession updates the mutable fields of a session (status, findings,
// metadata, project scope, updated_at) by id. The parent session is not one of
// them: CreateSession sets it once.
func UpdateSession(ctx context.Context, db *sql.DB, s core.Session) error {
	meta, err := marshalMetadata(s.Metadata)
	if err != nil {
		return fmt.Errorf("store.UpdateSession: %w", err)
	}
	res, err := db.ExecContext(ctx, `
		UPDATE sessions
		   SET project_slug = ?, status = ?, findings = ?, metadata = ?, updated_at = ?
		 WHERE id = ?`,
		s.ProjectSlug, string(s.Status), s.Findings, meta,
		core.FormatTime(s.UpdatedAt), s.ID)
	if err != nil {
		return fmt.Errorf("store.UpdateSession: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store.UpdateSession: rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("store.UpdateSession: no session with id %q", s.ID)
	}
	// session_end lands findings here, so this is the main entry point for a
	// session into the search corpus; IndexSessionFTS also removes the row when
	// findings were cleared back to nothing.
	if err := IndexSessionFTS(ctx, db, s); err != nil {
		return fmt.Errorf("store.UpdateSession: %w", err)
	}
	return nil
}

// ReactivateAmbientSession resumes an ambient session by its authoritative
// external identity. The targeted UPDATE flips status back to active, re-scopes
// only when project is non-empty, and bumps recency without touching findings or
// metadata. Returning the row gives callers its actual display name, including a
// legacy pre-digest name preserved by migration 010.
//
// A move's child (parent_session_id set, migration 039) is never re-scoped: it
// is the conversation's home for good, and a resume or compact from the
// repository it left must not carry it back there. No other session has a
// parent, so every install without isolation.requests resumes as before.
func ReactivateAmbientSession(
	ctx context.Context,
	db *sql.DB,
	externalClient, externalSessionID, project string,
	now time.Time,
) (core.Session, bool, error) {
	if externalClient == "" || externalSessionID == "" {
		return core.Session{}, false, nil
	}
	sess, found, err := sessionOne(ctx, db, `
		UPDATE sessions
		   SET status = 'active',
		       project_slug = CASE WHEN ? = '' OR parent_session_id IS NOT NULL THEN project_slug ELSE ? END,
		       updated_at = ?
		 WHERE ambient = 1
		   AND external_client = ?
		   AND claude_session_id = ?
		 RETURNING `+sessionCols,
		project, project, core.FormatTime(now.UTC()), externalClient, externalSessionID)
	if err != nil {
		return core.Session{}, false, fmt.Errorf("store.ReactivateAmbientSession: %w", err)
	}
	return sess, found, nil
}

// ActiveSessionIDs reports which of the given session ids belong to a currently
// active session. It backs the MCP server's connection-binding sweep: a binding
// whose session is no longer active -- ended by the session_end tool, the
// SessionEnd hook, or the idle reaper -- is evicted. Ids absent from the result
// (unknown or non-active) are simply not set.
func ActiveSessionIDs(ctx context.Context, db *sql.DB, ids []string) (map[string]bool, error) {
	active := make(map[string]bool, len(ids))
	const chunkSize = 500 // stay well under SQLite's bound-parameter limit
	for start := 0; start < len(ids); start += chunkSize {
		chunk := ids[start:min(start+chunkSize, len(ids))]
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		rows, err := db.QueryContext(ctx, `SELECT id FROM sessions
			WHERE status = 'active' AND id IN (`+placeholders(len(chunk))+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("store.ActiveSessionIDs: %w", err)
		}
		if err := scanIDs(rows, active); err != nil {
			return nil, fmt.Errorf("store.ActiveSessionIDs: %w", err)
		}
	}
	return active, nil
}

// scanIDs collects single-column id rows into dst, closing rows.
func scanIDs(rows *sql.Rows, dst map[string]bool) error {
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		dst[id] = true
	}
	return rows.Err()
}

// TouchSession heartbeats an active session by bumping its updated_at, marking it
// still-alive for the idle reaper. It only touches active sessions (the WHERE
// guard), so it never resurrects a completed or expired one, and is a no-op on an
// empty id. Callers fire it on activity (a bound MCP tool call), best-effort.
func TouchSession(ctx context.Context, db *sql.DB, id string, now time.Time) error {
	if id == "" {
		return nil
	}
	_, err := db.ExecContext(ctx,
		`UPDATE sessions SET updated_at = ? WHERE id = ? AND status = 'active'`,
		core.FormatTime(now.UTC()), id)
	if err != nil {
		return fmt.Errorf("store.TouchSession: %w", err)
	}
	return nil
}

// TouchAmbientSession is TouchSession keyed by the full external session id and
// client discriminator. The active-only guard prevents late hook traffic from
// reviving a completed or reaped row.
func TouchAmbientSession(ctx context.Context, db *sql.DB, externalClient, externalSessionID string, now time.Time) error {
	if externalClient == "" || externalSessionID == "" {
		return nil
	}
	_, err := db.ExecContext(ctx,
		`UPDATE sessions SET updated_at = ?
		  WHERE ambient = 1 AND external_client = ? AND claude_session_id = ?
		    AND status = 'active'`,
		core.FormatTime(now.UTC()), externalClient, externalSessionID)
	if err != nil {
		return fmt.Errorf("store.TouchAmbientSession: %w", err)
	}
	return nil
}

// SetSessionModel records which LLM powers an active session's agent, keyed by
// session ULID. The value is stored verbatim as the provider names it (e.g.
// "claude-fable-5"). Targeted single-column write for the same reason as
// ReactivateAmbientSession: no read-modify-write of the whole row, so it cannot
// clobber a concurrent findings/metadata update. The active-only guard keeps a
// completed/expired session's attribution frozen at what it ended with, and the
// model <> ? guard makes repeated reports of the same value free. No-op on an
// empty id or model (an agent that never learns its model is not an error), so
// a matched-zero-rows outcome is legitimate and deliberately unchecked.
func SetSessionModel(ctx context.Context, db *sql.DB, id, model string) error {
	if id == "" || model == "" {
		return nil
	}
	_, err := db.ExecContext(ctx,
		`UPDATE sessions SET model = ? WHERE id = ? AND status = 'active' AND model <> ?`,
		model, id, model)
	if err != nil {
		return fmt.Errorf("store.SetSessionModel: %w", err)
	}
	return nil
}

// SetAmbientSessionModel records the model by authoritative external identity.
func SetAmbientSessionModel(ctx context.Context, db *sql.DB, externalClient, externalSessionID, model string) error {
	if externalClient == "" || externalSessionID == "" || model == "" {
		return nil
	}
	_, err := db.ExecContext(ctx,
		`UPDATE sessions SET model = ?
		  WHERE ambient = 1 AND external_client = ? AND claude_session_id = ?
		    AND status = 'active' AND model <> ?`,
		model, externalClient, externalSessionID, model)
	if err != nil {
		return fmt.Errorf("store.SetAmbientSessionModel: %w", err)
	}
	return nil
}

// SetAmbientSessionTokens overwrites the harvested model token totals on an
// active ambient session, keyed by authoritative external identity. Like the
// other targeted ambient writers it is a single-purpose write (only the five
// token columns + updated_at, never a read-modify-write of the whole row), so a
// concurrent findings/model update cannot clobber it and vice versa.
//
// The totals are absolute cumulative values, so this OVERWRITES rather than
// accumulates: re-harvesting a resumed session's grown transcript (Claude Code
// SessionEnd) or re-reading the latest cumulative token_count every turn (Codex
// Stop) writes the same absolute total again -- idempotent, never double-counted.
// The active-only + ambient guard means it never revives a completed/expired row
// and never touches an explicit session. Bumping updated_at doubles as a
// heartbeat. No-op on an incomplete identity or an empty (all-zero) usage (a turn
// with no token record leaves any prior harvest intact). Reports whether a row
// was updated.
func SetAmbientSessionTokens(
	ctx context.Context,
	db *sql.DB,
	externalClient, externalSessionID string,
	tokens core.TokenUsage,
	now time.Time,
) (bool, error) {
	if externalClient == "" || externalSessionID == "" || tokens.Empty() {
		return false, nil
	}
	res, err := db.ExecContext(ctx, `
		UPDATE sessions
		   SET input_tokens = ?, cached_input_tokens = ?, cache_creation_tokens = ?,
		       output_tokens = ?, total_tokens = ?, updated_at = ?
		 WHERE ambient = 1 AND external_client = ? AND claude_session_id = ?
		   AND status = 'active'`,
		tokens.Input, tokens.Cached, tokens.CacheCreation, tokens.Output, tokens.Total,
		core.FormatTime(now.UTC()), externalClient, externalSessionID)
	if err != nil {
		return false, fmt.Errorf("store.SetAmbientSessionTokens: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store.SetAmbientSessionTokens: rows affected: %w", err)
	}
	return n > 0, nil
}

// UpdateAmbientFindings upserts provisional findings onto an active ambient
// session by full external identity. It backs the Codex Stop hook, which
// harvests the last agent message every turn: Codex has no SessionEnd event, so
// findings must be in place BEFORE the idle reaper expires the session.
//
// The write is TARGETED -- only findings + updated_at, never a read-modify-write
// of the whole row -- so a Stop landing between a resume's read and write cannot
// be clobbered (mirroring the ensureAmbientSession contract), and repeated Stops
// simply converge findings on the latest turn's message. The active-only + ambient
// guard means it never resurrects a completed/expired session and never touches an
// explicit session (whose findings the agent owns via session_update). Bumping
// updated_at doubles as a heartbeat. No-op on an incomplete identity or empty
// findings (a turn with nothing to harvest leaves any prior harvest intact).
// Reports whether a row was updated.
func UpdateAmbientFindings(
	ctx context.Context,
	db *sql.DB,
	externalClient, externalSessionID, findings string,
	now time.Time,
) (bool, error) {
	if externalClient == "" || externalSessionID == "" || findings == "" {
		return false, nil
	}
	res, err := db.ExecContext(ctx, `
		UPDATE sessions SET findings = ?, updated_at = ?
		 WHERE external_client = ? AND claude_session_id = ?
		   AND status = 'active' AND ambient = 1`,
		findings, core.FormatTime(now.UTC()), externalClient, externalSessionID)
	if err != nil {
		return false, fmt.Errorf("store.UpdateAmbientFindings: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store.UpdateAmbientFindings: rows affected: %w", err)
	}
	if n > 0 {
		// Codex has no SessionEnd, so for those sessions this is the only path
		// findings ever take -- without it their handoff prose would never
		// reach the corpus. Keyed by external identity, so the mirror looks the
		// id up rather than making this targeted write a transaction.
		if err := reindexSessionFTSByExternal(ctx, db, externalClient, externalSessionID); err != nil {
			return true, fmt.Errorf("store.UpdateAmbientFindings: %w", err)
		}
	}
	return n > 0, nil
}

// ExpireStaleSessions reaps active sessions whose last activity predates cutoff,
// flipping each to SessionExpired. It leaves updated_at untouched so the row still
// records when the session was last alive (the console orders and dates by it),
// which also keeps the flip idempotent: a reaped session no longer matches the
// active filter. It returns only the sessions it actually flipped (id + project)
// so the caller can release their task claims and record telemetry -- a session
// that was heartbeated or gracefully completed between the candidate read and the
// flip is skipped, never falsely reaped. Passing updated_at unchanged means a
// later graceful session_end/resume can still upgrade the row (its guards key off
// completed/active, not expired).
func ExpireStaleSessions(ctx context.Context, db *sql.DB, cutoff time.Time) ([]core.Session, error) {
	candidates, err := staleActiveSessions(ctx, db, cutoff)
	if err != nil {
		return nil, err
	}
	var reaped []core.Session
	for _, s := range candidates {
		flipped, err := expireSessionIfStillStale(ctx, db, s.ID, cutoff)
		if err != nil {
			return nil, err
		}
		if flipped {
			reaped = append(reaped, s)
		}
	}
	return reaped, nil
}

// expireSessionIfStillStale flips one session to expired iff it is STILL an
// active session idle past cutoff at flip time, reporting whether it did. The
// re-check closes the window between the candidate SELECT and this UPDATE: a
// heartbeat (TouchSession bumping updated_at) or a graceful completion (status
// leaving 'active') that lands in between wins, and the session is not reaped.
func expireSessionIfStillStale(ctx context.Context, db *sql.DB, id string, cutoff time.Time) (bool, error) {
	res, err := db.ExecContext(ctx,
		`UPDATE sessions SET status = 'expired'
		  WHERE id = ? AND status = 'active' AND updated_at < ?`,
		id, core.FormatTime(cutoff.UTC()))
	if err != nil {
		return false, fmt.Errorf("store.ExpireStaleSessions: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store.ExpireStaleSessions: rows affected: %w", err)
	}
	return n == 1, nil
}

// staleActiveSessions returns the active sessions whose last activity predates
// cutoff, oldest first -- the reap candidate set. Split out so the read (with its
// deferred Close) is done before ExpireStaleSessions issues its UPDATEs.
func staleActiveSessions(ctx context.Context, db *sql.DB, cutoff time.Time) ([]core.Session, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+sessionCols+`
		FROM sessions
		WHERE status = 'active' AND updated_at < ?
		ORDER BY updated_at ASC`, core.FormatTime(cutoff.UTC()))
	if err != nil {
		return nil, fmt.Errorf("store.staleActiveSessions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var stale []core.Session
	for rows.Next() {
		s, serr := scanSession(rows)
		if serr != nil {
			return nil, fmt.Errorf("store.staleActiveSessions: %w", serr)
		}
		stale = append(stale, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.staleActiveSessions: %w", err)
	}
	return stale, nil
}

// ActiveAmbientByCWD returns the active ambient (cc/* or cx/*) sessions on one
// host whose cwd matches, most recent first. session_start consults it to link a
// freshly created explicit session to the Claude session that owns the cwd (via
// its claude_session_id), so a graceful SessionEnd can close both at once instead
// of leaving the explicit one to the idle reaper. An empty cwd matches nothing
// (no basis to link).
//
// The host is matched exactly, including the empty one: the same absolute path
// on two machines is two different working directories, and linking across them
// would hand one agent's session to another agent entirely.
func ActiveAmbientByCWD(ctx context.Context, db *sql.DB, host, cwd string) ([]core.Session, error) {
	if cwd == "" {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT `+sessionCols+`
		FROM sessions
		WHERE status = 'active' AND ambient = 1 AND host = ? AND cwd = ?
		ORDER BY updated_at DESC, id DESC`, host, cwd)
	if err != nil {
		return nil, fmt.Errorf("store.ActiveAmbientByCWD: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []core.Session
	for rows.Next() {
		s, serr := scanSession(rows)
		if serr != nil {
			return nil, fmt.Errorf("store.ActiveAmbientByCWD: %w", serr)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ActiveAmbientByCWD: %w", err)
	}
	return out, nil
}

// SetSessionAgentProcess records which agent process owns a session: an
// agentproc identity, or "" for none. The SessionStart hook stamps it on every
// start -- startup, resume, clear and compact -- so the row always names the
// process running it NOW: a session resumed in a new process moves to that
// process, and one resumed by an older client that sends no identity drops the
// dead process it named before.
func SetSessionAgentProcess(ctx context.Context, db *sql.DB, id, proc string) error {
	res, err := db.ExecContext(ctx, `UPDATE sessions SET agent_process = ? WHERE id = ?`, proc, id)
	if err != nil {
		return fmt.Errorf("store.SetSessionAgentProcess: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store.SetSessionAgentProcess: rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("store.SetSessionAgentProcess: no session with id %q", id)
	}
	return nil
}

// ActiveAmbientByAgentProcess returns the active ambient (cc/* or cx/*) sessions
// on one host that the given agent process owns, most recent first. It is how an
// MCP connection finds the session its own agent's SessionStart hook created:
// both name the same process. An empty identity matches nothing.
//
// The caller decides what more than one row means; it is not resolved here. A
// Claude Code process runs one session at a time and ends the old one on
// /clear or /resume, so two rows are a process hosting several live sessions,
// and a tool call from it cannot be pinned to either.
func ActiveAmbientByAgentProcess(ctx context.Context, db *sql.DB, host, proc string) ([]core.Session, error) {
	if proc == "" {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT `+sessionCols+`
		FROM sessions
		WHERE status = 'active' AND ambient = 1 AND host = ? AND agent_process = ?
		ORDER BY updated_at DESC, id DESC`, host, proc)
	if err != nil {
		return nil, fmt.Errorf("store.ActiveAmbientByAgentProcess: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []core.Session
	for rows.Next() {
		s, serr := scanSession(rows)
		if serr != nil {
			return nil, fmt.Errorf("store.ActiveAmbientByAgentProcess: %w", serr)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ActiveAmbientByAgentProcess: %w", err)
	}
	return out, nil
}

// ReviveExpiredAgentSession reactivates the ambient session the idle reaper
// expired while the agent process that owns it was still alive. The reaper's TTL
// is a guess that a quiet session is dead (a crashed agent sends no SessionEnd);
// a tool call naming the same live process disproves it, and without the revival
// an agent back from a long pause would lose its process binding until its next
// SessionStart. Only EXPIRED rows qualify: a completed session was ended on
// purpose -- by the agent's session_end or the client's SessionEnd -- and is not
// reopened behind anyone's back. More than one expired candidate is a process
// that ran several sessions at once and revives nothing.
//
// The header naming the process is the client's own claim, and the revival is
// a write, so it reopens only a session the caller in ctx may act in: one the
// same principal started (access.SessionAccountOK) -- a restricted account's
// own, or for the owner, background work and every call with peer_auth off a
// session no restricted account started. Another principal's row is not a
// candidate at all, so it is filtered before the candidates are counted: a
// process runs as one OS account, so such a row is never this process's
// second session, and a forged stamp can neither be reopened nor make the
// caller's own revival ambiguous. The one candidate must also lie where the
// caller may place a session (access.Writable), or in a project the owner has
// locked (access.Locked: the owner's own session there revives as before and
// binds nothing until a lease opens it). A restricted account's session in a
// project it no longer reaches -- made confidential, sealed or locked since --
// stays expired.
//
// found is false when there was nothing to revive, including when a concurrent
// call revived the row first (the UPDATE re-checks the status and project).
func ReviveExpiredAgentSession(ctx context.Context, db *sql.DB, host, proc string, now time.Time) (core.Session, bool, error) {
	if proc == "" {
		return core.Session{}, false, nil
	}
	candidates, err := expiredAgentSessions(ctx, db, host, proc, access.AccountOf(ctx))
	if err != nil {
		return core.Session{}, false, fmt.Errorf("store.ReviveExpiredAgentSession: %w", err)
	}
	if len(candidates) != 1 {
		return core.Session{}, false, nil
	}
	c := candidates[0]
	if !access.Writable(ctx, c.project) && !access.Locked(ctx, c.project) {
		return core.Session{}, false, nil
	}
	sess, found, err := sessionOne(ctx, db, `
		UPDATE sessions SET status = 'active', updated_at = ?
		 WHERE id = ? AND status = 'expired' AND project_slug = ?
		 RETURNING `+sessionCols,
		core.FormatTime(now.UTC()), c.id, c.project)
	if err != nil {
		return core.Session{}, false, fmt.Errorf("store.ReviveExpiredAgentSession: %w", err)
	}
	return sess, found, nil
}

// expiredAgentCandidate is one session ReviveExpiredAgentSession could reopen.
type expiredAgentCandidate struct{ id, project string }

// expiredAgentSessions returns up to two expired ambient sessions on host that
// proc is stamped on and account started ("" the owner's): two are enough to
// tell one candidate from several. The rows are closed before it returns, so
// the caller's UPDATE never waits on this read's connection.
func expiredAgentSessions(ctx context.Context, db *sql.DB, host, proc, account string) ([]expiredAgentCandidate, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, project_slug FROM sessions
		WHERE status = 'expired' AND ambient = 1 AND host = ? AND agent_process = ? AND account = ?
		LIMIT 2`, host, proc, account)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []expiredAgentCandidate
	for rows.Next() {
		var c expiredAgentCandidate
		if err := rows.Scan(&c.id, &c.project); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ActiveSessionsByExternalIdentity returns every active session -- the ambient
// cc/* or cx/* plus any explicit session_start that linked to it -- stamped with
// the same full external id and client discriminator, ambient first. A graceful
// SessionEnd uses it to close only the issuing client's session family.
func ActiveSessionsByExternalIdentity(
	ctx context.Context,
	db *sql.DB,
	externalClient, externalSessionID string,
) ([]core.Session, error) {
	if externalClient == "" || externalSessionID == "" {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT `+sessionCols+`
		FROM sessions
		WHERE status = 'active' AND external_client = ? AND claude_session_id = ?
		ORDER BY ambient DESC, updated_at DESC, id DESC`, externalClient, externalSessionID)
	if err != nil {
		return nil, fmt.Errorf("store.ActiveSessionsByExternalIdentity: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []core.Session
	for rows.Next() {
		s, serr := scanSession(rows)
		if serr != nil {
			return nil, fmt.Errorf("store.ActiveSessionsByExternalIdentity: %w", serr)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ActiveSessionsByExternalIdentity: %w", err)
	}
	return out, nil
}

// SessionByID returns the session with the given id. found is false when absent.
func SessionByID(ctx context.Context, db *sql.DB, id string) (core.Session, bool, error) {
	return sessionOne(ctx, db, `SELECT `+sessionCols+` FROM sessions WHERE id = ? LIMIT 1`, id)
}

// SessionByName returns the session with the given (unique) name. found is false
// when absent.
//
// The name is read inside the namespace of the caller in ctx
// (access.QualifySessionName): a restricted account's "sess/x" is its own
// "<account>:sess/x", never the owner's "sess/x", so a name another principal
// holds reads exactly like a free one. The owner, background work and every
// call with peer_auth off look the name up as given.
func SessionByName(ctx context.Context, db *sql.DB, name string) (core.Session, bool, error) {
	return sessionOne(ctx, db, `SELECT `+sessionCols+` FROM sessions WHERE name = ? LIMIT 1`,
		access.QualifySessionName(ctx, name))
}

// AmbientSessionByExternalIdentity resolves the authoritative ambient row for
// a client-issued session id. Display names are deliberately absent from the
// predicate so legacy and digest-suffixed names behave identically.
func AmbientSessionByExternalIdentity(
	ctx context.Context,
	db *sql.DB,
	externalClient, externalSessionID string,
) (core.Session, bool, error) {
	if externalClient == "" || externalSessionID == "" {
		return core.Session{}, false, nil
	}
	sess, found, err := sessionOne(ctx, db, `SELECT `+sessionCols+` FROM sessions
		WHERE ambient = 1 AND external_client = ? AND claude_session_id = ? LIMIT 1`,
		externalClient, externalSessionID)
	if err != nil {
		return core.Session{}, false, fmt.Errorf("store.AmbientSessionByExternalIdentity: %w", err)
	}
	return sess, found, nil
}

// RecentFindings returns ended sessions with meaningful findings visible to a
// project (its own plus global sessions), most recent first. It backs the
// briefing's "recent findings" section, so it excludes both blank findings and
// the core.FindingNoSummary sentinel (a session that ended with nothing to
// harvest) -- a content-free line is not worth an agent's context.
//
// "Ended" is completed OR reaper-expired-while-ambient. A Codex ambient session
// has no SessionEnd event (design D5): the Stop hook harvests findings onto the
// live row and the idle reaper flips it to 'expired', so requiring 'completed'
// would hide every Codex session's findings. Restricting the expired arm to
// ambient rows keeps this exact: a CC ambient session only ever gains findings at
// SessionEnd (which also sets 'completed'), so no CC row is active-with-findings
// to be reaped, and a crashed explicit session (ambient = 0) still does not
// surface -- the set added here is precisely Codex's reaper-ended sessions.
func RecentFindings(ctx context.Context, db *sql.DB, project string, limit int) ([]core.Session, error) {
	if limit <= 0 {
		limit = 3
	}
	rows, err := db.QueryContext(ctx, `SELECT `+sessionCols+`
		FROM sessions
		WHERE (status = 'completed' OR (status = 'expired' AND ambient = 1))
		  AND findings <> '' AND findings <> ?
		  AND (project_slug = ? OR project_slug = '')
		ORDER BY updated_at DESC, id DESC LIMIT ?`, core.FindingNoSummary, project, limit)
	if err != nil {
		return nil, fmt.Errorf("store.RecentFindings: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []core.Session
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("store.RecentFindings: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// LatestSessionProject returns the project of the most recently updated session
// that has one -- the project the owner's agents worked in last -- or "" when
// no session names a project. It is the console's default for "which project"
// questions, such as the Settings briefing preview.
func LatestSessionProject(ctx context.Context, db *sql.DB) (string, error) {
	var project string
	err := db.QueryRowContext(ctx, `SELECT project_slug FROM sessions
		WHERE project_slug <> ''
		ORDER BY updated_at DESC, id DESC LIMIT 1`).Scan(&project)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store.LatestSessionProject: %w", err)
	}
	return project, nil
}

// ListSessions returns sessions newest-updated first, optionally filtered by
// status and to those updated since a cutoff (a zero `since` means all time),
// capped at limit (default 100). It backs the console Sessions list.
func ListSessions(ctx context.Context, db *sql.DB, status core.SessionStatus, since time.Time, limit int) ([]core.Session, error) {
	if limit <= 0 {
		limit = 100
	}
	query := `SELECT ` + sessionCols + ` FROM sessions`
	args := []any{}
	where := ""
	add := func(cond string, val any) {
		if where == "" {
			where = " WHERE " + cond
		} else {
			where += " AND " + cond
		}
		args = append(args, val)
	}
	if status != "" {
		add(`status = ?`, string(status))
	}
	if !since.IsZero() {
		add(`updated_at >= ?`, core.FormatTime(since))
	}
	query += where + ` ORDER BY updated_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store.ListSessions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []core.Session
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("store.ListSessions: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// LiveSessionCount counts the sessions live right now: active and heartbeating
// on or after the cutoff -- the same predicate as ProjectsWithCounts' live
// column and Session.LiveAsOf. It backs the console's Now nav badge, so it is
// one scalar query, safe on every page load.
func LiveSessionCount(ctx context.Context, db *sql.DB, cutoff time.Time) (int, error) {
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sessions WHERE status = 'active' AND updated_at >= ?`,
		core.FormatTime(cutoff)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store.LiveSessionCount: %w", err)
	}
	return n, nil
}

// LatestActiveAmbientSessionForProject returns the most recently updated active
// ambient (cc/* or cx/*) session in the given project, updated within the window, or
// found=false when none. It backs the MCP write-scope fallback: an agent that
// writes without calling session_start inherits its project's ambient session's
// provenance. A non-positive within disables the recency filter. Scoping to a
// single project is what prevents cross-agent bleed -- see ActiveAmbientProjects.
//
// scope narrows the candidates (see AmbientScope); a scope with no Host spans
// every machine, which is what lets a caller widen its search after finding
// nothing on its own.
func LatestActiveAmbientSessionForProject(ctx context.Context, db *sql.DB, scope AmbientScope, project string, within time.Duration) (core.Session, bool, error) {
	query, args := scope.where(`SELECT `+sessionCols+` FROM sessions
		WHERE status = 'active' AND ambient = 1 AND project_slug = ?`, []any{project})
	if within > 0 {
		query += ` AND updated_at >= ?`
		args = append(args, core.FormatTime(time.Now().UTC().Add(-within)))
	}
	query += ` ORDER BY updated_at DESC, id DESC LIMIT 1`
	return sessionOne(ctx, db, query, args...)
}

// AmbientScope narrows the ambient-fallback lookups (ActiveAmbientProjects,
// LatestActiveAmbientSessionForProject, ActiveAmbientSessionsForProject) -- the
// inference an MCP call falls back to when its connection is bound to nothing.
type AmbientScope struct {
	// Host narrows the candidates to one machine; "" means every host (the filter
	// is absent, not a match on the unnamed bucket).
	Host string
	// Caller is the calling agent's process identity (internal/agentproc), when
	// it named one. A session stamped with a DIFFERENT process belongs to another
	// agent and is never a candidate: a caller that identified itself is not
	// guessed into someone else's session -- which, once its own session has
	// ended, would otherwise be exactly what the sole-ambient inference picks.
	// Unstamped sessions (older clients) stay candidates, as before identities
	// existed.
	Caller string
}

// where appends the scope's filters to a sessions query.
func (a AmbientScope) where(query string, args []any) (string, []any) {
	if a.Host != "" {
		query += ` AND host = ?`
		args = append(args, a.Host)
	}
	if a.Caller != "" {
		query += ` AND (agent_process = '' OR agent_process = ?)`
		args = append(args, a.Caller)
	}
	return query, args
}

// ActiveAmbientProjects returns the distinct project slugs that have at least one
// active ambient (cc/* or cx/*) session updated within the window, ordered by each
// project's most recent activity. The MCP fallback consults it to tell a safe
// single-project inference (len 1) from the ambiguous concurrent-agent case
// (len > 1, agents in different repos) where guessing would bleed a write into
// the wrong project. A non-positive within disables the recency filter. The
// global scope is reported as the empty string, distinct from any named project.
//
// scope narrows the candidates to one machine and away from other agents' sessions
// (see AmbientScope).
func ActiveAmbientProjects(ctx context.Context, db *sql.DB, scope AmbientScope, within time.Duration) ([]string, error) {
	query, args := scope.where(`SELECT project_slug FROM sessions
		WHERE status = 'active' AND ambient = 1`, nil)
	if within > 0 {
		query += ` AND updated_at >= ?`
		args = append(args, core.FormatTime(time.Now().UTC().Add(-within)))
	}
	query += ` GROUP BY project_slug ORDER BY MAX(updated_at) DESC`
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store.ActiveAmbientProjects: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return nil, fmt.Errorf("store.ActiveAmbientProjects: %w", err)
		}
		out = append(out, slug)
	}
	return out, rows.Err()
}

// ActiveAmbientSessionsForProject returns every active ambient (cc/* or cx/*) session in
// the given project updated within the window, most recent first. resolveSession
// uses it to refuse targeting a session by inference when more than one same-
// project ambient could be the one meant -- two agents in the same repo -- so a
// session_update/end without an explicit id can't complete a sibling's session.
// A non-positive within disables the recency filter. scope narrows the
// candidates (see AmbientScope).
func ActiveAmbientSessionsForProject(ctx context.Context, db *sql.DB, scope AmbientScope, project string, within time.Duration) ([]core.Session, error) {
	query, args := scope.where(`SELECT `+sessionCols+` FROM sessions
		WHERE status = 'active' AND ambient = 1 AND project_slug = ?`, []any{project})
	if within > 0 {
		query += ` AND updated_at >= ?`
		args = append(args, core.FormatTime(time.Now().UTC().Add(-within)))
	}
	query += ` ORDER BY updated_at DESC, id DESC`
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store.ActiveAmbientSessionsForProject: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []core.Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("store.ActiveAmbientSessionsForProject: %w", err)
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

// SiblingFindings returns the most recent ended sessions with meaningful findings
// across the given sibling projects, newest first, capped at limit. It backs the
// briefing's "Sibling projects" section, so -- like RecentFindings -- it excludes
// blank findings and the core.FindingNoSummary sentinel, and treats a
// reaper-expired ambient session as ended so Codex's SessionEnd-less sessions
// surface too (see RecentFindings for why the expired arm is ambient-only). An
// empty slugs slice yields no results.
func SiblingFindings(ctx context.Context, db *sql.DB, slugs []string, limit int) ([]core.Session, error) {
	if len(slugs) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 2
	}
	args := make([]any, 0, len(slugs)+2)
	args = append(args, core.FindingNoSummary)
	for _, s := range slugs {
		args = append(args, s)
	}
	args = append(args, limit)
	rows, err := db.QueryContext(ctx, `SELECT `+sessionCols+`
		FROM sessions
		WHERE (status = 'completed' OR (status = 'expired' AND ambient = 1))
		  AND findings <> '' AND findings <> ?
		  AND project_slug IN (`+placeholders(len(slugs))+`)
		ORDER BY updated_at DESC, id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("store.SiblingFindings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []core.Session
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("store.SiblingFindings: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func sessionOne(ctx context.Context, db *sql.DB, query string, args ...any) (core.Session, bool, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return core.Session{}, false, fmt.Errorf("store: session query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return core.Session{}, false, rows.Err()
	}
	s, err := scanSession(rows)
	if err != nil {
		return core.Session{}, false, fmt.Errorf("store: scan session: %w", err)
	}
	return s, true, nil
}

func scanSession(rows interface{ Scan(dest ...any) error }) (core.Session, error) {
	var (
		s                core.Session
		status, meta     string
		ambient          int
		created, updated string
		parent           sql.NullString
	)
	if err := rows.Scan(
		&s.ID, &s.Name, &s.ProjectSlug, &status, &s.Findings, &s.ExternalSessionID,
		&s.ExternalClient, &s.CWD, &s.Host, &s.Source, &s.Model, &ambient, &s.Favorite, &meta, &created, &updated,
		&s.Tokens.Input, &s.Tokens.Cached, &s.Tokens.CacheCreation, &s.Tokens.Output, &s.Tokens.Total, &s.Account,
		&parent,
	); err != nil {
		return core.Session{}, err
	}
	s.ParentSessionID = parent.String
	s.Status = core.SessionStatus(status)
	s.Ambient = ambient != 0
	if meta != "" && meta != "{}" {
		if err := json.Unmarshal([]byte(meta), &s.Metadata); err != nil {
			return core.Session{}, fmt.Errorf("metadata: %w", err)
		}
	}
	var err error
	if s.CreatedAt, err = core.ParseTime(created); err != nil {
		return core.Session{}, fmt.Errorf("created_at: %w", err)
	}
	if s.UpdatedAt, err = core.ParseTime(updated); err != nil {
		return core.Session{}, fmt.Errorf("updated_at: %w", err)
	}
	return s, nil
}

// marshalMetadata serializes a metadata map to a JSON object string ("{}" when
// empty), matching the sessions.metadata column default.
func marshalMetadata(m map[string]any) (string, error) {
	if len(m) == 0 {
		return "{}", nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("marshal metadata: %w", err)
	}
	return string(b), nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
