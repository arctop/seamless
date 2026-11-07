package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/update"

	// Test-only, as in access_test.go: the end-to-end case drives the real
	// console routes over a real Unix socket.
	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/console"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// Every test here runs against a stand-in (or a real, in-memory) console on a
// Unix socket, a stand-in release host, and a fake program runner: never a
// real codesign, spctl, ditto or open, and never GitHub.

// Placeholder team identifiers: the shape of an Apple team ID, nobody's.
const (
	fakeTeam  = "EXAMPLE001"
	otherTeam = "EXAMPLE002"
)

var fakeRelease = update.Version{Major: 0, Minor: 9, Patch: 1}

const fakeSelf = "/opt/example/bin/seam"

// shortSocketDir is a directory short enough for a Unix socket path (capped
// near 104 bytes on macOS, where t.TempDir() is longer than that allows).
func touchSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "tid")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// serveSocket serves h on a fresh Unix socket and returns its path.
func serveTouchSocket(t *testing.T, h http.Handler) string {
	t.Helper()
	sock := filepath.Join(touchSocketDir(t), "s.sock")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock
}

// stubDaemon is a stand-in for the console's access and approvers JSON.
type stubDaemon struct {
	mu   sync.Mutex
	keys []approverKey
	// terminal is the access list's terminal approver (nil omits it).
	terminal *terminalApprover
	// approversMissing answers the approvers route as a daemon that locks nothing.
	approversMissing bool
}

func (d *stubDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	d.mu.Lock()
	defer d.mu.Unlock()
	switch r.Method + " " + r.URL.Path {
	case "GET /console/access":
		_ = json.NewEncoder(w).Encode(accessList{Requests: []accessFacts{}, Terminal: d.terminal, Durations: []string{"session", "1h", "4h"}})
	case "GET /console/approvers":
		if d.approversMissing {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no project is locked, so there are no approvers to manage"}`))
			return
		}
		keys := d.keys
		if keys == nil {
			keys = []approverKey{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"credentials": keys, "passkeys_on": true})
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
	}
}

func (d *stubDaemon) addKey(id, name string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.keys = append(d.keys, approverKey{ID: id, Kind: "touchid", Name: name, AddedAt: time.Now()})
}

// releaseHost stands in for GitHub's release downloads: zip under its asset
// name, and a checksums.txt that lists sum for it.
type releaseHost struct {
	srv  *httptest.Server
	zip  []byte
	sums string
	seen []string
	mu   sync.Mutex
}

