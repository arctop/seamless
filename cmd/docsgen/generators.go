package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/console"
	"github.com/arctop/seamless/internal/features"
	seamlessmcp "github.com/arctop/seamless/internal/mcp"
)

// A generator emits markdown that renderPages appends to a page's authored body,
// so generated content flows through goldmark like prose and inherits heading
// anchors, the TOC rail, and the search index for free.
//
// Generators read the code they document -- the MCP catalog, the config defaults
// -- rather than a transcription of it. That is the point: a tool or key that
// changes shows up in the docs on the next `make docs`, and `make docs-check`
// fails the build if nobody ran it.
type generator func(p *Page, srcDir string) (string, error)

var generators = map[string]generator{
	"mcp-tools":      generateMCPTools,
	"config":         generateConfig,
	"console-levels": generateConsoleLevels,
}

func generate(name string, p *Page, srcDir string) (string, error) {
	g, ok := generators[name]
	if !ok {
		known := make([]string, 0, len(generators))
		for k := range generators {
			known = append(known, k)
		}
		sort.Strings(known)
		return "", fmt.Errorf("unknown generator (known: %s)", strings.Join(known, ", "))
	}
	return g(p, srcDir)
}

// ---------------------------------------------------------------------------
// mcp-tools

// toolPartialDir holds optional per-tool supplements: docs-src/.../_tools/<name>.md
// is appended inside that tool's generated section. The underscore marks it a
// partial, so nav validation does not demand it appear in the sidebar (see
// isPartial). Generated material states what a tool takes; the partial is where
// a real captured call, its response, and its failure modes go -- those must be
// written by hand from a live instance, never invented by a generator.
const toolPartialDir = "_tools"

func generateMCPTools(p *Page, srcDir string) (string, error) {
	if len(p.Tools) == 0 {
		return "", fmt.Errorf("generator mcp-tools needs a `tools:` list in the page frontmatter")
	}
	catalog := make(map[string]mcp.Tool, len(seamlessmcp.Catalog()))
	for _, t := range seamlessmcp.Catalog() {
		catalog[t.Name] = t
	}

	optional := optionalToolNotes()

	var b strings.Builder
	for _, name := range p.Tools {
		tool, ok := catalog[name]
		if !ok {
			return "", fmt.Errorf("no such MCP tool %q (it must appear in mcp.Catalog())", name)
		}
		// Pin the anchor to the tool's real name: /reference/mcp/tasks/#tasks_claim.
		fmt.Fprintf(&b, "## %s {#%s}\n\n%s\n\n", tool.Name, tool.Name, escapeText(tool.Description))
		if note := optional[tool.Name]; note != "" {
			b.WriteString(note)
			b.WriteString("\n\n")
		}

		params, err := toolParams(tool)
		if err != nil {
			return "", fmt.Errorf("%s: %w", tool.Name, err)
		}
		if len(params) == 0 {
			b.WriteString("Takes no parameters.\n\n")
		} else {
			b.WriteString("| Parameter | Type | Required | Description |\n|---|---|---|---|\n")
			for _, param := range params {
				required := "no"
				if param.required {
					required = "**yes**"
				}
				fmt.Fprintf(&b, "| `%s` | %s | %s | %s |\n", param.name, param.typeName, required, param.description)
			}
			b.WriteString("\n")
		}

		partial, err := readPartial(filepath.Join(srcDir, filepath.Dir(p.Src), toolPartialDir, name+".md"))
		if err != nil {
			return "", err
		}
		if partial != "" {
			b.WriteString(partial)
			b.WriteString("\n\n")
		}
	}
	return b.String(), nil
}

// optionalToolNotes maps every tool an OPTIONAL feature owns to the sentence its
// generated section carries. The set comes from features.Registry(), never from
// a list here: a tool that becomes optional annotates itself on the next
// `make docs`, and one that stops being optional loses the line the same way.
func optionalToolNotes() map[string]string {
	notes := make(map[string]string)
	for _, f := range features.Registry() {
		for _, name := range f.Tools {
			notes[name] = fmt.Sprintf("**Optional** - part of *%s*, hidden when the `%s` feature is disabled: "+
				"the tool leaves `tools/list` and a call to it is refused as an unknown tool, while its stored data "+
				"stays exactly where it was. See [Optional features](/reference/console/#optional-features).",
				escapeText(f.Label), escapeText(string(f.Key)))
		}
	}
	return notes
}

// toolParam is one row of a tool's parameter table.
type toolParam struct {
	name        string
	typeName    string
	description string
	required    bool
}

