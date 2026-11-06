package passkey

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/protocol/webauthncose"
	"github.com/go-webauthn/webauthn/webauthn"
)

// ChallengeBytes is the exact size of every challenge this package signs or
// accepts: the size of an access approval's one-time challenge.
const ChallengeBytes = 32

// COSE algorithm identifiers of the accepted credential algorithms.
const (
	AlgES256 int64 = -7
	AlgEdDSA int64 = -8
	AlgRS256 int64 = -257
)

// MinRSABits is the smallest RSA modulus accepted, at registration and at
// every assertion.
const MinRSABits = 2048

// MaxResponseBytes caps the client's JSON response to either ceremony. A real
// response is a few kilobytes even with an attestation certificate chain.
const MaxResponseBytes = 64 << 10

// defaultRPName is the relying party name shown by the authenticator when
// Config.RPName is empty.
const defaultRPName = "Seamless"

var (
	// ErrInvalidConfig is a Config New refuses.
	ErrInvalidConfig = errors.New("invalid passkey configuration")
	// ErrInvalidUser is a User with no usable handle or name.
	ErrInvalidUser = errors.New("invalid passkey user")
	// ErrChallengeLength is a challenge that is not exactly ChallengeBytes long.
	ErrChallengeLength = errors.New("passkey challenge must be exactly 32 bytes")
	// ErrNoCredentials is an assertion begun with no credential to allow.
	ErrNoCredentials = errors.New("no passkey credentials to allow")
	// ErrInvalidCredential is a stored credential record that cannot be used
	// (no id, no user handle, or an algorithm that disagrees with its key).
	ErrInvalidCredential = errors.New("invalid passkey credential record")
	// ErrUnknownCredential is an assertion made by a credential that is not
	// among the allowed ones.
	ErrUnknownCredential = errors.New("unknown passkey credential")
	// ErrUnsupportedKey is a credential public key outside the accepted
	// algorithms (ES256, EdDSA, RS256), or one that does not parse.
	ErrUnsupportedKey = errors.New("passkey public key algorithm not accepted")
	// ErrWeakKey is an RSA credential key under MinRSABits.
	ErrWeakKey = errors.New("passkey RSA key is under 2048 bits")
	// ErrUserVerification is a response whose authenticator did not verify
	// the user (the UV flag is not set).
	ErrUserVerification = errors.New("passkey authenticator did not verify the user")
	// ErrSignCount is an assertion whose signature counter did not increase
	// while it or the stored counter is non-zero: a sign the credential may
	// have been cloned, or the assertion replayed.
	ErrSignCount = errors.New("passkey signature counter did not increase")
	// ErrVerification is any other failed check of a ceremony response:
	// parse errors, the challenge, the origin, the client data type, the
	// rpIdHash, the UP flag, the attestation statement or the signature. The
	// library's error is wrapped beside it; DebugInfo extracts its detail.
	ErrVerification = errors.New("passkey verification failed")
)

// Config names the relying party.
type Config struct {
	// RPID is the relying party id: a registrable domain such as
	// "localhost" or "example.com", lowercase, with no scheme, port or path.
	// An IP address is not a valid RP ID.
	RPID string
	// RPName is the name the authenticator shows; empty means "Seamless".
	RPName string
	// Origins are the exact origins a ceremony may come from, such as
	// "http://localhost:8081". Each is https, or http on localhost, and its
	// host is RPID or a subdomain of it.
	Origins []string
}

func (c Config) validate() error {
	if c.RPID == "" {
		return fmt.Errorf("%w: no RP ID", ErrInvalidConfig)
	}
	if err := protocol.ValidateRPID(c.RPID); err != nil {
		return fmt.Errorf("%w: RP ID %q: %w", ErrInvalidConfig, c.RPID, err)
	}
	if c.RPID != strings.ToLower(c.RPID) {
		return fmt.Errorf("%w: RP ID %q must be lowercase", ErrInvalidConfig, c.RPID)
	}
	if len(c.Origins) == 0 {
		return fmt.Errorf("%w: no origins", ErrInvalidConfig)
	}
	for _, o := range c.Origins {
		if err := validOrigin(o, c.RPID); err != nil {
			return fmt.Errorf("%w: origin %q: %w", ErrInvalidConfig, o, err)
		}
	}
	return nil
}

var (
	errOriginShape  = errors.New("an origin is scheme://host[:port] and nothing else")
	errOriginScheme = errors.New("the scheme must be https (or http on localhost)")
	errOriginHost   = errors.New("the host must be the RP ID or a subdomain of it")
)

