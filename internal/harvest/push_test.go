package harvest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// A body carries a push only under Field; anything else -- no field, a JSON
// null, a body that is not an object -- is no push, as tolerant as the hooks'
// own decode. A present push decodes whole or not at all.
func TestDecodePush(t *testing.T) {
	head := strings.Repeat("0a", 20)
	tokens := core.TokenUsage{Input: 1, Cached: 2, CacheCreation: 3, Output: 4, Total: 10}
	for _, tc := range []struct {
		name    string
		body    string
		want    Push
		present bool
		invalid bool
	}{
		{name: "no field", body: `{"session_id":"s"}`},
		{name: "null", body: `{"seamless_capture":null}`},
		{name: "not an object", body: `[1,2]`},
		{name: "not json", body: `nope`},
		{name: "empty body", body: ``},
		{name: "marker", body: `{"seamless_capture":{"version":1}}`, want: Push{Version: 1}, present: true},
		{name: "full", present: true,
			body: `{"session_id":"s","seamless_capture":{"version":1,"model":"claude-x","findings":"(auto-harvested) done",` +
				`"tokens":{"input":1,"cached":2,"cacheCreation":3,"output":4,"total":10},"git_head":"` + head + `",` +
				`"plan":{"basename":"example-plan","content":"# Plan"},"prompt":"p","report":"r","later":"ignored"}}`,
			want: Push{Version: 1, Model: "claude-x", Findings: "(auto-harvested) done", Tokens: &tokens, GitHead: head,
				Plan: &PlanFile{Basename: "example-plan", Content: "# Plan"}, Prompt: "p", Report: "r"}},
		{name: "no version", body: `{"seamless_capture":{"findings":"x"}}`, invalid: true},
		{name: "a later version", body: `{"seamless_capture":{"version":2}}`, invalid: true},
		{name: "wrong type", body: `{"seamless_capture":{"version":"1"}}`, invalid: true},
		{name: "not an object push", body: `{"seamless_capture":"x"}`, invalid: true},
		{name: "short head", body: `{"seamless_capture":{"version":1,"git_head":"abc1234"}}`, invalid: true},
		{name: "upper-case head", body: `{"seamless_capture":{"version":1,"git_head":"` + strings.ToUpper(head) + `"}}`, invalid: true},
		{name: "a ref, not a commit", body: `{"seamless_capture":{"version":1,"git_head":"refs/heads/main"}}`, invalid: true},
		{name: "unsafe basename", body: `{"seamless_capture":{"version":1,"plan":{"basename":"../x"}}}`, invalid: true},
		{name: "empty basename", body: `{"seamless_capture":{"version":1,"plan":{"basename":""}}}`, invalid: true},
		{name: "model with a newline", body: `{"seamless_capture":{"version":1,"model":"a\nb"}}`, invalid: true},
		{name: "synthetic model", body: `{"seamless_capture":{"version":1,"model":"<synthetic>"}}`, invalid: true},
		{name: "negative tokens", body: `{"seamless_capture":{"version":1,"tokens":{"input":-1,"total":-1}}}`, invalid: true},
		{name: "a total that is not the sum", body: `{"seamless_capture":{"version":1,"tokens":{"input":1,"total":9}}}`, invalid: true},
		{name: "findings past the cap",
			body: `{"seamless_capture":{"version":1,"findings":"` + strings.Repeat("x", MaxFindingsRunes+1) + `"}}`, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, present, err := DecodePush([]byte(tc.body))
			if tc.invalid {
				require.ErrorIs(t, err, ErrInvalidPush)
				require.False(t, present)
				require.Equal(t, Push{}, got, "never a partial push")
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.present, present)
			require.Equal(t, tc.want, got)
		})
	}
}

// What the parsers produce always validates: the client never builds a push
// the daemon would refuse from an ordinary transcript.
func TestPush_ParserOutputValidates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.jsonl")
	long := strings.Repeat("y", MaxRunes*2)
	line := `{"type":"assistant","message":{"id":"m","role":"assistant","model":"claude-x","content":[{"type":"text","text":"` +
		long + `"}],"usage":{"input_tokens":3,"output_tokens":4}}}`
	require.NoError(t, os.WriteFile(path, []byte(line+"\n"), 0o600))
	usage, ok := ClaudeTokens(path)
	require.True(t, ok)
	p := Push{Version: PushVersion, Findings: Findings(path), Model: TranscriptModel(path), Tokens: &usage}
	require.NoError(t, p.Validate())
	require.Len(t, []rune(p.Findings), MaxFindingsRunes)
	require.NoError(t, Push{Version: PushVersion, Findings: Fallback}.Validate())
	require.NoError(t, Push{Version: PushVersion, Findings: CodexStopFindings(long, "")}.Validate())
}

// The wire names are a frozen, additive contract between `seam hook` and the
// daemon: a rename here strands every client of the other release.
func TestPush_WireNames(t *testing.T) {
	raw, err := json.Marshal(Push{Version: 1, Model: "m", Findings: "f", Tokens: &core.TokenUsage{}, GitHead: "h",
		Plan: &PlanFile{Basename: "b", Content: "c"}, Prompt: "p", Report: "r"})
	require.NoError(t, err)
	var keys map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &keys))
	got := make([]string, 0, len(keys))
	for k := range keys {
		got = append(got, k)
	}
	require.ElementsMatch(t, []string{"version", "model", "findings", "tokens", "git_head", "plan", "prompt", "report"}, got)
	require.Equal(t, "seamless_capture", Field)
	require.Equal(t, "X-Seamless-Capture-Refused", RefusedHeader)
}

func TestPlanFilePath(t *testing.T) {
	plans := filepath.Join(string(filepath.Separator)+"home", "owner", ".claude", "plans")
	for _, tc := range []struct {
		raw  string
		want string
		ok   bool
	}{
		{filepath.Join(plans, "example-plan.md"), filepath.Join(plans, "example-plan.md"), true},
		{filepath.Join(plans, "sub", "example-plan.md"), "", false},
		{filepath.Join(plans, "example-plan.txt"), "", false},
		{filepath.Join(plans, "..", "example-plan.md"), "", false},
		{filepath.Join(plans, "bad:name.md"), "", false},
		{"", "", false},
	} {
		got, ok := PlanFilePath(plans, tc.raw)
		require.Equal(t, tc.ok, ok, tc.raw)
		require.Equal(t, tc.want, got, tc.raw)
	}
	_, ok := PlanFilePath("", filepath.Join(plans, "example-plan.md"))
	require.False(t, ok, "no plans dir: nothing is a plan file")
	require.Equal(t, "example-plan", PlanBasename(filepath.Join(plans, "example-plan.md")))
}
