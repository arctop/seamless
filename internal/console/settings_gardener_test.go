package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/gardener"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// aaConsole is a console with a real gardener over a fresh store, built from
// base as the file/env gardener config.
type aaConsole struct {
	db  *sql.DB
	mux *http.ServeMux
	svc *Service
	rec *events.Recorder
}

func newAaConsole(t *testing.T, base config.Gardener) aaConsole {
	t.Helper()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	dataDir := filepath.Join(dir, "data")
	mgr, err := files.NewManager(dataDir, db, slog.Default())
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })
	rec := events.NewRecorder(db)
	base.IntervalMinutes = 60
	garden := gardener.New(db, mgr, nil, nil, rec, gardener.FromConfig(base), slog.Default())
	svc, err := New(Config{DB: db, Files: mgr, Gardener: garden, Events: rec, DataDir: dataDir, APIKey: testKey,
		Level: "advanced", GardenerCfg: base})
	require.NoError(t, err)
	mux := http.NewServeMux()
	svc.Register(mux)
	return aaConsole{db: db, mux: mux, svc: svc, rec: rec}
}

// aaPending seeds a pending tool_error proposal and returns its id.
func aaPending(t *testing.T, db *sql.DB) string {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	p, err := store.CreateProposal(context.Background(), db, store.ProposalToolError, map[string]any{
		"key": "tool_error:" + id, "project": "", "suggested_title": "disk full", "surface": "tool", "name": "memory_write",
	})
	require.NoError(t, err)
	return p.ID
}

// aaGardenerRoutes are the Gardener's decision routes the switch guards, in
// console.go's order, with the form each takes, for a proposal id.
func aaGardenerRoutes(id string) []struct{ path, body string } {
	return []struct{ path, body string }{
		{"/console/gardener/group/dismiss", "ids=" + id},
		{"/console/gardener/group/apply", "ids=" + id},
		{"/console/gardener/plan/split-example/apply", ""},
		{"/console/gardener/" + id + "/apply", ""},
		{"/console/gardener/" + id + "/dismiss", ""},
		{"/console/gardener/" + id + "/hide", ""},
		{"/console/gardener/" + id + "/unhide", ""},
		{"/console/gardener/" + id + "/retarget", "to=example-ops"},
		{"/console/gardener/" + id + "/swap", "keep=" + id},
		{"/console/gardener/" + id + "/undo", ""},
	}
}

// aaStatus reads a proposal's status and decider.
func aaStatus(t *testing.T, db *sql.DB, id string) (string, string) {
	t.Helper()
	p, ok, err := store.ProposalByID(context.Background(), db, id)
	require.NoError(t, err)
	require.True(t, ok)
	return p.Status, p.ResolvedBy
}

// With the switch off -- set on Settings, read per request, no restart --
// every Gardener decision refuses an agent's process and a paired device and
// changes nothing; the owner's own callers apply as before.
func TestAaGardenerRoutes_SwitchOffRefusesAgents(t *testing.T) {
	c := newAaConsole(t, config.Gardener{})
	id := aaPending(t, c.db)
	owner := ""
	for _, caller := range uohCallers(t) {
		if caller.owner {
			continue
		}
		rr := uohPost(t, c.mux, caller, "/console/gardener/"+id+"/dismiss", "")
		require.NotEqual(t, http.StatusForbidden, rr.Code, "on: %s decides as before: %s", caller.name, rr.Body.String())
		status, _ := aaStatus(t, c.db, id)
		require.Equal(t, store.ProposalDismissed, status, caller.name)
		require.NoError(t, store.ReopenProposal(context.Background(), c.db, id))
	}

	// The owner turns agents' applying off on Settings.
	browser := uohCallers(t)[len(uohCallers(t))-1]
	require.True(t, browser.owner)
	rr := uohPost(t, c.mux, browser, "/console/settings/gardener", "agents=off")
	require.Equal(t, http.StatusSeeOther, rr.Code, rr.Body.String())
	require.Contains(t, rr.Header().Get("Location"), "notice=")

	for _, caller := range uohCallers(t) {
		if caller.owner {
			owner = caller.name
			continue
		}
		for _, rt := range aaGardenerRoutes(id) {
			rr := uohPost(t, c.mux, caller, rt.path, rt.body)
			uohRequireRefused(t, rr, caller, caller.name+" "+rt.path)
		}
		status, by := aaStatus(t, c.db, id)
		require.Equal(t, store.ProposalPending, status, "nothing changed for %s", caller.name)
		require.Empty(t, by)
	}
	require.NotEmpty(t, owner)

	rr = uohPost(t, c.mux, browser, "/console/gardener/"+id+"/apply", "")
	require.Equal(t, http.StatusSeeOther, rr.Code, rr.Body.String())
	status, by := aaStatus(t, c.db, id)
	require.Equal(t, store.ProposalApplied, status, "the owner's browser applies")
	require.Equal(t, gardener.ByOwner, by)
}

