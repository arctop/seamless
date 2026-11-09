package main

// The proof matrix's daemon (step 10): the real route table over real
// services -- MCP with access_request and the approval service, the hooks,
// the console with its view pool -- behind peerHandler with the store's
// leases, a project the owner has locked, and socket connections whose kernel
// chains the test names. The approval service is the daemon's own but for the
// kernel: Programs is injected, because an injected peer names no real
// executable (approval.Config.Programs; nil would be access.ProgramOf).

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/a2a"
	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/console"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/hooks"
	"github.com/arctop/seamless/internal/mcp"
	"github.com/arctop/seamless/internal/passkey"
	"github.com/arctop/seamless/internal/passkey/passkeytest"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

const (
	pmBoot   = "6c2f1a3e-0b7d-4c55-9a51-3d8f00a1b2c3"
	pmLocked = "example-finance" // confidential and locked
	pmOpen   = "example-ops"
	pmHost   = "laptop"
	pmKey    = "proof-matrix-key"
	pmOrigin = "http://localhost:8081"
	pmUID    = 501
	pmCWD    = "/work/finance" // mapped to pmLocked on pmHost
	// pmSecret is in the locked project's planted task and in no probe: a
	// response carrying it read the locked project.
	pmSecret = "ledger-zx41"

	// The programs the kernel names (approval.Config.Programs).
	pmClaudePath   = "/Users/owner/.local/share/claude/versions/2.1.319"
	pmCodexPath    = "/opt/homebrew/bin/codex"
	pmSeamPath     = "/Users/owner/.local/bin/seam"
	pmZshPath      = "/bin/zsh"
	pmTmuxPath     = "/opt/homebrew/Cellar/tmux/3.8/bin/tmux"
	pmHelperPath   = "/Applications/Seamless Approve.app/Contents/MacOS/Seamless Approve"
	pmTerminalPath = "/System/Applications/Utilities/Terminal.app/Contents/MacOS/Terminal"

	// pmAnchorRemedy is the anchor refusal's second line, word for word
	// (approval/anchor.go anchorRemedy).
	pmAnchorRemedy = "Remedy: run the work from an agent session (Claude Code or Codex), which asks for itself; for a one-off, " +
		"the owner approves that session's request with `seam approve` in their own terminal."
	// pmTouchIDBad is the console's answer to a Touch ID proof that does not
	// verify (console/access.go accessError).
	pmTouchIDBad = "the Touch ID signature does not verify for this request, this duration and these facts with that key: " +
		"approve again from the agent's dialog"
	// pmPasskeyBad is the console's answer to a passkey assertion that does
	// not verify (console/approvers.go approverError).
	pmPasskeyBad = "the passkey could not be verified; try again"
	// pmPasskeyRebound is the console's answer to a passkey assertion made
	// for this request with another duration (console/access.go accessError,
	// approval.ErrBindingChanged).
	pmPasskeyRebound = "the passkey approved this request for another duration, or other facts, than this approval names: " +
		"nothing was approved; reload the page, check the duration and approve again"
	// pmDecided, pmExpired and pmFactsChanged are accessError's 409s.
	pmDecided      = "this request was already decided"
	pmExpired      = "this request expired: the agent can ask again"
	pmFactsChanged = "the request's facts changed since this page loaded: reload and look at them again"
	// pmTouchIDFactsChanged is the same 409 said to the Touch ID helper, which
	// has no page to reload.
	pmTouchIDFactsChanged = "the request changed while Touch ID showed it (a new warning, or the agent's reason filled in), " +
		"so nothing was approved: run seam approve in your own terminal to see it as it stands now, " +
		"or have the agent ask again"
)

