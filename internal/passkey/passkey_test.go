package passkey_test

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/arctop/seamless/internal/passkey"
	"github.com/stretchr/testify/require"
)

const testOrigin = "http://localhost:8081"

func newVerifier(t *testing.T) *passkey.Verifier {
	t.Helper()
	v, err := passkey.New(passkey.Config{RPID: "localhost", Origins: []string{testOrigin}})
	require.NoError(t, err)
	return v
}

func newOwner(t *testing.T) passkey.User {
	t.Helper()
	return passkey.User{Handle: randomBytes(t, 16), Name: "owner", DisplayName: "Owner"}
}

func newChallenge(t *testing.T) []byte {
	t.Helper()
	return randomBytes(t, passkey.ChallengeBytes)
}

// register runs a whole registration with a and returns the stored credential.
func register(t *testing.T, v *passkey.Verifier, user passkey.User, a *authenticator) passkey.Credential {
	t.Helper()
	options, state, err := v.BeginRegistration(user, nil)
	require.NoError(t, err)
	cred, err := v.FinishRegistration(user, state, a.create(t, options, testOrigin))
	require.NoError(t, err)
	return cred
}

// approve runs a whole assertion over a fresh approval challenge.
func approve(t *testing.T, v *passkey.Verifier, cred passkey.Credential, a *authenticator, opts ...ceremonyOption) (passkey.Credential, error) {
	t.Helper()
	challenge := newChallenge(t)
	allowed := []passkey.Credential{cred}
	options, err := v.BeginAssertion(challenge, allowed)
	require.NoError(t, err)
	return v.FinishAssertion(challenge, allowed, a.get(t, options, testOrigin, opts...))
}

// requireRefused checks the sentinel and that the refusal names the check
// that made it, in the message or the library's detail, so a row cannot pass
// by failing for some other reason.
func requireRefused(t *testing.T, err, want error, reason string) {
	t.Helper()
	require.ErrorIs(t, err, want)
	require.Contains(t, err.Error()+" | "+passkey.DebugInfo(err), reason)
}

