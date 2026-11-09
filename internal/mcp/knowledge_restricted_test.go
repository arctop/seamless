package mcp_test

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/gardener"
	"github.com/arctop/seamless/internal/llm"
	mcpserver "github.com/arctop/seamless/internal/mcp"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/screening"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// Restricted local accounts at the knowledge tools (peer_auth.restricted): a
// restricted account's memories and notes carry its account label for as long
// as its words are the content; its writes of a briefing-injected kind are
// proposals held for the owner's promotion, screening on or off; and the
// owner's curation (stars, the gardener's queue) is refused to it. The owner's
// own writes answer exactly as before.

const (
	kaAccount = "_example_agent"
	kaOpen    = "example-open"
	kaConf    = "example-conf"
)

// kaProposalMessage is the contract's proposal text, transcribed so a drift in
// the server's copy fails here.
const kaProposalMessage = "Proposed: a restricted account's constraint, convention, protocol or stage waits for the " +
	"owner's promotion. Nothing in the corpus changed; the owner promotes or discards it (the console's Held " +
	"screen, or `seam promote <id>`). Do not retry or rephrase it to get past the proposal."

// kaChat is a gardener chat client that records each prompt and proposes
// nothing.
type kaChat struct {
	mu      sync.Mutex
	prompts []string
}

func (c *kaChat) Complete(_ context.Context, system, user string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prompts = append(c.prompts, system+"\n"+user)
	return `{"ops":[]}`, nil
}

func (c *kaChat) Model() string { return "example-chat" }

var _ llm.Chat = (*kaChat)(nil)

type kaOpts struct {
	screening   bool // the screening feature on
	noScreening bool // no screening service wired at all
}

// kaRig is one MCP server over a fresh store with peer_auth naming one
// restricted account, served three ways: the restricted account's socket view,
// the owner's TCP view, and no view at all (an install without peer_auth).
type kaRig struct {
	t        *testing.T
	ctx      context.Context
	db       *sql.DB
	mgr      *files.Manager
	dir      string
	scr      *screening.Service
	chat     *kaChat
	agent    *mcpclient.Client
	owner    *mcpclient.Client
	ownerURL string
	plain    *mcpclient.Client
	policy   *access.Policy
}

func newKARig(t *testing.T, o kaOpts) *kaRig {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mgr, err := files.NewManager(dir, db, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })
	for _, slug := range []string{kaOpen, kaConf} {
		_, err := store.EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
	}
	require.NoError(t, store.SetProjectIsolation(ctx, db, kaConf, core.IsolationConfidential))

	policy, err := access.New(config.PeerAuth{Enabled: true, Owner: "owner", Restricted: []string{kaAccount}},
		func(name string) (access.Account, error) {
			switch name {
			case "owner":
				return access.Account{UID: 501, Name: "owner", Home: "/Users/owner"}, nil
			case kaAccount:
				return access.Account{UID: 502, Name: kaAccount, Home: "/Users/" + kaAccount}, nil
			}
			return access.Account{}, fmt.Errorf("unknown account %q", name)
		})
	require.NoError(t, err)

	r := &kaRig{t: t, ctx: ctx, db: db, mgr: mgr, dir: dir, chat: &kaChat{}, policy: policy}
	rec := events.NewRecorder(db)
	ret := retrieve.New(db, nil, config.Budgets{MaxBriefingTokens: 1500, RecallBudgetTokens: 1000}, nil)
	ret.SetBodyReader(mgr.Store())
	garden := gardener.New(db, mgr, nil, r.chat, rec, gardener.Config{}, nil)
	cfg := mcpserver.Config{
		DB: db, Files: mgr, Retrieve: ret, Events: rec, Gardener: garden, APIKey: testKey,
		Features: config.Features{Research: true, Screening: o.screening},
	}
	if !o.noScreening {
		r.scr = screening.New(screening.Config{DB: db, Files: mgr, Events: rec, Gardener: garden})
		cfg.Screening = r.scr
	}
	ret.SetFeaturesConfig(cfg.Features)
	h := mcpserver.New(cfg).Handler()

	serve := func(view func() (access.View, bool)) string {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if view != nil {
				if v, ok := view(); ok {
					req = req.WithContext(access.WithView(req.Context(), v))
				}
			}
			h.ServeHTTP(w, req)
		}))
		t.Cleanup(ts.Close)
		return ts.URL
	}
	// The restricted view is built per request the way peerHandler builds it:
	// the kernel-named uid, then the open projects as the store has them now.
	agentURL := serve(func() (access.View, bool) {
		v, ok := policy.PeerView(502)
		require.True(t, ok)
		open, err := store.OpenProjectSlugs(context.Background(), db)
		require.NoError(t, err)
		return v.WithOpenProjects(open), true
	})
	ownerURL := serve(func() (access.View, bool) { return policy.OwnerView(), true })
	plainURL := serve(nil)
	r.agent = dialClient(t, ctx, agentURL, testKey)
	r.owner, r.ownerURL = dialClient(t, ctx, ownerURL, testKey), ownerURL
	r.plain = dialClient(t, ctx, plainURL, testKey)
	return r
}

