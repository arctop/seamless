package config

import (
	"fmt"

	"gopkg.in/yaml.v3"

	"github.com/arctop/seamless/internal/validate"
)

// RestrictedAccount is one entry of peer_auth.restricted: an OS account (a
// user name or a numeric uid) and, optionally, the shape of its reach (plan
// agent-accounts-and-releases, step B; memory restricted-account-reach-decisions).
//
// In the file an entry is either a plain name or uid, which reaches what every
// restricted account reached before shapes existed -- every open project that
// is not locked, reading and writing, and the global scope read-only -- or a
// mapping:
//
//	restricted:
//	  - _example_helper                            # plain
//	  - account: _example_agent
//	    exclude: [example-pipeline, example-vault] # every open, unlocked project but these
//	  - account: _example_desk
//	    only: [example-desk]                       # only these projects
//
// `only` names any number of open projects, or exactly one confidential or
// sealed project on its own, which the account then reads and writes: a reach
// that holds a fenced project holds nothing else (internal/access, WithReach).
// `exclude` and `only` never appear together. The reach itself is judged per
// request against the projects table; the config only names it.
//
// The mapping form was added after v0.8.5: a release before it refuses the
// entry at load.
type RestrictedAccount struct {
	// Account is the OS user name or numeric uid.
	Account string
	// Exclude names open projects the account never reaches, in any state.
	Exclude []string
	// Only names the projects the account reaches, and nothing else.
	Only []string
}

// BareRestricted returns plain entries for accounts: the form with no shape,
// as peer_auth.restricted: [a, b] reads.
func BareRestricted(accounts ...string) []RestrictedAccount {
	out := make([]RestrictedAccount, len(accounts))
	for i, a := range accounts {
		out[i] = RestrictedAccount{Account: a}
	}
	return out
}

// RestrictedNames returns the accounts entries name, in order.
func RestrictedNames(entries []RestrictedAccount) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Account
	}
	return out
}

// Shaped reports whether the entry narrows its reach (exclude) or names it
// (only). A plain entry is not shaped.
func (r RestrictedAccount) Shaped() bool { return len(r.Exclude) > 0 || len(r.Only) > 0 }

// UnmarshalYAML reads a plain name or uid, or a mapping. The mapping's keys
// are checked here, one by one (readShapedEntry): a value decoded through
// yaml.Node.Decode does not inherit the outer decoder's KnownFields, so
// without this a mistyped `exlude:` would load as a plain entry and widen the
// account to the full reach (design note 01M7C9TQH76XF1D59P3N18EHP9, section
// 5.2).
func (r *RestrictedAccount) UnmarshalYAML(node *yaml.Node) error {
	const what = "peer_auth.restricted entry"
	n := resolveAlias(node)
	switch n.Kind {
	case yaml.ScalarNode:
		var name string
		if err := n.Decode(&name); err != nil {
			return err
		}
		*r = RestrictedAccount{Account: name}
		return nil
	case yaml.MappingNode:
	default:
		return fmt.Errorf("config: %s (line %d) is neither an account (a name or uid) nor a mapping "+
			"with account and exclude or only", what, n.Line)
	}
	e, err := readShapedEntry(n, what, "account")
	if err != nil {
		return err
	}
	out := RestrictedAccount{Exclude: e.exclude, Only: e.only}
	switch {
	case e.id == nil:
		return fmt.Errorf("config: %s (line %d) has no account: name the OS user or uid", what, n.Line)
	case e.id.Kind != yaml.ScalarNode:
		return fmt.Errorf("config: %s (line %d): account is an OS user name or uid", what, e.id.Line)
	}
	if err := e.id.Decode(&out.Account); err != nil {
		return err
	}
	if e.exclude != nil && e.only != nil {
		return fmt.Errorf("config: %s %s (line %d) sets both exclude and only: use one -- only names "+
			"everything the account reaches, exclude what it never does", what, out.Account, n.Line)
	}
	*r = out
	return nil
}

// shapedEntry is one mapping entry as readShapedEntry read it: the value of
// its identifying key (nil when the entry has none), and its exclude and only
// lists (nil when the entry has no such key, never empty).
type shapedEntry struct {
	id      *yaml.Node
	exclude []string
	only    []string
}

// readShapedEntry reads a mapping entry that shapes a restricted account's
// reach: idKey (account under peer_auth.restricted, uid under a relay's
// restricted), exclude and only. Every key is checked here. A custom
// UnmarshalYAML receives the raw node, so the decoder's own checks never see
// it: neither KnownFields, nor the refusal of a key that appears twice. Both
// would widen an account without a word -- a mistyped `exlude:` dropped, or
// the first of two `exclude:` lists replaced by the second -- so an unknown
// key and a repeated key are each refused with its name and line. what names
// the entry in the refusals.
func readShapedEntry(n *yaml.Node, what, idKey string) (shapedEntry, error) {
	var out shapedEntry
	seen := map[string]int{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, val := n.Content[i], resolveAlias(n.Content[i+1])
		switch key.Value {
		case idKey, "exclude", "only":
		default:
			return shapedEntry{}, fmt.Errorf("config: %s (line %d) has unknown key %q: an entry maps %s, and at "+
				"most one of exclude and only", what, key.Line, key.Value, idKey)
		}
		if first, twice := seen[key.Value]; twice {
			return shapedEntry{}, fmt.Errorf("config: %s (line %d) sets %s twice (first on line %d): an entry holds "+
				"each key once", what, key.Line, key.Value, first)
		}
		seen[key.Value] = key.Line
		if key.Value == idKey {
			out.id = val
			continue
		}
		if val.Kind != yaml.SequenceNode {
			return shapedEntry{}, fmt.Errorf("config: %s (line %d): %s is a list of project slugs", what, val.Line, key.Value)
		}
		var slugs []string
		if err := val.Decode(&slugs); err != nil {
			return shapedEntry{}, fmt.Errorf("config: %s (line %d): %s is a list of project slugs: %w", what, val.Line, key.Value, err)
		}
		if len(slugs) == 0 {
			return shapedEntry{}, fmt.Errorf("config: %s (line %d): %s is empty -- name its projects, or remove the key",
				what, val.Line, key.Value)
		}
		if key.Value == "exclude" {
			out.exclude = slugs
		} else {
			out.only = slugs
		}
	}
	return out, nil
}

