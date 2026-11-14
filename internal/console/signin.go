package console

// Console sign-in with a passkey (console.sign_in: passkey; Config.SignIn).
//
// With it on, a browser signs in with a passkey the owner enrolled, in any
// browser, the server's included, so no browser needs the bearer key, and the
// key signs no browser in: a key-derived cookie names nothing. The key stays a
// bearer token for scripts (the seam CLI).
//
// A sign-in is bound the way a console unlock is (unlock.go). The browser's
// seamless_console cookie is random, and its row keeps the cookie and the
// browser's key hashed together (store.HashConsoleSignIn). A browser sends its
// cookies to every port of its host, so a request is signed in only when its
// Seamless-Browser-Proof (browserproof) verifies under the key the row was made
// with. The service worker signs every same-origin page fetch and every GET or
// HEAD navigation and never re-sends any other navigation (static/sw.js), so
// the sign-in ceremony's two POSTs are page fetches the worker signs, and a
// form posted from another site carries no proof. A page drawn without a proof
// (a hard reload skips the worker) is the login page in its checking mode
// (data-browser-proof="missing"): it sets up the key and the worker and asks
// for the page again, once.
//
// A new device adds its passkey on the Add a passkey page (/console/passkeys/
// add), a plain address that is no secret: creating a passkey there enrolls
// nothing. The page shows a short code naming the waiting passkey, and the
// owner approves that code on the server (`seamlessd approvers approve`, as the
// daemon's account under peer_auth). No route approves one.
//
// Every route here answers 404 while console.sign_in is key, and refuses a
// restricted account first, like every console route.

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"slices"
	"time"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/console/browserproof"
	"github.com/arctop/seamless/internal/passkey"
	"github.com/arctop/seamless/internal/screening"
	"github.com/arctop/seamless/internal/store"
)

// SignInView is what the console needs of the daemon's passkey sign-in
// (*approval.SignIn).
type SignInView interface {
	Origins() []string
	RPID() string
	Lifetime() time.Duration
	HasPasskey(ctx context.Context) (bool, error)
	BeginSignIn(ctx context.Context, browserKey string) (approval.Ticketed, error)
	FinishSignIn(ctx context.Context, browserKey, ticket string, response []byte) (approval.SignedIn, error)
	BeginEnrollment(ctx context.Context, name string) (approval.Ticketed, error)
	FinishEnrollment(ctx context.Context, ticket, name string, response []byte) (approval.PasskeyAdded, error)
	Enrollment(ctx context.Context, id string) (store.PasskeyEnrollment, error)
}

// SignInHeader names a login page served under passkey sign-in, so
// `seamlessd console-open` can tell, without a credential, that the daemon it
// dials takes no key from a browser.
const SignInHeader = "Seamless-Sign-In"

// passkeyAddPath is the Add a passkey page: a plain address, no secret.
const passkeyAddPath = "/console/passkeys/add"

// signInRecheck is how often a long-lived request (the live feed) asks again
// whether its browser is still signed in: a sign-out elsewhere, or the passkey
// removed, ends it within this. A variable so a test need not wait for it.
var signInRecheck = 30 * time.Second

// signInState is what a request says about its browser's sign-in.
type signInState struct {
	// cookie is the seamless_console cookie's value, "" without one.
	cookie string
	// key is the browser key the request's proof verifies under, "" without
	// one; proofErr is why there is none.
	key      string
	proofErr error
	// session is the live sign-in the cookie and the key name together, nil
	// when the browser is not signed in; token is its hash.
	session *store.ConsoleSignIn
	token   string
}

type (
	signInKey struct{}
	credKey   struct{}
)

