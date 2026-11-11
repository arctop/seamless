package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/update"
)

// `seamlessd update` applies a release by running that release's own
// installer script -- docs/install (POSIX) or docs/install.ps1 (PowerShell),
// published as release assets beside the Sigstore bundles the release
// workflow signs them with -- after verifying the script, and the release's
// checksums.txt, against bundles signed for that release's exact tag. The
// download, the archive's checksum and the binary swap all stay inside the
// script, so there is ONE upgrade implementation, the one a fresh install
// runs; `seamlessd update` adds what an installer run cannot do for itself:
// it names the release (never GitHub's "latest"), pins the installer to the
// checksums.txt it verified (SEAMLESS_CHECKSUMS_SHA256), backs the instance
// up, confirms the new release serving by observation, and rolls back when it
// does not (update_engine.go).
//
// Two ways in:
//
//   - `seamlessd update` is the owner, attended (runAttendedUpdate).
//   - `seamlessd update --auto ...` is the daemon's unattended updater, a
//     process the daemon starts with updaterArgs (runAutoUpdate).

// updateFlags is a parsed `seamlessd update` command line.
type updateFlags struct {
	check, dryRun, auto bool
	url                 string
	// attempt, from, to, why and config are the updater's arguments
	// (updaterArgs); config is also accepted attended.
	attempt, from, to, why, config string
	// set names the flags the command line gave.
	set map[string]bool
}

// autoOnlyFlags belong to the daemon's updater and mean nothing attended.
var autoOnlyFlags = []string{"attempt", "from", "to", "why"}

// notWithAutoFlags are the attended flags --auto refuses: the updater only
// ever installs what the daemon named, verified, and always for real.
var notWithAutoFlags = []string{"url", "check", "dry-run"}

// parseUpdateFlags parses `seamlessd update`'s arguments and refuses the
// combinations that cannot mean anything.
func parseUpdateFlags(args []string) (updateFlags, error) {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	var f updateFlags
	fs.BoolVar(&f.check, "check", false, "report installed vs the newest release, and what the update check is doing, then exit without changing anything")
	fs.BoolVar(&f.dryRun, "dry-run", false, "print what would run and exit without downloading, installing or recording anything")
	fs.StringVar(&f.url, "url", "", "run the installer at this https `URL` instead of a release's own (no signature to verify, no backup or rollback)")
	fs.StringVar(&f.config, "config", "", "path to seamless.yaml (overrides $SEAMLESS_CONFIG and the search path)")
	fs.BoolVar(&f.auto, "auto", false, "run as the daemon's unattended updater; the daemon starts it, not a person")
	fs.StringVar(&f.attempt, "attempt", "", "with --auto: the attempt `ID` the daemon minted")
	fs.StringVar(&f.from, "from", "", "with --auto: the `version` the daemon runs")
	fs.StringVar(&f.to, "to", "", "with --auto: the `version` to install")
	fs.StringVar(&f.why, "why", "", "with --auto: why the attempt runs (a word the attempt contract accepts)")
	if err := fs.Parse(args); err != nil {
		return updateFlags{}, err
	}
	if fs.NArg() > 0 {
		return updateFlags{}, fmt.Errorf("seamlessd.update: unexpected argument %q", fs.Arg(0))
	}
	f.set = map[string]bool{}
	fs.Visit(func(fl *flag.Flag) { f.set[fl.Name] = true })
	for _, name := range notWithAutoFlags {
		if f.auto && f.set[name] {
			return updateFlags{}, fmt.Errorf("seamlessd.update: --%s cannot be combined with --auto", name)
		}
	}
	for _, name := range autoOnlyFlags {
		if !f.auto && f.set[name] {
			return updateFlags{}, fmt.Errorf("seamlessd.update: --%s is an argument of the daemon's updater and needs --auto", name)
		}
	}
	return f, nil
}