func (r *kaRig) call(cli *mcpclient.Client, tool string, args map[string]any) map[string]any {
	r.t.Helper()
	return callJSON(r.t, r.ctx, cli, tool, args)
}

func (r *kaRig) refused(cli *mcpclient.Client, tool string, args map[string]any) string {
	r.t.Helper()
	isErr, text := callErr(r.t, r.ctx, cli, tool, args)
	require.True(r.t, isErr, "%s should be refused: %s", tool, text)
	return text
}

// write writes a memory into the open project through cli.
func (r *kaRig) write(cli *mcpclient.Client, name, kind, body string, extra map[string]any) map[string]any {
	r.t.Helper()
	args := map[string]any{"name": name, "kind": kind, "description": "about " + name, "body": body, "project": kaOpen}
	for k, v := range extra {
		args[k] = v
	}
	return r.call(cli, "memory_write", args)
}

// memory is the memory's index row and its file as parsed from disk.
func (r *kaRig) memory(name string) (core.Memory, core.Memory) {
	r.t.Helper()
	idx, ok, err := store.MemoryByNameIncludingInvalid(r.ctx, r.db, kaOpen, name)
	require.NoError(r.t, err)
	require.True(r.t, ok, "memory %s", name)
	disk, err := r.mgr.Store().ReadMemory(idx.FilePath)
	require.NoError(r.t, err)
	return idx, disk
}

// memAccount asserts the account label a memory carries in its index row and
// its file (the frontmatter key appears only when set).
func (r *kaRig) memAccount(name, want string) {
	r.t.Helper()
	idx, disk := r.memory(name)
	require.Equal(r.t, want, idx.Account, "%s: index account", name)
	require.Equal(r.t, want, disk.Account, "%s: file account", name)
	raw, err := os.ReadFile(filepath.Join(r.dir, filepath.FromSlash(idx.FilePath)))
	require.NoError(r.t, err)
	require.Equal(r.t, want != "", strings.Contains(string(raw), "\naccount: "), "%s: frontmatter key only when set", name)
}

func (r *kaRig) noteAccount(id, want string) {
	r.t.Helper()
	idx, ok, err := store.NoteByID(r.ctx, r.db, id)
	require.NoError(r.t, err)
	require.True(r.t, ok)
	disk, err := r.mgr.Store().ReadNote(idx.FilePath)
	require.NoError(r.t, err)
	require.Equal(r.t, want, idx.Account, "note %s: index account", id)
	require.Equal(r.t, want, disk.Account, "note %s: file account", id)
}

// corpus snapshots the knowledge corpus: every file under memory/ and notes/,
// and every row of both index tables.
func (r *kaRig) corpus() map[string]string {
	r.t.Helper()
	out := map[string]string{}
	for _, tree := range []string{"memory", "notes"} {
		root := filepath.Join(r.dir, tree)
		if _, err := os.Stat(root); os.IsNotExist(err) {
			continue
		}
		require.NoError(r.t, filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			rel, _ := filepath.Rel(r.dir, p)
			out["file:"+rel] = string(b)
			return nil
		}))
	}
	for _, table := range []string{"memories_index", "notes_index"} {
		for i, row := range r.rows(table) {
			out[fmt.Sprintf("row:%s:%d", table, i)] = row
		}
	}
	return out
}

// rows renders every row of an index table, ordered by id.
func (r *kaRig) rows(table string) []string {
	r.t.Helper()
	rows, err := r.db.Query(`SELECT * FROM ` + table + ` ORDER BY id`)
	require.NoError(r.t, err)
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	require.NoError(r.t, err)
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		require.NoError(r.t, rows.Scan(ptrs...))
		out = append(out, fmt.Sprint(vals...))
	}
	require.NoError(r.t, rows.Err())
	return out
}

// requireProposed asserts a proposal answer and the one pending hold behind
// it, and returns that hold.
func (r *kaRig) requireProposed(res map[string]any, tool string) store.HeldWrite {
	r.t.Helper()
	require.Equal(r.t, "proposed", res["status"], "%v", res)
	require.Equal(r.t, kaAccount, res["proposed_by"])
	require.Equal(r.t, kaProposalMessage, res["message"])
	hold := res["hold"].(map[string]any)
	reasons := hold["reasons"].([]any)
	require.Equal(r.t, string(screening.ReasonRestrictedAccount), reasons[0], "the proposal reason leads")
	held := pendingHolds(r.t, r.db)
	require.Len(r.t, held, 1)
	h := held[0]
	require.Equal(r.t, hold["id"], h.ID)
	require.Equal(r.t, tool, h.Tool)
	require.True(r.t, screening.IsProposal(h.Reasons))
	require.Equal(r.t, kaAccount, h.DetailValue(screening.DetailAccount))
	require.Equal(r.t, kaOpen, h.Project)
	return h
}

