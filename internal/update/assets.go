package update

// Release assets as files to fetch and check: where one downloads from, the
// opt-in macOS approver app's name, and the checksum checksums.txt lists for
// an asset. Pure functions, no network and no verifier: the seam CLI's `seam
// touchid enroll` uses them, and must not pull in the daemon's Sigstore
// verifier (it would more than double the CLI's size).

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// ReleaseDownloadBase holds every release's assets, one directory per tag:
// ReleaseDownloadBase + "/vX.Y.Z/<asset>". Never GitHub's "latest": a fetch
// always names the release it wants.
const ReleaseDownloadBase = "https://github.com/" + Repo + "/releases/download"

// ChecksumsAsset is the manifest every release carries: sha256sum's format,
// "<hex>  <name>" per line.
const ChecksumsAsset = "checksums.txt"

// ChecksumsBundleAsset is the Sigstore bundle over checksums.txt that every
// release after v0.6.0 carries (goreleaser signs it per tag; release.yml
// re-verifies it against the tag's exact identity).
const ChecksumsBundleAsset = ChecksumsAsset + ".sigstore.json"

// OwnerSignatureAsset is the owner's own signature over checksums.txt: an
// OpenSSH SSHSIG (`ssh-keygen -Y sign -n seamless-release`), made with one of
// the keys in the repository's release_signers file on a hardware security
// key, in an attended session, after CI built the release and before it was
// published (scripts/release/sign.sh). Not ".sig": checksums.txt.sig is the
// Sigstore detached signature the installers hand to cosign. An install
// whose update.release_signers pins the owner's keys installs only a release
// whose checksums.txt this verifies against them.
const OwnerSignatureAsset = ChecksumsAsset + ".sshsig"

// AssetURL is where release v's asset name downloads from under base
// (ReleaseDownloadBase in production). It is built from a parsed version and
// a name this binary chose, never from API text.
func AssetURL(base string, v Version, name string) string {
	return strings.TrimSuffix(base, "/") + "/v" + v.String() + "/" + name
}

// ApproverAsset is the macOS Touch ID approver app of release v (plan step
// T): a zip of "Seamless Approve.app", built, signed and notarized on the
// owner's Mac with the darwin binaries (scripts/release/macos.sh) as a
// separate, opt-in asset, and listed in the release's checksums.txt like
// every archive. cmd/releasekit spells the same name.
func ApproverAsset(v Version) string {
	return fmt.Sprintf("seamless-approve_%s_darwin.zip", v)
}

// ErrChecksum is an asset that checksums.txt does not vouch for: listed zero
// times or more than once, with a malformed SHA-256, or with one that does
// not match the bytes fetched.
var ErrChecksum = errors.New("checksums.txt does not vouch for the asset")

// ChecksumOf returns the lowercase hex SHA-256 checksums lists for name. Like
// the installers (awk '$2 == f') it matches the name field exactly, and it
// refuses a manifest that lists name zero times or several, or with a
// malformed hash: a reader of it would check nothing, or pick one of two.
func ChecksumOf(checksums []byte, name string) (string, error) {
	var found []string
	for line := range strings.SplitSeq(string(checksums), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == name {
			found = append(found, fields[0])
		}
	}
	if len(found) != 1 {
		return "", fmt.Errorf("%w: %s appears %d times", ErrChecksum, name, len(found))
	}
	sum := strings.ToLower(found[0])
	if raw, err := hex.DecodeString(sum); err != nil || len(raw) != sha256.Size {
		return "", fmt.Errorf("%w: %s has a malformed checksum", ErrChecksum, name)
	}
	return sum, nil
}

// VerifyChecksum checks that body is the asset name checksums lists.
func VerifyChecksum(checksums []byte, name string, body []byte) error {
	want, err := ChecksumOf(checksums, name)
	if err != nil {
		return err
	}
	got := sha256.Sum256(body)
	if hex.EncodeToString(got[:]) != want {
		return fmt.Errorf("%w: %s does not match its checksum", ErrChecksum, name)
	}
	return nil
}
