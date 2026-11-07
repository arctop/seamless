package console

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/store"
)

// The Touch ID helper's routes (plan step T), driven the way the helper drives
// them: over the peer_auth socket as the owner's uid, with no Origin, no
// cookie and no bearer key, signing with a software P-256 key in place of the
// Secure Enclave's.

// touchKey is a software stand-in for the helper's Secure Enclave key.
type touchKey struct {
	priv *ecdsa.PrivateKey
	pub  []byte
}

func newTouchKey(t *testing.T) touchKey {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	pub, err := priv.PublicKey.Bytes()
	require.NoError(t, err)
	return touchKey{priv: priv, pub: pub}
}

// sign is the Secure Enclave's ecdsaSignatureMessageX962SHA256: SHA-256 of
// msg, signed, ASN.1 DER.
func (k touchKey) sign(t *testing.T, msg []byte) string {
	t.Helper()
	digest := sha256.Sum256(msg)
	sig, err := ecdsa.SignASN1(rand.Reader, k.priv, digest[:])
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(sig)
}

// asOwnerSocket marks a request as the owner's uid on the peer_auth socket,
// as peerHandler would for the helper's connection.
func (f *accessFixture) asOwnerSocket(t *testing.T, req *http.Request) *http.Request {
	t.Helper()
	v, ok := f.policy.PeerView(501)
	require.True(t, ok)
	return req.WithContext(access.WithView(req.Context(), v))
}

// asOwnerTCP marks a request as the owner on TCP under peer_auth: the bearer
// key, and the owner's view with no kernel-reported uid.
func (f *accessFixture) asOwnerTCP(req *http.Request) *http.Request {
	req.Header.Set("Authorization", "Bearer "+testKey)
	return req.WithContext(access.WithView(req.Context(), f.policy.OwnerView()))
}

// form builds a POST of form to path.
func formPost(path string, form url.Values) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

func (f *accessFixture) socketJSON(t *testing.T, req *http.Request) (int, map[string]json.RawMessage) {
	t.Helper()
	return f.jsonDo(t, f.asOwnerSocket(t, req))
}

// enrolTouch enrols k over the socket with a fresh touchid code and returns
// the credential's id.
func (f *accessFixture) enrolTouch(t *testing.T, k touchKey, name string) string {
	t.Helper()
	code, _, err := store.MintEnrolCode(context.Background(), f.db, store.ApproverTouchID, time.Now())
	require.NoError(t, err)
	status, out := f.socketJSON(t, formPost("/console/approvers/touchid/enrol", url.Values{
		"code": {code}, "name": {name}, "public_key": {base64.StdEncoding.EncodeToString(k.pub)},
		"signature": {k.sign(t, approval.TouchIDEnrolMessage(code, k.pub))},
	}))
	require.Equal(t, http.StatusOK, status, string(out["error"]))
	var id, got string
	require.NoError(t, json.Unmarshal(out["id"], &id))
	require.NoError(t, json.Unmarshal(out["name"], &got))
	require.Equal(t, name, got)
	return id
}

// helperView is what the helper reads before it signs: the request's facts
// and its challenge, from the socket JSON.
type helperView struct {
	Request   approval.Facts `json:"request"`
	Status    string         `json:"status"`
	Challenge string         `json:"challenge"`
}

func (f *accessFixture) helperRead(t *testing.T, id string) helperView {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/console/access/"+id+"?format=json", nil)
	rr := do(f.mux, f.asOwnerSocket(t, req))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var v helperView
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &v))
	return v
}

// touchApproval is the helper's approve form for request v, signed by k over
// the given duration.
func touchApproval(t *testing.T, k touchKey, cred string, v helperView, duration string) url.Values {
	t.Helper()
	challenge, err := base64.StdEncoding.DecodeString(v.Challenge)
	require.NoError(t, err)
	msg := approval.TouchIDApproveMessage(v.Request.ID, challenge, v.Request.Digest, v.Request.Access, duration)
	return url.Values{"proof": {"touchid"}, "credential": {cred}, "signature": {k.sign(t, msg)},
		"duration": {duration}, "digest": {v.Request.Digest}}
}

