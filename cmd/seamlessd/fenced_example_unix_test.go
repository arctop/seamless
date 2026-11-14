//go:build unix

package main

// The owner's example (plan fenced-sessions P12; the narrative's "whole
// carry"), end to end on a throwaway daemon: this test binary re-executed as a
// real `seamlessd serve` (TestFencedExampleHelperDaemon) on a data dir, a free
// port and a config of its own -- isolation.requests on, update.check false,
// backups off, no LLM key -- in an environment built from scratch, so neither
// the developer's SEAMLESS_* nor ~/.seamless, nor port 8081, is ever touched.
// Without peer_auth (a scratch daemon cannot run one as the owner): the
// advisory tier, where the bearer key approves over the console as `seam
// approve` does.
//
// A conversation homed in a confidential data room (example-dataroom) reads
// a confidential finance project (example-finance), gets a write grant into
// it, then reads a third confidential project (example-legal). Each
// approval's facts state the whole carry -- the third says legal's content
// may end up in the data room and in finance -- what it writes is stamped
// with what it holds, and every record it leaves (findings, the harvest, a
// plan and a subagent's report) lands in the data room, even with its hooks
// firing from the finance repository.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/harvest"
	"github.com/arctop/seamless/internal/mcp"
	"github.com/arctop/seamless/internal/plans"
	"github.com/arctop/seamless/internal/store"
)

const (
	fmDataRoom = "example-dataroom"
	fmFinance  = "example-finance"
	fmLegal    = "example-legal"

	// fmExampleDaemonEnv makes this binary the example's daemon.
	fmExampleDaemonEnv = "FENCED_EXAMPLE_DAEMON"
	fmExampleKey       = "fenced-example-key-0123456789"
)

// fmExampleAgent is the example's Claude Code process.
var fmExampleAgent = access.ProcessToken{PID: 4100, Version: 41000, Boot: "8d3c1f0e-55aa-4c1e-9d0b-6f7e8a9b0c1d"}

// TestFencedExampleHelperDaemon is not a test: TestFencedExample_DataRoom runs
// this binary with FENCED_EXAMPLE_DAEMON=1 as its throwaway daemon, which is
// serve itself, on the config SEAMLESS_CONFIG names. It stops on SIGTERM, as
// serve does, or after three minutes regardless, so a failed test cannot leave
// it behind for long.
func TestFencedExampleHelperDaemon(t *testing.T) {
	if os.Getenv(fmExampleDaemonEnv) != "1" {
		t.Skip("the throwaway daemon of TestFencedExample_DataRoom; not a test of its own")
	}
	errc := make(chan error, 1)
	go func() { errc <- runServe([]string{"--config", os.Getenv("SEAMLESS_CONFIG")}) }()
	select {
	case err := <-errc:
		if err != nil {
			fmt.Fprintln(os.Stderr, "serve:", err)
			os.Exit(2)
		}
	case <-time.After(3 * time.Minute):
		os.Exit(3)
	}
}

// fmExample is the throwaway daemon as the agent and the owner reach it.
type fmExample struct {
	t                *testing.T
	ctx              context.Context
	url, host        string
	home, data, logf string
	files            string // the agent's own files: its transcripts
	db               *sql.DB
	agentID          string
	extID            string
}

