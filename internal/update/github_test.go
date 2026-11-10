package update

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The fetcher refuses plain http (RequireHTTPS), so these tests use TLS
// servers; srv.Client() trusts the throwaway certificate while the scheme
// rules under test stay unchanged.
func testFetcher(t *testing.T, srv *httptest.Server, now time.Time) *Fetcher {
	t.Helper()
	client := srv.Client()
	client.CheckRedirect = HTTPSOnlyRedirect
	return &Fetcher{Client: client, URL: srv.URL + "/repos/arctop/seamless/releases?per_page=20", Now: func() time.Time { return now }}
}

const oneRelease = `[{"tag_name":"v0.7.3","draft":false,"prerelease":false,
  "published_at":"2026-10-09T10:00:00Z","name":"ignored","body":"ignored",
  "assets":[{"name":"checksums.txt","state":"uploaded"},{"name":"install","state":"uploaded"},
            {"name":"install.sigstore.json","state":"uploaded"}]}]`

func TestFetch_OKSendsAnonymousHeaders(t *testing.T) {
	// A token in the environment must never ride along: the check is anonymous.
	t.Setenv("GITHUB_TOKEN", "ghp_secret")

	var got http.Header
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		require.Equal(t, "/repos/arctop/seamless/releases", r.URL.Path)
		require.Equal(t, "20", r.URL.Query().Get("per_page"))
		w.Header().Set("ETag", `W/"abc"`)
		w.Header().Set("Date", "Fri, 09 Oct 2026 12:00:00 GMT")
		_, _ = w.Write([]byte(oneRelease))
	}))
	defer srv.Close()

	page, err := testFetcher(t, srv, time.Now()).Fetch(context.Background(), "")
	require.NoError(t, err)
	require.False(t, page.NotModified)
	require.Equal(t, `W/"abc"`, page.ETag)
	require.Equal(t, time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC), page.ServerDate)
	require.Len(t, page.Releases, 1)
	require.Equal(t, "v0.7.3", page.Releases[0].TagName)

	require.Equal(t, "seamlessd-update-check", got.Get("User-Agent"))
	require.Equal(t, "application/vnd.github+json", got.Get("Accept"))
	require.Equal(t, "2022-11-28", got.Get("X-GitHub-Api-Version"))
	require.Empty(t, got.Get("Authorization"))
	require.Empty(t, got.Get("If-None-Match"), "no validator on a cold fetch")
	for k, vs := range got {
		for _, v := range vs {
			require.NotContains(t, v, "ghp_secret", "header %s leaks the token", k)
		}
	}
}

func TestFetch_NotModifiedReusesETag(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, `W/"abc"`, r.Header.Get("If-None-Match"))
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()

	page, err := testFetcher(t, srv, time.Now()).Fetch(context.Background(), `W/"abc"`)
	require.NoError(t, err)
	require.True(t, page.NotModified)
	require.Equal(t, `W/"abc"`, page.ETag, "a 304 without an ETag keeps the caller's validator")
	require.Empty(t, page.Releases)
}

func TestFetch_RateLimits(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	serverNow := now.Add(-48 * time.Hour) // GitHub's clock is the reference, not ours
	tests := []struct {
		name     string
		status   int
		headers  map[string]string
		wantWait time.Duration // RetryAt - now; 0 = not rate limited
	}{
		{"primary limit waits for reset, measured on GitHub's clock", http.StatusForbidden, map[string]string{
			"X-RateLimit-Remaining": "0",
			"X-RateLimit-Reset":     strconv.FormatInt(serverNow.Add(40*time.Minute).Unix(), 10),
			"Date":                  serverNow.Format(http.TimeFormat),
		}, 40 * time.Minute},
		{"a reset that is imminent still waits the floor", http.StatusForbidden, map[string]string{
			"X-RateLimit-Remaining": "0",
			"X-RateLimit-Reset":     strconv.FormatInt(serverNow.Add(time.Minute).Unix(), 10),
			"Date":                  serverNow.Format(http.TimeFormat),
		}, 15 * time.Minute},
		{"secondary limit honors Retry-After", http.StatusForbidden, map[string]string{"Retry-After": "7200"}, 2 * time.Hour},
		{"429 Retry-After below the floor", http.StatusTooManyRequests, map[string]string{"Retry-After": "60"}, 15 * time.Minute},
		{"bare 429 is still rate limiting", http.StatusTooManyRequests, nil, 15 * time.Minute},
		{"a garbage reset far away is capped", http.StatusForbidden, map[string]string{
			"X-RateLimit-Remaining": "0",
			"X-RateLimit-Reset":     strconv.FormatInt(now.Add(30*24*time.Hour).Unix(), 10),
			"Date":                  serverNow.Format(http.TimeFormat),
		}, 24 * time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tt.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()

			_, err := testFetcher(t, srv, now).Fetch(context.Background(), "")
			require.ErrorIs(t, err, ErrRateLimited)
			require.NotErrorIs(t, err, ErrUnavailable)
			var rl *RateLimitError
			require.ErrorAs(t, err, &rl)
			require.Equal(t, tt.wantWait, rl.RetryAt.Sub(now))
		})
	}
}

func TestFetch_Unavailable(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		want    string
	}{
		{"plain 403 is a refusal, not a rate limit", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		}, "403"},
		{"404", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) }, "404"},
		{"5xx", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }, "502"},
		{"bad json", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"not":"a list"}`)) }, "decode"},
		{"oversized body", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("[" + strings.Repeat(" ", maxListBytes) + "]"))
		}, "exceeds"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewTLSServer(tt.handler)
			defer srv.Close()

			_, err := testFetcher(t, srv, time.Now()).Fetch(context.Background(), "")
			require.ErrorIs(t, err, ErrUnavailable)
			require.NotErrorIs(t, err, ErrRateLimited)
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestFetch_RefusesHTTPSToHTTPRedirect(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(oneRelease))
	}))
	defer plain.Close()
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL, http.StatusFound)
	}))
	defer secure.Close()

	page, err := testFetcher(t, secure, time.Now()).Fetch(context.Background(), "")
	require.ErrorIs(t, err, ErrUnavailable)
	require.ErrorContains(t, err, "https")
	require.Empty(t, page.Releases)
}

func TestFetch_RefusesPlainHTTPURL(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("a plain-http URL must not be dialed")
	}))
	defer plain.Close()

	f := &Fetcher{Client: plain.Client(), URL: plain.URL}
	_, err := f.Fetch(context.Background(), "")
	require.ErrorContains(t, err, "https")
}

func TestFetch_ContextCancel(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := testFetcher(t, srv, time.Now()).Fetch(ctx, "")
	require.True(t, errors.Is(err, context.Canceled), "got %v", err)
}

func TestNewHTTPClient(t *testing.T) {
	c := NewHTTPClient()
	require.Equal(t, fetchTimeout, c.Timeout)
	require.NotNil(t, c.CheckRedirect)
	tr, ok := c.Transport.(*http.Transport)
	require.True(t, ok)
	require.NotNil(t, tr.Proxy, "honors HTTPS_PROXY from the environment")
}