var (
	pmShell     = access.ProcessToken{PID: 100, Version: 1000, Boot: pmBoot}
	pmAgent     = access.ProcessToken{PID: 200, Version: 2000, Boot: pmBoot}
	pmSibling   = access.ProcessToken{PID: 210, Version: 2100, Boot: pmBoot} // another agent under the same shell
	pmBridge    = access.ProcessToken{PID: 300, Version: 3000, Boot: pmBoot} // the agent's seam mcp-proxy
	pmSibBridge = access.ProcessToken{PID: 310, Version: 3100, Boot: pmBoot}
	pmCLI       = access.ProcessToken{PID: 320, Version: 3200, Boot: pmBoot} // a seam CLI call the agent's script makes
	pmHelper    = access.ProcessToken{PID: 600, Version: 6000, Boot: pmBoot} // Seamless Approve, launched by launchd

	pmShortRe = regexp.MustCompile(`R-[0-9A-Z]{4}`)
	pmCodeRe  = regexp.MustCompile(`[0-9A-HJKMNP-TV-Z]{4}(-[0-9A-HJKMNP-TV-Z]{4}){3}`)
)

// pmPrograms is the kernel every proof row runs under: it names the agents,
// their shell, their bridges and the Touch ID helper.
func pmPrograms() map[access.ProcessToken]string {
	return map[access.ProcessToken]string{
		pmShell: pmZshPath, pmAgent: pmClaudePath, pmSibling: pmClaudePath,
		pmBridge: pmSeamPath, pmSibBridge: pmSeamPath, pmCLI: pmSeamPath, pmHelper: pmHelperPath,
	}
}

