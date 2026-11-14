package store

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
)

// fstoreFilers files one request of each kind -- a lock's, a grant's and a
// move's -- for the same agent process, naming program, the one way each is
// filed (CreateAccessApproval, CreateGrantRequest, MoveRequestCreate).
func fstoreFilers(t *testing.T) map[string]func(program string) (AccessApproval, error) {
	t.Helper()
	db := leaseDB(t)
	ctx := context.Background()
	proc := access.ProcessToken{PID: 4242, Version: 7, Boot: "boot-1"}
	return map[string]func(string) (AccessApproval, error){
		AccessKindRequest: func(program string) (AccessApproval, error) {
			req := leaseReq("example-finance", proc)
			req.Program = program
			return CreateAccessApproval(ctx, db, req, fenceNow)
		},
		AccessKindGrant: func(program string) (AccessApproval, error) {
			req := grantReq("01SESSIONOPS")
			req.ProcessKey, req.PeerPID, req.PeerUID, req.Program = proc.Key(), proc.PID, 501, program
			return CreateGrantRequest(ctx, db, req, fenceNow)
		},
		AccessKindMove: func(program string) (AccessApproval, error) {
			req := moveReq("example-finance", "example-ops")
			req.ProcessKey, req.PeerPID, req.PeerUID, req.Program = proc.Key(), proc.PID, 501, program
			return MoveRequestCreate(ctx, db, req, fenceNow)
		},
	}
}

// One rule for the program a request files, whatever it asks for: a lock's
// request, a grant request and a move request each keep the program exactly
// as given up to maxAccessFieldBytes bytes of valid UTF-8, and each refuses a
// longer one, or one that is not valid UTF-8, with the same answer -- never
// cut short. The anchor rule judges a program by its last path element and
// its path's shape, and the approve core judges the stored row again
// (storedAnchorRefusal), so a program cut to fit could judge as another
// program than the one asking.
func TestAccessProgram_OneRuleForLockGrantAndMove(t *testing.T) {
	const tail = "/claude"
	atCap := "/" + strings.Repeat("x", maxAccessFieldBytes-1-len(tail)) + tail
	require.Len(t, atCap, maxAccessFieldBytes)
	wideAtCap := strings.Repeat("é", maxAccessFieldBytes/2)
	require.Len(t, wideAtCap, maxAccessFieldBytes)
	refused := map[string]string{
		"one byte over":          "/" + strings.Repeat("x", maxAccessFieldBytes-len(tail)) + tail,
		"a relay's longest":      "/" + strings.Repeat("x", 4096-1-len(tail)) + tail,
		"a character over":       wideAtCap + "é",
		"not UTF-8":              "/opt/agents/\xff\xfe/claude",
		"a cut UTF-8 sequence":   "/opt/agents/claude\xc3",
		"a shell's name cut off": "/" + strings.Repeat("x", maxAccessFieldBytes-1-len("/zs")) + "/zsh",
	}
	for kind, file := range fstoreFilers(t) {
		for _, program := range []string{atCap, wideAtCap, "/opt/agents/claude"} {
			a, err := file(program)
			require.NoError(t, err, "%s: a program of %d bytes", kind, len(program))
			require.Equal(t, kind, a.Kind)
			require.Equal(t, program, a.Program, "%s: the program is stored as given", kind)
		}
		for name, program := range refused {
			_, err := file(program)
			require.ErrorIs(t, err, ErrInvalidAccessRequest, "%s: %s", kind, name)
			require.ErrorContains(t, err, "program is limited to 1024 bytes of valid UTF-8", "%s: %s", kind, name)
		}
	}
}
