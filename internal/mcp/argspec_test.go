package mcp

import (
	"maps"
	"math"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store"
)

// schemaOf builds an input schema from tool options, so these tests exercise the
// same construction path the real tools use rather than a hand-built map that
// could drift from what mcp-go actually produces.
func schemaOf(opts ...mcp.ToolOption) mcp.ToolInputSchema {
	return mcp.NewTool("test", opts...).InputSchema
}

func TestKnownParams_SortedAndAliasAware(t *testing.T) {
	// A tool that declares "body" also accepts its aliases...
	withBody := schemaOf(
		mcp.WithString("title"),
		mcp.WithString("body"),
		mcp.WithString("id"),
	)
	require.Equal(t, []string{"body", "content", "id", "text", "title"}, knownParams(withBody))

	// ...but a tool with no body concept does not: "content" stays unknown.
	noBody := schemaOf(mcp.WithString("query"), mcp.WithString("scope"))
	require.Equal(t, []string{"query", "scope"}, knownParams(noBody))
}

func TestNormalizeArgs_UnknownParam(t *testing.T) {
	schema := schemaOf(
		mcp.WithString("id"),
		mcp.WithString("title"),
		mcp.WithString("depends_on"),
		mcp.WithString("project"),
	)

	for _, tc := range []struct {
		name string
		key  string
		want string
	}{
		{
			name: "typo gets a suggestion",
			key:  "despends_on",
			want: `unknown parameter "despends_on": did you mean "depends_on"? valid parameters are: depends_on, id, project, title`,
		},
		{
			name: "case-only difference gets a suggestion",
			key:  "Title",
			want: `unknown parameter "Title": did you mean "title"? valid parameters are: depends_on, id, project, title`,
		},
		{
			name: "nothing close enough omits the clause",
			key:  "wholly_unrelated",
			want: `unknown parameter "wholly_unrelated": valid parameters are: depends_on, id, project, title`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := normalizeArgs(schema, map[string]any{tc.key: "x"})
			require.EqualError(t, err, tc.want)
		})
	}
}

// TestNormalizeArgs_UnknownParamIsDeterministic pins the ordering guarantee:
// Properties is a map, so without sorted iteration the reported parameter (and
// the valid-values list) would vary between runs.
func TestNormalizeArgs_UnknownParamIsDeterministic(t *testing.T) {
	schema := schemaOf(mcp.WithString("id"), mcp.WithString("title"), mcp.WithString("project"))
	args := map[string]any{"aaa": "1", "zzz": "2", "mmm": "3"}

	for range 50 {
		_, err := normalizeArgs(schema, args)
		require.EqualError(t, err, `unknown parameter "aaa": valid parameters are: id, project, title`)
	}
}

// TestArgsAliasGroup is the pin named by the plan: the aliases are load-bearing
// (~11% of real agent tool failures were memory_append sent "body" instead of
// "content"), so deleting argBody must not delete the behavior.
func TestArgsAliasGroup(t *testing.T) {
	schema := schemaOf(
		mcp.WithString("name"),
		mcp.WithString("body", mcp.Required()),
	)

	for _, key := range []string{"body", "content", "text"} {
		got, err := normalizeArgs(schema, map[string]any{"name": "n", key: "  markdown  "})
		require.NoError(t, err)
		require.Equal(t, "  markdown  ", got["body"], "%s must collapse onto body, untrimmed", key)
		// The handler only ever reads the canonical name.
		require.Equal(t, map[string]any{"name": "n", "body": "  markdown  "}, got)
	}
}

