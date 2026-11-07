package update

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAssetURL_NamesTheTag(t *testing.T) {
	v := Version{Major: 0, Minor: 9, Patch: 1}
	require.Equal(t, "https://github.com/arctop/seamless/releases/download/v0.9.1/seamless-approve_0.9.1_darwin.zip",
		AssetURL(ReleaseDownloadBase, v, ApproverAsset(v)))
	require.Equal(t, "https://example.test/dl/v0.9.1/checksums.txt", AssetURL("https://example.test/dl/", v, ChecksumsAsset))
}

func TestChecksumOf(t *testing.T) {
	body := []byte("the app")
	sum := sha256.Sum256(body)
	hexSum := hex.EncodeToString(sum[:])
	manifest := []byte(strings.Repeat("0", 64) + "  seamless_0.9.1_darwin_arm64.tar.gz\n" +
		strings.ToUpper(hexSum) + "  seamless-approve_0.9.1_darwin.zip\n")

	got, err := ChecksumOf(manifest, "seamless-approve_0.9.1_darwin.zip")
	require.NoError(t, err)
	require.Equal(t, hexSum, got, "lower-cased")
	require.NoError(t, VerifyChecksum(manifest, "seamless-approve_0.9.1_darwin.zip", body))

	for name, tc := range map[string]struct {
		manifest []byte
		body     []byte
	}{
		"not listed":         {[]byte(hexSum + "  other.zip\n"), body},
		"listed twice":       {append(manifest, []byte(hexSum+"  seamless-approve_0.9.1_darwin.zip\n")...), body},
		"a malformed hash":   {[]byte("abc  seamless-approve_0.9.1_darwin.zip\n"), body},
		"a prefix name only": {[]byte(hexSum + "  seamless-approve_0.9.1_darwin.zip.old\n"), body},
		"other bytes":        {manifest, []byte("a tampered app")},
	} {
		err := VerifyChecksum(tc.manifest, "seamless-approve_0.9.1_darwin.zip", tc.body)
		require.True(t, errors.Is(err, ErrChecksum), "%s: %v", name, err)
	}
}
