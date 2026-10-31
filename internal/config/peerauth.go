package config

import (
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/arctop/seamless/internal/validate"
)

// PeerAuth is peer-user authentication: a Unix-socket listener whose callers
// the daemon identifies by the uid the kernel reports, plus the map of which OS
// users may reach which projects (internal/access holds the policy itself).
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
	// Socket is the listener the role users (and, optionally, the owner) dial.
	Socket PeerSocket `yaml:"socket"`
	// Users maps an OS account (user name or numeric uid) to the projects it may
	// reach over the socket. An account not listed here, and not the owner, is
	// refused at the door.
	Users map[string]PeerUser `yaml:"users"`
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

// PeerUser is one OS account's reach.
type PeerUser struct {
	// Sealed lists projects reachable ONLY from this account's socket
	// connections: the owner and every other account are refused them. A
	// project is sealed to at most one account. (Not the "sealed" isolation
	// state: that one is about agents in one project reading others; this one is
	// about which OS user may reach a project at all.)
	Sealed []string `yaml:"sealed"`
	// Projects lists projects this account may also reach without sealing them:
	// the owner keeps them too.
	Projects []string `yaml:"projects"`
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

// SealedTo returns the configured account each sealed project belongs to.
func (p PeerAuth) SealedTo() map[string]string {
	out := map[string]string{}
	for account, u := range p.Users {
		for _, slug := range u.Sealed {
			out[strings.TrimSpace(slug)] = account
		}
	}
	return out
}

// ReferencedProjects returns every project slug the block names, sorted and
// de-duplicated: what serve and doctor check against the projects table.
func (p PeerAuth) ReferencedProjects() []string {
	var out []string
	for _, u := range p.Users {
		for _, slug := range slices.Concat(u.Sealed, u.Projects) {
			if s := strings.TrimSpace(slug); s != "" && !slices.Contains(out, s) {
				out = append(out, s)
			}
		}
	}
	slices.Sort(out)
	return out
}

// validatePeerAuth checks the block's SHAPE: everything that can be judged from
// the file alone. Resolving accounts to uids (internal/access.New) and checking
// slugs against the projects table (serve, doctor) come later, because neither
// belongs in a config load. The users map is checked even while the block is
// disabled, so a typo is caught the day it is written rather than the day it is
// switched on.
func (c Config) validatePeerAuth() error {
	p := c.PeerAuth
	if err := validatePeerUsers(p); err != nil {
		return err
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
	if _, listed := p.Users[strings.TrimSpace(p.Owner)]; listed {
		return fmt.Errorf("config: peer_auth.users lists %q, the owner: the owner reaches every project not sealed to another account, so it takes no users entry", p.Owner)
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

// validatePeerUsers is the users map's shape: every account named sanely, every
// slug a real project slug, no account reaching nothing, no project listed twice
// for one account, no project sealed to two accounts, and no project sealed to
// one account and shared with another.
func validatePeerUsers(p PeerAuth) error {
	accounts := make([]string, 0, len(p.Users))
	for account := range p.Users {
		accounts = append(accounts, account)
	}
	slices.Sort(accounts) // a deterministic first error
	sealedTo := map[string]string{}
	for _, account := range accounts {
		if err := validateAccount("peer_auth.users key", account); err != nil {
			return err
		}
		u := p.Users[account]
		if len(u.Sealed)+len(u.Projects) == 0 {
			return fmt.Errorf("config: peer_auth.users.%s reaches no project: list its sealed projects (or shared projects), or remove the entry", account)
		}
		seen := map[string]bool{}
		for _, list := range []struct {
			key   string
			slugs []string
		}{{"sealed", u.Sealed}, {"projects", u.Projects}} {
			for _, raw := range list.slugs {
				slug, err := validate.Project(raw)
				if err != nil {
					return fmt.Errorf("config: peer_auth.users.%s.%s: %w", account, list.key, err)
				}
				if slug == "" {
					return fmt.Errorf("config: peer_auth.users.%s.%s names the global scope: global is readable by every account and writable only by the owner, so it is never listed", account, list.key)
				}
				if seen[slug] {
					return fmt.Errorf("config: peer_auth.users.%s lists project %q twice: name it once, under sealed or under projects", account, slug)
				}
				seen[slug] = true
				if list.key == "sealed" {
					if other, dup := sealedTo[slug]; dup {
						return fmt.Errorf("config: project %q is sealed to two accounts (%s and %s): a sealed project belongs to exactly one", slug, other, account)
					}
					sealedTo[slug] = account
				}
			}
		}
	}
	// Overlap: a project sealed to one account and shared with another would put
	// it within reach of both, which is what sealing exists to rule out.
	for _, account := range accounts {
		for _, raw := range p.Users[account].Projects {
			slug := strings.TrimSpace(raw)
			if owner, sealed := sealedTo[slug]; sealed && owner != account {
				return fmt.Errorf("config: project %q is sealed to %s and also listed under peer_auth.users.%s.projects: a sealed project is reachable from its own account only", slug, owner, account)
			}
		}
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
