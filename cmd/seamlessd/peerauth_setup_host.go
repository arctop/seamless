package main

// What seamlessd peer-auth setup reads on the machine (pasWorld), the
// preflight that turns it into a plan, and this machine's pasHost.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/hooks"
	"github.com/arctop/seamless/internal/releasesig"
	"github.com/arctop/seamless/internal/sshsig"
	"github.com/arctop/seamless/internal/update"
)

// pasWorld is what the preflight and the steps read: the owner's config, the
// files and directories on the path, the accounts, the pin, and launchd.
type pasWorld struct {
	// config is the owner's config as seamlessd loads it, and its file.
	config func() (config.Config, []byte, error)
	// inspect reports a path's owner and mode, not following a final link.
	inspect func(string) (fileFacts, error)
	// lookupUser and lookupGroup find an account and a group by name.
	lookupUser  func(name string) (uid, gid int, ok bool)
	lookupGroup func(name string) (gid int, ok bool)
	// takenIDs lists the macOS ids in use, as user ids or as group ids.
	takenIDs func() (map[int]bool, error)
	// systemAccount judges a uid as a system account (access.SystemAccount).
	systemAccount func(uid uint32) (bool, string)
	pin           func() (access.DaemonPin, bool, error)
	// launchdLoaded reports whether a launchd target (gui/<uid>/<label>) is
	// loaded.
	launchdLoaded func(target string) bool
}

