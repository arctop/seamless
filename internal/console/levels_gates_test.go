package console

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/store"
)

// levelGate finds a template's level gates and the level each names.
var levelGate = regexp.MustCompile(`\.Level\.AtLeast "([a-z]+)"`)

// Every level gate in a template is a registered surface, and every registered
// surface is a gate in the template it names -- the same bargain as
// features.Surfaces. A gate added without its phrase would hide something the
// Experience cards and the docs never mention; a phrase without its gate would
// promise something no level does. Counted per template and per level, so a
// gate whose level changed without its phrase fails too.
func TestSurfaces_EveryTemplateGateIsRegistered(t *testing.T) {
	entries, err := templateFS.ReadDir("templates")
	require.NoError(t, err)

	found := map[string][]string{}
	for _, e := range entries {
		src, err := templateFS.ReadFile("templates/" + e.Name())
		require.NoError(t, err)
		for _, m := range levelGate.FindAllStringSubmatch(string(src), -1) {
			found[e.Name()] = append(found[e.Name()], m[1])
		}
	}
	registered := map[string][]string{}
	for _, sf := range surfaces {
		registered[sf.Template] = append(registered[sf.Template], sf.Min.String())
	}
	for name := range found {
		slices.Sort(found[name])
	}
	for name := range registered {
		slices.Sort(registered[name])
	}
	for name, gates := range found {
		require.Equal(t, gates, registered[name],
			"templates/%s gates %v, but screens.go registers %v for it: register one surface (with its "+
				"owner-facing phrase) per {{if $.Level.AtLeast ...}} gate, in the same change", name, gates, registered[name])
	}
	for name := range registered {
		_, ok := found[name]
		require.True(t, ok, "screens.go registers surfaces for templates/%s, which has no level gate", name)
	}
}

// Surfaces are well formed: a gate hides something from a lower level, so
// none can sit at the first level, and each reads as a phrase.
func TestSurfaces_WellFormed(t *testing.T) {
	require.NotEmpty(t, surfaces)
	for _, sf := range surfaces {
		require.Contains(t, allLevels(), sf.Min, sf.Label)
		require.Greater(t, sf.Min, levelBasic, "%s: a gate at the first level hides nothing", sf.Label)
		require.NotEmpty(t, sf.Where, sf.Label)
		require.NotEmpty(t, sf.Label)
		_, err := os.Stat(filepath.Join("templates", sf.Template))
		require.NoError(t, err, "%s names a template that does not exist", sf.Label)
	}
	// The Experience cards name every surface, grouped by page.
	var named []string
	for _, lvl := range allLevels() {
		named = append(named, levelAdds(config.Features{Research: true}, lvl)...)
	}
	joined := strings.Join(named, "\n")
	for _, sf := range surfaces {
		require.Contains(t, joined, sf.Label, "the Experience cards must list %q", sf.Label)
	}
}

// present asserts each marker is in body; absent that none is.
func present(t *testing.T, body string, markers ...string) {
	t.Helper()
	for _, m := range markers {
		require.True(t, strings.Contains(body, m), "missing %q", m)
	}
}

func absent(t *testing.T, body string, markers ...string) {
	t.Helper()
	for _, m := range markers {
		require.False(t, strings.Contains(body, m), "unexpected %q", m)
	}
}

// Memories at Basic: no Reach or Utility sort, no surfaced counts, no utility
// score in the reader -- and the hidden sort still works by URL.
func TestLevelGates_Memories(t *testing.T) {
	db, mgr, mux := newConsoleWithFiles(t)
	mem := writeMemory(t, mgr, core.KindGotcha, "seamless", "boot-race", "the boot race")

	setLevel(t, db, "basic")
	list := getPeek(t, mux, "/console/memories").Body.String()
	absent(t, list, "sort=reach", "sort=utility", "never surfaced")
	present(t, list, "sort=recent", "sort=name", "sort=favorites")
	reader := getPeek(t, mux, "/console/memories/"+mem.ID+"?reader=1").Body.String()
	absent(t, reader, "<span>utility</span>")
	present(t, reader, "since surfaced")
	rr := getPeek(t, mux, "/console/memories?sort=utility")
	require.Equal(t, http.StatusOK, rr.Code, "a hidden sort still works by URL")
	require.Contains(t, rr.Body.String(), `aria-label="Sort: Utility"`)

	setLevel(t, db, "standard")
	list = getPeek(t, mux, "/console/memories").Body.String()
	present(t, list, "sort=reach", "sort=utility", "never surfaced")
	present(t, getPeek(t, mux, "/console/memories/"+mem.ID+"?reader=1").Body.String(), "<span>utility</span>")
}

