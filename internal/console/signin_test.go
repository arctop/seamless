package console

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/console/browserproof"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/passkey"
	"github.com/arctop/seamless/internal/passkey/passkeytest"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// Console sign-in with a passkey (console.sign_in: passkey, signin.go), in the
// style of the console's auth tests: requests as a browser sends them, its
// service worker's proof included unless a test leaves it out.

// signinConsole is a console with passkey sign-in on.
type signinConsole struct {
	db  *sql.DB
	mux http.Handler
	svc *approval.SignIn
	cs  *Service
}

func signinNewConsole(t *testing.T, mutate func(*Config)) *signinConsole {
	t.Helper()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	v, err := passkey.New(passkey.Config{RPID: "localhost", Origins: []string{approverOrigin}})
	require.NoError(t, err)
	svc, err := approval.NewSignIn(approval.SignInConfig{DB: db, Passkeys: v, Origins: []string{approverOrigin}})
	require.NoError(t, err)
	cfg := Config{
		DB: db, Events: events.NewRecorder(db), APIKey: testKey, Level: "advanced", SignIn: svc,
		Retrieve: retrieve.New(db, nil, config.Defaults().Budgets, nil),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	cs, err := New(cfg)
	require.NoError(t, err)
	mux := http.NewServeMux()
	cs.Register(mux)
	return &signinConsole{db: db, mux: mux, svc: svc, cs: cs}
}

// signinBrowser is one browser: the key its service worker signs with, and
// its cookie jar for the console.
type signinBrowser struct {
	key     *ecdsa.PrivateKey
	cookies map[string]string
	// unsigned sends no proof: a hard reload, or a program replaying the
	// cookie from another port.
	unsigned bool
}

func signinNewBrowser(t *testing.T) *signinBrowser {
	t.Helper()
	key, err := browserproof.NewKey()
	require.NoError(t, err)
	return &signinBrowser{key: key, cookies: map[string]string{}}
}

// publicKey is the browser key as a proof names it.
func (b *signinBrowser) publicKey(t *testing.T) string {
	t.Helper()
	k, err := browserproof.EncodeKey(&b.key.PublicKey)
	require.NoError(t, err)
	return k
}

// request is a request from b: its cookies, the same-origin header a page's
// fetch carries on a write, and its proof unless b.unsigned.
func (b *signinBrowser) request(t *testing.T, method, target string, form url.Values) *http.Request {
	t.Helper()
	body := strings.NewReader("")
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, approverOrigin+target, body)
	for name, value := range b.cookies {
		req.AddCookie(&http.Cookie{Name: name, Value: value})
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		req.Header.Set("Accept", "application/json")
	}
	if !b.unsigned {
		proof, err := browserproof.Sign(b.key, time.Now())
		require.NoError(t, err)
		req.Header.Set(browserproof.Header, proof)
	}
	return req
}

// do serves req and keeps the cookies the answer sets in b's jar.
func (sc *signinConsole) do(b *signinBrowser, req *http.Request) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	sc.mux.ServeHTTP(rr, req)
	if b != nil {
		for _, c := range rr.Result().Cookies() {
			if c.MaxAge < 0 {
				delete(b.cookies, c.Name)
			} else {
				b.cookies[c.Name] = c.Value
			}
		}
	}
	return rr
}

func signinJSON(t *testing.T, rr *httptest.ResponseRecorder) map[string]json.RawMessage {
	t.Helper()
	var out map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out), rr.Body.String())
	return out
}

func signinString(t *testing.T, out map[string]json.RawMessage, key string) string {
	t.Helper()
	var s string
	require.NoError(t, json.Unmarshal(out[key], &s), key)
	return s
}

// signinAdd creates authenticator a's passkey named name on the Add a passkey
// page as a device does, and returns the finish's answer (id, code,
// fingerprint, command). It enrolls nothing.
func (sc *signinConsole) signinAdd(t *testing.T, device *signinBrowser, a *passkeytest.Authenticator, name string) map[string]json.RawMessage {
	t.Helper()
	rr := sc.do(device, device.request(t, http.MethodPost, "/console/passkeys/add/begin", url.Values{"name": {name}}))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	begun := signinJSON(t, rr)
	created, err := a.Create(begun["options"], approverOrigin)
	require.NoError(t, err)
	rr = sc.do(device, device.request(t, http.MethodPost, "/console/passkeys/add/finish", url.Values{
		"ticket": {signinString(t, begun, "ticket")}, "name": {name}, "response": {string(created)}}))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	return signinJSON(t, rr)
}

