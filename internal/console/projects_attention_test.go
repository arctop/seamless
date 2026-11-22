package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// bdTask creates a task in project (plan "" for an off-plan task) and returns
// its id. kind "" is a step; who/what set a gate.
func bdTask(t *testing.T, db *sql.DB, project, plan, title string, kind core.TaskKind, who, what string, deps ...string) string {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	now := time.Now().UTC()
	if kind == "" {
		kind = core.TaskKindStep
	}
	require.NoError(t, store.CreateTask(context.Background(), db, core.Task{
		ID: id, ProjectSlug: project, PlanSlug: plan, Title: title, Status: core.TaskOpen,
		Kind: kind, Gate: core.TaskGate{Who: who, What: what}, DependsOn: deps,
		CreatedBy: "test", CreatedAt: now, UpdatedAt: now,
	}))
	return id
}

// bdClaim puts a task in progress held by holder until expires (holder ""
// and a zero expires: a claimless start).
func bdClaim(t *testing.T, db *sql.DB, id, holder string, expires time.Time) {
	t.Helper()
	var lease any
	if !expires.IsZero() {
		lease = core.FormatTime(expires.UTC())
	}
	_, err := db.ExecContext(context.Background(),
		`UPDATE tasks SET status = 'in_progress', claimed_by = ?, lease_expires_at = ? WHERE id = ?`, holder, lease, id)
	require.NoError(t, err)
}

// bdAge moves every task of a plan's last change back by age.
func bdAge(t *testing.T, db *sql.DB, project, plan string, age time.Duration) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `UPDATE tasks SET updated_at = ? WHERE project_slug = ? AND plan_slug = ?`,
		core.FormatTime(time.Now().UTC().Add(-age)), project, plan)
	require.NoError(t, err)
}

// bdCapture indexes a captured plan note with the given plan-status.
func bdCapture(t *testing.T, db *sql.DB, project, plan, status string) {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	stamp := core.FormatTime(time.Now().UTC())
	tags, err := json.Marshal([]string{"cc-plan", "plan:" + plan, "plan-status:" + status})
	require.NoError(t, err)
	_, err = db.ExecContext(context.Background(), `
		INSERT INTO notes_index (id, title, slug, description, project, file_path, tags, content_hash, created_at, updated_at)
		VALUES (?, ?, ?, '', ?, ?, ?, 'h', ?, ?)`,
		id, "Plan "+plan, "cc-plan-"+plan, project, "notes/"+project+"/cc-plan-"+plan+".md", string(tags), stamp, stamp)
	require.NoError(t, err)
}

// bdProject registers a project with a session touched now (Active).
func bdProject(t *testing.T, db *sql.DB, slug string) {
	t.Helper()
	ctx := context.Background()
	_, err := store.EnsureProject(ctx, db, slug, slug)
	require.NoError(t, err)
	id, err := core.NewID()
	require.NoError(t, err)
	now := time.Now().UTC()
	require.NoError(t, store.CreateSession(ctx, db, core.Session{
		ID: id, Name: "cc/" + slug, ProjectSlug: slug, Status: core.SessionCompleted, CreatedAt: now, UpdatedAt: now,
	}))
}

// bdRows indexes every project row of a board model by slug.
func bdRows(data projectsData) map[string]projectRowVM {
	out := map[string]projectRowVM{}
	for _, g := range data.Groups {
		for _, row := range g.Rows {
			out[row.Slug] = row
		}
	}
	for _, row := range data.Flat {
		out[row.Slug] = row
	}
	return out
}

// bdGet is an authenticated GET answering its status and body.
func bdGet(mux http.Handler, target string, asJSON bool) (int, string) {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	if asJSON {
		req.Header.Set("Accept", "application/json")
	}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr.Code, rr.Body.String()
}

