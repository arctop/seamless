package approval

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/passkey/passkeytest"
	"github.com/arctop/seamless/internal/store"
)

// The anchor rule (anchor.go): no request names a process whose tree reaches
// past one agent session, and the approve core never approves a stored one
// that does.

// The kernel's names, as measured on macOS (proc_pidpath) and as Linux
// reports them; the classifier reads their shape, never one account's home.
func TestClassify_Programs(t *testing.T) {
	for _, c := range []struct {
		path  string
		kind  programKind
		agent string
	}{
		{"/Users/owner/.local/share/claude/versions/2.1.319", programAgent, "Claude Code 2.1.319"},
		{"/home/owner/.local/share/claude/versions/2.1.318 (deleted)", programAgent, "Claude Code 2.1.318"},
		{"/usr/local/bin/claude", programAgent, "Claude Code"},
		{"claude", programAgent, "Claude Code"}, // Linux's kernel name through the PATH symlink
		{"/Users/owner/Library/Application Support/Claude/claude-code/2.1.317/91184d3d9ba3/claude.app/Contents/MacOS/claude",
			programAgent, "Claude Code 2.1.317"},
		{"/Applications/Claude.app/Contents/Helpers/disclaimer", programAgent, "Claude app"},
		{"/Applications/ChatGPT.app/Contents/Resources/codex-cli/CodexCLI.app/Contents/MacOS/codex", programAgent, "Codex"},
		{"/opt/homebrew/Caskroom/codex/0.46.0/codex-aarch64-apple-darwin", programAgent, "Codex 0.46.0"},
		{"/usr/lib/node_modules/@openai/codex/vendor/x86_64-unknown-linux-musl/codex/codex", programAgent, "Codex"},
		{`C:\Users\owner\AppData\Roaming\npm\codex-x86_64-pc-windows-msvc.exe`, programAgent, "Codex"},
		{"codex-x86_64-un", programAgent, "Codex"}, // Linux's 15-byte kernel name for a triple-named binary
		{"codex-voice-hos", programOther, ""},

		{"/opt/homebrew/Cellar/tmux/3.8/bin/tmux", programBroad, ""},
		{"tmux: server", programBroad, ""}, // Linux's comm once tmux names itself
		{"SCREEN", programBroad, ""},
		{"zellij", programBroad, ""},
		{"/bin/zsh", programBroad, ""},
		{"-zsh", programBroad, ""},
		{"/usr/bin/login", programBroad, ""},
		{"/usr/sbin/sshd", programBroad, ""},
		{"sshd-session", programBroad, ""},
		{"/sbin/launchd", programBroad, ""},
		{"/lib/systemd/systemd", programBroad, ""},
		{"/System/Applications/Utilities/Terminal.app/Contents/MacOS/Terminal", programBroad, ""},
		{"/Applications/iTerm.app/Contents/MacOS/iTermServer-3.5.4", programBroad, ""},
		{"/Applications/Ghostty.app/Contents/MacOS/ghostty", programBroad, ""},
		{"/Applications/Warp.app/Contents/MacOS/stable", programBroad, ""}, // a terminal no list names
		{"/usr/bin/alacritty", programBroad, ""},
		{"/usr/bin/kitty", programBroad, ""},
		{"/usr/bin/wezterm-gui", programBroad, ""},
		{"gnome-terminal-", programBroad, ""}, // Linux keeps 15 bytes
		{"/Applications/Visual Studio Code.app/Contents/Frameworks/Code Helper (Plugin).app/Contents/MacOS/Code Helper (Plugin)", programBroad, ""},
		{"/Applications/Cursor.app/Contents/MacOS/Cursor", programBroad, ""},
		{"/Applications/Claude.app/Contents/MacOS/Claude", programBroad, ""},
		{"/usr/share/code/code", programBroad, ""},

		{"/opt/homebrew/bin/node", programOther, ""},
		{"/usr/bin/python3", programOther, ""},
		{"/usr/bin/timeout", programOther, ""},
		{"/Applications/ChatGPT.app/Contents/Resources/codex-cli/bin/codex-code-mode-host", programOther, ""},
		{"/Applications/Xcode.app/Contents/Developer/usr/bin/git", programOther, ""},
		{"2.1.319", programAgent, "Claude Code 2.1.319"}, // the kernel's name alone, when the path is unreadable
		{"", programUnknown, ""},
	} {
		got := classify(c.path)
		require.Equal(t, c.kind, got.kind, c.path)
		require.Equal(t, c.agent, got.agent, c.path)
		require.Equal(t, c.agent, agentName(c.path), c.path)
	}
}

