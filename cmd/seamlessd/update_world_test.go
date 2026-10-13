package main

// The updater's test world: a TLS release server answering the GitHub API and
// asset URLs for releases signed by a virtual Sigstore, a fake daemon
// answering /healthz on the config's address, a data dir with a real
// database, an installed "seamlessd" file a fake installer swaps, and a
// scripted service. Nothing in it touches the live machine: HOME, the data
// dir and every path are temp dirs, and no SEAMLESS_* variable of the test
// process reaches the config.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sigstore/sigstore-go/pkg/testing/ca"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/update"
)

// scrubSeamlessEnv removes every SEAMLESS_* variable from the test process
// for the test's duration (config.LoadFrom reads them) and points HOME at
// home.
func scrubSeamlessEnv(t *testing.T, home string) {
	t.Helper()
	t.Setenv("HOME", home)
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(strings.ToUpper(key), "SEAMLESS_") {
			t.Setenv(key, os.Getenv(key))
			require.NoError(t, os.Unsetenv(key))
		}
	}
}

// signedFor is the certificate identity of the release workflow on tag v.
func signedFor(v string) string {
	return "https://github.com/arctop/seamless/.github/workflows/release.yml@refs/tags/v" + v
}

// fakeDaemon answers /healthz like a seamlessd, as whichever release it was
// last (re)started as, and records itself as running in the update state the
// way the checker's boot does.
type fakeDaemon struct {
	t       *testing.T
	dataDir string
	srv     *httptest.Server

	mu       sync.Mutex
	version  string // "" = down: /healthz answers 503
	instance string
	pid      int
	starts   int
}

func (d *fakeDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	ver, inst := d.version, d.instance
	d.mu.Unlock()
	if r.URL.Path != "/healthz" {
		http.NotFound(w, r)
		return
	}
	if ver == "" {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "version": ver + "+abc1234", "instance": inst})
}

// start (re)starts the daemon as release ver with a fresh instance and
// records it in the update state, as a daemon's boot does.
func (d *fakeDaemon) start(ver string) {
	d.mu.Lock()
	d.starts++
	d.version = ver
	d.instance = "instance-" + strconv.Itoa(d.starts)
	d.pid = 5000 + d.starts
	inst, pid := d.instance, d.pid
	d.mu.Unlock()
	v, ok := update.Parse(ver)
	require.True(d.t, ok)
	require.NoError(d.t, update.SaveState(d.dataDir, update.State{Running: update.Running{
		Version: v, Distribution: update.DistributionRelease, Kind: update.KindInstaller,
		Instance: inst, PID: pid, StartedAt: time.Now().UTC(),
	}}))
}

// startUnrecorded serves ver with a fresh instance the update state does not
// name: another daemon answering the same port.
func (d *fakeDaemon) startUnrecorded(ver string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.starts++
	d.version, d.instance = ver, "stranger-"+strconv.Itoa(d.starts)
}

func (d *fakeDaemon) stop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.version = ""
}

func (d *fakeDaemon) serving() (string, string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.version, d.instance
}

// world is one updater test's machine and network.
type world struct {
	t             *testing.T
	goos, goarch  string
	home, dataDir string
	installDir    string
	exe           string // the installed seamlessd
	cfgPath       string
	cfg           config.Config
	daemon        *fakeDaemon
	from          string // the release installed at the start

	virtual  *ca.VirtualSigstore
	releases *httptest.Server

	mu         sync.Mutex
	entities   map[string]*ca.TestEntity    // bundle bytes -> what it signs
	assets     map[string]map[string][]byte // tag -> asset -> bytes
	statuses   map[string]int               // "tag/asset" or "tag" -> forced status
	tags       map[string]update.APIRelease // tag -> release object
	installs   []installerRun               // every installer run, in order
	actions    []serviceAction              // every service verb, in order
	installer  func(w *world, run installerRun) error
	environ    []string
	freeSpace  uint64
	selfErr    error
	autoErr    error
	probeEdits func(*update.Probe)
}