// A restricted account's notes and non-briefing memories land as always,
// carrying its label in the file, the index and the read tools' output.
func TestRestricted_NonBriefingWritesLandLabeled(t *testing.T) {
	r := newKARig(t, kaOpts{})
	for _, kind := range core.MemoryKinds {
		if screening.IsBriefingKind(kind) {
			continue
		}
		name := "agent-" + string(kind)
		w := r.write(r.agent, name, string(kind), "Something the agent learned.\n", nil)
		require.NotContains(t, w, "status", "%s lands: no held or proposed marker", kind)
		r.memAccount(name, kaAccount)
		read := r.call(r.agent, "memory_read", map[string]any{"name": name, "project": kaOpen})
		require.Equal(t, kaAccount, read["account"])
	}
	n := r.call(r.agent, "notes_create", map[string]any{"title": "Agent findings", "body": "Found it.\n", "project": kaOpen})
	id := n["id"].(string)
	r.noteAccount(id, kaAccount)
	require.Equal(t, kaAccount, r.call(r.owner, "notes_read", map[string]any{"id": id})["account"])
	require.Empty(t, pendingHolds(t, r.db))
	require.Empty(t, eventsOf(t, r.db, core.EventMemoryHeld))
}

// The label follows the content like Model attribution does: a body change
// re-stamps it with the writer (the owner's clears it), a restricted
// account's new description or title stamps it, and metadata -- tags, a
// star, a supersede tombstone, an owner's description fix -- keeps it.
func TestRestricted_LabelFollowsContent(t *testing.T) {
	r := newKARig(t, kaOpts{})
	edit := func(cli *mcpclient.Client, args map[string]any) {
		t.Helper()
		args["name"], args["project"] = "lesson", kaOpen
		r.call(cli, "memory_edit", args)
	}
	r.write(r.owner, "lesson", "gotcha", "Line one.\n", nil)
	r.memAccount("lesson", "")

	edit(r.agent, map[string]any{"tags_add": []any{"x"}})
	r.memAccount("lesson", "") // tags only
	edit(r.agent, map[string]any{"description": "the agent's line"})
	r.memAccount("lesson", kaAccount) // its text now leads every index line
	edit(r.owner, map[string]any{"tags_remove": []any{"x"}})
	r.memAccount("lesson", kaAccount)
	edit(r.owner, map[string]any{"description": "the owner's fix"})
	r.memAccount("lesson", kaAccount) // the owner's description fix keeps it
	edit(r.owner, map[string]any{"edits": []any{map[string]any{"old_string": "Line one.", "new_string": "Line 1."}}})
	r.memAccount("lesson", "") // the owner's body edit clears it
	edit(r.agent, map[string]any{"edits": []any{map[string]any{"old_string": "Line 1.", "new_string": "Line one."}}})
	r.memAccount("lesson", kaAccount)

	r.call(r.owner, "memory_append", map[string]any{"name": "lesson", "project": kaOpen, "body": "Owner adds.\n"})
	r.memAccount("lesson", "")
	r.call(r.agent, "memory_append", map[string]any{"name": "lesson", "project": kaOpen, "body": "Agent adds.\n"})
	r.memAccount("lesson", kaAccount)
	r.write(r.owner, "lesson", "gotcha", "Owner rewrite.\n", nil)
	r.memAccount("lesson", "")
	r.write(r.agent, "lesson", "gotcha", "Agent rewrite.\n", nil)
	r.memAccount("lesson", kaAccount)

	r.call(r.owner, "favorite_set", map[string]any{"kind": "memory", "id": "lesson", "project": kaOpen, "favorite": true})
	idx, _ := r.memory("lesson")
	require.True(t, idx.Favorite)
	r.memAccount("lesson", kaAccount)
	r.write(r.owner, "lesson-v2", "gotcha", "The owner's replacement.\n", map[string]any{"supersedes": "lesson"})
	idx, _ = r.memory("lesson")
	require.False(t, idx.Active())
	r.memAccount("lesson", kaAccount) // the tombstone is metadata
	r.memAccount("lesson-v2", "")

	// Notes, the same rule.
	n := r.call(r.owner, "notes_create", map[string]any{"title": "Shared log", "body": "Start.\n", "project": kaOpen})
	id := n["id"].(string)
	r.noteAccount(id, "")
	r.call(r.agent, "notes_update", map[string]any{"id": id, "tags_add": []any{"y"}})
	r.noteAccount(id, "")
	r.call(r.agent, "notes_update", map[string]any{"id": id, "title": "Shared log, retitled"})
	r.noteAccount(id, kaAccount)
	r.call(r.owner, "notes_update", map[string]any{"id": id, "description": "the owner's summary"})
	r.noteAccount(id, kaAccount)
	r.call(r.owner, "notes_update", map[string]any{"id": id, "body": "Owner body.\n"})
	r.noteAccount(id, "")
	r.call(r.agent, "notes_append", map[string]any{"id": id, "body": "agent line"})
	r.noteAccount(id, kaAccount)
	r.call(r.owner, "notes_edit", map[string]any{"id": id, "edits": []any{map[string]any{"old_string": "Owner body.", "new_string": "Owner's body."}}})
	r.noteAccount(id, "")
	r.call(r.agent, "notes_edit", map[string]any{"id": id, "edits": []any{map[string]any{"old_string": "Owner's body.", "new_string": "Owner body."}}})
	r.noteAccount(id, kaAccount)
	r.call(r.owner, "notes_update", map[string]any{"id": id, "body": "Owner again.\n"})
	r.noteAccount(id, "")
	r.call(r.agent, "notes_update", map[string]any{"id": id, "body": "Agent body.\n"})
	r.noteAccount(id, kaAccount)
	r.call(r.owner, "favorite_set", map[string]any{"kind": "note", "id": id, "favorite": true})
	r.noteAccount(id, kaAccount)
}