func TestNormalizeArgs_AliasCollapse(t *testing.T) {
	schema := schemaOf(mcp.WithString("body", mcp.Required()))

	t.Run("agreeing aliases collapse", func(t *testing.T) {
		got, err := normalizeArgs(schema, map[string]any{"body": "same", "content": "same", "text": "same"})
		require.NoError(t, err)
		require.Equal(t, map[string]any{"body": "same"}, got)
	})

	t.Run("differing aliases are an error, never a silent winner", func(t *testing.T) {
		_, err := normalizeArgs(schema, map[string]any{"body": "one", "content": "two"})
		require.EqualError(t, err,
			"conflicting values for body: body and content differ; pass exactly one of body, content, text")
	})

	// argBody took the first non-blank, so it silently preferred content here.
	t.Run("a blank body no longer loses to a non-blank alias", func(t *testing.T) {
		_, err := normalizeArgs(schema, map[string]any{"body": "", "content": "two"})
		require.EqualError(t, err,
			"conflicting values for body: body and content differ; pass exactly one of body, content, text")
	})
}

// TestNormalizeArgs_RequiredIsAliasAware pins the trap that would break two
// green tests on day one: memory_write declares body as Required, and
// scope_alias_test calls it with only "content".
func TestNormalizeArgs_RequiredIsAliasAware(t *testing.T) {
	schema := schemaOf(
		mcp.WithString("name", mcp.Required()),
		mcp.WithString("body", mcp.Required()),
	)

	got, err := normalizeArgs(schema, map[string]any{"name": "n", "content": "via the alias"})
	require.NoError(t, err)
	require.Equal(t, "via the alias", got["body"])

	_, err = normalizeArgs(schema, map[string]any{"name": "n"})
	require.EqualError(t, err, `missing required parameter "body" (aliases: content, text)`)

	_, err = normalizeArgs(schema, map[string]any{"body": "b"})
	require.EqualError(t, err, `missing required parameter "name"`)
}

// TestNormalizeArgs_EmptyStringRule covers the carve-out and its limit. seam's
// own CLI sends depends_on:"" plan:"" project:"" body:"" unconditionally
// (cmd/seam/task.go:88), so those must read as absent -- while body:"" must
// still blank the body.
func TestNormalizeArgs_EmptyStringRule(t *testing.T) {
	schema := schemaOf(
		mcp.WithString("body"),
		mcp.WithString("plan"),
		mcp.WithString("project"),
		mcp.WithString("status", mcp.Enum("open", "done")),
		mcp.WithArray("depends_on", mcp.WithStringItems()),
		mcp.WithNumber("limit"),
		mcp.WithObject("metrics"),
	)

	got, err := normalizeArgs(schema, map[string]any{
		"body": "", "plan": "", "project": "", "status": "",
		"depends_on": "", "limit": "", "metrics": "",
	})
	require.NoError(t, err)

	// Interpreted parameters read as absent.
	require.NotContains(t, got, "status")
	require.NotContains(t, got, "depends_on")
	require.NotContains(t, got, "limit")
	require.NotContains(t, got, "metrics")

	// Free-form strings keep "" -- it is a real value, and scope resolution is
	// resolveWriteScope's job, not this validator's.
	require.Equal(t, "", got["body"])
	require.Equal(t, "", got["plan"])
	require.Equal(t, "", got["project"])
}

// TestNormalizeArgs_RequiredRunsAfterTheEmptyStringRule pins the pipeline order:
// a required enum sent as "" reports itself missing, not invalid.
func TestNormalizeArgs_RequiredRunsAfterTheEmptyStringRule(t *testing.T) {
	schema := schemaOf(mcp.WithString("kind", mcp.Required(), enumOf(core.MemoryKinds)))

	_, err := normalizeArgs(schema, map[string]any{"kind": ""})
	require.EqualError(t, err, `missing required parameter "kind" (one of: constraint, convention, runbook, protocol, gotcha, decision, refuted, reference, stage)`)
}

// TestNormalizeArgs_RequiredBatchesAndEnriches pins the two upgrades to the
// missing-required report: every absent required field is named in one message
// (not one rejection per round-trip), and a missing enum field carries its
// allowed values so the caller clears that hurdle on the same retry. This is the
// exact opening call from the field report -- name and kind both absent.
func TestNormalizeArgs_RequiredBatchesAndEnriches(t *testing.T) {
	schema := schemaOf(
		mcp.WithString("name", mcp.Required()),
		mcp.WithString("kind", mcp.Required(), enumOf(core.MemoryKinds)),
		mcp.WithString("body", mcp.Required()),
	)

	_, err := normalizeArgs(schema, map[string]any{"body": "b"})
	require.EqualError(t, err,
		`missing required parameters: "name", "kind" (one of: constraint, convention, runbook, protocol, gotcha, decision, refuted, reference, stage)`)
}

