package console

// Tests of the sidebar that fits the screen, with badges that mean "act"
// (plan:console-ux-pass UX 13): every badge is a queue or a pulse, a queue
// at zero draws nothing, the queues count what the Overview's needs model
// counts through the request's handle, a failed count costs its badge and
// never the page, and the frame keeps its head and foot on screen.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/plans"
	"github.com/arctop/seamless/internal/store"
)

// sbBadge is one sidebar link's badge as the page draws it.
type sbBadge struct {
	Drawn bool
	Kind  string
	Count int
	Zero  bool
}

var (
	sbNavRe   = regexp.MustCompile(`(?s)<nav class="nav"[^>]*>.*?</nav>`)
	sbLinkRe  = regexp.MustCompile(`<a href="(/console/[^"]*)" data-key="[^"]*" data-tip="[^"]*" class="[^"]*">(.*?)</a>`)
	sbCountRe = regexp.MustCompile(`<span class="count" data-badge="([a-z]+)"[^>]*?( data-zero)?>(\d+)</span>`)
)

// sbNav is the page's sidebar nav element.
func sbNav(t *testing.T, page string) string {
	t.Helper()
	nav := sbNavRe.FindString(page)
	require.NotEmpty(t, nav, "the page draws the sidebar")
	return nav
}

// sbBadges reads every sidebar link's badge, keyed by href.
func sbBadges(t *testing.T, page string) map[string]sbBadge {
	t.Helper()
	out := map[string]sbBadge{}
	for _, m := range sbLinkRe.FindAllStringSubmatch(sbNav(t, page), -1) {
		b := sbBadge{}
		if strings.Contains(m[2], `class="count"`) {
			c := sbCountRe.FindStringSubmatch(m[2])
			require.NotNil(t, c, "a drawn badge names its kind: %s", m[2])
			n, err := strconv.Atoi(c[3])
			require.NoError(t, err)
			b = sbBadge{Drawn: true, Kind: c[1], Count: n, Zero: c[2] != ""}
		}
		out[m[1]] = b
	}
	return out
}

func sbPage(t *testing.T, mux *http.ServeMux, path string) string {
	t.Helper()
	rr := getPeek(t, mux, path)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	return rr.Body.String()
}

// The product decision, transcribed on purpose (as the level matrix is):
// which sections wait on the owner, which one is a pulse, and that every
// other section carries no number at all.
var (
	sbQueues = []string{"gardener", "held", "plans", "tasks", "locks", "access", "mishaps"}
	sbPulses = []string{"now"}
)

// Every badge any screen can show has a kind and a navCounts field, and the
// kinds are exactly the decision above: no inventory total is a badge.
func TestSbBadges_EveryBadgeHasAKindAndAField(t *testing.T) {
	var queues, pulses []string
	for _, sc := range knownScreens() {
		switch {
		case sc.Badge == "":
			require.Empty(t, sc.BadgeKind, "%s has a badge kind and no badge", sc.ID)
			require.Empty(t, sc.BadgeTitle, "%s has a badge tooltip and no badge", sc.ID)
			continue
		case sc.BadgeKind == badgeQueue:
			queues = append(queues, sc.ID)
		case sc.BadgeKind == badgePulse:
			pulses = append(pulses, sc.ID)
		default:
			t.Fatalf("%s shows badge %q with no kind (%q): give it badgeQueue or badgePulse", sc.ID, sc.Badge, sc.BadgeKind)
		}
		_, ok := navCounts{}.count(sc.Badge)
		require.True(t, ok, "%s shows badge %q, which no navCounts field answers", sc.ID, sc.Badge)
		require.NotEmpty(t, sc.BadgeTitle, "%s's badge says what it counts", sc.ID)
		require.True(t, sc.NavRow, "%s has a badge and no sidebar link to carry it", sc.ID)
	}
	slices.Sort(queues)
	want := slices.Clone(sbQueues)
	slices.Sort(want)
	require.Equal(t, want, queues)
	require.Equal(t, sbPulses, pulses)
}

