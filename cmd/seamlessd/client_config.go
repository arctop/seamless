package main

// seamlessd client-config -- the pairing hand-off.
//
// It runs on the SERVER and prints exactly what an operator pastes on ANOTHER
// machine to make that machine a client of this daemon: the two installer
// one-liners, the manual install-hooks form for a machine that already has the
// binary, and the oldest client version any of it works on. It reads the
// resolved config and formats it; it mutates nothing and opens no connection.
//
// The refusals carry most of the weight. Every line this prints is a command
// with a URL and a bearer key baked in, and each refused state below produces a
// command that LOOKS right and cannot work: a client install handing out its
// own server's coordinates, a keyless install nothing can authenticate to, and
// -- the one that actually happens -- a loopback URL pasted onto a second
// machine, where it dials that machine's own port 8081 and fails as a
// connection error from the wrong end of the network.

import (
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strings"

	"github.com/arctop/seamless/internal/config"
)

// minClientVersion is the oldest seamlessd a paired client may run: it is the
// first release carrying `install-hooks --server-url`, so an older binary on
// the other machine has no flag to accept this server's URL at all and the
// installer's probe refuses rather than guessing.
//
// It is a constant and not a derivation because nothing in the build records
// which version a flag shipped in. Bump it here if the flag lands under a
// different version than planned.
const minClientVersion = "0.5.0"

// redactedKey stands in for the bearer key under --redact. It carries no shell
// metacharacters, so a redacted line is still a paste-able command of exactly
// the same shape, and it is obviously a placeholder, so nobody runs it as-is.
const redactedKey = "REPLACE_WITH_THE_API_KEY"

// The three canonical pairing commands. The env var names here are the ones
// docs/install and docs/install.ps1 read, and the flag names are the ones
// install-hooks defines -- this is a contract across four files, not a
// formatting choice, so they are written once and substituted into.
const (
	unixPairCommand    = "curl -fsSL https://thereisnospoon.org/install | SEAMLESS_SERVER_URL=%s SEAMLESS_MCP_API_KEY=%s sh"
	windowsPairCommand = "$env:SEAMLESS_SERVER_URL='%s'; $env:SEAMLESS_MCP_API_KEY='%s'; irm https://thereisnospoon.org/install.ps1 | iex"
	manualPairCommand  = "seamlessd install-hooks --server-url %s --api-key %s"
)

// runClientConfig parses the flags, loads the config, and renders the report to
// stdout. Every decision lives in clientConfigReport, which is pure over the
// config and a writer.
func runClientConfig(args []string) error {
	fs := flag.NewFlagSet("client-config", flag.ContinueOnError)
	redact := fs.Bool("redact", false,
		"print a placeholder instead of the API key, so the output is safe to paste into a ticket or a chat")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("seamlessd.client-config: %w", err)
	}
	return clientConfigReport(os.Stdout, cfg, *redact)
}

