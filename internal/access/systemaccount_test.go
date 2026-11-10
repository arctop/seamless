package access

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/require"
)

// macOS: the role range ends below 500; the first account it makes for a
// person is 501.
func TestDarwinSystemAccount(t *testing.T) {
	for uid, want := range map[uint32]bool{
		0: true, 1: true, 480: true, 499: true,
		500: false, 501: false, 502: false, 1501: false,
		4294967294: false, // nobody, uid -2
	} {
		got, rule := darwinSystemAccount(uid)
		require.Equal(t, want, got, "uid %d", uid)
		require.Equal(t, "macOS system accounts are below uid 500", rule)
	}
}

// Linux: below UID_MIN from login.defs, through the path seam; 1000 when the
// file or the key is missing or the value is no uid.
func TestLinuxSystemAccount(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
		return p
	}
	debian := write("debian", "# /etc/login.defs\n#\n# Min/max values for automatic uid selection in useradd\n#\nUID_MIN\t\t\t 1000\nUID_MAX\t\t\t60000\n#SYS_UID_MIN\t\t  100\n#SYS_UID_MAX\t\t  999\n")
	rhel5 := write("rhel5", "UID_MIN                   500\n")
	for _, tc := range []struct {
		name string
		path string
		uid  uint32
		want bool
		rule string
	}{
		{"a useradd --system uid", debian, 480, true, "below uid 1000, the UID_MIN in " + debian},
		{"the last system uid", debian, 999, true, "below uid 1000, the UID_MIN in "},
		{"the first ordinary uid", debian, 1000, false, "below uid 1000, the UID_MIN in "},
		{"an ordinary account", debian, 1001, false, ""},
		{"a lower UID_MIN", rhel5, 499, true, "below uid 500, the UID_MIN in " + rhel5},
		{"an ordinary account under a lower UID_MIN", rhel5, 501, false, ""},
		{"no file", filepath.Join(dir, "missing"), 999, true, "below uid 1000, the default, since " + filepath.Join(dir, "missing") + " cannot be read"},
		{"no file, an ordinary account", filepath.Join(dir, "missing"), 1000, false, "the default"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, rule := linuxSystemAccount(tc.uid, tc.path)
			require.Equal(t, tc.want, got)
			require.True(t, strings.HasPrefix(rule, "system accounts here are below uid "), rule)
			require.Contains(t, rule, tc.rule)
		})
	}
}

func TestParseUIDMin(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       uint32
		from       string
	}{
		{"spaces", "UID_MIN 1000\n", 1000, "the UID_MIN in /x"},
		{"tabs and a trailing comment", "UID_MIN\t\t500   # old RHEL\n", 500, "the UID_MIN in /x"},
		{"quoted", "UID_MIN \"2000\"\n", 2000, "the UID_MIN in /x"},
		{"the last line wins", "UID_MIN 1000\nUID_MIN 5000\n", 5000, "the UID_MIN in /x"},
		{"a comment is no setting", "#UID_MIN 10\n# UID_MIN 10\n", 1000, "the default, since /x sets no UID_MIN"},
		{"no such key", "UID_MAX 60000\nSYS_UID_MIN 100\n", 1000, "the default, since /x sets no UID_MIN"},
		{"a key with no value", "UID_MIN\n", 1000, "the default, since /x sets no UID_MIN"},
		{"empty", "", 1000, "the default, since /x sets no UID_MIN"},
		{"not a number", "UID_MIN lots\n", 1000, `the default, since /x sets UID_MIN "lots", which is not a uid`},
		{"negative", "UID_MIN -1\n", 1000, `the default, since /x sets UID_MIN "-1", which is not a uid`},
		{"hex is not taken", "UID_MIN 0x3E8\n", 1000, `the default, since /x sets UID_MIN "0x3E8", which is not a uid`},
		{"past a uid's range", "UID_MIN 99999999999\n", 1000, `which is not a uid`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, from := parseUIDMin(strings.NewReader(tc.body), "/x")
			require.Equal(t, tc.want, got)
			require.Contains(t, from, tc.from)
		})
	}
	got, from := parseUIDMin(iotest.ErrReader(errors.New("boom")), "/x")
	require.Equal(t, uint32(1000), got, "a read that fails gives the default")
	require.Equal(t, "the default, since /x cannot be read", from)
}

// The platform's own answer is its rule: the guide's _seamless (uid 480) is a
// system account on both, and the first account a person gets on either is not.
func TestSystemAccount_ThisPlatform(t *testing.T) {
	switch runtime.GOOS {
	case "darwin":
		ok, rule := SystemAccount(480)
		require.True(t, ok)
		require.Equal(t, "macOS system accounts are below uid 500", rule)
		ok, _ = SystemAccount(501)
		require.False(t, ok)
	case "linux":
		limit, _ := uidMinAt("/etc/login.defs")
		ok, rule := SystemAccount(limit - 1)
		require.True(t, ok, rule)
		require.Contains(t, rule, "/etc/login.defs")
		ok, _ = SystemAccount(limit)
		require.False(t, ok)
	default:
		ok, rule := SystemAccount(480)
		require.False(t, ok, "no range is invented")
		require.Contains(t, rule, "macOS and Linux only")
	}
}
