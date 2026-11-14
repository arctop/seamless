package console

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/store"
)

// retireSocketOwner is the owner's own connection on the peer_auth socket.
func retireSocketOwner(t *testing.T) access.View {
	t.Helper()
	p, err := access.New(config.PeerAuth{Enabled: true, Owner: "owner"}, func(n string) (access.Account, error) {
		if n == "owner" {
			return access.Account{UID: 501, Name: "owner"}, nil
		}
		return access.Account{}, fmt.Errorf("no %s", n)
	})
	require.NoError(t, err)
	sock, ok := p.PeerView(501)
	require.True(t, ok)
	return sock
}

// The terminal sign-in (signin_terminal.go), as a browser and the daemon's
// account meet it: the browser begins it and shows a code, the code is
// approved in the store as console-open --sign-in does, and that browser --
// its cookie with its own proof -- is then signed in, and no other.

// retireBegin begins a terminal sign-in from b and returns its id and code.
func retireBegin(t *testing.T, sc *signinConsole, b *signinBrowser) (string, string) {
	t.Helper()
	rr := sc.do(b, b.request(t, http.MethodPost, "/console/signin/terminal/begin", url.Values{}))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	got := signinJSON(t, rr)
	var id, code, command string
	require.NoError(t, json.Unmarshal(got["id"], &id))
	require.NoError(t, json.Unmarshal(got["code"], &code))
	require.NoError(t, json.Unmarshal(got["command"], &command))
	require.Equal(t, "seamlessd console-open --sign-in --code "+code, command, "the approval is the daemon's account's command")
	require.NotEmpty(t, b.cookies[cookieName], "the begin set the browser's cookie")
	return id, code
}