// Sessions at Basic: search, Status, and Updated stay; Retained and Sort go,
// and so does a session's review-signal scorecard.
func TestLevelGates_Sessions(t *testing.T) {
	db, mux := newConsoleLevel(t, config.Features{}, "basic")
	now := time.Now().UTC()
	id := mustID(t)
	require.NoError(t, store.CreateSession(context.Background(), db, core.Session{
		ID: id, Name: "cc/one", Status: core.SessionActive, CreatedAt: now, UpdatedAt: now,
	}))

	list := getPeek(t, mux, "/console/sessions").Body.String()
	absent(t, list, `<span class="mv2-sort-label">Retained</span>`, `class="menu sort-menu"`)
	present(t, list, `<span class="mv2-sort-label">Status</span>`, `<span class="mv2-sort-label">Updated</span>`, `name="q"`)
	absent(t, getPeek(t, mux, "/console/sessions/"+id).Body.String(), "session-scorecard")
	require.Equal(t, http.StatusOK, getPeek(t, mux, "/console/sessions?retained=no&sort=name").Code,
		"hidden filters still work by URL")

	setLevel(t, db, "standard")
	present(t, getPeek(t, mux, "/console/sessions").Body.String(),
		`<span class="mv2-sort-label">Retained</span>`, `class="menu sort-menu"`)
	present(t, getPeek(t, mux, "/console/sessions/"+id).Body.String(), "session-scorecard")
}

// Gardener at Basic: the ask box, three examples, the queue with Apply and
// Dismiss, and Recently decided stay; the filters, the split example, Hide
// forever and its ledger, and Retarget go.
func TestLevelGates_Gardener(t *testing.T) {
	ctx, db, _, mux := newConsoleWithGardener(t)
	seeded := seedQueue(t, ctx, db)
	toolErr := seeded[store.ProposalToolError]
	require.Equal(t, http.StatusSeeOther, post(mux, "/console/gardener/"+seeded[store.ProposalArchive].ID+"/hide").Code)

	setLevel(t, db, "basic")
	page := getPeek(t, mux, "/console/gardener").Body.String()
	absent(t, page, `<span class="mv2-sort-label">Type</span>`, `<span class="mv2-sort-label">Source</span>`,
		`id="rg-hidden"`)
	reader := getPeek(t, mux, "/console/gardener/"+toolErr.ID+"?reader=1").Body.String()
	absent(t, reader, "/hide")
	present(t, reader, "/dismiss", "/apply")

	setLevel(t, db, "standard")
	page = getPeek(t, mux, "/console/gardener").Body.String()
	present(t, page, `<span class="mv2-sort-label">Type</span>`, `<span class="mv2-sort-label">Source</span>`,
		`id="rg-hidden"`)
	present(t, getPeek(t, mux, "/console/gardener/"+toolErr.ID+"?reader=1").Body.String(), "/hide")

	// The ask box (it needs a chat provider) keeps three examples at Basic; the
	// project split is a Standard pattern.
	_, chatDB, chatMux := newConsoleWithChat(t, `{"ops":[]}`)
	setLevel(t, chatDB, "basic")
	page = getPeek(t, chatMux, "/console/gardener").Body.String()
	present(t, page, ">Merge duplicates</button>", ">Archive old guidance</button>", ">Correct scope</button>")
	absent(t, page, ">Split a project</button>")
	setLevel(t, chatDB, "standard")
	present(t, getPeek(t, chatMux, "/console/gardener").Body.String(), ">Split a project</button>")
}

