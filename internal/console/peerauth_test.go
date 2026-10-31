package console

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
)

func consolePolicy(t *testing.T) *access.Policy {
	t.Helper()
	accounts := map[string]access.Account{"katata": {UID: 501, Name: "katata"}, "_arctop_finance": {UID: 470, Name: "_arctop_finance"}}
	p, err := access.New(config.PeerAuth{Owner: "katata", Users: map[string]config.PeerUser{
		"_arctop_finance": {Sealed: []string{"arctop-finance"}},
	}}, func(n string) (access.Account, error) {
		if a, ok := accounts[n]; ok {
			return a, nil
		}
		return access.Account{}, fmt.Errorf("no %s", n)
	})
	require.NoError(t, err)
	return p
}

// The console is the owner's surface. Under peer_auth a configured account on
// the socket is refused it outright, key or no key; the owner on the socket
// needs no key, because the kernel already named it; the owner on TCP still
// needs one.
func TestConsole_PeerAuthGate(t *testing.T) {
	mux := newTestMux(t)
	p := consolePolicy(t)
	ownerSock, _ := p.PeerView(501)
	fin, _ := p.PeerView(470)

	for _, tc := range []struct {
		name   string
		view   *access.View
		key    string
		status int
	}{
		{"a configured account, even with the key", &fin, testKey, http.StatusForbidden},
		{"the owner on the socket, no key", &ownerSock, "", http.StatusOK},
		{"the owner on TCP, no key", ptr(p.OwnerView()), "", http.StatusUnauthorized},
		{"the owner on TCP, with the key", ptr(p.OwnerView()), testKey, http.StatusOK},
		{"peer_auth off, with the key", nil, testKey, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/console/settings", nil)
			req.Header.Set("Accept", "application/json")
			if tc.key != "" {
				req.Header.Set("Authorization", "Bearer "+tc.key)
			}
			if tc.view != nil {
				req = req.WithContext(access.WithView(req.Context(), *tc.view))
			}
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, req)
			require.Equal(t, tc.status, rr.Code, rr.Body.String())
		})
	}
}

func ptr[T any](v T) *T { return &v }
