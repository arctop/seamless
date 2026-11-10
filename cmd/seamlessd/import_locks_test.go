package main

import (
	"bufio"
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/archive"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

func lockGateFor(peerAuth, interactive bool, input string, flags importFlags) (*lockGate, *bytes.Buffer) {
	var cfg config.Config
	cfg.PeerAuth.Enabled = peerAuth
	out := &bytes.Buffer{}
	return &lockGate{
		cfg: cfg, flags: &flags, interactive: interactive,
		in: bufio.NewReader(strings.NewReader(input)), out: out, host: "here",
		daemonUp: func() bool { return false },
		local:    func(context.Context) ([]store.PasskeyFact, error) { return nil, nil },
	}, out
}

func lockedManifest(passkeys ...store.PasskeyFact) archive.Manifest {
	return archive.Manifest{SourceHost: "here", Locks: &archive.Locks{
		Projects:    []archive.LockedProject{{Slug: "acme"}, {Slug: "secrets"}},
		PasskeyRPID: "localhost", Passkeys: passkeys,
	}}
}

var heldPasskey = store.PasskeyFact{Name: "yubikey", Portable: true}

func TestRestoredPasskeys(t *testing.T) {
	bound := store.PasskeyFact{Name: "laptop"}
	cases := []struct {
		name       string
		locks      archive.Locks
		sourceHost string
		want       []string
		reason     string
	}{
		{name: "none in the backup", sourceHost: "here", reason: "carries no passkey"},
		{name: "another relying party", locks: archive.Locks{PasskeyRPID: "other.test", Passkeys: []store.PasskeyFact{heldPasskey}},
			sourceHost: "here", reason: `belong to "other.test"`},
		{name: "an unrecorded relying party counts", locks: archive.Locks{Passkeys: []store.PasskeyFact{bound}},
			sourceHost: "here", want: []string{"laptop"}},
		{name: "a device-bound passkey on its own machine", locks: archive.Locks{PasskeyRPID: "localhost", Passkeys: []store.PasskeyFact{bound}},
			sourceHost: "here", want: []string{"laptop"}},
		{name: "a device-bound passkey does not travel", locks: archive.Locks{PasskeyRPID: "localhost", Passkeys: []store.PasskeyFact{bound}},
			sourceHost: "old-mac", reason: "tied to"},
		{name: "a portable passkey travels", locks: archive.Locks{PasskeyRPID: "localhost", Passkeys: []store.PasskeyFact{bound, heldPasskey}},
			sourceHost: "old-mac", want: []string{"yubikey"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, reason := restoredPasskeys(c.locks, c.sourceHost, "here", "localhost")
			require.Equal(t, c.want, got)
			if c.reason == "" {
				require.Empty(t, reason)
			} else {
				require.Contains(t, reason, c.reason)
			}
		})
	}
}

func TestLockGate_FlagsAnswerWithoutAsking(t *testing.T) {
	g, _ := lockGateFor(true, false, "", importFlags{removeLocks: true})
	got, err := g.decide(context.Background(), archive.ModeFresh, lockedManifest())
	require.NoError(t, err)
	require.Equal(t, archive.LocksRemove, got)

	g, _ = lockGateFor(true, false, "", importFlags{keepLocks: true})
	got, err = g.decide(context.Background(), archive.ModeFresh, lockedManifest())
	require.NoError(t, err)
	require.Equal(t, archive.LocksKeep, got)
}

func TestLockGate_NoTerminalAndNoPasskeyRefuses(t *testing.T) {
	g, _ := lockGateFor(true, false, "", importFlags{})
	_, err := g.decide(context.Background(), archive.ModeFresh, lockedManifest())
	require.ErrorContains(t, err, "acme, secrets")
	require.ErrorContains(t, err, "--keep-locks")
	require.ErrorContains(t, err, "--remove-locks")
	require.ErrorContains(t, err, "Nothing was changed")
}

func TestLockGate_NoTerminalKeepsWhenAPasskeyIsOnRecord(t *testing.T) {
	g, out := lockGateFor(true, false, "", importFlags{})
	got, err := g.decide(context.Background(), archive.ModeFresh, lockedManifest(heldPasskey))
	require.NoError(t, err)
	require.Equal(t, archive.LocksKeep, got)
	require.Contains(t, out.String(), `"yubikey"`)
}

func TestLockGate_Menu(t *testing.T) {
	cases := []struct {
		name     string
		peerAuth bool
		mode     archive.Mode
		input    string
		want     archive.LockChoice
		errLike  string
		shows    string
	}{
		{name: "fresh defaults to keeping", peerAuth: true, mode: archive.ModeFresh, input: "\n", want: archive.LocksKeep, shows: "add a passkey right after"},
		{name: "fresh removes", peerAuth: true, mode: archive.ModeFresh, input: "2\n", want: archive.LocksRemove},
		{name: "fresh cancels", peerAuth: true, mode: archive.ModeFresh, input: "3\n", errLike: "nothing was changed"},
		{name: "merge defaults to adding one first", peerAuth: true, mode: archive.ModeMerge, input: "\n", errLike: "/console/approvers"},
		{name: "merge keeps", peerAuth: true, mode: archive.ModeMerge, input: "2\n", want: archive.LocksKeep},
		{name: "merge removes", peerAuth: true, mode: archive.ModeMerge, input: "3\n", want: archive.LocksRemove},
		{name: "peer_auth off stops first", mode: archive.ModeFresh, input: "\n", errLike: "turn on peer_auth", shows: "peer_auth is off"},
		{name: "peer_auth off can keep", mode: archive.ModeFresh, input: "2\n", want: archive.LocksKeep, shows: "Locks do nothing until peer_auth is on"},
		{name: "end of input stops", peerAuth: true, mode: archive.ModeFresh, input: "", errLike: "nothing was changed"},
		{name: "a bad answer asks again", peerAuth: true, mode: archive.ModeFresh, input: "9\nnope\n2\n", want: archive.LocksRemove, shows: "not one of the choices"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g, out := lockGateFor(c.peerAuth, true, c.input, importFlags{})
			got, err := g.decide(context.Background(), c.mode, lockedManifest())
			if c.errLike != "" {
				require.ErrorContains(t, err, c.errLike)
			} else {
				require.NoError(t, err)
				require.Equal(t, c.want, got)
			}
			if c.shows != "" {
				require.Contains(t, out.String(), c.shows)
			}
		})
	}
}