func TestNew_Config(t *testing.T) {
	cases := []struct {
		name string
		cfg  passkey.Config
		ok   bool
	}{
		{"localhost over http", passkey.Config{RPID: "localhost", Origins: []string{"http://localhost:8081"}}, true},
		{"localhost over https", passkey.Config{RPID: "localhost", Origins: []string{"https://localhost:8443"}}, true},
		{"domain and subdomain", passkey.Config{RPID: "example.com", Origins: []string{"https://example.com", "https://seamless.example.com:8443"}}, true},
		{"subdomain of localhost over http", passkey.Config{RPID: "localhost", Origins: []string{"http://console.localhost:8081"}}, true},
		{"no RP ID", passkey.Config{Origins: []string{"http://localhost:8081"}}, false},
		{"IPv4 RP ID", passkey.Config{RPID: "127.0.0.1", Origins: []string{"http://127.0.0.1:8081"}}, false},
		{"IPv6 RP ID", passkey.Config{RPID: "::1", Origins: []string{"http://[::1]:8081"}}, false},
		{"RP ID with a port", passkey.Config{RPID: "localhost:8081", Origins: []string{"http://localhost:8081"}}, false},
		{"RP ID with a scheme", passkey.Config{RPID: "https://example.com", Origins: []string{"https://example.com"}}, false},
		{"uppercase RP ID", passkey.Config{RPID: "Example.com", Origins: []string{"https://example.com"}}, false},
		{"no origins", passkey.Config{RPID: "localhost"}, false},
		{"IP origin under a localhost RP ID", passkey.Config{RPID: "localhost", Origins: []string{"http://127.0.0.1:8081"}}, false},
		{"http off localhost", passkey.Config{RPID: "example.com", Origins: []string{"http://example.com"}}, false},
		{"origin of another domain", passkey.Config{RPID: "example.com", Origins: []string{"https://example.org"}}, false},
		{"origin that only ends with the RP ID", passkey.Config{RPID: "example.com", Origins: []string{"https://notexample.com"}}, false},
		{"origin with a path", passkey.Config{RPID: "localhost", Origins: []string{"http://localhost:8081/console"}}, false},
		{"origin with a trailing slash", passkey.Config{RPID: "localhost", Origins: []string{"http://localhost:8081/"}}, false},
		{"origin with userinfo", passkey.Config{RPID: "localhost", Origins: []string{"http://owner@localhost:8081"}}, false},
		{"origin with another scheme", passkey.Config{RPID: "localhost", Origins: []string{"ftp://localhost"}}, false},
		{"origin with no scheme", passkey.Config{RPID: "localhost", Origins: []string{"localhost:8081"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, err := passkey.New(tc.cfg)
			if tc.ok {
				require.NoError(t, err)
				require.Equal(t, tc.cfg.RPID, v.RPID())
				return
			}
			require.ErrorIs(t, err, passkey.ErrInvalidConfig)
		})
	}
}

func TestBeginRegistration_Options(t *testing.T) {
	v := newVerifier(t)
	user := newOwner(t)
	existing := register(t, v, user, newAuthenticator(t, passkey.AlgES256, true))

	options, state, err := v.BeginRegistration(user, []passkey.Credential{existing})
	require.NoError(t, err)

	var o creationOptions
	require.NoError(t, json.Unmarshal(options, &o))
	pk := o.PublicKey
	require.Equal(t, "localhost", pk.RP.ID)
	require.Equal(t, "Seamless", pk.RP.Name)
	require.Equal(t, user.Handle, []byte(pk.User.ID))
	require.Equal(t, "owner", pk.User.Name)
	require.Equal(t, "Owner", pk.User.DisplayName)
	require.Len(t, pk.Challenge, passkey.ChallengeBytes)
	require.Equal(t, []byte(pk.Challenge), state.Challenge)
	require.Equal(t, user.Handle, state.UserHandle)
	require.Equal(t, "none", pk.Attestation)

	var algs []int64
	for _, p := range pk.PubKeyCredParams {
		require.Equal(t, "public-key", p.Type)
		algs = append(algs, p.Alg)
	}
	require.Equal(t, []int64{passkey.AlgES256, passkey.AlgEdDSA, passkey.AlgRS256}, algs)

	sel := pk.AuthenticatorSelection
	require.Equal(t, "preferred", sel["residentKey"])
	require.Equal(t, false, sel["requireResidentKey"])
	require.Equal(t, "required", sel["userVerification"])
	require.NotContains(t, sel, "authenticatorAttachment", "no attachment restriction")

	require.Len(t, pk.ExcludeCredentials, 1)
	require.Equal(t, existing.ID, []byte(pk.ExcludeCredentials[0].ID))

	_, again, err := v.BeginRegistration(user, nil)
	require.NoError(t, err)
	require.NotEqual(t, state.Challenge, again.Challenge, "every registration has its own challenge")
}

func TestBeginAssertion_Options(t *testing.T) {
	v := newVerifier(t)
	user := newOwner(t)
	a := newAuthenticator(t, passkey.AlgES256, true)
	b := newAuthenticator(t, passkey.AlgEdDSA, false)
	credA, credB := register(t, v, user, a), register(t, v, user, b)

	challenge := newChallenge(t)
	options, err := v.BeginAssertion(challenge, []passkey.Credential{credA, credB})
	require.NoError(t, err)

	var o requestOptions
	require.NoError(t, json.Unmarshal(options, &o))
	require.Equal(t, challenge, []byte(o.PublicKey.Challenge), "the approval's own challenge is the one signed")
	require.Equal(t, "localhost", o.PublicKey.RPID)
	require.Equal(t, "required", o.PublicKey.UserVerification)
	require.Len(t, o.PublicKey.AllowCredentials, 2)
	require.Equal(t, credA.ID, []byte(o.PublicKey.AllowCredentials[0].ID))
	require.Equal(t, credA.Transports, o.PublicKey.AllowCredentials[0].Transports)
	require.Equal(t, credB.ID, []byte(o.PublicKey.AllowCredentials[1].ID))
}

// TestCeremonies_Algorithms registers and asserts with every accepted
// algorithm, as a counting security key and as a synced passkey, with the
// authenticator verifying the user and without.
func TestCeremonies_Algorithms(t *testing.T) {
	algs := []struct {
		name string
		alg  int64
	}{{"ES256", passkey.AlgES256}, {"EdDSA", passkey.AlgEdDSA}, {"RS256", passkey.AlgRS256}}
	for _, alg := range algs {
		for _, counting := range []bool{true, false} {
			kind := "synced passkey"
			if counting {
				kind = "security key"
			}
			t.Run(alg.name+"/"+kind+"/uv on", func(t *testing.T) {
				v := newVerifier(t)
				user := newOwner(t)
				a := newAuthenticator(t, alg.alg, counting)
				cred := register(t, v, user, a)

				require.Equal(t, a.credID, cred.ID)
				require.Equal(t, alg.alg, cred.Algorithm)
				require.Equal(t, a.coseKey(t), cred.PublicKey)
				require.Equal(t, user.Handle, cred.UserHandle)
				require.Equal(t, uint32(0), cred.SignCount)
				require.Equal(t, a.aaguid, cred.AAGUID)
				require.Equal(t, a.transports, cred.Transports)
				require.Equal(t, a.attachment, cred.Attachment)
				require.Equal(t, "none", cred.AttestationFormat)
				require.Equal(t, !counting, cred.BackupEligible)
				require.Equal(t, !counting, cred.BackupState)

				for i := uint32(1); i <= 2; i++ {
					used, err := approve(t, v, cred, a)
					require.NoError(t, err)
					require.Equal(t, cred.ID, used.ID)
					want := uint32(0)
					if counting {
						want = i
					}
					require.Equal(t, want, used.SignCount)
					cred = used
				}
			})
			t.Run(alg.name+"/"+kind+"/uv off", func(t *testing.T) {
				v := newVerifier(t)
				user := newOwner(t)

				options, state, err := v.BeginRegistration(user, nil)
				require.NoError(t, err)
				unverified := newAuthenticator(t, alg.alg, counting)
				_, err = v.FinishRegistration(user, state, unverified.create(t, options, testOrigin, withoutFlag(flagUV)))
				require.ErrorIs(t, err, passkey.ErrUserVerification)

				a := newAuthenticator(t, alg.alg, counting)
				cred := register(t, v, user, a)
				_, err = approve(t, v, cred, a, withoutFlag(flagUV))
				require.ErrorIs(t, err, passkey.ErrUserVerification)
			})
		}
	}
}

func TestFinishRegistration_Refused(t *testing.T) {
	weak, err := rsa.GenerateKey(rand.Reader, 1024)
	require.NoError(t, err)

	cases := []struct {
		name   string
		reason string
		auth   func(t *testing.T) *authenticator
		opts   []ceremonyOption
		want   error
	}{
		{name: "rpIdHash of another RP ID", reason: "RP Hash mismatch", opts: []ceremonyOption{withRPID("example.com")}, want: passkey.ErrVerification},
		{name: "origin not configured", reason: "Error validating origin", opts: []ceremonyOption{withOrigin("http://localhost:9999")}, want: passkey.ErrVerification},
		{name: "clientData type of an assertion", reason: "Error validating ceremony type", opts: []ceremonyOption{withType("webauthn.get")}, want: passkey.ErrVerification},
		{name: "another challenge", reason: "Error validating challenge", opts: []ceremonyOption{withChallenge(make([]byte, passkey.ChallengeBytes))}, want: passkey.ErrVerification},
		{name: "UV flag missing", reason: "did not verify the user", opts: []ceremonyOption{withoutFlag(flagUV)}, want: passkey.ErrUserVerification},
		{name: "UP flag missing", reason: "user presence flag", opts: []ceremonyOption{withoutFlag(flagUP)}, want: passkey.ErrVerification},
		{
			name:   "algorithm not offered (ES384)",
			reason: "algorithm -35",
			auth:   func(t *testing.T) *authenticator { return newAuthenticator(t, algES384, true) },
			want:   passkey.ErrUnsupportedKey,
		},
		{
			name:   "RSA key under 2048 bits",
			reason: "1024 bits",
			auth:   func(t *testing.T) *authenticator { return newAuthenticatorWithKey(t, passkey.AlgRS256, weak, true) },
			want:   passkey.ErrWeakKey,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := newVerifier(t)
			user := newOwner(t)
			a := newAuthenticator(t, passkey.AlgES256, true)
			if tc.auth != nil {
				a = tc.auth(t)
			}
			options, state, err := v.BeginRegistration(user, nil)
			require.NoError(t, err)
			_, err = v.FinishRegistration(user, state, a.create(t, options, testOrigin, tc.opts...))
			requireRefused(t, err, tc.want, tc.reason)
		})
	}
}

