package config

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/arctop/seamless/internal/validate"
)

// PeerAuth is peer-user authentication: a Unix-socket listener whose callers
// the daemon identifies by the uid the kernel reports, the projects locked
// until the owner approves an agent's access (Locked), and the restricted
// local accounts that work beside the owner (Restricted) (internal/access
// holds the policy itself).
//
// Off by default, and off means exactly the behavior every earlier release had:
// no socket, no per-caller filtering, one static bearer key on TCP. On, every
// request carries a principal: the bearer key on TCP is the owner account, and
// a socket peer is whoever the kernel says it is.
//
// The yaml block is optional and was added after v0.7.4: a config that carries
// it does not load in a release that predates it (KnownFields), which is why the
// shipped example leaves it commented out.
type PeerAuth struct {
	// Enabled turns the socket listener and the per-caller policy on.
	Enabled bool `yaml:"enabled"`
	// Owner is the OS account (a user name, or a numeric uid) the TCP bearer key
	// stands for, and whose socket connections act as the owner. Required when
	// enabled.
	Owner string `yaml:"owner"`
	// Socket is the listener the owner's agents and the restricted accounts
	// dial.
	Socket PeerSocket `yaml:"socket"`
	// ForceLocked lists the force-locked projects: locks only this file can
	// remove. Even the owner's own agents reach one only through a lease the
	// owner approved, held by one session's socket connection; the TCP bearer
	// key never reaches it. Without a lease the project is locked: its name
	// shows, its content does not, and writes into it are refused. File only
	// (no env). The console sets locks of its own (store.ConsoleLocks), each
	// behind a passkey, and may remove only those.
	//
	// The lock is not an isolation state. Isolation (open, confidential,
	// sealed) says what flows in and out of a project; the lock says whether
	// its own agents need a fresh owner approval. A locked project is normally
	// confidential, so its approved sessions still read open projects and
	// global. The key was called "sealed" before any release shipped it; a
	// config that still says peer_auth.sealed is refused at load
	// (renamedPeerAuthKey).
	ForceLocked []string `yaml:"force_locked"`
	// LegacyLocked is force_locked's earlier name, peer_auth.locked. A key
	// a config already holds must keep loading (AGENTS.md, release
	// discipline), and dropping a lock would open what it closed, so load
	// folds it into ForceLocked (foldLegacyLocked) and doctor names the new
	// key. Nothing else reads this field.
	LegacyLocked []string `yaml:"locked"`
	// MaxLease caps every lease the owner approves on a locked project, the
	// "until this session ends" choice included: past it, a session asks again.
	// Unset (0) means DefaultMaxLease; set, it is at most 24h. File only.
	MaxLease Duration `yaml:"max_lease"`
	// Passkeys names the WebAuthn relying party the owner's approver passkeys
	// are registered to (the console's enrollment and approval pages). Unset,
	// it is the console itself on localhost (RelyingParty). File only.
	Passkeys PeerPasskeys `yaml:"passkeys"`
	// Users is retired: the 2026-10-31 design sealed projects to other OS
	// accounts the owner never reached. The key stays known so a config that
	// still names it reads how to move, not an unknown-field error. Anything in
	// it is refused at load, even while peer_auth is disabled (owner decision,
	// 2026-11-06; ErrPeerAuthUsersRetired). Absent or empty it means nothing.
	Users map[string]any `yaml:"users"`
	// Restricted lists restricted local accounts, each by OS user name or
	// numeric uid: a second account on the machine whose agents work beside the
	// owner's. Such an account connects over the socket only (the kernel's peer
	// uid names it; it never holds the bearer key), and reaches every project
	// that is open and not locked, reading and writing, plus the global scope
	// read-only. Its writes are labeled with its name, and a constraint,
	// convention or stage it writes is a proposal until the owner promotes it
	// (internal/access, KindRestricted). It files no access requests, gets no
	// read grants and holds no leases. Refused at load while peer_auth is
	// disabled and for the owner. File only (no env).
	Restricted []string `yaml:"restricted"`
}