// An inventory screen renders no count, a queue at zero renders none, and
// the pulse renders its zero, dimmed. With nothing waiting the nav is not
// marked, so neither the rail nor the phone bar carries a dot.
func TestSbSidebar_InventoryRendersNoCountAndAQueueAtZeroNone(t *testing.T) {
	c := ndNew(t, "advanced", featuresAll(true))
	ctx := context.Background()
	writeMemory(t, c.mgr, core.KindGotcha, "example-ops", "sb-rule", "a rule")
	writeNote(t, c.mgr, "example-ops", "sb-note", "A note", "# A note")
	ndSession(t, c.db, "cc/sb-done", "example-ops", false)
	_, err := store.EnsureProject(ctx, c.db, "example-ops", "example-ops")
	require.NoError(t, err)
	// An approved capture and a closed gate are no queue.
	ndCapture(t, c.mgr, "example-ops", "approved-plan", plans.StatusApproved)
	ndTask(t, c.db, core.Task{ProjectSlug: "example-ops", Title: "signed", Kind: core.TaskKindGate, Status: core.TaskDone,
		Gate: core.TaskGate{Who: "owner", What: "sign"}})

	page := sbPage(t, c.mux, "/console/settings")
	badges := sbBadges(t, page)
	for _, href := range []string{"/console/memories", "/console/notes", "/console/sessions", "/console/projects",
		"/console/labs", "/console/trials", "/console/", "/console/settings"} {
		b, ok := badges[href]
		require.True(t, ok, "%s is on the sidebar", href)
		require.False(t, b.Drawn, "%s renders no count", href)
	}
	for _, href := range []string{"/console/gardener", "/console/held", "/console/plans", "/console/tasks", "/console/locks",
		"/console/mishaps"} {
		require.False(t, badges[href].Drawn, "a queue at zero renders nothing: %s", href)
	}
	require.Equal(t, sbBadge{Drawn: true, Kind: "pulse", Count: 0, Zero: true}, badges["/console/now"],
		"the pulse draws its zero, dimmed")
	require.NotContains(t, sbNav(t, page), "data-waiting", "nothing waits: no dot anywhere")
}

// The Tasks badge counts what waits on the owner: the gates on the owner
// (a gate, and an open step whose own gate is open), the lapsed claims and
// the claimless starts. A gate on someone else, a step held only by a gate it
// depends on, a cleared gate and a live claim count nothing, whether or not
// the live claim's holder is still live.
func TestSbTasksBadge_CountsGatesOnTheOwnerAndLooseClaims(t *testing.T) {
	c := ndNew(t, "advanced", config.Features{})
	ctx := context.Background()
	now := time.Now().UTC()
	gate := ndTask(t, c.db, core.Task{ProjectSlug: "example-ops", Title: "Sign the renewal", Kind: core.TaskKindGate,
		Gate: core.TaskGate{Who: "owner", What: "sign the renewal"}})
	// Held only by the gate it depends on: counted through that gate.
	ndTask(t, c.db, core.Task{ProjectSlug: "example-ops", Title: "Order the disks", DependsOn: []string{gate}})
	// Its own gate waits on the owner, in the owner's class form.
	ndTask(t, c.db, core.Task{ProjectSlug: "example-ops", Title: "Turn on the firewall profile",
		Gate: core.TaskGate{Who: "Owner:ops", What: "pick the window"}})
	ndTask(t, c.db, core.Task{ProjectSlug: "example-ops", Title: "Review the change", Kind: core.TaskKindGate,
		Gate: core.TaskGate{Who: "reviewer:dana", What: "read it"}})
	cleared := ndTask(t, c.db, core.Task{ProjectSlug: "example-ops", Title: "Approved already",
		Gate: core.TaskGate{Who: "owner", What: "approve"}})
	_, err := store.UpdateTask(ctx, c.db, cleared, store.TaskPatch{ClearGate: &store.TaskGateClear{By: "owner"}}, "", now)
	require.NoError(t, err)
	gone := ndSession(t, c.db, "cc/sb-gone", "example-ops", false)
	live := ndSession(t, c.db, "cc/sb-live", "example-ops", true)
	expired, lease := now.Add(-15*time.Minute), now.Add(time.Hour)
	ndTask(t, c.db, core.Task{ProjectSlug: "example-ops", Title: "Move the exporter", PlanSlug: "metrics",
		Status: core.TaskInProgress, ClaimedBy: gone, LeaseExpiresAt: &expired})
	ndTask(t, c.db, core.Task{ProjectSlug: "example-ops", Title: "Tidy the routes", Status: core.TaskInProgress})
	ndTask(t, c.db, core.Task{ProjectSlug: "example-ops", Title: "Rotate the key", Status: core.TaskInProgress,
		ClaimedBy: live, LeaseExpiresAt: &lease})
	ndTask(t, c.db, core.Task{ProjectSlug: "example-ops", Title: "Benchmark the pool", Status: core.TaskInProgress,
		ClaimedBy: gone, LeaseExpiresAt: &lease})

	page := sbPage(t, c.mux, "/console/memories")
	require.Equal(t, sbBadge{Drawn: true, Kind: "queue", Count: 4}, sbBadges(t, page)["/console/tasks"],
		"two gates on the owner, one lapsed claim, one claimless start")
	require.Contains(t, sbNav(t, page), "data-waiting", "a queue with something in it marks the nav")
}

