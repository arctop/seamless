package console

// The Approvers page: the owner's passkeys and Touch ID keys, which approve
// access to locked projects, the passkey enrollment
// ceremony, and the Touch ID helper's enrollment (`seam touchid enroll`). It
// exists only while a project is locked (Config.Approvals set); otherwise
// every route here is a 404 and nothing on any other page changes.
//
// The page lists and adds; it never removes. Removing a credential, or
// resetting them all, is `seamlessd approvers remove|reset`, run as the
// daemon's own account -- the same proof that mints the enrollment code every
// credential needs. A console cookie, or the bearer key a script holds, is not
// that proof. The one credential a route retires is a Touch ID key that signs
// its own retirement while the same Mac enrolls its replacement with such a
// code (approversTouchIDEnroll).

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/passkey"
	"github.com/arctop/seamless/internal/store"
)

// formBodyPasskey bounds a passkey ceremony's form: the browser's response
// (at most passkey.MaxResponseBytes) form-encoded, with room to spare.
const formBodyPasskey int64 = 3 * passkey.MaxResponseBytes

// ApprovalsView is what the Approvers and access pages need of the daemon's
// approval service (*approval.Service).
type ApprovalsView interface {
	PasskeysOn() bool
	Origins() []string
	BeginPasskey(ctx context.Context, name string) (approval.Begun, error)
	FinishPasskey(ctx context.Context, id string, response []byte) (approval.Finished, error)
	ConfirmPasskeyCode(ctx context.Context, id, code string) (store.ApproverCredential, error)
	// EnrollTouchIDReplacing stores the Touch ID helper's new key, proven by
	// a live touchid enrollment code and the key's own signature over it, and
	// in the same transaction retires each earlier key of this Mac whose own
	// key signed the request to be retired. It returns the ids it retired.
	EnrollTouchIDReplacing(ctx context.Context, name, code string, publicKey, signature []byte,
		replaces []approval.TouchIDReplacement) (store.ApproverCredential, []string, error)

	Request(ctx context.Context, in approval.RequestInput) (store.AccessApproval, error)
	Pending(ctx context.Context) ([]approval.Facts, error)
	Facts(ctx context.Context, id string) (approval.Facts, error)
	Resolve(ctx context.Context, ref string) (store.AccessApproval, error)
	AssertionOptionsFor(ctx context.Context, id, duration, digest string) ([]byte, error)
	Approve(ctx context.Context, d approval.Decision) (store.AccessLease, error)
	Deny(ctx context.Context, id, by string) error
	Page(id string) string
	Ways(ctx context.Context, id string) access.Ways
	LeaseLength(choice string) (time.Duration, error)

	// Console unlocks (unlock.go): a passkey ceremony, or a terminal unlock
	// the daemon's account approves, that opens a locked project in the one
	// browser that began it, and how long such an unlock lasts.
	ConsoleLeaseLength() time.Duration
	BeginConsoleUnlock(ctx context.Context, project, tokenHash string) (approval.Begun, error)
	FinishConsoleUnlock(ctx context.Context, id, project, tokenHash string, response []byte) (store.ConsoleUnlock, error)
	BeginTerminalUnlock(ctx context.Context, project, tokenHash string) (approval.TerminalUnlock, error)

	// Console locks (lock.go): a passkey ceremony that turns a project's lock
	// on, or takes off a lock the console set.
	BeginLockChange(ctx context.Context, project string, action approval.LockAction) (approval.Begun, error)
	FinishLockChange(ctx context.Context, id, project string, action approval.LockAction, response []byte) (store.ApproverCredential, error)
}

// approverRow is one enrolled credential. Its JSON is what `seam touchid
// enroll` reads to confirm the helper enrolled a new key.
type approverRow struct {
	ID       string     `json:"id"`
	Kind     string     `json:"kind"`
	Name     string     `json:"name"`
	Synced   bool       `json:"synced"`
	AddedAt  time.Time  `json:"added_at"`
	AddedBy  string     `json:"added_by"` // how it was enrolled, for a person
	LastUsed *time.Time `json:"last_used_at,omitempty"`
}

