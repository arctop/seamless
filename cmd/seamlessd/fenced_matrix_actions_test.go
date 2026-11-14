package main

// The actions of the fenced-sessions matrix (fenced_matrix_test.go), one
// function each, in the order fmRun runs them on one conversation.

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/harvest"
	"github.com/arctop/seamless/internal/plans"
	"github.com/arctop/seamless/internal/store"
)

// tag is a row's own words for what: its case named in them, so a text one
// row records never reads as another's.
func (row *fmRow) tag(what string) string {
	return "fm " + what + " " + strings.NewReplacer("/", " ", "_", " ").Replace(row.c.String())
}

// fmRebind: session_start into an open project or the global scope is
// refused -- a conversation that holds a fenced project's content never goes
// back, native ones included -- and binds nothing; session_start with no
// arguments hands the connection to its own home session; an unscoped write
// lands in the home.
func fmRebind(t *testing.T, row *fmRow) {
	r, c := row.r, row.c
	cli := row.conv.dial()
	home := row.home()
	for _, dest := range []string{gmDocs, "global"} {
		where := "project " + dest
		if dest == "global" {
			where = "the global scope"
		}
		text := fmRefused(t, cli, "session_start", map[string]any{"project": dest})
		require.True(t, strings.HasPrefix(text, "session_start: nothing was bound: this conversation holds content from "), text)
		label, _, _ := strings.Cut(strings.TrimPrefix(text, "session_start: nothing was bound: this conversation holds content from "), ", so it")
		require.Contains(t, strings.Split(label, ", "), c.target, text)
		require.Contains(t, text, ", so it does not work in "+where+": a conversation holding a fenced project's content "+
			"never goes back to an open project or the global scope\nRemedy: keep working in project "+c.target+
			", your own session "+home.Name+"; record there what you meant to record in "+where, text)
	}
	own := gmOK(t, cli, "session_start", map[string]any{})
	require.Equal(t, home.ID, own["session_id"], "its own home session")
	require.Equal(t, c.target, own["project"])
	name := "fm-rebind"
	mem := gmOK(t, cli, "memory_write", map[string]any{"name": name, "kind": "reference", "description": "d", "body": "b\n"})
	require.Equal(t, c.target, mem["project"], "nothing was bound elsewhere: an unscoped write lands in the home")
	r.noLanded(gmDocs, "rebind", name, "fm rebind")
	r.noLanded("", "rebind", name, "fm rebind")
}

// fmHandle: a session= handle naming a session in an open project acts as
// that session nowhere -- not for a write, not for a read -- since the home
// may not write there; naming its own home session acts as itself.
func fmHandle(t *testing.T, row *fmRow) {
	r, c := row.r, row.c
	home := row.home()
	text := fmRefused(t, row.cli, "memory_write", map[string]any{"session": "docs-agent-session", "name": "fm-handle",
		"kind": "reference", "description": "d", "body": "b\n"})
	head := "memory_write: session docs-agent-session works in project " + gmDocs + ", and this conversation works in project " +
		c.target + ": "
	tail := ", so it may not act as that session\nRemedy: leave session= off to act as your own session " + home.Name +
		" in project " + c.target
	if c.sealed() {
		// A sealed home reads nothing outside it, so the open project is
		// unread because of the home's fence, never its own: an open project
		// is read by every other conversation.
		require.Equal(t, head+"its home's fence keeps its reads out of project "+gmDocs+tail, text)
	} else {
		require.True(t, strings.HasPrefix(text, head+"it holds content from "), text)
		require.True(t, strings.HasSuffix(text, ", which may not be written into project "+gmDocs+tail), text)
	}
	text = fmRefused(t, row.cli, "memory_read", map[string]any{"session": "docs-agent-session", "name": "docs-style"})
	require.NotContains(t, text, gmDocsToken, "acting as the open project's session is a read there too")
	r.noLanded(gmDocs, "session=", "fm-handle", "fm handle")
	got := gmOK(t, row.cli, "memory_write", map[string]any{"session": home.Name, "name": "fm-own-handle",
		"kind": "reference", "description": "d", "body": "b\n"})
	require.Equal(t, c.target, got["project"], "its own session, named, is itself")
}

