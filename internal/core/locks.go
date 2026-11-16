package core

// Repo locks (plan repo-locks): a lock guards a repository's refs -- its main
// branch, or a tag pattern -- so that agents on several machines land changes
// one at a time.
//
// A lock belongs to a repository, never to a project. Project mappings are path
// prefixes, not repositories: an umbrella mapping covers many repositories, one
// repository carved out of it maps to another project, and sessions of two
// projects work in one repository by design. A lock owned by a project could
// not be unique, and two projects could split the mutex. So a lock has no
// project, and its state carries no free text: the resource, the holder, since
// and until (owner decision D2). Its label is derived from the repository's
// basename and the lock's name, and every surface shows the two together
// ("LOCK seamless main"): "lease" already names peer_auth's approval leases,
// and "lock" also names a locked project.

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// RepoLockState is where a lock is in its life.
type RepoLockState string

const (
	// RepoLockRequested is a restricted account's declaration, waiting for the
	// owner to approve or discard it on the console. Nobody can hold it.
	RepoLockRequested RepoLockState = "requested"
	// RepoLockActive is a declared lock: free, or held by one session.
	RepoLockActive RepoLockState = "active"
)

// RepoLockStates lists every lock state.
var RepoLockStates = []RepoLockState{RepoLockRequested, RepoLockActive}

// Valid reports whether s is a lock state.
func (s RepoLockState) Valid() bool { return slices.Contains(RepoLockStates, s) }

// RepoLockAliasKind says how an alias names a repository.
type RepoLockAliasKind string

const (
	// RepoLockAliasOrigin is a normalized origin URL ("github.com/owner/repo"):
	// the identity every checkout of the repository shares, on every machine.
	RepoLockAliasOrigin RepoLockAliasKind = "origin"
	// RepoLockAliasPath is a machine and a main checkout root
	// ("example-host:/work/repo"): how a repository with no origin is locked,
	// recorded for every checkout seen too, so a checkout keeps its lock when
	// the repository gains or changes its origin.
	RepoLockAliasPath RepoLockAliasKind = "path"
)

// RepoLockAliasKinds lists every alias kind, origins first: the order a lock
// resolves in.
var RepoLockAliasKinds = []RepoLockAliasKind{RepoLockAliasOrigin, RepoLockAliasPath}

// RepoLockAlias is one repository identity that resolves to a lock.
type RepoLockAlias struct {
	Kind      RepoLockAliasKind `json:"kind"`
	Alias     string            `json:"alias"`
	CreatedAt time.Time         `json:"createdAt"`
}