// One definition, two surfaces: each queue badge is the count the Overview's
// matching needs item shows for the same state, the Tasks badge the sum of
// its three. The fixture holds one of every item plus the cases the two could
// disagree on: a gate on "Owner", an in-progress gate with no claim (a gate,
// once: never a claimless start, since no one claims a gate), a presented capture with no
// slug and an approved one, and a quiet holder's live claim.
func TestSbQueues_EqualTheNeedsModel(t *testing.T) {
	c := ndNew(t, "advanced", ndAllOn)
	ndFixture(t, c)
	now := time.Now().UTC()
	ndTask(t, c.db, core.Task{ProjectSlug: "example-web", Title: "Pick the vendor", Kind: core.TaskKindGate,
		Gate: core.TaskGate{Who: "Owner", What: "pick"}})
	ndTask(t, c.db, core.Task{ProjectSlug: "example-web", Title: "Walk the floor", Kind: core.TaskKindGate,
		Status: core.TaskInProgress, Gate: core.TaskGate{Who: "owner", What: "walk it"}})
	ndTask(t, c.db, core.Task{ProjectSlug: "example-web", Title: "Ship the banner",
		Gate: core.TaskGate{Who: "owner", What: "read the copy"}})
	quiet := ndSession(t, c.db, "cc/sb-quiet", "example-web", false)
	lease := now.Add(time.Hour)
	ndTask(t, c.db, core.Task{ProjectSlug: "example-web", Title: "Quiet holder", Status: core.TaskInProgress,
		ClaimedBy: quiet, LeaseExpiresAt: &lease})
	ndCapture(t, c.mgr, "example-web", "web-rollout", plans.StatusPresented)
	ndCapture(t, c.mgr, "example-web", "web-old", plans.StatusApproved)
	_, err := c.mgr.WriteNote(context.Background(), core.Note{ID: mustID(t), Slug: "cc-plan-loose", Title: "Loose capture",
		Project: "example-web", Body: "# loose", Tags: plans.SetStatusTag([]string{plans.TagPlan}, plans.StatusPresented),
		Created: now, Updated: now})
	require.NoError(t, err)

	ov := ndRead(t, c.mux)
	need := func(kind string) int {
		for _, card := range ov.Attention {
			if card.Kind == kind {
				return card.Count
			}
		}
		return 0
	}
	badges := sbBadges(t, sbPage(t, c.mux, "/console/"))
	got := func(href string) int { return badges[href].Count }
	require.Equal(t, need(needGates)+need(needLapsed)+need(needUnclaimed), got("/console/tasks"))
	require.Equal(t, need(needPlans), got("/console/plans"))
	require.Equal(t, need(needProposals), got("/console/gardener"))
	require.Equal(t, need(needHeld), got("/console/held"))
	require.Equal(t, need(needLocks), got("/console/locks"))
	require.Equal(t, need(needMishaps), got("/console/mishaps"))
	// And the fixture really exercises each one.
	require.Equal(t, 4, need(needGates))
	require.Equal(t, 2, need(needPlans))
	require.Equal(t, 1, need(needLapsed))
	require.Equal(t, 1, need(needUnclaimed), "the in-progress gate is counted once, with the gates")
	require.Equal(t, 1, need(needMishaps))
	for _, href := range []string{"/console/tasks", "/console/plans", "/console/gardener", "/console/held", "/console/locks",
		"/console/mishaps"} {
		require.Equal(t, "queue", badges[href].Kind, href)
	}
}