// fmReceipt: a moved conversation closes the step its pre-move session
// claimed with a receipt -- the status and at most one reference to an item
// in its new home, never any text -- written as that session, which releases
// the claim; nothing else reaches the home it left. A sealed home sends none,
// and a native conversation, which left no session, closes nothing by one.
func fmReceipt(t *testing.T, row *fmRow) {
	r, c := row.r, row.c
	noteID := gmOK(t, row.cli, "notes_create", map[string]any{"title": "fm receipt note", "body": "what was done"})["id"].(string)
	ref := c.target + ":" + noteID
	unchanged := func(id, claimedBy, what string) {
		t.Helper()
		task, err := store.TaskByID(r.ctx, r.db, id)
		require.NoError(t, err)
		require.Equal(t, core.TaskInProgress, task.Status, what)
		require.Equal(t, claimedBy, task.ClaimedBy, what)
	}
	builder := func() string {
		task, err := store.TaskByID(r.ctx, r.db, row.openStep)
		require.NoError(t, err)
		return task.ClaimedBy
	}()
	sealedRule := "a conversation whose home is sealed project " + gmSealed + " sends no receipts"
	text := fmRefused(t, row.cli, "tasks_update", map[string]any{"id": row.openStep, "status": "done", "reference": ref})
	if c.sealed() {
		require.Contains(t, text, sealedRule, text)
	} else {
		require.Contains(t, text, "a receipt closes only a task this conversation claimed before it moved", text)
	}
	unchanged(row.openStep, builder, "another agent's step")
	if !c.moved {
		return
	}
	if c.sealed() {
		text := fmRefused(t, row.cli, "tasks_update", map[string]any{"id": row.step, "status": "done"})
		require.Contains(t, text, sealedRule, text)
		unchanged(row.step, row.start.ID, "a sealed home's step")
		return
	}
	text = fmRefused(t, row.cli, "tasks_update", map[string]any{"id": row.step, "status": "dropped", "body": "carried text"})
	require.Contains(t, text, "a receipt carries no text: it sets the status alone, with at most one reference to an item "+
		"in the conversation's home, "+c.target+", so body is refused", text)
	unchanged(row.step, row.start.ID, "a receipt with text")

	got := gmOK(t, row.cli, "tasks_update", map[string]any{"id": row.step, "status": "done", "reference": ref})
	require.Equal(t, true, got["receipt"], "%v", got)
	require.Equal(t, ref, got["reference"])
	task, err := store.TaskByID(r.ctx, r.db, row.step)
	require.NoError(t, err)
	require.Equal(t, core.TaskDone, task.Status)
	require.Empty(t, task.ClaimedBy, "the receipt releases the claim")
	require.Equal(t, gmOpen, task.ProjectSlug)
	require.Equal(t, "the step's body", task.Body)
	require.Equal(t, 1, r.count(`SELECT COUNT(*) FROM events WHERE kind = 'task.transition' AND item_id = ? AND session_id = ?
		AND project_slug = ? AND json_extract(payload, '$.receipt') = 1 AND json_extract(payload, '$.reference') = ?`,
		row.step, row.start.ID, gmOpen, ref), "recorded as the session that claimed it, under the step's project")
	require.Empty(t, r.fmStamps(core.ItemKindTask, row.step), "a receipt carries nothing to stamp")

	text = fmRefused(t, row.cli, "tasks_add", map[string]any{"project": gmOpen, "title": "fm after the move"})
	require.Contains(t, text, "a write into project "+gmOpen+" is refused", text)
	r.noLanded(gmOpen, "after the move", "fm-after-the-move", "fm after the move")
}

