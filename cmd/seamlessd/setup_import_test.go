package main

// setup's import step. Every backup here is written the way seamlessd export
// writes one (archive.Export, over a scratch instance) and restored into a
// data dir under t.TempDir() that the run's injected config names: no test
// loads the machine's config, reads its home, or dials a daemon (the config's
// address is 127.0.0.1:0, and setupTestRun's daemonUp says no).

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/archive"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// impSpec is the scratch instance impBackup backs up.
type impSpec struct {
	// locked adds a sealed project the console locks (example-sealed), a
	// confidential one the exporting config force locks (example-legal), and
	// two passkeys under relying party localhost: a security key, which
	// travels, and a laptop's built-in one, which does not.
	locked bool
}

// The backup's corpus, and the machine it comes from.
const (
	impMemory  = "memory/example-ops/ops-runbook.md"
	impNote    = "notes/example-ops/ops-design.md"
	impSrcHost = "old-host"
)

var impFiles = map[string]string{
	impMemory: "---\nid: 01IMPMEMORY\nkind: runbook\nname: ops-runbook\n---\nrestart the queue before the workers\n",
	impNote:   "---\nid: 01IMPNOTE\nslug: ops-design\ntitle: Ops design\n---\nwhy the queue restarts first\n",
}

// impLocked is what a locked backup locks: example-legal by the exporting
// config's force lock, example-sealed from the console.
var impLocked = []string{"example-legal", "example-sealed"}

// impBackup writes a backup of a scratch instance as seamlessd export writes
// one, and returns its path.
func impBackup(t *testing.T, spec impSpec) string {
	t.Helper()
	ctx := context.Background()
	src := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(src, "seam.db"))
	require.NoError(t, err)
	_, err = store.EnsureProject(ctx, db, "example-ops", "Example Ops")
	require.NoError(t, err)
	var force []string
	if spec.locked {
		for slug, fence := range map[string]core.Isolation{"example-sealed": core.IsolationSealed, "example-legal": core.IsolationConfidential} {
			_, err = store.EnsureProject(ctx, db, slug, slug)
			require.NoError(t, err)
			require.NoError(t, store.SetProjectIsolation(ctx, db, slug, fence))
		}
		require.NoError(t, store.AddConsoleLock(ctx, db, "example-sealed", "cred-old", time.Now()))
		force = []string{"example-legal"}
		now := time.Now()
		for _, c := range []store.ApproverCredential{
			{Kind: store.ApproverPasskey, CredentialID: []byte("cred-yubikey"), PublicKey: []byte("yubikey-public"), Alg: -7,
				Name: "yubikey", Attachment: "cross-platform", RPID: "localhost"},
			{Kind: store.ApproverPasskey, CredentialID: []byte("cred-laptop"), PublicKey: []byte("laptop-public"), Alg: -7,
				Name: "laptop", Attachment: "platform", RPID: "localhost"},
		} {
			code, _, err := store.MintEnrollCode(ctx, db, store.ApproverPasskey, store.ApproverKeyFingerprint(c.PublicKey), now)
			require.NoError(t, err)
			_, err = store.EnrollApprover(ctx, db, c, code, now)
			require.NoError(t, err)
		}
	}
	require.NoError(t, db.Close())
	for rel, body := range impFiles {
		path := filepath.Join(src, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	}

	out := filepath.Join(t.TempDir(), "seamless-"+impSrcHost+"-backup.tar.gz")
	f, err := os.Create(out)
	require.NoError(t, err)
	_, err = archive.Export(ctx, archive.ExportOptions{DataDir: src, Out: f, Host: impSrcHost, SeamlessdVersion: "0.8.4",
		ForceLocked: force, PasskeyRPID: "localhost"})
	require.NoError(t, err)
	require.NoError(t, f.Close())
	return out
}

// impState is what a data dir holds of a backup's locks: its console locks,
// the names of its passkeys, and the fence of each project impLocked names.
type impState struct {
	locks    []string
	passkeys []string
	fences   map[string]core.Isolation
}