// pasPreflight reads the install and the machine, refuses a move it cannot
// make, and returns the plan and what the owner should know first.
func pasPreflight(ctx context.Context, h *pasHost, o pasOptions) (pasPlan, []string, error) {
	refuse := func(format string, a ...any) (pasPlan, []string, error) {
		return pasPlan{}, nil, fmt.Errorf("%w: %s", errPasRefused, fmt.Sprintf(format, a...))
	}
	w := h.world
	cfg, raw, err := w.config()
	if err != nil {
		return refuse("your config does not load: %v", err)
	}
	path := cfg.SourcePath()
	switch {
	case path == "":
		return refuse("no config file: setup moves an installed daemon, whose config it starts from")
	case cfg.IsClient() && cfg.SocketPath() == pasSocket && pasExists(w, pasConfig):
		return refuse("peer_auth is on here already: the daemon runs as its own account (its config is %s), and %s is its client's", pasConfig, path)
	case cfg.IsClient():
		return refuse("%s is a client's config: run setup on the computer that runs the daemon", path)
	case cfg.PeerAuth.Enabled:
		return refuse("peer_auth is on in %s already", path)
	}
	if _, _, err := pasFileKey(raw); err != nil {
		return refuse("%v", err)
	}
	p := pasPlan{V: pasPlanVersion, GOOS: h.goos, Owner: h.owner.name, OwnerUID: h.owner.uid, OwnerGroup: h.owner.group,
		Home: h.owner.home, Account: pasAccount, OldConfig: path, OldDataDir: filepath.Clean(cfg.DataDir),
		ServerURL: cfg.ServerURL(), RootOwnedBinaries: o.rootOwned}
	if h.hookSeam != nil {
		// What undo puts the hooks back on. One that already runs a seam under
		// the layout (a move made by hand) is no per-user seam to go back to.
		if s := h.hookSeam(); pasPath(s) && !strings.HasPrefix(s, pasRoot+"/") {
			p.OldSeam = s
		}
	}
	// Which clients the hooks step re-points: the ones wired now, never the
	// ones merely installed here.
	var clientNotes []string
	if h.wiredClients != nil {
		seamDir := filepath.Dir(h.exe)
		if p.OldSeam != "" {
			seamDir = filepath.Dir(p.OldSeam)
		}
		p.Clients, p.SkipSkills, clientNotes = h.wiredClients(cfg, absConfigPath(path), seamDir)
	}

	// The data directory: the owner's, a real directory, not under the layout.
	if f, err := w.inspect(p.OldDataDir); err != nil || !f.isDir || f.symlink {
		return refuse("the data directory %s is not there as a directory", p.OldDataDir)
	} else if int(f.uid) != h.owner.uid {
		return refuse("the data directory %s is not yours (uid %d)", p.OldDataDir, f.uid)
	}
	if cfg.TLSEnabled() {
		p.TLSCert, p.TLSKey = filepath.Clean(cfg.TLS.CertFile), filepath.Clean(cfg.TLS.KeyFile)
		for _, f := range []string{p.TLSCert, p.TLSKey} {
			if _, err := h.readFile(f); err != nil {
				return refuse("the TLS file %s cannot be read: %v", f, err)
			}
		}
	}

	// The account: there and a system account, or free to make.
	if uid, _, ok := w.lookupUser(pasAccount); ok {
		if uid == h.owner.uid {
			return refuse("%s is your own account", pasAccount)
		}
		if sys, why := w.systemAccount(uint32(uid)); !sys {
			return refuse("the account %s is there and is not a system account (%s)", pasAccount, why)
		}
		if _, ok := w.lookupGroup(pasAccount); !ok {
			return refuse("the account %s is there, but no group of that name", pasAccount)
		}
	} else {
		if _, ok := w.lookupGroup(pasAccount); ok {
			return refuse("a group %s is there without its account: remove it, or make the account by hand", pasAccount)
		}
		if h.goos == "darwin" {
			taken, err := w.takenIDs()
			if err != nil {
				return refuse("cannot list the ids in use (dscl): %v", err)
			}
			if p.UID = pasIDsFree(taken); p.UID == 0 {
				return refuse("no id below 500 is free as both a user id and a group id")
			}
		}
	}
	if pin, pinned, err := w.pin(); err != nil {
		return refuse("%v", err)
	} else if pinned && pin.Name != pasAccount {
		return refuse("%s pins %s as the daemon's account", access.DaemonAccountPath, pin.Name)
	}

	// The layout: root's where it is there, and nothing in the way.
	if f, err := w.inspect(pasRoot); err == nil {
		if !f.isDir || f.symlink || f.uid != 0 || f.mode.Perm()&0o022 != 0 {
			return refuse("%s is there and is not a directory of root's that only root can write", pasRoot)
		}
	}
	if _, err := w.inspect(relayDataDir()); err == nil {
		return refuse("this computer runs the relay (%s): a computer whose agents reach a server through a relay runs no daemon of its own", relayDataDir())
	}
	if _, err := w.inspect(pasData); err == nil {
		return refuse("%s is there already: a daemon's data, which setup would not move over", pasData)
	}
	for _, f := range pasSystemServiceFiles(h.goos) {
		if _, err := w.inspect(f); err == nil {
			return refuse("%s is there already: a system service for the daemon is installed", f)
		}
	}

	// The per-user services this move retires.
	for _, svc := range pasServiceNames(h.goos) {
		var def string
		if h.goos == "darwin" {
			def = launchdPlist(p.Home, svc)
		} else {
			def = filepath.Join(p.Home, ".config", "systemd", "user", svc)
		}
		_, err := w.inspect(def)
		if err == nil || (h.goos == "darwin" && w.launchdLoaded("gui/"+strconv.Itoa(p.OwnerUID)+"/"+svc)) {
			p.Services = append(p.Services, svc)
		}
	}

	// The binaries.
	switch {
	case o.sourceBuild:
		p.Source = pasExeDir(h.exe)
		for _, name := range systemBinaries {
			if f, err := w.inspect(filepath.Join(p.Source, name)); err != nil || f.isDir {
				return refuse("--source-build copies the seamlessd and seam beside this one, and %s is not there", filepath.Join(p.Source, name))
			}
		}
	case o.release != "":
		v, ok := update.Parse(o.release)
		if !ok {
			return refuse("--release %q is not a version (X.Y.Z)", o.release)
		}
		p.Release = v.String()
	default:
		v, ok := update.Parse(h.version)
		if h.distribution != update.DistributionRelease || !ok {
			return refuse("this seamlessd is a source build (%s): pass --release X.Y.Z to install a release's checked binaries, "+
				"or --source-build to install this build's (no signature checks them, and the daemon never updates itself)", h.version)
		}
		p.Release = v.String()
	}
	if h.goos == "darwin" && p.Release != "" {
		if team := h.team(ctx, h.exe); pasTeam.MatchString(team) {
			p.Team = team
		}
	}
	// The owner's release signers pin: the release is held to it before it is
	// installed, and the daemon keeps a copy of it, root's.
	var signersRow string
	if path := cfg.Update.ReleaseSigners; path != "" {
		if p.Source != "" {
			return refuse("update.release_signers (%s) installs only a release the owner signed, and --source-build copies binaries nobody signed: "+
				"install a release (--release X.Y.Z), or remove the key from %s", path, cfg.SourcePath())
		}
		data, err := h.readSigners(path)
		if err != nil {
			return refuse("%v", err)
		}
		signers, err := releasesig.ParseOwnerSigners(data)
		if err != nil {
			return refuse("update.release_signers %s: %v", path, err)
		}
		sum := sha256.Sum256(data)
		p.ReleaseSigners, p.SignersSHA256 = filepath.Clean(path), hex.EncodeToString(sum[:])
		signersRow = "signers: " + pasReleaseSigners + ", root's copy of " + pasHomeWords(p, p.ReleaseSigners) +
			": v" + p.Release + " installs only once its " + update.OwnerSignatureAsset + " verifies against " +
			strings.Join(releasesig.OwnerKeys(signers), "; ") + ", and so does every update after it"
	}
	if why := p.problem(); why != "" {
		return refuse("%s", why)
	}

	// What the owner should know first.
	var warnings []string
	if signersRow != "" {
		warnings = append(warnings, signersRow)
	}
	for _, kv := range h.environ {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "SEAMLESS_") && name != "SEAMLESS_CONFIG" {
			warnings = append(warnings, name+" is set in this shell: the daemon reads only its config file, so put that setting in "+path+" first if it matters")
		}
	}
	for _, k := range []struct{ key, value string }{
		{"tls.ca_file", cfg.TLS.CAFile}, {"backup.dir", cfg.Backup.Dir},
		{"commits.snapshot_file", cfg.Commits.SnapshotFile}, {"screening.lockdown_file", cfg.Screening.LockdownFile},
		{"screening.feed_socket", cfg.Screening.FeedSocket}, {"screening.feed_spool", cfg.Screening.FeedSpool},
	} {
		if strings.HasPrefix(k.value, p.Home+"/") {
			warnings = append(warnings, k.key+" names "+k.value+", in your home, which the daemon's account cannot reach: "+
				"after the move, point it at a path under "+pasRoot+" in "+pasConfig)
		}
	}
	if len(p.Services) == 0 {
		warnings = append(warnings, "no per-user service was found: stop a daemon you run by hand before the data moves")
	}
	if len(p.Clients) == 0 {
		warnings = append(warnings, "no agent client is wired to Seamless here, so the move re-points none: "+
			"run seamlessd install-hooks after it to wire one")
	}
	warnings = append(warnings, clientNotes...)
	if !pasSelfUpdating(p) && p.Source == "" {
		warnings = append(warnings, "the binaries stay root's: the daemon only tells you about a new release, and you replace them with sudo")
	}
	// The daemon's config must load before anything moves.
	daemon, err := pasDaemonConfig(raw, p, h.now())
	if err != nil {
		return refuse("%v", err)
	}
	dcfg, err := pasCheckDaemonConfig(h, daemon)
	if err != nil {
		return refuse("the daemon's config would not load: %v", err)
	}
	// A passkey is bound to its relying party for life: the owner sees which
	// one the approvers will be enrolled under before anything is made.
	rpID, origins := dcfg.RelyingParty()
	warnings = append([]string{"passkeys: approvers enroll under rp_id " + rpID + ", on " + strings.Join(origins, ", ") +
		" (a passkey is bound to it for life)"}, warnings...)
	return p, warnings, nil
}