// fmWriteGrant: from a confidential home (a locked one included), a write
// into another confidential project files a read-and-write request whose
// facts state the whole carry; approved read and write, the conversation
// writes there as its own sessions do and reads what it wrote. A sealed
// home asks for no confidential project, and nothing is filed.
func fmWriteGrant(t *testing.T, row *fmRow) {
	r, c := row.r, row.c
	grants := r.grantRows("")
	text := fmRefused(t, row.cli, "tasks_add", map[string]any{"project": gmHome, "title": "fm visit task", "body": "the visit's body"})
	if c.sealed() {
		require.NotContains(t, text, "Remedy: read-and-write request", text)
		items, isErr := gmCall(t, row.cli, "access_request", map[string]any{"project": gmHome, "write": true})
		require.True(t, isErr, "a sealed home asks for no confidential project: %v", items)
		require.Equal(t, grants, r.grantRows(""), "nothing filed")
		require.Zero(t, r.count(`SELECT COUNT(*) FROM tasks WHERE project_slug = ? AND title = 'fm visit task'`, gmHome))
		return
	}
	ls := strings.Split(text, "\n")
	require.Len(t, ls, 2, text)
	require.True(t, strings.HasPrefix(ls[0], "tasks_add: this "+c.holder()+" holds content from "), ls[0])
	require.True(t, strings.HasSuffix(ls[0], ", so it writes only into "+c.target+": a write into project "+gmHome+" is refused"), ls[0])
	require.True(t, strings.HasPrefix(ls[1], "Remedy: read-and-write request R-"), ls[1])
	short := fmShortOf(t, ls[1])
	f := r.gmFacts(r.tcp.URL, gmKey, short)
	require.Equal(t, store.AccessKindGrant, f.Kind)
	require.Equal(t, gmHome, f.Project)
	require.Equal(t, c.target, f.From)
	require.Equal(t, []string{"read-only", "read-write"}, f.AccessChoices)
	require.Equal(t, "read-write", f.AskedAccess)
	// The whole carry: the home, what it holds once approved, and where it
	// may write -- the default, read-only, approval's; the read-and-write
	// line says what the other adds.
	holds := []string{c.target, gmHome}
	slices.Sort(holds)
	require.Equal(t, approval.CarryFacts{Home: c.target, Holds: holds, WriteTo: []string{c.target}}, *f.Carry)
	warnings := []approval.Warning{{Code: approval.WarnCopyInto, Text: "content from " + gmHome + " may be copied into " + c.target}}
	if c.moved {
		warnings = append(warnings, approval.Warning{Code: approval.WarnCWDOutside,
			Text: "the session works outside " + c.target + "'s repository"})
	}
	require.Equal(t, warnings, f.Warnings)
	require.Equal(t, "content from "+gmHome+" may be copied into "+c.target+", and it may change "+gmHome+
		"'s tasks, memories and notes; content from "+c.target+" may be copied into "+gmHome, f.ReadWrite)
	require.False(t, f.Locked)
	r.fmApprove(short, "read-write")
	row.grant = f.ID

	got := gmOK(t, row.cli, "tasks_add", map[string]any{"project": gmHome, "title": "fm visit task", "body": "the visit's body"})
	require.Nil(t, got["withheld"], "a read-and-write grant reads what it writes: %v", got)
	require.Equal(t, "the visit's body", got["body"])
	mem := gmOK(t, row.cli, "memory_write", map[string]any{"project": gmHome, "name": "fm-visit", "kind": "reference",
		"description": "the quarterly ledger visit", "body": "what the visit found\n"})
	require.Equal(t, gmHome, mem["project"])
}

