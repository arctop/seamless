package mcp

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/arctop/seamless/internal/core"
)

// Argument normalization and validation against a tool's declared input schema.
//
// mcp.Required() and mcp.Enum() are advertising, not enforcement: mcp-go's
// handleToolCall looks the tool up and dispatches, with no jsonschema pass in
// between. Everything downstream then reads arguments through GetString/GetInt,
// which collapse absent, wrong-type, and typo'd-name into the same zero value --
// so a bad argument becomes a confident, plausible, wrong answer instead of a
// fix. That is the "no fake results on error" rule (AGENTS.md > Meta-rules) never
// having been applied to the input boundary.
//
// The rule these functions enforce:
//
//	Absent -> default. Present but uninterpretable -> error.
//
// Flexible where intent is unambiguous and lossless (array-or-CSV,
// number-or-numeric-string, body/content/text). Loud where intent is unknowable
// (unknown param, invalid enum, conflicting aliases).
//
// Scope is the deliberate exception: nothing here has an opinion about
// "project". resolveWriteScope/resolveReadScope already refuse to guess
// (errNoScope/errAmbiguousScope), and a second guard would either duplicate or
// contradict them.
//
// These are pure functions; validateMiddleware is what calls them.

// maxSafeInteger is the largest magnitude a number argument may carry. GetInt
// does int(v) on a float64 (mcp-go mcp/tools.go:129), which is
// implementation-defined once the value exceeds the integer range, so an
// out-of-range number is rejected here rather than silently becoming garbage.
// It is JSON's own safe-integer bound: past it, float64 cannot represent
// consecutive integers and the value the agent sent is not the value it meant.
const maxSafeInteger = 1 << 53

// aliasGroups maps a canonical parameter name to the alternate names accepted
// for it, and is keyed by the canonical name on purpose: the aliases apply only
// to a tool that actually declares the canonical property. That matches what
// argBody did (it tried body/content/text on every tool, and every tool calling
// it declares "body"), while keeping "content" an unknown parameter on a tool
// with no body concept, such as recall.
//
// These aliases are load-bearing, not politeness: per the field logs
// (mcp-tool-failures-mostly-infra-not-reasoning) ~11% of real agent tool
// failures were memory_append being sent "body" instead of "content".
// slug/name likewise: memories are addressed by name and notes by slug, and
// the gardener's tool-error evidence showed agents sending notes_read the
// other vocabulary. A tool declaring both (project_create) is unaffected --
// an exactly-declared name always wins over an alias.
// findings/summary is the same story for session_update/session_end: "summary"
// is the word agents reach for when closing out work, and it was the most common
// rejected parameter on session_end in the tool.call log.
var aliasGroups = map[string][]string{
	"body":     {"content", "text"},
	"slug":     {"name"},
	"findings": {"summary"},
}

// retiredParams are parameters a tool no longer declares but still accepts, so a
// client written against the old schema keeps working: a value the parameter
// always accepted is dropped before validation, and any other value is refused,
// exactly as it would have been. They are not advertised -- the point of retiring
// one is that agents stop seeing it -- and dropping a valid legacy value is not
// a fallback: the tool now works the answer out itself.
//
// session_start.source was Claude Code hook vocabulary (startup|resume|clear|
// compact) leaking into an agent-facing tool. Agents read "source" as the client
// and sent claude-code or codex, or set it to change what they believed was the
// session's lifecycle. The server now derives it from what the call actually
// did; the integration docs once told custom clients to send source=explicit.
var retiredParams = map[string]map[string][]string{
	"session_start": {"source": core.SessionSources},
}

// dropRetired removes a tool's retired parameters from raw (see retiredParams),
// refusing a value the parameter never accepted. raw is not mutated: a copy is
// made when anything is dropped, because the interactions feed records what the
// agent actually sent.
func dropRetired(tool string, raw map[string]any) (map[string]any, error) {
	retired := retiredParams[tool]
	if len(retired) == 0 {
		return raw, nil
	}
	out, copied := raw, false
	for _, name := range slices.Sorted(maps.Keys(retired)) {
		v, ok := raw[name]
		if !ok {
			continue
		}
		if s, isString := v.(string); !isString || !slices.Contains(retired[name], s) {
			shown := fmt.Sprint(v)
			if isString {
				shown = strconv.Quote(s)
			}
			return nil, fmt.Errorf("invalid %s %s: %s no longer takes %s -- omit it", name, shown, tool, name)
		}
		if !copied {
			out, copied = maps.Clone(raw), true
		}
		delete(out, name)
	}
	return out, nil
}