func TestLevelGates_GardenerRetarget(t *testing.T) {
	ctx, db, mux := newConsoleForSplit(t, stubChat{out: splitConsoleJSON})
	require.Equal(t, http.StatusSeeOther, postForm(mux, "/console/gardener/split", "source=example-app").Code)
	reps, err := store.PendingProposals(ctx, db, store.ProposalReproject)
	require.NoError(t, err)
	require.NotEmpty(t, reps)

	setLevel(t, db, "basic")
	absent(t, getPeek(t, mux, "/console/gardener/"+reps[0].ID+"?reader=1").Body.String(), "gardener-retarget")
	setLevel(t, db, "standard")
	present(t, getPeek(t, mux, "/console/gardener/"+reps[0].ID+"?reader=1").Body.String(), "gardener-retarget")
}

// Search at Basic: no time window and no sort; the hidden params still work.
func TestLevelGates_Search(t *testing.T) {
	db, mux := newConsoleLevel(t, config.Features{}, "basic")
	page := getPeek(t, mux, "/console/search?q=anything").Body.String()
	absent(t, page, "search-time-pills", `class="search-sort"`)
	present(t, page, "Starred only")
	require.Equal(t, http.StatusOK, getPeek(t, mux, "/console/search?q=anything&w=7d&sort=newest").Code)

	setLevel(t, db, "standard")
	present(t, getPeek(t, mux, "/console/search?q=anything").Body.String(), "search-time-pills", `class="search-sort"`)
}

// Event pages below Advanced keep the evidence and provenance; the decoded
// payload fields and the raw JSON are Advanced -- in the page and the pane.
func TestLevelGates_EventDetail(t *testing.T) {
	db, mux := newConsoleLevel(t, config.Features{}, "standard")
	id, err := events.NewRecorder(db).Record(context.Background(), core.Event{
		Kind: core.EventMemoryWritten, Payload: map[string]any{"name": "boot-race", "extra": "value"},
	})
	require.NoError(t, err)

	for _, path := range []string{"/console/events/" + id, "/console/events/" + id + "?peek=1"} {
		absent(t, getPeek(t, mux, path).Body.String(), "event-payload-card", "event-raw-card")
	}
	var data eventDetailData
	getJSON(t, mux, "/console/events/"+id+"?format=json", &data)
	require.NotEmpty(t, data.RawJSON, "the JSON answer is level-blind")

	setLevel(t, db, "advanced")
	for _, path := range []string{"/console/events/" + id, "/console/events/" + id + "?peek=1"} {
		present(t, getPeek(t, mux, path).Body.String(), "event-payload-card", "event-raw-card")
	}
}

// Settings at Basic: Features keeps its cards, blurbs, data lines, and the
// "nothing is deleted" promise, but not the precedence line or the tool
// names; Workspaces' repo hosts and unbound routes are Advanced.
func TestLevelGates_Settings(t *testing.T) {
	db, mux := newConsoleLevel(t, config.Features{}, "basic")
	ctx := context.Background()
	_, err := store.EnsureProject(ctx, db, "seamless", "Seamless")
	require.NoError(t, err)
	require.NoError(t, store.AddRepoMapping(ctx, db, "/Users/x/repos/seamless", "seamless"))
	// A route with no slug is "unbound" (the store's mutators refuse to write
	// one now; older installs and hand repairs still carry them).
	_, err = db.Exec(`INSERT INTO repo_map (host, path, slug, created_at) VALUES ('', '/Users/x/repos/orphan', '', ?)`,
		core.FormatTime(time.Now()))
	require.NoError(t, err)

	features := getPeek(t, mux, "/console/settings?s=features").Body.String()
	absent(t, features, `class="settings-precedence`, "(lab_open, trial_record, trial_query)")
	present(t, features, "3 agent tools", "nothing is ever deleted", `name="feature_research"`)

	setLevel(t, db, "standard")
	features = getPeek(t, mux, "/console/settings?s=features").Body.String()
	present(t, features, `class="settings-precedence`, "(lab_open, trial_record, trial_query)")
	workspaces := getPeek(t, mux, "/console/settings?s=workspaces").Body.String()
	absent(t, workspaces, `title="Machine this checkout lives on"`, "workspace-unbound")
	present(t, workspaces, "/Users/x/repos/seamless")

	setLevel(t, db, "advanced")
	workspaces = getPeek(t, mux, "/console/settings?s=workspaces").Body.String()
	present(t, workspaces, `title="Machine this checkout lives on"`, "workspace-unbound")
}