// fmStampRows: everything written while the conversation holds a fenced
// project other than the destination records it, set from the
// conversation's state: what it wrote into the project it visited names its
// home, and what it writes into its home now names the project it visited,
// each by the grant's approval -- in the file for a memory, in the index for
// a task, and on reads. A conversation that holds nothing but its home stamps
// nothing.
func fmStampRows(t *testing.T, row *fmRow) {
	r, c := row.r, row.c
	homeMem := gmOK(t, row.cli, "memory_write", map[string]any{"name": "fm-stamped", "kind": "reference",
		"description": "d", "body": "b\n"})
	homeID := homeMem["id"].(string)
	homeRead := gmOK(t, row.cli, "memory_read", map[string]any{"id": homeID})
	if c.sealed() {
		require.Empty(t, r.fmStamps(core.ItemKindMemory, homeID), "it holds nothing but its home")
		require.Nil(t, homeRead["may_contain"])
		return
	}
	require.Equal(t, []string{gmHome + " " + row.grant}, r.fmStamps(core.ItemKindMemory, homeID))
	require.Equal(t, []any{map[string]any{"project": gmHome, "grant": row.grant}}, homeRead["may_contain"])
	visit, found, err := store.MemoryByName(r.ctx, r.db, gmHome, "fm-visit")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, []string{c.target + " " + row.grant}, r.fmStamps(core.ItemKindMemory, visit.ID))
	require.Equal(t, []string{"- project: " + c.target, "grant: " + row.grant}, r.fmFrontmatterStamps(visit.FilePath),
		"the file carries the stamp")
	read := gmOK(t, row.cli, "memory_read", map[string]any{"project": gmHome, "name": "fm-visit"})
	require.Equal(t, []any{map[string]any{"project": c.target, "grant": row.grant}}, read["may_contain"])
	var taskID string
	require.NoError(t, r.db.QueryRowContext(r.ctx, `SELECT id FROM tasks WHERE project_slug = ? AND title = 'fm visit task'`,
		gmHome).Scan(&taskID))
	require.Equal(t, []string{c.target + " " + row.grant}, r.fmStamps(core.ItemKindTask, taskID))
}

// fmSubagent: a subagent shares its conversation's home and label: its
// briefing is the home's, wherever the hook fires from, and a planning
// subagent's report lands in the home, stamped like every other write there.
func fmSubagent(t *testing.T, row *fmRow) {
	r, c := row.r, row.c
	agentID := "fmsub" + strings.ReplaceAll(row.conv.extID, "-", "")
	prompt, report := "check the quarterly ledger visit", row.tag("subagent report")
	a := row.conv.subagentStart(agentID, prompt, report)
	require.Contains(t, a.context, c.target+" house rule", "a subagent is briefed from its conversation's home")
	require.NotContains(t, a.context, gmOpen+" house rule")
	// Its RELEVANT section reads the home and the targets of the
	// conversation's live grants -- the project the write grant visited --
	// and no other project, an open one the home could read included.
	require.NotContains(t, a.context, "docs-ledger-visit", a.context)
	if c.sealed() {
		require.NotContains(t, a.context, "fm-visit", a.context)
	} else {
		require.Contains(t, a.context, "fm-visit", "the live grant's target is in the subagent's scope: %s", a.context)
	}
	stop := row.conv.subagentStop(agentID, prompt, report)
	require.Empty(t, stop.captureRefusedBy)
	note, found, err := store.NoteBySlug(r.ctx, r.db, c.target, plans.AgentNotePrefix+agentID)
	require.NoError(t, err)
	require.True(t, found, "the report lands in the home")
	require.Contains(t, r.fmFile(note.FilePath), report)
	for _, p := range []string{gmOpen, gmDocs, gmHome, ""} {
		_, found, err := store.NoteBySlug(r.ctx, r.db, p, plans.AgentNotePrefix+agentID)
		require.NoError(t, err)
		require.False(t, found, "and nowhere else: %q", p)
	}
	row.stampedLikeTheHome(t, core.ItemKindNote, note.ID)
}

// stampedLikeTheHome requires that an item written into the home carries
// the stamp of the project the write grant row visited, by that grant's
// approval -- and none from a sealed home, which holds nothing else.
func (row *fmRow) stampedLikeTheHome(t *testing.T, kind, id string) {
	t.Helper()
	if row.c.sealed() {
		require.Empty(t, row.r.fmStamps(kind, id), "a conversation holding nothing but its home stamps nothing")
		return
	}
	require.NotEmpty(t, row.grant, "the write grant row approved its grant")
	require.Equal(t, []string{gmHome + " " + row.grant}, row.r.fmStamps(kind, id), "%s %s", kind, id)
}