func validOrigin(origin, rpID string) error {
	u, err := url.Parse(origin)
	if err != nil {
		return err
	}
	if u.Host == "" || u.Opaque != "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return errOriginShape
	}
	host := u.Hostname()
	if host != strings.ToLower(host) || net.ParseIP(host) != nil {
		return errOriginHost
	}
	if host != rpID && !strings.HasSuffix(host, "."+rpID) {
		return errOriginHost
	}
	switch u.Scheme {
	case "https":
	case "http":
		if host != "localhost" && !strings.HasSuffix(host, ".localhost") {
			return errOriginScheme
		}
	default:
		return errOriginScheme
	}
	return nil
}

// Verifier runs the registration and assertion ceremonies for one relying
// party. It is safe for concurrent use.
type Verifier struct {
	cfg Config
	wa  *webauthn.WebAuthn
}

// New validates cfg and returns a Verifier for it.
func New(cfg Config) (*Verifier, error) {
	if cfg.RPName == "" {
		cfg.RPName = defaultRPName
	}
	cfg.Origins = slices.Clone(cfg.Origins)
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("passkey.New: %w", err)
	}
	wa, err := webauthn.New(&webauthn.Config{
		RPID:                   cfg.RPID,
		RPDisplayName:          cfg.RPName,
		RPOrigins:              cfg.Origins,
		AttestationPreference:  protocol.PreferNoAttestation,
		AuthenticatorSelection: authenticatorSelection(),
	})
	if err != nil {
		return nil, fmt.Errorf("passkey.New: %w: %w", ErrInvalidConfig, err)
	}
	return &Verifier{cfg: cfg, wa: wa}, nil
}

// RPID returns the relying party id the Verifier was configured with.
func (v *Verifier) RPID() string { return v.cfg.RPID }

// authenticatorSelection is the one selection both ceremonies use: any
// attachment, discoverable credentials preferred, user verification required.
func authenticatorSelection() protocol.AuthenticatorSelection {
	return protocol.AuthenticatorSelection{
		ResidentKey:        protocol.ResidentKeyRequirementPreferred,
		RequireResidentKey: protocol.ResidentKeyNotRequired(),
		UserVerification:   protocol.VerificationRequired,
	}
}

// credentialParameters lists the accepted algorithms in preference order.
func credentialParameters() []protocol.CredentialParameter {
	algs := []int64{AlgES256, AlgEdDSA, AlgRS256}
	params := make([]protocol.CredentialParameter, len(algs))
	for i, alg := range algs {
		params[i] = protocol.CredentialParameter{
			Type:      protocol.PublicKeyCredentialType,
			Algorithm: webauthncose.COSEAlgorithmIdentifier(alg),
		}
	}
	return params
}

func checkChallenge(challenge []byte) error {
	if len(challenge) != ChallengeBytes {
		return fmt.Errorf("%w: got %d", ErrChallengeLength, len(challenge))
	}
	return nil
}

// DebugInfo returns the library's developer detail for a verification
// failure (what was expected and what was received), or "" when err carries
// none. It is for a log line: it can echo origins and challenges, so it never
// goes back to a client.
func DebugInfo(err error) string {
	var pe *protocol.Error
	if errors.As(err, &pe) {
		return pe.DevInfo
	}
	return ""
}

// User is the account a passkey is registered to: the owner.
type User struct {
	// Handle is the WebAuthn user handle: 1 to 64 random bytes, stable for
	// the owner, never personal data (the authenticator stores it and hands
	// it back with every discoverable assertion).
	Handle []byte
	// Name is the account name the authenticator shows (e.g. "owner").
	Name string
	// DisplayName is the friendly name the authenticator shows; empty means
	// Name.
	DisplayName string
}

func (u User) validate() error {
	if n := len(u.Handle); n < protocol.MinimumUserHandleLength || n > protocol.MaximumUserHandleLength {
		return fmt.Errorf("%w: the handle must be %d to %d bytes, got %d", ErrInvalidUser,
			protocol.MinimumUserHandleLength, protocol.MaximumUserHandleLength, n)
	}
	if strings.TrimSpace(u.Name) == "" {
		return fmt.Errorf("%w: no name", ErrInvalidUser)
	}
	return nil
}

// libUser adapts a user handle and its credentials to webauthn.User.
type libUser struct {
	handle      []byte
	name        string
	displayName string
	creds       []webauthn.Credential
}

func newLibUser(u User) libUser {
	display := u.DisplayName
	if display == "" {
		display = u.Name
	}
	return libUser{handle: u.Handle, name: u.Name, displayName: display}
}

func (u libUser) WebAuthnID() []byte                         { return u.handle }
func (u libUser) WebAuthnName() string                       { return u.name }
func (u libUser) WebAuthnDisplayName() string                { return u.displayName }
func (u libUser) WebAuthnCredentials() []webauthn.Credential { return u.creds }