// signinEnroll adds a's passkey and approves its code as the owner does on
// the server (store.ApprovePasskeyEnrollment is what `seamlessd approvers
// approve` runs).
func (sc *signinConsole) signinEnroll(t *testing.T, a *passkeytest.Authenticator, name string) store.ApproverCredential {
	t.Helper()
	added := sc.signinAdd(t, signinNewBrowser(t), a, name)
	cred, _, err := store.ApprovePasskeyEnrollment(context.Background(), sc.db, signinString(t, added, "code"), time.Now())
	require.NoError(t, err)
	return cred
}

// signinSignIn signs b in with authenticator a through the login routes and
// returns the cookie it was given.
func (sc *signinConsole) signinSignIn(t *testing.T, b *signinBrowser, a *passkeytest.Authenticator) string {
	t.Helper()
	rr := sc.do(b, b.request(t, http.MethodPost, "/console/signin/begin", url.Values{}))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	begun := signinJSON(t, rr)
	asserted, err := a.Get(begun["options"], approverOrigin)
	require.NoError(t, err)
	rr = sc.do(b, b.request(t, http.MethodPost, "/console/signin/finish", url.Values{
		"ticket": {signinString(t, begun, "ticket")}, "response": {string(asserted)}, "next": {"/console/memories"}}))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	require.Equal(t, "/console/memories", signinString(t, signinJSON(t, rr), "next"))
	var set *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == cookieName {
			set = c
		}
	}
	require.NotNil(t, set, "the finish sets the console cookie")
	require.True(t, set.HttpOnly)
	require.Equal(t, "/console", set.Path)
	require.Equal(t, http.SameSiteLaxMode, set.SameSite)
	require.Positive(t, set.MaxAge)
	require.NotEqual(t, consoleToken(testKey), set.Value, "the cookie is random, never derived from the key")
	return set.Value
}

// signedIn reports whether a GET of the overview as b is answered as a
// signed-in browser's.
func (sc *signinConsole) signedIn(t *testing.T, b *signinBrowser) bool {
	t.Helper()
	rr := sc.do(nil, b.request(t, http.MethodGet, "/console/", nil))
	switch rr.Code {
	case http.StatusOK:
		require.Contains(t, rr.Body.String(), `class="sidebar"`)
		return true
	case http.StatusSeeOther:
		require.True(t, strings.HasPrefix(rr.Header().Get("Location"), "/console/login"), rr.Header().Get("Location"))
		return false
	}
	t.Fatalf("GET /console/ answered %d: %s", rr.Code, rr.Body.String())
	return false
}

// The review's PoC forged the console cookie from the bearer key alone:
// sha256 of a prefix and the key, no login. Once passkey sign-in is on that
// value names no browser, with or without a proof, on a page or on a write;
// and the login form takes no key at all.
func TestSignIn_ForgedKeyDerivedCookieRefused(t *testing.T) {
	sc := signinNewConsole(t, nil)
	sc.signinEnroll(t, signinAuthenticator(t), "laptop")
	sum := sha256.Sum256([]byte("seamless-console\x00" + testKey))
	forged := hex.EncodeToString(sum[:])
	require.Equal(t, consoleToken(testKey), forged, "the PoC's value is the key-derived cookie")

	for _, signs := range []bool{false, true} {
		b := signinNewBrowser(t)
		b.unsigned = !signs
		b.cookies[cookieName] = forged
		require.False(t, sc.signedIn(t, b), "a key-derived cookie (proof %v) signs nothing in", signs)

		req := b.request(t, http.MethodGet, "/console/memories", nil)
		req.Header.Set("Accept", "application/json")
		rr := sc.do(nil, req)
		require.Equal(t, http.StatusUnauthorized, rr.Code)
		require.Contains(t, rr.Body.String(), "sign in to the console with a passkey")

		before, _, _, _, err := store.ConsoleLevel(context.Background(), sc.db, "basic")
		require.NoError(t, err)
		rr = sc.do(nil, b.request(t, http.MethodPost, "/console/settings/level", url.Values{"level": {"basic"}}))
		require.Contains(t, []int{http.StatusUnauthorized, http.StatusSeeOther}, rr.Code, rr.Body.String())
		after, _, _, _, err := store.ConsoleLevel(context.Background(), sc.db, "basic")
		require.NoError(t, err)
		require.Equal(t, before, after, "a forged cookie writes nothing")
	}

	// The login form refuses the key and sets no cookie.
	b := signinNewBrowser(t)
	req := b.request(t, http.MethodPost, "/console/login", url.Values{"key": {testKey}, "next": {"/console/"}})
	req.Header.Set("Accept", "text/html")
	rr := sc.do(b, req)
	require.Equal(t, http.StatusForbidden, rr.Code)
	require.Contains(t, rr.Body.String(), "the key no longer signs a browser in")
	require.Empty(t, rr.Result().Cookies(), "the key buys no cookie")
	require.Equal(t, config.ConsoleSignInPasskey, rr.Header().Get(SignInHeader))

	// The bearer key is still a script's credential.
	rr = sc.do(nil, authedReq(httptest.NewRequest(http.MethodGet, "/console/memories?format=json", nil)))
	require.Equal(t, http.StatusOK, rr.Code, "the seam CLI keeps the key")
}

