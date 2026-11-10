package access

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseDaemonPin(t *testing.T) {
	for _, tc := range []struct {
		body string
		name string
		uid  uint32
	}{
		{"_seamless 480\n", "_seamless", 480},
		{"_seamless 480", "_seamless", 480}, // no newline: printf rather than echo
		{"seamless-daemon.v2 1", "seamless-daemon.v2", 1},
		{"daemon 4294967295\n", "daemon", 4294967295},
		{strings.Repeat("a", 64) + " 480\n", strings.Repeat("a", 64), 480},
	} {
		pin, err := parseDaemonPin([]byte(tc.body))
		require.NoError(t, err, "%q", tc.body)
		require.Equal(t, DaemonPin{Name: tc.name, UID: tc.uid}, pin, "%q", tc.body)
	}

	for body, want := range map[string]string{
		"":                             "is empty",
		"\n":                           "is empty",
		"_seamless":                    "has 1 space-separated fields, not 2",
		"_seamless 480 extra\n":        "has 3 space-separated fields, not 2",
		"_seamless 480 \n":             "has 3 space-separated fields, not 2",
		"_seamless  480\n":             "has 3 space-separated fields, not 2",
		" _seamless 480\n":             "has 3 space-separated fields, not 2",
		"_seamless\t480\n":             "has 1 space-separated fields, not 2",
		"_seamless 480\r\n":            "has more than one line",
		"_seamless 480\n\n":            "has more than one line",
		"_seamless 480\nroot 0\n":      "has more than one line",
		"_seamless 0\n":                `gives "0" as the uid`,
		"_seamless 0480\n":             `gives "0480" as the uid`,
		"_seamless -1\n":               `gives "-1" as the uid`,
		"_seamless +480\n":             `gives "+480" as the uid`,
		"_seamless 4294967296\n":       `gives "4294967296" as the uid`,
		"_seamless 99999999999\n":      `gives "99999999999" as the uid`,
		"_seamless 0x1e0\n":            `gives "0x1e0" as the uid`,
		"#480 480\n":                   `names "#480", which is not an account name`,
		"1seamless 480\n":              `names "1seamless", which is not an account name`,
		"_sea$(id) 480\n":              `which is not an account name`,
		strings.Repeat("a", 65) + " 1": "which is not an account name",
		strings.Repeat("x", 257):       "is longer than 256 bytes",
	} {
		_, err := parseDaemonPin([]byte(body))
		require.Error(t, err, "%q", body)
		require.Contains(t, err.Error(), want, "%q", body)
		require.True(t, strings.HasSuffix(err.Error(), `: it must be one line, the daemon's account name and uid ("_seamless 480")`), err.Error())
	}
}

// pinRig is a pin under a temp dir, read through the host's file system with
// two things stood in for: every path under the dir is root's unless owners
// says otherwise (a test cannot chown to root), and the system's directories
// above it are root's 0755 (the test does not control them, and /tmp is 1777).
type pinRig struct {
	root   string
	owners map[string]uint32
}

func newPinRig(t *testing.T) *pinRig {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, os.Chmod(root, 0o755))
	return &pinRig{root: root, owners: map[string]uint32{}}
}

func (r *pinRig) fs() pinFS {
	stand := func(p string, f pinFacts) pinFacts {
		if p != r.root && !strings.HasPrefix(p, r.root+string(filepath.Separator)) {
			return pinFacts{uid: 0, mode: fs.ModeDir | 0o755}
		}
		f.uid = 0
		if u, ok := r.owners[p]; ok {
			f.uid = u
		}
		return f
	}
	return pinFS{
		lstat: func(p string) (pinFacts, error) {
			f, err := hostPinFS.lstat(p)
			if err != nil {
				return f, err
			}
			return stand(p, f), nil
		},
		evalSymlinks: hostPinFS.evalSymlinks,
		open: func(p string, n int) (pinFacts, []byte, error) {
			f, data, err := hostPinFS.open(p, n)
			if err != nil {
				return f, data, err
			}
			return stand(p, f), data, nil
		},
	}
}

