package main

// Client-side captures under peer_auth. With peer_auth on, the daemon runs as
// its own OS account and opens no path a hook names, so the captures that read
// the agent's files -- the session-end findings and token totals, the model
// sniff, plan and subagent capture, the git stamp -- are made here instead:
// `seam hook` runs as the owner, a child of the agent, and reads the same files
// with the same parsers the daemon uses when it may (internal/harvest). What it
// found rides in the hook body under harvest.Field, and the daemon applies it
// through the fences every hook write meets (internal/hooks pushed.go).
//
// It pushes only when server_url names the peer_auth Unix socket. That is the
// one transport whose daemon is certain to want the push: the socket exists
// only under peer_auth (config.PeerSocket), so a unix:// server_url means the
// daemon reads no agent files, and the request it lands on always carries a
// View, which is when the daemon reads the field at all. Learning it any other
// way would cost something the opt-in rule forbids: a probe request per hook,
// a field in the daemon's default responses, or state kept between hooks. On
// an http(s) server_url nothing here runs, and the payload forwarded is the
// agent client's own, byte for byte. (An owner agent on the TCP key under
// peer_auth makes none of these captures, as before; the TCP key never
// reaches a locked project either, and the guide sends such agents to the
// socket.)

import (
	"context"
	"encoding/json"
	"os"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/gitread"
	"github.com/arctop/seamless/internal/harvest"
)

// pushesCaptures reports whether this install's hooks push their captures:
// only when server_url names the peer_auth Unix socket (see the file header).
func pushesCaptures(cfg config.Config) bool {
	return cfg.SocketPath() != ""
}

// captureInput is the tolerant shape of every hook-body field a capture
// reads, across events and both clients. Unknown and absent fields are fine:
// a capture that finds nothing pushes nothing.
type captureInput struct {
	TranscriptPath       string          `json:"transcript_path"`
	CWD                  string          `json:"cwd"`
	Model                string          `json:"model"`
	ToolName             string          `json:"tool_name"`
	ToolInput            json.RawMessage `json:"tool_input"`
	ToolResponse         json.RawMessage `json:"tool_response"`
	AgentID              string          `json:"agent_id"`
	AgentTranscriptPath  string          `json:"agent_transcript_path"`
	LastAssistantMessage string          `json:"last_assistant_message"`
}