// recallKindScopes maps the item kinds recall's hits carry -- every hit says
// "kind":"note" (or memory, task, trial, session) -- to the scope that selects
// them, and each scope word to itself. recall's own kind parameter takes only a
// memory's frontmatter kind, but an agent that has just read a hit labelled
// kind note sends kind=note to search notes: the tool's output taught it that
// word under that name. The two vocabularies are disjoint
// (TestRecallKindScopes_DisjointFromMemoryKinds), so the value names its scope
// without a guess, and reading it as that scope loses nothing. The gardener's
// tool-error pass found agents sending kind=note, task and session across
// sessions, each one a refused call.
var recallKindScopes = map[string]string{
	core.ItemKindMemory: "memories", "memories": "memories",
	core.ItemKindNote: "notes", "notes": "notes",
	core.ItemKindTask: "tasks", "tasks": "tasks",
	core.ItemKindTrial: "trials", "trials": "trials",
	core.ItemKindSession: "sessions", "sessions": "sessions",
}

// recallKindAsScope rewrites recall's kind, when it names an item kind or a
// scope rather than a memory kind, into the scope it selects (see
// recallKindScopes). It refuses rather than picks when that scope contradicts
// an explicit one (only all, the default, and the same scope agree with it),
// and when no query rides along: kind alone browses a memory kind, and a scope
// has no browse to read it as. raw is not mutated (see dropRetired).
func recallKindAsScope(raw map[string]any) (map[string]any, error) {
	kind, ok := raw["kind"].(string)
	if !ok {
		return raw, nil
	}
	scope, ok := recallKindScopes[kind]
	if !ok {
		return raw, nil
	}
	if q, ok := raw["query"]; !ok || q == "" {
		kinds := make([]string, len(core.MemoryKinds))
		for i, k := range core.MemoryKinds {
			kinds[i] = string(k)
		}
		return nil, fmt.Errorf("invalid kind %q: kind with no query lists one memory kind (%s); to search %s, pass scope=%s with a query",
			kind, strings.Join(kinds, ", "), scope, scope)
	}
	if s, present := raw["scope"]; present && s != "" && s != "all" && s != scope {
		return nil, fmt.Errorf("conflicting values for scope: kind %q selects scope %s, but scope is %v; pass scope alone",
			kind, scope, s)
	}
	out := maps.Clone(raw)
	delete(out, "kind")
	out["scope"] = scope
	return out, nil
}

// argRewrites are per-tool rewrites applied before normalizeArgs, each for a
// value the tool's own vocabulary teaches agents to send under the wrong
// parameter. A rewrite must have exactly one reading of the value it rewrites;
// a value it cannot read without a guess is left for normalizeArgs to refuse.
var argRewrites = map[string]func(map[string]any) (map[string]any, error){
	"recall": recallKindAsScope,
}

// rewriteArgs applies tool's rewrite (see argRewrites), if it has one.
func rewriteArgs(tool string, raw map[string]any) (map[string]any, error) {
	if rw, ok := argRewrites[tool]; ok {
		return rw(raw)
	}
	return raw, nil
}

// aliasesFor returns the alternate names accepted for a canonical parameter.
func aliasesFor(prop string) []string {
	return aliasGroups[prop]
}

// namesFor returns a parameter's canonical name followed by its aliases, in the
// order a caller should read them ("body, content, text").
func namesFor(prop string) []string {
	return append([]string{prop}, aliasesFor(prop)...)
}

// knownParams returns every parameter name the schema accepts -- declared
// properties plus the aliases of those properties -- sorted.
//
// Sorted because Properties is a map: an unsorted list would make the error text
// nondeterministic and flake every test that reads it.
func knownParams(schema mcp.ToolInputSchema) []string {
	out := make([]string, 0, len(schema.Properties))
	for name := range schema.Properties {
		out = append(out, name)
		out = append(out, aliasesFor(name)...)
	}
	slices.Sort(out)
	return out
}

