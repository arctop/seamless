package console

// Access requests for locked projects (peer_auth.locked): the pending list,
// one request's facts with its passkey approval, and the JSON the terminal
// path (seam approve) reads. Every route here is a 404 unless a project is
// locked (Config.Approvals set) or read grants are on (Config.Grants set,
// isolation.requests): then the same pages carry a confidential project's
// requests and its live grants, and without peer_auth -- the advisory tier --
// the bearer key or the console cookie approves one.
//
// What the routes never do: approve without a proof. The passkey approval is
// an assertion over approval.PasskeyApproveChallenge -- the request's own
// challenge with the facts digest, the access and the duration -- so the page
// issues its options only once the duration is chosen (on the page, or fixed
// by ?for=); the Touch ID approval is the
// helper app's signature over approval.TouchIDApproveMessage, accepted only on
// the peer_auth socket; the terminal approval is not a route at all
// (seamlessd approve, run as the daemon's account). Denying needs no proof --
// it only ever closes a request -- but an agent's socket connection may not
// deny: only the owner's browser (the console cookie) or the TCP key can.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/agentproc"
	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/store"
)

// TerminalApprover tells `seam approve` how to reach the terminal path's
// commit: the daemon's account, its seamlessd, and the config it loads.
type TerminalApprover struct {
	Account   string `json:"account"`
	Seamlessd string `json:"seamlessd"`
	Config    string `json:"config"`
}

type accessListData struct {
	Requests  []approval.Facts  `json:"requests"`
	Terminal  *TerminalApprover `json:"terminal,omitempty"`
	Durations []string          `json:"durations"`
	MaxLease  string            `json:"max_lease"`
	// Leases are the agent trees' live leases, each with Revoke; Unlocks are
	// every browser's live console unlocks, each with Lock again.
	Leases  []leaseRow  `json:"leases"`
	Unlocks []unlockRow `json:"unlocks"`
	// Grants are the live read grants, each with Revoke (isolation.requests
	// on; absent otherwise). Advisory marks the tier without peer_auth, whose
	// approvals the bearer key makes.
	Grants   []grantRow `json:"grants,omitempty"`
	Advisory bool       `json:"advisory,omitempty"`
	// LockOn and GrantsOn say which halves the page shows.
	LockOn   bool `json:"-"`
	GrantsOn bool `json:"-"`
}