// fmStartExample seeds the throwaway daemon's data dir -- the three
// confidential projects, each mapped to a repository -- writes its config and
// starts it, returning once /healthz answers.
func fmStartExample(t *testing.T) *fmExample {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	e := &fmExample{t: t, ctx: ctx, home: filepath.Join(root, "home"), data: filepath.Join(root, "data"),
		logf: filepath.Join(root, "daemon.log"), files: filepath.Join(root, "agent"), host: config.Hostname(),
		agentID: fmExampleAgent.AgentID(), extID: "6f1c0d2e-3b4a-4c5d-8e9f-a0b1c2d3e4f5"}
	require.NoError(t, os.MkdirAll(e.home, 0o700))
	require.NoError(t, os.MkdirAll(e.files, 0o700))

	seed, err := store.Open(filepath.Join(e.data, "seam.db"))
	require.NoError(t, err)
	for _, p := range []string{fmDataRoom, fmFinance, fmLegal} {
		_, err := store.EnsureProject(ctx, seed, p, p)
		require.NoError(t, err)
		require.NoError(t, store.SetProjectIsolation(ctx, seed, p, core.IsolationConfidential))
		require.NoError(t, store.AddRepoMapping(ctx, seed, "/work/"+p, p))
	}
	require.NoError(t, seed.Close())

	// Free on both loopback addresses: the daemon listens on [::1] too, and
	// refuses to start where another process holds it.
	port, release := frelayDualLoopbackPort(t)
	require.NotEqual(t, 8081, port, "never the live daemon's port")
	e.url = fmt.Sprintf("http://127.0.0.1:%d", port)
	cfgPath := filepath.Join(root, "seamless.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(fmt.Sprintf("addr: '127.0.0.1:%d'\ndata_dir: '%s'\n"+
		"mcp:\n  api_key: '%s'\nisolation:\n  requests: true\nupdate:\n  check: false\nbackup:\n  auto: false\n",
		port, e.data, fmExampleKey)), 0o600))

	logFile, err := os.Create(e.logf)
	require.NoError(t, err)
	cmd := exec.Command(os.Args[0], "-test.run=^TestFencedExampleHelperDaemon$", "-test.timeout=5m") //nolint:gosec // the test binary
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + e.home, "TMPDIR=" + root, "LANG=C",
		"SEAMLESS_CONFIG=" + cfgPath, fmExampleDaemonEnv + "=1"}
	cmd.Stdout, cmd.Stderr = logFile, logFile
	release() // the daemon binds both halves itself
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		_ = logFile.Close()
		if t.Failed() {
			if raw, err := os.ReadFile(e.logf); err == nil {
				t.Logf("the throwaway daemon's log:\n%s", raw)
			}
		}
	})
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(30 * time.Second)
	for up := false; !up; {
		if resp, err := http.Get(e.url + "/healthz"); err == nil { //nolint:gosec,noctx // the throwaway daemon, polled until up
			up = resp.StatusCode == http.StatusOK
			_ = resp.Body.Close()
		}
		if up {
			break
		}
		select {
		case <-tick.C:
		case <-deadline:
			raw, _ := os.ReadFile(e.logf) //nolint:errcheck // best-effort diagnostics for the failure below
			t.Fatalf("the throwaway daemon did not come up:\n%s", raw)
		}
	}
	e.db, err = store.OpenExisting(filepath.Join(e.data, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = e.db.Close() })
	return e
}

// owner is an MCP connection as a project's own agent session would make it
// (no agent process named: bound by what it names).
func (e *fmExample) owner() *mcpclient.Client {
	return paClient(e.t, e.url, fmExampleKey)
}

// agent is the example agent's MCP connection: the key, its host, and its
// process, as `seam mcp-proxy` sends them.
func (e *fmExample) agent() *mcpclient.Client {
	return paClientHeaders(e.t, e.url, fmExampleKey, map[string]string{mcp.AgentProcessHeader: e.agentID, mcp.HostHeader: e.host})
}

// hook posts one of the agent's hooks from repository cwd, as `seam hook`
// does on the daemon's own machine (which reads the agent's files itself).
func (e *fmExample) hook(event, cwd string, body map[string]any) fmHookAnswer {
	t := e.t
	t.Helper()
	q := url.Values{"host": {e.host}, "agent_process": {e.agentID}}
	if event == "session-start" {
		q.Set("repo_root", cwd)
		q.Set("main_root", cwd)
	}
	body["session_id"], body["cwd"] = e.extID, cwd
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, e.url+"/api/hooks/"+event+"?"+q.Encode(), bytes.NewReader(raw))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+fmExampleKey)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, "hook %s: %s", event, out)
	a := fmHookAnswer{status: resp.StatusCode, body: string(out), captureRefusedBy: resp.Header.Get(harvest.RefusedHeader)}
	var shaped struct {
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if json.Unmarshal(out, &shaped) == nil {
		a.context = shaped.HookSpecificOutput.AdditionalContext
	}
	return a
}

// console makes one console request with the key, as `seam approve` does.
func (e *fmExample) console(method, path string, form url.Values) (int, string) {
	t := e.t
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, e.url+path, body)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+fmExampleKey)
	req.Header.Set("Accept", "application/json")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(out)
}

