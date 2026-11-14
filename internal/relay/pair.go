package relay

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/devices"
)

// pairRequestTimeout bounds each request of a pairing; the wait for the
// owner's approval is Pair's context's.
const pairRequestTimeout = 30 * time.Second

// ErrPaired is a pairing asked of a relay that already has one, without
// PairOptions.Replace.
var ErrPaired = errors.New("this relay is already paired")

// PairOptions is one pairing of this relay with a server.
type PairOptions struct {
	// Server is the server's address, https://host[:port].
	Server string
	// Code is the one-time code the owner minted on the server (its Devices
	// page, or seamlessd devices code).
	Code string
	// Name is the host name the relay's requests speak for; "" is this
	// machine's (config.Hostname).
	Name string
	// ServerFingerprint, when set, is the server certificate's SHA-256 the
	// owner compared (devices.CertFingerprint's form): the code goes only over
	// a connection presenting it.
	ServerFingerprint string
	// CA, when set, is the PEM of the root the server's certificate chains to
	// (tls.ca_file on every other surface): a copy goes into the data
	// directory, which the relay trusts beside the system's roots.
	CA []byte
	// Replace lets this pairing take the place of the one the data directory
	// holds (a server whose certificate was renewed is paired again).
	Replace bool
	// Poll and Waiting are devices.PairRequest's: how often to ask after a
	// pairing that waits for the owner's passkey, and what to tell the person
	// pairing once it does.
	Poll    time.Duration
	Waiting func(fingerprint string)
}

// Pair pairs the relay whose data directory is dataDir with a server: a new
// key (devices.NewRelayIdentity), sent with the code over TLS, and -- once the
// server took it, at once or after the owner's approval -- the pairing saved
// with the fingerprint of the certificate the server presented, which the
// relay pins from then on. Nothing is saved until the server paired it; a
// pairing it cannot save is reported with the device to revoke.
func Pair(ctx context.Context, dataDir string, o PairOptions) (State, error) {
	const op = "relay.Pair"
	server, err := ServerURLOf(o.Server)
	if err != nil {
		return State{}, fmt.Errorf("%s: %w", op, err)
	}
	if o.ServerFingerprint != "" && !devices.ValidCertFingerprint(o.ServerFingerprint) {
		return State{}, fmt.Errorf("%s: %q is not a certificate's SHA-256 fingerprint (AB:CD:..., 32 bytes)", op, o.ServerFingerprint)
	}
	name := o.Name
	if name == "" {
		if name = config.Hostname(); name == "" {
			return State{}, fmt.Errorf("%s: this computer reports no host name: name the relay", op)
		}
	}
	prev, err := LoadState(dataDir)
	switch {
	case err == nil && !o.Replace:
		return State{}, fmt.Errorf("%s: %w with %s as %s (device %s): to pair it again, revoke that device on the server's Devices page "+
			"and replace the pairing", op, ErrPaired, prev.ServerURL, prev.Name, prev.DeviceID)
	case err == nil, errors.Is(err, ErrNotPaired):
	case errors.Is(err, ErrState) && o.Replace:
		// A pairing that does not load is replaced like any other.
	default:
		return State{}, fmt.Errorf("%s: %w", op, err)
	}
	id, err := devices.NewRelayIdentity(name)
	if err != nil {
		return State{}, fmt.Errorf("%s: %w", op, err)
	}
	caName := ""
	if len(o.CA) > 0 {
		if err := validCA(o.CA); err != nil {
			return State{}, fmt.Errorf("%s: %w", op, err)
		}
		if caName, err = writeCA(dataDir, o.CA); err != nil {
			return State{}, fmt.Errorf("%s: %w", op, err)
		}
	}
	// A pairing that does not finish leaves the CA copies as they were: the
	// previous pairing's, and no other.
	fail := func(err error) (State, error) {
		if rerr := removeStaleCAs(dataDir, prev.CAFile); rerr != nil {
			err = errors.Join(err, rerr)
		}
		return State{}, fmt.Errorf("%s: %w", op, err)
	}
	cfg := config.Defaults()
	if caName != "" {
		cfg.TLS.CAFile = filepath.Join(dataDir, caName)
	}
	client, err := cfg.HTTPClient(pairRequestTimeout)
	if err != nil {
		return fail(err)
	}
	paired, err := devices.Pair(ctx, client, server.String(), devices.PairRequest{Code: o.Code, Name: name, Relay: id,
		ServerFingerprint: o.ServerFingerprint, Poll: o.Poll, Waiting: o.Waiting})
	if err != nil {
		return fail(err)
	}
	if paired.ServerFingerprint == "" {
		return fail(lostPairing(paired, errors.New("the server's certificate was not seen")))
	}
	certPEM, keyPEM, err := id.MarshalPEM()
	if err != nil {
		return fail(lostPairing(paired, err))
	}
	st := State{Version: stateVersion, ServerURL: server.String(), ServerFingerprint: paired.ServerFingerprint, CAFile: caName,
		DeviceID: paired.DeviceID, Name: paired.Name, Fingerprint: paired.Fingerprint, PairedAt: time.Now().UTC(),
		Certificate: string(certPEM), Key: string(keyPEM)}
	if err := SaveState(dataDir, st); err != nil {
		return fail(lostPairing(paired, err))
	}
	return st, nil
}

// lostPairing is a pairing the server made and the relay could not keep.
func lostPairing(p devices.Paired, err error) error {
	return fmt.Errorf("the server paired this relay as %s (device %s), but the pairing could not be saved (%w): revoke it on the "+
		"server's Devices page and pair again", p.Name, p.DeviceID, err)
}

// validCA checks that b holds at least one PEM certificate, and nothing that
// is not one.
func validCA(b []byte) error {
	rest, n := b, 0
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			return fmt.Errorf("the CA file holds a %s block: give the server's root certificate alone, never a key", block.Type)
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return fmt.Errorf("the CA file holds a certificate that does not parse: %w", err)
		}
		n++
	}
	if n == 0 {
		return errors.New("the CA file holds no PEM certificate")
	}
	return nil
}
