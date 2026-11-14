package mcp

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/store"
)

// A request the store would not record as given is named for the agent as
// what it refused -- a program over 1024 bytes, not the reason -- on the lock
// path and the grant and move paths alike.
func TestAccessRequestError_NamesWhatTheStoreRefused(t *testing.T) {
	const want = "no access request was filed: program is limited to 1024 bytes of valid UTF-8"
	program := fmt.Errorf("%w: program is limited to 1024 bytes of valid UTF-8", store.ErrInvalidAccessRequest)
	require.EqualError(t, accessRequestError(fmt.Errorf("approval.Request: store.CreateAccessApproval: %w", program)), want)
	grant := fmt.Errorf("approval.RequestGrant: store.CreateGrantRequest: %w", program)
	require.EqualError(t, grantRequestError(grant), want)
	require.EqualError(t, askRequestError(grant, "example-finance"), want)
	reason := fmt.Errorf("approval.Request: %w: the reason is limited to 200 characters", store.ErrInvalidAccessRequest)
	require.EqualError(t, accessRequestError(reason), "no access request was filed: the reason is limited to 200 characters")
}