// updaterArgs is the command line the daemon starts its unattended updater
// with, after the executable:
//
//	update --auto --attempt <ULID> --from <X.Y.Z> --to <X.Y.Z> --why <word> --config <absolute path>
//
// It is a contract between two releases, frozen in this shape: the daemon of
// one release builds it, and the seamlessd on disk -- which can be a newer
// release than the daemon that started it -- parses it (updateFlags.
// spawnRequest). A release may add flags; it never renames, drops or
// reorders these, and the parser of every release accepts this exact form.
func updaterArgs(req update.SpawnRequest, configPath string) []string {
	return []string{
		"update", "--auto",
		"--attempt", req.AttemptID,
		"--from", req.From.String(),
		"--to", req.To.String(),
		"--why", req.Why,
		"--config", configPath,
	}
}

// spawnRequest reads an updater command line (updaterArgs) back into the
// attempt it names and the config to load, and validates it before anything
// acts on it: the versions are exact X.Y.Z, the request passes
// SpawnRequest.Validate (a ULID, a why this release knows), the why is one
// the daemon spawns -- anything but WhyManual, the owner's attended run,
// which the daemon never starts -- the release moves forward (an unattended
// update never downgrades, whatever its why), and the config path is
// absolute.
func (f updateFlags) spawnRequest() (update.SpawnRequest, string, error) {
	from, err := exactVersion("--from", f.from)
	if err != nil {
		return update.SpawnRequest{}, "", err
	}
	to, err := exactVersion("--to", f.to)
	if err != nil {
		return update.SpawnRequest{}, "", err
	}
	req := update.SpawnRequest{AttemptID: f.attempt, From: from, To: to, Why: f.why}
	if err := req.Validate(); err != nil {
		return update.SpawnRequest{}, "", err
	}
	if req.Why == update.WhyManual {
		return update.SpawnRequest{}, "", fmt.Errorf("%w: --auto runs the attempts the daemon starts; %q is the owner's attended `seamlessd update`",
			update.ErrInvalidAttempt, req.Why)
	}
	if req.To.Compare(req.From) <= 0 {
		return update.SpawnRequest{}, "", fmt.Errorf("%w: an unattended update only moves forward, not from %s to %s",
			update.ErrInvalidAttempt, req.From, req.To)
	}
	if f.config == "" || !filepath.IsAbs(f.config) {
		return update.SpawnRequest{}, "", fmt.Errorf("--config %q: the updater needs the daemon's config as an absolute path", f.config)
	}
	return req, filepath.Clean(f.config), nil
}

// exactVersion parses a version flag that must be a release number written
// exactly as Version.String writes it.
func exactVersion(flagName, raw string) (update.Version, error) {
	v, ok := update.Parse(raw)
	if !ok || v.String() != raw {
		return update.Version{}, fmt.Errorf("%s %q: want a release version X.Y.Z", flagName, raw)
	}
	return v, nil
}

// runUpdate is `seamlessd update`.
func runUpdate(args []string) error {
	f, err := parseUpdateFlags(args)
	if err != nil {
		return err
	}
	switch {
	case f.auto:
		return runAutoUpdate(f, realUpdaterDeps(nil))
	case f.check:
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		return reportUpdateCheck(ctx, os.Stdout, update.NewFetcher(), runtime.GOOS, version, buildVersion(), cliUpdateView(ctx, f.config))
	default:
		return runAttendedUpdate(context.Background(), f, realUpdaterDeps(os.Stdout))
	}
}

