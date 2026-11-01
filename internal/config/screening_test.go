package config

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// The screening feature ships off, and its tunables have defaults that load
// without a file.
func TestLoadFrom_ScreeningDefaultsOff(t *testing.T) {
	clearSeamlessEnv(t)
	cfg, err := LoadFrom("")
	require.NoError(t, err)
	require.False(t, cfg.Features.Screening, "write screening ships off")
	require.Empty(t, cfg.Screening.URLAllowlist, "no allowlist: the URL screen is off")
	require.Equal(t, DefaultScreeningBlobMinLength, cfg.Screening.BlobMinLength)
}

func TestLoadFrom_ScreeningFileAndEnv(t *testing.T) {
	clearSeamlessEnv(t)
	path := writeConfig(t, `
features:
  screening: true
screening:
  url_allowlist: [example.com, "*.example.org"]
  blob_min_length: 120
`)
	cfg, err := LoadFrom(path)
	require.NoError(t, err)
	require.True(t, cfg.Features.Screening)
	require.Equal(t, []string{"example.com", "*.example.org"}, cfg.Screening.URLAllowlist)
	require.Equal(t, 120, cfg.Screening.BlobMinLength)

	t.Setenv("SEAMLESS_FEATURES_SCREENING", "false")
	t.Setenv("SEAMLESS_SCREENING_URL_ALLOWLIST", "docs.example.net, ,example.com,")
	t.Setenv("SEAMLESS_SCREENING_BLOB_MIN_LENGTH", "64")
	cfg, err = LoadFrom(path)
	require.NoError(t, err)
	require.False(t, cfg.Features.Screening, "env wins over file")
	require.Equal(t, []string{"docs.example.net", "example.com"}, cfg.Screening.URLAllowlist,
		"the env list replaces the file's, blanks dropped")
	require.Equal(t, 64, cfg.Screening.BlobMinLength)
	require.NoError(t, os.Unsetenv("SEAMLESS_SCREENING_URL_ALLOWLIST"))
	require.NoError(t, os.Unsetenv("SEAMLESS_SCREENING_BLOB_MIN_LENGTH"))

	// Present but uninterpretable is an error, never a silent default.
	t.Setenv("SEAMLESS_FEATURES_SCREENING", "sometimes")
	_, err = LoadFrom("")
	require.Error(t, err)
}

func TestScreeningValidate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cfg     Screening
		wantErr string
	}{
		{"defaults", Screening{BlobMinLength: DefaultScreeningBlobMinLength}, ""},
		{"wildcard and dot forms", Screening{BlobMinLength: 200, URLAllowlist: []string{"*.example.com", ".example.org", "example.net"}}, ""},
		{"zero length is present, not absent", Screening{BlobMinLength: 0}, "blob_min_length"},
		{"too short", Screening{BlobMinLength: 8}, "blob_min_length"},
		{"too long", Screening{BlobMinLength: 1 << 21}, "blob_min_length"},
		{"a URL", Screening{BlobMinLength: 200, URLAllowlist: []string{"https://example.com"}}, "is a URL"},
		{"a path", Screening{BlobMinLength: 200, URLAllowlist: []string{"example.com/docs"}}, "not a host name"},
		{"a port", Screening{BlobMinLength: 200, URLAllowlist: []string{"example.com:8080"}}, "carries a port"},
		{"empty entry", Screening{BlobMinLength: 200, URLAllowlist: []string{" "}}, "is empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

func TestLoadFrom_ScreeningRejectsAURLInTheAllowlist(t *testing.T) {
	clearSeamlessEnv(t)
	path := writeConfig(t, `
screening:
  url_allowlist: ["https://example.com/"]
`)
	_, err := LoadFrom(path)
	require.ErrorContains(t, err, "screening.url_allowlist")
}