// Every way a restricted account can land, replace, supersede or delete a
// briefing-injected kind is a proposal -- with the screening feature OFF --
// that leaves the corpus (files and index) byte-identical and files one
// pending hold carrying the reason and the account.
func TestRestricted_ProposalTriggers(t *testing.T) {
	appendTo := func(name string) (string, map[string]any) {
		return "memory_append", map[string]any{"name": name, "project": kaOpen, "body": "One more rule.\n"}
	}
	for _, tc := range []struct {
		name     string
		seed     func(r *kaRig) // the owner's corpus before the call
		call     func(r *kaRig) (string, map[string]any)
		op       string
		kind     core.MemoryKind
		metadata bool // a metadata-only edit: the held content keeps the owner's (empty) label
		check    func(t *testing.T, h store.HeldWrite)
	}{
		{name: "write a constraint", op: store.HeldOpWrite, kind: core.KindConstraint,
			call: func(r *kaRig) (string, map[string]any) {
				return "memory_write", map[string]any{"name": "new-rule", "kind": "constraint", "description": "a rule",
					"body": "Never do X.\n", "project": kaOpen}
			}},
		{name: "write a convention", op: store.HeldOpWrite, kind: core.KindConvention,
			call: func(r *kaRig) (string, map[string]any) {
				return "memory_write", map[string]any{"name": "new-conv", "kind": "convention", "description": "a layout",
					"body": "Docs live in docs/.\n", "project": kaOpen}
			}},
		{name: "write a protocol", op: store.HeldOpWrite, kind: core.KindProtocol,
			call: func(r *kaRig) (string, map[string]any) {
				return "memory_write", map[string]any{"name": "new-proto", "kind": "protocol", "description": "a handoff",
					"body": "Hand off by note.\n", "project": kaOpen}
			}},
		{name: "write a stage", op: store.HeldOpWrite, kind: core.KindStage,
			call: func(r *kaRig) (string, map[string]any) {
				return "memory_write", map[string]any{"name": "new-stage", "kind": "stage", "description": "a gate",
					"body": "Status: open\n\nWaiting on review.\n", "project": kaOpen}
			}},
		{name: "overwrite an owner constraint with a gotcha", op: store.HeldOpWrite, kind: core.KindGotcha,
			seed: func(r *kaRig) { r.write(r.owner, "owner-rule", "constraint", "Always do Y.\n", nil) },
			call: func(r *kaRig) (string, map[string]any) {
				return "memory_write", map[string]any{"name": "owner-rule", "kind": "gotcha", "description": "demoted",
					"body": "Y is optional.\n", "project": kaOpen}
			},
			check: func(t *testing.T, h store.HeldWrite) {
				require.NotEmpty(t, h.BaseHash, "an in-place write names its base")
			}},
		{name: "supersede an owner constraint", op: store.HeldOpWrite, kind: core.KindGotcha,
			seed: func(r *kaRig) { r.write(r.owner, "owner-rule", "constraint", "Always do Y.\n", nil) },
			call: func(r *kaRig) (string, map[string]any) {
				return "memory_write", map[string]any{"name": "replacement", "kind": "gotcha", "description": "softer",
					"body": "Y is optional.\n", "project": kaOpen, "supersedes": "owner-rule"}
			},
			check: func(t *testing.T, h store.HeldWrite) {
				require.Equal(t, "owner-rule", h.DetailValue("supersedes_name"))
				require.Empty(t, h.BaseHash, "a create has no base")
			}},
		{name: "append to an owner constraint", op: store.HeldOpWrite, kind: core.KindConstraint,
			seed: func(r *kaRig) { r.write(r.owner, "owner-rule", "constraint", "Always do Y.\n", nil) },
			call: func(*kaRig) (string, map[string]any) { return appendTo("owner-rule") }},
		{name: "edit an owner constraint", op: store.HeldOpWrite, kind: core.KindConstraint,
			seed: func(r *kaRig) { r.write(r.owner, "owner-rule", "constraint", "Always do Y.\n", nil) },
			call: func(*kaRig) (string, map[string]any) {
				return "memory_edit", map[string]any{"name": "owner-rule", "project": kaOpen,
					"edits": []any{map[string]any{"old_string": "Always", "new_string": "Sometimes"}}}
			}},
		{name: "retag an owner stage", op: store.HeldOpWrite, kind: core.KindStage, metadata: true,
			seed: func(r *kaRig) { r.write(r.owner, "owner-stage", "stage", "Status: open\n\nWaiting.\n", nil) },
			call: func(*kaRig) (string, map[string]any) {
				return "memory_edit", map[string]any{"name": "owner-stage", "project": kaOpen, "tags_add": []any{"z"}}
			}},
		{name: "delete an owner constraint", op: store.HeldOpDelete, kind: core.KindConstraint,
			seed: func(r *kaRig) { r.write(r.owner, "owner-rule", "constraint", "Always do Y.\n", nil) },
			call: func(*kaRig) (string, map[string]any) {
				return "memory_delete", map[string]any{"name": "owner-rule", "project": kaOpen}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newKARig(t, kaOpts{})
			if tc.seed != nil {
				tc.seed(r)
			}
			before := r.corpus()
			tool, args := tc.call(r)
			res := r.call(r.agent, tool, args)
			h := r.requireProposed(res, tool)
			require.Equal(t, tc.op, h.Op)
			require.Equal(t, tc.kind, h.MemoryKind)
			require.Equal(t, []string{string(screening.ReasonRestrictedAccount)}, h.Reasons, "screening off: the proposal reason alone")
			if h.Op == store.HeldOpWrite {
				mem, err := files.ParseMemory(h.Content, h.DetailValue("path"))
				require.NoError(t, err)
				want := kaAccount
				if tc.metadata {
					want = ""
				}
				require.Equal(t, want, mem.Account, "the held content carries the label it lands with")
			}
			if tc.check != nil {
				tc.check(t, h)
			}
			require.Equal(t, before, r.corpus(), "a proposal changes nothing in the corpus")
			require.Len(t, eventsOf(t, r.db, core.EventMemoryHeld), 1)
			require.Empty(t, eventsOf(t, r.db, core.EventMemoryScreened), "screening off: nothing is screened")
		})
	}
}

