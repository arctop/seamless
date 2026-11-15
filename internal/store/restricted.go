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

// RestrictedReachStates is the registered projects a restricted account's
// reach is drawn from for one request (access.View.WithReach): the slugs whose
// isolation is open, and those confidential or sealed (Fenced), each sorted. A
// project in a state this build does not know is in neither, so it fails
// closed.
type RestrictedReachStates struct {
	Open   []string
	Fenced []string
}

// ReachProjects loads RestrictedReachStates in one query.
func ReachProjects(ctx context.Context, db *sql.DB) (RestrictedReachStates, error) {
	rows, err := db.QueryContext(ctx, `SELECT slug, isolation FROM projects WHERE isolation IN (?, ?, ?) ORDER BY slug`,
		string(core.IsolationOpen), string(core.IsolationConfidential), string(core.IsolationSealed))
	if err != nil {
		return RestrictedReachStates{}, fmt.Errorf("store.ReachProjects: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out RestrictedReachStates
	for rows.Next() {
		var slug, iso string
		if err := rows.Scan(&slug, &iso); err != nil {
			return RestrictedReachStates{}, fmt.Errorf("store.ReachProjects: %w", err)
		}
		if core.Isolation(iso) == core.IsolationOpen {
			out.Open = append(out.Open, slug)
		} else {
			out.Fenced = append(out.Fenced, slug)
		}
	}
	if err := rows.Err(); err != nil {
		return RestrictedReachStates{}, fmt.Errorf("store.ReachProjects: %w", err)
	}
	return out, nil
}
