package main

// The fenced-sessions end-to-end matrix's harness (plan fenced-sessions P12):
// one agent conversation on the read-grant matrix's daemon (newGMRig: the real
// route table over real services, wired as serve wires them), driven the way
// Claude Code and its seam processes drive a daemon. Its SessionStart hook
// records it (the agent process in the query, as `seam hook` sends it), its
// MCP connection names its process (X-Seamless-Agent-Process, as `seam
// mcp-proxy` sends it), and under peer_auth both arrive on the socket edge
// with the kernel chain of the process that sent them -- the bridge or the
// hook, under the agent. The agent's own files (its transcript, a subagent's,
// a plan) are real files: without peer_auth the daemon reads them, as it does
// on the agent's own machine; under peer_auth the conversation pushes what
// `seam hook` reads from them with the daemon's own parsers (internal/harvest),
// since the daemon opens no path a hook names.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/harvest"
	"github.com/arctop/seamless/internal/store"
)

// The matrix conversation's processes, one boot with the read-grant
// matrix's: its Claude Code, the seam mcp-proxy its MCP connection runs
// through, and the seam hook its hooks run as. gmPrograms names them.
var (
	fmAgent  = access.ProcessToken{PID: 1200, Version: 12000, Boot: gmBoot}
	fmBridge = access.ProcessToken{PID: 1210, Version: 12100, Boot: gmBoot}
	fmHooker = access.ProcessToken{PID: 1220, Version: 12200, Boot: gmBoot}
)

// fmRuleToken is in every house rule fmPlant writes: a briefing that
// carries a project's rule carries "<project> house rule".
const fmRuleToken = "fm9rule"

// fmConv is one agent conversation: its client session id, the repository
// it works in, and how it reaches the daemon in the rig's tier.
type fmConv struct {
	r     *gmRig
	extID string
	cwd   string
	// files holds the agent's own files: its transcript, a subagent's, a
	// plan (plans is the rig's plans directory).
	files, plans string
	// mcpURL and hookURL are the socket servers of the bridge's and the
	// hook's chains under peer_auth, the TCP listener otherwise; key is the
	// bearer key on TCP, "" on the socket.
	mcpURL, hookURL, key string
}

// fmConversation is the matrix agent's conversation extID, working in
// project's repository. Nothing is recorded until its SessionStart hook runs.
func (r *gmRig) fmConversation(extID, project string) *fmConv {
	r.t.Helper()
	c := &fmConv{r: r, extID: extID, cwd: "/work/" + project, files: r.t.TempDir(), plans: r.plansDir()}
	if r.peer {
		c.mcpURL = r.socket(fmBridge, fmAgent).URL
		c.hookURL = r.socket(fmHooker, fmAgent).URL
	} else {
		c.mcpURL, c.hookURL, c.key = r.tcp.URL, r.tcp.URL, gmKey
	}
	return c
}

// plansDir is where the rig's hooks look for plan files.
func (r *gmRig) plansDir() string {
	return filepath.Join(r.dir, "plans")
}

// at is the conversation as seen from subtest t.
func (c *fmConv) at(t *testing.T) *fmConv {
	cc := *c
	cc.r = c.r.at(t)
	return &cc
}

// dial opens the conversation's MCP connection (a fresh one each call, as a
// new `seam mcp-proxy` would).
func (c *fmConv) dial() *mcpclient.Client {
	c.r.t.Helper()
	return c.r.dial(c.mcpURL, c.key, fmAgent)
}

// transcript is the conversation's transcript: its first prompt, then its
// last assistant message, said.
func (c *fmConv) transcript(said string) string {
	c.r.t.Helper()
	path := filepath.Join(c.files, c.extID+".jsonl")
	fmWriteTranscript(c.r.t, path, "work through the step", said)
	return path
}

// fmWriteTranscript writes a Claude Code transcript (or a subagent's): one
// user line, then one assistant line with no tool use (the terminal shape).
func fmWriteTranscript(t *testing.T, path, prompt, said string) {
	t.Helper()
	user, err := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": prompt}})
	require.NoError(t, err)
	asst, err := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant",
		"content": []any{map[string]any{"type": "text", "text": said}}}})
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, append(append(user, '\n'), append(asst, '\n')...), 0o600))
}