// Attention means something waits on the owner or is stuck. A project whose
// only unready work waits on other work -- held by a live claim, or simply
// open -- is plan sequencing and is not flagged; an open gate, a lapsed
// claim, a claimless start, a plan awaiting approval and unregistered data
// each flag their project, and a retired project is never flagged.
func TestBdAttentionMeansWaitingOnYouOrStuck(t *testing.T) {
	db, mux := newConsole(t)
	now := time.Now().UTC()
	for _, slug := range []string{"p-sequenced", "p-gate", "p-own-gate", "p-lapsed", "p-claimless", "p-approval", "p-shipped", "p-retired"} {
		bdProject(t, db, slug)
	}

	// Two steps wait on one held by a live claim: ordinary sequencing.
	held := bdTask(t, db, "p-sequenced", "seq", "held", "", "", "")
	bdClaim(t, db, held, "sess-live", now.Add(time.Hour))
	bdTask(t, db, "p-sequenced", "seq", "next", "", "", "", held)
	bdTask(t, db, "p-sequenced", "seq", "after", "", "", "", held)

	gate := bdTask(t, db, "p-gate", "", "Owner signs off", core.TaskKindGate, "owner", "sign the renewal")
	bdTask(t, db, "p-gate", "", "order the disks", "", "", "", gate)
	bdTask(t, db, "p-own-gate", "", "turn on the profile", "", "hardware", "the board arrives")
	bdClaim(t, db, bdTask(t, db, "p-lapsed", "", "move the exporter", "", "", ""), "sess-gone", now.Add(-25*time.Minute))
	bdClaim(t, db, bdTask(t, db, "p-claimless", "", "tidy the routes", "", "", ""), "", time.Time{})
	bdCapture(t, db, "p-approval", "rate-limits", "presented")
	bdCapture(t, db, "p-shipped", "old-plan", "approved")
	bdTask(t, db, "p-retired", "", "Old sign-off", core.TaskKindGate, "owner", "sign")
	require.NoError(t, store.RetireProject(context.Background(), db, "p-retired", now))
	// Data under a slug the registry does not list.
	bdTask(t, db, "p-ghost", "", "orphan", "", "", "")

	var data projectsData
	getJSON(t, mux, "/console/projects?format=json&group=flat", &data)
	rows := bdRows(data)
	flagged := map[string]bool{}
	for slug, row := range rows {
		if row.Attention {
			flagged[slug] = true
		}
	}
	require.Equal(t, map[string]bool{"p-gate": true, "p-own-gate": true, "p-lapsed": true, "p-claimless": true,
		"p-approval": true, "p-ghost": true}, flagged)
	require.Equal(t, 6, data.Attention)

	seq := rows["p-sequenced"]
	require.Equal(t, 1, seq.InProgress)
	require.Equal(t, 2, seq.Blocked, "waiting on in-progress work is blocked, not attention")
	require.Equal(t, 0, seq.Waiting)
	require.Equal(t, 1, rows["p-gate"].GatesOpen)
	require.Equal(t, 2, rows["p-gate"].Waiting, "the gate and the step behind it wait on people")
	require.Equal(t, 0, rows["p-gate"].Blocked)
	require.Equal(t, 1, rows["p-lapsed"].Lapsed)
	require.Equal(t, 0, rows["p-lapsed"].InProgress, "a lapsed claim is not in progress")
	require.Equal(t, 1, rows["p-claimless"].Unclaimed)
	require.Equal(t, 1, rows["p-approval"].PlansAwaiting)
	require.Equal(t, 0, rows["p-shipped"].PlansAwaiting, "an approved capture waits on nobody")
	require.True(t, rows["p-ghost"].Unregistered)
	require.Equal(t, 1, rows["p-retired"].GatesOpen)
	require.False(t, rows["p-retired"].Attention, "a retired project is never flagged")

	body := getHTMLBody(t, mux, "/console/projects?group=flat")
	for _, want := range []string{
		`data-need="claim-lapsed" title="In progress, but its holder&#39;s lease ran out: any agent can claim it">1 lapsed claim</span>`,
		`data-need="unclaimed" title="Marked in progress without a claim: any agent can claim it">1 unclaimed task</span>`,
		`data-need="gate" title="Waits on a person, or an event outside Seamless">1 open gate</span>`,
		`data-need="approval" title="A captured plan, presented and awaiting your approval">1 plan awaiting approval</span>`,
		`<span class="project-state danger" data-need="claim-lapsed"`,
		`<article id="p-p-gate" class="project-row row-link attention" data-href="/console/projects/p-gate">`,
		`<article id="p-p-sequenced" class="project-row row-link" data-href="/console/projects/p-sequenced">`,
		`>6 projects need attention</span>`,
	} {
		require.Contains(t, body, want)
	}
	seqRow := body[strings.Index(body, `id="p-p-sequenced"`):]
	seqRow = seqRow[:strings.Index(seqRow, "</article>")]
	require.NotContains(t, seqRow, "data-need=", "a sequenced project carries no attention badge")
	require.Contains(t, seqRow, `<span data-word="in-progress" title="Held by a live claim">1 in progress</span>`)
	require.Contains(t, seqRow, `>2 blocked</span>`)
}