func newReleaseHost(t *testing.T, zip []byte) *releaseHost {
	t.Helper()
	h := &releaseHost{zip: zip}
	sum := sha256.Sum256(zip)
	h.sums = strings.Repeat("0", 64) + "  seamless_0.9.1_darwin_arm64.tar.gz\n" +
		hex.EncodeToString(sum[:]) + "  " + update.ApproverAsset(fakeRelease) + "\n"
	h.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.seen = append(h.seen, r.URL.Path+" UA="+r.Header.Get("User-Agent")+" auth="+r.Header.Get("Authorization"))
		h.mu.Unlock()
		switch r.URL.Path {
		case "/v0.9.1/" + update.ApproverAsset(fakeRelease):
			_, _ = w.Write(h.zip)
		case "/v0.9.1/checksums.txt":
			_, _ = w.Write([]byte(h.sums))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(h.srv.Close)
	return h
}

// touchRunner fakes the programs seam touchid runs. Each knob is what the
// real program would say.
type touchRunner struct {
	mu    sync.Mutex
	calls [][]string

	verifyFails    bool   // codesign --verify (the plain check)
	requirementBad bool   // codesign --verify -R=...
	identifier     string // the app's Identifier=
	appTeam        string // the app's TeamIdentifier= ("" is not set)
	selfTeam       string // this seam's TeamIdentifier=
	spctlFails     bool
	// helper runs as Seamless Approve when open is called; nil does nothing
	// (the owner cancelled).
	helper func(argv []string)
	// openCode is open's exit status.
	openCode int
}

func newTouchRunner() *touchRunner {
	return &touchRunner{identifier: approverBundleID, appTeam: fakeTeam, selfTeam: fakeTeam}
}

func (f *touchRunner) exec(_ context.Context, argv []string, _ io.Reader, stdout, stderr io.Writer) (int, error) {
	f.mu.Lock()
	f.calls = append(f.calls, slices.Clone(argv))
	f.mu.Unlock()
	switch {
	case argv[0] == "codesign" && slices.Contains(argv, "-dvv"):
		path := argv[len(argv)-1]
		team, ident := f.appTeam, f.identifier
		if path == fakeSelf {
			team, ident = f.selfTeam, "seam"
		}
		if team == "" {
			team = "not set"
		}
		_, _ = fmt.Fprintf(stderr, "Executable=%s\nIdentifier=%s\nFormat=app bundle\nAuthority=Developer ID Application: Example (%s)\nTeamIdentifier=%s\n",
			path, ident, team, team)
		return 0, nil
	case argv[0] == "codesign" && slices.Contains(argv, "--verify"):
		if strings.HasPrefix(argv[len(argv)-2], "-R=") {
			if f.requirementBad {
				_, _ = io.WriteString(stderr, "test-requirement: code failed to satisfy specified code requirement(s)")
				return 3, nil
			}
			return 0, nil
		}
		if f.verifyFails {
			_, _ = io.WriteString(stderr, "a sealed resource is missing or invalid")
			return 1, nil
		}
		return 0, nil
	case argv[0] == "spctl":
		if f.spctlFails {
			_, _ = io.WriteString(stderr, "rejected")
			return 3, nil
		}
		return 0, nil
	case argv[0] == "ditto":
		// ditto -x -k <zip> <dir> unpacks the app into dir; ditto <src> <dst>
		// copies the bundle to dst.
		dst := argv[len(argv)-1]
		if argv[1] == "-x" {
			dst = filepath.Join(dst, approverAppName)
		}
		return 0, os.MkdirAll(filepath.Join(dst, "Contents", "MacOS"), 0o755)
	case argv[0] == "open":
		if f.helper != nil {
			f.helper(argv)
		}
		return f.openCode, nil
	}
	return -1, fmt.Errorf("unexpected program %v", argv)
}

func (f *touchRunner) recorded() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// touchRig is a terminal env on macOS aimed at a daemon on a Unix socket.
type touchRig struct {
	e      *env
	out    *strings.Builder
	errb   *strings.Builder
	runner *touchRunner
	sock   string
	home   string
	host   *releaseHost
	world  touchIDWorld
}

func newTouchRig(t *testing.T, daemon http.Handler, typed string) *touchRig {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the peer_auth socket is a Unix deployment")
	}
	rig := &touchRig{runner: newTouchRunner(), sock: serveTouchSocket(t, daemon), home: t.TempDir()}
	rig.host = newReleaseHost(t, []byte("PK\x03\x04 the app"))
	e, _, _ := stubEnv()
	rig.out, rig.errb = &strings.Builder{}, &strings.Builder{}
	e.stdout, e.stderr = rig.out, rig.errb
	e.stdin = strings.NewReader(typed)
	e.terminal = func() bool { return true }
	e.goos = "darwin"
	e.exec = rig.runner.exec
	cfg := config.Defaults()
	cfg.Addr = "127.0.0.1:1" // nothing listens: only the socket can answer
	cfg.AdvertisedURL = "unix://" + rig.sock
	e.loadConfig = func() (config.Config, error) { return cfg, nil }
	rig.e = e
	client := rig.host.srv.Client()
	client.CheckRedirect = update.HTTPSOnlyRedirect
	rig.world = touchIDWorld{
		home:         func() (string, error) { return rig.home, nil },
		self:         func() (string, error) { return fakeSelf, nil },
		version:      func() (update.Version, bool) { return fakeRelease, true },
		client:       client,
		downloadBase: rig.host.srv.URL,
	}
	prev := newTouchIDWorld
	newTouchIDWorld = func() touchIDWorld { return rig.world }
	t.Cleanup(func() { newTouchIDWorld = prev })
	return rig
}

func (r *touchRig) run(argv ...string) int {
	return dispatch(context.Background(), r.e, append([]string{"touchid"}, argv...))
}

func (r *touchRig) target() string { return approverAppPath(r.home) }

// staging is the staging directory the install used, read off its ditto call.
func (r *touchRig) staging(t *testing.T) string {
	t.Helper()
	for _, c := range r.runner.recorded() {
		if c[0] == "ditto" {
			dst := c[len(c)-1]
			if c[1] != "-x" {
				dst = filepath.Dir(dst)
			}
			return dst
		}
	}
	t.Fatal("no ditto call")
	return ""
}