// canonicalOf maps an incoming argument name to the schema property it feeds,
// resolving aliases. It reports false for a name the schema does not accept.
func canonicalOf(schema mcp.ToolInputSchema, key string) (string, bool) {
	if _, ok := schema.Properties[key]; ok {
		return key, true
	}
	for canon, aliases := range aliasGroups {
		if _, declared := schema.Properties[canon]; !declared {
			continue
		}
		if slices.Contains(aliases, key) {
			return canon, true
		}
	}
	return "", false
}

// propSchema returns the declared schema for a property. A property whose schema
// is not an object is a defect in this package's own tool constructors, not
// caller input, so it fails closed rather than passing the value through
// unvalidated.
func propSchema(schema mcp.ToolInputSchema, name string) (map[string]any, error) {
	p, ok := schema.Properties[name].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("internal error: parameter %q has an unreadable schema", name)
	}
	return p, nil
}

// normalizeArgs validates raw against the tool's declared schema and returns the
// arguments the handler should see: aliases collapsed onto their canonical name,
// legacy string forms coerced to their declared type, and absent keys absent.
//
// The order is load-bearing: unknown-param -> alias collapse -> empty-string
// rule -> required -> coerce -> enum -> range. Required runs after the
// empty-string rule so that a required enum sent as "" reports itself missing
// rather than invalid; coerce runs before enum and range so those compare
// against a value of the declared type.
//
// The returned map is always a fresh allocation: the caller hands it to the
// handler while keeping raw for the interactions feed, which records what the
// agent actually sent.
func normalizeArgs(schema mcp.ToolInputSchema, raw map[string]any) (map[string]any, error) {
	// Incoming keys are visited in sorted order throughout, so that a call with
	// two problems always reports the same one first.
	incoming := make([]string, 0, len(raw))
	for k := range raw {
		incoming = append(incoming, k)
	}
	slices.Sort(incoming)

	// Unknown parameters, and bucketing by canonical name for the collapse.
	present := make(map[string][]string, len(raw))
	canons := make([]string, 0, len(raw))
	for _, k := range incoming {
		canon, ok := canonicalOf(schema, k)
		if !ok {
			return nil, unknownParamError(k, knownParams(schema))
		}
		if len(present[canon]) == 0 {
			canons = append(canons, canon)
		}
		present[canon] = append(present[canon], k)
	}

	out := make(map[string]any, len(raw))
	for _, canon := range canons {
		v, err := collapseAliases(canon, present[canon], raw)
		if err != nil {
			return nil, err
		}
		out[canon] = v
	}

	// Empty-string rule, then required. Both need the schema.
	for _, canon := range canons {
		prop, err := propSchema(schema, canon)
		if err != nil {
			return nil, err
		}
		drop, err := emptyMeansAbsent(prop, out[canon], canon)
		if err != nil {
			return nil, err
		}
		if drop {
			delete(out, canon)
		}
	}

	var missing []string
	for _, name := range schema.Required {
		if _, ok := out[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return nil, missingRequiredError(schema, missing)
	}

	// Coerce -> enum -> range, per parameter. Sorted for the same determinism.
	remaining := make([]string, 0, len(out))
	for k := range out {
		remaining = append(remaining, k)
	}
	slices.Sort(remaining)

	for _, name := range remaining {
		prop, err := propSchema(schema, name)
		if err != nil {
			return nil, err
		}
		v, drop, err := coerceProp(prop, out[name], name)
		if err != nil {
			return nil, err
		}
		if drop {
			delete(out, name)
			continue
		}
		out[name] = v

		if err := checkEnum(prop, v, name); err != nil {
			return nil, err
		}
		if err := checkRange(prop, v, name); err != nil {
			return nil, err
		}
	}

	return out, nil
}

// collapseAliases reduces the names a parameter arrived under to one value.
//
// Zero present is absent (the caller never gets here); one present is taken
// verbatim and untrimmed, because a markdown body's leading whitespace is
// content; two or more that agree collapse; two that differ is an error. Picking
// a winner would silently discard one of two explicit intents -- which is the
// disagreement the helpers this replaces shipped with (argBody took the first
// non-blank, firstStringArg took the first present).
func collapseAliases(canon string, keys []string, raw map[string]any) (any, error) {
	first := raw[keys[0]]
	for _, k := range keys[1:] {
		if !reflect.DeepEqual(raw[k], first) {
			return nil, fmt.Errorf("conflicting values for %s: %s and %s differ; pass exactly one of %s",
				canon, keys[0], k, strings.Join(namesFor(canon), ", "))
		}
	}
	return first, nil
}

// emptyMeansAbsent reports whether an empty string should be read as "the key
// was not sent".
//
// It applies only where the value is interpreted -- an enum, array, number, or
// object -- never to a free-form string, where "" is a real value (blanking a
// body). This is a compatibility carve-out with a named beneficiary: seam's own
// CLI sends depends_on:"" plan:"" project:"" body:"" unconditionally
// (cmd/seam/task.go:88), so those must read as absent while body:"" still
// blanks. It is deliberately no wider than that -- a boolean sent as "" is
// uninterpretable and errors in coercion, because no client emits one and
// "present but uninterpretable -> error" is the rule this carve-out is an
// exception to.
func emptyMeansAbsent(prop map[string]any, v any, name string) (bool, error) {
	s, ok := v.(string)
	if !ok || s != "" {
		return false, nil
	}
	if _, hasEnum, err := enumValues(prop, name); err != nil {
		return false, err
	} else if hasEnum {
		return true, nil
	}
	switch propType(prop) {
	case "array", "number", "integer", "object":
		return true, nil
	}
	return false, nil
}

// propType returns a property's declared JSON type, or "" when it declares none
// (mcp.WithAny). A typeless property accepts any JSON value by construction, so
// coercion skips it rather than inventing a type for it.
func propType(prop map[string]any) string {
	t, _ := prop["type"].(string)
	return t
}

// coerceProp converts a value to its declared type, accepting the legacy string
// forms this repo's clients actually emit. It reports drop when the value means
// "absent" after conversion.
func coerceProp(prop map[string]any, v any, name string) (out any, drop bool, err error) {
	switch propType(prop) {
	case "string":
		s, err := coerceString(v, name)
		return s, false, err
	case "number":
		f, err := coerceNumber(v, name)
		return f, false, err
	case "integer":
		f, err := coerceInteger(v, name)
		return f, false, err
	case "array":
		// An array of objects is its own shape (memory_edit/notes_edit take a
		// list of {old_string, new_string} edits), and running it through
		// coerceStrings would reject every element as "expected a string" --
		// a type error about the caller's correct input. The declared item type
		// is what tells the two apart, so the schema stays the single source.
		if itemsType(prop) == "object" {
			list, err := coerceObjects(v, name)
			if err != nil {
				return nil, false, err
			}
			return list, len(list) == 0, nil
		}
		list, err := coerceStrings(v, name)
		if err != nil {
			return nil, false, err
		}
		// An empty array drops the key rather than yielding []string{}: a
		// handler that distinguishes "sent nothing" from "sent an empty list"
		// (tasks_update reads presence to decide which fields changed) must keep
		// reading absent as absent.
		return list, len(list) == 0, nil
	case "object":
		m, err := coerceObject(v, name)
		return m, false, err
	case "boolean":
		b, err := coerceBool(v, name)
		return b, false, err
	default:
		return v, false, nil
	}
}

// coerceString accepts a string and nothing else.
//
// The non-coercion is deliberate: rendering {"title": 42} as "42" invents a
// title the agent never wrote. (Today it becomes "" and tasks_add answers "title
// is required" -- a lie about which of the two problems occurred.) Coercion
// exists for legacy forms real clients emit, not to make every input fit.
func coerceString(v any, key string) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("invalid %s: expected a string, got %s", key, jsonTypeName(v))
	}
	return s, nil
}