// fmCaptures: a plan the conversation writes is captured into its home,
// wherever the hook fires from, filed under its home session.
func fmCaptures(t *testing.T, row *fmRow) {
	r, c := row.r, row.c
	basename := "fm-plan-" + strings.ReplaceAll(strings.TrimPrefix(row.conv.extID, "fm-"), "_", "-")
	a := row.conv.plan(basename, "# Plan\n\n"+row.tag("plan body")+"\n")
	require.Empty(t, a.captureRefusedBy)
	note, found, err := store.NoteBySlug(r.ctx, r.db, c.target, plans.NotePrefix+basename)
	require.NoError(t, err)
	require.True(t, found, "the plan lands in the home")
	require.Contains(t, r.fmFile(note.FilePath), row.tag("plan body"))
	for _, p := range []string{gmOpen, gmDocs, gmHome, ""} {
		_, found, err := store.NoteBySlug(r.ctx, r.db, p, plans.NotePrefix+basename)
		require.NoError(t, err)
		require.False(t, found, "and nowhere else: %q", p)
	}
	require.Equal(t, 1, r.count(`SELECT COUNT(*) FROM events WHERE kind = 'plan.captured' AND session_id = ? AND project_slug = ?`,
		row.home().ID, c.target), "filed under the home session")
	row.stampedLikeTheHome(t, core.ItemKindNote, note.ID)
}

// fmHarvest: the SessionEnd harvest becomes the home session's findings and
// is recorded nowhere else -- after a move the row it left keeps only its
// pointer.
func fmHarvest(t *testing.T, row *fmRow) {
	r, c := row.r, row.c
	said := row.tag("harvest")
	a := row.conv.end("prompt_input_exit", said)
	require.Empty(t, a.captureRefusedBy)
	home := row.home()
	require.Equal(t, core.SessionCompleted, home.Status)
	require.Equal(t, c.target, home.ProjectSlug)
	require.Equal(t, harvest.Prefix+said, home.Findings)
	r.fmOnlyIn(said, c.target)
	if c.moved {
		left, _, err := store.SessionByID(r.ctx, r.db, row.start.ID)
		require.NoError(t, err)
		require.Equal(t, store.MovePointer(c.target, home.Name), left.Findings, "the row it left keeps its pointer alone")
	}
	row.stampedLikeTheHome(t, core.ItemKindSession, home.ID)
}

// fmResume: a resumed conversation resumes in its home -- a move's child is
// never re-scoped to the repository's project -- and is briefed from it; a
// locked home whose lease ended with the session answers with the locked
// notice instead, the session recorded there and nowhere else.
func fmResume(t *testing.T, row *fmRow) {
	r, c := row.r, row.c
	a := row.conv.start("resume")
	home := row.home()
	require.Equal(t, c.target, home.ProjectSlug, "never re-scoped to the repository's project")
	if c.moved {
		require.Equal(t, row.start.ID, home.ParentSessionID, "the move's child resumes")
	}
	if c.locked() {
		require.Contains(t, a.context, "project "+gmFin+" is locked", a.context)
		require.NotContains(t, a.context, gmFin+" house rule")
		return
	}
	require.Equal(t, core.SessionActive, home.Status)
	require.Contains(t, a.context, c.target+" house rule", "briefed from its home")
	require.NotContains(t, a.context, gmOpen+" house rule")
	cli := row.conv.dial()
	mem := gmOK(t, cli, "memory_write", map[string]any{"name": "fm-resumed", "kind": "reference",
		"description": "d", "body": "b\n"})
	require.Equal(t, c.target, mem["project"])
	if c.sealed() {
		return
	}
	// The session's end ended its grant, never its label: the visited
	// project is written no more without a new approval, and what the home
	// writes still names it.
	text := fmRefused(t, cli, "tasks_add", map[string]any{"project": gmHome, "title": "fm after the end"})
	require.Contains(t, text, "\nRemedy: read-and-write request R-", text)
	require.NotEqual(t, row.grant, r.fmRequestOf(fmShortOf(t, text)), "a new request")
	row.stampedLikeTheHome(t, core.ItemKindMemory, mem["id"].(string))
}

