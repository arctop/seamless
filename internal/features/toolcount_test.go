package features

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
)

// registeredForTest is a stand-in for the caller's registered count. It is
// deliberately not mcp.ToolCount: this package cannot import mcp, and the whole
// point of taking the number as a parameter is that the arithmetic holds for any
// value of it.
const registeredForTest = 33

// optionalTools is how many MCP tools all optional features own together -- the
// widest legitimate gap between the registered and exposed counts. Derived, so
// this file keeps saying something true when a second tool-owning feature lands.
func optionalTools() int { return len(ToolOwners()) }

// The common case, not the exception: optional features ship OFF, so a fresh
// install exposes fewer tools than are registered and doctor must still pass.
func TestToolCountVerdict_DefaultOffPassesAndNamesTheFeature(t *testing.T) {
	off := Defaults()

	ok, detail := ToolCountVerdict(registeredForTest, registeredForTest-optionalTools(), &off, "")
	require.True(t, ok, "a default daemon must pass doctor: %s", detail)
	require.Equal(t,
		fmt.Sprintf("%d registered, %d exposed (research, repo_locks disabled)",
			registeredForTest, registeredForTest-optionalTools()),
		detail)
}

// The other half: with every feature on, the full surface is expected and the
// line stays the single-number form.
func TestToolCountVerdict_EverythingOnExpectsTheFullSurface(t *testing.T) {
	on := allOn()

	ok, detail := ToolCountVerdict(registeredForTest, registeredForTest, &on, "")
	require.True(t, ok, detail)
	require.Equal(t, fmt.Sprintf("%d tools (expected %d)", registeredForTest, registeredForTest), detail)

	// And the reduced count is now WRONG, so the check has not been softened into
	// one that passes on anything.
	ok, detail = ToolCountVerdict(registeredForTest, registeredForTest-optionalTools(), &on, "")
	require.False(t, ok)
	require.Contains(t, detail, fmt.Sprintf("expected %d", registeredForTest))
}

// A daemon with the feature off that exposes its tools anyway is a real failure:
// the gate is not firing. The reduced expectation is an expectation, not a floor.
func TestToolCountVerdict_FeatureOffButToolsExposedFails(t *testing.T) {
	off := Defaults()

	ok, detail := ToolCountVerdict(registeredForTest, registeredForTest, &off, "")
	require.False(t, ok)
	require.Equal(t,
		fmt.Sprintf("%d registered, %d exposed, expected %d with research, repo_locks disabled",
			registeredForTest, registeredForTest, registeredForTest-optionalTools()),
		detail)
}

// Failure-soft: the feature state is a second dependency of what used to be a
// self-contained check, so losing it must not fail an otherwise healthy daemon.
// Without it there is no single expected number, only the range between
// "everything optional off" and "everything on" -- the verdict judges that range
// and says out loud that it could not read the state.
func TestToolCountVerdict_UnreadableFeatureStateFallsBackToTheRange(t *testing.T) {
	low := registeredForTest - optionalTools()

	for _, live := range []int{low, registeredForTest} {
		ok, detail := ToolCountVerdict(registeredForTest, live, nil, "console returned 500")
		require.True(t, ok, "%d is inside the range: %s", live, detail)
		require.Contains(t, detail, "feature state unreadable: console returned 500")
		require.Contains(t, detail, fmt.Sprintf("expected %d-%d", low, registeredForTest))
	}

	// Soft, not blind: a count outside the range cannot be explained by any
	// feature state, so it still fails.
	ok, detail := ToolCountVerdict(registeredForTest, low-1, nil, "why")
	require.False(t, ok, detail)
	ok, _ = ToolCountVerdict(registeredForTest, registeredForTest+1, nil, "why")
	require.False(t, ok)
}

// The gap is derived from the registry, never from a literal: the day a second
// tool-owning feature lands, the arithmetic and the reason line follow it.
func TestToolCountVerdict_DerivesTheGapFromTheRegistry(t *testing.T) {
	off := Defaults()
	hidden := HiddenTools(off)
	require.NotEmpty(t, hidden, "with every feature off the registry must hide something")
	require.Len(t, hidden, optionalTools())

	ok, detail := ToolCountVerdict(registeredForTest, registeredForTest-len(hidden), &off, "")
	require.True(t, ok, detail)
	for _, f := range Registry() {
		if len(f.Tools) > 0 {
			require.Contains(t, detail, string(f.Key), "a disabled feature must be named as the reason")
		}
	}

	on := allOn()
	ok, detail = ToolCountVerdict(registeredForTest, registeredForTest, &on, "")
	require.True(t, ok, detail)
	require.Empty(t, HiddenTools(on))
}

// A feature that owns no tools explains nothing about this number, so it must
// not appear in the reason even while it is off -- an operator told "momentum
// disabled" would go looking for tools momentum never had.
func TestToolCountVerdict_NamesOnlyToolOwningFeatures(t *testing.T) {
	off := Defaults()
	_, detail := ToolCountVerdict(registeredForTest, registeredForTest-optionalTools(), &off, "")
	for _, f := range Registry() {
		if len(f.Tools) == 0 {
			require.NotContains(t, detail, string(f.Key),
				"%s owns no tools and cannot account for the gap", f.Key)
		}
	}
}

// allOn returns a features config with every registered feature enabled.
func allOn() config.Features {
	c := Defaults()
	for _, f := range Registry() {
		f.Set(&c, true)
	}
	return c
}