// PeerPasskeys is the WebAuthn relying party of the approver passkeys. A
// passkey is bound to its RP ID for life: changing rp_id later strands every
// passkey enrolled under the old one.
type PeerPasskeys struct {
	// RPID is a host name -- never an IP literal, which WebAuthn does not
	// accept -- such as "localhost" or "seamless.example.com". Empty, it is
	// derived (Config.RelyingParty).
	RPID string `yaml:"rp_id"`
	// Origins are the exact origins the console's passkey pages are served
	// from: https, or http on localhost, each on rp_id or a subdomain of it.
	// With both keys empty, a TLS install whose server_url names a host derives
	// both from that name (Config.ServedRelyingParty); any other install uses
	// the console's own address on localhost.
	Origins []string `yaml:"origins"`
}

// PeerSocket places the Unix-socket listener.
type PeerSocket struct {
	// Path is the socket's absolute path. Its directory must not be writable by
	// anyone but the daemon's account or root -- serve refuses otherwise, since
	// whoever can replace the socket can impersonate the daemon to every caller.
	// It must not sit inside data_dir, which is 0700 and so untraversable by the
	// callers the socket exists for.
	Path string `yaml:"path"`
	// Mode is the socket file's permission bits, in octal ("0666"). Connecting
	// needs write permission on the socket, so the mode decides who may try; the
	// kernel still names every peer, and an unconfigured uid is refused. Empty
	// means DefaultPeerSocketMode.
	Mode string `yaml:"mode"`
	// Group, when set, is the group the socket file is handed to (by name or
	// numeric gid) -- with a mode such as 0660, only that group's members may
	// connect at all. Empty leaves the daemon's primary group.
	Group string `yaml:"group"`
}

// ErrPeerAuthUsersRetired is the refusal of a filled-in peer_auth.users: the
// design that key belonged to is gone, and locking is what replaced it. The
// guide quotes the sentence, so the two change together.
var ErrPeerAuthUsersRetired = errors.New("config: peer_auth.users was the 2026-10-31 design, which sealed projects to " +
	"other OS accounts, and is no longer supported: list the projects under peer_auth.force_locked and remove users")

// RetiredUsersRefusal returns ErrPeerAuthUsersRetired while peer_auth.users
// names anything, and nil when it is absent or empty. It is the one reader of
// Users: config load and access.New both refuse through it, so a block that
// did not come through a load fails the same way.
func (p PeerAuth) RetiredUsersRefusal() error {
	if len(p.Users) > 0 {
		return ErrPeerAuthUsersRetired
	}
	return nil
}

// DefaultPeerSocketMode is the socket mode when peer_auth.socket.mode is unset:
// every local account may connect, and the kernel tells them apart. Unlisted
// accounts are refused before any route runs, the same posture the loopback TCP
// port has toward a caller without the key.
const DefaultPeerSocketMode = "0666"

// SocketFileMode parses Mode, defaulting to DefaultPeerSocketMode.
func (s PeerSocket) SocketFileMode() (uint32, error) {
	raw := strings.TrimSpace(s.Mode)
	if raw == "" {
		raw = DefaultPeerSocketMode
	}
	n, err := strconv.ParseUint(raw, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("config: peer_auth.socket.mode %q is not an octal mode such as 0666: %w", s.Mode, err)
	}
	if n > 0o777 {
		return 0, fmt.Errorf("config: peer_auth.socket.mode %q has bits beyond 0777 (no setuid, setgid or sticky on a socket)", s.Mode)
	}
	if n&0o600 != 0o600 {
		return 0, fmt.Errorf("config: peer_auth.socket.mode %q must keep owner read and write (0600): the daemon's own account must reach its socket", s.Mode)
	}
	return uint32(n), nil
}

