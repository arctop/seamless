package files

import (
	"fmt"
	"sort"

	"gopkg.in/yaml.v3"

	"github.com/arctop/seamless/internal/core"
)

// Provenance stamps in frontmatter (core.Stamp, plan fenced-sessions P6): the
// may_contain key of a memory or a note, one entry per fenced project the item
// may contain content from, naming what cleared the write under its kind:
//
//	may_contain:
//	    - project: example-legal
//	      grant: 01M7...
//
// The key is only ever added, and every entry round-trips: a key of an entry
// this release does not know (one a newer release added) is kept in the
// entry's Extra and written back after the known ones, and an entry naming no
// reference this release knows keeps its project and its keys. The index
// mirror (store.ProvenanceMirror) is built from what parses here.

const stampsKey = "may_contain"

// stampEntry is one may_contain entry as frontmatter carries it.
type stampEntry core.Stamp

// UnmarshalYAML reads a mapping ({project, grant|move|session, ...}); a bare
// scalar reads as a project with no reference. Any other shape is an error, as
// it is for every known key of the frontmatter.
func (e *stampEntry) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		var project string
		if err := value.Decode(&project); err != nil {
			return fmt.Errorf("%s entry: %w", stampsKey, err)
		}
		*e = stampEntry{Project: project}
		return nil
	case yaml.MappingNode:
	default:
		return fmt.Errorf("%s entry at line %d: want a mapping with a project", stampsKey, value.Line)
	}
	var raw map[string]any
	if err := value.Decode(&raw); err != nil {
		return fmt.Errorf("%s entry: %w", stampsKey, err)
	}
	out := stampEntry{}
	if p, ok := raw["project"].(string); ok {
		out.Project = p
		delete(raw, "project")
	}
	// The first reference in the canonical order names the entry's kind; any
	// other stays an unknown key, so the choice never depends on map order.
	for _, via := range core.StampVias {
		if ref, ok := raw[via].(string); ok && ref != "" {
			out.Via, out.Ref = via, ref
			delete(raw, via)
			break
		}
	}
	if len(raw) > 0 {
		out.Extra = raw
	}
	*e = out
	return nil
}

// MarshalYAML writes the project, the reference under its kind, then the
// unknown keys in sorted order, so a re-render is stable.
func (e stampEntry) MarshalYAML() (any, error) {
	b := newMap()
	b.put("project", scalarNode(e.Project))
	if e.Via != "" && e.Ref != "" {
		b.put(e.Via, scalarNode(e.Ref))
	}
	keys := make([]string, 0, len(e.Extra))
	for k := range e.Extra {
		if k == "project" || (k == e.Via && e.Ref != "") {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := &yaml.Node{}
		if err := v.Encode(e.Extra[k]); err != nil {
			return nil, fmt.Errorf("files: encode %s entry key %q: %w", stampsKey, k, err)
		}
		b.put(k, v)
	}
	return b.n, nil
}

func stampEntries(stamps []core.Stamp) []stampEntry {
	if len(stamps) == 0 {
		return nil
	}
	out := make([]stampEntry, len(stamps))
	for i, s := range stamps {
		out[i] = stampEntry(s)
	}
	return out
}

func stampsOf(entries []stampEntry) []core.Stamp {
	if len(entries) == 0 {
		return nil
	}
	out := make([]core.Stamp, len(entries))
	for i, e := range entries {
		out[i] = core.Stamp(e)
	}
	return out
}

// putStamps appends may_contain when there is any stamp: a file with none
// renders exactly as it did before the key existed.
func (b *mapBuilder) putStamps(entries []stampEntry) error {
	if len(entries) == 0 {
		return nil
	}
	seq := &yaml.Node{Kind: yaml.SequenceNode}
	for _, e := range entries {
		v := &yaml.Node{}
		if err := v.Encode(e); err != nil {
			return fmt.Errorf("files: encode %s: %w", stampsKey, err)
		}
		seq.Content = append(seq.Content, v)
	}
	b.put(stampsKey, seq)
	return nil
}

// extraBeside is extra without a may_contain of its own when the stamps are
// written: one key, one value. Parsing never leaves the key in Extra; a caller
// that built one by hand loses it to the stamps it set.
func extraBeside(extra map[string]any, entries []stampEntry) map[string]any {
	if len(entries) == 0 {
		return extra
	}
	if _, dup := extra[stampsKey]; !dup {
		return extra
	}
	out := make(map[string]any, len(extra)-1)
	for k, v := range extra {
		if k != stampsKey {
			out[k] = v
		}
	}
	return out
}
