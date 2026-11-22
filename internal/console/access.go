package console

// Access requests for locked projects: the pending list,
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
// issues its options for the duration it shows selected (?for=, else the
// first choice) and, where a grant offers read and write, only once the
// access is chosen; the Touch ID approval is the
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
	"path"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

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
	// Origin is the passkey relying party's origin, while a project is locked:
	// where a browser signs in for a passkey ceremony. An owner's client
	// config holds no peer_auth block to work it out from, so console-open
	// asks here, and the console's cookie stays on the host passkeys work on.
	Origin string `json:"origin,omitempty"`
	// LockOn and GrantsOn say which halves the page shows.
	LockOn   bool `json:"-"`
	GrantsOn bool `json:"-"`
	// Open is everything open right now in one list: the live leases, read
	// grants and console unlocks above, what ends first first (UX 41).
	Open []axOpenRow `json:"open"`
	// Cards are the waiting requests as the page draws them. OpenShown is
	// Open under the kind filter ?open= chose (Open itself while the filter
	// is not offered), Kinds the filter's chips, offered only past
	// axOpenFilterAt rows, and OpenKind the kind chosen ("" for all).
	// OpenHint says what each kind of row is, for the tiers this daemon has.
	Cards     []axCard     `json:"-"`
	OpenShown []axOpenRow  `json:"-"`
	Kinds     []axOpenKind `json:"-"`
	OpenKind  string       `json:"-"`
	OpenHint  string       `json:"-"`
}

// The kinds of row the Open right now list holds.
const (
	axOpenLease  = "lease"
	axOpenGrant  = "grant"
	axOpenUnlock = "unlock"
)

// axOpenFilterAt is how many rows the open list holds before it offers its kind
// filter.
const axOpenFilterAt = 5

// axOpenRow is one row of the Open right now list: a live lease, read grant or
// console unlock. The JSON names the row; the rest is how the page words it.
type axOpenRow struct {
	Kind      string    `json:"kind"`
	ID        string    `json:"id"`
	Project   string    `json:"project"`
	ExpiresAt time.Time `json:"expires_at"`
	// Who holds it: the agent and its pid, the session, or this or another
	// browser; For is a grant's asking project.
	Who string `json:"-"`
	For string `json:"-"`
	// The sub line: the session, the access a grant opened, when and how it
	// was approved, and where a grant's holder may write from now on (an
	// unlock says who unlocked it instead).
	Session   string    `json:"-"`
	Access    string    `json:"-"`
	GrantedAt time.Time `json:"-"`
	How       string    `json:"-"`
	Confined  string    `json:"-"`
	By        string    `json:"-"`
	// Its one action: the route that ends it, the button's word, and the
	// sentence it confirms with in the page.
	Action  string `json:"-"`
	Button  string `json:"-"`
	Confirm string `json:"-"`
}

// Icon is the row's kind icon.
func (o axOpenRow) Icon() string {
	switch o.Kind {
	case axOpenUnlock:
		return "monitor"
	case axOpenGrant:
		return "link"
	}
	return "bot"
}

// axOpenKind is one chip of the open list's kind filter.
type axOpenKind struct {
	Kind, Label, Href string
	N                 int
	On                bool
}

// axCard is one waiting request as the list draws it: its facts, who
// asks, what for, the reason on one line, and its warnings as one hint.
type axCard struct {
	approval.Facts
	Who, Asks, ReasonLine, WarnText string
	LockedChip                      bool
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
	// Access is what the owner approved: read, or write (read and write).
	Access string `json:"access"`
	// Dialog marks a grant approved in the agent's in-session dialog (the
	// advisory tier's Approve there, approval.DecidedByDialog); absent for
	// every other decision.
	Dialog bool `json:"dialog,omitempty"`
	// decidedBy is the grant's deciding proof, for the open list's words.
	decidedBy string
}