// write lays out dir/name with body at mode, dir 0755, and returns its path.
func (r *pinRig) write(t *testing.T, rel, body string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(r.root, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
	require.NoError(t, os.WriteFile(p, []byte(body), mode))
	require.NoError(t, os.Chmod(p, mode))
	return p
}

func (r *pinRig) read(path string) (DaemonPin, bool, error) { return readDaemonPin(path, r.fs()) }

func TestReadDaemonPin_Trusted(t *testing.T) {
	r := newPinRig(t)
	path := r.write(t, "etc/daemon-account", "_seamless 480\n", 0o644)
	pin, present, err := r.read(path)
	require.NoError(t, err)
	require.True(t, present)
	require.Equal(t, DaemonPin{Name: "_seamless", UID: 480, Path: path}, pin)
	require.Equal(t, "_seamless (uid 480)", pin.String())

	// 0600 and 0444 are writable by root alone too.
	for _, mode := range []os.FileMode{0o600, 0o444, 0o640} {
		require.NoError(t, os.Chmod(path, mode))
		_, _, err = r.read(path)
		require.NoError(t, err, "%04o", mode)
	}

	// A symlink in a trusted directory to a file in another trusted directory.
	target := r.write(t, "real/daemon-account", "_seamless 480\n", 0o644)
	link := filepath.Join(r.root, "etc", "link")
	require.NoError(t, os.Symlink(target, link))
	pin, _, err = r.read(link)
	require.NoError(t, err)
	require.Equal(t, link, pin.Path, "named by the path it was read from")
}

func TestReadDaemonPin_Absent(t *testing.T) {
	r := newPinRig(t)
	for _, p := range []string{filepath.Join(r.root, "daemon-account"), filepath.Join(r.root, "no", "such", "dir", "daemon-account")} {
		pin, present, err := r.read(p)
		require.NoError(t, err, p)
		require.False(t, present, p)
		require.Zero(t, pin, p)
	}
}

// Every way a pin can be another account's is a refusal naming the path and
// the problem, with present true: none falls back.
func TestReadDaemonPin_Untrusted(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, r *pinRig) string
		want  string
	}{
		{"owned by another account", func(t *testing.T, r *pinRig) string {
			p := r.write(t, "etc/daemon-account", "_seamless 480\n", 0o644)
			r.owners[p] = 501
			return p
		}, "is owned by uid 501: it must belong to root"},
		{"group-writable", func(t *testing.T, r *pinRig) string {
			return r.write(t, "etc/daemon-account", "_seamless 480\n", 0o664)
		}, "is mode 0664: it must be writable by root alone (0644)"},
		{"world-writable", func(t *testing.T, r *pinRig) string {
			return r.write(t, "etc/daemon-account", "_seamless 480\n", 0o646)
		}, "is mode 0646"},
		{"a group-writable parent", func(t *testing.T, r *pinRig) string {
			p := r.write(t, "etc/daemon-account", "_seamless 480\n", 0o644)
			require.NoError(t, os.Chmod(filepath.Dir(p), 0o775))
			return p
		}, "which is mode 0775: every directory above the pin must be writable by root alone"},
		{"a sticky world-writable parent", func(t *testing.T, r *pinRig) string {
			p := r.write(t, "etc/daemon-account", "_seamless 480\n", 0o644)
			require.NoError(t, os.Chmod(filepath.Dir(p), 0o777|os.ModeSticky))
			return p
		}, "which is mode 0777"},
		{"a parent owned by another account", func(t *testing.T, r *pinRig) string {
			p := r.write(t, "etc/daemon-account", "_seamless 480\n", 0o644)
			r.owners[filepath.Dir(p)] = 501
			return p
		}, "etc, which is owned by uid 501: every directory above the pin must belong to root"},
		{"a directory further up owned by another account", func(t *testing.T, r *pinRig) string {
			p := r.write(t, "opt/seamless/etc/daemon-account", "_seamless 480\n", 0o644)
			r.owners[filepath.Join(r.root, "opt")] = 501
			return p
		}, "opt, which is owned by uid 501"},
		{"a symlink to a file in a group-writable directory", func(t *testing.T, r *pinRig) string {
			target := r.write(t, "loose/daemon-account", "_seamless 480\n", 0o644)
			require.NoError(t, os.Chmod(filepath.Dir(target), 0o775))
			link := filepath.Join(r.root, "etc", "daemon-account")
			require.NoError(t, os.MkdirAll(filepath.Dir(link), 0o755))
			require.NoError(t, os.Symlink(target, link))
			return link
		}, "loose, which is mode 0775"},
		{"a symlink in a directory another account owns", func(t *testing.T, r *pinRig) string {
			target := r.write(t, "real/daemon-account", "_seamless 480\n", 0o644)
			link := filepath.Join(r.root, "theirs", "daemon-account")
			require.NoError(t, os.MkdirAll(filepath.Dir(link), 0o755))
			require.NoError(t, os.Symlink(target, link))
			r.owners[filepath.Dir(link)] = 501
			return link
		}, "theirs, which is owned by uid 501"},
		{"a symlink to nothing", func(t *testing.T, r *pinRig) string {
			link := filepath.Join(r.root, "etc", "daemon-account")
			require.NoError(t, os.MkdirAll(filepath.Dir(link), 0o755))
			require.NoError(t, os.Symlink(filepath.Join(r.root, "gone"), link))
			return link
		}, "cannot be resolved"},
		{"a directory", func(t *testing.T, r *pinRig) string {
			p := filepath.Join(r.root, "etc", "daemon-account")
			require.NoError(t, os.MkdirAll(p, 0o755))
			return p
		}, "is not a regular file"},
		{"does not parse", func(t *testing.T, r *pinRig) string {
			return r.write(t, "etc/daemon-account", "_seamless 480 extra\n", 0o644)
		}, "has 3 space-separated fields, not 2"},
		{"empty", func(t *testing.T, r *pinRig) string {
			return r.write(t, "etc/daemon-account", "", 0o644)
		}, "is empty"},
		{"too long", func(t *testing.T, r *pinRig) string {
			return r.write(t, "etc/daemon-account", "_seamless 480\n"+strings.Repeat("#", 300), 0o644)
		}, "is longer than 256 bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newPinRig(t)
			path := tc.setup(t, r)
			pin, present, err := r.read(path)
			require.ErrorIs(t, err, ErrDaemonPin)
			require.True(t, present, "a pin that is there is never the fallback")
			require.Zero(t, pin)
			require.True(t, strings.HasPrefix(err.Error(), "the daemon account pin "+path+" "), err.Error())
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestReadDaemonPin_Unreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a 0000 file")
	}
	r := newPinRig(t)
	path := r.write(t, "etc/daemon-account", "_seamless 480\n", 0o000)
	_, present, err := r.read(path)
	require.ErrorIs(t, err, ErrDaemonPin)
	require.True(t, present)
	require.Contains(t, err.Error(), "cannot be read")
}