// signInOf reads the request's sign-in, once per request (classified keeps
// it in the context). A store that cannot be read signs nobody in.
func (s *Service) signInOf(r *http.Request) signInState {
	if st, ok := r.Context().Value(signInKey{}).(*signInState); ok {
		return *st
	}
	var st signInState
	if c, err := r.Cookie(cookieName); err == nil {
		st.cookie = c.Value
	}
	now := time.Now()
	st.key, st.proofErr = browserproof.Verify(r.Header.Get(browserproof.Header), now)
	if st.cookie == "" || st.key == "" {
		return st
	}
	token := store.HashConsoleSignIn(st.cookie, st.key)
	sess, err := store.LiveConsoleSignIn(r.Context(), s.writeDB(), token, now)
	switch {
	case err == nil:
		st.session, st.token = &sess, token
	case errors.Is(err, store.ErrSignInNotFound):
	default:
		s.logger.Warn("console: sign-ins unavailable; this browser is not signed in", "error", err)
	}
	return st
}

// classified classifies the request's credential once and keeps the answer
// (and, under passkey sign-in, the sign-in it read) in its context, so the
// handler's own checks (browserOnly, the unlock page) do not read it again.
func (s *Service) classified(r *http.Request) (*http.Request, credential) {
	if s.cfg.SignIn != nil && !access.PeerAuthenticated(r.Context()) {
		st := s.signInOf(r)
		r = r.WithContext(context.WithValue(r.Context(), signInKey{}, &st))
	}
	cred := s.classify(r)
	return r.WithContext(context.WithValue(r.Context(), credKey{}, cred)), cred
}

// signedInSession is the live sign-in classified read for this request, nil
// when none.
func signedInSession(ctx context.Context) (*store.ConsoleSignIn, string) {
	if st, ok := ctx.Value(signInKey{}).(*signInState); ok && st.session != nil {
		return st.session, st.token
	}
	return nil, ""
}

// withSignIn ends the request when its sign-in does: at the sign-in's expiry,
// and for the live feed also when a check every signInRecheck finds it ended
// (signed out in another tab, or its passkey removed). The cancel is the
// caller's to defer.
func (s *Service) withSignIn(r *http.Request) (*http.Request, context.CancelFunc) {
	sess, token := signedInSession(r.Context())
	if sess == nil {
		return r, func() {}
	}
	ctx, cancel := context.WithDeadline(r.Context(), sess.ExpiresAt)
	if r.URL.Path == "/console/events" {
		go s.watchSignIn(ctx, cancel, token, signInRecheck)
	}
	return r.WithContext(ctx), cancel
}

// watchSignIn cancels ctx once the sign-in under token is no longer live,
// asking every interval. It returns when ctx ends, whichever way.
func (s *Service) watchSignIn(ctx context.Context, cancel context.CancelFunc, token string, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			_, err := store.LiveConsoleSignIn(ctx, s.writeDB(), token, time.Now())
			if errors.Is(err, store.ErrSignInNotFound) {
				cancel()
				return
			}
			if err != nil && ctx.Err() == nil {
				s.logger.Warn("console: sign-in recheck failed; the stream goes on until the next", "error", err)
			}
		}
	}
}

// refusedNotOwner answers a request from anyone but the owner -- a restricted
// account on the peer_auth socket -- and reports that it did: the console is
// the owner's surface. auth and the public sign-in routes both start here.
func (s *Service) refusedNotOwner(w http.ResponseWriter, r *http.Request) bool {
	if access.OwnerOnly(r.Context()) {
		return false
	}
	access.NoteRefusal(r.Context(), screening.RefusedOwnerOnly)
	msg := "forbidden: the console is the owner's surface"
	// The request route is the agent's own (seam access request from its
	// shell), so a restricted account hears access_request's answer there,
	// not the console's.
	if acct := access.AccountOf(r.Context()); acct != "" && r.URL.Path == "/console/access/request" {
		msg = access.RestrictedNoRequests(acct)
	}
	if wantsJSON(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": msg})
		return true
	}
	http.Error(w, msg, http.StatusForbidden)
	return true
}

