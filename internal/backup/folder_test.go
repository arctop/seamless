package backup

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func testFolders(t *testing.T) (folders, string) {
	t.Helper()
	data := t.TempDir()
	for _, d := range []string{"memory/alpha", "notes", "restore/01X"} {
		require.NoError(t, os.MkdirAll(filepath.Join(data, d), 0o700))
	}
	return folders{dataDir: data, goos: runtime.GOOS, freeBytes: func(string) (uint64, error) { return 212 << 30, nil }}, data
}

func TestFolders_InsideTheDataDirIsAnsweredFirst(t *testing.T) {
	f, data := testFolders(t)
	outside := t.TempDir()

	for name, dir := range map[string]string{
		"the data dir itself":            data,
		"an existing folder in it":       filepath.Join(data, "memory", "alpha"),
		"a missing folder in it":         filepath.Join(data, "memory", "beta", "x"),
		"a restore's staging":            filepath.Join(data, "restore", "01X"),
		"below the local backups folder": filepath.Join(data, LocalDirName, "sub"),
		"a dot-dot spelling":             filepath.Join(outside, "..", filepath.Base(outside), "..", filepath.Base(data), "notes"),
	} {
		t.Run(name, func(t *testing.T) {
			c := f.check(dir)
			require.Equal(t, FolderInsideDataDir, c.State)
			require.Zero(t, c.Free, "nothing more is said about such a path")
			require.False(t, c.FreeKnown)
			require.Equal(t, FolderInsideDataDir, f.prepare(dir, true).State)
		})
	}

	t.Run("the local backups folder is allowed", func(t *testing.T) {
		require.Equal(t, FolderWillCreate, f.check(LocalDir(data)).State)
		require.Equal(t, FolderReady, f.prepare(LocalDir(data), true).State)
		require.Equal(t, FolderReady, f.check(LocalDir(data)).State)
	})

	if runtime.GOOS == "windows" {
		return
	}
	t.Run("a symlink into the data dir", func(t *testing.T) {
		link := filepath.Join(outside, "into")
		require.NoError(t, os.Symlink(filepath.Join(data, "memory"), link))
		require.Equal(t, FolderInsideDataDir, f.check(link).State)
		require.Equal(t, FolderInsideDataDir, f.check(filepath.Join(link, "alpha", "missing")).State)
	})
	t.Run("a symlink inside the data dir pointing out", func(t *testing.T) {
		link := filepath.Join(data, "notes", "out")
		require.NoError(t, os.Symlink(outside, link))
		require.Equal(t, FolderInsideDataDir, f.check(link).State, "the answer does not depend on what is inside")
	})
	t.Run("a link inside the data dir leading to its backups folder", func(t *testing.T) {
		require.NoError(t, os.MkdirAll(LocalDir(data), 0o700))
		link := filepath.Join(data, "memory", "to-backups")
		require.NoError(t, os.Symlink(LocalDir(data), link))
		require.Equal(t, FolderInsideDataDir, f.check(link).State, "answered as a missing path there is")
		require.Equal(t, FolderReady, f.check(LocalDir(data)).State)
	})
	t.Run("the data dir's backups folder through the data dir's real path", func(t *testing.T) {
		real, err := filepath.EvalSymlinks(data)
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(LocalDir(data), 0o700))
		require.Equal(t, FolderReady, f.check(filepath.Join(real, LocalDirName)).State)
		link := filepath.Join(outside, "data-link")
		require.NoError(t, os.Symlink(data, link))
		require.Equal(t, FolderReady, f.check(filepath.Join(link, LocalDirName)).State)
		require.Equal(t, FolderInsideDataDir, f.check(filepath.Join(link, "notes")).State)
	})
	t.Run("a case variant on a case-insensitive disk", func(t *testing.T) {
		upper := strings.ToUpper(data)
		if upper == data {
			t.Skip("no letters to vary")
		}
		if _, err := os.Stat(upper); err != nil {
			t.Skip("this disk is case-sensitive")
		}
		require.Equal(t, FolderInsideDataDir, f.check(filepath.Join(upper, "memory")).State)
		require.Equal(t, FolderInsideDataDir, f.check(upper).State)
	})
}