// With the screening feature also on, a write both would hold is ONE hold
// carrying both reason sets, still answered as a proposal; a clean
// briefing-kind write is a proposal on the account reason alone; and a
// non-briefing write screening holds stays a plain screening hold.
func TestRestricted_ProposalFoldsWithScreening(t *testing.T) {
	r := newKARig(t, kaOpts{screening: true})
	before := r.corpus()
	res := r.write(r.agent, "risky-rule", "constraint", injected+"\n", nil)
	h := r.requireProposed(res, "memory_write")
	require.Equal(t, []string{string(screening.ReasonRestrictedAccount), "instruction.override"}, h.Reasons)
	require.Equal(t, before, r.corpus())
	sc := eventsOf(t, r.db, core.EventMemoryScreened)
	require.Len(t, sc, 1)
	require.Equal(t, true, sc[0]["held"])
	require.Len(t, eventsOf(t, r.db, core.EventMemoryHeld), 1)
	_, err := r.scr.Discard(r.ctx, h.ID)
	require.NoError(t, err)

	res = r.write(r.agent, "clean-rule", "constraint", "Run the checks first.\n", nil)
	h = r.requireProposed(res, "memory_write")
	require.Equal(t, []string{string(screening.ReasonRestrictedAccount)}, h.Reasons)
	_, err = r.scr.Discard(r.ctx, h.ID)
	require.NoError(t, err)

	res = r.write(r.agent, "risky-gotcha", "gotcha", injected+"\n", nil)
	require.Equal(t, "held", res["status"], "a screening hold of a non-briefing kind is not a proposal")
	require.NotContains(t, res, "proposed_by")
	held := pendingHolds(t, r.db)
	require.Len(t, held, 1)
	require.False(t, screening.IsProposal(held[0].Reasons))
}