func TestFinishRegistration_BadInput(t *testing.T) {
	v := newVerifier(t)
	user := newOwner(t)
	a := newAuthenticator(t, passkey.AlgES256, true)
	options, state, err := v.BeginRegistration(user, nil)
	require.NoError(t, err)
	response := a.create(t, options, testOrigin)

	t.Run("state of another user", func(t *testing.T) {
		other := newOwner(t)
		_, err := v.FinishRegistration(other, state, response)
		require.ErrorIs(t, err, passkey.ErrVerification)
	})
	t.Run("state challenge not 32 bytes", func(t *testing.T) {
		short := state
		short.Challenge = state.Challenge[:16]
		_, err := v.FinishRegistration(user, short, response)
		require.ErrorIs(t, err, passkey.ErrChallengeLength)
	})
	t.Run("not JSON", func(t *testing.T) {
		_, err := v.FinishRegistration(user, state, []byte("{"))
		require.ErrorIs(t, err, passkey.ErrVerification)
	})
	t.Run("over the size cap", func(t *testing.T) {
		_, err := v.FinishRegistration(user, state, []byte(strings.Repeat(" ", passkey.MaxResponseBytes+1)))
		require.ErrorIs(t, err, passkey.ErrVerification)
	})
	t.Run("the state survives JSON", func(t *testing.T) {
		raw, err := json.Marshal(state)
		require.NoError(t, err)
		var decoded passkey.RegistrationState
		require.NoError(t, json.Unmarshal(raw, &decoded))
		cred, err := v.FinishRegistration(user, decoded, response)
		require.NoError(t, err)
		require.Equal(t, a.credID, cred.ID)
	})
}

