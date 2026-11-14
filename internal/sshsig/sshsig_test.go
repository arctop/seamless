package sshsig_test

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/sshsig"
	"github.com/arctop/seamless/internal/sshsig/sshsigtest"
)

const ns = "seamless-release"

// releasePolicy is the policy a release is verified with: a security key
// must have been touched and must have verified its owner.
var releasePolicy = sshsig.Policy{Namespace: ns, SecurityKeyFlags: sshsig.FlagUserPresence | sshsig.FlagUserVerification}

func newKey(t *testing.T, keyType string) *sshsigtest.Key {
	t.Helper()
	k, err := sshsigtest.NewKey(keyType)
	require.NoError(t, err)
	return k
}

func signers(t *testing.T, lines ...string) []sshsig.Signer {
	t.Helper()
	s, err := sshsig.ParseAllowedSigners([]byte(strings.Join(lines, "\n") + "\n"))
	require.NoError(t, err)
	return s
}

func sign(t *testing.T, k *sshsigtest.Key, namespace string, msg []byte, o sshsigtest.Options) []byte {
	t.Helper()
	sig, err := k.Sign(namespace, msg, o)
	require.NoError(t, err)
	return sig
}

// TestParseAllowedSigners_RepositoryFile holds the repository's own
// release_signers to the fingerprints its keys were enrolled with: two
// security keys, each listed for the release namespace only.
func TestParseAllowedSigners_RepositoryFile(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "release_signers"))
	require.NoError(t, err)
	got, err := sshsig.ParseAllowedSigners(data)
	require.NoError(t, err)
	want := map[string]string{
		"owner-yk1": "SHA256:YvEX9rwVhKjx3YB38NzaGKFn+VBsbgLJuD5HK+AgNrg",
		"owner-yk2": "SHA256:dQID4KtjMpYBxiEhtZN43vF/sozOPO53wXhfEJ/Ntu4",
	}
	require.Len(t, got, len(want))
	for _, s := range got {
		require.Len(t, s.Principals, 1)
		fp, ok := want[s.Principals[0]]
		require.True(t, ok, "unexpected principal %q", s.Principals[0])
		require.Equal(t, fp, s.Key.Fingerprint(), s.Principals[0])
		require.Equal(t, sshsig.KeySKEd25519, s.Key.Type())
		require.True(t, s.Key.SecurityKey())
		require.Equal(t, []string{ns}, s.Namespaces, "a release key is listed for the release namespace only")
		require.True(t, s.AllowsNamespace(ns))
		require.False(t, s.AllowsNamespace("git"))
	}
}

func TestVerify_EveryKeyType(t *testing.T) {
	msg := []byte("abc123  seamless_0.9.0_linux_amd64.tar.gz\n")
	for _, keyType := range sshsig.KeyTypes {
		for _, alg := range []string{"sha256", "sha512"} {
			t.Run(keyType+"/"+alg, func(t *testing.T) {
				k := newKey(t, keyType)
				s := signers(t, k.SignersLine("owner-a", ns))
				v, err := sshsig.Verify(s, releasePolicy, msg, sign(t, k, ns, msg, sshsigtest.Options{HashAlgorithm: alg, Counter: 42}))
				require.NoError(t, err)
				require.Equal(t, k.Fingerprint(), v.Fingerprint)
				require.Equal(t, []string{"owner-a"}, v.Signer.Principals)
				require.Equal(t, keyType, v.KeyType)
				require.Equal(t, alg, v.HashAlgorithm)
				if strings.HasPrefix(keyType, "sk-") {
					require.Equal(t, byte(0x05), v.Flags)
					require.Equal(t, uint32(42), v.Counter)
				} else {
					require.Zero(t, v.Flags)
				}
			})
		}
	}
}

