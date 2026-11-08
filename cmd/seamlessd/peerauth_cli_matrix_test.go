//go:build darwin || linux

package main

// Step 10's CLI rows: the real `seam` binary, run as a child process of this
// test, against the daemon on a real Unix socket whose peer the kernel names
// (peerConnContext). The test process plays the agent: a lease anchored on it
// (selfToken) covers every seam it starts, because the kernel's chain of the
// seam's connection runs through it. A lease naming another process, no
// lease, and an expired one cover nothing.
//
// Every seam runs with a HOME, SEAMLESS_CONFIG and working directory inside a
// temp dir and with no inherited SEAMLESS_* variable, so it can only ever read
// this test's client config and dial this test's socket.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

var (
	cmSeamMu   sync.Mutex
	cmSeamPath string
)

// cmSeam builds the seam binary once per test run (a later test that finds it
// gone, its builder's temp dir cleaned up, builds it again).
func cmSeam(t *testing.T) string {
	t.Helper()
	cmSeamMu.Lock()
	defer cmSeamMu.Unlock()
	if cmSeamPath != "" {
		if _, err := os.Stat(cmSeamPath); err == nil {
			return cmSeamPath
		}
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	out := filepath.Join(t.TempDir(), "seam")
	start := time.Now()
	build := exec.Command("go", "build", "-o", out, "./cmd/seam")
	build.Dir = root
	build.Env = cmCleanEnv(os.Environ())
	msg, err := build.CombinedOutput()
	require.NoError(t, err, "go build ./cmd/seam: %s", msg)
	t.Logf("built seam in %s", time.Since(start).Round(time.Millisecond))
	cmSeamPath = out
	return out
}

// cmCleanEnv drops every SEAMLESS_* variable from env.
func cmCleanEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); !strings.HasPrefix(k, "SEAMLESS_") {
			out = append(out, kv)
		}
	}
	return out
}

// cmSeamRunner runs seam as a child of this process, against sock only.
type cmSeamRunner struct {
	bin, home, cfgPath string
}

func cmNewSeamRunner(t *testing.T, sock string) cmSeamRunner {
	t.Helper()
	home := t.TempDir()
	cfgPath := filepath.Join(home, ".config", "seamless", "seamless.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(cfgPath), 0o700))
	require.NoError(t, os.WriteFile(cfgPath, []byte("role: client\nserver_url: unix://"+sock+"\n"), 0o600))
	return cmSeamRunner{bin: cmSeam(t), home: home, cfgPath: cfgPath}
}

// run runs one seam command and returns its exit code, stdout and stderr.
func (r cmSeamRunner) run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.bin, args...)
	cmd.Dir = r.home
	// The whole environment, spelled out: nothing of the owner's (no
	// SEAMLESS_*, no real HOME) reaches the child.
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + r.home, "USERPROFILE=" + r.home,
		"SEAMLESS_CONFIG=" + r.cfgPath, "TMPDIR=" + os.TempDir(), "NO_COLOR=1", "LANG=C",
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		var exit *exec.ExitError
		require.True(t, errors.As(err, &exit), "seam %v did not run: %v", args, err)
		code = exit.ExitCode()
	}
	return code, stdout.String(), stderr.String()
}

// cmCLICase is one seam command. shows is the locked project whose secret it
// prints inside the leased tree ("" for a command that prints no content).
// outside is what it answers outside the tree: cmRefusedLocked (exit 1, the
// lock named), cmRefusedNotFound (exit 1, "not found": the item is absent),
// or cmFiltered (exit 0, the locked project's rows left out).
type cmCLICase struct {
	args    []string
	json    bool // also run with --json
	shows   string
	outside int
	// outsideOnly is a write probe, run only where it must be refused.
	outsideOnly bool
	// insideRefused is a read refused inside the tree too, by another rule
	// (the words its refusal carries): the confidential fence.
	insideRefused string
	// anyExit is a content-free command whose exit says nothing about the
	// lock (doctor judges the whole install): only its output is checked.
	anyExit bool
}

const (
	cmFiltered = iota
	cmRefusedLocked
	cmRefusedNotFound
)