// pmApprovals is the daemon's approval service as newApprovals builds it --
// passkeys on for pmOrigin, the events, the host, the lease cap -- with the
// kernel's program names injected (Programs) and the clock the test may run
// ahead (now).
func pmApprovals(t *testing.T, db *sql.DB, rec *events.Recorder, now func() time.Time, programs func(access.ProcessToken) string) *approval.Service {
	t.Helper()
	v, err := passkey.New(passkey.Config{RPID: "localhost", Origins: []string{pmOrigin}})
	require.NoError(t, err)
	return approval.New(approval.Config{
		DB: db, Passkeys: v, Origins: []string{pmOrigin}, UserName: "owner", UserDisplayName: "Seamless owner on " + pmHost,
		Events: rec, Host: pmHost, LeaseCap: 8 * time.Hour, Programs: programs, Now: now,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

type pmOpts struct {
	// dialogWait bounds the in-session dialog (mcp.Config.DialogWait); zero
	// is a minute, which no row reaches.
	dialogWait time.Duration
	// programs replaces pmPrograms.
	programs map[access.ProcessToken]string
}

// pmDaemon is one daemon of the proof matrix.
type pmDaemon struct {
	t      *testing.T
	db     *sql.DB
	cfg    config.Config
	policy *access.Policy
	svc    *approval.Service
	mux    *http.ServeMux
	tcp    *httptest.Server
	logger *slog.Logger

	mu   sync.Mutex
	skew time.Duration // the approval service's clock runs this far ahead
}

func newPMDaemon(t *testing.T, o pmOpts) *pmDaemon {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "seam.db")
	db, err := storetest.OpenFresh(t, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, store.AdoptLocalHost(ctx, db, pmHost))
	for _, slug := range []string{pmLocked, pmOpen} {
		_, err := store.EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
	}
	require.NoError(t, store.SetProjectIsolation(ctx, db, pmLocked, core.IsolationConfidential))
	require.NoError(t, store.AddRepoMapping(ctx, db, pmCWD, pmLocked))
	require.NoError(t, store.AddRepoMapping(ctx, db, "/work/ops", pmOpen))

	cfg := config.Defaults()
	cfg.DataDir = dir
	cfg.PeerAuth = config.PeerAuth{Enabled: true, Owner: "owner", Locked: []string{pmLocked}}
	policy, err := access.New(cfg.PeerAuth, lockOwnerResolve)
	require.NoError(t, err)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	programs := o.programs
	if programs == nil {
		programs = pmPrograms()
	}
	d := &pmDaemon{t: t, db: db, cfg: cfg, policy: policy, logger: logger}

	mgr, err := files.NewManager(dir, db, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })
	ret := retrieve.New(db, nil, config.Defaults().Budgets, nil)
	ret.SetBodyReader(mgr.Store())
	rec := events.NewRecorder(db)
	d.svc = pmApprovals(t, db, rec, d.now, func(tok access.ProcessToken) string { return programs[tok] })

	wait := o.dialogWait
	if wait == 0 {
		wait = time.Minute
	}
	mcpSrv := mcp.New(mcp.Config{
		DB: db, Files: mgr, Retrieve: ret, Events: rec, APIKey: pmKey, LocalHost: pmHost, Logger: logger,
		Approvals: d.svc, OfferAccessRequest: true, DialogWait: wait, DialogPoll: 5 * time.Millisecond,
	})
	a2aSrv, err := a2a.New(a2a.Config{Retrieve: ret, Events: rec, APIKey: pmKey, Version: "test", Endpoint: "http://127.0.0.1:8081/api/a2a"})
	require.NoError(t, err)
	hooksH := hooks.NewHandler(hooks.Config{
		DB: db, Retrieve: ret, Events: rec, Files: mgr, APIKey: pmKey, LocalHost: pmHost, Logger: logger,
		Approvals: d.svc, RevokeSessionLeases: d.svc.RevokeSession,
	})
	views, err := store.OpenViews(dbPath, policy.OwnerLocked())
	require.NoError(t, err)
	t.Cleanup(func() { _ = views.Close() })
	base, err := views.For(nil)
	require.NoError(t, err)
	consoleSrv, err := console.New(console.Config{
		DB: base, WriteDB: db, Files: mgr, Events: rec.ReadingFrom(base), Retrieve: ret, APIKey: pmKey, DataDir: dir,
		Approvals: d.svc, Terminal: &console.TerminalApprover{Account: "_seamless"}, Views: views, Logger: logger,
	})
	require.NoError(t, err)
	d.mux = daemonRoutes{
		healthz: healthzHandler(db, "instance", nil), mcp: mcpSrv.Handler(), a2a: a2aSrv.Handler(),
		agentCard: a2aSrv.CardHandler(), hooks: hooksH, console: consoleSrv,
	}.mux()
	d.tcp = httptest.NewServer(ownerHandler(policy, d.mux))
	t.Cleanup(func() { closeServer(d.tcp) })
	return d
}

// now is the approval service's clock: real time, run ahead by skew.
func (d *pmDaemon) now() time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	return time.Now().Add(d.skew)
}

func (d *pmDaemon) runAhead(by time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.skew = by
}

// socket serves the daemon on the peer_auth socket to the owner's uid, whose
// connection the kernel traced to chain (the peer first): peerConnContext's
// answer, injected.
func (d *pmDaemon) socket(chain ...access.ProcessToken) string {
	d.t.Helper()
	hops := make([]access.ChainHop, len(chain))
	for i, c := range chain {
		hops[i] = access.ChainHop{Token: c}
	}
	cred := peerCred{uid: pmUID, chain: func() ([]access.ChainHop, error) { return hops, nil }}
	h := peerHandler(d.policy, storeLeases(d.db), d.logger, d.mux)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), peerCredKey{}, cred)))
	}))
	d.t.Cleanup(func() { closeServer(srv) })
	return srv.URL
}

// session records an agent's ambient session the way its SessionStart hook
// does: on pmHost, stamped with its agent process.
func (d *pmDaemon) session(name, project string, proc access.ProcessToken) string {
	d.t.Helper()
	ctx := context.Background()
	id, err := core.NewID()
	require.NoError(d.t, err)
	now := time.Now().UTC()
	cwd := "/work/ops"
	if project == pmLocked {
		cwd = pmCWD
	}
	require.NoError(d.t, store.CreateSession(ctx, d.db, core.Session{
		ID: id, Name: name, ProjectSlug: project, Status: core.SessionActive, Ambient: true, Host: pmHost,
		ExternalSessionID: name + "-x", ExternalClient: "claude-code", CWD: cwd, Model: "example-model",
		CreatedAt: now, UpdatedAt: now,
	}))
	require.NoError(d.t, store.SetSessionAgentProcess(ctx, d.db, id, proc.AgentID()))
	return id
}