// TestArgsCoerceLegacyForms is the plan's headline pin: the array and CSV forms
// of a list parameter produce identical results, which is what proves the
// original bug (argString on a []string yielding "", silently dropping the deps)
// cannot come back in either direction.
func TestArgsCoerceLegacyForms(t *testing.T) {
	schema := schemaOf(
		mcp.WithArray("depends_on", mcp.WithStringItems()),
		mcp.WithNumber("lease_seconds"),
	)

	t.Run("array and CSV are identical", func(t *testing.T) {
		for _, tc := range []struct {
			name       string
			array, csv any
			want       []string
		}{
			{"single value", []any{"01ABC"}, "01ABC", []string{"01ABC"}},
			{"several values", []any{"a", "b", "c"}, "a,b,c", []string{"a", "b", "c"}},
			{"whitespace and blanks are dropped", []any{" a ", "", "b"}, " a , , b", []string{"a", "b"}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				fromArray, err := normalizeArgs(schema, map[string]any{"depends_on": tc.array})
				require.NoError(t, err)
				fromCSV, err := normalizeArgs(schema, map[string]any{"depends_on": tc.csv})
				require.NoError(t, err)

				require.Equal(t, tc.want, fromArray["depends_on"])
				require.Equal(t, fromArray, fromCSV, "the array and CSV forms must normalize identically")
			})
		}
	})

	// The numeric-string form seam's CLI sends still works, and still reaches the
	// handler's own overflow guard rather than being rejected here with different
	// wording (validation_test.go requires the text "invalid lease_seconds").
	t.Run("numeric strings coerce", func(t *testing.T) {
		got, err := normalizeArgs(schema, map[string]any{"lease_seconds": "60"})
		require.NoError(t, err)
		require.Equal(t, float64(60), got["lease_seconds"])

		got, err = normalizeArgs(schema, map[string]any{"lease_seconds": "99999999999999"})
		require.NoError(t, err, "9.99e13 is below the safe-integer bound: the handler owns this rejection")
		require.Equal(t, float64(99999999999999), got["lease_seconds"])
	})

	t.Run("an empty list drops the key", func(t *testing.T) {
		for _, empty := range []any{[]any{}, "", ",  ,"} {
			got, err := normalizeArgs(schema, map[string]any{"depends_on": empty})
			require.NoError(t, err)
			require.NotContains(t, got, "depends_on", "%#v must read as absent, not as an empty list", empty)
		}
	})
}

func TestCoerceString(t *testing.T) {
	s, err := coerceString("a title", "title")
	require.NoError(t, err)
	require.Equal(t, "a title", s)

	// The deliberate non-coercion: 42 -> "42" would invent a title.
	_, err = coerceString(float64(42), "title")
	require.EqualError(t, err, "invalid title: expected a string, got number")

	_, err = coerceString([]any{"a"}, "title")
	require.EqualError(t, err, "invalid title: expected a string, got array")
}