func TestLockGate_FreshNeverOffersAddingAPasskeyFirst(t *testing.T) {
	g, out := lockGateFor(true, true, "1\n", importFlags{})
	_, err := g.decide(context.Background(), archive.ModeFresh, lockedManifest())
	require.NoError(t, err)
	require.NotContains(t, out.String(), "add a passkey first",
		"a daemon started to add one would create a database, and turn this restore into a merge")
}

func TestLockGate_AsksWhetherTheOwnerStillHoldsIt(t *testing.T) {
	g, out := lockGateFor(true, true, "y\n", importFlags{})
	got, err := g.decide(context.Background(), archive.ModeFresh, lockedManifest(heldPasskey))
	require.NoError(t, err)
	require.Equal(t, archive.LocksKeep, got)
	require.Contains(t, out.String(), "Do you still have one of them?")
	require.NotContains(t, out.String(), "What do you want to do?")

	// "No" is the case the row cannot see, so it gets the no-passkey menu.
	g, out = lockGateFor(true, true, "n\n2\n", importFlags{})
	got, err = g.decide(context.Background(), archive.ModeFresh, lockedManifest(heldPasskey))
	require.NoError(t, err)
	require.Equal(t, archive.LocksRemove, got)
	require.Contains(t, out.String(), "What do you want to do?")
}

func TestLockGate_MergeReadsThisInstancesPasskeys(t *testing.T) {
	g, out := lockGateFor(true, false, "", importFlags{})
	g.local = func(context.Context) ([]store.PasskeyFact, error) {
		return []store.PasskeyFact{{Name: "here-key"}}, nil
	}
	got, err := g.decide(context.Background(), archive.ModeMerge, lockedManifest(heldPasskey))
	require.NoError(t, err)
	require.Equal(t, archive.LocksKeep, got)
	require.Contains(t, out.String(), `"here-key"`)
	require.NotContains(t, out.String(), "yubikey", "a merge does not bring the backup's passkeys")
}

func TestLockGate_MergeKeepingLocksNeedsTheDaemonDown(t *testing.T) {
	up := func(flags importFlags) (archive.LockChoice, error) {
		g, _ := lockGateFor(true, false, "", flags)
		g.daemonUp = func() bool { return true }
		return g.decide(context.Background(), archive.ModeMerge, lockedManifest())
	}
	_, err := up(importFlags{keepLocks: true})
	require.ErrorIs(t, err, archive.ErrDaemonRunning)
	require.ErrorContains(t, err, "Nothing was changed")

	_, err = up(importFlags{keepLocks: true, force: true})
	require.NoError(t, err)

	got, err := up(importFlags{removeLocks: true})
	require.NoError(t, err, "no lock arrives, so the daemon has nothing to reread")
	require.Equal(t, archive.LocksRemove, got)

	_, err = up(importFlags{dryRun: true})
	require.NoError(t, err)
}