// newWorld stands up a world where release from is installed and serving.
func newWorld(t *testing.T, goos, goarch, from string) *world {
	t.Helper()
	root := t.TempDir()
	w := &world{
		t: t, goos: goos, goarch: goarch,
		home:       filepath.Join(root, "home"),
		dataDir:    filepath.Join(root, "data"),
		installDir: filepath.Join(root, "home", ".local", "bin"),
		from:       from,
		entities:   map[string]*ca.TestEntity{},
		assets:     map[string]map[string][]byte{},
		statuses:   map[string]int{},
		tags:       map[string]update.APIRelease{},
		freeSpace:  1 << 40,
	}
	scrubSeamlessEnv(t, w.home)
	for _, dir := range []string{w.home, w.dataDir, w.installDir} {
		require.NoError(t, os.MkdirAll(dir, 0o700))
	}
	w.exe = filepath.Join(w.installDir, "seamlessd")
	require.NoError(t, os.WriteFile(w.exe, []byte("seamlessd "+from+"\n"), 0o755))

	// The daemon's database, migrated by "the running release".
	db, err := updateTestDB(t, filepath.Join(w.dataDir, "seam.db"))
	require.NoError(t, err)
	require.NoError(t, db.Close())

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	w.daemon = &fakeDaemon{t: t, dataDir: w.dataDir}
	w.daemon.srv = &httptest.Server{Listener: ln, Config: &http.Server{Handler: w.daemon, ReadHeaderTimeout: 5 * time.Second}}
	w.daemon.srv.Start()
	t.Cleanup(w.daemon.srv.Close)
	w.daemon.start(from)

	w.cfgPath = filepath.Join(w.home, ".config", "seamless", "seamless.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(w.cfgPath), 0o700))
	require.NoError(t, os.WriteFile(w.cfgPath, fmt.Appendf(nil,
		"addr: %s\ndata_dir: %s\nmcp:\n  api_key: test-key\n", ln.Addr().String(), w.dataDir), 0o600))
	w.cfg, err = config.LoadFrom(w.cfgPath)
	require.NoError(t, err)

	w.virtual, err = ca.NewVirtualSigstore()
	require.NoError(t, err)
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/arctop/seamless/releases/tags/{tag}", w.serveTag)
	mux.HandleFunc("/releases/download/{tag}/{asset}", w.serveAsset)
	mux.HandleFunc("/repos/arctop/seamless/releases", w.serveList)
	w.releases = httptest.NewTLSServer(mux)
	t.Cleanup(w.releases.Close)

	// The inherited environment an unattended updater starts with: what a
	// service manager gives the daemon, plus SEAMLESS_* noise that must never
	// reach the installer.
	w.environ = []string{
		"PATH=/usr/bin:/bin", "HOME=" + w.home, "TMPDIR=" + root, "LANG=C",
		"SEAMLESS_CONFIG=" + w.cfgPath, "SEAMLESS_MCP_API_KEY=leak", "SEAMLESS_NO_SERVICE=1",
		"SEAMLESS_VERSION=9.9.9",
	}
	w.installer = installerSwaps
	return w
}

func (w *world) serveTag(rw http.ResponseWriter, r *http.Request) {
	tag := r.PathValue("tag")
	w.mu.Lock()
	status := w.statuses[tag]
	rel, ok := w.tags[tag]
	w.mu.Unlock()
	if status != 0 {
		rw.WriteHeader(status)
		return
	}
	if !ok {
		http.NotFound(rw, r)
		return
	}
	_ = json.NewEncoder(rw).Encode(rel)
}

func (w *world) serveList(rw http.ResponseWriter, _ *http.Request) {
	w.mu.Lock()
	var list []update.APIRelease
	for _, rel := range w.tags {
		list = append(list, rel)
	}
	w.mu.Unlock()
	_ = json.NewEncoder(rw).Encode(list)
}

func (w *world) serveAsset(rw http.ResponseWriter, r *http.Request) {
	tag, name := r.PathValue("tag"), r.PathValue("asset")
	w.mu.Lock()
	status := w.statuses[tag+"/"+name]
	body, ok := w.assets[tag][name]
	w.mu.Unlock()
	if status != 0 {
		rw.WriteHeader(status)
		return
	}
	if !ok {
		http.NotFound(rw, r)
		return
	}
	rw.Header().Set("Content-Length", strconv.Itoa(len(body)))
	if r.Method == http.MethodHead {
		rw.WriteHeader(http.StatusOK)
		return
	}
	_, _ = rw.Write(body)
}

// sign signs artifact as identity and returns the bundle bytes the release
// server serves for it.
func (w *world) sign(identity string, artifact []byte) []byte {
	w.t.Helper()
	entity, err := w.virtual.Sign(identity, signingIssuer, artifact)
	require.NoError(w.t, err)
	w.mu.Lock()
	defer w.mu.Unlock()
	bundle := fmt.Appendf(nil, "bundle-%d", len(w.entities))
	w.entities[string(bundle)] = entity
	return bundle
}

// verify is the updater's verifier over the virtual Sigstore: the production
// verifier configuration and exact-tag policy (verifyReleaseEntity), with the
// bundle bytes mapped to the entity they stand for.
func (w *world) verify(bundle, artifact []byte, v update.Version) error {
	w.mu.Lock()
	entity, ok := w.entities[string(bundle)]
	w.mu.Unlock()
	if !ok {
		return errors.New("parse sigstore bundle: unknown bundle")
	}
	return verifyReleaseEntity(w.virtual, entity, artifact, v)
}

// releaseOpts shapes a published release.
type releaseOpts struct {
	prerelease      bool
	noChecksumsSign bool   // predates checksums.txt.sigstore.json
	installerSigner string // identity signing the installer ("" = its own tag)
	checksumsSigner string // identity signing checksums.txt ("" = its own tag)
	checksums       string // checksums.txt content ("" = generated)
	script          string // the install script ("" = a comment naming the release)
}

// publish puts release v on the release server, every asset signed for its
// own tag unless opts say otherwise.
func (w *world) publish(v string, opts releaseOpts) {
	w.t.Helper()
	ver, ok := update.Parse(v)
	require.True(w.t, ok)
	inst := installerAsset(w.goos)
	script := []byte("#!/bin/sh\n# the v" + v + " installer\n")
	if opts.script != "" {
		script = []byte(opts.script)
	}
	archiveName := platformArchive(ver, w.goos, w.goarch)
	archiveBytes := []byte("archive of " + v)
	checksums := opts.checksums
	if checksums == "" {
		sum := sha256.Sum256(archiveBytes)
		checksums = hex.EncodeToString(sum[:]) + "  " + archiveName + "\n" +
			strings.Repeat("a", 64) + "  seamless_" + v + "_plan9_amd64.tar.gz\n"
	}
	instSigner, sumSigner := signedFor(v), signedFor(v)
	if opts.installerSigner != "" {
		instSigner = opts.installerSigner
	}
	if opts.checksumsSigner != "" {
		sumSigner = opts.checksumsSigner
	}
	files := map[string][]byte{
		inst:                    script,
		inst + bundleSuffix:     w.sign(instSigner, script),
		checksumsAsset:          []byte(checksums),
		archiveName:             archiveBytes,
		checksumsAsset + ".sig": []byte("detached"),
	}
	if !opts.noChecksumsSign {
		files[checksumsAsset+bundleSuffix] = w.sign(sumSigner, []byte(checksums))
	}
	published := time.Now().Add(-48 * time.Hour).UTC()
	rel := update.APIRelease{TagName: "v" + v, Prerelease: opts.prerelease, PublishedAt: &published}
	for name := range files {
		rel.Assets = append(rel.Assets, update.APIAsset{Name: name, State: "uploaded"})
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.assets["v"+v] = files
	w.tags["v"+v] = rel
}

// pin is the SHA-256 the updater must hand the installer for v's checksums.
func (w *world) pin(v string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	sum := sha256.Sum256(w.assets["v"+v][checksumsAsset])
	return hex.EncodeToString(sum[:])
}

// setStatus forces an HTTP status for a tag ("v1.2.3") or an asset
// ("v1.2.3/install").
func (w *world) setStatus(key string, status int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.statuses[key] = status
}

// removeAsset unpublishes one asset of a release, from the downloads and from
// the release object.
func (w *world) removeAsset(v, name string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.assets["v"+v], name)
	rel := w.tags["v"+v]
	var kept []update.APIAsset
	for _, a := range rel.Assets {
		if a.Name != name {
			kept = append(kept, a)
		}
	}
	rel.Assets = kept
	w.tags["v"+v] = rel
}

// setAssetState sets the upload state the release object lists for one
// asset ("open" while GitHub is still receiving it).
func (w *world) setAssetState(v, name, state string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	rel := w.tags["v"+v]
	for i := range rel.Assets {
		if rel.Assets[i].Name == name {
			rel.Assets[i].State = state
		}
	}
	w.tags["v"+v] = rel
}

// probe is the install probe of an installer-managed install of w.from.
func (w *world) probe(configPath string, client bool) update.Probe {
	p := update.Probe{
		Distribution: update.DistributionRelease, Version: w.from, Client: client,
		Exe: w.exe, ExeDirWritable: true, ConfigIsInstallers: true, ConfigPath: configPath,
		Service: update.ServiceProbe{
			Path:  filepath.Join(w.home, "Library", "LaunchAgents", launchdLabel+".plist"),
			Found: true, Marker: true, Program: w.exe, RunsThisExe: true,
		},
		SupervisedReason: "not the launchd job",
	}
	if w.probeEdits != nil {
		w.probeEdits(&p)
	}
	return p
}

// deps wires the updater to this world. stdout is the attended terminal,
// nil for an unattended run.
func (w *world) deps(stdout *strings.Builder) updaterDeps {
	client := w.releases.Client()
	client.CheckRedirect = update.HTTPSOnlyRedirect
	client.Timeout = 10 * time.Second
	d := updaterDeps{
		goos: w.goos, goarch: w.goarch,
		version: w.from, distribution: update.DistributionRelease, buildVersion: w.from + "+abc1234",
		environ: w.environ, home: w.home,
		now: time.Now,
		ticker: func(d time.Duration) (<-chan time.Time, func()) {
			t := time.NewTicker(d)
			return t.C, t.Stop
		},
		releases: releaseSource{client: client, apiRepo: w.releases.URL + "/repos/arctop/seamless", downloadBase: w.releases.URL + "/releases/download"},
		listReleases: func(ctx context.Context) ([]update.APIRelease, error) {
			f := &update.Fetcher{Client: client, URL: w.releases.URL + "/repos/arctop/seamless/releases", Now: time.Now}
			page, err := f.Fetch(ctx, "")
			return page.Releases, err
		},
		verify:      w.verify,
		probe:       w.probe,
		selfCheck:   func() error { return w.selfErr },
		autoAllowed: func(context.Context, config.Config) error { return w.autoErr },
		service: func(a serviceAction) (bool, string) {
			w.mu.Lock()
			w.actions = append(w.actions, a)
			w.mu.Unlock()
			if a == actionStop {
				w.daemon.stop()
			}
			return true, ""
		},
		runInstaller: func(run installerRun) error {
			w.mu.Lock()
			w.installs = append(w.installs, run)
			inst := w.installer
			w.mu.Unlock()
			return inst(w, run)
		},
		freeBytes: func(string) (uint64, error) { return w.freeSpace, nil },
		sameFile:  sameFile,
		timing: updaterTiming{
			lockWait: 300 * time.Millisecond, lockPoll: 10 * time.Millisecond,
			confirm: confirmTiming{
				poll: 2 * time.Millisecond, request: 5 * time.Second,
				stable: 15 * time.Millisecond, deadline: 400 * time.Millisecond, starting: 400 * time.Millisecond,
			},
			installer: 30 * time.Second, daemonStop: 200 * time.Millisecond,
			finalTries: 3, finalRetry: time.Millisecond,
		},
	}
	if stdout != nil {
		d.stdout = stdout
	}
	return d
}

// envOf is a KEY=VALUE environment as a map; a name given twice fails.
func envOf(t *testing.T, env []string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		require.True(t, ok, "malformed env entry %q", kv)
		_, dup := out[k]
		require.False(t, dup, "%s is set twice", k)
		out[k] = v
	}
	return out
}