// hop is one process of a test chain: its token and the program the kernel
// names for it.
type hop struct {
	tok  access.ProcessToken
	prog string
}

func hopAt(pid int32, prog string) hop {
	return hop{tok: access.ProcessToken{PID: pid, Version: uint64(pid) * 10, Boot: boot}, prog: prog}
}

// The task's table: the chain from the seam process up, which process
// agentproc names (the nearest that is not a shell), and whether the request
// is filed and anchored there.
func TestRequest_AnchorRule(t *testing.T) {
	const (
		claudePath = "/Users/owner/.local/share/claude/versions/2.1.319"
		codexPath  = "/opt/homebrew/bin/codex"
	)
	seam := hopAt(900, "/Users/owner/.local/bin/seam")
	for _, c := range []struct {
		name string
		// chain is the seam process first, then its ancestors.
		chain []hop
		// header is the index of the process agentproc names.
		header int
		// stamped stamps a live session with that process (its SessionStart
		// hook ran there).
		stamped bool
		filed   bool
		agent   string
	}{
		{"claude -> zsh -> seam: anchors at claude",
			[]hop{seam, hopAt(800, "/bin/zsh"), hopAt(700, claudePath), hopAt(600, "/bin/zsh"), hopAt(500, "/usr/bin/login")}, 2, true, true, "Claude Code 2.1.319"},
		{"tmux -> zsh -> seam: refused",
			[]hop{seam, hopAt(800, "/bin/zsh"), hopAt(700, "/opt/homebrew/Cellar/tmux/3.8/bin/tmux")}, 2, false, false, ""},
		{"launchd -> Terminal -> zsh -> seam: refused",
			[]hop{seam, hopAt(800, "/bin/zsh"), hopAt(700, "/System/Applications/Utilities/Terminal.app/Contents/MacOS/Terminal")}, 2, false, false, ""},
		{"launchd -> Terminal -> login -> zsh -> seam: refused at login",
			[]hop{seam, hopAt(800, "/bin/zsh"), hopAt(750, "/usr/bin/login"), hopAt(700, "/System/Applications/Utilities/Terminal.app/Contents/MacOS/Terminal")}, 2, false, false, ""},
		{"sshd -> zsh -> seam: refused",
			[]hop{seam, hopAt(800, "/bin/zsh"), hopAt(700, "/usr/sbin/sshd")}, 2, false, false, ""},
		{"codex -> bash -> seam: anchors at codex",
			[]hop{seam, hopAt(800, "/bin/bash"), hopAt(700, codexPath), hopAt(600, "/bin/zsh")}, 2, true, true, "Codex"},
		{"a new terminal app no list names: refused, no agent started it",
			[]hop{seam, hopAt(800, "/bin/zsh"), hopAt(700, "/opt/someterm/bin/someterm")}, 2, false, false, ""},
		{"even with a session stamped there by hand",
			[]hop{seam, hopAt(800, "/bin/zsh"), hopAt(700, "/opt/someterm/bin/someterm")}, 2, true, false, ""},
		{"a script under Claude Code: below an agent",
			[]hop{seam, hopAt(800, "/usr/bin/python3"), hopAt(750, "/bin/zsh"), hopAt(700, claudePath)}, 1, false, true, ""},
		{"Claude Code run by node (npm), its session stamped there",
			[]hop{seam, hopAt(800, "/bin/sh"), hopAt(700, "/opt/homebrew/bin/node"), hopAt(600, "/bin/zsh"), hopAt(500, "/usr/bin/login")}, 2, true, true, ""},
		{"node no session stamped: refused",
			[]hop{seam, hopAt(700, "/opt/homebrew/bin/node"), hopAt(600, "/bin/zsh"), hopAt(500, "/usr/bin/login")}, 1, false, false, ""},
		{"the Claude app's per-server wrapper",
			[]hop{seam, hopAt(700, "/Applications/Claude.app/Contents/Helpers/disclaimer"), hopAt(600, "/Applications/Claude.app/Contents/MacOS/Claude")}, 1, false, true, "Claude app"},
		{"the Claude app itself: refused",
			[]hop{seam, hopAt(600, "/Applications/Claude.app/Contents/MacOS/Claude")}, 1, false, false, ""},
		{"tmux under Claude Code is still tmux: refused",
			[]hop{seam, hopAt(800, "/bin/zsh"), hopAt(750, "/opt/homebrew/bin/tmux"), hopAt(700, claudePath)}, 2, false, false, ""},
		{"a process the kernel no longer names (it exited): filed as before",
			[]hop{seam, hopAt(700, "")}, 1, false, true, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newReqFixture(t)
			ctx := context.Background()
			names := map[access.ProcessToken]string{}
			chain := make([]access.ProcessToken, len(c.chain))
			for i, h := range c.chain {
				chain[i], names[h.tok] = h.tok, h.prog
			}
			f.svc.cfg.Programs = func(t access.ProcessToken) string { return names[t] }
			anchor := chain[c.header]
			var sess string
			if c.stamped {
				sess = f.session(t, "cc/anchor", "/work/ops", anchor)
			}
			a, err := f.svc.Request(ctx, RequestInput{Project: lockFin, View: f.view(t, chain...), SessionID: sess,
				AgentProcess: anchor.AgentID(), Host: "laptop"})
			pending, perr := store.PendingAccessApprovals(ctx, f.db, f.now)
			require.NoError(t, perr)
			if !c.filed {
				require.ErrorIs(t, err, ErrBroadAnchor)
				var ae *AnchorError
				require.ErrorAs(t, err, &ae)
				require.NotContains(t, err.Error(), "R-", "no request id: none was filed")
				require.Contains(t, err.Error(), ".\nRemedy: run the work from an agent session")
				require.Empty(t, pending, "nothing was filed")
				return
			}
			require.NoError(t, err)
			require.Equal(t, anchor.Key(), a.ProcessKey)
			require.Equal(t, c.chain[c.header].prog, a.Program)
			facts, err := f.svc.Facts(ctx, a.ID)
			require.NoError(t, err)
			require.Equal(t, c.agent, facts.Agent)
			require.NotContains(t, warningCodes(facts), WarnAnchorIsShell)
		})
	}
}

