package passkey

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

// RegistrationState is what FinishRegistration needs from BeginRegistration:
// the challenge the authenticator must sign and the user it is for. The
// caller keeps it server side between the two requests (it serializes as
// JSON) and uses it once; handing it to the client would let the client pick
// its own challenge.
type RegistrationState struct {
	Challenge  []byte `json:"challenge"`
	UserHandle []byte `json:"user_handle"`
}

// BeginRegistration starts registering a passkey for user. It returns the
// options for navigator.credentials.create as JSON ({"publicKey": {...}},
// binary fields base64url, ready for
// PublicKeyCredential.parseCreationOptionsFromJSON) and the state to finish
// with. exclude lists the credentials already registered, so an authenticator
// that holds one of them declines to make a second.
func (v *Verifier) BeginRegistration(user User, exclude []Credential) ([]byte, RegistrationState, error) {
	const op = "passkey.BeginRegistration"
	if err := user.validate(); err != nil {
		return nil, RegistrationState{}, fmt.Errorf("%s: %w", op, err)
	}
	excluded := make([]protocol.CredentialDescriptor, len(exclude))
	for i, c := range exclude {
		lc := c.toLibrary()
		excluded[i] = lc.Descriptor()
	}
	creation, _, err := v.wa.BeginRegistration(newLibUser(user),
		webauthn.WithCredentialParameters(credentialParameters()),
		webauthn.WithAuthenticatorSelection(authenticatorSelection()),
		webauthn.WithConveyancePreference(protocol.PreferNoAttestation),
		webauthn.WithExclusions(excluded),
	)
	if err != nil {
		return nil, RegistrationState{}, fmt.Errorf("%s: %w", op, err)
	}
	challenge := []byte(creation.Response.Challenge)
	if err := checkChallenge(challenge); err != nil {
		return nil, RegistrationState{}, fmt.Errorf("%s: generated: %w", op, err)
	}
	options, err := json.Marshal(creation)
	if err != nil {
		return nil, RegistrationState{}, fmt.Errorf("%s: %w", op, err)
	}
	return options, RegistrationState{Challenge: slices.Clone(challenge), UserHandle: slices.Clone(user.Handle)}, nil
}

// FinishRegistration verifies the client's response to BeginRegistration
// (the JSON of PublicKeyCredential.toJSON()) and returns the new credential
// to store. The caller still refuses a credential id it already stores.
func (v *Verifier) FinishRegistration(user User, state RegistrationState, responseJSON []byte) (Credential, error) {
	const op = "passkey.FinishRegistration"
	if err := user.validate(); err != nil {
		return Credential{}, fmt.Errorf("%s: %w", op, err)
	}
	if err := checkChallenge(state.Challenge); err != nil {
		return Credential{}, fmt.Errorf("%s: %w", op, err)
	}
	if !bytes.Equal(state.UserHandle, user.Handle) {
		return Credential{}, fmt.Errorf("%s: %w: the registration was begun for another user", op, ErrVerification)
	}
	if len(responseJSON) > MaxResponseBytes {
		return Credential{}, fmt.Errorf("%s: %w: the response is over %d bytes", op, ErrVerification, MaxResponseBytes)
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes(responseJSON)
	if err != nil {
		return Credential{}, fmt.Errorf("%s: %w: %w", op, ErrVerification, err)
	}
	authData := parsed.Response.AttestationObject.AuthData
	if err := checkFlags(authData.Flags); err != nil {
		return Credential{}, fmt.Errorf("%s: %w", op, err)
	}
	alg, err := checkPublicKey(authData.AttData.CredentialPublicKey)
	if err != nil {
		return Credential{}, fmt.Errorf("%s: %w", op, err)
	}
	session := webauthn.SessionData{
		Challenge:        protocol.URLEncodedBase64(state.Challenge).String(),
		RelyingPartyID:   v.cfg.RPID,
		UserID:           user.Handle,
		UserVerification: protocol.VerificationRequired,
		CredParams:       credentialParameters(),
	}
	lc, err := v.wa.CreateCredential(newLibUser(user), session, parsed)
	if err != nil {
		return Credential{}, fmt.Errorf("%s: %w: %w", op, ErrVerification, err)
	}
	return fromLibrary(*lc, alg, user.Handle), nil
}