// Every count reads through the request's handle: a locked project's gate,
// lapsed claim, presented plan, mishap report and live agent are absent from
// the badges, and an unlock brings them back -- the absence is the view's.
func TestSbBadges_LockedProjectIsAbsent(t *testing.T) {
	lc := newLockedConsole(t)
	now := time.Now().UTC()
	ndTask(t, lc.db, core.Task{ProjectSlug: lockedSlug, Title: "sign " + lockedSecret, Kind: core.TaskKindGate,
		Gate: core.TaskGate{Who: "owner", What: "sign"}})
	expired := now.Add(-10 * time.Minute)
	ndTask(t, lc.db, core.Task{ProjectSlug: lockedSlug, Title: "close " + lockedSecret, Status: core.TaskInProgress,
		ClaimedBy: lc.sessID, LeaseExpiresAt: &expired})
	ndCapture(t, lc.mgr, lockedSlug, "ledger-capture", plans.StatusPresented)
	ndTask(t, lc.db, core.Task{ProjectSlug: openSlug, Title: "open gate", Kind: core.TaskKindGate,
		Gate: core.TaskGate{Who: "owner", What: "approve"}})
	ndMishap(t, lc.db, lockedSlug, "live-state: touched "+lockedSecret, now.Add(-2*time.Minute))
	ndMishap(t, lc.db, openSlug, "shell-trap: a bare == died", now.Add(-time.Minute))

	read := func(unlock string) map[string]sbBadge {
		status, body := lc.get(t, "/console/memories", unlock)
		require.Equal(t, http.StatusOK, status)
		return sbBadges(t, body)
	}
	locked := read("")
	require.Equal(t, 1, locked["/console/tasks"].Count, "the open project's gate alone")
	require.False(t, locked["/console/plans"].Drawn, "the only presented plan is the locked project's")
	require.Equal(t, 0, locked["/console/now"].Count, "the only live agent is the locked project's")
	require.Equal(t, 1, locked["/console/mishaps"].Count, "the open project's report alone")

	unlocked := read(lc.unlockByTerminal(t))
	require.Equal(t, 3, unlocked["/console/tasks"].Count, "unlocked: its gate and its lapsed claim count too")
	require.Equal(t, 1, unlocked["/console/plans"].Count)
	require.Equal(t, 1, unlocked["/console/now"].Count)
	require.Equal(t, 2, unlocked["/console/mishaps"].Count, "unlocked: its report counts too")
}

// A failed count never fails a page: the read that fails costs its badge,
// and every other badge is still drawn.
func TestSbBadges_AFailedCountCostsItsBadgeNotThePage(t *testing.T) {
	c := ndNew(t, "advanced", config.Features{})
	_, err := store.CreateProposal(context.Background(), c.db, store.ProposalArchive, map[string]any{"key": "sb-k1"})
	require.NoError(t, err)
	ndTask(t, c.db, core.Task{ProjectSlug: "example-ops", Title: "Sign the renewal", Kind: core.TaskKindGate,
		Gate: core.TaskGate{Who: "owner", What: "sign"}})
	before := sbBadges(t, sbPage(t, c.mux, "/console/notes"))
	require.Equal(t, 1, before["/console/gardener"].Count)
	require.Equal(t, 1, before["/console/tasks"].Count)

	_, err = c.db.Exec(`ALTER TABLE gardener_proposals RENAME TO sb_proposals_gone`)
	require.NoError(t, err)
	rr := getPeek(t, c.mux, "/console/notes")
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	after := sbBadges(t, rr.Body.String())
	require.False(t, after["/console/gardener"].Drawn, "the failed count draws no badge")
	require.Equal(t, 1, after["/console/tasks"].Count, "the other badges are read one by one")
}