func TestVerify_Refusals(t *testing.T) {
	msg := []byte("the checksums\n")
	owner := newKey(t, sshsig.KeySKEd25519)
	other := newKey(t, sshsig.KeySKEd25519)
	s := signers(t, owner.SignersLine("owner-a", ns))
	good := sign(t, owner, ns, msg, sshsigtest.Options{})

	truncated := good[:len(good)/2]
	garbage := []byte("-----BEGIN SSH SIGNATURE-----\nbm90IGEgc2lnbmF0dXJl\n-----END SSH SIGNATURE-----\n")
	raw, err := sshsig.Unarmor(good)
	require.NoError(t, err)
	trailing := sshsigtest.Armor(append(bytes.Clone(raw), 0))
	twoBlocks := append(bytes.Clone(good), good...)

	for _, tt := range []struct {
		name    string
		signers []sshsig.Signer
		msg     []byte
		sig     []byte
		want    error
		detail  string
	}{
		{"wrong namespace", s, msg, sign(t, owner, "git", msg, sshsigtest.Options{}), sshsig.ErrNamespace, `namespace "git"`},
		{"unlisted key", s, msg, sign(t, other, ns, msg, sshsigtest.Options{}), sshsig.ErrUnlisted, other.Fingerprint()},
		{"another file", s, []byte("other bytes\n"), good, sshsig.ErrBadSignature, "owner-a"},
		{"one byte changed", s, []byte("the checksums\r"), good, sshsig.ErrBadSignature, ""},
		{"truncated armor", s, msg, truncated, sshsig.ErrMalformed, ""},
		{"garbage", s, msg, garbage, sshsig.ErrMalformed, ""},
		{"not armored", s, msg, []byte("hello"), sshsig.ErrMalformed, "BEGIN SSH SIGNATURE"},
		{"empty", s, msg, nil, sshsig.ErrMalformed, ""},
		{"trailing byte", s, msg, trailing, sshsig.ErrMalformed, "trailing"},
		{"two blocks", s, msg, twoBlocks, sshsig.ErrMalformed, "more than one"},
		{"flags 01: touched, not verified", s, msg, sign(t, owner, ns, msg, sshsigtest.Options{Flags: sshsigtest.Flags(0x01)}), sshsig.ErrFlags, "user verification"},
		{"flags 04: verified, not touched", s, msg, sign(t, owner, ns, msg, sshsigtest.Options{Flags: sshsigtest.Flags(0x04)}), sshsig.ErrFlags, "user presence"},
		{"flags 00", s, msg, sign(t, owner, ns, msg, sshsigtest.Options{Flags: sshsigtest.Flags(0x00)}), sshsig.ErrFlags, ""},
		{"listed for another namespace", signers(t, owner.SignersLine("owner-a", "git")), msg, good, sshsig.ErrNamespace, "listed for namespaces"},
		{"negated namespace", signers(t, owner.SignersLine("owner-a", "*,!seamless-*")), msg, good, sshsig.ErrNamespace, ""},
		{"no signers", nil, msg, good, sshsig.ErrUnlisted, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := sshsig.Verify(tt.signers, releasePolicy, tt.msg, tt.sig)
			require.ErrorIs(t, err, tt.want)
			if tt.detail != "" {
				require.ErrorContains(t, err, tt.detail)
			}
		})
	}

	t.Run("flags 01 pass a policy that asks only for presence", func(t *testing.T) {
		_, err := sshsig.Verify(s, sshsig.Policy{Namespace: ns, SecurityKeyFlags: sshsig.FlagUserPresence}, msg,
			sign(t, owner, ns, msg, sshsigtest.Options{Flags: sshsigtest.Flags(0x01)}))
		require.NoError(t, err)
	})
	t.Run("altered flags break the signature", func(t *testing.T) {
		// A signature made with flags 01, its flags byte rewritten to 05.
		sig := sign(t, owner, ns, msg, sshsigtest.Options{Flags: sshsigtest.Flags(0x01)})
		raw, err := sshsig.Unarmor(sig)
		require.NoError(t, err)
		raw[len(raw)-5] = 0x05
		_, err = sshsig.Verify(s, releasePolicy, msg, sshsigtest.Armor(raw))
		require.ErrorIs(t, err, sshsig.ErrBadSignature)
	})
	t.Run("a key listed twice: either line's namespaces allow it", func(t *testing.T) {
		v, err := sshsig.Verify(signers(t, owner.SignersLine("owner-git", "git"), owner.SignersLine("owner-rel", ns)), releasePolicy, msg, good)
		require.NoError(t, err)
		require.Equal(t, []string{"owner-rel"}, v.Signer.Principals)
	})
	t.Run("no namespaces option allows every namespace", func(t *testing.T) {
		_, err := sshsig.Verify(signers(t, owner.SignersLine("owner-a", "")), releasePolicy, msg, good)
		require.NoError(t, err)
	})
	t.Run("a security key's other application is another key", func(t *testing.T) {
		twin := *owner
		twin.Application = "ssh:other"
		_, err := sshsig.Verify(s, releasePolicy, msg, sign(t, &twin, ns, msg, sshsigtest.Options{}))
		require.ErrorIs(t, err, sshsig.ErrUnlisted)
	})
	t.Run("a hash algorithm other than sha256 and sha512", func(t *testing.T) {
		raw, err := sshsig.Unarmor(good)
		require.NoError(t, err)
		bad := bytes.Replace(raw, []byte("\x00\x00\x00\x06sha512"), []byte("\x00\x00\x00\x06sha384"), 1)
		_, err = sshsig.Verify(s, releasePolicy, msg, sshsigtest.Armor(bad))
		require.ErrorIs(t, err, sshsig.ErrMalformed)
		require.ErrorContains(t, err, "sha384")
	})
	t.Run("a signature type that is not the key's", func(t *testing.T) {
		plain := newKey(t, sshsig.KeyEd25519)
		sig := sign(t, plain, ns, msg, sshsigtest.Options{})
		raw, err := sshsig.Unarmor(sig)
		require.NoError(t, err)
		// The ed25519 key's blob, with its signature field claiming to be a
		// security key's.
		blob := sshsigtest.Blob(plain.PublicBlob(), ns, "sha512",
			append(append([]byte("\x00\x00\x00\x1a"), sshsig.KeySKEd25519...), raw[len(raw)-68:]...))
		_, err = sshsig.Verify(signers(t, plain.SignersLine("p", ns)), releasePolicy, msg, sshsigtest.Armor(blob))
		require.ErrorIs(t, err, sshsig.ErrMalformed)
	})
	t.Run("Verify needs a namespace", func(t *testing.T) {
		_, err := sshsig.Verify(s, sshsig.Policy{}, msg, good)
		require.Error(t, err)
	})
}