// A sign-in is bound to the browser's key as an unlock is: its cookie, read on
// another port of the host and replayed without the key, signs nothing in;
// nor does it with another browser's key, nor the key without the cookie.
func TestSignIn_CookieReplayedWithoutItsKeyRefused(t *testing.T) {
	sc := signinNewConsole(t, nil)
	a := signinAuthenticator(t)
	sc.signinEnroll(t, a, "laptop")
	owner := signinNewBrowser(t)
	cookie := sc.signinSignIn(t, owner, a)
	require.True(t, sc.signedIn(t, owner))

	// The row keeps neither the cookie nor the key, only their hash.
	var token string
	require.NoError(t, sc.db.QueryRow(`SELECT token_hash FROM console_signins`).Scan(&token))
	require.Equal(t, store.HashConsoleSignIn(cookie, owner.publicKey(t)), token)
	require.NotEqual(t, store.HashConsoleToken(cookie), token, "the cookie alone is not the row's hash")

	replay := &signinBrowser{key: owner.key, cookies: map[string]string{cookieName: cookie}, unsigned: true}
	require.False(t, sc.signedIn(t, replay), "the cookie without the browser's proof")
	thief := signinNewBrowser(t)
	thief.cookies[cookieName] = cookie
	require.False(t, sc.signedIn(t, thief), "the cookie with another key's proof")

	stale := owner.request(t, http.MethodGet, "/console/", nil)
	proof, err := browserproof.Sign(owner.key, time.Now().Add(-browserproof.Window-time.Minute))
	require.NoError(t, err)
	stale.Header.Set(browserproof.Header, proof)
	require.Equal(t, http.StatusSeeOther, sc.do(nil, stale).Code, "a proof from outside its window")

	keyOnly := &signinBrowser{key: owner.key, cookies: map[string]string{}}
	require.False(t, sc.signedIn(t, keyOnly), "the key without the cookie")

	// The replayed writes change nothing either.
	rr := sc.do(nil, replay.request(t, http.MethodPost, "/console/settings/level", url.Values{"level": {"standard"}}))
	require.Contains(t, []int{http.StatusUnauthorized, http.StatusSeeOther}, rr.Code)
	_, overridden, _, _, err := store.ConsoleLevel(context.Background(), sc.db, "basic")
	require.NoError(t, err)
	require.False(t, overridden)

	// And the owner's browser is signed in throughout.
	require.True(t, sc.signedIn(t, owner))
	rr = sc.do(nil, owner.request(t, http.MethodPost, "/console/settings/level", url.Values{"level": {"standard"}}))
	require.Equal(t, http.StatusSeeOther, rr.Code, rr.Body.String())
	_, overridden, _, _, err = store.ConsoleLevel(context.Background(), sc.db, "basic")
	require.NoError(t, err)
	require.True(t, overridden, "the signed-in browser's own write lands")
}