// runAutoUpdate is the daemon's unattended updater: `seamlessd update --auto`
// with the arguments of updaterArgs. Before anything is recorded it refuses a
// command line that does not validate, a build that is not a release (a
// source build never writes update state: constraint
// dev-and-fixture-daemons-never-self-update), a config that does not load,
// and a client-role config (no daemon, no data dir to record in); from then
// on every outcome is recorded (runRecorded). It exits non-zero unless the
// release was installed and confirmed.
func runAutoUpdate(f updateFlags, d updaterDeps) error {
	req, configPath, err := f.spawnRequest()
	if err != nil {
		return fmt.Errorf("seamlessd.update: %w", err)
	}
	if !update.IsReleaseBuild(d.distribution, d.version) {
		return fmt.Errorf("seamlessd.update: --auto runs only in a release build, and this one (%s) is not", d.buildVersion)
	}
	cfg, err := config.LoadFrom(configPath)
	if err != nil {
		return fmt.Errorf("seamlessd.update: %w", err)
	}
	if cfg.IsClient() {
		return errors.New("seamlessd.update: role: client runs no daemon here, so nothing updates it unattended")
	}
	job := updateJob{req: req, cfg: cfg, configPath: configPath, probe: updaterProbe(d.probe(installConfigOf(cfg, configPath)), false)}
	final, err := runRecorded(context.Background(), d, job)
	if final.ID == "" {
		return fmt.Errorf("seamlessd.update: %w", err)
	}
	if !final.OK {
		return errors.Join(fmt.Errorf("seamlessd.update: the update to v%s stopped at %s: %s", final.To, final.Stage, final.Error), err)
	}
	return err
}

// attendedPlan is what an attended `seamlessd update` decided before it
// changes anything -- all of it printed, and all of it a dry run shows.
type attendedPlan struct {
	// from is this binary's release, zero for a development build.
	from update.Version
	// target is the release to install, zero when the owner pinned
	// something that is not a release number (pin then carries it verbatim).
	target update.Version
	pin    string
	pinned bool
	// newest is the newest release in the list, zero when it was not read.
	newest update.Version
	// installer is the release whose install script runs: target, or the
	// newest release in the floor fallback (floor).
	installer update.Version
	floor     bool
	// managed is an installer-managed install this binary runs, or a
	// peer_auth system daemon's (system): backup, confirmation, rollback and
	// an attempt record apply. reason says why not; dir is
	// SEAMLESS_INSTALL_DIR when managed, the executable's directory for a
	// system install.
	managed bool
	system  bool
	reason  string
	dir     string
}

