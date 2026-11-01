package core

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMemoryDigest_FollowsWhatTheMemorySays(t *testing.T) {
	base := Memory{Kind: KindConstraint, Project: "app", Name: "no-force-push",
		Description: "never force-push main", Body: "Rebase locally instead.\n"}
	d := MemoryDigest(base)
	require.Len(t, d, 64)
	require.Equal(t, d, MemoryDigest(base), "deterministic")

	same := base
	same.Favorite, same.Tags, same.ID, same.FilePath = true, []string{"x"}, "01X", "memory/app/no-force-push.md"
	require.Equal(t, d, MemoryDigest(same), "a star, a tag or a path tells an agent nothing new")

	for name, mut := range map[string]func(*Memory){
		"kind":        func(m *Memory) { m.Kind = KindGotcha },
		"project":     func(m *Memory) { m.Project = "" },
		"name":        func(m *Memory) { m.Name = "no-force-push-2" },
		"description": func(m *Memory) { m.Description += "!" },
		"body":        func(m *Memory) { m.Body += "Unless asked.\n" },
	} {
		m := base
		mut(&m)
		require.NotEqual(t, d, MemoryDigest(m), "changing the %s lapses a confirmation", name)
	}

	// Text cannot be shifted across a field boundary into the same digest.
	a := Memory{Description: "a|b", Body: "c"}
	b := Memory{Description: "a", Body: "b|c"}
	require.NotEqual(t, MemoryDigest(a), MemoryDigest(b))
}
