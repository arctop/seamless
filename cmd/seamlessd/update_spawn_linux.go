//go:build linux

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/arctop/seamless/internal/update"
)

// osUpdateSpawner is Linux's spawner: the updater as a transient user service
// of its own (systemdRunArgv).
func osUpdateSpawner(t spawnTarget, _ *slog.Logger) update.Spawner {
	return systemdRunSpawner{target: t}
}

// systemdRunSpawner starts the updater with systemd-run --user. Stopping or
// restarting seamless.service kills the unit's whole cgroup, so a child of the
// daemon dies with it, Setsid or not; a unit of its own survives both, and its
// RuntimeMaxSec and control-group kill also reap whatever the installer left
// behind. No `systemd-run --scope` fallback: it fails whenever this does (the
// same manager connection and binary), and it would keep the daemon's
// environment (2.01, systemd 249-261).
type systemdRunSpawner struct {
	target spawnTarget
}

// Spawn runs systemd-run and waits for it. Exit 0 means the manager created
// the unit and exec'd the updater (Type=exec); anything else is a spawn that
// did not happen, named from systemd-run's output.
func (s systemdRunSpawner) Spawn(ctx context.Context, req update.SpawnRequest) error {
	if err := req.Validate(); err != nil {
		return fmt.Errorf("seamlessd.spawn: %w", err)
	}
	environ := os.Environ()
	argv := systemdRunArgv(s.target, req, environ)
	out, err := runHandoffEnv(ctx, handoffTimeout, systemdRunEnv(environ, os.Geteuid()), argv[0], argv[1:]...)
	if err != nil {
		return fmt.Errorf("seamlessd.spawn: start the updater as a transient unit: %w", systemdRunFailure(out, err))
	}
	return nil
}
