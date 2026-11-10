package console

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// Provenance labels for restricted local accounts: every page that shows a
// memory, note, task, session or trial says "written by account <name>" when a
// restricted account wrote it, and an owner item renders exactly as before.

// accountChip is the one chip markup every surface renders.
func accountChip(name string) string {
	return `<span class="kind faint" data-account="` + name +
		`" title="Written by a restricted local account (peer_auth.restricted)">written by account ` + name + `</span>`
}

func updatedAccountChip(name string) string {
	return `<span class="kind faint" data-updated-account="` + name +
		`" title="Title or body rewritten by a restricted local account (peer_auth.restricted)">updated by account ` + name + `</span>`
}

// labeledItems seeds one item of each kind written by the restricted account
// and one twin written by the owner, and returns their ids.
type labeledIDs struct {
	memory, ownerMemory   string
	note, ownerNote       string
	task, ownerTask       string
	session, ownerSession string
	trial, ownerTrial     string
}

func seedLabeledItems(t *testing.T, f heldFixture) labeledIDs {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	newID := func() string {
		id, err := core.NewID()
		require.NoError(t, err)
		return id
	}
	var ids labeledIDs

	writeMem := func(name, account string) string {
		m, err := f.mgr.WriteMemory(ctx, core.Memory{ID: newID(), Kind: core.KindGotcha, Name: name,
			Description: "a labeled gotcha", Project: "app", Body: "Body of " + name + ".\n",
			Created: now, Updated: now, ValidFrom: now, Account: account})
		require.NoError(t, err)
		return m.ID
	}
	ids.memory, ids.ownerMemory = writeMem("account-gotcha", exampleAccount), writeMem("owner-gotcha", "")

	writeNote := func(slug, account string) string {
		n, err := f.mgr.WriteNote(ctx, core.Note{ID: newID(), Title: "Note " + slug, Slug: slug, Project: "app",
			Body: "Body of " + slug + ".", Created: now, Updated: now, Account: account})
		require.NoError(t, err)
		return n.ID
	}
	ids.note, ids.ownerNote = writeNote("account-note", exampleAccount), writeNote("owner-note", "")

	createSession := func(name, account string) string {
		id := newID()
		require.NoError(t, store.CreateSession(ctx, f.db, core.Session{ID: id, Name: name, ProjectSlug: "app",
			Status: core.SessionActive, Findings: "Found the flaky test.", Account: account,
			CreatedAt: now, UpdatedAt: now}))
		return id
	}
	ids.session, ids.ownerSession = createSession("cc/account01", exampleAccount), createSession("cc/owner001", "")

	createTask := func(title, account string) string {
		id := newID()
		require.NoError(t, store.CreateTask(ctx, f.db, core.Task{ID: id, ProjectSlug: "app", Title: title,
			Status: core.TaskOpen, Account: account, CreatedAt: now, UpdatedAt: now}))
		return id
	}
	ids.task, ids.ownerTask = createTask("account task", exampleAccount), createTask("owner task", "")
	// The restricted account also changed its task: the store stamps the
	// changer from the request's view.
	restricted := access.WithView(ctx, restrictedConsoleView(t))
	body := "Changed by the account."
	_, err := store.UpdateTask(restricted, f.db, ids.task, store.TaskPatch{Body: &body}, exampleAccount, now)
	require.NoError(t, err)

	createTrial := func(title, account string) string {
		id := newID()
		require.NoError(t, store.CreateTrial(ctx, f.db, core.Trial{ID: id, Lab: "boot-lab", Title: title,
			Outcome: core.OutcomePass, ProjectSlug: "app", Expected: "boots", Actual: "boots",
			Account: account, CreatedAt: now}))
		return id
	}
	ids.trial, ids.ownerTrial = createTrial("account trial", exampleAccount), createTrial("owner trial", "")
	return ids
}

