package config

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// defaultServerURL is the base URL for an install whose addr cannot be read as
// host:port at all. An addr in that shape is unusable as a listener too
// (net.Listen wants host:port, so the daemon never came up), which is why
// naming the built-in default is more useful than echoing the broken value
// back: every consumer of this URL is a client trying to reach a daemon.
const defaultServerURL = "http://127.0.0.1:8081"

// ServerURL is the base URL (scheme://host[:port], no trailing slash) that
// clients -- the seam CLI, installed hooks, the MCP registration, the console
// opener, the published cards -- use to reach this install's daemon.
//
// It is the single derivation: five copies of "split addr, map the wildcard to
// loopback, prefix http://" used to live in cmd/seam, cmd/seamlessd (twice),
// cmd/seambench and cmd/docsgen, and they had already drifted from each other
// on the malformed-addr edge. Bind address and client target are different
// questions that happen to have the same answer on a loopback install; keeping
// one derivation here is what lets the second question get its own answer
// (server_url) without hunting the first one down in five files.
// A configured server_url wins outright: it is the operator saying "this is
// where clients reach me", which is precisely the question no bind address can
// answer once the two differ (a wildcard bind, a LAN name, a reverse proxy).
func (c Config) ServerURL() string {
	if c.SocketPath() != "" {
		// A socket has no host to put in a URL. Every consumer appends a path to
		// this value and dials through HTTPClient, whose transport ignores the
		// host and dials the socket instead -- so the URL only has to be a valid
		// http one.
		return SocketBaseURL
	}
	if advertised := strings.TrimSpace(c.AdvertisedURL); advertised != "" {
		return strings.TrimSuffix(advertised, "/")
	}
	scheme := "http"
	if c.TLSEnabled() {
		scheme = "https"
	}
	host, port, err := net.SplitHostPort(c.Addr)
	if err != nil {
		return defaultServerURL
	}
	return scheme + "://" + net.JoinHostPort(reachableHost(host), port)
}

// SocketBaseURL is ServerURL for a server_url naming a Unix socket. The host is
// never resolved: HTTPClient's transport dials the socket for every request.
const SocketBaseURL = "http://localhost"

// SocketPath is the Unix socket a `server_url: unix:///path` names, or "" when
// server_url is an http(s) URL or unset. It is how a role user's install (the
// seam CLI, its hooks, its mcp-proxy bridge) reaches a daemon that tells
// callers apart by peer uid: the kernel, not a key, authenticates the dial.
func (c Config) SocketPath() string {
	u, err := url.Parse(strings.TrimSpace(c.AdvertisedURL))
	if err != nil || !strings.EqualFold(u.Scheme, "unix") {
		return ""
	}
	return u.Path
}

// HookBaseURL is the base URL the hook and MCP installers describe this
// install's daemon by: ServerURL, except for a socket server_url, where it is
// the unix:///path itself -- the one spelling from which the installers can
// tell that every hook and the MCP registration must go through the seam CLI
// (Claude Code cannot dial a socket). Anything that actually dials keeps using
// ServerURL with HTTPClient.
func (c Config) HookBaseURL() string {
	if p := c.SocketPath(); p != "" {
		return "unix://" + p
	}
	return c.ServerURL()
}

// DaemonAddress is where this install's daemon is reached, for messages a
// person reads: the socket path when server_url names one, else ServerURL.
func (c Config) DaemonAddress() string {
	if p := c.SocketPath(); p != "" {
		return "unix:" + p
	}
	return c.ServerURL()
}

