package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
)

// settingsServer serves the console settings JSON a client install reads, and
// records what it was asked for.
func settingsServer(t *testing.T, status int, body string) (*httptest.Server, *[]*http.Request) {
	t.Helper()
	var seen []*http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Clone(r.Context()))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func TestClientFeatures_ReadsTheServersEffectiveState(t *testing.T) {
	// An older server still names momentum and gamification: the keys are
	// read past, they switch nothing now.
	srv, seen := settingsServer(t, http.StatusOK,
		`{"featuresConfig":{"research":true,"momentum":false,"gamification":false,"repo_locks":false}}`)

	cfg := config.Defaults()
	cfg.MCP.APIKey = "server-key"
	got := clientFeatures(cfg, srv.URL)

	require.True(t, got.Research, "the server's stored override decides, not this machine's config")
	require.Equal(t, config.Features{Research: true}, got)
	require.Len(t, *seen, 1)
	require.Equal(t, "/console/settings?format=json", (*seen)[0].URL.RequestURI())
	require.Equal(t, "Bearer server-key", (*seen)[0].Header.Get("Authorization"))
}

// The Diary's key reads like every other: a server with it on says so, and
// an answer from a release before the key reads it as off, its shipped
// default.
func TestClientFeatures_ReadsTheDiaryKey(t *testing.T) {
	srv, _ := settingsServer(t, http.StatusOK, `{"featuresConfig":{"research":false,"diary":true}}`)
	got := clientFeatures(config.Defaults(), srv.URL)
	require.True(t, got.Diary, "the server's state decides")
	require.False(t, got.Research)

	older, _ := settingsServer(t, http.StatusOK, `{"featuresConfig":{"research":true}}`)
	got = clientFeatures(config.Defaults(), older.URL)
	require.False(t, got.Diary, "an answer with no diary key reads as off, the shipped default")
}

// A daemon that predates the features contract answers without featuresConfig.
// Decoding that absence into a value would read as "every optional feature off"
// -- which would delete an installed skill on a server that has the feature on.
func TestClientFeatures_AbsentFeaturesConfigDegradesLoudly(t *testing.T) {
	srv, _ := settingsServer(t, http.StatusOK, `{"briefingConfig":{}}`)

	cfg := config.Defaults()
	cfg.Features.Research = true // this machine's file/env base
	var got config.Features
	out := captureStdout(t, func() error {
		got = clientFeatures(cfg, srv.URL)
		return nil
	})

	require.True(t, got.Research, "the fallback is the file/env base, never all-off")
	require.Contains(t, out, "warning:")
	require.Contains(t, out, "featuresConfig")
}

func TestClientFeatures_UnreachableServerWarnsAndFallsBack(t *testing.T) {
	srv, _ := settingsServer(t, http.StatusOK, `{}`)
	url := srv.URL
	srv.Close() // nothing is listening now

	cfg := config.Defaults()
	cfg.Features.RepoLocks = true
	var got config.Features
	out := captureStdout(t, func() error {
		got = clientFeatures(cfg, url)
		return nil
	})

	require.True(t, got.RepoLocks)
	require.Contains(t, out, "warning:")
	require.Contains(t, out, "cannot read the feature toggles from "+url)
}

func TestClientConsoleJSON_NonOKNamesTheStatus(t *testing.T) {
	srv, _ := settingsServer(t, http.StatusUnauthorized, `{"error":"unauthorized"}`)

	var data struct{}
	err := clientConsoleJSON(config.Defaults(), srv.URL, "/console/settings?format=json", &data)
	require.ErrorContains(t, err, "401")
}

// A configured-but-unusable tls.ca_file is an error, never a silent fall back to
// the system pool: the request would then fail in the handshake and read as an
// outage. The constructor itself is tested where it lives
// (internal/config/httpclient_test.go); what this pins is that the client-role
// read goes THROUGH it instead of building a client of its own.
func TestClientConsoleJSON_UnusableCAFileIsAnError(t *testing.T) {
	cfg := config.Defaults()
	cfg.TLS.CAFile = filepath.Join(t.TempDir(), "absent.pem")
	var data struct{}
	err := clientConsoleJSON(cfg, "https://example.invalid", "/console/settings?format=json", &data)
	require.ErrorContains(t, err, "tls.ca_file")

	notPEM := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(notPEM, []byte("not a certificate"), 0o600))
	cfg.TLS.CAFile = notPEM
	err = clientConsoleJSON(cfg, "https://example.invalid", "/console/settings?format=json", &data)
	require.ErrorContains(t, err, "no PEM certificate found")
}