// signInRoute guards a public sign-in route: the owner only; a 404 while the
// key signs browsers in; a browser only, so never the peer_auth socket; and a
// write only from this origin, like every cookie write. It writes the refusal
// and never renders the console's chrome: the caller is not signed in.
func (s *Service) signInRoute(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.refusedNotOwner(w, r) {
			return
		}
		if s.cfg.SignIn == nil {
			s.publicError(w, r, http.StatusNotFound, "console sign-in with a passkey is off: the console's key signs a browser in")
			return
		}
		if access.PeerAuthenticated(r.Context()) {
			s.publicError(w, r, http.StatusForbidden, "a browser signs in, and adds a passkey, on the console's own address, never over the socket")
			return
		}
		if !safeMethod(r.Method) && !sameOriginRequest(r) {
			s.logger.Warn("console: rejected cross-origin sign-in request",
				"path", r.URL.Path, "origin", r.Header.Get("Origin"), "sec_fetch_site", r.Header.Get("Sec-Fetch-Site"))
			s.publicError(w, r, http.StatusForbidden, "forbidden: cross-origin request")
			return
		}
		next(w, r)
	}
}

// publicError answers a public route's refusal: JSON for the page's script,
// plain text otherwise, never a page in the console's chrome.
func (s *Service) publicError(w http.ResponseWriter, r *http.Request, status int, msg string) {
	if wantsJSON(r) || r.Method == http.MethodPost {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	http.Error(w, msg, status)
}

// signInKeyOf returns the browser key the request's proof verifies under, or
// writes the refusal: a sign-in is bound to the key, so it neither begins nor
// finishes without one.
func (s *Service) signInKeyOf(w http.ResponseWriter, r *http.Request) (string, bool) {
	key, err := browserproof.Verify(r.Header.Get(browserproof.Header), time.Now())
	if err == nil {
		return key, true
	}
	msg := "this browser sent no proof of its key: reload this page and sign in again"
	if errors.Is(err, browserproof.ErrClock) {
		msg = fmt.Sprintf("this browser's clock is more than %s off the daemon's: correct it, then sign in again", leaseWord(browserproof.Window))
	}
	writeJSON(w, http.StatusConflict, map[string]string{"error": msg})
	return "", false
}

// signInCookie writes the browser's seamless_console cookie, valid for maxAge
// (at most what the sign-in lasts; -1 deletes it). It carries the attributes
// the key-derived cookie always had, so a sign-in replaces one in place.
func (s *Service) signInCookie(w http.ResponseWriter, value string, maxAge time.Duration) {
	age := -1
	if maxAge > 0 {
		age = max(1, int(maxAge/time.Second))
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    value,
		Path:     "/console",
		HttpOnly: true,
		Secure:   s.cfg.SecureCookies,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   age,
	})
}

// signInError answers a ceremony step of the sign-in or the Add a passkey
// page with a JSON error the page shows. The passkey library's detail goes to
// the debug log only, and nothing is drawn in the console's chrome.
func (s *Service) signInError(w http.ResponseWriter, r *http.Request, err error) {
	msg, status := "", http.StatusConflict
	switch {
	case errors.Is(err, approval.ErrNoPasskey):
		msg = "no passkey is enrolled here yet: add one at " + passkeyAddPath + ", then sign in"
	case errors.Is(err, approval.ErrSignInTicket):
		msg = "this expired or was already used: start again"
	case errors.Is(err, approval.ErrNoBrowserKey):
		msg = "this browser sent no proof of its key: reload this page and sign in again"
	case errors.Is(err, passkey.ErrUnknownCredential):
		msg = "that passkey is not enrolled here: add it at " + passkeyAddPath + " first"
	case errors.Is(err, store.ErrEnrollmentBusy):
		msg, status = "too many passkeys are waiting for approval; try again in a few minutes", http.StatusTooManyRequests
	case errors.Is(err, store.ErrEnrollmentWaiting):
		msg = fmt.Sprintf("that passkey is already waiting for approval: approve the code its page showed, or add it again after %d minutes",
			int(store.PasskeyEnrollmentTTL/time.Minute))
	case errors.Is(err, store.ErrApproverExists):
		msg = "that passkey is already enrolled here: it signs in as it is"
	case errors.Is(err, store.ErrInvalidApprover):
		msg, status = "give the passkey a name of 1 to 64 letters, digits, spaces and punctuation, on one line", http.StatusBadRequest
	case errors.Is(err, approval.ErrRestricted):
		msg, status = "forbidden: the console is the owner's surface", http.StatusForbidden
	case errors.Is(err, passkey.ErrUserVerification):
		msg, status = "the authenticator did not verify you (a PIN, fingerprint or face); use one that does", http.StatusBadRequest
	case errors.Is(err, passkey.ErrUnsupportedKey), errors.Is(err, passkey.ErrWeakKey):
		msg, status = "that authenticator's key type is not accepted (ES256, EdDSA or RSA of at least 2048 bits)", http.StatusBadRequest
	case errors.Is(err, passkey.ErrSignCount):
		msg, status = "that passkey's counter went backwards: it may have been cloned", http.StatusBadRequest
	case errors.Is(err, store.ErrApproverCounter):
		msg = "another sign-in used that passkey at the same moment; try again"
	case errors.Is(err, passkey.ErrVerification):
		msg, status = "the passkey could not be verified; try again", http.StatusBadRequest
	}
	if msg == "" {
		s.logger.Error("console: sign-in request failed", "path", r.URL.Path, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "something went wrong; the daemon's log says what"})
		return
	}
	s.logger.Debug("console: sign-in ceremony refused", "path", r.URL.Path, "error", err, "detail", passkey.DebugInfo(err))
	writeJSON(w, status, map[string]string{"error": msg})
}