// ServerHost is the bare host of ServerURL: no scheme, no port, no IPv6
// brackets, lower-cased -- the form a Host header and a TLS SAN carry, and the
// form the Host allowlist compares against.
func (c Config) ServerHost() string {
	u, err := url.Parse(c.ServerURL())
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// IsClient reports whether this install runs no daemon of its own and dials a
// server elsewhere instead.
//
// It is the single predicate every branching call site shares. An unset role is
// the default (server): absence means the default, and a role that is neither
// value never reaches here, because Validate refuses it at load.
func (c Config) IsClient() bool {
	return c.Role == RoleClient
}

// TLSEnabled reports whether the daemon serves HTTPS rather than HTTP -- which
// is what decides ServerURL's scheme, whether the console's session cookie may
// be marked Secure, and what the non-loopback bind warning may truthfully claim.
//
// Both halves of the pair are required because serving TLS needs both; Validate
// refuses a half-set pair at load, so a lone cert_file can never quietly read as
// "no TLS" here.
func (c Config) TLSEnabled() bool {
	return strings.TrimSpace(c.TLS.CertFile) != "" && strings.TrimSpace(c.TLS.KeyFile) != ""
}

// AllowedHostsEffective is the extra-Host allowlist the daemon answers to: the
// configured allowed_hosts, plus the host of a CONFIGURED server_url. The
// advertised host is in the list whenever it was named, because advertising a
// name the guard then rejects is a self-inflicted 421.
//
// Loopback names and the concrete bind host are NOT here: hostGuard adds those
// itself, and they are facts about the listener rather than operator intent.
// That distinction is load-bearing. A DERIVED ServerHost is exactly one of
// those two -- loopback for a wildcard bind, the bind host for a concrete one
// -- so putting it in this list would add nothing the guard did not already
// allow while making the list non-empty, and a non-empty list is precisely what
// switches the guard on for a wildcard bind. The daemon would start refusing
// every Host but loopback on an `addr: 0.0.0.0` install where the operator
// named nothing at all.
//
// Empty return = "the operator has named no host", which is the state hostGuard
// reads as "a wildcard bind cannot have an allowlist".
func (c Config) AllowedHostsEffective() []string {
	var out []string
	add := func(h string) {
		if h = strings.ToLower(strings.Trim(strings.TrimSpace(h), "[]")); h != "" && !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	for _, h := range c.AllowedHosts {
		add(h)
	}
	// A socket server_url names no host at all, so it adds nothing here: the
	// guard is about Host headers on the TCP listener.
	if strings.TrimSpace(c.AdvertisedURL) != "" && c.SocketPath() == "" {
		add(c.ServerHost())
	}
	return out
}

// validateTransport enforces the role/server_url/tls rules, as one block so the
// cross-key rules (a client needs a URL, a client has no server certificate)
// sit beside the per-key ones they depend on.
func (c Config) validateTransport() error {
	// Absent (empty) means the default; present-but-unrecognized is an error,
	// never a silent fallback to server -- which is the one that opens a port.
	if c.Role != "" && !slices.Contains(Roles, c.Role) {
		return fmt.Errorf("config: role invalid %q: valid values are %s", c.Role, strings.Join(Roles, ", "))
	}
	if err := validateServerURL(c.AdvertisedURL); err != nil {
		return err
	}
	if (strings.TrimSpace(c.TLS.CertFile) == "") != (strings.TrimSpace(c.TLS.KeyFile) == "") {
		return fmt.Errorf("config: tls.cert_file and tls.key_file must be set together (one without the other cannot serve TLS)")
	}
	if c.IsClient() {
		if strings.TrimSpace(c.AdvertisedURL) == "" {
			return fmt.Errorf("config: role: client requires server_url -- a client runs no daemon of its own")
		}
		if strings.TrimSpace(c.TLS.CertFile) != "" {
			return fmt.Errorf("config: tls.cert_file is a server key; a client trusts its server with tls.ca_file")
		}
	}
	return nil
}

// validateServerURL refuses anything that is not a bare absolute base URL.
//
// The strictness is the point: every consumer appends a path to this value
// ("/api/mcp", "/healthz", "/console/"), and ServerHost compares its host
// against a Host header and a TLS SAN. A bare "host:8081" parses as a URL with
// scheme "host" -- valid to url.Parse, useless to both -- so the scheme check is
// what catches the most likely typo. A single trailing slash is accepted and
// trimmed (it names the same base URL); any deeper path is refused rather than
// silently dropped.
//
// A wildcard host is REFUSED here rather than mapped to loopback the way a
// wildcard bind is. The two look alike and are opposite questions: a wildcard
// addr is an operator answering "listen everywhere", which reachableHost can
// safely narrow to "dial me here", while a wildcard server_url is the operator
// answering "clients reach me here" with a non-answer. Rewriting that one would
// hand back an address that is only correct on the daemon's own machine, which
// is precisely the machine the value exists to point away from.
func validateServerURL(raw string) error {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil
	}
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("config: server_url %q is not a URL: %w", s, err)
	}
	switch u.Scheme {
	case "http", "https":
	case "unix":
		return validateSocketURL(s, u)
	default:
		return fmt.Errorf("config: server_url %q must be an absolute http:// or https:// URL, or unix:///path/to/socket", s)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("config: server_url %q names no host", s)
	}
	// IsWildcardHost is the predicate reachableHost rewrites by, so the derive
	// path and this refusal can never disagree about which hosts are
	// wildcards. url.Hostname strips the brackets, so "http://[::0]:8081"
	// arrives here as "::0", and a real IPv6 literal like [fd00::1] is not a
	// wildcard to either. The empty host is a wildcard too, which is why this
	// sits BELOW the "names no host" check: that value has its own, more
	// specific error.
	if host := u.Hostname(); IsWildcardHost(host) {
		return fmt.Errorf("config: server_url %q names the wildcard host %q, which says where the daemon LISTENS, not where a client dials: "+
			"put the wildcard in addr and set server_url to the name or address other machines reach this one at", s, host)
	}
	if strings.Trim(u.Path, "/") != "" || strings.Contains(u.Path, "//") {
		return fmt.Errorf("config: server_url %q must be a bare base URL (scheme://host:port), with no path", s)
	}
	if u.RawQuery != "" || u.ForceQuery {
		return fmt.Errorf("config: server_url %q must be a bare base URL, with no query string", s)
	}
	if u.Fragment != "" {
		return fmt.Errorf("config: server_url %q must be a bare base URL, with no fragment", s)
	}
	return nil
}

