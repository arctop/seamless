package access

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
)

// The owner-locked view (peer_auth.sealed): a passkey-sealed project is listed
// for the owner and readable only under a lease that names the request's own
// socket process.

const (
	lockFin   = "arctop-finance"
	lockLegal = "arctop-legal"
	lockOps   = "arctop-ops"
)

var (
	lockNow   = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	lockProc  = ProcessToken{PID: 4242, Version: 7, Boot: "6c2f1a3e-0b7d-4c55-9a51-3d8f00a1b2c3"}
	lockOther = ProcessToken{PID: 4243, Version: 7, Boot: lockProc.Boot}
)

func lockPolicy(t *testing.T) *Policy {
	t.Helper()
	p, err := New(config.PeerAuth{
		Enabled: true,
		Owner:   "katata",
		Sealed:  []string{lockLegal, lockFin},
	}, testResolve)
	require.NoError(t, err)
	return p
}

func lockLease(project string, proc ProcessToken, expires time.Time) Lease {
	return Lease{ID: "lease-" + project, Project: project, Process: proc.Key(), Expires: expires}
}

// socketView is the owner's socket view for proc, with leases attached at now.
func socketView(t *testing.T, p *Policy, proc ProcessToken, leases ...Lease) View {
	t.Helper()
	v, ok := p.PeerProcessView(501, proc)
	require.True(t, ok)
	return v.WithLeases(lockNow, leases)
}

func TestLockedView_Matrix(t *testing.T) {
	p := lockPolicy(t)
	live := lockLease(lockFin, lockProc, lockNow.Add(3*time.Hour))
	tcp := p.OwnerView().WithLeases(lockNow, []Lease{live})
	noProc, ok := p.PeerView(501)
	require.True(t, ok)
	noProc = noProc.WithLeases(lockNow, []Lease{live})

	views := map[string]View{
		"no view (peer_auth off)":             {},
		"owner over TCP, handed a live lease": tcp,
		"owner on the socket, no process":     noProc,
		"owner on the socket, locked":         socketView(t, p, lockProc),
		"owner on the socket, leased":         socketView(t, p, lockProc, live),
		"owner, lease ended an instant ago":   socketView(t, p, lockProc, lockLease(lockFin, lockProc, lockNow.Add(-time.Nanosecond))),
		"owner, lease ends exactly now":       socketView(t, p, lockProc, lockLease(lockFin, lockProc, lockNow)),
		"owner, another process's lease":      socketView(t, p, lockProc, lockLease(lockFin, lockOther, lockNow.Add(time.Hour))),
		"another process of the owner":        socketView(t, p, lockOther, live),
		"owner, lease from another boot": socketView(t, p, lockProc, lockLease(lockFin,
			ProcessToken{PID: lockProc.PID, Version: lockProc.Version, Boot: "0f0e0d0c-0000-4000-8000-000000000001"}, lockNow.Add(time.Hour))),
		"owner, lease on another project only": socketView(t, p, lockProc, lockLease(lockLegal, lockProc, lockNow.Add(time.Hour))),
	}
	// want[view] = {read fin, write fin, list fin, locked fin}
	type reach struct{ read, write, list, locked bool }
	want := map[string]reach{
		"no view (peer_auth off)":              {true, true, true, false},
		"owner over TCP, handed a live lease":  {false, false, true, true},
		"owner on the socket, no process":      {false, false, true, true},
		"owner on the socket, locked":          {false, false, true, true},
		"owner on the socket, leased":          {true, true, true, false},
		"owner, lease ended an instant ago":    {false, false, true, true},
		"owner, lease ends exactly now":        {false, false, true, true},
		"owner, another process's lease":       {false, false, true, true},
		"another process of the owner":         {false, false, true, true},
		"owner, lease from another boot":       {false, false, true, true},
		"owner, lease on another project only": {false, false, true, true},
	}
	require.Len(t, want, len(views))
	for name, v := range views {
		t.Run(name, func(t *testing.T) {
			w := want[name]
			require.Equal(t, w.read, v.Visible(lockFin), "read the sealed project")
			require.Equal(t, w.write, v.Writable(lockFin), "write the sealed project")
			require.Equal(t, w.list, v.Listed(lockFin), "list the sealed project")
			require.Equal(t, w.locked, v.Locked(lockFin), "locked")
			// Whatever the lease: the owner's open project and the global scope
			// stay the owner's, for reads and writes.
			for _, open := range []string{lockOps, "", "brand-new"} {
				require.True(t, v.Visible(open), "read %q", open)
				require.True(t, v.Writable(open), "write %q", open)
				require.True(t, v.Listed(open), "list %q", open)
				require.False(t, v.Locked(open), "locked %q", open)
			}
			// A lease on arctop-finance never opens arctop-legal.
			if v.who.Kind == KindOwner && name != "owner, lease on another project only" {
				require.False(t, v.Visible(lockLegal), "a lease opens its own project only")
				require.True(t, v.Locked(lockLegal))
			}
			// The context helpers agree with the view, which is what the store
			// fence reads.
			ctx := context.Background()
			if name != "no view (peer_auth off)" {
				ctx = WithView(ctx, v)
			}
			require.Equal(t, w.read, Visible(ctx, lockFin))
			require.Equal(t, w.write, Writable(ctx, lockFin))
		})
	}
}

