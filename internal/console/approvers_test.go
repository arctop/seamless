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

// enrolOverHTTP runs a passkey enrolment through the console routes.
func enrolOverHTTP(t *testing.T, mux *http.ServeMux, a *passkeytest.Authenticator, name, code string) map[string]json.RawMessage {
	t.Helper()
	status, begun := postCeremony(t, mux, "/console/approvers/passkey/begin", url.Values{"name": {name}, "code": {code}})
	require.Equal(t, http.StatusOK, status, string(begun["error"]))
	var id string
	require.NoError(t, json.Unmarshal(begun["id"], &id))
	created, err := a.Create(begun["options"], approverOrigin)
	require.NoError(t, err)
	status, fin := postCeremony(t, mux, "/console/approvers/passkey/finish", url.Values{"id": {id}, "response": {string(created)}})
	require.Equal(t, http.StatusOK, status, string(fin["error"]))
	fin["_id"] = json.RawMessage(`"` + id + `"`)
	return fin
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
	status, out := postCeremony(t, mux, "/console/approvers/passkey/begin", url.Values{"name": {"k"}, "code": {"c"}})
	require.Equal(t, http.StatusNotFound, status)
	require.Contains(t, ceremonyError(t, out), "no project is locked")
}

func TestApprovers_EnrolAndList(t *testing.T) {
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
	require.Contains(t, ceremonyError(t, out), "needs an enrolment code")
	status, out = postCeremony(t, mux, "/console/approvers/passkey/begin", url.Values{"name": {"key"}, "code": {"AAAA-BBBB-CCCC-DDDD"}})
	require.Equal(t, http.StatusBadRequest, status)
	require.Contains(t, ceremonyError(t, out), "not valid")

	code, _, err := store.MintEnrolCode(ctx, db, store.ApproverPasskey, time.Now())
	require.NoError(t, err)
	a, err := passkeytest.New(true)
	require.NoError(t, err)
	fin := enrolOverHTTP(t, mux, a, "security key", code)
	require.Contains(t, string(fin["name"]), "security key")

	body = page("localhost:8081")
	require.Contains(t, body, "security key")
	require.Contains(t, body, "an enrolment code")
	require.NotContains(t, body, "Nothing is enrolled yet")

	// A second passkey with no code: the first approves it.
	b, err := passkeytest.New(false)
	require.NoError(t, err)
	fin = enrolOverHTTP(t, mux, b, "phone", "")
	require.Contains(t, fin, "approve")
	var id string
	require.NoError(t, json.Unmarshal(fin["_id"], &id))
	assertion, err := a.Get(fin["approve"], approverOrigin)
	require.NoError(t, err)
	status, done := postCeremony(t, mux, "/console/approvers/passkey/approve", url.Values{"id": {id}, "response": {string(assertion)}})
	require.Equal(t, http.StatusOK, status, string(done["error"]))
	require.Contains(t, page("localhost:8081"), "approved by security key")

	n, err := store.CountApprovers(ctx, db)
	require.NoError(t, err)
	require.Equal(t, 2, n)
}

// A cookie-authenticated ceremony POST from another origin is refused like
// every console write (console-csrf-origin-check-contract).
func TestApprovers_CrossOriginRefused(t *testing.T) {
	_, mux := newApproversConsole(t)
	req := httptest.NewRequest(http.MethodPost, "/console/approvers/passkey/begin", strings.NewReader("name=k&code=c"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: cookieName, Value: consoleToken(testKey)})
	req.Header.Set("Origin", "http://localhost:9999")
	rr := do(mux, req)
	require.Equal(t, http.StatusForbidden, rr.Code)
}

// No console route mints an enrolment code: after every Approvers route has
// been hit, the code table is still empty.
func TestApprovers_NoRouteMintsACode(t *testing.T) {
	db, mux := newApproversConsole(t)
	for _, path := range []string{"/console/approvers/passkey/begin", "/console/approvers/passkey/finish", "/console/approvers/passkey/approve"} {
		postCeremony(t, mux, path, url.Values{"name": {"k"}, "code": {""}, "id": {"x"}, "response": {"{}"}})
	}
	do(mux, authedReq(httptest.NewRequest(http.MethodGet, "/console/approvers", nil)))
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM approver_enrol_codes`).Scan(&n))
	require.Zero(t, n)
}