// retireStatus asks after terminal sign-in id as b.
func retireStatus(t *testing.T, sc *signinConsole, b *signinBrowser, id string) (int, string) {
	t.Helper()
	rr := sc.do(b, b.request(t, http.MethodGet, "/console/signin/terminal/status?id="+url.QueryEscape(id)+"&next=/console/tasks", nil))
	var got struct {
		State string `json:"state"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	return rr.Code, got.State
}

// retireSignedIn reports whether b reaches a console page signed in.
func retireSignedIn(t *testing.T, sc *signinConsole, b *signinBrowser) bool {
	t.Helper()
	rr := sc.do(nil, b.request(t, http.MethodGet, "/console/", nil))
	return rr.Code == http.StatusOK
}

func TestTerminalSignIn_SignsInTheBrowserThatBeganIt(t *testing.T) {
	sc := signinNewConsole(t, nil)
	b := signinNewBrowser(t)
	id, code := retireBegin(t, sc, b)
	require.False(t, retireSignedIn(t, sc, b), "a waiting sign-in signs nothing in")
	status, state := retireStatus(t, sc, b, id)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, string(store.TerminalSignInWaiting), state)

	_, err := store.ApproveTerminalSignIn(context.Background(), sc.db, code, approval.SignInLifetime, time.Now())
	require.NoError(t, err)
	status, state = retireStatus(t, sc, b, id)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, string(store.TerminalSignInLive), state)
	require.True(t, retireSignedIn(t, sc, b), "the browser that showed the code is signed in")

	// The cookie, read on another port, with no proof or another browser's
	// key: nothing.
	thief := signinNewBrowser(t)
	thief.cookies[cookieName] = b.cookies[cookieName]
	require.False(t, retireSignedIn(t, sc, thief), "the cookie with another browser's key")
	status, _ = retireStatus(t, sc, thief, id)
	require.Equal(t, http.StatusNotFound, status, "another browser finds no such sign-in")
	unsigned := &signinBrowser{key: b.key, cookies: map[string]string{cookieName: b.cookies[cookieName]}, unsigned: true}
	require.False(t, retireSignedIn(t, sc, unsigned), "the cookie without its proof")
}

// A code another browser began signs in that browser only: the one that
// holds the code's page, not one that saw the code.
func TestTerminalSignIn_ACodeCannotSignInADifferentBrowser(t *testing.T) {
	sc := signinNewConsole(t, nil)
	owner, agent := signinNewBrowser(t), signinNewBrowser(t)
	_, code := retireBegin(t, sc, owner)
	agentID, _ := retireBegin(t, sc, agent)

	_, err := store.ApproveTerminalSignIn(context.Background(), sc.db, code, approval.SignInLifetime, time.Now())
	require.NoError(t, err)
	require.True(t, retireSignedIn(t, sc, owner))
	require.False(t, retireSignedIn(t, sc, agent), "a browser that began its own sign-in is not signed in by another's code")
	_, state := retireStatus(t, sc, agent, agentID)
	require.Equal(t, string(store.TerminalSignInWaiting), state)
}

// The routes need the browser's proof, a write from this origin, the owner,
// and passkey sign-in on.
func TestTerminalSignIn_Refusals(t *testing.T) {
	sc := signinNewConsole(t, nil)
	unsigned := signinNewBrowser(t)
	unsigned.unsigned = true
	rr := sc.do(unsigned, unsigned.request(t, http.MethodPost, "/console/signin/terminal/begin", url.Values{}))
	require.Equal(t, http.StatusConflict, rr.Code, "no proof, no key to bind: nothing begins")
	require.Empty(t, unsigned.cookies[cookieName])

	b := signinNewBrowser(t)
	req := b.request(t, http.MethodPost, "/console/signin/terminal/begin", url.Values{})
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	require.Equal(t, http.StatusForbidden, sc.do(b, req).Code, "a form posted from another site")

	restricted := b.request(t, http.MethodPost, "/console/signin/terminal/begin", url.Values{})
	restricted = restricted.WithContext(access.WithView(restricted.Context(), restrictedConsoleView(t)))
	require.Equal(t, http.StatusForbidden, sc.do(b, restricted).Code, "a restricted account")
	sock := b.request(t, http.MethodPost, "/console/signin/terminal/begin", url.Values{})
	sock = sock.WithContext(access.WithView(sock.Context(), retireSocketOwner(t)))
	require.Equal(t, http.StatusForbidden, sc.do(b, sock).Code, "the owner's socket is not a browser")

	off := signinNewConsole(t, func(c *Config) { c.SignIn = nil })
	rr = off.do(b, b.request(t, http.MethodPost, "/console/signin/terminal/begin", url.Values{}))
	require.Equal(t, http.StatusNotFound, rr.Code, "a 404 while the key signs browsers in")
	rr = off.do(b, b.request(t, http.MethodGet, "/console/signin/terminal/status?id=x", nil))
	require.Equal(t, http.StatusNotFound, rr.Code)
}

// setup's view of the daemon's sign-in: the owner's only, the mode, the key,
// the passkeys of the relying party and the passkey sign-ins since a time.
func TestSignInState_TheOwnersView(t *testing.T) {
	sc := signinNewConsole(t, nil)
	b := signinNewBrowser(t)
	rr := sc.do(nil, b.request(t, http.MethodGet, "/console/signin/state", nil))
	require.NotEqual(t, http.StatusOK, rr.Code, "a browser that is not signed in learns nothing")

	read := func(cs *signinConsole, query string) SignInState {
		req := b.request(t, http.MethodGet, "/console/signin/state"+query, nil)
		req.Header.Set("Authorization", "Bearer "+testKey)
		rr := cs.do(nil, req)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		var st SignInState
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &st))
		return st
	}
	st := read(sc, "")
	require.Equal(t, "passkey", st.SignIn)
	require.True(t, st.Key)
	require.Equal(t, "localhost", st.RPID)
	require.Zero(t, st.Passkeys)

	since := time.Now().Add(-time.Minute)
	_, err := store.CreateConsoleSignIn(context.Background(), sc.db, "c", "k", "01CRED", time.Hour, time.Now())
	require.NoError(t, err)
	require.Equal(t, 1, read(sc, "?since="+strconv.FormatInt(since.Unix(), 10)).SignIns)
	require.Zero(t, read(sc, "?since="+strconv.FormatInt(time.Now().Add(time.Minute).Unix(), 10)).SignIns)

	retired := signinNewConsole(t, func(c *Config) { c.APIKey = "" })
	req := b.request(t, http.MethodGet, "/console/signin/state", nil)
	req = req.WithContext(access.WithView(req.Context(), retireSocketOwner(t)))
	rr = retired.do(nil, req)
	require.Equal(t, http.StatusOK, rr.Code, "the owner over the socket needs no key")
	var got SignInState
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	require.False(t, got.Key, "a retired key is said")
	require.Equal(t, "passkey", got.SignIn)
}

// With the key retired and no passkey enrolled at all, the login page still
// offers a way in -- the terminal sign-in -- and it works; the key's login
// page never shows it.
func TestTerminalSignIn_AWayInWithTheKeyRetiredAndNoPasskey(t *testing.T) {
	sc := signinNewConsole(t, func(c *Config) { c.APIKey = "" })
	b := signinNewBrowser(t)
	rr := sc.do(nil, b.request(t, http.MethodGet, "/console/login", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	require.Contains(t, rr.Body.String(), `id="terminal-go"`)
	require.Contains(t, rr.Body.String(), "Sign in from a terminal on the server")

	id, code := retireBegin(t, sc, b)
	_, err := store.ApproveTerminalSignIn(context.Background(), sc.db, code, approval.SignInLifetime, time.Now())
	require.NoError(t, err)
	_, state := retireStatus(t, sc, b, id)
	require.Equal(t, string(store.TerminalSignInLive), state)
	require.True(t, retireSignedIn(t, sc, b), "signed in with no key and no passkey")

	keyed := signinNewConsole(t, func(c *Config) { c.SignIn = nil })
	rr = keyed.do(nil, b.request(t, http.MethodGet, "/console/login", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	require.NotContains(t, rr.Body.String(), "terminal-go", "the key's login page is as it was")
}
