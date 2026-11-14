package releasesig

import (
	"errors"
	"fmt"
	"strings"

	"github.com/arctop/seamless/internal/sshsig"
)

// The owner's signature: beside the Sigstore signature the release workflow
// makes, the owner signs each release's checksums.txt with a key on a
// hardware security key, in an attended session (scripts/release/sign.sh),
// and the workflow publishes the release only once that signature verifies
// (releasekit verify-owner). An admin token can push a tag and approve the
// release environment; it cannot make the owner's key sign. Which keys are
// the owner's is an OpenSSH allowed_signers file: the repository's
// release_signers for the release workflow, cask.sh and the installers, and
// update.release_signers, a copy the machine's administrator keeps, for the
// updater.

// OwnerNamespace is the SSHSIG namespace the owner signs releases in:
// `ssh-keygen -Y sign -n seamless-release`. A signature made for any other
// namespace (git's, another project's) never verifies as a release's.
const OwnerNamespace = "seamless-release"

// OwnerPolicy is what a release's owner signature must meet: made for
// OwnerNamespace, and, by a security key, with the key both touched and
// verifying its owner (PIN or biometric) -- flags 0x05. The allowed_signers
// lines never say verify-required (ssh-keygen refuses that option there), so
// this is where user verification is required.
var OwnerPolicy = sshsig.Policy{
	Namespace:        OwnerNamespace,
	SecurityKeyFlags: sshsig.FlagUserPresence | sshsig.FlagUserVerification,
}

// ErrNoOwnerKeys is a release-signers file that lists no key allowed to sign
// in OwnerNamespace, so no release could ever verify against it.
var ErrNoOwnerKeys = errors.New("releasesig: the release signers list no key for namespace " + OwnerNamespace)

// ParseOwnerSigners parses a release-signers file and requires at least one
// key allowed to sign in OwnerNamespace. Lines for other namespaces may stay
// (one allowed_signers file can serve several purposes); they never verify a
// release.
func ParseOwnerSigners(data []byte) ([]sshsig.Signer, error) {
	signers, err := sshsig.ParseAllowedSigners(data)
	if err != nil {
		return nil, err
	}
	for _, s := range signers {
		if s.AllowsNamespace(OwnerNamespace) {
			return signers, nil
		}
	}
	return nil, ErrNoOwnerKeys
}

// OwnerKeys describes the keys in signers that may sign a release, one
// "principal SHA256:..." per key, for what a tool prints.
func OwnerKeys(signers []sshsig.Signer) []string {
	var out []string
	for _, s := range signers {
		if s.AllowsNamespace(OwnerNamespace) {
			out = append(out, strings.Join(s.Principals, ",")+" "+s.Key.Fingerprint())
		}
	}
	return out
}

// VerifyOwner checks that sig, an armored SSHSIG, is the owner's signature
// over checksums: OwnerPolicy, by a key signers allows for OwnerNamespace.
// The error wraps the sshsig error that decided (sshsig.ErrUnlisted,
// sshsig.ErrBadSignature, ...).
func VerifyOwner(signers []sshsig.Signer, checksums, sig []byte) (sshsig.Verified, error) {
	v, err := sshsig.Verify(signers, OwnerPolicy, checksums, sig)
	if err != nil {
		return sshsig.Verified{}, fmt.Errorf("the owner's signature over checksums.txt: %w", err)
	}
	return v, nil
}

// Describe is one line about a verified owner signature: who, which key,
// and what the security key reported.
func Describe(v sshsig.Verified) string {
	who := strings.Join(v.Signer.Principals, ",")
	if strings.HasPrefix(v.KeyType, "sk-") {
		return fmt.Sprintf("%s (%s, %s, flags 0x%02x, counter %d)", who, v.Fingerprint, v.KeyType, v.Flags, v.Counter)
	}
	return fmt.Sprintf("%s (%s, %s)", who, v.Fingerprint, v.KeyType)
}