func TestParseAllowedSigners_Lines(t *testing.T) {
	k := newKey(t, sshsig.KeySKEd25519)
	e := newKey(t, sshsig.KeyECDSAP256)
	for _, tt := range []struct {
		name, data string
		check      func(t *testing.T, s []sshsig.Signer)
		wantErr    string
	}{
		{name: "comments, blank lines, a trailing comment and CRLF",
			data: "# header\r\n\r\n  " + k.SignersLine("a@example.com,b", ns) + " the daily key\r\n" + e.SignersLine("c", "") + "\n",
			check: func(t *testing.T, s []sshsig.Signer) {
				require.Len(t, s, 2)
				require.Equal(t, []string{"a@example.com", "b"}, s[0].Principals)
				require.Equal(t, 3, s[0].Line)
				require.Nil(t, s[1].Namespaces)
				require.Equal(t, sshsig.KeyECDSAP256, s[1].Key.Type())
			}},
		{name: "a quoted principal and two namespaces",
			data: `"owner one" namespaces="file,seamless-*" ` + k.AuthorizedKey(),
			check: func(t *testing.T, s []sshsig.Signer) {
				require.Equal(t, []string{"owner one"}, s[0].Principals)
				require.Equal(t, []string{"file", "seamless-*"}, s[0].Namespaces)
				require.True(t, s[0].AllowsNamespace(ns))
				require.False(t, s[0].AllowsNamespace("git"))
			}},
		{name: "empty file", data: "\n# nothing\n", check: func(t *testing.T, s []sshsig.Signer) { require.Empty(t, s) }},
		{name: "cert-authority", data: "a cert-authority " + k.AuthorizedKey(), wantErr: "cert-authority is not accepted"},
		{name: "verify-required", data: `a verify-required,namespaces="x" ` + k.AuthorizedKey(), wantErr: "verify-required is not an allowed_signers option"},
		{name: "valid-before", data: `a valid-before="20300101" ` + k.AuthorizedKey(), wantErr: "valid-before is not accepted"},
		{name: "valid-after", data: `a valid-after="20200101" ` + k.AuthorizedKey(), wantErr: "valid-after is not accepted"},
		{name: "unknown option", data: "a no-touch-required " + k.AuthorizedKey(), wantErr: `unknown option "no-touch-required"`},
		{name: "unquoted namespaces", data: "a namespaces=x " + k.AuthorizedKey(), wantErr: "double-quoted"},
		{name: "namespaces twice", data: `a namespaces="x",namespaces="y" ` + k.AuthorizedKey(), wantErr: "twice"},
		{name: "rsa", data: "a ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQ==", wantErr: `unsupported key type "ssh-rsa"`},
		{name: "a certificate", data: "a ssh-ed25519-cert-v01@openssh.com AAAA", wantErr: "unsupported key type"},
		{name: "type and key disagree", data: "a ssh-ed25519 " + base64.StdEncoding.EncodeToString(k.PublicBlob()), wantErr: "is a sk-ssh-ed25519@openssh.com key"},
		{name: "bad base64", data: "a ssh-ed25519 !!!", wantErr: "base64"},
		{name: "no key", data: "a", wantErr: "no key after the principals"},
		{name: "options and no key", data: `a namespaces="x"`, wantErr: "no key after the options"},
		{name: "no base64", data: "a ssh-ed25519", wantErr: "no base64 key"},
		{name: "unterminated quote", data: `a namespaces="x ` + k.AuthorizedKey(), wantErr: "unterminated quote"},
		{name: "an empty principal", data: "a,,b " + k.AuthorizedKey(), wantErr: "empty principal"},
		{name: "too large", data: strings.Repeat("#", sshsig.MaxSignersBytes+1), wantErr: "larger than"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, err := sshsig.ParseAllowedSigners([]byte(tt.data))
			if tt.wantErr != "" {
				require.ErrorIs(t, err, sshsig.ErrSigners)
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			tt.check(t, s)
		})
	}
}

