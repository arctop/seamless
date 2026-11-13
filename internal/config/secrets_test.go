package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFileSecrets(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []string
	}{
		{"a socket client config holds none", "role: client\nserver_url: \"unix:///opt/seamless/run/seamlessd.sock\"\n", nil},
		{"an empty file holds none", "", nil},
		{"an empty key is no key", "role: client\nserver_url: \"unix:///s.sock\"\nmcp:\n  api_key: \"\"\n", nil},
		{"the bearer key", "role: client\nserver_url: \"https://seam.example:8443\"\nmcp:\n  api_key: \"k\"\n", []string{"mcp.api_key"}},
		{"provider keys", "llm:\n  openai:\n    api_key: \"o\"\n  anthropic:\n    api_key: \"a\"\n", []string{"llm.openai.api_key", "llm.anthropic.api_key"}},
		{"a server URL with userinfo", "role: client\nserver_url: \"https://user:pw@seam.example:8443\"\n", []string{"server_url"}},
		{"a base URL with a token as its user", "llm:\n  ollama:\n    base_url: \"http://token@ollama.example:11434\"\n", []string{"llm.ollama.base_url"}},
		{"a bare base URL is an address", "llm:\n  openai:\n    base_url: \"https://api.example/v1\"\n", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "seamless.yaml")
			require.NoError(t, os.WriteFile(path, []byte(tt.body), 0o600))
			got, err := FileSecrets(path)
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

// The question is what the FILE holds: a key the environment supplies is not in
// it, so it does not make the file secret-bearing.
func TestFileSecrets_IgnoresTheEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seamless.yaml")
	require.NoError(t, os.WriteFile(path, []byte("role: client\nserver_url: \"unix:///s.sock\"\n"), 0o600))
	t.Setenv("SEAMLESS_MCP_API_KEY", "from-env")
	t.Setenv("SEAMLESS_SERVER_URL", "https://user:pw@seam.example:8443")

	got, err := FileSecrets(path)
	require.NoError(t, err)
	require.Empty(t, got)
}

// A file it cannot judge is an error, never a clean answer.
func TestFileSecrets_UnreadableIsAnError(t *testing.T) {
	dir := t.TempDir()
	_, err := FileSecrets(filepath.Join(dir, "missing.yaml"))
	require.ErrorContains(t, err, "config.FileSecrets: read")

	for name, body := range map[string]string{
		"unknown key":      "mcp:\n  api_kee: \"k\"\n",
		"two documents":    "role: client\n---\nmcp:\n  api_key: \"k\"\n",
		"not a config map": "- one\n- two\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".yaml")
			require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
			_, err := FileSecrets(path)
			require.ErrorContains(t, err, "config.FileSecrets: parse")
		})
	}
}

// secretFields is a hand list, so this walks Config for every string key whose
// name says credential or URL and fails on one the list misses. A new provider
// key, token or URL then cannot make a world-readable config look secret-free.
func TestSecretFields_CoverEveryCredentialShapedKey(t *testing.T) {
	listed := map[string]bool{}
	for _, f := range secretFields {
		listed[f.key] = f.url
	}
	var credential, urls []string
	var walk func(t reflect.Type, prefix string)
	walk = func(t reflect.Type, prefix string) {
		for i := range t.NumField() {
			field := t.Field(i)
			if !field.IsExported() {
				continue
			}
			tag, _, _ := strings.Cut(field.Tag.Get("yaml"), ",")
			if tag == "" || tag == "-" {
				continue
			}
			key := tag
			if prefix != "" {
				key = prefix + "." + tag
			}
			switch field.Type.Kind() {
			case reflect.Struct:
				walk(field.Type, key)
			case reflect.String:
				switch {
				case strings.HasSuffix(tag, "_file") || strings.HasSuffix(tag, "_path"):
					// A path to a secret, not the secret.
				case strings.Contains(tag, "key") || strings.Contains(tag, "token") ||
					strings.Contains(tag, "secret") || strings.Contains(tag, "password"):
					credential = append(credential, key)
				case strings.HasSuffix(tag, "url"):
					urls = append(urls, key)
				}
			}
		}
	}
	walk(reflect.TypeFor[Config](), "")

	require.NotEmpty(t, credential)
	for _, key := range credential {
		isURL, ok := listed[key]
		require.True(t, ok, "%s looks like a credential but secretFields does not list it", key)
		require.False(t, isURL, "%s is a credential, not a URL", key)
	}
	require.NotEmpty(t, urls)
	for _, key := range urls {
		isURL, ok := listed[key]
		require.True(t, ok, "%s is a URL, which can carry userinfo, but secretFields does not list it", key)
		require.True(t, isURL, "%s is listed, but not as a URL", key)
	}
	require.Len(t, secretFields, len(credential)+len(urls), "secretFields lists a key Config no longer has")
}