// plant puts a task carrying pmSecret in the locked project.
func (d *pmDaemon) plant() {
	d.t.Helper()
	id, err := core.NewID()
	require.NoError(d.t, err)
	now := time.Now().UTC()
	require.NoError(d.t, store.CreateTask(context.Background(), d.db, core.Task{
		ID: id, ProjectSlug: pmLocked, Title: "reconcile the ledger " + pmSecret, Status: core.TaskOpen,
		CreatedBy: "owner", CreatedAt: now, UpdatedAt: now,
	}))
}

// pmOwner is the agent's MCP client standing in for the owner at the
// in-session dialog: it records what it was shown, answers with answer, and
// collects the daemon's asks to launch the Touch ID helper.
type pmOwner struct {
	answer func(context.Context, mcpgo.ElicitationRequest) (*mcpgo.ElicitationResult, error)

	mu    sync.Mutex
	shown []mcpgo.ElicitationRequest
	asks  chan core.ApproverLaunch
}

func newPMOwner(answer func(context.Context, mcpgo.ElicitationRequest) (*mcpgo.ElicitationResult, error)) *pmOwner {
	return &pmOwner{answer: answer, asks: make(chan core.ApproverLaunch, 8)}
}

func (o *pmOwner) Elicit(ctx context.Context, req mcpgo.ElicitationRequest) (*mcpgo.ElicitationResult, error) {
	o.mu.Lock()
	o.shown = append(o.shown, req)
	o.mu.Unlock()
	return o.answer(ctx, req)
}

func (o *pmOwner) dialogs() []mcpgo.ElicitationRequest {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]mcpgo.ElicitationRequest(nil), o.shown...)
}

func pmAnswer(action mcpgo.ElicitationResponseAction, content any) func(context.Context, mcpgo.ElicitationRequest) (*mcpgo.ElicitationResult, error) {
	return func(context.Context, mcpgo.ElicitationRequest) (*mcpgo.ElicitationResult, error) {
		return &mcpgo.ElicitationResult{ElicitationResponse: mcpgo.ElicitationResponse{Action: action, Content: content}}, nil
	}
}

// pmAccepting accepts the form with a duration and a proof method, and a URL
// elicitation (the passkey page) with nothing.
func pmAccepting(duration, method string) func(context.Context, mcpgo.ElicitationRequest) (*mcpgo.ElicitationResult, error) {
	return func(ctx context.Context, req mcpgo.ElicitationRequest) (*mcpgo.ElicitationResult, error) {
		if req.Params.Mode == mcpgo.ElicitationModeURL {
			return pmAnswer(mcpgo.ElicitationResponseActionAccept, nil)(ctx, req)
		}
		return pmAnswer(mcpgo.ElicitationResponseActionAccept, map[string]any{"duration": duration, "method": method})(ctx, req)
	}
}

// pmURLCaps is a client that shows forms and opens URLs.
var pmURLCaps = &mcpgo.ElicitationCapability{Form: &struct{}{}, URL: &struct{}{}}