// The frame: only the sections scroll, Settings is pinned below them beside
// the account row, the collapsed rail marks a queue by its kind (never by a
// hard-coded href), the phone bar's menu button follows the nav's mark, and
// the g hint is a grid that stays until the next key.
func TestSbSidebar_FitsTheScreen(t *testing.T) {
	c := ndNew(t, "advanced", featuresAll(true))
	ndHeld(t, c.db, "example-ops")
	page := sbPage(t, c.mux, "/console/notes")
	nav := sbNav(t, page)

	scroll := strings.Index(nav, `<div class="nav-scroll">`)
	pinned := strings.Index(nav, `<div class="nav-pinned">`)
	settings := strings.Index(nav, `href="/console/settings"`)
	jumps := strings.Index(nav, `<div class="nav-jumps"`)
	require.True(t, scroll >= 0 && pinned > scroll && settings > pinned && jumps > settings,
		"Settings is pinned after the scrolling sections")
	require.Equal(t, 1, strings.Count(nav[pinned:jumps], `<a href="/console/`), "the pinned tail is Settings alone")
	require.Contains(t, nav[scroll:pinned], `href="/console/held"`)
	foot := strings.Index(page, `<div class="side-foot">`)
	require.Greater(t, foot, strings.Index(page, `</nav>`), "the account row follows the nav")
	require.Equal(t, sbBadge{Drawn: true, Kind: "queue", Count: 1}, sbBadges(t, page)["/console/held"])
	require.Contains(t, nav, "data-waiting", "a held write marks the nav: the phone bar's menu button shows a dot")

	css := string(consoleCSS)
	require.NotContains(t, css, `a[href="/console/now"] .count`, "the rail's dot follows the badge kind, not an href")
	require.NotContains(t, css, `a[href="/console/gardener"] .count`)
	require.Regexp(t, `(?s)@media \(min-width: 721px\) \{\s*:root\[data-sidebar="collapsed"\] \.nav a \.count\[data-badge="queue"\] \{[^}]*border-radius: 50%`, css,
		"collapsed, a queue draws a dot on its icon")
	require.Contains(t, css, `.shell:has(nav.nav[data-waiting]) #drawer-open::after`)
	require.Regexp(t, `(?s)\.sidebar \{[^}]*height: 100vh;\s*overflow: hidden;`, css, "the sidebar itself never scrolls")
	require.Regexp(t, `(?s)\.nav-scroll \{[^}]*overflow-y: auto;`, css, "its sections list does")
	require.Contains(t, css, `.sidebar[data-nav-below] .nav-scroll`, "an edge with more beyond it fades")
	require.NotContains(t, css, ".chord-hint span { white-space: nowrap", "the g hint no longer cuts itself off")
	require.Regexp(t, `(?s)\.chord-keys \{[^}]*display: grid;`, css)

	js := string(shellJS)
	require.NotContains(t, js, "setTimeout(endChord", "the g hint stays until the next key or Esc")
	require.Contains(t, js, "list.className = 'chord-keys';")
	require.Contains(t, js, "side.toggleAttribute('data-nav-above'")
	require.Contains(t, js, "side.toggleAttribute('data-nav-below'")
}

// The live refresh patches badges that come and go: a queue's badge appears
// when something starts waiting and goes when nothing does, with the link set
// unchanged, and the nav's own mark follows. The sidebar's nav is inside
// what a patch answer carries.
func TestSbNavigationJS_PatchesBadgesThatComeAndGo(t *testing.T) {
	js := string(navigationJS)
	require.Contains(t, js, "syncAttrs(currentNav, freshNav);", "data-waiting follows the refresh")
	require.Contains(t, js, "syncCount(currentLinks[j], freshLinks[j]);")
	require.Contains(t, js, "if (current) current.remove();", "a queue that empties loses its badge")
	require.Contains(t, js, "bump(link.appendChild(imported(fresh)));", "a queue that fills gains one")
	require.NotContains(t, js, "freshCounts.length === currentCounts.length",
		"the index-by-index count patch could not add or remove a badge")

	c := ndNew(t, "advanced", config.Features{Screening: true})
	ndHeld(t, c.db, "example-ops")
	req := httptest.NewRequest(http.MethodGet, "/console/notes", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set(partialHeader, "1")
	rr := do(c.mux, req)
	require.Equal(t, http.StatusOK, rr.Code)
	require.Equal(t, 1, sbBadges(t, rr.Body.String())["/console/held"].Count, "the patch answer carries the badges")
	require.Contains(t, sbNav(t, rr.Body.String()), "data-waiting")
}

// A gate someone marked in progress waits on a person: it is no agent's work
// in flight, so Now lists it nowhere among its loose ends (it would read as a
// quiet holder or a claimless start), and the Tasks badge counts it once, as
// a gate.
func TestSbStartedGateIsNoLooseEnd(t *testing.T) {
	c := ndNew(t, "advanced", ndAllOn)
	ndTask(t, c.db, core.Task{ProjectSlug: "example-web", Title: "Walk the floor", Kind: core.TaskKindGate,
		Status: core.TaskInProgress, Gate: core.TaskGate{Who: "owner", What: "walk it"}})
	var now nowData
	getJSON(t, c.mux, "/console/now?format=json", &now)
	for _, l := range now.Loose {
		require.NotEqual(t, "Walk the floor", l.Title, "a started gate is no loose end (state %q)", l.State)
	}
	require.Zero(t, now.InFlight, "a gate is no work in flight")
	badges := sbBadges(t, sbPage(t, c.mux, "/console/"))
	require.Equal(t, 1, badges["/console/tasks"].Count, "counted once, as a gate on the owner")
}
