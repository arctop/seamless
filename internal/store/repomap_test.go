package store

import (
	"context"
	"os"
	pathpkg "path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// Migration 025 on a fresh database: the host column and the repo_map table
// exist with the shape every query below assumes.
func TestMigration025_SessionHostAndRepoMap(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	var host string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT host FROM sessions WHERE 1 = 0 UNION ALL SELECT ''`).Scan(&host),
		"sessions.host must exist after migration 025")

	_, err := db.ExecContext(ctx, `
		INSERT INTO repo_map (host, path, slug, origin, created_at)
		VALUES ('h1', '/r', 'p', '', ?)`, core.FormatTime(time.Now().UTC()))
	require.NoError(t, err)

	// The primary key is (host, path): the same path on another host is another
	// row, the same path on the same host is a conflict.
	_, err = db.ExecContext(ctx, `
		INSERT INTO repo_map (host, path, slug, origin, created_at)
		VALUES ('h2', '/r', 'other', '', ?)`, core.FormatTime(time.Now().UTC()))
	require.NoError(t, err, "the same path on a second host is a distinct mapping")
	_, err = db.ExecContext(ctx, `
		INSERT INTO repo_map (host, path, slug, origin, created_at)
		VALUES ('h1', '/r', 'dup', '', ?)`, core.FormatTime(time.Now().UTC()))
	require.Error(t, err, "(host, path) must be unique")
}

// AdoptLocalHost is what an existing install runs into on its first start after
// upgrading: sessions and the JSON repo map predate the host column entirely.
// Running it twice must change nothing the first run did not.
func TestAdoptLocalHost_BackfillsAndIsIdempotent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	id, err := core.NewID()
	require.NoError(t, err)
	now := time.Now().UTC()
	require.NoError(t, CreateSession(ctx, db, core.Session{
		ID: id, Name: "cc/legacy", Status: core.SessionActive, Ambient: true,
		CWD: "/work/app", CreatedAt: now, UpdatedAt: now,
	}))
	require.NoError(t, SetSetting(ctx, db, SettingRepoProjectMap, `{"/work/app":"app"}`))

	require.NoError(t, AdoptLocalHost(ctx, db, "Alpha"))

	sess, ok, err := SessionByID(ctx, db, id)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "Alpha", sess.Host, "a session written before host scoping becomes this machine's")

	rows, err := RepoMapRows(ctx, db)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "Alpha", rows[0].Host)
	require.Equal(t, "/work/app", rows[0].Path)
	require.Equal(t, "app", rows[0].Slug)

	// The mirror still reads as the local view, so the console and doctor are
	// unaffected by the move to the table.
	mirror, err := RepoProjectMap(ctx, db)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"/work/app": "app"}, mirror)

	require.NoError(t, AdoptLocalHost(ctx, db, "Alpha"))
	again, err := RepoMapRows(ctx, db)
	require.NoError(t, err)
	require.Equal(t, rows, again, "adoption must be idempotent")
}

// A mapping recorded before the daemon ever ran (a seeder, `map-repo` on a fresh
// data dir) lands in the unnamed bucket; adoption re-homes it rather than
// leaving it in a bucket no resolver asks for.
func TestAdoptLocalHost_ReHomesTheUnnamedBucket(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	require.NoError(t, AddRepoMapping(ctx, db, "/seeded/repo", "seeded"))
	rows, err := RepoMapRows(ctx, db)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Empty(t, rows[0].Host, "no daemon has named this machine yet")

	require.NoError(t, AdoptLocalHost(ctx, db, "alpha"))
	rows, err = RepoMapRows(ctx, db)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "alpha", rows[0].Host)

	slug, err := ResolveProjectForCWD(ctx, db, "alpha", "/seeded/repo/pkg")
	require.NoError(t, err)
	require.Equal(t, "seeded", slug)
}

// After adoption, AddRepoMapping (the `seamlessd map-repo` path) must land in
// the SAME bucket the resolvers read, or an owner override would take effect
// only after a restart.
func TestAddRepoMapping_LandsOnTheAdoptedHost(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	require.NoError(t, AdoptLocalHost(ctx, db, "alpha"))
	require.NoError(t, AddRepoMapping(ctx, db, "/work/override", "chosen"))

	slug, err := ResolveProjectForCWD(ctx, db, "alpha", "/work/override/sub")
	require.NoError(t, err)
	require.Equal(t, "chosen", slug)

	mirror, err := RepoProjectMap(ctx, db)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"/work/override": "chosen"}, mirror)
}

// A remote agent that sends its roots is placed without the daemon ever touching
// its own disk, and the row it writes is scoped to ITS host.
func TestRegisterProjectForCWD_RemoteWithRoots(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	require.NoError(t, AdoptLocalHost(ctx, db, "alpha"))

	slug, moved, err := RegisterProjectForCWD(ctx, db, CWDIdentity{
		Host: "beta", CWD: `C:\repos\myapp\internal`, RepoRoot: `C:\repos\myapp`,
		MainRoot: `C:\repos\myapp`, Origin: "git@github.com:acme/myapp.git",
	}, "alpha")
	require.NoError(t, err)
	require.Nil(t, moved)
	require.Equal(t, "myapp", slug, "a Windows root yields the same slug on a POSIX daemon")

	rows, err := RepoMapRows(ctx, db)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "beta", rows[0].Host)
	require.Equal(t, `C:\repos\myapp`, rows[0].Path)

	// The local host's map is untouched -- including its mirror, which the
	// console still reads.
	local, err := ResolveProjectForCWD(ctx, db, "alpha", `C:\repos\myapp\internal`)
	require.NoError(t, err)
	require.Empty(t, local, "a remote mapping must not resolve for the local host")
	mirror, err := RepoProjectMap(ctx, db)
	require.NoError(t, err)
	require.Empty(t, mirror, "a remote registration must not touch the local mirror")

	// The remote host resolves its own subdirectories, backslashes and all.
	got, err := ResolveProjectForCWD(ctx, db, "beta", `C:\repos\myapp\internal\store`)
	require.NoError(t, err)
	require.Equal(t, "myapp", got)
}

// The whole point of Phase 1: a remote session with no roots is refused rather
// than resolved against the daemon's own filesystem, and nothing is written.
func TestRegisterProjectForCWD_RemoteWithoutRootsWritesNothing(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	require.NoError(t, AdoptLocalHost(ctx, db, "alpha"))

	// A real repo on the DAEMON's disk at the very path the remote client names:
	// the pre-host-scoping code would have walked it and mapped the remote agent
	// to this machine's repo.
	root := filepath.Join(t.TempDir(), "decoy")
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".git"), 0o755))

	slug, moved, err := RegisterProjectForCWD(ctx, db,
		CWDIdentity{Host: "beta", CWD: root}, "alpha")
	require.ErrorIs(t, err, ErrRemoteRootUnknown)
	require.Empty(t, slug)
	require.Nil(t, moved)

	rows, err := RepoMapRows(ctx, db)
	require.NoError(t, err)
	require.Empty(t, rows, "a refused registration must write nothing")
	require.Empty(t, ListProjectsSlugs(t, db))
}

// Cross-host adoption: the same repository checked out on a second machine joins
// the existing project when the origins agree (or either is unknown), and mints
// its own when two KNOWN origins disagree.
func TestRegisterProjectForCWD_CrossHostAdoption(t *testing.T) {
	const origin = "https://github.com/acme/backend.git"
	for _, tc := range []struct {
		name         string
		firstOrigin  string
		secondOrigin string
		want         string
	}{
		{"matching origins adopt", origin, "git@github.com:acme/backend.git", "backend"},
		{"unknown on the new side adopts", origin, "", "backend"},
		{"unknown on the owning side adopts", "", origin, "backend"},
		{"both unknown adopts", "", "", "backend"},
		{"known and different mints", origin, "https://github.com/other/backend.git", "backend-2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := openTestDB(t)
			ctx := context.Background()
			require.NoError(t, AdoptLocalHost(ctx, db, "alpha"))

			slug, _, err := RegisterProjectForCWD(ctx, db, CWDIdentity{
				Host: "beta", CWD: "/srv/backend", RepoRoot: "/srv/backend", Origin: tc.firstOrigin,
			}, "alpha")
			require.NoError(t, err)
			require.Equal(t, "backend", slug)

			slug, _, err = RegisterProjectForCWD(ctx, db, CWDIdentity{
				Host: "gamma", CWD: "/home/me/backend", RepoRoot: "/home/me/backend", Origin: tc.secondOrigin,
			}, "alpha")
			require.NoError(t, err)
			require.Equal(t, tc.want, slug)
		})
	}
}

// Two different repos that share a directory name on the SAME host keep today's
// rule: mint, never merge. (Cross-host adoption must not loosen this.)
func TestRegisterProjectForCWD_SameHostCollisionStillMints(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	require.NoError(t, AdoptLocalHost(ctx, db, "alpha"))

	const origin = "https://github.com/acme/backend.git"
	slug, _, err := RegisterProjectForCWD(ctx, db, CWDIdentity{
		Host: "beta", CWD: "/a/backend", RepoRoot: "/a/backend", Origin: origin,
	}, "alpha")
	require.NoError(t, err)
	require.Equal(t, "backend", slug)

	slug, _, err = RegisterProjectForCWD(ctx, db, CWDIdentity{
		Host: "beta", CWD: "/b/backend", RepoRoot: "/b/backend", Origin: origin,
	}, "alpha")
	require.NoError(t, err)
	require.Equal(t, "backend-2", slug, "same host, two directories: two projects")
}

// The remap hazard this phase exists to close: a remote host's mapped path is
// always "missing" from the daemon's disk, and must never be read as a moved
// repo.
func TestRegisterProjectForCWD_DeadOwnerHealIgnoresRemoteRows(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	require.NoError(t, AdoptLocalHost(ctx, db, "alpha"))

	// beta maps a path that does not exist on the daemon's disk.
	_, _, err := RegisterProjectForCWD(ctx, db, CWDIdentity{
		Host: "beta", CWD: "/remote/backend", RepoRoot: "/remote/backend",
		Origin: "https://github.com/acme/backend.git",
	}, "alpha")
	require.NoError(t, err)

	// A LOCAL repo of the same name now registers. Its origin differs, so it is a
	// different repository: it must mint, and beta's row must survive untouched.
	local := filepath.Join(t.TempDir(), "backend")
	require.NoError(t, os.MkdirAll(filepath.Join(local, ".git"), 0o755))
	slug, moved, err := RegisterProjectForCWD(ctx, db, CWDIdentity{
		Host: "alpha", CWD: local, RepoRoot: local, Origin: "https://github.com/other/backend.git",
	}, "alpha")
	require.NoError(t, err)
	require.Nil(t, moved, "a remote row is never evidence that a repo moved")
	require.Equal(t, "backend-2", slug)

	got, err := ResolveProjectForCWD(ctx, db, "beta", "/remote/backend")
	require.NoError(t, err)
	require.Equal(t, "backend", got, "the remote mapping must be left alone")
}

// RepoRootsForProject is what the gardener opens on disk, so it must return one
// host's paths and not every host's.
func TestRepoRootsForProject_IsHostScoped(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	require.NoError(t, AdoptLocalHost(ctx, db, "alpha"))
	require.NoError(t, AddRepoMapping(ctx, db, "/local/app", "app"))
	_, _, err := RegisterProjectForCWD(ctx, db, CWDIdentity{
		Host: "beta", CWD: "/remote/app", RepoRoot: "/remote/app",
	}, "alpha")
	require.NoError(t, err)

	roots, err := RepoRootsForProject(ctx, db, "alpha")
	require.NoError(t, err)
	require.Equal(t, map[string][]string{"app": {"/local/app"}}, roots)

	roots, err = RepoRootsForProject(ctx, db, "beta")
	require.NoError(t, err)
	require.Equal(t, map[string][]string{"app": {"/remote/app"}}, roots)
}

// Two agents in the same absolute path on two machines are two different agents.
func TestActiveAmbientByCWD_SameCWDOnTwoHosts(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	mk := func(name, host string) {
		id, err := core.NewID()
		require.NoError(t, err)
		require.NoError(t, CreateSession(ctx, db, core.Session{
			ID: id, Name: name, Status: core.SessionActive, Ambient: true,
			CWD: "/work/app", Host: host, CreatedAt: now, UpdatedAt: now,
		}))
	}
	mk("cc/alpha", "alpha")
	mk("cc/beta", "beta")

	got, err := ActiveAmbientByCWD(ctx, db, "alpha", "/work/app")
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "cc/alpha", got[0].Name)

	got, err = ActiveAmbientByCWD(ctx, db, "beta", "/work/app")
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "cc/beta", got[0].Name)

	got, err = ActiveAmbientByCWD(ctx, db, "gamma", "/work/app")
	require.NoError(t, err)
	require.Empty(t, got, "a third machine shares neither session")
}

// The host filter on the project-scoped ambient lookups is optional, and ""
// means "every host" rather than "the unnamed one" -- that is what lets a caller
// widen its search after finding nothing on its own machine.
func TestActiveAmbientProjects_HostFilterIsOptional(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	mk := func(name, host, project string) {
		id, err := core.NewID()
		require.NoError(t, err)
		require.NoError(t, CreateSession(ctx, db, core.Session{
			ID: id, Name: name, ProjectSlug: project, Status: core.SessionActive,
			Ambient: true, Host: host, CreatedAt: now, UpdatedAt: now,
		}))
	}
	mk("cc/a", "alpha", "app")
	mk("cc/b", "beta", "other")

	projects, err := ActiveAmbientProjects(ctx, db, AmbientScope{Host: "alpha"}, time.Hour)
	require.NoError(t, err)
	require.Equal(t, []string{"app"}, projects)

	projects, err = ActiveAmbientProjects(ctx, db, AmbientScope{}, time.Hour)
	require.NoError(t, err)
	require.Len(t, projects, 2, `"" is no filter at all`)

	sessions, err := ActiveAmbientSessionsForProject(ctx, db, AmbientScope{Host: "beta"}, "other", time.Hour)
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	sessions, err = ActiveAmbientSessionsForProject(ctx, db, AmbientScope{Host: "alpha"}, "other", time.Hour)
	require.NoError(t, err)
	require.Empty(t, sessions)
}

// normalizeOrigin decides whether two checkouts are the same repository, so the
// forms git actually writes must all reduce to one value.
func TestNormalizeOrigin(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"https://github.com/acme/Repo.git", "github.com/acme/repo"},
		{"git@github.com:acme/repo.git", "github.com/acme/repo"},
		{"ssh://git@github.com/acme/repo", "github.com/acme/repo"},
		{"https://github.com/acme/repo/", "github.com/acme/repo"},
		{"ssh://git@host:2222/acme/repo.git", "host:2222/acme/repo"},
	} {
		require.Equal(t, tc.want, normalizeOrigin(tc.in), tc.in)
	}
}

// Prefix matching has to work for a client whose separator is not the daemon's.
func TestPathHasPrefix_BothSeparators(t *testing.T) {
	require.True(t, pathHasPrefix("/a/b/c", "/a/b"))
	require.True(t, pathHasPrefix(`C:\a\b\c`, `C:\a\b`))
	require.True(t, pathHasPrefix("/a/b", "/a/b"))
	require.False(t, pathHasPrefix("/a/bc", "/a/b"))
	require.False(t, pathHasPrefix(`C:\a\bc`, `C:\a\b`))
}

// normPath follows the path's shape, so another host's path keeps its own form
// whichever OS the daemon runs. The Windows-shaped normalizer is the one that
// used to turn a POSIX client's "/elsewhere/app" into "\elsewhere\app".
func TestNormPathWith_PathShapes(t *testing.T) {
	// windowsClean stands in for filepath.Clean on a Windows daemon, for the
	// backslash-free paths normPathWith hands it: cleaned, with forward slashes
	// turned into backslashes.
	windowsClean := func(p string) string { return strings.ReplaceAll(pathpkg.Clean(p), "/", `\`) }
	posixClean := pathpkg.Clean // filepath.Clean on a POSIX daemon

	for _, tc := range []struct {
		name, in, posix, windows string
	}{
		{"empty", "", "", ""},
		{"blank", "   ", "", ""},
		{"POSIX path", "/elsewhere/app", "/elsewhere/app", "/elsewhere/app"},
		{"POSIX trailing slash", "/elsewhere/app/", "/elsewhere/app", "/elsewhere/app"},
		{"POSIX unclean", " /elsewhere//x/../app ", "/elsewhere/app", "/elsewhere/app"},
		{"POSIX root", "/", "/", "/"},
		{"Windows drive path", `C:\repos\app\`, `C:\repos\app`, `C:\repos\app`},
		{"UNC path", `\\srv\share\app\`, `\\srv\share\app`, `\\srv\share\app`},
		{"drive path, forward slashes", "C:/repos/app/", "C:/repos/app", `C:\repos\app`},
		{"relative", "a/b/", "a/b", `a\b`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.posix, normPathWith(posixClean, tc.in), "POSIX daemon")
			require.Equal(t, tc.windows, normPathWith(windowsClean, tc.in), "Windows daemon")
		})
	}

	// And through this daemon's real filepath.Clean, whatever OS runs the test.
	require.Equal(t, "/elsewhere/app", NormRepoPath("/elsewhere/app/"))
	require.Equal(t, `C:\repos\app`, NormRepoPath(`C:\repos\app\`))
}

// IsAbsRepoPath answers for whichever OS the path came from, not this one's.
func TestIsAbsRepoPath(t *testing.T) {
	for in, want := range map[string]bool{
		"/elsewhere/app":   true,
		" /elsewhere/app ": true,
		`C:\repos\app`:     true,
		"c:/repos/app":     true,
		`\\srv\share\app`:  true,
		"//srv/share/app":  true,
		"/":                true,
		"":                 false,
		"elsewhere/app":    false,
		`elsewhere\app`:    false,
		`\repos\app`:       false, // driveless: relative to a current drive
		"C:repos":          false, // drive-relative
		"C:":               false,
		`1:\repos`:         false,
		"~/app":            false,
	} {
		require.Equal(t, want, IsAbsRepoPath(in), in)
	}
}

// A POSIX client's paths are stored and matched as POSIX paths on every daemon,
// including a Windows one.
func TestRegisterProjectForCWD_RemotePOSIXPathKeepsItsShape(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	require.NoError(t, AdoptLocalHost(ctx, db, "alpha"))

	slug, _, err := RegisterProjectForCWD(ctx, db, CWDIdentity{
		Host: "beta", CWD: "/elsewhere/app/pkg", RepoRoot: "/elsewhere/app/",
	}, "alpha")
	require.NoError(t, err)
	require.Equal(t, "app", slug)

	rows, err := RepoMapRows(ctx, db)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "/elsewhere/app", rows[0].Path)

	got, err := ResolveProjectForCWD(ctx, db, "beta", "/elsewhere/app/pkg/sub")
	require.NoError(t, err)
	require.Equal(t, "app", got)

	removed, err := RemoveHostRepoMappings(ctx, db, "beta", []string{"/elsewhere/app/"})
	require.NoError(t, err)
	require.Len(t, removed, 1)
}

// A Windows daemon before the normPath fix stored a POSIX client's
// "/elsewhere/app" as "\elsewhere\app". Adoption gives such rows their slashes
// back, so the client's next session resolves its project instead of minting
// app-2. Only another host's single-leading-backslash rows are touched: a
// Windows client's drive and UNC paths, and every row of this machine's own,
// stay as they are.
func TestAdoptLocalHost_ReshapesMangledPOSIXRows(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	require.NoError(t, AdoptLocalHost(ctx, db, "alpha"))
	ts := core.FormatTime(time.Now().UTC())
	insert := func(host, path, slug, origin string) {
		t.Helper()
		_, err := db.ExecContext(ctx, `
			INSERT INTO repo_map (host, path, slug, origin, created_at)
			VALUES (?, ?, ?, ?, ?)`, host, path, slug, origin, ts)
		require.NoError(t, err)
	}
	insert("beta", `\elsewhere\app`, "app", "git@github.com:acme/app.git")
	insert("beta", `\`, "root", "")
	// The same repo in both shapes: the row already in the right shape wins.
	insert("gamma", `\srv\api`, "api-old", "")
	insert("gamma", "/srv/api", "api", "")
	// A Windows client's paths, and this machine's own rows, are not mangled.
	insert("winbox", `C:\repos\win`, "win", "")
	insert("winbox", `\\srv\share\unc`, "unc", "")
	insert("alpha", `\local\odd`, "local-odd", "")
	insert("", `\unnamed\odd`, "unnamed-odd", "") // re-homed to alpha, then left alone

	type hp struct{ host, path, slug, origin string }
	want := []hp{
		{"alpha", `\local\odd`, "local-odd", ""},
		{"alpha", `\unnamed\odd`, "unnamed-odd", ""},
		{"beta", "/", "root", ""},
		{"beta", "/elsewhere/app", "app", "git@github.com:acme/app.git"},
		{"gamma", "/srv/api", "api", ""},
		{"winbox", `C:\repos\win`, "win", ""},
		{"winbox", `\\srv\share\unc`, "unc", ""},
	}
	check := func() {
		t.Helper()
		rows, err := RepoMapRows(ctx, db)
		require.NoError(t, err)
		got := make([]hp, len(rows))
		for i, r := range rows {
			got[i] = hp{r.Host, r.Path, r.Slug, r.Origin}
		}
		require.Equal(t, want, got)
	}
	require.NoError(t, AdoptLocalHost(ctx, db, "alpha"))
	check()
	require.NoError(t, AdoptLocalHost(ctx, db, "alpha"))
	check() // idempotent

	slug, _, err := RegisterProjectForCWD(ctx, db, CWDIdentity{
		Host: "beta", CWD: "/elsewhere/app/pkg", RepoRoot: "/elsewhere/app",
	}, "alpha")
	require.NoError(t, err)
	require.Equal(t, "app", slug, "the reshaped row resolves; nothing is minted")
	check()
}

// RemoveRepoMappings drops the local row AND its mirror entry, and the removal
// survives a restart: AdoptLocalHost re-imports the mirror into the table on
// every daemon start, so a row-only delete would come back. Another host's row
// for the same path and a nested repo's own mapping are left alone.
func TestRemoveRepoMappings_DropsRowAndMirror(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	require.NoError(t, AdoptLocalHost(ctx, db, "alpha"))
	require.NoError(t, AddRepoMapping(ctx, db, "/work/app", "app"))
	require.NoError(t, AddRepoMapping(ctx, db, "/work/app/nested", "nested"))
	require.NoError(t, AddRepoMapping(ctx, db, "/work/keep", "keep"))
	_, _, err := RegisterProjectForCWD(ctx, db, CWDIdentity{
		Host: "beta", CWD: "/work/app", RepoRoot: "/work/app",
	}, "alpha")
	require.NoError(t, err)

	removed, err := RemoveRepoMappings(ctx, db, []string{"/work/app/"})
	require.NoError(t, err)
	require.Len(t, removed, 1)
	require.Equal(t, "alpha", removed[0].Host)
	require.Equal(t, "/work/app", removed[0].Path)
	require.Equal(t, "app", removed[0].Slug)

	check := func() {
		slug, err := ResolveProjectForCWD(ctx, db, "alpha", "/work/app/src")
		require.NoError(t, err)
		require.Empty(t, slug, "the unmapped path no longer resolves on this machine")
		slug, err = ResolveProjectForCWD(ctx, db, "alpha", "/work/app/nested")
		require.NoError(t, err)
		require.Equal(t, "nested", slug, "exact match only: a nested repo keeps its mapping")
		slug, err = ResolveProjectForCWD(ctx, db, "beta", "/work/app")
		require.NoError(t, err)
		require.Equal(t, "app", slug, "another host's row for the same path is not ours to remove")
		mirror, err := RepoProjectMap(ctx, db)
		require.NoError(t, err)
		require.Equal(t, map[string]string{"/work/app/nested": "nested", "/work/keep": "keep"}, mirror)
	}
	check()
	require.NoError(t, AdoptLocalHost(ctx, db, "alpha"))
	check()
}

// RemoveHostRepoMappings drops another host's row on a shared daemon and leaves
// this machine's mapping for the same path -- and the local mirror -- alone,
// since two machines with one home layout map the same paths. An unknown path
// fails the whole call, naming the host. Naming the local host is a local
// removal, mirror included.
func TestRemoveHostRepoMappings_LeavesLocalRouteAlone(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	require.NoError(t, AdoptLocalHost(ctx, db, "alpha"))
	require.NoError(t, AddRepoMapping(ctx, db, "/work/app", "app"))
	for _, root := range []string{"/work/app", "/tmp/scratch"} {
		_, _, err := RegisterProjectForCWD(ctx, db, CWDIdentity{
			Host: "beta", CWD: root, RepoRoot: root,
		}, "alpha")
		require.NoError(t, err)
	}

	_, err := RemoveHostRepoMappings(ctx, db, "beta", []string{"/work/app", "/work/typo"})
	require.ErrorIs(t, err, ErrRepoMappingNotFound)
	require.ErrorContains(t, err, `host "beta"`)
	slug, err := ResolveProjectForCWD(ctx, db, "beta", "/work/app")
	require.NoError(t, err)
	require.Equal(t, "app", slug, "a failed batch removes nothing")

	removed, err := RemoveHostRepoMappings(ctx, db, "beta", []string{"/work/app", "/tmp/scratch"})
	require.NoError(t, err)
	require.Len(t, removed, 2)
	for _, r := range removed {
		require.Equal(t, "beta", r.Host)
	}
	slug, err = ResolveProjectForCWD(ctx, db, "beta", "/tmp/scratch")
	require.NoError(t, err)
	require.Empty(t, slug)
	slug, err = ResolveProjectForCWD(ctx, db, "alpha", "/work/app")
	require.NoError(t, err)
	require.Equal(t, "app", slug, "the local row for the same path stays")
	mirror, err := RepoProjectMap(ctx, db)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"/work/app": "app"}, mirror)

	// The local host by name is a local removal: row and mirror both go.
	removed, err = RemoveHostRepoMappings(ctx, db, "alpha", []string{"/work/app"})
	require.NoError(t, err)
	require.Len(t, removed, 1)
	mirror, err = RepoProjectMap(ctx, db)
	require.NoError(t, err)
	require.Empty(t, mirror)
}

// One unknown path fails the whole call and removes nothing, so a typo never
// half-applies a batch.
func TestRemoveRepoMappings_UnknownPathRemovesNothing(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	require.NoError(t, AdoptLocalHost(ctx, db, "alpha"))
	require.NoError(t, AddRepoMapping(ctx, db, "/work/app", "app"))

	removed, err := RemoveRepoMappings(ctx, db, []string{"/work/app", "/work/typo"})
	require.ErrorIs(t, err, ErrRepoMappingNotFound)
	require.ErrorContains(t, err, "/work/typo")
	require.Empty(t, removed)

	slug, err := ResolveProjectForCWD(ctx, db, "alpha", "/work/app")
	require.NoError(t, err)
	require.Equal(t, "app", slug)
}

// An entry that only ever existed in the legacy JSON mirror (a hand-written
// setting, a database never adopted) is a local mapping too, and removing it
// clears the mirror even though there is no table row to delete.
func TestRemoveRepoMappings_MirrorOnlyEntry(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	require.NoError(t, SetSetting(ctx, db, SettingRepoProjectMap, `{"/legacy/repo":"legacy","/legacy/other":"other"}`))

	local, err := LocalRepoMappings(ctx, db)
	require.NoError(t, err)
	require.Len(t, local, 2)

	removed, err := RemoveRepoMappings(ctx, db, []string{"/legacy/repo", "/legacy/repo"})
	require.NoError(t, err)
	require.Len(t, removed, 1, "the same path twice is one removal")

	mirror, err := RepoProjectMap(ctx, db)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"/legacy/other": "other"}, mirror)
}

// LocalRepoMappings is this machine's view: the adopted host's rows, never
// another host's.
func TestLocalRepoMappings_ExcludesOtherHosts(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	require.NoError(t, AdoptLocalHost(ctx, db, "alpha"))
	require.NoError(t, AddRepoMapping(ctx, db, "/local/app", "app"))
	_, _, err := RegisterProjectForCWD(ctx, db, CWDIdentity{
		Host: "beta", CWD: "/remote/app", RepoRoot: "/remote/app",
	}, "alpha")
	require.NoError(t, err)

	local, err := LocalRepoMappings(ctx, db)
	require.NoError(t, err)
	require.Len(t, local, 1)
	require.Equal(t, "/local/app", local[0].Path)
}