// A read grant request is filed through the same rule.
func TestRequestGrant_AnchorRule(t *testing.T) {
	f := newGrantFixture(t)
	g := NewGrants(GrantsConfig{Approvals: f.svc})
	ctx := context.Background()
	tmux := hopAt(700, "/opt/homebrew/Cellar/tmux/3.8/bin/tmux")
	seam := hopAt(900, "/Users/owner/.local/bin/seam")
	f.svc.cfg.Programs = func(t access.ProcessToken) string {
		if t == tmux.tok {
			return tmux.prog
		}
		return ""
	}
	sess := f.session(t, "cc/ops", "/work/ops", tmux.tok) // stamped by hand from the tmux pane
	_, err := g.RequestGrant(access.WithView(ctx, f.view(t, seam.tok, tmux.tok)), GrantInput{Project: confHR, SessionID: sess})
	require.ErrorIs(t, err, ErrBroadAnchor)
	pending, err := store.PendingAccessApprovals(ctx, f.db, f.now)
	require.NoError(t, err)
	require.Empty(t, pending)
}

// A request an older daemon filed for a broad ancestor is never approved, on
// any path: the approve core refuses it before any proof is checked.
func TestApprove_RefusesAStoredBroadAnchor(t *testing.T) {
	f := newReqFixture(t)
	ctx := context.Background()
	tmux := access.ProcessToken{PID: 3698, Version: 36980, Boot: boot}
	row, err := store.CreateAccessApproval(ctx, f.db, store.AccessRequest{
		Project: lockFin, Kind: store.AccessKindRequest, Lease: 8 * time.Hour, TTL: RequestTTL,
		PeerUID: 501, PeerPID: tmux.PID, ProcessKey: tmux.Key(), Program: "/opt/homebrew/Cellar/tmux/3.8/bin/tmux",
	}, f.now)
	require.NoError(t, err)
	facts, err := f.svc.Facts(ctx, row.ID)
	require.NoError(t, err)
	require.NotEmpty(t, facts.Warnings)
	require.Equal(t, WarnAnchorIsShell, facts.Warnings[0].Code, "the broadest reach comes first")
	require.Contains(t, facts.Warnings[0].Text, "tmux, a terminal multiplexer, not an agent")
	require.Contains(t, facts.Warnings[0].Text, "Seamless refuses to approve it")

	for _, proof := range []Proof{
		{Kind: ProofDaemon},
		{Kind: ProofPasskey, Assertion: []byte("{}")},
		{Kind: ProofTouchID, CredentialID: "any", Signature: []byte{1}},
	} {
		_, err := f.svc.Approve(ctx, Decision{ID: row.ID, Duration: ForSession, Digest: facts.Digest, Proof: proof})
		require.ErrorIs(t, err, ErrBroadAnchor, proof.Kind)
		require.Contains(t, err.Error(), "tmux, a terminal multiplexer")
	}
	leases, err := store.LiveAccessLeases(ctx, f.db, f.now)
	require.NoError(t, err)
	require.Empty(t, leases)
	still, err := store.AccessApprovalByID(ctx, f.db, row.ID)
	require.NoError(t, err)
	require.Equal(t, store.ApprovalPending, still.Status, "refused, not decided: the owner denies it")

	// pid 1 is refused whatever its name.
	init1, err := store.CreateAccessApproval(ctx, f.db, store.AccessRequest{
		Project: lockLeg, Kind: store.AccessKindRequest, Lease: 8 * time.Hour, TTL: RequestTTL,
		PeerUID: 501, PeerPID: 1, ProcessKey: access.ProcessToken{PID: 1, Version: 10, Boot: boot}.Key(),
	}, f.now)
	require.NoError(t, err)
	ifacts, err := f.svc.Facts(ctx, init1.ID)
	require.NoError(t, err)
	_, err = f.svc.Approve(ctx, Decision{ID: init1.ID, Duration: ForSession, Digest: ifacts.Digest, Proof: Proof{Kind: ProofDaemon}})
	require.ErrorIs(t, err, ErrBroadAnchor)

	// An agent's stored request is approved as before.
	ok, err := store.CreateAccessApproval(ctx, f.db, store.AccessRequest{
		Project: lockFin, Kind: store.AccessKindRequest, Lease: 8 * time.Hour, TTL: RequestTTL,
		PeerUID: 501, PeerPID: agent.PID, ProcessKey: agent.Key(), Program: "/usr/local/bin/claude",
	}, f.now)
	require.NoError(t, err)
	f.approveDaemon(t, ok.ID, For1h)
}