func TestAccountChips_LabelAccountItemsAndLeaveOwnerItemsAlone(t *testing.T) {
	f := newRestrictedConsole(t, config.Features{Research: true}, true)
	ids := seedLabeledItems(t, f)
	chip := accountChip(exampleAccount)

	for _, tc := range []struct {
		name, labeled, owner string
	}{
		{"memory reader", "/console/memories/" + ids.memory + "?reader=1", "/console/memories/" + ids.ownerMemory + "?reader=1"},
		{"memory peek", "/console/memories/" + ids.memory + "?peek=1", "/console/memories/" + ids.ownerMemory + "?peek=1"},
		{"note reader", "/console/notes/" + ids.note + "?reader=1", "/console/notes/" + ids.ownerNote + "?reader=1"},
		{"note peek", "/console/notes/" + ids.note + "?peek=1", "/console/notes/" + ids.ownerNote + "?peek=1"},
		{"task reader", "/console/tasks/" + ids.task + "?reader=1", "/console/tasks/" + ids.ownerTask + "?reader=1"},
		{"task peek", "/console/tasks/" + ids.task + "?peek=1", "/console/tasks/" + ids.ownerTask + "?peek=1"},
		{"session page", "/console/sessions/" + ids.session, "/console/sessions/" + ids.ownerSession},
		{"session peek", "/console/sessions/" + ids.session + "?peek=1", "/console/sessions/" + ids.ownerSession + "?peek=1"},
		{"trial reader", "/console/trials/" + ids.trial + "?reader=1", "/console/trials/" + ids.ownerTrial + "?reader=1"},
		{"trial peek", "/console/trials/" + ids.trial + "?peek=1", "/console/trials/" + ids.ownerTrial + "?peek=1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			labeled := getPeek(t, f.mux, tc.labeled)
			require.Equal(t, 200, labeled.Code, labeled.Body.String())
			require.Contains(t, labeled.Body.String(), chip)
			owner := getPeek(t, f.mux, tc.owner)
			require.Equal(t, 200, owner.Code)
			require.NotContains(t, owner.Body.String(), "data-account")
			require.NotContains(t, owner.Body.String(), "data-updated-account")
			require.NotContains(t, owner.Body.String(), "by account")
		})
	}

	// A task the account created and then changed carries both chips.
	task := getPeek(t, f.mux, "/console/tasks/"+ids.task+"?peek=1").Body.String()
	require.Contains(t, task, updatedAccountChip(exampleAccount))

	// The session page labels its findings as the session's account's.
	sess := getPeek(t, f.mux, "/console/sessions/"+ids.session).Body.String()
	require.Contains(t, sess, `<span class="session-review-cue">Start here</span> `+chip)

	// The list rows label the account's items, and only those.
	for _, tc := range []struct {
		list, labeledName, ownerName string
	}{
		{"/console/memories", "account-gotcha", "owner-gotcha"},
		{"/console/notes", "Note account-note", "Note owner-note"},
		{"/console/tasks", "account task", "owner task"},
		// The account's session by its qualified name (SEAL A2).
		{"/console/sessions", exampleAccount + ":cc/account01", "cc/owner001"},
		{"/console/trials", "account trial", "owner trial"},
		{"/console/now", exampleAccount + ":cc/account01", "cc/owner001"},
	} {
		page := getPeek(t, f.mux, tc.list).Body.String()
		require.Contains(t, rowAround(t, page, tc.labeledName), chip, "%s: the account's row", tc.list)
		require.NotContains(t, rowAround(t, page, tc.ownerName), "data-account", "%s: the owner's row", tc.list)
	}

	// JSON carries the account on labeled items only, in the console's style.
	var mem map[string]any
	getJSON(t, f.mux, "/console/memories/"+ids.memory+"?format=json", &mem)
	require.Equal(t, exampleAccount, mem["account"])
	var ownerMem map[string]any
	getJSON(t, f.mux, "/console/memories/"+ids.ownerMemory+"?format=json", &ownerMem)
	require.NotContains(t, ownerMem, "account")
	var td map[string]any
	getJSON(t, f.mux, "/console/tasks/"+ids.task+"?format=json", &td)
	require.Equal(t, exampleAccount, td["account"])
	require.Equal(t, exampleAccount, td["updatedAccount"])
	var otd map[string]any
	getJSON(t, f.mux, "/console/tasks/"+ids.ownerTask+"?format=json", &otd)
	require.NotContains(t, otd, "account")
	require.NotContains(t, otd, "updatedAccount")
}