// runAttendedUpdate is the owner's `seamlessd update`: the same engine as the
// daemon's updater on an installer-managed install, attended.
//
//   - The target is the newest release by version from the release list
//     (update.Filter, update.Newest), never GitHub's /releases/latest, which a
//     backport can make an older version; SEAMLESS_VERSION set by the owner
//     pins it instead, in either direction. SEAMLESS_VERSION always reaches
//     the installer.
//   - The knobs the owner set win: their environment reaches the installer as
//     it is, and the updater adds only the knobs they did not set.
//   - A Homebrew install is refused with the brew command.
//   - A peer_auth system daemon that updates itself (update.KindSystem), run
//     as its account, is updated in place like the daemon's own updater does
//     it (runSystem). A peer_auth config on any other layout is refused: the
//     installer would set up a per-user service, which cannot host peer_auth.
//   - On an installer-managed install that this binary is (Detect, with gate 9
//     and the installer's own knobs set aside: updaterProbe), the update takes
//     the update lock, records its attempt (WhyManual), backs up, confirms the
//     release serving and rolls back when it does not. Already on the newest
//     release, it says so and changes nothing; pinning that release reinstalls
//     it.
//   - Anything else -- a source build (make update), a client machine, a
//     release build of another layout, a SEAMLESS_INSTALL_DIR that moves the
//     install, a pin that is not a release number -- runs the verified
//     installer as before, without a backup, a rollback or a record, and says
//     so: the attempt's From would not describe the install being replaced,
//     and there is no confirmation to vouch for an outcome.
//   - A pinned release from before signed checksums (the floor) is installed
//     by the newest release's installer, verified with that release's tag,
//     which fetches and checks the pinned archive itself.
//   - --url runs a custom installer as before: TLS only, nothing verified,
//     backed up or recorded.
func runAttendedUpdate(ctx context.Context, f updateFlags, d updaterDeps) error {
	w := d.stdout
	fmt.Fprintf(w, "\n%s %s\n", bold("Seamless"), dim("update"+dryRunTag(f.dryRun)))
	if u := strings.TrimSpace(f.url); u != "" {
		return runCustomInstaller(w, d, u, f.dryRun)
	}

	cfg, configPath, cfgErr := loadAttendedConfig(f.config)
	if errors.Is(cfgErr, fs.ErrPermission) {
		// A config this account may not read is most likely a peer_auth
		// daemon's (root:<account> 0640), and the installer must never run
		// over one: it would set up a per-user service beside it.
		return fmt.Errorf("seamlessd.update: %w; this account cannot read the config, so nothing is installed: "+
			"a peer_auth daemon updates as its own account (%s)", cfgErr, update.SystemHint)
	}
	ic := installConfig{path: configPath}
	if cfgErr == nil {
		ic = installConfigOf(cfg, configPath)
	}
	probe := updaterProbe(d.probe(ic), true)
	install := update.Detect(probe)
	if install.Kind == update.KindHomebrew {
		return fmt.Errorf("seamlessd.update: %s; update it with: %s", install.Reason, update.Hint(update.KindHomebrew))
	}
	fieldRowTo(w, "current", d.buildVersion)

	plan, err := planAttended(ctx, d, install, probe, cfgErr)
	if err != nil {
		return fmt.Errorf("seamlessd.update: %w", err)
	}
	if probe.PeerAuth && !plan.managed {
		return fmt.Errorf("seamlessd.update: this config turns peer_auth on, and the installer sets up a per-user service, which cannot host it (%s); "+
			"replace the binaries the daemon runs with sudo and restart it, or have it update itself as the peer_auth guide describes", plan.reason)
	}
	fieldRowTo(w, "target", plan.describeTarget())
	if plan.managed && !plan.pinned {
		switch c := plan.from.Compare(plan.target); {
		case c == 0:
			fieldRowTo(w, "status", dim("up to date"))
			fmt.Fprintf(w, "%s%s\n", fieldCont, dim(fmt.Sprintf("to reinstall it: SEAMLESS_VERSION=%s seamlessd update", plan.from)))
			return nil
		case c > 0:
			fieldRowTo(w, "status", dim("ahead of the newest published release; nothing to install"))
			fmt.Fprintf(w, "%s%s\n", fieldCont, dim("to move to another release, pin it: SEAMLESS_VERSION=X.Y.Z seamlessd update"))
			return nil
		}
	}
	plan.printRows(w, d, cfg)
	if f.dryRun {
		fmt.Fprintf(w, "%s%s\n", fieldCont, dim("no changes made -- re-run without --dry-run to update"))
		return nil
	}
	if !plan.managed {
		return runUnrecorded(ctx, d, plan)
	}

	id, err := core.NewID()
	if err != nil {
		return fmt.Errorf("seamlessd.update: %w", err)
	}
	req := update.SpawnRequest{AttemptID: id, From: plan.from, To: plan.target, Why: update.WhyManual}
	if err := req.Validate(); err != nil {
		return fmt.Errorf("seamlessd.update: %w", err)
	}
	job := updateJob{
		req: req, cfg: cfg, configPath: configPath, probe: probe,
		attended: true, installDir: plan.dir, newest: plan.newest, system: plan.system,
	}
	final, err := runRecorded(ctx, d, job)
	if final.ID == "" {
		return fmt.Errorf("seamlessd.update: %w", err)
	}
	fieldRowTo(w, "record", dim(tildePath(update.AttemptPath(cfg.DataDir))+"  ·  log "+tildePath(final.LogPath)))
	if !final.OK {
		return errors.Join(fmt.Errorf("seamlessd.update: v%s not installed (stopped at %s): %s", final.To, final.Stage, final.Error), err)
	}
	fieldRowTo(w, "update", green(fmt.Sprintf("v%s installed and serving", final.To)))
	return err
}

