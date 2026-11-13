package store

import (
	"context"
	"database/sql"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

func TestSetProjectIsolation_RoundTripWithoutUpdatedBump(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)

	require.NoError(t, CreateProject(ctx, db, core.Project{
		ID: "01P", Slug: "seam", Name: "Seam", CreatedAt: now, UpdatedAt: now,
	}))

	// A fresh project reads open both via the row and via IsolationOf.
	p, ok, err := ProjectBySlug(ctx, db, "seam")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, core.IsolationOpen, p.Isolation)

	for _, state := range core.Isolations {
		require.NoError(t, SetProjectIsolation(ctx, db, "seam", state))
		p, ok, err = ProjectBySlug(ctx, db, "seam")
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, state, p.Isolation)
		require.Equal(t, now, p.UpdatedAt.UTC(), "isolation must not bump updated_at")

		got, err := IsolationOf(ctx, db, "seam")
		require.NoError(t, err)
		require.Equal(t, state, got)
	}

	// A fence that silently fails open is worse than an error: not-found errors.
	err = SetProjectIsolation(ctx, db, "does-not-exist", core.IsolationSealed)
	require.ErrorIs(t, err, ErrProjectNotFound)

	// Present-but-unrecognized never defaults.
	require.Error(t, SetProjectIsolation(ctx, db, "seam", "hidden"))
	require.Error(t, SetProjectIsolation(ctx, db, "seam", ""))
}

func TestIsolationOf_GlobalAndUnknownAreOpen(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	got, err := IsolationOf(ctx, db, "")
	require.NoError(t, err)
	require.Equal(t, core.IsolationOpen, got)

	got, err = IsolationOf(ctx, db, "never-registered")
	require.NoError(t, err)
	require.Equal(t, core.IsolationOpen, got)
}

func TestCreateProject_IsolationStates(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)

	require.NoError(t, CreateProject(ctx, db, core.Project{
		ID: "01A", Slug: "conf", Name: "conf", Isolation: core.IsolationConfidential,
		CreatedAt: now, UpdatedAt: now,
	}))
	p, ok, err := ProjectBySlug(ctx, db, "conf")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, core.IsolationConfidential, p.Isolation)

	require.Error(t, CreateProject(ctx, db, core.Project{
		ID: "01B", Slug: "bad", Name: "bad", Isolation: "hidden",
		CreatedAt: now, UpdatedAt: now,
	}))
}

// TestIsolationPolicy_Matrix is the canonical policy-matrix test: every
// combination of caller and target scope across the three states plus global.
// The fences in mcp/retrieve/gardener all route through CanRead/CanWrite, so
// this table is the single place the model is pinned.
func TestIsolationPolicy_Matrix(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)

	// Two projects per state so cross-project pairs exist within a state.
	seed := map[string]core.Isolation{
		"alpha": core.IsolationOpen,
		"beta":  core.IsolationOpen,
		"conf":  core.IsolationConfidential,
		"conf2": core.IsolationConfidential,
		"seal":  core.IsolationSealed,
		"seal2": core.IsolationSealed,
	}
	for slug, state := range seed {
		require.NoError(t, CreateProject(ctx, db, core.Project{
			ID: "01" + slug, Slug: slug, Name: slug, Isolation: state,
			CreatedAt: now, UpdatedAt: now,
		}))
	}

	tests := []struct {
		caller, target    string
		canRead, canWrite bool
	}{
		// Same scope always passes, whatever the state ("" is global-to-global).
		{"", "", true, true},
		{"alpha", "alpha", true, true},
		{"conf", "conf", true, true},
		{"seal", "seal", true, true},

		// Global caller (never isolated): fenced targets leak nothing out;
		// sealed targets admit nothing in, confidential targets stay writable.
		{"", "alpha", true, true},
		{"", "conf", false, true},
		{"", "seal", false, false},

		// Open caller: same picture as global.
		{"alpha", "", true, true},
		{"alpha", "beta", true, true},
		{"alpha", "conf", false, true},
		{"alpha", "seal", false, false},

		// Confidential caller: inbound unchanged (still reads global and open),
		// but writes nowhere outside itself -- global included.
		{"conf", "", true, false},
		{"conf", "alpha", true, false},
		{"conf", "conf2", false, false},
		{"conf", "seal", false, false},

		// Sealed caller: nothing in or out -- reads and writes only itself.
		{"seal", "", false, false},
		{"seal", "alpha", false, false},
		{"seal", "conf", false, false},
		{"seal", "seal2", false, false},
	}
	for _, tt := range tests {
		name := tt.caller + "->" + tt.target
		t.Run(name, func(t *testing.T) {
			gotRead, err := CanRead(ctx, db, tt.caller, tt.target)
			require.NoError(t, err)
			require.Equal(t, tt.canRead, gotRead, "CanRead(%s)", name)

			gotWrite, err := CanWrite(ctx, db, tt.caller, tt.target)
			require.NoError(t, err)
			require.Equal(t, tt.canWrite, gotWrite, "CanWrite(%s)", name)
		})
	}
}