// A sign-in begun in one browser cannot be finished in another: the ceremony's
// challenge is bound to the key that began it.
func TestSignIn_AnAssertionSignsInOnlyTheBrowserThatBegan(t *testing.T) {
	sc := signinNewConsole(t, nil)
	a := signinAuthenticator(t)
	sc.signinEnroll(t, a, "laptop")
	owner, other := signinNewBrowser(t), signinNewBrowser(t)

	rr := sc.do(owner, owner.request(t, http.MethodPost, "/console/signin/begin", url.Values{}))
	require.Equal(t, http.StatusOK, rr.Code)
	begun := signinJSON(t, rr)
	asserted, err := a.Get(begun["options"], approverOrigin)
	require.NoError(t, err)
	finish := url.Values{"ticket": {signinString(t, begun, "ticket")}, "response": {string(asserted)}}
	rr = sc.do(other, other.request(t, http.MethodPost, "/console/signin/finish", finish))
	require.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
	require.Empty(t, other.cookies)
	n, err := store.CountLiveConsoleSignIns(context.Background(), sc.db, "", time.Now())
	require.NoError(t, err)
	require.Zero(t, n)

	rr = sc.do(owner, owner.request(t, http.MethodPost, "/console/signin/finish", finish))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	require.True(t, sc.signedIn(t, owner))

	// The ticket works once (a synced passkey reports no counter, so the
	// replay would verify again).
	again := signinNewBrowser(t)
	again.key = owner.key
	rr = sc.do(again, again.request(t, http.MethodPost, "/console/signin/finish", finish))
	require.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
	require.Contains(t, rr.Body.String(), "already used")

	// Without a proof a sign-in neither begins nor finishes.
	unsigned := signinNewBrowser(t)
	unsigned.unsigned = true
	rr = sc.do(unsigned, unsigned.request(t, http.MethodPost, "/console/signin/begin", url.Values{}))
	require.Equal(t, http.StatusConflict, rr.Code)
	require.Contains(t, rr.Body.String(), "no proof of its key")
}

// Sign-out ends the row the cookie names with the cookie alone -- the
// sidebar's plain form carries no proof -- and clears the cookies; the same
// cookie and key sign nothing in afterwards. A form posted from another site
// signs nobody out.
func TestSignIn_SignOutEndsTheSignIn(t *testing.T) {
	sc := signinNewConsole(t, nil)
	a := signinAuthenticator(t)
	sc.signinEnroll(t, a, "laptop")
	owner := signinNewBrowser(t)
	cookie := sc.signinSignIn(t, owner, a)
	owner.cookies[unlockCookieName] = "an-unlock-cookie"

	cross := owner.request(t, http.MethodPost, "/console/logout", url.Values{})
	cross.Header.Set("Sec-Fetch-Site", "cross-site")
	require.Equal(t, http.StatusForbidden, sc.do(nil, cross).Code)
	require.True(t, sc.signedIn(t, owner), "a cross-site form signs nobody out")

	native := owner.request(t, http.MethodPost, "/console/logout", url.Values{})
	native.Header.Del(browserproof.Header) // a form navigation: the worker re-sends no POST
	native.Header.Del("Accept")
	rr := sc.do(owner, native)
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Equal(t, "/console/login", rr.Header().Get("Location"))
	require.NotContains(t, owner.cookies, cookieName, "the console cookie is cleared")
	require.NotContains(t, owner.cookies, unlockCookieName, "and the browser's unlock cookie with it")

	var reason string
	require.NoError(t, sc.db.QueryRow(`SELECT ended_reason FROM console_signins`).Scan(&reason))
	require.Equal(t, store.SignInEndSignedOut, reason)
	owner.cookies[cookieName] = cookie
	require.False(t, sc.signedIn(t, owner), "a signed-out cookie and its key sign nothing in")
}