// The same for a read grant request: Grants.Approve refuses it too.
func TestGrantApprove_RefusesAStoredBroadAnchor(t *testing.T) {
	f := newGrantFixture(t)
	g := NewGrants(GrantsConfig{Approvals: f.svc})
	ctx := context.Background()
	sess := f.session(t, "cc/ops", "/work/ops", agent)
	tmux := access.ProcessToken{PID: 3698, Version: 36980, Boot: boot}
	row, err := store.CreateGrantRequest(ctx, f.db, store.GrantRequest{
		Project: confHR, From: openOps, Lease: 8 * time.Hour, TTL: RequestTTL,
		PeerUID: 501, PeerPID: tmux.PID, ProcessKey: tmux.Key(), Program: "tmux: server", SessionID: sess,
	}, f.now)
	require.NoError(t, err)
	facts, err := g.Facts(ctx, row.ID)
	require.NoError(t, err)
	_, err = g.Approve(ctx, Decision{ID: row.ID, Duration: For1h, Digest: facts.Digest, Proof: Proof{Kind: ProofDaemon}})
	require.ErrorIs(t, err, ErrBroadAnchor)
	grants, err := g.Live(ctx)
	require.NoError(t, err)
	require.Empty(t, grants)
}

// The facts serve their warnings most serious first (WarningOrder), whatever
// order they were found in.
func TestFacts_WarningOrder(t *testing.T) {
	require.Equal(t, []string{"anchor_is_shell", "copy_into", "session_tainted", "other_pending", "cwd_outside_repo",
		"no_session", "refresh"}, WarningOrder, "the order is a contract: the Touch ID prompt shows the first three")

	f := newReqFixture(t)
	ctx := context.Background()
	f.svc.cfg.Host = "laptop"
	require.NoError(t, store.AddRepoMapping(ctx, f.db, "/work/finance", lockFin))
	sess := f.session(t, "cc/fin", "/work/elsewhere", agent)
	_, err := store.SetSessionTaint(ctx, f.db, sess, "web", "tool", f.now)
	require.NoError(t, err)
	// A refresh, from a tree that also asks for another project, by a
	// session working elsewhere that read outside text, anchored (by an
	// older daemon) at a shell: every warning a lock's request can carry
	// but no_session at once.
	refresh, err := store.CreateAccessApproval(ctx, f.db, store.AccessRequest{
		Project: lockFin, Kind: store.AccessKindRefresh, Lease: 8 * time.Hour, TTL: RequestTTL,
		PeerUID: 501, PeerPID: agent.PID, ProcessKey: agent.Key(), Program: "/bin/zsh",
		SessionID: sess, SessionName: "cc/fin", CWD: "/work/elsewhere",
	}, f.now)
	require.NoError(t, err)
	_, err = store.CreateAccessApproval(ctx, f.db, store.AccessRequest{
		Project: lockLeg, Kind: store.AccessKindRequest, Lease: 8 * time.Hour, TTL: RequestTTL,
		PeerUID: 501, PeerPID: agent.PID, ProcessKey: agent.Key(), Program: "/bin/zsh", SessionID: sess,
	}, f.now)
	require.NoError(t, err)
	facts, err := f.svc.Facts(ctx, refresh.ID)
	require.NoError(t, err)
	require.Equal(t, []string{WarnAnchorIsShell, WarnTainted, WarnOtherPending, WarnCWDOutside, WarnRefreshOfLease},
		warningCodes(facts), "the refresh warning no longer comes before the broadest reach")
}