// A server built without the held queue cannot keep a proposal, so a
// would-be proposal is refused -- never written -- while everything else
// still lands.
func TestRestricted_NoHeldQueueRefusesProposals(t *testing.T) {
	r := newKARig(t, kaOpts{noScreening: true})
	r.write(r.owner, "owner-rule", "constraint", "Always do Y.\n", nil)
	before := r.corpus()
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"memory_write", map[string]any{"name": "new-rule", "kind": "constraint", "description": "a rule", "body": "X.\n", "project": kaOpen}},
		{"memory_write", map[string]any{"name": "owner-rule", "kind": "gotcha", "description": "d", "body": "X.\n", "project": kaOpen}},
		{"memory_write", map[string]any{"name": "other", "kind": "gotcha", "description": "d", "body": "X.\n", "project": kaOpen, "supersedes": "owner-rule"}},
		{"memory_append", map[string]any{"name": "owner-rule", "body": "X.\n", "project": kaOpen}},
		{"memory_edit", map[string]any{"name": "owner-rule", "project": kaOpen, "tags_add": []any{"x"}}},
		{"memory_delete", map[string]any{"name": "owner-rule", "project": kaOpen}},
	} {
		text := r.refused(r.agent, c.tool, c.args)
		require.Contains(t, text, "no held queue", "%s", c.tool)
		require.Contains(t, text, "nothing was written", "%s", c.tool)
	}
	require.Equal(t, before, r.corpus())
	r.write(r.agent, "agent-gotcha", "gotcha", "Fine.\n", nil)
	r.memAccount("agent-gotcha", kaAccount)
}

// The owner promotes a proposal with the held queue's Release: it lands
// exactly the proposed write, label included, superseding what it proposed
// to supersede; an append or edit of the owner's constraint shows only after
// it; and the account's later change to its own promoted constraint is again
// a proposal.
func TestRestricted_PromotionLandsTheProposal(t *testing.T) {
	r := newKARig(t, kaOpts{})
	r.write(r.owner, "owner-rule", "constraint", "Always do Y.\n", nil)

	res := r.write(r.agent, "agent-rule", "constraint", "Do Y, then Z.\n", map[string]any{"supersedes": "owner-rule"})
	h := r.requireProposed(res, "memory_write")
	isErr, _ := callErr(t, r.ctx, r.agent, "memory_read", map[string]any{"name": "agent-rule", "project": kaOpen})
	require.True(t, isErr, "a proposal is served to no one before its promotion")
	rel, err := r.scr.Release(r.ctx, h.ID)
	require.NoError(t, err)
	require.Equal(t, res["id"], rel.ItemID, "the promotion lands the id the agent was given")
	require.Equal(t, "example-open/owner-rule", rel.Superseded)
	r.memAccount("agent-rule", kaAccount)
	read := r.call(r.owner, "memory_read", map[string]any{"name": "agent-rule", "project": kaOpen})
	require.Equal(t, kaAccount, read["account"])
	require.Equal(t, "Do Y, then Z.\n", read["body"])
	old, _ := r.memory("owner-rule")
	require.False(t, old.Active())
	require.Equal(t, rel.ItemID, old.SupersededBy)
	r.memAccount("owner-rule", "")

	// A proposed append and edit of an owner constraint.
	r.write(r.owner, "owner-rule-2", "constraint", "Keep it small.\n", nil)
	ownerIdx, ownerDisk := r.memory("owner-rule-2")
	res = r.call(r.agent, "memory_append", map[string]any{"name": "owner-rule-2", "project": kaOpen, "body": "And tidy."})
	h = r.requireProposed(res, "memory_append")
	idx, disk := r.memory("owner-rule-2")
	require.Equal(t, ownerIdx.ContentHash, idx.ContentHash, "the index is unchanged until the promotion")
	require.Equal(t, ownerDisk.Body, disk.Body, "the file is unchanged until the promotion")
	_, err = r.scr.Release(r.ctx, h.ID)
	require.NoError(t, err)
	_, disk = r.memory("owner-rule-2")
	require.Equal(t, "Keep it small.\nAnd tidy.\n", disk.Body)
	r.memAccount("owner-rule-2", kaAccount)

	r.write(r.owner, "owner-rule-3", "constraint", "Ship weekly.\n", nil)
	ownerIdx, _ = r.memory("owner-rule-3")
	res = r.call(r.agent, "memory_edit", map[string]any{"name": "owner-rule-3", "project": kaOpen,
		"edits": []any{map[string]any{"old_string": "weekly", "new_string": "daily"}}})
	h = r.requireProposed(res, "memory_edit")
	idx, disk = r.memory("owner-rule-3")
	require.Equal(t, ownerIdx.ContentHash, idx.ContentHash)
	require.Equal(t, "Ship weekly.\n", disk.Body)
	_, err = r.scr.Release(r.ctx, h.ID)
	require.NoError(t, err)
	_, disk = r.memory("owner-rule-3")
	require.Equal(t, "Ship daily.\n", disk.Body)
	r.memAccount("owner-rule-3", kaAccount)

	// Its own promoted constraint is still binding text: a change is a proposal.
	before := r.corpus()
	res = r.call(r.agent, "memory_append", map[string]any{"name": "agent-rule", "project": kaOpen, "body": "Also W.\n"})
	h = r.requireProposed(res, "memory_append")
	require.Equal(t, before, r.corpus())
	_, err = r.scr.Discard(r.ctx, h.ID)
	require.NoError(t, err)
	res = r.write(r.agent, "agent-rule", "constraint", "Rewritten.\n", nil)
	h = r.requireProposed(res, "memory_write")
	require.Equal(t, before, r.corpus())
	_, err = r.scr.Discard(r.ctx, h.ID)
	require.NoError(t, err)

	// A proposed delete removes the memory only when promoted.
	res = r.call(r.agent, "memory_delete", map[string]any{"name": "owner-rule-3", "project": kaOpen})
	h = r.requireProposed(res, "memory_delete")
	r.memory("owner-rule-3")
	_, err = r.scr.Release(r.ctx, h.ID)
	require.NoError(t, err)
	_, found, err := store.MemoryByNameIncludingInvalid(r.ctx, r.db, kaOpen, "owner-rule-3")
	require.NoError(t, err)
	require.False(t, found)
}

