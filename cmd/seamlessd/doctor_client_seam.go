package main

// Doctor's "hooks' seam" row on a peer_auth client (server_url is the
// socket): who can replace the seam executable the Claude Code hooks run,
// and, for a copy only root can replace, whether it is still the daemon's
// release.
//
// The hooks run seam as the owner, beside every agent, so whoever can
// rewrite that file runs code as the owner at the next hook. Three layouts
// (docs-src/guides/peer-auth.md, "Keep the hooks' seam out of the daemon's
// reach"):
//
//   - the owner's own copy (~/.local/bin/seam): any agent running as the
//     owner can rewrite it;
//   - the daemon's (/opt/seamless/bin/seam): no agent can, and it moves with
//     the daemon's own updates, but a compromised daemon could rewrite it;
//   - root's (/opt/seamless/client/seam): neither can, and an administrator
//     refreshes it after each update, so it may run a release behind.
//
// The row says which one this machine has. It is information, not a fault,
// for the first two (the guide's trade-off); a root's copy built from another
// commit than the daemon's is a warning, with the refresh step.

import (
	"debug/buildinfo"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/hooks"
)

// guideClientSection is the guide's section on the root-owned client copy.
const guideClientSection = `"Keep the hooks' seam out of the daemon's reach"`

// clientSeamEnv is what the row reads of the machine, injectable for tests.
type clientSeamEnv struct {
	// seams are the seam executables the hooks run, as the settings record them.
	seams []string
	euid  int
	// inspect is the owner and mode of a path without following a final symlink.
	inspect func(string) (fileFacts, error)
	resolve func(string) (string, error)
	// daemonPin reads root's pin of the daemon's account (access.DaemonAccount).
	daemonPin func() (access.DaemonPin, bool, error)
	// revision is the commit a binary was built from (its embedded VCS
	// revision), ok false when it carries none.
	revision func(path string) (string, bool)
	// daemonCommit is the commit the daemon serves at /healthz, ok false when
	// it cannot be read.
	daemonCommit func() (version, commit string, ok bool)
}

// hostClientSeamEnv reads this machine for cfg.
func hostClientSeamEnv(cfg config.Config) clientSeamEnv {
	env := clientSeamEnv{
		euid: os.Geteuid(), inspect: inspectPath, resolve: filepath.EvalSymlinks, daemonPin: access.DaemonAccount,
		revision: binaryRevision,
		daemonCommit: func() (string, string, bool) {
			return healthzCommit(cfg)
		},
	}
	var settings []string
	if home, err := expandHome("~/.claude/settings.json"); err == nil {
		settings = append(settings, home)
	}
	if managed, err := claudeManagedSettingsFiles(claudeManagedSettingsDir()); err == nil {
		settings = append(settings, managed...)
	}
	for _, path := range settings {
		bin, _, ok := hooks.RecordedCommandPaths(hooks.ClientClaudeCode, path)
		if !ok {
			continue
		}
		if expanded, err := expandHome(bin); err == nil {
			bin = expanded
		}
		if !slices.Contains(env.seams, bin) {
			env.seams = append(env.seams, bin)
		}
	}
	return env
}

// binaryRevision is the VCS revision Go embedded in the binary at path.
func binaryRevision(path string) (string, bool) {
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return "", false
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" && s.Value != "" {
			return s.Value, true
		}
	}
	return "", false
}