// agents_may_apply: false in the file bars agents with no console row at all.
func TestAaGardenerRoutes_FileOffRefusesAgents(t *testing.T) {
	c := newAaConsole(t, config.Gardener{AgentsMayApply: new(false)})
	id := aaPending(t, c.db)
	agent := uohCallers(t)[0]
	uohRequireRefused(t, uohPost(t, c.mux, agent, "/console/gardener/"+id+"/apply", ""), agent, "apply")
	rr := postForm(c.mux, "/console/gardener/"+id+"/dismiss", "")
	require.Equal(t, http.StatusSeeOther, rr.Code, "the bearer key is the owner's: %s", rr.Body.String())
}

// Both Settings routes are the owner's in every direction: an agent's process
// and a paired device are refused with nothing stored; the owner's callers
// save and reset.
func TestAaSettingsGardenerRoutes_AreTheOwners(t *testing.T) {
	for _, caller := range uohCallers(t) {
		t.Run(caller.name, func(t *testing.T) {
			c := newAaConsole(t, config.Gardener{})
			for _, body := range []string{"agents=on", "agents=off", "auto_set=1&auto=memory_wanted"} {
				rr := uohPost(t, c.mux, caller, "/console/settings/gardener", body)
				if !caller.owner {
					uohRequireRefused(t, rr, caller, body)
					_, found, err := store.GardenerOverride(context.Background(), c.db)
					require.NoError(t, err)
					require.False(t, found, "nothing is stored")
					continue
				}
				require.Equal(t, http.StatusSeeOther, rr.Code, rr.Body.String())
			}
			rr := uohPost(t, c.mux, caller, "/console/settings/gardener/reset", "")
			if !caller.owner {
				uohRequireRefused(t, rr, caller, "reset")
				return
			}
			require.Equal(t, http.StatusSeeOther, rr.Code, rr.Body.String())
			_, found, err := store.GardenerOverride(context.Background(), c.db)
			require.NoError(t, err)
			require.False(t, found, "reset clears the row")
		})
	}
}

// aaFlash posts body to the save route with the bearer key and returns the
// redirect's flash: its kind (notice or error) and text. Every answer lands on
// the Gardener group of the Knowledge engine section.
func aaFlash(t *testing.T, c aaConsole, path, body string) (string, string) {
	t.Helper()
	rr := postForm(c.mux, path, body)
	require.Equal(t, http.StatusSeeOther, rr.Code, rr.Body.String())
	u, err := url.Parse(rr.Header().Get("Location"))
	require.NoError(t, err)
	require.Equal(t, "/console/settings", u.Path)
	require.Equal(t, "engine", u.Query().Get("s"))
	require.Equal(t, gardenerSettingsAnchor, u.Fragment)
	if m := u.Query().Get("error"); m != "" {
		return "error", m
	}
	return "notice", u.Query().Get("notice")
}