// A stat that fails for another reason than absence is a refusal too.
func TestReadDaemonPin_StatError(t *testing.T) {
	boom := errors.New("input/output error")
	fsys := pinFS{lstat: func(string) (pinFacts, error) { return pinFacts{}, boom }}
	_, present, err := readDaemonPin("/opt/seamless/etc/daemon-account", fsys)
	require.ErrorIs(t, err, ErrDaemonPin)
	require.True(t, present)
	require.Equal(t, "the daemon account pin /opt/seamless/etc/daemon-account cannot be read: input/output error", err.Error())
}

func TestDaemonUID(t *testing.T) {
	pin := DaemonPin{Name: "_seamless", UID: 480, Path: DaemonAccountPath}
	never := func(uint32) (bool, string) { panic("the system-account rule is not asked when a pin is present") }
	ok, rule := DaemonUID(480, pin, true, never)
	require.True(t, ok)
	require.Equal(t, "/opt/seamless/etc/daemon-account pins the daemon's account as _seamless (uid 480)", rule)
	ok, _ = DaemonUID(484, pin, true, never)
	require.False(t, ok, "another role account is refused under a pin")
	ok, _ = DaemonUID(501, pin, true, never)
	require.False(t, ok)

	// No pin: the system-account rule.
	ok, rule = DaemonUID(484, DaemonPin{}, false, darwinSystemAccount)
	require.True(t, ok)
	require.Equal(t, "macOS system accounts are below uid 500", rule)
	ok, _ = DaemonUID(501, DaemonPin{}, false, darwinSystemAccount)
	require.False(t, ok)
}