// The attention chip narrows the board, through ?show=attention: only the
// flagged projects stay, the fold that holds one opens, the chip turns into
// the way back, and every view, sort and window link keeps the narrowing. The
// parameter is a strict enum like the others.
func TestBdAttentionChipNarrowsTheBoard(t *testing.T) {
	db, mux := newConsole(t)
	bdProject(t, db, "p-calm")
	bdProject(t, db, "p-busy")
	bdTask(t, db, "p-busy", "", "Owner signs", core.TaskKindGate, "owner", "sign")
	// A quiet project that waits on the owner.
	_, err := store.EnsureProject(context.Background(), db, "p-sleepy", "p-sleepy")
	require.NoError(t, err)
	bdCapture(t, db, "p-sleepy", "dormant", "presented")

	body := getHTMLBody(t, mux, "/console/projects")
	require.Contains(t, body, `<a class="lib-active-filter project-attn-chip" href="/console/projects?group=activity&amp;sort=recent&amp;q=&amp;w=24h&amp;show=attention" data-seam-query`)
	require.Contains(t, body, `href="/console/projects?group=flat&amp;sort=recent&amp;q=&amp;w=24h">Flat</a>`, "an unnarrowed board's links name no show")
	require.NotContains(t, body, `name="show"`)

	var data projectsData
	getJSON(t, mux, "/console/projects?format=json&show=attention", &data)
	require.Equal(t, "attention", data.Show)
	require.Equal(t, 2, data.Projects)
	require.Equal(t, 2, data.Attention)
	require.Len(t, data.Groups, 2)
	require.Equal(t, []string{"p-busy"}, pbSlugs(data.Groups[0].Rows))
	require.Equal(t, "quiet", data.Groups[1].Key)
	require.True(t, data.Groups[1].Open, "the narrowing opens the fold that holds a flagged project")
	require.Equal(t, []string{"p-sleepy"}, pbSlugs(data.Groups[1].Rows))
	require.Equal(t, 1, data.OpenTasks, "the tiles count what the board shows")

	body = getHTMLBody(t, mux, "/console/projects?show=attention&group=family&sort=name")
	require.Contains(t, body, `<span class="lib-active-filter project-attn-chip on">`)
	require.Contains(t, body, `<a href="/console/projects?group=family&amp;sort=name&amp;q=&amp;w=24h" data-seam-query>Show all</a>`)
	require.Contains(t, body, `href="/console/projects?group=flat&amp;sort=name&amp;q=&amp;w=24h&amp;show=attention">Flat</a>`)
	require.Contains(t, body, `href="/console/projects?group=family&amp;sort=recent&amp;q=&amp;w=24h&amp;show=attention">Recent</a>`)
	require.Contains(t, body, `href="/console/projects?group=family&amp;sort=name&amp;q=&amp;w=7d&amp;show=attention">7d</a>`)
	require.Contains(t, body, `<input type="hidden" name="show" value="attention">`)
	require.Contains(t, body, "Only the projects that need attention")
	require.NotContains(t, body, `id="p-p-calm"`)

	// Nothing to narrow to: the board says so and offers the way back.
	getJSON(t, mux, "/console/projects?format=json&show=attention&q=calm", &data)
	require.Zero(t, data.Projects)
	body = getHTMLBody(t, mux, "/console/projects?show=attention&q=calm")
	require.Contains(t, body, "No project matching &ldquo;calm&rdquo; needs attention.")

	for _, bad := range []string{"bogus", "1", "Attention", "attention,all"} {
		code, msg := bdGet(mux, "/console/projects?show="+bad, true)
		require.Equal(t, http.StatusBadRequest, code, bad)
		require.Contains(t, msg, "all, attention", "show=%s names the valid values", bad)
	}
}