// clientConfigReport writes the pairing block to w, or returns the error that
// explains why this install has no usable pairing to hand out.
//
// Pure over cfg and w on purpose: the refusals and the printed commands are the
// whole product of this command, and keeping them out of flag parsing, config
// loading and stdout is what lets every one of them be asserted without a
// config file or a network.
func clientConfigReport(w io.Writer, cfg config.Config, redact bool) error {
	src := configSourceLabel(cfg)
	if cfg.IsClient() {
		return fmt.Errorf("seamlessd.client-config: role: client -- this install runs no daemon of its own, it dials %s; "+
			"run `seamlessd client-config` on THAT machine to pair another client", cfg.DaemonAddress())
	}
	key := strings.TrimSpace(cfg.MCP.APIKey)
	if key == "" {
		return fmt.Errorf("seamlessd.client-config: mcp.api_key is empty in %s -- a client has nothing to authenticate with; "+
			"set it (openssl rand -hex 32) and restart the daemon", src)
	}
	if cfg.ServerURLNamesUser() {
		// Every line below is a command for another machine, --redact's
		// included: each would carry the user and password into its shell
		// history, as the device one-liners would (devices.ErrPairingNamesUser).
		return fmt.Errorf("seamlessd.client-config: server_url names a user: remove it, Seamless uses none -- the commands would "+
			"carry it, password and all, to the other machine and its shell history; leave scheme://host:port in server_url "+
			"in %s and restart the daemon", src)
	}
	url := cfg.ServerURL()
	host := cfg.ServerHost()
	if host == "" {
		return fmt.Errorf("seamlessd.client-config: server url %q names no host; "+
			"set `server_url: http://<this-machine>:8081` in %s and restart the daemon", url, src)
	}
	if isLoopbackServerHost(host) {
		return fmt.Errorf("seamlessd.client-config: %s is a loopback address -- no other machine can reach it; "+
			"set `server_url: http://<this-machine>:8081` (the name or address other devices use) plus `addr: 0.0.0.0:8081` in %s, then restart the daemon. "+
			"A SECOND USER ON THE SAME BOX needs none of that: they can pair directly with "+
			"`seamlessd install-hooks --server-url %s --api-key <key>`", url, src, url)
	}

	// A warning and not a refusal: http over a trusted LAN is a legitimate
	// choice, and the operator already had to widen the bind address and name
	// server_url to get here. What it must not be is silent -- the key in the
	// commands below, and every memory the client fetches with it, cross the
	// network in the clear.
	if strings.HasPrefix(url, "http://") {
		fmt.Fprintf(w, "%s %s is plain http: the bearer key below and every memory it fetches cross the network unencrypted\n%s%s\n",
			yellow("warning:"), url, fieldCont,
			dim("set tls.cert_file + tls.key_file and restart to serve https, or keep this inside a trusted LAN"))
	}

	shown := key
	if redact {
		shown = redactedKey
	}
	fmt.Fprintf(w, "\n%s %s\n", bold("Seamless"), dim("client pairing"))
	clientField(w, "server", url)
	clientField(w, "key", shown)
	clientField(w, "clients", dim("seamlessd "+minClientVersion+" or newer (--server-url landed in that release)"))

	fmt.Fprintf(w, "\n%s\n", dim("Run ONE of these on the CLIENT machine:"))
	pairBlock(w, "macOS / Linux", fmt.Sprintf(unixPairCommand, url, shown))
	pairBlock(w, "Windows (PowerShell)", fmt.Sprintf(windowsPairCommand, url, shown))
	pairBlock(w, "already installed", fmt.Sprintf(manualPairCommand, url, shown))

	if redact {
		fmt.Fprintf(w, "\n%s\n", dim("--redact: the key is masked. Replace "+redactedKey+
			" with the real key, which `seamlessd client-config` prints without --redact."))
	}
	return nil
}

// pairBlock prints one labelled command: the heading, then the command itself
// on its own indented line so it survives a copy-paste out of a terminal.
func pairBlock(w io.Writer, label, command string) {
	fmt.Fprintf(w, "\n  %s\n    %s\n", bold(label), green(command))
}

// clientField is fieldRow against an arbitrary writer. fieldRow itself prints
// to stdout, which this command cannot use: its whole output is the thing under
// test, and capturing stdout to assert a command string would make every
// assertion depend on process state. Same layout, so the block still lines up
// with the installer's.
func clientField(w io.Writer, name, value string) {
	fmt.Fprintf(w, "  %s  %s\n", dim(fmt.Sprintf("%-*s", fieldLabelWidth, name)), value)
}

// configSourceLabel names the file an operator has to edit for the advice in a
// refusal to be actionable. Defaults-plus-environment has no file to name, so
// it says so rather than inventing a path that does not exist yet.
func configSourceLabel(cfg config.Config) string {
	src := cfg.SourcePath()
	if src == "" {
		return "seamless.yaml (no config file loaded; defaults + environment only)"
	}
	return tildePath(src)
}

// isLoopbackServerHost reports whether host addresses only this machine. It
// takes the bare host config.ServerHost returns -- no scheme, no port, no IPv6
// brackets -- and reuses netguard's loopbackHosts so the set of names that mean
// "here" stays written down once.
func isLoopbackServerHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if loopbackHosts[h] {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