// fmHookAnswer is a hook's answer: its status, body, the injected context
// and the capture-refused header.
type fmHookAnswer struct {
	status           int
	body, context    string
	captureRefusedBy string
}

// hook posts one hook event as `seam hook` does: host and agent process in
// the query (the repository roots for a session start), the key on TCP, and
// -- under peer_auth -- the capture it pushes (push, when not nil).
func (c *fmConv) hook(event string, body map[string]any, push *harvest.Push) fmHookAnswer {
	t := c.r.t
	t.Helper()
	q := url.Values{"host": {gmHost}, "agent_process": {fmAgent.AgentID()}}
	if event == "session-start" {
		q.Set("repo_root", c.cwd)
		q.Set("main_root", c.cwd)
	}
	if c.r.peer && push != nil {
		push.Version = harvest.PushVersion
		body[harvest.Field] = push
	}
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, c.hookURL+"/api/hooks/"+event+"?"+q.Encode(), bytes.NewReader(raw))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	a := fmHookAnswer{status: resp.StatusCode, body: string(out), captureRefusedBy: resp.Header.Get(harvest.RefusedHeader)}
	var shaped struct {
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if json.Unmarshal(out, &shaped) == nil {
		a.context = shaped.HookSpecificOutput.AdditionalContext
	}
	require.Equal(t, http.StatusOK, a.status, "hook %s: %s", event, out)
	return a
}

// start posts the conversation's SessionStart (source startup, resume,
// compact or clear) and returns the hook's answer.
func (c *fmConv) start(source string) fmHookAnswer {
	c.r.t.Helper()
	return c.hook("session-start", map[string]any{"session_id": c.extID, "cwd": c.cwd, "source": source,
		"hook_event_name": "SessionStart", "transcript_path": filepath.Join(c.files, c.extID+".jsonl")}, nil)
}

// end posts the conversation's SessionEnd, its transcript's last message
// said: the harvest.
func (c *fmConv) end(reason, said string) fmHookAnswer {
	c.r.t.Helper()
	path := c.transcript(said)
	return c.hook("session-end", map[string]any{"session_id": c.extID, "cwd": c.cwd, "reason": reason,
		"hook_event_name": "SessionEnd", "transcript_path": path},
		&harvest.Push{Findings: harvest.Findings(path)})
}

// plan posts the PostToolUse of a plan-mode Write of plan basename, body
// its text.
func (c *fmConv) plan(basename, body string) fmHookAnswer {
	t := c.r.t
	t.Helper()
	path := filepath.Join(c.plans, basename+".md")
	require.NoError(t, os.MkdirAll(c.plans, 0o755))
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return c.hook("post-tool-use", map[string]any{"session_id": c.extID, "cwd": c.cwd, "hook_event_name": "PostToolUse",
		"tool_name": "Write", "tool_input": map[string]any{"file_path": path, "content": body}, "permission_mode": "plan",
		"transcript_path": filepath.Join(c.files, c.extID+".jsonl")},
		&harvest.Push{Plan: &harvest.PlanFile{Basename: harvest.PlanBasename(path), Content: body}})
}

// subagentStart posts a subagent's SubagentStart whose spawn prompt is
// prompt, returning its answer (the subagent's briefing).
func (c *fmConv) subagentStart(agentID, prompt, report string) fmHookAnswer {
	c.r.t.Helper()
	main := c.subagentTranscript(agentID, prompt, report)
	return c.hook("subagent-start", map[string]any{"session_id": c.extID, "cwd": c.cwd, "hook_event_name": "SubagentStart",
		"agent_id": agentID, "agent_type": "Explore", "transcript_path": main}, &harvest.Push{Prompt: prompt})
}

// subagentStop posts a planning subagent's SubagentStop: its prompt and its
// final report.
func (c *fmConv) subagentStop(agentID, prompt, report string) fmHookAnswer {
	c.r.t.Helper()
	main := c.subagentTranscript(agentID, prompt, report)
	return c.hook("subagent-stop", map[string]any{"session_id": c.extID, "cwd": c.cwd, "hook_event_name": "SubagentStop",
		"agent_id": agentID, "agent_type": "Explore", "permission_mode": "plan", "transcript_path": main},
		&harvest.Push{Prompt: prompt, Report: report})
}