// signInBegin starts a sign-in for the browser whose key the request proves:
// a ticket and the assertion options over a challenge bound to that key.
func (s *Service) signInBegin(w http.ResponseWriter, r *http.Request) {
	key, ok := s.signInKeyOf(w, r)
	if !ok {
		return
	}
	begun, err := s.cfg.SignIn.BeginSignIn(r.Context(), key)
	if err != nil {
		s.signInError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, begun)
}

// signInFinish verifies the assertion, signs this browser in under a fresh
// cookie bound to its key, and answers where to go. A cookie the browser held
// before ends with it: it is signing in again.
func (s *Service) signInFinish(w http.ResponseWriter, r *http.Request) {
	key, ok := s.signInKeyOf(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	signed, err := s.cfg.SignIn.FinishSignIn(ctx, key, r.PostForm.Get("ticket"), []byte(r.PostForm.Get("response")))
	if err != nil {
		s.signInError(w, r, err)
		return
	}
	if c, cerr := r.Cookie(cookieName); cerr == nil && c.Value != "" {
		if _, err := store.EndConsoleSignInByCookie(ctx, s.writeDB(), c.Value, time.Now()); err != nil {
			s.logger.Warn("console: end the browser's earlier sign-in", "error", err)
		}
	}
	s.signInCookie(w, signed.Cookie, time.Until(signed.SignIn.ExpiresAt))
	writeJSON(w, http.StatusOK, map[string]any{
		"signed_in": true, "next": safeNext(r.PostForm.Get("next")),
		"expires_at": signed.SignIn.ExpiresAt.UTC().Format(time.RFC3339), "passkey": signed.Credential.Name,
	})
}

// signOutFirst is POST /console/logout under passkey sign-in, ahead of next
// (the key's sign-out): the sign-in that this browser's cookie names ends, and
// the browser's cookies go. It needs the cookie alone, not the proof, so the
// sidebar's plain form works from any page, and a form posted from another
// site is refused by the origin check. With sign-in off it is next, exactly.
func (s *Service) signOutFirst(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.SignIn == nil {
			next(w, r)
			return
		}
		if s.refusedNotOwner(w, r) {
			return
		}
		bearer := access.PeerAuthenticated(r.Context()) || (s.cfg.APIKey != "" && bearerEquals(r, s.cfg.APIKey))
		if !bearer && !sameOriginRequest(r) {
			s.logger.Warn("console: rejected cross-origin sign-out", "origin", r.Header.Get("Origin"), "sec_fetch_site", r.Header.Get("Sec-Fetch-Site"))
			http.Error(w, "forbidden: cross-origin request", http.StatusForbidden)
			return
		}
		if c, err := r.Cookie(cookieName); err == nil && c.Value != "" {
			if _, err := store.EndConsoleSignInByCookie(r.Context(), s.writeDB(), c.Value, time.Now()); err != nil {
				s.serverErrorBare(w, r, err)
				return
			}
		}
		s.signInCookie(w, "", -1)
		// This browser's console unlocks belong to its cookie too: signed out,
		// it keeps none.
		if unlockCookieValue(r) != "" {
			http.SetCookie(w, &http.Cookie{Name: unlockCookieName, Value: "", Path: "/console", HttpOnly: true,
				Secure: s.cfg.SecureCookies, SameSite: http.SameSiteStrictMode, MaxAge: -1})
		}
		followable(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/console/login", http.StatusSeeOther)
		})(w, r)
	}
}