// ReferencedProjects returns every project slug the block names, sorted and
// de-duplicated: what serve and doctor check against the projects table.
func (p PeerAuth) ReferencedProjects() []string {
	var out []string
	for _, slug := range p.ForceLocked {
		if s := strings.TrimSpace(slug); s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return out
}

// renamedPeerAuthKey reports a peer_auth key spelled the way a pre-release
// build spelled it, and its name now: peer_auth.sealed was renamed
// before any release shipped it, because "sealed" is an isolation state and the
// lock is a separate setting. Without this, KnownFields would refuse the old
// key as an unknown field and leave the owner to guess the new name. A key
// nested deeper is not this one.
// Syntax errors are left to the strict decode, as in explicitNullPath.
func renamedPeerAuthKey(data []byte) (old, now string) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil || doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		return "", ""
	}
	top := resolveAlias(doc.Content[0])
	if top.Kind != yaml.MappingNode {
		return "", ""
	}
	for i := 0; i+1 < len(top.Content); i += 2 {
		if top.Content[i].Value != "peer_auth" {
			continue
		}
		block := resolveAlias(top.Content[i+1])
		if block.Kind != yaml.MappingNode {
			return "", ""
		}
		for j := 0; j+1 < len(block.Content); j += 2 {
			if block.Content[j].Value == "sealed" {
				return "peer_auth.sealed", "peer_auth.force_locked"
			}
		}
	}
	return "", ""
}

func resolveAlias(n *yaml.Node) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	if n == nil {
		return &yaml.Node{}
	}
	return n
}

// DefaultMaxLease is peer_auth.max_lease when unset (owner decision,
// 2026-11-06).
const DefaultMaxLease = 8 * time.Hour

// maxMaxLease bounds peer_auth.max_lease: an approval is a decision about one
// working session, never a standing grant.
const maxMaxLease = 24 * time.Hour

// LeaseCap returns peer_auth.max_lease, or DefaultMaxLease when unset.
func (p PeerAuth) LeaseCap() time.Duration {
	if p.MaxLease <= 0 {
		return DefaultMaxLease
	}
	return time.Duration(p.MaxLease)
}

// RelyingParty returns the WebAuthn relying party for the approver passkeys:
// the configured rp_id and origins; unset, the TLS server_url's host name
// (ServedRelyingParty); otherwise "localhost" and the console's own address on
// localhost (http, or https when the daemon serves TLS). A console reached as
// 127.0.0.1 cannot run a passkey ceremony at all -- an IP is not a valid RP ID
// -- which is why the last default names localhost.
func (c Config) RelyingParty() (rpID string, origins []string) {
	p := c.PeerAuth.Passkeys
	rpID = strings.TrimSpace(p.RPID)
	for _, o := range p.Origins {
		if o = strings.TrimSpace(o); o != "" {
			origins = append(origins, strings.TrimSuffix(o, "/"))
		}
	}
	if rpID == "" && len(origins) == 0 {
		if host, origin, ok := c.ServedRelyingParty(); ok {
			return host, []string{origin}
		}
	}
	if rpID == "" {
		rpID = "localhost"
	}
	if len(origins) > 0 {
		return rpID, origins
	}
	scheme := "http"
	if c.TLSEnabled() {
		scheme = "https"
	}
	port := "8081"
	if _, pt, err := net.SplitHostPort(c.Addr); err == nil && pt != "" && pt != "0" {
		port = pt
	}
	return rpID, []string{scheme + "://" + net.JoinHostPort(rpID, port)}
}

