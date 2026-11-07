package console

// The Approvers page: the owner's passkeys and Touch ID keys, which approve
// access to locked projects (peer_auth.locked), the passkey enrollment
// ceremony, and the Touch ID helper's enrollment (`seam touchid enroll`). It
// exists only while a project is locked (Config.Approvals set); otherwise
// every route here is a 404 and nothing on any other page changes.
//
// The page lists and adds; it never removes. Removing a credential, or
// resetting them all, is `seamlessd approvers remove|reset`, run as the
// daemon's own account -- the same proof that mints the enrollment code a first
// credential needs. A console cookie, or the bearer key a script holds, is not
// that proof.

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
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
	BeginPasskey(ctx context.Context, name, code string) (approval.Begun, error)
	FinishPasskey(ctx context.Context, id string, response []byte) (approval.Finished, error)
	ApprovePasskey(ctx context.Context, id string, response []byte) (store.ApproverCredential, error)
	// EnrollTouchID stores the Touch ID helper's new key, proven by a live
	// touchid enrollment code and the key's own signature over it.
	EnrollTouchID(ctx context.Context, name, code string, publicKey, signature []byte) (store.ApproverCredential, error)

	Request(ctx context.Context, in approval.RequestInput) (store.AccessApproval, error)
	Pending(ctx context.Context) ([]approval.Facts, error)
	Facts(ctx context.Context, id string) (approval.Facts, error)
	Resolve(ctx context.Context, ref string) (store.AccessApproval, error)
	AssertionOptions(ctx context.Context, id string) ([]byte, error)
	Approve(ctx context.Context, d approval.Decision) (store.AccessLease, error)
	Deny(ctx context.Context, id, by string) error
	Page(id string) string
	Ways(ctx context.Context, id string) access.Ways
	LeaseLength(choice string) (time.Duration, error)

	// Console unlocks (unlock.go): a passkey ceremony that opens a locked
	// project in one browser, and how long such an unlock lasts.
	ConsoleLeaseLength() time.Duration
	BeginConsoleUnlock(ctx context.Context, project, tokenHash string) (approval.Begun, error)
	FinishConsoleUnlock(ctx context.Context, id, project, tokenHash string, response []byte) (store.ConsoleUnlock, error)
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
		msg = "that enrollment code is not valid: it may be mistyped, expired (codes last 10 minutes) or already used"
	case errors.Is(err, store.ErrEnrollProof):
		msg = "the first passkey needs an enrollment code: mint one with `seamlessd approvers enroll-code passkey`, run as the daemon's account"
	case errors.Is(err, store.ErrInvalidApprover):
		msg = "give the passkey a name of 1 to 64 characters on one line"
	case errors.Is(err, store.ErrApproverExists):
		msg = "that passkey is already enrolled"
	case errors.Is(err, approval.ErrNoEnrollment):
		msg = "this enrollment expired or was already used; start again"
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
	begun, err := s.cfg.Approvals.BeginPasskey(r.Context(), r.PostForm.Get("name"), r.PostForm.Get("code"))
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
	if fin.Credential != nil {
		writeJSON(w, http.StatusOK, map[string]any{"enrolled": fin.Credential.ID, "name": fin.Credential.Name})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"approve": fin.Approve})
}

func (s *Service) approversApprove(w http.ResponseWriter, r *http.Request) {
	if s.approversOff(w, r) {
		return
	}
	cred, err := s.cfg.Approvals.ApprovePasskey(r.Context(), r.PostForm.Get("id"), []byte(r.PostForm.Get("response")))
	if err != nil {
		s.approverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enrolled": cred.ID, "name": cred.Name})
}

// The Touch ID helper's enrollment form: what the Secure Enclave key it made
// looks like on the wire.
const (
	// touchIDPublicKeyBytes is a raw uncompressed P-256 point: 0x04, X, Y.
	touchIDPublicKeyBytes = 65
	// touchIDMaxSignatureBytes bounds an ASN.1 DER P-256 signature (at most
	// 72 bytes), with room to spare.
	touchIDMaxSignatureBytes = 128
)

// approversTouchIDEnroll enrolls the Touch ID helper's key: POST
// /console/approvers/touchid/enroll with the code the owner typed into the
// helper's dialog, the key's name, its public key, and its signature over
// approval.TouchIDEnrollMessage(code, public key). The code is the proof --
// only `seamlessd approvers enroll-code touchid`, run as the daemon's account,
// mints one -- and the signature shows the code's holder holds the key. It is
// accepted on the peer_auth socket only, where the helper runs as the owner's
// uid; the TCP key and the console cookie never enroll a Touch ID key.
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
		bad("a Touch ID key needs an enrollment code: mint one with `seamlessd approvers enroll-code touchid`, run as the daemon's account")
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
	cred, err := s.cfg.Approvals.EnrollTouchID(r.Context(), name, code, pub, sig)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]any{"id": cred.ID, "name": cred.Name})
	case errors.Is(err, approval.ErrTouchIDKey):
		bad("the new key's signature over the enrollment code does not verify")
	case errors.Is(err, store.ErrApproverExists):
		bad("that Touch ID key is already enrolled")
	case errors.Is(err, store.ErrInvalidApprover):
		bad("give the Touch ID key a name of 1 to 64 characters on one line")
	case errors.Is(err, store.ErrEnrollProof):
		bad("a Touch ID key needs an enrollment code: mint one with `seamlessd approvers enroll-code touchid`, run as the daemon's account")
	default:
		s.approverError(w, r, err)
	}
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