// A sign-in lasts what the service says and no longer: past its expiry the
// cookie and key sign nothing in, and a request it does sign in carries the
// expiry as its deadline, so the live feed ends there.
func TestSignIn_Expiry(t *testing.T) {
	sc := signinNewConsole(t, nil)
	a := signinAuthenticator(t)
	cred := sc.signinEnroll(t, a, "laptop")
	b := signinNewBrowser(t)
	cookie, err := store.NewConsoleToken()
	require.NoError(t, err)
	b.cookies[cookieName] = cookie
	_, err = store.CreateConsoleSignIn(context.Background(), sc.db, cookie, b.publicKey(t), cred.ID, time.Hour, time.Now().Add(-2*time.Hour))
	require.NoError(t, err)
	require.False(t, sc.signedIn(t, b), "an expired sign-in")

	live, err := store.CreateConsoleSignIn(context.Background(), sc.db, cookie, b.publicKey(t), cred.ID, time.Hour, time.Now())
	require.NoError(t, err)
	require.True(t, sc.signedIn(t, b))
	req, _ := sc.cs.classified(b.request(t, http.MethodGet, "/console/events", nil))
	req, cancel := sc.cs.withSignIn(req)
	defer cancel()
	deadline, ok := req.Context().Deadline()
	require.True(t, ok)
	require.WithinDuration(t, live.ExpiresAt, deadline, time.Second)
	require.Equal(t, approval.SignInLifetime, sc.svc.Lifetime())
}

// The live feed opened by a signed-in browser ends once its sign-in ends,
// not only at its expiry: a sign-out in another tab, or the passkey removed.
func TestSignIn_LiveFeedEndsWithTheSignIn(t *testing.T) {
	old := signInRecheck
	signInRecheck = 20 * time.Millisecond
	t.Cleanup(func() { signInRecheck = old })
	sc := signinNewConsole(t, nil)
	a := signinAuthenticator(t)
	cred := sc.signinEnroll(t, a, "laptop")
	owner := signinNewBrowser(t)
	sc.signinSignIn(t, owner, a)
	srv := httptest.NewServer(sc.mux)
	t.Cleanup(srv.Close)

	req := owner.request(t, http.MethodGet, "/console/events", nil)
	out, err := http.NewRequest(http.MethodGet, srv.URL+"/console/events", nil)
	require.NoError(t, err)
	out.Header = req.Header.Clone()
	resp, err := http.DefaultClient.Do(out)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, ": connected\n", line)

	_, err = store.RemoveApproverSigningOut(context.Background(), sc.db, cred.ID, time.Now())
	require.NoError(t, err)
	ended := make(chan error, 1)
	go func() {
		_, err := bufio.NewReader(resp.Body).ReadString(0)
		ended <- err
	}()
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the live feed outlived the sign-in")
	}
}

