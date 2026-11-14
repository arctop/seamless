package main

// The tailnet's command line, as setup's address step and doctor ask it:
// whether Tailscale runs here, this computer's MagicDNS name and addresses,
// whether the tailnet issues certificates for that name, and -- setup only,
// when the owner chose it -- the certificate itself (tailscale cert).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"time"
)

// tailscaleCLI is the tailnet's command line. Injected so tests never run
// tailscale, and nothing reaches the tailnet or a certificate authority.
type tailscaleCLI interface {
	// Status is `tailscale status --json`, read.
	Status(ctx context.Context) (tailscaleStatus, error)
	// Cert is `tailscale cert --cert-file certFile --key-file keyFile name`.
	Cert(ctx context.Context, name, certFile, keyFile string) error
}

// tailscaleStatus is what setup and doctor read of `tailscale status --json`.
type tailscaleStatus struct {
	// running: the node is up on its tailnet (BackendState Running).
	running bool
	// name is this computer's MagicDNS name (Self.DNSName), lower-case and
	// without the final dot; "" when the tailnet gives it none.
	name string
	// addrs are this computer's tailnet addresses (Self.TailscaleIPs).
	addrs []netip.Addr
	// certDomains are the names the tailnet issues certificates for
	// (CertDomains): empty unless HTTPS certificates are on for the tailnet.
	certDomains []string
}

// issues reports whether the tailnet issues a certificate for name.
func (s tailscaleStatus) issues(name string) bool {
	name = strings.TrimSuffix(strings.ToLower(name), ".")
	return name != "" && slices.ContainsFunc(s.certDomains, func(d string) bool {
		return strings.TrimSuffix(strings.ToLower(d), ".") == name
	})
}

// usable says why this computer's tailnet name cannot be the Server's, ""
// when it can: Tailscale runs, names this computer, and issues certificates
// for that name.
func (s tailscaleStatus) usable() string {
	switch {
	case !s.running:
		return "Tailscale is installed but not running here"
	case s.name == "":
		return "Tailscale runs here, but the tailnet gives this computer no name: turn on MagicDNS in the tailnet's admin console"
	case !s.issues(s.name):
		return "Tailscale runs here as " + s.name + ", but the tailnet issues no certificate for it: turn on HTTPS certificates " +
			"in the tailnet's admin console (DNS page)"
	}
	return ""
}

// parseTailscaleStatus reads `tailscale status --json`.
func parseTailscaleStatus(raw []byte) (tailscaleStatus, error) {
	var doc struct {
		BackendState string
		Self         *struct {
			DNSName      string
			TailscaleIPs []string
		}
		CertDomains []string
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return tailscaleStatus{}, fmt.Errorf("tailscale status --json: %w", err)
	}
	st := tailscaleStatus{running: doc.BackendState == "Running", certDomains: doc.CertDomains}
	if doc.Self != nil {
		st.name = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(doc.Self.DNSName)), ".")
		for _, s := range doc.Self.TailscaleIPs {
			if a, err := netip.ParseAddr(s); err == nil {
				st.addrs = append(st.addrs, a.Unmap())
			}
		}
	}
	return st, nil
}

// errNoTailscale is a computer with no tailscale command to ask.
var errNoTailscale = errors.New("no tailscale command on this computer")

// execTailscale runs the tailscale command line found on this computer.
type execTailscale struct{}

// tailscaleStatusTimeout bounds `tailscale status`, which answers from the
// local daemon; tailscaleCertTimeout bounds `tailscale cert`, which waits on
// the certificate authority's challenge.
const (
	tailscaleStatusTimeout = 5 * time.Second
	tailscaleCertTimeout   = 3 * time.Minute
)

// tailscalePath finds the command: on PATH, else the macOS app's own.
func tailscalePath() (string, error) {
	if p, err := exec.LookPath("tailscale"); err == nil {
		return p, nil
	}
	if runtime.GOOS == "darwin" {
		const app = "/Applications/Tailscale.app/Contents/MacOS/Tailscale"
		if p, err := exec.LookPath(app); err == nil {
			return p, nil
		}
	}
	return "", errNoTailscale
}

func (execTailscale) Status(ctx context.Context) (tailscaleStatus, error) {
	bin, err := tailscalePath()
	if err != nil {
		return tailscaleStatus{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, tailscaleStatusTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "status", "--json").Output()
	if err != nil && len(out) == 0 {
		return tailscaleStatus{}, fmt.Errorf("tailscale status: %w", err)
	}
	// A stopped node exits non-zero and still prints its status.
	return parseTailscaleStatus(out)
}

func (execTailscale) Cert(ctx context.Context, name, certFile, keyFile string) error {
	bin, err := tailscalePath()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, tailscaleCertTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "cert", "--cert-file", certFile, "--key-file", keyFile, name).CombinedOutput()
	if err != nil {
		if said := strings.TrimSpace(setupPrintable(strings.ReplaceAll(string(out), "\n", " "))); said != "" {
			return fmt.Errorf("tailscale cert: %w: %s", err, said)
		}
		return fmt.Errorf("tailscale cert: %w", err)
	}
	return nil
}