// loadAttendedConfig loads the config an attended update works with: --config
// when given, else the usual search. The path comes back absolute.
func loadAttendedConfig(flagPath string) (config.Config, string, error) {
	if flagPath != "" {
		cfg, err := config.LoadFrom(flagPath)
		return cfg, absConfigPath(flagPath), err
	}
	cfg, err := config.Load()
	return cfg, absConfigPath(cfg.SourcePath()), err
}

// planAttended decides what an attended update installs, with which
// installer, and whether it is managed.
func planAttended(ctx context.Context, d updaterDeps, install update.Install, probe update.Probe, cfgErr error) (attendedPlan, error) {
	var p attendedPlan
	p.from, _ = update.Parse(d.version) // zero for a development build, which is never managed
	p.pin, p.pinned = lookupEnvIn(d.goos, d.environ, "SEAMLESS_VERSION")
	p.pin = strings.TrimSpace(p.pin)
	targetOK := false
	if p.pinned {
		p.target, targetOK = update.Parse(p.pin)
	}
	if !p.pinned || !targetOK {
		newest, err := newestRelease(ctx, d)
		if err != nil {
			if errors.Is(err, update.ErrRateLimited) {
				return p, fmt.Errorf("%w (try again later, or pin a version: SEAMLESS_VERSION=x.y.z seamlessd update)", err)
			}
			return p, err
		}
		p.newest = newest
		if !p.pinned {
			p.target, targetOK = newest, true
		}
	}

	p.installer = p.target
	if !targetOK {
		// A pin that is not a release number (a release candidate, say) goes
		// to the newest installer verbatim, which installs it as it always did.
		p.installer, p.floor = p.newest, true
	} else {
		rel, err := d.releases.releaseByTag(ctx, p.target)
		if err != nil {
			if errors.Is(err, errFetchMissing) {
				return p, fmt.Errorf("v%s is not a published release", p.target)
			}
			return p, fmt.Errorf("re-read v%s: %w", p.target, err)
		}
		switch err := checkPublished(rel, p.target, releaseAssetNames(p.target, d.goos, d.goarch)); {
		case err == nil:
		case errors.Is(err, errReleaseIncomplete) &&
			checkPublished(rel, p.target, []string{platformArchive(p.target, d.goos, d.goarch)}) == nil:
			if p.newest.IsZero() {
				if p.newest, err = newestRelease(ctx, d); err != nil {
					return p, fmt.Errorf("v%s predates signed checksums, and the newest release to install it with is unknown: %w", p.target, err)
				}
			}
			p.installer, p.floor = p.newest, true
		case errors.Is(err, errReleaseInFlight):
			return p, fmt.Errorf("%w; try again in a few minutes", err)
		default:
			return p, err
		}
	}

	switch {
	case cfgErr != nil:
		p.reason = "the config did not load: " + cfgErr.Error()
	case install.Kind == update.KindSystem && (!targetOK || p.floor):
		p.reason = fmt.Sprintf("an in-place update needs signed checksums, and %s has none", p.describePin())
	case install.Kind == update.KindSystem:
		p.managed, p.system, p.dir = true, true, filepath.Dir(probe.Exe)
	case install.Kind != update.KindInstaller:
		p.reason = fmt.Sprintf("not an installer-managed install (%s): %s", install.Kind, install.Reason)
	case !targetOK:
		p.reason = fmt.Sprintf("SEAMLESS_VERSION=%s is not a release number X.Y.Z", p.pin)
	default:
		env := shapeEnv{goos: d.goos, home: d.home, environ: d.environ, sameFile: d.sameFile}
		dir, err := installDirFor(env, probe.Service, probe.Exe)
		if err != nil {
			p.reason = err.Error()
			break
		}
		if own, set := lookupEnvIn(d.goos, d.environ, "SEAMLESS_INSTALL_DIR"); set && !d.sameFile(strings.TrimSpace(own), dir) {
			p.reason = fmt.Sprintf("SEAMLESS_INSTALL_DIR moves the install from %s to %s", dir, own)
			break
		}
		p.managed, p.dir = true, dir
	}
	return p, nil
}