func TestCoerceNumber(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   any
		want float64
	}{
		{"json number", float64(10), 10},
		{"numeric string", "10", 10},
		{"padded numeric string", " 10 ", 10},
		{"negative", float64(-3), -3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, err := coerceNumber(tc.in, "limit")
			require.NoError(t, err)
			require.Equal(t, tc.want, f)
		})
	}

	for _, tc := range []struct {
		name string
		in   any
		want string
	}{
		{"non-numeric string", "abc", `invalid limit: expected a number, got "abc"`},
		{"boolean", true, "invalid limit: expected a number, got boolean"},
		{"array", []any{1}, "invalid limit: expected a number, got array"},
		// ParseFloat accepts these, which is why checkFinite runs on the string
		// path at all -- JSON itself cannot carry them.
		{"NaN via string", "NaN", "invalid limit: expected a finite number"},
		{"Inf via string", "Inf", "invalid limit: expected a finite number"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := coerceNumber(tc.in, "limit")
			require.EqualError(t, err, tc.want)
		})
	}

	t.Run("beyond the safe-integer bound", func(t *testing.T) {
		_, err := coerceNumber(math.Pow(2, 53)+2, "limit")
		require.ErrorContains(t, err, "too large to be represented exactly")

		// The bound itself is still fine.
		f, err := coerceNumber(math.Pow(2, 53), "limit")
		require.NoError(t, err)
		require.Equal(t, math.Pow(2, 53), f)
	})
}

func TestCoerceInteger(t *testing.T) {
	for _, in := range []any{float64(10), float64(10.0), "10", "10.0"} {
		got, err := coerceInteger(in, "limit")
		require.NoError(t, err)
		require.Equal(t, float64(10), got)
	}
	for _, in := range []any{float64(1.9), "1.9", float64(-2.5)} {
		_, err := coerceInteger(in, "limit")
		require.ErrorContains(t, err, "expected an integer")
	}
}

func TestCoerceStrings(t *testing.T) {
	_, err := coerceStrings(float64(1), "depends_on")
	require.EqualError(t, err,
		"invalid depends_on: expected an array of strings or a comma-separated string, got number")

	_, err = coerceStrings([]any{"a", float64(2)}, "depends_on")
	require.EqualError(t, err, "invalid depends_on[1]: expected a string, got number")
}

func TestCoerceObject(t *testing.T) {
	want := map[string]any{"latency_ms": float64(12)}

	m, err := coerceObject(want, "metrics")
	require.NoError(t, err)
	require.Equal(t, want, m)

	m, err = coerceObject(`{"latency_ms": 12}`, "metrics")
	require.NoError(t, err)
	require.Equal(t, want, m, "a stringified object is the legacy form and must match the native one")

	for _, in := range []any{`[1,2]`, `null`, `not json`, float64(1)} {
		_, err := coerceObject(in, "metrics")
		require.Error(t, err, "%#v must not pass as an object", in)
	}
}

func TestCoerceBool(t *testing.T) {
	for _, tc := range []struct {
		in   any
		want bool
	}{
		{true, true}, {false, false}, {"true", true}, {"false", false},
	} {
		b, err := coerceBool(tc.in, "force")
		require.NoError(t, err)
		require.Equal(t, tc.want, b)
	}

	// Deliberately outside the empty-string carve-out: no client sends a boolean
	// as "", so it stays uninterpretable rather than defaulting to false.
	_, err := coerceBool("", "force")
	require.EqualError(t, err, `invalid force: expected true or false, got ""`)

	_, err = coerceBool("yes", "force")
	require.EqualError(t, err, `invalid force: expected true or false, got "yes"`)
}

func TestNormalizeArgs_Enum(t *testing.T) {
	schema := schemaOf(mcp.WithString("status", enumOf(core.TaskStatuses)))

	got, err := normalizeArgs(schema, map[string]any{"status": "in_progress"})
	require.NoError(t, err)
	require.Equal(t, "in_progress", got["status"])

	// The valid-values list renders in declared (lifecycle) order, not sorted.
	_, err = normalizeArgs(schema, map[string]any{"status": "in-progress"})
	require.EqualError(t, err, `invalid status "in-progress": valid values are open, in_progress, done, dropped`)
}

func TestNormalizeArgs_Range(t *testing.T) {
	schema := schemaOf(mcp.WithNumber("limit", mcp.Min(1), mcp.Max(100)))

	got, err := normalizeArgs(schema, map[string]any{"limit": float64(10)})
	require.NoError(t, err)
	require.Equal(t, float64(10), got["limit"])

	_, err = normalizeArgs(schema, map[string]any{"limit": float64(0)})
	require.EqualError(t, err, "invalid limit: must be >= 1")

	_, err = normalizeArgs(schema, map[string]any{"limit": float64(101)})
	require.EqualError(t, err, "invalid limit: must be <= 100")

	// The bound renders as an agent would write it, not as 1.000000.
	_, err = normalizeArgs(schema, map[string]any{"limit": "-5"})
	require.EqualError(t, err, "invalid limit: must be >= 1")
}