func TestAaSettingsGardenerSave(t *testing.T) {
	c := newAaConsole(t, config.Gardener{AutoApply: []string{"tool_error"}})
	ctx := context.Background()
	stored := func() config.GardenerOverride {
		o, _, err := store.GardenerOverride(ctx, c.db)
		require.NoError(t, err)
		return o
	}

	// Refusals store nothing and name the valid values.
	for body, want := range map[string]string{
		"":                      "nothing to save",
		"agents=maybe":          `agents invalid "maybe": valid values are on, off`,
		"agents=on&agents=off":  "agents must be given exactly once",
		"auto=memory_wanted":    "auto kinds come with auto_set=1",
		"auto_set=2":            "auto_set must be given exactly once, as 1",
		"auto_set=1&auto=merge": `auto: "merge" cannot be applied automatically`,
		"auto_set=1&auto=ship_plan&auto=ship_plan": `auto "ship_plan" must be given at most once`,
	} {
		kind, msg := aaFlash(t, c, "/console/settings/gardener", body)
		require.Equal(t, "error", kind, body)
		require.Contains(t, msg, want, body)
		_, found, err := store.GardenerOverride(ctx, c.db)
		require.NoError(t, err)
		require.False(t, found, "nothing stored for %q", body)
	}

	// Each control stores only its own field.
	kind, msg := aaFlash(t, c, "/console/settings/gardener", "agents=off")
	require.Equal(t, "notice", kind)
	require.Contains(t, msg, "Agents may no longer apply, dismiss or hide proposals")
	require.Equal(t, config.GardenerOverride{AgentsMayApply: new(false)}, stored())
	sw := c.svc.gardenerSwitches(ctx)
	require.False(t, sw.AgentsMayApply)
	require.Equal(t, gardener.SourceConsole, sw.AgentsSource)
	require.Equal(t, []string{"tool_error"}, sw.AutoApply, "the other switch keeps the file's value")
	require.Equal(t, gardener.SourceConfig, sw.AutoSource)

	kind, msg = aaFlash(t, c, "/console/settings/gardener", "auto_set=1&auto=ship_plan&auto=memory_wanted")
	require.Equal(t, "notice", kind)
	require.Contains(t, msg, "New memory wanted, ship plan proposals are applied by themselves")
	require.Equal(t, &[]string{"memory_wanted", "ship_plan"}, stored().AutoApply, "in store.ProposalKinds order")
	require.Equal(t, new(false), stored().AgentsMayApply, "kept")

	kind, msg = aaFlash(t, c, "/console/settings/gardener", "auto_set=1")
	require.Equal(t, "notice", kind)
	require.Contains(t, msg, "Automatic apply is off")
	require.Equal(t, &[]string{}, stored().AutoApply, "none checked is a choice of none")
	require.Empty(t, c.svc.gardenerSwitches(ctx).AutoApply, "the console's none wins over the file's tool_error")

	// Every save and the reset record a gardener.action event by console.
	kind, msg = aaFlash(t, c, "/console/settings/gardener/reset", "")
	require.Equal(t, "notice", kind)
	require.Contains(t, msg, "follow the config file again: agents may apply proposals. New tool error proposals")
	sw = c.svc.gardenerSwitches(ctx)
	require.True(t, sw.AgentsMayApply)
	require.Equal(t, []string{"tool_error"}, sw.AutoApply, "reset returns every control to the file's value")
	evs, err := c.rec.ByKinds(ctx, []core.EventKind{core.EventGardenerAction}, "", "", 20)
	require.NoError(t, err)
	var actions []string
	for _, e := range evs {
		require.Equal(t, "console", e.Payload["by"])
		actions = append(actions, e.Payload["action"].(string))
	}
	require.Equal(t, []string{"switches_reset", "switches", "switches", "switches"}, actions)
}

