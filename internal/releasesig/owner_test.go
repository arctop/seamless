package releasesig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/sshsig"
	"github.com/arctop/seamless/internal/sshsig/sshsigtest"
)

func ownerKey(t *testing.T, keyType string) *sshsigtest.Key {
	t.Helper()
	k, err := sshsigtest.NewKey(keyType)
	require.NoError(t, err)
	return k
}

func TestVerifyOwner(t *testing.T) {
	checksums := []byte("aaaa  seamless_0.9.0_linux_amd64.tar.gz\n")
	yk := ownerKey(t, sshsig.KeySKEd25519)
	ec := ownerKey(t, sshsig.KeySKECDSAP256)
	plain := ownerKey(t, sshsig.KeyEd25519)
	stranger := ownerKey(t, sshsig.KeySKEd25519)
	signers, err := ParseOwnerSigners([]byte(strings.Join([]string{
		yk.SignersLine("owner-a", OwnerNamespace),
		ec.SignersLine("owner-b", OwnerNamespace),
		plain.SignersLine("owner-c", OwnerNamespace),
	}, "\n")))
	require.NoError(t, err)
	sign := func(k *sshsigtest.Key, ns string, msg []byte, o sshsigtest.Options) []byte {
		sig, err := k.Sign(ns, msg, o)
		require.NoError(t, err)
		return sig
	}

	for _, k := range []*sshsigtest.Key{yk, ec, plain} {
		v, err := VerifyOwner(signers, checksums, sign(k, OwnerNamespace, checksums, sshsigtest.Options{}))
		require.NoError(t, err, k.Type)
		require.Equal(t, k.Fingerprint(), v.Fingerprint)
		require.Contains(t, Describe(v), k.Fingerprint())
	}
	for _, tt := range []struct {
		name string
		sig  []byte
		msg  []byte
		want error
	}{
		{"git's namespace", sign(yk, "git", checksums, sshsigtest.Options{}), checksums, sshsig.ErrNamespace},
		{"a stranger's key", sign(stranger, OwnerNamespace, checksums, sshsigtest.Options{}), checksums, sshsig.ErrUnlisted},
		{"another file", sign(yk, OwnerNamespace, []byte("other\n"), sshsigtest.Options{}), checksums, sshsig.ErrBadSignature},
		{"touched without the PIN", sign(yk, OwnerNamespace, checksums, sshsigtest.Options{Flags: sshsigtest.Flags(0x01)}), checksums, sshsig.ErrFlags},
		{"sk-ecdsa touched without the PIN", sign(ec, OwnerNamespace, checksums, sshsigtest.Options{Flags: sshsigtest.Flags(0x01)}), checksums, sshsig.ErrFlags},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := VerifyOwner(signers, tt.msg, tt.sig)
			require.ErrorIs(t, err, tt.want)
		})
	}
}

func TestParseOwnerSigners(t *testing.T) {
	k := ownerKey(t, sshsig.KeySKEd25519)
	_, err := ParseOwnerSigners([]byte("# no keys\n"))
	require.ErrorIs(t, err, ErrNoOwnerKeys)
	_, err = ParseOwnerSigners([]byte(k.SignersLine("owner-a", "git")))
	require.ErrorIs(t, err, ErrNoOwnerKeys, "a key for git's namespace only signs no release")
	s, err := ParseOwnerSigners([]byte(k.SignersLine("owner-a", "git") + "\n" + k.SignersLine("owner-a", "seamless-*")))
	require.NoError(t, err)
	require.Equal(t, []string{"owner-a " + k.Fingerprint()}, OwnerKeys(s))
	_, err = ParseOwnerSigners([]byte(`owner-a verify-required ` + k.AuthorizedKey()))
	require.ErrorIs(t, err, sshsig.ErrSigners)

	data, err := os.ReadFile(filepath.Join("..", "..", "release_signers"))
	require.NoError(t, err)
	s, err = ParseOwnerSigners(data)
	require.NoError(t, err)
	require.Equal(t, []string{
		"owner-yk1 SHA256:YvEX9rwVhKjx3YB38NzaGKFn+VBsbgLJuD5HK+AgNrg",
		"owner-yk2 SHA256:dQID4KtjMpYBxiEhtZN43vF/sozOPO53wXhfEJ/Ntu4",
	}, OwnerKeys(s))
}

func TestTagIdentityRegexp(t *testing.T) {
	re, err := TagIdentityRegexp("v0.9.0-rc.1")
	require.NoError(t, err)
	require.Equal(t, `^https://github\.com/arctop/seamless/\.github/workflows/release\.yml@refs/tags/v0\.9\.0-rc\.1$`, re)
	re, err = TagIdentityRegexp("v0.9.0")
	require.NoError(t, err)
	require.Equal(t, IdentityRegexp(mustVersion(t, "0.9.0")), re)
	for _, bad := range []string{"0.9.0", "v0.9", "v0.9.0-", "v0.9.0-rc 1", "v0.9.0$", "refs/tags/v0.9.0", ""} {
		_, err := TagIdentityRegexp(bad)
		require.ErrorIs(t, err, ErrTag, bad)
	}
}

func TestVerifyEntityForTag(t *testing.T) {
	sums := []byte("aaaa  seamless_0.9.0-rc.1_linux_amd64.tar.gz\n")
	virtual, entity := signedAsset(t, signedFor("0.9.0-rc.1"), SigningIssuer, sums)
	require.NoError(t, VerifyEntityForTag(virtual, entity, sums, "v0.9.0-rc.1"))
	require.ErrorContains(t, VerifyEntityForTag(virtual, entity, sums, "v0.9.0"), "sigstore verification for v0.9.0")
	require.ErrorContains(t, VerifyEntityForTag(virtual, entity, sums, "v0.9.0-rc.2"), "sigstore verification")
	require.ErrorContains(t, VerifyEntityForTag(virtual, entity, []byte("other"), "v0.9.0-rc.1"), "sigstore verification")
	require.ErrorIs(t, VerifyEntityForTag(virtual, entity, sums, "0.9.0-rc.1"), ErrTag)
}