func TestFolders_CheckOnlyLooks(t *testing.T) {
	f, _ := testFolders(t)
	parent := t.TempDir()

	missing := filepath.Join(parent, "a", "Seamless Backups")
	c := f.check(missing)
	require.Equal(t, FolderWillCreate, c.State)
	require.True(t, c.FreeKnown)
	require.EqualValues(t, 212<<30, c.Free)
	require.NoDirExists(t, filepath.Join(parent, "a"), "the check never creates")

	require.Equal(t, FolderReady, f.check(parent).State)
	require.Empty(t, files(t, parent), "and never writes")

	file := filepath.Join(parent, "file")
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	require.Equal(t, FolderNotDirectory, f.check(file).State)
	require.Equal(t, FolderNotDirectory, f.check(filepath.Join(file, "below")).State)

	require.Equal(t, FolderNotAbsolute, f.check("relative/dir").State)
	require.Equal(t, FolderNotAbsolute, f.check("").State)
}

func TestFolders_PrepareMakesAndProbes(t *testing.T) {
	f, _ := testFolders(t)
	dir := filepath.Join(t.TempDir(), "a", "Seamless Backups")
	c := f.prepare(dir, true)
	require.Equal(t, FolderReady, c.State)
	require.DirExists(t, dir)
	require.Empty(t, files(t, dir), "the probe is removed")
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(dir)
		require.NoError(t, err)
		require.Equal(t, fs.FileMode(0o700), fi.Mode().Perm())
	}

	gone := filepath.Join(t.TempDir(), "gone")
	c = f.prepare(gone, false)
	require.Equal(t, FolderMissing, c.State, "a run does not re-create a folder it was saved with")
	require.NoDirExists(t, gone)
	require.Contains(t, c.Message(), "no longer exists")
}

func TestFolders_NotWritable(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX modes that bind this account")
	}
	f, _ := testFolders(t)
	ro := filepath.Join(t.TempDir(), "ro")
	require.NoError(t, os.Mkdir(ro, 0o500))
	t.Cleanup(func() { _ = os.Chmod(ro, 0o700) })

	c := f.check(ro)
	require.Equal(t, FolderNotWritable, c.State)
	require.Equal(t, "folder-not-writable", c.Reason())
	require.Equal(t, ro+" is not writable", c.Message())
	require.Equal(t, FolderNotWritable, f.check(filepath.Join(ro, "Seamless Backups")).State)
	require.Equal(t, FolderNotWritable, f.prepare(filepath.Join(ro, "Seamless Backups"), true).State)
}

func TestClassify_PrivacyIsMacOSsEPERM(t *testing.T) {
	eperm := &fs.PathError{Op: "open", Path: "/x", Err: syscall.EPERM}
	eacces := &fs.PathError{Op: "open", Path: "/x", Err: syscall.EACCES}
	require.Equal(t, FolderPrivacy, classify("darwin", eperm))
	require.Equal(t, FolderNotWritable, classify("darwin", eacces), "a file mode says not writable")
	require.Equal(t, FolderNotWritable, classify("linux", eperm), "only macOS's EPERM is the privacy protection")
	require.Equal(t, FolderMissing, classify("linux", fs.ErrNotExist))
	require.Equal(t, FolderError, classify("linux", errors.New("boom")))
	if runtime.GOOS != "windows" {
		require.Equal(t, FolderNotWritable, classify("linux", &fs.PathError{Op: "open", Path: "/x", Err: syscall.EROFS}))
		require.Equal(t, FolderNotDirectory, classify("linux", &fs.PathError{Op: "open", Path: "/x", Err: syscall.ENOTDIR}))
	}

	c := FolderCheck{Dir: "/Users/me/Documents/Backups", State: FolderPrivacy}
	require.Contains(t, c.Message(), "System Settings > Privacy & Security")
}