// leftovers lists what is in ~/Applications besides the app.
func (r *touchRig) leftovers(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(r.home, "Applications"))
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	var out []string
	for _, e := range entries {
		if e.Name() != approverAppName {
			out = append(out, e.Name())
		}
	}
	return out
}

func happyDaemon() *stubDaemon {
	return &stubDaemon{terminal: testTerminal}
}

// enrols makes the fake helper enrol a key on d.
func enrols(d *stubDaemon) func([]string) {
	return func(argv []string) { d.addKey("01KTOUCHIDKEY0000000000001", argv[len(argv)-1]) }
}

func TestTouchIDEnroll_ReleaseHappyPath(t *testing.T) {
	d := happyDaemon()
	rig := newTouchRig(t, d, "\n")
	rig.runner.helper = enrols(d)

	require.Equal(t, 0, rig.run("enroll", "--name", "Touch ID on laptop"), rig.errb.String())
	stage := rig.staging(t)
	staged := filepath.Join(stage, approverAppName)
	zip := filepath.Join(stage, update.ApproverAsset(fakeRelease))
	require.Equal(t, [][]string{
		{"ditto", "-x", "-k", zip, stage},
		{"codesign", "--verify", "--strict", "--deep", staged},
		{"codesign", "-dvv", staged},
		{"spctl", "--assess", "--type", "execute", staged},
		{"codesign", "-dvv", fakeSelf},
		{"codesign", "--verify", "--strict", "--deep",
			`-R=anchor apple generic and identifier "org.thereisnospoon.seamless.approve" and certificate leaf[subject.OU] = "` + fakeTeam + `"`, staged},
		{"open", "-W", "-n", "-a", rig.target(), "--args", "enroll", "--socket", rig.sock, "--name", "Touch ID on laptop"},
	}, rig.runner.recorded())
	require.Equal(t, filepath.Join(rig.home, "Applications"), filepath.Dir(stage), "staged beside the target, so the install is one rename")
	require.True(t, isDir(rig.target()), "installed")
	require.Empty(t, rig.leftovers(t), "the staging directory is gone")

	// The release host saw two anonymous fetches with the fixed User-Agent.
	require.Equal(t, []string{
		"/v0.9.1/checksums.txt UA=" + touchIDUserAgent + " auth=",
		"/v0.9.1/" + update.ApproverAsset(fakeRelease) + " UA=" + touchIDUserAgent + " auth=",
	}, rig.host.seen)

	out := rig.out.String()
	require.Contains(t, out, "Installed Seamless Approve at "+rig.target())
	require.Contains(t, out, "sudo -k -u _seamless env SEAMLESS_CONFIG=/opt/seamless/seamless.yaml /opt/seamless/bin/seamlessd approvers enroll-code touchid")
	require.Contains(t, out, `Enrolled the Touch ID key "Touch ID on laptop" (01KTOUCHIDKEY0000000000001)`)
}

// A tampered or foreign app is refused, and nothing is installed.
func TestTouchIDEnroll_RefusesATamperedApp(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*touchRig)
		want   string
	}{
		"a checksum mismatch":                  {func(r *touchRig) { r.host.zip = []byte("PK\x03\x04 another app") }, "does not match its checksum"},
		"an asset checksums.txt does not list": {func(r *touchRig) { r.host.sums = strings.Repeat("0", 64) + "  other.zip\n" }, "appears 0 times"},
		"a signature that does not verify":     {func(r *touchRig) { r.runner.verifyFails = true }, "code signature does not verify"},
		"a wrong identifier":                   {func(r *touchRig) { r.runner.identifier = "com.example.other" }, `signed as "com.example.other"`},
		"a wrong team":                         {func(r *touchRig) { r.runner.appTeam = otherTeam }, "signed by team " + otherTeam + ", and this seam by team " + fakeTeam},
		"no team":                              {func(r *touchRig) { r.runner.appTeam = "" }, "no Developer ID team"},
		"a signature not chaining to Apple":    {func(r *touchRig) { r.runner.requirementBad = true }, "does not chain to Apple"},
		"no notarization":                      {func(r *touchRig) { r.runner.spctlFails = true }, "Gatekeeper does not accept it"},
	} {
		t.Run(name, func(t *testing.T) {
			d := happyDaemon()
			rig := newTouchRig(t, d, "\n")
			rig.runner.helper = enrols(d)
			tc.mutate(rig)
			require.Equal(t, 1, rig.run("enroll"))
			require.Contains(t, rig.errb.String(), "refusing Seamless Approve")
			require.Contains(t, rig.errb.String(), tc.want)
			require.False(t, isDir(rig.target()), "nothing installed")
			require.Empty(t, rig.leftovers(t), "the staging directory is gone")
			for _, c := range rig.runner.recorded() {
				require.NotEqual(t, "open", c[0], "the refused app never runs")
			}
		})
	}
}