func errorOf(t *testing.T, out map[string]json.RawMessage) string {
	t.Helper()
	var msg string
	require.NoError(t, json.Unmarshal(out["error"], &msg))
	return msg
}

func TestTouchID_EnrolOverTheSocket(t *testing.T) {
	f := newAccessFixture(t)
	ctx := context.Background()
	k := newTouchKey(t)
	id := f.enrolTouch(t, k, "Touch ID on laptop")

	// The approvers JSON lists it, which is how seam touchid enroll confirms it.
	status, out := f.socketJSON(t, httptest.NewRequest(http.MethodGet, "/console/approvers?format=json", nil))
	require.Equal(t, http.StatusOK, status)
	var list struct {
		Credentials []struct {
			ID, Kind, Name string
		} `json:"credentials"`
	}
	raw, err := json.Marshal(out)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &list))
	require.Len(t, list.Credentials, 1)
	require.Equal(t, id, list.Credentials[0].ID)
	require.Equal(t, store.ApproverTouchID, list.Credentials[0].Kind)
	require.Equal(t, "Touch ID on laptop", list.Credentials[0].Name)
	page := do(f.mux, authedReq(httptest.NewRequest(http.MethodGet, "/console/approvers", nil))).Body.String()
	require.Contains(t, page, `<span class="badge">Touch ID</span>`)
	require.Contains(t, page, "seam touchid enroll")

	// The same key again, with a fresh code: already enrolled.
	code, _, err := store.MintEnrolCode(ctx, f.db, store.ApproverTouchID, time.Now())
	require.NoError(t, err)
	status, out = f.socketJSON(t, formPost("/console/approvers/touchid/enrol", url.Values{
		"code": {code}, "name": {"again"}, "public_key": {base64.StdEncoding.EncodeToString(k.pub)},
		"signature": {k.sign(t, approval.TouchIDEnrolMessage(code, k.pub))},
	}))
	require.Equal(t, http.StatusBadRequest, status)
	require.Contains(t, errorOf(t, out), "already enrolled")
}