// One definition of blocked and of waiting on people: for one fixture, each
// project's blocked, waiting and ready counts on the board are the lengths of
// the Tasks page's rails for that project.
func TestBdBoardAndTasksPageCountAlike(t *testing.T) {
	db, mux := newConsole(t)
	now := time.Now().UTC()
	bdProject(t, db, "p-one")
	bdProject(t, db, "p-two")
	free := bdTask(t, db, "p-one", "", "free", "", "", "")
	bdTask(t, db, "p-one", "", "after free", "", "", "", free)
	gate := bdTask(t, db, "p-one", "plan-a", "GATE", core.TaskKindGate, "owner", "approve")
	bdTask(t, db, "p-one", "plan-a", "behind the gate", "", "", "", gate)
	bdTask(t, db, "p-one", "plan-a", "behind both", "", "", "", gate, free)
	bdTask(t, db, "p-one", "", "own gate", "", "hardware", "the board arrives")
	held := bdTask(t, db, "p-two", "plan-b", "held", "", "", "")
	bdClaim(t, db, held, "sess-live", now.Add(time.Hour))
	bdTask(t, db, "p-two", "plan-b", "after held", "", "", "", held)
	startedGate := bdTask(t, db, "p-two", "", "GATE started", core.TaskKindGate, "vendor", "ships")
	bdClaim(t, db, startedGate, "", time.Time{})
	bdTask(t, db, "p-two", "", "ready", "", "", "")

	var board projectsData
	getJSON(t, mux, "/console/projects?format=json&group=flat", &board)
	// The board counts plan steps too: the Tasks page with its steps shown.
	code, page := bdGet(mux, "/console/tasks?format=json&steps=1", true)
	require.Equal(t, http.StatusOK, code, page)
	rails := bdTasksRails(t, page)
	rows := bdRows(board)
	for _, slug := range []string{"p-one", "p-two"} {
		require.Equal(t, rails["blocked"][slug], rows[slug].Blocked, "%s blocked", slug)
		require.Equal(t, rails["gates"][slug]+rails["gated"][slug], rows[slug].Waiting, "%s waiting on people", slug)
		require.Equal(t, rails["ready"][slug], rows[slug].Ready, "%s ready", slug)
	}
	require.Equal(t, 1, rows["p-one"].Blocked)
	require.Equal(t, 4, rows["p-one"].Waiting)
	require.Equal(t, 1, rows["p-two"].Waiting, "a gate someone started still waits on people")
	require.Equal(t, 1, rows["p-two"].Blocked)
	require.Equal(t, rails["blocked"][""]+rows["p-one"].Blocked+rows["p-two"].Blocked, board.Blocked)
}

// bdTasksRails reads the Tasks page's JSON model: each rail's length per
// project.
func bdTasksRails(t *testing.T, body string) map[string]map[string]int {
	t.Helper()
	var page map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	out := map[string]map[string]int{}
	for _, rail := range []string{"ready", "blocked", "gates", "gated"} {
		out[rail] = map[string]int{}
		raw, ok := page[rail]
		if !ok || string(raw) == "null" {
			continue
		}
		var rows []struct {
			Project string `json:"project"`
		}
		require.NoError(t, json.Unmarshal(raw, &rows), rail)
		for _, r := range rows {
			out[rail][r.Project]++
		}
	}
	return out
}

// Rows lead with work: the work line states the plans (moving within seven
// days, or stalled) and the tasks in the status words, and the summary tiles
// run agents, tasks, plans, then knowledge.
func TestBdRowsLeadWithWork(t *testing.T) {
	db, mux := newConsole(t)
	now := time.Now().UTC()
	bdProject(t, db, "p-work")
	bdProject(t, db, "p-idle")
	step := bdTask(t, db, "p-work", "moving", "step one", "", "", "")
	bdClaim(t, db, step, "sess-live", now.Add(time.Hour))
	bdTask(t, db, "p-work", "moving", "step two", "", "", "", step)
	bdTask(t, db, "p-work", "stalled", "old step", "", "", "")
	bdAge(t, db, "p-work", "stalled", 8*24*time.Hour)
	done := bdTask(t, db, "p-work", "shipped", "only step", "", "", "")
	_, err := db.ExecContext(context.Background(), `UPDATE tasks SET status = 'done' WHERE id = ?`, done)
	require.NoError(t, err)

	var data projectsData
	getJSON(t, mux, "/console/projects?format=json&group=flat", &data)
	row := bdRows(data)["p-work"]
	require.Equal(t, 2, row.PlansActive, "a plan with every step closed is not active")
	require.Equal(t, 1, row.PlansMoving)
	require.Equal(t, 1, data.PlansMoving)
	require.Equal(t, 2, data.PlansActive)

	body := getHTMLBody(t, mux, "/console/projects?group=flat")
	work := body[strings.Index(body, `id="p-p-work"`):]
	work = work[strings.Index(work, `<div class="project-row-work">`):]
	work = work[:strings.Index(work, "</div>")]
	var words []string
	for _, part := range strings.Split(work, `<span data-word="`)[1:] {
		words = append(words, part[:strings.Index(part, `"`)])
	}
	require.Equal(t, []string{"plans", "stalled", "in-progress", "ready", "blocked"}, words, "plans first, then the tasks")
	require.Contains(t, work, ">1 plan moving</span>")
	require.Contains(t, work, ">1 plan stalled</span>")
	idle := body[strings.Index(body, `id="p-p-idle"`):]
	require.Contains(t, idle[:strings.Index(idle, "</article>")], `<span class="project-row-idle">No open work</span>`)
	require.Contains(t, body, `<time datetime="`, "last activity names its moment")

	tiles := body[strings.Index(body, `class="project-summary-grid"`):]
	tiles = tiles[:strings.Index(tiles, "</section>")]
	var order []int
	for _, label := range []string{"<span>Agents live</span>", "<span>Open tasks</span>", "<span>Plans in motion</span>", "<span>Knowledge base</span>", "<span>Knowledge reach</span>"} {
		at := strings.Index(tiles, label)
		require.NotEqual(t, -1, at, label)
		order = append(order, at)
	}
	for i := 1; i < len(order); i++ {
		require.Less(t, order[i-1], order[i], "the tiles lead with work")
	}
	require.Contains(t, tiles, "<small>of 2 with open steps · 1 stalled</small>")
	require.Contains(t, tiles, "<small>1 in progress · 1 ready · 1 blocked</small>")
}