// ServedRelyingParty is the relying party a TLS install derives from the name
// it serves: when TLS is on and ServerURL is an https URL whose host is a DNS
// name (never an IP literal, never a socket), the RP ID is exactly that host and
// the origin is https://<host>[:<port>]. ok is false otherwise. The host is kept
// as named and never widened to a parent domain, which would bring every other
// host under that domain into the passkeys' scope.
//
// It reads no file: whether the certificate covers the name is doctor's and
// serve's warning (a certificate that does not cover it fails every client's
// TLS anyway), so every reader of the relying party -- serve, export, import,
// console-open -- derives the same answer from the same config.
func (c Config) ServedRelyingParty() (rpID, origin string, ok bool) {
	if !c.TLSEnabled() || c.SocketPath() != "" {
		return "", "", false
	}
	u, err := url.Parse(c.ServerURL())
	if err != nil || !strings.EqualFold(u.Scheme, "https") {
		return "", "", false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || validRPID(host) != nil {
		return "", "", false
	}
	origin = "https://" + host
	if port := u.Port(); port != "" && port != "443" {
		origin += ":" + port
	}
	return host, origin, true
}

// validatePeerPasskeys is the passkeys block's shape: rp_id a lowercase host
// name (never an IP literal), and every origin exactly scheme://host[:port] --
// https, or http on localhost -- with a host that is rp_id or a subdomain of
// it. Origins without an rp_id are refused rather than read against the
// default: a config naming origins means to name its relying party too.
func validatePeerPasskeys(p PeerPasskeys) error {
	rpID := strings.TrimSpace(p.RPID)
	if rpID == "" {
		if len(p.Origins) > 0 {
			return fmt.Errorf("config: peer_auth.passkeys.origins is set without rp_id: name the relying party the origins belong to")
		}
		return nil
	}
	if err := validRPID(rpID); err != nil {
		return fmt.Errorf("config: peer_auth.passkeys.rp_id %q: %w", p.RPID, err)
	}
	seen := map[string]bool{}
	for _, raw := range p.Origins {
		o := strings.TrimSuffix(strings.TrimSpace(raw), "/")
		if err := validPasskeyOrigin(o, rpID); err != nil {
			return fmt.Errorf("config: peer_auth.passkeys.origins %q: %w", raw, err)
		}
		if seen[o] {
			return fmt.Errorf("config: peer_auth.passkeys.origins lists %q twice", raw)
		}
		seen[o] = true
	}
	return nil
}

// validRPID accepts a lowercase DNS host name: dot-separated labels of
// letters, digits and inner dashes. An IP literal is refused.
func validRPID(s string) error {
	if net.ParseIP(s) != nil {
		return fmt.Errorf("an IP address is not a valid WebAuthn RP ID: use a host name such as localhost")
	}
	if len(s) > 253 {
		return fmt.Errorf("too long for a host name")
	}
	for label := range strings.SplitSeq(s, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return fmt.Errorf("not a host name (lowercase letters, digits, dots and dashes)")
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return fmt.Errorf("not a host name (lowercase letters, digits, dots and dashes)")
			}
		}
	}
	return nil
}

func validPasskeyOrigin(o, rpID string) error {
	u, err := url.Parse(o)
	if err != nil || u.Host == "" || u.Opaque != "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("an origin is scheme://host[:port] and nothing else")
	}
	host := u.Hostname()
	if net.ParseIP(host) != nil {
		return fmt.Errorf("an IP address cannot carry a passkey: use a host name")
	}
	if host != rpID && !strings.HasSuffix(host, "."+rpID) {
		return fmt.Errorf("the host must be rp_id %q or a subdomain of it", rpID)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if host != "localhost" && !strings.HasSuffix(host, ".localhost") {
			return fmt.Errorf("http is accepted only on localhost: browsers refuse passkeys on any other plain-http origin")
		}
	default:
		return fmt.Errorf("the scheme must be https (or http on localhost)")
	}
	return nil
}

// validatePeerAuth checks the block's SHAPE: everything that can be judged from
// the file alone. Resolving accounts to uids (internal/access.New) and checking
// slugs against the projects table (serve, doctor) come later, because neither
// belongs in a config load. The locked list is checked even while the block is
// disabled, so a typo is caught the day it is written rather
// than the day it is switched on. A filled-in users key is refused first, and
// alone: nothing else in such a block can be read the way it was written.
func (c Config) validatePeerAuth() error {
	p := c.PeerAuth
	if err := p.RetiredUsersRefusal(); err != nil {
		return err
	}
	if err := validatePeerLocked(p); err != nil {
		return err
	}
	if err := validatePeerRestricted(p); err != nil {
		return err
	}
	if err := validatePeerPasskeys(p.Passkeys); err != nil {
		return err
	}
	if m := time.Duration(p.MaxLease); m < 0 || m > maxMaxLease || m%time.Second != 0 {
		return fmt.Errorf("config: peer_auth.max_lease %s must be a whole number of seconds, at most %s (unset means %s)", m, maxMaxLease, DefaultMaxLease)
	}
	if !p.Enabled {
		return nil
	}
	if strings.TrimSpace(p.Owner) == "" {
		return fmt.Errorf("config: peer_auth.owner is required when peer_auth is enabled: name the OS account the TCP bearer key stands for")
	}
	if err := validateAccount("peer_auth.owner", p.Owner); err != nil {
		return err
	}
	path := strings.TrimSpace(p.Socket.Path)
	if path == "" {
		return fmt.Errorf("config: peer_auth.socket.path is required when peer_auth is enabled")
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("config: peer_auth.socket.path %q must be absolute", path)
	}
	if within(path, c.DataDir) {
		return fmt.Errorf("config: peer_auth.socket.path %q is inside data_dir %q, which is 0700: no caller could traverse to it -- put the socket in its own directory", path, c.DataDir)
	}
	if _, err := p.Socket.SocketFileMode(); err != nil {
		return err
	}
	if g := strings.TrimSpace(p.Socket.Group); g != "" {
		if err := validateAccount("peer_auth.socket.group", g); err != nil {
			return err
		}
	}
	return nil
}

