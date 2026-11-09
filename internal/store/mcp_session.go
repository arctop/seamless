package store

// MCP client sessions (migration 036): what a client declared at initialize,
// kept past a daemon restart. internal/mcp decides what to keep and when
// (client_caps.go); this file only stores it, keyed by the Mcp-Session-Id, and
// keeps the table from growing.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/arctop/seamless/internal/core"
)

// MCPSessionRetention is how long after its last use a connection's row is
// kept. A connection unused for this long is gone (a bridge lasts as long as its
// agent), and a client that did come back simply shows no dialog, as before any
// of this existed.
const MCPSessionRetention = 30 * 24 * time.Hour

// SaveMCPSession records what the client of MCP session id declared at
// initialize, and drops rows no call has used within MCPSessionRetention: the
// one moment a row is added is the moment to clear the old ones, so the table
// stays bounded without a job of its own.
func SaveMCPSession(ctx context.Context, db *sql.DB, id, capabilities string, now time.Time) error {
	if id == "" {
		return fmt.Errorf("store.SaveMCPSession: %w: empty session id", ErrInvalidAccessRequest)
	}
	stamp := core.FormatTime(now)
	if _, err := db.ExecContext(ctx, `INSERT INTO mcp_sessions (id, capabilities, created_at, last_seen_at)
		VALUES (?, ?, ?, ?) ON CONFLICT (id) DO UPDATE SET capabilities = excluded.capabilities, last_seen_at = excluded.last_seen_at`,
		id, capabilities, stamp, stamp); err != nil {
		return fmt.Errorf("store.SaveMCPSession: %w", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM mcp_sessions WHERE last_seen_at < ?`,
		core.FormatTime(now.Add(-MCPSessionRetention))); err != nil {
		return fmt.Errorf("store.SaveMCPSession: prune: %w", err)
	}
	return nil
}

// MCPSessionCapabilities returns what MCP session id declared at initialize and
// when a call last used it; ok is false when no row names it.
func MCPSessionCapabilities(ctx context.Context, db *sql.DB, id string) (capabilities string, lastSeen time.Time, ok bool, err error) {
	var seen string
	err = db.QueryRowContext(ctx, `SELECT capabilities, last_seen_at FROM mcp_sessions WHERE id = ?`, id).Scan(&capabilities, &seen)
	if errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, false, nil
	}
	if err != nil {
		return "", time.Time{}, false, fmt.Errorf("store.MCPSessionCapabilities: %w", err)
	}
	lastSeen, err = core.ParseTime(seen)
	if err != nil {
		return "", time.Time{}, false, fmt.Errorf("store.MCPSessionCapabilities: last_seen_at: %w", err)
	}
	return capabilities, lastSeen, true, nil
}

// TouchMCPSession records that a call used MCP session id at now, so the
// connection is not pruned while it is in use. A row that no longer exists is
// left that way: it was pruned, and only an initialize brings one back.
func TouchMCPSession(ctx context.Context, db *sql.DB, id string, now time.Time) error {
	if _, err := db.ExecContext(ctx, `UPDATE mcp_sessions SET last_seen_at = ? WHERE id = ?`, core.FormatTime(now), id); err != nil {
		return fmt.Errorf("store.TouchMCPSession: %w", err)
	}
	return nil
}
