package passkey

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCredential_LibraryRoundTrip checks that converting a credential record
// to the library's and back loses nothing the record holds.
func TestCredential_LibraryRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		cred Credential
	}{
		{
			name: "security key",
			cred: Credential{
				ID: []byte{1, 2, 3}, PublicKey: []byte{0xa5}, Algorithm: AlgES256, SignCount: 42,
				AAGUID: make([]byte, 16), Transports: []string{"usb", "nfc"}, Attachment: "cross-platform",
				UserHandle: []byte("owner-handle"), AttestationFormat: "none",
			},
		},
		{
			name: "synced passkey",
			cred: Credential{
				ID: []byte{4, 5, 6}, PublicKey: []byte{0xa4}, Algorithm: AlgEdDSA,
				AAGUID: []byte("0123456789abcdef"), Transports: []string{"internal", "hybrid"}, Attachment: "platform",
				BackupEligible: true, BackupState: true, UserHandle: []byte("owner-handle"), AttestationFormat: "packed",
			},
		},
		{
			name: "eligible, not backed up, no transports",
			cred: Credential{
				ID: []byte{7}, PublicKey: []byte{0xa4}, Algorithm: AlgRS256, SignCount: 1,
				AAGUID: make([]byte, 16), Transports: []string{}, BackupEligible: true,
				UserHandle: []byte("owner-handle"), AttestationFormat: "none",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lc := tc.cred.toLibrary()
			require.True(t, lc.Flags.UserPresent)
			require.True(t, lc.Flags.UserVerified)
			require.Equal(t, tc.cred, fromLibrary(lc, tc.cred.Algorithm, tc.cred.UserHandle))
		})
	}
}