// foldLegacyLocked moves the slugs under the old key, peer_auth.locked, into
// ForceLocked. A slug listed under both keys counts once; one listed twice under
// a single key stays twice, so validatePeerLocked still refuses it.
func (p *PeerAuth) foldLegacyLocked() {
	underNewKey := slices.Clone(p.ForceLocked)
	for _, slug := range p.LegacyLocked {
		if !slices.Contains(underNewKey, slug) {
			p.ForceLocked = append(p.ForceLocked, slug)
		}
	}
}

// validatePeerLocked is the locked list's shape: real project slugs, never the
// global scope, none twice.
func validatePeerLocked(p PeerAuth) error {
	seen := map[string]bool{}
	for _, raw := range p.ForceLocked {
		slug, err := validate.Project(raw)
		if err != nil {
			return fmt.Errorf("config: peer_auth.force_locked: %w", err)
		}
		if slug == "" {
			return fmt.Errorf("config: peer_auth.force_locked names the global scope: a global item is injected into every project's briefing, so global is never locked")
		}
		if seen[slug] {
			return fmt.Errorf("config: peer_auth.force_locked lists project %q twice: name it once", slug)
		}
		seen[slug] = true
	}
	return nil
}

// validatePeerRestricted is the restricted list's shape: every entry an OS
// account name or numeric uid, none twice, none the owner's, and none at all
// while peer_auth is disabled. A restricted account is known only through the
// socket's kernel credential, so a list that no listener could ever serve is a
// mistake to say out loud, not a setting to keep. Whether an
// entry resolves to the owner's uid (a numeric id naming the owner, say) needs
// the OS and is checked when the policy is built (access.New).
func validatePeerRestricted(p PeerAuth) error {
	if len(p.Restricted) == 0 {
		return nil
	}
	if !p.Enabled {
		return fmt.Errorf("config: peer_auth.restricted lists accounts while peer_auth is disabled: a restricted account " +
			"reaches Seamless only over the peer_auth socket -- enable peer_auth, or remove restricted")
	}
	owner := strings.TrimSpace(p.Owner)
	seen := map[string]bool{}
	for _, raw := range p.Restricted {
		if err := validateAccount("peer_auth.restricted entry", raw); err != nil {
			return err
		}
		if raw == owner {
			return fmt.Errorf("config: peer_auth.restricted lists %q, the owner: the owner is never restricted -- remove it", raw)
		}
		if seen[raw] {
			return fmt.Errorf("config: peer_auth.restricted lists %q twice: name each account once", raw)
		}
		seen[raw] = true
	}
	return nil
}

// validateAccount accepts an OS user or group name, or a numeric id. The real
// lookup happens at serve time; this only refuses what no lookup could match.
func validateAccount(key, name string) error {
	n := strings.TrimSpace(name)
	if n == "" || n != name || strings.ContainsAny(n, " \t\r\n:/") || strings.HasPrefix(n, "-") {
		return fmt.Errorf("config: %s %q is not an OS account name or numeric id", key, name)
	}
	return nil
}

// within reports whether path is dir or inside it, lexically.
func within(path, dir string) bool {
	if strings.TrimSpace(dir) == "" {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(path))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