// facts is a request's facts as the owner's console shows them.
func (e *fmExample) facts(short string) approval.Facts {
	e.t.Helper()
	status, body := e.console(http.MethodGet, "/console/access/"+url.PathEscape(short)+"?format=json", nil)
	require.Equal(e.t, http.StatusOK, status, body)
	var one struct {
		Request  approval.Facts `json:"request"`
		Advisory bool           `json:"advisory"`
	}
	require.NoError(e.t, json.Unmarshal([]byte(body), &one), body)
	require.True(e.t, one.Advisory, "without peer_auth the approval is advisory")
	return one.Request
}

// approve approves request f with the key over the console (seam approve's
// y), with access chosen ("" for the default, read-only), and returns the
// answer.
func (e *fmExample) approve(f approval.Facts, accessChoice string) map[string]any {
	e.t.Helper()
	form := url.Values{"duration": {approval.For1h}, "digest": {f.Digest}}
	if accessChoice != "" {
		form.Set("access", accessChoice)
	}
	status, body := e.console(http.MethodPost, "/console/access/"+url.PathEscape(f.ID)+"/approve?format=json", form)
	require.Equal(e.t, http.StatusOK, status, body)
	var out map[string]any
	require.NoError(e.t, json.Unmarshal([]byte(body), &out), body)
	return out
}

func (e *fmExample) count(query string, args ...any) int {
	e.t.Helper()
	var n int
	require.NoError(e.t, e.db.QueryRowContext(e.ctx, query, args...).Scan(&n))
	return n
}

// stamps is the stamps item_provenance holds for one item, as
// "<source project> <approval id>" lines.
func (e *fmExample) stamps(kind, id string) []string {
	e.t.Helper()
	rows, err := e.db.QueryContext(e.ctx, `SELECT source_project, approval_id FROM item_provenance
		WHERE item_kind = ? AND item_id = ? ORDER BY source_project`, kind, id)
	require.NoError(e.t, err)
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var p, a string
		require.NoError(e.t, rows.Scan(&p, &a))
		out = append(out, p+" "+a)
	}
	require.NoError(e.t, rows.Err())
	return out
}

// fmPlantExample writes one project's content by its own agent session: a
// house rule, a note whose body carries secret, and returns the note's id.
func fmPlantExample(t *testing.T, cli *mcpclient.Client, project, secret string) string {
	t.Helper()
	gmOK(t, cli, "session_start", map[string]any{"project": project, "name": project + "-own-agent"})
	gmOK(t, cli, "memory_write", map[string]any{"name": project + "-house-rule", "kind": "constraint",
		"description": project + " house rule", "body": "The " + project + " house rule.\n"})
	return gmOK(t, cli, "notes_create", map[string]any{"title": project + " working notes",
		"body": "The " + project + " working notes: " + secret + "."})["id"].(string)
}

// fmCarry is a request's carry as its facts must state it.
func fmCarry(home string, holds, writeTo []string) approval.CarryFacts {
	holds, writeTo = slices.Clone(holds), slices.Clone(writeTo)
	slices.Sort(holds)
	slices.Sort(writeTo)
	return approval.CarryFacts{Home: home, Holds: holds, WriteTo: writeTo}
}