func TestUser_Validation(t *testing.T) {
	v := newVerifier(t)
	cases := []struct {
		name string
		user passkey.User
	}{
		{"no handle", passkey.User{Name: "owner"}},
		{"handle over 64 bytes", passkey.User{Handle: make([]byte, 65), Name: "owner"}},
		{"no name", passkey.User{Handle: []byte{1}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := v.BeginRegistration(tc.user, nil)
			require.ErrorIs(t, err, passkey.ErrInvalidUser)
			_, err = v.FinishRegistration(tc.user, passkey.RegistrationState{}, nil)
			require.ErrorIs(t, err, passkey.ErrInvalidUser)
		})
	}
}

func TestChallengeLength(t *testing.T) {
	v := newVerifier(t)
	user := newOwner(t)
	a := newAuthenticator(t, passkey.AlgES256, true)
	cred := register(t, v, user, a)
	for _, n := range []int{0, 16, 31, 33, 64} {
		challenge := make([]byte, n)
		_, err := v.BeginAssertion(challenge, []passkey.Credential{cred})
		require.ErrorIs(t, err, passkey.ErrChallengeLength, "begin with %d bytes", n)
		_, err = v.FinishAssertion(challenge, []passkey.Credential{cred}, []byte("{}"))
		require.ErrorIs(t, err, passkey.ErrChallengeLength, "finish with %d bytes", n)
	}
}