func impStateOf(t *testing.T, dir string) impState {
	t.Helper()
	ctx := context.Background()
	db, err := store.OpenExisting(filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	facts, err := store.ReadLockFacts(ctx, db)
	require.NoError(t, err)
	st := impState{fences: map[string]core.Isolation{}}
	if len(facts.ConsoleLocked) > 0 {
		st.locks = facts.ConsoleLocked
	}
	for _, p := range facts.Passkeys {
		st.passkeys = append(st.passkeys, p.Name)
	}
	slices.Sort(st.passkeys)
	for _, slug := range impLocked {
		if fence, err := store.IsolationOf(ctx, db, slug); err == nil {
			st.fences[slug] = fence
		}
	}
	return st
}

// impCopy copies the file at src to dst.
func impCopy(t *testing.T, src, dst string) {
	t.Helper()
	raw, err := os.ReadFile(src)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(dst), 0o700))
	require.NoError(t, os.WriteFile(dst, raw, 0o600))
}

// impRestored holds dir to a fresh restore of the backup's corpus: every file
// byte for byte, and a database behind them.
func impRestored(t *testing.T, dir string) {
	t.Helper()
	require.FileExists(t, filepath.Join(dir, "seam.db"))
	for rel, body := range impFiles {
		got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		require.NoError(t, err)
		require.Equal(t, body, string(got), rel)
	}
}