func TestMountRoot(t *testing.T) {
	for _, tc := range []struct{ goos, dir, want string }{
		{"darwin", "/Volumes/T7/Seamless Backups", "/Volumes/T7"},
		{"darwin", "/Volumes/T7", "/Volumes/T7"},
		{"darwin", "/Volumes/", ""},
		{"darwin", "/Users/me/Backups", ""},
		{"linux", "/media/me/T7/Backups", "/media/me/T7"},
		{"linux", "/media/me", ""},
		{"linux", "/run/media/me/T7/x", "/run/media/me/T7"},
		{"linux", "/mnt/nas/backups", "/mnt/nas"},
		{"linux", "/home/me/Dropbox", ""},
		{"linux", "/Volumes/T7", ""},
		{"windows", `D:\Backups`, `D:\`},
		{"windows", `C:\Backups`, ""},
		{"windows", `\\server\share\x`, ""},
	} {
		require.Equal(t, tc.want, mountRoot(tc.goos, tc.dir), "%s %s", tc.goos, tc.dir)
	}
}

func TestFolders_DisconnectedDiskIsNeverWrittenUnder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mount points")
	}
	f, _ := testFolders(t)
	f.goos = "linux"
	dir := "/mnt/seamless-test-absent-disk-7f3a/Seamless Backups"
	c := f.check(dir)
	require.Equal(t, FolderDisconnected, c.State)
	require.Equal(t, "/mnt/seamless-test-absent-disk-7f3a", c.Volume)
	require.Equal(t, "/mnt/seamless-test-absent-disk-7f3a is not connected", c.Message())
	require.Equal(t, FolderDisconnected, f.prepare(dir, true).State, "nothing is created under an absent mount point")
}

// fakeFacts serves FileFacts for the guard: every path is a directory the
// daemon owns, mode 0700, unless an override says otherwise.
func fakeFacts(euid uint32, over map[string]FileFacts, probe *FileFacts) func(string) (FileFacts, error) {
	return func(p string) (FileFacts, error) {
		if probe != nil && strings.HasPrefix(filepath.Base(p), ".seamless-probe-") {
			return *probe, nil
		}
		if f, ok := over[p]; ok {
			return f, nil
		}
		if _, err := os.Lstat(p); err != nil {
			return FileFacts{}, err
		}
		return FileFacts{UID: euid, Mode: fs.ModeDir | 0o700, IsDir: true}, nil
	}
}

func TestFolders_GuardHoldsPeerAuthsRule(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("peer_auth runs on macOS and Linux")
	}
	const daemon = 260
	parent := t.TempDir()
	dir := filepath.Join(parent, "Seamless Backups")
	require.NoError(t, os.Mkdir(dir, 0o700))
	real, err := filepath.EvalSymlinks(parent)
	require.NoError(t, err)

	cases := []struct {
		name   string
		over   map[string]FileFacts
		probe  *FileFacts
		wantOK bool
		want   string
	}{
		{name: "owned by the daemon, private", wantOK: true},
		{name: "root's ancestors are fine", over: map[string]FileFacts{
			filepath.Dir(parent): {UID: 0, Mode: fs.ModeDir | 0o755, IsDir: true},
		}, wantOK: true},
		{name: "a sticky world-writable ancestor is fine", over: map[string]FileFacts{
			filepath.Dir(parent): {UID: 0, Mode: fs.ModeDir | fs.ModeSticky | 0o777, IsDir: true},
		}, wantOK: true},
		{name: "a person's ancestor", over: map[string]FileFacts{
			parent: {UID: 501, Mode: fs.ModeDir | 0o755, IsDir: true}, real: {UID: 501, Mode: fs.ModeDir | 0o755, IsDir: true},
		}, want: "owned by uid 501"},
		{name: "a group-writable ancestor", over: map[string]FileFacts{
			parent: {UID: daemon, Mode: fs.ModeDir | 0o775, IsDir: true}, real: {UID: daemon, Mode: fs.ModeDir | 0o775, IsDir: true},
		}, want: "writable by other accounts"},
		{name: "the folder is another account's", over: map[string]FileFacts{
			dir: {UID: 501, Mode: fs.ModeDir | 0o700, IsDir: true},
		}, want: "owned by uid 501"},
		{name: "the folder is a symlink", over: map[string]FileFacts{
			dir: {UID: daemon, Mode: fs.ModeSymlink | 0o777, Symlink: true},
		}, want: "is a symlink"},
		{name: "the folder is world-writable", over: map[string]FileFacts{
			dir: {UID: daemon, Mode: fs.ModeDir | 0o777, IsDir: true},
		}, want: "writable by other accounts"},
		{name: "the disk does not keep ownership", probe: &FileFacts{UID: 99, Mode: 0o600}, want: "does not keep ownership"},
		{name: "the disk does not keep modes", probe: &FileFacts{UID: daemon, Mode: 0o777}, want: "does not keep permissions"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := testFolders(t)
			f.guard = &Guard{EUID: daemon, Inspect: fakeFacts(daemon, tc.over, tc.probe)}
			c := f.prepare(dir, true)
			if tc.wantOK {
				require.Equal(t, FolderReady, c.State, c.Problem)
				return
			}
			require.Equal(t, FolderUnsafe, c.State)
			require.Contains(t, c.Problem, tc.want)
			require.Equal(t, c.Problem, c.Message())
			require.Empty(t, files(t, dir), "a probe that failed the rule is removed too")
		})
	}
}
