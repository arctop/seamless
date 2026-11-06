package passkey

import (
	"fmt"
	"math/big"

	"github.com/go-webauthn/webauthn/protocol/webauthncose"
)

// checkPublicKey parses a COSE_Key and returns its algorithm. It refuses a key
// that does not parse, one whose algorithm is not ES256, EdDSA or RS256, and
// an RSA key under MinRSABits. The library parser has already tied the
// algorithm to the key type and curve (an EC2 key's curve to ES256, an OKP
// key to Ed25519); an RSA key's algorithm is only checked here, and its size
// nowhere else.
func checkPublicKey(cose []byte) (int64, error) {
	key, err := webauthncose.ParsePublicKey(cose)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", ErrUnsupportedKey, err)
	}
	switch k := key.(type) {
	case webauthncose.EC2PublicKeyData:
		if k.Algorithm != AlgES256 {
			return 0, fmt.Errorf("%w: EC2 key with algorithm %d", ErrUnsupportedKey, k.Algorithm)
		}
		return AlgES256, nil
	case webauthncose.OKPPublicKeyData:
		if k.Algorithm != AlgEdDSA {
			return 0, fmt.Errorf("%w: OKP key with algorithm %d", ErrUnsupportedKey, k.Algorithm)
		}
		return AlgEdDSA, nil
	case webauthncose.RSAPublicKeyData:
		if k.Algorithm != AlgRS256 {
			return 0, fmt.Errorf("%w: RSA key with algorithm %d", ErrUnsupportedKey, k.Algorithm)
		}
		if bits := new(big.Int).SetBytes(k.Modulus).BitLen(); bits < MinRSABits {
			return 0, fmt.Errorf("%w: %d bits", ErrWeakKey, bits)
		}
		return AlgRS256, nil
	default:
		return 0, fmt.Errorf("%w: key type %T", ErrUnsupportedKey, key)
	}
}