// dial connects to base's MCP endpoint as the agent proc names (the
// agent-process and host headers its seam sends), with o answering dialogs
// (nil: a client that declares no elicitation) and caps overlaid on what the
// handler declares (nil: the bare `elicitation: {}`, form mode).
func (d *pmDaemon) dial(base string, proc access.ProcessToken, o *pmOwner, caps *mcpgo.ElicitationCapability) *mcpclient.Client {
	d.t.Helper()
	headers := map[string]string{mcp.AgentProcessHeader: proc.AgentID(), mcp.HostHeader: pmHost}
	trans, err := transport.NewStreamableHTTP(base+"/api/mcp", transport.WithHTTPHeaders(headers))
	require.NoError(d.t, err)
	var opts []mcpclient.ClientOption
	if o != nil {
		opts = append(opts, mcpclient.WithElicitationHandler(o))
	}
	cli := mcpclient.NewClient(trans, opts...)
	d.t.Cleanup(func() { _ = cli.Close() })
	if o != nil {
		cli.OnNotification(func(n mcpgo.JSONRPCNotification) {
			if n.Method != core.ApproverNotification {
				return
			}
			b, err := json.Marshal(n.Params.AdditionalFields)
			if err != nil {
				return
			}
			var ask core.ApproverLaunch
			if json.Unmarshal(b, &ask) == nil {
				o.asks <- ask
			}
		})
	}
	ctx := context.Background()
	require.NoError(d.t, cli.Start(ctx))
	var init mcpgo.InitializeRequest
	init.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
	init.Params.ClientInfo = mcpgo.Implementation{Name: "proof-matrix", Version: "0"}
	if caps != nil {
		init.Params.Capabilities.Elicitation = caps
	}
	_, err = cli.Initialize(ctx, init)
	require.NoError(d.t, err)
	return cli
}

// agent is the agent's own MCP connection: its bridge, the agent, its shell.
func (d *pmDaemon) agent(o *pmOwner, caps *mcpgo.ElicitationCapability) *mcpclient.Client {
	d.t.Helper()
	return d.dial(d.socket(pmBridge, pmAgent, pmShell), pmAgent, o, caps)
}

// sibling is another agent's connection under the same shell.
func (d *pmDaemon) sibling() *mcpclient.Client {
	d.t.Helper()
	return d.dial(d.socket(pmSibBridge, pmSibling, pmShell), pmSibling, nil, nil)
}

// refused calls a tool that must be refused and returns its text.
func (d *pmDaemon) refused(cli *mcpclient.Client, name string, args map[string]any) string {
	d.t.Helper()
	text, isErr := paCall(d.t, cli, name, args)
	require.True(d.t, isErr, "%s must be refused: %s", name, text)
	return text
}

// file has cli's tree ask for the locked project the way an agent does --
// a call refused at the lock -- and returns the request it named.
func (d *pmDaemon) file(cli *mcpclient.Client) store.AccessApproval {
	d.t.Helper()
	text := d.refused(cli, "tasks_list", map[string]any{"project": pmLocked})
	short := pmShortRe.FindString(text)
	require.NotEmpty(d.t, short, text)
	a, err := d.svc.Resolve(context.Background(), short)
	require.NoError(d.t, err)
	return a
}

func (d *pmDaemon) row(id string) store.AccessApproval {
	d.t.Helper()
	a, err := store.AccessApprovalByID(context.Background(), d.db, id)
	require.NoError(d.t, err)
	return a
}

func (d *pmDaemon) leases() []store.AccessLease {
	d.t.Helper()
	rows, err := store.LiveAccessLeases(context.Background(), d.db, time.Now())
	require.NoError(d.t, err)
	return rows
}

func (d *pmDaemon) count(query string, args ...any) int {
	d.t.Helper()
	var n int
	require.NoError(d.t, d.db.QueryRow(query, args...).Scan(&n))
	return n
}

// nothing asserts that no lease exists anywhere and that each request stands
// where want says.
func (d *pmDaemon) nothing(why string, want map[string]string) {
	d.t.Helper()
	require.Empty(d.t, d.leases(), "%s: no lease", why)
	require.Zero(d.t, d.count(`SELECT COUNT(*) FROM access_leases`), "%s: no lease row at all", why)
	for id, status := range want {
		require.Equal(d.t, status, d.row(id).Status, "%s: request %s", why, approval.ShortID(id))
	}
}