// pasExists reports whether path is there.
func pasExists(w pasWorld, path string) bool {
	_, err := w.inspect(path)
	return err == nil
}

// pasCheckDaemonConfig loads the daemon's config from a scratch copy.
func pasCheckDaemonConfig(h *pasHost, daemon []byte) (config.Config, error) {
	dir, err := h.tempDir()
	if err != nil {
		return config.Config{}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "seamless.yaml")
	if err := os.WriteFile(path, daemon, 0o600); err != nil {
		return config.Config{}, err
	}
	return config.LoadFrom(path)
}

// relayDataDir is the relay's state directory on a Client.
func relayDataDir() string { return pasRoot + "/relay" }

// pasSystemServiceFiles are the system service definitions a daemon under
// peer_auth may already have.
func pasSystemServiceFiles(goos string) []string {
	if goos == "darwin" {
		return []string{pasDaemonPlistPath(), systemDaemonPlistDir + "/" + legacySystemDaemonLabel + ".plist"}
	}
	var out []string
	for _, dir := range systemUnitDirs {
		out = append(out, filepath.Join(dir, systemDaemonUnit))
	}
	return out
}

// pasHookSeam is the seam the owner's Claude Code hooks run
// (~/.claude/settings.json), "" when there are none.
// pasWiredClients is pasHost.wiredClients on this machine: the agent clients
// an update would observe as wired (observeClients) in the owner's home and
// environment, and the skills they lack (skillOptOuts).
func pasWiredClients(home string, environ []string) func(config.Config, string, string) ([]string, []string, []string) {
	return func(cfg config.Config, configPath, seamDir string) (clients, skip, notes []string) {
		env := shapeEnv{goos: runtime.GOOS, home: home, environ: environ, claudeManagedDir: claudeManagedSettingsDir()}
		clients, notes = observeClients(env, cfg, seamDir, configPath)
		skip, more := skillOptOuts(env, clients)
		return clients, skip, append(notes, more...)
	}
}