// seedTopology registers four projects -- the one under test, a parent, a
// child, and a family sibling -- wired into exactly the topology a tighten has
// to unwind: kid is parented to app, app is parented to platform, and app
// shares a family with sibling.
func seedTopology(t *testing.T, db *sql.DB) (context.Context, time.Time) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	for _, slug := range []string{"app", "platform", "kid", "sibling"} {
		require.NoError(t, CreateProject(ctx, db, core.Project{
			ID: "01" + slug, Slug: slug, Name: slug, CreatedAt: now, UpdatedAt: now,
		}))
	}
	require.NoError(t, SetProjectParent(ctx, db, "app", "platform", now))
	require.NoError(t, SetProjectParent(ctx, db, "kid", "app", now))
	_, err := AddFamilyMembers(ctx, db, "acme", []string{"app", "sibling"})
	require.NoError(t, err)
	return ctx, now
}

func TestPreviewIsolationTighten_ReportsWithoutMutating(t *testing.T) {
	db := openTestDB(t)
	ctx, _ := seedTopology(t, db)

	// kid: a leaf with a parent and no family -- one detach, nothing blocking.
	eff, err := PreviewIsolationTighten(ctx, db, "kid", core.IsolationConfidential)
	require.NoError(t, err)
	require.Equal(t, "kid", eff.Project)
	require.Equal(t, core.IsolationOpen, eff.From)
	require.Equal(t, core.IsolationConfidential, eff.To)
	require.Equal(t, "app", eff.Parent)
	require.Empty(t, eff.Families)
	require.False(t, eff.Blocked())
	require.True(t, eff.DetachesTopology())

	// app: family + parent + a child. Children are a REPORT, not an error.
	eff, err = PreviewIsolationTighten(ctx, db, "app", core.IsolationSealed)
	require.NoError(t, err)
	require.Equal(t, []string{"acme"}, eff.Families)
	require.Equal(t, "platform", eff.Parent)
	require.Equal(t, []string{"kid"}, eff.Children)
	require.True(t, eff.Blocked())

	// The preview wrote nothing: state, parent, and family are untouched.
	p, ok, err := ProjectBySlug(ctx, db, "app")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, core.IsolationOpen, p.Isolation)
	require.Equal(t, "platform", p.ParentSlug)
	fams, err := ProjectFamilies(ctx, db)
	require.NoError(t, err)
	require.Equal(t, []string{"app", "sibling"}, fams["acme"])
}

func TestPreviewIsolationTighten_RejectsWhatItCannotAnswer(t *testing.T) {
	db := openTestDB(t)
	ctx, _ := seedTopology(t, db)
	require.NoError(t, SetProjectIsolation(ctx, db, "sibling", core.IsolationSealed))

	tests := []struct {
		name  string
		slug  string
		state core.Isolation
		want  error
	}{
		{"unknown slug", "nope", core.IsolationConfidential, ErrProjectNotFound},
		{"the global scope has no row", "", core.IsolationConfidential, ErrProjectNotFound},
		{"loosening is not a tighten", "sibling", core.IsolationConfidential, ErrNotATighten},
		{"open is never a tighten", "app", core.IsolationOpen, ErrNotATighten},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := PreviewIsolationTighten(ctx, db, tt.slug, tt.state)
			require.ErrorIs(t, err, tt.want)
		})
	}

	// A present-but-unrecognized state never defaults to one that works.
	_, err := PreviewIsolationTighten(ctx, db, "app", "hidden")
	require.Error(t, err)

	// Re-asserting the state a project already holds is a legal no-op tighten.
	eff, err := PreviewIsolationTighten(ctx, db, "sibling", core.IsolationSealed)
	require.NoError(t, err)
	require.Equal(t, core.IsolationSealed, eff.From)
}