// coerceNumber accepts a JSON number or a numeric string (the form seam's CLI
// and older agents send).
func coerceNumber(v any, key string) (float64, error) {
	switch t := v.(type) {
	case float64:
		return checkFinite(t, key)
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil {
			return 0, fmt.Errorf("invalid %s: expected a number, got %q", key, t)
		}
		return checkFinite(f, key)
	default:
		return 0, fmt.Errorf("invalid %s: expected a number, got %s", key, jsonTypeName(v))
	}
}

// coerceInteger accepts the same native and legacy string forms as
// coerceNumber, then enforces mathematical integrality. JSON decoders represent
// both 1 and 1.0 as float64, so checking the value -- rather than its lexical
// spelling -- is the only stable integer rule.
func coerceInteger(v any, key string) (float64, error) {
	f, err := coerceNumber(v, key)
	if err != nil {
		return 0, err
	}
	if math.Trunc(f) != f {
		return 0, fmt.Errorf("invalid %s: expected an integer, got %s", key, formatNumber(f))
	}
	return f, nil
}

// checkFinite rejects the float64 values that cannot survive the trip to an int.
// NaN and the infinities are unreachable from JSON but not from a numeric string
// -- ParseFloat accepts "NaN" and "Inf" -- which is the whole reason this runs on
// both paths.
func checkFinite(f float64, key string) (float64, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("invalid %s: expected a finite number", key)
	}
	if math.Abs(f) > maxSafeInteger {
		return 0, fmt.Errorf("invalid %s: %s is too large to be represented exactly", key, formatNumber(f))
	}
	return f, nil
}