// Credential is a registered passkey: everything needed to verify its later
// assertions, and nothing about its attestation (none is trusted, so none is
// kept). It is the record the store persists.
type Credential struct {
	// ID is the credential id the authenticator chose (at most 1023 bytes).
	ID []byte
	// PublicKey is the credential public key as a COSE_Key.
	PublicKey []byte
	// Algorithm is the COSE algorithm of PublicKey: AlgES256, AlgEdDSA or
	// AlgRS256.
	Algorithm int64
	// SignCount is the highest signature counter seen; 0 for an
	// authenticator that does not count (every synced passkey).
	SignCount uint32
	// AAGUID is the authenticator model id the authenticator reported (16
	// bytes, all zero when the client anonymized it). Display only.
	AAGUID []byte
	// Transports are the transports the client reported ("usb", "nfc",
	// "ble", "hybrid", "internal"), passed back as hints at assertion.
	Transports []string
	// Attachment is "platform", "cross-platform" or "" as the client
	// reported it at registration. Display only.
	Attachment string
	// BackupEligible is the BE flag: the credential can be synced. It is
	// fixed for the credential's life; an assertion that disagrees fails.
	BackupEligible bool
	// BackupState is the BS flag as last seen: the credential is synced.
	BackupState bool
	// UserHandle is the handle of the User it was registered to.
	UserHandle []byte
	// AttestationFormat is the attestation statement format the client sent
	// ("none" for almost every client when none is requested).
	AttestationFormat string
}

// validate checks a stored credential record before it is used to verify an
// assertion, including the key itself (algorithm and RSA size).
func (c Credential) validate() error {
	if len(c.ID) == 0 {
		return fmt.Errorf("%w: no id", ErrInvalidCredential)
	}
	if len(c.UserHandle) == 0 {
		return fmt.Errorf("%w: no user handle", ErrInvalidCredential)
	}
	alg, err := checkPublicKey(c.PublicKey)
	if err != nil {
		return err
	}
	if alg != c.Algorithm {
		return fmt.Errorf("%w: the record says algorithm %d but its key is %d", ErrInvalidCredential, c.Algorithm, alg)
	}
	return nil
}

// toLibrary converts c to the library's credential record. The flags the
// library reads are BE (an assertion must match it); UP and UV are set
// because registration required both.
func (c Credential) toLibrary() webauthn.Credential {
	flags := protocol.FlagUserPresent | protocol.FlagUserVerified
	if c.BackupEligible {
		flags |= protocol.FlagBackupEligible
	}
	if c.BackupState {
		flags |= protocol.FlagBackupState
	}
	transports := make([]protocol.AuthenticatorTransport, len(c.Transports))
	for i, t := range c.Transports {
		transports[i] = protocol.AuthenticatorTransport(t)
	}
	return webauthn.Credential{
		ID:                c.ID,
		PublicKey:         c.PublicKey,
		AttestationFormat: c.AttestationFormat,
		Transport:         transports,
		Flags:             webauthn.NewCredentialFlags(flags),
		Authenticator: webauthn.Authenticator{
			AAGUID:     c.AAGUID,
			SignCount:  c.SignCount,
			Attachment: protocol.AuthenticatorAttachment(c.Attachment),
		},
	}
}

// fromLibrary converts the library's record of a newly registered credential.
// alg is the algorithm checkPublicKey read from its key.
func fromLibrary(lc webauthn.Credential, alg int64, userHandle []byte) Credential {
	transports := make([]string, len(lc.Transport))
	for i, t := range lc.Transport {
		transports[i] = string(t)
	}
	return Credential{
		ID:                slices.Clone(lc.ID),
		PublicKey:         slices.Clone(lc.PublicKey),
		Algorithm:         alg,
		SignCount:         lc.Authenticator.SignCount,
		AAGUID:            slices.Clone(lc.Authenticator.AAGUID),
		Transports:        transports,
		Attachment:        string(lc.Authenticator.Attachment),
		BackupEligible:    lc.Flags.BackupEligible,
		BackupState:       lc.Flags.BackupState,
		UserHandle:        slices.Clone(userHandle),
		AttestationFormat: lc.AttestationFormat,
	}
}

// checkFlags requires the UP and UV flags of a response's authenticator data.
func checkFlags(flags protocol.AuthenticatorFlags) error {
	if !flags.UserPresent() {
		return fmt.Errorf("%w: the user presence flag is not set", ErrVerification)
	}
	if !flags.UserVerified() {
		return ErrUserVerification
	}
	return nil
}
