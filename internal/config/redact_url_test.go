package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// RedactedURL drops a URL's user and password whole: a password, and a token
// given as the user with no password (which net/http's user:***@ and
// url.URL.Redacted's user:xxxxx@ both print). A URL that names no user comes
// back exactly as given, and so does anything that is not a URL with an
// authority. A value url.Parse refuses still loses what its authority holds
// before the last "@".
func TestRedact_RedactedURL(t *testing.T) {
	for raw, want := range map[string]string{
		"https://alice:s3cret@box.example:8081":         "https://box.example:8081",
		"http://alice@192.0.2.10:8081":                  "http://192.0.2.10:8081",
		"https://example-token-0123@box.example":        "https://box.example",
		"https://@box.example:8081":                     "https://box.example:8081",
		"https://alice:s3cret@[2001:db8::10]:8081/":     "https://[2001:db8::10]:8081/",
		"https://alice:s3cret@box.example:8081/api/mcp": "https://box.example:8081/api/mcp",
		"http://alice:s3 cret@box.example:8081":         "http://box.example:8081",
		"http://alice:s3 cret@box.example:8081/x?y=a@b": "http://box.example:8081/x?y=a@b",
		"https://box.example:8081":                      "https://box.example:8081",
		"http://127.0.0.1:8081":                         "http://127.0.0.1:8081",
		"https://box.example:8081/?next=https://a@b.c":  "https://box.example:8081/?next=https://a@b.c",
		"unix:///opt/seamless/run/seamlessd.sock":       "unix:///opt/seamless/run/seamlessd.sock",
		"http://[::1/x@y":                               "http://[::1/x@y",
		"%":                                             "%",
		"":                                              "",
	} {
		require.Equal(t, want, RedactedURL(raw), "%q", raw)
	}
}

// DaemonAddress, what every message a person reads names, carries no user;
// ServerURL, what every client dials, is as configured.
func TestRedact_DaemonAddressNamesNoUser(t *testing.T) {
	c := Defaults()
	c.AdvertisedURL = "https://alice:s3cret@box.example:8081"
	require.NoError(t, c.Validate(), "it loads")
	require.Equal(t, "https://box.example:8081", c.DaemonAddress())
	require.Equal(t, "https://alice:s3cret@box.example:8081", c.ServerURL(), "the dial is unchanged")
	require.Equal(t, "box.example", c.ServerHost())

	c.AdvertisedURL = "https://box.example:8081"
	require.Equal(t, "https://box.example:8081", c.DaemonAddress())
	require.Equal(t, "http://127.0.0.1:8081", Defaults().DaemonAddress(), "a default install is as it was")
}

// A server_url refused at load is named without its user and password: the
// refusal reaches the terminal, and a daemon's log when it stops a start.
// One that names no user is quoted as before.
func TestRedact_ServerURLRefusalsNameNoPassword(t *testing.T) {
	for _, raw := range []string{
		"https://alice:s3cret@0.0.0.0:8081",
		"https://alice:s3cret@box.example:8081/path",
		"https://alice:s3cret@box.example:8081/?q=1",
		"https://alice:s3cret@box.example:8081/#f",
		"ftp://alice:s3cret@box.example:8081",
		"http://alice:s3 cret@box.example:8081",
		"https://alice:s3cret@:8081",
		"unix://alice:s3cret@/opt/seamless/run/seamlessd.sock",
	} {
		c := Defaults()
		c.AdvertisedURL = raw
		err := c.Validate()
		require.Error(t, err, raw)
		require.NotContains(t, err.Error(), "s3cret", raw)
		require.NotContains(t, err.Error(), "alice", raw)
		require.Contains(t, err.Error(), "server_url", raw)
	}

	c := Defaults()
	c.AdvertisedURL = "https://box.example:8081/path"
	require.ErrorContains(t, c.Validate(), `server_url "https://box.example:8081/path" must be a bare base URL`)
	c.AdvertisedURL = "http://[::1"
	require.ErrorContains(t, c.Validate(), `server_url "http://[::1" is not a URL: parse "http://[::1"`, "a parse error is quoted as before")
}