func pasHookSeam() string {
	path, err := expandHome("~/.claude/settings.json")
	if err != nil {
		return ""
	}
	bin, _, ok := hooks.RecordedCommandPaths(hooks.ClientClaudeCode, path)
	if !ok {
		return ""
	}
	if expanded, err := expandHome(bin); err == nil {
		bin = expanded
	}
	return bin
}

// realPasHost is this machine.
func realPasHost() *pasHost {
	h := &pasHost{
		goos: runtime.GOOS, goarch: runtime.GOARCH, euid: os.Geteuid(),
		environ: os.Environ(), interactive: stdinIsTerminal(), in: bufio.NewReader(os.Stdin), out: os.Stdout,
		now: time.Now, version: version, distribution: distribution, wait: time.Second,
		readFile: os.ReadFile, reachable: serverReachable, tempDir: pasTempDir,
		hookSeam: pasHookSeam, updaterBusy: updaterLockHeld,
	}
	if u, err := user.Current(); err == nil {
		h.owner = pasHostOwner{name: u.Username, home: u.HomeDir}
		if uid, err := strconv.Atoi(u.Uid); err == nil {
			h.owner.uid = uid // else 0, which the plan refuses as the owner's uid
		}
		if g, err := user.LookupGroupId(u.Gid); err == nil {
			h.owner.group = g.Name
		}
	}
	h.wiredClients = pasWiredClients(h.owner.home, h.environ)
	if exe, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			exe = real
		}
		h.exe = exe
	}
	h.world = pasWorld{
		config: func() (config.Config, []byte, error) {
			cfg, err := config.Load()
			if err != nil || cfg.SourcePath() == "" {
				return cfg, nil, err
			}
			raw, err := os.ReadFile(cfg.SourcePath())
			return cfg, raw, err
		},
		inspect: inspectPath,
		lookupUser: func(name string) (int, int, bool) {
			u, err := user.Lookup(name)
			if err != nil {
				return 0, 0, false
			}
			uid, uerr := strconv.Atoi(u.Uid)
			gid, gerr := strconv.Atoi(u.Gid)
			return uid, gid, uerr == nil && gerr == nil
		},
		lookupGroup: func(name string) (int, bool) {
			g, err := user.LookupGroup(name)
			if err != nil {
				return 0, false
			}
			gid, err := strconv.Atoi(g.Gid)
			return gid, err == nil
		},
		takenIDs:      pasDsclTakenIDs,
		systemAccount: access.SystemAccount,
		pin:           access.DaemonAccount,
		launchdLoaded: func(target string) bool { return exec.Command("/bin/launchctl", "print", target).Run() == nil },
	}
	h.shell = func(ctx context.Context, script string) error {
		// Every block is shown before it runs, but the state file's own line.
		if !strings.HasSuffix(script, " -a "+pasStatePath+" >/dev/null") || strings.Contains(script, "\n") {
			fmt.Fprintf(h.out, "%s\n", pasIndent(script))
		}
		cmd := exec.CommandContext(ctx, "/bin/sh", "-c", script)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		return cmd.Run()
	}
	h.runOwner = func(ctx context.Context, argv []string, env []string) error {
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Env = append(os.Environ(), env...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		return cmd.Run()
	}
	h.fetch = pasFetchRelease
	h.readSigners = pasReadSigners
	h.team = pasCodesignTeam
	h.health = pasSocketHealth
	return h
}

// pasIndent shows a block of commands as it runs, each line indented.
func pasIndent(script string) string {
	return "    " + strings.ReplaceAll(script, "\n", "\n    ")
}

// pasTempDir is a directory of this run's own, 0700.
func pasTempDir() (string, error) {
	dir, err := os.MkdirTemp("", "seamless-peer-auth-")
	if err != nil {
		return "", err
	}
	return dir, os.Chmod(dir, 0o700)
}