// agents_may_apply: false in the file locks the switch: the route refuses to
// turn it on with nothing stored, the page shows the switch locked, and Reset
// leaves it off.
func TestAaSettingsGardener_FileFalseLocksTheSwitch(t *testing.T) {
	c := newAaConsole(t, config.Gardener{AgentsMayApply: new(false)})
	ctx := context.Background()
	kind, msg := aaFlash(t, c, "/console/settings/gardener", "agents=on")
	require.Equal(t, "error", kind)
	require.Contains(t, msg, "Agents stay barred from applying proposals: gardener.agents_may_apply: false is set in the config file")
	_, found, err := store.GardenerOverride(ctx, c.db)
	require.NoError(t, err)
	require.False(t, found)

	// A row that says on (written before the file changed) changes nothing.
	require.NoError(t, store.SetGardenerOverride(ctx, c.db, config.GardenerOverride{AgentsMayApply: new(true)}))
	sw := c.svc.gardenerSwitches(ctx)
	require.False(t, sw.AgentsMayApply)
	require.True(t, sw.AgentsLocked)

	rr := do(c.mux, authedReq(httptest.NewRequest(http.MethodGet, "/console/settings?s=engine", nil)))
	require.Equal(t, http.StatusOK, rr.Code)
	page := rr.Body.String()
	require.Contains(t, page, `id="settings-gardener"`)
	require.Regexp(t, `<button class="btn small primary" type="submit" disabled title="Set in the config file or environment">Let agents apply proposals</button>`, page)
	require.Contains(t, page, "gardener.agents_may_apply: false is final there")

	kind, _ = aaFlash(t, c, "/console/settings/gardener/reset", "")
	require.Equal(t, "notice", kind)
	require.False(t, c.svc.gardenerSwitches(ctx).AgentsMayApply)
}

// The Gardener group renders both controls with where each came from, one
// checkbox per AutoSafe kind, and Your setup states both in one line.
func TestAaSettingsGardener_Renders(t *testing.T) {
	c := newAaConsole(t, config.Gardener{})
	get := func(section string) string {
		rr := do(c.mux, authedReq(httptest.NewRequest(http.MethodGet, "/console/settings?s="+section, nil)))
		require.Equal(t, http.StatusOK, rr.Code)
		return rr.Body.String()
	}
	page := get("engine")
	require.Contains(t, page, `id="settings-gardener"`)
	require.Contains(t, page, "Turn agents&rsquo; applying off")
	require.Contains(t, page, "<strong>Default</strong>")
	require.NotContains(t, page, `form="gardener-reset"`, "nothing to reset")
	for _, k := range gardener.AutoSafeKinds() {
		require.Contains(t, page, `<input type="checkbox" name="auto" value="`+k+`">`, k)
	}
	require.Equal(t, len(gardener.AutoSafeKinds()), strings.Count(page, `name="auto" value=`))
	require.Contains(t, get("setup"), "agents may apply proposals; nothing applies by itself")

	ctx := context.Background()
	require.NoError(t, store.SetGardenerOverride(ctx, c.db, config.GardenerOverride{AgentsMayApply: new(false),
		AutoApply: &[]string{"memory_wanted"}}))
	page = get("engine")
	require.Contains(t, page, `<input type="checkbox" name="auto" value="memory_wanted" checked>`)
	require.Contains(t, page, "Let agents apply proposals")
	require.Contains(t, page, `form="gardener-reset"`)
	require.Contains(t, page, `action="/console/settings/gardener/reset"`)
	require.Contains(t, get("setup"), "only you apply proposals; memory wanted apply by themselves")
}

// The frozen contract the Gardener screen reads, and the guard: a failed read
// bars agents (fail closed) while the screen shows the file's values.
func TestAaGardenerSwitches_ContractAndFailClosed(t *testing.T) {
	c := newAaConsole(t, config.Gardener{AutoApply: []string{"ship_plan", "abandon_plan"}})
	ctx := context.Background()
	sw := c.svc.gardenerSwitches(ctx)
	require.True(t, sw.AgentsMayApply)
	require.Equal(t, []string{"abandon_plan", "ship_plan"}, sw.AutoApply, "store.ProposalKinds order")
	req := httptest.NewRequest(http.MethodPost, "/console/gardener/x/apply", nil)
	require.False(t, c.svc.gardenerAgentsBarred(req))

	require.NoError(t, c.db.Close())
	require.True(t, c.svc.gardenerAgentsBarred(req), "switches that cannot be read bar agents")
	sw = c.svc.gardenerSwitches(ctx)
	require.True(t, sw.ReadFailed)
	require.Equal(t, []string{"abandon_plan", "ship_plan"}, sw.AutoApply)
}