// An install without restricted accounts has nothing to label: no page grows
// a chip, and the list JSON grows no key.
func TestAccountChips_NoneWithoutRestrictedAccounts(t *testing.T) {
	f := newRestrictedConsole(t, config.Features{Research: true}, false)
	ctx := context.Background()
	now := time.Now().UTC()
	id, err := core.NewID()
	require.NoError(t, err)
	_, err = f.mgr.WriteMemory(ctx, core.Memory{ID: id, Kind: core.KindGotcha, Name: "plain-gotcha",
		Description: "an owner gotcha", Project: "app", Body: "Body.\n", Created: now, Updated: now, ValidFrom: now})
	require.NoError(t, err)
	seedOwnerWork(t, f.db, now)

	for _, path := range []string{"/console/memories", "/console/memories/" + id, "/console/notes", "/console/tasks",
		"/console/sessions", "/console/trials", "/console/now"} {
		page := getPeek(t, f.mux, path).Body.String()
		require.NotContains(t, page, "data-account", path)
		require.NotContains(t, page, "by account", path)
	}
	for _, path := range []string{"/console/memories?format=json", "/console/tasks?format=json",
		"/console/sessions?format=json", "/console/trials?format=json"} {
		require.NotContains(t, getPeek(t, f.mux, path).Body.String(), `"account"`, path)
	}
}

func seedOwnerWork(t *testing.T, db *sql.DB, now time.Time) {
	t.Helper()
	ctx := context.Background()
	newID := func() string {
		id, err := core.NewID()
		require.NoError(t, err)
		return id
	}
	require.NoError(t, store.CreateSession(ctx, db, core.Session{ID: newID(), Name: "cc/owner002", ProjectSlug: "app",
		Status: core.SessionActive, CreatedAt: now, UpdatedAt: now}))
	require.NoError(t, store.CreateTask(ctx, db, core.Task{ID: newID(), ProjectSlug: "app", Title: "owner task",
		Status: core.TaskOpen, CreatedAt: now, UpdatedAt: now}))
	require.NoError(t, store.CreateTrial(ctx, db, core.Trial{ID: newID(), Lab: "boot-lab", Title: "owner trial",
		Outcome: core.OutcomePass, ProjectSlug: "app", CreatedAt: now}))
}

// rowTags are the tags a list row opens with -- a library rail item, a
// session row, a live agent card's head -- and the tag that closes it.
var rowTags = [][2]string{
	{`<a class="rail-item`, "</a>"},
	{`<div class="id-line"`, "</div>"},
	{`<article class="now-agent"`, "</header>"},
}

// rowAround returns the list row that names name: from the nearest row tag
// before it to that row's closing tag.
func rowAround(t *testing.T, page, name string) string {
	t.Helper()
	at := strings.Index(page, ">"+name+"<")
	require.GreaterOrEqual(t, at, 0, "no row names %q", name)
	start, closer := -1, ""
	for _, tag := range rowTags {
		if i := strings.LastIndex(page[:at], tag[0]); i > start {
			start, closer = i, tag[1]
		}
	}
	require.GreaterOrEqual(t, start, 0, "no row tag before %q", name)
	end := strings.Index(page[at:], closer)
	require.GreaterOrEqual(t, end, 0)
	return page[start : at+end]
}