// describePin names the pinned or target release for a reason.
func (p attendedPlan) describePin() string {
	if p.target.IsZero() {
		return "SEAMLESS_VERSION=" + p.pin
	}
	return "v" + p.target.String()
}

// describeTarget is the target row.
func (p attendedPlan) describeTarget() string {
	switch {
	case p.target.IsZero():
		return p.pin + dim(" (SEAMLESS_VERSION; not a release number, installed unchecked by the installer)")
	case p.pinned:
		return "v" + p.target.String() + dim(" (SEAMLESS_VERSION)")
	default:
		return "v" + p.target.String() + dim(" (the newest release)")
	}
}

// printRows prints the plan: the installer's source and signature, where the
// install goes, the backup and rollback (or why there is none), and the
// equivalent hand-run one-liner.
func (p attendedPlan) printRows(w io.Writer, d updaterDeps, cfg config.Config) {
	if p.system {
		fieldRowTo(w, "source", d.releases.assetURL(p.target, platformArchive(p.target, d.goos, d.goarch)))
		fieldRowTo(w, "signature", dim(fmt.Sprintf("sigstore bundle for checksums.txt, signed by this repo's release workflow on v%s, which pins the archive", p.target)))
		fieldRowTo(w, "install", tildePath(p.dir)+dim(" (seamlessd and seam swapped in place; the service starts the daemon again)"))
		fieldRowTo(w, "backup", dim(tildePath(filepath.Join(backupDir(cfg.DataDir), backupName(p.from, d.now())))))
		fieldRowTo(w, "confirm", dim(fmt.Sprintf("v%s must then serve, or v%s's binaries go back", p.target, p.from)))
		return
	}
	src := d.releases.assetURL(p.installer, installerAsset(d.goos))
	fieldRowTo(w, "source", src)
	if p.floor {
		fieldRowTo(w, "signature", dim(fmt.Sprintf("sigstore bundle, signed by this repo's release workflow on v%s; that installer checks the archive it downloads", p.installer)))
	} else {
		fieldRowTo(w, "signature", dim(fmt.Sprintf("sigstore bundles for the installer and checksums.txt, signed by this repo's release workflow on v%s", p.installer)))
	}
	pinText := p.pin
	if !p.target.IsZero() {
		pinText = p.target.String()
	}
	if p.managed {
		fieldRowTo(w, "install", tildePath(p.dir))
		at := d.now()
		fieldRowTo(w, "backup", dim(tildePath(filepath.Join(backupDir(cfg.DataDir), backupName(p.from, at)))))
		fieldRowTo(w, "confirm", dim(fmt.Sprintf("v%s must then serve, or v%s is reinstalled", p.target, p.from)))
	} else {
		fieldRowTo(w, "backup", yellow("none")+dim(" -- "+p.reason+"; the installer runs without a backup, a rollback or a record"))
	}
	fieldRowTo(w, "run", dim(installRunHint(d.goos, src, pinText)))
}

