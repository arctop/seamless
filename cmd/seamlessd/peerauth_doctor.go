package main

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/store"
)

// peerAuthChecks is doctor's peer_auth section: the policy as it resolves on
// this machine, every reason serve would refuse to start under it, and the one
// cost the mode carries. Run it as the daemon's own account (sudo -u) against
// the daemon's config: the file and account checks describe whoever runs it.
func peerAuthChecks(ctx context.Context, cfg config.Config, db *sql.DB, env peerAuthEnv) []check {
	const name = "peer_auth"
	pa := cfg.PeerAuth
	if !pa.Enabled {
		if len(pa.ForceLocked) > 0 {
			return []check{{statusWarn, name, "off, but peer_auth.force_locked is filled in: nothing is locked until peer_auth.enabled is true"}}
		}
		return []check{{statusInfo, name, "off: no socket; the TCP bearer key reaches every project (the default)"}}
	}
	if !access.PeerCredSupported {
		return []check{{statusFail, name, "enabled, but this platform has no Unix-socket peer credentials Seamless can read (macOS and Linux only)"}}
	}
	policy, err := access.New(pa, env.resolve)
	if err != nil {
		return []check{{statusFail, name, err.Error()}}
	}
	if db != nil {
		console, lerr := store.ConsoleLockedSlugs(ctx, db)
		if lerr != nil {
			return []check{{statusFail, name, "console locks: " + lerr.Error()}}
		}
		policy = policy.WithConsoleLocks(console)
	}
	owner := policy.Owner()
	mode := pa.Socket.Mode
	if strings.TrimSpace(mode) == "" {
		mode = config.DefaultPeerSocketMode
	}
	out := []check{{statusOK, name, fmt.Sprintf("socket %s (mode %s%s); owner %s (uid %d) on TCP and the socket",
		pa.Socket.Path, mode, groupSuffix(pa.Socket.Group), owner.Name, owner.UID)}}
	if len(pa.LegacyLocked) > 0 {
		out = append(out, check{statusWarn, "peer_auth.force_locked", "this config uses peer_auth.locked, the old name of peer_auth.force_locked: rename the key"})
	}
	if locked := policy.OwnerLocked(); len(locked) > 0 {
		out = append(out, check{statusOK, "peer_auth.force_locked", fmt.Sprintf("locked: [%s] (force locked in this file: [%s]); closed to every owner request "+
			"that holds no lease the owner approved (the TCP bearer key never holds one)", strings.Join(locked, ", "), strings.Join(policy.ForceLocked(), ", "))})
		out = append(out, approverCheck(ctx, db, runtime.GOOS))
	}
	for _, a := range policy.RestrictedAccounts() {
		out = append(out, check{statusOK, "peer_auth.restricted", fmt.Sprintf("%s (uid %d): restricted -- the socket only; "+
			"reads and writes every open project that is not locked, reads global; its writes are labeled with its name, "+
			"and its constraints, conventions, protocols and stages wait for the owner's promotion", a.Name, a.UID)})
	}
	rt := &peerAuthRuntime{policy: policy, cfg: pa, env: env}
	for _, w := range rt.restrictedReachWarnings() {
		out = append(out, check{statusWarn, "peer_auth.restricted", w})
	}
	problems := rt.filesystemProblems(cfg, false)
	if db != nil {
		pp, perr := projectProblems(ctx, db, policy)
		if perr != nil {
			problems = append(problems, "projects: "+perr.Error())
		}
		problems = append(problems, pp...)
	}
	for _, p := range problems {
		out = append(out, check{statusFail, name, p})
	}
	if len(problems) == 0 {
		out = append(out, check{statusOK, name, "preflight passes: serve will start with the boundary up"})
	}
	out = append(out, check{statusInfo, "peer_auth.captures",
		"the daemon reads no agent files under peer_auth: seam hook reads them and pushes session-end findings and token " +
			"totals, the model, plan-file and subagent capture and the git head, from clients whose server_url is the " +
			"peer_auth socket; agents on the TCP key make none of these captures"})
	return out
}

func groupSuffix(g string) string {
	if strings.TrimSpace(g) == "" {
		return ""
	}
	return ", group " + g
}

// approverCheck reports the enrolled approver credentials while a project is
// locked (so under peer_auth). None is a warning, not a failure: the terminal
// path (seam approve, confirmed by a sudo to the daemon's account) needs no
// credential. goos is the daemon's platform, for how to enroll one.
func approverCheck(ctx context.Context, db *sql.DB, goos string) check {
	const name = "peer_auth.approvers"
	if db == nil {
		return check{statusInfo, name, "not checked: no database"}
	}
	creds, err := store.ApproverCredentials(ctx, db, "")
	if err != nil {
		return check{statusFail, name, err.Error()}
	}
	if len(creds) == 0 {
		return check{statusWarn, name, "no passkey or Touch ID key is enrolled: only the terminal path (seam approve) can approve " +
			"access to a locked project; to enroll one, " + firstApproverAdvice(goos, true)}
	}
	kinds := map[string]int{}
	for _, c := range creds {
		kinds[c.Kind]++
	}
	return check{statusOK, name, fmt.Sprintf("%d enrolled (passkeys %d, Touch ID keys %d)",
		len(creds), kinds[store.ApproverPasskey], kinds[store.ApproverTouchID])}
}

// restrictedReachWarnings reports why a restricted account might not reach the
// socket at all: a socket under the owner's home directory (which the account
// cannot traverse), or a directory on the way to it that other accounts cannot
// search. They are warnings, not preflight failures -- a socket group the
// account belongs to can open a 0750 directory, and the owner may add the
// account to it later -- and they say nothing while no account is restricted.
func (rt *peerAuthRuntime) restrictedReachWarnings() []string {
	if !rt.policy.HasRestricted() {
		return nil
	}
	var out []string
	sock := filepath.Clean(rt.cfg.Socket.Path)
	if home := strings.TrimSpace(rt.policy.Owner().Home); home != "" && home != "/" && pathWithin(sock, home) {
		out = append(out, fmt.Sprintf("socket %s is inside the owner's home %s, which a restricted account cannot traverse: "+
			"put the socket in a directory of its own outside every home (such as /opt/seamless/run)", sock, home))
	}
	if strings.TrimSpace(rt.cfg.Socket.Group) != "" {
		return out // reach goes through the group; membership is the owner's to check
	}
	for dir := filepath.Dir(sock); ; dir = filepath.Dir(dir) {
		facts, err := rt.env.inspect(dir)
		if err == nil && facts.isDir && facts.mode.Perm()&0o001 == 0 {
			out = append(out, fmt.Sprintf("directory %s (mode %#o) is not searchable by other accounts, so a restricted "+
				"account cannot reach the socket under it: allow others to search it (o+x), or hand the socket to a group "+
				"the account is in (peer_auth.socket.group)", dir, facts.mode.Perm()))
		}
		if parent := filepath.Dir(dir); parent == dir {
			break
		}
	}
	return out
}

// pathWithin reports whether path is dir or lies inside it, lexically.
func pathWithin(path, dir string) bool {
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(path))
	return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))))
}
