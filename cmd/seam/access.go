package main

// seam access request -- an agent asks the owner for a locked project, or --
// with isolation.requests on -- to read a confidential one.
//
// Under peer_auth a locked project (peer_auth.locked) is closed to every owner
// request until the owner approves a lease for the asking agent's process. A
// refused call already files a pending request by itself; this is the explicit
// form, with the agent's own reason attached. The owner decides it with seam
// approve (approve.go) in their own terminal, or on the console's passkey page.
//
// A confidential project's fence works the same way when the daemon offers
// read grants (isolation.requests): the request asks for read-only access to
// that one project for the asking session's own project.
//
// The routes are the console's /console/access family. They are 404 with a JSON
// message when the daemon locks nothing, and that message is the answer the
// caller needs ("no project is locked"), so these requests surface a 404's body
// rather than consoleRequest's bare "not found".

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/arctop/seamless/internal/config"
)

// maxReasonRunes is the daemon's limit on an access request's reason
// (store.MaxAccessReasonRunes, pinned by a test): refused here before connecting,
// so a reason that cannot be filed never reaches the daemon at all.
const maxReasonRunes = 200

// accessFacts mirrors approval.Facts: everything the owner is shown about one
// request. Every field is the daemon's fact except Reason, which is the agent's
// own words. The keys are pinned against the daemon's struct by a test, since
// this CLI must not import internal/approval (it pulls in SQLite).
type accessFacts struct {
	ID          string          `json:"id"`
	Short       string          `json:"short_id"`
	Project     string          `json:"project"`
	Isolation   string          `json:"isolation"`
	Access      string          `json:"access"`
	Kind        string          `json:"kind"`
	Program     string          `json:"program"`
	PID         int32           `json:"pid"`
	UID         uint32          `json:"uid"`
	Session     string          `json:"session"`
	SessionID   string          `json:"session_id"`
	CWD         string          `json:"cwd"`
	Repo        string          `json:"repo"`
	Model       string          `json:"model"`
	Reason      string          `json:"reason"`
	Warnings    []accessWarning `json:"warnings"`
	RequestedAt time.Time       `json:"requested_at"`
	ExpiresAt   time.Time       `json:"expires_at"`
	Digest      string          `json:"digest"`
	From        string          `json:"from,omitempty"`
}

// accessWarning mirrors approval.Warning.
type accessWarning struct {
	Code string `json:"code"`
	Text string `json:"text"`
}

// terminalApprover mirrors console.TerminalApprover: how the terminal path
// reaches its commit -- the daemon's account, its seamlessd, and its config.
type terminalApprover struct {
	Account   string `json:"account"`
	Seamlessd string `json:"seamlessd"`
	Config    string `json:"config"`
}

// accessList is GET /console/access?format=json.
type accessList struct {
	Requests  []accessFacts     `json:"requests"`
	Terminal  *terminalApprover `json:"terminal"`
	Durations []string          `json:"durations"`
	MaxLease  string            `json:"max_lease"`
}

// accessOne is GET /console/access/{id}?format=json.
type accessOne struct {
	Request   accessFacts       `json:"request"`
	Status    string            `json:"status"`
	Page      string            `json:"page"`
	Terminal  *terminalApprover `json:"terminal"`
	Durations []string          `json:"durations"`
	MaxLease  string            `json:"max_lease"`
	// Advisory marks a read grant request on a daemon without peer_auth:
	// seam approve commits it over the console with the bearer key, and any
	// process holding the key could.
	Advisory bool `json:"advisory"`
}

// accessFiled is POST /console/access/request's answer.
type accessFiled struct {
	ID      string `json:"id"`
	Short   string `json:"short_id"`
	Project string `json:"project"`
	Kind    string `json:"kind"`
	From    string `json:"from"`
	Page    string `json:"page"`
}

// kindGrant is a read grant request's kind (store.AccessKindGrant).
const kindGrant = "grant"

// The paths of the access routes.
const (
	accessListPath    = "/console/access?format=json"
	accessRequestPath = "/console/access/request?format=json"
)

func accessOnePath(ref string) string {
	return "/console/access/" + url.PathEscape(ref) + "?format=json"
}