// A local build (--app) skips the release fetch and Gatekeeper's check, and
// still has to be the helper, intact, from seam's own team.
func TestTouchIDEnroll_LocalBuild(t *testing.T) {
	d := happyDaemon()
	rig := newTouchRig(t, d, "\n")
	rig.runner.helper = enrols(d)
	rig.world.version = func() (update.Version, bool) { return update.Version{}, false }
	src := filepath.Join(t.TempDir(), "build", approverAppName)
	require.NoError(t, os.MkdirAll(filepath.Join(src, "Contents"), 0o755))

	require.Equal(t, 0, rig.run("enroll", "--app", src), rig.errb.String())
	calls := rig.runner.recorded()
	require.Equal(t, []string{"ditto", src, filepath.Join(rig.staging(t), approverAppName)}, calls[0])
	for _, c := range calls {
		require.NotEqual(t, "spctl", c[0], "a local build is not notarized")
	}
	require.Empty(t, rig.host.seen, "nothing fetched")
	require.True(t, isDir(rig.target()))
}

// A source build of seam has no release asset: it needs --app.
func TestTouchIDEnroll_SourceBuildNeedsApp(t *testing.T) {
	rig := newTouchRig(t, happyDaemon(), "\n")
	rig.world.version = func() (update.Version, bool) { return update.Version{}, false }
	require.Equal(t, 1, rig.run("enroll"))
	require.Contains(t, rig.errb.String(), "this seam is not a release build")
	require.Contains(t, rig.errb.String(), "--app")
	require.Empty(t, rig.host.seen)
	require.Empty(t, rig.runner.recorded())
}

// An unsigned seam cannot check the publisher: the owner sees the team and
// decides.
func TestTouchIDEnroll_UnsignedSeamAsks(t *testing.T) {
	for answer, ok := range map[string]bool{"y\n\n": true, "n\n": false, "": false} {
		d := happyDaemon()
		rig := newTouchRig(t, d, answer)
		rig.runner.helper = enrols(d)
		rig.runner.selfTeam = ""
		src := filepath.Join(t.TempDir(), approverAppName)
		require.NoError(t, os.MkdirAll(src, 0o755))
		code := rig.run("enroll", "--app", src)
		require.Contains(t, rig.out.String(), "This seam is not signed with a Developer ID")
		require.Contains(t, rig.out.String(), "The app is signed by team "+fakeTeam+" (Developer ID Application: Example ("+fakeTeam+"))")
		if ok {
			require.Equal(t, 0, code, rig.errb.String())
			require.True(t, isDir(rig.target()))
			continue
		}
		require.Equal(t, 1, code, "%q", answer)
		require.Contains(t, rig.errb.String(), "not confirmed")
		require.False(t, isDir(rig.target()))
		for _, c := range rig.runner.recorded() {
			require.False(t, slices.ContainsFunc(c, func(a string) bool { return strings.HasPrefix(a, "-R=") }), "no team to pin: %v", c)
		}
	}
}