type approversData struct {
	Credentials []approverRow `json:"credentials"`
	// PasskeysOn is false when the relying party is not configured; the page
	// then lists credentials and explains, with no enrollment form.
	PasskeysOn bool `json:"passkeys_on"`
	// Origin is where a passkey ceremony must run; OnOrigin is whether this
	// page was loaded from one of the configured origins. A page on
	// 127.0.0.1 cannot run one at all (an IP is not a valid RP ID).
	Origin   string `json:"origin,omitempty"`
	OnOrigin bool   `json:"on_origin"`
}

func (s *Service) approversOff(w http.ResponseWriter, r *http.Request) bool {
	if s.cfg.Approvals == nil {
		s.notFound(w, r, "no project is locked, so there are no approvers to manage")
		return true
	}
	return false
}

// requestOrigin is the origin the browser loaded this page from.
func requestOrigin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (s *Service) approversPage(w http.ResponseWriter, r *http.Request) {
	if s.approversOff(w, r) {
		return
	}
	creds, err := store.ApproverCredentials(r.Context(), s.db(r.Context()), "")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d := approversData{Credentials: []approverRow{}, PasskeysOn: s.cfg.Approvals.PasskeysOn()}
	origins := s.cfg.Approvals.Origins()
	if len(origins) > 0 {
		d.Origin = origins[0]
	}
	d.OnOrigin = slices.Contains(origins, requestOrigin(r))
	for _, c := range creds {
		d.Credentials = append(d.Credentials, approverRow{
			ID: c.ID, Kind: c.Kind, Name: c.Name, Synced: c.BackupState, AddedAt: c.AddedAt,
			AddedBy: approverAddedBy(c.AddedBy, creds), LastUsed: c.LastUsedAt,
		})
	}
	s.render(w, r, "approvers", pageData{Title: "Approvers", Active: "approvers", Data: d})
}

// approverError answers a ceremony step with a JSON error the page shows. The
// library's developer detail goes to the debug log only: it can echo origins
// and challenges.
func (s *Service) approverError(w http.ResponseWriter, r *http.Request, err error) {
	msg, status := "the passkey could not be verified; try again", http.StatusBadRequest
	switch {
	case errors.Is(err, store.ErrEnrollCode):
		msg = "that enrollment code is not valid for this key: it may be mistyped, expired (codes last 10 minutes), already used, or minted for another key"
	case errors.Is(err, store.ErrEnrollProof):
		msg = "every passkey needs a one-time enrollment code, minted for it once the passkey exists"
	case errors.Is(err, store.ErrInvalidApprover):
		msg = "give the passkey a name of 1 to 64 characters on one line"
	case errors.Is(err, store.ErrApproverExists):
		msg = "that passkey is already enrolled"
	case errors.Is(err, approval.ErrNoEnrollment):
		msg = "this enrollment expired or was already used; start again"
	case errors.Is(err, approval.ErrTooManyEnrollments):
		msg, status = "too many passkey enrollments are waiting; try again in a few minutes", http.StatusTooManyRequests
	case errors.Is(err, approval.ErrPasskeysOff):
		msg, status = "passkeys are not configured on this daemon", http.StatusConflict
	case errors.Is(err, passkey.ErrUserVerification):
		msg = "the authenticator did not verify you (a PIN, fingerprint or face); use one that does"
	case errors.Is(err, passkey.ErrUnsupportedKey), errors.Is(err, passkey.ErrWeakKey):
		msg = "that authenticator's key type is not accepted (ES256, EdDSA or RSA of at least 2048 bits)"
	case errors.Is(err, passkey.ErrSignCount):
		msg = "the approving passkey's counter went backwards: it may have been cloned"
	case errors.Is(err, passkey.ErrVerification), errors.Is(err, passkey.ErrUnknownCredential):
	default:
		if !errors.Is(err, store.ErrApproverCounter) {
			s.serverError(w, r, err)
			return
		}
		msg = "another approval used that passkey at the same moment; try again"
	}
	s.logger.Debug("console: approver ceremony refused", "path", r.URL.Path, "error", err, "detail", passkey.DebugInfo(err))
	writeJSON(w, status, map[string]string{"error": msg})
}