func TestNormalizeArgs_IntegerRejectsFractions(t *testing.T) {
	schema := schemaOf(mcp.WithInteger("limit", mcp.Min(1), mcp.Max(100)))

	got, err := normalizeArgs(schema, map[string]any{"limit": "10.0"})
	require.NoError(t, err)
	require.Equal(t, float64(10), got["limit"])

	for _, value := range []any{float64(1.9), "1.9"} {
		_, err := normalizeArgs(schema, map[string]any{"limit": value})
		require.EqualError(t, err, "invalid limit: expected an integer, got 1.9")
	}
}

// TestNormalizeArgs_RangeBoundStorageTypes pins the range check against every
// type a declared bound can arrive as. mcp.Min/mcp.Max are generic over
// int|int64|float64 and store exactly what the call site wrote, so mcp.Min(1)
// stores an int while a JSON-decoded schema yields float64. Reading only one of
// those is not a loud failure: an unreadable bound reports "no bound declared"
// and the range check is skipped entirely, which is how a bump to mcp-go v0.56
// silently dropped the limit floor on recall and trial_query.
func TestNormalizeArgs_RangeBoundStorageTypes(t *testing.T) {
	for _, tc := range []struct {
		name string
		min  any
	}{
		{"int", 1},
		{"int64", int64(1)},
		{"float64", float64(1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The bound is set directly rather than through mcp.Min, so the
			// stored type is the subject of the test instead of whatever the
			// current mcp-go infers from an untyped constant.
			schema := schemaOf(mcp.WithNumber("limit",
				func(s map[string]any) { s["minimum"] = tc.min }))

			_, err := normalizeArgs(schema, map[string]any{"limit": float64(0)})
			require.EqualError(t, err, "invalid limit: must be >= 1")

			got, err := normalizeArgs(schema, map[string]any{"limit": float64(10)})
			require.NoError(t, err)
			require.Equal(t, float64(10), got["limit"])
		})
	}
}

// TestNormalizeArgs_NoOpinionAboutProject pins the boundary between this
// validator and scope resolution: project has no enum, is never required, and ""
// is not rejected here. resolveWriteScope already refuses to guess, and a second
// guard would duplicate or contradict it.
func TestNormalizeArgs_NoOpinionAboutProject(t *testing.T) {
	schema := schemaOf(mcp.WithString("name", mcp.Required()), mcp.WithString("project"))

	for _, project := range []string{"", "seamless", "global", "_global", "../traversal"} {
		got, err := normalizeArgs(schema, map[string]any{"name": "n", "project": project})
		require.NoError(t, err, "project %q is resolveWriteScope's business, not the validator's", project)
		require.Equal(t, project, got["project"])
	}
}

func TestNormalizeArgs_TypelessAndEmpty(t *testing.T) {
	// nil arguments are legitimate (a tool with no required params).
	got, err := normalizeArgs(schemaOf(mcp.WithString("kind")), map[string]any{})
	require.NoError(t, err)
	require.Empty(t, got)

	// A typeless property (mcp.WithAny) accepts any JSON by construction.
	anySchema := schemaOf(mcp.WithAny("payload"))
	got, err = normalizeArgs(anySchema, map[string]any{"payload": []any{float64(1)}})
	require.NoError(t, err)
	require.Equal(t, []any{float64(1)}, got["payload"])
}

