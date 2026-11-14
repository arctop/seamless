// Package files is the markdown layer: memory and note files are the source of
// truth for durable knowledge. It parses/serializes YAML frontmatter, writes
// atomically, keeps the SQLite index mirrors in sync, and watches the trees for
// out-of-band edits. Ported from Seam v1 (internal/note + internal/watcher).
package files

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// splitDocument splits a markdown file into its YAML frontmatter text and body.
// A document with no leading "---\n" delimiter has empty frontmatter and the
// whole content as body. Ported from v1 note.ParseFrontmatter (delimiter logic).
func splitDocument(content string) (yamlText, body string) {
	content = strings.ReplaceAll(content, "\r\n", "\n")

	if !strings.HasPrefix(content, "---\n") {
		return "", content
	}
	rest := content[4:] // skip leading "---\n"

	yamlText, body, found := strings.Cut(rest, "\n---")
	if !found {
		// Empty frontmatter block written as "---\n---\n".
		if strings.HasPrefix(rest, "---\n") || rest == "---" {
			b := strings.TrimPrefix(rest, "---")
			return "", strings.TrimPrefix(b, "\n")
		}
		// No closing delimiter: the whole thing is the body.
		return "", content
	}
	return yamlText, strings.TrimPrefix(body, "\n")
}

// wrapDocument renders frontmatter YAML and a body into full file content,
// always ending with a trailing newline.
func wrapDocument(yamlBytes []byte, body string) string {
	var b strings.Builder
	b.WriteString("---\n")
	b.Write(yamlBytes)
	if len(yamlBytes) > 0 && yamlBytes[len(yamlBytes)-1] != '\n' {
		b.WriteByte('\n')
	}
	b.WriteString("---\n")
	if body != "" {
		b.WriteString(body)
		if !strings.HasSuffix(body, "\n") {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// yaml.Node helpers (control key ordering + null emission)
// ---------------------------------------------------------------------------

func scalarNode(s string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Value: s}
}

func nullNode() *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}
}

// stringOrNull emits a plain scalar, or an explicit null for the empty string.
func stringOrNull(s string) *yaml.Node {
	if s == "" {
		return nullNode()
	}
	return scalarNode(s)
}

func boolNode(v bool) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: fmt.Sprintf("%t", v)}
}

func flowSeqNode(items []string) *yaml.Node {
	n := &yaml.Node{Kind: yaml.SequenceNode, Style: yaml.FlowStyle}
	for _, it := range items {
		n.Content = append(n.Content, scalarNode(it))
	}
	return n
}

// mapBuilder accumulates ordered key/value pairs for a MappingNode.
type mapBuilder struct{ n *yaml.Node }

func newMap() *mapBuilder { return &mapBuilder{n: &yaml.Node{Kind: yaml.MappingNode}} }

func (b *mapBuilder) put(key string, val *yaml.Node) {
	b.n.Content = append(b.n.Content, scalarNode(key), val)
}

// putIf appends key/value only when value is non-empty.
func (b *mapBuilder) putIf(key, val string) {
	if val != "" {
		b.put(key, scalarNode(val))
	}
}

// putExtra appends unknown keys in sorted order so re-serialization is stable.
func (b *mapBuilder) putExtra(extra map[string]any) error {
	if len(extra) == 0 {
		return nil
	}
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := &yaml.Node{}
		if err := v.Encode(extra[k]); err != nil {
			return fmt.Errorf("files: encode extra %q: %w", k, err)
		}
		b.put(k, v)
	}
	return nil
}

// captureExtra decodes value into raw and copies any key not in known into extra.
func captureExtra(value *yaml.Node, known map[string]bool) (map[string]any, error) {
	var raw map[string]any
	if err := value.Decode(&raw); err != nil {
		return nil, err
	}
	var extra map[string]any
	for k, v := range raw {
		if known[k] {
			continue
		}
		if extra == nil {
			extra = make(map[string]any)
		}
		extra[k] = v
	}
	return extra, nil
}

// ---------------------------------------------------------------------------
// Memory frontmatter
// ---------------------------------------------------------------------------

var memoryKnownKeys = map[string]bool{
	"id": true, "kind": true, "name": true, "description": true,
	"project": true, "created": true, "updated": true, "valid_from": true,
	"invalid_at": true, "superseded_by": true, "source_session": true,
	"model": true, "favorite": true, "tags": true, "account": true,
	stampsKey: true,
}

