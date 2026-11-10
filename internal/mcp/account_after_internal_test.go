package mcp

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// accountAfter keeps a restricted account's label until every labeled field
// is the owner's; textOwned says which fields are.
func TestAccountAfter(t *testing.T) {
	for _, tc := range []struct {
		name             string
		current, writer  string
		changed, ownsAll bool
		want             string
	}{
		{"the account's change stamps it", "", "bob", true, false, "bob"},
		{"another account's change re-stamps it", "eve", "bob", true, false, "bob"},
		{"the account's metadata change stamps nothing", "", "bob", false, false, ""},
		{"the account's metadata change keeps another's", "eve", "bob", false, false, "eve"},
		{"the owner's partial change keeps it", "bob", "", true, false, "bob"},
		{"the owner's metadata change keeps it", "bob", "", false, false, "bob"},
		{"the owner owning all of it clears it", "bob", "", true, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, accountAfter(tc.current, tc.writer, tc.changed, tc.ownsAll))
		})
	}
}

func TestTextOwned(t *testing.T) {
	for _, tc := range []struct {
		name          string
		before, after string
		replaced      bool
		want          bool
	}{
		{"replaced with new text", "old", "new", true, true},
		{"replaced with the same text", "old", "old", true, false},
		{"only whitespace differs", "old\n", "  old", true, false},
		{"kept around an edit or append", "old", "old and more", false, false},
		{"empty after", "old", "", false, true},
		{"empty after a replace", "old", " \n", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, textOwned(tc.before, tc.after, tc.replaced))
		})
	}
}