// An installed app is replaced only after a y/N; kept, it is checked and used.
func TestTouchIDEnroll_ExistingInstall(t *testing.T) {
	t.Run("kept", func(t *testing.T) {
		d := happyDaemon()
		rig := newTouchRig(t, d, "n\n\n")
		rig.runner.helper = enrols(d)
		marker := filepath.Join(rig.target(), "Contents", "kept")
		require.NoError(t, os.MkdirAll(filepath.Dir(marker), 0o755))
		require.NoError(t, os.WriteFile(marker, nil, 0o600))
		require.Equal(t, 0, rig.run("enroll"), rig.errb.String())
		require.Contains(t, rig.out.String(), "Replace it? [y/N]")
		require.Contains(t, rig.out.String(), "Keeping the installed app.")
		require.FileExists(t, marker, "the installed app is untouched")
		require.Empty(t, rig.host.seen, "nothing fetched")
		calls := rig.runner.recorded()
		require.Equal(t, []string{"codesign", "--verify", "--strict", "--deep", rig.target()}, calls[0], "the kept app is checked")
		require.Equal(t, "open", calls[len(calls)-1][0])
	})
	t.Run("replaced", func(t *testing.T) {
		d := happyDaemon()
		rig := newTouchRig(t, d, "y\n\n")
		rig.runner.helper = enrols(d)
		marker := filepath.Join(rig.target(), "Contents", "old")
		require.NoError(t, os.MkdirAll(filepath.Dir(marker), 0o755))
		require.NoError(t, os.WriteFile(marker, nil, 0o600))
		require.Equal(t, 0, rig.run("enroll"), rig.errb.String())
		require.NoFileExists(t, marker, "the new app replaced the old")
		require.True(t, isDir(rig.target()))
		require.Empty(t, rig.leftovers(t), "the previous app went with the staging directory")
	})
	t.Run("kept but tampered", func(t *testing.T) {
		rig := newTouchRig(t, happyDaemon(), "n\n\n")
		require.NoError(t, os.MkdirAll(rig.target(), 0o755))
		rig.runner.verifyFails = true
		require.Equal(t, 1, rig.run("enroll"))
		require.Contains(t, rig.errb.String(), "code signature does not verify")
		for _, c := range rig.runner.recorded() {
			require.NotEqual(t, "open", c[0])
		}
	})
}

func TestTouchIDEnroll_CancelledOrFailed(t *testing.T) {
	rig := newTouchRig(t, happyDaemon(), "\n")
	require.Equal(t, 1, rig.run("enroll"))
	require.Contains(t, rig.errb.String(), "no new Touch ID key is enrolled")
	require.True(t, isDir(rig.target()), "the app stays installed")

	rig = newTouchRig(t, happyDaemon(), "\n")
	rig.runner.openCode = 1
	require.Equal(t, 1, rig.run("enroll"))
	require.Contains(t, rig.errb.String(), "open could not start Seamless Approve")

	// No Return: nothing opens.
	rig = newTouchRig(t, happyDaemon(), "")
	require.Equal(t, 0, rig.run("enroll"), rig.errb.String())
	require.Contains(t, rig.out.String(), "Nothing enrolled. Seamless Approve stays installed")
	for _, c := range rig.runner.recorded() {
		require.NotEqual(t, "open", c[0])
	}
}

func TestTouchIDEnroll_Refusals(t *testing.T) {
	t.Run("not macOS", func(t *testing.T) {
		rig := newTouchRig(t, happyDaemon(), "\n")
		rig.e.goos = "linux"
		require.Equal(t, 1, rig.run("enroll"))
		require.Contains(t, rig.errb.String(), "the Touch ID approver exists only on macOS")
		require.Empty(t, rig.runner.recorded())
	})
	t.Run("no terminal", func(t *testing.T) {
		rig := newTouchRig(t, happyDaemon(), "\n")
		rig.e.terminal = func() bool { return false }
		rig.e.loadConfig = func() (config.Config, error) {
			t.Fatal("nothing may be read without the owner's terminal")
			return config.Config{}, nil
		}
		require.Equal(t, 1, rig.run("enroll"))
		require.Contains(t, rig.errb.String(), "seam touchid enroll is the owner's own terminal path")
		require.Contains(t, rig.errb.String(), "an agent cannot use it")
		require.Empty(t, rig.runner.recorded())
	})
	t.Run("no socket", func(t *testing.T) {
		rig := newTouchRig(t, happyDaemon(), "\n")
		cfg := config.Defaults()
		rig.e.loadConfig = func() (config.Config, error) { return cfg, nil }
		require.Equal(t, 1, rig.run("enroll"))
		require.Contains(t, rig.errb.String(), "needs server_url to name the peer_auth socket")
		require.Empty(t, rig.runner.recorded())
		require.Empty(t, rig.host.seen)
	})
	t.Run("a daemon that locks nothing", func(t *testing.T) {
		d := happyDaemon()
		d.approversMissing = true
		rig := newTouchRig(t, d, "\n")
		require.Equal(t, 1, rig.run("enroll"))
		require.Contains(t, rig.errb.String(), "no project is locked")
		require.Empty(t, rig.runner.recorded(), "nothing installs")
	})
	t.Run("no terminal approver", func(t *testing.T) {
		rig := newTouchRig(t, &stubDaemon{}, "\n")
		require.Equal(t, 1, rig.run("enroll"))
		require.Contains(t, rig.errb.String(), "seamlessd approvers enroll-code touchid")
		require.Empty(t, rig.runner.recorded())
	})
	t.Run("a terminal approver that is not one", func(t *testing.T) {
		rig := newTouchRig(t, &stubDaemon{terminal: &terminalApprover{Account: "-oops", Seamlessd: "/x", Config: "/y"}}, "\n")
		require.Equal(t, 1, rig.run("enroll"))
		require.Contains(t, rig.errb.String(), "not an account name")
	})
	for _, bad := range []string{"", "-x", "a\nb", strings.Repeat("k", 65)} {
		rig := newTouchRig(t, happyDaemon(), "\n")
		require.Equal(t, 1, rig.run("enroll", "--name", bad), "%q", bad)
		require.Contains(t, rig.errb.String(), "--name")
		require.Empty(t, rig.runner.recorded())
	}
	rig := newTouchRig(t, happyDaemon(), "\n")
	require.Equal(t, 1, rig.run("enroll", "--app", " "))
	require.Contains(t, rig.errb.String(), "--app needs the path")
}