// A locked project keeps to its counts: no work line, no attention, and its
// plans, gates, claims and captures are absent from the read itself -- the
// attention view leaves its row out, and no total counts its work. A task of
// an open project that a locked project's task blocks is blocked on the board,
// as on the Tasks page.
func TestBdLockedProjectKeepsToCounts(t *testing.T) {
	lc := newLockedConsole(t)
	now := time.Now().UTC()
	// The locked project holds every reason to need attention.
	lgate := bdTask(t, lc.db, lockedSlug, "ledger-plan", "GATE ledger "+lockedSecret, core.TaskKindGate, "owner", "sign "+lockedSecret)
	bdTask(t, lc.db, lockedSlug, "ledger-plan", "behind "+lockedSecret, "", "", "", lgate)
	bdClaim(t, lc.db, bdTask(t, lc.db, lockedSlug, "", "lapsed "+lockedSecret, "", "", ""), "sess-gone", now.Add(-time.Hour))
	bdCapture(t, lc.db, lockedSlug, "ledger-capture", "presented")
	// An open project's task waits on the locked project's open task.
	_, err := store.EnsureProject(context.Background(), lc.db, "example-web", "example-web")
	require.NoError(t, err)
	bdTask(t, lc.db, "example-web", "", "waits on the ledger", "", "", "", lc.taskID)
	bdTask(t, lc.db, "example-web", "", "free", "", "", "")

	get := func(target string) string {
		rr := lc.do(lc.browser(http.MethodGet, target, "", nil))
		require.Equal(t, http.StatusOK, rr.Code, target)
		return rr.Body.String()
	}
	var data projectsData
	require.NoError(t, json.Unmarshal([]byte(get("/console/projects?format=json&group=flat")), &data))
	rows := bdRows(data)
	locked := rows[lockedSlug]
	require.True(t, locked.Locked)
	require.Equal(t, projectRowVM{Slug: lockedSlug, Name: lockedSlug, Locked: true, TileIcon: "lock",
		Sessions: 1, OpenTasks: 4, Memories: 1, Notes: 2, LastActive: locked.LastActive}, locked,
		"counts and last activity only: no work, no plans, no attention")
	require.Zero(t, data.PlansActive, "the locked plan is not read")
	require.Zero(t, data.PlansAwaiting, "the locked capture is not read")
	require.Zero(t, data.GatesOpen)
	require.Zero(t, data.Lapsed)

	web := rows["example-web"]
	require.Equal(t, 1, web.Blocked, "blocked by a locked project's task, as the Tasks page says")
	require.Equal(t, 1, web.Ready)
	rr := lc.do(lc.browser(http.MethodGet, "/console/tasks?format=json", "", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	rails := bdTasksRails(t, rr.Body.String())
	require.Equal(t, rails["blocked"]["example-web"], web.Blocked)
	require.Equal(t, rails["ready"]["example-web"], web.Ready)

	var narrowed projectsData
	require.NoError(t, json.Unmarshal([]byte(get("/console/projects?format=json&show=attention")), &narrowed))
	require.NotContains(t, bdRows(narrowed), lockedSlug, "a locked row is never judged, so the attention view leaves it out")
	require.Empty(t, narrowed.Locked)

	for _, target := range []string{"/console/projects", "/console/projects?show=attention", "/console/projects?format=json",
		"/console/projects?group=flat"} {
		body := get(target)
		for _, secret := range []string{lockedSecret, "ledger-capture", "ledger-plan", lc.taskID} {
			require.NotContains(t, body, secret, "%s shows a locked item", target)
		}
	}
	html := get("/console/projects?group=flat")
	row := html[strings.Index(html, `<article id="p-`+lockedSlug+`"`):]
	row = row[:strings.Index(row, "</article>")]
	require.Contains(t, row, "Name, counts and last activity only: 1 mem &middot; 2 notes &middot; 1 session.")
	require.NotContains(t, row, "data-need=")
	require.NotContains(t, row, "data-word=")
}