func (s *Service) approversBegin(w http.ResponseWriter, r *http.Request) {
	if s.approversOff(w, r) {
		return
	}
	// A passkey is created in a browser, which never comes over the socket.
	// Beginning needs no proof, since the code follows the credential, so an
	// agent on the socket could otherwise fill the waiting enrollments.
	if access.PeerAuthenticated(r.Context()) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "a passkey is enrolled from a browser, on the console's Approvers page"})
		return
	}
	begun, err := s.cfg.Approvals.BeginPasskey(r.Context(), r.PostForm.Get("name"))
	if err != nil {
		s.approverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, begun)
}

func (s *Service) approversFinish(w http.ResponseWriter, r *http.Request) {
	if s.approversOff(w, r) {
		return
	}
	fin, err := s.cfg.Approvals.FinishPasskey(r.Context(), r.PostForm.Get("id"), []byte(r.PostForm.Get("response")))
	if err != nil {
		s.approverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"fingerprint": fin.Fingerprint,
		"command": s.enrollCodeCommand(store.ApproverPasskey, fin.Fingerprint)})
}

// approversConfirm finishes a passkey enrollment with the code minted for its key.
func (s *Service) approversConfirm(w http.ResponseWriter, r *http.Request) {
	if s.approversOff(w, r) {
		return
	}
	cred, err := s.cfg.Approvals.ConfirmPasskeyCode(r.Context(), r.PostForm.Get("id"), r.PostForm.Get("code"))
	if err != nil {
		s.approverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enrolled": cred.ID, "name": cred.Name})
}

// enrollCodeCommand is the command that mints a kind's code for one key, as the
// daemon's account when the daemon knows how that is reached.
func (s *Service) enrollCodeCommand(kind, fingerprint string) string {
	verb := "approvers enroll-code " + kind + " --key " + fingerprint
	t := s.cfg.Terminal
	if t == nil || t.Account == "" || t.Seamlessd == "" {
		return "seamlessd " + verb
	}
	env := ""
	if t.Config != "" {
		env = "env SEAMLESS_CONFIG=" + shellWord(t.Config) + " "
	}
	return "sudo -k -u " + shellWord(t.Account) + " " + env + shellWord(t.Seamlessd) + " " + verb
}

// The Touch ID helper's enrollment form: what the Secure Enclave key it made
// looks like on the wire.
const (
	// touchIDPublicKeyBytes is a raw uncompressed P-256 point: 0x04, X, Y.
	touchIDPublicKeyBytes = 65
	// touchIDMaxSignatureBytes bounds an ASN.1 DER P-256 signature (at most
	// 72 bytes), with room to spare.
	touchIDMaxSignatureBytes = 128
	// touchIDMaxReplaces bounds the earlier keys one enrollment may retire:
	// the helper keeps one enrolled key per Mac, so more is never honest.
	touchIDMaxReplaces = 4
)

// touchIDCredentialID is an approver id as the helper sends it back: what
// Validate.isCredentialID admits on its side.
var touchIDCredentialID = regexp.MustCompile(`^[0-9A-Za-z_-]{1,64}$`)

// approversTouchIDEnroll enrolls the Touch ID helper's key: POST
// /console/approvers/touchid/enroll with the code the owner typed into the
// helper's dialog, the key's name, its public key, and its signature over
// approval.TouchIDEnrollMessage(code, public key). The code is the proof --
// only `seamlessd approvers enroll-code touchid --key <fingerprint>`, run as the
// daemon's account, mints one, and only for this key -- and the signature shows
// the code's holder holds the key. It is
// accepted on the peer_auth socket only, where the helper runs as the owner's
// uid; the TCP key and the console cookie never enroll a Touch ID key.
//
// A helper replacing this Mac's earlier key also sends, for each, a replace
// field (its approver id) and a replace_signature field (that key's own
// signature over approval.TouchIDReplaceMessage), in pairs. Each one whose
// signature verifies is retired with the enrollment, and the answer lists it
// under retired; any other is left enrolled. A key retires only itself, so the
// page's rule holds: no route removes a credential its holder did not ask to
// remove, and a passkey never.
func (s *Service) approversTouchIDEnroll(w http.ResponseWriter, r *http.Request) {
	if s.approversOff(w, r) {
		return
	}
	if !access.PeerAuthenticated(r.Context()) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "a Touch ID key is enrolled over the peer_auth socket only: " +
			"run seam touchid enroll in the owner's terminal on this Mac"})
		return
	}
	bad := func(msg string) { writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg}) }
	name := strings.TrimSpace(r.PostForm.Get("name"))
	if name == "" || len([]rune(name)) > store.MaxApproverNameRunes || strings.ContainsAny(name, "\r\n\t") {
		bad("give the Touch ID key a name of 1 to 64 characters on one line")
		return
	}
	code := strings.TrimSpace(r.PostForm.Get("code"))
	if code == "" {
		bad("a Touch ID key needs an enrollment code: Seamless Approve shows the command that mints one for its key")
		return
	}
	pub, err := base64.StdEncoding.Strict().DecodeString(r.PostForm.Get("public_key"))
	if err != nil || len(pub) != touchIDPublicKeyBytes || pub[0] != 0x04 {
		bad("public_key must be the key's 65-byte uncompressed P-256 point in standard base64")
		return
	}
	sig, err := base64.StdEncoding.Strict().DecodeString(r.PostForm.Get("signature"))
	if err != nil || len(sig) == 0 || len(sig) > touchIDMaxSignatureBytes {
		bad("signature must be the new key's ASN.1 DER signature over the enrollment message in standard base64")
		return
	}
	replaces, problem := touchIDReplaces(r.PostForm["replace"], r.PostForm["replace_signature"])
	if problem != "" {
		bad(problem)
		return
	}
	cred, retired, err := s.cfg.Approvals.EnrollTouchIDReplacing(r.Context(), name, code, pub, sig, replaces)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, touchIDEnrolled{ID: cred.ID, Name: cred.Name, Retired: retired})
	case errors.Is(err, approval.ErrTouchIDKey):
		bad("the new key's signature over the enrollment code does not verify")
	case errors.Is(err, store.ErrApproverExists):
		bad("that Touch ID key is already enrolled")
	case errors.Is(err, store.ErrInvalidApprover):
		bad("give the Touch ID key a name of 1 to 64 characters on one line")
	case errors.Is(err, store.ErrEnrollProof):
		bad("a Touch ID key needs an enrollment code: Seamless Approve shows the command that mints one for its key")
	default:
		s.approverError(w, r, err)
	}
}