// The owner's writes are untouched by all of it: every kind lands with no
// hold, no label and the same response keys, through the owner's view and
// with no view at all (peer_auth off).
func TestRestricted_OwnerWritesUnchanged(t *testing.T) {
	r := newKARig(t, kaOpts{})
	keys := func(m map[string]any) []string {
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		slices.Sort(out)
		return out
	}
	for _, who := range []struct {
		name, prefix string
		cli          *mcpclient.Client
	}{{"owner", "own", r.owner}, {"peer_auth off", "off", r.plain}} {
		for _, kind := range core.MemoryKinds {
			name := who.prefix + "-" + string(kind)
			body := "Owner knowledge.\n"
			if kind == core.KindStage {
				body = "Status: open\n\nWaiting.\n"
			}
			w := r.write(who.cli, name, string(kind), body, nil)
			require.Equal(t, []string{"id", "name", "project", "updated"}, keys(w), "%s %s", who.name, kind)
			read := r.call(who.cli, "memory_read", map[string]any{"name": name, "project": kaOpen})
			require.Equal(t, []string{"body", "content_hash", "description", "id", "kind", "name", "project",
				"source_session", "tags"}, keys(read), "%s %s", who.name, kind)
			r.memAccount(name, "")
			a := r.call(who.cli, "memory_append", map[string]any{"name": name, "project": kaOpen, "body": "More.\n"})
			require.Equal(t, []string{"id", "name", "status"}, keys(a))
			e := r.call(who.cli, "memory_edit", map[string]any{"name": name, "project": kaOpen,
				"edits": []any{map[string]any{"old_string": "More.", "new_string": "Even more."}}})
			require.Equal(t, []string{"content_hash", "diff", "id", "name", "project"}, keys(e))
			r.memAccount(name, "")
			d := r.call(who.cli, "memory_delete", map[string]any{"name": name, "project": kaOpen})
			require.Equal(t, "deleted", d["status"])
		}
		n := r.call(who.cli, "notes_create", map[string]any{"title": who.name + " note", "body": "B.\n", "project": kaOpen})
		require.Equal(t, []string{"id", "project", "slug", "title"}, keys(n))
		read := r.call(who.cli, "notes_read", map[string]any{"id": n["id"]})
		require.NotContains(t, read, "account")
		r.noteAccount(n["id"].(string), "")
		f := r.call(who.cli, "favorite_set", map[string]any{"kind": "note", "id": n["id"], "favorite": true})
		require.Equal(t, true, f["favorite"])
	}
	require.Empty(t, pendingHolds(t, r.db))
	require.Empty(t, eventsOf(t, r.db, core.EventMemoryHeld))
}