// fmClear: /clear ends the conversation and starts the next one in the same
// agent process. Without peer_auth it is a new conversation, homed in the
// repository's project with nothing held; under peer_auth the label stays
// with the process tree, so a move's next conversation, back in the open
// repository, may not write there.
func fmClear(t *testing.T, row *fmRow) {
	r, c := row.r, row.c
	row.conv.end("clear", row.tag("before the clear"))
	next := r.fmConversation(row.conv.extID+"-next", row.origin)
	a := next.start("clear")
	sess := next.ambient()
	require.Equal(t, row.origin, sess.ProjectSlug, "the next conversation starts in its repository's project")
	cli := next.dial()
	switch {
	case c.locked() && !c.moved:
		require.Contains(t, a.context, "project "+gmFin+" is locked", "the lease ended with the session it was approved for")
	case !c.moved:
		require.Contains(t, a.context, c.target+" house rule")
		mem := gmOK(t, cli, "memory_write", map[string]any{"name": "fm-next", "kind": "reference", "description": "d", "body": "b\n"})
		require.Equal(t, c.target, mem["project"])
		// What the next conversation holds: without peer_auth its own home
		// alone; under peer_auth the process tree's label, which still holds
		// the project the write grant visited.
		held := []string{c.target}
		if c.peer && !c.sealed() {
			held = []string{c.target, gmHome}
			slices.Sort(held)
		}
		text := fmRefused(t, cli, "session_start", map[string]any{"project": gmDocs})
		require.True(t, strings.HasPrefix(text, "session_start: nothing was bound: this conversation holds content from "+
			strings.Join(held, ", ")+", so it does not work in project "+gmDocs+": "), text)
	case !c.peer:
		require.Contains(t, a.context, gmOpen+" house rule")
		mem := gmOK(t, cli, "memory_write", map[string]any{"name": "fm-next", "kind": "reference", "description": "d", "body": "b\n"})
		require.Equal(t, gmOpen, mem["project"], "a new conversation holds nothing")
		text := fmRefused(t, cli, "notes_read", map[string]any{"id": row.note})
		require.Contains(t, text, "\nRemedy: move request R-", "and reaches the target only by moving again")
		gmNoSecretsBut(t, text, "the next conversation's read")
	default:
		text := fmRefused(t, cli, "memory_write", map[string]any{"name": "fm-next", "kind": "reference", "description": "d", "body": "b\n"})
		require.Contains(t, text, "this agent holds content from ", text)
		require.Contains(t, text, c.target, text)
		require.Contains(t, text, "a write into project "+gmOpen+" is refused", text)
		r.noLanded(gmOpen, "after /clear", "fm-next", "fm next")
		// Its record may not land in its open home either: its end records
		// the not-recorded line, and the harvest is nowhere.
		said := row.tag("next harvest")
		end := next.end("prompt_input_exit", said)
		require.Contains(t, end.captureRefusedBy, "this conversation's findings", end.captureRefusedBy)
		sess := next.ambient()
		require.Equal(t, core.SessionCompleted, sess.Status)
		require.True(t, strings.HasPrefix(sess.Findings, "Not recorded: this agent holds content from "), sess.Findings)
		require.Contains(t, sess.Findings, "and its findings may not be written into project "+gmOpen+".")
		require.Zero(t, r.count(`SELECT COUNT(*) FROM sessions WHERE findings LIKE ?`, "%"+said+"%"))
		require.Zero(t, r.count(`SELECT COUNT(*) FROM events WHERE payload LIKE ?`, "%"+said+"%"))
	}
}
