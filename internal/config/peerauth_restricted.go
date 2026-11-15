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

// restrictedEntryKeys are the keys a mapping entry may hold.
var restrictedEntryKeys = map[string]bool{"account": true, "exclude": true, "only": true}

// UnmarshalYAML reads a plain name or uid, or a mapping. The mapping's keys
// are checked here, one by one: a value decoded through yaml.Node.Decode does
// not inherit the outer decoder's KnownFields, so without this a mistyped
// `exlude:` would load as a plain entry and widen the account to the full
// reach (design note 01M7C9TQH76XF1D59P3N18EHP9, section 5.2).
func (r *RestrictedAccount) UnmarshalYAML(node *yaml.Node) error {
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
		return fmt.Errorf("config: peer_auth.restricted entry (line %d) is neither an account (a name or uid) nor a mapping "+
			"with account and exclude or only", n.Line)
	}
	var out RestrictedAccount
	var hasAccount, hasExclude, hasOnly bool
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, val := n.Content[i], resolveAlias(n.Content[i+1])
		if !restrictedEntryKeys[key.Value] {
			return fmt.Errorf("config: peer_auth.restricted entry (line %d) has unknown key %q: an entry maps account, and at "+
				"most one of exclude and only", key.Line, key.Value)
		}
		switch key.Value {
		case "account":
			if val.Kind != yaml.ScalarNode {
				return fmt.Errorf("config: peer_auth.restricted entry (line %d): account is an OS user name or uid", val.Line)
			}
			if err := val.Decode(&out.Account); err != nil {
				return err
			}
			hasAccount = true
		case "exclude", "only":
			if val.Kind != yaml.SequenceNode {
				return fmt.Errorf("config: peer_auth.restricted entry (line %d): %s is a list of project slugs", val.Line, key.Value)
			}
			var slugs []string
			if err := val.Decode(&slugs); err != nil {
				return fmt.Errorf("config: peer_auth.restricted entry (line %d): %s is a list of project slugs: %w", val.Line, key.Value, err)
			}
			if len(slugs) == 0 {
				return fmt.Errorf("config: peer_auth.restricted entry (line %d): %s is empty -- name its projects, or remove the key",
					val.Line, key.Value)
			}
			if key.Value == "exclude" {
				out.Exclude, hasExclude = slugs, true
			} else {
				out.Only, hasOnly = slugs, true
			}
		}
	}
	switch {
	case !hasAccount:
		return fmt.Errorf("config: peer_auth.restricted entry (line %d) has no account: name the OS user or uid", n.Line)
	case hasExclude && hasOnly:
		return fmt.Errorf("config: peer_auth.restricted entry %s (line %d) sets both exclude and only: use one -- only names "+
			"everything the account reaches, exclude what it never does", out.Account, n.Line)
	}
	*r = out
	return nil
}

// validateRestrictedShape is one entry's exclude or only list: real project
// slugs, never the global scope, none twice, and never both lists. It is the
// shape alone; whether each project exists is serve's and doctor's to check.
func validateRestrictedShape(e RestrictedAccount) error {
	if len(e.Exclude) > 0 && len(e.Only) > 0 {
		return fmt.Errorf("config: peer_auth.restricted %s sets both exclude and only: use one", e.Account)
	}
	for _, list := range []struct {
		key   string
		slugs []string
	}{{"exclude", e.Exclude}, {"only", e.Only}} {
		seen := map[string]bool{}
		for _, raw := range list.slugs {
			slug, err := validate.Project(raw)
			if err != nil {
				return fmt.Errorf("config: peer_auth.restricted %s %s: %w", e.Account, list.key, err)
			}
			if slug == "" {
				return fmt.Errorf("config: peer_auth.restricted %s %s names the global scope: it is read-only for every "+
					"restricted account, and never a project to name here", e.Account, list.key)
			}
			if slug != raw {
				return fmt.Errorf("config: peer_auth.restricted %s %s lists %q: write the slug without surrounding space", e.Account, list.key, raw)
			}
			if seen[slug] {
				return fmt.Errorf("config: peer_auth.restricted %s %s lists project %q twice: name it once", e.Account, list.key, slug)
			}
			seen[slug] = true
		}
	}
	return nil
}
