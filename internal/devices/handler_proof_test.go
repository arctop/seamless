package devices

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// A paired relay's malformed report of its caller is refused before any route
// runs (wire version 1), never served as the owner; a well-formed one, and a
// relay that forwards nothing, pass. Written against the edge's API as it was
// before relays were judged, so it shows what changed.
func TestJudgeProof_ARelaysMalformedReportIsRefused(t *testing.T) {
	f := pairNewFixture(t, false)
	seen := make(chan deviceProbe, 4)
	ts, pool, _ := f.deviceServer(t, seen, nil)
	id := pairIdentity(t, "example-laptop")
	f.pairRelayDevice(t, "example-laptop", id)
	relay := deviceClient(pool, id)

	status, _, _, reached := deviceGet(t, relay, ts.URL+"/api/mcp", http.Header{
		"X-Seamless-Relay-Version": {"1"}, "X-Seamless-Relay-Uid": {"not-a-uid"},
		"X-Seamless-Relay-Chain": {"7d1c2b3a-4e5f-4a6b-8c7d-9e0f1a2b3c4d:2200:2200000"},
	}, seen)
	require.Equal(t, http.StatusBadRequest, status, "a malformed report is refused")
	require.False(t, reached, "and reaches no route")

	status, _, p, reached := deviceGet(t, relay, ts.URL+"/api/mcp", http.Header{
		"X-Seamless-Relay-Version": {"1"}, "X-Seamless-Relay-Uid": {"501"},
		"X-Seamless-Relay-Chain": {"7d1c2b3a-4e5f-4a6b-8c7d-9e0f1a2b3c4d:2200:2200000"},
	}, seen)
	require.Equal(t, http.StatusOK, status)
	require.True(t, reached)
	require.Empty(t, p.relayHdrs, "never passed on raw")

	status, _, _, reached = deviceGet(t, relay, ts.URL+"/api/mcp", nil, seen)
	require.Equal(t, http.StatusOK, status)
	require.True(t, reached)
}
