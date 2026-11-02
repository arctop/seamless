package validate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

func TestPath(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{"simple", "notes/foo.md", false},
		{"nested", "memory/seam/chroma-boot-race.md", false},
		{"dot-prefix", "./foo.md", false},
		{"empty", "", true},
		{"absolute", "/etc/passwd", true},
		{"traversal", "../secret", true},
		{"traversal-middle", "notes/../../etc/passwd", true},
		{"null-byte", "foo\x00bar", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Path(tt.path)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestPathWithinDir(t *testing.T) {
	base := "/data/notes"
	tests := []struct {
		name    string
		rel     string
		wantErr bool
	}{
		{"within", "inbox/foo.md", false},
		{"root", ".", false},
		{"escape", "../etc/passwd", true},
		{"absolute", "/etc/passwd", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := PathWithinDir(tt.rel, base)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestTitle(t *testing.T) {
	tests := []struct {
		name    string
		title   string
		wantErr bool
	}{
		{"simple", "My Note", false},
		{"slash", "TCP/IP notes", false},
		{"ab-testing", "A/B testing", false},
		// Regression: date-range titles with ".." must be accepted (v1 rejected
		// these, causing a 37% notes_create error rate).
		{"date-range-dotdot", "summary 2026-07-05..08", false},
		{"leading-dotdot", "..hidden thoughts", false},
		{"empty", "", true},
		{"null-byte", "bad\x00title", true},
		{"backslash", "path\\to\\thing", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Title(tt.title)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestTitleLength(t *testing.T) {
	long := make([]byte, 256)
	for i := range long {
		long[i] = 'a'
	}
	require.Error(t, Title(string(long)))
	require.NoError(t, Title(string(long[:255])))
}

func TestName(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"simple", "chroma-boot-race", false},
		{"underscored", "f5_ssh_signing", false},
		// The ".." fix applies to Title only: Name still rejects it, since Name
		// values feed filenames directly.
		{"date-range-dotdot", "summary 2026-07-05..08", true},
		{"slash", "a/b", true},
		{"backslash", "a\\b", true},
		{"empty", "", true},
		{"null-byte", "bad\x00name", true},

		// Audit I11: a leading dot hides the file from the owner reviewing
		// their own corpus, and "." alone yields the alarming-looking "..md".
		{"leading-dot", ".hidden", true},
		{"bare-dot", ".", true},
		{"interior-dot-is-fine", "v1.2-notes", false},
		{"canonical-slug", "team-alpha_2026", false},
		{"unicode-is-portable", "café-東京", false},
		{"forbidden-less-than", "team<alpha", true},
		{"forbidden-greater-than", "team>alpha", true},
		{"forbidden-colon", "team:alpha", true},
		{"forbidden-quote", "team\"alpha", true},
		{"forbidden-pipe", "team|alpha", true},
		{"forbidden-question", "team?alpha", true},
		{"forbidden-star", "team*alpha", true},
		{"control-newline", "team\nalpha", true},
		{"control-unit-separator", "team\x1falpha", true},
		{"control-delete", "team\x7falpha", true},
		{"trailing-dot", "team-alpha.", true},
		{"trailing-space", "team-alpha ", true},

		// Audit I11: Windows resolves these as devices in every directory, so
		// the file cannot exist there. Rejected everywhere, because the corpus
		// is markdown that gets synced and cloned across machines.
		{"reserved-con", "con", true},
		{"reserved-uppercase", "CON", true},
		{"reserved-with-extension", "nul.md", true},
		{"reserved-com1", "COM1", true},
		{"reserved-lpt9", "lpt9", true},
		{"reserved-prefix-only-is-fine", "console-layout", false},
		{"reserved-substring-is-fine", "auxiliary", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Name(tt.input)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestProject(t *testing.T) {
	for _, global := range []string{"global", "_global"} {
		got, err := Project(global)
		require.NoError(t, err)
		require.Empty(t, got)
	}
	got, err := Project("team-alpha")
	require.NoError(t, err)
	require.Equal(t, "team-alpha", got)

	for _, bad := range []string{"all", "../notes", "a/b", "team:alpha", "team-alpha."} {
		_, err := Project(bad)
		require.Error(t, err)
	}
}

func TestIsolation(t *testing.T) {
	for _, state := range core.Isolations {
		got, err := Isolation(string(state))
		require.NoError(t, err)
		require.Equal(t, state, got)
	}
	got, err := Isolation("  sealed  ")
	require.NoError(t, err)
	require.Equal(t, core.IsolationSealed, got)

	// Present-but-uninterpretable is an error, never a default -- and the
	// message names the accepted values.
	for _, bad := range []string{"", "Open", "hidden", "confidental"} {
		_, err := Isolation(bad)
		require.Error(t, err)
		require.Contains(t, err.Error(), "valid values are open, confidential, sealed")
	}
}

func TestCommitIDs(t *testing.T) {
	sha1 := "0123456789abcdef0123456789abcdef01234567"
	sha256 := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	other := "fedcba9876543210fedcba9876543210fedcba98"

	got, err := CommitIDs([]string{" " + sha1 + " ", sha256, "", "0123456789ABCDEF0123456789ABCDEF01234567", other}, 10)
	require.NoError(t, err)
	require.Equal(t, []string{sha1, sha256, other}, got,
		"trimmed, lowercased, blanks dropped, a repeat keeps its first place")

	got, err = CommitIDs(nil, 10)
	require.NoError(t, err)
	require.Nil(t, got)

	for _, bad := range []string{
		"0123456",                   // an abbreviation
		sha1[:39],                   // one short of SHA-1
		sha1 + "0",                  // between the two lengths
		"g" + sha1[1:],              // not hex
		sha1[:20] + " " + sha1[21:], // a space inside
	} {
		_, err := CommitIDs([]string{sha1, bad}, 10)
		require.Error(t, err, "%q", bad)
		require.Contains(t, err.Error(), "entry 1")
		require.Contains(t, err.Error(), "not a full commit id")
	}
	_, err = CommitIDs([]string{strings.Repeat("x", 500)}, 10)
	require.Error(t, err)
	require.Less(t, len(err.Error()), 300, "a long paste is not echoed back whole")

	_, err = CommitIDs([]string{sha1, sha256, other}, 2)
	require.ErrorContains(t, err, "3 distinct commit ids: one call records at most 2")
	got, err = CommitIDs([]string{sha1, sha1, strings.ToUpper(sha1)}, 1)
	require.NoError(t, err, "the bound counts distinct ids")
	require.Equal(t, []string{sha1}, got)
	require.True(t, core.IsCommitID(sha256))
	require.False(t, core.IsCommitID(strings.ToUpper(sha1)), "the canonical form is lowercase")
}
