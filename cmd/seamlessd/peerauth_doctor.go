package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
)

// peerAuthChecks is doctor's peer_auth section: the policy as it resolves on
// this machine, every reason serve would refuse to start under it, and the one
// cost the mode carries. Run it as the daemon's own account (sudo -u) against
// the daemon's config: the file and account checks describe whoever runs it.
func peerAuthChecks(ctx context.Context, cfg config.Config, db *sql.DB, env peerAuthEnv) []check {
	const name = "peer_auth"
	pa := cfg.PeerAuth
	if !pa.Enabled {
		if len(pa.Users) > 0 {
			return []check{{statusWarn, name, "off, but peer_auth.users is filled in: nothing is sealed until peer_auth.enabled is true"}}
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
	owner := policy.Owner()
	mode := pa.Socket.Mode
	if strings.TrimSpace(mode) == "" {
		mode = config.DefaultPeerSocketMode
	}
	out := []check{{statusOK, name, fmt.Sprintf("socket %s (mode %s%s); owner %s (uid %d) on TCP and the socket",
		pa.Socket.Path, mode, groupSuffix(pa.Socket.Group), owner.Name, owner.UID)}}
	for _, a := range policy.Accounts() {
		out = append(out, check{statusOK, "peer_auth.user", fmt.Sprintf("%s (uid %d): sealed [%s], shared [%s]; sessions land in %s",
			a.Account.Name, a.Account.UID, strings.Join(a.Sealed, ", "), strings.Join(a.Projects, ", "), a.Home)})
	}
	rt := &peerAuthRuntime{policy: policy, cfg: pa, env: env}
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
		"the daemon reads no agent files under peer_auth: session-end findings and token totals, model sniffing, " +
			"plan-file and subagent capture are off; agents record findings through session_end instead"})
	return out
}

func groupSuffix(g string) string {
	if strings.TrimSpace(g) == "" {
		return ""
	}
	return ", group " + g
}
