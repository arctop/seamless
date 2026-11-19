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
	"slices"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/devices"
	"github.com/arctop/seamless/internal/shellcmd"
	"github.com/arctop/seamless/internal/store"
)

// runMapRepo records a repo mapping, so an agent whose working directory is
// under path is resolved to the given project slug by the hooks and by
// session_start. --host maps a path on another machine instead: a relay's
// computer, whose requests speak for the name the relay paired as, so that a
// restricted account's clone there resolves (a restricted account registers
// nothing itself). On a client install it refuses, naming the command to run
// on the server, whose database holds the map.
func runMapRepo(args []string) error {
	fs := flag.NewFlagSet("map-repo", flag.ContinueOnError)
	path := fs.String("path", "", "absolute repo path (default: current directory; required with --host)")
	project := fs.String("project", "", "project slug to map the path to")
	host := fs.String("host", "", "map a path on that machine instead of this one: the name a relay or key paired as (the Devices page lists it)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("seamlessd.map-repo: unexpected argument %q (use --path)", fs.Arg(0))
	}
	if strings.TrimSpace(*project) == "" {
		return fmt.Errorf("seamlessd.map-repo: --project is required")
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("seamlessd.map-repo: %w", err)
	}
	if cfg.IsClient() {
		return clientMapRepoRefusal(*path, *project, *host)
	}
	db, err := openMapRepoDB(cfg, os.Stderr)
	if err != nil {
		return fmt.Errorf("seamlessd.map-repo: %w", err)
	}
	defer func() { _ = db.Close() }()
	return mapRepo(context.Background(), db, os.Stdout, os.Stderr, *path, *project, *host)
}

// mapRepo is runMapRepo's core over an open database. host "" or this
// machine's own name maps a path here, made absolute against the working
// directory; any other host maps that machine's path exactly as given
// (mapRemoteRepo). It also ensures the project exists, so mapping a new slug
// registers it.
func mapRepo(ctx context.Context, db *sql.DB, out, warn io.Writer, path, slug, host string) error {
	host = devices.NormalizeName(host)
	local, _, err := store.GetSetting(ctx, db, store.SettingLocalHost)
	if err != nil {
		return fmt.Errorf("seamlessd.map-repo: %w", err)
	}
	if host != "" && host != strings.TrimSpace(local) {
		return mapRemoteRepo(ctx, db, out, warn, host, path, slug)
	}
	abs, err := localRepoPath(path)
	if err != nil {
		return fmt.Errorf("seamlessd.map-repo: %w", err)
	}
	if err := store.AddRepoMapping(ctx, db, abs, slug); err != nil {
		return fmt.Errorf("seamlessd.map-repo: %w", err)
	}
	if _, err := store.EnsureProject(ctx, db, slug, slug); err != nil {
		return fmt.Errorf("seamlessd.map-repo: %w", err)
	}
	_, _ = fmt.Fprintf(out, "mapped %s -> project %q\n", abs, slug)
	// A path this disk does not have is most often another computer's, such
	// as the clone a restricted account behind a relay asked to be mapped:
	// it was mapped here all the same (a directory may be created later), and
	// the note says how to map it there instead.
	if _, err := os.Stat(abs); errors.Is(err, os.ErrNotExist) {
		_, _ = fmt.Fprintln(warn, absentRepoPathNote(ctx, db, abs, slug))
	}
	return nil
}

// mapRemoteRepo maps another machine's path. The path must already be
// absolute, on that machine's OS (store.IsAbsRepoPath): resolving a relative
// one, or defaulting to the working directory, would name a path on the wrong
// disk. It is never stat'd, as unmap-repo --host stats nothing. A host no
// paired device and no session has named is mapped, with a warning: nothing
// resolves through the row until a request speaks for that name.
func mapRemoteRepo(ctx context.Context, db *sql.DB, out, warn io.Writer, host, path, slug string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("seamlessd.map-repo: --host needs --path: the repository is on %s, so this directory is no default", host)
	}
	if !store.IsAbsRepoPath(path) {
		return fmt.Errorf("seamlessd.map-repo: --path must be absolute with --host: %q is a path on %s, not here", path, host)
	}
	known, paired := repoHosts(ctx, db)
	if err := store.AddHostRepoMapping(ctx, db, host, path, slug); err != nil {
		return fmt.Errorf("seamlessd.map-repo: %w", err)
	}
	if _, err := store.EnsureProject(ctx, db, slug, slug); err != nil {
		return fmt.Errorf("seamlessd.map-repo: %w", err)
	}
	_, _ = fmt.Fprintf(out, "mapped %s (host %q) -> project %q\n", store.NormRepoPath(path), host, slug)
	if !known[host] {
		_, _ = fmt.Fprintf(warn, "warning: no paired device and no session has named host %q, so nothing resolves through "+
			"this mapping until one does. A relay's requests speak for the name it paired as%s\n", host, pairedDevicesSuffix(paired))
	}
	return nil
}