func TestTouchID_EnrolRefusals(t *testing.T) {
	f := newAccessFixture(t)
	ctx := context.Background()
	k, other := newTouchKey(t), newTouchKey(t)
	code, _, err := store.MintEnrolCode(ctx, f.db, store.ApproverTouchID, time.Now())
	require.NoError(t, err)
	passkeyCode, _, err := store.MintEnrolCode(ctx, f.db, store.ApproverPasskey, time.Now())
	require.NoError(t, err)
	pub := base64.StdEncoding.EncodeToString(k.pub)
	good := func() url.Values {
		return url.Values{"code": {code}, "name": {"laptop"}, "public_key": {pub},
			"signature": {k.sign(t, approval.TouchIDEnrolMessage(code, k.pub))}}
	}
	with := func(key, val string) url.Values {
		v := good()
		v.Set(key, val)
		return v
	}
	for _, tc := range []struct {
		name string
		form url.Values
		want string
	}{
		{"no code", with("code", ""), "needs an enrolment code"},
		{"a wrong code", url.Values{"code": {"AAAA-BBBB-CCCC-DDDD"}, "name": {"laptop"}, "public_key": {pub},
			"signature": {k.sign(t, approval.TouchIDEnrolMessage("AAAA-BBBB-CCCC-DDDD", k.pub))}}, "not valid"},
		{"a passkey's code", url.Values{"code": {passkeyCode}, "name": {"laptop"}, "public_key": {pub},
			"signature": {k.sign(t, approval.TouchIDEnrolMessage(passkeyCode, k.pub))}}, "not valid"},
		{"no name", with("name", " "), "a name of 1 to 64 characters"},
		{"a name on two lines", with("name", "a\nb"), "a name of 1 to 64 characters"},
		{"a public key that is not base64", with("public_key", "not*base64"), "65-byte uncompressed P-256 point"},
		{"a public key of the wrong length", with("public_key", base64.StdEncoding.EncodeToString(k.pub[:33])), "65-byte uncompressed P-256 point"},
		{"a signature that is not base64", with("signature", "%%%"), "standard base64"},
		{"no signature", with("signature", ""), "standard base64"},
		{"a signature by another key", with("signature", other.sign(t, approval.TouchIDEnrolMessage(code, k.pub))), "does not verify"},
		{"a signature over another code", with("signature", k.sign(t, approval.TouchIDEnrolMessage("ZZZZ-ZZZZ-ZZZZ-ZZZZ", k.pub))), "does not verify"},
	} {
		status, out := f.socketJSON(t, formPost("/console/approvers/touchid/enrol", tc.form))
		require.Equal(t, http.StatusBadRequest, status, tc.name)
		require.Contains(t, errorOf(t, out), tc.want, tc.name)
	}
	n, err := store.CountApprovers(ctx, f.db)
	require.NoError(t, err)
	require.Zero(t, n, "nothing refused was enrolled")

	// Off the socket: the TCP key, and a browser's cookie, never enrol one.
	status, out := f.jsonDo(t, f.asOwnerTCP(formPost("/console/approvers/touchid/enrol", good())))
	require.Equal(t, http.StatusForbidden, status)
	require.Contains(t, errorOf(t, out), "peer_auth socket only")
	req := formPost("/console/approvers/touchid/enrol", good())
	req.AddCookie(&http.Cookie{Name: cookieName, Value: consoleToken(testKey)})
	rr := do(f.mux, req)
	require.Equal(t, http.StatusForbidden, rr.Code, "a cookie POST without Origin is refused as every console write is")
	n, err = store.CountApprovers(ctx, f.db)
	require.NoError(t, err)
	require.Zero(t, n)

	// The code was never consumed by a refusal: the good form still enrols.
	status, out = f.socketJSON(t, formPost("/console/approvers/touchid/enrol", good()))
	require.Equal(t, http.StatusOK, status, string(out["error"]))
	// And a code works once.
	other2 := newTouchKey(t)
	status, out = f.socketJSON(t, formPost("/console/approvers/touchid/enrol", url.Values{"code": {code}, "name": {"second"},
		"public_key": {base64.StdEncoding.EncodeToString(other2.pub)}, "signature": {other2.sign(t, approval.TouchIDEnrolMessage(code, other2.pub))}}))
	require.Equal(t, http.StatusBadRequest, status)
	require.Contains(t, errorOf(t, out), "already used")

	var codes int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM approver_enrol_codes`).Scan(&codes))
	require.Equal(t, 2, codes, "the route consumed a code and minted none")
}

func TestTouchID_EnrolOffIsNotFound(t *testing.T) {
	_, mux := newConsole(t)
	rr := do(mux, authedReq(formPost("/console/approvers/touchid/enrol", url.Values{"code": {"x"}})))
	require.Equal(t, http.StatusNotFound, rr.Code, "no project is locked: the route does not exist")
}

func TestTouchID_ApproveALockRequest(t *testing.T) {
	f := newAccessFixture(t)
	ctx := context.Background()
	k, stranger := newTouchKey(t), newTouchKey(t)
	cred := f.enrolTouch(t, k, "laptop")
	id, _ := f.file(t)
	other, _ := f.anotherRequest(t)
	v := f.helperRead(t, id)
	require.Equal(t, store.ApprovalPending, v.Status)
	require.NotEmpty(t, v.Challenge, "a pending request read over the socket carries its challenge")
	ov := f.helperRead(t, other)

	approve := func(form url.Values) (int, string) {
		status, out := f.socketJSON(t, formPost("/console/access/"+id+"/approve", form))
		if status == http.StatusOK {
			return status, ""
		}
		return status, errorOf(t, out)
	}
	leases := func() int {
		got, err := store.ActiveAccessLeasesFor(ctx, f.db, []string{accAgent.Key()}, time.Now())
		require.NoError(t, err)
		return len(got)
	}

	good := touchApproval(t, k, cred, v, "1h")
	with := func(key, val string) url.Values {
		form := url.Values{}
		for kk, vv := range good {
			form[kk] = append([]string(nil), vv...)
		}
		form.Set(key, val)
		return form
	}
	for _, tc := range []struct {
		name   string
		form   url.Values
		status int
		want   string
	}{
		{"a wrong key", with("signature", touchApproval(t, stranger, cred, v, "1h").Get("signature")), http.StatusBadRequest, "does not verify"},
		{"a signature over another request", with("signature", touchApproval(t, k, cred, ov, "1h").Get("signature")), http.StatusBadRequest, "does not verify"},
		{"a changed duration", with("duration", "4h"), http.StatusBadRequest, "does not verify"},
		{"a changed digest", with("digest", strings.Repeat("0", 64)), http.StatusConflict, "facts changed"},
		{"a signature that is not base64", with("signature", "not base64!"), http.StatusBadRequest, "standard base64"},
		{"no credential", with("credential", ""), http.StatusBadRequest, "credential"},
		{"an unknown credential", with("credential", "01KNOSUCHKEY00000000000000"), http.StatusBadRequest, "does not verify"},
		{"an unknown proof", with("proof", "faceid"), http.StatusBadRequest, "the valid value is touchid"},
		{"an empty proof", with("proof", ""), http.StatusBadRequest, "the valid value is touchid"},
	} {
		status, msg := approve(tc.form)
		require.Equal(t, tc.status, status, tc.name)
		require.Contains(t, msg, tc.want, tc.name)
		require.Zero(t, leases(), tc.name)
	}

	// Over TCP, even with the owner's key and a good signature: refused.
	status, out := f.jsonDo(t, f.asOwnerTCP(formPost("/console/access/"+id+"/approve", good)))
	require.Equal(t, http.StatusForbidden, status)
	require.Contains(t, errorOf(t, out), "peer_auth socket only")
	status, out = f.jsonDo(t, authedReq(formPost("/console/access/"+id+"/approve", good)))
	require.Equal(t, http.StatusForbidden, status, "without a view at all (no peer_auth) too")
	require.Contains(t, errorOf(t, out), "peer_auth socket only")
	require.Zero(t, leases())

	status, msg := approve(good)
	require.Equal(t, http.StatusOK, status, msg)
	require.Equal(t, 1, leases(), "the agent's tree now holds the lease")
	a, err := store.AccessApprovalByID(ctx, f.db, id)
	require.NoError(t, err)
	require.Equal(t, cred, a.DecidedBy, "the Touch ID key decided it")
	row, err := store.ApproverByID(ctx, f.db, cred)
	require.NoError(t, err)
	require.NotNil(t, row.LastUsedAt)

	status, msg = approve(good)
	require.Equal(t, http.StatusConflict, status, "a replay after success")
	require.Contains(t, msg, "already decided")
	require.Equal(t, 1, leases())

	// Decided: the challenge is no longer handed out.
	require.Empty(t, f.helperRead(t, id).Challenge)
}

// The challenge is in the socket JSON of a pending request only: never over
// TCP and never on the page.
func TestTouchID_ChallengeOnlyOnTheSocket(t *testing.T) {
	f := newAccessFixture(t)
	id, _ := f.file(t)
	a, err := store.AccessApprovalByID(context.Background(), f.db, id)
	require.NoError(t, err)
	b64 := base64.StdEncoding.EncodeToString(a.Challenge)
	require.Equal(t, b64, f.helperRead(t, id).Challenge)

	for name, req := range map[string]*http.Request{
		"TCP JSON":       f.asOwnerTCP(httptest.NewRequest(http.MethodGet, "/console/access/"+id+"?format=json", nil)),
		"no-view JSON":   authedReq(httptest.NewRequest(http.MethodGet, "/console/access/"+id+"?format=json", nil)),
		"socket HTML":    f.asOwnerSocket(t, httptest.NewRequest(http.MethodGet, "/console/access/"+id, nil)),
		"TCP HTML":       f.asOwnerTCP(httptest.NewRequest(http.MethodGet, "/console/access/"+id, nil)),
		"socket listing": f.asOwnerSocket(t, httptest.NewRequest(http.MethodGet, "/console/access?format=json", nil)),
	} {
		rr := do(f.mux, req)
		require.Equal(t, http.StatusOK, rr.Code, name)
		require.NotContains(t, rr.Body.String(), b64, name)
		require.NotContains(t, rr.Body.String(), `"challenge"`, name)
	}
}

// Under peer_auth a read grant request is approved with a Touch ID proof
// through the same approve core.
func TestTouchID_ApproveAReadGrant(t *testing.T) {
	f := newAccessFixture(t)
	ctx := context.Background()
	_, err := store.EnsureProject(ctx, f.db, "example-hr", "example-hr")
	require.NoError(t, err)
	require.NoError(t, store.SetProjectIsolation(ctx, f.db, "example-hr", core.IsolationConfidential))
	_, err = store.EnsureProject(ctx, f.db, "example-ops", "example-ops")
	require.NoError(t, err)
	grants := approval.NewGrants(approval.GrantsConfig{Approvals: f.svc})
	cs, err := New(Config{DB: f.db, Events: events.NewRecorder(f.db), APIKey: testKey, Level: "advanced", Approvals: f.svc, Grants: grants})
	require.NoError(t, err)
	f.mux = http.NewServeMux()
	cs.Register(f.mux)
	sid, err := core.NewID()
	require.NoError(t, err)
	now := time.Now().UTC()
	require.NoError(t, store.CreateSession(ctx, f.db, core.Session{ID: sid, Name: "cc/ops", ProjectSlug: "example-ops",
		Status: core.SessionActive, Ambient: true, CWD: "/work/example-ops", CreatedAt: now, UpdatedAt: now}))
	require.NoError(t, store.SetSessionAgentProcess(ctx, f.db, sid, accAgent.AgentID()))

	k := newTouchKey(t)
	cred := f.enrolTouch(t, k, "laptop")
	status, filed := f.jsonDo(t, f.asAgent(t, formPost("/console/access/request", url.Values{"project": {"example-hr"}})))
	require.Equal(t, http.StatusOK, status, string(filed["error"]))
	var id string
	require.NoError(t, json.Unmarshal(filed["id"], &id))
	v := f.helperRead(t, id)
	require.Equal(t, store.AccessKindGrant, v.Request.Kind)
	require.NotEmpty(t, v.Challenge)

	// Over TCP the Touch ID proof is refused before the grant is judged.
	status, _ = f.jsonDo(t, f.asOwnerTCP(formPost("/console/access/"+id+"/approve", touchApproval(t, k, cred, v, "1h"))))
	require.Equal(t, http.StatusForbidden, status)
	status, out := f.socketJSON(t, formPost("/console/access/"+id+"/approve", touchApproval(t, k, cred, v, "4h")))
	require.Equal(t, http.StatusOK, status, string(out["error"]))
	require.Contains(t, string(out["from"]), "example-ops")
	live, err := store.LiveAccessGrants(ctx, f.db, time.Now())
	require.NoError(t, err)
	require.Len(t, live, 1)
	require.Equal(t, "example-hr", live[0].Project)
}

// No Touch ID route mints an enrolment code.
func TestTouchID_NoRouteMintsACode(t *testing.T) {
	f := newAccessFixture(t)
	for _, req := range []*http.Request{
		f.asOwnerSocket(t, formPost("/console/approvers/touchid/enrol", url.Values{"name": {"k"}})),
		f.asOwnerTCP(formPost("/console/approvers/touchid/enrol", url.Values{"name": {"k"}})),
	} {
		do(f.mux, req)
	}
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM approver_enrol_codes`).Scan(&n))
	require.Zero(t, n)
}

// anotherRequest files a second pending request, from another agent process
// of the owner's, so a signature over it can be tried on the first.
func (f *accessFixture) anotherRequest(t *testing.T) (id, short string) {
	t.Helper()
	agent := access.ProcessToken{PID: 201, Version: 2001, Boot: accessBoot}
	v, ok := f.policy.PeerChainView(501, []access.ProcessToken{{PID: 301, Version: 3001, Boot: accessBoot}, agent})
	require.True(t, ok)
	req := formPost("/console/access/request", url.Values{"project": {"example-finance"}, "reason": {"another"}})
	req.Header.Set("X-Seamless-Agent-Process", agent.AgentID())
	status, out := f.jsonDo(t, req.WithContext(access.WithView(req.Context(), v)))
	require.Equal(t, http.StatusOK, status, string(out["error"]))
	require.NoError(t, json.Unmarshal(out["id"], &id))
	require.NoError(t, json.Unmarshal(out["short_id"], &short))
	return id, short
}