func TestParsePublicKey_Refusals(t *testing.T) {
	k := newKey(t, sshsig.KeySKEd25519)
	blob := k.PublicBlob()
	_, err := sshsig.ParsePublicKey(append(bytes.Clone(blob), 1))
	require.ErrorContains(t, err, "trailing")
	_, err = sshsig.ParsePublicKey(blob[:len(blob)-1])
	require.Error(t, err)
	// A security key with an empty application.
	noApp := *k
	noApp.Application = ""
	_, err = sshsig.ParsePublicKey(noApp.PublicBlob())
	require.Error(t, err)
}

// TestOwnerKeys_Fingerprints holds the parser to fingerprints ssh-keygen
// computed for the owner's two keys, from the lines alone (not the
// repository file), so a change to the parser that drops the key's
// application from its encoding cannot pass by changing both.
func TestOwnerKeys_Fingerprints(t *testing.T) {
	for line, fp := range map[string]string{
		`owner-yk1 namespaces="seamless-release" sk-ssh-ed25519@openssh.com AAAAGnNrLXNzaC1lZDI1NTE5QG9wZW5zc2guY29tAAAAINd9uqZ0YLBjsCnvfvpExpYZXXWNQpK+afg2GUxlh9aUAAAAEnNzaDphcmN0b3AtcmVsZWFzZQ==`: "SHA256:YvEX9rwVhKjx3YB38NzaGKFn+VBsbgLJuD5HK+AgNrg",
		`owner-yk2 namespaces="seamless-release" sk-ssh-ed25519@openssh.com AAAAGnNrLXNzaC1lZDI1NTE5QG9wZW5zc2guY29tAAAAIHaqnbLidFoOJONHvetlWqfvIF2xvMVWfWjWHyv9LFszAAAAEnNzaDphcmN0b3AtcmVsZWFzZQ==`: "SHA256:dQID4KtjMpYBxiEhtZN43vF/sozOPO53wXhfEJ/Ntu4",
	} {
		s := signers(t, line)
		require.Equal(t, fp, s[0].Key.Fingerprint())
		require.Equal(t, sshsig.KeySKEd25519, s[0].Key.Type())
	}
}

// sshKeygen is the system's ssh-keygen when it can do SSHSIG (-Y), else the
// test is skipped.
func sshKeygen(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("no ssh-keygen on PATH")
	}
	out, _ := exec.Command(path, "-Y", "nope").CombinedOutput()
	if !bytes.Contains(out, []byte("-Y verify")) {
		t.Skipf("%s has no -Y verify", path)
	}
	return path
}