// serverErrorBare logs a failure and answers it without the console's chrome,
// for a request that may not be signed in.
func (s *Service) serverErrorBare(w http.ResponseWriter, r *http.Request, err error) {
	s.logger.Error("console: request failed", "path", r.URL.Path, "error", err)
	s.publicError(w, r, http.StatusInternalServerError, "something went wrong; the daemon's log says what")
}

// passkeyAddData is the Add a passkey page.
type passkeyAddData struct {
	// OnOrigin is whether the page was loaded from one of the relying party's
	// origins, the only places a passkey can be made for it; Origin is the
	// first of them.
	OnOrigin bool
	Origin   string
	// Localhost is a relying party of localhost: only a browser on the
	// server's own machine can make or use its passkeys.
	Localhost bool
	// SignedIn is whether this browser is signed in, for the way back.
	SignedIn bool
	// Minutes is how long a created passkey waits for the owner's approval.
	Minutes int
}

// enrollmentCodeSlot is the placeholder the page's command carries until the
// page has a code.
const enrollmentCodeSlot = "XXXX-XXXX"

// approveEnrollmentCommand is the server's half of adding a passkey: the
// approval, as the daemon's account when the daemon knows how that is
// reached, of the code the device's page shows.
func (s *Service) approveEnrollmentCommand(code string) string {
	return s.daemonCommand("approvers approve --code " + code)
}

// passkeyAddPage serves the Add a passkey page: open to any browser that can
// reach the console, since creating a passkey there enrolls nothing.
func (s *Service) passkeyAddPage(w http.ResponseWriter, r *http.Request) {
	r, _ = s.classified(r)
	d := passkeyAddData{
		Localhost: s.cfg.SignIn.RPID() == "localhost",
		Minutes:   int(store.PasskeyEnrollmentTTL / time.Minute),
	}
	if sess, _ := signedInSession(r.Context()); sess != nil {
		d.SignedIn = true
	}
	if origins := s.cfg.SignIn.Origins(); len(origins) > 0 {
		d.Origin = origins[0]
		d.OnOrigin = slices.Contains(origins, requestOrigin(r))
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.signinPages.ExecuteTemplate(w, "passkey-add", d); err != nil {
		s.logger.Error("console: render the Add a passkey page", "error", err)
	}
}

// passkeyAddBegin starts creating a passkey named name.
func (s *Service) passkeyAddBegin(w http.ResponseWriter, r *http.Request) {
	begun, err := s.cfg.SignIn.BeginEnrollment(r.Context(), r.PostForm.Get("name"))
	if err != nil {
		s.signInError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, begun)
}

// passkeyAddFinish takes the device's new passkey, records it as waiting for
// the owner, and answers the code and the command that approves it.
func (s *Service) passkeyAddFinish(w http.ResponseWriter, r *http.Request) {
	added, err := s.cfg.SignIn.FinishEnrollment(r.Context(), r.PostForm.Get("ticket"), r.PostForm.Get("name"),
		[]byte(r.PostForm.Get("response")))
	if err != nil {
		s.signInError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": added.ID, "code": added.Code, "name": added.Name, "fingerprint": added.Fingerprint,
		"pending_until": added.PendingUntil.UTC().Format(time.RFC3339), "minutes": int(store.PasskeyEnrollmentTTL / time.Minute),
		"command": s.approveEnrollmentCommand(added.Code),
	})
}

