package console

import (
	"context"
	"database/sql"
	"encoding/json"
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
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/passkey"
	"github.com/arctop/seamless/internal/passkey/passkeytest"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

const approverOrigin = "http://localhost:8081"

func newApproversConsole(t *testing.T) (*sql.DB, *http.ServeMux) {
	t.Helper()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	v, err := passkey.New(passkey.Config{RPID: "localhost", Origins: []string{approverOrigin}})
	require.NoError(t, err)
	svc, err := New(Config{
		DB: db, Events: events.NewRecorder(db), APIKey: testKey, Level: "advanced",
		Approvals: approval.New(approval.Config{DB: db, Passkeys: v, Origins: []string{approverOrigin}}),
	})
	require.NoError(t, err)
	mux := http.NewServeMux()
	svc.Register(mux)
	return db, mux
}

// postCeremony posts one ceremony step as the page's script does, with the
// bearer key, and decodes the JSON answer.
func postCeremony(t *testing.T, mux *http.ServeMux, path string, form url.Values) (int, map[string]json.RawMessage) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	rr := do(mux, req)
	var out map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out), rr.Body.String())
	return rr.Code, out
}

func ceremonyError(t *testing.T, out map[string]json.RawMessage) string {
	t.Helper()
	var msg string
	require.NoError(t, json.Unmarshal(out["error"], &msg))
	return msg
}

// finishOverHTTP runs a passkey enrollment's begin and finish through the
// console routes and returns its id and finish's answer: by code, that names
// the key to mint a code for; otherwise it asks an existing passkey's approval.
func finishOverHTTP(t *testing.T, mux *http.ServeMux, a *passkeytest.Authenticator, name string, byCode bool) (string, map[string]json.RawMessage) {
	t.Helper()
	form := url.Values{"name": {name}}
	if byCode {
		form.Set("by_code", "1")
	}
	status, begun := postCeremony(t, mux, "/console/approvers/passkey/begin", form)
	require.Equal(t, http.StatusOK, status, string(begun["error"]))
	var id string
	require.NoError(t, json.Unmarshal(begun["id"], &id))
	created, err := a.Create(begun["options"], approverOrigin)
	require.NoError(t, err)
	status, fin := postCeremony(t, mux, "/console/approvers/passkey/finish", url.Values{"id": {id}, "response": {string(created)}})
	require.Equal(t, http.StatusOK, status, string(fin["error"]))
	return id, fin
}

// confirmOverHTTP mints the code for the key finish named, as the owner's
// terminal does, and confirms the enrollment with it.
func confirmOverHTTP(t *testing.T, db *sql.DB, mux *http.ServeMux, id string, fin map[string]json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	var fp string
	require.NoError(t, json.Unmarshal(fin["fingerprint"], &fp))
	code, _, err := store.MintEnrollCode(context.Background(), db, store.ApproverPasskey, fp, time.Now())
	require.NoError(t, err)
	status, done := postCeremony(t, mux, "/console/approvers/passkey/confirm", url.Values{"id": {id}, "code": {code}})
	require.Equal(t, http.StatusOK, status, string(done["error"]))
	return done
}

// enrollPasskeyByCode enrolls a through svc directly: begin, create, finish,
// mint for the key finish named, confirm.
func enrollPasskeyByCode(t *testing.T, db *sql.DB, svc *approval.Service, a *passkeytest.Authenticator, name string) store.ApproverCredential {
	t.Helper()
	ctx := context.Background()
	begun, err := svc.BeginPasskey(ctx, name, true)
	require.NoError(t, err)
	created, err := a.Create(begun.Options, approverOrigin)
	require.NoError(t, err)
	fin, err := svc.FinishPasskey(ctx, begun.ID, created)
	require.NoError(t, err)
	code, _, err := store.MintEnrollCode(ctx, db, store.ApproverPasskey, fin.Fingerprint, time.Now())
	require.NoError(t, err)
	cred, err := svc.ConfirmPasskeyCode(ctx, begun.ID, code)
	require.NoError(t, err)
	return cred
}

// authed adds the bearer key to a request.
func authedReq(req *http.Request) *http.Request {
	req.Header.Set("Authorization", "Bearer "+testKey)
	return req
}

