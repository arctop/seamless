package shellcmd

import (
	"os/exec"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestToolsFor_AbsolutePathsPerOS(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		tools := ToolsFor(goos)
		require.Equal(t, Sudo, tools.Sudo, goos)
		require.Equal(t, Env, tools.Env, goos)
		for name, p := range map[string]string{"id": tools.ID, "install": tools.Install, "tee": tools.Tee, "chmod": tools.Chmod,
			"chown": tools.Chown, "mv": tools.Mv, "sum": tools.Sum, "tailscale": tools.Tailscale} {
			require.True(t, len(p) > 1 && p[0] == '/', "%s %s: %q", goos, name, p)
		}
	}
	mac, linux := ToolsFor("darwin"), ToolsFor("linux")
	require.Equal(t, "/usr/sbin/chown", mac.Chown)
	require.Equal(t, "/bin/chown", linux.Chown)
	require.Equal(t, "/usr/bin/shasum -a 256", mac.Sum)
	require.Equal(t, "/usr/bin/sha256sum", linux.Sum)
	require.Equal(t, "/bin/launchctl", mac.Launchctl)
	require.Empty(t, mac.Systemctl)
	require.Equal(t, "/bin/systemctl", linux.Systemctl)
	require.Equal(t, "/bin/loginctl", linux.Loginctl)
	require.Empty(t, mac.Loginctl)
	require.Empty(t, linux.Dscl)
	require.Equal(t, "/usr/bin/sudo /usr/bin/install -m 0755 a b", mac.AsRoot(mac.Install, "-m", "0755", "a", "b"))
}

func TestPosixArg(t *testing.T) {
	for in, want := range map[string]string{
		"":                       "''",
		"plain":                  "plain",
		"/opt/seamless/bin/x":    "/opt/seamless/bin/x",
		"user@host:1.2,3+4%5_6-": "user@host:1.2,3+4%5_6-",
		"two words":              "'two words'",
		"it's":                   `'it'\''s'`,
		"=cmd":                   "'=cmd'",
		"~/x":                    "'~/x'",
		"$HOME":                  "'$HOME'",
		"a;b":                    "'a;b'",
		"caf\u00e9":              "'caf\u00e9'",
	} {
		require.Equal(t, want, PosixArg(in), "%q", in)
	}
}

// What PosixArg quotes, sh reads back as the one word it was.
func TestPosixArg_ShellReadsItBack(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no POSIX shell")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	for _, in := range []string{"", "plain", "two words", "it's", `a"b`, "$HOME", "`id`", "=x", "~", "a\\b", "x\ny"} {
		out, err := exec.Command(sh, "-c", "printf %s "+PosixArg(in)).Output()
		require.NoError(t, err, "%q", in)
		require.Equal(t, in, string(out), "%q", in)
	}
}
