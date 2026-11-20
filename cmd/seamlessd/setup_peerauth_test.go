package main

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// setupPeerAuthArrange records the peer-auth setup runs a test's setup makes.
func setupPeerAuthArrange(r *setupRun, err error) *[][]string {
	var runs [][]string
	r.peerAuthSetup = func(args []string) error {
		runs = append(runs, args)
		return err
	}
	return &runs
}

// Choosing "Turn on locked projects (peer_auth)" on a re-run runs the move,
// which shows its plan and asks once at the terminal: no --yes.
func TestSetupLockedProjects_TheChosenChangeRunsTheMove(t *testing.T) {
	r, out, _ := setupTestRun(t, "darwin", nil, setupFactsStandalone, true, "3\n")
	runs := setupPeerAuthArrange(r, nil)
	require.NoError(t, setupDirect(r))
	require.Equal(t, [][]string{{"peer-auth", "setup"}}, *runs)
	require.Contains(t, out.String(), "turning on locked projects: seamlessd peer-auth setup")
	require.NotContains(t, out.String(), "by hand")
}

// SEAMLESS_PEER_AUTH=1 is the installers' --peer-auth: consent in the
// environment, so the move does not ask again.
func TestSetupLockedProjects_TheKnobRunsTheMoveWithoutAsking(t *testing.T) {
	t.Run("a re-run of an installed Standalone", func(t *testing.T) {
		r, out, _ := setupTestRun(t, "linux", []string{"SEAMLESS_PEER_AUTH=1"}, setupFactsStandalone, false, "")
		runs := setupPeerAuthArrange(r, nil)
		require.NoError(t, setupDirect(r))
		require.Equal(t, [][]string{{"peer-auth", "setup", "--yes"}}, *runs)
		require.Contains(t, out.String(), "turning on locked projects")
	})
	t.Run("a first install, through the installer's two halves", func(t *testing.T) {
		state := filepath.Join(t.TempDir(), "setup.state")
		r, out, _ := setupTestRun(t, "darwin", []string{"SEAMLESS_PEER_AUTH=1"}, setupFactsFresh, false, "")
		runs := setupPeerAuthArrange(r, nil)
		require.NoError(t, setupForInstaller(r, state))
		require.Contains(t, out.String(), "Standalone (SEAMLESS_PEER_AUTH)")
		require.Empty(t, *runs, "nothing moves before the installer has installed")
		require.NoError(t, setupFinish(r, state))
		require.Equal(t, [][]string{{"peer-auth", "setup", "--yes"}}, *runs)
	})
	t.Run("with SEAMLESS_SETUP=server, after the Server's own steps", func(t *testing.T) {
		r, _, _ := setupTestRun(t, "linux", []string{"SEAMLESS_PEER_AUTH=1", "SEAMLESS_SETUP=server"}, setupFactsStandalone, false, "")
		st, err := setupDecide(r)
		require.NoError(t, err)
		require.Equal(t, "server", st.change)
		ids := []string{}
		for _, s := range setupStepsLeft(st, "linux") {
			ids = append(ids, s.id)
		}
		require.Contains(t, ids, "locked-projects")
	})
	t.Run("already on: nothing to change", func(t *testing.T) {
		r, _, _ := setupTestRun(t, "linux", []string{"SEAMLESS_PEER_AUTH=1"}, setupFactsLocked, false, "")
		st, err := setupDecide(r)
		require.NoError(t, err)
		require.Empty(t, st.change)
		require.Empty(t, setupStepsLeft(st, "linux"))
	})
}

// Without the knob or the choice, a first install lists the step with the
// by-hand ones, saying how to have setup do it.
func TestSetupLockedProjects_NotAskedForIsListedWithTheWayToAsk(t *testing.T) {
	r, out, _ := setupTestRun(t, "linux", nil, setupFactsFresh, false, "")
	st := setupState{kind: setupStandalone, answer: setupFromEnv}
	require.NoError(t, setupHandOff(r, &st, setupStepsLeft(st, "linux"), true))
	require.Contains(t, out.String(), "setup does not do this Standalone step unless asked:")
	require.Contains(t, out.String(), "the installer does it with SEAMLESS_PEER_AUTH=1")
}

// A move that stops says where it stands; setup stops with it.
func TestSetupLockedProjects_AStoppedMoveSaysWhereItStands(t *testing.T) {
	r, _, _ := setupTestRun(t, "linux", []string{"SEAMLESS_PEER_AUTH=1"}, setupFactsStandalone, false, "")
	setupPeerAuthArrange(r, errors.New("exit status 1"))
	err := setupDirect(r)
	require.ErrorContains(t, err, "seamlessd peer-auth setup: exit status 1")
	require.ErrorContains(t, err, "seamlessd peer-auth status says where it stands")
}

// SEAMLESS_PEER_AUTH is refused, before anything is installed, wherever the
// move cannot be made.
func TestSetupLockedProjects_TheKnobIsRefusedWhereItCannotBeDone(t *testing.T) {
	for _, tt := range []struct {
		name  string
		goos  string
		env   []string
		facts setupFacts
		err   string
	}{
		{"a value other than 1", "linux", []string{"SEAMLESS_PEER_AUTH=yes"}, setupFactsFresh, `invalid SEAMLESS_PEER_AUTH="yes": set it to 1`},
		{"Windows", "windows", []string{"SEAMLESS_PEER_AUTH=1"}, setupFactsFresh, "Windows has no peer_auth"},
		{"a Client of a Server", "linux", []string{"SEAMLESS_PEER_AUTH=1", "SEAMLESS_SERVER_URL=https://box.example:8081", "SEAMLESS_MCP_API_KEY=k"},
			setupFactsFresh, "a Client runs no daemon"},
		{"an installed Client", "darwin", []string{"SEAMLESS_PEER_AUTH=1"}, setupFactsClient, "a Client runs no daemon"},
		{"no service", "darwin", []string{"SEAMLESS_PEER_AUTH=1", "SEAMLESS_NO_SERVICE=1"}, setupFactsFresh, "SEAMLESS_NO_SERVICE installs none"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, _, _ := setupTestRun(t, tt.goos, tt.env, tt.facts, false, "")
			_, err := setupDecide(r)
			require.ErrorContains(t, err, tt.err)
			require.ErrorIs(t, err, errSetupRefused)
		})
	}
}