func TestTouchID_DefaultKeyName(t *testing.T) {
	require.Equal(t, "Touch ID on laptop", defaultKeyName("laptop"))
	require.Equal(t, "Touch ID", defaultKeyName(""))
	require.Equal(t, "Touch ID", defaultKeyName(strings.Repeat("h", 60)), "too long to name: the bare default")
	require.Equal(t, store.MaxApproverNameRunes, maxApproverNameRunes)
	require.Equal(t, store.ApproverTouchID, approverKindTouchID)
}

func TestTouchID_ShellWords(t *testing.T) {
	require.Equal(t, []string{"sudo", "-u", "_seamless", "SEAMLESS_CONFIG=/opt/s/seamless.yaml", `'/Applications/My App/seamlessd'`, `'it'\''s'`, "''"},
		shellWords([]string{"sudo", "-u", "_seamless", "SEAMLESS_CONFIG=/opt/s/seamless.yaml", "/Applications/My App/seamlessd", "it's", ""}))
}

func TestTouchID_ParseSignature(t *testing.T) {
	sig := parseSignature("Executable=/x\nIdentifier=org.example.app\nPlatform identifier=27\nAuthority=Developer ID Application: Example (" + fakeTeam +
		")\nAuthority=Developer ID Certification Authority\nTeamIdentifier=" + fakeTeam + "\n")
	require.Equal(t, codeSignature{identifier: "org.example.app", team: fakeTeam, authority: "Developer ID Application: Example (" + fakeTeam + ")"}, sig)
	require.Empty(t, parseSignature("Identifier=a.out\nSignature=adhoc\nTeamIdentifier=not set\n").team)
	require.Empty(t, parseSignature(`TeamIdentifier=A" or "1`).team, "only a team ID's shape reaches a requirement")
}

func TestTouchIDStatus(t *testing.T) {
	d := happyDaemon()
	d.addKey("01KTOUCHIDKEY0000000000001", "Touch ID on laptop")
	rig := newTouchRig(t, d, "")
	require.Equal(t, 0, rig.run("status"), rig.errb.String())
	out := rig.out.String()
	require.Contains(t, out, "Seamless Approve: not installed at "+rig.target())
	require.Contains(t, out, "server_url: the peer_auth socket "+rig.sock)
	require.Contains(t, out, `01KTOUCHIDKEY0000000000001  "Touch ID on laptop"`)

	require.NoError(t, os.MkdirAll(rig.target(), 0o755))
	rig.out.Reset()
	require.Equal(t, 0, rig.run("status"), rig.errb.String())
	require.Contains(t, rig.out.String(), "Seamless Approve: installed at "+rig.target()+" ("+approverBundleID+", team "+fakeTeam+", signature intact)")

	rig.e.goos = "windows"
	require.Equal(t, 1, rig.run("status"))
	require.Contains(t, rig.errb.String(), "only on macOS")
}