func TestBeginAssertion_NoCredentials(t *testing.T) {
	v := newVerifier(t)
	_, err := v.BeginAssertion(newChallenge(t), nil)
	require.ErrorIs(t, err, passkey.ErrNoCredentials)
	_, err = v.BeginAssertion(newChallenge(t), []passkey.Credential{{}})
	require.ErrorIs(t, err, passkey.ErrInvalidCredential)
}

func TestFinishAssertion_Refused(t *testing.T) {
	ed := newAuthenticator(t, passkey.AlgEdDSA, true)
	es := newAuthenticator(t, passkey.AlgES256, true)
	weak, err := rsa.GenerateKey(rand.Reader, 1024)
	require.NoError(t, err)

	cases := []struct {
		name   string
		reason string
		alg    int64
		opts   []ceremonyOption
		want   error
	}{
		{name: "rpIdHash of another RP ID", reason: "RP Hash mismatch", opts: []ceremonyOption{withRPID("example.com")}, want: passkey.ErrVerification},
		{name: "origin not configured", reason: "Error validating origin", opts: []ceremonyOption{withOrigin("http://localhost:9999")}, want: passkey.ErrVerification},
		{name: "clientData type of a registration", reason: "Error validating ceremony type", opts: []ceremonyOption{withType("webauthn.create")}, want: passkey.ErrVerification},
		{name: "another challenge", reason: "Error validating challenge", opts: []ceremonyOption{withChallenge(make([]byte, passkey.ChallengeBytes))}, want: passkey.ErrVerification},
		{name: "UV flag missing", reason: "did not verify the user", opts: []ceremonyOption{withoutFlag(flagUV)}, want: passkey.ErrUserVerification},
		{name: "UP flag missing", reason: "user presence flag", opts: []ceremonyOption{withoutFlag(flagUP)}, want: passkey.ErrVerification},
		{name: "bad signature", reason: "assertion signature", opts: []ceremonyOption{withCorruptSignature()}, want: passkey.ErrVerification},
		{name: "ES256 credential, Ed25519 signature", reason: "assertion signature", alg: passkey.AlgES256, opts: []ceremonyOption{withSigner(ed)}, want: passkey.ErrVerification},
		{name: "EdDSA credential, ES256 signature", reason: "assertion signature", alg: passkey.AlgEdDSA, opts: []ceremonyOption{withSigner(es)}, want: passkey.ErrVerification},
		{name: "RS256 credential, ES256 signature", reason: "assertion signature", alg: passkey.AlgRS256, opts: []ceremonyOption{withSigner(es)}, want: passkey.ErrVerification},
		{name: "userHandle of another user", reason: "User handle and User ID do not match", opts: []ceremonyOption{withUserHandle([]byte("someone-else"))}, want: passkey.ErrVerification},
		{name: "backup eligibility changed", reason: "Backup Eligible flag inconsistency", opts: []ceremonyOption{withFlag(flagBE)}, want: passkey.ErrVerification},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			alg := tc.alg
			if alg == 0 {
				alg = passkey.AlgES256
			}
			v := newVerifier(t)
			a := newAuthenticator(t, alg, true)
			cred := register(t, v, newOwner(t), a)
			_, err := approve(t, v, cred, a, tc.opts...)
			requireRefused(t, err, tc.want, tc.reason)
		})
	}

	t.Run("stored RSA key under 2048 bits", func(t *testing.T) {
		v := newVerifier(t)
		user := newOwner(t)
		a := newAuthenticatorWithKey(t, passkey.AlgRS256, weak, true)
		a.userHandle = user.Handle
		// Registration refuses this key, so the record is written by hand:
		// a row from before the floor, or a tampered one.
		cred := passkey.Credential{ID: a.credID, PublicKey: a.coseKey(t), Algorithm: passkey.AlgRS256, UserHandle: user.Handle}
		_, err := approve(t, v, cred, a)
		require.ErrorIs(t, err, passkey.ErrWeakKey)
	})
	t.Run("stored key of an algorithm not accepted", func(t *testing.T) {
		v := newVerifier(t)
		user := newOwner(t)
		a := newAuthenticator(t, algES384, true)
		a.userHandle = user.Handle
		cred := passkey.Credential{ID: a.credID, PublicKey: a.coseKey(t), Algorithm: algES384, UserHandle: user.Handle}
		_, err := approve(t, v, cred, a)
		require.ErrorIs(t, err, passkey.ErrUnsupportedKey)
	})
	t.Run("stored algorithm disagrees with the key", func(t *testing.T) {
		v := newVerifier(t)
		a := newAuthenticator(t, passkey.AlgES256, true)
		cred := register(t, v, newOwner(t), a)
		cred.Algorithm = passkey.AlgEdDSA
		_, err := approve(t, v, cred, a)
		require.ErrorIs(t, err, passkey.ErrInvalidCredential)
	})
	t.Run("credential not allowed when finishing", func(t *testing.T) {
		v := newVerifier(t)
		user := newOwner(t)
		a, b := newAuthenticator(t, passkey.AlgES256, true), newAuthenticator(t, passkey.AlgES256, true)
		credA, credB := register(t, v, user, a), register(t, v, user, b)
		challenge := newChallenge(t)
		options, err := v.BeginAssertion(challenge, []passkey.Credential{credA, credB})
		require.NoError(t, err)
		// credB was removed between the two requests.
		_, err = v.FinishAssertion(challenge, []passkey.Credential{credA}, b.get(t, options, testOrigin))
		require.ErrorIs(t, err, passkey.ErrUnknownCredential)
	})
	t.Run("not JSON", func(t *testing.T) {
		v := newVerifier(t)
		a := newAuthenticator(t, passkey.AlgES256, true)
		cred := register(t, v, newOwner(t), a)
		_, err := v.FinishAssertion(newChallenge(t), []passkey.Credential{cred}, []byte(`{"id":`))
		require.ErrorIs(t, err, passkey.ErrVerification)
	})
	t.Run("over the size cap", func(t *testing.T) {
		v := newVerifier(t)
		a := newAuthenticator(t, passkey.AlgES256, true)
		cred := register(t, v, newOwner(t), a)
		_, err := v.FinishAssertion(newChallenge(t), []passkey.Credential{cred}, []byte(strings.Repeat(" ", passkey.MaxResponseBytes+1)))
		require.ErrorIs(t, err, passkey.ErrVerification)
	})
}