// TestNormalizeArgs_ReturnsAFreshMap pins replace-don't-mutate: the middleware
// hands the normalized copy downstream while the interactions feed keeps the raw
// args as evidence of what the agent actually sent.
func TestNormalizeArgs_ReturnsAFreshMap(t *testing.T) {
	schema := schemaOf(mcp.WithArray("depends_on", mcp.WithStringItems()))
	raw := map[string]any{"depends_on": "a,b"}

	got, err := normalizeArgs(schema, raw)
	require.NoError(t, err)
	require.Equal(t, []string{"a", "b"}, got["depends_on"])
	require.Equal(t, "a,b", raw["depends_on"], "raw must survive normalization unchanged")
}

func TestSuggestParam(t *testing.T) {
	known := []string{"add_depends_on", "body", "depends_on", "id", "project", "status", "title"}

	for _, tc := range []struct {
		in, want string
	}{
		{"despends_on", "depends_on"},
		{"Title", "title"},
		{"TITLE", "title"},
		{"titel", "title"},
		{"statuss", "status"},
		{"bodyy", "body"},
		{"wholly_unrelated", ""},
		// Short names get a tighter bound: "id" is 2 edits from "body" but the
		// caller plainly did not mean it.
		{"xy", ""},
	} {
		require.Equal(t, tc.want, suggestParam(tc.in, known), "suggestParam(%q)", tc.in)
	}
}

// TestSuggestParam_TiesGoToTheFirstCandidate pins the tie-break: only a strictly
// better match displaces the incumbent, so an equidistant pair resolves to the
// first candidate rather than the last.
//
// That is a guarantee about the list, not about the alphabet -- suggestParam
// requires sorted input, and knownParams is what supplies it. The pair below is
// deliberately equidistant: "kine" is one edit from both.
func TestSuggestParam_TiesGoToTheFirstCandidate(t *testing.T) {
	require.Equal(t, "kind", suggestParam("kine", []string{"kind", "mine"}))

	// The two halves of the guarantee, together: knownParams sorts, so the
	// suggestion normalizeArgs reports is the lexicographically first best match
	// no matter what order Properties happened to yield.
	schema := schemaOf(mcp.WithString("mine"), mcp.WithString("kind"))
	for range 50 {
		_, err := normalizeArgs(schema, map[string]any{"kine": "x"})
		require.EqualError(t, err,
			`unknown parameter "kine": did you mean "kind"? valid parameters are: kind, mine`)
	}
}

func TestLevenshtein(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		max  int
		want int
	}{
		{"", "", 2, 0},
		{"abc", "abc", 2, 0},
		{"abc", "abd", 2, 1},
		{"kitten", "sitting", 3, 3},
		{"body", "", 2, 3},
		// Over the bound: only "greater than max" is guaranteed.
		{"depends_on", "id", 2, 3},
		{"abcdef", "uvwxyz", 2, 3},
	} {
		got := levenshtein(tc.a, tc.b, tc.max)
		if tc.want > tc.max {
			require.Greater(t, got, tc.max, "levenshtein(%q, %q, %d)", tc.a, tc.b, tc.max)
			continue
		}
		require.Equal(t, tc.want, got, "levenshtein(%q, %q, %d)", tc.a, tc.b, tc.max)
	}

	// Multi-byte input is measured in runes, not bytes.
	require.Equal(t, 1, levenshtein("naïve", "naive", 2))
}

func TestJSONTypeName(t *testing.T) {
	for in, want := range map[any]string{
		nil:        "null",
		true:       "boolean",
		float64(1): "number",
		"s":        "string",
	} {
		require.Equal(t, want, jsonTypeName(in))
	}
	require.Equal(t, "array", jsonTypeName([]any{}))
	require.Equal(t, "object", jsonTypeName(map[string]any{}))
}