// validateRestrictedShape is one entry's exclude or only list
// (validateReachShape), named as peer_auth.restricted names the entry.
func validateRestrictedShape(e RestrictedAccount) error {
	return validateReachShape("peer_auth.restricted "+e.Account, e.Exclude, e.Only)
}

// validateReachShape is one restricted entry's exclude or only list: real
// project slugs, never the global scope, none twice, and never both lists. It
// is the shape alone; whether each project exists is serve's and doctor's to
// check. where names the entry in the refusals.
func validateReachShape(where string, exclude, only []string) error {
	if len(exclude) > 0 && len(only) > 0 {
		return fmt.Errorf("config: %s sets both exclude and only: use one", where)
	}
	for _, list := range []struct {
		key   string
		slugs []string
	}{{"exclude", exclude}, {"only", only}} {
		seen := map[string]bool{}
		for _, raw := range list.slugs {
			slug, err := validate.Project(raw)
			if err != nil {
				return fmt.Errorf("config: %s %s: %w", where, list.key, err)
			}
			if slug == "" {
				return fmt.Errorf("config: %s %s names the global scope: it is read-only for every "+
					"restricted account, and never a project to name here", where, list.key)
			}
			if slug != raw {
				return fmt.Errorf("config: %s %s lists %q: write the slug without surrounding space", where, list.key, raw)
			}
			if seen[slug] {
				return fmt.Errorf("config: %s %s lists project %q twice: name it once", where, list.key, slug)
			}
			seen[slug] = true
		}
	}
	return nil
}

// RelayRestricted is one restricted account on a relay's computer, an entry
// of peer_auth.relays.<relay>.restricted (plan networked-relay): its uid
// there and, optionally, the shape of its reach, as RestrictedAccount is for
// an account on this machine.
//
// In the file the map's value is either the uid, which reaches what such an
// account reached before shapes existed -- every open project that is not
// locked, reading and writing, and the global scope read-only -- or a
// mapping:
//
//	relays:
//	  example-laptop:
//	    owner_uid: 501
//	    restricted:
//	      _example_helper: 502                 # plain
//	      _example_agent:
//	        uid: 503
//	        exclude: [example-pipeline]        # every open, unlocked project but these
//	      _example_desk:
//	        uid: 504
//	        only: [example-desk]               # only these projects
//
// exclude and only mean what they mean under peer_auth.restricted, with the
// same rule (internal/access, WithReach): only names any number of open
// projects, or exactly one confidential or sealed project on its own, and the
// two never appear together.
//
// The mapping form was added after v0.10.2: a release before it refuses the
// entry at load.
type RelayRestricted struct {
	// UID is the account's numeric uid on the relay's computer.
	UID uint32
	// Exclude names open projects the account never reaches, in any state.
	Exclude []string
	// Only names the projects the account reaches, and nothing else.
	Only []string
}

// BareRelayRestricted returns plain entries for a relay's accounts, by name:
// the form with no shape, as restricted: {a: 502} reads.
func BareRelayRestricted(uids map[string]uint32) map[string]RelayRestricted {
	out := make(map[string]RelayRestricted, len(uids))
	for name, uid := range uids {
		out[name] = RelayRestricted{UID: uid}
	}
	return out
}

// Shaped reports whether the entry narrows its reach (exclude) or names it
// (only). A plain entry is not shaped.
func (r RelayRestricted) Shaped() bool { return len(r.Exclude) > 0 || len(r.Only) > 0 }

// UnmarshalYAML reads a uid, or a mapping with uid and at most one of exclude
// and only. A scalar is decoded exactly as the map's uint32 value was before
// shapes existed, so every value that loaded then loads the same now, and
// every one that was refused still is. The mapping's keys are checked one by
// one (readShapedEntry).
func (r *RelayRestricted) UnmarshalYAML(node *yaml.Node) error {
	const what = "peer_auth.relays restricted entry"
	n := resolveAlias(node)
	switch n.Kind {
	case yaml.ScalarNode:
		var uid uint32
		if err := n.Decode(&uid); err != nil {
			return err
		}
		*r = RelayRestricted{UID: uid}
		return nil
	case yaml.MappingNode:
	default:
		return fmt.Errorf("config: %s (line %d) is neither a uid nor a mapping with uid and exclude or only", what, n.Line)
	}
	e, err := readShapedEntry(n, what, "uid")
	if err != nil {
		return err
	}
	out := RelayRestricted{Exclude: e.exclude, Only: e.only}
	switch {
	case e.id == nil:
		return fmt.Errorf("config: %s (line %d) has no uid: name the account's uid on that computer (id -u <name> there)", what, n.Line)
	case e.id.Kind != yaml.ScalarNode:
		return fmt.Errorf("config: %s (line %d): uid is the account's numeric uid on that computer", what, e.id.Line)
	}
	if err := e.id.Decode(&out.UID); err != nil {
		return err
	}
	if e.exclude != nil && e.only != nil {
		return fmt.Errorf("config: %s for uid %d (line %d) sets both exclude and only: use one -- only names "+
			"everything the account reaches, exclude what it never does", what, out.UID, n.Line)
	}
	*r = out
	return nil
}