// reachableHost maps a bind host that means "every interface" to loopback. A
// wildcard is an answer to "where do I listen", never to "where do I dial":
// advertising http://0.0.0.0:8081 hands a client an address that reaches, at
// most, the client's own machine. Which hosts count is IsWildcardHost's call
// alone, so this rewrite and validateServerURL's refusal cannot disagree.
func reachableHost(host string) string {
	if IsWildcardHost(host) {
		return "127.0.0.1"
	}
	return host
}

// IsWildcardHost reports whether host means "every interface": the empty host
// of a bare-port bind (":8081"), or any spelling of the unspecified address.
//
// It is the one definition of a wildcard host. The derive path (reachableHost),
// the server_url refusal and the daemon's bind guards (cmd/seamlessd's
// hostGuard and isLoopbackBind) ask it different questions -- what to dial
// instead, whether to refuse, whether a Host allowlist is knowable -- but about
// the same set of hosts, and a second copy of that set is how they drift.
//
// The test is the address, not its spelling. "[::0]", "[0:0:0:0:0:0:0:0]" and
// "[::ffff:0.0.0.0]" all bind exactly the socket "[::]" does, and the literal
// list this replaced missed every one of them. netip.ParseAddr is the parser
// net.Listen and net.Dial apply to a literal host: it accepts a zone, which the
// unspecified address ignores ("[::%lo0]:8081" still listens everywhere), and
// Unmap folds the IPv4-mapped form the way net.IP.IsUnspecified does. One pair
// of IPv6 brackets is stripped, for a caller that split a host itself.
//
// A NAME is never a wildcard, even one a resolver maps to 0.0.0.0 (the libc
// resolver reads "0" and "00.0.0.0" that way; Go's own refuses both): answering
// would put a platform-dependent lookup on the config path.
func IsWildcardHost(host string) bool {
	if len(host) >= 2 && host[0] == '[' && host[len(host)-1] == ']' {
		host = host[1 : len(host)-1]
	}
	if host == "" {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.WithZone("").Unmap().IsUnspecified()
}

var (
	hostnameOnce   sync.Once
	cachedHostname string
)

// Hostname is this machine's hostname, lower-cased and cached for the life of
// the process: it identifies which device a session, a repo mapping, or a
// captured plan came from, so it is read on hot paths (every hook fires one)
// and must not re-syscall each time.
//
// Lower-cased because it is compared with hostnames that arrived over the wire,
// where case is not preserved. An unreadable hostname yields "", which callers
// must treat as "unknown machine" rather than as a name -- never as a stand-in
// for the local one.
func Hostname() string {
	hostnameOnce.Do(func() {
		h, err := os.Hostname()
		if err != nil {
			return // stays "": unknown, not a guess
		}
		cachedHostname = strings.ToLower(strings.TrimSpace(h))
	})
	return cachedHostname
}

// validateSocketURL accepts unix:///absolute/path (and its one-slash spelling
// unix:/absolute/path): a daemon's peer_auth socket. There is no host, query or
// fragment to carry, so any of them is a mistake rather than something to drop.
func validateSocketURL(raw string, u *url.URL) error {
	if u.Host != "" || u.User != nil {
		return fmt.Errorf("config: server_url %q names a host; a socket URL is unix:///absolute/path", raw)
	}
	if u.Opaque != "" || !filepath.IsAbs(u.Path) || filepath.Clean(u.Path) != u.Path {
		return fmt.Errorf("config: server_url %q must name an absolute, clean socket path: unix:///absolute/path", raw)
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return fmt.Errorf("config: server_url %q must be a bare socket URL, with no query or fragment", raw)
	}
	return nil
}