// aaAutoApplied seeds a proposal about project that automatic apply applied at.
func aaAutoApplied(t *testing.T, db *sql.DB, project string, at time.Time) string {
	t.Helper()
	ctx := context.Background()
	id, err := core.NewID()
	require.NoError(t, err)
	p, err := store.CreateProposal(ctx, db, store.ProposalMemoryWanted, map[string]any{
		"key": "memory_wanted:" + project + ":" + id, "project": project, "suggested_title": "zeta handshake",
	})
	require.NoError(t, err)
	require.NoError(t, store.ResolveProposal(ctx, db, p.ID, store.ProposalApplied, gardener.ByAuto, at))
	return p.ID
}

// aaAttention reads the Overview's attention cards as JSON.
func aaAttention(t *testing.T, mux http.Handler, req *http.Request) []attnCard {
	t.Helper()
	req.Header.Set("Accept", "application/json")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var data struct{ Attention []attnCard }
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &data))
	return data.Attention
}

// aaAutoCard is the automatic-apply card among cards, nil when absent.
func aaAutoCard(cards []attnCard) *attnCard {
	for i := range cards {
		if cards[i].Href == "/console/gardener/decisions#gardener-auto" {
			return &cards[i]
		}
	}
	return nil
}

// The Overview counts what automatic apply applied since the owner's last
// decision on the Gardener; nothing applied, no card. Looking writes nothing.
func TestAaOverview_AutoAppliedCard(t *testing.T) {
	c := newAaConsole(t, config.Gardener{})
	overview := func() *attnCard {
		return aaAutoCard(aaAttention(t, c.mux, authedReq(httptest.NewRequest(http.MethodGet, "/console/", nil))))
	}
	require.Nil(t, overview(), "automatic apply off: no card")

	now := time.Now().UTC()
	aaAutoApplied(t, c.db, "", now.Add(-2*time.Hour))
	undone := aaAutoApplied(t, c.db, "", now.Add(-time.Hour))
	card := overview()
	require.NotNil(t, card)
	require.Equal(t, "2 proposals applied automatically", card.Title)
	require.Equal(t, "info", card.Sev)
	require.Equal(t, "review or undo them in the decision history", card.Sub)
	require.NotNil(t, overview(), "looking changes nothing")

	// A switches change is made on Settings, and Run a pass now is recorded
	// after the pass it ran: neither is looking at what was applied.
	require.Equal(t, http.StatusSeeOther, postForm(c.mux, "/console/settings/gardener", "agents=on").Code)
	for _, action := range []string{gardener.ActionRunNow, gardener.ActionRequest, gardener.ActionSplit} {
		_, err := c.rec.Record(context.Background(), core.Event{Kind: core.EventGardenerAction,
			Payload: map[string]any{"action": action, "by": "console"}})
		require.NoError(t, err)
	}
	require.NotNil(t, overview())

	// An undone apply leaves the count; the owner's decision on the Gardener
	// is when they last looked.
	require.NoError(t, store.ReopenProposal(context.Background(), c.db, undone))
	require.Equal(t, "1 proposal applied automatically", overview().Title)
	require.Equal(t, http.StatusSeeOther, postForm(c.mux, "/console/gardener/"+undone+"/dismiss", "").Code)
	require.Nil(t, overview(), "nothing applied since the owner's last decision")

	aaAutoApplied(t, c.db, "", time.Now().UTC().Add(time.Minute))
	card = overview()
	require.NotNil(t, card)
	require.Equal(t, "1 proposal applied automatically", card.Title)
	require.Equal(t, "since your last Gardener decision · review or undo them", card.Sub)
}

// The card reads through the request's handle: a locked project's automatic
// applies are not counted for a browser that holds nothing open on it.
func TestAaOverview_AutoAppliedCardLeavesOutALockedProject(t *testing.T) {
	lc := newLockedConsole(t)
	now := time.Now().UTC()
	aaAutoApplied(t, lc.db, lockedSlug, now.Add(-time.Hour))
	aaAutoApplied(t, lc.db, openSlug, now.Add(-time.Hour))
	card := aaAutoCard(aaAttention(t, lc.mux, lc.browser(http.MethodGet, "/console/", "", nil)))
	require.NotNil(t, card)
	require.Equal(t, "1 proposal applied automatically", card.Title, "the locked project's apply is not counted")
}