func TestApprovers_OffIsNotFound(t *testing.T) {
	_, mux := newConsole(t)
	rr := do(mux, authedReq(httptest.NewRequest(http.MethodGet, "/console/approvers", nil)))
	require.Equal(t, http.StatusNotFound, rr.Code, "no project is locked: the page does not exist")
	for _, path := range []string{"/console/approvers/passkey/begin", "/console/approvers/passkey/finish",
		"/console/approvers/passkey/approve", "/console/approvers/passkey/confirm"} {
		status, out := postCeremony(t, mux, path, url.Values{"name": {"k"}, "by_code": {"1"}, "id": {"x"}, "code": {"c"}, "response": {"{}"}})
		require.Equal(t, http.StatusNotFound, status, path)
		require.Contains(t, ceremonyError(t, out), "no project is locked", path)
	}
}

func TestApprovers_EnrollAndList(t *testing.T) {
	db, mux := newApproversConsole(t)
	ctx := context.Background()

	page := func(host string) string {
		req := authedReq(httptest.NewRequest(http.MethodGet, "/console/approvers", nil))
		req.Host = host
		rr := do(mux, req)
		require.Equal(t, http.StatusOK, rr.Code)
		return rr.Body.String()
	}
	body := page("localhost:8081")
	require.Contains(t, body, "Nothing is enrolled yet")
	require.Contains(t, body, `id="approver-form"`, "on the relying party's origin, the form is offered")
	require.Contains(t, page("127.0.0.1:8081"), "can only be created on", "on 127.0.0.1 the page sends the owner to localhost")

	status, out := postCeremony(t, mux, "/console/approvers/passkey/begin", url.Values{"name": {"key"}})
	require.Equal(t, http.StatusBadRequest, status)
	require.Contains(t, ceremonyError(t, out), "needs a one-time enrollment code")

	a, err := passkeytest.New(true)
	require.NoError(t, err)
	id, fin := finishOverHTTP(t, mux, a, "security key", true)
	require.NotContains(t, fin, "enrolled", "by code, finish stores nothing: the code comes next")
	status, out = postCeremony(t, mux, "/console/approvers/passkey/confirm", url.Values{"id": {id}, "code": {"AAAA-BBBB-CCCC-DDDD"}})
	require.Equal(t, http.StatusBadRequest, status)
	require.Contains(t, ceremonyError(t, out), "not valid")
	n, err := store.CountApprovers(ctx, db)
	require.NoError(t, err)
	require.Zero(t, n)

	done := confirmOverHTTP(t, db, mux, id, fin)
	require.Contains(t, string(done["name"]), "security key")
	require.NotEmpty(t, done["enrolled"])

	body = page("localhost:8081")
	require.Contains(t, body, "security key")
	require.Contains(t, body, "an enrollment code")
	require.NotContains(t, body, "Nothing is enrolled yet")

	// A second passkey with no code: the first approves it.
	b, err := passkeytest.New(false)
	require.NoError(t, err)
	id, fin = finishOverHTTP(t, mux, b, "phone", false)
	require.Contains(t, fin, "approve")
	require.NotContains(t, fin, "fingerprint", "an approved enrollment needs no code")
	assertion, err := a.Get(fin["approve"], approverOrigin)
	require.NoError(t, err)
	status, done = postCeremony(t, mux, "/console/approvers/passkey/approve", url.Values{"id": {id}, "response": {string(assertion)}})
	require.Equal(t, http.StatusOK, status, string(done["error"]))
	require.Contains(t, page("localhost:8081"), "approved by security key")

	n, err = store.CountApprovers(ctx, db)
	require.NoError(t, err)
	require.Equal(t, 2, n)
}