// toolParams flattens a tool's JSON-Schema properties into table rows: required
// parameters first in their declared order, then the optional ones sorted by
// name (so the output is stable regardless of Go's map iteration).
//
// Every unrecognized schema shape is an error, not a skipped row. An mcp-go
// upgrade that reshapes Properties must fail the build loudly rather than
// quietly emit empty parameter tables that read as "this tool takes nothing".
func toolParams(tool mcp.Tool) ([]toolParam, error) {
	required := make(map[string]bool, len(tool.InputSchema.Required))
	for _, name := range tool.InputSchema.Required {
		if _, ok := tool.InputSchema.Properties[name]; !ok {
			return nil, fmt.Errorf("schema requires %q but declares no such property", name)
		}
		required[name] = true
	}

	optional := make([]string, 0, len(tool.InputSchema.Properties))
	for name := range tool.InputSchema.Properties {
		if !required[name] {
			optional = append(optional, name)
		}
	}
	sort.Strings(optional)

	names := append(append([]string{}, tool.InputSchema.Required...), optional...)
	out := make([]toolParam, 0, len(names))
	for _, name := range names {
		spec, ok := tool.InputSchema.Properties[name].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("property %q: expected a JSON-Schema object, got %T", name, tool.InputSchema.Properties[name])
		}
		typeName, ok := spec["type"].(string)
		if !ok {
			return nil, fmt.Errorf("property %q: schema has no string \"type\"", name)
		}
		desc, _ := spec["description"].(string)
		if enum, ok := spec["enum"]; ok {
			values, err := enumValues(enum)
			if err != nil {
				return nil, fmt.Errorf("property %q: %w", name, err)
			}
			desc = strings.TrimRight(strings.TrimSpace(desc), ".") + ". One of: " + values + "."
			desc = strings.TrimPrefix(desc, ". ")
		}
		out = append(out, toolParam{
			name:        name,
			typeName:    typeName,
			description: escapeCell(desc),
			required:    required[name],
		})
	}
	return out, nil
}

// enumValues renders a property's allowed values. mcp.Enum() builds []string,
// but a schema round-tripped through JSON arrives as []any, and both are valid
// inputs -- so accept either and reject anything else.
func enumValues(enum any) (string, error) {
	var values []string
	switch list := enum.(type) {
	case []string:
		values = list
	case []any:
		for _, v := range list {
			s, ok := v.(string)
			if !ok {
				return "", fmt.Errorf("enum: expected string values, got %T", v)
			}
			values = append(values, s)
		}
	default:
		return "", fmt.Errorf("enum: expected a list of strings, got %T", enum)
	}
	out := make([]string, 0, len(values))
	for _, v := range values {
		out = append(out, "`"+v+"`")
	}
	return strings.Join(out, ", "), nil
}

// escapeText makes a plain-text string from the code (a tool description, a
// config default) safe to embed in generated markdown.
//
// The docs renderer runs with html.WithUnsafe, because authored pages embed raw
// HTML. Generated text is not authored: descriptions like "pass plan=<slug>"
// would otherwise reach the browser as an unknown <slug> element and render as
// nothing at all -- the text silently disappears rather than failing loudly. So
// every generated string is HTML-escaped before it becomes markdown.
func escapeText(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// escapeCell makes text safe inside a GFM table cell: escapeText, plus pipes and
// newlines, either of which would shred the row into extra columns or end the
// table early. Pipes are not hypothetical -- several tool descriptions spell out
// their accepted values as "startup|resume|clear|compact|explicit".
func escapeCell(s string) string {
	s = escapeText(s)
	s = strings.ReplaceAll(s, "|", "\\|")
	return strings.Join(strings.Fields(s), " ")
}

// escapeCodeCell escapes a value that will sit inside a code span in a table
// cell. Only the pipe needs escaping: goldmark HTML-escapes code-span content
// itself, so running escapeText here too would double-encode and publish a
// default of "&amp;lt;" where the code says "<".
func escapeCodeCell(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, "|", "\\|")), " ")
}

// readPartial returns a partial's contents, or "" when it does not exist.
func readPartial(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read partial %s: %w", path, err)
	}
	return strings.TrimSpace(string(raw)), nil
}

// ---------------------------------------------------------------------------
// console-levels