// runUnrecorded runs a verified installer for an install the engine does not
// manage: download and verify the installer (and the target's checksums.txt,
// which then pins it), run it attended, and report its exit.
func runUnrecorded(ctx context.Context, d updaterDeps, p attendedPlan) error {
	w := d.stdout
	dl, err := d.releases.downloadRelease(ctx, p.installer, d.goos, !p.floor)
	if err != nil {
		return fmt.Errorf("seamlessd.update: download v%s: %w", p.installer, err)
	}
	r, err := verifyDownload(dl, d.goos, d.goarch, d.verify)
	if err != nil {
		return fmt.Errorf("seamlessd.update: refusing to run the v%s installer: %w", p.installer, err)
	}
	fmt.Fprintf(w, "%s%s\n", fieldCont, green("signature verified")+dim(fmt.Sprintf(" -- release workflow identity on tag v%s", p.installer)))
	knobs := map[string]string{}
	if !p.target.IsZero() {
		r.version = p.target
		knobs = releaseKnobs(r)
	}
	env, err := attendedInstallerEnv(d.goos, d.environ, knobs)
	if err != nil {
		return fmt.Errorf("seamlessd.update: %w", err)
	}
	fmt.Fprintf(w, "\n%s\n", dim("running the installer (unrecorded)..."))
	if err := d.runInstaller(installerRun{script: r.installer, env: env, out: w, timeout: d.timing.installer}); err != nil {
		return fmt.Errorf("seamlessd.update: the installer failed: %w", err)
	}
	return nil
}

// runCustomInstaller is --url: the installer at a URL the owner names, fetched
// over https (every redirect hop too) and run with their environment as it
// is. There is no bundle to verify and no release to pin, back up or roll
// back to, so it is TLS-only and unrecorded, with a printed warning.
func runCustomInstaller(w io.Writer, d updaterDeps, url string, dryRun bool) error {
	fieldRowTo(w, "source", url)
	fieldRowTo(w, "signature", yellow("none")+dim(" -- custom --url carries no sigstore bundle; https is the only authentication"))
	pin, _ := lookupEnvIn(d.goos, d.environ, "SEAMLESS_VERSION")
	fieldRowTo(w, "run", dim(installRunHint(d.goos, url, strings.TrimSpace(pin))))
	if dryRun {
		fmt.Fprintf(w, "%s%s\n", fieldCont, dim("no changes made -- re-run without --dry-run to update"))
		return nil
	}
	script, err := fetchInstallerWith(d.releases.client, url)
	if err != nil {
		return fmt.Errorf("seamlessd.update: %w", err)
	}
	fmt.Fprintf(w, "\n%s\n", dim("running the installer..."))
	env := d.environ
	if env == nil {
		env = []string{}
	}
	if err := d.runInstaller(installerRun{script: []byte(script), env: env, out: w, timeout: d.timing.installer}); err != nil {
		return fmt.Errorf("seamlessd.update: installer failed (equivalent to: %s): %w", installRunHint(d.goos, url, ""), err)
	}
	return nil
}

// installRunHint is the equivalent hand-run one-liner for running the
// installer at url on goos, pinned to version when one is given -- shown for
// transparency. It runs without the updater's signature checks.
func installRunHint(goos, url, version string) string {
	if goos == "windows" {
		if version != "" {
			return fmt.Sprintf("$env:SEAMLESS_VERSION='%s'; irm %s | iex", version, url)
		}
		return "irm " + url + " | iex"
	}
	if version != "" {
		return fmt.Sprintf("curl -fsSL %s | SEAMLESS_VERSION=%s sh", url, version)
	}
	return "curl -fsSL " + url + " | sh"
}

// fetchInstaller downloads the installer script at url over HTTPS with the
// update client (https-only redirects, a whole-request timeout).
func fetchInstaller(url string) (string, error) {
	return fetchInstallerWith(update.NewHTTPClient(), url)
}

