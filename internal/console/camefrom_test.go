package console

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// A project page's "came from <project>" filter (plan fenced-sessions P10):
// the memories, notes and tasks of a project that may contain content from
// another one, over the provenance index.

// p10CameFromItems writes example-legal's items: one of each kind stamped from
// example-finance, a memory and a task stamped from example-hr, an unstamped
// one of each kind, and a stamp row that outlived its task.
func p10CameFromItems(t *testing.T, f convFixture) map[string]string {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	// The approvals the stamps name: the index reads a stamp's word from its
	// approval's kind.
	grant, err := store.CreateGrantRequest(ctx, f.db, store.GrantRequest{Project: "example-finance", From: "example-legal",
		Lease: time.Hour, TTL: 10 * time.Minute, SessionID: "01P10ASKER0000000000000000", SessionName: "cc/p10-asker"}, now)
	require.NoError(t, err)
	carry, err := store.CarryOf("example-ops", []string{"example-legal"}, []string{"example-legal"})
	require.NoError(t, err)
	move, err := store.MoveRequestCreate(ctx, f.db, store.MoveRequest{Project: "example-legal", From: "example-ops", Carry: carry,
		Lease: time.Hour, TTL: 10 * time.Minute, ConversationClient: "claude-code", ConversationID: "p10-mover",
		SessionID: "01P10MOVER0000000000000000", SessionName: "cc/p10-mover"}, now)
	require.NoError(t, err)
	fin := []core.Stamp{{Project: "example-finance", Via: core.StampViaGrant, Ref: grant.ID}}
	hr := []core.Stamp{{Project: "example-hr", Via: core.StampViaMove, Ref: move.ID}}
	ids := map[string]string{"grant": grant.ID}
	mem := func(name string, stamps []core.Stamp) {
		m, err := f.mgr.WriteMemory(ctx, core.Memory{ID: mustID(t), Kind: core.KindDecision, Name: name, Description: name + " rule",
			Project: "example-legal", Body: name + "\n", Created: now, Updated: now, ValidFrom: now, MayContain: stamps})
		require.NoError(t, err)
		ids[name] = m.ID
	}
	mem("contract-terms", fin)
	mem("retention-policy", hr)
	mem("plain-legal", nil)
	note := func(title string, stamps []core.Stamp) {
		n, err := f.mgr.WriteNote(ctx, core.Note{ID: mustID(t), Title: title, Slug: strings.ReplaceAll(title, " ", "-"),
			Project: "example-legal", Body: title + "\n", Created: now, Updated: now, MayContain: stamps})
		require.NoError(t, err)
		ids[title] = n.ID
	}
	note("vendor review", fin)
	note("plain legal note", nil)
	task := func(title string, stamps []core.Stamp) {
		tk := core.Task{ID: mustID(t), ProjectSlug: "example-legal", Title: title, Status: core.TaskOpen, CreatedAt: now, UpdatedAt: now}
		require.NoError(t, store.CreateTask(ctx, f.db, tk))
		require.NoError(t, store.StampWorkItem(ctx, f.db, core.ItemKindTask, tk.ID, tk.ProjectSlug, stamps, now))
		ids[title] = tk.ID
	}
	task("sign the vendor contract", fin)
	task("archive the old policy", hr)
	task("plain legal chore", nil)
	ids["orphan"] = mustID(t)
	require.NoError(t, store.StampWorkItem(ctx, f.db, core.ItemKindTask, ids["orphan"], "example-legal", fin, now))
	return ids
}