// installerVersion is the SEAMLESS_VERSION a run was given.
func installerVersion(t *testing.T, run installerRun) string {
	t.Helper()
	return envOf(t, run.env)["SEAMLESS_VERSION"]
}

// installerSwaps is a well-behaved installer: it replaces the installed
// seamlessd and restarts the daemon as the release it was pinned to.
func installerSwaps(w *world, run installerRun) error {
	v := installerVersion(w.t, run)
	if err := os.WriteFile(w.exe, []byte("seamlessd "+v+"\n"), 0o755); err != nil {
		return err
	}
	w.daemon.start(v)
	return nil
}

// runAuto runs the unattended updater for from -> to in this world and
// returns the error and the attempt it recorded.
func (w *world) runAuto(to string) (update.Attempt, error) {
	w.t.Helper()
	id := testAttemptID(w.t)
	f, err := parseUpdateFlags(updaterArgs(update.SpawnRequest{
		AttemptID: id, From: mustVersion(w.t, w.from), To: mustVersion(w.t, to), Why: update.WhyAuto,
	}, w.cfgPath)[1:])
	require.NoError(w.t, err)
	err = runAutoUpdate(f, w.deps(nil))
	a, rerr := update.ReadAttempt(w.dataDir)
	require.NoError(w.t, rerr)
	return a, err
}

func mustVersion(t *testing.T, s string) update.Version {
	t.Helper()
	v, ok := update.Parse(s)
	require.True(t, ok, "version %q", s)
	return v
}

func testAttemptID(t *testing.T) string {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	return id
}