// coerceStrings accepts an array of strings or a comma-separated string. Both
// forms trim and drop blanks, so ["a"] and "a" -- and [" a ", ""] and " a ," --
// normalize identically. That equivalence is the point: it is what makes the
// legacy CSV form lossless rather than a second, subtly different code path.
func coerceStrings(v any, key string) ([]string, error) {
	switch t := v.(type) {
	case []any:
		out := make([]string, 0, len(t))
		for i, e := range t {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("invalid %s[%d]: expected a string, got %s", key, i, jsonTypeName(e))
			}
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		return out, nil
	case string:
		return parseCommaList(t), nil
	default:
		return nil, fmt.Errorf("invalid %s: expected an array of strings or a comma-separated string, got %s",
			key, jsonTypeName(v))
	}
}

// itemsType returns the declared type of an array property's elements, or ""
// when the property declares no item schema. mcp.Items stores whatever the call
// site handed it, and a JSON-decoded schema carries map[string]any, so both are
// read; anything else reports "" and the property falls back to the string-array
// path, which is what every array property in this package was before.
func itemsType(prop map[string]any) string {
	items, ok := prop["items"].(map[string]any)
	if !ok {
		return ""
	}
	t, _ := items["type"].(string)
	return t
}

// coerceObjects accepts an array of objects, or a JSON-array string (the form an
// agent sends when it stringifies its own arguments). Unlike coerceStrings it
// drops nothing: an element of an edit list has no "blank" reading, and silently
// discarding one would apply a different edit set than the caller asked for.
func coerceObjects(v any, key string) ([]map[string]any, error) {
	switch t := v.(type) {
	case []any:
		out := make([]map[string]any, 0, len(t))
		for i, e := range t {
			m, ok := e.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("invalid %s[%d]: expected an object, got %s", key, i, jsonTypeName(e))
			}
			out = append(out, m)
		}
		return out, nil
	case []map[string]any:
		return t, nil
	case string:
		var raw []any
		if err := json.Unmarshal([]byte(strings.TrimSpace(t)), &raw); err != nil {
			return nil, fmt.Errorf("invalid %s: expected an array of objects, got %q", key, t)
		}
		return coerceObjects(raw, key)
	default:
		return nil, fmt.Errorf("invalid %s: expected an array of objects, got %s", key, jsonTypeName(v))
	}
}

// coerceObject accepts an object or a JSON-object string (the form an agent
// sends when it stringifies its own arguments).
func coerceObject(v any, key string) (map[string]any, error) {
	switch t := v.(type) {
	case map[string]any:
		return t, nil
	case string:
		var m map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(t)), &m); err != nil || m == nil {
			// m == nil covers the literal "null", which unmarshals into a map
			// without error and would otherwise pass as an empty object.
			return nil, fmt.Errorf("invalid %s: expected a JSON object, got %q", key, t)
		}
		return m, nil
	default:
		return nil, fmt.Errorf("invalid %s: expected an object, got %s", key, jsonTypeName(v))
	}
}