// Stars and the gardener's queue are the owner's curation: refused to a
// restricted account before anything is resolved, unchanged for the owner.
func TestRestricted_CurationRefused(t *testing.T) {
	r := newKARig(t, kaOpts{})
	r.write(r.owner, "owner-gotcha", "gotcha", "Watch out.\n", nil)
	idx, _ := r.memory("owner-gotcha")
	p, err := store.CreateProposal(r.ctx, r.db, store.ProposalArchive, map[string]any{
		"id": idx.ID, "name": idx.Name, "project": kaOpen, "reason": "stale",
	})
	require.NoError(t, err)

	before := r.corpus()
	for _, kind := range []string{"memory", "note", "project"} {
		text := r.refused(r.agent, "favorite_set", map[string]any{"kind": kind, "id": "owner-gotcha", "favorite": true, "project": kaOpen})
		require.Contains(t, text, "not open to account "+kaAccount)
		require.Contains(t, text, "the owner sets it from the console")
	}
	for _, action := range []string{"apply", "dismiss", "hide"} {
		text := r.refused(r.agent, "gardener_apply", map[string]any{"id": p.ID, "action": action})
		require.Contains(t, text, "not open to account "+kaAccount)
		require.Contains(t, text, "Gardener screen")
	}
	// An id that names nothing gets the same answer: the refusal comes first.
	require.Contains(t, r.refused(r.agent, "gardener_apply", map[string]any{"id": "01K000000000000000000NOPE0"}),
		"not open to account")
	require.Equal(t, before, r.corpus())
	still, ok, err := store.ProposalByID(r.ctx, r.db, p.ID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, store.ProposalPending, still.Status)

	// The owner: both work as before.
	r.call(r.owner, "favorite_set", map[string]any{"kind": "memory", "id": "owner-gotcha", "project": kaOpen, "favorite": true})
	idx, _ = r.memory("owner-gotcha")
	require.True(t, idx.Favorite)
	out := r.call(r.owner, "gardener_apply", map[string]any{"id": p.ID, "action": "dismiss"})
	require.Equal(t, store.ProposalDismissed, out["status"])
	require.Contains(t, r.refused(r.owner, "gardener_apply", map[string]any{"id": "01K000000000000000000NOPE0"}), "no proposal with id")
}

// The gardener tool a restricted account keeps, the read, stays inside its
// reach: a confidential project's proposals are not listed for it. The two
// planners are the owner's curation, refused before anything is looked up: one
// text whichever project is named, and no memory reaches the chat client.
func TestRestricted_GardenerReadsStayFenced(t *testing.T) {
	r := newKARig(t, kaOpts{})
	seed := func(project, name string) core.Memory {
		id, err := core.NewID()
		require.NoError(t, err)
		now := time.Now().UTC()
		m, err := r.mgr.WriteMemory(r.ctx, core.Memory{ID: id, Kind: core.KindGotcha, Name: name, Project: project,
			Description: "the " + name + " line", Body: "Body of " + name + ".\n",
			Created: now, Updated: now, ValidFrom: now})
		require.NoError(t, err)
		return m
	}
	openMem := seed(kaOpen, "open-gotcha")
	confMem := seed(kaConf, "conf-secret-gotcha")
	for _, m := range []core.Memory{openMem, confMem} {
		_, err := store.CreateProposal(r.ctx, r.db, store.ProposalArchive, map[string]any{
			"id": m.ID, "name": m.Name, "project": m.Project, "reason": "stale",
		})
		require.NoError(t, err)
	}

	listed := r.call(r.agent, "gardener_proposals", map[string]any{})
	require.EqualValues(t, 1, listed["count"])
	require.Contains(t, mustJSON(t, listed), "open-gotcha")
	require.NotContains(t, mustJSON(t, listed), "conf-secret-gotcha")

	for tool, argss := range map[string][]map[string]any{
		"gardener_request": {
			{"request": "archive stale ones", "project": kaOpen},
			{"request": "archive stale ones", "project": kaConf},
			{"request": "archive stale ones", "project": "example-nowhere"},
			{"request": "archive stale ones", "project": "all"},
		},
		"gardener_split": {{"source": kaOpen}, {"source": kaConf}, {"source": "example-nowhere"}},
	} {
		first := r.refused(r.agent, tool, argss[0])
		require.Contains(t, first, "not open to account "+kaAccount, tool)
		require.Contains(t, first, "gardener_proposals", tool)
		require.Contains(t, first, "Gardener screen", tool)
		for _, args := range argss[1:] {
			require.Equal(t, first, r.refused(r.agent, tool, args), "%s %v answers alike", tool, args)
		}
	}
	r.chat.mu.Lock()
	prompts := slices.Clone(r.chat.prompts)
	r.chat.mu.Unlock()
	require.Empty(t, prompts, "a refused planner reads no memory")

	// The owner, bound to the confidential project, reads both proposals: what
	// kept one from the account above is its reach, not the queue. (Last, so no
	// owner session exists while the account's calls run.)
	bound := dialClient(t, r.ctx, r.ownerURL, testKey)
	r.call(bound, "session_start", map[string]any{"project": kaConf})
	require.EqualValues(t, 2, r.call(bound, "gardener_proposals", map[string]any{})["count"])
}
