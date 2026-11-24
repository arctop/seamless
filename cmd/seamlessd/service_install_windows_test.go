//go:build windows

package main

import (
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

// TestServiceTaskArguments: the task action's argument string is what
// Windows hands serve, so every path -- with a space, a quote or a trailing
// backslash in it -- arrives whole, and splitTaskArguments reads it back the
// same way. It reads the form install.ps1's cmdlets wrote, every path quoted,
// as the same argv: a task they registered is the one install-service would.
func TestServiceTaskArguments(t *testing.T) {
	for _, dir := range []string{`C:\Users\me`, `C:\Users\Jane Doe`, `C:\Users\a"b`, `C:\odd dir\`} {
		args := serviceTaskArgs(dir+`\.config\seamless\seamless.yaml`, dir+`\.seamless\seamlessd.log`)
		got, err := windows.DecomposeCommandLine(`C:\seamlessd.exe ` + taskArguments(args))
		require.NoError(t, err)
		require.Equal(t, args, got[1:], dir)
		split, err := splitTaskArguments(taskArguments(args))
		require.NoError(t, err)
		require.Equal(t, args, split, dir)
	}
	split, err := splitTaskArguments(`serve --config "C:\Users\me\.config\seamless\seamless.yaml" --log-file "C:\Users\me\.seamless\seamlessd.log"`)
	require.NoError(t, err)
	require.Equal(t, serviceTaskArgs(`C:\Users\me\.config\seamless\seamless.yaml`, `C:\Users\me\.seamless\seamlessd.log`), split)
}

// TestRealServiceTaskEnv reads this user and this executable.
func TestRealServiceTaskEnv(t *testing.T) {
	env, err := realServiceTaskEnv()
	require.NoError(t, err)
	require.Regexp(t, `^S-1-`, env.sid)
	require.NotEmpty(t, env.account)
	require.NotEmpty(t, env.exe)
	require.NotNil(t, env.splitArgs)
	mine, err := env.isCurrentUser(env.account)
	require.NoError(t, err)
	require.True(t, mine, "the trigger's account is this user")
}
