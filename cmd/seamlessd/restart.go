package main

import (
	"context"
	"database/sql"
	"errors"
	"slices"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/store"
)

// errServeRestart is what serve returns, after a clean shutdown, when the console
// asked for a restart: a change to the locked projects applies at startup. main
// answers it with reexecSelf once serve's deferred cleanup has released the data
// dir lock and closed the database.
var errServeRestart = errors.New("seamlessd.serve: restart requested")

// lockedProjects returns every project serve would lock under cfg, sorted: the
// force-locked ones and the console's. db may be nil, for a caller with no
// database yet, which then sees the file's list alone.
func lockedProjects(ctx context.Context, cfg config.Config, db *sql.DB) ([]string, error) {
	out := slices.Clone(cfg.PeerAuth.ForceLocked)
	if db != nil {
		console, err := store.ConsoleLockedSlugs(ctx, db)
		if err != nil {
			return nil, err
		}
		out = append(out, console...)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}