// localRepoPath is map-repo's path on this machine: the working directory by
// default, made absolute.
func localRepoPath(path string) (string, error) {
	if path == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		path = wd
	}
	return filepath.Abs(path)
}

// repoHosts reports the hosts a mapping can resolve for -- every active
// device's name, and every host a session or a mapping came from -- and the
// active devices' names alone, sorted, for the texts. It is best effort: a
// read that fails leaves its hosts out, and only a warning or a note reads it.
func repoHosts(ctx context.Context, db *sql.DB) (known map[string]bool, paired []string) {
	known = map[string]bool{}
	if devs, err := store.ListDevices(ctx, db); err == nil {
		for _, d := range devs {
			if d.Status == store.DeviceActive {
				known[d.Name] = true
				paired = append(paired, d.Name)
			}
		}
	}
	if hosts, err := store.SessionHostsSince(ctx, db, time.Time{}); err == nil {
		for _, h := range hosts {
			known[h.Host] = true
		}
	}
	if rows, err := store.RepoMapRows(ctx, db); err == nil {
		for _, r := range rows {
			known[r.Host] = true
		}
	}
	delete(known, "")
	slices.Sort(paired)
	return known, slices.Compact(paired)
}

// pairedDevicesSuffix ends a sentence about the paired devices: their names, or that
// there are none.
func pairedDevicesSuffix(paired []string) string {
	if len(paired) == 0 {
		return "; this server has no paired device."
	}
	return " (paired here: " + strings.Join(paired, ", ") + ")."
}

// absentRepoPathNote is the note after a mapping of a path this machine's
// disk does not have.
func absentRepoPathNote(ctx context.Context, db *sql.DB, abs, slug string) string {
	_, paired := repoHosts(ctx, db)
	quoted := shellcmd.PosixArg(abs)
	return fmt.Sprintf("note: %s does not exist on this machine. A clone on another computer, such as a restricted "+
		"account's behind a relay, is mapped with that computer's name and its path there: seamlessd map-repo --host NAME "+
		"--path %s --project %s%s seamlessd unmap-repo --path %s removes this machine's mapping.",
		abs, quoted, shellcmd.PosixArg(slug), pairedDevicesSuffix(paired), quoted)
}

// clientMapRepoRefusal is map-repo's answer on a client install. This
// computer's repositories resolve in the server's database, under the name it
// speaks for there (the name its relay or key paired as), so the mapping is
// made there: the refusal prints that command, with the path made absolute
// here, where it is this computer's. Nothing is written.
func clientMapRepoRefusal(path, slug, host string) error {
	abs, err := localRepoPath(path)
	if err != nil {
		return fmt.Errorf("seamlessd.map-repo: %w", err)
	}
	name := devices.NormalizeName(host)
	if name == "" {
		name = config.Hostname()
	}
	shown := "<this computer's name>"
	if name != "" {
		shown = shellcmd.PosixArg(name)
	}
	return fmt.Errorf("seamlessd.map-repo: this computer is a client (role: client), and its repositories are mapped in "+
		"the server's database, not here. Map this clone on the server, as its owner (under peer_auth, as the daemon's "+
		"account):\n  seamlessd map-repo --host %s --path %s --project %s\n--host is the name this computer paired as, "+
		"which the server's Devices page lists", shown, shellcmd.PosixArg(abs), shellcmd.PosixArg(slug))
}

// openMapRepoDB is openServedDB, except that a server install with no database
// yet gets one. map-repo is the one one-shot command with a before-any-daemon
// use: the installers can leave no daemon at all (SEAMLESS_NO_SERVICE, a host
// with no user service), and serve's AdoptLocalHost re-homes the rows of a
// map-repo that ran before it did. Creating a database moves no schema under
// anyone. A client install never gets here: runMapRepo refuses it first,
// naming the command to run on the server.
func openMapRepoDB(cfg config.Config, warnings io.Writer) (*sql.DB, error) {
	if _, err := os.Stat(cfg.DBPath()); errors.Is(err, os.ErrNotExist) && !cfg.IsClient() {
		return store.Open(cfg.DBPath())
	}
	return openServedDB(cfg, warnings)
}