// grantRow is one live read grant on the access page.
type grantRow struct {
	ID      string `json:"id"`
	Project string `json:"project"`
	From    string `json:"from"`
	Session string `json:"session,omitempty"`
	// Agent names the holding process as a lease row does; "" in the tier
	// without peer_auth, whose grants a session holds.
	Agent     string    `json:"agent,omitempty"`
	PID       int32     `json:"pid,omitempty"`
	GrantedAt time.Time `json:"granted_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// GrantsView is what the access pages need of the daemon's read grants
// (*approval.Grants, isolation.requests).
type GrantsView interface {
	Advisory() bool
	RequestGrant(ctx context.Context, in approval.GrantInput) (store.AccessApproval, error)
	Approve(ctx context.Context, d approval.Decision) (store.AccessGrant, error)
	RevokeGrant(ctx context.Context, id string) (store.AccessGrant, error)
	Live(ctx context.Context) ([]store.AccessGrant, error)
	accessCore
}

// accessCore is the request side a lock's approvals and the read grants
// share: the same rows, facts and decisions.
type accessCore interface {
	Pending(ctx context.Context) ([]approval.Facts, error)
	Facts(ctx context.Context, id string) (approval.Facts, error)
	Resolve(ctx context.Context, ref string) (store.AccessApproval, error)
	Deny(ctx context.Context, id, by string) error
	LeaseLength(choice string) (time.Duration, error)
	Page(id string) string
	Ways(ctx context.Context, id string) access.Ways
}

// core is the request side this console serves: the lock's approval service
// when a project is locked, else the read grants' own.
func (s *Service) core() accessCore {
	if s.cfg.Approvals != nil {
		return s.cfg.Approvals
	}
	return s.cfg.Grants
}

// accessOff answers a 404 for the access routes when neither a lock nor
// read grants are on -- in the same words as before read grants existed.
func (s *Service) accessOff(w http.ResponseWriter, r *http.Request) bool {
	if s.cfg.Approvals == nil && s.cfg.Grants == nil {
		return s.approversOff(w, r)
	}
	return false
}

// advisory reports the read grants' advisory tier: no peer_auth, the bearer
// key decides.
func (s *Service) advisory() bool { return s.cfg.Grants != nil && s.cfg.Grants.Advisory() }

// leaseRow is one live agent lease on the access page.
type leaseRow struct {
	ID      string `json:"id"`
	Project string `json:"project"`
	Session string `json:"session,omitempty"`
	// Agent names the leased process as seam approve does ("Claude Code
	// 2.1.319 (pid 4242)", approval.AgentLabel), from the approval behind
	// the lease.
	Agent     string    `json:"agent,omitempty"`
	PID       int32     `json:"pid"`
	GrantedAt time.Time `json:"granted_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// unlockRow is one live console unlock on the access page.
type unlockRow struct {
	ID          string    `json:"id"`
	Project     string    `json:"project"`
	By          string    `json:"by"`
	ExpiresAt   time.Time `json:"expires_at"`
	ThisBrowser bool      `json:"this_browser"`
}

type accessOneData struct {
	Request approval.Facts `json:"request"`
	Status  string         `json:"status"`
	Page    string         `json:"page,omitempty"`
	// For is the duration ?for= chose (one of approval.Durations; an invalid
	// one is ignored, and the owner chooses on the page). PasskeyOptions are
	// issued only with one: the assertion signs the duration.
	For            string            `json:"for,omitempty"`
	PasskeyOptions json.RawMessage   `json:"passkey_options,omitempty"`
	Terminal       *TerminalApprover `json:"terminal,omitempty"`
	Durations      []string          `json:"durations"`
	MaxLease       string            `json:"max_lease"`
	// Advisory marks a read grant request in the tier without peer_auth: seam
	// approve commits it over this console with the bearer key (POST
	// /console/access/{id}/approve without a passkey response), and the page
	// offers a plain Approve.
	Advisory bool `json:"advisory,omitempty"`
	// Challenge is the request's challenge, standard base64, for the Touch ID
	// helper to sign over (approval.TouchIDApproveMessage). It is in the JSON
	// of a pending request read over the peer_auth socket only, and never on
	// the page. Handing it out grants nothing: it is a nonce every proof must
	// cover, not a secret -- the passkey options above carry the same bytes to
	// the browser -- and a proof over it still needs the owner's passkey or
	// the Secure Enclave key, both of which ask for the owner's presence.
	Challenge string `json:"challenge,omitempty"`
	// OnOrigin and Origin are for the page: a passkey ceremony runs only on
	// the relying party's origin.
	OnOrigin bool   `json:"-"`
	Origin   string `json:"-"`
	// PasskeyReady says a passkey can approve the pending request here
	// (passkeys set up, an origin, one enrolled); ForWords is For as the page
	// says it.
	PasskeyReady bool   `json:"-"`
	ForWords     string `json:"-"`
	// Choices are the duration radios, the default first, each worded as
	// durationWords says it: a choice max_lease cuts short reads as the cap.
	Choices []durationChoice `json:"-"`
}

// durationChoice is one duration radio: its value and its words.
type durationChoice struct{ Value, Words string }

// durationChoices words every duration the daemon offers, the default first.
func durationChoices(maxLease string) []durationChoice {
	out := make([]durationChoice, len(approval.Durations))
	for i, c := range approval.Durations {
		out[i] = durationChoice{Value: c, Words: durationWords(c, maxLease)}
	}
	return out
}

// chosenDuration is the duration ?for= names, "" when it names none or one
// the daemon does not offer: then the owner chooses on the page.
func chosenDuration(r *http.Request) string {
	if v := r.URL.Query().Get("for"); slices.Contains(approval.Durations, v) {
		return v
	}
	return ""
}

// durationWords is a duration choice as the page's choices word it. A fixed
// length max_lease cuts short reads as the cap, as seam approve says it:
// "20m (the max_lease cap)".
func durationWords(choice, maxLease string) string {
	fixed := map[string]time.Duration{approval.For1h: time.Hour, approval.For4h: 4 * time.Hour}
	switch d, ok := fixed[choice]; {
	case choice == approval.ForSession:
		return "until this session ends (at most " + leaseWords(maxLease) + ")"
	case !ok:
		return choice
	case leaseCut(d, maxLease):
		return leaseWords(maxLease) + " (the max_lease cap)"
	case d == time.Hour:
		return "1 hour"
	default:
		return fmt.Sprintf("%d hours", int(d/time.Hour))
	}
}

// leaseCut reports whether max_lease (a Go duration) is shorter than d.
func leaseCut(d time.Duration, maxLease string) bool {
	limit, err := time.ParseDuration(maxLease)
	return err == nil && limit > 0 && limit < d
}

// leaseWords is a lease length as a page says it: Go's 8h0m0s reads 8h, and
// 1h30m0s reads 1h30m. Anything that does not parse is shown as it is.
func leaseWords(d string) string {
	v, err := time.ParseDuration(d)
	if err != nil || v <= 0 {
		return d
	}
	out := v.String()
	if strings.HasSuffix(out, "m0s") {
		out = strings.TrimSuffix(out, "0s")
	}
	if strings.HasSuffix(out, "h0m") {
		out = strings.TrimSuffix(out, "0m")
	}
	return out
}

func (s *Service) maxLease() string {
	d, err := s.core().LeaseLength(approval.ForSession)
	if err != nil {
		return ""
	}
	return d.String()
}

func (s *Service) accessList(w http.ResponseWriter, r *http.Request) {
	if s.accessOff(w, r) {
		return
	}
	pending, err := s.core().Pending(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if pending == nil {
		pending = []approval.Facts{}
	}
	d := accessListData{Requests: pending, Terminal: s.cfg.Terminal, Durations: approval.Durations, MaxLease: s.maxLease(),
		LockOn: s.cfg.Approvals != nil, GrantsOn: s.cfg.Grants != nil, Advisory: s.advisory()}
	if d.LockOn {
		if d.Leases, d.Unlocks, err = s.accessGrants(r.Context()); err != nil {
			s.serverError(w, r, err)
			return
		}
	} else {
		d.Leases, d.Unlocks = []leaseRow{}, []unlockRow{}
	}
	if d.GrantsOn {
		if d.Grants, err = s.readGrants(r.Context()); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	s.render(w, r, "access", pageData{Title: "Access requests", Active: "access", Data: d})
}

// readGrants lists the live read grants: metadata about confidential
// projects (slugs, ids, times), which no console handle hides.
func (s *Service) readGrants(ctx context.Context) ([]grantRow, error) {
	live, err := s.cfg.Grants.Live(ctx)
	if err != nil {
		return nil, err
	}
	rows := make([]grantRow, 0, len(live))
	for _, g := range live {
		row := grantRow{ID: g.ID, Project: g.Project, From: g.FromProject, PID: g.PeerPID, GrantedAt: g.GrantedAt, ExpiresAt: g.ExpiresAt,
			Agent: approval.AgentLabel("", g.PeerPID)}
		if a, err := store.AccessApprovalByID(ctx, s.writeDB(), g.ApprovalID); err == nil {
			row.Session, row.Agent = a.SessionName, approval.AgentLabel(a.Program, g.PeerPID)
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// accessGrants lists what is open right now: the agent trees' live leases and
// every browser's live console unlocks. Both are metadata about locked
// projects (slugs, ids, times), read from tables no handle hides.
func (s *Service) accessGrants(ctx context.Context) ([]leaseRow, []unlockRow, error) {
	now := time.Now()
	leases, err := store.LiveAccessLeases(ctx, s.writeDB(), now)
	if err != nil {
		return nil, nil, err
	}
	lrows := make([]leaseRow, 0, len(leases))
	for _, l := range leases {
		row := leaseRow{ID: l.ID, Project: l.Project, PID: l.PeerPID, GrantedAt: l.GrantedAt, ExpiresAt: l.ExpiresAt,
			Agent: approval.AgentLabel("", l.PeerPID)}
		if a, err := store.AccessApprovalByID(ctx, s.writeDB(), l.ApprovalID); err == nil {
			row.Session, row.Agent = a.SessionName, approval.AgentLabel(a.Program, l.PeerPID)
		}
		lrows = append(lrows, row)
	}
	unlocks, err := store.AllLiveConsoleUnlocks(ctx, s.writeDB(), now)
	if err != nil {
		return nil, nil, err
	}
	creds, err := store.ApproverCredentials(ctx, s.writeDB(), "")
	if err != nil {
		return nil, nil, err
	}
	names := make(map[string]string, len(creds))
	for _, c := range creds {
		names[c.ID] = c.Name
	}
	var mine string
	if rc := reachOf(ctx); rc != nil {
		mine = rc.token
	}
	urows := make([]unlockRow, 0, len(unlocks))
	for _, u := range unlocks {
		by := "a removed passkey"
		switch {
		case u.Credential == store.ConsoleUnlockByDaemon:
			by = "the daemon's account (console-open)"
		case names[u.Credential] != "":
			by = names[u.Credential]
		}
		urows = append(urows, unlockRow{ID: u.ID, Project: u.Project, By: by, ExpiresAt: *u.ExpiresAt,
			ThisBrowser: mine != "" && u.TokenHash == mine})
	}
	return lrows, urows, nil
}

func (s *Service) accessOne(w http.ResponseWriter, r *http.Request) {
	if s.accessOff(w, r) {
		return
	}
	a, err := s.core().Resolve(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrApprovalNotFound) {
		s.notFound(w, r, "no such access request")
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	facts, err := s.core().Facts(r.Context(), a.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	// A request past its wait is expired whether or not a sweep has written
	// that yet: the page must not offer to decide it.
	status := a.Status
	if status == store.ApprovalPending && !time.Now().Before(a.ExpiresAt) {
		status = store.ApprovalExpired
	}
	d := accessOneData{Request: facts, Status: status, Page: s.core().Page(a.ID), Terminal: s.cfg.Terminal,
		Durations: approval.Durations, MaxLease: s.maxLease(), Advisory: a.Kind == store.AccessKindGrant && s.advisory()}
	d.Choices = durationChoices(d.MaxLease)
	if status == store.ApprovalPending && access.PeerAuthenticated(r.Context()) && wantsJSON(r) {
		d.Challenge = base64.StdEncoding.EncodeToString(a.Challenge)
	}
	if s.cfg.Approvals == nil {
		// The advisory tier: no passkeys, no terminal commit through sudo.
		s.render(w, r, "access_one", pageData{Title: "Access request " + facts.Short, Active: "access", Data: d})
		return
	}
	if origins := s.cfg.Approvals.Origins(); len(origins) > 0 {
		d.Origin = origins[0]
		d.OnOrigin = containsString(origins, requestOrigin(r))
	}
	if status == store.ApprovalPending && s.cfg.Approvals.PasskeysOn() {
		// The remedies' own rule (approval.Service.Ways): passkeys set up, an
		// origin to verify them on, and one enrolled -- so the page never
		// offers a passkey the remedy does not, nor a link with no origin.
		d.PasskeyReady = s.cfg.Approvals.Ways(r.Context(), "").Passkey
		// The options only once the duration is chosen: they sign it, with
		// the facts this page shows.
		if d.For = chosenDuration(r); d.For != "" {
			d.ForWords = durationWords(d.For, d.MaxLease)
			if d.PasskeyReady {
				// Options over the binding of the request, the chosen
				// duration and the facts digest this page shows.
				if opts, err := s.cfg.Approvals.AssertionOptionsFor(r.Context(), a.ID, d.For, facts.Digest); err == nil {
					d.PasskeyOptions = opts
				} else {
					s.logger.Debug("console: passkey options", "request", facts.Short, "error", err)
				}
			}
		}
	}
	s.render(w, r, "access_one", pageData{Title: "Access request " + facts.Short, Active: "access", Data: d})
}

func containsString(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// accessRequest files a request explicitly: `seam access request`, run from
// the agent's own shell, so the kernel's chain and the agent-process header
// name the agent.
func (s *Service) accessRequest(w http.ResponseWriter, r *http.Request) {
	if s.accessOff(w, r) {
		return
	}
	v, _ := access.FromContext(r.Context())
	proc := strings.TrimSpace(r.Header.Get("X-Seamless-Agent-Process"))
	if !agentproc.Valid(proc) {
		proc = ""
	}
	project := strings.TrimSpace(r.PostForm.Get("project"))
	host := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Seamless-Host")))
	// A read grant is for a project no lock covers: a locked project this
	// tree holds a lease on is still the lock's (a live lease answers, or a
	// refresh is filed).
	_, leased := v.Lease(project)
	if s.cfg.Grants != nil && !access.Locked(r.Context(), project) && !leased {
		s.grantRequest(w, r, project, proc, host)
		return
	}
	if s.cfg.Approvals == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "that project is not confidential: nothing to request"})
		return
	}
	a, err := s.cfg.Approvals.Request(r.Context(), approval.RequestInput{
		Project: project, View: v, AgentProcess: proc,
		Host:   host,
		Reason: r.PostForm.Get("reason"), Via: approval.ViaRequest,
	})
	if err != nil {
		msg := err.Error()
		var anchor *approval.AnchorError
		var live *approval.AccessLiveError
		switch {
		case errors.As(err, &anchor):
			// Its own two lines, the rule and the remedy: no request exists.
			msg = anchor.Error()
		case errors.As(err, &live):
			msg = live.Answer()
		case errors.Is(err, approval.ErrNoProcess):
			msg = "this connection has no verified process: request access from an agent session whose seam dials the peer_auth socket"
		case errors.Is(err, approval.ErrNoAnchor), errors.Is(err, approval.ErrAnchorNotInChain):
			msg = "no agent session names this process: run it from the agent's own shell, where seam names the agent that started it"
		case errors.Is(err, approval.ErrNotLocked):
			msg = "that project is not locked: nothing to request"
		case errors.Is(err, store.ErrInvalidAccessRequest):
			msg = "the reason is limited to 200 characters"
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	// The passkey page only when a passkey can approve the request there.
	writeJSON(w, http.StatusOK, map[string]any{
		"id": a.ID, "short_id": approval.ShortID(a.ID), "project": a.Project, "kind": a.Kind,
		"page": s.cfg.Approvals.Ways(r.Context(), a.ID).Page,
	})
}

// accessApprove approves a request with a passkey assertion over its binding
// (approval.PasskeyApproveChallenge) for the duration and the facts digest
// the page showed, both resent with the form. A
// read grant request goes to the read grants, where -- in the advisory tier
// only -- a decision without a passkey response is the bearer key's.
//
// proof=touchid is the Touch ID helper's form instead: credential names the
// enrolled key and signature is its signature over the decision, and the same
// approve core checks it, for a lock's request and a read grant alike. It is
// accepted on the peer_auth socket only, the one transport the helper uses.
func (s *Service) accessApprove(w http.ResponseWriter, r *http.Request) {
	if s.accessOff(w, r) {
		return
	}
	touch, ok, err := touchIDProof(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if ok && !access.PeerAuthenticated(r.Context()) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": errTouchIDOffSocket.Error()})
		return
	}
	a, err := s.core().Resolve(r.Context(), r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such access request"})
		return
	}
	if a.Kind == store.AccessKindGrant {
		if ok {
			s.grantApproveTouchID(w, r, a, touch)
			return
		}
		s.grantApprove(w, r, a)
		return
	}
	if s.cfg.Approvals == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such access request"})
		return
	}
	proof := approval.Proof{Kind: approval.ProofPasskey, Assertion: []byte(r.PostForm.Get("response"))}
	if ok {
		proof = touch
	}
	lease, err := s.cfg.Approvals.Approve(r.Context(), approval.Decision{
		ID: a.ID, Duration: r.PostForm.Get("duration"), Digest: r.PostForm.Get("digest"), Proof: proof,
	})
	if err != nil {
		s.accessError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"approved": approval.ShortID(a.ID), "project": a.Project, "expires_at": lease.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

// proofTouchIDValue is the form value that selects the Touch ID proof.
const proofTouchIDValue = approval.ProofTouchID

// errTouchIDOffSocket is a Touch ID proof that did not arrive on the
// peer_auth socket.
var errTouchIDOffSocket = errors.New("a Touch ID approval is accepted on the peer_auth socket only: " +
	"approve from the agent's dialog, or with seam approve in the owner's terminal")

// touchIDProof reads the Touch ID helper's proof from an approve form: ok is
// false when the form names no proof, which keeps the passkey (or, in the
// advisory tier, the bearer key's) decision exactly as before. A proof that is
// present but empty or unknown, a missing credential, and a signature that is
// not standard base64 are refused, never read as absent.
func touchIDProof(r *http.Request) (approval.Proof, bool, error) {
	if !r.PostForm.Has("proof") {
		return approval.Proof{}, false, nil
	}
	if kind := r.PostForm.Get("proof"); kind != proofTouchIDValue {
		return approval.Proof{}, false, fmt.Errorf("invalid proof %q: the valid value is %s (leave proof out to approve with a passkey response)",
			kind, proofTouchIDValue)
	}
	cred := strings.TrimSpace(r.PostForm.Get("credential"))
	if cred == "" {
		return approval.Proof{}, false, errors.New("a Touch ID approval names its key: credential is the enrolled key's id")
	}
	sig, err := base64.StdEncoding.Strict().DecodeString(r.PostForm.Get("signature"))
	if err != nil || len(sig) == 0 {
		return approval.Proof{}, false, errors.New("signature must be the Touch ID key's ASN.1 DER signature in standard base64")
	}
	return approval.Proof{Kind: approval.ProofTouchID, CredentialID: cred, Signature: sig}, true, nil
}

// grantApproveTouchID decides a read grant request with a Touch ID proof
// under peer_auth: the read grants' approve, with the answer grantApprove
// gives.
func (s *Service) grantApproveTouchID(w http.ResponseWriter, r *http.Request, a store.AccessApproval, proof approval.Proof) {
	if s.cfg.Grants == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "read grants are off (isolation.requests)"})
		return
	}
	grant, err := s.cfg.Grants.Approve(r.Context(), approval.Decision{
		ID: a.ID, Duration: r.PostForm.Get("duration"), Digest: r.PostForm.Get("digest"), Proof: proof,
	})
	if err != nil {
		s.accessError(w, r, err)
		return
	}
	afterAccessWrite(w, r, map[string]any{
		"approved": approval.ShortID(a.ID), "project": a.Project, "from": a.FromProject, "grant": grant.ID,
		"expires_at": grant.ExpiresAt.UTC().Format(time.RFC3339),
	}, "Approved "+approval.ShortID(a.ID)+": "+a.Project+" is readable from "+a.FromProject+", read-only, until "+
		grant.ExpiresAt.Local().Format("15:04")+".")
}

// accessDeny closes a request without a lease. An agent's socket connection
// may not deny: the owner's browser or the TCP key only.
func (s *Service) accessDeny(w http.ResponseWriter, r *http.Request) {
	if s.accessOff(w, r) {
		return
	}
	if access.PeerAuthenticated(r.Context()) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "deny from the console, or with seam approve in the owner's terminal"})
		return
	}
	a, err := s.core().Resolve(r.Context(), r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such access request"})
		return
	}
	if err := s.core().Deny(r.Context(), a.ID, "console"); err != nil {
		s.accessError(w, r, err)
		return
	}
	payload := map[string]any{"denied": approval.ShortID(a.ID), "project": a.Project}
	if s.cfg.Approvals == nil {
		// The advisory tier's page denies with a plain form.
		afterAccessWrite(w, r, payload, "Denied "+approval.ShortID(a.ID)+": "+a.Project+" stays closed to that session.")
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *Service) accessError(w http.ResponseWriter, r *http.Request, err error) {
	msg := ""
	switch {
	case errors.Is(err, approval.ErrBindingChanged):
		// The passkey signed this request for another duration (or over the
		// bare challenge an older page asked for): what it approved is not
		// what was sent.
		msg = "the passkey approved this request for another duration, or other facts, than this approval names: " +
			"nothing was approved; reload the page, check the duration and approve again"
	case errors.Is(err, approval.ErrFactsChanged) && r.PostForm.Get("proof") == proofTouchIDValue:
		// The Touch ID helper, which has no page to reload: it showed the
		// facts in its prompt, and its call has ended by the time it says so.
		msg = "the request changed while Touch ID showed it (a new warning, or the agent's reason filled in), " +
			"so nothing was approved: run seam approve in your own terminal to see it as it stands now, " +
			"or have the agent ask again"
	case errors.Is(err, approval.ErrFactsChanged):
		msg = "the request's facts changed since this page loaded: reload and look at them again"
	case errors.Is(err, store.ErrApprovalNotPending):
		msg = "this request was already decided"
	case errors.Is(err, store.ErrApprovalExpired):
		msg = "this request expired: the agent can ask again"
	case errors.Is(err, approval.ErrDuration):
		msg = "choose a duration: until the session ends, 1 hour or 4 hours"
	case errors.Is(err, approval.ErrNoProof), errors.Is(err, approval.ErrPasskeysOff), errors.Is(err, approval.ErrPeerProof):
		msg = "approving needs a passkey here, or seam approve in the owner's terminal"
	case errors.Is(err, approval.ErrTouchIDKey):
		// The helper's signature, or the key it names: refused like a passkey
		// assertion that does not verify.
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the Touch ID signature does not verify for this request, " +
			"this duration and these facts with that key: approve again from the agent's dialog"})
		return
	case errors.Is(err, approval.ErrSealed):
		msg = "a sealed project is never granted, and never granted to: deny the request"
	case errors.Is(err, approval.ErrNotConfidential):
		msg = "that project is no longer confidential: nothing to grant"
	case errors.Is(err, approval.ErrBroadAnchor):
		msg = "this request names a process that is not an agent (a shell, terminal, multiplexer or login process): " +
			"approving it would open the project to every program it started, so it is never approved; deny it"
	}
	if msg != "" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": msg})
		return
	}
	s.approverError(w, r, err)
}