// requireLease asserts the one live lease: anchor's tree, the locked
// project, sessionID's, and exactly length long.
func (d *pmDaemon) requireLease(anchor access.ProcessToken, sessionID string, length time.Duration) store.AccessLease {
	d.t.Helper()
	leases := d.leases()
	require.Len(d.t, leases, 1)
	l := leases[0]
	require.Equal(d.t, anchor.Key(), l.ProcessKey, "the lease names the agent, never its bridge or shell")
	require.Equal(d.t, pmLocked, l.Project)
	require.Equal(d.t, sessionID, l.SessionID)
	require.Equal(d.t, length, l.ExpiresAt.Sub(l.GrantedAt), "the approved duration")
	return l
}

// requireTreeOpen asserts the lease's reach: the agent's own connection and
// a CLI call its script makes read the locked project over MCP; a sibling
// agent's tree and the TCP key stay locked out.
func (d *pmDaemon) requireTreeOpen(agent, sibling *mcpclient.Client) {
	d.t.Helper()
	text, isErr := paCall(d.t, agent, "tasks_list", map[string]any{"project": pmLocked})
	require.False(d.t, isErr, text)
	require.Contains(d.t, text, pmSecret, "the leased agent reads the locked project")
	cli := d.dial(d.socket(pmCLI, pmAgent, pmShell), pmAgent, nil, nil)
	text, isErr = paCall(d.t, cli, "tasks_list", map[string]any{"project": pmLocked})
	require.False(d.t, isErr, text)
	require.Contains(d.t, text, pmSecret, "a process the agent started is inside the lease")

	text = d.refused(sibling, "tasks_list", map[string]any{"project": pmLocked})
	require.Contains(d.t, text, "project "+pmLocked+" is locked", "a sibling tree stays locked")
	require.NotContains(d.t, text, pmSecret)
	text = d.refused(paClient(d.t, d.tcp.URL, pmKey), "tasks_list", map[string]any{"project": pmLocked})
	require.Contains(d.t, text, "locked", "the TCP key never holds a lease")
	require.NotContains(d.t, text, pmSecret)
}

// do sends one console request and returns its status and body. A form
// sends a POST body; key is the bearer key ("" on the socket, where the
// kernel is the credential).
func pmDo(base, key, method, target string, form url.Values, header map[string]string) (int, string, error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, base+target, body)
	if err != nil {
		return 0, "", err
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Accept", "application/json")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	resp, err := client.Do(req.WithContext(ctx))
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out), err
}

func (d *pmDaemon) do(base, key, method, target string, form url.Values, header map[string]string) (int, string) {
	d.t.Helper()
	status, body, err := pmDo(base, key, method, target, form, header)
	require.NoError(d.t, err)
	return status, body
}