func TestTouchID_Help(t *testing.T) {
	help := helpText()
	section := help[strings.Index(help, "\n"+groupAccess+":\n"):]
	section = section[:strings.Index(section[1:], "\n\n")+1]
	require.Contains(t, section, "seam touchid enroll [--app PATH] [--name NAME]")
	require.Contains(t, section, "seam touchid status")
	page := commandHelp(touchIDEnrollCmd)
	for _, want := range []string{"Nothing installs it but this command", "an agent cannot use it", "unix:///path", approverBundleID} {
		require.Contains(t, page, want)
	}
	e, out, _ := stubEnv()
	require.Equal(t, 0, dispatch(context.Background(), e, []string{"touchid", "enroll", "--help"}), "help needs no terminal")
	require.Contains(t, out.String(), "usage: seam touchid enroll")
}

// End to end against the real console routes on a real Unix socket: the
// daemon's access JSON names the code's account, the fake helper enrols a
// software P-256 key with a code the daemon's account minted, and seam reads
// the new key back from the Approvers JSON -- so the shapes seam decodes are
// the console's own.
func TestTouchIDEnroll_AgainstTheConsole(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the peer_auth socket is a Unix deployment")
	}
	ctx := context.Background()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	policy, err := access.New(config.PeerAuth{Owner: "owner", Locked: []string{"example-finance"}}, func(n string) (access.Account, error) {
		return access.Account{UID: 501, Name: n}, nil
	})
	require.NoError(t, err)
	svc := approval.New(approval.Config{DB: db, Events: events.NewRecorder(db)})
	cs, err := console.New(console.Config{DB: db, Events: events.NewRecorder(db), APIKey: "k", Level: "advanced", Approvals: svc,
		Terminal: &console.TerminalApprover{Account: "_seamless", Seamlessd: "/opt/seamless/bin/seamlessd", Config: "/opt/seamless/seamless.yaml"}})
	require.NoError(t, err)
	mux := http.NewServeMux()
	cs.Register(mux)
	// The owner's uid on the socket, as peerHandler names it.
	owner, ok := policy.PeerView(501)
	require.True(t, ok)
	daemon := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r.WithContext(access.WithView(r.Context(), owner)))
	})

	rig := newTouchRig(t, daemon, "\n")
	code, _, err := store.MintEnrolCode(ctx, db, store.ApproverTouchID, time.Now())
	require.NoError(t, err)
	var enrolled string
	rig.runner.helper = func(argv []string) {
		// Seamless Approve: a new key, signed over the code the owner typed,
		// posted over the socket it was given, the way the helper posts it.
		i := slices.Index(argv, "--socket")
		sock, name := argv[i+1], argv[slices.Index(argv, "--name")+1]
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		require.NoError(t, err)
		pub, err := key.PublicKey.Bytes()
		require.NoError(t, err)
		digest := sha256.Sum256(approval.TouchIDEnrolMessage(code, pub))
		sig, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
		require.NoError(t, err)
		client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}}}
		form := url.Values{"code": {code}, "name": {name}, "public_key": {base64.StdEncoding.EncodeToString(pub)},
			"signature": {base64.StdEncoding.EncodeToString(sig)}}
		req, err := http.NewRequest(http.MethodPost, "http://localhost/console/approvers/touchid/enrol", strings.NewReader(form.Encode()))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Connection", "close")
		resp, err := client.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		var got map[string]string
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
		require.Equal(t, http.StatusOK, resp.StatusCode, got["error"])
		enrolled = got["id"]
	}

	require.Equal(t, 0, rig.run("enroll", "--name", "Touch ID on laptop"), rig.errb.String())
	require.NotEmpty(t, enrolled)
	require.Contains(t, rig.out.String(), `Enrolled the Touch ID key "Touch ID on laptop" (`+enrolled+`)`)
	require.Contains(t, rig.out.String(), "sudo -k -u _seamless env SEAMLESS_CONFIG=/opt/seamless/seamless.yaml /opt/seamless/bin/seamlessd approvers enroll-code touchid")

	rig.out.Reset()
	require.Equal(t, 0, rig.run("status"), rig.errb.String())
	require.Contains(t, rig.out.String(), enrolled+`  "Touch ID on laptop"  added `)
}