// TestFinishAssertion_AnotherApprovalsChallenge is the property the approval
// flow rests on: an assertion made for one approval does not verify against
// another approval's challenge, though it is otherwise valid.
func TestFinishAssertion_AnotherApprovalsChallenge(t *testing.T) {
	v := newVerifier(t)
	a := newAuthenticator(t, passkey.AlgES256, false)
	cred := register(t, v, newOwner(t), a)
	allowed := []passkey.Credential{cred}

	first, second := newChallenge(t), newChallenge(t)
	_, err := v.BeginAssertion(first, allowed)
	require.NoError(t, err)
	optionsSecond, err := v.BeginAssertion(second, allowed)
	require.NoError(t, err)
	response := a.get(t, optionsSecond, testOrigin)

	_, err = v.FinishAssertion(first, allowed, response)
	require.ErrorIs(t, err, passkey.ErrVerification)
	require.Contains(t, passkey.DebugInfo(err), "Expected", "the library's detail says what it expected")

	_, err = v.FinishAssertion(second, allowed, response)
	require.NoError(t, err, "the same response verifies against its own approval")
}

// TestFinishAssertion_Replay submits the same assertion twice. A counting
// authenticator's replay fails the counter rule here. A synced passkey
// reports 0 every time, so its replay verifies here, and it is the store's
// single use of the approval's challenge (store.ApproveAccess decides a
// pending approval once) that refuses it.
func TestFinishAssertion_Replay(t *testing.T) {
	t.Run("counting authenticator: the counter refuses it", func(t *testing.T) {
		v := newVerifier(t)
		a := newAuthenticator(t, passkey.AlgES256, true)
		cred := register(t, v, newOwner(t), a)
		challenge := newChallenge(t)
		options, err := v.BeginAssertion(challenge, []passkey.Credential{cred})
		require.NoError(t, err)
		response := a.get(t, options, testOrigin)

		used, err := v.FinishAssertion(challenge, []passkey.Credential{cred}, response)
		require.NoError(t, err)
		require.Equal(t, uint32(1), used.SignCount)

		_, err = v.FinishAssertion(challenge, []passkey.Credential{used}, response)
		require.ErrorIs(t, err, passkey.ErrSignCount)
	})
	t.Run("synced passkey: verifies here, the store refuses it", func(t *testing.T) {
		v := newVerifier(t)
		a := newAuthenticator(t, passkey.AlgEdDSA, false)
		cred := register(t, v, newOwner(t), a)
		challenge := newChallenge(t)
		options, err := v.BeginAssertion(challenge, []passkey.Credential{cred})
		require.NoError(t, err)
		response := a.get(t, options, testOrigin)

		used, err := v.FinishAssertion(challenge, []passkey.Credential{cred}, response)
		require.NoError(t, err)
		_, err = v.FinishAssertion(challenge, []passkey.Credential{used}, response)
		require.NoError(t, err)
	})
}

