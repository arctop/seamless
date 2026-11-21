package mcp

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// The corpus deliberately contains a name that is a prefix of another
// (chroma-boot vs chroma-boot-race): the boundary rule must keep the shorter
// slug from false-matching inside the longer one.
func TestMishapMemoryIDs(t *testing.T) {
	corpus := []core.Memory{
		{ID: "M1", Name: "chroma-boot-race"},
		{ID: "M2", Name: "chroma-boot"},
		{ID: "M3", Name: "pkill-target-check"},
	}
	tests := []struct {
		name string
		text string
		want []string
	}{
		{"name mid-text", "violated pkill-target-check: pkill -f hit the daemon", []string{"M3"}},
		{"no name present", "wiped the wrong scratch directory", nil},
		{"multiple names in one text", "ignored chroma-boot-race and then pkill-target-check", []string{"M1", "M3"}},
		{"prefix slug does not match inside a longer slug", "raced chroma-boot-race again", []string{"M1"}},
		{"shorter slug still matches on its own", "raced chroma-boot on startup", []string{"M2"}},
		{"slug embedded in a longer token does not match", "ranchroma-boot-racer overflowed", nil},
		{"slug extended by a suffix does not match", "a pkill-target-checklist is not that memory", nil},
		{"punctuation delimits", "see `chroma-boot-race` (again)", []string{"M1"}},
		{"whole text is the slug", "chroma-boot-race", []string{"M1"}},
		{"match is case-sensitive (exact slug)", "violated Chroma-Boot-Race", nil},
		{"multibyte neighbors delimit", "échroma-boot-raceé", []string{"M1"}},
		{"empty text", "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, mishapMemoryIDs(tt.text, corpus))
		})
	}
}

// A memory row without a name or id (defensive: never produced by the store)
// must not match everything or emit empty ids.
func TestMishapMemoryIDs_SkipsBlankRows(t *testing.T) {
	corpus := []core.Memory{{ID: "M1", Name: ""}, {ID: "", Name: "chroma-boot"}}
	require.Nil(t, mishapMemoryIDs("chroma-boot text", corpus))
}

// mishapPayload adds "type" only for an entry that opens with one, keeps the
// description the entry as written, and scans the whole entry for slugs. The
// untyped payloads are exactly the shape every report had before types.
func TestMishapPayload(t *testing.T) {
	corpus := []core.Memory{{ID: "M1", Name: "zsh-equals-expansion"}}
	for _, tc := range []struct {
		name, entry string
		want        map[string]any
	}{
		{"typed", "shell-trap: a bare == died", map[string]any{"description": "shell-trap: a bare == died", "type": "shell-trap"}},
		{"typed and linked", "ignored-warning: violated zsh-equals-expansion", map[string]any{
			"description": "ignored-warning: violated zsh-equals-expansion", "type": "ignored-warning", "item_ids": []string{"M1"}}},
		{"a slug as the prefix links, and is no type", "zsh-equals-expansion: bit again", map[string]any{
			"description": "zsh-equals-expansion: bit again", "item_ids": []string{"M1"}}},
		{"old free text", "pkill matched the live daemon", map[string]any{"description": "pkill matched the live daemon"}},
		{"unknown word", "note: slow build", map[string]any{"description": "note: slow build"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, mishapPayload(tc.entry, 4000, corpus))
		})
	}
	// Truncation trims the description, never the type read from the whole entry.
	long := mishapPayload("live-state: "+strings.Repeat("x", 100), 20, nil)
	require.Equal(t, "live-state", long["type"])
	require.True(t, strings.HasPrefix(long["description"].(string), "live-state: xxxxxxxx\n"), long["description"])
}
