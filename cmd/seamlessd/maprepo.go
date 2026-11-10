package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/store"
)

// runMapRepo upserts an entry in the repo_project_map setting so an agent whose
// working directory is under path is resolved to the given project slug by the
// hooks and by session_start.
func runMapRepo(args []string) error {
	fs := flag.NewFlagSet("map-repo", flag.ContinueOnError)
	path := fs.String("path", "", "absolute repo path (default: current directory)")
	project := fs.String("project", "", "project slug to map the path to")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*project) == "" {
		return fmt.Errorf("seamlessd.map-repo: --project is required")
	}

	abs := *path
	if abs == "" {
		wd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("seamlessd.map-repo: %w", err)
		}
		abs = wd
	}
	abs, err := filepath.Abs(abs)
	if err != nil {
		return fmt.Errorf("seamlessd.map-repo: %w", err)
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("seamlessd.map-repo: %w", err)
	}
	db, err := openMapRepoDB(cfg, os.Stderr)
	if err != nil {
		return fmt.Errorf("seamlessd.map-repo: %w", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	if err := store.AddRepoMapping(ctx, db, abs, *project); err != nil {
		return fmt.Errorf("seamlessd.map-repo: %w", err)
	}
	if _, err := store.EnsureProject(ctx, db, *project, *project); err != nil {
		return fmt.Errorf("seamlessd.map-repo: %w", err)
	}
	fmt.Printf("mapped %s -> project %q\n", abs, *project)
	return nil
}

// openMapRepoDB is openServedDB, except that a server install with no database
// yet gets one. map-repo is the one one-shot command with a before-any-daemon
// use: the installers can leave no daemon at all (SEAMLESS_NO_SERVICE, a host
// with no user service), and serve's AdoptLocalHost re-homes the rows of a
// map-repo that ran before it did. Creating a database moves no schema under
// anyone. A client install never runs a daemon, so it gets openServedDB's
// refusal rather than a database nothing will ever read.
func openMapRepoDB(cfg config.Config, warnings io.Writer) (*sql.DB, error) {
	if _, err := os.Stat(cfg.DBPath()); errors.Is(err, os.ErrNotExist) && !cfg.IsClient() {
		return store.Open(cfg.DBPath())
	}
	return openServedDB(cfg, warnings)
}
