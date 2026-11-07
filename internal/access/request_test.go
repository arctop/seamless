package access

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Every lock remedy offers only the approval paths the daemon has (Ways):
// the owner's own terminal always, Touch ID once a key is enrolled (and only
// where the agent's client shows the approval dialog), a passkey page once a
// passkey can sign there.
func TestWays_OwnerApproves(t *testing.T) {
	const page = "https://example.test/console/access/01REQ"
	for _, c := range []struct {
		name string
		w    Ways
		want string
	}{
		{"no approvers", Ways{},
			"the owner approves it in their own terminal with `seam approve R-K71M`"},
		{"passkey only", Ways{Passkey: true, Page: page},
			"the owner approves it in their own terminal with `seam approve R-K71M`, or with a passkey at " + page},
		{"Touch ID only", Ways{TouchID: true},
			"the owner approves it with Touch ID in the approval dialog (where this agent's client shows one), " +
				"or in their own terminal with `seam approve R-K71M`"},
		{"both", Ways{TouchID: true, Passkey: true, Page: page},
			"the owner approves it with Touch ID in the approval dialog (where this agent's client shows one), " +
				"in their own terminal with `seam approve R-K71M`, or with a passkey at " + page},
	} {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, c.w.OwnerApproves("R-K71M"))
		})
	}
	require.Equal(t, "the owner approves it in their own terminal with `seam approve` and the request's id, "+
		"or with a passkey on the request's console page", Ways{Passkey: true}.OwnerApproves(""), "no request yet")

	require.Equal(t, "`seam approve R-K71M` in their own terminal", Ways{}.StillPending("R-K71M"))
	require.Equal(t, "`seam approve R-K71M` in their own terminal, a passkey at "+page+", or Touch ID in the next approval dialog",
		Ways{TouchID: true, Passkey: true, Page: page}.StillPending("R-K71M"))
}

// A lock refusal for a process no approval may name says why nothing was
// filed and what to do, on its two lines, and names no request.
func TestLockedRefusal_Unfiled(t *testing.T) {
	p := lockPolicy(t)
	sock := socketView(t, p, lockProc)
	unfiled := LockRequest{Unfiled: "Seamless files no access request for this process: it is tmux, a terminal multiplexer, " +
		"not an agent, and an approval would open the project to every program it started, other agent sessions included",
		UnfiledRemedy: "run the work from an agent session (Claude Code or Codex), which asks for itself."}
	ctx := WithLockRequester(WithView(context.Background(), sock), func(context.Context, string) (LockRequest, error) {
		return unfiled, nil
	})
	for _, err := range []error{Refusal(ctx, lockFin), sock.RefusalFor(lockFin, unfiled)} {
		require.ErrorIs(t, err, ErrNotReachable)
		ls := strings.Split(err.Error(), "\n")
		require.Len(t, ls, 2, err.Error())
		require.Equal(t, "project "+lockFin+" is locked: even the owner's own agents need the owner's approval, and "+
			"Seamless files no access request for this process: it is tmux, a terminal multiplexer, not an agent, and an "+
			"approval would open the project to every program it started, other agent sessions included.", ls[0])
		require.Equal(t, "Remedy: run the work from an agent session (Claude Code or Codex), which asks for itself.", ls[1])
		require.NotContains(t, err.Error(), "R-", "no request id: none was filed")
	}
	_, err := SessionScope(ctx, lockLegal)
	require.ErrorContains(t, err, "Seamless files no access request")
}