// A cookie-authenticated ceremony POST from another origin is refused like
// every console write (console-csrf-origin-check-contract).
func TestApprovers_CrossOriginRefused(t *testing.T) {
	_, mux := newApproversConsole(t)
	req := httptest.NewRequest(http.MethodPost, "/console/approvers/passkey/begin", strings.NewReader("name=k&by_code=1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: cookieName, Value: consoleToken(testKey)})
	req.Header.Set("Origin", "http://localhost:9999")
	rr := do(mux, req)
	require.Equal(t, http.StatusForbidden, rr.Code)
}

// No console route mints an enrollment code: after every Approvers route has
// been hit, the code table is still empty.
func TestApprovers_NoRouteMintsACode(t *testing.T) {
	db, mux := newApproversConsole(t)
	for _, path := range []string{"/console/approvers/passkey/begin", "/console/approvers/passkey/finish",
		"/console/approvers/passkey/approve", "/console/approvers/passkey/confirm"} {
		postCeremony(t, mux, path, url.Values{"name": {"k"}, "by_code": {"1"}, "code": {"AAAA-BBBB-CCCC-DDDD"}, "id": {"x"}, "response": {"{}"}})
	}
	do(mux, authedReq(httptest.NewRequest(http.MethodGet, "/console/approvers", nil)))
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM approver_enroll_codes`).Scan(&n))
	require.Zero(t, n)
}

// A by-code finish names the key to mint a code for and the command that mints
// it, as the daemon's account where the daemon knows how that is reached. The
// key it names is the key the code then enrolls.
func TestApprovers_FinishNamesTheKeyAndItsCommand(t *testing.T) {
	ctx := context.Background()
	plainDB, plainMux := newApproversConsole(t)
	withTerminal := newAccessFixture(t)
	for name, tc := range map[string]struct {
		db      *sql.DB
		mux     *http.ServeMux
		command string
	}{
		"no terminal approver": {plainDB, plainMux, "seamlessd approvers enroll-code passkey --key "},
		"the daemon's account": {withTerminal.db, withTerminal.mux,
			"sudo -k -u _seamless env SEAMLESS_CONFIG=/opt/seamless/etc/seamless.yaml /opt/seamless/bin/seamlessd approvers enroll-code passkey --key "},
	} {
		t.Run(name, func(t *testing.T) {
			a, err := passkeytest.New(true)
			require.NoError(t, err)
			id, fin := finishOverHTTP(t, tc.mux, a, "security key", true)
			var fp, command string
			require.NoError(t, json.Unmarshal(fin["fingerprint"], &fp))
			require.NoError(t, json.Unmarshal(fin["command"], &command))
			require.Equal(t, fp, store.FormatKeyFingerprint(fp), "a fingerprint, in its printed form")
			require.Equal(t, tc.command+fp, command)
			require.NotContains(t, fin, "approve")

			done := confirmOverHTTP(t, tc.db, tc.mux, id, fin)
			var cred string
			require.NoError(t, json.Unmarshal(done["enrolled"], &cred))
			row, err := store.ApproverByID(ctx, tc.db, cred)
			require.NoError(t, err)
			require.Equal(t, fp, store.ApproverKeyFingerprint(row.PublicKey), "the key finish named is the key enrolled")
		})
	}
}

// The confirm route: a code minted for another key is refused and leaves the
// enrollment waiting; the right code enrolls it, once.
func TestApprovers_ConfirmRoute(t *testing.T) {
	db, mux := newApproversConsole(t)
	ctx := context.Background()
	a, err := passkeytest.New(true)
	require.NoError(t, err)
	id, fin := finishOverHTTP(t, mux, a, "security key", true)

	other, _, err := store.MintEnrollCode(ctx, db, store.ApproverPasskey, "Y2WK-2PG4-B7DB-FATZ", time.Now())
	require.NoError(t, err)
	status, out := postCeremony(t, mux, "/console/approvers/passkey/confirm", url.Values{"id": {id}, "code": {other}})
	require.Equal(t, http.StatusBadRequest, status)
	require.Contains(t, ceremonyError(t, out), "minted for another key")
	n, err := store.CountApprovers(ctx, db)
	require.NoError(t, err)
	require.Zero(t, n)

	done := confirmOverHTTP(t, db, mux, id, fin)
	require.Contains(t, string(done["name"]), "security key")

	status, out = postCeremony(t, mux, "/console/approvers/passkey/confirm", url.Values{"id": {id}, "code": {"AAAA-BBBB-CCCC-DDDD"}})
	require.Equal(t, http.StatusBadRequest, status)
	require.Contains(t, ceremonyError(t, out), "expired or was already used")
	n, err = store.CountApprovers(ctx, db)
	require.NoError(t, err)
	require.Equal(t, 1, n)
}

// A restricted account's ceremony POSTs all stop at the console's own gate,
// the confirm route with its siblings.
func TestApprovers_RefusedForARestrictedAccount(t *testing.T) {
	db, mux := newApproversConsole(t)
	view := restrictedConsoleView(t)
	for _, path := range []string{"/console/approvers/passkey/begin", "/console/approvers/passkey/finish",
		"/console/approvers/passkey/approve", "/console/approvers/passkey/confirm"} {
		form := url.Values{"name": {"k"}, "by_code": {"1"}, "id": {"x"}, "code": {"AAAA-BBBB-CCCC-DDDD"}, "response": {"{}"}}
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")
		rr := do(mux, req.WithContext(access.WithView(req.Context(), view)))
		require.Equal(t, http.StatusForbidden, rr.Code, path)
		require.Contains(t, rr.Body.String(), "the console is the owner's surface", path)
	}
	n, err := store.CountApprovers(context.Background(), db)
	require.NoError(t, err)
	require.Zero(t, n)
}