func TestTightenProjectIsolation_DetachesFamilyAndParent(t *testing.T) {
	db := openTestDB(t)
	ctx, now := seedTopology(t, db)

	// Re-parent the child away so app is free to tighten.
	require.NoError(t, SetProjectParent(ctx, db, "kid", "platform", now))

	eff, err := TightenProjectIsolation(ctx, db, "app", core.IsolationConfidential, now)
	require.NoError(t, err)
	require.Equal(t, []string{"acme"}, eff.Families)
	require.Equal(t, "platform", eff.Parent)
	require.Empty(t, eff.Children)

	p, ok, err := ProjectBySlug(ctx, db, "app")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, core.IsolationConfidential, p.Isolation)
	require.Empty(t, p.ParentSlug, "a tighten detaches the parent link")

	// It left the family; the family itself survives with its other members.
	fams, err := ProjectFamilies(ctx, db)
	require.NoError(t, err)
	require.Equal(t, []string{"sibling"}, fams["acme"])
	sibs, err := SiblingProjects(ctx, db, "app")
	require.NoError(t, err)
	require.Empty(t, sibs, "an isolated project has no family surface left")

	// Confidential -> sealed is a further tighten and stays idempotent with the
	// topology already unwound.
	eff, err = TightenProjectIsolation(ctx, db, "app", core.IsolationSealed, now)
	require.NoError(t, err)
	require.False(t, eff.DetachesTopology(), "nothing left to detach")
	got, err := IsolationOf(ctx, db, "app")
	require.NoError(t, err)
	require.Equal(t, core.IsolationSealed, got)
}

func TestTightenProjectIsolation_ChildrenBlockAndWriteNothing(t *testing.T) {
	db := openTestDB(t)
	ctx, now := seedTopology(t, db)

	eff, err := TightenProjectIsolation(ctx, db, "app", core.IsolationSealed, now)
	require.ErrorIs(t, err, ErrIsolationHasChildren)
	require.Equal(t, []string{"kid"}, eff.Children, "the refusal still names what to re-parent")

	// A blocked tighten is not a partial one: state, parent, and family all stand.
	p, ok, err := ProjectBySlug(ctx, db, "app")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, core.IsolationOpen, p.Isolation)
	require.Equal(t, "platform", p.ParentSlug)
	fams, err := ProjectFamilies(ctx, db)
	require.NoError(t, err)
	require.Equal(t, []string{"app", "sibling"}, fams["acme"])

	// Re-parenting the child clears the block.
	require.NoError(t, SetProjectParent(ctx, db, "kid", "platform", now))
	_, err = TightenProjectIsolation(ctx, db, "app", core.IsolationSealed, now)
	require.NoError(t, err)
}

func TestIsolatedSlugs(t *testing.T) {
	db := openTestDB(t)
	ctx, _ := seedTopology(t, db)
	require.NoError(t, SetProjectIsolation(ctx, db, "app", core.IsolationConfidential))
	require.NoError(t, SetProjectIsolation(ctx, db, "kid", core.IsolationSealed))

	got, err := IsolatedSlugs(ctx, db, []string{"sibling", " app ", "", "never-registered", "kid"})
	require.NoError(t, err)
	require.Equal(t, []ProjectIsolation{
		{Slug: "app", State: core.IsolationConfidential},
		{Slug: "kid", State: core.IsolationSealed},
	}, got)

	got, err = IsolatedSlugs(ctx, db, []string{"sibling", "platform"})
	require.NoError(t, err)
	require.Empty(t, got)
}

