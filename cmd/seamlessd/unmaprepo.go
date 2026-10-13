package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/store"
)

// runUnmapRepo removes this machine's repo mappings -- the inverse of map-repo.
// --path removes the mapping for one repo root; --stale removes every mapping
// whose path no longer exists on disk, which is the list doctor's repo map check
// warns about. --host, with --path, removes another machine's mapping from this
// database instead: on a shared daemon that machine's rows live here, out of
// reach of its own unmap-repo. Like map-repo it writes straight to the database,
// so no running daemon is needed. Only routes go: projects and everything in
// them stay.
func runUnmapRepo(args []string) error {
	fs := flag.NewFlagSet("unmap-repo", flag.ContinueOnError)
	path := fs.String("path", "", "repo path whose mapping to remove (exact match, made absolute)")
	stale := fs.Bool("stale", false, "remove every mapping on this machine whose path no longer exists on disk")
	host := fs.String("host", "", "with --path: remove that host's mapping instead of this machine's")
	dryRun := fs.Bool("dry-run", false, "print what would be removed without changing anything")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("seamlessd.unmap-repo: unexpected argument %q (use --path)", fs.Arg(0))
	}
	if (strings.TrimSpace(*path) == "") == !*stale {
		return fmt.Errorf("seamlessd.unmap-repo: pass exactly one of --path DIR or --stale")
	}
	// --stale stats each path, and another machine's paths cannot be stat'd from
	// here (constraint network-install-identity-is-host-scoped).
	if *stale && strings.TrimSpace(*host) != "" {
		return fmt.Errorf("seamlessd.unmap-repo: --host needs --path; --stale only checks this machine's disk")
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("seamlessd.unmap-repo: %w", err)
	}
	db, err := store.Open(cfg.DBPath())
	if err != nil {
		return fmt.Errorf("seamlessd.unmap-repo: %w", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	targets, err := unmapTargets(ctx, db, *path, *stale, *host)
	if err != nil {
		return fmt.Errorf("seamlessd.unmap-repo: %w", err)
	}
	if len(targets) == 0 {
		fmt.Println("no stale mappings: every path mapped on this machine exists on disk")
		return nil
	}
	if *dryRun {
		for _, r := range targets {
			fmt.Printf("would unmap %s%s -> project %q\n", r.Path, hostSuffix(*host), r.Slug)
		}
		return nil
	}
	paths := make([]string, len(targets))
	for i, r := range targets {
		paths[i] = r.Path
	}
	removed, err := store.RemoveHostRepoMappings(ctx, db, *host, paths)
	if err != nil {
		return fmt.Errorf("seamlessd.unmap-repo: %w", err)
	}
	for _, r := range removed {
		fmt.Printf("unmapped %s%s -> project %q\n", r.Path, hostSuffix(*host), r.Slug)
	}
	return nil
}

// hostSuffix names the host an unmap-repo line is about when --host was given.
func hostSuffix(host string) string {
	if strings.TrimSpace(host) == "" {
		return ""
	}
	return fmt.Sprintf(" (host %q)", strings.TrimSpace(host))
}

// unmapTargets picks the mappings an unmap-repo run acts on. With stale it is
// every local mapping whose path is provably gone (a clean not-exist, the same
// test doctor applies; any other stat failure keeps the mapping). Otherwise it
// is the mapping for exactly that path, and a path the host has not mapped is an
// error rather than an empty success. host "" or this machine's own name means
// this machine; any other host is matched by exact absolute path, never stat'd.
func unmapTargets(ctx context.Context, db *sql.DB, path string, stale bool, host string) ([]store.RepoMapRow, error) {
	host = strings.TrimSpace(host)
	localHost, _, err := store.GetSetting(ctx, db, store.SettingLocalHost)
	if err != nil {
		return nil, err
	}
	if host != "" && host != localHost {
		return remoteUnmapTargets(ctx, db, host, path)
	}
	local, err := store.LocalRepoMappings(ctx, db)
	if err != nil {
		return nil, err
	}
	if stale {
		var dead []store.RepoMapRow
		for _, r := range local {
			if _, serr := os.Lstat(r.Path); errors.Is(serr, fs.ErrNotExist) {
				dead = append(dead, r)
			}
		}
		return dead, nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	var out []store.RepoMapRow
	for _, r := range local {
		if filepath.Clean(r.Path) == abs {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w on this machine: %s", store.ErrRepoMappingNotFound, abs)
	}
	return out, nil
}

// remoteUnmapTargets is unmapTargets for another host. The path is that
// machine's, so it must already be absolute: resolving a relative one against
// this machine's working directory would name a path on the wrong disk. That
// machine's OS need not be this one's, so absolute means absolute on some OS
// (store.IsAbsRepoPath), and paths compare through the store's own
// normalization: filepath judges and cleans by this machine's rules, which
// refuse "/home/me/app" on Windows and "C:\repos\app" elsewhere.
func remoteUnmapTargets(ctx context.Context, db *sql.DB, host, path string) ([]store.RepoMapRow, error) {
	if !store.IsAbsRepoPath(path) {
		return nil, fmt.Errorf("--path must be absolute with --host: %q is a path on %s, not here", path, host)
	}
	want := store.NormRepoPath(path)
	rows, err := store.RepoMapRows(ctx, db)
	if err != nil {
		return nil, err
	}
	var out []store.RepoMapRow
	for _, r := range rows {
		if r.Host == host && store.NormRepoPath(r.Path) == want {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w on host %q: %s", store.ErrRepoMappingNotFound, host, want)
	}
	return out, nil
}