// pmErrorText is a console JSON error's text.
func pmErrorText(t *testing.T, body string) string {
	t.Helper()
	var out struct {
		Error string `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &out), body)
	return out.Error
}

// pmAccessOne is GET /console/access/{id}'s JSON: what seam approve, the
// passkey page and the Touch ID helper read.
type pmAccessOne struct {
	Request        approval.Facts  `json:"request"`
	Status         string          `json:"status"`
	Page           string          `json:"page"`
	PasskeyOptions json.RawMessage `json:"passkey_options"`
	Challenge      string          `json:"challenge"`
}

func pmReadAccess(base, key, ref string) (pmAccessOne, error) {
	return pmReadAccessFor(base, key, ref, "")
}

// pmReadAccessFor reads request ref's page with the duration ?for= chose:
// the passkey options it carries sign that duration ("" chooses none, and
// the page carries none).
func pmReadAccessFor(base, key, ref, duration string) (pmAccessOne, error) {
	path := "/console/access/" + url.PathEscape(ref) + "?format=json"
	if duration != "" {
		path += "&for=" + url.QueryEscape(duration)
	}
	status, body, err := pmDo(base, key, http.MethodGet, path, nil, nil)
	if err != nil {
		return pmAccessOne{}, err
	}
	if status != http.StatusOK {
		return pmAccessOne{}, fmt.Errorf("GET /console/access/%s: %d %s", ref, status, body)
	}
	var one pmAccessOne
	if err := json.Unmarshal([]byte(body), &one); err != nil {
		return pmAccessOne{}, err
	}
	return one, nil
}

func (d *pmDaemon) readAccess(base, key, ref string) pmAccessOne {
	d.t.Helper()
	one, err := pmReadAccess(base, key, ref)
	require.NoError(d.t, err)
	return one
}

// mintCode is `seamlessd approvers enroll-code <kind>`'s core, as the
// daemon's account runs it.
func (d *pmDaemon) mintCode(kind string) string {
	d.t.Helper()
	var out strings.Builder
	require.NoError(d.t, approvers(context.Background(), d.db, d.cfg, &out, []string{"enroll-code", kind}, time.Now()))
	code := pmCodeRe.FindString(out.String())
	require.NotEmpty(d.t, code, out.String())
	return code
}

// pmTouchKey is a software P-256 key playing the helper's Secure Enclave key.
type pmTouchKey struct {
	key *ecdsa.PrivateKey
	id  string // its approver credential id
}

func pmSign(key *ecdsa.PrivateKey, msg []byte) ([]byte, error) {
	dg := sha256.Sum256(msg)
	return ecdsa.SignASN1(rand.Reader, key, dg[:])
}

func pmNewKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	return key
}

// enrollTouchID enrolls a key the way `seam touchid enroll` does: a code the
// daemon's account minted, then the helper's POST on the socket with the
// key's signature over it.
func (d *pmDaemon) enrollTouchID() pmTouchKey {
	d.t.Helper()
	code := d.mintCode(store.ApproverTouchID)
	key := pmNewKey(d.t)
	pub, err := key.PublicKey.Bytes()
	require.NoError(d.t, err)
	sig, err := pmSign(key, approval.TouchIDEnrollMessage(code, pub))
	require.NoError(d.t, err)
	status, body := d.do(d.socket(pmHelper), "", http.MethodPost, "/console/approvers/touchid/enroll", url.Values{
		"name": {"this Mac"}, "code": {code},
		"public_key": {base64.StdEncoding.EncodeToString(pub)}, "signature": {base64.StdEncoding.EncodeToString(sig)},
	}, nil)
	require.Equal(d.t, http.StatusOK, status, body)
	var out struct {
		ID string `json:"id"`
	}
	require.NoError(d.t, json.Unmarshal([]byte(body), &out))
	require.NotEmpty(d.t, out.ID)
	return pmTouchKey{key: key, id: out.ID}
}

func pmNewAuthenticator(t *testing.T) *passkeytest.Authenticator {
	t.Helper()
	a, err := passkeytest.New(true)
	require.NoError(t, err)
	return a
}

// enrollPasskey enrolls a virtual authenticator on the Approvers page's
// ceremony routes, with a code the daemon's account minted.
func (d *pmDaemon) enrollPasskey() *passkeytest.Authenticator {
	d.t.Helper()
	a := pmNewAuthenticator(d.t)
	code := d.mintCode(store.ApproverPasskey)
	status, body := d.do(d.tcp.URL, pmKey, http.MethodPost, "/console/approvers/passkey/begin",
		url.Values{"name": {"security key"}, "code": {code}}, nil)
	require.Equal(d.t, http.StatusOK, status, body)
	var begun approval.Begun
	require.NoError(d.t, json.Unmarshal([]byte(body), &begun))
	resp, err := a.Create(begun.Options, pmOrigin)
	require.NoError(d.t, err)
	status, body = d.do(d.tcp.URL, pmKey, http.MethodPost, "/console/approvers/passkey/finish",
		url.Values{"id": {begun.ID}, "response": {string(resp)}}, nil)
	require.Equal(d.t, http.StatusOK, status, body)
	require.Contains(d.t, body, `"enrolled"`)
	return a
}

// pmTouchIDSignature is the helper's signature over a request it read
// (one), for duration: approval.TouchIDApproveMessage of the daemon's facts.
func pmTouchIDSignature(k *ecdsa.PrivateKey, one pmAccessOne, duration string) ([]byte, error) {
	challenge, err := base64.StdEncoding.DecodeString(one.Challenge)
	if err != nil {
		return nil, err
	}
	if len(challenge) == 0 {
		return nil, errors.New("the request's JSON carried no challenge")
	}
	return pmSign(k, approval.TouchIDApproveMessage(one.Request.ID, challenge, one.Request.Digest, one.Request.Access, duration))
}

// pmTouchIDForm is the helper's approve form (macos Flows.approve).
func pmTouchIDForm(credential string, sig []byte, duration, digest string) url.Values {
	return url.Values{
		"proof": {approval.ProofTouchID}, "credential": {credential},
		"signature": {base64.StdEncoding.EncodeToString(sig)}, "duration": {duration}, "digest": {digest},
	}
}

// pmHelperApprove is the Touch ID helper's whole run on the socket: read the
// request (with its challenge), sign the decision, post it. It reports
// failures as errors, so a goroutine can run it.
func pmHelperApprove(helper string, k pmTouchKey, id, duration string) error {
	one, err := pmReadAccess(helper, "", id)
	if err != nil {
		return err
	}
	sig, err := pmTouchIDSignature(k.key, one, duration)
	if err != nil {
		return err
	}
	status, body, err := pmDo(helper, "", http.MethodPost, "/console/access/"+id+"/approve",
		pmTouchIDForm(k.id, sig, duration, one.Request.Digest), nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("the helper's approve: %d %s", status, body)
	}
	return nil
}

// touchIDPost posts a Touch ID proof for request id on the helper's socket.
func (d *pmDaemon) touchIDPost(helper, id string, form url.Values) (int, string) {
	d.t.Helper()
	return d.do(helper, "", http.MethodPost, "/console/access/"+id+"/approve", form, nil)
}

// assertion is the passkey page's ceremony for request ref and duration: its
// options, issued for that duration (?for=) and read over the console,
// answered by the authenticator from pmOrigin.
func (d *pmDaemon) assertion(a *passkeytest.Authenticator, ref, duration string) (pmAccessOne, []byte) {
	d.t.Helper()
	one, err := pmReadAccessFor(d.tcp.URL, pmKey, ref, duration)
	require.NoError(d.t, err)
	require.NotEmpty(d.t, one.PasskeyOptions, "a pending request's page carries its passkey options for the duration chosen")
	resp, err := a.Get(one.PasskeyOptions, pmOrigin)
	require.NoError(d.t, err)
	return one, resp
}

// passkeyPost posts the passkey page's approve form for request id.
func (d *pmDaemon) passkeyPost(id string, assertion []byte, duration, digest string) (int, string) {
	d.t.Helper()
	return d.do(d.tcp.URL, pmKey, http.MethodPost, "/console/access/"+id+"/approve",
		url.Values{"response": {string(assertion)}, "duration": {duration}, "digest": {digest}}, nil)
}

// pmWithin receives from ch, failing after a generous bound.
func pmWithin[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(20 * time.Second):
		require.FailNow(t, "timed out waiting for "+what)
		var zero T
		return zero
	}
}

func pmLines(s string) []string { return strings.Split(strings.TrimSpace(s), "\n") }

// pmOrList joins choices as the remedies do (access.orList).
func pmOrList(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	case 2:
		return items[0] + ", or " + items[1]
	}
	return strings.Join(items[:len(items)-1], ", ") + ", or " + items[len(items)-1]
}
