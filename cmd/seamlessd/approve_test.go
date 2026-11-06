package main

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
)

// seamlessd approve commits only as the daemon's own account: the data dir
// is its and 0700, and it is not the owner.
func TestApproverAccountCheck(t *testing.T) {
	cfg := config.Defaults()
	cfg.DataDir = "/opt/seamless/data"
	cfg.PeerAuth = config.PeerAuth{Enabled: true, Owner: "owner", Locked: []string{"example-finance"}}
	env := func(euid int, dirUID uint32, mode os.FileMode) peerAuthEnv {
		return peerAuthEnv{
			euid: euid,
			resolve: func(name string) (access.Account, error) {
				return access.Account{UID: 501, Name: name}, nil
			},
			inspect: func(string) (fileFacts, error) {
				return fileFacts{uid: dirUID, mode: os.ModeDir | mode, isDir: true}, nil
			},
		}
	}
	require.NoError(t, approverAccountCheck(cfg, env(480, 480, 0o700)), "the daemon's account")
	require.ErrorContains(t, approverAccountCheck(cfg, env(501, 480, 0o700)), "not the daemon's account", "the owner without sudo")
	require.ErrorContains(t, approverAccountCheck(cfg, env(480, 480, 0o755)), "not the daemon's account", "a data dir others can read is no proof")
	require.ErrorContains(t, approverAccountCheck(cfg, env(501, 501, 0o700)), "owner's account", "the owner's own data dir")

	off := cfg
	off.PeerAuth.Locked = nil
	require.ErrorContains(t, approverAccountCheck(off, env(480, 480, 0o700)), "locks no project")
	disabled := cfg
	disabled.PeerAuth.Enabled = false
	require.ErrorContains(t, approverAccountCheck(disabled, env(480, 480, 0o700)), "locks no project")
}