// TestEnumOf_DerivesFromCanonicalSets is the drift pin. The gardener's schema
// hand-listed six proposal kinds while the store has always accepted seven, so
// gardener_proposals{kind:"abandon_plan"} worked but was undocumented -- and
// validating against the transcribed list would have broken it. Deriving from the
// canonical set means a new kind cannot reach the store without reaching the
// schema.
func TestEnumOf_DerivesFromCanonicalSets(t *testing.T) {
	for _, tc := range []struct {
		name string
		opt  mcp.PropertyOption
		want []string
	}{
		{"memory kinds", enumOf(core.MemoryKinds), []string{
			"constraint", "convention", "runbook", "protocol", "gotcha", "decision", "refuted", "reference", "stage",
		}},
		{"task statuses", enumOf(core.TaskStatuses), []string{"open", "in_progress", "done", "dropped"}},
		{"proposal kinds", enumOf(store.ProposalKinds), store.ProposalKinds},
		{"session sources", enumOf(core.SessionSources), core.SessionSources},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prop := map[string]any{}
			tc.opt(prop)
			require.Equal(t, tc.want, prop["enum"], "the schema enum must be the canonical set, in order")
		})
	}

	require.Contains(t, store.ProposalKinds, "abandon_plan", "the drift this derivation exists to fix")
}

// recall's kind takes a memory kind, and recallKindScopes reads an item kind or
// a scope sent there as the scope it selects. That reading is a guess unless the
// vocabularies are disjoint, and it is a dead end unless every key lands on a
// real scope: a new memory kind named like an item kind, or a new item kind
// without an entry, fails here first.
func TestRecallKindScopes_DisjointFromMemoryKinds(t *testing.T) {
	for k, scope := range recallKindScopes {
		require.False(t, core.MemoryKind(k).Valid(), "%q is a memory kind: kind=%s must keep filtering memories", k, k)
		require.Contains(t, retrieve.RecallScopes, scope, "%q maps to %q, which is not a recall scope", k, scope)
	}
	for _, item := range append(append([]string{}, core.KnowledgeItemKinds...), core.WorkItemKinds...) {
		require.Contains(t, recallKindScopes, item, "recall hits carry kind %q, so kind=%s must read as its scope", item, item)
	}
	for _, scope := range retrieve.RecallScopes {
		if scope == "all" {
			continue
		}
		require.Equal(t, scope, recallKindScopes[scope], "scope word %q sent as kind must read as itself", scope)
	}
}