// GrantsView is what the access pages need of the daemon's read grants
// (*approval.Grants, isolation.requests).
type GrantsView interface {
	Advisory() bool
	RequestGrant(ctx context.Context, in approval.GrantInput) (store.AccessApproval, error)
	// Ask files the kind the asker's home and the target call for (a move,
	// or a grant asking for read and write): `seam access request --write`.
	Ask(ctx context.Context, in approval.AskInput) (store.AccessApproval, error)
	Approve(ctx context.Context, d approval.Decision) (store.AccessGrant, error)
	// ApproveMove decides a move request (isolation.requests).
	ApproveMove(ctx context.Context, d approval.Decision) (approval.MoveApproved, error)
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
// read grants are on.
func (s *Service) accessOff(w http.ResponseWriter, r *http.Request) bool {
	if s.cfg.Approvals == nil && s.cfg.Grants == nil {
		return s.lockRoutesOff(w, r, accessOffWords)
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
	// decidedBy is the deciding proof of the approval behind the lease, for
	// the open list's words.
	decidedBy string
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
	// For is the duration the passkey options sign, the one the page shows
	// selected: the one ?for= chose, else the first choice, the default (an
	// invalid ?for= is ignored). PasskeyOptions are issued only with one: the
	// assertion signs the duration.
	For            string          `json:"for,omitempty"`
	PasskeyOptions json.RawMessage `json:"passkey_options,omitempty"`
	// PreFor is that duration for a grant whose access is still to choose:
	// the page keeps it selected, and issues no options, rather than signing
	// read-only.
	PreFor    string            `json:"-"`
	Terminal  *TerminalApprover `json:"terminal,omitempty"`
	Durations []string          `json:"durations"`
	MaxLease  string            `json:"max_lease"`
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
	// Access is the access ?access= chose, one of the request's
	// Facts.AccessChoices ("" for the default, read-only): the passkey options
	// sign it with the duration. Accesses are the access radios, the default
	// first, where the request offers a choice.
	Access   string         `json:"access,omitempty"`
	Accesses []accessChoice `json:"-"`
	// ForGood marks a move into a project no lock covers: it never ends, so
	// the page asks no duration (the proof still signs the default one).
	ForGood bool `json:"-"`
	// InDialog marks a request decided in the agent's in-session dialog (the
	// advisory tier's Approve or Deny there, approval.DecidedByDialog).
	InDialog bool `json:"-"`
	// Granted is what an approved grant request opened (nil for every other
	// request): the facts above are the request as asked, whose access is the
	// default, read-only, even when the owner approved read and write.
	Granted *grantedView `json:"granted,omitempty"`

	// How the card words the request (UX 41): Who is the program the kernel
	// names (the agent's name, else the program's base name), Asks what it
	// asks to do, KindWords its kind, and LockedChip whether the head says
	// locked. Selected is the duration the segments show selected -- For,
	// or PreFor while the access is still to choose -- and ApproveWords the
	// Approve button's. CapNote says how long "until its session ends" can
	// last.
	Who          string `json:"-"`
	Asks         string `json:"-"`
	KindWords    string `json:"-"`
	LockedChip   bool   `json:"-"`
	Selected     string `json:"-"`
	ApproveWords string `json:"-"`
	CapNote      string `json:"-"`
	// AccessNote is the access segments' hint, where the request offers a
	// choice. WarnLines are the warnings as the page's strip says them, one
	// sentence each.
	AccessNote string   `json:"-"`
	WarnLines  []string `json:"-"`
}

// grantedView is the grant an approved request opened: its access, when it
// ends or ended, and the page's line about it.
type grantedView struct {
	Access    string     `json:"access"`
	ExpiresAt time.Time  `json:"expires_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
	// Words is Access as the facts word it; Line says what the approval
	// opened and until when.
	Words string `json:"-"`
	Line  string `json:"-"`
}

// grantedOf is the view of grant g, which request a opened, at now.
func grantedOf(a store.AccessApproval, g store.AccessGrant, now time.Time) *grantedView {
	v := &grantedView{Access: g.Access, ExpiresAt: g.ExpiresAt, RevokedAt: g.RevokedAt, Words: "read-only"}
	if g.Access == store.GrantAccessWrite {
		v.Words = "read and write"
	}
	end := g.ExpiresAt
	if g.RevokedAt != nil && g.RevokedAt.Before(end) {
		end = *g.RevokedAt
	}
	if now.Before(end) {
		v.Line = grantNotice(a, g)
		return v
	}
	v.Line = "Approved " + approval.ShortID(a.ID) + " as " + v.Words + " for " + a.FromProject +
		"; the grant ended at " + end.Local().Format("15:04") + "."
	return v
}

// accessChoice is one access radio: its value and its words.
type accessChoice struct{ Value, Words string }

// Label is the choice as a segment says it; Words is its hint's sentence.
func (c accessChoice) Label() string {
	if c.Value == accessReadWrite {
		return "Read and write"
	}
	return "Read-only"
}

// accessNote is the access segments' hint: what each choice lets happen, as
// the choices word it, one sentence each.
func accessNote(choices []accessChoice) string {
	parts := make([]string, 0, len(choices))
	for _, c := range choices {
		parts = append(parts, axSentence(c.Words))
	}
	return strings.Join(parts, " ")
}

// accessChoices words a request's access choices, the default first:
// read-only, then read and write with what it adds.
func accessChoices(f approval.Facts) []accessChoice {
	var out []accessChoice
	for _, c := range f.AccessChoices {
		words := "read-only (the default): " + accessCopyLine(f)
		if c == accessReadWrite {
			words = "read and write: " + f.ReadWrite
		}
		out = append(out, accessChoice{Value: c, Words: words})
	}
	return out
}

// accessReadWrite is a grant's read-and-write choice (approval's word).
const accessReadWrite = "read-write"

// accessCopyLine is a grant's copy line, the facts' copy_into warning.
func accessCopyLine(f approval.Facts) string {
	for _, w := range f.Warnings {
		if w.Code == approval.WarnCopyInto {
			return w.Text
		}
	}
	return ""
}

// chosenAccess is the access ?access= names, "" when it names none or one
// the request does not offer: then the default, read-only, stands.
func chosenAccess(r *http.Request, f approval.Facts) string {
	if v := r.URL.Query().Get("access"); slices.Contains(f.AccessChoices, v) {
		return v
	}
	return ""
}

// durationChoice is one duration radio: its value and its words.
type durationChoice struct{ Value, Words string }

// axCapWords is how durationWords marks a length max_lease cuts short.
const axCapWords = " (the max_lease cap)"

// Label is the choice as a segment says it: "Until its session ends", "1
// hour", "4 hours", or a length max_lease cut short as the cap's length
// ("20m"). The cap itself is the segments' hint (accessOneData.CapNote).
func (c durationChoice) Label() string {
	if c.Value == approval.ForSession {
		return "Until its session ends"
	}
	return strings.TrimSuffix(c.Words, axCapWords)
}

// axApproveWords is the Approve button's words for the duration it signs:
// "until its session ends", "for 1 hour", "for 20m".
func axApproveWords(c durationChoice) string {
	if c.Value == approval.ForSession {
		return "until its session ends"
	}
	return "for " + c.Label()
}

// axOfferedDuration is the duration the page shows selected for choice, the
// duration ?for= named ("" for none): the first choice, the default, when it
// names none. A fixed length the choices leave out (max_lease reaches it
// sooner) grants what the last choice, the cap, grants, so it reads as that.
func axOfferedDuration(choice string, choices []durationChoice) durationChoice {
	if len(choices) == 0 {
		return durationChoice{}
	}
	if choice == "" {
		return choices[0]
	}
	for _, c := range choices {
		if c.Value == choice {
			return c
		}
	}
	return choices[len(choices)-1]
}

// durationChoices words the durations the daemon offers, the default first,
// as max_lease grants them: the fixed lengths up to the first one that
// reaches the cap, which reads as the cap when it is cut short. A longer one
// would grant the same, so it is not offered twice (the approval dialog's
// choices are the same, internal/mcp durationChoices).
func durationChoices(maxLease string) []durationChoice {
	limit, err := time.ParseDuration(maxLease)
	capped := err == nil && limit > 0
	fixed := map[string]time.Duration{approval.For1h: time.Hour, approval.For4h: 4 * time.Hour}
	var out []durationChoice
	for _, c := range approval.Durations {
		out = append(out, durationChoice{Value: c, Words: durationWords(c, maxLease)})
		if d, ok := fixed[c]; ok && capped && d >= limit {
			break
		}
	}
	return out
}

// chosenDuration is the duration ?for= names, "" when it names none or one
// the daemon does not offer: then the first choice, the default, stands
// (axOfferedDuration).
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

// axCapNote is the duration segments' hint: how long "until its session ends"
// lasts at most, "" when the cap is not known.
func axCapNote(maxLease string) string {
	if limit, err := time.ParseDuration(maxLease); err != nil || limit <= 0 {
		return ""
	}
	return "Until its session ends lasts at most " + leaseWords(maxLease) + ", the peer_auth.max_lease cap. " +
		"Nothing extends it silently: past its end, the agent asks again."
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
	creds, err := s.approverCreds(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if d.LockOn {
		if d.Leases, d.Unlocks, err = s.accessGrants(r.Context(), creds); err != nil {
			s.serverError(w, r, err)
			return
		}
		if origins := s.cfg.Approvals.Origins(); len(origins) > 0 {
			d.Origin = origins[0]
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
	d.Cards = axWaitingCards(pending)
	d.Open = axOpenRows(d.Leases, d.Grants, d.Unlocks, creds)
	d.OpenShown, d.Kinds, d.OpenKind = axOpenFilter(d.Open, r.URL.Query().Get("open"))
	d.OpenHint = axOpenHint(d.LockOn, d.GrantsOn)
	s.render(w, r, "access", pageData{Title: "Access requests", Active: "access", Data: d})
}

// approverCreds is every enrolled approver by its store id: the names the
// open list says an approval or an unlock was made with. Approver
// credentials are about no project (a table no handle hides).
func (s *Service) approverCreds(ctx context.Context) (map[string]store.ApproverCredential, error) {
	creds, err := store.ApproverCredentials(ctx, s.db(ctx), "")
	if err != nil {
		return nil, err
	}
	out := make(map[string]store.ApproverCredential, len(creds))
	for _, c := range creds {
		out[c.ID] = c
	}
	return out, nil
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
		host := approval.RelayName(ctx, s.db(ctx), g.RelayID)
		row := grantRow{ID: g.ID, Project: g.Project, From: g.FromProject, PID: g.PeerPID, GrantedAt: g.GrantedAt, ExpiresAt: g.ExpiresAt,
			Agent: approval.AgentLabelOn("", g.PeerPID, host), Access: g.Access, Dialog: g.DecidedBy == approval.DecidedByDialog,
			decidedBy: g.DecidedBy}
		if a, err := store.AccessApprovalByID(ctx, s.db(ctx), g.ApprovalID); err == nil {
			row.Session, row.Agent = a.SessionName, approval.AgentLabelOn(a.Program, g.PeerPID, host)
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// accessGrants lists what is open right now: the agent trees' live leases and
// every browser's live console unlocks, the unlocks named by creds. Both are
// metadata about locked projects (slugs, ids, times), read through the
// request's handle from tables no handle hides (store's viewExempt), so a
// locked project's rows are listed for the owner to end.
func (s *Service) accessGrants(ctx context.Context, creds map[string]store.ApproverCredential) ([]leaseRow, []unlockRow, error) {
	now := time.Now()
	leases, err := store.LiveAccessLeases(ctx, s.db(ctx), now)
	if err != nil {
		return nil, nil, err
	}
	lrows := make([]leaseRow, 0, len(leases))
	for _, l := range leases {
		host := approval.RelayName(ctx, s.db(ctx), l.RelayID)
		row := leaseRow{ID: l.ID, Project: l.Project, PID: l.PeerPID, GrantedAt: l.GrantedAt, ExpiresAt: l.ExpiresAt,
			Agent: approval.AgentLabelOn("", l.PeerPID, host)}
		if a, err := store.AccessApprovalByID(ctx, s.db(ctx), l.ApprovalID); err == nil {
			row.Session, row.Agent, row.decidedBy = a.SessionName, approval.AgentLabelOn(a.Program, l.PeerPID, host), a.DecidedBy
		}
		lrows = append(lrows, row)
	}
	unlocks, err := store.AllLiveConsoleUnlocks(ctx, s.db(ctx), now)
	if err != nil {
		return nil, nil, err
	}
	names := make(map[string]string, len(creds))
	for id, c := range creds {
		names[id] = c.Name
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

// What decided an approval, as the approval services record it in
// decided_by beside an approver's store id: the terminal path's commit as
// the daemon's account (approval's decidedByDaemon) and the advisory tier's
// bearer key (approval's decidedByKey). The dialog's is
// approval.DecidedByDialog.
const (
	axDecidedByTerminal  = "daemon-account"
	axDecidedByBearerKey = "bearer-key"
)

// axHowApproved words what decided an approval, for the open list's sub line:
// "from a terminal", "with the passkey <name>", "with Touch ID (<name>)".
func axHowApproved(decidedBy string, creds map[string]store.ApproverCredential) string {
	switch decidedBy {
	case "":
		return ""
	case axDecidedByTerminal:
		return "from a terminal"
	case axDecidedByBearerKey:
		return "with the bearer key"
	case approval.DecidedByDialog:
		return "in the agent's approval dialog"
	}
	c, ok := creds[decidedBy]
	switch {
	case !ok:
		return "with an approver since removed"
	case c.Kind == store.ApproverTouchID && c.Name != "":
		return "with Touch ID (" + c.Name + ")"
	case c.Kind == store.ApproverTouchID:
		return "with Touch ID"
	case c.Name != "":
		return "with the passkey " + c.Name
	}
	return "with a passkey"
}

// axOpenRows is what is open right now as one list: every live lease, read
// grant and console unlock, what ends first first (then by kind and id, so
// the order is stable), each with its words and the route that ends it.
func axOpenRows(leases []leaseRow, grants []grantRow, unlocks []unlockRow, creds map[string]store.ApproverCredential) []axOpenRow {
	rows := make([]axOpenRow, 0, len(leases)+len(grants)+len(unlocks))
	for _, l := range leases {
		who := l.Agent
		if who == "" {
			who = fmt.Sprintf("pid %d", l.PID)
		}
		rows = append(rows, axOpenRow{Kind: axOpenLease, ID: l.ID, Project: l.Project, ExpiresAt: l.ExpiresAt, Who: who,
			Session: l.Session, GrantedAt: l.GrantedAt, How: axHowApproved(l.decidedBy, creds),
			Action: "/console/access/leases/" + l.ID + "/revoke", Button: "Revoke",
			Confirm: "Revoke this lease? " + who + " loses " + l.Project + " at once, and asks again to get it back."})
	}
	for _, g := range grants {
		who, session := g.Agent, g.Session
		switch {
		case who != "":
		case g.Session != "":
			who, session = g.Session, ""
		default:
			who = fmt.Sprintf("pid %d", g.PID)
		}
		row := axOpenRow{Kind: axOpenGrant, ID: g.ID, Project: g.Project, For: g.From, ExpiresAt: g.ExpiresAt, Who: who,
			Session: session, Access: "read-only", GrantedAt: g.GrantedAt, How: axHowApproved(g.decidedBy, creds),
			Action: "/console/access/grants/" + g.ID + "/revoke", Button: "Revoke",
			Confirm: "Revoke this grant? " + who + " can no longer read " + g.Project +
				", and its writes stay confined: a revoke does not unread what it read."}
		if g.Access == store.GrantAccessWrite {
			row.Access = "read and write"
		}
		if g.From != "" {
			row.Confined = "writes only into " + g.From + " from now on"
			if g.Access == store.GrantAccessWrite {
				row.Confined = "writes only into " + g.From + " and " + g.Project + " from now on"
			}
		}
		rows = append(rows, row)
	}
	for _, u := range unlocks {
		who, where := "Another browser", "that browser"
		if u.ThisBrowser {
			who, where = "This browser", "this browser"
		}
		rows = append(rows, axOpenRow{Kind: axOpenUnlock, ID: u.ID, Project: u.Project, ExpiresAt: u.ExpiresAt, Who: who, By: u.By,
			Action: "/console/access/unlocks/" + u.ID + "/end", Button: "Lock again",
			Confirm: "Lock " + u.Project + " again? It closes in " + where + " until it is unlocked again."})
	}
	slices.SortStableFunc(rows, func(a, b axOpenRow) int {
		if c := a.ExpiresAt.Compare(b.ExpiresAt); c != 0 {
			return c
		}
		if c := strings.Compare(a.Kind, b.Kind); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return rows
}

// axOpenFilter is the open list as the page shows it under ?open=: past
// axOpenFilterAt rows it offers a chip for all of them and one per kind
// present, and kind (one of those) keeps that kind's rows. At or under it the
// list is short enough to read whole, and ?open= is ignored.
func axOpenFilter(rows []axOpenRow, kind string) ([]axOpenRow, []axOpenKind, string) {
	if len(rows) <= axOpenFilterAt {
		return rows, nil, ""
	}
	counts := make(map[string]int, 3)
	for _, r := range rows {
		counts[r.Kind]++
	}
	if counts[kind] == 0 {
		kind = ""
	}
	kinds := []axOpenKind{{Label: "All", Href: "/console/access", N: len(rows), On: kind == ""}}
	for _, k := range []struct{ kind, label string }{{axOpenLease, "Leases"}, {axOpenGrant, "Grants"}, {axOpenUnlock, "Unlocks"}} {
		if n := counts[k.kind]; n > 0 {
			kinds = append(kinds, axOpenKind{Kind: k.kind, Label: k.label, Href: "/console/access?open=" + k.kind, N: n, On: kind == k.kind})
		}
	}
	if kind == "" {
		return rows, kinds, ""
	}
	shown := make([]axOpenRow, 0, counts[kind])
	for _, r := range rows {
		if r.Kind == kind {
			shown = append(shown, r)
		}
	}
	return shown, kinds, kind
}

// axOpenHint says what each kind of row in the open list is, for the halves
// this daemon has: leases and unlocks with a lock, grants with read grants.
func axOpenHint(lock, grants bool) string {
	var parts []string
	if lock {
		parts = append(parts, "A lease opens a locked project to one agent and the programs it started.")
	}
	if grants {
		parts = append(parts, "A grant opens a confidential project to one agent, read-only unless you chose read and write.")
	}
	if lock {
		parts = append(parts, "An unlock opens a locked project in one browser, never to an agent.")
	}
	return strings.Join(append(parts, "Each ends on its own; Revoke or Lock again ends it now."), " ")
}

// axWaitingCards are the waiting requests as the list draws them.
func axWaitingCards(pending []approval.Facts) []axCard {
	cards := make([]axCard, 0, len(pending))
	for _, f := range pending {
		texts := make([]string, 0, len(f.Warnings))
		for _, w := range f.Warnings {
			texts = append(texts, axSentence(w.Text))
		}
		cards = append(cards, axCard{Facts: f, Who: axWho(f), Asks: axAsks(f), ReasonLine: axReasonLine(f.Reason),
			WarnText: strings.Join(texts, " "), LockedChip: axLocked(f)})
	}
	return cards
}

// axWho names the asking program as the kernel names it: the agent's name
// ("Claude Code 2.1.319"), else the program's base name; "" when the daemon
// knows no program. Program is a path on the daemon's machine (or a relay's),
// never this console's browser's.
func axWho(f approval.Facts) string {
	if f.Agent != "" {
		return f.Agent
	}
	if f.Program == "" {
		return ""
	}
	return path.Base(f.Program)
}

// axAsks is what a request asks to do with its project: move into it, or
// read it, or read and write it (a lock's lease reads and writes; a grant
// asks for what AskedAccess says, read-only when it says nothing).
func axAsks(f approval.Facts) string {
	switch {
	case f.Kind == store.AccessKindMove:
		return "move into"
	case f.AskedAccess == accessReadWrite, f.AskedAccess == "" && f.Access == accessReadWrite:
		return "read and write"
	}
	return "read"
}

// axKindWords is a request's kind as the request's card says it.
func axKindWords(f approval.Facts) string {
	switch {
	case f.Kind == store.AccessKindRefresh:
		return "a refresh of a lease it holds"
	case f.Kind == store.AccessKindMove:
		return "a move into a fenced project, for good"
	case f.Kind == store.AccessKindGrant && len(f.AccessChoices) > 0:
		return "a grant of a confidential project: read-only, or read and write as you choose"
	case f.Kind == store.AccessKindGrant:
		return "a read-only grant of a confidential project"
	}
	return "a first request"
}

// axLocked reports whether a request's project is locked: a lock's own
// request (or its refresh) always is; a grant or a move says so in Locked.
func axLocked(f approval.Facts) bool {
	return f.Kind == store.AccessKindRequest || f.Kind == store.AccessKindRefresh || f.Locked
}

// axReasonLine is an agent's reason on one line, its whitespace runs folded to
// one space: the waiting list's excerpt, which the page cuts to the row's
// width (the request's own page quotes it whole).
func axReasonLine(reason string) string { return strings.Join(strings.Fields(reason), " ") }

// axSentence is a warning's text as a sentence of a hint: its first letter
// upper case, ending with a period.
func axSentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	r, n := utf8.DecodeRuneInString(s)
	s = string(unicode.ToUpper(r)) + s[n:]
	if !strings.HasSuffix(s, ".") {
		s += "."
	}
	return s
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
	kindOfGrants := a.Kind == store.AccessKindGrant || a.Kind == store.AccessKindMove
	d := accessOneData{Request: facts, Status: status, Page: s.core().Page(a.ID), Terminal: s.cfg.Terminal,
		Durations: approval.Durations, MaxLease: s.maxLease(), Advisory: kindOfGrants && s.advisory(),
		InDialog: a.DecidedBy == approval.DecidedByDialog}
	d.Choices = durationChoices(d.MaxLease)
	d.Accesses, d.Access = accessChoices(facts), chosenAccess(r, facts)
	d.AccessNote = accessNote(d.Accesses)
	for _, warn := range facts.Warnings {
		d.WarnLines = append(d.WarnLines, axSentence(warn.Text))
	}
	d.Who, d.Asks, d.KindWords, d.LockedChip = axWho(facts), axAsks(facts), axKindWords(facts), axLocked(facts)
	d.CapNote = axCapNote(d.MaxLease)
	if d.ForGood = a.Kind == store.AccessKindMove && !facts.Locked; d.ForGood {
		// A move no lock covers lasts for good: there is no duration to
		// choose, and the proof signs the default.
		d.Choices = []durationChoice{{Value: approval.ForSession, Words: forGoodWords}}
		d.CapNote = ""
	}
	// The duration the page opens with selected: the one ?for= chose, else
	// the first, the default (UX 41, D14). Under passkeys it is the one the
	// options sign, so the owner sees it selected before any passkey prompt.
	sel := axOfferedDuration(chosenDuration(r), d.Choices)
	d.Selected, d.ApproveWords = sel.Value, axApproveWords(sel)
	if d.ForGood {
		d.ApproveWords = "the move"
	}
	if status == store.ApprovalPending && access.PeerAuthenticated(r.Context()) && wantsJSON(r) {
		d.Challenge = base64.StdEncoding.EncodeToString(a.Challenge)
	}
	if status == store.ApprovalApproved && a.Kind == store.AccessKindGrant {
		g, ok, err := store.AccessGrantForApproval(r.Context(), s.db(r.Context()), a.ID)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		if ok {
			d.Granted = grantedOf(a, g, time.Now())
		}
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
		// The options sign the duration the page shows selected (Selected),
		// with the facts it shows (Addendum 6). A move no lock covers signs
		// the default.
		if d.For = d.Selected; d.ForGood {
			d.For = approval.ForSession
		}
		if d.For != "" && len(d.Accesses) > 1 && d.Access == "" {
			// A grant offering read and write never skips its access step:
			// the in-session dialog that sends the owner here with ?for=
			// chose only how long, and signing that would approve read-only
			// whatever the owner meant (P13, 2026-11-14). Its access
			// segments wait with neither chosen, the duration stays
			// selected, and no options are issued until the owner chooses.
			d.PreFor, d.For = d.For, ""
		}
		if d.For != "" {
			d.ForWords = durationWords(d.For, d.MaxLease)
			if d.ForGood {
				d.ForWords = forGoodWords
			}
			if d.PasskeyReady {
				// Options over the binding of the request, the chosen
				// duration and access, and the facts digest this page shows.
				if opts, err := s.cfg.Approvals.AssertionOptionsForAccess(r.Context(), a.ID, d.For, d.Access, facts.Digest); err == nil {
					d.PasskeyOptions = opts
				} else {
					s.logger.Debug("console: passkey options", "request", facts.Short, "error", err)
				}
			}
		}
	}
	s.render(w, r, "access_one", pageData{Title: "Access request " + facts.Short, Active: "access", Data: d})
}

// forGoodWords is how long a move no lock covers lasts, as the pages say it.
const forGoodWords = "for good: a move does not end"

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
	// write is read only with read grants on: off, the route reads the form
	// it always read.
	var write bool
	if s.cfg.Grants != nil {
		var err error
		if write, err = formFlag(r, "write"); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	}
	// A read grant is for a project no lock covers: a locked project this
	// tree holds a lease on is still the lock's (a live lease answers, or a
	// refresh is filed).
	_, leased := v.Lease(project)
	lockCovers := access.Locked(r.Context(), project) || leased
	// write asks for what the asker's home and the target call for: a
	// read-and-write grant between confidential projects, else a move (with
	// read grants on; without them a lock's lease reads and writes already).
	switch {
	case s.cfg.Grants != nil && write:
		if s.askRequest(w, r, project, proc, host, lockCovers) {
			return
		}
	case s.cfg.Grants != nil && !lockCovers:
		s.grantRequest(w, r, project, proc, host)
		return
	}
	if s.cfg.Approvals == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "that project is not confidential: nothing to request"})
		return
	}
	var asked approval.Asked
	a, err := s.cfg.Approvals.Request(approval.WithAsked(r.Context(), &asked), approval.RequestInput{
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
			// The reason's length, or a fact the store would not record as
			// given (the program the kernel names, longer than 1024 bytes or
			// not valid UTF-8): say which, without the internals' prefixes.
			if _, detail, ok := strings.Cut(msg, store.ErrInvalidAccessRequest.Error()+": "); ok {
				msg = "no access request was filed: " + detail
			}
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	// The passkey page only when a passkey can approve the request there.
	filed, reason := asked.Outcome(a)
	writeJSON(w, http.StatusOK, map[string]any{
		"id": a.ID, "short_id": approval.ShortID(a.ID), "project": a.Project, "kind": a.Kind,
		"filed": filed, "reason": reason,
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
	switch a.Kind {
	case store.AccessKindGrant:
		if ok {
			s.grantApproveTouchID(w, r, a, touch)
			return
		}
		s.grantApprove(w, r, a)
		return
	case store.AccessKindMove:
		s.moveApprove(w, r, a, touch, ok)
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
		Access: r.PostForm.Get("access"),
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
		Access: r.PostForm.Get("access"),
	})
	if err != nil {
		s.accessError(w, r, err)
		return
	}
	afterAccessWrite(w, r, grantApproved(a, grant), grantNotice(a, grant))
}

// grantApproved is the JSON answer to an approved grant: access names what
// the owner chose (read or write).
func grantApproved(a store.AccessApproval, g store.AccessGrant) map[string]any {
	return map[string]any{
		"approved": approval.ShortID(a.ID), "project": a.Project, "from": a.FromProject, "grant": g.ID,
		"access": g.Access, "expires_at": g.ExpiresAt.UTC().Format(time.RFC3339),
	}
}

// grantNotice is the page's notice for an approved grant: what it opens, how,
// and until when.
func grantNotice(a store.AccessApproval, g store.AccessGrant) string {
	how := "readable from " + a.FromProject + ", read-only,"
	if g.Access == store.GrantAccessWrite {
		how = "readable and writable from " + a.FromProject + ","
	}
	return "Approved " + approval.ShortID(a.ID) + ": " + a.Project + " is " + how + " until " + g.ExpiresAt.Local().Format("15:04") + "."
}

// moveApprove decides a move request: a passkey assertion, or -- in the
// advisory tier only -- the bearer key's decision (no passkey response).
// Moves exist only with read grants on (isolation.requests). A Touch ID proof
// reaches the approve core, which refuses it for a move (ErrTouchIDMove).
func (s *Service) moveApprove(w http.ResponseWriter, r *http.Request, a store.AccessApproval, touch approval.Proof, isTouch bool) {
	if s.cfg.Grants == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "moves are off (isolation.requests)"})
		return
	}
	proof := approval.Proof{Kind: approval.ProofKey}
	switch {
	case isTouch:
		proof = touch
	case r.PostForm.Get("response") != "":
		proof = approval.Proof{Kind: approval.ProofPasskey, Assertion: []byte(r.PostForm.Get("response"))}
	}
	done, err := s.cfg.Grants.ApproveMove(r.Context(), approval.Decision{
		ID: a.ID, Duration: r.PostForm.Get("duration"), Digest: r.PostForm.Get("digest"), Proof: proof,
		Access: r.PostForm.Get("access"),
	})
	if err != nil {
		s.accessError(w, r, err)
		return
	}
	payload := map[string]any{"approved": approval.ShortID(a.ID), "kind": a.Kind, "project": a.Project, "from": a.FromProject,
		"session": done.Child.Name}
	notice := "Approved " + approval.ShortID(a.ID) + ": the agent works in " + a.Project + " from now on, as session " +
		done.Child.Name + "."
	if done.Lease != nil {
		payload["expires_at"] = done.Lease.ExpiresAt.UTC().Format(time.RFC3339)
		notice = strings.TrimSuffix(notice, ".") + "; its lock is open to it until " + done.Lease.ExpiresAt.Local().Format("15:04") + "."
	}
	afterAccessWrite(w, r, payload, notice)
}

// formFlag reads a boolean form field: absent is false; 1, true and on are
// true; 0 and false are false. Anything else is refused, never read as
// absent.
func formFlag(r *http.Request, name string) (bool, error) {
	if !r.PostForm.Has(name) {
		return false, nil
	}
	switch strings.ToLower(strings.TrimSpace(r.PostForm.Get(name))) {
	case "1", "true", "on":
		return true, nil
	case "0", "false":
		return false, nil
	}
	return false, fmt.Errorf("invalid %s %q: valid values are true and false", name, r.PostForm.Get(name))
}

// accessDeny closes a request without a lease. An agent's socket connection
// may not deny: the owner's browser or the TCP key only, or through a relay
// the owner's own hands there (relayOwnerHands), which has neither.
func (s *Service) accessDeny(w http.ResponseWriter, r *http.Request) {
	if s.accessOff(w, r) {
		return
	}
	if access.PeerAuthenticated(r.Context()) && !relayOwnerHands(r) {
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
	// The locked tier's page denies from its script, which asks for JSON. A
	// plain form follows the redirect: the advisory tier's page, and the
	// locked tier's two-step Deny in a browser without script.
	afterAccessWrite(w, r, map[string]any{"denied": approval.ShortID(a.ID), "project": a.Project},
		"Denied "+approval.ShortID(a.ID)+": "+deniedWords(a)+".")
}

// deniedWords says what a denial leaves: a move's agent stays where it is, a
// fenced project stays closed to the session that asked.
func deniedWords(a store.AccessApproval) string {
	if a.Kind == store.AccessKindMove {
		home := a.FromProject
		if home == "" {
			home = "the global scope"
		}
		return "the agent stays in " + home + ", and " + a.Project + " stays closed to it"
	}
	return a.Project + " stays closed to that session"
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
			"so nothing was approved: run seam approve in a terminal of your own " + access.NotBang + " to see it as it stands now, " +
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
	case errors.Is(err, approval.ErrAccess):
		msg = "choose an access the request offers: read-only, or read and write where it asks between confidential projects"
	case errors.Is(err, approval.ErrTouchIDMove):
		msg = approval.ErrTouchIDMove.Error()
	case errors.Is(err, approval.ErrNotAMove):
		msg = "this move no longer fits the projects as they stand (its home is confidential now): deny it, and the agent asks again"
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
