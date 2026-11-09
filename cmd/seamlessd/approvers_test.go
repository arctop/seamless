package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

var codeRe = regexp.MustCompile(`[0-9A-HJKMNP-TV-Z]{4}(-[0-9A-HJKMNP-TV-Z]{4}){3}`)

func TestApprovers_Verbs(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	cfg := config.Defaults()
	cfg.Addr = "127.0.0.1:9123"
	cfg.PeerAuth.Locked = []string{"example-finance"}
	run := func(args ...string) (string, error) {
		var out bytes.Buffer
		err := approvers(ctx, db, cfg, &out, args, now)
		return out.String(), err
	}

	out, err := run("list")
	require.NoError(t, err)
	require.Contains(t, out, "No approver credentials are enrolled")

	out, err = run("enroll-code", "passkey")
	require.NoError(t, err)
	code := codeRe.FindString(out)
	require.NotEmpty(t, code, out)
	require.Contains(t, out, "http://localhost:9123/console/approvers", "the passkey page on the relying party's origin, never 127.0.0.1")
	require.NoError(t, store.CheckEnrollCode(ctx, db, store.ApproverPasskey, code, now))

	out, err = run("enroll-code", "touchid")
	require.NoError(t, err)
	require.Contains(t, out, "seam touchid enroll")

	for _, bad := range [][]string{{"enroll-code"}, {"enroll-code", "password"}, {"enroll-code", "passkey", "x"},
		{"remove"}, {"reset"}, {"reset", "--force"}, {"list", "x"}, {"frobnicate"}} {
		_, err := run(bad...)
		require.Error(t, err, "%v", bad)
	}

	cred, err := store.EnrollApprover(ctx, db, store.ApproverCredential{
		Kind: store.ApproverPasskey, CredentialID: []byte{1, 2, 3}, PublicKey: []byte{9}, Alg: -7, Name: "laptop",
	}, code, "", now)
	require.NoError(t, err)

	out, err = run("list")
	require.NoError(t, err)
	require.Contains(t, out, cred.ID)
	require.Contains(t, out, `"laptop"`)
	require.Contains(t, out, "an enrollment code")

	out, err = run("list", "--json")
	require.NoError(t, err)
	var got struct {
		Approvers []approverJSON `json:"approvers"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.Len(t, got.Approvers, 1)
	require.Equal(t, "010203", got.Approvers[0].CredentialID)

	out, err = run("remove", cred.ID)
	require.NoError(t, err)
	require.Contains(t, out, "Only the terminal approval path")
	_, err = run("remove", cred.ID)
	require.ErrorIs(t, err, store.ErrApproverNotFound)

	out, err = run("reset", "--yes")
	require.NoError(t, err)
	require.Contains(t, out, "Removed 0 credential(s)")
}

// doctor names the enrolled approvers while a project is locked, and warns
// when there are none.
func TestApproverCheck(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	for goos, touchID := range map[string]bool{"darwin": true, "linux": false, "windows": false} {
		c := approverCheck(ctx, db, goos)
		require.Equal(t, statusWarn, c.status)
		require.Contains(t, c.detail, "only the terminal path")
		require.Contains(t, c.detail, "to enroll one, mint a code with `seamlessd approvers enroll-code passkey`, run as the daemon's account")
		require.Equal(t, touchID, strings.Contains(c.detail, "seam touchid enroll"), goos)
	}

	now := time.Now()
	code, _, err := store.MintEnrollCode(ctx, db, store.ApproverPasskey, now)
	require.NoError(t, err)
	_, err = store.EnrollApprover(ctx, db, store.ApproverCredential{
		Kind: store.ApproverPasskey, CredentialID: []byte{1}, PublicKey: []byte{9}, Alg: -7, Name: "key",
	}, code, "", now)
	require.NoError(t, err)
	c := approverCheck(ctx, db, "darwin")
	require.Equal(t, statusOK, c.status)
	require.Contains(t, c.detail, "passkeys 1")
	require.Equal(t, statusInfo, approverCheck(ctx, nil, "darwin").status)
}

// With nothing enrolled, list and doctor give the same advice: a passkey code
// minted as the daemon's account everywhere, and seam touchid enroll too only
// on a Mac whose daemon serves the peer_auth socket the Touch ID app needs.
func TestApprovers_EmptyListAdvice(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	passkey := "No approver credentials are enrolled. To enroll one, mint a code with `seamlessd approvers enroll-code passkey`, " +
		"run as the daemon's account"
	touchID := "; or, for this Mac's Touch ID, run `seam touchid enroll` in the owner's own terminal, which prints the command that mints its code"
	for _, tc := range []struct {
		goos     string
		peerAuth bool
		want     string
	}{
		{"darwin", true, passkey + touchID + ".\n"},
		{"darwin", false, passkey + ".\n"},
		{"linux", true, passkey + ".\n"},
		{"windows", true, passkey + ".\n"},
	} {
		cfg := config.Defaults()
		cfg.PeerAuth.Enabled = tc.peerAuth
		var out bytes.Buffer
		require.NoError(t, approversOn(ctx, db, cfg, &out, []string{"list"}, time.Now(), tc.goos))
		require.Equal(t, tc.want, out.String(), "%s peer_auth=%v", tc.goos, tc.peerAuth)
		if tc.peerAuth {
			advice := firstApproverAdvice(tc.goos, true)
			require.Contains(t, out.String(), advice)
			require.Contains(t, approverCheck(ctx, db, tc.goos).detail, "; to enroll one, "+advice, "doctor says it the same way")
		}
	}
}

// console-open signs in on the relying party's origin while a project is
// locked, and on the server URL exactly as before otherwise.
func TestConsoleBase(t *testing.T) {
	cfg := config.Defaults()
	cfg.Addr = "127.0.0.1:9123"
	require.Equal(t, cfg.ServerURL(), consoleBase(cfg), "peer_auth off: unchanged")
	cfg.PeerAuth.Locked = []string{"example-finance"}
	require.Equal(t, cfg.ServerURL(), consoleBase(cfg), "a locked list while disabled changes nothing")
	cfg.PeerAuth.Enabled = true
	require.Equal(t, "http://localhost:9123", consoleBase(cfg))
}

// A config that is not the daemon's own -- a peer_auth owner's client config,
// which holds no peer_auth block -- asks the daemon where passkeys work, so
// console-open signs in there rather than on 127.0.0.1. Asked and unanswered,
// or read by the daemon's own config, the base is what it always was.
func TestConsoleOpenBase_AsksTheDaemon(t *testing.T) {
	var hits atomic.Int32
	var mu sync.Mutex
	status, origin := http.StatusNotFound, ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/console/access" || r.URL.Query().Get("format") != "json" {
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			http.Error(w, "no key", http.StatusUnauthorized)
			return
		}
		mu.Lock()
		s, o := status, origin
		mu.Unlock()
		if s != http.StatusOK {
			http.Error(w, "no project is locked", s)
			return
		}
		_, _ = fmt.Fprintf(w, `{"requests":[],"origin":%q}`, o)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	set := func(st int, o string) {
		mu.Lock()
		status, origin = st, o
		mu.Unlock()
	}
	cfg := config.Config{AdvertisedURL: srv.URL, Role: config.RoleClient}
	cfg.MCP.APIKey = "test-key"

	set(http.StatusNotFound, "")
	require.Equal(t, srv.URL, consoleOpenBase(cfg), "a daemon that locks nothing: unchanged")

	set(http.StatusOK, "")
	require.Equal(t, srv.URL, consoleOpenBase(cfg), "no origin named: unchanged")

	localhost := "http://localhost:" + u.Port()
	set(http.StatusOK, localhost)
	require.Equal(t, localhost, consoleOpenBase(cfg), "the owner's client config signs in where passkeys work")

	set(http.StatusOK, "https://not-there.example.test")
	require.Equal(t, srv.URL, consoleOpenBase(cfg), "an origin nothing answers on: the config's own URL")

	// The daemon's own config asks nothing: it already knows.
	before := hits.Load()
	own := cfg
	own.PeerAuth = config.PeerAuth{Enabled: true, Owner: "owner", Locked: []string{"example-finance"}}
	_ = consoleOpenBase(own)
	require.Equal(t, before, hits.Load(), "a config with peer_auth on does not ask")

	// A default install's config: the same 404, the same URL.
	set(http.StatusNotFound, "")
	def := config.Defaults()
	def.Addr = u.Host
	def.MCP.APIKey = "test-key"
	require.Equal(t, def.ServerURL(), consoleOpenBase(def))
	require.Equal(t, def.ServerURL(), consoleBase(def), "and consoleBase is untouched")
}

// The daemon's answer is only ever a bare origin, and localhost only on the
// machine it names.
func TestUsableOrigin(t *testing.T) {
	local := config.Config{AdvertisedURL: "http://127.0.0.1:8081"}
	remote := config.Config{AdvertisedURL: "http://server.example.test:8081"}
	for _, tc := range []struct {
		origin string
		cfg    config.Config
		want   bool
	}{
		{"http://localhost:8081", local, true},
		{"https://localhost:8081", local, true},
		{"https://seamless.example.test", local, true},
		{"https://seamless.example.test", remote, true},
		{"http://localhost:8081", remote, false},
		{"http://seamless.example.test", remote, false},
		{"", local, false},
		{"localhost:8081", local, false},
		{"ftp://localhost:8081", local, false},
		{"https://seamless.example.test/console/", local, false},
		{"https://seamless.example.test?x=1", local, false},
		{"https://user@seamless.example.test", local, false},
	} {
		require.Equal(t, tc.want, usableOrigin(tc.origin, tc.cfg), "%q from %s", tc.origin, tc.cfg.AdvertisedURL)
	}
}

// The redirect a daemon would not send carries no key anywhere.
func TestDaemonOrigin_FollowsNoRedirect(t *testing.T) {
	var strayed atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		strayed.Add(1)
		_, _ = w.Write([]byte(`{"origin":"http://localhost:1"}`))
	}))
	t.Cleanup(elsewhere.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/console/access?format=json", http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	cfg := config.Config{AdvertisedURL: srv.URL}
	cfg.MCP.APIKey = "test-key"
	require.Empty(t, daemonOrigin(cfg))
	require.Zero(t, strayed.Load(), "the key follows no redirect")
}

// A socket config cannot drive a browser: console-open says so, and where the
// console is when the daemon can be asked. The socket dial is real.
func TestSocketConsoleRefusal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a Unix-socket daemon is a POSIX deployment")
	}
	dir, err := os.MkdirTemp("", "sc")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"origin":"http://localhost:8091"}`))
	}), ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	cfg := config.Config{AdvertisedURL: "unix://" + sock, Role: config.RoleClient}
	err = socketConsoleRefusal(cfg)
	require.ErrorContains(t, err, "the daemon's socket (unix:"+sock+"), which a browser cannot dial")
	require.ErrorContains(t, err, "open http://localhost:8091/console/ and sign in with the daemon's key")
	require.ErrorContains(t, err, "SEAMLESS_CONFIG")

	gone := config.Config{AdvertisedURL: "unix:///nonexistent/seamless/d.sock", Role: config.RoleClient}
	require.ErrorContains(t, socketConsoleRefusal(gone), "open the daemon's TCP address and sign in")
}