// touchIDEnrolled is the Touch ID enrollment's answer. Retired lists the
// earlier keys of this Mac it retired, and is absent when there were none, so
// a first enrollment's answer is what it always was.
type touchIDEnrolled struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Retired []string `json:"retired,omitempty"`
}

// touchIDReplaces reads the enrollment's replace and replace_signature
// fields, which come in pairs, or says what is wrong with them.
func touchIDReplaces(ids, sigs []string) ([]approval.TouchIDReplacement, string) {
	const pairs = "replace and replace_signature come in pairs: an earlier key's approver id, and that key's signature over the replacement message in standard base64"
	switch {
	case len(ids) == 0 && len(sigs) == 0:
		return nil, ""
	case len(ids) != len(sigs):
		return nil, pairs
	case len(ids) > touchIDMaxReplaces:
		return nil, fmt.Sprintf("an enrollment replaces at most %d earlier keys", touchIDMaxReplaces)
	}
	out := make([]approval.TouchIDReplacement, len(ids))
	for i := range ids {
		sig, err := base64.StdEncoding.Strict().DecodeString(sigs[i])
		if !touchIDCredentialID.MatchString(ids[i]) || err != nil || len(sig) == 0 || len(sig) > touchIDMaxSignatureBytes {
			return nil, pairs
		}
		out[i] = approval.TouchIDReplacement{ID: ids[i], Signature: sig}
	}
	return out, ""
}

// approverAddedBy renders added_by for the list.
func approverAddedBy(addedBy string, rows []store.ApproverCredential) string {
	if addedBy == store.AddedByCode {
		return "an enrollment code"
	}
	for _, r := range rows {
		if r.ID == addedBy {
			return "approved by " + r.Name
		}
	}
	return "approved by a removed credential"
}