// generateConsoleLevels renders the console's "what each level shows" matrix
// from its own registries (console.LevelMatrix): the screens, the Settings
// sections, and the in-page surfaces, each against the three levels. The
// levels themselves come from config.ConsoleLevels.
func generateConsoleLevels(_ *Page, _ string) (string, error) {
	levels := config.ConsoleLevels
	rows := console.LevelMatrix()
	var b strings.Builder
	for _, group := range []string{console.MatrixScreens, console.MatrixSections, console.MatrixSurfaces} {
		fmt.Fprintf(&b, "### %s\n\n", group)
		b.WriteString("| |")
		for _, lvl := range levels {
			fmt.Fprintf(&b, " %s |", titleCase(lvl))
		}
		b.WriteString("\n|---|")
		for range levels {
			b.WriteString("---|")
		}
		b.WriteString("\n")
		n := 0
		for _, r := range rows {
			if r.Group != group {
				continue
			}
			first := slices.Index(levels, r.Min)
			if first < 0 {
				return "", fmt.Errorf("%s %q: unknown level %q", group, r.Name, r.Min)
			}
			name := "**" + escapeCell(r.Name) + "**"
			if r.Note != "" {
				name += " - " + escapeCell(r.Note)
			}
			fmt.Fprintf(&b, "| %s |", name)
			for i := range levels {
				cell := "-"
				if i >= first {
					cell = "shown"
				}
				fmt.Fprintf(&b, " %s |", cell)
			}
			b.WriteString("\n")
			n++
		}
		if n == 0 {
			return "", fmt.Errorf("console.LevelMatrix has no %q rows", group)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n") + "\n", nil
}

// titleCase upper-cases a level key's first letter ("basic" -> "Basic").
func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// ---------------------------------------------------------------------------
// config

func generateConfig(_ *Page, _ string) (string, error) {
	var b strings.Builder
	b.WriteString("## Every key\n\n")
	b.WriteString("| Key | Type | Default |\n|---|---|---|\n")

	rows, err := configRows(reflect.ValueOf(config.Defaults()), "")
	if err != nil {
		return "", err
	}
	for _, r := range rows {
		b.WriteString(r)
	}

	example, err := os.ReadFile("seamless.yaml.example")
	if err != nil {
		return "", fmt.Errorf("read seamless.yaml.example (docsgen must run from the repo root): %w", err)
	}
	b.WriteString("\n## Annotated example\n\n")
	b.WriteString("This is `seamless.yaml.example` from the repository, verbatim.\n\n")
	b.WriteString("```yaml\n")
	b.WriteString(strings.TrimRight(string(example), "\n"))
	b.WriteString("\n```\n")
	return b.String(), nil
}

// configRows walks a config struct in field-declaration order (never map order)
// emitting one table row per leaf key, recursing into nested config structs to
// build dotted paths like `llm.openai.chat_model`.
func configRows(v reflect.Value, prefix string) ([]string, error) {
	var rows []string
	t := v.Type()
	for i := range t.NumField() {
		field := t.Field(i)
		if !field.IsExported() {
			continue // e.g. Config.sourcePath: internal bookkeeping, not a key
		}
		tag, _, _ := strings.Cut(field.Tag.Get("yaml"), ",")
		if tag == "" || tag == "-" {
			continue
		}
		key := tag
		if prefix != "" {
			key = prefix + "." + tag
		}
		fv := v.Field(i)
		if fv.Kind() == reflect.Struct {
			nested, err := configRows(fv, key)
			if err != nil {
				return nil, err
			}
			rows = append(rows, nested...)
			continue
		}
		rows = append(rows, fmt.Sprintf("| `%s` | %s | %s |\n", key, configType(fv.Type()), formatDefault(fv)))
	}
	return rows, nil
}

// durationType is config.Duration, which the table names for how it is
// written rather than for the int64 it is.
var durationType = reflect.TypeFor[config.Duration]()

// configType renders a key's Type column: the Go type (string, int, []int),
// except where the Go spelling would mislead someone writing YAML. A
// config.Duration is written as 6h, so it reads "duration". A pointer is an
// optional key -- unset is a state of its own, which the build or another
// setting decides -- so *bool reads "bool, optional".
//
// A map reads as what its YAML looks like: the key type, then the value's own
// keys when it is a struct (peer_auth.users maps an account to {sealed,
// projects}), rather than a Go type name the YAML never spells.
func configType(t reflect.Type) string {
	switch {
	case t == durationType:
		return "duration"
	case t.Kind() == reflect.Pointer:
		return configType(t.Elem()) + ", optional"
	case t.Kind() == reflect.Map:
		elem := t.Elem()
		if elem.Kind() != reflect.Struct {
			return "map of " + configType(t.Key()) + " to " + configType(elem)
		}
		var fields []string
		for i := range elem.NumField() {
			f := elem.Field(i)
			tag, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
			if !f.IsExported() || tag == "" || tag == "-" {
				continue
			}
			fields = append(fields, tag+" "+configType(f.Type))
		}
		return "map of " + configType(t.Key()) + " to {" + strings.Join(fields, ", ") + "}"
	default:
		return t.String()
	}
}

// formatDefault renders a default value for the table. A zero value prints as a
// dash rather than `""` or `0`: the useful statement is "no default", and an
// empty-looking code span reads like a typo. An optional (pointer) key whose
// default is nil prints "unset" instead: that is a state with a meaning of its
// own, not the absence of a default.
func formatDefault(v reflect.Value) string {
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return "unset"
		}
		return formatDefault(v.Elem())
	}
	if v.IsZero() {
		return "-"
	}
	switch v.Kind() {
	case reflect.String:
		return "`" + escapeCodeCell(v.String()) + "`"
	case reflect.Slice:
		parts := make([]string, 0, v.Len())
		for i := range v.Len() {
			parts = append(parts, fmt.Sprintf("%v", v.Index(i).Interface()))
		}
		return "`[" + strings.Join(parts, ", ") + "]`"
	default:
		// %v honors fmt.Stringer, which is how a config.Duration prints as 6h
		// rather than as its nanosecond count.
		return fmt.Sprintf("`%v`", v.Interface())
	}
}
