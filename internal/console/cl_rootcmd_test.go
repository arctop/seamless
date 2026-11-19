package console

import (
	"fmt"
	"path"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/backup"
	"github.com/arctop/seamless/internal/shellcmd"
)

// Every command the console's pages show the owner to run as the daemon's
// account names sudo, and the env sudo runs, by absolute path: the owner's
// PATH can lead through a directory the owner's processes write (owner
// decision 2026-11-18, task L4).

// clRootWordProblem says what is wrong with a printed command line: "" when it
// does not start with sudo, or when sudo is shellcmd.Sudo and an env it runs
// is shellcmd.Env; else why.
func clRootWordProblem(line string) string {
	argv := strings.Fields(line)
	if len(argv) == 0 || path.Base(argv[0]) != "sudo" {
		return ""
	}
	if argv[0] != shellcmd.Sudo {
		return fmt.Sprintf("sudo is %q, not %s", argv[0], shellcmd.Sudo)
	}
	i := 1
	for i < len(argv) && strings.HasPrefix(argv[i], "-") {
		if argv[i] == "-u" {
			i++
		}
		i++
	}
	if i < len(argv) && path.Base(argv[i]) == "env" && argv[i] != shellcmd.Env {
		return fmt.Sprintf("sudo runs env as %q, not %s", argv[i], shellcmd.Env)
	}
	return ""
}

func TestCLRootWordProblem_Judges(t *testing.T) {
	for line, bad := range map[string]bool{
		"/usr/bin/sudo -k -u _seamless /usr/bin/env SEAMLESS_CONFIG=/c /s approvers approve --code X": false,
		"/usr/bin/sudo -u <daemon account> seamlessd restore /b":                                      false,
		"seamlessd approvers approve --code X":                                                        false,
		"sudo -u _seamless /s restore /b":                                                             true,
		"/usr/bin/sudo -k -u _seamless env SEAMLESS_CONFIG=/c /s console-open":                        true,
	} {
		require.Equal(t, bad, clRootWordProblem(line) != "", line)
	}
}

// clCommands is every printed command s builds for the daemon's account.
func clCommands(s *Service) map[string]string {
	return map[string]string{
		"enroll-code":  s.enrollCodeCommand("passkey", "ABCD-EFGH-JKMN-PQRS"),
		"approve code": s.approveEnrollmentCommand("7KQ2-M9XD"),
		"sign-in":      s.terminalSignInCommand("7KQ2-M9XD"),
		"restore":      s.restoreCommand("/backups/one.tar.gz", backup.ModeReplace),
		"merge":        s.restoreCommand("/backups/one.tar.gz", backup.ModeMerge),
		"unlock":       s.unlockCommand("example-finance", "7KQ2-M9XD"),
	}
}

func TestCLConsoleCommands_NameSudoAndEnvByPath(t *testing.T) {
	full := &TerminalApprover{Account: "_seamless", Seamlessd: "/opt/seamless/bin/seamlessd", Config: "/opt/seamless/etc/seamless.yaml"}
	noConfig := &TerminalApprover{Account: "_seamless", Seamlessd: "/opt/seamless/bin/seamlessd"}
	for name, tc := range map[string]struct {
		terminal *TerminalApprover
		sudos    int // how many of the six commands go through sudo
	}{
		"the daemon's account and config": {full, 6},
		"no config":                       {noConfig, 6},
		// Without a terminal approver the passkey and sign-in commands are a
		// bare seamlessd; restore and unlock keep a placeholder account.
		"no terminal approver": {nil, 3},
	} {
		t.Run(name, func(t *testing.T) {
			sudos := 0
			for what, line := range clCommands(&Service{cfg: Config{Terminal: tc.terminal}}) {
				require.False(t, strings.HasPrefix(line, "sudo ") || strings.HasPrefix(line, "env "), "%s: %s", what, line)
				require.Empty(t, clRootWordProblem(line), "%s: %s", what, line)
				if strings.HasPrefix(line, shellcmd.Sudo+" ") {
					sudos++
				}
				if tc.terminal != nil && tc.terminal.Config != "" {
					require.Contains(t, line, " "+shellcmd.Env+" SEAMLESS_CONFIG=/opt/seamless/etc/seamless.yaml ", what)
				} else {
					require.NotContains(t, line, "env", what)
				}
			}
			require.Equal(t, tc.sudos, sudos)
		})
	}
	require.Equal(t, "/usr/bin/sudo -k -u _seamless /usr/bin/env SEAMLESS_CONFIG=/opt/seamless/etc/seamless.yaml "+
		"/opt/seamless/bin/seamlessd console-open --unlock example-finance --code 7KQ2-M9XD",
		(&Service{cfg: Config{Terminal: full}}).unlockCommand("example-finance", "7KQ2-M9XD"))
	require.Equal(t, "/usr/bin/sudo -u <daemon account> seamlessd restore --merge /backups/one.tar.gz",
		(&Service{}).restoreCommand("/backups/one.tar.gz", backup.ModeMerge), "the placeholder account, as before")
}