// Adding a passkey: the address is plain, the device's page gets a code that
// names its own passkey, and approving a code enrolls exactly the passkey it
// names. Another device's passkey, made the same way, stays waiting until its
// own code is approved, and no code works twice. Nothing the page answers
// enrolls anything.
func TestSignIn_AddPasskeyCodeCannotEnrollAnotherKey(t *testing.T) {
	sc := signinNewConsole(t, nil)
	ctx := context.Background()

	// The page is the same for every visitor: nothing in it is a secret.
	page := func() string {
		rr := sc.do(nil, httptest.NewRequest(http.MethodGet, approverOrigin+passkeyAddPath, nil))
		require.Equal(t, http.StatusOK, rr.Code)
		require.Equal(t, "no-store", rr.Header().Get("Cache-Control"))
		return rr.Body.String()
	}
	first := page()
	require.Equal(t, first, page())
	require.Contains(t, first, "Add a passkey on this device")
	require.NotContains(t, first, "sidebar", "a visitor who is not signed in sees no console chrome")

	owner, attacker := signinAuthenticator(t), signinAuthenticator(t)
	ownerAdd := sc.signinAdd(t, signinNewBrowser(t), owner, "owner phone")
	attackerAdd := sc.signinAdd(t, signinNewBrowser(t), attacker, "owner phone")
	ownerCode, attackerCode := signinString(t, ownerAdd, "code"), signinString(t, attackerAdd, "code")
	require.NotEqual(t, ownerCode, attackerCode)
	require.Regexp(t, `^[0-9A-HJKMNP-TV-Z]{4}-[0-9A-HJKMNP-TV-Z]{4}$`, ownerCode)
	require.Equal(t, "seamlessd approvers approve --code "+ownerCode, signinString(t, ownerAdd, "command"))
	n, err := store.CountApprovers(ctx, sc.db)
	require.NoError(t, err)
	require.Zero(t, n, "creating a passkey enrolls nothing")

	cred, e, err := store.ApprovePasskeyEnrollment(ctx, sc.db, ownerCode, time.Now())
	require.NoError(t, err)
	require.Equal(t, owner.CredentialID(), cred.CredentialID, "the code enrolls the passkey it names")
	require.Equal(t, signinString(t, ownerAdd, "fingerprint"), store.ApproverKeyFingerprint(cred.PublicKey))
	require.Equal(t, e.Credential.PublicKey, cred.PublicKey)
	require.Equal(t, store.AddedByCode, cred.AddedBy)
	require.Equal(t, "localhost", cred.RPID)
	_, _, err = store.ApprovePasskeyEnrollment(ctx, sc.db, ownerCode, time.Now())
	require.ErrorIs(t, err, store.ErrEnrollmentNotFound, "a code works once")

	creds, err := store.ApproverCredentials(ctx, sc.db, store.ApproverPasskey)
	require.NoError(t, err)
	require.Len(t, creds, 1, "the attacker's passkey is not enrolled by the owner's code")

	// The attacker's passkey signs nothing in; the owner's does.
	b := signinNewBrowser(t)
	rr := sc.do(b, b.request(t, http.MethodPost, "/console/signin/begin", url.Values{}))
	begun := signinJSON(t, rr)
	_, err = attacker.Get(begun["options"], approverOrigin)
	require.Error(t, err, "the attacker's credential is not among the allowed ones")
	sc.signinSignIn(t, b, owner)
	require.True(t, sc.signedIn(t, b))

	// The device pages' states: the owner's is enrolled, the attacker's waits.
	status := func(added map[string]json.RawMessage) string {
		rr := sc.do(nil, httptest.NewRequest(http.MethodGet,
			approverOrigin+passkeyAddPath+"/status?id="+url.QueryEscape(signinString(t, added, "id")), nil))
		return signinString(t, signinJSON(t, rr), "state")
	}
	require.Equal(t, enrollmentEnrolled, status(ownerAdd))
	require.Equal(t, enrollmentWaiting, status(attackerAdd))
	rr = sc.do(nil, httptest.NewRequest(http.MethodGet, approverOrigin+passkeyAddPath+"/status?id=01NOSUCHTHING", nil))
	require.Equal(t, http.StatusNotFound, rr.Code)

	// A finish whose name differs from the begin's fails its challenge, so a
	// page cannot relabel the passkey it was begun for.
	d := signinNewBrowser(t)
	rr = sc.do(d, d.request(t, http.MethodPost, "/console/passkeys/add/begin", url.Values{"name": {"tablet"}}))
	begun = signinJSON(t, rr)
	created, err := signinAuthenticator(t).Create(begun["options"], approverOrigin)
	require.NoError(t, err)
	rr = sc.do(d, d.request(t, http.MethodPost, "/console/passkeys/add/finish", url.Values{
		"ticket": {signinString(t, begun, "ticket")}, "name": {"the owner's laptop"}, "response": {string(created)}}))
	require.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())

	// An enrolled passkey is refused when added again: it signs in as it is.
	again := sc.do(d, d.request(t, http.MethodPost, "/console/passkeys/add/begin", url.Values{"name": {"again"}}))
	_, err = owner.Create(signinJSON(t, again)["options"], approverOrigin)
	require.Error(t, err, "the browser is told to exclude an enrolled passkey")
}

// The add page's command names the daemon's account under peer_auth, as the
// terminal unlock's does: the owner approves as that account there, and as
// themselves without peer_auth.
func TestSignIn_AddPasskeyCommandFollowsTheTier(t *testing.T) {
	plain := signinNewConsole(t, nil)
	added := plain.signinAdd(t, signinNewBrowser(t), signinAuthenticator(t), "phone")
	require.Equal(t, "seamlessd approvers approve --code "+signinString(t, added, "code"), signinString(t, added, "command"))

	daemon := signinNewConsole(t, func(c *Config) {
		c.Terminal = &TerminalApprover{Account: "_seamless", Seamlessd: "/opt/seamless/bin/seamlessd", Config: "/opt/seamless/etc/seamless.yaml"}
	})
	added = daemon.signinAdd(t, signinNewBrowser(t), signinAuthenticator(t), "phone")
	require.Equal(t, "sudo -k -u _seamless env SEAMLESS_CONFIG=/opt/seamless/etc/seamless.yaml /opt/seamless/bin/seamlessd approvers approve --code "+
		signinString(t, added, "code"), signinString(t, added, "command"))
}