// accessCall performs one access-route request and returns the body of a 200.
//
// It sends the header set every MCP request of this CLI sends (mcpHeaders): the
// bearer key, this machine's name and the agent process that launched seam. The
// request route needs the last two -- they are how the daemon finds the asking
// agent's session -- and consoleRequest sends neither. form, when non-nil, is the
// URL-encoded body the console's POST routes read.
//
// Any status but 200 is an error carrying the handler's JSON message, 404
// included: "no project is locked" and "no such access request" are the answer,
// where consoleRequest would report a bare "not found".
func accessCall(cfg config.Config, method, path string, form url.Values) ([]byte, error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, cfg.ServerURL()+path, body)
	if err != nil {
		return nil, err
	}
	for k, v := range mcpHeaders(cfg) {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "application/json")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	client, err := cfg.HTTPClient(consoleTimeout)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("console unreachable at %s: %w", cfg.DaemonAddress(), err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(consoleError(resp))
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("unreadable console response from %s: %w", path, err)
	}
	if !json.Valid(b) {
		return nil, fmt.Errorf("unreadable console response from %s: not JSON", path)
	}
	return b, nil
}

// accessGet fetches an access route and decodes it into v.
func accessGet(cfg config.Config, path string, v any) error {
	b, err := accessCall(cfg, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	return decodeConsole(path, b, v)
}

// --- access request ---

var accessRequestCmd = spec("access request", groupAccess,
	"ask the owner for a locked project (from the agent's own shell)",
	exactly(1, "project"), bindAccessRequest, runAccessRequest).withLong(
	`Files an access request for a locked project (peer_auth.locked), naming the
agent that runs this command: run it from the agent's own shell, where seam
names the agent that started it. A refused call files one by itself; this is
the explicit form, with a reason the owner sees labeled as the agent's words
(at most 200 characters).

On a daemon that offers read grants (isolation.requests), a confidential
project can be asked for the same way: read-only, for the asking session's own
project, whose writes then stay inside that project.

Asking again while a request is pending answers with that request. The owner
decides it with seam approve <short id> in their own terminal, or on the
console's passkey page when the daemon names one.`)

type accessRequestOpts struct {
	reason *string
	asJSON *bool
}

func bindAccessRequest(fs *flag.FlagSet) *accessRequestOpts {
	return &accessRequestOpts{
		reason: fs.String("reason", "", "why it needs the project, in the agent's own `TEXT` (at most 200 characters)"),
		asJSON: jsonFlag(fs, jsonUsage),
	}
}

func runAccessRequest(_ context.Context, e *env, o *accessRequestOpts, pos []string) error {
	reason := strings.TrimSpace(*o.reason)
	if n := utf8.RuneCountInString(reason); n > maxReasonRunes {
		return fmt.Errorf("the reason is limited to %d characters (this one has %d)", maxReasonRunes, n)
	}
	cfg, err := e.loadConfig()
	if err != nil {
		return err
	}
	form := url.Values{"project": {pos[0]}}
	if reason != "" {
		form.Set("reason", reason)
	}
	b, err := accessCall(cfg, http.MethodPost, accessRequestPath, form)
	if err != nil {
		return err
	}
	if *o.asJSON {
		return writeRaw(e.stdout, b)
	}
	var got accessFiled
	if err := json.NewDecoder(bytes.NewReader(b)).Decode(&got); err != nil {
		return fmt.Errorf("unreadable console response from %s: %w", accessRequestPath, err)
	}
	if got.Kind == kindGrant {
		fmt.Fprintf(e.stdout, "Requested read access to %s for %s (%s, read-only). The owner approves it with: seam approve %s\n",
			printable(got.Project), printable(got.From), printable(got.Short), printable(got.Short))
	} else {
		fmt.Fprintf(e.stdout, "Requested %s for %s (%s). The owner approves it with: seam approve %s\n",
			printable(got.Short), printable(got.Project), printable(got.Kind), printable(got.Short))
	}
	if got.Page != "" {
		fmt.Fprintf(e.stdout, "or with a passkey: %s\n", printable(got.Page))
	}
	return nil
}