// fetchInstallerWith fetches the --url installer with client. Its output is
// piped to a shell, so the transport is required-HTTPS, on the URL itself
// (--url can name anything) and -- through the client's CheckRedirect -- on
// every redirect hop: a 302 from https to http would otherwise hand the whole
// script to whoever is on the wire.
func fetchInstallerWith(client *http.Client, url string) (string, error) {
	if err := update.RequireHTTPS(url); err != nil {
		return "", fmt.Errorf("the installer is piped to a shell: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", update.UserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch installer %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch installer %s: unexpected status %s", url, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAssetBytes+1))
	if err != nil {
		return "", fmt.Errorf("read installer %s: %w", url, err)
	}
	if len(body) > maxAssetBytes {
		return "", fmt.Errorf("fetch installer %s: response exceeds %d bytes", url, maxAssetBytes)
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return "", fmt.Errorf("fetch installer %s: empty response", url)
	}
	return string(body), nil
}

// cliUpdateView is the update view `update --check` adds the daemon's rows
// from (updateCheckRows), or nil when no config loads (the check itself still
// runs). configPath is --config, "" for the usual search. The database is
// opened read-only and only to read the console's override: a newer CLI must
// never migrate a database an older daemon serves.
func cliUpdateView(ctx context.Context, configPath string) *updateView {
	cfg, _, err := loadAttendedConfig(configPath)
	if err != nil {
		return nil
	}
	var db *sql.DB
	if !cfg.IsClient() {
		if d, oerr := store.OpenExisting(cfg.DBPath()); oerr == nil {
			db = d
			defer func() { _ = db.Close() }()
		}
	}
	v := loadUpdateView(ctx, cfg, db)
	return &v
}

// reportUpdateCheck reads the release list and compares its newest
// installable release for goos with the running build, printing a short
// verdict. current is the bare version that is compared; display is what the
// "current" row shows (it carries +commit). view, when set, adds what the
// daemon's update check and automatic updates are doing (updateCheckRows),
// and those rows print even when the release list cannot be read: they come
// from the daemon's own records, and a failed or paused update is when the
// owner needs them. It changes nothing, and it asks regardless of
// update.check: running it is the owner asking.
//
// "Newest" is the highest version the list holds once drafts, prereleases,
// irregular tags and releases still uploading their assets are dropped
// (update.Filter) -- not GitHub's "latest", which a backport can make an older
// version (memory github-releases-latest-is-not-max-version).
func reportUpdateCheck(ctx context.Context, w io.Writer, f *update.Fetcher, goos, current, display string, view *updateView) error {
	page, err := f.Fetch(ctx, "")
	if err != nil {
		if errors.Is(err, update.ErrRateLimited) {
			err = fmt.Errorf("seamlessd.update: %w (try again later, or pin a version: SEAMLESS_VERSION=x.y.z seamlessd update)", err)
		} else {
			err = fmt.Errorf("seamlessd.update: %w", err)
		}
		if view != nil {
			fmt.Fprintf(w, "\n%s %s\n", bold("Seamless"), dim("update --check"))
			fieldRowTo(w, "current", display)
			fieldRowTo(w, "newest", yellow("unknown")+dim(" -- the release list could not be read"))
			updateCheckRows(w, *view, time.Now())
		}
		return err
	}

	fmt.Fprintf(w, "\n%s %s\n", bold("Seamless"), dim("update --check"))
	fieldRowTo(w, "current", display)
	newest, ok := update.Newest(update.Filter(page.Releases, goos))
	if !ok {
		fieldRowTo(w, "newest", yellow("none")+dim(" -- no installable release for "+goos+" in the newest 20"))
		return nil
	}
	fieldRowTo(w, "newest", "v"+newest.Version.String()+dim(" (published "+newest.PublishedAt.UTC().Format("2006-01-02")+")"))

	cur, curOK := update.Parse(current)
	switch {
	case !curOK:
		fieldRowTo(w, "status", yellow("development build")+dim(" -- 'seamlessd update' installs the newest release"))
	case cur.Compare(newest.Version) < 0:
		fieldRowTo(w, "status", green("update available")+dim(" -- run 'seamlessd update' to upgrade"))
	case cur.Compare(newest.Version) == 0:
		fieldRowTo(w, "status", dim("up to date"))
	default:
		fieldRowTo(w, "status", dim("ahead of the newest published release"))
	}
	if view != nil {
		updateCheckRows(w, *view, time.Now())
	}
	return nil
}