func TestCameFrom_ProjectPageFilter(t *testing.T) {
	f := convConsole(t)
	ids := p10CameFromItems(t, f)
	finLine := core.MayContainLine([]core.Stamp{{Project: "example-finance", Via: core.StampViaGrant, Ref: ids["grant"]}})

	page := getHTMLBody(t, f.mux, "/console/projects/example-legal?tab=memories")
	for _, tab := range []string{"memories", "notes", "tasks"} {
		panel := tabPanel(t, page, tab)
		require.Contains(t, panel, `id="camefrom-`+tab+`"`, tab)
		require.Contains(t, panel, `data-camefrom="example-finance"`, tab)
		require.Contains(t, panel, `data-camefrom="example-hr"`, tab)
		require.NotContains(t, panel, `data-camefrom-active`, "no filter is on")
	}
	finChip := between(t, tabPanel(t, page, "tasks"), `data-camefrom="example-finance"`, `<a class="camefrom-chip`, "</a>")
	require.Contains(t, finChip, `<span class="pill">1</span>`, "the orphaned stamp counts nowhere")
	memories := tabPanel(t, page, "memories")
	require.Contains(t, memories, "plain-legal", "unfiltered, every memory")
	require.Contains(t, between(t, memories, "contract-terms", "<tr", "</tr>"), finLine, "a row shows its stamp")
	require.NotContains(t, between(t, memories, "plain-legal", "<tr", "</tr>"), "data-may-contain")
	require.Contains(t, between(t, tabPanel(t, page, "notes"), "vendor review", "<tr", "</tr>"), finLine)
	require.Contains(t, tabPanel(t, page, "tasks"), "Ready queue", "unfiltered, the plans and the ready queue")

	// Filtered: each tab lists the items that may contain content from
	// example-finance, and counts them.
	page = getHTMLBody(t, f.mux, "/console/projects/example-legal?tab=memories&from=example-finance")
	memories = tabPanel(t, page, "memories")
	require.Contains(t, memories, ids["contract-terms"])
	require.NotContains(t, memories, "plain-legal")
	require.NotContains(t, memories, "retention-policy")
	require.Contains(t, memories, `data-camefrom-active="example-finance"`)
	require.Contains(t, between(t, memories, `data-camefrom="example-finance"`, `<a class="camefrom-chip`, "</a>"), `aria-current="true"`)
	notes := tabPanel(t, page, "notes")
	require.Contains(t, notes, "vendor review")
	require.NotContains(t, notes, "plain legal note")
	tasks := tabPanel(t, page, "tasks")
	require.Contains(t, tasks, "Tasks that may contain content from example-finance")
	require.Contains(t, tasks, ids["sign the vendor contract"])
	require.Contains(t, between(t, tasks, "sign the vendor contract", "<tr", "</tr>"), finLine)
	require.NotContains(t, tasks, "archive the old policy")
	require.NotContains(t, tasks, "plain legal chore")
	require.NotContains(t, tasks, ids["orphan"])
	require.NotContains(t, tasks, "Ready queue", "the filter lists the tasks, not the plans")
	bar := page[strings.Index(page, `id="tabs"`):]
	bar = bar[:strings.Index(bar, "</div>")]
	require.Contains(t, bar, `href="?tab=notes&amp;from=example-finance"`, "the tab links keep the filter")
	require.Contains(t, between(t, bar, "Memories", "<a", "</a>"), `<span class="pill">1</span>`, "a narrowed tab counts what it lists")
	require.Contains(t, page, "on.getAttribute('data-camefrom-active')", "a tab switch keeps the filter")

	// A project no stamp names is a filter that matches nothing, named.
	page = getHTMLBody(t, f.mux, "/console/projects/example-legal?from=example-nowhere")
	require.Contains(t, tabPanel(t, page, "memories"), "No memories in example-legal may contain content from example-nowhere")
	require.Contains(t, tabPanel(t, page, "notes"), "No notes in example-legal may contain content from example-nowhere")
	require.Contains(t, tabPanel(t, page, "tasks"), "No tasks in example-legal may contain content from example-nowhere")
	require.Contains(t, tabPanel(t, page, "memories"), `data-camefrom="example-nowhere"`)

	// Strict like ?tab=: present and uninterpretable is a 400.
	for _, q := range []string{"from=", "from=example-finance&from=example-hr", "from=..%2Fnotes", "from=a%2Fb"} {
		rr := getPeek(t, f.mux, "/console/projects/example-legal?"+q)
		require.Equal(t, http.StatusBadRequest, rr.Code, q)
	}

	// A project none of whose items carries a stamp shows no filter and no
	// stamp: its page is as it was.
	writeNote(t, f.mgr, "example-ops", "ops-plain", "ops plain note", "nothing held")
	page = getHTMLBody(t, f.mux, "/console/projects/example-ops")
	require.NotContains(t, page, "camefrom")
	require.NotContains(t, page, "data-may-contain")
	require.NotContains(t, page, "data-camefrom-active", "nor the tab script's filter half")
}

// A source that is a locked project this request does not read is named and
// nothing more; the items the filter lists are this project's own.
func TestCameFrom_LockedSourceIsNamedOnly(t *testing.T) {
	lc := newLockedConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()
	stamp := []core.Stamp{{Project: lockedSlug, Via: core.StampViaMove, Ref: "01P10LOCKEDMOVE00000000000"}}
	mem, err := lc.mgr.WriteMemory(ctx, core.Memory{ID: mustID(t), Kind: core.KindDecision, Name: "ops-close-summary",
		Description: "what ops needs from the close", Project: openSlug, Body: "summary\n", Created: now, Updated: now, ValidFrom: now,
		MayContain: stamp})
	require.NoError(t, err)

	code, page := lc.get(t, "/console/projects/"+openSlug+"?tab=memories&from="+lockedSlug, "")
	require.Equal(t, http.StatusOK, code)
	require.NotContains(t, page, lockedSecret)
	require.NotContains(t, page, lc.memID)
	memories := tabPanel(t, page, "memories")
	require.Contains(t, memories, mem.ID)
	require.NotContains(t, memories, "ops-runbook", "the unstamped memory is filtered out")
	chip := between(t, memories, `data-camefrom="`+lockedSlug+`"`, `<a class="camefrom-chip`, "</a>")
	require.Contains(t, chip, "ico-lock")
	require.Contains(t, chip, "A locked project: its name only.")

	// The locked project's own page is still its unlock page.
	code, _ = lc.get(t, "/console/projects/"+lockedSlug+"?from="+openSlug, "")
	require.Equal(t, http.StatusSeeOther, code)
}