// coerceBool accepts a boolean or the strings "true"/"false".
func coerceBool(v any, key string) (bool, error) {
	switch t := v.(type) {
	case bool:
		return t, nil
	case string:
		switch strings.TrimSpace(t) {
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
		return false, fmt.Errorf("invalid %s: expected true or false, got %q", key, t)
	default:
		return false, fmt.Errorf("invalid %s: expected true or false, got %s", key, jsonTypeName(v))
	}
}

// enumValues returns a property's allowed values, in the order they were
// declared -- not sorted. The canonical sets carry meaning in their order
// (open, in_progress, done, dropped is a lifecycle, not an alphabet), and they
// are slices, so the determinism that forces knownParams to sort does not apply.
//
// mcp.Enum stores a []string and every schema validated here is built in-process,
// but ToolInputSchema has an UnmarshalJSON, and a schema that arrived that way
// carries []any -- so both are read. Any other shape is an error rather than a
// skip: silently not enforcing a declared enum is precisely the failure this
// file exists to remove.
func enumValues(prop map[string]any, name string) ([]string, bool, error) {
	v, ok := prop["enum"]
	if !ok {
		return nil, false, nil
	}
	switch t := v.(type) {
	case []string:
		return t, true, nil
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			s, ok := e.(string)
			if !ok {
				return nil, false, fmt.Errorf("internal error: parameter %q declares a non-string enum value", name)
			}
			out = append(out, s)
		}
		return out, true, nil
	default:
		return nil, false, fmt.Errorf("internal error: parameter %q declares an unreadable enum", name)
	}
}

// checkEnum enforces a declared enum. It applies to string values only: an enum
// is only ever declared on a string property here, and coercion has already run,
// so anything else is a property this validator has no opinion about.
func checkEnum(prop map[string]any, v any, name string) error {
	values, ok, err := enumValues(prop, name)
	if err != nil || !ok {
		return err
	}
	s, isString := v.(string)
	if !isString {
		return nil
	}
	if slices.Contains(values, s) {
		return nil
	}
	return fmt.Errorf("invalid %s %q: valid values are %s", name, s, strings.Join(values, ", "))
}

// checkRange enforces mcp.Min/mcp.Max. The handler keeps its own domain guards:
// the validator owns type, the handler owns range and domain (tasks_claim's
// lease_seconds documents an int64-overflow hazard a schema bound cannot
// express).
func checkRange(prop map[string]any, v any, name string) error {
	f, ok := v.(float64)
	if !ok {
		return nil
	}
	if lo, ok := numberBound(prop, "minimum"); ok && f < lo {
		return fmt.Errorf("invalid %s: must be >= %s", name, formatNumber(lo))
	}
	if hi, ok := numberBound(prop, "maximum"); ok && f > hi {
		return fmt.Errorf("invalid %s: must be <= %s", name, formatNumber(hi))
	}
	return nil
}

// numberBound reads a minimum/maximum bound. The stored type depends on how the
// bound got there: a JSON-decoded schema yields float64, but mcp.Min/mcp.Max are
// generic over int|int64|float64 and store whatever the call site wrote, so
// mcp.Min(1) stores an int. Every case is handled because the miss is silent --
// an unreadable bound drops the range check entirely rather than failing loudly.
func numberBound(prop map[string]any, key string) (float64, bool) {
	switch v := prop[key].(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	default:
		return 0, false
	}
}

// formatNumber renders a bound or value the way an agent wrote it: 1, not 1.
func formatNumber(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// jsonTypeName names a value's JSON type, for error text that tells the caller
// what it actually sent.
func jsonTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case float64, int, int64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return fmt.Sprintf("%T", v)
	}
}

// paramRedirects maps a rejected parameter name to a pointer at the tool the
// caller most likely wants: an agent sending query= is trying to search, and
// the fix is a different call, not a different spelling. A tool that declares
// the parameter (recall declares query) never reaches the unknown path, so the
// hint only ever shows where it applies.
var paramRedirects = map[string]string{
	"query": "to search by text, use the recall tool (recall query=<text>)",
}