// RepoLock is one lock on a repository's refs.
type RepoLock struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Refs are the guarded refs: full ref names (refs/heads/main) or patterns
	// with one '*' (refs/tags/v*), each one valid under ValidRepoLockRef.
	Refs  []string      `json:"refs"`
	State RepoLockState `json:"state"`
	// Holder is the holding session's ULID, "" when free. HeldSince is when
	// that hold began and LeaseExpiresAt when it lapses; both are nil when the
	// lock is free. A hold whose lease has lapsed still names its holder until
	// someone takes the lock or the holder releases it (HoldLive says which).
	Holder         string     `json:"holder,omitempty"`
	HeldSince      *time.Time `json:"heldSince,omitempty"`
	LeaseExpiresAt *time.Time `json:"leaseExpiresAt,omitempty"`
	// CreatedBy is the declaring session's ULID, "" when the declaration named
	// no session; Account is the restricted account that declared it, "" for
	// the owner principal.
	CreatedBy string    `json:"createdBy,omitempty"`
	Account   string    `json:"account,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	// Aliases are the repository identities that resolve to the lock, origins
	// first; Projects are the projects whose restricted accounts the
	// declaration lists as holders.
	Aliases  []RepoLockAlias `json:"aliases"`
	Projects []string        `json:"projects"`
}

// HoldLive reports whether the lock is held as of now: active, a holder, and an
// unexpired lease. It is Task.ClaimLive's rule exactly: After is strict, so a
// lease expiring at exactly now is already dead, and a lapsed hold is free to
// take.
func (l RepoLock) HoldLive(now time.Time) bool {
	return l.Holder != "" && l.State == RepoLockActive && l.LeaseExpiresAt != nil && l.LeaseExpiresAt.After(now)
}

// Repo is the repository's basename, from the lock's first alias: an origin's
// last path segment, else a checkout root's. "" for a lock with no alias.
func (l RepoLock) Repo() string {
	for _, kind := range RepoLockAliasKinds {
		for _, a := range l.Aliases {
			if a.Kind != kind {
				continue
			}
			p := a.Alias
			if kind == RepoLockAliasPath {
				// "host:path": the host never holds a ':' (a hostname cannot).
				if _, rest, ok := strings.Cut(p, ":"); ok {
					p = rest
				}
			}
			p = strings.TrimRight(p, `/\`)
			if i := strings.LastIndexAny(p, `/\`); i >= 0 {
				p = p[i+1:]
			}
			if p != "" {
				return p
			}
		}
	}
	return ""
}

// Label is how every surface names the lock: its repository and its name
// ("seamless main"). A line naming it reads "LOCK seamless main".
func (l RepoLock) Label() string {
	if repo := l.Repo(); repo != "" {
		return repo + " " + l.Name
	}
	return l.Name
}

// DefaultRepoLockName is the lock a declaration or a hold names when it names
// none: the one that guards the main branch.
const DefaultRepoLockName = "main"

// maxRepoLockName bounds a lock's name.
const maxRepoLockName = 64

// MaxRepoLockRefs bounds how many refs one lock guards.
const MaxRepoLockRefs = 16

// maxRepoLockRef bounds one guarded ref, as git bounds a ref name's component.
const maxRepoLockRef = 255

// ErrRepoLockName is a lock name that is not a token.
var ErrRepoLockName = errors.New("invalid lock name")

// ErrRepoLockRef is a guarded ref git would not accept as a ref name or a
// one-'*' pattern under refs/.
var ErrRepoLockRef = errors.New("invalid guarded ref")

// ValidRepoLockName reports whether name is a lock name: a token of lowercase
// ASCII letters, digits, '.', '_' and '-', at most 64 bytes, starting and
// ending with a letter or a digit, with no ".." and not ending in ".lock" (so
// refs/heads/<name>, the default guarded ref, is always a ref git accepts).
func ValidRepoLockName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: empty", ErrRepoLockName)
	}
	if len(name) > maxRepoLockName {
		return fmt.Errorf("%w: longer than %d bytes", ErrRepoLockName, maxRepoLockName)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		alnum := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
		switch {
		case alnum:
		case (c == '.' || c == '_' || c == '-') && i > 0 && i < len(name)-1:
		default:
			return fmt.Errorf("%w %q: a name is lowercase letters, digits, '.', '_' and '-', "+
				"starting and ending with a letter or a digit (main, release)", ErrRepoLockName, name)
		}
	}
	if strings.Contains(name, "..") || strings.HasSuffix(name, ".lock") {
		return fmt.Errorf("%w %q: no \"..\", and no \".lock\" at the end", ErrRepoLockName, name)
	}
	return nil
}

// ValidRepoLockRef reports whether ref is a guarded ref: a full ref name under
// refs/ (refs/heads/main) or a pattern with one '*' (refs/tags/v*), by git's
// check-ref-format rules with --refspec-pattern. Nothing is folded or trimmed:
// a ref that needs either is refused, never rewritten.
func ValidRepoLockRef(ref string) error {
	bad := func(why string) error { return fmt.Errorf("%w %q: %s", ErrRepoLockRef, ref, why) }
	switch {
	case ref == "":
		return fmt.Errorf("%w: empty", ErrRepoLockRef)
	case len(ref) > maxRepoLockRef:
		return bad(fmt.Sprintf("longer than %d bytes", maxRepoLockRef))
	case !strings.HasPrefix(ref, "refs/"):
		return bad("a guarded ref is a full ref name under refs/ (refs/heads/main, refs/tags/v*)")
	case strings.Count(ref, "*") > 1:
		return bad("a pattern holds one '*' at most")
	case strings.Contains(ref, ".."), strings.Contains(ref, "@{"), strings.Contains(ref, "//"):
		return bad(`no "..", "@{" or "//"`)
	case strings.HasSuffix(ref, "/"), strings.HasSuffix(ref, "."):
		return bad("no '/' or '.' at the end")
	}
	for _, r := range ref {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(" ~^:?[\\", r) {
			return bad(`no space, control character, or any of ~ ^ : ? [ \`)
		}
	}
	for part := range strings.SplitSeq(ref, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return bad(`no component that is empty, starts with '.' or ends in ".lock"`)
		}
	}
	return nil
}

// DefaultRepoLockRefs are the refs a lock guards when its declaration names
// none: the branch of the lock's own name.
func DefaultRepoLockRefs(name string) []string { return []string{"refs/heads/" + name} }

// LocalOriginPath reports whether origin, a git remote URL, names a repository
// on the same machine -- a path, absolute or relative, or a file:// URL -- and
// returns that path. A URL with any other scheme, or git's scp-like
// "host:path", names another machine's. "" is no origin at all.
func LocalOriginPath(origin string) (string, bool) {
	s := strings.TrimSpace(origin)
	if s == "" {
		return "", false
	}
	if len(s) >= len("file://") && strings.EqualFold(s[:len("file://")], "file://") {
		p := s[len("file://"):]
		if rest, ok := strings.CutPrefix(p, "localhost/"); ok {
			p = "/" + rest
		}
		if !strings.HasPrefix(p, "/") {
			return "", false // file://<another host>/...: not this machine's path
		}
		return p, true
	}
	if strings.Contains(s, "://") {
		return "", false
	}
	if driveRooted(s) || strings.HasPrefix(s, `\\`) {
		return s, true
	}
	// git's rule: a ':' before the first slash is an scp-like remote.
	colon, slash := strings.IndexByte(s, ':'), strings.IndexAny(s, `/\`)
	if colon >= 0 && (slash < 0 || colon < slash) {
		return "", false
	}
	return s, true
}

// driveRooted reports a Windows drive-rooted path ("C:\repo", "c:/repo").
func driveRooted(p string) bool {
	if len(p) < 3 || p[1] != ':' || (p[2] != '\\' && p[2] != '/') {
		return false
	}
	c := p[0] | 0x20
	return c >= 'a' && c <= 'z'
}

// maxOriginHops bounds FollowLocalOrigin: a clone of a clone of a clone is
// plausible, and a longer chain is a loop.
const maxOriginHops = 8

// FollowLocalOrigin is the origin a lock keys a checkout on: a side clone whose
// origin is another checkout on the same machine is a checkout of that
// checkout's repository. It follows the chain with read, which reports the
// main checkout root and the origin of the repository at a path ("" and ""
// when the path is no checkout: a bare repository, or nothing), and returns the
// first origin in the chain that is not a local path, or else the path of the
// last local repository, by which the lock then keys the checkout. A relative
// path is read against the checkout that named it (base, then each hop's main
// root). An origin that is not a local path is returned as it is, and "" stays
// "". Only a caller on the machine the paths name may call it: read opens them.
func FollowLocalOrigin(origin, base string, read func(path string) (mainRoot, origin string)) string {
	cur, from := strings.TrimSpace(origin), base
	last := ""
	seen := map[string]bool{}
	for range maxOriginHops {
		p, local := LocalOriginPath(cur)
		if !local {
			return cur
		}
		if !filepath.IsAbs(p) && from != "" {
			p = filepath.Join(from, p)
		}
		p = filepath.Clean(p)
		if seen[p] {
			return p // a loop: key on where it closes
		}
		seen[p] = true
		main, next := read(p)
		if main == "" {
			return p
		}
		last = main
		if strings.TrimSpace(next) == "" {
			return main
		}
		cur, from = strings.TrimSpace(next), main
	}
	return last
}

// EventRepoLockChanged is the log's record of a repo lock change (item_id: the
// lock; session_id: the session that acted, or the holder a release freed;
// payload: action -- one of the RepoLockAction words -- and name, plus
// lease_seconds, reclaimed and prior_holder for a hold, aliases for an alias
// record, and by for the console's own actions). Never a path or a free word.
const EventRepoLockChanged EventKind = "repo_lock.changed"

// The RepoLockAction words, the action of an EventRepoLockChanged.
const (
	RepoLockActionDeclared      = "declared"       // the owner principal declared a lock
	RepoLockActionRequested     = "requested"      // a restricted account's declaration waits for the owner
	RepoLockActionAliased       = "aliased"        // a declaration or a hold recorded new aliases of an existing lock
	RepoLockActionAcquired      = "acquired"       // a session took the lock
	RepoLockActionHeartbeat     = "heartbeat"      // its holder refreshed the lease
	RepoLockActionReleased      = "released"       // its holder let it go
	RepoLockActionForceReleased = "force_released" // the owner let it go, from the console
	RepoLockActionApproved      = "approved"       // the owner approved a waiting declaration
	RepoLockActionDiscarded     = "discarded"      // the owner discarded a waiting declaration
)