// subagentTranscript writes subagent agentID's transcript in Claude Code's
// layout beside the conversation's, and returns the conversation's path.
func (c *fmConv) subagentTranscript(agentID, prompt, report string) string {
	c.r.t.Helper()
	main := filepath.Join(c.files, c.extID+".jsonl")
	fmWriteTranscript(c.r.t, harvest.SubagentTranscriptPath("", main, agentID), prompt, report)
	return main
}

// ambient is the conversation's one ambient row (after a move, the child),
// active or not.
func (c *fmConv) ambient() core.Session {
	c.r.t.Helper()
	sess, ok, err := store.AmbientSessionByExternalIdentity(c.r.ctx, c.r.db, "claude-code", c.extID)
	require.NoError(c.r.t, err)
	require.True(c.r.t, ok, "conversation %s has an ambient row", c.extID)
	return sess
}

// fmApprove approves request ref as the tier's owner does: under peer_auth
// with seamlessd approve's core (the daemon account's proof); without it with
// seam approve's POST over the console (the bearer key). accessChoice is a
// grant's access ("" for the default, read-only). It returns the facts the
// owner approved.
func (r *gmRig) fmApprove(ref, accessChoice string) approval.Facts {
	t := r.t
	t.Helper()
	f := r.gmFacts(r.tcp.URL, gmKey, ref)
	if r.peer {
		args := []string{f.Short, "--for", approval.For1h, "--digest", f.Digest}
		if accessChoice != "" {
			args = append(args, "--access", accessChoice)
		}
		var out bytes.Buffer
		require.NoError(t, approveWith(r.ctx, r.svc, r.grants, &out, args))
		require.Contains(t, out.String(), "Approved "+f.Short+": ")
		return f
	}
	form := url.Values{"duration": {approval.For1h}, "digest": {f.Digest}}
	if accessChoice != "" {
		form.Set("access", accessChoice)
	}
	status, body := r.consoleDo(r.tcp.URL, gmKey, http.MethodPost, "/console/access/"+url.PathEscape(f.ID)+"/approve?format=json",
		form, nil)
	require.Equal(t, http.StatusOK, status, body)
	return f
}

// fmPlant plants what the matrix reads, each project's by an agent of its
// own -- over TCP with the key, and in a project locked under peer_auth by its
// leased agent tree (as plantAll does): a house rule (a constraint, which
// every briefing of its project pins) in target and in the open project, a
// note carrying target's secret, and the open docs project's session and
// memory, which a session= handle names. It returns the note's id.
func (r *gmRig) fmPlant(target string) string {
	t := r.t
	t.Helper()
	var note string
	for _, p := range []string{target, gmOpen} {
		cli := paClient(t, r.tcp.URL, gmKey)
		if r.peer && p == gmFin {
			r.stamp(gmFin, "cc/fin-agent", gmFinAgent)
			now := r.clock.Now()
			a, err := store.CreateAccessApproval(r.ctx, r.db, store.AccessRequest{Project: gmFin, Kind: store.AccessKindRequest,
				Lease: 8 * time.Hour, TTL: time.Minute, PeerUID: gmUID, PeerPID: gmFinAgent.PID, ProcessKey: gmFinAgent.Key()}, now)
			require.NoError(t, err)
			_, err = store.ApproveAccessFor(r.ctx, r.db, a.ID, a.Challenge, "daemon-account", 8*time.Hour, now)
			require.NoError(t, err)
			cli = r.dial(r.socket(gmFinBridge, gmFinAgent).URL, "", gmFinAgent)
		}
		gmOK(t, cli, "session_start", map[string]any{"project": p, "name": p + "-rules"})
		gmOK(t, cli, "memory_write", map[string]any{"name": p + "-house-rule", "kind": "constraint",
			"description": p + " house rule " + fmRuleToken, "body": "The " + p + " house rule " + fmRuleToken + ".\n"})
		if p == target {
			note = gmOK(t, cli, "notes_create", map[string]any{"title": p + " working notes",
				"body": "The " + p + " working notes: " + gmSecrets[p] + "."})["id"].(string)
		}
	}
	docs := paClient(t, r.tcp.URL, gmKey)
	gmOK(t, docs, "session_start", map[string]any{"project": gmDocs, "name": "docs-agent-session"})
	gmOK(t, docs, "memory_write", map[string]any{"name": "docs-style", "kind": "convention",
		"description": "docs " + gmSearch, "body": "docs body " + gmDocsToken})
	gmOK(t, docs, "memory_write", map[string]any{"name": "docs-ledger-visit", "kind": "reference",
		"description": "the quarterly ledger visit checklist", "body": "docs ledger visit\n"})
	return note
}