// memoryFrontmatter mirrors a memory file's YAML frontmatter. Timestamps are
// kept as strings here (RFC3339 on disk) and converted at the core boundary, so
// this layer never depends on yaml's timestamp resolution. Extra preserves
// unknown keys (e.g. Obsidian plugin fields) across a round-trip.
type memoryFrontmatter struct {
	ID            string         `yaml:"id"`
	Kind          string         `yaml:"kind"`
	Name          string         `yaml:"name"`
	Description   string         `yaml:"description"`
	Project       string         `yaml:"project"`
	Created       string         `yaml:"created"`
	Updated       string         `yaml:"updated"`
	ValidFrom     string         `yaml:"valid_from"`
	InvalidAt     string         `yaml:"invalid_at"`
	SupersededBy  string         `yaml:"superseded_by"`
	SourceSession string         `yaml:"source_session"`
	Model         string         `yaml:"model"`
	Account       string         `yaml:"account"`
	Favorite      bool           `yaml:"favorite"`
	Tags          []string       `yaml:"tags"`
	MayContain    []stampEntry   `yaml:"may_contain"`
	Extra         map[string]any `yaml:"-"`
}

func (fm *memoryFrontmatter) UnmarshalYAML(value *yaml.Node) error {
	type plain memoryFrontmatter
	if err := value.Decode((*plain)(fm)); err != nil {
		return err
	}
	extra, err := captureExtra(value, memoryKnownKeys)
	if err != nil {
		return err
	}
	fm.Extra = extra
	return nil
}

func (fm *memoryFrontmatter) MarshalYAML() (any, error) {
	b := newMap()
	b.put("id", scalarNode(fm.ID))
	b.put("kind", scalarNode(fm.Kind))
	b.put("name", scalarNode(fm.Name))
	b.put("description", scalarNode(fm.Description))
	b.putIf("project", fm.Project)
	b.put("created", scalarNode(fm.Created))
	b.put("updated", scalarNode(fm.Updated))
	b.put("valid_from", scalarNode(fm.ValidFrom))
	b.put("invalid_at", stringOrNull(fm.InvalidAt))
	b.put("superseded_by", stringOrNull(fm.SupersededBy))
	b.putIf("source_session", fm.SourceSession)
	b.putIf("model", fm.Model)
	b.putIf("account", fm.Account)
	if fm.Favorite {
		b.put("favorite", boolNode(true))
	}
	if len(fm.Tags) > 0 {
		b.put("tags", flowSeqNode(fm.Tags))
	}
	if err := b.putStamps(fm.MayContain); err != nil {
		return nil, err
	}
	if err := b.putExtra(extraBeside(fm.Extra, fm.MayContain)); err != nil {
		return nil, err
	}
	return b.n, nil
}

// ---------------------------------------------------------------------------
// Note frontmatter
// ---------------------------------------------------------------------------

var noteKnownKeys = map[string]bool{
	"id": true, "title": true, "slug": true, "description": true,
	"project": true, "created": true, "updated": true, "source_url": true,
	"model": true, "favorite": true, "tags": true, "account": true,
	stampsKey: true,
}

// noteFrontmatter mirrors a note file's YAML frontmatter.
type noteFrontmatter struct {
	ID          string         `yaml:"id"`
	Title       string         `yaml:"title"`
	Slug        string         `yaml:"slug"`
	Description string         `yaml:"description"`
	Project     string         `yaml:"project"`
	Created     string         `yaml:"created"`
	Updated     string         `yaml:"updated"`
	SourceURL   string         `yaml:"source_url"`
	Model       string         `yaml:"model"`
	Account     string         `yaml:"account"`
	Favorite    bool           `yaml:"favorite"`
	Tags        []string       `yaml:"tags"`
	MayContain  []stampEntry   `yaml:"may_contain"`
	Extra       map[string]any `yaml:"-"`
}

func (fm *noteFrontmatter) UnmarshalYAML(value *yaml.Node) error {
	type plain noteFrontmatter
	if err := value.Decode((*plain)(fm)); err != nil {
		return err
	}
	extra, err := captureExtra(value, noteKnownKeys)
	if err != nil {
		return err
	}
	fm.Extra = extra
	return nil
}

func (fm *noteFrontmatter) MarshalYAML() (any, error) {
	b := newMap()
	b.put("id", scalarNode(fm.ID))
	b.put("title", scalarNode(fm.Title))
	b.putIf("slug", fm.Slug)
	b.putIf("description", fm.Description)
	b.putIf("project", fm.Project)
	b.put("created", scalarNode(fm.Created))
	b.put("updated", scalarNode(fm.Updated))
	b.putIf("source_url", fm.SourceURL)
	b.putIf("model", fm.Model)
	b.putIf("account", fm.Account)
	if fm.Favorite {
		b.put("favorite", boolNode(true))
	}
	if len(fm.Tags) > 0 {
		b.put("tags", flowSeqNode(fm.Tags))
	}
	if err := b.putStamps(fm.MayContain); err != nil {
		return nil, err
	}
	if err := b.putExtra(extraBeside(fm.Extra, fm.MayContain)); err != nil {
		return nil, err
	}
	return b.n, nil
}