func TestLockedView_HiddenRestrictedLeases(t *testing.T) {
	p := lockPolicy(t)
	locked := socketView(t, p, lockProc)
	require.Equal(t, []string{lockFin, lockLegal}, locked.Hidden(), "every locked project is hidden content")
	require.True(t, locked.Restricted())
	require.Empty(t, locked.Leases())

	leased := socketView(t, p, lockProc,
		lockLease(lockFin, lockProc, lockNow.Add(time.Hour)),
		lockLease(lockFin, lockProc, lockNow.Add(2*time.Hour)), // the later of two wins
		lockLease(lockOps, lockProc, lockNow.Add(time.Hour)),   // not sealed: dropped
		lockLease("", lockProc, lockNow.Add(time.Hour)),        // global: dropped
	)
	require.Equal(t, []string{lockLegal}, leased.Hidden(), "a leased project is no longer hidden")
	l, ok := leased.Lease(lockFin)
	require.True(t, ok)
	require.Equal(t, lockNow.Add(2*time.Hour), l.Expires)
	require.Len(t, leased.Leases(), 1, "only leases on sealed projects are kept")
	_, ok = leased.Lease(lockOps)
	require.False(t, ok)

	both := socketView(t, p, lockProc,
		lockLease(lockFin, lockProc, lockNow.Add(time.Hour)), lockLease(lockLegal, lockProc, lockNow.Add(time.Hour)))
	require.Empty(t, both.Hidden())
	require.False(t, both.Restricted(), "an owner holding every lease is refused nothing")

	// WithLeases replaces, never accumulates: a later request's view is judged
	// on its own leases.
	again := leased.WithLeases(lockNow, nil)
	require.Equal(t, []string{lockFin, lockLegal}, again.Hidden())
}

func TestLockedView_RefusalAndSessionScope(t *testing.T) {
	p := lockPolicy(t)
	tcp := p.OwnerView()
	sock := socketView(t, p, lockProc)

	err := tcp.Refusal(lockFin)
	require.ErrorIs(t, err, ErrNotReachable)
	require.Contains(t, err.Error(), "bearer key never reach")
	require.Contains(t, err.Error(), lockFin)

	err = sock.Refusal(lockFin)
	require.ErrorIs(t, err, ErrNotReachable)
	require.Contains(t, err.Error(), "is locked")
	require.Contains(t, err.Error(), "passkey")
	require.NotContains(t, err.Error(), "sudo -u", "the old design's remedy does not apply")

	_, err = sock.SessionScope(lockFin)
	require.ErrorIs(t, err, ErrNotReachable, "a locked project takes no session (fail closed until the owner decides)")
	got, err := socketView(t, p, lockProc, lockLease(lockFin, lockProc, lockNow.Add(time.Hour))).SessionScope(lockFin)
	require.NoError(t, err)
	require.Equal(t, lockFin, got, "a leased session lands in its project")
	got, err = sock.SessionScope(lockOps)
	require.NoError(t, err)
	require.Equal(t, lockOps, got)
}

func TestLockedView_LegacyUsersUnchanged(t *testing.T) {
	// A users-map policy (the 2026-10-31 design) never grows a lock: leases
	// are ignored, and its sealed projects stay unlisted for the owner.
	p := testPolicy(t)
	v, ok := p.PeerProcessView(501, lockProc)
	require.True(t, ok)
	v = v.WithLeases(lockNow, []Lease{lockLease(lockFin, lockProc, lockNow.Add(time.Hour))})
	require.False(t, v.Visible(lockFin))
	require.False(t, v.Listed(lockFin), "sealed to another account: not even listed")
	require.False(t, v.Locked(lockFin))
	require.Empty(t, v.Leases())
	require.Equal(t, []string{"arctop-finance", "arctop-legal"}, v.Hidden())
	require.Contains(t, v.Refusal(lockFin).Error(), "sudo -u")

	fin, ok := p.PeerProcessView(470, lockProc)
	require.True(t, ok)
	fin = fin.WithLeases(lockNow, []Lease{lockLease(lockLegal, lockProc, lockNow.Add(time.Hour))})
	require.False(t, fin.Visible(lockLegal), "a configured account never takes a lease")
	require.False(t, fin.Listed(lockLegal))
	require.True(t, fin.Listed(lockFin))
}

