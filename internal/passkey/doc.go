// Package passkey registers the owner's passkeys and verifies the assertions
// that approve access to a sealed project. It is a thin wrapper over
// github.com/go-webauthn/webauthn that fixes every WebAuthn setting Seamless
// relies on, keeps the credential record in a struct of its own (Credential)
// so the store never depends on the library's types, and turns the library's
// soft signals into errors. It imports nothing else from Seamless: the caller
// hands it the stored credentials and an approval's challenge, and stores what
// it returns.
//
// Attestation is "none". Seamless trusts no attestation: it never asks which
// make or model of authenticator made a credential, so any passkey the owner
// chooses can be registered (a platform authenticator, a security key with a
// PIN, a phone over hybrid, a password manager). If a client sends an
// attestation statement anyway, the library still checks that it is well
// formed and correctly signed, but nothing is trusted because of it.
//
// User verification is required on every ceremony, and the server checks the
// UV flag itself rather than trusting the browser to have asked: a passkey
// approves only after the authenticator verified the owner (biometric, PIN or
// device passcode), not merely that someone touched it. The UP (user present)
// flag is checked too. Both flags are checked here before the library runs,
// and again by the library.
//
// The accepted algorithms are ES256 (-7), EdDSA (-8) and RS256 (-257), the set
// WebAuthn Level 3 recommends, which covers every authenticator in use. An RSA
// key must have a modulus of at least 2048 bits. The library sets no floor on
// RSA key size, so this package enforces it, both when a credential is
// registered and every time a stored credential is used.
//
// No authenticator attachment is requested, so platform and roaming
// authenticators are both offered. Discoverable credentials (resident keys)
// are preferred, not required: a security key that is out of resident-key
// slots still registers, and assertions list the allowed credential ids, so a
// non-discoverable credential still signs.
//
// An assertion signs the approval's own challenge: 32 random bytes generated
// when the approval was created and stored with it. There is no separate
// assertion state to persist, because the challenge is the only per-ceremony
// value; FinishAssertion takes the challenge, so a caller that loads the
// approval by id and passes its challenge cannot verify a response made for
// another approval. A challenge that is not exactly 32 bytes is refused.
//
// The signature counter follows WebAuthn section 7.2 step 22: a counter that
// fails to increase is refused unless both the stored and the reported value
// are 0. Synced passkeys always report 0, so 0 after 0 is accepted, and so is
// a first non-zero value after 0. A credential that drops back to 0 after
// counting is refused: a clone of a counting key could otherwise pass by
// reporting 0. The library only raises a clone warning; this package turns
// the rule into ErrSignCount.
//
// A replayed assertion is caught in two layers. When the credential counts,
// the second use fails the counter rule here. When it does not (a synced
// passkey reports 0 every time), the replay verifies here, and it is the
// store's single use of the challenge (store.ApproveAccess decides a pending
// approval once) that refuses it.
//
// The relying party id is a registrable domain, never an IP address, and every
// origin must be https (or http on localhost, the one place browsers allow
// WebAuthn over plain http) on that domain or a subdomain of it. A console
// served on 127.0.0.1 therefore cannot use passkeys; it must be reached as
// localhost.
package passkey