// pasDsclTakenIDs reads every user id and group id in use on macOS.
func pasDsclTakenIDs() (map[int]bool, error) {
	taken := map[int]bool{}
	for _, list := range [][]string{{"/Users", "UniqueID"}, {"/Groups", "PrimaryGroupID"}} {
		out, err := exec.Command("/usr/bin/dscl", ".", "-list", list[0], list[1]).Output()
		if err != nil {
			return nil, err
		}
		for line := range bytes.SplitSeq(out, []byte("\n")) {
			f := strings.Fields(string(line))
			if len(f) != 2 {
				continue
			}
			if id, err := strconv.Atoi(f[1]); err == nil {
				taken[id] = true
			}
		}
	}
	return taken, nil
}

// pasCodesignTeam is the Developer ID team codesign reads off path, "" when
// it names none (ad hoc, a source build) or codesign cannot read it.
func pasCodesignTeam(ctx context.Context, path string) string {
	out, err := exec.CommandContext(ctx, "/usr/bin/codesign", "-dvv", path).CombinedOutput()
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		if team, ok := strings.CutPrefix(strings.TrimSpace(line), "TeamIdentifier="); ok && pasTeam.MatchString(team) {
			return team
		}
	}
	return ""
}

// pasFetchRelease downloads release v's checksums.txt, its Sigstore bundle
// and this machine's archive, checks them as a system install's update does
// (verifySystemDownload: the exact tag's identity, the archive's checksum,
// its layout), and returns the binaries in it with the checksums. With
// ownerSig it also downloads the owner's checksums.txt.sshsig, as the
// updater does under update.release_signers (fetchTarget): none listed
// leaves it nil, which the caller refuses (ownerSignatureMissing); one still
// uploading is errReleaseInFlight.
func pasFetchRelease(ctx context.Context, v update.Version, ownerSig bool) (pasRelease, error) {
	src := newReleaseSource()
	rel, err := src.releaseByTag(ctx, v)
	if err != nil {
		return pasRelease{}, err
	}
	if err := checkPublished(rel, v, systemAssetNames(v, runtime.GOOS, runtime.GOARCH)); err != nil {
		return pasRelease{}, err
	}
	dl := systemDownload{version: v, archiveName: platformArchive(v, runtime.GOOS, runtime.GOARCH)}
	if dl.checksums, err = src.fetchAsset(ctx, v, checksumsAsset); err != nil {
		return pasRelease{}, err
	}
	if dl.checksumsBundle, err = src.fetchAsset(ctx, v, checksumsAsset+bundleSuffix); err != nil {
		return pasRelease{}, err
	}
	if dl.archive, err = src.fetchArchive(ctx, v, dl.archiveName); err != nil {
		return pasRelease{}, err
	}
	if err := verifySystemDownload(dl, releasesig.Embedded()); err != nil {
		return pasRelease{}, err
	}
	out := pasRelease{bins: map[string][]byte{}, checksums: dl.checksums}
	if ownerSig {
		switch listed, uploaded := listedAsset(rel, update.OwnerSignatureAsset); {
		case listed && !uploaded:
			return pasRelease{}, fmt.Errorf("%w: v%s is still uploading %s", errReleaseInFlight, v, update.OwnerSignatureAsset)
		case listed:
			if out.ownerSig, err = src.fetchAsset(ctx, v, update.OwnerSignatureAsset); err != nil {
				return pasRelease{}, err
			}
		}
	}
	err = walkArchive(dl.archive, func(name string, r io.Reader) error {
		data, err := io.ReadAll(io.LimitReader(r, maxBinaryBytes+1))
		if err != nil {
			return err
		}
		out.bins[name] = data
		return nil
	})
	return out, err
}

// pasReadSigners reads the owner's update.release_signers once and holds what
// it read to the updater's own rules for a pin on a per-user install
// (loadReleaseSigners: an absolute path, root's or this account's file under
// directories no other account can write, a key for the release namespace),
// so setup pins the bytes it checked.
func pasReadSigners(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", errReleaseSigners, path, err)
	}
	data, err := io.ReadAll(io.LimitReader(f, sshsig.MaxSignersBytes+1))
	_ = f.Close()
	if err != nil {
		return nil, fmt.Errorf("%w: read %s: %w", errReleaseSigners, path, err)
	}
	fsys := hostSignersFS()
	fsys.open = func(string) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }
	if _, err := loadReleaseSigners(path, false, fsys); err != nil {
		return nil, err
	}
	return data, nil
}

// pasSocketHealth asks the daemon on the socket for /healthz.
func pasSocketHealth(ctx context.Context) error {
	cfg := config.Config{Role: config.RoleClient, AdvertisedURL: "unix://" + pasSocket}
	client, err := cfg.HTTPClient(5 * time.Second)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, config.SocketBaseURL+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("/healthz answers %s", resp.Status)
	}
	return nil
}