// The states of a passkey a device added, as the page waiting on it reads
// them.
const (
	enrollmentWaiting  = "pending"  // its code waits for the owner
	enrollmentEnrolled = "enrolled" // approved: it signs in
	enrollmentExpired  = "expired"  // its deadline passed unapproved, or no such passkey
	enrollmentEnded    = "ended"    // every credential was reset while it waited
)

// passkeyAddStatus answers the page waiting on an approval with the state of
// the passkey it added (?id=). It says only that; the id is the enrollment's
// own, which the finish answered to that page.
func (s *Service) passkeyAddStatus(w http.ResponseWriter, r *http.Request) {
	e, err := s.cfg.SignIn.Enrollment(r.Context(), r.URL.Query().Get("id"))
	if errors.Is(err, store.ErrEnrollmentNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"state": enrollmentExpired,
			"error": "no passkey added from this page is waiting: add it again for a new code"})
		return
	}
	if err != nil {
		s.serverErrorBare(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	switch {
	case e.Status == store.EnrollmentEnrolled:
		writeJSON(w, http.StatusOK, map[string]string{"state": enrollmentEnrolled, "name": e.Credential.Name})
	case e.Status == store.EnrollmentPending && e.PendingUntil.After(time.Now()):
		writeJSON(w, http.StatusOK, map[string]string{"state": enrollmentWaiting, "pending_until": e.PendingUntil.UTC().Format(time.RFC3339)})
	case e.Status == store.EnrollmentPending:
		writeJSON(w, http.StatusOK, map[string]string{"state": enrollmentExpired})
	default:
		writeJSON(w, http.StatusOK, map[string]string{"state": enrollmentEnded})
	}
}

// signInLoginData fills the login page's passkey half.
func (s *Service) signInLoginData(r *http.Request, d *loginData) {
	st := s.signInOf(r)
	d.SignIn = true
	d.AddURL = passkeyAddPath
	d.Lifetime = lifetimeWord(s.cfg.SignIn.Lifetime())
	// A browser that holds a cookie and sent no proof skipped the service
	// worker (a hard reload): the page sets the worker up and asks again.
	d.Checking = st.cookie != "" && st.key == "" && !errors.Is(st.proofErr, browserproof.ErrClock)
	if errors.Is(st.proofErr, browserproof.ErrClock) && d.Error == "" {
		d.Error = fmt.Sprintf("This browser's clock is more than %s off the daemon's: correct it, then sign in.", leaseWord(browserproof.Window))
	}
	if origins := s.cfg.SignIn.Origins(); len(origins) > 0 {
		d.Origin = origins[0]
		d.OnOrigin = slices.Contains(origins, requestOrigin(r))
	}
	// Said only when known: a store that cannot be read is not "none".
	has, err := s.cfg.SignIn.HasPasskey(r.Context())
	if err != nil {
		s.logger.Warn("console: enrolled passkeys unavailable for the login page", "error", err)
	}
	d.NoPasskey = err == nil && !has
}

// lifetimeWord renders a sign-in's lifetime for the owner: "7 days", "12 hours".
func lifetimeWord(d time.Duration) string {
	if d >= 24*time.Hour && d%(24*time.Hour) == 0 {
		return plural(int(d/(24*time.Hour)), "day", "days")
	}
	return leaseWord(d)
}

// signInTemplates parses the Add a passkey page: a standalone document like
// the login page, with no console chrome, since its visitor is not signed in.
func signInTemplates() (*template.Template, error) {
	t, err := template.New("passkey-add").Funcs(funcs).ParseFS(templateFS, "templates/signin_add.html")
	if err != nil {
		return nil, fmt.Errorf("console: parse signin_add: %w", err)
	}
	return t, nil
}