// Facts.Agent is the readable agent name, part of the JSON (and so of the
// digest) when known, absent otherwise.
func TestFacts_AgentName(t *testing.T) {
	f := newReqFixture(t)
	ctx := context.Background()
	f.svc.cfg.Programs = func(t access.ProcessToken) string {
		if t == agent {
			return "/home/owner/.local/share/claude/versions/2.1.319"
		}
		return ""
	}
	sess := f.session(t, "cc/fin", "/work/ops", agent)
	a := f.request(t, lockFin, sess, bridge, agent, shell)
	facts, err := f.svc.Facts(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, "Claude Code 2.1.319", facts.Agent)
	b, err := json.Marshal(facts)
	require.NoError(t, err)
	require.Contains(t, string(b), `"agent":"Claude Code 2.1.319"`)
	before := facts.Digest
	facts.Agent = ""
	require.NotEqual(t, before, digestOf(facts), "the digest covers the agent's name")

	other := f.request(t, lockLeg, f.session(t, "cc/b", "/work/ops", other), otherBr, other, shell)
	ofacts, err := f.svc.Facts(ctx, other.ID)
	require.NoError(t, err)
	b, err = json.Marshal(ofacts)
	require.NoError(t, err)
	require.NotContains(t, string(b), `"agent"`, "no name, no key")
}

// Ways offers only the approval paths the daemon has: a passkey page only
// once a passkey is enrolled, Touch ID only once a key is.
func TestWays_OnlyThePathsThatExist(t *testing.T) {
	f := newReqFixture(t)
	f.svc.cfg.Origins = []string{origin}
	ctx := context.Background()
	sess := f.session(t, "cc/fin", "/work/ops", agent)
	a := f.request(t, lockFin, sess, bridge, agent, shell)
	require.Equal(t, access.Ways{}, f.svc.Ways(ctx, a.ID), "nothing enrolled: only the terminal")
	require.NotEmpty(t, f.svc.Page(a.ID), "Page still names the page itself (the console links it)")

	f.enrollTouchIDKey(t)
	require.Equal(t, access.Ways{TouchID: true}, f.svc.Ways(ctx, a.ID))

	key := newAuthenticator(t)
	f.enrollWithCode(t, key, "security key")
	require.Equal(t, access.Ways{TouchID: true, Passkey: true, Page: origin + "/console/access/" + a.ID}, f.svc.Ways(ctx, a.ID))
	require.Equal(t, access.Ways{TouchID: true, Passkey: true}, f.svc.Ways(ctx, ""), "no request yet: no page")

	advisory := NewGrants(GrantsConfig{DB: f.db})
	require.Equal(t, access.Ways{}, advisory.Ways(ctx, a.ID), "the advisory tier's decision is the bearer key's")
	require.Equal(t, f.svc.Ways(ctx, a.ID), NewGrants(GrantsConfig{Approvals: f.svc}).Ways(ctx, a.ID))
}

// enrollTouchIDKey enrolls a Touch ID key, as the helper app would.
func (f *fixture) enrollTouchIDKey(t *testing.T) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	pub, err := key.PublicKey.Bytes()
	require.NoError(t, err)
	code := f.code(t, store.ApproverTouchID)
	d := sha256.Sum256(TouchIDEnrollMessage(code, pub))
	sig, err := ecdsa.SignASN1(rand.Reader, key, d[:])
	require.NoError(t, err)
	_, err = f.svc.EnrollTouchID(context.Background(), "this Mac", code, pub, sig)
	require.NoError(t, err)
}

func newAuthenticator(t *testing.T) *passkeytest.Authenticator {
	t.Helper()
	a, err := passkeytest.New(true)
	require.NoError(t, err)
	return a
}

func TestAgentLabel(t *testing.T) {
	for _, tc := range []struct {
		program string
		pid     int32
		want    string
	}{
		{"/Users/owner/.local/share/claude/versions/2.1.319", 4242, "Claude Code 2.1.319 (pid 4242)"},
		{"/usr/local/bin/python3", 4242, "python3 (pid 4242)"},
		{"", 4242, "pid 4242"},
		{"/usr/local/bin/python3", 0, ""},
	} {
		require.Equal(t, tc.want, AgentLabel(tc.program, tc.pid), "%q %d", tc.program, tc.pid)
	}
}
