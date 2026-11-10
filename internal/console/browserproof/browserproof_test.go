package browserproof

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"math/big"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestVerify_RoundTrip(t *testing.T) {
	priv, err := NewKey()
	require.NoError(t, err)
	want, err := EncodeKey(&priv.PublicKey)
	require.NoError(t, err)
	now := time.Now()
	for _, skew := range []time.Duration{0, Window - time.Second, -Window + time.Second} {
		proof, err := Sign(priv, now.Add(skew))
		require.NoError(t, err)
		got, err := Verify(proof, now)
		require.NoError(t, err, "skew %s", skew)
		require.Equal(t, want, got)
	}
}

// A proof a browser's WebCrypto made: r||s over SHA-256 of Message, which is
// what static/sw.js sends. Built here by hand from the documented format.
func TestVerify_WebCryptoShape(t *testing.T) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	require.NoError(t, err)
	key := base64.RawURLEncoding.EncodeToString(der)
	unix := time.Now().Unix()
	sum := sha256.Sum256([]byte("seamless browser proof v1\n" + strconv.FormatInt(unix, 10)))
	r, s, err := ecdsa.Sign(rand.Reader, priv, sum[:])
	require.NoError(t, err)
	sig := append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	got, err := Verify("v1."+strconv.FormatInt(unix, 10)+"."+key+"."+base64.RawURLEncoding.EncodeToString(sig), time.Now())
	require.NoError(t, err)
	require.Equal(t, key, got)
}

func TestVerify_Refusals(t *testing.T) {
	priv, err := NewKey()
	require.NoError(t, err)
	other, err := NewKey()
	require.NoError(t, err)
	now := time.Now()
	good, err := Sign(priv, now)
	require.NoError(t, err)
	parts := strings.Split(good, ".")
	otherKey, err := EncodeKey(&other.PublicKey)
	require.NoError(t, err)
	theirs, err := Sign(other, now)
	require.NoError(t, err)
	theirSig := strings.Split(theirs, ".")[3]

	_, err = Verify("", now)
	require.ErrorIs(t, err, ErrNone)

	stale, err := Sign(priv, now.Add(-Window-time.Second))
	require.NoError(t, err)
	_, err = Verify(stale, now)
	require.ErrorIs(t, err, ErrClock, "a proof from before the window")
	ahead, err := Sign(priv, now.Add(Window+time.Second))
	require.NoError(t, err)
	_, err = Verify(ahead, now)
	require.ErrorIs(t, err, ErrClock, "a proof from after the window")

	// A P-384 key is not one the console's page makes.
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	require.NoError(t, err)
	der384, err := x509.MarshalPKIXPublicKey(&p384.PublicKey)
	require.NoError(t, err)

	// The key part spelled with a non-zero trailing bit: the same bytes under
	// lax base64, so it must be refused to keep one spelling per key.
	k := parts[2]
	last := strings.IndexByte("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_", k[len(k)-1])
	require.GreaterOrEqual(t, last, 0)
	lax := k[:len(k)-1] + string("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"[last|1])
	if lax == k {
		lax = k[:len(k)-1] + string("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"[last&^1])
	}

	big1 := new(big.Int).Lsh(big.NewInt(1), 300)
	for name, header := range map[string]string{
		"another key's signature":       strings.Join([]string{"v1", parts[1], parts[2], theirSig}, "."),
		"another key named":             strings.Join([]string{"v1", parts[1], otherKey, parts[3]}, "."),
		"another time":                  strings.Join([]string{"v1", strconv.FormatInt(now.Unix()+1, 10), parts[2], parts[3]}, "."),
		"padded time":                   strings.Join([]string{"v1", "0" + parts[1], parts[2], parts[3]}, "."),
		"a huge time":                   strings.Join([]string{"v1", big1.String(), parts[2], parts[3]}, "."),
		"another version":               strings.Join([]string{"v2", parts[1], parts[2], parts[3]}, "."),
		"a fifth part":                  good + ".x",
		"three parts":                   strings.Join(parts[:3], "."),
		"a P-384 key":                   strings.Join([]string{"v1", parts[1], base64.RawURLEncoding.EncodeToString(der384), parts[3]}, "."),
		"a key that is not base64":      strings.Join([]string{"v1", parts[1], "!!!", parts[3]}, "."),
		"a key spelled two ways":        strings.Join([]string{"v1", parts[1], lax, parts[3]}, "."),
		"a padded signature":            good + "==",
		"a short signature":             strings.Join([]string{"v1", parts[1], parts[2], parts[3][:40]}, "."),
		"an empty key":                  strings.Join([]string{"v1", parts[1], "", parts[3]}, "."),
		"an overlong key":               strings.Join([]string{"v1", parts[1], strings.Repeat("A", maxKeyChars+4), parts[3]}, "."),
		"a signature that is not ours ": strings.Join([]string{"v1", parts[1], parts[2], base64.RawURLEncoding.EncodeToString(make([]byte, 64))}, "."),
	} {
		_, err := Verify(header, now)
		require.ErrorIs(t, err, ErrInvalid, name)
	}
}