// withCapture returns payload with the capture for event added under
// harvest.Field, or payload unchanged when there is nothing to push (the
// event reads no files, they held nothing, or the payload is not a JSON
// object). It never fails the hook: any problem pushes nothing, and the daemon
// then records what it always recorded under peer_auth -- no capture.
func withCapture(ctx context.Context, event, client string, payload []byte, plansDir string) []byte {
	push, ok := buildCapture(ctx, event, client, payload, plansDir)
	if !ok || push.Validate() != nil {
		// A file that parsed into something the daemon would refuse (a negative
		// token count in a malformed transcript): pushing it would only be
		// refused whole there.
		return payload
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(payload, &body); err != nil || body == nil {
		return payload
	}
	raw, err := json.Marshal(push)
	if err != nil {
		return payload
	}
	body[harvest.Field] = raw // replaces anything the agent client sent under the name
	out, err := json.Marshal(body)
	if err != nil {
		return payload
	}
	return out
}

// buildCapture reads what event's capture needs from the agent's own files.
// ok is false when there is nothing to push. An absent --client is Claude
// Code, as it is to the daemon.
func buildCapture(ctx context.Context, event, client string, payload []byte, plansDir string) (harvest.Push, bool) {
	var in captureInput
	if err := json.Unmarshal(payload, &in); err != nil {
		return harvest.Push{}, false
	}
	claude := client == "" || client == hookClientClaudeCode
	push := harvest.Push{Version: harvest.PushVersion}
	switch event {
	case "session-start", "user-prompt-submit":
		// Codex states its model in every payload; Claude Code's is sniffed.
		if !claude || in.Model != "" {
			return harvest.Push{}, false
		}
		push.Model = sniffModel(in.TranscriptPath)
		return push, push.Model != ""
	case "session-end":
		if !claude {
			return harvest.Push{}, false // Codex has no SessionEnd
		}
		push.Findings = harvest.Findings(in.TranscriptPath)
		if usage, ok := harvest.ClaudeTokens(in.TranscriptPath); ok {
			push.Tokens = &usage
		}
		push.Model = sniffModel(in.TranscriptPath)
		return push, true
	case "stop":
		if claude {
			return harvest.Push{}, false // Claude Code installs no Stop hook
		}
		push.Findings = harvest.CodexStopFindings(in.LastAssistantMessage, in.TranscriptPath)
		if usage, ok := harvest.CodexTokens(in.TranscriptPath); ok {
			push.Tokens = &usage
		}
		return push, push.Findings != "" || push.Tokens != nil
	case "post-tool-use":
		if !claude {
			return harvest.Push{}, false
		}
		return planCapture(in, plansDir, push)
	case "permission-request":
		// Presenting reads no file, but the daemon under peer_auth acts only on
		// a push: the bare version says this client makes the captures.
		return push, claude && in.ToolName == "ExitPlanMode"
	case "subagent-start":
		if claude {
			push.Prompt, _, _ = harvest.SubagentTranscript(
				harvest.SubagentTranscriptPath(in.AgentTranscriptPath, in.TranscriptPath, in.AgentID))
		} else {
			push.Prompt = harvest.CodexFirstUserMessage(in.TranscriptPath)
		}
		return push, push.Prompt != ""
	case "subagent-stop":
		if !claude {
			return harvest.Push{}, false // Codex's SubagentStop only heartbeats the parent
		}
		path := harvest.SubagentTranscriptPath(in.AgentTranscriptPath, in.TranscriptPath, in.AgentID)
		if _, err := os.Stat(path); path == "" || err != nil {
			return harvest.Push{}, false // no transcript: nothing to wait for or push
		}
		push.Prompt, push.Report, _ = harvest.AwaitSubagentReport(ctx, path)
		if push.Prompt == "" && push.Report == "" {
			return harvest.Push{}, false
		}
		push.GitHead = gitHead(in.CWD)
		return push, true
	}
	return harvest.Push{}, false
}

// planCapture is the post-tool-use capture: the plan file a Write, Edit or
// MultiEdit wrote, or the one an ExitPlanMode approval names, read under this
// account's own plans dir, with the commit it was written at. An approval
// always pushes (the daemon falls back to the plan text in the payload, or the
// session's draft); an iteration whose file cannot be read pushes nothing.
func planCapture(in captureInput, plansDir string, push harvest.Push) (harvest.Push, bool) {
	var raw string
	switch in.ToolName {
	case "Write", "Edit", "MultiEdit":
		var ti struct {
			FilePath string `json:"file_path"`
		}
		if err := json.Unmarshal(in.ToolInput, &ti); err != nil {
			return harvest.Push{}, false
		}
		raw = ti.FilePath
	case "ExitPlanMode":
		var resp struct {
			FilePath string `json:"filePath"`
		}
		if len(in.ToolResponse) > 0 {
			if err := json.Unmarshal(in.ToolResponse, &resp); err != nil {
				resp.FilePath = ""
			}
		}
		raw = resp.FilePath
	default:
		return harvest.Push{}, false
	}
	push.GitHead = gitHead(in.CWD)
	if path, ok := harvest.PlanFilePath(plansDir, raw); ok {
		push.Plan = &harvest.PlanFile{Basename: harvest.PlanBasename(path)}
		if b, err := os.ReadFile(path); err == nil {
			push.Plan.Content = string(b)
		}
	}
	if in.ToolName == "ExitPlanMode" {
		return push, true
	}
	return push, push.Plan != nil && push.Plan.Content != ""
}

// sniffModel is a Claude Code transcript's last main-thread model, or "" when
// it holds none the daemon would accept.
func sniffModel(transcriptPath string) string {
	if m := harvest.TranscriptModel(transcriptPath); harvest.ValidModel(m) {
		return m
	}
	return ""
}

// gitHead is the commit checked out at cwd, or "" when it is not a full
// commit id (the daemon refuses a push carrying anything else).
func gitHead(cwd string) string {
	if cwd == "" {
		return ""
	}
	if head := gitread.Head(cwd); harvest.ValidGitHead(head) {
		return head
	}
	return ""
}