// unknownParamError names the bad parameter, suggests the intended one when it
// can, and lists what the tool accepts. errResult prefixes the tool name.
func unknownParamError(key string, known []string) error {
	suffix := ""
	if hint, ok := paramRedirects[key]; ok {
		suffix = "; " + hint
	}
	if s := suggestParam(key, known); s != "" {
		return fmt.Errorf("unknown parameter %q: did you mean %q? valid parameters are: %s%s",
			key, s, strings.Join(known, ", "), suffix)
	}
	return fmt.Errorf("unknown parameter %q: valid parameters are: %s%s", key, strings.Join(known, ", "), suffix)
}

// missingRequiredError reports every absent required parameter in one message --
// not one per round-trip -- and, for each, the hints that let a caller fix it
// without a second rejection: the alternate names an aliased parameter accepts,
// and the allowed values of an enum. That levels these up to the self-correcting
// enum/scope errors -- an agent that omitted kind is told it is required AND what
// the valid kinds are, in a single response -- and collapses the "one mistake,
// one round-trip" sequence a caller that sent none of the required fields used to
// pay for.
func missingRequiredError(schema mcp.ToolInputSchema, names []string) error {
	parts := make([]string, 0, len(names))
	for _, name := range names {
		part := fmt.Sprintf("%q", name)
		if a := aliasesFor(name); len(a) > 0 {
			part += fmt.Sprintf(" (aliases: %s)", strings.Join(a, ", "))
		}
		// An enum on a missing field is the caller's next hurdle; naming its
		// values here clears it before the retry. A malformed schema is this
		// package's own defect and coerceProp surfaces it on the present-value
		// path, so a lookup miss here just omits the hint rather than masking the
		// missing-required report.
		if prop, err := propSchema(schema, name); err == nil {
			if vals, ok, verr := enumValues(prop, name); verr == nil && ok {
				part += fmt.Sprintf(" (one of: %s)", strings.Join(vals, ", "))
			}
		}
		parts = append(parts, part)
	}
	if len(parts) == 1 {
		return fmt.Errorf("missing required parameter %s", parts[0])
	}
	return fmt.Errorf("missing required parameters: %s", strings.Join(parts, ", "))
}

// suggestParam returns the parameter the caller most likely meant, or "" when
// nothing is close enough. Three tiers, first hit wins: a case-only difference,
// then a small edit distance, then nothing.
//
// This is presentational only. The call is rejected either way -- the suggestion
// changes how the error reads, never what happens -- and per AGENTS.md nothing
// may branch on error text. It runs only on the rejection path, against at most
// a handful of names.
//
// known must be sorted, and only a strictly better match displaces the incumbent,
// so ties resolve to the lexicographically first candidate. Properties is a map:
// without both, the suggestion would vary between runs and flake every test that
// reads the message.
func suggestParam(unknown string, known []string) string {
	for _, k := range known {
		if strings.EqualFold(unknown, k) {
			return k
		}
	}
	best, bestDist := "", 0
	for _, k := range known {
		// Short names get a tighter bound: at distance 2, "id" is closer to
		// "kind" than to anything the caller meant.
		threshold := 2
		if min(len([]rune(unknown)), len([]rune(k))) <= 4 {
			threshold = 1
		}
		d := levenshtein(unknown, k, threshold)
		if d > threshold {
			continue
		}
		if best == "" || d < bestDist {
			best, bestDist = k, d
		}
	}
	return best
}

// levenshtein returns the edit distance between a and b, giving up once it
// exceeds max: the result is then only guaranteed to be greater than max, which
// is all a threshold test needs.
func levenshtein(a, b string, max int) int {
	ra, rb := []rune(a), []rune(b)
	if abs(len(ra)-len(rb)) > max {
		return max + 1
	}
	prev := make([]int, len(rb)+1)
	curr := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		curr[0] = i
		best := curr[0]
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			curr[j] = min(prev[j]+1, curr[j-1]+1, prev[j-1]+cost)
			best = min(best, curr[j])
		}
		if best > max {
			return max + 1
		}
		prev, curr = curr, prev
	}
	return prev[len(rb)]
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// enumOf turns a canonical set into a schema enum, so the schema a tool
// advertises and the set its handler validates against cannot drift apart. They
// already had: the gardener's hand-written list omitted abandon_plan, which the
// store has accepted the whole time.
func enumOf[T ~string](values []T) mcp.PropertyOption {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = string(v)
	}
	return mcp.Enum(out...)
}