func TestFinishAssertion_SignCountRule(t *testing.T) {
	cases := []struct {
		name     string
		stored   uint32
		reported uint32
		want     uint32
		err      error
	}{
		{name: "0 then 0 (synced passkey)", stored: 0, reported: 0, want: 0},
		{name: "0 then non-zero", stored: 0, reported: 7, want: 7},
		{name: "increasing", stored: 5, reported: 6, want: 6},
		{name: "equal", stored: 5, reported: 5, err: passkey.ErrSignCount},
		{name: "decreasing", stored: 5, reported: 3, err: passkey.ErrSignCount},
		{name: "non-zero then 0 (a counting key cannot stop counting)", stored: 5, reported: 0, err: passkey.ErrSignCount},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := newVerifier(t)
			a := newAuthenticator(t, passkey.AlgES256, true)
			cred := register(t, v, newOwner(t), a)
			cred.SignCount = tc.stored
			used, err := approve(t, v, cred, a, withCounter(tc.reported))
			if tc.err != nil {
				require.ErrorIs(t, err, tc.err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, used.SignCount)
		})
	}
}

func TestFinishAssertion_BackupStateUpdated(t *testing.T) {
	v := newVerifier(t)
	a := newAuthenticator(t, passkey.AlgES256, false)
	cred := register(t, v, newOwner(t), a)
	require.True(t, cred.BackupState)
	used, err := approve(t, v, cred, a, withoutFlag(flagBS))
	require.NoError(t, err)
	require.True(t, used.BackupEligible)
	require.False(t, used.BackupState, "BS is reported as last seen")
}

func TestDebugInfo(t *testing.T) {
	require.Empty(t, passkey.DebugInfo(nil))
	require.Empty(t, passkey.DebugInfo(errors.New("plain")))

	v := newVerifier(t)
	a := newAuthenticator(t, passkey.AlgES256, true)
	cred := register(t, v, newOwner(t), a)
	_, err := approve(t, v, cred, a, withOrigin("http://localhost:9999"))
	require.ErrorIs(t, err, passkey.ErrVerification)
	require.Contains(t, passkey.DebugInfo(err), "http://localhost:9999")
	require.NotContains(t, err.Error(), "localhost:9999", "the detail stays out of the message")
}