func TestFencedExample_DataRoom(t *testing.T) {
	e := fmStartExample(t)
	secrets := map[string]string{fmFinance: "fin7ledger", fmLegal: "leg7clause", fmDataRoom: "room7index"}
	notes := map[string]string{}
	for _, p := range []string{fmDataRoom, fmFinance, fmLegal} {
		notes[p] = fmPlantExample(t, e.owner(), p, secrets[p])
	}
	room, fin := "/work/"+fmDataRoom, "/work/"+fmFinance

	// The agent starts in the data room's repository: its home.
	start := e.hook("session-start", room, map[string]any{"source": "startup", "hook_event_name": "SessionStart"})
	require.Contains(t, start.context, fmDataRoom+" house rule")
	home, ok, err := store.AmbientSessionByExternalIdentity(e.ctx, e.db, "claude-code", e.extID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, fmDataRoom, home.ProjectSlug)
	cli := e.agent()
	cite := func(target string) string { return approval.CiteByReferenceLine(target, fmDataRoom) }

	// 1. A read of the finance project: refused, and a read request filed.
	text := fmRefused(t, cli, "notes_read", map[string]any{"id": notes[fmFinance]})
	readFin := fmShortOf(t, text)
	require.Equal(t, "notes_read: project "+fmFinance+" is confidential: only a conversation working in it, or one the "+
		"owner granted a read of it, reads it -- if your task is about "+fmFinance+", ask the owner for a grant of it "+
		"(session_start project="+fmFinance+" asks); otherwise leave it alone\nRemedy: read request "+readFin+" is waiting "+
		"-- the owner approves it in their own terminal with `seam approve "+readFin+"`, or with Approve on the console's "+
		"Access requests page; retry once it is approved. An approved read is read-only, and nothing read from "+fmFinance+
		" may be written anywhere but "+fmDataRoom+". "+cite(fmFinance), text)
	f1 := e.facts(readFin)
	require.Equal(t, store.AccessKindGrant, f1.Kind)
	require.Equal(t, fmDataRoom, f1.From)
	require.Equal(t, fmCarry(fmDataRoom, []string{fmDataRoom, fmFinance}, []string{fmDataRoom}), *f1.Carry)
	require.Equal(t, []approval.Warning{{Code: approval.WarnCopyInto, Text: "content from " + fmFinance +
		" may be copied into " + fmDataRoom}}, f1.Warnings)
	require.Equal(t, "read-only", f1.AskedAccess)
	require.Equal(t, []string{"read-only", "read-write"}, f1.AccessChoices)
	require.Equal(t, "content from "+fmFinance+" may be copied into "+fmDataRoom+", and it may change "+fmFinance+
		"'s tasks, memories and notes; content from "+fmDataRoom+" may be copied into "+fmFinance, f1.ReadWrite)
	got := e.approve(f1, "")
	require.Equal(t, store.GrantAccessRead, got["access"])
	require.Equal(t, 1, e.count(`SELECT COUNT(*) FROM access_grants WHERE approval_id = ? AND decided_by = 'bearer-key'`, f1.ID))

	items, isErr := gmCall(t, cli, "notes_read", map[string]any{"id": notes[fmFinance]})
	require.False(t, isErr, "%v", items)
	require.Contains(t, items[0], secrets[fmFinance])
	require.True(t, strings.HasPrefix(gmText(items[1:]), "Read access to "+fmFinance+" was approved until "), items)

	// What it writes at home now may contain the finance project's content.
	summary := gmOK(t, cli, "memory_write", map[string]any{"name": "room-index-summary", "kind": "reference",
		"description": "the data room index", "body": "The index, citing " + fmFinance + ":" + notes[fmFinance] + ".\n"})
	require.Equal(t, fmDataRoom, summary["project"])
	require.Equal(t, []string{fmFinance + " " + f1.ID}, e.stamps(core.ItemKindMemory, summary["id"].(string)))

	// 2. A write into the finance project: refused, and a read-and-write
	// request filed; the owner chooses read and write.
	text = fmRefused(t, cli, "tasks_add", map[string]any{"project": fmFinance, "title": "reconcile the data room index"})
	writeFin := fmShortOf(t, text)
	require.Equal(t, "tasks_add: this session holds content from "+fmDataRoom+", "+fmFinance+", so it writes only into "+
		fmDataRoom+": a write into project "+fmFinance+" is refused\nRemedy: read-and-write request "+writeFin+
		" is waiting -- the owner approves it in their own terminal with `seam approve "+writeFin+"`, or with Approve on the "+
		"console's Access requests page; retry once it is approved. Approved read and write, this session changes "+
		fmFinance+"'s tasks, memories and notes as its own sessions do; approved read-only, it reads "+fmFinance+
		" and writes nothing there. "+cite(fmFinance), text)
	f2 := e.facts(writeFin)
	require.Equal(t, store.AccessKindGrant, f2.Kind)
	require.Equal(t, fmCarry(fmDataRoom, []string{fmDataRoom, fmFinance}, []string{fmDataRoom}), *f2.Carry)
	require.Equal(t, []approval.Warning{{Code: approval.WarnCopyInto, Text: "content from " + fmFinance +
		" may be copied into " + fmDataRoom}}, f2.Warnings)
	require.Equal(t, "read-write", f2.AskedAccess)
	require.Equal(t, f1.ReadWrite, f2.ReadWrite)
	got = e.approve(f2, "read-write")
	require.Equal(t, store.GrantAccessWrite, got["access"])

	task := gmOK(t, cli, "tasks_add", map[string]any{"project": fmFinance, "title": "reconcile the data room index",
		"body": "match the index to the ledger"})
	require.Equal(t, "match the index to the ledger", task["body"], "a read-and-write grant reads what it writes")
	require.Equal(t, []string{fmDataRoom + " " + f2.ID}, e.stamps(core.ItemKindTask, task["id"].(string)))
	visit := gmOK(t, cli, "memory_write", map[string]any{"project": fmFinance, "name": "data-room-visit",
		"kind": "reference", "description": "what the data room review found", "body": "Two entries differ.\n"})
	require.Equal(t, fmFinance, visit["project"])
	read := gmOK(t, cli, "memory_read", map[string]any{"id": visit["id"]})
	require.Equal(t, []any{map[string]any{"project": fmDataRoom, "grant": f2.ID}}, read["may_contain"])

	// 3. A read of the legal project: its facts state the whole carry --
	// legal's content may end up in the data room and in finance.
	text = fmRefused(t, cli, "notes_read", map[string]any{"id": notes[fmLegal]})
	readLegal := fmShortOf(t, text)
	require.Contains(t, text, "\nRemedy: read request "+readLegal+" is waiting -- ")
	f3 := e.facts(readLegal)
	require.Equal(t, fmCarry(fmDataRoom, []string{fmDataRoom, fmFinance, fmLegal}, []string{fmDataRoom, fmFinance}), *f3.Carry)
	require.Equal(t, []approval.Warning{{Code: approval.WarnCopyInto, Text: "content from " + fmLegal +
		" may be copied into " + fmDataRoom + " and " + fmFinance}}, f3.Warnings)
	require.Equal(t, "content from "+fmLegal+" may be copied into "+fmDataRoom+" and "+fmFinance+", and it may change "+
		fmLegal+"'s tasks, memories and notes; content from "+fmDataRoom+" and "+fmFinance+" may be copied into "+fmLegal,
		f3.ReadWrite)
	e.approve(f3, "")
	items, isErr = gmCall(t, cli, "notes_read", map[string]any{"id": notes[fmLegal]})
	require.False(t, isErr, "%v", items)
	require.Contains(t, items[0], secrets[fmLegal])

	// Every write now names what it may contain, by the approval that
	// cleared it: at home the finance and legal content, in finance the
	// data room's and legal's.
	second := gmOK(t, cli, "memory_write", map[string]any{"name": "room-index-review", "kind": "reference",
		"description": "the review", "body": "Reviewed.\n"})
	require.Equal(t, fmDataRoom, second["project"])
	// The finance content was cleared into the home by both finance
	// approvals; the stamp names the first.
	require.Equal(t, []string{fmFinance + " " + f1.ID, fmLegal + " " + f3.ID}, e.stamps(core.ItemKindMemory, second["id"].(string)))
	finNote := gmOK(t, cli, "notes_create", map[string]any{"project": fmFinance, "title": "data room follow-up",
		"body": "Follow up on the two entries."})
	require.Equal(t, []string{fmDataRoom + " " + f2.ID, fmLegal + " " + f3.ID}, e.stamps(core.ItemKindNote, finNote["id"].(string)))

	// Every record goes to the home, though the hooks fire from the
	// finance repository: findings, a plan, a subagent's report, the harvest.
	// Both approvals that cleared a project's content into the home name it:
	// the stamp names the first, finance's read grant.
	homeStamps := []string{fmFinance + " " + f1.ID, fmLegal + " " + f3.ID}
	upd := gmOK(t, cli, "session_update", map[string]any{"findings": "the index and the ledger differ in two entries"})
	require.Equal(t, home.ID, upd["session_id"], "%v", upd)
	updated, ok, err := store.SessionByID(e.ctx, e.db, home.ID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "the index and the ledger differ in two entries", updated.Findings)
	require.Equal(t, homeStamps, e.stamps(core.ItemKindSession, home.ID), "the findings are stamped")
	plansDir := filepath.Join(e.home, ".claude", "plans")
	require.NoError(t, os.MkdirAll(plansDir, 0o700))
	planPath := filepath.Join(plansDir, "reconcile-the-room.md")
	require.NoError(t, os.WriteFile(planPath, []byte("# Reconcile the room\n\nMatch the index to the ledger.\n"), 0o600))
	a := e.hook("post-tool-use", fin, map[string]any{"hook_event_name": "PostToolUse", "tool_name": "Write",
		"tool_input": map[string]any{"file_path": planPath}, "permission_mode": "plan"})
	require.Empty(t, a.captureRefusedBy)
	main := filepath.Join(e.files, e.extID+".jsonl")
	fmWriteTranscript(t, harvest.SubagentTranscriptPath("", main, "roomscan1"), "scan the room", "the room scan report")
	a = e.hook("subagent-stop", fin, map[string]any{"hook_event_name": "SubagentStop", "agent_id": "roomscan1",
		"agent_type": "Explore", "permission_mode": "plan", "transcript_path": main})
	require.Empty(t, a.captureRefusedBy)
	said := "The data room index and the finance ledger differ in two entries."
	fmWriteTranscript(t, main, "reconcile the room", said)
	a = e.hook("session-end", fin, map[string]any{"hook_event_name": "SessionEnd", "reason": "prompt_input_exit",
		"transcript_path": main})
	require.Empty(t, a.captureRefusedBy)

	for _, slug := range []string{plans.NotePrefix + "reconcile-the-room", plans.AgentNotePrefix + "roomscan1"} {
		note, found, err := store.NoteBySlug(e.ctx, e.db, fmDataRoom, slug)
		require.NoError(t, err)
		require.True(t, found, "%s lands in the home", slug)
		require.Equal(t, homeStamps, e.stamps(core.ItemKindNote, note.ID), "%s is stamped with what the conversation holds", slug)
		for _, p := range []string{fmFinance, fmLegal} {
			_, found, err := store.NoteBySlug(e.ctx, e.db, p, slug)
			require.NoError(t, err)
			require.False(t, found, "%s lands nowhere else", slug)
		}
	}
	ended, ok, err := store.SessionByID(e.ctx, e.db, home.ID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, core.SessionCompleted, ended.Status)
	require.Equal(t, fmDataRoom, ended.ProjectSlug)
	require.Equal(t, harvest.Prefix+said, ended.Findings, "the harvest is the home session's findings")
	require.Equal(t, homeStamps, e.stamps(core.ItemKindSession, home.ID))
	for _, f := range []approval.Facts{f1, f2, f3} {
		require.Equal(t, 1, e.count(`SELECT COUNT(*) FROM access_grants WHERE approval_id = ? AND decided_by = 'bearer-key'`, f.ID),
			"%s: the bearer key's decision, advisory without peer_auth", f.Short)
	}
	for _, p := range []string{fmFinance, fmLegal} {
		require.Zero(t, e.count(`SELECT COUNT(*) FROM sessions WHERE project_slug = ? AND claude_session_id = ?`, p, e.extID),
			"no session of the conversation in %s", p)
		require.Zero(t, e.count(`SELECT COUNT(*) FROM events WHERE project_slug = ? AND payload LIKE ?`, p, "%two entries.%"),
			"the harvest is in no %s event", p)
	}
	require.Zero(t, e.count(`SELECT COUNT(*) FROM sessions WHERE findings LIKE ? AND project_slug != ?`, "%two entries%", fmDataRoom))

	// The owner's console tells the conversation's story on one page: what it
	// reached and how, and what it wrote with its stamps.
	status, body := e.console(http.MethodGet, "/console/sessions/"+home.ID+"?format=json", nil)
	require.Equal(t, http.StatusOK, status, body)
	var page struct {
		Conversation struct {
			Reach []struct {
				Project string `json:"project"`
				Ways    []struct {
					Kind  string `json:"kind"`
					Words string `json:"words"`
				} `json:"ways"`
			} `json:"reach"`
			Written []struct {
				Project string `json:"project"`
			} `json:"written"`
		} `json:"conversation"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &page), body)
	reached := map[string]int{}
	for _, rc := range page.Conversation.Reach {
		reached[rc.Project] = len(rc.Ways)
	}
	require.Equal(t, 2, reached[fmFinance], "a read grant and a read-and-write grant: %s", body)
	require.Equal(t, 1, reached[fmLegal], "%s", body)
	var wrote []string
	for _, w := range page.Conversation.Written {
		wrote = append(wrote, w.Project)
	}
	require.Contains(t, wrote, fmFinance, body)
	require.Contains(t, wrote, fmDataRoom, body)
}