// TestSetProjectParent_IsolationRequiresStandalone pins the link-side half of
// the topology rule: an isolated project may neither take a parent nor be one,
// while a DETACH stays open to it so a project that acquired a fence and a
// parent by some other route can still comply. The refused cases also pin the
// copy contract -- the refusal names the fenced slug and its state, states the
// remedy, and echoes nothing the fence exists to keep in.
func TestSetProjectParent_IsolationRequiresStandalone(t *testing.T) {
	const secret = "the acquisition roadmap"

	tests := []struct {
		name        string
		preAttach   bool // wire kid -> root while both are still open
		child       core.Isolation
		parent      core.Isolation
		arg         string // the parent argument under test
		wantRefused bool
		wantParent  string
		wantCopy    []string
	}{
		{name: "open child takes an open parent", child: core.IsolationOpen, parent: core.IsolationOpen,
			arg: "root", wantParent: "root"},
		{name: "confidential child may not take a parent", child: core.IsolationConfidential, parent: core.IsolationOpen,
			arg: "root", wantRefused: true, wantCopy: []string{"kid is confidential"}},
		{name: "sealed child may not take a parent", child: core.IsolationSealed, parent: core.IsolationOpen,
			arg: "root", wantRefused: true, wantCopy: []string{"kid is sealed"}},
		{name: "confidential project may not be a parent", child: core.IsolationOpen, parent: core.IsolationConfidential,
			arg: "root", wantRefused: true, wantCopy: []string{"root is confidential"}},
		{name: "sealed project may not be a parent", child: core.IsolationOpen, parent: core.IsolationSealed,
			arg: "root", wantRefused: true, wantCopy: []string{"root is sealed"}},
		{name: "both fenced names both sides", child: core.IsolationSealed, parent: core.IsolationConfidential,
			arg: "root", wantRefused: true, wantCopy: []string{"kid is sealed", "root is confidential"}},
		{name: "a fenced child may still detach", preAttach: true, child: core.IsolationSealed, parent: core.IsolationOpen,
			arg: "", wantParent: ""},
		{name: "a fenced child may detach from a fenced parent", preAttach: true, child: core.IsolationConfidential,
			parent: core.IsolationSealed, arg: "", wantParent: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := openTestDB(t)
			ctx := context.Background()
			seeded := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
			now := seeded.Add(time.Hour)

			for _, slug := range []string{"kid", "root"} {
				require.NoError(t, CreateProject(ctx, db, core.Project{
					ID: "01" + slug, Slug: slug, Name: slug, Description: secret,
					CreatedAt: seeded, UpdatedAt: seeded,
				}))
			}
			if tt.preAttach {
				require.NoError(t, SetProjectParent(ctx, db, "kid", "root", seeded))
			}
			require.NoError(t, SetProjectIsolation(ctx, db, "kid", tt.child))
			require.NoError(t, SetProjectIsolation(ctx, db, "root", tt.parent))

			err := SetProjectParent(ctx, db, "kid", tt.arg, now)
			p, ok, perr := ProjectBySlug(ctx, db, "kid")
			require.NoError(t, perr)
			require.True(t, ok)

			if !tt.wantRefused {
				require.NoError(t, err)
				require.Equal(t, tt.wantParent, p.ParentSlug)
				require.Equal(t, now, p.UpdatedAt.UTC(), "a topology change bumps updated_at")
				return
			}

			require.ErrorIs(t, err, ErrIsolationStandalone)
			for _, want := range tt.wantCopy {
				require.Contains(t, err.Error(), want, "the refusal names the fenced side and its state")
			}
			require.Contains(t, err.Error(), "set isolation back to open",
				"the refusal states the remedy, not just the rule")
			require.NotContains(t, err.Error(), secret,
				"a refusal never echoes what the fence keeps in")

			require.Empty(t, p.ParentSlug, "a refused link writes nothing")
			require.Equal(t, seeded, p.UpdatedAt.UTC(), "a refused link does not bump updated_at")
		})
	}
}

// TestSetProjectParent_ReattachAfterDetachNeedsTheFenceDown walks the round trip
// the guard is really about: a fenced project detaches, cannot re-attach while
// the fence stands, and can once it is back to open.
func TestSetProjectParent_ReattachAfterDetachNeedsTheFenceDown(t *testing.T) {
	db := openTestDB(t)
	ctx, now := seedTopology(t, db)
	require.NoError(t, SetProjectIsolation(ctx, db, "app", core.IsolationSealed))

	// The detach is how a fenced project reaches the standalone state.
	require.NoError(t, SetProjectParent(ctx, db, "app", "", now))
	p, ok, err := ProjectBySlug(ctx, db, "app")
	require.NoError(t, err)
	require.True(t, ok)
	require.Empty(t, p.ParentSlug)

	err = SetProjectParent(ctx, db, "app", "platform", now)
	require.ErrorIs(t, err, ErrIsolationStandalone)

	require.NoError(t, SetProjectIsolation(ctx, db, "app", core.IsolationOpen))
	require.NoError(t, SetProjectParent(ctx, db, "app", "platform", now))
	p, ok, err = ProjectBySlug(ctx, db, "app")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "platform", p.ParentSlug)
}