// impLockEvents counts the audit events a lock change recorded for slug.
func impLockEvents(t *testing.T, dir, slug string) int {
	t.Helper()
	db, err := store.OpenExisting(filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM events WHERE kind = ? AND project_slug = ?`,
		string(core.EventConsoleLockChanged), slug).Scan(&n))
	return n
}

// setupImportTestRun is a first install's run on this OS whose import lands
// in dataDir: its config is the one seamlessd import would load there on a
// first install (peer_auth off), at an address no daemon answers.
func setupImportTestRun(t *testing.T, environ []string, interactive bool, input, dataDir string) (*setupRun, *strings.Builder) {
	t.Helper()
	r, out, _ := setupTestRun(t, runtime.GOOS, environ, setupFactsFresh, interactive, input)
	cfg := config.Defaults()
	cfg.DataDir, cfg.Addr = dataDir, "127.0.0.1:0"
	r.loadConfig = func() (config.Config, error) { return cfg, nil }
	r.home = t.TempDir()
	return r, out
}

// setupImportFirstHalf runs setup's first half under the installer (--state),
// and returns the state it wrote.
func setupImportFirstHalf(t *testing.T, r *setupRun) (setupState, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "setup.state")
	if err := setupForInstaller(r, path); err != nil {
		require.NoFileExists(t, path, "a stopped setup writes no state")
		return setupState{}, err
	}
	return readSetupState(path)
}

func TestSetupImport_Fresh(t *testing.T) {
	for _, tt := range []struct {
		name, input string
	}{
		{"Enter starts fresh", "1\n\n"},
		{"so does the word", "1\nfresh\n"},
		{"so does 1", "1\n1\n"},
		{"and the end of input", "1\n"},
	} {
		t.Run("at the terminal: "+tt.name, func(t *testing.T) {
			dest := filepath.Join(t.TempDir(), "data")
			r, out := setupImportTestRun(t, nil, true, tt.input, dest)
			st, err := setupImportFirstHalf(t, r)
			require.NoError(t, err, out.String())
			require.Equal(t, setupAsked, st.answer)
			require.Contains(t, out.String(), "Start fresh, or from a backup?")
			require.Contains(t, out.String(), "[1] Fresh")
			require.NotContains(t, out.String(), "Backup file:")
			require.NoDirExists(t, dest, "starting fresh writes nothing: the daemon makes its own database")
		})
	}

	t.Run("no terminal and no SEAMLESS_IMPORT: nothing asked and nothing said", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "data")
		r, out := setupImportTestRun(t, []string{"SEAMLESS_SETUP=standalone"}, false, "", dest)
		_, err := setupImportFirstHalf(t, r)
		require.NoError(t, err)
		require.Equal(t, "  setup        Standalone (SEAMLESS_SETUP)\n", out.String(), "the import step adds nothing to a run that sets no import")
		require.NoDirExists(t, dest)
	})

	t.Run("a terminal and an update's knobs: nothing asked", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "data")
		r, out := setupImportTestRun(t, []string{"SEAMLESS_VERSION=0.9.0", "SEAMLESS_INSTALL_DIR=/h/.local/bin"}, true, "", dest)
		r.in = bufio.NewReader(setupNoRead{t})
		st, err := setupImportFirstHalf(t, r)
		require.NoError(t, err)
		require.Equal(t, setupDefault, st.answer)
		require.Equal(t, "  setup        Standalone\n", out.String())
		require.NoDirExists(t, dest)
	})

	t.Run("a Client is never offered an import", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("Windows asks the Client branch without the Server question")
		}
		dest := filepath.Join(t.TempDir(), "data")
		r, out := setupImportTestRun(t, nil, true, "2\n2\nhttps://box.example:8081\nk\n", dest)
		st, err := setupImportFirstHalf(t, r)
		require.NoError(t, err, out.String())
		require.Equal(t, setupClient, st.kind)
		require.NotContains(t, out.String(), "Start fresh, or from a backup?")
		require.NoDirExists(t, dest)
	})
}

func TestSetupImport_PlainBackup(t *testing.T) {
	backup := impBackup(t, impSpec{})

	t.Run("SEAMLESS_IMPORT, no terminal", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "data")
		r, out := setupImportTestRun(t, []string{"SEAMLESS_IMPORT=" + backup}, false, "", dest)
		st, err := setupImportFirstHalf(t, r)
		require.NoError(t, err, out.String())
		require.Equal(t, setupState{kind: setupStandalone, role: config.RoleServer, installed: setupNone, answer: setupFromEnv}, st)
		impRestored(t, dest)
		text := out.String()
		require.Contains(t, text, "  setup        Standalone (SEAMLESS_IMPORT)\n")
		require.Contains(t, text, "  import       "+tildePath(backup)+"\n")
		require.Contains(t, text, "from "+impSrcHost+", ")
		require.Contains(t, text, ": 1 memory, 1 note\n")
		require.Contains(t, text, "  imported     1 memory, 1 note -> "+tildePath(dest)+"\n")
		require.NotContains(t, text, "locked")
		require.Empty(t, impStateOf(t, dest).locks)
	})

	t.Run("SEAMLESS_IMPORT and SEAMLESS_SETUP=server", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("Windows is never the Server")
		}
		dest := filepath.Join(t.TempDir(), "data")
		cert, key := setupAddrPair(t, setupAddrDomain)
		r, out := setupImportTestRun(t, []string{"SEAMLESS_SETUP=server", "SEAMLESS_IMPORT=" + backup, "SEAMLESS_SERVER_NAME=" + setupAddrDomain,
			"SEAMLESS_TLS_CERT=" + cert, "SEAMLESS_TLS_KEY=" + key}, false, "", dest)
		home := r.home
		w := setupAddrArrange(t, r, map[string][]string{setupAddrDomain: {setupAddrLANv4}}, setupAddrIfaces, tailscaleStatus{}, errNoTailscale)
		r.home = home
		st, err := setupImportFirstHalf(t, r)
		require.NoError(t, err, out.String())
		require.Equal(t, setupServer, st.kind)
		require.Contains(t, out.String(), "Networked: Server (SEAMLESS_SETUP)")
		impRestored(t, dest)
		require.Len(t, w.wrote, 1, "the Server's config, written after the import")
	})

	t.Run("a ~ path", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "data")
		r, out := setupImportTestRun(t, nil, false, "", dest)
		impCopy(t, backup, filepath.Join(r.home, "backup.tar.gz"))
		r.environ = []string{"SEAMLESS_IMPORT=~/backup.tar.gz"}
		_, err := setupImportFirstHalf(t, r)
		require.NoError(t, err, out.String())
		impRestored(t, dest)
	})

	t.Run("at the terminal: Backup, then its path, asked again until it is one", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "data")
		notBackup := filepath.Join(t.TempDir(), "notes.txt")
		require.NoError(t, os.WriteFile(notBackup, []byte("not a backup"), 0o600))
		input := "1\n2\n\nrelative.tar.gz\n" + filepath.Join(t.TempDir(), "missing.tar.gz") + "\n" + t.TempDir() + "\n" + notBackup + "\n" + backup + "\n"
		r, out := setupImportTestRun(t, nil, true, input, dest)
		st, err := setupImportFirstHalf(t, r)
		require.NoError(t, err, out.String())
		require.Equal(t, setupAsked, st.answer)
		text := out.String()
		require.Contains(t, text, "[2] Backup")
		require.Contains(t, text, "not a full path")
		require.Contains(t, text, "there is no file at")
		require.Contains(t, text, "that is a folder; a backup is one file")
		require.Contains(t, text, "that is not a Seamless backup")
		require.Equal(t, 6, strings.Count(text, "Backup file: "), "an empty answer and each wrong one are asked again")
		impRestored(t, dest)
	})

	t.Run("at the terminal: a dropped path, quoted or escaped", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("Windows paths keep their backslashes")
		}
		spaced := filepath.Join(t.TempDir(), "my backups", "old one.tar.gz")
		impCopy(t, backup, spaced)
		for _, dropped := range []string{"'" + spaced + "'", strings.ReplaceAll(spaced, " ", `\ `)} {
			dest := filepath.Join(t.TempDir(), "data")
			r, out := setupImportTestRun(t, nil, true, "1\n2\n"+dropped+"\n", dest)
			_, err := setupImportFirstHalf(t, r)
			require.NoError(t, err, out.String())
			impRestored(t, dest)
		}
	})

	t.Run("at the terminal: no path at the end of input stops", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "data")
		r, out := setupImportTestRun(t, nil, true, "1\n2\n", dest)
		_, err := setupImportFirstHalf(t, r)
		require.ErrorIs(t, err, errSetupRefused)
		require.ErrorContains(t, err, "no backup: set SEAMLESS_IMPORT to the backup's path for a run without questions")
		require.Contains(t, out.String(), "Backup file: ")
		require.NoDirExists(t, dest)
	})
}

// TestSetupImport_LockedBackup holds a backup with locks and passkeys to the
// lock gate under each answer: kept by default only where a passkey can open
// them, removed only when the owner says so, and nothing written when the
// owner stops, or when no one can be asked and nothing answered.
func TestSetupImport_LockedBackup(t *testing.T) {
	backup := impBackup(t, impSpec{locked: true})
	passkeys := []string{"laptop", "yubikey"}
	fences := map[string]core.Isolation{"example-legal": core.IsolationConfidential, "example-sealed": core.IsolationSealed}

	kept := func(t *testing.T, dest, text string) {
		t.Helper()
		impRestored(t, dest)
		require.Equal(t, impState{locks: impLocked, passkeys: passkeys, fences: fences}, impStateOf(t, dest),
			"the locks, the fences and the passkeys come back as the backup holds them")
		require.Contains(t, text, "               locked: example-legal, example-sealed\n")
		require.Contains(t, text, "restored 2 approver credential(s)")
		require.Contains(t, text, "  locks        kept on example-legal, example-sealed\n")
		if setupPeerAuthOS(runtime.GOOS) {
			require.Contains(t, text, "nothing enforces a lock until locked projects (peer_auth) are on")
		} else {
			require.Contains(t, text, setupWindowsLocks)
		}
	}
	removed := func(t *testing.T, dest, text string) {
		t.Helper()
		impRestored(t, dest)
		require.Equal(t, impState{passkeys: passkeys, fences: fences}, impStateOf(t, dest),
			"removing the locks keeps the fences, and a restore brings the passkeys back")
		require.Contains(t, text, "               locks removed: example-legal, example-sealed\n")
		require.NotContains(t, text, "  locks        kept")
		for _, slug := range impLocked {
			require.Positive(t, impLockEvents(t, dest, slug), "a removed lock leaves an audit event: %s", slug)
		}
	}
	stopped := func(t *testing.T, dest string, err error) {
		t.Helper()
		require.ErrorIs(t, err, errSetupRefused)
		require.NoDirExists(t, dest, "nothing was written")
	}

	t.Run("no terminal and no answer: stopped, as seamlessd import stops", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "data")
		r, out := setupImportTestRun(t, []string{"SEAMLESS_IMPORT=" + backup}, false, "", dest)
		_, err := setupImportFirstHalf(t, r)
		stopped(t, dest, err)
		require.ErrorContains(t, err, "seamlessd setup: SEAMLESS_IMPORT: this backup holds 2 locked projects (example-legal, example-sealed) "+
			"and no passkey can open them here: peer_auth is off in this config")
		require.ErrorContains(t, err, "Nothing was changed. Set SEAMLESS_IMPORT_LOCKS=keep to import them locked, "+
			"or SEAMLESS_IMPORT_LOCKS=remove to import them without their locks")
		require.NotContains(t, err.Error(), "--keep-locks")
		require.Contains(t, out.String(), "This backup holds 2 locked projects: example-legal, example-sealed.")
	})
	t.Run("SEAMLESS_IMPORT_LOCKS=keep", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "data")
		r, out := setupImportTestRun(t, []string{"SEAMLESS_IMPORT=" + backup, "SEAMLESS_IMPORT_LOCKS=keep"}, false, "", dest)
		_, err := setupImportFirstHalf(t, r)
		require.NoError(t, err, out.String())
		kept(t, dest, out.String())
	})
	t.Run("SEAMLESS_IMPORT_LOCKS=remove", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "data")
		r, out := setupImportTestRun(t, []string{"SEAMLESS_IMPORT=" + backup, "SEAMLESS_IMPORT_LOCKS=remove"}, false, "", dest)
		_, err := setupImportFirstHalf(t, r)
		require.NoError(t, err, out.String())
		removed(t, dest, out.String())
	})

	// At the terminal, the gate's own menu (peer_auth is off on every first
	// install): 1 stops, 2 keeps, 3 removes; Enter and the end of input stop.
	for _, tt := range []struct {
		name, answer string
		want         string // kept, removed, stopped
	}{
		{"1 stops", "1\n", "stopped"},
		{"Enter stops", "\n", "stopped"},
		{"the end of input stops", "", "stopped"},
		{"2 keeps", "2\n", "kept"},
		{"3 removes", "3\n", "removed"},
		{"a wrong answer is asked again", "4\n3\n", "removed"},
	} {
		t.Run("at the terminal: "+tt.name, func(t *testing.T) {
			dest := filepath.Join(t.TempDir(), "data")
			r, out := setupImportTestRun(t, nil, true, "1\n2\n"+backup+"\n"+tt.answer, dest)
			_, err := setupImportFirstHalf(t, r)
			text := out.String()
			require.Contains(t, text, "What do you want to do?")
			require.Contains(t, text, "Stop, so I can turn on peer_auth first.")
			switch tt.want {
			case "kept":
				require.NoError(t, err, text)
				kept(t, dest, text)
			case "removed":
				require.NoError(t, err, text)
				removed(t, dest, text)
			default:
				stopped(t, dest, err)
				require.ErrorContains(t, err, "the import stopped at the backup's locked projects, and nothing was installed")
				if setupPeerAuthOS(runtime.GOOS) {
					require.ErrorContains(t, err, "install with SEAMLESS_IMPORT="+backup+", SEAMLESS_IMPORT_LOCKS=keep and SEAMLESS_NO_SERVICE=1, turn on locked projects")
				}
			}
		})
	}
}

// TestSetupImport_UnattendedIsSeamlessdImportWithoutATerminal: with no one to
// ask, setup's import and seamlessd import --from decide alike -- a backup
// without locks is restored, locks no passkey can open stop both with nothing
// changed, and locks a passkey on record can open are kept by both -- and
// land the same locks.
func TestSetupImport_UnattendedIsSeamlessdImportWithoutATerminal(t *testing.T) {
	plain, locked := impBackup(t, impSpec{}), impBackup(t, impSpec{locked: true})
	// seamlessd import's run here: no terminal (a test's stdin is none), so its
	// gate cannot ask either.
	command := func(t *testing.T, cfg config.Config, backup string) error {
		t.Helper()
		var err error
		captureStdout(t, func() error {
			err = importArchive(context.Background(), cfg, backup, &importFlags{})
			return nil
		})
		return err
	}
	for _, tt := range []struct {
		name     string
		backup   string
		peerAuth bool
		locks    []string
		refused  string
	}{
		{name: "no locks: both restore it", backup: plain},
		{name: "locks no passkey can open: both stop with nothing changed", backup: locked,
			refused: "this backup holds 2 locked projects (example-legal, example-sealed) and no passkey can open them here: " +
				"peer_auth is off in this config, so locks are not enforced and no passkey can be used. Nothing was changed."},
		{name: "locks a passkey on record can open: both keep them", backup: locked, peerAuth: true, locks: impLocked},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Defaults()
			cfg.Addr = "127.0.0.1:0"
			cfg.PeerAuth.Enabled = tt.peerAuth
			cfg.DataDir = filepath.Join(t.TempDir(), "command")
			cmdErr := command(t, cfg, tt.backup)

			dest := filepath.Join(t.TempDir(), "setup")
			r, out := setupImportTestRun(t, []string{"SEAMLESS_IMPORT=" + tt.backup}, false, "", dest)
			setupCfg := cfg
			setupCfg.DataDir = dest
			r.loadConfig = func() (config.Config, error) { return setupCfg, nil }
			r.host = config.Hostname()
			_, setupErr := setupImportFirstHalf(t, r)

			if tt.refused != "" {
				require.ErrorContains(t, cmdErr, tt.refused)
				require.ErrorContains(t, setupErr, tt.refused)
				require.NoDirExists(t, cfg.DataDir)
				require.NoDirExists(t, dest)
				return
			}
			require.NoError(t, cmdErr)
			require.NoError(t, setupErr, out.String())
			impRestored(t, cfg.DataDir)
			impRestored(t, dest)
			require.Equal(t, tt.locks, impStateOf(t, cfg.DataDir).locks)
			require.Equal(t, impStateOf(t, cfg.DataDir), impStateOf(t, dest), "setup lands what the command lands")
		})
	}
}

// TestSetupImport_BeforePeerAuth is the half of the order with peer_auth this
// build can test. The import is a step of setup's first half and locked
// projects one of the after half, so the import runs as the owner into the
// owner's data dir before locked projects are turned on. Turning them on (the
// peer_auth setup engine's step 6, the guide's "Move an existing install over"
// by hand) renames that data dir to the daemon's account; the locks, fences
// and passkeys a backup brought must come with it, under each answer.
func TestSetupImport_BeforePeerAuth(t *testing.T) {
	require.False(t, setupSteps["import"].after, "the import runs before the installer's own steps")
	require.True(t, setupSteps["locked-projects"].after, "locked projects come after the install")
	for _, kind := range []setupKind{setupStandalone, setupServer} {
		ids := setupPlans[kind]
		require.Contains(t, ids, "import")
		require.Contains(t, ids, "locked-projects")
	}
	require.NotContains(t, setupPlans[setupClient], "import", "a Client never imports: its data is its Server's")

	backup := impBackup(t, impSpec{locked: true})
	for _, tt := range []struct {
		answer string
		want   impState
	}{
		{"keep", impState{locks: impLocked, passkeys: []string{"laptop", "yubikey"},
			fences: map[string]core.Isolation{"example-legal": core.IsolationConfidential, "example-sealed": core.IsolationSealed}}},
		{"remove", impState{passkeys: []string{"laptop", "yubikey"},
			fences: map[string]core.Isolation{"example-legal": core.IsolationConfidential, "example-sealed": core.IsolationSealed}}},
	} {
		t.Run(tt.answer, func(t *testing.T) {
			owner := filepath.Join(t.TempDir(), "owner", ".seamless")
			r, out := setupImportTestRun(t, []string{"SEAMLESS_IMPORT=" + backup, "SEAMLESS_IMPORT_LOCKS=" + tt.answer}, false, "", owner)
			st, err := setupImportFirstHalf(t, r)
			require.NoError(t, err, out.String())
			require.Equal(t, tt.want, impStateOf(t, owner))

			// The after half lists locked projects by hand, once the install is done.
			r.out = &strings.Builder{}
			require.NoError(t, setupHandOff(r, &st, setupStepsLeft(st, r.goos), true))
			if setupPeerAuthOS(r.goos) {
				require.Contains(t, r.out.(*strings.Builder).String(), "peer-auth/#move-an-existing-install-over")
			}

			// The move: one rename of the whole data dir, as the engine and the
			// guide do it. The database holds paths relative to it.
			daemon := filepath.Join(t.TempDir(), "opt", "data")
			require.NoError(t, os.MkdirAll(filepath.Dir(daemon), 0o700))
			require.NoError(t, os.Rename(owner, daemon))
			require.Equal(t, tt.want, impStateOf(t, daemon), "what the import restored moves with the data dir")
			impRestored(t, daemon)
			mode, err := archive.DetectMode(owner)
			require.NoError(t, err)
			require.Equal(t, archive.ModeFresh, mode, "nothing is left behind in the owner's data dir")
		})
	}
	t.Run("stop: nothing to move", func(t *testing.T) {
		owner := filepath.Join(t.TempDir(), "owner", ".seamless")
		r, _ := setupImportTestRun(t, []string{"SEAMLESS_IMPORT=" + backup}, false, "", owner)
		_, err := setupImportFirstHalf(t, r)
		require.ErrorIs(t, err, errSetupRefused)
		require.NoDirExists(t, owner)
	})
}

// TestSetupImport_RefusesWhereItMustNotImport: setup imports only into an
// empty data dir, the one the daemon this install starts reads, with no other
// daemon answering there, and only from a file it can read as a whole backup.
// Each refusal comes before anything is written.
func TestSetupImport_RefusesWhereItMustNotImport(t *testing.T) {
	backup := impBackup(t, impSpec{})
	refused := func(t *testing.T, r *setupRun, dest string, want ...string) {
		t.Helper()
		_, err := setupImportFirstHalf(t, r)
		require.ErrorIs(t, err, errSetupRefused)
		for _, w := range want {
			require.ErrorContains(t, err, w)
		}
	}

	t.Run("a data dir that holds a memory already", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "data")
		stray := filepath.Join(dest, "memory", "_global", "kept.md")
		require.NoError(t, os.MkdirAll(filepath.Dir(stray), 0o700))
		require.NoError(t, os.WriteFile(stray, []byte("---\nid: 01KEPT\n---\nmine\n"), 0o600))
		r, _ := setupImportTestRun(t, []string{"SEAMLESS_IMPORT=" + backup}, false, "", dest)
		refused(t, r, dest, "holds Seamless data already", "setup starts from a backup only in an empty data dir", "mv ")
		got, err := os.ReadFile(stray)
		require.NoError(t, err)
		require.Equal(t, "---\nid: 01KEPT\n---\nmine\n", string(got))
		require.NoFileExists(t, filepath.Join(dest, "seam.db"))
	})
	t.Run("a data dir with only a log and a dot file is empty", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "data")
		require.NoError(t, os.MkdirAll(filepath.Join(dest, "memory"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dest, "seamlessd.log"), []byte("log\n"), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dest, "memory", ".DS_Store"), []byte{0}, 0o600))
		r, out := setupImportTestRun(t, []string{"SEAMLESS_IMPORT=" + backup}, false, "", dest)
		_, err := setupImportFirstHalf(t, r)
		require.NoError(t, err, out.String())
		impRestored(t, dest)
	})
	t.Run("a daemon answering where this one will", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "data")
		r, _ := setupImportTestRun(t, []string{"SEAMLESS_IMPORT=" + backup}, false, "", dest)
		r.daemonUp = func(config.Config) bool { return true }
		refused(t, r, dest, "a Seamless daemon already answers at http://127.0.0.1:0", "seamlessd stop")
		require.NoDirExists(t, dest)
	})
	t.Run("SEAMLESS_DATA_DIR moves only this run's data dir", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "data")
		r, _ := setupImportTestRun(t, []string{"SEAMLESS_IMPORT=" + backup, "SEAMLESS_DATA_DIR=" + dest}, false, "", dest)
		refused(t, r, dest, "SEAMLESS_DATA_DIR=", "the backup would land where it never looks")
		require.NoDirExists(t, dest)
	})
	for name, tt := range map[string]struct {
		file func(t *testing.T) string
		want string
	}{
		"a missing file": {func(t *testing.T) string { return filepath.Join(t.TempDir(), "gone.tar.gz") }, "there is no file at"},
		"a folder":       {func(t *testing.T) string { return t.TempDir() }, "that is a folder; a backup is one file"},
		"not a backup": {func(t *testing.T) string {
			p := filepath.Join(t.TempDir(), "x.tar.gz")
			require.NoError(t, os.WriteFile(p, []byte("not gzip"), 0o600))
			return p
		}, "that is not a Seamless backup"},
		"a backup cut short": {func(t *testing.T) string {
			raw, err := os.ReadFile(backup)
			require.NoError(t, err)
			p := filepath.Join(t.TempDir(), "short.tar.gz")
			require.NoError(t, os.WriteFile(p, raw[:len(raw)*2/3], 0o600))
			return p
		}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			dest := filepath.Join(t.TempDir(), "data")
			file := tt.file(t)
			r, _ := setupImportTestRun(t, []string{"SEAMLESS_IMPORT=" + file}, false, "", dest)
			refused(t, r, dest, "SEAMLESS_IMPORT="+file+": "+tt.want)
			require.NoDirExists(t, dest)
		})
	}
	t.Run("a refused import leaves nothing for the installer: no state", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "setup.state")
		r, _ := setupImportTestRun(t, []string{"SEAMLESS_IMPORT=" + filepath.Join(t.TempDir(), "gone.tar.gz")}, false, "", filepath.Join(t.TempDir(), "data"))
		require.ErrorIs(t, setupForInstaller(r, path), errSetupRefused)
		require.NoFileExists(t, path)
	})
}

// TestSetupImport_TheHandOff: the import runs in setup's first half (--state),
// before the installer's own steps, and never again in --finish, which names
// what is left by hand and no import among it.
func TestSetupImport_TheHandOff(t *testing.T) {
	backup := impBackup(t, impSpec{})
	dest := filepath.Join(t.TempDir(), "data")
	state := filepath.Join(t.TempDir(), "setup.state")
	r, out := setupImportTestRun(t, []string{"SEAMLESS_IMPORT=" + backup}, false, "", dest)
	require.NoError(t, setupForInstaller(r, state))
	require.NotContains(t, out.String(), "by hand", "the steps by hand come after the installer's own")
	impRestored(t, dest)
	before, err := os.ReadFile(filepath.Join(dest, "seam.db"))
	require.NoError(t, err)

	r, out = setupImportTestRun(t, []string{"SEAMLESS_IMPORT=" + backup}, false, "", dest)
	r.loadConfig = func() (config.Config, error) {
		t.Fatal("--finish imported again")
		return config.Config{}, nil
	}
	require.NoError(t, setupFinish(r, state))
	text := out.String()
	require.NotContains(t, text, "import")
	if setupPeerAuthOS(runtime.GOOS) {
		require.Contains(t, text, "setup does not do this Standalone step yet:")
		require.Contains(t, text, "locked")
	} else {
		require.Empty(t, text, "on Windows nothing is left by hand")
	}
	after, err := os.ReadFile(filepath.Join(dest, "seam.db"))
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func TestSetupImportPath(t *testing.T) {
	r, _, _ := setupTestRun(t, runtime.GOOS, nil, setupFactsFresh, false, "")
	r.home = t.TempDir()
	full := filepath.Join(r.home, "b.tar.gz") // a full path on every OS
	for raw, want := range map[string]string{
		full:              full,
		"~/b.tar.gz":      full,
		"~":               r.home,
		"b.tar.gz":        "",
		"./b.tar.gz":      "",
		"~other/b.tar.gz": "",
		"-":               "",
		full + "\n":       "",
	} {
		got, problem := setupImportPath(r, raw)
		require.Equal(t, want, got, raw)
		require.Equal(t, want == "", problem != "", "%q: %s", raw, problem)
	}
	r.home = ""
	_, problem := setupImportPath(r, "~/b.tar.gz")
	require.Contains(t, problem, "no home directory")

	for p, want := range map[string]bool{
		`C:\b.tar.gz`: true, `c:/b.tar.gz`: true, `\\nas\share\b.tar.gz`: true,
		`C:b.tar.gz`: false, `\b.tar.gz`: false, `b.tar.gz`: false, `1:\b`: false, "/b.tar.gz": false,
	} {
		require.Equal(t, want, setupFullPath("windows", p), p)
	}
	require.True(t, setupFullPath("darwin", "/b"))
	require.False(t, setupFullPath("linux", `C:\b`))

	for in, want := range map[string]string{
		`'/a b/c.tar.gz'`:   "/a b/c.tar.gz",
		`"/a b/c.tar.gz"`:   "/a b/c.tar.gz",
		`/a\ b/c\ d.tar.gz`: "/a b/c d.tar.gz",
		`/plain.tar.gz`:     "/plain.tar.gz",
		`'unbalanced`:       `'unbalanced`,
		`/ends\`:            `/ends\`,
	} {
		require.Equal(t, want, setupShellWord("linux", in), in)
	}
	require.Equal(t, `C:\a b\c.tar.gz`, setupShellWord("windows", `"C:\a b\c.tar.gz"`))
	require.Equal(t, `C:\a\b.tar.gz`, setupShellWord("windows", `C:\a\b.tar.gz`))
}

// The manifest's strings are the backup's, so setup's rows drop a control
// character a crafted one carries.
func TestSetupPrintable(t *testing.T) {
	require.Equal(t, "host[31mred", setupPrintable("host\x1b[31mred"))
	require.Equal(t, "plain, words", setupPrintable("plain, words"))
}

// TestLockGate_RefusalNamesTheCallersAnswers: seamlessd import's refusal is
// word for word what it was, and setup's names its knob instead.
func TestLockGate_RefusalNamesTheCallersAnswers(t *testing.T) {
	g, _ := lockGateFor(true, false, "", importFlags{})
	_, err := g.decide(context.Background(), archive.ModeFresh, lockedManifest())
	require.EqualError(t, err, "this backup holds 2 locked projects (acme, secrets) and no passkey can open them here: the backup carries no passkey. "+
		"Nothing was changed. Pass --keep-locks to import them locked and add a passkey right after, or --remove-locks to import them without their locks")

	g, _ = lockGateFor(true, false, "", importFlags{})
	g.answers = setupLockAnswers
	_, err = g.decide(context.Background(), archive.ModeFresh, lockedManifest())
	require.EqualError(t, err, "this backup holds 2 locked projects (acme, secrets) and no passkey can open them here: the backup carries no passkey. "+
		"Nothing was changed. Set SEAMLESS_IMPORT_LOCKS=keep to import them locked, or SEAMLESS_IMPORT_LOCKS=remove to import them without their locks")
}
