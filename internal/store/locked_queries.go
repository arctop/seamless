package store

// Reads the agent surfaces need about locked projects (peer_auth.locked; the
// MCP server and the hooks), beside the lease storage in leases.go. They only
// read; ApproveAccess stays the one writer of a lease.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
)

// LastAccessLeaseEnd returns when the newest lease any of processKeys held on
// project ended, as of now: its revocation when it was revoked, else its
// expiry. ok is false when none of them ever held a lease there that has
// ended. It words the refusal after a lease lapses ("locked (access ended at
// 15:04)"); it grants nothing. Empty keys are ignored, and more than
// access.MaxChainHops keys are refused, as in ActiveAccessLeasesFor.
func LastAccessLeaseEnd(ctx context.Context, db *sql.DB, processKeys []string, project string, now time.Time) (time.Time, bool, error) {
	keys := make([]any, 0, len(processKeys)+2)
	for _, k := range processKeys {
		if k != "" {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return time.Time{}, false, nil
	}
	if len(keys) > access.MaxChainHops {
		return time.Time{}, false, fmt.Errorf("store.LastAccessLeaseEnd: %w: %d process keys, at most %d",
			ErrInvalidAccessRequest, len(keys), access.MaxChainHops)
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",")
	at := core.FormatTime(now)
	var (
		expires string
		revoked sql.NullString
	)
	err := db.QueryRowContext(ctx, `SELECT expires_at, revoked_at FROM access_leases
		WHERE process_key IN (`+marks+`) AND project_slug = ?
		  AND ((revoked_at IS NOT NULL AND revoked_at <= ?) OR expires_at <= ?)
		ORDER BY granted_at DESC, id DESC LIMIT 1`, append(keys, project, at, at)...).Scan(&expires, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("store.LastAccessLeaseEnd: %w", err)
	}
	end, err := core.ParseTime(expires)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("store.LastAccessLeaseEnd: expires_at: %w", err)
	}
	if revoked.Valid {
		r, err := core.ParseTime(revoked.String)
		if err != nil {
			return time.Time{}, false, fmt.Errorf("store.LastAccessLeaseEnd: revoked_at: %w", err)
		}
		if r.Before(end) {
			end = r
		}
	}
	return end, true, nil
}