// insertStampedMemory inserts a memories_index row carrying a source_session
// provenance stamp. invalidAt "" means active.
func insertStampedMemory(t *testing.T, db *sql.DB, id, name, project, stamp, updated, invalidAt string) {
	t.Helper()
	var inv any
	if invalidAt != "" {
		inv = invalidAt
	}
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO memories_index
		    (id, kind, name, description, project, file_path, tags, valid_from,
		     invalid_at, superseded_by, source_session, content_hash, created_at, updated_at)
		VALUES (?, 'gotcha', ?, ?, ?, ?, '[]', ?, ?, NULL, ?, 'h', ?, ?)`,
		id, name, name+" description", project,
		"memory/"+dirOf(project)+"/"+name+".md", updated, inv, stamp, updated, updated)
	require.NoError(t, err)
}

// insertProvenanceSession registers a session so a memory stamp can resolve to
// a project.
func insertProvenanceSession(t *testing.T, db *sql.DB, id, name, project string, at time.Time) {
	t.Helper()
	require.NoError(t, CreateSession(context.Background(), db, core.Session{
		ID: id, Name: name, ProjectSlug: project, Status: core.SessionActive,
		CreatedAt: at, UpdatedAt: at,
	}))
}

// TestGlobalMemoriesFromProjectSessions_TracesLeaksByEitherStamp pins the
// provenance audit: only ACTIVE, GLOBAL memories, only those whose stamp
// resolves to this project's sessions, matched by either spelling the stamp can
// take (session ULID for bound writes, session name for ambient ones).
func TestGlobalMemoriesFromProjectSessions_TracesLeaksByEitherStamp(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	ts := func(min int) string { return core.FormatTime(base.Add(time.Duration(min) * time.Minute)) }

	boundID := "01JBOUNDSESSION00000000001"
	insertProvenanceSession(t, db, boundID, "cc/bound1", "acme", base)
	insertProvenanceSession(t, db, "01JAMBIENTSESSION00000000", "cc/ambient1", "acme", base)
	insertProvenanceSession(t, db, "01JOTHERSESSION000000000", "cc/other1", "other", base)

	// The leaks: global memories this project's sessions wrote, one stamped with
	// the session ULID (bound write) and one with the session name (ambient).
	insertStampedMemory(t, db, "01M1", "bound-leak", "", boundID, ts(9), "")
	insertStampedMemory(t, db, "01M2", "ambient-leak", "", "cc/ambient1", ts(8), "")

	// Everything the audit must NOT claim.
	insertStampedMemory(t, db, "01M3", "other-project-leak", "", "cc/other1", ts(7), "")
	insertStampedMemory(t, db, "01M4", "unstamped", "", "", ts(6), "")
	insertStampedMemory(t, db, "01M5", "already-inside", "acme", boundID, ts(5), "")
	insertStampedMemory(t, db, "01M6", "archived-leak", "", boundID, ts(4), ts(10))
	insertStampedMemory(t, db, "01M7", "orphan-stamp", "", "cc/vanished", ts(3), "")

	got, err := GlobalMemoriesFromProjectSessions(ctx, db, "acme")
	require.NoError(t, err)
	require.Equal(t, []string{"bound-leak", "ambient-leak"}, memNames(got),
		"newest-updated first, and nothing but this project's global provenance")

	// A project whose sessions never wrote globally has no leak to repair.
	got, err = GlobalMemoriesFromProjectSessions(ctx, db, "other")
	require.NoError(t, err)
	require.Equal(t, []string{"other-project-leak"}, memNames(got))
	got, err = GlobalMemoriesFromProjectSessions(ctx, db, "never-registered")
	require.NoError(t, err)
	require.Empty(t, got)

	// project_slug = '' rows are real global sessions, so "" is ambiguous rather
	// than "every project" -- answering it would overstate the leak.
	_, err = GlobalMemoriesFromProjectSessions(ctx, db, "  ")
	require.Error(t, err)
}

// TestMigration019_ExistingRowsDefaultOpen upgrades a database that already
// holds a pre-isolation project row and verifies the new column lands with
// 'open' for it.
func TestMigration019_ExistingRowsDefaultOpen(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "seam.db")
	dsn := "file:" + url.PathEscape(dbPath) + "?_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	// Cut the list at the migration under test rather than at its end, so
	// appending later migrations does not silently re-point this test.
	all := migrationList()
	before := 0
	for i, m := range all {
		if m.Version == 19 {
			before = i
			break
		}
	}
	require.NotZero(t, before, "migration 19 must exist and must not be the first")
	require.NoError(t, migrate(db, all[:before]))

	_, err = db.Exec(
		`INSERT INTO projects (id, slug, name, description, created_at, updated_at)
		 VALUES ('01OLD', 'legacy', 'Legacy', '', '2026-07-01T00:00:00Z', '2026-07-01T00:00:00Z')`)
	require.NoError(t, err)

	require.NoError(t, migrate(db, all))

	p, ok, err := ProjectBySlug(context.Background(), db, "legacy")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, core.IsolationOpen, p.Isolation)
}

// recordDestProjects registers example projects in every isolation state for
// the RecordDestinationOf tests: two open, two confidential, one sealed.
func recordDestProjects(t *testing.T, db *sql.DB) context.Context {
	t.Helper()
	ctx := context.Background()
	at := time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)
	for i, p := range []struct {
		slug  string
		state core.Isolation
	}{
		{"example-ops", core.IsolationOpen},
		{"example-docs", core.IsolationOpen},
		{"example-finance", core.IsolationConfidential},
		{"example-legal", core.IsolationConfidential},
		{"example-sealed", core.IsolationSealed},
	} {
		require.NoError(t, CreateProject(ctx, db, core.Project{
			ID: "01PRJ" + string(rune('A'+i)), Slug: p.slug, Name: p.slug, Isolation: p.state,
			CreatedAt: at, UpdatedAt: at,
		}))
	}
	return ctx
}

// TestRecordDestination_Matrix pins where a conversation's own record goes,
// from the isolation of the projects its ambient row and its links (explicit
// sessions sharing its external identity, in any status) are in: the ambient
// row while no isolated project is held, or while the ambient row's own is one
// of them (a native conversation keeps its record at home, whatever it binds
// to); otherwise the newest link in the one isolated project its links reach,
// and nowhere for two or more.
func TestRecordDestination_Matrix(t *testing.T) {
	type link struct {
		project string
		status  core.SessionStatus
	}
	tests := []struct {
		name    string
		ambient string // "-" for a conversation with no ambient row
		links   []link // oldest first
		kind    RecordDestKind
		link    int // index into links of the destination, for RecordDestLink
		fenced  []string
	}{
		{name: "open, nothing linked", ambient: "example-ops", kind: RecordDestAmbient},
		{name: "open, linked to another open project", ambient: "example-ops",
			links: []link{{"example-docs", core.SessionActive}}, kind: RecordDestAmbient},
		{name: "open, linked to confidential", ambient: "example-ops",
			links: []link{{"example-finance", core.SessionActive}},
			kind:  RecordDestLink, link: 0, fenced: []string{"example-finance"}},
		{name: "open, linked to sealed", ambient: "example-ops",
			links: []link{{"example-sealed", core.SessionActive}},
			kind:  RecordDestLink, link: 0, fenced: []string{"example-sealed"}},
		{name: "global, linked to confidential", ambient: "",
			links: []link{{"example-finance", core.SessionActive}},
			kind:  RecordDestLink, link: 0, fenced: []string{"example-finance"}},
		{name: "a rebind back to open keeps the destination", ambient: "example-ops",
			links: []link{{"example-finance", core.SessionActive}, {"example-docs", core.SessionActive}},
			kind:  RecordDestLink, link: 0, fenced: []string{"example-finance"}},
		{name: "an ended link still counts", ambient: "example-ops",
			links: []link{{"example-finance", core.SessionCompleted}},
			kind:  RecordDestLink, link: 0, fenced: []string{"example-finance"}},
		{name: "an expired link still counts", ambient: "example-ops",
			links: []link{{"example-sealed", core.SessionExpired}},
			kind:  RecordDestLink, link: 0, fenced: []string{"example-sealed"}},
		{name: "two links into one isolated project: the newest", ambient: "example-ops",
			links: []link{{"example-finance", core.SessionCompleted}, {"example-finance", core.SessionActive}},
			kind:  RecordDestLink, link: 1, fenced: []string{"example-finance"}},
		{name: "no ambient row, linked to confidential", ambient: "-",
			links: []link{{"example-finance", core.SessionActive}},
			kind:  RecordDestLink, link: 0, fenced: []string{"example-finance"}},
		{name: "native confidential, nothing linked", ambient: "example-finance",
			kind: RecordDestAmbient, fenced: []string{"example-finance"}},
		{name: "native confidential, linked to open", ambient: "example-finance",
			links: []link{{"example-ops", core.SessionActive}},
			kind:  RecordDestAmbient, fenced: []string{"example-finance"}},
		{name: "native confidential, linked to itself", ambient: "example-finance",
			links: []link{{"example-finance", core.SessionActive}},
			kind:  RecordDestAmbient, fenced: []string{"example-finance"}},
		{name: "native confidential, linked to another confidential", ambient: "example-finance",
			links: []link{{"example-legal", core.SessionActive}},
			kind:  RecordDestAmbient, fenced: []string{"example-finance", "example-legal"}},
		{name: "native confidential, linked to sealed", ambient: "example-finance",
			links: []link{{"example-sealed", core.SessionExpired}},
			kind:  RecordDestAmbient, fenced: []string{"example-finance", "example-sealed"}},
		{name: "native sealed, linked to confidential", ambient: "example-sealed",
			links: []link{{"example-finance", core.SessionCompleted}},
			kind:  RecordDestAmbient, fenced: []string{"example-finance", "example-sealed"}},
		{name: "native confidential, linked to two other isolated projects", ambient: "example-finance",
			links: []link{{"example-legal", core.SessionActive}, {"example-sealed", core.SessionActive}},
			kind:  RecordDestAmbient, fenced: []string{"example-finance", "example-legal", "example-sealed"}},
		{name: "native confidential, linked to another confidential and back home", ambient: "example-finance",
			links: []link{{"example-legal", core.SessionCompleted}, {"example-finance", core.SessionActive}},
			kind:  RecordDestAmbient, fenced: []string{"example-finance", "example-legal"}},
		{name: "open, linked to two isolated projects", ambient: "example-ops",
			links: []link{{"example-finance", core.SessionActive}, {"example-sealed", core.SessionExpired}},
			kind:  RecordDestWithheld, fenced: []string{"example-finance", "example-sealed"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := openTestDB(t)
			ctx := recordDestProjects(t, db)
			base := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
			const ext = "conv-0001"
			if tc.ambient != "-" {
				linkSession(t, db, "01AMB", tc.ambient, true, "claude-code", ext, core.SessionActive, base)
			}
			ids := make([]string, len(tc.links))
			for i, l := range tc.links {
				ids[i] = "01LNK" + string(rune('A'+i))
				linkSession(t, db, ids[i], l.project, false, "claude-code", ext, l.status, base.Add(time.Duration(i+1)*time.Minute))
			}
			// Another conversation's link never counts.
			linkSession(t, db, "01OTHER", "example-legal", false, "claude-code", "conv-0002", core.SessionActive, base)

			d, err := RecordDestinationOf(ctx, db, "claude-code", ext)
			require.NoError(t, err)
			require.Equal(t, tc.kind, d.Kind)
			require.Equal(t, tc.fenced, d.Fenced)
			require.Equal(t, tc.ambient != "-", d.HasAmbient)
			require.Len(t, d.Links, len(tc.links))
			if tc.kind == RecordDestLink {
				require.Equal(t, ids[tc.link], d.Session.ID)
			} else {
				require.Empty(t, d.Session.ID)
			}
			for _, f := range tc.fenced {
				require.True(t, d.Holds(f))
			}
			require.False(t, d.Holds("example-ops"))
		})
	}
}

// An incomplete identity names no conversation: the ambient row, as always,
// with nothing looked up.
func TestRecordDestination_IncompleteIdentity(t *testing.T) {
	db := openTestDB(t)
	ctx := recordDestProjects(t, db)
	for _, id := range [][2]string{{"", "conv-0001"}, {"claude-code", ""}} {
		d, err := RecordDestinationOf(ctx, db, id[0], id[1])
		require.NoError(t, err)
		require.Equal(t, RecordDestination{}, d)
	}
}