// healthzCommit asks the daemon cfg dials which release and commit it runs.
// /healthz sends no credential and reveals no data.
func healthzCommit(cfg config.Config) (string, string, bool) {
	client, err := cfg.HTTPClient(codexActivityTimeout)
	if err != nil {
		return "", "", false
	}
	resp, err := client.Get(cfg.ServerURL() + "/healthz")
	if err != nil {
		return "", "", false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", "", false
	}
	var body struct {
		Version string `json:"version"`
		Commit  string `json:"commit"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body); err != nil || body.Commit == "" {
		return "", "", false
	}
	version, _, _ := strings.Cut(body.Version, "+")
	return version, body.Commit, true
}

// clientSeamChecks is the row for each seam the hooks run, on a client of a
// peer_auth socket only: elsewhere there is no daemon account to keep it
// from, and the report is as it was.
func clientSeamChecks(cfg config.Config) []check {
	if cfg.SocketPath() == "" || (runtime.GOOS != "darwin" && runtime.GOOS != "linux") {
		return nil
	}
	return clientSeamRows(hostClientSeamEnv(cfg))
}

// clientSeamRows judges each seam in env.
func clientSeamRows(env clientSeamEnv) []check {
	const name = "hooks' seam"
	daemonUID, daemonName := -1, "the daemon's account"
	if pin, pinned, err := env.daemonPin(); err == nil && pinned {
		daemonUID, daemonName = int(pin.UID), pin.Name+" (the daemon's account)"
	}
	var out []check
	for _, seam := range env.seams {
		writers, err := pathWriters(seam, env)
		if err != nil {
			continue // the hooks row reports a missing executable
		}
		switch {
		case slices.Contains(writers, env.euid):
			out = append(out, check{statusInfo, name, fmt.Sprintf("the hooks run %s, which your account can replace, and so can every agent "+
				"running as you; install the hooks with --seam naming a copy only root can replace, as the peer_auth guide does under %s",
				seam, guideClientSection)})
		case daemonUID >= 0 && slices.Contains(writers, daemonUID):
			out = append(out, check{statusInfo, name, fmt.Sprintf("the hooks run %s, which %s can replace: it moves with the daemon's own updates, "+
				"and a compromised daemon could rewrite it; to keep it out of the daemon's reach, see the peer_auth guide under %s",
				seam, daemonName, guideClientSection)})
		case len(writers) == 1 && writers[0] == 0:
			out = append(out, rootSeamRow(seam, env))
		default:
			uids := make([]string, 0, len(writers))
			for _, w := range writers {
				uids = append(uids, describeWriter(w))
			}
			out = append(out, check{statusWarn, name, fmt.Sprintf("the hooks run %s, which %s can replace", seam, strings.Join(uids, " and "))})
		}
	}
	return out
}

// anyAccount stands for "every account": a group- or world-writable path.
const anyAccount = -2

func describeWriter(uid int) string {
	if uid == anyAccount {
		return "every account"
	}
	return "uid " + strconv.Itoa(uid)
}

// rootSeamRow is the row for a seam only root can replace: OK, unless it was
// built from another commit than the daemon runs.
func rootSeamRow(seam string, env clientSeamEnv) check {
	const name = "hooks' seam"
	ok := fmt.Sprintf("the hooks run %s, which only root can replace: neither your agents nor the daemon's account can rewrite it", seam)
	rev, haveRev := env.revision(seam)
	version, commit, haveCommit := env.daemonCommit()
	if !haveRev || !haveCommit {
		return check{statusOK, name, ok + "; refresh it as root after each update the daemon installs (the peer_auth guide, " + guideClientSection + ")"}
	}
	if strings.HasPrefix(rev, commit) {
		return check{statusOK, name, ok + fmt.Sprintf("; it is the daemon's release (v%s)", version)}
	}
	short := rev
	if len(short) > 7 {
		short = short[:7]
	}
	return check{statusWarn, name, fmt.Sprintf("the hooks run %s, root's copy, built from commit %s, but the daemon runs v%s (commit %s): "+
		"refresh it as root, as the peer_auth guide does under %s", seam, short, version, commit, guideClientSection)}
}

// pathWriters lists the accounts that could replace path: the owners of the
// file and of every directory above it (the link's and the target's), but
// root, plus anyAccount where a mode lets every account write (a sticky
// directory aside). root is listed only when no one else is.
func pathWriters(path string, env clientSeamEnv) ([]int, error) {
	real, err := env.resolve(path)
	if err != nil {
		return nil, err
	}
	var writers []int
	add := func(uid int) {
		if !slices.Contains(writers, uid) {
			writers = append(writers, uid)
		}
	}
	facts, err := env.inspect(real)
	if err != nil {
		return nil, err
	}
	if facts.uid != 0 {
		add(int(facts.uid))
	}
	if facts.mode.Perm()&0o022 != 0 {
		add(anyAccount)
	}
	starts := []string{filepath.Dir(real)}
	if d := filepath.Dir(filepath.Clean(path)); d != starts[0] {
		starts = append(starts, d)
	}
	for _, start := range starts {
		start, err := env.resolve(start)
		if err != nil {
			return nil, err
		}
		for dir := start; ; dir = filepath.Dir(dir) {
			f, err := env.inspect(dir)
			if err != nil {
				return nil, err
			}
			if f.uid != 0 {
				add(int(f.uid))
			}
			if f.mode.Perm()&0o022 != 0 && f.mode&os.ModeSticky == 0 {
				add(anyAccount)
			}
			if next := filepath.Dir(dir); next == dir {
				break
			}
		}
	}
	if len(writers) == 0 {
		writers = []int{0}
	}
	return writers, nil
}
