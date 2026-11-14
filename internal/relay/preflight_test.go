//go:build darwin || linux

package relay

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
)

// relayOwnedBy reports path (and nothing else) as owned by uid.
func relayOwnedBy(env Env, path string, uid uint32) Env {
	inner := env.Inspect
	env.Inspect = func(p string) (FileFacts, error) {
		f, err := inner(p)
		if err == nil && filepath.Clean(p) == filepath.Clean(path) {
			f.UID = uid
		}
		return f, err
	}
	return env
}

// relayRequireProblem asserts exactly one problem, containing want.
func relayRequireProblem(t *testing.T, problems []string, want, name string) {
	t.Helper()
	require.Len(t, problems, 1, "%s: %v", name, problems)
	require.Contains(t, problems[0], want, name)
}

// The runbook's layout passes, and a missing data directory is created 0700
// once its parents have passed.
func TestRelayPreflight_AcceptsTheLayoutAndCreatesTheDataDir(t *testing.T) {
	l := relayLayoutAt(t)
	env := relayTestEnv(t, l)
	require.Empty(t, env.Problems(l, false))
	cert := relayNewCert(t, "example-server")
	relayWriteState(t, l, "https://example-server:8081", cert.fp, cert.pem)
	require.Empty(t, env.Problems(l, false), "with its pairing in place")

	fresh := relayLayoutAt(t)
	require.NoError(t, os.Remove(fresh.DataDir))
	freshEnv := relayTestEnv(t, fresh)
	require.Len(t, freshEnv.Problems(fresh, false), 1, "doctor only looks")
	require.Empty(t, freshEnv.Problems(fresh, true))
	require.Equal(t, os.FileMode(0o700), relayMode(t, fresh.DataDir))
	require.Empty(t, freshEnv.PairProblems(fresh.DataDir))

	// An account the relay may not run as makes nothing: a data dir it made
	// would be its own, and the relay's account would be refused it.
	other := relayLayoutAt(t)
	require.NoError(t, os.Remove(other.DataDir))
	root := relayTestEnv(t, other)
	root.EUID = 0
	require.NotEmpty(t, root.Problems(other, true))
	require.NotEmpty(t, root.PairProblems(other.DataDir))
	_, err := os.Lstat(other.DataDir)
	require.ErrorIs(t, err, os.ErrNotExist)
}

// The relay runs as the account the agents' clients trust to answer the
// socket: the pinned one, or with no pin a system account, never root.
func TestRelayPreflight_TheAccountRule(t *testing.T) {
	l := relayLayoutAt(t)
	me := uint32(os.Geteuid())
	pinned := func(uid uint32) func() (access.DaemonPin, bool, error) {
		return func() (access.DaemonPin, bool, error) {
			return access.DaemonPin{Name: "_seamless", UID: uid, Path: access.DaemonAccountPath}, true, nil
		}
	}
	for name, tc := range map[string]struct {
		edit func(*Env)
		want string
	}{
		"the pinned account": {func(e *Env) { e.DaemonAccount = pinned(me) }, ""},
		"another than the pinned": {func(e *Env) { e.DaemonAccount = pinned(me + 1) },
			fmt.Sprintf("the relay runs as uid %d, but %s pins the daemon's account as _seamless (uid %d)", me, access.DaemonAccountPath, me+1)},
		"no pin, not a system account": {func(e *Env) {
			e.SystemAccount = func(uint32) (bool, string) { return false, "system accounts are below uid 500" }
		}, "which is not a system account (system accounts are below uid 500)"},
		"a pin not believed": {func(e *Env) {
			e.DaemonAccount = func() (access.DaemonPin, bool, error) {
				return access.DaemonPin{}, true, fmt.Errorf("%w %s is owned by uid 501", access.ErrDaemonPin, access.DaemonAccountPath)
			}
		}, "no socket is trusted while it stands"},
	} {
		env := relayTestEnv(t, l)
		tc.edit(&env)
		problems := env.AccountProblems()
		if tc.want == "" {
			require.Empty(t, problems, name)
			continue
		}
		relayRequireProblem(t, problems, tc.want, name)
		require.Equal(t, problems, env.Problems(l, false), "%s: serve refuses on it", name)
	}
	root := relayTestEnv(t, l)
	root.EUID = 0
	relayRequireProblem(t, root.AccountProblems(), "the relay runs as root", "root")
}