// A page drawn for a browser that holds a cookie but sent no proof (a hard
// reload skips the service worker) is the login page in its checking mode:
// it loads the browser key and asks again through the worker. The whole
// console goes there, not just locked reads, and a prefetch is refused.
func TestSignIn_UnprovenPageChecksThroughTheWorker(t *testing.T) {
	sc := signinNewConsole(t, nil)
	a := signinAuthenticator(t)
	sc.signinEnroll(t, a, "laptop")
	owner := signinNewBrowser(t)
	sc.signinSignIn(t, owner, a)

	owner.unsigned = true
	rr := sc.do(nil, owner.request(t, http.MethodGet, "/console/memories?w=7d", nil))
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Equal(t, "/console/login?next=/console/memories?w=7d", rr.Header().Get("Location"))

	rr = sc.do(nil, owner.request(t, http.MethodGet, "/console/login?next="+url.QueryEscape("/console/memories?w=7d"), nil))
	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	require.Contains(t, body, `<html lang="en" data-browser-proof="missing">`)
	require.Contains(t, body, `src="/console/static/browserkey.js"`)
	require.Contains(t, body, `data-next="/console/memories?w=7d"`)
	require.Equal(t, "no-store", rr.Header().Get("Cache-Control"))

	prefetch := owner.request(t, http.MethodGet, "/console/memories", nil)
	prefetch.Header.Set("Sec-Purpose", "prefetch")
	rr = sc.do(nil, prefetch)
	require.Equal(t, http.StatusServiceUnavailable, rr.Code)
	require.Equal(t, "no-store", rr.Header().Get("Cache-Control"))

	// The proven login page is not marked, and a signed-in browser is sent on.
	owner.unsigned = false
	rr = sc.do(nil, owner.request(t, http.MethodGet, "/console/login?next=/console/notes", nil))
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Equal(t, "/console/notes", rr.Header().Get("Location"))
	fresh := signinNewBrowser(t)
	rr = sc.do(nil, fresh.request(t, http.MethodGet, "/console/login", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	require.NotContains(t, rr.Body.String(), `data-browser-proof="missing"`)
	require.Contains(t, rr.Body.String(), "Sign in with a passkey")
	require.NotContains(t, rr.Body.String(), `name="key"`, "no key field")
}

// With sign-in off every route here is a 404 and the login page is the
// key's, unchanged: no header, no passkey card, the key's cookie working.
func TestSignIn_OffIsTheKeysSignIn(t *testing.T) {
	_, mux := newConsole(t)
	for _, target := range []string{"/console/passkeys/add", "/console/passkeys/add/status?id=x"} {
		rr := do(mux, httptest.NewRequest(http.MethodGet, target, nil))
		require.Equal(t, http.StatusNotFound, rr.Code, target)
		require.NotContains(t, rr.Body.String(), "sidebar", target)
	}
	for _, target := range []string{"/console/signin/begin", "/console/signin/finish", "/console/passkeys/add/begin", "/console/passkeys/add/finish"} {
		req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(""))
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		rr := do(mux, req)
		require.Equal(t, http.StatusNotFound, rr.Code, target)
	}
	rr := do(mux, httptest.NewRequest(http.MethodGet, "/console/login", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	require.Empty(t, rr.Header().Get(SignInHeader))
	require.Contains(t, rr.Body.String(), `name="key"`)
	require.NotContains(t, rr.Body.String(), "passkey")
	require.NotContains(t, rr.Body.String(), "browserkey.js")

	req := httptest.NewRequest(http.MethodGet, "/console/", nil)
	req.AddCookie(consoleCookie())
	require.Equal(t, http.StatusOK, do(mux, req).Code, "the key-derived cookie signs in, as it always did")
}

// The public routes stop a restricted account before anything else, like
// every console route, and refuse the socket: a browser signs in.
func TestSignIn_RefusedForARestrictedAccountAndTheSocket(t *testing.T) {
	sc := signinNewConsole(t, nil)
	view := restrictedConsoleView(t)
	for _, target := range []string{"/console/signin/begin", "/console/signin/finish", "/console/passkeys/add/begin", "/console/passkeys/add/finish"} {
		b := signinNewBrowser(t)
		req := b.request(t, http.MethodPost, target, url.Values{"name": {"k"}})
		rr := sc.do(nil, req.WithContext(access.WithView(req.Context(), view)))
		require.Equal(t, http.StatusForbidden, rr.Code, target)
		require.Contains(t, rr.Body.String(), "the console is the owner's surface", target)
	}
	for _, target := range []string{passkeyAddPath, passkeyAddPath + "/status?id=x"} {
		req := httptest.NewRequest(http.MethodGet, approverOrigin+target, nil)
		rr := sc.do(nil, req.WithContext(access.WithView(req.Context(), view)))
		require.Equal(t, http.StatusForbidden, rr.Code, target)
	}
	// The owner's own socket connection is not a browser either.
	p, err := access.New(config.PeerAuth{Enabled: true, Owner: "owner"}, func(n string) (access.Account, error) {
		if n == "owner" {
			return access.Account{UID: 501, Name: "owner"}, nil
		}
		return access.Account{}, fmt.Errorf("no %s", n)
	})
	require.NoError(t, err)
	sock, ok := p.PeerView(501)
	require.True(t, ok)
	for _, target := range []string{"/console/signin/begin", "/console/passkeys/add/begin"} {
		b := signinNewBrowser(t)
		req := b.request(t, http.MethodPost, target, url.Values{"name": {"k"}})
		rr := sc.do(nil, req.WithContext(access.WithView(req.Context(), sock)))
		require.Equal(t, http.StatusForbidden, rr.Code, target)
		require.Contains(t, rr.Body.String(), "never over the socket", target)
	}
	var waiting int
	require.NoError(t, sc.db.QueryRow(`SELECT COUNT(*) FROM passkey_enrollments`).Scan(&waiting))
	require.Zero(t, waiting)
}

// A signed-in browser is the owner's console for open projects; a locked
// project still needs an unlock, as before.
func TestSignIn_LockedProjectsStillNeedAnUnlock(t *testing.T) {
	var svc *approval.SignIn
	lc := signinNewLockedConsole(t, func(c *Config) {
		v, err := passkey.New(passkey.Config{RPID: "localhost", Origins: []string{approverOrigin}})
		require.NoError(t, err)
		svc, err = approval.NewSignIn(approval.SignInConfig{DB: c.WriteDB, Passkeys: v, Origins: []string{approverOrigin}})
		require.NoError(t, err)
		c.SignIn = svc
	})
	sc := &signinConsole{db: lc.db, mux: lc.mux, svc: svc, cs: lc.cs}
	a := signinAuthenticator(t)
	sc.signinEnroll(t, a, "laptop")
	owner := &signinBrowser{key: lc.key, cookies: map[string]string{}}
	cookie := sc.signinSignIn(t, owner, a)

	rr := sc.do(nil, owner.request(t, http.MethodGet, "/console/memories", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	require.Contains(t, rr.Body.String(), openMarker, "the signed-in browser reads the open project")
	require.NotContains(t, rr.Body.String(), lockedSecret, "and not the locked one")

	// The terminal unlock, begun by this signed-in browser: lc.browser adds
	// the key-derived cookie, which signs nothing in now, so the sign-in's
	// cookie goes with it.
	req := lc.browser(http.MethodPost, "/console/unlock/"+lockedSlug+"/terminal", "", url.Values{})
	req.Header.Set("Cookie", cookieName+"="+cookie)
	rr = sc.do(owner, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var begun terminalBegun
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &begun))
	lc.approveCode(t, lockedSlug, begun.Code)
	rr = sc.do(nil, owner.request(t, http.MethodGet, "/console/memories/"+lc.memID, nil))
	require.Equal(t, http.StatusOK, rr.Code)
	require.Contains(t, rr.Body.String(), lockedSecret, "the unlock opens it in this browser")
}

func signinAuthenticator(t *testing.T) *passkeytest.Authenticator {
	t.Helper()
	a, err := passkeytest.New(false)
	require.NoError(t, err)
	return a
}