func TestProcessToken(t *testing.T) {
	for _, tc := range []struct {
		name string
		tok  ProcessToken
		ok   bool
	}{
		{"a real token", lockProc, true},
		{"pid zero", ProcessToken{PID: 0, Version: 1, Boot: "b"}, false},
		{"negative pid", ProcessToken{PID: -1, Version: 1, Boot: "b"}, false},
		{"no boot", ProcessToken{PID: 1, Version: 1}, false},
		{"a separator in the boot", ProcessToken{PID: 1, Version: 1, Boot: "a:b"}, false},
		{"an over-long boot", ProcessToken{PID: 1, Version: 1, Boot: strings.Repeat("a", 65)}, false},
		{"version zero is a version", ProcessToken{PID: 1, Version: 0, Boot: "b"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.ok, tc.tok.Valid())
			if tc.ok {
				require.NotEmpty(t, tc.tok.Key())
			} else {
				require.Empty(t, tc.tok.Key())
			}
		})
	}
	require.NotEqual(t, lockProc.Key(), lockOther.Key())
	require.NotEqual(t, lockProc.Key(), ProcessToken{PID: lockProc.PID, Version: lockProc.Version + 1, Boot: lockProc.Boot}.Key(),
		"a reused pid is another process")

	p := lockPolicy(t)
	v, ok := p.PeerProcessView(501, ProcessToken{})
	require.True(t, ok)
	require.False(t, v.Principal().HasProcess, "an invalid token is not attached")
	require.Empty(t, v.WithLeases(lockNow, []Lease{{Project: lockFin, Expires: lockNow.Add(time.Hour)}}).Leases(),
		"no token, no lease -- not even one whose process is blank")
	_, ok = p.PeerProcessView(9999, lockProc)
	require.False(t, ok, "an unknown uid has no view, token or not")
}

func TestLockedPolicy_Listings(t *testing.T) {
	p := lockPolicy(t)
	require.Equal(t, []string{lockFin, lockLegal}, p.OwnerSealed())
	require.Equal(t, []string{lockFin, lockLegal}, p.SealedProjects(), "the background surfaces treat them as sealed")
	require.Equal(t, []string{lockFin, lockLegal}, p.Projects(), "serve checks they exist and are fenced")
	require.Empty(t, p.Accounts())

	legacy := testPolicy(t)
	require.Empty(t, legacy.OwnerSealed())
	require.Equal(t, []string{"arctop-finance", "arctop-legal"}, legacy.SealedProjects())
}

func TestLockedPolicy_Refusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  config.PeerAuth
		want string
	}{
		{"both designs at once", config.PeerAuth{Owner: "katata", Sealed: []string{"x"},
			Users: map[string]config.PeerUser{"_arctop_legal": {Sealed: []string{"y"}}}}, "both sealed and users"},
		{"a slug twice", config.PeerAuth{Owner: "katata", Sealed: []string{"x", " x"}}, "distinct"},
		{"a blank slug", config.PeerAuth{Owner: "katata", Sealed: []string{" "}}, "distinct"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.cfg, testResolve)
			require.ErrorIs(t, err, ErrInvalidPolicy)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

// A config written for 6afda40 (the users map, no sealed list) loads and
// resolves to exactly its old meaning: the owner never reaches the account's
// seal -- not listed, not locked, no lease taken -- and the account reaches its
// own project. The passkey design does not leak into it.
func TestLegacyUsersConfig_KeepsItsMeaning(t *testing.T) {
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "SEAMLESS_") {
			t.Setenv(k, "")
			require.NoError(t, os.Unsetenv(k))
		}
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	path := filepath.Join(t.TempDir(), "seamless.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
data_dir: /opt/seamless/data
peer_auth:
  enabled: true
  owner: katata
  socket:
    path: /opt/seamless/run/seamlessd.sock
  users:
    _arctop_finance:
      sealed: [arctop-finance]
    _arctop_hq:
      projects: [arctop-hq, arctop-ops]
`), 0o600))
	cfg, err := config.LoadFrom(path)
	require.NoError(t, err)
	p, err := New(cfg.PeerAuth, testResolve)
	require.NoError(t, err)

	require.Empty(t, p.OwnerSealed())
	require.Equal(t, []string{lockFin}, p.SealedProjects())
	for name, v := range map[string]View{
		"owner over TCP":                      p.OwnerView().WithLeases(lockNow, []Lease{lockLease(lockFin, lockProc, lockNow.Add(time.Hour))}),
		"owner on the socket, handed a lease": socketView(t, p, lockProc, lockLease(lockFin, lockProc, lockNow.Add(time.Hour))),
	} {
		t.Run(name, func(t *testing.T) {
			require.False(t, v.Visible(lockFin), "the owner never reaches a project sealed to an account")
			require.False(t, v.Writable(lockFin))
			require.False(t, v.Listed(lockFin), "nor sees its name")
			require.False(t, v.Locked(lockFin), "it is sealed to an account, not locked behind the passkey")
			require.Empty(t, v.Leases())
			require.True(t, v.Visible(lockOps))
			require.Contains(t, v.Refusal(lockFin).Error(), "sudo -u", "the old design's remedy")
		})
	}
	fin, ok := p.PeerView(470)
	require.True(t, ok)
	require.True(t, fin.Visible(lockFin))
	require.True(t, fin.Writable(lockFin))
	require.False(t, fin.Writable(""), "the account still never writes global")
	require.Equal(t, lockFin, fin.Home())
}
