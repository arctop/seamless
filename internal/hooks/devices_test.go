package hooks

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/devices"
)

// The hooks' one verifier: the shared key, or a paired device's credential the
// daemon's edge matched; a device key with no edge mark is a wrong key.
func TestVerifyBearer_PairedDeviceOnlyWhenTheEdgeMatchedIt(t *testing.T) {
	key, _, err := devices.NewKey()
	require.NoError(t, err)
	req := func(auth string, marked bool) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/api/hooks/session-start", nil)
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
		if marked {
			r = r.WithContext(devices.WithCredential(r.Context(), devices.Credential{DeviceID: "01DEVICEEXAMPLE", Kind: devices.KindKey, Name: "example-ci"}))
		}
		return r
	}
	require.True(t, verifyBearer(req("Bearer shared", false), "shared"))
	require.True(t, verifyBearer(req("Bearer "+key, true), "shared"))
	require.True(t, verifyBearer(req("", true), ""), "the edge's mark holds with no shared key")
	require.False(t, verifyBearer(req("Bearer "+key, false), "shared"))
	require.False(t, verifyBearer(req("", false), "shared"))
	require.False(t, verifyBearer(req("Bearer shared", false), ""))
}