// TestInterop_SSHKeygenSigns: signatures ssh-keygen makes with throwaway
// keys verify here.
func TestInterop_SSHKeygenSigns(t *testing.T) {
	keygen := sshKeygen(t)
	for _, tt := range []struct{ name, t, b string }{
		{"ed25519", "ed25519", ""},
		{"ecdsa p256", "ecdsa", "256"},
		{"ecdsa p384", "ecdsa", "384"},
		{"ecdsa p521", "ecdsa", "521"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			key := filepath.Join(dir, "k")
			args := []string{"-q", "-t", tt.t, "-N", "", "-C", "", "-f", key}
			if tt.b != "" {
				args = append(args, "-b", tt.b)
			}
			out, err := exec.Command(keygen, args...).CombinedOutput()
			require.NoError(t, err, string(out))
			msg := filepath.Join(dir, "checksums.txt")
			require.NoError(t, os.WriteFile(msg, []byte("abc  file\n"), 0o600))
			out, err = exec.Command(keygen, "-Y", "sign", "-n", ns, "-f", key, msg).CombinedOutput()
			require.NoError(t, err, string(out))
			pub, err := os.ReadFile(key + ".pub")
			require.NoError(t, err)
			sig, err := os.ReadFile(msg + ".sig")
			require.NoError(t, err)
			s := signers(t, `tester namespaces="`+ns+`" `+strings.TrimSpace(string(pub)))
			v, err := sshsig.Verify(s, releasePolicy, []byte("abc  file\n"), sig)
			require.NoError(t, err)
			require.Equal(t, "sha512", v.HashAlgorithm)
			fp, err := exec.Command(keygen, "-l", "-E", "sha256", "-f", key+".pub").CombinedOutput()
			require.NoError(t, err, string(fp))
			require.Contains(t, string(fp), v.Fingerprint)
			_, err = sshsig.Verify(s, releasePolicy, []byte("abc  file\r"), sig)
			require.ErrorIs(t, err, sshsig.ErrBadSignature)
		})
	}
}

// TestInterop_SSHKeygenVerifies: signatures made here -- the synthetic
// security-key ones included -- pass ssh-keygen -Y verify, which is what
// proves the security-key construction is OpenSSH's.
func TestInterop_SSHKeygenVerifies(t *testing.T) {
	keygen := sshKeygen(t)
	for _, keyType := range sshsig.KeyTypes {
		t.Run(keyType, func(t *testing.T) {
			dir := t.TempDir()
			k := newKey(t, keyType)
			msg := []byte("abc  file\n")
			sig := sign(t, k, ns, msg, sshsigtest.Options{})
			sigPath := filepath.Join(dir, "m.sig")
			require.NoError(t, os.WriteFile(sigPath, sig, 0o600))
			signersPath := filepath.Join(dir, "signers")
			require.NoError(t, os.WriteFile(signersPath, []byte(k.SignersLine("tester", ns)+"\n"), 0o600))
			cmd := exec.Command(keygen, "-Y", "verify", "-f", signersPath, "-I", "tester", "-n", ns, "-s", sigPath)
			cmd.Stdin = bytes.NewReader(msg)
			out, err := cmd.CombinedOutput()
			require.NoError(t, err, string(out))
			require.Contains(t, string(out), `Good "seamless-release" signature for tester`)
			require.Contains(t, string(out), k.Fingerprint())
			cmd = exec.Command(keygen, "-Y", "verify", "-f", signersPath, "-I", "tester", "-n", ns, "-s", sigPath)
			cmd.Stdin = strings.NewReader("abc  file\r")
			out, err = cmd.CombinedOutput()
			require.Error(t, err, string(out))
		})
	}
}

func TestErrorsAreDistinct(t *testing.T) {
	all := []error{sshsig.ErrMalformed, sshsig.ErrNamespace, sshsig.ErrUnlisted, sshsig.ErrBadSignature, sshsig.ErrFlags, sshsig.ErrSigners}
	for i, a := range all {
		for j, b := range all {
			require.Equal(t, i == j, errors.Is(a, b))
		}
	}
}