func TestLockGate_DryRunNeverAsks(t *testing.T) {
	g, out := lockGateFor(true, true, "", importFlags{dryRun: true})
	got, err := g.decide(context.Background(), archive.ModeFresh, lockedManifest())
	require.NoError(t, err)
	require.Equal(t, archive.LocksKeep, got)
	require.NotContains(t, out.String(), "What do you want to do?")
	require.Contains(t, out.String(), "A real import will ask")
}

func TestLockGate_Advice(t *testing.T) {
	report := func(mode archive.Mode) *archive.ImportReport {
		return &archive.ImportReport{Mode: mode, Locks: &archive.LockOutcome{Locked: []string{"acme", "secrets"}}}
	}
	advice := func(g *lockGate, mode archive.Mode, m archive.Manifest) string {
		_, err := g.decide(context.Background(), mode, m)
		require.NoError(t, err)
		var b bytes.Buffer
		g.advise(&b, report(mode))
		return b.String()
	}

	g, _ := lockGateFor(true, true, "y\n", importFlags{})
	got := advice(g, archive.ModeFresh, lockedManifest(heldPasskey))
	require.Contains(t, got, "http://localhost:8081/console/unlock/acme", "unlocking one is the real check that the passkey works")

	g, _ = lockGateFor(true, true, "n\n1\n", importFlags{})
	got = advice(g, archive.ModeFresh, lockedManifest(heldPasskey))
	require.Contains(t, got, "/console/approvers", "an owner who said no is told to add one")
	require.NotContains(t, got, "still opens")

	g, _ = lockGateFor(false, false, "", importFlags{keepLocks: true})
	require.Contains(t, advice(g, archive.ModeFresh, lockedManifest()), "Turn it on before you start the daemon")

	g, _ = lockGateFor(true, false, "", importFlags{keepLocks: true, force: true})
	require.Contains(t, advice(g, archive.ModeMerge, lockedManifest()), "Restart the daemon now")

	g, _ = lockGateFor(true, false, "", importFlags{removeLocks: true})
	require.Empty(t, advice(g, archive.ModeFresh, lockedManifest()))
}

func TestImportArchive_LockedBackup(t *testing.T) {
	src := throwawayInstance(t)
	ctx := context.Background()
	db, err := store.OpenExisting(filepath.Join(src, "seam.db"))
	require.NoError(t, err)
	require.NoError(t, store.SetProjectIsolation(ctx, db, "seam", core.IsolationConfidential))
	require.NoError(t, store.AddConsoleLock(ctx, db, "seam", "cred", time.Now()))
	require.NoError(t, db.Close())

	archivePath := filepath.Join(t.TempDir(), "instance.tar.gz")
	out := captureStdout(t, func() error { return runExport([]string{"-o", archivePath}) })
	require.Contains(t, out, "locked: seam")

	locksOf := func(dir string) []string {
		db, err := store.OpenExisting(filepath.Join(dir, "seam.db"))
		require.NoError(t, err)
		defer func() { _ = db.Close() }()
		got, err := store.ConsoleLockedSlugs(ctx, db)
		require.NoError(t, err)
		return got
	}

	// No terminal here, and no passkey can open the project: nothing happens.
	refused := filepath.Join(t.TempDir(), "refused")
	t.Setenv("SEAMLESS_DATA_DIR", refused)
	err = runImport([]string{"--from", archivePath, "-embed=false"})
	require.ErrorContains(t, err, "--remove-locks")
	require.NoDirExists(t, refused)

	kept := filepath.Join(t.TempDir(), "kept")
	t.Setenv("SEAMLESS_DATA_DIR", kept)
	out = captureStdout(t, func() error { return runImport([]string{"--from", archivePath, "-embed=false", "--keep-locks"}) })
	require.Contains(t, out, "locked: seam")
	require.Equal(t, []string{"seam"}, locksOf(kept))

	removed := filepath.Join(t.TempDir(), "removed")
	t.Setenv("SEAMLESS_DATA_DIR", removed)
	out = captureStdout(t, func() error { return runImport([]string{"--from", archivePath, "-embed=false", "--remove-locks"}) })
	require.Contains(t, out, "locks removed: seam")
	require.Empty(t, locksOf(removed))

	require.ErrorContains(t, runImport([]string{"--from", archivePath, "--keep-locks", "--remove-locks"}), "contradict")
}