func TestPeerAuthMatrix_CLI(t *testing.T) {
	ctx := context.Background()
	euid := uint32(os.Geteuid())
	d := cmNewDaemon(t, access.Account{UID: euid, Name: cmOwnerName, Home: "/Users/" + cmOwnerName}, nil)

	// The real socket, the kernel naming each peer at accept.
	dir, err := os.MkdirTemp("", "cm")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	srv := &http.Server{Handler: peerHandler(d.policy, storeLeases(d.db), d.logger, d.mux), ConnContext: peerConnContext}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	// The test process is the agent; a lease on it covers its children.
	self := selfToken(t)
	other := access.ProcessToken{PID: self.PID + 100000, Version: self.Version, Boot: self.Boot}
	lease := func(tok access.ProcessToken) {
		t.Helper()
		for _, p := range []string{cmFinance, cmLegal} {
			// The request row as filing writes it for this process (filing's
			// anchor rule would refuse the test binary unless an agent runs
			// above it, which no CI runner has), approved on the terminal path.
			a, err := store.CreateAccessApproval(ctx, d.db, store.AccessRequest{Project: p, Kind: store.AccessKindRequest,
				Lease: time.Hour, TTL: 5 * time.Minute, PeerUID: euid, PeerPID: tok.PID, ProcessKey: tok.Key(), SessionName: "cc/cm-cli"}, time.Now())
			require.NoError(t, err)
			d.approveShort(t, ctx, approval.ShortID(a.ID), p)
		}
	}
	revokeAll := func() {
		t.Helper()
		live, err := store.LiveAccessLeases(ctx, d.db, time.Now())
		require.NoError(t, err)
		for _, l := range live {
			require.NoError(t, store.RevokeAccessLease(ctx, d.db, l.ID, store.LeaseRevokedOwner, time.Now()))
		}
	}
	lease(self)

	// Planted by the agent itself, over the real socket: its own lease.
	client := config.Config{Role: config.RoleClient, AdvertisedURL: "unix://" + sock}
	hc, err := client.HTTPClient(0)
	require.NoError(t, err)
	agent, err := mcpclient.NewStreamableHttpClient(client.ServerURL()+"/api/mcp", transport.WithHTTPBasicClient(hc))
	require.NoError(t, err)
	t.Cleanup(func() { _ = agent.Close() })
	require.NoError(t, agent.Start(ctx))
	var init mcpgo.InitializeRequest
	init.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
	init.Params.ClientInfo = mcpgo.Implementation{Name: "cm-agent", Version: "0"}
	_, err = agent.Initialize(ctx, init)
	require.NoError(t, err)
	d.fin = d.plant(t, agent, cmFinance, cmFinSecret, "fin")
	d.legal = d.plant(t, agent, cmLegal, cmLegalSecret, "legal")
	d.plantOpen(t)
	// A repository of example-finance's, for seam prime.
	finRepo := filepath.Join(t.TempDir(), "finance-repo")
	require.NoError(t, os.MkdirAll(finRepo, 0o755))
	require.NoError(t, store.AddRepoMapping(ctx, d.db, finRepo, cmFinance))

	// A git repository for seam plan check, which reads the plan through the
	// console and then runs git there.
	gitRepo := filepath.Join(t.TempDir(), "plan-repo")
	gitOK := cmGitRepo(t, gitRepo)

	seam := cmNewSeamRunner(t, sock)
	fin, legal := d.fin, d.legal
	callJSON := func(m map[string]any) string { return mustJSON(t, m) }
	cases := []cmCLICase{
		// Tool-backed. example-legal is open, so a read with no session reaches
		// it once the lock opens; example-finance is confidential, so its tool
		// reads name a session bound there (seam call's "session").
		{args: []string{"memory", "read", "--project", cmLegal, legal.memName}, json: true, shows: cmLegal, outside: cmRefusedLocked},
		{args: []string{"memory", "read", "--id", legal.memID}, json: true, shows: cmLegal, outside: cmRefusedLocked},
		// The confidential project's fence still wants a session bound there:
		// an unbound read is refused inside the tree too, by the fence.
		{args: []string{"memory", "read", "--project", cmFinance, fin.memName}, json: true, insideRefused: "confidential", outside: cmRefusedLocked},
		{args: []string{"memory", "list", "--kind", "gotcha", "--project", cmLegal}, json: true, shows: cmLegal, outside: cmRefusedLocked},
		{args: []string{"memory", "list", "--project", cmLegal, cmSearch}, json: true, shows: cmLegal, outside: cmRefusedLocked},
		{args: []string{"note", "read", "--project", cmLegal, legal.noteSlug}, json: true, shows: cmLegal, outside: cmRefusedLocked},
		{args: []string{"note", "read", "--id", legal.noteID}, json: true, shows: cmLegal, outside: cmRefusedLocked},
		{args: []string{"note", "list", "--project", cmLegal, cmSearch}, json: true, shows: cmLegal, outside: cmRefusedLocked},
		{args: []string{"task", "list", "--project", cmLegal}, json: true, shows: cmLegal, outside: cmRefusedLocked},
		{args: []string{"ready", "--project", cmLegal}, json: true, shows: cmLegal, outside: cmRefusedLocked},
		{args: []string{"recall", "--project", cmLegal, cmSearch}, json: true, shows: cmLegal, outside: cmRefusedLocked},
		{args: []string{"recall", cmSearch}, json: true, outside: cmFiltered},
		{args: []string{"usage"}, json: true, outside: cmFiltered},
		{args: []string{"status"}, json: true, outside: cmFiltered},
		{args: []string{"call", "memory_read", callJSON(map[string]any{"project": cmLegal, "name": legal.memName})}, shows: cmLegal, outside: cmRefusedLocked},
		{args: []string{"call", "memory_read", callJSON(map[string]any{"id": fin.memID, "session": fin.sessName})}, shows: cmFinance, outside: cmRefusedLocked},
		{args: []string{"call", "notes_read", callJSON(map[string]any{"id": fin.noteID, "session": fin.sessName})}, shows: cmFinance, outside: cmRefusedLocked},
		{args: []string{"call", "tasks_list", callJSON(map[string]any{"project": cmFinance, "session": fin.sessName})}, shows: cmFinance, outside: cmRefusedLocked},
		{args: []string{"call", "recall", callJSON(map[string]any{"query": cmSearch, "project": cmFinance, "session": fin.sessName})}, shows: cmFinance, outside: cmRefusedLocked},
		{args: []string{"call", "trial_query", callJSON(map[string]any{"lab": fin.lab, "session": fin.sessName})}, shows: cmFinance, outside: cmRefusedLocked},
		{args: []string{"call", "lab_open", callJSON(map[string]any{"lab": fin.lab, "session": fin.sessName})}, shows: cmFinance, outside: cmRefusedLocked},
		// A session= handle alone: the call's scope is that session's.
		{args: []string{"call", "tasks_ready", callJSON(map[string]any{"session": fin.sessName})}, shows: cmFinance, outside: cmRefusedLocked},
		// A write through the handle: outside the tree only, so the agent's own
		// findings stay as planted.
		{args: []string{"call", "session_update", callJSON(map[string]any{"session": fin.sessName, "findings": "cli " + cmSearch})}, outsideOnly: true, outside: cmRefusedLocked},
		// Console-backed: the console's JSON routes, read through the view
		// the calling tree's lease chooses (the owner's surface: no
		// confidential fence, the lock alone).
		{args: []string{"plan", "list"}, json: true, shows: cmFinance, outside: cmFiltered},
		{args: []string{"plan", "show", fin.capture}, json: true, shows: cmFinance, outside: cmRefusedLocked},
		{args: []string{"plan", "show", legal.capture}, json: true, shows: cmLegal, outside: cmRefusedLocked},
		{args: []string{"sessions"}, json: true, outside: cmFiltered},
		{args: []string{"doctor"}, anyExit: true},
		{args: []string{"sessions", fin.sessID}, json: true, shows: cmFinance, outside: cmRefusedLocked},
		{args: []string{"proposals"}, json: true, outside: cmFiltered},
		{args: []string{"project", "isolation", cmFinance}, json: true, outside: cmRefusedLocked},
		// Last: prime starts a session in the locked project its cwd maps to.
		{args: []string{"prime", "--cwd", finRepo}, shows: cmFinance, outside: cmRefusedLocked},
	}

	if gitOK {
		// It prints verdicts, never a body: inside, it reads the plan and
		// reaches git; outside, the console names the lock.
		cases = append(cases, cmCLICase{args: []string{"plan", "check", "--cwd", gitRepo, fin.capture}, outside: cmRefusedLocked})
	}

	runAll := func(t *testing.T, state string, inside bool) {
		for _, c := range cases {
			if inside && c.outsideOnly {
				continue
			}
			variants := [][]string{c.args}
			if c.json {
				variants = append(variants, append(append([]string{}, c.args...), "--json"))
			}
			for _, args := range variants {
				what := fmt.Sprintf("%s: seam %s", state, strings.Join(args, " "))
				code, stdout, stderr := seam.run(t, args...)
				both := stdout + stderr
				if c.anyExit {
					for _, it := range d.secrets() {
						assert.NotContains(t, both, it.secret, "%s prints no content", what)
					}
					continue
				}
				if inside && c.insideRefused != "" {
					assert.Equal(t, 1, code, "%s: %s", what, firstN(both, 400))
					assert.Contains(t, stderr, c.insideRefused, what)
					for _, it := range d.secrets() {
						assert.NotContains(t, both, it.secret, "%s leaked %s", what, it.project)
					}
					continue
				}
				if inside {
					// Non-fatal: every command is judged.
					if !assert.Equal(t, 0, code, "%s: %s", what, firstN(both, 400)) {
						continue
					}
					for _, it := range d.secrets() {
						if it.project == c.shows {
							assert.Contains(t, stdout, it.secret, "%s reads %s inside the leased tree", what, it.project)
						}
					}
					continue
				}
				for _, it := range d.secrets() {
					assert.NotContains(t, both, it.secret, "%s leaked %s", what, it.project)
				}
				switch c.outside {
				case cmRefusedLocked:
					assert.Equal(t, 1, code, "%s: %s", what, firstN(both, 400))
					assert.Contains(t, stderr, "locked", "%s reports the lock", what)
				case cmRefusedNotFound:
					assert.Equal(t, 1, code, "%s: %s", what, firstN(both, 400))
					assert.Contains(t, stderr, "not found", what)
				case cmFiltered:
					assert.Equal(t, 0, code, "%s: %s", what, firstN(both, 400))
				}
			}
		}
	}

	t.Run("inside the leased tree", func(t *testing.T) { runAll(t, "inside the leased tree", true) })

	t.Run("a lease naming another process", func(t *testing.T) {
		revokeAll()
		lease(other)
		runAll(t, "a lease naming another process", false)
	})

	t.Run("no lease", func(t *testing.T) {
		revokeAll()
		runAll(t, "no lease", false)
	})

	t.Run("an expired lease", func(t *testing.T) {
		lease(self)
		_, err := d.db.Exec(`UPDATE access_leases SET expires_at = ? WHERE revoked_at IS NULL`, core.FormatTime(time.Now().Add(-time.Second)))
		require.NoError(t, err)
		live, err := store.LiveAccessLeases(ctx, d.db, time.Now())
		require.NoError(t, err)
		require.Empty(t, live)
		runAll(t, "an expired lease", false)
	})

	// Nothing the commands did outside the tree moved the planted state.
	d.itemsUnchanged(t, d.fin, true, "the CLI rows")
	d.itemsUnchanged(t, d.legal, true, "the CLI rows")
}

// cmGitRepo makes dir a git repository with one commit, reading no git
// config of the owner's. It reports false when git is not installed.
func cmGitRepo(t *testing.T, dir string) bool {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		return false
	}
	require.NoError(t, os.MkdirAll(dir, 0o755))
	for _, args := range [][]string{
		{"init", "-q"},
		{"-c", "user.name=example", "-c", "user.email=example@example.invalid", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		cmd := exec.Command(git, args...)
		cmd.Dir = dir
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}
	return true
}