// Every way another account could change what the relay says is refused.
func TestRelayPreflight_RefusesAnUnsafeLayout(t *testing.T) {
	other := uint32(os.Geteuid()) + 4242
	for name, tc := range map[string]struct {
		edit func(t *testing.T, l *Layout, env *Env)
		want string
	}{
		"a data dir others can read": {func(t *testing.T, l *Layout, _ *Env) { require.NoError(t, os.Chmod(l.DataDir, 0o750)) },
			"must be 0700"},
		"a data dir of another account": {func(_ *testing.T, l *Layout, env *Env) { *env = relayOwnedBy(*env, l.DataDir, other) },
			fmt.Sprintf("is owned by uid %d, not the relay's account", other)},
		"a symlinked data dir": {func(t *testing.T, l *Layout, _ *Env) {
			real := l.DataDir + "-real"
			require.NoError(t, os.Rename(l.DataDir, real))
			require.NoError(t, os.Symlink(real, l.DataDir))
		}, "is a symlink"},
		"a socket dir others can write": {func(t *testing.T, l *Layout, _ *Env) { require.NoError(t, os.Chmod(filepath.Dir(l.Socket), 0o775)) },
			"must not be group- or world-writable"},
		"a socket dir of another account": {func(_ *testing.T, l *Layout, env *Env) { *env = relayOwnedBy(*env, filepath.Dir(l.Socket), other) },
			"it must belong to the relay's account or root"},
		"a directory above writable by others": {func(t *testing.T, l *Layout, _ *Env) {
			require.NoError(t, os.Chmod(filepath.Dir(filepath.Dir(l.Socket)), 0o777))
		}, "writable by other accounts"},
		"a binary others can write": {func(t *testing.T, _ *Layout, env *Env) {
			exe, err := env.Exe()
			require.NoError(t, err)
			require.NoError(t, os.Chmod(exe, 0o775))
		}, "the seamlessd binary"},
		"a binary of another account": {func(t *testing.T, _ *Layout, env *Env) {
			exe, err := env.Exe()
			require.NoError(t, err)
			*env = relayOwnedBy(*env, exe, other)
		}, "it must belong to root or the relay's account"},
		"a socket inside the data dir": {func(_ *testing.T, l *Layout, _ *Env) { l.Socket = filepath.Join(l.DataDir, "s.sock") },
			"is inside the data directory"},
		"a pairing others can read": {func(t *testing.T, l *Layout, _ *Env) {
			require.NoError(t, os.Chmod(filepath.Join(l.DataDir, StateName), 0o644))
		}, "chmod 600"},
		"a pairing of another account": {func(_ *testing.T, l *Layout, env *Env) {
			*env = relayOwnedBy(*env, filepath.Join(l.DataDir, StateName), other)
		}, "pair again as the relay's account"},
	} {
		l := relayLayoutAt(t)
		env := relayTestEnv(t, l)
		cert := relayNewCert(t, "example-server")
		relayWriteState(t, l, "https://example-server:8081", cert.fp, cert.pem)
		tc.edit(t, &l, &env)
		problems := env.Problems(l, true)
		require.NotEmpty(t, problems, name)
		require.True(t, relayAnyContains(problems, tc.want), "%s: %v", name, problems)
	}
}

func relayAnyContains(ss []string, sub string) bool {
	for _, s := range ss {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// The socket: a stale one of the relay's own is replaced, a live one is
// refused, and anything else at the path is refused.
func TestRelayListen_StaleLiveAndForeign(t *testing.T) {
	l := relayLayoutAt(t)
	env := relayTestEnv(t, l)
	ln, err := env.Listen(l.Socket)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o666), relayMode(t, l.Socket)&0o777, "every local account may connect; the server judges the uid")
	_, err = env.Listen(l.Socket)
	require.ErrorIs(t, err, ErrSocketTaken, "a running relay or daemon is never displaced")
	require.NoError(t, ln.(*net.UnixListener).Close())

	// A stale socket (no one listens) left by a relay that died.
	stale, err := net.Listen("unix", l.Socket)
	require.NoError(t, err)
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	require.NoError(t, stale.Close())
	_, err = os.Lstat(l.Socket)
	require.NoError(t, err, "the stale socket is still there")
	ln, err = env.Listen(l.Socket)
	require.NoError(t, err)
	require.NoError(t, ln.Close())

	require.NoError(t, os.WriteFile(l.Socket, []byte("x"), 0o600))
	_, err = env.Listen(l.Socket)
	require.ErrorIs(t, err, ErrSocketTaken, "a file that is not a socket is never removed")
	require.NoError(t, os.Remove(l.Socket))

	foreign := relayOwnedBy(env, l.Socket, uint32(os.Geteuid())+1)
	stale, err = net.Listen("unix", l.Socket)
	require.NoError(t, err)
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	require.NoError(t, stale.Close())
	_, err = foreign.Listen(l.Socket)
	require.True(t, errors.Is(err, ErrSocketTaken), "another account's socket is never removed: %v", err)
}
