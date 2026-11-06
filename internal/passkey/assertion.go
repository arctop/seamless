package passkey

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

// BeginAssertion returns the options for navigator.credentials.get as JSON
// ({"publicKey": {...}}, binary fields base64url, ready for
// PublicKeyCredential.parseRequestOptionsFromJSON): the approval's challenge
// (exactly ChallengeBytes), user verification required, and the allowed
// credentials listed by id so a non-discoverable credential can sign too.
//
// There is no state to keep: FinishAssertion takes the same challenge, which
// the caller reads from the approval being decided.
func (v *Verifier) BeginAssertion(challenge []byte, allowed []Credential) ([]byte, error) {
	const op = "passkey.BeginAssertion"
	if err := checkChallenge(challenge); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	if len(allowed) == 0 {
		return nil, fmt.Errorf("%s: %w", op, ErrNoCredentials)
	}
	creds := make([]webauthn.Credential, len(allowed))
	for i, c := range allowed {
		if len(c.ID) == 0 {
			return nil, fmt.Errorf("%s: %w: no id", op, ErrInvalidCredential)
		}
		creds[i] = c.toLibrary()
	}
	// The user handle only feeds the library's session record, which is
	// discarded: FinishAssertion rebuilds it from the challenge.
	assertion, _, err := v.wa.BeginLogin(libUser{handle: allowed[0].UserHandle, creds: creds},
		webauthn.WithChallenge(bytes.Clone(challenge)),
		webauthn.WithUserVerification(protocol.VerificationRequired),
	)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	options, err := json.Marshal(assertion)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	return options, nil
}

// FinishAssertion verifies the client's response (the JSON of
// PublicKeyCredential.toJSON()) against challenge, which must be the
// approval's own: an assertion signed over any other challenge fails. allowed
// are the credentials that may approve; the one that signed is returned with
// its signature counter and backup state updated, for the caller to store.
//
// A verified assertion approves nothing by itself: the caller still decides
// the approval through the store, which accepts its challenge once.
func (v *Verifier) FinishAssertion(challenge []byte, allowed []Credential, responseJSON []byte) (Credential, error) {
	const op = "passkey.FinishAssertion"
	if err := checkChallenge(challenge); err != nil {
		return Credential{}, fmt.Errorf("%s: %w", op, err)
	}
	if len(responseJSON) > MaxResponseBytes {
		return Credential{}, fmt.Errorf("%s: %w: the response is over %d bytes", op, ErrVerification, MaxResponseBytes)
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(responseJSON)
	if err != nil {
		return Credential{}, fmt.Errorf("%s: %w: %w", op, ErrVerification, err)
	}
	stored, ok := findCredential(allowed, parsed.RawID)
	if !ok {
		return Credential{}, fmt.Errorf("%s: %w", op, ErrUnknownCredential)
	}
	if err := stored.validate(); err != nil {
		return Credential{}, fmt.Errorf("%s: %w", op, err)
	}
	if err := checkFlags(parsed.Response.AuthenticatorData.Flags); err != nil {
		return Credential{}, fmt.Errorf("%s: %w", op, err)
	}

	// The session the library checks against is rebuilt here rather than
	// kept from BeginAssertion: the challenge is the caller's, the allowed
	// list is the one credential that answered, and the user is whoever that
	// credential was registered to (so a returned userHandle must match it).
	session := webauthn.SessionData{
		Challenge:            protocol.URLEncodedBase64(challenge).String(),
		RelyingPartyID:       v.cfg.RPID,
		UserID:               stored.UserHandle,
		AllowedCredentialIDs: [][]byte{stored.ID},
		UserVerification:     protocol.VerificationRequired,
	}
	user := libUser{handle: stored.UserHandle, creds: []webauthn.Credential{stored.toLibrary()}}
	lc, err := v.wa.ValidateLogin(user, session, parsed)
	if err != nil {
		return Credential{}, fmt.Errorf("%s: %w: %w", op, ErrVerification, err)
	}

	count, err := nextSignCount(stored.SignCount, parsed.Response.AuthenticatorData.Counter)
	if err != nil {
		return Credential{}, fmt.Errorf("%s: %w", op, err)
	}
	used := stored
	used.SignCount = count
	used.BackupState = lc.Flags.BackupState
	return used, nil
}

// nextSignCount applies the counter rule of WebAuthn section 7.2 step 22: a
// counter that does not increase is refused unless both it and the stored one
// are 0 -- synced passkeys never count, and report 0 every time. A counting
// authenticator that suddenly reports 0 is refused like any other drop: a
// clone of it could otherwise pass by reporting 0. The library only flags a
// possible clone; this is the rule Seamless enforces.
func nextSignCount(stored, got uint32) (uint32, error) {
	if (stored != 0 || got != 0) && got <= stored {
		return stored, fmt.Errorf("%w: stored %d, got %d", ErrSignCount, stored, got)
	}
	return got, nil
}

func findCredential(allowed []Credential, id []byte) (Credential, bool) {
	for _, c := range allowed {
		if len(c.ID) != 0 && bytes.Equal(c.ID, id) {
			return c, true
		}
	}
	return Credential{}, false
}