func TestRecallKindAsScope(t *testing.T) {
	for _, tc := range []struct {
		name    string
		raw     map[string]any
		want    map[string]any
		wantErr string
	}{
		{"an item kind becomes its scope",
			map[string]any{"query": "q", "kind": "note"},
			map[string]any{"query": "q", "scope": "notes"}, ""},
		{"a session hit's kind searches sessions",
			map[string]any{"query": "q", "kind": "session", "limit": float64(3)},
			map[string]any{"query": "q", "scope": "sessions", "limit": float64(3)}, ""},
		{"a scope word sent as kind",
			map[string]any{"query": "q", "kind": "tasks"},
			map[string]any{"query": "q", "scope": "tasks"}, ""},
		{"scope all agrees: the kind narrows it",
			map[string]any{"query": "q", "kind": "task", "scope": "all"},
			map[string]any{"query": "q", "scope": "tasks"}, ""},
		{"the same scope agrees",
			map[string]any{"query": "q", "kind": "trial", "scope": "trials"},
			map[string]any{"query": "q", "scope": "trials"}, ""},
		{"an empty scope is absent",
			map[string]any{"query": "q", "kind": "memory", "scope": ""},
			map[string]any{"query": "q", "scope": "memories"}, ""},
		{"a memory kind is left alone",
			map[string]any{"query": "q", "kind": "convention", "scope": "notes"},
			map[string]any{"query": "q", "kind": "convention", "scope": "notes"}, ""},
		{"an unknown kind is left for the enum to refuse",
			map[string]any{"query": "q", "kind": "notez"},
			map[string]any{"query": "q", "kind": "notez"}, ""},
		{"a non-string kind is left for coercion to refuse",
			map[string]any{"query": "q", "kind": float64(1)},
			map[string]any{"query": "q", "kind": float64(1)}, ""},
		{"a contradicting scope is refused, not picked",
			map[string]any{"query": "q", "kind": "note", "scope": "tasks"},
			nil, `conflicting values for scope: kind "note" selects scope notes, but scope is tasks; pass scope alone`},
		{"no query has no browse to read it as",
			map[string]any{"kind": "task"},
			nil, `invalid kind "task": kind with no query lists one memory kind (constraint, convention, runbook, protocol, gotcha, decision, refuted, reference, stage); to search tasks, pass scope=tasks with a query`},
		{"an empty query is no query",
			map[string]any{"query": "", "kind": "note"},
			nil, "to search notes, pass scope=notes with a query"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := maps.Clone(tc.raw)
			got, err := rewriteArgs("recall", tc.raw)
			require.Equal(t, before, tc.raw, "the raw arguments are the interactions feed's evidence and must not be mutated")
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}

	// Only recall is rewritten: kind=note on another tool is that tool's to refuse.
	raw := map[string]any{"kind": "note"}
	got, err := rewriteArgs("memory_write", raw)
	require.NoError(t, err)
	require.Equal(t, raw, got)
}

func TestTasksListQueryAsID(t *testing.T) {
	const id = "01M7X4PRQSC29J9C58S6Q476HX"
	for _, tc := range []struct {
		name    string
		raw     map[string]any
		want    map[string]any
		wantErr string
	}{
		{"a ULID query is the id it names",
			map[string]any{"query": id},
			map[string]any{"id": id}, ""},
		{"surrounding space is not part of an id",
			map[string]any{"query": " " + id + "\n", "plan": "p"},
			map[string]any{"id": id, "plan": "p"}, ""},
		{"the same id agrees",
			map[string]any{"query": id, "id": id},
			map[string]any{"id": id}, ""},
		{"an empty id is absent",
			map[string]any{"query": id, "id": ""},
			map[string]any{"id": id}, ""},
		{"text is left for the unknown-parameter refusal",
			map[string]any{"query": "fix the recall kind error", "plan": "p"},
			map[string]any{"query": "fix the recall kind error", "plan": "p"}, ""},
		{"26 characters that are no ULID are text",
			map[string]any{"query": "abcdefghijklmnopqrstuvwxyz"},
			map[string]any{"query": "abcdefghijklmnopqrstuvwxyz"}, ""},
		{"a non-string query is left for coercion",
			map[string]any{"query": float64(1)},
			map[string]any{"query": float64(1)}, ""},
		{"a different id is refused, not picked",
			map[string]any{"query": id, "id": "01M7X4PQ4R1ZS9TVRZME049AG9"},
			nil, `conflicting values for id: query "` + id + `" is a task id, but id is 01M7X4PQ4R1ZS9TVRZME049AG9; pass id alone`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := maps.Clone(tc.raw)
			got, err := rewriteArgs("tasks_list", tc.raw)
			require.Equal(t, before, tc.raw, "the raw arguments are the interactions feed's evidence and must not be mutated")
			if tc.wantErr != "" {
				require.EqualError(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

// The query redirect names recall's narrow scopes from the canonical set, so a
// caller searching one tool's items is told how to search only those.
func TestParamRedirects_QueryNamesRecallScopes(t *testing.T) {
	err := unknownParamError("query", []string{"id", "plan"})
	require.EqualError(t, err, `unknown parameter "query": valid parameters are: id, plan; to search by text, use the recall tool `+
		`(recall query=<text>; add scope=<memories|notes|tasks|trials|sessions> to search only that item type)`)
}

// Past the listing's ceiling a bigger limit cannot help, so the note names a
// query instead of a limit the schema would refuse.
func TestBrowseMoreNote_NamesALimitOnlyWhileOneFits(t *testing.T) {
	require.Equal(t, "5 more convention memories match past limit=10; pass limit=15 to list all of them",
		browseMoreNote("convention", 10, 5))
	note := browseMoreNote("convention", 100, 7)
	require.Contains(t, note, "a listing stops at 100; add a query to search within kind=convention")
	require.NotContains(t, note, "pass limit=")
	require.Contains(t, browseMoreNote("convention", 60, 41), "a listing stops at 100", "60+41 is past the ceiling")
}