// fmOnlyIn requires that text is held in project and nowhere else: no
// session's findings, no event and no search row outside it.
func (r *gmRig) fmOnlyIn(text, project string) {
	t := r.t
	t.Helper()
	like := "%" + text + "%"
	for _, q := range []string{
		`SELECT COUNT(*) FROM sessions WHERE findings LIKE ? AND project_slug != ?`,
		`SELECT COUNT(*) FROM events WHERE payload LIKE ? AND COALESCE(project_slug, '') != ?`,
		`SELECT COUNT(*) FROM fts WHERE (title || ' ' || name || ' ' || description || ' ' || body) LIKE ? AND project != ?`,
	} {
		require.Zero(t, r.count(q, like, project), "%q outside %s: %s", text, project, q)
	}
	require.NotZero(t, r.count(`SELECT COUNT(*) FROM sessions WHERE findings LIKE ? AND project_slug = ?`, like, project),
		"%q is recorded in %s", text, project)
}

// fmFile is an item's file in the rig's corpus (an index row carries no
// body).
func (r *gmRig) fmFile(rel string) string {
	r.t.Helper()
	raw, err := os.ReadFile(filepath.Join(r.dir, rel))
	require.NoError(r.t, err)
	return string(raw)
}

// fmStamps is the stamps item_provenance holds for one item, as
// "<source project> <approval id>" lines.
func (r *gmRig) fmStamps(kind, id string) []string {
	r.t.Helper()
	rows, err := r.db.QueryContext(r.ctx, `SELECT source_project, approval_id FROM item_provenance
		WHERE item_kind = ? AND item_id = ? ORDER BY source_project`, kind, id)
	require.NoError(r.t, err)
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var p, a string
		require.NoError(r.t, rows.Scan(&p, &a))
		out = append(out, p+" "+a)
	}
	require.NoError(r.t, rows.Err())
	return out
}

// fmRequestOf is the id of the request short names.
func (r *gmRig) fmRequestOf(short string) string {
	r.t.Helper()
	a, err := r.grants.Resolve(r.ctx, short)
	require.NoError(r.t, err)
	return a.ID
}

// fmRefused calls tool on cli and requires a refusal, returning its text.
func fmRefused(t *testing.T, cli *mcpclient.Client, tool string, args map[string]any) string {
	t.Helper()
	items, isErr := gmCall(t, cli, tool, args)
	require.True(t, isErr, "%s %v must be refused: %v", tool, args, items)
	return gmText(items)
}

// fmShortOf is the request a refusal's remedy names.
func fmShortOf(t *testing.T, text string) string {
	t.Helper()
	short := gmShort.FindString(text)
	require.NotEmpty(t, short, "a request is named: %s", text)
	return short
}

// fmFrontmatterStamps reads a memory's or a note's may_contain from its
// file: the block's lines, trimmed ("- project: <slug>", "grant: <id>").
func (r *gmRig) fmFrontmatterStamps(rel string) []string {
	r.t.Helper()
	raw, err := os.ReadFile(filepath.Join(r.dir, rel))
	require.NoError(r.t, err)
	head, _, _ := strings.Cut(strings.TrimPrefix(string(raw), "---\n"), "\n---\n")
	_, block, found := strings.Cut(head, "may_contain:")
	if !found {
		return nil
	}
	var out []string
	for line := range strings.SplitSeq(block, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "-") {
			break // the next key
		}
		out = append(out, trimmed)
	}
	return out
}
