package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/arctop/seamless/internal/core"
)

// OpenProjectSlugs returns the slug of every registered project whose isolation
// is open, sorted: the reach the peer_auth transport hands a restricted local
// account for one request (access.View.WithOpenProjects), which then drops the
// locked ones. A project in any other state -- confidential, sealed, or a value
// this build does not know -- is left out, so an unknown state fails closed.
func OpenProjectSlugs(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT slug FROM projects WHERE isolation = ? ORDER BY slug`,
		string(core.IsolationOpen))
	if err != nil {
		return nil, fmt.Errorf("store.OpenProjectSlugs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return nil, fmt.Errorf("store.OpenProjectSlugs: %w", err)
		}
		out = append(out, slug)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.OpenProjectSlugs: %w", err)
	}
	return out, nil
}