// TestVerifier_Concurrent finishes assertions from several goroutines on one
// Verifier, for the race detector.
func TestVerifier_Concurrent(t *testing.T) {
	v := newVerifier(t)
	user := newOwner(t)
	const n = 8
	type job struct {
		challenge []byte
		cred      passkey.Credential
		response  []byte
	}
	jobs := make([]job, n)
	for i := range jobs {
		a := newAuthenticator(t, passkey.AlgES256, i%2 == 0)
		cred := register(t, v, user, a)
		challenge := newChallenge(t)
		options, err := v.BeginAssertion(challenge, []passkey.Credential{cred})
		require.NoError(t, err)
		jobs[i] = job{challenge: challenge, cred: cred, response: a.get(t, options, testOrigin)}
	}
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Go(func() {
			_, errs[i] = v.FinishAssertion(j.challenge, []passkey.Credential{j.cred}, j.response)
		})
	}
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "job %d", i)
	}
}

// AssertionChallenge reads the challenge a response claims it signed, and
// verifies nothing: it explains a refusal, never makes a decision.
func TestAssertionChallenge(t *testing.T) {
	v := newVerifier(t)
	a := newAuthenticator(t, passkey.AlgES256, true)
	cred := register(t, v, newOwner(t), a)
	challenge := newChallenge(t)
	options, err := v.BeginAssertion(challenge, []passkey.Credential{cred})
	require.NoError(t, err)

	got, err := passkey.AssertionChallenge(a.get(t, options, testOrigin))
	require.NoError(t, err)
	require.Equal(t, challenge, got)

	other := newChallenge(t)
	got, err = passkey.AssertionChallenge(a.get(t, options, testOrigin, withChallenge(other), withCorruptSignature()))
	require.NoError(t, err, "a response that would not verify still names its challenge")
	require.Equal(t, other, got)

	for name, bad := range map[string][]byte{
		"empty":    nil,
		"not json": []byte("{"),
		"too long": []byte(strings.Repeat(" ", passkey.MaxResponseBytes+1)),
	} {
		_, err := passkey.AssertionChallenge(bad)
		require.ErrorIs(t, err, passkey.ErrVerification, name)
	}
}

// A caller that keeps no state between the two requests names the challenge
// itself (BeginRegistrationWith): the options carry it, the state carries it,
// and a response over any other challenge fails, as for the random one.
func TestBeginRegistrationWith_TheCallersChallenge(t *testing.T) {
	v := newVerifier(t)
	user := newOwner(t)
	challenge := newChallenge(t)
	options, state, err := v.BeginRegistrationWith(user, nil, challenge)
	require.NoError(t, err)
	require.Equal(t, challenge, state.Challenge)
	var o struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
		} `json:"publicKey"`
	}
	require.NoError(t, json.Unmarshal(options, &o))
	require.NotEmpty(t, o.PublicKey.Challenge)

	a := newAuthenticator(t, passkey.AlgES256, false)
	cred, err := v.FinishRegistration(user, passkey.RegistrationState{Challenge: challenge, UserHandle: user.Handle}, a.create(t, options, testOrigin))
	require.NoError(t, err, "a state rebuilt from the same challenge finishes")
	require.NotEmpty(t, cred.ID)

	other := newChallenge(t)
	b := newAuthenticator(t, passkey.AlgES256, false)
	_, err = v.FinishRegistration(user, passkey.RegistrationState{Challenge: other, UserHandle: user.Handle}, b.create(t, options, testOrigin))
	require.ErrorIs(t, err, passkey.ErrVerification, "a response over another challenge fails")

	for _, n := range []int{0, 16, 33} {
		_, _, err := v.BeginRegistrationWith(user, nil, make([]byte, n))
		require.ErrorIs(t, err, passkey.ErrChallengeLength, "%d bytes", n)
	}
}
