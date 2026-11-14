package devices

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/store"
)

// MintCode refuses a server whose address names a user, as it refuses one no
// other computer can pair with: no code is minted, and no surface gets one to
// print beside the one-liners. (It needs service.go's MintCode to ask
// ServerFacts.PairingRefusal: the hub snippet of wave nt1113h's REDACT.)
func TestRedact_MintCodeRefusesAUserInTheAddress(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	for _, facts := range []ServerFacts{
		{URL: "https://alice:s3cret@example-server.example:8081", TLS: true, CertFingerprint: CertFingerprint([]byte("x"))},
		{URL: "http://alice:s3cret@192.0.2.10:8081"},
	} {
		svc := New(Config{DB: db, Events: events.NewRecorder(db), LocalHost: "example-server", Server: facts})
		_, err := svc.MintCode(ctx, store.DeviceByConsole)
		require.ErrorIs(t, err, ErrPairingNamesUser, facts.URL)
		require.NotContains(t, err.Error(), "s3cret")
	}
	open, err := store.CountOpenPairingCodes(ctx, db, time.Now())
	require.NoError(t, err)
	require.Zero(t, open, "no code was minted")

	svc := New(Config{DB: db, Events: events.NewRecorder(db), LocalHost: "example-server",
		Server: ServerFacts{URL: "https://example-server.example:8081", TLS: true, CertFingerprint: CertFingerprint([]byte("x"))}})
	code, err := svc.MintCode(ctx, store.DeviceByConsole)
	require.NoError(t, err, "an address with no user mints as before")
	require.NotEmpty(t, code.Code)
}
